# Multi-Tenancy

> **Purpose:** how tenant isolation is achieved, enforced, tested and operated — from the token claim to the row-level security policy to the noisy-neighbour controls and the tenant lifecycle.
> **Derived from:** `docs/spec/00-design-baseline.md` §16 (isolation matrix and isolation guard), with §15 (consistency), §13 (event envelope), §17 (residency, retention). Where this document and the baseline disagree, the baseline wins and this document is a defect.

The governing decision is baseline A3: **pooled by default with row-level security; siloed schema available per tenant tier.** Pooled is the only model that reaches 50 000 merchants across 500 tenants (§18) at a sane cost; silo exists for tenants with contractual isolation requirements and is priced accordingly.

---

## 1. The isolation matrix, expanded

Each row of baseline §16.1 with: the mechanism, what breaks if it is absent, and the test that proves it holds. "Absent" here is not hypothetical — every one of these has been the root cause of a published multi-tenant breach somewhere.

### 1.1 Database

| | |
|---|---|
| **Pooled mechanism** | Shared Aurora cluster, shared schema. `tenant_id UUID NOT NULL` on every tenant-scoped table, first column of every composite index. PostgreSQL Row-Level Security enabled and **forced** on each. Application connects as `pp_app`, a role with neither `BYPASSRLS` nor `SUPERUSER`. Every transaction opens with `SET LOCAL app.tenant_id = $1`. |
| **Siloed mechanism** | Dedicated schema (tier S1) or dedicated cluster (tier S2). RLS remains enabled — silo is defence in depth on top of pooled isolation, never a replacement for it. |
| **Failure mode if absent** | A `WHERE tenant_id = ?` forgotten on one query in one repository method returns another tenant's payments. This is a single-line defect with an unbounded blast radius, and code review does not reliably catch it — a repository has dozens of methods and they all look alike. |
| **Test** | `internal/infrastructure/postgres/rls_integration_test.go::TestCrossTenantAccessIsImpossible` — insert rows for tenant A and tenant B, open a transaction with `SET LOCAL app.tenant_id = B`, run an *unqualified* `SELECT`, assert zero rows for A. It deliberately omits the `WHERE` clause so it tests the database, not the query. `::TestRLSBlocksADirectUpdate` and `::TestWithCheckRejectsACrossTenantInsert` cover the write side. |
| **Also tested** | `TestAppRoleLacksBypassRLS` (queries `pg_roles`), `TestEveryTenantScopedTableHasForcedRLS` (enumerates `information_schema` and fails on a table without a policy — this is the test that catches the *next* migration that forgets one). |

### 1.2 Cache

| | |
|---|---|
| **Pooled mechanism** | Every key is `pp:{tenant_id}:{namespace}:{id}`. The key is constructed by `cache.Key(ctx, ns, id)`, which reads the tenant from `context.Context` and returns an error if there is none — a caller cannot build an unprefixed key. Per-tenant memory quota tracked and enforced; `SCAN`-based namespace eviction for tenant offboarding. |
| **Siloed mechanism** | Dedicated Redis logical DB (S1) or dedicated cluster (S2). |
| **Failure mode if absent** | Cache poisoning across tenants: tenant A's merchant configuration served to tenant B because the key was `config:{merchant_id}` and merchant IDs are only unique *within* a tenant (baseline §2). Worse than a database leak because it is intermittent and therefore hard to reproduce. |
| **Test** | `internal/infrastructure/redis/cache_test.go::TestBuildKeyIsTenantScoped`, `::TestBuildKeyRejectsAnUntenantedKey`, `::TestEveryComponentRejectsAContextWithoutATenant` and `::TestGetOrLoadDoesNotShareFlightsAcrossTenants` — the last is the subtle one: single-flight collapsing must never merge two tenants' loads. |

### 1.3 Events

| | |
|---|---|
| **Pooled mechanism** | Shared topics (baseline §13.3). `tenantid` is a **required** envelope extension — the codec refuses to encode an event without it. Consumers filter by tenant where they are tenant-scoped. Kafka ACLs bind each service principal to the exact topics it may read or write. |
| **Siloed mechanism** | Dedicated topics `pp.{tenant}.…` with per-tenant ACLs. |
| **Failure mode if absent** | A projection consumer writes tenant A's event into tenant B's read model. Because projections are asynchronous, the corruption is discovered long after the causing deploy, and the repair requires replaying from Kafka with a corrected consumer — during which the read model is wrong. |
| **Test** | `internal/events/envelope_test.go::TestValidateRejectsEachMissingRequiredField` (the codec refuses an envelope without a tenant) and `::TestValidateRejectsMalformedIdentifiers`; on the consumer side, `internal/events/consumer_test.go::TestIdempotentHandlerDropsDuplicates` and `::TestDedupRowRollsBackWithTheWork`. A dedicated cross-tenant-envelope rejection test does not exist. | <!-- doc-refs: allow-missing -->

### 1.4 Configuration

| | |
|---|---|
| **Mechanism** | `configurations` and `configuration_versions` are tenant-scoped rows under RLS, versioned per merchant. Reads on the payment path come from the local snapshot cache with ≤ 30 s bounded staleness (baseline §15), keyed by tenant. |
| **Failure mode if absent** | Tenant A's routing policy applied to tenant B's payment: money routed to a gateway B has no contract with, which fails at best and settles to the wrong party at worst. |
| **Test** | `internal/platform/config/provider_test.go::TestPriorityInvalidationIsImmediate` and `::TestInvalidationSurvivesARefreshThatDoesNotRestoreTheMerchant`. A test asserting the snapshot is tenant-scoped **does not exist**. | <!-- doc-refs: allow-missing -->

### 1.5 Credentials

