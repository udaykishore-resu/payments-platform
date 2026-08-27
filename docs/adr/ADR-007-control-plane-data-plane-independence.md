# ADR-007: Control plane and data plane are separately deployed and independently scaled

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §5 (deployable units), §15 (consistency model, ambiguity A5), §18 (non-functional targets) of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Refines ADR-006; depended on by ADR-019 (fail-static config), ADR-020 (event backbone)

## Context

ADR-006 established the plane as the primary architectural boundary. This ADR states the single
most load-bearing consequence: **the data plane must not have a synchronous runtime dependency on
the control plane.**

The forces:

1. **Availability arithmetic.** The data plane targets 99.99 % monthly (≤ 4 m 23 s), the control
   plane 99.9 % (≤ 43 m). A synchronous dependency makes the data plane's ceiling the *product*
   of the two: 0.9999 × 0.999 = 99.89 %, i.e. ≈ 47 minutes of monthly downtime — 10× worse than
   the target, and the target is a contractual number for tenants. There is no amount of
   retry logic that fixes this; retries convert unavailability into latency, and latency past
   the 250 ms p99 budget *is* unavailability under our SLI definition.
2. **The control plane is the riskier codebase.** It carries admin UI surface, bulk imports,
   configuration validation, long-running list queries, and human-triggered operations. It is
   where a `SELECT` without a `LIMIT` gets written. That risk must not be able to reach the
   money path.
3. **Load profiles are unrelated.** Control-plane request rate is on the order of tens of
   requests per second (500 tenants doing admin work). Data-plane request rate is 5 000 TPS
   sustained and 15 000 TPS peak — a 100–1000× difference. Sizing one fleet for both means
   either wasting money on control-plane capacity or brownouts on the money path.
4. **Configuration is read on every payment.** Stage 9 of the §12 pipeline loads merchant context
   with a 5 ms budget. A synchronous control-plane call cannot fit: even an in-cluster gRPC call
   with a warm connection is 1–3 ms p99, plus the control plane's own database query, plus tail
   amplification at 15 000 TPS — and it would put 15 000 QPS of read load on a fleet sized for 50.
5. **Deploy cadence.** Control-plane changes are frequent and low-risk. Money-path changes are
   infrequent and high-ceremony. Coupling them means either slowing down the former or cheapening
   the process for the latter.

What breaks if we choose wrong: the first control-plane incident becomes a payment outage. This
is not hypothetical — a synchronous config lookup on the hot path is the single most common way
a "highly available" payment system inherits the availability of its admin service.

## Decision

**The control plane and the data plane are separate deployables with separate scaling policies,
separate databases (or at minimum separate connection pools and separate Aurora endpoints), and
no synchronous runtime dependency in the payment direction.**

Precisely:

1. `control-plane-api` and the data-plane binaries (`payment-api`, `payment-orchestrator`,
   `webhook-ingress`, `outbox-relay`, `event-consumer`) are separate images, separate
   Deployments, separate HPAs, separate PodDisruptionBudgets and separate IAM roles.
2. **Direction of dependency is one-way at runtime.** The control plane publishes
   `configuration.published.v1`, `merchant.activated.v1`, `merchant.suspended.v1` etc. to Kafka
   (§13.2). The data plane consumes them into a local snapshot. The data plane never calls the
   control plane synchronously on the payment path.
3. **The data plane's config read is a local, in-memory snapshot lookup** with a bounded
   staleness of ≤ 30 s p99 (§18) and push invalidation via Kafka. Its age is exported as
   `pp_config_snapshot_age_seconds` (§22.2). Behaviour when the control plane is gone is defined
   by ADR-019 (fail-static with a 15-minute cliff), not by this ADR.
4. **Off-hot-path synchronous calls are permitted and expected**: `workflow-worker` (Automation
   plane) calls the control plane during onboarding step 8 (`apply-configuration`); `platformctl`
   calls it for operations. These are not on the money path and inherit the control plane's
   99.9 % without harm.
5. **Startup must not require the control plane.** A data-plane pod that cannot reach the control
   plane starts, loads its last-known-good snapshot from its own store, and reports `ready`. A
   cold pod with no snapshot at all reads the snapshot from Postgres (the data plane's own
   replicated copy), not from the control-plane API.
6. Autoscaling signals are per-plane: control plane on request rate and CPU; `payment-api` on
   concurrent connections; `payment-orchestrator` on in-flight gateway calls; `webhook-ingress`
   on queue depth; `outbox-relay` on `pp_outbox_backlog`; `event-consumer` on `pp_consumer_lag`.

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Separate deployables, async config propagation (chosen)** | Data-plane availability is independent of control-plane availability; config read is an in-memory lookup (~microseconds, comfortably inside the 5 ms stage budget); control plane can be deployed, scaled to zero at night, or broken during an incident without touching revenue; blast radius is contained by construction | Configuration is eventually consistent — a merchant suspension takes up to 30 s p99 to take effect, and we must design for that window; requires an invalidation path, a snapshot store, and staleness monitoring; "what config was in effect" becomes a question needing a versioned answer | **Accepted** |
| **Synchronous config read with an aggressive local cache** | Strong read-your-writes semantics for config; simplest mental model ("just call the API"); no event pipeline to build or operate; cache makes the common case fast | The cache is the availability story, and a cache that must never miss is not a cache — it is a snapshot with a bug. On a cold start, a cache stampede, or a key eviction, the data plane calls the control plane at money-path rates. A control-plane outage then correlates with exactly the moment the data plane needs it most (deploys, scale-ups, incidents all cause cold caches). This is the option a reasonable engineer pushes for — it is genuinely simpler, and the cache hit rate in steady state would be > 99.9 % — and it loses because the 0.1 % is perfectly correlated with the failure we are trying to survive | Rejected |
| **One deployable containing both planes, internally modular** | No propagation delay at all — config reads are a function call against the same database; no eventual consistency to reason about; one deploy, one dashboard; strictly simplest | Availability, scaling and deploy cadence all become shared: one number, one signal, one change process. A control-plane bulk query saturates the connection pool the payment path needs. Rejected in ADR-006 for the same reasons; restated here because the config-read argument is the concrete instance of it | Rejected |
| **Separate deployables, shared database (data plane reads config tables directly)** | No event pipeline; strong consistency on config reads; still gets deploy and scaling independence | Couples the two planes at the layer that actually fails — the database. A control-plane query pattern (unbounded admin list, a migration, a `VACUUM` storm) directly degrades payment reads. Shared tables also destroy the ownership boundary: the data plane would depend on the control plane's *physical schema*, so any control-plane migration becomes a coordinated cross-plane release. Independence in deployment without independence in the datastore is theatre | Rejected |

