# 02 — Functional Requirements

> Purpose: specify *what the system does*, as numbered behaviours `FR-01..FR-91` grouped by the nine bounded contexts of [`00-design-baseline.md`](./00-design-baseline.md) §3. Every FR realises one or more `BR-nn` from [`01-business-requirements.md`](./01-business-requirements.md); the baseline is binding and this file elaborates it, never contradicts it.

---

## 0. How to read an FR

Every requirement carries the same ten fields.

| Field | Meaning |
|---|---|
| **Statement** | The behaviour, in the imperative. |
| **Trigger** | The event or call that initiates it. |
| **Preconditions** | What must already hold. A violated precondition is an error, not a branch. |
| **Main flow** | The nominal path, numbered. |
| **Alternates** | Named `A*` (alternate, still a success) and `E*` (exception, an error outcome). Exception flows are requirements, not documentation. |
| **Postconditions** | What is true afterwards, including the durable state. |
| **Events** | Domain events emitted, per baseline §13.2. All are written through the outbox in the same transaction as the state change (§13.4). |
| **Validation** | Validation-plane levels involved, per baseline §21. |
| **API** | Endpoint(s) from baseline §19, or `internal` for non-API behaviour. |
| **Traces** | `BR-nn` realised. |

Conventions used throughout and not repeated per FR:

- Every mutating control-plane and money-path call requires `Idempotency-Key` (baseline §14.1);
  the idempotency behaviours themselves are specified once in FR-54..FR-58 and apply everywhere.
- Every request passes pipeline stages 1–6 of baseline §12 (TLS, request ID, authn, tenant
  resolution, authz, rate limit) before reaching the behaviour described. FR-04, FR-05 and FR-06
  specify those stages once.
- Every error response is `application/problem+json` per baseline §20 with a reserved `code`.
- Every state change writes an audit record (FR-88).

---

## 1. BC-1 — Tenant & Identity (Control plane)

### FR-01 — Register a tenant

**Statement.** Create a tenant with an immutable isolation tier, residency region and commercial plan, and allocate its isolation resources.
**Trigger.** Platform operator calls the tenant provisioning API (`platformctl tenant create` wraps it).
**Preconditions.** Caller holds the platform-operator role; `tier ∈ {POOLED, SILOED}`; `residency_region` is an enabled region; plan exists.
**Main flow.** 1) L1 schema validation. 2) Allocate `ten_` ULID. 3) Insert `tenants` row with tier, region, plan, status `PROVISIONING`. 4) Enqueue isolation-resource provisioning (FR-02). 5) On completion set status `ACTIVE`.
**Alternates.** *E1* duplicate legal name + registration number → `409` (advisory, not a uniqueness guarantee — different legal entities can share a name across jurisdictions, so this is a soft conflict the operator can override with `?allowDuplicate=true`). *E2* residency region not enabled → `422 CONFIGURATION_INVALID`. *E3* provisioning fails → tenant stays `PROVISIONING`, alert raised, no partial `ACTIVE`.
**Postconditions.** Tenant row exists; tier and region are write-once (enforced by a DB trigger, not only by application code).
**Events.** `audit.recorded.v1`.
**Validation.** L1, L4.
**API.** `POST /v1/tenants` (operator scope).
**Traces.** BR-01.

### FR-02 — Provision isolation resources for a siloed tenant

**Statement.** For `SILOED` tenants, create the dedicated schema, KMS CMK, Redis namespace, Kafka topics and object-storage bucket before the tenant becomes `ACTIVE`.
**Trigger.** FR-01 step 4.
**Preconditions.** Tenant row in `PROVISIONING`; tier is `SILOED`.
**Main flow.** 1) Create KMS CMK with a tenant-scoped key policy. 2) Create dedicated schema and run migrations against it. 3) Create dedicated topics with the same partition counts as the pooled equivalents (baseline §13.3). 4) Create the bucket with Object Lock enabled. 5) Register the resource references on the tenant row.
**Alternates.** *A1* tier is `POOLED` → step is a no-op; pooled resources already exist. *E1* any step fails → compensate in reverse order (delete topics, drop schema, schedule CMK deletion with the mandatory waiting period) and leave the tenant `PROVISIONING` with a failure reason.
**Postconditions.** Every siloed resource exists and is referenced; no half-provisioned tenant is ever `ACTIVE`.
**Events.** `audit.recorded.v1`.
**Validation.** L3.
**API.** internal (workflow).
**Traces.** BR-01.

### FR-03 — Issue an API client with least-privilege scopes

**Statement.** Issue an API client bound to a scope set, optionally restricted to a merchant subset and an IP allowlist, returning the secret exactly once.
**Trigger.** `POST /v1/api-clients`.
**Preconditions.** Tenant `ACTIVE`; caller holds `clients:write`; every requested scope is in the published vocabulary and is a subset of the caller's own scopes (no privilege escalation).
**Main flow.** 1) Validate scopes against the vocabulary and against the caller's scopes. 2) Generate a `cli_` ULID and a 256-bit secret. 3) Store an Argon2id hash of the secret; the plaintext is never persisted. 4) Return the plaintext once, with `Cache-Control: no-store`.
**Alternates.** *E1* requested scope exceeds the caller's → `403 FORBIDDEN` with the offending scope named. *E2* merchant subset references a merchant of another tenant → `403 TENANT_MISMATCH` + security event (FR-91).
**Postconditions.** Client row exists with scope set, merchant restriction, IP allowlist, `created_at`, `expires_at`.
**Events.** `audit.recorded.v1`.
**Validation.** L1, L4.
**API.** `POST /v1/api-clients`, `GET /v1/api-clients`.
**Traces.** BR-02.

### FR-04 — Authenticate a request

**Statement.** Authenticate every non-webhook request by OAuth2/JWT (JWKS-verified) or mTLS client certificate, within a 2 ms budget.
**Trigger.** Any API request (baseline §12 stage 3).
**Preconditions.** None.
**Main flow.** 1) Extract bearer token or client certificate. 2) Verify signature against the cached JWKS (background-refreshed) or the certificate chain. 3) Verify `exp`, `nbf`, `aud`, `iss`. 4) Materialise a `Principal` into the request context.
**Alternates.** *A1* token signed by the previous key in the 2-key JWKS window → accepted (30-day rotation, baseline §17.2). *E1* invalid/expired token → `401 UNAUTHENTICATED`; the token is not logged. *E2* JWKS endpoint unreachable → serve from cache; if the cache is also cold, `503 SERVICE_UNAVAILABLE` — never accept unverified tokens.
**Postconditions.** `Principal` in context, or the request is terminated.
**Events.** `audit.recorded.v1` on authentication failure spikes only (per-failure auditing at this stage is a log-volume attack vector).
**Validation.** —
**API.** all.
**Traces.** BR-02.

### FR-05 — Authorize a request by scope and attribute

**Statement.** Enforce the endpoint's required scope (RBAC) and any attribute constraints (merchant subset, IP allowlist, environment) within a 2 ms budget.
**Trigger.** Baseline §12 stage 5.
**Preconditions.** FR-04 succeeded.
**Main flow.** 1) Look up the route's required scope from the compiled route table. 2) Assert the principal holds it. 3) Evaluate ABAC predicates: requested `merchantId` ∈ the client's merchant subset; source IP ∈ allowlist; environment matches.
**Alternates.** *E1* missing scope → `403 FORBIDDEN`, audited. *E2* merchant outside the subset → `403 FORBIDDEN` (deliberately not `404`; the caller is authenticated and the merchant's existence is not a secret from its own tenant). *E3* IP outside allowlist → `403` + security event.
**Postconditions.** Authorization decision recorded on the span.
**Events.** `audit.recorded.v1` on denial.
**Validation.** —
**API.** all.
**Traces.** BR-02.

### FR-06 — Resolve the tenant and enforce the isolation guard

**Statement.** Derive `tenant_id` exclusively from the authenticated principal, set it on the request context and on the database session (`SET LOCAL app.tenant_id`), and treat a disagreeing body/query value as a security event.
**Trigger.** Baseline §12 stage 4.
**Preconditions.** FR-04 succeeded.
**Main flow.** 1) Read `tenant_id` from the principal. 2) Bind it into `context.Context`. 3) On every repository call, `SET LOCAL app.tenant_id` inside the transaction so PostgreSQL RLS applies. 4) Ignore any `tenantId` in the request body.
**Alternates.** *E1* body/query `tenantId` disagrees with the token → `403 TENANT_MISMATCH`, audit record, security event, alert. *E2* repository invoked with no tenant in context → return `ErrMissingTenantContext` **without** issuing a query.
**Postconditions.** Every query in the request executes under RLS scoped to one tenant.
**Events.** `audit.recorded.v1`.
**Validation.** —
**API.** all.
**Traces.** BR-01, BR-02.

### FR-07 — Enforce the tenant data-residency policy

**Statement.** Constrain personal-data writes to the tenant's declared region and expose the residency constraint as a routing-eligibility input.
**Trigger.** Any write of merchant principal data, KYC artifact, or a routing decision.
**Preconditions.** Tenant has a declared `residency_region`.
**Main flow.** 1) Personal-data repositories assert the writing region equals the tenant's region. 2) Object-storage writes use the region's bucket. 3) The routing engine receives the residency policy in the merchant context snapshot and excludes gateways whose processing region violates it (see FR-62).
**Alternates.** *E1* write attempted in a non-conforming region → refuse, raise a security event, alert. *E2* every candidate gateway is excluded on residency → the routing plan is empty → `503 NO_ELIGIBLE_GATEWAY` with the exclusion reason on the persisted plan.
**Postconditions.** No personal data outside the declared region; every residency exclusion is recorded on the routing plan for later evidence.
**Events.** `audit.recorded.v1`.
**Validation.** L4, L5.
**API.** internal.
**Traces.** BR-36.

### FR-08 — Rotate and revoke API client credentials with overlap

**Statement.** Rotate an API client secret with a configurable overlap window during which both secrets authenticate, and propagate revocation globally within 60 s.
**Trigger.** `POST /v1/api-clients/{clientId}:rotate` or `DELETE /v1/api-clients/{clientId}`.
**Preconditions.** Caller holds `clients:write`; client belongs to the caller's tenant.
**Main flow.** 1) Generate the new secret, store its hash alongside the old one with `old_valid_until = now + overlap`. 2) Return the new secret once. 3) After the window, a scheduled job deletes the old hash. 4) Revocation writes a tombstone to the revocation list, which every region's authenticator polls at ≤ 15 s.
**Alternates.** *A1* rotation while a prior rotation's overlap is still open → the oldest secret is dropped immediately; at most two secrets are ever valid. *E1* revocation of an already-revoked client → `204` (idempotent).
**Postconditions.** At most two valid secrets; revocation is effective within 60 s globally.
**Events.** `audit.recorded.v1`.
**Validation.** L1.
**API.** `POST /v1/api-clients/{clientId}:rotate`, `DELETE /v1/api-clients/{clientId}`.
**Traces.** BR-02.

---

## 2. BC-2 — Merchant Registry (Control plane)

### FR-09 — Register a merchant

**Statement.** Create a merchant under the caller's tenant in state `CREATED`.
**Trigger.** `POST /v1/merchants`.
**Preconditions.** Tenant `ACTIVE`; caller holds `merchants:write`; `Idempotency-Key` present.
**Main flow.** 1) L1 schema validation including the PAN detector. 2) Idempotency claim (FR-54). 3) Allocate `mrc_` ULID. 4) Insert `merchants` + `merchant_business_profile` rows in one transaction with the outbox row. 5) Return `201` with the merchant representation and an `ETag`.
**Alternates.** *A1* duplicate `Idempotency-Key` with identical fingerprint → replay `201` with `Idempotent-Replay: true`. *E1* `external_reference` already used in this tenant → `409`. *E2* body contains a Luhn-valid 13–19 digit string → `400 SENSITIVE_DATA_IN_REQUEST`, value not logged, security event.
**Postconditions.** Merchant exists in `CREATED`; no onboarding case yet.
**Events.** `merchant.created.v1`.
**Validation.** L1, L2 (structural subset).
**API.** `POST /v1/merchants`.
**Traces.** BR-03.

### FR-10 — Update a merchant profile with optimistic concurrency

**Statement.** Patch mutable merchant attributes under `If-Match`, rejecting stale writes.
**Trigger.** `PATCH /v1/merchants/{merchantId}`.
**Preconditions.** Merchant exists in the caller's tenant; `If-Match` present; `Idempotency-Key` present.
**Main flow.** 1) Load merchant, compute current `ETag`. 2) Compare with `If-Match`. 3) Apply the patch to mutable fields only. 4) Persist with a version increment; write the audit diff.
**Alternates.** *E1* `If-Match` mismatch → `412`. *E2* patch touches an immutable field (tenant, legal entity after KYC approval, registration country) → `422` naming the field. *E3* merchant `TERMINATED` → `422 INVALID_STATE_TRANSITION`.
**Postconditions.** New version persisted; prior value retained in the audit record.
**Events.** `audit.recorded.v1`.
**Validation.** L1, L2.
**API.** `PATCH /v1/merchants/{merchantId}`.
**Traces.** BR-03, BR-33.

### FR-11 — List and read merchants

**Statement.** Return merchants of the caller's tenant with opaque cursor pagination and stable ordering.
**Trigger.** `GET /v1/merchants`, `GET /v1/merchants/{merchantId}`.
**Preconditions.** `merchants:read`.
**Main flow.** 1) Apply filters (state, country, created range). 2) Order by `(created_at, merchant_id)` descending. 3) Encode the cursor as the last `(created_at, merchant_id)` tuple, signed to make it opaque and tamper-evident. 4) Return `{ data, next_cursor }`.
**Alternates.** *A1* client restricted to a merchant subset → results are further filtered. *E1* cursor fails signature verification → `400 VALIDATION_FAILED`. *E2* merchant of another tenant → `404 MERCHANT_NOT_FOUND` (existence is not disclosed across tenants; contrast FR-05 *E2*, which is intra-tenant).
**Postconditions.** No cross-tenant row is ever returned — enforced by RLS, not only by the `WHERE` clause.
**Events.** —
**Validation.** L1.
**API.** `GET /v1/merchants`, `GET /v1/merchants/{merchantId}`.
**Traces.** BR-03.

### FR-12 — Activate a merchant under explicit guards

