# 14 — Database Architecture

## What this shows and why it matters

One Aurora PostgreSQL cluster per payment-processing region plus an Aurora Global secondary,
carrying every bounded context's tables under row-level security. Diagram A is the topology and
the routing rules for which connection serves which operation; Diagram B is the per-context table
grouping and the key relationships; Diagram C is the partitioning and RLS boundary. The details
that carry the most weight are the **partition alignment of `payments` and `payment_attempts`**
(Amendment A-02) and the fact that the application connects as a **non-`BYPASSRLS` role**, which
is what turns tenant isolation from a code convention into a database guarantee.

## Diagram A — Cluster topology and connection routing

```mermaid
flowchart TB
  subgraph RPRIM["Primary region - active"]
    W["Aurora writer instance"]
    R1["Aurora reader AZ b"]
    R2["Aurora reader AZ c"]
    RE["Reader endpoint - load balanced"]
  end

  subgraph RSEC["Secondary region - passive"]
    GW2["Aurora Global secondary, read only until promoted"]
  end

  subgraph CLIENTS["Connection routing by operation"]
    OPW["Payment writes, idempotency claims, ledger appends - CP, writer only"]
    OPR["GET payments and lists - AP, reader endpoint"]
    OPRYW["Read-your-writes after a mutation - write token forces the writer"]
    OPCTL["Control plane writes - writer only"]
    OPANA["Analytics and projections - reader endpoint or Kafka consumers"]
  end

  PGB["PgBouncer or RDS Proxy, transaction pooling, per-service pool"]

  OPW --> PGB --> W
  OPCTL --> PGB
  OPR --> RE
  OPRYW --> W
  OPANA --> RE
  W --> R1
  W --> R2
  R1 --> RE
  R2 --> RE
  W -.->|"Aurora Global replication, RPO under 1 s typical, 5 s budgeted"| GW2
  SYNC["In-region 6-way replication across 3 AZs, synchronous commit, RPO 0"]
  W -.-> SYNC
```

## Diagram B — Per-context table groups

```mermaid
erDiagram
    TENANTS ||--o{ API_CLIENTS : owns
    TENANTS ||--o{ ROLE_BINDINGS : scopes
    TENANTS ||--o{ MERCHANTS : owns
    MERCHANTS ||--|| MERCHANT_BUSINESS_PROFILE : has
    MERCHANTS ||--o{ MERCHANT_BANK_ACCOUNTS : has
    MERCHANTS ||--o| ONBOARDING_CASES : has
    ONBOARDING_CASES ||--|| WORKFLOW_INSTANCES : backed_by
    WORKFLOW_INSTANCES ||--o{ WORKFLOW_STEPS : checkpoints
    WORKFLOW_INSTANCES ||--o{ WORKFLOW_DLQ : parks
    MERCHANTS ||--o{ GATEWAY_CONNECTIONS : binds
    GATEWAYS ||--o{ GATEWAY_CONNECTIONS : provides
    GATEWAY_CONNECTIONS ||--o{ GATEWAY_CREDENTIALS_META : references
    GATEWAYS ||--o{ GATEWAY_HEALTH : reports
    MERCHANTS ||--|| CONFIGURATIONS : current
    CONFIGURATIONS ||--o{ CONFIGURATION_VERSIONS : appends
    MERCHANTS ||--o{ PAYMENTS : submits
    PAYMENTS ||--o{ PAYMENT_ATTEMPTS : has
    PAYMENTS ||--|| ROUTING_PLANS : decided_by
    PAYMENTS ||--o{ REFUNDS : reverses
    PAYMENT_ATTEMPTS ||--o{ INBOUND_WEBHOOKS : resolved_by
    PAYMENTS ||--o{ LEDGER_ENTRIES : posts
    LEDGER_ACCOUNTS ||--o{ LEDGER_ENTRIES : holds
    RECONCILIATION_RUNS ||--o{ RECONCILIATION_EXCEPTIONS : opens
    TENANTS ||--o{ AUDIT_RECORDS : chains

    TENANTS {
        string tenant_id PK
        string tier
        string residency_region
    }
    MERCHANTS {
        string merchant_id PK
        string tenant_id FK
        string state
    }
    PAYMENTS {
        string payment_id PK
        string tenant_id FK
        string merchant_id FK
        bigint amount_minor
        string currency
        string state
        int aggregate_version
        date partition_month
    }
    PAYMENT_ATTEMPTS {
        string attempt_id PK
        string payment_id FK
        string gateway_id FK
        string outcome
        string gateway_idempotency_key
        date partition_month
    }
    IDEMPOTENCY_RECORDS {
        string scope_hash PK
        string state
        string request_fingerprint
        timestamp lease_expires_at
    }
    LEDGER_ENTRIES {
        string ledger_entry_id PK
        string account_id FK
        bigint amount_minor
        string direction
    }
```

