# ADR-013: Gateway timeouts leave the payment in PROCESSING; no timer may fail a payment

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §12.3 (the timeout rule), §1.3 ambiguity A7, §9.1 (attempt outcomes), §24 (failure mode catalog) of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Depends on ADR-012 (attempts); related to ADR-011 (mandatory `Lookup`)

## Context

At §12 stage 14 the orchestrator calls a gateway with a hard 8 s timeout. Three things can
happen: a response arrives, a definitive error arrives, or **nothing arrives**. The third case is
the one that decides whether this platform is trustworthy.

The forces:

1. **A timeout is not a failure — it is an absence of information.** The request may never have
   reached the gateway; it may have reached it and been declined; it may have reached it, been
   *approved*, and the response lost on the way back. All three are consistent with the same
   observation on our side. The customer's card may already be charged.
2. **The tempting behaviour is catastrophic.** "Timeout → mark failed → retry on another gateway"
   is the single most common cause of double charges in real payment platforms (baseline A7 says
   this in as many words). The first authorization succeeds silently; the failover succeeds
   visibly; the customer is charged twice and the merchant eats a chargeback plus a fine.
3. **The opposite error is also real.** Leaving a payment in `PROCESSING` forever, with no
   resolution mechanism, is a stuck payment: the merchant cannot ship, the customer's funds are
   held, and support has no answer. `PROCESSING` is only acceptable if resolution is *guaranteed
   and bounded*.
4. **Base rate matters for sizing.** At 5 000 TPS, even a 0.05 % ambiguity rate is 2.5 payments
   per second, ~216 000 per day, entering reconciliation. The resolution path must be automated
   and cheap; a human queue is not an option.
5. **Clients need a truthful answer immediately.** A synchronous API cannot hold the connection
   for the reconciliation window. It must be able to say "processing" and mean it.

What breaks if we choose wrong: double charges at scale, or a growing pool of permanently stuck
payments, or an API that reports `FAILED` for payments that actually succeeded — the worst of
the three, because the merchant then re-charges the customer *deliberately*.

## Decision

**A gateway timeout or ambiguous transport error records the attempt as `TIMEOUT_UNKNOWN` and
leaves the payment in `PROCESSING`. No timer, no scheduler, and no request-path code may
transition a payment to `FAILED` because time elapsed. Only positive evidence resolves an
ambiguous attempt.**

1. **Attempt outcome:** `TIMEOUT_UNKNOWN` (§9.1). It is never retried automatically and never
   produces a failover attempt (ADR-012). It is a terminal *attempt* outcome that is resolvable
   only by the reconciler.
2. **Payment state:** stays `PROCESSING`. Per §9, `PROCESSING → PENDING` is available for
   asynchronous/unknown outcomes, and `PENDING → {AUTHORIZED, CAPTURED, FAILED, EXPIRED}` is the
   resolution edge.
3. **Client response:** `202 Accepted` semantics on the synchronous endpoint — HTTP `200` with
   `status: "processing"` and no terminal outcome claimed. The client polls
   `GET /v1/payments/{id}` or waits for the merchant webhook. The API never reports a payment as
   failed when we do not know it failed.
4. **Resolution paths, in order of speed** (§12.3):
   - **(a) Gateway webhook** — typically seconds. The webhook carries the gateway reference; the
     ingress path resolves it to the attempt.
   - **(b) Reconciler polling `Adapter.Lookup(gateway_idempotency_key)`** — the deterministic key
     from §14.4 is stored on the attempt *before* dispatch, so it is always available. Schedule:
     first poll at 30 s, then exponential with jitter (1 m, 2 m, 5 m, 15 m, 30 m, hourly),
     bounded by the gateway's own authorization lifetime.
   - **(c) Settlement report** — hours to a day, the backstop.
   `Lookup` is a **mandatory** SPI method (ADR-011) precisely so path (b) always exists.
5. **`payment.reconciliation_required.v1`** is emitted when an attempt enters `TIMEOUT_UNKNOWN`
   (§13.2 marks this event as **alerting**).
