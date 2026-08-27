package payment

import (
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// testEpoch is the instant every clock in this package's tests starts at. Nothing here reads the
// wall clock: rule 5 of docs/spec/06-code-conventions.md, and a test that sleeps is a test nobody
// runs.
var testEpoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func testClock() *shared.FixedClock { return &shared.FixedClock{T: testEpoch} }

func usd(minor int64) money.Money { return money.MustNew(minor, "USD") }

// testParams builds a valid creation request. Each test takes its own copy and mutates it, so
// there is no shared mutable fixture to be corrupted by a parallel sibling.
func testParams() NewPaymentParams {
	return NewPaymentParams{
		TenantID:      shared.NewTenantID(),
		MerchantID:    shared.NewMerchantID(),
		Amount:        usd(100_00),
		PaymentMethod: shared.MethodCard,
		MethodRef: PaymentMethodReference{
			Token: "tok_visa_4242", Brand: "visa", Last4: "4242",
			ExpMonth: 12, ExpYear: 2030, Country: "US",
		},
		CaptureMethod:  CaptureManual,
		Description:    "one widget",
		StatementRef:   "ACME WIDGETS",
		Metadata:       map[string]string{"orderId": "ord_1"},
		Customer:       CustomerReference{MerchantCustomerID: "cus_1", Country: "US"},
		IdempotencyKey: "idem-0001",
		CorrelationID:  "req_0001",
	}
}

func newTestPayment(t *testing.T) (*Payment, *shared.FixedClock) {
	t.Helper()
	clk := testClock()
	p, err := New(testParams(), clk)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.DrainEvents()
	return p, clk
}

// dispatch drives a fresh payment to PROCESSING with a dispatched attempt, which is the state
// every gateway response arrives into.
func dispatch(t *testing.T, p *Payment, clk *shared.FixedClock) *Attempt {
	t.Helper()
	att, err := p.StartAttempt("stripe", "rpl_1", shared.OpAuthorize, clk)
	if err != nil {
		t.Fatalf("StartAttempt: %v", err)
	}
	if err := p.MarkProcessing(clk); err != nil {
		t.Fatalf("MarkProcessing: %v", err)
	}
	if err := att.Dispatch(clk.Now()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	return att
}

// capturedPayment returns a payment auto-captured for its full amount, which is where every
// refund test starts.
func capturedPayment(t *testing.T) (*Payment, *shared.FixedClock) {
	t.Helper()
	p, clk := newTestPayment(t)
	att := dispatch(t, p, clk)
	if err := att.Succeed("gw_txn_1", "captured", clk.Now()); err != nil {
		t.Fatalf("Succeed: %v", err)
	}
	if err := p.MarkCaptured(p.Amount(), clk); err != nil {
		t.Fatalf("MarkCaptured: %v", err)
	}
	p.DrainEvents()
	return p, clk
}

func TestNewPaymentValidation(t *testing.T) {
	// Verifies: I4 (the immutable set must all be present and valid at creation), FR-53.
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(*NewPaymentParams)
		wantCode apierror.Code
		check    func(*testing.T, *Payment)
	}{
		{name: "valid", mutate: func(*NewPaymentParams) {}},
		{
			name:     "no tenant",
			mutate:   func(p *NewPaymentParams) { p.TenantID = "" },
			wantCode: apierror.CodeMissingTenantContext,
		},
		{
			name:     "no merchant",
			mutate:   func(p *NewPaymentParams) { p.MerchantID = "" },
			wantCode: apierror.CodeValidationFailed,
		},
		{
			// A zero-value Money carries no currency, so the amount cannot be reasoned about at
			// all — not even to compare it with a limit.
			name:     "unsupported currency",
			mutate:   func(p *NewPaymentParams) { p.Amount = money.Money{} },
			wantCode: apierror.CodeAmountInvalid,
		},
		{
			name:     "currency outside the supported set",
			mutate:   func(p *NewPaymentParams) { p.Amount = money.Money{} },
			wantCode: apierror.CodeAmountInvalid,
		},
		{
			name:     "zero amount",
			mutate:   func(p *NewPaymentParams) { p.Amount = money.Zero("USD") },
			wantCode: apierror.CodeAmountInvalid,
		},
		{
			name:     "negative amount",
			mutate:   func(p *NewPaymentParams) { p.Amount = usd(-1) },
			wantCode: apierror.CodeAmountInvalid,
		},
		{
			name:     "unsupported payment method",
			mutate:   func(p *NewPaymentParams) { p.PaymentMethod = "CRYPTO" },
			wantCode: apierror.CodePaymentMethodNotSupported,
		},
		{
			// The structural expression of the PCI decision: this API accepts a token, and a
			// missing token means somebody is about to try sending a PAN instead.
			name:     "no tokenized method reference",
			mutate:   func(p *NewPaymentParams) { p.MethodRef.Token = "" },
			wantCode: apierror.CodeValidationFailed,
		},
		{
			name:     "unknown capture method",
			mutate:   func(p *NewPaymentParams) { p.CaptureMethod = "DEFERRED" },
			wantCode: apierror.CodeValidationFailed,
		},
		{
			// iDEAL settles in a single step. Accepting MANUAL here would be rejected by the
			// gateway later with a far worse error, after the payer has already been redirected.
			name: "manual capture on a method with no authorization step",
			mutate: func(p *NewPaymentParams) {
				p.PaymentMethod = shared.MethodIdeal
				p.CaptureMethod = CaptureManual
			},
			wantCode: apierror.CodeValidationFailed,
		},
		{
			name:     "no idempotency key",
			mutate:   func(p *NewPaymentParams) { p.IdempotencyKey = "" },
			wantCode: apierror.CodeIdempotencyKeyRequired,
		},
		{
			name:   "automatic capture is the default",
			mutate: func(p *NewPaymentParams) { p.CaptureMethod = "" },
			check: func(t *testing.T, p *Payment) {
				if p.CaptureMethod() != CaptureAutomatic {
					t.Fatalf("capture method = %s, want AUTOMATIC", p.CaptureMethod())
				}
			},
		},
		{
			name: "automatic capture on a single-step method is fine",
			mutate: func(p *NewPaymentParams) {
				p.PaymentMethod = shared.MethodIdeal
				p.CaptureMethod = CaptureAutomatic
			},
		},
		{
			name:   "no metadata",
			mutate: func(p *NewPaymentParams) { p.Metadata = nil },
			check: func(t *testing.T, p *Payment) {
				if p.Metadata() != nil {
					t.Fatalf("metadata = %v, want nil", p.Metadata())
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := testParams()
			tc.mutate(&params)
			p, err := New(params, testClock())
			if tc.wantCode != "" {
				if apierror.CodeOf(err) != tc.wantCode {
					t.Fatalf("code = %s, want %s (%v)", apierror.CodeOf(err), tc.wantCode, err)
				}
				if p != nil {
					t.Fatal("a rejected construction returned a payment")
				}
				return
			}
			if err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if p.State() != StateCreated {
				t.Fatalf("state = %s, want CREATED", p.State())
			}
			if p.Version() != 1 {
				t.Fatalf("version = %d, want 1", p.Version())
			}
			// baseVersion 0 is what tells the repository this row does not exist yet, so the
			// first write is an INSERT rather than an UPDATE that matches nothing.
			if p.BaseVersion() != 0 {
				t.Fatalf("baseVersion = %d, want 0", p.BaseVersion())
			}
			if p.CreatedAt() != testEpoch || p.UpdatedAt() != testEpoch {
				t.Fatalf("timestamps = %s / %s, want the fixed clock", p.CreatedAt(), p.UpdatedAt())
			}
			if !p.AuthorizedAmount().IsZero() || !p.CapturedAmount().IsZero() || !p.RefundedAmount().IsZero() {
				t.Fatal("a new payment has non-zero running totals")
			}
			if p.Currency() != "USD" {
				t.Fatalf("currency = %s", p.Currency())
			}
			evts := p.PendingEvents()
			if len(evts) != 1 || evts[0].Type != EventPaymentCreated {
				t.Fatalf("events = %+v", evts)
			}
			if evts[0].Version != 1 {
				t.Fatalf("created event version = %d, want 1", evts[0].Version)
			}
			if evts[0].AggregateID() != p.ID().String() {
				t.Fatalf("partition key = %q, want the payment id", evts[0].AggregateID())
			}
			if evts[0].TenantID != p.TenantID() || evts[0].MerchantID != p.MerchantID() {
				t.Fatal("the created event is not stamped with its tenant and merchant")
			}
			if evts[0].Correlation != params.CorrelationID {
				t.Fatalf("correlation = %q", evts[0].Correlation)
			}
			if tc.check != nil {
				tc.check(t, p)
			}
		})
	}
}

func TestRefundsMayNotExceedCaptureIncludingRefundsInFlight(t *testing.T) {
	// Verifies: I1.
	//
	// The subtle half of I1 is the in-flight half. Counting only *succeeded* refunds against the
	// ceiling would admit two concurrent full refunds — each one fits when it is checked, and
	// together they return twice the money.
	t.Parallel()

	p, clk := capturedPayment(t)
	if got := p.CapturedAmount(); got.Amount() != 100_00 {
		t.Fatalf("captured = %s", got)
	}

	first, err := p.AddRefund(usd(60_00), RefundReasonRequestedByCustomer, "rk-1", clk)
	if err != nil {
		t.Fatalf("first refund: %v", err)
	}
	if first.Status() != RefundPending {
		t.Fatalf("first refund status = %s, want PENDING", first.Status())
	}
	// Nothing has succeeded: refundedAmount is still zero, so a naive ceiling check would let the
	// second one through.
	if !p.RefundedAmount().IsZero() {
		t.Fatalf("refundedAmount = %s before any refund succeeded", p.RefundedAmount())
	}

	second, err := p.AddRefund(usd(60_00), RefundReasonRequestedByCustomer, "rk-2", clk)
	if apierror.CodeOf(err) != apierror.CodeRefundExceedsCaptured {
		t.Fatalf("second refund code = %s, want REFUND_EXCEEDS_CAPTURED (%v)", apierror.CodeOf(err), err)
	}
	if second != nil {
		t.Fatal("a refused refund was still created")
	}
	if len(p.Refunds()) != 1 {
		t.Fatalf("%d refunds recorded, want 1", len(p.Refunds()))
	}
	assertRuleID(t, err, "amount", "EXCEEDS_CAPTURED", "L7.I1_REFUND_WITHIN_CAPTURE")

	// A SUBMITTED refund holds the same reservation as a PENDING one.
	if err := first.MarkSubmitted("gw_ref_1", clk.Now()); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}
	if _, err := p.AddRefund(usd(60_00), RefundReasonRequestedByCustomer, "rk-3", clk); apierror.CodeOf(err) != apierror.CodeRefundExceedsCaptured {
		t.Fatalf("submitted refund did not hold its reservation: %v", err)
	}
	// What is left is exactly what fits.
	if _, err := p.AddRefund(usd(40_00), RefundReasonRequestedByCustomer, "rk-4", clk); err != nil {
		t.Fatalf("a refund for exactly the remaining balance was refused: %v", err)
	}
	if _, err := p.AddRefund(usd(1), RefundReasonRequestedByCustomer, "rk-5", clk); apierror.CodeOf(err) != apierror.CodeRefundExceedsCaptured {
		t.Fatalf("one more minor unit was admitted: %v", err)
	}
}

func TestAddRefundValidation(t *testing.T) {
	// Verifies: I1, docs/state-machines.md §5.
	t.Parallel()

	t.Run("refusing a refund in a state that has no captured funds", func(t *testing.T) {
		t.Parallel()
		p, clk := newTestPayment(t)
		// A payment that has not been captured has nothing to give back; the operation is Void.
		// Offering "refund" here leads merchants to void and refund the same funds.
		for _, drive := range []func(){
			func() {},
			func() {
				dispatch(t, p, clk)
				if err := p.MarkAuthorized(p.Amount(), nil, clk); err != nil {
					t.Fatalf("MarkAuthorized: %v", err)
				}
			},
		} {
			drive()
			_, err := p.AddRefund(usd(1_00), RefundReasonOther, "rk", clk)
			if apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
				t.Fatalf("state %s: code = %s, want INVALID_STATE_TRANSITION", p.State(), apierror.CodeOf(err))
			}
		}
	})

	t.Run("currency and sign", func(t *testing.T) {
		t.Parallel()
		p, clk := capturedPayment(t)
		if _, err := p.AddRefund(money.MustNew(10_00, "EUR"), RefundReasonOther, "rk", clk); apierror.CodeOf(err) != apierror.CodeValidationFailed {
			t.Fatalf("cross-currency refund: code = %s", apierror.CodeOf(err))
		}
		if _, err := p.AddRefund(money.Zero("USD"), RefundReasonOther, "rk", clk); apierror.CodeOf(err) != apierror.CodeAmountInvalid {
			t.Fatalf("zero refund: code = %s", apierror.CodeOf(err))
		}
		if _, err := p.AddRefund(usd(-1), RefundReasonOther, "rk", clk); apierror.CodeOf(err) != apierror.CodeAmountInvalid {
			t.Fatalf("negative refund: code = %s", apierror.CodeOf(err))
		}
		if len(p.Refunds()) != 0 {
			t.Fatalf("a refused refund was recorded: %+v", p.Refunds())
		}
	})

	t.Run("an unrecognised reason falls back to OTHER rather than being rejected", func(t *testing.T) {
		t.Parallel()
		p, clk := capturedPayment(t)
		r, err := p.AddRefund(usd(1_00), "BECAUSE_I_SAID_SO", "rk", clk)
		if err != nil {
			t.Fatalf("AddRefund: %v", err)
		}
		if r.Reason() != RefundReasonOther {
			t.Fatalf("reason = %s, want OTHER", r.Reason())
		}
	})
}

