# Documentation Index

Sixty-seven documents, all derived from one. This page tells you which one answers your question and
whether it is written for you.

Every document in this tree is **subordinate to
[`spec/00-design-baseline.md`](spec/00-design-baseline.md)**. Where a document disagrees with the
baseline, the baseline wins and the document is a defect. That relationship is stated in the header
of every file, and it is what makes it safe to read any one of them in isolation.

**Reading orders**

- *New engineer* — [`diagrams/01`](diagrams/01-system-context.md) → [`diagrams/02`](diagrams/02-high-level-design.md) → [`diagrams/20`](diagrams/20-end-to-end-sequence.md) → [`architecture.md`](architecture.md) → the plane document for your area
- *Design reviewer* — [`spec/00-design-baseline.md`](spec/00-design-baseline.md) end to end, then [`adr/README.md`](adr/README.md)
- *Payments deep dive* — [`payment-flow.md`](payment-flow.md) → [`data-plane.md`](data-plane.md) → [`state-machines.md`](state-machines.md) → [`diagrams/08`](diagrams/08-payment-flow.md)–[`10`](diagrams/10-gateway-failover.md)
- *Security or compliance review* — [`security.md`](security.md) → [`compliance.md`](compliance.md) → [`multi-tenancy.md`](multi-tenancy.md) → [`diagrams/17`](diagrams/17-security-architecture.md)
- *On-call and infrastructure* — [`failure-handling.md`](failure-handling.md) → [`observability.md`](observability.md) → [`disaster-recovery.md`](disaster-recovery.md) → [`deployment.md`](deployment.md)

---

## 1. Specifications — the normative layer

`docs/spec/` is where requirements and the binding design live. Numbering skips 07 and 08; the
traceability matrix is 09.

| Document | Answers | Read it if you are |
|---|---|---|
| [`spec/00-design-baseline.md`](spec/00-design-baseline.md) | **Everything, normatively.** Scope and non-goals, the twelve-entry ambiguity register, ubiquitous language, the nine bounded contexts, the layering rule, the nine deployables, IDs, money, both state machines, gateway health, the onboarding workflow, the 17-stage pipeline and the timeout rule, the event catalog, the idempotency contract, CAP per operation, multi-tenancy, the PCI boundary, NFR targets, the API surface, the error model, the validation contract, the observability contract, the configuration document, the failure catalog, the repository layout, traceability, and the definition of done. 1 089 lines. | **Everyone, eventually. A reviewer, first.** |
| [`spec/01-business-requirements.md`](spec/01-business-requirements.md) | *Why* the platform exists, who it serves, and what business outcomes it must produce — `BR-01`…`BR-38`, numbered and testable | Product, commercial, anyone questioning scope |
| [`spec/02-functional-requirements.md`](spec/02-functional-requirements.md) | *What the system does* — `FR-01`…`FR-91`, grouped by bounded context, each realising one or more `BR` | Engineers building or reviewing a feature; QA |
| [`spec/03-non-functional-requirements.md`](spec/03-non-functional-requirements.md) | The qualities it must exhibit — latency, throughput, scale, availability, durability, security, privacy, compliance, observability, operability, cost — as `NFR-01`…`NFR-61`, each with the mechanism that delivers it | SRE, architects, anyone signing off on a target |
| [`spec/04-domain-model.md`](spec/04-domain-model.md) | The binding domain model: aggregates, value objects, invariants, and the **complete relational schema** | Backend engineers, DBAs |
| [`spec/05-bounded-contexts.md`](spec/05-bounded-contexts.md) | What each of the nine contexts owns, publishes and depends on, and the integration pattern between each pair | Architects, tech leads deciding where code belongs |
| [`spec/06-code-conventions.md`](spec/06-code-conventions.md) | The fifteen binding rules for every Go file, the naming scheme, and the reference implementations to read before writing new code | **Anyone writing Go here — read this first** |
| [`spec/09-traceability.md`](spec/09-traceability.md) | Requirement → design section → package → test, for every requirement. **Generated** by `scripts/traceability.sh`; CI fails on drift, an orphan requirement or an orphan test | Auditors, compliance, anyone proving coverage |

---

## 2. Architecture

