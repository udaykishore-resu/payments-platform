package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// PaymentRepository persists the Payment aggregate — the payment row, its attempts and its
// refunds — inside one transaction, and drains its domain events into the outbox in that same
// transaction.
//
// Two design points that the SQL below exists to serve:
//
//   - **The aggregate loads in one round trip.** Attempts and refunds arrive as JSONB arrays
//     built by correlated subqueries in the same statement. The obvious alternative — load the
//     payment, then loop — is an N+1 that costs one network round trip per child, and on the
//     capture path that is the difference between a 6 ms database budget and a 30 ms one. The
//     less obvious alternative, three statements in a pgx batch, is also one round trip but
//     reads the three parts at three different points in the transaction's snapshot; a single
//     statement cannot.
//   - **Every by-ID query carries partition_month.** It is derived in Go from the payment ULID,
//     so it is an equality predicate the planner can use to prune to a single monthly partition
//     rather than probing eighty-four of them. See amendment A-02.
type PaymentRepository struct {
	q      querier
	tenant shared.TenantID
	clock  shared.Clock
	outbox *OutboxRepository
}

var _ ports.PaymentRepository = (*PaymentRepository)(nil)

// Explicit column lists. There is no SELECT * anywhere in this package: with a positional Scan,
// adding a column to a table silently shifts every value after it, and the first symptom is a
// gateway reference in the decline-reason field of a support ticket six weeks later.
const paymentColumns = `
    payment_id, partition_month, tenant_id, merchant_id, state, amount, currency,
    payment_method, capture_method,
    method_token, method_brand, method_last4, method_exp_month, method_exp_year,
    method_country, method_network_token,
    authorized_amount, captured_amount, refunded_amount,
    selected_gateway, routing_plan_id, current_attempt_id,
    risk_decision, three_ds_status, description, statement_descriptor, metadata,
    customer_ref, customer_email_hash, customer_ip, customer_country,
    idempotency_key, correlation_id, reconciliation_required,
    version, created_at, updated_at, authorized_at, captured_at, expires_at`

// childJSON builds the attempts and refunds arrays. Timestamps are emitted as epoch
// microseconds rather than as formatted strings: microseconds are exactly TIMESTAMPTZ's
// resolution, they survive a JSON round trip without a parser, and they carry no dependence on
// the session's DateStyle — which is a setting an operator can change and nobody would connect
// to a payment failing to load.
const childJSON = `
    coalesce((
        SELECT jsonb_agg(jsonb_build_object(
            'id',              a.attempt_id,
            'gateway_id',      a.gateway_id,
            'connection_id',   coalesce(a.gateway_connection_id, ''),
            'operation',       a.operation,
            'sequence',        a.attempt_number,
            'amount',          a.amount,
            'currency',        a.currency,
            'outcome',         a.outcome,
            'gateway_ref',     a.gateway_reference,
            'idem_key',        a.gateway_idempotency_key,
            'decline_reason',  a.decline_reason_code,
            'error_code',      a.normalized_error_code,
            'error_message',   a.error_message,
            'raw_status',      a.raw_status,
            'no_retry',        a.network_advice_no_retry,
            'dispatched_at',   (EXTRACT(EPOCH FROM a.request_sent_at)      * 1000000)::bigint,
            'resolved_at',     (EXTRACT(EPOCH FROM a.response_received_at) * 1000000)::bigint,
            'latency_ms',      a.latency_ms,
            'created_at',      (EXTRACT(EPOCH FROM a.created_at) * 1000000)::bigint,
            'updated_at',      (EXTRACT(EPOCH FROM a.updated_at) * 1000000)::bigint
        ) ORDER BY a.attempt_number)
        FROM pp.payment_attempts a
        WHERE a.payment_id = p.payment_id AND a.partition_month = p.partition_month
    ), '[]'::jsonb) AS attempts,
    coalesce((
        SELECT jsonb_agg(jsonb_build_object(
            'id',              r.refund_id,
            'amount',          r.amount,
            'currency',        r.currency,
            'reason',          r.reason,
            'status',          r.status,
            'gateway_ref',     r.gateway_reference,
            'idem_key',        r.idempotency_key,
            'failure_code',    r.failure_code,
            'failure_message', r.failure_message,
            'created_at',      (EXTRACT(EPOCH FROM r.created_at)   * 1000000)::bigint,
            'updated_at',      (EXTRACT(EPOCH FROM r.updated_at)   * 1000000)::bigint,
            'submitted_at',    (EXTRACT(EPOCH FROM r.submitted_at) * 1000000)::bigint,
            'settled_at',      (EXTRACT(EPOCH FROM r.settled_at)   * 1000000)::bigint
        ) ORDER BY r.created_at)
        FROM pp.refunds r
        WHERE r.payment_id = p.payment_id AND r.partition_month = p.partition_month
    ), '[]'::jsonb) AS refunds`

