// Package l7domain is validation level 7: the transitions and aggregate invariants that guard
// a state change at the moment it is written.
//
// L7 is the last line, and it is deliberately redundant with L5. A bug in L5 must still not be
// able to move money twice. Each rule here is mirrored by a database constraint — a CHECK, a
// trigger, or in the case of the anti-double-charge invariant a partial unique index — and the
// division of labour is exact: the rule is the fast, explanatory check that tells a caller
// what is wrong, and the constraint is the one that is still true when the rule has a bug.
//
// Nothing in this package restates a transition table. The payment machine, the attempt
// machine and the merchant machine live in internal/domain and are *asked*, never copied. A
// second copy of a transition table is not a safety net; it is a second definition that
// diverges on the day someone adds a state to one of them.
//
// Mode is ShortCircuit: once the transition itself is illegal, the invariant checks that
// assume it are answering a question about a state the aggregate will never be in.
//
// See docs/validation-plane.md §3.7 and baseline §9 invariants I1–I5.
package l7domain

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// AggregateKind selects which aggregate a command targets, and therefore which rules apply.
type AggregateKind string

// The aggregate kinds L7 guards.
const (
	KindPayment  AggregateKind = "PAYMENT"
	KindAttempt  AggregateKind = "ATTEMPT"
	KindMerchant AggregateKind = "MERCHANT"
	KindLedger   AggregateKind = "LEDGER"
)

// Command is the state change being attempted.
type Command struct {
	Kind AggregateKind
	// Operation names the money-moving operation, where the command has one.
	Operation shared.Operation
	// ExpectedVersion is the optimistic-concurrency token the caller read with (invariant I5).
	ExpectedVersion shared.Version

	// TargetPaymentState, TargetAttemptOutcome and TargetMerchantStatus are the requested
	// destinations; the zero value means "this command does not move that machine".
	TargetPaymentState   payment.State
	TargetAttemptOutcome payment.AttemptOutcome
	TargetMerchantStatus merchant.Status

	// Amount is the capture or refund amount, where the command has one.
	Amount money.Money

	// ChangedImmutableFields names any of amount, currency, merchant or tenant that the command
	// would alter (invariant I4). Populated by the repository's change detection.
	ChangedImmutableFields []string

	// DisputeResolution is set when the command resolves a dispute.
	DisputeResolution *DisputeResolution
}

// DisputeResolution is the outcome of a chargeback and the state the payment held before it.
type DisputeResolution struct {
	Won bool
	// PriorState is the state the payment was in when the dispute was raised, which is where a
	// won dispute must return it.
	PriorState payment.State
}

// PaymentAggregate is the payment as loaded, plus the derived facts the invariants need.
type PaymentAggregate struct {
	Present          bool
	ID               shared.PaymentID
	TenantID         shared.TenantID
	MerchantID       shared.MerchantID
	State            payment.State
	Version          shared.Version
	AuthorizedAmount money.Money
	CapturedTotal    money.Money
	RefundedTotal    money.Money
	// SuccessfulAttempts counts attempts already in a successful terminal state (invariant I3).
	SuccessfulAttempts int
	// TwoStepFlow marks a payment that authorizes and captures separately, which is the only
	// shape in which the captured-vs-authorized invariant is meaningful.
	TwoStepFlow bool
}

// AttemptView is the attempt a command targets.
type AttemptView struct {
	Present    bool
	ID         shared.AttemptID
	PaymentID  shared.PaymentID
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	Outcome    payment.AttemptOutcome
	Amount     money.Money
}

// MerchantAggregate is the merchant as loaded, plus the activation guards.
type MerchantAggregate struct {
	Present bool
	ID      shared.MerchantID
	Status  merchant.Status
	Version shared.Version

	CertifiedConnections int
	// HasValidPublishedConfig records that a non-empty, L4-valid configuration is published.
	HasValidPublishedConfig    bool
	ComplianceAttestationDone  bool
	OpenCriticalReconciliation int
	// NonTerminalPayments is what blocks termination.
	NonTerminalPayments int
}

// LedgerEntry is one line of a double-entry posting.
type LedgerEntry struct {
	AccountID string
	// DebitMinor and CreditMinor are the two halves; exactly one is non-zero on a well-formed
	// entry.
	DebitMinor  int64
	CreditMinor int64
	Currency    money.Currency
}

// LedgerWrite is a group of entries being appended, plus what the write actually does.
type LedgerWrite struct {
	Present bool
	Entries []LedgerEntry
	// MutatesExistingEntries is true when the write would UPDATE or DELETE, which the ledger
	// never permits: a correction is a reversing entry, not an edit.
	MutatesExistingEntries bool
}

// UnitOfWork records what the repository is about to write alongside the state change. These
// are the two structural guarantees the outbox pattern depends on, and they are checked as
// rules so that a violation is reported as a rule ID rather than discovered as a missing
// event three days later.
type UnitOfWork struct {
	StateChanged        bool
	EventsAppended      int
	VersionIncrementBy  int64
	OutboxWritesQueued  int
	OutboxInSameTxn     bool
	ExpectedOutboxWrite bool
}

// MoneyOperands is a pair of amounts a command combines, for the currency-consistency rule.
type MoneyOperands struct {
	Present bool
	A       money.Money
	B       money.Money
	Label   string
}

// Subject is everything L7 evaluates.
type Subject struct {
	Command    Command
	Payment    PaymentAggregate
	Attempt    AttemptView
	Merchant   MerchantAggregate
	Ledger     LedgerWrite
	UnitOfWork UnitOfWork
	Operands   MoneyOperands
	// Now is the injected clock reading.
	Now time.Time
}

// Deps is empty by construction, and that is the point.
//
// L7 has no configuration: what a transition table permits is not a tenant setting, and an
// invariant that could be relaxed per merchant is not an invariant. The type exists so the
// level's constructor has the same shape as the other six, and so that a future rule that
// genuinely needs a parameter has somewhere to put it without changing every call site.
type Deps struct{}

// DefaultDeps returns the (empty) dependencies.
func DefaultDeps() Deps { return Deps{} }