**Statement.** Transition `PRODUCTION_READY → ACTIVE` only when every activation guard holds, and record the deciding principal.
**Trigger.** Workflow step 12 (`activate`), or an operator call.
**Preconditions.** Merchant in `PRODUCTION_READY`.
**Main flow.** 1) Assert ≥ 1 `GatewayConnection` in `CERTIFIED`. 2) Assert a non-empty validated `MerchantConfiguration` at status `ACTIVE`. 3) Assert a completed compliance attestation. 4) Assert zero open critical reconciliation exceptions. 5) Apply the transition with optimistic concurrency. 6) Emit `merchant.activated.v1`.
**Alternates.** *E1* any guard unmet → `409 INVALID_STATE_TRANSITION` whose `details[]` names each unmet guard (all of them, not the first). *E2* concurrent activation → one succeeds on the version check, the other gets `409`.
**Postconditions.** Merchant `ACTIVE`; data-plane merchant cache invalidated; first payment possible within the config-propagation SLO.
**Events.** `merchant.activated.v1`, `audit.recorded.v1`.
**Validation.** L7.
**API.** internal (workflow) + operator endpoint.
**Traces.** BR-18, BR-17, BR-30.

### FR-13 — Suspend and unsuspend a merchant

**Statement.** Suspend a merchant — by operator action or by automation — such that new payments are rejected while refunds, voids, webhook processing and reconciliation continue; and restore it.
**Trigger.** Operator call; or risk-policy breach, compliance expiry, or gateway de-provisioning detected by the automation plane.
**Preconditions.** Merchant `ACTIVE` (suspend) or `SUSPENDED` (unsuspend).
**Main flow.** 1) Apply the FSM transition. 2) Record actor, reason code and justification. 3) Emit `merchant.suspended.v1` marked for **priority** cache invalidation — the data plane treats it out-of-band rather than on the ≤ 30 s path. 4) Data-plane caches evict the merchant entry on receipt.
**Alternates.** *A1* unsuspend requires the suspension reason to be resolved; a suspension raised by automation cannot be cleared by the same automation without a human or an explicit policy re-evaluation. *E1* suspend a `TERMINATED` merchant → `409`.
**Postconditions.** `POST /v1/payments` → `409 MERCHANT_NOT_ACTIVE`; `/refund` and `/void` continue to succeed; inbound webhooks continue to be processed.
**Events.** `merchant.suspended.v1`, `audit.recorded.v1`.
**Validation.** L7.
**API.** operator endpoint + internal.
**Traces.** BR-31.

### FR-14 — Terminate a merchant

**Statement.** Terminate a merchant only when no payment is in a non-terminal state, de-provisioning every gateway connection and revoking credentials.
**Trigger.** Operator or tenant-admin call.
**Preconditions.** Merchant in `CREATED`, `VALIDATION_FAILED`, `KYC_FAILED`, `PROVISIONING_FAILED`, `CONFIGURATION_FAILED`, `CERTIFICATION_FAILED`, `ACTIVE` or `SUSPENDED`.
**Main flow.** 1) Query payments in non-terminal states (`CREATED`, `REQUIRES_ACTION`, `PROCESSING`, `PENDING`, `AUTHORIZED`, `DISPUTED`). 2) If none, transition to `TERMINATED`. 3) Start the de-provisioning workflow: delete webhook registrations, revoke gateway credentials, close gateway sub-accounts. 4) Retain all financial and audit records.
**Alternates.** *E1* non-terminal payments exist → `422` listing up to 100 payment IDs and a total count. *E2* de-provisioning fails at a gateway → the merchant stays `TERMINATED` (the platform-side truth) and a reconciliation exception is raised for the orphaned gateway resource; termination is never blocked by a third party's availability.
**Postconditions.** No new payment is possible; historical records intact; connections de-provisioned or flagged as exceptions.
**Events.** `merchant.terminated.v1`, `audit.recorded.v1`.
**Validation.** L7.
**API.** `DELETE /v1/merchants/{merchantId}`.
**Traces.** BR-32.

### FR-15 — Right-to-erasure by crypto-shredding

**Statement.** On a verified erasure request, destroy the data key that encrypts the subject's personal data, rendering it unreadable, while retaining financial and audit records under the legal-obligation basis.
**Trigger.** Verified data-subject request via the compliance surface.
**Preconditions.** Merchant `TERMINATED`; retention obligations for the *personal* data have lapsed or the legal basis permits erasure; a compliance principal has approved.
**Main flow.** 1) Identify the envelope data keys covering the subject's personal fields and KYC artifacts. 2) Schedule KMS key-material deletion. 3) Overwrite in-place index entries derived from personal data (name, e-mail) with tombstones. 4) Write an erasure attestation to the audit log. 5) Confirm to the requester.
**Alternates.** *E1* an active legal hold exists → refuse with the hold reference; the refusal is itself auditable evidence of lawful processing. *E2* the same key protects records still within a retention obligation → refuse and escalate; key scoping must be per-purpose, and a scoping error is a defect to fix, not a reason to over-delete.
**Postconditions.** Personal data unreadable; ledger, payments and audit rows remain queryable in non-personal form; the attestation is permanent.
**Events.** `audit.recorded.v1`.
**Validation.** L4.
**API.** compliance endpoint.
**Traces.** BR-32, BR-37.

---

## 3. BC-3 — Onboarding (Automation plane)

### FR-16 — Submit the onboarding package with batch validation

**Statement.** Accept the complete onboarding document and return, in one response, every L2 rule that failed — never only the first.
**Trigger.** `POST /v1/merchants/{merchantId}/onboarding`.
**Preconditions.** Merchant in `CREATED` or `VALIDATION_FAILED`; `onboarding:write`; `Idempotency-Key` present.
**Main flow.** 1) L1 schema validation. 2) Run the full L2 rule set in accumulate mode. 3) If clean, create the `onb_` case, transition merchant `→ VALIDATING`, and start the workflow (FR-17). 4) Return `202` with the case representation.
**Alternates.** *E1* one or more L2 rules fail → `422` with `details[]` containing every `{ field, code, message }`, the stable rule ID (e.g. `L2.BENEFICIAL_OWNER_REQUIRED`) and a remediation string; merchant → `VALIDATION_FAILED`. *E2* merchant already has a live case → return the existing case with `200` (business-key idempotency, FR-17).
**Postconditions.** Exactly one case per merchant; merchant state reflects the outcome.
**Events.** `merchant.validated.v1` on success.
**Validation.** L1, L2.
**API.** `POST /v1/merchants/{merchantId}/onboarding`.
**Traces.** BR-04.

### FR-17 — Start the onboarding workflow under a business key

**Statement.** Start exactly one `merchant-onboarding@v1` instance per merchant; starting it again is a no-op that returns the existing instance.
**Trigger.** FR-16 step 3.
**Preconditions.** Merchant `VALIDATING`; no live instance for this `merchant_id`.
**Main flow.** 1) `INSERT` into `workflow_instances` with a unique index on `(workflow_name, business_key) WHERE state NOT IN ('COMPLETED','ABORTED')`. 2) On success, enqueue step 1. 3) Return `wfr_` id.
**Alternates.** *A1* unique-index conflict → load and return the existing instance; do not error. *E1* the merchant already completed onboarding → `409` unless a re-onboarding flag is set by an operator.
**Postconditions.** One live instance; concurrent starts converge on it.
**Events.** `audit.recorded.v1`.
**Validation.** L7.
**API.** internal.
**Traces.** BR-04.

### FR-18 — Correct and resubmit a failed validation

**Statement.** Allow a `VALIDATION_FAILED` merchant to be corrected and resubmitted without creating a new merchant or losing the failure history.
**Trigger.** `POST /v1/merchants/{merchantId}/onboarding` on a `VALIDATION_FAILED` merchant.
**Preconditions.** Merchant `VALIDATION_FAILED`.
**Main flow.** 1) Re-run FR-16. 2) On success, transition `VALIDATION_FAILED → VALIDATING`. 3) Append the new attempt to the case's attempt history; do not overwrite prior failure annotations.
**Alternates.** *E1* still failing → new annotations appended, state remains `VALIDATION_FAILED`.
**Postconditions.** Case carries an ordered history of submissions and their rule outcomes — this is what makes "why did onboarding take nine days?" answerable.
**Events.** `merchant.validated.v1` on success.
**Validation.** L1, L2.
**API.** as FR-16.
**Traces.** BR-04.

### FR-19 — Submit KYC/KYB to the vendor idempotently

**Statement.** Submit the verification package to the configured KYC provider using a stable vendor reference key so that retries never create a second vendor case.
**Trigger.** Workflow step 2 (`submit-kyc`).
**Preconditions.** Merchant `VALIDATING`; provider configured for the tenant's region.
**Main flow.** 1) Derive the vendor reference key deterministically from `(case_id, attempt_ordinal)`. 2) Call the provider through the ACL. 3) Persist the provider case reference. 4) Transition merchant `→ KYC_PENDING`. 5) Move to step 3 (`await-kyc-decision`).
**Alternates.** *A1* the provider reports the reference key already exists → adopt the existing case; this is a success, not a conflict. *E1* provider 5xx or timeout → retry 5× exponential 1 s→60 s (baseline §11); the step remains resumable. *E2* retries exhausted → step to DLQ (FR-32), instance `FAILED`, alert.
**Postconditions.** Exactly one vendor case per attempt ordinal; merchant `KYC_PENDING`.
**Events.** `audit.recorded.v1`.
**Validation.** L2, L3.
**API.** internal.
**Traces.** BR-05.

### FR-20 — Deliver a signal to a waiting workflow step

**Statement.** Deliver an external signal (KYC decision, compliance approval, bank verification result) to a workflow instance blocked on a wait step, exactly once in effect, with the signal itself audited.
**Trigger.** `POST /v1/merchants/{merchantId}/onboarding/signals/{signal}` or an inbound vendor webhook.
**Preconditions.** A live instance blocked on a step accepting `{signal}`; the caller holds the signal's required scope (e.g. `onboarding:approve`).
**Main flow.** 1) Authenticate and authorize. 2) Dedupe on `(instance_id, signal, external_ref)`. 3) Persist the signal payload as the step's result checkpoint. 4) Release the step; the worker resumes at the next step.
**Alternates.** *A1* duplicate signal → `200` with the prior outcome; no second release. *E1* no step is waiting for this signal → `409 INVALID_STATE_TRANSITION` naming the instance's current step. *E2* instance is `FAILED` or `ABORTED` → `409`.
**Postconditions.** Step result checkpointed before the next step begins; signal recorded with principal and timestamp.
**Events.** `audit.recorded.v1`.
**Validation.** L1, L7.
**API.** `POST /v1/merchants/{merchantId}/onboarding/signals/{signal}`.
**Traces.** BR-05, BR-19.

### FR-21 — Handle a KYC failure and resubmission

**Statement.** Record a negative KYC decision with its reason, transition the merchant to `KYC_FAILED`, notify, and permit a resubmission with a new document set.
**Trigger.** KYC decision signal with a negative verdict, or the 7-day wait timeout.
**Preconditions.** Merchant `KYC_PENDING`.
**Main flow.** 1) Persist the verdict, reason codes and evidence references. 2) Transition `→ KYC_FAILED`. 3) Emit `merchant.kyc_failed.v1`. 4) Leave the case resumable.
**Alternates.** *A1* resubmission → `KYC_FAILED → KYC_PENDING`, attempt ordinal increments, a new vendor reference key is derived (FR-19). *A2* the 7-day wait expires → escalate to an operator queue; the workflow does **not** auto-fail the merchant, because a slow vendor is not a failed applicant. *E1* terminate → `KYC_FAILED → TERMINATED`.
**Postconditions.** Every verdict and its evidence retained ≥ 5 years under Object Lock.
**Events.** `merchant.kyc_failed.v1`, `audit.recorded.v1`.
**Validation.** L2, L7.
**API.** internal + signal endpoint.
**Traces.** BR-05.

### FR-22 — Validate bank account structure

**Statement.** Validate every settlement account's checksum, length and country consistency as a pure, offline L2 rule.
**Trigger.** Workflow step 4 (`validate-bank-account`), and on any bank-account write.
**Preconditions.** Merchant `KYC_APPROVED`.
**Main flow.** 1) Normalise the account identifier. 2) Apply the scheme-appropriate checksum (IBAN mod-97, ABA routing checksum, BSB/CLABE/etc. per country). 3) Assert the account country is consistent with the merchant's registered country or an explicitly permitted corridor. 4) Store the account encrypted, masked to the last four characters everywhere else.
**Alternates.** *E1* checksum fails → `BANK_VALIDATION_FAILED` with the specific rule ID. *E2* country mismatch without a permitted corridor → `BANK_VALIDATION_FAILED`.
**Postconditions.** Account stored encrypted; only the mask is readable through any API.
**Events.** —
**Validation.** L2.
**API.** internal.
**Traces.** BR-06.

### FR-23 — Verify bank account ownership

**Statement.** Where the provider supports it, verify that the account belongs to the merchant's legal entity, as an impure L3 rule that never runs on the payment path.
**Trigger.** Workflow step 4 after FR-22 passes.
**Preconditions.** FR-22 passed; an ownership provider is configured for the account's country.
**Main flow.** 1) Call the provider (name-matching, micro-deposit or open-banking confirmation). 2) On confirmation, transition `KYC_APPROVED → BANK_VALIDATED`. 3) Emit `merchant.bank_validated.v1`.
**Alternates.** *A1* no provider for the country → mark `UNVERIFIED_BY_POLICY`, record the gap, and proceed if tenant policy permits; the gap is visible in the compliance report rather than silently ignored. *E1* mismatch → `BANK_VALIDATION_FAILED`; a **new** account may be supplied and the flow returns to `KYC_APPROVED` without redoing KYC. *E2* provider outage → retry 5× exponential; the step remains resumable.
**Postconditions.** Merchant `BANK_VALIDATED` or `BANK_VALIDATION_FAILED`.
**Events.** `merchant.bank_validated.v1`.
**Validation.** L3.
**API.** internal.
**Traces.** BR-06.

### FR-24 — Provision gateway accounts (fan-out)

**Statement.** Provision a gateway-side account or sub-account per selected gateway, concurrently, idempotently on an external reference, tolerating partial failure.
**Trigger.** Workflow step 5 (`provision-gateways`).
**Preconditions.** Merchant `BANK_VALIDATED`; ≥ 1 gateway selected; each selected gateway's capability descriptor covers the merchant's country and requested methods.
**Main flow.** 1) Transition `→ GATEWAY_PROVISIONING`. 2) Fan out one activity per gateway, each with its own 60 s timeout and 5× exponential retry. 3) Each activity derives an external reference from `(merchant_id, gateway_id)` and passes it to the adapter's `Provision`. 4) On success, create a `gwc_` connection row in state `PROVISIONED`. 5) When all branches settle, transition `→ CONFIGURING` if all succeeded.
**Alternates.** *A1* the gateway reports the external reference already exists → adopt the existing account. *E1* one branch fails after retries → merchant `→ PROVISIONING_FAILED`; successful branches are **not** rolled back (the case can be retried per gateway), but aborting the whole case does compensate them (FR-31). *E2* the adapter's response fails L6 shape validation → treat as a provisioning failure, not as a success with odd data.
**Postconditions.** One connection row per successfully provisioned gateway, each with its external reference.
**Events.** `merchant.gateway_provisioned.v1` per gateway.
**Validation.** L3, L6.
**API.** internal.
**Traces.** BR-09, BR-10.

