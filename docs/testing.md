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

| Level | Count (approx) | Share | Runtime | Runs on | Dependencies |
|---|---|---|---|---|---|
| Unit (pure domain, application with fakes) | ~3 400 | 68 % | < 20 s total | every push | none |
| Integration (real Postgres/Redis/Kafka) | ~640 | 13 % | ~6 min | every PR | testcontainers |
| Contract (gateway adapters, event schemas, OpenAPI) | ~380 | 8 % | ~90 s | every PR | simulator + schema registry |
| API / spec-driven | ~210 | 4 % | ~2 min | every PR | in-process server |
| Workflow | ~120 | 2.5 % | ~3 min | every PR | testcontainers |
| State machine (property/exhaustive) | ~40 files, ~10⁶ generated cases | 1 % | ~45 s | every PR | none |
| Security | ~150 | 3 % | ~2 min | every PR | testcontainers |
| E2E | ~45 | 0.9 % | ~8 min | merge to main | full stack |
| Chaos | ~28 scenarios | 0.5 % | ~25 min | nightly + pre-release | full stack + fault injection |
| Load (k6) | 4 scenarios | — | 15 min – 4 h | nightly / pre-release | staging |

The shape is conventional; the reasoning for *this* system is not. The domain layer is stdlib-only by construction (§4), which makes the FSMs, `Money`, the validation rules, the routing policy evaluation and the idempotency fingerprinting all testable with zero infrastructure. That is a deliberate architectural choice made *so that* the base of the pyramid can be wide and fast. Where the domain is pure, unit tests are cheap enough to be exhaustive; where it is not, no amount of mocking makes an integration test unnecessary.

### 1.1 Coverage gates

| Scope | Gate | Enforced by |
|---|---|---|
| `internal/domain/**` | **95 %** statements | `scripts/coverage.sh` |
| `internal/application/**` | 90 % | same |
| `internal/validation/**` | 95 % | same |
| `internal/platform/idempotency`, `internal/platform/tenantctx` | **100 %** | same |
| `internal/workflows/**` | 85 % | same |
| `internal/adapters/gateway/**` | 80 % + the full contract suite | same |
| `internal/infrastructure/**` | 70 % | same |
| Repository overall | 80 % | same |
| **Any package, any PR** | Coverage may not **decrease** | delta check vs `main` |

### 1.2 Why coverage percentage is a bad gate, and what is used instead

Coverage measures which lines executed. It says nothing about whether anything was asserted. A test that calls every method and asserts `err == nil` yields 100 % coverage and detects approximately nothing. Worse, a coverage gate creates pressure to write exactly that kind of test — the metric becomes the target, and the suite fills with executions masquerading as verifications.

The gates above are therefore a **floor against neglect**, not a measure of quality. The actual quality gate is a list of **critical-path assertions**: named, individually-required properties, each traced to a baseline section, each of which must be covered by a test that fails when the property is broken. It is registered in `tests/critical_paths.yaml` and verified by `TestEveryCriticalPathIsAsserted`.

```yaml
# tests/critical_paths.yaml  (excerpt — 47 entries)
- id: CP-01
  property: "A second successful attempt for one payment is rejected by the database"
  baseline: "§9 I3"
  tests: ["tests/integration/invariants_test.go::TestTwoSuccessfulAttemptsRejectedByDatabase"]
  mutation_probes: ["drop_partial_unique_index", "widen_index_to_all_outcomes"]
- id: CP-02
  property: "A timed-out gateway call never fails the payment"
  baseline: "§12.3, §1.3 A7"
  tests: ["tests/chaos/gateway_timeout_test.go::TestTimeoutLeavesPaymentProcessingAndReconciles"]
  mutation_probes: ["timeout_maps_to_FAILED", "timeout_triggers_failover"]
- id: CP-03
  property: "sum(refunds) can never exceed captured_amount"
  baseline: "§9 I1"
  tests: ["internal/domain/payment/refund_test.go::TestRefundCannotExceedCaptured",
          "tests/integration/invariants_test.go::TestRefundOverdraftRejectedByConstraint"]
  mutation_probes: ["check_uses_gte", "check_removed"]
- id: CP-07
  property: "A repository call without tenant context returns ErrMissingTenantContext, never rows"
  baseline: "§16.2"
  tests: ["tests/integration/tenancy_test.go::TestCrossTenantAccessIsImpossible"]
  mutation_probes: ["guard_removed", "rls_policy_dropped"]
```

**Mutation-adjacent verification.** Full mutation testing of a Go codebase this size is too slow for PR CI. Instead, each critical path declares named `mutation_probes` — deliberate, scripted breakages applied to a scratch copy of the tree. `scripts/mutation-probe.sh` applies each probe, runs *only* the named tests, and asserts they **fail**. A probe whose tests still pass means the property is unasserted, and CI fails with the exact critical-path ID.

```bash
# Runs nightly and on any PR touching internal/domain/** or migrations/**
scripts/mutation-probe.sh --paths tests/critical_paths.yaml --fail-on-survivor
# CP-01 drop_partial_unique_index      → tests FAILED  ✓ (property is asserted)
# CP-01 widen_index_to_all_outcomes    → tests FAILED  ✓
# CP-02 timeout_maps_to_FAILED         → tests FAILED  ✓
# CP-03 check_uses_gte                 → tests PASSED  ✗ SURVIVOR — CP-03 is not asserted
```

This is a fraction of the cost of full mutation testing and targets precisely the properties that matter. It also produces the artifact that makes an audit conversation short: a list of the money-safety properties, and the evidence that each one is verified by a test that demonstrably fails when the property is removed.

---

## 2. Unit tests

Location: alongside the code (`internal/**/*_test.go`). Build tag: none. No network, no filesystem, no clock, no randomness.

### 2.1 Pure domain

```go
// internal/domain/payment/state_test.go — table-driven, the default shape
func TestPaymentTransitions(t *testing.T) {
    tests := []struct {
        name    string
        from    State
        to      State
        wantErr error
    }{
        {"created to processing",        StateCreated,   StateProcessing, nil},
        {"created to captured is illegal", StateCreated, StateCaptured,   ErrInvalidTransition},
        {"settled to processing is illegal", StateSettled, StateProcessing, ErrInvalidTransition},
        {"refunded to captured is illegal",  StateRefunded, StateCaptured,  ErrInvalidTransition},
        {"failed is terminal",           StateFailed,    StateProcessing, ErrInvalidTransition},
        {"processing to pending",        StateProcessing, StatePending,   nil},
        {"pending to authorized",        StatePending,   StateAuthorized, nil},
        {"disputed back to captured",    StateDisputed,  StateCaptured,   nil},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel()
            p := newPaymentInState(t, tt.from)
            err := p.TransitionTo(tt.to, testReason)
            require.ErrorIs(t, err, tt.wantErr)
            if tt.wantErr == nil {
                require.Equal(t, tt.to, p.State())
                require.Len(t, p.UncommittedEvents(), 1)     // I5: exactly one event per change
                require.Equal(t, uint64(2), p.Version())     // optimistic concurrency advances
            } else {
                require.Equal(t, tt.from, p.State())         // no partial mutation on rejection
                require.Empty(t, p.UncommittedEvents())
            }
        })
    }
}
```

The two assertions in the failure branch matter more than the one in the success branch: a rejected transition must leave the aggregate **entirely** unchanged, including its event list. An FSM that mutates and then errors is the source of the "impossible state in production" class of bug.

### 2.2 Property-based tests for the FSMs

Exhaustive where the state space allows (§3), property-based where it does not.

