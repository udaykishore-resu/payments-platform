package domain

import "time"

type PaymentStatus string

const (
	PaymentStatusPending   PaymentStatus = "pending"
	PaymentStatusCompleted PaymentStatus = "completed"
	PaymentStatusFailed    PaymentStatus = "failed"
	PaymentStatusReversed  PaymentStatus = "reversed"
)

type AccountStatus string

const (
	AccountStatusActive AccountStatus = "active"
	AccountStatusFrozen AccountStatus = "frozen"
	AccountStatusClosed AccountStatus = "closed"
)

// Account is a ledger account — the source or destination of a payment. Balance is never stored
// as a mutable column; it is always derived as SUM(ledger_entries.amount_minor) for the account,
// which is the only representation that can't drift out of sync with the entries that are
// supposed to justify it. (For hot accounts this is optimized with a materialized/cached balance
// refreshed transactionally — see repository layer — but the ledger entries remain the source of
// truth.)
type Account struct {
	ID       string
	Currency string
	Status   AccountStatus
}

// LedgerEntry is one leg of a double-entry posting. A completed Payment always produces exactly
// two LedgerEntry rows whose AmountMinor sums to zero — one negative (debit, funds leaving the
// source account) and one positive (credit, funds arriving at the destination account).
type LedgerEntry struct {
	ID          string
	PaymentID   string
	AccountID   string
	AmountMinor int64
	Currency    string
	CreatedAt   time.Time
}

// Payment is the aggregate root for a single money-movement request.
type Payment struct {
	ID              string
	IdempotencyKey  string
	SourceAccountID string
	DestAccountID   string
	Amount          Money
	Status          PaymentStatus
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// CreatePaymentInput is the validated application-layer command to create a payment.
type CreatePaymentInput struct {
	IdempotencyKey  string
	RequestHash     string // hash of the full request body, used to detect idempotency-key reuse with a different body
	SourceAccountID string
	DestAccountID   string
	Amount          Money
	ClientID        string
}

func (in CreatePaymentInput) Validate() error {
	if in.IdempotencyKey == "" {
		return ErrInvalidRequest
	}
	if in.SourceAccountID == "" || in.DestAccountID == "" {
		return ErrInvalidRequest
	}
	if in.SourceAccountID == in.DestAccountID {
		return ErrInvalidRequest
	}
	return in.Amount.Validate()
}

// OutboxEvent is a durable record of a fact that must eventually be published to downstream
// consumers (see docs/adr/ADR-004-idempotency-ledger.md for why this exists as a table rather
// than a direct publish call).
type OutboxEvent struct {
	ID          string
	AggregateID string // payment ID
	EventType   string // e.g. "payment.completed"
	Payload     []byte // JSON envelope, includes trace context for cross-boundary correlation
	Attempts    int
}
