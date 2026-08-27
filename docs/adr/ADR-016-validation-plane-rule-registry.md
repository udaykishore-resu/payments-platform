# ADR-016: Validation as a first-class plane with a stable rule registry

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §21 (validation plane contract, L1–L7), §12 (pipeline stages 7, 10, 15, 16), §20 (error model) of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Related to ADR-006 (planes), ADR-017 (PAN detector is an L1 rule), ADR-019 (config is an L5 input)

## Context

Validation in a payments platform is not "check the JSON". It spans schema shape, merchant
eligibility, gateway reachability, configuration coherence, per-payment limits, gateway response
integrity and domain state transitions — seven distinct concerns that run at different times, in
different processes, with different purity properties, and that fail with different HTTP codes.

The forces:

1. **Scattered validation is unauditable.** A compliance question — "prove that this merchant's
   transactions were checked against their configured limit" — cannot be answered by pointing at
   an `if` statement somewhere in a handler. It needs a rule with an identity, a documented
   meaning, and a recorded outcome.
2. **Failures must be traceable to a rule, not a message.** `422 VALIDATION_FAILED: amount too
   large` tells a client nothing actionable. `L5.AMOUNT_WITHIN_MERCHANT_LIMIT` is a stable
   identifier that maps to documentation, to remediation, and to a support runbook — and it
   survives message rewording and translation.
3. **Purity is a hard-path requirement.** §12 gives L1 3 ms, L5 5 ms and L6 3 ms. A rule that
   makes a network call cannot live on the payment path. But some validation genuinely needs the
   network (L3, gateway reachability), so the model must distinguish rather than forbid.
4. **Order is load-bearing.** PAN detection (L1) must run before anything logs the request.
   Idempotency (stage 8) must run before merchant load (stage 9). L6 must run before the state
   transition (stage 16), because accepting a gateway response that says a different amount was
   captured is worse than rejecting it.
5. **Rules change more often than code structure.** Limits, prohibited-country lists and
   3DS thresholds change per merchant and per regulation. The mechanism must accommodate frequent
   *data* change without frequent *code* change — but without becoming a programming language.
6. **The temptation is a DSL.** Every rules engine begins as a small config format and ends as an
   untyped, untested, unversioned interpreter running on the money path.

What breaks if we choose wrong: validation that cannot be proven to a regulator; error messages
clients cannot act on; or a scripting engine that lets a configuration change take down payments.

## Decision

**Validation is a plane: one engine, one `Rule[T]` contract, seven numbered levels, and a
registry of stable rule IDs. Rules are Go code, pure and total wherever possible. There is no
scripting language and no runtime-evaluated expression syntax on the payment path.**

1. **The contract** (§21):
   ```go
   type Rule[T any] interface {
       ID() RuleID           // "L5.AMOUNT_WITHIN_MERCHANT_LIMIT" — stable, documented
       Severity() Severity   // ERROR | WARNING
       Evaluate(ctx context.Context, subject T) Outcome
   }
   ```
   `ID()` is part of the public contract: once published it is never reused for a different
   meaning, and a rule's removal is a documented deprecation, not a deletion.
2. **Seven levels** with fixed semantics (§21): L1 API/schema at the edge (pure); L2 merchant
   (mostly pure, vendor calls marked impure); L3 gateway (impure, network — **never on the payment
   hot path**); L4 configuration (pure, control-plane write path); L5 payment (pure, config is an
   *input* not a lookup); L6 response (pure, post-gateway); L7 domain/state (pure, in the
   aggregate).
3. **Purity is enforced by placement, not by convention.** L1, L4, L5, L6, L7 rule packages may
   not import anything that performs I/O — checked by the §4 architecture rules. L3 is explicitly
   impure and lives in a package the payment path cannot import.
4. **Deployment is in-process, contract is plane-level.** The validation plane has no binary of
   its own: a 3 ms budget cannot absorb a network hop for a pure function. It is a plane by
   *contract* — stable IDs, purity rules, documented remediation, uniform error mapping — and a
   library by deployment (ADR-006).
