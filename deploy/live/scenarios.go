package live

import (
	"context"
	"time"

	"github.com/gerardrecinto/webscale-edge-lab/deploy"
	"github.com/gerardrecinto/webscale-edge-lab/load"
	"github.com/gerardrecinto/webscale-edge-lab/proxy"
)

// Summary is what clients and the controller saw during one deployment.
type Summary struct {
	Scenario   string         `json:"scenario"`
	Proxy      string         `json:"proxy"`
	Instances  int            `json:"instances"`
	Version    string         `json:"version"`
	Status     string         `json:"status"`
	Reason     string         `json:"reason,omitempty"`
	DurationMs int64          `json:"durationMs"`
	PeakOut    int            `json:"peakOut"`
	Limit      int            `json:"limit"`
	Requests   int            `json:"requests"`
	Failed     int            `json:"failed"`
	Builds     map[string]int `json:"builds"` // responses by build
	Reloads    int            `json:"reloads,omitempty"`
	Canary     *deploy.Window `json:"canary,omitempty"`
	Baseline   *deploy.Window `json:"baseline,omitempty"`
	LastFailMs int64          `json:"lastFailMs"` // time of the last failed request, -1 if none
	EndedAtMs  int64          `json:"endedAtMs"`  // when the deploy finished, on the same clock
	Final      map[string]int `json:"finalBuilds"`
}

func run(k proxy.Kind, n int, rate int, plan func(*Lab, *load.Generator) deploy.Plan, scenario string) (Summary, *Lab, error) {
	lab, err := NewLab(k, n, 700*time.Millisecond)
	if err != nil {
		return Summary{}, nil, err
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	g := load.Start(ctx, load.Options{URL: lab.Proxy.URL(), Rate: rate, Workers: 8})
	time.Sleep(time.Second)
	p := plan(lab, g)
	var e deploy.Engine
	rep := e.Run(context.Background(), lab.Targets, p)
	endedAt := g.Run.Now()
	time.Sleep(time.Second)
	stop()
	g.Wait()
	s := Summary{Scenario: scenario, Proxy: string(k), Instances: n, Version: p.Version, Status: rep.Status, Reason: rep.Reason,
		DurationMs: rep.Duration.Milliseconds(), PeakOut: rep.PeakOut, Limit: p.MaxUnavailable, Builds: map[string]int{},
		Canary: rep.Canary, Baseline: rep.Baseline, LastFailMs: load.LastFailure(g.Run.Snapshot()), EndedAtMs: endedAt, Final: map[string]int{}}
	for _, x := range g.Run.Snapshot() {
		s.Requests++
		if x.Status != 200 {
			s.Failed++
		}
		s.Builds[x.Version]++
	}
	for _, b := range lab.Backends {
		s.Final[b.Version()]++
	}
	if rr, ok := lab.Router.(*ReloadRouter); ok {
		s.Reloads = rr.Reloads
	}
	return s, lab, nil
}

// RollingUnderLoad replaces every instance, a batch at a time, while clients keep sending requests.
func RollingUnderLoad(k proxy.Kind, n, batch int) (Summary, error) {
	s, lab, err := run(k, n, 200, func(*Lab, *load.Generator) deploy.Plan {
		return deploy.Plan{Version: "v2", MaxUnavailable: batch, StepTimeout: 30 * time.Second}
	}, "rolling-deploy")
	if lab != nil {
		lab.Close()
	}
	return s, err
}

// CanaryCatchesFlakyBuild rolls out a build that passes /healthz but fails every third request.
func CanaryCatchesFlakyBuild(n int) (Summary, error) {
	s, lab, err := run(proxy.HAProxy, n, 200, func(_ *Lab, g *load.Generator) deploy.Plan {
		return deploy.Plan{Version: "v3-flaky", MaxUnavailable: 2, Canary: 1, CanaryWindow: 3 * time.Second, StepTimeout: 30 * time.Second,
			Observe: Observe(g.Run, "v3-flaky", "v1"), Judge: deploy.Judge{MinRequests: 40, MaxErrorRate: 0.05, MaxErrorGap: 0.02}}
	}, "canary-flaky-build")
	if lab != nil {
		lab.Close()
	}
	return s, err
}

// ReadinessGateBlocksBrokenBuild rolls out a build that fails every request.
func ReadinessGateBlocksBrokenBuild(n int) (Summary, error) {
	s, lab, err := run(proxy.HAProxy, n, 150, func(*Lab, *load.Generator) deploy.Plan {
		return deploy.Plan{Version: "v4-broken", MaxUnavailable: 1, StepTimeout: 20 * time.Second}
	}, "readiness-gate-broken-build")
	if lab != nil {
		lab.Close()
	}
	return s, err
}
