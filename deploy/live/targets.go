// Package live connects the deploy engine to real load balancers and real
// application instances: HAProxy through its runtime API, Nginx and Apache
// through a graceful reload.
package live

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gerardrecinto/webscale-edge-lab/backend"
	"github.com/gerardrecinto/webscale-edge-lab/deploy"
	"github.com/gerardrecinto/webscale-edge-lab/load"
	"github.com/gerardrecinto/webscale-edge-lab/proxy"
)

// App is one application instance the controller can stop and start.
type App interface {
	Addr() string
	Stop(ctx context.Context) error
	Start(ctx context.Context, version string) error
}

// Router is the load balancer's side of taking an instance in and out.
type Router interface {
	// Remove stops new requests to the instance and returns once the balancer
	// itself no longer has requests running on it.
	Remove(ctx context.Context, name string) error
	// Restore routes requests to the instance again and returns once the
	// balancer treats it as in service.
	Restore(ctx context.Context, name string) error
	Routed(name string) bool
}

var httpc = &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

func get(ctx context.Context, url string) (int, http.Header, string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := httpc.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, resp.Header, string(b), nil
}

// AppTarget is one instance behind one router.
type AppTarget struct {
	Name          string
	App           App
	Router        Router
	VerifyTimeout time.Duration // how long a new build may take to become ready, default 15s
	version       string
	mu            sync.Mutex
}

func NewAppTarget(name string, app App, r Router, version string) *AppTarget {
	return &AppTarget{Name: name, App: app, Router: r, version: version}
}

func (t *AppTarget) ID() string { return t.Name }
func (t *AppTarget) Version() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.version
}
func (t *AppTarget) InRotation() bool { return t.Router.Routed(t.Name) }