### FR-25 — Store credentials and register webhooks

**Statement.** Write gateway credentials to the secrets store under the tenant-scoped path and register the platform's webhook endpoint with each gateway, both idempotently and both compensatable.
**Trigger.** Workflow steps 6 and 7.
**Preconditions.** Connection in `PROVISIONED`.
**Main flow.** 1) Write credential material to `/{env}/{tenant}/{merchant}/{gateway}`, encrypted with the tenant's CMK; retain only the returned `secretRef` on the connection row. 2) Register the webhook URL and capture the gateway's signing secret, stored the same way. 3) Persist the webhook registration reference.
**Alternates.** *A1* a webhook registration already exists for this URL → adopt it. *E1* secrets write fails → retry 3× exponential, then compensate (`delete secret version`). *E2* webhook registration fails → retry 5×, then compensate (`delete webhook registration`).
**Postconditions.** No credential material anywhere outside the secrets store; `Secret[T]` wrapping in memory; nothing logged.
**Events.** `audit.recorded.v1` (references only, never material).
**Validation.** L3.
**API.** internal.
**Traces.** BR-11.

### FR-26 — Apply the initial configuration

**Statement.** Compose and publish the merchant's first configuration version from the onboarding submission plus tenant defaults, validated at L4.
**Trigger.** Workflow step 8 (`apply-configuration`).
**Preconditions.** Merchant `CONFIGURING`; ≥ 1 connection with stored credentials.
**Main flow.** 1) Merge tenant defaults with the merchant's requested methods, currencies, countries, routing preference, limits and webhook endpoints. 2) Run L4 validation against the gateways' capability descriptors. 3) Persist as version 1 with status `ACTIVE`. 4) Publish `configuration.published.v1`.
**Alternates.** *E1* L4 fails → merchant `→ CONFIGURATION_FAILED` with the failing rule IDs; the submission can be corrected and the step retried. *E2* concurrent publish → version conflict; the workflow retries with the new base version.
**Postconditions.** Exactly one `ACTIVE` configuration version; data-plane caches primed.
**Events.** `configuration.published.v1`.
**Validation.** L4.
**API.** internal (equivalent to `PUT /v1/merchants/{merchantId}/configuration`).
**Traces.** BR-07, BR-08, BR-13, BR-14, BR-15.

### FR-27 — Run sandbox validation

**Statement.** Execute a smoke suite against each connection in the gateway's sandbox, proving credentials, connectivity, a basic authorize/capture and webhook receipt.
**Trigger.** Workflow step 9 (`sandbox-validation`).
**Preconditions.** Merchant `SANDBOX_VALIDATION`; connections have credentials and webhook registrations.
**Main flow.** 1) Assert the adapter is in sandbox mode; refuse to proceed against a production endpoint. 2) Execute the smoke set per connection. 3) Record a run record with per-assertion outcomes. 4) On full pass, transition `→ CERTIFICATION`.
**Alternates.** *E1* any assertion fails → merchant `→ CONFIGURATION_FAILED` with the failing assertion and its remediation. *E2* the 15 min timeout expires → retry once, then fail the step.
**Postconditions.** A durable sandbox run record; no production endpoint contacted.
**Events.** `audit.recorded.v1`.
**Validation.** L3, L6.
**API.** internal; `platformctl certify --sandbox`.
**Traces.** BR-16.

### FR-28 — Run the certification suite and produce a signed report

**Statement.** Execute the full assertion matrix (baseline §11.4) for every `(gateway, payment_method, currency)` triple in the merchant's configuration and produce a signed, immutable `CertificationReport`.
**Trigger.** Workflow step 10 (`certification`), or `platformctl certify`.
**Preconditions.** Sandbox validation passed; the configuration enumerates the triples.
**Main flow.** 1) Enumerate the triples from configuration ∩ capability descriptors. 2) For each, assert: authorize→capture→refund; authorize→void; declined test card maps to a normalized `DECLINED` reason; signed webhook received and moves state; 3DS reaches `REQUIRES_ACTION` and completes; duplicate idempotency key returns the same result; amount/currency echo matches. 3) Write the report to object storage, hash it, sign it, and reference it from the merchant record. 4) Transition `CERTIFICATION → APPROVED`; mark each covered connection `CERTIFIED`.
**Alternates.** *A1* delta run — a configuration change adding a triple certifies only the new triples, invalidating certification for those triples until the run passes. *E1* any assertion fails → `CERTIFICATION_FAILED` with the failing assertion; recoverable to `CERTIFICATION` or `CONFIGURING`. *E2* the 30 min timeout expires → retry once, then fail.
**Postconditions.** `PRODUCTION_READY` is unreachable without a passing report; report is immutable and referenced.
**Events.** `merchant.certified.v1`.
**Validation.** L3, L6, L7.
**API.** internal; `platformctl certify`.
**Traces.** BR-17, BR-09.

### FR-29 — Clear the manual compliance gate

**Statement.** Block the workflow at step 11 until a principal holding `onboarding:approve` records a decision, with optional four-eyes requiring two distinct principals.
**Trigger.** Workflow reaches step 11; cleared by FR-20.
**Preconditions.** Merchant `APPROVED`; case at the gate.
**Main flow.** 1) Present the review package: KYC evidence references, bank verification result, certification report, configuration diff, risk profile. 2) Await the signal. 3) On approve, transition `APPROVED → PRODUCTION_READY`. 4) Record principal, decision, reason and evidence reference in the hash-chained audit log.
**Alternates.** *A1* four-eyes enabled → the second approval must come from a different principal; a repeat from the same principal is rejected with `422`. *A2* 5-day SLA breach → escalate to a named queue; **no timer may auto-approve**. *E1* rejection → the case branches to remediation or `TERMINATED` per the reason code; never a silent stall.
**Postconditions.** An immutable, attributable compliance decision exists for every activation.
**Events.** `audit.recorded.v1`.
**Validation.** L7.
**API.** `POST /v1/merchants/{merchantId}/onboarding/signals/compliance-review`.
**Traces.** BR-19.

### FR-30 — Query onboarding case status

**Statement.** Return the case's current step, per-step history with durations and outcomes, blocking reason, and remediation guidance.
**Trigger.** `GET /v1/merchants/{merchantId}/onboarding`.
**Preconditions.** `onboarding:read`.
**Main flow.** 1) Load case and step history. 2) Project into a client-facing view that names the current blocker in business terms ("awaiting KYC provider decision, submitted 2 days ago") rather than internal step names alone.
**Alternates.** *A1* no case → `404`. *A2* case `FAILED` → include the DLQ reference and the error chain summary, redacted of vendor internals.
**Postconditions.** — (read-only)
**Events.** —
**Validation.** L1.
**API.** `GET /v1/merchants/{merchantId}/onboarding`.
**Traces.** BR-04.

### FR-31 — Abort a case and compensate in reverse order

**Statement.** Aborting an onboarding case runs the compensation of every completed step in strict reverse order, tolerating individual compensation failures without abandoning the rest.
**Trigger.** Operator abort, or `→ TERMINATED` on a merchant with a live case.
**Preconditions.** A live instance exists.
**Main flow.** 1) Mark the instance `ABORTING`. 2) Walk completed steps in reverse; run each compensation with its own retry policy: `activate → suspend merchant`; `apply-configuration → roll back to previous version`; `register-webhooks → delete registration`; `store-credentials → delete secret version`; `provision-gateways → de-provision sub-account`; `submit-kyc → cancel KYC case`. 3) Mark `ABORTED`.
**Alternates.** *A1* a compensation is not applicable (step never completed) → skip. *E1* a compensation exhausts retries → record a reconciliation exception for the orphaned external resource and **continue** with the remaining compensations; a stuck third party must not prevent local cleanup. *E2* abort is requested twice → idempotent.
**Postconditions.** No orphaned external resource goes unrecorded; local state is consistent.
**Events.** `audit.recorded.v1`.
**Validation.** L7.
**API.** operator endpoint.
**Traces.** BR-10, BR-32.

### FR-32 — Resume a workflow instance after a worker crash, and park exhausted steps

**Statement.** A workflow instance whose worker dies is leased by another worker and resumes from the last checkpointed step result, replaying no completed step; a step that exhausts its retries parks the instance and its payload in the DLQ with the full error chain.
**Trigger.** Lease expiry detected by any `workflow-worker`; or retry exhaustion on a step.
**Preconditions.** Instance in `RUNNING` with an expired lease, or a step at its retry ceiling.
**Main flow (resume).** 1) Workers poll `workflow_instances` with `FOR UPDATE SKIP LOCKED WHERE lease_expires_at < now()`. 2) The winner extends the lease and loads the step history. 3) Execution continues from the first step with no committed result — every earlier step's *result* is read from the checkpoint, not re-executed. 4) Heartbeats extend the lease while the step runs.
**Main flow (DLQ).** 1) Step exceeds its retry budget. 2) Persist the step payload, the full wrapped error chain and the attempt history to `workflow_dlq`. 3) Instance `→ FAILED`. 4) Emit an alert. 5) An operator can repair the input and replay the step, which resumes the instance from that point.
**Alternates.** *A1* the step is idempotent and its side effect committed but the checkpoint did not (crash in the gap) → re-execution is safe by the step's idempotency key; this is exactly why every step in baseline §11 declares idempotency. *E1* a non-idempotent step (none exist by design) would require operator adjudication; the engine refuses to auto-resume such a step and raises a manual-intervention exception.
**Postconditions.** At-most-once *effect* per step despite at-least-once execution; no instance is lost by a pod dying.
**Events.** `audit.recorded.v1`.
**Validation.** L7.
**API.** internal; `platformctl workflow replay`.
**Traces.** BR-04, BR-10.

---

## 4. BC-4 — Gateway Registry & Integration (Control + Data)

### FR-33 — Register a gateway and publish its capability descriptor

**Statement.** Register a gateway with a versioned capability descriptor that is the single source of truth for countries, currencies, payment methods, operations, 3DS support, partial-capture support, refund window and webhook signature scheme.
**Trigger.** Platform-operator call or a repository-managed descriptor deploy.
**Preconditions.** An adapter implementing the gateway SPI exists and passes the adapter contract suite.
**Main flow.** 1) L4-validate the descriptor (well-formed ISO codes, non-empty operation set, declared signature scheme is implemented). 2) Assert the adapter contract suite passes against the descriptor. 3) Persist with a version. 4) Publish so routing, L4 configuration validation and certification matrices all read the same data.
**Alternates.** *E1* the adapter's behaviour disagrees with the descriptor (contract-suite failure) → refuse registration; a descriptor that lies is worse than no descriptor.
**Postconditions.** Exactly one authoritative capability source per gateway.
**Events.** `audit.recorded.v1`.
**Validation.** L4.
**API.** `GET /v1/gateways`, `GET /v1/gateways/{gatewayId}` (read); operator endpoint (write).
**Traces.** BR-34.

### FR-34 — Change a descriptor with a live-dependency guard

**Statement.** A descriptor change that removes a capability on which live merchant configurations depend is refused, naming the affected merchants.
**Trigger.** Descriptor update.
**Preconditions.** Gateway registered.
**Main flow.** 1) Diff old vs. new descriptor. 2) For each removed capability, query configurations that reference it. 3) If any live merchant depends on it, refuse. 4) Otherwise persist the new version and publish.
**Alternates.** *A1* additive-only change → applied without a guard check. *A2* forced removal with an explicit `--deprecate` flag → capability marked deprecated with a sunset date, affected merchants notified, removal permitted only after the sunset. *E1* refusal → `422 CONFIGURATION_INVALID` with up to 100 affected merchant IDs and a count.
**Postconditions.** No live merchant is silently broken by a registry change.
**Events.** `audit.recorded.v1`.
**Validation.** L4.
**API.** operator endpoint.
**Traces.** BR-34.

### FR-35 — Create and track a gateway connection

**Statement.** Maintain a connection per `(merchant, gateway)` carrying the external account reference, credential reference, webhook registration reference and certification status.
**Trigger.** FR-24/FR-25 during onboarding, or an operator adding a gateway to a live merchant.
**Preconditions.** Merchant exists; gateway registered; descriptor covers the merchant's country.
**Main flow.** 1) Create the `gwc_` row in `PENDING`. 2) Advance through `PROVISIONED → CONFIGURED → CERTIFIED` as the corresponding steps complete. 3) Expose the connection and its status through the control API.
**Alternates.** *A1* adding a gateway to an already-`ACTIVE` merchant → runs the same provision/credential/webhook/certify sub-flow without disturbing live traffic on existing connections. *E1* de-provisioning a connection that is the merchant's only `CERTIFIED` one → refused unless the merchant is being suspended or terminated; a merchant must never be `ACTIVE` with zero routable gateways.
**Postconditions.** Connection state accurately reflects provisioned reality; historical payments remain resolvable against a de-provisioned connection.
**Events.** `merchant.gateway_provisioned.v1`.
**Validation.** L3, L7.
**API.** control-plane connection endpoints.
**Traces.** BR-09, BR-10.

### FR-36 — Compute and publish gateway health

**Statement.** Maintain health per `(gateway_id, operation)` using the state machine of baseline §10 and publish every transition.
**Trigger.** Every gateway call outcome; and a cool-down timer.
**Preconditions.** Gateway registered.
**Main flow.** 1) Record outcome and latency into a 30 s sliding window. 2) `HEALTHY → DEGRADED` when error rate > 5 % over 30 s with ≥ 20 samples. 3) `DEGRADED → UNHEALTHY` when error rate > 25 % or p99 > 5 s; open the circuit. 4) After a 30 s cool-down, `→ PROBING` (half-open). 5) Three consecutive successes → `HEALTHY`; any failure → `UNHEALTHY` with the cool-down doubled, capped at 5 min. 6) Publish `gateway.health_changed.v1` on every transition.
**Alternates.** *A1* sample count below the minimum → remain in the current state; a single failure must not open a circuit. *A2* per-merchant contractual pinning overrides routing but not health measurement — health stays per-gateway because per-merchant samples are statistically meaningless. *E1* Kafka unavailable → health remains locally correct and is gossiped when the broker returns; health is an AP concern (baseline §15).
**Postconditions.** Routing sees health within seconds; the control plane records the history.
**Events.** `gateway.health_changed.v1`.
**Validation.** —
**API.** `GET /v1/gateways/{gatewayId}/health`.
**Traces.** BR-35.

### FR-37 — Operator health override with mandatory expiry

