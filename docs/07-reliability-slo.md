# 07 — Reliability: SLI / SLO / SLA, Error Budgets, Testing

## SLIs (Service Level Indicators — what we measure)

| SLI | Definition |
|---|---|
| Availability | Proportion of requests that receive a valid HTTP response (any status the client should treat as "the service answered", i.e. not 5xx/timeout) within 5s |
| Latency | Proportion of requests completing within target P95/P99 thresholds |
| Correctness | Proportion of payments with balanced ledger entries (target: 100.000%, i.e. this SLI existing at all is a canary, not a tolerance) |
| Durability | Proportion of committed payments with a correspondingly published (eventually) outbox event within RPO window |

## SLOs (Service Level Objectives — our internal target)

| SLI | SLO | Window |
|---|---|---|
| Availability | 99.95% | Rolling 30 days |
| Latency P95 | < 250ms | Rolling 30 days |
| Latency P99 | < 600ms | Rolling 30 days |
| Correctness | 100% (any deviation is a SEV-1, not a budget line item) | Continuous |
| Durability (event delivery within 5 min of commit) | 99.9% | Rolling 30 days |

## SLA (external commitment — typically a subset/looser version of the SLO, with a business remedy)

99.9% monthly availability commitment to merchant clients, with service credits for breach. The
internal SLO (99.95%) is intentionally tighter than the external SLA so engineering has a buffer
to detect and respond to degradation before it becomes a contractual breach.

## Error Budget

- 99.95% monthly availability SLO → **21.9 minutes/month** allowed downtime-equivalent budget.
- Error budget policy: when > 50% of the monthly budget is consumed, feature deploys require
  extra sign-off (a second reviewer + explicit risk acknowledgment) and rollout is more
  conservative (smaller canary %, longer bake time). When the budget is fully exhausted, only
  reliability/bug-fix work is deployed until the budget resets — this is the mechanism that
  actually gives "reliability" teeth against feature-velocity pressure, not just a metric on a
  dashboard.
- **Multi-window, multi-burn-rate alerting** (the SRE-book-recommended pattern, not a naive
  "error rate > X% for 5 min" rule, which is both slow to catch fast burns and noisy on brief
  blips):
  - Fast burn: 5m and 1h windows, burn rate implying budget exhaustion in < 2 hours → page
    immediately (SEV-1/2).
  - Slow burn: 6h and 3d windows, burn rate implying budget exhaustion before month-end at current
    rate → ticket, reviewed same business day (SEV-3).

## Chaos Engineering & Failure Injection

Run in a dedicated chaos environment first, then periodically (quarterly minimum) in production
during a announced game-day window with on-call actively watching, per experiment:

| Experiment | Validates |
|---|---|
| Kill a random `payments-api` pod under load | Zero-downtime self-healing, no dropped in-flight requests beyond graceful-shutdown grace period |
| Kill the Aurora writer | Automated failover completes within RTO, app circuit breaker sheds load gracefully during the gap instead of queuing/crashing |
| Inject latency on SQS calls | Outbox relay backs off correctly, doesn't exhaust connections, backlog metric fires as expected |
| Saturate DB connection pool | Readiness probe correctly takes the pod out of rotation instead of serving slow/failing requests |
| Duplicate-send the same idempotency key at high concurrency | No double ledger entry under real concurrent load, not just single-threaded test |
| Simulate full AZ network partition | Traffic correctly avoids the partitioned AZ, no split-brain, no data loss |
| Simulate clock skew on one node | No ordering-correctness regression (validates the "never trust wall-clock for ordering" design decision) |

## Load & Stress Testing

- **Load test**: sustained 500 req/s for 30 min against a staging environment sized like
  production, verifying P95/P99 hold and no error-rate regression — gate for every major release.
- **Stress test**: ramp until the system degrades, to find the actual breaking point (not the
  target load) — informs capacity planning and confirms the system fails *gracefully* (fast 503s,
  not cascading timeouts) rather than falling over silently.
- **Soak test**: 500 req/s for 24h+ to catch slow leaks (memory, connection pool exhaustion,
  goroutine leaks) that short tests miss.

## Capacity Planning

- Baseline reserved capacity sized for P90 of historical/expected traffic; HPA burst capacity
  sized to the stress-test-determined breaking point with headroom, reviewed quarterly against
  actual growth trend — avoids both under-provisioning (availability risk) and static
  over-provisioning (cost-optimization NFR).
- Aurora instance class and read replica count reviewed against the same quarterly capacity
  review, informed by connection pool saturation and query latency trends, not guessed.

## Incident Response

- Severity levels SEV-1 (customer-impacting outage / data integrity issue) through SEV-4 (minor,
  no customer impact) with defined page/ticket routing per severity, matching the alert table in
  `06-observability.md`.
- Every SEV-1/2 incident gets a blameless postmortem within 5 business days, with concrete action
  items tracked to completion — the mechanism by which the failure-mode tables in this document
  actually stay current instead of going stale.
