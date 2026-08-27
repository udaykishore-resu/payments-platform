# RB-008: Outbox backlog growing / relay stalled

- **Severity:** ticket (`OutboxBacklogGrowing`, P2) · page (`OutboxStalled`, P1)
- **Alert:** `OutboxBacklogGrowing`
  ```promql
  pp_outbox_backlog > 10000 and deriv(pp_outbox_backlog[10m]) > 0
  ```
  `OutboxStalled`
  ```promql
  pp_outbox_backlog > 0 and rate(pp_outbox_published_total[5m]) == 0
  ```
- **Triggered when:** more than 10 000 unpublished rows on a topic **and** the backlog is rising,
  for 10 minutes; or any backlog at all with zero publishes for 5 minutes.
- **Plane / service:** data · `outbox-relay`
- **Related:** `docs/failure-handling.md` F-8, F-9 and §5.3, `docs/events.md`,
  `docs/adr/ADR-010-at-least-once-effectively-once.md`, [kafka.md](kafka.md),
  [consumer-lag.md](consumer-lag.md)

## What this means

Every state change writes its event to `pp.outbox_events` **inside the same transaction** as the
state itself. The relay claims rows (`FOR UPDATE SKIP LOCKED`, partitioned by a stable
`shard_bucket` derived from the partition key so one aggregate's events always land on one
replica and stay ordered) and publishes them to Kafka.

The two alerts are different failures:

- **Growing** — the relay is publishing, just not fast enough, or Kafka is throttling. KEDA scales
  the relay on this exact gauge, so a rising backlog *with the relay already at
  `maxReplicaCount`* means the bottleneck is downstream, and adding replicas will not help.
- **Stalled** — backlog with a **zero** publish rate. That is not slow, it is stopped: a stuck
  advisory lock, a crash-loop, or a Kafka authentication failure. Nothing downstream is advancing
  at all.

**Nothing is lost either way.** The relay backs off and keeps rows; that decoupling is the entire
reason the outbox exists, and it is why the payment path is unaffected.

## Impact

- **The payment path: unaffected.** Payments are created, authorized, captured and refunded
  normally. This is by design, and it is the property worth stating first.
- **Downstream is frozen**: read projections, the ledger, audit fan-out, merchant webhook
  deliveries, config invalidation. Merchants see stale payment status in their dashboards and
  their webhooks stop arriving.
- **On `OutboxStalled`, the ledger is not advancing**, which means reported balances do not reflect
  payments that have already settled — the same consequence as
  [consumer-lag.md](consumer-lag.md)'s ledger case, one step earlier in the pipeline.
- Second-order: `payment.reconciliation_required.v1` is one of the events not being published, so a
  stalled outbox silently disables the reconciler
  ([timeout-unknown.md](timeout-unknown.md), [reconciliation.md](reconciliation.md)).

## Immediate triage (first 5 minutes)

1. The one command that answers most of this:
   ```bash
   ./bin/platformctl outbox status
   ```
   It prints unpublished, failed, claimed, the oldest unpublished row with its age, and the
   breakdown by topic, and it exits non-zero when the oldest row is past the drain threshold.
2. Growing or stalled?
   ```promql
   pp_outbox_backlog
   rate(pp_outbox_published_total[5m])
   deriv(pp_outbox_backlog[10m])
   ```
3. Is the relay alive, and is KEDA already at the ceiling?
   ```bash
   kubectl -n pp-data-plane get pods -l app=outbox-relay
   kubectl -n pp-data-plane logs deploy/outbox-relay --since=10m | tail -50
   kubectl -n pp-data-plane get hpa,scaledobject -l app=outbox-relay
   ```
4. Is Kafka the problem?
   ```promql
   kafka_cluster_partition_underreplicated
   pp_consumer_lag
   ```
5. Stuck claims — rows claimed by a replica that no longer exists:
   ```sql
   SELECT topic,
          count(*) FILTER (WHERE published_at IS NULL)                          AS unpublished,
          count(*) FILTER (WHERE claimed_at IS NOT NULL AND published_at IS NULL) AS claimed,
          min(created_at) FILTER (WHERE published_at IS NULL)                   AS oldest,
          max(publish_attempts)                                                 AS max_attempts
   FROM   pp.outbox_events
   WHERE  published_at IS NULL
   GROUP  BY topic ORDER BY unpublished DESC;

   SELECT last_error, count(*) FROM pp.outbox_events
   WHERE  published_at IS NULL AND last_error <> ''
   GROUP  BY last_error ORDER BY 2 DESC LIMIT 10;
   ```
6. Is the writer generating faster than anyone could drain?
   ```promql
   pp:payments:tps5m
   ```

## Diagnosis

- **Publish rate is zero and the relay pods are `CrashLoopBackOff`** → the relay cannot start.
  Read its startup error; the loader names every missing variable at once. → *M1*.
- **Publish rate zero, pods `Running`, logs show SASL/authentication or authorization failures** →
  a credential expired or the broker ACLs changed. → *M2*.
- **Publish rate zero, pods `Running`, no errors, `claimed` count high and static** → rows are
  claimed by replicas that died without releasing. → *M3*.
- **`kafka_cluster_partition_underreplicated > 0`** → the broker cannot accept writes at
  `min.insync.replicas=2`. → [kafka.md](kafka.md); the outbox backing up is the *correct*
  behaviour and will drain on its own.
