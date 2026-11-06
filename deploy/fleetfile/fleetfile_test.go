package fleetfile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadReadsHostsFileAndRejectsTypos(t *testing.T) {
	d := t.TempDir()
	hosts := write(t, d, "hosts", "# web tier\nweb-001\n\nweb-002\n")
	f, err := Load(write(t, d, "f.json", `{"hostsFile":"`+hosts+`","commands":{}}`))
	if err != nil || len(f.Hosts) != 2 || f.Hosts[1] != "web-002" {
		t.Fatalf("%v %+v", err, f)
	}
	if _, err := Load(write(t, d, "typo.json", `{"hosts":["a"],"comands":{}}`)); err == nil {
		t.Error("an unknown field was accepted; a typo in a command name should not pass silently")
	}
	if _, err := Load(write(t, d, "empty.json", `{"hosts":[]}`)); err == nil {
		t.Error("an empty fleet was accepted")
	}
}

func TestObserverParsesTheCommandOutput(t *testing.T) {
	f := &Fleet{ObserveCommand: `echo '{"canary":{"requests":900,"errors":3,"p95Ms":41},"baseline":{"requests":9000,"errors":20,"p95Ms":38}}' # {{.Version}} {{.SinceUnix}}`}
	obs, err := f.Observer("v2")
	if err != nil {
		t.Fatal(err)
	}
	c, b, err := obs(context.Background(), time.Now())
	if err != nil || c.Requests != 900 || c.Errors != 3 || c.P95 != 41*time.Millisecond || b.Requests != 9000 {
		t.Fatalf("%v %+v %+v", err, c, b)
	}
	if _, err := (&Fleet{}).Observer("v2"); err == nil {
		t.Error("a canary without an observeCommand was accepted")
	}
	bad, _ := (&Fleet{ObserveCommand: "echo not json"}).Observer("v2")
	if _, _, err := bad(context.Background(), time.Now()); err == nil || !strings.Contains(err.Error(), "expected JSON") {
		t.Errorf("%v", err)
	}
}
