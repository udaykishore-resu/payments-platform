# payments-platform

A multi-tenant payment gateway onboarding and payment orchestration platform. Two products share
one codebase and one control surface: **onboarding** takes a merchant from "signed up" to
"processing live money" through an automated, resumable, auditable workflow covering KYC/KYB, bank
validation, gateway provisioning, configuration, sandbox validation and certification; and
**orchestration** accepts payment instructions from onboarded merchants and executes them against
one of several third-party gateways with scored routing, failover, idempotency, webhook
reconciliation and a shadow ledger. The problem it solves is the one that appears the moment a
platform has more than one gateway and more than one merchant: keeping "did this money move?"
answerable — under timeouts, retries, partitions, crashes and duplicate requests — without ever
answering it twice.

Everything in this repository is derived from a single source of truth,
[`docs/spec/00-design-baseline.md`](docs/spec/00-design-baseline.md). Where any document, contract,
test or line of code disagrees with the baseline, the baseline wins and the other artifact is a
defect. Read the [status and limitations](#status-and-limitations) section before drawing
conclusions about production readiness — this is a reference implementation, not a system that has
processed real money.

Go 1.24.7 · one module · nine deployables · no copyleft in the dependency graph (CI-enforced)

---

## What this is not

The ambiguity register in baseline §1.2 resolves these explicitly, because each one would otherwise
be assumed by somebody:

| Not this | Why | What happens instead |
|---|---|---|
| **An acquirer or card scheme member** | No licence, no scheme membership, no settlement with schemes | Licensed gateways (Stripe, Adyen, PayPal) are orchestrated behind an adapter SPI |
| **A card data vault** | Storing PAN drags all nine services into PCI DSS SAQ-D | PAN never enters the platform; only gateway tokens, network-token vault references and stored-instrument references are accepted ([§17](docs/spec/00-design-baseline.md), [ADR-017](docs/adr/ADR-017-pci-scope-minimisation.md)) |
| **A KYC/KYB decision engine** | Identity verification is a regulated, vendor-supplied capability | A KYC provider port is integrated; the platform owns the *workflow*, never the *decision* |
| **A general-purpose BPM engine** | Scope discipline | A purpose-built durable saga engine behind a port, plus a Temporal adapter ([ADR-014](docs/adr/ADR-014-owned-workflow-engine-behind-port.md)) |
| **A fraud-scoring ML system** | Different problem, different data, different team | A risk *policy* engine with a port for an external scorer |
| **A payment institution holding client money** | Avoids e-money licensing, safeguarding and client-money segregation | Funds settle gateway → merchant. The ledger is a *shadow* ledger for reconciliation, not custody (A1) |

---

## Start here

| If you are… | Read | Because it answers |
|---|---|---|
| **An architect** evaluating the design | [`docs/architecture.md`](docs/architecture.md) | The five planes, the control loop, the container/component view, and a 15-entry trade-off register with the rejected options steelmanned |
| **A reviewer** checking the design holds together | [`docs/spec/00-design-baseline.md`](docs/spec/00-design-baseline.md) | Everything, normatively: ambiguity register, bounded contexts, layering rule, state machines, idempotency contract, CAP-per-operation, PCI boundary, definition of done |
| **A security architect** | [`docs/security.md`](docs/security.md) then [`docs/compliance.md`](docs/compliance.md) | Trust zones, identity flow, secret handling, the `Secret[T]` type, threat model; then PCI/PSD2/GDPR/AML boundaries and evidence |
| **An SRE / on-call engineer** | [`docs/runbooks/README.md`](docs/runbooks/README.md), then [`docs/failure-handling.md`](docs/failure-handling.md), [`docs/observability.md`](docs/observability.md), [`docs/disaster-recovery.md`](docs/disaster-recovery.md) | **35 runbooks** behind an index, one per distinct `runbook_url`, with `scripts/check-runbook-links.sh` asserting that no alert reference dangles and that no paging alert lacks one. Then: what breaks, how it degrades, what the SLIs and burn-rate alerts are, and the ordered region-failover procedure |
| **A payments specialist** | [`docs/payment-flow.md`](docs/payment-flow.md) then [`docs/data-plane.md`](docs/data-plane.md) | Authorize → capture → settle → refund → dispute, where idempotency binds, and the 17-stage request pipeline with its latency budgets |
| **A cloud architect** | [`docs/deployment.md`](docs/deployment.md) + [`terraform/README.md`](terraform/README.md) | Environments, progressive delivery, the AWS substrate and the multi-region posture |
| **A compliance reviewer / auditor** | [`docs/compliance.md`](docs/compliance.md), [`docs/spec/09-traceability.md`](docs/spec/09-traceability.md) | Regulatory boundaries, retention, and the requirement → design → code → test matrix |
| **New to the repository** | [`docs/diagrams/README.md`](docs/diagrams/README.md) | 20 diagrams with a stated reading order per role; start 01 → 02 → 20 |
| **Looking for a decision's rationale** | [`docs/adr/README.md`](docs/adr/README.md) | 24 indexed ADRs, each naming a mechanical check that would catch its violation |
| **Writing code here** | [`CONTRIBUTING.md`](CONTRIBUTING.md) | Layering rules, definition of done, and the recipes for adding a gateway, a rule, an event, a state or a migration |

A full annotated index is in [`docs/README.md`](docs/README.md); `docs/` now holds 104 markdown files, 35 of them runbooks.

---

## The five planes

The primary decomposition is the **plane**, not the aggregate and not the table. A plane is a
horizontal slice with its own availability target, scaling signal, blast radius and change cadence
([ADR-006](docs/adr/ADR-006-five-plane-decomposition.md)). Bounded contexts remain the *code*
boundary; planes are the *deployment* boundary. That is why there are nine binaries rather than
twenty-five.

**Control plane** (99.9 %) owns declared desired state: tenants and identity, the merchant
registry, the gateway registry, and configuration and policy. It is authoritative for what *should*
be true, is versioned and audited on every write, and is never on the payment hot path. One
deployable, `control-plane-api`, scaling on admin request rate. Its blast radius is one tenant's
configuration.

**Automation plane** (99.9 %) turns desired state into actual state. `workflow-worker` leases
durable onboarding sagas, runs the twelve activities of `merchant-onboarding@v1`, checkpoints each
step's result before the next begins, compensates completed steps in strict reverse order on abort,
and blocks on the manual compliance gate until an authorized principal signals it. It scales on
onboarding starts, retry backlog and DLQ depth, and can be down for an hour without a single
payment being affected.

**Validation plane** has **no deployable**, and that is deliberate. It is a library
(`internal/validation`) of 243 registered rules across seven levels — L1 schema at the edge, L2
merchant, L3 gateway, L4 configuration, L5 payment pre-dispatch, L6 gateway-response, L7 domain
state — linked into whichever binary runs them. Making it a service would put a network hop in the
middle of the money path in exchange for nothing, because the rules are pure and total. It is a
*plane* rather than merely a package because it has its own governance: rule IDs are stable and
documented, and CI fails the build if a registered rule is undocumented.

**Data plane** (99.99 %) is where money moves. Five deployables — `payment-api`,
`payment-orchestrator`, `webhook-ingress`, `outbox-relay`, `event-consumer` — cover payment
orchestration, gateway integration, webhook ingestion and the ledger. Payment writes are strongly
consistent and fail closed rather than degrade. It reads configuration only from a locally cached
snapshot fed by Kafka, so it has no synchronous dependency on the control plane and never inherits
99.9 % from it.

**Observability plane** (99.9 %) is downstream of everything and blocks nothing. It owns the
hash-chained audit trail, the RED and business metric families, traces and logs. Its one upstream
edge is deliberate: gateway health windows publish `gateway.health_changed.v1`, which the routing
engine consumes and the control plane records.

### The control loop

The platform is a reconciliation system, not a request/response system with background jobs bolted
on:

```
desired state → validate → automate → actual state → data plane → observe → evaluate → control
      ▲                                                                                    │
      └────────────────────────────────────────────────────────────────────────────────────┘
```

Each arrow is a mechanism with an artifact, not a metaphor. Desired state is a versioned
configuration document; validate is L4 producing an `Outcome` per rule ID; automate is a workflow
instance whose activities mutate the world; actual state is gateway sub-accounts, webhook
registrations, secret versions and certification reports; the data plane runs against a cached
snapshot of that state; observe produces `pp_*` series and audit records; evaluate compares them to
thresholds and emits health events, burn-rate alerts and reconciliation exceptions; and control
issues a new configuration version, a suspension, a circuit-state change or a rollback.
[`docs/architecture.md`](docs/architecture.md) §3.2 names four concrete instances of the loop
closing.

### Primary system diagram

Reproduced verbatim from [`docs/diagrams/02-high-level-design.md`](docs/diagrams/02-high-level-design.md),
Diagram A. Dotted edges are asynchronous; solid edges are synchronous calls or direct writes.

```mermaid
flowchart TB
  subgraph PCTL["Control plane - 99.9 percent"]
    CPAPI["control-plane-api"]
    BC1["BC-1 Tenant and Identity"]
    BC2["BC-2 Merchant Registry"]
    BC4R["BC-4 Gateway Registry"]
    BC5["BC-5 Configuration and Policy"]
  end

  subgraph PAUT["Automation plane - 99.9 percent"]
    WFW["workflow-worker"]
    BC3["BC-3 Onboarding sagas and gates"]
  end

  subgraph PVAL["Validation plane - library, no deployable"]
    VL["L1 to L7 rule engine"]
  end

  subgraph PDATA["Data plane - 99.99 percent"]
    PAPI["payment-api"]
    PORC["payment-orchestrator"]
    WHIG["webhook-ingress"]
    ORLY["outbox-relay"]
    ECON["event-consumer"]
    BC6["BC-6 Payment Orchestration"]
    BC4I["BC-4 Gateway Integration adapters"]
    BC7["BC-7 Webhook Ingestion"]
    BC8["BC-8 Ledger and Reconciliation"]
  end

  subgraph POBS["Observability plane"]
    BC9["BC-9 Audit hash chain"]
    TEL["Telemetry and health feedback"]
  end

  subgraph PTOOL["Non-production and ops"]
    GSIM["gateway-simulator - test builds only"]
    PCTLC["platformctl"]
  end

  CPAPI --> BC1
  CPAPI --> BC2
  CPAPI --> BC4R
  CPAPI --> BC5
  CPAPI -.->|"configuration.published.v1"| PDATA
  BC2 -.->|"merchant.activated.v1 and merchant.suspended.v1"| PDATA

  CPAPI -->|"start onboarding case"| WFW
  WFW --> BC3
  WFW -->|"provision, register webhook, certify"| BC4I
  WFW -->|"apply configuration version"| CPAPI

  PAPI --> PORC
  PORC --> BC6
  PORC --> BC4I
  WHIG --> BC7
  BC7 -.->|"webhook.received.v1"| ECON
  ECON --> BC8
  ECON --> BC9

  PVAL -.->|"L1 L5 L6 L7"| PDATA
  PVAL -.->|"L2 L3"| PAUT
  PVAL -.->|"L4"| PCTL

  PDATA -->|"domain and audit events"| POBS
  PAUT --> POBS
  TEL -.->|"gateway.health_changed.v1 feedback loop"| PCTL
  GSIM -.->|"contract suite target"| BC4I
  PCTLC -.->|"migrations, config validation, DR drills"| PCTL
```

The datastore and event-backbone view is Diagram B in the same file.

---

## The decisions that matter most

### 1. The payment attempt is a first-class aggregate — double-charging is structurally impossible

A `Payment` is the merchant's *intent*; a `PaymentAttempt` is *one execution of that intent against
one gateway*. Failover creates a **new attempt**; it never mutates the old one. The attempt row is
written **before** the gateway call, carrying a deterministic gateway idempotency key derived as
`base32(HMAC-SHA256(attempt_id, gateway_salt))[:32]`, so a transport retry to the same gateway
reuses the key and the gateway dedupes, while a failover to a different gateway is correctly a new
authorization.

What makes this structural rather than careful is invariant **I3**: at most one attempt per payment
may be in a successful terminal state, enforced by a **partial unique index on
`(payment_id) WHERE outcome='SUCCESS'`**. A bug in the orchestrator, the consumer dedup path, or a
duplicated Kafka message cannot move money twice, because the database refuses the second row.
Amendment A-02 keeps that guarantee under partitioning: `payments` and `payment_attempts` are
range-partitioned on a `partition_month` derived from the **payment's** ULID timestamp, so every
attempt of a payment shares that payment's partition and the partial unique index constrains the
full set rather than one month of it.

→ [ADR-012](docs/adr/ADR-012-payment-attempt-first-class-aggregate.md) · baseline §9 (I3, A-02), §9.1, §14.4

### 2. No timer may fail a payment

If a gateway call times out or returns an ambiguous transport error, we do not know whether money
moved. The attempt is recorded `TIMEOUT_UNKNOWN`, the payment **stays `PROCESSING`**, and the
synchronous endpoint returns `202` with `state: PROCESSING` rather than an error. It is never
retried automatically and never failed by a timer. Only positive evidence resolves it, in order of
speed: a gateway webhook; a reconciler polling the gateway's lookup API using our deterministic
idempotency key; or a settlement report.

The alternative — auto-failing a timed-out authorization and retrying elsewhere — is the single most
common cause of double charges in real platforms. A timeout is an absence of information, not a
failure, and the state model has to be able to say so.

→ [ADR-013](docs/adr/ADR-013-timeout-leaves-payment-processing.md) · baseline §12.3, A7

### 3. Postgres is authoritative for idempotency; Redis is a non-authoritative accelerator

The idempotency scope is `(tenant_id, merchant_id, method, path_template, idempotency_key)`, and the
claim is an `INSERT … ON CONFLICT DO NOTHING` against a unique index in Postgres. A request
fingerprint — SHA-256 over the JCS-canonicalized body plus the scope tuple — catches the client bug
where one key is reused for two different payments (`422 IDEMPOTENCY_KEY_REUSED`). A concurrent
duplicate gets `409 IDEMPOTENT_REQUEST_IN_PROGRESS` with `Retry-After`; it is never blocked on a
lease held by another process, because blocking a request thread on a distributed lease is how
thread pools die under a retry storm. An expired lease is reclaimed atomically.

**Redis mirrors completed records purely for latency.** A Redis miss, a stale entry, or a total
Redis outage degrades p99 and never correctness, because the unique index is in Postgres. This is
the property that lets Redis be operated as a cache — restarted, flushed, resized — rather than as a
system of record.

→ [ADR-009](docs/adr/ADR-009-postgres-authoritative-idempotency.md) · baseline §14, A6

### 4. PCI scope minimisation — PAN cannot enter the platform

The API accepts three payment-instrument shapes and only three: a gateway token, a network-token
vault reference, or a stored-instrument reference. There is no field named `pan`, `cardNumber`,
`cvv`, `cvc`, `track1`, `track2` or `expiry` anywhere in the contract — not optional, not
deprecated, absent — and every token field must **start with a letter**, so a 13–19 digit PAN cannot
satisfy the pattern even in a field that accepts strings.

That is the schema half. The enforcement half is independent: the L1 validator runs a Luhn-checked
PAN detector over **every string field of every request**; a hit is `400 SENSITIVE_DATA_IN_REQUEST`,
the value is not logged, and a security event is raised. Structured logging is allowlist-based (only
registered field names serialize) and the linter forbids `%+v`/`%#v` on request types, so there is
no path by which a PAN reaches a log even if one arrives. Credentials are only ever `Secret[T]`,
whose `String()`, `MarshalJSON()` and `Format()` all return `[REDACTED]`.

The intent is assessment at SAQ-A/A-EP rather than SAQ-D. If a tenant requires vaulting, that
capability lives in a physically and administratively separate system with its own SAQ-D
assessment, reached through a port, and is **not part of this repository**.

→ [ADR-017](docs/adr/ADR-017-pci-scope-minimisation.md) · baseline §17, A2 · [`docs/security.md`](docs/security.md), [`docs/compliance.md`](docs/compliance.md)

---

## Repository layout

Binding, per baseline §25. The dependency rule is enforced mechanically by
`scripts/check-architecture.sh` in CI, not by review.

```
cmd/                        composition roots only — one per deployable. Wiring, no business logic.
  payment-api/              data plane public REST edge; the only public money-path ingress
  payment-orchestrator/     owns the payment FSM and gateway dispatch; bulkheaded per gateway
  control-plane-api/        desired-state writes; never on the payment hot path
  webhook-ingress/          accept-and-persist only, ≤50 ms budget; processing is asynchronous
  workflow-worker/          leases onboarding sagas; runs activities and compensations
  outbox-relay/             Postgres → Kafka. The only publisher in the system.
  event-consumer/           projections, ledger, audit, notifications
  gateway-simulator/        test-only deployable; guarded out of production images
  platformctl/              migrations, seeding, config validation, certification runs, DR drills

internal/
  domain/                   entities, value objects, FSMs. Imports stdlib and pkg/ ONLY —
                            no database/sql, no net/http, no otel, no AWS, no uuid library.
    shared/ tenant/ merchant/ payment/ gateway/ routing/ risk/ ledger/ audit/ compliance/ config/
  application/              use cases + the ports they own. May NOT import infrastructure or
                            any adapter other than the gateway SPI.
    ports/ merchant/ onboarding/ payment/ webhook/ config/ gateway/ ledger/ apptest/
  validation/               the validation plane: engine + rules/l1api…l7domain (243 rules)
  workflows/                engine port, Postgres engine, Temporal adapter, onboarding definition
  policies/                 RBAC/ABAC, risk and compliance policy evaluation
  events/                   envelope, registry, codec, idempotent consumer
  platform/                 idempotency, tenantctx, authn, authz, config, runtime, health, errors
  adapters/gateway/         spi (port declaration — stdlib + domain only), registry, contract
                            suite, stripe, adyen, paypal, simulator, httpx
  transport/                httpapi (REST, always built), grpcapi (harness always built;
                            service impls behind //go:build grpc)
  infrastructure/           postgres, redis, kafka, secrets, telemetry, httpx, grpcx,
                            resilience, crypto, clock. May import anything; nothing may
                            import another adapter's internals.
  composition/              shared wiring helpers for the composition roots

pkg/                        stdlib-only, extractable: apierror, ids, money.
                            A dependency here is a dependency imposed on every future consumer.
api/                        openapi/ (6 677 lines, 21 paths), proto/ (6 .proto, codegen not
                            committed), events/ (26 JSON Schemas), errors/catalog.yaml (97 codes)
migrations/                 16 ordered SQL pairs, forward-only, every up has a down
config/                     per-environment defaults and seed policies. NEVER a secret —
                            every credential is a secret:// reference resolved at use.
deploy/                     docker-compose.dev.yml and the local observability config
deployments/k8s/            kustomize base + dev/staging/prod overlays; deployments/argocd/
helm/                       umbrella chart + 9 service subcharts + pp-common
terraform/                  12 modules + dev/staging/prod stacks + policy references
docs/                       spec/, adr/, diagrams/, runbooks/ (35 runbooks + an index), plane docs
tests/                      integration/, contract/, e2e/, chaos/, load/, testenv/
scripts/                    verify, the twelve fitness functions it runs, dev stack, migrate, seed, DR drill
```

The one deliberate exception to the layering table is `internal/adapters/gateway/spi`: it is a
*port declaration* (interfaces and value types only, importing nothing outside stdlib,
`internal/domain/**` and `pkg/**`), placed next to its implementations for discoverability. The
exception is narrow and mechanically enforced — the architecture check asserts both that `spi`
imports nothing forbidden, and that no *other* package under `internal/adapters/` is imported from
`internal/application/`.

---

## Quick start

### Prerequisites

| Tool | Version | Needed for |
|---|---|---|
| Go | 1.24.7 or newer (`go.mod` declares 1.24.7) | everything |
| Docker + Compose v2 | any current | `make dev-up`; `docker-compose` v1 is detected as a fallback |
| GNU make, bash | — | the targets below |
| python3 with `pyyaml`, `jsonschema` | — | `check-openapi.sh`, `check-events.sh`, `check-error-catalog.sh` |
| golangci-lint | v2.5.0 | `make lint`; falls back to `go run …@v2.5.0` if absent |
| govulncheck, syft, k6, terraform | optional | `make vuln`, `make sbom`, `make loadtest`, terraform validation |

`make` with no target prints the full target list — 45 of them, each with a description parsed from
its rule line.

### 1. Bring the local stack up

```bash
make dev-up
```

`scripts/dev-up.sh` starts Postgres 16.6, Redis 7.4, Redpanda (Kafka), the gateway simulator, the
OTel collector, Jaeger, Prometheus and Grafana; **waits for every container's own healthcheck**
rather than sleeping; then runs migrations to completion and seeds the `dev` profile at scale 25.
It prints the endpoints when the stack is *usable*, not merely started:

```
Postgres    postgres://pp_dev:pp_dev_not_a_real_password@localhost:5432/pp
Redis       redis://localhost:6379
Kafka       localhost:19092   (Redpanda; advertised as localhost from the host)
Simulator   http://localhost:8090
OTLP        localhost:4317 (grpc) / 4318 (http)
Prometheus  http://localhost:9090
Grafana     http://localhost:3000   (anonymous admin)
Jaeger      http://localhost:16686
```

Useful flags: `scripts/dev-up.sh --no-seed`, `--no-migrate`, `--rebuild`, `--timeout 300`.
Tear down with `make dev-down` (removes volumes).

### 2. Migrations

`make dev-up` already applies them. Against a database you started yourself:

```bash
make migrate          # scripts/migrate.sh up  — runs the static checks first
make migrate-status   # applied vs pending
```

Both default to `DSN=postgres://pp_dev:pp_dev_not_a_real_password@localhost:5432/pp?sslmode=disable`;
override with `make migrate DSN=...`.

### 3. Seed

```bash
make seed             # scripts/seed.sh --profile dev --scale 25
```

The dataset is **generated, never copied** — there is no import path and no `--from-dump`, because
anonymising a relational payment dataset is not reliably achievable. The same profile, scale and
seed produce byte-identical data. The command prints what you need for the next step:

```
tenant   ten_…
gateways stripe, adyen, simulator
merchants (25):
  mrc_…            ← the first merchant is always ACTIVE, deliberately
  …
credential references to provide in PP_SECRETS_FILE:
  secret://sandbox/ten_…/mrc_…/simulator
```

Profiles: `minimal`, `dev`, `integration`, `load`, `e2e` (`config/seed/profiles.yaml`).

### 4. Run a service

```bash
make run-payment-api            # :8080, admin/metrics on :8081
make run-control-plane-api      # :8082, admin :8092
make run-webhook-ingress        # :8083, admin :8093
make run-payment-orchestrator   # internal gRPC :9095, admin :8087
make run-workflow-worker        # admin :8084
make run-gateway-simulator      # :8090
```

Every `run-*` target sources [`.env.dev`](.env.dev) with `PP_SERVICE_NAME` already set. That file is
the single statement of the local variable set: shared values at the top, and a per-service block
that supplies each binary's listener addresses and the variables only it requires. To see exactly
what a target will export:

```bash
make dev-env SERVICE=payment-api
```

`PP_ENVIRONMENT` must be `sandbox` or `production` — there is **no** `development`, and a process
handed anything else exits before it binds a port. Every service reports *every* missing or
malformed variable at once, so one fix-and-restart cycle is enough.

`config/dev.yaml` documents the shape of these variables; nothing loads it, and the environment is
the source of truth (`config/README.md`).

The probes and metrics bind to `PP_ADMIN_ADDR` on a **separate port** from the public listener, so
that an ingress routing the public port cannot reach `/metrics` by construction.

`make run-outbox-relay` and `make run-event-consumer` do **not** start yet, and the reason is a
defect rather than a missing variable — see [status and limitations](#status-and-limitations).

### 5. Make a payment against the gateway simulator

`POST /v1/payments` requires an `Idempotency-Key` header — the API returns
`400 IDEMPOTENCY_KEY_REQUIRED` without one. The body carries integer minor units and a payment
*reference*, never an instrument.

```bash
curl -sS -X POST http://localhost:8080/v1/payments \
  -H 'Authorization: Bearer '"$PP_TEST_AUTH_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: 6b1f0f6e-3a1a-4f0e-9a3d-2f2f9f3f7f11' \
  -H 'X-Request-Id: req-local-0001' \
  -d '{
    "merchantId": "mrc_REPLACE_WITH_AN_ACTIVE_MERCHANT_FROM_make_seed",
    "amount": { "amount": 1050, "currency": "USD" },
    "paymentMethod": "CARD",
    "paymentMethodReference": {
      "type": "GATEWAY_TOKEN",
      "gatewayCode": "simulator",
      "token": "tok_sim_visa_approve",
      "brand": "VISA",
      "last4": "4242",
      "expiryMonth": 11,
      "expiryYear": 2029
    },
    "captureMode": "MANUAL",
    "statementDescriptor": "ACME* ORDER 4471",
    "payerCountry": "US",
    "reference": "order-4471",
    "metadata": { "orderId": "4471" }
  }'
```

A successful authorization returns `201` with `ETag`, `Location` and `X-Request-Id` headers and a
body of this shape:

```json
{
  "id": "pay_01JB8Z9K2QW3E4R5T6Y7V8J9N0",
  "merchantId": "mrc_01JB8Z11111111111111111111",
  "state": "AUTHORIZED",
  "amount": { "amount": 1050, "currency": "USD" },
  "authorizedAmount": { "amount": 1050, "currency": "USD" },
  "capturedAmount": { "amount": 0, "currency": "USD" },
  "refundedAmount": { "amount": 0, "currency": "USD" },
  "paymentMethod": "CARD",
  "captureMode": "MANUAL",
  "riskDecision": "ALLOW",
  "riskScore": 12,
  "threeDsStatus": "EXEMPT_LOW_VALUE",
  "routingPlanId": "rpl_01JB8ZEEEEEEEEEEEEEEEEEEEE",
  "currentAttemptId": "att_01JB8Z44444444444444444444",
  "reconciliationRequired": false,
  "version": 3,
  "createdAt": "2026-08-26T14:03:10.980Z",
  "updatedAt": "2026-08-26T14:03:11.412Z"
}
```

Repeat the identical request with the **same** `Idempotency-Key` and you get the same body back with
`Idempotent-Replay: true`. Send a *different* body with the same key and you get
`422 IDEMPOTENCY_KEY_REUSED`. If the gateway times out you get `202` with `"state": "PROCESSING"` —
not an error — and you poll `GET /v1/payments/{paymentId}`. Never re-issue a timed-out payment under
a new key.

Errors are RFC 9457 `application/problem+json` with a machine-readable `code`, `category` and
`retryable` flag; the full catalogue is `api/errors/catalog.yaml`.

**On the bearer token:** tenancy is derived exclusively from the authenticated principal — a
`tenantId` in a body or query string is ignored, or, if it disagrees with the token, raised as a
`403 TENANT_MISMATCH` security event.

To get one locally, run the development OIDC issuer and mint a token:

```bash
make run-dev-issuer                         # serves JWKS + /token on :8088
make dev-token                              # prints a token and an example curl
export PP_TEST_AUTH_TOKEN="$(scripts/dev-token.sh)"
```

`scripts/devissuer` generates an RS256 key pair, publishes it at the `PP_AUTH_JWKS_URL` that
`.env.dev` and `config/dev.yaml` already point at, and mints tokens carrying every claim the
validator requires: `iss`, `aud`, `exp`, `nbf`, `iat`, `jti`, `tenant_id`, `env`, `scope` and
`roles`. Its own test drives `internal/platform/authn`'s validator against it, so a token it mints
is a token the platform accepts. There is deliberately no "skip authentication in development"
switch — a switch like that is one environment variable away from being on in production.

Two behaviours worth knowing before they cost you an hour:

- The default roles are `svc:payment-client,svc:onboarding-client,tenant-admin`, because RBAC is
  the **union** of grants across roles and no single role covers both reading payments
  tenant-wide and creating them. `operator` in particular denies `payments:write`.
- `merchant_scope` is empty by default. Setting it (`scripts/dev-token.sh --merchant mrc_…`)
  narrows the credential, and a narrowed credential is denied tenant-wide listings — which is
  correct, and is the least obvious `403` in the platform.

---

## How to verify the whole thing

```bash
make verify        # everything CI verifies, in CI's order — run this before pushing
make verify-fast   # the same, minus the race detector (~4 min tier)
```

`scripts/verify.sh` runs 17 gates. It is ordered cheapest-and-most-likely-to-fail first, because a
developer who waits four minutes to be told about a formatting problem stops running the script. It
does **not** stop at the first failure — every stage runs and the summary lists all of them, so one
CI round trip does not become five. Pass `--fail-fast` when iterating on a single stage,
`--only NAME` / `--skip NAME` to select.

| # | Stage | What it checks |
|---|---|---|
| 1 | `fmt` | `gofmt -l .` — formatting drift |
| 2 | `vet` | `go vet ./...` |
| 3 | `build` | `go build ./...` |
| 4 | `test-race` (or `test` with `--fast`) | `go test ./... -race -count=1`. `-count=1` defeats the cache; a cached PASS from before the change is the most misleading thing a verifier can print |
| 5 | `lint` | `golangci-lint run ./...` against `.golangci.yml` |
| 6 | `architecture` | The baseline §4 dependency rule, including the SPI exception, as a fitness function |
| 7 | `error-catalog` | `pkg/apierror` ↔ `api/errors/catalog.yaml` agree; codes unique, categories valid, status/retryable consistent |
| 8 | `rules-documented` | Every registered validation rule ID appears in `docs/validation-plane.md`, and every documented ID resolves |
| 9 | `metrics-cardinality` | §22.3 — `merchant_id` and `payment_id` are never metric labels; ≤10⁴ series per metric per service |
| 10 | `events` | The event registry ↔ `api/events/` JSON Schemas |
| 11 | `openapi` | The REST contract validates; CI additionally diffs it against `main` for breaking changes |
| 12 | `migrations` | Numbering, up/down pairing, RLS presence, destructive-change markers |
| 13 | `secrets` | Credential, key and PAN patterns across the tree |
| 14 | `licences` | No copyleft in the dependency graph |
| 15 | `runbook-links` | Every alert's `runbook_url` resolves to a file under `docs/runbooks/`; every alert with `page: "true"` has one; every runbook is referenced or indexed |
| 16 | `doc-references` | Every repo-relative path cited by a document under `docs/` or a root `*.md` exists. This is the check that catches a document promising a script that was never written |
| 17 | `coverage` | The per-scope coverage floors of `docs/testing.md` §1.1, and the 36 critical-path properties of §1.2 — each must still name a test that exists. With `--fast`, only the registry half runs |

Individual gates are also targets: `make check-arch`, `make check-contracts`, `make check-security`,
`make check-runbooks`, `make check-docs`, `make critical-paths`.
`make traceability` regenerates `docs/spec/09-traceability.md`.

CI (`.github/workflows/ci.yml`) runs these as 15 jobs, adding SAST (CodeQL), `govulncheck`,
dependency review, Kubernetes/Helm manifest validation, terraform fmt/validate/tflint/checkov, image
build and scan, and a coverage gate. `nightly.yml` adds flake hunting, chaos, a soak test, a rescan,
the DR drill and traceability regeneration.

---

## Testing

Three claims, and every test exists to support one of them: **money cannot move twice**, **state
cannot become impossible**, **a tenant cannot see another tenant's data**. A test supporting none of
those has to justify its maintenance cost.

| Level | Command | Build tag | Needs running services |
|---|---|---|---|
| Unit (pure domain, application and infrastructure with fakes) — 1 179 tests | `make test` (`go test ./... -short`) | none | **No** |
| The same with the race detector | `make test-race` (`-race -count=1`) | none | **No** |
| Contract (gateway adapters, event schemas, JSON Schema compatibility) — 50 tests | `make test-contract` | **none** — no tag is passed, deliberately: the suite reads committed schemas and drives adapters against stubbed transports, so it belongs in the cheapest stage and also runs under `make test` | **No** |
| Integration (real Postgres, Redis, Kafka) — 66 tests | `make test-integration` | `integration` | **Yes** — services that are **already running**; there is no testcontainers dependency. Point `PP_TEST_POSTGRES_DSN` / `PP_TEST_REDIS_ADDR` / `PP_TEST_KAFKA_BROKERS` at them, or run `make dev-up` |
| End-to-end (black-box, through the HTTP edge) — 6 tests | `make test-e2e` | `e2e` | **Yes** — `make dev-up` first, plus `PP_TEST_BASE_URL`, `PP_TEST_AUTH_TOKEN`, `PP_TEST_TENANT_ID`, `PP_TEST_SIMULATOR_URL` |
| Chaos (fault injection, crash, partition, clock skew, retry storm) — 20 tests | `make test-chaos` | `chaos` | **Yes** — in-process port decorators always run; the destructive infrastructure scenarios need `PP_TEST_CHAOS_INFRA=1` |
| Load (k6: steady-state, ramp, spike, soak) | `make loadtest SCENARIO=… BASE=… TOKEN=…` | n/a (JavaScript) | **Yes** — a deployed target |
| Everything but chaos and load | `make test-all` | — | Yes (integration) |

**Suites that need services skip rather than fail** when their variables are unset, and the skip
message names the exact variable and what it should contain. That is deliberate: a suite that fails
on a laptop for want of a container teaches people to ignore red.

`make cover` produces `coverage.out` and `coverage.html`, prints the total, and then runs
`scripts/coverage.sh` against the profile it has just produced. That script **fails** on a drop
below the per-scope floor and **warns** on the distance to the `docs/testing.md` §1.1 target,
which every scope is currently below; `scripts/coverage.sh --enforce-targets` treats the target as
the threshold and currently fails. It then checks that all 36 properties in
[`tests/critical_paths.yaml`](tests/critical_paths.yaml) still name a test that exists —
`make critical-paths` runs that half alone, in under a second.

Full strategy, the named failure-scenario tests, the data policy and the flakiness policy:
[`docs/testing.md`](docs/testing.md) and [`tests/README.md`](tests/README.md).

---

## Deployment

| Layer | Where | What is there |
|---|---|---|
| **Infrastructure** | [`terraform/`](terraform/) | 12 modules — network, eks, aurora, elasticache, msk, kms, secrets, s3, dns, edge, observability, dr — and `envs/{dev,staging,prod}` stacks with their own backends. 57 `.tf` files. Reference IAM policy documents in `terraform/policies/`. |
| **Charts** | [`helm/`](helm/) | An umbrella `payments-platform` chart with `values-{dev,staging,prod}.yaml`, 9 per-service subcharts, and a `pp-common` library chart. `helm/scripts/validate-manifests.py` and `check-no-literal-secrets.sh` gate rendered output. |
| **Cluster primitives** | [`deployments/k8s/`](deployments/k8s/) | Kustomize base (namespaces, network policies, resource quotas, limit ranges, priority classes, service accounts, OTel agent DaemonSet and gateway) plus dev/staging/prod overlays. |
| **GitOps** | [`deployments/argocd/`](deployments/argocd/) | `AppProject` and an `ApplicationSet` for the fleet, plus the `.github/actions/gitops-promote` composite action. |
| **Pipelines** | [`.github/workflows/`](.github/workflows/) | `ci.yml` (15 jobs, gated on `ci-ok`), `cd.yml` (publish → post-merge gates → promote dev → smoke → staging → smoke → prod, with a `rollback` job), `codeql.yml`, `nightly.yml`. |
| **Images** | [`Dockerfile`](Dockerfile) | One multi-stage build parameterised by `SERVICE`; `SOURCE_DATE_EPOCH` and `-trimpath` make a local build byte-identical to CI's for the same commit; a guard stage refuses to build a production image of `gateway-simulator`. |

The narrative — environments, progressive delivery, the promotion gates, the AWS substrate, the
active/passive money-region posture — is in [`docs/deployment.md`](docs/deployment.md) and
[`docs/disaster-recovery.md`](docs/disaster-recovery.md).

---

## Project statistics

Measured against this tree, not estimated. Commands are given so any number can be re-derived.

### Code

| Metric | Value | How measured |
|---|---:|---|
| Go packages | 83 | `go list ./... \| wc -l` |
| Go files | 478 | `find . -name '*.go' \| wc -l` |
| — implementation | 311 files, **108 391 lines** | `find . -name '*.go' -not -name '*_test.go' \| xargs wc -l` |
| — test | 167 files, **63 795 lines** | `find . -name '*_test.go' \| xargs wc -l` |
| Total Go | 172 186 lines | sum of the two |
| Test-to-implementation ratio | 0.59 : 1 | — |
| Top-level test functions | **1 271** | `grep -rh '^func Test' --include='*_test.go' . \| wc -l` (internal 1 186 · tests 61 · cmd 22 · scripts 2 · pkg 0) |
| — run by `go test ./...` (untagged) | 1 179 in 138 files | the rest are behind `integration` (66), `chaos` (20) and `e2e` (6) |
| Benchmarks | 2 | `grep -rh '^func Benchmark' --include='*_test.go' . \| wc -l` |
| Statement coverage, `go test ./... -short` | **57.9 %** overall; `internal/domain/payment` 97.2 %, `internal/domain/merchant` 99.3 % | `make cover`; per-scope figures in [`docs/testing.md`](docs/testing.md) §1.1 |
| Deployable binaries | 9 | `ls cmd/` |
| Gateway adapters | 4 (stripe, adyen, paypal, simulator) behind 1 SPI | `ls internal/adapters/gateway/` |
| Build tags in use | `integration` (18 files), `chaos` (8), `e2e` (4), `temporal` (2), `grpc` (1), `tools` (1) | `grep -rh '^//go:build' --include='*.go' . \| sort \| uniq -c` |

### Contracts and data

| Metric | Value | How measured |
|---|---:|---|
| OpenAPI document | 6 677 lines, **21 paths**, **36 operations** (26 path operations + 10 webhook callbacks) | `wc -l`, and `grep -c 'operationId:' api/openapi/payments-platform.v1.yaml` |
| Protobuf files | 6 | `find api/proto -name '*.proto' \| wc -l` |
| Event JSON Schemas | **26** (25 `.v1` event types + 1 envelope) | `ls api/events/*.schema.json \| wc -l` |
| Error codes | **81**, plus 16 detail sub-codes | `python3 -c "import yaml;d=yaml.safe_load(open('api/errors/catalog.yaml'));print(len(d['codes']),len(d['detail_codes']))"`. Note that `grep -c '^  - code:'` returns 97 because it counts both lists |
| Validation rules registered | **243** across L1–L7 (L1 38 · L2 40 · L3 28 · L4 44 · L5 48 · L6 22 · L7 23) | `docs/validation-plane.md` §3.8, asserted by `rules.Count()` |
| Additional documented rule identifiers | 156 emitted directly by aggregates and invariant checks | `docs/validation-plane.md` §6 |
| SQL migrations | **16** up/down pairs, 2 595 lines of forward SQL | `ls migrations/*.up.sql \| wc -l`; `ls migrations/*.up.sql \| xargs wc -l` |
| State machines with an exhaustive transition test | **10**, over 1 103 `(from, to)` pairs, 166 accepted and 937 rejected | [`docs/state-machines.md`](docs/state-machines.md) §16.2 |
| Critical-path properties with a named test | **36**, 70 test references | `tests/critical_paths.yaml`, checked by `scripts/coverage.sh` |
| Bounded contexts | 9 | baseline §3 |
| Planes | 5 | baseline §2 ("Plane"), `docs/architecture.md` §2.2 |

### Documentation and infrastructure

| Metric | Value | How measured |
|---|---:|---|
| Markdown documents under `docs/` | **104** | `find docs -name '*.md' \| wc -l` |
| — runbooks | **35** plus an index | `ls docs/runbooks/*.md \| wc -l` (36 including `README.md`) |
| — normative specification documents | 8 (`docs/spec/`) | the baseline alone is 1 089 lines |
| — architecture and plane documents | 18 plus `docs/README.md` | `ls docs/*.md \| wc -l` returns 19 |
| — ADRs | **24 indexed**, 19 present as files (ADR-006…024); ADR-001…005 are index entries only | `ls docs/adr/*.md \| wc -l` returns 20, including the index |
| — diagram documents | 20 + an index | `ls docs/diagrams/ \| wc -l` |
| Markdown documents repository-wide | 119 | `find . -name '*.md' -not -path './.git/*' \| wc -l` |
| Mermaid diagrams in `docs/diagrams/` | **42** | `grep -c '^```mermaid' docs/diagrams/*.md` summed |
| Mermaid diagrams across all of `docs/` | 122 | same, recursive. All 122, plus the 3 in this file, parse under `mermaid.parse()` |
| Terraform files / modules / env stacks | 57 / 12 / 3 | `find terraform -name '*.tf' \| wc -l`; `ls terraform/modules \| wc -l`; `ls terraform/envs` |
| Helm files / subcharts | 129 / 9 + `pp-common` + umbrella | `find helm -type f \| wc -l`; `ls helm/charts` |
| Kubernetes + ArgoCD manifests | 28 files | `find deployments -type f \| wc -l` |
| Make targets | **51** | `grep -cE '^[a-z][a-z0-9-]*:' Makefile` |
| Shell scripts in `scripts/` | 21 (+3 Go tools: `archcheck`, `specdump`, `devissuer`) | `ls scripts/*.sh \| wc -l` |
| CI/CD workflows | 4 (`ci` with 15 jobs, `cd`, `codeql`, `nightly`) | `.github/workflows/` |
| Verification gates in `make verify` | **17** | `scripts/verify.sh` |

---

## Status and limitations

**This is a reference implementation, delivered as a single body of work. It has never processed
real money, has never run in a production environment, and has never been assessed by a QSA.** The
design is complete and internally consistent, the module compiles clean (`go build ./...`,
`go vet ./...` and `go test ./... -race` all exit 0), `golangci-lint run` reports **0 findings**,
and **1 271** test functions exist — but the following are real, specific gaps, and a reader
should weigh them before treating any part of this as production-ready.

### Build-tagged code that the default build does not compile

- **The Temporal workflow adapter** (`internal/workflows/engine/temporal/temporal.go`) is behind
  `//go:build temporal` and **requires `go.temporal.io/sdk`, which is not in `go.mod`**. `go build
  ./...` and `go test ./...` never see it, and **nothing in CI compiles it** — no job passes
  `-tags temporal`. The decision is deliberate: the SDK pulls in a large gRPC/protobuf surface
  that only one alternative engine implementation needs. The consequence is that the "the port
  keeps the build-vs-buy decision reversible" claim of
  [ADR-014](docs/adr/ADR-014-owned-workflow-engine-behind-port.md) is **unverified by any build in
  this repository**. The default Postgres engine is fully built and tested.
- **The gRPC service implementations** (`internal/transport/grpcapi/services.go`) are behind
  `//go:build grpc` pending protobuf codegen: they compile only after `buf generate` has produced
  the bindings, which are deliberately not committed. **No CI job runs `buf generate`**, so the
  tagged file is currently compiled by nothing. The gRPC *harness* — interceptor chain, error
  mapping, health service, keepalive, graceful stop — carries no tag and is always built, vetted
  and race-tested.

### Two services cannot start locally, and one of the two defects also breaks production TLS

`make run-outbox-relay` and `make run-event-consumer` cannot start. Two independent defects:

1. **The environment sets do not intersect.** `isLocal` in
   `internal/infrastructure/kafka/config.go` and `internal/infrastructure/redis/client.go` both
   accept `local`, `test`, `development` or `dev`, and permit `PLAINTEXT`/`SASL_PLAINTEXT` only
   when it returns true. `shared.ParseEnvironment` accepts only `sandbox` and `production`. The
   two sets have **no intersection**, so plaintext is unreachable from any configuration the
   platform will actually start with. This is why [`.env.dev`](.env.dev) leaves `PP_REDIS_ADDR`
   empty and lets idempotency fall back to Postgres.
2. **`kafka.Config.ClientOptions` sets both `kgo.Dialer` and `kgo.DialTLSConfig`.** franz-go
   rejects that combination outright, so client construction fails — and this one is **not**
   limited to local runs: it breaks `SSL` and `SASL_SSL`, which is what production configuration
   uses.

Both fixes are small — accept `sandbox` in both `isLocal`s, and drop the plain dialer when
`DialTLSConfig` is set — and both are stated in [`.env.dev`](.env.dev) where a developer meets
them. The other six `run-*` targets start.

### Domain defects found by the state-machine audit

Each of these is a genuine code defect, not a documentation gap; they are documented in
[`docs/state-machines.md`](docs/state-machines.md) at the section that describes the machine.

- **Multiple partial captures are impossible.** `CAPTURED → CAPTURED` is not a declared
  self-transition, so a second `MarkCaptured` on an already-captured payment is refused with
  `INVALID_STATE_TRANSITION` — *after* invariant I2's cumulative check has already passed, so the
  error the caller sees is about the state machine rather than about the amount. Any configured
  multiple-partial-capture limit above 1 is unreachable. (`docs/state-machines.md` §3.)
- **A dispute won after settlement lands in `CAPTURED`, not `SETTLED`.**
  `Payment.ResolveDispute(won=true)` decides between the two by scanning the aggregate's
  **pending-event** slice for `payment.settled.v1`, and the repository drains that slice on every
  write. A payment loaded from the database therefore always looks unsettled. (§3.)
- **The documented bank-replacement recovery always fails.**
  `merchant.ValidateBankAccount` special-cases `BANK_VALIDATION_FAILED` as a state to advance
  from, but then attempts `→ BANK_VALIDATED`, which the transition table permits only from
  `KYC_APPROVED`; from `BANK_VALIDATION_FAILED` the only forward edge is `→ KYC_APPROVED`. The
  call is always refused — and it **mutates the bank-account record before** the refused
  transition, so the aggregate is left with an account marked verified and a status that says
  otherwise. (§2.1.)

### Schema drift: three column constraints are narrower than their state machine

`migrations/0013_state_guards.up.sql` seeds real transition tables for the payment and merchant
machines, and `TestTransitionTablesMatchDomain` keeps those two honest. Nothing compares a
machine's state universe with the `CHECK (… IN (…))` on its column, and three have drifted apart:

| Column | Migration allows | The domain has | Cannot be persisted |
|---|---:|---:|---|
| `pp.gateway_connections.status` | 7 | 9 | `PROVISIONING_FAILED`, `CERTIFICATION_FAILED` |
| `pp.workflow_instances.state` | 7 | 11 | `RETRY_BACKOFF`, `PARKED`, `POISONED`, `COMPENSATED`, `CANCELED` — and the migration allows `ABORTED`, which the domain does not define |
| `pp.workflow_steps.state` | 8 | 13 | `TIMED_OUT`, `AMBIGUOUS`, `LEASE_LOST`, `RETRY_SCHEDULED`, `DLQ` — the entire retry and unknown-outcome path |

In every case the states that cannot be persisted are the failure and recovery states, which is
to say the ones that matter when something has gone wrong. A check comparing each machine's
`States()` against its column constraint would catch all three and does not exist.

### Suites that need services and skip otherwise

Integration (66 tests), e2e (6) and the destructive half of chaos require running
Postgres/Redis/Kafka, a running stack, and in the e2e case a bearer token. They **skip with an
explanatory message** naming the exact variable when it is unset. That is the right default — a
suite that fails on a laptop for want of a container teaches people to ignore red — but it also
means a green `make test` says nothing about those 92 tests, and the CI integration job is the
only place they are known to have run.

### Infrastructure that was validated structurally but never applied

Terraform was validated **structurally**: HCL2 parse, reference resolution, and module-input
completeness. Helm and Kustomize were validated the same way: YAML parse, template balance, values
resolution, and per-kind field assertions. **Neither has ever been applied to a real cluster or a
real AWS account.** No `terraform apply` has run, no chart has been installed, the ArgoCD
`ApplicationSet` has never synced, and the DR drill (`make dr-drill`) has never executed against
real infrastructure — it needs credentials for a `dr-verify` account that does not exist. Note
also that `terraform`, `helm` and `kubeconform` **could not be installed in the build
environment**, so the structural validation above is what was possible, not what was preferred.
Consequently the RPO ≤ 5 s / RTO ≤ 15 min targets in baseline §18 are **design targets, not
measured results**.

### Chaos scenarios that exist as design rather than as tests

`tests/chaos/` covers 20 scenarios in-process against the real orchestrator and the real
resilience primitives. Node loss, AZ loss, region loss, disk pressure, certificate expiry,
Secrets Manager denial, KEDA scaling under outbox backlog and the combined-fault scenario need a
cluster, and no cluster has ever run this. They are specified in
[`docs/failure-handling.md`](docs/failure-handling.md) and
[`docs/disaster-recovery.md`](docs/disaster-recovery.md) §9 and tested by nothing.

### Documented artifacts that do not exist

<!-- doc-refs: allow-missing begin -->

| Referenced by | Missing artifact |
|---|---|
| `docs/testing.md` §1.2 | `scripts/mutation-probe.sh` — the mutation-probe harness, and the `mutation_probes` key in the critical-path registry. §1.2 now says so and says what building it would take. The registry itself exists (`tests/critical_paths.yaml`, 36 entries) and is checked by `scripts/coverage.sh` |
| `docs/testing.md` §9.3 | `scripts/flaky-gate.sh`, and the 30-day per-test results store the whole flakiness policy depends on. The nightly `flake-hunt` job is the one part that is real |
| `docs/observability.md`, `docs/deployment.md` §4.1 | `scripts/metrics-lint.sh` and `scripts/check-logging.sh` — the metric-cardinality and logging fitness functions are `scripts/check-metrics-cardinality.sh` and the `.golangci.yml` rules respectively |
| `docs/deployment.md`, `docs/runbooks/error-budget-policy.md` | `scripts/slo-gate.sh` — the error-budget gate is a policy, not a script |
| baseline §25 | `pkg/otelx` — `pkg/` contains `apierror`, `ids` and `money` only |
| `docs/events.md` | `scripts/check-event-compat.sh` — event compatibility is asserted by `tests/contract/compat_test.go` instead |

<!-- doc-refs: allow-missing end -->

`scripts/check-doc-references.sh` (the `doc-references` stage of `make verify`) now asserts that
every repo-relative path cited by a document under `docs/` or by a root `*.md` resolves. The table
above is the residue: artifacts a document deliberately reports as absent, marked so the check
does not flag them.

### Test coverage that is thinner than the numbers suggest

1 271 test functions is a real number, but it is unevenly distributed, and the overall statement
coverage under `go test ./... -short` is **57.9 %**. Packages with implementation code and **no
test file at all** include:

- `internal/domain/shared` — IDs, clock, primitives, and the generic `StateMachine` that every FSM
  in the platform is built from. Exercised only through its users
- `pkg/money`, `pkg/ids`, `pkg/apierror` — **none**. `Money` is the type baseline §7 says is
  "enforced by the type system and by tests"; the tests are the half that is missing, and
  `docs/testing.md` §2.3 lists the five properties that ought to be asserted
- `internal/transport/httpapi` — router, decode, problem rendering; none at that level, though
  `httpapi/handlers` is well covered
- every `cmd/` composition root

The two aggregates the earlier version of this section listed as untested are no longer:
`internal/domain/payment` is at **97.2 %** and `internal/domain/merchant` at **99.3 %**, with
**70** test functions and **314** cases between them, including three exhaustive FSM property
tests (`TestPaymentMachineAcceptsExactlyTheDeclaredEdges`,
`TestAttemptMachineAcceptsExactlyTheDeclaredEdges`,
`TestMerchantMachineAcceptsExactlyTheDeclaredEdges`).

Per-scope coverage against the gates `docs/testing.md` §1.1 declares, all of which are below
target: domain 80.5 % (target 95), application 52.5 % (90), validation 80.3 % (95), workflows
57.7 % (85), adapters 54.0 % (80), infrastructure 53.2 % (70), repository overall 57.9 % (80).
`scripts/coverage.sh` fails on a **drop** below the measured floor and warns on the distance to
the target, so the gap is visible on every run rather than only in a document.

### Requirements traceability

**44 of 190 requirements remain untraced.** 31 are process or operational commitments with no code
to point at — a runbook that must be followed, a review that must happen, an SLO that is a
promise. The other **13 are real coverage gaps**: a requirement the design states and nothing in
the tree demonstrably implements or tests. `docs/spec/09-traceability.md` names each one;
`make traceability` regenerates it.

### Other known inconsistencies

- **ADR-001 through ADR-005 have no files.** They are index entries in `docs/adr/README.md`
  describing pre-expansion decisions, explicitly recorded "for continuity". ADR-003's partial
  supersession by ADR-020 is therefore documented in the superseding record only.
- **`docs/spec/` skips 07 and 08.** The sequence runs 00–06 then 09; the traceability matrix is 09.
- `config/*.yaml` is read by no Go code and consumed by no chart or overlay. `config/README.md`
  says so at the top rather than describing a precedence order that nothing implements; the files
  document the shape of each environment's configuration, and `.env.dev` is what actually drives a
  local run.
- The four state machines of `docs/state-machines.md` §12–§15 — onboarding case, idempotency
  record, inbound webhook, reconciliation exception — have no `shared.StateMachine` table and
  therefore no exhaustive property test. They are enforced by a column `CHECK` and by the SQL
  each command issues.
- The workflow instance and step machines have exhaustive tests, but those derive their
  expectation from the machine itself rather than from an independent table, so they cannot catch
  a wrong edge. `docs/state-machines.md` §16.2 marks them as such.

### What is genuinely solid

For balance: the module compiles clean and `golangci-lint` reports zero findings; the layering
rule is enforced by an executable fitness function rather than by review; the idempotency, outbox,
RLS, invariant and concurrency behaviours have real integration tests against real Postgres; ten
state machines are asserted exhaustively over 1 103 ordered pairs, eight of them against an
independently transcribed expectation; the payment and merchant aggregates are near-fully covered;
the 243 validation rules are registered, documented and CI-checked for documentation; the OpenAPI
contract is large, example-rich and breaking-change-diffed against `main`; every ADR names the
mechanical check that would catch its violation; and 36 named money-safety properties each point
at a test that a build gate proves still exists. The gaps above are gaps in *evidence and
operations*, and three specific domain defects — not gaps in the design.

---

## Contributing

[`CONTRIBUTING.md`](CONTRIBUTING.md) covers the layering rules and why they are enforced
mechanically, the twelve-point definition of done, and the recipes for adding a gateway, a
validation rule, an event, a state-machine transition and a migration.

## Security

Vulnerability reporting, supported versions, the PCI scope statement and credential-exposure
response: [`SECURITY.md`](SECURITY.md).

## Licence

Apache License 2.0 — see [`LICENSE`](LICENSE). `scripts/check-licences.sh` gates the dependency
graph against copyleft so that this stays true of everything the binaries link.