```go
// internal/domain/payment/state_property_test.go
func TestFSMProperties(t *testing.T) {
    rapid.Check(t, func(t *rapid.T) {
        p := newPayment(t, rapid.SampledFrom(currencies).Draw(t, "ccy"))
        applied := []State{p.State()}

        for i := 0; i < rapid.IntRange(1, 40).Draw(t, "steps"); i++ {
            next := rapid.SampledFrom(allStates).Draw(t, "next")
            before := p.State()
            err := p.TransitionTo(next, testReason)

            if err != nil {
                // P1: a rejected transition is a no-op
                require.Equal(t, before, p.State())
                continue
            }
            // P2: every accepted transition is in the §9 table
            require.True(t, transitionTable[before][next],
                "accepted an undocumented transition %s -> %s", before, next)
            // P3: terminal states never transition
            require.False(t, isTerminal(before), "escaped terminal state %s", before)
            // P4: version is strictly monotonic (I5)
            require.Greater(t, p.Version(), uint64(len(applied)))
            applied = append(applied, next)
        }
        // P5: replaying the event log reconstructs an identical aggregate
        replayed := ReplayPayment(p.ID(), p.AllEvents())
        require.Equal(t, p.State(), replayed.State())
        require.Equal(t, p.Version(), replayed.Version())
        require.Equal(t, p.CapturedAmount(), replayed.CapturedAmount())
        // P6: refunded never exceeds captured (I1) at any reachable state
        require.LessOrEqual(t, p.RefundedTotal().Amount(), p.CapturedAmount().Amount())
    })
}
```

The same shape covers the merchant lifecycle (§8) including Amendment A-01's `COMPLIANCE_REJECTED` exits, the attempt FSM (§9.1), and the gateway health FSM (§10) — for which the property is that cool-down doubles monotonically and is capped at 5 minutes.

### 2.3 Property-based tests for Money

`Money` (§7) is where a property-based test earns its keep, because the failure modes are arithmetic and adversarial inputs find them faster than examples do.

```go
// internal/domain/shared/money_property_test.go
func TestMoneyProperties(t *testing.T) {
    rapid.Check(t, func(t *rapid.T) {
        ccy := rapid.SampledFrom([]Currency{USD, EUR, JPY, BHD, CLF}).Draw(t, "ccy")
        a := rapid.Int64Range(0, 1<<50).Draw(t, "a")
        b := rapid.Int64Range(0, 1<<50).Draw(t, "b")
        ma, mb := MustNew(a, ccy), MustNew(b, ccy)

        // Commutativity and associativity
        s1, _ := ma.Add(mb); s2, _ := mb.Add(ma)
        require.Equal(t, s1, s2)

        // Identity and inverse
        z := MustNew(0, ccy)
        r, _ := ma.Add(z);  require.Equal(t, ma, r)
        d, _ := ma.Sub(ma); require.Equal(t, z, d)

        // Currency mismatch is an error, never a panic and never a conversion (§7 rule 3)
        if ccy != JPY {
            _, err := ma.Add(MustNew(b, JPY))
            require.ErrorIs(t, err, ErrCurrencyMismatch)
        }

        // Largest-remainder allocation: parts always sum to the whole (§7 rule 4)
        n := rapid.IntRange(1, 17).Draw(t, "parts")
        parts, err := ma.Allocate(equalRatios(n))
        require.NoError(t, err)
        total := MustNew(0, ccy)
        for _, p := range parts { total, _ = total.Add(p) }
        require.Equal(t, ma, total, "allocation lost or created %d minor units", ma.Amount()-total.Amount())
        // and no part differs from another by more than one minor unit
        require.LessOrEqual(t, maxOf(parts).Amount()-minOf(parts).Amount(), int64(1))

        // Serialization round-trip is exact and integral (§7 rule 5)
        js, _ := json.Marshal(ma)
        require.NotContains(t, string(js), ".")
        var back Money
        require.NoError(t, json.Unmarshal(js, &back))
        require.Equal(t, ma, back)

        // Overflow is an error, not a wrap
        _, err = MustNew(math.MaxInt64, ccy).Add(MustNew(1, ccy))
        require.ErrorIs(t, err, ErrAmountOverflow)
    })
}
```

The allocation property is the one that catches real bugs: a fee split of `1000` across `3` that produces `333/333/333` loses a cent per transaction, which becomes a reconciliation exception a week later and an unexplained ledger imbalance a month later.

### 2.4 Application layer

Use cases are tested against **fakes**, not mocks: an in-memory repository implementing the real port with the real semantics (including optimistic-concurrency conflicts and unique-constraint violations), a controllable `Clock`, a deterministic `IDGenerator`, and a scriptable gateway port.

The distinction is load-bearing. A mock asserts "the repository's `Save` was called once with these arguments" — which passes even when `Save` would fail in reality. A fake actually stores, actually conflicts, actually rejects a duplicate. Tests written against fakes catch the concurrency bugs; tests written against mocks catch refactorings.

---

## 3. State machine tests

Both FSMs get an **exhaustive transition matrix** test, not a sampled one. The payment FSM has 15 states; the merchant FSM has 21. Full matrices are 225 and 441 cases — trivially exhaustive, and the exhaustiveness is the point.

```go
// internal/domain/payment/matrix_test.go
func TestExhaustiveTransitionMatrix(t *testing.T) {
    // The expected matrix is transcribed directly from baseline §9 and is the
    // ONLY place it is encoded in test code. If the table and the implementation
    // agree, they agree with the spec.
    for _, from := range AllStates {
        for _, to := range AllStates {
            from, to := from, to
            t.Run(fmt.Sprintf("%s->%s", from, to), func(t *testing.T) {
                t.Parallel()
                want := specTable[from][to]          // from §9, transcribed once
                p := newPaymentInState(t, from)
                err := p.TransitionTo(to, testReason)
                if want {
                    require.NoError(t, err)
                    require.Equal(t, to, p.State())
                } else {
                    require.ErrorIs(t, err, ErrInvalidTransition)
                    require.Equal(t, from, p.State())
                }
            })
        }
    }
}

// Guards are separate: a transition can be in the table and still be refused
// because a guard fails (§8: "-> ACTIVE requires a CERTIFIED connection, ...").
func TestActivationGuards(t *testing.T) {
    cases := []struct{ name string; setup func(*Merchant); wantErr error }{
        {"no certified connection", withNoCertifiedConnection, ErrNoCertifiedGateway},
        {"empty configuration", withEmptyConfig, ErrConfigurationRequired},
        {"missing compliance attestation", withoutAttestation, ErrComplianceAttestationRequired},
        {"open critical reconciliation exception", withCriticalException, ErrOpenReconciliationException},
        {"all guards satisfied", fullySatisfied, nil},
    }
    // ...
}

// §8: "-> TERMINATED requires zero payments in a non-terminal state"
func TestTerminationBlockedByInFlightPayments(t *testing.T) { /* ... */ }

// §8: suspension permits money OUT but not money IN
func TestSuspendedMerchantRejectsPaymentsButAllowsRefunds(t *testing.T) {
    m := merchantInState(t, StateSuspended)
    require.ErrorIs(t, m.AuthorizeNewPayment(ctx), ErrMerchantNotActive)
    require.NoError(t, m.PermitRefund(ctx))
    require.NoError(t, m.PermitVoid(ctx))
    require.NoError(t, m.PermitWebhookProcessing(ctx))
}
```

A structural test closes the loop: `TestSpecTableMatchesDocumentation` parses the transition tables out of `docs/spec/00-design-baseline.md` §8 and §9 and asserts they match `specTable` exactly. The spec and the tests cannot drift, because drifting fails the build.

---

## 4. Integration tests

Build tag `integration`. Real Postgres 15, Redis 7, Kafka (Redpanda for speed, MSK-compatible), via testcontainers.

