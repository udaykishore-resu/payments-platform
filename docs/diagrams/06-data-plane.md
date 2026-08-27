# 06 — Data Plane

## What this shows and why it matters

The data plane is the money path: `payment-api`, `payment-orchestrator`, `webhook-ingress`,
`outbox-relay` and `event-consumer`, carrying BC-6 Payment Orchestration, the integration half of
BC-4, BC-7 Webhook Ingestion and BC-8 Ledger & Reconciliation at a 99.99 % availability target.
Diagram A shows the components and the bulkheads that stop one slow dependency from taking the
plane down. Diagram B shows the 17-stage request pipeline, whose **order is load-bearing** —
authentication before tenant resolution, tenant resolution before rate limiting, schema
validation before idempotency, idempotency before any side effect, and the attempt row written
before the gateway call.

## Diagram A — Components and bulkheads

```mermaid
flowchart TB
  LB["ALB and WAF"]

  subgraph INGRESS["Ingress tier - scales on connection count"]
    PAPI["payment-api - stateless"]
    WHIG["webhook-ingress - 50 ms budget, accept and persist only"]
  end

  subgraph ORCH["Orchestration tier - scales on in-flight gateway calls"]
    PORC["payment-orchestrator - owns the payment FSM"]
    RTE["Routing engine"]
    RSK["Risk engine"]
  end

  subgraph ACL["BC-4 gateway integration - anti-corruption layer"]
    BSTR["Bulkhead and breaker - Stripe"]
    BADY["Bulkhead and breaker - Adyen"]
    BPPL["Bulkhead and breaker - PayPal"]
  end

  subgraph ASYNC["Asynchronous tier"]
    ORLY["outbox-relay"]
    ECON["event-consumer"]
    RCN["reconciler"]
    LED["BC-8 ledger, append only"]
  end

  subgraph STATE["State"]
    PGW["Aurora writer - payments, attempts, idempotency, outbox"]
    PGR["Aurora readers"]
    RDS["Redis - idempotency mirror, token buckets, config snapshot"]
  end

  LB --> PAPI
  LB --> WHIG
  PAPI -->|"gRPC"| PORC
  PAPI -->|"GET reads"| PGR
  PAPI --> RDS
  PORC --> RSK --> RTE
  RTE --> BSTR
  RTE --> BADY
  RTE --> BPPL
  PORC --> PGW
  WHIG --> PGW
  PGW --> ORLY --> ECON
  ECON --> LED
  ECON --> PGW
  RCN -->|"resolves TIMEOUT_UNKNOWN attempts"| BSTR
  RCN --> PGW
  PGW -.-> PGR
  BSTR -.->|"health windows"| RTE
  BADY -.->|"health windows"| RTE
  BPPL -.->|"health windows"| RTE
```

## Diagram B — The 17-stage request pipeline

