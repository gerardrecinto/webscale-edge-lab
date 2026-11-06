// pushbutton rolls a build across a fleet without downtime.
//
//	pushbutton serve                     web page and REST API over a simulated fleet
//	pushbutton demo [-hosts 1000]        a rollout on a simulated fleet, printed as it happens
//	pushbutton live -proxy haproxy       the same engine against a real HAProxy, Nginx or Apache
//	                                     with real client traffic running during the deploy
//	pushbutton deploy -fleet f.json -version v2
//	                                     roll a build across real hosts driven by the commands in f.json
//	                                     (exit 0 done, 1 rolled back, 3 rollback failed, 4 cancelled)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gerardrecinto/webscale-edge-lab/deploy"
	"github.com/gerardrecinto/webscale-edge-lab/deploy/fleetfile"
	"github.com/gerardrecinto/webscale-edge-lab/deploy/live"
	"github.com/gerardrecinto/webscale-edge-lab/load"
	"github.com/gerardrecinto/webscale-edge-lab/proxy"
	"github.com/gerardrecinto/webscale-edge-lab/service"
	"github.com/gerardrecinto/webscale-edge-lab/splunk"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pushbutton serve | demo | live")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "demo":
		err = demo(os.Args[2:])
	case "live":
		err = liveRun(os.Args[2:])
	case "deploy":
		os.Exit(deployFleet(os.Args[2:]))
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pushbutton:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8090", "listen address; keep it on loopback unless a token is set")
	hosts := fs.Int("hosts", 200, "simulated fleet size")
	step := fs.Duration("step", 60*time.Millisecond, "simulated time for each drain, update, verify and enable step")
	fs.Parse(args)
	token := os.Getenv("PUSHBUTTON_TOKEN")
	if token == "" && !strings.HasPrefix(*addr, "127.0.0.1") && !strings.HasPrefix(*addr, "localhost") {
		return errors.New("refusing to listen on a non-loopback address without PUSHBUTTON_TOKEN set")
	}
	svc := service.New(deploy.NewSim(*hosts, "v1", *step), token)
	if url := os.Getenv("SPLUNK_HEC_URL"); url != "" {
		b := splunk.NewBatcher(&splunk.Sender{URL: url, Token: os.Getenv("SPLUNK_HEC_TOKEN"), Host: hostname()}, time.Second, 100)
		b.OnError = func(err error) { fmt.Fprintln(os.Stderr, "splunk:", err) }
		defer b.Close()
		svc.Emit = func(id string, e deploy.Event) {
			b.Add(map[string]any{"deployment": id, "type": e.Type, "target": e.Target, "version": e.Version, "detail": e.Detail})
		}
		fmt.Println("sending deployment events to Splunk HEC")
	}
	srv := &http.Server{Addr: *addr, Handler: svc.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	fmt.Printf("pushbutton on http://%s (%d simulated hosts, token required for writes: %v)\n", *addr, *hosts, token != "")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func hostname() string { h, _ := os.Hostname(); return h }

func demo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	hosts := fs.Int("hosts", 1000, "simulated fleet size")
	pct := fs.Float64("batch", 5, "percent of the fleet that may be out at once")
	failHost := fs.String("fail", "", "make this host's readiness check fail on the new build, for example host-0437")
	step := fs.Duration("step", time.Millisecond, "simulated time per step")
	fs.Parse(args)
	s := deploy.NewSim(*hosts, "v1", *step)
	if *failHost != "" {
		h := s.Host(*failHost)
		if h == nil {
			return fmt.Errorf("no host %s", *failHost)
		}
		h.FailAt("v2", "verify", "readiness probe returned 503")
	}
	e := deploy.Engine{Emit: func(ev deploy.Event) {
		switch ev.Type {
		case "plan", "target_failed", "rollback_start", "rollback_done", "rollback_failed", "done":
			fmt.Printf("%s  %-15s %s %s\n", ev.At.Format("15:04:05.000"), ev.Type, ev.Target, ev.Detail)
		}
	}}
	rep := e.Run(context.Background(), s.Targets(), deploy.Plan{Version: "v2", MaxUnavailable: deploy.PercentBatch(*hosts, *pct)})
	fmt.Printf("\n%s in %s: %d hosts, at most %d out at once, fewest serving %d of %d, builds now %v\n", rep.Status, rep.Duration.Round(time.Millisecond), rep.Targets, rep.PeakOut, s.MinInRotation(), *hosts, s.Versions())
	if rep.Reason != "" {
		fmt.Println("reason:", rep.Reason)
	}
	return nil
}

