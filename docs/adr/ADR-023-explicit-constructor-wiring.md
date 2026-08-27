# ADR-023: Explicit constructor wiring in composition roots — no DI framework, no wiring codegen

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §4 (layering and the dependency rule), §5 (deployable units), §25 (repository layout — `cmd/*` composition roots only) of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Related to ADR-001 (Go), ADR-006 (planes), ADR-024 (monorepo)

## Context

Nine binaries (§5) each assemble a different subset of the same components: repositories, ports,
adapters, validation rules, the event registry, telemetry, health checks. `payment-orchestrator`
needs gateway adapters and the payment repository; `control-plane-api` needs neither.
`cmd/*` is defined as composition roots only — no business logic (§25, §4).

The forces:

1. **Wiring is the one place where the architecture is visible in code.** §4's dependency rule
   says the domain imports nothing, the application depends on ports, and adapters implement them.
   The composition root is where those abstractions are bound to concrete implementations. If that
   binding is hidden by a framework, the architecture is documented but not readable.
2. **Startup failures must be loud and early.** A missing dependency should fail to compile, or at
   worst fail at process start with a precise message — never at 03:00 when a code path is first
   exercised with a nil interface.
3. **Ordering matters and is not always inferable.** Telemetry must initialise before anything
   that emits spans; the config snapshot must load before readiness reports true; graceful
   shutdown must close in reverse dependency order or in-flight payments are dropped. These are
   sequencing decisions, not just object-graph decisions.
4. **Go has no runtime DI conventions of the kind Java or C# has.** Reflection-based containers
   fight the language rather than fitting it, and the ecosystem norm is explicit construction.
5. **Debuggability during incidents.** "Where does this repository come from?" must be answerable
   by reading one file and following a variable, not by reasoning about a provider graph or
   reading generated code.
6. **Nine roots means real duplication.** Roughly 100–300 lines each of similar-looking
   construction. That is the honest cost of this decision and the strongest argument against it.

What breaks if we choose wrong: a nil interface discovered in production; or an architecture whose
seams are invisible; or hundreds of lines of near-duplicate wiring that drift apart.

## Decision

**Every deployable is assembled by explicit constructor calls in its own `cmd/<binary>/main.go`
(and a small `wire.go`-style helper file if the root grows). No dependency-injection framework, no
reflection-based container, no code generation for wiring, no service locator, and no
package-level `init()` side-effect registration.**

1. **Each `cmd/*` root** constructs, in order: configuration → telemetry → clock and ID generator
   → infrastructure clients (Postgres, Redis, Kafka, secrets) → repositories → adapters →
   validation registry → application services → transport handlers → health/readiness → the
   lifecycle manager.
2. **Everything is an interface parameter at the application layer.** Constructors take their
   dependencies; nothing reaches into a global. There is no package-level mutable state holding a
   database handle.
3. **Registration is explicit, not implicit.** Gateway adapters (ADR-011), validation rules
   (ADR-016), event consumers and Kafka handlers are registered by an explicit call in the root.
   `init()` may not register anything — a component's presence must be visible in the root's text.
4. **Shutdown is explicit and reverse-ordered.** A lifecycle manager holds an ordered list of
   closers; shutdown drains HTTP/gRPC, then workers, then flushes the outbox relay, then closes
   clients, then telemetry. The order is written down in one place.
5. **Shared construction lives in small, explicit helper functions**, not in a framework:
   `infrastructure.NewPostgres(cfg)` and similar. Helpers return concrete values and take explicit
   config; they never register anything globally.
6. **Startup validates the graph.** Each root runs a self-check before reporting ready: required
   adapters present, capability descriptors matching implementations (ADR-011), every registered
   rule documented (ADR-016), every required config key set. Failure exits non-zero with a
   specific message.
7. **`cmd/*` contains no business logic** — enforced by the architecture check (§4).

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Explicit constructor wiring (chosen)** | The object graph is readable Go in one file per binary — the architecture is legible, not just documented; missing dependencies are compile errors; initialisation and shutdown ordering are explicit, which matters because both are correctness concerns here; no build-time or runtime magic to debug during an incident; stack traces point at real code; new engineers can follow it without learning a framework | ~100–300 lines of wiring per binary × 9 binaries, with visible near-duplication; adding a dependency to a widely-used component means editing several roots; the roots grow over time and need deliberate refactoring into helper functions to stay readable | **Accepted** |
| **`google/wire` (compile-time DI codegen)** | Generates the wiring, so no hand-written duplication; still compile-time-checked, so missing providers are build errors; generated code is readable Go; well-regarded in the Go community and specifically designed for this problem | Provider sets become their own abstraction layer to learn and debug — when generation fails, the error messages are about the provider graph rather than about the code you wrote; the generated file is committed and reviewed but never hand-edited, so it is a second source of truth; it solves duplication but not ordering, and ordering is the part that actually causes production incidents here; it adds a code-generation step to every build and a regeneration discipline to every dependency change. This is the strongest rejected option and a reasonable engineer would push for it on the duplication argument — it loses because the duplication it removes is boring and the complexity it adds is not | Rejected |
| **`uber-go/fx` (runtime DI with lifecycle management)** | Handles both wiring and lifecycle (start/stop hooks with ordering), which is genuinely valuable; mature, well-documented; removes almost all wiring code; dependency graph is introspectable | Resolution is at runtime via reflection, so a missing or ambiguous dependency is a **startup panic**, not a compile error — and in a nine-binary fleet, a missing provider in one rarely-deployed binary is exactly the failure that ships; stack traces pass through framework internals, which is worse precisely during incidents; the object graph becomes invisible in source, so the architectural seams §4 defines are no longer readable; it imposes its own idioms on every constructor. The lifecycle management is the real draw, and we get most of it from a ~50-line ordered-closer list | Rejected |
| **Service locator / global registry (`container.Get[T]()`)** | Minimal wiring code; any component can fetch what it needs; easy to add a dependency without touching a root | Dependencies become invisible at the type level: a constructor's signature no longer tells you what it needs, so the compiler cannot help and tests cannot know what to stub; every component becomes coupled to the container; it is the anti-pattern that makes §4's dependency rule unenforceable, because any package can reach anything at runtime | Rejected |
| **Package-level singletons initialised in `init()`** | Least code of all; components are just available | Initialisation order across packages is compiler-determined and effectively unpredictable; testing requires mutating globals, so tests interfere with each other and cannot run in parallel; a component's presence in a binary becomes invisible; multi-tenant configuration cannot be expressed at all | Rejected |

