# ADR-003: Amazon SQS + SNS for asynchronous event distribution

## Status
Accepted

## Context
Payment completion must reliably notify multiple independent downstream consumers
(notifications, settlement, fraud/risk) without making the payment write path depend on their
availability, and without losing or duplicating events beyond what an idempotent consumer can
already handle.

## Decision
Transactional outbox in Postgres → relay process publishes to an SNS topic → SNS fans out to one
SQS queue per consumer, each with a dead-letter queue (DLQ).

- SQS/SNS are fully managed, require no cluster operations, and provide built-in DLQ and
  redrive-policy support out of the box — fastest path to a reliable, low-maintenance fan-out for
  the traffic volumes in the NFRs (hundreds to low thousands of events/sec).
- One SQS queue per consumer (via SNS fan-out) means a slow or broken consumer only backs up its
  own queue — a bulkhead between downstream services, matching the "partial failure shouldn't
  cascade" principle.
- At-least-once delivery is acceptable because every consumer is required (by contract) to be
  idempotent on `payment_id` + `event_type` — duplicates are expected and handled at the edge,
  not treated as an anomaly.

## WHEN to use this choice
Good default for decoupled, fan-out, at-least-once eventing at moderate volume where consumers
can be idempotent. Reassess for Kafka/MSK if any of the following becomes a real requirement:
long retention/replay for event-sourcing or audit reconstruction, ordered consumption across
partitions at very high throughput, or many more consumers than SNS fan-out comfortably supports.

## Alternatives Considered
- **Amazon MSK (Kafka)**: superior for event replay, long retention, and ordered high-throughput
  streams, but meaningfully higher operational complexity (partition management, consumer group
  rebalancing, broker capacity planning) than the current event volume justifies. Revisit when
  audit/event-sourcing requirements demand replaying the full event history, or throughput
  exceeds what SQS fan-out handles cleanly.
- **EventBridge**: nice for cross-account/cross-service routing rules, but SNS/SQS is simpler and
  sufficient for a small, known set of internal consumers.
- **Direct HTTP webhooks to consumers**: rejected — reintroduces synchronous coupling and no
  built-in retry/DLQ semantics; would have to be reinvented.

## Tradeoffs
- At-least-once delivery pushes an idempotency requirement onto every consumer — an explicit
  contract documented and enforced via a shared "event envelope" schema
  (`event_id`, `payment_id`, `event_type`, `occurred_at`, `attempt`).
- SNS fan-out is not ordered across queues by default; consumers that need strict ordering per
  account must key on `dedup_id`/sequence and reorder locally, or we introduce SQS FIFO queues
  per-account-partition for those specific consumers (higher cost, lower throughput ceiling).

## Risks
- Outbox relay lag under load or during a DB failover window delays event delivery (not lost,
  just delayed) — monitored via `outbox_unpublished_count` metric and alerted if it exceeds a
  threshold sustained for N minutes (see `06-observability.md`).
- A stuck/poison event lands in a DLQ — requires an operational runbook step to inspect, fix, and
  redrive (see `08-runbook.md`).