```go
//go:build integration

// tests/integration/main_test.go — one container set per package, reused across tests
func TestMain(m *testing.M) {
    ctx := context.Background()
    stack := testenv.Start(ctx, testenv.WithPostgres(15), testenv.WithRedis(7), testenv.WithRedpanda())
    defer stack.Terminate(ctx)
    if err := stack.Migrate("migrations/"); err != nil { log.Fatal(err) }
    os.Exit(m.Run())
}
```

| Property | Choice | Reasoning |
|---|---|---|
| Container lifetime | Per package, not per test | Startup dominates; per-test containers make the suite 20× slower. Isolation comes from per-test schemas/prefixes, not per-test containers |
| Isolation | Each test runs in a transaction rolled back at the end **or**, where a transaction is the thing under test, in a dedicated schema `test_<ulid>` dropped in `t.Cleanup` | A rolled-back transaction cannot test commit-time constraint behaviour, so the second mode exists |
| Migrations | The **real** `migrations/` directory, applied by the **real** runner | A test schema built from a hand-written DDL file proves nothing about production. This is also how migration bugs are caught before deploy |
| Parallelism | `t.Parallel()` everywhere, with per-test tenant IDs | Forces the tests to be isolation-correct, which is the property under test anyway |
| Fixtures | Builders (§8), never shared mutable globals | |

### 4.1 What integration tests assert that unit tests cannot

| Area | Example test |
|---|---|
| Constraint enforcement | `TestTwoSuccessfulAttemptsRejectedByDatabase` — insert directly, bypassing the domain, assert a unique violation on the I3 partial index |
| Partition alignment (A-02) | `TestAttemptCreatedNextMonthSharesPaymentPartition` — create a payment in month M, an attempt in M+1, assert both are in the same partition and I3 still holds |
| Optimistic concurrency | `TestConcurrentCaptureOnlyOneWins` — 16 goroutines capture the same payment; exactly one succeeds, 15 get `ErrVersionConflict` |
| Idempotency claim | `TestConcurrentClaimsSecondGets409` — two goroutines, same key; one proceeds, one gets `IDEMPOTENT_REQUEST_IN_PROGRESS` (§1.3 A6) |
| Idempotency fingerprint | `TestSameKeyDifferentBodyIs422` (§14.2) |
| Lease reclaim | `TestExpiredLeaseIsReclaimedAtomically` — 8 goroutines race to reclaim; exactly one wins |
| RLS | `TestCrossTenantAccessIsImpossible` — set `app.tenant_id` to A, query B's row by primary key, assert **zero rows at the database level** with the app guard disabled |
| Outbox | `TestStateAndOutboxCommitAtomically` — inject a failure between the state write and the outbox write, assert neither is visible |
| Relay | `TestRelaySkipLockedNoDoublePublish` — 4 relays over 10 000 rows; every row published exactly once |
| Dedup | `TestDuplicateEventIsDroppedByDedupTable` (§13.5) |
| Consumer transactionality | `TestConsumerHandlerAndDedupCommitTogether` — handler panics after the dedup insert; assert the dedup row is rolled back so the message is reprocessed |
| Cache fallback | `TestIdempotencyIsCorrectWithRedisDown` — kill Redis mid-suite, assert correctness holds and only latency degrades |

### 4.2 Tenant isolation negative tests

The isolation guard (§16.2) gets adversarial treatment, because it is a security boundary and a passing positive test proves nothing about it.

```go
func TestCrossTenantAccessIsImpossible(t *testing.T) {
    a, b := seedTenant(t, "A"), seedTenant(t, "B")
    payB := seedPayment(t, b)

    t.Run("repository with tenant A context cannot read tenant B's payment", func(t *testing.T) {
        ctx := tenantctx.With(context.Background(), a.ID)
        _, err := repo.GetPayment(ctx, payB.ID)
        require.ErrorIs(t, err, domain.ErrNotFound)   // NOT ErrForbidden — existence is not disclosed
    })

    t.Run("RLS blocks it even with the application guard removed", func(t *testing.T) {
        // Raw connection as the app role (NOT BYPASSRLS), no repository layer at all.
        conn := stack.RawAppConn(t)
        _, err := conn.Exec(ctx, "SET LOCAL app.tenant_id = $1", a.ID)
        require.NoError(t, err)
        rows, err := conn.Query(ctx, "SELECT id FROM payments WHERE id = $1", payB.ID)
        require.NoError(t, err)
        require.False(t, rows.Next(), "RLS did not block cross-tenant read")
    })

    t.Run("no tenant in context returns an error, never rows", func(t *testing.T) {
        _, err := repo.GetPayment(context.Background(), payB.ID)
        require.ErrorIs(t, err, domain.ErrMissingTenantContext)
    })

    t.Run("tenant_id in the body is ignored, mismatch is a security event", func(t *testing.T) {
        resp := postJSON(t, tokenFor(a), "/v1/payments", map[string]any{
            "tenantId": b.ID, "amount": 1000, "currency": "USD", /* ... */
        })
        require.Equal(t, 403, resp.Code)
        require.Equal(t, "TENANT_MISMATCH", problemCode(t, resp))
        requireSecurityEventRaised(t, "TENANT_MISMATCH", a.ID)
    })

    t.Run("cache keys are tenant-prefixed and cannot collide", func(t *testing.T) { /* pp:{tenant}:… */ })
    t.Run("S3 prefix condition denies cross-tenant object access", func(t *testing.T) { /* ... */ })
    t.Run("Kafka consumer filters reject an event with a foreign tenantid", func(t *testing.T) { /* ... */ })
}
```

The generated variant runs every repository method (there are 94) under a foreign tenant context and asserts each returns `ErrNotFound` or `ErrMissingTenantContext` and never a row. A new repository method with a missing guard fails this test on the day it is written — which is the only time fixing it is cheap.

---

## 5. Contract, API, and workflow tests

### 5.1 The shared gateway adapter suite

Every adapter under `internal/adapters/gateway/` — Stripe, Adyen, PayPal, and the simulator — must pass the identical suite. It is the executable definition of the `GatewaySPI` port, and it is what makes "adding a gateway" a bounded piece of work.

```go
// internal/adapters/gateway/contract/suite.go — exported, run by each adapter's test
func RunAdapterContractSuite(t *testing.T, newAdapter func(t *testing.T) gateway.SPI) {
    t.Run("Authorize/Capture/Refund round-trip", ...)
    t.Run("Authorize then Void", ...)
    t.Run("Sale (auto-capture) reaches CAPTURED directly", ...)
    t.Run("Partial capture up to the authorized amount", ...)
    t.Run("Capture above authorized is rejected before dispatch", ...)
    t.Run("Refund up to captured; over-refund rejected", ...)
    t.Run("Hard decline maps to DECLINED with a normalized reason and is NOT retryable", ...)
    t.Run("Soft decline maps to DECLINED with a retryable reason", ...)
    t.Run("5xx maps to ERROR and is retryable", ...)
    t.Run("Timeout maps to TIMEOUT_UNKNOWN and is NEVER retryable", ...)
    t.Run("Same gateway idempotency key returns the same result, no second charge", ...)
    t.Run("Different attempt id yields a different key", ...)
    t.Run("Amount and currency echoed by the gateway must match what was sent (L6)", ...)
    t.Run("Response failing L6 yields GATEWAY_CONTRACT_VIOLATION, never a state change", ...)
    t.Run("Webhook signature verification: valid accepted, tampered rejected", ...)
    t.Run("Webhook replay outside the 5-minute window is rejected", ...)
    t.Run("3DS challenge yields REQUIRES_ACTION with a completion URL", ...)
    t.Run("Capability descriptor matches observed behaviour", ...)
    t.Run("No PAN, token or credential appears in any log line or span attribute", ...)
    t.Run("Context cancellation aborts within 100ms and records TIMEOUT_UNKNOWN", ...)
    t.Run("Money is never float: all amounts are integer minor units on the wire", ...)
}
```

