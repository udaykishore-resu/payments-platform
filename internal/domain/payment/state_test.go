package payment

import (
	"testing"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// declaredPaymentEdges restates the payment transition table from docs/state-machines.md §3.1
// and docs/spec/00-design-baseline.md §9 **by hand**.
//
// It is written out longhand rather than derived from Machine().Edges() on purpose. A property
// test whose expectation is computed from the thing under test proves only that a map round-trips
// through itself; the whole value of this test is that the table in state.go and the table in the
// specification are two independent statements of the same fact, and this file is the place they
// are compared. If a refactor changes state.go, this test must be changed too — deliberately,
// with the specification open.
var declaredPaymentEdges = map[State]map[State]bool{
	// §3.1 #1–#4: dispatch and pre-flight rejection.
	StateCreated: {
		StateProcessing:     true,
		StateRequiresAction: true,
		StateFailed:         true,
		StateCanceled:       true,
	},
	// §3.1 #5–#8: the payer completes, abandons or lets the challenge lapse.
	StateRequiresAction: {
		StateProcessing: true,
		StateFailed:     true,
		StateCanceled:   true,
		StateExpired:    true,
	},
	// §3.1 #9–#13. CAPTURED is directly reachable for sale/auto-capture methods, and a gateway
	// may issue the 3-D Secure challenge only after seeing the authorization request.
	StateProcessing: {
		StateAuthorized:     true,
		StateCaptured:       true,
		StatePending:        true,
		StateFailed:         true,
		StateRequiresAction: true,
	},
	// §3.1 #14–#17: asynchronous resolution, or the reconciler resolving an unknown outcome.
	StatePending: {
		StateAuthorized: true,
		StateCaptured:   true,
		StateFailed:     true,
		StateExpired:    true,
	},
	// §3.1 #18–#21.
	StateAuthorized: {
		StateCaptured: true,
		StateVoided:   true,
		StateExpired:  true,
		StateFailed:   true,
	},
	// §3.1 #22–#25.
	StateCaptured: {
		StateSettled:           true,
		StatePartiallyRefunded: true,
		StateRefunded:          true,
		StateDisputed:          true,
	},
	// §3.1 #26–#28: refund after settlement is the ordinary case, not an exception.
	StateSettled: {
		StatePartiallyRefunded: true,
		StateRefunded:          true,
		StateDisputed:          true,
	},
	// §3.1 #29–#31, including the declared self-loop for successive partial refunds.
	StatePartiallyRefunded: {
		StatePartiallyRefunded: true,
		StateRefunded:          true,
		StateDisputed:          true,
	},
	// §3.1 #32: terminal for money-out, but not for disputes.
	StateRefunded: {
		StateDisputed: true,
	},
	// §3.1 #33–#35: dispute lost, or won pre- or post-settlement.
	StateDisputed: {
		StateRefunded: true,
		StateCaptured: true,
		StateSettled:  true,
	},
	// Terminal: no outgoing edge at all.
	StateVoided:   {},
	StateFailed:   {},
	StateCanceled: {},
	StateExpired:  {},
}

// declaredPaymentEdgeCount is the count the specification states: 35 accepted, 161 rejected out
// of 14 × 14 = 196 ordered pairs.
const declaredPaymentEdgeCount = 35

func TestPaymentMachineAcceptsExactlyTheDeclaredEdges(t *testing.T) {
	// Verifies: the payment FSM in docs/spec/00-design-baseline.md §9 and
	// docs/state-machines.md §3.1, rule 14 of docs/spec/06-code-conventions.md.
	t.Parallel()

	m := Machine()
	if len(m.States()) != len(AllStates) {
		t.Fatalf("machine universe has %d states, AllStates has %d", len(m.States()), len(AllStates))
	}
	if len(declaredPaymentEdges) != len(AllStates) {
		t.Fatalf("the declared table covers %d from-states, AllStates has %d",
			len(declaredPaymentEdges), len(AllStates))
	}

	accepted, rejected := 0, 0
	for _, from := range AllStates {
		for _, to := range AllStates {
			want := declaredPaymentEdges[from][to]
			if want {
				accepted++
			} else {
				rejected++
			}
			if got := m.CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
			err := m.Transition(from, to)
			if want {
				if err != nil {
					t.Errorf("Transition(%s, %s) = %v, want nil", from, to, err)
				}
				continue
			}
			// Every refusal is the same, machine-readable code, so a client can branch on it
			// rather than on the message.
			if apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
				t.Errorf("Transition(%s, %s) code = %s, want INVALID_STATE_TRANSITION",
					from, to, apierror.CodeOf(err))
			}
		}
	}
	if accepted+rejected != len(AllStates)*len(AllStates) {
		t.Fatalf("visited %d pairs, want %d", accepted+rejected, len(AllStates)*len(AllStates))
	}
	if accepted != declaredPaymentEdgeCount {
		t.Errorf("the declared table has %d edges, the specification states %d",
			accepted, declaredPaymentEdgeCount)
	}
	if got := len(m.Edges()); got != declaredPaymentEdgeCount {
		t.Errorf("machine has %d edges, want %d", got, declaredPaymentEdgeCount)
	}
}

