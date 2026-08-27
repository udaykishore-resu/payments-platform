# 08 — Payment Flow: Authorization, Capture, Settlement

## What this shows and why it matters

The full money path for a two-step card payment: authorize now, capture later, settle when the
gateway says so. Two diagrams because authorization and capture/settlement have different actors
and different timescales — authorization is a sub-second synchronous request, capture may be days
later, and settlement is observed asynchronously from a gateway report, never computed by us
(A12). Read this alongside diagram 06: every stage number referenced here maps to the 17-stage
pipeline.

## Diagram A — Authorization

```mermaid
sequenceDiagram
    autonumber
    participant MB as Merchant backend
    participant PA as payment-api
    participant IDM as Idempotency store, Postgres
    participant CFG as Config cache
    participant PO as payment-orchestrator
    participant RK as Risk engine
    participant RT as Routing engine
    participant GA as Gateway adapter
    participant GW as Gateway
    participant DB as Aurora writer
    participant OB as Outbox and Kafka

    MB->>PA: POST /v1/payments, Idempotency-Key, token reference only
    PA->>PA: stages 2 to 6, trace, authn, tenant guard, authz, rate limit
    PA->>PA: stage 7 L1 schema plus PAN detector
    PA->>IDM: stage 8 claim, INSERT ON CONFLICT DO NOTHING
    alt Key already IN_FLIGHT
        IDM-->>PA: conflict
        PA-->>MB: 409 IDEMPOTENT_REQUEST_IN_PROGRESS with Retry-After 1
    else Key already COMPLETED
        IDM-->>PA: stored snapshot
        PA-->>MB: replayed status and body, Idempotent-Replay true
    else New claim
        IDM-->>PA: claimed, lease held
    end
    PA->>CFG: stage 9 merchant context, staleness 30 s allowed
    CFG-->>PA: snapshot, fail static if control plane is down
    PA->>PA: stage 10 L5 payment validation
    PA->>PO: dispatch over gRPC
    PO->>RK: stage 11 risk evaluation
    RK-->>PO: ALLOW or FORCE_3DS or RISK_DECLINED
    PO->>RT: stage 12 build routing plan
    RT-->>PO: ordered candidates with reason annotations, plan persisted
    PO->>DB: stage 13 INSERT payment CREATED and attempt PENDING
    Note over PO,DB: gateway_idempotency_key derived from attempt_id is written before dispatch
    PO->>GA: stage 14 authorize, 8 s hard timeout, bulkhead and breaker
    GA->>GW: authorize with the derived idempotency key
    GW-->>GA: approved, auth code, amount and currency echo
    GA-->>PO: normalized outcome SUCCESS
    PO->>PO: stage 15 L6 response validation, signature schema and echo
    PO->>DB: stage 16 payment to AUTHORIZED plus outbox row, one transaction
    DB->>OB: payment.authorized.v1
    PO-->>PA: authorized
    PA->>IDM: stage 17 store response snapshot, mark COMPLETED
    PA-->>MB: 201 with payment in state AUTHORIZED
```

## Diagram B — Capture, settlement and refund

```mermaid
sequenceDiagram
    autonumber
    participant MB as Merchant backend
    participant PA as payment-api
    participant PO as payment-orchestrator
    participant GA as Gateway adapter
    participant GW as Gateway
    participant WI as webhook-ingress
    participant EC as event-consumer
    participant LG as Ledger BC-8
    participant RC as Reconciler
    participant DB as Aurora writer

    MB->>PA: POST /v1/payments/id/capture, Idempotency-Key, amount optional
    PA->>PO: capture command
    PO->>PO: L7 guard, AUTHORIZED to CAPTURED, invariant I2 captured <= authorized
    PO->>GA: capture on the same attempt and the same gateway
    GA->>GW: capture
    GW-->>GA: captured
    PO->>DB: payment to CAPTURED plus outbox, one transaction
    DB->>EC: payment.captured.v1
    EC->>LG: append ledger entries, double entry, append only
    PA-->>MB: 200 CAPTURED

    GW->>WI: settlement webhook or settlement report
    WI->>DB: persist inbound_webhooks, 50 ms budget, no processing
    DB->>EC: webhook.received.v1
    EC->>PO: apply settlement, CAPTURED to SETTLED
    PO->>DB: payment to SETTLED plus outbox
    DB->>EC: payment.settled.v1
    EC->>LG: settlement ledger entries
    Note over LG: we observe settlement, we never compute it

    MB->>PA: POST /v1/payments/id/refund
    PA->>PO: refund command
    PO->>PO: L7 guard plus invariant I1, sum of refunds <= captured_amount
    PO->>GA: refund on the settling gateway
    GA->>GW: refund
    GW-->>GA: accepted
    PO->>DB: payment to PARTIALLY_REFUNDED or REFUNDED plus outbox
    DB->>EC: payment.refunded.v1
    EC->>LG: reversing ledger entries

    RC->>GW: nightly reconciliation, compare our state to the gateway report
    GW-->>RC: statement rows
    RC->>DB: open reconciliation_exceptions on mismatch
```

## Legend and notes

- **The three-way `alt` at stage 8 is the whole idempotency contract in one place.** A concurrent
  duplicate gets `409` with `Retry-After` rather than blocking a request thread on someone else's
  lease — blocking is how thread pools die under retry storms (A6, §14.3). A completed duplicate
  replays the stored snapshot with `Idempotent-Replay: true`. A same-key-different-fingerprint
  request gets `422 IDEMPOTENCY_KEY_REUSED` (§14.2), which is the check that catches the client
  bug of reusing one key for two different payments.
- **The attempt row and its derived gateway idempotency key are written before the gateway call.**
  `gateway_idempotency_key = base32(HMAC-SHA256(attempt_id, gateway_salt))[:32]` is reproducible
  after a crash, so a transport retry to the same gateway dedupes at the gateway and the
  reconciler can look the transaction up later (A10, §14.4).
- **Capture and refund go to the *same* attempt and the *same* gateway** that authorized. They are
  not routed. Routing happens once, at authorization; a capture on a different gateway is
  meaningless.
- **Settlement is observed, not computed.** It arrives as a gateway settlement report or webhook
  and is reconciled. We take no custody of funds; the ledger is a shadow ledger for reconciliation
  (A1, A12).
- **`CAPTURED → SETTLED → REFUNDED` is the normal refund path**, not an exception. §9 explicitly
  allows `SETTLED → PARTIALLY_REFUNDED` and `SETTLED → REFUNDED` because refunding after
  settlement is what actually happens in production.
- **Invariants I1 and I2 are enforced twice** — in the aggregate method (L7) and by database
  `CHECK` constraints with serialized updates — because a bug in the application layer must still
  not be able to refund more than was captured (§9).
- **`webhook-ingress` persists and returns.** It never processes inline; its budget is 50 ms and
  everything downstream of `webhook.received.v1` is asynchronous. See diagram 11.

## Related

- [Design baseline §9 payment state machine, §12 pipeline, §14 idempotency contract](../spec/00-design-baseline.md)
- [06 — Data plane](06-data-plane.md), [09 — Gateway routing](09-gateway-routing.md), [11 — Webhook flow](11-webhook-flow.md)
- [docs/payment-flow.md](../payment-flow.md)
