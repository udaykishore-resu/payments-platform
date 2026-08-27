-- 0004 — BC-3: onboarding cases, the workflow engine's durable state, and certification reports.
--
-- The workflow tables are storage-shaped rather than domain-shaped on purpose: the engine *is*
-- the domain logic here. The lease and epoch columns are the load-bearing part - see the comment
-- on workflow_instances.attempt_epoch.

CREATE TABLE pp.onboarding_cases (
    case_id              TEXT        PRIMARY KEY
                         CHECK (case_id ~ '^onb_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id            TEXT        NOT NULL,
    merchant_id          TEXT        NOT NULL,
    workflow_instance_id TEXT        NOT NULL,
    status               TEXT        NOT NULL DEFAULT 'OPEN'
                         CHECK (status IN ('OPEN', 'BLOCKED', 'COMPLETED', 'ABANDONED')),
    current_step_key     TEXT        NOT NULL DEFAULT '',
    blocked_reason       TEXT        NOT NULL DEFAULT '',
    selected_gateways    TEXT[]      NOT NULL DEFAULT '{}',
    annotations          JSONB       NOT NULL DEFAULT '[]',
    sla_due_at           TIMESTAMPTZ,
    opened_at            TIMESTAMPTZ NOT NULL,
    closed_at            TIMESTAMPTZ,
    version              BIGINT      NOT NULL DEFAULT 0,

    CONSTRAINT onboarding_closed_is_terminal
        CHECK ((status IN ('COMPLETED', 'ABANDONED')) = (closed_at IS NOT NULL)),
    CONSTRAINT onboarding_annotations_is_array CHECK (jsonb_typeof(annotations) = 'array')
);

-- One live case per merchant. Without this, a double-submitted onboarding request produces two
-- cases, two workflow instances and two gateway provisioning runs against the same merchant.
CREATE UNIQUE INDEX uq_case_live ON pp.onboarding_cases (tenant_id, merchant_id)
    WHERE status IN ('OPEN', 'BLOCKED');

ALTER TABLE pp.onboarding_cases ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.onboarding_cases FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.onboarding_cases
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE pp.workflow_instances (
    instance_id       TEXT        PRIMARY KEY
                      CHECK (instance_id ~ '^wfr_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id         TEXT        NOT NULL,
    workflow_name     TEXT        NOT NULL CHECK (workflow_name <> ''),
    workflow_version  INTEGER     NOT NULL DEFAULT 1 CHECK (workflow_version >= 1),
    business_key      TEXT        NOT NULL CHECK (business_key <> ''),
    state             TEXT        NOT NULL DEFAULT 'PENDING' CHECK (state IN (
                          'PENDING', 'RUNNING', 'WAITING_SIGNAL', 'COMPENSATING',
                          'COMPLETED', 'FAILED', 'ABORTED')),
    current_step      TEXT        NOT NULL DEFAULT '',
    input             JSONB       NOT NULL DEFAULT '{}',
    checkpoint        JSONB       NOT NULL DEFAULT '{}',
    attempt           INTEGER     NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    lease_owner       TEXT,
    lease_expires_at  TIMESTAMPTZ,
    attempt_epoch     BIGINT      NOT NULL DEFAULT 0 CHECK (attempt_epoch >= 0),
    run_after         TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error        TEXT        NOT NULL DEFAULT '',
    correlation_id    TEXT        NOT NULL DEFAULT '',
    version           BIGINT      NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL,
    completed_at      TIMESTAMPTZ,

    -- A half-set lease is a lease nobody can reason about: an owner with no deadline never
    -- expires, and a deadline with no owner cannot be renewed. Both or neither.
    CONSTRAINT workflow_lease_is_paired
        CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL)),
    CONSTRAINT workflow_terminal_has_completed_at
        CHECK ((state IN ('COMPLETED', 'FAILED', 'ABORTED')) = (completed_at IS NOT NULL))
);

COMMENT ON COLUMN pp.workflow_instances.attempt_epoch IS
    'Fencing token. It increments on every lease acquisition, and every step write carries the '
    'epoch it believes it holds. A worker that paused past its lease expiry, had the instance '
    'taken over, and then woke up will present a stale epoch and its write is rejected. Without '
    'this the lease is advisory: the expiry only stops a *polite* worker.';
COMMENT ON COLUMN pp.workflow_instances.checkpoint IS
    'Every completed step''s result is written here before the next step begins. That ordering '
    'is what makes a crash resumable rather than a replay of side effects.';

CREATE UNIQUE INDEX uq_wf_business_key
    ON pp.workflow_instances (tenant_id, workflow_name, business_key)
    WHERE state NOT IN ('COMPLETED', 'FAILED', 'ABORTED');

COMMENT ON INDEX pp.uq_wf_business_key IS
    'baseline section 11: starting a workflow twice is a no-op returning the existing instance. '
    'The index is what makes that true under concurrency rather than under a read-then-write race.';

