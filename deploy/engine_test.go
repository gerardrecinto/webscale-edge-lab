package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func countType(r Report, typ string) int {
	n := 0
	for _, e := range r.Events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func TestRollingAcrossAThousandHostsHoldsTheLimit(t *testing.T) {
	s := NewSim(1000, "v1", time.Millisecond)
	var e Engine
	rep := e.Run(context.Background(), s.Targets(), Plan{Version: "v2", MaxUnavailable: PercentBatch(1000, 5)})
	if rep.Status != StatusSucceeded {
		t.Fatalf("status %s: %s", rep.Status, rep.Reason)
	}
	if rep.PeakOut > 50 {
		t.Errorf("controller counted %d out at once, limit 50", rep.PeakOut)
	}
	if min := s.MinInRotation(); min < 950 {
		t.Errorf("fleet dipped to %d in rotation, limit is 950", min)
	}
	if v := s.Versions(); v["v2"] != 1000 {
		t.Errorf("versions %v", v)
	}
	t.Logf("1000 hosts, batch 50: %s, peak out %d, fewest in rotation %d", rep.Duration.Round(time.Millisecond), rep.PeakOut, s.MinInRotation())
}

func TestFailureAtOneHostRollsEverythingBack(t *testing.T) {
	s := NewSim(1000, "v1", time.Millisecond)
	s.Host("host-0437").FailAt("v2", "verify", "readiness probe returned 503")
	var e Engine
	rep := e.Run(context.Background(), s.Targets(), Plan{Version: "v2", MaxUnavailable: 50})
	if rep.Status != StatusRolledBack {
		t.Fatalf("status %s: %s", rep.Status, rep.Reason)
	}
	if rep.FailedHost != "host-0437" {
		t.Errorf("failed host %q", rep.FailedHost)
	}
	if v := s.Versions(); v["v1"] != 1000 {
		t.Errorf("versions after rollback %v", v)
	}
	if rep.PeakOut > 50 {
		t.Errorf("peak out %d during rollback, limit 50", rep.PeakOut)
	}
	if s.MinInRotation() < 950 {
		t.Errorf("fleet dipped to %d during rollback", s.MinInRotation())
	}
	if countType(rep, "rollback_done") != 1 {
		t.Error("no rollback_done event")
	}
	// Hosts after the failing batch were never touched.
	if n := countType(rep, "target_enable"); n > 450+50 {
		t.Errorf("%d targets were enabled on v2, the rollout should have stopped near host 437", n)
	}
}

func TestRollbackThatCannotFinishIsReportedNotHidden(t *testing.T) {
	s := NewSim(20, "v1", 0)
	h := s.Host("host-0003")
	h.FailAt("v2", "verify", "bad build")
	h.FailAt("v1", "enable", "balancer unreachable")
	var e Engine
	rep := e.Run(context.Background(), s.Targets(), Plan{Version: "v2", MaxUnavailable: 4})
	if rep.Status != StatusRollbackFailed {
		t.Fatalf("status %s", rep.Status)
	}
	if !strings.Contains(rep.Reason, "manual action") {
		t.Errorf("reason %q", rep.Reason)
	}
}

func observer(canaryReq, canaryErr int) Observer {
	return func(ctx context.Context, since time.Time) (Window, Window, error) {
		return Window{Requests: canaryReq, Errors: canaryErr, P95: 20 * time.Millisecond}, Window{Requests: 5000, Errors: 5, P95: 20 * time.Millisecond}, nil
	}
}

func TestCanaryThatLooksBadStopsAfterTheCanaryHosts(t *testing.T) {
	s := NewSim(100, "v1", 0)
	var e Engine
	rep := e.Run(context.Background(), s.Targets(), Plan{
		Version: "v2", MaxUnavailable: 10, Canary: 2, CanaryWindow: time.Millisecond,
		Observe: observer(400, 120), Judge: Judge{MinRequests: 100, MaxErrorRate: 0.02, MaxErrorGap: 0.01},
	})
	if rep.Status != StatusRolledBack || !strings.Contains(rep.Reason, "canary") {
		t.Fatalf("status %s reason %q", rep.Status, rep.Reason)
	}
	if v := s.Versions(); v["v1"] != 100 {
		t.Errorf("versions %v", v)
	}
	if n := countType(rep, "target_enable"); n != 2 {
		t.Errorf("%d targets were updated, only the 2 canaries should have been", n)
	}
}

func TestCanaryFailsClosedWithoutEnoughTraffic(t *testing.T) {
	s := NewSim(30, "v1", 0)
	var e Engine
	rep := e.Run(context.Background(), s.Targets(), Plan{
		Version: "v2", MaxUnavailable: 5, Canary: 1, CanaryWindow: time.Millisecond,
		Observe: observer(3, 0), Judge: Judge{MinRequests: 100, MaxErrorRate: 0.02, MaxErrorGap: 0.01},
	})
	if rep.Status != StatusRolledBack || !strings.Contains(rep.Reason, "only 3 canary requests") {
		t.Fatalf("status %s reason %q", rep.Status, rep.Reason)
	}
}

func TestGoodCanaryContinuesToTheWholeFleet(t *testing.T) {
	s := NewSim(60, "v1", 0)
	var e Engine
	rep := e.Run(context.Background(), s.Targets(), Plan{
		Version: "v2", MaxUnavailable: 6, Canary: 3, CanaryWindow: time.Millisecond,
		Observe: observer(500, 2), Judge: Judge{MinRequests: 100, MaxErrorRate: 0.02, MaxErrorGap: 0.01, MaxP95Ratio: 1.5},
	})
	if rep.Status != StatusSucceeded {
		t.Fatalf("status %s: %s", rep.Status, rep.Reason)
	}
	if s.Versions()["v2"] != 60 || rep.Canary == nil || countType(rep, "canary_pass") != 1 {
		t.Errorf("versions %v canary %v", s.Versions(), rep.Canary)
	}
}

func TestObserverErrorRollsBackInsteadOfGuessing(t *testing.T) {
	s := NewSim(10, "v1", 0)
	var e Engine
	rep := e.Run(context.Background(), s.Targets(), Plan{
		Version: "v2", MaxUnavailable: 2, Canary: 1, CanaryWindow: time.Millisecond,
		Observe: func(context.Context, time.Time) (Window, Window, error) {
			return Window{}, Window{}, errors.New("metrics store down")
		},
	})
	if rep.Status != StatusRolledBack || s.Versions()["v1"] != 10 {
		t.Fatalf("status %s versions %v", rep.Status, s.Versions())
	}
}

func TestAbortedRunResumesWithoutRepeatingFinishedHosts(t *testing.T) {
	s := NewSim(100, "v1", time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	var seen atomic.Int64
	e := Engine{Emit: func(ev Event) {
		if ev.Type == "target_enable" && seen.Add(1) == 30 {
			cancel()
		}
	}}
	rep := e.Run(ctx, s.Targets(), Plan{Version: "v2", MaxUnavailable: 10})
	if rep.Status != StatusAborted {
		t.Fatalf("status %s: %s", rep.Status, rep.Reason)
	}
	done := s.Versions()["v2"]
	if done < 30 || done >= 100 {
		t.Fatalf("%d hosts on v2 after abort", done)
	}
	var e2 Engine
	rep2 := e2.Run(context.Background(), s.Targets(), Plan{Version: "v2", MaxUnavailable: 10})
	if rep2.Status != StatusSucceeded || rep2.Skipped != done {
		t.Fatalf("resume: status %s skipped %d, want %d", rep2.Status, rep2.Skipped, done)
	}
	if s.Versions()["v2"] != 100 {
		t.Errorf("versions %v", s.Versions())
	}
	for _, h := range s.States() {
		if h.State != "serving" {
			t.Fatalf("%s is %s after the resumed run, it must be back in rotation", h.ID, h.State)
		}
	}
}

func TestJudge(t *testing.T) {
	j := Judge{MinRequests: 100, MaxErrorRate: 0.05, MaxErrorGap: 0.01, MaxP95Ratio: 2}
	base := Window{Requests: 1000, Errors: 1, P95: 10 * time.Millisecond}
	cases := []struct {
		name string
		w    Window
		ok   bool
	}{
		{"healthy", Window{Requests: 200, Errors: 1, P95: 12 * time.Millisecond}, true},
		{"too few", Window{Requests: 10}, false},
		{"over ceiling", Window{Requests: 200, Errors: 20, P95: 10 * time.Millisecond}, false},
		{"worse than baseline", Window{Requests: 200, Errors: 6, P95: 10 * time.Millisecond}, false},
		{"slow", Window{Requests: 200, Errors: 0, P95: 25 * time.Millisecond}, false},
	}
	for _, c := range cases {
		if ok, why := j.Verdict(c.w, base); ok != c.ok {
			t.Errorf("%s: got %v (%s)", c.name, ok, why)
		}
	}
}

func TestPercentBatch(t *testing.T) {
	for _, c := range []struct {
		n    int
		p    float64
		want int
	}{{1000, 5, 50}, {10, 5, 1}, {3, 10, 1}, {200, 25, 50}} {
		if got := PercentBatch(c.n, c.p); got != c.want {
			t.Errorf("PercentBatch(%d,%v)=%d want %d", c.n, c.p, got, c.want)
		}
	}
}

func ExampleEngine_Run() {
	s := NewSim(6, "v1", 0)
	var e Engine
	rep := e.Run(context.Background(), s.Targets(), Plan{Version: "v2", MaxUnavailable: 2})
	fmt.Println(rep.Status, rep.PeakOut <= 2, s.Versions())
	// Output: succeeded true map[v2:6]
}