func TestPaymentTerminalStates(t *testing.T) {
	t.Parallel()

	terminal := map[State]bool{
		StateFailed: true, StateCanceled: true, StateVoided: true, StateExpired: true,
	}
	for _, s := range AllStates {
		if got := Machine().IsTerminal(s); got != terminal[s] {
			t.Errorf("IsTerminal(%s) = %v, want %v", s, got, terminal[s])
		}
		if got := s.IsTerminal(); got != terminal[s] {
			t.Errorf("State(%s).IsTerminal() = %v, want %v", s, got, terminal[s])
		}
	}
	// SETTLED and REFUNDED are deliberately not terminal. Treating them as terminal makes the
	// refund-after-settlement path — the *normal* path — unrepresentable.
	if StateSettled.IsTerminal() || StateRefunded.IsTerminal() {
		t.Fatal("SETTLED and REFUNDED must not be terminal")
	}
	if !StateCreated.IsKnown() || State("MELTED").IsKnown() {
		t.Fatal("IsKnown does not discriminate declared states")
	}
	if StateCreated.String() != "CREATED" {
		t.Fatalf("String() = %q", StateCreated.String())
	}
}

func TestPaymentMachineRefusesTheTransitionsTheBaselineNames(t *testing.T) {
	// Verifies: docs/spec/00-design-baseline.md §9 "Explicitly invalid", docs/state-machines.md §3.2.
	t.Parallel()

	tests := []struct {
		from State
		to   State
		why  string
	}{
		{StateSettled, StateProcessing,
			"re-dispatching a payment whose funds already moved is a double charge with a settled counterpart"},
		{StateRefunded, StateCaptured,
			"would recreate captured funds that have already gone back to the cardholder"},
		{StateCaptured, StateAuthorized,
			"un-capturing is not an operation any gateway offers; a system that models it will try it"},
		{StateCreated, StateCaptured,
			"must pass through PROCESSING, which is where the attempt row is written before the gateway call"},
		{StatePending, StateProcessing,
			"would re-dispatch a payment whose outcome is unknown"},
		{StateVoided, StateCaptured,
			"the hold is gone; capturing a released authorization is at best a failure"},
	}

	for _, tc := range tests {
		t.Run(tc.from.String()+"_to_"+tc.to.String(), func(t *testing.T) {
			t.Parallel()
			if Machine().CanTransition(tc.from, tc.to) {
				t.Fatalf("%s → %s is permitted: %s", tc.from, tc.to, tc.why)
			}
			if apierror.CodeOf(Machine().Transition(tc.from, tc.to)) != apierror.CodeInvalidStateTransition {
				t.Fatalf("%s → %s: wrong error code", tc.from, tc.to)
			}
		})
	}

	// FAILED → anything. "We told the merchant no" has no exit; any exit re-opens a payment the
	// merchant has already reported to their customer as declined.
	for _, to := range AllStates {
		if Machine().CanTransition(StateFailed, to) {
			t.Errorf("FAILED → %s is permitted; FAILED must have no exit at all", to)
		}
	}
}

