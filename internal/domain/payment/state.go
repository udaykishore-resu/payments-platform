// Package payment is the payment orchestration bounded context's domain model (BC-6).
//
// It contains the Payment aggregate, the PaymentAttempt aggregate, refunds, and the state
// machines that govern them. It imports nothing but the standard library, pkg/* and the shared
// kernel: no SQL, no HTTP, no gateway types. Everything in this package is deterministic and
// unit-testable without a running dependency.
package payment

import "github.com/udaykishore-resu/payments-platform/internal/domain/shared"

// State is the lifecycle state of a Payment.
//
// The set is larger than the brief's because a real payment system must be able to represent
// asynchronous and unknown outcomes. Without PENDING and without the rule that a timeout does
// not change state, a gateway that fails to answer forces the platform to guess, and a guess
// in this position is either a lost sale or a double charge. See docs/spec/00-design-baseline.md
// §9 and ADR-013.
type State string

const (
	// StateCreated is the payment as accepted by the API, before any gateway interaction.
	// Money has not moved and nothing external knows the payment exists.
	StateCreated State = "CREATED"

	// StateRequiresAction means the payer must complete an out-of-band step, almost always a
	// 3-D Secure challenge or a redirect to a bank. The payment is parked, not failed.
	StateRequiresAction State = "REQUIRES_ACTION"

	// StateProcessing means an attempt is in flight at a gateway. Critically, a payment also
	// *stays* here when a gateway call times out: at that moment we do not know whether money
	// moved, and the only safe representation of "we do not know" is "still processing".
	StateProcessing State = "PROCESSING"

	// StatePending means the gateway accepted the instruction but the outcome is genuinely
	// asynchronous — a bank debit that clears in days, a voucher awaiting payment. Distinct
	// from PROCESSING because PROCESSING beyond a few seconds is an anomaly worth alerting on,
	// whereas PENDING for three days is normal.
	StatePending State = "PENDING"

	// StateAuthorized means funds are held but not taken.
	StateAuthorized State = "AUTHORIZED"

	// StateCaptured means funds have been taken from the payer.
	StateCaptured State = "CAPTURED"

	// StateSettled means the gateway has reported the funds as settled to the merchant. This
	// is observed from settlement reports, never asserted by us.
	StateSettled State = "SETTLED"

	// StatePartiallyRefunded means some but not all of the captured amount has been returned.
	StatePartiallyRefunded State = "PARTIALLY_REFUNDED"

	// StateRefunded means the full captured amount has been returned.
	StateRefunded State = "REFUNDED"

	// StateVoided means an authorization was released before capture.
	StateVoided State = "VOIDED"

	// StateFailed means the payment definitively did not succeed.
	StateFailed State = "FAILED"

	// StateCanceled means the merchant or payer abandoned the payment before it reached a
	// gateway, or during a REQUIRES_ACTION step.
	StateCanceled State = "CANCELED"

	// StateExpired means an authorization or a required action lapsed.
	StateExpired State = "EXPIRED"

	// StateDisputed means the payer's issuer has raised a chargeback. It is not terminal: a
	// dispute can be won, returning the payment to CAPTURED or SETTLED, or lost, moving it to
	// REFUNDED.
	StateDisputed State = "DISPUTED"
)

// AllStates is the complete state universe, used to build the machine and to drive the
// exhaustive transition property test.
var AllStates = []State{
	StateCreated, StateRequiresAction, StateProcessing, StatePending,
	StateAuthorized, StateCaptured, StateSettled,
	StatePartiallyRefunded, StateRefunded, StateVoided,
	StateFailed, StateCanceled, StateExpired, StateDisputed,
}

// terminalStates have no outgoing transitions. Note that REFUNDED and SETTLED are *not*
// terminal: a refunded payment can still be disputed, and a settled payment can still be
// refunded. Treating them as terminal is a common and expensive modelling error — it makes the
// refund-after-settlement path, which is the *normal* path, unrepresentable.
var terminalStates = []State{StateFailed, StateCanceled, StateVoided, StateExpired}

