-- 0001 — extensions, schema, roles and grants.
--
-- Everything else in this directory assumes the objects created here exist. It is deliberately
-- the only migration that touches roles: role grants are the last line of defence behind RLS
-- (docs/multi-tenancy.md §2.1), and keeping them in one reviewed file makes "who can write to
-- the ledger" a question with one place to look.

-- No extensions are required. That is deliberate: every extension is a privilege the migration
-- role would need (CREATE EXTENSION is superuser-only on managed PostgreSQL unless the extension
-- is allow-listed), and a schema that needs none is a schema that applies identically on Aurora,
-- on a managed instance and in a throwaway test container.

CREATE SCHEMA IF NOT EXISTS pp;
COMMENT ON SCHEMA pp IS
    'Payments platform schema. Every tenant-scoped table here carries tenant_id NOT NULL, '
    'ENABLE + FORCE ROW LEVEL SECURITY, and a policy with both USING and WITH CHECK.';

-- Roles.
--
-- pp_app is the ONLY role the services connect as, and it is created explicitly WITHOUT
-- BYPASSRLS. BYPASSRLS makes every policy on every table inert for that role, silently and
-- globally: the RLS layer would still exist in the schema, still pass a naive "is RLS enabled"
-- audit, and protect nothing. That is worse than no RLS at all because it produces false
-- confidence. NOINHERIT is here for the same reason — a role that inherits a future privileged
-- role acquires its privileges without anyone editing this file.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pp_app') THEN
        CREATE ROLE pp_app NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pp_migrate') THEN
        CREATE ROLE pp_migrate NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOLOGIN;
    END IF;
    -- pp_relay is the narrow cross-tenant role for the three platform-wide jobs (outbox relay,
    -- reconciliation sweeper, audit anchoring). It gets USING (true) policies on exactly the
    -- tables it needs and no access at all to payments, merchants or configurations. Widening a
    -- policy for a specific minimal role is auditable; a global bypass is not.
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pp_relay') THEN
        CREATE ROLE pp_relay NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pp_readonly') THEN
        CREATE ROLE pp_readonly NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOLOGIN;
    END IF;
END
$$;

-- The rationale for pp_app's privileges is in docs/multi-tenancy.md section 2.1 and in the
-- comment above rather than in a COMMENT ON ROLE: commenting on a role requires privileges the
-- migration role does not need for anything else, and needing them for a comment is not a
-- trade worth making.

GRANT USAGE ON SCHEMA pp TO pp_app, pp_relay, pp_readonly;

-- Default privileges, so a table created by a later migration is reachable without that
-- migration having to remember. Forgetting a GRANT fails loudly at first query; forgetting a
-- REVOKE fails silently forever, which is why the REVOKEs are written per table at the point of
-- creation rather than relying on a default.
ALTER DEFAULT PRIVILEGES IN SCHEMA pp GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO pp_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA pp GRANT SELECT ON TABLES TO pp_readonly;
ALTER DEFAULT PRIVILEGES IN SCHEMA pp GRANT USAGE, SELECT ON SEQUENCES TO pp_app;

-- schema_migrations is the runner's ledger. It lives in pp so a single search_path reaches it,
-- and it is written only by the migration runner holding the advisory lock (see migrate.go).
CREATE TABLE IF NOT EXISTS pp.schema_migrations (
    version     INTEGER      PRIMARY KEY,
    name        TEXT         NOT NULL,
    checksum    TEXT         NOT NULL,
    applied_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    applied_by  TEXT         NOT NULL DEFAULT current_user,
    duration_ms INTEGER      NOT NULL DEFAULT 0
);

COMMENT ON COLUMN pp.schema_migrations.checksum IS
    'SHA-256 of the up-migration text as applied. A mismatch on a later run means someone edited '
    'an already-applied migration, which is the single most common way a staging schema and a '
    'production schema silently diverge. The runner refuses to proceed rather than reapplying.';

-- partition_registry drives the archival job (04-domain-model.md section 8.4). It is
-- platform-global, not tenant-scoped: a partition spans every tenant.
CREATE TABLE IF NOT EXISTS pp.partition_registry (
    table_name     TEXT        NOT NULL,
    partition_name TEXT        NOT NULL,
    range_start    TIMESTAMPTZ NOT NULL,
    range_end      TIMESTAMPTZ NOT NULL,
    state          TEXT        NOT NULL DEFAULT 'ATTACHED'
                   CHECK (state IN ('ATTACHED', 'DETACHED', 'ARCHIVED', 'DROPPED')),
    archived_uri   TEXT,
    archived_at    TIMESTAMPTZ,
    row_count      BIGINT,
    manifest_sha256 TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (table_name, partition_name),
    CHECK (range_end > range_start),
    CHECK (state <> 'ARCHIVED' OR (archived_uri IS NOT NULL AND manifest_sha256 IS NOT NULL))
);

COMMENT ON TABLE pp.partition_registry IS
    'One row per monthly partition of every partitioned table. The archival job will not DROP a '
    'partition whose row is not ARCHIVED with a verified manifest checksum, which is what stops '
    '"detach, fail to upload, drop" from silently destroying a month of payments.';

GRANT SELECT ON pp.schema_migrations, pp.partition_registry TO pp_app, pp_readonly;
