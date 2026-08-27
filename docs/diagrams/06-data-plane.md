# 06 — Data Plane

## What this shows and why it matters

The data plane is the money path: `payment-api`, `payment-orchestrator`, `webhook-ingress`,
`outbox-relay` and `event-consumer`, carrying BC-6 Payment Orchestration, the integration half of
BC-4, BC-7 Webhook Ingestion and BC-8 Ledger & Reconciliation at a 99.99 % availability target.
Diagram A shows the components and the bulkheads that stop one slow dependency from taking the
plane down. Diagram B is the **HTTP middleware chain exactly as `internal/transport/httpapi/middleware.New`
builds it** — fifteen stages, outermost first, pinned by `TestChainOrderMatchesBaselineSection12`.
Diagram C is the 17-stage §12 pipeline, which spans the whole payment operation and continues
past the chain into the handler and the orchestrator. Both orders are load-bearing —
authentication before tenant resolution, tenant resolution before rate limiting, the PAN scan
before authentication, the idempotency claim after every rejection stage, and the attempt row
written before the gateway call.

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

  subgraph ACL["BC-4 gateway integration - registry of 4 adapters over a shared httpx client"]
    BSTR["stripe - bulkhead and breaker per gateway and operation"]
    BADY["adyen - bulkhead and breaker"]
    BPPL["paypal - bulkhead and breaker"]
    BSIM["simulator - non-production, contract suite target"]
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
    SEC["Secrets provider - AWS Secrets Manager in production, file backed in sandbox"]
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
  RTE --> BSIM
  PORC -.->|"resolve credentials by secret reference at the moment of use"| SEC
  WHIG -.->|"current plus previous signing secret"| SEC
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

## Diagram B — The HTTP middleware chain as built

Fifteen stages, outermost first. This is `middleware.Names()` verbatim; the test that pins it
fails the build on a reordering.

```mermaid
flowchart TB
  M01["1 recover - 500, never a stack trace"]
  M02["2 requestid - correlation id before the first log line"]
  M03["3 tracing - span encloses authentication, route template as the label"]
  M04["4 logging - observes the status every later stage produces"]
  M05["5 metrics - RED series, unsampled"]
  M06["6 bodylimit - buffer the raw bytes under the ceiling, then L1 PAN scan"]
  M07["7 contenttype - application json, or merge-patch json on PATCH"]
  M08["8 cors - the preflight answer must precede authentication"]
  M09["9 securityheaders - set on rejected responses too"]
  M10["10 authn - JWT via JWKS, mTLS SPIFFE peer identity, or API key"]
  M11["11 tenant - tenant from the token only, plus the merchant scope guard"]
  M12["12 authz - RBAC plus ABAC from the method and route template"]
  M13["13 ratelimit - token bucket in Redis, local fallback"]
  M14["14 concurrency - adaptive in-flight limit plus priority shedding"]
  M15["15 idempotency - claim, innermost, immediately before the handler"]
  HDL["Handler - L1 schema decode, then L5, risk, routing, dispatch, L6, L7"]

  M01 --> M02 --> M03 --> M04 --> M05 --> M06 --> M07 --> M08 --> M09 --> M10 --> M11 --> M12 --> M13 --> M14 --> M15 --> HDL

  M06 -.->|"413 REQUEST_TOO_LARGE or 400 SENSITIVE_DATA_IN_REQUEST"| X["Terminate with problem+json"]
  M07 -.->|"415 UNSUPPORTED_MEDIA_TYPE"| X
  M10 -.->|"401 UNAUTHENTICATED"| X
  M11 -.->|"403 TENANT_MISMATCH plus security event"| X
  M12 -.->|"403 FORBIDDEN or DUAL_CONTROL_REQUIRED"| X
  M13 -.->|"429 RATE_LIMITED"| X
  M14 -.->|"503 shed by priority class"| X
  M15 -.->|"400 IDEMPOTENCY_KEY_REQUIRED, 409 IDEMPOTENT_REQUEST_IN_PROGRESS, 422 IDEMPOTENCY_KEY_REUSED"| X
  M15 -.->|"stored snapshot, Idempotent-Replay true"| RP["Replay the recorded response"]
```

## Diagram C — The 17-stage §12 pipeline

Stages 1–8 are the chain above; stages 9–17 run in the handler and the orchestrator. Numbers are
§12's, so the two diagrams can be read against each other.

