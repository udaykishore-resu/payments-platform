-- 0009 — BC-7: inbound gateway webhooks and their deduplication claim.
--
-- Store first, process later, always. A webhook endpoint that processes synchronously is an
-- endpoint that times out under load and makes the gateway retry, multiplying the load exactly
-- when it is highest.

CREATE TABLE pp.inbound_webhooks (
    webhook_id          TEXT        PRIMARY KEY
                        CHECK (webhook_id ~ '^whk_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id           TEXT,
    merchant_id         TEXT        NOT NULL DEFAULT '',
    gateway_id          TEXT        NOT NULL,
    gateway_event_id    TEXT        NOT NULL CHECK (gateway_event_id <> ''),
    event_type          TEXT        NOT NULL DEFAULT '',
    signature           TEXT        NOT NULL DEFAULT '',
    signature_valid     BOOLEAN     NOT NULL DEFAULT false,
    signature_scheme    TEXT        NOT NULL DEFAULT '',
    headers             JSONB       NOT NULL DEFAULT '{}',
    raw_body            BYTEA       NOT NULL,
    body_sha256         TEXT        NOT NULL CHECK (body_sha256 ~ '^[0-9a-f]{64}$'),
    status              TEXT        NOT NULL DEFAULT 'RECEIVED' CHECK (status IN (
                            'RECEIVED', 'VERIFIED', 'RESOLVED', 'PROCESSED',
                            'REJECTED', 'DUPLICATE', 'PARKED')),
    resolved_payment_id TEXT        NOT NULL DEFAULT '',
    resolved_event_type TEXT        NOT NULL DEFAULT '',
    attempts            SMALLINT    NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 8),
    last_error          TEXT        NOT NULL DEFAULT '',
    retry_after         TIMESTAMPTZ,
    received_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at        TIMESTAMPTZ,
    version             BIGINT      NOT NULL DEFAULT 0,

    -- The (gateway, gateway event id) pair IS the deduplication. It is a unique index rather
    -- than an in-memory set so that it survives a pod restart and works across replicas - the
    -- two conditions under which an in-memory set silently stops deduplicating.
    CONSTRAINT uq_webhook_gateway_event UNIQUE (gateway_id, gateway_event_id),

    -- An unverified webhook can never reach a processing state. Signature verification precedes
    -- every interpretation; the body is persisted anyway, for forensics, but is never acted on.
    CONSTRAINT webhook_processing_requires_signature
        CHECK (status NOT IN ('RESOLVED', 'PROCESSED') OR signature_valid),
    CONSTRAINT webhook_body_bounded CHECK (octet_length(raw_body) <= 1048576),
    CONSTRAINT webhook_headers_is_object CHECK (jsonb_typeof(headers) = 'object')
);

COMMENT ON COLUMN pp.inbound_webhooks.tenant_id IS
    'Nullable, because tenancy is unknown until the payload resolves to a payment. The RLS '
    'policy therefore admits NULL rows: an unresolved webhook belongs to nobody yet, and the '
    'ingress writes it as a dedicated narrower role.';

CREATE INDEX idx_webhook_unprocessed ON pp.inbound_webhooks (received_at)
    WHERE status IN ('RECEIVED', 'VERIFIED', 'RESOLVED');
CREATE INDEX idx_webhook_parked ON pp.inbound_webhooks (gateway_id, received_at)
    WHERE status = 'PARKED';
CREATE INDEX idx_webhook_payment ON pp.inbound_webhooks (resolved_payment_id)
    WHERE resolved_payment_id <> '';

ALTER TABLE pp.inbound_webhooks ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.inbound_webhooks FORCE  ROW LEVEL SECURITY;

-- The one policy in this schema that admits a NULL tenant_id, and the reason is stated above.
-- It is still fail-closed for a *resolved* row: once tenant_id is set, only that tenant sees it.
CREATE POLICY tenant_isolation ON pp.inbound_webhooks
    FOR ALL
    TO pp_app
    USING      (tenant_id IS NULL OR tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id IS NULL OR tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE pp.webhook_dedup (
    gateway_id       TEXT        NOT NULL,
    gateway_event_id TEXT        NOT NULL,
    webhook_id       TEXT        NOT NULL,
    first_seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at       TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (gateway_id, gateway_event_id)
);

COMMENT ON TABLE pp.webhook_dedup IS
    'The dedup claim is INSERT ... ON CONFLICT DO NOTHING; zero rows means duplicate, drop and '
    'count. Retention is 30 days and must exceed every gateway''s own retry window, or a '
    'gateway''s last retry arrives after we have forgotten the first delivery.';

CREATE INDEX idx_webhook_dedup_expiry ON pp.webhook_dedup (expires_at);

CREATE TABLE pp.event_dedup (
    consumer_group TEXT        NOT NULL,
    event_id       TEXT        NOT NULL,
    processed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (consumer_group, event_id)
);

COMMENT ON TABLE pp.event_dedup IS
    'baseline section 13.5. The dedup insert must happen in the same transaction as the '
    'handler''s work: a dedup row committed separately from the effect it guards is a dedup row '
    'that lies. Retention 30 days, at least the topic retention.';

CREATE INDEX idx_dedup_expiry ON pp.event_dedup (expires_at);