const selectPaymentAggregate = `
SELECT p.payment_id, p.partition_month, p.tenant_id, p.merchant_id, p.state, p.amount, p.currency,
       p.payment_method, p.capture_method,
       p.method_token, p.method_brand, p.method_last4, p.method_exp_month, p.method_exp_year,
       p.method_country, p.method_network_token,
       p.authorized_amount, p.captured_amount, p.refunded_amount,
       p.selected_gateway, p.routing_plan_id, p.current_attempt_id,
       p.risk_decision, p.three_ds_status, p.description, p.statement_descriptor, p.metadata,
       p.customer_ref, p.customer_email_hash, p.customer_ip, p.customer_country,
       p.idempotency_key, p.correlation_id, p.reconciliation_required,
       p.version, p.created_at, p.updated_at, p.authorized_at, p.captured_at, p.expires_at,` +
	childJSON + `
FROM pp.payments p
WHERE p.payment_id = $1 AND p.partition_month = $2 AND p.tenant_id = $3`

// Create inserts a new payment and drains its events into the outbox in the same transaction.
//
// created_at is taken from the aggregate, which took it from the clock at construction; the
// partition month is derived from the ULID. The database CHECK
// payments_partition_matches_created_at asserts the two agree, so a writer that ever computed
// one of them from now() instead fails at the insert rather than producing a row that a by-ID
// lookup can never find.
func (r *PaymentRepository) Create(ctx context.Context, p *payment.Payment) error {
	if err := requireOwner(ctx, r.tenant, p.TenantID()); err != nil {
		return err
	}
	const q = `
INSERT INTO pp.payments (` + paymentColumns + `)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,
        $21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40)`

	meta, err := json.Marshal(nonNilMetadata(p.Metadata()))
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "postgres: encode payment metadata")
	}

	if _, err := r.q.Exec(ctx, q,
		p.ID().String(), shared.PartitionMonth(p.ID()), p.TenantID().String(), p.MerchantID().String(),
		string(p.State()), p.Amount().Amount(), string(p.Amount().Currency()),
		string(p.PaymentMethod()), string(p.CaptureMethod()),
		p.MethodRef().Token, p.MethodRef().Brand, p.MethodRef().Last4,
		p.MethodRef().ExpMonth, p.MethodRef().ExpYear, string(p.MethodRef().Country),
		p.MethodRef().NetworkToken,
		authorizedAmountArg(p), p.CapturedAmount().Amount(), p.RefundedAmount().Amount(),
		p.SelectedGateway().String(), p.RoutingPlanID().String(), currentAttemptID(p),
		"ALLOW", "", p.Description(), p.StatementRef(), meta,
		p.Customer().MerchantCustomerID, p.Customer().EmailHash, p.Customer().IPAddress,
		string(p.Customer().Country),
		p.IdempotencyKey(), p.CorrelationID(), p.HasUnresolvedAttempt(),
		int64(p.Version()), p.CreatedAt(), p.UpdatedAt(),
		p.AuthorizedAt(), p.CapturedAt(), p.AuthExpiresAt(),
	); err != nil {
		return mapError(err, "create payment")
	}
	// The row now exists at the aggregate's version, so the first Save inside this same unit of
	// work compares against the right expectation rather than against a version the row skipped.
	p.MarkPersisted()

	return r.drain(ctx, p, "")
}

// Get loads a payment by ID within the caller's tenant.
//
// A payment belonging to a different tenant returns PAYMENT_NOT_FOUND, never a 403. That is not
// politeness: distinguishing "not yours" from "does not exist" turns this endpoint into an
// existence oracle over other tenants' identifiers, and the identifiers are ULIDs — ordered by
// creation time, so guessing a neighbour is cheap. RLS makes the row invisible, so the
// repository sees no rows for both cases and could not tell them apart even if it wanted to.
func (r *PaymentRepository) Get(ctx context.Context, id shared.PaymentID) (*payment.Payment, error) {
	return r.get(ctx, id, selectPaymentAggregate)
}

// GetForUpdate loads a payment holding a row lock.
//
// It is one of exactly three places in this platform where a pessimistic lock appears on the
// money path, and the reason is invariant I1: two concurrent partial refunds that each read
// refunded_amount, each compute a legal increment and each write it will together exceed the
// captured amount. Serializing the read-modify-write on the parent row makes that
// unrepresentable. The alternative — compare-and-swap on refunded_amount with a retry loop — is
// equally correct and produces unbounded retries under a refund storm, which is when it matters.
//
// FOR UPDATE OF p locks only the payments row. Without the OF clause the lock would extend to
// every row the statement touches, which here means every attempt and every refund, for no
// benefit and with a much larger contention footprint.
func (r *PaymentRepository) GetForUpdate(ctx context.Context, id shared.PaymentID) (*payment.Payment, error) {
	return r.get(ctx, id, selectPaymentAggregate+" FOR UPDATE OF p")
}