**Statement.** Allow an operator to force a gateway's circuit open (drain), force it closed, or pin it healthy, with a mandatory expiry after which the override auto-reverts.
**Trigger.** `POST /v1/gateways/{gatewayId}/health:override`.
**Preconditions.** Caller holds the operator role; `expiresAt` present and ≤ 72 h in the future.
**Main flow.** 1) Validate the expiry bound (default 4 h). 2) Persist the override with actor, reason and expiry. 3) Publish `gateway.health_changed.v1` reflecting the effective state. 4) A scheduled job reverts expired overrides and republishes.
**Alternates.** *E1* missing or excessive expiry → `422`; an unbounded override is how a gateway stays drained for six months. *E2* forcing closed a gateway whose measured state is `UNHEALTHY` → permitted but flagged prominently on the incident dashboard and audited as a risk acceptance.
**Postconditions.** Override is time-bounded, audited, and visible.
**Events.** `gateway.health_changed.v1`, `audit.recorded.v1`.
**Validation.** L1, L4.
**API.** `POST /v1/gateways/{gatewayId}/health:override`.
**Traces.** BR-35.

### FR-38 — Rotate gateway credentials with a dual-run overlap

**Statement.** Rotate a connection's gateway credentials such that both the old and the new credential are usable during an overlap window, with zero payment failures attributable to the rotation.
**Trigger.** `POST /v1/gateways/{gatewayId}/credentials:rotate`, or the ≤ 90 day scheduler.
**Preconditions.** Connection `CERTIFIED` or `CONFIGURED`; no rotation in flight for this connection.
**Main flow.** 1) Create the new credential at the gateway. 2) Store it as a new secret version; mark it `PENDING`. 3) Run L3 validation with the new credential (a read-only gateway call). 4) On success, promote the new version to `PRIMARY` and mark the old `SECONDARY` with `valid_until = now + overlap` (default 30 min, configurable per gateway). 5) In-flight requests already holding the old reference complete normally; new requests resolve to `PRIMARY`. 6) After the window, revoke the old credential at the gateway and delete the secret version. 7) Verify revocation by a negative-path call.
**Alternates.** *A1* the gateway does not support two concurrent credentials → the adapter declares this in its descriptor and the rotation uses a short maintenance window with request draining instead, scheduled outside the merchant's peak hours. *E1* L3 validation of the new credential fails → the old credential remains `PRIMARY`, the new version is deleted, rotation is marked `FAILED`, alert raised — never a partial cutover (FR-39). *E2* revocation fails at step 6 → retry; on exhaustion raise a reconciliation exception for a live credential that should be dead. This is a security finding, not a routine error.
**Postconditions.** Exactly one `PRIMARY` credential; at most one `SECONDARY` within its window; credential age metric reset.
**Events.** `audit.recorded.v1` (references only).
**Validation.** L3.
**API.** `POST /v1/gateways/{gatewayId}/credentials:rotate`.
**Traces.** BR-12, BR-11.

### FR-39 — Roll back a failed credential rotation

**Statement.** A rotation that fails at any step leaves the connection on its previous working credential, with the partial artifacts cleaned up.
**Trigger.** Any failure inside FR-38.
**Preconditions.** A rotation is in flight.
**Main flow.** 1) Determine the furthest completed step. 2) Compensate in reverse: revoke the newly created gateway credential; delete the pending secret version; restore `PRIMARY` to the prior version. 3) Mark the rotation `FAILED` with the failing step and error chain. 4) Alert.
**Alternates.** *E1* the compensating revoke fails → the new credential exists but is unused; raise a security-severity reconciliation exception naming it. Unused-but-live credentials must never be forgotten.
**Postconditions.** Payments continue on the old credential throughout; no window in which neither credential works.
**Events.** `audit.recorded.v1`.
**Validation.** L3.
**API.** internal.
**Traces.** BR-12.

### FR-40 — Execute a gateway operation through the adapter

**Statement.** Every gateway interaction goes through the adapter ACL with a hard 8 s timeout, a per-gateway bulkhead, a circuit breaker and the attempt's deterministic gateway idempotency key.
**Trigger.** Orchestrator dispatch (FR-63), certification, provisioning, credential validation.
**Preconditions.** Circuit for `(gateway, operation)` is not `OPEN`; a bulkhead slot is available; the attempt row is already persisted.
**Main flow.** 1) Acquire a bulkhead slot (per-gateway semaphore). 2) Resolve credentials via `secretRef` into a `Secret[T]`. 3) Translate the domain request into the gateway's wire format inside the ACL — no gateway type crosses into `internal/domain`. 4) Attach `gateway_idempotency_key = base32(HMAC-SHA256(attempt_id, gateway_salt))[:32]`. 5) Call with an 8 s deadline. 6) Record latency and outcome into the health window (FR-36).
**Alternates.** *A1* transport retry to the **same** gateway reuses the same key — the gateway dedupes, so a retry cannot double-charge. *E1* circuit `OPEN` → fail fast with `GATEWAY_CIRCUIT_OPEN`, classified retryable, so routing moves to the next candidate without burning the 8 s budget. *E2* bulkhead full → fail fast with `GATEWAY_CIRCUIT_OPEN` semantics; a slow gateway must not consume the orchestrator's goroutines or the ingress pool. *E3* deadline exceeded → `TIMEOUT_UNKNOWN` (FR-66).
**Postconditions.** Latency and outcome recorded; no credential material logged.
**Events.** —
**Validation.** L6 on the response.
**API.** internal.
**Traces.** BR-09, BR-11, BR-20.

### FR-41 — Validate the gateway response (L6)

**Statement.** Every gateway response is validated for signature (where applicable), schema shape, and echoed amount/currency before it is allowed to change domain state.
**Trigger.** Any adapter response.
**Preconditions.** A response was received.
**Main flow.** 1) Verify the response signature where the gateway signs synchronous responses. 2) Validate against the adapter's response schema. 3) Assert the echoed amount and currency equal what we sent, in minor units. 4) Map the gateway's status and reason code into the normalized domain outcome and the soft/hard decline classification.
**Alternates.** *E1* signature or schema invalid → `502 GATEWAY_CONTRACT_VIOLATION`; the attempt is `ERROR` (safe to retry), not `SUCCESS`. *E2* amount or currency mismatch → `502 GATEWAY_CONTRACT_VIOLATION` and a **critical** alert: the gateway charged a different amount than we asked for, which is a money-correctness incident. *E3* unmapped reason code → classify as a **hard** decline (fail safe) and alert so the mapping can be completed (FR-65).
**Postconditions.** No unvalidated gateway data ever reaches the domain layer.
**Events.** —
**Validation.** L6.
**API.** internal.
**Traces.** BR-20, BR-23, BR-34.

### FR-42 — Detect and reconcile provisioning drift

**Statement.** A scheduled reconciliation compares desired gateway connections against the gateways' actual accounts and webhook registrations, raising exceptions on divergence.
**Trigger.** Scheduled run (default hourly per gateway).
**Preconditions.** Gateway adapter supports enumeration or per-connection lookup.
**Main flow.** 1) For each active connection, fetch the gateway-side account and webhook registration. 2) Compare against desired state. 3) On divergence — missing account, missing/incorrect webhook URL, unexpected extra account — raise a typed reconciliation exception with a remediation runbook reference.
**Alternates.** *A1* self-healing is enabled for low-risk divergence (webhook URL drift) → re-register automatically and record the correction. *E1* an orphaned gateway account with no local connection → security-severity exception; this is either a failed compensation or an unauthorised action.
**Postconditions.** Desired and actual converge, or the gap is visible and owned.
**Events.** `audit.recorded.v1`.
**Validation.** L3.
**API.** internal; `platformctl reconcile connections`.
**Traces.** BR-10, BR-34.

---

## 5. BC-5 — Configuration & Policy (Control plane)

### FR-43 — Read the current configuration

**Statement.** Return the merchant's active configuration document with an `ETag` and its version number.
**Trigger.** `GET /v1/merchants/{merchantId}/configuration`.
**Preconditions.** `config:read`.
**Main flow.** 1) Load the active version. 2) Redact `secretRef` values to opaque references. 3) Return the document, `version`, and an `ETag` derived from the version.
**Alternates.** *A1* `?version=n` → return that historical version, read-only. *E1* no configuration yet → `404`.
**Postconditions.** —
**Events.** —
**Validation.** L1.
**API.** `GET /v1/merchants/{merchantId}/configuration`.
**Traces.** BR-33.

### FR-44 — Publish a configuration version

**Statement.** Validate (L4), version, persist, audit with a structured diff, and publish a new configuration document; the write is `If-Match`-guarded and idempotent.
**Trigger.** `PUT /v1/merchants/{merchantId}/configuration`.
**Preconditions.** `config:write`; `If-Match` matches the current version; `Idempotency-Key` present.
**Main flow.** 1) L1 then L4 validation in accumulate mode. 2) Assign `version = current + 1`. 3) Persist the full new document, retaining the prior document verbatim. 4) Write the audit record with actor and a field-level diff. 5) Write `configuration.published.v1` to the outbox in the same transaction. 6) Return `200` with the new `ETag`.
**Alternates.** *E1* L4 failure → `422 CONFIGURATION_INVALID` with every failing rule ID. *E2* `If-Match` stale → `412`. *E3* concurrent publish wins the version → `409 CONFIGURATION_VERSION_CONFLICT` with the current version so the client can rebase.
**Postconditions.** History is strictly append-only; the previous document is never mutated or deleted.
**Events.** `configuration.published.v1`.
**Validation.** L1, L4.
**API.** `PUT /v1/merchants/{merchantId}/configuration`.
**Traces.** BR-33, BR-07, BR-08, BR-13, BR-14, BR-15.

### FR-45 — List configuration versions and diffs

**Statement.** Return the version history with actor, timestamp, validation outcome and a structured diff against the preceding version.
**Trigger.** `GET /v1/merchants/{merchantId}/configuration/versions`.
**Preconditions.** `config:read`.
**Main flow.** 1) Load versions, newest first, cursor-paginated. 2) Compute or read the cached diff per adjacent pair. 3) Return with actor attribution.
**Alternates.** *A1* `?from=&to=` → return the diff between two arbitrary versions.
**Postconditions.** —
**Events.** —
**Validation.** L1.
**API.** `GET /v1/merchants/{merchantId}/configuration/versions`.
**Traces.** BR-33.

### FR-46 — Roll back configuration

**Statement.** Republish a prior configuration document **as a new version**, never by deleting or reverting in place.
**Trigger.** `POST /v1/merchants/{merchantId}/configuration/rollback` with a target version.
**Preconditions.** `config:write`; target version exists; `Idempotency-Key` present.
**Main flow.** 1) Load the target document. 2) Re-run L4 validation against *current* capability descriptors — a document valid six months ago may reference a since-removed capability. 3) Persist as `version = current + 1` with `rolled_back_from` provenance. 4) Publish `configuration.rolled_back.v1`.
**Alternates.** *E1* the target no longer validates → `422` naming what changed underneath; the operator must edit rather than blindly restore. This is the case naive rollback implementations get wrong. *E2* target version is the current version → `200` no-op.
**Postconditions.** Rollback is itself an auditable forward version; history remains append-only.
**Events.** `configuration.rolled_back.v1`.
**Validation.** L4.
**API.** `POST /v1/merchants/{merchantId}/configuration/rollback`.
**Traces.** BR-33, BR-13.

### FR-47 — Propagate configuration to the data plane

**Statement.** A published configuration takes effect in every data-plane instance within p99 ≤ 30 s, via Kafka invalidation plus a bounded-staleness cache.
**Trigger.** `configuration.published.v1` / `configuration.rolled_back.v1` / `merchant.suspended.v1`.
**Preconditions.** Data plane is consuming `pp.config.configuration.v1`.
**Main flow.** 1) Each data-plane instance consumes the invalidation. 2) It evicts the merchant's cached snapshot. 3) The next request for that merchant loads the fresh snapshot from the control plane read path. 4) `pp_config_snapshot_age_seconds` is exported per service.
**Alternates.** *A1* `merchant.suspended.v1` → priority path, processed ahead of the normal invalidation queue; a suspended merchant must stop taking payments now, not in 30 s. *E1* consumer lag exceeds the SLO → alert at > 5 min; a synthetic probe publishes a marker config change and measures end-to-end propagation continuously.
**Postconditions.** Data-plane behaviour reflects the published configuration within the SLO.
**Events.** —
**Validation.** —
**API.** internal.
**Traces.** BR-33, BR-31.

### FR-48 — Serve fail-static configuration during a control-plane outage

**Statement.** When the control plane is unreachable, the data plane continues on its last-known-good configuration snapshot; past a defined staleness cliff it fails closed for *new* merchants while continuing to serve existing ones.
**Trigger.** Control-plane read failure or invalidation-stream stall.
**Preconditions.** A cached snapshot exists.
**Main flow.** 1) On a cache miss with the control plane down, serve the last-known-good snapshot and increment a degradation counter. 2) Export snapshot age. 3) Alert at age > 5 min. 4) At age > `max_config_staleness` (default 15 min), refuse payments for merchants with **no** cached snapshot (`503 SERVICE_UNAVAILABLE`) while continuing to serve merchants that do have one.
**Alternates.** *E1* no cached snapshot and the control plane is down → `503`, retryable, with `Retry-After`. Never fail open: processing without limits is a compliance breach, and processing with guessed limits is worse.
**Postconditions.** Graceful degradation with a defined cliff, not an undefined one.
**Events.** —
**Validation.** —
**API.** internal.
**Traces.** BR-14, BR-33.

### FR-49 — Configure payment methods, currencies and countries

**Statement.** Validate the merchant's declared methods, currencies and countries against the intersection of the selected gateways' capability descriptors and the merchant's jurisdictional constraints.
**Trigger.** FR-44 with changes to these fields.
**Preconditions.** ≥ 1 gateway connection.
**Main flow.** 1) Compute the supported set as the union over eligible connections of each descriptor's capabilities. 2) Assert every declared method/currency/country is in that set. 3) Assert currency codes are ISO 4217 with the correct minor-unit exponent. 4) Mark newly added `(gateway, method, currency)` triples as requiring a delta certification run before they are live.
**Alternates.** *E1* an unsupported method → `422 CONFIGURATION_INVALID` naming the method and which gateways would be required to support it. *E2* an unknown or non-ISO currency → `422 CURRENCY_NOT_SUPPORTED`. *E3* a blocked country in `countries` → `422`.
**Postconditions.** The configuration cannot express a combination that no gateway can execute.
**Events.** `configuration.published.v1`.
**Validation.** L4.
**API.** via FR-44.
**Traces.** BR-07, BR-08, BR-17.

### FR-50 — Configure the routing policy

