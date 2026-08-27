# 01 — Business Requirements

> Purpose: state *why* the platform exists, *who* it serves, and *what business outcomes* it must produce, as numbered, testable requirements `BR-01..BR-38`. Derived from and subordinate to [`00-design-baseline.md`](./00-design-baseline.md); where this file and the baseline disagree, the baseline wins and this file is a defect.

---

## 1. Problem statement

Getting a merchant from "signed contract" to "first live authorization" is, in most payment
businesses, a 4–12 week manual process: spreadsheets of KYB documents, e-mail threads with a
gateway's implementation team, hand-edited configuration in three environments, and a
certification step whose evidence is a screenshot in a ticket. The cost is roughly
€2,500–€6,000 of human effort per merchant, and it does not amortise — merchant number 5,000
costs the same as merchant number 5.

Once live, the merchant is usually pinned to a single gateway. When that gateway degrades
(issuer outage, regional acquirer incident, expired credentials, a bad deploy on their side),
the merchant's authorization rate drops and nobody notices until the finance team reconciles at
month-end. Industry-observed single-gateway authorization rates sit around 84–89 % for
card-not-present; a competent multi-gateway failover recovers 1.5–4 percentage points of that,
which at €100 M annual GMV is €1.5–4 M of recovered revenue.

The two failure modes are structurally the same problem: **the configuration and the execution
of payment routing are treated as human work rather than as a versioned, validated, auditable
system**. This platform makes both machine work.

### 1.1 What the market already does, and where it is thin

| Segment | Example posture | Gap this platform targets |
|---|---|---|
| Single-gateway direct integration | Merchant integrates Stripe or Adyen directly | No failover, no portability, gateway pricing power is absolute |
| Gateway-owned orchestration | Adyen/Stripe "unified" products | Structurally conflicted: the orchestrator prefers itself |
| Independent orchestrators | Gr4vy, Spreedly, Primer | Strong on routing; weak on *onboarding automation*, weak on tenant-level (PSP/ISV) multi-tenancy |
| PSP-internal build | Home-grown ops tooling | Correct domain understanding, chronically under-invested; onboarding is manual |

The wedge is the **combination**: automated, resumable, auditable onboarding *plus* vendor-neutral
orchestration, delivered **multi-tenant** so that a PSP, marketplace or ISV can resell it to its
own merchants without operating a payments engineering team.

### 1.2 The two products, one codebase

| | Onboarding | Orchestration |
|---|---|---|
| Buyer | Operations / compliance leadership | Payments / engineering leadership |
| Unit of value | One merchant activated | One authorization executed |
| Dominant metric | Time-to-first-payment, cost per merchant | Authorization rate, availability |
| Load profile | Low volume, long-running, human-gated | High volume, sub-second, fully automated |
| Plane | Control + Automation | Data |
| Failure tolerance | Delay is acceptable; wrong state is not | Delay is revenue loss; double-charging is existential |

They share a codebase because they share the *same source of truth* — the merchant, its
configuration, its gateway connections and its certification evidence. Splitting them into two
products with a synchronising integration would reintroduce, as a distributed-systems problem,
exactly the configuration-drift problem the platform exists to eliminate. They are separately
deployable (§5 of the baseline) so that they scale and fail independently.

---

## 2. Personas

| Persona | Employed by | Primary jobs-to-be-done | Success looks like | Principal failure they fear |
|---|---|---|---|---|
| **Platform Operator** | Us | Run the platform for all tenants; onboard tenants; manage the gateway registry; respond to incidents; enforce error-budget policy | Zero manual work per merchant; tenant self-service deflection > 90 % | A double charge attributable to platform logic |
| **Tenant Admin** | PSP / marketplace / ISV | Onboard *their* merchants; set default routing and risk policy; monitor authorization rate across their book; export billing data | Merchant activated same-day without contacting us | Losing merchants to a competitor because activation took three weeks |
| **Merchant Admin** | The merchant | Submit business + bank details; configure payment methods, currencies, refund window, webhook endpoints; view payments; issue refunds | First live payment on day one; refunds work | A customer they cannot refund |
| **Compliance Officer** | Tenant or us | Review KYC/KYB evidence; approve the manual gate; produce audit evidence on demand; enforce residency and sanctions policy | Every activation has a complete, immutable evidence chain | An activation with no reviewable evidence trail |
| **SRE** | Us | Keep the data plane inside its SLO; run DR drills; triage gateway degradation; manage capacity | Alerts are actionable; toil is near zero | Silent correctness failure that no alert catches |
| **Gateway Partner** | Stripe / Adyen / PayPal | Receive well-formed, correctly-idempotent traffic; not be used for card testing | Low dispute and retry-abuse rate from our BIN traffic | Being de-registered as an accessory to card testing |

Persona ↔ surface mapping: Platform Operator and SRE use `platformctl` plus the operator console;
Tenant Admin and Merchant Admin use the control-plane API and the admin web surface; Compliance
Officer uses the audit/evidence surface and the manual-gate signal endpoint
(`POST /v1/merchants/{merchantId}/onboarding/signals/{signal}`); Gateway Partner is an integration
counterparty with no UI, whose expectations are encoded in the adapter contract suite.

---

## 3. Value hypotheses

Each is falsifiable and has a kill criterion. If a hypothesis is falsified, the associated
requirements are re-scoped rather than defended.

| # | Hypothesis | Instrument | Kill criterion |
|---|---|---|---|
| VH-1 | Automating onboarding reduces cost per activated merchant by ≥ 80 % | Time-tracked operator minutes per activation | < 50 % reduction after 100 activations |
| VH-2 | Multi-gateway failover lifts authorization rate by ≥ 1.5 pp | A/B: failover-enabled vs. pinned cohorts, matched on BIN mix and ticket size | < 0.5 pp uplift over 1 M matched payments |
| VH-3 | Machine-checked certification eliminates go-live defects | Count of P1 incidents in the first 30 days post-activation | ≥ 1 P1 per 20 activations traceable to a config/integration defect |
| VH-4 | Tenants will self-serve merchant configuration if the API is safe (validated, versioned, rollback-able) | Ratio of config changes made via API vs. support ticket | < 70 % self-serve after 6 months |
| VH-5 | Vendor-neutrality is worth paying for | Renewal/expansion rate among tenants with ≥ 2 live gateways vs. 1 | No differential |
| VH-6 | A shadow ledger plus reconciliation removes month-end finance toil | Hours spent on payment reconciliation per tenant per month | < 50 % reduction |

---

## 4. Business capabilities

Capability map, with the owning bounded context from baseline §3. This is the decomposition the
BRs are indexed against.

| Capability | Sub-capabilities | Owner |
|---|---|---|
| Tenant lifecycle | Tenant registration, tier assignment, API client issuance, residency declaration | BC-1 |
| Merchant lifecycle | Registration, business profile, bank accounts, suspension, termination | BC-2 |
| Onboarding automation | Durable workflow, KYC/KYB, bank validation, provisioning, compensation, manual gates | BC-3 |
| Gateway management | Registry, capability descriptors, connections, credentials, health, rotation | BC-4 |
| Configuration & policy | Payment methods, currencies, countries, routing, limits, risk, webhooks, settlement, flags, versioning, rollback | BC-5 |
| Payment execution | Create, authorize, capture, refund, void, idempotency, routing, failover | BC-6 |
| Async ingestion | Webhook receipt, signature verification, dedup, translation to domain events | BC-7 |
| Financial record | Shadow ledger, settlement ingestion, reconciliation, exception management, disputes | BC-8 |
| Evidence & assurance | Audit trail, certification reports, compliance reporting, retention | BC-9 |