func (r *PaymentRepository) get(ctx context.Context, id shared.PaymentID, q string) (*payment.Payment, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	month := shared.PartitionMonth(id)
	if month.IsZero() {
		// A malformed identifier has no partition, so there is nothing to prune to and the query
		// would scan every partition to find nothing. Refuse it here.
		return nil, apierror.Newf(apierror.CodeValidationFailed,
			"malformed payment identifier %q", id).
			WithDetail(apierror.Detail{
				Field: "paymentId", Code: "INVALID_IDENTIFIER",
				Message: "expected a prefixed ULID", RuleID: "L1.IDENTIFIER_WELL_FORMED",
			})
	}
	row := r.q.QueryRow(ctx, q, id.String(), month, r.tenant.String())
	p, err := scanPaymentAggregate(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFound(apierror.CodePaymentNotFound, "payment", id.String())
		}
		return nil, mapError(err, "get payment")
	}
	return p, nil
}

// Save persists a modified aggregate using optimistic concurrency on the version it was loaded
// with, and drains its events into the outbox in the same transaction.
//
// `WHERE version = $expected` with zero rows affected is the whole mechanism. The losing writer
// is told it lost rather than silently overwriting the winner — and it is told with a conflict
// the caller must resolve by reloading, never by retrying blindly, because a blind retry of a
// money command can re-apply an effect that already happened (R-CC-5).
//
// Note what is NOT written here: amount, currency, merchant, tenant and created_at. They are
// immutable (I4) and the UPDATE simply does not name them, so a bug in this file cannot change
// them. The BEFORE UPDATE trigger in migration 0013 is the second line, for the paths that do
// not come through here at all.
func (r *PaymentRepository) Save(ctx context.Context, p *payment.Payment) error {
	if err := requireOwner(ctx, r.tenant, p.TenantID()); err != nil {
		return err
	}
	const q = `
UPDATE pp.payments SET
    state                   = $4,
    capture_method          = $5,
    authorized_amount       = $6,
    captured_amount         = $7,
    refunded_amount         = $8,
    selected_gateway        = $9,
    routing_plan_id         = $10,
    current_attempt_id      = $11,
    description             = $12,
    statement_descriptor    = $13,
    metadata                = $14,
    reconciliation_required = $15,
    version                 = $16,
    updated_at              = $17,
    authorized_at           = $18,
    captured_at             = $19,
    expires_at              = $20
WHERE payment_id = $1 AND partition_month = $2 AND tenant_id = $21 AND version = $3`

	meta, err := json.Marshal(nonNilMetadata(p.Metadata()))
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "postgres: encode payment metadata")
	}

	// The expectation is the version the row holds — what this aggregate was read at, or what it
	// was last written as — and not `version-1`.
	//
	// The difference matters whenever a unit of work makes more than one state change before it
	// saves, which the dispatch path does as a matter of course: it starts an attempt and marks
	// the payment PROCESSING before the first write. Under `version-1` that save expects a
	// version the row never held, fails, and is reported as a concurrency conflict against a
	// writer that does not exist — an I5 violation reported where I5 was never at risk.
	expected := int64(p.BaseVersion())

	tag, err := r.q.Exec(ctx, q,
		p.ID().String(), shared.PartitionMonth(p.ID()), expected,
		string(p.State()), string(p.CaptureMethod()),
		authorizedAmountArg(p), p.CapturedAmount().Amount(), p.RefundedAmount().Amount(),
		p.SelectedGateway().String(), p.RoutingPlanID().String(), currentAttemptID(p),
		p.Description(), p.StatementRef(), meta, p.HasUnresolvedAttempt(),
		int64(p.Version()), p.UpdatedAt(), p.AuthorizedAt(), p.CapturedAt(), p.AuthExpiresAt(),
		r.tenant.String(),
	)
	if err != nil {
		return mapError(err, "save payment")
	}
	if tag.RowsAffected() == 0 {
		return apierror.Newf(apierror.CodePaymentAlreadyProcessed,
			"payment %s was modified concurrently; reload and reapply", p.ID()).
			WithDetail(apierror.Detail{
				Code: "VERSION_CONFLICT",
				Message: "another writer advanced this payment past the version you loaded; " +
					"do not retry the command blindly, reload and decide again",
				RuleID: "I5.OPTIMISTIC_CONCURRENCY",
			})
	}

	// The row now holds this aggregate's version, so a second save inside the same unit of work
	// compares against the right thing. Without this the *first* save would succeed and every
	// subsequent one in the same transaction would conflict.
	p.MarkPersisted()

	if err := r.saveChildren(ctx, p); err != nil {
		return err
	}
	return r.drain(ctx, p, "")
}

// saveChildren upserts the aggregate's attempts and refunds.
//
// ON CONFLICT DO UPDATE rather than "delete all and reinsert": deleting an attempt destroys the
// only evidence that a charge may exist at a gateway, which is the entire reason an attempt is a
// first-class row rather than a set of columns on the payment (ADR-012). The DELETE grant is
// revoked on both tables anyway, so the wrong approach would not even run.
func (r *PaymentRepository) saveChildren(ctx context.Context, p *payment.Payment) error {
	for _, a := range p.Attempts() {
		if err := r.upsertAttempt(ctx, a); err != nil {
			return err
		}
	}
	for _, rf := range p.Refunds() {
		if err := r.upsertRefund(ctx, rf); err != nil {
			return err
		}
	}
	return nil
}

