package payment

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// RefundStatus is the lifecycle of one refund.
//
// A refund is asynchronous at every gateway worth integrating: the gateway accepts the
// instruction and settles it hours or days later. Modelling it as a boolean on the payment
// ("refunded: true") loses the window in which the merchant has promised the customer their
// money back and the money has not moved, which is precisely the window support tickets are
// about.
type RefundStatus string

const (
	// RefundPending means the refund exists in our system but has not been sent.
	RefundPending RefundStatus = "PENDING"
	// RefundSubmitted means the gateway accepted the instruction.
	RefundSubmitted RefundStatus = "SUBMITTED"
	// RefundSucceeded means the gateway confirmed the money moved.
	RefundSucceeded RefundStatus = "SUCCEEDED"
	// RefundFailed means the gateway refused or the refund could not be completed. The amount
	// returns to the refundable balance.
	RefundFailed RefundStatus = "FAILED"
	// RefundCanceled means the refund was withdrawn before submission.
	RefundCanceled RefundStatus = "CANCELED"
)

// AllRefundStatuses is the complete universe.
var AllRefundStatuses = []RefundStatus{
	RefundPending, RefundSubmitted, RefundSucceeded, RefundFailed, RefundCanceled,
}

var refundMachine = shared.NewStateMachine("refund", RefundPending,
	AllRefundStatuses,
	[]RefundStatus{RefundSucceeded, RefundFailed, RefundCanceled},
	[]shared.Transition[RefundStatus]{
		{From: RefundPending, To: RefundSubmitted},
		{From: RefundPending, To: RefundCanceled},
		{From: RefundPending, To: RefundFailed},
		{From: RefundSubmitted, To: RefundSucceeded},
		{From: RefundSubmitted, To: RefundFailed},
	})

// RefundMachine exposes the refund state machine for the validation plane and the tests.
func RefundMachine() *shared.StateMachine[RefundStatus] { return refundMachine }

// String satisfies fmt.Stringer.
func (s RefundStatus) String() string { return string(s) }

// IsTerminal reports whether the refund can still change.
func (s RefundStatus) IsTerminal() bool { return refundMachine.IsTerminal(s) }

// RefundReason is the normalized reason a refund was issued. It matters beyond bookkeeping:
// schemes and gateways treat a fraud-driven refund differently from a customer-request refund,
// and dispute defence depends on being able to show why money was returned.
type RefundReason string

const (
	RefundReasonRequestedByCustomer RefundReason = "REQUESTED_BY_CUSTOMER"
	RefundReasonDuplicate           RefundReason = "DUPLICATE"
	RefundReasonFraudulent          RefundReason = "FRAUDULENT"
	RefundReasonProductUnavailable  RefundReason = "PRODUCT_UNAVAILABLE"
	RefundReasonServiceNotProvided  RefundReason = "SERVICE_NOT_PROVIDED"
	RefundReasonPricingError        RefundReason = "PRICING_ERROR"
	RefundReasonDisputeConceded     RefundReason = "DISPUTE_CONCEDED"
	RefundReasonOther               RefundReason = "OTHER"
)

var refundReasons = map[RefundReason]struct{}{
	RefundReasonRequestedByCustomer: {}, RefundReasonDuplicate: {}, RefundReasonFraudulent: {},
	RefundReasonProductUnavailable: {}, RefundReasonServiceNotProvided: {},
	RefundReasonPricingError: {}, RefundReasonDisputeConceded: {}, RefundReasonOther: {},
}

// IsValid reports whether r is a known reason.
func (r RefundReason) IsValid() bool { _, ok := refundReasons[r]; return ok }

// String satisfies fmt.Stringer.
func (r RefundReason) String() string { return string(r) }

// Refund is one return of funds against a captured payment. It is part of the Payment
// aggregate: it is created through Payment.AddRefund, which is where invariant I1 is enforced,
// and it is never loaded or written independently of its payment.
type Refund struct {
	id             shared.RefundID
	paymentID      shared.PaymentID
	tenantID       shared.TenantID
	amount         money.Money
	reason         RefundReason
	status         RefundStatus
	gatewayRef     string
	idempotencyKey string
	failureCode    string
	failureMessage string
	createdAt      time.Time
	updatedAt      time.Time
	submittedAt    *time.Time
	settledAt      *time.Time
}

