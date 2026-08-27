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
    PA->>PA: recover, requestid, tracing, logging, metrics
    PA->>PA: bodylimit buffers the raw bytes and runs the L1 PAN scan, then contenttype, cors, securityheaders
    PA->>PA: authn, tenant, authz, ratelimit, concurrency
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
    PA->>PA: handler decodes the body, L1 schema validation
    PA->>CFG: stage 9 merchant context, staleness 30 s allowed
    CFG-->>PA: snapshot with its gateway connections, fail static if control plane is down
    PA->>PA: stage 10 L5 payment validation
    PA->>PO: dispatch over gRPC
    PO->>RK: stage 11 risk evaluation
    RK-->>PO: ALLOW or FORCE_3DS or RISK_DECLINED
    PO->>RT: stage 12 build routing plan
    RT-->>PO: ordered candidates with reason annotations, plan persisted
    PO->>PO: breaker Allow, bulkhead Acquire, resolve the gateway credential by secret reference
    PO->>DB: T1 stage 13 - StartAttempt, bind connectionId, MarkProcessing, COMMIT
    Note over PO,DB: gateway_idempotency_key = base32 HMAC-SHA256 of attempt_id and operation, written before dispatch
    PO->>GA: T2 stage 14 - authorize under its own 8 s deadline, bulkhead and breaker
    GA->>GW: authorize with the derived idempotency key
    GW-->>GA: approved, auth code, amount and currency echo
    GA-->>PO: normalized outcome StatusAuthorized
    PO->>PO: settle classifies unknown, then transport error, then contract, then the business outcome
    PO->>PO: stage 15 L6 response validation, schema, amount and currency echo
    PO->>DB: T3 stage 16 - attempt SUCCESS, payment AUTHORIZED, audit and outbox, one transaction
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
    PO->>PO: SuccessfulAttempt names the gateway that holds the authorization, no routing, no failover
    PO->>DB: StartAttempt with operation capture, bind connectionId, commit the attempt row first
    PO->>GA: capture through the same gateway
    GA->>GW: capture
    GW-->>GA: captured
    PO->>PO: L6 response validation, then L7 MarkCaptured, invariant I2 captured <= authorized
    PO->>DB: attempt SUCCESS, payment CAPTURED, audit and outbox, one transaction
    DB->>EC: payment.captured.v1
    EC->>LG: append ledger entries, double entry, append only
    PA-->>MB: 200 CAPTURED

    GW->>WI: settlement webhook
    WI->>DB: dedup claim plus raw envelope, one transaction, 50 ms budget, no interpretation
    WI-->>GW: 202 Accepted
    DB->>EC: the asynchronous processor picks the stored delivery up
    EC->>EC: re-verify the stored payload, then MarkSettled, CAPTURED to SETTLED
    EC->>DB: payment state, ledger entries and outbox in one transaction
    DB->>EC: payment.settled.v1
    Note over LG: we observe settlement, we never compute it

    MB->>PA: POST /v1/payments/id/refund
    PA->>PO: refund command
    PO->>PO: AddRefund, L7 guard plus invariant I1, sum of refunds <= captured_amount
    PO->>DB: refund row PENDING committed BEFORE the gateway call
    PO->>GA: refund through the gateway that took the funds
    GA->>GW: refund
    GW-->>GA: accepted
    PO->>PO: refund PENDING to SUBMITTED, or SUCCEEDED directly if the gateway settled it inline
    PO->>DB: payment to PARTIALLY_REFUNDED or REFUNDED plus audit and outbox, one transaction
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
  `gateway_idempotency_key = base32(HMAC-SHA256(attempt_id ‖ 0x00 ‖ operation, salt))[:32]` is
  reproducible after a crash, so a transport retry to the same gateway dedupes at the gateway and
  the reconciler can look the transaction up later (A10, §14.4). The attempt also carries
  `connectionId`, stamped in the same pre-dispatch window, so "which credential signed this
  request" is answerable from the attempt row rather than by re-deriving it from the merchant's
  connections as they stand today (migration `0016`).
- **Capture and void go to the *same gateway* but a *new attempt row*.** `CaptureExisting` and
  `VoidExisting` both call `followUp`, which finds the successful authorization's gateway, opens a
  fresh attempt with operation `capture` or `void`, binds its `connectionId` and commits it before
  the call — the same T1-before-T2 ordering as authorization, for the same reason. There is no
  routing and no failover on either: routing happens once, at authorization.
- **The refund row is committed before the gateway is called, for the same reason the attempt is.**
  A crash mid-refund must leave a record, or a merchant has told a customer their money is coming
  back and nothing in the system knows. A refund whose outcome is *unknown* stays `PENDING` and
  enters reconciliation; it is emphatically not retried, because a duplicate refund is a duplicate
  payout.
- **The refund's own FSM is `PENDING → SUBMITTED → SUCCEEDED | FAILED`, with `PENDING → CANCELED`.**
  `SUBMITTED` is "the gateway accepted it"; `SUCCEEDED` is "the gateway confirmed it moved", which
  usually arrives later as a webhook and calls `ConfirmRefund`. Only the payment reaches
  `PARTIALLY_REFUNDED` / `REFUNDED`; the refund entity has its own states (diagram 13, Diagram D).
- **Settlement is observed, not computed.** It arrives as a gateway settlement report or webhook
  and is reconciled. We take no custody of funds; the ledger is a shadow ledger for reconciliation
  (A1, A12).
- **`CAPTURED → SETTLED → REFUNDED` is the normal refund path**, not an exception. §9 explicitly
  allows `SETTLED → PARTIALLY_REFUNDED` and `SETTLED → REFUNDED` because refunding after
  settlement is what actually happens in production.
- **Invariants I1 and I2 are enforced twice** — in the aggregate method (L7) and by database
  `CHECK` constraints with serialized updates — because a bug in the application layer must still
  not be able to refund more than was captured (§9).
- **`webhook-ingress` persists and returns `202 Accepted`** (`200 OK` for a delivery it already
  holds). It never processes inline; its budget is 50 ms and every interpretation step runs in the
  separate asynchronous processor. See diagram 11.

## Related

- [Design baseline §9 payment state machine, §12 pipeline, §14 idempotency contract](../spec/00-design-baseline.md)
- [06 — Data plane](06-data-plane.md), [09 — Gateway routing](09-gateway-routing.md), [11 — Webhook flow](11-webhook-flow.md)
- [docs/payment-flow.md](../payment-flow.md)
