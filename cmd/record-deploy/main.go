// record-deploy runs the push-button deployment scenarios against real HAProxy,
// Nginx and Apache and a simulated fleet of 1000 hosts, and merges the results
// into docs/data.json under "deploy". It leaves the results from cmd/record alone.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gerardrecinto/webscale-edge-lab/deploy"
	"github.com/gerardrecinto/webscale-edge-lab/deploy/live"
	"github.com/gerardrecinto/webscale-edge-lab/proxy"
)

func version(cmd string, args ...string) string {
	out, err := exec.Command(cmd, args...).CombinedOutput()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

func main() {
	var rows []live.Summary
	add := func(s live.Summary, err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERR", err)
			os.Exit(1)
		}
		fmt.Printf("%-28s %-8s %-12s %5dms peak %d/%d  %d requests %d failed %v\n", s.Scenario, s.Proxy, s.Status, s.DurationMs, s.PeakOut, s.Limit, s.Requests, s.Failed, s.Builds)
		rows = append(rows, s)
	}
	for _, k := range []proxy.Kind{proxy.HAProxy, proxy.Nginx, proxy.Apache} {
		add(live.RollingUnderLoad(k, 6, 2))
	}
	add(live.CanaryCatchesFlakyBuild(6))
	add(live.ReadinessGateBlocksBrokenBuild(4))

	// The controller's rules at scale, on a simulated fleet.
	sim := func(fail bool) map[string]any {
		s := deploy.NewSim(1000, "v1", time.Millisecond)
		if fail {
			s.Host("host-0437").FailAt("v2", "verify", "readiness probe returned 503")
		}
		var e deploy.Engine
		rep := e.Run(context.Background(), s.Targets(), deploy.Plan{Version: "v2", MaxUnavailable: 50})
		return map[string]any{"hosts": 1000, "batch": 50, "status": rep.Status, "durationMs": rep.Duration.Milliseconds(), "peakOut": rep.PeakOut,
			"fewestServing": s.MinInRotation(), "builds": s.Versions(), "failedTarget": rep.FailedHost, "reason": rep.Reason}
	}
	sims := []map[string]any{sim(false), sim(true)}
	fmt.Println("sim", sims)

	b, err := os.ReadFile("docs/data.json")
	if err != nil {
		panic(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		panic(err)
	}
	doc["deploy"] = map[string]any{
		"rows": rows,
		"sim":  sims,
		"versions": map[string]string{
			"haproxy": version("haproxy", "-v"), "nginx": version("nginx", "-v"), "apache": version("apache2", "-v"),
		},
		"recorded": time.Now().UTC().Format("2006-01-02"),
		"note":     "Linux container, loopback only, backends and proxies on one machine. Each instance takes 700 ms to start, as a stand-in for a real service.",
	}
	out, _ := json.MarshalIndent(doc, "", " ")
	if err := os.WriteFile("docs/data.json", out, 0o644); err != nil {
		panic(err)
	}
}
