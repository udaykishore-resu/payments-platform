# Security Architecture

> **Purpose:** the binding security design for the payment orchestration platform — trust model, layered controls, identity, authorization, secrets, threat model, supply chain and incident response.
> **Derived from:** `docs/spec/00-design-baseline.md` — normatively §16 (multi-tenancy), §17 (PCI scope), §20 (error model), §24 (failure catalog). Where this document and the baseline disagree, the baseline wins and this document is a defect.

---

## 1. Trust model — Zero Trust, stated concretely

Zero Trust is not "we bought a mesh". It is four claims that must each be enforceable and testable.

| Claim | Concrete meaning here | Enforcement point | Test |
|---|---|---|---|
| **No implicit trust from network position** | Being inside the VPC, the cluster, or the namespace grants zero authority. A pod in `pp-data` may not call `control-plane-api` merely because it can route to it. | NetworkPolicy default-deny (§3.3) + mesh `AuthorizationPolicy` keyed on SPIFFE ID, not IP | `tests/integration/netpolicy_test.go::TestDefaultDenyBlocksUnlistedFlow` |
| **Every hop is authenticated** | Client→edge: TLS 1.3 + OAuth2/mTLS. Edge→service: mTLS. Service→service: mTLS with SPIFFE identity. Service→AWS: IRSA (OIDC federation, no static keys). Service→Postgres: TLS + IAM auth token. Service→Kafka: SASL_SSL (mTLS principal). | Envoy sidecar `PeerAuthentication: STRICT`; Postgres `hostssl … clientcert=verify-full`; Kafka `ssl.client.auth=required` | `tests/integration/mtls_test.go::TestPlaintextConnectionRefused` |
| **Every request is authorized** | Authentication answers *who*; a separate mandatory stage answers *may they* (§12 stage 5). Default deny. Tenant isolation is a distinct stage *before* authorization (stage 4) so a valid token for tenant A can never reach tenant B's data even if the policy engine has a bug. | `internal/platform/authz` + `internal/policies` + Postgres RLS | `tests/integration/authz_test.go`, `TestCrossTenantAccessIsImpossible` (§16.2) |
| **Verification is continuous, not at-connect** | Tokens are short-lived (≤ 15 min access) and re-validated per request — never cached as "session established". mTLS certs are 24 h with 12 h rotation. Health of an identity (revocation list, tenant suspension, merchant suspension) is re-checked per request against a ≤ 30 s-stale cache with priority invalidation on `merchant.suspended.v1`. | Per-request JWT validation; SDS cert rotation; §13.2 priority invalidation | `tests/integration/revocation_test.go::TestSuspendedTenantRejectedWithin30s` |

### 1.1 Identity taxonomy

| Principal class | Identity | Issued by | Lifetime | Where it appears |
|---|---|---|---|---|
| Human operator | OIDC ID token + access token, IdP-backed, MFA-mandatory (WebAuthn; TOTP is a fallback only for break-glass) | Corporate IdP | 8 h session, 15 min access token | `control-plane-api`, admin console, `platformctl` |
| Machine client (tenant integration) | OAuth2 **client-credentials**, per `(tenant, api_client)` | Platform authorization server | 15 min access token, no refresh token | `payment-api`, `control-plane-api` |
| Workload (our own services) | SPIFFE-style X.509 SVID: `spiffe://pp.internal/ns/{namespace}/sa/{service-account}` | Cluster CA (mesh control plane), private CA rooted in AWS Private CA | 24 h cert, rotated at 50 % TTL | Service-to-service mTLS, mesh authz |
| Workload → AWS | IRSA: k8s SA token → STS `AssumeRoleWithWebIdentity` → per-deployable IAM role | EKS OIDC provider + IAM | 1 h STS credentials, auto-refreshed | Secrets Manager, KMS, S3, Kafka (MSK IAM), RDS IAM auth |
| Gateway (inbound webhook) | HMAC or asymmetric signature over the raw body, per gateway's scheme | The gateway | Per-request, ±5 min window | `webhook-ingress` only |
| Break-glass | Named human role, hardware-token MFA, time-boxed 60 min, session-recorded, auto-audited, dual-approval to grant | IdP + IAM session policy | 60 min, non-renewable | Emergency only; every use pages the security channel |

**Rule:** there is no shared account, no long-lived static AWS key, no bearer token that outlives 15 minutes, and no service account whose credential a human can read.

### 1.2 SPIFFE ID → workload mapping

| Deployable (§5) | SPIFFE ID | IAM role | May initiate connections to |
|---|---|---|---|
| `payment-api` | `spiffe://pp.internal/ns/pp-data/sa/payment-api` | `pp-payment-api` | `payment-orchestrator`, Redis, Postgres (read replica), Secrets Manager (JWKS cache seed) |
| `payment-orchestrator` | `…/ns/pp-data/sa/payment-orchestrator` | `pp-payment-orchestrator` | Postgres (writer), Redis, Kafka, NAT→gateway allowlist, Secrets Manager, KMS |
| `control-plane-api` | `…/ns/pp-control/sa/control-plane-api` | `pp-control-plane-api` | Postgres (writer), Kafka, Secrets Manager, KMS |
| `workflow-worker` | `…/ns/pp-automation/sa/workflow-worker` | `pp-workflow-worker` | Postgres, Kafka, NAT→gateway+KYC+bank allowlist, Secrets Manager, KMS, S3 |
| `webhook-ingress` | `…/ns/pp-data/sa/webhook-ingress` | `pp-webhook-ingress` | Postgres (writer, `inbound_webhooks` only), Secrets Manager (webhook secrets) |
| `outbox-relay` | `…/ns/pp-data/sa/outbox-relay` | `pp-outbox-relay` | Postgres (writer, `outbox_events` only), Kafka (producer) |
| `event-consumer` | `…/ns/pp-data/sa/event-consumer` | `pp-event-consumer` | Kafka (consumer), Postgres (writer, projections/ledger/audit), S3 |

Anything not listed in the last column is denied by NetworkPolicy *and* by mesh `AuthorizationPolicy` *and* by IAM. Three independent denials; a single misconfiguration does not open a path.

---

## 2. Defence in depth — the layer stack

```
Internet
  │  L0  CloudFront + AWS Shield Advanced + WAF        DDoS, bot, PAN-shaped-body reject
  ▼
  │  L1  ALB, TLS 1.3 termination, mTLS optional       cipher policy, HSTS, no TLS<1.2
  ▼
  │  L2  VPC: public subnets hold only the ALB & NAT   no data-plane public egress
  ▼
  │  L3  Private subnets, SGs (stateful) + NACLs       coarse subnet-level backstop
  ▼
  │  L4  EKS: namespaces, NetworkPolicy default-deny   PodSecurity restricted, seccomp
  ▼
  │  L5  Mesh mTLS + AuthorizationPolicy (SPIFFE)      workload identity, not IP
  ▼
  │  L6  App: authn → tenant guard → authz → limits    §12 pipeline, stages 3–6
  ▼
  │  L7  Data: RLS, envelope encryption, KMS, WORM     database is the last defender
```

Each layer assumes every layer above it has already been defeated. That assumption is what makes the stack worth having.

### 2.1 Edge (L0–L1)

**TLS policy.** TLS 1.3 preferred, TLS 1.2 permitted only for the public API with the restricted suite list below; TLS 1.0/1.1 and all of SSL are rejected at the ALB policy level (`ELBSecurityPolicy-TLS13-1-2-Res-2021-06`). Internal traffic is TLS 1.3 only — we control both ends, so there is no compatibility argument.

| Setting | Value | Reasoning |
|---|---|---|
| Min version (public) | TLS 1.2 | A small tail of merchant server stacks still lacks 1.3; 1.2 with AEAD-only suites is acceptable, 1.1 is not (no AEAD, SHA-1 PRF) |
| Min version (internal, mesh, DB, Kafka) | TLS 1.3 | Both ends are ours; 1.3 removes renegotiation, static RSA and CBC entirely |
| TLS 1.3 suites | `TLS_AES_256_GCM_SHA384`, `TLS_AES_128_GCM_SHA256`, `TLS_CHACHA20_POLY1305_SHA256` | The full sane 1.3 set; ChaCha for mobile clients without AES-NI |
| TLS 1.2 suites | `ECDHE-ECDSA-AES256-GCM-SHA384`, `ECDHE-RSA-AES256-GCM-SHA384`, `ECDHE-ECDSA-AES128-GCM-SHA256`, `ECDHE-RSA-AES128-GCM-SHA256` | ECDHE only (forward secrecy), GCM only (no CBC → no Lucky13/BEAST class), no RSA key exchange |
| Curves | `X25519`, `secp256r1` | X25519 preferred; P-256 for FIPS-constrained clients |
| Session tickets | Rotated every 12 h, per-region key, no ticket reuse across regions | A stolen static ticket key breaks forward secrecy for every session it covers |
| HSTS | `Strict-Transport-Security: max-age=63072000; includeSubDomains; preload` | 2 years, preloaded. Payment APIs must never be reachable over cleartext, even for a first request |
| Other headers | `X-Content-Type-Options: nosniff`, `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store` on every `/v1/**` response | The API returns JSON only; `default-src 'none'` makes any injected HTML inert. `no-store` prevents payment responses landing in an intermediary cache |
| OCSP stapling | On, must-staple on the leaf | Removes the client-side OCSP soft-fail hole |
| Certificate | ACM, RSA-2048 + ECDSA P-256 dual cert, 13-month max, CT-logged, CAA pinned to our CA | CAA + CT monitoring is the practical defence against mis-issuance |

**WAF rules that actually matter for a payment API.** Generic OWASP rule sets are noisy on JSON APIs; these are the rules kept in `Block` and why.

| # | Rule | Action | Reasoning |
|---|---|---|---|
| W1 | Body size > 256 KB on `/v1/payments*` | Block | A payment instruction is < 8 KB. Anything larger is an attack or a bug; blocking early protects the JSON parser |
| W2 | Body matches PAN shape (13–19 digits, Luhn-valid, separators stripped) | Block, log **without** the body, emit security event | Second line of the §17.2 PAN detector. Blocking at the edge means the PAN never reaches an application log buffer at all |
| W3 | `Content-Type` not `application/json` on `/v1/**` mutations | Block | Kills multipart/form smuggling and most CSRF-shaped confusion |
| W4 | Request contains `Transfer-Encoding` **and** `Content-Length` | Block | Request smuggling (CL.TE / TE.CL) desync — the class that lets an attacker prepend to another tenant's request |
| W5 | Header count > 60, single header > 8 KB, URI > 2 KB | Block | Parser-differential and resource-exhaustion primitives |
| W6 | Rate-based: > 2 000 req / 5 min per source IP on `/v1/payments` | Block 10 min | Coarse pre-auth backstop only. Real limits are per-tenant, post-auth (§6 of `failure-handling.md`) — IP limits alone are useless behind NAT and CGNAT |
| W7 | Rate-based: > 100 req / 5 min per IP on `/oauth2/token` | Block 30 min | Credential stuffing against client-credentials |
| W8 | Distinct card-token count > 30 per merchant per 10 min with authorization rate < 20 % | Alert + challenge | **Card testing signature.** See T-1 in §8.2 |
| W9 | AWS managed `AWSManagedRulesKnownBadInputsRuleSet` | Block | Log4Shell-class payloads, path traversal, null bytes |
| W10 | AWS managed `AWSManagedRulesAmazonIpReputationList` | Block | Known scanners/botnets; cheap, low false-positive |
| W11 | Geo-block for countries in the platform sanctions list (`KP`, `IR`, …) | Block | Sanctions posture (§23 `blockedCountries`) enforced at the edge as well as in policy |
| W12 | SQLi/XSS managed rules on `/v1/**` | **Count**, not Block | The API is parameterized-SQL and JSON-only; these rules false-positive on legitimate merchant descriptors ("O'Brien & Sons"). Counting keeps the signal without breaking merchants |

