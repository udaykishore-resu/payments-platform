package payment

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Payment is the aggregate root for one merchant instruction to move money.
//
// Aggregate boundary: a Payment owns its attempts and its refunds. All three are written in
// one transaction, because the invariants that matter — "at most one successful attempt",
// "refunds never exceed capture" — span them. Nothing outside this boundary is written in the
// same transaction; the routing plan, the ledger and the outbox are separate concerns reached
// through events.
//
// Immutability: amount, currency, merchant, tenant and payment method are fixed at creation
// (invariant I4). A merchant that wants to charge a different amount creates a different
// payment. This is not a convenience decision — a mutable amount makes the idempotency
// fingerprint meaningless and makes the ledger unauditable.
//
// Concurrency: optimistic, via version. Every mutating method increments the version, and the
// repository writes with `WHERE version = $old`. A losing writer gets a conflict rather than a
// lost update. Pessimistic locking was rejected because the contended case here is a duplicate
// request, and the correct answer to a duplicate is to reject it, not to queue behind it.
type Payment struct {
	id         shared.PaymentID
	tenantID   shared.TenantID
	merchantID shared.MerchantID

	amount        money.Money
	captureMethod CaptureMethod
	paymentMethod shared.PaymentMethod
	methodRef     PaymentMethodReference

	state   State
	version shared.Version
	// baseVersion is the version the persisted row holds: what this aggregate was read at, or
	// what it was last successfully written as. It is the optimistic-concurrency expectation and
	// it is deliberately *not* derived from version, because a unit of work may make several
	// state changes before a single save. See BaseVersion.
	baseVersion shared.Version

	// Running totals. These are denormalized onto the aggregate rather than recomputed from
	// attempts and refunds on every read because they are checked on every capture and refund,
	// and because the database CHECK constraints that back invariants I1 and I2 need columns
	// to constrain.
	authorizedAmount money.Money
	capturedAmount   money.Money
	refundedAmount   money.Money

	attempts []*Attempt
	refunds  []*Refund

	// selectedGateway is the gateway of the currently live or most recent attempt. It is
	// derived state, kept for query convenience and for metric labelling.
	selectedGateway shared.GatewayID
	routingPlanID   shared.PlanID

	description  string
	statementRef string
	metadata     map[string]string
	customer     CustomerReference

	idempotencyKey string
	correlationID  string

	createdAt    time.Time
	updatedAt    time.Time
	authorizedAt *time.Time
	capturedAt   *time.Time
	// authExpiresAt is when an authorization lapses if not captured. Card authorizations
	// typically expire in 7 days; the exact window is gateway- and scheme-dependent and is
	// supplied by the adapter.
	authExpiresAt *time.Time

	// events accumulates domain events raised by this unit of work. The application layer
	// drains them and writes them to the outbox in the same transaction as the state change
	// (baseline §13.4). They are not persisted as part of the aggregate.
	events []Event
}

// PaymentMethodReference identifies the tender without ever carrying card data.
//
// This type is the structural expression of the PCI decision in baseline §17: there is no
// field here that could hold a primary account number. The token is issued by the gateway's
// client-side tokenization (Stripe Elements, Adyen Components, PayPal SDK); the display fields
// are what the gateway hands back for receipts and are safe to store.
type PaymentMethodReference struct {
	// Token is the gateway-issued reference. Opaque to the platform.
	Token string
	// Type narrows the tender within the payment method, e.g. "visa" within CARD.
	Brand string
	// Last4 is the last four digits of the funding instrument. Storing four digits is
	// explicitly permitted and is not cardholder data under PCI DSS.
	Last4 string
	// ExpMonth and ExpYear support expiry-driven decline prediction and account updater flows.
	ExpMonth int
	ExpYear  int
	// Country is the issuing country, used for interchange estimation and risk scoring.
	Country shared.Country
	// NetworkToken indicates the reference is a scheme network token rather than a
	// gateway-local token, which changes both the interchange and the portability between
	// gateways.
	NetworkToken bool
}

// CustomerReference carries the merchant's identifier for the payer plus the minimum needed
// for risk scoring and SCA. It deliberately does not carry a full customer profile: the
// platform is not the merchant's CRM, and every personal field stored here is a GDPR liability
// with no offsetting benefit.
type CustomerReference struct {
	MerchantCustomerID string
	EmailHash          string // SHA-256 of the lowercased email; used for velocity, never for contact
	IPAddress          string // retained for the risk window only, then truncated
	Country            shared.Country
}