// SaveAttempt persists one attempt without rewriting the aggregate.
//
// This exists because the attempt row must be committed *before* the gateway call — that is what
// makes a crash between "decided to call" and "called" leave evidence — and that commit sits
// directly in the payment latency budget. Writing the whole aggregate there would turn a
// one-row insert into a multi-statement transaction on the hot path for no benefit.
func (r *PaymentRepository) SaveAttempt(ctx context.Context, a *payment.Attempt) error {
	if err := requireOwner(ctx, r.tenant, a.TenantID()); err != nil {
		return err
	}
	return r.upsertAttempt(ctx, a)
}

func (r *PaymentRepository) upsertAttempt(ctx context.Context, a *payment.Attempt) error {
	const q = `
INSERT INTO pp.payment_attempts (
    attempt_id, partition_month, payment_id, tenant_id, gateway_id, gateway_connection_id,
    operation, attempt_number, amount, currency, outcome, gateway_idempotency_key,
    gateway_reference, decline_reason_code, decline_is_retryable, network_advice_no_retry,
    normalized_error_code, error_message, raw_status,
    request_sent_at, response_received_at, latency_ms, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)
ON CONFLICT (attempt_id, partition_month) DO UPDATE SET
    -- gateway_connection_id is absent from this list on purpose. It is stamped at insert, before
    -- the gateway call, and it describes the credential that signed a request already sent. A
    -- later upsert carrying a different value would rewrite that evidence, which is the one thing
    -- an attempt row exists not to allow.
    outcome                 = EXCLUDED.outcome,
    gateway_reference       = EXCLUDED.gateway_reference,
    decline_reason_code     = EXCLUDED.decline_reason_code,
    decline_is_retryable    = EXCLUDED.decline_is_retryable,
    network_advice_no_retry = EXCLUDED.network_advice_no_retry,
    normalized_error_code   = EXCLUDED.normalized_error_code,
    error_message           = EXCLUDED.error_message,
    raw_status              = EXCLUDED.raw_status,
    request_sent_at         = EXCLUDED.request_sent_at,
    response_received_at    = EXCLUDED.response_received_at,
    latency_ms              = EXCLUDED.latency_ms,
    updated_at              = EXCLUDED.updated_at`

	// The attempt's partition is its PAYMENT's month, never its own. An attempt created by a
	// delayed capture in September against a payment created in August belongs in August's
	// partition — that is what keeps invariant I3's per-partition index constraining the whole
	// set, and the foreign key refuses any other value.
	month := shared.PartitionMonth(a.PaymentID())

	var declineRetryable *bool
	if a.Outcome() == payment.OutcomeDeclined {
		v := a.DeclineReason().PermitsFailover()
		declineRetryable = &v
	}

	if _, err := r.q.Exec(ctx, q,
		a.ID().String(), month, a.PaymentID().String(), a.TenantID().String(),
		a.GatewayID().String(), a.ConnectionID().String(), string(a.Operation()),
		a.Sequence(), a.Amount().Amount(), string(a.Amount().Currency()),
		string(a.Outcome()), a.IdempotencyKey(), a.GatewayRef(),
		string(a.DeclineReason()), declineRetryable, a.NetworkAdviceNoRetry(),
		a.ErrorCode(), a.ErrorMessage(), a.RawStatus(),
		a.DispatchedAt(), a.ResolvedAt(), a.Latency().Milliseconds(),
		a.CreatedAt(), a.UpdatedAt(),
	); err != nil {
		return mapError(err, "save payment attempt")
	}
	return nil
}

func (r *PaymentRepository) upsertRefund(ctx context.Context, rf *payment.Refund) error {
	const q = `
INSERT INTO pp.refunds (
    refund_id, tenant_id, payment_id, partition_month, amount, currency, reason, status,
    gateway_reference, idempotency_key, failure_code, failure_message,
    created_at, updated_at, submitted_at, settled_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
ON CONFLICT (refund_id) DO UPDATE SET
    status            = EXCLUDED.status,
    gateway_reference = EXCLUDED.gateway_reference,
    failure_code      = EXCLUDED.failure_code,
    failure_message   = EXCLUDED.failure_message,
    updated_at        = EXCLUDED.updated_at,
    submitted_at      = EXCLUDED.submitted_at,
    settled_at        = EXCLUDED.settled_at`

	if _, err := r.q.Exec(ctx, q,
		rf.ID().String(), rf.TenantID().String(), rf.PaymentID().String(),
		shared.PartitionMonth(rf.PaymentID()),
		rf.Amount().Amount(), string(rf.Amount().Currency()),
		string(rf.Reason()), string(rf.Status()),
		rf.GatewayRef(), rf.IdempotencyKey(), rf.FailureCode(), rf.FailureMessage(),
		rf.CreatedAt(), rf.UpdatedAt(), rf.SubmittedAt(), rf.SettledAt(),
	); err != nil {
		return mapError(err, "save refund")
	}
	return nil
}

