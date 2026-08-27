# 00 — Canonical Design Baseline

> **Status:** Accepted · **Applies to:** every artifact in this repository
>
> This document is the single source of truth. Specifications (`docs/spec/`), architecture
> documents (`docs/*.md`), API contracts (`api/`), code (`internal/`, `cmd/`), and tests
> (`tests/`) are all *derived* from this file. If any of them disagrees with this document,
> this document wins and the other artifact is a defect.
>
> Nothing in this repository may be implemented before the corresponding section here exists.
> That is what "spec-driven" means operationally.

---

## 1. Scope, non-goals, and the ambiguity register

### 1.1 What this platform is

A **multi-tenant payment gateway onboarding and payment orchestration platform**. Two distinct
products share one codebase and one control surface:

1. **Onboarding** — take a merchant from "signed up" to "processing live money" through an
   automated, resumable, auditable workflow that includes KYC/KYB, bank validation, gateway
   provisioning, configuration, sandbox validation and certification.
2. **Orchestration** — accept payment instructions from onboarded merchants and execute them
   against one of several third-party payment gateways, with routing, failover, idempotency,
   webhook reconciliation and a ledger.

### 1.2 What this platform is explicitly not

| Not this | Why | What we do instead |
|---|---|---|
| An acquirer or card scheme member | We do not hold a licence, we do not settle with schemes | We orchestrate licensed gateways (Stripe, Adyen, PayPal) |
| A card data vault | Storing PAN drags the whole platform into PCI DSS SAQ-D | PAN never enters our services; see §17 |
| A KYC/KYB decision engine | Identity verification is a regulated, vendor-supplied capability | We integrate a KYC provider port and own the *workflow*, not the *decision* |
| A general-purpose BPM engine | Scope discipline | A purpose-built durable saga engine behind a port, plus a Temporal adapter |
| A fraud-scoring ML system | Different problem, different team, different data | A risk *policy* engine with a port for an external scorer |

### 1.3 Ambiguity register

The brief left the following underspecified. Each is resolved here with a production-grade
default and a rationale. **These are decisions, not guesses — they are testable and reversible.**

| # | Ambiguity | Decision | Rationale |
|---|---|---|---|
| A1 | Does the platform take custody of funds? | **No.** Funds settle gateway → merchant. We are a technical orchestrator and system of record for *instructions and outcomes*, not a payment institution holding client money. | Avoids e-money licensing, safeguarding, and client-money segregation. Ledger is a *shadow* ledger for reconciliation, not a money-custody ledger. |
| A2 | Does the API accept raw card data? | **No.** Only gateway-issued tokens / network tokens. Requests containing PAN-like data are rejected at the edge. | Keeps 8 of 9 services out of the CDE. §17. |
| A3 | Tenancy model | **Pooled by default with row-level security; siloed schema available per tenant tier.** | Pooled is the only model that scales to thousands of merchants economically; silo exists for tenants with contractual isolation requirements. §16. |
| A4 | Consistency for money state | **Strongly consistent (CP).** A payment write that cannot reach the regional primary fails closed. | Under partition, refusing a payment costs one lost sale; double-charging costs a chargeback, a fine, and trust. §15. |
| A5 | Consistency for configuration reads | **Eventually consistent with bounded staleness (≤ 30 s), plus push invalidation.** | The data plane must not have a synchronous dependency on the control plane. §15. |
| A6 | Concurrent duplicate idempotent requests | **Second caller receives `409 IDEMPOTENT_REQUEST_IN_PROGRESS` with `Retry-After`.** We do not block, and we do not process twice. | Blocking a request thread on a lease held by another process is how thread pools die under retry storms. §14. |
| A7 | Gateway call timeout with no response | **The payment stays in `PROCESSING`; the attempt is marked `UNKNOWN`; a reconciler resolves it.** We never auto-fail on timeout. | Auto-failing a timed-out authorization and retrying elsewhere is the single most common cause of double charges in real platforms. §12.3. |
| A8 | Delivery semantics | **At-least-once delivery, effectively-once *business* effect** via idempotent consumers + a dedup table + database-enforced invariants. | Exactly-once delivery is not achievable across process/broker boundaries; exactly-once *effect* is, and is what the business actually needs. §13.5. |
| A9 | Multi-region posture | **Active/passive per payment-processing region with an active/active control plane.** | Active/active money movement requires a global consensus store or conflict resolution on financial state; the risk is not justified at this scale. §18. |
| A10 | Who owns the gateway idempotency key? | **The orchestrator**, derived deterministically from `attempt_id`. Never the client's key. | The client key scopes *our* API; each gateway attempt needs its own stable key so a network retry to the same gateway is safe while a failover to another gateway is a genuinely new attempt. §14.4. |
| A11 | Certification definition | **A machine-checked suite of sandbox transactions per (gateway, payment method, currency) that must pass before `PRODUCTION_READY`.** | Makes "certified" an artifact, not an opinion. §11.4. |
| A12 | Settlement | **Ingested from gateway settlement reports/webhooks and reconciled; we do not compute settlement.** | See A1. |

---

## 2. Ubiquitous language

Terms are used with exactly these meanings in code, docs, APIs and events. Deviating names are
a review-blocking defect.

| Term | Definition |
|---|---|
| **Tenant** | The top-level isolation boundary. A platform customer (typically a PSP, marketplace or ISV) that owns merchants. Every row, key, event and log line belongs to exactly one tenant. |
| **Merchant** | A business onboarded under a tenant that submits payment instructions. Identified by `merchant_id`, unique within a tenant. |
| **Onboarding Case** | The stateful record of one merchant's journey from `CREATED` to `ACTIVE`. Backed by exactly one workflow instance. |
| **Gateway** | A third-party payment processor (Stripe, Adyen, PayPal). Described by a *capability descriptor*, reached through an *adapter*. |
| **Gateway Connection** | The binding of one merchant to one gateway: provisioned account reference, credential reference, webhook registration, certification status. |
| **Capability Descriptor** | Declarative statement of what a gateway supports: countries, currencies, payment methods, operations, 3DS, partial capture, refund window, webhook signature scheme. |
| **Payment** | The merchant's *intent* to move money. Immutable in amount/currency/merchant after creation. Has exactly one lifecycle state. |
| **Payment Attempt** | One execution of that intent against one gateway. A payment has 1..N attempts. Failover creates a new attempt; it never mutates the old one. |
| **Authorization** | A hold on funds. Reversible by *void*. |
| **Capture** | Conversion of a hold into a debit. Reversible by *refund*. |
| **Settlement** | Gateway-reported movement of captured funds to the merchant. Observed, not performed, by us. |
| **Routing Plan** | An ordered, reason-annotated list of candidate gateways produced for one payment at one instant. Persisted with the payment for auditability. |
| **Idempotency Key** | A client-supplied opaque string scoping one *logical operation* to one *effect*, within `(tenant, merchant, endpoint)`. |
| **Desired State** | Configuration as declared through the control plane. Versioned, validated, auditable. |
| **Actual State** | Configuration and provisioning as it exists in the world (gateway accounts, webhooks, credentials). |
| **Reconciliation** | The process of detecting and closing the gap between desired and actual state, or between our payment state and the gateway's. |
| **Plane** | A horizontal slice of the platform with its own availability target, scaling behaviour and blast radius: Control, Automation, Validation, Data, Observability. |

---

## 3. Bounded contexts and the context map

Nine bounded contexts. Each owns its data, publishes its events, and is reachable only through
its published interface.

```
┌───────────────────────── CONTROL PLANE ──────────────────────────┐
│  Tenant &        Merchant        Gateway         Configuration   │
│  Identity        Registry        Registry        & Policy        │
└──────────────────────────────────────────────────────────────────┘
                              │ publishes config versions (events)
                              ▼
┌───────────────────── AUTOMATION PLANE ───────────────────────────┐
│  Onboarding Orchestration (durable sagas, compensation, gates)   │
└──────────────────────────────────────────────────────────────────┘
                              │ provisions / certifies
                              ▼
┌──────────────────────── DATA PLANE ──────────────────────────────┐
│  Payment Orchestration   Routing   Risk   Gateway Integration    │
│  Webhook Ingestion       Ledger & Reconciliation                 │
└──────────────────────────────────────────────────────────────────┘
                              │ emits domain + audit events
                              ▼
┌───────────────────── OBSERVABILITY PLANE ────────────────────────┐
│  Telemetry, Audit Trail, Health/Feedback into Control            │
└──────────────────────────────────────────────────────────────────┘
```

