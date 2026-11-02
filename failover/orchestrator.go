package failover

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// FenceStore hands out strictly increasing epochs. In a real design this is a
// compare-and-set in a store outside both regions (a third region). Here it
// is a mutex, so the lab shows the protocol, not the storage.
type FenceStore struct {
	mu    sync.Mutex
	epoch uint64
}

func NewFenceStore(start uint64) *FenceStore { return &FenceStore{epoch: start} }

func (f *FenceStore) Next() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.epoch++
	return f.epoch
}

// Route is where clients should send writes.
type Route struct {
	URL   string
	Epoch uint64
	Name  string
}

// Router holds the current route; the orchestrator swaps it on failover.
type Router struct{ p atomic.Pointer[Route] }

func (r *Router) Set(x Route) { r.p.Store(&x) }
func (r *Router) Get() Route  { return *r.p.Load() }

// Config tunes detection, fencing and the RPO gate.
type Config struct {
	Interval      time.Duration // probe and lease-renew period
	Timeout       time.Duration // per request
	FailThreshold int           // consecutive failures before a prober votes
	Vantages      int           // number of independent probers
	Quorum        int           // probers that must agree
	LeaseTTL      time.Duration // old primary stops writing this long after its last renewal
	MaxLagOps     uint64        // RPO budget in writes; failover is refused beyond it
	Force         bool          // override the gate
}

// Event is a timestamped step, used by the drill to report the timeline.
type Event struct {
	At   time.Time
	What string
}

// Orchestrator watches the primary and runs the failover when a quorum of
// probers agree it is down and the replica is within the lag budget.
type Orchestrator struct {
	cfg   Config
	store *FenceStore
	rt    *Router

	mu        sync.Mutex
	primary   *Region
	secondary *Region
	lastRenew time.Time
	lastSeq   uint64
	fails     []int
	blocked   []bool
	events    []Event
	done      bool
	gated     bool
}

func NewOrchestrator(cfg Config, primary, secondary *Region, store *FenceStore, rt *Router) *Orchestrator {
	o := &Orchestrator{cfg: cfg, primary: primary, secondary: secondary, store: store, rt: rt,
		fails: make([]int, cfg.Vantages), blocked: make([]bool, cfg.Vantages), lastRenew: time.Now()}
	rt.Set(Route{URL: primary.URL(), Epoch: 1, Name: primary.Name})
	return o
}

func (o *Orchestrator) log(format string, a ...any) {
	o.mu.Lock()
	o.events = append(o.events, Event{time.Now(), fmt.Sprintf(format, a...)})
	o.mu.Unlock()
}

func (o *Orchestrator) Events() []Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Event(nil), o.events...)
}

// BlockVantage makes prober i unable to reach the primary (a partial partition
// of the monitoring path only).
func (o *Orchestrator) BlockVantage(i int, b bool) {
	o.mu.Lock()
	o.blocked[i] = b
	o.mu.Unlock()
}

// Done reports whether a failover has completed.
func (o *Orchestrator) Done() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.done
}

// Gated reports whether a failover was refused by the lag gate.
func (o *Orchestrator) Gated() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.gated
}

func (o *Orchestrator) call(c *http.Client, from, url string) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	req.Header.Set("X-From", from)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp, errors.New(resp.Status)
	}
	return resp, nil
}

// Run probes until ctx ends. After a failover it keeps renewing the new
// primary's lease.
func (o *Orchestrator) Run(ctx context.Context) {
	t := time.NewTicker(o.cfg.Interval)
	defer t.Stop()
	cl := &http.Client{Timeout: o.cfg.Timeout}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		o.mu.Lock()
		p, done := o.primary, o.done
		o.mu.Unlock()
		epoch := o.rt.Get().Epoch
		// Renew in its own goroutine: a hung renewal must not delay detection.
		go func() {
			if _, err := o.call(cl, "orchestrator", fmt.Sprintf("%s/lease?ttl=%s&epoch=%d", p.URL(), o.cfg.LeaseTTL, epoch)); err == nil {
				o.mu.Lock()
				o.lastRenew = time.Now()
				o.mu.Unlock()
			}
		}()
		if done {
			continue
		}
		if o.probe(cl, p) {
			o.failover(ctx)
		}
	}
}