```mermaid
flowchart TB
  S01["1 TLS, WAF, edge rate limit - outside the app"]
  S02["2 Request id and trace context - 1 ms"]
  S03["3 Authentication OAuth2 JWT or mTLS - 2 ms"]
  S04["4 Tenant resolution and isolation guard - 1 ms"]
  S05["5 Authorization RBAC and ABAC - 2 ms"]
  S06["6 Per-tenant rate limit and concurrency bulkhead - 2 ms"]
  S07["7 L1 schema validation including PAN detector - 3 ms"]
  S08["8 Idempotency claim, Postgres authoritative - 8 ms"]
  S09["9 Merchant context load from cached config - 5 ms"]
  S10["10 L5 payment validation - 5 ms"]
  S11["11 Risk engine - 15 ms"]
  S12["12 Routing engine produces the routing plan - 5 ms"]
  S13["13 Create attempt, persist, dispatch - 10 ms"]
  S14["14 Gateway adapter call - 8 s hard timeout"]
  S15["15 L6 response validation - 3 ms"]
  S16["16 L7 transition plus outbox write in one transaction - 10 ms"]
  S17["17 Idempotency completion and response - 5 ms"]

  S01 --> S02 --> S03 --> S04 --> S05 --> S06 --> S07 --> S08 --> S09 --> S10 --> S11 --> S12 --> S13 --> S14 --> S15 --> S16 --> S17

  S03 -.->|"401 UNAUTHENTICATED"| F["Terminate with problem+json"]
  S04 -.->|"403 TENANT_MISMATCH plus security event"| F
  S05 -.->|"403 FORBIDDEN"| F
  S06 -.->|"429 RATE_LIMITED"| F
  S07 -.->|"400 VALIDATION_FAILED or SENSITIVE_DATA_IN_REQUEST"| F
  S08 -.->|"409 IDEMPOTENT_REQUEST_IN_PROGRESS or 422 IDEMPOTENCY_KEY_REUSED"| F
  S09 -.->|"404 or 409 MERCHANT_NOT_ACTIVE"| F
  S10 -.->|"422"| F
  S11 -.->|"422 RISK_DECLINED or force 3DS"| F
  S12 -.->|"503 NO_ELIGIBLE_GATEWAY"| F
  S14 -.->|"timeout - attempt TIMEOUT_UNKNOWN, payment stays PROCESSING"| U["202 semantics, status processing"]
  S15 -.->|"502 GATEWAY_CONTRACT_VIOLATION"| F
  S16 -.->|"409 INVALID_STATE_TRANSITION"| F
```

## Legend and notes

- **Bulkheads exist at three levels.** Deployment level (`payment-api` and `payment-orchestrator`
  are separate binaries so a slow gateway cannot consume the ingress connection pool, §5), tenant
  level (per-tenant concurrency limit at stage 6), and gateway level (a separate connection pool,
  semaphore and circuit breaker per gateway in the ACL). One unhealthy gateway degrades only its
  own share of traffic.
- **Stage 4 before stage 6 is deliberate.** Rate limits are per tenant and per merchant, so the
  tenant must be resolved from the *token* first. A `tenant_id` in the body is ignored, or, if it
  disagrees with the token, treated as a security event (§16.2).
- **Stage 7 before stage 8 is deliberate.** A malformed or PAN-bearing request must be rejected
  before it consumes an idempotency key, otherwise a client burns keys on requests that never had
  a chance of succeeding — and, worse, a PAN would reach the idempotency fingerprint.
- **Stage 13 before stage 14 is the single most important ordering in the platform.** The attempt
  row, with its deterministic `gateway_idempotency_key = base32(HMAC-SHA256(attempt_id, salt))`,
  is committed *before* the gateway is called. If the process dies mid-call, the reconciler can
  still find the attempt and look the transaction up at the gateway using that same key (§14.4).
- **Stage 14 timeout does not fail the payment.** The attempt becomes `TIMEOUT_UNKNOWN`, the
  payment stays `PROCESSING`, and the client gets `202`-style semantics. Only a webhook, a
  reconciler lookup, or a settlement report may move it out. **No timer may fail a payment**
  (A7, §12.3) — auto-failing a timed-out authorization and retrying elsewhere is the most common
  cause of double charges in real platforms.
- **Stage 16 is one transaction.** State change, event log append and outbox row commit together
  or not at all (§13.4).
- **The dotted edges are exits, not the happy path.** Each is annotated with the exact error code
  from §20.2 so the diagram doubles as a reference for which stage produces which code.
- **`webhook-ingress` does not appear in Diagram B** because it does not run this pipeline: it has
  a ≤ 50 ms accept-and-persist budget and all processing is asynchronous. See diagram 11.

## Related

- [Design baseline §12 pipeline, §12.3 timeout rule, §14.4 gateway idempotency, §5 deployables](../spec/00-design-baseline.md)
- [05 — Validation plane](05-validation-plane.md), [08 — Payment flow](08-payment-flow.md), [11 — Webhook flow](11-webhook-flow.md)
- [docs/data-plane.md](../data-plane.md), [docs/payment-flow.md](../payment-flow.md)
