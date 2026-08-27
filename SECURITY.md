# Security Policy

This document covers how to report a vulnerability, what is supported, the security model in
summary, the PCI DSS scope statement, and what to do if a credential is exposed.

The binding design is [`docs/security.md`](docs/security.md) — trust model, layered controls,
identity, authorization, secret management, logging safety, the STRIDE threat model, supply chain
and incident-response hooks. The regulatory position is [`docs/compliance.md`](docs/compliance.md).
Both are subordinate to [`docs/spec/00-design-baseline.md`](docs/spec/00-design-baseline.md) §16 and
§17.

**Read this first:** this repository is a **reference implementation**. It has never processed real
money, has never been deployed to a production environment, has never held a real credential, and
has never been assessed by a QSA. Treat everything below as the *intended* policy for an operating
deployment. Nothing here should be read as a claim about a live system. See the
[status and limitations](README.md#status-and-limitations) section of the README for the specific
gaps.

---

## Reporting a vulnerability

**Do not open a public issue, a pull request, or a discussion thread for a security problem.** A
public report is a disclosure, and a disclosure before a fix is available is a disclosure to whoever
is watching.

Report privately, by whichever of these the operator of this repository has enabled:

1. **GitHub private vulnerability reporting** — the *Security* tab → *Report a vulnerability*. This
   is preferred: it creates a private fork for the fix and a CVE request path.
2. **Email to the security contact for this repository.** If you are running this code, replace this
   line with your real address and PGP key before publishing. This reference tree deliberately
   carries no address rather than an invented one, because a security contact that does not resolve
   is worse than an obvious absence.

### What to include

A report we can act on in one pass:

- The **affected component** — the deployable, package or file, and the version, commit or image
  digest.
- **What an attacker gains.** Impact in terms of the three claims this platform makes: can money
  move twice, can state become impossible, can one tenant reach another tenant's data? Anything that
  breaks one of those is treated as critical regardless of exploit complexity.
- **Reproduction** — the smallest sequence of requests, inputs or conditions that demonstrates it. A
  failing test is ideal.
- **Preconditions** — required privileges, network position, tenant tier, or configuration.
- Any **mitigations** you have identified.

**Never include real cardholder data, real credentials or real personal data in a report.** If a
proof of concept requires a credential, say so and we will provision a scoped test credential; do
not send one.

### What to expect

| Stage | Target |
|---|---|
| Acknowledgement of the report | 2 business days |
| Initial triage and severity assignment | 5 business days |
| Status update cadence while open | at least every 7 days |
| Fix or documented mitigation, critical severity | 30 days |
| Fix or documented mitigation, high severity | 60 days |
| Coordinated public disclosure | after a fix ships, by agreement with the reporter |

Severity is assigned by the impact framing above rather than by CVSS alone: a low-complexity
cross-tenant read is critical here even if its CVSS base score is not.

### Safe harbour

Good-faith research is welcome. Do not access, modify or exfiltrate data that is not yours; do not
degrade a service others depend on; do not use social engineering or physical attacks; and stop as
soon as you have demonstrated the issue. Research conducted within those limits will not be pursued.
Report promptly and give a reasonable window before public disclosure.

---

## Supported versions

**No version of this repository has been released.** There are no tags, no published images and no
supported branch. `main` is the only branch, and the only supported state is its current head.

The versioning policy that *would* apply to an operating deployment, and which the CI/CD pipeline
already implements, is:

| Aspect | Policy |
|---|---|
| Version source | An annotated Git tag. `git describe --tags --always --dirty` stamps `VERSION` into the binary and the image; `<binary> -version` answers the first question of every incident |
| Support window | The current minor and the one before it receive security fixes |
| Image identity | Immutable tags only — **there is no `latest`**. Promotion between environments moves a **digest**, not a tag, so dev, staging and production provably run the same bytes |
| Signing | Every image is `cosign`-signed **by digest**, with a CycloneDX SBOM and a build provenance attestation. Signing a tag signs a pointer that can be moved |
| API versioning | URI major version (`/v1`), additive-only within a major. Deprecation is announced with `Deprecation` and `Sunset` headers, never by an in-place change |
| Event versioning | Additive-only within a major (`.v1`). A breaking change is a new `.v2` type published **alongside** `.v1` until every consumer has migrated |
| Error-code versioning | A code is a public contract. Adding one is additive; changing an existing code's `retryable`, `http_status` or `grpc_code` is **breaking**, because client SDKs, the workflow engine and the outbox relay branch on those fields |

Dependencies are updated by Dependabot (`.github/dependabot.yml`, production dependencies
security-only); `govulncheck` runs in CI and nightly; CodeQL runs on pushes to `main` and on pull
requests that touch Go or the module graph; and `scripts/check-licences.sh` fails the build on
copyleft entering the graph.

---

## Security model in summary

Full detail in [`docs/security.md`](docs/security.md). The short version, so a reader can decide
whether to go deeper.

**Zero trust, stated concretely.** There is no trusted network position. Every request across every
boundary carries an identity: tenants and their machine clients authenticate with OAuth2
client-credentials JWTs validated against a cached, background-refreshed JWKS with a required issuer,
audience and maximum token age; humans authenticate through OIDC; and services authenticate to each
other with mesh mTLS and SPIFFE workload identities, where per-RPC peer identity is the carrier
rather than a bearer token.

**Tenant identity comes from the token and from nowhere else.** A `tenant_id` in a request body or
query string is ignored, or — if it disagrees with the token — treated as a security event
(`403 TENANT_MISMATCH`, audited, alerted). Every repository method extracts the tenant from
`context.Context` and returns `ErrMissingTenantContext` rather than querying when it is absent.
Behind that sits PostgreSQL Row-Level Security with `SET LOCAL app.tenant_id`, with the application
connecting as a **non-`BYPASSRLS`** role, and a migration gate that fails the build if a table with a
`tenant_id` column lacks either RLS or a policy. The negative test asserts that a query for tenant
B's row under tenant A's context returns zero rows **at the database level** — proving the
application guard is not the only thing standing there.

**Authorization is RBAC plus ABAC.** Scopes are narrow and per-operation (`payments:write`,
`payments:refund`, `config:write`, `credentials:rotate`, `onboarding:approve`); ABAC conditions layer
on residency, tenant tier and merchant state.

**Secrets are references, never material.** No table, environment variable, ConfigMap, image layer,
log line, trace attribute, metric label, core dump or error message may contain credential material,
and each of those is enforced by a mechanism rather than a convention: an admission policy on pod
specs, a CI scan of `deployments/k8s/**` and `helm/**`, layer scanning of built images, a full
git-history secret scan on every PR, a log-pipeline quarantine detector, `RLIMIT_CORE=0`, an
allowlist span serializer, and a metric-registry lint. In code, credential material is only ever
`Secret[T]`, whose `String()`, `MarshalJSON()` and `Format()` all return `[REDACTED]`. The database
holds a `secretref://…#vN` reference plus a SHA-256 fingerprint for rotation verification; the
material lives in AWS Secrets Manager under `/{env}/{tenant}/{merchant}/{gateway}/{purpose}` with a
per-environment KMS CMK (per-tenant for the siloed tier), reached over a VPC endpoint with IRSA
credentials, cached in memory for at most five minutes and zeroed on eviction.

**No human IAM principal can read a production secret.** An organization-level SCP denies
`secretsmanager:GetSecretValue` on `/prod/**`, so it cannot be re-granted by an account admin. Every
read is a CloudTrail event streamed to the SIEM, and a read by an unexpected principal or above the
per-service baseline alerts.

**Rotation is automated and overlapping.** Gateway API credentials ≤ 90 days with a mandatory ≥ 24 h
dual-run and automated verification before promotion; platform JWT signing keys 30 days with a
two-key JWKS window; mTLS workload certificates 24 hours at 50 % TTL; database access via IAM
authentication, so there is no long-lived password to rotate at all.

**Logging cannot leak.** Structured logging uses an **allowlist** — only registered field names are
serialized — so there is no `log.Printf("%+v", req)` path, and the linter forbids `%+v` and `%#v` on
request types. The L1 validator runs a Luhn-checked PAN detector over every string field of every
request; a hit is `400 SENSITIVE_DATA_IN_REQUEST`, the value is **not** logged, and a security event
carries the field path and length only, never the offending value.

**Encryption.** TLS 1.3 externally, mTLS between services, TLS to Postgres, Redis and Kafka
(SASL_SSL). At rest: KMS CMK per environment across Aurora, S3, EBS and Kafka, plus application-level
envelope encryption of credential material with a per-tenant DEK — so a Secrets Manager compromise
alone yields ciphertext. Right-to-erasure is implemented as crypto-shredding of the tenant's data
key, with financial records retained under the legal-obligation basis.

**Supply chain.** Distroless/scratch images with no shell; reproducible builds
(`-trimpath` + `SOURCE_DATE_EPOCH`) so a local build is byte-identical to CI's for the same commit;
SBOM and provenance attestation on every image; `cosign` signature by digest; CodeQL, `govulncheck`,
dependency review and a licence gate in CI.

**Audit is tamper-evident.** `audit_records` is a hash chain, append-only, written through the
transactional outbox so a Kafka outage backs up the outbox rather than failing a payment. Retention
is 7 years, WORM, with S3 Object Lock.

---

## PCI DSS scope statement

**Cardholder data neither traverses nor is stored by this platform.** The design intent is
assessment at **SAQ-A / A-EP** level rather than SAQ-D, and that property is structural rather than
promised.

**What the API accepts.** Exactly three payment-instrument shapes, as a closed `oneOf` with
`additionalProperties: false` on each variant: a **gateway token**, a **network-token vault
reference**, or a **stored-instrument reference**. There is no field named `pan`, `cardNumber`,
`cvv`, `cvc`, `track1`, `track2` or `expiry` anywhere in the contract — not optional, not deprecated,
absent. Every token field must **start with a letter** (`^[A-Za-z][A-Za-z0-9_.:-]{7,254}$`), and a
PAN is 13–19 digits, so a PAN cannot satisfy the pattern. This is why a bare network token — a DPAN,
which is itself PAN-formatted — is *not* accepted inline: it is referenced by vault reference
instead, keeping the digits out of request bodies entirely.

**What enforces it, independently of the schema.** The L1 validator runs a Luhn-checked PAN detector
over **every string field of every request**, including `metadata` values. A hit returns
`400 SENSITIVE_DATA_IN_REQUEST`, does not log the value, and raises a security event. Schema and
detector are belt and braces on purpose: a schema can be bypassed by a field added later, a detector
cannot.

**Where cardholder data would live if a tenant needed it.** In a card-vault service in a **separate
AWS account, separate VPC, separate cluster, with a dedicated HSM/KMS, its own change control and
its own SAQ-D assessment** — reached through a port. **It is not part of this repository**, and
adding it to this repository would be a rejected change, not a feature.

**Other regulatory boundaries.** PSD2/SCA: 3DS is a *policy outcome* of the risk engine,
per-merchant and per-corridor configurable, with exemptions (TRA, low value, MIT) modelled explicitly
and audited. GDPR: personal data is stored in the tenant's declared residency region, and the routing
engine will not select a gateway whose region violates that policy. AML/KYC: decisions and evidence
retained ≥ 5 years, immutable, with S3 Object Lock. Retention: payments and ledger 7 years, audit
7 years WORM, idempotency records 7 days, logs 30 days hot / 400 days archive, **no PII in logs**.

**Standing caveat.** None of this has been validated by a QSA, and no assessment has been performed.
The scope statement describes the design; an operating deployment would still need its own
assessment, its own network segmentation evidence and its own attestation.

---

## If a credential is exposed

Assume exposure means compromise. A credential that appeared in a log aggregator, a screenshot, a
CI artifact, a public repository or a chat message has been read by something you do not control.

**The order matters. Do not start with deletion.**

1. **Rotate first, revoke second, delete last — and preserve the evidence before any of them.**
   Deleting the old credential before an audit snapshot destroys the only record of what it could
   reach and when it was used. Take the CloudTrail/SIEM snapshot for the credential's full lifetime
   *before* revoking it.
2. **Rotate through the dual-run workflow**, not by hand: mint a new credential at the source →
   store it as a new secret version (`AWSPENDING`) → run both in parallel while new work uses the
   new version → verify N successful calls → promote to `AWSCURRENT` → revoke the old at the source
   after the soak. A hand-rotation that skips the overlap takes the money path down.
3. **Denylist the token family**, not just the individual token. If a JWT leaked, revoke by `jti`
   *and* consider the client's whole credential; if a gateway API key leaked, treat every key minted
   under that account as suspect.
4. **Scope the exposure window** from the log index: when was it created, when did it first appear
   where it should not have, what principals read it, and what did those principals do. The
   `controlId` on every security event cross-references `docs/compliance.md` §2, so an auditor can
   trace event → control → PCI requirement.
5. **Raise a security event** with `severity: CRITICAL`, `category: DATA_EXPOSURE`, and evidence
   limited to allowlisted structured fields. **Never put the offending value in the event** — the
   field path and length only. Putting a leaked credential into the incident record leaks it again,
   into a system with a seven-year retention.
6. **If the credential was committed to git, rotate it regardless of whether it was real.**
   `scripts/check-secrets.sh` (stage 13 of `make verify`) scans the working tree for private key
   blocks, high-confidence provider credential shapes, generic secret-named assignments and
   Luhn-valid 13–19 digit runs, and blocks the merge — but a value that has already been pushed has
   been fetched by every clone and every cache. Rewriting history does not un-fetch it.
7. **If cardholder data may have been in scope**, follow the PAN-incident path instead: quarantine,
   scope the exposure window from the log index, and notify the QSA and the acquirer per contract.
   This is the one class where external notification obligations run on a clock you do not control.
8. **Close the loop.** Every incident ends with the same two steps: a blameless postmortem, and a
   new or strengthened control **plus the test that would have caught it**. The test is what makes
   the fix durable — without it, the control is a decision that decays.

The response playbook pointers in [`docs/security.md`](docs/security.md) §9.3 name a runbook per
class — tenant isolation, credential rotation, PCI incident, supply chain, audit tamper.
**Those runbooks have not been written**: `docs/runbooks/` is empty in this tree. The first actions
above are the substance of what they would contain; writing them is tracked as a known gap in the
[README](README.md#status-and-limitations).

---

## Reporting a security problem in this documentation

If a document here describes a control that the code does not implement, that is a security defect
and is in scope for a report. Several are already known and listed in the README's status section —
notably that the per-package coverage gates, the mutation-probe harness and every operational
runbook are referenced but absent. A new one is worth reporting.