func TestPaymentMachineAllowsTheNonObviousLegalTransitions(t *testing.T) {
	// Verifies: docs/state-machines.md §3.1 #26, #29, #34. These are the edges a well-meaning
	// "simplification" removes, because each of them looks wrong until you know why it is there.
	t.Parallel()

	tests := []struct {
		from State
		to   State
		why  string
	}{
		{StateSettled, StateRefunded,
			"refund after settlement is the ordinary case; a merchant refunds days after the money settled"},
		{StateSettled, StatePartiallyRefunded,
			"the partial form of the same ordinary case"},
		{StatePartiallyRefunded, StatePartiallyRefunded,
			"successive partial refunds; the self-transition is declared explicitly because the machine never permits an implicit one"},
		{StateDisputed, StateCaptured,
			"a dispute won pre-settlement returns the payment to where it was"},
		{StateDisputed, StateSettled,
			"a dispute won post-settlement does the same"},
		{StateRefunded, StateDisputed,
			"a refunded payment can still be charged back"},
	}

	for _, tc := range tests {
		t.Run(tc.from.String()+"_to_"+tc.to.String(), func(t *testing.T) {
			t.Parallel()
			if !Machine().CanTransition(tc.from, tc.to) {
				t.Fatalf("%s → %s is refused: %s", tc.from, tc.to, tc.why)
			}
			if err := Machine().Transition(tc.from, tc.to); err != nil {
				t.Fatalf("%s → %s: %v", tc.from, tc.to, err)
			}
		})
	}

	// PARTIALLY_REFUNDED is the *only* declared self-transition. An implicit one anywhere else
	// would hide an idempotency bug: a duplicate capture on an already-captured payment would
	// look like it succeeded.
	for _, s := range AllStates {
		want := s == StatePartiallyRefunded
		if got := Machine().CanTransition(s, s); got != want {
			t.Errorf("CanTransition(%s, %s) = %v, want %v", s, s, got, want)
		}
	}
}

func TestStatePredicatesOverTheFullStateSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state      State
		successful bool
		inFlight   bool
		refundable bool
	}{
		{state: StateCreated},
		{state: StateRequiresAction, inFlight: true},
		{state: StateProcessing, inFlight: true},
		{state: StatePending, inFlight: true},
		{state: StateAuthorized, successful: true},
		{state: StateCaptured, successful: true, refundable: true},
		{state: StateSettled, successful: true, refundable: true},
		{state: StatePartiallyRefunded, successful: true, refundable: true},
		// Fully refunded: the money is back with the payer, so nothing further is refundable and
		// the payment is not "successful" from the merchant's point of view.
		{state: StateRefunded},
		{state: StateVoided},
		{state: StateFailed},
		{state: StateCanceled},
		{state: StateExpired},
		{state: StateDisputed},
	}

	if len(tests) != len(AllStates) {
		t.Fatalf("the predicate table covers %d states, AllStates has %d", len(tests), len(AllStates))
	}
	seen := make(map[State]bool, len(tests))

	for _, tc := range tests {
		t.Run(tc.state.String(), func(t *testing.T) {
			t.Parallel()
			if got := tc.state.IsSuccessful(); got != tc.successful {
				t.Errorf("IsSuccessful() = %v, want %v", got, tc.successful)
			}
			if got := tc.state.IsInFlight(); got != tc.inFlight {
				t.Errorf("IsInFlight() = %v, want %v", got, tc.inFlight)
			}
			if got := tc.state.AllowsRefund(); got != tc.refundable {
				t.Errorf("AllowsRefund() = %v, want %v", got, tc.refundable)
			}
			// A payment that can be refunded must be one where money actually moved; the two
			// predicates cannot drift apart in that direction without producing refunds against
			// authorizations.
			if tc.refundable && !tc.successful {
				t.Error("AllowsRefund is true for a state that is not successful")
			}
			// In flight and terminal are mutually exclusive by construction.
			if tc.state.IsInFlight() && tc.state.IsTerminal() {
				t.Error("a terminal state reports as in flight")
			}
		})
		seen[tc.state] = true
	}
	for _, s := range AllStates {
		if !seen[s] {
			t.Errorf("%s is missing from the predicate table", s)
		}
	}
}

