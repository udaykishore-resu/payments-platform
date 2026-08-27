# Contributing

This document is how work gets done in this repository. It assumes you have read
[`README.md`](README.md) and will read [`docs/spec/00-design-baseline.md`](docs/spec/00-design-baseline.md)
before changing anything structural.

One rule sits above all the others: **the baseline is the single source of truth.** Specifications,
architecture documents, API contracts, code and tests are all *derived* from it. If your change
makes any of them disagree with the baseline, either the change is wrong or the baseline needs
amending first — in that order, and never silently. Nothing may be implemented before the
corresponding baseline section exists. That is what "spec-driven" means operationally.

---

## 1. The layering rules, and why they are enforced mechanically

The architecture is Clean/Hexagonal. The dependency rule points inward:

```
cmd/*                         composition roots only
  ↓ wires
internal/infrastructure, internal/adapters      driven + driving adapters
  ↓ implements
internal/application                            use cases + the ports they own
  ↓ depends on
internal/domain                                 entities, value objects, FSMs
                                                imports nothing but stdlib and pkg/
```

Stated as constraints:

| Package | May import | May **not** import |
|---|---|---|
| `internal/domain/**` | stdlib, `internal/domain/**`, `pkg/**` | anything else — no `database/sql`, no `net/http`, no otel, no AWS, no UUID library |
| `internal/application/**` | stdlib, `internal/domain/**`, `internal/application/ports`, `internal/adapters/gateway/spi` † | `internal/infrastructure/**`, any adapter other than `spi`, any driver |
| `internal/validation/**`, `internal/workflows/engine` | stdlib, domain, `application/ports` | infrastructure |
| `internal/infrastructure/**`, `internal/adapters/**` | anything | another adapter's internals |
| `cmd/**` | anything | business logic — composition only |
| `pkg/**` | **stdlib only** | everything under `internal/` |

† **The SPI exception.** `internal/adapters/gateway/spi` is a port *declaration* — interfaces and
value types, importing nothing outside stdlib, `internal/domain/**` and `pkg/**`. It lives under
`adapters/` next to its implementations because a reader looking for "how do we talk to Stripe"
would not think to look in `application/ports`. Duplicating its twenty-odd request and result types
into `ports` to satisfy a directory-naming convention would add a translation layer whose only job
is to prove a rule was followed. The exception is narrow and is itself checked: the architecture
gate asserts both that `spi` imports nothing forbidden, and that no *other* package under
`internal/adapters/` is imported from `internal/application/`.

`pkg/**` is stdlib-only because it is the part of this repository that could be extracted and
published. A dependency added there is a dependency imposed on every future consumer.

### Why mechanically, and not by review

Because a rule enforced by review is a rule enforced on the days the reviewer is paying attention.
Layering violations are cheap to introduce (one import line, added by an IDE, invisible in a large
diff) and expensive to remove (by the time anyone notices, twenty call sites depend on it). A
Friday-afternoon `database/sql` import in the domain package is not caught by a human reading a
600-line PR; it is caught by `scripts/check-architecture.sh` in 200 milliseconds.

Run it alone with `make check-arch`. It is stage 6 of `make verify` and a required CI job. It fails
the build — it does not warn. A rule that only warns is a rule that is ignored
([ADR-024](docs/adr/ADR-024-monorepo-single-go-module.md)).

The same reasoning produces the other eight fitness functions: the error catalogue check, the
rule-documentation check, the metric-cardinality check, the event registry/schema check, the OpenAPI
check, the migration check, the secret scan and the licence check. Each of them exists because the
failure it catches is silent.

---

## 2. Definition of done

Baseline §27. A change — a phase, a feature, a fix — is complete only when **all twelve** hold. Not
eleven.

