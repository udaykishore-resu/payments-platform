package payment

import (
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// newTestRefund returns a refund in PENDING, as Payment.AddRefund would have created it.
func newTestRefund(t *testing.T) *Refund {
	t.Helper()
	return newRefund(shared.NewPaymentID(), shared.NewTenantID(), usd(25_00),
		RefundReasonRequestedByCustomer, "rk-1", testEpoch)
}

func TestNewRefundStartsPendingAndNormalisesItsReason(t *testing.T) {
	t.Parallel()

	r := newTestRefund(t)
	if r.Status() != RefundPending {
		t.Fatalf("status = %s, want PENDING", r.Status())
	}
	if r.Amount().Amount() != 25_00 || r.Reason() != RefundReasonRequestedByCustomer {
		t.Fatalf("amount = %s reason = %s", r.Amount(), r.Reason())
	}
	if r.IdempotencyKey() != "rk-1" {
		t.Fatalf("idempotencyKey = %q", r.IdempotencyKey())
	}
	if r.CreatedAt() != testEpoch || r.UpdatedAt() != testEpoch {
		t.Fatal("the refund was not stamped from the supplied instant")
	}
	if r.SubmittedAt() != nil || r.SettledAt() != nil || r.GatewayRef() != "" {
		t.Fatal("a fresh refund carries submission or settlement state")
	}

	// An unrecognised reason becomes OTHER rather than being stored verbatim: the reason drives
	// scheme treatment and dispute defence, so an uncontrolled value there is worse than a
	// truthful "we were not told".
	odd := newRefund(shared.NewPaymentID(), shared.NewTenantID(), usd(1_00), "WHATEVER", "rk", testEpoch)
	if odd.Reason() != RefundReasonOther {
		t.Fatalf("reason = %s, want OTHER", odd.Reason())
	}
}

func TestRefundTransitionGuards(t *testing.T) {
	// Verifies: docs/state-machines.md §5.
	t.Parallel()

	submittedAt := testEpoch.Add(time.Minute)
	settledAt := submittedAt.Add(24 * time.Hour)

	t.Run("pending to submitted to succeeded", func(t *testing.T) {
		t.Parallel()
		r := newTestRefund(t)
		if err := r.MarkSubmitted("gw_ref_1", submittedAt); err != nil {
			t.Fatalf("MarkSubmitted: %v", err)
		}
		if r.Status() != RefundSubmitted || r.GatewayRef() != "gw_ref_1" {
			t.Fatalf("status = %s ref = %q", r.Status(), r.GatewayRef())
		}
		if r.SubmittedAt() == nil || !r.SubmittedAt().Equal(submittedAt) {
			t.Fatalf("submittedAt = %v", r.SubmittedAt())
		}
		// Submitting twice is not idempotent, and must not be: the second call would be a second
		// instruction to the gateway.
		if apierror.CodeOf(r.MarkSubmitted("gw_ref_1", submittedAt)) != apierror.CodeInvalidStateTransition {
			t.Fatal("a submitted refund was submitted again")
		}
		if err := r.markSucceeded("gw_ref_1_final", settledAt); err != nil {
			t.Fatalf("markSucceeded: %v", err)
		}
		if r.Status() != RefundSucceeded || r.GatewayRef() != "gw_ref_1_final" {
			t.Fatalf("status = %s ref = %q", r.Status(), r.GatewayRef())
		}
		if r.SettledAt() == nil || !r.SettledAt().Equal(settledAt) {
			t.Fatalf("settledAt = %v", r.SettledAt())
		}
	})

	t.Run("a blank gateway reference does not erase the one on file", func(t *testing.T) {
		t.Parallel()
		r := newTestRefund(t)
		if err := r.MarkSubmitted("gw_ref_1", submittedAt); err != nil {
			t.Fatalf("MarkSubmitted: %v", err)
		}
		if err := r.markSucceeded("", settledAt); err != nil {
			t.Fatalf("markSucceeded: %v", err)
		}
		if r.GatewayRef() != "gw_ref_1" {
			t.Fatalf("gatewayRef = %q, want the submission reference", r.GatewayRef())
		}
	})

	t.Run("cancel withdraws a refund that was never sent", func(t *testing.T) {
		t.Parallel()
		r := newTestRefund(t)
		if err := r.Cancel(submittedAt); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if r.Status() != RefundCanceled || !r.Status().IsTerminal() {
			t.Fatalf("status = %s", r.Status())
		}
		// Cancelling something already at the gateway would leave our record and the gateway's
		// disagreeing about whether money is moving.
		other := newTestRefund(t)
		if err := other.MarkSubmitted("gw_ref_1", submittedAt); err != nil {
			t.Fatalf("MarkSubmitted: %v", err)
		}
		if apierror.CodeOf(other.Cancel(settledAt)) != apierror.CodeInvalidStateTransition {
			t.Fatal("a submitted refund was cancelled")
		}
	})

	t.Run("failure is recorded from either PENDING or SUBMITTED", func(t *testing.T) {
		t.Parallel()
		pre := newTestRefund(t)
		if err := pre.MarkFailed("PRE_DISPATCH", "no eligible connection", submittedAt); err != nil {
			t.Fatalf("MarkFailed from PENDING: %v", err)
		}
		if pre.Status() != RefundFailed || pre.FailureCode() != "PRE_DISPATCH" ||
			pre.FailureMessage() != "no eligible connection" {
			t.Fatalf("refund = %s / %q / %q", pre.Status(), pre.FailureCode(), pre.FailureMessage())
		}

		post := newTestRefund(t)
		if err := post.MarkSubmitted("gw_ref_1", submittedAt); err != nil {
			t.Fatalf("MarkSubmitted: %v", err)
		}
		if err := post.MarkFailed("R_DECLINED", "issuer refused", settledAt); err != nil {
			t.Fatalf("MarkFailed from SUBMITTED: %v", err)
		}
		if post.Status() != RefundFailed {
			t.Fatalf("status = %s", post.Status())
		}
	})

	t.Run("terminal statuses are terminal", func(t *testing.T) {
		t.Parallel()
		// A retry of a failed refund is a *new* refund with a new idempotency key; re-dispatching
		// this row risks a double refund if the first failure was misclassified. And money that
		// has left cannot be un-refunded — that would be a capture, which no gateway offers.
		reach := map[RefundStatus]func(*Refund) error{
			RefundSucceeded: func(r *Refund) error {
				if err := r.MarkSubmitted("gw", submittedAt); err != nil {
					return err
				}
				return r.markSucceeded("gw", settledAt)
			},
			RefundFailed:   func(r *Refund) error { return r.MarkFailed("X", "y", submittedAt) },
			RefundCanceled: func(r *Refund) error { return r.Cancel(submittedAt) },
		}
		for status, drive := range reach {
			r := newTestRefund(t)
			if err := drive(r); err != nil {
				t.Fatalf("reaching %s: %v", status, err)
			}
			for name, act := range map[string]func() error{
				"MarkSubmitted": func() error { return r.MarkSubmitted("gw", settledAt) },
				"markSucceeded": func() error { return r.markSucceeded("gw", settledAt) },
				"MarkFailed":    func() error { return r.MarkFailed("X", "y", settledAt) },
				"Cancel":        func() error { return r.Cancel(settledAt) },
			} {
				err := act()
				if apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
					t.Errorf("%s from %s: code = %s, want INVALID_STATE_TRANSITION",
						name, status, apierror.CodeOf(err))
				}
				if r.Status() != status {
					t.Fatalf("%s from %s moved the refund to %s", name, status, r.Status())
				}
			}
		}
	})
}

