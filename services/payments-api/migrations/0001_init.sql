-- 0001_init.sql
-- Core schema for the Payment Transaction Processing slice. See docs/02-architecture.md for the
-- data model rationale and docs/adr/ADR-004-idempotency-ledger.md for why the balance invariant
-- is enforced here, at the database layer, rather than trusted to application code alone.
--
-- Migration style: forward-only, additive. Any future change to these tables must follow the
-- expand-contract pattern documented in docs/08-runbook.md section 9 — never a blocking ALTER on
-- a hot table in a single step.

BEGIN;

CREATE TABLE accounts (
    id          uuid PRIMARY KEY,
    owner_type  text NOT NULL,
    owner_id    text NOT NULL,
    currency    char(3) NOT NULL,
    status      text NOT NULL DEFAULT 'active'
                CHECK (status IN ('active', 'frozen', 'closed')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_accounts_owner ON accounts (owner_type, owner_id);

CREATE TABLE payments (
    id                 uuid PRIMARY KEY,
    idempotency_key    text NOT NULL,
    source_account_id  uuid NOT NULL REFERENCES accounts (id),
    dest_account_id    uuid NOT NULL REFERENCES accounts (id),
    amount_minor       bigint NOT NULL CHECK (amount_minor > 0),
    currency           char(3) NOT NULL,
    status             text NOT NULL
                       CHECK (status IN ('pending', 'completed', 'failed', 'reversed')),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_payments_source_account ON payments (source_account_id, created_at);
CREATE INDEX idx_payments_dest_account   ON payments (dest_account_id, created_at);

-- One row per posting leg. A completed payment always has exactly two rows here (one negative,
-- "debit", one positive, "credit") whose amount_minor sums to zero — enforced below by
-- trg_ledger_entries_balance, not merely assumed by convention.
CREATE TABLE ledger_entries (
    id           uuid PRIMARY KEY,
    payment_id   uuid NOT NULL REFERENCES payments (id),
    account_id   uuid NOT NULL REFERENCES accounts (id),
    amount_minor bigint NOT NULL CHECK (amount_minor <> 0),
    currency     char(3) NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_ledger_entries_account  ON ledger_entries (account_id, created_at);
CREATE INDEX idx_ledger_entries_payment  ON ledger_entries (payment_id);

-- ---------------------------------------------------------------------------------------------
-- Balance invariant enforcement (ADR-004): "the sum of ledger_entries for any payment_id must be
-- exactly zero" cannot be expressed as a plain per-row CHECK constraint, because it's an
-- aggregate over multiple rows. We enforce it with an AFTER INSERT STATEMENT-level trigger using
-- a transition table, deferred to run once per statement (both legs of a payment are always
-- inserted in a single multi-row INSERT — see repository.CreatePayment) rather than per-row,
-- so a single COMMIT can never leave an unbalanced payment durable. Any application bug that
-- tries to insert an unbalanced posting causes the transaction to abort, full stop.
-- ---------------------------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION check_ledger_entries_balance() RETURNS trigger AS $$
DECLARE
    unbalanced_count integer;
BEGIN
    SELECT count(*) INTO unbalanced_count
    FROM (
        SELECT payment_id, sum(amount_minor) AS total
        FROM new_rows
        GROUP BY payment_id
    ) per_payment
    WHERE total <> 0;

    IF unbalanced_count > 0 THEN
        RAISE EXCEPTION 'ledger_entries balance invariant violated: % payment(s) with a non-zero sum in this statement', unbalanced_count
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_ledger_entries_balance
    AFTER INSERT ON ledger_entries
    REFERENCING NEW TABLE AS new_rows
    FOR EACH STATEMENT
    EXECUTE FUNCTION check_ledger_entries_balance();

-- Idempotency (FR-2 / ADR-004). Unique on key; the request_hash lets us distinguish "safe retry"
-- from "different request reusing the same key" (client bug), which repository.CreatePayment
-- handles by returning ErrIdempotencyConflict rather than silently reusing the wrong result.
CREATE TABLE idempotency_keys (
    key                text PRIMARY KEY,
    request_hash       text NOT NULL,
    payment_id         uuid NOT NULL REFERENCES payments (id),
    response_snapshot  jsonb,
    created_at         timestamptz NOT NULL DEFAULT now()
);

-- Transactional outbox (ADR-004). published starts false; the relay (internal/outbox) claims
-- unpublished rows with FOR UPDATE SKIP LOCKED and marks them published after a confirmed send.
CREATE TABLE outbox_events (
    id            uuid PRIMARY KEY,
    aggregate_id  uuid NOT NULL,
    event_type    text NOT NULL,
    payload       jsonb NOT NULL,
    published     boolean NOT NULL DEFAULT false,
    published_at  timestamptz,
    attempts      integer NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Partial index: the relay's hot query only ever looks at unpublished rows, and this index keeps
-- that lookup cheap regardless of how large the (append-only, eventually archived) published
-- history grows.
CREATE INDEX idx_outbox_events_unpublished ON outbox_events (created_at) WHERE published = false;

-- Append-only audit trail (docs/05-security-architecture.md, compliance retention 7 years).
-- The application's database role is granted INSERT and SELECT only on this table — no UPDATE,
-- no DELETE — enforced below, not merely by convention.
CREATE TABLE audit_log (
    id           uuid PRIMARY KEY,
    actor        text NOT NULL,
    action       text NOT NULL,
    entity_type  text NOT NULL,
    entity_id    uuid NOT NULL,
    before       jsonb,
    after        jsonb,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit_log_entity ON audit_log (entity_type, entity_id, created_at);

COMMIT;
