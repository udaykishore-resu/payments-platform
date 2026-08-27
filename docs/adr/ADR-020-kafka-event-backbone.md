# ADR-020: Kafka as the event backbone with per-aggregate partition keys

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §13 (event catalog, envelope, topics, outbox, effectively-once consumption), §5 (`outbox-relay`, `event-consumer`), §16.1 (tenancy in events) of docs/spec/00-design-baseline.md
- **Supersedes / Related:** **Supersedes ADR-003 (messaging: SQS) for platform event distribution.** Depends on ADR-010 (outbox + dedup)

## Relationship to ADR-003 — what changed

ADR-003 chose **SQS** and was correct for the scope it addressed: a single service with a small
number of asynchronous work items, point-to-point, where the requirement was "get this task done
eventually, once" and the consumer set was known and static. SQS is an excellent work queue and
that decision is not retracted for work-queue use cases.

Four things changed between that decision and this one, and each is decisive:

1. **The consumer set became open and multiplied.** §13.2 lists 25 event types, most with three
   to five independent consumers (Ledger, Notification, Analytics, Audit, Routing feedback, Data
   plane cache). SQS is point-to-point: fan-out requires SNS in front, and then each consumer
   needs its own queue, its own subscription, its own DLQ and its own redrive policy — an
   O(events × consumers) topology to provision and maintain.
2. **Ordering became a correctness requirement.** §13.3 requires per-aggregate ordering:
   `payment.authorized` must not be processed after `payment.captured` for the same payment.
   Standard SQS has no ordering. FIFO SQS does, per message group, but caps at 300 TPS per API
   action without batching (3 000 with batching) — against a peak of ~15 000 events/s, which
   would require sharding across many FIFO queues and reimplementing partitioning by hand.
3. **Replay became a requirement.** Rebuilding a projection, onboarding a new consumer, or
   reprocessing after a consumer bug requires reading history. SQS deletes on acknowledgement;
   there is no history to read. Every replay would need a bespoke re-publication from the outbox,
   which works but makes a routine operation a bespoke one.
4. **Volume grew by orders of magnitude.** The earlier scope did not contemplate 5 000 TPS
   sustained and 15 000 TPS peak with ~3 events per payment.

**Disposition:** ADR-003 is **superseded for platform event distribution**. It remains valid for
point-to-point work queues with no ordering or replay requirement (for example, outbound
notification delivery to a third-party provider), and those uses do not require a new ADR.

## Context

The event backbone carries every domain event between the nine bounded contexts and is what makes
ADR-006's plane decoupling and ADR-007's control/data-plane independence work in practice.

The forces, stated concretely:

1. **Throughput:** ~15 000 events/s at peak. §13.3 sizes `pp.payments.payment.v1` at 48
   partitions, 30-day retention.
2. **Ordering per aggregate, none globally.** All events for one payment must be ordered; no
   consumer may assume a global order.
3. **Fan-out to independent consumer groups**, each able to fail, lag and be replayed
   independently.
4. **Replay and rebuild** as a routine operation, not an emergency one.
5. **Retention:** 30 days for payments and merchants, 7 days plus compaction for configuration,
   400 days for audit (then S3).
6. **Compaction** for state-like topics: configuration and gateway health need "latest value per
   key" semantics so a new consumer can bootstrap without reading history.
7. **Tenant isolation:** shared topics with `tenantid` in the envelope and Kafka ACLs by
   principal (§16.1), plus dedicated topics for siloed tenants.
8. **Correctness must not depend on the broker.** ADR-010 puts correctness in the outbox, the
   dedup table and database invariants — so the broker choice must be *reversible*.

What breaks if we choose wrong: out-of-order state transitions corrupting the payment lifecycle;
an inability to rebuild a projection after a bug; or a per-consumer topology that becomes
unmaintainable as consumers multiply.

## Decision

**Kafka (AWS MSK) is the event backbone. Partition key is the aggregate ID. Ordering is
guaranteed per partition key and nowhere else. `outbox-relay` is the only publisher.**

1. **Topics** follow `pp.<context>.<aggregate>.v1` with `.retry` and `.dlq` siblings (§13.3).
   Partition counts, retention and cleanup policy are as specified in §13.3 —
   `pp.payments.payment.v1` at 48 partitions / 30 d / delete;
   `pp.config.configuration.v1` at 12 / 7 d / **compact**;
   `pp.gateways.health.v1` at 6 / 1 d / **compact**;
   `pp.audit.v1` at 12 / 400 d → S3.
2. **Partition key is the aggregate ID** (`payment_id`, `merchant_id`, `gateway_id`,
   `gateway_ref`, `tenant_id` for audit) and is carried explicitly as `partitionkey` in the
   envelope (§13.1). Ordering guarantee: **per partition key only.**