| Document | Answers | Read it if you are |
|---|---|---|
| [`architecture.md`](architecture.md) | The system-level decomposition: the five planes and why the plane is the primary boundary, the control loop with four concrete instances of it closing, the C4 container and component views, why `payment-api` and `payment-orchestrator` are two binaries, the architectural styles and where each applies, a **15-entry trade-off register** with the rejected options steelmanned, CAP positioning per operation, SOLID/DRY/KISS applied concretely, twelve-factor point by point, and the scaling arithmetic for 5 000 TPS | **An architect evaluating this design** |
| [`lld.md`](lld.md) | Package-by-package design of `internal/`, the composition-root and wiring pattern, the concurrency model, resource-pool sizing arithmetic, and the sequence-level behaviour of the money path | An engineer about to write code in an unfamiliar package |
| [`state-machines.md`](state-machines.md) | Every FSM in the platform as an explicit transition table with guards, side effects, emitted events, terminal states, forbidden transitions and the dual enforcement in domain and database | Engineers, QA, support, auditors |
| [`events.md`](events.md) | The binding contract for every published event: envelope, versioning policy, payload schemas, topic configuration, ordering guarantees, the transactional outbox, idempotent consumption and DLQ handling | Anyone producing or consuming an event |
| [`multi-tenancy.md`](multi-tenancy.md) | How isolation is achieved, enforced, tested and operated — from the token claim to the RLS policy to noisy-neighbour controls and the tenant lifecycle | Security architects, backend engineers, anyone adding a table |

---

## 3. The planes

One document per plane, describing runtime behaviour rather than structure. The validation plane has
no deployable; the observability plane's document is [`observability.md`](observability.md) under
Operations.

| Document | Answers | Read it if you are |
|---|---|---|
| [`control-plane.md`](control-plane.md) | How **desired state** is authored, validated, versioned, published, propagated, rolled back and audited — and the rules that keep all of it off the payment hot path | Platform engineers, operators |
| [`automation-plane.md`](automation-plane.md) | The durable workflow/saga engine behind the `WorkflowEngine` port, the `merchant-onboarding@v1` definition in full, the worker execution model, and the guarantees that make a crashed worker safe | Workflow and backend engineers |
| [`validation-plane.md`](validation-plane.md) | The seven-level engine, the **complete 243-rule catalog** with subject, precondition, predicate, severity, error code and remediation for each, and the shadow → warn → enforce process for changing it | Engineers, QA, support triaging a rejection |
| [`data-plane.md`](data-plane.md) | The money path at runtime: the 17-stage request pipeline with its latency budgets, scaling and bulkheads, the orchestrator's crash-recoverable ordering, smart routing, the risk engine, and idempotency under concurrency | **Data-plane engineers, SRE, payments specialists** |
| [`onboarding.md`](onboarding.md) | What a merchant submits, what the platform does with it, what a human decides, how gateway accounts are actually provisioned, and how a broken onboarding is unwound | Onboarding engineers, product, compliance |
| [`payment-flow.md`](payment-flow.md) | End-to-end narratives for every scenario: actors, transitions, events, ledger entries, idempotency behaviour, failure branches and reconciliation | **Payments specialists, integration partners** |

---

## 4. Operations

| Document | Answers | Read it if you are |
|---|---|---|
| [`failure-handling.md`](failure-handling.md) | The complete failure catalog with detection, response and recovery per mode; the resilience toolkit with concrete parameters; the retry-safety table; the degradation ladder; backpressure design; dead-letter handling | **On-call, before you are on-call** |
| [`observability.md`](observability.md) | Metrics, traces, logs and audit; how they correlate; the SLO burn-rate alert rules with their exact expressions; the dashboards; and the automation those signals drive | SRE, engineers instrumenting anything |
| [`disaster-recovery.md`](disaster-recovery.md) | RPO/RTO per data store, the multi-region topology and why money movement is active/passive, the failover and failback procedures **with real commands**, restore-drill evidence, and the proof that a region failover cannot create a duplicate payment | SRE, incident commanders, auditors |
| [`deployment.md`](deployment.md) | How the nine deployables run: Kubernetes topology and workload configuration, the AWS substrate, GitOps and progressive delivery, the CI/CD gates, zero-downtime migrations, and environment policy | Cloud architects, platform engineers, release managers |
| [`testing.md`](testing.md) | The test pyramid as applied here, what each level asserts, the named failure-scenario tests, the critical-path registry concept, and the exact commands for local and CI runs | Everyone writing tests; QA leads |
| [`runbooks/README.md`](runbooks/README.md) | **35 runbooks** behind an index, one per distinct `runbook_url` referenced from the alert rules in `deployments/` and from [`security.md`](security.md). Each carries the alert that fires it, the first-five-minutes triage, the decision tree, and what to check afterwards. `scripts/check-runbook-links.sh` asserts every reference resolves, that every alert with `page: "true"` has one, and that no runbook is orphaned | **On-call, at 3 a.m.** |

---

## 5. Security and compliance