// drain writes the aggregate's pending events to the outbox and to the payment event log, in the
// same transaction as the state change.
//
// This is the entire mechanism that makes the dual-write problem not arise: the state row and
// the event row commit together or not at all. It is also invariant I5's write side — one event
// log row per state change, keyed on (payment_id, aggregate_version), so a version gap is
// detectable by reading the log rather than by noticing that a consumer never heard about a
// capture.
func (r *PaymentRepository) drain(ctx context.Context, p *payment.Payment, actor string) error {
	events := p.DrainEvents()
	if len(events) == 0 {
		return nil
	}

	msgs := make([]ports.OutboxMessage, 0, len(events))
	now := r.clock.Now()
	for _, e := range events {
		body, err := json.Marshal(struct {
			Type       string         `json:"type"`
			PaymentID  string         `json:"paymentId"`
			MerchantID string         `json:"merchantId"`
			Version    int64          `json:"aggregateVersion"`
			OccurredAt time.Time      `json:"occurredAt"`
			Payload    map[string]any `json:"payload,omitempty"`
		}{
			Type: string(e.Type), PaymentID: e.PaymentID.String(),
			MerchantID: e.MerchantID.String(), Version: int64(e.Version),
			OccurredAt: e.OccurredAt.UTC(), Payload: e.Payload,
		})
		if err != nil {
			return apierror.Wrapf(err, apierror.CodeInternalError,
				"postgres: encode %s payload", e.Type)
		}
		msgs = append(msgs, ports.OutboxMessage{
			ID:            shared.NewEventID(),
			TenantID:      e.TenantID,
			Topic:         e.Type.Topic(),
			Type:          string(e.Type),
			AggregateID:   e.AggregateID(),
			AggregateType: "payment",
			// The partition key is the payment ID, so every event for one payment lands on one
			// Kafka partition and is strictly ordered relative to its siblings. It is also what
			// the outbox relay's shard bucket is derived from, so one payment's events are
			// claimed by one relay replica and cannot be reordered by scaling out.
			PartitionKey: e.PaymentID.String(),
			Payload:      body,
			Headers:      map[string]string{"correlationId": e.Correlation},
			OccurredAt:   e.OccurredAt,
			AvailableAt:  now,
		})

		if err := r.appendEventLog(ctx, e, actor, msgs[len(msgs)-1].ID); err != nil {
			return err
		}
	}
	return r.outbox.Append(ctx, msgs...)
}

func (r *PaymentRepository) appendEventLog(
	ctx context.Context, e payment.Event, actor string, eventID shared.EventID,
) error {
	const q = `
INSERT INTO pp.payment_event_log (
    tenant_id, payment_id, partition_month, aggregate_version,
    from_state, to_state, trigger, actor, event_id, occurred_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`

	toState, _ := e.Payload["state"].(string)
	if _, err := r.q.Exec(ctx, q,
		e.TenantID.String(), e.PaymentID.String(), shared.PartitionMonth(e.PaymentID),
		int64(e.Version), "", toState, string(e.Type), actor, eventID.String(), e.OccurredAt,
	); err != nil {
		return mapError(err, "append payment event log")
	}
	return nil
}

// List returns a tenant-scoped, cursor-paginated page ordered newest first.
//
// It fetches limit+1 rows and discards the extra. That is how the response knows whether a next
// cursor exists without a second COUNT query — and a COUNT over a merchant's payments is a scan
// this endpoint cannot afford.
func (r *PaymentRepository) List(
	ctx context.Context, f ports.PaymentFilter, page ports.Page,
) ([]*payment.Payment, string, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, "", err
	}
	cur, err := DecodeCursor(page.Cursor)
	if err != nil {
		return nil, "", err
	}
	limit := pageLimit(page.Limit)

	c := newCond(r.tenant.String())
	c.raw("p.tenant_id = $1")
	if !f.MerchantID.IsZero() {
		c.eq("p.merchant_id", f.MerchantID.String())
	}
	if len(f.States) > 0 {
		states := make([]string, 0, len(f.States))
		for _, s := range f.States {
			states = append(states, string(s))
		}
		c.inStrings("p.state", states)
	}
	if f.Currency != "" {
		c.eq("p.currency", string(f.Currency))
	}
	if !f.GatewayID.IsZero() {
		c.eq("p.selected_gateway", f.GatewayID.String())
	}
	if f.CreatedAfter != nil {
		c.gte("p.created_at", f.CreatedAfter.UTC())
		// The partition key is a pure function of created_at, so a time filter is also a
		// partition filter. Stating it explicitly is what turns "scan every partition and
		// discard" into "open the two partitions that can contain a match".
		c.gte("p.partition_month", monthOf(*f.CreatedAfter))
	}
	if f.CreatedBefore != nil {
		c.lte("p.created_at", f.CreatedBefore.UTC())
		c.lte("p.partition_month", monthOf(*f.CreatedBefore))
	}
	if f.AmountMin != nil {
		c.gte("p.amount", f.AmountMin.Amount())
	}
	if f.AmountMax != nil {
		c.lte("p.amount", f.AmountMax.Amount())
	}
	c.keysetBefore("p.created_at", "p.payment_id", cur)

	q := selectPaymentList + c.where() +
		" ORDER BY p.created_at DESC, p.payment_id DESC LIMIT " + c.limitPlaceholder()

	rows, err := r.q.Query(ctx, q, c.argsWith(limit+1)...)
	if err != nil {
		return nil, "", mapError(err, "list payments")
	}
	defer rows.Close()

	out := make([]*payment.Payment, 0, limit)
	for rows.Next() {
		p, err := scanPaymentAggregate(rows)
		if err != nil {
			return nil, "", mapError(err, "list payments")
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, "", mapError(err, "list payments")
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = EncodeCursor(Cursor{Time: last.CreatedAt(), ID: last.ID().String()})
	}
	return out, next, nil
}