| # | Item | What "done" looks like here |
|---|---|---|
| 1 | Objective stated | The PR description says what problem this solves, not what code it changes |
| 2 | Requirements enumerated | A `BR-`/`FR-`/`NFR-` identifier exists, or the change is traced to an existing one |
| 3 | Design documented | The relevant `docs/` document reflects the change; if it is structural, an ADR exists |
| 4 | Interfaces defined | Ports declared by the consumer in `internal/application/ports`, narrow (1–4 methods) |
| 5 | Data model defined | Tables, columns, constraints and indexes are in a migration, not implied by code |
| 6 | Failure modes enumerated | Each new failure appears in `docs/failure-handling.md` with detection, response and degradation |
| 7 | Security considerations stated | Tenant scoping, authz scope, secret handling, PCI implications — explicitly, even when "none" |
| 8 | Validation rules defined | New inputs have L1/L4/L5 rules with stable IDs, documented in `docs/validation-plane.md` |
| 9 | Observability defined | Metrics (respecting §22.3 cardinality), log fields, span attributes, and an alert if it can page |
| 10 | Tests written **and passing** | See §8 below. "Passing" includes the race detector |
| 11 | Implementation complete | No `TODO`, no commented-out branch, no unreachable placeholder |
| 12 | Verification run | `make verify` green locally, and the CI `ci-ok` gate green |

Item 12 is the mechanical half of items 1–11. Items 1–11 are the half a human has to check, which
is what the review checklist in §9 is for.

---

## 3. How to add a gateway

There is one definition of an integrated gateway: **it implements the SPI and it passes the
contract suite.** There is no second definition, no "mostly working", and no exception for an
adapter that only implements the payment path.

### 3.1 Implement the SPI

Create `internal/adapters/gateway/<name>/`. Use `stripe/`, `adyen/` or `paypal/` as the reference —
each splits into `factory.go`, `gateway.go`, `mapping.go`, `model.go`, `provision.go` and
`webhook.go`, and that split is worth keeping.

You must satisfy four interfaces from `internal/adapters/gateway/spi`:

| Interface | Methods | Notes |
|---|---|---|
| `Factory` | `ID`, `NewGateway`, `NewProvisioner`, `NewVerifier` | Registered in `internal/adapters/gateway/registry/wiring.go` |
| `PaymentGateway` | `ID`, `Authorize`, `Capture`, `Refund`, `Void`, `Lookup` | `Lookup` is what the reconciler uses to resolve a `TIMEOUT_UNKNOWN` attempt; it must find a transaction by our deterministic idempotency key **alone** |
| `GatewayProvisioner` | `ID`, `Provision`, `Deprovision`, `RegisterWebhook`, `UnregisterWebhook`, `VerifyCredentials` | Every one of these is a compensable workflow activity; `Provision` must be idempotent on an external reference and `Deprovision` must tolerate a missing account |
| `WebhookVerifier` | `ID`, `Verify` | Signature scheme, replay window, and a stable event ID for the same payload |

Also declare the **capability descriptor** — countries, currencies, payment methods, operations, 3DS
support, partial capture, refund window, webhook signature scheme. Routing hard-filters on it, so an
overstated capability becomes a `503 NO_ELIGIBLE_GATEWAY` for someone else's payment or a runtime
failure for yours.

Rules that are not negotiable:

- **No gateway type ever reaches `internal/domain`.** The adapter is an anti-corruption layer. Map
  in `mapping.go` and map totally: an unmapped status is `TIMEOUT_UNKNOWN`, never a guess.
- **A client timeout after the request was written returns `ErrOutcomeUnknown`, not an error.** A
  refused connection — where the request provably never left the process — returns a plain error and
  is safe to retry. Getting these two backwards is how a payer is charged twice, or how a healthy
  payment is parked in reconciliation waiting for a human.
- **Never return `(nil, nil)`.**
- **Credentials are `Secret[T]`** and must not appear in an error string, a log line or a span
  attribute.
- **Hard declines must map to a non-failover reason.** Retrying a stolen-card decline on another
  gateway is card-testing behaviour and gets the platform de-registered.

### 3.2 Pass the contract suite

Add `internal/adapters/gateway/<name>/<name>_contract_test.go`:

