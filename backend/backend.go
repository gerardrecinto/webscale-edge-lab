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
}

func New(id string) (*Backend, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := &Backend{ID: id, Port: ln.Addr().(*net.TCPAddr).Port}
	b.healthy.Store(true)
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
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b.inflight.Add(1)
		defer b.inflight.Add(-1)
		if d := b.delayMs.Load(); d > 0 {
			time.Sleep(time.Duration(d) * time.Millisecond)
		}
		w.Header().Set("X-Backend", b.ID)
		if b.broken.Load() {
			http.Error(w, "broken", http.StatusServiceUnavailable)
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

func (b *Backend) SetHealthy(v bool)        { b.healthy.Store(v) }
func (b *Backend) SetBroken(v bool)         { b.broken.Store(v) }
func (b *Backend) SetDelay(d time.Duration) { b.delayMs.Store(d.Milliseconds()) }
func (b *Backend) Inflight() int64          { return b.inflight.Load() }
