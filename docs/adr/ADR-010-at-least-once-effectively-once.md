# ADR-010: At-least-once delivery with effectively-once business semantics

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §13.4 (outbox), §13.5 (effectively-once consumption), §1.3 ambiguity A8, §9 (invariants I1–I3) of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Refines ADR-003 (messaging) and ADR-004 (idempotency + ledger); related to ADR-020

## Context

Every state change in the platform must produce exactly one *business effect* in every downstream
consumer — one ledger entry, one notification, one projection update — while the transport
underneath is a network that loses, duplicates and reorders messages.

The forces:

1. **Exactly-once delivery does not exist across process boundaries.** A consumer that has
   processed a message and then crashes before acknowledging cannot be distinguished, by the
   broker, from one that crashed before processing. The broker must redeliver. Any protocol
   claiming otherwise has moved the deduplication somewhere else and not told you.
2. **Exactly-once *effect* does exist** and is what the business needs: the ledger must not
   double-count a capture, the merchant must not receive two "payment authorized" emails, the
   projection must not increment twice.
3. **The dual-write problem is the actual danger.** Writing the payment state to Postgres and
   then publishing to Kafka is two commits with a window between them. Crash in the window and
   either the state exists with no event (ledger never learns about a capture — a silent
   financial discrepancy) or the event exists with no state (ledger records a capture that was
   rolled back — a phantom entry). At 5 000 TPS a 10 ms window is hit thousands of times a day
   over a year.
4. **Volume.** §13.3 sizes `pp.payments.payment.v1` at 48 partitions and 30 days retention; at
   5 000 TPS with ~3 events per payment, that is ~15 000 events/s at peak. Whatever we choose
   must work at that rate without per-message coordination.
5. **Defence in depth is mandatory for money.** A bug in the deduplication path must still not be
   able to move money twice. The last line of defence has to be something a bug cannot talk its
   way past — a database constraint.

What breaks if we choose wrong: a ledger that does not reconcile with the gateway, discovered
weeks later during settlement; or duplicate refunds; or a system that is correct but so slow it
cannot carry the load.

## Decision

**At-least-once delivery, effectively-once business effect, achieved by three independent
mechanisms stacked in depth: the transactional outbox, the consumer dedup table, and
database-enforced business invariants.**

1. **Transactional outbox (§13.4).** Every state change and its event are written in **one**
   database transaction: the state row plus a row in `outbox_events`. There is no code path that
   publishes to Kafka directly — `outbox-relay` is the **only** publisher, and it is a separate
   deployable (§5). The relay polls with `SELECT … FOR UPDATE SKIP LOCKED`, publishes, and marks
   published. If it crashes after publishing but before marking, it republishes on restart. That
   is the at-least-once part, and it is deliberate: a duplicate is recoverable, a loss is not.
2. **Effectively-once consumption (§13.5).** Every consumer, in one transaction:
   ```
   INSERT INTO event_dedup (consumer_group, event_id) VALUES ($1, $2) ON CONFLICT DO NOTHING
   → 0 rows affected: already processed → ACK and drop
   → 1 row affected: handle the event in this same transaction → commit → ACK
   ```
   The dedup row and the effect commit together, so "processed" and "effect applied" cannot
   diverge. The unique key is `(consumer_group, event_id)`, so adding a consumer group replays
   the topic cleanly.
3. **Database-enforced invariants as the last line (§9).** I1 (`sum(refunds) ≤ captured`),
   I2 (`captured ≤ authorized`), I3 (partial unique index on `(payment_id) WHERE
   outcome='SUCCESS'`, partition-aligned per amendment A-02). These hold **even if both the
   outbox and the dedup table have bugs**. A duplicate capture does not become a duplicate
   ledger entry because the constraint refuses it.
4. **Ordering is per partition key only** (§13.3). Partition key is the aggregate ID, so all
   events for one payment are ordered. No consumer may assume a global order; this is asserted by
   the consumer contract tests.
