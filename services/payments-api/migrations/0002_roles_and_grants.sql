-- 0002_roles_and_grants.sql
-- Least-privilege database role for the application (docs/05-security-architecture.md).
-- The migration runner itself uses a separate, more privileged role (DDL owner); the
-- application at runtime connects only as payments_api_app, which cannot alter schema, and
-- critically cannot UPDATE or DELETE ledger_entries or audit_log — the append-only guarantee for
-- those two tables is enforced by GRANT, not just by "the application code doesn't do that".

BEGIN;

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'payments_api_app') THEN
        CREATE ROLE payments_api_app LOGIN PASSWORD NULL; -- password/auth managed via IAM DB auth or Secrets Manager, not a static SQL password
    END IF;
END
$$;

GRANT SELECT, INSERT, UPDATE ON accounts, payments, idempotency_keys TO payments_api_app;
GRANT SELECT, INSERT, UPDATE ON outbox_events TO payments_api_app;

-- Append-only tables: INSERT + SELECT only, explicitly no UPDATE/DELETE grant.
GRANT SELECT, INSERT ON ledger_entries TO payments_api_app;
GRANT SELECT, INSERT ON audit_log TO payments_api_app;

COMMIT;