```mermaid
flowchart TB
  S01["1 TLS, WAF, edge rate limit - outside the app"]
  S02["2 Request id and trace context - 1 ms"]
  S03["3 Authentication OAuth2 JWT or mTLS - 2 ms"]
  S04["4 Tenant resolution and isolation guard - 1 ms"]
  S05["5 Authorization RBAC and ABAC - 2 ms"]
  S06["6 Per-tenant rate limit and concurrency bulkhead - 2 ms"]
  S07["7 L1 - PAN scan in bodylimit above authn, schema decode in the handler below stage 8"]
  S08["8 Idempotency claim, Postgres authoritative - 8 ms"]
  S09["9 Merchant context load from cached config - 5 ms"]
  S10["10 L5 payment validation - 5 ms"]
  S11["11 Risk engine - 15 ms"]
  S12["12 Routing engine produces the routing plan - 5 ms"]
  S13["13 T1 - create attempt, bind connectionId, MarkProcessing, commit before dispatch - 10 ms"]
  S14["14 T2 - gateway adapter call under its own 8 s deadline"]
  S15["15 L6 response validation - 3 ms"]
  S16["16 T3 - L7 transition, attempt, audit and outbox in one transaction - 10 ms"]
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
- **L1 is split across two places in the implementation, and only half of it is above stage 8.**
  The PAN scan runs inside `bodylimit` (chain stage 6, above authentication), so a PAN-bearing
  request is rejected `400 SENSITIVE_DATA_IN_REQUEST` before it can reach a log, an authenticator
  or the idempotency fingerprint — stronger than §12's placement. The *schema decode*, however,
  runs in the handler, below the idempotency claim, so a syntactically invalid body does consume
  its key (the claim is settled with `FailTerminal`, and the 400 replays). §12 states the reverse;
  the divergence is recorded in this diagram rather than papered over.
- **Stage 13 before stage 14 is the single most important ordering in the platform.** `Orchestrator.attemptOnce`
  creates the attempt, binds `connectionId` to the merchant-to-gateway connection it is about to
  dispatch over, marks the payment `PROCESSING` and commits (**T1**) — all before `client.Authorize`
  (**T2**) is called. The attempt carries the deterministic
  `gateway_idempotency_key = base32(HMAC-SHA256(attempt_id ‖ operation, salt))[:32]`, so if the
  process dies mid-call the reconciler can find the attempt and look the transaction up at the
  gateway with the same key (§14.4).
- **`connectionId` is bound before the commit, not after.** Binding it afterwards would leave the
  exact crash window T1 exists to close: a charge at the gateway whose credential the record
  cannot name. The bind is best-effort and never fails a payment — a missing connection entry
  leaves the field blank rather than declining (migration `0016`,
  `payment_attempts.gateway_connection_id`).
- **Stage 16 is T3.** The attempt row, the payment's new state, the audit record and every outbox
  event the aggregate raised commit in one `UoW.Within`. There is no path in `orchestrator.go`
  that writes one without the others.
- **Stage 14 timeout does not fail the payment.** The attempt becomes `TIMEOUT_UNKNOWN`, the
  payment stays `PROCESSING`, and the client gets `202`-style semantics. Only a webhook, a
  reconciler lookup, or a settlement report may move it out. **No timer may fail a payment**
  (A7, §12.3) — auto-failing a timed-out authorization and retrying elsewhere is the most common
  cause of double charges in real platforms.
- **The dotted edges are exits, not the happy path.** Each is annotated with the exact error code
  from §20.2 so the diagram doubles as a reference for which stage produces which code.
- **`webhook-ingress` runs the chain in Diagram B but not the pipeline in Diagram C.** Its route
  is on the anonymous allowlist — the caller is a gateway holding a signature and no platform
  credential — so `authn`, `tenant` and `authz` pass it through and the handler verifies the
  gateway's own signature over the raw bytes `bodylimit` buffered. Its budget is ≤ 50 ms and all
  interpretation is asynchronous. See diagram 11.
- **The adapter set is four, not three.** `stripe`, `adyen`, `paypal` and `simulator` are the
  factories `registry.BuiltIn` returns; they share one `httpx` client with per-gateway pools and
  caps, speak the `spi` port, and are held to it by the `contract` suite. `simulator` is compiled
  in but only ever configured outside production.

## Related

- [Design baseline §12 pipeline, §12.3 timeout rule, §14.4 gateway idempotency, §5 deployables](../spec/00-design-baseline.md)
- [05 — Validation plane](05-validation-plane.md), [08 — Payment flow](08-payment-flow.md), [11 — Webhook flow](11-webhook-flow.md)
- [docs/data-plane.md](../data-plane.md), [docs/payment-flow.md](../payment-flow.md)
