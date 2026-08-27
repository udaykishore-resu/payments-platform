# ADR-011: Gateway-agnostic core with an adapter SPI and capability descriptors

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §2 (Capability Descriptor), §3 (Anti-Corruption Layer), §11.4 (certification), §12 (pipeline stages 14–15), §25 (repository layout) of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Related to ADR-006 (planes), ADR-015 (routing), ADR-012 (attempts)

## Context

The platform orchestrates payments across Stripe, Adyen and PayPal today, and must be able to add
a fourth gateway without touching the payment domain. Every gateway differs in wire format,
error taxonomy, capability set, webhook signature scheme, idempotency semantics and partial-capture
behaviour. Baseline §3 mandates an Anti-Corruption Layer around every gateway: **no gateway type
ever appears in `internal/domain`.**

The forces:

1. **Gateway differences are not cosmetic.** Stripe uses form-encoded requests and an
   `Idempotency-Key` header; Adyen uses JSON with a `reference` field; PayPal uses OAuth bearer
   tokens with a different refresh lifecycle. Decline reasons are three incompatible
   vocabularies. Partial capture is supported by some and not others, with different limits on
   the number of captures. 3DS integration differs structurally, not just in field names.
2. **The routing engine must know capabilities *before* dispatch.** §12 stage 12 selects a
   gateway; if capability mismatches only surface as a runtime `ErrNotSupported` at stage 14, we
   have already burned 8 s of budget and produced a failed attempt for a payment that was never
   routable there. Capabilities must be *data* the router can filter on.
3. **The domain must not learn gateway concepts.** If `payment.Payment` grows an `if gateway ==
   "adyen"` branch, the aggregate is no longer testable without a gateway, and every new gateway
   edits the money domain — the highest-risk code in the repository.
4. **Adding a gateway is a recurring business event.** Time-to-integrate is a commercial number.
   The target is that a new gateway is a new package plus a descriptor plus a passing contract
   suite, with zero edits to `internal/domain` and `internal/application`.
5. **Substitutability must be enforced, not hoped for.** An adapter that maps a network timeout to
   `DECLINED` turns a retryable ambiguity into a terminal failure and corrupts the failover
   decision — that is a direct path to a double charge or a lost sale. LSP violations here are
   financial bugs.

What breaks if we choose wrong: gateway concepts leak into the domain and every integration
becomes a change to the money path; or capability mismatches are discovered at dispatch time and
degrade the authorization rate; or an adapter's error mapping silently violates the failover
contract.

## Decision

**A gateway-agnostic core with a Service Provider Interface (`internal/adapters/gateway/spi`),
declarative capability descriptors, and a machine-checked substitutability contract suite.
Adapters are compiled in and registered explicitly at the composition root.**

1. **The required core interface** every adapter implements (§ LLD): `ID()`, `Capabilities()`,
   `Authorize`, `Capture`, `Refund`, `Void`, `Lookup`, `VerifyWebhook`.
   - `Lookup(ctx, GatewayIdempotencyKey)` is **mandatory, not optional**: it is what closes the
     `TIMEOUT_UNKNOWN` loop (ADR-013). A gateway with no lookup API cannot be integrated safely
     and that is a deliberate gate, not an oversight.
   - `VerifyWebhook` is **pure** — it does not touch the network — so it is testable with fixtures
     and cannot become a latency dependency in the ingress path.
2. **Optional capabilities are separate interfaces** (ISP): `Provisioner`, `WebhookRegistrar`,
   `PartialCapturer`, `ThreeDSInitiator`. Discovered by type assertion and **cross-checked against
   the descriptor at startup**: an adapter whose descriptor claims a capability it does not
   implement fails the readiness check. The binary refuses to start rather than failing a payment.
3. **The capability descriptor is data** (§2): countries, currencies, payment methods, operations,
   3DS support, partial capture (and its limit), refund window, webhook signature scheme. It is
   versioned, persisted, and referenced by ID on every routing plan so a routing decision is
   reproducible against the descriptor that was in effect (ADR-015).
4. **Normalized response** (`spi.Response`): `Outcome ∈ {SUCCESS, DECLINED, ERROR,
   TIMEOUT_UNKNOWN}`, a normalized `DeclineReason` carrying a Hard/Soft classification,
   `GatewayRef`, echoed `Amount` (for L6 verification, §21), and an **allowlisted** `Raw` map —
   never the raw response body, because that is how PAN-adjacent data ends up in our logs (§17).
