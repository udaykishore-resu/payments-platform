# Testing Strategy

> **Purpose:** the test pyramid as applied to this platform, what each level asserts, the named failure-scenario tests, the data and flakiness policies, and the exact commands to run everything locally and in CI.
> **Derived from and subordinate to [`docs/spec/00-design-baseline.md`](spec/00-design-baseline.md) §24 (failure catalog), §9 (payment FSM and invariants), §8 (merchant FSM), §14 (idempotency), §11.4 (certification), §16.2 (isolation guard), §17.2 (redaction), §18 (NFR targets), §21 (validation levels), §26 (traceability).** Where this document disagrees with the baseline, the baseline wins and this document is a defect.

---

## 0. What this test suite is for

Three claims, and every test below exists to support one of them:

1. **Money cannot move twice.** Invariant I3, the idempotency contract (§14), the never-retry-an-unknown-outcome rule (§12.3), and the database constraints that back them.
2. **State cannot become impossible.** The payment and merchant FSMs (§8, §9) accept exactly the documented transitions and nothing else, under concurrency, after crashes, and after replay.
3. **A tenant cannot see another tenant's data.** The isolation guard (§16.2), enforced at the application layer *and* by RLS, with a negative test that proves the database layer alone would stop it.

A test that supports none of these still has to justify its maintenance cost. That framing is what keeps the suite from becoming an undifferentiated mass of assertions nobody trusts.

---

## 1. The pyramid, the proportions, and the coverage argument

Counted as **top-level `func Test…` declarations**, which is the only definition that can be
re-derived from the tree; each is worth several subtests. Measure with
`grep -rh '^func Test' --include='*_test.go' .` and, per tag,
`grep -rl '^//go:build <tag>' --include='*_test.go' . | xargs grep -h '^func Test'`.

| Level | Build tag | Tests | Share | Runs on | Dependencies |
|---|---|---:|---:|---|---|
| Unit and in-package (pure domain, application with fakes, infrastructure with fakes) | none | 1 179 in 138 files | 92.8 % | every push, `go test ./...` | none |
| Integration (real Postgres, Redis, Kafka) | `integration` | 66 in 18 files | 5.2 % | every PR | a running Postgres/Redis/Kafka named by `PP_TEST_*`; **skips** otherwise |
| Chaos (fault injection, crash, partition, clock skew, retry storm) | `chaos` | 20 in 6 files | 1.6 % | nightly + pre-release | in-process port decorators always; the destructive half needs `PP_TEST_CHAOS_INFRA=1` |
| E2E (black box, through the HTTP edge) | `e2e` | 6 in 4 files | 0.5 % | merge to `main` | a running stack plus a bearer token |
| **Total** | | **1 271** | | | |
| Load (k6) | n/a (JavaScript) | 4 scenarios | — | nightly / pre-release | a deployed target |

Where those 1 271 sit, by area — the shape matters more than the total:

| Area | Tests |
|---|---:|
| `internal/infrastructure` (Postgres, Redis, Kafka, secrets, telemetry, resilience, crypto) | 398 |
| `internal/domain` | 252 |
| `internal/platform` (idempotency, tenantctx, authn/authz, config, runtime, secret) | 135 |
| `internal/application` | 131 |
| `internal/workflows` | 74 |
| `internal/transport` | 53 |
| `internal/events`, `internal/policies`, `internal/composition` | 52 |
| `internal/validation` | 51 |
| `internal/adapters/gateway` | 40 |
| `tests/` (integration, chaos, e2e, contract) | 61 |
| `cmd/` | 22 |
| `scripts/` (the arch checker and the dev issuer) | 2 |
| `pkg/` (`apierror`, `ids`, `money`) | **0** |

The shape is conventional; the reasoning for *this* system is not. The domain layer is stdlib-only by construction (§4), which makes the FSMs, `Money`, the validation rules, the routing policy evaluation and the idempotency fingerprinting all testable with zero infrastructure. That is a deliberate architectural choice made *so that* the base of the pyramid can be wide and fast. Where the domain is pure, unit tests are cheap enough to be exhaustive; where it is not, no amount of mocking makes an integration test unnecessary.

Two honest caveats about that table. **`pkg/` has no tests at all** — `Money` is the type baseline
§7 says is "enforced by the type system and by tests", and the tests are the half that is missing;
it is exercised only through its callers. And the 1 179 untagged tests are the only ones a green
`go test ./...` says anything about: the other 92 skip with an explanatory message unless the
services they need are pointed at by environment variables.

**The suite uses the standard library only.** There is no `testify`, no `rapid`, no
`testcontainers` and no `go-vcr` in `go.mod` — assertions are `if got != want { t.Errorf(…) }`,
property tests are exhaustive loops over enumerable state spaces rather than generated samples,
and the integration suites connect to services that are already running rather than starting
containers themselves. That is a deliberate trade: a test dependency is a dependency the
production module carries in its graph, and `scripts/check-licences.sh` gates that graph.

### 1.1 Coverage gates

Enforced by [`scripts/coverage.sh`](../scripts/coverage.sh), the `coverage` stage of
[`scripts/verify.sh`](../scripts/verify.sh), and by `make cover`, which passes it the profile it
has just produced so the suite runs once.

The script compares each scope against **two** numbers, and the difference between them is the
honest part of this section:

- **floor** — what the tree achieves today, minus a point of slack. Dropping below it **fails**
  the build. This is the last row of the original table ("coverage may not decrease") turned into
  a property rather than an intention, and it does not need a comparison against `main` to work.
- **target** — the gate this document has always stated. Being below it **warns**, and prints the
  distance, on every run.

| Scope | Target | Floor | Measured (`go test ./... -short`) |
|---|---:|---:|---:|
| `internal/domain/**` | 95 % | 79 % | 80.5 % |
| `internal/application/**` | 90 % | 51 % | 52.5 % |
| `internal/validation/**` | 95 % | 79 % | 80.3 % |
| `internal/platform/idempotency` | 100 % | 86 % | 87.3 % |
| `internal/platform/tenantctx` | 100 % | 96 % | 97.0 % |
| `internal/workflows/**` | 85 % | 56 % | 57.7 % |
| `internal/adapters/gateway/**` | 80 % + the full contract suite | 52 % | 54.0 % |
| `internal/infrastructure/**` | 70 % | 52 % | 53.2 % |
| Repository overall | 80 % | 56 % | 57.9 % |

**Every scope is below its target**, several by a long way, and the table says so rather than
implying a gate that passes. `scripts/coverage.sh --enforce-targets` treats the target column as
the failure threshold; it currently fails, and it is the switch that turns the aspiration into a
gate on the day the tree earns it.

Two things distort these numbers downward and are worth knowing before reading them as a verdict.
The measurement is `go test ./... -short`: no build tags, so the 92 integration, chaos and e2e
tests contribute nothing, and a package whose behaviour is covered only by an integration test
reads as uncovered. And packages with no test file of their own — `internal/domain/shared`,
`internal/transport/httpapi`, all of `pkg/`, every `cmd/` composition root — count their
statements in the denominator at 0 %. That is the right default: coverage that only appears when
a container is running is not coverage a developer's laptop can rely on.

Individual packages that matter most, for contrast: `internal/domain/payment` is at **97.2 %**
and `internal/domain/merchant` at **99.3 %**.

### 1.2 Why coverage percentage is a bad gate, and what is used instead

Coverage measures which lines executed. It says nothing about whether anything was asserted. A test that calls every method and asserts `err == nil` yields 100 % coverage and detects approximately nothing. Worse, a coverage gate creates pressure to write exactly that kind of test — the metric becomes the target, and the suite fills with executions masquerading as verifications.

The gates above are therefore a **floor against neglect**, not a measure of quality. The actual quality gate is a list of **critical-path assertions**: named, individually-required properties, each traced to a baseline section, each of which must be covered by a test. It is registered in [`tests/critical_paths.yaml`](../tests/critical_paths.yaml) — **36 entries** — and checked by the second stage of [`scripts/coverage.sh`](../scripts/coverage.sh).

```yaml
# tests/critical_paths.yaml  (excerpt — 36 entries)
  - id: CP-01
    property: "A second successful attempt for one payment is refused by the database, with the domain bypassed"
    baseline: "§9 I3"
    needs: integration
    tests:
      - "internal/infrastructure/postgres/invariants_integration_test.go::TestI3RejectsSecondSuccessfulAttempt"
      - "tests/integration/invariants_test.go::TestI3HoldsWhenTwoAttemptsSucceedConcurrently"

  - id: CP-02
    property: "A timed-out gateway call never fails the payment and is never retried automatically"
    baseline: "§12.3, §1.3 A7, ADR-013"
    needs: chaos
    tests:
      - "tests/chaos/gateway_test.go::TestGatewayTimeoutLeavesPaymentProcessingAndNeverRetries"
      - "internal/domain/payment/payment_test.go::TestMarkFailedRespectsTheTransitionTable"
      - "internal/domain/payment/state_test.go::TestAttemptOutcomeFailoverAndReconciliation"

  - id: CP-25
    property: "A statement with no tenant in the session sees nothing, and never returns rows"
    baseline: "§16.2"
    needs: integration
    tests:
      - "internal/infrastructure/postgres/rls_integration_test.go::TestQueryWithoutTenantContextReturnsNoRows"
      - "tests/integration/tenant_isolation_test.go::TestAStatementWithNoTenantInTheSessionSeesNothing"
```

**What is checked.** `scripts/coverage.sh` asserts that every `tests:` entry resolves: the file
exists and declares a top-level `func <Name>(`. A renamed or deleted test therefore fails the
build naming the critical-path ID that lost its evidence, instead of silently leaving a property
unasserted — which is precisely the failure mode a coverage percentage cannot see. All 36 entries
and their 70 test references are checked on every `make verify`, in well under a second, because
the check reads source rather than running anything.