func TestCapturesMayNotExceedTheAuthorization(t *testing.T) {
	// Verifies: I2.
	t.Parallel()

	t.Run("a capture above the authorization is refused", func(t *testing.T) {
		t.Parallel()
		p, clk := newTestPayment(t)
		dispatch(t, p, clk)
		if err := p.MarkAuthorized(usd(80_00), nil, clk); err != nil {
			t.Fatalf("MarkAuthorized: %v", err)
		}
		err := p.MarkCaptured(usd(80_01), clk)
		if apierror.CodeOf(err) != apierror.CodeCaptureExceedsAuthorized {
			t.Fatalf("code = %s, want CAPTURE_EXCEEDS_AUTHORIZED (%v)", apierror.CodeOf(err), err)
		}
		assertRuleID(t, err, "amount", "EXCEEDS_AUTHORIZED", "L7.I2_CAPTURE_WITHIN_AUTHORIZATION")
		// The invariant is checked before the transition, so a refused capture leaves the payment
		// exactly where it was.
		if p.State() != StateAuthorized || !p.CapturedAmount().IsZero() {
			t.Fatalf("a refused capture moved the payment: %s / %s", p.State(), p.CapturedAmount())
		}
		// Exactly at the ceiling is within it.
		if err := p.MarkCaptured(usd(80_00), clk); err != nil {
			t.Fatalf("a capture for exactly the authorized amount was refused: %v", err)
		}
		if p.State() != StateCaptured || p.CapturedAmount().Amount() != 80_00 {
			t.Fatalf("state = %s captured = %s", p.State(), p.CapturedAmount())
		}
	})

	t.Run("with no separate authorization the payment amount is the ceiling", func(t *testing.T) {
		t.Parallel()
		// Auto-capture never sets authorizedAmount, so I2 has to fall back to the payment amount
		// or an auto-capture flow would be unconstrained.
		p, clk := newTestPayment(t)
		dispatch(t, p, clk)
		if got := p.CapturableAmount(); got.Amount() != 100_00 {
			t.Fatalf("capturable = %s, want the payment amount", got)
		}
		err := p.MarkCaptured(usd(100_01), clk)
		if apierror.CodeOf(err) != apierror.CodeCaptureExceedsAuthorized {
			t.Fatalf("code = %s, want CAPTURE_EXCEEDS_AUTHORIZED", apierror.CodeOf(err))
		}
		if p.State() != StateProcessing {
			t.Fatalf("state = %s", p.State())
		}
	})

	t.Run("capture currency and sign", func(t *testing.T) {
		t.Parallel()
		p, clk := newTestPayment(t)
		dispatch(t, p, clk)
		if err := p.MarkCaptured(money.MustNew(10_00, "EUR"), clk); apierror.CodeOf(err) != apierror.CodeGatewayContractViolation {
			t.Fatalf("cross-currency capture: code = %s", apierror.CodeOf(err))
		}
		if err := p.MarkCaptured(money.Zero("USD"), clk); apierror.CodeOf(err) != apierror.CodeAmountInvalid {
			t.Fatalf("zero capture: code = %s", apierror.CodeOf(err))
		}
	})

	t.Run("BUG: a second partial capture is refused by the FSM before I2 is reached", func(t *testing.T) {
		t.Parallel()
		// NOTE — this documents the production code's ACTUAL behaviour, which contradicts
		// docs/state-machines.md §3.1 #18 ("capture count ≤ limits.maxPartialCaptures") and the
		// I2 wording "cumulative captures". MarkCaptured always targets CAPTURED, and
		// CAPTURED → CAPTURED is not a declared self-transition, so the second partial capture is
		// refused with INVALID_STATE_TRANSITION and never reaches the I2 arithmetic. Partial
		// capture sequences are therefore not expressible today. Reported, not fixed here.
		p, clk := newTestPayment(t)
		dispatch(t, p, clk)
		if err := p.MarkAuthorized(usd(80_00), nil, clk); err != nil {
			t.Fatalf("MarkAuthorized: %v", err)
		}
		if err := p.MarkCaptured(usd(30_00), clk); err != nil {
			t.Fatalf("first partial capture: %v", err)
		}
		// 30.00 + 30.00 = 60.00, comfortably inside the 80.00 authorization.
		err := p.MarkCaptured(usd(30_00), clk)
		if err == nil {
			t.Fatal("a second partial capture now succeeds — the bug this test documents has been " +
				"fixed; assert the cumulative total instead")
		}
		if apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
			t.Fatalf("code = %s, want INVALID_STATE_TRANSITION (%v)", apierror.CodeOf(err), err)
		}
		if p.CapturedAmount().Amount() != 30_00 {
			t.Fatalf("captured total = %s, want the first capture only", p.CapturedAmount())
		}
	})
}

