# 17 — Security Architecture

## What this shows and why it matters

Trust zones, the four authentication mechanisms that cross them (`jwt`/`jwks`, `mtls`, `apikey`,
plus gateway signatures inbound), the PCI DSS scope boundary, the secrets provider, and key
management. The single most consequential design decision on this page is the one drawn as a hard
line in Diagram B: **PAN never enters the platform**, which is what keeps eight of the nine
deployables out of the cardholder data environment and the assessment at SAQ-A/A-EP rather than
SAQ-D. Everything else here is defence in depth around identity — tenant identity comes only from
the token, workload identity comes only from SPIFFE and IRSA, and no credential material is ever
held in application memory longer than a call. Diagram C is where each control actually sits in
the request chain.

## Diagram A — Trust zones and identity flows

```mermaid
flowchart TB
  subgraph Z0["Zone 0 - untrusted, public internet"]
    CLIENT["Merchant backend or admin browser"]
    GWCB["Gateway webhook callback"]
  end

  subgraph Z1["Zone 1 - edge, terminates TLS 1.3"]
    WAF["WAF and ALB"]
    OIDC["Tenant identity provider - OIDC and OAuth2"]
  end

  subgraph Z2["Zone 2 - application, mTLS only, SPIFFE identity per workload"]
    PAPI["payment-api"]
    CPAPI["control-plane-api"]
    WHIG["webhook-ingress"]
    PORC["payment-orchestrator"]
    WFW["workflow-worker"]
  end

  subgraph Z3["Zone 3 - data, private subnets, no internet route"]
    AUR["Aurora - TLS, non-BYPASSRLS app role"]
    MSKB["MSK - SASL_SSL"]
    RDS["Redis - in-transit encryption"]
  end

  subgraph Z4["Zone 4 - secrets and keys, behind one ports.SecretsProvider"]
    PROV["secrets.New picks by environment - file in sandbox, AWS in production, and never file in production"]
    SMF["File backend - local stack, tests and the gateway simulator"]
    SMA["AWS backend - Secrets Manager over a hand-rolled SigV4, zero SDK dependencies"]
    STS["STS AssumeRoleWithWebIdentity - the IRSA credential exchange"]
    KMS["KMS CMK per environment, per tenant on the siloed tier"]
  end

  subgraph Z5["Zone 5 - third party"]
    GWX["Stripe, Adyen, PayPal"]
    KYCX["KYC and bank vendors"]
  end

  CLIENT -->|"OAuth2 client credentials or authorization code"| OIDC
  OIDC -->|"access token with tenant claim and scopes"| CLIENT
  CLIENT -->|"jwt - Bearer token over TLS 1.3"| WAF --> PAPI
  CLIENT -->|"apikey - client id and secret, compared in constant time against a stored reference"| WAF
  PAPI -.->|"jwks - cached, background refresh, 2-key rotation window"| OIDC
  GWCB -->|"HMAC or asymmetric signature over the raw octets, not a bearer token"| WHIG

  PAPI ==>|"mtls - SPIFFE URI SAN, not a CN and not a header"| PORC
  CPAPI ==>|"mtls"| WFW
  PORC --> AUR
  PORC --> RDS
  PORC --> MSKB
  WFW --> AUR

  PORC -.->|"resolve a secret reference at the moment of use"| PROV
  WFW -.->|"resolve a secret reference"| PROV
  WHIG -.->|"current plus previous signing secret"| PROV
  PROV --> SMF
  PROV --> SMA
  SMA -.->|"IRSA web identity token exchanged for session credentials"| STS
  SMA -.->|"envelope decrypt, secret path scoped by an IAM prefix condition"| KMS
  PORC ==>|"outbound TLS 1.3 via the egress gateway, static source IPs"| GWX
  WFW ==>|"outbound TLS 1.3"| KYCX
```

## Diagram B — PCI scope boundary, secrets and key management

