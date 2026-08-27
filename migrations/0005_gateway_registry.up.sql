-- 0005 — BC-4: the gateway catalog, per-merchant connections, credential metadata and health.

-- gateways is the ONLY table in this schema without tenant_id, and therefore the only one
-- without RLS. It is a platform-global catalog: every tenant sees the same gateway definitions,
-- and a per-tenant copy would drift. It is read-only to the application role; writes go through
-- platformctl.
CREATE TABLE pp.gateways (
    gateway_id       TEXT        PRIMARY KEY
                     CHECK (gateway_id ~ '^[a-z][a-z0-9-]{0,31}$'),
    display_name     TEXT        NOT NULL CHECK (display_name <> ''),
    vendor           TEXT        NOT NULL DEFAULT '',
    api_version      TEXT        NOT NULL DEFAULT '',
    base_urls        JSONB       NOT NULL DEFAULT '{}',
    capabilities     JSONB       NOT NULL DEFAULT '{}',
    cost_model       JSONB       NOT NULL DEFAULT '{"rates":[]}',
    signature_scheme TEXT        NOT NULL DEFAULT 'HMAC_SHA256',
    status           TEXT        NOT NULL DEFAULT 'ACTIVE'
                     CHECK (status IN ('ACTIVE', 'DEGRADED', 'DEPRECATED', 'DISABLED')),
    regions          TEXT[]      NOT NULL DEFAULT '{}',
    version          BIGINT      NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT gateways_capabilities_is_object CHECK (jsonb_typeof(capabilities) = 'object'),
    CONSTRAINT gateways_base_urls_is_object    CHECK (jsonb_typeof(base_urls) = 'object')
);

COMMENT ON TABLE pp.gateways IS
    'Platform-global gateway registry. The gateway identifier is a human-authored slug rather '
    'than a ULID because it appears in routing configuration, in webhook URLs and in metric '
    'label values, where "stripe" is enormously more useful than an opaque identifier. The '
    'charset CHECK exists because the value is interpolated into metric labels, Redis key '
    'prefixes and secrets paths.';

REVOKE INSERT, UPDATE, DELETE ON pp.gateways FROM pp_app;

