# 17 — Security Architecture

## What this shows and why it matters

Trust zones, the identity flows that cross them (OIDC, OAuth2, mTLS, IRSA), the PCI DSS scope
boundary, and secret and key management. The single most consequential design decision on this
page is the one drawn as a hard line in Diagram B: **PAN never enters the platform**, which is what
keeps eight of the nine deployables out of the cardholder data environment and the assessment at
SAQ-A/A-EP rather than SAQ-D. Everything else here is defence in depth around identity — tenant
identity comes only from the token, workload identity comes only from SPIFFE and IRSA, and no
credential material is ever held in application memory longer than a call.

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

  subgraph Z4["Zone 4 - secrets and keys"]
    SM["Secrets Manager"]
    KMS["KMS CMK per environment, per tenant on the siloed tier"]
  end

  subgraph Z5["Zone 5 - third party"]
    GWX["Stripe, Adyen, PayPal"]
    KYCX["KYC and bank vendors"]
  end

  CLIENT -->|"OAuth2 client credentials or authorization code"| OIDC
  OIDC -->|"access token with tenant claim and scopes"| CLIENT
  CLIENT -->|"Bearer token over TLS 1.3"| WAF --> PAPI
  PAPI -.->|"JWKS verification, cached, background refresh, 2-key window"| OIDC
  GWCB -->|"HMAC or asymmetric signature, not a bearer token"| WHIG

  PAPI ==>|"mTLS, SPIFFE peer identity"| PORC
  CPAPI ==>|"mTLS"| WFW
  PORC --> AUR
  PORC --> RDS
  PORC --> MSKB
  WFW --> AUR

  PORC -.->|"IRSA assumed role, secret path scoped by prefix condition"| SM
  SM -.->|"envelope decrypt"| KMS
  WFW -.->|"IRSA"| SM
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
  SECT --> GROT
  SECT --> JROT
  IRSA --> SECT
  KMS2 --> SHRED
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