## Consequences

### Positive

- Data-plane availability is bounded only by its own dependencies (Aurora, Redis, Kafka,
  gateways), not by the control plane's.
- Config reads cost ~microseconds instead of ~2–5 ms, freeing budget in the §12 pipeline.
- The control plane can be scaled, restarted, migrated or rolled back during business hours.
- A control-plane deploy does not require a money-path change review.
- Capacity is right-sized: the control-plane fleet is sized for tens of RPS, not 15 000.

### Negative

- **Config changes are not immediate.** A merchant suspension takes up to 30 s (p99) to stop new
  payments. §13.2 marks `merchant.suspended.v1` as a **priority invalidation** for this reason,
  but the window is real and must be stated to tenants and to compliance.
- We must build and operate a snapshot store, an invalidation consumer, and staleness alerting —
  work that a synchronous read would not require.
- Debugging "which configuration version was in effect for payment X" requires the answer to be
  persisted with the payment; it is not derivable from the current control-plane state.
- Two databases (or two endpoints) means two backup/restore stories and two migration pipelines.

### Neutral / accepted costs

- The Automation plane *does* depend synchronously on the control plane. This is accepted: both
  target 99.9 %, and an onboarding step that fails is retried by the workflow engine (§11).
- The one-way rule means the control plane learns about data-plane state (gateway health,
  reconciliation exceptions) only through events, which is slower but also the correct coupling.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| A synchronous control-plane call is added to the hot path | Medium | **Critical** — silently re-couples availability | Architecture check forbids importing a control-plane client from hot-path packages; `TestDataPlaneServesWithControlPlaneDown` in `tests/integration` fails the build if it regresses | A control-plane client span under a `POST /v1/payments` trace; control-plane RPS correlating with payment TPS |
| Snapshot staleness exceeds the bound without anyone noticing | Medium | High — compliance limits enforced against stale policy | `pp_config_snapshot_age_seconds` alerts at 5 min (§15); hard cliff at `max_config_staleness` = 15 min per ADR-019 | The gauge itself; synthetic config-propagation probe (§18, ≤ 30 s p99) |
| A suspended merchant processes a payment inside the propagation window | Medium | Medium — a small number of payments for a merchant we intended to stop | Priority invalidation path for `merchant.suspended.v1`; suspension also writes a data-plane-visible row so the next payment for that merchant fails at stage 9 even if the event is late; refunds/voids remain permitted by design (§8) | Count of payments accepted for a merchant whose suspension event timestamp precedes the payment |
| Shared Aurora cluster reintroduced for cost reasons | Medium | High | Separate cluster (or at minimum separate writer endpoint + separate connection pools + separate roles) is a terraform-enforced invariant; a shared-cluster change requires reopening this ADR | Terraform plan diff; connection-pool metrics showing control-plane query patterns on the data-plane endpoint |
| Cold-start pod cannot obtain a snapshot and fails ready | Low | High during a scale-up event | Snapshot is persisted in the data plane's own Postgres and in the pod's ephemeral volume; readiness requires *a* snapshot, not a *fresh* one | Readiness failure rate during HPA scale-out; `pp_config_snapshot_age_seconds` at pod start |

## Validation

- **Chaos test:** `tests/chaos/control_plane_loss_test.go` scales `control-plane-api` to zero for
  20 minutes under sustained payment load. Assertions: payment success rate stays within SLO for
  the first 15 minutes; the fail-closed cliff for *new* merchants engages at 15 minutes exactly;
  existing merchants continue throughout.
- **Dependency assertion:** the trace of a `POST /v1/payments` request must contain zero spans
  whose `service` attribute is `control-plane-api`. Asserted in CI against a recorded trace from
  the e2e suite.
- **Propagation SLI:** synthetic probe publishes a config version and measures time to
  data-plane effect. SLO p99 ≤ 30 s (§18); alert at > 5 min (§22.4).
- **Scaling independence:** control-plane replica count and data-plane replica count must show
  no meaningful correlation over a 90-day window. Correlation implies a hidden coupling.

## Revisit criteria

Reopen if:

1. The 30 s propagation window is shown to cause a material compliance or financial exposure
   (e.g. a regulator requires sub-second suspension enforcement) — the fix would be a synchronous
   *revocation* check on a separate, tiny, 99.99 % service, not a general re-coupling.
2. Control-plane availability is measured at ≥ 99.99 % for four consecutive quarters *and* its
   change risk profile has materially changed — even then, the deploy-cadence argument stands
   on its own.
3. We adopt a configuration substrate with genuinely different characteristics (e.g. a
   globally-replicated strongly-consistent store with < 1 ms reads and independent failure
   domains), which would change the trade rather than remove it.