5. **Every rule ID is documented.** `docs/validation-plane.md` carries meaning and remediation for
   every ID, and `TestEveryRuleIsDocumented` fails the build if one is missing. Documentation is a
   build dependency, not an aspiration.
6. **Failures map to the error model deterministically** (§20): L1 → `400 VALIDATION_FAILED`;
   L2/L4/L5 → `422` with the rule ID in `details[].code`; L6 → `502
   GATEWAY_CONTRACT_VIOLATION`; L7 → `409 INVALID_STATE_TRANSITION`.
7. **Merchant-specific *values* are configuration; merchant-specific *logic* is a rule.** A limit
   threshold is data in the configuration document (§23). "Compare amount to the limit" is code.
   The boundary is deliberate and is what keeps a DSL from being necessary.

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Validation plane with a typed rule registry in Go (chosen)** | Every failure names a stable, documented rule, so clients and support get actionable errors and auditors get evidence; purity is enforceable by the import graph, which keeps the hot path fast and deterministic; rules are unit-testable in isolation with table-driven tests; the compiler catches subject-type mismatches; documentation completeness is a build gate | Adding a rule requires a code change and a deploy — slower than editing config; the seven-level taxonomy has genuine grey areas (is a merchant-state check L2 or L5?) that require judgement; a registry is one more thing that can go stale | **Accepted** |
| **Scattered validation (checks inline in handlers and aggregates)** | Zero framework; the check sits where the data is, which is the most readable form locally; no registry to maintain; fastest to write | No stable identifiers, so errors are prose and cannot be mapped to remediation or counted per rule; impossible to answer "which checks ran for this payment?"; the same check gets written twice with subtly different semantics in two handlers; purity cannot be enforced, so a network call ends up on the hot path eventually; compliance evidence becomes a code-reading exercise. This is the default that happens if we decide nothing, and it is why the plane exists | Rejected |
| **Rules DSL / embedded scripting (CEL, Rego, Lua, a bespoke expression language)** | Rules become data: change a rule without a deploy; non-engineers could in principle author them; strong fit for policy-shaped decisions and genuinely excellent for authorization policy | On the money path this trades compile-time safety for runtime failure: a type error, a nil dereference or an unbounded expression becomes a production incident triggered by a *configuration change*, which is the change class with the least review. Evaluation cost is unpredictable against a 5 ms budget. Testing a DSL rule requires a harness the DSL does not provide. Versioning, review and rollback of DSL rules recreate the entire code-change process with worse tooling. And the promised benefit is mostly illusory here: what actually changes frequently is *thresholds*, which are already configuration. This is the option a platform engineer pushes for on agility grounds, and it is the right answer for authorization policy (where we do use ordered policy evaluation, §6) — it loses for transaction validation because the failure mode is a payment outage caused by a config edit | Rejected |
| **Validation as a separate service (its own binary, called over gRPC)** | True plane isolation; independently deployable and scalable; a single logical place to audit; other systems could call it | A network hop for a pure function, inside a 3–5 ms budget, at 15 000 TPS peak; it would need the merchant configuration too, duplicating the snapshot problem; it introduces a synchronous dependency on the payment path for logic that has no state. All cost, no benefit | Rejected |
| **JSON Schema for everything (including business rules)** | Declarative, standard, tooling exists; already used for L1 and for event schemas | Expresses shape, not relationships: "refund amount ≤ captured amount minus prior refunds" is not a schema constraint; cross-field, stateful and configuration-dependent rules fall outside the model entirely | Rejected as a general mechanism; **retained for L1**, where shape is exactly the concern |

## Consequences

### Positive

- Every validation failure is traceable to a documented rule ID, which makes client errors
  actionable, support faster, and compliance evidence mechanical.
- Purity is enforced by the import graph, so the hot path stays deterministic and fast, and rules
  are trivially unit-testable.
- Rule outcomes are countable: `pp_payments_total{outcome="risk_declined"}` and per-rule counters
  make it visible when a rule starts rejecting more than it should.
- The seven levels give a shared vocabulary — "that's an L6 problem" locates a bug in one
  sentence.
