package secgate

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gerardrecinto/webscale-edge-lab/backend"
)

func load(t *testing.T) []Finding {
	t.Helper()
	f, err := os.Open("testdata/trivy.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fs, err := ParseTrivy(f)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

var now = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func TestParseTrivy(t *testing.T) {
	fs := load(t)
	if len(fs) != 6 {
		t.Fatalf("%d findings", len(fs))
	}
	if fs[1].ID != "CVE-2024-24790" || fs[1].Severity != "CRITICAL" || fs[1].Fixed != "1.21.11" || fs[1].Target != "checkout (gobinary)" {
		t.Errorf("%+v", fs[1])
	}
	if _, err := ParseTrivy(strings.NewReader(`{"hello":"world"}`)); err == nil {
		t.Error("a file that is not a Trivy report was accepted")
	}
}

func TestThresholdBlocksHighAndAbove(t *testing.T) {
	d := Evaluate(load(t), Policy{FailOn: "HIGH"}, now)
	if d.Pass || len(d.Blocking) != 4 || d.Below != 2 {
		t.Fatalf("%+v", d)
	}
	if d.Blocking[0].Severity != "CRITICAL" {
		t.Errorf("critical findings should sort first: %+v", d.Blocking)
	}
}

func TestFixedOnlySkipsFindingsWithNoFix(t *testing.T) {
	d := Evaluate(load(t), Policy{FailOn: "CRITICAL", FixedOnly: true}, now)
	// Two criticals: one has a fix and blocks, the other has none and is skipped.
	if len(d.Blocking) != 1 || d.Blocking[0].ID != "CVE-2024-24790" || d.NoFix != 1 {
		t.Fatalf("%+v", d)
	}
}

func TestWaiverPassesUntilItExpires(t *testing.T) {
	p := Policy{FailOn: "CRITICAL", FixedOnly: true, Waivers: []Waiver{{ID: "CVE-2024-24790", Package: "stdlib", Reason: "fix ships in the 2.5 release on Nov 12", Expires: now.Add(7 * 24 * time.Hour)}}}
	if d := Evaluate(load(t), p, now); !d.Pass || len(d.Waived) != 1 {
		t.Fatalf("live waiver: %+v", d)
	}
	d := Evaluate(load(t), p, now.Add(8*24*time.Hour))
	if d.Pass || len(d.Expired) != 1 {
		t.Fatalf("expired waiver should block again: %+v", d)
	}
}

func TestWaiverNeedsReasonAndExpiry(t *testing.T) {
	d := Evaluate(nil, Policy{FailOn: "HIGH", Waivers: []Waiver{{ID: "CVE-1"}}}, now)
	if d.Pass || len(d.Problems) != 2 {
		t.Fatalf("%+v", d)
	}
	if d := Evaluate(nil, Policy{FailOn: "SEVERE"}, now); d.Pass {
		t.Error("an unknown severity must not pass silently")
	}
}

func TestHeaderCheckerOnTheLabBackend(t *testing.T) {
	h := http.Header{}
	backend.SecurityHeaders(h)
	if f := CheckHeaders(h); len(f) != 0 {
		t.Errorf("the backend's headers should pass cleanly: %+v", f)
	}
}

func TestHeaderCheckerCatchesWhatIsMissing(t *testing.T) {
	h := http.Header{}
	h.Set("X-Powered-By", "Express")
	h.Set("Server", "nginx/1.18.0")
	h.Set("Content-Security-Policy", "default-src 'self' 'unsafe-inline'")
	h.Set("Strict-Transport-Security", "max-age=3600")
	got := map[string]string{}
	for _, f := range CheckHeaders(h) {
		got[f.Header] = f.Level
	}
	for header, level := range map[string]string{
		"X-Powered-By": "fail", "Server": "warn", "Content-Security-Policy": "warn", "Strict-Transport-Security": "warn",
		"X-Content-Type-Options": "fail", "X-Frame-Options": "fail", "Referrer-Policy": "warn", "Permissions-Policy": "warn",
	} {
		if got[header] != level {
			t.Errorf("%s: got %q want %q (all: %v)", header, got[header], level, got)
		}
	}
	if !HasFailures(CheckHeaders(h)) {
		t.Error("HasFailures should be true")
	}
}