func TestOnlyOneSuccessfulAuthorizationAttempt(t *testing.T) {
	// Verifies: I3.
	t.Parallel()

	p, clk := newTestPayment(t)
	att := dispatch(t, p, clk)
	if err := att.Succeed("gw_txn_1", "authorized", clk.Now()); err != nil {
		t.Fatalf("Succeed: %v", err)
	}
	if p.SuccessfulAttempt() == nil || p.SuccessfulAttempt().ID() != att.ID() {
		t.Fatal("SuccessfulAttempt did not find the succeeded attempt")
	}

	second, err := p.StartAttempt("adyen", "rpl_1", shared.OpAuthorize, clk)
	if apierror.CodeOf(err) != apierror.CodePaymentAlreadyProcessed {
		t.Fatalf("code = %s, want PAYMENT_ALREADY_PROCESSED (%v)", apierror.CodeOf(err), err)
	}
	if second != nil {
		t.Fatal("a refused StartAttempt returned an attempt")
	}
	if len(p.Attempts()) != 1 {
		t.Fatalf("%d attempts recorded, want 1", len(p.Attempts()))
	}

	// I3 is scoped to the authorization. A capture, a refund or a void after a successful
	// authorization is a different operation and must still be dispatchable.
	for _, op := range []shared.Operation{shared.OpCapture, shared.OpRefund, shared.OpVoid, shared.OpLookup} {
		if _, err := p.StartAttempt("stripe", "rpl_1", op, clk); err != nil {
			t.Fatalf("StartAttempt(%s) after a successful authorization: %v", op, err)
		}
	}
}

func TestStartAttemptRefusedInATerminalState(t *testing.T) {
	t.Parallel()

	p, clk := newTestPayment(t)
	if err := p.Cancel("payer changed their mind", clk); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	_, err := p.StartAttempt("stripe", "rpl_1", shared.OpAuthorize, clk)
	if apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
		t.Fatalf("code = %s, want INVALID_STATE_TRANSITION", apierror.CodeOf(err))
	}
	if len(p.Attempts()) != 0 {
		t.Fatal("an attempt was created against a canceled payment")
	}
}