---

## 5. Out of scope

Restating the baseline's non-goals is not the point; these are the *business* exclusions that
sales, pricing and roadmap must respect.

| Excluded | Business reason | Consequence accepted |
|---|---|---|
| Holding merchant funds (baseline A1) | E-money licensing, safeguarding, client-money audit — a different company | We cannot monetise float or offer instant payouts |
| Storing PAN (baseline A2) | SAQ-D would apply to all nine services; assessment and control cost is 6–7 figures annually | Merchants must use gateway-hosted fields or an external vault |
| Making the KYC decision | Regulated activity with vendor liability | Vendor SLA becomes our critical path; we own the *workflow*, not the *verdict* |
| Fraud scoring model | Different data, different team, different feedback loop | We expose a risk *policy* engine and a scorer port |
| Chargeback representment authoring | Human, jurisdictional, evidence-heavy | We record and route disputes; we do not fight them |
| Merchant-facing checkout UI/SDK | Would drag PAN into our scope | We integrate the gateways' hosted fields |
| Cross-border FX / multi-currency pricing | Requires FX licensing and rate risk management | Gateway-settled currency only; `supportedCurrencies` is a filter, not a conversion |
| Active/active money movement (baseline A9) | Conflict resolution on financial state is not worth the risk at this scale | Region failover has an RTO of 15 min |
| Alternative-rail orchestration (open banking, crypto, RTP) | Not the wedge; adapter port makes it additive later | Card and card-adjacent wallets only in v1 |

---

## 6. Commercial constraints

| # | Constraint | Implication on requirements |
|---|---|---|
| CC-1 | Pricing is per-transaction (basis-points-free, flat fee per authorization attempt) plus a per-activated-merchant onboarding fee | The platform must **meter** attempts and activations accurately enough to bill on (BR-36); metering errors are revenue errors |
| CC-2 | Gross margin target ≥ 80 % at ≥ 100 M payments/month | Unit infrastructure cost per 1,000 payments is a first-class NFR (see `03-non-functional-requirements.md`, NFR-46) |
| CC-3 | We hold no gateway contract on the merchant's behalf; the merchant or tenant contracts directly | Credentials belong to the merchant; we are a custodian, not a principal (BR-11) |
| CC-4 | Gateway partner agreements forbid retry patterns that resemble card testing | Hard-decline no-failover (BR-23) is contractual, not merely prudent |
| CC-5 | Enterprise tenants require contractual data isolation | The siloed tenancy tier must exist from day one (baseline §16), priced as a premium tier |
| CC-6 | EU tenants require EU-resident personal data | Residency is a tenant-level declaration that constrains routing (BR-35) |
| CC-7 | Support is tiered; Tier-1 must not require engineering | Every operator action must exist as an audited API/CLI operation, not a database edit |
| CC-8 | 12-month runway to prove VH-1 and VH-2 | MUST-priority requirements define the fundable milestone; SHOULD/COULD are explicitly deferrable |

---

## 7. Success metrics (money-denominated)

Targets are for the end of the first commercial year unless stated. Each has a defined
measurement source so it cannot be argued about.

| Metric | Definition | Baseline (manual industry norm) | Target | Source |
|---|---|---|---|---|
| **Time-to-first-payment (TTFP)** | Wall-clock from `merchant.created.v1` to the first `payment.authorized.v1` for that merchant | 4–12 weeks | p50 ≤ 2 business days; p95 ≤ 7 business days | Event-time difference, computed in the analytics projection |
| **Automated onboarding duration** | Sum of workflow step durations excluding human/vendor wait states | — | p95 ≤ 30 min (baseline §18) | `pp_onboarding_duration_seconds` |
| **Human touch rate** | Fraction of activations requiring any operator action beyond the compliance gate | ~100 % | ≤ 10 % | Audit records with actor type `HUMAN` per case |
| **Onboarding cost per merchant** | (Operator minutes × loaded rate) + attributable infra | €2,500–€6,000 | ≤ €400 | Time tracking + cost allocation |
| **Authorization uplift from failover** | Auth rate of failover-enabled cohort minus matched pinned cohort | 0 pp | ≥ +1.5 pp | `pp_payments_total{outcome}` split by cohort |
| **Recovered revenue from failover** | Uplift × cohort GMV | €0 | ≥ €1.5 M per €100 M GMV | Analytics projection |
| **Double-charge incidents** | Payments with > 1 attempt in a successful terminal state | Rare but non-zero | **0**, structurally (invariant I3) | Continuous query + reconciliation exceptions |
| **Operational toil** | SRE hours per month on repeatable manual work | — | ≤ 5 % of SRE capacity | Toil ledger + incident review |
| **Unresolved reconciliation exceptions** | Open critical exceptions older than 24 h | — | 0 | `pp_reconciliation_exceptions{severity="critical"}` |
| **Config self-service ratio** | Config changes via API ÷ total config changes | ~0 % | ≥ 90 % | Audit records by actor type |
| **Support cost per merchant per month** | Tier-1 + Tier-2 minutes × loaded rate | — | ≤ €6 | Ticketing export |
| **Gross margin** | (Revenue − attributable infra − support) ÷ revenue | — | ≥ 80 % at ≥ 100 M payments/month | Finance, using the cost model in NFR-46 |

---

## 8. Business requirements

Format for every requirement:

- **Statement** — what must be true, in business terms.
- **Rationale** — why; the cost of not doing it.
- **Acceptance criteria** — machine-checkable where possible; each becomes ≥ 1 test.
- **Priority** — MUST (fundable milestone), SHOULD (first commercial year), COULD (opportunistic).
- **Owner** — the bounded context accountable, per baseline §3.

Traceability: each BR is realised by one or more `FR-nn` in
[`02-functional-requirements.md`](./02-functional-requirements.md) and constrained by one or more
`NFR-nn` in [`03-non-functional-requirements.md`](./03-non-functional-requirements.md).

---

### BR-01 — Tenant registration and tier assignment

**Statement.** A platform operator can register a tenant with a declared isolation tier (`POOLED`
or `SILOED`), a declared data-residency region, and a commercial plan, and the platform enforces
those declarations for the tenant's entire lifetime.

**Rationale.** Tenancy tier and residency are not settings — they determine which database, which
KMS key, which topics and which regions the tenant's data may touch (baseline §16.1, §17.3). If
they are mutable after data exists, every migration becomes a compliance event. Fixing them at
registration is cheaper than a data-residency incident.

**Acceptance criteria.**
1. A tenant record cannot be created without `tier`, `residency_region` and `plan`.
2. Changing `tier` or `residency_region` on an existing tenant is rejected; a documented
   migration procedure (export/re-import under a new tenant) is the only path.
3. A `SILOED` tenant's rows are provably absent from the pooled schema (integration test).
4. Every tenant-scoped table row carries `tenant_id`; a row with a NULL `tenant_id` cannot be
   inserted (`NOT NULL` + RLS).

**Priority.** MUST · **Owner.** BC-1 Tenant & Identity

---

### BR-02 — API client credentials with least-privilege scopes

**Statement.** Each tenant can issue, list, rotate and revoke API clients, each bound to an
explicit scope set drawn from the published scope vocabulary (`merchants:write`, `payments:refund`,
`credentials:rotate`, …), and optionally to a merchant subset and an IP allowlist.

