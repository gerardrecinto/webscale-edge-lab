package proxy

import (
	"fmt"
	"strings"
)

func envoyConfig(c Config, port, admin int) string {
	lb := "ROUND_ROBIN"
	switch c.Balance {
	case "leastconn":
		lb = "LEAST_REQUEST"
	case "hash-consistent":
		lb = "RING_HASH"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `admin:
  address: {socket_address: {address: 127.0.0.1, port_value: %d}}
static_resources:
  listeners:
  - name: l
    address: {socket_address: {address: 127.0.0.1, port_value: %d}}
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: ingress
          route_config:
            name: r
            virtual_hosts:
            - name: all
              domains: ["*"]
              routes:
              - match: {prefix: "/"}
                route:
                  cluster: be
`, admin, port)
	if c.Balance == "hash-consistent" {
		fmt.Fprintf(&b, "                  hash_policy:\n                  - header: {header_name: %s}\n", c.HashHdr)
	}
	fmt.Fprintf(&b, `                  retry_policy:
                    retry_on: connect-failure,refused-stream
                    num_retries: %d
          http_filters:
          - name: envoy.filters.http.router
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
  clusters:
  - name: be
    type: STATIC
    connect_timeout: 0.5s
    lb_policy: %s
`, c.Retries, lb)
	if c.MaxConn > 0 {
		n := c.MaxConn * len(c.Backends)
		fmt.Fprintf(&b, `    circuit_breakers:
      thresholds:
      - max_connections: %d
        max_pending_requests: %d
        max_requests: %d
`, n, 1, n)
	}
	fmt.Fprintf(&b, `    health_checks:
    - timeout: 0.3s
      interval: %.1fs
      no_traffic_interval: %.1fs
      unhealthy_threshold: 2
      healthy_threshold: 2
      http_health_check: {path: /healthz}
    outlier_detection:
      consecutive_5xx: 3
      interval: 0.3s
      base_ejection_time: 3s
      max_ejection_percent: 50
    load_assignment:
      cluster_name: be
      endpoints:
      - lb_endpoints:
`, float64(c.HCIntervalMs)/1000, float64(c.HCIntervalMs)/1000)
	for _, a := range c.Backends {
		host, port := splitHostPort(a)
		fmt.Fprintf(&b, "        - endpoint: {address: {socket_address: {address: %s, port_value: %s}}}\n", host, port)
	}
	return b.String()
}

func splitHostPort(a string) (string, string) {
	i := strings.LastIndex(a, ":")
	return a[:i], a[i+1:]
}