```go
// internal/adapters/gateway/adyen/adapter_contract_test.go
func TestAdyenAdapterContract(t *testing.T) {
    contract.RunAdapterContractSuite(t, func(t *testing.T) gateway.SPI {
        return adyen.New(adyen.Config{BaseURL: recordedServer(t, "testdata/adyen"), /* ... */})
    })
}
```

Two execution modes: against **recorded fixtures** (`go-vcr` cassettes, deterministic, runs in PR CI) and against **live sandbox** accounts (nightly, catches gateway-side contract drift before it reaches a merchant). Fixtures are re-recorded weekly by an automated job; a diff in the recording fails the job and requires a human to look at what the gateway changed.

The certification suite of §11.4 is this same suite, parameterized by `(gateway, payment_method, currency)`, run against sandbox during onboarding step 10. The identical assertions gate a merchant's `PRODUCTION_READY` and gate our own CI — which is why "certified" is an artifact rather than an opinion.

### 5.2 Consumer-driven contracts for events

Each consumer declares what it needs from an event type; the producer's CI verifies it still provides it.

```yaml
# tests/contract/consumers/ledger/payment.captured.v1.yaml
consumer: ledger
event: payment.captured.v1
requires:
  envelope: [id, type, subject, time, tenantid, merchantid, aggregateid, aggregateversion, traceparent]
  data:
    - { path: paymentId,        type: string, pattern: "^pay_[0-9A-HJKMNP-TV-Z]{26}$" }
    - { path: capturedAmount.amount,   type: integer, minimum: 1 }
    - { path: capturedAmount.currency, type: string,  pattern: "^[A-Z]{3}$" }
    - { path: gatewayId,        type: string }
    - { path: attemptId,        type: string }
  invariants:
    - "capturedAmount.amount is integer minor units, never a decimal string or float"
    - "aggregateversion increases monotonically per aggregateid"
```

`TestAllConsumerContractsSatisfied` generates an instance of every producer's event from the registered schema and validates it against every registered consumer contract. Adding a required field, tightening a type, or removing a field inside a major version fails the build (§13.1: additive-only within a major). The complementary `TestNoConsumerReadsAnUndeclaredField` runs consumers against an event stripped to exactly its declared fields; a consumer that reads something undeclared fails, which prevents the undeclared dependency that turns a legal additive change into a production incident.

### 5.3 API tests: spec-driven validation

Every request and response in the API suite is validated against `api/openapi/*.yaml` by middleware installed only in tests. There is no hand-written expectation of the response shape.

```go
func TestCreatePaymentConformsToSpec(t *testing.T) {
    srv := newTestServer(t, withOpenAPIValidation("api/openapi/data-plane.yaml"))
    resp := srv.POST("/v1/payments").
        WithHeader("Idempotency-Key", "k-"+ulid.Make().String()).
        WithJSON(builders.PaymentRequest().WithAmount(1050, "USD").Build()).
        Expect()
    resp.Status(201)                        // validated against the spec's declared responses
    resp.Header("Location").NotEmpty()
    resp.JSON().Path("$.amount").IsEqual(1050)      // integer minor units, §7 rule 5
    resp.JSON().Path("$.status").IsEqual("processing")
    // The middleware has already asserted: no undocumented fields, every required
    // field present, every type and format correct, the status code declared.
}

func TestEveryErrorIsProblemJSON(t *testing.T) {
    for _, tc := range errorCases {          // one per §20.2 reserved code
        resp := tc.invoke(t, srv)
        resp.ContentType("application/problem+json")
        resp.JSON().Path("$.code").IsEqual(tc.wantCode)
        resp.JSON().Path("$.category").IsEqual(tc.wantCategory)
        resp.JSON().Path("$.retryable").IsEqual(tc.wantRetryable)
        resp.JSON().Path("$.traceId").NotEmpty()
        resp.JSON().Path("$.requestId").NotEmpty()
    }
}

// §26: every reserved error code is reachable and documented
func TestEveryReservedCodeIsExercised(t *testing.T) { /* reads api/errors/catalog.yaml */ }
```

Additional spec-driven checks: `TestSpecHasNoBreakingChange` (diff against `main`), `TestEveryEndpointRequiresAuth` (enumerates paths from the spec and asserts `401` without a token), `TestEveryMutatingEndpointRequiresIdempotencyKey` (§14.1), `TestCursorPaginationIsStableUnderConcurrentWrites`, `TestETagAndIfMatchSemantics` (§19.3).

### 5.4 Workflow tests

```go
// tests/integration/workflow_test.go

// Resume after crash — the central guarantee of §11
func TestWorkflowResumesAfterWorkerCrash(t *testing.T) {
    w := startWorker(t)
    inst := startOnboarding(t, merchant)
    waitForStep(t, inst, "provision-gateways")
    calls := gatewaySim.CallCount("Provision")

    w.Kill()                                   // SIGKILL — no graceful shutdown, no lease release
    w2 := startWorker(t)                       // a different worker
    waitForState(t, inst, WorkflowCompleted)

    // No completed step is replayed (§11 semantics)
    require.Equal(t, calls, gatewaySim.CallCount("Provision"))
    require.Equal(t, 1, gatewaySim.CallCount("RegisterWebhook"))
    require.Equal(t, []string{"validate-merchant", "submit-kyc", /* ... */ "activate"},
        completedStepsOf(t, inst))             // each exactly once, in order
    require.Equal(t, w2.ID, leaseHolderOf(t, inst))
}

// Compensation ordering — strict reverse order (§11)
func TestCompensationsRunInStrictReverseOrder(t *testing.T) {
    gatewaySim.FailStep("apply-configuration", errPermanent)
    inst := startOnboarding(t, merchant)
    waitForState(t, inst, WorkflowFailed)

    require.Equal(t,
        []string{"delete-webhook-registration", "delete-secret-version",
                 "deprovision-subaccount", "cancel-kyc-case"},
        compensationOrderOf(t, inst))
    require.Empty(t, gatewaySim.OrphanedResources())
    require.Equal(t, 1, dlqDepth(t, "workflow_dlq"))
    require.Contains(t, dlqEntry(t, inst).ErrorChain, "apply-configuration")
}

// Idempotent replay — a redelivered signal or a duplicated activity must be harmless
func TestIdempotentReplayOfEveryStep(t *testing.T) {
    for _, step := range allSteps {
        t.Run(step, func(t *testing.T) {
            inst := runToStep(t, step)
            before := externalSideEffectsSnapshot(t)
            replayStep(t, inst, step)          // deliver the same activity result twice
            require.Equal(t, before, externalSideEffectsSnapshot(t))
        })
    }
}

// Manual gate (§11 step 11) — blocks, requires authorization, is audited
func TestManualComplianceGate(t *testing.T) {
    inst := runToStep(t, "compliance-review")
    requireStaysBlocked(t, inst, 3*time.Second)

    // Unauthorized principal cannot signal
    err := signal(t, inst, "approve", principalWithout("onboarding:approve"))
    require.ErrorIs(t, err, ErrForbidden)
    requireStaysBlocked(t, inst, time.Second)

    // Authorized principal can, and the signal is audited
    require.NoError(t, signal(t, inst, "approve", principalWith("onboarding:approve")))
    waitForState(t, inst, WorkflowCompleted)
    requireAuditRecord(t, inst.MerchantID, "onboarding.compliance_approved",
        withActor("principal:compliance:carol"))
}

// Amendment A-01: a compliance rejection must be representable and must route correctly
func TestComplianceRejectionRoutesBackToConfiguring(t *testing.T) {
    inst := runToStep(t, "compliance-review")
    require.NoError(t, signal(t, inst, "reject", principalWith("onboarding:approve"),
        withReasonCode("PROHIBITED_MCC_COUNTRY")))
    requireMerchantState(t, merchant, StateComplianceRejected)
    require.NoError(t, transitionMerchant(t, merchant, StateConfiguring))
}

// Business key: starting twice is a no-op returning the existing instance (§11)
func TestStartingOnboardingTwiceIsANoOp(t *testing.T) {
    a := startOnboarding(t, merchant)
    b := startOnboarding(t, merchant)
    require.Equal(t, a.ID, b.ID)
    require.Equal(t, 1, workflowInstanceCount(t, merchant))
}
```