**Rationale.** A single all-powerful key per tenant means a leaked key can move money for every
merchant in the book. Scoped clients turn a credential leak from an existential event into a
bounded one, and let a tenant give its own merchant-facing portal a key that can read payments
but not rotate gateway credentials.

**Acceptance criteria.**
1. A request bearing a token without the endpoint's required scope receives `403 FORBIDDEN` and
   an audit record.
2. Client rotation supports an overlap window in which both the old and new secret authenticate,
   after which the old secret is revoked.
3. Revocation takes effect within 60 s across every region.
4. Scope grants and revocations are audited with actor, timestamp and prior value.

**Priority.** MUST · **Owner.** BC-1

---

### BR-03 — Merchant registration under a tenant

**Statement.** A tenant admin can register a merchant with a legal entity name, registration
number, country, MCC, contact principals and a merchant-scoped external reference, receiving a
`mrc_`-prefixed identifier unique within the tenant.

**Rationale.** The merchant record is the anchor for every downstream artifact — case,
configuration, connections, payments, audit. It must exist before anything else, and its identity
must be stable and non-guessable so that identifiers can appear in logs and support tickets
without leaking enumeration.

**Acceptance criteria.**
1. `POST /v1/merchants` with a valid body returns `201` with a `mrc_` ULID and state `CREATED`.
2. The tenant is taken from the authenticated principal; a `tenantId` in the body that disagrees
   yields `403 TENANT_MISMATCH` plus a security event (baseline §16.2).
3. Re-posting with the same `Idempotency-Key` and identical fingerprint replays the original
   `201` with `Idempotent-Replay: true`.
4. `merchant.created.v1` is published in the same transaction as the row insert (outbox).
5. A merchant's `external_reference` is unique within a tenant.

**Priority.** MUST · **Owner.** BC-2 Merchant Registry

---

### BR-04 — Structured onboarding submission

**Statement.** A merchant admin (or a tenant admin acting for them) can submit the complete
onboarding package — business profile, beneficial ownership, bank account(s), requested payment
methods, currencies, countries and gateway preferences — as a single validated document, and
receive either an accepted onboarding case or a field-level list of what is wrong.

**Rationale.** The dominant cause of multi-week onboarding is round-trip latency on incomplete
submissions. Field-level, machine-generated rejection converts a five-day e-mail loop into a
five-second API response. This is where most of the TTFP target is won.

**Acceptance criteria.**
1. Submission runs validation level L2 (baseline §21) and returns every failing rule ID and
   remediation string in one `422` response, not the first failure only.
2. A valid submission transitions the merchant `CREATED → VALIDATING` and starts exactly one
   `merchant-onboarding@v1` instance keyed on `merchant_id`.
3. Submitting twice for the same merchant returns the existing case, not a second one.
4. A `VALIDATION_FAILED` case can be corrected and resubmitted without creating a new merchant.

**Priority.** MUST · **Owner.** BC-3 Onboarding

---

### BR-05 — KYC/KYB verification through a vendor port

**Statement.** The platform submits identity and business verification to a configured KYC/KYB
provider, tracks the case asynchronously (including provider-requested additional documents), and
records the decision and its evidence immutably.

**Rationale.** We do not make the decision (baseline §1.2) but we are accountable for the
*evidence chain*. AML retention is ≥ 5 years, immutable (baseline §17.3). Owning the workflow and
not the verdict keeps the regulated activity with the vendor while keeping the auditable record
with us.

**Acceptance criteria.**
1. Submission is idempotent on a vendor reference key; a retry never creates a second vendor case.
2. The decision arrives as a signal (webhook or poll) and drives `KYC_PENDING → KYC_APPROVED |
   KYC_FAILED`; a 7-day timeout escalates rather than silently failing.
3. Evidence artifacts are stored in object storage with Object Lock and referenced from the case.
4. A `KYC_FAILED` merchant can resubmit (`KYC_FAILED → KYC_PENDING`) with a new document set.
5. Vendor outage does not fail the case: the step retries with exponential backoff to a 7-day
   ceiling and the case remains resumable.

**Priority.** MUST · **Owner.** BC-3

---

### BR-06 — Bank account validation

**Statement.** Each settlement bank account is validated for format (IBAN/BBAN/routing+account
checksums), for country consistency with the merchant's registered country, and — where the
provider supports it — for account ownership, before the merchant can reach `BANK_VALIDATED`.

**Rationale.** A wrong settlement account is discovered at first settlement, weeks later, and is
expensive and slow to correct. Validating at onboarding is a two-second check that prevents a
two-week remediation.

**Acceptance criteria.**
1. Structural validation (checksum, length, country) is a pure L2 rule and runs offline.
2. Ownership verification (where available) is an L3 rule and never runs on the payment path.
3. Failure yields `BANK_VALIDATION_FAILED`, from which a *new* account may be supplied
   (`BANK_VALIDATION_FAILED → KYC_APPROVED`) without redoing KYC.
4. Account numbers are stored encrypted, are `Secret[T]`-wrapped in memory, and are masked to the
   last four characters in every read API and log line.

**Priority.** MUST · **Owner.** BC-2 / BC-3

---

### BR-07 — Payment-method configuration

**Statement.** A merchant's enabled payment methods are explicit configuration, validated against
the intersection of what each selected gateway's capability descriptor supports and what the
merchant's country/MCC permits.

**Rationale.** "Apple Pay is enabled but the gateway connection was never provisioned for it" is a
classic day-one production failure. Deriving the legal set from capability descriptors makes the
impossible configuration unrepresentable rather than merely discouraged.

**Acceptance criteria.**
1. Enabling a method not supported by any eligible gateway is rejected at L4 with
   `CONFIGURATION_INVALID` and the offending method named.
2. A payment for a method not in the merchant's configuration is rejected at L5 with
   `PAYMENT_METHOD_NOT_SUPPORTED` before any gateway call.
3. Certification (BR-17) covers every `(gateway, payment_method, currency)` triple in the
   configuration.

**Priority.** MUST · **Owner.** BC-5 Configuration & Policy

---

### BR-08 — Currency and country configuration

**Statement.** A merchant declares its supported settlement currencies and selling countries; the
platform enforces both on the payment path and uses them as routing eligibility inputs.

**Rationale.** Currency support is a per-gateway, per-corridor property; countries drive both
gateway eligibility and sanctions screening. Making them configuration rather than implicit
behaviour means the merchant can be told *why* a payment was refused.

**Acceptance criteria.**
1. Currency codes are ISO 4217 with the correct minor-unit exponent (baseline §7).
2. A payment in an unconfigured currency fails L5 with `CURRENCY_NOT_SUPPORTED`.
3. A country in `risk.blockedCountries` is refused with `RISK_DECLINED` and audited.
4. Adding a currency requires a passing certification run for that currency before it becomes
   effective in production.

**Priority.** MUST · **Owner.** BC-5

---

### BR-09 — Multi-gateway support per merchant

**Statement.** A merchant can hold two or more concurrent, independently certified gateway
connections and process live traffic across them under one routing policy.

**Rationale.** This is the mechanism behind VH-2 and the entire orchestration value proposition. A
merchant with one gateway has no failover, no negotiating leverage and no migration path.

**Acceptance criteria.**
1. Two connections for the same merchant can both be `CERTIFIED` and both receive live traffic.
2. Each connection has its own credentials, webhook registration, health record and certification
   report.