- Documentation cannot rot silently: the build fails.

### Negative

- Rule changes require a deploy. For genuinely dynamic policy this is friction, and we accept it
  by keeping *values* in configuration.
- The seven-level taxonomy has grey areas and will produce recurring placement debates.
- The registry is a global surface: a rule added carelessly runs for every tenant.
- Generic `Rule[T]` with per-level subject types means some duplication in wiring code.

### Neutral / accepted costs

- L3 (gateway) rules are impure by nature and therefore excluded from the payment path. This means
  gateway reachability is validated on a schedule and during onboarding, never per payment — a
  deliberate staleness we accept because the circuit breaker (§10) covers the live case.
- Warnings (`Severity: WARNING`) do not block but must be surfaced somewhere, which is one more
  output channel to design.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| A rule performs I/O on the hot path | Medium | High — latency budget blown, non-determinism | Pure rule packages may not import I/O packages; enforced by `scripts/check-architecture.sh`; L3 lives in a package the payment path cannot import | Architecture check; stage-10 latency p99 against the 5 ms budget |
| Rule IDs reused or renamed | Medium | Medium — breaks client error handling and historical analysis | IDs are append-only; a CI check compares the registry against the committed catalog and fails on a removed or repurposed ID; deprecation is a documented status, not a deletion | Registry diff check |
| Rule registry grows unmaintained | High over time | Medium | `TestEveryRuleIsDocumented`; per-rule outcome counters make never-firing rules visible for removal review | Rules with zero firings over 90 days |
| Validation duplicated between levels with divergent semantics | Medium | Medium — inconsistent behaviour by entry point | Each level has a defined subject type and responsibility; a check appearing at two levels requires justification in review; the same rule function may be *registered* at two levels but not reimplemented | Code search for duplicated predicates; conflicting outcomes for the same input at different levels |
| A new rule rejects valid traffic across all tenants | Medium | **High** — revenue impact | New ERROR-severity rules ship first as `WARNING` with counters, promoted to `ERROR` only after the observed firing rate matches expectation; feature-flagged per tenant tier for rollout | Rule firing rate on introduction; authorization-rate SLI |
| Configuration values drift from what rules assume | Medium | Medium | L4 validates configuration coherence at write time; L5 takes config as an explicit typed input, so a missing field is a compile or unmarshal error, not a silent zero | `CONFIGURATION_INVALID` rate; L5 rules erroring on absent inputs |

## Validation

- **Documentation gate:** `TestEveryRuleIsDocumented` — every registered ID has meaning and
  remediation in `docs/validation-plane.md`. Build fails otherwise.
- **Purity gate:** architecture check asserts pure rule packages import no I/O.
- **Latency:** stage 7 ≤ 3 ms, stage 10 ≤ 5 ms, stage 15 ≤ 3 ms at p99, from server-side
  histograms. Rules are the only thing in those stages, so a regression is attributable.
- **Error actionability:** every `4xx`/`422` returned to a client carries at least one rule ID in
  `details[].code`. Asserted in the contract test suite for the public API.
- **Coverage:** every rule has table-driven tests covering pass, fail and boundary. Enforced by a
  registry-completeness test, not by coverage percentage.
- **Support signal:** share of validation-related support tickets that required reading source
  code to answer. Target: near zero. That number is the real test of whether IDs and documentation
  are doing their job.

## Revisit criteria

Reopen if:

1. Rule changes become the dominant reason for data-plane deploys (say > 30 % of releases) — at
   that point a **narrowly scoped, sandboxed, statically-analysed** expression layer for
   *threshold-shaped* rules only becomes worth evaluating, with a hard prohibition on loops,
   unbounded execution and I/O.
2. A regulator requires rule authoring by non-engineers with an audit trail — the answer would
   still be a constrained authoring surface generating reviewed code, not a runtime interpreter.
3. The seven-level model proves to have a genuine gap (a class of validation that fits nowhere),
   which would be evidence the taxonomy is wrong rather than incomplete.
4. Per-tenant rule variation becomes common enough that the registry cannot express it as
   configuration-parameterised rules.