- **Publish rate healthy but backlog rising, relay below `maxReplicaCount`** → KEDA is scaling;
  give it a few minutes. → *M4* only if it does not.
- **Publish rate healthy, relay at `maxReplicaCount`, backlog still rising** → the bottleneck is
  the broker (throttling, a leader election, an ISR shrink), not the relay. **Do not add
  replicas.** → [kafka.md](kafka.md).
- **Backlog concentrated on one topic** → that topic's partitions or its consumer are the problem,
  not the relay as a whole.
- **`last_error` shows a serialization or schema error on specific rows** → a poison row the relay
  keeps retrying. → *M5*.
- **`pp:payments:tps5m` is at a record high** → this is genuine load, and the backlog is the
  outbox doing its job of absorbing it. → *M4*.

## Mitigation

**M1 — fix the relay's configuration and roll.** `outbox-relay` requires `PP_ENVIRONMENT`,
`PP_REGION` and `PP_DATABASE_URL`; the shard variables `PP_RELAY_SHARD` / `PP_RELAY_TOTAL_SHARDS`
must satisfy `0 <= shard < total`, and the process refuses to start otherwise.
```bash
kubectl -n pp-data-plane logs deploy/outbox-relay --tail=40   # names every missing variable
kubectl -n pp-data-plane rollout restart deployment/outbox-relay
kubectl -n pp-data-plane rollout status deployment/outbox-relay --timeout=5m
```
Expected: `rate(pp_outbox_published_total[5m])` becomes non-zero within a minute.

**M2 — rotate the broker credential** via the dual-run workflow
([security-credential-rotation.md](security-credential-rotation.md)). Expected: publishes resume
immediately; the backlog drains at the relay's throughput.

**M3 — release stale claims.** A restart is the safe way; the relay re-claims on start and
`SKIP LOCKED` makes concurrent claims harmless:
```bash
kubectl -n pp-data-plane rollout restart deployment/outbox-relay
```
Expected: `claimed` falls, `unpublished` starts dropping. Prefer this to editing `claimed_at` by
hand — publishing is at-least-once and consumers are effectively-once, so a redundant claim costs
a duplicate delivery, which is harmless, whereas a wrong `UPDATE` is not.

**M4 — raise the relay ceiling.** Only when the relay is genuinely the bottleneck (publishing at
capacity, broker healthy):
```bash
kubectl -n pp-data-plane scale deployment/outbox-relay --replicas=<higher>
```
Ordering is preserved across replicas because the shard bucket is derived from the partition key
and is stable across rescales — that is why scaling out here is safe when scaling out a naive
relay would not be.

**M5 — quarantine a poison row.** Identify it precisely, and move it rather than deleting it:
```sql
SELECT outbox_id, event_id, event_type, topic, publish_attempts, last_error
FROM   pp.outbox_events
WHERE  published_at IS NULL AND publish_attempts > 20
ORDER  BY publish_attempts DESC LIMIT 20;
```
Push its `available_at` forward so it stops blocking the claim, and file the defect with the
`event_id`. Never `DELETE` — the row is the only record that the state change should have been
published, and deleting it makes the divergence permanent and invisible.

## Rollback / escalation

- **`OutboxStalled` is a page from minute zero.** Nothing downstream is advancing; treat it as
  Sev-2 immediately and Sev-1 if the ledger topic is involved.
- **Backlog over 100 000 rows or the oldest row over 15 minutes** → page, per the queue-depth
  table in `docs/failure-handling.md` §5.3.
- **The reconciler depends on this.** If `payment.reconciliation_required.v1` is in the backlog
  while `TimeoutUnknownSpike` is firing, escalate both together: the resolution path for ambiguous
  money is switched off.
- **Do not stop the writers to "let it catch up".** The payment path is intentionally decoupled
  from this, and stopping payments to protect a queue inverts the whole design.
- **Do not truncate the outbox.** There is no scenario in this runbook where that is the answer.

## Verification

```promql
rate(pp_outbox_published_total[5m]) > 0
pp_outbox_backlog < 1000
deriv(pp_outbox_backlog[10m]) < 0     # while draining
```
```bash
./bin/platformctl outbox status        # exits 0; "oldest none — the outbox is drained"
```
Then confirm the *consumers* caught up too — a drained outbox that dumped 100 000 events into
Kafka has simply moved the queue: see [consumer-lag.md](consumer-lag.md). Check the ledger
balances after a large drain:
```sql
SELECT account_id, sum(amount) FROM pp.ledger_entries GROUP BY account_id HAVING sum(amount) <> 0;
```

## Follow-up

- Record peak backlog, oldest row age, drain duration, and whether KEDA hit `maxReplicaCount`.
- If the ceiling was hit, the ceiling is the finding — either raise it permanently or fix the
  downstream bottleneck it was hiding.
- If a credential expired, the rotation automation did not run at 75 days as it should have
  (`docs/security.md` §5.3). That is a separate ticket and a more important one.
- Any row with `publish_attempts` in the hundreds is a poison-row defect: add the case to the
  event contract suite so the shape fails in CI (`make test-contract`).
- Confirm the coverage: `tests/chaos/infra_test.go::TestKafkaUnavailableLosesNoEvents` and
  `tests/integration/outbox_test.go::TestBacklogMetricReflectsReality`.
