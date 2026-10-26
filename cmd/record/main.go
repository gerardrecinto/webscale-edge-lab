// record runs every scenario against real HAProxy and Envoy processes and
// writes docs/data.json for the Pages viewer.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/gerardrecinto/webscale-edge-lab/proxy"
	"github.com/gerardrecinto/webscale-edge-lab/scenarios"
)

func main() {
	var results []*scenarios.Result
	add := func(r *scenarios.Result, err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERR", err)
			os.Exit(1)
		}
		fmt.Printf("%-20s %-24s %v\n", r.Scenario, r.Variant, r.Metrics)
		results = append(results, r)
	}
	V := scenarios.Variants
	for _, v := range []scenarios.Variant{V[0], V[2]} {
		add(scenarios.Distribution(v))
		add(scenarios.BackendCrash(v))
		add(scenarios.HealthCheckDrain(v))
		add(scenarios.Overload(v))
		add(scenarios.RollingDeploy(v))
	}
	for _, v := range V {
		add(scenarios.BackendErrors(v))
	}
	add(scenarios.RateLimit(V[0]))
	add(scenarios.ConsistentHash(V[0], "hash-consistent"))
	add(scenarios.ConsistentHash(V[0], "hash-modulo"))
	add(scenarios.ConsistentHash(V[2], "hash-consistent"))

	cfg := proxy.Config{Backends: []string{"127.0.0.1:9001", "127.0.0.1:9002", "127.0.0.1:9003"}, Names: []string{"b0", "b1", "b2"}, Balance: "roundrobin", Passive: true}
	out := map[string]any{
		"results": results,
		"configs": map[string]string{
			"haproxy": proxy.Render(proxy.HAProxy, cfg),
			"envoy":   proxy.Render(proxy.Envoy, proxy.Config{Backends: cfg.Backends, Names: cfg.Names, Balance: "roundrobin"}),
		},
		"versions": map[string]string{"haproxy": "3.4.4", "envoy": "1.38.3"},
	}
	b, _ := json.MarshalIndent(out, "", " ")
	if err := os.WriteFile("docs/data.json", b, 0o644); err != nil {
		panic(err)
	}
}
