# `tests/` — the cross-cutting suites

Everything under `tests/` asserts something no single package can: a property that only exists once
the database, the broker, the gateway and the HTTP surface are all in the picture. Package-local
tests live next to the code they test and are not described here.

> The authority for *what* is tested is [`docs/testing.md`](../docs/testing.md); the authority for
> what the system must do is [`docs/spec/00-design-baseline.md`](../docs/spec/00-design-baseline.md).
> This file is the operator's guide: what to run, what it needs, how long it takes, and what a
> failure in each suite actually means.

---

## The five suites at a glance

| Suite | Tag | Needs | Runtime | Runs on | Fails when |
|---|---|---|---|---|---|
| [`tests/contract`](#contract) | *(none)* | nothing | < 2 s | every push | a published event schema and the code that produces it disagree |
| [`tests/integration`](#integration) | `integration` | PostgreSQL 15+ | ~10 s | every PR | a constraint, policy or index that the platform's safety rests on is gone |
| [`tests/chaos`](#chaos) | `chaos` | nothing (in-process faults) | ~2 s | every PR + nightly | the system reacts to a fault in a way that could move money twice |
| [`tests/e2e`](#e2e) | `e2e` | the full stack over HTTP | ~8 min | merge to `main` | a promise in the published API is not kept end to end |
| [`tests/load`](#load) | — (k6) | staging | 15 min – 4 h | nightly / pre-release | an SLO in baseline §18 is missed |

`tests/testenv` is the shared harness. It is deliberately **untagged**, so a break in it is caught
by the cheapest CI stage rather than the slowest.

---

## Running everything

```bash
# The whole tagged surface must compile even when nothing is configured to run it.
gofmt -l tests                                  # must be empty
go vet ./tests/...                              # untagged: contract + testenv
go vet -tags integration ./tests/...
go vet -tags e2e ./tests/...
go vet -tags chaos ./tests/...

# The two suites that need nothing.
go test ./tests/... -race -count=1               # contract
go test -tags chaos ./tests/... -race -count=1   # chaos

# The two that need something, and skip loudly without it.
go test -tags integration ./tests/... -race -count=1
go test -tags e2e ./tests/... -count=1
```

Every suite that needs a dependency **skips with a message naming the environment variable and the
command that would provide it**. That is a deliberate design rule, not a convenience: a test that
silently passed because a service was missing would make the suite's green a statement about the
runner rather than about the system.

```
--- SKIP: TestCrossTenantReadsAndWritesAreRefusedByTheDatabase (0.00s)
    testenv.go:70: PP_TEST_POSTGRES_DSN is not set — this test needs a real PostgreSQL 15+
    instance. Run scripts/dev-up.sh and export the variables it prints, or point the variable at
    an existing service. (PP_TEST_DATABASE_URL is accepted as a fallback.)
```

---

## Environment variables

All of them are read in `tests/testenv/testenv.go`, which is the only configuration surface these
suites have — there is no config file and no flag, because a harness with two ways to be configured
has one way to be configured wrongly.

| Variable | Used by | What it must point at |
|---|---|---|
| `PP_TEST_POSTGRES_DSN` | integration | A PostgreSQL 15+ database, **connected as `pp_app`** (see below) |
| `PP_TEST_DATABASE_URL` | integration | Accepted as a fallback for the above, shared with the older suite in `internal/infrastructure/postgres` |
| `PP_TEST_POSTGRES_SCRATCH_DSN` | integration | A **throwaway** database the migration test may migrate all the way down. Must not be the one above |
| `PP_TEST_REDIS_ADDR` | integration | `host:port` of a Redis 7 |
| `PP_TEST_KAFKA_BROKERS` | integration | Comma-separated broker list |
| `PP_TEST_BASE_URL` | e2e | The data plane, e.g. `http://localhost:8080` |
| `PP_TEST_CONTROL_URL` | e2e | The control plane; defaults to `PP_TEST_BASE_URL` |
| `PP_TEST_SIMULATOR_URL` | e2e | The gateway simulator, e.g. `http://localhost:9090` |
| `PP_TEST_AUTH_TOKEN` | e2e | A bearer token carrying `payments:write` and `merchants:write` |
| `PP_TEST_TENANT_ID` | e2e | The tenant that token is scoped to |
| `PP_TEST_MERCHANT_ID` | e2e | An `ACTIVE` merchant in that tenant, for the payment tests |
| `PP_TEST_CHAOS_INFRA` | chaos | `1` to opt into the scenarios that stop and start real infrastructure |

### The role the integration suite connects as

**It must not be a superuser and must not have `BYPASSRLS`.** Half of what the integration suite
asserts — RLS filtering, the append-only revokes, the absence of a bypass — is invisible to a
superuser, and a suite that ran as one would pass against a schema that protects nothing.

`testenv.RequireNonBypassRLS` skips loudly rather than passing quietly if you get this wrong:

```
--- SKIP: TestCrossTenantReadsAndWritesAreRefusedByTheDatabase
    connected as "postgres" which is superuser=true bypassrls=false: the RLS and constraint
    negative tests would pass without the database doing anything.
```

A local setup that works:

```bash
createdb pptest && createdb ppscratch
# Apply migrations once as an owner/superuser, then let pp_app log in.
psql -d pptest -c "ALTER ROLE pp_app WITH LOGIN PASSWORD 'x';"
psql -d pptest -c "GRANT CREATE ON DATABASE pptest TO pp_app;"
psql -d pptest -c "GRANT CREATE, USAGE ON SCHEMA pp TO pp_app;"

export PP_TEST_POSTGRES_DSN="postgres://pp_app:x@127.0.0.1:5432/pptest?sslmode=disable"
export PP_TEST_POSTGRES_SCRATCH_DSN="postgres://postgres@127.0.0.1:5432/ppscratch?sslmode=disable"
```

---

## <a id="contract"></a>`tests/contract` — event schemas and consumer contracts

**No build tag. No dependencies. Always runs.**

```bash
go test ./tests/contract/... -race -count=1
```

Three things, and each catches a different class of defect:

1. **Producer conformance.** Every registered event type is produced through the *real* codec
   (`internal/events`) and validated against its JSON Schema in `api/events/`, payload and envelope
   both. Without this a schema is a document rather than a contract.
2. **Consumer satisfaction.** Each consumer's required field set is declared in Go
   (`consumerContracts`) and asserted against the produced event. Schema conformance alone does not
   give you this: a producer may legally stop populating an *optional* field and stay schema-valid
   while every consumer that depended on it breaks.
3. **Schema compatibility.** Within a major version only additive changes to optional fields are
   permitted (baseline §13.1); a new major must keep its predecessor on disk for the dual-publish
   window.

The JSON Schema validator is hand-written against the standard library, covering exactly the
constructs `api/events/` uses — `type`, `required`, `properties`, `enum`, `additionalProperties`,
plus `const`, `pattern`, length and numeric bounds, `items`, local `$ref`, and the date formats.
`TestValidatorUnderstandsEverySchemaKeyword` fails the build when a schema grows a construct the
validator does not implement, which converts the characteristic failure of a hand-rolled validator —
silently ignoring a rule — into a message naming the keyword and the file.

**Interpreting a failure.** The message names the JSON path and the rule:
`data.capturedAmount.amount: is number, want type integer` means a producer started emitting a
decimal for money. `Ledger reads capturedTotal.amount, which the producer did not supply` means a
field a consumer depends on stopped being populated — the schema is still satisfied, and the
consumer is still broken.

---

## <a id="integration"></a>`tests/integration` — against a real PostgreSQL

**Tag: `integration`.** Skips without `PP_TEST_POSTGRES_DSN`.

```bash
go test -tags integration ./tests/integration/... -race -count=1
```

| File | What only a real database can tell you |
|---|---|
| `payment_lifecycle_test.go` | Each stage changes exactly what it should — and nothing else |
| `idempotency_test.go` | N concurrent identical creates yield **one** payment, asserted at the database; a replay returns the byte-identical stored response; a reused key with a different body is reported as reuse (the transport maps it to `422 IDEMPOTENCY_KEY_REUSED`); an expired lease is reclaimed by exactly one racer |
| `tenant_isolation_test.go` | With the application guard removed, every cross-tenant read returns zero rows and every write is refused. Catalog-driven: a new tenant-scoped table with no probe fails the build |
| `invariants_test.go` | I1, I2 and I3 attacked directly with raw SQL, including under concurrency |
| `outbox_test.go` | Two relay shards preserve per-aggregate order; a publish failure leaves the row claimable; the backlog gauge equals the table it summarises |
| `workflow_resume_test.go` | A worker killed at each of the twelve onboarding steps: exclusive takeover, fencing epoch, checkpoint intact, no duplicate step record |
| `migration_test.go` | Up → all the way down → up again on a scratch database, with a byte-identical schema fingerprint and the reference data still present |

**Isolation.** Every test gets two tenants and two merchants of its own from `testenv.Isolate`, and
every test runs `t.Parallel()`. Cleanup is *asserted*, not assumed: the scope deletes what the role
may delete, checks the deletable tables are empty, and compares the shared namespace against a
snapshot taken at the start. A test that wrote outside its own tenant fails in its own failure
message rather than as an inexplicable flake three days later.

**Interpreting a failure.**

- `expected the DATABASE to reject this, got no error` — a constraint, index or policy is gone.
  Nothing about the application changed; the last line of defence did.
- `rejected by constraint "x", want "y"` — something refused the write, but not the thing under
  test. The invariant's own guard may already be missing.
- `test mutated shared state outside its own tenants` — the test leaked. Fix it before it makes an
  unrelated test flaky.
- `connected as "…" which is superuser=true` — see the role note above; the suite skipped rather
  than lying to you.

**A defect this suite found and works around.** `postgres.DefaultPoolConfig` sets
`pgx.QueryExecModeExec`, which makes pgx infer parameter types from Go values; a `[]byte` becomes
`bytea`. Every repository method that passes a `[]byte` into a `jsonb` column therefore fails with
SQLSTATE 22P02 — confirmed for `OutboxRepository.Append`, and the same shape appears in
`WebhookRepository.Record` and the workflow repository's writes. It is a production defect, not a
test-only one. The affected tests seed with raw SQL and say so at the call site; the fix belongs in
`internal/infrastructure/postgres`. See `repoWritesJSONB` in `repos_test.go`.

---

## <a id="chaos"></a>`tests/chaos` — fault injection

**Tag: `chaos`.** Needs nothing; the destructive infrastructure scenarios are gated behind
`PP_TEST_CHAOS_INFRA`.

```bash
go test -tags chaos ./tests/chaos/... -race -count=1
```

Every scenario has four parts, and one missing any of them is not a chaos test:

1. a **steady-state hypothesis** — the properties that must hold before, during and after;
2. a **fault**, injected through a composable decorator over a port (`TimeoutAlways`, `FailAfter`,
   `FailFor`, `SlowBy`, `PartitionFor`, and the unit-of-work, velocity and publisher decorators);
3. an **observation** of what the system did while the fault was in force;
4. an assertion that the hypothesis **still holds**, sampled at every committed transaction.

| Scenario | `docs/testing.md` | The property |
|---|---|---|
| Gateway timeout | C-1, FS-1 | `PROCESSING`, `TIMEOUT_UNKNOWN`, **exactly one dispatch**, no failover |
| Gateway 5xx storm | C-2 | A 5xx is an unknown outcome too: no failover |
| Soft decline | FS-7 | Failover *is* legitimate — the control that stops the two above being satisfied by a system that never fails over |
| Slow gateway | C-19 | Latency degrades; a deadline that fires mid-call is unknown, never failed |
| Postgres primary loss | C-6, FS-4 | Fails closed, retryable, no partial write — walked across each of the orchestrator's transactions |
| Pool exhaustion | C-6 | Refuses before opening a transaction, so the vendor was provably never called |
| **Redis loss** | C-7, FS-6 | **Correctness unchanged**, only the risk assessment records itself degraded |
| Kafka loss | C-8, FS-5 | The request path is unaffected; the outbox retains everything and drains in order |
| Pod crash mid-payment | C-14, FS-9 | No second dispatch across the client's retry |
| Worker crash mid-workflow | C-13, FS-8 | A completed step does not re-execute; the fencing epoch advances |
| Clock skew | C-20 | Webhooks outside the ±5 min tolerance are refused, in both directions |
| Retry storm | C-23 | The retry budget bounds retries to a ratio of original traffic; the adaptive limiter sheds instead of queueing |
| Network partition | C-18, FS-10 | Fails closed, heals automatically, and is **not** classified as an unknown outcome |

The Redis one is the important one. The architecture claims Redis is a non-authoritative
accelerator that can disappear without affecting correctness, and a claim like that is worth exactly
as much as the test that would fail if it stopped being true.

**Two things to know about the harness.**

- The hypothesis is sampled **at every commit**, not on a wall-clock ticker. `docs/testing.md` §6.3
  specifies a 250 ms ticker, which is right against a running system reading a database; in-process
  it would be a data race against a single-owner aggregate, and taking a lock the production code
  does not take would be testing a system that does not exist. Committing is the moment a state
  becomes observable, so every committed state is checked — a violation that self-corrected across
  two transactions is still caught. See `env.Watch`.
- No scenario sleeps. Partitions and lease expiries are expressed on injected clocks, so a
  three-minute partition costs no wall-clock time and heals at an exact instant.

**Interpreting a failure.** Every message states the money consequence, because that is what makes
the difference between a test worth fixing and a test worth deleting: *"the gateway was called 2
times, want exactly 1. A retry or a failover happened on an unknown outcome."*

A failure that reads `the fault never fired; this scenario asserted nothing` is not a product bug —
it is the scenario telling you it stopped testing anything, which is the failure mode a chaos suite
is otherwise blind to.

---

## <a id="e2e"></a>`tests/e2e` — the full stack over HTTP

**Tag: `e2e`.** Skips without the stack.

```bash
make up
export PP_TEST_BASE_URL=http://localhost:8080
export PP_TEST_SIMULATOR_URL=http://localhost:9090
export PP_TEST_AUTH_TOKEN="$(scripts/dev-token.sh)"
export PP_TEST_TENANT_ID=ten_...
go test -tags e2e ./tests/e2e/... -count=1
# Run the journey first; it prints the merchant id the payment tests want.
export PP_TEST_MERCHANT_ID=mrc_...
```

Nothing in this package imports `internal/`. That is the point of it: these tests hold themselves
to the published contract — `api/openapi/payments-platform.v1.yaml`, `api/errors/catalog.yaml`, and
the simulator's own protocol — because that contract is what the platform actually promises.

Gateway behaviour is selected **by the payment's amount**, using the simulator's documented trigger
table (`internal/adapters/gateway/simulator/scenario.go`): the last two digits of the amount in
minor units pick the scenario — `…00` approve, `…01` hard decline, `…02` soft decline, `…05` 3-D
Secure, `…07` timeout, `…12` HTTP 500. No back channel into the system under test.

| Test | What it proves |
|---|---|
| `TestMerchantJourneyFromRegistrationToSettlement` | The brief's goal in one run: registration → onboarding → validation → provisioning → certification → activation → payment → routing → gateway → webhook → ledger → settlement, asserting observable state at each stage |
| `TestATimedOutPaymentIsNeverChargedTwice` | The one that matters. Accepted-then-silent gateway: `PROCESSING`, `TIMEOUT_UNKNOWN`, no retry, reconciler resolves via lookup, **exactly one charge at the simulator** |
| `TestGatewayFailoverCreatesASecondAttemptAndOneSuccess` | Soft decline fails over to a second attempt with a different key and one success; a hard decline produces zero failover |
| `TestThreeDSChallengeCompletesAndOnlyThenCaptures` | `REQUIRES_ACTION` holds nothing: no authorization, no capture, until the payer authenticates |
| `TestPartialCaptureAndPartialRefundRespectTheInvariants` | I1 and I2 over HTTP, including the over-capture and over-refund refusals |
| `TestDisputeMovesThePaymentAndHoldsTheFunds` | A dispute is a claim: the money moves on resolution, not on the claim |

**Interpreting a failure.** Failures print the problem document, so a wrong status reads as
`got 500, want 202 — 500 INTERNAL_ERROR (server, retryable=false)`. The double-charge test is the
one to read first if several fail at once: everything else asserts that the platform does what it
should when things work, and that one asserts the case where doing the obvious thing loses
somebody's money.

A **skip** inside a running e2e suite is meaningful and is always explained: the 3-D Secure and
dispute tests skip when the simulator did not drive the corresponding callback, because a test that
cannot tell "the platform mishandled the notification" from "no notification was sent" must not
assert either.

---

## <a id="load"></a>`tests/load` — k6

See [`tests/load/README.md`](load/README.md). Point it at staging, never at production. Each
scenario carries its SLO assertions as k6 thresholds, so the run **fails** rather than producing a
chart somebody has to interpret.

---

## Conventions every test here follows

| Rule | Why |
|---|---|
| **No `time.Sleep` as a synchronisation primitive.** Channels, `testenv.Eventually`, `testenv.Consistently`, or an injected clock | A sleep is a bet that CI is no slower than the machine the test was written on. It loses that bet on the busy day, producing a failure that looks like a product bug and gets re-run away |
| **Fixed clocks and seeded identifiers** | The same test produces the same tenant on every run, so a failure leaves rows a human can find. The one exception is documented: identifiers written to tables where `DELETE` is revoked carry a per-run token, because retention there is a partition `DETACH` and a deterministic id would collide with the previous run's row |
| **`t.Parallel()` wherever no mutable state is shared** | It makes the isolation properties under test actually get exercised. Where a test is *not* parallel, the comment says which shared resource it owns |
| **Table-driven, with the negative case in the table** | The positive case is nearly free of information; the row that must be *rejected* is the one that fails when a constraint is dropped |
| **Every test names the requirement it verifies** | A `Verifies:` line citing the baseline section, the ADR or the `docs/testing.md` scenario. A test whose purpose has to be reconstructed is a test that gets deleted in the next cleanup |
| **Cleanup is asserted** | `testenv.Isolate` deletes, then checks it deleted, then checks the shared namespace did not move |
| **Failure messages state the consequence** | `want 1, got 2` says what happened; *"every charge after the first is money taken from a cardholder for one purchase"* says why anyone should care |

---

## CI

| Trigger | Suites | Budget |
|---|---|---|
| Every push | `go vet` under all four tags; `tests/contract`; `tests/chaos` | ~1 min |
| Pull request | + `tests/integration` against service containers | ~2 min |
| Merge to `main` | + `tests/e2e` against the deployed dev stack | ~8 min |
| Nightly | + `-count=5 -race` repeat run; the `PP_TEST_CHAOS_INFRA` scenarios | ~30 min |
| Pre-release | + all four k6 scenarios including the 4 h soak | ~5 h |

CI retries **nothing** automatically. A re-run is a manual action and is recorded against the test,
because a flaky test in the money path is either a wrong test or a nondeterministic money path, and
"it passes on re-run" is a description of a race condition rather than a resolution
(`docs/testing.md` §9.3).
