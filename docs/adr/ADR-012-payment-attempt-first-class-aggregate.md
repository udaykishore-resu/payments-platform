# ADR-012: A payment attempt is a first-class aggregate; failover creates a new attempt

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §2 (Payment Attempt), §9 (payment FSM, invariants I3–I5), §9.1 (attempt outcomes), §14.4 (gateway idempotency), §1.3 ambiguity A10 of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Refines ADR-004; depended on by ADR-013, ADR-015

## Context

The single most expensive failure mode in a payment orchestrator is charging a customer twice. It
arises from one specific pattern: a gateway call whose outcome is unknown, followed by a retry
against a different gateway, followed by both authorizations succeeding. The customer is charged
twice, the merchant faces a chargeback and a fine, and the platform's authorization data becomes
untrustworthy.

The forces:

1. **Failover is a business requirement.** If Stripe is degraded, the payment should be attempted
   on Adyen. Without failover, a gateway incident is a revenue incident (§10 exists precisely to
   drive this).
2. **A retry and a failover are different operations with different safety properties.** A retry
   to the *same* gateway with the *same* idempotency key is safe: the gateway dedupes. A failover
   to a *different* gateway is genuinely a new authorization attempt against a different acquirer
   — the first one may still succeed.
3. **If the payment row carries the gateway reference, failover must mutate it.** Overwriting
   `gateway_ref` and `gateway_id` destroys the record of the first attempt. The first attempt may
   still be in flight; if it later succeeds, we have no row to reconcile it against, no
   idempotency key to look it up with, and no way to void it. The evidence needed to *detect* the
   double charge has been overwritten by the operation that caused it.
4. **The audit and reconciliation story needs per-attempt granularity.** "Which gateway did we try,
   when, with what key, and what happened?" is a question asked during every dispute, every
   reconciliation exception, and every gateway contract negotiation about authorization rates.
5. **Structural beats procedural.** A rule enforced by a database constraint holds under bugs,
   under races, under a bad deploy, and under an operator running SQL by hand. A rule enforced by
   careful code holds until someone is careless.

What breaks if we choose wrong: double charges, invisible because the record of the first attempt
was destroyed by the second.

## Decision

**`PaymentAttempt` is a first-class aggregate with its own identity (`att_` prefix, §6), its own
lifecycle, and its own row. A payment has 1..N attempts. Failover creates a new attempt; it never
mutates an existing one. Attempt rows are append-only in every field that describes the gateway
interaction.**

1. **The attempt row is written and committed *before* the gateway call** (§12 stage 13). If we
   crash between the write and the call, the attempt exists in `DISPATCHED` state with a
   deterministic key and is reconcilable. If the row were written after, a crash would leave money
   possibly moved with no record at all.
2. **Attempt FSM** (§9.1): `PENDING → DISPATCHED → { SUCCESS | DECLINED | ERROR | TIMEOUT_UNKNOWN }`.
   Terminal outcomes are immutable. A `TIMEOUT_UNKNOWN` attempt is resolved by reconciliation into
   `SUCCESS` or `DECLINED` — that resolution is the *only* permitted transition out of it, and it
   is the reconciler's write, not the request path's.
3. **The gateway idempotency key is derived from the attempt** (§14.4, A10):
   `base32(HMAC-SHA256(attempt_id, gateway_salt))[:32]`, stored on the attempt row before dispatch.
   Consequences that fall out for free:
   - a transport retry to the same gateway reuses the same key → the gateway dedupes;
   - a failover creates a new attempt → a new key → correctly a *new* authorization;
   - the key is reproducible after a crash, so the reconciler can look the transaction up.
   The **client's** idempotency key is a different concern at a different layer (ADR-009) and is
   never sent to a gateway.
4. **Invariant I3, enforced by the database**: a partial unique index on
   `(payment_id) WHERE outcome = 'SUCCESS'`. At most one attempt per payment may be in a
   successful terminal state. A second success **cannot be committed**. Per amendment A-02, both
   `payments` and `payment_attempts` are range-partitioned on a `partition_month` derived from the
   *payment's* ULID timestamp, so all attempts of a payment share its partition and the partial
   unique index constrains the full set rather than one month of it.