| Document | Answers | Read it if you are |
|---|---|---|
| [`security.md`](security.md) | The binding security design: trust model, layered controls, identity and authentication, authorization, secret management and the `Secret[T]` type, the threat model, supply-chain controls, and incident response | **Security architects** |
| [`compliance.md`](compliance.md) | The regulatory position: PCI DSS scope and how it is *enforced* rather than asserted, PSD2/SCA, GDPR and data residency, AML/KYC, retention schedules, and the auditability design that makes all of it evidenceable | Compliance, audit, security architects |
| [`multi-tenancy.md`](multi-tenancy.md) | (Also listed under Architecture.) The isolation matrix and the isolation guard, defence in depth from token claim to RLS policy to the negative test that proves the database alone would stop a cross-tenant read | Security architects, auditors |

---

## 6. Decisions

[`adr/README.md`](adr/README.md) is the index, the ADR process, the required record structure, and
the supersession rules. **Twenty-four decisions are indexed; nineteen exist as files.** ADR-001
through ADR-005 predate this expansion and are recorded in the index for continuity only — they have
not been renumbered or rewritten, and ADR-003's partial supersession by ADR-020 is documented in the
superseding record.

Every record follows the same template — Context, Decision, Options considered with **at least three
real options and the rejected ones steelmanned**, Consequences, Risks with detection signals,
Validation (the metric or test that would show the decision was wrong), and Revisit criteria — and
every one names a mechanical check, because an architectural decision without a CI gate, test or
metric is not done.

| ADR | Decision |
|---|---|
| 001–005 | *Index entries only:* Go as the language · PostgreSQL as the primary store · SQS as the messaging substrate (superseded in part by 020) · idempotency records and an append-only ledger (refined by 009, 010, 012) · Kubernetes as the deployment platform |
| [006](adr/ADR-006-five-plane-decomposition.md) | Five-plane decomposition as the primary architectural boundary — nine binaries, not twenty-five |
| [007](adr/ADR-007-control-plane-data-plane-independence.md) | Control and data plane independence — 99.99 % must not inherit 99.9 % |
| [008](adr/ADR-008-pooled-multi-tenancy-with-rls.md) | Pooled multi-tenancy with forced PostgreSQL RLS; siloed tier without a code fork |
| [009](adr/ADR-009-postgres-authoritative-idempotency.md) | **Postgres-authoritative idempotency; Redis is a latency accelerator only** |
| [010](adr/ADR-010-at-least-once-effectively-once.md) | At-least-once delivery, effectively-once effect — outbox + dedup + database invariants, stacked |
| [011](adr/ADR-011-gateway-agnostic-adapter-spi.md) | Gateway-agnostic core with an adapter SPI and a machine-checked substitutability contract |
| [012](adr/ADR-012-payment-attempt-first-class-aggregate.md) | **Payment attempt as a first-class aggregate — a partial unique index makes double-charging structurally impossible** |
| [013](adr/ADR-013-timeout-leaves-payment-processing.md) | **Gateway timeouts leave the payment in `PROCESSING` — no timer may fail a payment** |
| [014](adr/ADR-014-owned-workflow-engine-behind-port.md) | An owned durable workflow engine behind a port, with a maintained Temporal adapter keeping the decision reversible |
| [015](adr/ADR-015-scored-routing-with-persisted-plan.md) | Scored routing with a persisted, auditable, recomputable plan |
| [016](adr/ADR-016-validation-plane-rule-registry.md) | Validation as a plane with a stable rule registry — no DSL on the money path |
| [017](adr/ADR-017-pci-scope-minimisation.md) | **PCI scope minimisation — PAN never enters the platform, enforced not asserted** |
| [018](adr/ADR-018-money-as-integer-minor-units.md) | Money as `int64` minor units plus an explicit currency value object; no floating point, ever |
| [019](adr/ADR-019-fail-static-configuration.md) | Fail-static configuration with a staleness cliff — neither fail-open nor fail-closed |
| [020](adr/ADR-020-kafka-event-backbone.md) | Kafka as the event backbone; states exactly what changed since ADR-003 chose SQS |
| [021](adr/ADR-021-active-passive-money-active-active-control.md) | Active/passive money state, active/active control plane — one writer, so invariant I3 survives |
| [022](adr/ADR-022-rest-external-grpc-internal.md) | REST + RFC 9457 externally, Protobuf over mTLS gRPC internally — one taxonomy, two encodings |
| [023](adr/ADR-023-explicit-constructor-wiring.md) | Explicit constructor wiring in composition roots — no DI framework, no service locator |
| [024](adr/ADR-024-monorepo-single-go-module.md) | Monorepo, one Go module, and build-failing architectural fitness functions |

---

## 7. Diagrams

[`diagrams/README.md`](diagrams/README.md) is the index, with the question each diagram answers, its
primary audience, and four role-based reading orders. Twenty diagram documents contain **42 Mermaid
diagrams**; a file with more than one splits by heading rather than producing an unreadable single
graph. Node labels avoid unquoted punctuation so every diagram parses under strict Mermaid.

