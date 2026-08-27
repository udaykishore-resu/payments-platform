-- 0016 down. Removes the well-formedness constraint this migration added.
--
-- It deliberately does NOT drop pp.payment_attempts.gateway_connection_id. Two reasons, and the
-- second is the one that matters:
--
--   1. The column is 0007_payments' — 0016 only guarantees it exists (ADD COLUMN IF NOT EXISTS)
--      for a database restored from a dump that predates it. Dropping it here would leave a
--      database whose schema no longer matches the migration that owns the column, so a later
--      re-run of 0007 is not what repairs it and nothing else would.
--
--   2. DROP COLUMN is instant and irreversible (migrations/README.md §3.1): the down script's
--      ADD COLUMN would hand the column back with every value NULL. On payment_attempts that is
--      not a schema rollback, it is the loss of the record of which credential signed each
--      dispatch — exactly the evidence this migration exists to create.
--
-- As with every down script here, this is for tearing down a test database. A production rollback
-- is a new, higher-numbered, compensating migration.
-- pp:destructive drops only the NOT VALID check added by 0016; the column and its data survive
ALTER TABLE pp.payment_attempts
    DROP CONSTRAINT IF EXISTS attempt_connection_ref_well_formed;