| # | Bounded context | Aggregates | Owns tables | Plane | Relationship |
|---|---|---|---|---|---|
| BC-1 | **Tenant & Identity** | `Tenant`, `ApiClient`, `Principal` | `tenants`, `api_clients`, `roles`, `role_bindings` | Control | Upstream to all (shared kernel: `TenantID`) |
| BC-2 | **Merchant Registry** | `Merchant` | `merchants`, `merchant_business_profile`, `merchant_bank_accounts` | Control | Customer of BC-1; supplier to BC-3, BC-5 |
| BC-3 | **Onboarding** | `OnboardingCase`, `WorkflowInstance` | `onboarding_cases`, `workflow_instances`, `workflow_steps`, `workflow_dlq` | Automation | Conformist to BC-2; ACL to external KYC/bank vendors |
| BC-4 | **Gateway Registry & Integration** | `Gateway`, `GatewayConnection`, `GatewayHealth` | `gateways`, `gateway_connections`, `gateway_credentials_meta`, `gateway_health` | Control (registry) + Data (integration) | Anti-corruption layer around every external gateway |
| BC-5 | **Configuration & Policy** | `MerchantConfiguration`, `RoutingPolicy`, `RiskPolicy`, `CompliancePolicy`, `FeatureFlag` | `configurations`, `configuration_versions`, `policies`, `feature_flags` | Control | Published-language supplier to Data Plane |
| BC-6 | **Payment Orchestration** | `Payment`, `PaymentAttempt`, `Refund` | `payments`, `payment_attempts`, `refunds`, `routing_plans`, `idempotency_records` | Data | Customer of BC-5 (cached config), BC-4 (adapters) |
| BC-7 | **Webhook Ingestion** | `InboundWebhook` | `inbound_webhooks`, `webhook_dedup` | Data | ACL translating gateway payloads into domain events |
| BC-8 | **Ledger & Reconciliation** | `LedgerEntry`, `ReconciliationRun` | `ledger_entries`, `ledger_accounts`, `reconciliation_runs`, `reconciliation_exceptions` | Data | Downstream event consumer; strictly append-only |
| BC-9 | **Audit** | `AuditRecord` | `audit_records` (hash-chained) | Observability | Downstream of everything; write-only from the app's perspective |

**Context map patterns in use**

- *Shared Kernel* — `internal/domain/shared` (IDs, Money, Currency, Country, Clock, DomainError). Deliberately tiny; changes require review from every context owner.
- *Anti-Corruption Layer* — every gateway adapter (`internal/adapters/gateway/*`) and every external vendor client. No gateway type ever appears in `internal/domain`.
- *Published Language* — the versioned event envelope (§13) and the OpenAPI contracts (§19).
- *Customer/Supplier* — Data Plane is a customer of Configuration; the supplier may not break the customer's contract without a new major event version.
- *Conformist* — Onboarding conforms to Merchant Registry's model rather than maintaining a translation.

---

## 4. Layering and the dependency rule

Clean/Hexagonal architecture, enforced mechanically (`scripts/check-architecture.sh`, wired into CI).

```
        ┌──────────────────────────────────────────────┐
        │  cmd/*            composition roots only     │
        └───────────────────┬──────────────────────────┘
                            │ wires
        ┌───────────────────▼──────────────────────────┐
        │  internal/infrastructure, internal/adapters  │  driven + driving adapters
        └───────────────────┬──────────────────────────┘
                            │ implements
        ┌───────────────────▼──────────────────────────┐
        │  internal/application (use cases + ports)    │
        └───────────────────┬──────────────────────────┘
                            │ depends on
        ┌───────────────────▼──────────────────────────┐
        │  internal/domain (entities, VOs, FSMs)       │  ← imports nothing but stdlib
        └──────────────────────────────────────────────┘
```

**The rule, stated as CI-checkable constraints:**

| Package | May import | May **not** import |
|---|---|---|
| `internal/domain/**` | stdlib, `internal/domain/**`, `pkg/**` | anything else — *no* `database/sql`, `net/http`, otel, AWS, uuid libraries |
| `internal/application/**` | stdlib, `internal/domain/**`, `internal/application/ports`, `internal/adapters/gateway/spi` † | `internal/infrastructure/**`, `internal/adapters/**` other than `spi`, any driver |
| `internal/validation/**`, `internal/workflows/engine` | stdlib, `domain`, `application/ports` | infrastructure |
| `internal/infrastructure/**`, `internal/adapters/**` | anything | other adapters' internals |
| `cmd/**` | anything | business logic (composition only) |
| `pkg/**` | stdlib only | everything internal |

† **The SPI exception.** `internal/adapters/gateway/spi` is a *port declaration*, not an adapter:
it contains only interfaces and value types and imports nothing outside the standard library,
`internal/domain/**` and `pkg/**`. It lives under `adapters/` for discoverability — next to the
implementations that satisfy it — rather than in `application/ports`, where a reader looking for
"how do we talk to Stripe" would not think to look. Duplicating its twenty-odd request and
result types into `ports` to satisfy a directory naming convention would add a translation layer
whose only job is to prove a rule was followed. The exception is narrow and is mechanically
enforced: `scripts/check-architecture.sh` asserts both that `spi` imports nothing forbidden, and
that no *other* package under `internal/adapters/` is imported from `internal/application/`.

Rationale for `pkg` being stdlib-only: it is the part of this repo that could be extracted and
published; a dependency there is a dependency imposed on every future consumer (KISS + DRY
without creating a distributed monolith of shared code).

---

## 5. Deployable units

Nine binaries, deliberately split along blast-radius and scaling-behaviour lines, **not** along
"one service per table".

| Binary | Plane | Scaling driver | Availability target | Notes |
|---|---|---|---|---|
| `control-plane-api` | Control | Admin request rate (low) | 99.9 % | REST+gRPC. Writes desired state. Never on the payment hot path. |
| `payment-api` | Data | Payment TPS | 99.99 % | The only public money-path ingress. Stateless. |
| `payment-orchestrator` | Data | Payment TPS | 99.99 % | Owns the payment FSM and gateway calls. Bulkheaded per gateway. |
| `workflow-worker` | Automation | Onboarding volume (low) + retry backlog | 99.9 % | Leases workflow instances; runs activities and compensations. |
| `webhook-ingress` | Data | Gateway webhook volume (spiky) | 99.99 % | Accept-and-persist only; ≤ 50 ms budget. Processing is asynchronous. |
| `outbox-relay` | Data | Outbox backlog | 99.99 % | Postgres → Kafka. The only publisher. |
| `event-consumer` | Data | Kafka lag | 99.9 % | Projections, ledger, audit, notifications. |
| `gateway-simulator` | Test only | — | — | Never deployed to production; `//go:build` guarded from prod images. |
| `platformctl` | Ops | — | — | Migrations, config validation, certification runs, DR drills. |

`payment-api` and `payment-orchestrator` are separate binaries so that a slow gateway cannot
consume the ingress connection pool, and so that ingress can scale on connection count while
orchestration scales on in-flight gateway calls (bulkhead at the deployment level).

---

## 6. Identifier scheme

Prefixed, lexicographically sortable, collision-resistant, non-guessable, and self-describing
in logs.

```
<prefix>_<26-char Crockford Base32 ULID>
pay_01JB8Z9K2QW3E4R5T6Y7U8I9O0
```

| Entity | Prefix | Entity | Prefix |
|---|---|---|---|
| Tenant | `ten_` | Payment | `pay_` |
| API client | `cli_` | Payment attempt | `att_` |
| Merchant | `mrc_` | Refund | `ref_` |
| Onboarding case | `onb_` | Routing plan | `rpl_` |
| Workflow instance | `wfr_` | Ledger entry | `led_` |
| Workflow step | `wfs_` | Inbound webhook | `whk_` |
| Gateway | `gw_` | Domain event | `evt_` |
| Gateway connection | `gwc_` | Audit record | `aud_` |
| Configuration version | `cfv_` | Reconciliation run | `rcn_` |

Properties: ULID gives 48-bit millisecond timestamp + 80 bits of entropy → time-ordered index
locality in Postgres (unlike UUIDv4, which fragments B-trees), 1.21e24 IDs per millisecond
before a 50 % collision chance, and no PII. IDs are **opaque to clients**; no client may parse
them. Implemented once in `pkg/ids`.

---

## 7. Money

```go
type Money struct {  // value object, immutable
    amount   int64   // MINOR units, always
    currency Currency
}
```

Rules, enforced by the type system and by tests:

1. **No floating point anywhere in the money path.** Ever. `float64` cannot represent 0.10.
2. Amounts are **minor units** (cents, pence, fils). Exponent comes from an embedded ISO 4217
   table: `USD`=2, `JPY`=0, `BHD`=3, `CLF`=4.
