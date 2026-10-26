// Package scenarios runs the same situations through HAProxy and Envoy and
// returns what clients actually saw. Tests assert on the results and
// cmd/record writes them for the Pages viewer.
package scenarios

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/gerardrecinto/webscale-edge-lab/backend"
	"github.com/gerardrecinto/webscale-edge-lab/load"
	"github.com/gerardrecinto/webscale-edge-lab/proxy"
)

type Bucket struct {
	T   int64 `json:"t"`
	OK  int   `json:"ok"`
	Err int   `json:"err"`
}

type Result struct {
	Scenario string           `json:"scenario"`
	Variant  string           `json:"variant"` // e.g. haproxy, haproxy+passive, envoy
	Metrics  map[string]any   `json:"metrics"`
	Timeline []Bucket         `json:"timeline,omitempty"`
	ByBack   map[string][]int `json:"byBackend,omitempty"` // per-backend served count per bucket
	Notes    []string         `json:"notes,omitempty"`
}

type Variant struct {
	Name    string
	Kind    proxy.Kind
	Passive bool
}

var Variants = []Variant{{"haproxy", proxy.HAProxy, false}, {"haproxy+passive", proxy.HAProxy, true}, {"envoy", proxy.Envoy, false}}

type env struct {
	bs  []*backend.Backend
	p   *proxy.Proxy
	cfg proxy.Config
}

func setup(v Variant, n int, cfg proxy.Config) (*env, error) {
	e := &env{cfg: cfg}
	for i := 0; i < n; i++ {
		b, err := backend.New(fmt.Sprintf("b%d", i))
		if err != nil {
			return nil, err
		}
		e.bs = append(e.bs, b)
		cfg.Backends = append(cfg.Backends, b.Addr())
		cfg.Names = append(cfg.Names, b.ID)
	}
	cfg.Passive = v.Passive
	p, err := proxy.Start(v.Kind, cfg)
	if err != nil {
		e.close()
		return nil, err
	}
	e.p = p
	return e, nil
}

func (e *env) close() {
	if e.p != nil {
		e.p.Stop()
	}
	for _, b := range e.bs {
		b.Crash()
	}
}

func buckets(s []load.Sample, width int64, names []string) ([]Bucket, map[string][]int) {
	var end int64
	for _, x := range s {
		if x.AtMs > end {
			end = x.AtMs
		}
	}
	n := int(end/width) + 1
	out := make([]Bucket, n)
	by := map[string][]int{}
	for _, nm := range names {
		by[nm] = make([]int, n)
	}
	for i := range out {
		out[i].T = int64(i) * width
	}
	for _, x := range s {
		i := int(x.AtMs / width)
		if x.Status == 200 {
			out[i].OK++
			if a, ok := by[x.Backend]; ok {
				a[i]++
			}
		} else {
			out[i].Err++
		}
	}
	return out, by
}

func names(e *env) []string {
	var n []string
	for _, b := range e.bs {
		n = append(n, b.ID)
	}
	return n
}

func finish(r *Result, e *env, g *load.Generator, width int64) {
	s := g.Run.Snapshot()
	r.Timeline, r.ByBack = buckets(s, width, names(e))
	r.Metrics["requests"] = len(s)
	r.Metrics["errors"] = load.Count(s, 0, 1<<60, load.Failed)
}

func sleepMs(ms int64) { time.Sleep(time.Duration(ms) * time.Millisecond) }

// Distribution: three healthy backends, how evenly does traffic spread?
func Distribution(v Variant) (*Result, error) {
	e, err := setup(v, 3, proxy.Config{Balance: "roundrobin"})
	if err != nil {
		return nil, err
	}
	defer e.close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	g := load.Start(ctx, load.Options{URL: e.p.URL(), Rate: 150})
	g.Wait()
	s := g.Run.Snapshot()
	r := &Result{Scenario: "distribution", Variant: v.Name, Metrics: map[string]any{}}
	counts := load.ByBackend(s, 0, 1<<60)
	r.Metrics["perBackend"] = counts
	total, maxDev := 0, 0.0
	for _, c := range counts {
		total += c
	}
	for _, c := range counts {
		d := float64(c)/float64(total)*3 - 1
		if d < 0 {
			d = -d
		}
		if d > maxDev {
			maxDev = d
		}
	}
	r.Metrics["maxDeviationFromEven"] = maxDev
	finish(r, e, g, 250)
	return r, nil
}

