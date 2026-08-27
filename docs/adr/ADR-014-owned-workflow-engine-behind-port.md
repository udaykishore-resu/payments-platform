# ADR-014: Own the durable workflow engine behind a port, with a Temporal adapter as the alternative implementation

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §11 (onboarding workflow definition and engine semantics), §1.2 (not a general-purpose BPM engine), §5 (`workflow-worker`), §25 (repository layout) of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Related to ADR-006 (Automation plane), ADR-010 (outbox)

## Context

Onboarding is a twelve-step, resumable, compensating, human-gated workflow (§11) that can span
**days** — step 3 (`await-kyc-decision`) has a 7-day timeout and step 11 (`compliance-review`) is
a manual gate with a 5-day timeout. It calls external vendors that fail, times out, and must be
compensated in strict reverse order. It must survive pod crashes without re-executing completed
steps and without losing a KYC submission.

The forces:

1. **This is a durable execution problem, and durable execution is hard to get right.** Leases,
   fencing, checkpointing, retry classification, compensation ordering, idempotent activities,
   and DLQ handling are all subtle. Writing them badly produces a system that loses onboarding
   cases or double-submits KYC.
2. **Temporal exists and solves exactly this.** It is mature, well-operated, has an excellent
   operator UI, and its retry/timeout/signal model maps closely onto §11's requirements.
3. **But the state we care about lives in our Postgres.** Step 2 submits KYC *and* moves the
   merchant to `KYC_PENDING` *and* writes an outbox event. With our own engine those are **one
   transaction**. With Temporal, "activity succeeded" commits to Temporal's store and "merchant is
   `KYC_PENDING`" commits to ours — two commits, one window, and closing the window requires every
   activity to be idempotent against its own partial completion.
4. **Volume is low and latency is irrelevant.** Onboarding is 50 000 merchants over the platform's
   lifetime, not 5 000 per second. A Postgres-backed engine polling with `FOR UPDATE SKIP LOCKED`
   is comfortably sufficient; there is no throughput argument for a specialised system here.
5. **Operational surface has a real cost.** Temporal self-hosted is another stateful cluster
   (its own Cassandra/Postgres, its own history shards, its own scaling and upgrade story, its own
   DR plan). Temporal Cloud removes that but adds a vendor on the critical path of merchant
   activation and puts workflow history — which includes KYC-adjacent metadata — in a third party.
6. **Auditability is a regulated requirement.** §17.3 requires KYC decisions and evidence retained
   ≥ 5 years, immutable. Our audit trail is hash-chained (§13.2, BC-9) under our own retention
   policy. Temporal's Event History plus archival is a different retention story that we would
   have to prove to an auditor.
7. **Determinism constraints are a real tax.** Temporal replays workflow code deterministically:
   no `time.Now`, no `rand`, no map iteration, no direct I/O in workflow functions. Our engine
   never replays — it reads `state`, `current_step`, `context` and executes the *next* step — so
   activity code has no determinism constraints at all.

What breaks if we choose wrong: either we build a mediocre workflow engine and lose onboarding
cases, or we adopt a heavyweight dependency whose transactional model does not match our data and
whose operational cost dwarfs the workload.

## Decision

**Define the workflow engine as a port (`internal/workflows/engine`) depending only on
`internal/domain` and `internal/application/ports`. Ship `engine/postgres` as the default
implementation. Maintain `engine/temporal` as a fully implemented alternative. The onboarding
definition and its activities (`internal/workflows/onboarding`) are identical under both.**

1. **The port is the contract**, not the implementation. Nothing in
   `internal/application/onboarding` knows which engine is wired; selection happens at the
   composition root (ADR-023).
2. **`engine/postgres` semantics** (§11): every step's result is checkpointed before the next
   begins; resuming reads state and executes the *next* step — **it does not replay**; an aborted
   instance runs compensations of completed steps in strict reverse order; a step that exhausts
   retries moves the instance to `FAILED` and the payload to `workflow_dlq` with the full error
   chain; a manual gate blocks until an authorized principal signals it and the signal is audited.
3. **The transactional property is the reason for the default.** A step commit updates
   `workflow_steps`, `workflow_instances`, the domain table (e.g. `merchants`) and `outbox_events`
   in **one** transaction. There is no window in which a step is recorded complete but its domain
   effect is not.
4. **Leases are fenced and use the database's clock.** `lease_expires_at` is compared against
   `now()` in Postgres, not the pod's clock, so node skew (§24 F-19) cannot cause double
   acquisition. A `lease_epoch` fences a resurrected worker's late write.
