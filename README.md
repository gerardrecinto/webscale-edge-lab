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

## Regional failover drill (lab model, not production)

A second part of the lab models active-passive failover between two regions, as loopback HTTP servers in one process. Replication is async and lagged on purpose, so the data loss is real and measurable.

What is in `failover/`:

- **Fencing epoch + lease.** Each failover takes a new epoch from a fence store. The old primary only accepts writes while the orchestrator keeps renewing its lease, so if the fence call cannot reach it, the orchestrator waits out the lease before promoting. Writes with a stale epoch get 409.
- **Quorum of probers.** 3 probers, each needs 3 consecutive failures, 2 of 3 must agree. One blind prober does not trigger a failover (tested).
- **Replication lag gate (RPO).** The replica must be within `MaxLagOps` writes of the highest sequence any prober saw, otherwise failover is refused until an operator sets force.
- **Client circuit breaker.** Opens after 3 consecutive failures, half-open probe after 100 ms.
- **Chaos.** Per-region injected delay, 503 rate and dropped traffic from a named caller (used to partition the orchestrator from a primary the clients can still reach).

Run it (`go run ./cmd/failover-drill`, about 12 s, output from my laptop):

```
== crash
   +   119ms quorum says primary down
   +   120ms epoch 2 issued
   +   120ms fence unreachable, waiting 199ms for the lease to lapse
   +   320ms promoted us-east-1 at epoch 2
   writes acked 240, client errors 65, breaker rejected calls: true
   detect 119ms  RTO 330ms  lost writes 9 (RPO window 39ms)  stale writes accepted after promote 0  lag gate hit: false

== partition
   +   120ms quorum says primary down
   +   120ms epoch 2 issued
   +   220ms fence unreachable, waiting 61ms for the lease to lapse
   +   282ms promoted us-east-1 at epoch 2
   writes acked 293, client errors 23, breaker rejected calls: true
   detect 120ms  RTO 380ms  lost writes 0 (RPO window 0ms)  stale writes accepted after promote 0  lag gate hit: false

== laggate
   +    80ms quorum says primary down
   +    80ms failover refused: replica 151 writes behind, budget 20
   +   600ms epoch 2 issued
   +   600ms fence unreachable, lease already lapsed
   +   601ms promoted us-east-1 at epoch 2
   writes acked 241, client errors 128, breaker rejected calls: true
   detect 80ms  RTO 639ms  lost writes 159 (RPO window 794ms)  stale writes accepted after promote 0  lag gate hit: true
```

How to read it: RTO is measured from the fault to the first write acknowledged by the new primary. In the crash run, about 200 ms of the 330 ms is the lease wait, which is the price of not being able to tell a dead primary from an unreachable one. The lost-write window in the crash run (39 ms) matches the 40 ms replication delay I configured, and the lag gate run lost 159 writes because the replica was 1.5 s behind and I forced the failover. Numbers shift a little between runs.

Tests: `go test -race ./failover` covers the breaker states, stale-epoch and expired-lease rejection, the quorum, the gate (refuses, then force overrides) and the crash and partition drills.

What I ran into:

- **A slow lease renewal delayed failure detection.** My first version renewed the lease in the same loop as the probes. In the partition run the renewal hung for its whole 100 ms timeout on every tick, and detection took 341 ms instead of about 120 ms. Renewing in its own goroutine fixed it.
- **The lag figure is a lower bound.** Once the primary is dead nobody can ask it for its sequence, so the gate uses the highest sequence a prober saw before the fault. Writes acknowledged between the last probe and the crash are not counted, so the real loss can be higher than the gate believed.
- **Fencing needs a path to the old primary or a lease.** Fencing by epoch only works if the old primary learns the epoch. The lease is what covers the case where it cannot be reached, and it costs RTO.

Not covered: a real fence store outside both regions (it is a mutex here), failback, more than two regions, real network behavior, and real cross-region replication services. The reason a prober's lag number can be stale is exactly that they are all in one process.

## Layout

- `backend/` an HTTP service you can crash, restart, break (503s), slow, mark unhealthy, or shut down gracefully
- `proxy/` generates configuration and starts real HAProxy or Envoy processes; `Render` returns the config for the docs
- `load/` constant-rate load generator and summaries
- `scenarios/` the situations above, shared by the tests and the recorder
- `cmd/record/` runs everything and writes `docs/data.json`
- `failover/` the regional failover model: regions, replication, fencing, orchestrator, breaker, drill
- `cmd/failover-drill/` runs the failover drills and prints measured RTO and RPO

## Not covered

Virtual IPs and failover between load balancers (keepalived, VRRP), BGP and ECMP, anycast, L4 balancing and Maglev-style hashing, TLS termination, HTTP/2 and gRPC, multiple hosts, real network latency, and any throughput benchmark.

## References

- HAProxy: [configuration manual](https://docs.haproxy.org/3.0/configuration.html), [management guide](https://docs.haproxy.org/3.0/management.html)
- Envoy: [health checking](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/health_checking), [outlier detection](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/outlier), [circuit breaking](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/circuit_breaking), [load balancers](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/load_balancing/load_balancers)
- Google SRE Book: [Load Balancing at the Frontend](https://sre.google/sre-book/load-balancing-frontend/), [Load Balancing in the Datacenter](https://sre.google/sre-book/load-balancing-datacenter/)
- Eisenbud et al., [Maglev: A Fast and Reliable Software Network Load Balancer](https://research.google/pubs/maglev-a-fast-and-reliable-software-network-load-balancer/)