// NewPaymentParams are the inputs to creating a payment. A parameter struct rather than a
// twelve-argument constructor: the arguments are mostly the same type, and a positional
// mix-up between two string identifiers is a bug the compiler cannot catch.
type NewPaymentParams struct {
	TenantID       shared.TenantID
	MerchantID     shared.MerchantID
	Amount         money.Money
	PaymentMethod  shared.PaymentMethod
	MethodRef      PaymentMethodReference
	CaptureMethod  CaptureMethod
	Description    string
	StatementRef   string
	Metadata       map[string]string
	Customer       CustomerReference
	IdempotencyKey string
	CorrelationID  string
}

// New creates a Payment in CREATED, validating the invariants that must hold from the first
// instant. Business rules that depend on merchant configuration — currency enabled, amount
// within limits, method permitted — are validation-plane concerns (L5) and are checked before
// this constructor is reached; the constructor enforces only what is true of every payment
// everywhere.
func New(p NewPaymentParams, clock shared.Clock) (*Payment, error) {
	if p.TenantID.IsZero() {
		return nil, apierror.New(apierror.CodeMissingTenantContext, "payment requires a tenant")
	}
	if p.MerchantID.IsZero() {
		return nil, apierror.New(apierror.CodeValidationFailed, "payment requires a merchant")
	}
	if !p.Amount.IsValid() {
		return nil, apierror.New(apierror.CodeAmountInvalid, "payment requires a valid currency")
	}
	if !p.Amount.IsPositive() {
		return nil, apierror.New(apierror.CodeAmountInvalid, "payment amount must be greater than zero").
			WithDetail(apierror.Detail{
				Field: "amount", Code: "NOT_POSITIVE",
				Message: "amount must be a positive integer in the currency's minor units",
				RuleID:  "L5.AMOUNT_IS_POSITIVE",
			})
	}
	if !p.PaymentMethod.IsValid() {
		return nil, apierror.Newf(apierror.CodePaymentMethodNotSupported, "unsupported payment method %q", p.PaymentMethod)
	}
	if p.MethodRef.Token == "" {
		return nil, apierror.New(apierror.CodeValidationFailed, "a tokenized payment method reference is required").
			WithDetail(apierror.Detail{
				Field: "paymentMethod.token", Code: "MISSING_TOKEN",
				Message: "card data must be tokenized at the gateway edge; this API accepts only a token reference",
				RuleID:  "L5.PAYMENT_METHOD_TOKENIZED",
			})
	}
	cm := p.CaptureMethod
	if cm == "" {
		cm = CaptureAutomatic
	}
	if !cm.IsValid() {
		return nil, apierror.Newf(apierror.CodeValidationFailed, "invalid capture method %q", p.CaptureMethod)
	}
	// A manual capture on a method that has no authorization step would be accepted here and
	// rejected by the gateway later, with a much worse error. Catch it at the boundary.
	if cm == CaptureManual && !p.PaymentMethod.SupportsSeparateCapture() {
		return nil, apierror.Newf(apierror.CodeValidationFailed,
			"payment method %s does not support manual capture", p.PaymentMethod).
			WithDetail(apierror.Detail{
				Field: "captureMethod", Code: "UNSUPPORTED_FOR_METHOD",
				Message: "this payment method settles in a single step; use AUTOMATIC",
				RuleID:  "L5.CAPTURE_METHOD_SUPPORTED",
			})
	}
	if p.IdempotencyKey == "" {
		return nil, apierror.New(apierror.CodeIdempotencyKeyRequired, "")
	}

	now := clock.Now()
	zero := money.Zero(p.Amount.Currency())
	pay := &Payment{
		id:               shared.NewPaymentID(),
		tenantID:         p.TenantID,
		merchantID:       p.MerchantID,
		amount:           p.Amount,
		captureMethod:    cm,
		paymentMethod:    p.PaymentMethod,
		methodRef:        p.MethodRef,
		state:            StateCreated,
		version:          1,
		baseVersion:      0,
		authorizedAmount: zero,
		capturedAmount:   zero,
		refundedAmount:   zero,
		description:      p.Description,
		statementRef:     p.StatementRef,
		metadata:         copyMetadata(p.Metadata),
		customer:         p.Customer,
		idempotencyKey:   p.IdempotencyKey,
		correlationID:    p.CorrelationID,
		createdAt:        now,
		updatedAt:        now,
	}
	pay.raise(EventPaymentCreated, now, map[string]any{
		"amount":        pay.amount,
		"paymentMethod": string(pay.paymentMethod),
		"captureMethod": string(pay.captureMethod),
	})
	return pay, nil
}