6. **Expiry is not failure.** A payment may reach `EXPIRED` — but only on positive evidence that
   the authorization window closed (gateway confirmation or the gateway's documented auth
   lifetime having demonstrably elapsed *and* a `Lookup` returning not-found). Even then, the
   transition is made by the reconciler with evidence recorded, not by a timer firing on a clock.
7. **Human escalation** is a bounded backstop: an ambiguous attempt unresolved after 24 h becomes
   a critical reconciliation exception and pages. It is not the primary mechanism.

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Stay `PROCESSING`, resolve by reconciliation (chosen)** | The only option that cannot produce a double charge from ambiguity; every ambiguous attempt has a durable row and a deterministic key, so resolution is always possible; the client is told the truth; the ledger never records an outcome we have not observed | Introduces a state the client must handle and an SLA for resolution; requires a reconciler, a lookup API on every gateway, and monitoring of an ambiguity queue; merchants must be educated that "processing" is a real, non-terminal answer | **Accepted** |
| **Timeout → `FAILED`, then fail over to the next gateway** | Best possible customer experience *when the first attempt genuinely failed*: the payment completes on the fallback within seconds; the highest apparent authorization rate; no reconciliation infrastructure needed; the simplest code | Charges the customer twice whenever the first attempt actually succeeded. The probability is not small: a gateway under load frequently approves and then fails to deliver the response — timeouts correlate with load, and load correlates with successful-but-slow. Each occurrence is a chargeback, a fine, remediation cost and reputational damage; at scale it is also a scheme-level ratio that attracts scrutiny. This is the option product pressure will push for, framed as "don't lose the sale", and it is explicitly forbidden by A7 | Rejected |
| **Timeout → `FAILED` after N seconds with no failover** | Simple; bounded; no double charge from *our* retry; the client gets a terminal answer quickly | We report `FAILED` for a payment that may have succeeded. The merchant, believing it failed, asks the customer to pay again — so the double charge happens anyway, one layer up, and now it is the *merchant's* fault in the customer's eyes and ours in the merchant's. The ledger also permanently records a false outcome, which corrupts reconciliation and authorization-rate analytics | Rejected — moves the harm rather than removing it |
| **Synchronously block the client until resolution** | Client gets a definitive answer, no new state to handle | Resolution can take minutes to hours (path (c) takes a day). Holding HTTP connections that long exhausts the ingress tier and violates every latency SLO; it is not implementable at 5 000 TPS | Rejected |
| **Optimistically assume success on timeout** | No double charge from failover; merchant can ship | We would record money as moved without evidence. If it did not move, we have a phantom authorization, an unfulfillable capture, and a ledger that does not reconcile — plus a merchant who shipped goods for free | Rejected |

## Consequences

### Positive

- Ambiguity cannot become a double charge. The one input the system refuses to act on is the
  absence of information.
- Every ambiguous attempt is durable, keyed and lookupable, so resolution is mechanical.
- The ledger only ever records observed outcomes, which is what makes reconciliation meaningful.
- The `Lookup` requirement in the SPI forces us to reject, at integration time, any gateway that
  cannot support this guarantee — a much cheaper place to discover it than in production.

### Negative

- **The client contract is harder.** `status: "processing"` is a real state that SDKs, merchant
  integrations and checkout UIs must handle. Some merchants will implement it badly.
- Merchant-visible latency to a terminal answer, in the ambiguous case, is seconds to minutes
  (webhook or first poll) rather than immediate.
- We must build and operate a reconciler, an exception queue, and alerting on ambiguity volume.
- A gateway with a slow or unreliable `Lookup` degrades our resolution SLA and we inherit that.
- Support tooling must expose "why is this payment still processing?" clearly, or it becomes a
  ticket driver.

### Neutral / accepted costs

- Measured authorization rate looks slightly worse than a competitor who fails over on timeout,
  because their double charges are counted as successes until the chargebacks arrive. We accept
  losing that comparison and should be able to explain it commercially.
