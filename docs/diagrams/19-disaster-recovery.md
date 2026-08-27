# 19 — Disaster Recovery

## What this shows and why it matters

The multi-region topology, every replication path with its own RPO characteristic, and the ordered
failover sequence that meets RTO ≤ 15 min for a region loss and ≤ 60 s for an AZ loss. The posture
is active/passive for payment processing and active/active for the control plane (A9). The single
most important property in the failover sequence is that **payment writes stop before they can
split-brain**: the old primary is fenced before the secondary is promoted, because two Aurora
writers accepting payment writes for the same merchant is a class of damage that no amount of
subsequent reconciliation can fully repair.

## Diagram A — Topology and replication paths

```mermaid
flowchart TB
  subgraph RA["Region A - payments active, control active"]
    EKSA["EKS - full replica counts"]
    AURA["Aurora Global PRIMARY - accepts writes"]
    MSKA["MSK cluster A"]
    RDSA["ElastiCache A - ephemeral, not replicated"]
    S3A["S3 bucket A"]
    SMA["Secrets Manager A"]
  end

  subgraph RB["Region B - payments passive warm, control active"]
    EKSB["EKS - control full, data plane warm at reduced replicas"]
    AURB["Aurora Global SECONDARY - read only until promoted"]
    MSKB["MSK cluster B"]
    RDSB["ElastiCache B - cold, rebuilt from Postgres and Kafka"]
    S3B["S3 bucket B"]
    SMB["Secrets Manager B - replicated secrets"]
  end

  R53["Route 53 - health checks on /readyz, failover records, 30 s TTL"]

  R53 -->|"payments"| EKSA
  R53 -.->|"payments after promotion only"| EKSB
  R53 -->|"control plane, both regions"| EKSB

  AURA -->|"storage level, asynchronous, RPO under 1 s typical and 5 s budgeted"| AURB
  MSKA -->|"MirrorMaker 2, asynchronous, offsets translated"| MSKB
  S3A -->|"Cross-Region Replication, minutes"| S3B
  SMA -->|"Secrets Manager multi-region replicas"| SMB
  SYNCA["In-region 6-way replication across 3 AZs, synchronous commit, RPO 0"]
  AURA -.-> SYNCA
  RDSA -.->|"NOT replicated by design"| RDSB
  EKSA --> AURA
  EKSB -->|"reads only"| AURB
```

## Diagram B — Region failover sequence

```mermaid
sequenceDiagram
    autonumber
    participant HC as Route 53 health checks
    participant OC as On-call and incident commander
    participant PC as platformctl DR runbook
    participant RA as Region A control
    participant AUR as Aurora Global
    participant RB as Region B EKS
    participant DNS as Route 53 records
    participant CL as Merchant clients
    participant RC as Reconciler

    HC->>OC: region A /readyz failing across all AZs, page
    OC->>PC: declare region failover, start the clock, RTO budget 15 min
    PC->>RA: fence the old primary, revoke the application role and drain ALB targets
    Note over RA: fencing precedes promotion, two writers on financial state is unrecoverable
    PC->>AUR: promote the region B secondary to primary
    AUR-->>PC: promotion complete, writes now accepted in region B
    PC->>RB: scale the data plane from warm to full replicas
    RB->>RB: config cache rebuilds from the compacted pp.config.configuration.v1
    RB->>RB: idempotency and payment state read directly from the promoted cluster
    RB-->>PC: /readyz green
    PC->>DNS: flip the payments failover record to region B
    DNS-->>CL: 30 s TTL, clients follow
    CL->>RB: payments resume
    PC->>RC: run reconciliation for the replication gap window
    RC->>RC: replay outbox rows not yet published, resolve TIMEOUT_UNKNOWN attempts against the gateways
    RC-->>OC: exceptions opened for anything unresolved
    OC->>OC: declare recovery, record actual RPO and RTO against the drill baseline
```

## Diagram C — Failure scope and response