3. Arithmetic on differing currencies returns `ErrCurrencyMismatch` — it is not a panic and not
   a silent conversion.
4. Splitting (e.g. multi-capture, fee allocation) uses **largest-remainder allocation** so the
   parts always sum to the whole.
5. Serialization on the wire is `{"amount": 1050, "currency": "USD"}` — integer minor units.
   Never a decimal string, never a float.
6. Negative amounts are legal only for ledger credit entries; the API rejects `amount <= 0`.

---

## 8. Merchant lifecycle state machine

```
                         ┌──────────────────────────────────────┐
                         ▼                                      │
CREATED ──► VALIDATING ──► KYC_PENDING ──► KYC_APPROVED ──► BANK_VALIDATED
   │            │              │                                │
   │            ▼              ▼                                ▼
   │   VALIDATION_FAILED   KYC_FAILED              BANK_VALIDATION_FAILED
   │
   └──────────────────────────────────────────────────────────────┐
                                                                  ▼
BANK_VALIDATED ──► GATEWAY_PROVISIONING ──► CONFIGURING ──► SANDBOX_VALIDATION
                            │                    │                 │
                            ▼                    ▼                 │
                   PROVISIONING_FAILED   CONFIGURATION_FAILED       │
                                                                    ▼
                              CERTIFICATION ──► APPROVED ──► PRODUCTION_READY ──► ACTIVE
                                    │                                                │
                                    ▼                                                ▼
                          CERTIFICATION_FAILED                          SUSPENDED ⇄ ACTIVE
                                                                                     │
                                                                                     ▼
                                                                                TERMINATED
```

**Transition table** (`from → {to}`); anything absent is rejected with `INVALID_STATE_TRANSITION`.

| From | Allowed to |
|---|---|
| `CREATED` | `VALIDATING`, `TERMINATED` |
| `VALIDATING` | `KYC_PENDING`, `VALIDATION_FAILED` |
| `VALIDATION_FAILED` | `VALIDATING` (after correction), `TERMINATED` |
| `KYC_PENDING` | `KYC_APPROVED`, `KYC_FAILED` |
| `KYC_FAILED` | `KYC_PENDING` (resubmission), `TERMINATED` |
| `KYC_APPROVED` | `BANK_VALIDATED`, `BANK_VALIDATION_FAILED` |
| `BANK_VALIDATION_FAILED` | `KYC_APPROVED` (retry with new account), `TERMINATED` |
| `BANK_VALIDATED` | `GATEWAY_PROVISIONING` |
| `GATEWAY_PROVISIONING` | `CONFIGURING`, `PROVISIONING_FAILED` |
| `PROVISIONING_FAILED` | `GATEWAY_PROVISIONING`, `TERMINATED` |
| `CONFIGURING` | `SANDBOX_VALIDATION`, `CONFIGURATION_FAILED` |
| `CONFIGURATION_FAILED` | `CONFIGURING`, `TERMINATED` |
| `SANDBOX_VALIDATION` | `CERTIFICATION`, `CONFIGURATION_FAILED` |
| `CERTIFICATION` | `APPROVED`, `CERTIFICATION_FAILED`, `COMPLIANCE_REJECTED` |
| `CERTIFICATION_FAILED` | `CERTIFICATION`, `CONFIGURING`, `TERMINATED` |
| `COMPLIANCE_REJECTED` | `CONFIGURING`, `KYC_PENDING`, `TERMINATED` |
| `APPROVED` | `PRODUCTION_READY`, `SUSPENDED` |
| `PRODUCTION_READY` | `ACTIVE`, `SUSPENDED` |
| `ACTIVE` | `SUSPENDED`, `TERMINATED` |
| `SUSPENDED` | `ACTIVE`, `TERMINATED` |
| `TERMINATED` | — (terminal) |

**Amendment A-01 (`COMPLIANCE_REJECTED`).** The original lifecycle had no exit from the manual
compliance gate (onboarding step 11) other than approval, which made a compliance officer's
rejection unrepresentable — the workflow would have had to either lie (`CERTIFICATION_FAILED`,
blaming the integration for a policy decision) or hang. `COMPLIANCE_REJECTED` is a distinct,
non-terminal state carrying the reviewer's reason code; it routes back to `CONFIGURING` (fixable
configuration, e.g. a prohibited MCC/country combination) or `KYC_PENDING` (fixable evidence) or
forward to `TERMINATED`. `APPROVED → SUSPENDED` is added for the same reason: an adverse finding
between approval and activation must be expressible without terminating the merchant.

Guards worth stating explicitly:

- `→ ACTIVE` requires: at least one `GatewayConnection` in `CERTIFIED`, a non-empty validated
  `MerchantConfiguration`, a completed compliance attestation, and no open critical
  reconciliation exception.
- `ACTIVE → SUSPENDED` is available to an operator *and* to the automation plane (risk breach,
  compliance expiry, gateway de-provisioning). Suspension **rejects new payments** but
  **permits refunds, voids and webhook processing** — you must always be able to give money
  back.
- `→ TERMINATED` requires zero payments in a non-terminal state.

---

## 9. Payment state machine

The brief's transitions are a subset of these. The additions exist because a real payment
system has to represent *asynchronous* and *unknown* outcomes; without them, a timeout forces a
lie.

```
                    ┌─────────► REQUIRES_ACTION ──────┐  (3DS / redirect)
                    │                 │                │
CREATED ────────────┼─────────────────┼────────────────▼
                    │                 │            PROCESSING ──────► AUTHORIZED ──► CAPTURED
                    │                 ▼                 │                 │             │
                    └──────────► CANCELED           PENDING               │             │
                                     ▲              (async/unknown)       │             │
                                     │                  │                 │             ▼
                                  FAILED ◄──────────────┘                 │          SETTLED
                                                                          ▼             │
                                                                       VOIDED           │
                                                                                        ▼
                                          PARTIALLY_REFUNDED ◄──────────────────► REFUNDED
                                                                                        │
                                                                                        ▼
                                                                                    DISPUTED
```

**Transition table.**

| From | Allowed to | Trigger |
|---|---|---|
| `CREATED` | `PROCESSING`, `REQUIRES_ACTION`, `FAILED`, `CANCELED` | orchestrator dispatch / pre-flight rejection |
| `REQUIRES_ACTION` | `PROCESSING`, `FAILED`, `CANCELED`, `EXPIRED` | customer completes or abandons 3DS |
| `PROCESSING` | `AUTHORIZED`, `CAPTURED`, `PENDING`, `FAILED`, `REQUIRES_ACTION` | gateway response (`CAPTURED` directly for sale/auto-capture methods) |
| `PENDING` | `AUTHORIZED`, `CAPTURED`, `FAILED`, `EXPIRED` | asynchronous method (bank debit, voucher) or reconciler resolving an `UNKNOWN` attempt |
| `AUTHORIZED` | `CAPTURED`, `VOIDED`, `EXPIRED`, `FAILED` | capture / void / auth expiry |
| `CAPTURED` | `SETTLED`, `PARTIALLY_REFUNDED`, `REFUNDED`, `DISPUTED` | settlement report / refund / chargeback |
| `SETTLED` | `PARTIALLY_REFUNDED`, `REFUNDED`, `DISPUTED` | refund after settlement is the *normal* case |
| `PARTIALLY_REFUNDED` | `PARTIALLY_REFUNDED`, `REFUNDED`, `DISPUTED` | further refunds up to captured amount |
| `REFUNDED` | `DISPUTED` | terminal for money-out except disputes |
| `DISPUTED` | `REFUNDED`, `CAPTURED`, `SETTLED` | dispute lost (funds reversed) / won (funds restored) |
| `FAILED`, `CANCELED`, `VOIDED`, `EXPIRED` | — | terminal |