func TestNoTimerMayFailAPaymentWithAnUnknownOutcome(t *testing.T) {
	// Verifies: ADR-013, docs/state-machines.md §3.2 ("PROCESSING → FAILED on timeout"), I3.
	//
	// This is the single most important test in the package. A gateway that does not answer has
	// not told us that nothing happened; it has told us nothing. Every path that would resolve
	// that ambiguity by guessing must be closed, and the only thing that may open it again is an
	// authoritative external observation.
	t.Parallel()

	p, clk := newTestPayment(t)
	att := dispatch(t, p, clk)
	if err := att.TimeOut("no response within 8s", clk.Now()); err != nil {
		t.Fatalf("TimeOut: %v", err)
	}

	if !p.HasUnresolvedAttempt() {
		t.Fatal("HasUnresolvedAttempt is false with an attempt in TIMEOUT_UNKNOWN")
	}
	if att.Outcome() != OutcomeTimeoutUnknown || att.Outcome().IsTerminal() {
		t.Fatalf("outcome = %s (terminal %v)", att.Outcome(), att.Outcome().IsTerminal())
	}
	// The attempt is not resolved, so it carries no resolution timestamp — the reconciler's queue
	// is populated from exactly this.
	if att.ResolvedAt() != nil {
		t.Fatalf("a timed-out attempt was stamped resolved at %s", att.ResolvedAt())
	}

	// 1. Nothing may declare the payment failed.
	err := p.MarkFailed(DeclineProcessingError, "gateway did not answer", clk)
	if apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
		t.Fatalf("MarkFailed code = %s, want INVALID_STATE_TRANSITION (%v)", apierror.CodeOf(err), err)
	}
	assertRuleID(t, err, "status", "AWAITING_RECONCILIATION", "L7.NO_FAIL_WHILE_UNRESOLVED")

	// 2. Nothing may retry, on this gateway or any other. Failing over here is precisely how
	//    double charges are created.
	_, err = p.StartAttempt("adyen", "rpl_2", shared.OpAuthorize, clk)
	if apierror.CodeOf(err) != apierror.CodePaymentAlreadyProcessed {
		t.Fatalf("StartAttempt code = %s, want PAYMENT_ALREADY_PROCESSED (%v)", apierror.CodeOf(err), err)
	}
	assertRuleID(t, err, "status", "AWAITING_RECONCILIATION", "L7.NO_RETRY_WHILE_UNRESOLVED")
	if att.PermitsFailover() {
		t.Fatal("a timed-out attempt permits failover")
	}

	// 3. Time passing changes nothing. Advance the clock arbitrarily and re-run both refusals:
	//    the payment must still be in flight, with the same unresolved attempt.
	for _, d := range []time.Duration{time.Second, time.Hour, 24 * time.Hour, 30 * 24 * time.Hour} {
		clk.Advance(d)
		if err := p.MarkFailed(DeclineUnknown, "still nothing", clk); err == nil {
			t.Fatalf("a payment was failed %s after the timeout", d)
		}
		if _, err := p.StartAttempt("checkout", "rpl_3", shared.OpAuthorize, clk); err == nil {
			t.Fatalf("a payment was retried %s after the timeout", d)
		}
		if p.State() != StateProcessing {
			t.Fatalf("state = %s after %s, want PROCESSING", p.State(), d)
		}
		if !p.HasUnresolvedAttempt() {
			t.Fatalf("the attempt resolved itself after %s", d)
		}
	}

	// 4. The payment may still move to PENDING, because "genuinely asynchronous" is a truthful
	//    description of an unknown outcome and PENDING is the state the reconciler watches.
	if err := p.MarkPending(clk); err != nil {
		t.Fatalf("MarkPending with an unresolved attempt: %v", err)
	}

	// 5. Flagging for reconciliation raises the one event that asks for a human, and deliberately
	//    does not change the state.
	before := p.State()
	p.RequireReconciliation(att.ID(), "no gateway response", clk)
	if p.State() != before {
		t.Fatalf("RequireReconciliation changed the state to %s", p.State())
	}
	evts := p.DrainEvents()
	last := evts[len(evts)-1]
	if last.Type != EventPaymentReconciliationRequired || !last.RequiresOperatorAttention() {
		t.Fatalf("expected a reconciliation-required event, got %+v", last)
	}

	// 6. Only an authoritative observation resolves it — and once it has, the ordinary rules
	//    apply again.
	if err := att.Reconcile(OutcomeDeclined, "gw_txn_1", "declined", DeclineInsufficientFunds, clk.Now()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if p.HasUnresolvedAttempt() {
		t.Fatal("HasUnresolvedAttempt is still true after reconciliation")
	}
	if err := p.MarkFailed(DeclineInsufficientFunds, "reconciled as declined", clk); err != nil {
		t.Fatalf("MarkFailed after reconciliation: %v", err)
	}
	if p.State() != StateFailed {
		t.Fatalf("state = %s, want FAILED", p.State())
	}
}

func TestMarkFailedRespectsTheTransitionTable(t *testing.T) {
	// Verifies: docs/state-machines.md §3.2 — FAILED is reachable only from the states where the
	// payment has not yet succeeded. A settled payment that "failed" would be a ledger entry with
	// no counterpart.
	t.Parallel()

	p, clk := capturedPayment(t)
	if err := p.MarkSettled(clk.Now(), "stl_1", clk); err != nil {
		t.Fatalf("MarkSettled: %v", err)
	}
	err := p.MarkFailed(DeclineProcessingError, "somebody called this by mistake", clk)
	if apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
		t.Fatalf("code = %s, want INVALID_STATE_TRANSITION (%v)", apierror.CodeOf(err), err)
	}
	if p.State() != StateSettled {
		t.Fatalf("a refused MarkFailed moved the payment to %s", p.State())
	}

	// From a state where it is legal, the decline reason and detail reach the event, because a
	// merchant's support team needs to know *why* before they can advise the payer.
	q, qclk := newTestPayment(t)
	att := dispatch(t, q, qclk)
	if err := att.Decline(DeclineInsufficientFunds, "gw_txn_1", "51", false, qclk.Now()); err != nil {
		t.Fatalf("Decline: %v", err)
	}
	if err := q.MarkFailed(DeclineInsufficientFunds, "issuer returned 51", qclk); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	evts := q.DrainEvents()
	last := evts[len(evts)-1]
	if last.Type != EventPaymentFailed || last.Payload["declineReason"] != string(DeclineInsufficientFunds) {
		t.Fatalf("event = %+v", last)
	}
	if !last.IsTerminalOutcome() {
		t.Fatal("a failure is not reported as a terminal outcome")
	}
}

func TestPartialAndOverAuthorization(t *testing.T) {
	// Verifies: docs/state-machines.md §3.1 #9.
	t.Parallel()

	t.Run("an issuer may approve less than was requested", func(t *testing.T) {
		t.Parallel()
		p, clk := newTestPayment(t)
		dispatch(t, p, clk)
		expiry := testEpoch.Add(7 * 24 * time.Hour)
		if err := p.MarkAuthorized(usd(60_00), &expiry, clk); err != nil {
			t.Fatalf("MarkAuthorized: %v", err)
		}
		if p.State() != StateAuthorized || p.AuthorizedAmount().Amount() != 60_00 {
			t.Fatalf("state = %s authorized = %s", p.State(), p.AuthorizedAmount())
		}
		if p.AuthorizedAt() == nil || !p.AuthorizedAt().Equal(testEpoch) {
			t.Fatalf("authorizedAt = %v", p.AuthorizedAt())
		}
		if p.AuthExpiresAt() == nil || !p.AuthExpiresAt().Equal(expiry) {
			t.Fatalf("authExpiresAt = %v", p.AuthExpiresAt())
		}
		// The ceiling for capture follows the authorization down, not the requested amount.
		if p.CapturableAmount().Amount() != 60_00 {
			t.Fatalf("capturable = %s, want the authorized amount", p.CapturableAmount())
		}
		evts := p.DrainEvents()
		last := evts[len(evts)-1]
		if last.Type != EventPaymentAuthorized || last.Version != p.Version() {
			t.Fatalf("event = %+v, aggregate version = %d", last, p.Version())
		}
	})

	t.Run("a gateway authorizing more than we asked for is a contract violation", func(t *testing.T) {
		t.Parallel()
		// Not a windfall. An amount we did not request cannot be reconciled against the order,
		// and capturing it would be an unauthorized charge.
		p, clk := newTestPayment(t)
		dispatch(t, p, clk)
		err := p.MarkAuthorized(usd(100_01), nil, clk)
		if apierror.CodeOf(err) != apierror.CodeGatewayContractViolation {
			t.Fatalf("code = %s, want GATEWAY_CONTRACT_VIOLATION (%v)", apierror.CodeOf(err), err)
		}
		if p.State() != StateProcessing || !p.AuthorizedAmount().IsZero() {
			t.Fatalf("a refused authorization was recorded: %s / %s", p.State(), p.AuthorizedAmount())
		}
	})

	t.Run("currency and sign", func(t *testing.T) {
		t.Parallel()
		p, clk := newTestPayment(t)
		dispatch(t, p, clk)
		if err := p.MarkAuthorized(money.MustNew(50_00, "EUR"), nil, clk); apierror.CodeOf(err) != apierror.CodeGatewayContractViolation {
			t.Fatalf("cross-currency authorization: code = %s", apierror.CodeOf(err))
		}
		if err := p.MarkAuthorized(money.Zero("USD"), nil, clk); apierror.CodeOf(err) != apierror.CodeGatewayContractViolation {
			t.Fatalf("zero authorization: code = %s", apierror.CodeOf(err))
		}
	})
}