- Ambiguity volume becomes a first-class operational metric and a gateway scorecard item.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| Someone adds a timer that fails stuck payments (e.g. "clean up old PROCESSING rows") | Medium | **Critical** — reintroduces the false-failure path | The `FAILED` transition requires an evidence argument (gateway response or lookup result) in the domain API; there is no zero-argument way to fail a payment; test `TestNoTimerCanFailAPayment` asserts no scheduled job holds the capability | Code review; the test; audit of transitions whose evidence field is empty |
| Reconciler cannot resolve because `Lookup` is broken or the gateway lost the transaction | Low–Medium | High — payment stuck | Escalation to critical exception at 24 h with paging; manual resolution runbook with recorded evidence; settlement report as the final backstop | `pp_reconciliation_exceptions{severity="critical"}`; age histogram of unresolved ambiguous attempts |
| Ambiguity volume spikes during a gateway incident and swamps the reconciler | Medium | Medium | Reconciler scales horizontally on the queue; polling schedule is exponential so the backlog does not amplify load on an already-degraded gateway; circuit breaker (§10) shifts new traffic away | Ambiguous-attempt creation rate; reconciler lag |
| Merchant treats `processing` as failure and re-charges the customer | Medium | High — a double charge we did not cause but will be blamed for | Prominent SDK and documentation guidance; the API returns no terminal outcome and no decline reason; certification (§11.4) exercises the async loop so integrators meet this state before going live; merchant webhooks carry the eventual terminal outcome | Support tickets; payments created within seconds of a `PROCESSING` payment for the same card fingerprint and amount (a detectable signature) |
| Customer's funds held by an authorization that we never capture | Medium | Medium — customer complaint, not financial loss | Once resolved to `AUTHORIZED`, normal auth-expiry handling applies; unresolvable-but-successful attempts are voided as part of the reconciliation runbook | Age of `AUTHORIZED` payments with no capture; void volume from reconciliation |
| The 8 s timeout is too aggressive and manufactures ambiguity | Medium | Medium | 8 s is set above the p99.9 of every integrated gateway's authorize latency; reviewed quarterly against `pp_gateway_request_duration_seconds` | Ratio of `TIMEOUT_UNKNOWN` to total attempts per gateway; gateway latency distribution against the 8 s line |

## Validation

- **The decisive metric:** double charges attributable to timeout handling. Target **zero per
  quarter**, measured from chargeback reason codes and merchant reports.
- **Resolution SLO:** ≥ 99 % of `TIMEOUT_UNKNOWN` attempts resolved within 5 minutes; ≥ 99.9 %
  within 1 hour; **100 % within 24 hours** or paged as a critical exception.
- **Chaos test:** `tests/chaos` induces a gateway that accepts, approves, and then drops the
  response. Assert: attempt is `TIMEOUT_UNKNOWN`, payment is `PROCESSING`, no second attempt is
  created, the reconciler resolves it to `AUTHORIZED` via `Lookup`, and exactly one authorization
  exists at the gateway.
- **Ambiguity rate:** `TIMEOUT_UNKNOWN` share of attempts, per gateway, tracked as a gateway
  scorecard metric. A rate above ~0.1 % for a gateway is a commercial conversation.
- **Stuck-payment count:** payments in `PROCESSING` older than 1 hour. Target: zero in steady
  state.

## Revisit criteria

Reopen if:

1. Ambiguity resolution routinely exceeds the 5-minute p99 — the reconciler design, not this
   policy, is what changes.
2. A gateway offers a genuinely idempotent authorize endpoint with a synchronous
   "was-this-processed" check fast enough to run inline (sub-second) — that would let us resolve
   ambiguity *within* the request and return a terminal answer, which is strictly better and does
   not contradict this ADR's principle (still evidence, never a timer).
3. The volume of `PROCESSING`-state complaints indicates the client contract is unworkable for a
   material share of integrators — the response is better SDK ergonomics and clearer webhooks,
   not failing the payment.
4. Regulatory guidance mandates a maximum time-to-terminal-state for payment instructions, which
   would require negotiating explicit resolution SLAs into gateway contracts.