---

## 6. Security, performance and chaos

### 6.1 Security tests

| Area | Tests |
|---|---|
| **Authn negative matrix** | No token; malformed token; expired token; not-yet-valid (`nbf`); wrong `iss`; wrong `aud`; unknown `kid`; revoked key; token signed by a key from another environment; token whose signature is valid but truncated |
| **JWT attacks** | `alg: none` (accepted → immediate build failure); `alg: HS256` signed with the RSA **public key** as the HMAC secret (key confusion); `kid` path traversal (`../../etc/passwd`); `kid` SQL injection; embedded `jwk`/`jku` header pointing at an attacker URL; nested JWT; oversized token (10 MB) causing a DoS; claim-type confusion (`aud` as an array vs string) |
| **Authz negative matrix** | For every endpoint × every scope in §19: assert exactly the documented scope grants access and every other scope yields `403`. Generated from the OpenAPI security definitions, so a new endpoint without a scope declaration fails |
| **Cross-tenant** | §4.2, plus: a token for tenant A with a path parameter for tenant B's merchant; a webhook signed by tenant B's gateway secret targeting tenant A; a cursor from tenant A's pagination replayed under tenant B |
| **Injection** | SQL injection over every string input (payload corpus); the ULID parser fed adversarial input; JSON depth bombs; duplicate JSON keys; `__proto__`; Unicode normalization confusables in merchant names; header injection via `X-Request-Id`; CRLF in log-bound fields (asserts the JSON serializer escapes rather than breaks the line) |
| **PAN detector** (§17.2) | Valid Luhn 13–19 digits in every string field, in nested objects, in array elements, with separators (`-`, space, `.`), split across adjacent fields (must **not** trip — a false positive that rejects legitimate traffic is also a failure), Unicode digits, and — critically — `TestPANDetectorNeverLogsTheValue`, which asserts the rejected value appears in no log line, span attribute or error body |
| **Redaction** | `TestSecretNeverSerializes` fuzzes every `fmt` verb over `Secret[T]`; `TestAccessLogHeaderAllowlist`; `TestNoRequestStructIsEverFormatted` (AST-level) |
| **Rate limit** | Per-tenant and per-merchant limits enforced; `429` carries `Retry-After` and `RateLimit-*` (§19.3); a tenant exceeding its limit does **not** affect another tenant's success rate (the bulkhead assertion); limits still apply with Redis down, via the local fallback |
| **Webhook security** | Valid signature accepted; tampered body rejected; replayed timestamp beyond ±5 min rejected; nonce reuse rejected; signature from the wrong gateway rejected; unsigned rejected; timing-safe comparison verified by a statistical timing test |
| **Transport** | TLS < 1.3 refused externally; mTLS required between services; plaintext Postgres/Redis/Kafka connections refused |

```go
func TestJWTAlgNoneIsRejected(t *testing.T) {
    tok := forgeToken(t, map[string]any{"alg": "none", "typ": "JWT"},
        map[string]any{"sub": "attacker", "tenant_id": victimTenant, "scope": "payments:write"})
    resp := srv.POST("/v1/payments").WithHeader("Authorization", "Bearer "+tok).Expect()
    resp.Status(401)
    require.Equal(t, "UNAUTHENTICATED", problemCode(t, resp))
    requireSecurityEventRaised(t, "INVALID_TOKEN_ALGORITHM", "")
}

func TestJWTKeyConfusionIsRejected(t *testing.T) {
    pub := jwks.PublicKeyPEM(t)                       // the RS256 verification key
    tok := signHS256(t, pub, claimsFor(victimTenant)) // use it as an HMAC secret
    srv.POST("/v1/payments").WithHeader("Authorization", "Bearer "+tok).Expect().Status(401)
}
```

### 6.2 Performance and load (k6)

Location `tests/load/`. Run against staging with production-shaped data (§6.1 of `deployment.md`). Each scenario carries its SLO assertions as k6 thresholds, so the load test **fails** rather than producing a chart someone has to interpret.

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

Location `tests/chaos/`. Each scenario maps to a §24 failure-catalog entry, uses a named fault-injection mechanism, and states a **steady-state hypothesis** — the property that must hold throughout, not merely be restored afterwards.

| # | Scenario | §24 entry | Injection mechanism | Steady-state hypothesis |
|---|---|---|---|---|
| C-1 | Gateway timeout | Gateway timeout | Simulator holds the connection past 8 s | No payment reaches `FAILED`; every affected payment is `PROCESSING` with a `TIMEOUT_UNKNOWN` attempt; **zero second dispatches**; all resolve within 15 min |
| C-2 | Gateway 5xx storm | Gateway 5xx | Simulator returns 503 for 60 s | Retries ≤ 2 on the same attempt with the same gateway key; then failover as a *new* attempt; auth rate recovers within 2 min; no duplicate authorizations |
| C-3 | Gateway hard decline | Hard decline | Simulator returns a stolen-card code | Terminal `FAILED`; **zero failover attempts** (card-testing behaviour) |
| C-4 | Gateway sustained errors | Sustained errors | 40 % error rate for 5 min | Circuit opens within 30 s of crossing 25 %; `gateway.health_changed.v1` published; routing shifts; circuit half-opens after cool-down; cool-down doubles on probe failure, capped at 5 min |
| C-5 | All gateways unhealthy | All unhealthy | All simulators fail | `503 NO_ELIGIBLE_GATEWAY` with `Retry-After`; **fail closed**; zero payments created in an indeterminate state |
| C-6 | Postgres primary loss | Postgres primary loss | `aws rds failover-db-cluster` (staging) / pause the container (local) | Readiness fails, liveness holds (**no mass restart**); writes reject `503` (retryable); reads served from replicas; recovery ≤ 60 s; zero lost committed writes |
| C-7 | Redis total loss | Redis loss | Kill the cluster | Correctness unchanged; idempotency falls back to Postgres; rate limits fall back to local buckets; p99 rises but stays < 400 ms; **zero duplicate payments** |
| C-8 | Kafka unavailable | Kafka loss | Stop brokers for 10 min | **Zero data loss**; the outbox retains rows and backs off; `pp_outbox_backlog` rises then fully drains; payment success rate unaffected; every event eventually published exactly-effectively-once |
| C-9 | Outbox backlog | Outbox backlog | Throttle the relay to 10 % | Backlog alert fires; KEDA scales the relay; backlog drains; ordering per partition key preserved |
| C-10 | Poison message | Consumer poison | Inject a malformed event | Message → `.retry` → `.dlq` after the configured attempts; the consumer **continues**; lag returns to baseline; alert fires |
| C-11 | Duplicate webhook | Duplicate webhook | Deliver the same webhook 5× concurrently | Exactly one state transition; four dedup drops counted; no duplicate ledger entry |
| C-12 | Webhook replay attack | Replay attack | Replay with a 10-minute-old timestamp; and with a reused nonce | `401`; security event raised; no state change |
| C-13 | Pod crash mid-workflow | Pod crash mid-workflow | `kubectl delete pod --grace-period=0` on `workflow-worker` | Lease expires; another worker resumes from the checkpoint; **no completed step re-executed**; onboarding completes |
| C-14 | Pod crash mid-payment | (implied) | SIGKILL `payment-orchestrator` between dispatch and commit | Attempt row exists with `DISPATCHED`; payment `PROCESSING`; **no retry**; reconciler resolves via the deterministic key; single charge |
| C-15 | Node loss | Node loss | Terminate an instance | PDBs respected; connections drained; p99 blip < 30 s; zero request errors beyond in-flight on the dead node |
| C-16 | AZ loss | AZ loss | Terminate all nodes in one AZ + Aurora failover | RTO ≤ 60 s, no human action; SLI holds; see `disaster-recovery.md` §9.1 |
| C-17 | Region loss | Region loss | Full GD-R game day | RTO ≤ 15 min, RPO ≤ 5 s, **zero duplicate payments**; `disaster-recovery.md` §6, §9.2 |
| C-18 | Network partition | (retry storm / partition) | `tc netem` / Chaos Mesh `NetworkChaos`: partition orchestrator ↔ Postgres | **CP behaviour**: writes fail closed with `503`, never a partial write; no split-brain; recovery is automatic |
| C-19 | Slow dependency | (latency) | Chaos Mesh 3 s latency on the gateway path | Bulkhead caps in-flight calls; `payment-api` unaffected (the §5 bulkhead argument); adaptive limiter sheds |
| C-20 | Clock skew | Clock skew | Skew a pod by +6 min | Webhook signature windows reject; ULID generation does not regress; no state corruption; alert fires |
| C-21 | Config corruption | Config corruption | Publish a config failing checksum/L4 | Publish rejected; data plane keeps last-known-good; zero payment impact |
| C-22 | Config staleness cliff | (fail-static) | Stop the control plane for 20 min | ≤ 900 s: serve last-known-good; > 900 s: fail closed for **new** merchants only, existing merchants keep processing (§15) |
| C-23 | Retry storm | Retry storm | 50 000 clients retrying with no backoff | Adaptive concurrency sheds; `429` with `Retry-After`; throughput capped but the system **survives**; no cascading failure |
| C-24 | Certificate expiry | (implied) | Expire the mTLS cert on one service | Traffic fails closed for that path; cert-manager renews; alert fires ≥ 14 d before expiry in the healthy case |
| C-25 | Secrets Manager unavailable | (implied) | Deny `secretsmanager:GetSecretValue` | Cached credentials continue to work; rotation is deferred; alert fires; no payment impact |
| C-26 | DLQ growth | DLQ | Force 100 messages to the DLQ | Alert fires; redrive tooling reprocesses them idempotently; zero duplicate effects after redrive |
| C-27 | Disk pressure | (node) | Fill node ephemeral storage | Eviction respects priority classes: batch first, money path last |
| C-28 | Combined | — | Redis loss **+** one gateway degraded **+** a node loss, simultaneously | All individual hypotheses hold together; no emergent failure |