```go
func TestContract(t *testing.T) {
    contract.RunSuite(t, contract.Subject{
        Name:           "<name>",
        NewGateway:     func(d spi.HTTPDoer) spi.PaymentGateway { /* … */ },
        NewProvisioner: func(d spi.HTTPDoer) spi.GatewayProvisioner { /* … */ },
        NewVerifier:    func(d spi.HTTPDoer) spi.WebhookVerifier { /* … */ },
        // fixtures: the gateway's real response shapes
    })
}
```

`contract.RunSuite` runs **17 named assertions** (`contract.AssertionCount`), each a subtest so a
failure names the obligation rather than a line number:

`IdempotencyKeyIsSent` · `RepeatedCallWithSameKeyIsSafe` · `TimeoutYieldsOutcomeUnknown` ·
`ConnectionRefusedYieldsError` · `HardDeclineMapsToNonFailoverReason` ·
`SoftDeclineMapsToFailoverReason` · `UnmappedDeclineIsUnknownAndDoesNotFailover` ·
`SuccessCarriesGatewayRef` · `LookupFindsByIdempotencyKeyAlone` · `AmountAndCurrencyEchoed` ·
`ContextCancellationIsHonoured` · `CredentialsNeverAppearInErrorsOrLogs` ·
`WebhookSignatureVerification` · `WebhookEventIDIsStableForSamePayload` · `NilResultAndNilErrorNeverReturned` ·
`ProvisionIsIdempotent` · `DeprovisionToleratesMissingAccount`

Use the gateway's **real response shapes** as fixtures, trimmed to the fields the adapter reads plus
enough surrounding structure that a field it *should* be reading but is not shows up as a failure
rather than as an absence.

Run it with `make test-contract`. No live gateway is involved — the suite drives a recording
`HTTPDoer`.

### 3.3 Finish the integration

1. Register the factory in `internal/adapters/gateway/registry/wiring.go`.
2. Add the gateway slug to the relevant seed profiles in `config/seed/profiles.yaml` if local
   development should be able to route to it.
3. Add the sandbox certification matrix entries — the certification suite of baseline §11.4 asserts
   authorize→capture→refund, authorize→void, a mapped decline, a signature-verified webhook that
   moves state, a 3DS challenge reaching `REQUIRES_ACTION`, an idempotent duplicate, and an
   amount/currency echo, for **each** `(gateway, payment_method, currency)` the merchant enabled.
   `PRODUCTION_READY` is unreachable without a passing report.
4. Document it in `docs/data-plane.md` and, if the routing weights or hard filters change,
   `docs/payment-flow.md`.

---

## 4. How to add a validation rule

Rules live in `internal/validation/rules/l{1..7}<name>/` and implement:

```go
type Rule[T any] interface {
    ID() RuleID           // "L5.AMOUNT_WITHIN_MERCHANT_LIMIT" — stable, documented, never reused
    Severity() Severity   // ERROR | WARNING
    Evaluate(ctx context.Context, subject T) Outcome
}
```

Rules are **pure and total** wherever possible: same input → same outcome, no clock read, no
network, no panic, no regex compiled from user input (catastrophic backtracking is a DoS vector).
L3 is the one impure level and is barred from the payment hot path — it runs only in
`workflow-worker` and in scheduled probes.

Pick the right level: L1 API/schema at the edge; L2 merchant; L3 gateway; L4 configuration on the
control-plane write path; L5 payment pre-dispatch; L6 gateway response post-call; L7 domain state in
aggregate methods. The level determines where it runs and which HTTP status it produces.

### The rollout: shadow → warn → enforce

A new `ERROR` rule is a behaviour change with revenue consequences. A rule that looks obviously
correct will reject a fraction of a percent of traffic that has been succeeding for two years,
because the real world contains a legacy integration you did not know about. Stage is set in the
registry entry, not by a code edit, and is configuration — so demoting `Enforce → Warn` during an
incident is a config publish propagating in ≤ 30 s, not a deploy.

