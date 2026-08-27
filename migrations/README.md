# Migrations

Forward-only, numbered, paired. `NNNN_<slug>.up.sql` and `NNNN_<slug>.down.sql`. Numbering is
contiguous from `0001`; a gap is a merge accident and CI fails on it
(`TestMigrationNumberingIsContiguous`).

The schema itself is documented in `docs/spec/04-domain-model.md` §6–§9. This file is about how a
migration *runs*, and what makes one safe.

---

## 1. Expand / contract

Every schema change that could be observed by a running process is split into an **expand**
migration, a **backfill**, and a **contract** migration, deployed in that order with a release
boundary between each. The reason is that during a rolling deploy, the old binary and the new
binary run against the same database at the same time — for minutes, and for hours if the
rollout is paused. A change that is only correct for one of them is an outage for the other.

| Phase | What it may do | What it may not do |
|---|---|---|
| **Expand** | Add a nullable column. Add a table. Add an index `CONCURRENTLY`. Add a `CHECK` as `NOT VALID`. Widen a type. Add a new value to a `CHECK` list. | Anything the *old* binary would fail on. |
| **Backfill** | Populate the new column in bounded batches, off the release path, at a rate that leaves the payment path alone. Then `VALIDATE CONSTRAINT`. | Hold one long transaction. Run inside the migration. |
| **Contract** | Drop the old column. Drop the old index. Tighten `NULL` to `NOT NULL`. Narrow a `CHECK`. | Run before every replica of the old binary is gone. |

Worked example — renaming `payments.statement_ref` to `payments.statement_descriptor`:

1. `00NN_expand`: `ADD COLUMN statement_descriptor TEXT`. Deploy code that writes **both** and
   reads the new one falling back to the old.
2. `00NN_backfill`: `UPDATE … SET statement_descriptor = statement_ref WHERE statement_descriptor
   IS NULL` in batches of 10 000.
3. Deploy code that writes and reads only the new column.
4. `00NN_contract`: `DROP COLUMN statement_ref`.

Four steps for a rename is not ceremony. Doing it in one step means that for the duration of the
rollout, half the fleet writes a column the other half reads as `NULL`, and the payments written
in that window have no statement descriptor — permanently, because nobody notices until a
cardholder rings their bank.

---

## 2. How a migration runs

### 2.1 In a cluster: ArgoCD sync wave

Migrations run as a Kubernetes `Job` in **sync wave −1**, before any Deployment in wave 0. ArgoCD
will not progress to wave 0 until the Job reports success, so a failed migration blocks the
deploy instead of shipping code against a schema that does not have what it needs.

```yaml
metadata:
  annotations:
    argocd.argoproj.io/sync-wave: "-1"
    argocd.argoproj.io/hook: Sync
    argocd.argoproj.io/hook-delete-policy: BeforeHookCreation
```

Properties this arrangement depends on:

- **The Job and the Deployment are the same image.** The migrations are embedded (`embed.go`), so
  the schema a pod applies is exactly the schema the binary in that pod was built against. A
  ConfigMap of SQL is a second artifact with its own version, and the day it drifts from the image
  is the day the migration applies a statement the code does not expect.
- **The runner takes a session-level advisory lock** (`pg_advisory_lock`) before reading
  `schema_migrations`. Several pods start at once during a rollout; without the lock they race,
  two apply the same migration, and the second gets a duplicate-object error that looks like a
  broken migration rather than a broken lock.
- **The runner connects as `pp_migrate`, not `pp_app`.** `pp_app` owns nothing and holds no DDL
  privilege, which is what stops an application bug from altering the schema.
- **Each migration runs in its own transaction.** A migration that cannot run transactionally —
  `CREATE INDEX CONCURRENTLY` is the only one in practice — is marked and run outside, and it is
  the only kind of migration that can leave a half-applied artifact (an `INVALID` index) behind.
  Drop and recreate it; do not `VALIDATE` it.
- **Checksums are recorded and verified.** Editing an already-applied migration changes its
  checksum, and the runner refuses to proceed rather than silently running a schema that no longer
  matches what production has. Fix it forward.

### 2.2 Locally and in CI

```
platformctl migrate up            # apply everything pending
platformctl migrate up --dry-run  # print the plan and the statement counts, touch nothing
platformctl migrate status        # applied versions, pending versions, checksum mismatches
```

`--dry-run` acquires the same advisory lock and reads the same ledger, so it reports the plan the
real run would take rather than a plausible-looking guess.

---

## 3. Rollback is forward-only

There is a `.down.sql` for every migration. **It is not the production rollback mechanism.**

The `down` scripts exist for two purposes: tearing down an ephemeral test database, and forcing
the author to think about what their change would take to undo — which is where "this is a
one-way door" is usually discovered. They are exercised by CI on a throwaway database
(`TestEveryUpHasADown`), which is the only environment they are safe in.

Production rollback is a **new, higher-numbered, compensating migration**. The reasons are
specific, not stylistic:

1. **A `down` cannot restore data.** `DROP COLUMN` is instant and irreversible; the down script's
   `ADD COLUMN` gives you the column back with every value `NULL`. For `payments` that is the
   difference between a schema rollback and a data-loss incident.
2. **A `down` runs against a schema that has moved on.** If `0042` shipped and `0043` shipped on
   top of it, `0042.down` is written against a database that no longer exists.
3. **A `down` breaks the ledger's monotonicity.** `schema_migrations` is an append-only record of
   what was applied when. Deleting a row from it destroys the audit trail of the deploy, and the
   audit trail of a deploy is what an incident review reads first.