3. Removing a gateway from the routing policy does not delete the connection; historical payments
   remain resolvable against it.
4. A merchant with ≥ 2 healthy connections experiences no payment failure when one connection's
   circuit is open (chaos test).

**Priority.** MUST · **Owner.** BC-4 Gateway Registry & Integration

---

### BR-10 — Automated gateway provisioning

**Statement.** For each selected gateway, the platform creates the merchant's gateway-side account
or sub-account, stores the resulting credentials, and registers webhooks — without human action —
and can de-provision cleanly if onboarding is aborted.

**Rationale.** Provisioning is the step most often done by e-mailing an implementation manager. It
is also the step whose partial failure creates orphaned gateway accounts that nobody reconciles.
Automation plus compensation makes partial failure self-healing.

**Acceptance criteria.**
1. Provisioning fans out per selected gateway and is idempotent on the external reference.
2. Aborting the case runs `de-provision sub-account`, `delete secret version` and
   `delete webhook registration` in strict reverse order (baseline §11).
3. A provisioning failure on one gateway does not roll back a successful provisioning on another
   unless the case itself is aborted.
4. Orphan detection: a scheduled reconciliation compares desired connections to gateway-side
   accounts and raises an exception on divergence.

**Priority.** MUST · **Owner.** BC-3 / BC-4

---

### BR-11 — Gateway credential custody

**Statement.** Gateway credentials are held in a dedicated secrets store under a per-tenant path,
are never returned by any read API, never appear in logs or error messages, and are referenced by
the platform only through an opaque `secretRef`.

**Rationale.** CC-3: the credentials are the merchant's, not ours; we are a custodian. A leaked
gateway secret allows an attacker to move the merchant's money directly, bypassing every control
we own. This is the single highest-impact secret in the system.

**Acceptance criteria.**
1. Credential material is only ever held in the `Secret[T]` wrapper whose `String()`,
   `MarshalJSON()` and `Format()` return `[REDACTED]` (baseline §17.2).
2. No API surface — including error details and admin exports — can return credential material;
   a contract test asserts this over the whole OpenAPI surface.
3. Access is IAM-scoped by path prefix per `(env, tenant, merchant, gateway)`; a service can read
   only the prefixes its role permits.
4. Every credential read is audited with principal, purpose and `attempt_id` where applicable.

**Priority.** MUST · **Owner.** BC-4

---

### BR-12 — Credential rotation with dual-run overlap

**Statement.** Gateway API credentials rotate on a schedule of ≤ 90 days, and on demand, with an
overlap window during which both the old and the new credential are valid, and with automatic
rollback if the new credential fails validation.

**Rationale.** Rotation without overlap is a scheduled outage: in-flight requests carrying the old
credential fail at the instant of cutover. Overlap turns a step change into a ramp. Automating
rotation is also what makes a 90-day policy survivable at 50,000 merchants — 50,000 manual
rotations per quarter is not a policy, it is a fiction.

**Acceptance criteria.**
1. Rotation is a durable workflow: create new credential → validate (L3) → publish new
   `secretRef` → drain overlap window → revoke old → verify revocation.
2. During the overlap window, payments succeed continuously (zero-error chaos test).
3. If L3 validation of the new credential fails, the old credential remains active and the
   rotation is marked failed with an alert — never a partial cutover.
4. Credential age is exported as a metric; age > 90 days raises a compliance finding.

**Priority.** MUST · **Owner.** BC-4

---

### BR-13 — Routing configuration

**Statement.** A merchant's routing policy is declarative configuration — a strategy, a primary, an
ordered fallback list, conditional rules keyed on payment attributes, and scoring weights over
health, success rate, cost and latency — validated before publication and versioned.

**Rationale.** Routing is where authorization uplift is created and where a bad change destroys
revenue silently. Declarative + validated + versioned + rollback-able is the only form in which a
tenant can be trusted to change it themselves (VH-4).

**Acceptance criteria.**
1. L4 validation rejects a policy naming a gateway with no `CERTIFIED` connection, a rule whose
   condition can never match the merchant's configured currencies/methods, or weights not summing
   to 1.0.
2. Every payment persists its `RoutingPlan` — the ordered candidate list with a reason annotation
   per candidate — for audit and post-hoc analysis (baseline §2).
3. A routing policy change takes effect in the data plane within the config-propagation SLO
   (p99 ≤ 30 s).
4. Rollback to the previous version is a single operation and is itself a new version.

**Priority.** MUST · **Owner.** BC-5

---

### BR-14 — Transaction limits

**Statement.** Per-merchant limits — maximum transaction amount, daily volume, velocity
(payments/minute, per-card/hour), maximum partial captures and maximum refund window — are
configuration, enforced before dispatch, and produce a specific, actionable error.

**Rationale.** Limits are the merchant's own protection against a compromised checkout and our
protection against a merchant becoming a fraud conduit. Enforcing them pre-dispatch means a
breach costs one rejected request rather than one chargeback plus a scheme fine.

**Acceptance criteria.**
1. Amount above `risk.maxTransactionAmount` fails L5 with `AMOUNT_EXCEEDS_LIMIT`, before routing.
2. Daily-volume and velocity counters are tenant- and merchant-scoped and survive pod restarts.
3. A refund requested outside `limits.maxRefundWindowDays` is refused with a business-rule error
   naming the window.
4. Limit breaches emit an audit record and a metric; sustained breaches can trigger automated
   suspension per policy.

**Priority.** MUST · **Owner.** BC-5

---

### BR-15 — Risk policy and SCA

**Statement.** A configurable risk policy decides, per payment, whether to allow, decline, or force
3DS/SCA, using merchant configuration, velocity state, corridor and an optional external scorer;
PSD2 exemptions (TRA, low-value, MIT) are modelled explicitly and every decision is auditable.

**Rationale.** SCA is a legal obligation in the EEA and a conversion cost everywhere. Treating 3DS
as a *policy outcome* rather than a client flag lets the merchant tune the conversion/liability
trade-off per corridor without code changes, and gives the compliance officer an evidence trail
for exemption claims.

**Acceptance criteria.**
1. Amount ≥ `risk.require3DSAbove` forces `REQUIRES_ACTION` with `THREE_DS_REQUIRED`.
2. An exemption claim records which exemption, on what basis, with what inputs.
3. The risk engine fails to the **policy default**, never to "allow" (baseline §12 stage 11).
4. Risk decisions are latency-budgeted at 15 ms p99 and are shed (to policy default) rather than
   allowed to blow the payment latency SLO.

**Priority.** MUST · **Owner.** BC-5

---

### BR-16 — Sandbox validation before certification

**Statement.** Before certification, the platform executes a smoke suite against each gateway
connection in sandbox, proving credentials work, webhooks arrive, and the basic authorize/capture
path functions.

**Rationale.** Certification is a 30-minute full-matrix run. Failing it on a typo'd credential
wastes 30 minutes and an operator's attention. Sandbox validation is a 15-minute cheap gate that
catches the common failures early — the same reason a build runs unit tests before integration
tests.

**Acceptance criteria.**
1. `CONFIGURING → SANDBOX_VALIDATION → CERTIFICATION` is the only path to certification.
2. Sandbox failure returns the merchant to `CONFIGURATION_FAILED` with the failing assertion.
3. Sandbox runs never touch production gateway endpoints; the environment is asserted by the
   adapter and by a CI check.

**Priority.** MUST · **Owner.** BC-3

---