const selectPaymentList = `
SELECT p.payment_id, p.partition_month, p.tenant_id, p.merchant_id, p.state, p.amount, p.currency,
       p.payment_method, p.capture_method,
       p.method_token, p.method_brand, p.method_last4, p.method_exp_month, p.method_exp_year,
       p.method_country, p.method_network_token,
       p.authorized_amount, p.captured_amount, p.refunded_amount,
       p.selected_gateway, p.routing_plan_id, p.current_attempt_id,
       p.risk_decision, p.three_ds_status, p.description, p.statement_descriptor, p.metadata,
       p.customer_ref, p.customer_email_hash, p.customer_ip, p.customer_country,
       p.idempotency_key, p.correlation_id, p.reconciliation_required,
       p.version, p.created_at, p.updated_at, p.authorized_at, p.captured_at, p.expires_at,` +
	childJSON + `
FROM pp.payments p`

// FindUnresolved returns payments carrying an attempt whose outcome is TIMEOUT_UNKNOWN and which
// has been unresolved for longer than olderThan.
//
// This is the reconciler's work queue, and it is the reason a gateway timeout is survivable
// rather than a coin flip. A TIMEOUT_UNKNOWN attempt is never retried and never treated as a
// failure: the payment stays in PROCESSING with reconciliation_required set, and this query is
// what eventually resolves it by asking the gateway what actually happened.
func (r *PaymentRepository) FindUnresolved(
	ctx context.Context, olderThan time.Duration, limit int,
) ([]*payment.Payment, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	q := selectPaymentList + `
WHERE p.tenant_id = $1
  AND p.reconciliation_required
  AND p.updated_at < $2
ORDER BY p.created_at ASC
LIMIT $3`
	return r.queryList(ctx, "find unresolved payments", q,
		r.tenant.String(), r.clock.Now().Add(-olderThan), pageLimit(limit))
}

// FindExpiredAuthorizations returns authorized payments past their expiry, for the sweeper that
// moves them to EXPIRED.
//
// The sweeper exists so that the platform expires an authorization before the gateway silently
// does. An authorization the gateway has released but which we still believe is live produces a
// capture that fails at the worst possible moment — after the merchant has shipped.
func (r *PaymentRepository) FindExpiredAuthorizations(
	ctx context.Context, now time.Time, limit int,
) ([]*payment.Payment, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	q := selectPaymentList + `
WHERE p.tenant_id = $1
  AND p.state = 'AUTHORIZED'
  AND p.expires_at IS NOT NULL
  AND p.expires_at <= $2
ORDER BY p.expires_at ASC
LIMIT $3`
	return r.queryList(ctx, "find expired authorizations", q,
		r.tenant.String(), now.UTC(), pageLimit(limit))
}

// CountOpen returns the number of payments in a non-terminal state for a merchant.
//
// It backs the merchant-termination guard: a merchant may not be terminated while money is in
// flight, because terminating one leaves those payments with nobody to settle to. The literal
// state list matches the partial index idx_payments_state_open exactly — a bind parameter here
// would not, and the query would degrade from an index scan over a tiny partial index to a scan
// of every payment the merchant has ever made.
func (r *PaymentRepository) CountOpen(ctx context.Context, merchantID shared.MerchantID) (int, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return 0, err
	}
	const q = `
SELECT count(*) FROM pp.payments
WHERE tenant_id = $1 AND merchant_id = $2
  AND state IN ('CREATED','PROCESSING','PENDING','REQUIRES_ACTION','AUTHORIZED')`
	var n int
	if err := r.q.QueryRow(ctx, q, r.tenant.String(), merchantID.String()).Scan(&n); err != nil {
		return 0, mapError(err, "count open payments")
	}
	return n, nil
}

func (r *PaymentRepository) queryList(
	ctx context.Context, op, q string, args ...any,
) ([]*payment.Payment, error) {
	rows, err := r.q.Query(ctx, q, args...)
	if err != nil {
		return nil, mapError(err, op)
	}
	defer rows.Close()
	var out []*payment.Payment
	for rows.Next() {
		p, err := scanPaymentAggregate(rows)
		if err != nil {
			return nil, mapError(err, op)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, op)
	}
	return out, nil
}

// --- scanning ----------------------------------------------------------------------------------

// scanRow is satisfied by both pgx.Row and pgx.Rows, so one scanner serves the by-ID path and
// the listing path. Two scanners over forty columns would drift, and the drift would be a
// column read into the wrong field on exactly one of the two paths.
type scanRow interface{ Scan(dest ...any) error }

