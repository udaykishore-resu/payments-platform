# RB-011: DLQ triage — classification and disposition

- **Severity:** ticket (the procedure; the alerts that call it are in [dlq.md](dlq.md))
- **Alert:** none directly. This runbook is the classification step
  `docs/failure-handling.md` §6.2 names, invoked from `DLQNotEmpty` and `DLQGrowingFast`.
- **Triggered when:** a DLQ is non-empty. **Every DLQ message is classified within one business
  day** — the `pp_dlq_depth` alert exists to make that enforceable rather than aspirational.
- **Plane / service:** data · `event-consumer`, `workflow-worker`
- **Related:** `docs/failure-handling.md` §6.2–6.4, [dlq.md](dlq.md),
  `docs/events.md`, `docs/adr/ADR-010-at-least-once-effectively-once.md`

## What this means

Triage is the decision procedure, not the incident response. [dlq.md](dlq.md) stops the bleeding;
this decides what happens to each parked message. It is written down because the four outcomes have
very different costs and because two of them are irreversible.

The parked record carries everything needed: the original envelope verbatim, the raw bytes, the
consumer group, the full attempt history with timestamps and error chains, the code version and
image digest, the trace ID and the tenant. You should not need the logs.

## Impact

Deferred triage compounds. Each unclassified message is a state transition that has not been
applied, and after 30 days the broker deletes it and the transition is lost for good. A DLQ that
is triaged weekly instead of daily is a DLQ where the 30-day deadline is met by accident.

## Immediate triage (first 5 minutes)

This is a same-business-day procedure, not a five-minute one, but start here:

1. Inventory:
   ```promql
   pp_dlq_depth
   sum by (queue, type) (pp_dlq_depth)
   ```
2. Workflow DLQ, which lives in Postgres:
   ```bash
   ./bin/platformctl workflow dlq --limit 100
   ```
   ```sql
   SELECT reason, count(*), min(parked_at) AS oldest, max(replay_count) AS max_replays
   FROM   pp.workflow_dlq WHERE replayed_at IS NULL
   GROUP  BY reason ORDER BY 2 DESC;
   ```
3. Ageing — anything approaching 30 days is the priority regardless of severity:
   ```sql
   SELECT dlq_id, instance_id, step_key, parked_at, now() - parked_at AS age
   FROM   pp.workflow_dlq WHERE replayed_at IS NULL
   ORDER  BY parked_at LIMIT 20;
   ```
4. Business impact check, before classification: is a payment stuck? Is the ledger out of balance?
   Is a merchant's projection wrong?
   ```sql
   SELECT account_id, sum(amount) FROM pp.ledger_entries
   GROUP  BY account_id HAVING sum(amount) <> 0;
   ```
   A stuck payment escalates to Sev-2 immediately and stops being a triage exercise.

## Diagnosis

The classification, from `docs/failure-handling.md` §6.2 step 1:

| Class | Signature | Disposition |
|---|---|---|
| **Poison** | Malformed, unparseable, schema violation. Parked with zero retries | *M4* discard, or *M3* manual repair if the content is recoverable |
| **Transient-that-outlived-retries** | A dependency was down longer than the ~45 min retry ladder. Error chain names a timeout or a connection failure | *M1* replay |
| **Bug** | A handler defect. Same error every attempt, deterministic | *M2* fix then replay |
| **Data** | References an entity that does not exist or is in an unexpected state | *M3* manual repair, then replay |

Then, before deciding:

- **Blast radius** — group by `type` plus error signature. One message is a curiosity; a thousand
  is an incident, and a thousand of one signature is one cause with one fix.
- **Business impact** — a stuck payment is Sev-2. An out-of-balance ledger is Sev-1.
- **Age** — anything near 30 days jumps the queue.

## Mitigation