**Statement.** Validate and store a declarative routing policy: strategy, primary, ordered fallback, conditional rules and scoring weights.
**Trigger.** FR-44 with routing changes.
**Preconditions.** Named gateways have `CERTIFIED` connections for the relevant triples.
**Main flow.** 1) Assert every named gateway has a `CERTIFIED` connection. 2) Assert scoring weights are non-negative and sum to 1.0. 3) Assert every conditional rule's predicate is satisfiable given the merchant's configured currencies, methods and countries — an unreachable rule is a defect, not a harmless no-op. 4) Assert the fallback list contains no duplicates and does not name the primary.
**Alternates.** *E1* a named gateway is not certified → `422` naming it. *E2* weights do not sum to 1.0 → `422`. *E3* unsatisfiable rule → `422` with the rule index and why it can never match.
**Postconditions.** Routing policy is executable and total: some candidate is produced for every payment the configuration permits.
**Events.** `configuration.published.v1`.
**Validation.** L4.
**API.** via FR-44.
**Traces.** BR-13, BR-09.

### FR-51 — Configure limits, risk policy and SCA thresholds

**Statement.** Validate and store transaction limits, velocity limits, blocked countries, 3DS thresholds and refund/capture windows.
**Trigger.** FR-44 with risk/limit changes.
**Preconditions.** —
**Main flow.** 1) Assert `require3DSAbove ≤ maxTransactionAmount`; assert both are in a configured currency. 2) Assert `dailyVolumeLimit ≥ maxTransactionAmount`. 3) Assert velocity limits are positive integers. 4) Assert `maxRefundWindowDays` does not exceed the minimum refund window across the merchant's certified gateways — promising a 180-day refund window on a gateway that allows 120 is a promise we cannot keep. 5) Assert `blockedCountries` are valid ISO 3166-1 alpha-2.
**Alternates.** *E1* refund window exceeds a gateway's capability → `422` naming the gateway and its window. *E2* `require3DSAbove` in an unconfigured currency → `422 CURRENCY_NOT_SUPPORTED`.
**Postconditions.** Limits are internally consistent and externally achievable.
**Events.** `configuration.published.v1`.
**Validation.** L4.
**API.** via FR-44.
**Traces.** BR-14, BR-15, BR-25.

### FR-52 — Configure webhook endpoints and settlement preferences

**Statement.** Validate and store outbound webhook endpoints (URL, event selectors, secret reference, retry policy) and settlement preferences (schedule, currency, hold days).
**Trigger.** FR-44 with webhook/settlement changes.
**Preconditions.** —
**Main flow.** 1) Assert every URL is HTTPS with a resolvable public host; reject private/link-local/metadata addresses (SSRF guard). 2) Assert event selectors match known event types or valid wildcards. 3) Generate or accept a signing secret, stored by reference only. 4) Assert the settlement currency is in `supportedCurrencies` and the schedule is one the gateway supports.
**Alternates.** *A1* endpoint verification — send a signed challenge and require a correct echo before the endpoint receives real events. *E1* a private-range or metadata-service URL → `422`; this is the classic SSRF vector in webhook configuration. *E2* an unknown event selector → `422` listing valid selectors.
**Postconditions.** No webhook endpoint can be used to probe internal networks; secrets never returned by any read.
**Events.** `configuration.published.v1`.
**Validation.** L4.
**API.** via FR-44.
**Traces.** BR-12, BR-30, BR-33.

---

## 6. BC-6 — Payment Orchestration (Data plane)

### FR-53 — Create a payment (main flow)

**Statement.** Accept a payment instruction from an active merchant and execute it through the ordered pipeline of baseline §12, returning a definitive outcome, an actionable next step, or an honest `processing`.
**Trigger.** `POST /v1/payments`.
**Preconditions.** Merchant `ACTIVE`; token-based payment method reference; `Idempotency-Key` present.
**Main flow.** 1) Stages 1–6 (edge, request ID, authn, tenant, authz, rate limit). 2) L1 schema validation including the PAN detector. 3) Idempotency claim (FR-54). 4) Merchant context load from cache (FR-59). 5) L5 payment validation (FR-60). 6) Risk evaluation (FR-61). 7) Routing plan construction and persistence (FR-62). 8) Create and persist the `att_` attempt row **before** the gateway call. 9) Dispatch (FR-63). 10) L6 response validation (FR-41). 11) L7 state transition plus outbox write in one transaction. 12) Idempotency completion with a stored response snapshot. 13) Respond.
**Alternates.** *A1* auto-capture method → `PROCESSING → CAPTURED` directly, skipping `AUTHORIZED`. *A2* 3DS required → `REQUIRES_ACTION` with the redirect/challenge payload (FR-68). *A3* asynchronous method → `PENDING` (FR-73). *E1* any pipeline stage fails → the corresponding error from baseline §20 with `retryable` set correctly; the payment is left in a state consistent with how far it got, never in a fabricated one.
**Postconditions.** `payments` row, ≥ 1 `payment_attempts` row, one `routing_plans` row, one `idempotency_records` row, ≥ 2 outbox rows, ledger entries for any money-affecting outcome.
**Events.** `payment.created.v1`, `payment.attempted.v1`, then one of `payment.authorized.v1` / `payment.captured.v1` / `payment.failed.v1`.
**Validation.** L1, L5, L6, L7.
**API.** `POST /v1/payments`.
**Traces.** BR-20, BR-21, BR-24.

### FR-54 — Claim idempotency

**Statement.** Claim the idempotency scope `(tenant_id, merchant_id, method, path_template, idempotency_key)` atomically in PostgreSQL before any side effect, recording a request fingerprint.
**Trigger.** Baseline §12 stage 8, on every mutating endpoint.
**Preconditions.** `Idempotency-Key` header present, 1–255 characters.
**Main flow.** 1) Canonicalize the body (JCS: sorted keys, no insignificant whitespace) and compute `SHA-256` over it plus the scope tuple. 2) `INSERT … ON CONFLICT DO NOTHING` into `idempotency_records` with state `IN_FLIGHT` and `lease_expires_at = now + lease_ttl`. 3) If inserted, proceed. 4) On completion, write the response snapshot and set `COMPLETED` or `FAILED_TERMINAL` in the same transaction as the state change.
**Alternates.** *E1* header absent → `400 IDEMPOTENCY_KEY_REQUIRED`. *A1* conflict → dispatch to FR-55/FR-56/FR-57/FR-58 by the existing record's state and fingerprint. *A2* Redis mirror hit for a `COMPLETED` record → serve the replay from Redis; a Redis miss or total outage costs latency, never correctness — PostgreSQL remains authoritative.
**Postconditions.** At most one execution per scope tuple, ever, within the retention window (7 days, must exceed the longest client retry window).
**Events.** —
**Validation.** L1.
**API.** all mutating endpoints.
**Traces.** BR-21.

### FR-55 — Concurrent duplicate idempotent request

**Statement.** When a second request arrives for a scope whose record is `IN_FLIGHT` with a live lease, reject it immediately with `409 IDEMPOTENT_REQUEST_IN_PROGRESS` and `Retry-After: 1`. Do not block; do not process twice.
**Trigger.** FR-54 conflict where the existing record is `IN_FLIGHT` and `lease_expires_at > now()`.
**Preconditions.** Fingerprints match (a mismatch is FR-56).
**Main flow.** 1) Read the existing record. 2) Confirm the lease is live. 3) Return `409 IDEMPOTENT_REQUEST_IN_PROGRESS` with `Retry-After: 1` and `retryable: true`. 4) Increment `pp_idempotency_outcomes_total{outcome="in_progress"}`.
**Alternates.** *E1* the client retries before the first completes → another `409`; the client SDK backs off on `Retry-After`.
**Rationale note.** Blocking the second request on the first's lease is the alternative, and it is how thread pools die under retry storms: N concurrent duplicates hold N request slots waiting on one worker. Failing fast keeps the ingress pool free. Contract cost: the client must handle `409` as retryable — which is why `retryable: true` is machine-readable (baseline §20).
**Postconditions.** Exactly one execution; the platform's concurrency is bounded by real work, not by duplicate work waiting.
**Events.** —
**Validation.** —
**API.** all mutating endpoints.
**Traces.** BR-21.

### FR-56 — Idempotency key reused with a different body

**Statement.** Same key, different request fingerprint → `422 IDEMPOTENCY_KEY_REUSED`, regardless of the record's state.
**Trigger.** FR-54 conflict with a fingerprint mismatch.
**Preconditions.** A record exists for the scope tuple.
**Main flow.** 1) Compare the incoming fingerprint with the stored one. 2) On mismatch, return `422 IDEMPOTENCY_KEY_REUSED`, non-retryable, with a `detail` explaining that the key is bound to a different request body. 3) Increment `pp_idempotency_outcomes_total{outcome="conflict"}`.
**Alternates.** —
**Rationale note.** This catches the client bug where one key is reused across two genuinely different payments. Without the fingerprint check, the second payment would be silently swallowed and replayed as the first — the customer is charged once but the merchant believes two orders were paid.
**Postconditions.** No payment is silently lost to a client-side key-generation bug.
**Events.** —
**Validation.** —
**API.** all mutating endpoints.
**Traces.** BR-21.

### FR-57 — Replay a completed idempotent request

**Statement.** A duplicate of a `COMPLETED` or `FAILED_TERMINAL` record returns the stored status code and body with `Idempotent-Replay: true`.
**Trigger.** FR-54 conflict where the record is terminal and fingerprints match.
**Preconditions.** A response snapshot is stored.
**Main flow.** 1) Read the snapshot (Redis first, PostgreSQL authoritative). 2) Return the stored status, headers-of-record and body byte-for-byte. 3) Set `Idempotent-Replay: true`. 4) Increment `pp_idempotency_outcomes_total{outcome="replay"}`.
**Alternates.** *A1* the record is `FAILED_TERMINAL` → replay the stored *error*; a client retrying a business-rule rejection must get the same rejection, not a fresh attempt. *E1* the record is terminal but the snapshot is missing (a defect) → `500 INTERNAL_ERROR` with a page; never re-execute, because re-execution is the double-charge path.
**Postconditions.** Byte-identical responses for identical requests within the retention window.
**Events.** —
**Validation.** —
**API.** all mutating endpoints.
**Traces.** BR-21.

### FR-58 — Reclaim an expired idempotency lease

**Statement.** When the original process died mid-execution, another request may atomically reclaim the expired lease and re-execute — safely, because the gateway-level key makes re-dispatch non-duplicating.
**Trigger.** FR-54 conflict where the record is `IN_FLIGHT` and `lease_expires_at < now()`.
**Preconditions.** Fingerprints match.
**Main flow.** 1) `UPDATE idempotency_records SET lease_expires_at = now + ttl, lease_owner = $me WHERE key = $k AND lease_expires_at < now()` — reclaim succeeds only if exactly one row is affected. 2) Determine whether an attempt row already exists for this payment. 3) If an attempt exists in `DISPATCHED` or `TIMEOUT_UNKNOWN`, do **not** dispatch again; return `processing` semantics and let reconciliation resolve it (FR-66). 4) Otherwise execute normally.
**Alternates.** *E1* the reclaim `UPDATE` affects zero rows (another process won) → treat as FR-55. *E2* an attempt exists in a successful terminal state → transition the idempotency record to `COMPLETED` from the payment's actual state and replay it.
**Rationale note.** Step 3 is the subtle one: a crashed process may have dispatched to the gateway. Re-dispatching would be a second authorization. Deferring to reconciliation is slower and correct; re-dispatching is faster and occasionally catastrophic.
**Postconditions.** A crashed request is recoverable without any possibility of a second authorization.
**Events.** `payment.reconciliation_required.v1` where an in-flight attempt is found.
**Validation.** L7.
**API.** all mutating endpoints.
**Traces.** BR-21, BR-28.

### FR-59 — Load merchant context and reject non-active merchants

**Statement.** Load the merchant's cached configuration snapshot (≤ 30 s stale) and refuse payment creation for any merchant not in `ACTIVE`.
**Trigger.** Baseline §12 stage 9.
**Preconditions.** Idempotency claimed.
**Main flow.** 1) Look up the snapshot in the local cache. 2) On miss, load from the control-plane read path and populate. 3) Assert merchant state is `ACTIVE`. 4) Bind the snapshot into the request context for L5, risk and routing.
**Alternates.** *A1* merchant `SUSPENDED` and the operation is refund/void → permitted (FR-70, FR-71). *E1* merchant not `ACTIVE` and the operation is payment creation → `409 MERCHANT_NOT_ACTIVE`. *E2* merchant unknown → `404 MERCHANT_NOT_FOUND`. *E3* control plane down and no cached snapshot → per FR-48.
**Postconditions.** Every downstream stage evaluates against one consistent snapshot, so a mid-request config change cannot produce a self-inconsistent decision.
**Events.** —
**Validation.** —
**API.** money-path endpoints.
**Traces.** BR-20, BR-31.

### FR-60 — Apply L5 payment validation

**Statement.** Validate the payment against the merchant's configuration — amount limits, currency, payment method, country, capture mode, refund/capture window — as pure rules with the configuration snapshot as an input.
**Trigger.** Baseline §12 stage 10.
**Preconditions.** Merchant context loaded.
**Main flow.** 1) `amount > 0` and integer minor units. 2) `currency ∈ supportedCurrencies`. 3) `paymentMethod ∈ paymentMethods`. 4) `amount ≤ risk.maxTransactionAmount` (same currency; no implicit conversion). 5) Daily-volume and velocity counters within limits. 6) Country not in `blockedCountries`.
**Alternates.** *E1* amount over limit → `422 AMOUNT_EXCEEDS_LIMIT` with the limit. *E2* unsupported currency → `422 CURRENCY_NOT_SUPPORTED`. *E3* unsupported method → `422 PAYMENT_METHOD_NOT_SUPPORTED`. *E4* velocity breached → `422` with `Retry-After` derived from the window. *E5* currency mismatch in a limit comparison → `ErrCurrencyMismatch` surfaced as `422`, never a silent conversion.
**Postconditions.** No gateway call is made for a payment the merchant's own configuration forbids — the breach costs one rejected request, not a chargeback.
**Events.** —
**Validation.** L5.
**API.** `POST /v1/payments`.
**Traces.** BR-14, BR-07, BR-08.

### FR-61 — Evaluate risk and force SCA

