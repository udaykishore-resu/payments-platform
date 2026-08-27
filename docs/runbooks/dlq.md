# RB-010: Dead-letter queue depth

- **Severity:** ticket (`DLQNotEmpty`, P2) · page (`DLQGrowingFast`, P1)
- **Alert:** `DLQNotEmpty`
  ```promql
  pp_dlq_depth > 0
  ```
  `DLQGrowingFast`
  ```promql
  deriv(pp_dlq_depth[10m]) > 1
  ```
- **Triggered when:** any message has been parked in a DLQ for 15 minutes; or the DLQ is filling at
  more than 1 message/second, sustained for 5 minutes.
- **Plane / service:** data · `event-consumer`
- **Related:** `docs/failure-handling.md` §6 (topology, triage, replay safety, poison policy),
  [dlq-triage.md](dlq-triage.md), [consumer-lag.md](consumer-lag.md), [outbox.md](outbox.md)

## What this means

A message failed 3 in-process attempts (100 ms, 400 ms, 1.6 s full jitter), then 5 cycles on the
`.retry` topic with escalating delays (5 s, 30 s, 2 m, 10 m, 30 m), and was parked on `.dlq` with
its full diagnostic payload. **The consumer committed the offset and continued** — a poison message
never blocks a partition, which is the property that stops one bad message from becoming an outage.

`DLQNotEmpty` alerting at any depth is not paranoia; it is the enforcement mechanism for the policy
that every DLQ message is classified within one business day. `DLQGrowingFast` is a different
animal: one message is a curiosity, a thousand is an incident, and a fast-filling DLQ is almost
always **one** cause — one deploy, one schema change, one upstream producer bug.

The DLQ is **never auto-consumed**. Every replay is a deliberate, approved human action, and
replay is **not reversible**.

## Impact

- **The payment path: unaffected.** Payments are written and authorized normally.
- **Per parked message**: one entity's projection is stale until replay. If the message is a
  ledger event, a balance is wrong. If it is `payment.reconciliation_required.v1`, an ambiguous
  payment is not being reconciled — check [reconciliation.md](reconciliation.md).
- **`DLQGrowingFast`**: whole classes of state transitions are not being applied. At 1 msg/s, an
  hour is 3 600 lost applications.
- **Retention is 30 days.** A message not triaged in 30 days is deleted by the broker, and that
  state transition is then permanently lost. This is the deadline the P2 exists to protect.

## Immediate triage (first 5 minutes)

1. Depth and rate, by queue:
   ```promql
   pp_dlq_depth
   deriv(pp_dlq_depth[10m])
   topk(5, pp_dlq_depth)
   ```
2. **Group by cause before doing anything else.** One cause or many decides everything below:
   ```promql
   sum by (queue, type) (pp_dlq_depth)
   ```
3. Read the parked records. Each carries the original envelope, the raw bytes, the consumer group,
   the full attempt history with error chains, the code version and image digest, the trace ID and
   the tenant — everything a triager needs without going back to the logs.
4. Correlate with deploys and schema changes in the window:
   ```bash
   kubectl -n pp-data-plane rollout history deployment/event-consumer
   ./bin/platformctl migrate status
   ```
5. For the **workflow** DLQ, which is in Postgres rather than Kafka:
   ```bash
   ./bin/platformctl workflow dlq --limit 50
   ```
   ```sql
   SELECT dlq_id, instance_id, step_key, reason, parked_at, replay_count
   FROM   pp.workflow_dlq WHERE replayed_at IS NULL ORDER BY parked_at LIMIT 50;
   ```
6. Is a payment stuck behind any of this? That escalates to Sev-2 immediately (step 3 of the
   triage procedure in [dlq-triage.md](dlq-triage.md)).

## Diagnosis

Full classification is [dlq-triage.md](dlq-triage.md). The incident-time fork:

- **Fast fill, one `type`, one error signature, starting at a deploy boundary** → a handler
  regression. → *M1* (roll back), then replay.
- **Fast fill, one `type`, starting at a producer deploy** → the producer is emitting a shape the
  consumer cannot read. → *M2*.
- **Fast fill with deserialization errors** → a schema change without a compatible rollout. Parse
  failures are parked with **zero** retries, so the DLQ fills at the full production rate. → *M2*.
- **Slow accumulation, mixed types, "references a non-existent aggregate"** → ordering artifacts
  whose predecessors are in flight; the retry ladder usually resolves them. If they reached the
  DLQ, the predecessor is genuinely missing. → check [outbox.md](outbox.md).
- **Repeated poisoning of one aggregate (> 5 messages)** → that aggregate is quarantined and
  subsequent events for it are parked directly. The quarantine is working; the aggregate is the
  bug. → *M3*.