CREATE TABLE pp.gateway_connections (
    connection_id           TEXT        PRIMARY KEY
                            CHECK (connection_id ~ '^gwc_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id               TEXT        NOT NULL,
    merchant_id             TEXT        NOT NULL,
    gateway_id              TEXT        NOT NULL REFERENCES pp.gateways (gateway_id) ON DELETE RESTRICT,
    environment             TEXT        NOT NULL CHECK (environment IN ('sandbox', 'production')),
    status                  TEXT        NOT NULL DEFAULT 'UNPROVISIONED' CHECK (status IN (
                                'UNPROVISIONED', 'PROVISIONING', 'PROVISIONED', 'CERTIFYING',
                                'CERTIFIED', 'DEGRADED', 'REVOKED')),
    certification_status    TEXT        NOT NULL DEFAULT 'NOT_STARTED',
    certification_report_id TEXT        NOT NULL DEFAULT '',
    external_account_ref    TEXT        NOT NULL DEFAULT '',
    credential_ref          TEXT        NOT NULL DEFAULT '',
    webhook_registration_id TEXT        NOT NULL DEFAULT '',
    webhook_endpoint        TEXT        NOT NULL DEFAULT '',
    webhook_secret_ref      TEXT        NOT NULL DEFAULT '',
    credential_rotated_at   TIMESTAMPTZ,
    credential_expires_at   TIMESTAMPTZ,
    status_reason           TEXT        NOT NULL DEFAULT '',
    revocation_reason       TEXT        NOT NULL DEFAULT '',
    last_error              TEXT        NOT NULL DEFAULT '',
    version                 BIGINT      NOT NULL DEFAULT 0,
    created_at              TIMESTAMPTZ NOT NULL,
    updated_at              TIMESTAMPTZ NOT NULL,
    provisioned_at          TIMESTAMPTZ,
    certified_at            TIMESTAMPTZ,
    revoked_at              TIMESTAMPTZ,
    last_health_check_at    TIMESTAMPTZ,

    CONSTRAINT uq_gw_connection UNIQUE (tenant_id, merchant_id, gateway_id, environment),

    -- CERTIFIED is the status that lets money flow. Every precondition for it is a column, so
    -- "certified" cannot be asserted by an UPDATE that forgot one of them.
    CONSTRAINT connection_certified_is_complete CHECK (
        status <> 'CERTIFIED' OR (
            certification_report_id <> '' AND credential_ref <> '' AND webhook_registration_id <> ''
        )),
    CONSTRAINT connection_credential_max_age CHECK (
        credential_expires_at IS NULL
        OR credential_rotated_at IS NULL
        OR credential_expires_at <= credential_rotated_at + INTERVAL '90 days'),
    CONSTRAINT connection_credential_ref_is_a_path CHECK (credential_ref NOT LIKE '%sk_live%')
);

COMMENT ON CONSTRAINT connection_credential_ref_is_a_path ON pp.gateway_connections IS
    'Belt and braces. The real control is that the column holds a secrets-manager path and the '
    'material never leaves the secrets manager; this CHECK is a tripwire for the day somebody '
    '"temporarily" pastes a live key in to unblock a test.';

CREATE INDEX idx_gw_conn_certified ON pp.gateway_connections (tenant_id, merchant_id)
    WHERE status = 'CERTIFIED';
CREATE INDEX idx_gw_conn_merchant ON pp.gateway_connections (tenant_id, merchant_id, gateway_id);
CREATE INDEX idx_gw_cred_expiry ON pp.gateway_connections (credential_expires_at)
    WHERE status <> 'REVOKED' AND credential_expires_at IS NOT NULL;

COMMENT ON INDEX pp.idx_gw_cred_expiry IS
    'Drives the 90-day credential rotation workflow. Partial on status so revoked connections do '
    'not keep the index warm forever.';

ALTER TABLE pp.gateway_connections ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.gateway_connections FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.gateway_connections
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE pp.gateway_credentials_meta (
    credential_ref  TEXT        PRIMARY KEY,
    tenant_id       TEXT        NOT NULL,
    connection_id   TEXT        NOT NULL REFERENCES pp.gateway_connections (connection_id) ON DELETE CASCADE,
    kms_key_ref     TEXT        NOT NULL DEFAULT '',
    rotated_at      TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ,
    rotation_state  TEXT        NOT NULL DEFAULT 'CURRENT'
                    CHECK (rotation_state IN ('CURRENT', 'ROTATING', 'RETIRED')),

    CONSTRAINT credentials_meta_holds_no_material CHECK (credential_ref NOT LIKE '%sk_live%')
);

COMMENT ON TABLE pp.gateway_credentials_meta IS
    'Contains no credential material, only the metadata needed to schedule rotation. The primary '
    'key is a secrets-manager path.';

ALTER TABLE pp.gateway_credentials_meta ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.gateway_credentials_meta FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.gateway_credentials_meta
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- gateway_health is platform-wide by design (baseline section 10): a per-merchant sample is too
-- sparse to be statistically meaningful, and a gateway that is down is down for everyone. It
-- carries no tenant_id and therefore no RLS.
CREATE TABLE pp.gateway_health (
    gateway_id                  TEXT        NOT NULL REFERENCES pp.gateways (gateway_id) ON DELETE CASCADE,
    operation                   TEXT        NOT NULL CHECK (operation IN (
                                    'authorize', 'capture', 'refund', 'void', 'lookup',
                                    'provision', 'webhook_register')),
    state                       TEXT        NOT NULL DEFAULT 'HEALTHY'
                                CHECK (state IN ('HEALTHY', 'DEGRADED', 'UNHEALTHY', 'PROBING')),
    error_rate                  NUMERIC(5,4) NOT NULL DEFAULT 0
                                CHECK (error_rate BETWEEN 0 AND 1),
    p99_latency_ms              INTEGER     NOT NULL DEFAULT 0 CHECK (p99_latency_ms >= 0),
    sample_count                INTEGER     NOT NULL DEFAULT 0 CHECK (sample_count >= 0),
    window_started_at           TIMESTAMPTZ,
    cooldown_seconds            INTEGER     NOT NULL DEFAULT 30
                                CHECK (cooldown_seconds BETWEEN 30 AND 300),
    cooldown_until              TIMESTAMPTZ,
    consecutive_probe_successes SMALLINT    NOT NULL DEFAULT 0
                                CHECK (consecutive_probe_successes >= 0),
    last_observed_at            TIMESTAMPTZ,
    state_changed_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    version                     BIGINT      NOT NULL DEFAULT 0,

    PRIMARY KEY (gateway_id, operation)
);

COMMENT ON TABLE pp.gateway_health IS
    'The gossip point for circuit state, not the hot path. The sliding window lives in each '
    'orchestrator process; this row is written on state change only. Writing every sample would '
    'be a 5000 TPS write amplifier for no benefit. It is persisted at all so that a restarting '
    'pod does not begin life believing every gateway is perfectly healthy - which is how a '
    'fleet-wide restart during an outage sends a thundering herd at a dead gateway.';
COMMENT ON CONSTRAINT gateway_health_cooldown_seconds_check ON pp.gateway_health IS
    'baseline section 10: the cool-down starts at 30 s and doubles on each failed probe, capped '
    'at 5 minutes. A value outside that band means the doubling logic has a bug.';
