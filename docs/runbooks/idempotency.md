# RB-013: Idempotency conflicts and in-progress storms

- **Severity:** ticket (`IdempotencyConflictSpike`, P3) · ticket (`IdempotencyInProgressStorm`, P2)
- **Alert:** `IdempotencyConflictSpike`
  ```promql
  sum(rate(pp_idempotency_outcomes_total{outcome="conflict"}[10m])) > 1
  ```
  `IdempotencyInProgressStorm`
  ```promql
  sum(rate(pp_idempotency_outcomes_total{outcome="in_progress"}[5m]))
    / clamp_min(sum(rate(pp_idempotency_outcomes_total[5m])), 1e-9) > 0.2
  ```
- **Triggered when:** more than 1 conflict/second for 10 minutes; or more than 20 % of idempotency
  claims hit an in-progress record for 5 minutes.
- **Plane / service:** data · `payment-api`
- **Related:** `docs/adr/ADR-009-postgres-authoritative-idempotency.md`,
  `docs/spec/00-design-baseline.md` §14.3, `docs/failure-handling.md` F-18 (retry storm),
  [redis-loss.md](redis-loss.md), [payment-api-latency.md](payment-api-latency.md)

## What this means

Every mutating request carries an `Idempotency-Key`. The claim is made in **Postgres**, which is
authoritative; Redis is a latency accelerator in front of it and nothing more (ADR-009). A claim
has three outcomes:

- **`conflict`** — the same key arrived with a *different request body*. That is
  `IDEMPOTENCY_KEY_REUSED`, HTTP 422. It is a **client bug**: their key generation is not unique per
  logical operation. It is not our failure, and the platform is doing exactly the right thing by
  refusing, because the alternative is executing a different operation under a key that already
  names another one.
- **`in_progress`** — the same key, same body, arrived while the first request is still running.
  The platform returns `409 IDEMPOTENT_REQUEST_IN_PROGRESS`, which is the one 4xx marked
  **retryable** in the error model.
- **`replayed`** — the stored response is returned. This is the mechanism working.

So the two alerts point in opposite directions. A conflict spike points **outward**, at a client.
An in-progress storm points **inward**, at us: clients are retrying faster than we are completing,
and the first question is whether *our* latency caused it.

## Impact

- **Conflicts**: those specific requests are rejected with 422. The merchant sees failed API calls
  for operations they believe are new. Nothing is double-charged — that is precisely what the
  rejection prevents.
- **In-progress storm**: clients are queueing behind their own in-flight requests. Retry traffic
  multiplies load, which increases latency, which produces more retries. This is the beginning of
  the retry-storm failure mode (F-18), which is how a small incident becomes an outage.
- **No money at risk in either case.** Both outcomes are the idempotency layer doing its job.

## Immediate triage (first 5 minutes)

1. Which outcome, and how concentrated?
   ```promql
   sum by (outcome) (rate(pp_idempotency_outcomes_total[5m]))
   sum(rate(pp_idempotency_outcomes_total{outcome="in_progress"}[5m]))
     / clamp_min(sum(rate(pp_idempotency_outcomes_total[5m])), 1e-9)
   ```
2. **Is it one tenant?** `tenant_id` is not a metric label by design, so identify from logs:
   ```logql
   {namespace="pp-data-plane", service="payment-api"} | json
     | error_code="IDEMPOTENCY_KEY_REUSED"
     | line_format "{{.tenant_id}} {{.route}} {{.idempotency_key_hash}}"
   ```
3. For an in-progress storm, **check our own latency first**:
   ```promql
   pp:payment_api_latency:p99_5m
   pp:payment_api_latency_bad:ratio_rate5m
   pp:gateway_latency:p99_5m
   pp_http_inflight_requests{service="payment-api"}
   ```
4. Is the request rate up without unique work? That is the definitive retry-storm signature:
   ```promql
   pp:payment_api_requests:rate5m
   sum(rate(pp_idempotency_outcomes_total{outcome="claimed"}[5m]))    # genuinely new work
   ```
   Request rate rising while `claimed` stays flat means retries, not new business.
5. From the system of record:
   ```sql
   SET LOCAL app.tenant_id = 'ten_…';
   SELECT state, count(*), min(created_at) AS oldest,
          count(*) FILTER (WHERE lease_expires_at < now()) AS expired_leases
   FROM   pp.idempotency_records
   WHERE  created_at > now() - interval '1 hour'
   GROUP  BY state;
   ```

## Diagnosis

- **Conflicts, one tenant, constant rate** → their SDK is reusing keys (a timestamp, a hash of a
  field that changes, a retry that regenerates the body). → *M1*.
- **Conflicts, one tenant, started abruptly** → they deployed. → *M1*, with the timestamp.
- **Conflicts across many tenants** → suspect **us**: a change to what goes into the request
  fingerprint would turn identical retries into conflicts. Check the last `payment-api` deploy.
  → *M2*.
