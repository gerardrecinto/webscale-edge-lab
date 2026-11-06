package live

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// JavaApp runs the java-app service as a real JVM process. Stop sends SIGTERM
// and waits for the JVM's shutdown hook to drain and exit, as an init system
// or a Kubernetes pod would.
type JavaApp struct {
	Cmd  []string // e.g. {"java", "-jar", "shop-api.jar"}
	ID   string
	Port int

	proc   *exec.Cmd
	exited chan struct{}
}

func (a *JavaApp) Addr() string { return "127.0.0.1:" + strconv.Itoa(a.Port) }

func (a *JavaApp) Start(ctx context.Context, version string) error {
	args := append(append([]string(nil), a.Cmd[1:]...), "--port", strconv.Itoa(a.Port), "--id", a.ID, "--version", version, "--drain-ms", "10000")
	c := exec.Command(a.Cmd[0], args...)
	if os.Getenv("PUSHBUTTON_DEBUG") != "" {
		c.Stdout, c.Stderr = os.Stderr, os.Stderr
	}
	if err := c.Start(); err != nil {
		return err
	}
	a.proc = c
	a.exited = make(chan struct{})
	go func(c *exec.Cmd, done chan struct{}) { _ = c.Wait(); close(done) }(c, a.exited)
	return nil
}

func (a *JavaApp) Stop(ctx context.Context) error {
	if a.proc == nil {
		return nil
	}
	if err := a.proc.Process.Signal(syscall.SIGTERM); err != nil {
		return nil // already gone
	}
	select {
	case <-a.exited:
		return nil
	case <-time.After(15 * time.Second):
		_ = a.proc.Process.Kill()
		<-a.exited
		return fmt.Errorf("the JVM did not exit within 15s of SIGTERM and was killed")
	case <-ctx.Done():
		_ = a.proc.Process.Kill()
		<-a.exited
		return ctx.Err()
	}
}
