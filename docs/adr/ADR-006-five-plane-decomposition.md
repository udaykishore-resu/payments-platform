# ADR-006: Five-plane decomposition as the primary architectural boundary

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §2 (definition of *Plane*), §3 (bounded contexts and context map), §5 (deployable units) of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Related to ADR-001 (Go), ADR-005 (Kubernetes); refined by ADR-007, ADR-024

## Context

We are building two products on one codebase: merchant onboarding (low volume, long-running,
human-gated, tolerant of minutes of latency) and payment orchestration (5 000 TPS sustained,
15 000 TPS peak per region, p99 ≤ 250 ms excluding gateway time, 99.99 % availability). Baseline
§3 identifies nine bounded contexts and §5 identifies nine deployable binaries. The open question
is what the *primary* decomposition axis is — the one that determines deployment units,
availability targets, on-call boundaries and blast radius.

The forces:

1. **Availability targets differ by an order of magnitude.** Data plane 99.99 % (≤ 4 m 23 s of
   monthly downtime), control plane 99.9 % (≤ 43 m). If they share a deployment unit, the whole
   thing is bound to the stricter target and every admin-console change becomes a money-path
   change-management event. That is a ~10× increase in change friction for the 80 % of commits
   that touch no money.
2. **Scaling drivers differ in kind, not degree.** Payment ingress scales on concurrent
   connections; the orchestrator scales on in-flight gateway calls (bounded by an 8 s hard
   timeout, so ~40 000 in-flight goroutines at peak); webhook ingress scales on gateway-driven
   spikes we do not control; the workflow worker scales on retry backlog. A single autoscaling
   signal cannot serve four different queueing systems.
3. **Blast radius must be asymmetric.** A bug in configuration publishing must not be able to
   stop payments. Baseline §15 makes this explicit: the data plane serves last-known-good config
   when the control plane is entirely gone.
4. **Nine contexts is not nine services.** Splitting one service per aggregate would put a
   network hop between `Payment` and `PaymentAttempt` — two aggregates that must be written in
   *one* transaction to satisfy invariant I3 (§9). That is not a scaling boundary; it is a
   correctness hazard sold as modularity.
5. **Team topology.** At the scale this platform targets (50 000 merchants, 500 tenants) the
   engineering organisation is small. Twenty-plus services means twenty-plus deployment
   pipelines, dashboards and on-call runbooks maintained by people who also have to ship
   features.

What breaks if we choose wrong: too fine a split and every payment costs 3–5 extra network hops
(each ~1–3 ms p99 in-cluster, plus tail amplification), distributed transactions appear where
none are needed, and the 250 ms p99 budget is consumed by our own RPC overhead. Too coarse a
split and a slow gateway consumes the ingress connection pool, admin traffic competes with money
traffic for CPU, and a control-plane deploy is a money-path deploy.

## Decision

**The plane is the primary architectural boundary. Bounded contexts are the primary *code*
boundary. They are deliberately different axes and neither is derived from the other.**

Five planes, each with its own availability target, scaling behaviour and blast radius:

| Plane | Contexts | Binaries | Availability | Scales on |
|---|---|---|---|---|
| Control | BC-1, BC-2, BC-4 (registry), BC-5 | `control-plane-api` | 99.9 % | admin request rate |
| Automation | BC-3 | `workflow-worker` | 99.9 % | onboarding volume + retry backlog |
| Data | BC-4 (integration), BC-6, BC-7, BC-8 | `payment-api`, `payment-orchestrator`, `webhook-ingress`, `outbox-relay`, `event-consumer` | 99.99 % | payment TPS, webhook volume, outbox backlog, Kafka lag |
| Validation | cross-cutting (`internal/validation`, L1–L7) | in-process library in every plane | inherits host | — |
| Observability | BC-9 | `event-consumer` (audit sink) | 99.9 % | event volume |

Binding consequences, each mechanically checkable:

1. A deployable binary belongs to exactly one plane and is declared in `deployments/k8s` with a
   `plane` label. CI rejects a manifest without one.
2. Cross-plane calls on the payment hot path are **asynchronous only** (Kafka, or a locally
   cached snapshot). Synchronous cross-plane calls are permitted only off the hot path
   (e.g. the workflow worker calling the control plane during onboarding).
3. Contexts within a plane may share a database schema and be composed into one binary. Contexts
   in different planes may not share a transaction.
4. The Validation plane is a plane by *contract* (§21: stable rule IDs, purity rules, documented
   remediation) and a library by *deployment*. It has no binary of its own because a network hop
   for a pure function is indefensible on a 3 ms budget (see ADR-016).

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Five-plane decomposition (chosen)** | Boundaries follow availability, scaling and blast-radius differences that actually exist; nine binaries, not twenty-five; contexts stay as code modules so refactoring a boundary is a package move, not a migration; payment write path stays in one process and one transaction | Requires discipline to stop a plane becoming a monolith internally; "which plane does this belong to" is a real design question for genuinely cross-cutting features; a plane can still be too big if we are careless | **Accepted** |
| **Microservice per aggregate (9+ services)** | Textbook DDD alignment; each context independently deployable and scalable; clear ownership; failure of one aggregate's service is contained; independent technology choices per service | Payment and PaymentAttempt must be written in one transaction for invariant I3 (§9) — splitting them forces a saga for a *single user action*, which is how double-charging bugs get born; adds 3–5 in-cluster hops per payment against a 250 ms p99 budget where our own pipeline (§12) already consumes ~75 ms; multiplies operational surface (9 pipelines → 9 sets of dashboards, SLOs, runbooks, dependency upgrades) with no corresponding availability gain — availability of a serial chain is the *product* of its members, so nine 99.99 % services in series is 99.91 %; every context boundary becomes a versioned wire contract that cannot be refactored in an afternoon. This is the option a reasonable engineer pushes for, and it loses on transactional integrity first and availability arithmetic second | Rejected |
| **Modular monolith (1 binary, enforced module boundaries)** | Simplest deployment; no distributed-systems tax at all; in-process calls are free; one database, one transaction, trivially correct; refactoring boundaries is cheap | Cannot give the data plane 99.99 % and the control plane 99.9 % — one binary has one availability number and one deploy cadence; a slow gateway adapter consumes the same goroutine and connection pools as admin traffic (no bulkhead); autoscaling has one signal for four workloads with different queueing behaviour; a memory leak in webhook parsing takes down payments. It also does not survive the org: one binary means one deploy queue and change-freeze windows that block everyone | Rejected |
| **Two services (control + data), contexts inside** | Captures the single most important boundary cheaply; half the operational surface of the chosen option | Loses the bulkhead between `payment-api` and `payment-orchestrator` that §5 requires (a slow gateway would consume the ingress pool); merges the spiky, untrusted webhook ingress with the money path; puts the outbox relay's throughput characteristics in the same process as request handling | Rejected — this is the chosen option minus the splits we have concrete reasons for |

