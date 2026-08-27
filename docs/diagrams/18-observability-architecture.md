# 18 — Observability Architecture

## What this shows and why it matters

Telemetry from OTel SDK to collector agent to collector gateway to backends, the separate and
non-negotiable audit path, and the feedback loop that turns observed gateway behaviour back into
routing and control decisions. Observability is a plane in this platform, not a bolt-on, for two
reasons: the §22.1 mandatory context (`trace_id`, `tenant_id`, `merchant_id`, `payment_id`,
`gateway_id`, …) is what makes a single payment forensically reconstructible across nine binaries;
and `gateway.health_changed.v1` is a *control* signal, so the health pipeline has correctness
consequences, not just diagnostic ones.

## Diagram A — Telemetry pipeline and the audit path

```mermaid
flowchart LR
  subgraph APP["Every deployable"]
    SDK["OTel SDK - traces, metrics, logs with mandatory context"]
    MET["Prometheus registry - RED plus business metrics"]
    AUDW["Audit writer - hash-chained records"]
  end

  AGENT["OTel collector agent - DaemonSet, tail sampling buffer, resource detection"]
  GWC["OTel collector gateway - StatefulSet, batching, redaction processor, tenant attribution"]

  subgraph BACK["Backends"]
    TRACE["Trace backend - exemplar linked"]
    TSDB["Metrics - Prometheus remote write, long-term store"]
    LOGS["Log store - 30 d hot, 400 d archive"]
    DASH["Dashboards and SLO burn-rate alerts"]
    PAGE["Paging and ticketing"]
  end

  subgraph AUDIT["Audit path - separate, CP, never sampled"]
    OBXA["outbox_events in the same transaction as the audited change"]
    TAUD["pp.audit.v1 - 12 partitions, 400 d, key tenant_id"]
    AUDS["Audit sink - append only, hash chain verified on read"]
    S3A["S3 with Object Lock - 7 year WORM"]
    SIEM["Enterprise SIEM"]
  end

  SDK --> AGENT
  MET --> AGENT
  AGENT --> GWC
  GWC --> TRACE
  GWC --> TSDB
  GWC --> LOGS
  TSDB --> DASH --> PAGE
  TRACE -.->|"exemplars carry tenant_id and payment_id"| TSDB

  AUDW --> OBXA --> TAUD --> AUDS
  AUDS --> S3A
  TAUD --> SIEM
  GWC -.->|"redaction processor drops any unregistered attribute"| LOGS
```

## Diagram B — The feedback loop into control

```mermaid
flowchart TB
  CALL["Gateway adapter call outcome and latency"]
  WIN["Sliding health window per gateway and operation - 30 s, min 20 samples"]
  FSM["Health FSM - HEALTHY DEGRADED UNHEALTHY PROBING"]
  BRK["Circuit breaker state - CLOSED OPEN HALF_OPEN"]
  EVT["gateway.health_changed.v1 on pp.gateways.health.v1, compacted"]
  RTE["Routing engine - hard filter F6 and the health scoring weight"]
  CPR["Control plane records health for operator visibility"]
  ALRT["Alerting - authorization success rate drop, breaker open"]

  SLI1["SLI payment API availability 99.99 percent"]
  SLI2["SLI payment API p99 latency 250 ms"]
  SLI3["SLI authorization success rate, merchant baseline minus 5 points"]
  SLI4["SLI webhook processing lag p99 60 s"]
  SLI5["SLI config propagation p99 30 s"]
  BURN["Burn-rate alerts - 14.4x over 1 h pages, 6x over 6 h tickets"]
  EB["Error budget policy - 2x burn freezes features, 10x in 1 h triggers incident and rollback"]

  CFGAGE["pp_config_snapshot_age_seconds"]
  CLIFF["Alert past 5 min, fail closed for NEW merchants past max_config_staleness 15 min"]

  CALL --> WIN --> FSM --> BRK
  FSM --> EVT
  EVT --> RTE
  EVT --> CPR
  EVT --> ALRT
  SLI1 --> BURN
  SLI2 --> BURN
  SLI3 --> BURN
  SLI4 --> BURN
  SLI5 --> BURN
  BURN --> EB
  EB -.->|"gates CI"| DEPLOY["Deployment pipeline"]
  CFGAGE --> CLIFF
```

## Legend and notes

- **Cardinality is a design constraint, not a tuning detail.** `merchant_id` and `payment_id` are
  **never** metric labels — with 50 000 merchants they would produce unqueryable series counts.
  They live in logs, traces and metric *exemplars*, so a spike on a low-cardinality series links
  straight to a representative trace carrying the high-cardinality identity. CI runs a cardinality
  lint over the metric registry and a label set may not exceed 10⁴ series per metric per service
  (§22.3).
- **The audit path is deliberately not the telemetry path.** Audit records go through the same
  transactional outbox as domain events, are hash-chained, are never sampled, and land in S3 with
  Object Lock for 7-year WORM. Telemetry may be sampled and dropped under load; audit may not
  (§15, §17.3).
- **Redaction happens at the collector gateway as well as in the application.** The application's
  allowlist logger is the primary control; the gateway's redaction processor is the backstop that
  catches an attribute added by a library rather than by our code.
- **Tail sampling is buffered at the agent, and errors are always kept.** A 100 % sample of failed
  and slow payment traces plus a low base rate of successful ones gives the forensic coverage that
  matters at a cost that scales.
- **Diagram B is the only loop that runs upstream against the plane ordering, and it is
  intentional.** Health is observed in the data plane, published as a domain event, and consumed
  by routing (a correctness-affecting consumer) and by the control plane (a recording consumer).
  This is the platform's single closed control loop (§10).
- **`pp_config_snapshot_age_seconds` implements graceful degradation with a defined cliff.** Under
  5 minutes: normal. Past 5 minutes: alert. Past `max_config_staleness` (default 15 minutes): the
  data plane fails closed for **new** merchants while continuing to serve existing ones. Neither
  fail-open (kills compliance) nor fail-closed-immediately (kills revenue) — fail-static with a
  bound (§15).
- **The error budget policy is automated in CI**, not a wiki page: burn above 2× freezes features,
  above 10× in one hour triggers an incident and a rollback (§18).

## Related

- [Design baseline §10 gateway health, §15 fail-static, §22 observability contract, §18 SLOs](../spec/00-design-baseline.md)
- [09 — Gateway routing](09-gateway-routing.md), [12 — Event architecture](12-event-architecture.md), [17 — Security architecture](17-security-architecture.md)
- [docs/architecture.md](../architecture.md), [docs/runbooks](../runbooks)
