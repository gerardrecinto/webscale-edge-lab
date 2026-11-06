package deploy

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Sim is a fleet of in-memory hosts. It exists so the controller's rules can be
// tested and demonstrated at 1000 hosts without 1000 processes, and it checks
// the rules independently: it counts the hosts actually in rotation at every
// step, so a controller bug shows up as a capacity dip even if the controller's
// own counters look fine.
type Sim struct {
	hosts []*SimHost
	in    atomic.Int64
	min   atomic.Int64
	step  time.Duration
}

type SimHost struct {
	fleet *Sim
	id    string

	mu      sync.Mutex
	version string
	state   string            // serving | draining | out | updating
	fail    map[string]string // "version/step" -> error text
}

func NewSim(n int, version string, step time.Duration) *Sim {
	s := &Sim{step: step}
	for i := 0; i < n; i++ {
		s.hosts = append(s.hosts, &SimHost{fleet: s, id: fmt.Sprintf("host-%04d", i), version: version, state: "serving", fail: map[string]string{}})
	}
	s.in.Store(int64(n))
	s.min.Store(int64(n))
	return s
}

func (s *Sim) Targets() []Target {
	out := make([]Target, len(s.hosts))
	for i, h := range s.hosts {
		out[i] = h
	}
	return out
}

// Host returns one host by ID, or nil.
func (s *Sim) Host(id string) *SimHost {
	for _, h := range s.hosts {
		if h.id == id {
			return h
		}
	}
	return nil
}

// MinInRotation is the fewest hosts that were ever serving at once.
func (s *Sim) MinInRotation() int { return int(s.min.Load()) }

// ResetMin starts a new measurement.
func (s *Sim) ResetMin() { s.min.Store(s.in.Load()) }

func (s *Sim) leave() {
	n := s.in.Add(-1)
	for {
		m := s.min.Load()
		if n >= m || s.min.CompareAndSwap(m, n) {
			return
		}
	}
}

func (s *Sim) Versions() map[string]int {
	m := map[string]int{}
	for _, h := range s.hosts {
		m[h.Version()]++
	}
	return m
}

type HostState struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	State   string `json:"state"`
}

func (s *Sim) States() []HostState {
	out := make([]HostState, len(s.hosts))
	for i, h := range s.hosts {
		h.mu.Lock()
		out[i] = HostState{h.id, h.version, h.state}
		h.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// FailAt makes a step fail for a build: step is drain, update, verify or enable.
func (h *SimHost) FailAt(version, step, msg string) {
	h.mu.Lock()
	h.fail[version+"/"+step] = msg
	h.mu.Unlock()
}

func (h *SimHost) failure(version, step string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m, ok := h.fail[version+"/"+step]; ok {
		return fmt.Errorf("%s", m)
	}
	return nil
}

func (h *SimHost) pause(ctx context.Context) error {
	if h.fleet.step <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(h.fleet.step):
		return nil
	}
}

func (h *SimHost) ID() string { return h.id }

func (h *SimHost) Version() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.version
}

func (h *SimHost) InRotation() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state == "serving"
}

func (h *SimHost) Drain(ctx context.Context) error {
	h.mu.Lock()
	if h.state == "serving" {
		h.state = "draining"
		h.mu.Unlock()
		h.fleet.leave()
	} else {
		h.mu.Unlock()
	}
	if err := h.pause(ctx); err != nil {
		return err
	}
	if err := h.failure(h.Version(), "drain"); err != nil {
		return err
	}
	h.mu.Lock()
	h.state = "out"
	h.mu.Unlock()
	return nil
}

func (h *SimHost) Update(ctx context.Context, version string) error {
	h.mu.Lock()
	h.state = "updating"
	h.mu.Unlock()
	if err := h.pause(ctx); err != nil {
		return err
	}
	if err := h.failure(version, "update"); err != nil {
		return err
	}
	h.mu.Lock()
	h.version = version
	h.state = "out"
	h.mu.Unlock()
	return nil
}

func (h *SimHost) Verify(ctx context.Context, version string) error {
	if err := h.pause(ctx); err != nil {
		return err
	}
	if err := h.failure(version, "verify"); err != nil {
		return err
	}
	if v := h.Version(); v != version {
		return fmt.Errorf("running %s, want %s", v, version)
	}
	return nil
}

func (h *SimHost) Enable(ctx context.Context) error {
	if err := h.pause(ctx); err != nil {
		return err
	}
	if err := h.failure(h.Version(), "enable"); err != nil {
		return err
	}
	h.mu.Lock()
	was := h.state
	h.state = "serving"
	h.mu.Unlock()
	if was != "serving" {
		h.fleet.in.Add(1)
	}
	return nil
}
