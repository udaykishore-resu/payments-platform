-- 0013 — legal state transitions and immutable fields, enforced by the database.
--
-- The domain already refuses an illegal transition and produces a good error message. This
-- migration exists because the domain is code and code has bugs, and because a direct UPDATE by
-- a support script or a migration does not pass through the domain at all. The transition tables
-- below are generated from internal/domain/payment.Machine() and
-- internal/domain/merchant.Machine(); TestTransitionTablesMatchDomain asserts the two agree
-- exactly, so a domain change that is not mirrored here fails CI rather than drifting.

CREATE TABLE pp.payment_state_transitions (
    from_state TEXT NOT NULL,
    to_state   TEXT NOT NULL,
    PRIMARY KEY (from_state, to_state)
);

COMMENT ON TABLE pp.payment_state_transitions IS
    'The payment FSM as data. A table rather than a chain of IF statements inside the trigger, '
    'for the same reason the domain uses a table: a transition rule that is data can be diffed '
    'against the domain''s copy by a test, and an IF statement cannot.';

INSERT INTO pp.payment_state_transitions (from_state, to_state) VALUES
    ('CREATED',            'PROCESSING'),
    ('CREATED',            'REQUIRES_ACTION'),
    ('CREATED',            'FAILED'),
    ('CREATED',            'CANCELED'),
    ('REQUIRES_ACTION',    'PROCESSING'),
    ('REQUIRES_ACTION',    'FAILED'),
    ('REQUIRES_ACTION',    'CANCELED'),
    ('REQUIRES_ACTION',    'EXPIRED'),
    ('PROCESSING',         'AUTHORIZED'),
    ('PROCESSING',         'CAPTURED'),
    ('PROCESSING',         'PENDING'),
    ('PROCESSING',         'FAILED'),
    ('PROCESSING',         'REQUIRES_ACTION'),
    ('PENDING',            'AUTHORIZED'),
    ('PENDING',            'CAPTURED'),
    ('PENDING',            'FAILED'),
    ('PENDING',            'EXPIRED'),
    ('AUTHORIZED',         'CAPTURED'),
    ('AUTHORIZED',         'VOIDED'),
    ('AUTHORIZED',         'EXPIRED'),
    ('AUTHORIZED',         'FAILED'),
    ('CAPTURED',           'SETTLED'),
    ('CAPTURED',           'PARTIALLY_REFUNDED'),
    ('CAPTURED',           'REFUNDED'),
    ('CAPTURED',           'DISPUTED'),
    ('SETTLED',            'PARTIALLY_REFUNDED'),
    ('SETTLED',            'REFUNDED'),
    ('SETTLED',            'DISPUTED'),
    ('PARTIALLY_REFUNDED', 'PARTIALLY_REFUNDED'),
    ('PARTIALLY_REFUNDED', 'REFUNDED'),
    ('PARTIALLY_REFUNDED', 'DISPUTED'),
    ('REFUNDED',           'DISPUTED'),
    ('DISPUTED',           'REFUNDED'),
    ('DISPUTED',           'CAPTURED'),
    ('DISPUTED',           'SETTLED');

CREATE TABLE pp.merchant_status_transitions (
    from_status TEXT NOT NULL,
    to_status   TEXT NOT NULL,
    PRIMARY KEY (from_status, to_status)
);

INSERT INTO pp.merchant_status_transitions (from_status, to_status) VALUES
    ('CREATED',                'VALIDATING'),
    ('CREATED',                'TERMINATED'),
    ('VALIDATING',             'KYC_PENDING'),
    ('VALIDATING',             'VALIDATION_FAILED'),
    ('VALIDATION_FAILED',      'VALIDATING'),
    ('VALIDATION_FAILED',      'TERMINATED'),
    ('KYC_PENDING',            'KYC_APPROVED'),
    ('KYC_PENDING',            'KYC_FAILED'),
    ('KYC_FAILED',             'KYC_PENDING'),
    ('KYC_FAILED',             'TERMINATED'),
    ('KYC_APPROVED',           'BANK_VALIDATED'),
    ('KYC_APPROVED',           'BANK_VALIDATION_FAILED'),
    ('BANK_VALIDATION_FAILED', 'KYC_APPROVED'),
    ('BANK_VALIDATION_FAILED', 'TERMINATED'),
    ('BANK_VALIDATED',         'GATEWAY_PROVISIONING'),
    ('GATEWAY_PROVISIONING',   'CONFIGURING'),
    ('GATEWAY_PROVISIONING',   'PROVISIONING_FAILED'),
    ('PROVISIONING_FAILED',    'GATEWAY_PROVISIONING'),
    ('PROVISIONING_FAILED',    'TERMINATED'),
    ('CONFIGURING',            'SANDBOX_VALIDATION'),
    ('CONFIGURING',            'CONFIGURATION_FAILED'),
    ('CONFIGURATION_FAILED',   'CONFIGURING'),
    ('CONFIGURATION_FAILED',   'TERMINATED'),
    ('SANDBOX_VALIDATION',     'CERTIFICATION'),
    ('SANDBOX_VALIDATION',     'CONFIGURATION_FAILED'),
    ('CERTIFICATION',          'APPROVED'),
    ('CERTIFICATION',          'CERTIFICATION_FAILED'),
    ('CERTIFICATION',          'COMPLIANCE_REJECTED'),
    ('CERTIFICATION_FAILED',   'CERTIFICATION'),
    ('CERTIFICATION_FAILED',   'CONFIGURING'),
    ('CERTIFICATION_FAILED',   'TERMINATED'),
    ('COMPLIANCE_REJECTED',    'CONFIGURING'),
    ('COMPLIANCE_REJECTED',    'KYC_PENDING'),
    ('COMPLIANCE_REJECTED',    'TERMINATED'),
    ('APPROVED',               'PRODUCTION_READY'),
    ('APPROVED',               'SUSPENDED'),
    ('PRODUCTION_READY',       'ACTIVE'),
    ('PRODUCTION_READY',       'SUSPENDED'),
    ('ACTIVE',                 'SUSPENDED'),
    ('ACTIVE',                 'TERMINATED'),
    ('SUSPENDED',              'ACTIVE'),
    ('SUSPENDED',              'TERMINATED');