```
$ make critical-paths
==> critical paths — every registered property still has its test (docs/testing.md §1.2)
    36 critical path(s), 70 test reference(s) checked
✓ critical paths: every registered property still names a test that exists
```

**What is not checked, and would be the next thing worth building.** That the named test still
*asserts* the property. The technique is mutation-adjacent probing: each critical path would
declare named `mutation_probes` — scripted breakages applied to a scratch copy of the tree
(`drop_partial_unique_index`, `timeout_maps_to_FAILED`, `check_uses_gte`) — and a runner would
apply each probe, run *only* the named tests, and require them to **fail**. A probe whose tests
still pass is a property nobody is asserting, and it would fail CI with the exact critical-path
ID.

**This is not implemented.**
There is no `scripts/mutation-probe.sh` in this repository and no `mutation_probes` key in the registry. <!-- doc-refs: allow-missing -->
What it would take: a way to apply and revert a probe
against a scratch worktree (a patch series, or a build-tag-selected variant of the file under
test), a runner that maps a `path::TestName` reference to `go test -run`, and a decision about
what to do with probes that need a live database — the ones that matter most, `drop_partial_unique_index`
among them, are schema mutations rather than code mutations, so they need the integration
services running and cannot go in the PR tier. Until that exists, the registry proves that the
evidence *exists*, not that it *works*; §1.2's argument stands but its second half is unbuilt.

---

## 2. Unit tests

Location: alongside the code (`internal/**/*_test.go`). Build tag: none. No network, no filesystem, no clock, no randomness.

### 2.1 Pure domain

Table-driven, standard library only. The real shape, from
`internal/domain/payment/state_test.go`:

```go
func TestPaymentMachineRefusesTheTransitionsTheBaselineNames(t *testing.T) {
    t.Parallel()
    tests := []struct {
        from, to State
        why      string
    }{
        {StateSettled, StateProcessing,
            "would re-dispatch a payment whose funds have already moved to the merchant"},
        {StateRefunded, StateCaptured,
            "would recreate captured funds that have already been returned to the cardholder"},
        {StateCaptured, StateAuthorized,
            "un-capturing is not an operation any gateway offers"},
        {StateFailed, StateProcessing,
            "FAILED means we told the merchant no; any exit re-opens a declined payment"},
        {StateCreated, StateCaptured,
            "must pass through PROCESSING, where the attempt row is written before the call"},
    }
    for _, tc := range tests {
        t.Run(tc.from.String()+"_to_"+tc.to.String(), func(t *testing.T) {
            t.Parallel()
            if Machine().CanTransition(tc.from, tc.to) {
                t.Fatalf("%s → %s is permitted: %s", tc.from, tc.to, tc.why)
            }
        })
    }
}
```

The `why` field is not decoration. A forbidden-transition test whose failure message is
`false != true` tells the next reader nothing about whether the transition should be re-allowed;
one that says "would recreate captured funds that have already been returned to the cardholder"
answers the question the reader is actually asking.

The aggregate-level counterpart asserts the thing that matters more than the state change: a
**rejected** transition must leave the aggregate entirely unchanged, including its pending-event
list and its version. An FSM that mutates and then errors is the source of the "impossible state
in production" class of bug, and `internal/domain/payment/payment_test.go` checks the no-partial-
mutation property on every refusal path.

### 2.2 Exhaustive tests for the FSMs

**Exhaustive, not sampled, and not generated.** Every state space in this platform is small
enough to enumerate — the largest is the merchant's 441 ordered pairs — so there is no reason to
draw samples from it and hope. There is no property-testing library in `go.mod`; a `rapid`-style
generator would explore a subset of a space the loop below covers completely, at the cost of a
dependency and a reproducibility story.

The shape, and the detail that makes it worth anything, is in
[`docs/state-machines.md`](state-machines.md) §16.2: the expected edge table in the test is
**hand-written**, not derived from `Machine().Edges()`. An expectation computed from the code
under test passes for any table, including one with `SETTLED → PROCESSING` in it. The map in the
test and the literal in `state.go` are two independent transcriptions of the same specification,
and the test is where they are compared.

Ten machines are covered this way. Eight of them — merchant, payment, attempt, refund, tenant,
API client, gateway health, gateway connection — have an independent expected table. The two
workflow machines derive their expectation from the machine itself and are weaker for it;
`docs/state-machines.md` §16.2 says so in the same table.

### 2.3 `Money`

`Money` (baseline §7) is the type a property-based test would earn its keep on, because the
failure modes are arithmetic: largest-remainder allocation that loses a minor unit, a currency
mismatch silently coerced, an overflow that wraps instead of erroring, a serialization that emits
a decimal point.

**`pkg/money` has no test file.** Baseline §7 says the rules are "enforced by the type system and
by tests"; the type system half is real — `Money` carries its currency, the constructor validates,
and arithmetic returns an error rather than panicking — and the tests half is missing. `Money` is
exercised only indirectly, through the payment aggregate's refund and capture arithmetic
(`internal/domain/payment`, 97.2 % covered) and through the L5 amount rules. The properties worth
asserting directly, listed here so that writing them is a matter of doing it rather than deciding
what to do:

| Property | Baseline |
|---|---|
| Addition is commutative and associative; zero is the identity; `a − a = 0` | §7 |
| A currency mismatch is an error, never a panic and never an implicit conversion | §7 rule 3 |
| `Allocate` distributes by largest remainder: the parts always sum to the whole, and no two parts differ by more than one minor unit | §7 rule 4 |
| JSON round-trips exactly, as an integer, with no decimal point anywhere in the encoding | §7 rule 5 |
| Overflow returns `ErrAmountOverflow` rather than wrapping | §7 |

The allocation property is the one that catches real bugs: a fee split of `1000` across `3` that
produces `333/333/333` loses a cent per transaction, which becomes a reconciliation exception a
week later and an unexplained ledger imbalance a month after that.

### 2.4 Application layer

Use cases are tested against **fakes**, not mocks: an in-memory repository implementing the real port with the real semantics (including optimistic-concurrency conflicts and unique-constraint violations), a controllable `Clock`, a deterministic `IDGenerator`, and a scriptable gateway port.

The distinction is load-bearing. A mock asserts "the repository's `Save` was called once with these arguments" — which passes even when `Save` would fail in reality. A fake actually stores, actually conflicts, actually rejects a duplicate. Tests written against fakes catch the concurrency bugs; tests written against mocks catch refactorings.

---

## 3. State machine tests

Every machine gets an **exhaustive transition matrix** test, not a sampled one. The payment FSM
has **14** states and the merchant FSM **21**; the full matrices are **196** and **441** ordered
pairs, which is trivially exhaustive — and the exhaustiveness is the point, because the value is
not in the 35 and 42 pairs someone thought about but in the 161 and 399 that must be refused.

The full per-machine coverage table, and the reason the expected edge tables are hand-written
rather than derived, are in [`docs/state-machines.md`](state-machines.md) §16.2. The counts:

| Machine | States | Legal | Pairs | Rejections asserted |
|---|---:|---:|---:|---:|
| Merchant | 21 | 42 | 441 | 399 |
| Payment | 14 | 35 | 196 | 161 |
| Gateway connection | 9 | 20 | 81 | 61 |
| Workflow step | 13 | 19 | 169 | 150 |
| Workflow instance | 11 | 21 | 121 | 100 |
| Payment attempt | 6 | 9 | 36 | 27 |
| Refund | 5 | 5 | 25 | 20 |
| Gateway health | 4 | 7 | 16 | 9 |
| Tenant | 3 | 4 | 9 | 5 |
| API client | 3 | 4 | 9 | 5 |
| **Total** | **89** | **166** | **1 103** | **937** |

**Guards are tested separately**, because a transition can be in the table and still be refused
because a guard fails. `PRODUCTION_READY → ACTIVE` is in the table; it is refused unless all four
clauses of the activation guard hold. The named guard tests:

| Guard | Test |
|---|---|
| A-01's compliance rejection has exactly three exits, and none is forward | `internal/domain/merchant/state_test.go::TestAmendmentA01ComplianceRejection` |
| Suspension permits money **out** but not money **in** | `internal/domain/merchant/state_test.go::TestCanAcceptPaymentsIsNarrowerThanCanIssueRefunds` |
| A suspension reason that needs operator review cannot be lifted automatically | `internal/domain/merchant/state_test.go::TestSuspensionReasonRequiresOperatorReviewToLift` |
| Certification cannot be recorded without a report reference (A11) | `internal/domain/gateway/connection_test.go::TestCertifyRequiresAReportID` |
| A capture above the authorization is refused before any state changes (I2) | `internal/domain/payment/payment_test.go::TestMarkFailedRespectsTheTransitionTable` and the I2 cases beside it |
| A refund reaches `SUCCEEDED` only through its payment | `internal/domain/payment/refund_test.go::TestARefundReachesSucceededOnlyThroughThePayment` |

**A structural test closes the loop at the database.**
`internal/infrastructure/postgres/migrations_test.go::TestTransitionTablesMatchDomain` parses the
`INSERT INTO pp.payment_state_transitions` and `INSERT INTO pp.merchant_status_transitions` seeds
out of `migrations/0013_state_guards.up.sql` and asserts they equal `payment.Machine().Edges()`
and `merchant.Machine().Edges()` exactly. A domain change not mirrored in the migration fails the
build rather than drifting into production.

What that test does **not** cover, and this document should not imply it does: the `CHECK (state
IN (…))` lists on the other machines' columns are compared with nothing, which is where the three
schema drifts recorded in `docs/state-machines.md` §7, §8 and §9 come from. Nor does anything
parse this document or the baseline; the tie between the Go table and the specification is the
hand-written expected table in each `state_test.go`, and a reviewer.

---

## 4. Integration tests

