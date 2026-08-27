package payment

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Attempt is one execution of a payment instruction against one gateway.
//
// Making the attempt a first-class entity rather than a set of columns on the payment is the
// design decision that makes double-charging structurally impossible (ADR-012). The chain of
// reasoning:
//
//  1. Failover must not mutate the record of the previous try, because that record is the only
//     evidence that a charge may exist at the previous gateway.
//  2. Each try therefore needs its own identity, its own gateway, and its own outcome.
//  3. Each try's gateway idempotency key is derived from that identity, so a transport retry to
//     the same gateway is deduplicated by the gateway, while a deliberate failover to a
//     different gateway is correctly a new authorization.
//  4. A partial unique index on (payment_id) WHERE outcome = 'SUCCESS' then makes "two
//     successful attempts on one payment" unrepresentable in the database, not merely
//     prevented by application code.
//
// Each of those four steps is load-bearing; remove any one and the guarantee is only as good
// as the least careful code path.
type Attempt struct {
	id        shared.AttemptID
	paymentID shared.PaymentID
	tenantID  shared.TenantID
	gatewayID shared.GatewayID
	// connectionID names the merchant-to-gateway binding this try was dispatched over.
	//
	// It is distinct from gatewayID and the distinction is the point: one merchant can hold
	// several connections to the same gateway — a live one and one being re-provisioned, or two
	// sub-accounts in different corridors — and the credential, the external account and the
	// certification state all belong to the *connection*, not to the gateway. Without it, an
	// attempt that failed authentication cannot be traced to the credential it used, which is
	// the first question asked when a rotation goes wrong.
	//
	// It is optional in the domain rather than required, because attempts persisted before this
	// field existed have no value for it and rehydrating them must not fail. A repository is
	// permitted to load a blank one; the orchestrator is not permitted to create one.
	connectionID shared.ConnectionID
	operation    shared.Operation
	sequence     int

	amount  money.Money
	outcome AttemptOutcome

	// gatewayRef is the gateway's own identifier for the transaction, captured as soon as it
	// is known. It is what the reconciler uses to ask "did this actually happen".
	gatewayRef string
	// idempotencyKey is the key sent to the gateway. Derived deterministically from the
	// attempt ID so it survives a crash and can be recomputed during reconciliation.
	idempotencyKey string

	declineReason DeclineReason
	errorCode     string
	errorMessage  string
	// rawStatus is the gateway's own status string, retained verbatim for support and
	// reconciliation. It never drives control flow — that is what the normalized outcome and
	// decline reason are for.
	rawStatus string

	// networkAdvice carries scheme-level retry guidance where the gateway surfaces it
	// (Visa/Mastercard return codes that say "do not retry this card"). When present it
	// overrides the platform's own failover judgement in the restrictive direction only.
	networkAdviceNoRetry bool

	dispatchedAt *time.Time
	resolvedAt   *time.Time
	latency      time.Duration
	createdAt    time.Time
	updatedAt    time.Time
}

// newAttempt is package-private: an Attempt is only ever created through Payment.StartAttempt,
// which is what enforces the ordering and the invariants.
func newAttempt(paymentID shared.PaymentID, tenantID shared.TenantID, gatewayID shared.GatewayID,
	op shared.Operation, sequence int, amount money.Money, now time.Time) *Attempt {
	id := shared.NewAttemptID()
	return &Attempt{
		id:             id,
		paymentID:      paymentID,
		tenantID:       tenantID,
		gatewayID:      gatewayID,
		operation:      op,
		sequence:       sequence,
		amount:         amount,
		outcome:        OutcomePending,
		idempotencyKey: DeriveGatewayIdempotencyKey(id, op, defaultKeySalt),
		createdAt:      now,
		updatedAt:      now,
	}
}

// Accessors.

func (a *Attempt) ID() shared.AttemptID        { return a.id }
func (a *Attempt) PaymentID() shared.PaymentID { return a.paymentID }
func (a *Attempt) TenantID() shared.TenantID   { return a.tenantID }
func (a *Attempt) GatewayID() shared.GatewayID { return a.gatewayID }