| | |
|---|---|
| **Mechanism** | One secret per `(tenant, merchant, gateway)` at IAM path `/{env}/{tenant}/{merchant}/{gateway}/{purpose}`. IAM policy scoped by path prefix; siloed tenants additionally carry a `secretsmanager:ResourceTag/tenant` condition matched against the IRSA session tag. KMS CMK per environment; per-tenant CMK for siloed. |
| **Failure mode if absent** | Tenant A's payment dispatched with tenant B's Stripe key: the charge lands in B's account. This is a financial-loss event, not merely a privacy one, and it is not reversible by a code fix. |
| **Test** | `internal/infrastructure/secrets/reference_test.go::TestReferenceValidateStopsCrossTenantResolution`, `::TestReferenceValidateStopsEnvironmentCrossing` and `::TestSecretIDMirrorsTheIAMPath` — the last is what makes the application check and the IAM condition the same statement. `internal/application/payment/components_test.go::TestCredentialsAreResolvedPerCallAndNeverCached` covers the dispatch side. The **IAM denial itself** is asserted by nothing; no AWS account has ever been used. | <!-- doc-refs: allow-missing -->

### 1.6 Object storage

| | |
|---|---|
| **Mechanism** | `s3://pp-{env}-artifacts/{tenant_id}/…`. The IRSA role's policy carries `Condition: {"StringLike": {"s3:prefix": ["${aws:PrincipalTag/tenant}/*"]}}` and the object ARN pattern is likewise templated, so IAM — not application code — enforces the prefix. Siloed tenants get a dedicated bucket with a dedicated CMK. |
| **Failure mode if absent** | KYC documents and certification reports readable across tenants. These are the most sensitive artifacts the platform holds. |
| **Test** | **none.** The S3 prefix condition is a Terraform policy document that has never been applied, so nothing asserts `AccessDenied` from AWS. `internal/infrastructure/secrets/reference_test.go::TestSecretIDMirrorsTheIAMPath` is the nearest thing: it pins the path shape the condition keys off. | <!-- doc-refs: allow-missing -->

### 1.7 Logs

| | |
|---|---|
| **Mechanism** | `tenant_id` injected from `context.Context` on every record by the logging middleware (never passed by callers — see `security.md` §6.2). Log query views are filtered by the viewer's tenant claim; a `tenant-admin` querying logs receives a view scoped by a server-side filter they cannot edit. |
| **Failure mode if absent** | Support tooling leaks one tenant's payment flow to another's administrator. Also a GDPR incident, because merchant principal identifiers are personal data. |
| **Test** | `internal/infrastructure/telemetry/logging_test.go::TestLoggerBindsContextFields` and `::TestAllowlistDropsUnregisteredKeys` — the tenant is bound from context, and an unregistered field never reaches the record. A cross-tenant log-query test **does not exist**; log-view authorization is not implemented here. | <!-- doc-refs: allow-missing -->

### 1.8 Metrics

| | |
|---|---|
| **Mechanism** | `tenant_id` is a label **only** on low-cardinality SLO counters (a bounded set: per-tenant availability and rate-limit counters). High-cardinality series carry `tenant_tier` and attach `tenant_id` as an **exemplar**, which is a trace pointer rather than a series dimension. `merchant_id` and `payment_id` are never labels (baseline §22.3). |
| **Failure mode if absent** | Two failures at once: a cardinality explosion that takes down Prometheus (500 tenants × 48 routes × 5 statuses is already 120 000 series on one metric), and cross-tenant business-volume inference by anyone with dashboard access. |
| **Test** | `internal/infrastructure/telemetry/metrics_test.go::TestCardinalityGuardRejectsForbiddenLabels` and `::TestSeriesOverflowFoldsRatherThanDroppingOrGrowing` — the second is the one that matters operationally: at the ceiling the guard folds into an `other` bucket rather than dropping data or growing without bound. `scripts/check-metrics-cardinality.sh` is the CI half. |

### 1.9 Compute

| | |
|---|---|
| **Mechanism** | Shared pods with per-tenant concurrency bulkheads and rate limits (§4). Siloed tier S2 gets a dedicated node group selected by taint/toleration and a dedicated namespace. |
| **Failure mode if absent** | One tenant's traffic burst consumes the shared connection pool and the goroutine budget; every other tenant sees timeouts. This is the most *likely* isolation failure in practice — far more likely than a data leak — and it is an availability breach against the 99.99 % target (§18). |
| **Test** | **none as a load test.** The bulkhead behaviour is asserted in-process by `internal/infrastructure/resilience/ratelimiter_test.go` and `tests/chaos/retry_storm_test.go::TestAdaptiveLimiterShedsRatherThanQueues`; the multi-tenant version — tenant A at 20× its limit while tenant B stays within SLO — needs a deployed target and has never been run. | <!-- doc-refs: allow-missing -->

---

## 2. PostgreSQL Row-Level Security

### 2.1 Roles

```sql
-- Migration role: owns the schema, runs DDL. Used ONLY by platformctl. Not BYPASSRLS —
-- the migration role owns the tables, and a table owner bypasses RLS by default unless
-- the policy is FORCED (see 2.2). That default is the trap this design avoids.
CREATE ROLE pp_migrate LOGIN;

-- Application role: the only role the services connect as.
-- NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE, and it owns nothing.
CREATE ROLE pp_app LOGIN NOSUPERUSER NOBYPASSRLS NOINHERIT NOCREATEDB NOCREATEROLE;
GRANT USAGE ON SCHEMA pp TO pp_app;
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA pp TO pp_app;
-- Deliberately no DELETE on financial tables: payments, payment_attempts, refunds,
-- ledger_entries and audit_records are append-only or update-only by design (§9, §13).
REVOKE DELETE ON pp.payments, pp.payment_attempts, pp.refunds,
                 pp.ledger_entries, pp.audit_records FROM pp_app;

-- Read-only analytics role, also non-BYPASSRLS, used by the reporting replica.
CREATE ROLE pp_readonly LOGIN NOSUPERUSER NOBYPASSRLS;
GRANT SELECT ON ALL TABLES IN SCHEMA pp TO pp_readonly;
```

**Why `pp_app` must not have `BYPASSRLS`.** `BYPASSRLS` makes every policy on every table inert for that role, silently and globally. There is no per-query opt-out and no warning. If the application role carried it, the RLS layer would exist in the schema, pass a naive "is RLS enabled" audit, and protect nothing — the worst possible state, because it produces false confidence. The same argument applies to connecting as the table owner: a table owner bypasses its own RLS unless the policy is `FORCE`d, which is why §2.2 uses `FORCE ROW LEVEL SECURITY` on every table rather than the plain `ENABLE`.