// Accessors. The aggregate's fields are unexported so that no code outside this package can
// put a Payment into a state the methods would have refused. Read access is free; write access
// goes through a method that checks the transition table and the invariants.

func (p *Payment) ID() shared.PaymentID          { return p.id }
func (p *Payment) TenantID() shared.TenantID     { return p.tenantID }
func (p *Payment) MerchantID() shared.MerchantID { return p.merchantID }
func (p *Payment) Amount() money.Money           { return p.amount }
func (p *Payment) Currency() money.Currency      { return p.amount.Currency() }
func (p *Payment) State() State                  { return p.state }
func (p *Payment) Version() shared.Version       { return p.version }

// BaseVersion is the version the row held when this aggregate was last read or written. It is
// what a repository's optimistic-concurrency predicate must compare against.
//
// # Why the repository cannot derive it
//
// The obvious derivation — "the aggregate bumped its version once, so the row holds version−1" —
// is right for a unit of work that makes exactly one change and wrong for every other. The
// orchestrator's dispatch makes two before its first save (start the attempt, mark the payment
// PROCESSING), so version−1 names a version the row never held and the save fails as a
// concurrency conflict against nobody. Tracking where the aggregate started is the only thing
// that is correct for both.
func (p *Payment) BaseVersion() shared.Version { return p.baseVersion }

// MarkPersisted records that the aggregate's current version is now the row's version.
//
// A repository calls it after a successful write. It is a method rather than a field a
// repository sets because it is the one operation that is legitimately *not* a state change:
// nothing about the payment changed, only what the database has seen — so it must not touch
// updatedAt and must not raise an event.
func (p *Payment) MarkPersisted()                      { p.baseVersion = p.version }
func (p *Payment) CaptureMethod() CaptureMethod        { return p.captureMethod }
func (p *Payment) PaymentMethod() shared.PaymentMethod { return p.paymentMethod }
func (p *Payment) MethodRef() PaymentMethodReference   { return p.methodRef }
func (p *Payment) AuthorizedAmount() money.Money       { return p.authorizedAmount }
func (p *Payment) CapturedAmount() money.Money         { return p.capturedAmount }
func (p *Payment) RefundedAmount() money.Money         { return p.refundedAmount }
func (p *Payment) SelectedGateway() shared.GatewayID   { return p.selectedGateway }
func (p *Payment) RoutingPlanID() shared.PlanID        { return p.routingPlanID }
func (p *Payment) Description() string                 { return p.description }
func (p *Payment) StatementRef() string                { return p.statementRef }
func (p *Payment) Customer() CustomerReference         { return p.customer }
func (p *Payment) IdempotencyKey() string              { return p.idempotencyKey }
func (p *Payment) CorrelationID() string               { return p.correlationID }
func (p *Payment) CreatedAt() time.Time                { return p.createdAt }
func (p *Payment) UpdatedAt() time.Time                { return p.updatedAt }
func (p *Payment) AuthorizedAt() *time.Time            { return p.authorizedAt }
func (p *Payment) CapturedAt() *time.Time              { return p.capturedAt }
func (p *Payment) AuthExpiresAt() *time.Time           { return p.authExpiresAt }
func (p *Payment) PartitionMonth() time.Time           { return shared.PartitionMonth(p.id) }

// Metadata returns a copy. Returning the live map would let a caller mutate aggregate state
// without going through a method, which is exactly what the unexported fields prevent.
func (p *Payment) Metadata() map[string]string { return copyMetadata(p.metadata) }

// Attempts returns the attempts in creation order. The slice is a copy; the Attempt pointers
// are not, because an Attempt is part of this aggregate and its methods enforce their own
// invariants.
func (p *Payment) Attempts() []*Attempt { return append([]*Attempt(nil), p.attempts...) }

// Refunds returns the refunds in creation order.
func (p *Payment) Refunds() []*Refund { return append([]*Refund(nil), p.refunds...) }

// LatestAttempt returns the most recently created attempt, or nil.
func (p *Payment) LatestAttempt() *Attempt {
	if len(p.attempts) == 0 {
		return nil
	}
	return p.attempts[len(p.attempts)-1]
}

