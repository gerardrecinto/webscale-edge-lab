package failover

import (
	"errors"
	"sync"
	"time"
)

// ErrOpen is returned without calling the dependency while the breaker is open.
var ErrOpen = errors.New("circuit open")

type breakerState int

const (
	closed breakerState = iota
	open
	halfOpen
)

// Breaker opens after Threshold consecutive failures, rejects calls for
// Cooldown, then lets one probe call through (half open).
type Breaker struct {
	Threshold int
	Cooldown  time.Duration

	mu       sync.Mutex
	state    breakerState
	fails    int
	openedAt time.Time
	probing  bool
	now      func() time.Time
}

func NewBreaker(threshold int, cooldown time.Duration) *Breaker {
	return &Breaker{Threshold: threshold, Cooldown: cooldown, now: time.Now}
}

// Do runs fn unless the breaker is open.
func (b *Breaker) Do(fn func() error) error {
	b.mu.Lock()
	switch b.state {
	case open:
		if b.now().Sub(b.openedAt) < b.Cooldown {
			b.mu.Unlock()
			return ErrOpen
		}
		b.state, b.probing = halfOpen, true
	case halfOpen:
		if b.probing {
			b.mu.Unlock()
			return ErrOpen
		}
		b.probing = true
	}
	b.mu.Unlock()

	err := fn()

	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
	if err == nil {
		b.state, b.fails = closed, 0
		return nil
	}
	b.fails++
	if b.state == halfOpen || b.fails >= b.Threshold {
		b.state, b.openedAt = open, b.now()
	}
	return err
}

// Open reports whether calls are currently being rejected.
func (b *Breaker) Open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state == open
}
