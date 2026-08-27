# RB-016: Aurora writer failover (database primary failover)

- **Severity:** page (P1)
- **Alert:** `AuroraFailoverDetected`
  ```promql
  changes(pg_writer_instance_changed_total[10m]) > 0
  ```
- **Triggered when:** the Aurora writer instance changed at all, `for: 0m` — this fires on the
  event, not on a threshold.
- **Plane / service:** data · `aurora`
- **Related:** `docs/failure-handling.md` F-6, `docs/disaster-recovery.md` §2.1 and §4.1,
  `docs/spec/00-design-baseline.md` §18 (RTO) and A4/§15 (the CP choice),
  [db-pool-exhaustion.md](db-pool-exhaustion.md), [dr-replication-lag.md](dr-replication-lag.md)

## What this means

The Aurora cluster promoted a replica to writer. This is **in-region** failover — nothing to do
with cross-region promotion, which is manual and covered by
[region-failover.md](region-failover.md). It completes in ≤ 60 s.

The platform's designed behaviour during it:

- **Readiness fails within 5 s**; three consecutive failures (15 s) removes the pod from the load
  balancer, so traffic is shed rather than erroring.
- **Liveness holds.** Pods stay warm and pools stay connected. This is the important part: a
  liveness probe that also failed would restart every pod in the fleet during a 60-second event,
  turning a brief write outage into a cold-start stampede against a brand-new writer.
- **Writes reject `503 SERVICE_UNAVAILABLE`**, retryable. A payment that cannot reach the primary
  **fails closed** rather than being processed twice (baseline A4 — the CP choice).
- **Reads shift to the replica** with ≤ 1 s staleness.
- **Pools drain and re-establish with exponential backoff and jitter**, to avoid a thundering herd
  against the new primary.

So the alert is largely informational — *unless pods restarted*. If they did, a liveness probe has
grown a downstream dependency, and **that** is the real incident.

## Impact

- Up to ~60 s of write rejections with a retryable 503. Merchants' create/capture/refund calls fail
  and their SDKs retry; the sale is deferred, not lost.
- Reads continue with ≤ 1 s staleness.
- Ladder rung 9 (read-only) is the shape of the degradation while failover is in progress.
- **No money at risk by construction**: idempotency records and outbox rows share the state
  transaction, so there is nothing written-but-uncommitted to recover. In-flight gateway calls at
  the moment of failover may land as `TIMEOUT_UNKNOWN` — that is the reconciler's job, not a manual
  one ([timeout-unknown.md](timeout-unknown.md)).

## Immediate triage (first 5 minutes)

1. Confirm the failover completed and where the writer now is:
   ```bash
   aws rds describe-db-clusters --db-cluster-identifier pp-prod \
     --query 'DBClusters[0].DBClusterMembers[?IsClusterWriter==`true`].DBInstanceIdentifier'
   aws rds describe-events --source-type db-cluster --duration 30 \
     --source-identifier pp-prod --query 'Events[].{t:Date,m:Message}'
   ```
   ```sql
   SELECT pg_is_in_recovery();      -- must be f on the writer endpoint
   SELECT inet_server_addr(), current_setting('server_version');
   ```
2. **Did pods restart?** This is the question that decides whether this is an incident:
   ```bash
   kubectl -n pp-data-plane get pods -l app=payment-api \
     -o custom-columns='NAME:.metadata.name,RESTARTS:.status.containerStatuses[0].restartCount,AGE:.metadata.creationTimestamp'
   kubectl -n pp-data-plane get events --sort-by=.lastTimestamp | grep -iE 'liveness|killing' | tail -20
   ```
3. Are we recovering?
   ```promql
   sum by (status) (rate(pp_http_requests_total{route="/v1/payments"}[5m]))
   pp_db_pool_in_use / pp_db_pool_max
   pp:payment_api_error:ratio_rate5m
   ```
4. Replica lag after the promotion:
   ```promql
   aws_rds_aurora_global_db_replication_lag
   ```
5. Any payments caught mid-flight:
   ```promql
   sum(rate(pp_payments_total{outcome="timeout_unknown"}[10m]))
   pp_attempts_unresolved
   ```

## Diagnosis

- **Failover completed, no pod restarts, errors already falling** → the design worked. → *M1*
  (verify and stand down).
- **Pods restarted on liveness** → **the real incident.** A liveness probe is checking a downstream
  dependency, which is a probe-design bug: liveness answers "is this process wedged", readiness
  answers "can it serve right now". → *M2*.
- **Errors persisting past ~2 minutes** → pools have not re-established. → *M3*.
- **Repeated failovers (`changes(...) > 1`)** → the cluster is unstable; this is an AWS-side
  problem. → *M4*.
- **`pp_db_pool_in_use / pp_db_pool_max` pinned at 1 after recovery** → a thundering herd against
  the new writer. → [db-pool-exhaustion.md](db-pool-exhaustion.md), *M3*.
