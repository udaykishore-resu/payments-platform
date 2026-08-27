-- 0002 — BC-1: tenants, API clients, roles and role bindings.
--
-- ID columns are TEXT with a regex CHECK rather than UUID. The prefix is load-bearing: it makes
-- a log line self-describing and turns "you passed a merchant ID where a payment ID was
-- expected" into a 400 at the edge instead of a 404 three layers in (baseline section 6).
-- Crockford Base32 excludes I, L, O and U, which is what the character class encodes.

CREATE TABLE pp.tenants (
    tenant_id           TEXT        PRIMARY KEY
                        CHECK (tenant_id ~ '^ten_[0-9A-HJKMNP-TV-Z]{26}$'),
    name                TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    tier                TEXT        NOT NULL CHECK (tier IN ('POOLED', 'SILOED')),
    status              TEXT        NOT NULL DEFAULT 'ACTIVE'
                        CHECK (status IN ('ACTIVE', 'SUSPENDED', 'TERMINATED')),
    residency_region    TEXT        NOT NULL
                        CHECK (residency_region IN ('GLOBAL', 'EU', 'UK', 'US', 'APAC')),
    kms_key_ref         TEXT,
    environments        TEXT[]      NOT NULL DEFAULT ARRAY['sandbox']::TEXT[],
    enabled_gateways    TEXT[]      NOT NULL DEFAULT '{}',
    enabled_currencies  TEXT[]      NOT NULL DEFAULT '{}',
    enabled_methods     TEXT[]      NOT NULL DEFAULT '{}',
    feature_flags       JSONB       NOT NULL DEFAULT '{}',
    max_merchants       INTEGER     NOT NULL DEFAULT 1000 CHECK (max_merchants >= 0),
    requests_per_second INTEGER     NOT NULL DEFAULT 500  CHECK (requests_per_second >= 0),
    concurrent_payments INTEGER     NOT NULL DEFAULT 64   CHECK (concurrent_payments >= 0),
    cache_memory_mb     INTEGER     NOT NULL DEFAULT 256  CHECK (cache_memory_mb >= 0),
    max_payment_amount  BIGINT      NOT NULL DEFAULT 0    CHECK (max_payment_amount >= 0),
    max_payment_currency CHAR(3),
    status_reason       TEXT        NOT NULL DEFAULT '',
    version             BIGINT      NOT NULL DEFAULT 0    CHECK (version >= 0),
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    suspended_at        TIMESTAMPTZ,
    terminated_at       TIMESTAMPTZ,

    -- A siloed tenant without a key reference is a tenant whose contractual isolation exists
    -- only in the sales deck. The CHECK is what makes the promise structural.
    CONSTRAINT tenants_siloed_requires_key CHECK (tier <> 'SILOED' OR kms_key_ref IS NOT NULL),
    CONSTRAINT tenants_terminated_has_time CHECK (status <> 'TERMINATED' OR terminated_at IS NOT NULL)
);

COMMENT ON COLUMN pp.tenants.tier IS
    'Immutable after creation. A POOLED to SILOED move is the migration project in '
    'docs/multi-tenancy.md section 5.1, not an UPDATE; the immutability trigger in 0013 enforces it.';
COMMENT ON COLUMN pp.tenants.max_payment_amount IS
    'Tenant-level backstop in minor units. Zero means no ceiling. This is not the limit a '
    'merchant hits in practice - that is the per-merchant risk policy - it exists so a mistaken '
    'or compromised merchant configuration cannot authorize more than the commercial '
    'relationship justifies.';

-- The tenants table is the one tenant-scoped table whose own key IS the tenant. The policy
-- compares the primary key rather than a tenant_id column, which is the same predicate with a
-- different column name.
ALTER TABLE pp.tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.tenants FORCE  ROW LEVEL SECURITY;

