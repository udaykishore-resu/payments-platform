# ADR-017: PCI scope minimisation — PAN never enters the platform; tokenisation at the gateway edge

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §17 (PCI DSS scope boundary), §1.2 (not a card data vault), §1.3 ambiguity A2, §12 stage 7 of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Related to ADR-016 (the PAN detector is an L1 rule), ADR-011 (adapters)

## Context

PCI DSS scope is determined by where cardholder data is stored, processed or transmitted. Every
system component in that path is in scope, along with everything connected to it that is not
adequately segmented. Scope is the single largest determinant of the platform's compliance cost,
audit burden, and change velocity.

The forces:

1. **Scope is contagious.** A service that touches PAN drags in everything it connects to unless
   segmentation is proven. In a nine-binary platform sharing a cluster, a database and a message
   bus, "adequately segmented" is expensive to establish and expensive to maintain.
2. **The cost difference is an order of magnitude.** SAQ-A / A-EP is a self-assessment
   questionnaire. SAQ-D (or a full Report on Compliance) means an annual QSA engagement,
   quarterly ASV scans, penetration testing, formal change control, file-integrity monitoring,
   documented key management with dual control and split knowledge, and evidence retention across
   the whole environment. Realistically the difference is single-digit tens of thousands of
   dollars per year versus low-to-mid six figures, plus engineering time measured in
   person-months.
3. **Scope slows everything down.** In a CDE, a routine dependency bump becomes a change-control
   artifact. Multiply by every deploy across nine services.
4. **We do not need PAN.** Baseline A1: we are not an acquirer and take no custody of funds.
   Orchestration requires a *reference* to a payment instrument, not the instrument. Gateways all
   provide tokenisation at their edge (hosted fields, SDK tokenisation) and network tokens exist
   for the cross-gateway case.
5. **But some tenants will ask for vaulting**, typically for gateway portability — a token from
   Stripe is not usable at Adyen. Saying "never" forecloses a real product need.
6. **A policy is not a control.** "Don't send us PANs" written in documentation will be violated,
   accidentally, by an integrator putting a card number in a `description` field or a metadata
   map. If it lands in our logs, we are in scope retroactively and must treat it as an incident.

What breaks if we choose wrong: the platform lands in SAQ-D, compliance cost and change friction
rise by an order of magnitude, and a single leaked PAN in a log becomes a breach notification.

## Decision

**PAN, CVV and track data never enter the platform. Tokenisation happens at the gateway edge —
gateway-hosted fields or SDK tokenisation — and our API accepts only gateway tokens, network
tokens or payment-method references. If a tenant requires vaulting, it lives in a physically and
administratively separate system reached through a port and is not part of this repository.**

Enforcement is technical, not policy (§17.2):

1. **The PAN detector is an L1 validation rule** at §12 stage 7, running over **every string
   field** of every request: 13–19 digits after stripping separators, Luhn-valid. A hit returns
   `400 SENSITIVE_DATA_IN_REQUEST`, the offending value is **not logged**, and a security event is
   raised. It runs before any logging or persistence of the request body.
2. **Structured logging uses an allowlist.** Only registered field names are serialized. There is
   no `log.Printf("%+v", req)` path; the linter forbids `%+v` and `%#v` on request types. This is
   the control that matters most, because the realistic failure is not "we store a PAN" but "a PAN
   reaches a log aggregator".
3. **`Secret[T]` wrapper** whose `String()`, `MarshalJSON()` and `Format()` all return
   `[REDACTED]`. Credentials are only ever this type, so a credential cannot be printed by
   accident.
4. **Adapters return an allowlisted `Raw map[string]string`, never the raw gateway response body**
   (ADR-011), so a gateway echoing card data cannot pull it into our storage.
5. **The optional `card-vault` service, if built, is a separate AWS account, separate VPC,
   separate cluster, dedicated HSM/KMS, its own change control and its own SAQ-D assessment** —
   and explicitly not in this repository. The orchestration platform reaches it through a port
   and receives only tokens back.