### BR-17 — Machine-checked certification

**Statement.** "Certified" means a passing, signed, immutable `CertificationReport` covering the
full assertion matrix (baseline §11.4) for every `(gateway, payment_method, currency)` triple the
merchant enabled. It is never a human opinion or a checkbox.

**Rationale.** Every go-live defect this platform is designed to prevent — unmapped decline codes,
unverified webhook signatures, broken idempotency, amount/currency mismatch — is an assertion in
that matrix. Making the report an artifact makes "are we sure?" answerable by URL.

**Acceptance criteria.**
1. `APPROVED` is unreachable without a passing report; `PRODUCTION_READY` is unreachable without
   `APPROVED` (baseline §8 transition table).
2. The report is stored in object storage, hashed, signed, and referenced from the merchant record.
3. Changing configuration in a way that adds a triple invalidates certification for that triple
   and requires a delta re-run before the triple is live.
4. A report older than the configured validity period (default 12 months) raises a compliance
   finding.

**Priority.** MUST · **Owner.** BC-3

---

### BR-18 — Production activation with explicit guards

**Statement.** A merchant becomes `ACTIVE` only when it has ≥ 1 `CERTIFIED` gateway connection, a
non-empty validated configuration, a completed compliance attestation, and no open critical
reconciliation exception — and the activation is audited with the deciding principal.

**Rationale.** Activation is the moment real money becomes possible. Every guard is a failure mode
we have seen: activating without certification, without a routing target, without compliance
sign-off, or while a prior reconciliation problem is unresolved.

**Acceptance criteria.**
1. Attempting `→ ACTIVE` with any guard unmet is rejected with `INVALID_STATE_TRANSITION` naming
   the unmet guard.
2. Activation emits `merchant.activated.v1`, which invalidates the data-plane merchant cache.
3. The first payment for a newly activated merchant succeeds within the config-propagation SLO
   after activation.

**Priority.** MUST · **Owner.** BC-2 / BC-3

---

### BR-19 — Manual compliance gate

**Statement.** The onboarding workflow includes a blocking manual gate that only a principal with
the `onboarding:approve` scope can clear, with a recorded decision, reason and evidence reference;
the gate has a 5-day SLA and escalates rather than expiring silently.

**Rationale.** Full automation of a compliance decision is not defensible to a regulator or an
acquirer. The gate is where human accountability attaches. It is also where the audit trail must
be strongest, because it is the step most likely to be examined.

**Acceptance criteria.**
1. The gate blocks the workflow indefinitely until signalled; no timer may auto-approve.
2. The approving principal, timestamp, decision, reason and evidence reference are written to the
   hash-chained audit log.
3. A rejection routes the case to a terminal or remediation branch — never a silent stall.
4. Four-eyes mode (configurable per tenant) requires two distinct approving principals.

**Priority.** MUST · **Owner.** BC-3 / BC-9

---

### BR-20 — Accept and execute a payment instruction

**Statement.** An active merchant can submit a payment instruction (amount in minor units,
currency, tokenized payment method, capture mode, metadata) and receive a definitive
`AUTHORIZED`/`CAPTURED`, an actionable `REQUIRES_ACTION`, a definitive failure, or an honest
`processing` — never a fabricated outcome.

**Rationale.** This is the product. The "never a fabricated outcome" clause is the hard part:
timing out and reporting failure is the behaviour that causes double charges when the client
retries (baseline A7, §12.3).

**Acceptance criteria.**
1. Amounts are integer minor units; a decimal or float amount is rejected at L1.
2. A gateway timeout leaves the payment `PROCESSING` and returns `processing` semantics — never
   `FAILED`.
3. Every response carries `requestId` and `traceId` and, on error, a machine-readable `code` and
   `retryable` flag (baseline §20).
4. A payment for a non-`ACTIVE` merchant is refused with `409 MERCHANT_NOT_ACTIVE`.

**Priority.** MUST · **Owner.** BC-6 Payment Orchestration

---

### BR-21 — No double charge, structurally

**Statement.** It must be impossible — not merely unlikely — for one payment instruction to result
in two successful authorizations, under client retries, concurrent duplicate requests, process
crashes, network partitions, duplicate webhooks or duplicate event delivery.

**Rationale.** A double charge is a chargeback, a fine, a support cost and a trust loss. It is the
one defect that can end the business. Defence is layered: client idempotency keys, gateway-level
idempotency keys derived from `attempt_id`, effectively-once event consumption, and — as the final
backstop — a database partial unique index (invariant I3) that makes a second successful attempt
physically unwritable.

**Acceptance criteria.**
1. Concurrent identical requests: exactly one executes; the other receives
   `409 IDEMPOTENT_REQUEST_IN_PROGRESS` with `Retry-After` (baseline A6).
2. Same key + different body → `422 IDEMPOTENCY_KEY_REUSED`.
3. Replay of a completed key returns the stored status and body with `Idempotent-Replay: true`.
4. Invariant I3 is enforced by a partial unique index; a deliberate attempt to write a second
   `SUCCESS` attempt fails at the database level even with application checks disabled.
5. A killed orchestrator mid-dispatch results in either zero or one authorization, never two
   (chaos test).

**Priority.** MUST · **Owner.** BC-6

---

### BR-22 — Failover on retryable failure

**Statement.** When an attempt fails with a retryable classification — transport error, gateway 5xx,
circuit open, or a decline reason in the *retryable decline* set — the platform creates a **new
attempt** on the next eligible gateway in the routing plan, within the merchant's configured
retry budget.

**Rationale.** This is the mechanism of VH-2 and the authorization-uplift metric. Creating a new
attempt rather than mutating the old one is what keeps the audit trail honest and what makes the
gateway idempotency keys correct (baseline §14.4).

**Acceptance criteria.**
1. Failover never mutates a prior attempt; the payment accumulates 1..N attempts.
2. Each attempt carries its own deterministically derived gateway idempotency key.
3. Failover respects the retry budget and the end-to-end latency SLO; it does not cascade
   indefinitely.
4. `payment.attempted.v1` is emitted per attempt with the routing reason, so uplift is measurable.
5. Exhausting the plan yields `503 NO_ELIGIBLE_GATEWAY` or the last definitive decline —
   whichever is truthful — never a synthesised success.

**Priority.** MUST · **Owner.** BC-6

---

### BR-23 — Hard declines must never fail over

**Statement.** A hard decline — stolen card, invalid account, closed account, pickup, fraud
suspected — terminates the payment as `FAILED` immediately. No other gateway is tried.

**Rationale.** Retrying a hard decline across gateways is the exact signature of card testing.
Scheme monitoring programmes and gateway partners treat it as abuse; the consequence is
de-registration (CC-4). It also has no upside: a hard decline is deterministic and will decline
again.

**Acceptance criteria.**
1. Every gateway's decline reason codes are mapped by its adapter into a normalised
   soft/hard classification; the mapping is covered by the adapter contract suite.
2. A hard decline produces exactly one attempt and a terminal `FAILED` payment.
3. An unmapped decline code is treated as **hard** (fail safe) and raises an alert so the mapping
   can be completed.
4. A conformance test asserts zero failovers across a corpus of hard-decline fixtures for every
   adapter.

**Priority.** MUST · **Owner.** BC-6 / BC-4

---

### BR-24 — Two-step flows: authorize then capture

**Statement.** Merchants can authorize and capture separately, capture partially up to the
configured maximum number of partial captures, and void an uncaptured authorization.