Build tag `integration`. Real Postgres, Redis and Kafka — **already running**, not started by the
suite. There is no `testcontainers` dependency in `go.mod`, and that is deliberate: it would put
a Docker client, its transitive graph and its licences into the production module's dependency
graph, which `scripts/check-licences.sh` gates, in exchange for starting containers a developer
has usually already started with `make dev-up`.

Dependencies are discovered through environment variables, and a suite whose variable is unset
**skips with a message naming the variable and the command that would supply it**. A test that
silently passes because a service was missing is worse than one that fails: it makes the suite's
green a statement about the runner's environment rather than about the system.

| Variable | Used for | Notes |
|---|---|---|
| `PP_TEST_POSTGRES_DSN` | the primary DSN for every Postgres suite | `PP_TEST_DATABASE_URL` is accepted as a fallback, because the older `internal/infrastructure/postgres` suite already used that name |
| `PP_TEST_POSTGRES_SCRATCH_DSN` | `migration_test.go` only | separate on purpose: that suite migrates all the way **down**, which destroys the schema, and doing that to the database the rest of the suite is using turns one failure into forty |
| `PP_TEST_REDIS_ADDR` | `host:port` for the idempotency accelerator | |
| `PP_TEST_KAFKA_BROKERS` | comma-separated broker list | |
| `PP_TEST_BASE_URL`, `PP_TEST_CONTROL_URL`, `PP_TEST_SIMULATOR_URL`, `PP_TEST_AUTH_TOKEN`, `PP_TEST_TENANT_ID` | the `e2e` suite | `PP_TEST_CONTROL_URL` defaults to `PP_TEST_BASE_URL`, because the local compose stack fronts both behind one gateway |
| `PP_TEST_CHAOS_INFRA` | opts the `chaos` suite into the scenarios that stop and start real infrastructure | without it only the in-process port-decorator scenarios run — the right default, since a nightly job may pause containers and a laptop may not |

`tests/testenv` is the shared harness and is **deliberately untagged**, so one compile of it
covers the integration, e2e, chaos and contract suites and a break in it is caught by the cheapest
CI stage rather than the slowest.

```go
//go:build integration

// tests/integration — three lines at the top of each test, not a TestMain.
// Process-wide state is what makes t.Parallel() unsafe, and t.Parallel() is what makes the
// isolation properties under test actually get exercised.
func setup(t *testing.T) (*pgxpool.Pool, *testenv.Scope) {
    t.Helper()
    pool := testenv.Postgres(t)          // skips if PP_TEST_POSTGRES_DSN is unset;
                                         // applies the REAL migrations, once per process
    testenv.RequireNonBypassRLS(t, pool)  // refuses to run as a role that would make
                                         // every negative assertion below vacuous
    return pool, testenv.Isolate(t, pool)  // per-test tenants and merchants; cleanup asserted
}
```

| Property | Choice | Reasoning |
|---|---|---|
| Service lifetime | One shared database per process; migrations applied once under `sync.Once` | Running the migrator per test would serialize the whole suite behind an advisory lock for no benefit |
| Pool | `pgxpool` directly, not the repository's `*postgres.Pool` | Half of what these suites assert is what the database does when the application layer is **not** in the way, and that means issuing SQL the repositories would never issue |
| Role | `RequireNonBypassRLS` fails the test if the connection can bypass RLS | A tenant-isolation test run as a superuser passes for the wrong reason |
| Isolation | Per-test deterministic tenant and merchant IDs derived from a per-test nonce; `Tenanted` runs work inside a transaction that is always rolled back | Every constraint under test is a commit-time behaviour, so the mode that needs a commit exists too |
| Migrations | The **real** `migrations/` directory, applied by the **real** runner | A schema built from a hand-written DDL file proves nothing about production, and this is how migration bugs are caught before deploy |
| Cleanup | `Isolate` snapshots the **shared** namespace before the test and asserts it is unchanged after | Catches a test that wrote outside its own tenant — a bug that would otherwise surface as a flake in an unrelated test three days later |

### 4.1 What integration tests assert that unit tests cannot

Every test named here exists; the path is where to find it.

| Area | Test |
|---|---|
| Constraint enforcement (I3) | `internal/infrastructure/postgres/invariants_integration_test.go::TestI3RejectsSecondSuccessfulAttempt` — insert directly, bypassing the domain, and assert the unique violation on the I3 partial index |
| I3 under contention | `tests/integration/invariants_test.go::TestI3HoldsWhenTwoAttemptsSucceedConcurrently` |
| Refund overdraft (I1) | `internal/infrastructure/postgres/invariants_integration_test.go::TestI1RejectsRefundExceedingCapture`; concurrently, `tests/integration/invariants_test.go::TestConcurrentPartialRefundsCannotExceedTheCapturedAmount` |
| Capture overdraft (I2) | `internal/infrastructure/postgres/invariants_integration_test.go::TestI2RejectsCaptureExceedingAuthorization` |
| Immutable fields (I4) | `internal/infrastructure/postgres/invariants_integration_test.go::TestI4ImmutableFieldsRejectedAtDatabase` |
| Illegal transition at the database | `internal/infrastructure/postgres/invariants_integration_test.go::TestDatabaseRefusesAnIllegalStateTransition` — the domain is not in the call path at all |
| Append-only ledger and audit | `internal/infrastructure/postgres/invariants_integration_test.go::TestLedgerAndAuditAreAppendOnly`, `::TestPaymentTablesRejectDelete` |
| PAN tripwire | `internal/infrastructure/postgres/invariants_integration_test.go::TestPANTripwireRejectsABareCardNumber` |
| Idempotency claim under contention | `tests/integration/idempotency_test.go::TestConcurrentIdenticalCreatesYieldExactlyOnePayment` (§1.3 A6) |
| Idempotency replay | `tests/integration/idempotency_test.go::TestReplayReturnsTheByteIdenticalStoredResponse` |
| Idempotency fingerprint | `tests/integration/idempotency_test.go::TestSameKeyWithADifferentBodyIsReportedAsReuse` (§14.2) |
| Lease reclaim | `tests/integration/idempotency_test.go::TestExpiredLeaseIsReclaimedExactlyOnce`, `::TestAnExpiredLeaseCannotBeReclaimedWithADifferentBody` |
| Outbox ordering across shards | `tests/integration/outbox_test.go::TestTwoRelayShardsPreservePerAggregateOrder` |
| Outbox failure handling | `tests/integration/outbox_test.go::TestAPublishFailureLeavesTheRowClaimable`, `::TestBacklogMetricReflectsReality` |
| Webhook dedup and ordering | `tests/integration/webhook_test.go::TestDuplicateWebhookIsDroppedByTheUniqueIndex`, `::TestAnOutOfOrderWebhookIsANoOp` |
| Webhook that cannot be resolved | `tests/integration/webhook_test.go::TestWebhookForAnUnknownPaymentOpensAReconciliationException` |
| Unverified webhook | `tests/integration/webhook_test.go::TestAWebhookThatFailedVerificationCannotReachAProcessingState` |
| Migrations apply, roll back and re-apply identically | `tests/integration/migration_test.go::TestMigrationsApplyRollBackAndReapplyIdentically` — against `PP_TEST_POSTGRES_SCRATCH_DSN`, because it migrates all the way down |
| Migration pairing and checksums | `tests/integration/migration_test.go::TestEveryMigrationHasABalancedPairAndADenseVersion`, `::TestMigrationChecksumsAreStableAcrossLoads` |
| Workflow resume | `tests/integration/workflow_resume_test.go::TestWorkerCrashAtEveryOnboardingStepResumesWithoutRepeatingWork` |
| One live onboarding per merchant | `tests/integration/workflow_resume_test.go::TestStartingOnboardingTwiceIsRefusedByTheDatabase` |
| Full lifecycle persistence | `tests/integration/payment_lifecycle_test.go::TestPaymentLifecyclePersistsEveryStage` |
| Unit-of-work nesting | `internal/infrastructure/postgres/rls_integration_test.go::TestUnitOfWorkRefusesNesting` |

### 4.2 Tenant isolation negative tests

The isolation guard (§16.2) gets adversarial treatment, because it is a security boundary and a
passing positive test proves nothing about it. Nine tests across two files, all tagged
`integration`:

| Property | Test |
|---|---|
| A tenant-A session cannot read a tenant-B row by primary key | `internal/infrastructure/postgres/rls_integration_test.go::TestCrossTenantAccessIsImpossible` |
| RLS blocks a direct `UPDATE`, not merely a `SELECT` | `::TestRLSBlocksADirectUpdate` |
| `WITH CHECK` rejects a cross-tenant `INSERT` — you cannot write into another tenant either | `::TestWithCheckRejectsACrossTenantInsert` |
| No tenant in the session returns **zero rows**, never rows | `::TestQueryWithoutTenantContextReturnsNoRows` |
| The application role genuinely lacks `BYPASSRLS`, so every assertion above means something | `::TestAppRoleLacksBypassRLS` |
| Every tenant-scoped table has `FORCE ROW LEVEL SECURITY` **in the live catalog**, not merely in the migration text | `::TestEveryTenantScopedTableHasForcedRLSInCatalog` |
| Partitions inherit RLS and the I3 index — a partition created without them is a silent hole | `::TestPartitionsCarryRLSAndTheI3Index` |
| `SET LOCAL` does not leak across transactions on a pooled connection | `::TestSetLocalDoesNotLeakAcrossTransactions` |
| The registry rows of one tenant are invisible to another | `tests/integration/tenant_isolation_test.go::TestATenantCannotSeeAnotherTenantsRegistryRow` |
| Reads **and** writes are refused by the database with the application guard removed | `::TestCrossTenantReadsAndWritesAreRefusedByTheDatabase` |
| A statement with no tenant in the session sees nothing | `::TestAStatementWithNoTenantInTheSessionSeesNothing` |