```mermaid
flowchart TB
  CARD["Cardholder enters PAN and CVV"]
  HOSTED["Gateway hosted fields or SDK tokenization - runs in the gateway origin"]
  TOKEN["Gateway token, network token, or payment method reference"]

  subgraph OUT["OUT OF SCOPE - this platform, 8 of 9 services, SAQ-A or A-EP"]
    API["Our API receives ONLY the token reference"]
    L1P["L1 PAN detector - 13 to 19 digits, Luhn valid after stripping separators"]
    REJ["400 SENSITIVE_DATA_IN_REQUEST, value never logged, security event raised"]
    ALLOW["Structured logging allowlist - only registered field names serialize"]
    LINT["Linter forbids percent-plus-v and percent-hash-v on request types"]
    SECT["Secret of T wrapper - String, MarshalJSON and Format all return REDACTED"]
  end

  subgraph IN["IN SCOPE - optional, segregated, NOT this repository"]
    VAULT["card-vault - separate AWS account, VPC, cluster, dedicated HSM and KMS, own SAQ-D"]
  end

  subgraph KEYS["Key and credential management"]
    REF["Reference grammar - secret scheme, environment, tenant, merchant, gateway, purpose, version"]
    MAT["Material - plaintext exists as a bare string only long enough to be wrapped, and never leaves the secrets package"]
    ERRS["Every error is built from the reference, the HTTP status and the AWS error code - never from a response body, which for GetSecretValue IS the secret"]
    KROT["KMS annual automatic rotation"]
    GROT["Gateway API credentials rotated within 90 days, automated workflow with dual-run overlap"]
    JROT["JWT signing keys rotated every 30 days, 2-key JWKS window"]
    ENV["Application-level envelope encryption for credential material"]
    IRSA["IRSA - one IAM role per deployable, secret paths scoped by prefix"]
    SHRED["Right to erasure implemented as crypto-shredding of the tenant data key"]
  end

  CARD --> HOSTED --> TOKEN --> API
  API --> L1P
  L1P -->|"PAN-like value detected"| REJ
  L1P -->|"clean"| ALLOW
  ALLOW --> LINT --> SECT
  API -.->|"token reference only, tenant capability"| VAULT
  SECT --> ENV --> KMS2["KMS CMK"]
  KMS2 --> KROT
  REF --> MAT --> ERRS
  MAT --> SECT
  SECT --> GROT
  SECT --> JROT
  IRSA --> SECT
  KMS2 --> SHRED
```

## Diagram C — Where each control sits in the request chain

The security stages of `middleware.New`, in the order they actually run. Their placement relative
to one another is the design; see [06 — Data plane](06-data-plane.md) for the full fifteen.

```mermaid
flowchart LR
  BL["bodylimit - buffer the raw octets under the ceiling, then the L1 PAN scan"]
  CT["contenttype"]
  CO["cors - the preflight answer must precede authentication, a browser preflight carries no credentials by design"]
  SH["securityheaders - set on rejected responses too, which is why it is above authentication and not in the handler"]
  AN["authn - jwt via jwks, mtls SPIFFE peer identity, or apikey; a nil authenticator rejects everything"]
  TN["tenant - from the verified principal only, plus the merchant scope guard on any route with a merchantId"]
  AZ["authz - permission derived from the method and route template, so a route with no table entry is DENIED"]
  RL["ratelimit - per tenant and per merchant, which is why it is below tenant resolution"]
  CC["concurrency - adaptive limit plus shedding by priority class"]
  ID["idempotency - innermost, so a request any earlier stage rejected never consumes a key"]

  BL --> CT --> CO --> SH --> AN --> TN --> AZ --> RL --> CC --> ID
```

## Legend and notes

- **Tenant identity is derived exclusively from the authenticated principal.** A `tenant_id` in a
  request body or query string is ignored or, if it disagrees with the token, treated as a
  security event: `403 TENANT_MISMATCH` plus an audit record plus an alert. Every repository method
  extracts tenant from `context.Context` and returns `ErrMissingTenantContext` rather than
  querying if it is absent (§16.2).
- **The webhook edge authenticates in the opposite direction.** Gateways do not present bearer
  tokens; they sign payloads. Verification uses the per-connection signing secret from Secrets
  Manager, with a ±5 min timestamp window and nonce reuse detection to defeat replay (§24, and
  diagram 11).