- **A handler panic in the logs** → treated as poison for that message only; the consumer
  recovered. Sev-3 bug ticket, automatically. → *M1* if it is systematic.
- **Depth static and small, ages under a day** → this is the P2 doing its job. → *M4*.

## Mitigation

**M0 — stop the bleeding first.** On `DLQGrowingFast`, the priority is to stop *adding* messages.
Every message parked while you diagnose is another one to replay later.

**M1 — roll back the consumer.**
```bash
kubectl -n pp-data-plane rollout undo deployment/event-consumer
kubectl -n pp-data-plane rollout status deployment/event-consumer --timeout=5m
```
Expected: `deriv(pp_dlq_depth[10m])` returns to zero within a rollout.

**M2 — roll back the producer**, or ship the compatible consumer. An incompatible schema change is
a contract break; the event schema compatibility rules and their check are in
`docs/events.md` and `scripts/check-events.sh`:
```bash
./scripts/check-events.sh
```
Expected: the check names the incompatible field. Fix forward or roll back the producer; do not
"just replay", which replays into the same failure.

**M3 — leave the quarantine in place** and fix the aggregate. Lifting a quarantine before the
underlying entity is repaired lets one broken entity consume the whole retry budget again.

**M4 — classify and dispose.** Follow [dlq-triage.md](dlq-triage.md). The four dispositions are
replay, fix-then-replay, discard (approved and audited), and manual repair.

### Replaying: what exists, and what does not

`docs/failure-handling.md` §6.3 specifies a replay tool —
`platformctl dlq replay --topic … --since … --until … --filter … --dry-run` — with ten safety
checks. **That subcommand is not implemented in this repository.** `platformctl help` lists
`migrate`, `seed`, `config validate`, `certify`, `dr-drill`, `outbox status`, `workflow
list|resume|dlq`, `verify-audit-chain` and `version`, and nothing else.

Consequences for you, right now:

- **Workflow DLQ entries can be replayed**, one at a time, through the workflow engine:
  ```bash
  ./bin/platformctl workflow dlq
  ./bin/platformctl workflow resume wfr_…
  ```
  `replay_count` is capped at 5 by a database constraint, deliberately: a DLQ entry that can be
  replayed without bound is a retry loop with a manual trigger.
- **Kafka DLQ topics have no supported replay path in this build.** Do not improvise one with
  `kcat` during an incident. The safety checks in §6.3 exist because replaying the wrong selection
  is the most likely way to make things worse, and dedup integrity
  (`pp.event_dedup`) is the only thing standing between a replay and a double application. Park
  the work, keep the messages (30-day retention), and escalate — building the tool is a planned
  task with review, not an incident improvisation.

## Rollback / escalation

- **A payment is stuck behind a DLQ message** → Sev-2 immediately, per the triage procedure.
- **Ledger events in the DLQ** → Sev-1. The ledger is append-only; a mistake there is not editable,
  only compensable. Money-affecting events replay **one at a time with verification between each**,
  and that is not something to attempt without the tooling and an approver.
- **A discard is proposed** → `payments:replay_dlq` is dual-controlled for the `operator` role. A
  discarded event is a permanently lost state transition, and the discard itself is audited with
  its reason. No single person discards.
- **Any message approaching 30 days** → escalate before it expires. After expiry the transition is
  gone and the only remaining path is manual repair from another source.
- **Replay is not reversible.** Stated here rather than discovered during an incident.

## Verification

```promql
pp_dlq_depth == 0
deriv(pp_dlq_depth[10m]) <= 0
pp_consumer_lag < 1000
```
```bash
./bin/platformctl workflow dlq          # "empty — no unreplayed dead-lettered steps"
```
After any replay or repair, verify the state the messages were meant to produce:
```sql
-- the ledger must balance
SELECT account_id, sum(amount) FROM pp.ledger_entries GROUP BY account_id HAVING sum(amount) <> 0;
-- no workflow left parked
SELECT count(*) FROM pp.workflow_dlq WHERE replayed_at IS NULL;
```
And trigger a reconciliation run for the affected merchants, per safety check 8 of §6.3: replay
without verification is hope.

## Follow-up

- Record every disposition with its reason. Dispositions are reviewed weekly, and a recurring
  cause is a missing test — that is the mechanism by which this queue gets smaller over time.
- If the cause was a schema change, `scripts/check-events.sh` should have caught it in CI. If it
  did not, extend the check; that is the durable fix.
- If a handler panicked, the Sev-3 bug ticket exists automatically — make sure it has the parked
  envelope attached.
- **Implement `platformctl dlq replay`** with the ten safety checks of §6.3. Every incident that
  ends with "we could not replay safely" should push this up the backlog, and the postmortem is
  where that pressure is recorded.
