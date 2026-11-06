// Package deploy rolls a new build across a fleet without taking the service
// down. It knows nothing about a particular proxy: a Target is one host or
// instance that can be taken out of rotation, updated, checked and put back.
//
// The rules it enforces:
//   - at most MaxUnavailable targets are out of rotation at any moment
//   - a target is only put back after the new build passes its own readiness check
//   - the first failure stops the rollout and rolls every touched target back
//   - with a canary stage, the new build must look as good as the old one on
//     live traffic before the rest of the fleet is touched
package deploy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Target is one host or instance behind the load balancer.
type Target interface {
	ID() string
	// Version is the build currently installed.
	Version() string
	// InRotation reports whether the balancer is currently sending it traffic.
	InRotation() bool
	// Drain stops new traffic and returns once in-flight requests have finished.
	Drain(ctx context.Context) error
	// Update installs and starts the given build while the target is out of rotation.
	Update(ctx context.Context, version string) error
	// Verify checks the target directly, not through the load balancer, and
	// fails if it is not ready or is not running the expected build.
	Verify(ctx context.Context, version string) error
	// Enable puts the target back in rotation and returns once the balancer
	// reports it healthy.
	Enable(ctx context.Context) error
}

// Window is what live traffic saw from one build over the observation period.
type Window struct {
	Requests int
	Errors   int
	P95      time.Duration
}

func (w Window) ErrorRate() float64 {
	if w.Requests == 0 {
		return 0
	}
	return float64(w.Errors) / float64(w.Requests)
}

// Observer reports the new build's window and the old build's window since a time.
type Observer func(ctx context.Context, since time.Time) (canary, baseline Window, err error)

// Judge decides whether a canary is good enough to continue.
type Judge struct {
	MinRequests  int     // fewer requests than this is not enough evidence; the canary fails closed
	MaxErrorRate float64 // absolute ceiling on the canary's error rate
	MaxErrorGap  float64 // how much worse than the baseline the error rate may be
	MaxP95Ratio  float64 // canary p95 may be at most this many times the baseline p95, 0 = not checked
}

func (j Judge) Verdict(canary, baseline Window) (bool, string) {
	if canary.Requests < j.MinRequests {
		return false, fmt.Sprintf("only %d canary requests, need %d", canary.Requests, j.MinRequests)
	}
	if canary.ErrorRate() > j.MaxErrorRate {
		return false, fmt.Sprintf("canary error rate %.1f%% is over the %.1f%% ceiling", 100*canary.ErrorRate(), 100*j.MaxErrorRate)
	}
	if canary.ErrorRate()-baseline.ErrorRate() > j.MaxErrorGap {
		return false, fmt.Sprintf("canary error rate %.1f%% against baseline %.1f%%", 100*canary.ErrorRate(), 100*baseline.ErrorRate())
	}
	if j.MaxP95Ratio > 0 && baseline.P95 > 0 && float64(canary.P95) > j.MaxP95Ratio*float64(baseline.P95) {
		return false, fmt.Sprintf("canary p95 %s is more than %.1fx the baseline %s", canary.P95, j.MaxP95Ratio, baseline.P95)
	}
	return true, fmt.Sprintf("canary %d requests, %.2f%% errors, baseline %.2f%%", canary.Requests, 100*canary.ErrorRate(), 100*baseline.ErrorRate())
}

type Plan struct {
	Version        string
	MaxUnavailable int           // batch size and the cap on targets out at once, at least 1
	Canary         int           // targets updated first and judged before the rest; 0 = plain rolling
	CanaryWindow   time.Duration // how long to watch the canary
	Observe        Observer      // required when Canary > 0
	Judge          Judge
	StepTimeout    time.Duration // for one target's drain, update, verify and enable; default 60s
	Settle         time.Duration // pause after each batch
}

// PercentBatch converts "at most p percent unavailable" into a batch size for n targets, at least 1.
func PercentBatch(n int, p float64) int {
	b := int(float64(n) * p / 100)
	if b < 1 {
		b = 1
	}
	return b
}

