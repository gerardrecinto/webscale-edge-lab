package service

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gerardrecinto/webscale-edge-lab/deploy"
	"github.com/gerardrecinto/webscale-edge-lab/secgate"
)

func start(t *testing.T, hosts int, step time.Duration, token string) (*Service, *httptest.Server) {
	t.Helper()
	s := New(deploy.NewSim(hosts, "v1", step), token)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

func do(t *testing.T, method, url, token string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func waitDone(t *testing.T, base, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_, v := do(t, "GET", base+"/api/deployments/"+id, "", nil)
		if v["running"] == false {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("deployment did not finish")
	return nil
}

func status(v map[string]any) string { return v["report"].(map[string]any)["status"].(string) }

func TestDeployFlow(t *testing.T) {
	s, srv := start(t, 40, time.Millisecond, "")
	code, r := do(t, "POST", srv.URL+"/api/deployments", "", Request{Version: "v2", BatchPercent: 10})
	if code != 202 {
		t.Fatalf("%d %v", code, r)
	}
	v := waitDone(t, srv.URL, r["id"].(string))
	if status(v) != "succeeded" {
		t.Fatalf("%v", v["report"])
	}
	if got := s.Fleet.Versions(); got["v2"] != 40 {
		t.Errorf("%v", got)
	}
	if peak := v["progress"].(map[string]any)["peakOut"].(float64); peak > 4 {
		t.Errorf("peak out %v with a limit of 4", peak)
	}
	if min := v["minInRotation"].(float64); min < 36 {
		t.Errorf("fleet dipped to %v serving, the limit is 36", min)
	}
	_, f := do(t, "GET", srv.URL+"/api/fleet", "", nil)
	if f["total"].(float64) != 40 || len(f["hosts"].([]any)) != 40 {
		t.Errorf("fleet %v", f["total"])
	}
}

func TestOnlyOneDeploymentAtATime(t *testing.T) {
	_, srv := start(t, 50, 20*time.Millisecond, "")
	code, r := do(t, "POST", srv.URL+"/api/deployments", "", Request{Version: "v2", BatchPercent: 10})
	if code != 202 {
		t.Fatal(code, r)
	}
	if code, r := do(t, "POST", srv.URL+"/api/deployments", "", Request{Version: "v3", BatchPercent: 10}); code != 409 {
		t.Fatalf("second deployment: %d %v", code, r)
	}
	do(t, "POST", srv.URL+"/api/deployments/"+r["id"].(string)+"/cancel", "", nil)
	waitDone(t, srv.URL, r["id"].(string))
}

func TestValidation(t *testing.T) {
	_, srv := start(t, 20, 0, "")
	for name, body := range map[string]any{
		"empty version":     Request{BatchPercent: 5},
		"shell in version":  Request{Version: "v2; rm -rf /", BatchPercent: 5},
		"zero batch":        Request{Version: "v2"},
		"huge batch":        Request{Version: "v2", BatchPercent: 90},
		"too many canaries": Request{Version: "v2", BatchPercent: 5, Canary: 15},
		"unknown host":      Request{Version: "v2", BatchPercent: 5, FailHost: "host-9999"},
		"unknown field":     map[string]any{"version": "v2", "batchPercent": 5, "sudo": true},
	} {
		if code, r := do(t, "POST", srv.URL+"/api/deployments", "", body); code != 400 {
			t.Errorf("%s: got %d %v", name, code, r)
		}
	}
	if code, _ := do(t, "GET", srv.URL+"/api/deployments/nope", "", nil); code != 404 {
		t.Errorf("unknown id: %d", code)
	}
}

func TestInjectedHostFailureRollsBack(t *testing.T) {
	s, srv := start(t, 50, time.Millisecond, "")
	_, r := do(t, "POST", srv.URL+"/api/deployments", "", Request{Version: "v2", BatchPercent: 10, FailHost: "host-0042"})
	v := waitDone(t, srv.URL, r["id"].(string))
	if status(v) != "rolled_back" || v["report"].(map[string]any)["failedTarget"] != "host-0042" {
		t.Fatalf("%v", v["report"])
	}
	if got := s.Fleet.Versions(); got["v1"] != 50 {
		t.Errorf("%v", got)
	}
}

func TestBadCanaryRollsBack(t *testing.T) {
	s, srv := start(t, 50, time.Millisecond, "")
	_, r := do(t, "POST", srv.URL+"/api/deployments", "", Request{Version: "v2", BatchPercent: 10, Canary: 2, BadCanary: true})
	v := waitDone(t, srv.URL, r["id"].(string))
	if status(v) != "rolled_back" || !strings.Contains(v["report"].(map[string]any)["reason"].(string), "canary") {
		t.Fatalf("%v", v["report"])
	}
	if got := s.Fleet.Versions(); got["v1"] != 50 {
		t.Errorf("%v", got)
	}
}

func TestGoodCanaryContinues(t *testing.T) {
	s, srv := start(t, 50, time.Millisecond, "")
	_, r := do(t, "POST", srv.URL+"/api/deployments", "", Request{Version: "v2", BatchPercent: 10, Canary: 2})
	if v := waitDone(t, srv.URL, r["id"].(string)); status(v) != "succeeded" {
		t.Fatalf("%v", v["report"])
	}
	if s.Fleet.Versions()["v2"] != 50 {
		t.Errorf("%v", s.Fleet.Versions())
	}
}

func TestCancelAborts(t *testing.T) {
	_, srv := start(t, 60, 10*time.Millisecond, "")
	_, r := do(t, "POST", srv.URL+"/api/deployments", "", Request{Version: "v2", BatchPercent: 5})
	time.Sleep(100 * time.Millisecond)
	do(t, "POST", srv.URL+"/api/deployments/"+r["id"].(string)+"/cancel", "", nil)
	if v := waitDone(t, srv.URL, r["id"].(string)); status(v) != "aborted" {
		t.Fatalf("%v", v["report"])
	}
}

func TestTokenProtectsWritesOnly(t *testing.T) {
	_, srv := start(t, 10, 0, "s3cret")
	body := Request{Version: "v2", BatchPercent: 20}
	if code, _ := do(t, "POST", srv.URL+"/api/deployments", "", body); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := do(t, "POST", srv.URL+"/api/deployments", "wrong", body); code != 401 {
		t.Errorf("wrong token: %d", code)
	}
	if code, _ := do(t, "GET", srv.URL+"/api/fleet", "", nil); code != 200 {
		t.Errorf("reads should be open: %d", code)
	}
	code, r := do(t, "POST", srv.URL+"/api/deployments", "s3cret", body)
	if code != 202 {
		t.Fatalf("right token: %d %v", code, r)
	}
	waitDone(t, srv.URL, r["id"].(string))
}

func TestPageAndHeaders(t *testing.T) {
	_, srv := start(t, 5, 0, "")
	for _, path := range []string{"/", "/app.js", "/style.css", "/api/fleet"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
		if f := secgate.CheckHeaders(resp.Header); secgate.HasFailures(f) {
			t.Errorf("%s fails the header check: %+v", path, f)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") {
			t.Errorf("%s CSP %q", path, csp)
		}
	}
	resp, _ := http.Get(srv.URL + "/")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), "<script>") || strings.Contains(string(b), "onclick=") || strings.Contains(string(b), "style=") {
		t.Error("the page has inline script or style, which the CSP would block")
	}
}

func TestMetrics(t *testing.T) {
	_, srv := start(t, 20, 0, "")
	_, r := do(t, "POST", srv.URL+"/api/deployments", "", Request{Version: "v2", BatchPercent: 10})
	waitDone(t, srv.URL, r["id"].(string))
	resp, _ := http.Get(srv.URL + "/metrics")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{`pushbutton_deployments_total{status="succeeded"} 1`, `pushbutton_fleet_targets{version="v2"} 20`, "pushbutton_peak_unavailable"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("metrics missing %q:\n%s", want, b)
		}
	}
}

func TestEventsReachTheEmitHook(t *testing.T) {
	s, srv := start(t, 10, 0, "")
	var mu sync.Mutex // Emit is called from several goroutines
	var n int
	var ids = map[string]bool{}
	s.Emit = func(id string, e deploy.Event) { mu.Lock(); n++; ids[id] = true; mu.Unlock() }
	_, r := do(t, "POST", srv.URL+"/api/deployments", "", Request{Version: "v2", BatchPercent: 20})
	waitDone(t, srv.URL, r["id"].(string))
	mu.Lock()
	defer mu.Unlock()
	if n < 40 || !ids[r["id"].(string)] {
		t.Errorf("%d events for %v", n, ids)
	}
}