- **`aws_rds_aurora_global_db_replication_lag` high after promotion** → the new writer is catching
  the secondary up; RPO is temporarily at risk. → [dr-replication-lag.md](dr-replication-lag.md).
- **`TimeoutUnknownSpike` fires alongside** → in-flight gateway calls were caught. Expected in small
  numbers. → [timeout-unknown.md](timeout-unknown.md).

## Mitigation

**M1 — verify and stand down.** For a clean failover the correct action is to confirm recovery and
record it. Write in the channel: writer identity before and after, duration of 503s, peak error
rate, pod restart count (zero), and the count of `TIMEOUT_UNKNOWN` attempts created.

**M2 — fix the liveness probe.** Immediately, and it is the highest-value action in this runbook:
```bash
kubectl -n pp-data-plane get deployment payment-api \
  -o jsonpath='{.spec.template.spec.containers[0].livenessProbe}{"\n"}'
```
Liveness must be `/livez` — a pure process check with no downstream dependency. Readiness is
`/readyz` and may depend on the database. If liveness points at `/readyz` or `/healthz`, that is
the defect. Fix it in Git and roll:
```bash
kubectl -n pp-data-plane rollout status deployment/payment-api --timeout=5m
```

**M3 — help the pools re-establish.** First, wait: backoff with jitter is doing the right thing and
a restart replaces jittered reconnection with a synchronised stampede. If pools are genuinely
wedged after 5 minutes, restart **one deployment at a time**:
```bash
kubectl -n pp-data-plane rollout restart deployment/payment-api
kubectl -n pp-data-plane rollout status deployment/payment-api --timeout=5m
# only then the next one
```
Check the connection arithmetic before restarting everything at once — `PP_DATABASE_MAX_CONNS` ×
replicas × deployments must stay under the writer's `max_connections`
([db-pool-exhaustion.md](db-pool-exhaustion.md)).

**M4 — AWS escalation on repeated failovers.** Include the cluster identifier, the event list from
step 1, and the timestamps. Do not attempt a manual failover to "settle" it — you would be adding a
third writer change to an unstable cluster.

## Rollback / escalation

- **This alert pages, and the most common correct outcome is "verified, stood down".** Say so
  explicitly rather than leaving it open; a page that is habitually left open trains the rotation
  to ignore it.
- **Pods restarted** → Sev-2, and the probe fix ships the same day. The next failover will be worse
  otherwise.
- **Write errors past 5 minutes** → Sev-1: this is no longer a failover, it is a database outage.
  Escalate to the data-platform owner and open an AWS case.
- **Do not fail payments that were in flight.** They are `TIMEOUT_UNKNOWN` or they are rolled back
  by the database; there is no third outcome. [timeout-unknown.md](timeout-unknown.md).
- **Do not raise the connection pool size to "recover faster".** More connections against a
  freshly promoted writer is the herd the backoff exists to prevent.
- **If a cross-region failover is being contemplated because of this: it is not warranted.** This
  is in-region, automatic, ≤ 60 s. Region promotion is a human decision with a 15-minute RTO and a
  5-second RPO, and it is a strictly worse trade for a completed in-region failover.

## Verification

```promql
changes(pg_writer_instance_changed_total[10m]) == 0
sum(rate(pp_http_requests_total{route="/v1/payments",status="5xx"}[5m])) == 0
pp:payment_api_error:ratio_rate5m < 0.0001
pp_db_pool_in_use / pp_db_pool_max < 0.7
aws_rds_aurora_global_db_replication_lag < 5
```
```sql
SELECT pg_is_in_recovery();     -- f on the writer endpoint
```
Then confirm nothing was lost or double-applied:
```sql
SET LOCAL app.tenant_id = 'ten_…';
-- I3: at most one successful attempt per payment.
SELECT payment_id, count(*) FROM pp.payment_attempts
WHERE  outcome = 'SUCCESS' GROUP BY payment_id HAVING count(*) > 1;
-- Nothing stranded in a non-terminal state from the window.
SELECT state, count(*) FROM pp.payments
WHERE  created_at > now() - interval '1 hour' GROUP BY state;
```
The first query returns zero rows. And check the outbox drained rather than stalling on the
reconnect: `./bin/platformctl outbox status`.

## Follow-up

- Record: writer before and after, failover duration, 503 count, pod restarts, `TIMEOUT_UNKNOWN`
  attempts created and how each resolved.
- If a liveness probe was implicated, audit **every** deployment's probes in the same pass. This
  defect is copy-pasted between services more often than it is invented twice.
- If it is not covered, add the chaos case:
  `tests/chaos/infra_test.go::TestDatabaseUnavailableMidTransactionFailsClosed` removes the writer
  mid-burst and asserts every payment reaches exactly one terminal state, I3 holds, and idempotent
  replays return the stored result.
- Ask why the failover happened. An unexplained failover is an unexamined AWS-side fault, and the
  event log from step 1 usually says.
