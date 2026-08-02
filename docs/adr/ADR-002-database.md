# ADR-002: Amazon Aurora PostgreSQL as the system of record

## Status
Accepted

## Context
The ledger requires ACID transactions, strong consistency, and the ability to express a hard
invariant ("debits == credits per payment") as a database-enforced constraint, not just
application logic that can be bypassed by a future bug or a manual `UPDATE`.

## Decision
Amazon Aurora PostgreSQL, Multi-AZ, with an Aurora Global Database for cross-region DR.

- PostgreSQL gives us `SERIALIZABLE` isolation, `CHECK` constraints, triggers, and row-level
  locking (`FOR UPDATE SKIP LOCKED`, used by the outbox relay) — the exact primitives needed for
  correct double-entry bookkeeping and safe concurrent outbox processing.
- Aurora's storage layer replicates 6 copies across 3 AZs with continuous backup to S3, giving
  very high durability (this is the layer that satisfies the "0 lost committed ledger rows" NFR),
  independent of the compute layer's health.
- Aurora Global Database gives a cross-region read replica with typically sub-second replication
  lag, promotable during a regional disaster — the backbone of the region-failure recovery plan.
- Managed automated failover (~30s) for AZ-level database failures, without operator intervention.

## WHEN to use this choice
Any workload needing strong transactional guarantees over structured, relational data with
enforceable invariants. Not the right default for high-cardinality time-series (use
Timestream/Prometheus), simple key-value lookups at extreme scale (use DynamoDB), or full-text
search (use OpenSearch) — those are separate concerns in this platform.

## Alternatives Considered
- **DynamoDB**: near-infinite horizontal scale and simpler ops, but weaker multi-item
  transactional semantics for the "read balance, write two balanced entries, write outbox row, all
  atomically" invariant (DynamoDB transactions exist but are limited to 100 items/4MB and lack
  `SERIALIZABLE`-style range invariants like "these entries must sum to zero" without extra
  application logic). Considered for `idempotency_keys` as a future optimization (single-key
  lookups, very high write volume) but kept in Postgres for v1 to keep the transaction boundary
  simple (idempotency check and ledger write in the *same* DB transaction).
- **Vanilla self-managed PostgreSQL on EC2**: full control, lower cost at very large scale, but
  we lose Aurora's managed failover, storage-layer replication, and Global Database DR story —
  meaning the team would have to build and constantly test failover tooling itself. Rejected: the
  operational burden isn't justified until scale/cost pressure demands it (revisit at
  significantly higher scale).
- **CockroachDB / Spanner-like distributed SQL**: attractive for true multi-region active-active
  writes with strong consistency, but adds operational complexity and a less mature AWS-native
  integration story than Aurora. Revisit if true multi-region active-active writes (not
  warm-standby) becomes a hard requirement.

## Tradeoffs
- Aurora scales reads easily (replicas) but write throughput is bounded by a single writer
  instance; if write volume outgrows one writer, we'll need sharding by account/tenant — deferred
  until metrics show we're approaching that ceiling (see `07-reliability-slo.md` capacity
  planning).
- Aurora Global Database replication is asynchronous across regions — a region failure can lose
  up to ~1s of the most recent commits (RPO ≈ 1s cross-region, RPO = 0 within-region). This is
  disclosed explicitly in the DR plan rather than papered over.

## Risks
- Single Aurora writer is a scaling and blast-radius chokepoint — mitigated by connection pooling
  (RDS Proxy / pgbouncer sidecar), query optimization, and a documented sharding path.
- Schema migrations on a hot financial table need care (see runbook: online migration procedure,
  `pg_repack`/expand-contract pattern, never a blocking `ALTER` on `ledger_entries` in place).