6. **Design intent: SAQ-A / A-EP for the orchestration platform**, because cardholder data
   neither traverses nor is stored by it.
7. **Detection is also a runtime control**, not only an ingress one: a sampled scanner runs the
   same Luhn/length detector over log streams and database text columns, and any hit is a Sev-2
   security incident with a defined runbook.

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **PAN never enters; tokenise at the gateway edge (chosen)** | Keeps 8 of 9 services out of the CDE, so SAQ-A/A-EP instead of SAQ-D; no card data to protect means no card data to leak, which is the only truly reliable control; engineering velocity preserved (no CDE change control on routine work); no HSM, key ceremony or dual-control key management to operate; the detector converts a policy into a runtime control | Merchants are bound to a gateway's token namespace, so switching gateways requires re-tokenisation or network tokens — a genuine product limitation; some payment flows (MOTO, certain recurring migrations) are harder without vaulting; we depend on each gateway's tokenisation UX, which we do not control and which affects merchant conversion | **Accepted** |
| **Build a card vault inside the platform** | Gateway portability: one token works everywhere, which is a real competitive feature and directly supports the routing/failover story in ADR-015; full control over the tokenisation UX; enables MOTO and card-on-file migration between processors | Puts the *entire* platform in SAQ-D scope unless segmentation is airtight — and segmentation inside one Kubernetes cluster, one database and one Kafka is exactly what a QSA scrutinises hardest. Brings HSM procurement, key ceremonies with dual control and split knowledge, quarterly ASV scans, annual penetration tests, formal change control on every deploy, and file-integrity monitoring. It also makes us a high-value breach target for data we do not need to hold. This is the option a product-minded engineer pushes for — the portability argument is genuinely strong — and it loses on the disproportion between the benefit and the scope cost, and on §1.2's explicit scope statement | Rejected for this repository; **available as a separate, separately-assessed system** if the product case is made |
| **Proxy card data through the platform to the gateway (never store, only transmit)** | Merchant integration is simpler (one API, no gateway SDK on their page); enables routing decisions based on the actual card; no vault to operate; feels like a small step | "Transmitting" cardholder data is squarely in PCI scope — SAQ-D territory — with no storage benefit to show for it. Every service on the transmission path, every log, every trace, every APM tool and every crash dump becomes a potential exposure. The temptation to log a request during debugging is constant and one incident ends the assessment. Strictly worse than either neighbour: full scope cost, none of the vault's portability benefit | Rejected |
| **Accept PAN only in a tightly segregated ingress service within the same repository/cluster** | Cheaper than a fully separate account; scope theoretically limited to one service | Segmentation must be *proven* to a QSA, and shared cluster, shared node pools, shared service mesh, shared observability pipeline and shared CI make that argument fragile; a single misconfigured network policy or a shared sidecar collapses it; the blast radius of being wrong is the whole platform's scope | Rejected — if we ever hold PAN it goes in a separate account, per §17.1 |

## Consequences

### Positive

- The orchestration platform is assessable at SAQ-A/A-EP: no CDE, no HSM, no quarterly ASV scan
  on the main platform, no CDE change control on routine deploys.
- The most reliable data-protection control is having no data. A breach of our database exposes
  tokens that are useless outside their gateway.
- Engineering velocity is preserved for the 100 % of work that is not card handling.
- The logging allowlist and `Secret[T]` are useful well beyond PCI — they prevent credential and
  PII leakage generally.

### Negative

- **Gateway token portability is limited.** A Stripe token is not usable at Adyen. This directly
  constrains failover for card-on-file and recurring payments: cross-gateway failover works for
  a fresh checkout token but not for a stored one. Network tokens mitigate this where issuers and
  gateways support them (`featureFlags.networkTokens`, §23), and the limitation must be stated
  plainly to merchants rather than papered over.
- Merchants must integrate a gateway's hosted fields or SDK, which is more integration work for
  them and gives us less control over checkout conversion.
- Some flows (MOTO, certain migrations, some recurring scenarios) are unavailable or awkward.
- The PAN detector has a false-positive surface: any 13–19 digit Luhn-valid string — some order
  IDs, some bank references — will be rejected. Merchants will hit this and be annoyed.

