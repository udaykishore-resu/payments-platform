# 06 — Code Conventions

> Binding for every Go file in this repository. Derived from `00-design-baseline.md` §4 and §25.

## Module and layout

- Module path: `github.com/udaykishore-resu/payments-platform`. Single Go module, monorepo (ADR-024).
- Layer boundaries and the import rule: baseline §4. Enforced by `scripts/check-architecture.sh` in CI.
  - `internal/domain/**` may import only stdlib, other `internal/domain/**`, and `pkg/**`.
  - `internal/application/**` may import stdlib, domain, and `internal/application/ports`. Never infrastructure.
  - `pkg/**` may import only the standard library.

## Reference implementations

Read these before writing new code; they establish the patterns everything else follows.

| Concern | Reference |
|---|---|
| Value object, no floats, allocation | `pkg/money/money.go` |
| Identifiers, prefixes, partition keys | `pkg/ids/ids.go` |
| Error model, codes, categories, retryability | `pkg/apierror/apierror.go` |
| Declarative state machines | `internal/domain/shared/fsm.go` |
| Typed IDs, clock, country, payment method | `internal/domain/shared/` |
| Aggregate design, invariants, events, rehydration | `internal/domain/payment/` |
| Lifecycle FSM driven by an external workflow | `internal/domain/merchant/` |

## Rules

1. **Aggregate fields are unexported.** Read access through accessors; write access only through
   methods that check the transition table and the invariants. A repository reconstructs an
   aggregate through an explicit `Rehydrate(RehydrateParams)` function that validates the
   persisted state is one this binary understands.
2. **State machines are tables**, built with `shared.NewStateMachine`. Never scattered `if`
   statements. Self-transitions must be declared explicitly if legal.
3. **Errors are `*apierror.Error`.** Use the registered codes. Add a `Detail` with a stable
   `RuleID` whenever a caller could plausibly fix the input. Never return a bare `errors.New`
   from a domain method.
4. **No floats in the money path.** `money.Money` only.
5. **Time comes from `shared.Clock`**, never `time.Now()` inside domain or application code.
   All timestamps are UTC.
6. **Domain events are plain structs** raised into a `[]Event` on the aggregate and drained by
   the repository inside the state-change transaction. The domain knows nothing about
   CloudEvents, Kafka or JSON.
7. **Every exported symbol has a doc comment** that says *why*, not just *what*. A comment that
   restates the signature is noise; a comment that explains the failure mode the code prevents
   is the reason the file is worth reading.
8. **Comments explain trade-offs at the decision point.** Where a non-obvious choice was made,
   say what the alternative was and what it would have cost.
9. **No panics outside programming errors** detectable at init or first test run. Never panic in
   a request path.
10. **Constructors validate.** An aggregate that exists is a valid aggregate.
11. **Copy slices and maps** on the way in and out of aggregates. Returning the live backing
    array lets a caller mutate state without going through a method.
12. **`context.Context` is the first parameter** of every function that does I/O, and is
    propagated, never stored in a struct.
13. **Interfaces are declared by the consumer**, in `internal/application/ports`, not by the
    implementation. Keep them narrow — one to four methods.
14. **Table-driven tests**, `t.Parallel()` where safe, deterministic clocks and IDs, no sleeps.
    Every state machine gets the exhaustive property test: for every (from, to) pair in the
    state universe, the machine accepts exactly the pairs in its table.
15. **No secrets, PAN, CVV or credentials in logs.** Credential material is carried in a
    redacting wrapper type, never a bare string.

## Naming

- Packages: singular, lowercase, no underscores (`payment`, not `payments` or `payment_domain`).
- Files: `<concept>.go`, tests `<concept>_test.go`, build-tagged integration tests
  `<concept>_integration_test.go` with `//go:build integration`.
- Event type constants: `Event<Aggregate><PastTenseVerb>`, value `"<aggregate>.<verb>.v1"`.
- Rule IDs: `L<level>.<SCREAMING_SNAKE_DESCRIPTION>`.
- Metrics: `pp_<subsystem>_<name>_<unit>`, per baseline §22.2.

## Verification a change must pass

```
gofmt -l .            # must be empty
go vet ./...
go build ./...
go test ./... -race
golangci-lint run
scripts/check-architecture.sh
```