5. **Failover eligibility is a domain rule, not a retry policy.** Only `ERROR`, and `DECLINED`
   whose reason is in the *retryable decline* set (issuer unavailable, soft do-not-honour, network
   error), may produce a new attempt. `TIMEOUT_UNKNOWN` **never** produces a failover
   automatically (ADR-013). A hard decline (stolen card, invalid account) never fails over —
   retrying it elsewhere is card-testing behaviour and gets the platform de-registered by the
   schemes.
6. **The routing plan that produced each attempt is persisted** and referenced from it, so the
   decision is reproducible (ADR-015).

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Attempt as a first-class aggregate, append-only (chosen)** | Makes double-charging *structurally* impossible via I3 rather than merely unlikely; preserves the full history needed for reconciliation, disputes and gateway performance analysis; gives each gateway interaction its own deterministic idempotency key, which is what makes retry-vs-failover safe; a crashed dispatch leaves a reconcilable record; the routing plan and the attempt tell a complete story | An extra row and an extra write per attempt on the hot path; the payment's "current" gateway state is a derived query over attempts rather than a column read (mitigated by a denormalized pointer to the latest attempt, maintained in the same transaction); more complex queries and a partitioning constraint that must be respected | **Accepted** |
| **Mutate the payment row on failover (single row, `gateway_id` + `gateway_ref` columns)** | Simplest schema; one row per payment; the "current gateway" is a column read; fewer writes on the hot path; every query is simpler | Destroys the record of the previous attempt at exactly the moment it becomes critical — the previous attempt may still be in flight and may still succeed. There is then no attempt ID to derive a lookup key from, no row to reconcile against, and no way to void the orphan authorization. It also makes I3 inexpressible: with one row there is nothing to constrain. This is the option that looks obviously simpler until the first double charge, and it is the specific design this ADR exists to forbid | Rejected |
| **Attempts as an append-only event log on the payment (no attempt table)** | Full history preserved; no extra aggregate; naturally append-only; fits an event-sourced style | A JSONB array or event stream cannot carry a partial unique index, so I3 becomes application-enforced — the exact weakening this ADR exists to prevent; querying "attempts for gateway X in the last hour" for routing feedback becomes a scan; the deterministic idempotency key needs a stable per-attempt identity anyway, which is an aggregate by another name | Rejected |
| **Attempt as a value object owned by the Payment aggregate (no independent identity)** | Keeps the aggregate boundary tight and DDD-clean; loading a payment loads its attempts; no cross-aggregate consistency questions | The reconciler must address a *single attempt* directly, days later, without loading and locking the whole payment — and locking the payment to resolve one stale attempt would serialize reconciliation against live traffic on that payment; the deterministic gateway key requires attempt identity to be stable and externally referenceable; the webhook path arrives with a gateway reference and must resolve to one attempt without a scan | Rejected — attempts are inside the payment's *consistency* boundary (written in the same transaction) but have their own identity |
| **Attempt row written *after* the gateway call returns** | Avoids a write for calls that fail fast; slightly lower latency on the happy path | A crash between call and write means money may have moved with **no record whatsoever** — unreconcilable, undetectable, and unrecoverable. Saves ~1 ms and costs the entire safety property | Rejected |

## Consequences

### Positive

- **I3 makes a second successful authorization on one payment a database error.** No code path,
  no race, no bad deploy and no manual SQL can commit it.
- A `TIMEOUT_UNKNOWN` attempt always has a durable row and a reproducible key, so the
  reconciliation loop in ADR-013 can always close.
- Per-gateway authorization rates, latencies and decline distributions are directly queryable per
  attempt — this is the data the routing engine's `S(g)` factor consumes (ADR-015) and the data we
  take into gateway commercial reviews.
- Disputes and support investigations have a complete, ordered narrative per payment.
- Failover is expressible as "create a new attempt", which is a much easier thing to reason about
  and test than "mutate state safely under concurrency".

### Negative

- One additional row and one additional index maintenance cost per attempt on the hot path; at
  5 000 TPS with a ~1.05 average attempts-per-payment, roughly 5 250 attempt rows/s.
