package live

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gerardrecinto/webscale-edge-lab/deploy"
	"github.com/gerardrecinto/webscale-edge-lab/load"
	"github.com/gerardrecinto/webscale-edge-lab/proxy"
)

// javaCommand returns how to start the service: the Gradle-built jar when
// JAVA_APP_JAR is set (CI does this), otherwise a quick javac of the sources.
func javaCommand(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("java"); err != nil {
		t.Skip("java not installed")
	}
	if jar := os.Getenv("JAVA_APP_JAR"); jar != "" {
		return []string{"java", "-jar", jar}
	}
	if _, err := exec.LookPath("javac"); err != nil {
		t.Skip("javac not installed and JAVA_APP_JAR not set")
	}
	out := t.TempDir()
	srcs, _ := filepath.Glob("../../java-app/src/main/java/dev/pushbutton/*.java")
	args := append([]string{"--release", "17", "-d", out}, srcs...)
	if b, err := exec.Command("javac", args...).CombinedOutput(); err != nil {
		t.Fatalf("javac: %v\n%s", err, b)
	}
	return []string{"java", "-cp", out, "dev.pushbutton.Main"}
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// Four real JVMs behind HAProxy are replaced one pair at a time while clients
// keep sending requests: each JVM is drained, sent SIGTERM, finishes its
// requests in its shutdown hook and exits, and the new one must pass its checks
// before it is put back.
func TestRollingDeployOfRealJVMs(t *testing.T) {
	if !proxy.Available(proxy.HAProxy) {
		t.Skip("haproxy not installed")
	}
	cmd := javaCommand(t)
	var apps []*JavaApp
	var cfg proxy.Config
	for i := 0; i < 4; i++ {
		a := &JavaApp{Cmd: cmd, ID: fmt.Sprintf("j%d", i), Port: freePort(t)}
		if err := a.Start(context.Background(), "v1"); err != nil {
			t.Fatal(err)
		}
		apps = append(apps, a)
		cfg.Backends = append(cfg.Backends, a.Addr())
		cfg.Names = append(cfg.Names, a.ID)
	}
	t.Cleanup(func() {
		for _, a := range apps {
			a.Stop(context.Background())
		}
	})
	p, err := proxy.Start(proxy.HAProxy, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	router := NewHAProxyRouter(p)
	var targets []deploy.Target
	for _, a := range apps {
		targets = append(targets, NewAppTarget(a.ID, a, router, "v1"))
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		seen := map[string]bool{}
		for i := 0; i < 12; i++ {
			seen[load.One(p.URL(), nil).Backend] = true
		}
		if len(seen) == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("HAProxy never served from all four JVMs")
		}
		time.Sleep(250 * time.Millisecond)
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	g := load.Start(ctx, load.Options{URL: p.URL(), Rate: 150, Workers: 6})
	time.Sleep(time.Second)
	var e deploy.Engine
	rep := e.Run(context.Background(), targets, deploy.Plan{Version: "v2", MaxUnavailable: 2, StepTimeout: 40 * time.Second})
	time.Sleep(time.Second)
	stop()
	g.Wait()
	total, failed, versions := failures(g)
	t.Logf("4 JVMs: %s in %s, peak out %d, %d requests, %d failed, builds %v", rep.Status, rep.Duration.Round(10*time.Millisecond), rep.PeakOut, total, failed, versions)
	if rep.Status != deploy.StatusSucceeded {
		t.Fatalf("%s: %s", rep.Status, rep.Reason)
	}
	if failed != 0 {
		t.Errorf("%d of %d requests failed while the JVMs were replaced", failed, total)
	}
	if versions["v1"] == 0 || versions["v2"] == 0 {
		t.Errorf("traffic did not span the deploy: %v", versions)
	}
}