// SuccessfulAttempt returns the attempt that succeeded, or nil. Invariant I3 guarantees there
// is at most one.
func (p *Payment) SuccessfulAttempt() *Attempt {
	for _, a := range p.attempts {
		if a.Outcome() == OutcomeSuccess {
			return a
		}
	}
	return nil
}

// HasUnresolvedAttempt reports whether any attempt is in an outcome that requires
// reconciliation. A payment with an unresolved attempt must not be failed, must not be
// retried, and must not be allowed to reach a terminal state.
func (p *Payment) HasUnresolvedAttempt() bool {
	for _, a := range p.attempts {
		if a.Outcome().RequiresReconciliation() {
			return true
		}
	}
	return false
}

// RefundableAmount returns captured minus refunded.
func (p *Payment) RefundableAmount() money.Money {
	remaining, err := p.capturedAmount.Sub(p.refundedAmount)
	if err != nil {
		// Currencies are fixed at construction, so this is unreachable; return zero rather
		// than panic, because a panic in a payment path is worse than a conservative answer.
		return money.Zero(p.amount.Currency())
	}
	return remaining
}

// CapturableAmount returns the amount still available to capture.
func (p *Payment) CapturableAmount() money.Money {
	base := p.authorizedAmount
	if base.IsZero() {
		base = p.amount
	}
	remaining, err := base.Sub(p.capturedAmount)
	if err != nil {
		return money.Zero(p.amount.Currency())
	}
	return remaining
}

// --- state transitions ---------------------------------------------------------------------

// StartAttempt records that the orchestrator has selected a gateway and is about to dispatch.
//
// The ordering here is the single most important thing in the payment path: the attempt row is
// created and committed *before* the gateway is called. If the process dies between this call
// and the gateway response, the attempt exists in TIMEOUT_UNKNOWN-equivalent limbo and the
// reconciler can find it using the deterministic gateway idempotency key. If the attempt were
// created after the call, a crash would leave a charge at the gateway that the platform has no
// record of — money moved, and nothing in our system knows.
func (p *Payment) StartAttempt(gatewayID shared.GatewayID, planID shared.PlanID, op shared.Operation, clock shared.Clock) (*Attempt, error) {
	if p.state.IsTerminal() {
		return nil, apierror.Newf(apierror.CodeInvalidStateTransition,
			"cannot start an attempt: payment is in terminal state %s", p.state)
	}
	if p.SuccessfulAttempt() != nil && op == shared.OpAuthorize {
		// Invariant I3, enforced in the domain as well as in the database. The database index
		// is the guarantee; this check is the one that produces a comprehensible error.
		return nil, apierror.New(apierror.CodePaymentAlreadyProcessed,
			"this payment already has a successful authorization attempt")
	}
	if p.HasUnresolvedAttempt() {
		return nil, apierror.New(apierror.CodePaymentAlreadyProcessed,
			"this payment has an attempt with an unknown outcome and is awaiting reconciliation; "+
				"retrying now risks a duplicate charge").
			WithDetail(apierror.Detail{
				Field: "status", Code: "AWAITING_RECONCILIATION",
				Message: "an earlier attempt timed out and its outcome is not yet known",
				RuleID:  "L7.NO_RETRY_WHILE_UNRESOLVED",
			})
	}

	now := clock.Now()
	att := newAttempt(p.id, p.tenantID, gatewayID, op, len(p.attempts)+1, p.amount, now)
	p.attempts = append(p.attempts, att)
	p.selectedGateway = gatewayID
	p.routingPlanID = planID
	p.touch(now)
	p.raise(EventPaymentAttempted, now, map[string]any{
		"attemptId": att.ID().String(),
		"gatewayId": gatewayID.String(),
		"operation": string(op),
		"sequence":  att.Sequence(),
	})
	return att, nil
}

// MarkProcessing moves the payment into PROCESSING, which is where it sits while an attempt is
// in flight.
// It raises no event of its own: payment.attempted.v1 already records that a gateway was
// engaged, and a second event carrying no information a consumer can act on is noise on a
// topic that carries the platform's highest message volume.
func (p *Payment) MarkProcessing(clock shared.Clock) error {
	return p.transition(StateProcessing, clock, "", nil)
}

// RequireAction parks the payment awaiting an out-of-band step and records where the payer
// must be sent.
func (p *Payment) RequireAction(action NextAction, clock shared.Clock) error {
	return p.transition(StateRequiresAction, clock, EventPaymentRequiresAction, map[string]any{
		"actionType":  string(action.Type),
		"redirectUrl": action.RedirectURL,
	})
}

