package live

import (
	"fmt"
	"time"

	"github.com/gerardrecinto/webscale-edge-lab/backend"
	"github.com/gerardrecinto/webscale-edge-lab/deploy"
	"github.com/gerardrecinto/webscale-edge-lab/load"
	"github.com/gerardrecinto/webscale-edge-lab/proxy"
)

// Lab is a real proxy in front of real backend processes (goroutines with their
// own listeners), wired to deploy targets. Tests and the CLI demo both use it.
type Lab struct {
	Proxy    *proxy.Proxy
	Backends []*backend.Backend
	Targets  []deploy.Target
	Router   Router
}

// Quirks are builds that misbehave on purpose.
func Quirks() map[string]func(*backend.Backend) {
	return map[string]func(*backend.Backend){
		"v3-flaky":  func(b *backend.Backend) { b.SetFlaky(3) },     // fails every third request, passes /healthz
		"v4-broken": func(b *backend.Backend) { b.SetBroken(true) }, // fails everything
	}
}

// NewLab starts n backends on build v1 behind the given proxy and waits until
// the proxy is serving from all of them.
func NewLab(k proxy.Kind, n int, startDelay time.Duration) (*Lab, error) {
	if !proxy.Available(k) {
		return nil, fmt.Errorf("%s is not installed", k)
	}
	l := &Lab{}
	var cfg proxy.Config
	for i := 0; i < n; i++ {
		b, err := backend.New(fmt.Sprintf("b%d", i))
		if err != nil {
			l.Close()
			return nil, err
		}
		l.Backends = append(l.Backends, b)
		cfg.Backends = append(cfg.Backends, b.Addr())
		cfg.Names = append(cfg.Names, b.ID)
	}
	p, err := proxy.Start(k, cfg)
	if err != nil {
		l.Close()
		return nil, err
	}
	l.Proxy = p
	if k == proxy.HAProxy {
		l.Router = NewHAProxyRouter(p)
	} else {
		l.Router = NewReloadRouter(p)
	}
	for _, b := range l.Backends {
		t := NewAppTarget(b.ID, &GoApp{B: b, Quirks: Quirks(), StartDelay: startDelay}, l.Router, "v1")
		t.VerifyTimeout = 4 * time.Second
		l.Targets = append(l.Targets, t)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		seen := map[string]bool{}
		for i := 0; i < 3*n; i++ {
			seen[load.One(p.URL(), nil).Backend] = true
		}
		if len(seen) == n {
			return l, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	l.Close()
	return nil, fmt.Errorf("the proxy never served from every backend")
}

func (l *Lab) Close() {
	if l.Proxy != nil {
		l.Proxy.Stop()
	}
	for _, b := range l.Backends {
		b.Crash()
	}
}
