package splunk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type collector struct {
	mu     sync.Mutex
	bodies []string
	calls  atomic.Int32
}

func (c *collector) handler(failFirst int, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Splunk "+token {
			w.WriteHeader(403)
			io.WriteString(w, `{"text":"Invalid token","code":4}`)
			return
		}
		if int(c.calls.Add(1)) <= failFirst {
			w.WriteHeader(503)
			return
		}
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, string(b))
		c.mu.Unlock()
		io.WriteString(w, `{"text":"Success","code":0}`)
	}
}

func TestSendBatchesEventsInOneRequestWithTheRightEnvelope(t *testing.T) {
	var c collector
	srv := httptest.NewServer(c.handler(0, "tok"))
	defer srv.Close()
	s := &Sender{URL: srv.URL, Token: "tok", Host: "deploy-01"}
	at := time.Date(2026, 11, 6, 10, 0, 0, 500_000_000, time.UTC)
	err := s.Send(context.Background(), []time.Time{at, at}, []any{map[string]any{"type": "target_drain", "target": "web-1"}, map[string]any{"type": "target_enable", "target": "web-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.bodies) != 1 {
		t.Fatalf("%d requests, want 1", len(c.bodies))
	}
	dec := json.NewDecoder(strings.NewReader(c.bodies[0]))
	var n int
	for dec.More() {
		var e struct {
			Time       float64        `json:"time"`
			Host       string         `json:"host"`
			Source     string         `json:"source"`
			SourceType string         `json:"sourcetype"`
			Event      map[string]any `json:"event"`
		}
		if err := dec.Decode(&e); err != nil {
			t.Fatal(err)
		}
		if e.Time != 1793959200.5 || e.Host != "deploy-01" || e.Source != "pushbutton" || e.SourceType != "pushbutton:deploy" || e.Event["target"] != "web-1" {
			t.Errorf("%+v", e)
		}
		n++
	}
	if n != 2 {
		t.Errorf("%d events in the body", n)
	}
}

func TestSendRetriesServerErrorsButNotABadToken(t *testing.T) {
	var c collector
	srv := httptest.NewServer(c.handler(2, "tok"))
	defer srv.Close()
	s := &Sender{URL: srv.URL, Token: "tok", Backoff: time.Millisecond}
	if err := s.Send(context.Background(), nil, []any{"x"}); err != nil {
		t.Fatalf("two 503s then success should work: %v", err)
	}
	if c.calls.Load() != 3 {
		t.Errorf("%d calls", c.calls.Load())
	}
	var c2 collector
	srv2 := httptest.NewServer(c2.handler(0, "right"))
	defer srv2.Close()
	bad := &Sender{URL: srv2.URL, Token: "wrong", Backoff: time.Millisecond}
	err := bad.Send(context.Background(), nil, []any{"x"})
	var pe *PermanentError
	if !errors.As(err, &pe) || pe.Status != 403 || pe.Text != "Invalid token" {
		t.Fatalf("want a permanent 403, got %v", err)
	}
}

func TestSendGivesUpAfterTheRetries(t *testing.T) {
	var c collector
	srv := httptest.NewServer(c.handler(100, "tok"))
	defer srv.Close()
	s := &Sender{URL: srv.URL, Token: "tok", Backoff: time.Millisecond, Retries: 1}
	err := s.Send(context.Background(), nil, []any{"x"})
	if err == nil || !strings.Contains(err.Error(), "giving up after 2 attempts") {
		t.Fatalf("%v", err)
	}
}

func TestBatcherFlushesOnCloseAndNeverBlocksAdd(t *testing.T) {
	var c collector
	srv := httptest.NewServer(c.handler(0, "tok"))
	defer srv.Close()
	b := NewBatcher(&Sender{URL: srv.URL, Token: "tok"}, time.Hour, 0)
	start := time.Now()
	for i := 0; i < 5; i++ {
		b.Add(map[string]int{"n": i})
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Error("Add took too long")
	}
	b.Close()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) != 1 || strings.Count(c.bodies[0], `"event"`) != 5 {
		t.Fatalf("bodies %v", c.bodies)
	}
}

func TestBatcherFlushesAtMaxBatch(t *testing.T) {
	var c collector
	srv := httptest.NewServer(c.handler(0, "tok"))
	defer srv.Close()
	b := NewBatcher(&Sender{URL: srv.URL, Token: "tok"}, time.Hour, 3)
	for i := 0; i < 7; i++ {
		b.Add(i)
	}
	b.Close()
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, body := range c.bodies {
		n := strings.Count(body, `"event"`)
		if n > 3 {
			t.Errorf("a request carried %d events, the limit is 3", n)
		}
		total += n
	}
	if total != 7 || len(c.bodies) < 3 {
		t.Fatalf("%d events in %d requests", total, len(c.bodies))
	}
}