4. **The application is already rolled back.** The old binary ran against the *pre*-expand schema
   and against the *post*-expand schema — that is the whole point of expand/contract. Rolling the
   schema back is almost never necessary; rolling the code back is enough.

So: `0043_drop_widget_column.up.sql` went wrong → write `0044_restore_widget_column.up.sql`, with
whatever backfill the restoration needs. It is slower to type and enormously faster to reason
about at 3 a.m.

---

## 4. Zero-downtime checklist for the dangerous operations

Every row here has caused a production outage somewhere. `lock_timeout` is set to `1s` for the
migration session (see `docs/lld.md` §4.1) so that a migration which cannot get its lock **fails
fast instead of queueing** — a queued `ACCESS EXCLUSIVE` request blocks every subsequent reader
behind it, which turns a slow migration into a total outage in seconds.

| Operation | Hazard | Safe recipe |
|---|---|---|
| `CREATE INDEX` | `SHARE` lock blocks all writes for the build, which on a billion-row partition is hours. | `CREATE INDEX CONCURRENTLY`, outside a transaction. On failure the index is left `INVALID`: drop it and retry, never `REINDEX` it into service. |
| `DROP INDEX` | `ACCESS EXCLUSIVE`. | `DROP INDEX CONCURRENTLY`. |
| `ADD COLUMN … NOT NULL DEFAULT x` | Safe on PG 11+ (the default is stored in the catalog, no rewrite) — but only for a *constant* default. A volatile default still rewrites the whole table. | Constant defaults are fine. For a computed one: add nullable, backfill in batches, then set the default and the `NOT NULL`. |
| `ADD COLUMN` with a `NOT NULL` and no default | Full rewrite plus an immediate constraint failure on every existing row. | Nullable → backfill → `SET NOT NULL` via a validated `CHECK` (below). |
| `SET NOT NULL` | Full table scan under `ACCESS EXCLUSIVE`. | `ADD CONSTRAINT c CHECK (col IS NOT NULL) NOT VALID`, then `VALIDATE CONSTRAINT c` (which takes only `SHARE UPDATE EXCLUSIVE`), then `SET NOT NULL` — PG 12+ recognises the validated constraint and skips the scan. |
| `ADD CHECK` | Full scan under `ACCESS EXCLUSIVE`. | `NOT VALID`, then `VALIDATE CONSTRAINT` in a separate migration. |
| `ADD FOREIGN KEY` | Locks both tables and scans the child. | `NOT VALID`, then `VALIDATE`. |
| `ALTER COLUMN TYPE` | Full rewrite, `ACCESS EXCLUSIVE`, and every dependent index and view rebuilt. | Almost always: new column, dual-write, backfill, swap, drop. The exceptions that are free are `varchar(n)` → `text` and widening `varchar(n)`. |
| `RENAME COLUMN` / `RENAME TABLE` | Instant on the database, catastrophic for the fleet: the old binary's `SELECT` names a column that no longer exists. | Never rename in place. Expand/contract, as in §1. |
| `DROP COLUMN` | Instant, irreversible, and breaks any replica still selecting it. | Contract phase only, one full release after the last reader shipped. |
| `DROP TABLE` in an `up` migration | Irreversible data loss with no compensating migration possible. | Requires an explicit `-- IRREVERSIBLE:` marker on the line above, and `TestNoUnmarkedDropTable` fails the build without it. |
| Backfill `UPDATE` | One statement over a billion rows holds a transaction for hours: vacuum stops, bloat grows, and a replica falls behind until it is useless. | Batches of ~10 000 keyed on the primary key, committed per batch, with a sleep between them, and a kill switch. Run as a Job, not as a migration. |
| Adding a partition | A missing partition is a **hard INSERT failure on the payment path**. | `pp.create_future_partitions()` runs hourly with thirteen months of lead time, and alerts below two. Never rely on a migration to create the partition a new month needs. |
| Anything on `payments`, `payment_attempts`, `ledger_entries`, `audit_records` | These are partitioned and enormous, and the money path runs through the first two. | Operate on partitions, not the parent, wherever the operation permits it. Confirm the plan against a restored production-sized snapshot first — an operation that is instant on a staging database with ten thousand rows tells you nothing. |

### Before you merge

- [ ] Does the *old* binary still work against this schema? If not, it is a contract migration and
      it does not ship in the same release as the code that needs it.
- [ ] Does the *new* binary still work against the *previous* schema? If not, the rollout has an
      ordering dependency that ArgoCD's sync wave does not express, and it needs a feature flag.
- [ ] Is every new tenant-scoped table `ENABLE`d **and** `FORCE`d for RLS with a policy carrying
      **both** `USING` and `WITH CHECK`? `TestEveryTenantScopedTableHasRLS` fails otherwise, and
      skipping it is not an option — it is the test that catches the *next* migration that forgets.
- [ ] Does every new index lead with `tenant_id`, and is every new unique constraint scoped by it?
      A global unique on `merchant_id` collides across tenants, because `merchant_id` is unique
      only *within* a tenant.
- [ ] Does every new index have a named query in `docs/spec/04-domain-model.md` §7? An index with
      no query is write amplification with a maintenance cost.
- [ ] No new `GRANT` to `pp_app` beyond the minimum. No `BYPASSRLS`. No `SECURITY DEFINER`
      function — it runs as its owner and bypasses the caller's RLS entirely.
- [ ] Has the estimated lock duration been measured against a production-sized table, not a
      development one?