**Statement.** Evaluate the risk policy within a 15 ms budget, producing allow / decline / force-3DS, with PSD2 exemptions modelled explicitly and every decision recorded.
**Trigger.** Baseline §12 stage 11.
**Preconditions.** L5 passed.
**Main flow.** 1) Evaluate deterministic policy rules from the configuration snapshot (amount thresholds, corridor, velocity state, blocked lists). 2) Optionally call the external scorer port with a sub-budget deadline. 3) Combine into a decision. 4) If `amount ≥ risk.require3DSAbove` or the policy demands it, set `require_3ds`. 5) If an exemption applies (TRA, low value, MIT), record which exemption, on what basis, with which inputs. 6) Persist the decision with the payment.
**Alternates.** *A1* scorer times out or errors → fall back to the **policy default** (per merchant configuration), never to "allow". *E1* decision is decline → `422 RISK_DECLINED`; no gateway call. *E2* 3DS required but the payment method cannot carry 3DS → `422 THREE_DS_REQUIRED` explaining that the method is ineligible.
**Postconditions.** Every SCA decision and every exemption claim is auditable evidence.
**Events.** —
**Validation.** L5.
**API.** `POST /v1/payments`.
**Traces.** BR-15.

### FR-62 — Build and persist the routing plan

**Statement.** Produce an ordered, reason-annotated list of candidate gateways for this payment at this instant, and persist it with the payment.
**Trigger.** Baseline §12 stage 12.
**Preconditions.** Risk allows; merchant has ≥ 1 connection.
**Main flow.** 1) Start from the merchant's certified connections. 2) Filter by capability descriptor: currency, payment method, country, operation. 3) Filter by residency policy (FR-07). 4) Filter out gateways whose circuit for this operation is `OPEN`. 5) Apply the configuration's conditional rules to select the primary and fallback ordering. 6) Score remaining candidates by the configured weights over health, recent success rate, cost and latency. 7) Persist an `rpl_` row with each candidate, its score components and, for each excluded gateway, the exclusion reason.
**Alternates.** *A1* per-merchant contractual pinning → the pinned gateway is forced first regardless of score, but is still subject to eligibility filters. *E1* every candidate filtered out → empty plan → `503 NO_ELIGIBLE_GATEWAY` with `Retry-After`; the persisted plan records why each gateway was excluded, which is what makes this debuggable at 3 a.m.
**Postconditions.** The routing decision is reconstructible after the fact — including counterfactuals — for uplift analysis and dispute evidence.
**Events.** —
**Validation.** L5.
**API.** internal.
**Traces.** BR-13, BR-09, BR-36, BR-22.

### FR-63 — Create and dispatch an attempt

**Statement.** Persist the attempt row before the gateway call, dispatch through the adapter, and record the outcome.
**Trigger.** Baseline §12 stage 13.
**Preconditions.** A non-empty routing plan; invariant I3 currently satisfied.
**Main flow.** 1) Allocate `att_`. 2) Compute `gateway_idempotency_key = base32(HMAC-SHA256(attempt_id, gateway_salt))[:32]` and store it on the attempt row. 3) Insert the attempt in `PENDING`, then mark `DISPATCHED` immediately before the call — so that a crash leaves evidence that a call may have been made. 4) Call the adapter (FR-40). 5) Validate the response (FR-41). 6) Set the outcome: `SUCCESS` / `DECLINED` / `ERROR` / `TIMEOUT_UNKNOWN`. 7) Apply the payment state transition and write the outbox row in one transaction.
**Alternates.** *E1* the partial unique index on `(payment_id) WHERE outcome='SUCCESS'` rejects a second success → this is invariant I3 firing; treat as a critical defect signal, alert, and return the *existing* successful outcome rather than the new one. *E2* the attempt row insert fails → no gateway call is made; fail with `503`.
**Postconditions.** Every gateway call has a durable pre-call record, so no call is ever invisible after a crash.
**Events.** `payment.attempted.v1`.
**Validation.** L6, L7.
**API.** internal.
**Traces.** BR-20, BR-21, BR-22.

### FR-64 — Fail over after a retryable failure

**Statement.** On a retryable outcome — transport error, gateway 5xx, circuit open, or a decline reason in the retryable set — create a **new attempt** on the next eligible gateway from the routing plan, within the retry budget and the end-to-end latency SLO.
**Trigger.** Attempt outcome `ERROR`, or `DECLINED` with a soft/retryable reason.
**Preconditions.** The routing plan has a further candidate; the retry budget is not exhausted; the elapsed end-to-end time leaves room inside the p99 ≤ 1.5 s target.
**Main flow.** 1) Classify the outcome via the adapter's normalized reason mapping. 2) If retryable and a candidate remains, select the next candidate. 3) Create a **new** attempt with a **new** `att_` and therefore a **new** gateway idempotency key — a genuinely new authorization at a different gateway. 4) Dispatch (FR-63). 5) Record the failover reason on the new attempt.
**Alternates.** *A1* same-gateway transport retry (≤ 2, jittered) happens *within* FR-40 using the *same* attempt and therefore the same gateway key, so the gateway dedupes; only after those are exhausted does a new attempt get created. *E1* budget or candidates exhausted → FR-67. *E2* the prior attempt is `TIMEOUT_UNKNOWN` → **no failover** (FR-66); an unknown outcome is not a retryable one.
**Postconditions.** A payment accumulates 1..N attempts; no prior attempt is ever mutated; uplift is measurable from `payment.attempted.v1` counts and outcomes.
**Events.** `payment.attempted.v1` per attempt.
**Validation.** L6, L7.
**API.** internal.
**Traces.** BR-22, BR-09.

### FR-65 — Hard decline: terminate without failover

**Statement.** A hard decline terminates the payment as `FAILED` immediately. No other gateway is tried, under any configuration.
**Trigger.** Attempt outcome `DECLINED` with a reason mapped as hard (stolen card, invalid/closed account, pickup, fraud suspected, do-not-honour with a hard code).
**Preconditions.** The adapter has mapped the reason code.
**Main flow.** 1) Map the gateway reason into the normalized hard-decline set. 2) Transition the payment `PROCESSING → FAILED` with the normalized reason. 3) Emit `payment.failed.v1` with the reason. 4) Return `422`/`402`-class error with `code: GATEWAY_DECLINED` and `retryable: false`.
**Alternates.** *E1* the reason code is **unmapped** → classify as hard (fail safe), emit an alert so the mapping is completed, and do not fail over. Guessing "soft" on an unknown code is how a platform accidentally runs card testing. *E2* configuration attempts to force failover on hard declines → the option does not exist; there is no flag.
**Rationale note.** Retrying hard declines across gateways is the signature pattern of card testing. Scheme monitoring and gateway partner agreements treat it as abuse; the consequence is de-registration. There is also no upside — a hard decline is deterministic.
**Postconditions.** Exactly one attempt; terminal `FAILED`; a conformance test asserts zero failovers over the hard-decline fixture corpus for every adapter.
**Events.** `payment.failed.v1`.
**Validation.** L6, L7.
**API.** `POST /v1/payments`.
**Traces.** BR-23.

### FR-66 — Gateway timeout with an unknown outcome

**Statement.** When a gateway call times out or returns an ambiguous transport error, mark the attempt `TIMEOUT_UNKNOWN`, leave the payment in `PROCESSING`, return `processing` semantics, and enqueue reconciliation. Never auto-fail, never fail over, never let a timer decide.
**Trigger.** Adapter deadline exceeded (8 s), connection reset after the request was written, or any error where the request may have been received.
**Preconditions.** An attempt row exists in `DISPATCHED`.
**Main flow.** 1) Set the attempt outcome `TIMEOUT_UNKNOWN` with the transport error. 2) Leave the payment `PROCESSING`. 3) Write `payment.reconciliation_required.v1` to the outbox with alerting. 4) Complete the idempotency record as `COMPLETED` with a `status: "processing"` snapshot — so a client retry replays `processing` rather than starting a second payment. 5) Respond `200` with `status: "processing"` and a poll/webhook hint.
**Alternates.** *A1* resolution by gateway webhook → FR-78 moves the payment to its true state. *A2* resolution by reconciler lookup using the stored gateway idempotency key → FR-85. *A3* resolution by settlement report → FR-83. *E1* unresolved at 24 h → critical reconciliation exception with a page.
**Rationale note.** The alternative — auto-failing on timeout and letting the client retry elsewhere — is the single most common cause of double charges in production payment systems, because the original authorization frequently did succeed. Honesty about uncertainty costs latency; guessing costs a chargeback plus a fine.
**Postconditions.** No code path moves a payment out of `PROCESSING` on a timer alone; a fault-injection test asserts this.
**Events.** `payment.reconciliation_required.v1`.
**Validation.** L7.
**API.** `POST /v1/payments`, `/capture`, `/refund`, `/void`.
**Traces.** BR-20, BR-28.

### FR-67 — Routing plan exhausted

**Statement.** When every candidate in the routing plan has been attempted or excluded, return the truthful outcome — the last definitive decline, or `503 NO_ELIGIBLE_GATEWAY` if no candidate could be attempted — never a synthesised success or a generic failure.
**Trigger.** Failover selection finds no further candidate.
**Preconditions.** ≥ 0 attempts made.
**Main flow.** 1) If ≥ 1 attempt produced a definitive `DECLINED`, transition `→ FAILED` with that reason and return `GATEWAY_DECLINED`. 2) If all attempts were `ERROR`/circuit-open and no definitive answer exists, transition `→ FAILED` with `retryable: true` and return `503`. 3) If no attempt was possible at all, return `503 NO_ELIGIBLE_GATEWAY` with `Retry-After`, leaving the payment `FAILED` with a routing reason.
**Alternates.** *E1* any attempt is `TIMEOUT_UNKNOWN` → the payment stays `PROCESSING` regardless of other attempts' outcomes; an unknown dominates a known failure, because the unknown one may have taken money.
**Postconditions.** The response distinguishes "the issuer said no" from "we could not ask" — a distinction merchants make business decisions on.
**Events.** `payment.failed.v1`.
**Validation.** L7.
**API.** `POST /v1/payments`.
**Traces.** BR-22, BR-20.

### FR-68 — 3DS / REQUIRES_ACTION and its completion

**Statement.** When the gateway or the risk policy requires a customer challenge, return `REQUIRES_ACTION` with the challenge payload, and resume the payment when the challenge completes, expires or is abandoned.
**Trigger.** Risk policy forces 3DS, or the gateway responds with a challenge requirement.
**Preconditions.** The payment method supports 3DS on the selected gateway (per capability descriptor).
**Main flow.** 1) Transition `CREATED|PROCESSING → REQUIRES_ACTION`. 2) Persist the challenge reference and its expiry. 3) Return the redirect/challenge payload to the merchant. 4) On the completion callback or webhook, verify it against the stored reference and transition `REQUIRES_ACTION → PROCESSING`. 5) Continue the pipeline from dispatch.
**Alternates.** *A1* the customer abandons → the challenge expiry moves the payment to `EXPIRED`. This is the one expiry that is legitimate: nothing was ever authorized, so no money is at risk. *A2* the challenge fails authentication → `FAILED` with an authentication reason; **no failover**, because another gateway would issue another challenge to a customer who already failed one. *E1* a completion callback for an unknown or already-completed challenge → `409`, counted, not fatal.
**Postconditions.** Liability shift evidence (ECI/CAVV or equivalent) is recorded with the payment for dispute defence.
**Events.** `payment.attempted.v1` on resume.
**Validation.** L6, L7.
**API.** `POST /v1/payments`, plus the completion callback.
**Traces.** BR-15, BR-20.

### FR-69 — Capture, full and partial

**Statement.** Capture an authorized payment in full or in part, up to the authorized amount and up to the configured maximum number of partial captures.
**Trigger.** `POST /v1/payments/{paymentId}/capture`.
**Preconditions.** Payment `AUTHORIZED`; merchant `ACTIVE`; `Idempotency-Key` present.
**Main flow.** 1) Idempotency claim. 2) Assert `captured_total + amount ≤ authorized_amount` (invariant I2, enforced in the domain and by a DB `CHECK`). 3) Assert the partial-capture count is below `limits.maxPartialCaptures`. 4) Dispatch capture on the **same** gateway as the successful authorization — capture never routes. 5) Transition `AUTHORIZED → CAPTURED` on full capture; on partial capture the payment remains `AUTHORIZED` until the final capture or the authorization expires. 6) Write ledger entries.
**Alternates.** *A1* multi-capture amount splitting uses largest-remainder allocation so the parts sum exactly to the whole (baseline §7.4). *E1* amount exceeds the capturable balance → `422` with `code: EXCEEDS_CAPTURABLE`. *E2* partial-capture cap exceeded → `422`. *E3* payment already `CAPTURED` → `409 PAYMENT_ALREADY_PROCESSED`. *E4* capture times out → FR-66 applies; the payment stays in its prior state with a `TIMEOUT_UNKNOWN` attempt.
**Postconditions.** `captured_amount ≤ authorized_amount` always; ledger balanced.
**Events.** `payment.captured.v1`.
**Validation.** L5, L7.
**API.** `POST /v1/payments/{paymentId}/capture`.
**Traces.** BR-24.

### FR-70 — Refund, including after settlement and under concurrency

**Statement.** Refund a captured or settled payment, in full or in part, repeatedly, up to the captured total, within the configured refund window — and correctly under concurrent partial refunds.
**Trigger.** `POST /v1/payments/{paymentId}/refund`.
**Preconditions.** Payment in `CAPTURED`, `SETTLED` or `PARTIALLY_REFUNDED`; `Idempotency-Key` present. Merchant may be `ACTIVE` **or** `SUSPENDED`.
**Main flow.** 1) Idempotency claim. 2) `SELECT … FOR UPDATE` on the payment row to serialize refund accounting. 3) Assert `refunded_total + amount ≤ captured_amount` (invariant I1; DB `CHECK` as the backstop). 4) Assert `now ≤ captured_at + limits.maxRefundWindowDays`. 5) Allocate `ref_`; dispatch to the same gateway as the capture. 6) Transition to `PARTIALLY_REFUNDED` or `REFUNDED`. 7) Write ledger entries.
**Alternates.** *A1* **refund after settlement is the normal case** — `SETTLED → PARTIALLY_REFUNDED | REFUNDED` is a first-class transition, not an exception path. *A2* merchant `SUSPENDED` → permitted; you must always be able to give money back. *E1* concurrent partial refunds that together exceed the captured amount → the row lock serializes them; exactly one succeeds, the other gets `422 REFUND_EXCEEDS_CAPTURED`. *E2* outside the refund window → `422` naming the window and the capture date. *E3* the gateway's own refund window has closed → `502 GATEWAY_DECLINED` with a remediation pointing at an out-of-band payout; L4 (FR-51) is what prevents configuring a window we cannot honour. *E4* refund on a `DISPUTED` payment → refused; refunding a payment already in dispute double-refunds the customer.
**Postconditions.** `sum(refunds.amount) ≤ captured_amount` holds under all interleavings; ledger balanced.
**Events.** `payment.refunded.v1`.
**Validation.** L5, L7.
**API.** `POST /v1/payments/{paymentId}/refund`.
**Traces.** BR-25, BR-31.

### FR-71 — Void an uncaptured authorization

