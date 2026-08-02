# ADR-004: Transactional outbox + DB-enforced double-entry ledger for exactly-once financial correctness

## Status
Accepted

## Context
The single hardest requirement in this system: money must never be created, destroyed, or moved
twice. Client retries, load balancer replays, pod crashes, and network partitions are all
*expected*, not exceptional — the design must be correct under all of them, not just on the happy
path.

## Decision
Two complementary patterns, both enforced at the database layer rather than trusted to
application code alone:

1. **Idempotency key table + unique constraint**, checked and written inside the *same* database
   transaction as the ledger write. A duplicate request with the same key returns the original
   committed result; the same key with a different request body is rejected as a client error
   rather than silently deduplicated (protects against masking real client bugs).
2. **Transactional outbox**: the event announcing "payment completed" is written as a row in the
   same DB transaction as the ledger entries — not published directly to SQS from the request
   handler. A separate relay process (running as a goroutine in every pod, using
   `SELECT ... FOR UPDATE SKIP LOCKED` to safely claim rows across concurrently-running pods)
   publishes outbox rows to SNS/SQS and marks them published. This eliminates the classic
   "dual-write" bug where a DB commit succeeds but the message publish fails (or vice versa),
   silently losing or duplicating a financial event.
3. **Ledger balance invariant enforced by the database**, not just application code: a
   `CHECK`/trigger-based constraint on `ledger_entries` requires that all entries sharing a
   `payment_id` sum to zero before the transaction is allowed to commit. An application bug
   literally cannot produce an unbalanced ledger — the database refuses the write.

## WHEN to use this pattern
Any time a state change must be *atomically* paired with a notification/side-effect, especially
where the side-effect involves an external system that can't participate in the same database
transaction (SQS, a webhook, another service's database). This is the standard, industry-proven
answer (used at Stripe, Shopify, and others) to the dual-write problem.

## Alternatives Considered
- **Two-phase commit (2PC) between Postgres and SQS**: SQS doesn't support XA/2PC; not available.
- **Publish to SQS first, then write DB**: if the DB write then fails, we've told downstream
  consumers about a payment that doesn't exist — worse failure mode than a delayed event.
- **"Best effort" — publish to SQS inside the request handler after commit, no outbox**: simplest
  to build, but a crash between commit and publish silently loses the event with no recovery
  mechanism. Rejected — violates the "0 lost financial events" NFR.
- **Change Data Capture (CDC) via Debezium/DMS reading the WAL** instead of an explicit outbox
  table: also a valid exactly-once-effectively-delivery pattern and removes the need for a
  relay-polling loop, but adds a CDC pipeline (Kafka Connect or DMS) as new infrastructure to
  operate. Deferred: the polling-relay outbox is simpler to operate at current scale; CDC is the
  natural upgrade path if outbox polling latency or DB load from polling becomes a bottleneck.

## Tradeoffs
- Outbox polling adds a small, tunable latency (target: sub-second p99) between commit and
  external event delivery — acceptable per FR-6 (async settlement is explicitly allowed to be
  eventually consistent).
- Extra table and extra background worker to operate versus "just call SQS" — accepted
  complexity in exchange for correctness guarantees that are non-negotiable for a ledger.
- `SERIALIZABLE` isolation for the ledger write can produce serialization failures under high
  contention on the same account (e.g. rapid-fire payments from the same source account); the
  application must catch `40001` and retry with backoff — adds code complexity but is the correct
  way to get strict correctness under concurrency without pessimistic locking everywhere.

## Risks
- Relay implementation bug could double-publish (mitigated: consumers are contractually required
  to be idempotent, so a duplicate publish is safe, just wasteful) or under-publish (mitigated:
  `outbox_unpublished_count` alerting + reconciliation job that periodically cross-checks
  `payments` against `outbox_events`).
- High contention on a single hot account (e.g. a large merchant's central account) under
  `SERIALIZABLE` could increase retry rate and latency — mitigated with per-account rate limiting
  and, if needed later, splitting hot accounts into sharded sub-ledgers reconciled periodically.