func scanPaymentAggregate(row scanRow) (*payment.Payment, error) {
	var (
		p              payment.RehydrateParams
		id, tenantID   string
		merchantID     string
		partitionMonth time.Time
		state, ccy     string
		method, capMth string
		amount         int64
		authAmt        *int64
		capAmt, refAmt int64
		gateway, plan  string
		currentAttempt string
		risk, threeDS  string
		metaRaw        []byte
		methodCountry  string
		custCountry    string
		version        int64
		attemptsRaw    []byte
		refundsRaw     []byte
		mref           payment.PaymentMethodReference
		cust           payment.CustomerReference
		reconRequired  bool
	)

	if err := row.Scan(
		&id, &partitionMonth, &tenantID, &merchantID, &state, &amount, &ccy,
		&method, &capMth,
		&mref.Token, &mref.Brand, &mref.Last4, &mref.ExpMonth, &mref.ExpYear,
		&methodCountry, &mref.NetworkToken,
		&authAmt, &capAmt, &refAmt,
		&gateway, &plan, &currentAttempt,
		&risk, &threeDS, &p.Description, &p.StatementRef, &metaRaw,
		&cust.MerchantCustomerID, &cust.EmailHash, &cust.IPAddress, &custCountry,
		&p.IdempotencyKey, &p.CorrelationID, &reconRequired,
		&version, &p.CreatedAt, &p.UpdatedAt, &p.AuthorizedAt, &p.CapturedAt, &p.AuthExpiresAt,
		&attemptsRaw, &refundsRaw,
	); err != nil {
		return nil, err
	}

	currency := money.Currency(ccy)
	if !currency.IsSupported() {
		// A currency this binary does not know means the row was written by a newer version, or
		// the supported set was narrowed under it. Either way, rehydrating would produce an
		// aggregate whose arithmetic is wrong by an order of magnitude for a three-decimal
		// currency, so it fails loudly instead.
		return nil, apierror.Newf(apierror.CodeInternalError,
			"payment %s is denominated in unsupported currency %q", id, ccy)
	}

	mref.Country = shared.Country(methodCountry)
	cust.Country = shared.Country(custCountry)

	p.ID = shared.PaymentID(id)
	p.TenantID = shared.TenantID(tenantID)
	p.MerchantID = shared.MerchantID(merchantID)
	p.Amount = money.MustNew(amount, currency)
	p.CaptureMethod = payment.CaptureMethod(capMth)
	p.PaymentMethod = shared.PaymentMethod(method)
	p.MethodRef = mref
	p.State = payment.State(state)
	p.Version = shared.Version(version)
	p.CapturedAmount = money.MustNew(capAmt, currency)
	p.RefundedAmount = money.MustNew(refAmt, currency)
	p.AuthorizedAmount = money.Zero(currency)
	if authAmt != nil {
		p.AuthorizedAmount = money.MustNew(*authAmt, currency)
	}
	p.SelectedGateway = shared.GatewayID(gateway)
	p.RoutingPlanID = shared.PlanID(plan)
	p.Customer = cust

	if len(metaRaw) > 0 {
		if err := json.Unmarshal(metaRaw, &p.Metadata); err != nil {
			return nil, apierror.Wrapf(err, apierror.CodeInternalError,
				"payment %s has unreadable metadata", id)
		}
	}

	attempts, err := decodeAttempts(attemptsRaw, p.ID, p.TenantID, currency)
	if err != nil {
		return nil, err
	}
	refunds, err := decodeRefunds(refundsRaw, p.ID, p.TenantID)
	if err != nil {
		return nil, err
	}
	p.Attempts = attempts
	p.Refunds = refunds

	return payment.Rehydrate(p)
}