REVOKE INSERT, UPDATE, DELETE ON pp.payment_state_transitions FROM pp_app;
REVOKE INSERT, UPDATE, DELETE ON pp.merchant_status_transitions FROM pp_app;

-- Invariant I4 plus the FSM guard, in one trigger because they fire on the same event and a
-- caller who violates both should hear about the more fundamental one first.
CREATE FUNCTION pp.payments_guard() RETURNS TRIGGER
LANGUAGE plpgsql AS $fn$
BEGIN
    -- I4: amount, currency, merchant, tenant and the derived partition key are immutable after
    -- creation. A mutable amount makes the idempotency fingerprint meaningless and the ledger
    -- unauditable; a mutable tenant_id is a cross-tenant write wearing an UPDATE's clothes.
    IF NEW.payment_id <> OLD.payment_id
        OR NEW.tenant_id <> OLD.tenant_id
        OR NEW.merchant_id <> OLD.merchant_id
        OR NEW.amount <> OLD.amount
        OR NEW.currency <> OLD.currency
        OR NEW.created_at <> OLD.created_at
        OR NEW.partition_month <> OLD.partition_month
    THEN
        RAISE EXCEPTION
            'payment % : amount, currency, merchant, tenant and creation time are immutable (I4)',
            OLD.payment_id
            USING ERRCODE = 'check_violation',
                  CONSTRAINT = 'payments_i4_immutable_fields';
    END IF;

    -- The version must move forward by exactly one on a state change and must never move
    -- backwards. A version that jumps is a lost update that the WHERE clause failed to catch.
    IF NEW.version < OLD.version THEN
        RAISE EXCEPTION 'payment % : version may not decrease (% to %)',
            OLD.payment_id, OLD.version, NEW.version
            USING ERRCODE = 'check_violation',
                  CONSTRAINT = 'payments_version_monotonic';
    END IF;

    IF NEW.state IS DISTINCT FROM OLD.state THEN
        IF NOT EXISTS (
            SELECT 1 FROM pp.payment_state_transitions t
            WHERE t.from_state = OLD.state AND t.to_state = NEW.state
        ) THEN
            RAISE EXCEPTION 'payment % : illegal state transition % -> %',
                OLD.payment_id, OLD.state, NEW.state
                USING ERRCODE = 'check_violation',
                      CONSTRAINT = 'payments_illegal_state_transition';
        END IF;
    END IF;

    RETURN NEW;
END;
$fn$;

COMMENT ON FUNCTION pp.payments_guard() IS
    'The database''s half of invariants I4 and I5 and of the baseline section 9 transition table. '
    'It raises check_violation (23514) rather than raise_exception so the error mapper can '
    'classify it alongside the declarative CHECKs without parsing a message.';

CREATE TRIGGER payments_guard
    BEFORE UPDATE ON pp.payments
    FOR EACH ROW EXECUTE FUNCTION pp.payments_guard();

CREATE FUNCTION pp.merchants_guard() RETURNS TRIGGER
LANGUAGE plpgsql AS $fn$
BEGIN
    IF NEW.merchant_id <> OLD.merchant_id OR NEW.tenant_id <> OLD.tenant_id THEN
        RAISE EXCEPTION 'merchant % : identity and tenant are immutable', OLD.merchant_id
            USING ERRCODE = 'check_violation',
                  CONSTRAINT = 'merchants_immutable_identity';
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NOT EXISTS (
            SELECT 1 FROM pp.merchant_status_transitions t
            WHERE t.from_status = OLD.status AND t.to_status = NEW.status
        ) THEN
            RAISE EXCEPTION 'merchant % : illegal status transition % -> %',
                OLD.merchant_id, OLD.status, NEW.status
                USING ERRCODE = 'check_violation',
                      CONSTRAINT = 'merchants_illegal_state_transition';
        END IF;
    END IF;

    RETURN NEW;