-- current_setting(..., true) - the `true` is missing_ok. Without it an unset GUC raises; with it
-- the expression is NULL, `tenant_id = NULL` is NULL, and NULL is not TRUE, so an unset GUC
-- yields ZERO rows rather than ALL rows. That is the single most important line in this file.
-- USING filters what a statement can see; WITH CHECK constrains what it can write. A policy with
-- only USING lets tenant A insert a row stamped with tenant B's ID - unreadable to A, but
-- present, visible to B, and corrupting B's data.
CREATE POLICY tenant_isolation ON pp.tenants
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE pp.api_clients (
    client_id               TEXT        PRIMARY KEY
                            CHECK (client_id ~ '^cli_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id               TEXT        NOT NULL REFERENCES pp.tenants (tenant_id) ON DELETE RESTRICT,
    name                    TEXT        NOT NULL CHECK (name <> ''),
    scopes                  TEXT[]      NOT NULL CHECK (cardinality(scopes) > 0),
    allowed_cidrs           TEXT[]      NOT NULL DEFAULT '{}',
    credential_ref          TEXT        NOT NULL DEFAULT '',
    previous_credential_ref TEXT        NOT NULL DEFAULT '',
    rotation_overlap_until  TIMESTAMPTZ,
    status                  TEXT        NOT NULL DEFAULT 'ACTIVE'
                            CHECK (status IN ('ACTIVE', 'DISABLED', 'REVOKED')),
    status_reason           TEXT        NOT NULL DEFAULT '',
    last_used_at            TIMESTAMPTZ,
    expires_at              TIMESTAMPTZ,
    last_rotated_at         TIMESTAMPTZ,
    revoked_at              TIMESTAMPTZ,
    version                 BIGINT      NOT NULL DEFAULT 0,
    created_at              TIMESTAMPTZ NOT NULL,
    updated_at              TIMESTAMPTZ NOT NULL,

    CONSTRAINT api_clients_revoked_has_time CHECK (status <> 'REVOKED' OR revoked_at IS NOT NULL)
);

COMMENT ON COLUMN pp.api_clients.credential_ref IS
    'A secrets-manager path, never credential material. The column is a path by design; a '
    'plaintext secret here would be readable by every holder of SELECT on this table, which '
    'includes the analytics replica.';

CREATE UNIQUE INDEX uq_api_client_name ON pp.api_clients (tenant_id, name);
CREATE INDEX idx_api_clients_tenant ON pp.api_clients (tenant_id, status, client_id);

ALTER TABLE pp.api_clients ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.api_clients FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.api_clients
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- roles is a platform-global catalog: the scope vocabulary is the same for every tenant, and a
-- per-tenant copy would drift. It carries no tenant_id and therefore no RLS; it is read-only to
-- the application role.
CREATE TABLE pp.roles (
    role_id    TEXT        PRIMARY KEY,
    name       TEXT        NOT NULL UNIQUE,
    scopes     TEXT[]      NOT NULL CHECK (cardinality(scopes) > 0),
    is_system  BOOLEAN     NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE pp.roles IS
    'Platform-global role catalog. No tenant_id, therefore no RLS: every tenant sees the same '
    'role definitions. Writes are platformctl-only; pp_app holds SELECT.';

REVOKE INSERT, UPDATE, DELETE ON pp.roles FROM pp_app;

CREATE TABLE pp.role_bindings (
    binding_id   TEXT        PRIMARY KEY,
    tenant_id    TEXT        NOT NULL REFERENCES pp.tenants (tenant_id) ON DELETE RESTRICT,
    role_id      TEXT        NOT NULL REFERENCES pp.roles (role_id) ON DELETE RESTRICT,
    subject_type TEXT        NOT NULL CHECK (subject_type IN ('API_CLIENT', 'USER')),
    subject_id   TEXT        NOT NULL CHECK (subject_id <> ''),
    granted_by   TEXT        NOT NULL DEFAULT '',
    granted_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT uq_role_binding UNIQUE (tenant_id, role_id, subject_type, subject_id)
);

CREATE INDEX idx_role_bindings_subject ON pp.role_bindings (tenant_id, subject_type, subject_id);

ALTER TABLE pp.role_bindings ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.role_bindings FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.role_bindings
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
