-- 0010 — BC-8: the shadow double-entry ledger and reconciliation.
--
-- This is a shadow ledger for reconciliation, not a money-custody ledger (A1). It is still
-- strictly append-only, because the value of a ledger is entirely in the fact that nobody can
-- edit it: a correction is a reversing entry, never an UPDATE.

CREATE TABLE pp.ledger_accounts (
    account_id     TEXT        PRIMARY KEY,
    tenant_id      TEXT        NOT NULL,
    merchant_id    TEXT        NOT NULL,
    currency       CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    account_type   TEXT        NOT NULL CHECK (account_type IN (
                       'MERCHANT_RECEIVABLE', 'GATEWAY_CLEARING', 'FEES_PAYABLE',
                       'REFUNDS_PAYABLE', 'DISPUTES_HELD', 'SETTLEMENT_SUSPENSE')),
    normal_side    TEXT        NOT NULL CHECK (normal_side IN ('DEBIT', 'CREDIT')),
    balance        BIGINT      NOT NULL DEFAULT 0,
    entry_count    BIGINT      NOT NULL DEFAULT 0 CHECK (entry_count >= 0),
    status         TEXT        NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'CLOSED')),
    version        BIGINT      NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL,

    CONSTRAINT uq_ledger_account
        UNIQUE (tenant_id, merchant_id, currency, account_type),
    CONSTRAINT ledger_account_closed_at_zero
        CHECK (status <> 'CLOSED' OR balance = 0)
);

COMMENT ON TABLE pp.ledger_accounts IS
    'One account per (tenant, merchant, currency, type). Split by currency because a balance '
    'that is the sum of cents and yen is an integer with no unit, and pkg/money deliberately has '
    'no exchange rate to convert it with.';
COMMENT ON COLUMN pp.ledger_accounts.balance IS
    'A projection. The entries are authoritative; a nightly job recomputes the fold and raises a '
    'CRITICAL reconciliation exception on drift. entry_count is carried alongside because two '
    'projections with the same balance and different entry counts have found each other''s bug.';

ALTER TABLE pp.ledger_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.ledger_accounts FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.ledger_accounts
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE pp.ledger_entries (
    entry_id             TEXT        NOT NULL
                         CHECK (entry_id ~ '^led_[0-9A-HJKMNP-TV-Z]{26}$'),
    partition_month      TIMESTAMPTZ NOT NULL,
    tenant_id            TEXT        NOT NULL,
    merchant_id          TEXT        NOT NULL,
    account_type         TEXT        NOT NULL CHECK (account_type IN (
                             'MERCHANT_RECEIVABLE', 'GATEWAY_CLEARING', 'FEES_PAYABLE',
                             'REFUNDS_PAYABLE', 'DISPUTES_HELD', 'SETTLEMENT_SUSPENSE')),
    side                 TEXT        NOT NULL CHECK (side IN ('DEBIT', 'CREDIT')),
    amount               BIGINT      NOT NULL CHECK (amount > 0),
    currency             CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    entry_type           TEXT        NOT NULL,
    transaction_group_id TEXT        NOT NULL CHECK (transaction_group_id <> ''),
    source_event_id      TEXT        NOT NULL DEFAULT '',
    source_event_type    TEXT        NOT NULL DEFAULT '',
    payment_id           TEXT        NOT NULL DEFAULT '',
    attempt_id           TEXT        NOT NULL DEFAULT '',
    refund_id            TEXT        NOT NULL DEFAULT '',
    gateway_ref          TEXT        NOT NULL DEFAULT '',
    description          TEXT        NOT NULL DEFAULT '',
    occurred_at          TIMESTAMPTZ NOT NULL,
    recorded_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (entry_id, partition_month)
) PARTITION BY RANGE (partition_month);

COMMENT ON COLUMN pp.ledger_entries.amount IS
    'Always strictly positive; direction is carried by side and by nothing else. Permitting a '
    'negative debit would give every movement two spellings, and a ledger with two spellings for '
    'the same thing is a ledger whose reports depend on which spelling the writer happened to use.';
COMMENT ON COLUMN pp.ledger_entries.partition_month IS
    'The *payment''s* month when a payment is present, not the entry''s. A refund six months '
    'after a capture, or a dispute a year later, still lands with its payment, so the reconciler '
    'reads a payment and all of its ledger impact with one partition scan.';

-- Idempotent posting under at-least-once delivery. A redelivered event cannot double-post
-- because the second insert of the same (event, account, direction) triple collides.
CREATE UNIQUE INDEX uq_ledger_source
    ON pp.ledger_entries (source_event_id, account_type, side, partition_month)
    WHERE source_event_id <> '';
CREATE INDEX idx_ledger_group ON pp.ledger_entries (transaction_group_id);
CREATE INDEX idx_ledger_account_time
    ON pp.ledger_entries (tenant_id, merchant_id, account_type, currency, occurred_at DESC);
