# ADR-022: REST for external APIs, gRPC for internal service-to-service

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §19 (API surface, cross-cutting HTTP semantics), §20 (error model, HTTP/gRPC mapping), §5 (deployable units) of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Related to ADR-006 (planes), ADR-007 (no synchronous control→data calls on the hot path)

## Context

Two audiences consume this platform's interfaces, and they have almost nothing in common.
External: merchant developers integrating a payment API, often from a language and toolchain we
do not control, frequently debugging with `curl` and browser dev tools, and subject to corporate
proxies and firewalls. Internal: our own services, in one language, in one cluster, behind a
service mesh, where the constraints are latency, type safety and contract drift.

The forces:

1. **External integration cost is a commercial number.** Time-to-first-successful-payment is
   something merchants measure. Every unfamiliar technology in the integration path adds days.
   REST + JSON is what every payment API in the market speaks (Stripe, Adyen, PayPal), so it is
   what integrators expect and what their existing tooling, HTTP clients, proxies and logging
   already handle.
2. **Internal calls have a latency budget.** The §12 pipeline allots single-digit milliseconds per
   stage. JSON marshal/unmarshal plus HTTP/1.1 connection handling is measurable at 15 000 TPS;
   Protobuf over HTTP/2 with multiplexed connections is meaningfully cheaper in both CPU and
   allocation.
3. **Internal contract drift is a real failure mode.** Two services agreeing on a JSON shape by
   convention will diverge. A compiled IDL makes divergence a build failure.
4. **Baseline §19 and §20 already fix much of this**: public REST at `/v1`, internal gRPC, RFC
   9457 `application/problem+json` errors, and an explicit error-category mapping between HTTP
   status and gRPC code.
5. **Idempotency, ETags, cursor pagination and rate-limit headers are HTTP-native** (§19.3). They
   have natural expressions in REST and awkward ones elsewhere.
6. **Webhooks are inbound HTTP** from gateways (§19.2) — we do not choose that protocol, and any
   external surface must coexist with it.

What breaks if we choose wrong: merchants take weeks instead of days to integrate; or internal
calls eat the latency budget; or internal contracts drift silently until a production
deserialization failure.

## Decision

**REST + JSON over HTTP/1.1 and HTTP/2 for every external API. gRPC over HTTP/2 with mTLS for
every internal service-to-service call. Contracts are `api/openapi/` and `api/proto/`
respectively, and both are generated from, and validated against, committed specifications.**

1. **External (`/v1`)**: REST, `application/json`, errors as `application/problem+json` (RFC 9457)
   with our extensions (§20). URI major versioning, additive-only within a major, deprecation via
   `Sunset` and `Deprecation` headers.
2. **HTTP semantics are load-bearing, not decorative** (§19.3): `Idempotency-Key` request header
   and `Idempotent-Replay: true` on replays; `ETag` + `If-Match` on mutable resources with `412`
   on mismatch; opaque cursor pagination (`?limit=&cursor=`) — **never** offset pagination, which
   is unstable under concurrent writes; `RateLimit-*` and `Retry-After` on `429`; `X-Request-Id`,
   W3C `traceparent`, `X-Correlation-Id`.
3. **Internal**: gRPC with Protobuf, mTLS via the service mesh, deadlines propagated from the
   inbound request's remaining budget. Every internal call has a deadline; a call without one is
   a defect.
4. **The error model maps deterministically** in both directions (§20.1): `VALIDATION` ↔
   `InvalidArgument`, `CONFLICT` ↔ `Aborted`/`FailedPrecondition`, `RATE_LIMIT` ↔
   `ResourceExhausted`, `GATEWAY`/`INFRASTRUCTURE` ↔ `Unavailable`, `TIMEOUT` ↔
   `DeadlineExceeded`. One error taxonomy, two encodings, and the `retryable` flag is
   machine-readable in both.
5. **Contract-first, both sides.** OpenAPI and `.proto` files are committed artifacts; handlers
   and clients are generated or validated against them; a handler whose behaviour diverges from
   its spec fails the contract test suite (`tests/contract`).
