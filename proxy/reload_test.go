package proxy

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gerardrecinto/webscale-edge-lab/backend"
)

func serveCounts(t *testing.T, url string, n int) map[string]int {
	t.Helper()
	m := map[string]int{}
	for i := 0; i < n; i++ {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
		m[resp.Header.Get("X-Backend")]++
	}
	return m
}

// A server marked down in the config stops getting requests after a graceful
// reload, with no failed request, and gets them again once it is unmarked.
func TestReloadRemovesAndRestoresABackend(t *testing.T) {
	for _, k := range []Kind{Nginx, Apache} {
		t.Run(string(k), func(t *testing.T) {
			if !Available(k) {
				t.Skipf("%s not installed", k)
			}
			var cfg Config
			for _, id := range []string{"b0", "b1", "b2"} {
				b, err := backend.New(id)
				if err != nil {
					t.Fatal(err)
				}
				defer b.Crash()
				cfg.Backends = append(cfg.Backends, b.Addr())
				cfg.Names = append(cfg.Names, id)
			}
			p, err := Start(k, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Stop()
			time.Sleep(300 * time.Millisecond)
			if m := serveCounts(t, p.URL(), 30); len(m) != 3 {
				t.Fatalf("before reload: %v", m)
			}
			cfg.Down = map[string]bool{"b1": true}
			if err := p.Reload(cfg); err != nil {
				t.Fatal(err)
			}
			m := serveCounts(t, p.URL(), 30)
			if m["b1"] != 0 || m["b0"] == 0 || m["b2"] == 0 {
				t.Fatalf("after marking b1 down: %v", m)
			}
			cfg.Down = nil
			if err := p.Reload(cfg); err != nil {
				t.Fatal(err)
			}
			if m := serveCounts(t, p.URL(), 30); m["b1"] == 0 {
				t.Fatalf("b1 did not come back: %v", m)
			}
		})
	}
}
