# 05 — Security Architecture

## Principles

- **Zero Trust**: no implicit trust between network zones; every call authenticated and
  authorized regardless of network origin (pod-to-pod inside the VPC is not "trusted by default").
- **Least Privilege**: every IAM role, DB role, and Kubernetes ServiceAccount is scoped to the
  minimum permissions needed, reviewed at each production-readiness gate.
- **Defense in Depth**: no single control is trusted alone (e.g. the ledger balance invariant is
  enforced by both application logic *and* a DB constraint — see ADR-004).

## Identity & Access

| Layer | Mechanism | Why chosen | Alternative considered |
|---|---|---|---|
| Client → API | OAuth2 client-credentials (machine clients) or OIDC (user-delegated), JWT bearer tokens validated against a JWKS endpoint (Cognito or Auth0) | Industry-standard, supports scoped access tokens (`payments:write`, `payments:read`), short-lived tokens reduce blast radius of leaked credentials | API keys alone — rejected as sole mechanism; too coarse-grained and hard to rotate without client coordination. Kept as a secondary factor for partner integrations layered under OAuth2, not a replacement |
| Service → Service (in-cluster) | mTLS via service mesh (App Mesh or Istio), SPIFFE/SPIRE-style workload identity | Encrypts and authenticates pod-to-pod traffic without each service hand-rolling TLS; supports the zero-trust principle inside the cluster, not just at the edge | Plain in-cluster HTTP trusting NetworkPolicy alone — rejected; NetworkPolicy restricts *reachability* but not *identity*, insufficient for zero trust |
| Service → AWS APIs | IAM Roles for Service Accounts (IRSA) — no long-lived AWS credentials in pods | Scoped, auto-rotated, auditable via CloudTrail; eliminates static AWS access keys entirely | Static IAM user access keys mounted as secrets — explicitly rejected, long-lived credential risk |
| Human → Cluster | Kubernetes RBAC mapped to IAM/SSO identity via EKS access entries; no shared `kubectl` credentials | Individual accountability, revocable per-person, auditable | Shared admin kubeconfig — rejected |
| Authorization model | RBAC for coarse roles (admin, operator, read-only-auditor) + ABAC-style scope checks in the app layer for fine-grained resource ownership (a client can only read its own payments) | RBAC alone is too coarse for "can client X see payment Y" — that's an attribute (ownership) check the app must enforce regardless of role | Pure ABAC everywhere — rejected as unnecessary complexity for the coarse admin/operator/auditor split, which is naturally role-based |

## Data Protection

- **Encryption in transit**: TLS 1.2+ enforced at the ALB (WAF terminates and re-encrypts to the
  cluster ingress); mTLS inside the mesh; DB connections use TLS with certificate verification.
- **Encryption at rest**: Aurora storage encrypted via AWS KMS (customer-managed key, not the
  default AWS-managed key, so key rotation/revocation is under our control); S3 backups and audit
  archive encrypted with a separate KMS key; EBS volumes for EKS nodes encrypted by default.
- **Key rotation**: KMS customer-managed keys on automatic annual rotation; DB credentials rotated
  via Secrets Manager on a 30-day schedule with zero-downtime credential refresh in the app
  (short-TTL in-memory cache, background refresh before expiry).
- **Secrets management**: AWS Secrets Manager is the single source of truth for DB credentials,
  API signing keys, and third-party credentials. Kubernetes Secrets are never used to statically
  store these — the External Secrets Operator syncs from Secrets Manager into short-lived
  in-cluster secrets, and pods reference them via projected volumes, not environment variables
  (avoids leaking secrets into process listings/crash dumps/logs as easily).
- **PII/PCI scoping**: this service stores account identifiers and monetary amounts, not raw card
  data (card acquiring is explicitly out of scope, see `01-requirements.md`), reducing PCI-DSS
  scope. Any future card-data handling would require tokenization via a PCI-compliant vault
  (e.g. a dedicated tokenization service), never stored directly in this service's database.

## Network Security