**Rationale.** Physical-goods, marketplace and services merchants cannot capture at order time.
Without two-step support the platform is unusable for a large share of the addressable market.

**Acceptance criteria.**
1. Invariant I2 holds: `captured_amount ≤ authorized_amount`, enforced in the domain and by a DB
   constraint.
2. Partial captures beyond `limits.maxPartialCaptures` are refused with a business-rule error.
3. Multi-capture splitting uses largest-remainder allocation so parts sum exactly to the whole
   (baseline §7.4).
4. Voiding a fully captured payment is refused with `INVALID_STATE_TRANSITION`.

**Priority.** MUST · **Owner.** BC-6

---

### BR-25 — Refunds, including after settlement

**Statement.** A merchant can refund a captured or settled payment, in full or in part, repeatedly,
up to the captured total, within the configured refund window — and refunding after settlement is
the normal, fully supported case, not an exception.

**Rationale.** Most refunds happen after settlement; a platform that models refund as
"reverse the capture" is wrong about how payments work. Invariant I1 (`sum(refunds) ≤ captured`)
is the correctness boundary and must be database-enforced because concurrent partial refunds are
a real race.

**Acceptance criteria.**
1. `SETTLED → PARTIALLY_REFUNDED | REFUNDED` is a supported transition.
2. Concurrent partial refunds totalling more than the captured amount: exactly one succeeds; the
   other receives `REFUND_EXCEEDS_CAPTURED` (serialized update + `CHECK`).
3. A refund is idempotent on the client key and produces its own `ref_` identifier and ledger
   entries.
4. Refunds are permitted while the merchant is `SUSPENDED` (see BR-31).

**Priority.** MUST · **Owner.** BC-6

---

### BR-26 — Voids

**Statement.** An authorized, uncaptured payment can be voided, releasing the hold; a void is
idempotent and terminal.

**Rationale.** A hold left on a customer's card after a cancelled order is a support call and, in
some jurisdictions, a regulatory complaint. Void must be as reliable as capture.

**Acceptance criteria.**
1. `AUTHORIZED → VOIDED` succeeds; `CAPTURED → VOIDED` is refused.
2. A void whose gateway call times out follows the same `TIMEOUT_UNKNOWN` rule as any other
   gateway call — never auto-failed.
3. Authorization expiry moves the payment to `EXPIRED` without inventing a void.

**Priority.** MUST · **Owner.** BC-6

---

### BR-27 — Disputes and chargebacks

**Statement.** The platform ingests dispute lifecycle notifications from gateways, moves the
payment to `DISPUTED`, records the reason code, deadline and amount, notifies the merchant, and
reflects the outcome (`REFUNDED` if lost, restoration to `CAPTURED`/`SETTLED` if won).

**Rationale.** Disputes are the financial consequence of everything upstream (BR-15, BR-23), and
the dispute rate is the metric gateways use to judge us. We do not author representment (out of
scope) but we must not lose the event.

**Acceptance criteria.**
1. `payment.disputed.v1` is emitted and consumed by Ledger, Risk and Notification.
2. Dispute deadlines are surfaced with enough lead time to act (configurable, default 5 days).
3. A dispute on a payment we have no record of raises a critical reconciliation exception rather
   than being dropped.
4. Dispute rate per merchant and per gateway is a first-class metric.

**Priority.** SHOULD · **Owner.** BC-8 Ledger & Reconciliation

---

### BR-28 — Resolve unknown outcomes by reconciliation

**Statement.** Every attempt in `TIMEOUT_UNKNOWN` is resolved to a definitive outcome by webhook,
by gateway status lookup using our deterministic idempotency key, or by settlement report — within
a bounded time — and never by a timer that guesses.

**Rationale.** Baseline A7 and §12.3. This requirement is what makes BR-20's "never a fabricated
outcome" operable. The reconciler is the component that converts honesty about uncertainty into
eventual certainty.

**Acceptance criteria.**
1. Every `TIMEOUT_UNKNOWN` attempt enqueues `payment.reconciliation_required.v1` with alerting.
2. p95 resolution ≤ 5 min; p99 ≤ 30 min; anything unresolved at 24 h is a critical exception with
   a page.
3. No code path transitions a payment out of `PROCESSING` on a timer alone; a test asserts this
   by fault injection.
4. The reconciler can reconstruct the gateway idempotency key from `attempt_id` after a total
   process loss.

**Priority.** MUST · **Owner.** BC-6 / BC-8

---

### BR-29 — Shadow ledger

**Statement.** The platform maintains an append-only, double-entry shadow ledger of every
authorization, capture, refund, void, settlement and dispute, sufficient to reconcile against the
gateway's own reporting and to answer "what did we believe, and when?".

**Rationale.** We hold no funds (A1), so this is not a money-custody ledger; it is the artifact
that makes reconciliation and finance reporting possible without querying eight gateways. Its
append-only nature is what makes it trustworthy.

**Acceptance criteria.**
1. Ledger entries are written in the same transaction as the state change that caused them.
2. No `UPDATE` or `DELETE` is permitted on `ledger_entries`; corrections are compensating entries.
3. Debits and credits balance per transaction; a continuous invariant check alerts on imbalance.
4. A ledger position can be reconstructed as of any past timestamp.

**Priority.** MUST · **Owner.** BC-8

---

### BR-30 — Settlement observation and reconciliation

**Statement.** The platform ingests gateway settlement reports and webhooks, matches them to
payments, moves matched payments to `SETTLED`, and raises typed exceptions for unmatched,
duplicate, amount-mismatched or missing items.

**Rationale.** Baseline A12: we observe settlement, we do not compute it. The value is the
*matching* and the *exception queue* — that is the finance toil VH-6 targets.

**Acceptance criteria.**
1. Reconciliation runs are recorded (`rcn_`) with inputs, outputs and exception counts.
2. Exception types are enumerated and each has a documented remediation runbook.
3. Re-ingesting the same settlement file is a no-op (dedup on file hash + line identity).
4. Open critical exceptions block merchant activation (BR-18 guard).

**Priority.** MUST · **Owner.** BC-8

---

### BR-31 — Suspension that still permits refunds

**Statement.** A merchant can be suspended by an operator or automatically by the platform (risk
breach, compliance expiry, gateway de-provisioning). Suspension rejects **new payments** but
continues to permit **refunds, voids, webhook processing and reconciliation**.

**Rationale.** Baseline §8. Suspending a merchant's ability to give money back turns a commercial
dispute into a consumer-protection incident. You must always be able to return funds.

**Acceptance criteria.**
1. `POST /v1/payments` for a `SUSPENDED` merchant → `409 MERCHANT_NOT_ACTIVE`.
2. `POST /v1/payments/{id}/refund` and `/void` for a `SUSPENDED` merchant succeed.
3. Inbound webhooks for a suspended merchant are processed normally.
4. `merchant.suspended.v1` triggers **priority** cache invalidation in the data plane, not the
   normal ≤ 30 s path.
5. Suspension records actor, reason code and free-text justification in the audit log.

**Priority.** MUST · **Owner.** BC-2

---

### BR-32 — Termination and erasure

**Statement.** A merchant can be terminated only when it has no payments in a non-terminal state.
Termination de-provisions gateway connections, revokes credentials, and — on a right-to-erasure
request — crypto-shreds the tenant/merchant data key while retaining financial records under the
legal-obligation basis.

