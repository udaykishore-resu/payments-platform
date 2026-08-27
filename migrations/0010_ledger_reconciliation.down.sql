-- 0010 down. IRREVERSIBLE: drops the ledger and reconciliation tables and every partition of
-- ledger_entries. In an environment holding real financial records this must never be run;
-- ledger entries are retained 7 years under the carve-out in docs/multi-tenancy.md section 6.1.
DROP TABLE IF EXISTS pp.reconciliation_exceptions;
DROP TABLE IF EXISTS pp.reconciliation_runs;
DROP TABLE IF EXISTS pp.ledger_entries;
DROP TABLE IF EXISTS pp.ledger_accounts;