- VPC with private subnets for EKS nodes and Aurora — no direct internet route; egress via NAT
  gateway only for what's explicitly allow-listed (AWS APIs via VPC endpoints where possible, to
  avoid NAT/internet exposure entirely for AWS-internal calls).
- Kubernetes `NetworkPolicy` default-deny, with explicit allow rules per service pair (payments-api
  → Aurora on 5432, payments-api → SQS via VPC endpoint, etc.) — implements microsegmentation so a
  compromised pod can't freely reach unrelated services.
- AWS WAF in front of the ALB: managed rule groups for the OWASP Top 10 (SQLi, XSS, etc.), plus a
  custom rate-based rule as a first line of defense against volumetric abuse before it reaches the
  app-level rate limiter.
- AWS Shield Standard (DDoS) by default on the ALB/CloudFront; Shield Advanced is a cost/benefit
  decision for the platform team once traffic/risk profile justifies it (documented as a scaling
  decision, not baked in from day one, per the cost-optimization NFR).
- Database in a dedicated subnet with a security group allowing inbound only from the EKS node
  security group on 5432 — nothing else can reach it, including other subnets in the VPC.

## Application-Layer Security (OWASP Top 10 mapping)

| Risk | Mitigation in this service |
|---|---|
| Injection (SQLi) | 100% parameterized queries (no string-concatenated SQL); enforced via linter + code review; least-privilege DB role |
| Broken authentication | Short-lived JWTs, JWKS signature verification, no custom crypto |
| Sensitive data exposure | Encryption in transit/at rest; structured logs scrub `amount`/account identifiers to hashed/truncated form in non-audit logs; full detail only in the access-controlled audit log |
| XXE / insecure deserialization | JSON-only API with strict schema validation (no XML parsing, no `eval`-style deserialization) |
| Broken access control | Per-request ownership check (client can only access its own payments) enforced in the service layer, tested explicitly |
| Security misconfiguration | Infrastructure as code (Terraform + Kustomize) code-reviewed like application code; no manual console changes to prod; production checklist gate |
| XSS | N/A for a JSON API with no HTML rendering; output encoding still applied defensively on any error messages that echo client input |
| Insecure deserialization / mass assignment | Explicit allow-listed request DTOs, never binding raw JSON directly onto domain/DB structs |
| Using components with known vulnerabilities | Automated dependency scanning (`govulncheck`, Trivy image scan) in CI, blocking merge on critical CVEs |
| Insufficient logging & monitoring | Structured audit log for every state-changing action; see `06-observability.md` |

## Rate Limiting & Abuse Prevention

- Edge: WAF rate-based rule (coarse, IP-based, absorbs volumetric abuse).
- App: per-client token-bucket rate limiter keyed on authenticated `client_id` (fair, doesn't
  punish other clients for one bad actor) — this is the primary defense against retry storms.
- CORS: API is server-to-server (merchant backends), so CORS is deny-by-default with no
  browser-origin access unless a specific first-party web client is added later, at which point an
  explicit allow-list (not a wildcard) would be configured.
- CSRF: not applicable to a bearer-token, non-cookie-authenticated API.

## Threat Model Summary (STRIDE, abbreviated)

| Threat | Primary mitigation |
|---|---|
| Spoofing | OAuth2/OIDC + mTLS identity everywhere |
| Tampering | TLS everywhere, DB constraints preventing invalid ledger states, append-only audit log |
| Repudiation | Immutable audit log with actor identity on every mutation |
| Information disclosure | Least-privilege IAM/DB roles, encryption at rest/in transit, log scrubbing |
| Denial of service | WAF + rate limiting + circuit breakers + autoscaling |
| Elevation of privilege | RBAC/ABAC checks server-side on every request, never trusting client-supplied role claims without JWKS-verified signature |

## Security Testing

- SAST (static analysis) and dependency/CVE scanning in CI on every PR (blocking on
  critical/high).
- Container image scanning (Trivy) before any image is allowed to be deployed.
- Periodic third-party penetration test (at minimum annually, and before major architecture
  changes) — process item, not automatable, tracked in the production checklist.
- `security-review` process applied to every PR touching auth, the ledger transaction boundary, or
  IAM/network policy.
