# RB-034: Tenant tier migration (pooled ↔ siloed)

- **Severity:** ticket (a planned change, not an incident). It becomes a page only if step 5's
  freeze exceeds its budget or step 3's verification fails after cutover.
- **Alert:** none. This is a scheduled operation, **drilled quarterly against a synthetic tenant in
  staging** (`docs/multi-tenancy.md` §5).
- **Triggered when:** a tenant's contract, volume or residency requirement changes tier.
- **Plane / service:** data and control · one tenant's whole footprint
- **Related:** `docs/multi-tenancy.md` §5 (the eight-step procedure, authoritative) and §6 (tenant
  lifecycle), `docs/adr/ADR-008-pooled-multi-tenancy-with-rls.md`,
  [security-tenant-isolation.md](security-tenant-isolation.md), [reconciliation.md](reconciliation.md)

## What this means

Moving a tenant between pooled (shared schema, RLS-isolated) and siloed (its own schema/cluster,
own CMK, own Redis DB, own topics and ACLs) is eight steps, and the design decision that shapes all
of them is: **no dual-write.**

A dual-write across two clusters during cutover would reintroduce exactly the failure mode the
outbox exists to eliminate (baseline §13.4). Instead the tenant is briefly frozen — **payment
writes only**, expected ≤ 30 s — and the cutover happens with a single writer at all times.

The eight steps, with their rollbacks:

| # | Step | Rollback |
|---|---|---|
| 1 | **Provision** — Terraform applies the target schema/cluster, per-tenant CMK, Redis DB, topics, ACLs, node group; migrations to the same version as the source | Destroy; no production impact |
| 2 | **Backfill** — logical replication (or `COPY` for small tenants) filtered by `tenant_id`, run against a **read replica** so the writer is untouched | Drop the target data |
| 3 | **Verify** — row counts and per-table checksums source↔target; ledger balances recomputed and compared; audit hash chain re-verified end to end on the target | Repeat backfill |
| 4 | **Catch-up** — replication follows until lag < 1 s | — |
| 5 | **Freeze** — tenant set to `MIGRATING`: payment writes return `503` + `Retry-After: 5`; **reads continue; refunds and voids continue** against the source. ≤ 30 s | Clear the flag; nothing has moved |
| 6 | **Cut over** — drain in-flight, wait for lag = 0, flip `tenants.tier`, the connection factory, the cache namespace and the topic set. Configuration cache invalidated with priority | Flip back — the source is intact and current, because step 7 has not run |
| 7 | **Unfreeze + soak** — traffic on the target; source retained read-only for 30 days. Reverse replication is **not** enabled | Rollback within the soak window is a second, reversed migration |
| 8 | **Reclaim** — after 30 days and a clean reconciliation run, source rows deleted and pooled quota released | Irreversible |

## Impact

- **Step 5 only, and only payment writes.** ≤ 30 s of `503` + `Retry-After: 5`. Client SDKs retry
  and the payments land on the target.
- **Reads continue throughout.** **Refunds and voids continue** — baseline §8: you must always be
  able to give money back, and the freeze is not allowed to break that.
- Everything else is invisible to the merchant.
- The risk is not the freeze; it is **step 3 verification being done badly**, which produces a
  tenant whose data is subtly incomplete on the target and whose source rows are deleted 30 days
  later.

## Immediate triage (first 5 minutes)

This is a planned change. "Triage" here is the pre-flight, and it is not optional:

1. Confirm the drill has been run this quarter against a synthetic tenant in staging. If it has not,
   stop — a procedure that has not been exercised in three months is a document, not a runbook.
2. Confirm the schema versions match:
   ```bash
   ./bin/platformctl migrate status               # against the source
   PP_DSN="$TARGET_DSN" ./bin/platformctl migrate status
   ```
3. Baseline the source, so verification has something to compare against:
   ```sql
   SET LOCAL app.tenant_id = 'ten_…';
   SELECT 'payments' t, count(*) FROM pp.payments
   UNION ALL SELECT 'payment_attempts', count(*) FROM pp.payment_attempts
   UNION ALL SELECT 'merchants',        count(*) FROM pp.merchants
   UNION ALL SELECT 'ledger_entries',   count(*) FROM pp.ledger_entries
   UNION ALL SELECT 'audit_records',    count(*) FROM pp.audit_records
   UNION ALL SELECT 'idempotency_records', count(*) FROM pp.idempotency_records
   UNION ALL SELECT 'outbox_unpublished', count(*) FROM pp.outbox_events WHERE published_at IS NULL;
   ```
4. Verify the audit chain **on the source, before starting**:
   ```bash
   ./bin/platformctl verify-audit-chain ten_…
   ```
   An unverifiable chain before a migration becomes an unverifiable chain after one, and then nobody
   can tell which caused it.
5. Confirm there are no open critical reconciliation exceptions for the tenant:
   ```sql
   SET LOCAL app.tenant_id = 'ten_…';
   SELECT severity, state, count(*) FROM pp.reconciliation_exceptions
   WHERE  state IN ('OPEN','INVESTIGATING') GROUP BY severity, state;
   ```
6. Confirm the outbox is drained: `./bin/platformctl outbox status`.