**Rationale.** Baseline §8 and §17.3. Deleting rows would destroy the financial record we are
legally obliged to keep for 7 years; crypto-shredding satisfies erasure of personal data while
preserving the ledger. Requiring terminal payment state prevents orphaning money in flight.

**Acceptance criteria.**
1. `→ TERMINATED` with any payment in a non-terminal state is refused, naming the payments.
2. Termination runs the compensating de-provision path for every connection.
3. After crypto-shredding, personal data is unreadable while ledger and audit rows remain
   queryable in their non-personal form.
4. The erasure operation is itself audited and its evidence retained.

**Priority.** MUST · **Owner.** BC-2 / BC-9

---

### BR-33 — Tenant self-service configuration with versioning and rollback

**Statement.** Tenant and merchant admins can read, validate, publish and roll back configuration
through the API, with optimistic concurrency (`ETag`/`If-Match`), full version history, actor
attribution and diffs — with no platform-operator involvement.

**Rationale.** VH-4 and CC-7. Self-service is only safe when it is validated (L4), versioned,
attributable and reversible. Given those four properties, the risk of a tenant changing its own
configuration is lower than the risk of a support engineer doing it from a ticket.

**Acceptance criteria.**
1. A stale `If-Match` yields `412`; a concurrent publish yields
   `CONFIGURATION_VERSION_CONFLICT`.
2. Rollback publishes the prior document **as a new version**; history is append-only
   (baseline §23).
3. Every version records actor, timestamp, validation result and a structured diff.
4. Config changes propagate to the data plane within p99 ≤ 30 s and are observable via a synthetic
   probe.

**Priority.** MUST · **Owner.** BC-5

---

### BR-34 — Gateway registry and capability descriptors

**Statement.** Every supported gateway is described by a declarative capability descriptor —
countries, currencies, payment methods, operations, 3DS support, partial-capture support, refund
window, webhook signature scheme — and all eligibility decisions derive from it.

**Rationale.** Hard-coding "Adyen supports X" into routing logic guarantees drift the moment the
gateway changes. A descriptor makes capability data, testable against the adapter contract suite,
and makes adding a gateway a data-plus-adapter change rather than a logic change.

**Acceptance criteria.**
1. Routing eligibility, L4 configuration validation and certification matrices are all computed
   from the descriptor — there is no second source.
2. A descriptor change that removes a capability a live merchant depends on is refused, with the
   affected merchants listed.
3. The adapter contract suite asserts the adapter's real behaviour matches its descriptor.

**Priority.** MUST · **Owner.** BC-4

---

### BR-35 — Gateway health visibility and operator control

**Statement.** Gateway health per `(gateway, operation)` is observable in real time, drives routing
automatically, and can be overridden by an operator (force-open, force-close, drain) with the
override audited and time-bounded.

**Rationale.** Automation handles the common case; incidents are the uncommon case. An SRE who
knows a gateway is about to fail needs a lever that does not require a deploy. Time-bounding the
override prevents the classic "someone drained a gateway in March and nobody noticed until
October" failure.

**Acceptance criteria.**
1. `GET /v1/gateways/{gatewayId}/health` reflects the state machine of baseline §10.
2. `gateway.health_changed.v1` is consumed by routing within seconds.
3. An operator override carries a mandatory expiry (default 4 h, max 72 h) and auto-reverts.
4. Overrides are audited and surfaced on the incident dashboard.

**Priority.** SHOULD · **Owner.** BC-4

---

### BR-36 — Data residency enforcement

**Statement.** A tenant's declared residency region constrains where its personal data is stored
and which gateways the routing engine may select; a routing candidate whose processing region
violates the policy is ineligible regardless of health or cost.

**Rationale.** Baseline §17.3. Residency violations are found by auditors, not by monitoring, and
are remediated at enormous cost. Making residency a *routing eligibility filter* rather than a
policy document is what turns it into an enforced control.

**Acceptance criteria.**
1. A gateway whose region violates the tenant's residency policy never appears in a routing plan;
   the exclusion reason is recorded on the plan.
2. KYC artifacts and merchant principal data are written only to the declared region's stores.
3. A residency violation attempt raises a security event and an audit record.
4. A residency compliance report can be produced per tenant on demand.

**Priority.** MUST · **Owner.** BC-1 / BC-5

---

### BR-37 — Audit trail and compliance reporting

**Statement.** Every state-changing action — by human or by automation — writes an immutable,
hash-chained audit record capturing actor, action, subject, before/after, correlation ID and
timestamp; auditors and compliance officers can export a tenant-scoped, tamper-evident report over
any time range.

**Rationale.** In a payments platform the audit trail is not a feature, it is the evidence that the
platform behaved as specified. Hash-chaining means tampering is detectable rather than merely
discouraged; WORM storage means it is not deletable.

**Acceptance criteria.**
1. Audit records are append-only and chained; a verification job recomputes the chain and alerts
   on a break.
2. Audit contains no PAN, no CVV, no credential material, and no unmasked bank account numbers.
3. Retention is 7 years, WORM (Object Lock), with an integrity attestation per period.
4. Export is tenant-scoped, cursor-paginated, and cannot cross tenant boundaries.

**Priority.** MUST · **Owner.** BC-9 Audit

---

### BR-38 — Metering for billing

**Statement.** The platform produces an accurate, reproducible, tenant-scoped meter of billable
events — authorization attempts, activated merchants, certification runs, siloed-tier resources —
exportable for invoicing, and reconcilable against the ledger.

**Rationale.** CC-1: a metering error is a revenue error in one direction or a customer-trust error
in the other. Deriving the meter from the event stream (rather than from a separate counter) means
it can be recomputed from source and defended in a billing dispute.

**Acceptance criteria.**
1. The meter is a deterministic projection of the event log; recomputing a closed period yields
   the identical result.
2. Meter output reconciles to `payments`/`payment_attempts` within 0 variance for a closed period.
3. Late-arriving events are attributed to the period in which they occurred, with a restatement
   record.

**Priority.** SHOULD · **Owner.** BC-8

---

## 9. Priority summary

| Priority | Count | IDs |
|---|---|---|
| MUST | 35 | BR-01..BR-26, BR-28..BR-34, BR-36, BR-37 |
| SHOULD | 3 | BR-27, BR-35, BR-38 |
| COULD | 0 | Deferred items are tracked in §11 rather than parked as COULD requirements — a COULD requirement is a requirement nobody is accountable for |

The MUST set is exactly the fundable milestone under CC-8: a tenant can be registered, a merchant
can be onboarded end-to-end without human work beyond the compliance gate, payments execute across
≥ 2 gateways with failover and no double-charge risk, money can be given back, and every action is
auditable.

---

## 10. Alternatives considered and rejected

