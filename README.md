# webscale-edge-lab

The same load-balancing situations run through real HAProxy and Envoy processes in front of small Go backends that can crash, return errors, slow down, fail their health check or be replaced. Clients are real HTTP requests; every number is measured.

**Live demo:** https://gerardrecinto.github.io/webscale-edge-lab/ shows the recorded results side by side: failover, rolling deploy under load, a backend that fails requests but passes health checks, overload shedding, per-client rate limiting and consistent hashing.

Personal learning project: loopback only, one machine, HAProxy 3.4.4 and Envoy 1.38.3 locally (HAProxy from apt and Envoy 1.31 from the official image in CI). No VIPs, no real network, no production traffic.

## Scenarios (`go test ./... -v`)

| Scenario | What is asserted |
|---|---|
| Even spread | 3 backends, round robin, within 10% of even, no errors, on both proxies |
| Backend crash | a crashed backend is routed around with at most 5 client errors (connect retries hide it), and the restarted backend rejoins within 3 s |
| Backend returns 503, health check still 200 | plain HAProxy keeps sending it a third of the traffic; HAProxy with `observe layer7` and Envoy outlier detection both cut the error rate below 25% |
| Failing health check | traffic stops reaching the target within 1.5 s with zero client errors |
| Overload | with a per-backend cap, excess load is shed with 503 and accepted requests keep a bounded p95 |
| Per-client rate limit (HAProxy stick table) | a noisy client is mostly answered 429; a quiet client sees no failures |
| Rolling deploy | all three backends drained, stopped and replaced one at a time under constant load: zero failed requests, on both proxies |
| Consistent hashing | the same user always reaches the same backend; removing a backend moves 0% of unaffected users with consistent hashing and about half with the default map-based hash |

Tests skip a proxy if its binary is not on `PATH`.

## What I ran into

- **Envoy `max_ejection_percent` defaults to 10%.** With three backends that rounds to none: outlier detection counted 33 detected ejections and enforced 0 (`ejections_overflow` went up). The broken backend kept serving 503s to a third of requests until I set the percentage to 50.
- **Envoy's health checks ran far less often than configured.** The admin stats showed 3 successful checks in 4 seconds against a 0.3 s interval. I believe the 60 s default `no_traffic_interval` was applying because the proxy-to-backend connections were kept alive and no new connections were being opened. A crashed backend was never marked unhealthy and stayed in rotation, hidden by connect-failure retries. Setting `no_traffic_interval` to the check interval fixed it (42 checks in the same window). I did not read Envoy's source to confirm the cause, only observed the fix.
- **HAProxy has no passive checking by default.** The same 503-returning backend kept a third of the traffic (33% errors) until `observe layer7 error-limit 3 on-error mark-down` was added (7%). Neither HAProxy nor Envoy reaches zero: after ejection both let the server back and need to see errors again.
- **Overload behaves differently.** HAProxy queues for a free slot and answers 503 after the queue timeout (about 300 ms). Envoy's circuit breaker answers 503 within a few milliseconds and shed far more requests for the same offered load, because I set `max_pending_requests` to 1. The two configurations are not equivalent, so read that row as a contrast in style and not as a ranking.
- **Envoy's ring hash is not perfectly stable.** Removing a backend rebuilds the ring over the remaining hosts, and about 19% of users who were not on the removed backend moved. HAProxy's consistent hash moved none; HAProxy's default map-based hash moved 49%.
- Envoy runs with `--concurrency 1`, because each worker thread balances independently and round robin would otherwise be approximate.

## Layout

- `backend/` an HTTP service you can crash, restart, break (503s), slow, mark unhealthy, or shut down gracefully
- `proxy/` generates configuration and starts real HAProxy or Envoy processes; `Render` returns the config for the docs
- `load/` constant-rate load generator and summaries
- `scenarios/` the situations above, shared by the tests and the recorder
- `cmd/record/` runs everything and writes `docs/data.json`

## Not covered

Virtual IPs and failover between load balancers (keepalived, VRRP), BGP and ECMP, anycast, L4 balancing and Maglev-style hashing, TLS termination, HTTP/2 and gRPC, multiple hosts, real network latency, and any throughput benchmark.

## References

- HAProxy: [configuration manual](https://docs.haproxy.org/3.0/configuration.html), [management guide](https://docs.haproxy.org/3.0/management.html)
- Envoy: [health checking](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/health_checking), [outlier detection](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/outlier), [circuit breaking](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/circuit_breaking), [load balancers](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/load_balancing/load_balancers)
- Google SRE Book: [Load Balancing at the Frontend](https://sre.google/sre-book/load-balancing-frontend/), [Load Balancing in the Datacenter](https://sre.google/sre-book/load-balancing-datacenter/)
- Eisenbud et al., [Maglev: A Fast and Reliable Software Network Load Balancer](https://research.google/pubs/maglev-a-fast-and-reliable-software-network-load-balancer/)
