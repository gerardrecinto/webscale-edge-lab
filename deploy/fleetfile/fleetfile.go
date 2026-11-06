// Package fleetfile reads the JSON that describes a real fleet: which hosts,
// and the shell commands that drain, update, verify and enable one of them.
package fleetfile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/template"
	"time"

	"github.com/gerardrecinto/webscale-edge-lab/deploy"
)

type Fleet struct {
	Hosts             []string          `json:"hosts"`
	HostsFile         string            `json:"hostsFile"` // one host per line, # comments allowed
	Vars              map[string]string `json:"vars"`
	Commands          deploy.Commands   `json:"commands"`
	ObserveCommand    string            `json:"observeCommand"` // prints the canary and baseline numbers as JSON, see Observer
	InventoryParallel int               `json:"inventoryParallel"`
	StepTimeoutSec    int               `json:"stepTimeoutSec"`
}

func Load(path string) (*Fleet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Fleet
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.HostsFile != "" {
		hb, err := os.ReadFile(f.HostsFile)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(hb), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				f.Hosts = append(f.Hosts, line)
			}
		}
	}
	if len(f.Hosts) == 0 {
		return nil, fmt.Errorf("%s lists no hosts", path)
	}
	return &f, nil
}

func (f *Fleet) StepTimeout() time.Duration {
	if f.StepTimeoutSec <= 0 {
		return 120 * time.Second
	}
	return time.Duration(f.StepTimeoutSec) * time.Second
}

func (f *Fleet) Targets(ctx context.Context) ([]deploy.Target, error) {
	return deploy.NewCmdFleet(ctx, f.Hosts, f.Commands, f.Vars, f.InventoryParallel)
}

type window struct {
	Requests int     `json:"requests"`
	Errors   int     `json:"errors"`
	P95Ms    float64 `json:"p95Ms"`
}

// Observer runs ObserveCommand and reads
//
//	{"canary":{"requests":900,"errors":3,"p95Ms":41},"baseline":{"requests":9000,"errors":20,"p95Ms":38}}
//
// {{.Version}} and {{.SinceUnix}} are available in the command, so it can be a
// query against Splunk or Prometheus for the time since the canary went live.
func (f *Fleet) Observer(version string) (deploy.Observer, error) {
	if strings.TrimSpace(f.ObserveCommand) == "" {
		return nil, fmt.Errorf("a canary needs observeCommand in the fleet file")
	}
	t, err := template.New("observe").Option("missingkey=error").Parse(f.ObserveCommand)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, since time.Time) (deploy.Window, deploy.Window, error) {
		var cmdline bytes.Buffer
		if err := t.Execute(&cmdline, map[string]any{"Version": version, "SinceUnix": since.Unix(), "Vars": f.Vars}); err != nil {
			return deploy.Window{}, deploy.Window{}, err
		}
		out, err := exec.CommandContext(ctx, "sh", "-c", cmdline.String()).Output()
		if err != nil {
			return deploy.Window{}, deploy.Window{}, fmt.Errorf("observeCommand: %w", err)
		}
		var r struct {
			Canary, Baseline window
		}
		if err := json.Unmarshal(out, &r); err != nil {
			return deploy.Window{}, deploy.Window{}, fmt.Errorf("observeCommand did not print the expected JSON: %w", err)
		}
		conv := func(w window) deploy.Window {
			return deploy.Window{Requests: w.Requests, Errors: w.Errors, P95: time.Duration(w.P95Ms * float64(time.Millisecond))}
		}
		return conv(r.Canary), conv(r.Baseline), nil
	}, nil
}