5. **The substitutability contract is executable.** `adapters/gateway/contract` runs the same
   suite against every adapter, including the simulator. Its clauses (from the LLD) are binding:
   transport failure → `ERROR` or `TIMEOUT_UNKNOWN`, **never** `DECLINED`; an unparseable response
   → `ERROR`, never `SUCCESS`; `SUCCESS` always carries a `GatewayRef` usable by `Lookup`; amount
   and currency always echoed; hard declines classified `Hard` and never retried elsewhere;
   capability differences appear in `Capabilities()`, never as a runtime `ErrNotSupported`.
6. **Registration is explicit** in the composition root (ADR-023). No dynamic discovery, no
   plugin loading, no `init()` side-effect registration.

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **In-process SPI + capability descriptors + contract suite (chosen)** | Domain stays gateway-free and testable with no gateway at all; capabilities are filterable data, so routing excludes ineligible gateways *before* dispatch; one contract suite proves substitutability for every adapter including new ones; no network hop between orchestrator and adapter, preserving the §12 budget; a new gateway is a package, a descriptor and a green suite | All adapters share the orchestrator's process, so a badly-behaved adapter (unbounded allocation, a blocking call outside the context) can affect its neighbours; adding a gateway requires a deploy of the orchestrator; the SPI must be general enough for gateways we have not met, which risks either over-abstraction or churn | **Accepted** |
| **A service per gateway (`stripe-service`, `adyen-service`, …)** | Strong isolation: a bad adapter cannot affect others; independent deploys, so onboarding a gateway does not redeploy the money path; per-gateway scaling and per-gateway resource limits; different teams could own different integrations | Adds a network hop inside the 8 s gateway budget and, worse, a second place a `TIMEOUT_UNKNOWN` can originate — we would now have to reconcile ambiguity between *us and our own service* as well as between us and the gateway, doubling the ambiguity surface for zero business benefit; every capability query becomes an RPC or a cache; the attempt row must still be written by the orchestrator before dispatch (ADR-012), so the transactional boundary does not move and the split buys no correctness; N services × pipelines, dashboards, on-call. This is the option a service-oriented engineer pushes for, and the isolation argument is genuine — it loses on ambiguity amplification | Rejected |
| **Plugin system with dynamic loading (Go plugins / WASM / scripting)** | Add a gateway without redeploying; third parties could contribute adapters; hot-swap during an incident | Go's `plugin` package requires byte-identical toolchain and dependency versions and is effectively unusable in a container pipeline; WASM would need the entire HTTP client, crypto and TLS surface marshalled across the boundary and loses the type system at exactly the seam where LSP violations are financial bugs; dynamically loaded code cannot be covered by the compile-time capability cross-check or by the contract suite in CI; unsigned third-party code executing inside the money path is a supply-chain risk we will not take. "Deploy to add a gateway" is not a problem worth this | Rejected |
| **No abstraction — a switch on gateway ID inside the orchestrator** | Fewest moving parts; no interface design to get wrong; the most direct code | Every new gateway edits the highest-risk file in the repository; the domain becomes untestable without gateway knowledge; capability checks scatter into conditionals that routing cannot introspect; violates §3's ACL requirement outright | Rejected |
| **Adopt a third-party orchestration abstraction (e.g. an open-source gateway abstraction library)** | Someone else maintains the adapters; faster initial integration | Their normalization decisions become our domain model, including their decline taxonomy and their treatment of ambiguity — and ADR-013 shows that the treatment of ambiguity is the single most important behaviour in this system; we would inherit a `DECLINED`-on-timeout mapping we could not change; capability descriptors and the contract suite are exactly the parts we are unwilling to outsource | Rejected |

## Consequences

### Positive

- `internal/domain` compiles and tests with zero gateway packages imported — mechanically checked
  by the §4 architecture rules.
- Routing can exclude an ineligible gateway as a hard filter (ADR-015) rather than discovering the
  mismatch after an 8 s dispatch.
- One contract suite is the definition of "correctly integrated". A new adapter is
  reviewed against an executable specification rather than against reviewer memory.
- The simulator implements the same SPI, so integration and chaos tests exercise the real code
  path with deterministic failure injection.
