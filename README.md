# Payments Platform — Payment Transaction Processing (reference slice)

This repository is the first production-grade vertical slice of an enterprise fintech platform:
**Payment Transaction Processing** — the primitive that moves money between two ledger accounts
exactly once, durably, with full auditability. Stack: **Go on AWS (EKS, Aurora PostgreSQL, SQS,
Terraform)**. See `docs/01-requirements.md` for why this specific feature was chosen as the
flagship deliverable instead of a shallow pass across many features.

## Where to start

| If you want... | Read... |
|---|---|
| The business/technical requirements and acceptance criteria | `docs/01-requirements.md` |
| The system architecture and request flow | `docs/02-architecture.md` |
| *Why* each major technology was chosen, alternatives considered, tradeoffs | `docs/adr/ADR-00*.md` |
| What happens when X fails and how it recovers | `docs/04-failure-recovery-design.md` |
| The security model (authN/authZ, encryption, network, OWASP) | `docs/05-security-architecture.md` |
| Metrics/logs/traces/alerts | `docs/06-observability.md` |
| SLIs/SLOs/error budgets/chaos testing | `docs/07-reliability-slo.md` |
| On-call procedures | `docs/08-runbook.md` |
| The go/no-go gate before this serves real traffic | `docs/09-production-checklist.md` |
| Design-review / calibration questions | `docs/10-interview-questions.md` |

## Repository layout

```
docs/                        Planning artifacts (read these first — "plan everything, then build")
services/payments-api/       The Go service implementing Create Payment / Get Payment
  cmd/server/                 Composition root (main.go) — wiring only, no business logic
  internal/domain/            Core types + invariants (Money, Payment, LedgerEntry) — no I/O
  internal/service/           Business logic, transport-agnostic, unit-tested with a fake repo
  internal/repository/        The only package that knows SQL; the transactional outbox + ledger
                               write lives here (see ADR-004)
  internal/api/                HTTP handlers, DTOs, routing
  internal/middleware/         Auth (JWT/JWKS), rate limiting, circuit breaker, recovery, metrics
  internal/outbox/             Background relay publishing outbox rows to SQS
  internal/events/             SQS publisher
  internal/observability/      Structured logging, Prometheus metrics, OpenTelemetry tracing
  migrations/                  SQL schema, including the DB-enforced ledger balance invariant
  tests/integration/           Full-stack tests against a real Postgres (build-tagged)
deploy/k8s/                  Kustomize base + dev/staging/prod overlays
deploy/terraform/            AWS infrastructure modules (Aurora, SQS, ECR, EKS node group) + prod env
.github/workflows/           CI (lint/test/scan/build) and CD (progressive deploy + canary + rollback)
scripts/                     smoke-test.sh, canary-analysis.sh used by CD
```

## Running locally

```
cd services/payments-api
docker compose -f ../../deploy/docker-compose.test.yml up -d   # Postgres on :5433
export DATABASE_DSN="postgres://payments_test:payments_test@localhost:5433/payments_test?sslmode=disable"
make run    # AUTH_DISABLED=true, so no live JWKS/AWS credentials needed locally
```

```
make test               # unit tests (no DB required)
make test-integration   # requires DATABASE_DSN pointed at a live, migrated Postgres
```

## Known limitations of this reference implementation

Written transparently rather than glossed over, per the same standard the code itself is held to:

- **No Go toolchain was available in the environment that authored this repo** (no network access
  to `proxy.golang.org` or `go.dev`), so the code was written and manually reviewed line-by-line
  for correctness but has **not been compiled**. `go.sum` is intentionally not committed — CI's
  first job (`go mod tidy`) generates and should commit it. Treat the first CI run on this repo as
  the first real compiler feedback and fix anything it surfaces (most likely candidates: minor
  `gofmt` alignment nits, since formatting could not be auto-fixed without `gofmt` itself).
- The outbox relay's max-attempts handling (`internal/outbox/relay.go`) logs and stops retrying a
  poison event rather than moving it to a dedicated failed-events table — noted inline as a
  scope cut, not an oversight (see the comment in `publishOne`).
- `deploy/k8s/overlays/{staging,dev}` reuse `base/externalsecret.yaml`'s `prod/payments-api/*`
  Secrets Manager paths rather than environment-specific paths — flagged with a `NOTE` in each
  overlay's `kustomization.yaml`; a real rollout adds the retargeting patch before staging is used
  for anything real.
- `deploy/terraform/envs/prod/main.tf` is a composition root, not an `apply`-ready root: VPC/EKS
  cluster bootstrapping and the exact OIDC provider ARN are left as required variables /
  placeholders (`REPLACE_WITH_EKS_OIDC_PROVIDER_ARN`) rather than invented values, since those are
  account-specific and shouldn't be guessed.
- Refunds, multi-currency FX, card-network acquiring, and fraud scoring are explicitly out of
  scope for this slice — see `docs/01-requirements.md`, "Explicit Non-Goals" — this repo is the
  ledger-movement primitive the rest of the platform builds on, not the whole platform.

## Feature status

Create Payment (`POST /v1/payments`) and Get Payment (`GET /v1/payments/{id}`), built to the full
bar described in `docs/09-production-checklist.md`: idempotent, double-entry, DB-enforced balance
invariant, transactional outbox, circuit breaker, rate limiting, OAuth2/OIDC auth, structured
logs/metrics/traces, Kubernetes deployment with zero-downtime rollout, Terraform-defined AWS
infrastructure, and a CI/CD pipeline with automated canary analysis and rollback.
