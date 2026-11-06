// hostdoctor reads /proc and says what is wrong with this Linux host and what to run next.
//
//	hostdoctor                 check this machine
//	hostdoctor -pid 4242       also check one process's file descriptors
//	hostdoctor -json           machine-readable output
//
// Exit status is 1 when there is a critical finding.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/gerardrecinto/webscale-edge-lab/hostdoctor"
)

func main() {
	proc := flag.String("proc", "/proc", "path to a /proc tree (a copy captured from another host works)")
	pid := flag.Int("pid", 0, "also check this process")
	asJSON := flag.Bool("json", false, "print JSON")
	flag.Parse()
	f, err := hostdoctor.Collect(*proc, hostdoctor.Options{PID: *pid})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	findings := hostdoctor.Diagnose(f)
	crit := false
	for _, x := range findings {
		if x.Level == "crit" {
			crit = true
		}
	}
	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]any{"facts": f, "findings": findings})
	} else {
		fmt.Printf("%d CPUs, load %.2f, %d MB of %d MB memory available, TCP %v\n\n", f.CPUs, f.Load1, f.MemAvail/1024, f.MemTotal/1024, f.TCP)
		if len(findings) == 0 {
			fmt.Println("nothing to report")
		}
		for _, x := range findings {
			fmt.Printf("%-4s %-10s %s\n          next: %s\n", x.Level, x.Area, x.What, x.Next)
		}
	}
	if crit {
		os.Exit(1)
	}
}
