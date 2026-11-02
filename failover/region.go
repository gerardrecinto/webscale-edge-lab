// Package failover is a lab model of active-passive regional failover:
// two regions, async replication, a fencing epoch, a lease, a replication
// lag gate, a quorum of probers and a circuit breaker on the client.
// Everything runs on loopback in one process. It is not a production system.
package failover

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Entry is one replicated write.
type Entry struct {
	Seq uint64    `json:"seq"`
	Key string    `json:"key"`
	Val string    `json:"val"`
	At  time.Time `json:"at"`
}

// State is what /state returns.
type State struct {
	Seq      uint64 `json:"seq"`
	Epoch    uint64 `json:"epoch"`
	Writable bool   `json:"writable"`
}

// Chaos is the fault set a region applies to incoming requests.
type Chaos struct {
	Delay     time.Duration // added to every request
	ErrorRate float64       // fraction answered 503
	DropFrom  string        // callers (X-From) whose requests hang until timeout
}

// Region is one HTTP service holding an ordered log of writes.
type Region struct {
	Name string

	mu       sync.Mutex
	log      []Entry
	data     map[string]string
	epoch    uint64
	primary  bool
	leaseEnd time.Time

	chaos atomic.Pointer[Chaos]
	rnd   atomic.Uint64
	srv   *http.Server
	ln    net.Listener
	dead  atomic.Bool
}

// NewRegion starts a region on a loopback port. Primary regions start with
// epoch 1 and a lease that the orchestrator must keep renewing.
func NewRegion(name string, primary bool, lease time.Duration) (*Region, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &Region{Name: name, data: map[string]string{}, primary: primary, ln: ln}
	if primary {
		r.epoch = 1
		r.leaseEnd = time.Now().Add(lease)
	}
	r.chaos.Store(&Chaos{})
	r.rnd.Store(uint64(len(name))*2654435761 + 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/write", r.wrap(r.handleWrite))
	mux.HandleFunc("/health", r.wrap(r.handleHealth))
	mux.HandleFunc("/state", r.wrap(r.handleState))
	mux.HandleFunc("/log", r.wrap(r.handleLog))
	mux.HandleFunc("/lease", r.wrap(r.handleLease))
	mux.HandleFunc("/fence", r.wrap(r.handleFence))
	mux.HandleFunc("/promote", r.wrap(r.handlePromote))
	r.srv = &http.Server{Handler: mux}
	go r.srv.Serve(ln)
	return r, nil
}

func (r *Region) URL() string { return "http://" + r.ln.Addr().String() }

// Kill closes the listener and every open connection, like a crashed region.
func (r *Region) Kill() {
	r.dead.Store(true)
	r.srv.Close()
}

func (r *Region) SetChaos(c Chaos) { r.chaos.Store(&c) }

func (r *Region) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return State{Seq: uint64(len(r.log)), Epoch: r.epoch, Writable: r.writableLocked()}
}

func (r *Region) Has(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.data[key]
	return ok
}

func (r *Region) writableLocked() bool {
	return r.primary && time.Now().Before(r.leaseEnd)
}

// Apply appends a replicated entry. Entries must arrive in order.
func (r *Region) Apply(e Entry) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e.Seq != uint64(len(r.log))+1 {
		return false
	}
	r.log = append(r.log, e)
	r.data[e.Key] = e.Val
	return true
}

// next is a tiny xorshift so chaos error injection is repeatable per region.
func (r *Region) next() float64 {
	for {
		old := r.rnd.Load()
		x := old
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		if r.rnd.CompareAndSwap(old, x) {
			return float64(x%10000) / 10000
		}
	}
}

func (r *Region) wrap(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		c := r.chaos.Load()
		if c.DropFrom != "" && req.Header.Get("X-From") == c.DropFrom {
			select {
			case <-req.Context().Done():
			case <-time.After(30 * time.Second):
			}
			return
		}
		if c.Delay > 0 {
			time.Sleep(c.Delay)
		}
		if c.ErrorRate > 0 && r.next() < c.ErrorRate {
			http.Error(w, "injected", http.StatusServiceUnavailable)
			return
		}
		h(w, req)
	}
}

func (r *Region) handleWrite(w http.ResponseWriter, req *http.Request) {
	epoch, _ := strconv.ParseUint(req.Header.Get("X-Epoch"), 10, 64)
	k, v := req.URL.Query().Get("k"), req.URL.Query().Get("v")
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case !r.primary:
		http.Error(w, "not primary", http.StatusConflict)
	case epoch < r.epoch:
		http.Error(w, "fenced: stale epoch", http.StatusConflict)
	case !time.Now().Before(r.leaseEnd):
		http.Error(w, "lease expired", http.StatusConflict)
	default:
		e := Entry{Seq: uint64(len(r.log)) + 1, Key: k, Val: v, At: time.Now()}
		r.log = append(r.log, e)
		r.data[k] = v
		fmt.Fprintf(w, "%d", e.Seq)
	}
}

func (r *Region) handleHealth(w http.ResponseWriter, req *http.Request) {
	st := r.State()
	w.Header().Set("X-Seq", strconv.FormatUint(st.Seq, 10))
	w.WriteHeader(http.StatusOK)
}

func (r *Region) handleState(w http.ResponseWriter, req *http.Request) {
	json.NewEncoder(w).Encode(r.State())
}

// handleLog serves entries after ?from=N that are at least ?older= old,
// which is how replication lag is injected without sleeping in the replicator.
func (r *Region) handleLog(w http.ResponseWriter, req *http.Request) {
	from, _ := strconv.ParseUint(req.URL.Query().Get("from"), 10, 64)
	older, _ := time.ParseDuration(req.URL.Query().Get("older"))
	cut := time.Now().Add(-older)
	r.mu.Lock()
	var out []Entry
	for _, e := range r.log {
		if e.Seq >= from && e.At.Before(cut) {
			out = append(out, e)
		}
	}
	r.mu.Unlock()
	json.NewEncoder(w).Encode(out)
}

func (r *Region) handleLease(w http.ResponseWriter, req *http.Request) {
	ttl, _ := time.ParseDuration(req.URL.Query().Get("ttl"))
	epoch, _ := strconv.ParseUint(req.URL.Query().Get("epoch"), 10, 64)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.primary || epoch < r.epoch {
		http.Error(w, "not primary at this epoch", http.StatusConflict)
		return
	}
	r.leaseEnd = time.Now().Add(ttl)
}

func (r *Region) handleFence(w http.ResponseWriter, req *http.Request) {
	epoch, _ := strconv.ParseUint(req.URL.Query().Get("epoch"), 10, 64)
	r.mu.Lock()
	defer r.mu.Unlock()
	if epoch > r.epoch {
		r.epoch = epoch
	}
	r.primary = false
}

func (r *Region) handlePromote(w http.ResponseWriter, req *http.Request) {
	epoch, _ := strconv.ParseUint(req.URL.Query().Get("epoch"), 10, 64)
	ttl, _ := time.ParseDuration(req.URL.Query().Get("ttl"))
	r.mu.Lock()
	defer r.mu.Unlock()
	if epoch <= r.epoch {
		http.Error(w, "epoch not newer", http.StatusConflict)
		return
	}
	r.epoch, r.primary, r.leaseEnd = epoch, true, time.Now().Add(ttl)
}