// BackendCrash: one backend dies at 1.5 s and comes back at 4 s.
func BackendCrash(v Variant) (*Result, error) {
	e, err := setup(v, 3, proxy.Config{Balance: "roundrobin"})
	if err != nil {
		return nil, err
	}
	defer e.close()
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	g := load.Start(ctx, load.Options{URL: e.p.URL(), Rate: 100})
	sleepMs(1500)
	crashAt := g.Run.Now()
	e.bs[0].Crash()
	sleepMs(2500)
	restartAt := g.Run.Now()
	_ = e.bs[0].Restart()
	g.Wait()
	s := g.Run.Snapshot()
	r := &Result{Scenario: "backend-crash", Variant: v.Name, Metrics: map[string]any{}}
	failsDuring := load.Count(s, crashAt, restartAt, load.Failed)
	r.Metrics["crashAtMs"], r.Metrics["restartAtMs"] = crashAt, restartAt
	r.Metrics["clientErrorsAfterCrash"] = failsDuring
	lastB0 := int64(-1)
	for _, x := range s {
		if x.Backend == "b0" && x.AtMs >= crashAt && x.AtMs < restartAt {
			lastB0 = x.AtMs
		}
	}
	r.Metrics["lastResponseFromCrashedBackendMs"] = lastB0
	firstBack := int64(-1)
	for _, x := range s {
		if x.Backend == "b0" && x.AtMs >= restartAt && x.Status == 200 {
			firstBack = x.AtMs - restartAt
			break
		}
	}
	r.Metrics["msUntilRestartedBackendServesAgain"] = firstBack
	finish(r, e, g, 250)
	return r, nil
}

// BackendErrors: one backend starts returning 503 while its health endpoint
// still answers 200. Active health checks cannot see this.
func BackendErrors(v Variant) (*Result, error) {
	e, err := setup(v, 3, proxy.Config{Balance: "roundrobin"})
	if err != nil {
		return nil, err
	}
	defer e.close()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	g := load.Start(ctx, load.Options{URL: e.p.URL(), Rate: 100})
	sleepMs(1500)
	at := g.Run.Now()
	e.bs[0].SetBroken(true)
	g.Wait()
	s := g.Run.Snapshot()
	r := &Result{Scenario: "backend-errors", Variant: v.Name, Metrics: map[string]any{}}
	after := load.Count(s, at, 1<<60, func(load.Sample) bool { return true })
	fails := load.Count(s, at, 1<<60, load.Failed)
	r.Metrics["brokeAtMs"] = at
	r.Metrics["requestsAfter"] = after
	r.Metrics["errorsAfter"] = fails
	r.Metrics["errorRateAfter"] = float64(fails) / float64(after)
	r.Metrics["lastErrorMs"] = load.LastFailure(s)
	finish(r, e, g, 250)
	return r, nil
}

// HealthCheckDrain: a backend starts failing its health endpoint (what a
// deploy does before stopping it). How long until traffic stops reaching it?
func HealthCheckDrain(v Variant) (*Result, error) {
	e, err := setup(v, 3, proxy.Config{Balance: "roundrobin"})
	if err != nil {
		return nil, err
	}
	defer e.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g := load.Start(ctx, load.Options{URL: e.p.URL(), Rate: 100})
	sleepMs(1500)
	at := g.Run.Now()
	e.bs[0].SetHealthy(false)
	g.Wait()
	s := g.Run.Snapshot()
	r := &Result{Scenario: "health-check-drain", Variant: v.Name, Metrics: map[string]any{}}
	last := int64(-1)
	for _, x := range s {
		if x.Backend == "b0" && x.AtMs >= at {
			last = x.AtMs - at
		}
	}
	r.Metrics["msUntilTrafficStops"] = last
	r.Metrics["errors"] = load.Count(s, 0, 1<<60, load.Failed)
	finish(r, e, g, 250)
	return r, nil
}

