# 01 — Requirements: Payment Transaction Processing

## Context

This document scopes the first vertical slice of an enterprise-grade fintech platform: **Payment
Transaction Processing** — the primitive that moves money between two ledger accounts (e.g.
customer → merchant) and records it durably, exactly once, with full auditability.

This feature was chosen as the flagship deliverable because it is the **hardest correctness
problem** in the whole platform: it touches money, must never double-execute, must survive every
failure mode in the org-wide failure list, and every other feature (refunds, disputes, payouts,
reporting) is built on top of it. If this is right, the rest of the platform inherits a proven
pattern. If this is wrong, nothing else matters.

## Actors

- **Client application** (merchant backend, mobile app, partner integration) — calls the Payments
  API with API-key + OAuth2 client-credentials or user OIDC token.
- **Payments API service** — owns payment creation, validation, ledger posting, idempotency.
- **Downstream consumers** — notification service, settlement/batch service, fraud/risk service —
  consume `payment.completed` / `payment.failed` events asynchronously.
- **Operator / SRE** — operates, monitors, and recovers the service.
- **Auditor / Compliance** — consumes immutable audit logs and ledger history.

## Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-1 | Client can create a payment (`POST /v1/payments`) specifying source account, destination account, amount, currency, and a client-supplied `Idempotency-Key`. |
| FR-2 | Retrying the same `Idempotency-Key` (network retry, client bug, LB replay) returns the original result and **never** creates a second monetary movement. |
| FR-3 | Every payment produces two balanced ledger entries (debit + credit) that sum to zero — double-entry bookkeeping, enforced, not just conventional. |
| FR-4 | Client can fetch payment status (`GET /v1/payments/{id}`): `pending`, `completed`, `failed`, `reversed`. |
| FR-5 | On completion or failure, the system emits an event (`payment.completed` / `payment.failed`) exactly once to downstream consumers, even across crashes. |
| FR-6 | Payments are asynchronously settleable — the API returns fast; heavier settlement work happens out of the request path. |
| FR-7 | All monetary amounts are represented as integer minor units (cents) with an ISO-4217 currency code — never floating point. |
| FR-8 | Full audit trail: who/what/when for every state transition, immutable, queryable by auditors. |

## Non-Functional Requirements (mapped to org-wide quality attributes)

| Attribute | Target | Notes |
|---|---|---|
| Availability | 99.95% monthly (≈21.6 min downtime/month) | Multi-AZ active-active; see `07-reliability-slo.md` |
| Latency | P50 < 80ms, P95 < 250ms, P99 < 600ms (API ack, not settlement) | Measured at ALB, excludes client network |
| Durability | 0 lost financial events; RPO ≤ 0 for committed ledger rows, RPO ≤ 5 min for downstream events | Aurora synchronous multi-AZ commit + outbox |
| Throughput | 500 req/s sustained per region at launch, horizontally scalable to 5,000 req/s | HPA + read replicas |
| Consistency | Strong consistency for ledger write path; eventual consistency acceptable for downstream fan-out | Outbox pattern |
| Recoverability | RTO ≤ 15 min for AZ failure, ≤ 60 min for region failure | See `04-failure-recovery-design.md` |
| Security | PCI-DSS-aligned control set, least privilege, encryption in transit/at rest | See `05-security-architecture.md` |
| Compliance | Full audit log retention 7 years (regulatory minimum for financial records in most jurisdictions) | Immutable append-only audit table + S3 archive |
| Observability | Every request traceable end-to-end; every ledger mutation alertable | See `06-observability.md` |
| Cost | Reserve capacity for baseline, burst via HPA; avoid over-provisioning | See ADR-005 |

## Explicit Non-Goals (v1 of this slice)

- Multi-currency FX conversion (assume same-currency transfer; FX is a separate bounded context).
- Refunds/reversals workflow (builds on this ledger primitive but is a separate feature).
- Card network integration (assumes accounts are already funded internal ledger accounts — this
  is the *ledger movement* primitive, not the card-acquiring flow).
- Fraud/risk scoring logic (this service emits events that a separate fraud service consumes).

Excluding these keeps the slice small enough to build to the full production bar rather than
spreading effort thin across a shallow implementation of everything.

## Acceptance Criteria

1. Duplicate `POST /v1/payments` with the same `Idempotency-Key` and body → same `payment_id`,
   same result, no second ledger entry (verified by integration test + chaos test with concurrent
   duplicate requests).
2. Sum of all ledger entries for any payment is always exactly zero (DB constraint, not just app
   logic).
3. Killing the process mid-request (`SIGKILL`) after DB commit but before event publish still
   results in the event being published exactly once within RPO (outbox relay recovers it).
4. Service passes load test at 500 req/s for 30 minutes with P99 < 600ms and zero data
   inconsistency.
5. Service degrades gracefully (circuit breaker opens, 503 with `Retry-After`) when the database
   is unavailable, instead of hanging or cascading failure to callers.