### Neutral / accepted costs

- We are dependent on gateways' tokenisation availability; a gateway tokenisation outage is a
  checkout outage we cannot route around.
- The detector costs a scan over request strings inside the 3 ms L1 budget. Measurable but small
  for typical payloads; payload size limits keep it bounded.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| A PAN arrives in a free-text field (description, metadata) and is stored or logged | Medium | **Critical** — retroactive scope, breach notification | L1 PAN detector over every string field before logging or persistence; value never logged on rejection; sampled scanner over logs and text columns as a second line | `SENSITIVE_DATA_IN_REQUEST` rate by merchant; scanner hits |
| Detector false positives reject legitimate requests | Medium | Medium — merchant friction | Detector applies to string fields only, with an explicit exclusion list for fields whose format is validated as non-card (e.g. IBAN-shaped, known reference formats); clear error message naming the field; merchant guidance in docs | `SENSITIVE_DATA_IN_REQUEST` rate concentrated in one merchant or one field |
| A gateway echoes card data in a response and we persist it | Medium | High | Adapters map into an allowlisted `Raw map[string]string`; the raw body is never persisted or logged; L6 response validation operates on typed fields | Contract suite assertion that `Raw` contains only allowlisted keys; scanner over `Raw` payloads |
| Debug logging added under incident pressure prints a request | Medium | **Critical** | Linter forbids `%+v`/`%#v` on request types; logging is allowlist-based so an unregistered field is dropped even if someone tries; incident runbooks state the prohibition explicitly | SAST/lint gate; log-scanner hits |
| Scope creep — a feature request to "just accept the card for this one flow" | Medium | **Critical** | This ADR and §1.2 are the answer; any such feature requires a superseding ADR and a QSA consultation *before* implementation, not after | Design review; presence of PAN-shaped fields in an OpenAPI diff |
| Token portability limitation blocks a deal | Medium | Medium — commercial, not technical | Network tokens where supported; honest positioning; the separate-vault option exists as a funded product decision | Sales feedback; count of merchants requesting cross-gateway card-on-file |
| Assessment scope challenged by a QSA despite the design | Low | High | Annual scoping validation with the QSA; network segmentation evidence and data-flow diagrams maintained as living artifacts in `docs/compliance.md` | Annual assessment outcome |

## Validation

- **Ingress test:** a request containing a Luhn-valid 16-digit number in any string field —
  including nested objects, arrays and metadata maps — returns `400 SENSITIVE_DATA_IN_REQUEST`,
  and the value appears in **no** log line, trace attribute or database row. Asserted in
  `tests/integration` with a log-capturing harness.
- **Log scanner:** a scheduled job runs the detector over a sample of log streams and over text
  columns in the database. Target: **zero hits, ever**. A single hit is a Sev-2 with a defined
  runbook.
- **Scope assertion:** the annual assessment confirms SAQ-A/A-EP eligibility for the orchestration
  platform. That outcome is the definitive validation of this ADR.
- **Data-flow evidence:** `docs/compliance.md` carries a current data-flow diagram showing no
  cardholder data crossing the platform boundary; reviewed each assessment cycle.
- **Cost check:** compliance cost stays in the SAQ-A/A-EP band. A jump toward SAQ-D cost means
  scope has moved and this ADR has been violated somewhere.

## Revisit criteria

Reopen if:

1. A funded product decision requires card vaulting for gateway portability. The decision to
   reopen is not "should we hold PAN?" but "should we build a *separate*, separately-assessed
   vault?" — this ADR's boundary survives either way.
2. PCI DSS scoping guidance changes materially (e.g. a future version alters how tokenisation and
   proxying affect scope).
3. Network tokens reach sufficient issuer and gateway coverage that cross-gateway portability is
   solved without a vault — which would *strengthen* this decision, and should be recorded.
4. A tenant's regulatory regime requires card data residency under their own control in a way that
   gateway tokenisation cannot satisfy.