func TestARefundReachesSucceededOnlyThroughThePayment(t *testing.T) {
	// Verifies: docs/state-machines.md §5 — the payment's refundedAmount and state must move in
	// the same step as the refund's status, or the two drift and the nightly reconciliation opens
	// an exception nobody can explain.
	t.Parallel()

	// markSucceeded is unexported, so no code outside this package can call it at all: the only
	// way in is Payment.ConfirmRefund. What this test can check is the guard that remains even
	// inside the package — a refund cannot jump straight from PENDING to SUCCEEDED, so a refund
	// that was never submitted to a gateway cannot be recorded as having paid the customer.
	r := newTestRefund(t)
	err := r.markSucceeded("gw_ref_1", testEpoch)
	if apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
		t.Fatalf("code = %s, want INVALID_STATE_TRANSITION (%v)", apierror.CodeOf(err), err)
	}
	if r.Status() != RefundPending || r.SettledAt() != nil {
		t.Fatalf("a refused markSucceeded moved the refund: %s / %v", r.Status(), r.SettledAt())
	}

	// Through the payment, the two move together and stay consistent.
	p, clk := capturedPayment(t)
	live, err := p.AddRefund(usd(25_00), RefundReasonDuplicate, "rk-1", clk)
	if err != nil {
		t.Fatalf("AddRefund: %v", err)
	}
	// ConfirmRefund refuses too, for the same reason, until the refund has been submitted.
	if apierror.CodeOf(p.ConfirmRefund(live.ID(), "gw", clk)) != apierror.CodeInvalidStateTransition {
		t.Fatal("an unsubmitted refund was confirmed")
	}
	if !p.RefundedAmount().IsZero() || p.State() != StateCaptured {
		t.Fatalf("a refused confirmation moved the payment: %s / %s", p.State(), p.RefundedAmount())
	}

	if err := live.MarkSubmitted("gw_ref_1", clk.Now()); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}
	if err := p.ConfirmRefund(live.ID(), "gw_ref_1", clk); err != nil {
		t.Fatalf("ConfirmRefund: %v", err)
	}
	if live.Status() != RefundSucceeded {
		t.Fatalf("refund status = %s", live.Status())
	}
	if p.RefundedAmount().Amount() != 25_00 || p.State() != StatePartiallyRefunded {
		t.Fatalf("the payment did not move with the refund: %s / %s", p.State(), p.RefundedAmount())
	}
	// Confirming twice must not double-count: the refund's own FSM is what stops it.
	if err := p.ConfirmRefund(live.ID(), "gw_ref_1", clk); err == nil {
		t.Fatal("a succeeded refund was confirmed a second time")
	}
	if p.RefundedAmount().Amount() != 25_00 {
		t.Fatalf("refundedAmount = %s after a duplicate confirmation", p.RefundedAmount())
	}
}