**Statement.** Release the hold on an authorized, uncaptured payment; the operation is idempotent and terminal.
**Trigger.** `POST /v1/payments/{paymentId}/void`.
**Preconditions.** Payment `AUTHORIZED` with `captured_amount = 0`; `Idempotency-Key` present. Merchant may be `ACTIVE` or `SUSPENDED`.
**Main flow.** 1) Idempotency claim. 2) Assert state and zero captures. 3) Dispatch void to the authorizing gateway. 4) Transition `AUTHORIZED → VOIDED`. 5) Write reversing ledger entries.
**Alternates.** *A1* partial capture already occurred → void is refused; the remaining balance is released by the gateway at authorization expiry, and the platform reflects `EXPIRED` on the residual. *E1* payment `CAPTURED` → `409 INVALID_STATE_TRANSITION`. *E2* void times out → FR-66; the payment stays `AUTHORIZED` with a `TIMEOUT_UNKNOWN` attempt until reconciled. *E3* the authorization has already expired at the gateway → treat as success (the hold is released either way) and record the reason.
**Postconditions.** No hold outlives its purpose without being recorded as `VOIDED` or `EXPIRED`.
**Events.** `payment.voided.v1`.
**Validation.** L5, L7.
**API.** `POST /v1/payments/{paymentId}/void`.
**Traces.** BR-26, BR-31.

### FR-72 — Read and list payments with read-your-writes

**Statement.** Serve payment reads from a replica with bounded staleness, guaranteeing that a caller reads its own writes.
**Trigger.** `GET /v1/payments/{paymentId}`, `GET /v1/payments`.
**Preconditions.** `payments:read`.
**Main flow.** 1) On a write, return an opaque write token (LSN-derived) in a response header. 2) On a read carrying that token, compare against the replica's applied LSN; if the replica is behind, read from the primary. 3) Without a token, read from the replica (≤ 1 s stale). 4) List reads are cursor-paginated with stable `(created_at, payment_id)` ordering.
**Alternates.** *A1* no token and the client needs strong consistency → an explicit `Consistency: strong` header forces a primary read, rate-limited to protect the primary. *E1* replica unavailable → fall back to the primary with a degradation counter.
**Postconditions.** A merchant never sees a payment it just created as missing — the single most common complaint about read-replica architectures.
**Events.** —
**Validation.** L1.
**API.** `GET /v1/payments/{paymentId}`, `GET /v1/payments`.
**Traces.** BR-20.

### FR-73 — Asynchronous payment methods

**Statement.** For methods whose outcome is inherently asynchronous (bank debit, voucher, some wallets), transition to `PENDING` and resolve on webhook or reconciliation, with an explicit expiry.
**Trigger.** Gateway responds with a pending/initiated status for an async method.
**Preconditions.** The method is declared async in the capability descriptor.
**Main flow.** 1) Transition `PROCESSING → PENDING` with the method's expected resolution window and a hard expiry. 2) Return `status: "pending"` with the expected window. 3) On the resolving webhook, transition `PENDING → AUTHORIZED | CAPTURED | FAILED`. 4) At hard expiry with no resolution, transition `PENDING → EXPIRED` and reconcile against the gateway before doing so.
**Alternates.** *E1* expiry reached but a gateway lookup shows the payment succeeded → resolve to the true state; expiry never overrides evidence. *E2* the merchant polls before resolution → `status: "pending"`, not an error.
**Postconditions.** `PENDING` is a real state with an owner and a deadline, not a parking lot.
**Events.** `payment.captured.v1` / `payment.authorized.v1` / `payment.failed.v1` on resolution.
**Validation.** L7.
**API.** `POST /v1/payments`, `GET /v1/payments/{paymentId}`.
**Traces.** BR-20, BR-28.

---

## 7. BC-7 — Webhook Ingestion (Data plane)

### FR-74 — Accept and persist an inbound webhook

**Statement.** Accept a gateway webhook, persist it verbatim, and acknowledge within a 50 ms budget; all interpretation is asynchronous.
**Trigger.** `POST /v1/webhooks/{gateway}`.
**Preconditions.** The gateway is registered and has a declared signature scheme.
**Main flow.** 1) Read the body with a size cap. 2) Verify the signature (FR-75). 3) Insert an `whk_` row with the raw body, headers-of-record, gateway, receipt timestamp and a computed `gateway_ref`. 4) Write `webhook.received.v1` to the outbox. 5) Return `200` immediately.
**Alternates.** *E1* body exceeds the cap → `413`. *E2* the database is unavailable → `503`; gateways retry, so shedding is safe, whereas acknowledging without persisting loses the event permanently. *E3* processing budget exceeded → still acknowledge if persisted; the budget governs the accept path only.
**Rationale note.** Accept-and-persist rather than accept-and-process is what keeps webhook ingress available during a downstream incident: gateways time out aggressively and disable endpoints that are slow, and a disabled webhook endpoint silently breaks reconciliation for every merchant on that gateway.
**Postconditions.** Every accepted webhook is durable before acknowledgement.
**Events.** `webhook.received.v1`.
**Validation.** L1.
**API.** `POST /v1/webhooks/{gateway}`.
**Traces.** BR-28, BR-30.

### FR-75 — Verify the webhook signature

**Statement.** Verify every inbound webhook against the gateway's declared signature scheme using the connection's stored signing secret, in constant time.
**Trigger.** FR-74 step 2.
**Preconditions.** A signing secret exists for the gateway (and, where the scheme is per-connection, for the merchant).
**Main flow.** 1) Select the scheme from the capability descriptor (HMAC-SHA256 over a canonical string, detached JWS, or mTLS peer identity). 2) Resolve the secret via `secretRef`. 3) Compute and compare in constant time. 4) During a signing-secret rotation overlap, accept either the current or the previous secret.
**Alternates.** *E1* signature invalid → `401 WEBHOOK_SIGNATURE_INVALID`, raise a security event, do **not** persist the body beyond a truncated forensic sample. *E2* no secret configured → `503` and a critical alert; silently accepting unsigned webhooks would let anyone move a payment's state.
**Postconditions.** No unauthenticated payload can influence domain state.
**Events.** `audit.recorded.v1` on failure.
**Validation.** L1.
**API.** `POST /v1/webhooks/{gateway}`.
**Traces.** BR-11, BR-30.

### FR-76 — Reject a replayed webhook

**Statement.** Reject webhooks whose timestamp is outside a ±5 minute skew window or whose nonce has already been seen.
**Trigger.** FR-74, after signature verification.
**Preconditions.** The scheme carries a timestamp and/or nonce.
**Main flow.** 1) Parse the signed timestamp. 2) Reject if `|now − timestamp| > 5 min`. 3) Check the nonce against the replay table. 4) Record the nonce with a TTL exceeding the skew window.
**Alternates.** *A1* the scheme has no timestamp → rely on the dedup table (FR-77) alone and record the weaker guarantee in the gateway's descriptor. *E1* skew exceeded or nonce reused → `401 WEBHOOK_REPLAY_DETECTED` + security event. *E2* local clock skew detected by NTP monitoring → alert; the ±5 min window tolerates normal skew but not a broken clock.
**Postconditions.** A captured webhook cannot be replayed to re-drive a state transition.
**Events.** `audit.recorded.v1`.
**Validation.** L1.
**API.** `POST /v1/webhooks/{gateway}`.
**Traces.** BR-30.

### FR-77 — Deduplicate a duplicate webhook

**Statement.** A webhook already processed (same `(gateway, gateway_event_id)`) is dropped silently and counted; gateways retry aggressively and duplicates are normal traffic, not errors.
**Trigger.** Webhook processing.
**Preconditions.** A dedup key can be derived.
**Main flow.** 1) `INSERT INTO webhook_dedup (gateway, gateway_event_id) ON CONFLICT DO NOTHING`. 2) Zero rows affected → acknowledge and drop; increment the duplicate counter. 3) Otherwise process within the same transaction as the dedup row.
**Alternates.** *A1* the gateway supplies no event ID → derive one from a hash of the canonicalized payload; note the weaker guarantee. *E1* duplicate rate above a threshold → investigate, since it usually means we are acknowledging too slowly.
**Postconditions.** Effectively-once processing per baseline §13.5.
**Events.** —
**Validation.** —
**API.** internal.
**Traces.** BR-21, BR-30.

### FR-78 — Process a webhook into a domain state transition

**Statement.** Translate a verified, deduplicated webhook into a domain event and apply the corresponding state transition, including resolving `TIMEOUT_UNKNOWN` attempts.
**Trigger.** `webhook.received.v1` consumed by the webhook processor.
**Preconditions.** Webhook persisted, verified, deduped.
**Main flow.** 1) The gateway's ACL maps the payload to a domain intent (authorized, captured, failed, refunded, settled, disputed, chargeback-reversed). 2) Resolve the target payment by the gateway reference or the stored gateway idempotency key. 3) Apply the L7 transition with optimistic concurrency. 4) If the payment had a `TIMEOUT_UNKNOWN` attempt, resolve that attempt's true outcome and clear the reconciliation flag. 5) Write ledger entries and the outbox event in the same transaction.
**Alternates.** *A1* the webhook describes a state the payment is already in → no-op, counted (webhooks race with synchronous responses constantly). *E1* the transition is invalid from the current state → do not force it; raise a reconciliation exception with both states. A gateway telling us something impossible is a real signal, not a bug to paper over. *E2* mapping fails (unknown event type) → park in the DLQ, alert, and continue consuming; one unmappable event must not stall the partition.
**Postconditions.** Async gateway truth converges into platform state within the webhook-lag SLO (p99 ≤ 60 s).
**Events.** the mapped `payment.*.v1`.
**Validation.** L6, L7.
**API.** internal.
**Traces.** BR-28, BR-30, BR-27.

### FR-79 — Webhook for an unknown payment

**Statement.** A webhook referencing a payment we have no record of raises a critical reconciliation exception; it is never dropped.
**Trigger.** FR-78 step 2 fails to resolve a payment.
**Preconditions.** The webhook is verified and deduped.
**Main flow.** 1) Retry resolution after a short delay (the webhook may have overtaken our own commit). 2) On continued failure, raise a reconciliation exception of type `ORPHAN_GATEWAY_EVENT` carrying the full payload reference. 3) Alert at critical severity if the event implies money moved (captured, refunded, disputed).
**Alternates.** *A1* the payment appears within the retry window → process normally; this race is common at high TPS. *E1* the event belongs to another platform sharing the gateway account → the exception is closed as `NOT_OURS` by an operator, which is itself a finding about account hygiene.
**Rationale note.** An orphan capture event means money moved that we cannot attribute. Dropping it — the naive behaviour — makes the ledger silently wrong.
**Postconditions.** No money-moving gateway event is ever discarded unexamined.
**Events.** `payment.reconciliation_required.v1`.
**Validation.** L7.
**API.** internal.
**Traces.** BR-28, BR-30.

---

## 8. BC-8 — Ledger & Reconciliation (Data plane)

### FR-80 — Append ledger entries transactionally

**Statement.** Write balanced, double-entry, append-only ledger entries in the same database transaction as the state change that caused them.
**Trigger.** Any money-affecting transition: authorized, captured, refunded, voided, settled, disputed, dispute-reversed.
**Preconditions.** The payment state change is being committed.
**Main flow.** 1) Derive the entry set from the transition type and amount. 2) Assert debits equal credits per transaction. 3) `INSERT` into `ledger_entries` with `led_` IDs, account references, amounts in minor units and the causing event reference. 4) Commit with the state change and outbox row.
**Alternates.** *A1* a correction is required → post a compensating entry pair; never `UPDATE` or `DELETE`. *E1* imbalance detected pre-commit → abort the transaction and raise a critical defect alert; an unbalanced ledger write is never committed.
**Postconditions.** `ledger_entries` is append-only (enforced by revoking `UPDATE`/`DELETE` on the table from the application role); a position is reconstructible as of any past timestamp.
**Events.** —
**Validation.** L7.
**API.** internal.
**Traces.** BR-29.

### FR-81 — Continuously verify the ledger balance invariant

**Statement.** A continuous check asserts that debits equal credits per transaction and that per-payment ledger totals agree with the payment aggregate's `authorized`, `captured` and `refunded` amounts.
**Trigger.** Scheduled (default every 5 minutes over a moving window) plus a full pass nightly.
**Preconditions.** —
**Main flow.** 1) Aggregate entries by transaction and assert balance. 2) Aggregate by payment and compare with the aggregate row. 3) On divergence, raise a critical reconciliation exception naming the payment and the delta. 4) Export `pp_reconciliation_exceptions{severity}`.
**Alternates.** *A1* divergence attributable to an in-flight transaction at the window boundary → re-check once before raising; a false page is expensive.
**Postconditions.** A ledger/aggregate divergence is detected within minutes, not at month-end.
**Events.** `payment.reconciliation_required.v1` on divergence.
**Validation.** —
**API.** internal; `platformctl reconcile ledger`.
**Traces.** BR-29.

### FR-82 — Ingest a settlement report

**Statement.** Ingest gateway settlement reports (file or webhook), idempotently on the file hash and line identity, into a staging structure before matching.
**Trigger.** Scheduled fetch, or a settlement webhook.
**Preconditions.** The connection has settlement reporting configured.
**Main flow.** 1) Fetch the report. 2) Compute the file hash; if already ingested, no-op. 3) Parse through the gateway's ACL into a normalized settlement-line model (gateway reference, gross, fees, net, currency, settlement date). 4) Persist lines with a `(file_hash, line_no)` identity. 5) Open an `rcn_` reconciliation run.
**Alternates.** *A1* a corrected/restated report supersedes an earlier one → both are retained; the run records which file is authoritative and why. *E1* parse failure → the whole file is rejected, not partially ingested; a half-ingested settlement file is worse than none. *E2* an unexpected currency or a negative gross where the schema forbids it → reject the line into an exception, continue with the rest.
**Postconditions.** Re-ingesting the same file is a no-op; every ingested line is attributable to a file.
**Events.** —
**Validation.** L6.
**API.** internal; `platformctl reconcile settlement`.
**Traces.** BR-30.

### FR-83 — Match settlement lines to payments