Rules W1–W5 and W9–W11 are evaluated before the rate-based rules so a malformed flood is dropped at the cheapest possible stage.

**DDoS.** Shield Advanced with the DDoS Response Team engaged; CloudFront absorbs L3/L4; ALB is not directly resolvable (only the CloudFront distribution's origin, locked by a shared secret header and the AWS-managed prefix list). L7 volumetric is handled by W6/W7 plus adaptive concurrency (`failure-handling.md` §7). Health-check paths (`/healthz`, `/readyz`) are not exposed at the edge at all.

### 2.2 Network (L2–L3)

| Element | Design | Reasoning |
|---|---|---|
| VPC | One per environment per region. `/16`. No VPC peering between environments — ever | Peering is a lateral-movement path and a route-table foot-gun |
| Subnet tiers | `public/*` (ALB + NAT only, `/24` × 3 AZ) · `app/*` (EKS nodes, `/20` × 3 AZ) · `data/*` (Aurora, MSK, ElastiCache, `/22` × 3 AZ) | Three tiers, three AZs. `data/*` has **no route to any NAT gateway** |
| Egress | Data-plane pods egress **only** via a dedicated NAT in `public/*` whose route is paired with a **domain-allowlisted** egress proxy. Everything else is `VPC endpoint` or denied | The single largest exfiltration channel in a payments platform is a compromised dependency making an outbound HTTPS call. Removing general egress removes the channel |
| Egress allowlist | `api.stripe.com`, `*.adyen.com`, `api-m.paypal.com`, KYC vendor, bank-validation vendor, IdP JWKS. Per-destination, per-service-account. `webhook-ingress` and `event-consumer` have **no** internet egress | Enumerated in `terraform/modules/egress-allowlist`. A new destination is a reviewed PR, not a runtime decision |
| Merchant webhook egress | Delivered from a **separate** egress path (`webhook-sender` NAT + SSRF-guarding proxy, §8.2 T-10) that resolves and pins the IP before connect and refuses RFC1918/link-local/metadata targets | Merchant-supplied URLs are attacker-controlled input |
| VPC endpoints | Interface: Secrets Manager, KMS, STS, ECR (api+dkr), CloudWatch Logs, SSM, Kinesis(Firehose), Kafka(MSK). Gateway: S3, DynamoDB. All with restrictive endpoint policies | AWS-service traffic never touches the internet, and the endpoint policy is a second `aws:PrincipalArn`/`aws:PrincipalTag` check independent of IAM |
| Security groups | Referenced by SG-ID, never CIDR, for all intra-VPC flows. One SG per role. Aurora SG ingress: only `app-nodes` SG on 5432. MSK SG ingress: only `app-nodes` SG on 9094 | SG-to-SG references survive IP churn; a CIDR rule silently widens as subnets grow |
| NACLs | Stateless backstop: `data/*` denies all inbound from `0.0.0.0/0`, allows only `app/*` CIDRs on the datastore ports and ephemeral return. `public/*` denies inbound to 22/3389 | NACLs cannot express identity, so they are a coarse second opinion — valuable exactly because they fail differently from SGs |
| SSH / bastion | None. No SSH daemon on nodes. Node access is SSM Session Manager only, IAM-gated, session-logged to S3 | An SSH key is a long-lived credential a human can copy |
| Flow logs | VPC flow logs (all, including ACCEPT) → S3 + Athena; DNS query logs → CloudWatch | Exfiltration is usually visible in DNS before it is visible anywhere else |
| IMDS | IMDSv2 required, hop limit 1, IMDS disabled entirely on pods via `hostNetwork: false` + NetworkPolicy deny to `169.254.169.254` | Blocks the classic SSRF→instance-credentials chain |

### 2.3 Cluster (L4)

**Namespaces** map to planes (§2 of the baseline): `pp-control`, `pp-automation`, `pp-data`, `pp-observability`, `pp-system`. Cross-namespace traffic is default-denied and enumerated below.

**NetworkPolicy: default deny, both directions, every namespace.**

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-all
  namespace: pp-data
spec:
  podSelector: {}
  policyTypes: [Ingress, Egress]
---
# DNS is the only universally granted egress. Note the port-and-namespace scoping:
# a blanket "allow UDP/53 anywhere" is an exfiltration tunnel.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-dns
  namespace: pp-data
spec:
  podSelector: {}
  policyTypes: [Egress]
  egress:
    - to:
        - namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: kube-system } }
          podSelector: { matchLabels: { k8s-app: kube-dns } }
      ports: [{ protocol: UDP, port: 53 }, { protocol: TCP, port: 53 }]
```

The exact allowed flows — this table is the complete set; anything absent is denied:

| From | To | Port | Why |
|---|---|---|---|
| `ingress-nginx` (pp-system) | `payment-api`, `webhook-ingress` (pp-data) | 8443 | Public data-plane ingress |
| `ingress-nginx` (pp-system) | `control-plane-api` (pp-control) | 8443 | Admin ingress (separate ALB, separate hostname, IdP-gated) |
| `payment-api` (pp-data) | `payment-orchestrator` (pp-data) | 9443 (gRPC) | The only caller of the orchestrator |
| `payment-orchestrator`, `control-plane-api`, `workflow-worker`, `webhook-ingress`, `outbox-relay`, `event-consumer` | Aurora endpoint (data subnets) | 5432 | Persistence |
| `payment-api`, `payment-orchestrator` | ElastiCache (data subnets) | 6379 | Idempotency accelerator, rate limits |
| `outbox-relay`, `event-consumer`, `control-plane-api`, `payment-orchestrator` | MSK (data subnets) | 9094 | Kafka SASL_SSL |
| `payment-orchestrator`, `workflow-worker` | egress-proxy (pp-system) | 3128 | The only path to gateway/vendor APIs |
| `webhook-sender` (pp-data) | ssrf-guard-proxy (pp-system) | 3129 | The only path to merchant-supplied URLs |
| all pods | kube-dns (pp-system) | 53 | Name resolution |
| all pods | VPC endpoint ENIs | 443 | AWS APIs (Secrets Manager, KMS, STS, S3) |
| `prometheus` (pp-observability) | all pods | 9090 (`/metrics`) | Scraping — **ingress-only**, the scraped pod gets no reciprocal egress |
| **Denied and worth naming** | `payment-api` → Aurora **writer** | — | The API never writes payment state; the orchestrator does. Enforced at network *and* DB-role level |
| **Denied and worth naming** | `pp-data` → `pp-control` | — | The data plane must never have a synchronous dependency on the control plane (§15 fail-static). The network makes that architectural rule physically true |
| **Denied and worth naming** | any pod → `169.254.169.254` | — | IMDS |

Example of a concrete allow, showing that the selector is on identity labels, not IP:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: orchestrator-ingress-from-api
  namespace: pp-data
spec:
  podSelector:
    matchLabels: { app.kubernetes.io/name: payment-orchestrator }
  policyTypes: [Ingress]
  ingress:
    - from:
        - podSelector:
            matchLabels: { app.kubernetes.io/name: payment-api }
      ports: [{ protocol: TCP, port: 9443 }]
```

**Pod hardening.** `PodSecurity` admission in `enforce: restricted` on every `pp-*` namespace (`audit: restricted`, `warn: restricted` too, so violations surface in CI and at apply time).

```yaml
# deployments/k8s/base/payment-orchestrator/deployment.yaml (security fragment)
spec:
  template:
    spec:
      automountServiceAccountToken: false        # see note below
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532                         # distroless "nonroot"
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile: { type: RuntimeDefault }
      containers:
        - name: orchestrator
          image: <registry>/payment-orchestrator@sha256:…   # digest, never a tag
          securityContext:
            allowPrivilegeEscalation: false
            privileged: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }      # add: [] — we need none, not even NET_BIND_SERVICE
          ports: [{ containerPort: 9443 }]       # >1024, so no capability is required to bind
          volumeMounts:
            - { name: tmp, mountPath: /tmp }
            - { name: cache, mountPath: /var/cache/pp }
      volumes:
        - { name: tmp,   emptyDir: { medium: Memory, sizeLimit: 64Mi } }
        - { name: cache, emptyDir: { sizeLimit: 256Mi } }
```

| Control | Value | Reasoning |
|---|---|---|
| `runAsNonRoot` + numeric UID 65532 | required | A numeric UID (not a name) means the check is enforceable at admission; a name requires image inspection |
| `readOnlyRootFilesystem` | true | Removes the write primitive most post-exploitation tooling needs. Writable paths are explicit, `noexec` where possible, and memory-backed for `/tmp` |
| `capabilities: drop ALL` | no `add` | Services listen on ports > 1024 precisely so `NET_BIND_SERVICE` is unnecessary |
| `seccompProfile: RuntimeDefault` | required | Blocks ~ 300 rarely used syscalls, including most container-escape primitives. A custom profile per service was evaluated and rejected: the maintenance cost exceeded the marginal benefit over `RuntimeDefault` + dropped caps + read-only rootfs |
| `automountServiceAccountToken: false` | default | Most services need no Kubernetes API access. Where a projected token *is* needed (IRSA), it is mounted explicitly as a bounded-audience projected volume, not the legacy secret-backed token |
| IRSA token | `serviceAccountToken` projected volume, `audience: sts.amazonaws.com`, `expirationSeconds: 3600` | Audience-bound and short-lived: a stolen token is useless against the Kubernetes API and expires in an hour |
| Resource limits | Requests and limits set on every container; no `BestEffort` pods in `pp-data` | An unbounded pod is a noisy-neighbour and a DoS amplifier |
| Topology | `topologySpreadConstraints` across 3 AZs, `PodDisruptionBudget` minAvailable 2/3 | §24 node loss / AZ loss |
| Admission | Kyverno/Gatekeeper policies: deny `:latest`, deny unsigned images (§9), deny `hostPath`/`hostNetwork`/`hostPID`, deny `NodePort`, require `runAsNonRoot`, require resource limits, require the `pp.tenant-tier` and `pp.plane` labels | Admission is where a bad manifest is cheapest to stop |
| Runtime | Falco with rules for: shell spawned in a container, write to `/proc/*/mem`, outbound connection to a non-allowlisted destination, read of a secrets mount by a non-owning process | Detection for what prevention missed |