// ConnectionID is the merchant-to-gateway binding this attempt used. It is empty for attempts
// recorded before the field existed; a renderer should omit it rather than emit a blank, because
// an identifier that resolves to nothing is worse than an absent one.
func (a *Attempt) ConnectionID() shared.ConnectionID { return a.connectionID }

func (a *Attempt) Operation() shared.Operation  { return a.operation }
func (a *Attempt) Sequence() int                { return a.sequence }
func (a *Attempt) Amount() money.Money          { return a.amount }
func (a *Attempt) Outcome() AttemptOutcome      { return a.outcome }
func (a *Attempt) GatewayRef() string           { return a.gatewayRef }
func (a *Attempt) IdempotencyKey() string       { return a.idempotencyKey }
func (a *Attempt) DeclineReason() DeclineReason { return a.declineReason }
func (a *Attempt) ErrorCode() string            { return a.errorCode }
func (a *Attempt) ErrorMessage() string         { return a.errorMessage }
func (a *Attempt) RawStatus() string            { return a.rawStatus }
func (a *Attempt) DispatchedAt() *time.Time     { return a.dispatchedAt }
func (a *Attempt) ResolvedAt() *time.Time       { return a.resolvedAt }
func (a *Attempt) Latency() time.Duration       { return a.latency }
func (a *Attempt) CreatedAt() time.Time         { return a.createdAt }
func (a *Attempt) UpdatedAt() time.Time         { return a.updatedAt }
func (a *Attempt) NetworkAdviceNoRetry() bool   { return a.networkAdviceNoRetry }

// BindConnection records which merchant-to-gateway connection this attempt was dispatched over.
//
// # Why it is a separate call rather than a StartAttempt parameter
//
// StartAttempt is the aggregate's invariant gate — terminal state, one successful authorization,
// no start while an outcome is unknown — and those checks are about the *payment*. The connection
// is resolved a moment later, by the gateway resolver, and threading it through the aggregate's
// gate would mean the payment had to know about credential resolution. Binding it here keeps the
// two concerns where they belong and makes the ordering explicit at the call site.
//
// # The two invariants
//
// It refuses after dispatch, because the connection is a statement about which credential signed
// the request that has already gone out, and a value that can be edited afterwards is not
// evidence. It refuses a re-bind to a different connection for the same reason: a failover
// creates a *new* attempt (see the type comment), so an attempt whose connection changed would
// mean a previous try's record had been overwritten — exactly the mutation this aggregate's
// design exists to prevent. Re-binding the same value is a no-op, so a retried save is safe.
func (a *Attempt) BindConnection(id shared.ConnectionID) error {
	if id == "" {
		return apierror.New(apierror.CodeValidationFailed,
			"a connection reference is required to bind an attempt to its connection")
	}
	if a.connectionID == id {
		return nil
	}
	if a.connectionID != "" {
		return apierror.Newf(apierror.CodeInvalidStateTransition,
			"attempt %s is already bound to a connection and may not be re-bound; "+
				"a failover creates a new attempt rather than editing this one", a.id)
	}
	if a.outcome != OutcomePending {
		return apierror.Newf(apierror.CodeInvalidStateTransition,
			"attempt %s has already been dispatched (%s); its connection can no longer be recorded",
			a.id, a.outcome)
	}
	a.connectionID = id
	return nil
}

// Dispatch records that the request has been sent.
func (a *Attempt) Dispatch(now time.Time) error {
	if err := attemptMachine.Transition(a.outcome, OutcomeDispatched); err != nil {
		return err
	}
	a.outcome = OutcomeDispatched
	a.dispatchedAt = &now
	a.updatedAt = now
	return nil
}