## Consequences

### Positive

- Availability targets are set per plane and defended per plane. A control-plane incident consumes
  the control-plane error budget only.
- The payment write path (§12 stages 8–17) executes in **one process and one database
  transaction**, which is what makes invariants I1–I5 enforceable by the database rather than by
  a saga.
- Deploy cadence decouples: control-plane changes ship on the normal path; data-plane changes go
  through the stricter money-path change process. Empirically this is the majority of commits.
- Bulkheads are deployment-level, not just code-level: a gateway that hangs consumes
  `payment-orchestrator` capacity, not `payment-api` connections.

### Negative

- Five planes × nine binaries is more operational surface than a monolith: nine images, nine
  Helm subcharts, nine sets of SLO dashboards, nine readiness contracts.
- A feature that genuinely spans planes (e.g. "suspend merchant, immediately stop payments")
  requires an event path and an invalidation contract rather than a function call, and the
  latency of that path (config propagation ≤ 30 s p99, §18) is now a product-visible number.
- "Which plane owns this?" becomes a recurring design argument. We accept it as the cost of
  having the boundary at all.

### Neutral / accepted costs

- Contexts and planes are different axes, so the mental model has two dimensions. New engineers
  need the context map in §3 explained before they can place a change.
- BC-4 (Gateway) deliberately spans two planes — registry in Control, integration in Data. This
  is a real seam, not an accident, and it is documented as such.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| A plane becomes an internal monolith with tangled contexts | High | Medium — slows change, does not break production | `scripts/check-architecture.sh` enforces §4 import rules; context packages may not import each other's internals, only their published interfaces | Import-graph cycle count in CI; PRs that touch > 3 context packages |
| Someone adds a synchronous control-plane call on the payment hot path | Medium | **High** — couples 99.99 % to 99.9 %, breaks A5 | Hot-path packages may not import a control-plane client; enforced by the architecture check. Integration test `TestDataPlaneServesWithControlPlaneDown` kills the control plane and asserts payments continue | `pp_config_snapshot_age_seconds` rising; any control-plane client span appearing under a `POST /v1/payments` trace |
| Plane boundaries drawn correctly but binaries sized wrongly (e.g. orchestrator too coarse) | Medium | Medium | Per-binary saturation SLIs; §5 explicitly allows re-splitting a binary without moving a context | CPU/goroutine saturation diverging between co-located workloads |
| Cross-plane event contract drift breaks the data plane | Medium | High | Published Language (§13): versioned envelope, additive-only within a major, `.v2` published alongside `.v1`; consumer contract tests in `tests/contract` | Schema-registry incompatibility failures; consumer deserialization error rate |
| Distributed monolith by the back door — planes deploy in lockstep | Medium | Medium | Deploy independence is asserted: each plane's CI runs the other planes' contract tests against the *released* version, not `HEAD` | Correlation between deploy timestamps across planes |

## Validation

- **Deploy independence:** over any rolling 90-day window, ≥ 80 % of control-plane deploys must
  ship without an accompanying data-plane deploy. Measured from the release pipeline. If planes
  always deploy together, the boundary is decorative and this ADR is wrong.
- **Blast-radius isolation:** **not tested end to end.** The property — terminating the control
  plane entirely and asserts payment success rate stays within SLO for the full
  `max_config_staleness` window (15 min, §15).
- **Latency budget:** the §12 pipeline stages sum to ≤ 75 ms of platform time at p99, measured
  from server-side histograms. Cross-plane hops appearing in that budget is a defect.
- **Availability arithmetic:** data-plane availability SLI ≥ 99.99 % monthly *while*
  control-plane availability is allowed to sit at 99.9 %. If data-plane availability tracks
  control-plane availability, the decoupling is not real.

## Revisit criteria

Reopen this ADR if any of the following hold:

1. A single plane exceeds ~15 engineers or three teams — at that point plane-internal ownership
   ambiguity costs more than the extra services would.
2. One context inside the data plane needs a scaling profile that is more than ~5× different
   from its co-tenants (e.g. ledger volume grows to dominate orchestrator resource use),
   justifying its own binary — note this is a *binary* split, not a plane split, and does not
   require reopening the plane model.
3. A regulatory requirement demands physical separation of a context (e.g. a residency regime
   requiring the ledger in a jurisdiction the rest of the plane cannot enter).
4. Measured in-cluster RPC p99 falls below ~0.5 ms and transactional integrity across services
   becomes cheap (a genuine platform shift, not an incremental improvement) — the transactional
   argument against per-aggregate services is the load-bearing one and would need to fall first.
