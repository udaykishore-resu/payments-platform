package payment

import (
	"context"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/apptest"
	dpayment "github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// newUnresolved drives a payment into the state the reconciler exists for: a committed attempt
// whose outcome the gateway never reported.
func newUnresolved(t *testing.T) (*harness, *dpayment.Payment) {
	t.Helper()
	h := newHarness(t)
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Err: spi.ErrOutcomeUnknown})
	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return h, h.loadPayment(res.Payment.ID())
}

func (h *harness) reconciler() *Reconciler {
	return NewReconciler(ReconcilerDeps{
		UoW: h.uow, Gateways: h.resolver, Audit: h.audit, Metrics: h.metrics,
		Clock: h.clock, Settings: ReconcilerConfig{
			MinAge: 0, BatchSize: 10, LookupTimeout: time.Second, KeySalt: "test-salt",
		},
	})
}

// TestReconcilerResolvesAnAuthorizedTimeout.
//
// This is the case that would be a *lost sale* under any "treat a timeout as a failure" policy:
// the gateway did authorize, we simply never heard. Asking is the only way to find out, and
// asking is only possible because the attempt row exists and its key is deterministic.
func TestReconcilerResolvesAnAuthorizedTimeout(t *testing.T) {
	// Verifies: BR-28, FR-85.
	t.Parallel()
	h, p := newUnresolved(t)
	defer h.finish()
	amount := mustEUR(8450)
	h.adapterA.Script(shared.OpLookup, apptest.GatewayScript{
		Result: &spi.Result{Status: spi.StatusAuthorized, GatewayRef: "gwref_found", AuthorizedAmount: &amount},
	})

	out, err := h.reconciler().ResolveUnknown(context.Background())
	if err != nil {
		t.Fatalf("ResolveUnknown: %v", err)
	}
	if out.Resolved != 1 {
		t.Fatalf("resolved %d of %d, want 1 (exceptions=%d)", out.Resolved, out.Examined, out.Exceptions)
	}
	got := h.loadPayment(p.ID())
	if got.State() != dpayment.StateAuthorized {
		t.Fatalf("state = %s, want AUTHORIZED", got.State())
	}
	if o := got.Attempts()[0].Outcome(); o != dpayment.OutcomeSuccess {
		t.Fatalf("attempt = %s, want SUCCESS", o)
	}
}

// TestReconcilerTreatsNotFoundAsPositiveEvidenceThatNothingHappened.
//
// NOT_FOUND is only evidence because the key is deterministic: we asked about a transaction we
// can name, and the gateway has no record of it. That is what makes ERROR — the one outcome that
// permits a retry — the correct resolution.
func TestReconcilerTreatsNotFoundAsPositiveEvidenceThatNothingHappened(t *testing.T) {
	t.Parallel()
	h, p := newUnresolved(t)
	defer h.finish()
	h.adapterA.Script(shared.OpLookup, apptest.GatewayScript{
		Result: &spi.Result{Status: spi.StatusNotFound},
	})

	if _, err := h.reconciler().ResolveUnknown(context.Background()); err != nil {
		t.Fatalf("ResolveUnknown: %v", err)
	}
	got := h.loadPayment(p.ID())
	if o := got.Attempts()[0].Outcome(); o != dpayment.OutcomeError {
		t.Fatalf("attempt = %s, want ERROR", o)
	}
	if got.HasUnresolvedAttempt() {
		t.Fatal("the payment still has an unresolved attempt after a NOT_FOUND lookup")
	}
}

// TestReconcilerResolvesADeclinedTimeout.
func TestReconcilerResolvesADeclinedTimeout(t *testing.T) {
	t.Parallel()
	h, p := newUnresolved(t)
	defer h.finish()
	h.adapterA.Script(shared.OpLookup, apptest.GatewayScript{
		Result: &spi.Result{
			Status: spi.StatusDeclined, GatewayRef: "gwref_dec",
			DeclineReason: dpayment.DeclineStolenCard,
		},
	})

	if _, err := h.reconciler().ResolveUnknown(context.Background()); err != nil {
		t.Fatalf("ResolveUnknown: %v", err)
	}
	got := h.loadPayment(p.ID())
	if o := got.Attempts()[0].Outcome(); o != dpayment.OutcomeDeclined {
		t.Fatalf("attempt = %s, want DECLINED", o)
	}
	if got.State() != dpayment.StateFailed {
		t.Fatalf("state = %s, want FAILED", got.State())
	}
}

