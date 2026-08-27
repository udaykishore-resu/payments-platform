# 10 — Gateway Failover

## What this shows and why it matters

Failover is the most dangerous mechanism in a payment orchestrator, because a careless
implementation turns a transient error into a double charge or turns the platform into a card
testing service. This diagram shows the three branches that matter and, critically, the two where
failover **must not** happen: a hard decline (retrying a stolen card on another gateway is card
testing behaviour and gets the platform de-registered from the schemes) and a `TIMEOUT_UNKNOWN`
(we do not know whether money moved, so retrying anywhere is a coin flip on a double charge).

## Diagram A — Failover decision

```mermaid
flowchart TB
  A1["Attempt 1 dispatched to routing plan position 1"]
  RESP["Gateway response or transport outcome"]

  OK["SUCCESS - authorized or captured"]
  ERRT["ERROR - our side or transport failed before the gateway could act"]
  DEC["DECLINED - the gateway definitively said no"]
  TMO["TIMEOUT_UNKNOWN - no response within the 8 s hard timeout"]

  SOFT["Decline reason is in the retryable decline set - issuer unavailable, soft do-not-honour, network error"]
  HARD["Hard decline - stolen card, invalid account, pickup card, do-not-honour hard code"]

  SAMEG["Retry at most 2 times with jitter on the SAME attempt and SAME gateway"]
  BRK["Circuit breaker records the failure, may open for this gateway and operation"]
  NEXT["Advance to routing plan position 2"]
  NEWATT["Create a NEW attempt - new attempt_id, new derived gateway idempotency key"]
  NOCAND["No further eligible position"]

  TERMF["Payment to FAILED, terminal, reason code preserved"]
  NEVER["NO FAILOVER - card testing risk, scheme de-registration risk"]
  STAY["Payment stays PROCESSING, attempt marked TIMEOUT_UNKNOWN"]
  RECQ["payment.reconciliation_required.v1 to the reconciler, alerting"]
  ERR503["503 NO_ELIGIBLE_GATEWAY"]

  A1 --> RESP
  RESP --> OK
  RESP --> ERRT
  RESP --> DEC
  RESP --> TMO

  ERRT --> SAMEG
  SAMEG -->|"still failing"| BRK --> NEXT
  DEC --> SOFT
  DEC --> HARD
  SOFT --> NEXT
  HARD --> NEVER --> TERMF
  TMO --> STAY --> RECQ

  NEXT --> NEWATT --> A2["Attempt 2 dispatched"]
  NEXT --> NOCAND --> ERR503
  OK --> DONE["Invariant I3 - at most one attempt per payment in a successful terminal state"]
```

## Diagram B — Circuit opens and traffic shifts

```mermaid
sequenceDiagram
    autonumber
    participant PO as payment-orchestrator
    participant BR as Breaker for stripe authorize
    participant ST as Stripe
    participant AD as Adyen
    participant RT as Routing engine
    participant OB as Outbox and Kafka
    participant CP as control-plane-api

    PO->>BR: authorize, attempt att_1
    BR->>ST: authorize
    ST-->>BR: 503 after 6 s
    Note over BR: error rate crosses 5 percent over 30 s with 20 samples, state DEGRADED
    PO->>BR: authorize, further payments
    BR->>ST: authorize
    ST-->>BR: 503 repeatedly
    Note over BR: error rate above 25 percent, state UNHEALTHY, circuit OPEN
    BR->>OB: gateway.health_changed.v1 on pp.gateways.health.v1
    OB-->>RT: consume health change
    OB-->>CP: control plane records health for operator visibility

    PO->>RT: routing plan for att_1 already exists, advance to position 2
    PO->>PO: create NEW attempt att_2 with a NEW derived idempotency key
    PO->>AD: authorize att_2
    AD-->>PO: approved
    PO->>OB: payment.attempted.v1 then payment.authorized.v1
    Note over PO: att_1 remains recorded as ERROR, it is never mutated

    Note over BR: cool-down 30 s, circuit HALF_OPEN, state PROBING
    PO->>BR: next payment, probe allowed
    BR->>ST: authorize
    ST-->>BR: approved, 3 consecutive successes needed
    BR->>OB: gateway.health_changed.v1 back to HEALTHY
    OB-->>RT: stripe re-enters candidate generation
```

## Legend and notes

- **A retry is not a failover.** A transport `ERROR` is retried at most twice with jitter on the
  *same* attempt against the *same* gateway, reusing the same derived idempotency key so the
  gateway dedupes. Only when that budget is exhausted does the orchestrator advance the routing
  plan and create a **new attempt** (§24, §14.4).
- **A new attempt means a new key means a genuinely new authorization.** That is the whole point
  of deriving the gateway key from `attempt_id` rather than from the client's key: a same-gateway
  retry is safe, and a cross-gateway failover is correctly treated as a distinct authorization,
  with the previous one separately voided or reconciled (A10).
- **Hard decline is terminal and never failed over.** This is a business rule with regulatory
  teeth, not a performance tuning choice. The ACL maps each gateway's proprietary reason codes to
  a normalized set and the retryable-decline membership is a property of that normalized code —
  which is exactly what the §11.4 certification assertion "a declined test card yields a mapped
  `DECLINED` with a normalized reason code" proves before go-live.
- **`TIMEOUT_UNKNOWN` never fails over and never auto-fails.** The payment stays `PROCESSING` and
  `payment.reconciliation_required.v1` is emitted with alerting. Resolution, fastest first: the
  gateway webhook arrives; the reconciler polls the gateway's lookup API using our deterministic
  key; the settlement report lands (A7, §12.3).
- **Old attempts are immutable.** Failover creates a row; it never mutates the previous one. The
  attempt history is the forensic record of what was tried where.
- **Invariant I3 is the structural backstop.** A partial unique index on
  `(payment_id) WHERE outcome='SUCCESS'`, partition-aligned per Amendment A-02, makes it
  impossible for two attempts of one payment to both be successful — even if every layer of
  application logic above it is wrong.
- **The circuit is per `(gateway, operation)`.** Stripe's `authorize` opening does not remove
  Stripe's `refund` from the candidate set — you must always be able to give money back.

## Related

- [Design baseline §9.1 attempt outcomes, §10 gateway health, §14.4 gateway idempotency, §24 failure catalog](../spec/00-design-baseline.md)
- [09 — Gateway routing](09-gateway-routing.md), [13 — State machines](13-state-machines.md)
- [docs/failure-handling.md](../failure-handling.md), [docs/payment-flow.md](../payment-flow.md)