| Stage | Runs | Recorded | Affects the response | Exit criterion |
|---|---|---|---|---|
| **Shadow** | Yes, every request | Yes, into `pp_validation_outcomes_total{rule,stage="shadow",result}` and the audit record | **No** — the report drops shadow outcomes before building the `Problem` | ≥ 7 days and ≥ 10⁵ evaluations with a would-reject rate within the pre-agreed budget (default 0.05 % of requests), and every distinct rejecting merchant reviewed |
| **Warn** | Yes | Yes, `severity=WARNING` | Yes — appears in `details[]` and on the merchant's dashboard; the operation still succeeds | ≥ 14 days, and the top-20 affected merchants notified and either fixed or granted an expiring exception |
| **Enforce** | Yes | Yes, `severity=ERROR` | Yes — rejects | — |

```go
Register(l5.AmountAboveMethodMinimum{}, validation.Stage(validation.Shadow),
         validation.Since("2026-09-01"), validation.Owner("payments-core"))
```

Promotion requires the **shadow report** as an artifact: a dashboard link showing would-reject rate
by merchant, route and rule, plus 20 sampled shadow-rejected requests with a human judgement that
each *should* have been rejected. Retirement is symmetric — `Enforce → Warn → Shadow → RETIRED` —
and the ID stays documented with `status: RETIRED`, because old audit records reference it.

**The one exception:** a rule closing a security or compliance hole (a newly sanctioned country, a
PAN-detector gap) ships directly to `Enforce` with an incident-style approval recorded in the PR.
That path is rare and is audited.

### Then

- Document the ID, its meaning and its remediation in `docs/validation-plane.md`, and update the
  §3.8 totals table. `make verify` stage 8 (`scripts/check-rules-documented.sh`) fails the build
  otherwise, in both directions: an undocumented rule and a documented ID that resolves to nothing.
- Add the error code to `api/errors/catalog.yaml` if it is new.
- Test the rule against the **zero value** of its subject and against a fuzz corpus; a panic is a
  test failure.

---

## 5. How to add an event

Order matters here: **schema first**. An event is a published contract and the schema is the only
executable statement of what it means.

1. **Write the JSON Schema** — `api/events/<aggregate>.<verb>.v1.schema.json`. It must set
   `additionalProperties: false` and a non-empty `required` list, carry `x-topic` and
   `x-partition-key`, use the versioned type name in both `$id` and `title`, and include
   `examples` that validate against itself. `additionalProperties: false` is a compatibility gate,
   not a style rule: without it the schema cannot detect a producer that started sending an
   undeclared field, which is exactly the change the additive-only promise needs to classify.
2. **Register the type** in `internal/events/registry.go` with its topic and partition-key field.
   `scripts/check-events.sh` enforces seven properties across the pair (registry ↔ schema): no type
   without a schema, no schema without a producer, valid JSON Schema, examples that validate,
   matching topic and partition key, matching `$id`/`title`, and the two structural requirements
   above.
3. **Write the translation** in `internal/events/translate.go` — domain event struct → envelope.
   The domain knows nothing about CloudEvents, Kafka or JSON; domain events are plain structs raised
   into a `[]Event` on the aggregate and drained by the repository **inside the state-change
   transaction**. That transactional outbox write is what eliminates the dual-write failure mode.
4. **Write the consumer** in `cmd/event-consumer` (or wherever it belongs) using the idempotent
   consumer in `internal/events/consumer.go`: dedup `INSERT (consumer_group, event_id) ON CONFLICT
   DO NOTHING`; zero rows affected means already processed, so ACK and drop; otherwise handle
   **within the same transaction as the dedup row**, commit, ACK.
5. Add the event to the catalog table in `docs/events.md` and baseline §13.2, naming its consumers.

Rules: events are immutable, versioned in the type name (`.v1`), **additive-only within a major
version** (new optional fields only), and idempotently consumable. A breaking change is a new `.v2`
type published **alongside** `.v1` until every consumer has migrated — never an in-place edit.
Ordering is guaranteed **per partition key only**; since the key is the aggregate ID, all events for
one payment are ordered and no consumer may assume a global order.