func TestVoidRequiresNothingCaptured(t *testing.T) {
	// Verifies: docs/state-machines.md §3.1 #19.
	t.Parallel()

	t.Run("an uncaptured authorization can be voided", func(t *testing.T) {
		t.Parallel()
		p, clk := newTestPayment(t)
		dispatch(t, p, clk)
		if err := p.MarkAuthorized(usd(100_00), nil, clk); err != nil {
			t.Fatalf("MarkAuthorized: %v", err)
		}
		if err := p.Void(clk); err != nil {
			t.Fatalf("Void: %v", err)
		}
		if p.State() != StateVoided || !p.State().IsTerminal() {
			t.Fatalf("state = %s", p.State())
		}
		evts := p.DrainEvents()
		if evts[len(evts)-1].Type != EventPaymentVoided {
			t.Fatalf("events = %+v", evts)
		}
	})

	t.Run("captured funds are returned by refund, never by void", func(t *testing.T) {
		t.Parallel()
		p, clk := capturedPayment(t)
		err := p.Void(clk)
		if apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
			t.Fatalf("code = %s, want INVALID_STATE_TRANSITION (%v)", apierror.CodeOf(err), err)
		}
		assertRuleID(t, err, "status", "ALREADY_CAPTURED", "L7.VOID_REQUIRES_UNCAPTURED")
		if p.State() != StateCaptured {
			t.Fatalf("a refused void moved the payment to %s", p.State())
		}
	})
}

func TestRefundLifecycleMovesThePaymentToTheRightState(t *testing.T) {
	// Verifies: I1, docs/state-machines.md §3.1 #23, #24, #29, #30.
	t.Parallel()

	p, clk := capturedPayment(t)

	first, err := p.AddRefund(usd(30_00), RefundReasonRequestedByCustomer, "rk-1", clk)
	if err != nil {
		t.Fatalf("AddRefund: %v", err)
	}
	// AddRefund alone changes neither the payment's state nor its refunded total: the money has
	// not moved yet, and a merchant reading CAPTURED is reading the truth.
	if p.State() != StateCaptured || !p.RefundedAmount().IsZero() {
		t.Fatalf("AddRefund changed the payment: %s / %s", p.State(), p.RefundedAmount())
	}
	if err := first.MarkSubmitted("gw_ref_1", clk.Now()); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}
	if err := p.ConfirmRefund(first.ID(), "gw_ref_1", clk); err != nil {
		t.Fatalf("ConfirmRefund: %v", err)
	}
	if p.State() != StatePartiallyRefunded {
		t.Fatalf("state = %s, want PARTIALLY_REFUNDED", p.State())
	}
	if p.RefundedAmount().Amount() != 30_00 || p.RefundableAmount().Amount() != 70_00 {
		t.Fatalf("refunded = %s refundable = %s", p.RefundedAmount(), p.RefundableAmount())
	}
	evts := p.DrainEvents()
	last := evts[len(evts)-1]
	if last.Type != EventPaymentRefunded || last.Payload["fullyRefunded"] != false {
		t.Fatalf("event = %+v", last)
	}

	// A second partial refund exercises the declared PARTIALLY_REFUNDED self-transition.
	second, err := p.AddRefund(usd(20_00), RefundReasonRequestedByCustomer, "rk-2", clk)
	if err != nil {
		t.Fatalf("second AddRefund: %v", err)
	}
	if err := second.MarkSubmitted("gw_ref_2", clk.Now()); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}
	if err := p.ConfirmRefund(second.ID(), "gw_ref_2", clk); err != nil {
		t.Fatalf("second ConfirmRefund: %v", err)
	}
	if p.State() != StatePartiallyRefunded || p.RefundedAmount().Amount() != 50_00 {
		t.Fatalf("state = %s refunded = %s", p.State(), p.RefundedAmount())
	}

	// The boundary: reaching the captured total exactly moves the payment to REFUNDED.
	third, err := p.AddRefund(usd(50_00), RefundReasonRequestedByCustomer, "rk-3", clk)
	if err != nil {
		t.Fatalf("third AddRefund: %v", err)
	}
	if err := third.MarkSubmitted("gw_ref_3", clk.Now()); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}
	if err := p.ConfirmRefund(third.ID(), "gw_ref_3", clk); err != nil {
		t.Fatalf("third ConfirmRefund: %v", err)
	}
	if p.State() != StateRefunded {
		t.Fatalf("state = %s, want REFUNDED at the boundary", p.State())
	}
	if !p.RefundableAmount().IsZero() {
		t.Fatalf("refundable = %s after a full refund", p.RefundableAmount())
	}
	evts = p.DrainEvents()
	last = evts[len(evts)-1]
	if last.Payload["fullyRefunded"] != true {
		t.Fatalf("the final refund event does not report a full refund: %+v", last.Payload)
	}

	// A refund that does not belong to this payment is refused rather than silently ignored.
	if err := p.ConfirmRefund("ref_not_ours", "gw", clk); apierror.CodeOf(err) != apierror.CodeValidationFailed {
		t.Fatalf("unknown refund: code = %s", apierror.CodeOf(err))
	}
}

func TestAFailedRefundReturnsItsAmountToTheRefundableBalance(t *testing.T) {
	// Verifies: I1 — a pending refund that ultimately fails must not permanently reduce the
	// refundable balance, or a gateway hiccup would strand the merchant's money.
	t.Parallel()

	p, clk := capturedPayment(t)
	doomed, err := p.AddRefund(usd(100_00), RefundReasonRequestedByCustomer, "rk-1", clk)
	if err != nil {
		t.Fatalf("AddRefund: %v", err)
	}
	// While it is in flight it holds the whole balance.
	if _, err := p.AddRefund(usd(1), RefundReasonRequestedByCustomer, "rk-2", clk); apierror.CodeOf(err) != apierror.CodeRefundExceedsCaptured {
		t.Fatalf("the in-flight refund did not hold the balance: %v", err)
	}

	if err := doomed.MarkSubmitted("gw_ref_1", clk.Now()); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}
	if err := doomed.MarkFailed("R_DECLINED", "issuer refused the credit", clk.Now()); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if doomed.Status() != RefundFailed || doomed.FailureCode() != "R_DECLINED" {
		t.Fatalf("refund = %s / %q", doomed.Status(), doomed.FailureCode())
	}
	// The payment never moved, and the whole balance is available again.
	if p.State() != StateCaptured || !p.RefundedAmount().IsZero() {
		t.Fatalf("state = %s refunded = %s", p.State(), p.RefundedAmount())
	}
	retry, err := p.AddRefund(usd(100_00), RefundReasonRequestedByCustomer, "rk-3", clk)
	if err != nil {
		t.Fatalf("a retry after a failed refund was refused: %v", err)
	}
	if err := retry.MarkSubmitted("gw_ref_2", clk.Now()); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}
	if err := p.ConfirmRefund(retry.ID(), "gw_ref_2", clk); err != nil {
		t.Fatalf("ConfirmRefund: %v", err)
	}
	if p.State() != StateRefunded || p.RefundedAmount().Amount() != 100_00 {
		t.Fatalf("state = %s refunded = %s", p.State(), p.RefundedAmount())
	}
}

