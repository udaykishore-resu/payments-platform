# 05 — Validation Plane

## What this shows and why it matters

Seven validation levels, each with a stable rule identifier so a failure is traceable to a
documented rule and a remediation. The plane is a library, not a service: L1–L7 are linked into
whichever binary runs them. This diagram positions each level on the two lifecycles that matter —
the synchronous payment request path and the long-running onboarding path — because *where* a
level runs determines whether it may touch the network. L1, L4, L5, L6 and L7 are pure and total
(same input, same outcome, no panics, no I/O); L3 is explicitly impure and is therefore **never**
invoked on the payment hot path.

## Diagram A — Validation levels across the payment request lifecycle

```mermaid
flowchart LR
  REQ["Inbound POST /v1/payments"]
  L1["L1 API and schema - edge middleware, pure"]
  PAN["PAN detector - 13 to 19 digits, Luhn valid after stripping separators"]
  IDEM["Idempotency claim - stage 8"]
  CTX["Merchant context load from cached config"]
  L5["L5 payment validation - limits, currency, method, risk policy"]
  RISK["Risk engine"]
  ROUTE["Routing engine"]
  DISP["Dispatch to gateway adapter"]
  L6["L6 response validation - signature, schema, amount and currency echo"]
  L7["L7 domain and state transition"]
  OK["Commit state plus outbox, respond"]

  E1["400 VALIDATION_FAILED"]
  E2["400 SENSITIVE_DATA_IN_REQUEST plus security event, value never logged"]
  E5["422 - AMOUNT_EXCEEDS_LIMIT, CURRENCY_NOT_SUPPORTED, PAYMENT_METHOD_NOT_SUPPORTED"]
  E6["502 GATEWAY_CONTRACT_VIOLATION"]
  E7["409 INVALID_STATE_TRANSITION"]

  REQ --> L1
  L1 --> PAN
  L1 -->|"schema violation"| E1
  PAN -->|"hit"| E2
  PAN -->|"clean"| IDEM --> CTX --> L5
  L5 -->|"violation"| E5
  L5 --> RISK --> ROUTE --> DISP --> L6
  L6 -->|"violation"| E6
  L6 --> L7
  L7 -->|"illegal transition"| E7
  L7 --> OK
```

## Diagram B — Validation levels across the onboarding lifecycle

```mermaid
flowchart TB
  SUB["Merchant submission via control-plane-api"]
  L2["L2 merchant validation - identity, business profile, MCC, country, bank format"]
  L2F["422 plus onboarding case annotation, merchant to VALIDATION_FAILED"]
  KYC["Steps 2 and 3 - KYC vendor, impure, vendor is the decision maker"]
  BANKV["Step 4 - bank account validation port"]
  PROV["Step 5 - provision gateways"]
  L3["L3 gateway validation - credential probe, capability descriptor match, webhook reachability"]
  L3F["422 or mark gateway_connection unhealthy"]
  CFGW["Step 8 - configuration write through the control plane"]
  L4["L4 configuration validation - schema, policy coherence, referential, checksum"]
  L4F["422 CONFIGURATION_INVALID - nothing persisted"]
  SBOX["Step 9 sandbox validation"]
  CERT["Step 10 certification matrix per gateway, method and currency"]
  L5S["L5 and L6 exercised for real inside the certification suite"]
  GATE["Step 11 compliance-review manual gate"]
  ACT["Step 12 activate - L7 guards the transition to ACTIVE"]

  SUB --> L2
  L2 -->|"fail"| L2F
  L2 -->|"pass"| KYC --> BANKV --> PROV --> L3
  L3 -->|"fail"| L3F
  L3 -->|"pass"| CFGW --> L4
  L4 -->|"fail"| L4F
  L4 -->|"pass"| SBOX --> CERT --> L5S --> GATE --> ACT
```

## Legend and notes

- **Level identifiers are stable and documented.** A rule is named like
  `L5.AMOUNT_WITHIN_MERCHANT_LIMIT`; `TestEveryRuleIsDocumented` fails the build if a rule ID has
  no entry in `docs/validation-plane.md` (§21). That is what makes an error response actionable
  rather than merely accurate.
- **L1 carries the PAN detector, and that placement is load-bearing.** It runs at stage 7 of the
  pipeline — before idempotency, before any logging of the body, before persistence — so a
  request containing PAN-like data is rejected at `400 SENSITIVE_DATA_IN_REQUEST` with the
  offending value never written to a log. This is one of the enforcement controls that keeps the
  platform at SAQ-A/A-EP rather than SAQ-D (§17.2).
- **L3 is the only impure level.** It makes network calls (credential probes, webhook
  reachability, capability checks), so it runs in onboarding, during credential rotation, and on
  a scheduled probe — never inside a payment request. A failing L3 marks the
  `gateway_connection` unhealthy rather than failing an unrelated payment.
- **L5 takes configuration as an input, which is what keeps it pure.** The cached configuration
  snapshot is passed in; the rule does not fetch it. Same snapshot plus same payment gives the
  same outcome, which makes L5 fully unit-testable and replayable against historical config
  versions.
- **L6 is the gateway contract check, and it is not optional.** It verifies the response
  signature, the response schema, and that the amount and currency the gateway echoed match what
  we sent. A mismatch is `502 GATEWAY_CONTRACT_VIOLATION` — we refuse to record a state change
  from a response we cannot trust.
- **L7 is enforced inside aggregate methods**, not in a service layer, and is mirrored by
  database constraints (I1–I3). A transition the FSM rejects returns
  `409 INVALID_STATE_TRANSITION`; the partial unique index on successful attempts is the
  last-resort backstop that makes double-charging structurally impossible (§9).
- **Certification exercises L5 and L6 for real.** The §11.4 matrix asserts that a declined test
  card yields a mapped `DECLINED` with a normalized reason code and that amount and currency
  echo — i.e. it proves the anti-corruption layer and L6 work against the live sandbox before
  real money is at risk.

## Related

- [Design baseline §21 validation plane contract, §12 pipeline, §17.2 PCI enforcement, §11.4 certification](../spec/00-design-baseline.md)
- [06 — Data plane and the 17-stage pipeline](06-data-plane.md)
- [07 — Merchant onboarding saga](07-merchant-onboarding.md)
- [docs/validation-plane.md](../validation-plane.md)