Fault injection: **Chaos Mesh** in-cluster (`PodChaos`, `NetworkChaos`, `IOChaos`, `TimeChaos`, `StressChaos`); the **gateway simulator's** fault API (latency, error rate, response mutation, connection hold) for gateway-side faults; **AWS FIS** for AZ and instance faults; `toxiproxy` for local runs.

```go
//go:build chaos

func TestTimeoutLeavesPaymentProcessingAndReconciles(t *testing.T) {  // C-1
    env := chaos.NewEnv(t)
    hypothesis := env.SteadyState(
        chaos.NoPaymentInState("FAILED"),
        chaos.NoDuplicateSuccessfulAttempts(),
        chaos.LedgerBalanced(),
    )
    hypothesis.RequireHoldsNow(t)

    env.Simulator.HoldConnection("authorize", 12*time.Second)  // > the 8s hard timeout
    pay := env.CreatePayment(t, builders.Payment().WithAmount(5000, "USD"))

    env.Await(t, func() bool { return env.Payment(pay).State == "PROCESSING" })
    att := env.LatestAttempt(t, pay)
    require.Equal(t, "TIMEOUT_UNKNOWN", att.Outcome)
    require.Equal(t, 1, env.Simulator.CallCount("authorize"), "a second dispatch occurred")
    require.Equal(t, 1, env.EventCount("payment.reconciliation_required.v1", pay))

    hypothesis.RequireHeldThroughout(t)      // continuously sampled, not just at the end

    env.Simulator.ReportOutOfBand(att.GatewayIdempotencyKey, "AUTHORISED")
    env.Await(t, func() bool { return env.Payment(pay).State == "AUTHORIZED" })
    require.Equal(t, 1, env.Simulator.CallCount("authorize"))   // still exactly one charge
    hypothesis.RequireHoldsNow(t)
}
```

`RequireHeldThroughout` samples the hypothesis every 250 ms for the duration. Checking only at the end would miss a transient state that violated an invariant and then self-corrected — which is exactly the class of bug that later manifests as a mysterious duplicate.

---

## 7. The named failure scenarios

The scenarios from the brief, each as a named test with setup, action and assertion.

### FS-1 Gateway timeout during authorization
- **Test:** `tests/chaos/gateway_timeout_test.go::TestTimeoutLeavesPaymentProcessingAndReconciles`
- **Setup:** Merchant `ACTIVE`, routing plan `[adyen, stripe]`, simulator configured to hold the connection 12 s (> the 8 s hard timeout, §12 stage 14).
- **Action:** `POST /v1/payments` with a fresh idempotency key.
- **Assert:** Response is `200` with `status: "processing"` (§12.3 semantics), **not** an error. Payment is `PROCESSING`. Latest attempt is `TIMEOUT_UNKNOWN`. `simulator.CallCount("authorize") == 1` — **no retry, no failover**. `payment.reconciliation_required.v1` emitted exactly once. After the simulator reports `AUTHORISED` out of band, the payment becomes `AUTHORIZED`, the ledger has exactly one entry, and the call count is still 1.

### FS-2 Duplicate payment submission
- **Tests:** `tests/integration/idempotency_test.go::TestDuplicateSubmissionReplays`, `::TestConcurrentDuplicatesSecondGets409`, `::TestSameKeyDifferentBodyIs422`
- **Setup:** Merchant `ACTIVE`; idempotency key `k1`.
- **Action:** (a) submit, wait for completion, submit again with `k1` and an identical body; (b) submit twice concurrently with `k1`; (c) submit with `k1` and a *different* amount.
- **Assert:** (a) `201` with the byte-identical stored response snapshot and `Idempotent-Replay: true`; exactly one payment; exactly one gateway call. (b) One `201`, one `409 IDEMPOTENT_REQUEST_IN_PROGRESS` with `Retry-After: 1` (§1.3 A6); the loser did **not** block on a lease; exactly one payment. (c) `422 IDEMPOTENCY_KEY_REUSED` (§14.2); no second payment.

### FS-3 Duplicate webhook delivery
- **Test:** `tests/chaos/webhook_duplicate_test.go::TestDuplicateWebhookProcessedOnce`
- **Setup:** Payment `PROCESSING`; a valid `payment.captured` webhook body signed by the gateway.
- **Action:** Deliver the identical webhook 5× concurrently from 5 connections.
- **Assert:** All 5 receive `200` (a gateway must never be told to retry a webhook we already have). Exactly **one** state transition `PROCESSING → CAPTURED`. Exactly one `payment.captured.v1` event. Exactly one ledger entry. `webhook_dedup` has one row. `pp_webhooks_deduplicated_total` incremented by 4.

### FS-4 Database unavailable mid-payment
- **Test:** `tests/chaos/db_unavailable_test.go::TestDatabaseLossFailsClosedWithoutLoss`
- **Setup:** Steady 200 TPS.
- **Action:** Force an Aurora failover (staging) / pause the Postgres container (local) for 45 s.
- **Assert:** Requests during the window receive `503 SERVICE_UNAVAILABLE`, `retryable: true`, with `Retry-After` — **never `500`**, never a partial write. Readiness fails; **liveness holds** and no pod restarts (the §1.7 rule in `deployment.md`). `GET` requests continue from replicas. On recovery: zero payments in an indeterminate state, zero duplicate attempts, invariants I1/I3 hold, and every payment that returned `201` before the failure is present and correct.