5. **Scope discipline** (§1.2): this is not a general-purpose BPM engine. It supports exactly what
   §11 requires — sequential and fan-out steps, timeouts, classified retries, compensation,
   signals, and versioned definitions. Features beyond that (child workflows, continue-as-new,
   cron schedules, dynamic branching) are explicitly out of scope and are the trigger to switch
   implementations rather than to extend ours.
6. **The Temporal adapter is maintained, not aspirational.** The mapping is documented
   step-by-step in `docs/automation-plane.md` §1.6; CI runs the onboarding definition's tests
   against both implementations. This is what makes the decision reversible.

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Own engine behind a port + Temporal adapter (chosen)** | Step completion and domain state commit in **one transaction** — no dual-write window at any step; zero additional stateful infrastructure, so one backup, one DR drill, one upgrade path; audit lives in our own hash-chained store under our own retention; activity code has no determinism constraints; scope is bounded to §11 so the engine stays small (low hundreds of lines of coordination logic plus SQL); the port makes adoption of Temporal a configuration change if the trade shifts | We own the correctness of leases, fencing, retry classification and compensation ordering — a real engineering and testing burden; our operator surface is `platformctl` and SQL, not a polished UI, and Temporal's UI is genuinely better; we will re-learn lessons Temporal already encoded; polling has a floor on step-dispatch latency (~200 ms), irrelevant here but real | **Accepted** |
| **Adopt Temporal outright (no own engine)** | Mature, battle-tested durable execution; excellent Web UI and `tctl` for operators — the single strongest argument; timers, child workflows, continue-as-new, signals and queries all first-class; large community and hiring pool; we would write far less coordination code | Activity effects commit to *our* database while workflow progress commits to *Temporal's*: two stores, two commits, a window between them. Every activity must be idempotent against its own partial completion, which we do anyway as defence in depth — but here it becomes **load-bearing**, and load-bearing idempotency that is only exercised in rare crash windows is exactly the kind of correctness we cannot test into existence. Plus: another stateful cluster (or a vendor in the activation path), determinism constraints on workflow code, no first-class compensation construct (sagas are hand-rolled `defer` stacks either way), and workflow history as a second audit-retention story. This is the option a distributed-systems engineer pushes for, and it is the right call for a team running many complex workflows — it loses here on transactional coupling and on the disproportion between Temporal's operational weight and a 12-step, low-volume workflow | Rejected as default; **retained as a maintained adapter** |
| **AWS Step Functions** | Fully managed, no cluster to run; visual workflow representation; native integration with Lambda and other AWS services; per-state-transition billing suits low volume | Workflow definition lives in ASL (JSON) outside our repository and outside our type system, so a step's contract cannot be checked by the compiler; 1-year maximum execution time is fine but the 25 000-event history limit is a real constraint for long-lived retrying workflows; the same dual-store problem as Temporal, plus a harder debugging story; compensation must be hand-built as an explicit path; vendor lock-in on a component we explicitly want portable (ADR-021 contemplates multi-region, and Step Functions state is regional); local testing is poor, which slows the workflow that is hardest to test | Rejected |
| **A naive job table with a cron worker** | Trivial to build; no new concepts; obviously fits in Postgres | It becomes a workflow engine anyway, one incident at a time — first leases, then retries, then compensation, then signals, then versioning — but designed reactively rather than deliberately, with each mechanism bolted on under time pressure. The compensation-in-reverse-order requirement alone is not something a job table grows correctly. This is the option that looks like the chosen one but without the design | Rejected |
| **Embed a third-party Go workflow library in-process** | Keeps the transaction; no external cluster | The libraries in this space either assume their own store (reintroducing dual-write) or are thin enough that we are writing the same code with an extra dependency and less control over the schema our audit depends on | Rejected |

## Consequences

### Positive

- Step completion, domain state change and outbox event are atomic. Onboarding cannot record
  "KYC submitted" without the merchant actually being in `KYC_PENDING`, and vice versa.
- One stateful system (Postgres) for the whole Automation plane: one backup, one restore drill,
  one failover story, one set of credentials.
- Workflow history is queryable with SQL alongside the domain data, which makes support
  investigations ("what happened to merchant X's onboarding?") a single join.
- Audit retention is uniform across the platform.
- The port genuinely de-risks the decision: if the workflow portfolio grows in complexity, we
  switch implementations without rewriting the onboarding definition.

### Negative