```mermaid
flowchart LR
  AZ["Single AZ loss"]
  NODE["Node loss"]
  PG["Aurora primary instance loss, region healthy"]
  REDIS["Redis cluster loss"]
  KAFKA["MSK unavailable"]
  REG["Full region loss"]

  RAZ["Multi-AZ everywhere plus 3x capacity headroom, no action, RTO 60 s"]
  RNODE["PDB, anti-affinity, surge, connections drained, brief latency blip"]
  RPG["Aurora automatic failover under 60 s, readiness fails, LB sheds, writes reject 503 meanwhile"]
  RREDIS["Fall back to Postgres for idempotency and a local token bucket for rate limits, latency up only"]
  RKAFKA["Outbox retains rows and backs off, zero data loss, pp_outbox_backlog alerts"]
  RREG["Diagram B sequence, RTO 15 min, RPO 5 s budgeted"]

  DRILL["platformctl dr-drill - region-failover, writer-failover or restore, measured not assumed"]

  AZ --> RAZ
  NODE --> RNODE
  PG --> RPG
  REDIS --> RREDIS
  KAFKA --> RKAFKA
  REG --> RREG
  RAZ --> DRILL
  RPG --> DRILL
  RREG --> DRILL
```

## Legend and notes

- **Fencing before promotion is the whole sequence in one step.** Steps 3 and 4 are ordered that
  way deliberately: revoke the application role and drain the load balancer in region A *first*, so
  that a partially-reachable region A cannot keep accepting payment writes after region B is
  promoted. This is the operational expression of A4 — under partition, refuse the write.
- **Redis is deliberately not replicated.** It holds only a mirror of completed idempotency
  records, token buckets and a configuration snapshot, all of which are rebuildable from Postgres
  and from the compacted Kafka topic. Replicating it would add a failure mode and a cost for no
  correctness gain (§14.3).
- **The warm secondary exists because a cold start cannot make 15 minutes.** Reduced-replica pods
  keep images pulled, connection pools alive and the configuration cache populated from the
  compacted topic. Scaling warm to full is a minutes operation; scaling from zero is not.
- **The replication gap is real and is reconciled, not ignored.** Aurora Global replication is
  asynchronous, so up to the RPO budget of transactions may exist in the failed region's storage
  and not the promoted one. Step 13 replays unpublished outbox rows and resolves ambiguous
  attempts against the gateways using the deterministic gateway idempotency key derived from
  `attempt_id` — which is precisely why that key is derived rather than random (§14.4).
- **Payments that were `PROCESSING` at the moment of failure stay `PROCESSING`.** No failover step
  may fail a payment; the reconciler resolves them from the gateway's own record (§12.3).
- **The control plane needs no failover**, because it is already active in both regions and its
  reads are served locally from the compacted configuration topic. Only its *writes* follow the
  Aurora primary.
- **RPO and RTO are drill-measured numbers, not aspirations.** `platformctl dr-drill` takes one of
  three scenarios — `region-failover`, `writer-failover`, `restore` — and records actual figures
  against the §18 targets; a drill that misses the target is a defect with an owner, not a note
  (§18, §27).
- **Step 13's two halves have different maturity.** Replaying unpublished outbox rows is
  `outbox-relay` doing its ordinary job against the promoted writer, and needs no new machinery.
  Resolving ambiguous attempts against the gateways is `internal/application/payment.Reconciler`,
  which is implemented and tested but is not yet constructed by any binary — so today that half of
  the gap-closing is an operator following the runbook rather than a process doing it. The step is
  drawn because the sequence is not correct without it.

## Related

- [Design baseline §15 consistency, §18 RPO and RTO, §24 failure mode catalog](../spec/00-design-baseline.md)
- [14 — Database architecture](14-database-architecture.md), [16 — AWS architecture](16-aws-architecture.md)
- [docs/failure-handling.md](../failure-handling.md), [docs/runbooks](../runbooks)
