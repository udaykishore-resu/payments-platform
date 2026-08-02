# ADR-001: Go as the implementation language

## Status
Accepted

## Context (WHAT problem it solves)
We need a language/runtime for a latency-sensitive, concurrency-heavy financial API that must be
easy to reason about for correctness (money bugs are unacceptable), cheap to run at scale, and
fast to start (for HPA scale-out and zero-downtime rolling deploys).

## Decision (WHY chosen)
Go 1.22.

- Compiles to a single static binary → minimal container attack surface (distroless base image,
  no interpreter/JVM to patch), fast cold start (~tens of ms) which matters for HPA reactivity and
  rolling deploys.
- Goroutines + channels make the outbox-relay-as-background-worker pattern (running inside the
  same process as the API) simple and efficient, without a separate runtime/thread-pool model to
  tune.
- Strong static typing + explicit error handling (no exceptions swallowing a failed ledger write)
  fits a domain where "the code silently continued after an error" is a production incident.
- Mature AWS SDK v2, first-class OpenTelemetry and Prometheus client support.
- Fast compilation → fast CI feedback loop, supporting the zero-downtime/frequent-deploy goal.

## WHEN to use this choice
Appropriate for stateless, latency-sensitive backend services with moderate business-logic
complexity and high concurrency needs. Reassess if the team needs heavy numerical/ML workloads
(favor Python) or a large existing JVM ecosystem investment (favor Kotlin/Java).

## Alternatives Considered
- **Java/Kotlin**: mature financial-services ecosystem (many banks run JVM), excellent tooling,
  but heavier memory footprint and slower cold start hurts elastic scaling and pod density; JVM
  warm-up complicates aggressive HPA. Rejected for this service, but reasonable for
  compute-heavy batch/settlement services later.
- **Node.js/TypeScript**: fast iteration, huge ecosystem, but single-threaded event loop is a
  poor fit for CPU-bound crypto/validation work under load, and weaker type guarantees for
  money-handling code without heavy discipline (branded types etc.).
- **Rust**: best raw performance and memory safety, but smaller hiring pool and slower feature
  velocity for a team standing up an entire platform; revisit for extreme low-latency
  fraud-scoring hot paths later.

## Tradeoffs
- Smaller package ecosystem than Java/Node for niche integrations (mitigated: AWS and Postgres
  ecosystems are first-class in Go).
- No mature ORM culture in Go — we write raw SQL. This is treated as a feature, not a bug, for
  financial correctness (see ADR-004), but it means more boilerplate per query and requires
  discipline (migrations, generated query code) to avoid SQL injection and drift.
- Generics are still relatively young in the ecosystem (Go 1.18+); some libraries lag.

## Risks
- Hiring pool for Go is smaller than Java/Node in some markets — mitigated by Go's low learning
  curve for engineers coming from C-family languages.
- Team must enforce discipline around raw SQL (no string concatenation) via linting
  (`sqlc`/parameterized queries only) and code review, or SQL injection risk increases relative to
  an ORM that does it by default.
