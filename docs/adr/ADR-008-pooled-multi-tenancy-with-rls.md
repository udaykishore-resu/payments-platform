# ADR-008: Pooled multi-tenancy with PostgreSQL Row-Level Security, siloed tier available

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §16 (multi-tenancy model), §1.3 ambiguity A3, §18 (merchant scale) of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Related to ADR-002 (PostgreSQL); constrains ADR-020 (topics), ADR-021 (regions)

## Context

Baseline §18 targets **50 000 merchants across 500 tenants** in a single logical platform, with
5 000 TPS sustained per region. Baseline §16 fixes the isolation matrix. This ADR records why
the pooled-with-RLS model was chosen and what it costs.

The forces:

1. **Economics.** A dedicated Aurora cluster costs roughly $300–$1 500/month at minimum viable
   size (writer + one reader, small instance classes) before storage and I/O. At 500 tenants that
   is $150 k–$750 k/month in database spend for a platform whose aggregate workload comfortably
   fits on a handful of clusters. Most tenants are small: the distribution is heavily skewed, and
   a per-tenant cluster means paying for the p99 tenant 500 times.
2. **Operational surface.** 500 clusters means 500 sets of parameter groups, 500 failover events
   to observe, 500 backup verifications, and — the killer — **500 schema migrations per release**.
   A migration that takes 40 s and must run serially with verification is ~6 hours of release
   window; run in parallel it is a self-inflicted thundering herd against the control plane. A
   migration that fails on tenant 383 leaves the fleet in mixed schema state.
3. **Connection economics.** PostgreSQL connections cost ~5–10 MB of backend memory each. Pooled,
   the data plane holds one pool of a few hundred connections. Database-per-tenant means either
   500 pools (memory-prohibitive on the application side) or connection churn per request
   (latency-prohibitive against a 5 ms stage budget).
4. **Isolation is a hard requirement, not a nice-to-have.** A cross-tenant read is a reportable
   security incident and, for a payments platform, potentially a contractual and regulatory
   breach. Whatever model we choose must make cross-tenant access *structurally* impossible, not
   merely absent from the code.
5. **Some tenants will contractually require isolation.** Large PSPs and regulated tenants will
   ask for a dedicated database, a dedicated KMS key, and an attestation. Saying "no" loses those
   deals; saying "yes" for everyone loses the economics.
6. **Noisy neighbours are real.** One tenant's bulk reconciliation query must not degrade another
   tenant's payment latency.

What breaks if we choose wrong: at one extreme, an unbounded operational and cost burden that
makes the platform unshippable; at the other, a single missing `WHERE tenant_id = ?` that leaks
one merchant's payment data to another.

## Decision

**Pooled by default — shared cluster, shared schema, `tenant_id` on every table, PostgreSQL
Row-Level Security enforced by the database — with a siloed tier (dedicated schema or dedicated
cluster) available per tenant tier.**

Binding specifics:

1. Every tenant-scoped table carries a non-null `tenant_id` and has RLS **enabled and forced**
   (`ALTER TABLE … ENABLE ROW LEVEL SECURITY; … FORCE ROW LEVEL SECURITY`). Policies compare
   `tenant_id` to `current_setting('app.tenant_id')`.
2. The application connects as a role that **does not** have `BYPASSRLS` and is not the table
   owner. The migration role is separate and does have owner rights; the two roles are never the
   same credential.
3. Every transaction begins with `SET LOCAL app.tenant_id = $1`, set by a single wrapper in
   `internal/infrastructure/postgres`. `SET LOCAL` (not `SET`) so the value cannot leak across a
   pooled connection's next transaction. A checked-out connection with a stale GUC is a bug the
   `LOCAL` scope makes impossible.
4. Tenant identity comes **exclusively** from the authenticated principal (§16.2). A `tenant_id`
   in a request body or query string that disagrees with the token is a security event:
   `403 TENANT_MISMATCH` + audit + alert.
5. Repository methods take `context.Context`; a call with no tenant in context returns
   `ErrMissingTenantContext` rather than issuing a query. Defence in depth: application guard →
   RLS policy → integration test.
