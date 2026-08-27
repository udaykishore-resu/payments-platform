-- 0012 — the transactional outbox.
--
-- The state row and the event row commit together or not at all, so the dual-write failure mode
-- (state committed, event lost; or event published, state rolled back) never arises. The relay
-- is at-least-once by construction and duplicates are handled by the consumer dedup table.

CREATE TABLE pp.outbox_events (
    outbox_id         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id          TEXT        NOT NULL UNIQUE
                      CHECK (event_id ~ '^evt_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id         TEXT        NOT NULL,
    aggregate_type    TEXT        NOT NULL DEFAULT '',
    aggregate_id      TEXT        NOT NULL DEFAULT '',
    event_type        TEXT        NOT NULL CHECK (event_type <> ''),
    topic             TEXT        NOT NULL CHECK (topic <> ''),
    partition_key     TEXT        NOT NULL,

    -- shard_bucket is the mechanism that keeps one aggregate's events in order when the relay is
    -- scaled out. It is derived from the partition key, so every event of one payment lands in
    -- the same bucket, and a relay replica claims whole buckets. Two replicas can therefore never
    -- hold two events of the same aggregate at the same time and publish them out of order -
    -- which FOR UPDATE SKIP LOCKED alone does NOT prevent: SKIP LOCKED stops two replicas from
    -- claiming the same ROW, not from claiming two rows of the same aggregate.
    --
    -- 64 fixed buckets rather than a modulus over the live replica count: the bucket must not
    -- change when the fleet scales, or a rescale would move an aggregate mid-stream and reorder
    -- exactly the events that were in flight during the rescale.
    shard_bucket      SMALLINT    NOT NULL
                      GENERATED ALWAYS AS ((hashtext(partition_key) & 63)::SMALLINT) STORED,

    payload           BYTEA       NOT NULL,
    headers           JSONB       NOT NULL DEFAULT '{}',
    occurred_at       TIMESTAMPTZ NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    available_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at        TIMESTAMPTZ,
    claimed_by        TEXT        NOT NULL DEFAULT '',
    published_at      TIMESTAMPTZ,
    publish_attempts  SMALLINT    NOT NULL DEFAULT 0 CHECK (publish_attempts >= 0),
    last_error        TEXT        NOT NULL DEFAULT '',

    CONSTRAINT outbox_headers_is_object CHECK (jsonb_typeof(headers) = 'object')
);

COMMENT ON COLUMN pp.outbox_events.available_at IS
    'Supports delayed publication for the retry tiers. The claim predicate is available_at <= '
    'now(), so a failed publish is rescheduled by moving this forward rather than by holding the '
    'row in memory somewhere.';

-- The relay's claim index. Partial on published_at IS NULL so it shrinks back to near-zero once
-- a backlog drains; a non-partial index would stay bloated forever, carrying every event the
-- platform has ever published.
CREATE INDEX idx_outbox_unpublished
    ON pp.outbox_events (shard_bucket, available_at, outbox_id)
    WHERE published_at IS NULL;

CREATE INDEX idx_outbox_published_sweep ON pp.outbox_events (published_at)
    WHERE published_at IS NOT NULL;

COMMENT ON INDEX pp.idx_outbox_unpublished IS
    'Column order matters: shard_bucket first so a replica''s claim is an index range rather than '
    'a filter over the whole backlog, then available_at and outbox_id so the claim within a '
    'bucket is in insertion order - which is what per-aggregate ordering ultimately rests on.';

ALTER TABLE pp.outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.outbox_events FORCE  ROW LEVEL SECURITY;

-- The writer's policy: the ordinary tenant predicate, because an outbox row is written in the
-- same transaction as the tenant-scoped state change that produced it.
CREATE POLICY tenant_isolation ON pp.outbox_events
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- The relay's policy: deliberately platform-wide, and deliberately scoped to a role that has no
-- access at all to payments, merchants or configurations. Widening a policy for a specific
-- minimal role is auditable; granting BYPASSRLS to the application role is not.
CREATE POLICY relay_reads_all ON pp.outbox_events
    FOR ALL
    TO pp_relay
    USING      (true)
    WITH CHECK (true);

GRANT SELECT, UPDATE ON pp.outbox_events TO pp_relay;
