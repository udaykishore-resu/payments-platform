-- 0011 — BC-9: the hash-chained audit table.
--
-- "Append-only" is a claim; the chain is the evidence. entry_digest covers prev_digest, so
-- altering any record invalidates every digest after it, and the head digest is the whole chain
-- compressed into thirty-two bytes.

CREATE TABLE pp.audit_records (
    audit_id       TEXT        NOT NULL
                   CHECK (audit_id ~ '^aud_[0-9A-HJKMNP-TV-Z]{26}$'),
    partition_month TIMESTAMPTZ NOT NULL,
    tenant_id      TEXT        NOT NULL,
    sequence       BIGINT      NOT NULL CHECK (sequence >= 1),
    actor_type     TEXT        NOT NULL CHECK (actor_type IN ('USER', 'SERVICE', 'SYSTEM')),
    actor_id       TEXT        NOT NULL DEFAULT '',
    actor_name     TEXT        NOT NULL DEFAULT '',
    actor_ip       TEXT        NOT NULL DEFAULT '',
    actor_user_agent TEXT      NOT NULL DEFAULT '',
    actor_on_behalf_of TEXT    NOT NULL DEFAULT '',
    action         TEXT        NOT NULL CHECK (action <> ''),
    resource_type  TEXT        NOT NULL DEFAULT '',
    resource_id    TEXT        NOT NULL DEFAULT '',
    outcome        TEXT        NOT NULL CHECK (outcome IN ('SUCCESS', 'FAILURE', 'DENIED')),
    before_state   JSONB,
    after_state    JSONB,
    reason         TEXT        NOT NULL DEFAULT '',
    correlation_id TEXT        NOT NULL DEFAULT '',
    trace_id       TEXT        NOT NULL DEFAULT '',
    occurred_at    TIMESTAMPTZ NOT NULL,
    recorded_at    TIMESTAMPTZ NOT NULL,
    prev_digest    TEXT        NOT NULL CHECK (prev_digest ~ '^[0-9a-f]{64}$'),
    entry_digest   TEXT        NOT NULL CHECK (entry_digest ~ '^[0-9a-f]{64}$'),

    PRIMARY KEY (audit_id, partition_month)
) PARTITION BY RANGE (partition_month);

COMMENT ON TABLE pp.audit_records IS
    'Append-only and tamper-evident. Retention 7 years, WORM in the archive. Per-tenant insert '
    'is serialized by pg_advisory_xact_lock(hashtext(tenant_id)) in the repository, which is '
    'acceptable because the audit write is off the response path and is not in the payment '
    'latency budget.';
COMMENT ON COLUMN pp.audit_records.before_state IS
    'Allowlisted snapshot. No PII, no PAN, no secrets: the same allowlist that governs '
    'structured logs, so a Secret[T] serializes to [REDACTED] here exactly as it does there.';
COMMENT ON COLUMN pp.audit_records.partition_month IS
    'Derived from the audit ULID, not from a timestamp column. It matters more here than '
    'anywhere: this table has no UPDATE grant, so a row must land in the right partition the '
    'first time - there is no way to move it afterwards.';

-- The sequence is dense and monotonic per tenant, and the unique index is what makes a gap a
-- tamper signal rather than a coincidence. It has to include the partition key, so global
-- density is asserted by the verifier walking the chain rather than by the index alone.
CREATE UNIQUE INDEX uq_audit_sequence ON pp.audit_records (tenant_id, sequence, partition_month);
CREATE UNIQUE INDEX uq_audit_digest ON pp.audit_records (tenant_id, entry_digest, partition_month);
CREATE INDEX idx_audit_resource
    ON pp.audit_records (tenant_id, resource_type, resource_id, recorded_at DESC);
CREATE INDEX idx_audit_actor ON pp.audit_records (tenant_id, actor_id, recorded_at DESC);
CREATE INDEX idx_audit_chain ON pp.audit_records (tenant_id, sequence DESC);

-- Deliberately NOT created: an index on action. High cardinality, low selectivity, and the
-- audit UI always scopes by tenant plus resource or tenant plus actor first.

ALTER TABLE pp.audit_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.audit_records FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.audit_records
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

REVOKE UPDATE, DELETE ON pp.audit_records FROM pp_app;