// machine is the payment state machine. Every transition the platform permits is in this
// table; everything else is rejected with INVALID_STATE_TRANSITION. The table is the single
// source of truth for the domain check, the database CHECK constraint generated from it, and
// the diagram in docs/state-machines.md.
var machine = shared.NewStateMachine("payment", StateCreated, AllStates, terminalStates,
	[]shared.Transition[State]{
		// Dispatch. CAPTURED is reachable directly from PROCESSING because most payment
		// methods, and card payments with auto-capture, have no separate authorization step.
		{From: StateCreated, To: StateProcessing},
		{From: StateCreated, To: StateRequiresAction},
		{From: StateCreated, To: StateFailed},
		{From: StateCreated, To: StateCanceled},

		// Strong customer authentication and redirects.
		{From: StateRequiresAction, To: StateProcessing},
		{From: StateRequiresAction, To: StateFailed},
		{From: StateRequiresAction, To: StateCanceled},
		{From: StateRequiresAction, To: StateExpired},

		// Gateway outcomes. PROCESSING may return to REQUIRES_ACTION because some gateways
		// only issue the 3DS challenge after seeing the authorization request.
		{From: StateProcessing, To: StateAuthorized},
		{From: StateProcessing, To: StateCaptured},
		{From: StateProcessing, To: StatePending},
		{From: StateProcessing, To: StateFailed},
		{From: StateProcessing, To: StateRequiresAction},

		// Asynchronous resolution, and the resolution of an unknown outcome by the reconciler.
		{From: StatePending, To: StateAuthorized},
		{From: StatePending, To: StateCaptured},
		{From: StatePending, To: StateFailed},
		{From: StatePending, To: StateExpired},

		// Post-authorization.
		{From: StateAuthorized, To: StateCaptured},
		{From: StateAuthorized, To: StateVoided},
		{From: StateAuthorized, To: StateExpired},
		{From: StateAuthorized, To: StateFailed},

		// Post-capture. Note CAPTURED → SETTLED is driven by a settlement report, not by us.
		{From: StateCaptured, To: StateSettled},
		{From: StateCaptured, To: StatePartiallyRefunded},
		{From: StateCaptured, To: StateRefunded},
		{From: StateCaptured, To: StateDisputed},

		// Refund after settlement is the ordinary case, not an exception.
		{From: StateSettled, To: StatePartiallyRefunded},
		{From: StateSettled, To: StateRefunded},
		{From: StateSettled, To: StateDisputed},

		// Successive partial refunds. The self-transition is declared explicitly: the state
		// machine never permits an implicit self-transition, because an implicit one hides
		// idempotency bugs (a duplicate capture on an already-captured payment would look like
		// it succeeded).
		{From: StatePartiallyRefunded, To: StatePartiallyRefunded},
		{From: StatePartiallyRefunded, To: StateRefunded},
		{From: StatePartiallyRefunded, To: StateDisputed},

		{From: StateRefunded, To: StateDisputed},

		// Dispute resolution: lost (funds reversed) or won (funds restored to their prior
		// position, which is CAPTURED or SETTLED depending on where the payment was).
		{From: StateDisputed, To: StateRefunded},
		{From: StateDisputed, To: StateCaptured},
		{From: StateDisputed, To: StateSettled},
	})

// Machine exposes the payment state machine for the validation plane, the documentation
// generator, the SQL constraint generator and the exhaustive property test.
func Machine() *shared.StateMachine[State] { return machine }

// IsTerminal reports whether the payment can never change state again.
func (s State) IsTerminal() bool { return machine.IsTerminal(s) }

// IsKnown reports whether s is a state this binary understands. A row carrying an unknown
// state means a deployment rolled back over data written by a newer version; that must fail
// loudly rather than be coerced into something plausible.
func (s State) IsKnown() bool { return machine.IsKnown(s) }

// String satisfies fmt.Stringer.
func (s State) String() string { return string(s) }

// IsSuccessful reports whether money has moved or is held in the merchant's favour.
func (s State) IsSuccessful() bool {
	switch s {
	case StateAuthorized, StateCaptured, StateSettled, StatePartiallyRefunded:
		return true
	default:
		return false
	}
}

// IsInFlight reports whether the platform is waiting on an outcome. These are the payments the
// reconciler cares about, and the ones a region failover must resolve rather than retry.
func (s State) IsInFlight() bool {
	return s == StateProcessing || s == StatePending || s == StateRequiresAction
}