3. **Envelope is CloudEvents 1.0-compatible** with required platform extensions: `tenantid`,
   `merchantid`, `correlationid`, `causationid`, `traceparent`, `aggregateid`,
   `aggregateversion`, `partitionkey`.
4. **Versioning in the type name** (`.v1`), additive-only within a major, `.v2` published
   alongside `.v1` until every consumer migrates. Never an in-place schema edit.
5. **Only `outbox-relay` publishes.** No service holds a Kafka producer. This is what makes the
   outbox guarantee (ADR-010) unbypassable.
6. **Consumers are idempotent** via the dedup table (§13.5) and dedupe on
   `(consumer_group, event_id)` — so at-least-once redelivery, consumer-group resets and replays
   are all safe.
7. **Poison messages** go `.retry` → `.dlq` with the full error chain; the consumer never stalls
   on one message. `pp_dlq_depth` alerts.
8. **Tenant isolation:** shared topics with `tenantid` in the envelope, tenant-scoped consumer
   filters, Kafka ACLs by principal; dedicated topics for the siloed tier (§16.1, ADR-008).
9. **`aggregateversion` lets a consumer detect a gap or a reordering** within a key and park the
   event rather than apply it out of order.

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Kafka / MSK (chosen)** | Per-partition ordering with an explicit key, which is exactly the guarantee we need and no more; consumer groups give independent fan-out with independent lag and independent replay; retention plus offset reset makes replay a routine operation; log compaction gives "latest per key" semantics for configuration and health topics, so a cold consumer bootstraps without reading history; 15 000 events/s is unremarkable for Kafka; mature ecosystem (schema registry, Connect, Streams) if we ever need it | Operationally the heaviest option: brokers, partitions, ISR, rebalances, consumer-group management and version upgrades — MSK removes much but not all of this; partition count is effectively a one-way door per topic (increasing it changes key→partition mapping and breaks ordering for in-flight keys); rebalance storms are a real failure mode; per-tenant isolation is by convention and ACL rather than by construction | **Accepted** |
| **SQS + SNS fan-out** | Nearly zero operational burden; scales automatically; per-consumer DLQ and redrive built in; excellent for work queues; ADR-003's original choice and a good one for its scope | No replay — messages are deleted on acknowledgement, so rebuilding a projection requires bespoke re-publication from the outbox for every rebuild; ordering requires FIFO, which caps at 3 000 TPS per API action with batching and needs manual sharding across queues to reach 15 000/s, at which point we have hand-built partitioning with none of Kafka's tooling; O(events × consumers) queues and subscriptions to provision; no compaction, so a cold configuration consumer cannot bootstrap from the topic. This is the incumbent and the option with the strongest operational-simplicity argument — it loses on ordering-at-throughput and replay | Rejected for event distribution; **retained for point-to-point work queues** |
| **EventBridge** | Serverless, no capacity management; content-based routing rules are genuinely powerful; excellent AWS-service integration; schema registry included | No ordering guarantees at all — disqualifying on its own for payment lifecycle events; 24-hour retention with archive/replay that is coarse-grained and not consumer-group scoped; per-event pricing at 15 000/s becomes significant; routing rules put business logic in infrastructure configuration, outside our review and test process | Rejected |
| **Postgres-only queueing (`LISTEN/NOTIFY` or polling the outbox directly)** | No new infrastructure at all; transactional with the state change by construction; we already have the outbox table; strong consistency; one backup story | The outbox already exists, so this is genuinely tempting — but consumers would poll the primary at consumer-count × poll-rate, adding load to the most contended resource in the platform; fan-out to N consumer groups means N cursors and N scans over the same table; retention becomes a table-growth problem rather than a broker setting; cross-region event distribution (ADR-021) has no answer; `LISTEN/NOTIFY` is not durable and drops notifications on disconnect. Viable at low volume, and it is what we would fall back to — but not at 15 000 events/s with six consumer groups | Rejected |
| **NATS JetStream** | Lighter operationally than Kafka; good ordering and replay; simpler mental model; fast | Smaller ecosystem and smaller operational knowledge base; no managed AWS offering, so we would run it ourselves — trading MSK's managed brokers for self-managed ones, which is the opposite of the direction we want; compaction semantics differ from Kafka's in ways our configuration-bootstrap design depends on | Rejected |
| **Kinesis** | Managed, AWS-native, ordered per shard, replay within retention (up to 365 days) | Shard management is manual and resharding is disruptive; 5 reads/s per shard limits fan-out and forces the enhanced fan-out feature (and its cost); no compaction; consumer-group semantics are weaker and require the KCL with its own DynamoDB lease table — an extra stateful dependency | Rejected |

## Consequences

### Positive

- Per-payment event ordering is guaranteed, so consumers can apply state transitions safely.
- Adding a consumer is adding a consumer group — no infrastructure provisioning, no fan-out
  topology change, and it can replay history from the start of retention to build its state.