// probe runs every vantage in parallel and reports whether a quorum has
// seen FailThreshold failures in a row.
func (o *Orchestrator) probe(cl *http.Client, p *Region) bool {
	var wg sync.WaitGroup
	for i := 0; i < o.cfg.Vantages; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o.mu.Lock()
			blocked := o.blocked[i]
			o.mu.Unlock()
			ok := false
			if !blocked {
				req, _ := http.NewRequest(http.MethodGet, p.URL()+"/health", nil)
				req.Header.Set("X-From", "probe-"+strconv.Itoa(i))
				if resp, err := cl.Do(req); err == nil {
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						ok = true
						if s, err := strconv.ParseUint(resp.Header.Get("X-Seq"), 10, 64); err == nil {
							o.mu.Lock()
							if s > o.lastSeq {
								o.lastSeq = s
							}
							o.mu.Unlock()
						}
					}
				}
			}
			o.mu.Lock()
			if ok {
				o.fails[i] = 0
			} else {
				o.fails[i]++
			}
			o.mu.Unlock()
		}(i)
	}
	wg.Wait()
	o.mu.Lock()
	defer o.mu.Unlock()
	votes := 0
	for _, f := range o.fails {
		if f >= o.cfg.FailThreshold {
			votes++
		}
	}
	return votes >= o.cfg.Quorum
}

// Lag is the number of writes the replica is known to be missing. When the
// primary is unreachable it uses the highest sequence a prober ever saw, so
// the figure is a lower bound on what was acknowledged just before the fault.
func (o *Orchestrator) Lag() uint64 {
	o.mu.Lock()
	last := o.lastSeq
	sec := o.secondary
	o.mu.Unlock()
	have := sec.State().Seq
	if last <= have {
		return 0
	}
	return last - have
}

func (o *Orchestrator) failover(ctx context.Context) {
	o.mu.Lock()
	seen := o.gated
	o.mu.Unlock()
	if !seen {
		o.log("quorum says primary down")
	}
	o.mu.Lock()
	force := o.cfg.Force
	o.mu.Unlock()
	if lag := o.Lag(); lag > o.cfg.MaxLagOps && !force {
		o.mu.Lock()
		first := !o.gated
		o.gated = true
		o.mu.Unlock()
		if first {
			o.log("failover refused: replica %d writes behind, budget %d", lag, o.cfg.MaxLagOps)
		}
		return
	}
	o.mu.Lock()
	old, nu, lastRenew := o.primary, o.secondary, o.lastRenew
	o.mu.Unlock()

	epoch := o.store.Next()
	o.log("epoch %d issued", epoch)

	cl := &http.Client{Timeout: o.cfg.Timeout}
	if _, err := o.call(cl, "orchestrator", fmt.Sprintf("%s/fence?epoch=%d", old.URL(), epoch)); err == nil {
		o.log("old primary fenced directly")
	} else {
		wait := time.Until(lastRenew.Add(o.cfg.LeaseTTL + 20*time.Millisecond))
		if wait > 0 {
			o.log("fence unreachable, waiting %s for the lease to lapse", wait.Round(time.Millisecond))
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
		} else {
			o.log("fence unreachable, lease already lapsed")
		}
	}
	if _, err := o.call(cl, "orchestrator", fmt.Sprintf("%s/promote?epoch=%d&ttl=%s", nu.URL(), epoch, o.cfg.LeaseTTL)); err != nil {
		o.log("promote failed: %v", err)
		return
	}
	o.rt.Set(Route{URL: nu.URL(), Epoch: epoch, Name: nu.Name})
	o.mu.Lock()
	o.primary, o.secondary, o.done, o.lastRenew = nu, old, true, time.Now()
	o.mu.Unlock()
	o.log("promoted %s at epoch %d", nu.Name, epoch)
}

// Client sends writes to the current route behind a circuit breaker.
type Client struct {
	rt *Router
	br *Breaker
	cl *http.Client
	ID string
}

func NewClient(rt *Router, br *Breaker, timeout time.Duration) *Client {
	return &Client{rt: rt, br: br, cl: &http.Client{Timeout: timeout}, ID: "client"}
}

// Write returns the name of the region that accepted the write.
func (c *Client) Write(k, v string) (string, error) {
	var region string
	err := c.br.Do(func() error {
		rt := c.rt.Get()
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/write?k=%s&v=%s", rt.URL, k, v), nil)
		req.Header.Set("X-Epoch", strconv.FormatUint(rt.Epoch, 10))
		req.Header.Set("X-From", c.ID)
		resp, err := c.cl.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return errors.New(resp.Status)
		}
		region = rt.Name
		return nil
	})
	return region, err
}

// WriteTo bypasses the router, for stale-client tests.
func WriteTo(url string, epoch uint64, k, v string) (int, error) {
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/write?k=%s&v=%s", url, k, v), nil)
	req.Header.Set("X-Epoch", strconv.FormatUint(epoch, 10))
	req.Header.Set("X-From", "stale-client")
	resp, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// SetForce lets the operator override the lag gate, like a break-glass flag.
func (o *Orchestrator) SetForce(b bool) {
	o.mu.Lock()
	o.cfg.Force = b
	o.mu.Unlock()
}
