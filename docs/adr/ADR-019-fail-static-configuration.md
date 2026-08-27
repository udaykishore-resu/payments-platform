# ADR-019: Fail-static configuration in the data plane with a defined staleness cliff

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §15 (consistency model — "fail-static, not fail-open"), §1.3 ambiguity A5, §12 stage 9, §23 (configuration document) of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Depends on ADR-007 (plane independence); related to ADR-020 (invalidation transport)

## Context

Every payment reads merchant configuration: supported currencies and methods, routing policy,
risk limits, refund window, feature flags (§23). ADR-007 establishes that this read must be a
local snapshot rather than a call to the control plane. This ADR answers the question that
follows: **what happens when the snapshot cannot be refreshed?**

The forces:

1. **Both naive answers are wrong.** Fail-closed (stop processing when config cannot be
   refreshed) turns every control-plane blip into a revenue outage — the exact coupling ADR-007
   exists to prevent. Fail-open (process without limits when config is unavailable) means risk
   limits, blocked-country lists and 3DS thresholds stop applying at precisely the moment the
   platform is least healthy, which is a compliance failure and an attacker's ideal window.
2. **Configuration changes slowly; payments happen constantly.** A merchant's configuration
   changes a handful of times a month. Serving a configuration that is 10 minutes old is almost
   always identical to serving the current one. The staleness that matters is not average
   staleness but the specific cases where a change was urgent.
3. **Some changes are urgent.** `merchant.suspended.v1` is marked **priority invalidation** in
   §13.2 for a reason: continuing to process for a merchant we have just suspended for risk or
   compliance reasons is materially different from serving a slightly old spending limit.
4. **Unbounded staleness is indefensible.** "We were using six-hour-old limits" is not an answer
   we can give a regulator or a tenant. Whatever we serve stale, there must be a defined bound
   and a defined behaviour at that bound.
5. **New and existing merchants are different risks.** For an existing merchant with a known
   configuration, continuing with last-known-good is a small, bounded risk. For a merchant we
   have never seen — who may have been created, suspended or restricted in the window we cannot
   see — we have no basis at all.
6. **Silent degradation is the real danger.** A stale snapshot that nobody notices is worse than
   a loud failure. Staleness must be a first-class, exported, alerting signal.

What breaks if we choose wrong: a control-plane incident becomes a payment outage (fail-closed),
or an unbounded window where limits are not enforced (fail-open), or an unbounded window nobody
knew about (silent staleness).

## Decision

**The data plane fails *static*: it continues serving with its last-known-good configuration
snapshot, with a defined staleness cliff at which it fails closed for new merchants while
continuing to serve existing ones.**

1. **Normal operation:** bounded staleness ≤ 30 s p99 (§18), maintained by Kafka push
   invalidation (`configuration.published.v1`, `merchant.*`) plus a periodic full refresh as a
   backstop against a missed event.
2. **Control plane degraded or gone:** keep processing with the last-known-good snapshot.
   Configuration is *versioned*, so "last-known-good" is a specific, identifiable version, and
   the version in effect is recorded on the payment.
3. **Staleness is exported and alerted:** `pp_config_snapshot_age_seconds` per service (§22.2),
   alerting at **5 minutes** (§15, §22.4).
4. **The cliff — `max_config_staleness`, default 15 minutes:**
   - **Existing merchants** (present in the snapshot with an `ACTIVE` state): continue to be
     served, using the snapshot's policy. Revenue continues.
   - **Merchants not in the snapshot**, or whose snapshot entry is not `ACTIVE`: **fail closed**
     — `409 MERCHANT_NOT_ACTIVE` / `404 MERCHANT_NOT_FOUND`. We do not admit a merchant we cannot
     currently verify.
   - Beyond the cliff, **risk-increasing operations are refused** for everyone: no new payment
     above the snapshot's `require3DSAbove` threshold without 3DS, no configuration-dependent
     feature enabled by a flag we cannot confirm. Refunds, voids and webhook processing continue
     unconditionally — §8 requires that you must always be able to give money back.