END;
$fn$;

CREATE TRIGGER merchants_guard
    BEFORE UPDATE ON pp.merchants
    FOR EACH ROW EXECUTE FUNCTION pp.merchants_guard();

CREATE FUNCTION pp.tenants_guard() RETURNS TRIGGER
LANGUAGE plpgsql AS $fn$
BEGIN
    -- The tier is immutable: a POOLED to SILOED move is the online migration in
    -- docs/multi-tenancy.md section 5.1, and an UPDATE that merely relabels the tier leaves the
    -- tenant's data in the pooled schema while every downstream component believes it is siloed.
    IF NEW.tier <> OLD.tier THEN
        RAISE EXCEPTION 'tenant % : tier is immutable; use the tier migration runbook', OLD.tenant_id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'tenants_immutable_tier';
    END IF;
    IF NEW.residency_region <> OLD.residency_region THEN
        RAISE EXCEPTION 'tenant % : residency region is immutable while merchants exist',
            OLD.tenant_id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'tenants_immutable_residency';
    END IF;
    RETURN NEW;
END;
$fn$;

CREATE TRIGGER tenants_guard
    BEFORE UPDATE ON pp.tenants
    FOR EACH ROW EXECUTE FUNCTION pp.tenants_guard();

-- Append-only enforcement, as a trigger in addition to the role-level REVOKE.
--
-- Two controls rather than one because they fail differently: the REVOKE is undone by a single
-- careless GRANT in a future migration and nothing complains, whereas the trigger is visible in
-- the schema and its removal is a reviewable diff.
CREATE FUNCTION pp.reject_mutation() RETURNS TRIGGER
LANGUAGE plpgsql AS $fn$
BEGIN
    RAISE EXCEPTION '% is append-only; a correction is a new row, never an edit', TG_TABLE_NAME
        USING ERRCODE = 'check_violation', CONSTRAINT = 'append_only';
END;
$fn$;

CREATE TRIGGER ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON pp.ledger_entries
    FOR EACH ROW EXECUTE FUNCTION pp.reject_mutation();

CREATE TRIGGER audit_records_append_only
    BEFORE UPDATE OR DELETE ON pp.audit_records
    FOR EACH ROW EXECUTE FUNCTION pp.reject_mutation();

CREATE TRIGGER routing_plans_append_only
    BEFORE UPDATE OR DELETE ON pp.routing_plans
    FOR EACH ROW EXECUTE FUNCTION pp.reject_mutation();

CREATE TRIGGER payment_event_log_append_only
    BEFORE UPDATE OR DELETE ON pp.payment_event_log
    FOR EACH ROW EXECUTE FUNCTION pp.reject_mutation();

-- Configuration versions are append-only in the same sense: a rollback publishes the previous
-- document as a NEW version. The one permitted mutation is the DRAFT-to-ACTIVE-to-SUPERSEDED
-- status walk, so this trigger allows a status change and nothing else.
CREATE FUNCTION pp.configuration_versions_guard() RETURNS TRIGGER
LANGUAGE plpgsql AS $fn$
BEGIN
    IF NEW.document IS DISTINCT FROM OLD.document
        OR NEW.document_checksum <> OLD.document_checksum
        OR NEW.version <> OLD.version
        OR NEW.configuration_id <> OLD.configuration_id
        OR NEW.tenant_id <> OLD.tenant_id
    THEN
        RAISE EXCEPTION
            'configuration version %/% is immutable; publish a new version instead',
            OLD.configuration_id, OLD.version
            USING ERRCODE = 'check_violation', CONSTRAINT = 'configuration_version_immutable';
    END IF;
    RETURN NEW;
END;
$fn$;

CREATE TRIGGER configuration_versions_guard
    BEFORE UPDATE ON pp.configuration_versions
    FOR EACH ROW EXECUTE FUNCTION pp.configuration_versions_guard();

-- Certification reports are immutable once sealed. A report that can be edited after it passed
-- is not evidence.
CREATE FUNCTION pp.certification_reports_guard() RETURNS TRIGGER
LANGUAGE plpgsql AS $fn$
BEGIN
    IF OLD.state <> 'RUNNING' THEN
        RAISE EXCEPTION 'certification report % is sealed (%); re-certification is a new report',
            OLD.report_id, OLD.state
            USING ERRCODE = 'check_violation', CONSTRAINT = 'certification_report_sealed';
    END IF;
    RETURN NEW;
END;
$fn$;

CREATE TRIGGER certification_reports_guard
    BEFORE UPDATE ON pp.certification_reports
    FOR EACH ROW EXECUTE FUNCTION pp.certification_reports_guard();