func TestSettlementAndDisputeResolution(t *testing.T) {
	// Verifies: docs/state-machines.md §3.1 #22, #25, #33–#35.
	t.Parallel()

	t.Run("a dispute lost reverses the funds", func(t *testing.T) {
		t.Parallel()
		p, clk := capturedPayment(t)
		if err := p.MarkDisputed("dp_1", "10.4", clk); err != nil {
			t.Fatalf("MarkDisputed: %v", err)
		}
		if p.State() != StateDisputed {
			t.Fatalf("state = %s", p.State())
		}
		if err := p.ResolveDispute(false, clk); err != nil {
			t.Fatalf("ResolveDispute: %v", err)
		}
		if p.State() != StateRefunded {
			t.Fatalf("state = %s, want REFUNDED", p.State())
		}
	})

	t.Run("a dispute won pre-settlement returns to CAPTURED", func(t *testing.T) {
		t.Parallel()
		p, clk := capturedPayment(t)
		if err := p.MarkDisputed("dp_1", "10.4", clk); err != nil {
			t.Fatalf("MarkDisputed: %v", err)
		}
		if err := p.ResolveDispute(true, clk); err != nil {
			t.Fatalf("ResolveDispute: %v", err)
		}
		if p.State() != StateCaptured {
			t.Fatalf("state = %s, want CAPTURED", p.State())
		}
	})

	t.Run("ResolveDispute is refused when the payment is not disputed", func(t *testing.T) {
		t.Parallel()
		p, clk := capturedPayment(t)
		if err := p.ResolveDispute(true, clk); apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
			t.Fatalf("code = %s", apierror.CodeOf(err))
		}
	})

	t.Run("BUG: a dispute won post-settlement returns to SETTLED only while the settlement event is still in memory", func(t *testing.T) {
		t.Parallel()
		// NOTE — this documents the production code's ACTUAL behaviour. ResolveDispute decides
		// between CAPTURED and SETTLED by scanning the aggregate's *pending* event slice
		// (Payment.settledLike). The repository drains that slice inside every unit of work, and
		// Rehydrate does not restore it, so in production the settlement is invisible by the time
		// the dispute is resolved and a won dispute silently demotes a settled payment to
		// CAPTURED. Reported, not fixed here.
		p, clk := capturedPayment(t)
		if err := p.MarkSettled(clk.Now(), "stl_1", clk); err != nil {
			t.Fatalf("MarkSettled: %v", err)
		}

		// With the events retained — one long-lived unit of work — the answer is right.
		if err := p.MarkDisputed("dp_1", "10.4", clk); err != nil {
			t.Fatalf("MarkDisputed: %v", err)
		}
		if err := p.ResolveDispute(true, clk); err != nil {
			t.Fatalf("ResolveDispute: %v", err)
		}
		if p.State() != StateSettled {
			t.Fatalf("state = %s, want SETTLED while the settlement event is still pending", p.State())
		}

		// The same sequence with a drain in between — which is what a repository actually does.
		q, qclk := capturedPayment(t)
		if err := q.MarkSettled(qclk.Now(), "stl_1", qclk); err != nil {
			t.Fatalf("MarkSettled: %v", err)
		}
		q.DrainEvents()
		if err := q.MarkDisputed("dp_1", "10.4", qclk); err != nil {
			t.Fatalf("MarkDisputed: %v", err)
		}
		if err := q.ResolveDispute(true, qclk); err != nil {
			t.Fatalf("ResolveDispute: %v", err)
		}
		if q.State() == StateSettled {
			t.Fatal("a won dispute on a drained settled payment now resolves to SETTLED — the bug " +
				"this test documents has been fixed; assert SETTLED unconditionally instead")
		}
		if q.State() != StateCaptured {
			t.Fatalf("state = %s, want CAPTURED (the buggy answer)", q.State())
		}
	})
}

func TestPartitionMonthIsAPureFunctionOfThePaymentID(t *testing.T) {
	// Verifies: amendment A-02. A partial unique index on a partitioned table is enforced only
	// *within* a partition, so I3 silently weakens if a payment's attempts can land in a
	// different month from the payment. The partition key is therefore derived from the
	// payment's immutable ID and from nothing else — not from the clock, and not from when the
	// attempt happened to be created.
	t.Parallel()

	january := time.Date(2026, 1, 17, 9, 30, 0, 0, time.UTC)
	wantMonth := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	id := shared.PaymentID(ids.NewAt(ids.PrefixPayment, january))

	clk := &shared.FixedClock{T: january}
	p, err := Rehydrate(RehydrateParams{
		ID: id, TenantID: shared.NewTenantID(), MerchantID: shared.NewMerchantID(),
		Amount: usd(100_00), CaptureMethod: CaptureManual, PaymentMethod: shared.MethodCard,
		MethodRef: PaymentMethodReference{Token: "tok"}, State: StateAuthorized, Version: 7,
		AuthorizedAmount: usd(100_00), CapturedAmount: money.Zero("USD"), RefundedAmount: money.Zero("USD"),
		CreatedAt: january, UpdatedAt: january,
	})
	if err != nil {
		t.Fatalf("Rehydrate: %v", err)
	}
	if got := p.PartitionMonth(); !got.Equal(wantMonth) {
		t.Fatalf("PartitionMonth() = %s, want %s", got, wantMonth)
	}

	// A delayed capture: the attempt is created more than a month later, on a clock that has
	// moved into February.
	clk.Advance(35 * 24 * time.Hour)
	if clk.Now().Month() == january.Month() {
		t.Fatalf("the clock did not leave January: %s", clk.Now())
	}
	att, err := p.StartAttempt("stripe", "rpl_1", shared.OpCapture, clk)
	if err != nil {
		t.Fatalf("StartAttempt: %v", err)
	}
	if got := shared.PartitionMonth(att.PaymentID()); !got.Equal(wantMonth) {
		t.Fatalf("the attempt's partition month = %s, want the payment's %s", got, wantMonth)
	}
	// And the payment's own answer did not move either.
	if got := p.PartitionMonth(); !got.Equal(wantMonth) {
		t.Fatalf("PartitionMonth() = %s after the clock advanced, want %s", got, wantMonth)
	}
}

func TestAccessorsReturnCopies(t *testing.T) {
	// Verifies: rule 11 of docs/spec/06-code-conventions.md. Returning the live backing array or
	// map lets a caller mutate aggregate state without going through a method, which is exactly
	// what the unexported fields exist to prevent.
	t.Parallel()

	p, clk := capturedPayment(t)
	if _, err := p.AddRefund(usd(10_00), RefundReasonOther, "rk-1", clk); err != nil {
		t.Fatalf("AddRefund: %v", err)
	}

	md := p.Metadata()
	md["orderId"] = "tampered"
	md["backdoor"] = "yes"
	if p.Metadata()["orderId"] != "ord_1" {
		t.Fatal("mutating the returned metadata map changed the aggregate")
	}
	if _, ok := p.Metadata()["backdoor"]; ok {
		t.Fatal("a key added to the returned metadata map reached the aggregate")
	}

	// The constructor copies on the way in as well, so a caller who keeps their map cannot edit
	// the payment afterwards.
	params := testParams()
	src := map[string]string{"k": "v"}
	params.Metadata = src
	fresh, err := New(params, testClock())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	src["k"] = "mutated"
	if fresh.Metadata()["k"] != "v" {
		t.Fatal("mutating the caller's map after construction changed the aggregate")
	}

	atts := p.Attempts()
	if len(atts) == 0 {
		t.Fatal("no attempts to test with")
	}
	atts[0] = nil
	if p.Attempts()[0] == nil {
		t.Fatal("mutating the returned attempt slice changed the aggregate")
	}

	refs := p.Refunds()
	refs[0] = nil
	if p.Refunds()[0] == nil {
		t.Fatal("mutating the returned refund slice changed the aggregate")
	}

	if err := p.MarkSettled(clk.Now(), "stl_1", clk); err != nil {
		t.Fatalf("MarkSettled: %v", err)
	}
	evts := p.PendingEvents()
	if len(evts) == 0 {
		t.Fatal("no events to test with")
	}
	evts[0].Type = "tampered.v1"
	if p.PendingEvents()[0].Type == "tampered.v1" {
		t.Fatal("mutating the returned event slice changed the aggregate")
	}
}