## Diagnosis

Decision points during the migration:

- **Step 3 checksums disagree** → do not proceed. Repeat the backfill. → *M1*.
- **Ledger balances differ between source and target** → the backfill missed rows or duplicated
  them. Do not proceed. → *M1*.
- **Audit chain does not verify on the target** → an unverifiable chain after a migration is
  indistinguishable from tampering. Do not proceed. → *M1*, and if it does not resolve,
  [audit-integrity.md](audit-integrity.md).
- **Step 4 lag will not fall below 1 s** → the source is writing faster than replication catches up.
  Reschedule to a lower-traffic window. → *M2*.
- **Step 5 freeze exceeds 30 s** → clear the flag and abort. Nothing has moved. → *M3*.
- **Step 6 shows in-flight transactions that will not drain** → do not flip. → *M3*.
- **After step 7, the tenant reports missing data** → roll back within the soak window. → *M4*.
- **Idempotency records did not migrate** → **stop.** A client retrying across the cutover would get
  a second execution: the one bug that must not happen during a migration. → *M1*.

## Mitigation

**M1 — repeat the backfill.** Step 2's rollback is "drop the target data", and it costs nothing
because the source is untouched — the backfill runs against a read replica by design.

**M2 — reschedule.** There is no deadline that justifies cutting over with lag above 1 s.

**M3 — abort the freeze.** Clear `MIGRATING`; writes resume on the source immediately. Steps 5 and 6
are explicitly reversible for exactly this reason, and using that reversibility is the procedure
working, not a failure.

**M4 — roll back within the soak window.** Source rows are retained read-only for 30 days, and
reverse replication is deliberately **not** enabled — so rollback is a second, reversed migration,
not a flip. Plan it as such; it takes the same eight steps.

**M5 — after step 8, there is no rollback.** Source rows are deleted. The gate on step 8 is 30 days
**and** a clean reconciliation run, and both halves matter.

## Rollback / escalation

- **Never dual-write.** It will be proposed as a way to avoid the freeze. It reintroduces the exact
  failure mode the outbox exists to eliminate, and a 30-second freeze is simpler and provably
  correct.
- **Never let the freeze block refunds or voids.** Baseline §8. If the implementation does, that is
  a bug in the freeze, not an acceptable trade.
- **Never skip step 3.** It is the only place where "the data arrived correctly" is established, and
  its failure mode is silent for 30 days.
- **Migrate idempotency records with the tenant.** Without them a client retrying across the cutover
  executes twice.
- **Migrate unpublished outbox rows only.** Published rows stay behind: re-publishing already
  published events is safe (dedup makes consumers effectively-once) but generates avoidable noise,
  while unpublished rows must move or events are lost.
- **Freeze exceeding 2 minutes** → page, abort, and treat as an incident.
- **Any doubt at step 6** → do not flip. The source is still current; that stops being true the
  moment you do.

## Verification

Per step 3, and again after step 6:
```sql
-- Row counts and checksums, source vs target, per table.
SET LOCAL app.tenant_id = 'ten_…';
SELECT 'payments' t, count(*), md5(string_agg(payment_id, ',' ORDER BY payment_id)) FROM pp.payments
UNION ALL
SELECT 'payment_attempts', count(*), md5(string_agg(attempt_id, ',' ORDER BY attempt_id)) FROM pp.payment_attempts
UNION ALL
SELECT 'idempotency_records', count(*), md5(string_agg(idempotency_record_id, ',' ORDER BY idempotency_record_id)) FROM pp.idempotency_records;

-- Ledger balances recomputed and compared.
SELECT account_id, sum(amount) FROM pp.ledger_entries GROUP BY account_id ORDER BY account_id;

-- Unpublished outbox rows moved, published ones did not.
SELECT count(*) FROM pp.outbox_events WHERE published_at IS NULL;
```
```bash
./bin/platformctl verify-audit-chain ten_…     # on the TARGET, exit 0
./bin/platformctl outbox status
```
Then prove the tenant works end to end on the target: a payment created, authorized, captured and
refunded, plus a configuration read. And confirm the migration itself is an audit record — it must
be, and its absence is a finding:
```sql
SET LOCAL app.tenant_id = 'ten_…';
SELECT action, resource_type, outcome, occurred_at FROM pp.audit_records
WHERE  action LIKE '%tier%' OR action LIKE '%migrat%' ORDER BY sequence DESC LIMIT 5;
```

## Follow-up

- Record the actual freeze duration against the ≤ 30 s budget. That number is what the next
  migration is planned against.
- Record the verification outputs — counts, checksums, balances, chain verification — as evidence.
  A migration whose verification was not recorded cannot be defended at step 8.
- Schedule the 30-day reclaim with an owner, and gate it on a clean reconciliation run rather than
  on the calendar alone.
- Feed the quarterly drill: any step that needed knowledge not in `docs/multi-tenancy.md` §5 is a
  documentation defect with an owner.
- If the tenant is siloed now, confirm the per-tenant CMK is in use and that crypto-shredding
  behaviour is understood — audit exports are encrypted under the **retention** key, not the tenant
  key, precisely so shredding does not destroy the audit trail.