// MarkPending records that the gateway accepted the instruction but the outcome is genuinely
// asynchronous.
func (p *Payment) MarkPending(clock shared.Clock) error {
	return p.transition(StatePending, clock, "", nil)
}

// MarkAuthorized records a successful authorization for the given amount.
//
// Partial authorization is real — some issuers approve less than requested — so the authorized
// amount is a parameter rather than assumed equal to the payment amount.
func (p *Payment) MarkAuthorized(authorized money.Money, expiresAt *time.Time, clock shared.Clock) error {
	if authorized.Currency() != p.amount.Currency() {
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"gateway authorized in %s but the payment is in %s", authorized.Currency(), p.amount.Currency())
	}
	if !authorized.IsPositive() {
		return apierror.New(apierror.CodeGatewayContractViolation, "authorized amount must be positive")
	}
	more, cmpErr := authorized.GreaterThan(p.amount)
	if cmpErr != nil {
		return apierror.Wrap(cmpErr, apierror.CodeInternalError,
			"authorized amount could not be compared with the payment amount")
	}
	if more {
		// A gateway authorizing more than we asked for is a contract violation, not a windfall.
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"gateway authorized %s which exceeds the requested %s", authorized, p.amount)
	}
	now := clock.Now()
	if err := machine.Transition(p.state, StateAuthorized); err != nil {
		return err
	}
	p.state = StateAuthorized
	p.authorizedAmount = authorized
	p.authorizedAt = &now
	p.authExpiresAt = expiresAt
	p.touch(now)
	p.raise(EventPaymentAuthorized, now, map[string]any{
		"authorizedAmount": authorized,
		"gatewayId":        p.selectedGateway.String(),
	})
	return nil
}

// MarkCaptured records a capture. It is used both for the two-step flow (AUTHORIZED →
// CAPTURED) and for auto-capture (PROCESSING → CAPTURED).
//
// Invariant I2 is enforced here: cumulative captures may not exceed the authorized amount.
func (p *Payment) MarkCaptured(captured money.Money, clock shared.Clock) error {
	if captured.Currency() != p.amount.Currency() {
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"capture currency %s does not match payment currency %s", captured.Currency(), p.amount.Currency())
	}
	if !captured.IsPositive() {
		return apierror.New(apierror.CodeAmountInvalid, "capture amount must be positive")
	}
	newTotal, err := p.capturedAmount.Add(captured)
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "capture total overflow")
	}
	ceiling := p.authorizedAmount
	if ceiling.IsZero() {
		// Auto-capture: nothing was separately authorized, so the payment amount is the
		// ceiling.
		ceiling = p.amount
	}
	over, cmpErr := newTotal.GreaterThan(ceiling)
	if cmpErr != nil {
		// I2 cannot be evaluated, so it cannot be asserted. Failing closed is the only safe
		// answer: treating an uncomparable pair as "within the authorization" would admit the
		// capture the invariant exists to refuse.
		return apierror.Wrap(cmpErr, apierror.CodeInternalError,
			"cumulative captures could not be compared with the authorized amount")
	}
	if over {
		return apierror.Newf(apierror.CodeCaptureExceedsAuthorized,
			"capturing %s would bring the total to %s, exceeding the authorized %s",
			captured, newTotal, ceiling).
			WithDetail(apierror.Detail{
				Field: "amount", Code: "EXCEEDS_AUTHORIZED",
				Message: "cumulative captures may not exceed the authorized amount",
				RuleID:  "L7.I2_CAPTURE_WITHIN_AUTHORIZATION",
			})
	}
	now := clock.Now()
	if err := machine.Transition(p.state, StateCaptured); err != nil {
		return err
	}
	p.state = StateCaptured
	p.capturedAmount = newTotal
	if p.capturedAt == nil {
		p.capturedAt = &now
	}
	p.touch(now)
	p.raise(EventPaymentCaptured, now, map[string]any{
		"capturedAmount": captured,
		"capturedTotal":  newTotal,
		"gatewayId":      p.selectedGateway.String(),
	})
	return nil
}