type Event struct {
	At      time.Time `json:"at"`
	Type    string    `json:"type"`
	Target  string    `json:"target,omitempty"`
	Version string    `json:"version,omitempty"`
	Detail  string    `json:"detail,omitempty"`
}

const (
	StatusSucceeded      = "succeeded"
	StatusRolledBack     = "rolled_back"
	StatusRollbackFailed = "rollback_failed"
	StatusAborted        = "aborted"
	StatusRunning        = "running"
)

type Report struct {
	Version    string        `json:"version"`
	Status     string        `json:"status"`
	Reason     string        `json:"reason,omitempty"`
	Targets    int           `json:"targets"`
	Updated    int           `json:"updated"`
	Skipped    int           `json:"skipped"` // already on the requested build
	PeakOut    int           `json:"peakOut"` // most targets out of rotation at once
	Duration   time.Duration `json:"durationNs"`
	Events     []Event       `json:"events"`
	Canary     *Window       `json:"canary,omitempty"`
	Baseline   *Window       `json:"baseline,omitempty"`
	FailedHost string        `json:"failedTarget,omitempty"`
}

// Progress is a cheap snapshot for a UI.
type Progress struct {
	Status  string `json:"status"`
	Phase   string `json:"phase"`
	Total   int    `json:"total"`
	Done    int    `json:"done"`
	Out     int    `json:"out"`
	PeakOut int    `json:"peakOut"`
}

type Engine struct {
	// Emit is called for every event, from several goroutines, so it must be safe for concurrent use.
	Emit func(Event)

	mu       sync.Mutex
	events   []Event
	out      atomic.Int64
	peak     atomic.Int64
	done     atomic.Int64
	total    atomic.Int64
	status   atomic.Value
	phase    atomic.Value
	prevVers map[string]string
	outSet   map[string]bool // targets currently out of rotation

	finishOnly map[string]bool // already on the build, only need verify and enable
}

func (e *Engine) Snapshot() Progress {
	st, _ := e.status.Load().(string)
	ph, _ := e.phase.Load().(string)
	return Progress{Status: st, Phase: ph, Total: int(e.total.Load()), Done: int(e.done.Load()), Out: int(e.out.Load()), PeakOut: int(e.peak.Load())}
}

func (e *Engine) emit(typ, target, version, detail string) {
	ev := Event{At: time.Now(), Type: typ, Target: target, Version: version, Detail: detail}
	e.mu.Lock()
	e.events = append(e.events, ev)
	e.mu.Unlock()
	if e.Emit != nil {
		e.Emit(ev)
	}
}

// setOut records that a target is out of rotation (true) or back (false). It
// is idempotent, so a target that failed mid-update and is later rolled back is
// counted once.
func (e *Engine) setOut(id string, out bool) {
	e.mu.Lock()
	if e.outSet == nil {
		e.outSet = map[string]bool{}
	}
	was := e.outSet[id]
	if out == was {
		e.mu.Unlock()
		return
	}
	if out {
		e.outSet[id] = true
	} else {
		delete(e.outSet, id)
	}
	e.mu.Unlock()
	if out {
		e.markOut(1)
	} else {
		e.markOut(-1)
	}
}

func (e *Engine) isOut(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.outSet[id]
}

func (e *Engine) markOut(delta int64) {
	n := e.out.Add(delta)
	for {
		p := e.peak.Load()
		if n <= p || e.peak.CompareAndSwap(p, n) {
			return
		}
	}
}

var errAbort = errors.New("deployment cancelled")

