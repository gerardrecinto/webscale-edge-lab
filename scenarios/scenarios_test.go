package scenarios

import (
	"testing"

	"github.com/gerardrecinto/webscale-edge-lab/proxy"
)

func need(t *testing.T, v Variant) {
	t.Helper()
	if !proxy.Available(v.Kind) {
		t.Skipf("%s not installed", v.Kind)
	}
}

func run(t *testing.T, v Variant, f func(Variant) (*Result, error)) *Result {
	t.Helper()
	need(t, v)
	r, err := f(v)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s %s %v", r.Scenario, r.Variant, r.Metrics)
	return r
}

func num(r *Result, k string) float64 {
	switch x := r.Metrics[k].(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return -1
}

func TestDistributionIsEven(t *testing.T) {
	for _, v := range []Variant{Variants[0], Variants[2]} {
		r := run(t, v, Distribution)
		if num(r, "maxDeviationFromEven") > 0.1 || num(r, "errors") != 0 {
			t.Fatalf("%s: %v", v.Name, r.Metrics)
		}
	}
}

func TestCrashedBackendIsRoutedAroundWithAlmostNoClientErrors(t *testing.T) {
	for _, v := range []Variant{Variants[0], Variants[2]} {
		r := run(t, v, BackendCrash)
		if num(r, "clientErrorsAfterCrash") > 5 {
			t.Fatalf("%s: %v", v.Name, r.Metrics)
		}
		if b := num(r, "msUntilRestartedBackendServesAgain"); b < 0 || b > 3000 {
			t.Fatalf("%s: restarted backend never rejoined: %v", v.Name, r.Metrics)
		}
	}
}

// A backend that answers 503 while its health endpoint says 200 is invisible
// to active checks. HAProxy needs "observe layer7" to react; Envoy's outlier
// detection does it once max_ejection_percent allows ejecting one of three hosts.
func TestBackendReturning5xxIsOnlyEjectedWithPassiveChecks(t *testing.T) {
	plain := run(t, Variants[0], BackendErrors)
	if num(plain, "errorRateAfter") < 0.25 {
		t.Fatalf("plain haproxy should keep sending a third of requests to the broken backend: %v", plain.Metrics)
	}
	for _, v := range []Variant{Variants[1], Variants[2]} {
		r := run(t, v, BackendErrors)
		// Envoy un-ejects after base_ejection_time (3s) and has to see three more errors before
		// ejecting again, so it does not reach zero in this window; it must still beat the plain third.
		if num(r, "errorRateAfter") > 0.25 {
			t.Fatalf("%s should have ejected the broken backend: %v", v.Name, r.Metrics)
		}
	}
}

func TestFailingHealthCheckDrainsATargetWithoutClientErrors(t *testing.T) {
	for _, v := range []Variant{Variants[0], Variants[2]} {
		r := run(t, v, HealthCheckDrain)
		if num(r, "errors") != 0 || num(r, "msUntilTrafficStops") > 1500 {
			t.Fatalf("%s: %v", v.Name, r.Metrics)
		}
	}
}

func TestOverloadIsShedNotQueuedForever(t *testing.T) {
	for _, v := range []Variant{Variants[0], Variants[2]} {
		r := run(t, v, Overload)
		if num(r, "shed503") < 10 || num(r, "otherFailures") != 0 || num(r, "okP95Ms") > 800 {
			t.Fatalf("%s: %v", v.Name, r.Metrics)
		}
	}
}

func TestPerClientRateLimitLeavesQuietClientsAlone(t *testing.T) {
	r := run(t, Variants[0], RateLimit)
	if num(r, "noisy429") < 600 || num(r, "noisyOK") > 100 || num(r, "quietFailed") != 0 {
		t.Fatalf("%v", r.Metrics)
	}
}

func TestRollingDeployHasZeroFailedRequests(t *testing.T) {
	for _, v := range []Variant{Variants[0], Variants[2]} {
		r := run(t, v, RollingDeploy)
		if num(r, "errors") != 0 {
			t.Fatalf("%s: %v", v.Name, r.Metrics)
		}
	}
}

func TestConsistentHashingMovesFewerUsersThanModulo(t *testing.T) {
	cons, err := ConsistentHash(Variants[0], "hash-consistent")
	need(t, Variants[0])
	if err != nil {
		t.Fatal(err)
	}
	mod, err := ConsistentHash(Variants[0], "hash-modulo")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("consistent %v\nmodulo %v", cons.Metrics, mod.Metrics)
	if num(cons, "usersStableOnRepeat") != 300 || num(mod, "usersStableOnRepeat") != 300 {
		t.Fatal("same user must always reach the same backend")
	}
	if num(cons, "unrelatedMovedFraction") > 0.05 {
		t.Fatalf("consistent hashing moved unrelated users: %v", cons.Metrics)
	}
	if num(mod, "unrelatedMovedFraction") < 0.3 {
		t.Fatalf("modulo hashing should reshuffle most users: %v", mod.Metrics)
	}
	if proxy.Available(proxy.Envoy) {
		ring, err := ConsistentHash(Variants[2], "hash-consistent")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("envoy ring %v", ring.Metrics)
		if num(ring, "usersStableOnRepeat") != 300 || num(ring, "unrelatedMovedFraction") >= num(mod, "unrelatedMovedFraction") {
			t.Fatalf("%v", ring.Metrics)
		}
	}
}