5. **Poison messages** go `.retry` → `.dlq` (§13.3, §24) with the full error chain. The consumer
   never stops for one bad message.

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Outbox + dedup table + DB invariants (chosen)** | No dual write, by construction; correctness rests on the database we already trust for money; works identically for Kafka, SQS or anything else, so the transport stays a replaceable detail; the three layers are independent, so a bug in one is caught by another; the outbox table doubles as a durable event log for replay and for debugging "did we emit that?" | Adds a write to every state-changing transaction (~1 extra row); publish latency is polling-bound (target ≤ 200 ms p99, tunable against database load); the relay is another deployable to run and monitor; the dedup table is high-churn and needs partitioning and pruning; consumers must be written to a discipline, and a consumer that does effects outside its transaction silently breaks the guarantee | **Accepted** |
| **Kafka transactions / exactly-once semantics (EOS)** | Genuinely exactly-once *within Kafka*: consume-transform-produce is atomic across topics; no dedup table for Kafka-to-Kafka stages; well-supported and battle-tested in stream processing | The guarantee stops at Kafka's boundary. Our effects are **Postgres writes** — a ledger entry, a projection row — and Kafka's transaction coordinator cannot include a Postgres commit. So we would still need the dedup table for every effect that touches the database, which is all of them. Meanwhile EOS costs: transactional producers add coordinator round-trips (measurably lower throughput and higher p99), `read_committed` consumers add latency until the LSO advances, zombie fencing adds operational complexity, and it does nothing at all for the *ingress* dual-write problem, which is where the real risk is. This is the option a Kafka-experienced engineer pushes for, and within a pure stream-processing topology it would be right — it loses because our effects live in a different transactional domain | Rejected |
| **Distributed transactions (XA / two-phase commit across Postgres and the broker)** | True atomicity across stores; the mental model everyone wants | 2PC blocks on coordinator failure with locks held on the payment tables — precisely the tables that must never block at 5 000 TPS; Kafka has no XA resource manager; recovery requires a durable coordinator that is itself a single point of failure; in-doubt transactions require manual resolution during an incident, which is the worst possible time. The availability cost is paid on every transaction to protect against a failure the outbox handles for free | Rejected |
| **Publish-then-write, or write-then-publish (no outbox)** | Simplest possible code; one fewer table, no relay to operate; lowest latency to the broker | This *is* the dual-write bug. Either orphan events (ledger records money that was rolled back) or lost events (money moved and the ledger never hears). At our volume the window is hit continuously, and the resulting discrepancies surface during settlement reconciliation weeks later, when they are expensive to diagnose | Rejected |
| **Change Data Capture (Debezium on the WAL) instead of an outbox table** | No application-side write; captures everything; no polling latency; battle-tested | Couples the event schema to the *physical table schema*, so a column rename becomes a breaking change to a published contract (§13 requires versioned, additive-only events); emitting a well-formed domain event requires transformation logic that then lives outside the domain; adds Kafka Connect as an operational dependency with its own failure modes; replication-slot management becomes a production hazard (a stalled slot fills the primary's disk). The outbox pattern gives us the same durability with an *intentional* published contract | Rejected — reconsider if outbox write amplification becomes the bottleneck |

## Consequences

### Positive

- The dual-write failure mode is eliminated by construction, not by care.
- Kafka loss is a backlog, not a data-loss event: the outbox retains rows and backs off (§24).
  This is why "Kafka down" is a latency incident rather than a correctness incident.
- Correctness does not depend on broker semantics, so ADR-020's choice of Kafka is genuinely
  reversible.
- Replay is available: reset a consumer group, and the dedup table's `consumer_group` scoping
  makes a clean rebuild possible without touching other consumers.
- Invariants I1–I3 mean that even a total failure of the event pipeline cannot produce a double
  charge or an over-refund.

### Negative

- Every state-changing transaction carries an extra insert; at 5 000 TPS with ~3 events per
  payment that is ~15 000 outbox rows/s at peak, plus the same volume again in dedup rows across
  consumer groups. Both tables must be partitioned with partition-drop pruning.
- Publish latency is bounded by the relay's polling interval, so the end-to-end event path has a
  floor we would not have with direct publishing.
- Consumers must be written correctly: the effect **must** be in the same transaction as the
  dedup insert. A consumer that sends an email (a non-transactional effect) can still send it
  twice — such consumers must be at-least-once tolerant by design, and that is a per-consumer
  design obligation we cannot enforce mechanically.
- The relay is a throughput chokepoint: one logical publisher for the whole platform. It must be
  horizontally scalable (`SKIP LOCKED` makes this safe) and monitored on `pp_outbox_backlog`.

### Neutral / accepted costs

- Duplicates are normal traffic, not incidents. Dashboards must show dedup hit rate as an
  expected non-zero number so that operators do not chase it.
- Non-transactional effects (emails, webhooks to merchants) are effectively-once only to the
  extent that the downstream is idempotent; outbound merchant webhooks therefore carry an event
  ID and merchants are told to dedupe on it.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| A consumer performs its effect outside the dedup transaction | Medium | High — duplicate effects | `IdempotentConsumer` wrapper owns the transaction and passes the `Tx` to the handler; a handler that opens its own connection is a review-blocking defect; contract test in `tests/contract` asserts double-delivery produces one effect for every registered consumer | Duplicate-effect assertions in the contract suite; ledger reconciliation exceptions |
| Someone publishes to Kafka directly, bypassing the outbox | Medium | **Critical** — reintroduces the dual write | Only `outbox-relay` links the Kafka producer; architecture check forbids importing the producer from any other binary | Import-graph check in CI |
| Outbox backlog grows unbounded during a Kafka outage | Medium | Medium — event lag, disk pressure on the primary | Relay backs off and retains (§24); `pp_outbox_backlog` alerting; capacity plan sized for a 4-hour Kafka outage at peak (~200 M rows worst case — the partition strategy must accommodate it or we shed non-critical topics first) | `pp_outbox_backlog` gauge; primary disk-free |
| Dedup table growth degrades consumers | High if unmanaged | Medium | Partition by day, retain 30 days (matching topic retention — dedup must outlive the longest possible redelivery), drop partitions | Table size; consumer insert latency |
| Relay publishes out of order after a crash | Medium | Medium — consumers see reordering within a key | Relay orders by `(partition_key, sequence)` and publishes per key in sequence; consumers are required to tolerate reordering *between* keys only, and aggregate version (`aggregateversion`) lets a consumer detect and park an out-of-order event | Consumer-side version-gap counter |
| Invariant I3 silently weakened by partitioning | Low (already found once) | **Critical** | Amendment A-02: both `payments` and `payment_attempts` partition on a `partition_month` derived from the *payment's* ULID, so all attempts share the payment's partition; test asserts an attempt created months later lands in the payment's partition | `TestAttemptSharesPaymentPartition`; partition-routing assertion in CI |

## Validation

- **Double-delivery contract test:** every registered consumer is fed the same event twice and
  asserted to produce exactly one effect. Runs in `tests/contract` for every consumer, enforced by
  a registry-completeness check so a new consumer cannot skip it.
- **Crash-window test:** kill the process between the state commit and the relay publish; assert
  the event is still published after restart and that no state exists without its event.
- **Reconciliation as the real proof:** `reconciliation_runs` compares our ledger against gateway
  settlement reports. The success criterion is **zero unexplained discrepancies per month**. This
  is the only end-to-end validation that matters; the unit tests validate mechanisms, this
  validates the outcome.
- **Metrics:** `pp_outbox_backlog` p99 ≤ 200 ms of lag in steady state; `pp_consumer_lag` within
  SLO; dedup hit rate tracked as an expected non-zero baseline.

## Revisit criteria

Reopen if:

1. Outbox write amplification becomes a demonstrable bottleneck on the primary (> 20 % of write
   IOPS) — CDC-from-WAL becomes the leading alternative at that point, with the schema-coupling
   cost accepted and mitigated by an explicit transformation layer.
2. The entire consumer topology becomes Kafka-to-Kafka with no database effects, which would make
   Kafka EOS genuinely sufficient for those stages.
3. A transactional messaging substrate emerges that can enlist a Postgres commit without 2PC's
   blocking behaviour.
4. Reconciliation surfaces a discrepancy class that this stack cannot explain — that is evidence
   the model is wrong, not just the implementation.
