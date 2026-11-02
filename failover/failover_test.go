package failover

import (
	"context"
	"testing"
	"time"
)

func TestBreakerOpensAndRecovers(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewBreaker(3, time.Second)
	b.now = func() time.Time { return now }
	fail := func() error { return context.DeadlineExceeded }
	for i := 0; i < 3; i++ {
		if err := b.Do(fail); err != context.DeadlineExceeded {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if !b.Open() {
		t.Fatal("should be open after 3 failures")
	}
	called := false
	if err := b.Do(func() error { called = true; return nil }); err != ErrOpen || called {
		t.Fatal("open breaker must reject without calling")
	}
	now = now.Add(2 * time.Second)
	if err := b.Do(func() error { return nil }); err != nil {
		t.Fatalf("half-open probe should run and succeed: %v", err)
	}
	if b.Open() {
		t.Fatal("should be closed after a good probe")
	}
}

func TestBreakerHalfOpenFailureReopens(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewBreaker(1, time.Second)
	b.now = func() time.Time { return now }
	b.Do(func() error { return context.Canceled })
	now = now.Add(2 * time.Second)
	b.Do(func() error { return context.Canceled })
	if err := b.Do(func() error { return nil }); err != ErrOpen {
		t.Fatalf("failed probe must reopen the breaker, got %v", err)
	}
}

func TestStaleEpochAndExpiredLeaseAreRejected(t *testing.T) {
	r, err := NewRegion("a", true, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Kill()
	if code, _ := WriteTo(r.URL(), 1, "x", "1"); code != 200 {
		t.Fatalf("fresh primary should accept, got %d", code)
	}
	if code, _ := WriteTo(r.URL(), 0, "x", "2"); code != 409 {
		t.Fatalf("stale epoch should be fenced, got %d", code)
	}
	time.Sleep(200 * time.Millisecond)
	if code, _ := WriteTo(r.URL(), 1, "x", "3"); code != 409 {
		t.Fatalf("expired lease should stop writes, got %d", code)
	}
}

func newPair(t *testing.T, cfg Config, lag time.Duration) (*Region, *Region, *Orchestrator, *Router, context.CancelFunc) {
	t.Helper()
	p := Params{Config: cfg}
	defaults(&p)
	a, _ := NewRegion("a", true, p.Config.LeaseTTL)
	b, _ := NewRegion("b", false, p.Config.LeaseTTL)
	ctx, cancel := context.WithCancel(context.Background())
	go Replicate(ctx, a, b, lag)
	rt := &Router{}
	o := NewOrchestrator(p.Config, a, b, NewFenceStore(1), rt)
	go o.Run(ctx)
	t.Cleanup(func() { cancel(); a.Kill(); b.Kill() })
	return a, b, o, rt, cancel
}

func TestOneBlindProberDoesNotTriggerFailover(t *testing.T) {
	_, _, o, _, _ := newPair(t, Config{}, 20*time.Millisecond)
	o.BlockVantage(0, true) // one prober loses sight of a healthy primary
	time.Sleep(700 * time.Millisecond)
	if o.Done() {
		t.Fatal("1 of 3 probers failing must not reach quorum")
	}
}

func TestGateRefusesWhenReplicaIsBehind(t *testing.T) {
	a, _, o, rt, _ := newPair(t, Config{MaxLagOps: 5}, 2*time.Second)
	for i := 0; i < 40; i++ {
		WriteTo(a.URL(), 1, "k"+string(rune('a'+i%26))+string(rune('0'+i/26)), "v")
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // a probe records the primary's sequence
	a.Kill()
	time.Sleep(900 * time.Millisecond)
	if o.Done() || !o.Gated() {
		t.Fatalf("gate should hold: done=%v gated=%v", o.Done(), o.Gated())
	}
	if rt.Get().Name != "a" {
		t.Fatal("route must not move while gated")
	}
	o.SetForce(true)
	time.Sleep(500 * time.Millisecond)
	if !o.Done() {
		t.Fatal("force should override the gate")
	}
}

func TestCrashDrill(t *testing.T) {
	r, err := RunDrill(Params{Scenario: Crash})
	if err != nil {
		t.Fatal(err)
	}
	if r.RTO == 0 || r.RTO > 1500*time.Millisecond {
		t.Fatalf("RTO %v out of range", r.RTO)
	}
	if r.RPOWindow > 40*time.Millisecond+150*time.Millisecond {
		t.Fatalf("lost window %v is much larger than replication lag", r.RPOWindow)
	}
	if !r.BreakerOpened {
		t.Fatal("client breaker should have rejected calls during the outage")
	}
}

func TestPartitionDrillHasNoSplitBrain(t *testing.T) {
	r, err := RunDrill(Params{Scenario: Partition})
	if err != nil {
		t.Fatal(err)
	}
	if r.RTO == 0 {
		t.Fatal("never recovered")
	}
	if r.StaleAccepted != 0 {
		t.Fatalf("old primary accepted %d writes after the new one was promoted", r.StaleAccepted)
	}
}
