package live

import (
	"context"
	"testing"
	"time"

	"github.com/gerardrecinto/webscale-edge-lab/deploy"
	"github.com/gerardrecinto/webscale-edge-lab/load"
	"github.com/gerardrecinto/webscale-edge-lab/proxy"
)

func failures(g *load.Generator) (total, failed int, versions map[string]int) {
	versions = map[string]int{}
	for _, s := range g.Run.Snapshot() {
		total++
		if s.Status != 200 {
			failed++
		}
		versions[s.Version]++
	}
	return
}

func skipUnless(t *testing.T, k proxy.Kind) {
	t.Helper()
	if !proxy.Available(k) {
		t.Skipf("%s not installed", k)
	}
}

func TestRollingDeployUnderLoadDropsNoRequests(t *testing.T) {
	for _, k := range []proxy.Kind{proxy.HAProxy, proxy.Nginx, proxy.Apache} {
		t.Run(string(k), func(t *testing.T) {
			skipUnless(t, k)
			s, err := RollingUnderLoad(k, 6, 2)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %s in %dms, peak out %d, %d requests, %d failed, responses by build %v, %d reloads", k, s.Status, s.DurationMs, s.PeakOut, s.Requests, s.Failed, s.Builds, s.Reloads)
			if s.Status != deploy.StatusSucceeded {
				t.Fatalf("status %s: %s", s.Status, s.Reason)
			}
			if s.Failed != 0 {
				t.Errorf("%d of %d client requests failed during the deploy", s.Failed, s.Requests)
			}
			if s.PeakOut > 2 {
				t.Errorf("peak out %d, limit 2", s.PeakOut)
			}
			if s.Builds["v1"] == 0 || s.Builds["v2"] == 0 {
				t.Errorf("traffic did not span the deploy: %v", s.Builds)
			}
			if s.Final["v2"] != 6 {
				t.Errorf("final builds %v", s.Final)
			}
		})
	}
}

func TestCanaryStopsABuildThatPassesHealthChecksButFailsRequests(t *testing.T) {
	skipUnless(t, proxy.HAProxy)
	s, err := CanaryCatchesFlakyBuild(6)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s (%s), %d requests, %d failed (%.1f%%), canary %+v baseline %+v", s.Status, s.Reason, s.Requests, s.Failed, 100*float64(s.Failed)/float64(s.Requests), s.Canary, s.Baseline)
	if s.Status != deploy.StatusRolledBack {
		t.Fatalf("status %s: %s", s.Status, s.Reason)
	}
	if s.Final["v1"] != 6 {
		t.Errorf("builds after rollback %v", s.Final)
	}
	// Only the one canary instance, a sixth of the traffic, could ever fail, and only for the window.
	if frac := float64(s.Failed) / float64(s.Requests); frac > 0.08 {
		t.Errorf("%.1f%% of requests failed, the canary should have limited it to a few percent", 100*frac)
	}
	if s.LastFailMs > s.EndedAtMs {
		t.Errorf("a request failed at %dms, after the rollback finished at %dms", s.LastFailMs, s.EndedAtMs)
	}
	if s.Builds["v3-flaky"] == 0 {
		t.Error("the canary never took traffic")
	}
}

func TestBuildThatFailsReadinessNeverTakesTraffic(t *testing.T) {
	skipUnless(t, proxy.HAProxy)
	s, err := ReadinessGateBlocksBrokenBuild(4)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s (%s), %d requests, %d failed, builds %v", s.Status, s.Reason, s.Requests, s.Failed, s.Builds)
	if s.Status != deploy.StatusRolledBack {
		t.Fatalf("status %s: %s", s.Status, s.Reason)
	}
	if s.Failed != 0 || s.Builds["v4-broken"] != 0 {
		t.Errorf("%d failed, %d answered by the broken build; neither should happen", s.Failed, s.Builds["v4-broken"])
	}
}

var _ = context.Background
var _ = time.Second