**Statement.** Match each settlement line to a payment or refund, move matched payments to `SETTLED`, and raise typed exceptions for everything else.
**Trigger.** A reconciliation run opened by FR-82.
**Preconditions.** Lines are staged.
**Main flow.** 1) Match on the gateway reference, falling back to `(gateway_idempotency_key, amount, currency, date-window)`. 2) On a match, assert the amount and currency agree. 3) Transition `CAPTURED → SETTLED` and write settlement ledger entries including fees. 4) Record match rate and exception counts on the run.
**Alternates.** *A1* the line matches a payment in `PROCESSING` with a `TIMEOUT_UNKNOWN` attempt → this **resolves** the unknown: the money moved, so the payment advances to `CAPTURED` then `SETTLED`. Settlement is the slowest but most authoritative resolution path. *E1* no match → exception `UNMATCHED_SETTLEMENT_LINE` (critical: money moved that we cannot attribute). *E2* a payment we believe captured has no settlement line beyond the expected window → exception `MISSING_SETTLEMENT` (critical). *E3* amount mismatch → exception `SETTLEMENT_AMOUNT_MISMATCH` (critical) — this is the case where the gateway settled a different amount than we captured. *E4* duplicate settlement of the same payment → exception `DUPLICATE_SETTLEMENT`.
**Postconditions.** Every settlement line ends in exactly one of: matched, or a typed exception with an owner.
**Events.** `payment.settled.v1`.
**Validation.** L7.
**API.** internal.
**Traces.** BR-30, BR-28.

### FR-84 — Manage the reconciliation exception lifecycle

**Statement.** Every exception has a type, a severity, an owner, a documented remediation runbook, and a resolution recorded with its rationale.
**Trigger.** Any exception raised by FR-42, FR-79, FR-81, FR-83, FR-85.
**Preconditions.** —
**Main flow.** 1) Persist with type, severity, subject references, first-seen and evidence. 2) Route by severity: critical → page; major → ticket; minor → queue. 3) Expose through the operator surface with the runbook link. 4) Resolution requires a principal, a resolution code and free text; auto-resolution is permitted only for exception types explicitly marked self-healing.
**Alternates.** *A1* the same exception recurs → increment an occurrence counter rather than creating duplicates. *E1* an open critical exception for a merchant blocks that merchant's activation (FR-12 guard). *E2* an exception open beyond its SLA escalates automatically.
**Postconditions.** `pp_reconciliation_exceptions{severity="critical"}` older than 24 h is the metric that must be zero (BR success metric).
**Events.** `audit.recorded.v1`.
**Validation.** —
**API.** operator surface.
**Traces.** BR-30, BR-28, BR-18.

### FR-85 — Resolve a TIMEOUT_UNKNOWN attempt by gateway lookup

**Statement.** For every attempt in `TIMEOUT_UNKNOWN`, poll the gateway's transaction-lookup API using the attempt's deterministic gateway idempotency key until a definitive outcome is obtained.
**Trigger.** `payment.reconciliation_required.v1`, plus a sweeper over unresolved attempts.
**Preconditions.** The attempt row carries the gateway idempotency key (written before dispatch, FR-63).
**Main flow.** 1) Reconstruct the key from `attempt_id` — no state from the crashed process is required. 2) Call the gateway's lookup/search API. 3) If the transaction exists and succeeded → resolve the attempt to `SUCCESS` and transition the payment accordingly. 4) If it exists and declined → resolve to `DECLINED`. 5) If it definitively does not exist → resolve to `ERROR` (safe: nothing happened) and permit normal failover. 6) Clear the reconciliation flag.
**Alternates.** *A1* a webhook resolves it first → the sweeper finds it already resolved and exits. *A2* the gateway has no lookup API → escalate to settlement-based resolution (FR-83) and record the longer resolution SLA against that gateway in its descriptor. *E1* lookup is itself ambiguous → back off and retry with a widening interval; escalate to a critical exception at 24 h. *E2* the lookup shows **two** transactions for the key → critical alert; this means the gateway's idempotency failed, which is a partner incident.
**Postconditions.** p95 resolution ≤ 5 min, p99 ≤ 30 min; nothing unresolved past 24 h without a page.
**Events.** the resolving `payment.*.v1`.
**Validation.** L6, L7.
**API.** internal; `platformctl reconcile unknown`.
**Traces.** BR-28.

### FR-86 — Handle a dispute lifecycle notification

**Statement.** Ingest dispute opened / evidence-required / won / lost notifications, transition the payment, record deadlines and amounts, and notify the merchant with actionable lead time.
**Trigger.** A dispute webhook or a settlement-file dispute line.
**Preconditions.** The payment resolves (else FR-79).
**Main flow.** 1) Transition `CAPTURED | SETTLED | PARTIALLY_REFUNDED → DISPUTED`. 2) Persist reason code, dispute amount, evidence deadline and the gateway's case reference. 3) Write ledger entries reflecting the provisional debit. 4) Emit `payment.disputed.v1` for Ledger, Risk and Notification. 5) Notify the merchant with configurable lead time (default 5 days before deadline).
**Alternates.** *A1* dispute lost → `DISPUTED → REFUNDED` with reversing ledger entries. *A2* dispute won → `DISPUTED → CAPTURED | SETTLED` restoring the funds position. *E1* dispute on a payment already fully refunded → do not double-debit; raise an exception, because this usually indicates the customer was refunded *and* charged back.
**Postconditions.** Dispute rate per merchant and per gateway is a first-class metric feeding the risk policy.
**Events.** `payment.disputed.v1`.
**Validation.** L7.
**API.** internal.
**Traces.** BR-27.

### FR-87 — Produce the billing meter projection

**Statement.** Produce a deterministic, tenant-scoped meter of billable events derived from the event log, reproducible for any closed period.
**Trigger.** Scheduled period close, or an on-demand recompute.
**Preconditions.** The event log for the period is complete (consumer lag ≈ 0).
**Main flow.** 1) Consume `payment.attempted.v1`, `merchant.activated.v1`, certification run records and siloed-resource inventory. 2) Aggregate by tenant, period and meter dimension. 3) Reconcile the meter against `payments`/`payment_attempts` counts; require zero variance for a closed period. 4) Emit the billing export.
**Alternates.** *A1* late-arriving events for a closed period → attribute to the period in which they occurred and emit a restatement record rather than silently altering a closed meter. *E1* non-zero variance → block the export and raise a critical exception; a wrong invoice is worse than a late one.
**Postconditions.** Recomputing a closed period yields the identical result — the meter is defensible in a billing dispute.
**Events.** —
**Validation.** —
**API.** internal; `platformctl meter export`.
**Traces.** BR-38.

---

## 9. BC-9 — Audit (Observability plane)

### FR-88 — Write a hash-chained audit record

**Statement.** Every state-changing action, by human or automation, writes an immutable audit record chained to its predecessor by a cryptographic digest.
**Trigger.** Any mutation across every context.
**Preconditions.** —
**Main flow.** 1) Compose the record: `aud_` ID, tenant, actor (type, ID, principal), action, subject, before/after (redacted), correlation and causation IDs, timestamp, source service. 2) Compute `digest = H(prev_digest || canonical(record))`. 3) Append within the same transaction as the mutation where the store permits, else via the outbox with strict ordering per tenant. 4) Ship to the audit sink and SIEM.
**Alternates.** *A1* the audit sink is unavailable → buffer to a local WAL and replay; audit is CP for the chain but must never block a money-path commit indefinitely. *E1* a redaction rule would emit a forbidden field (PAN, CVV, credential, unmasked account) → the write is rejected as a defect and the field is dropped; the record is still written.
**Postconditions.** Tampering with any record invalidates every subsequent digest.
**Events.** `audit.recorded.v1`.
**Validation.** L4 (redaction allowlist).
**API.** internal.
**Traces.** BR-37.

### FR-89 — Verify the audit chain

**Statement.** A scheduled job recomputes the hash chain per tenant per period and alerts on any break, publishing a signed integrity attestation.
**Trigger.** Scheduled (default daily) and on demand.
**Preconditions.** —
**Main flow.** 1) Walk the chain for the period. 2) Recompute digests. 3) On a match, publish a signed attestation covering `(tenant, period, first_id, last_id, terminal_digest)` to WORM storage. 4) On a break, raise a security-severity incident naming the first divergent record.
**Alternates.** *E1* a gap in the sequence (missing record) → treated as a break, not as a benign omission.
**Postconditions.** Integrity is provable per period, not merely asserted.
**Events.** —
**Validation.** —
**API.** internal; `platformctl audit verify`.
**Traces.** BR-37.

### FR-90 — Export a tenant-scoped audit and compliance report

**Statement.** Produce a tenant-scoped, cursor-paginated, tamper-evident export over an arbitrary time range and action filter, suitable for an auditor.
**Trigger.** `GET /v1/audit` / compliance export endpoint.
**Preconditions.** `audit:read`; tenant derived from the principal.
**Main flow.** 1) Query within the tenant's partition under RLS. 2) Stream records with their digests and the period attestations that cover them. 3) Sign the export manifest.
**Alternates.** *A1* the range spans archived periods → transparently read from S3/Glacier with a documented restore latency. *E1* a cross-tenant filter is attempted → `403 TENANT_MISMATCH` + security event.
**Postconditions.** An auditor can verify the export independently using the published digest scheme.
**Events.** `audit.recorded.v1` (the export itself is an auditable action).
**Validation.** L1.
**API.** compliance export endpoint.
**Traces.** BR-37, BR-36.

### FR-91 — Raise a security event on an isolation or data-boundary violation

**Statement.** Tenant-mismatch attempts, PAN detection, webhook signature failures, IP-allowlist denials and residency violations raise structured security events routed to the SIEM and to alerting, without logging the offending value.
**Trigger.** FR-06 *E1*, FR-09 *E2*, FR-05 *E3*, FR-07 *E1*, FR-75 *E1*, FR-76 *E1*.
**Preconditions.** —
**Main flow.** 1) Emit a security event with the class, principal, source IP, route, correlation ID and a non-reversible fingerprint of the offending input. 2) Write the audit record. 3) Feed the alerting rule that pages on a spike. 4) Never write the raw value to any log, span or metric.
**Alternates.** *A1* a repeated pattern from one principal → automatic rate-limit tightening and, past a threshold, client suspension pending review. *E1* the detector itself errors → fail closed on the request and alert; a broken PAN detector must not become an open door.
**Postconditions.** Boundary violations are observable and correlatable without becoming a data-leak vector themselves.
**Events.** `audit.recorded.v1`.
**Validation.** L1.
**API.** all.
**Traces.** BR-01, BR-02, BR-36, BR-37.

---

## 10. Alternatives considered and rejected (functional design)

| # | Alternative | Why rejected |
|---|---|---|
| FALT-1 | **Block the second concurrent idempotent request until the first completes** | Superficially friendlier: the caller gets the real answer instead of a `409`. But it converts every retry storm into a thread-pool exhaustion event — N duplicates occupy N request slots waiting on one worker, and the ingress pool dies before the storm does. Baseline A6 chooses fail-fast; FR-55 implements it and makes `retryable: true` machine-readable so SDKs recover automatically. |
| FALT-2 | **Redis as the authoritative idempotency store** | Sub-millisecond claims and trivially expiring records. Rejected because a Redis failover can lose acknowledged writes, and a lost idempotency claim is a double charge. PostgreSQL is authoritative; Redis mirrors completed records purely as a latency accelerator (FR-54 *A2*). |
| FALT-3 | **Fail over on `TIMEOUT_UNKNOWN`** | Would improve apparent authorization rate and reduce `processing` responses. It is also the dominant real-world cause of double charges, since the timed-out authorization frequently succeeded. Baseline A7; FR-66. |
| FALT-4 | **Mutate the existing attempt on failover instead of creating a new one** | Fewer rows, simpler queries. Destroys the audit trail, breaks the derivation of gateway idempotency keys from `attempt_id` (a mutated attempt would reuse a key at a different gateway), and makes invariant I3 unenforceable. FR-64 creates a new attempt. |
| FALT-5 | **A single global `retryable` decline list rather than per-adapter mapping** | Cheaper to maintain, but decline semantics genuinely differ per gateway and per scheme, and a mis-classified hard decline is a card-testing incident. Each adapter owns its mapping and the contract suite tests it; unmapped codes fail safe to hard (FR-65 *E1*). |
| FALT-6 | **Process webhooks synchronously on the ingress request** | Simpler, and removes a queue. Rejected: gateways time out aggressively and disable slow endpoints, and a disabled endpoint silently breaks reconciliation for every merchant on that gateway. FR-74 accepts and persists within 50 ms; interpretation is asynchronous. |
| FALT-7 | **Drop webhooks for unknown payments** | The naive default. An orphan capture or dispute event means money moved that we cannot attribute; dropping it makes the ledger quietly wrong and is discovered at month-end. FR-79 raises a critical exception instead. |
| FALT-8 | **Revert configuration in place on rollback** | One row, one update, obvious semantics. It destroys history, makes "what was live at 14:03?" unanswerable, and — worse — restores a document that may no longer validate against current capability descriptors without noticing. FR-46 republishes forward and re-validates. |
| FALT-9 | **Route captures and refunds independently of the authorizing gateway** | Would allow cost optimisation per operation. It is not physically possible: a capture must go to the acquirer holding the authorization. FR-69/FR-70 pin these operations to the successful attempt's gateway. |
| FALT-10 | **Timer-based expiry of `PROCESSING` payments** | Bounds the state machine and cleans up dashboards. It also silently lies about payments whose money moved. Only evidence — webhook, lookup, or settlement — may move a payment out of `PROCESSING` (FR-66, FR-85). `PENDING` (FR-73) has a hard expiry precisely because it is a state where the gateway has told us nothing is yet committed, and even there a lookup precedes expiry. |
| FALT-11 | **A generic BPM engine for onboarding** | Fast to start, and Temporal is genuinely excellent. Rejected as the *only* option because a hard dependency on an external orchestrator on the activation path adds an availability dependency and a vendor coupling to the control plane. Baseline §1.2 keeps a purpose-built durable engine behind a port with a Temporal adapter, so the choice is deferrable and reversible (FR-32 specifies the engine semantics either implementation must provide). |
| FALT-12 | **Let the compliance gate auto-approve after its SLA** | Removes a human bottleneck and improves TTFP. It also removes the accountable human from the one decision a regulator or acquirer will ask about. FR-29 *A2* escalates instead; no timer may auto-approve. |

---

## 11. Coverage summary

| Bounded context | FRs | Count |
|---|---|---|
| BC-1 Tenant & Identity | FR-01..FR-08 | 8 |
| BC-2 Merchant Registry | FR-09..FR-15 | 7 |
| BC-3 Onboarding | FR-16..FR-32 | 17 |
| BC-4 Gateway Registry & Integration | FR-33..FR-42 | 10 |
| BC-5 Configuration & Policy | FR-43..FR-52 | 10 |
| BC-6 Payment Orchestration | FR-53..FR-73 | 21 |
| BC-7 Webhook Ingestion | FR-74..FR-79 | 6 |
| BC-8 Ledger & Reconciliation | FR-80..FR-87 | 8 |
| BC-9 Audit | FR-88..FR-91 | 4 |
| **Total** | | **91** |

Every `BR-nn` in [`01-business-requirements.md`](./01-business-requirements.md) §12 is realised by at
least one FR above; CI fails on an orphan requirement (no test) or an orphan test (no requirement)
per baseline §26.