// Run rolls the plan's version across targets. It is safe to run again after an
// abort: targets already on the version are skipped.
func (e *Engine) Run(ctx context.Context, targets []Target, p Plan) Report {
	start := time.Now()
	if p.MaxUnavailable < 1 {
		p.MaxUnavailable = 1
	}
	if p.StepTimeout == 0 {
		p.StepTimeout = 60 * time.Second
	}
	e.out.Store(0)
	e.peak.Store(0)
	e.done.Store(0)
	e.mu.Lock()
	e.outSet = map[string]bool{}
	e.mu.Unlock()
	e.finishOnly = map[string]bool{}
	e.status.Store(StatusRunning)
	e.phase.Store("planning")
	e.mu.Lock()
	e.events = nil
	e.mu.Unlock()

	rep := Report{Version: p.Version, Targets: len(targets)}
	finish := func(status, reason string) Report {
		rep.Status, rep.Reason = status, reason
		rep.PeakOut = int(e.peak.Load())
		rep.Duration = time.Since(start)
		e.mu.Lock()
		rep.Events = append([]Event(nil), e.events...)
		e.mu.Unlock()
		e.status.Store(status)
		e.phase.Store("finished")
		return rep
	}
	if p.Canary > 0 && p.Observe == nil {
		return finish(StatusAborted, "a canary stage needs an Observer")
	}

	prev := map[string]string{}
	var todo []Target
	for _, t := range targets {
		prev[t.ID()] = t.Version()
		if t.Version() == p.Version {
			if t.InRotation() {
				rep.Skipped++
				continue
			}
			// A previous run installed the build and stopped before putting
			// the target back. Finish that: verify it and enable it.
			e.finishOnly[t.ID()] = true
		}
		todo = append(todo, t)
	}
	sort.SliceStable(todo, func(i, j int) bool { return todo[i].ID() < todo[j].ID() })
	e.total.Store(int64(len(todo)))
	e.prevVers = prev
	e.emit("plan", "", p.Version, fmt.Sprintf("%d targets, %d already current, batch %d, canary %d", len(targets), rep.Skipped, p.MaxUnavailable, p.Canary))

	var touched []Target // every target whose build may have changed, in order
	rollback := func(reason string, failed string) Report {
		rep.FailedHost = failed
		e.phase.Store("rollback")
		e.emit("rollback_start", "", "", reason)
		ok := e.rollback(ctx, touched, p)
		if !ok {
			return finish(StatusRollbackFailed, reason+"; rollback did not complete, manual action needed")
		}
		return finish(StatusRolledBack, reason)
	}

	i := 0
	if p.Canary > 0 && p.Canary < len(todo) {
		e.phase.Store("canary")
		stageStart := time.Now()
		batch := todo[:p.Canary]
		i = p.Canary
		failed, err := e.updateBatch(ctx, batch, p, &touched)
		if err != nil {
			if errors.Is(err, errAbort) {
				return finish(StatusAborted, err.Error())
			}
			return rollback(err.Error(), failed)
		}
		e.emit("canary_watch", "", p.Version, fmt.Sprintf("watching %s", p.CanaryWindow))
		select {
		case <-ctx.Done():
			return finish(StatusAborted, errAbort.Error())
		case <-time.After(p.CanaryWindow):
		}
		cw, bw, err := p.Observe(ctx, stageStart)
		if err != nil {
			return rollback("canary observation failed: "+err.Error(), "")
		}
		rep.Canary, rep.Baseline = &cw, &bw
		ok, why := p.Judge.Verdict(cw, bw)
		if !ok {
			e.emit("canary_fail", "", p.Version, why)
			return rollback("canary failed: "+why, "")
		}
		e.emit("canary_pass", "", p.Version, why)
	}

	e.phase.Store("rollout")
	for i < len(todo) {
		end := i + p.MaxUnavailable
		if end > len(todo) {
			end = len(todo)
		}
		failed, err := e.updateBatch(ctx, todo[i:end], p, &touched)
		if err != nil {
			if errors.Is(err, errAbort) {
				return finish(StatusAborted, err.Error())
			}
			return rollback(err.Error(), failed)
		}
		i = end
		if p.Settle > 0 && i < len(todo) {
			select {
			case <-ctx.Done():
				return finish(StatusAborted, errAbort.Error())
			case <-time.After(p.Settle):
			}
		}
	}
	rep.Updated = len(todo)
	e.emit("done", "", p.Version, fmt.Sprintf("%d targets updated", len(todo)))
	return finish(StatusSucceeded, "")
}