**M1 — replay (transient, now resolved).** For workflow steps:
```bash
./bin/platformctl workflow dlq              # find the instance id
./bin/platformctl workflow resume wfr_…     # resume from the last checkpoint
```
Completed steps are not replayed; the engine resumes from the checkpoint (baseline §11). The
`replay_count` cap of 5 is a database constraint: five is where someone has to look at *why* it
keeps failing rather than pressing the button again.

For Kafka DLQ topics: the specified tool (`platformctl dlq replay`, §6.3) **is not implemented in
this repository**. Until it is, Kafka-topic replay is not a supported operation — record the
decision to defer, keep the messages inside the 30-day window, and escalate. Do not improvise a
producer loop: replay safety rests entirely on `pp.event_dedup` being intact and on original
`(partition_key, sequence)` ordering, and an ad-hoc script guarantees neither.

**M2 — fix then replay.** Deploy the fix first and verify it. The specified tool refuses to replay
when the running image digest equals the digest recorded on the DLQ records, because replaying into
the same bug produces the same DLQ entries plus wasted capacity. Apply the same rule by hand:
compare the digest on the parked record against the running one.
```bash
kubectl -n pp-data-plane get deployment event-consumer \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
```

**M3 — manual repair (data).** Repair the referenced entity through its own supported path — the
control-plane API for configuration, the workflow engine for onboarding state — then replay. Never
by direct `UPDATE` on an aggregate table: the FSM guard, the version column, the event log, the
outbox and the audit chain are all bypassed by a direct write.

**M4 — discard. Dual-controlled, audited, irreversible.** `payments:replay_dlq` is dual-controlled
for the `operator` role. A discarded event is a **permanently lost state transition**. The discard
is audited with its reason. Legitimate cases are narrow: a duplicate, or an event superseded by
later state that the FSM would reject anyway.

**M5 — record.** Every disposition writes an audit record. Dispositions are reviewed weekly for
patterns; a recurring cause is a missing test, and that review is the only mechanism that makes the
queue shrink over time.

## Rollback / escalation

- **Money-affecting events (ledger, payment state) replay serially, with verification between
  each.** The ledger is append-only: a mistake is not editable, only compensable. If that pace is
  not possible, escalate rather than batching.
- **Any discard needs a second person.** Not a rubber stamp — the approver's job is to ask what is
  lost.
- **`replay_count` at 5** → stop. Escalate to the owning team. The message is telling you the fix
  is not the replay.
- **A message within 3 days of the 30-day retention** → escalate to the owning team with a
  deadline, in writing. After expiry the only path is reconstruction from another source, if one
  exists.
- **Replay is not reversible.** The safety checks are mandatory rather than advisory for that
  reason alone.

## Verification

```bash
./bin/platformctl workflow dlq       # "empty — no unreplayed dead-lettered steps"
```
```promql
pp_dlq_depth == 0
```
```sql
-- Every parked entry is disposed of.
SELECT count(*) FROM pp.workflow_dlq WHERE replayed_at IS NULL;
-- The ledger balances after any replay.
SELECT account_id, sum(amount) FROM pp.ledger_entries GROUP BY account_id HAVING sum(amount) <> 0;
-- Dedup is intact over the replayed range.
SELECT count(*) FROM pp.event_dedup WHERE processed_at > now() - interval '24 hours';
```
Post-replay, compare the payment-state distribution against the pre-replay snapshot and trigger a
reconciliation run for the affected merchants. Replay without verification is hope.

## Follow-up

- File one issue per distinct cause, not per message.
- A recurring cause is a missing test. Add it where it belongs: a consumer unit test for a handler
  bug, an event contract test for a shape (`make test-contract`, `./scripts/check-events.sh`), a
  test for a dependency (`internal/events/consumer_test.go::TestPoisonEnvelopeIsNonRetryable` is the existing one).
- Feed the weekly review with counts by class. A rising *poison* fraction means schema discipline
  is slipping; a rising *transient* fraction means the retry ladder is too short for a real
  dependency's outage profile.
- Where a disposition was deferred because the replay tool does not exist, say so explicitly in the
  review. That is the evidence that justifies building it.