### 2.4 Service (L5–L6)

- **mTLS everywhere internal.** `PeerAuthentication: STRICT` per namespace; plaintext ports are not exposed at all. The mesh `AuthorizationPolicy` is keyed on the SPIFFE principal:

```yaml
apiVersion: security.istio.io/v1
kind: AuthorizationPolicy
metadata: { name: orchestrator-callers, namespace: pp-data }
spec:
  selector: { matchLabels: { app.kubernetes.io/name: payment-orchestrator } }
  action: ALLOW
  rules:
    - from:
        - source:
            principals: ["cluster.local/ns/pp-data/sa/payment-api"]
      to:
        - operation:
            methods: ["POST"]
            paths: ["/pp.payment.v1.PaymentOrchestrator/*"]
```

- **Authn/authz middleware order is fixed** and mirrors §12 stages 3–6: `RequestID → Trace → Authn → TenantGuard → Authz → RateLimit/Bulkhead → Validation(L1)`. The tenant guard sits **between** authn and authz deliberately: authorization decisions are evaluated inside an already-pinned tenant scope, so a policy bug cannot produce a cross-tenant grant.
- **The internal gRPC surface is not a trust shortcut.** `payment-orchestrator` re-derives the tenant from the propagated identity context and re-applies the isolation guard. "It came from `payment-api`, so it is fine" is precisely the implicit trust Zero Trust forbids.

### 2.5 Data (L7)

| Control | Implementation | Reasoning |
|---|---|---|
| Encryption at rest | KMS CMK per environment (per tenant for siloed tier, §16.1): Aurora storage, EBS/gp3, S3 (SSE-KMS, bucket key on), MSK, ElastiCache, EBS snapshots, RDS backups | Baseline §17.2. Customer-managed keys, not AWS-managed, so key policy and grant history are ours and auditable |
| Encryption in transit | TLS 1.3 to Postgres (`sslmode=verify-full`, server cert pinned to RDS CA bundle), Redis in-transit encryption on, Kafka SASL_SSL | `verify-full`, not `require`: `require` accepts any cert and defeats the point |
| Row-Level Security | Every tenant-scoped table; app role is not `BYPASSRLS`; `SET LOCAL app.tenant_id` per transaction | Full detail in `multi-tenancy.md` §2 |
| Column/field-level envelope encryption | Sensitive fields (gateway credentials, KYC artifact references, bank account numbers, merchant principal PII) are encrypted with a per-tenant data key; the DEK is wrapped by the tenant KMS CMK and stored alongside the ciphertext | Makes crypto-shredding possible (destroy the CMK/DEK → the ciphertext is unrecoverable) and limits blast radius of a database dump to "ciphertext plus wrapped keys" |
| Envelope format | `v1.<kms_key_id>.<b64 wrapped_dek>.<b64 nonce>.<b64 ciphertext>` — AES-256-GCM, 96-bit random nonce, AAD = `tenant_id \|\| table \|\| column \|\| row_pk` | AAD binding means ciphertext cannot be moved between rows, columns or tenants — a copy-paste attack on the database produces a decryption failure, not a leak |
| DEK caching | Decrypted DEKs cached in memory ≤ 5 min, never written to disk, zeroed on eviction, never in a core dump (`GODEBUG=madvdontneed=1`, core dumps disabled via `RLIMIT_CORE=0`) | Bounds KMS cost without creating a durable plaintext key |
| Backups | Aurora automated backups + cross-region snapshot copy, both KMS-encrypted; restore drills quarterly | A backup restored into a less-protected environment is a classic leak; restore targets are locked to equally-classified accounts by KMS key policy |
| Deletion | Crypto-shredding of the tenant DEK/CMK, with the financial-records carve-out (see `compliance.md` §4.5) | Physical deletion across replicas, backups and WAL is not achievable in bounded time; key destruction is |
| Query safety | Parameterized statements only; `sqlc`-generated accessors; the linter forbids `fmt.Sprintf` into any identifier that reaches a `Query`/`Exec` | SQL injection is prevented structurally, not by review vigilance |

### 2.6 Secrets (L7, and see §5)

Covered in full in §5. The single-sentence version: secrets live in AWS Secrets Manager under an IAM-path-scoped prefix, are fetched at runtime over a VPC endpoint using IRSA, are held in memory as `Secret[T]`, and never touch an environment variable, a ConfigMap, an image layer, a log line, or a crash dump.

---

## 3. Authentication

### 3.1 Machine clients — OAuth2 client-credentials

| Property | Value | Reasoning |
|---|---|---|
| Grant | `client_credentials` only | There is no user at the other end of a merchant's server-to-server integration; auth-code and password grants are not offered |
| Client authentication | `private_key_jwt` (RFC 7523, `ES256`) preferred; `client_secret_basic` permitted for legacy tenants with a 12-month migration deadline | A shared secret is replayable off a single log leak; a signed assertion is not |
| Access token | JWT, `ES256`, 15 min TTL, audience `https://api.example.com`, scopes per §19 | 15 min bounds the value of a stolen token to a window shorter than most exfiltration-to-use loops |
| Refresh token | **None** for client-credentials | The client already holds a credential that can mint a new access token; a refresh token would be a second, longer-lived secret with no benefit |
| Claims | `iss`, `aud`, `sub` (= `cli_…`), `exp`, `nbf`, `iat`, `jti`, `tenant_id`, `scope`, `merchant_scope` (optional), `env`, `cnf` (optional, mTLS thumbprint) | `tenant_id` in the token is the *only* source of tenant identity (§16.2) |
| Sender constraining | `cnf.x5t#S256` (RFC 8705 mTLS-bound tokens) available and **required** for `payments:refund`, `credentials:rotate` and every control-plane write | Binds the token to the TLS client cert: a stolen bearer token is unusable without the private key. Applied to the operations where theft is most costly |
| Rate limit on `/oauth2/token` | 100/5 min per IP (W7), 20/min per client | Credential stuffing and secret brute-forcing |
| Client secret rotation | Two-secret overlap window, 90 days max age, `POST /v1/clients/{id}/secrets` mints the new one; old one revocable immediately | Zero-downtime rotation requires the overlap; without it every rotation is an outage and therefore never happens |

### 3.2 Humans — OIDC

- Authorization Code + PKCE (S256), no implicit, no hybrid.
- MFA mandatory. WebAuthn/FIDO2 required for `platform-admin` and `credentials:rotate`; TOTP acceptable for `auditor` and `operator`. SMS is not an accepted factor.
- ID token validated for `nonce` and `at_hash`; access token validated per §3.3.
- Session 8 h absolute, 30 min idle, re-auth (`prompt=login` + `max_age=0`) required before any dual-control approval or credential rotation.
- Group→role mapping is derived from the IdP group claim through an explicit mapping table in `config/`, never from a free-form claim, and never trusting a group name the IdP does not own.
- Device posture (managed device, disk encryption, OS patch level) is asserted by the IdP as a claim and required for `platform-admin`.

### 3.3 JWT validation rules

These are non-negotiable and are implemented once in `internal/platform/authn`. The order matters: cheap structural rejections precede cryptographic work, which precedes network work.

```go
// internal/platform/authn/jwt.go (abridged, but the checks are complete)

var allowedAlgs = map[string]bool{"ES256": true, "RS256": true} // allowlist, never from the token

const (
    maxClockSkew  = 60 * time.Second // symmetric tolerance for nbf/exp/iat
    maxTokenAge   = 15 * time.Minute // independent of exp: bounds a bad issuer config
    maxTokenBytes = 8 << 10
)

func (v *Validator) Validate(ctx context.Context, raw string) (*Claims, error) {
    if len(raw) > maxTokenBytes {
        return nil, errTokenTooLarge // DoS guard before any parsing
    }
    tok, err := jwt.ParseSigned(raw, algNames(allowedAlgs))
    if err != nil {
        return nil, errMalformed
    }

    // 1. Algorithm. The header is attacker-controlled; only the allowlist decides.
    //    Rejects alg=none, and rejects HS256 outright so a public RSA/EC key can never
    //    be replayed as an HMAC secret (the classic confusion attack).
    hdr := tok.Headers[0]
    if !allowedAlgs[hdr.Algorithm] {
        return nil, errAlgNotAllowed
    }

    // 2. Key resolution is by (iss, kid) — never by an embedded jwk/jku/x5u header,
    //    which would let the token nominate its own key.
    if hdr.JSONWebKey != nil || hdr.ExtraHeaders["jku"] != nil || hdr.ExtraHeaders["x5u"] != nil {
        return nil, errEmbeddedKeyRejected
    }
    if hdr.KeyID == "" {
        return nil, errMissingKid
    }

    var unsafe Claims
    if err := tok.UnsafeClaimsWithoutVerification(&unsafe); err != nil {
        return nil, errMalformed
    }
    // 3. Issuer allowlist BEFORE key lookup, so an unknown iss cannot drive a JWKS fetch.
    iss, ok := v.issuers[unsafe.Issuer]
    if !ok {
        return nil, errUntrustedIssuer
    }
    key, err := v.jwks.Key(ctx, unsafe.Issuer, hdr.KeyID) // cache; see below
    if err != nil {
        return nil, errUnknownKey
    }
    // 4. The key's declared alg must match the header alg — a key published as ES256
    //    may not be used to verify an RS256 token.
    if key.Algorithm != hdr.Algorithm {
        return nil, errAlgKeyMismatch
    }

    var c Claims
    if err := tok.Claims(key.Public(), &c); err != nil { // signature verified here
        return nil, errBadSignature
    }

    now := v.clock.Now()
    switch {
    case c.Audience != iss.ExpectedAudience:        // exact match, not prefix, not "contains"
        return nil, errBadAudience
    case c.Expiry == nil || now.After(c.Expiry.Time().Add(maxClockSkew)):
        return nil, errExpired
    case c.NotBefore != nil && now.Add(maxClockSkew).Before(c.NotBefore.Time()):
        return nil, errNotYetValid
    case c.IssuedAt == nil || now.Sub(c.IssuedAt.Time()) > maxTokenAge+maxClockSkew:
        return nil, errStale
    case c.IssuedAt.Time().After(now.Add(maxClockSkew)):
        return nil, errIssuedInFuture
    case c.ID == "":                                 // jti required for replay tracking
        return nil, errMissingJTI
    case c.TenantID == "" || !ids.HasPrefix(c.TenantID, "ten_"):
        return nil, errMissingTenant
    case c.Env != v.env:                             // a staging token must not work in prod
        return nil, errEnvMismatch
    }
    // 5. Replay: jti seen within its own TTL window. Redis SETNX with TTL = exp-now;
    //    on Redis failure this degrades to "not enforced" and raises a metric —
    //    it never fails the request, because a Redis outage must not stop payments (§24).
    if seen, err := v.replay.SeenBefore(ctx, c.Issuer, c.ID, c.Expiry.Time()); err == nil && seen {
        return nil, errTokenReplayed
    }
    // 6. Revocation: token-level (jti denylist) and principal-level (client/tenant
    //    suspension), both from a ≤30s-stale cache with priority push invalidation.
    if v.revoked.Check(ctx, c) {
        return nil, errRevoked
    }
    // 7. Sender constraining, where the scope demands it.
    if iss.RequireMTLSBinding(c.Scope) {
        if err := verifyCnfThumbprint(ctx, c.Confirmation); err != nil {
            return nil, errTokenNotBoundToClient
        }
    }
    return &c, nil
}
```

