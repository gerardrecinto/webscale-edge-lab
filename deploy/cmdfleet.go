package deploy

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"text/template"
)

// Commands are the shell commands that drive one host. Each is a Go template
// with {{.Host}}, {{.Version}} and {{.Vars.name}} available, run with `sh -c`
// so an ssh invocation, a curl to a balancer API, or a systemctl call all work.
// Host and Version are checked against a strict pattern before they are put
// into a command, so a bad inventory entry cannot inject shell.
type Commands struct {
	Version    string `json:"version"`    // prints the installed build on stdout
	InRotation string `json:"inRotation"` // exit 0 when the balancer is sending it traffic
	Drain      string `json:"drain"`      // take it out of the balancer and wait for requests to finish
	Update     string `json:"update"`     // install and start {{.Version}}
	Verify     string `json:"verify"`     // exit 0 when the host itself says it is ready on {{.Version}}
	Enable     string `json:"enable"`     // put it back in the balancer, return when it is in service
}

var safeToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// CmdTarget is a host driven by shell commands.
type CmdTarget struct {
	host string
	c    *cmdSet

	mu      sync.Mutex
	version string
	rotated bool
}

type cmdSet struct {
	tmpl map[string]*template.Template
	vars map[string]string
}

func compile(c Commands, vars map[string]string) (*cmdSet, error) {
	cs := &cmdSet{tmpl: map[string]*template.Template{}, vars: vars}
	for name, src := range map[string]string{"version": c.Version, "inRotation": c.InRotation, "drain": c.Drain, "update": c.Update, "verify": c.Verify, "enable": c.Enable} {
		if strings.TrimSpace(src) == "" {
			return nil, fmt.Errorf("command %q is empty", name)
		}
		t, err := template.New(name).Option("missingkey=error").Parse(src)
		if err != nil {
			return nil, fmt.Errorf("command %q: %w", name, err)
		}
		cs.tmpl[name] = t
	}
	return cs, nil
}

func (cs *cmdSet) run(ctx context.Context, name, host, version string) (string, error) {
	var b bytes.Buffer
	err := cs.tmpl[name].Execute(&b, map[string]any{"Host": host, "Version": version, "Vars": cs.vars})
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", b.String())
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		return strings.TrimSpace(out.String()), fmt.Errorf("%s on %s: %w: %s", name, host, err, msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// NewCmdFleet builds a target per host and reads each host's build and
// rotation state, at most parallel hosts at a time, so inventorying a thousand
// hosts takes the time of a thousand/parallel ssh round trips rather than all of them in a row.
func NewCmdFleet(ctx context.Context, hosts []string, c Commands, vars map[string]string, parallel int) ([]Target, error) {
	cs, err := compile(c, vars)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, h := range hosts {
		if !safeToken.MatchString(h) {
			return nil, fmt.Errorf("host %q is not a plain hostname or address, refusing to put it in a command", h)
		}
		if seen[h] {
			return nil, fmt.Errorf("host %q is listed twice", h)
		}
		seen[h] = true
	}
	if parallel < 1 {
		parallel = 16
	}
	out := make([]Target, len(hosts))
	errs := make([]error, len(hosts))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, h string) {
			defer wg.Done()
			defer func() { <-sem }()
			t := &CmdTarget{host: h, c: cs}
			v, err := cs.run(ctx, "version", h, "")
			if err != nil {
				errs[i] = err
				return
			}
			t.version = v
			_, err = cs.run(ctx, "inRotation", h, "")
			t.rotated = err == nil
			out[i] = t
		}(i, h)
	}
	wg.Wait()
	var bad []string
	for _, e := range errs {
		if e != nil {
			bad = append(bad, e.Error())
		}
	}
	if len(bad) > 0 {
		if len(bad) > 5 {
			bad = append(bad[:5], fmt.Sprintf("and %d more", len(bad)-5))
		}
		return nil, fmt.Errorf("could not read the state of %d hosts:\n  %s", len(errs)-countNil(errs), strings.Join(bad, "\n  "))
	}
	return out, nil
}

func countNil(e []error) int {
	n := 0
	for _, x := range e {
		if x == nil {
			n++
		}
	}
	return n
}

func (t *CmdTarget) ID() string { return t.host }
func (t *CmdTarget) Version() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.version
}
func (t *CmdTarget) InRotation() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rotated
}

func (t *CmdTarget) Drain(ctx context.Context) error {
	if _, err := t.c.run(ctx, "drain", t.host, t.Version()); err != nil {
		return err
	}
	t.mu.Lock()
	t.rotated = false
	t.mu.Unlock()
	return nil
}

func (t *CmdTarget) Update(ctx context.Context, version string) error {
	if !safeToken.MatchString(version) {
		return fmt.Errorf("version %q is not a plain build identifier", version)
	}
	if _, err := t.c.run(ctx, "update", t.host, version); err != nil {
		return err
	}
	t.mu.Lock()
	t.version = version
	t.mu.Unlock()
	return nil
}

func (t *CmdTarget) Verify(ctx context.Context, version string) error {
	if !safeToken.MatchString(version) {
		return fmt.Errorf("version %q is not a plain build identifier", version)
	}
	_, err := t.c.run(ctx, "verify", t.host, version)
	return err
}

func (t *CmdTarget) Enable(ctx context.Context) error {
	if _, err := t.c.run(ctx, "enable", t.host, t.Version()); err != nil {
		return err
	}
	t.mu.Lock()
	t.rotated = true
	t.mu.Unlock()
	return nil
}
