// Package splunk ships deploy events to a Splunk HTTP Event Collector so a
// rollout can be searched and alerted on next to the application's own logs.
package splunk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

type Sender struct {
	URL        string // e.g. https://splunk.example.com:8088/services/collector/event
	Token      string
	Host       string
	Source     string // default "pushbutton"
	SourceType string // default "pushbutton:deploy"
	Index      string // optional
	Client     *http.Client
	Retries    int           // attempts after the first on a 5xx or network error, default 2
	Backoff    time.Duration // default 200ms, doubled each attempt
}

type envelope struct {
	Time       float64 `json:"time"`
	Host       string  `json:"host,omitempty"`
	Source     string  `json:"source"`
	SourceType string  `json:"sourcetype"`
	Index      string  `json:"index,omitempty"`
	Event      any     `json:"event"`
}

type hecReply struct {
	Text string `json:"text"`
	Code int    `json:"code"`
}

// PermanentError is a reply that retrying cannot fix, such as a bad token.
type PermanentError struct {
	Status int
	Text   string
}

func (e *PermanentError) Error() string {
	return fmt.Sprintf("splunk rejected the events: HTTP %d %s", e.Status, e.Text)
}

// Send posts events in one request. HEC accepts several JSON objects back to
// back in one body, which is cheaper than one request per event.
func (s *Sender) Send(ctx context.Context, at []time.Time, events []any) error {
	if len(events) == 0 {
		return nil
	}
	if s.Source == "" {
		s.Source = "pushbutton"
	}
	if s.SourceType == "" {
		s.SourceType = "pushbutton:deploy"
	}
	if s.Client == nil {
		s.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if s.Retries == 0 {
		s.Retries = 2
	}
	if s.Backoff == 0 {
		s.Backoff = 200 * time.Millisecond
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for i, ev := range events {
		t := time.Now()
		if i < len(at) {
			t = at[i]
		}
		if err := enc.Encode(envelope{Time: float64(t.UnixNano()) / 1e9, Host: s.Host, Source: s.Source, SourceType: s.SourceType, Index: s.Index, Event: ev}); err != nil {
			return err
		}
	}
	var last error
	wait := s.Backoff
	for attempt := 0; attempt <= s.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			wait *= 2
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", s.URL, bytes.NewReader(body.Bytes()))
		req.Header.Set("Authorization", "Splunk "+s.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.Client.Do(req)
		if err != nil {
			last = err
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		switch {
		case resp.StatusCode == 200:
			return nil
		case resp.StatusCode >= 500 || resp.StatusCode == 429:
			last = fmt.Errorf("splunk returned HTTP %d", resp.StatusCode)
		default:
			var r hecReply
			_ = json.Unmarshal(b, &r)
			return &PermanentError{Status: resp.StatusCode, Text: r.Text}
		}
	}
	return fmt.Errorf("giving up after %d attempts: %w", s.Retries+1, last)
}

// Batcher collects events and sends them every Interval or every MaxBatch
// events, whichever comes first. Add never blocks on the network, so a slow
// Splunk cannot slow a deploy down.
type Batcher struct {
	S        *Sender
	Interval time.Duration
	MaxBatch int
	OnError  func(error)

	mu     sync.Mutex
	events []any
	at     []time.Time
	stop   chan struct{}
	done   chan struct{}
	kick   chan struct{}
}

// NewBatcher starts the background sender. Zero values mean one second and 100 events.
func NewBatcher(s *Sender, interval time.Duration, maxBatch int) *Batcher {
	if interval <= 0 {
		interval = time.Second
	}
	if maxBatch <= 0 {
		maxBatch = 100
	}
	b := &Batcher{S: s, Interval: interval, MaxBatch: maxBatch, stop: make(chan struct{}), done: make(chan struct{}), kick: make(chan struct{}, 1)}
	go b.loop()
	return b
}

func (b *Batcher) Add(ev any) {
	b.mu.Lock()
	b.events = append(b.events, ev)
	b.at = append(b.at, time.Now())
	full := len(b.events) >= b.MaxBatch
	b.mu.Unlock()
	if full {
		select { // wake the sender; if it is already awake there is nothing to do
		case b.kick <- struct{}{}:
		default:
		}
	}
}

// flush sends everything buffered, at most MaxBatch events per request.
func (b *Batcher) flush() {
	b.mu.Lock()
	ev, at := b.events, b.at
	b.events, b.at = nil, nil
	b.mu.Unlock()
	for len(ev) > 0 {
		n := b.MaxBatch
		if n > len(ev) {
			n = len(ev)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := b.S.Send(ctx, at[:n], ev[:n])
		cancel()
		if err != nil && b.OnError != nil {
			b.OnError(err)
		}
		ev, at = ev[n:], at[n:]
	}
}

func (b *Batcher) loop() {
	defer close(b.done)
	t := time.NewTicker(b.Interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			b.flush()
		case <-b.kick:
			b.flush()
		case <-b.stop:
			b.flush()
			return
		}
	}
}

// Close sends what is left and stops the background sender.
func (b *Batcher) Close() {
	close(b.stop)
	<-b.done
}