**Explicitly invalid** (the brief's examples, plus the ones that actually bite):
`SETTLED → PROCESSING`, `REFUNDED → CAPTURED`, `CAPTURED → AUTHORIZED`, `FAILED → *`,
`CREATED → CAPTURED` (must pass through `PROCESSING`), any transition that would make
`refunded_total > captured_total`.

**Aggregate invariants** (enforced in the domain *and* by database constraints):

- **I1** — `sum(refunds.amount) ≤ captured_amount`. DB: `CHECK` + serialized update.
- **I2** — `captured_amount ≤ authorized_amount` for two-step flows.
- **I3** — At most **one** `PaymentAttempt` per payment may be in a successful terminal state
  (`AUTHORIZED`/`CAPTURED`). DB: partial unique index on `(payment_id) WHERE outcome='SUCCESS'`.
  *This is the constraint that makes double-charging structurally impossible rather than
  merely unlikely.*
  **Amendment A-02 (partition alignment).** `payments` and `payment_attempts` are range-partitioned
  by month. A partial unique index on a partitioned table is only enforced *within* a partition, so
  I3 would silently weaken if a payment's attempts could land in different months — which they can,
  since an attempt may be created days after the payment (delayed capture, reconciliation). Both
  tables are therefore partitioned on a `partition_month` column derived from the **payment's**
  ULID timestamp (`partition_month := date_trunc('month', ids.TimeOf(payment_id))`), so every
  attempt of a payment shares that payment's partition and the index constrains the full set.
  The partition key is a pure function of an immutable ID, which also gives the planner static
  pruning on point lookups.
- **I4** — Amount, currency, merchant and tenant of a `Payment` are immutable after creation.
- **I5** — Every state change appends exactly one row to the payment event log; the aggregate
  version increments monotonically (optimistic concurrency).

### 9.1 Payment attempt outcomes

An attempt is the unit of gateway interaction and has its own small FSM:

`PENDING → DISPATCHED → { SUCCESS | DECLINED | ERROR | TIMEOUT_UNKNOWN }`

- `DECLINED` — the gateway definitively said no. Deterministic; may trigger failover only if the
  decline reason is in the *retryable decline* set (issuer unavailable, do-not-honour with soft
  code, network error). A hard decline (stolen card, invalid account) must **never** be retried
  on another gateway — that is card testing behaviour and gets the platform de-registered.
- `ERROR` — our side or transport failed *before* the gateway could have acted. Safe to retry.
- `TIMEOUT_UNKNOWN` — we do not know whether money moved. **Never retried automatically.**
  Enters the reconciliation queue; resolved by gateway status lookup or webhook.

---

## 10. Gateway health state machine

Health is per `(gateway_id, operation)` — deliberately *not* per merchant, because per-merchant
samples are too sparse to be statistically meaningful. Per-merchant overrides exist for
contractual pinning.

```
HEALTHY ──(error rate > 5% over 30s, min 20 samples)──► DEGRADED
DEGRADED ──(error rate > 25% or p99 > 5s)──► UNHEALTHY   [circuit OPEN]
UNHEALTHY ──(cool-down 30s)──► PROBING                   [circuit HALF_OPEN]
PROBING ──(3 consecutive successes)──► HEALTHY
PROBING ──(any failure)──► UNHEALTHY  [cool-down doubles, capped at 5 min]
```

State changes publish `gateway.health_changed.v1`, which the routing engine consumes and the
control plane records. This is the feedback loop from Observability back into Control.

---

## 11. Onboarding workflow definition

Workflow `merchant-onboarding@v1`. Business key: `merchant_id` (guarantees one live instance
per merchant — starting it twice is a no-op returning the existing instance).

| # | Step | Activity | Idempotent | Timeout | Retry | Compensation |
|---|---|---|---|---|---|---|
| 1 | `validate-merchant` | Validation Plane L2 | yes (pure) | 5 s | 3 × 200 ms | — |
| 2 | `submit-kyc` | KYC vendor port | yes (vendor ref key) | 30 s | 5 × exp 1 s→60 s | cancel KYC case |
| 3 | `await-kyc-decision` | signal wait | n/a | 7 d | — | cancel KYC case |
| 4 | `validate-bank-account` | Bank validation port | yes | 30 s | 5 × exp | — |
| 5 | `provision-gateways` | Gateway adapter `Provision` (fan-out per selected gateway) | yes (external ref) | 60 s | 5 × exp | de-provision sub-account |
| 6 | `store-credentials` | Secrets port (write + reference) | yes | 10 s | 3 × exp | delete secret version |
| 7 | `register-webhooks` | Gateway adapter `RegisterWebhook` | yes | 30 s | 5 × exp | delete webhook registration |
| 8 | `apply-configuration` | Control plane config apply | yes (version) | 10 s | 3 × exp | roll back to previous version |
| 9 | `sandbox-validation` | Certification suite (sandbox) | yes (run id) | 15 m | 2 × | — |
| 10 | `certification` | Certification suite (full matrix) | yes (run id) | 30 m | 2 × | — |
| 11 | `compliance-review` | **manual gate** | n/a | 5 d | — | — |
| 12 | `activate` | Merchant FSM `→ ACTIVE` | yes | 5 s | 3 × | suspend merchant |

**Semantics guaranteed by the engine (§ 20):** every step's *result* is checkpointed before the
next step begins; resuming an instance replays no completed step; an aborted instance runs the
compensations of completed steps in strict reverse order; a step that exhausts retries moves the
instance to `FAILED` and the step payload to the workflow DLQ with the full error chain; a
manual gate blocks until an authorized principal signals it, and the signal is itself audited.

### 11.4 Certification suite

Certification is a machine-checked matrix, not a checkbox. For each
`(gateway, payment_method, currency)` the merchant enabled, the suite asserts:

| Assertion | Why |
|---|---|
| Authorize → Capture → Refund round-trip succeeds in sandbox | proves the happy path end-to-end |
| Authorize → Void succeeds | proves reversal works before real money is at risk |
| A declined test card yields a mapped `DECLINED` outcome with a normalized reason code | proves the ACL maps errors correctly |
| A webhook is received, signature-verified, and moves the payment state | proves the async loop is closed |
| 3DS challenge flow reaches `REQUIRES_ACTION` and completes | proves SCA compliance |
| Duplicate submission with the same idempotency key returns the same result | proves idempotency in the real integration |
| Amount/currency echoed by the gateway match what we sent | proves L6 response validation |

The run produces a signed, immutable `CertificationReport` stored in object storage and
referenced from the merchant record. `PRODUCTION_READY` is unreachable without a passing report.

---

## 12. Data plane request pipeline

Ordered, and the order is load-bearing. Each stage has a latency budget; the sum is the p99 SLO
minus gateway time.

| # | Stage | Budget | Fails with | Notes |
|---|---|---|---|---|
| 1 | TLS / WAF / edge rate limit | — | `429`, `403` | Outside the app |
| 2 | Request ID + trace context | 1 ms | — | `traceparent` in, always out |
| 3 | Authentication (OAuth2/JWT or mTLS) | 2 ms | `401 UNAUTHENTICATED` | JWKS cached, background refresh |
| 4 | Tenant resolution + isolation guard | 1 ms | `403 TENANT_MISMATCH` | Tenant comes from the *token*, never from the body |
| 5 | Authorization (RBAC + ABAC) | 2 ms | `403 FORBIDDEN` | |
| 6 | Per-tenant / per-merchant rate limit + concurrency bulkhead | 2 ms | `429 RATE_LIMITED` | Token bucket in Redis, local fallback |
| 7 | **L1 schema validation** | 3 ms | `400 VALIDATION_FAILED` | Includes the PAN detector (§17) |
| 8 | **Idempotency** claim | 8 ms | `409`, `422` | Postgres-authoritative |
| 9 | Merchant context load (cached config) | 5 ms | `404`, `409 MERCHANT_NOT_ACTIVE` | ≤ 30 s stale allowed |
| 10 | **L5 payment validation** (limits, currency, method, risk policy) | 5 ms | `422` | |
| 11 | Risk engine | 15 ms | `422 RISK_DECLINED`, or forces 3DS | Fail-open to *policy default*, not to "allow" |
| 12 | Routing engine → routing plan | 5 ms | `503 NO_ELIGIBLE_GATEWAY` | Plan persisted |
| 13 | Orchestrator: create attempt, persist, dispatch | 10 ms | | Attempt row written **before** the gateway call |
| 14 | Gateway adapter call | ≤ 8 s hard timeout | | Circuit breaker + bulkhead per gateway |
| 15 | **L6 response validation** | 3 ms | `502 GATEWAY_CONTRACT_VIOLATION` | Signature, schema, amount/currency echo |
| 16 | **L7 state transition** + outbox write (one transaction) | 10 ms | `409 INVALID_STATE_TRANSITION` | |
| 17 | Idempotency completion + response | 5 ms | | Response snapshot stored |

### 12.3 The timeout rule

If stage 14 times out or returns an ambiguous transport error, the attempt is recorded as
`TIMEOUT_UNKNOWN` and the payment remains `PROCESSING`. The client receives `202 Accepted`
semantics on a synchronous endpoint (`status: "processing"`), not a failure. Resolution paths,
in order of speed: (a) gateway webhook arrives; (b) reconciler polls the gateway's lookup API
using our deterministic idempotency key; (c) settlement report. Only these can move the payment
out of `PROCESSING`. **No timer may fail a payment.**

---

## 13. Event catalog

### 13.1 Envelope

CloudEvents 1.0 structural compatibility, plus required platform extensions.

```json
{
  "specversion": "1.0",
  "id": "evt_01JB8Z9K2QW3E4R5T6Y7U8I9O0",
  "type": "payment.authorized.v1",
  "source": "/payments-platform/payment-orchestrator",
  "subject": "pay_01JB8Z9K2QW3E4R5T6Y7U8I9O0",
  "time": "2026-08-26T14:03:11.412Z",
  "datacontenttype": "application/json",
  "dataschema": "https://schemas.example.com/events/payment.authorized.v1.json",
  "tenantid": "ten_01J...",
  "merchantid": "mrc_01J...",
  "correlationid": "req_01J...",
  "causationid": "evt_01J...",
  "traceparent": "00-4bf92f...-00f067aa0ba902b7-01",
  "aggregateid": "pay_01J...",
  "aggregateversion": 4,
  "partitionkey": "pay_01J...",
  "data": { }
}
```

Rules: events are **immutable**, **versioned in the type name** (`.v1`), **additive-only within
a major version** (new optional fields only), and **idempotently consumable** (consumers dedupe
on `(consumer_group, id)`). A breaking change is a new `.v2` type published alongside `.v1`
until every consumer has migrated — never an in-place edit.

### 13.2 Catalog

| Event type | Context | Aggregate | Partition key | Consumers |
|---|---|---|---|---|
| `merchant.created.v1` | BC-2 | Merchant | `merchant_id` | Onboarding, Audit, Analytics |
| `merchant.validated.v1` | BC-3 | Merchant | `merchant_id` | Onboarding, Audit |
| `merchant.kyc_approved.v1` | BC-3 | Merchant | `merchant_id` | Onboarding, Audit, Compliance |
| `merchant.kyc_failed.v1` | BC-3 | Merchant | `merchant_id` | Onboarding, Audit, Notification |
| `merchant.bank_validated.v1` | BC-3 | Merchant | `merchant_id` | Onboarding, Audit |
| `merchant.gateway_provisioned.v1` | BC-4 | GatewayConnection | `merchant_id` | Onboarding, Config, Audit |
| `merchant.certified.v1` | BC-3 | Merchant | `merchant_id` | Onboarding, Audit |
| `merchant.activated.v1` | BC-2 | Merchant | `merchant_id` | Data plane cache, Audit, Notification |
| `merchant.suspended.v1` | BC-2 | Merchant | `merchant_id` | Data plane cache (**priority invalidation**), Audit |
| `merchant.terminated.v1` | BC-2 | Merchant | `merchant_id` | All |
| `configuration.published.v1` | BC-5 | Configuration | `merchant_id` | Data plane cache, Audit |
| `configuration.rolled_back.v1` | BC-5 | Configuration | `merchant_id` | Data plane cache, Audit |
| `payment.created.v1` | BC-6 | Payment | `payment_id` | Ledger, Analytics, Audit |
| `payment.attempted.v1` | BC-6 | PaymentAttempt | `payment_id` | Analytics, Routing feedback |
| `payment.authorized.v1` | BC-6 | Payment | `payment_id` | Ledger, Notification, Analytics |
| `payment.captured.v1` | BC-6 | Payment | `payment_id` | Ledger, Notification, Analytics |
| `payment.failed.v1` | BC-6 | Payment | `payment_id` | Ledger, Notification, Routing feedback |
| `payment.voided.v1` | BC-6 | Payment | `payment_id` | Ledger, Notification |
| `payment.refunded.v1` | BC-6 | Payment | `payment_id` | Ledger, Notification |
| `payment.settled.v1` | BC-8 | Payment | `payment_id` | Ledger, Reconciliation |
| `payment.disputed.v1` | BC-8 | Payment | `payment_id` | Ledger, Risk, Notification |
| `payment.reconciliation_required.v1` | BC-6 | Payment | `payment_id` | Reconciler (**alerting**) |
| `webhook.received.v1` | BC-7 | InboundWebhook | `gateway_ref` | Webhook processor |
| `gateway.health_changed.v1` | BC-4 | GatewayHealth | `gateway_id` | Routing, Control plane, Alerting |
| `audit.recorded.v1` | BC-9 | AuditRecord | `tenant_id` | Audit sink, SIEM |

### 13.3 Topics

`pp.<context>.<aggregate>.v1`, plus `.dlq` and `.retry` siblings.

| Topic | Partitions | Retention | Cleanup | Key |
|---|---|---|---|---|
| `pp.merchants.merchant.v1` | 12 | 30 d | delete | `merchant_id` |
| `pp.config.configuration.v1` | 12 | 7 d + compact | compact | `merchant_id` |
| `pp.payments.payment.v1` | 48 | 30 d | delete | `payment_id` |
| `pp.gateways.health.v1` | 6 | 1 d + compact | compact | `gateway_id` |
| `pp.webhooks.inbound.v1` | 24 | 7 d | delete | `gateway_ref` |
| `pp.audit.v1` | 12 | 400 d → S3 | delete | `tenant_id` |
| `*.dlq` | same | 30 d | delete | same |

Ordering guarantee: **per partition key only.** Because the key is the aggregate ID, all events
for one payment are ordered; there is no global order and no consumer may assume one.

### 13.4 Outbox

Every state change and its event are written in **one** database transaction — the state row and
an `outbox_events` row. `outbox-relay` polls with `FOR UPDATE SKIP LOCKED`, publishes to Kafka,
marks published. This eliminates the dual-write failure mode (state committed, event lost, or
event published, state rolled back). Relay is at-least-once by construction; duplicates are
handled by §13.5.

### 13.5 Effectively-once consumption

```
receive → dedup INSERT (consumer_group, event_id) ON CONFLICT DO NOTHING
        → if 0 rows affected: ACK and drop (already processed)
        → else: handle within the same transaction as the dedup row
        → commit → ACK
```

Plus database-level business invariants (I1–I3) as the last line of defence, because a bug in
the dedup path must still not be able to move money twice.

---

## 14. Idempotency contract

### 14.1 Scope and key

The idempotency scope is `(tenant_id, merchant_id, method, path_template, idempotency_key)`.
Required on `POST /v1/payments`, `/capture`, `/refund`, `/void`, and every control-plane
mutation. Key: client-supplied, 1–255 chars, opaque. Recommended: a UUID per logical operation.

### 14.2 Request fingerprint

`SHA-256` over the canonicalized request body (JCS: sorted keys, no insignificant whitespace)
plus the scope tuple. Same key + different fingerprint → `422 IDEMPOTENCY_KEY_REUSED`. This is
the check that catches a client bug where one key is reused for two different payments.

### 14.3 Record lifecycle

| State | Meaning | Behaviour on a duplicate |
|---|---|---|
| `IN_FLIGHT` | claimed, lease held (`lease_expires_at`) | `409 IDEMPOTENT_REQUEST_IN_PROGRESS` + `Retry-After: 1` |
| `COMPLETED` | response snapshot stored | replay stored status + body + `Idempotent-Replay: true` |
| `FAILED_TERMINAL` | non-retryable failure snapshot stored | replay the stored error |
| lease expired | the original process died | reclaim atomically (`UPDATE … WHERE lease_expires_at < now()`), re-execute |

The claim is `INSERT … ON CONFLICT DO NOTHING` against a unique index. **Postgres is
authoritative.** Redis mirrors completed records purely as a latency accelerator; a Redis miss
or a total Redis outage degrades latency, never correctness. Retention: 7 days (configurable;
must exceed the longest client retry window), then archived to S3 with the audit trail.

### 14.4 Gateway-level idempotency

Distinct concern, distinct key. `gateway_idempotency_key = base32(HMAC-SHA256(attempt_id,
gateway_salt))[:32]`, stored on the attempt row before dispatch. Properties:

- A transport retry to the **same** gateway reuses the same key → the gateway dedupes → no
  double charge.
- A failover to a **different** gateway creates a **new attempt** → a new key → correctly a new
  authorization (the previous one is separately voided/reconciled).
- The key is reproducible after a crash, so the reconciler can look the transaction up.

---

## 15. Consistency model (CAP, per store, per operation)

CAP is not a system-wide choice; it is a per-operation choice.

| Operation | Choice | Under partition | Mechanism |
|---|---|---|---|
| Payment write (create/authorize/capture/refund) | **CP** | reject with `503`, do not degrade | Single regional Aurora writer; no cross-region writes |
| Idempotency claim | **CP** (linearizable) | reject | Unique index on the primary |
| Ledger append | **CP** | reject | Same transaction as state change |
| Payment read (`GET`) | **AP**, read-your-writes for the caller | serve from replica, may be ≤ 1 s stale | Replica reads with a write-token fallback to primary |
| Merchant/config read on the payment path | **AP**, bounded staleness ≤ 30 s | serve last-known-good from local cache | Cache + Kafka invalidation; **fail-static, not fail-open** |
| Merchant/config write | **CP** | reject | Control plane primary |
| Gateway health | **AP** | serve stale, decay confidence | Local windows + Kafka gossip |
| Audit write | **CP** for the hash chain | buffer to local WAL, replay | Append-only with chained digests |
| Analytics/projections | **AP** | lag | Kafka consumers |

**Fail-static, not fail-open** deserves emphasis: if the control plane is entirely down, the data
plane keeps processing with its last-known-good configuration snapshot rather than either
stopping (fail-closed, kills revenue) or ignoring limits (fail-open, kills compliance). The
snapshot's age is exported as a metric and alerts past 5 minutes; past `max_config_staleness`
(default 15 min) the data plane *does* fail closed for new merchants while continuing to serve
existing ones. Graceful degradation with a defined cliff.

---

## 16. Multi-tenancy model

### 16.1 Isolation matrix

| Dimension | Pooled tier (default) | Siloed tier |
|---|---|---|
| **Database** | Shared cluster, shared schema, `tenant_id` on every table, **PostgreSQL Row-Level Security** with `SET LOCAL app.tenant_id`, app connects as a non-`BYPASSRLS` role | Dedicated schema (or cluster) per tenant |
| **Cache** | Key prefix `pp:{tenant_id}:…`, per-tenant memory quota, `SCAN`-based namespace eviction | Dedicated Redis DB / cluster |
| **Events** | Shared topics, `tenantid` in the envelope, tenant-scoped consumer filters, Kafka ACLs by principal | Dedicated topics |
| **Configuration** | Row-scoped, versioned per tenant | Same |
| **Credentials** | One secret per `(tenant, merchant, gateway)`; IAM path `/{env}/{tenant}/{merchant}/{gateway}`; KMS key per tenant for siloed tier | Dedicated KMS CMK |
| **Object storage** | `s3://bucket/{tenant_id}/…` with IAM condition on the prefix | Dedicated bucket |
| **Logs** | `tenant_id` field on every record; log views filtered by tenant claim | Dedicated log group |
| **Metrics** | `tenant_id` label only on low-cardinality series (SLO counters); high-cardinality series use `tenant_tier` + exemplars carrying `tenant_id` | Dedicated dashboards |
| **Compute** | Shared pods; per-tenant concurrency bulkhead + rate limit | Dedicated node group / namespace |

### 16.2 The isolation guard

Tenant identity is derived **exclusively** from the authenticated principal. A `tenant_id` in a
request body or query string is either ignored or, if it disagrees with the token, treated as a
security event (`403 TENANT_MISMATCH` + audit + alert). Every repository method takes a
`context.Context` from which the tenant is extracted; a repository call with no tenant in
context returns `ErrMissingTenantContext` rather than querying. Defence in depth: application
guard → RLS policy → integration test `TestCrossTenantAccessIsImpossible` that asserts a query
for tenant B's row under tenant A's context returns zero rows *at the database level*.

---

## 17. PCI DSS scope boundary

### 17.1 The boundary

```
┌── OUT OF SCOPE (this platform, 8 of 9 services) ─────────────────┐
│  Merchant checkout → gateway-hosted fields / SDK tokenization    │
│  → our API receives ONLY: gateway token, network token, or       │
│    payment-method reference. No PAN. No CVV. No track data.      │
└──────────────────────────────────────────────────────────────────┘
                              │  token reference only
┌── IN SCOPE (optional, segregated) ───────────────────────────────┐
│  card-vault service: separate AWS account, separate VPC,         │
│  separate cluster, dedicated HSM/KMS, its own change control     │
│  and its own SAQ-D assessment. NOT part of this repository.      │
└──────────────────────────────────────────────────────────────────┘
```

The design intent: the orchestration platform is assessed at **SAQ-A/A-EP** level because
cardholder data neither traverses nor is stored by it. If a tenant requires vaulting, that
capability lives in a physically and administratively separate system reached through a port.

### 17.2 Enforcement (not just policy)

| Control | Implementation |
|---|---|
| PAN cannot enter | L1 validator runs a PAN detector (13–19 digits, Luhn-valid, after stripping separators) over every string field. A hit → `400 SENSITIVE_DATA_IN_REQUEST`, the value is **not** logged, a security event is raised. |
| PAN/CVV cannot be logged | Structured logging uses an **allowlist**: only registered field names are serialized. There is no `log.Printf("%+v", req)` path — the linter forbids `%+v` / `%#v` on request types. |
| Secrets cannot be logged | `Secret[T]` wrapper type whose `String()`, `MarshalJSON()` and `Format()` all return `[REDACTED]`. Credentials are only ever this type. |
| Encryption at rest | KMS CMK per environment (per tenant for siloed); Aurora, S3, EBS, Kafka all encrypted; application-level envelope encryption for credential material. |
| Encryption in transit | TLS 1.3 externally; mTLS between services via the service mesh; TLS to Postgres, Redis (in-transit encryption on), and Kafka (SASL_SSL). |
| Key rotation | KMS annual automatic; gateway API credentials rotated ≤ 90 days by an automated workflow with dual-run overlap; JWT signing keys 30 days with a 2-key JWKS window. |
| Least privilege | IRSA per service account, one IAM role per deployable, secret paths scoped by prefix condition. |

### 17.3 Regulatory boundaries beyond PCI

| Regime | Boundary drawn |
|---|---|
| **PSD2 / SCA** | 3DS is a *policy outcome* of the risk engine, per-merchant/per-corridor configurable; exemptions (TRA, low value, MIT) are modelled explicitly and audited. |
| **GDPR / data residency** | Personal data (merchant principals, KYC artifacts) is stored in the tenant's declared residency region; the routing engine will not select a gateway whose region violates the tenant's residency policy. Right-to-erasure is implemented as crypto-shredding of the tenant's data key, with financial records retained under the legal-obligation basis. |
| **AML/KYC** | KYC decisions and their evidence are retained ≥ 5 years, immutable, in object storage with Object Lock. |
| **Records retention** | Payments and ledger: 7 years. Audit: 7 years, WORM. Idempotency: 7 days. Logs: 30 days hot / 400 days archive. PII in logs: none. |

---

## 18. Non-functional targets

| Category | Target | How measured |
|---|---|---|
| Data plane availability | 99.99 % monthly (≤ 4 m 23 s) | Successful-request ratio SLI, 30 d window |
| Control plane availability | 99.9 % monthly | Same |
| Payment API latency | p50 ≤ 60 ms, p99 ≤ 250 ms **excluding** gateway time | Server-side histogram |
| End-to-end payment latency | p99 ≤ 1.5 s including gateway | Trace-derived |
| Throughput | 5 000 TPS sustained per region, 15 000 TPS peak, 3× headroom | Load tests in `tests/load` |
| Merchant scale | 50 000 merchants across 500 tenants | Data model + index design |
| Onboarding duration | ≤ 30 min automated portion (p95), excluding external KYC SLA | Workflow duration histogram |
| Config propagation | ≤ 30 s p99 from publish to data-plane effect | Synthetic probe |
| **RPO** | ≤ 5 s (in-region: 0 — synchronous commit; cross-region: Aurora Global ≤ 1 s typical, 5 s budgeted) | DR drill |
| **RTO** | ≤ 15 min region failover, ≤ 60 s AZ failover | DR drill |
| Durability | 99.999999999 % (S3), Aurora 6-way replication across 3 AZs | Provider SLA |
| Error budget policy | Burn > 2× → feature freeze; > 10× in 1 h → incident + rollback | Automated in CI |

---

## 19. API surface

Public REST (`/v1`), internal gRPC. Full contracts in `api/openapi/` and `api/proto/`.

### 19.1 Control plane

| Method | Path | Idempotent | Auth scope |
|---|---|---|---|
| `POST` | `/v1/merchants` | key required | `merchants:write` |
| `GET` | `/v1/merchants/{merchantId}` | safe | `merchants:read` |
| `PATCH` | `/v1/merchants/{merchantId}` | key required + `If-Match` | `merchants:write` |
| `GET` | `/v1/merchants` | safe, cursor-paginated | `merchants:read` |
| `POST` | `/v1/merchants/{merchantId}/onboarding` | key required | `onboarding:write` |
| `GET` | `/v1/merchants/{merchantId}/onboarding` | safe | `onboarding:read` |
| `POST` | `/v1/merchants/{merchantId}/onboarding/signals/{signal}` | key required | `onboarding:approve` |
| `GET` | `/v1/merchants/{merchantId}/configuration` | safe | `config:read` |
| `PUT` | `/v1/merchants/{merchantId}/configuration` | key required + `If-Match` | `config:write` |
| `GET` | `/v1/merchants/{merchantId}/configuration/versions` | safe | `config:read` |
| `POST` | `/v1/merchants/{merchantId}/configuration/rollback` | key required | `config:write` |
| `GET` | `/v1/gateways` | safe | `gateways:read` |
| `GET` | `/v1/gateways/{gatewayId}` | safe | `gateways:read` |
| `GET` | `/v1/gateways/{gatewayId}/health` | safe | `gateways:read` |
| `POST` | `/v1/gateways/{gatewayId}/credentials:rotate` | key required | `credentials:rotate` |

### 19.2 Data plane

| Method | Path | Idempotent | Auth scope |
|---|---|---|---|
| `POST` | `/v1/payments` | **key required** | `payments:write` |
| `GET` | `/v1/payments/{paymentId}` | safe | `payments:read` |
| `GET` | `/v1/payments` | safe, cursor-paginated | `payments:read` |
| `POST` | `/v1/payments/{paymentId}/capture` | **key required** | `payments:capture` |
| `POST` | `/v1/payments/{paymentId}/refund` | **key required** | `payments:refund` |
| `POST` | `/v1/payments/{paymentId}/void` | **key required** | `payments:void` |
| `POST` | `/v1/webhooks/{gateway}` | signature-authenticated | none (gateway auth) |
| `GET` | `/healthz` `/readyz` `/livez` `/metrics` | safe | none / cluster-internal |

### 19.3 Cross-cutting HTTP semantics

| Concern | Contract |
|---|---|
| Idempotency | `Idempotency-Key` request header; `Idempotent-Replay: true` on replays |
| Concurrency | `ETag` on mutable resources; `If-Match` required on `PATCH`/`PUT`; mismatch → `412` |
| Correlation | `X-Request-Id` (echoed or generated), W3C `traceparent`, `X-Correlation-Id` |
| Pagination | Opaque cursor: `?limit=&cursor=`; response `{ "data": [], "next_cursor": null }`. No offset pagination — it is unstable under concurrent writes |
| Rate limits | `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset`, `Retry-After` on `429` |
| Versioning | URI major version + additive-only within a major; deprecation via `Sunset` and `Deprecation` headers |
| Partial failure | `502/503/504` are explicitly *retryable*; `4xx` other than `409`/`429` are not |
| Content type | `application/json`; errors `application/problem+json` (RFC 9457) with our extensions |

---

## 20. Error model

```json
{
  "type": "https://errors.example.com/PAYMENT_ALREADY_PROCESSED",
  "title": "Payment has already been processed",
  "status": 409,
  "code": "PAYMENT_ALREADY_PROCESSED",
  "detail": "Payment pay_01J... is in state CAPTURED and cannot be captured again",
  "category": "BUSINESS_RULE",
  "retryable": false,
  "requestId": "req_01J...",
  "traceId": "4bf92f3577b34da6a3ce929d0e0e4736",
  "details": [
    { "field": "amount", "code": "EXCEEDS_CAPTURABLE", "message": "…" }
  ],
  "docsUrl": "https://docs.example.com/errors/PAYMENT_ALREADY_PROCESSED"
}
```

### 20.1 Categories

| Category | HTTP | gRPC | Retryable | Alert |
|---|---|---|---|---|
| `VALIDATION` | 400 / 422 | `InvalidArgument` | no | no |
| `AUTHENTICATION` | 401 | `Unauthenticated` | no | on spike |
| `AUTHORIZATION` | 403 | `PermissionDenied` | no | on spike |
| `NOT_FOUND` | 404 | `NotFound` | no | no |
| `CONFLICT` | 409 / 412 | `Aborted` / `FailedPrecondition` | sometimes (409 in-flight: yes) | no |
| `BUSINESS_RULE` | 422 | `FailedPrecondition` | no | no |
| `RATE_LIMIT` | 429 | `ResourceExhausted` | **yes**, after `Retry-After` | on sustained |
| `GATEWAY` | 502 | `Unavailable` | **yes**, unless a hard decline | yes |
| `TIMEOUT` | 504 | `DeadlineExceeded` | **ambiguous — see §12.3** | yes |
| `INFRASTRUCTURE` | 503 | `Unavailable` | **yes** | yes |
| `INTERNAL` | 500 | `Internal` | no | yes, page |

The `retryable` flag is machine-readable and is what client SDKs, the workflow engine and the
outbox relay branch on. It is not advisory prose.

### 20.2 Reserved codes (excerpt; the full catalog is `api/errors/catalog.yaml`)

`VALIDATION_FAILED`, `SENSITIVE_DATA_IN_REQUEST`, `IDEMPOTENCY_KEY_REQUIRED`,
`IDEMPOTENCY_KEY_REUSED`, `IDEMPOTENT_REQUEST_IN_PROGRESS`, `TENANT_MISMATCH`,
`MERCHANT_NOT_FOUND`, `MERCHANT_NOT_ACTIVE`, `PAYMENT_NOT_FOUND`,
`PAYMENT_ALREADY_PROCESSED`, `INVALID_STATE_TRANSITION`, `AMOUNT_EXCEEDS_LIMIT`,
`REFUND_EXCEEDS_CAPTURED`, `CURRENCY_NOT_SUPPORTED`, `PAYMENT_METHOD_NOT_SUPPORTED`,
`NO_ELIGIBLE_GATEWAY`, `RISK_DECLINED`, `THREE_DS_REQUIRED`, `GATEWAY_DECLINED`,
`GATEWAY_TIMEOUT`, `GATEWAY_CONTRACT_VIOLATION`, `GATEWAY_CIRCUIT_OPEN`,
`WEBHOOK_SIGNATURE_INVALID`, `WEBHOOK_REPLAY_DETECTED`, `CONFIGURATION_INVALID`,
`CONFIGURATION_VERSION_CONFLICT`, `WORKFLOW_STEP_FAILED`, `RATE_LIMITED`,
`SERVICE_UNAVAILABLE`, `INTERNAL_ERROR`.

---

## 21. Validation plane contract

Seven levels, each with a stable identifier so failures are traceable to a rule.

| Level | Name | Runs where | Pure? | Failure |
|---|---|---|---|---|
| L1 | API / schema | edge middleware | yes | `400 VALIDATION_FAILED` |
| L2 | Merchant | onboarding workflow, merchant writes | mostly (vendor calls impure) | `422` + case annotation |
| L3 | Gateway | onboarding, credential rotation, scheduled probe | no (network) | `422` / marks connection unhealthy |
| L4 | Configuration | control plane write path | yes | `422 CONFIGURATION_INVALID` |
| L5 | Payment | data plane, pre-dispatch | yes (config is an input) | `422` |
| L6 | Response | data plane, post-gateway | yes | `502 GATEWAY_CONTRACT_VIOLATION` |
| L7 | Domain / state | aggregate methods | yes | `409 INVALID_STATE_TRANSITION` |

Contract for every rule:

```go
type Rule[T any] interface {
    ID() RuleID           // e.g. "L5.AMOUNT_WITHIN_MERCHANT_LIMIT" — stable, documented
    Severity() Severity   // ERROR | WARNING
    Evaluate(ctx context.Context, subject T) Outcome
}
```

Rules are **pure and total** wherever possible: same input → same outcome, no panics, no
network. Impure rules (L3) are explicitly marked and are never invoked on the payment hot path.
Every rule ID appears in `docs/validation-plane.md` with its meaning and remediation, and the
CI check `TestEveryRuleIsDocumented` fails the build if one is missing.

---

## 22. Observability contract

### 22.1 Mandatory context

Every log line, span and (where cardinality permits) metric carries:
`trace_id`, `span_id`, `correlation_id`, `tenant_id`, `merchant_id`, `payment_id`, `gateway_id`,
`service`, `version`, `environment`, `region`.

### 22.2 RED + business metrics

| Metric | Type | Labels |
|---|---|---|
| `pp_http_requests_total` | counter | `service,route,method,status,tenant_tier` |
| `pp_http_request_duration_seconds` | histogram | `service,route,method` |
| `pp_payments_total` | counter | `outcome,currency,payment_method,gateway,tenant_tier` |
| `pp_payment_authorization_rate` | gauge (recorded rule) | `gateway,currency` |
| `pp_gateway_request_duration_seconds` | histogram | `gateway,operation` |
| `pp_gateway_errors_total` | counter | `gateway,operation,class` |
| `pp_circuit_breaker_state` | gauge (0/1/2) | `gateway,operation` |
| `pp_idempotency_outcomes_total` | counter | `outcome` (`new,replay,in_progress,conflict`) |
| `pp_routing_decisions_total` | counter | `gateway,reason` |
| `pp_workflow_step_duration_seconds` | histogram | `workflow,step,outcome` |
| `pp_workflow_instances` | gauge | `workflow,state` |
| `pp_onboarding_duration_seconds` | histogram | `outcome` |
| `pp_outbox_backlog` | gauge | `topic` |
| `pp_consumer_lag` | gauge | `topic,group` |
| `pp_config_snapshot_age_seconds` | gauge | `service` |
| `pp_reconciliation_exceptions` | gauge | `severity` |
| `pp_dlq_depth` | gauge | `queue` |

### 22.3 Cardinality rule

`merchant_id` and `payment_id` are **never** metric labels. They live in logs, traces and
exemplars. A metric label set may not exceed 10⁴ series per metric per service; CI runs a
cardinality lint over the metric registry.

### 22.4 SLIs and burn-rate alerts

| SLI | SLO | Fast burn | Slow burn |
|---|---|---|---|
| Payment API availability | 99.99 % | 14.4× over 1 h → page | 6× over 6 h → ticket |
| Payment API latency (p99 ≤ 250 ms) | 99 % of 5-min windows | 14.4× over 1 h → page | 6× over 6 h → ticket |
| Authorization success rate | ≥ merchant baseline − 5 pp | 30-min drop → page | — |
| Webhook processing lag | p99 ≤ 60 s | > 5 min → page | — |
| Config propagation | p99 ≤ 30 s | > 5 min → page | — |

---

## 23. Configuration document (desired state)

Canonical schema, versioned; `api/openapi` carries the machine-readable form.

```json
{
  "merchantId": "mrc_01J...",
  "version": 7,
  "status": "ACTIVE",
  "environment": "production",
  "supportedCurrencies": ["USD", "EUR"],
  "paymentMethods": ["CARD", "APPLE_PAY"],
  "countries": ["US", "DE"],
  "routing": {
    "strategy": "PRIORITY_WITH_FALLBACK",
    "primary": "stripe",
    "fallback": ["adyen"],
    "rules": [
      { "when": { "currency": "EUR", "paymentMethod": "CARD" },
        "then": { "primary": "adyen", "fallback": ["stripe"] } }
    ],
    "weights": { "health": 0.4, "successRate": 0.3, "cost": 0.2, "latency": 0.1 }
  },
  "risk": {
    "maxTransactionAmount": { "amount": 1000000, "currency": "USD" },
    "require3DSAbove":      { "amount": 50000,   "currency": "USD" },
    "dailyVolumeLimit":     { "amount": 50000000,"currency": "USD" },
    "velocity": { "maxPaymentsPerMinute": 300, "maxPerCardPerHour": 5 },
    "blockedCountries": ["KP", "IR"]
  },
  "limits": { "maxRefundWindowDays": 180, "maxPartialCaptures": 5 },
  "webhooks": {
    "endpoints": [{ "url": "https://…", "events": ["payment.*"], "secretRef": "…" }],
    "retryPolicy": { "maxAttempts": 8, "backoff": "EXPONENTIAL_JITTER" }
  },
  "settlement": { "schedule": "DAILY", "currency": "USD", "holdDays": 2 },
  "featureFlags": { "networkTokens": true, "partialCapture": true }
}
```

Every configuration write: validated (L4) → assigned the next version → persisted with the full
prior document retained → audited with actor and diff → published as
`configuration.published.v1`. Rollback publishes the previous document *as a new version* (never
deletes), so history is strictly append-only.

---

## 24. Failure mode catalog (design inputs)

| Failure | Detection | Response | Degradation |
|---|---|---|---|
| Gateway timeout | client timeout 8 s | attempt `TIMEOUT_UNKNOWN`, payment stays `PROCESSING`, reconcile | latency up, no correctness loss |
| Gateway 5xx | status | classify retryable; retry ≤ 2 with jitter on the *same* attempt; then failover as a *new* attempt | success rate dips |
| Gateway hard decline | mapped reason code | terminal `FAILED`; **no failover** | none |
| Gateway sustained errors | breaker threshold | open circuit, route to fallback, emit health event | traffic shifts |
| All gateways unhealthy | routing returns empty | `503 NO_ELIGIBLE_GATEWAY`, `Retry-After` | payments rejected — fail closed |
| Postgres primary loss | health probe / driver | Aurora auto-failover ≤ 60 s; readiness fails, LB sheds | writes reject `503`, reads from replica |
| Redis loss | probe | fall back to Postgres for idempotency, local token bucket for rate limits | latency up, limits coarser |
| Kafka loss | producer errors | outbox retains rows and backs off; **no data loss**; alert on backlog | events lag |
| Outbox backlog | `pp_outbox_backlog` | scale relay, alert | eventual consistency window widens |
| Consumer poison message | retry exhaustion | `.retry` topic → `.dlq`, alert; consumer continues | one message parked |
| Duplicate webhook | dedup table | drop silently, count metric | none |
| Webhook replay attack | timestamp skew > 5 min or nonce reuse | reject `401`, security event | none |
| Config corruption | L4 validation + checksum | reject publish; data plane keeps last-known-good | none |
| Pod crash mid-workflow | lease expiry | another worker leases and resumes from checkpoint | onboarding delayed |
| Node loss | k8s | PDB + anti-affinity + surge; connections drained | brief latency blip |
| AZ loss | AWS | multi-AZ everywhere; capacity headroom 3× | none if headroom holds |
| Region loss | health checks | promote Aurora Global secondary; Route 53 failover; §18 RTO 15 min | see DR doc |
| Retry storm | rate-limit + concurrency metrics | adaptive concurrency limiter sheds load; `429` with backoff | throughput capped, system survives |
| Clock skew | NTP monitoring | signature windows tolerate ±5 min; ULID generation guards against regression | none |

---

## 25. Repository layout (binding)

```
cmd/                      composition roots, one per deployable (§5)
internal/
  domain/                 entities, value objects, FSMs — stdlib only
    shared/ tenant/ merchant/ payment/ gateway/ routing/ risk/ ledger/ audit/ compliance/
  application/            use cases + ports (interfaces owned by the application)
    ports/ merchant/ onboarding/ payment/ webhook/ config/ gateway/ ledger/
  validation/             validation plane: engine + rules/l1..l7
  workflows/              engine port, postgres engine, temporal adapter, onboarding definition
  policies/               RBAC/ABAC, risk, compliance policy evaluation
  events/                 envelope, registry, codec, idempotent consumer
  platform/               idempotency, tenantctx, authn, authz, config, health, errors
  adapters/gateway/       spi, registry, stripe, adyen, paypal, simulator, contract suite
  infrastructure/         postgres, redis, kafka, secrets, telemetry, httpx, grpcx,
                          resilience, crypto, clock
pkg/                      stdlib-only reusable: apierror, money, ids, otelx
api/                      openapi/, proto/, events/ (JSON Schema), errors/ (catalog)
migrations/               ordered SQL, forward-only, each with a down script
config/                   per-environment config, seed policies, routing defaults
deployments/k8s/          kustomize base + dev/staging/prod overlays
helm/                     umbrella chart + per-service subcharts
terraform/                modules + per-environment stacks
docs/                     spec/, adr/, diagrams/, runbooks/, plane docs
tests/                    integration/, contract/, e2e/, chaos/, load/
scripts/                  build, verify, architecture check, DR drill
```

---

## 26. Traceability

Every requirement gets an ID and is traced to design, code and test. The matrix lives in
`docs/spec/09-traceability.md` and is regenerated by `scripts/traceability.sh`.

- `BR-nn` business requirement → `FR-nn` functional → `NFR-nn` non-functional
- each maps to ≥ 1 design section here, ≥ 1 package, ≥ 1 test
- CI fails on an orphan requirement (no test) or an orphan test (no requirement)

---

## 27. Definition of done (per phase)

A phase is complete only when all twelve hold:

1. Objective stated 2. Requirements enumerated 3. Design documented 4. Interfaces defined
5. Data model defined 6. Failure modes enumerated 7. Security considerations stated
8. Validation rules defined 9. Observability defined 10. Tests written **and passing**
11. Implementation complete 12. Verification run (build, vet, race, lint, SAST, vuln scan,
    contract validation, manifest validation, architecture check)