## Consequences

### Positive

- The composition root is a readable map of each binary: what it contains, in what order it
  starts, and in what order it stops. During an incident that file answers "what is running here?"
  in seconds.
- Missing dependencies are compile errors. There is no path from "forgot to provide X" to a nil
  interface at 03:00.
- Startup and shutdown ordering — both correctness concerns for in-flight payments and for the
  outbox relay — are explicit and reviewable.
- Testing is trivial: construct the component under test with test doubles. No container, no
  framework, no global reset between tests, and tests run in parallel safely.
- Zero build-time or runtime magic; stack traces are real.

### Negative

- Real duplication across nine roots. This is the cost and it is not small: a change to a widely
  used constructor's signature touches several files.
- Roots grow. Without deliberate maintenance they become long and less readable — the very
  property we chose them for.
- Adding a dependency deep in a component means threading it through constructors, which is more
  keystrokes than a framework would need.

### Neutral / accepted costs

- Ordering must be maintained by hand and is not checked by the compiler. The startup self-check
  catches missing components; it does not catch a subtly wrong order, so shutdown ordering in
  particular needs an integration test.
- Engineers arriving from Java/Spring or from `fx` codebases will find this verbose. That is a
  documentation and onboarding matter, not a design problem.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| Composition roots drift apart (different binaries wire the same component differently) | Medium | Medium — inconsistent behaviour between binaries | Shared construction in explicit helper functions with a single signature per component; a test asserts that common components are constructed through the shared helper in every root | Code review; a lint over `cmd/*` for direct construction of components that have a helper |
| Roots grow unreadable | High over time | Medium | Extract into named phase functions (`newInfrastructure`, `newAdapters`, `newHandlers`) once a root exceeds ~200 lines; keep them in `cmd/<binary>/` so the map stays local | Root file length tracked in CI |
| Wrong shutdown order drops in-flight payments | Medium | **High** | Lifecycle manager with an ordered closer list defined in one place per root; integration test issues in-flight payments, sends `SIGTERM`, and asserts every accepted request completes and the outbox drains | Graceful-shutdown integration test; count of requests failed during rolling deploys |
| A component silently missing from a binary | Low | High | Startup self-check asserts required components are present and consistent (capability descriptors vs implementations, rules vs documentation); non-zero exit on failure, so the rollout fails rather than the payment | Readiness failure at rollout; self-check error message |
| `init()` registration creeps back in | Medium | Medium — invisible components | Lint forbids non-trivial `init()` in `internal/**`; registries take explicit `Register(...)` calls from the root | Lint gate; code review |
| Business logic leaks into `cmd/*` | Medium | Medium | Architecture check enforces §4: `cmd/**` may import anything but must contain composition only; enforced by a size/complexity heuristic plus review | Architecture check; cyclomatic complexity of `cmd/*` files |
| Duplication cost grows past the tipping point as binaries are added | Medium | Medium | The revisit criteria below name the threshold explicitly; `wire` remains a mechanical migration from this state, which is not true in reverse | Aggregate wiring LOC across `cmd/*` |

## Validation

- **Compile-time check:** removing a required dependency from a constructor call must fail the
  build. Trivially true by construction; stated so that any future change preserving "wiring
  compiles with a missing dependency" is recognised as a regression.
- **Startup self-check:** each binary refuses to report ready if its component graph is incomplete
  or inconsistent. Asserted in `tests/integration` by deliberately omitting a required adapter.
- **Graceful shutdown test:** `SIGTERM` during sustained load; assert zero accepted-but-unfinished
  payment requests, the outbox drained, and no dropped spans.
- **Wiring volume:** aggregate LOC in `cmd/*` tracked in CI. Crossing ~2 500 lines total is the
  review trigger named below, not an automatic failure.
- **Onboarding signal:** a new engineer should be able to describe what `payment-orchestrator`
  contains, by reading its root, in under 15 minutes. Checked informally at onboarding and treated
  as evidence about readability.

## Revisit criteria

Reopen if:

1. Aggregate wiring in `cmd/*` exceeds ~2 500 lines, or the same component is constructed
   identically in six or more roots — at that point `wire` becomes the leading candidate, because
   it preserves compile-time checking and the migration is mechanical.
2. Binary count grows beyond ~15.
3. A genuine need for runtime-dynamic component graphs appears (e.g. per-tenant component
   variants selected at runtime) that explicit wiring cannot express cleanly.
4. Ordering bugs cause more than one production incident — that would be evidence that manual
   ordering is not sustainable and that `fx`-style lifecycle management is worth its costs.
