# 02 — Architecture: Payment Transaction Processing

## System Context (C4 Level 1)

```
                        ┌────────────────────┐
                        │   Merchant / Client │
                        │   (REST + OIDC)     │
                        └──────────┬──────────┘
                                   │ HTTPS (TLS 1.3, mTLS optional)
                                   ▼
                     ┌─────────────────────────┐
                     │   AWS WAF + ALB (edge)   │  rate limit, DDoS, TLS termination
                     └────────────┬─────────────┘
                                  ▼
                    ┌──────────────────────────┐
                    │   payments-api (EKS)      │  Go service, this repo
                    │   - REST handlers          │
                    │   - idempotency guard       │
                    │   - ledger service          │
                    │   - outbox writer           │
                    └───────┬───────────┬────────┘
                            │           │
                 ┌──────────▼───┐   ┌───▼─────────────┐
                 │ Aurora        │   │ Outbox Relay      │
                 │ PostgreSQL     │   │ (same pod, bg     │
                 │ (Multi-AZ)     │   │ goroutine)         │
                 │ - accounts     │   └─────┬──────────────┘
                 │ - ledger_entries│         │ publishes
                 │ - payments      │         ▼
                 │ - outbox_events │   ┌─────────────┐
                 │ - idempotency   │   │  Amazon SQS  │──▶ SNS fan-out ──▶ notification-svc,
                 │ - audit_log     │   │  (+ DLQ)     │                     settlement-svc,
                 └────────────────┘   └─────────────┘                     fraud-svc (consumers,
                                                                            out of scope here)
```

## Why this shape (and not alternatives)

**Synchronous API + asynchronous fan-out**, rather than a fully synchronous "call three services
and wait" design or a fully asynchronous "submit and poll" design:

- *Why*: The caller needs a fast, strongly-consistent answer to "did the ledger move or not"
  (FR-1–FR-4), but downstream consumers (notifications, fraud, settlement batch) don't need to
  block the caller and shouldn't be able to fail the payment if, say, the notification service is
  down.
- *What problem it solves*: Decouples the availability of the core ledger write from the
  availability of every downstream consumer — a bulkhead at the architecture level.
- *When to use this pattern*: Any time there's one authoritative write that must succeed/fail
  atomically and N non-authoritative reactions to that write.
- *Alternative considered*: Full synchronous orchestration (API calls notification + fraud
  services inline before responding). Rejected — couples the payment's availability to N other
  services' availability and latency, violating the availability and latency NFRs.
- *Alternative considered*: Fully async "202 Accepted, poll for result." Rejected for v1 because
  clients (merchants) expect a synchronous authorization-style answer for a same-currency ledger
  transfer that itself has no external network hop; the work here is a local DB transaction, not a
  slow external call, so sync is achievable within the P95 250ms budget.
- *Tradeoff*: The service must still guarantee eventual delivery of the async events (outbox +
  relay), which is more implementation complexity than "just call SQS inline" — but calling SQS
  inline in the same request would reintroduce a dual-write problem (DB commit succeeds, SQS
  publish fails → lost event, or vice versa).
- *Risk*: Outbox relay lag under load could delay downstream consumers. Mitigated with relay
  concurrency, SQS backpressure metrics, and alerting on outbox queue depth.

## Component Responsibilities

| Component | Responsibility | Failure isolation |
|---|---|---|
| ALB + WAF | TLS termination, L7 rate limiting, OWASP rule set, DDoS absorption | Multi-AZ by default (AWS-managed) |
| payments-api pods | Validate request → idempotency check → open DB txn → post ledger entries → write outbox row → commit → respond | Stateless; any pod can serve any request; horizontal scale via HPA |
| Aurora PostgreSQL | System of record for accounts, ledger, payments, outbox, idempotency keys, audit log | Multi-AZ synchronous replica; automated failover ~30s |
| Outbox relay (in-process worker) | Poll `outbox_events` for unpublished rows, publish to SQS, mark published, retry with backoff | Runs in every pod with leader-lease-free "claim row" pattern (`FOR UPDATE SKIP LOCKED`) so it scales horizontally and tolerates pod death mid-batch |
| SQS + DLQ | Durable, at-least-once delivery to downstream consumers; poison messages land in DLQ after N retries | Managed service; DLQ + alarm for stuck messages |

## Data Model (core tables, see migrations for full DDL)

- `accounts(id, owner_type, owner_id, currency, status)`
- `ledger_entries(id, payment_id, account_id, direction[debit|credit], amount_minor, currency, created_at)`
  — a **CHECK** constraint plus a DB trigger enforces that entries for a given `payment_id` sum to
  zero before commit, so an application bug literally cannot unbalance the books.
- `payments(id, idempotency_key, source_account_id, dest_account_id, amount_minor, currency, status, created_at, updated_at)`
- `idempotency_keys(key, request_hash, payment_id, response_snapshot, created_at)` — unique index
  on `key`; a replayed request with a *different* body under the same key is rejected (422), not
  silently reused, to catch client bugs rather than mask them.
- `outbox_events(id, aggregate_id, event_type, payload, published, published_at, attempts)`
- `audit_log(id, actor, action, entity_type, entity_id, before, after, created_at)` — append-only,
  no UPDATE/DELETE grants even for the application role.

## Deployment Topology (target: AWS, 2 regions, 3 AZs each)

- **Primary region** active, **secondary region** warm-standby (Aurora Global Database, read
  replica promoted on region failure — see `04-failure-recovery-design.md` for the drill).
- EKS cluster spans 3 AZs; `payments-api` Deployment has pod anti-affinity across AZs and a
  PodDisruptionBudget so voluntary disruptions (node drains, cluster upgrades) never drop below
  quorum capacity.
- Aurora writer in one AZ, synchronous readable replicas in the other two; automated failover.

## Request Flow — Create Payment (happy path)

1. ALB/WAF → pod. `AuthMiddleware` validates JWT (OIDC), extracts `client_id`/scopes.
2. `RateLimitMiddleware` enforces per-client token bucket (protects against retry storms /
   thundering herd from a single misbehaving client without punishing others — bulkhead per
   client).
3. `IdempotencyMiddleware` hashes the request body, looks up `idempotency_keys` by
   `Idempotency-Key` header. Hit with matching hash → return stored response (short-circuit,
   no DB write). Hit with different hash → `409 idempotency_key_conflict`. Miss → proceed.
4. `PaymentService.Create`: opens a DB transaction at `SERIALIZABLE` isolation, validates account
   status/currency/sufficient balance, inserts `payments` row (`status=completed` for this
   synchronous ledger-only slice — no external settlement hop), inserts two balanced
   `ledger_entries`, inserts one `outbox_events` row (`payment.completed`), inserts one
   `idempotency_keys` row with the response snapshot, commits.
5. Response returned to client (P95 target 250ms).
6. Outbox relay goroutine (running independently, polling every 200ms with jitter) picks up the
   new outbox row, publishes to SQS, marks `published=true`.

## Request Flow — Failure Injected at Each Step

See `04-failure-recovery-design.md` for the full matrix; summarized: any failure before the DB
commit → no partial state (single transaction, atomic). Any failure after commit but before
response reaches the client → client retries with same idempotency key → step 3 short-circuits to
the already-committed result, no duplicate. Any failure after commit but before outbox publish →
relay picks it up on its next poll (durable in the DB row, not lost).
