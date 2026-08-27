-- 0006 — BC-5: versioned merchant configuration, policies and feature flags.
--
-- The whole point of this context is that history is append-only: a rollback publishes the
-- previous document as a NEW version. Deleting or editing a version destroys the answer to
-- "what were we running when that payment failed", which is the first question in every
-- configuration incident.

CREATE TABLE pp.configurations (
    configuration_id TEXT        PRIMARY KEY,
    tenant_id        TEXT        NOT NULL,
    merchant_id      TEXT        NOT NULL,
    environment      TEXT        NOT NULL CHECK (environment IN ('sandbox', 'production')),
    current_version  INTEGER     NOT NULL DEFAULT 0 CHECK (current_version >= 0),
    status           TEXT        NOT NULL DEFAULT 'DRAFT'
                     CHECK (status IN ('DRAFT', 'ACTIVE', 'SUPERSEDED')),
    version          BIGINT      NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,

    CONSTRAINT uq_config_scope UNIQUE (tenant_id, merchant_id, environment)
);

ALTER TABLE pp.configurations ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.configurations FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.configurations
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE pp.configuration_versions (
    configuration_version_id TEXT        PRIMARY KEY
                             CHECK (configuration_version_id ~ '^cfv_[0-9A-HJKMNP-TV-Z]{26}$'),
    configuration_id         TEXT        NOT NULL REFERENCES pp.configurations (configuration_id) ON DELETE RESTRICT,
    tenant_id                TEXT        NOT NULL,
    merchant_id              TEXT        NOT NULL,
    environment              TEXT        NOT NULL CHECK (environment IN ('sandbox', 'production')),
    version                  INTEGER     NOT NULL CHECK (version >= 1),
    status                   TEXT        NOT NULL DEFAULT 'DRAFT'
                             CHECK (status IN ('DRAFT', 'ACTIVE', 'SUPERSEDED')),
    document                 JSONB       NOT NULL,
    document_checksum        TEXT        NOT NULL CHECK (document_checksum ~ '^[0-9a-f]{64}$'),
    previous_version         INTEGER     NOT NULL DEFAULT 0 CHECK (previous_version >= 0),
    rolled_back_from         INTEGER,
    comment                  TEXT        NOT NULL DEFAULT '',
    created_by               TEXT        NOT NULL DEFAULT '',
    created_at               TIMESTAMPTZ NOT NULL,
    published_at             TIMESTAMPTZ,

    CONSTRAINT uq_config_version UNIQUE (configuration_id, version),
    CONSTRAINT config_document_is_object CHECK (jsonb_typeof(document) = 'object'),
    CONSTRAINT config_published_has_comment
        CHECK (status = 'DRAFT' OR (published_at IS NOT NULL AND comment <> ''))
);

COMMENT ON CONSTRAINT config_published_has_comment ON pp.configuration_versions IS
    'A configuration history with no reasons is a list of diffs nobody can interpret six months '
    'later. The comment is required at publish, not at draft, so authoring stays cheap.';
COMMENT ON COLUMN pp.configuration_versions.document_checksum IS
    'sha256 of the JCS-canonicalized document. A mismatch is corruption, and the publish is '
    'blocked rather than recorded with a checksum that does not describe what was stored.';

-- Exactly one ACTIVE version per (merchant, environment). Enforced here rather than in the
-- publish transaction because two concurrent publishes both reading "current is 4" would both
-- write version 5, and only an index notices.
CREATE UNIQUE INDEX uq_config_active_version
    ON pp.configuration_versions (tenant_id, merchant_id, environment)
    WHERE status = 'ACTIVE';
CREATE INDEX idx_config_versions_history
    ON pp.configuration_versions (configuration_id, version DESC);
CREATE INDEX idx_config_versions_since
    ON pp.configuration_versions (published_at)
    WHERE status = 'ACTIVE' AND published_at IS NOT NULL;

COMMENT ON INDEX pp.idx_config_versions_since IS
    'ListActiveSince: how a data-plane replica warms and refreshes its snapshot without scanning '
    'the whole history.';

-- No UPDATE and no DELETE. The role-level revoke is the control; the trigger in 0013 is the
-- second one, because a future GRANT could silently undo this line.
REVOKE UPDATE, DELETE ON pp.configuration_versions FROM pp_app;

ALTER TABLE pp.configuration_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.configuration_versions FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.configuration_versions
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE pp.policies (
    policy_id   TEXT        PRIMARY KEY,
    tenant_id   TEXT        NOT NULL,
    merchant_id TEXT,
    policy_type TEXT        NOT NULL CHECK (policy_type IN ('ROUTING', 'RISK', 'COMPLIANCE')),
    definition  JSONB       NOT NULL,
    status      TEXT        NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'DISABLED')),
    version     BIGINT      NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT policies_definition_is_object CHECK (jsonb_typeof(definition) = 'object')
);

COMMENT ON COLUMN pp.policies.merchant_id IS
    'NULL means a tenant-wide default. The uniqueness index coalesces it to the empty string '
    'because NULL is not equal to NULL in a unique index, which would let two tenant-wide '
    'routing policies coexist.';

CREATE UNIQUE INDEX uq_policy_active
    ON pp.policies (tenant_id, coalesce(merchant_id, ''), policy_type)
    WHERE status = 'ACTIVE';

ALTER TABLE pp.policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.policies FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.policies
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE pp.feature_flags (
    flag_id            TEXT        PRIMARY KEY,
    tenant_id          TEXT        NOT NULL,
    merchant_id        TEXT,
    flag_key           TEXT        NOT NULL CHECK (flag_key <> ''),
    enabled            BOOLEAN     NOT NULL DEFAULT false,
    rollout_percentage SMALLINT    NOT NULL DEFAULT 0
                       CHECK (rollout_percentage BETWEEN 0 AND 100),
    variant            JSONB,
    owner              TEXT        NOT NULL CHECK (owner <> ''),
    expires_at         TIMESTAMPTZ NOT NULL,
    version            BIGINT      NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE pp.feature_flags IS
    'Every flag has an owner and an expiry, both NOT NULL. A flag with neither is permanent '
    'branching that nobody remembers owning, and a flag past its expiry resolves to the compiled '
    'default and raises a stale-flag alert rather than silently persisting.';

CREATE UNIQUE INDEX uq_flag_scope
    ON pp.feature_flags (tenant_id, coalesce(merchant_id, ''), flag_key);
CREATE INDEX idx_flag_expiry ON pp.feature_flags (expires_at);

ALTER TABLE pp.feature_flags ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.feature_flags FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.feature_flags
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