- Compacted configuration and health topics mean a restarting data-plane pod bootstraps its
  snapshot from the topic without a control-plane call (which is what makes ADR-007 and ADR-019
  implementable).
- The DLQ/retry pattern is uniform across all consumers.
- Correctness does not depend on Kafka: the outbox retains on broker loss and the dedup table
  handles redelivery, so a Kafka outage is a lag incident (§24) and this choice stays reversible.

### Negative

- MSK is the heaviest operational component we run: broker sizing, storage, partition planning,
  version upgrades, ACL management and rebalance behaviour.
- Partition counts are effectively fixed: increasing them remaps keys and breaks ordering for
  in-flight aggregates, so §13.3's numbers must be right with headroom (48 partitions for payments
  gives ~312 events/s per partition at peak — comfortable, and sized for growth).
- Tenant isolation on shared topics is by envelope field and ACL, not by construction. A consumer
  bug could process another tenant's event; the tenant guard (§16.2) must apply on the consumer
  side too.
- Cost: MSK at this scale is a meaningful line item compared to SQS's per-request pricing at our
  volume.
- Consumer rebalances cause brief processing pauses; sticky assignment and tuned session timeouts
  are required, not optional.

### Neutral / accepted costs

- Two messaging technologies in the platform (Kafka for events, SQS where ADR-003 still applies).
  This is deliberate — a work queue and an event log are different tools — but it must be
  documented so the choice is not made by coin-flip.
- 30-day retention bounds replay depth; anything older is rebuilt from the audit archive in S3
  or from the source-of-truth tables.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| Partition count outgrown, requiring a repartition | Medium over years | High — ordering break during migration | §13.3 counts sized with ~3× headroom; a repartition is a planned migration with a dual-topic cutover, never an in-place change | Per-partition throughput approaching broker limits; consumer lag concentrated in specific partitions |
| Wrong partition key chosen for a new event type | Medium | High — ordering silently lost for that aggregate | `partitionkey` is a required envelope field and the event registry asserts it equals the aggregate ID for every registered type; a contract test enforces it | Registry test; consumer-side `aggregateversion` gap counter |
| Consumer rebalance storms | Medium | Medium — processing pauses, lag spikes | Sticky partition assignment; tuned `session.timeout.ms` and `max.poll.interval.ms`; consumers commit offsets only after the dedup transaction commits | Rebalance rate; `pp_consumer_lag` sawtooth |
| Kafka outage stalls the platform | Low | Medium — event lag only | Outbox retains and backs off (§24); no correctness loss; capacity plan sized for a 4-hour outage at peak | `pp_outbox_backlog`; producer error rate |
| Cross-tenant event processing by a buggy consumer | Low | **Critical** | `tenantid` in the envelope; tenant context set from the envelope before any repository call, so RLS (ADR-008) applies on the consumer side identically to the request side; Kafka ACLs by principal; dedicated topics for siloed tenants | Cross-tenant access test on the consumer path; RLS zero-row anomalies |
| Schema drift breaks consumers | Medium | High | Versioned type names, additive-only within a major, `.v2` alongside `.v1`; JSON Schema in `api/events/` with a compatibility check in CI | Schema-compatibility gate; consumer deserialization error rate |
| MSK cost growth | Medium | Low–Medium | Retention tuned per topic (§13.3); audit tiered to S3 at 400 days; compaction where state semantics allow | Monthly MSK cost per event |

## Validation

- **Ordering test:** publish a rapid sequence of lifecycle events for one payment across a
  rebalance and a broker restart; assert consumers observe them in `aggregateversion` order.
- **Replay test:** reset a consumer group to the earliest offset and rebuild a projection; assert
  the rebuilt state matches the live state exactly and that other consumer groups are unaffected.
- **Throughput test:** `tests/load` sustains 15 000 events/s through the relay and all consumer
  groups with `pp_consumer_lag` staying inside SLO.
- **Outage test:** stop Kafka for 30 minutes under load; assert zero event loss, outbox backlog
  drains cleanly on recovery, and no payment correctness errors occur.
- **Lag SLI:** `pp_consumer_lag` within SLO per group; webhook processing lag p99 ≤ 60 s (§22.4).
- **Reversibility check:** the outbox relay's publisher is behind an interface with a Postgres-
  polling implementation used in integration tests. If that implementation cannot be made to work,
  our claim that the broker choice is reversible is false.

## Revisit criteria

Reopen if:

1. Event volume falls by an order of magnitude and stays there — Postgres-only queueing would then
   be genuinely sufficient and materially cheaper.
2. MSK operational burden or cost exceeds the value of ordering, replay and compaction — the
   honest test is whether we actually *use* replay and compaction; if we never do, we are paying
   for options we do not exercise.
3. A managed service appears offering per-key ordering, consumer groups, replay and compaction
   with materially lower operational weight.
4. Multi-region event distribution (ADR-021) requires semantics MSK replication cannot provide.
