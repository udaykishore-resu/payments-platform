# ADR-015: Routing is a scored decision over capability descriptors and live health, persisted as an auditable plan

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §2 (Routing Plan), §10 (gateway health FSM), §12 stage 12, §23 (configuration document, `routing.weights`) of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Depends on ADR-011 (capability descriptors), ADR-012 (attempts); related to ADR-019 (fail-static)

## Context

For each payment the platform must choose which gateway to attempt first, and which to fall back
to. That choice determines the authorization rate (the merchant's revenue), the processing cost
(the merchant's margin), and the blast radius of a gateway incident.

The forces:

1. **Static configuration cannot react to reality.** A merchant configured `primary: stripe` keeps
   sending traffic to Stripe while Stripe is returning 40 % errors. §10 gives us a health FSM
   precisely so that routing can react within ~30 s; ignoring it wastes the mechanism.
2. **Multiple factors genuinely trade off.** Health, success rate, cost and latency are all real
   and sometimes disagree. The worked example in `docs/data-plane.md` is instructive: Adyen has
   the best success rate *and* the lowest cost and still loses, because being `DEGRADED` costs it
   0.24 of a possible 0.40 — cheap failures are not cheap.
3. **Some constraints are not tradeable.** A gateway that does not support the currency, does not
   support the payment method, is not `CERTIFIED` for this merchant, or would violate the tenant's
   data-residency policy (§17.3) must be **excluded**, not down-weighted. No score may resurrect
   an ineligible candidate.
4. **Every routing decision is subject to later dispute.** A merchant will ask why their payment
   went to the expensive gateway. A postmortem will ask why traffic did not shift. An auditor will
   ask whether residency was respected. Answering requires the decision, its inputs, and the
   config and descriptor versions in effect — not a reconstruction from current state, which has
   changed.
5. **Latency budget is 5 ms** (§12 stage 12). Scoring a handful of candidates over locally
   maintained windows fits. A network call to a model server does not.
6. **Flapping is a real failure mode.** A scoring function that reorders candidates on noise
   produces traffic oscillation, which degrades issuer approval rates and makes gateway-side
   anomaly detection fire.

What breaks if we choose wrong: revenue lost to a degraded gateway we kept using; or traffic
oscillating between gateways on statistical noise; or an unexplainable decision in front of a
merchant, a regulator, or an incident review.

## Decision

**Routing is a two-phase decision — hard filters, then a weighted score over normalized factors —
producing an ordered, reason-annotated `RoutingPlan` that is persisted with the payment.**

1. **Phase 1: hard filters.** Applied in a fixed order; each removal is recorded on the plan with
   a reason code. Filters include: certification status for `(merchant, gateway, method,
   currency)`; capability descriptor support for currency, method, country and operation;
   gateway connection state; circuit-breaker state (`UNHEALTHY` = open circuit is a filter, not a
   penalty); tenant residency policy. **A hard filter is never traded off against score.**
2. **Phase 2: weighted score** on `[0, 1]`, higher is better:
   `score(g) = w_health·H(g) + w_success·S(g) + w_cost·C(g) + w_latency·L(g)`
   - Weights come from `config.routing.weights` (§23), validated by
     `L4.ROUTING_WEIGHTS_SUM_TO_ONE`. Defaults: health 0.4, success 0.3, cost 0.2, latency 0.1.
   - `S(g)` uses **Bayesian smoothing** — `ŝ = (successes + α·prior)/(n + α)` with `α = 50` and
     `prior` = the merchant's 30-day authorization baseline — so a gateway with 6/6 successes does
     not outrank one with 4 000 samples at 94 %. Normalized against a **fixed band**
     (`clamp((ŝ − 0.85)/(0.98 − 0.85), 0, 1)`) rather than min-max across candidates, because
     min-max amplifies noise into flapping when candidates are close.
   - `L(g) = 1 − clamp(p95_authorize_ms / 3000, 0, 1)` and is weighted lowest deliberately: inside
     an 8 s budget, a slower gateway that approves is worth more than a fast one that declines.
3. **Ties are explicit.** Scores within **0.02** are treated as tied (below that the difference is
   inside the noise of a 30-minute success window) and broken deterministically, so the same
   inputs always produce the same plan.
4. **Affinity is a bonus, never a rule.** A `+0.05` bonus for 30 minutes to the gateway that most
   recently approved for this `(merchant, card fingerprint)` improves issuer approval rates by
   keeping traffic on a familiar acquirer BIN — expressed as a bonus so it can never override
   health.
5. **The plan is persisted** (`routing_plans`, `rpl_` prefix) with: the ordered candidates and
   their scores and ranks, **the raw factor inputs**, the weights used, every exclusion with its
   reason, and the configuration version and descriptor versions in effect.
6. **Decisions are recomputable.** `platformctl routing explain rpl_…` re-runs the scoring
   function offline against the stored inputs and asserts it reproduces the stored score. A
   mismatch is a defect in either the code or the record — this is what makes "auditable" a
   testable property rather than a claim.
7. **Empty candidate set → `503 NO_ELIGIBLE_GATEWAY`** with `Retry-After`. We fail closed (§24).

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Scored decision over descriptors + live health, persisted plan (chosen)** | Reacts to gateway degradation within the health FSM's ~30 s window; makes the trade-off between health, success, cost and latency explicit and tunable per merchant; hard filters keep non-negotiable constraints non-negotiable; the persisted plan answers "why this gateway?" months later with the inputs, not a reconstruction; deterministic and offline-recomputable, so it is testable and explainable | A scoring function is a tuning surface, and tuning surfaces get tuned badly; four factors with weights invite bikeshedding; the plan is an extra row per payment (~5 000 rows/s at peak); merchants may find "it depends on a score" less predictable than "it goes to Stripe" | **Accepted** |
| **Static config-only routing (`primary` + ordered `fallback`)** | Completely predictable; a merchant can state exactly where their traffic goes; trivial to implement, test and explain; no tuning surface, no scoring bugs; no extra row per payment | Cannot react to health at all — traffic keeps going to a gateway returning errors until a human edits configuration, which during an incident is minutes of lost revenue at best; cost and success-rate optimisation are impossible; the failover order is fixed even when the fallback is the one that is degraded. It also does not remove the need for hard filters (currency/method/certification), so the "simple" option still needs half the machinery. This is the option a merchant-facing PM pushes for on predictability grounds — it loses because the health feedback loop in §10 exists and unused feedback is just cost | Rejected — retained *inside* the model as `PRIORITY_WITH_FALLBACK` strategy plus rules (§23), which the scorer honours as a strong prior |
| **ML-optimised router (a model predicting authorization probability)** | Potentially the highest authorization rate; learns issuer-, BIN- and time-of-day-specific patterns a hand-tuned function cannot; industry-proven for large processors | Cannot explain a decision to a merchant, an auditor, or a postmortem in terms anyone can act on; requires a training pipeline, feature store, model registry and drift monitoring — a substantial platform we do not have; a model call inside a 5 ms budget means an in-process model with a stale feature set, and a network call blows the budget; training data at our scale is thin, and thin data plus a feedback loop (the router determines its own training distribution) produces confident nonsense; a regression is invisible until authorization rates drop. The `RiskScorer` port precedent (§6.5) shows the right shape: an external model informs a *policy*, it does not become the policy | Rejected now; the factor design deliberately leaves room for a learned `S(g)` estimator later |
| **Round-robin / weighted random across eligible gateways** | Trivially fair; naturally spreads load; no scoring bugs; good for negotiating volume commitments across acquirers | Ignores health and success rate, so it deliberately routes a share of traffic to a degraded gateway; makes authorization rate a function of luck; makes per-payment behaviour non-deterministic and therefore unexplainable | Rejected — weighted distribution remains available as a *tie-break* and for deliberate volume-commitment splitting |
| **Cost-first routing (cheapest eligible gateway wins)** | Directly optimises the number the CFO looks at; simple to explain | A 0.3 % cheaper gateway with a 3 pp lower authorization rate loses far more revenue than it saves — a declined sale costs 100 % of the margin, a fee costs a fraction of a percent. Cost is correctly weighted at 0.2, not 1.0 | Rejected |

## Consequences

### Positive

- Traffic shifts away from a degrading gateway automatically, within the §10 detection window,
  with no human in the loop.
- Merchants get per-merchant control (weights, rules, pinning) without the platform losing its
  safety filters.
- Every decision is reproducible from stored inputs, which turns routing disputes and postmortems
  into a query rather than an argument.
- Hard filters mean residency and certification violations are structurally impossible in a plan,
  not merely improbable.
- The stored factor inputs are also the dataset that would train any future learned estimator.

### Negative

- One `routing_plans` row per payment: at peak, ~5 000 rows/s and meaningful storage over the
  7-year retention. Plans must be partitioned and the raw-input payload kept compact.
- The scoring function is a tuning surface. Weight changes are configuration, so they can be made
  quickly — including quickly and wrongly. `L4` validation catches malformed weights, not unwise
  ones.
- Explaining routing to a merchant requires tooling (`routing explain`) rather than a sentence.
- Four factors' inputs must be maintained: health windows, success EWMAs, cost tables, latency
  windows. A stale cost table silently misprices decisions.

### Neutral / accepted costs

- The 0.02 tie band and the `α = 50` smoothing constant are judgement calls. They are documented,
  versioned with the config, and revisited against flapping data rather than defended in the
  abstract.
- Routing behaviour differs slightly between regions because health and latency windows are local.
  This is correct — a gateway can be degraded in one region only — but it means "why did EU route
  differently from US?" is a legitimate and frequent question.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| Score flapping causes traffic oscillation | Medium | Medium — issuer approval degradation, gateway-side anomaly alerts | Fixed-band normalization (not min-max); Bayesian smoothing with `α = 50`; 30-min EWMA windows; the 0.02 tie band with deterministic tie-breaks; 30-min affinity bonus | `pp_routing_decisions_total{gateway}` share changing by > 20 pp within 10 minutes with no health event |
| Stale cost table misprices routing | Medium | Medium — margin leakage | Cost is a descriptor field with a freshness stamp; a cost input older than 100 days makes `C(g)` fall back to the merchant's configured default rather than a stale number | Descriptor freshness gauge; cost-factor fallback counter |
| Weights misconfigured (e.g. cost 0.9) | Medium | High — authorization rate drop | `L4.ROUTING_WEIGHTS_SUM_TO_ONE` plus per-factor bounds; configuration changes are versioned, audited and rollback-able (§23); authorization-rate SLI alerts on a 30-min drop (§22.4) | Authorization success rate SLI; config diff in audit |
| A hard filter is implemented as a penalty | Low | **Critical** — residency or certification violation | Filters and scores are separate code paths with separate types; a candidate removed in phase 1 is not present in phase 2's input at all; test asserts a filtered candidate can never appear in a plan regardless of score | `TestFilteredCandidateNeverRouted`; residency audit |
| Plan storage becomes a cost or performance problem | Medium | Medium | Partitioned by month with archival to S3 alongside payments; factor inputs stored as a compact typed payload, not free-form JSON | Table size; write p99 at stage 12 |
| `NO_ELIGIBLE_GATEWAY` from over-aggressive filtering | Medium | High — payments rejected | Exclusion reasons are recorded per candidate, so the cause is immediately visible; alerting on the error code; the health FSM's `PROBING` state ensures a recovering gateway returns to eligibility | `NO_ELIGIBLE_GATEWAY` rate by merchant and reason |
| Scoring code and stored plan diverge (record is not reproducible) | Low | Medium — auditability claim is false | `platformctl routing explain` recomputes and asserts equality; run as a sampled production job, not only in tests | Recomputation mismatch counter |

## Validation

- **Reproducibility:** a sampled job re-runs `routing explain` against 0.1 % of persisted plans
  daily and asserts the recomputed score equals the stored score. Target: zero mismatches.
- **Reaction time:** in a chaos test, degrade a gateway to a 30 % error rate and assert that
  ≥ 90 % of traffic has shifted away within 60 seconds (health FSM detection ~30 s plus routing
  propagation).
- **Authorization rate:** the routing engine's justification is revenue. A/B the scored router
  against static priority routing on matched merchant cohorts; the scored router must show a
  non-negative authorization-rate delta and a positive one during gateway incidents.
- **Stability:** measure gateway traffic-share variance in steady state (no health events).
  Oscillation beyond a few percentage points over 10-minute windows indicates flapping and
  triggers a tuning review.
- **Filter integrity:** zero payments routed to a gateway that a hard filter should have excluded,
  ever. Audited against certification and residency records.

## Revisit criteria

Reopen if:

1. Flapping persists after tuning the smoothing constants and tie band — the scoring model, not
   the parameters, would then be wrong.
2. We have ≥ 12 months of clean routing outcome data and ≥ 5 gateways, at which point a learned
   estimator for `S(g)` becomes worth evaluating *as a factor input*, with the explainability
   contract (persisted inputs, recomputable decision) preserved.
3. A merchant tier requires contractually fixed routing that the scoring model cannot express as
   a rule or pin — pinning already exists per §10, so this would have to be something stronger.
4. Routing plan storage cost exceeds a material share of the data-plane database spend.