## Diagram C — Partitioning and the RLS boundary

```mermaid
flowchart TB
  APPROLE["Application role - NOT BYPASSRLS, this is the whole point"]
  SETL["SET LOCAL app.tenant_id at the start of every transaction"]
  RLSP["RLS policy USING tenant_id equals current_setting app.tenant_id"]
  NOCTX["No tenant in context - repository returns ErrMissingTenantContext, no query is issued"]
  CROSS["Query for tenant B under tenant A context returns zero rows at the database level"]
  TEST["TestCrossTenantAccessIsImpossible asserts exactly that"]

  subgraph PART["Range partitioning by partition_month"]
    PKEY["partition_month derived from the PAYMENT ULID timestamp"]
    PP["payments_2026_08, payments_2026_09, ..."]
    PA["payment_attempts_2026_08, payment_attempts_2026_09, ..."]
    ALIGN["Every attempt shares its payment partition, even if created weeks later"]
    IDX["Partial unique index on payment_id WHERE outcome equals SUCCESS - invariant I3"]
    PRUNE["Static partition pruning on point lookups, the key is a pure function of an immutable ID"]
  end

  subgraph MIG["Schema change discipline"]
    FWD["migrations are ordered and forward only, each with a down script"]
    EXP["expand then contract, never a blocking rewrite on a hot table"]
    LOCK["lock_timeout and statement_timeout set on every migration"]
  end

  APPROLE --> SETL --> RLSP
  APPROLE --> NOCTX
  RLSP --> CROSS --> TEST
  PKEY --> PP
  PKEY --> PA
  PP --> ALIGN
  PA --> ALIGN
  ALIGN --> IDX
  PKEY --> PRUNE
  FWD --> EXP --> LOCK
```

## Legend and notes

- **Payment writes never leave the regional writer.** They are CP: under partition the write is
  rejected with `503` rather than degraded. There are no cross-region writes, which is what makes
  active/passive the right posture for money movement (A4, A9, §15).
- **Reads are AP with read-your-writes for the caller.** `GET` is served from the reader endpoint
  and may be ≤ 1 s stale; a caller that just wrote carries a write token that forces its next read
  to the writer. Stale reads are acceptable; a caller not seeing its own write is not.
- **Amendment A-02 is the subtle one.** A partial unique index on a partitioned table is enforced
  only *within* a partition. If a payment's attempts could land in different monthly partitions —
  and they can, since an attempt may be created days later by delayed capture or reconciliation —
  invariant I3 would silently weaken to "at most one success per payment per month". Both tables
  are therefore partitioned on `partition_month := date_trunc('month', ids.TimeOf(payment_id))`,
  derived from the **payment's** ULID, so every attempt shares its payment's partition and the
  index constrains the full set.
- **ULIDs, not UUIDv4.** A 48-bit millisecond timestamp prefix gives time-ordered index locality
  in Postgres B-trees; UUIDv4 fragments them. The partition key being a pure function of an
  immutable ID also gives the planner static pruning on point lookups (§6).
- **RLS is defence in depth, not the only defence.** The order is: application guard (tenant from
  the token only, §16.2) → repository refusing to query without tenant context → RLS policy →
  an integration test that asserts cross-tenant reads return zero rows at the database level. The
  application role is deliberately not `BYPASSRLS`; a migration or admin role that is,
  is used only by `platformctl`.
- **`idempotency_records` is drawn without a relationship** because it is keyed by the scope tuple
  `(tenant_id, merchant_id, method, path_template, idempotency_key)` hash, not by a foreign key.
  Postgres is authoritative for it; Redis mirrors completed records purely for latency (§14.3).
- **Ledger tables are append-only.** There is no `UPDATE` path; a correction is a compensating
  entry. Retention is 7 years for payments and ledger, 7 years WORM for audit, 7 days for
  idempotency (§17.3).

## Related

- [Design baseline §6 identifiers, §9 invariants, §15 consistency, §16 multi-tenancy](../spec/00-design-baseline.md)
- [16 — AWS architecture](16-aws-architecture.md), [19 — Disaster recovery](19-disaster-recovery.md)
- [docs/multi-tenancy.md](../multi-tenancy.md), [docs/lld.md](../lld.md)
