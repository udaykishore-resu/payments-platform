# Architecture Decision Records

This directory holds the Architecture Decision Records (ADRs) for the multi-tenant payment gateway
onboarding and payment orchestration platform.

Every ADR is subordinate to **`docs/spec/00-design-baseline.md`**. The baseline is the single
source of truth; an ADR that contradicts it is a defect in the ADR, not in the baseline. Each
record cites the baseline section it derives from.

**ADR-001 through ADR-005 predate this expansion.** They were written for an earlier, narrower
version of the project and are recorded here for continuity. They have not been renumbered or
rewritten. Where a later decision changes one of them, the later ADR says so explicitly and states
what changed — see ADR-020's treatment of ADR-003.

## Index

| # | Title | Status | Date | Baseline § | Summary |
|---|---|---|---|---|---|
| 001 | Go as the implementation language and runtime | Accepted | *pre-expansion* | §4, §5 | Go for all services: static binaries, predictable latency without a JIT, first-class concurrency, and a small runtime footprint per pod. |
| 002 | PostgreSQL as the primary datastore | Accepted | *pre-expansion* | §9, §15 | PostgreSQL (Aurora) as the system of record: transactional guarantees, constraint enforcement and RLS are what the money invariants rest on. |
| 003 | SQS as the asynchronous messaging substrate | Accepted · **superseded in part by [ADR-020](ADR-020-kafka-event-backbone.md)** | *pre-expansion* | §13 | SQS for asynchronous work in the original narrow scope. Superseded for platform event distribution; still valid for point-to-point work queues with no ordering or replay requirement. |
| 004 | Idempotency records and an append-only ledger | Accepted · refined by [ADR-009](ADR-009-postgres-authoritative-idempotency.md), [ADR-010](ADR-010-at-least-once-effectively-once.md), [ADR-012](ADR-012-payment-attempt-first-class-aggregate.md) | *pre-expansion* | §14, §9 | Durable idempotency records and a strictly append-only ledger as the basis for safe retries and reconciliation. |
| 005 | Kubernetes as the deployment platform | Accepted | *pre-expansion* | §5, §18 | Kubernetes for all deployables: per-plane scaling, PDBs, anti-affinity and rolling deploys. |
| 006 | [Five-plane decomposition as the primary architectural boundary](ADR-006-five-plane-decomposition.md) | Accepted | 2026-08-26 | §2, §3, §5 | The plane — not the aggregate — is the primary deployment boundary; bounded contexts remain the code boundary. Nine binaries, not twenty-five. |
| 007 | [Control plane and data plane independence](ADR-007-control-plane-data-plane-independence.md) | Accepted | 2026-08-26 | §5, §15, §18 | Separate deployables, separate scaling, and **no synchronous data-plane dependency on the control plane** — 99.99 % must not inherit 99.9 %. |
| 008 | [Pooled multi-tenancy with PostgreSQL RLS](ADR-008-pooled-multi-tenancy-with-rls.md) | Accepted | 2026-08-26 | §16, A3 | Pooled shared schema with forced Row-Level Security by default; siloed tier available per tenant tier without a code fork. |
| 009 | [Postgres-authoritative idempotency](ADR-009-postgres-authoritative-idempotency.md) | Accepted | 2026-08-26 | §14, A6 | Postgres unique index is authoritative; Redis is a latency accelerator only. Concurrent duplicates get `409`, never a blocking wait. |
| 010 | [At-least-once delivery, effectively-once effect](ADR-010-at-least-once-effectively-once.md) | Accepted | 2026-08-26 | §13.4, §13.5, A8 | Transactional outbox + consumer dedup table + database invariants, stacked in depth. Exactly-once delivery does not exist; exactly-once effect does. |
| 011 | [Gateway-agnostic core with an adapter SPI](ADR-011-gateway-agnostic-adapter-spi.md) | Accepted | 2026-08-26 | §3, §11.4, §12 | One SPI, declarative capability descriptors, and a machine-checked substitutability contract. No gateway type ever reaches the domain. |
| 012 | [Payment attempt as a first-class aggregate](ADR-012-payment-attempt-first-class-aggregate.md) | Accepted | 2026-08-26 | §9, §9.1, §14.4, A10 | Failover creates a new attempt; it never mutates the old one. A partial unique index makes double-charging structurally impossible. |
| 013 | [Gateway timeouts leave the payment in `PROCESSING`](ADR-013-timeout-leaves-payment-processing.md) | Accepted | 2026-08-26 | §12.3, A7 | A timeout is an absence of information, not a failure. Only positive evidence resolves an ambiguous attempt — **no timer may fail a payment**. |
| 014 | [Owned durable workflow engine behind a port](ADR-014-owned-workflow-engine-behind-port.md) | Accepted | 2026-08-26 | §11, §1.2 | Postgres engine as the default because step completion and domain state commit in one transaction; a maintained Temporal adapter keeps the decision reversible. |
| 015 | [Scored routing with a persisted, auditable plan](ADR-015-scored-routing-with-persisted-plan.md) | Accepted | 2026-08-26 | §2, §10, §12, §23 | Hard filters then a weighted score over health, success rate, cost and latency; the plan, its inputs and its exclusions are persisted and recomputable. |
| 016 | [Validation as a plane with a stable rule registry](ADR-016-validation-plane-rule-registry.md) | Accepted | 2026-08-26 | §21, §12, §20 | Seven levels, typed `Rule[T]` contract, stable documented rule IDs, purity enforced by the import graph. No DSL on the money path. |
| 017 | [PCI scope minimisation](ADR-017-pci-scope-minimisation.md) | Accepted | 2026-08-26 | §17, A2 | PAN never enters the platform; tokenisation happens at the gateway edge. Enforced by an L1 PAN detector and allowlist logging, not by policy. |
| 018 | [Money as integer minor units](ADR-018-money-as-integer-minor-units.md) | Accepted | 2026-08-26 | §7 | `int64` minor units plus an explicit currency value object. No floating point in the money path, ever; largest-remainder allocation for splits. |
| 019 | [Fail-static configuration with a staleness cliff](ADR-019-fail-static-configuration.md) | Accepted | 2026-08-26 | §15, A5, §23 | Serve last-known-good configuration when the control plane is gone; at 15 minutes, fail closed for unknown merchants while existing ones keep processing. |
| 020 | [Kafka as the event backbone](ADR-020-kafka-event-backbone.md) | Accepted · **supersedes ADR-003 for event distribution** | 2026-08-26 | §13, §5, §16.1 | Per-aggregate partition keys, consumer groups, replay and compaction. States exactly what changed since ADR-003 chose SQS. |
| 021 | [Active/passive money state, active/active control plane](ADR-021-active-passive-money-active-active-control.md) | Accepted | 2026-08-26 | §15, §18, §24, A4, A9 | Single writer for money state so invariant I3 survives; active/active where it is cheap. Aurora Global promotion is deliberately manual. |
| 022 | [REST externally, gRPC internally](ADR-022-rest-external-grpc-internal.md) | Accepted | 2026-08-26 | §19, §20 | REST + JSON + RFC 9457 for merchants; Protobuf over mTLS gRPC between services. One error taxonomy, two encodings. |
| 023 | [Explicit constructor wiring in composition roots](ADR-023-explicit-constructor-wiring.md) | Accepted | 2026-08-26 | §4, §5, §25 | No DI framework, no wiring codegen, no service locator. Missing dependencies are compile errors; startup and shutdown ordering are explicit. |
| 024 | [Monorepo with a single Go module and CI fitness functions](ADR-024-monorepo-single-go-module.md) | Accepted | 2026-08-26 | §4, §25, §26, §27 | One module, one dependency graph, and build-failing architectural gates. A rule that only warns is a rule that is ignored. |