- **Service-to-service is mTLS with SPIFFE workload identity, terminated by the mesh.** There is no
  in-cluster plaintext hop, and a compromised pod cannot impersonate another workload by presenting
  a stolen bearer token, because the peer identity is a certificate bound to the service account.
- **IRSA gives one IAM role per deployable, with secret paths scoped by a prefix condition**
  (`/{env}/{tenant}/{merchant}/{gateway}`). `payment-orchestrator` cannot read
  `workflow-worker`'s KYC vendor credentials even though both run in the same cluster (§17.2).
- **Four authentication mechanisms, not one.** `jwt` (bearer, OAuth2/OIDC) and `jwks` (cached key
  resolution with a background refresh and a two-key rotation window) at the public edge; `mtls`
  for service-to-service, keyed on the certificate's SPIFFE **URI SAN** rather than its CN,
  because a CN is a string an operator typed while a URI SAN is a statement the cluster CA stands
  behind and is bound to the private key that just completed the handshake; and `apikey` for
  machine clients, whose secret is compared in constant time against material resolved from the
  secrets provider by reference. The composition root picks which a binary uses; the middleware
  takes an `Authenticator` interface and knows about none of them.
- **The secrets provider is one port with two implementations, and the choice is made in one
  place.** `secrets.New` resolves `auto` to `file` in sandbox and `aws` in production; it will
  never resolve `auto` to the file backend in production, and an explicit `file` still has to get
  past `NewFileProvider`'s own refusal. Nine binaries need a provider, and a backend selected
  correctly in eight composition roots and wrongly in the ninth is a credential outage in the
  deployable nobody exercises locally.
- **The AWS backend is written directly against `net/http`, with SigV4 implemented in-package.**
  Four Secrets Manager calls and one STS call do not justify the official SDK's transitive module
  set running inside the process that holds gateway credentials; SigV4 is a two-hundred-line HMAC
  construction with published test vectors, and asserting against those buys the same assurance
  at a dependency count of zero. The trade is stated plainly: retry classification, endpoint
  resolution and the credential chain are now ours to own.
- **Plaintext credential material never leaves the secrets package.** It exists as a bare string
  only long enough to be moved into a `Material`, and every error returned from that package is
  built from the reference, the HTTP status and the AWS error code — never from a response body,
  which for `GetSecretValue` *is* the secret.
- **A secret the ingress cannot read is `502 DEPENDENCY_FAILURE`, not `401`.** Refusing the
  webhook is correct either way, but the two page different people, and conflating an
  infrastructure failure with an authentication failure sends the wrong team to the wrong
  dashboard during an outage.
- **Three independent controls stop credential and PAN leakage into logs**, and they are
  structural rather than procedural: an allowlist-based structured logger (unregistered fields are
  simply not serialized), a linter ban on `%+v` / `%#v` over request types, and a `Secret[T]`
  wrapper whose `String()`, `MarshalJSON()` and `Format()` all return `[REDACTED]`. There is no
  code path that can print a secret by accident.
- **Crypto-shredding satisfies right-to-erasure without breaking financial retention.** Destroying
  the tenant's data key renders personal data unrecoverable while financial records remain
  retained under the legal-obligation basis (§17.3).
- **3DS is a policy outcome of the risk engine, not a flag.** PSD2/SCA exemptions (TRA, low value,
  MIT) are modelled explicitly per merchant and per corridor, and every exemption decision is
  audited (§17.3).
- **Audit records are hash-chained and CP.** Under partition the audit writer buffers to a local
  WAL and replays rather than breaking the chain (§15).

## Related

- [Design baseline §16.2 isolation guard, §17 PCI scope and enforcement, §17.3 regulatory boundaries](../spec/00-design-baseline.md)
- [01 — System context](01-system-context.md), [16 — AWS architecture](16-aws-architecture.md), [18 — Observability architecture](18-observability-architecture.md)
- [docs/security.md](../security.md), [docs/compliance.md](../compliance.md)