6. **Siloed tier** is the same code path with a different connection target and a per-tenant KMS
   CMK, dedicated Redis DB, dedicated topics and dedicated log group (§16.1). Silo is a
   *deployment* decision, not a code fork — there is exactly one data-access implementation.
7. Noisy-neighbour control is per-tenant rate limiting and a concurrency bulkhead at §12 stage 6,
   plus per-tenant cache memory quotas.

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Pooled + RLS (chosen)** | One cluster to operate, back up, patch and fail over; one migration per release; connection pooling works; cost scales with *load*, not tenant count; isolation enforced by the database engine, below the application, so an application bug cannot bypass it; siloed tier available without a code fork | RLS adds a predicate to every query — measurable but small overhead (typically 3–8 % on indexed point lookups when `tenant_id` leads the index; worse if indexes are not designed for it); a `BYPASSRLS` role or a table missing `FORCE` silently disables protection; noisy neighbours need explicit controls; a corrupted shared cluster affects all tenants | **Accepted** |
| **Database (or cluster) per tenant** | Strongest isolation, trivially explained to auditors; per-tenant backup/restore and point-in-time recovery; per-tenant scaling; blast radius of a bad query is one tenant; per-tenant residency is easy | 500 clusters × migrations, patching, failovers, backups; database spend measured in hundreds of thousands per month for a workload that fits on a few clusters; connection-pool explosion; provisioning a new tenant becomes an infrastructure operation with minutes-to-hours of latency instead of a row insert; cross-tenant platform analytics requires a fan-out over 500 databases. This is the option a security-minded engineer pushes for, and it is genuinely the strongest isolation — it loses on migration operability first (mixed-schema fleets are a correctness hazard, not just a chore) and cost second | Rejected as the default; **retained as the siloed tier** for tenants who require and fund it |
| **Schema per tenant (shared cluster)** | Better isolation than pooled without per-cluster cost; `search_path` switching is cheap; per-tenant restore is possible with more effort | PostgreSQL's catalog is the bottleneck: 500 tenants × ~40 tables = 20 000 tables, plus indexes; `pg_class` bloat degrades planning time and `pg_dump`, autovacuum scheduling and connection startup all suffer; DDL must be applied 500 times anyway, so the migration problem is *not* solved; prepared-statement caches are per-schema, so a pooled connection switching `search_path` invalidates plans; partitioning (§9 A-02) multiplies the table count further. Gets most of the cost of silo with much less of the benefit | Rejected — worst of both, at our table count |
| **Application-only filtering (`WHERE tenant_id = ?` everywhere)** | Zero database-level complexity; no RLS overhead; no GUC plumbing; works identically on any database | One forgotten predicate is a cross-tenant data breach, and the failure is silent — the query succeeds and returns more rows than it should. Code review and linting cannot prove absence over thousands of queries, including ad-hoc ones, reporting queries, and anything an engineer runs in a console. For a payments platform this is an unacceptable residual risk: the control is one careless developer thick | Rejected as the *only* control; retained as the **first** layer of defence in depth |
| **Pooled + RLS + separate cluster per region only** | — | This is the chosen option; region separation is orthogonal and handled in ADR-021 | n/a |

## Consequences

### Positive

- Isolation is enforced by PostgreSQL itself: a query missing its tenant predicate returns zero
  rows rather than another tenant's data. The failure mode is "no data", which is loud, instead of
  "wrong data", which is silent.
- One migration per release. Tenant provisioning is an `INSERT`, so a new tenant is live in
  milliseconds.
- Cost tracks load, not tenant count.
- The siloed tier is a commercial product feature that costs no additional code paths.

### Negative

- Every index on a tenant-scoped table must lead with `tenant_id` or the RLS predicate forces a
  filter after the index scan. This is a permanent design constraint on the data model and it
  will occasionally cost us a natural index ordering.
- RLS is silently bypassable by misconfiguration: a role with `BYPASSRLS`, a table owner
  connection, or a table where RLS was enabled but not `FORCE`d. This must be tested, not assumed.