// --- attempt outcomes -------------------------------------------------------------------------

// declaredAttemptEdges restates docs/state-machines.md §4.1 by hand.
var declaredAttemptEdges = map[AttemptOutcome]map[AttemptOutcome]bool{
	// #2 dispatched, #3 a pre-dispatch failure that provably never left the process.
	OutcomePending: {
		OutcomeDispatched: true,
		OutcomeError:      true,
	},
	// #4–#7.
	OutcomeDispatched: {
		OutcomeSuccess:        true,
		OutcomeDeclined:       true,
		OutcomeError:          true,
		OutcomeTimeoutUnknown: true,
	},
	// #8–#10: the reconciler is the only thing that resolves an unknown outcome, and it may
	// resolve it in any direction the gateway reports.
	OutcomeTimeoutUnknown: {
		OutcomeSuccess:  true,
		OutcomeDeclined: true,
		OutcomeError:    true,
	},
	OutcomeSuccess:  {},
	OutcomeDeclined: {},
	OutcomeError:    {},
}

func TestAttemptMachineAcceptsExactlyTheDeclaredEdges(t *testing.T) {
	// Verifies: docs/state-machines.md §4.1.
	t.Parallel()

	m := AttemptMachine()
	if len(m.States()) != len(AllAttemptOutcomes) || len(declaredAttemptEdges) != len(AllAttemptOutcomes) {
		t.Fatalf("universe sizes disagree: machine %d, declared %d, AllAttemptOutcomes %d",
			len(m.States()), len(declaredAttemptEdges), len(AllAttemptOutcomes))
	}

	for _, from := range AllAttemptOutcomes {
		for _, to := range AllAttemptOutcomes {
			want := declaredAttemptEdges[from][to]
			if got := m.CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
			err := m.Transition(from, to)
			if want && err != nil {
				t.Errorf("Transition(%s, %s) = %v, want nil", from, to, err)
			}
			if !want && apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
				t.Errorf("Transition(%s, %s) code = %s, want INVALID_STATE_TRANSITION",
					from, to, apierror.CodeOf(err))
			}
		}
	}

	// TIMEOUT_UNKNOWN is deliberately not terminal — that is the whole point of it. SUCCESS,
	// DECLINED and ERROR are.
	terminal := map[AttemptOutcome]bool{
		OutcomeSuccess: true, OutcomeDeclined: true, OutcomeError: true,
	}
	for _, o := range AllAttemptOutcomes {
		if got := o.IsTerminal(); got != terminal[o] {
			t.Errorf("AttemptOutcome(%s).IsTerminal() = %v, want %v", o, got, terminal[o])
		}
	}
	// DECLINED → anything and SUCCESS → anything are both closed. A correction to a successful
	// attempt is a void or a refund on the payment, never an edit of the attempt.
	for _, to := range AllAttemptOutcomes {
		if m.CanTransition(OutcomeSuccess, to) {
			t.Errorf("SUCCESS → %s is permitted", to)
		}
		if m.CanTransition(OutcomeDeclined, to) {
			t.Errorf("DECLINED → %s is permitted", to)
		}
	}
}