CREATE INDEX idx_ledger_payment ON pp.ledger_entries (payment_id, partition_month)
    WHERE payment_id <> '';

COMMENT ON INDEX pp.idx_ledger_payment IS
    '"Show the ledger impact of this payment" - the first question in every payment dispute.';

ALTER TABLE pp.ledger_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.ledger_entries FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.ledger_entries
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Append-only at the role level. This is the control; the trigger in 0013 is the second one.
REVOKE UPDATE, DELETE ON pp.ledger_entries FROM pp_app;

CREATE TABLE pp.reconciliation_runs (
    run_id            TEXT        PRIMARY KEY,
    tenant_id         TEXT        NOT NULL,
    gateway_id        TEXT        NOT NULL DEFAULT '',
    run_type          TEXT        NOT NULL CHECK (run_type IN (
                          'UNKNOWN_ATTEMPT_RESOLUTION', 'SETTLEMENT_MATCH',
                          'LEDGER_BALANCE', 'DESIRED_VS_ACTUAL_CONFIG')),
    window_start      TIMESTAMPTZ NOT NULL,
    window_end        TIMESTAMPTZ NOT NULL,
    state             TEXT        NOT NULL DEFAULT 'SCHEDULED'
                      CHECK (state IN ('SCHEDULED', 'RUNNING', 'COMPLETED', 'FAILED')),
    records_examined  INTEGER     NOT NULL DEFAULT 0,
    exceptions_opened INTEGER     NOT NULL DEFAULT 0,
    exceptions_closed INTEGER     NOT NULL DEFAULT 0,
    report_uri        TEXT,
    version           BIGINT      NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT uq_recon_window
        UNIQUE (tenant_id, gateway_id, run_type, window_start, window_end),
    CONSTRAINT recon_window_is_ordered CHECK (window_end > window_start)
);

COMMENT ON CONSTRAINT uq_recon_window ON pp.reconciliation_runs IS
    'Re-running a window must be idempotent. Without this, a retried scheduler produces a second '
    'run over the same window and a second copy of every exception it finds.';

ALTER TABLE pp.reconciliation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.reconciliation_runs FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.reconciliation_runs
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE pp.reconciliation_exceptions (
    exception_id TEXT        PRIMARY KEY,
    run_id       TEXT        REFERENCES pp.reconciliation_runs (run_id) ON DELETE SET NULL,
    tenant_id    TEXT        NOT NULL,
    merchant_id  TEXT        NOT NULL DEFAULT '',
    payment_id   TEXT        NOT NULL DEFAULT '',
    attempt_id   TEXT        NOT NULL DEFAULT '',
    external_ref TEXT        NOT NULL DEFAULT '',
    kind         TEXT        NOT NULL CHECK (kind <> ''),
    severity     TEXT        NOT NULL CHECK (severity IN ('CRITICAL', 'MAJOR', 'MINOR')),
    detail       TEXT        NOT NULL DEFAULT '',
    expected     JSONB       NOT NULL DEFAULT '{}',
    actual       JSONB       NOT NULL DEFAULT '{}',
    state        TEXT        NOT NULL DEFAULT 'OPEN'
                 CHECK (state IN ('OPEN', 'INVESTIGATING', 'RESOLVED', 'ACCEPTED')),
    assignee     TEXT        NOT NULL DEFAULT '',
    resolution   TEXT        NOT NULL DEFAULT '',
    resolved_by  TEXT        NOT NULL DEFAULT '',
    opened_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at  TIMESTAMPTZ,

    -- Exception identity is the discrepancy, not the run. Re-detecting the same discrepancy in a
    -- later run must update this row rather than open a second one, or an unresolved exception
    -- accumulates one duplicate per run until the queue is unusable.
    CONSTRAINT uq_recon_exception_identity
        UNIQUE (tenant_id, kind, payment_id, external_ref),
    CONSTRAINT recon_resolved_has_time
        CHECK ((state IN ('RESOLVED', 'ACCEPTED')) = (resolved_at IS NOT NULL))
);

CREATE INDEX idx_recon_open_critical ON pp.reconciliation_exceptions (tenant_id, merchant_id)
    WHERE state = 'OPEN' AND severity = 'CRITICAL';
COMMENT ON INDEX pp.idx_recon_open_critical IS
    'The merchant activation guard. Partial and tiny, because the guard sits on the activation '
    'path and must not degrade as the exception history grows.';

CREATE INDEX idx_recon_open ON pp.reconciliation_exceptions (tenant_id, severity, opened_at DESC)
    WHERE state IN ('OPEN', 'INVESTIGATING');

ALTER TABLE pp.reconciliation_exceptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.reconciliation_exceptions FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.reconciliation_exceptions
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