- A shared cluster means a shared failure domain and shared resource limits; noisy-neighbour
  control is now our problem to solve explicitly.
- `SET LOCAL` requires every query to run inside a transaction, including simple reads. This is a
  small but real constraint on the data-access layer.

### Neutral / accepted costs

- Two tiers means two operational runbooks (pooled and siloed), even though the code is one path.
- Cross-tenant platform analytics must be built deliberately (an aggregate pipeline with an
  explicit, audited privileged role) rather than falling out of a shared schema.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| A new table ships without RLS enabled/forced | Medium | **Critical** — silent cross-tenant exposure | CI check `TestEveryTenantScopedTableHasForcedRLS` enumerates `pg_class`/`pg_policy` after migrations and fails on any tenant-scoped table without `relrowsecurity` **and** `relforcerowsecurity` and a matching policy | The CI check; a periodic production job asserting the same |
| Application role acquires `BYPASSRLS` or connects as owner | Low | **Critical** | Role grants are terraform-managed; a startup assertion queries `pg_roles` for the current user and refuses to start if `rolbypassrls` is true or the user owns tenant tables | Startup failure; drift detection in terraform plan |
| Missing `SET LOCAL` on some code path | Medium | High — query returns zero rows (fails safe) but breaks the feature | Single wrapper owns transaction start; `ErrMissingTenantContext` on repository calls without context; RLS makes the failure a visible zero-row result rather than a leak | Spike in zero-row results / `ErrMissingTenantContext` counter |
| Index design ignores RLS, causing planner regressions at scale | Medium | Medium — latency, not correctness | `tenant_id` leads composite indexes by convention; `EXPLAIN` assertions in `tests/integration` for the hot queries; load test at 50 000 merchants of seeded data | Query p99 divergence between load-test and production shapes |
| Noisy neighbour degrades others | Medium | Medium | Per-tenant rate limit + concurrency bulkhead at §12 stage 6; per-tenant cache quotas; statement timeouts on analytic queries | `pp_http_requests_total{status="429"}` by tenant; per-tenant p99 divergence |
| Siloed tier drifts into a code fork | Medium | Medium — doubles maintenance | Silo differs only in connection target and key material; a `tier` conditional in business logic is a review-blocking defect | Code search for `tier ==` outside infrastructure packages |
| Shared-cluster incident affects all tenants at once | Low | High | Aurora multi-AZ with ≤ 60 s failover (§18); the siloed tier exists precisely for tenants who will not accept this | Cluster-level health; correlated multi-tenant error spike |

## Validation

- **The definitive test:** `TestCrossTenantAccessIsImpossible` (§16.2) — under tenant A's context,
  query for a row known to belong to tenant B by primary key. Assert **zero rows at the database
  level**, with the application guard deliberately disabled so the test proves RLS, not the guard.
- **Overhead measurement:** benchmark the hot payment queries with RLS on and off. Accept ≤ 10 %
  p99 overhead; anything more triggers an index-design review before it triggers a model review.
- **Scale test:** load the pooled cluster with 500 tenants × 100 merchants and the §18 payment
  volume; assert p99 stays inside the §12 stage budgets.
- **Migration time:** a representative migration must complete in one window for the pooled fleet.
  If the siloed tier grows to the point where its serialized migration time exceeds the release
  window, that is the signal in the revisit criteria.

## Revisit criteria

Reopen if:

1. Siloed tenants exceed ~25, at which point the silo fleet has the migration and operations
   problem that disqualified database-per-tenant, and needs its own automation model.
2. A single tenant's data volume or query load exceeds ~20 % of the pooled cluster's capacity —
   at that point they should be moved to the siloed tier regardless of contract.
3. Measured RLS overhead exceeds 10 % p99 on the payment path after index tuning.
4. A data-residency regime requires per-tenant physical placement for a material fraction of
   tenants, making silo the effective default.
5. Any cross-tenant exposure incident occurs — the postmortem reopens this ADR by default,
   whether or not RLS was the failing control.