func TestAttemptOutcomeFailoverAndReconciliation(t *testing.T) {
	// Verifies: docs/spec/00-design-baseline.md §9.1, ADR-013.
	t.Parallel()

	tests := []struct {
		outcome        AttemptOutcome
		failover       bool
		reconciliation bool
		why            string
	}{
		{outcome: OutcomePending, why: "nothing has happened yet"},
		{outcome: OutcomeDispatched, why: "the call is still in flight"},
		{outcome: OutcomeSuccess, why: "money moved; a second gateway would be a second charge"},
		{
			outcome: OutcomeDeclined,
			why:     "it depends on why, so the decline reason carries the decision, not the outcome",
		},
		{
			outcome:  OutcomeError,
			failover: true,
			why:      "the gateway never made a decision, so another gateway is not a second charge attempt",
		},
		{
			outcome:        OutcomeTimeoutUnknown,
			reconciliation: true,
			why: "money may already have moved: failing over here is precisely how double charges " +
				"are created, and the outcome is not terminal either",
		},
	}

	if len(tests) != len(AllAttemptOutcomes) {
		t.Fatalf("the table covers %d outcomes, AllAttemptOutcomes has %d", len(tests), len(AllAttemptOutcomes))
	}

	for _, tc := range tests {
		t.Run(tc.outcome.String(), func(t *testing.T) {
			t.Parallel()
			if got := tc.outcome.PermitsFailover(); got != tc.failover {
				t.Errorf("PermitsFailover() = %v, want %v (%s)", got, tc.failover, tc.why)
			}
			if got := tc.outcome.RequiresReconciliation(); got != tc.reconciliation {
				t.Errorf("RequiresReconciliation() = %v, want %v (%s)", got, tc.reconciliation, tc.why)
			}
		})
	}

	// The single rule this whole package exists to protect: an unknown outcome neither permits a
	// retry elsewhere nor terminates the attempt.
	if OutcomeTimeoutUnknown.PermitsFailover() {
		t.Fatal("TIMEOUT_UNKNOWN permits failover")
	}
	if OutcomeTimeoutUnknown.IsTerminal() {
		t.Fatal("TIMEOUT_UNKNOWN is terminal")
	}
	// An outcome the adapters have not been taught about must not permit failover either.
	if AttemptOutcome("WEIRD").PermitsFailover() || AttemptOutcome("").PermitsFailover() {
		t.Fatal("an unrecognised outcome permits failover")
	}
}

func TestDeclineReasonPermitsFailover(t *testing.T) {
	// Verifies: docs/spec/00-design-baseline.md §9.1 — the retryable-decline allowlist.
	t.Parallel()

	tests := []struct {
		reason   DeclineReason
		failover bool
	}{
		// Soft: the issuer or gateway might approve the same instruction elsewhere or later.
		{DeclineIssuerUnavailable, true},
		{DeclineTryAgainLater, true},
		{DeclineProcessingError, true},
		{DeclineDoNotHonorSoft, true},
		// Hard: the instruction itself is bad. Retrying elsewhere is card testing.
		{DeclineInsufficientFunds, false},
		{DeclineCardExpired, false},
		{DeclineIncorrectNumber, false},
		{DeclineIncorrectCVC, false},
		{DeclineStolenCard, false},
		{DeclineLostCard, false},
		{DeclineFraudulent, false},
		{DeclineRestrictedCard, false},
		{DeclineInvalidAccount, false},
		{DeclineCurrencyNotSupp, false},
		{DeclineAuthRequired, false},
		{DeclineBlockedByRisk, false},
		{DeclineUnknown, false},
	}

	for _, tc := range tests {
		t.Run(tc.reason.String(), func(t *testing.T) {
			t.Parallel()
			if got := tc.reason.PermitsFailover(); got != tc.failover {
				t.Errorf("PermitsFailover() = %v, want %v", got, tc.failover)
			}
			// IsHard is the exact complement; the two must not be able to drift.
			if tc.reason.IsHard() == tc.reason.PermitsFailover() {
				t.Errorf("IsHard() and PermitsFailover() agree, which cannot be right")
			}
		})
	}
}