// MarkFailed records a definitive failure.
//
// It refuses to run while an attempt is unresolved. That refusal is deliberate and is the
// mechanism behind ADR-013: nothing — not a timeout, not a timer, not an operator in a hurry —
// may declare a payment failed while there is an outstanding possibility that money moved.
func (p *Payment) MarkFailed(reason DeclineReason, detail string, clock shared.Clock) error {
	if p.HasUnresolvedAttempt() {
		return apierror.New(apierror.CodeInvalidStateTransition,
			"cannot fail a payment with an unresolved attempt; the outcome must be reconciled first").
			WithDetail(apierror.Detail{
				Field: "status", Code: "AWAITING_RECONCILIATION",
				Message: "an attempt timed out and its outcome is unknown",
				RuleID:  "L7.NO_FAIL_WHILE_UNRESOLVED",
			})
	}
	now := clock.Now()
	if err := machine.Transition(p.state, StateFailed); err != nil {
		return err
	}
	p.state = StateFailed
	p.touch(now)
	p.raise(EventPaymentFailed, now, map[string]any{
		"declineReason": string(reason),
		"detail":        detail,
		"gatewayId":     p.selectedGateway.String(),
	})
	return nil
}

// Void releases an authorization before capture.
func (p *Payment) Void(clock shared.Clock) error {
	if !p.capturedAmount.IsZero() {
		return apierror.New(apierror.CodeInvalidStateTransition,
			"cannot void a payment that has been captured; issue a refund instead").
			WithDetail(apierror.Detail{
				Field: "status", Code: "ALREADY_CAPTURED",
				Message: "voiding releases an authorization; captured funds are returned by refund",
				RuleID:  "L7.VOID_REQUIRES_UNCAPTURED",
			})
	}
	return p.transition(StateVoided, clock, EventPaymentVoided, map[string]any{
		"gatewayId": p.selectedGateway.String(),
	})
}

// Cancel abandons a payment that has not reached a gateway, or one parked awaiting payer
// action.
func (p *Payment) Cancel(reason string, clock shared.Clock) error {
	return p.transition(StateCanceled, clock, EventPaymentCanceled, map[string]any{"reason": reason})
}

// Expire records that an authorization or a required action lapsed.
func (p *Payment) Expire(clock shared.Clock) error {
	return p.transition(StateExpired, clock, EventPaymentExpired, nil)
}

// MarkSettled records a settlement report from the gateway.
func (p *Payment) MarkSettled(settledAt time.Time, settlementRef string, clock shared.Clock) error {
	return p.transition(StateSettled, clock, EventPaymentSettled, map[string]any{
		"settledAt":     settledAt.UTC().Format(time.RFC3339Nano),
		"settlementRef": settlementRef,
	})
}

// AddRefund records a refund against the payment, enforcing invariant I1.
//
// The refund's own lifecycle (submitted → succeeded/failed at the gateway) lives on the Refund
// entity. The aggregate's refundedAmount tracks only refunds that have succeeded, because a
// pending refund that ultimately fails must not permanently reduce the refundable balance.
func (p *Payment) AddRefund(amount money.Money, reason RefundReason, idempotencyKey string, clock shared.Clock) (*Refund, error) {
	if !p.state.AllowsRefund() {
		return nil, apierror.Newf(apierror.CodeInvalidStateTransition,
			"a payment in state %s cannot be refunded", p.state)
	}
	if amount.Currency() != p.amount.Currency() {
		return nil, apierror.Newf(apierror.CodeValidationFailed,
			"refund currency %s does not match payment currency %s", amount.Currency(), p.amount.Currency())
	}
	if !amount.IsPositive() {
		return nil, apierror.New(apierror.CodeAmountInvalid, "refund amount must be positive")
	}
	// Include refunds that are in flight, not merely succeeded, in the ceiling check. Two
	// concurrent refund requests each for the full amount must not both be admitted; the
	// database's serialized update on the payment row plus this check makes that impossible.
	committed := p.refundedAmount
	for _, r := range p.refunds {
		if r.Status() == RefundPending || r.Status() == RefundSubmitted {
			var err error
			committed, err = committed.Add(r.Amount())
			if err != nil {
				return nil, apierror.Wrap(err, apierror.CodeInternalError, "refund total overflow")
			}
		}
	}
	prospective, err := committed.Add(amount)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "refund total overflow")
	}
	over, cmpErr := prospective.GreaterThan(p.capturedAmount)
	if cmpErr != nil {
		// As in MarkCaptured: an invariant that cannot be evaluated must not be reported as
		// satisfied.
		return nil, apierror.Wrap(cmpErr, apierror.CodeInternalError,
			"cumulative refunds could not be compared with the captured amount")
	}
	if over {
		return nil, apierror.Newf(apierror.CodeRefundExceedsCaptured,
			"refunding %s would bring the total to %s, exceeding the captured %s",
			amount, prospective, p.capturedAmount).
			WithDetail(apierror.Detail{
				Field: "amount", Code: "EXCEEDS_CAPTURED",
				Message: "cumulative refunds, including any in flight, may not exceed the captured amount",
				RuleID:  "L7.I1_REFUND_WITHIN_CAPTURE",
			})
	}

	now := clock.Now()
	r := newRefund(p.id, p.tenantID, amount, reason, idempotencyKey, now)
	p.refunds = append(p.refunds, r)
	p.touch(now)
	return r, nil
}