---

## 6. How to change a state machine

State machines are **tables**, built with `shared.NewStateMachine`, never scattered `if` statements.
Self-transitions must be declared explicitly if legal. Changing one touches four places, and a PR
that touches fewer than four is incomplete:

1. **The table** — `internal/domain/<aggregate>/state.go`. Add the `from → to` entry with its
   trigger. If the new state carries data (a reason code, a reviewer), model it.
2. **The migration** — the database constraints and any `CHECK` or trigger in `migrations/` that
   enumerate legal states (see `0013_state_guards`). The domain and the database must agree; the
   database is the last line of defence for the invariants a bug in the domain would let through.
3. **The docs** — baseline §8 or §9 (the transition table *and* the ASCII diagram),
   `docs/state-machines.md`, and `docs/diagrams/13-state-machines.md`. If the change alters what a
   guard requires, say which guard.
4. **The exhaustive test** — for **every** `(from, to)` pair in the state universe, the machine
   accepts exactly the pairs in its table and rejects everything else. Not a sample of interesting
   transitions: the full cross product. This is the test that catches the transition somebody added
   to the table and forgot to think about.

Two things that will get a state-machine change rejected regardless of how clean the code is:

- **A transition that makes an impossible state representable.** `SETTLED → PROCESSING`,
  `REFUNDED → CAPTURED`, `CAPTURED → AUTHORIZED`, anything out of `FAILED`, and anything that could
  make `refunded_total > captured_total`.
- **A transition that lets a timer fail a payment.** Nothing may move a payment out of `PROCESSING`
  except positive evidence: a webhook, a reconciler lookup, or a settlement report
  ([ADR-013](docs/adr/ADR-013-timeout-leaves-payment-processing.md)).

Amendment A-01 is the worked example of doing this well: the original merchant lifecycle had no exit
from the manual compliance gate other than approval, which made a compliance officer's rejection
unrepresentable — the workflow would have had to lie (`CERTIFICATION_FAILED`, blaming the
integration for a policy decision) or hang. `COMPLIANCE_REJECTED` was added as a distinct
non-terminal state carrying the reviewer's reason code, with routes back to `CONFIGURING`, back to
`KYC_PENDING`, or forward to `TERMINATED`.

---

## 7. How to write a migration

Migrations are `migrations/NNNN_slug.up.sql` with a mandatory matching `NNNN_slug.down.sql`,
numbered contiguously from 0001. `scripts/check-migrations.sh` (stage 12 of `make verify`) enforces
seven properties:

| Gate | Rule |
|---|---|
| G1 | Numbering is contiguous from 0001 — no gaps, no duplicates. A gap means a merge dropped a file |
| G2 | Every `.up.sql` has a `.down.sql` with the identical slug |
| G3 | Every table with a `tenant_id` column has RLS **enabled** *and* at least one **policy** — enabling RLS without a policy denies everything; a policy without RLS enforces nothing |
| G4 | No *unmarked* destructive statement. `DROP TABLE/COLUMN/INDEX/CONSTRAINT`, `TRUNCATE`, `ALTER COLUMN … TYPE` and `SET NOT NULL` in an `.up.sql` need a `-- pp:destructive <reason>` marker on the preceding line |
| G5 | No `CREATE INDEX` without `CONCURRENTLY` on a table introduced in an earlier migration |
| G6 | No `BEGIN`/`COMMIT` inside a file — the runner owns the transaction |
| G7 | Filenames match `^[0-9]{4}_[a-z0-9_]+\.(up\|down)\.sql$` |

