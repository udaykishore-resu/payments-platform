# RB-002: payment-api latency error-budget burn

- **Severity:** page (`PaymentAPILatencyFastBurn`, P1) · ticket (`PaymentAPILatencySlowBurn`, P2)
- **Alert:** `PaymentAPILatencyFastBurn`
  ```promql
  pp:payment_api_latency_bad:ratio_rate1h > (14.4 * 0.01)
    and pp:payment_api_latency_bad:ratio_rate5m > (14.4 * 0.01)
  ```
  `PaymentAPILatencySlowBurn`
  ```promql
  pp:payment_api_latency_bad:ratio_rate6h > (6 * 0.01)
    and pp:payment_api_latency_bad:ratio_rate30m > (6 * 0.01)
  ```
- **Triggered when:** more than 14.4 % of requests exceed the 250 ms p99 SLO over both 1 h and 5 m,
  for 2 minutes (fast); or more than 6 % over both 6 h and 30 m, for 15 minutes (slow).
- **Plane / service:** data · `payment-api`
- **Related:** `docs/observability.md` §4.2, `docs/failure-handling.md` §2.1 (the timeout cascade)
  and §2.8 (adaptive concurrency), `docs/spec/00-design-baseline.md` §12 (the 18-stage budget)

## What this means

The latency SLO is expressed as a **good-events ratio against the 0.25 s histogram bucket**, not
as a p99 gauge, so that burn-rate arithmetic works: 1 % of requests are allowed to be slower than
250 ms, and `pp:payment_api_latency_bad:ratio_rate*` is the fraction that were.

250 ms is the API's own budget, not the payment's. A payment that reaches a gateway spends up to
8 s there by design; that time is in `pp_gateway_request_duration_seconds`, not here. So this
alert is about the platform's own work — validation, idempotency claim, routing decision, state
write, outbox write — running long. Something in the 18-stage pipeline of baseline §12 is over
budget.

The single most common cause, and the one the alert's own description points at: **a CPU limit on
a latency-sensitive service**. CFS throttling produces exactly this signature — p50 flat, p99
ruined, no errors.

## Impact

Merchants see slow checkouts. Nothing is wrong yet: no payment is lost, no money is at risk, and
the error rate is unaffected. But latency is upstream of everything else — slow responses cause
client timeouts, client timeouts cause retries, retries cause a retry storm
(`docs/failure-handling.md` F-18), and a retry storm is how a latency problem becomes an outage.

Second-order: every slow request holds a connection and a concurrency slot, so the adaptive
limiter starts reducing the in-flight ceiling, which sheds P3/P4 traffic (ladder rungs 2 and 4).

## Immediate triage (first 5 minutes)

1. Size it, and separate p50 from p99 — the gap is the diagnosis:
   ```promql
   pp:payment_api_latency:p99_5m
   pp:payment_api_latency:p50_5m
   pp:payment_api_latency_bad:ratio_rate5m
   ```
2. Check CPU throttling first. Panel 7 of the service-health dashboard, or:
   ```promql
   rate(container_cpu_cfs_throttled_seconds_total{container="payment-api"}[5m])
   ```
   ```bash
   kubectl -n pp-data-plane get deployment payment-api \
     -o jsonpath='{.spec.template.spec.containers[0].resources}{"\n"}'
   ```
3. Which route?
   ```promql
   histogram_quantile(0.99, sum by (le, route)
     (rate(pp_http_request_duration_seconds_bucket{service="payment-api"}[5m])))
   ```
4. Dependencies — database pool wait, Redis, gateways:
   ```promql
   pp_db_pool_in_use / pp_db_pool_max
   redis_up
   pp:gateway_latency:p99_5m
   pp_http_inflight_requests{service="payment-api"}
   ```
5. Volume — is this load, or is it slowness at constant load?
   ```promql
   pp:payment_api_requests:rate5m
   pp:payments:tps5m
   ```

## Diagnosis

- **`container_cpu_cfs_throttled_seconds_total` is non-zero** → someone added or lowered a CPU
  limit. p50 flat, p99 ruined is the signature. → *M1*.
- **p50 rose with p99** → the whole service is slower, not a tail. Look at load
  (`pp:payment_api_requests:rate5m`) and at the deploy history. → *M2* or *M4*.