// ConfirmRefund records that a refund succeeded at the gateway and moves the payment to
// PARTIALLY_REFUNDED or REFUNDED accordingly.
func (p *Payment) ConfirmRefund(refundID shared.RefundID, gatewayRef string, clock shared.Clock) error {
	var target *Refund
	for _, r := range p.refunds {
		if r.ID() == refundID {
			target = r
			break
		}
	}
	if target == nil {
		return apierror.Newf(apierror.CodeValidationFailed, "refund %s does not belong to payment %s", refundID, p.id)
	}
	now := clock.Now()
	if err := target.markSucceeded(gatewayRef, now); err != nil {
		return err
	}
	newTotal, err := p.refundedAmount.Add(target.Amount())
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "refund total overflow")
	}

	cmp, cmpErr := newTotal.Cmp(p.capturedAmount)
	if cmpErr != nil {
		// A discarded error here yielded cmp == 0, which reads as "fully refunded" and moves the
		// payment to REFUNDED on a comparison that never happened.
		return apierror.Wrap(cmpErr, apierror.CodeInternalError,
			"cumulative refunds could not be compared with the captured amount")
	}
	next := StatePartiallyRefunded
	if cmp >= 0 {
		next = StateRefunded
	}
	if err := machine.Transition(p.state, next); err != nil {
		return err
	}
	p.state = next
	p.refundedAmount = newTotal
	p.touch(now)
	p.raise(EventPaymentRefunded, now, map[string]any{
		"refundId":      refundID.String(),
		"refundAmount":  target.Amount(),
		"refundedTotal": newTotal,
		"fullyRefunded": next == StateRefunded,
	})
	return nil
}

// MarkDisputed records a chargeback.
func (p *Payment) MarkDisputed(disputeRef, reasonCode string, clock shared.Clock) error {
	return p.transition(StateDisputed, clock, EventPaymentDisputed, map[string]any{
		"disputeRef": disputeRef,
		"reasonCode": reasonCode,
	})
}

// ResolveDispute moves a disputed payment back to its pre-dispute position (won) or to
// refunded (lost).
func (p *Payment) ResolveDispute(won bool, clock shared.Clock) error {
	if p.state != StateDisputed {
		return apierror.Newf(apierror.CodeInvalidStateTransition,
			"payment is in state %s, not DISPUTED", p.state)
	}
	next := StateRefunded
	if won {
		next = StateCaptured
		if p.capturedAt != nil && p.settledLike() {
			next = StateSettled
		}
	}
	return p.transition(next, clock, EventPaymentDisputeResolved, map[string]any{"won": won})
}

// settledLike reports whether the payment had reached settlement before the dispute. Recorded
// via the presence of a settlement event in history; approximated here by the capture age.
func (p *Payment) settledLike() bool {
	for i := len(p.events) - 1; i >= 0; i-- {
		if p.events[i].Type == EventPaymentSettled {
			return true
		}
	}
	return false
}

// RequireReconciliation flags the payment as needing the reconciler's attention. It does not
// change the payment's state — that is the point.
func (p *Payment) RequireReconciliation(attemptID shared.AttemptID, reason string, clock shared.Clock) {
	now := clock.Now()
	p.touch(now)
	p.raise(EventPaymentReconciliationRequired, now, map[string]any{
		"attemptId": attemptID.String(),
		"reason":    reason,
		"gatewayId": p.selectedGateway.String(),
	})
}

// transition is the common path for state changes with no additional invariant.
func (p *Payment) transition(to State, clock shared.Clock, evt EventType, payload map[string]any) error {
	if err := machine.Transition(p.state, to); err != nil {
		return err
	}
	now := clock.Now()
	p.state = to
	p.touch(now)
	if evt != "" {
		p.raise(evt, now, payload)
	}
	return nil
}