CREATE INDEX idx_wf_lease ON pp.workflow_instances (run_after, lease_expires_at, instance_id)
    WHERE state IN ('PENDING', 'RUNNING', 'COMPENSATING');
CREATE INDEX idx_wf_signal_wait ON pp.workflow_instances (tenant_id, workflow_name, business_key)
    WHERE state = 'WAITING_SIGNAL';
CREATE INDEX idx_wf_stuck ON pp.workflow_instances (updated_at)
    WHERE state IN ('PENDING', 'RUNNING', 'WAITING_SIGNAL', 'COMPENSATING');

COMMENT ON INDEX pp.idx_wf_stuck IS
    'FindStuck. A workflow that is neither running nor failed nor complete is the failure mode '
    'nobody alerts on until a merchant calls.';

ALTER TABLE pp.workflow_instances ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.workflow_instances FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.workflow_instances
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE pp.workflow_steps (
    step_id        TEXT        PRIMARY KEY
                   CHECK (step_id ~ '^wfs_[0-9A-HJKMNP-TV-Z]{26}$'),
    instance_id    TEXT        NOT NULL REFERENCES pp.workflow_instances (instance_id) ON DELETE CASCADE,
    tenant_id      TEXT        NOT NULL,
    name           TEXT        NOT NULL CHECK (name <> ''),
    sequence       INTEGER     NOT NULL CHECK (sequence >= 0),
    state          TEXT        NOT NULL DEFAULT 'PENDING' CHECK (state IN (
                       'PENDING', 'RUNNING', 'SUCCEEDED', 'FAILED', 'SKIPPED',
                       'COMPENSATING', 'COMPENSATED', 'COMPENSATION_FAILED')),
    attempt        INTEGER     NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    input          JSONB       NOT NULL DEFAULT '{}',
    output         JSONB       NOT NULL DEFAULT '{}',
    error          TEXT        NOT NULL DEFAULT '',
    started_at     TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ,
    compensated_at TIMESTAMPTZ,

    CONSTRAINT uq_step_sequence UNIQUE (instance_id, sequence),
    CONSTRAINT uq_step_name_attempt UNIQUE (instance_id, name, attempt)
);

CREATE INDEX idx_steps_instance ON pp.workflow_steps (instance_id, sequence);

ALTER TABLE pp.workflow_steps ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.workflow_steps FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.workflow_steps
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE pp.workflow_dlq (
    dlq_id       BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id    TEXT        NOT NULL,
    instance_id  TEXT        NOT NULL,
    step_key     TEXT        NOT NULL DEFAULT '',
    payload      JSONB       NOT NULL DEFAULT '{}',
    reason       TEXT        NOT NULL DEFAULT '',
    parked_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    replayed_at  TIMESTAMPTZ,
    replay_count SMALLINT    NOT NULL DEFAULT 0 CHECK (replay_count BETWEEN 0 AND 5)
);

COMMENT ON CONSTRAINT workflow_dlq_replay_count_check ON pp.workflow_dlq IS
    'A DLQ entry that can be replayed without bound is a retry loop with a manual trigger. Five '
    'is where an operator has to look at why it keeps failing instead of pressing the button again.';

CREATE INDEX idx_dlq_unreplayed ON pp.workflow_dlq (parked_at) WHERE replayed_at IS NULL;

ALTER TABLE pp.workflow_dlq ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.workflow_dlq FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.workflow_dlq
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE pp.certification_reports (
    report_id       TEXT        PRIMARY KEY,
    tenant_id       TEXT        NOT NULL,
    merchant_id     TEXT        NOT NULL,
    gateway_id      TEXT        NOT NULL,
    environment     TEXT        NOT NULL CHECK (environment IN ('sandbox', 'production')),
    suite_version   TEXT        NOT NULL DEFAULT '',
    state           TEXT        NOT NULL DEFAULT 'RUNNING'
                    CHECK (state IN ('RUNNING', 'PASSED', 'FAILED')),
    matrix          JSONB       NOT NULL DEFAULT '[]',
    assertions      JSONB       NOT NULL DEFAULT '[]',
    artifact_uri    TEXT,
    artifact_sha256 TEXT CHECK (artifact_sha256 IS NULL OR artifact_sha256 ~ '^[0-9a-f]{64}$'),
    signature       TEXT,
    started_at      TIMESTAMPTZ NOT NULL,
    completed_at    TIMESTAMPTZ,

    CONSTRAINT cert_sealed_has_artifact
        CHECK (state = 'RUNNING' OR (artifact_sha256 IS NOT NULL AND completed_at IS NOT NULL))
);

COMMENT ON TABLE pp.certification_reports IS
    'A report is PASSED only if every matrix cell passes all seven assertions. There is '
    'deliberately no waiver column: a waiver flag is the mechanism by which "certified" degrades '
    'into an opinion.';

CREATE INDEX idx_cert_merchant
    ON pp.certification_reports (tenant_id, merchant_id, gateway_id, started_at DESC);

ALTER TABLE pp.certification_reports ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.certification_reports FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.certification_reports
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
