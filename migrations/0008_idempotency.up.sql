-- 0008 — the idempotency record.
--
-- Postgres is authoritative here and Redis is a read-through accelerator in front of it
-- (ADR-009). The unique index below IS the concurrency control: two concurrent identical
-- requests resolve deterministically because one INSERT wins and the other returns zero rows,
-- not because the application read first and then decided.
--
-- Not partitioned, deliberately. Retention is 7 days and the sweep is small, but the real reason
-- is that a unique index on a partitioned table must include the partition key - and a claim
-- tuple that included a partition key would stop being the claim.

CREATE TABLE pp.idempotency_records (
    idempotency_record_id TEXT        PRIMARY KEY,
    tenant_id             TEXT        NOT NULL,
    merchant_id           TEXT        NOT NULL DEFAULT '',
    method                TEXT        NOT NULL CHECK (method <> ''),
    path_template         TEXT        NOT NULL CHECK (path_template <> ''),
    idempotency_key       TEXT        NOT NULL
                          CHECK (length(idempotency_key) BETWEEN 1 AND 255),
    request_fingerprint   TEXT        NOT NULL
                          CHECK (request_fingerprint ~ '^[0-9a-f]{64}$'),
    state                 TEXT        NOT NULL DEFAULT 'IN_FLIGHT'
                          CHECK (state IN ('IN_FLIGHT', 'COMPLETED', 'FAILED_TERMINAL')),
    lease_owner           TEXT        NOT NULL DEFAULT '',
    lease_expires_at      TIMESTAMPTZ NOT NULL,
    response_status       SMALLINT    CHECK (response_status IS NULL
                                             OR response_status BETWEEN 100 AND 599),
    response_body         BYTEA,
    resource_id           TEXT        NOT NULL DEFAULT '',
    request_id            TEXT        NOT NULL DEFAULT '',
    trace_id              TEXT        NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at          TIMESTAMPTZ,
    expires_at            TIMESTAMPTZ NOT NULL,

    -- The claim tuple. baseline section 14.1: a client's key is only unique within a tenant, a
    -- merchant and an endpoint. Scoping by the key alone would let one tenant's choice of key
    -- collide with another's - and the collision would look like a successful replay.
    CONSTRAINT uq_idem_claim
        UNIQUE (tenant_id, merchant_id, method, path_template, idempotency_key),

    CONSTRAINT idem_settled_has_response
        CHECK (state = 'IN_FLIGHT' OR (response_status IS NOT NULL AND completed_at IS NOT NULL))
);

COMMENT ON TABLE pp.idempotency_records IS
    'The authoritative record of which logical operations have run. A rejected request still '
    'consumed its key, which is why this table exists independently of the payment it guards.';
COMMENT ON COLUMN pp.idempotency_records.request_fingerprint IS
    'SHA-256 over the JCS-canonicalized body plus the scope tuple. Same key, different '
    'fingerprint is a client bug - one key used for two different operations - and must be '
    'reported as 422 IDEMPOTENCY_KEY_REUSED rather than silently treated as a replay.';
COMMENT ON COLUMN pp.idempotency_records.lease_expires_at IS
    'NOT NULL so the reclaim predicate is total. A NULL lease deadline would make '
    '"WHERE lease_expires_at < now()" skip exactly the rows a crashed process left behind.';

CREATE INDEX idx_idem_expiry ON pp.idempotency_records (expires_at);
CREATE INDEX idx_idem_lease ON pp.idempotency_records (lease_expires_at)
    WHERE state = 'IN_FLIGHT';

COMMENT ON INDEX pp.idx_idem_lease IS
    'Reclaiming leases from dead processes. Partial on IN_FLIGHT because a completed record''s '
    'lease deadline is meaningless and would otherwise bloat the index with every request ever made.';

ALTER TABLE pp.idempotency_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.idempotency_records FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.idempotency_records
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
