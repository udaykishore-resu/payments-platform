-- scripts/sql/dr-invariants.sql
--
-- The money-safety invariants, asserted against a restored database. Run by
-- scripts/dr-drill.sh step 3 (disaster-recovery.md §5.3, RD-1) and by the nightly
-- integrity job.
--
-- WHY THESE ARE SQL AND NOT GO
--   These are properties of the *data*, not of the code. Asserting them through the
--   application would test the application's ability to read its own writes; asserting
--   them in SQL tests the thing the restore actually produced. A restore that satisfies
--   every one of these is a restore the platform can be started against; one that does
--   not is a corrupt backup, and the difference must be discoverable without deploying
--   anything.
--
-- OUTPUT CONTRACT
--   Each check emits exactly one row: check_id, description, violations, verdict.
--   `verdict` is 'PASS' or 'FAIL'. The drill script greps for FAIL, so a check that
--   cannot decide must return FAIL — an inconclusive integrity check is a failed one.
--
-- Every count is a COUNT of *violating rows*, never a boolean, because "how badly" is the
-- first question asked when one of these fails and the answer must already be in the
-- artifact rather than require a second connection to a cluster that is about to be
-- deleted.

\pset pager off
\timing on
\set ON_ERROR_STOP on

-- Read-only, and say so: this runs against a restored production snapshot. A drill that
-- can write to the artefact it is verifying is a drill that can invalidate its own result.
SET default_transaction_read_only = on;
SET statement_timeout = '30min';

\echo '=== DR invariants (disaster-recovery.md §5.3 pass criteria) ==='

-- ---------------------------------------------------------------------------------------
-- I1 — sum(refunds) may never exceed captured_amount.
--      Baseline §9 I1; critical path CP-03. The database enforces this with a CHECK
--      constraint; this query asserts the constraint was actually in force for every row
--      that exists, which is a different claim — a constraint added later does not
--      validate history unless someone ran VALIDATE.
-- ---------------------------------------------------------------------------------------
SELECT
    'I1'                                                     AS check_id,
    'sum(refunds) <= captured_amount, per payment'           AS description,
    COUNT(*)                                                 AS violations,
    CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END       AS verdict
FROM (
    SELECT p.id
    FROM pp.payments p
    LEFT JOIN pp.refunds r
           ON r.payment_id = p.id
          AND r.status IN ('SUCCEEDED', 'PENDING')
    GROUP BY p.id, p.captured_amount
    HAVING COALESCE(SUM(r.amount), 0) > COALESCE(p.captured_amount, 0)
) AS overdrafts;

-- ---------------------------------------------------------------------------------------
-- I2 — sum(captures) may never exceed authorized_amount.
--      Baseline §9 I2. Same reasoning as I1.
-- ---------------------------------------------------------------------------------------
SELECT
    'I2'                                                     AS check_id,
    'captured_amount <= authorized_amount, per payment'      AS description,
    COUNT(*)                                                 AS violations,
    CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END       AS verdict
FROM pp.payments
WHERE authorized_amount IS NOT NULL
  AND captured_amount IS NOT NULL
  AND captured_amount > authorized_amount;

-- ---------------------------------------------------------------------------------------
-- I3 — a payment may have at most one attempt in a successful terminal state.
--      Baseline §9 I3; critical path CP-01. This is THE duplicate-charge invariant: two
--      successful attempts on one payment means the payer was charged twice. The partial
--      unique index enforces it; this asserts the outcome.
-- ---------------------------------------------------------------------------------------
SELECT
    'I3'                                                          AS check_id,
    'at most one successful attempt per payment (no double charge)' AS description,
    COUNT(*)                                                      AS violations,
    CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END            AS verdict
FROM (
    SELECT payment_id
    FROM pp.payment_attempts
    WHERE outcome IN ('AUTHORIZED', 'CAPTURED', 'SUCCEEDED')
    GROUP BY payment_id
    HAVING COUNT(*) > 1
) AS doubled;

-- ---------------------------------------------------------------------------------------
-- I4 — the ledger balances, per account and currency.
--      Double-entry bookkeeping has exactly one property worth checking and this is it.
--      A restore where debits and credits disagree is a restore that lost rows, and the
--      difference names which account to investigate.
-- ---------------------------------------------------------------------------------------
SELECT
    'I4'                                                     AS check_id,
    'ledger debits = credits, per account and currency'      AS description,
    COUNT(*)                                                 AS violations,
    CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END       AS verdict