// Succeed records a successful gateway response.
func (a *Attempt) Succeed(gatewayRef, rawStatus string, now time.Time) error {
	if err := attemptMachine.Transition(a.outcome, OutcomeSuccess); err != nil {
		return err
	}
	a.outcome = OutcomeSuccess
	a.gatewayRef = gatewayRef
	a.rawStatus = rawStatus
	a.resolve(now)
	return nil
}

// Decline records a definitive refusal with a normalized reason.
func (a *Attempt) Decline(reason DeclineReason, gatewayRef, rawStatus string, networkAdviceNoRetry bool, now time.Time) error {
	if err := attemptMachine.Transition(a.outcome, OutcomeDeclined); err != nil {
		return err
	}
	if reason == "" {
		// An adapter that cannot classify a decline must produce UNKNOWN, which does not
		// permit failover. Silence must not be read as permission.
		reason = DeclineUnknown
	}
	a.outcome = OutcomeDeclined
	a.declineReason = reason
	a.gatewayRef = gatewayRef
	a.rawStatus = rawStatus
	a.networkAdviceNoRetry = networkAdviceNoRetry
	a.resolve(now)
	return nil
}

// Fail records an error that occurred before the gateway could have acted on the instruction.
//
// The caller must be certain of that. If there is any possibility the gateway received and
// acted on the request, the correct call is TimeOut, not Fail. The distinction is the
// difference between a safe retry and a double charge.
func (a *Attempt) Fail(code, message string, now time.Time) error {
	if err := attemptMachine.Transition(a.outcome, OutcomeError); err != nil {
		return err
	}
	a.outcome = OutcomeError
	a.errorCode = code
	a.errorMessage = message
	a.resolve(now)
	return nil
}

// TimeOut records that the outcome is unknown.
//
// This is not a failure. The attempt stays open, the payment stays in PROCESSING, and the
// reconciler owns the resolution. Nothing in the platform may convert this into a failure
// on a timer.
func (a *Attempt) TimeOut(message string, now time.Time) error {
	if err := attemptMachine.Transition(a.outcome, OutcomeTimeoutUnknown); err != nil {
		return err
	}
	a.outcome = OutcomeTimeoutUnknown
	a.errorCode = string(apierror.CodeGatewayTimeout)
	a.errorMessage = message
	a.updatedAt = now
	if a.dispatchedAt != nil {
		a.latency = now.Sub(*a.dispatchedAt)
	}
	return nil
}

// Reconcile resolves an attempt whose outcome was unknown, using what the gateway reports when
// asked directly. It is the only path out of TIMEOUT_UNKNOWN.
func (a *Attempt) Reconcile(outcome AttemptOutcome, gatewayRef, rawStatus string, reason DeclineReason, now time.Time) error {
	if a.outcome != OutcomeTimeoutUnknown {
		return apierror.Newf(apierror.CodeInvalidStateTransition,
			"attempt %s is in outcome %s, not TIMEOUT_UNKNOWN, and does not need reconciliation", a.id, a.outcome)
	}
	if err := attemptMachine.Transition(a.outcome, outcome); err != nil {
		return err
	}
	a.outcome = outcome
	if gatewayRef != "" {
		a.gatewayRef = gatewayRef
	}
	if rawStatus != "" {
		a.rawStatus = rawStatus
	}
	if outcome == OutcomeDeclined {
		if reason == "" {
			reason = DeclineUnknown
		}
		a.declineReason = reason
	}
	a.resolve(now)
	return nil
}

func (a *Attempt) resolve(now time.Time) {
	a.resolvedAt = &now
	a.updatedAt = now
	if a.dispatchedAt != nil {
		a.latency = now.Sub(*a.dispatchedAt)
	}
}

// PermitsFailover reports whether the orchestrator may try a different gateway after this
// attempt. It combines the outcome-level rule with the decline-reason-level rule and with any
// scheme-level "do not retry" advice, taking the most restrictive answer.
func (a *Attempt) PermitsFailover() bool {
	if a.networkAdviceNoRetry {
		return false
	}
	switch a.outcome {
	case OutcomeError:
		return true
	case OutcomeDeclined:
		return a.declineReason.PermitsFailover()
	default:
		// SUCCESS, PENDING, DISPATCHED and TIMEOUT_UNKNOWN all forbid failover, each for its
		// own reason: already succeeded, not finished, or unknown.
		return false
	}
}

