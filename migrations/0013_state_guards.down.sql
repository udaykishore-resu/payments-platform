-- 0013 down. Removes the database-side transition and immutability guards. Note what this
-- leaves behind: a schema where an illegal payment transition or a mutated amount is accepted
-- silently. It is intended only for tearing down a test database.
DROP TRIGGER IF EXISTS certification_reports_guard ON pp.certification_reports;
DROP FUNCTION IF EXISTS pp.certification_reports_guard();
DROP TRIGGER IF EXISTS configuration_versions_guard ON pp.configuration_versions;
DROP FUNCTION IF EXISTS pp.configuration_versions_guard();
DROP TRIGGER IF EXISTS payment_event_log_append_only ON pp.payment_event_log;
DROP TRIGGER IF EXISTS routing_plans_append_only ON pp.routing_plans;
DROP TRIGGER IF EXISTS audit_records_append_only ON pp.audit_records;
DROP TRIGGER IF EXISTS ledger_entries_append_only ON pp.ledger_entries;
DROP FUNCTION IF EXISTS pp.reject_mutation();
DROP TRIGGER IF EXISTS tenants_guard ON pp.tenants;
DROP FUNCTION IF EXISTS pp.tenants_guard();
DROP TRIGGER IF EXISTS merchants_guard ON pp.merchants;
DROP FUNCTION IF EXISTS pp.merchants_guard();
DROP TRIGGER IF EXISTS payments_guard ON pp.payments;
DROP FUNCTION IF EXISTS pp.payments_guard();
-- IRREVERSIBLE: the transition tables are reference data recreated by the up migration.
DROP TABLE IF EXISTS pp.merchant_status_transitions;
DROP TABLE IF EXISTS pp.payment_state_transitions;