The last of these is the generated one, and it is the one that keeps working as the schema grows:
`tests/integration/tenant_isolation_test.go::TestEveryTenantScopedTableIsCoveredByAnIsolationCase`
enumerates the tenant-scoped tables from the catalog and asserts each is covered by an isolation
case. A new tenant-scoped table with no isolation test fails this on the day it is added — which
is the only time fixing it is cheap.

The two properties tested here that are easiest to get subtly wrong: a cross-tenant read returns
**not found**, not **forbidden**, because `403` on a foreign ID discloses that the ID exists; and
`tenant_id` in a request body is ignored entirely rather than validated, because the tenant comes
from the authenticated principal and from nothing else (baseline §16.2).

---

## 5. Contract, API, and workflow tests

### 5.1 The shared gateway adapter suite

Every adapter under `internal/adapters/gateway/` — Stripe, Adyen, PayPal and the simulator — must
pass the identical suite, [`internal/adapters/gateway/contract/suite.go`](../internal/adapters/gateway/contract/suite.go).
It is the executable definition of the `spi` port, and it is what makes "adding a gateway" a
bounded piece of work. Seventeen cases, run by `contract.RunSuite(t, subject)`:

```go
// internal/adapters/gateway/contract/suite.go
func RunSuite(t *testing.T, s Subject) {
    t.Run("IdempotencyKeyIsSent", ...)
    t.Run("RepeatedCallWithSameKeyIsSafe", ...)
    t.Run("TimeoutYieldsOutcomeUnknown", ...)              // A7: never DECLINED, never FAILED
    t.Run("ConnectionRefusedYieldsError", ...)             // provably never reached the gateway
    t.Run("HardDeclineMapsToNonFailoverReason", ...)
    t.Run("SoftDeclineMapsToFailoverReason", ...)
    t.Run("UnmappedDeclineIsUnknownAndDoesNotFailover", ...) // fail closed on a code we do not know
    t.Run("SuccessCarriesGatewayRef", ...)
    t.Run("LookupFindsByIdempotencyKeyAlone", ...)         // the reconciler's only handle after a crash
    t.Run("AmountAndCurrencyEchoed", ...)                  // L6
    t.Run("ContextCancellationIsHonoured", ...)
    t.Run("CredentialsNeverAppearInErrorsOrLogs", ...)
    t.Run("WebhookSignatureVerification", ...)
    t.Run("WebhookEventIDIsStableForSamePayload", ...)     // the dedup key must be a pure function
    t.Run("NilResultAndNilErrorNeverReturned", ...)        // "nothing happened" is not representable
    t.Run("ProvisionIsIdempotent", ...)
    t.Run("DeprovisionToleratesMissingAccount", ...)       // compensations must be re-runnable
}
```

```go
// internal/adapters/gateway/adyen/adyen_contract_test.go — one line per adapter
func TestAdyenContract(t *testing.T) { contract.RunSuite(t, adyenSubject(t)) }
```

Three of those cases deserve naming. `UnmappedDeclineIsUnknownAndDoesNotFailover` is the fail-closed
rule: a decline code no adapter has mapped must **not** be assumed retryable, because assuming
retryable on an unknown code is how a platform starts card-testing on someone else's cards.
`LookupFindsByIdempotencyKeyAlone` is what makes A7 recoverable — after a crash the only handle we
have on the gateway's side is the key we derived before dispatching. And
`NilResultAndNilErrorNeverReturned` closes the hole where an adapter returns "nothing happened",
which the orchestrator has no state to represent.

**Execution mode: in-process HTTP doubles, run untagged.** `make test-contract` runs
`./tests/contract/... ./internal/adapters/...` with **no** build tag, because the suite needs no
database, no broker and no running service — it drives each adapter against a stubbed transport.
There is no `go-vcr` dependency and no recorded-cassette corpus, and no job runs the suite against
a live sandbox account; the "nightly against live sandbox" tier that would catch gateway-side
contract drift before it reaches a merchant does not exist in this repository.

The certification suite of §11.4 is this same suite in its onboarding role, parameterized by
`(gateway, payment_method, currency)` and run against sandbox during onboarding step 10. The
identical assertions gate a merchant's `PRODUCTION_READY` and gate our own CI — which is why
"certified" is an artifact rather than an opinion.

### 5.2 Consumer-driven contracts for events

Each consumer declares what it needs from an event type; the producer's CI verifies it still
provides it. The registry is a **Go literal** in
[`tests/contract/events_test.go`](../tests/contract/events_test.go), not a directory of YAML
files — it lives next to the assertion that uses it so that adding a consumer requirement and
running it are the same edit.

```go
// tests/contract/events_test.go — consumerContracts, deliberately restricted to consumers whose
// CORRECTNESS depends on the field. A consumer that merely displays a value does not belong
// here, because listing everything makes the list mean nothing.
{
    Consumer:  "Ledger",
    EventType: "payment.captured.v1",
    Why: "The ledger posts on the per-capture delta and reconciles on the cumulative total; " +
         "deriving one from the other is wrong the first time a capture event is redelivered.",
    Requires: []fieldRequirement{
        {"attemptId", "string"},
        {"capturedAmount.amount", "integer"},     // integer minor units, never a float
        {"capturedAmount.currency", "string"},
        {"capturedTotal.amount", "integer"},
        {"authorizedAmount.amount", "integer"},
        {"isFinalCapture", "boolean"},
    },
},
```

The `Why` field is required by convention and is the thing that makes the registry survivable: it
is the argument a future reviewer needs when someone proposes removing the field.

| Test | Asserts |
|---|---|
| `TestEveryConsumerContractIsSatisfied` | Every declared requirement is present, at the declared type, in an instance generated from the producer's registered schema |
| `TestEveryContractedConsumerIsDeclaredBySchema` | The complement: a consumer cannot depend on a field the schema does not declare, which is the undeclared dependency that turns a legal additive change into a production incident |
| `TestPublishedSchemasAreBackwardCompatible` | Additive-only within a major version (§13.1) |
| `TestCompatibilityCheckerDetectsEachBreakingChange` | The compatibility checker itself is tested against a corpus of deliberate breaks, so a checker that silently stops checking is caught |
| `TestEveryRegisteredEventTypeProducesASchemaConformingEvent` | Every type the Go registry knows about can actually produce an instance its own schema accepts |
| `TestSchemaExamplesConformToTheirOwnSchema` | An example that its own schema would reject is worse than no example — consumers copy examples into their fixtures |

### 5.3 API tests: contract-driven validation

The route table, the DTOs and the OpenAPI document are asserted against each other in
[`internal/transport/httpapi/handlers/contract_test.go`](../internal/transport/httpapi/handlers/contract_test.go).
The direction matters: the tests read `api/openapi/payments-platform.v1.yaml` — there is **one**
OpenAPI document, not one per plane — and assert the code matches it, so the contract is the
statement and the code is what is checked.

| Test | Asserts |
|---|---|
| `TestEveryDeclaredOperationHasARoute` | Every `operationId` in the document is served. A documented endpoint that 404s is a lie told to an SDK generator |
| `TestSuccessResponsesValidateAgainstTheDeclaredSchema` | Handler responses validate against the schema the document declares for that status |
| `TestDTOFieldsAreDeclaredByTheContract` | No response struct carries a field the document does not declare — the undocumented-field leak, caught structurally |
| `TestPermissionTableMatchesTheContractScopes` | The scope each route requires equals the scope the document's security block declares |
| `TestAnonymousRoutesAreExactlyTheUnauthenticatedOnes` | The set of routes reachable without a token is exactly the set the document says is public. Stated as an equality in both directions, so neither adding nor forgetting a route slips through |
| `TestIdempotencyRequirementMatchesTheContract` | Every mutating endpoint the document marks as requiring `Idempotency-Key` enforces it (§14.1) |
| `TestRateLimitTableMatchesTheContract` | The limits the code applies are the limits the document publishes (§19.3) |

`scripts/check-openapi.sh` validates the document itself as the `openapi` stage of
`scripts/verify.sh`, and CI additionally diffs it against `main` for breaking changes.

### 5.4 Workflow tests

The workflow suite is in three places, and the split is by what each layer can prove:

| Where | Tag | What it proves |
|---|---|---|
| `internal/workflows/engine/postgres/*_test.go` | none | The **engine**: leases, fencing, crash-and-resume at every step, compensation ordering, DLQ behaviour — against an in-memory store that implements the same port |
| `internal/workflows/onboarding/onboarding_test.go` | none | The **definition**: that `merchant-onboarding@v1` is sound, that every activity and compensation is registered, and that each named business outcome lands in the right merchant state |
| `tests/integration/workflow_resume_test.go` | `integration` | The same guarantees against **real Postgres**, where the lease predicate and the partial unique index actually have to work |

The named guarantees, and the test for each:

| Guarantee | Test |
|---|---|
| Resume replays no completed step, at **every** step | `internal/workflows/engine/postgres/engine_test.go::TestCrashAndResumeAtEveryStep`; `internal/workflows/onboarding/onboarding_test.go::TestResumeDoesNotReplayCompletedSteps`; against real Postgres, `tests/integration/workflow_resume_test.go::TestWorkerCrashAtEveryOnboardingStepResumesWithoutRepeatingWork` |
| A crash **between** the step write and the instance write resumes correctly — the window a naive checkpoint misses | `internal/workflows/engine/postgres/engine_test.go::TestResumeAfterACrashBetweenTheStepWriteAndTheInstanceWrite` |
| Compensations run in strict reverse order | `internal/workflows/engine/postgres/compensate_test.go::TestCompensationsRunInStrictReverseOrder`; `internal/workflows/onboarding/onboarding_test.go::TestCompensationOrderOnCertificationFailure` |
| Compensation stops at the money pivot rather than rolling back past it | `::TestCompensationStopsAtACompletedRetainedPivot`, `::TestAbortIsRefusedPastAnIrreversiblePivot` |
| A compensation receives the step's **output**, not its input — you cannot delete a webhook registration whose ID you never captured | `::TestCompensationReceivesTheStepsOutputNotItsInput` |
| One failed compensation does not abandon the rest | `::TestAFailedCompensationDoesNotStopTheRest` |
| Compensations tolerate the forward operation never having happened | `internal/workflows/onboarding/onboarding_test.go::TestCompensationsTolerateTheForwardOperationNeverHavingHappened` |
| Starting twice is idempotent on the business key | `internal/workflows/engine/postgres/engine_test.go::TestStartIsIdempotentOnBusinessKey`; at the database, `tests/integration/workflow_resume_test.go::TestStartingOnboardingTwiceIsRefusedByTheDatabase` |
| A terminal instance releases the business key so a new one can start | `::TestStartAfterTerminalInstanceCreatesANewOne` |
| Retry exhaustion reaches the DLQ **with the full error chain** | `::TestRetryExhaustionReachesTheDLQWithTheErrorChain` |
| A KYC rejection is a business outcome, not an engine failure | `internal/workflows/onboarding/onboarding_test.go::TestKYCRejectionIsABusinessOutcome` |
| **Amendment A-01**: a compliance rejection uses `COMPLIANCE_REJECTED`, not `CERTIFICATION_FAILED` | `internal/workflows/onboarding/onboarding_test.go::TestComplianceRejectionUsesTheAmendedState` |
| Credential storage output carries no key material | `::TestStoreCredentialsOutputCarriesNoMaterial` |
| The certification report is hashed, tamper-evident and stored under retention | `::TestCertificationReportIsHashedAndStoredUnderRetention`, `::TestCertificationHashIsDeterministicAndTamperEvident` |
| Certification catches a gateway that ignores idempotency keys | `::TestCertificationCatchesAGatewayThatIgnoresIdempotencyKeys` |
| Validation rejects a merchant **before any side effect** | `::TestValidationRejectsAMerchantBeforeAnySideEffect` |

Two of those are worth pausing on.
`TestCertificationCatchesAGatewayThatIgnoresIdempotencyKeys` is the assertion that makes
certification more than a smoke test: a gateway that accepts our key and charges twice anyway is
the failure mode certification exists to find, and finding it during onboarding is the difference
between one duplicate charge and a merchant's worth of them. And
`TestMalformedVariantsFailEachSoundnessCheck` tests the *checker*: it feeds deliberately broken
workflow definitions to `TestOnboardingDefinitionIsSound` and requires each to be rejected, so a
soundness check that has silently stopped checking is itself caught.

## 6. Security, performance and chaos

### 6.1 Security tests

Untagged, so they run under plain `make test`. They live beside the code they defend rather than
in a `tests/security` tier, <!-- doc-refs: allow-missing --> which is what keeps them running on every push.

| Area | Where | What is asserted |
|---|---|---|
| **JWT attack matrix** | `internal/platform/authn/jwt_test.go::TestAttackMatrix` | `alg: none`; `alg: none` with the `kid` stripped too, in case the `kid` check were what saved us; RS256→HS256 key confusion signed with the public key; unknown `kid`; a key from another environment; wrong `iss`; wrong `aud`; `aud` type confusion; expired, not-yet-valid, and truncated-signature tokens. Each case names the reason it must be rejected **for**, so a token rejected by accident for the wrong reason is still a failure |
| **Clock skew** | `::TestClockSkewIsSymmetricAndBounded` | The tolerance is symmetric and bounded — a one-sided skew allowance is an expiry that never expires |
| **Replay** | `::TestReplayDetection`, `::TestReplayStoreOutageDegradesLoudlyRatherThanFailing` | A replayed `jti` is refused; a replay-store outage degrades **loudly**, never silently into acceptance |
| **Revocation** | `::TestRevocationIsRecheckedPerRequest` | Revocation is not cached for the token's lifetime |
| **JWKS** | `internal/platform/authn/jwks_test.go` | Rotation overlap (both keys validate); refresh is rate-limited; the document is size-bounded; a fetch error serves stale rather than failing open; background refresh is stoppable and leaks no goroutine |
| **mTLS** | `internal/platform/authn/mtls_test.go` | SPIFFE ID parsing; `::TestPeerAuthenticationRequiresAVerifiedChain` — a peer certificate that was not chain-verified is not an identity |
| **API keys** | `internal/platform/authn/apikey_test.go` | Rotation overlap; the rejection matrix; `::TestPrincipalScopeWildcardExpandsOnlyInTheGrant` — `payments:*` grants, it never *matches* a request scope |
| **Authorization** | `internal/platform/authz/authz_test.go` | `::TestMatrixIsComplete` and `::TestMatrixInvariants` over the whole role × permission matrix; `::TestDefaultDeny`; `::TestExplicitDenyBeatsAllow`; `::TestNoAllowEverCrossesATenantBoundary`; `::TestConditionsAreTotalOnEmptyInput` (a condition that is undefined on empty input is a condition that fails open); `::TestAllowIsExplainable`; `::TestDualControl` |
| **Cross-tenant** | §4.2 | Eleven tests, at the database, with the application guard removed |
| **PAN detector** (§17.2) | `internal/platform/secret/pan_test.go` | Every scheme detected; non-PANs **not** detected (a false positive that rejects legitimate traffic is also a failure); nested struct fields scanned; the scan is depth-bounded so a hostile payload cannot make it recurse; and `::TestPANDetectorNeverLogsTheValue`, which asserts the rejected value appears in no message |
| **PAN tripwire at the database** | `internal/infrastructure/postgres/invariants_integration_test.go::TestPANTripwireRejectsABareCardNumber` | The last line of defence, with the application bypassed |
| **Redaction** | `internal/platform/secret/secret_test.go` | `::TestSecretRedactsEveryFormattingVerb` covers every `fmt` verb; then inside a containing struct, through JSON, through `slog`, for non-string payloads, and `::TestUnmarshalJSONDoesNotEchoTheValue` |
| **Secret providers** | `internal/infrastructure/secrets/redaction_test.go` | No provider path leaks material into an error; the file provider's paths do not leak either |
| **Rate limiting** | `internal/infrastructure/redis/ratelimit_test.go`, `internal/infrastructure/resilience/ratelimiter_test.go` | The limiter's arithmetic, and the local fallback that keeps limits enforced with Redis down |

Two of those deserve their reasoning stated. `TestConditionsAreTotalOnEmptyInput` exists because
an authorization condition that is *undefined* on empty input is a condition that fails open the
first time a field is missing — which is the first time an attacker omits it. And
`TestContainsPANRejectsNonPANs` is as important as the positive case: a detector that trips on an
order reference which happens to pass Luhn takes the platform down for legitimate traffic, and a
detector nobody can leave enabled protects nothing.

**What is not here.** There is no injection corpus (SQL injection payloads, JSON depth bombs,
duplicate keys, Unicode confusables, CRLF in log-bound fields), no oversized-token DoS case, and
no statistical timing test for the signature comparison. Parameterised queries and `encoding/json`
make most of that structurally hard rather than merely unlikely, but "structurally hard" is an
argument, not evidence, and this section should not read as though the evidence exists.

### 6.2 Performance and load (k6)