- The startup cross-check turns a whole class of "capability lied" bugs into a failed rollout.

### Negative

- The SPI is a shared abstraction: a genuinely new concept (say, a gateway that requires
  multi-step authorization with an out-of-band step) may force an SPI change touching every
  adapter. We accept periodic SPI versioning as the cost.
- All adapters share the orchestrator process. We mitigate with per-gateway bulkheads and circuit
  breakers (§10, §12 stage 14) but cannot fully isolate memory or goroutine misbehaviour.
- Adding a gateway requires a `payment-orchestrator` deploy — a money-path change with the
  associated ceremony.
- Descriptor accuracy is a maintenance obligation: a gateway that quietly changes its refund
  window makes our descriptor wrong, and the router will keep selecting it.

### Neutral / accepted costs

- Normalization loses information. `Raw` keeps an allowlisted subset for diagnostics; anything not
  on the allowlist is gone by design, and occasionally that will make a support investigation
  harder. That is the price of §17's logging discipline.
- Some gateway-specific behaviour necessarily lives in configuration rather than code
  (e.g. per-gateway TRA fraud-rate bands, §6 risk), which spreads gateway knowledge across two
  places.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| An adapter maps a timeout to `DECLINED` | Medium | **Critical** — corrupts failover, risks double charge | Contract suite clause with an explicit test injecting transport failure; code review checklist; the simulator exercises every transport failure class | Contract suite failure in CI; `pp_gateway_errors_total{class}` distribution showing implausibly few timeouts for a gateway |
| Descriptor drifts from reality | High over time | High — routing selects a gateway that will reject | L3 validation runs a scheduled capability probe against each gateway (§21); descriptor changes are versioned and audited; certification (§11.4) re-verifies the matrix per merchant | L3 probe failures; rising `NO_ELIGIBLE_GATEWAY` or post-dispatch capability rejections |
| Descriptor claims a capability the adapter lacks | Low | High | Startup cross-check by type assertion; the pod fails readiness rather than serving | Readiness failure at rollout |
| SPI over-generalises into a lowest-common-denominator | Medium | Medium — we cannot use a gateway's differentiating features | Optional capability interfaces (ISP) rather than a fat core interface; new capabilities are added as new optional interfaces, never as core methods | Frequency of core-interface changes; count of adapters implementing an interface (an interface with one implementer is a smell) |
| Adapter misbehaviour affects co-located adapters | Medium | Medium | Per-gateway bulkhead (bounded concurrency) and circuit breaker (§10); hard 8 s context deadline on every call; memory limits per pod | `pp_circuit_breaker_state`; goroutine count per gateway; pod OOM events |
| Raw response leaks sensitive data into logs | Low | **Critical** — PCI scope | `Raw` is an allowlisted `map[string]string` populated by the adapter, never the response body; §17 logging allowlist; linter forbids `%+v` on response types | PAN-detector on log samples; SAST rule |

## Validation

- **Domain purity:** `scripts/check-architecture.sh` asserts zero imports from
  `internal/adapters/gateway/**` into `internal/domain/**` or `internal/application/**` (other
  than the port).
- **Contract suite completeness:** a registry test asserts every registered adapter runs the full
  contract suite. An adapter cannot be registered without it.
- **Time-to-integrate:** the target for a fourth gateway is ≤ 3 engineer-weeks from start to a
  passing certification matrix (§11.4), with **zero** lines changed in `internal/domain` and
  `internal/application`. Measured on the next integration; a diff touching those packages is the
  falsification of this ADR.
- **Pre-dispatch capability accuracy:** count of attempts that fail at stage 14/15 for a reason
  the descriptor should have predicted. Target: zero. Any occurrence is a descriptor defect.

## Revisit criteria

Reopen if:

1. The gateway count exceeds ~8 and per-gateway resource contention inside the orchestrator
   becomes measurable — the answer is likely sharded orchestrator deployments per gateway group
   (same code, different routing of pods), not a service split.
2. A gateway we must support has no `Lookup`-equivalent API, forcing us to either reject the
   integration or weaken ADR-013's reconciliation guarantee. That is a genuine architectural
   conflict and needs an explicit decision, not a workaround.
3. Core SPI changes exceed ~2 per year, indicating the abstraction is not tracking reality.
4. A regulatory or contractual requirement demands per-gateway process or account isolation.
