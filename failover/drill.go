package failover

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Scenario names a drill.
type Scenario string

const (
	Crash     Scenario = "crash"     // primary region dies
	Partition Scenario = "partition" // orchestrator loses the primary, clients still reach it
	LagGate   Scenario = "laggate"   // replica far behind: gate refuses, then operator forces
)

// Params configures a drill. Zero values get lab defaults.
type Params struct {
	Scenario   Scenario
	ReplLag    time.Duration
	WriteEvery time.Duration
	Config     Config
}

// Result is what a drill measured. Every field comes from timestamps taken
// during the run, not from the configuration.
type Result struct {
	Scenario      Scenario
	Writes        int           // acknowledged by some region
	Failed        int           // client errors (includes breaker rejections)
	BreakerOpened bool          // client breaker rejected at least one call
	Detect        time.Duration // fault to quorum decision
	RTO           time.Duration // fault to first acknowledged write on the new primary
	Lost          int           // acknowledged writes missing on the new primary
	RPOWindow     time.Duration // fault time minus the oldest lost write's ack time
	LagAtDecision uint64
	Gated         bool
	StaleAccepted int // writes accepted by the old primary after the new one was promoted
	Timeline      []string
}

func defaults(p *Params) {
	if p.ReplLag == 0 {
		p.ReplLag = 40 * time.Millisecond
	}
	if p.WriteEvery == 0 {
		p.WriteEvery = 5 * time.Millisecond
	}
	c := &p.Config
	if c.Interval == 0 {
		c.Interval = 40 * time.Millisecond
	}
	if c.Timeout == 0 {
		c.Timeout = 100 * time.Millisecond
	}
	if c.FailThreshold == 0 {
		c.FailThreshold = 3
	}
	if c.Vantages == 0 {
		c.Vantages = 3
	}
	if c.Quorum == 0 {
		c.Quorum = 2
	}
	if c.LeaseTTL == 0 {
		c.LeaseTTL = 300 * time.Millisecond
	}
	if c.MaxLagOps == 0 {
		c.MaxLagOps = 50
	}
}

type ack struct {
	key string
	at  time.Time
}

// RunDrill starts two regions, replication, the orchestrator and a writer,
// injects the fault, waits for recovery and returns the measurements.
func RunDrill(p Params) (Result, error) {
	defaults(&p)
	res := Result{Scenario: p.Scenario}
	a, err := NewRegion("us-west-2", true, p.Config.LeaseTTL)
	if err != nil {
		return res, err
	}
	b, err := NewRegion("us-east-1", false, p.Config.LeaseTTL)
	if err != nil {
		return res, err
	}
	defer a.Kill()
	defer b.Kill()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Replicate(ctx, a, b, p.ReplLag)

	rt := &Router{}
	orch := NewOrchestrator(p.Config, a, b, NewFenceStore(1), rt)
	go orch.Run(ctx)

	br := NewBreaker(3, 100*time.Millisecond)
	cli := NewClient(rt, br, 150*time.Millisecond)

	var (
		mu      sync.Mutex
		acks    []ack
		failed  int
		sawOpen bool
		t0      time.Time
		rtoAt   time.Time
	)
	wctx, wcancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(p.WriteEvery)
		defer t.Stop()
		for i := 0; ; i++ {
			select {
			case <-wctx.Done():
				return
			case <-t.C:
			}
			key := fmt.Sprintf("k%06d", i)
			region, err := cli.Write(key, "v")
			now := time.Now()
			mu.Lock()
			if err == nil {
				acks = append(acks, ack{key, now})
				if !t0.IsZero() && rtoAt.IsZero() && region == b.Name {
					rtoAt = now
				}
			} else {
				failed++
				if err == ErrOpen {
					sawOpen = true
				}
			}
			mu.Unlock()
		}
	}()

	time.Sleep(800 * time.Millisecond) // steady state
	mu.Lock()
	t0 = time.Now()
	mu.Unlock()
	switch p.Scenario {
	case Crash, LagGate:
		a.Kill()
	case Partition:
		a.SetChaos(Chaos{DropFrom: "orchestrator"})
		for i := 0; i < p.Config.Vantages; i++ {
			orch.BlockVantage(i, true)
		}
	}

	deadline := time.Now().Add(4 * time.Second)
	forced := false
	for time.Now().Before(deadline) {
		mu.Lock()
		got := !rtoAt.IsZero()
		mu.Unlock()
		if got {
			break
		}
		if p.Scenario == LagGate && orch.Gated() && !forced {
			time.Sleep(500 * time.Millisecond) // show the gate holding
			res.LagAtDecision = orch.Lag()
			orch.SetForce(true)
			forced = true
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond) // let replication drain what it can
	wcancel()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	res.Writes, res.Failed, res.BreakerOpened, res.Gated = len(acks), failed, sawOpen, orch.Gated()
	if !rtoAt.IsZero() {
		res.RTO = rtoAt.Sub(t0)
	}
	var promoted time.Time
	for _, e := range orch.Events() {
		res.Timeline = append(res.Timeline, fmt.Sprintf("+%6dms %s", e.At.Sub(t0).Milliseconds(), e.What))
		if len(e.What) > 8 && e.What[:8] == "promoted" {
			promoted = e.At
		}
		if e.What == "quorum says primary down" && res.Detect == 0 {
			res.Detect = e.At.Sub(t0)
		}
	}
	var oldest time.Time
	for _, k := range acks {
		if !b.Has(k.key) {
			res.Lost++
			if oldest.IsZero() || k.at.Before(oldest) {
				oldest = k.at
			}
		}
	}
	if !oldest.IsZero() {
		res.RPOWindow = t0.Sub(oldest)
		if res.RPOWindow < 0 {
			res.RPOWindow = 0
		}
	}
	if !promoted.IsZero() && p.Scenario == Partition {
		a.mu.Lock()
		for _, e := range a.log {
			if e.At.After(promoted) {
				res.StaleAccepted++
			}
		}
		a.mu.Unlock()
	}
	return res, nil
}