- **In-progress storm with our p99 elevated** → we caused it. Fixing us fixes the storm.
  → [payment-api-latency.md](payment-api-latency.md), then *M3*.
- **In-progress storm with our p99 flat** → the client is retrying too aggressively (no jitter, or
  a timeout shorter than our p99). → *M4*.
- **Many `expired_leases` in the query above** → requests are dying mid-flight and leaving claims
  behind; look for pod restarts or OOM kills. → *M5*.
- **`redis_up == 0`** → the claim path fell back to Postgres. Correctness is unaffected; latency is
  up, which can itself provoke a storm. → [redis-loss.md](redis-loss.md).
- **In-progress storm plus rising `TIMEOUT_UNKNOWN`** → clients are retrying payments that are
  genuinely still running against a slow gateway. The 409 is protecting them.
  → [timeout-unknown.md](timeout-unknown.md), [gateway-degradation.md](gateway-degradation.md).

## Mitigation

**M1 — tell the client.** This is the entire mitigation for a conflict spike and it is not a
consolation prize. Give them: the window, the affected route, the count, and what a correct key
looks like — one key per logical operation, generated once and reused verbatim across retries,
never regenerated and never derived from a mutable field. Expected effect: the rate falls when they
ship. Track it as a ticket against their integration.

**M2 — roll back `payment-api`** if the fingerprint changed on our side:
```bash
kubectl -n pp-data-plane rollout undo deployment/payment-api
kubectl -n pp-data-plane rollout status deployment/payment-api --timeout=5m
```
Expected: conflict rate returns to baseline within a rollout. A change to what the request
fingerprint covers is a **contract change**, and it belongs in a versioned release, not a patch.

**M3 — fix our latency.** A storm we induced is fixed by fixing us. See
[payment-api-latency.md](payment-api-latency.md); the in-progress ratio falls as p99 falls.

**M4 — throttle the client, last.** Reduce their quota so their retries are shed cheaply at the
edge rather than expensively at the claim. Do this *after* ruling out our own latency — rate
limiting a client for retrying against our slowness is blaming them for our incident.

**M5 — let the leases expire.** Claim leases expire on their own; that is what `lease_expires_at`
is for. Do **not** delete `pp.idempotency_records` rows to "unstick" a client: the stored response
is what makes a retry safe, and deleting the record turns the next retry into a fresh execution —
which, on `POST /v1/payments`, is a second authorization. If pods are dying mid-request, fix that
([orchestrator-memory.md](orchestrator-memory.md)), not the records.

## Rollback / escalation

- **Never widen the fingerprint to make conflicts disappear.** A conflict means two different
  operations claimed one key. Accepting the second one under the first one's identity is the
  double-charge path, arrived at from a different direction.
- **Never delete idempotency records during an incident.** See *M5*.
- **In-progress ratio above 50 %, or request rate more than 3× baseline with flat unique keys** →
  this is a retry storm; escalate to Sev-2 and expect the adaptive limiter and priority shedder to
  start dropping P3/P4 (`docs/failure-handling.md` §4 rungs 2 and 4). Refunds and voids have
  reserved capacity and continue.
- **Conflicts from a tenant who insists their keys are unique** → get one example key and its two
  bodies from the logs (hashed, never the raw body) and show them the difference. This conversation
  ends quickly with evidence and slowly without it.

## Verification

```promql
sum(rate(pp_idempotency_outcomes_total{outcome="conflict"}[10m])) < 1
sum(rate(pp_idempotency_outcomes_total{outcome="in_progress"}[5m]))
  / clamp_min(sum(rate(pp_idempotency_outcomes_total[5m])), 1e-9) < 0.2
pp:payment_api_latency:p99_5m < 0.25
```
Confirm the request rate and the unique-claim rate have converged again — a storm that ended
because the client gave up is not a storm that was fixed. And confirm no double execution
happened:
```sql
SET LOCAL app.tenant_id = 'ten_…';
-- I3: at most one successful attempt per payment.
SELECT payment_id, count(*) FROM pp.payment_attempts
WHERE  outcome = 'SUCCESS' GROUP BY payment_id HAVING count(*) > 1;
```
Zero rows. This is the invariant the whole mechanism exists to protect.

## Follow-up

- For a client bug: the ticket against their integration, with the evidence, and a follow-up date.
  If it recurs, it is an SDK defect and belongs upstream in the client library.
- For an induced storm: the postmortem question is why our latency rose, not why they retried.
- Check that our own retry guidance is being honoured: every 429 and 503 carries `Retry-After` and
  full-jitter guidance, and a client ignoring it is a documentation or SDK problem.
- If leases were being abandoned, the lease duration and the request deadline are out of step —
  that is arithmetic worth writing down.