// AllowsRefund reports whether a refund could be attempted from this state, subject to the
// amount invariant checked separately.
func (s State) AllowsRefund() bool {
	switch s {
	case StateCaptured, StateSettled, StatePartiallyRefunded:
		return true
	default:
		return false
	}
}

// AttemptOutcome is the result of one gateway interaction.
//
// The four values are not interchangeable, and conflating any two of them causes a specific,
// expensive failure:
//
//   - Conflating DECLINED and ERROR makes the platform fail over on a hard decline, which is
//     indistinguishable from card testing and gets the platform's gateway accounts closed.
//   - Conflating ERROR and TimeoutUnknown makes the platform retry a request that may already
//     have taken the payer's money.
type AttemptOutcome string

const (
	// OutcomePending means the attempt row exists but has not been dispatched. It is written
	// before the gateway call precisely so that a crash between "decided to call" and "called"
	// leaves evidence.
	OutcomePending AttemptOutcome = "PENDING"

	// OutcomeDispatched means the request is in flight.
	OutcomeDispatched AttemptOutcome = "DISPATCHED"

	// OutcomeSuccess means the gateway confirmed the operation.
	OutcomeSuccess AttemptOutcome = "SUCCESS"

	// OutcomeDeclined means the gateway definitively refused. Deterministic.
	OutcomeDeclined AttemptOutcome = "DECLINED"

	// OutcomeError means the call failed before the gateway could have acted, or the gateway
	// returned an error that is unambiguously not an authorization decision. Safe to retry.
	OutcomeError AttemptOutcome = "ERROR"

	// OutcomeTimeoutUnknown means we do not know whether money moved. Never retried
	// automatically; resolved by webhook, by gateway lookup, or by settlement reconciliation.
	OutcomeTimeoutUnknown AttemptOutcome = "TIMEOUT_UNKNOWN"
)

// AllAttemptOutcomes is the complete outcome universe.
var AllAttemptOutcomes = []AttemptOutcome{
	OutcomePending, OutcomeDispatched, OutcomeSuccess,
	OutcomeDeclined, OutcomeError, OutcomeTimeoutUnknown,
}

var attemptMachine = shared.NewStateMachine("payment_attempt", OutcomePending,
	AllAttemptOutcomes,
	[]AttemptOutcome{OutcomeSuccess, OutcomeDeclined, OutcomeError},
	[]shared.Transition[AttemptOutcome]{
		{From: OutcomePending, To: OutcomeDispatched},
		// A pre-dispatch failure (circuit open, credential missing) resolves the attempt
		// without ever reaching the gateway.
		{From: OutcomePending, To: OutcomeError},
		{From: OutcomeDispatched, To: OutcomeSuccess},
		{From: OutcomeDispatched, To: OutcomeDeclined},
		{From: OutcomeDispatched, To: OutcomeError},
		{From: OutcomeDispatched, To: OutcomeTimeoutUnknown},
		// The reconciler is the only thing that may resolve an unknown outcome, and it may
		// resolve it in any direction the gateway reports.
		{From: OutcomeTimeoutUnknown, To: OutcomeSuccess},
		{From: OutcomeTimeoutUnknown, To: OutcomeDeclined},
		{From: OutcomeTimeoutUnknown, To: OutcomeError},
	})

// AttemptMachine exposes the attempt state machine.
func AttemptMachine() *shared.StateMachine[AttemptOutcome] { return attemptMachine }

// IsTerminal reports whether the outcome can still change. TIMEOUT_UNKNOWN is deliberately
// *not* terminal — that is the whole point of it.
func (o AttemptOutcome) IsTerminal() bool { return attemptMachine.IsTerminal(o) }

// String satisfies fmt.Stringer.
func (o AttemptOutcome) String() string { return string(o) }

// PermitsFailover reports whether the orchestrator may create a new attempt on a different
// gateway after this outcome.
//
// This method encodes one of the most consequential rules in the platform:
//
//   - ERROR: yes. The gateway never made a decision, so trying another one is not a second
//     charge attempt on the same instruction.
//   - TIMEOUT_UNKNOWN: no. Money may already have moved. Failing over here is precisely how
//     double charges are created.
//   - DECLINED: it depends on *why*, which is why the decline reason carries the decision
//     rather than the outcome. See DeclineReason.PermitsFailover.
//   - SUCCESS: no.
func (o AttemptOutcome) PermitsFailover() bool { return o == OutcomeError }