| # | Diagram | Question it answers |
|---|---|---|
| [01](diagrams/01-system-context.md) | System Context | Who talks to the platform, what does it talk to, and where does trust change hands? |
| [02](diagrams/02-high-level-design.md) | High-Level Design (C4 L2) | Which of the nine deployables owns this, and what breaks if it is down? |
| [03](diagrams/03-control-plane.md) | Control Plane | How does declared desired state become effective configuration, and how is drift closed? |
| [04](diagrams/04-automation-plane.md) | Automation Plane | How does a long-running saga survive a crash, and what happens when it must unwind? |
| [05](diagrams/05-validation-plane.md) | Validation Plane | Which of the seven levels rejects this input, where does it run, and with what error code? |
| [06](diagrams/06-data-plane.md) | Data Plane | What happens at each of the 17 pipeline stages, and where are the bulkheads? |
| [07](diagrams/07-merchant-onboarding.md) | Merchant Onboarding Saga | What are the twelve steps, and what is undone if step 10 fails? |
| [08](diagrams/08-payment-flow.md) | Payment Flow | How does a payment go authorize → capture → settle, and where is idempotency enforced? |
| [09](diagrams/09-gateway-routing.md) | Gateway Routing | Why did this payment go to this gateway, and what could have excluded it? |
| [10](diagrams/10-gateway-failover.md) | Gateway Failover | When may we retry elsewhere, and when must we absolutely not? |
| [11](diagrams/11-webhook-flow.md) | Webhook Flow | How is an untrusted gateway callback verified, deduped and applied without corrupting state? |
| [12](diagrams/12-event-architecture.md) | Event Architecture | How does a state change become an event, and what happens to a poison message? |
| [13](diagrams/13-state-machines.md) | State Machines | Is this transition legal, and what guards it? |
| [14](diagrams/14-database-architecture.md) | Database Architecture | Where does this table live, how is it partitioned, and what enforces tenant isolation? |
| [15](diagrams/15-kubernetes-architecture.md) | Kubernetes Architecture | Where does each workload run, and what stops one plane starving another? |
| [16](diagrams/16-aws-architecture.md) | AWS Architecture | What is the cloud substrate, and what is the multi-region posture? |
| [17](diagrams/17-security-architecture.md) | Security Architecture | What are the trust zones, how does identity flow, and where is the PCI boundary? |
| [18](diagrams/18-observability-architecture.md) | Observability Architecture | How does telemetry reach a backend, and how does observation change routing? |
| [19](diagrams/19-disaster-recovery.md) | Disaster Recovery | What replicates where, and what is the ordered failover procedure? |
| [20](diagrams/20-end-to-end-sequence.md) | End-to-End Sequence | What is the whole story, from signup to settled payment to the feedback loop? |

---

## 8. Documentation outside `docs/`

Twelve more READMEs live next to what they describe, because a convention explained three
directories away from the file it governs is a convention nobody reads.

| Document | Covers |
|---|---|
| [`../README.md`](../README.md) | The front door: what this is, what it is not, the decisions that matter, quick start, verification, statistics, and an honest status and limitations section |
| [`../CONTRIBUTING.md`](../CONTRIBUTING.md) | Layering rules and why they are mechanical, the definition of done, and the recipes for adding a gateway, a validation rule, an event, a state transition and a migration |
| [`../SECURITY.md`](../SECURITY.md) | Vulnerability reporting, supported versions, the PCI scope statement, and credential-exposure response |
| [`../api/README.md`](../api/README.md) | The contract directory: OpenAPI, Protobuf, event schemas and the error catalogue |
| [`../api/events/README.md`](../api/events/README.md) | Event schema conventions and the compatibility promise |
| [`../migrations/README.md`](../migrations/README.md) | Expand/contract, forward-only rollback, the destructive-statement marker, and the pre-merge checklist |
| [`../config/README.md`](../config/README.md) | What belongs in `config/` and what never does; the `secret://` reference model; configuration precedence |
| [`../tests/README.md`](../tests/README.md) | How the suites are laid out, their build tags, and what each needs to run |
| [`../tests/load/README.md`](../tests/load/README.md) | The four k6 scenarios and how to run them |
| [`../terraform/README.md`](../terraform/README.md) | Module layout, environment stacks and the validation tooling |
| [`../terraform/policies/README.md`](../terraform/policies/README.md) | The reference IAM policy documents |
| [`../helm/payments-platform/README.md`](../helm/payments-platform/README.md) · [`../helm/charts/pp-common/README.md`](../helm/charts/pp-common/README.md) | The umbrella chart and the shared library chart |
| [`../deployments/argocd/README.md`](../deployments/argocd/README.md) · [`../deployments/grafana/README.md`](../deployments/grafana/README.md) | GitOps application definitions; dashboard provisioning |
