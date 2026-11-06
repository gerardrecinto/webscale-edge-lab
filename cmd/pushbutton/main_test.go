package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeFleet(t *testing.T, n int) (root, file string) {
	t.Helper()
	root = t.TempDir()
	var hosts []string
	for i := 0; i < n; i++ {
		h := fmt.Sprintf("web-%03d", i)
		hosts = append(hosts, h)
		os.MkdirAll(filepath.Join(root, h), 0o755)
		os.WriteFile(filepath.Join(root, h, "version"), []byte("v1\n"), 0o644)
		os.WriteFile(filepath.Join(root, h, "serving"), nil, 0o644)
	}
	d := "{{.Vars.dir}}/{{.Host}}"
	f := map[string]any{
		"hosts": hosts, "vars": map[string]string{"dir": root}, "inventoryParallel": 8, "stepTimeoutSec": 20,
		"commands": map[string]string{
			"version": "cat " + d + "/version", "inRotation": "test -e " + d + "/serving", "drain": "rm -f " + d + "/serving",
			"update": "echo {{.Version}} > " + d + "/version",
			"verify": `test "$(cat ` + d + `/version)" = "{{.Version}}" && ! test -e ` + d + `/broken-{{.Version}}`,
			"enable": "touch " + d + "/serving",
		},
	}
	b, _ := json.Marshal(f)
	file = filepath.Join(root, "fleet.json")
	os.WriteFile(file, b, 0o644)
	return
}

func versionOf(root, host string) string {
	b, _ := os.ReadFile(filepath.Join(root, host, "version"))
	return strings.TrimSpace(string(b))
}

func TestDeployCommand(t *testing.T) {
	root, file := makeFleet(t, 20)
	if code := deployFleet([]string{"-fleet", file, "-version", "v2", "-batch", "10", "-dry-run"}); code != 0 {
		t.Fatalf("dry run exit %d", code)
	}
	if versionOf(root, "web-000") != "v1" {
		t.Fatal("a dry run changed a host")
	}
	rep := filepath.Join(root, "report.json")
	if code := deployFleet([]string{"-fleet", file, "-version", "v2", "-batch", "10", "-report", rep}); code != 0 {
		t.Fatalf("deploy exit %d", code)
	}
	for i := 0; i < 20; i++ {
		if v := versionOf(root, fmt.Sprintf("web-%03d", i)); v != "v2" {
			t.Fatalf("web-%03d runs %s", i, v)
		}
	}
	var r struct {
		Status  string
		PeakOut int
	}
	b, _ := os.ReadFile(rep)
	if err := json.Unmarshal(b, &r); err != nil || r.Status != "succeeded" || r.PeakOut > 2 {
		t.Fatalf("report %s: %v %+v", b, err, r)
	}
}

func TestDeployCommandRollsBackAndExitsOne(t *testing.T) {
	root, file := makeFleet(t, 20)
	os.WriteFile(filepath.Join(root, "web-013", "broken-v2"), nil, 0o644)
	if code := deployFleet([]string{"-fleet", file, "-version", "v2", "-batch", "10"}); code != 1 {
		t.Fatalf("exit %d, want 1 for a rollback", code)
	}
	for i := 0; i < 20; i++ {
		if v := versionOf(root, fmt.Sprintf("web-%03d", i)); v != "v1" {
			t.Fatalf("web-%03d runs %s after the rollback", i, v)
		}
	}
}

func TestDeployCommandUsageErrors(t *testing.T) {
	if code := deployFleet([]string{"-version", "v2"}); code != 2 {
		t.Errorf("missing -fleet: exit %d", code)
	}
	_, file := makeFleet(t, 3)
	if code := deployFleet([]string{"-fleet", file, "-version", "v2", "-canary", "1"}); code != 2 {
		t.Errorf("a canary with no observeCommand: exit %d, want 2", code)
	}
}
