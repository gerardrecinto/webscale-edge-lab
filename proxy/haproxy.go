package proxy

import (
	"fmt"
	"strings"
)

func haproxyConfig(c Config, port int, sock string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `global
  stats socket %s mode 600 level admin
  maxconn 4096

defaults
  mode http
  timeout connect 500ms
  timeout client 10s
  timeout server 10s
  timeout queue %dms
  retries %d
  option redispatch

frontend fe
  bind 127.0.0.1:%d
`, sock, queueMs(c), c.Retries, port)
	if c.RateLimit > 0 {
		// One stick-table entry per X-Client value, counting requests over the last second.
		fmt.Fprintf(&b, `  stick-table type string len 64 size 1k expire 10s store http_req_rate(1s)
  http-request track-sc0 req.hdr(x-client)
  http-request deny deny_status 429 if { sc_http_req_rate(0) gt %d }
`, c.RateLimit)
	}
	b.WriteString("  default_backend be\n\nbackend be\n")
	switch c.Balance {
	case "leastconn":
		b.WriteString("  balance leastconn\n")
	case "hash-consistent":
		fmt.Fprintf(&b, "  balance hdr(%s)\n  hash-type consistent\n", c.HashHdr)
	case "hash-modulo":
		fmt.Fprintf(&b, "  balance hdr(%s)\n  hash-type map-based\n", c.HashHdr)
	default:
		b.WriteString("  balance roundrobin\n")
	}
	b.WriteString("  option httpchk\n  http-check send meth GET uri /healthz\n")
	ds := fmt.Sprintf("  default-server inter %dms fall 2 rise 2", c.HCIntervalMs)
	if c.MaxConn > 0 {
		ds += fmt.Sprintf(" maxconn %d", c.MaxConn)
	}
	if c.Passive {
		ds += " observe layer7 error-limit 3 on-error mark-down"
	}
	b.WriteString(ds + "\n")
	for i, a := range c.Backends {
		fmt.Fprintf(&b, "  server %s %s check\n", c.Names[i], a)
	}
	return b.String()
}

func queueMs(c Config) int {
	if c.QueueMs > 0 {
		return c.QueueMs
	}
	return 1000
}
