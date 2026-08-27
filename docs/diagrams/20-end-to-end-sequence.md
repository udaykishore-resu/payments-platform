# 20 — End-to-End Sequence

## What this shows and why it matters

One continuous narrative from a merchant signing up to that merchant's payments being routed,
executed, reconciled, ledgered, observed, and fed back into routing and control — the full brief's
final goal in a single trace. Every other diagram in this set is a zoom into one band of this
sequence. It is split into two diagrams only because the timescales are different by four orders
of magnitude: Diagram A spans minutes to days (onboarding through activation), Diagram B spans
milliseconds to hours (a payment, its webhook, its ledger entries, and the closed control loop
that changes how the *next* payment is routed).

## Diagram A — Merchant to activation

```mermaid
sequenceDiagram
    autonumber
    participant MO as Merchant operator
    participant CP as control-plane-api
    participant VP as Validation plane L2 to L4
    participant WF as workflow-worker
    participant KV as KYC and bank vendors
    participant GA as Gateway adapter ACL
    participant SM as Secrets Manager and KMS
    participant CS as Certification suite
    participant CO as Compliance officer
    participant MR as Merchant Registry
    participant KB as Outbox and Kafka
    participant OT as Observability and Audit

    MO->>CP: POST /v1/merchants, Idempotency-Key, tenant from the token only
    CP->>MR: merchant CREATED
    MR->>KB: merchant.created.v1
    MO->>CP: POST /onboarding
    CP->>WF: start merchant-onboarding@v1, business key merchant_id

    WF->>VP: L2 validate-merchant
    VP-->>WF: pass, merchant to KYC_PENDING
    WF->>KV: submit-kyc then await decision up to 7 d
    KV-->>WF: APPROVED
    WF->>KV: validate-bank-account
    KV-->>WF: ownership confirmed, merchant to BANK_VALIDATED

    WF->>GA: provision-gateways, fan out per selected gateway
    GA-->>WF: external account references, merchant to CONFIGURING
    WF->>SM: store-credentials, only a secretRef travels in the workflow payload
    WF->>GA: register-webhooks
    WF->>CP: apply-configuration
    CP->>VP: L4 configuration validation
    VP-->>CP: valid, version n assigned
    CP->>KB: configuration.published.v1 on the compacted topic

    WF->>CS: sandbox-validation then certification matrix
    CS->>GA: authorize capture refund void 3DS decline idempotency echo, per gateway method currency
    CS-->>WF: signed CertificationReport in S3, connections CERTIFIED
    WF->>KB: merchant.certified.v1, merchant to APPROVED

    WF->>CO: compliance-review manual gate
    CO-->>WF: approved, the signal itself is audited
    WF->>MR: activate, L7 guard, merchant to PRODUCTION_READY then ACTIVE
    MR->>KB: merchant.activated.v1
    KB-->>OT: audit chain appended, onboarding duration histogram recorded
```

## Diagram B — Payment to feedback loop