func (t *AppTarget) Drain(ctx context.Context) error {
	if err := t.Router.Remove(ctx, t.Name); err != nil {
		return err
	}
	// The balancer says it is done. The instance's own gauge is the second
	// opinion: it also covers requests the balancer has already handed over.
	for {
		_, _, body, err := get(ctx, "http://"+t.App.Addr()+"/debug/inflight")
		if err != nil {
			return nil // nothing is listening, so nothing is in flight
		}
		if n, _ := strconv.Atoi(strings.TrimSpace(body)); n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("instance still has requests in flight: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (t *AppTarget) Update(ctx context.Context, version string) error {
	if err := t.App.Stop(ctx); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	if err := t.App.Start(ctx, version); err != nil {
		return fmt.Errorf("start %s: %w", version, err)
	}
	t.mu.Lock()
	t.version = version
	t.mu.Unlock()
	return nil
}

// Verify talks to the instance directly. It must be up, answer, and say it is
// the build we asked for.
func (t *AppTarget) Verify(ctx context.Context, version string) error {
	limit := t.VerifyTimeout
	if limit == 0 {
		limit = 15 * time.Second
	}
	deadline := time.Now().Add(limit)
	var last error
	for time.Now().Before(deadline) {
		code, h, _, err := get(ctx, "http://"+t.App.Addr()+"/healthz")
		switch {
		case err != nil:
			last = err
		case code != 200:
			last = fmt.Errorf("/healthz returned %d", code)
		default:
			code, h, _, err = get(ctx, "http://"+t.App.Addr()+"/")
			if err != nil {
				last = err
			} else if code != 200 {
				last = fmt.Errorf("a real request returned %d", code)
			} else if got := h.Get("X-Version"); got != version {
				return fmt.Errorf("instance reports build %q, want %q", got, version)
			} else {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%v: %w", last, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("not ready in time: %v", last)
}

func (t *AppTarget) Enable(ctx context.Context) error { return t.Router.Restore(ctx, t.Name) }

// ---- Go backend as an App ----

// GoApp runs the lab's Go backend. Quirks lets a build behave badly on purpose,
// for example "v3" -> a bug that fails every third request.
type GoApp struct {
	B      *backend.Backend
	Quirks map[string]func(*backend.Backend)
	// StartDelay is how long a new build takes to start listening. Real services
	// need seconds; with no delay a swap is over in a millisecond and a test
	// proves little.
	StartDelay time.Duration
}

func (a *GoApp) Addr() string { return a.B.Addr() }
func (a *GoApp) Stop(ctx context.Context) error {
	a.B.Shutdown()
	return nil
}
func (a *GoApp) Start(ctx context.Context, version string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(a.StartDelay):
	}
	a.B.SetVersion(version)
	if err := a.B.Restart(); err != nil {
		return err
	}
	if q := a.Quirks[version]; q != nil {
		q(a.B)
	}
	return nil
}

// ---- HAProxy runtime API ----

type HAProxyRouter struct {
	P       *proxy.Proxy
	Backend string // HAProxy backend section name
}

func NewHAProxyRouter(p *proxy.Proxy) *HAProxyRouter { return &HAProxyRouter{P: p, Backend: "be"} }

type serverStat struct {
	Status string
	Scur   int
}

// stats parses `show stat` by column name, so a different HAProxy version that
// adds columns does not break it.
func (r *HAProxyRouter) stats() (map[string]serverStat, error) {
	out, err := r.P.HAProxyCmd("show stat")
	if err != nil {
		return nil, err
	}
	out = strings.TrimPrefix(strings.TrimSpace(out), "# ")
	rd := csv.NewReader(strings.NewReader(out))
	rd.FieldsPerRecord = -1
	rows, err := rd.ReadAll()
	if err != nil || len(rows) < 2 {
		return nil, fmt.Errorf("could not parse show stat: %v", err)
	}
	col := map[string]int{}
	for i, n := range rows[0] {
		col[n] = i
	}
	m := map[string]serverStat{}
	for _, row := range rows[1:] {
		if len(row) <= col["status"] || row[col["pxname"]] != r.Backend || row[col["svname"]] == "BACKEND" || row[col["svname"]] == "FRONTEND" {
			continue
		}
		n, _ := strconv.Atoi(row[col["scur"]])
		m[row[col["svname"]]] = serverStat{Status: row[col["status"]], Scur: n}
	}
	return m, nil
}

func (r *HAProxyRouter) cmd(c string) error {
	out, err := r.P.HAProxyCmd(c)
	if err != nil {
		return err
	}
	if s := strings.TrimSpace(out); s != "" {
		return fmt.Errorf("haproxy: %s", s)
	}
	return nil
}

func (r *HAProxyRouter) Remove(ctx context.Context, name string) error {
	if err := r.cmd(fmt.Sprintf("set server %s/%s state drain", r.Backend, name)); err != nil {
		return err
	}
	for {
		st, err := r.stats()
		if err != nil {
			return err
		}
		if s, ok := st[name]; ok && s.Scur == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("haproxy still has sessions on %s: %w", name, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (r *HAProxyRouter) Restore(ctx context.Context, name string) error {
	if err := r.cmd(fmt.Sprintf("set server %s/%s state ready", r.Backend, name)); err != nil {
		return err
	}
	for {
		st, err := r.stats()
		if err != nil {
			return err
		}
		if s, ok := st[name]; ok && strings.HasPrefix(s.Status, "UP") {
			return nil
		}
		select {
		case <-ctx.Done():
			s := st[name]
			return fmt.Errorf("haproxy reports %s as %q: %w", name, s.Status, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (r *HAProxyRouter) Routed(name string) bool {
	st, err := r.stats()
	if err != nil {
		return false
	}
	return strings.HasPrefix(st[name].Status, "UP")
}

// ---- Nginx and Apache: graceful reload ----

// ReloadRouter marks instances down in the proxy's config and reloads it. Many
// instances in one batch change the config together, so reloads are coalesced:
// a caller whose change was already applied by someone else's reload does not
// reload again.
type ReloadRouter struct {
	P *proxy.Proxy

	mu      sync.Mutex
	down    map[string]bool
	gen     int
	applied int
	Reloads int // how many reloads were actually done
}

func NewReloadRouter(p *proxy.Proxy) *ReloadRouter {
	return &ReloadRouter{P: p, down: map[string]bool{}}
}

func (r *ReloadRouter) change(name string, down bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if down {
		r.down[name] = true
	} else {
		delete(r.down, name)
	}
	r.gen++
	mine := r.gen
	if r.applied >= mine {
		return nil
	}
	c := r.P.Config()
	c.Down = map[string]bool{}
	for k := range r.down {
		c.Down[k] = true
	}
	if err := r.P.Reload(c); err != nil {
		return err
	}
	r.applied = r.gen
	r.Reloads++
	return nil
}

func (r *ReloadRouter) Remove(ctx context.Context, name string) error  { return r.change(name, true) }
func (r *ReloadRouter) Restore(ctx context.Context, name string) error { return r.change(name, false) }
func (r *ReloadRouter) Routed(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.down[name]
}

// ---- Canary observation from live client traffic ----

// Observe compares what clients saw from the new build with what they saw from
// the old one, using the X-Version header on every response.
func Observe(run *load.Run, newV, oldV string) deploy.Observer {
	return func(ctx context.Context, since time.Time) (deploy.Window, deploy.Window, error) {
		from := since.Sub(run.Start).Milliseconds()
		var cw, bw deploy.Window
		var cl, bl []int64
		for _, s := range run.Snapshot() {
			if s.AtMs < from {
				continue
			}
			switch s.Version {
			case newV:
				cw.Requests++
				if s.Status != 200 {
					cw.Errors++
				}
				cl = append(cl, s.LatencyMs)
			case oldV:
				bw.Requests++
				if s.Status != 200 {
					bw.Errors++
				}
				bl = append(bl, s.LatencyMs)
			}
		}
		cw.P95, bw.P95 = p95(cl), p95(bl)
		return cw, bw, nil
	}
}

func p95(l []int64) time.Duration {
	if len(l) == 0 {
		return 0
	}
	sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
	return time.Duration(l[int(0.95*float64(len(l)-1))]) * time.Millisecond
}