- "What is the current gateway for this payment?" becomes a join or a maintained pointer. We
  maintain a `latest_attempt_id` on the payment in the same transaction, which is denormalization
  we must keep honest.
- The partitioning constraint (A-02) is subtle and easy to break: partitioning attempts by their
  *own* creation month would silently weaken I3. It needs a test, not a comment.
- More rows to retain for 7 years (§17.3), increasing storage and archival volume.

### Neutral / accepted costs

- Attempt count per payment becomes a metric worth watching; a rising average means gateway
  health or routing quality is degrading, which is useful but is one more signal to interpret.
- The domain model has one more aggregate to explain during onboarding of new engineers.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| Partial unique index defeated by partitioning | Low (found once, in A-02) | **Critical** — I3 silently gone | Both tables partition on `partition_month` derived from the payment's ULID; `TestAttemptSharesPaymentPartition` asserts an attempt created months after the payment lands in the payment's partition; a schema check asserts the index exists on every partition | The test; a periodic production assertion counting payments with > 1 `SUCCESS` attempt (must be exactly zero) |
| Failover triggered on `TIMEOUT_UNKNOWN` by a future change | Medium | **Critical** — the double-charge path | The failover-eligibility function is a pure domain function with exhaustive table-driven tests including every outcome × decline-reason combination; `TIMEOUT_UNKNOWN` is not a case that returns eligible | Unit test; `pp_payments_total` correlation between timeouts and subsequent attempts |
| Hard decline retried on another gateway | Medium | High — scheme sanctions, possible de-registration | Decline reasons are normalized with a Hard/Soft classification by the ACL (ADR-011) and the contract suite asserts the classification for each gateway's known hard codes; failover eligibility checks the classification, not the raw code | Count of failovers following a `Hard` decline (must be zero); scheme fraud/retry ratio reports |
| Attempt row written after dispatch by a refactor | Low | **Critical** | The dispatch function takes an already-persisted attempt as a parameter — there is no API that dispatches without one; asserted by a test that crashes between persist and dispatch and finds the row | Crash-injection test; orphan gateway transactions found by reconciliation |
| Attempt volume degrades write throughput | Medium | Medium | Partitioned tables with partition-drop archival; indexes limited to those routing/reconciliation actually use | Write p99; index bloat; `pg_stat_user_tables` |
| Denormalized `latest_attempt_id` drifts | Medium | Low–Medium | Updated in the same transaction as attempt creation; a consistency check job compares it against `max(created_at)` per payment | Consistency job exception count |

## Validation

- **The invariant test:** attempt to insert two `SUCCESS` attempts for one payment, from two
  concurrent transactions, and assert the second fails at the database level with a unique
  violation — with the application-level guard deliberately disabled, so the test proves the
  constraint and not the code.
- **Failover test:** induce a `TIMEOUT_UNKNOWN` on gateway A and assert (a) no second attempt is
  created, (b) the payment stays `PROCESSING`, (c) a reconciliation task exists. Then induce an
  `ERROR` and assert a *new* attempt with a *different* gateway idempotency key is created.
- **Key determinism test:** the gateway idempotency key derived for an attempt must be identical
  after a process restart and must differ across attempts. Asserted with a fixed salt fixture.
- **The production metric:** count of payments with more than one `SUCCESS` attempt. Target and
  tolerance: **exactly zero, forever**. A single occurrence is a Sev-1.
- **Duplicate-charge rate:** zero customer-reported double charges attributable to failover per
  quarter.

## Revisit criteria

Reopen if:

1. Attempt-row write volume becomes a demonstrable bottleneck — the response is partitioning and
   archival tuning, not merging attempts back into the payment row.
2. A gateway relationship requires a fundamentally different attempt model (e.g. a network-level
   retry protocol where the acquirer, not us, owns the retry decision and reports it back).
3. Multi-currency or multi-acquirer split settlement introduces a legitimate case for two
   concurrent successful authorizations on one payment — that would require amending I3, which is
   a change to the platform's central safety property and demands its own ADR and an explicit
   risk acceptance.
