// failover-drill runs the lab failover scenarios and prints what it measured.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/gerardrecinto/webscale-edge-lab/failover"
)

func main() {
	scenario := flag.String("scenario", "all", "crash, partition, laggate or all")
	lag := flag.Duration("repl-lag", 0, "replication delay (default 40ms; laggate uses 1.5s)")
	flag.Parse()

	names := []failover.Scenario{failover.Crash, failover.Partition, failover.LagGate}
	if *scenario != "all" {
		names = []failover.Scenario{failover.Scenario(*scenario)}
	}
	for _, n := range names {
		p := failover.Params{Scenario: n, ReplLag: *lag}
		if n == failover.LagGate && *lag == 0 {
			p.ReplLag = 1500 * time.Millisecond
			p.Config.MaxLagOps = 20
		}
		r, err := failover.RunDrill(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("== %s\n", r.Scenario)
		for _, l := range r.Timeline {
			fmt.Println("  ", l)
		}
		fmt.Printf("   writes acked %d, client errors %d, breaker rejected calls: %v\n", r.Writes, r.Failed, r.BreakerOpened)
		fmt.Printf("   detect %dms  RTO %dms  lost writes %d (RPO window %dms)  stale writes accepted after promote %d  lag gate hit: %v\n\n",
			r.Detect.Milliseconds(), r.RTO.Milliseconds(), r.Lost, r.RPOWindow.Milliseconds(), r.StaleAccepted, r.Gated)
	}
}