// TestReconcilerOpensAnExceptionWhenTheGatewayAlsoCannotSay.
//
// The payment does not move. Guessing here is precisely the failure the whole design exists to
// avoid, and an exception is the honest record that a human has to look.
func TestReconcilerOpensAnExceptionWhenTheGatewayAlsoCannotSay(t *testing.T) {
	// Verifies: FR-84.
	t.Parallel()
	for _, tc := range []struct {
		name   string
		script apptest.GatewayScript
	}{
		{"the lookup itself fails", apptest.GatewayScript{Err: spi.ErrOutcomeUnknown}},
		{"the gateway says PENDING", apptest.GatewayScript{Result: &spi.Result{Status: spi.StatusPending}}},
		{"the gateway returns nothing", apptest.GatewayScript{}},
	} {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, p := newUnresolved(t)
			defer h.finish()
			h.adapterA.Script(shared.OpLookup, tc.script)

			out, err := h.reconciler().ResolveUnknown(context.Background())
			if err != nil {
				t.Fatalf("ResolveUnknown: %v", err)
			}
			if out.Exceptions != 1 {
				t.Fatalf("exceptions = %d, want 1", out.Exceptions)
			}
			got := h.loadPayment(p.ID())
			if o := got.Attempts()[0].Outcome(); o != dpayment.OutcomeTimeoutUnknown {
				t.Fatalf("attempt = %s, want it left at TIMEOUT_UNKNOWN", o)
			}
			if got.State() != dpayment.StateProcessing {
				t.Fatalf("state = %s, want PROCESSING", got.State())
			}
			if len(h.store.OpenExceptions()) == 0 {
				t.Fatal("no reconciliation exception was opened")
			}
		})
	}
}

// TestAuthorizationExpirySweeper.
//
// Expiring the platform's record slightly early is harmless — the merchant re-authorizes.
// Expiring it late means attempting a capture against a hold the issuer already released, which
// at several gateways succeeds and then reverses weeks later as an unexplained chargeback.
func TestAuthorizationExpirySweeper(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Result: authorizedResult(mustEUR(8450))})

	cmd := createCommand()
	cmd.CaptureMethod = dpayment.CaptureManual
	res, err := h.svc.Create(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	h.clock.Advance(8 * 24 * time.Hour)
	out, err := h.reconciler().SweepExpiredAuthorizations(context.Background())
	if err != nil {
		t.Fatalf("SweepExpiredAuthorizations: %v", err)
	}
	if out.Resolved != 1 {
		t.Fatalf("expired %d of %d authorizations, want 1", out.Resolved, out.Examined)
	}
	if got := h.loadPayment(res.Payment.ID()); got.State() != dpayment.StateExpired {
		t.Fatalf("state = %s, want EXPIRED", got.State())
	}
}