`TestAppRoleLacksBypassRLS` asserts `rolbypassrls = false` and `rolsuper = false` for `pp_app` on every environment at startup — the service refuses to become ready if the assertion fails. That is a deliberate fail-closed: running the data plane with an over-privileged database role is worse than not running it.

### 2.2 Policy DDL

```sql
-- Applied to every tenant-scoped table. This is the pattern; migrations/ carries one
-- instance per table, and TestEveryTenantScopedTableHasForcedRLS enforces completeness.

ALTER TABLE pp.payments ENABLE  ROW LEVEL SECURITY;
ALTER TABLE pp.payments FORCE   ROW LEVEL SECURITY;   -- applies to the owner too

-- Read/write policy. current_setting(..., true) returns NULL when the GUC is unset,
-- and NULL = anything is NULL, which is not TRUE, so an unset GUC yields ZERO rows
-- rather than ALL rows. That is the single most important line in this file.
CREATE POLICY tenant_isolation ON pp.payments
  FOR ALL
  TO pp_app
  USING       (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK  (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- Belt and braces: the column cannot be null, and an INSERT that omits it fails loudly
-- rather than being silently filtered by the policy.
ALTER TABLE pp.payments ALTER COLUMN tenant_id SET NOT NULL;

-- Index discipline: tenant_id leads every index so the planner can use the policy
-- predicate rather than filtering after the fact.
CREATE INDEX payments_tenant_merchant_created_idx
  ON pp.payments (tenant_id, merchant_id, created_at DESC);
CREATE UNIQUE INDEX payments_tenant_id_pk_idx
  ON pp.payments (tenant_id, payment_id);

-- The invariant from baseline §9 (I3), which is what makes double-charging structurally
-- impossible. Note it is tenant-scoped too.
CREATE UNIQUE INDEX payment_attempts_one_success_idx
  ON pp.payment_attempts (tenant_id, payment_id)
  WHERE outcome = 'SUCCESS';
```

Two details that matter:

- **`USING` and `WITH CHECK` must both be present.** `USING` filters what a statement can *see* (SELECT/UPDATE/DELETE); `WITH CHECK` constrains what it can *write* (INSERT/UPDATE). A policy with only `USING` lets tenant A **insert** a row stamped with tenant B's ID — it cannot read it back, but the row is there, it appears in B's queries, and it corrupts B's data.
- **`current_setting('app.tenant_id', true)`** — the `true` is `missing_ok`. Without it, an unset GUC raises an exception; with it, the expression is `NULL` and the policy evaluates to `NULL`, which PostgreSQL treats as not-true, so the result is zero rows. Failing closed with zero rows is preferable to an exception because it composes correctly inside joins and subqueries, where an exception in one branch would be reported as an opaque query failure.

### 2.3 The `SET LOCAL` protocol

```go
// internal/infrastructure/postgres/tx.go
func (d *DB) InTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
    tenant, ok := tenantctx.From(ctx)
    if !ok {
        return domain.ErrMissingTenantContext // never query without a tenant (baseline §16.2)
    }
    tx, err := d.pool.Begin(ctx)
    if err != nil {
        return err
    }
    defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

    // SET LOCAL, not SET. Scoped to this transaction; reverted on COMMIT or ROLLBACK.
    // Parameterized via set_config, not string concatenation — a tenant ID reaching
    // a SET statement by interpolation would be a SQL injection sink.
    if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant.ID); err != nil {
        return err
    }
    if err := fn(ctx, tx); err != nil {
        return err
    }
    return tx.Commit(ctx)
}
```

The third argument to `set_config` is `is_local` — `true` makes it transaction-scoped, exactly equivalent to `SET LOCAL`.

| Rule | Reasoning |
|---|---|
| Every query runs inside `InTx`, including single-statement reads | A bare `pool.Query` has no transaction, so `SET LOCAL` has nowhere to live, and a plain `SET` would leak (below). The uniformity is what makes the rule auditable |
| `SET LOCAL`, never `SET` | See §2.4 |
| `set_config($1, …)`, never string interpolation | `SET` does not accept bind parameters; `set_config` does. Interpolating into `SET` is injectable |
| Missing tenant → `ErrMissingTenantContext`, not a default | A default tenant is a backdoor with a friendly name |
| The GUC is set from `tenantctx`, which is populated only by the authentication middleware or by an event-envelope handler | One writer, many readers. Nothing else may call `tenantctx.With` — enforced by making the constructor package-private and exposing it through two audited entry points |

**Cross-tenant operations.** A small number of operations are legitimately platform-wide: the outbox relay, the reconciler sweeper, the audit anchoring job. These do not disable RLS. They run as a distinct role `pp_relay` whose policy is `USING (true)` on exactly the three tables it needs (`outbox_events`, `reconciliation_exceptions`, `audit_records`) and which has no access at all to `payments`, `merchants` or `configurations`. Widening a policy for a specific, minimal role is auditable; a global bypass is not.

### 2.4 Connection pooling and session GUCs — the PgBouncer bug

PgBouncer is deployed in **transaction pooling** mode (the only mode that gives meaningful connection multiplexing for a service with thousands of short transactions).

In transaction pooling, a server connection is returned to the pool at `COMMIT`/`ROLLBACK` and handed to a *different* client for the next transaction. Therefore:

| Statement | Scope | Behaviour under transaction pooling | Result |
|---|---|---|---|
| `SET app.tenant_id = 'A'` | **session** | The GUC persists on the server connection after `COMMIT`. The connection returns to the pool. The next transaction — possibly tenant B's — inherits `app.tenant_id = 'A'` | **Catastrophic.** Tenant B's queries are filtered by tenant A's ID. B sees A's data, or (if B's own `SET` lands first) A sees B's. Intermittent, load-dependent, and invisible in single-tenant testing |
| `SET LOCAL app.tenant_id = 'A'` | **transaction** | Reverted at `COMMIT`/`ROLLBACK`, before the connection is released | Correct. The next borrower starts with the GUC unset, and an unset GUC yields zero rows (§2.2) |

The failure mode is worth stating precisely because it is so easy to reach: `SET` (no `LOCAL`) works perfectly in every unit test, in every local development run with a direct connection, and in session-pooling mode. It fails only under transaction pooling, under concurrency, across tenants — that is, only in production.

Defences, layered:

| Layer | Control |
|---|---|
| Code | Only `InTx` may set the GUC, and it uses `set_config(..., true)`. There is no other call site |
| Lint | A custom analyzer rejects any SQL string literal matching `(?i)^\s*SET\s+(?!LOCAL)` outside migrations |
| PgBouncer | `server_reset_query = DISCARD ALL` on the session-pool fallback; `ignore_startup_parameters` limited; `pool_mode = transaction` set explicitly per database rather than inherited |
| Runtime assertion | In non-production environments, `InTx` asserts after `BEGIN` that `current_setting('app.tenant_id', true)` was empty *before* it set it. A non-empty value means a leaked session GUC and panics the test run |
| Test | `internal/infrastructure/postgres/rls_integration_test.go::TestSetLocalDoesNotLeakAcrossTransactions` — asserts the session GUC set inside one transaction is invisible to the next on the same connection, which is the property PgBouncer's transaction pooling depends on. With `SET` instead of `SET LOCAL` it fails immediately, which is exactly why it exists. It runs against Postgres directly; **no test runs through PgBouncer** | <!-- doc-refs: allow-missing -->

**Prepared statements.** Transaction pooling and server-side prepared statements interact badly (a statement prepared on one server connection is not present on another). `pgx` is configured with `QueryExecModeExec` / `statement_cache_capacity=0` behind PgBouncer, and PgBouncer runs `max_prepared_statements` > 0 only where the version supports protocol-level prepared statement tracking. This is a performance concern, not a correctness one, but it is the second thing that surprises people about transaction pooling and belongs next to the first.

### 2.5 The negative test

```go
// internal/infrastructure/postgres/rls_integration_test.go
func TestCrossTenantAccessIsImpossible(t *testing.T) {
    ctx := context.Background()
    a, b := seedTenant(t, "ten_A"), seedTenant(t, "ten_B")
    payA := seedPayment(t, a, money.USD(1050))
    payB := seedPayment(t, b, money.USD(2000))

    // Connect as pp_app — NOT as the migration role, NOT as postgres.
    db := connectAs(t, "pp_app")

    // Scoped to tenant B, run a query with NO tenant predicate at all.
    // If RLS is doing its job, the database filters; if the test passed only
    // because the repository added a WHERE clause, this would not prove anything.
    err := db.InTx(tenantctx.With(ctx, b), func(ctx context.Context, tx pgx.Tx) error {
        var ids []string
        rows, _ := tx.Query(ctx, `SELECT payment_id FROM pp.payments`) // deliberately unqualified
        for rows.Next() { var id string; _ = rows.Scan(&id); ids = append(ids, id) }

        require.NotContains(t, ids, payA.ID, "tenant A's payment visible under tenant B")
        require.Contains(t, ids, payB.ID)

        // Direct read by primary key must also return zero rows, not a permission error:
        // an error would be an existence oracle.
        var n int
        require.NoError(t, tx.QueryRow(ctx,
            `SELECT count(*) FROM pp.payments WHERE payment_id = $1`, payA.ID).Scan(&n))
        require.Equal(t, 0, n)

        // WITH CHECK: cannot write a row stamped with another tenant.
        _, werr := tx.Exec(ctx,
            `INSERT INTO pp.payments (payment_id, tenant_id, merchant_id, amount_minor, currency, state)
             VALUES ($1,$2,$3,$4,$5,'CREATED')`,
            ids2.New("pay"), a.ID, "mrc_x", 100, "USD")
        require.Error(t, werr, "WITH CHECK must reject a cross-tenant insert")
        return nil
    })
    require.NoError(t, err)
}

func TestQueryWithoutTenantContextReturnsNoRows(t *testing.T) {
    // Proves the fail-closed property of current_setting(..., true) being NULL.
    db := connectAs(t, "pp_app")
    tx, _ := db.Raw().Begin(context.Background()) // bypass InTx: no GUC is set
    defer tx.Rollback(context.Background())
    var n int
    require.NoError(t, tx.QueryRow(context.Background(),
        `SELECT count(*) FROM pp.payments`).Scan(&n))
    require.Equal(t, 0, n, "unset app.tenant_id must yield zero rows, never all rows")
}
```

---

## 3. Tenant context propagation

Tenant identity has exactly one origin and travels a defined path. Every hop either carries it or refuses to proceed.

```
Token claim  ──►  authn middleware  ──►  isolation guard  ──►  context.Context
  tenant_id        validates JWT          rejects body           tenantctx.With
                   (security.md §3.3)     tenant mismatch
                                                │
        ┌───────────────────────────────────────┼──────────────────────────────┐
        ▼                                       ▼                              ▼
  repository call                         gRPC to orchestrator            outbox write
  InTx → SET LOCAL app.tenant_id          metadata pp-tenant-id           events.tenantid
        │                                       │                              │
        ▼                                       ▼                              ▼
   RLS policy                            re-derived + re-guarded          Kafka envelope
                                                                               │
                                                                               ▼
                                                                     consumer: envelope →
                                                                     tenantctx.With → InTx
```

### 3.1 Derivation

| Source | Rule |
|---|---|
| JWT `tenant_id` claim | The authoritative source for all synchronous API traffic. Validated for `ten_` prefix and known-tenant existence |
| Request body / query / header | **Never** a source. If a `tenant_id` appears in a body and disagrees with the token: `403 TENANT_MISMATCH`, audit record, security event, page (baseline §16.2, `security.md` §9.1). If it agrees, it is ignored anyway — accepting it would create a code path where the body matters |
| Event envelope `tenantid` | The authoritative source for asynchronous consumers (§3.3) |
| Workflow instance | Persisted on the instance row; restored on lease acquisition (§3.4) |
| mTLS SPIFFE identity | Identifies the *workload*, never the tenant. `svc:internal` is not tenant-scoped, which is precisely why a propagated tenant context is mandatory for it (`security.md` §4.1) |

### 3.2 In-process: `context.Context`

```go
// internal/platform/tenantctx/tenantctx.go
type ctxKey struct{}

type Tenant struct {
    ID       string // ten_...
    Tier     Tier   // POOLED | SILOED_S1 | SILOED_S2
    Residency string // e.g. "eu-west-1"
    Source   Source // TOKEN | EVENT_ENVELOPE | WORKFLOW_INSTANCE — recorded for audit
}

// With is intentionally the only constructor and is called from exactly three places:
// the authn middleware, the event consumer's envelope decoder, and the workflow lease
// acquisition path. A lint rule enforces the call-site allowlist.
func With(ctx context.Context, t Tenant) context.Context { return context.WithValue(ctx, ctxKey{}, t) }

func From(ctx context.Context) (Tenant, bool) {
    t, ok := ctx.Value(ctxKey{}).(Tenant)
    return t, ok
}

// MustFrom is used at the boundary of the persistence layer. It returns an error, never
// panics, and never substitutes a default.
func MustFrom(ctx context.Context) (Tenant, error) {
    t, ok := From(ctx)
    if !ok { return Tenant{}, domain.ErrMissingTenantContext }
    return t, nil
}
```

| Rule | Enforcement |
|---|---|
| Every repository method takes `ctx` first | Lint rule `repository-requires-context` (`security.md` §6.4) |
| No tenant in context → `ErrMissingTenantContext`, no query issued | `InTx` (§2.3); tested by `TestQueryWithoutTenantContextReturnsNoRows` |
| Goroutines inherit the parent's context | Lint rule forbidding `context.Background()` outside `cmd/**`, test files, and explicitly-annotated detached-work sites |
| Detached work (fire-and-forget) | Uses `context.WithoutCancel(ctx)`, which preserves values (including the tenant) while dropping the deadline. `context.Background()` in a goroutine is the classic way to lose the tenant |

### 3.3 Across async boundaries: the event envelope

`tenantid` is a required extension of the CloudEvents envelope (baseline §13.1). The codec enforces it:

```go
// internal/events/codec.go
func (c *Codec) Encode(ctx context.Context, e Event) ([]byte, error) {
    t, err := tenantctx.MustFrom(ctx)
    if err != nil {
        return nil, fmt.Errorf("encode %s: %w", e.Type, err) // cannot publish without a tenant
    }
    env := Envelope{ /* … */ TenantID: t.ID, MerchantID: e.MerchantID, /* … */ }
    return json.Marshal(env)
}

func (c *Codec) Decode(b []byte) (Envelope, error) {
    var env Envelope
    if err := json.Unmarshal(b, &env); err != nil { return env, err }
    if !ids.HasPrefix(env.TenantID, "ten_") {
        return env, ErrEnvelopeMissingTenant // reject before any handler sees it
    }
    return env, nil
}
```

The consumer restores it before dispatching to a handler, and — importantly — **re-validates it against the payload**:

```go
// internal/events/consumer.go
func (c *Consumer) handle(ctx context.Context, msg kafka.Message) error {
    env, err := c.codec.Decode(msg.Value)
    if err != nil { return c.toDLQ(ctx, msg, err) }

    tenant, err := c.tenants.Lookup(ctx, env.TenantID) // resolves tier + residency
    if err != nil { return c.toDLQ(ctx, msg, err) }
    ctx = tenantctx.With(ctx, tenant)

    // Defence in depth: if the payload also carries a tenant_id (some do, for
    // convenience), it must agree with the envelope. Disagreement is a security event,
    // not a warning — it means either a producer bug or a forged message.
    if pid := env.PayloadTenantID(); pid != "" && pid != env.TenantID {
        c.security.Emit(ctx, security.TenantMismatchInEvent(env))
        return c.toDLQ(ctx, msg, ErrEnvelopeTenantMismatch)
    }

    // Dedup + handler inside ONE transaction, which also carries SET LOCAL (baseline §13.5).
    return c.db.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
        inserted, err := dedupInsert(ctx, tx, c.group, env.ID)
        if err != nil || !inserted { return err } // already processed → ACK and drop
        return c.dispatch(ctx, tx, env)
    })
}
```

Because the dedup insert, the handler's writes and the `SET LOCAL` all share one transaction, an effectively-once consumer is also a tenant-scoped consumer — the two properties are enforced by the same construct rather than by two independent disciplines.

### 3.4 Workflow activities

The workflow engine leases instances (baseline §11). The tenant is a column on `workflow_instances`, written at start from the initiating request's context, and restored on every lease acquisition:

```go
func (w *Worker) runStep(ctx context.Context, inst Instance, step Step) error {
    ctx = tenantctx.With(ctx, w.tenants.MustLookup(ctx, inst.TenantID))
    ctx = merchantctx.With(ctx, inst.MerchantID)
    ctx, span := w.tracer.Start(ctx, "workflow.step", trace.WithAttributes(
        attribute.String("workflow", inst.Workflow), attribute.String("step", step.ID)))
    defer span.End()
    return step.Activity(ctx, inst.Payload) // every activity now sees the right tenant
}
```

The same restoration happens for compensations, for DLQ replay, and for the Temporal adapter (where the tenant travels in the workflow input and in a search attribute, never in a worker-global). A step's activity never reads a tenant from its payload — it reads it from the context, so a tampered payload cannot redirect it.

---

## 4. Noisy-neighbour control

Isolation of *data* is a correctness property; isolation of *capacity* is an availability property, and it is the one that fails more often. Every shared resource has a per-tenant quota.

### 4.1 The quota table

| Resource | Mechanism | Default per tenant | Reasoning |
|---|---|---|---|
| **API rate** | Distributed token bucket in Redis, keyed `rl:{tenant}:{merchant}:{route_class}`; local in-process bucket as the fallback when Redis is unavailable | 500 rps sustained, burst 1 000 (tier-dependent), per merchant 100 rps | 500 rps is 10 % of the 5 000 TPS regional target (§18), so no single tenant can consume more than a tenth of capacity without an explicit contract |
| **Concurrency** | Weighted semaphore per tenant in `payment-api` and per `(tenant, gateway)` in `payment-orchestrator` | 64 in-flight per tenant per pod; 32 per `(tenant, gateway)` | Sizing math in `failure-handling.md` §6. Concurrency, not rate, is what protects a connection pool: a rate limit permits 500 rps of 10-second requests |
| **DB connections** | PgBouncer per-database pool + per-service `max_conns`; no per-tenant pool (that would be thousands of pools) — tenant fairness comes from the concurrency semaphore upstream | Pool 200 per writer, 400 per reader | A per-tenant pool at 500 tenants is unimplementable; bounding concurrency *before* the pool achieves the same fairness with one pool |
| **Query cost** | `SET LOCAL statement_timeout` per request class: 250 ms data-plane reads, 2 s writes, 30 s control-plane reports. Plus a row-limit ceiling of 1 000 on every list endpoint (cursor pagination, baseline §19.3) and a planner guard that rejects a plan whose estimated cost exceeds a threshold for interactive routes | 250 ms / 2 s / 30 s | An unbounded query from one tenant's report holds a connection and a snapshot, blocking vacuum and inflating bloat for everyone. `statement_timeout` is set per transaction via the same `SET LOCAL` mechanism as the tenant GUC |
| **Kafka** | MSK client quotas per principal: produce and consume byte-rate, plus request-percentage quota. Siloed tenants have their own principals and therefore their own quotas; pooled tenants share a service principal, so fairness there is enforced upstream by the API rate limit | 20 MB/s produce, 50 MB/s consume, 200 % request rate per service principal | Request-percentage quota is the one that matters: a tenant generating many tiny events starves the broker's request handlers long before it saturates bytes |
| **Cache memory** | Per-tenant byte accounting maintained on write; on breach, that tenant's namespace is evicted LRU-first — **not** the shared instance's global LRU | 256 MB pooled, unbounded within the tenant's own instance for siloed | Redis' global LRU is the classic noisy-neighbour amplifier: a tenant writing a large working set evicts every other tenant's hot configuration, converting one tenant's inefficiency into everyone's cache-miss storm |
| **Outbound gateway concurrency** | Bulkhead per `(gateway, tenant_tier)` in the orchestrator, plus a per-gateway global bulkhead | Per gateway: 200 global, 32 per tenant | Protects the shared gateway connection budget and our standing with the gateway, whose own rate limits are per-account |
| **Workflow slots** | `workflow-worker` leases at most N instances per tenant concurrently | 16 per tenant per worker | Prevents one tenant's 5 000-merchant bulk onboarding from starving another's single urgent one |
| **Webhook delivery** | Per-merchant delivery concurrency 4, per-tenant 64 | | A merchant with a dead endpoint must not consume the delivery pool |
| **Storage** | Per-tenant S3 prefix size and object-count monitoring, alert at 80 % of contract | | Cost control and a leading indicator of misuse |

### 4.2 Fairness algorithm

Rate limiting alone is not fairness: at saturation, every tenant is at its limit and the aggregate still exceeds capacity. Under overload the scheduler must degrade *proportionally*, not first-come-first-served.

**Deficit round-robin over per-tenant queues, with priority classes.**

```
Admission (per request):
  1. Token bucket for (tenant, merchant, route_class).
     Empty → 429 RATE_LIMITED with Retry-After and RateLimit-* headers (§19.3).
  2. Per-tenant concurrency semaphore, non-blocking TryAcquire.
     Full → enqueue in the tenant's local queue (bounded, depth 128).
     Queue full → 429 immediately. A queue that grows without bound is a latency bomb;
     shedding at the door is honest.
  3. Priority class from the operation (§5 of failure-handling.md):
     P0 refund/void > P1 capture > P2 authorize > P3 read > P4 report.

Scheduler (per worker tick):
  deficit[t] += quantum[t]                     // quantum ∝ tenant's contracted share
  for each tenant t in round-robin order:
      while deficit[t] >= cost(head(queue[t])):
          deficit[t] -= cost(head(queue[t]))
          dispatch(pop(queue[t]))
  // A tenant that is idle accumulates at most one quantum of credit (capped), so
  // an idle tenant returning to traffic gets prompt service without being able to
  // bank a burst that starves everyone else.

Under sustained overload (adaptive limiter reduces global concurrency):
  quantum[t] scales down proportionally for every tenant, so each tenant loses the
  same FRACTION of its share. Absolute equality would punish large tenants for being
  large; proportional degradation preserves the contracted ratio.
```

| Parameter | Value | Reasoning |
|---|---|---|
| `quantum[t]` | Proportional to the tenant's contracted rps, normalized so the sum equals available concurrency | Makes the contract, not arrival order, decide the share |
| Idle credit cap | 1 quantum | Without a cap, a tenant silent for an hour arrives with an hour of credit and monopolizes the scheduler |
| Queue depth | 128 per tenant | At 64 concurrency and ~50 ms service time, 128 queued ≈ 100 ms of extra latency — within the 250 ms p99 budget (§18). Deeper queues buy nothing but latency |
| `cost(req)` | 1 for reads, 3 for payment writes, 5 for refunds | Reflects real resource consumption, so a tenant sending only expensive operations does not get the same throughput as one sending cheap ones |
| Priority inversion guard | P0 (refund/void) bypasses the deficit scheduler up to a reserved 10 % of concurrency | Baseline §8: you must always be able to give money back, even under overload |

Local-vs-distributed: the token bucket is authoritative in Redis. On a Redis outage each pod falls back to a local bucket sized `global_limit / replica_count × 1.2`, which over-admits slightly (the 1.2 accounts for uneven load balancing) and is corrected as soon as Redis returns. Baseline §24: "Redis loss → local token bucket, limits coarser" — coarser, never absent.

---

## 5. Tenant tiers

| | **Pooled** (default) | **Siloed S1** (schema) | **Siloed S2** (infrastructure) |
|---|---|---|---|
| Database | Shared cluster, shared schema, RLS | Shared cluster, **dedicated schema**, RLS still on | **Dedicated Aurora cluster** |
| KMS | Environment CMK | **Per-tenant CMK** | Per-tenant CMK, optionally customer-managed external key |
| Cache | Shared Redis, key prefix + quota | Dedicated logical DB | **Dedicated Redis cluster** |
| Kafka | Shared topics, envelope filtering | Shared topics, dedicated consumer groups | **Dedicated topics + ACLs + quotas** |
| Compute | Shared pods, per-tenant bulkhead | Shared pods, reserved concurrency floor | **Dedicated node group + namespace** |
| Object storage | Shared bucket, prefix + IAM condition | Shared bucket, per-tenant CMK | **Dedicated bucket** |
| Logs/metrics | Shared, tenant-filtered views | Shared, dedicated index pattern | **Dedicated log group + dashboards** |
| Noisy-neighbour exposure | Bounded by quotas | Bounded by quotas + reserved floor | None (physical) |
| Blast radius of a platform-wide DB incident | Shared | Shared cluster, isolated schema | Isolated |
| Maintenance windows | Platform-wide | Platform-wide | **Tenant-specific** |
| Data residency | Regional pool | Regional pool | **Tenant-chosen region** |
| Restore granularity | Cluster-level PITR; single-tenant restore requires filtered export | Schema-level restore | **Cluster-level PITR for that tenant alone** |
| Marginal infra cost | ~$0 | ~$120/mo (schema overhead, separate connections, separate CMK) | ~$2 400/mo (cluster, Redis, node group, topics) |
| Typical fit | The default; SMB and mid-market PSPs | Regulated tenants needing schema separation for audit | Bank/large-PSP contractual isolation, dedicated maintenance windows, residency requirements |

**What does not change across tiers.** RLS stays on. The tenant guard stays on. The isolation tests run identically. A siloed tenant is not permitted to be a place where the pooled controls are relaxed — a tenant that pays for more isolation gets more isolation, not different code paths. There is exactly one repository implementation; the tier selects a connection factory and a key policy, nothing else.

### 5.1 Migrating pooled → siloed

Online, reversible, with a bounded write-freeze measured in seconds rather than minutes.

| # | Phase | Action | Rollback |
|---|---|---|---|
| 1 | Provision | Terraform applies the target schema/cluster, per-tenant CMK, Redis DB, topics, ACLs and node group. Migrations run to the same version as the source | Destroy; no production impact |
| 2 | Backfill | Logical replication (or `COPY` for small tenants) of the tenant's rows, filtered by `tenant_id`, into the target. Runs against a read replica so the writer is untouched | Drop the target data |
| 3 | Verify | Row counts and per-table checksums per tenant compared source↔target; ledger balances recomputed and compared; audit hash chain re-verified end-to-end on the target | Repeat backfill |
| 4 | Catch-up | Replication follows until lag < 1 s | — |
| 5 | **Freeze** | The tenant is placed in `MIGRATING`: new payment writes return `503 SERVICE_UNAVAILABLE` with `Retry-After: 5`; **reads continue**; refunds and voids continue against the source until step 6. Expected duration ≤ 30 s | Clear the flag; nothing has moved |
| 6 | Cut over | Drain in-flight transactions, wait for lag = 0, flip the tenant's routing record (`tenants.tier`, connection factory, cache namespace, topic set). Configuration cache invalidated with priority | Flip back — the source is still intact and still current, because step 7 has not run |
| 7 | Unfreeze + soak | Traffic resumes on the target. The source rows are retained read-only for 30 days. Reverse replication is **not** enabled; rollback within the soak window means a second, reversed migration | — |
| 8 | Reclaim | After 30 days and a clean reconciliation run, source rows are deleted and the pooled quota is released | — |

| Guard | Reasoning |
|---|---|
| The freeze rejects payment *writes*, never refunds or voids | Baseline §8: you must always be able to give money back |
| No dual-write | A dual-write across two clusters during cutover reintroduces exactly the dual-write failure mode the outbox exists to eliminate (baseline §13.4). A brief freeze is simpler and provably correct |
| Idempotency records migrate with the tenant | Otherwise a client retrying across the cutover gets a second execution — the one bug that must not happen during a migration |
| Outbox rows migrate **unpublished-only**; published rows stay behind | Re-publishing already-published events is safe by §13.5 but generates avoidable noise; unpublished rows must move or events are lost |
| The audit chain is re-verified on the target before cutover, and the migration itself is an audit record | An unverifiable chain after a migration is indistinguishable from tampering |
| Runbook | `docs/runbooks/tenant-tier-migration.md`; drilled quarterly against a synthetic tenant in staging |

---

## 6. Tenant lifecycle

| Phase | Actions | Guards | Events |
|---|---|---|---|
| **Provisioning** | Create `tenants` row (`ten_` ULID); assign tier and residency region; create KMS CMK (siloed) or attach the environment CMK; create the OAuth2 client and its first credential; create IAM path scoping and the S3 prefix; register Kafka ACLs (siloed); seed default quotas and policies from the seed profiles in `config/seed/`; create the audit chain genesis record | `platform-admin` + dual control. Residency region must be a supported region; tier must be one the contract covers | `tenant.provisioned.v1` |
| **Configuration bootstrap** | Seed default routing weights, risk policy, retention policy, notification endpoints, feature flags; create the first configuration version (`v1`); run L4 validation | Bootstrap is a normal versioned config write, so it is validated, audited and rollback-able like any other. There is no privileged "seed" path that skips validation | `configuration.published.v1` |
| **Active** | Normal operation | | |
| **Suspension** | Tenant-level: new payments rejected `403 TENANT_SUSPENDED`; **refunds, voids and webhook processing continue** (baseline §8, applied at tenant scope); onboarding workflows pause at their next checkpoint rather than being killed; configuration writes rejected; reads continue | Reversible. Reason recorded (non-payment, compliance, risk, contractual, tenant request). Automated suspension is available to the risk/compliance automation, not only to humans | `tenant.suspended.v1` (priority cache invalidation) |
| **Reinstatement** | Reverse of suspension; paused workflows resume from their checkpoint | Dual control if the suspension reason was compliance or risk | `tenant.reinstated.v1` |
| **Offboarding — notice** | 30-day notice period. New payments blocked; refunds, voids, disputes, settlement ingestion and reconciliation continue; the tenant may export their data (`audit:export`, payment export, configuration export) | No payment may be in a non-terminal state at the end of the window; the offboarding job reports the open set daily | `tenant.offboarding_started.v1` |
| **Offboarding — closure** | All merchants `→ TERMINATED` (which itself requires zero non-terminal payments, baseline §8); gateway connections de-provisioned; webhooks deregistered at each gateway; credentials revoked at the gateway *then* deleted from Secrets Manager; Kafka ACLs and consumer groups removed; node group/topics destroyed for siloed | Dual control. Blocked while any payment is non-terminal, any dispute is open, or any critical reconciliation exception is unresolved | `tenant.offboarded.v1` |
| **Deletion** | See §6.1 | Dual control + a 30-day cooling-off period after closure before key destruction | `tenant.data_erased.v1` |

### 6.1 Deletion and crypto-shredding

Physical erasure of every copy of a tenant's rows — across the primary, three AZ replicas, a cross-region secondary, WAL, automated backups, snapshots, S3 versions and Kafka retention — cannot be completed in a bounded, provable time. Key destruction can.

| Step | Action | Effect |
|---|---|---|
| 1 | Verify the carve-out set (below) has been re-encrypted under the **retention key**, a separate CMK with a different key policy and a 7-year scheduled lifetime | Financial records survive the shred |
| 2 | Delete the tenant's DEKs from the key table; delete cached DEKs from every pod (a fleet-wide cache purge signal) | In-memory copies gone |
| 3 | `ScheduleKeyDeletion` on the tenant CMK with the maximum 30-day window (siloed), or destroy the tenant DEK wrapped under the environment CMK (pooled) | Every ciphertext encrypted under that key becomes permanently unrecoverable, including in backups and snapshots |
| 4 | Best-effort physical deletion of non-encrypted, non-carve-out rows and objects: `DELETE` under RLS in batches, S3 prefix delete with version expiry, Kafka topic delete (siloed) or reliance on the ≤ 30 d retention (pooled) | Reduces surface; not the mechanism relied upon |
| 5 | Emit `tenant.data_erased.v1` and write a final audit record naming the destroyed key IDs, the carve-out set, the legal basis and the approvers. **This record is retained** | Provable erasure |
| 6 | Verify: a restore of a pre-deletion snapshot into an isolated account must fail to decrypt the tenant's fields. Drilled during the DR exercise | Proof, not assertion |

**The financial-records carve-out.** GDPR Art. 17(3)(b) — processing necessary for compliance with a legal obligation — and the AML/payments retention regimes require certain records to survive an erasure request. The carve-out set is exactly:

| Retained | Retention | Form after shredding |
|---|---|---|
| `payments`, `payment_attempts`, `refunds` — amount, currency, timestamps, state history, gateway reference, merchant ID | 7 years | Retained in full; these fields are not personal data. Any personal-data field is replaced by a tombstone |
| `ledger_entries` | 7 years | Retained in full, append-only |
| `audit_records` | 7 years, WORM | Retained in full; the hash chain must remain verifiable, so records are never deleted or rewritten. Personal data inside an audit record is stored envelope-encrypted under the retention key so it is retained-but-protected |
| KYC decision + evidence reference | ≥ 5 years (`compliance.md` §5) | Retained under the retention key |
| Everything else — merchant principal PII, contact details, KYC document contents, support correspondence, logs, projections, cache | — | Unrecoverable at step 3 |

The carve-out is enumerated in `internal/domain/compliance/retention.go` and asserted by `internal/domain/compliance/retention_test.go::TestErasureCarveOut` and `::TestErasureRequestCompleteRespectsTheCarveOut`. It is not yet backed by a machine-readable policy file the retention job reads; see `compliance.md` §6. Full legal reasoning in `compliance.md` §4.5.

---

## 7. Cross-tenant leakage checklist (code review)

Applied to every PR that touches persistence, caching, events, logging, workflows or configuration. A reviewer who cannot answer "yes" to the relevant lines blocks the PR.

**Data access**
- [ ] Every new query runs inside `InTx` (so `SET LOCAL app.tenant_id` is in force). No `pool.Query` outside `internal/infrastructure/postgres`.
- [ ] Every new table that holds tenant-scoped data has `tenant_id NOT NULL`, `ENABLE` **and** `FORCE ROW LEVEL SECURITY`, and a policy with **both** `USING` and `WITH CHECK`.
- [ ] Every new index leads with `tenant_id`. Every new unique constraint is scoped by `tenant_id` (a global unique on `merchant_id` would collide across tenants — baseline §2 says `merchant_id` is unique *within* a tenant).
- [ ] No `WHERE tenant_id = $1` written by hand as the *only* protection — RLS is the enforcement; an explicit predicate is an optimization, not a control.
- [ ] No new grant to `pp_app` beyond the minimum; no `BYPASSRLS` anywhere; no new `SECURITY DEFINER` function (it runs as its owner and bypasses the caller's RLS).
- [ ] Any new cross-tenant job uses a dedicated minimal role with a narrow `USING (true)` policy on named tables, not a global bypass.

**Identity and context**
- [ ] Tenant is read from `tenantctx`, never from a request body, query parameter, header, path segment or event payload.
- [ ] No new call site of `tenantctx.With` outside the three allowlisted entry points.
- [ ] Every new goroutine inherits the caller's context; detached work uses `context.WithoutCancel(ctx)`, not `context.Background()`.
- [ ] Any new internal gRPC handler re-derives and re-guards the tenant rather than trusting the caller.

**Cache and storage**
- [ ] Every cache key is built via `cache.Key(ctx, …)`. No string-concatenated keys.
- [ ] Every S3 path is built via `objectstore.Path(ctx, …)` and begins with the tenant prefix.
- [ ] Any new cache entry has a defined tenant memory-quota accounting path.

**Events and async**
- [ ] Every new event type is published through the codec (so `tenantid` is mandatory).
- [ ] Every new consumer restores the tenant from the envelope before touching data, and validates payload-vs-envelope agreement.
- [ ] Every new workflow activity reads the tenant from context, not from its payload.
- [ ] DLQ replay paths restore the tenant from the archived envelope, not from an operator-supplied parameter.

**Observability**
- [ ] No new metric label containing `tenant_id` on a high-cardinality series; no `merchant_id` or `payment_id` label at all.
- [ ] Every new log call uses registered `logx` fields; no `%+v`, no `Any`.
- [ ] No new error message interpolates a request struct.

**Capacity**
- [ ] Any new shared resource (pool, semaphore, queue, buffer, external API budget) has a per-tenant bound.
- [ ] Any new unbounded loop over tenant data is paginated and rate-limited.

**Tests**
- [ ] A negative test exists proving the new path returns zero rows / `403` / `AccessDenied` for a foreign tenant, and it asserts at the *enforcement* layer (database, IAM), not at the application wrapper.
- [ ] `TestEveryTenantScopedTableHasForcedRLS` still passes (it will fail automatically on a new table without a policy — do not skip it).
