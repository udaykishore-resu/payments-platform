# RB-001: payment-api availability error-budget burn

- **Severity:** page (`PaymentAPIFastBurn`, P1) · ticket (`PaymentAPISlowBurn`, P2)
- **Alert:** `PaymentAPIFastBurn`
  ```promql
  pp:payment_api_error:ratio_rate1h > (14.4 * 0.0001)
    and pp:payment_api_error:ratio_rate5m > (14.4 * 0.0001)
  ```
  `PaymentAPISlowBurn`
  ```promql
  pp:payment_api_error:ratio_rate6h > (6 * 0.0001)
    and pp:payment_api_error:ratio_rate30m > (6 * 0.0001)
  ```
- **Triggered when:** the fast rule needs a 14.4× burn sustained over **both** a 1 h and a 5 m
  window for 2 minutes — 2 % of the 30-day budget already gone, whole budget gone in ~2 days. The
  slow rule needs 6× over **both** 6 h and 30 m for 15 minutes — 5 % gone, ~5 days to exhaustion.
- **Plane / service:** data · `payment-api`
- **Related:** `docs/observability.md` §4.1 (the burn-rate arithmetic), §3.4 (the recording rules),
  `docs/failure-handling.md` §4 (degradation ladder), [error-budget-policy.md](error-budget-policy.md)

## What this means

The SLO is 99.99 % availability over 30 days: a budget of 0.01 % of requests, about 4 m 23 s of
full-outage equivalent. Burning at 1× exhausts it in exactly 30 days.

Both rules are multi-window, multi-burn-rate, and that shape is the whole point. The **long**
window (1 h / 6 h) asserts this is a real, sustained problem rather than a blip. The **short**
window (5 m / 30 m) asserts it is *still happening now*. Requiring both is what stops a
five-minute glitch from paging and what stops an already-recovered incident from paging for
another hour.

What counts as an error is `pp:payment_api_error:ratio_rate*`, computed from
`pp_http_requests_total{status="5xx"}` over total. A **4xx is not an error** — a rejected bad
request is the API working. So this alert is about *us*, not about clients.

## Impact

Merchants are getting 5xx on the money path. Depending on the route: `POST /v1/payments` failing
means sales are not being taken; `POST /v1/payments/{id}/refunds` failing means money cannot be
given back, which is the more expensive of the two (baseline §8, and rung 8 of the degradation
ladder exists to preserve exactly that). Reads failing degrade merchant dashboards.

At 14.4× the budget is gone in about two days, after which the error-budget policy imposes a hard
deploy freeze — so this alert costs engineering time as well as merchant trust.

Degraded, not down: idempotency means a client that retries a failed create is safe, and every
5xx the platform emits is marked `retryable: true` in its problem document, so well-behaved SDKs
are already retrying.

## Immediate triage (first 5 minutes)

1. Confirm it is still burning right now, and get the size:
   ```promql
   pp:payment_api_error:ratio_rate5m
   pp:error_budget_remaining:payment_api
   ```
2. Find out *which* route and which status:
   ```promql
   sum by (route, status) (rate(pp_http_requests_total{service="payment-api",status="5xx"}[5m]))
   topk(5, sum by (route) (rate(pp_http_requests_total{service="payment-api",status="5xx"}[5m])))
   ```
3. Is this one root cause with a page of its own? Check the inhibitors before going further:
   ```promql
   count(pp_circuit_breaker_state{operation="authorize"} == 2)
   pp_config_snapshot_age_seconds
   changes(pg_writer_instance_changed_total[10m])
   redis_up
   ```
4. Correlate with the last deploy. This is the highest-prior-probability cause:
   ```bash
   kubectl -n pp-data-plane rollout history deployment/payment-api
   kubectl -n pp-data-plane get pods -l app=payment-api -o wide
   ```
5. Pull one failing request end to end. The exemplar → trace → logs path in
   `docs/observability.md` §1.5 takes under two minutes:
   ```logql
   {namespace="pp-data-plane", service="payment-api"} | json | status >= 500
     | line_format "{{.ts}} {{.route}} {{.status}} {{.error_code}} {{.trace_id}}"
   ```

## Diagnosis

- **A deploy landed within the burn window** → the deploy is the suspect until proven otherwise.
  → *M1: roll back*.
- **`AllGatewaysUnhealthy` or `NoEligibleGatewayErrors` is also firing** → this alert is a
  symptom; the 5xx are `NO_ELIGIBLE_GATEWAY`. → [no-eligible-gateway.md](no-eligible-gateway.md).
