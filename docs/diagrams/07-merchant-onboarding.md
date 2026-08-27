# 07 — Merchant Onboarding Saga

## What this shows and why it matters

`merchant-onboarding@v1` as it actually executes: twelve steps, each checkpointed, two of them
blocking on an external signal, and every step with an external side effect paired with a
compensation. This is the diagram that answers "what happens if provisioning succeeds but
certification fails on the third gateway three days later?" — the answer is the compensation
branch in Diagram B, which unwinds completed steps in strict reverse order so no orphaned gateway
sub-account, secret version or webhook registration is left behind. The p95 target for the
automated portion is ≤ 30 min excluding external KYC SLA (§18).

## Diagram A — Happy path

```mermaid
sequenceDiagram
    autonumber
    participant OPS as Merchant operator
    participant CP as control-plane-api
    participant WF as workflow-worker
    participant VP as Validation plane
    participant KV as KYC vendor
    participant BV as Bank validation vendor
    participant GA as Gateway adapter ACL
    participant SM as Secrets Manager and KMS
    participant CS as Certification suite
    participant CO as Compliance officer
    participant MR as Merchant Registry
    participant OB as Outbox and Kafka

    OPS->>CP: POST /v1/merchants with Idempotency-Key
    CP->>MR: create merchant in state CREATED
    MR->>OB: merchant.created.v1
    OPS->>CP: POST /v1/merchants/id/onboarding
    CP->>WF: start instance, business key merchant_id
    Note over WF: second start returns the existing instance

    WF->>VP: step 1 validate-merchant, L2, 5 s budget
    VP-->>WF: pass
    WF->>OB: merchant.validated.v1, merchant to KYC_PENDING
    WF->>KV: step 2 submit-kyc, vendor ref key, 5 retries exp
    KV-->>WF: case accepted
    WF->>WF: step 3 await-kyc-decision, signal wait up to 7 d
    KV-->>WF: decision APPROVED via signal
    WF->>OB: merchant.kyc_approved.v1
    WF->>BV: step 4 validate-bank-account
    BV-->>WF: ownership confirmed
    WF->>OB: merchant.bank_validated.v1

    WF->>GA: step 5 provision-gateways, fan out per selected gateway
    GA-->>WF: external account references
    WF->>OB: merchant.gateway_provisioned.v1
    WF->>SM: step 6 store-credentials, returns secret reference only
    SM-->>WF: secretRef, material never leaves KMS envelope
    WF->>GA: step 7 register-webhooks
    GA-->>WF: webhook registration ids
    WF->>CP: step 8 apply-configuration, L4 validated, version n
    CP->>OB: configuration.published.v1

    WF->>CS: step 9 sandbox-validation, 15 m budget
    CS-->>WF: pass
    WF->>CS: step 10 certification, full gateway method currency matrix
    CS-->>WF: signed CertificationReport stored in S3
    WF->>OB: merchant.certified.v1, connections to CERTIFIED

    WF->>CO: step 11 compliance-review, manual gate up to 5 d
    CO-->>WF: signed approval, the signal itself is audited
    WF->>MR: step 12 activate, L7 guards the transition
    MR->>OB: merchant.activated.v1
    OB-->>CP: data plane cache warms, merchant is live
```

## Diagram B — Failure and compensation branch

```mermaid
sequenceDiagram
    autonumber
    participant WF as workflow-worker
    participant CS as Certification suite
    participant CP as control-plane-api
    participant GA as Gateway adapter ACL
    participant SM as Secrets Manager and KMS
    participant MR as Merchant Registry
    participant DQ as workflow_dlq
    participant OPS as Operator

    WF->>CS: step 10 certification
    CS-->>WF: FAIL, 3DS challenge never reached REQUIRES_ACTION on adyen
    WF->>WF: retry 2 times as defined, still failing
    Note over WF: error is non-retryable, instance moves to COMPENSATING

    alt Fixable configuration, operator chooses repair
        WF->>MR: merchant to CERTIFICATION_FAILED
        Note over MR: allowed onward, CERTIFICATION or CONFIGURING or TERMINATED
        OPS->>CP: corrected configuration, version n plus 1
        CP-->>WF: resume from the step 8 checkpoint
        WF->>CS: re-run step 10 only, steps 1 to 7 are not replayed
    else Abort, unwind the saga
        WF->>CP: compensate step 8, roll back to configuration version n minus 1
        WF->>GA: compensate step 7, delete webhook registrations
        WF->>SM: compensate step 6, delete secret version
        WF->>GA: compensate step 5, de-provision gateway sub-accounts
        WF->>WF: compensate steps 3 and 2, cancel KYC case
        WF->>MR: merchant to TERMINATED, requires zero non-terminal payments
    end

    alt A compensation itself fails
        GA-->>WF: de-provision returned 500
        WF->>DQ: park step payload with the full error chain
        DQ->>OPS: alert on pp_dlq_depth
        Note over OPS: never retried blindly, a half-completed de-provision can leave a live sub-account
    end
```

## Legend and notes

- **Steps 1, 4, 9, 10 and 11 have no compensation** and so do not appear in the unwind sequence.
  A validation, a lookup, a sandbox run and a human decision leave no external side effect to
  undo (§11).
- **Compensation order is strictly reverse and only covers completed steps.** Step 8 rolls back
  before step 7 deletes webhooks, because a configuration that still points at a webhook we are
  about to delete is a worse intermediate state than the reverse.
- **`store-credentials` returns a reference, never material.** The credential is written into
  Secrets Manager under `/{env}/{tenant}/{merchant}/{gateway}` and only the `secretRef` travels
  through the workflow payload — the payload is persisted in `workflow_steps` and must never
  contain a secret (§16.1, §17.2).
- **Resume is from the checkpoint, not from the beginning.** In the "fixable" branch, correcting
  the configuration re-runs step 10 only. Steps 1–7 are not replayed, which is why a KYC case is
  never resubmitted and a gateway sub-account is never provisioned twice.
- **The compliance gate can reject, and that is a first-class state.** `COMPLIANCE_REJECTED`
  (Amendment A-01) carries the reviewer's reason code and routes back to `CONFIGURING` (fixable
  configuration such as a prohibited MCC/country combination), back to `KYC_PENDING` (fixable
  evidence), or forward to `TERMINATED`. Without it a rejection would have to lie by recording
  `CERTIFICATION_FAILED`, blaming the integration for a policy decision.
- **Certification produces an artifact, not an opinion.** The signed, immutable
  `CertificationReport` in object storage is referenced from the merchant record, and
  `PRODUCTION_READY` is unreachable without a passing report (A11, §11.4).
- **`→ TERMINATED` requires zero payments in a non-terminal state** (§8). The unwind branch
  therefore cannot complete for a merchant with an in-flight payment; it parks instead.

## Related

- [Design baseline §8 merchant lifecycle, §11 workflow definition, §11.4 certification suite](../spec/00-design-baseline.md)
- [04 — Automation plane](04-automation-plane.md), [13 — State machines](13-state-machines.md)
- [docs/onboarding.md](../onboarding.md), [docs/automation-plane.md](../automation-plane.md)