| Rule | Value | Reasoning |
|---|---|---|
| Algorithm allowlist | `{ES256, RS256}`, taken from configuration, never from the token header | Defeats `alg: none` and the RS256→HS256 confusion attack in one move. HMAC algorithms are excluded entirely so there is no symmetric path to confuse |
| `jwk`/`jku`/`x5u` headers | Rejected outright | Otherwise the token nominates the key that verifies it |
| `kid` | Required, and `(iss, kid)` must resolve to a known key | Prevents JWKS-fetch amplification and key-confusion across issuers |
| Issuer | Exact-match allowlist, checked *before* any key fetch | An unknown `iss` must never cause an outbound request (SSRF via JWKS) |
| Audience | Exact string equality against the per-issuer expected audience | `strings.Contains`-style audience checks are a known cross-service token-replay bug |
| Clock skew | ±60 s | Matches NTP discipline in §24 (clock skew). Larger windows extend the life of expired tokens; smaller windows generate false rejections during leap-second and NTP-step events |
| `iat` max age | 15 min + skew | Independent backstop if an issuer misconfigures `exp` |
| `jti` | Required; replay-checked for the token's remaining lifetime | Turns a captured token into a single-use artifact for the classes we care about |
| `env` claim | Must equal the deployment environment | Stops a staging credential from ever authenticating in production — a real and common incident cause |
| JWKS cache | In-memory, TTL 10 min, **background refresh every 5 min** (never synchronous refresh on the request path), stale-if-error up to 24 h, negative-cache unknown `kid` for 30 s, per-issuer refresh rate limit of 1 fetch / 30 s | A synchronous JWKS fetch on a cache miss is a self-inflicted DoS: one key rotation plus a burst of traffic becomes a thundering herd against the IdP. Stale-if-error means an IdP outage degrades to "keep validating with known keys" rather than "reject all traffic" |
| JWKS fetch | Only to the allowlisted issuer host, via the egress proxy, TLS `verify-full`, response ≤ 64 KB, ≤ 10 keys, 2 s timeout | Bounds an SSRF or a malicious-IdP response |
| Key rotation tolerance | Two overlapping platform signing keys published for 30 days each (§17.2) | A verifier that has not yet refreshed still validates tokens signed with the previous key |
| Failure mode | Every failure returns the same `401 UNAUTHENTICATED` body (baseline §20); the specific reason goes to logs and metrics only | Distinguishing "unknown client" from "bad signature" in the response is an oracle |

### 3.4 Token lifetime, refresh, revocation

| Token | TTL | Refresh | Revocation |
|---|---|---|---|
| Machine access token | 15 min | Re-issue via client-credentials | `jti` denylist (Redis, TTL = remaining life) + client disabled flag pushed as a priority cache invalidation; effective ≤ 30 s |
| Human access token | 15 min | Refresh token, 8 h absolute, rotating with reuse-detection (a reused refresh token revokes the whole family) | IdP session revocation + local denylist |
| Human refresh token | 8 h absolute | Rotated on every use | Family revocation on reuse detection |
| Workload SVID | 24 h | Rotated at 50 % TTL via SDS | CRL-free: short TTL is the revocation mechanism; emergency revocation is CA-level plus mesh policy deny |
| Webhook signature | Per-request | n/a | Secret rotation (§5.3) |

**Why not longer machine tokens.** A 15-minute token means a credential stolen from a log, a heap dump or a proxy is worthless within one quarter-hour. The cost is one extra token request per 15 minutes per client, which is negligible next to payment volume. Revocation exists as well, because "it expires soon" is not an answer during an active incident.

### 3.5 Service-to-service: mTLS peer identity

The peer's SPIFFE ID from the verified client certificate is the authoritative caller identity. It is extracted from the TLS connection state (never from a header — `X-Forwarded-Client-Cert` is only trusted when it arrives on a connection whose peer is the sidecar itself), placed in `context.Context`, and used for the mesh `AuthorizationPolicy` and for application-level caller checks. Tenant identity is *not* carried by the mTLS identity: it travels in the propagated request context and, across async boundaries, in the event envelope's `tenantid` (§13.1) — see `multi-tenancy.md` §3.

---

## 4. Authorization

Two layers, evaluated in one deterministic algorithm: **RBAC** decides whether the role may perform the action at all; **ABAC** decides whether this specific subject, on this specific resource, under these conditions, may proceed.

### 4.1 Roles

| Role | Assigned to | Scope |
|---|---|---|
| `platform-admin` | Platform staff (human, OIDC, WebAuthn) | All tenants. Break-glass-adjacent; every action dual-controlled or audited-and-alerted |
| `tenant-admin` | A tenant's own administrator (human) | One tenant, all its merchants |
| `merchant-admin` | A merchant's administrator (human) | One tenant, one merchant |
| `operator` | Platform SRE/support (human) | All tenants, **read-heavy**; the mutations available are operational (suspend, retry, replay), never financial |
| `auditor` | Internal audit, external QSA, regulator (human) | Read-only across the scope granted, including audit records; no ability to mutate or to read secrets |
| `svc:payment-client` | Machine client (tenant integration, data plane) | One tenant, optionally one merchant |
| `svc:onboarding-client` | Machine client (tenant integration, control plane) | One tenant |
| `svc:internal` | Our own workloads (mTLS/SPIFFE) | Bounded by the service's own SPIFFE ID; not tenant-scoped, and therefore *never* used to satisfy a tenant-scoped read without an explicit propagated tenant context |

### 4.2 Role × permission matrix

Permissions are the auth scopes of baseline §19. `✓` = granted, `∅` = denied, `D` = granted but requires dual control (§4.4), `S` = granted only within the principal's own merchant scope.

| Permission | `platform-admin` | `tenant-admin` | `merchant-admin` | `operator` | `auditor` | `svc:payment-client` | `svc:onboarding-client` | `svc:internal` |
|---|---|---|---|---|---|---|---|---|
| `merchants:read` | ✓ | ✓ | S | ✓ | ✓ | S | ✓ | ✓ |
| `merchants:write` | ✓ | ✓ | S | ∅ | ∅ | ∅ | ✓ | ∅ |
| `merchants:suspend` | ✓ | ✓ | ∅ | ✓ | ∅ | ∅ | ∅ | ✓ (risk automation) |
| `merchants:terminate` | D | D | ∅ | ∅ | ∅ | ∅ | ∅ | ∅ |
| `onboarding:read` | ✓ | ✓ | S | ✓ | ✓ | ∅ | ✓ | ✓ |
| `onboarding:write` | ✓ | ✓ | S | ∅ | ∅ | ∅ | ✓ | ✓ |
| `onboarding:approve` | D | D | ∅ | ∅ | ∅ | ∅ | ∅ | ∅ |
| `config:read` | ✓ | ✓ | S | ✓ | ✓ | S (cached, own merchant) | ✓ | ✓ |
| `config:write` | D | ✓ | S | ∅ | ∅ | ∅ | ✓ | ∅ |
| `config:rollback` | D | D | ∅ | D | ∅ | ∅ | ∅ | ∅ |
| `gateways:read` | ✓ | ✓ | S | ✓ | ✓ | ✓ | ✓ | ✓ |
| `gateways:write` | ✓ | ∅ | ∅ | ∅ | ∅ | ∅ | ∅ | ∅ |
| `credentials:rotate` | D | D | ∅ | ∅ | ∅ | ∅ | ∅ | ✓ (scheduled rotation workflow only) |
| `credentials:read` | ∅ | ∅ | ∅ | ∅ | ∅ | ∅ | ∅ | ✓ (own `(tenant,merchant,gateway)` path only) |
| `payments:read` | ✓ | ✓ | S | ✓ | ✓ | S | ∅ | ✓ |
| `payments:write` | ∅ | ∅ | ∅ | ∅ | ∅ | ✓ | ∅ | ✓ (orchestrator) |
| `payments:capture` | ∅ | ∅ | ∅ | ∅ | ∅ | ✓ | ∅ | ✓ |
| `payments:refund` | ∅ | ∅ | S + D | D | ∅ | ✓ | ∅ | ✓ |
| `payments:void` | ∅ | ∅ | S | D | ∅ | ✓ | ∅ | ✓ |
| `payments:replay_dlq` | ✓ | ∅ | ∅ | D | ∅ | ∅ | ∅ | ∅ |
| `ledger:read` | ✓ | ✓ | S | ✓ | ✓ | ∅ | ∅ | ✓ |
| `ledger:write` | ∅ | ∅ | ∅ | ∅ | ∅ | ∅ | ∅ | ✓ (event-consumer only, append-only) |
| `audit:read` | ✓ | ✓ (own tenant) | ∅ | ✓ | ✓ | ∅ | ∅ | ∅ |
| `audit:export` | D | D | ∅ | ∅ | ✓ | ∅ | ∅ | ∅ |
| `tenants:read` | ✓ | ✓ (self) | ∅ | ✓ | ✓ | ∅ | ∅ | ✓ |
| `tenants:write` | D | ∅ | ∅ | ∅ | ∅ | ∅ | ∅ | ∅ |
| `secrets:*` | ∅ | ∅ | ∅ | ∅ | ∅ | ∅ | ∅ | ∅ |

Three lines in that table carry most of the design intent:

- **No human role holds `payments:write`.** Humans do not create payments. A support engineer who could create a payment could move money; the role does not exist, so the attack does not exist. Refunds are the one money-moving operation a human can reach, and only under dual control.
- **`secrets:*` is denied to every principal.** There is no API, no console path and no role that reads a gateway credential. Services read their own path via IAM (`credentials:read` for `svc:internal`); humans never do. Rotation is an automated workflow a human can *trigger* (`credentials:rotate`, dual-controlled) but whose material they cannot see.
- **`platform-admin` cannot write payments or read secrets.** The most privileged role is deliberately not omnipotent, so a compromised admin session is a serious incident rather than a total one.

### 4.3 ABAC conditions

RBAC grants are then filtered by attribute predicates. Every one of these is a hard condition — failing any denies.