5. **Priority invalidation is a separate, faster path.** Suspension and termination events are
   consumed ahead of routine config updates, and a suspension also writes a data-plane-visible
   marker so that even a missed event surfaces at the next merchant load.
6. **The snapshot is durable, not just in-memory.** It is persisted in the data plane's own store
   so a pod restart or a scale-out event during a control-plane outage does not produce a cold,
   unservable pod. Readiness requires *a* snapshot, not a *fresh* one.
7. **The effective configuration version is recorded on every payment**, so "what policy applied
   to this payment?" is answerable from the payment row, not from current state.

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Fail-static with a defined cliff (chosen)** | Control-plane outages do not stop revenue for existing merchants; limits and policies continue to be enforced from the last-known-good version, so compliance posture degrades gracefully rather than vanishing; the cliff bounds the exposure and gives a defensible answer ("never more than 15 minutes stale, and here is the versioned document that applied"); asymmetric treatment of new vs existing merchants matches the actual risk asymmetry | Two operating modes to reason about, test and document; the cliff value is a judgement call; new-merchant onboarding is blocked during a long control-plane outage; a stale policy *is* being enforced for up to 15 minutes, which must be disclosed | **Accepted** |
| **Fail-open (process without configuration constraints when unavailable)** | Maximum availability; simplest possible behaviour; no cliff to tune; never turns away a payment | Risk limits, velocity checks, blocked-country lists and 3DS thresholds stop applying exactly when the platform is degraded — which is the window an attacker would choose and the window a regulator will ask about. It also cannot work mechanically: routing needs the merchant's gateway connections, and there is no "open" default for which acquirer to use. Availability bought by disabling controls is not availability | Rejected |
| **Fail-closed (reject payments when configuration cannot be refreshed)** | Strictest correctness: never act on stale policy; trivially defensible to a regulator; no cliff, no modes, no stale-policy disclosure | Makes data-plane availability depend on control-plane availability, undoing ADR-007 entirely: a 43-minute control-plane budget becomes a 43-minute payment outage. For a configuration that changes a few times a month, refusing payments because we cannot re-read an unchanged document trades certain revenue loss for a hypothetical policy divergence. This is the option a compliance-minded engineer pushes for, and the "never act on stale data" principle is genuinely sound — it loses because the alternative is not "act on unknown data" but "act on a known, versioned, recent document" | Rejected as the general behaviour; **adopted specifically past the cliff for unknown merchants**, where the principle does apply |
| **Fail-static with no cliff (serve last-known-good indefinitely)** | Maximum availability; simplest of the static variants; no mode switch | Unbounded staleness has no defensible answer. A multi-hour control-plane outage would leave us admitting new merchants we cannot verify and enforcing arbitrarily old limits, with no automatic protection and no forcing function to fix the outage | Rejected |
| **Synchronous read with a circuit breaker falling back to cache** | Strong consistency in the common case; the breaker bounds the damage | This is ADR-007's rejected synchronous option with a fallback bolted on: it still puts control-plane latency on the payment path in the normal case, and the fallback is the same snapshot we would otherwise maintain. Strictly more machinery, strictly worse steady-state latency | Rejected |

## Consequences

### Positive

- A total control-plane outage is a *degraded* condition, not an outage: existing merchants keep
  processing for at least 15 minutes, and in practice most control-plane incidents are shorter.
- Policy enforcement never disappears — it degrades to a known, versioned, recent document.
- The cliff creates a forcing function: past 15 minutes the business impact escalates
  (new-merchant onboarding blocked, risk-increasing operations refused), which correctly matches
  the urgency of restoring the control plane.
- The configuration version in effect is recorded per payment, so post-hoc questions have exact
  answers.
- The behaviour is disclosable: "configuration is eventually consistent within 30 seconds, and
  never more than 15 minutes stale" is a statement we can put in a contract.

### Negative

- For up to 15 minutes, a merchant whose limits were *just* tightened may transact under the old
  limits. This is a real, bounded exposure that must be disclosed to compliance and tenants.
- Two modes (normal, past-cliff) means more behaviour to test, more to document, and a mode
  transition to get right.
