// Package backend is a small HTTP service whose health and behaviour can be
// changed while it runs, so proxy scenarios can crash it, make it return
// errors, slow it down, fail its health check, or replace it.
package backend

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type Backend struct {
	ID   string
	Port int

	mu       sync.Mutex
	srv      *http.Server
	healthy  atomic.Bool // /healthz answer
	broken   atomic.Bool // every normal request returns 503
	delayMs  atomic.Int64
	Served   atomic.Int64
	inflight atomic.Int64
	version  atomic.Value // string: the build this instance is running
	flakyN   atomic.Int64 // when > 0, every Nth request returns 503 (a subtle bug a health check misses)
	reqs     atomic.Int64
}

func New(id string) (*Backend, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := &Backend{ID: id, Port: ln.Addr().(*net.TCPAddr).Port}
	b.healthy.Store(true)
	b.version.Store("v1")
	b.serve(ln)
	return b, nil
}

func (b *Backend) Addr() string { return "127.0.0.1:" + strconv.Itoa(b.Port) }

func (b *Backend) serve(ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !b.healthy.Load() {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "ok")
	})
	// The in-flight gauge is what a deploy controller polls to know a drained
	// instance has finished its work and is safe to stop.
	mux.HandleFunc("/debug/inflight", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, b.inflight.Load())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b.inflight.Add(1)
		defer b.inflight.Add(-1)
		if d := b.delayMs.Load(); d > 0 {
			time.Sleep(time.Duration(d) * time.Millisecond)
		}
		SecurityHeaders(w.Header())
		w.Header().Set("X-Backend", b.ID)
		w.Header().Set("X-Version", b.Version())
		if b.broken.Load() {
			http.Error(w, "broken", http.StatusServiceUnavailable)
			return
		}
		if n := b.flakyN.Load(); n > 0 && b.reqs.Add(1)%n == 0 {
			http.Error(w, "flaky", http.StatusServiceUnavailable)
			return
		}
		b.Served.Add(1)
		fmt.Fprint(w, b.ID)
	})
	srv := &http.Server{Handler: mux}
	b.mu.Lock()
	b.srv = srv
	b.mu.Unlock()
	go srv.Serve(ln)
}

// Crash closes the listener and every open connection at once.
func (b *Backend) Crash() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.srv != nil {
		_ = b.srv.Close()
		b.srv = nil
	}
}

// Restart brings a crashed backend back on the same port.
func (b *Backend) Restart() error {
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ {
		if ln, err = net.Listen("tcp", b.Addr()); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	b.broken.Store(false)
	b.healthy.Store(true)
	b.SetFlaky(0)
	b.serve(ln)
	return nil
}

// Shutdown stops accepting and waits for in-flight requests: a graceful stop.
func (b *Backend) Shutdown() {
	b.mu.Lock()
	srv := b.srv
	b.srv = nil
	b.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

// SetFlaky makes every nth request fail with 503 while /healthz stays 200,
// which is how a build with a real bug looks to an active health check. 0 turns it off.
func (b *Backend) SetFlaky(n int) { b.reqs.Store(0); b.flakyN.Store(int64(n)) }

func (b *Backend) SetHealthy(v bool)        { b.healthy.Store(v) }
func (b *Backend) SetBroken(v bool)         { b.broken.Store(v) }
func (b *Backend) SetDelay(d time.Duration) { b.delayMs.Store(d.Milliseconds()) }
func (b *Backend) Inflight() int64          { return b.inflight.Load() }

// Version is the build this instance reports in X-Version.
func (b *Backend) Version() string { return b.version.Load().(string) }

// SetVersion changes the reported build. A deploy calls it between Shutdown and Restart.
func (b *Backend) SetVersion(v string) { b.version.Store(v) }

// SecurityHeaders sets the response headers the OWASP Secure Headers Project
// recommends for an application that is not meant to be framed or sniffed.
func SecurityHeaders(h http.Header) {
	h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "geolocation=(), camera=(), microphone=()")
	h.Set("Cache-Control", "no-store")
}