// TestExpirySweeperRefusesToExpireAPaymentWithAnUnresolvedAttempt.
//
// EXPIRED is terminal, and reaching it would assert that no money moved — on a payment where
// money may have. The unresolved attempt outranks the clock.
func TestExpirySweeperRefusesToExpireAPaymentWithAnUnresolvedAttempt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Result: authorizedResult(mustEUR(8450))})
	cmd := createCommand()
	cmd.CaptureMethod = dpayment.CaptureManual
	res, err := h.svc.Create(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Drive the same payment into an unresolved capture, so the clock and the ambiguity disagree.
	p := h.loadPayment(res.Payment.ID())
	att, err := p.StartAttempt(gwA, p.RoutingPlanID(), shared.OpCapture, h.clock)
	if err != nil {
		t.Fatalf("StartAttempt: %v", err)
	}
	if err := att.Dispatch(h.clock.Now()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if err := att.TimeOut("capture timed out", h.clock.Now()); err != nil {
		t.Fatalf("TimeOut: %v", err)
	}

	h.clock.Advance(8 * 24 * time.Hour)
	out, err := h.reconciler().SweepExpiredAuthorizations(context.Background())
	if err != nil {
		t.Fatalf("SweepExpiredAuthorizations: %v", err)
	}
	if out.Resolved != 0 {
		t.Fatalf("a payment with an unresolved attempt was expired")
	}
	if got := h.loadPayment(res.Payment.ID()); got.State() == dpayment.StateExpired {
		t.Fatal("state = EXPIRED for a payment whose outcome is unknown")
	}
}

// TestSettlementIngestion.
//
// Settlement is observed, never asserted: the platform does not settle funds, so the only thing
// that can move a payment to SETTLED is the gateway saying it did.
func TestSettlementIngestion(t *testing.T) {
	// Verifies: BR-30, FR-82, FR-83.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})
	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	out, err := h.reconciler().IngestSettlement(context.Background(), []SettlementRow{{
		GatewayID: gwA, PaymentID: res.Payment.ID(),
		Gross: mustEUR(8450), Fee: mustEUR(275), Net: mustEUR(8175),
		SettledAt: h.clock.Now(), SettlementRef: "po_1",
	}})
	if err != nil {
		t.Fatalf("IngestSettlement: %v", err)
	}
	if out.Resolved != 1 {
		t.Fatalf("ingested %d of %d rows, want 1", out.Resolved, out.Examined)
	}
	if got := h.loadPayment(res.Payment.ID()); got.State() != dpayment.StateSettled {
		t.Fatalf("state = %s, want SETTLED", got.State())
	}
}

// TestSettlementForAnUnknownPaymentOpensAnException.
//
// This is the single most important signal the settlement sweep produces: money moved for
// something the platform did not think existed. Dropping the row would make it invisible.
func TestSettlementForAnUnknownPaymentOpensAnException(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()

	if _, err := h.reconciler().IngestSettlement(context.Background(), []SettlementRow{{
		GatewayID: gwA, PaymentID: shared.PaymentID("pay_01HZNOTOURSPAYMENT000000"),
		Gross: mustEUR(1000), Fee: mustEUR(30), Net: mustEUR(970),
		SettledAt: h.clock.Now(), SettlementRef: "po_2",
	}}); err != nil {
		t.Fatalf("IngestSettlement: %v", err)
	}
	ex := h.store.OpenExceptions()
	if len(ex) != 1 {
		t.Fatalf("got %d exceptions, want 1", len(ex))
	}
	if ex[0].Kind != "SETTLEMENT_FOR_UNKNOWN_PAYMENT" {
		t.Fatalf("exception kind = %q", ex[0].Kind)
	}
}

// TestSettlementThatDoesNotReconcileIsAnExceptionNotAPosting.
//
// A row whose three numbers disagree is a gateway contract violation or a parsing defect. Posting
// it would make the arithmetic work by inventing a number, which is exactly what a shadow ledger
// must never do.
func TestSettlementThatDoesNotReconcileIsAnExceptionNotAPosting(t *testing.T) {
	// Verifies: BR-30.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})
	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	out, err := h.reconciler().IngestSettlement(context.Background(), []SettlementRow{{
		GatewayID: gwA, PaymentID: res.Payment.ID(),
		Gross: mustEUR(8450), Fee: mustEUR(275), Net: mustEUR(8000), // 8000 + 275 ≠ 8450
		SettledAt: h.clock.Now(), SettlementRef: "po_3",
	}})
	if err != nil {
		t.Fatalf("IngestSettlement: %v", err)
	}
	if out.Exceptions != 1 {
		t.Fatalf("exceptions = %d, want 1", out.Exceptions)
	}
	if got := h.loadPayment(res.Payment.ID()); got.State() == dpayment.StateSettled {
		t.Fatal("a settlement row that does not reconcile was posted anyway")
	}
}

var _ = money.Zero