- New-merchant admission is unavailable during a long control-plane outage, which is visible to
  tenants onboarding at that moment.
- `max_config_staleness` is a tuning parameter with a genuine trade-off and no objectively correct
  value.

### Neutral / accepted costs

- The snapshot must be durable and its refresh path must be independently monitored — additional
  machinery relative to a naive cache.
- Different data-plane pods may briefly hold different snapshot versions. Acceptable because
  configuration is versioned and each payment records the version it used; not acceptable to
  ignore, so version skew across pods is also monitored.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| Suspended merchant keeps processing during the staleness window | Medium | High — risk or compliance exposure | Priority invalidation path for `merchant.suspended.v1`; suspension also writes a data-plane-visible marker read at stage 9; suspension permits refunds/voids by design so the merchant is stopped only for new money-in | Count of payments accepted for a merchant whose suspension timestamp precedes the payment; propagation-probe SLI |
| Staleness rises silently | Medium | **High** — the whole mechanism depends on visibility | `pp_config_snapshot_age_seconds` per service with a 5-minute alert; the cliff behaviour itself emits a distinct event and metric when engaged | The gauge; cliff-engaged counter |
| Cliff never exercised, so its behaviour is broken when first needed | High if untested | High | `internal/platform/config/provider_test.go::TestStalenessLadder` and `::TestFailedRefreshIsFailStatic` assert both sides of the asymmetry in isolation; **no test runs past the cliff against a live data plane**, and no staging drill has been run | Unit test result; the drill record does not exist |
| Cliff too aggressive and cuts off legitimate traffic | Low | Medium | The cliff only blocks *unknown* merchants and risk-increasing operations; existing-merchant traffic is unaffected regardless of duration | Rate of `MERCHANT_NOT_FOUND`/`MERCHANT_NOT_ACTIVE` during a cliff event |
| Missed invalidation event leaves an indefinitely stale entry while the gauge looks healthy | Medium | Medium | Periodic full refresh as a backstop; snapshot carries a monotonically increasing config version per merchant and a whole-snapshot generation, so a gap is detectable | Version-gap counter; divergence between control-plane current version and snapshot version |
| Pods disagree on snapshot version during a rollout | Medium | Low–Medium | Version recorded per payment; version skew across pods is a monitored gauge with an alert on sustained skew | Snapshot-version skew gauge |
| Fail-static reasoning creeps into other subsystems where it is wrong (e.g. credentials) | Medium | High | This ADR is scoped to *configuration*. Credentials, tokens and keys are **not** fail-static: an expired credential fails the operation | Review; credential-error handling tests |

## Validation

- **Chaos test:** scale the control plane to zero for 20 minutes under load. Assert: existing
  merchants process normally throughout; the cliff engages at exactly `max_config_staleness`;
  new-merchant requests fail closed after the cliff and not before; refunds and voids succeed the
  whole time.
- **Propagation SLI:** synthetic probe measures publish-to-effect. SLO p99 ≤ 30 s (§18); alert at
  > 5 min.
- **Staleness distribution:** `pp_config_snapshot_age_seconds` p99 in steady state should sit well
  under 30 s. A rising floor indicates the invalidation path is degrading before it fails.
- **Cliff exposure:** total minutes per quarter spent past the cliff. Target: zero. Any occurrence
  is a control-plane availability failure and consumes that error budget.
- **Auditability:** for a sample of payments, the recorded configuration version must match a real
  published version and must be retrievable. This is what makes "we enforced a known policy" a
  checkable claim.

## Revisit criteria

Reopen if:

1. Regulatory or contractual requirements demand a shorter maximum staleness for a specific policy
   class (e.g. sanctions screening lists) — the likely answer is a separate, stricter propagation
   path for that class, not a change to the general model.
2. Cliff events occur more than once a quarter, indicating control-plane availability is the real
   problem and the cliff is masking it.
3. Measured propagation p99 exceeds 30 s consistently, which would mean the invalidation transport
   (ADR-020) needs attention before this policy does.
4. The 15-minute default proves either too generous (a real exposure materialises inside the
   window) or too tight (legitimate operations blocked during routine control-plane maintenance).