| # | Alternative | Why rejected |
|---|---|---|
| ALT-1 | **Become a licensed PSP and take fund custody** | Multiplies regulatory surface (safeguarding, client-money audit, capital requirements) and adds 12–18 months before first revenue. The orchestration value hypothesis (VH-2) is testable without a licence. Contradicts baseline A1. |
| ALT-2 | **Vault PAN ourselves to offer gateway portability** | Would put all nine services in PCI SAQ-D scope, changing the cost structure of every deploy, every log line and every engineer's access. Network tokens and gateway-side tokenization achieve most of the portability benefit at a fraction of the control cost. Contradicts baseline A2. |
| ALT-3 | **Single-tenant deployment per customer** | Operationally simple and trivially isolated, but the per-tenant fixed cost destroys CC-2 gross margin below ~10 M payments/month, and 500 tenants means 500 upgrade pipelines. Pooled-with-RLS plus a siloed premium tier gets the isolation where it is contractually required and the economics everywhere else (baseline A3, §16). |
| ALT-4 | **Buy an orchestration vendor and build only onboarding** | The two products share the merchant, configuration and certification model. Integrating a third-party orchestrator would mean synchronising that model across a vendor boundary — reintroducing config drift, which is the problem being solved. Also forfeits VH-5 (vendor neutrality is the differentiator). |
| ALT-5 | **Onboarding as a human-in-the-loop service (ops team as the product)** | Fastest to first customer, but VH-1's 80 % cost reduction is unreachable and margin degrades with scale. It is a services business, not a platform business. Rejected on CC-2. |
| ALT-6 | **Let merchants choose the gateway per request (client-directed routing)** | Simple and transparent, but moves routing intelligence to the merchant's code, forfeits authorization uplift (VH-2), and makes failover impossible without a client retry — which is exactly the double-charge pattern BR-21 exists to eliminate. Client *pinning* is supported as a per-merchant override; per-request gateway selection is not. |
| ALT-7 | **Treat 3DS as a client flag rather than a policy outcome** | Puts SCA compliance in the merchant's integration, where it cannot be audited or tuned centrally, and makes exemption claims unevidenced. Rejected on BR-15's compliance criteria. |
| ALT-8 | **Auto-fail payments on gateway timeout, let clients retry** | The single most common cause of double charges in production payment systems. Rejected explicitly by baseline A7; BR-28 is the alternative. |
| ALT-9 | **Certification as an operator checklist** | Cheap, and it is what most of the market does. It produces go-live defects at a rate that destroys VH-3 and consumes the support budget that CC-7 depends on. Machine-checked certification (BR-17) is the whole reason go-live defects can be driven toward zero. |
| ALT-10 | **Hard-delete on right-to-erasure** | Would destroy records we are legally obliged to retain for 7 years, creating a direct conflict between GDPR Art. 17 and AML/records obligations. Crypto-shredding resolves the conflict (BR-32). |

---

## 11. Deferred (post-milestone) capabilities

Tracked here rather than as COULD requirements so that the requirement set stays honest about the
fundable scope.

| Capability | Trigger to promote |
|---|---|
| Alternative rails (open banking, RTP, wallets beyond Apple/Google Pay) | ≥ 3 tenants with a contracted need |
| Network-token provisioning and lifecycle management in-platform | Measured uplift ≥ 0.5 pp over gateway-managed tokens |
| ML-based routing optimisation (bandit over the scoring weights) | ≥ 6 months of clean attempt-outcome data |
| Dispute representment authoring | Dispute volume ≥ 500/month across the book |
| Merchant-facing analytics product | Tenant demand + VH-6 confirmed |
| Active/active payment processing | Regulatory or contractual RTO requirement below 15 min |

---

## 12. Traceability index

| BR | Realised by (FR) | Constrained by (NFR) | Baseline sections |
|---|---|---|---|
| BR-01 | FR-01, FR-02, FR-06 | NFR-29, NFR-37, NFR-59 | §16, §17.3 |
| BR-02 | FR-03, FR-04, FR-05, FR-08 | NFR-28, NFR-31, NFR-34 | §12, §19.1 |
| BR-03 | FR-09, FR-10, FR-11 | NFR-06, NFR-16, NFR-61 | §6, §19.1 |
| BR-04 | FR-16, FR-17, FR-18, FR-30 | NFR-08, NFR-61 | §11, §21 |
| BR-05 | FR-19, FR-20, FR-21 | NFR-41, NFR-42 | §11, §17.3 |
| BR-06 | FR-22, FR-23 | NFR-32, NFR-38 | §11, §21 |
| BR-07 | FR-26, FR-49, FR-60 | NFR-02, NFR-20 | §21, §23 |
| BR-08 | FR-49, FR-60 | NFR-02, NFR-20 | §7, §23 |
| BR-09 | FR-24, FR-28, FR-35, FR-62 | NFR-04, NFR-11, NFR-57 | §3, §11.4 |
| BR-10 | FR-24, FR-31, FR-42 | NFR-08, NFR-22 | §11 |
| BR-11 | FR-25, FR-40, FR-75 | NFR-30, NFR-32, NFR-34 | §17.2 |
| BR-12 | FR-38, FR-39, FR-52 | NFR-31, NFR-49 | §17.2 |
| BR-13 | FR-46, FR-50, FR-62 | NFR-11, NFR-21 | §12, §23 |
| BR-14 | FR-48, FR-51, FR-60 | NFR-02, NFR-20, NFR-22 | §12, §23 |
| BR-15 | FR-51, FR-61, FR-68 | NFR-02, NFR-40 | §12, §17.3 |
| BR-16 | FR-27 | NFR-08 | §11 |
| BR-17 | FR-12, FR-28, FR-49 | NFR-08, NFR-57 | §11.4 |
| BR-18 | FR-12, FR-84 | NFR-21, NFR-45 | §8 |
| BR-19 | FR-20, FR-29 | NFR-34, NFR-42 | §11, §22 |
| BR-20 | FR-53, FR-59, FR-66, FR-67, FR-72, FR-73 | NFR-01, NFR-02, NFR-03 | §9, §12 |
| BR-21 | FR-54, FR-55, FR-56, FR-57, FR-58, FR-63, FR-77 | NFR-12, NFR-24, NFR-36 | §14, §13.5 |
| BR-22 | FR-62, FR-64, FR-67 | NFR-03, NFR-04, NFR-11 | §9.1, §24 |
| BR-23 | FR-41, FR-65 | NFR-45, NFR-53 | §9.1, §24 |
| BR-24 | FR-53, FR-69 | NFR-01, NFR-24 | §9 |
| BR-25 | FR-51, FR-70 | NFR-12, NFR-24 | §9 |
| BR-26 | FR-71 | NFR-24 | §9 |
| BR-27 | FR-78, FR-86 | NFR-10, NFR-45 | §13.2 |
| BR-28 | FR-58, FR-66, FR-79, FR-83, FR-85 | NFR-03, NFR-10, NFR-46 | §12.3 |
| BR-29 | FR-80, FR-81 | NFR-17, NFR-24 | §3 (BC-8) |
| BR-30 | FR-74, FR-75, FR-76, FR-77, FR-78, FR-82, FR-83, FR-84 | NFR-05, NFR-09, NFR-10 | §13.2 |
| BR-31 | FR-13, FR-47, FR-59, FR-70, FR-71 | NFR-21, NFR-22 | §8 |
| BR-32 | FR-14, FR-15, FR-31 | NFR-38, NFR-42 | §8, §17.3 |
| BR-33 | FR-10, FR-43, FR-44, FR-45, FR-46, FR-47 | NFR-06, NFR-21, NFR-49 | §23 |
| BR-34 | FR-33, FR-34, FR-41, FR-42 | NFR-53, NFR-54, NFR-57 | §2, §11.4 |
| BR-35 | FR-36, FR-37 | NFR-11, NFR-45, NFR-47 | §10 |
| BR-36 | FR-07, FR-62, FR-90 | NFR-37, NFR-38 | §17.3 |
| BR-37 | FR-15, FR-88, FR-89, FR-90, FR-91 | NFR-34, NFR-41, NFR-42 | §3 (BC-9) |
| BR-38 | FR-87 | NFR-58, NFR-59 | §13 |