func TestRehydrateRefund(t *testing.T) {
	t.Parallel()

	submittedAt := testEpoch.Add(time.Minute)
	settledAt := submittedAt.Add(48 * time.Hour)

	base := RehydrateRefundParams{
		ID: shared.NewRefundID(), PaymentID: shared.NewPaymentID(), TenantID: shared.NewTenantID(),
		Amount: usd(12_34), Reason: RefundReasonFraudulent, Status: RefundSucceeded,
		GatewayRef: "gw_ref_9", IdempotencyKey: "rk-9", FailureCode: "", FailureMessage: "",
		CreatedAt: testEpoch, UpdatedAt: settledAt, SubmittedAt: &submittedAt, SettledAt: &settledAt,
	}

	r, err := RehydrateRefund(base)
	if err != nil {
		t.Fatalf("RehydrateRefund: %v", err)
	}
	checks := []struct {
		field string
		ok    bool
	}{
		{"id", r.ID() == base.ID},
		{"paymentId", r.PaymentID() == base.PaymentID},
		{"tenantId", r.TenantID() == base.TenantID},
		{"amount", r.Amount().Equal(base.Amount)},
		{"reason", r.Reason() == base.Reason},
		{"status", r.Status() == base.Status},
		{"gatewayRef", r.GatewayRef() == base.GatewayRef},
		{"idempotencyKey", r.IdempotencyKey() == base.IdempotencyKey},
		{"createdAt", r.CreatedAt().Equal(base.CreatedAt)},
		{"updatedAt", r.UpdatedAt().Equal(base.UpdatedAt)},
		{"submittedAt", r.SubmittedAt() != nil && r.SubmittedAt().Equal(submittedAt)},
		{"settledAt", r.SettledAt() != nil && r.SettledAt().Equal(settledAt)},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s did not survive rehydration", c.field)
		}
	}

	bad := base
	bad.Status = "REVERSING"
	if _, err := RehydrateRefund(bad); apierror.CodeOf(err) != apierror.CodeInternalError {
		t.Fatalf("unknown status: code = %s, want INTERNAL_ERROR", apierror.CodeOf(err))
	}
	empty := base
	empty.Status = ""
	if _, err := RehydrateRefund(empty); err == nil {
		t.Fatal("an empty status was accepted")
	}
}
