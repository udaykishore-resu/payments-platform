package testenv

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// The fixtures below write rows with raw SQL rather than through the repositories.
//
// That is the point of them. Every negative test in tests/integration asserts that the *database*
// refuses something with the domain out of the way; a fixture that went through the repository
// would be asserting that the repository refuses it, which the unit tests already cover and which
// says nothing about what happens the day a new call site forgets the guard.

// Payment is a row in pp.payments.
type Payment struct {
	ID             string
	PartitionMonth time.Time
	TenantID       string
	MerchantID     string
	State          string
	Amount         int64
	Currency       string

	AuthorizedAmount *int64
	CapturedAmount   int64
	RefundedAmount   int64

	CreatedAt    time.Time
	AuthorizedAt *time.Time
	Version      int64
}

// NewPayment builds a valid CREATED payment anchored to this scope's clock. The partition month
// is derived from the same instant the id's ULID timestamp carries, which is the schema's
// payments_partition_matches_created_at constraint restated in Go — a fixture that got this wrong
// would fail at insert with a confusing message instead of at the assertion it was written for.
func (s *Scope) NewPayment(tenant, merchant, seed string, minor int64) Payment {
	at := s.Clock.Now()
	return Payment{
		ID:             s.IDAt(PrefixPayment, at, seed),
		PartitionMonth: PartitionMonth(at),
		TenantID:       tenant,
		MerchantID:     merchant,
		State:          "CREATED",
		Amount:         minor,
		Currency:       "USD",
		CreatedAt:      at,
	}
}

// Insert writes the payment. The token is deliberately a non-numeric string: pp.payments carries
// a CHECK that rejects a 13-19 digit token, the schema-level tripwire behind the L1 PAN detector.
func (p Payment) Insert(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `
INSERT INTO pp.payments (
    payment_id, partition_month, tenant_id, merchant_id, state, amount, currency,
    payment_method, capture_method, method_token, method_brand, method_last4,
    authorized_amount, captured_amount, refunded_amount,
    idempotency_key, version, created_at, updated_at, authorized_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,'CARD','MANUAL','tok_test_visa','visa','4242',
        $8,$9,$10,$11,$12,$13,$13,$14)`,
		p.ID, p.PartitionMonth, p.TenantID, p.MerchantID, p.State, p.Amount, p.Currency,
		p.AuthorizedAmount, p.CapturedAmount, p.RefundedAmount,
		"idem-"+p.ID, p.Version, p.CreatedAt, p.AuthorizedAt)
	return err
}

// Attempt is a row in pp.payment_attempts.
type Attempt struct {
	ID             string
	PartitionMonth time.Time
	PaymentID      string
	TenantID       string
	GatewayID      string
	Operation      string
	Number         int
	Amount         int64
	Currency       string
	Outcome        string
	IdempotencyKey string
	Retryable      *bool
	CreatedAt      time.Time
	RequestSentAt  *time.Time
}

// NewAttempt builds an attempt in the same partition as its payment. Sharing the payment's
// partition is amendment A-02 and is what makes invariant I3's per-partition unique index
// constrain the whole set rather than one month of it.
func (s *Scope) NewAttempt(p Payment, seed string, number int, outcome string) Attempt {
	at := s.Clock.Now()
	sent := at
	return Attempt{
		ID:             s.IDAt(PrefixAttempt, at, seed),
		PartitionMonth: p.PartitionMonth,
		PaymentID:      p.ID,
		TenantID:       p.TenantID,
		GatewayID:      "simulator",
		Operation:      "authorize",
		Number:         number,
		Amount:         p.Amount,
		Currency:       p.Currency,
		Outcome:        outcome,
		IdempotencyKey: "gwk-" + seed + "-" + s.nonce[:8],
		CreatedAt:      at,
		RequestSentAt:  &sent,
	}
}

// Insert writes the attempt.
func (a Attempt) Insert(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `
INSERT INTO pp.payment_attempts (
    attempt_id, partition_month, payment_id, tenant_id, gateway_id, operation,
    attempt_number, amount, currency, outcome, gateway_idempotency_key,
    decline_is_retryable, request_sent_at, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$14)`,
		a.ID, a.PartitionMonth, a.PaymentID, a.TenantID, a.GatewayID, a.Operation,
		a.Number, a.Amount, a.Currency, a.Outcome, a.IdempotencyKey,
		a.Retryable, a.RequestSentAt, a.CreatedAt)
	return err
}

// InsertLedgerPair writes a balanced debit/credit pair for a payment.
//
// A pair rather than a single entry, always: the ledger's only real invariant is that every
// transaction group balances, and a helper that could write one side of a transaction would make
// it possible to build a fixture the reconciler would correctly flag as broken.
func (s *Scope) InsertLedgerPair(ctx context.Context, tx pgx.Tx, p Payment, group, entryType string, amount int64) error {
	at := s.Clock.Now()
	month := PartitionMonth(at)
	rows := []struct {
		account string
		side    string
		seed    string
	}{
		{"GATEWAY_CLEARING", "DEBIT", group + "/dr"},
		{"MERCHANT_RECEIVABLE", "CREDIT", group + "/cr"},
	}
	for _, r := range rows {
		_, err := tx.Exec(ctx, `
INSERT INTO pp.ledger_entries (
    entry_id, partition_month, tenant_id, merchant_id, account_type, side, amount, currency,
    entry_type, transaction_group_id, payment_id, occurred_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			s.IDAt(PrefixLedger, at, r.seed), month, p.TenantID, p.MerchantID,
			r.account, r.side, amount, p.Currency, entryType, group, p.ID, at)
		if err != nil {
			return err
		}
	}
	return nil
}

// InsertOutbox appends one outbox row.
func (s *Scope) InsertOutbox(ctx context.Context, tx pgx.Tx, tenant, eventType, topic, partitionKey, aggregateID, seed string) (string, error) {
	at := s.Clock.Now()
	id := s.IDAt(PrefixEvent, at, seed)
	_, err := tx.Exec(ctx, `
INSERT INTO pp.outbox_events (
    event_id, tenant_id, aggregate_type, aggregate_id, event_type, topic, partition_key,
    payload, occurred_at, available_at)
VALUES ($1,$2,'payment',$3,$4,$5,$6,$7,$8, now())`,
		id, tenant, aggregateID, eventType, topic, partitionKey, []byte(`{"seed":"`+seed+`"}`), at)
	return id, err
}

// LedgerBalanced reports whether every transaction group touching a payment has equal debits and
// credits. It is the ledger's one true invariant and the cheapest thing to check after any money
// movement, so both the integration suite and the chaos steady-state hypothesis use it.
func LedgerBalanced(ctx context.Context, tx pgx.Tx, paymentID string) (bool, error) {
	var unbalanced int64
	err := tx.QueryRow(ctx, `
SELECT count(*) FROM (
    SELECT transaction_group_id
    FROM pp.ledger_entries
    WHERE payment_id = $1
    GROUP BY transaction_group_id
    HAVING sum(CASE WHEN side = 'DEBIT' THEN amount ELSE -amount END) <> 0
) g`, paymentID).Scan(&unbalanced)
	if err != nil {
		return false, err
	}
	return unbalanced == 0, nil
}

// CountRows is the small query every assertion in the integration suite ends up making.
func CountRows(ctx context.Context, tx pgx.Tx, query string, args ...any) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, query, args...).Scan(&n)
	return n, err
}
