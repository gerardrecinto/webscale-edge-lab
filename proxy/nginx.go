package proxy

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// nginxConfig renders an open-source Nginx. OSS Nginx has no active health
// checks, so a deploy cannot lean on the balancer to notice a bad instance: the
// controller verifies the instance itself before it goes back in the upstream.
// Servers listed in c.Down are written with the down flag, which a graceful
// reload turns into "no new requests".
func nginxConfig(c Config, port int, dir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `worker_processes 2;
pid %[1]s/nginx.pid;
error_log %[1]s/error.log warn;
daemon off;
worker_shutdown_timeout 5s;
events { worker_connections 1024; }
http {
  access_log off;
  client_body_temp_path %[1]s/body;
  proxy_temp_path %[1]s/proxy;
  fastcgi_temp_path %[1]s/fastcgi;
  uwsgi_temp_path %[1]s/uwsgi;
  scgi_temp_path %[1]s/scgi;
  upstream be {
    keepalive 16;
`, dir)
	for i, a := range c.Backends {
		flag := ""
		if c.Down[c.Names[i]] {
			flag = " down"
		}
		fmt.Fprintf(&b, "    server %s max_fails=2 fail_timeout=2s%s; # %s\n", a, flag, c.Names[i])
	}
	fmt.Fprintf(&b, `  }
  server {
    listen 127.0.0.1:%d;
    location / {
      proxy_pass http://be;
      proxy_http_version 1.1;
      proxy_set_header Connection "";
      proxy_connect_timeout 500ms;
      proxy_next_upstream error timeout;
      proxy_next_upstream_tries %d;
    }
  }
}
`, port, c.Retries+1)
	return b.String()
}

// reloadNginx rewrites the config, sends SIGHUP to the master and waits until
// the old workers have finished their requests and exited. That wait is what
// makes the reload a safe point to stop a drained instance.
func (p *Proxy) reloadNginx(c Config) error {
	if err := os.WriteFile(p.Conf, []byte(nginxConfig(c, p.port, p.Dir)), 0o644); err != nil {
		return err
	}
	if out, err := p.testConf(); err != nil {
		return fmt.Errorf("nginx rejected the new config: %s", out)
	}
	if err := p.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		n, shutting := nginxWorkers(p.cmd.Process.Pid)
		if n == 2 && shutting == 0 {
			return nil
		}
	}
	return fmt.Errorf("nginx workers did not settle after reload")
}

// nginxWorkers counts worker processes under the master and how many say they
// are shutting down, by reading the process titles Nginx sets.
func nginxWorkers(master int) (workers, shutting int) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(master) + "/task/" + strconv.Itoa(master) + "/children")
	if err != nil {
		return 0, 0
	}
	for _, f := range strings.Fields(string(b)) {
		cl, err := os.ReadFile("/proc/" + f + "/cmdline")
		if err != nil {
			continue
		}
		title := string(cl)
		if strings.Contains(title, "worker process") {
			workers++
			if strings.Contains(title, "shutting down") {
				shutting++
			}
		}
	}
	return
}