func liveRun(args []string) error {
	fs := flag.NewFlagSet("live", flag.ExitOnError)
	kind := fs.String("proxy", "haproxy", "haproxy, nginx or apache2")
	n := fs.Int("instances", 6, "backend instances")
	version := fs.String("version", "v2", "build to roll out; v3-flaky and v4-broken misbehave on purpose")
	rate := fs.Int("rate", 200, "client requests per second during the deploy")
	canary := fs.Int("canary", 0, "canary instances")
	fs.Parse(args)
	k := proxy.Kind(*kind)
	if k != proxy.HAProxy && k != proxy.Nginx && k != proxy.Apache {
		return fmt.Errorf("unknown proxy %q", *kind)
	}
	lab, err := live.NewLab(k, *n, 700*time.Millisecond)
	if err != nil {
		return err
	}
	defer lab.Close()
	ctx, stop := context.WithCancel(context.Background())
	g := load.Start(ctx, load.Options{URL: lab.Proxy.URL(), Rate: *rate, Workers: 8})
	time.Sleep(time.Second)
	e := deploy.Engine{Emit: func(ev deploy.Event) {
		fmt.Printf("%s  %-16s %-4s %s\n", ev.At.Format("15:04:05.000"), ev.Type, ev.Target, ev.Detail)
	}}
	plan := deploy.Plan{Version: *version, MaxUnavailable: max(1, *n/3), StepTimeout: 30 * time.Second}
	if *canary > 0 {
		plan.Canary, plan.CanaryWindow = *canary, 3*time.Second
		plan.Observe = live.Observe(g.Run, *version, "v1")
		plan.Judge = deploy.Judge{MinRequests: 40, MaxErrorRate: 0.05, MaxErrorGap: 0.02}
	}
	rep := e.Run(context.Background(), lab.Targets, plan)
	time.Sleep(time.Second)
	stop()
	g.Wait()
	total, failed := 0, 0
	by := map[string]int{}
	for _, s := range g.Run.Snapshot() {
		total++
		if s.Status != 200 {
			failed++
		}
		by[s.Version]++
	}
	fmt.Printf("\n%s behind %s: %s in %s, at most %d of %d out at once\n", *version, *kind, rep.Status, rep.Duration.Round(10*time.Millisecond), rep.PeakOut, *n)
	fmt.Printf("clients sent %d requests during the deploy, %d failed, answered by build %v\n", total, failed, by)
	if rep.Reason != "" {
		fmt.Println("reason:", rep.Reason)
	}
	return nil
}

func deployFleet(args []string) int {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	file := fs.String("fleet", "", "fleet JSON file (see examples/fleet.ssh.json)")
	version := fs.String("version", "", "build to roll out")
	pct := fs.Float64("batch", 5, "percent of the fleet that may be out of rotation at once")
	canary := fs.Int("canary", 0, "hosts to update first and judge on live numbers (needs observeCommand)")
	window := fs.Duration("canary-window", 5*time.Minute, "how long to watch the canary")
	minReq := fs.Int("min-requests", 200, "canary: fewer requests than this fails the canary")
	maxErr := fs.Float64("max-error-rate", 0.01, "canary: highest acceptable error rate")
	maxGap := fs.Float64("max-error-gap", 0.005, "canary: how much worse than the baseline the error rate may be")
	maxP95 := fs.Float64("max-p95-ratio", 1.5, "canary: p95 may be at most this many times the baseline's, 0 to skip")
	dry := fs.Bool("dry-run", false, "read the fleet and print the plan; change nothing")
	report := fs.String("report", "", "write the full report as JSON to this file")
	fs.Parse(args)
	if *file == "" || *version == "" {
		fmt.Fprintln(os.Stderr, "usage: pushbutton deploy -fleet fleet.json -version v2 [-batch 5] [-canary 2] [-dry-run]")
		return 2
	}
	fl, err := fleetfile.Load(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	targets, err := fl.Targets(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	current := map[string]int{}
	needs := 0
	for _, t := range targets {
		current[t.Version()]++
		if t.Version() != *version || !t.InRotation() {
			needs++
		}
	}
	batch := deploy.PercentBatch(len(targets), *pct)
	fmt.Printf("%d hosts, running %v; %d need work; at most %d out of rotation at once (%d batches)\n", len(targets), current, needs, batch, (needs+batch-1)/batch)
	if *dry {
		fmt.Println("dry run: nothing was changed")
		return 0
	}
	plan := deploy.Plan{Version: *version, MaxUnavailable: batch, StepTimeout: fl.StepTimeout(), Canary: *canary, CanaryWindow: *window,
		Judge: deploy.Judge{MinRequests: *minReq, MaxErrorRate: *maxErr, MaxErrorGap: *maxGap, MaxP95Ratio: *maxP95}}
	if *canary > 0 {
		if plan.Observe, err = fl.Observer(*version); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	var hec *splunk.Batcher
	if url := os.Getenv("SPLUNK_HEC_URL"); url != "" {
		hec = splunk.NewBatcher(&splunk.Sender{URL: url, Token: os.Getenv("SPLUNK_HEC_TOKEN"), Host: hostname()}, time.Second, 100)
		hec.OnError = func(err error) { fmt.Fprintln(os.Stderr, "splunk:", err) }
	}
	id := fmt.Sprintf("%s-%d", *version, time.Now().Unix())
	e := deploy.Engine{Emit: func(ev deploy.Event) {
		fmt.Printf("%s  %-16s %s %s\n", ev.At.Format("15:04:05.000"), ev.Type, ev.Target, ev.Detail)
		if hec != nil {
			hec.Add(map[string]any{"deployment": id, "type": ev.Type, "target": ev.Target, "version": ev.Version, "detail": ev.Detail})
		}
	}}
	rep := e.Run(ctx, targets, plan)
	if hec != nil {
		hec.Close()
	}
	fmt.Printf("\n%s in %s: %d hosts updated, %d already current, at most %d out at once\n", rep.Status, rep.Duration.Round(time.Second), rep.Updated, rep.Skipped, rep.PeakOut)
	if rep.Reason != "" {
		fmt.Println("reason:", rep.Reason)
	}
	if *report != "" {
		if b, err := json.MarshalIndent(rep, "", "  "); err == nil {
			_ = os.WriteFile(*report, b, 0o644)
		}
	}
	switch rep.Status {
	case deploy.StatusSucceeded:
		return 0
	case deploy.StatusRolledBack:
		return 1
	case deploy.StatusRollbackFailed:
		return 3
	default:
		return 4
	}
}
