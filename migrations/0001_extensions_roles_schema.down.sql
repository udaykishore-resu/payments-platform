-- 0001 down.
--
-- IRREVERSIBLE: dropping the schema destroys every table in it. This script exists for local
-- development and for tearing down an ephemeral test database. It is never run in an
-- environment holding real data; production rollback is forward-only with a compensating
-- migration (see migrations/README.md).

DROP TABLE IF EXISTS pp.partition_registry;
DROP TABLE IF EXISTS pp.schema_migrations;

ALTER DEFAULT PRIVILEGES IN SCHEMA pp REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM pp_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA pp REVOKE SELECT ON TABLES FROM pp_readonly;
ALTER DEFAULT PRIVILEGES IN SCHEMA pp REVOKE USAGE, SELECT ON SEQUENCES FROM pp_app;

REVOKE USAGE ON SCHEMA pp FROM pp_app, pp_relay, pp_readonly;

DROP SCHEMA IF EXISTS pp CASCADE;

-- Roles are intentionally not dropped: they may own objects in other databases on the same
-- cluster, and DROP ROLE would fail or, worse, succeed and orphan those grants.