func newRefund(paymentID shared.PaymentID, tenantID shared.TenantID, amount money.Money,
	reason RefundReason, idempotencyKey string, now time.Time) *Refund {
	if !reason.IsValid() {
		reason = RefundReasonOther
	}
	return &Refund{
		id:             shared.NewRefundID(),
		paymentID:      paymentID,
		tenantID:       tenantID,
		amount:         amount,
		reason:         reason,
		status:         RefundPending,
		idempotencyKey: idempotencyKey,
		createdAt:      now,
		updatedAt:      now,
	}
}

// Accessors.

func (r *Refund) ID() shared.RefundID         { return r.id }
func (r *Refund) PaymentID() shared.PaymentID { return r.paymentID }
func (r *Refund) TenantID() shared.TenantID   { return r.tenantID }
func (r *Refund) Amount() money.Money         { return r.amount }
func (r *Refund) Reason() RefundReason        { return r.reason }
func (r *Refund) Status() RefundStatus        { return r.status }
func (r *Refund) GatewayRef() string          { return r.gatewayRef }
func (r *Refund) IdempotencyKey() string      { return r.idempotencyKey }
func (r *Refund) FailureCode() string         { return r.failureCode }
func (r *Refund) FailureMessage() string      { return r.failureMessage }
func (r *Refund) CreatedAt() time.Time        { return r.createdAt }
func (r *Refund) UpdatedAt() time.Time        { return r.updatedAt }
func (r *Refund) SubmittedAt() *time.Time     { return r.submittedAt }
func (r *Refund) SettledAt() *time.Time       { return r.settledAt }

// MarkSubmitted records that the gateway accepted the refund instruction.
func (r *Refund) MarkSubmitted(gatewayRef string, now time.Time) error {
	if err := refundMachine.Transition(r.status, RefundSubmitted); err != nil {
		return err
	}
	r.status = RefundSubmitted
	r.gatewayRef = gatewayRef
	r.submittedAt = &now
	r.updatedAt = now
	return nil
}

// markSucceeded is package-private: a refund only reaches SUCCEEDED through
// Payment.ConfirmRefund, because the payment's refundedAmount and state must move in the same
// step. Allowing a refund to be confirmed independently would let the two drift.
func (r *Refund) markSucceeded(gatewayRef string, now time.Time) error {
	if err := refundMachine.Transition(r.status, RefundSucceeded); err != nil {
		return err
	}
	r.status = RefundSucceeded
	if gatewayRef != "" {
		r.gatewayRef = gatewayRef
	}
	r.settledAt = &now
	r.updatedAt = now
	return nil
}

// MarkFailed records that the refund could not be completed. The amount returns to the
// refundable balance automatically, because Payment.AddRefund only counts PENDING and
// SUBMITTED refunds against the ceiling.
func (r *Refund) MarkFailed(code, message string, now time.Time) error {
	if err := refundMachine.Transition(r.status, RefundFailed); err != nil {
		return err
	}
	r.status = RefundFailed
	r.failureCode = code
	r.failureMessage = message
	r.updatedAt = now
	return nil
}

// Cancel withdraws a refund that has not been submitted.
func (r *Refund) Cancel(now time.Time) error {
	if err := refundMachine.Transition(r.status, RefundCanceled); err != nil {
		return err
	}
	r.status = RefundCanceled
	r.updatedAt = now
	return nil
}

// RehydrateRefundParams carries persisted refund state.
type RehydrateRefundParams struct {
	ID             shared.RefundID
	PaymentID      shared.PaymentID
	TenantID       shared.TenantID
	Amount         money.Money
	Reason         RefundReason
	Status         RefundStatus
	GatewayRef     string
	IdempotencyKey string
	FailureCode    string
	FailureMessage string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	SubmittedAt    *time.Time
	SettledAt      *time.Time
}

// RehydrateRefund reconstructs a Refund from persisted state.
func RehydrateRefund(p RehydrateRefundParams) (*Refund, error) {
	if !refundMachine.IsKnown(p.Status) {
		return nil, apierror.Newf(apierror.CodeInternalError, "refund %s has unknown status %q", p.ID, p.Status)
	}
	return &Refund{
		id: p.ID, paymentID: p.PaymentID, tenantID: p.TenantID, amount: p.Amount,
		reason: p.Reason, status: p.Status, gatewayRef: p.GatewayRef,
		idempotencyKey: p.IdempotencyKey, failureCode: p.FailureCode, failureMessage: p.FailureMessage,
		createdAt: p.CreatedAt, updatedAt: p.UpdatedAt, submittedAt: p.SubmittedAt, settledAt: p.SettledAt,
	}, nil
}
