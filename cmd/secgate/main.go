// secgate is the pipeline step that decides whether a build may ship.
//
//	secgate trivy -policy policy.json report.json
//	secgate headers https://staging.example.com/
//
// It exits 1 when the gate fails, so a Jenkins or GitHub Actions step stops there.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/gerardrecinto/webscale-edge-lab/secgate"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: secgate trivy [-policy file] report.json | secgate headers URL")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "trivy":
		os.Exit(trivy(os.Args[2:]))
	case "headers":
		os.Exit(headers(os.Args[2:]))
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}

func trivy(args []string) int {
	fs := flag.NewFlagSet("trivy", flag.ExitOnError)
	policyFile := fs.String("policy", "", "policy JSON file (default: fail on HIGH and above)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: secgate trivy [-policy file] report.json")
		return 2
	}
	pol := secgate.Policy{FailOn: "HIGH"}
	if *policyFile != "" {
		b, err := os.ReadFile(*policyFile)
		if err == nil {
			err = json.Unmarshal(b, &pol)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "policy:", err)
			return 2
		}
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer f.Close()
	findings, err := secgate.ParseTrivy(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	d := secgate.Evaluate(findings, pol, time.Now())
	for _, x := range d.Blocking {
		fix := x.Fixed
		if fix == "" {
			fix = "no fix yet"
		}
		fmt.Printf("BLOCK  %-8s %-18s %s %s (%s)\n", x.Severity, x.ID, x.Package, x.Installed, fix)
	}
	for _, x := range d.Expired {
		fmt.Printf("       %s: the waiver has expired\n", x.ID)
	}
	for _, x := range d.Waived {
		fmt.Printf("WAIVED %-8s %-18s %s\n", x.Severity, x.ID, x.Package)
	}
	for _, p := range d.Problems {
		fmt.Println("POLICY", p)
	}
	fmt.Printf("%d findings: %d blocking, %d waived, %d below %s, %d without a fix skipped\n", len(findings), len(d.Blocking), len(d.Waived), d.Below, pol.FailOn, d.NoFix)
	if !d.Pass {
		return 1
	}
	return 0
}

func headers(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: secgate headers URL")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", args[0], nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	resp.Body.Close()
	found := secgate.CheckHeaders(resp.Header)
	for _, f := range found {
		fmt.Printf("%-4s %-28s %s\n", f.Level, f.Header, f.Why)
	}
	if len(found) == 0 {
		fmt.Println("all checked headers present")
	}
	if secgate.HasFailures(found) {
		return 1
	}
	return 0
}