### FS-5 Kafka unavailable
- **Test:** `tests/chaos/kafka_unavailable_test.go::TestKafkaLossLosesNoEvents`
- **Setup:** Steady 200 TPS; a marker set of 5 000 payments.
- **Action:** Stop all brokers for 10 minutes, then restart.
- **Assert:** Payment success rate is **unaffected** (the outbox decouples publishing from the request path, §13.4). `pp_outbox_backlog` rises monotonically; the relay backs off without erroring the request path. `OutboxBacklogGrowing` alert fires. After recovery, the backlog drains fully; every one of the 5 000 payments has its complete event set; per-`payment_id` ordering is preserved; consumers dedupe any redelivery; ledger entry count matches the payment count exactly.

### FS-6 Redis unavailable
- **Test:** `tests/chaos/redis_unavailable_test.go::TestRedisLossDegradesLatencyNotCorrectness`
- **Setup:** Steady 200 TPS including 5 % idempotent replays.
- **Action:** Kill the Redis cluster for 5 minutes.
- **Assert:** Zero errors attributable to Redis. Idempotency remains correct via Postgres (§14.3) — every replay still returns the stored snapshot. Rate limits still enforced via local token buckets, coarser but bounded. `pp_config_snapshot_age_seconds` rises but stays under the cliff. p99 rises to ≤ 400 ms and returns to baseline within 60 s of recovery. **Zero duplicate payments.**

### FS-7 Gateway failover mid-transaction
- **Test:** `tests/chaos/gateway_failover_test.go::TestFailoverCreatesNewAttemptNotDuplicate`
- **Setup:** Routing plan `[adyen, stripe]`; Adyen returns `503` for the first 2 calls of each payment.
- **Action:** Submit a payment.
- **Assert:** Attempt 1 (Adyen) retries ≤ 2 times **on the same attempt** with the **same** `gateway_idempotency_key` (§14.4). On exhaustion, attempt 2 is created for Stripe with a **different** key (asserted by inequality, and by the key being a pure function of `attempt_id`). Attempt 1 is `ERROR`; attempt 2 is `SUCCESS`; the payment is `AUTHORIZED`. Exactly one successful attempt (I3). The routing plan is persisted with reasons. `pp_routing_decisions_total{reason="fallback_error"}` incremented once. Variant `TestHardDeclineNeverFailsOver` asserts that a hard decline produces **zero** attempt-2 (§9.1).

### FS-8 Merchant onboarding interrupted
- **Tests:** `tests/integration/workflow_test.go::TestWorkflowResumesAfterWorkerCrash`, `::TestCompensationsRunInStrictReverseOrder`
- **Setup:** Onboarding advanced to `provision-gateways` (step 5) with two gateways provisioned.
- **Action:** (a) SIGKILL the worker; (b) separately, make `apply-configuration` (step 8) fail permanently.
- **Assert:** (a) The lease expires; a different worker resumes; **no completed step re-executes** (external call counts unchanged); the instance completes; the merchant reaches `ACTIVE`; `Provision` was called exactly once per gateway. (b) The instance moves to `FAILED`; compensations run in strict reverse order (`delete-webhook-registration`, `delete-secret-version`, `deprovision-subaccount`, `cancel-kyc-case`); the simulator reports zero orphaned resources; the step payload with its full error chain is in `workflow_dlq`; the merchant is **not** `ACTIVE`.

### FS-9 Pod crash mid-request
- **Test:** `tests/chaos/pod_crash_test.go::TestCrashAfterGatewayCallBeforeCommit`
- **Setup:** A hook in `payment-orchestrator` that SIGKILLs the process after the gateway returns `AUTHORISED` but before the state transaction commits.
- **Action:** Submit a payment; kill; let the client retry with the same idempotency key.
- **Assert:** The attempt row exists (written **before** dispatch, §12 stage 13) with `DISPATCHED`. The payment is `PROCESSING`. The client's retry receives `409 IDEMPOTENT_REQUEST_IN_PROGRESS`, then after lease expiry receives `processing` semantics — **never a second dispatch**. `simulator.CallCount("authorize") == 1`. The reconciler resolves the attempt via the deterministic key; the payment becomes `AUTHORIZED`; exactly one ledger entry. Variants cover crashing before dispatch (Window 1) and after commit before response (Window 2) — see `disaster-recovery.md` §7.2.

### FS-10 Network partition
- **Test:** `tests/chaos/partition_test.go::TestPartitionFailsClosedNoSplitBrain`
- **Setup:** Chaos Mesh `NetworkChaos` partitioning `payment-orchestrator` from the Aurora writer subnet, while `payment-api` remains connected.
- **Action:** Hold for 3 minutes under 100 TPS, then heal.
- **Assert:** **CP behaviour** (§1.3 A4): writes fail closed with `503`, retryable; zero partial writes; zero payments in an indeterminate state. Reads continue from replicas (`AP`, bounded staleness). No orchestrator instance believes it can write. On heal: automatic recovery within 30 s, no manual intervention, invariants hold, and the reconciliation sweep finds zero exceptions. The DR fence variant (`TestFencedRegionRejectsWrites`) asserts a pod with a stale epoch stops accepting writes within 10 s (`disaster-recovery.md` §3).

---

## 8. Test data strategy

### 8.1 Builders

No fixture files, no `testdata/*.json` for domain objects, no shared setup functions that a hundred tests depend on.

```go
// tests/builders/payment.go
func Payment() *PaymentBuilder {
    return &PaymentBuilder{                 // valid by default; every field overridable
        tenantID:   ids.Deterministic("ten", 1),
        merchantID: ids.Deterministic("mrc", 1),
        amount:     money.MustNew(1000, money.USD),
        method:     "CARD",
        state:      payment.StateCreated,
    }
}
func (b *PaymentBuilder) WithAmount(minor int64, ccy string) *PaymentBuilder { /* ... */ }
func (b *PaymentBuilder) InState(s payment.State) *PaymentBuilder { /* replays the real
    transition path to reach s — never sets the field directly, so an unreachable
    state is a compile-or-test-time failure rather than an invalid fixture */ }
func (b *PaymentBuilder) Build() *payment.Payment { /* ... */ }
```

`InState` replaying the real transition path is the detail that matters: a builder that assigns `state = REFUNDED` directly can construct aggregates the FSM would never produce, and tests built on them assert behaviour that cannot occur in production.

### 8.2 Determinism

| Source of nondeterminism | Replacement |
|---|---|
| Wall clock | The `Clock` port (§25 `internal/infrastructure/clock`). Tests use `clock.NewFake(t, baseTime)` with explicit `Advance()`. `time.Now()` outside the clock package is a lint failure |
| ID generation | `ids.Deterministic(prefix, seed)` produces stable, valid, sortable ULIDs. Test output diffs are readable and failures are reproducible |
| Random | A seeded `*rand.Rand` injected through a port; property tests print the seed on failure and accept `-rapid.seed` to replay |
| Map iteration | Sorted before assertion; `TestNoUnsortedMapIterationInAssertions` catches the common cause of a 1-in-20 flake |
| Goroutine scheduling | `synctest`-style deterministic scheduling where feasible; `-race` on every run; explicit synchronization instead of `time.Sleep` |
| Timeouts/polling | `require.Eventually` with an explicit budget and interval. A bare `time.Sleep` in a test is a lint failure |
| Container ports | Testcontainers dynamic ports; never a hardcoded port |

### 8.3 No shared mutable fixtures