// attemptJSON mirrors the jsonb_build_object in childJSON. The two are a contract: a field
// renamed in one and not the other silently becomes a zero value, which for `outcome` would mean
// an attempt rehydrating as PENDING when it actually succeeded.
type attemptJSON struct {
	ID            string `json:"id"`
	GatewayID     string `json:"gateway_id"`
	ConnectionID  string `json:"connection_id"`
	Operation     string `json:"operation"`
	Sequence      int    `json:"sequence"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Outcome       string `json:"outcome"`
	GatewayRef    string `json:"gateway_ref"`
	IdemKey       string `json:"idem_key"`
	DeclineReason string `json:"decline_reason"`
	ErrorCode     string `json:"error_code"`
	ErrorMessage  string `json:"error_message"`
	RawStatus     string `json:"raw_status"`
	NoRetry       bool   `json:"no_retry"`
	DispatchedAt  *int64 `json:"dispatched_at"`
	ResolvedAt    *int64 `json:"resolved_at"`
	LatencyMS     int64  `json:"latency_ms"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

func decodeAttempts(
	raw []byte, pid shared.PaymentID, tid shared.TenantID, fallback money.Currency,
) ([]*payment.Attempt, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var rows []attemptJSON
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, apierror.Wrapf(err, apierror.CodeInternalError,
			"payment %s has unreadable attempts", pid)
	}
	out := make([]*payment.Attempt, 0, len(rows))
	for _, a := range rows {
		ccy := money.Currency(a.Currency)
		if !ccy.IsSupported() {
			ccy = fallback
		}
		att, err := payment.RehydrateAttempt(payment.RehydrateAttemptParams{
			ID:                   shared.AttemptID(a.ID),
			PaymentID:            pid,
			TenantID:             tid,
			GatewayID:            shared.GatewayID(a.GatewayID),
			ConnectionID:         shared.ConnectionID(a.ConnectionID),
			Operation:            shared.Operation(a.Operation),
			Sequence:             a.Sequence,
			Amount:               money.MustNew(a.Amount, ccy),
			Outcome:              payment.AttemptOutcome(a.Outcome),
			GatewayRef:           a.GatewayRef,
			IdempotencyKey:       a.IdemKey,
			DeclineReason:        payment.DeclineReason(a.DeclineReason),
			ErrorCode:            a.ErrorCode,
			ErrorMessage:         a.ErrorMessage,
			RawStatus:            a.RawStatus,
			NetworkAdviceNoRetry: a.NoRetry,
			DispatchedAt:         microsToTimePtr(a.DispatchedAt),
			ResolvedAt:           microsToTimePtr(a.ResolvedAt),
			Latency:              time.Duration(a.LatencyMS) * time.Millisecond,
			CreatedAt:            time.UnixMicro(a.CreatedAt).UTC(),
			UpdatedAt:            time.UnixMicro(a.UpdatedAt).UTC(),
		})
		if err != nil {
			return nil, err
		}
		out = append(out, att)
	}
	return out, nil
}

type refundJSON struct {
	ID             string `json:"id"`
	Amount         int64  `json:"amount"`
	Currency       string `json:"currency"`
	Reason         string `json:"reason"`
	Status         string `json:"status"`
	GatewayRef     string `json:"gateway_ref"`
	IdemKey        string `json:"idem_key"`
	FailureCode    string `json:"failure_code"`
	FailureMessage string `json:"failure_message"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
	SubmittedAt    *int64 `json:"submitted_at"`
	SettledAt      *int64 `json:"settled_at"`
}

func decodeRefunds(raw []byte, pid shared.PaymentID, tid shared.TenantID) ([]*payment.Refund, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var rows []refundJSON
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, apierror.Wrapf(err, apierror.CodeInternalError,
			"payment %s has unreadable refunds", pid)
	}
	out := make([]*payment.Refund, 0, len(rows))
	for _, rf := range rows {
		ccy := money.Currency(rf.Currency)
		if !ccy.IsSupported() {
			return nil, apierror.Newf(apierror.CodeInternalError,
				"refund %s is denominated in unsupported currency %q", rf.ID, rf.Currency)
		}
		r, err := payment.RehydrateRefund(payment.RehydrateRefundParams{
			ID:             shared.RefundID(rf.ID),
			PaymentID:      pid,
			TenantID:       tid,
			Amount:         money.MustNew(rf.Amount, ccy),
			Reason:         payment.RefundReason(rf.Reason),
			Status:         payment.RefundStatus(rf.Status),
			GatewayRef:     rf.GatewayRef,
			IdempotencyKey: rf.IdemKey,
			FailureCode:    rf.FailureCode,
			FailureMessage: rf.FailureMessage,
			CreatedAt:      time.UnixMicro(rf.CreatedAt).UTC(),
			UpdatedAt:      time.UnixMicro(rf.UpdatedAt).UTC(),
			SubmittedAt:    microsToTimePtr(rf.SubmittedAt),
			SettledAt:      microsToTimePtr(rf.SettledAt),
		})
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// --- small helpers -----------------------------------------------------------------------------

func microsToTimePtr(v *int64) *time.Time {
	if v == nil {
		return nil
	}
	t := time.UnixMicro(*v).UTC()
	return &t
}

func monthOf(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func nonNilMetadata(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// authorizedAmountArg maps the aggregate's authorized amount onto a nullable column.
//
// The domain carries a zero Money for "never authorized" because a Money is always in some
// currency; the database needs NULL there, because invariant I2 is
// `authorized_amount IS NULL OR captured_amount <= authorized_amount` and writing zero instead
// would make every auto-captured payment violate I2 the instant it captured.
func authorizedAmountArg(p *payment.Payment) *int64 {
	if p.AuthorizedAt() == nil {
		return nil
	}
	v := p.AuthorizedAmount().Amount()
	return &v
}

// currentAttemptID returns the identifier of the payment's most recent attempt, or the empty
// string. It is denormalized onto the payment row so that an operator dashboard can show "which
// gateway is this stuck payment sitting at" without joining to the attempts table.
func currentAttemptID(p *payment.Payment) string {
	if a := p.LatestAttempt(); a != nil {
		return a.ID().String()
	}
	return ""
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: NFR-15.
//
// Partition-pruned payment access: the identifier carries the partition key, so a read touches
// one month rather than every month
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