func (p *Payment) touch(now time.Time) {
	p.updatedAt = now
	p.version = p.version.Next()
}

func (p *Payment) raise(t EventType, at time.Time, payload map[string]any) {
	p.events = append(p.events, Event{
		Type:        t,
		PaymentID:   p.id,
		TenantID:    p.tenantID,
		MerchantID:  p.merchantID,
		OccurredAt:  at,
		Version:     p.version,
		Payload:     payload,
		Correlation: p.correlationID,
	})
}

// PendingEvents returns the domain events raised in this unit of work. The application layer
// drains them with DrainEvents and writes them to the outbox inside the same transaction as
// the state change.
func (p *Payment) PendingEvents() []Event { return append([]Event(nil), p.events...) }

// DrainEvents returns and clears the pending events. Called exactly once per unit of work,
// by the repository, inside the transaction.
func (p *Payment) DrainEvents() []Event {
	out := p.events
	p.events = nil
	return out
}

func copyMetadata(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// NextActionType names the kind of out-of-band step a payer must complete.
type NextActionType string

const (
	ActionRedirect      NextActionType = "REDIRECT"
	ActionThreeDSChall  NextActionType = "THREE_DS_CHALLENGE"
	ActionDisplayQR     NextActionType = "DISPLAY_QR_CODE"
	ActionAwaitTransfer NextActionType = "AWAIT_BANK_TRANSFER"
)

// NextAction is what the merchant must show the payer to complete the payment.
type NextAction struct {
	Type        NextActionType
	RedirectURL string
	ExpiresAt   *time.Time
}

// RehydrateParams carries the persisted state of a Payment back into the aggregate.
//
// This exists because the aggregate's fields are unexported: the repository cannot construct a
// Payment field by field, and it must not be able to, or the invariants would be bypassed on
// every read. Rehydrate is the single, explicit, reviewed doorway, and it validates that what
// came out of the database is a state this binary understands.
type RehydrateParams struct {
	ID               shared.PaymentID
	TenantID         shared.TenantID
	MerchantID       shared.MerchantID
	Amount           money.Money
	CaptureMethod    CaptureMethod
	PaymentMethod    shared.PaymentMethod
	MethodRef        PaymentMethodReference
	State            State
	Version          shared.Version
	AuthorizedAmount money.Money
	CapturedAmount   money.Money
	RefundedAmount   money.Money
	Attempts         []*Attempt
	Refunds          []*Refund
	SelectedGateway  shared.GatewayID
	RoutingPlanID    shared.PlanID
	Description      string
	StatementRef     string
	Metadata         map[string]string
	Customer         CustomerReference
	IdempotencyKey   string
	CorrelationID    string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	AuthorizedAt     *time.Time
	CapturedAt       *time.Time
	AuthExpiresAt    *time.Time
}

// Rehydrate reconstructs a Payment from persisted state.
func Rehydrate(p RehydrateParams) (*Payment, error) {
	if !p.State.IsKnown() {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"payment %s has unknown state %q; this row may have been written by a newer version of the service",
			p.ID, p.State)
	}
	return &Payment{
		id: p.ID, tenantID: p.TenantID, merchantID: p.MerchantID,
		amount: p.Amount, captureMethod: p.CaptureMethod, paymentMethod: p.PaymentMethod,
		methodRef: p.MethodRef, state: p.State, version: p.Version, baseVersion: p.Version,
		authorizedAmount: p.AuthorizedAmount, capturedAmount: p.CapturedAmount, refundedAmount: p.RefundedAmount,
		attempts: p.Attempts, refunds: p.Refunds,
		selectedGateway: p.SelectedGateway, routingPlanID: p.RoutingPlanID,
		description: p.Description, statementRef: p.StatementRef,
		metadata: copyMetadata(p.Metadata), customer: p.Customer,
		idempotencyKey: p.IdempotencyKey, correlationID: p.CorrelationID,
		createdAt: p.CreatedAt, updatedAt: p.UpdatedAt,
		authorizedAt: p.AuthorizedAt, capturedAt: p.CapturedAt, authExpiresAt: p.AuthExpiresAt,
	}, nil
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-20, BR-21, BR-24, BR-25, BR-26, BR-28, FR-53, FR-63, FR-66, FR-69, FR-70, FR-71.
//
// The payment aggregate: creation, the FSM, capture, refund, void, and the I1/I2 money
// invariants that make a second charge unrepresentable rather than merely unlikely
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
