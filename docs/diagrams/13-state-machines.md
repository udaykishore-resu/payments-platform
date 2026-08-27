# 13 — State Machines

## What this shows and why it matters

Two finite state machines govern everything the platform does with a merchant and with money, plus
a third small one for the unit of gateway interaction. They are drawn here exactly as the
transition tables in §8, §9 and §9.1 define them: anything not drawn is rejected with
`409 INVALID_STATE_TRANSITION`. These diagrams are normative, not illustrative — the FSMs live in
`internal/domain` (stdlib only, no I/O), are enforced inside aggregate methods at L7, and are
mirrored by database constraints so that a bug above the domain layer still cannot move money
twice.

## Diagram A — Merchant lifecycle

```mermaid
stateDiagram-v2
    [*] --> CREATED

    CREATED --> VALIDATING
    CREATED --> TERMINATED

    VALIDATING --> KYC_PENDING
    VALIDATING --> VALIDATION_FAILED
    VALIDATION_FAILED --> VALIDATING: after correction
    VALIDATION_FAILED --> TERMINATED

    KYC_PENDING --> KYC_APPROVED
    KYC_PENDING --> KYC_FAILED
    KYC_FAILED --> KYC_PENDING: resubmission
    KYC_FAILED --> TERMINATED

    KYC_APPROVED --> BANK_VALIDATED
    KYC_APPROVED --> BANK_VALIDATION_FAILED
    BANK_VALIDATION_FAILED --> KYC_APPROVED: retry with a new account
    BANK_VALIDATION_FAILED --> TERMINATED

    BANK_VALIDATED --> GATEWAY_PROVISIONING
    GATEWAY_PROVISIONING --> CONFIGURING
    GATEWAY_PROVISIONING --> PROVISIONING_FAILED
    PROVISIONING_FAILED --> GATEWAY_PROVISIONING
    PROVISIONING_FAILED --> TERMINATED

    CONFIGURING --> SANDBOX_VALIDATION
    CONFIGURING --> CONFIGURATION_FAILED
    CONFIGURATION_FAILED --> CONFIGURING
    CONFIGURATION_FAILED --> TERMINATED

    SANDBOX_VALIDATION --> CERTIFICATION
    SANDBOX_VALIDATION --> CONFIGURATION_FAILED

    CERTIFICATION --> APPROVED
    CERTIFICATION --> CERTIFICATION_FAILED
    CERTIFICATION --> COMPLIANCE_REJECTED
    CERTIFICATION_FAILED --> CERTIFICATION
    CERTIFICATION_FAILED --> CONFIGURING
    CERTIFICATION_FAILED --> TERMINATED

    COMPLIANCE_REJECTED --> CONFIGURING: fixable configuration
    COMPLIANCE_REJECTED --> KYC_PENDING: fixable evidence
    COMPLIANCE_REJECTED --> TERMINATED

    APPROVED --> PRODUCTION_READY
    APPROVED --> SUSPENDED
    PRODUCTION_READY --> ACTIVE
    PRODUCTION_READY --> SUSPENDED
    ACTIVE --> SUSPENDED
    ACTIVE --> TERMINATED
    SUSPENDED --> ACTIVE
    SUSPENDED --> TERMINATED
    TERMINATED --> [*]
```

## Diagram B — Payment lifecycle

```mermaid
stateDiagram-v2
    [*] --> CREATED

    CREATED --> PROCESSING: orchestrator dispatch
    CREATED --> REQUIRES_ACTION: 3DS or redirect required
    CREATED --> FAILED: pre-flight rejection
    CREATED --> CANCELED

    REQUIRES_ACTION --> PROCESSING: customer completes the challenge
    REQUIRES_ACTION --> FAILED
    REQUIRES_ACTION --> CANCELED
    REQUIRES_ACTION --> EXPIRED: customer abandons

    PROCESSING --> AUTHORIZED
    PROCESSING --> CAPTURED: sale or auto-capture methods
    PROCESSING --> PENDING: async method or unknown attempt
    PROCESSING --> FAILED
    PROCESSING --> REQUIRES_ACTION: gateway steps up

    PENDING --> AUTHORIZED
    PENDING --> CAPTURED
    PENDING --> FAILED
    PENDING --> EXPIRED

    AUTHORIZED --> CAPTURED
    AUTHORIZED --> VOIDED
    AUTHORIZED --> EXPIRED: authorization expiry
    AUTHORIZED --> FAILED

    CAPTURED --> SETTLED: settlement report
    CAPTURED --> PARTIALLY_REFUNDED
    CAPTURED --> REFUNDED
    CAPTURED --> DISPUTED

    SETTLED --> PARTIALLY_REFUNDED: the normal refund path
    SETTLED --> REFUNDED
    SETTLED --> DISPUTED

    PARTIALLY_REFUNDED --> PARTIALLY_REFUNDED: further refunds up to captured
    PARTIALLY_REFUNDED --> REFUNDED
    PARTIALLY_REFUNDED --> DISPUTED

    REFUNDED --> DISPUTED

    DISPUTED --> REFUNDED: dispute lost, funds reversed
    DISPUTED --> CAPTURED: dispute won
    DISPUTED --> SETTLED: dispute won after settlement

    FAILED --> [*]
    CANCELED --> [*]
    VOIDED --> [*]
    EXPIRED --> [*]
```