// RequiresReconciliation reports whether an attempt in this outcome must be resolved by the
// reconciler before the payment can reach a terminal state.
func (o AttemptOutcome) RequiresReconciliation() bool { return o == OutcomeTimeoutUnknown }

// DeclineReason is the platform's normalized decline taxonomy.
//
// Every gateway has its own decline codes — Stripe has `card_declined` with a dozen
// `decline_code` values, Adyen has refusal reasons, PayPal has something else again. The
// adapters map into this set, and only this set reaches the core. Without normalization the
// routing engine cannot answer "should I retry this elsewhere" without knowing which gateway
// it came from, which is exactly the coupling the adapter pattern exists to prevent.
type DeclineReason string

const (
	// Soft declines: the issuer or gateway might approve the same instruction elsewhere or
	// later. Failover is legitimate.
	DeclineIssuerUnavailable DeclineReason = "ISSUER_UNAVAILABLE"
	DeclineTryAgainLater     DeclineReason = "TRY_AGAIN_LATER"
	DeclineProcessingError   DeclineReason = "PROCESSING_ERROR"
	DeclineDoNotHonorSoft    DeclineReason = "DO_NOT_HONOR"

	// Hard declines: the instruction itself is bad. Retrying elsewhere is, from the schemes'
	// point of view, indistinguishable from card testing, and it will not succeed.
	DeclineInsufficientFunds DeclineReason = "INSUFFICIENT_FUNDS"
	DeclineCardExpired       DeclineReason = "CARD_EXPIRED"
	DeclineIncorrectNumber   DeclineReason = "INCORRECT_NUMBER"
	DeclineIncorrectCVC      DeclineReason = "INCORRECT_CVC"
	DeclineStolenCard        DeclineReason = "STOLEN_CARD"
	DeclineLostCard          DeclineReason = "LOST_CARD"
	DeclineFraudulent        DeclineReason = "FRAUDULENT"
	DeclineRestrictedCard    DeclineReason = "RESTRICTED_CARD"
	DeclineInvalidAccount    DeclineReason = "INVALID_ACCOUNT"
	DeclineCurrencyNotSupp   DeclineReason = "CURRENCY_NOT_SUPPORTED"
	DeclineAuthRequired      DeclineReason = "AUTHENTICATION_REQUIRED"
	DeclineBlockedByRisk     DeclineReason = "BLOCKED_BY_GATEWAY_RISK"
	DeclineUnknown           DeclineReason = "UNKNOWN"
)

// softDeclines is the allowlist of reasons that permit trying another gateway. It is an
// allowlist rather than a blocklist deliberately: a decline reason the adapters have not been
// taught about maps to DeclineUnknown, and an unknown reason must not fail over. Defaulting to
// "retry" on unknown input is how a platform ends up card testing on behalf of an attacker.
var softDeclines = map[DeclineReason]struct{}{
	DeclineIssuerUnavailable: {},
	DeclineTryAgainLater:     {},
	DeclineProcessingError:   {},
	DeclineDoNotHonorSoft:    {},
}

// PermitsFailover reports whether this decline reason allows an attempt on another gateway.
func (d DeclineReason) PermitsFailover() bool {
	_, ok := softDeclines[d]
	return ok
}

// IsHard reports whether the decline is a property of the instruction rather than of the
// route. Hard declines are counted separately in gateway health so that a merchant with a bad
// customer base does not open a circuit breaker on an otherwise healthy gateway.
func (d DeclineReason) IsHard() bool { return !d.PermitsFailover() }

// String satisfies fmt.Stringer.
func (d DeclineReason) String() string { return string(d) }

// CaptureMethod selects whether funds are taken immediately or held for later capture.
type CaptureMethod string

const (
	// CaptureAutomatic authorizes and captures in one gateway call. Correct for digital goods
	// and anything shipped immediately.
	CaptureAutomatic CaptureMethod = "AUTOMATIC"
	// CaptureManual authorizes only, leaving capture to a later explicit call. Correct for
	// physical goods, where capturing before shipment is both a chargeback risk and, in some
	// jurisdictions, a regulatory problem.
	CaptureManual CaptureMethod = "MANUAL"
)

// IsValid reports whether c is a known capture method.
func (c CaptureMethod) IsValid() bool {
	return c == CaptureAutomatic || c == CaptureManual
}
