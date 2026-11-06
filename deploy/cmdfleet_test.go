package deploy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fleet whose "hosts" are directories: a version file, and a serving marker
// that stands in for being in the balancer. The commands are plain shell, the
// same way an ssh-driven fleet's would be.
func dirFleet(t *testing.T, n int) (string, []string, Commands) {
	t.Helper()
	root := t.TempDir()
	var hosts []string
	for i := 0; i < n; i++ {
		h := fmt.Sprintf("web-%03d", i)
		hosts = append(hosts, h)
		os.MkdirAll(filepath.Join(root, h), 0o755)
		os.WriteFile(filepath.Join(root, h, "version"), []byte("v1\n"), 0o644)
		os.WriteFile(filepath.Join(root, h, "serving"), nil, 0o644)
	}
	d := "{{.Vars.dir}}/{{.Host}}"
	return root, hosts, Commands{
		Version:    "cat " + d + "/version",
		InRotation: "test -e " + d + "/serving",
		Drain:      "rm -f " + d + "/serving",
		Update:     "echo {{.Version}} > " + d + "/version",
		Verify:     `test "$(cat ` + d + `/version)" = "{{.Version}}" && ! test -e ` + d + `/broken-{{.Version}}`,
		Enable:     "touch " + d + "/serving",
	}
}

func serving(root string, hosts []string) int {
	n := 0
	for _, h := range hosts {
		if _, err := os.Stat(filepath.Join(root, h, "serving")); err == nil {
			n++
		}
	}
	return n
}

func TestCommandFleetRollsOutAndInventoriesInParallel(t *testing.T) {
	root, hosts, cmds := dirFleet(t, 60)
	targets, err := NewCmdFleet(context.Background(), hosts, cmds, map[string]string{"dir": root}, 16)
	if err != nil {
		t.Fatal(err)
	}
	var e Engine
	rep := e.Run(context.Background(), targets, Plan{Version: "v2", MaxUnavailable: 6, StepTimeout: 20 * time.Second})
	if rep.Status != StatusSucceeded || rep.PeakOut > 6 {
		t.Fatalf("%s %s peak %d", rep.Status, rep.Reason, rep.PeakOut)
	}
	if serving(root, hosts) != 60 {
		t.Errorf("%d of 60 serving at the end", serving(root, hosts))
	}
	for _, h := range hosts {
		if b, _ := os.ReadFile(filepath.Join(root, h, "version")); strings.TrimSpace(string(b)) != "v2" {
			t.Fatalf("%s runs %q", h, b)
		}
	}
}

func TestCommandFleetRollsBackWhenAHostFailsVerification(t *testing.T) {
	root, hosts, cmds := dirFleet(t, 30)
	os.WriteFile(filepath.Join(root, "web-017", "broken-v2"), nil, 0o644) // this host cannot run v2
	targets, err := NewCmdFleet(context.Background(), hosts, cmds, map[string]string{"dir": root}, 8)
	if err != nil {
		t.Fatal(err)
	}
	var e Engine
	rep := e.Run(context.Background(), targets, Plan{Version: "v2", MaxUnavailable: 5, StepTimeout: 20 * time.Second})
	if rep.Status != StatusRolledBack || rep.FailedHost != "web-017" {
		t.Fatalf("%s failed=%s: %s", rep.Status, rep.FailedHost, rep.Reason)
	}
	if serving(root, hosts) != 30 {
		t.Errorf("%d of 30 serving after the rollback", serving(root, hosts))
	}
	for _, h := range hosts {
		if b, _ := os.ReadFile(filepath.Join(root, h, "version")); strings.TrimSpace(string(b)) != "v1" {
			t.Fatalf("%s runs %q after the rollback", h, b)
		}
	}
}

func TestCommandFleetRefusesInputThatCouldInjectShell(t *testing.T) {
	root, _, cmds := dirFleet(t, 1)
	vars := map[string]string{"dir": root}
	for _, bad := range []string{"web-001; touch /tmp/pwned", "$(id)", "web 001", "web-001\nweb-002", "`id`", "../etc", ""} {
		if _, err := NewCmdFleet(context.Background(), []string{bad}, cmds, vars, 1); err == nil {
			t.Errorf("host %q was accepted", bad)
		}
	}
	targets, _ := NewCmdFleet(context.Background(), []string{"web-000"}, cmds, vars, 1)
	if err := targets[0].Update(context.Background(), "v2; touch /tmp/pwned"); err == nil {
		t.Error("a version with shell in it was accepted")
	}
	if _, err := os.Stat("/tmp/pwned"); err == nil {
		t.Error("an injected command ran")
	}
	if _, err := NewCmdFleet(context.Background(), []string{"a", "a"}, cmds, vars, 1); err == nil {
		t.Error("a duplicate host was accepted")
	}
}

func TestCommandFleetReportsUnreachableHostsBeforeTouchingAnything(t *testing.T) {
	root, hosts, cmds := dirFleet(t, 10)
	os.Remove(filepath.Join(root, "web-004", "version"))
	os.Remove(filepath.Join(root, "web-007", "version"))
	_, err := NewCmdFleet(context.Background(), hosts, cmds, map[string]string{"dir": root}, 4)
	if err == nil || !strings.Contains(err.Error(), "2 hosts") || !strings.Contains(err.Error(), "web-004") {
		t.Fatalf("%v", err)
	}
}

func TestCommandFleetTemplateErrors(t *testing.T) {
	_, hosts, cmds := dirFleet(t, 1)
	cmds.Drain = ""
	if _, err := NewCmdFleet(context.Background(), hosts, cmds, nil, 1); err == nil {
		t.Error("an empty command was accepted")
	}
	cmds.Drain = "echo {{.Nope"
	if _, err := NewCmdFleet(context.Background(), hosts, cmds, nil, 1); err == nil {
		t.Error("a broken template was accepted")
	}
	_, hosts, cmds = dirFleet(t, 1)
	cmds.Version = "echo {{.Vars.missing}}" // a variable the fleet file never set
	if _, err := NewCmdFleet(context.Background(), hosts, cmds, map[string]string{"dir": "x"}, 1); err == nil {
		t.Error("a missing variable was not reported")
	}
}