- **p50 flat, p99 up, `pp_db_pool_in_use / pp_db_pool_max` near 1** → requests are queuing for a
  connection. → [db-pool-exhaustion.md](db-pool-exhaustion.md), then *M3*.
- **`redis_up == 0`** → the idempotency read is falling back to Postgres. Expect +15–30 ms on p99,
  which alone does not breach 250 ms; if it does, Redis is masking a second problem.
  → [redis-loss.md](redis-loss.md).
- **One route dominates and it is a list/report route** → P3/P4 work, which the ladder is meant to
  shed before it hurts the money path. → *M5*.
- **Latency rose at a deploy boundary** → *M4*.
- **`pp:gateway_latency:p99_5m` is high** → the API is waiting on the orchestrator waiting on a
  gateway. This is not an API problem. → [gateway-degradation.md](gateway-degradation.md).

## Mitigation

**M1 — remove the CPU limit (keep the request).** A CPU *limit* on a latency-sensitive service
buys nothing and costs the tail; the *request* is what schedules it.
```bash
kubectl -n pp-data-plane patch deployment payment-api --type=json \
  -p='[{"op":"remove","path":"/spec/template/spec/containers/0/resources/limits/cpu"}]'
kubectl -n pp-data-plane rollout status deployment/payment-api --timeout=5m
```
Expected: throttling goes to zero and p99 falls within one rollout. Land the same change in Git in
the same hour or the next sync reverts it.

**M2 — scale out.** Horizontal, not vertical:
```bash
kubectl -n pp-data-plane scale deployment/payment-api --replicas=<current+50%>
```
Expected: p99 falls proportionally if the bottleneck was CPU or in-flight ceiling. If p99 does not
move, the bottleneck is downstream — stop scaling, you are adding pool connections for nothing.

**M3 — raise the pool ceiling, carefully.** Only with headroom on the writer's `max_connections`:
`PP_DATABASE_MAX_CONNS` per pod × replicas must stay under it with room for migrations and a human
with `psql`. See [db-pool-exhaustion.md](db-pool-exhaustion.md) for the arithmetic before touching it.

**M4 — roll back.**
```bash
kubectl -n pp-data-plane rollout undo deployment/payment-api
```

**M5 — shed the expensive routes.** Lower the limiter ceiling so the shedder drops P3/P4 first:
```bash
kubectl -n pp-data-plane set env deployment/payment-api PP_CONCURRENCY_MAX_LIMIT=128
```
Expected: reports and list endpoints get 503 + `Retry-After`; single-payment reads and all writes
continue (rungs 2 and 4).

## Rollback / escalation

- **15 minutes with no improvement** → escalate to the data-plane owner; Sev-2.
- **The error-rate burn alert starts too** → latency has become unavailability. Switch to
  [payment-api-availability.md](payment-api-availability.md), which takes precedence.
- **`pp_http_inflight_requests` climbing while throughput is flat** → you are watching a queue
  form. Do not wait; shed (*M5*) now, before the retry storm starts.
- **Any mitigation that involves increasing a timeout: stop.** The timeout cascade in
  `docs/failure-handling.md` §2.1 is arithmetic, not preference. Raising one number breaks the
  invariant that a caller's budget exceeds its callee's, and the failure mode is orphaned work on
  a money path.

## Verification

```promql
pp:payment_api_latency:p99_5m           < 0.25
pp:payment_api_latency_bad:ratio_rate5m < 0.01
rate(container_cpu_cfs_throttled_seconds_total{container="payment-api"}[5m]) == 0
```
Both burn alerts clear on their own. Check that `pp_http_inflight_requests` has returned to its
baseline — a p99 that recovered because traffic was shed is not a p99 that recovered.

## Follow-up

- Record which stage of the §12 pipeline consumed the budget, from a trace, not from a guess.
- If it was a CPU limit: find who added it and why, and put the reasoning in the chart rather than
  removing it again next quarter.
- If it was pool contention: revisit `PP_DATABASE_MAX_CONNS` × replicas against the writer class,
  and write the number down in `docs/deployment.md`.
- Add a load-test scenario that reproduces it: `scripts/loadtest.sh <scenario> --base … --token …`.