// updateBatch updates every target in the batch at once. If any fails it still
// waits for the others, so the caller sees a settled batch. It returns the
// first failure's target ID.
func (e *Engine) updateBatch(ctx context.Context, batch []Target, p Plan, touched *[]Target) (string, error) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	var firstID string
	*touched = append(*touched, batch...)
	for _, t := range batch {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			if err := e.moveTo(ctx, t, p.Version, p.StepTimeout); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr, firstID = err, t.ID()
				}
				mu.Unlock()
				return
			}
			e.done.Add(1)
		}(t)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return "", errAbort
	}
	return firstID, firstErr
}

// moveTo takes one target out, installs version, verifies and returns it to rotation.
// A failure leaves the target counted as out, because it is: rollback puts it back.
func (e *Engine) moveTo(ctx context.Context, t Target, version string, timeout time.Duration) error {
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	id := t.ID()
	e.setOut(id, true)
	steps := []struct {
		name string
		fn   func() error
	}{
		{"drain", func() error { return t.Drain(sctx) }},
		{"update", func() error { return t.Update(sctx, version) }},
		{"verify", func() error { return t.Verify(sctx, version) }},
		{"enable", func() error { return t.Enable(sctx) }},
	}
	if e.finishOnly[id] {
		steps = steps[2:]
	}
	for _, s := range steps {
		if err := s.fn(); err != nil {
			e.emit("target_failed", id, version, fmt.Sprintf("%s: %v", s.name, err))
			return fmt.Errorf("%s: %s: %w", id, s.name, err)
		}
		e.emit("target_"+s.name, id, version, "")
	}
	e.setOut(id, false)
	return nil
}

// rollback restores every touched target to the build it had before. Targets
// that failed mid-update are still out of rotation, so they go first and the
// batch limit holds throughout.
func (e *Engine) rollback(ctx context.Context, touched []Target, p Plan) bool {
	// The caller's context may be cancelled; a rollback still has to run.
	rctx := context.WithoutCancel(ctx)
	var stuck, healthy []Target
	for _, t := range touched {
		// Targets that failed mid-update are still out of rotation and already
		// count against the limit, so they are restored first.
		if e.isOut(t.ID()) {
			stuck = append(stuck, t)
		} else {
			healthy = append(healthy, t)
		}
	}
	all := append(stuck, healthy...)
	ok := true
	var mu sync.Mutex
	for i := 0; i < len(all); i += p.MaxUnavailable {
		end := i + p.MaxUnavailable
		if end > len(all) {
			end = len(all)
		}
		var wg sync.WaitGroup
		for _, t := range all[i:end] {
			wg.Add(1)
			go func(t Target) {
				defer wg.Done()
				want := e.prevVers[t.ID()]
				if err := e.rollbackOne(rctx, t, want, p.StepTimeout); err != nil {
					mu.Lock()
					ok = false
					mu.Unlock()
				}
			}(t)
		}
		wg.Wait()
	}
	if ok {
		e.emit("rollback_done", "", "", fmt.Sprintf("%d targets restored", len(all)))
	}
	return ok
}

// rollbackOne restores one target to its old build.
func (e *Engine) rollbackOne(ctx context.Context, t Target, version string, timeout time.Duration) error {
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	id := t.ID()
	e.setOut(id, true)
	for _, s := range []struct {
		name string
		fn   func() error
	}{
		{"drain", func() error { return t.Drain(sctx) }},
		{"update", func() error { return t.Update(sctx, version) }},
		{"verify", func() error { return t.Verify(sctx, version) }},
		{"enable", func() error { return t.Enable(sctx) }},
	} {
		if err := s.fn(); err != nil {
			e.emit("rollback_failed", id, version, fmt.Sprintf("%s: %v", s.name, err))
			return err
		}
	}
	e.setOut(id, false)
	e.emit("target_rolled_back", id, version, "")
	return nil
}