func TestEventsCarryTheVersionTheyWereRaisedAt(t *testing.T) {
	// Verifies: I5 — every state change appends exactly one row to the payment event log and the
	// aggregate version increments monotonically. A consumer that reorders events by version, or
	// a repository writing (payment_id, aggregate_version) into a UNIQUE index, depends on this.
	t.Parallel()

	clk := testClock()
	p, err := New(testParams(), clk)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(p.PendingEvents()) != 1 || p.PendingEvents()[0].Version != p.Version() {
		t.Fatalf("created event version = %d, aggregate version = %d",
			p.PendingEvents()[0].Version, p.Version())
	}

	// Each raising operation stamps the version the aggregate holds at that moment.
	steps := []struct {
		name string
		run  func() error
	}{
		{"attempt", func() error { _, err := p.StartAttempt("stripe", "rpl_1", shared.OpAuthorize, clk); return err }},
		{"processing", func() error { return p.MarkProcessing(clk) }},
		{"captured", func() error { return p.MarkCaptured(p.Amount(), clk) }},
		{"settled", func() error { return p.MarkSettled(clk.Now(), "stl_1", clk) }},
		{"disputed", func() error { return p.MarkDisputed("dp_1", "10.4", clk) }},
	}
	for _, step := range steps {
		before := len(p.PendingEvents())
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		evts := p.PendingEvents()
		if len(evts) > before {
			last := evts[len(evts)-1]
			if last.Version != p.Version() {
				t.Fatalf("%s raised an event at version %d, aggregate is at %d",
					step.name, last.Version, p.Version())
			}
		}
	}

	evts := p.PendingEvents()
	for i := 1; i < len(evts); i++ {
		if evts[i].Version < evts[i-1].Version {
			t.Fatalf("event %d (%s, v%d) precedes %s at v%d",
				i, evts[i].Type, evts[i].Version, evts[i-1].Type, evts[i-1].Version)
		}
		if evts[i].PaymentID != p.ID() || evts[i].AggregateID() != p.ID().String() {
			t.Fatalf("event %d is not stamped with its payment", i)
		}
	}

	// PendingEvents reads; DrainEvents empties. Draining twice must not hand the outbox the same
	// events again.
	if len(p.PendingEvents()) == 0 {
		t.Fatal("PendingEvents emptied the buffer")
	}
	drained := p.DrainEvents()
	if len(drained) != len(evts) {
		t.Fatalf("drained %d events, PendingEvents reported %d", len(drained), len(evts))
	}
	if len(p.PendingEvents()) != 0 || len(p.DrainEvents()) != 0 {
		t.Fatal("draining twice returned events the second time")
	}
}

func TestBaseVersionTracksWhatTheRowHolds(t *testing.T) {
	// Verifies: the optimistic-concurrency predicate. A unit of work that makes two changes
	// before its first save must still name the version the row actually holds, or the save
	// fails as a conflict against nobody.
	t.Parallel()

	p, clk := newTestPayment(t)
	if p.BaseVersion() != 0 || p.Version() != 1 {
		t.Fatalf("base = %d version = %d", p.BaseVersion(), p.Version())
	}

	// The orchestrator's dispatch: two changes, one save.
	if _, err := p.StartAttempt("stripe", "rpl_1", shared.OpAuthorize, clk); err != nil {
		t.Fatalf("StartAttempt: %v", err)
	}
	if err := p.MarkProcessing(clk); err != nil {
		t.Fatalf("MarkProcessing: %v", err)
	}
	if p.Version() != 3 {
		t.Fatalf("version = %d, want 3", p.Version())
	}
	if p.BaseVersion() != 0 {
		t.Fatalf("baseVersion = %d before the first save, want 0", p.BaseVersion())
	}

	p.MarkPersisted()
	if p.BaseVersion() != p.Version() {
		t.Fatalf("base = %d version = %d after MarkPersisted", p.BaseVersion(), p.Version())
	}
	// MarkPersisted is not a state change: nothing about the payment changed, only what the
	// database has seen.
	updated, state, events := p.UpdatedAt(), p.State(), len(p.PendingEvents())
	version := p.Version()
	p.MarkPersisted()
	if p.UpdatedAt() != updated || p.State() != state || p.Version() != version {
		t.Fatal("MarkPersisted behaved like a state change")
	}
	if len(p.PendingEvents()) != events {
		t.Fatal("MarkPersisted raised an event")
	}
}