// defaultKeySalt is a build-time constant used when no per-environment salt is configured.
// It is not a secret: the gateway idempotency key needs to be deterministic and unguessable
// enough to avoid accidental collision, not unforgeable. The gateway authenticates us by our
// API credentials, not by this key.
const defaultKeySalt = "payments-platform/gateway-idempotency/v1"

// DeriveGatewayIdempotencyKey computes the key sent to the gateway for one attempt.
//
// Properties that matter:
//
//   - Deterministic. After a crash the reconciler recomputes the same key from the persisted
//     attempt ID and can ask the gateway "what happened to this".
//   - Distinct per attempt. Failing over to another gateway produces a different key, so the
//     new gateway correctly treats it as a new authorization rather than deduplicating against
//     something it never saw.
//   - Stable across retries of the same attempt. A transport-level retry to the same gateway
//     reuses the key, so the gateway deduplicates and no second charge occurs.
//   - Bounded length and charset. Gateways vary: Stripe accepts up to 255 characters, others
//     are stricter, so this is capped at 32 Base32 characters, which every integrated gateway
//     accepts.
func DeriveGatewayIdempotencyKey(attemptID shared.AttemptID, op shared.Operation, salt string) string {
	mac := hmac.New(sha256.New, []byte(salt))
	mac.Write([]byte(attemptID.String()))
	mac.Write([]byte{0})
	mac.Write([]byte(op.String()))
	sum := mac.Sum(nil)
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum)
	return strings.ToLower(enc[:32])
}

// RehydrateAttempt reconstructs an Attempt from persisted state. Like Payment.Rehydrate, this
// is the single reviewed doorway that lets the repository rebuild an aggregate whose fields are
// otherwise unreachable.
func RehydrateAttempt(p RehydrateAttemptParams) (*Attempt, error) {
	if !attemptMachine.IsKnown(p.Outcome) {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"attempt %s has unknown outcome %q", p.ID, p.Outcome)
	}
	return &Attempt{
		id: p.ID, paymentID: p.PaymentID, tenantID: p.TenantID, gatewayID: p.GatewayID,
		connectionID: p.ConnectionID,
		operation:    p.Operation, sequence: p.Sequence, amount: p.Amount, outcome: p.Outcome,
		gatewayRef: p.GatewayRef, idempotencyKey: p.IdempotencyKey,
		declineReason: p.DeclineReason, errorCode: p.ErrorCode, errorMessage: p.ErrorMessage,
		rawStatus: p.RawStatus, networkAdviceNoRetry: p.NetworkAdviceNoRetry,
		dispatchedAt: p.DispatchedAt, resolvedAt: p.ResolvedAt, latency: p.Latency,
		createdAt: p.CreatedAt, updatedAt: p.UpdatedAt,
	}, nil
}

// RehydrateAttemptParams carries persisted attempt state.
type RehydrateAttemptParams struct {
	ID        shared.AttemptID
	PaymentID shared.PaymentID
	TenantID  shared.TenantID
	GatewayID shared.GatewayID
	// ConnectionID may be empty: attempts written before the column existed have no value for
	// it, and refusing to rehydrate them would make the whole payment unreadable over a field
	// that is descriptive rather than load-bearing.
	ConnectionID         shared.ConnectionID
	Operation            shared.Operation
	Sequence             int
	Amount               money.Money
	Outcome              AttemptOutcome
	GatewayRef           string
	IdempotencyKey       string
	DeclineReason        DeclineReason
	ErrorCode            string
	ErrorMessage         string
	RawStatus            string
	NetworkAdviceNoRetry bool
	DispatchedAt         *time.Time
	ResolvedAt           *time.Time
	Latency              time.Duration
	CreatedAt            time.Time
	UpdatedAt            time.Time
}