6. **Not everything internal is gRPC.** Asynchronous inter-plane communication is Kafka
   (ADR-020). gRPC is for synchronous request/response, which — per ADR-007 — is deliberately
   rare on the payment hot path.
7. **No gRPC endpoint is exposed externally**, and no external REST route is used for
   service-to-service calls.

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **REST externally, gRPC internally (chosen)** | Each audience gets the protocol suited to it: familiar and debuggable outside, fast and type-safe inside; HTTP semantics we depend on (idempotency keys, ETags, cursors, rate-limit headers) are native to REST; Protobuf makes internal contract drift a compile error; HTTP/2 multiplexing and binary encoding save real CPU and allocation at 15 000 TPS; the error taxonomy maps cleanly to both | Two contract toolchains (OpenAPI + Protobuf) and two sets of generated code; the boundary between them needs translation code, which is a place for bugs; engineers must know both; some duplication of types across the boundary | **Accepted** |
| **REST everywhere (internal included)** | One protocol, one toolchain, one mental model; every internal call debuggable with `curl`; no Protobuf toolchain, no code generation for internal calls; simpler onboarding | JSON marshal/unmarshal costs measurable CPU and allocation on a 15 000 TPS path — not fatal, but paid on every call for no benefit; internal contracts drift because JSON shapes are agreed by convention and nothing fails until runtime; HTTP/1.1 connection management is worse than HTTP/2 multiplexing under high concurrency; streaming and deadline propagation are ad hoc. This is the option a simplicity-minded engineer pushes for, and the "one protocol" argument is genuinely strong for a small team — it loses on contract drift more than on performance | Rejected |
| **gRPC everywhere (gRPC-Web or grpc-gateway externally)** | One IDL, one generated-client story; strongly typed contracts for merchants too; excellent streaming | External integrators do not want a Protobuf toolchain to accept a payment; gRPC-Web requires a proxy and still does not work with the debugging tools merchants use; corporate proxies and firewalls handle HTTP/2 trailers inconsistently; idempotency keys, ETags and cursor pagination all become bespoke metadata conventions with no ecosystem support; `curl` and browser dev tools stop working, which is a real cost to every integration conversation. `grpc-gateway` would give REST, but generated from proto — meaning our public contract's shape is dictated by internal service definitions, which is exactly backwards for a contract we must keep stable for years | Rejected |
| **GraphQL externally** | Clients fetch exactly the fields they need; one endpoint; strong introspection and tooling; excellent for read-heavy, deeply nested, client-varied data | Payment APIs are command-shaped, not query-shaped: `POST /v1/payments/{id}/capture` is an operation with side effects, and GraphQL mutations model that awkwardly; HTTP caching, idempotency headers, ETags and status-code semantics are all lost or reinvented at the application layer; unbounded query complexity is a denial-of-service surface on a money path; no payment platform in the market speaks GraphQL, so it adds integration friction rather than removing it. It would be a reasonable choice for a merchant *dashboard* backend and remains available for that | Rejected for the transactional API |
| **REST externally, plain HTTP+JSON internally with a shared Go types package** | Type safety inside without Protobuf, since both sides import the same structs; simplest possible internal contract | Only works while every service is Go and every service deploys in lockstep — a shared types package makes the boundary a compile-time coupling, so two services can never be at different versions of the contract. That is a distributed monolith, and it is precisely the failure ADR-006 warns against | Rejected |

## Consequences

### Positive

- Merchants integrate with tools they already have; the API looks like the payment APIs they have
  already integrated.
- Internal contracts are compiled: a field type change breaks the build, not production.
- HTTP semantics carry real weight — `409` for in-flight idempotency (ADR-009), `412` for ETag
  mismatch, `Retry-After` for backoff — with an ecosystem that already understands them.
- Deadline propagation over gRPC means an inbound 250 ms budget is respected by every downstream
  hop rather than each hop inventing its own timeout.
- One error taxonomy across both encodings means an error can cross the boundary without losing
  its category or its `retryable` flag.

### Negative

- Two contract toolchains, two code-generation steps, two sets of CI checks.
- Translation code between the external REST shape and internal gRPC messages is a real surface
  where fields get dropped or mismapped — and it is boring code, which is where mistakes hide.
