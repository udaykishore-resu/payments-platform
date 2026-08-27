# 09 — Gateway Routing Decision

## What this shows and why it matters

Routing runs at stage 12 with a 5 ms budget and produces a **Routing Plan**: an ordered,
reason-annotated list of candidate gateways for one payment at one instant, persisted with the
payment for auditability. The structure is deliberately two-phase — **hard filters** that express
correctness and compliance (a gateway that cannot do EUR simply cannot be chosen), then **scoring**
that expresses preference (health, success rate, cost, latency). Mixing the two is the classic
routing bug: a cost weight must never be able to outvote a data-residency constraint.

## Diagram A — Candidate generation, filtering, scoring, plan

```mermaid
flowchart TB
  IN["Payment context - currency, method, country, amount, merchant config"]
  CAND["Candidate generation - every gateway_connection for this merchant in CERTIFIED"]

  subgraph HARD["Hard filters - correctness and compliance, no weighting"]
    F1["Capability descriptor supports currency"]
    F2["Capability descriptor supports payment method"]
    F3["Capability descriptor supports country and operation"]
    F4["Connection is CERTIFIED and not de-provisioned"]
    F5["Data residency policy allows this gateway region"]
    F6["Circuit breaker is not OPEN for this gateway and operation"]
    F7["Merchant pin or contractual override, if present, wins outright"]
  end

  EMPTY["Candidate set empty"]
  ERR["503 NO_ELIGIBLE_GATEWAY with Retry-After - fail closed"]

  subgraph SCORE["Scoring - configurable weights from the merchant configuration"]
    W1["health 0.4 - HEALTHY DEGRADED UNHEALTHY PROBING, confidence decays with staleness"]
    W2["successRate 0.3 - recent authorization rate for this corridor"]
    W3["cost 0.2 - scheme plus gateway fee model"]
    W4["latency 0.1 - p99 for this gateway and operation"]
  end

  RULES["Configuration routing rules - when currency EUR and method CARD then primary adyen"]
  STRAT["Strategy PRIORITY_WITH_FALLBACK - primary first, then declared fallbacks, then scored remainder"]
  PLAN["Routing Plan rpl_ - ordered candidates plus a reason code per position"]
  PERSIST["Persist plan with the payment - routing_plans table"]
  METRIC["pp_routing_decisions_total by gateway and reason"]
  DISPATCH["Attempt 1 dispatched to position 1"]

  IN --> CAND --> F1 --> F2 --> F3 --> F4 --> F5 --> F6 --> F7
  F7 -->|"none survive"| EMPTY --> ERR
  F7 -->|"one or more survive"| RULES --> STRAT
  STRAT --> W1
  STRAT --> W2
  STRAT --> W3
  STRAT --> W4
  W1 --> PLAN
  W2 --> PLAN
  W3 --> PLAN
  W4 --> PLAN
  PLAN --> PERSIST --> DISPATCH
  PLAN --> METRIC
```

## Diagram B — Health input to the routing decision

```mermaid
stateDiagram-v2
    [*] --> HEALTHY
    HEALTHY --> DEGRADED: error rate above 5 percent over 30 s, min 20 samples
    DEGRADED --> HEALTHY: error rate recovers
    DEGRADED --> UNHEALTHY: error rate above 25 percent or p99 above 5 s
    UNHEALTHY --> PROBING: cool-down 30 s elapsed, circuit HALF_OPEN
    PROBING --> HEALTHY: 3 consecutive successes, circuit CLOSED
    PROBING --> UNHEALTHY: any failure, cool-down doubles capped at 5 min

    note right of UNHEALTHY
        Circuit OPEN. Hard filter F6 removes
        this gateway from candidate generation
        entirely, it is not merely down-weighted.
    end note

    note right of DEGRADED
        Still eligible. Enters scoring with a
        reduced health component, so traffic
        shifts gradually rather than cliff-edging.
    end note
```

## Legend and notes

- **Health is per `(gateway_id, operation)`, never per merchant.** Per-merchant samples are too
  sparse to be statistically meaningful — a merchant doing 20 payments a day cannot produce a
  reliable error rate over a 30-second window. Per-merchant behaviour is expressed as a
  contractual *pin* (filter F7), not as a health signal (§10).
- **`DEGRADED` down-weights, `UNHEALTHY` excludes.** This is the difference between the scoring
  phase and the filter phase. Degradation shifts traffic smoothly; an open circuit removes the
  gateway from the candidate set so no attempt is even created against it (`GATEWAY_CIRCUIT_OPEN`).
- **Filter F5 is a compliance filter, not a preference.** The routing engine will not select a
  gateway whose region violates the tenant's declared data-residency policy, and no combination of
  cost or latency weights can override it (§17.3).
- **Empty candidate set fails closed.** `503 NO_ELIGIBLE_GATEWAY` with `Retry-After`. Rejecting a
  payment costs one lost sale; routing to an ineligible gateway costs a compliance breach or a
  guaranteed decline (§24).
- **The plan is persisted, and that is the audit story.** `routing_plans` records the ordered
  candidates *and the reason code for each position* at the instant of decision. Six months later
  the question "why did this payment go to Adyen?" has a recorded answer, including what the
  health state was at the time.
- **Health confidence decays with staleness.** Gateway health is an AP read served from local
  windows plus Kafka gossip; under partition we serve stale health with decayed confidence rather
  than assuming healthy (§15).
- **The plan is built once per payment, not per attempt.** Failover walks down the existing plan;
  it does not re-run routing. See diagram 10.

## Related

- [Design baseline §10 gateway health, §12 stage 12, §23 routing configuration, §17.3 residency](../spec/00-design-baseline.md)
- [10 — Gateway failover](10-gateway-failover.md), [08 — Payment flow](08-payment-flow.md)
- [docs/data-plane.md](../data-plane.md), [docs/failure-handling.md](../failure-handling.md)