| Condition | Predicate | Applies to | Reasoning |
|---|---|---|---|
| **Tenant match** | `subject.tenant_id == resource.tenant_id` | every tenant-scoped resource | Baseline §16.2. Tenant comes from the token; a body/query `tenant_id` that disagrees is `403 TENANT_MISMATCH` + security event, not a silent override |
| **Merchant scope** | `subject.merchant_scope == ∅ ∨ resource.merchant_id ∈ subject.merchant_scope` | merchant, config, payment, ledger | Lets a tenant issue a client credential restricted to one merchant — the common marketplace pattern |
| **Environment** | `subject.env == resource.env == deployment.env` | everything | A sandbox credential must never act on production data, and vice versa |
| **Amount threshold** | `operation ∈ {refund} ∧ amount > config.dualControlThreshold` → require dual control | refunds, and any manual money-out | The blast radius of a refund is unbounded without it; a threshold makes the common case fast and the dangerous case reviewed |
| **Residency** | `resource.residency_region ∈ subject.permitted_regions ∧ deployment.region == resource.residency_region` | merchant PII, KYC artifacts, payments carrying personal data | GDPR §17.3. A read of EU-resident data from a US region is denied at the policy layer *and* the data is not present there |
| **Merchant state** | `resource.merchant.state == ACTIVE` for `payments:write/capture`; `state ∈ {ACTIVE, SUSPENDED}` for `payments:refund/void` | payment operations | Baseline §8: suspension rejects new payments but must always permit giving money back |
| **Time window** | `now ∈ subject.allowed_hours` (optional per tenant), break-glass sessions ≤ 60 min | human roles | Limits the window in which a stolen human session is useful |
| **Device posture** | `subject.device.managed == true ∧ subject.device.compliant == true` | `platform-admin`, `credentials:rotate` | An admin action from an unmanaged laptop is the shape of a real compromise |
| **Source constraint** | `subject.cnf.x5t#S256 == tls.peer_thumbprint` where the scope requires binding | `payments:refund`, `credentials:rotate`, control-plane writes | Sender-constrained tokens (§3.1) |
| **Freshness** | `now - subject.auth_time ≤ 5 min` | dual-control approvals, `credentials:rotate` | Forces re-authentication for the highest-consequence actions |

### 4.4 Evaluation algorithm

Deterministic, total, side-effect free, and **default-deny at every exit**.

```go
// internal/policies/authz/evaluate.go
func Evaluate(ctx context.Context, req Request) Decision {
    // 0. Tenant isolation is NOT part of policy evaluation. It has already run as
    //    pipeline stage 4 and has pinned ctx to exactly one tenant. If it somehow
    //    did not, we deny — we never infer a tenant here.
    tenant, ok := tenantctx.From(ctx)
    if !ok {
        return Deny("NO_TENANT_CONTEXT")            // fail closed
    }

    // 1. Principal must be authenticated and not revoked (re-checked; cheap, and the
    //    cache may have been invalidated since stage 3).
    if req.Principal == nil || req.Principal.Revoked {
        return Deny("UNAUTHENTICATED")
    }

    // 2. Explicit denies win over everything. Deny rules exist for incident response
    //    ("freeze this client now") and are evaluated first so nothing can override them.
    if d := explicitDenies.Match(req); d.Matched {
        return Deny(d.Reason)
    }

    // 3. RBAC: union of permissions across the principal's role bindings, scoped to
    //    this tenant. A binding for another tenant contributes nothing.
    perms := roleBindings.PermissionsFor(req.Principal, tenant)
    if !perms.Has(req.Permission) {
        return Deny("PERMISSION_NOT_GRANTED")       // default deny: absent = denied
    }

    // 4. ABAC: every condition attached to the (role, permission) pair must hold.
    //    Conditions are conjunctive. There is no "any-of" escape hatch.
    for _, cond := range conditionsFor(req.Principal.Roles, req.Permission) {
        if !cond.Holds(ctx, req) {
            return Deny("CONDITION_FAILED:" + cond.ID())
        }
    }

    // 5. Dual control, if the matrix or an ABAC threshold demands it.
    if requiresDualControl(req, perms) {
        appr, err := approvals.Lookup(ctx, req.ApprovalRef)
        switch {
        case err != nil || appr == nil:
            return Deny("DUAL_CONTROL_REQUIRED")
        case appr.ApproverID == req.Principal.ID:
            return Deny("DUAL_CONTROL_SELF_APPROVAL")   // the whole point
        case appr.Expired(clock.Now()) || appr.RequestFingerprint != req.Fingerprint:
            return Deny("DUAL_CONTROL_STALE_OR_MISMATCHED")
        }
    }

    // 6. Allow. The decision, its inputs and the matched rule IDs are emitted as an
    //    audit record for every mutating permission and every denial.
    return Allow(perms.MatchedRuleIDs())
}
```

Properties that make this reviewable:

| Property | How it holds |
|---|---|
| Default deny | Every path that is not an explicit `Allow` returns `Deny`. There is no fallthrough and no "if we cannot decide, permit" branch |
| Deny precedence | Explicit denies are evaluated at step 2, before any grant is computed |
| Conjunctive conditions | ABAC conditions are AND-ed; there is no disjunctive escape |
| No self-approval | Checked explicitly, and again at the storage layer by a `CHECK (approver_id <> requester_id)` constraint |
| Totality | No panics, no network calls, no clock reads outside the injected `Clock`. Property-tested with `tests/integration/authz_property_test.go` over generated principals × permissions × resources, asserting that no generated input yields `Allow` across a tenant boundary |
| Auditability | Every decision carries the matched rule IDs; denials are logged with the failing condition ID so an operator can answer "why was this denied" without guessing |
| Fail-closed under dependency failure | If the role-binding cache is stale beyond its budget or the approvals store is unreachable, the result is `Deny`, not `Allow`. Authorization is the one place the platform is deliberately *not* fail-static |

---

## 5. Secrets management

### 5.1 Storage and access