- Some types exist twice (once in OpenAPI, once in Protobuf), and keeping their *semantics*
  aligned is a review responsibility that no tool checks.
- Debugging an internal call requires `grpcurl` and reflection rather than `curl`.

### Neutral / accepted costs

- We give up bidirectional streaming externally. Nothing in §19.2 needs it; webhooks and polling
  cover the asynchronous cases.
- gRPC's use is deliberately limited: ADR-007 makes synchronous internal calls rare on the hot
  path, so the performance benefit is smaller than it would be in a chattier architecture. The
  contract-safety benefit is the larger one and applies regardless.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| REST↔gRPC translation drops or mismaps a field | Medium | High — silent data loss on the money path | Translation is generated or table-driven where possible; contract tests assert round-trip fidelity for every mapped type; L6-style echo assertions on amounts (ADR-018) catch the highest-impact case | Round-trip contract test; field-coverage check between OpenAPI and proto schemas |
| Public contract drifts from the OpenAPI spec | Medium | High — merchants integrate against a lie | Handlers validated against the spec in `tests/contract`; spec is the source of truth and CI fails on divergence; spec diffs reviewed as public-contract changes | Contract test failures; spec-diff review gate |
| A breaking change ships inside a major version | Medium | **High** — merchant integrations break | Additive-only rule within a major (§19); an OpenAPI diff tool gates the build on breaking changes; deprecation via `Sunset`/`Deprecation` headers with a stated window | Breaking-change gate in CI |
| Internal gRPC call made without a deadline | Medium | Medium — unbounded latency, resource exhaustion | Interceptor rejects outbound calls with no deadline; deadline derived from the inbound request's remaining budget | Interceptor rejection counter; spans with no deadline attribute |
| gRPC endpoint accidentally exposed externally | Low | High — bypasses edge controls (WAF, rate limit, PAN detector) | Ingress configuration exposes only REST routes; network policy restricts gRPC ports to in-cluster; asserted by a manifest validation check | Manifest check; external port scan in the security pipeline |
| Two protocols confuse new engineers into using the wrong one | Medium | Low–Medium | The rule is one sentence ("external REST, internal gRPC, asynchronous Kafka") and is enforced by the architecture check: external packages may not import gRPC clients and vice versa | Architecture check; code review |
| Error taxonomy diverges between the two encodings | Medium | Medium — inconsistent retry behaviour in clients | One error catalog (`api/errors/catalog.yaml`) generates both mappings; a test asserts every code has both an HTTP status and a gRPC code and a consistent `retryable` flag | Catalog completeness test |

## Validation

- **Contract tests:** every public route validated against `api/openapi/`; every internal service
  validated against `api/proto/`. Divergence fails the build.
- **Integration time:** time-to-first-successful-sandbox-payment for a new merchant developer.
  Target ≤ 1 day with the published SDK, ≤ 2 days without. This is the number that justifies REST
  externally; if it is not met, the API design (not the protocol) is the problem.
- **Latency:** internal gRPC call p99 ≤ 3 ms in-cluster, measured from spans. If internal calls
  cost more than that, the performance argument for gRPC is not being realised.
- **Breaking-change gate:** zero breaking changes shipped within a major version, measured by the
  OpenAPI diff tool over the release history.
- **Error consistency:** every error code in the catalog resolves to exactly one HTTP status, one
  gRPC code and one `retryable` value, asserted by a catalog test.

## Revisit criteria

Reopen if:

1. Merchants consistently request typed contracts and a gRPC or GraphQL surface — the likely
   answer is an *additional* surface generated from the same domain, not a replacement of REST.
2. Internal synchronous call volume grows enough that gRPC's benefits (or costs) become material
   — noting that ADR-007 deliberately keeps this volume low, so growth here is itself a signal
   worth investigating.
3. A non-Go internal service is introduced, which would strengthen the gRPC-internally decision
   rather than weaken it.
4. A merchant-facing dashboard backend is built with genuinely query-shaped, client-varied read
   requirements — GraphQL is a reasonable choice *there* and would not affect the transactional
   API.