```mermaid
sequenceDiagram
    autonumber
    participant MB as Merchant backend
    participant PA as payment-api
    participant PO as payment-orchestrator
    participant RT as Routing and risk
    participant GA as Gateway adapter with breaker
    participant G1 as Gateway primary
    participant G2 as Gateway fallback
    participant WI as webhook-ingress
    participant EC as event-consumer
    participant LG as Ledger and reconciliation
    participant OT as Observability
    participant CP as Control plane

    MB->>PA: POST /v1/payments, token reference only, Idempotency-Key
    PA->>PA: stages 2 to 7, authn, tenant guard, authz, rate limit, L1 and PAN detector
    PA->>PA: stage 8 idempotency claim, Postgres authoritative
    PA->>PA: stage 9 merchant context from the cache, fail static if control is down
    PA->>PA: stage 10 L5 payment validation
    PA->>PO: dispatch
    PO->>RT: stage 11 risk then stage 12 routing
    RT-->>PO: routing plan rpl_ persisted, ordered and reason annotated
    PO->>PO: stage 13 create attempt att_1, derive the gateway idempotency key, commit
    PO->>GA: stage 14 authorize on the primary
    GA->>G1: authorize
    G1-->>GA: 503 repeatedly, breaker opens for this gateway and operation
    GA->>OT: gateway.health_changed.v1

    PO->>PO: advance the plan, create a NEW attempt att_2 with a NEW derived key
    PO->>GA: authorize on the fallback
    GA->>G2: authorize
    G2-->>GA: approved with amount and currency echo
    PO->>PO: stage 15 L6 response validation then stage 16 L7 transition plus outbox, one transaction
    PO->>EC: payment.authorized.v1
    PA-->>MB: 201 AUTHORIZED, stage 17 snapshot stored

    MB->>PA: POST /capture later
    PA->>PO: capture on the same attempt and the same gateway
    PO->>EC: payment.captured.v1
    EC->>LG: append double-entry ledger rows

    G2->>WI: signed settlement webhook
    WI->>WI: signature, replay window, dedup, then persist in 50 ms
    WI-->>G2: 200 before any interpretation
    WI->>EC: webhook.received.v1
    EC->>PO: apply settlement, L7 guard, CAPTURED to SETTLED
    PO->>EC: payment.settled.v1
    EC->>LG: settlement entries, reconciliation run compares our state to the gateway report

    EC->>OT: RED and business metrics, traces with exemplars, hash-chained audit
    OT->>OT: evaluate SLIs, authorization success rate per gateway and corridor
    OT->>CP: gateway.health_changed.v1 recorded, breaker cool-down then PROBING
    CP->>RT: routing weights and health inputs updated for the NEXT payment
    OT->>MB: error budget burn and alerting drive operator action, not silent degradation
```

## Legend and notes

- **The two diagrams share one causal chain.** `correlationid` and `causationid` on the event
  envelope make the full chain — from `merchant.created.v1` through `payment.settled.v1` —
  reconstructible from the event log alone, and `traceparent` links it to the distributed trace
  (§13.1, §22.1).
- **Diagram A step 20 is the certification matrix doing real work.** It exercises authorize,
  capture, refund, void, a mapped decline, a signature-verified webhook, a 3DS challenge, an
  idempotency replay and an amount/currency echo — per `(gateway, payment_method, currency)`. That
  is why the failover in Diagram B is safe: the ACL's decline mapping and L6 echo checking were
  proven against the sandbox before real money was at risk (§11.4).
- **Diagram B deliberately shows the failover path, not the happy path**, because the happy path is
  the failover path minus two messages. The critical detail is that the fallback attempt is a
  **new attempt with a new derived key**, so it is a genuinely new authorization rather than a
  retry that might double-charge (A10, §14.4).
- **The 201 goes out at step 22, before settlement exists.** Authorization, capture and settlement
  are three separate events at three separate timescales; the merchant is told about each as it
  happens rather than being blocked on the slowest.
- **`webhook-ingress` returns 200 before interpretation** (step 30). Everything downstream is
  asynchronous, which is what keeps a slow ledger from turning into a gateway redelivery storm.
- **The last four steps are the closed loop and the reason "Observability" is a plane.** Observed
  gateway behaviour becomes `gateway.health_changed.v1`, which changes the candidate set and the
  scoring weights for the *next* payment, and is recorded by the control plane for operator
  visibility. Nothing here is a dashboard-only signal (§10, §22).
- **Where this sequence can stop, it stops loudly.** `503 NO_ELIGIBLE_GATEWAY` when the candidate
  set empties, `422 RISK_DECLINED` at step 7, `502 GATEWAY_CONTRACT_VIOLATION` at step 19, and a
  payment left in `PROCESSING` with `payment.reconciliation_required.v1` if the gateway call times
  out. There is no branch in which the platform guesses.

## Related

- [Design baseline — the whole document, in order](../spec/00-design-baseline.md)
- [07 — Merchant onboarding](07-merchant-onboarding.md), [08 — Payment flow](08-payment-flow.md), [10 — Gateway failover](10-gateway-failover.md), [11 — Webhook flow](11-webhook-flow.md), [18 — Observability architecture](18-observability-architecture.md)
- [docs/architecture.md](../architecture.md)
