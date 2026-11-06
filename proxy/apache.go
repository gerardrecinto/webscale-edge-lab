package proxy

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const apacheModules = "/usr/lib/apache2/modules"

// apacheConfig renders Apache HTTP Server with mod_proxy_balancer. Members in
// c.Down get status=+D (disabled), so a graceful restart stops sending them
// requests while requests already running finish.
func apacheConfig(c Config, port int, dir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `ServerRoot "%[1]s"
ServerName localhost
PidFile "%[1]s/apache.pid"
ErrorLog "%[1]s/error.log"
LogLevel warn
Mutex file:%[1]s
DefaultRuntimeDir %[1]s
User www-data
Group www-data
Listen 127.0.0.1:%[2]d
`, dir, port)
	for _, m := range []string{"mpm_event", "authz_core", "unixd", "proxy", "proxy_http", "proxy_balancer", "lbmethod_byrequests", "slotmem_shm", "headers"} {
		if m == "unixd" {
			continue // built in
		}
		fmt.Fprintf(&b, "LoadModule %s_module %s/mod_%s.so\n", m, apacheModules, m)
	}
	b.WriteString("ServerLimit 2\nStartServers 2\nThreadsPerChild 64\nMaxRequestWorkers 128\nMinSpareThreads 25\nMaxSpareThreads 75\n")
	b.WriteString("<Proxy \"balancer://be\">\n")
	for i, a := range c.Backends {
		flag := ""
		if c.Down[c.Names[i]] {
			flag = " status=+D"
		}
		fmt.Fprintf(&b, "  BalancerMember \"http://%s\" retry=1 connectiontimeout=1 timeout=10%s\n", a, flag)
	}
	b.WriteString("  ProxySet lbmethod=byrequests failonstatus=502\n</Proxy>\n")
	b.WriteString("ProxyPass \"/\" \"balancer://be/\"\nProxyPassReverse \"/\" \"balancer://be/\"\n")
	return b.String()
}

func (p *Proxy) reloadApache(c Config) error {
	if err := os.WriteFile(p.Conf, []byte(apacheConfig(c, p.port, p.Dir)), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("apache2", "-f", p.Conf, "-t").CombinedOutput(); err != nil {
		return fmt.Errorf("apache rejected the new config: %s", out)
	}
	if err := p.cmd.Process.Signal(syscall.SIGUSR1); err != nil { // graceful restart
		return err
	}
	// Apache gives no easy "old children are gone" signal. A short pause lets
	// the new generation take over, and the controller then also waits for the
	// instance's own in-flight count to reach zero.
	time.Sleep(400 * time.Millisecond)
	return nil
}