- **`ConfigSnapshotCliff` is firing** → new-merchant payments are being refused by design.
  → [config-staleness.md](config-staleness.md).
- **`AuroraFailoverDetected` is firing** → the 5xx are the ~60 s of write rejection during
  failover, and they should already be recovering. → [aurora-failover.md](aurora-failover.md).
- **Errors concentrated on one route with one `error_code`** → an application defect on that
  path. → *M1* if it is new, *M2* if it is not.
- **Errors spread across every route, `DEPENDENCY_FAILURE` / pool timeouts in the logs** →
  a store, not the API. → [db-pool-exhaustion.md](db-pool-exhaustion.md), [redis-loss.md](redis-loss.md).
- **Errors concentrated in one tenant, and volume for that tenant just jumped** → a retry storm
  induced by that client. → *M3*.
- **Errors on a subset of pods only** → a bad node, a partial rollout, or one pod that lost a
  dependency. → *M4*.

## Mitigation

**M1 — roll back the deploy.** The fastest correct action when a deploy correlates.
```bash
kubectl -n pp-data-plane rollout undo deployment/payment-api
kubectl -n pp-data-plane rollout status deployment/payment-api --timeout=5m
```
Expected: 5xx rate falls to baseline within one rollout (typically 2–4 min). If it does not, the
deploy was not the cause — say so in the channel and go back to Diagnosis.

**M2 — shed load down the ladder.** If the API is saturated rather than broken, the adaptive
limiter is already shedding; forcing a lower ceiling buys headroom for P0/P1 work:
```bash
kubectl -n pp-data-plane set env deployment/payment-api PP_CONCURRENCY_MAX_LIMIT=128
```
Expected: 503 with `Retry-After` on P3/P4 operations, refunds and voids unaffected (rungs 2–6 of
`docs/failure-handling.md` §4). Revert the variable in the same incident; it is not a setting.

**M3 — throttle the offending tenant.** Reduce their quota rather than the fleet's. This is a
control-plane change (`docs/multi-tenancy.md` §5), and it is reversible.

**M4 — remove the bad pods.** `kubectl -n pp-data-plane delete pod <name>` on the subset. If the
whole node is implicated, cordon it; PDB plus topology spread keep the remainder serving
(`docs/failure-handling.md` F-15).

**M5 — scale out.** Only after M2 confirms saturation and only if the database has headroom:
adding `payment-api` replicas multiplies pool connections against one writer, which converts an
API problem into a database problem (see [db-pool-exhaustion.md](db-pool-exhaustion.md)).

## Rollback / escalation

- **15 minutes with no downward movement in `pp:payment_api_error:ratio_rate5m`** → escalate to
  the data-plane service owner and open a Sev-2.
- **`pp:error_budget_remaining:payment_api < 0.10`** → Sev-1, hard deploy freeze, engineering
  manager informed. See [error-budget-policy.md](error-budget-policy.md).
- **Refunds specifically failing** → Sev-1 immediately regardless of burn rate. Money-out is the
  thing the whole degradation ladder is shaped to preserve.
- **Any suspicion the 5xx are hiding a money-state ambiguity** (5xx on `POST /v1/payments` with a
  gateway call already dispatched) → treat as [timeout-unknown.md](timeout-unknown.md) in parallel;
  do not wait for the availability incident to close.

## Verification

```promql
# Must return to the SLO floor and stay there for a full long window.
pp:payment_api_error:ratio_rate5m   < 0.0001
pp:payment_api_error:ratio_rate1h   < 0.0001
pp:payment_api_availability:ratio_rate5m > 0.9999
```
Both burn alerts must clear on their own — do not silence them to close the incident. Confirm
that `pp:error_budget_remaining:payment_api` has stopped falling; it will not recover, because a
spent budget is spent, and that is the point of the policy.

## Follow-up

- Record the peak burn rate, the total budget consumed, and the minutes at each degradation rung.
- File the defect with the trace ID of one failing request attached — a postmortem with a trace is
  a postmortem with an answer.
- If the cause was a deploy, ask why the canary did not catch it (`docs/deployment.md` §5) and fix
  the canary's analysis window or its query, not just the code.
- Add the regression test. `docs/testing.md` names where: a unit test if the defect was logic, a
  contract test if it was a shape, a chaos test if it was a dependency.