G4 does not forbid destructive statements — expand/contract requires them. It forbids *unannounced*
ones, so that a reviewer looks at the deploy-ordering question ("is every replica of the old binary
gone?") instead of at the SQL.

### Expand / contract

Every schema change observable by a running process is split into an **expand** migration, a
**backfill**, and a **contract** migration, deployed in that order with a release between each.
Renaming `statement_ref` to `statement_descriptor`:

1. `00NN_expand`: `ADD COLUMN statement_descriptor TEXT`. Deploy code that writes **both** and reads
   the new one with a fallback to the old.
2. Backfill the existing rows.
3. Deploy code that writes and reads only the new column.
4. `00NN_contract`: `-- pp:destructive …` then `DROP COLUMN statement_ref`.

**Never rename in place.** It is instant on the database and catastrophic for the fleet: the old
binary's `SELECT` names a column that no longer exists.

**Rollback is forward-only.** The `.down.sql` exists so the expand/contract sequence can be
*rehearsed* — a migration that has never been reversed in a test is a migration whose reversal is a
hypothesis. In production, you roll forward. Because the old binary ran correctly against both the
pre-expand and the post-expand schema, rolling the *application* back is safe and does not require
touching the schema at all. That is the whole point of the pattern.

Before opening the PR, answer the checklist question from `migrations/README.md`: **does the *old*
binary still work against this schema?** If not, it is a contract migration and it does not ship in
the same release as the code that needs it.

---

## 8. Tests

| You changed | You must run |
|---|---|
| Anything | `make test-race` (`-count=1`, the race detector is non-negotiable on a concurrent money path) |
| A gateway adapter | `make test-contract` |
| A repository, migration, or anything touching Postgres/Redis/Kafka | `make test-integration` |
| A public HTTP contract | `make test-integration` and, with a stack up, `make test-e2e` |
| A resilience or failure path | `make test-chaos` (add `PP_TEST_CHAOS_INFRA=1` for the destructive scenarios) |
| Anything at all, before pushing | `make verify` |

Conventions, from baseline §06:

- **Table-driven tests**, `t.Parallel()` where safe, deterministic clocks (`shared.FixedClock`) and
  deterministic IDs, **no sleeps**. A sleep is a bet that this machine is no slower than the one the
  test was written on, and it loses that bet on the day CI is busy — producing a failure that looks
  like a product bug and gets re-run away. Poll with a deadline and fail with a message naming the
  state the thing was actually in.
- Integration tests are `<concept>_integration_test.go` with `//go:build integration`.
- A suite that needs services **skips** when its variables are unset, and the skip message names the
  exact variable and what it should contain. A suite that fails on a laptop for want of a container
  teaches people to ignore red.
- Every state machine gets the exhaustive property test described in §6.

Three claims frame the whole suite: money cannot move twice; state cannot become impossible; a
tenant cannot see another tenant's data. A test supporting none of them still has to justify its
maintenance cost.

Note the honest state of the coverage gates: `docs/testing.md` §1.1 specifies per-package floors
(95 % domain, 100 % for `platform/idempotency` and `platform/tenantctx`) enforced by
`scripts/coverage.sh`, **which is not present in this tree**. `make cover` reports rather than
blocks, and says so. Do not treat a green `make cover` as a passed gate.

---

## 9. Commits and pull requests

### Commits

- **One logical change per commit.** A commit that is "fix lint and also add refund partial capture"
  cannot be reverted, cherry-picked or bisected.
- **Imperative subject, ≤ 72 characters**, prefixed with the area:
  `payment: keep a timed-out attempt in PROCESSING`, `migrations: expand statement_descriptor`,
  `docs/adr: record the routing score weights`.
- **The body says why**, not what. The diff already says what. If the change encodes a trade-off,
  name the alternative and what it would have cost.
- Reference the requirement or ADR: `Refs FR-42`, `Implements ADR-015`.
- **Never commit a `go.mod` or `go.sum` change as a side effect.** CI runs with `-mod=readonly` and
  no target runs `go get`, `go mod tidy` or `go env -w`. A dependency change is its own PR with its
  own justification and its own licence-check result.
- Releases are cut from annotated tags; `git describe --tags` is what stamps `VERSION` into the
  binary and the image, so tags are semantic and never moved.

### Pull requests

The PR description is the durable artifact — it is what someone reads in eighteen months when they
are trying to understand why. It must contain:

1. **What problem this solves**, in prose, before any mention of the implementation.
2. **The baseline section, requirement ID or ADR** it derives from.
3. **What was considered and rejected**, if a real choice was made.
4. **The blast radius**: which planes, which deployables, whether it touches the money path.
5. **The rollout**: is a migration involved, is it expand or contract, does it need a feature flag
   or a validation stage, and what is the rollback lever.
6. **Evidence**: the `make verify` result, and for a money-path change, which specific test would
   fail if the property were removed.

Keep PRs small enough to actually review. A 2 000-line PR receives approval, not review. A migration
plus its code plus its docs is one PR; three unrelated fixes is three.

CI runs on `pull_request` and through the merge queue (`merge_group`), one run per PR with
force-pushes cancelling the previous run. The `ci-ok` job is the required gate; every other job
feeds it.

---

## 10. Review checklist

Reviewers work through this. Items in **bold** are blocking regardless of how good the rest is.

**Correctness of the money path**

- [ ] **Can this move money twice?** Trace the retry, the failover, the duplicate webhook and the
      replayed Kafka message. If the answer rests on "the code is careful", it is a rejection —
      it must rest on a database constraint.
- [ ] **Does anything here let a timer, a deadline or a cancellation fail a payment?**
- [ ] Is a new gateway call idempotent on our deterministic key, and does the attempt row get
      written **before** the call?
- [ ] Are amounts `money.Money` end to end? **No float appears anywhere in the money path.**
- [ ] Does the state change and its outbox event commit in **one** transaction?

**Boundaries**

- [ ] Layering: does `make check-arch` pass, and is any new SPI-shaped exception genuinely a port?
- [ ] Does any gateway or vendor type leak into `internal/domain`?
- [ ] Is the port declared by the consumer, in `application/ports`, and narrow?
- [ ] Does the data plane acquire any **synchronous** dependency on the control plane? (P1 — blocking)

**Tenancy and security**

- [ ] **Is tenant identity taken from the authenticated principal only**, never from a body or query
      string?
- [ ] Does every new table with a `tenant_id` have RLS **enabled and policied**?
- [ ] Does every new repository method take `context.Context` and fail with
      `ErrMissingTenantContext` rather than querying without a tenant?
- [ ] Are credentials `Secret[T]`? Could any new string field carry a PAN, and does the L1 detector
      see it?
- [ ] Is a new authz scope the narrowest one that works?

**Contracts**

- [ ] Is the OpenAPI change additive within `/v1`? A breaking change needs `/v2` and a `Sunset`
      header, not an edit.
- [ ] Is the event change additive within `.v1`? A breaking change is a `.v2` published alongside.
- [ ] Do new error codes exist in `api/errors/catalog.yaml` with the right category, status and
      `retryable` flag? Changing an existing code's `retryable` is **breaking** — client SDKs, the
      workflow engine and the outbox relay branch on it.

**Operations**

- [ ] Do new metrics respect the cardinality rule — **`merchant_id` and `payment_id` are never
      labels**?
- [ ] If this can page someone, does the alert exist, and is there a runbook? (See the README's
      status section: `docs/runbooks/` is currently empty, so "there is a runbook" usually means
      "this PR adds one".)
- [ ] Does the failure mode appear in `docs/failure-handling.md` with detection, response and
      degradation?
- [ ] Is the degradation posture explicit — fail-closed, fail-static or fail-open — and is it the
      right one? Fail-open on a limit is a compliance incident; fail-closed on configuration is a
      revenue incident.

**Discipline**

- [ ] Does the change contradict the baseline? If it does and it should, the baseline amendment is
      in the same PR and is the first commit.
- [ ] Does every exported symbol have a doc comment that says **why**? A comment restating the
      signature is noise.
- [ ] Are all twelve definition-of-done items satisfied?
- [ ] Is `go.mod` untouched?
