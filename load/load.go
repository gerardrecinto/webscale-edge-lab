// Package load sends steady request streams through a proxy and records what
// each request saw.
package load

import (
	"context"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

type Sample struct {
	AtMs      int64  `json:"atMs"`
	LatencyMs int64  `json:"latencyMs"`
	Status    int    `json:"status"` // 0 means the request failed before a response
	Backend   string `json:"backend"`
}

type Run struct {
	Start   time.Time
	mu      sync.Mutex
	Samples []Sample
}

func (r *Run) add(s Sample) { r.mu.Lock(); r.Samples = append(r.Samples, s); r.mu.Unlock() }

func (r *Run) Snapshot() []Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]Sample(nil), r.Samples...)
	sort.Slice(out, func(i, j int) bool { return out[i].AtMs < out[j].AtMs })
	return out
}

func (r *Run) Now() int64 { return time.Since(r.Start).Milliseconds() }

type Options struct {
	URL     string
	Rate    int // requests per second across all workers
	Workers int
	Header  func(i int) map[string]string
}

// Start runs until ctx is cancelled. Call Wait afterwards.
type Generator struct {
	Run *Run
	wg  sync.WaitGroup
}

func Start(ctx context.Context, o Options) *Generator {
	if o.Workers == 0 {
		o.Workers = 4
	}
	g := &Generator{Run: &Run{Start: time.Now()}}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	per := time.Second * time.Duration(o.Workers) / time.Duration(o.Rate)
	for w := 0; w < o.Workers; w++ {
		g.wg.Add(1)
		go func(w int) {
			defer g.wg.Done()
			t := time.NewTicker(per)
			defer t.Stop()
			i := w
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				i += o.Workers
				req, _ := http.NewRequest("GET", o.URL, nil)
				if o.Header != nil {
					for k, v := range o.Header(i) {
						req.Header.Set(k, v)
					}
				}
				at := g.Run.Now()
				t0 := time.Now()
				resp, err := client.Do(req)
				s := Sample{AtMs: at, LatencyMs: time.Since(t0).Milliseconds()}
				if err == nil {
					s.Status = resp.StatusCode
					s.Backend = resp.Header.Get("X-Backend")
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				g.Run.add(s)
			}
		}(w)
	}
	return g
}

func (g *Generator) Wait() { g.wg.Wait() }

// Summary helpers

func Count(s []Sample, from, to int64, pred func(Sample) bool) int {
	n := 0
	for _, x := range s {
		if x.AtMs >= from && x.AtMs < to && pred(x) {
			n++
		}
	}
	return n
}

func OK(s Sample) bool     { return s.Status == 200 }
func Failed(s Sample) bool { return s.Status != 200 }

func ByBackend(s []Sample, from, to int64) map[string]int {
	m := map[string]int{}
	for _, x := range s {
		if x.AtMs >= from && x.AtMs < to && x.Status == 200 {
			m[x.Backend]++
		}
	}
	return m
}

func Percentile(s []Sample, p float64, pred func(Sample) bool) int64 {
	var l []int64
	for _, x := range s {
		if pred(x) {
			l = append(l, x.LatencyMs)
		}
	}
	if len(l) == 0 {
		return 0
	}
	sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
	return l[int(p*float64(len(l)-1))]
}

// LastFailure is the time of the last failed request, or -1.
func LastFailure(s []Sample) int64 {
	last := int64(-1)
	for _, x := range s {
		if x.Status != 200 && x.AtMs > last {
			last = x.AtMs
		}
	}
	return last
}

// One sends a single request and reports what it saw.
func One(url string, hdr map[string]string) Sample {
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	t0 := time.Now()
	resp, err := client.Do(req)
	s := Sample{LatencyMs: time.Since(t0).Milliseconds()}
	if err == nil {
		s.Status = resp.StatusCode
		s.Backend = resp.Header.Get("X-Backend")
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	return s
}
