-- 0007 — BC-6: the money tables.
--
-- partition_month is the range-partition key of BOTH payments and payment_attempts, and it is a
-- pure function of the payment's ULID: partition_month = date_trunc('month', ids.TimeOf(payment_id)).
-- Amendment A-02 is the reason. A partial unique index on a partitioned table is enforced only
-- *within* a partition, so invariant I3 ("at most one successful attempt per payment") would
-- silently weaken the moment a payment's attempts could land in different months - which they can,
-- because an attempt may be created days after the payment by a delayed capture or by
-- reconciliation. Deriving both tables' partition from the *payment's* immutable ID puts every
-- attempt in its payment's partition, so the per-partition index constrains the whole set.
--
-- The same rule buys static partition pruning: GET /v1/payments/{id} decodes the ULID timestamp
-- client-side and adds partition_month = $decoded as an equality, so the planner touches one
-- partition instead of eighty-four.

CREATE TABLE pp.payments (
    payment_id            TEXT        NOT NULL
                          CHECK (payment_id ~ '^pay_[0-9A-HJKMNP-TV-Z]{26}$'),
    partition_month       TIMESTAMPTZ NOT NULL,
    tenant_id             TEXT        NOT NULL,
    merchant_id           TEXT        NOT NULL,
    state                 TEXT        NOT NULL DEFAULT 'CREATED' CHECK (state IN (
                              'CREATED', 'REQUIRES_ACTION', 'PROCESSING', 'PENDING',
                              'AUTHORIZED', 'CAPTURED', 'SETTLED', 'PARTIALLY_REFUNDED',
                              'REFUNDED', 'VOIDED', 'FAILED', 'CANCELED', 'EXPIRED', 'DISPUTED')),
    amount                BIGINT      NOT NULL CHECK (amount > 0),
    currency              CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    payment_method        TEXT        NOT NULL,
    capture_method        TEXT        NOT NULL DEFAULT 'AUTOMATIC'
                          CHECK (capture_method IN ('AUTOMATIC', 'MANUAL')),

    method_token          TEXT        NOT NULL,
    method_brand          TEXT        NOT NULL DEFAULT '',
    method_last4          TEXT        NOT NULL DEFAULT '',
    method_exp_month      SMALLINT    NOT NULL DEFAULT 0 CHECK (method_exp_month BETWEEN 0 AND 12),
    method_exp_year       SMALLINT    NOT NULL DEFAULT 0 CHECK (method_exp_year >= 0),
    method_country        CHAR(2)     NOT NULL DEFAULT '',
    method_network_token  BOOLEAN     NOT NULL DEFAULT false,

    authorized_amount     BIGINT      CHECK (authorized_amount IS NULL OR authorized_amount >= 0),
    captured_amount       BIGINT      NOT NULL DEFAULT 0 CHECK (captured_amount >= 0),
    refunded_amount       BIGINT      NOT NULL DEFAULT 0 CHECK (refunded_amount >= 0),

    selected_gateway      TEXT        NOT NULL DEFAULT '',
    routing_plan_id       TEXT        NOT NULL DEFAULT '',
    current_attempt_id    TEXT        NOT NULL DEFAULT '',

    risk_decision         TEXT        NOT NULL DEFAULT 'ALLOW'
                          CHECK (risk_decision IN ('ALLOW', 'REQUIRE_3DS', 'DECLINE')),
    three_ds_status       TEXT        NOT NULL DEFAULT '',
    description           TEXT        NOT NULL DEFAULT '',
    statement_descriptor  TEXT        NOT NULL DEFAULT '',
    metadata              JSONB       NOT NULL DEFAULT '{}',

    customer_ref          TEXT        NOT NULL DEFAULT '',
    customer_email_hash   TEXT        NOT NULL DEFAULT '',
    customer_ip           TEXT        NOT NULL DEFAULT '',
    customer_country      CHAR(2)     NOT NULL DEFAULT '',

    idempotency_key       TEXT        NOT NULL DEFAULT '',
    correlation_id        TEXT        NOT NULL DEFAULT '',
    reconciliation_required BOOLEAN   NOT NULL DEFAULT false,

    version               BIGINT      NOT NULL DEFAULT 0 CHECK (version >= 0),
    created_at            TIMESTAMPTZ NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL,
    authorized_at         TIMESTAMPTZ,
    captured_at           TIMESTAMPTZ,
    expires_at            TIMESTAMPTZ,

    PRIMARY KEY (payment_id, partition_month),

    -- I1. The domain recomputes the sum from the loaded refunds and returns
    -- REFUND_EXCEEDS_CAPTURED with a usable message; this CHECK is what is still true when the
    -- domain has a bug. Both exist; only one is trusted.
    CONSTRAINT payments_i1_refund_within_capture CHECK (refunded_amount <= captured_amount),

    -- I2. NULL authorized_amount means there was never an authorization step (an automatic
    -- capture on a single-step method), and in that case there is nothing to bound the capture
    -- against. Writing zero instead of NULL there would make every auto-captured payment
    -- violate I2 the instant it captured.
    CONSTRAINT payments_i2_capture_within_auth
        CHECK (authorized_amount IS NULL OR captured_amount <= authorized_amount),
    CONSTRAINT payments_auth_amount_needs_auth_time
        CHECK (authorized_amount IS NULL OR authorized_at IS NOT NULL),

    -- The partition key must be exactly the month of the row's own created_at, which the writer
    -- derives from the ULID. If they ever disagree, a by-ID lookup prunes to the wrong partition
    -- and silently returns nothing.
    CONSTRAINT payments_partition_matches_created_at
        CHECK (partition_month = date_trunc('month', created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'),

    -- A schema-level PAN tripwire sitting behind the L1 detector. It cannot catch a tokenized
    -- PAN and is not meant to; it catches the day somebody wires a raw card number into the
    -- token field, which is the accident that puts the whole platform in PCI scope.
    CONSTRAINT payments_token_is_not_a_pan CHECK (method_token !~ '^[0-9]{13,19}$'),
    CONSTRAINT payments_metadata_is_object CHECK (jsonb_typeof(metadata) = 'object'),
    CONSTRAINT payments_metadata_bounded CHECK (pg_column_size(metadata) < 8192)
) PARTITION BY RANGE (partition_month);

COMMENT ON TABLE pp.payments IS
    'Range-partitioned monthly on partition_month, which is derived from the payment ULID and '
    'never from now(). See amendment A-02 and 04-domain-model.md section 8.2.';
COMMENT ON COLUMN pp.payments.method_token IS
    'A gateway or network token. There is no column on this table that could hold a primary '
    'account number, which is the structural expression of the PCI boundary in baseline '
    'section 17 - not a policy, an absence.';
COMMENT ON COLUMN pp.payments.refunded_amount IS
    'Reserved when a refund is REQUESTED and released when it FAILS, so it is deliberately not '
    'the same number as sum(refunds WHERE state = SUCCEEDED). The nightly reconciliation compares '
    'the two rather than assuming them equal.';

CREATE INDEX idx_payments_merchant_created
    ON pp.payments (tenant_id, merchant_id, created_at DESC, payment_id DESC);
COMMENT ON INDEX pp.idx_payments_merchant_created IS
    'Cursor pagination over (created_at, payment_id). DESC matches the scan direction so the '
    'planner needs no sort node.';

CREATE INDEX idx_payments_state_open ON pp.payments (tenant_id, state, created_at)
    WHERE state IN ('CREATED', 'PROCESSING', 'PENDING', 'REQUIRES_ACTION', 'AUTHORIZED');
COMMENT ON INDEX pp.idx_payments_state_open IS
    'The merchant-termination guard ("zero payments in a non-terminal state") and the stuck-'
    'payments dashboard. Partial because ~99.5 percent of rows are terminal within a day: an '
    'unpartial index on state would be enormous and never selective.';

CREATE INDEX idx_payments_reconciliation ON pp.payments (tenant_id, created_at)
    WHERE reconciliation_required;
CREATE INDEX idx_payments_expiring ON pp.payments (expires_at)
    WHERE state = 'AUTHORIZED' AND expires_at IS NOT NULL;
CREATE INDEX idx_payments_metadata
    ON pp.payments USING GIN (metadata jsonb_path_ops) WHERE metadata <> '{}';

-- Deliberately NOT created: any index on method_token. We must never be able to answer "which
-- payments used this token" efficiently - that is an enumeration primitive over a sensitive
-- value, and its absence is a control.

ALTER TABLE pp.payments ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.payments FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.payments
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- No DELETE on the money path, ever. Retention is a partition DETACH, not a DELETE.
REVOKE DELETE ON pp.payments FROM pp_app;

CREATE TABLE pp.payment_attempts (
    attempt_id              TEXT        NOT NULL
                            CHECK (attempt_id ~ '^att_[0-9A-HJKMNP-TV-Z]{26}$'),
    partition_month         TIMESTAMPTZ NOT NULL,
    payment_id              TEXT        NOT NULL,
    tenant_id               TEXT        NOT NULL,
    gateway_id              TEXT        NOT NULL,
    gateway_connection_id   TEXT        NOT NULL DEFAULT '',
    operation               TEXT        NOT NULL CHECK (operation IN (
                                'authorize', 'capture', 'refund', 'void', 'lookup',
                                'provision', 'webhook_register')),
    attempt_number          SMALLINT    NOT NULL CHECK (attempt_number BETWEEN 1 AND 4),
    amount                  BIGINT      NOT NULL CHECK (amount > 0),
    currency                CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),

    -- The domain models the whole attempt lifecycle in one field, using PENDING and DISPATCHED
    -- as outcome values rather than NULL. That is a deliberate divergence from a NULL-outcome
    -- design: a nullable outcome makes "not yet dispatched" and "dispatched, result lost"
    -- indistinguishable, and those two need different handling. `state` below is derived so the
    -- coarse three-phase view the operator dashboards use is still available.
    outcome                 TEXT        NOT NULL DEFAULT 'PENDING' CHECK (outcome IN (
                                'PENDING', 'DISPATCHED', 'SUCCESS', 'DECLINED',
                                'ERROR', 'TIMEOUT_UNKNOWN')),
    state                   TEXT        GENERATED ALWAYS AS (
                                CASE outcome
                                    WHEN 'PENDING'    THEN 'PENDING'
                                    WHEN 'DISPATCHED' THEN 'DISPATCHED'
                                    ELSE 'COMPLETED'
                                END) STORED,

    gateway_idempotency_key TEXT        NOT NULL CHECK (gateway_idempotency_key <> ''),
    gateway_reference       TEXT        NOT NULL DEFAULT '',
    decline_reason_code     TEXT        NOT NULL DEFAULT '',
    decline_is_retryable    BOOLEAN,
    network_advice_no_retry BOOLEAN     NOT NULL DEFAULT false,
    normalized_error_code   TEXT        NOT NULL DEFAULT '',
    error_message           TEXT        NOT NULL DEFAULT '',
    raw_status              TEXT        NOT NULL DEFAULT '',
    gateway_payload         JSONB       NOT NULL DEFAULT '{}',

    request_sent_at         TIMESTAMPTZ,
    response_received_at    TIMESTAMPTZ,
    latency_ms              INTEGER     NOT NULL DEFAULT 0 CHECK (latency_ms >= 0),
    created_at              TIMESTAMPTZ NOT NULL,
    updated_at              TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (attempt_id, partition_month),

    -- The FK is what makes a wrong partition_month unwritable. Without it, an attempt could be
    -- inserted into a different month than its payment, and I3's per-partition index would stop
    -- constraining the full set - silently, with no error at write time.
    FOREIGN KEY (payment_id, partition_month)
        REFERENCES pp.payments (payment_id, partition_month) ON DELETE RESTRICT,

    CONSTRAINT uq_attempt_number UNIQUE (payment_id, partition_month, attempt_number),
    CONSTRAINT attempt_decline_has_retryability
        CHECK (outcome <> 'DECLINED' OR decline_is_retryable IS NOT NULL),
    CONSTRAINT attempt_dispatched_has_time
        CHECK (outcome IN ('PENDING', 'ERROR') OR request_sent_at IS NOT NULL),
    CONSTRAINT attempt_payload_is_object CHECK (jsonb_typeof(gateway_payload) = 'object')
) PARTITION BY RANGE (partition_month);

COMMENT ON COLUMN pp.payment_attempts.gateway_idempotency_key IS
    'base32(HMAC-SHA256(attempt_id, salt))[:32], written BEFORE dispatch. Deterministic in the '
    'attempt ID so the reconciler can reproduce it after a crash and ask the gateway what '
    'happened. A transport retry to the same gateway reuses it and is deduplicated there; a '
    'failover creates a new attempt and therefore correctly a new key.';
COMMENT ON COLUMN pp.payment_attempts.outcome IS
    'TIMEOUT_UNKNOWN is not terminal and never means failure. It sets '
    'payments.reconciliation_required and leaves the payment in PROCESSING, because the only '
    'safe representation of "we do not know whether money moved" is "still processing".';

CREATE INDEX idx_attempts_payment
    ON pp.payment_attempts (payment_id, partition_month, attempt_number);
CREATE UNIQUE INDEX uq_attempt_gw_idem
    ON pp.payment_attempts (gateway_id, gateway_idempotency_key, partition_month);
CREATE INDEX idx_attempts_unknown ON pp.payment_attempts (gateway_id, request_sent_at)
    WHERE outcome = 'TIMEOUT_UNKNOWN';

-- NOTE: uq_attempt_success, the partial unique index that IS invariant I3, cannot live on the
-- partitioned parent - PostgreSQL requires a unique index on a partitioned table to include the
-- partition key, and does not permit a partial unique index there at all. It is therefore
-- created on each partition individually by pp.create_partition() in 0014, and
-- `platformctl partitions verify` fails if any partition of payment_attempts lacks it.

ALTER TABLE pp.payment_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.payment_attempts FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.payment_attempts
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

REVOKE DELETE ON pp.payment_attempts FROM pp_app;

CREATE TABLE pp.refunds (
    refund_id               TEXT        PRIMARY KEY
                            CHECK (refund_id ~ '^ref_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id               TEXT        NOT NULL,
    payment_id              TEXT        NOT NULL,
    partition_month         TIMESTAMPTZ NOT NULL,
    amount                  BIGINT      NOT NULL CHECK (amount > 0),
    currency                CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    reason                  TEXT        NOT NULL DEFAULT 'OTHER' CHECK (reason IN (
                                'REQUESTED_BY_CUSTOMER', 'DUPLICATE', 'FRAUDULENT',
                                'PRODUCT_UNAVAILABLE', 'SERVICE_NOT_PROVIDED',
                                'PRICING_ERROR', 'DISPUTE_CONCEDED', 'OTHER')),
    status                  TEXT        NOT NULL DEFAULT 'PENDING' CHECK (status IN (
                                'PENDING', 'SUBMITTED', 'SUCCEEDED', 'FAILED', 'CANCELED')),
    gateway_reference       TEXT        NOT NULL DEFAULT '',
    idempotency_key         TEXT        NOT NULL DEFAULT '',
    failure_code            TEXT        NOT NULL DEFAULT '',
    failure_message         TEXT        NOT NULL DEFAULT '',
    requested_by            TEXT        NOT NULL DEFAULT '',
    created_at              TIMESTAMPTZ NOT NULL,
    updated_at              TIMESTAMPTZ NOT NULL,
    submitted_at            TIMESTAMPTZ,
    settled_at              TIMESTAMPTZ,

    FOREIGN KEY (payment_id, partition_month)
        REFERENCES pp.payments (payment_id, partition_month) ON DELETE RESTRICT,
    CONSTRAINT refunds_succeeded_has_time
        CHECK (status <> 'SUCCEEDED' OR settled_at IS NOT NULL)
);

COMMENT ON TABLE pp.refunds IS
    'Not partitioned. Refund volume is roughly 2 percent of payment volume and refunds are always '
    'queried by payment, so partitioning would buy nothing and would cost the foreign key that '
    'keeps refunds attached to a real payment.';

CREATE UNIQUE INDEX uq_refund_idem ON pp.refunds (tenant_id, idempotency_key)
    WHERE idempotency_key <> '';
CREATE INDEX idx_refunds_payment ON pp.refunds (payment_id, partition_month, created_at);

ALTER TABLE pp.refunds ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.refunds FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.refunds
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

REVOKE DELETE ON pp.refunds FROM pp_app;

CREATE TABLE pp.routing_plans (
    routing_plan_id        TEXT        PRIMARY KEY
                           CHECK (routing_plan_id ~ '^rpl_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id              TEXT        NOT NULL,
    merchant_id            TEXT        NOT NULL,
    payment_id             TEXT        NOT NULL DEFAULT '',
    candidates             JSONB       NOT NULL,
    excluded               JSONB       NOT NULL DEFAULT '[]',
    strategy               TEXT        NOT NULL DEFAULT '',
    policy_version         INTEGER     NOT NULL DEFAULT 0,
    config_version         INTEGER     NOT NULL DEFAULT 0,
    config_snapshot_age_ms INTEGER     NOT NULL DEFAULT 0,
    health_snapshot        JSONB       NOT NULL DEFAULT '{}',
    decided_at             TIMESTAMPTZ NOT NULL,
    decided_by             TEXT        NOT NULL DEFAULT '',

    CONSTRAINT routing_candidates_is_array CHECK (jsonb_typeof(candidates) = 'array'),
    CONSTRAINT routing_excluded_is_array   CHECK (jsonb_typeof(excluded) = 'array')
);

COMMENT ON TABLE pp.routing_plans IS
    'Insert-only. A plan is the record of a decision at an instant; amending it destroys the '
    'audit trail. The excluded list matters as much as the candidate list: "why was this gateway '
    'NOT chosen" is the question asked in every routing incident.';

CREATE INDEX idx_routing_plans_payment ON pp.routing_plans (payment_id) WHERE payment_id <> '';
REVOKE UPDATE, DELETE ON pp.routing_plans FROM pp_app;

ALTER TABLE pp.routing_plans ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.routing_plans FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.routing_plans
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- payment_event_log is the database-side proof of I5: exactly one row per state change, and a
-- version that moves by exactly one. A gap in the sequence for a payment is detectable by
-- reading the log, which is what TestI5_VersionGapsAreImpossible does.
CREATE TABLE pp.payment_event_log (
    seq               BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id         TEXT        NOT NULL,
    payment_id        TEXT        NOT NULL,
    partition_month   TIMESTAMPTZ NOT NULL,
    aggregate_version BIGINT      NOT NULL CHECK (aggregate_version >= 1),
    from_state        TEXT        NOT NULL DEFAULT '',
    to_state          TEXT        NOT NULL DEFAULT '',
    trigger           TEXT        NOT NULL DEFAULT '',
    actor             TEXT        NOT NULL DEFAULT '',
    event_id          TEXT        NOT NULL DEFAULT '',
    occurred_at       TIMESTAMPTZ NOT NULL,

    CONSTRAINT uq_payment_event_version UNIQUE (payment_id, aggregate_version)
);

COMMENT ON CONSTRAINT uq_payment_event_version ON pp.payment_event_log IS
    'I5. Two writers that both believed they were producing version 7 cannot both record it; the '
    'loser gets 23505 rather than overwriting the winner''s history.';

CREATE INDEX idx_payment_event_log_payment
    ON pp.payment_event_log (payment_id, aggregate_version);

REVOKE UPDATE, DELETE ON pp.payment_event_log FROM pp_app;

ALTER TABLE pp.payment_event_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.payment_event_log FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.payment_event_log
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