| Rule | Enforcement |
|---|---|
| Every test creates the data it needs | Builders; no `var globalMerchant` |
| Every test uses its own tenant | `tenantID := testenv.NewTenant(t)` — a fresh ULID per test, registered for cleanup |
| Tests run `t.Parallel()` | Which makes a shared mutable fixture fail immediately and loudly rather than intermittently |
| Package-level state is banned in tests | `TestNoPackageLevelTestState` (AST check) |
| Seeded reference data (currencies, gateway descriptors, routing defaults) is **immutable** | Loaded once, read-only, guarded by a copy-on-read accessor |

### 8.4 Proving cleanup

A test that leaks rows, keys, topics or containers makes the *next* test flaky, and the failure appears somewhere else entirely. So cleanup is asserted, not assumed.

```go
// tests/integration/testenv/cleanup.go
func Isolate(t *testing.T) *Scope {
    s := &Scope{
        Tenant: NewTenant(t),
        Schema: fmt.Sprintf("test_%s", ulid.Make()),
        Prefix: fmt.Sprintf("t_%s_", ulid.Make()),
    }
    before := snapshotResources(t, s)      // row counts per table, redis key count, topics, files

    t.Cleanup(func() {
        s.dropSchema(t)
        s.deleteRedisPrefix(t)
        s.deleteTopics(t)

        after := snapshotResources(t, s)
        // The assertion: this test's namespace is empty, and the SHARED namespace
        // is exactly as it was found. The second half catches a test that wrote
        // outside its own tenant — a bug that would otherwise appear as a flake
        // in an unrelated test three days later.
        require.Equal(t, before.Shared, after.Shared,
            "test mutated shared state: %s", diff(before.Shared, after.Shared))
        require.Zero(t, after.Owned.TotalRows, "left %d rows behind", after.Owned.TotalRows)
        require.Zero(t, after.Owned.RedisKeys)
        require.Zero(t, after.Owned.Topics)
        require.Zero(t, goroutineDelta(t), "leaked goroutines:\n%s", goroutineDump())
    })
    return s
}
```

`goleak.VerifyTestMain` runs in every integration package as a second layer, catching goroutine leaks from unclosed clients and un-cancelled contexts.

---

## 9. Flakiness policy

A flaky test is worse than no test: it trains the team to re-run CI without reading the failure, and eventually a real failure gets re-run away.

### 9.1 Detection

| Mechanism | Detail |
|---|---|
| Every CI run publishes JUnit XML to a results store | Per-test pass/fail history over 30 days |
| Nightly repeat run | `go test ./... -count=5 -race` on `main`; any test not passing 5/5 is flagged |
| Flakiness score | `failures / runs` over 30 d, weighted toward recency |
| Retry telemetry | CI retries **nothing** automatically. A re-run is a manual action, and it is recorded against the test |
| Timing regression | A test whose p95 duration doubles is flagged — it is usually a `Sleep` racing something |

### 9.2 Quarantine

| Score | Action |
|---|---|
| 1 failure in 30 d | Logged, no action |
| ≥ 2 failures in 30 d, or ≥ 1 % | Auto-filed issue, owner = the last person to modify the test, due in 5 working days |
| ≥ 5 % | **Quarantined**: moved to the `flaky` build tag, excluded from the required check, still run nightly for visibility. A 14-day fix-or-delete clock starts |
| Quarantine expired | The test is **deleted** and a note is added to `tests/critical_paths.yaml` if it covered a critical path — which immediately fails `TestEveryCriticalPathIsAsserted` and forces a replacement rather than silent loss of coverage |

Quarantine is capped at **15 tests repository-wide**. Exceeding the cap fails the build for everyone. Without a cap, quarantine becomes a landfill.

### 9.3 The money-path rule

**A flaky test in the money path blocks the release. It is never quarantined.**

The money path is defined precisely, so this is not a judgement call:

- `internal/domain/payment/**`, `internal/domain/ledger/**`, `internal/domain/shared/money*`
- `internal/application/payment/**`, `internal/platform/idempotency/**`
- Every test named in `tests/critical_paths.yaml`
- `tests/integration/invariants_test.go`, `tests/integration/idempotency_test.go`, `tests/integration/tenancy_test.go`
- Every FS-1 … FS-10 test in §7

Rationale: a flaky test here means one of two things — the test is wrong, or **the system is nondeterministic in the money path**. The second is a production incident that has not happened yet. Both require investigation before shipping. "It passes on re-run" is a description of a race condition, not a resolution.

Enforcement: `scripts/flaky-gate.sh` reads the results store, intersects the flagged set with the money-path list, and fails the release pipeline with the test name and its failure history. There is no override flag. Shipping with a flaky money-path test requires deleting the test in a reviewed PR — which is visible, deliberate, and attributable, unlike a re-run.

---

## 10. Running it

### 10.1 Locally

```bash
make test                 # unit only, no containers, ~20s — the inner loop
make test-race            # unit with -race, ~55s
make test-integration     # testcontainers: postgres, redis, redpanda, ~6min
make test-contract        # gateway adapter suite + event contracts, ~90s
make test-api             # OpenAPI-validated request/response suite, ~2min
make test-workflow        # workflow engine suite, ~3min
make test-security        # authn/authz/injection/JWT/PAN, ~2min
make test-all             # everything except chaos and load
make cover                # coverage report + gate check + delta vs main
make mutation-probe       # critical-path mutation probes (~4min)

# Targeted
go test ./internal/domain/payment/... -run TestExhaustiveTransitionMatrix -v
go test -tags=integration ./tests/integration/... -run TestCrossTenantAccessIsImpossible -v
go test ./internal/domain/... -run TestMoneyProperties -rapid.checks=100000
go test ./... -run TestFSMProperties -rapid.seed=1724680000000000000   # replay a failure

# Full stack locally (docker compose: all 7 services + simulator + postgres + redis + redpanda)
make up
make test-e2e             # ~8min against the local stack
make test-chaos-local     # toxiproxy-based subset of §6.3, ~10min
make down

# Load (requires k6; point at staging, never at prod)
k6 run tests/load/steady-state.js  --env BASE=https://api.staging.example.com
k6 run tests/load/ramp.js          --env BASE=https://api.staging.example.com
k6 run tests/load/spike.js         --env BASE=https://api.staging.example.com
k6 run tests/load/soak.js          --env BASE=https://api.staging.example.com   # 4h

# Verify everything CI verifies, before pushing
make verify               # fmt, generate-drift, lint, arch check, logging check,
                          # metrics lint, unit, race, integration, contract, api,
                          # sast, vuln scan, manifest validation, traceability
```

### 10.2 In CI

| Trigger | Stages (`deployment.md` §4.1) | Budget |
|---|---|---|
| Every push | 1–7: format, lint, architecture, logging, metrics lint, unit, race | ~4 min |
| Pull request | + 8–13: integration, contract, API, event compat, SAST, deps, terraform | ~11 min |
| PR touching `internal/domain/**` or `migrations/**` | + mutation probes, + the previous release's binary against the new schema | +6 min |
| Merge to `main` | + 14–22: build, image scan, SBOM, sign, manifest validation, policy, traceability, SLO gate; then dev deploy + e2e | ~9 min + 8 min |
| Nightly | `-count=5 -race` repeat run; full chaos suite; live-sandbox contract suite; fixture re-recording; RD-1 restore drill (`disaster-recovery.md` §5.3) | ~70 min |
| Pre-release | Full chaos + all four k6 scenarios (including the 4 h soak) + staging canary | ~5 h |
| Quarterly | GD-R region failover game day (`disaster-recovery.md` §9.2) | 4 h |

Parallelization: unit tests shard across 8 runners by package; integration across 4 runners, each with its own container set. The suite is designed so that no shard depends on another — which is the same property that makes `t.Parallel()` safe, tested by the same isolation assertions in §8.4.

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