FROM (
    SELECT
        e.account_id,
        e.currency,
        SUM(CASE WHEN e.side = 'DEBIT'  THEN e.amount ELSE 0 END) AS debits,
        SUM(CASE WHEN e.side = 'CREDIT' THEN e.amount ELSE 0 END) AS credits
    FROM pp.ledger_entries e
    GROUP BY e.account_id, e.currency
    HAVING SUM(CASE WHEN e.side = 'DEBIT'  THEN e.amount ELSE 0 END)
        <> SUM(CASE WHEN e.side = 'CREDIT' THEN e.amount ELSE 0 END)
) AS unbalanced;

-- ---------------------------------------------------------------------------------------
-- I5 — the audit chain is unbroken.
--      Each audit record carries prev_hash = the hash of the record before it. A break
--      means a record between two survivors did not make it into the backup — which is
--      the RPO consequence disaster-recovery.md §8.2 requires to be stated explicitly in
--      the postmortem rather than discovered later.
--
--      The chain is verified in full by `platformctl audit verify-chain`, which recomputes
--      each hash. This query is the cheap structural half: every non-genesis record's
--      prev_hash must match the hash of the record that precedes it in sequence, within
--      the same tenant.
-- ---------------------------------------------------------------------------------------
SELECT
    'I5'                                                     AS check_id,
    'audit chain links resolve (structural)'                 AS description,
    COUNT(*)                                                 AS violations,
    CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END       AS verdict
FROM (
    SELECT
        a.id,
        a.prev_hash,
        LAG(a.record_hash) OVER (PARTITION BY a.tenant_id ORDER BY a.sequence_number)
            AS expected_prev
    FROM pp.audit_records a
) AS chained
WHERE expected_prev IS NOT NULL
  AND (prev_hash IS DISTINCT FROM expected_prev);

-- ---------------------------------------------------------------------------------------
-- I6 — no payment is stranded in a non-terminal state older than the reconciliation SLA.
--      §24: a timed-out gateway call leaves a payment PROCESSING and the reconciler
--      resolves it. A restore containing payments that have been PROCESSING for days
--      means the reconciler was not running, which is a finding about the *source*
--      system that only a restore drill surfaces.
-- ---------------------------------------------------------------------------------------
SELECT
    'I6'                                                              AS check_id,
    'no payment PROCESSING for more than 24h (reconciler was running)' AS description,
    COUNT(*)                                                          AS violations,
    CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END                AS verdict
FROM pp.payments
WHERE status = 'PROCESSING'
  AND updated_at < now() - INTERVAL '24 hours';

-- ---------------------------------------------------------------------------------------
-- I7 — every tenant-scoped table still has RLS enabled and at least one policy.
--      A restore that drops the policies is a restore into which the application would
--      start happily and serve cross-tenant reads. This is the one invariant that is
--      about the schema rather than the rows, and it belongs here because a schema
--      restored without its policies looks completely normal.
-- ---------------------------------------------------------------------------------------
SELECT
    'I7'                                                     AS check_id,
    'every tenant-scoped table has RLS enabled and a policy'  AS description,
    COUNT(*)                                                 AS violations,
    CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END       AS verdict
FROM (
    SELECT c.relname
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'tenant_id' AND a.attnum > 0
    WHERE n.nspname = 'pp'
      AND c.relkind IN ('r', 'p')
      AND (
            c.relrowsecurity IS NOT TRUE
         OR NOT EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid)
          )
) AS unprotected;

-- ---------------------------------------------------------------------------------------
-- I8 — the outbox has not lost its ordering guarantees.
--      Every undispatched row must still carry a partition key; a NULL there means the
--      event would be published to an arbitrary partition and per-aggregate ordering
--      (§13.3) is gone for that aggregate — silently, and permanently.
-- ---------------------------------------------------------------------------------------
SELECT
    'I8'                                                     AS check_id,
    'every outbox row carries a partition key'               AS description,
    COUNT(*)                                                 AS violations,
    CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END       AS verdict
FROM pp.outbox_events
WHERE partition_key IS NULL OR partition_key = '';

\echo '=== end of DR invariants ==='
