# RB-009: Kafka consumer lag

- **Severity:** ticket (`ConsumerLagHigh`, P2) · page (`LedgerConsumerLagCritical`, P1)
- **Alert:** `ConsumerLagHigh`
  ```promql
  pp_consumer_lag > 50000
  ```
  `LedgerConsumerLagCritical`
  ```promql
  pp_consumer_lag{group="ledger"} > 10000
  ```
- **Triggered when:** any consumer group is more than 50 000 messages behind for 10 minutes; or the
  **ledger** group is more than 10 000 behind for 5 minutes.
- **Plane / service:** data · `event-consumer`
- **Related:** `docs/failure-handling.md` F-10 and §5.3, `docs/events.md`,
  `docs/adr/ADR-010-at-least-once-effectively-once.md`, [dlq.md](dlq.md), [outbox.md](outbox.md)

## What this means

Consumers build projections and the ledger from the event stream. Lag means the read side is
behind the write side. KEDA scales consumers on lag, **capped at the partition count** — a
consumer group cannot usefully have more members than partitions, so lag that persists at max
replicas means either a slow handler or one hot partition, not a shortage of capacity.

The ledger threshold is five times lower than everyone else's on purpose: a lagging projection is
a stale dashboard, and a lagging ledger is a financial report that does not reflect payments that
have already settled. Same mechanism, different consequence.

Consumers are **effectively-once**: `(consumer_group, event_id)` dedup in `pp.event_dedup` makes
redelivery harmless. That is what makes several of the mitigations below safe.

## Impact

- **The payment path: unaffected.** Consumers are downstream of the outbox, which is downstream of
  the transaction. Payments continue.
- **`ConsumerLagHigh`**: merchant-facing projections (payment lists, status views) are stale.
  Merchants see payments they made minutes ago missing from a list while `GET /v1/payments/{id}`
  is correct, because the single-resource read goes to the authoritative store.
- **`LedgerConsumerLagCritical`**: balances are wrong. Anything derived from the ledger — reporting,
  payouts, reconciliation inputs — is derived from a stale picture. This is why it pages.
- Nothing is lost. Lag drains.

## Immediate triage (first 5 minutes)

1. Which group, which topic, and is it one partition or all of them?
   ```promql
   pp_consumer_lag
   topk(5, pp_consumer_lag)
   sum by (group) (pp_consumer_lag)
   ```
2. Are the consumers running, and are they at the partition-count ceiling?
   ```bash
   kubectl -n pp-data-plane get pods -l app=event-consumer
   kubectl -n pp-data-plane get scaledobject,hpa -l app=event-consumer
   kubectl -n pp-data-plane logs deploy/event-consumer --since=10m | tail -60
   ```
3. Is this consumption being slow, or production being fast?
   ```promql
   rate(pp_outbox_published_total[5m])
   pp:payments:tps5m
   deriv(pp_consumer_lag[10m])
   ```
4. Poison messages piling up in parallel?
   ```promql
   pp_dlq_depth
   ```
5. Is the handler's own dependency slow?
   ```promql
   pp_db_pool_in_use / pp_db_pool_max
   pp:payment_api_latency:p99_5m
   ```
6. Confirm the dedup table has not been truncated — replay safety depends entirely on it:
   ```sql
   SELECT count(*) AS dedup_rows, min(processed_at) AS oldest FROM pp.event_dedup;
   ```

## Diagnosis

- **Lag on one partition only, others at zero** → a hot partition. Either one aggregate is
  producing everything, or a poison message is being retried in place. → *M1*, and check
  [dlq.md](dlq.md).
- **Lag across all partitions, replicas below the partition count** → KEDA is still scaling. Give
  it a cycle. → *M2* if it does not.
- **Lag across all partitions, replicas at the partition count** → the handler is slow. Adding pods
  is impossible and would not help. → *M3*.
- **Lag started immediately after a large outbox drain** → this is the outbox's backlog arriving.
  Expected, and it drains. → *M2*, and note it in the channel so nobody escalates twice.
- **Consumers `CrashLoopBackOff`** → configuration or a panic. `event-consumer` requires
  `PP_CONSUMER_GROUP` and `PP_CONSUMER_TOPICS` with no defaults. → *M4*.
- **`pp_dlq_depth` rising at the same rate as lag** → messages are failing, not merely queuing.
  → [dlq.md](dlq.md) takes precedence.