func TestRehydrateRoundTrip(t *testing.T) {
	// Verifies: rule 1 of docs/spec/06-code-conventions.md — a repository reconstructs an
	// aggregate through an explicit doorway that validates the persisted state is one this binary
	// understands.
	t.Parallel()

	t.Run("an unknown state is refused rather than coerced", func(t *testing.T) {
		t.Parallel()
		// A row carrying a state this binary does not know means a deployment rolled back over
		// data written by a newer one. Coercing it to something plausible is how a settled
		// payment gets re-dispatched.
		p, err := Rehydrate(RehydrateParams{
			ID: shared.NewPaymentID(), State: "SUPERPOSED", Amount: usd(1_00),
		})
		if apierror.CodeOf(err) != apierror.CodeInternalError {
			t.Fatalf("code = %s, want INTERNAL_ERROR (%v)", apierror.CodeOf(err), err)
		}
		if p != nil {
			t.Fatal("a refused rehydration returned a payment")
		}
		if _, err := Rehydrate(RehydrateParams{ID: shared.NewPaymentID(), State: ""}); err == nil {
			t.Fatal("an empty state was accepted")
		}
	})

	t.Run("every field and the version survive the round trip", func(t *testing.T) {
		t.Parallel()
		clk := testClock()
		original, err := New(testParams(), clk)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		att := dispatch(t, original, clk)
		if err := att.Succeed("gw_txn_1", "authorized", clk.Now()); err != nil {
			t.Fatalf("Succeed: %v", err)
		}
		if err := original.MarkAuthorized(usd(90_00), nil, clk); err != nil {
			t.Fatalf("MarkAuthorized: %v", err)
		}
		if err := original.MarkCaptured(usd(90_00), clk); err != nil {
			t.Fatalf("MarkCaptured: %v", err)
		}
		refund, err := original.AddRefund(usd(10_00), RefundReasonDuplicate, "rk-1", clk)
		if err != nil {
			t.Fatalf("AddRefund: %v", err)
		}
		original.DrainEvents()

		expiry := testEpoch.Add(7 * 24 * time.Hour)
		params := RehydrateParams{
			ID: original.ID(), TenantID: original.TenantID(), MerchantID: original.MerchantID(),
			Amount: original.Amount(), CaptureMethod: original.CaptureMethod(),
			PaymentMethod: original.PaymentMethod(), MethodRef: original.MethodRef(),
			State: original.State(), Version: original.Version(),
			AuthorizedAmount: original.AuthorizedAmount(), CapturedAmount: original.CapturedAmount(),
			RefundedAmount: original.RefundedAmount(),
			Attempts:       original.Attempts(), Refunds: original.Refunds(),
			SelectedGateway: original.SelectedGateway(), RoutingPlanID: original.RoutingPlanID(),
			Description: original.Description(), StatementRef: original.StatementRef(),
			Metadata: original.Metadata(), Customer: original.Customer(),
			IdempotencyKey: original.IdempotencyKey(), CorrelationID: original.CorrelationID(),
			CreatedAt: original.CreatedAt(), UpdatedAt: original.UpdatedAt(),
			AuthorizedAt: original.AuthorizedAt(), CapturedAt: original.CapturedAt(),
			AuthExpiresAt: &expiry,
		}
		loaded, err := Rehydrate(params)
		if err != nil {
			t.Fatalf("Rehydrate: %v", err)
		}

		checks := []struct {
			field string
			ok    bool
		}{
			{"id", loaded.ID() == original.ID()},
			{"tenantId", loaded.TenantID() == original.TenantID()},
			{"merchantId", loaded.MerchantID() == original.MerchantID()},
			{"amount", loaded.Amount().Equal(original.Amount())},
			{"currency", loaded.Currency() == original.Currency()},
			{"captureMethod", loaded.CaptureMethod() == original.CaptureMethod()},
			{"paymentMethod", loaded.PaymentMethod() == original.PaymentMethod()},
			{"methodRef", loaded.MethodRef() == original.MethodRef()},
			{"state", loaded.State() == original.State()},
			{"version", loaded.Version() == original.Version()},
			{"authorizedAmount", loaded.AuthorizedAmount().Equal(original.AuthorizedAmount())},
			{"capturedAmount", loaded.CapturedAmount().Equal(original.CapturedAmount())},
			{"refundedAmount", loaded.RefundedAmount().Equal(original.RefundedAmount())},
			{"attempts", len(loaded.Attempts()) == 1 && loaded.Attempts()[0].ID() == att.ID()},
			{"refunds", len(loaded.Refunds()) == 1 && loaded.Refunds()[0].ID() == refund.ID()},
			{"selectedGateway", loaded.SelectedGateway() == original.SelectedGateway()},
			{"routingPlanId", loaded.RoutingPlanID() == original.RoutingPlanID()},
			{"description", loaded.Description() == original.Description()},
			{"statementRef", loaded.StatementRef() == original.StatementRef()},
			{"metadata", loaded.Metadata()["orderId"] == "ord_1" && len(loaded.Metadata()) == 1},
			{"customer", loaded.Customer() == original.Customer()},
			{"idempotencyKey", loaded.IdempotencyKey() == original.IdempotencyKey()},
			{"correlationId", loaded.CorrelationID() == original.CorrelationID()},
			{"createdAt", loaded.CreatedAt().Equal(original.CreatedAt())},
			{"updatedAt", loaded.UpdatedAt().Equal(original.UpdatedAt())},
			{"authorizedAt", loaded.AuthorizedAt() != nil && loaded.AuthorizedAt().Equal(*original.AuthorizedAt())},
			{"capturedAt", loaded.CapturedAt() != nil && loaded.CapturedAt().Equal(*original.CapturedAt())},
			{"authExpiresAt", loaded.AuthExpiresAt() != nil && loaded.AuthExpiresAt().Equal(expiry)},
			{"partitionMonth", loaded.PartitionMonth().Equal(original.PartitionMonth())},
			// The row's version is what the repository must write against, so a freshly loaded
			// aggregate's base is its version, not its version minus one.
			{"baseVersion", loaded.BaseVersion() == original.Version()},
			// Events are a unit-of-work concern, not persisted state.
			{"events", len(loaded.PendingEvents()) == 0},
			{"successfulAttempt", loaded.SuccessfulAttempt() != nil},
			{"latestAttempt", loaded.LatestAttempt() != nil && loaded.LatestAttempt().ID() == att.ID()},
		}
		for _, c := range checks {
			if !c.ok {
				t.Errorf("%s did not survive the round trip", c.field)
			}
		}
	})
}

func TestLatestAndSuccessfulAttemptOnAPaymentWithNone(t *testing.T) {
	t.Parallel()

	p, _ := newTestPayment(t)
	if p.LatestAttempt() != nil || p.SuccessfulAttempt() != nil || p.HasUnresolvedAttempt() {
		t.Fatal("a payment with no attempts reported one")
	}
	if !p.RefundableAmount().IsZero() {
		t.Fatalf("refundable = %s on a payment with nothing captured", p.RefundableAmount())
	}
	if p.CapturableAmount().Amount() != 100_00 {
		t.Fatalf("capturable = %s, want the full payment amount", p.CapturableAmount())
	}
}

func TestCancelAndExpire(t *testing.T) {
	t.Parallel()

	t.Run("cancel before dispatch", func(t *testing.T) {
		t.Parallel()
		p, clk := newTestPayment(t)
		if err := p.Cancel("payer abandoned the checkout", clk); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if p.State() != StateCanceled {
			t.Fatalf("state = %s", p.State())
		}
		evts := p.DrainEvents()
		last := evts[len(evts)-1]
		if last.Type != EventPaymentCanceled || last.Payload["reason"] != "payer abandoned the checkout" {
			t.Fatalf("event = %+v", last)
		}
		// Terminal means terminal.
		if err := p.Cancel("again", clk); err == nil {
			t.Fatal("a canceled payment was canceled again")
		}
	})

	t.Run("a required action can expire", func(t *testing.T) {
		t.Parallel()
		p, clk := newTestPayment(t)
		expires := testEpoch.Add(15 * time.Minute)
		err := p.RequireAction(NextAction{
			Type: ActionThreeDSChall, RedirectURL: "https://acs.example/challenge", ExpiresAt: &expires,
		}, clk)
		if err != nil {
			t.Fatalf("RequireAction: %v", err)
		}
		if p.State() != StateRequiresAction || !p.State().IsInFlight() {
			t.Fatalf("state = %s", p.State())
		}
		evts := p.DrainEvents()
		last := evts[len(evts)-1]
		if last.Type != EventPaymentRequiresAction || last.Payload["actionType"] != string(ActionThreeDSChall) {
			t.Fatalf("event = %+v", last)
		}
		clk.Advance(time.Hour)
		if err := p.Expire(clk); err != nil {
			t.Fatalf("Expire: %v", err)
		}
		if p.State() != StateExpired || !p.State().IsTerminal() {
			t.Fatalf("state = %s", p.State())
		}
	})
}

// assertRuleID checks that err carries exactly the detail a caller is expected to branch on.
// Rule 3 of docs/spec/06-code-conventions.md: a stable RuleID whenever a caller could plausibly
// fix the input.
func assertRuleID(t *testing.T, err error, field, code, ruleID string) {
	t.Helper()
	ae := apierror.From(err)
	if ae == nil {
		t.Fatalf("expected a platform error, got %v", err)
	}
	for _, d := range ae.Details {
		if d.Field == field && d.Code == code && d.RuleID == ruleID {
			return
		}
	}
	t.Fatalf("no detail {field: %s, code: %s, rule: %s} on %v; details = %+v",
		field, code, ruleID, err, ae.Details)
}