## The ADR process

### When an ADR is required

Write one when a decision is **costly to reverse** or **binding on other people's work**:

- a change to a plane boundary, a deployable unit, or the layering rules in §4;
- adopting, replacing or removing a stateful dependency (database, broker, cache, workflow engine);
- anything affecting money correctness — state machines, invariants, idempotency, delivery
  semantics, money representation;
- anything affecting the tenancy or isolation model;
- anything affecting PCI, residency or another regulatory boundary;
- a public contract choice: protocol, versioning policy, error model, pagination;
- an availability or consistency posture (CP/AP per operation, region topology, failure behaviour).

You do **not** need an ADR for a decision that is local to one package and cheap to reverse.
"Would a competent engineer joining in a year ask *why is it like this?* and be unable to find the
answer in the code?" is the practical test.

### What a record must contain

Use the template every record in this directory follows: Context (the forces, with numbers where
numbers exist), Decision (unambiguous and testable), Options considered (**at least three real
options, with the rejected ones steelmanned** — including the one a reasonable engineer would push
for, and why it lost), Consequences (positive, negative, and accepted costs), Risks and mitigations
with detection signals, Validation (the metric or test that would show the decision was wrong), and
Revisit criteria.

Two rules that carry most of the weight:

- **Steelman the rejected options.** If the "Cons" column reads like a strawman, the decision has
  not actually been made — it has been assumed. Several records here reject an option that is
  genuinely better on one dimension and say so.
- **Every ADR names a mechanical check.** Per §27, an architectural decision without a
  corresponding CI gate, test or metric is not done. See ADR-024.

### Review

Proposed records are reviewed by **Platform Architecture** plus the owners of every affected
bounded context (§3). Records touching money correctness (§7, §9, §14) additionally require review
by the payments domain owner; records touching §16 (tenancy) or §17 (PCI, residency) additionally
require security and compliance review. A record is `Proposed` until that review completes, then
`Accepted`.

### Supersession

Records are **immutable once accepted**. They are not edited to reflect a later change of mind.

- A later decision that replaces an earlier one is a **new ADR** whose *Supersedes / Related* line
  names the old one, and which explains **what changed** — not merely that it changed. ADR-020 is
  the worked example: it states the four specific things that changed since ADR-003 chose SQS, and
  it records precisely which parts of ADR-003 remain valid.
- The superseded record's **Status** line is updated to `Superseded by ADR-0NN` and this index is
  updated. That status change is the only permitted edit to an accepted record.
- Partial supersession is normal and should be stated as such ("superseded for X; still applies to
  Y"), because most decisions are replaced in scope rather than in whole.
- Numbers are never reused and records are never deleted. A wrong decision that was made and later
  reversed is part of the design history and is often the most useful record in the directory.