func TestUnrecognisedDeclineReasonDoesNotPermitFailover(t *testing.T) {
	// Verifies: docs/spec/00-design-baseline.md §9.1. The set is an allowlist rather than a
	// blocklist precisely so that this holds. Defaulting to "retry" on unknown input is how a
	// platform ends up card testing on behalf of an attacker.
	t.Parallel()

	for _, r := range []DeclineReason{
		"",
		"unknown",
		"do_not_honor",              // a raw gateway code that was never normalized
		"ISSUER_UNAVAILABLE ",       // a trailing space from a sloppy adapter
		"issuer_unavailable",        // the right reason in the wrong case
		"SOMETHING_NEW_FROM_STRIPE", // a code added upstream that the adapters do not know
	} {
		if r.PermitsFailover() {
			t.Errorf("DeclineReason(%q) permits failover", r)
		}
		if !r.IsHard() {
			t.Errorf("DeclineReason(%q) is not treated as hard", r)
		}
	}
}

// --- refunds ------------------------------------------------------------------------------------

// declaredRefundEdges restates docs/state-machines.md §5 by hand. The document names the first two
// states REQUESTED and PROCESSING; the implementation calls them PENDING and SUBMITTED and adds
// CANCELED for a refund withdrawn before it was ever sent.
var declaredRefundEdges = map[RefundStatus]map[RefundStatus]bool{
	// §5 #2, #3, plus the withdrawal of a refund that never left.
	RefundPending: {
		RefundSubmitted: true,
		RefundFailed:    true,
		RefundCanceled:  true,
	},
	// §5 #4, #5.
	RefundSubmitted: {
		RefundSucceeded: true,
		RefundFailed:    true,
	},
	RefundSucceeded: {},
	RefundFailed:    {},
	RefundCanceled:  {},
}

func TestRefundMachineAcceptsExactlyTheDeclaredEdges(t *testing.T) {
	// Verifies: docs/state-machines.md §5.
	t.Parallel()

	m := RefundMachine()
	if len(m.States()) != len(AllRefundStatuses) || len(declaredRefundEdges) != len(AllRefundStatuses) {
		t.Fatalf("universe sizes disagree: machine %d, declared %d, AllRefundStatuses %d",
			len(m.States()), len(declaredRefundEdges), len(AllRefundStatuses))
	}

	for _, from := range AllRefundStatuses {
		for _, to := range AllRefundStatuses {
			want := declaredRefundEdges[from][to]
			if got := m.CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
			err := m.Transition(from, to)
			if want && err != nil {
				t.Errorf("Transition(%s, %s) = %v, want nil", from, to, err)
			}
			if !want && apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
				t.Errorf("Transition(%s, %s) code = %s, want INVALID_STATE_TRANSITION",
					from, to, apierror.CodeOf(err))
			}
		}
	}

	terminal := map[RefundStatus]bool{
		RefundSucceeded: true, RefundFailed: true, RefundCanceled: true,
	}
	for _, s := range AllRefundStatuses {
		if got := s.IsTerminal(); got != terminal[s] {
			t.Errorf("RefundStatus(%s).IsTerminal() = %v, want %v", s, got, terminal[s])
		}
	}
	// A retry of a failed refund is a *new* refund with a new idempotency key. Re-dispatching the
	// same row risks a double refund if the first failure was misclassified.
	if m.CanTransition(RefundFailed, RefundSubmitted) || m.CanTransition(RefundFailed, RefundPending) {
		t.Fatal("a FAILED refund can be re-dispatched")
	}
	// Money has left; a reversal of a refund is a new capture, which no gateway supports.
	if m.CanTransition(RefundSucceeded, RefundFailed) {
		t.Fatal("SUCCEEDED → FAILED is permitted")
	}
	// A refund cannot skip the gateway: PENDING → SUCCEEDED would let the payment's
	// refundedAmount move without anything having been submitted.
	if m.CanTransition(RefundPending, RefundSucceeded) {
		t.Fatal("PENDING → SUCCEEDED is permitted; a refund may not skip submission")
	}
}

