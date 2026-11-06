// Package proxy starts real HAProxy and Envoy processes with generated
// configuration in front of a set of backends.
package proxy

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Kind string

const (
	HAProxy Kind = "haproxy"
	Envoy   Kind = "envoy"
	Nginx   Kind = "nginx"
	Apache  Kind = "apache2"
)

// Config is the proxy-neutral description of a scenario's setup.
type Config struct {
	Backends     []string // host:port
	Names        []string
	Balance      string // roundrobin | leastconn | hash-consistent | hash-modulo
	HashHdr      string // header used for hash balancing
	MaxConn      int    // per-backend concurrent request cap, 0 = none
	QueueMs      int    // how long a request may wait for a free slot (HAProxy)
	Retries      int
	RateLimit    int // HAProxy only: requests per second per X-Client value, 0 = none
	HCIntervalMs int
	Passive      bool            // HAProxy: mark a server down after consecutive 5xx (observe layer7)
	Down         map[string]bool // Nginx and Apache: backend names to render as down or disabled
}

type Proxy struct {
	Kind Kind
	Addr string // client-facing host:port
	Sock string // HAProxy admin socket
	Dir  string
	Conf string
	cmd  *exec.Cmd
	Log  *os.File
	port int
	cfg  Config
}

func freePort() int {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func Available(k Kind) bool { _, err := exec.LookPath(string(k)); return err == nil }

func (p *Proxy) testConf() (string, error) {
	out, err := exec.Command("nginx", "-t", "-c", p.Conf, "-p", p.Dir).CombinedOutput()
	return string(out), err
}

// Reload applies a changed configuration without dropping requests that are
// already running: SIGHUP for Nginx, a graceful restart for Apache. HAProxy
// changes go through its runtime API instead (see HAProxyCmd).
func (p *Proxy) Reload(c Config) error {
	p.cfg = c
	switch p.Kind {
	case Nginx:
		return p.reloadNginx(c)
	case Apache:
		return p.reloadApache(c)
	}
	return fmt.Errorf("%s has no config reload; use its runtime API", p.Kind)
}

// Config returns the configuration last applied.
func (p *Proxy) Config() Config { return p.cfg }

func Start(k Kind, c Config) (*Proxy, error) {
	if c.HCIntervalMs == 0 {
		c.HCIntervalMs = 300
	}
	if c.Retries == 0 {
		c.Retries = 2
	}
	dir, _ := os.MkdirTemp("", "edge-"+string(k))
	p := &Proxy{Kind: k, Dir: dir, cfg: c}
	port := freePort()
	p.port = port
	p.Addr = fmt.Sprintf("127.0.0.1:%d", port)
	_ = os.Chmod(dir, 0o777) // Apache's workers run as another user
	var args []string
	switch k {
	case HAProxy:
		p.Sock = filepath.Join(dir, "admin.sock")
		p.Conf = filepath.Join(dir, "haproxy.cfg")
		if err := os.WriteFile(p.Conf, []byte(haproxyConfig(c, port, p.Sock)), 0o644); err != nil {
			return nil, err
		}
		args = []string{"-f", p.Conf, "-db"}
	case Nginx:
		p.Conf = filepath.Join(dir, "nginx.conf")
		if err := os.WriteFile(p.Conf, []byte(nginxConfig(c, port, dir)), 0o644); err != nil {
			return nil, err
		}
		args = []string{"-c", p.Conf, "-p", dir}
	case Apache:
		p.Conf = filepath.Join(dir, "apache.conf")
		if err := os.WriteFile(p.Conf, []byte(apacheConfig(c, port, dir)), 0o644); err != nil {
			return nil, err
		}
		args = []string{"-f", p.Conf, "-DFOREGROUND"}
	case Envoy:
		p.Conf = filepath.Join(dir, "envoy.yaml")
		if err := os.WriteFile(p.Conf, []byte(envoyConfig(c, port, freePort())), 0o644); err != nil {
			return nil, err
		}
		args = []string{"-c", p.Conf, "--concurrency", "1", "--log-level", "warn", "--base-id", fmt.Sprint(port)}
	}
	p.Log, _ = os.Create(filepath.Join(dir, "proxy.log"))
	p.cmd = exec.Command(string(k), args...)
	p.cmd.Stdout, p.cmd.Stderr = p.Log, p.Log
	if err := p.cmd.Start(); err != nil {
		return nil, err
	}
	for i := 0; i < 100; i++ {
		if conn, err := net.DialTimeout("tcp", p.Addr, 100*time.Millisecond); err == nil {
			conn.Close()
			return p, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "proxy.log"))
	p.Stop()
	return nil, fmt.Errorf("%s did not start: %s", k, strings.TrimSpace(string(b)))
}

func (p *Proxy) Stop() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_, _ = p.cmd.Process.Wait()
	}
	if p.Log != nil {
		p.Log.Close()
	}
}

func (p *Proxy) URL() string { return "http://" + p.Addr + "/" }

// HAProxyCmd sends a command to the runtime admin socket and returns the reply.
func (p *Proxy) HAProxyCmd(cmd string) (string, error) {
	c, err := net.Dial("unix", p.Sock)
	if err != nil {
		return "", err
	}
	defer c.Close()
	fmt.Fprintf(c, "%s\n", cmd)
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String(), nil
}

// Render returns the configuration Start would write, with placeholder ports,
// for display in docs.
func Render(k Kind, c Config) string {
	if c.HCIntervalMs == 0 {
		c.HCIntervalMs = 300
	}
	if c.Retries == 0 {
		c.Retries = 2
	}
	switch k {
	case HAProxy:
		return haproxyConfig(c, 8080, "/tmp/admin.sock")
	case Nginx:
		return nginxConfig(c, 8080, "/tmp/nginx")
	case Apache:
		return apacheConfig(c, 8080, "/tmp/apache")
	}
	return envoyConfig(c, 8080, 9901)
}