// Overload: slow backends, a small per-backend concurrency cap, and far more
// concurrent clients than the cap allows.
func Overload(v Variant) (*Result, error) {
	e, err := setup(v, 3, proxy.Config{Balance: "leastconn", MaxConn: 2, QueueMs: 300})
	if err != nil {
		return nil, err
	}
	defer e.close()
	for _, b := range e.bs {
		b.SetDelay(150 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	g := load.Start(ctx, load.Options{URL: e.p.URL(), Rate: 300, Workers: 30})
	g.Wait()
	s := g.Run.Snapshot()
	r := &Result{Scenario: "overload", Variant: v.Name, Metrics: map[string]any{}}
	ok := load.Count(s, 0, 1<<60, load.OK)
	shed := load.Count(s, 0, 1<<60, func(x load.Sample) bool { return x.Status == 503 })
	r.Metrics["ok"], r.Metrics["shed503"] = ok, shed
	r.Metrics["okP50Ms"] = load.Percentile(s, 0.5, load.OK)
	r.Metrics["okP95Ms"] = load.Percentile(s, 0.95, load.OK)
	r.Metrics["shedP50Ms"] = load.Percentile(s, 0.5, func(x load.Sample) bool { return x.Status == 503 })
	r.Metrics["shedP95Ms"] = load.Percentile(s, 0.95, func(x load.Sample) bool { return x.Status == 503 })
	r.Metrics["otherFailures"] = load.Count(s, 0, 1<<60, func(x load.Sample) bool { return x.Status != 200 && x.Status != 503 })
	finish(r, e, g, 250)
	return r, nil
}

// RateLimit (HAProxy only): per-client request-rate limit via a stick table.
func RateLimit(v Variant) (*Result, error) {
	e, err := setup(v, 3, proxy.Config{Balance: "roundrobin", RateLimit: 40})
	if err != nil {
		return nil, err
	}
	defer e.close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	noisy := load.Start(ctx, load.Options{URL: e.p.URL(), Rate: 300, Workers: 6, Header: func(int) map[string]string { return map[string]string{"X-Client": "noisy"} }})
	quiet := load.Start(ctx, load.Options{URL: e.p.URL(), Rate: 10, Workers: 1, Header: func(int) map[string]string { return map[string]string{"X-Client": "quiet"} }})
	noisy.Wait()
	quiet.Wait()
	ns, qs := noisy.Run.Snapshot(), quiet.Run.Snapshot()
	r := &Result{Scenario: "rate-limit", Variant: v.Name, Metrics: map[string]any{}}
	r.Metrics["limitPerSecond"] = 40
	r.Metrics["noisyRequests"] = len(ns)
	r.Metrics["noisyOK"] = load.Count(ns, 0, 1<<60, load.OK)
	r.Metrics["noisy429"] = load.Count(ns, 0, 1<<60, func(x load.Sample) bool { return x.Status == 429 })
	r.Metrics["quietRequests"] = len(qs)
	r.Metrics["quietFailed"] = load.Count(qs, 0, 1<<60, load.Failed)
	return r, nil
}

// RollingDeploy replaces every backend one at a time under constant load,
// draining through the health check first.
func RollingDeploy(v Variant) (*Result, error) {
	e, err := setup(v, 3, proxy.Config{Balance: "roundrobin"})
	if err != nil {
		return nil, err
	}
	defer e.close()
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Second)
	defer cancel()
	g := load.Start(ctx, load.Options{URL: e.p.URL(), Rate: 100})
	sleepMs(1000)
	var marks []int64
	for _, b := range e.bs {
		b.SetHealthy(false) // 1. fail the health check so the proxy stops routing here
		sleepMs(1200)       // 2. wait out fall x interval plus in-flight work
		b.Shutdown()        // 3. graceful stop
		marks = append(marks, g.Run.Now())
		sleepMs(300)
		if err := b.Restart(); err != nil { // 4. new version on the same port, healthy again
			return nil, err
		}
		sleepMs(1200) // 5. wait for rise x interval before the next one
	}
	cancel()
	g.Wait()
	r := &Result{Scenario: "rolling-deploy", Variant: v.Name, Metrics: map[string]any{}}
	r.Metrics["replacedAtMs"] = marks
	finish(r, e, g, 250)
	return r, nil
}

// ConsistentHash: which backend serves each of 300 users, and how many move
// when one backend is removed?
func ConsistentHash(v Variant, balance string) (*Result, error) {
	e, err := setup(v, 3, proxy.Config{Balance: balance, HashHdr: "x-user"})
	if err != nil {
		return nil, err
	}
	defer e.close()
	which := func() map[int]string {
		m := map[int]string{}
		for u := 0; u < 300; u++ {
			g := loadOne(e.p.URL(), fmt.Sprintf("user-%d", u))
			m[u] = g
		}
		return m
	}
	before := which()
	again := which()
	stable := 0
	for u, b := range before {
		if again[u] == b {
			stable++
		}
	}
	e.bs[2].SetHealthy(false)
	sleepMs(1500)
	after := which()
	moved, offRemoved, onRemoved, onRemovedMoved := 0, 0, 0, 0
	for u, b := range before {
		if b == "b2" {
			onRemoved++
			if after[u] != b {
				onRemovedMoved++
			}
			continue
		}
		offRemoved++
		if after[u] != b {
			moved++
		}
	}
	r := &Result{Scenario: "consistent-hash", Variant: v.Name + "/" + balance, Metrics: map[string]any{}}
	r.Metrics["usersStableOnRepeat"] = stable
	r.Metrics["usersOnRemovedBackend"] = onRemoved
	r.Metrics["usersNotOnRemovedBackend"] = offRemoved
	r.Metrics["unrelatedUsersMoved"] = moved
	r.Metrics["unrelatedMovedFraction"] = float64(moved) / float64(offRemoved)
	r.Metrics["removedUsersRehomed"] = onRemovedMoved
	dist := map[string]int{}
	for _, b := range before {
		dist[b]++
	}
	r.Metrics["usersPerBackendBefore"] = dist
	return r, nil
}

func loadOne(url, user string) string {
	x := load.One(url, map[string]string{"X-User": user})
	if x.Status != 200 {
		return fmt.Sprintf("err%d", x.Status)
	}
	return x.Backend
}

func SortedKeys[V any](m map[string]V) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}