Location [`tests/load/`](../tests/load/) — four k6 scripts plus `tests/load/lib`, run through `make loadtest SCENARIO=… BASE=… TOKEN=…`. Run against staging with production-shaped data (`deployment.md` §6.1). Each scenario carries its SLO assertions as k6 thresholds, so the load test **fails** rather than producing a chart someone has to interpret. None of the four has ever been run against a deployed target; see [`README.md`](../README.md#status-and-limitations).

| Scenario | Shape | Duration | Asserts |
|---|---|---|---|
| **steady-state** | Constant 5 000 TPS (§18 sustained target) | 30 min | p50 ≤ 60 ms, p99 ≤ 250 ms excluding gateway; error rate ≤ 0.01 %; zero `PROCESSING` payments unresolved after 15 min; `pp_consumer_lag` bounded |
| **ramp** | 0 → 15 000 TPS over 20 min (§18 peak) | 30 min | SLOs hold to 15 000; HPA scales without a latency cliff; no `503`s from `NO_ELIGIBLE_GATEWAY`; DB connections stay under the PgBouncer pool ceiling |
| **spike** | 1 000 → 15 000 in 30 s, hold 5 min, drop | 15 min | Errors during the spike are `429`/`503` with `Retry-After` — **never `500`**; recovery to baseline p99 within 3 min; the adaptive concurrency limiter sheds load rather than the system collapsing (§24 retry storm) |
| **soak** | 3 000 TPS steady | 4 h | No memory growth beyond 5 % after the first 30 min; no goroutine growth; no connection-pool leak; no file-descriptor growth; p99 at hour 4 within 10 % of hour 1; zero unresolved reconciliation exceptions |

```javascript
// tests/load/steady-state.js
export const options = {
  scenarios: {
    steady: { executor: 'constant-arrival-rate', rate: 5000, timeUnit: '1s',
              duration: '30m', preAllocatedVUs: 2000, maxVUs: 6000 },
  },
  thresholds: {
    'http_req_duration{endpoint:create_payment}': ['p(50)<60', 'p(99)<250'],
    'http_req_failed':                            ['rate<0.0001'],
    'checks':                                     ['rate>0.9999'],
    'pp_unresolved_processing':                   ['value==0'],
  },
};

export default function () {
  const key = `k6-${__VU}-${__ITER}-${Date.now()}`;   // unique idempotency key per logical op
  const res = http.post(`${BASE}/v1/payments`, JSON.stringify(payment()), {
    headers: { 'Idempotency-Key': key, 'Authorization': `Bearer ${TOKEN}`,
               'Content-Type': 'application/json' },
    tags: { endpoint: 'create_payment' },
  });
  check(res, {
    'status is 201':            (r) => r.status === 201,
    'amount is integer minor':  (r) => Number.isInteger(r.json('amount')),
    'traceId present':          (r) => !!r.json('traceId') || !!r.headers['Traceparent'],
  });
  // 2% of iterations retry with the SAME key: idempotency must hold under load
  if (__ITER % 50 === 0) {
    const replay = http.post(`${BASE}/v1/payments`, JSON.stringify(payment()), {
      headers: { 'Idempotency-Key': key, /* ... */ }, tags: { endpoint: 'replay' },
    });
    check(replay, { 'replay is idempotent': (r) =>
      r.status === 201 && r.headers['Idempotent-Replay'] === 'true' });
  }
}
```

The 2 % replay traffic is the point of running these at all: idempotency correctness under contention is not observable in a functional test with four goroutines.

### 6.3 Chaos tests

Location [`tests/chaos/`](../tests/chaos/), build tag `chaos`: **20 tests across 6 files**. Each
maps to a §24 failure-catalog entry, injects its fault through a named decorator, and states a
**steady-state hypothesis** — the properties that must hold *throughout*, not merely be restored
afterwards.

**Most of it runs in-process, and that is a deliberate trade.** The scenarios drive the *real*
payment orchestrator, the *real* gateway simulator and the *real* resilience primitives (breaker,
bulkhead, adaptive limiter, retry budget), wired to in-memory ports from
`internal/application/apptest`. Nothing the scenario is *about* is stubbed. Pausing a container
proves the deployment reacts; decorating a port proves the **code** reacts, deterministically, in
under a second, on every pull request. The scenarios that genuinely need infrastructure stopped —
an Aurora failover, a broker outage — are gated behind `PP_TEST_CHAOS_INFRA` and skip loudly
without it.

| File | Tests | Scenarios covered |
|---|---:|---|
| `gateway_test.go` | 5 | Steady state with no fault; gateway timeout (**never retries**); 5xx storm (does not fail over on an unknown outcome); soft decline (fails over, exactly one success); a slow gateway (degrades latency, times out safely) |
| `infra_test.go` | 4 | Database unavailable mid-transaction (fails closed); connection-pool exhaustion (rejects rather than queues); Redis loss (latency, not correctness); Kafka unavailable (loses no events) |
| `crash_test.go` | 3 | Pod crash between dispatch and commit (**never dispatches twice**); worker crash mid-workflow (resumes without repeating a side effect); an unknown outcome resolved by **lookup, not by guessing** |
| `retry_storm_test.go` | 3 | The retry budget bounds a storm; the adaptive limiter sheds rather than queues; a storm against the orchestrator produces no duplicate payment |
| `partition_test.go` | 2 | Partition fails closed and heals with no split brain; a partition is **not** reported as an unknown outcome |
| `clock_skew_test.go` | 3 | Skew beyond the webhook tolerance fails closed; a tampered body is rejected regardless of the clock; a secret rotation does not drop webhooks |

The last one in `partition_test.go` is the subtle one and is worth its own line: a network
partition is a *provable* non-delivery, so classifying it as `TIMEOUT_UNKNOWN` would park a
payment for the reconciler that could safely have been retried immediately. Getting that
distinction wrong in either direction costs money — one way a duplicate charge, the other way a
lost sale and a reconciliation exception nobody can close.

What the in-process design does **not** cover, and this document should not imply it does: node
loss, AZ loss, region loss, disk pressure, certificate expiry, Secrets Manager denial, KEDA
scaling under outbox backlog, and the combined-fault scenario. Those need a cluster, and no
cluster has ever run this. They are exercised as *design* in
[`docs/failure-handling.md`](failure-handling.md) and [`docs/disaster-recovery.md`](disaster-recovery.md)
§9, and their absence from the suite is one of the gaps listed in
[`README.md`](../README.md#status-and-limitations).

```go
//go:build chaos

func TestGatewayTimeoutLeavesPaymentProcessingAndNeverRetries(t *testing.T) {
    e := newEnv(t)
    h := e.Hypothesis()
    h.HoldsNow(t, "before the fault")

    var faults Counter
    e.Route(gwPrimary, Chain(e.Primary, TimeoutAlways(&faults)))

    stop := e.Watch(t, h)               // samples the hypothesis continuously
    res, err := e.Create(e.Ctx(), "timeout", 5_000)
    stop()

    // §12.3: an unknown outcome is not an error. The caller is told "processing".
    if err != nil {
        t.Fatalf("a gateway timeout was reported to the caller as an error: %v", err)
    }
    if got := res.Payment.State(); got != dpayment.StateProcessing {
        t.Fatalf("payment state = %s after a gateway timeout, want PROCESSING", got)
    }
    // The dispatch count is read from the fault decorator rather than from the adapter,
    // because the decorator sits where the network would be: it counts what was *sent*,
    // which is the number that determines whether the card was charged twice.
    if faults.Calls() != 1 {
        t.Fatalf("the gateway was called %d times, want exactly 1", faults.Calls())
    }
    h.HoldsNow(t, "after the timeout")
}
```

`e.Watch` samples the hypothesis for the duration and `stop()` reports any breach. Checking only
at the end would miss a transient state that violated an invariant and then self-corrected —
which is exactly the class of bug that later manifests as a mysterious duplicate.

---

## 7. The named failure scenarios

The scenarios from the brief, each as a named test with setup, action and assertion.

### FS-1 Gateway timeout during authorization
- **Tests:** `tests/chaos/gateway_test.go::TestGatewayTimeoutLeavesPaymentProcessingAndNeverRetries`, `tests/chaos/crash_test.go::TestAnUnknownOutcomeIsResolvedByLookupNotByGuessing`
- **Setup:** Merchant `ACTIVE`, routing plan `[adyen, stripe]`, simulator configured to hold the connection 12 s (> the 8 s hard timeout, §12 stage 14).
- **Action:** `POST /v1/payments` with a fresh idempotency key.
- **Assert:** Response is `200` with `status: "processing"` (§12.3 semantics), **not** an error. Payment is `PROCESSING`. Latest attempt is `TIMEOUT_UNKNOWN`. `simulator.CallCount("authorize") == 1` — **no retry, no failover**. `payment.reconciliation_required.v1` emitted exactly once. After the simulator reports `AUTHORISED` out of band, the payment becomes `AUTHORIZED`, the ledger has exactly one entry, and the call count is still 1.

### FS-2 Duplicate payment submission
- **Tests:** `tests/integration/idempotency_test.go::TestReplayReturnsTheByteIdenticalStoredResponse`, `::TestConcurrentIdenticalCreatesYieldExactlyOnePayment`, `::TestSameKeyWithADifferentBodyIsReportedAsReuse`
- **Setup:** Merchant `ACTIVE`; idempotency key `k1`.
- **Action:** (a) submit, wait for completion, submit again with `k1` and an identical body; (b) submit twice concurrently with `k1`; (c) submit with `k1` and a *different* amount.
- **Assert:** (a) `201` with the byte-identical stored response snapshot and `Idempotent-Replay: true`; exactly one payment; exactly one gateway call. (b) One `201`, one `409 IDEMPOTENT_REQUEST_IN_PROGRESS` with `Retry-After: 1` (§1.3 A6); the loser did **not** block on a lease; exactly one payment. (c) `422 IDEMPOTENCY_KEY_REUSED` (§14.2); no second payment.

### FS-3 Duplicate webhook delivery
- **Tests:** `tests/integration/webhook_test.go::TestDuplicateWebhookIsDroppedByTheUniqueIndex`, `::TestAnOutOfOrderWebhookIsANoOp`
- **Setup:** Payment `PROCESSING`; a valid `payment.captured` webhook body signed by the gateway.
- **Action:** Deliver the identical webhook 5× concurrently from 5 connections.
- **Assert:** All 5 receive `200` (a gateway must never be told to retry a webhook we already have). Exactly **one** state transition `PROCESSING → CAPTURED`. Exactly one `payment.captured.v1` event. Exactly one ledger entry. `webhook_dedup` has one row. `pp_webhooks_deduplicated_total` incremented by 4.

### FS-4 Database unavailable mid-payment
- **Tests:** `tests/chaos/infra_test.go::TestDatabaseUnavailableMidTransactionFailsClosed`, `::TestConnectionPoolExhaustionRejectsRatherThanQueues`
- **Setup:** Steady 200 TPS.
- **Action:** Force an Aurora failover (staging) / pause the Postgres container (local) for 45 s.
- **Assert:** Requests during the window receive `503 SERVICE_UNAVAILABLE`, `retryable: true`, with `Retry-After` — **never `500`**, never a partial write. Readiness fails; **liveness holds** and no pod restarts (the §1.7 rule in `deployment.md`). `GET` requests continue from replicas. On recovery: zero payments in an indeterminate state, zero duplicate attempts, invariants I1/I3 hold, and every payment that returned `201` before the failure is present and correct.

### FS-5 Kafka unavailable
- **Tests:** `tests/chaos/infra_test.go::TestKafkaUnavailableLosesNoEvents`, `tests/integration/outbox_test.go::TestAPublishFailureLeavesTheRowClaimable`
- **Setup:** Steady 200 TPS; a marker set of 5 000 payments.
- **Action:** Stop all brokers for 10 minutes, then restart.
- **Assert:** Payment success rate is **unaffected** (the outbox decouples publishing from the request path, §13.4). `pp_outbox_backlog` rises monotonically; the relay backs off without erroring the request path. `OutboxBacklogGrowing` alert fires. After recovery, the backlog drains fully; every one of the 5 000 payments has its complete event set; per-`payment_id` ordering is preserved; consumers dedupe any redelivery; ledger entry count matches the payment count exactly.

### FS-6 Redis unavailable
- **Test:** `tests/chaos/infra_test.go::TestRedisLossDegradesLatencyNotCorrectness`
- **Setup:** Steady 200 TPS including 5 % idempotent replays.
- **Action:** Kill the Redis cluster for 5 minutes.
- **Assert:** Zero errors attributable to Redis. Idempotency remains correct via Postgres (§14.3) — every replay still returns the stored snapshot. Rate limits still enforced via local token buckets, coarser but bounded. `pp_config_snapshot_age_seconds` rises but stays under the cliff. p99 rises to ≤ 400 ms and returns to baseline within 60 s of recovery. **Zero duplicate payments.**

### FS-7 Gateway failover mid-transaction
- **Tests:** `tests/chaos/gateway_test.go::TestSoftDeclineFailsOverAndProducesExactlyOneSuccess`, `internal/application/payment/orchestrator_test.go::TestFailoverNeverProducesTwoSuccessfulAttempts`
- **Setup:** Routing plan `[adyen, stripe]`; Adyen returns `503` for the first 2 calls of each payment.
- **Action:** Submit a payment.
- **Assert:** Attempt 1 (Adyen) retries ≤ 2 times **on the same attempt** with the **same** `gateway_idempotency_key` (§14.4). On exhaustion, attempt 2 is created for Stripe with a **different** key (asserted by inequality, and by the key being a pure function of `attempt_id`). Attempt 1 is `ERROR`; attempt 2 is `SUCCESS`; the payment is `AUTHORIZED`. Exactly one successful attempt (I3). The routing plan is persisted with reasons. `pp_routing_decisions_total{reason="fallback_error"}` incremented once. `internal/application/payment/orchestrator_test.go::TestHardDeclineDoesNotFailOver` asserts that a hard decline produces **zero** attempt-2 (§9.1).

### FS-8 Merchant onboarding interrupted
- **Tests:** `tests/integration/workflow_resume_test.go::TestWorkerCrashAtEveryOnboardingStepResumesWithoutRepeatingWork`, `internal/workflows/engine/postgres/compensate_test.go::TestCompensationsRunInStrictReverseOrder`, `internal/workflows/onboarding/onboarding_test.go::TestCompensationOrderOnCertificationFailure`
- **Setup:** Onboarding advanced to `provision-gateways` (step 5) with two gateways provisioned.
- **Action:** (a) SIGKILL the worker; (b) separately, make `apply-configuration` (step 8) fail permanently.
- **Assert:** (a) The lease expires; a different worker resumes; **no completed step re-executes** (external call counts unchanged); the instance completes; the merchant reaches `ACTIVE`; `Provision` was called exactly once per gateway. (b) The instance moves to `FAILED`; compensations run in strict reverse order (`delete-webhook-registration`, `delete-secret-version`, `deprovision-subaccount`, `cancel-kyc-case`); the simulator reports zero orphaned resources; the step payload with its full error chain is in `workflow_dlq`; the merchant is **not** `ACTIVE`.

### FS-9 Pod crash mid-request
- **Test:** `tests/chaos/crash_test.go::TestPodCrashBetweenDispatchAndCommitNeverDispatchesTwice`
- **Setup:** A hook in `payment-orchestrator` that SIGKILLs the process after the gateway returns `AUTHORISED` but before the state transaction commits.
- **Action:** Submit a payment; kill; let the client retry with the same idempotency key.
- **Assert:** The attempt row exists (written **before** dispatch, §12 stage 13) with `DISPATCHED`. The payment is `PROCESSING`. The client's retry receives `409 IDEMPOTENT_REQUEST_IN_PROGRESS`, then after lease expiry receives `processing` semantics — **never a second dispatch**. `simulator.CallCount("authorize") == 1`. The reconciler resolves the attempt via the deterministic key; the payment becomes `AUTHORIZED`; exactly one ledger entry. Variants cover crashing before dispatch (Window 1) and after commit before response (Window 2) — see `disaster-recovery.md` §7.2.

### FS-10 Network partition
- **Tests:** `tests/chaos/partition_test.go::TestPartitionFailsClosedAndHealsWithoutSplitBrain`, `::TestAPartitionIsNotReportedAsAnUnknownOutcome`
- **Setup:** Chaos Mesh `NetworkChaos` partitioning `payment-orchestrator` from the Aurora writer subnet, while `payment-api` remains connected.
- **Action:** Hold for 3 minutes under 100 TPS, then heal.
- **Assert:** **CP behaviour** (§1.3 A4): writes fail closed with `503`, retryable; zero partial writes; zero payments in an indeterminate state. Reads continue from replicas (`AP`, bounded staleness). No orchestrator instance believes it can write. On heal: automatic recovery within 30 s, no manual intervention, invariants hold, and the reconciliation sweep finds zero exceptions. The DR fence — a pod with a stale epoch refusing writes — is designed in `disaster-recovery.md` §3 and has **no test**; nothing in this repository exercises a region fence.

---

## 8. Test data strategy

### 8.1 Fixtures and scopes

No fixture files, no `testdata/*.json` for domain objects, <!-- doc-refs: allow-missing --> no shared setup functions that a
hundred tests depend on. There is no `tests/builders` package: <!-- doc-refs: allow-missing --> in-package tests construct
aggregates through their real constructors and drive them through their real commands, and the
cross-cutting suites get their data from `testenv.Isolate`, which is the closest thing to a
builder in this tree.

```go
// tests/testenv/postgres.go
func Isolate(t testing.TB, pool *pgxpool.Pool) *Scope {
    EnsurePartitions(t, pool)

    nonce := Nonce(t)                 // derived from the test's name: stable across runs,
    base  := BaseTime()               // distinct across tests, no global counter
    s := &Scope{
        Pool:      pool,
        TenantA:   DeterministicID(PrefixTenant,   base, nonce+"/A"),
        TenantB:   DeterministicID(PrefixTenant,   base, nonce+"/B"),
        MerchantA: DeterministicID(PrefixMerchant, base, nonce+"/mA"),
        MerchantB: DeterministicID(PrefixMerchant, base, nonce+"/mB"),
        Clock:     NewClock(base),
    }
    s.SeedTenant(s.TenantA); s.SeedTenant(s.TenantB)
    s.SeedMerchant(s.TenantA, s.MerchantA); s.SeedMerchant(s.TenantB, s.MerchantB)

    s.before = s.sharedSnapshot()
    t.Cleanup(s.assertClean)          // the shared namespace must be unchanged afterwards
    return s
}
```

Two tenants and two merchants **by default**, in every scope, is the detail that pays for itself:
almost every isolation bug is only visible when a second tenant's data exists to leak, and a
harness that makes you ask for the second tenant is a harness where most tests do not have one.

Reaching a state is done by replaying the real transition path, never by assigning the field. An
aggregate constructed with `state = REFUNDED` directly can be one the FSM would never produce, and
a test built on it asserts behaviour that cannot occur in production.

### 8.2 Determinism

| Source of nondeterminism | Replacement |
|---|---|
| Wall clock | The `shared.Clock` port. Tests use `shared.FixedClock{T: base}` with explicit `Advance()`; `testenv.NewClock(base)` is the cross-cutting suites' version. `time.Now()` in the domain would be a layering violation the architecture check catches |
| ID generation | `testenv.DeterministicID(prefix, at, seed)` produces stable, valid, sortable ULIDs from a seed. Test output diffs are readable and failures are reproducible |
| Per-test namespacing | `testenv.Nonce(t)` derives the namespace from the **test's own name**, so two tests never collide and one test's ids are the same on every run |
| Random | Injected, never package-level. The suite runs no generated-input property tests, so there is no seed to print — the state spaces are enumerated exhaustively instead (§2.2) |
| Map iteration | Sorted before assertion. `shared.StateMachine.Next()` and `.States()` sort for the same reason |
| Goroutine scheduling | `-race` on every run (`make test-race`, and the `test-race` stage of `scripts/verify.sh`); explicit synchronization rather than `time.Sleep` |
| Timeouts and polling | An explicit per-test budget — the integration suite's `ctx(t)` gives 30 s, generous for a round trip and mean for a deadlock, so a blocked lock fails with a deadline instead of hanging until CI's global timeout kills the run and names nothing |
| Service ports | Supplied by `PP_TEST_*`, never hardcoded |

### 8.3 No shared mutable fixtures

| Rule | Enforcement |
|---|---|
| Every test creates the data it needs | `testenv.Isolate` per test; no `var globalMerchant` |
| Every test uses its own tenants | `testenv.Nonce(t)` derives them from the test's name, so they are stable across runs and distinct across tests |
| Tests run `t.Parallel()` | Which makes a shared mutable fixture fail immediately and loudly rather than intermittently — and which is why the integration suite has no `TestMain` that configures process-wide state |
| The one piece of process-wide state is guarded | Migrations run under `sync.Once`, because running the migrator per test would serialize the whole suite behind an advisory lock for no benefit |
| Seeded reference data (currencies, gateway descriptors, routing defaults) is treated as immutable | Read-only; a test that needs different reference data seeds it under its own tenant |

### 8.4 Proving cleanup

A test that leaks rows makes the *next* test flaky, and the failure appears somewhere else
entirely. So cleanup is asserted, not assumed —
`(*testenv.Scope).assertClean`, registered by `Isolate` in `t.Cleanup`:

```go
// tests/testenv/postgres.go
func (s *Scope) assertClean() {
    for _, tenant := range []string{s.TenantA, s.TenantB} {
        // Deletable artifacts, in dependency order. A cleanup error is REPORTED, not fatal:
        // it must not mask the test's own failure, but it must be visible.
        stmts := []string{
            `DELETE FROM pp.outbox_events            WHERE tenant_id = $1`,
            `DELETE FROM pp.idempotency_records      WHERE tenant_id = $1`,
            `DELETE FROM pp.reconciliation_exceptions WHERE tenant_id = $1`,
            `DELETE FROM pp.inbound_webhooks         WHERE tenant_id = $1`,
            `DELETE FROM pp.workflow_steps           WHERE tenant_id = $1`,
            `DELETE FROM pp.workflow_dlq             WHERE tenant_id = $1`,
            `DELETE FROM pp.workflow_instances       WHERE tenant_id = $1`,
            `DELETE FROM pp.gateway_connections      WHERE tenant_id = $1`,
        }
        // … run them under the tenant's own session GUC, committed …

        // Assert the deletable set is actually empty, PER TABLE, so a silently-failing
        // DELETE is a failure here rather than a mystery in the next test.
        s.assertEmpty(ctx, tenant, "pp.outbox_events", "pp.idempotency_records", …)
    }

    // The half that matters most: the SHARED namespace must be exactly as it was found.
    // This catches a test that wrote outside its own tenant — a bug that would otherwise
    // appear as a flake in an unrelated test three days later.
    after := s.sharedSnapshot()
    // … report any drift, table by table …
}
```

Two design points. The `DELETE`s run under the tenant's own session GUC and are **committed**, so
cleanup itself goes through RLS — a cleanup that needed to bypass RLS would be evidence that the
test could too. And ledger and audit rows are deliberately **not** in the delete list: they are
append-only by trigger, so a suite that could delete them would prove the append-only guarantee
false.

There is no `goleak`. Goroutine leaks are caught, when they are caught, by `-race` and by the
per-test context budget rather than by a leak detector.

---

## 9. Flakiness policy

A flaky test is worse than no test: it trains the team to re-run CI without reading the failure, and eventually a real failure gets re-run away.

### 9.1 Detection

| Mechanism | Detail |
|---|---|
| Every CI run publishes JUnit XML to a results store | Per-test pass/fail history over 30 days. **Not implemented** — no results store exists |
| Nightly repeat run | `go test ./... -count=5 -race` on `main`; any test not passing 5/5 is flagged. **Implemented** — the `flake-hunt` job of `.github/workflows/nightly.yml` |
| Flakiness score | `failures / runs` over 30 d, weighted toward recency. **Not implemented** — needs the results store above |
| Retry telemetry | CI retries **nothing** automatically. A re-run is a manual action, and it is recorded against the test |
| Timing regression | A test whose p95 duration doubles is flagged — it is usually a `Sleep` racing something. **Not implemented** |

### 9.2 Quarantine

| Score | Action |
|---|---|
| 1 failure in 30 d | Logged, no action |
| ≥ 2 failures in 30 d, or ≥ 1 % | Issue filed, owner = the last person to modify the test, due in 5 working days |
| ≥ 5 % | **Quarantined**: moved behind a `flaky` build tag, excluded from the required check, still run nightly for visibility. A 14-day fix-or-delete clock starts |
| Quarantine expired | The test is **deleted**, and if it was the only evidence for an entry in `tests/critical_paths.yaml` that entry immediately fails `scripts/coverage.sh --only-paths` and forces a replacement rather than a silent loss of coverage |

Quarantine is capped at **15 tests repository-wide**. Exceeding the cap fails the build for
everyone. Without a cap, quarantine becomes a landfill.

**Status: the policy above is a commitment, not a mechanism.** There is no `flaky` build tag in
the tree, no results store, and no quarantine list. The one part that is real is the nightly
repeat run — `.github/workflows/nightly.yml` has a `flake-hunt` job — and the critical-path
registry, which does catch a deleted test the moment it is deleted. The scoring, the cap and the
auto-filed issue need a results store this repository does not have.

### 9.3 The money-path rule

**A flaky test in the money path blocks the release. It is never quarantined.**

The money path is defined precisely, so this is not a judgement call:

- `internal/domain/payment/**`, `internal/domain/ledger/**`, `pkg/money`
- `internal/application/payment/**`, `internal/platform/idempotency/**`
- Every test named in [`tests/critical_paths.yaml`](../tests/critical_paths.yaml)
- `tests/integration/invariants_test.go`, `tests/integration/idempotency_test.go`, `tests/integration/tenant_isolation_test.go`
- Every FS-1 … FS-10 test in §7

Rationale: a flaky test here means one of two things — the test is wrong, or **the system is
nondeterministic in the money path**. The second is a production incident that has not happened
yet. Both require investigation before shipping. "It passes on re-run" is a description of a race
condition, not a resolution.

Enforcement would be a gate that reads the results store, intersects the flagged set with the
money-path list above, and fails the release pipeline with the test name and its failure history,
with no override flag. **That gate does not exist**; there is no `scripts/flaky-gate.sh`. <!-- doc-refs: allow-missing --> What
exists today is the rule, the definition of the path it applies to, and a nightly repeat run that
surfaces the candidates. Building the gate needs somewhere to keep 30 days of per-test history,
which is the missing piece rather than the logic.

---

## 10. Running it

### 10.1 Locally

```bash
make test                 # untagged tests, no services (~40 s) — the inner loop
make test-race            # the same with -race and -count=1
make test-contract        # the gateway adapter suite + event contracts (untagged; no tag is passed)
make test-integration     # -tags=integration; needs PP_TEST_POSTGRES_DSN et al., skips without
make test-e2e             # -tags=e2e against a running stack (make dev-up first)
make test-chaos           # -tags=chaos; the destructive half needs PP_TEST_CHAOS_INFRA=1
make test-all             # test-race + test-integration + test-contract
make cover                # coverage.out, coverage.html, then the §1.1 floors and §1.2 registry
make critical-paths       # the §1.2 registry alone (instant)

# Targeted
go test ./internal/domain/payment/... -run TestPaymentMachineAcceptsExactlyTheDeclaredEdges -v
go test ./internal/domain/merchant/... -run TestAmendmentA01ComplianceRejection -v
go test -tags=integration ./tests/integration/... -run TestCrossTenantReadsAndWritesAreRefusedByTheDatabase -v
go test -tags=chaos ./tests/chaos/... -run TestGatewayTimeoutLeavesPaymentProcessingAndNeverRetries -v

# Full stack locally (docker compose: postgres, redis, redpanda, the simulator, the OTel
# collector, Jaeger, Prometheus, Grafana — then migrations and the dev seed)
make dev-up
make test-e2e
make dev-down

# Load (requires k6; point at staging, never at prod)
make loadtest SCENARIO=steady-state BASE=https://api.staging.example.com TOKEN=…
make loadtest SCENARIO=ramp  BASE=…  TOKEN=…
make loadtest SCENARIO=spike BASE=…  TOKEN=…
make loadtest SCENARIO=soak  BASE=…  TOKEN=…     # 4 h

# Everything CI verifies, before pushing
make verify               # 17 stages: fmt, vet, build, test-race, lint, architecture,
                          # error-catalog, rules-documented, metrics-cardinality, events,
                          # openapi, migrations, secrets, licences, runbook-links,
                          # doc-references, coverage
make verify-fast          # the same without the race detector; coverage runs registry-only
```

There is no `make test-api`, `make test-workflow`, `make test-security`, `make mutation-probe`,
`make up`/`make down` or `make test-chaos-local`. The API, workflow and security tests are not a
separate tier — they are in-package tests under `internal/transport/httpapi/handlers`,
`internal/workflows` and `internal/platform/{authn,authz,secret}`, and they run under plain
`make test`. `make` with no target prints every target with its description.

### 10.2 In CI

`.github/workflows/ci.yml` runs **15 jobs**, gated on `ci-ok`:

| Job | What it runs |
|---|---|
| `prepare` | Module and toolchain setup, shared cache |
| `fast` | `gofmt`, `go vet`, the repository's own check scripts |
| `unit` | `go test ./... -race -count=1` |
| `integration` | `-tags=integration` against service containers |
| `contracts` | OpenAPI, event schemas, the error catalogue |
| `migrations` | Numbering, pairing, RLS presence, destructive markers |
| `sast` | CodeQL |
| `vuln` | `govulncheck` |
| `dependency-review` | New dependencies and their licences |
| `manifests` | Kubernetes and Helm render + validate |
| `terraform` | `fmt`, `validate`, `tflint`, `checkov` |
| `image` | Multi-stage build per service |
| `scan` | Image vulnerability scan |
| `coverage-gate` | The coverage profile and its gates |
| `ci-ok` | The required check every other job feeds |

`.github/workflows/nightly.yml` adds `flake-hunt` (the repeat run), `chaos`, `soak`, `rescan`,
`dr-drill`, `traceability` and `report`.

| Trigger | Budget |
|---|---|
| Every push | `fast` + `unit`, ~4 min |
| Pull request | the full 15-job set, ~11 min |
| Merge to `main` | plus `cd.yml`: publish → post-merge gates → dev → smoke → staging → smoke → prod |
| Nightly | `nightly.yml`, ~70 min |

Two things in the table above the pipelines do **not** do, stated so nobody plans around them: no
job runs `buf generate`, so the `grpc`-tagged service implementations are compiled by nothing; and
no job builds with `-tags temporal`, because `go.temporal.io/sdk` is not in `go.mod`. Both are in
[`README.md`](../README.md#status-and-limitations).

---

## 11. Cross-references

| Topic | Document |
|---|---|
| CI stages and gates these tests run in | [`docs/deployment.md`](deployment.md) §4 |
| Failure catalog the chaos scenarios map to | [`docs/failure-handling.md`](failure-handling.md), baseline §24 |
| DR game days and their pass criteria | [`docs/disaster-recovery.md`](disaster-recovery.md) §9 |
| Metrics and SLOs the load tests assert against | [`docs/observability.md`](observability.md) §3, §4 |
| Validation rule IDs asserted by the L1–L7 tests | [`docs/validation-plane.md`](validation-plane.md), baseline §21 |
| Certification suite (the contract suite in its onboarding role) | baseline §11.4, [`docs/onboarding.md`](onboarding.md) |
| Traceability matrix (test ↔ requirement) | `docs/spec/09-traceability.md`, baseline §26 |