- **`pp_db_pool_in_use / pp_db_pool_max` near 1 on the consumer's pool** → the handler is waiting
  for connections. → [db-pool-exhaustion.md](db-pool-exhaustion.md).
- **`kafka_cluster_partition_underreplicated > 0`** → broker trouble; lag is a symptom.
  → [kafka.md](kafka.md).

## Mitigation

**M1 — deal with the hot partition.** Confirm it first:
```promql
pp_consumer_lag{group="$g"}   # by partition label; one non-zero series is the signature
```
If a poison message is blocking, it should already have been parked — a poison message must never
block a partition (`docs/failure-handling.md` §6.4). If it is blocking, that is the bug, and the
immediate action is a consumer restart to force a rebalance:
```bash
kubectl -n pp-data-plane rollout restart deployment/event-consumer
```
Expected: the partition starts advancing, or the message lands in the DLQ within one retry ladder.

**M2 — wait, with a deadline.** For a drain after a burst, the correct action is to watch
`deriv(pp_consumer_lag[10m])` be negative and report the projected catch-up time. Set a 30-minute
deadline; if lag is not falling by then, go to *M3*.

**M3 — increase handler throughput.** `PP_WORKER_CONCURRENCY` and `PP_WORKER_BATCH_SIZE` control
how much one consumer does at once:
```bash
kubectl -n pp-data-plane set env deployment/event-consumer \
  PP_WORKER_CONCURRENCY=8 PP_WORKER_BATCH_SIZE=200
kubectl -n pp-data-plane rollout status deployment/event-consumer --timeout=5m
```
Expected: lag begins falling within a minute. Cost: a longer unit of work for shutdown to wait for,
and more database connections per pod — check the pool arithmetic first
([db-pool-exhaustion.md](db-pool-exhaustion.md)). Revert after the incident or land it in Git.

**M4 — fix the consumer configuration.** The startup failure names every missing variable at once:
```bash
kubectl -n pp-data-plane logs deploy/event-consumer --tail=40
```
Two variables have no defaults on purpose: a defaulted `PP_CONSUMER_GROUP` means two deployments
silently share a group and each sees half the partitions; empty `PP_CONSUMER_TOPICS` means a
process that reports healthy and consumes nothing.

**M5 — add partitions.** The real fix when the partition count is the ceiling, but it is a
*planned* change, not an incident action: adding partitions changes key→partition mapping and can
reorder events for an aggregate mid-stream. Do it in a change window, not at 03:00.

## Rollback / escalation

- **Ledger lag is a page.** Escalate to the payments product owner and the finance contact if it
  exceeds 30 minutes: reports are being produced from a picture that is known to be wrong.
- **Lag rising for 30 minutes despite mitigation** → Sev-2, bring in the events/platform owner.
- **Never reset offsets to skip past a backlog.** Skipping is not catching up; it is silently
  discarding state transitions. The projection then never becomes correct, and the discrepancy is
  discovered by a merchant.
- **Never truncate `pp.event_dedup`.** Dedup is the only thing that makes redelivery safe. Without
  it, a redelivery is a double application, and on the ledger topic that is double-counted money.
- **If lag is caused by broker trouble, stop scaling consumers.** More consumers against a
  struggling broker is a rebalance storm.

## Verification

```promql
pp_consumer_lag < 1000
pp_consumer_lag{group="ledger"} < 100
deriv(pp_consumer_lag[10m]) <= 0
```
Then verify the projections are actually correct, not merely current:
```sql
-- The ledger must balance per account.
SELECT account_id, sum(amount) AS balance FROM pp.ledger_entries
GROUP  BY account_id HAVING sum(amount) <> 0;

-- Dedup is intact and covering the replayed window.
SELECT count(*) FROM pp.event_dedup WHERE processed_at > now() - interval '1 hour';
```
The first query must return zero rows.

## Follow-up

- Record peak lag per group, time to drain, and whether the partition-count ceiling was reached.
- If it was reached, size the partition count against measured peak throughput and schedule the
  change properly.
- If one partition was hot, the partition key is wrong for that aggregate's traffic pattern. That
  is a design finding, and it belongs in `docs/events.md`.
- If `PP_WORKER_CONCURRENCY` was raised and helped, land it in the chart with the measurement
  attached, rather than leaving an undocumented `kubectl set env` in production.