## Diagram C — Payment attempt outcome

```mermaid
stateDiagram-v2
    [*] --> PENDING
    PENDING --> DISPATCHED: attempt row committed, then the gateway is called
    DISPATCHED --> SUCCESS: authorized or captured
    DISPATCHED --> DECLINED: the gateway definitively said no
    DISPATCHED --> ERROR: our side or transport failed before the gateway could act
    DISPATCHED --> TIMEOUT_UNKNOWN: no response inside the 8 s hard timeout

    SUCCESS --> [*]
    DECLINED --> [*]
    ERROR --> [*]
    TIMEOUT_UNKNOWN --> [*]

    note right of TIMEOUT_UNKNOWN
        Never retried automatically.
        Enters the reconciliation queue.
        The payment stays PROCESSING.
    end note

    note right of DECLINED
        Failover only if the normalized reason
        is in the retryable decline set.
        A hard decline is terminal.
    end note
```

## Legend and notes

- **Everything absent is forbidden.** The explicitly invalid transitions worth naming are
  `SETTLED → PROCESSING`, `REFUNDED → CAPTURED`, `CAPTURED → AUTHORIZED`, `FAILED → anything`,
  `CREATED → CAPTURED` (it must pass through `PROCESSING`), and any transition that would make
  `refunded_total > captured_total` (§9).
- **`COMPLIANCE_REJECTED` and `APPROVED → SUSPENDED` are Amendment A-01.** The original lifecycle
  had no exit from the manual compliance gate other than approval, so a compliance officer's
  rejection was unrepresentable — the workflow would have had to lie by recording
  `CERTIFICATION_FAILED` (blaming the integration for a policy decision) or hang. Likewise an
  adverse finding between approval and activation must be expressible without terminating the
  merchant (§8).
- **`PENDING` and `TIMEOUT_UNKNOWN` exist so that a timeout does not force a lie.** Without them,
  an ambiguous gateway outcome would have to be recorded as either success or failure, and both
  are potentially false. `PENDING` also carries genuinely asynchronous methods such as bank debits
  and vouchers (§9, §9.1).
- **Guards on `→ ACTIVE`**: at least one `GatewayConnection` in `CERTIFIED`, a non-empty validated
  `MerchantConfiguration`, a completed compliance attestation, and no open critical reconciliation
  exception. `→ TERMINATED` requires zero payments in a non-terminal state (§8).
- **`SUSPENDED` rejects new payments but permits refunds, voids and webhook processing.** You must
  always be able to give money back, and suspension is available to an operator *and* to the
  automation plane on a risk breach, compliance expiry or gateway de-provisioning (§8).
- **`DISPUTED` is not terminal in either direction.** A dispute lost reverses funds
  (`→ REFUNDED`); a dispute won restores them (`→ CAPTURED` or `→ SETTLED`, depending on where the
  payment was when the dispute landed).
- **Invariants enforced alongside the FSM**: I1 `sum(refunds) ≤ captured_amount`, I2
  `captured ≤ authorized`, I3 at most one attempt per payment in a successful terminal state
  (partition-aligned partial unique index, Amendment A-02), I4 amount/currency/merchant/tenant
  immutable after creation, I5 one event-log row per state change with a monotonic aggregate
  version.

## Related

- [Design baseline §8 merchant lifecycle, §9 payment state machine, §9.1 attempt outcomes](../spec/00-design-baseline.md)
- [07 — Merchant onboarding](07-merchant-onboarding.md), [08 — Payment flow](08-payment-flow.md), [10 — Gateway failover](10-gateway-failover.md)
- [docs/state-machines.md](../state-machines.md)