- We own durable-execution correctness. Leases, fencing, retry classification, compensation
  ordering and DLQ semantics are ours to test and ours to get wrong.
- The operator experience is materially worse than Temporal's UI. `platformctl` plus SQL plus
  Grafana is functional but not comparable, and this is felt most during an incident.
- Maintaining a second implementation (Temporal adapter) has a real ongoing cost: every port
  change must be implemented twice and tested twice.
- Polling introduces a step-dispatch latency floor. Irrelevant for a workflow with 5-day steps,
  but it means the engine is unsuitable for anything latency-sensitive — which is a constraint we
  must remember rather than discover.

### Neutral / accepted costs

- Our versioning model ("new version, new definition; in-flight instances finish on the old one")
  is simpler and less powerful than Temporal's `GetVersion` patching. It is sufficient for a
  workflow that changes a few times a year.
- We deliberately have no child workflows or continue-as-new. If we need them, that is the signal
  to switch, and the port makes that cheap.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| A lease bug causes two workers to run the same step | Low | High — duplicate KYC submission, duplicate provisioning | Lease acquisition is a single conditional `UPDATE` using the database's `now()`; `lease_epoch` fencing rejects a stale worker's write; every activity is independently idempotent (§11 marks each step's idempotency basis) | `TestConcurrentWorkersRunEachStepOnce`; duplicate external vendor references |
| Compensation runs out of order or partially | Medium | High — orphaned gateway sub-accounts, dangling webhooks | Compensations are a stack recorded per completed step and executed in strict reverse; each compensation is itself idempotent and retried; failures go to the DLQ with the chain, never silently skipped | `pp_workflow_step_duration_seconds{outcome="compensation_failed"}`; orphan-resource reconciliation job |
| Our engine accretes BPM features and becomes a product | Medium | Medium — maintenance burden, scope creep | §1.2 scope statement is binding; a feature request beyond §11 triggers a switch-to-Temporal evaluation rather than an engine change | Engine LOC and cyclomatic complexity tracked in CI; count of engine features not used by `merchant-onboarding@v1` |
| Temporal adapter rots and stops being a real option | High if unattended | Medium — the reversibility we claim becomes fictional | The onboarding definition's test suite runs against both implementations in CI; a quarterly drill runs a full onboarding on the Temporal adapter in staging | CI matrix status; drill outcome recorded in the ADR review |
| A stuck instance goes unnoticed | Medium | Medium — merchant never activates | `pp_workflow_instances{workflow,state}` gauge with alerting on instances exceeding their step timeout; `pp_dlq_depth` alerting | The gauges; onboarding duration histogram p95 against the ≤ 30 min target |
| Postgres becomes a bottleneck for workflow polling | Low | Low | `FOR UPDATE SKIP LOCKED` with an index on `(state, next_poll_at)`; volume is orders of magnitude below the payment path | Poll query latency; worker idle ratio |

## Validation

- **Crash-resumption test:** kill `workflow-worker` at every step boundary and mid-step; assert no
  completed step re-executes, no external vendor call is duplicated, and the instance completes.
- **Compensation test:** abort an instance after step 7 (`register-webhooks`); assert
  compensations run for steps 7, 6, 5 in that order, that each is idempotent under re-run, and
  that no external resource is orphaned.
- **Dual-implementation test:** the entire onboarding suite passes against both `engine/postgres`
  and `engine/temporal`. A test that passes on only one is a port-abstraction leak and a defect.
- **Business metric:** onboarding duration p95 ≤ 30 minutes for the automated portion (§18),
  excluding external KYC SLA and the manual gate.
- **Reliability metric:** zero onboarding cases lost or stuck without an alert, per quarter.
- **Engine size:** if the Postgres engine's coordination logic exceeds ~2 000 LOC, we are building
  a product we said we would not build; that is a review trigger.

## Revisit criteria

Switch to Temporal (a configuration change, per the port) if any of these hold:

1. We need more than three distinct workflow definitions, or any workflow needs child workflows,
   continue-as-new, or dynamic branching.
2. The engine's coordination code exceeds ~2 000 LOC or accumulates more than a handful of
   correctness defects in a year — at that point we are paying Temporal's cost without its
   maturity.
3. Operator burden during incidents becomes a recurring complaint that better tooling on our side
   cannot address.
4. A team materially larger than today's takes over, changing the maintain-vs-adopt calculus.

Conversely, reconsider *removing* the Temporal adapter if it has not been exercised beyond CI for
four consecutive quarters and the workflow portfolio has not grown — dead abstractions cost more
than they preserve.