| Aspect | Design |
|---|---|
| Store | AWS Secrets Manager. KMS CMK per environment; per-tenant CMK for siloed-tier tenants (baseline §16.1) |
| Envelope | Secrets Manager performs KMS envelope encryption; application-level credential *material* is additionally envelope-encrypted with a per-tenant DEK (§2.5) before storage, so a Secrets Manager compromise alone yields ciphertext |
| Path scheme | `/{env}/{tenant_id}/{merchant_id}/{gateway}/{purpose}` — e.g. `/prod/ten_01J.../mrc_01J.../stripe/api_key` |
| Reference scheme | The database stores **only** a reference: `secretref://prod/ten_01J.../mrc_01J.../stripe/api_key#v3`. `gateway_credentials_meta` holds the reference, the version, the created/rotates-at timestamps and a SHA-256 fingerprint of the material (for rotation verification without ever reading the material back into a comparison log). No table anywhere holds the material |
| Retrieval | At use time, over the Secrets Manager VPC endpoint, with IRSA credentials. Cached in memory ≤ 5 min, `Secret[T]`-wrapped, zeroed on eviction |
| IAM scoping | Each deployable's role is scoped by resource path prefix and by KMS grant. Example: `payment-orchestrator` may `GetSecretValue` on `arn:aws:secretsmanager:*:*:secret:/prod/*/*/*/api_key-*` only, and only with `kms:ViaService = secretsmanager.<region>.amazonaws.com` |
| Tenant scoping in IAM | Siloed tenants get a per-tenant condition (`secretsmanager:ResourceTag/tenant = ten_…` matched against the pod's IRSA session tag) so IAM itself enforces the tenant boundary for the highest-assurance tier |
| Endpoint policy | The Secrets Manager VPC endpoint policy independently restricts to our account, our roles and the `/prod/*` path — a second, non-IAM check |
| Denied to humans | No human IAM principal has `secretsmanager:GetSecretValue` on `/prod/**`. An SCP denies it at the organization level, so it cannot be re-granted by an account admin |
| Audit | Every `GetSecretValue` is a CloudTrail event, streamed to the SIEM. A read by an unexpected principal, or a read rate above the per-service baseline, alerts |

### 5.2 Prohibitions (enforced, not advised)

| Prohibition | Enforcement |
|---|---|
| No secret in an environment variable in plaintext | Admission policy rejects any pod spec whose env var name matches `(?i)(secret\|password\|token\|api_?key\|credential\|private_?key)` unless the value is a `secretKeyRef` to a bootstrap-only secret; CI runs the same check against `deployments/k8s/**` and `helm/**`. Env vars carry *references* (`PP_STRIPE_CREDENTIAL_REF=secretref://…`), never material |
| No secret in a ConfigMap | Admission policy + CI scan of all `ConfigMap` data for high-entropy strings and known credential prefixes (`sk_live_`, `AKIA`, `-----BEGIN`) |
| No secret in an image | `docker history` + `trufflehog`/`gitleaks` scan of every built layer in CI; the build fails on a hit. Images are `FROM scratch`/distroless with no shell, so there is nowhere to stash one |
| No secret in the repository | `gitleaks` pre-commit hook + a CI scan of the full history on every PR; any hit blocks the merge and triggers rotation of the leaked value regardless of whether it was real |
| No secret in a log | `Secret[T]` (§6) + allowlist serializer + a log-pipeline detector that quarantines and alerts on any record matching the credential patterns |
| No secret in a core dump | `RLIMIT_CORE=0`, `/proc/sys/kernel/core_pattern` set to a discard handler on nodes, and DEKs zeroed on eviction |
| No secret in a trace or metric | Span attributes go through the same allowlist serializer; the metric registry lint rejects any label whose name matches the credential pattern |
| No secret in an error message | `pkg/apierror` constructors take structured fields only; the RFC 9457 `detail` field is built from a template, never from `%v` of an arbitrary value |

### 5.3 Rotation

| Secret | Max age | Mechanism | Overlap | Failure behaviour |
|---|---|---|---|---|
| KMS CMK | 365 d | Automatic KMS key rotation (backing material rotates, key ID stable) | n/a — KMS decrypts with the historical material transparently | None; ciphertext remains readable |
| Gateway API credentials | **90 d** | Automated workflow: mint new credential at the gateway → store as a new secret version (`AWSPENDING`) → dual-run: new version used for new attempts while the old remains valid → verify N successful calls → promote to `AWSCURRENT` → revoke old at the gateway after a 24 h soak | ≥ 24 h dual-run | Rotation aborts and rolls back to `AWSCURRENT`; alerts. Credential age is a gauge; > 80 d warns, > 90 d pages |
| Platform JWT signing key | **30 d** | Two-key JWKS window: the new key is published to JWKS and *not* used for signing for 10 min (propagation), then becomes the signer; the previous key stays published for 30 d for verification only | 30 d verify-only | If publication fails, signing does not switch. Verifiers never see a `kid` they cannot resolve |
| OAuth2 client secrets | 90 d | Two-secret overlap; tenant-initiated via API, with a reminder at 60 d and enforcement (auth failure) at 120 d | Both valid until the old is explicitly revoked | Tenant self-service; expiry is announced, never silent |
| Webhook signing secrets (inbound, per gateway) | Gateway-dictated | Accept both current and previous secret during the gateway's own rotation window | Per gateway | Signature verification tries both; a verify against the previous secret increments a metric so a stuck rotation is visible |
| Webhook signing secrets (outbound, to merchants) | 180 d | Two-secret publication; both signatures sent (`X-PP-Signature: v1=…,v1=…`) during overlap | 30 d | Merchant migrates at their own pace within the window |
| mTLS workload certs | 24 h | Mesh SDS, rotate at 50 % TTL | 12 h | Pod continues on the existing cert until expiry; failure to rotate alerts at 75 % TTL |
| Database credentials | 30 d | IAM database authentication — there is no long-lived DB password to rotate; the 15-min IAM token is minted per connection | n/a | Falls back to a Secrets-Manager-stored password only in break-glass, which alerts |
| Break-glass credentials | Per use | Minted on approval, valid 60 min, non-renewable, revoked on session end | n/a | Every issuance pages the security channel |

**Why 90 days for gateway credentials rather than 30.** Rotation at a gateway is an external, rate-limited, occasionally-manual operation with a real failure rate; at 30 days the rotation workflow itself becomes a top source of incidents. 90 days with a mandatory dual-run and automated verification is the point where the risk reduction and the operational risk cross. JWT signing keys, by contrast, rotate entirely within our control with a clean two-key window, so 30 days costs nothing.

---

## 6. Logging safety

Baseline §17.2 makes this a PCI-scope control, not a hygiene preference.

### 6.1 `Secret[T]`

```go
// pkg/secret/secret.go — stdlib only, per §4 of the baseline
type Secret[T any] struct{ v T }

func New[T any](v T) Secret[T] { return Secret[T]{v: v} }

// Expose is the ONLY accessor. It is named to be greppable and reviewable:
// every call site is a place where plaintext exists, and CI counts them.
func (s Secret[T]) Expose() T { return s.v }

func (Secret[T]) String() string                { return "[REDACTED]" }
func (Secret[T]) GoString() string              { return "[REDACTED]" }
func (Secret[T]) MarshalJSON() ([]byte, error)  { return []byte(`"[REDACTED]"`), nil }
func (Secret[T]) MarshalText() ([]byte, error)  { return []byte("[REDACTED]"), nil }
func (Secret[T]) LogValue() slog.Value          { return slog.StringValue("[REDACTED]") }
func (s Secret[T]) Format(f fmt.State, verb rune) { io.WriteString(f, "[REDACTED]") }
```

`Format` is the load-bearing one: it covers `%v`, `%s`, `%d`, `%+v`, `%#v` and every other verb, so even a developer deliberately trying to print the value gets `[REDACTED]`. `MarshalJSON` covers accidental inclusion in a response or an event payload. `LogValue` covers `log/slog`. Every credential, token, signing key, DEK and bank account number in the codebase is declared as `Secret[string]` or `Secret[[]byte]`; `internal/adapters/gateway/spi` will not compile with a bare `string` credential field.

### 6.2 Allowlist serializer

There is no reflective struct logging. `internal/infrastructure/telemetry/logx` exposes typed field constructors, and only registered field names are serialized:

```go
logx.Info(ctx, "payment.dispatched",
    logx.PaymentID(p.ID), logx.GatewayID(g.ID), logx.Amount(p.Amount),
    logx.AttemptNumber(a.N), logx.Outcome(res.Outcome))
```

| Property | Detail |
|---|---|
| Registry | `logx.RegisterField(name, kind)` at package init; the registry is enumerated in `docs/observability.md` and CI asserts it matches the baseline §22.1 mandatory context |
| Unregistered field | Dropped, and a `pp_log_field_rejected_total{field}` counter increments, so the drop is visible rather than silent |
| Free-form message | The message is a **static string constant** — an event name, not an interpolated sentence. Interpolation is where PANs end up |
| Value types | Values are typed (`ID`, `Money`, `Enum`, `Duration`, `Bool`, `Count`). There is no `logx.Any` |
| Amount logging | `Money` logs as `{"amount":1050,"currency":"USD"}` — minor units, never a formatted string (baseline §7) |
| Context injection | `trace_id`, `span_id`, `correlation_id`, `tenant_id`, `merchant_id`, `service`, `version`, `environment`, `region` are injected from `ctx`, not passed by callers, so they cannot be forgotten or forged |

### 6.3 PAN detector

Runs in three places: L1 validation (baseline §17.2), the WAF (W2), and the log pipeline as a last resort.

| Aspect | Detail |
|---|---|
| Algorithm | Strip `-`, ` `, `.` from every string field; scan for runs of 13–19 digits; Luhn-check each run; check the leading digits against known IIN ranges to cut false positives |
| Scope | Every string field of every request body, every header value except `Authorization`, and every query parameter. Recursive through nested objects and arrays, depth-capped at 32 |
| False positives | Order references and invoice numbers can be Luhn-valid by chance (≈ 10 %). The IIN check plus a per-field allowlist (`merchant_reference` is length-capped at 12, below the 13-digit floor) brings the practical rate near zero. A false positive is a `400`, which is recoverable; a false negative is a PCI scope breach, which is not — the detector is deliberately biased toward blocking |
| On hit | `400 SENSITIVE_DATA_IN_REQUEST` (baseline §20.2). The offending value is **never** logged — not truncated, not masked, not hashed. The log records field *path* and *length* only. A `pp_security_events_total{type="pan_in_request"}` counter increments and a security event is emitted |
| Log-pipeline backstop | The Firehose transform re-runs the detector over every log record; a hit quarantines the record to a restricted-access bucket, drops it from the searchable index, and pages security — because a hit here means two upstream controls failed |
| Also detected | CVV shape (a 3–4 digit field named `cvv`/`cvc`/`csc`/`security_code`), track-1/track-2 patterns (`%B…^…^…?`, `;…=…?`), and PEM private-key headers |

### 6.4 Lint rules

Enforced by a custom `go/analysis` pass in `scripts/lint/` and wired into CI (baseline §27 step 12).

| Rule | Detects | Reasoning |
|---|---|---|
| `no-verbose-format-on-requests` | `%+v` / `%#v` / `%v` applied to any type in `api/**`, any type named `*Request`/`*Payload`/`*Body`, or any type transitively containing a `Secret[T]` | The single most common PAN/credential leak in real systems |
| `no-reflective-log` | `slog.Any`, `zap.Any`, `logx.Any`, `json.Marshal` of a request type into a log call | Reflection defeats the allowlist |
| `no-print` | `fmt.Print*`, `println`, `log.Print*` outside `cmd/**` and `_test.go` | Anything that reaches stdout bypasses the pipeline entirely |
| `secret-not-string` | A struct field whose name matches the credential pattern and whose type is `string`/`[]byte` | Forces `Secret[T]` at declaration |
| `no-secret-in-error` | `fmt.Errorf` whose format arguments include a `Secret[T]` or a credential-named identifier | Errors get logged and returned |
| `no-sprintf-sql` | `fmt.Sprintf` whose result flows into `Query`/`Exec`/`QueryRow` | SQL injection |
| `no-tenant-from-body` | Any read of a `TenantID` field from a request struct outside the explicit mismatch-check function | Baseline §16.2 |
| `repository-requires-context` | A repository method without `ctx context.Context` as its first parameter | No context, no tenant, no RLS GUC |
| `no-http-in-domain` | Imports outside the §4 allowlist | Layering, enforced by `scripts/check-architecture.sh` |

### 6.5 Never-log list

Never, in any form — not masked, not truncated, not hashed, not "just the last four":

PAN · CVV/CVC/CSC · track data · PIN/PIN block · full bank account number (last 4 only, and only in the merchant-facing UI, never in a log) · IBAN in full · gateway API keys, secrets and webhook signing secrets · JWT signing private keys · DEKs and unwrapped key material · raw `Authorization` header values · OAuth client secrets · refresh tokens · full request bodies of `POST /v1/payments` · KYC document contents or their pre-signed URLs · national identifier numbers · passwords or password hashes · session cookies · MFA seeds or recovery codes · the plaintext of anything a `Secret[T]` wraps.

Permitted in logs: entity IDs (opaque per §6 of the baseline), `Money` in minor units, enum values, rule IDs, error codes, durations, counts, gateway *names*, last-4 of a card **only** when supplied by the gateway as a display field and only in the payment record, never in a log line.

---

## 7. Threat model — STRIDE per trust boundary

### 7.1 Boundaries

| # | Boundary | Crossing |
|---|---|---|
| TB-1 | Internet → edge | Merchant/tenant clients, attackers |
| TB-2 | Edge → data-plane services | Authenticated requests |
| TB-3 | Service → service (intra-cluster) | mTLS gRPC/HTTP |
| TB-4 | Service → datastore | Postgres, Redis, Kafka, S3 |
| TB-5 | Service → external gateway/vendor | Outbound HTTPS via egress proxy |
| TB-6 | Gateway → webhook ingress | Inbound signed callbacks |
| TB-7 | Platform → merchant webhook endpoint | Outbound to merchant-supplied URL |
| TB-8 | Human operator → control plane | OIDC-authenticated admin actions |
| TB-9 | CI/CD → cluster/registry | Build and deploy pipeline |
| TB-10 | Tenant A ↔ Tenant B | Logical, inside shared infrastructure |

### 7.2 STRIDE

| Boundary | Spoofing | Tampering | Repudiation | Information disclosure | Denial of service | Elevation of privilege |
|---|---|---|---|---|---|---|
| **TB-1** | Forged token → JWT validation §3.3 (alg allowlist, iss/aud, jti replay) | Body modification in transit → TLS 1.3, HSTS preload, no cleartext port | Client denies sending → request ID + audit record + signed access log | TLS 1.3 only, no cleartext, `no-store` | Shield + WAF W6/W7 + adaptive concurrency | Scope escalation → default-deny RBAC, scopes minted per client, not per request |
| **TB-2** | Header spoofing (`X-Tenant-Id`, `X-Forwarded-For`) → edge strips all `X-PP-*` and identity headers; tenant only from the token | Request replay → idempotency (§14) + jti replay check | — | Error responses are templated; no stack traces, no internal IDs | Per-tenant token bucket + bulkhead | Confused deputy → tenant guard runs before authz |
| **TB-3** | Impersonating a service → mTLS SPIFFE identity; no header-based identity | In-cluster MITM → STRICT mTLS, no plaintext port exists | — | Sidecar-only plaintext on loopback; NetworkPolicy blocks lateral reach | Per-caller concurrency limits; circuit breakers | Namespace escape → PodSecurity restricted, seccomp, dropped caps, no privileged pods |
| **TB-4** | Stolen DB credential → IAM auth tokens (15 min), no static password | Direct row edit → RLS + append-only ledger + hash-chained audit + `CHECK` invariants (I1–I3) | Silent data change → audit chain + Postgres audit extension on DDL and on `audit_records` | Dump of the DB → field-level envelope encryption with AAD binding; RLS | Connection exhaustion → PgBouncer + per-service pool caps | `BYPASSRLS`/superuser → app role explicitly lacks both; migration role is separate and used only by `platformctl` |
| **TB-5** | Fake gateway endpoint → TLS `verify-full` + CA pinning per gateway + egress domain allowlist | Response tampering → L6 response validation (signature, schema, amount/currency echo) | Gateway denies receiving → deterministic gateway idempotency key (§14.4) makes lookup possible | Credential in a request log → `Secret[T]`, egress proxy does not log bodies | Slow gateway → 8 s hard timeout, bulkhead, breaker | SSRF to internal → no general egress; proxy allowlist by service account |
| **TB-6** | Forged webhook → signature verification against the gateway's scheme, over the **raw** body | Payload tampering → same signature; body is persisted verbatim before parsing | — | Webhook body may contain last-4 only; stored encrypted, access-controlled | Webhook flood → accept-and-persist ≤ 50 ms, async processing, per-gateway rate limit | Webhook that mutates state directly → webhooks never write payment state synchronously; they enqueue, and the FSM (L7) still governs |
| **TB-7** | Merchant endpoint impersonated by DNS poisoning → resolve-then-pin, TLS `verify-full` | Response ignored except for a 2xx ack | Merchant denies delivery → delivery log with response status and timing, retained | Payload contains only IDs and non-sensitive fields; never PAN, never credentials | Merchant endpoint slow → 5 s timeout, per-merchant delivery bulkhead, exponential backoff | SSRF → SSRF-guard proxy (T-10 below) |
| **TB-8** | Session hijack → short sessions, WebAuthn, device posture, sender-constrained tokens | Config tampering → versioned config, `If-Match`, full diff audited | Admin denies acting → audit record with actor, IP, device, approval ref; hash-chained | Console shows no secrets, ever | — | Privilege escalation via role self-grant → `tenants:write`/`gateways:write` dual-controlled; role bindings are themselves audited resources |
| **TB-9** | Forged image → cosign signature + admission verification | Build tampering → SLSA provenance, hermetic build, pinned toolchain | Deploy denies → signed provenance attestation with the builder identity | Secrets in CI → OIDC federation to AWS, no long-lived CI keys; masked outputs | — | CI compromise → deploy role is scoped, requires a signed provenance predicate; production deploy needs a human approval that CI cannot supply |
| **TB-10** | Tenant A presents tenant B's ID → tenant comes only from the token | A writes B's row → RLS + application guard | — | A reads B's data → RLS, cache key prefix, S3 prefix IAM condition, log view filter | A starves B → per-tenant rate limit, bulkhead, Kafka quota, cache quota | A gains B's role → role bindings are tenant-scoped; a binding in another tenant contributes zero permissions |

### 7.3 Attacker scenarios and the control that stops each

| # | Scenario | How it would work | Primary control | Backstop | Detection signal |
|---|---|---|---|---|---|
| **T-1** | **Card testing** | Attacker with a stolen merchant API key submits thousands of low-value payments against a token list to find live cards | Per-merchant velocity limits from config (`maxPaymentsPerMinute`, `maxPerCardPerHour`, §23) + risk engine (§12 stage 11) + hard declines are **never** failed over to another gateway (§9.1) | WAF W8 (distinct tokens × low auth rate), auto-suspension of the merchant on breach | `pp_payments_total{outcome="declined"}` rate vs merchant baseline; distinct-token cardinality; auth rate < 20 % over 10 min → page |
| **T-2** | **Payment ID enumeration** | Attacker iterates `GET /v1/payments/{id}` to harvest other merchants' payment data | IDs are ULIDs with 80 bits of entropy and are opaque (§6) — not guessable | Tenant + merchant ABAC on every read: a valid ID from another tenant returns `404`, not `403` (no existence oracle). RLS returns zero rows regardless | `pp_http_requests_total{route="/v1/payments/{id}",status="404"}` rate per client above baseline → alert |
| **T-3** | **Webhook forgery** | Attacker POSTs a fabricated `payment.captured` to `/v1/webhooks/stripe` to mark an unpaid order as paid | Signature verification over the **raw** body against the gateway's scheme, before any parsing (baseline §19.2: gateway auth) | Webhooks never transition state synchronously; the event is queued and the L7 FSM validates the transition against the payment's actual attempt records. A capture with no matching dispatched attempt is rejected and alerts | `WEBHOOK_SIGNATURE_INVALID` counter; any occurrence at volume → page |
| **T-4** | **Webhook replay** | Attacker captures a legitimate signed webhook and re-sends it, or re-sends an old one | Timestamp in the signed payload must be within ±5 min (§24 clock skew) **and** the `(gateway, gateway_ref)` pair must be unseen in `webhook_dedup` | Consumer-side effectively-once (§13.5) and the FSM: a duplicate `captured` on an already-captured payment is a no-op, not a second capture | `WEBHOOK_REPLAY_DETECTED` counter; nonce reuse → security event |
| **T-5** | **Idempotency-key collision abuse** | Attacker guesses or reuses another party's idempotency key to read back a stored response, or to suppress a legitimate payment | The idempotency scope is `(tenant_id, merchant_id, method, path_template, key)` (§14.1) — a key from another tenant addresses a different record entirely and is invisible | Request fingerprint check: the same key with a different canonicalized body returns `422 IDEMPOTENCY_KEY_REUSED` (§14.2), so a collision cannot silently replay a *different* payment's response | `pp_idempotency_outcomes_total{outcome="conflict"}` spike per client |
| **T-6** | **Cross-tenant access via a forged body field** | Attacker sends `{"tenant_id":"ten_victim", …}` with a valid token for their own tenant | Tenant is derived exclusively from the token; a body `tenant_id` is ignored, or, if it disagrees, produces `403 TENANT_MISMATCH` + audit + alert (§16.2) | RLS at the database: even if the guard were bypassed, the query runs under `app.tenant_id` from the token and returns zero rows | `TENANT_MISMATCH` counter — any non-zero value is a security event and pages; there is no legitimate cause |
| **T-7** | **Credential exfiltration via logs** | Attacker (or a curious insider) reads a gateway API key from an application log, a trace or an error message | `Secret[T]` renders `[REDACTED]` under every format verb and every marshaller (§6.1); the allowlist serializer will not emit unregistered fields | Log-pipeline detector quarantines any record matching credential patterns; log access is itself audited; secrets are 90-day-rotated so a historical leak has bounded value | `pp_log_field_rejected_total`; quarantine-bucket writes → page |
| **T-8** | **Dependency compromise** | A transitive Go module or a base image is backdoored and exfiltrates credentials or payment data | No general egress: a compromised dependency cannot reach an attacker-controlled host, only the allowlisted gateway domains through a proxy that logs every connection (§2.2) | Pinned versions + `go.sum` + `go mod verify` + SBOM diff on every build + `govulncheck` gate + distroless base with no shell (§9) | Egress-proxy denied-destination counter; SBOM diff showing an unreviewed new module; Falco outbound-connection rule |
| **T-9** | **Insider config change** | An operator quietly repoints a merchant's routing to a gateway they control, or raises a risk limit | `config:write` is not granted to `operator` at all (§4.2); `config:rollback` is dual-controlled; every write is versioned with a full prior document retained (§23) | Hash-chained audit record with actor, device, approval reference and the complete before/after diff; `configuration.published.v1` is consumed by the SIEM | Any config write outside change windows, or by an unexpected actor, alerts. Routing changes to a gateway not in the tenant's contracted set alert unconditionally |
| **T-10** | **SSRF via a merchant-supplied webhook URL** | Merchant configures `webhooks.endpoints[].url` as `http://169.254.169.254/latest/meta-data/iam/…` or `http://10.0.x.x:5432` to make the platform fetch internal resources | Validation at configuration write (L4): scheme must be `https`, port 443 only, host must resolve to a public IP, no userinfo, no fragment; hostnames in `.internal`, `.local`, `.cluster.local` rejected | Delivery goes through an SSRF-guard proxy that re-resolves at connect time, **pins the resolved IP** (defeating DNS rebinding), and refuses RFC1918/loopback/link-local/CGNAT/IPv6-ULA/IPv4-mapped-IPv6 targets; redirects are not followed; the sender egresses on a NAT with no route to the VPC's private ranges and no IMDS access | Proxy denied-target counter with the merchant ID; any hit is a security event and the endpoint is quarantined pending review |
| **T-11** | **Token theft from a merchant's infrastructure** | Attacker exfiltrates a valid access token and calls the API from elsewhere | 15-minute TTL; mTLS sender-constraining (`cnf`) required for refunds and control-plane writes (§3.1) | `jti` replay detection; anomaly detection on source ASN/geography per client; immediate revocation via the denylist | Simultaneous use of one `jti` or one `sub` from two ASNs within 60 s → page |
| **T-12** | **Refund fraud by a compromised merchant admin** | Attacker with a `merchant-admin` session issues large refunds to a card they control | Dual control above the configured threshold (§4.3); re-authentication required within 5 min; refunds capped by I1 (`sum(refunds) ≤ captured`) | Velocity alerting on refund rate vs merchant baseline; refunds to a payment method differing from the original are refused by the gateway adapter contract | `payment.refunded.v1` volume anomaly per merchant; refund-to-capture ratio > baseline + 3σ |
| **T-13** | **Compromised CI pipeline deploying a malicious image** | Attacker with repo write access pushes a build that adds an exfiltration path | Admission verifies a cosign signature **and** a SLSA provenance predicate naming the expected builder, workflow and source repo (§9) | Production deploy requires a human approval outside the pipeline's control; the deploy IAM role cannot be assumed by the build role | Admission-denied events; provenance mismatch → page |
| **T-14** | **Database read-replica or backup exfiltration** | Attacker obtains a snapshot or replica dump | KMS-encrypted at rest with a CMK whose key policy restricts decrypt to our roles; field-level envelope encryption for sensitive columns with AAD binding to `(tenant, table, column, row)` | Snapshot sharing is denied by SCP; restore targets are constrained by the key policy | CloudTrail on `ModifyDBSnapshotAttribute`, `CreateDBSnapshot` by an unexpected principal → page |

---

## 8. Supply chain

| Control | Implementation | Reasoning |
|---|---|---|
| Dependency pinning | `go.mod` with exact versions, `go.sum` committed, `GOFLAGS=-mod=readonly`, `GONOSUMDB` unset (checksum DB always consulted), `GOPRIVATE` empty | A floating dependency is an unreviewed deploy |
| Integrity verification | `go mod verify` in CI before build; `GONOSUMCHECK` never set; the module cache is populated from a proxy we control that mirrors upstream and refuses to serve a module whose hash changed | Defends against a retroactively modified upstream tag |
| Dependency review | Renovate opens PRs; major bumps and any new *direct* dependency require a named reviewer; a new transitive dependency appears in the SBOM diff on the PR and must be acknowledged | The point at which a supply-chain attack is cheapest to catch is the PR |
| Vendoring | Not used. The module proxy mirror plus `go.sum` gives reproducibility without a 200 MB diff on every bump | |
| SBOM | CycloneDX 1.5, generated per image at build time from the build graph (not by scanning the filesystem), signed, attached to the image as an attestation, and archived for 7 years | A filesystem scan misses what the linker dropped and includes what it did not; the build graph is authoritative. 7 years matches the records-retention floor so we can answer "was CVE-X in the image running on this date" |
| SBOM diff gate | CI fails if the SBOM gains a component not present in the base commit's SBOM without an accompanying `go.mod` change | Catches build-time injection |
| Base image | `gcr.io/distroless/static-debian12:nonroot` for Go binaries. No shell, no package manager, no libc for `CGO_ENABLED=0` builds. `USER 65532`. Rebuilt weekly and on any base advisory | No shell means no `curl \| sh`, no reverse shell, and a dramatically smaller CVE surface |
| Build | `CGO_ENABLED=0`, `-trimpath`, `-ldflags "-s -w -buildid="`, pinned Go toolchain version in `go.mod` (`toolchain go1.2x.y`), hermetic container build with no network access after dependency fetch | Reproducibility: two builds of the same commit produce byte-identical images, which is what makes provenance meaningful |
| Reproducibility check | CI rebuilds each image twice in independent runners and compares digests; a mismatch fails the build | An unreproducible build cannot be attested honestly |
| Image signing | `cosign sign` keyless (Fulcio/Rekor) using the CI workload's OIDC identity; the signature and the SLSA provenance attestation are pushed to the registry alongside the image | Keyless removes a long-lived signing key from the threat model; Rekor gives a public, tamper-evident record |
| Admission verification | Kyverno `verifyImages` requires: a valid cosign signature; a Rekor inclusion proof; a SLSA v1.0 provenance predicate whose `builder.id` is our GitHub Actions workflow, whose `externalParameters.source` is this repository, and whose `invocation` references a protected branch or a tag | Signature alone proves "someone signed it"; provenance proves "our pipeline built it from our source" |
| SLSA level | **Build L3** target: hermetic, provenance generated by a trusted builder the build steps cannot forge, non-falsifiable, with signed materials. Source track: protected branches, required reviews, signed commits, no force-push | L3 is the level at which a compromised build *step* cannot forge provenance — which is the actual threat |
| Registry | Private ECR, immutable tags, image scanning on push, lifecycle policy retaining production digests for 7 years; pull-through cache for upstream images so no production pull depends on Docker Hub | Immutable tags make `image@sha256:` and `image:tag` equivalent claims |
| Deployment reference | Manifests reference images by **digest only**; `:latest` is rejected at admission | A tag is a mutable pointer; a digest is a fact |
| Vulnerability gates | `govulncheck` (Go-aware, symbol-reachability-based) on every PR; Trivy on every image. Gate: **any reachable Critical or High blocks the merge**; unreachable High is a 14-day ticket; Medium is a 90-day ticket. Production images are rescanned daily and a new Critical pages within 24 h | Symbol reachability matters: a CVE in a package whose vulnerable function is never called is not a production risk, and treating it as one trains everyone to ignore the gate |
| Exceptions | A time-boxed, named-owner exception file (`.security/exceptions.yaml`) with an expiry date; CI fails when an exception expires | Undated exceptions become permanent |
| Secret scanning | `gitleaks` pre-commit and full-history CI scan; GitHub push protection enabled | |
| SAST | `gosec` + `staticcheck` + the custom passes in §6.4, all blocking | |
| IaC scanning | `tfsec`/`checkov` on `terraform/`, `kubesec`/`kyverno-cli` on `deployments/` and `helm/`, blocking on High | An open security group is as bad as an injection bug |

---

## 9. Incident response hooks

### 9.1 Detect → alert → auto-mitigate

| Signal | Threshold | Alert | Auto-mitigation | Manual step |
|---|---|---|---|---|
| `TENANT_MISMATCH` | any occurrence | **Page** (Sev-1 candidate) | Client key flagged; request rejected | Investigate the client; assume compromise until disproven |
| PAN detected in a request | ≥ 1 / 5 min per merchant | Page security | Request blocked; merchant integration flagged | Contact merchant; confirm the checkout uses hosted fields |
| PAN detected in a **log** | any | **Page** Sev-1 | Record quarantined, index write blocked | Full PCI incident procedure; scope assessment |
| `WEBHOOK_SIGNATURE_INVALID` | > 10 / min per gateway | Page | Source IP throttled at the WAF | Verify against the gateway's rotation state |
| Auth failure rate per client | > 50 % over 5 min, min 20 samples | Ticket; page at > 95 % | Client throttled to 1 rps | Check for a rotation the tenant did not complete |
| Card-testing signature (T-1) | distinct tokens > 30 / 10 min **and** auth rate < 20 % | **Page** | Merchant auto-suspended (`ACTIVE → SUSPENDED`); refunds/voids still permitted per §8 of the baseline | Confirm with the merchant; review the token source |
| Refund anomaly (T-12) | refund rate > baseline + 3σ over 1 h | Page | Refunds above the dual-control threshold blocked for that merchant | Review the refund set with the merchant |
| Egress proxy denied destination | any | Page security | Connection refused (already) | Identify the caller; treat as possible dependency compromise (T-8) |
| SSRF-guard denied target | any | Page security | Endpoint quarantined; deliveries paused for that merchant | Review the configured URL |
| Secrets Manager read by unexpected principal | any | **Page** | — (detect-only; blocking is IAM's job) | Rotate the secret; investigate the principal |
| Secrets Manager read rate | > 3× the service's 7-day baseline | Ticket | — | Usually a cache bug; verify |
| Admission denial (unsigned/unprovenanced image) | any | Page | Deploy blocked | Verify the pipeline; treat as T-13 until cleared |
| `jti` reuse from two ASNs < 60 s | any | Page | Token denylisted; client forced to re-authenticate | Token theft procedure (T-11) |
| Falco: shell in container | any | **Page** Sev-1 | Pod cordoned and evicted with a memory snapshot preserved | Forensics on the snapshot |
| Break-glass session opened | any | Notify security channel | Session recorded | Post-hoc review within 24 h, mandatory |
| CloudTrail: `PutKeyPolicy`, `ScheduleKeyDeletion`, `ModifyDBSnapshotAttribute`, `DeleteTrail` | any | **Page** | — | Assume account compromise until disproven |
| Audit hash-chain verification failure | any | **Page** Sev-1 | Audit writes continue (buffered); the affected range is frozen from export | Tamper procedure (`compliance.md` §6.4) |
| Certificate expiring | < 14 d | Ticket; page at < 3 d | Auto-renew attempted | |
| Credential age | > 80 d warn, > 90 d page | | Rotation workflow triggered automatically at 75 d | |

Auto-mitigation is deliberately limited to actions that are **reversible and non-financial**: throttle, suspend, quarantine, cordon, denylist. No automation cancels, refunds or fails a payment — baseline §12.3, "no timer may fail a payment", generalizes to "no alert may move money".

### 9.2 Security event schema

Emitted to Kafka `pp.audit.v1` (baseline §13.3) and forked to the SIEM. Distinct from the audit record (`compliance.md` §6), which records *authorized business actions*; a security event records *a security-relevant observation*.

```json
{
  "specversion": "1.0",
  "id": "evt_01JB8Z9K2QW3E4R5T6Y7U8I9O0",
  "type": "security.tenant_mismatch.v1",
  "source": "/payments-platform/payment-api",
  "time": "2026-08-26T14:03:11.412Z",
  "tenantid": "ten_01J...",
  "merchantid": "mrc_01J...",
  "correlationid": "req_01J...",
  "traceparent": "00-4bf92f...-00f067aa0ba902b7-01",
  "data": {
    "severity": "HIGH",
    "category": "ISOLATION",
    "outcome": "BLOCKED",
    "controlId": "SEC-ISO-001",
    "principal": {
      "type": "MACHINE_CLIENT",
      "id": "cli_01J...",
      "tenantId": "ten_01J...",
      "authMethod": "OAUTH2_CLIENT_CREDENTIALS",
      "tokenId": "jti-...",
      "roles": ["svc:payment-client"]
    },
    "source": {
      "ip": "203.0.113.7",
      "asn": 64496,
      "country": "DE",
      "userAgent": "pp-go/1.4.2",
      "tlsVersion": "TLSv1.3",
      "clientCertThumbprint": null
    },
    "resource": { "type": "payment", "id": "pay_01J...", "tenantId": "ten_01J..." },
    "action": "POST /v1/payments",
    "detection": { "rule": "L1.TENANT_MISMATCH", "detector": "isolation-guard", "confidence": "CERTAIN" },
    "evidence": { "claimedTenantId": "ten_01J...", "tokenTenantId": "ten_01J..." },
    "mitigation": { "automatic": ["REQUEST_BLOCKED", "CLIENT_FLAGGED"], "manual": "INVESTIGATE" }
  }
}
```

| Field | Rule |
|---|---|
| `severity` | `CRITICAL` (page, Sev-1) · `HIGH` (page) · `MEDIUM` (ticket) · `LOW` (metric only) |
| `category` | `ISOLATION` · `AUTHN` · `AUTHZ` · `DATA_EXPOSURE` · `INTEGRITY` · `AVAILABILITY` · `SUPPLY_CHAIN` · `FRAUD` |
| `outcome` | `BLOCKED` · `DETECTED` · `ALLOWED_WITH_ALERT` — never absent, so an event's meaning is unambiguous |
| `controlId` | Stable identifier of the control that fired, cross-referenced in `compliance.md` §2 so an auditor can trace event → control → PCI requirement |
| `evidence` | Structured, allowlisted fields only. **Never** the offending value for PAN/credential detections — the field path and length only |
| `data.principal.tokenId` | The `jti`, not the token. A token in a security event is a credential in a log |
| Retention | 400 d hot in the SIEM, 7 years in S3 with Object Lock (`compliance.md` §5) |
| Ordering | Partitioned by `tenantid`; consumers must not assume global order (baseline §13.3) |

### 9.3 Response playbook pointers

| Class | Runbook | First action |
|---|---|---|
| Suspected cross-tenant leak | `docs/runbooks/security-tenant-isolation.md` | Freeze the implicated client; verify RLS with the negative test; preserve the request trace |
| Credential compromise | `docs/runbooks/security-credential-rotation.md` | Rotate via the dual-run workflow; denylist the token family; do **not** delete the old credential before the audit snapshot |
| PAN in scope | `docs/runbooks/security-pci-incident.md` | Quarantine, scope the exposure window from the log index, notify the QSA and the acquirer per contract |
| Image/supply chain | `docs/runbooks/security-supply-chain.md` | Freeze deploys; pin production to the last known-good digest; SBOM diff |
| Audit tamper | `docs/runbooks/audit-tamper.md` | Freeze exports, recompute the chain from the last anchor, preserve the divergence range |

Every runbook ends with the same two steps: write the blameless postmortem, and add or strengthen the control plus the test that would have caught it — the test is what makes the fix durable.