func TestRefundReasonIsValid(t *testing.T) {
	t.Parallel()

	for _, r := range []RefundReason{
		RefundReasonRequestedByCustomer, RefundReasonDuplicate, RefundReasonFraudulent,
		RefundReasonProductUnavailable, RefundReasonServiceNotProvided, RefundReasonPricingError,
		RefundReasonDisputeConceded, RefundReasonOther,
	} {
		if !r.IsValid() {
			t.Errorf("RefundReason(%s).IsValid() = false", r)
		}
		if r.String() != string(r) {
			t.Errorf("String() = %q", r.String())
		}
	}
	for _, r := range []RefundReason{"", "BECAUSE", "requested_by_customer"} {
		if r.IsValid() {
			t.Errorf("RefundReason(%q).IsValid() = true", r)
		}
	}
}

func TestCaptureMethodIsValid(t *testing.T) {
	t.Parallel()

	if !CaptureAutomatic.IsValid() || !CaptureManual.IsValid() {
		t.Fatal("a declared capture method is not valid")
	}
	for _, c := range []CaptureMethod{"", "DEFERRED", "automatic"} {
		if c.IsValid() {
			t.Errorf("CaptureMethod(%q).IsValid() = true", c)
		}
	}
}

func TestEveryPaymentEventTypeIsListedAndShareOneTopic(t *testing.T) {
	t.Parallel()

	seen := make(map[EventType]bool, len(AllEventTypes))
	for _, e := range AllEventTypes {
		if seen[e] {
			t.Fatalf("%s is listed twice in AllEventTypes", e)
		}
		seen[e] = true
		// One topic keyed by payment ID is what gives per-payment ordering. Splitting the events
		// across topics would lose it for no benefit.
		if e.Topic() != "pp.payments.payment.v1" {
			t.Errorf("%s topic = %q", e, e.Topic())
		}
		if e.String() != string(e) {
			t.Errorf("String() = %q", e.String())
		}
	}
	for _, e := range []EventType{
		EventPaymentCreated, EventPaymentAttempted, EventPaymentRequiresAction,
		EventPaymentAuthorized, EventPaymentCaptured, EventPaymentFailed, EventPaymentVoided,
		EventPaymentCanceled, EventPaymentExpired, EventPaymentRefunded, EventPaymentSettled,
		EventPaymentDisputed, EventPaymentDisputeResolved, EventPaymentReconciliationRequired,
	} {
		if !seen[e] {
			t.Errorf("%s is not listed in AllEventTypes", e)
		}
	}

	// Exactly one event type asks for a human: a payment whose outcome we do not know.
	for _, e := range AllEventTypes {
		evt := Event{Type: e}
		want := e == EventPaymentReconciliationRequired
		if got := evt.RequiresOperatorAttention(); got != want {
			t.Errorf("%s RequiresOperatorAttention() = %v, want %v", e, got, want)
		}
	}

	terminalOutcome := map[EventType]bool{
		EventPaymentFailed: true, EventPaymentVoided: true, EventPaymentCanceled: true,
		EventPaymentExpired: true, EventPaymentRefunded: true,
	}
	for _, e := range AllEventTypes {
		if got := (Event{Type: e}).IsTerminalOutcome(); got != terminalOutcome[e] {
			t.Errorf("%s IsTerminalOutcome() = %v, want %v", e, got, terminalOutcome[e])
		}
	}
}
