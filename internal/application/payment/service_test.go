package payment

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/apptest"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	dpayment "github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

var _ = money.Zero

// TestCreateHappyPath walks the whole pipeline once, so that every later test can be about one
// deviation from it.
func TestCreateHappyPath(t *testing.T) {
	// Verifies: BR-20, FR-53.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})

	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Plan == nil {
		t.Fatal("no routing plan was returned; the plan is the audit record behind the decision")
	}
	if got, _ := res.Plan.Primary(); got.GatewayID != gwA {
		t.Fatalf("primary = %s, want gw-a", got.GatewayID)
	}
	p := h.loadPayment(res.Payment.ID())
	if p.State() != dpayment.StateCaptured {
		t.Fatalf("state = %s, want CAPTURED", p.State())
	}
	if got := h.audit.Actions(); len(got) < 2 {
		t.Fatalf("audit trail = %v, want at least payment.created and the dispatch outcome", got)
	}
}

// TestCreateAssertsTenantContext.
//
// Tenant identity has exactly one origin (baseline §16.2). A command with no tenant is a command
// whose origin was lost somewhere between the token and here, and the only safe response is to
// refuse — a request with no tenant scoped to "whatever the repository defaults to" is an
// isolation defect waiting for a row-level-security misconfiguration.
func TestCreateAssertsTenantContext(t *testing.T) {
	// Verifies: FR-06, NFR-29.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	cmd := createCommand()
	cmd.TenantID = ""

	if _, err := h.svc.Create(context.Background(), cmd); err == nil {
		t.Fatal("a command with no tenant was accepted")
	} else if apierror.CodeOf(err) != apierror.CodeMissingTenantContext {
		t.Fatalf("got %s, want MISSING_TENANT_CONTEXT", apierror.CodeOf(err))
	}
}

// TestCreateRefusesATenantThatDoesNotOwnTheMerchant asserts the cross-tenant guard, and asserts
// that it answers NOT_FOUND rather than FORBIDDEN: distinguishing "not yours" from "does not
// exist" leaks the existence of another tenant's identifiers to anyone who can guess one.
func TestCreateRefusesATenantThatDoesNotOwnTheMerchant(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	snap := defaultSnapshot()
	snap.TenantID = "ten_01HZOTHERTENANT0000000000"
	h.loader.snap = snap

	if _, err := h.svc.Create(context.Background(), createCommand()); err == nil {
		t.Fatal("a merchant belonging to another tenant was accepted")
	} else if apierror.CodeOf(err) != apierror.CodePaymentNotFound {
		t.Fatalf("got %s, want PAYMENT_NOT_FOUND", apierror.CodeOf(err))
	}
}

// TestCreateRefusesASuspendedMerchant, and reports the suspension rather than a gateway problem:
// telling a merchant with three healthy connections that they have no gateway sends their
// engineer to look at a configuration that is fine.
func TestCreateRefusesASuspendedMerchant(t *testing.T) {
	// Verifies: FR-59.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	snap := defaultSnapshot()
	snap.Status = "SUSPENDED"
	h.loader.snap = snap

	_, err := h.svc.Create(context.Background(), createCommand())
	if err == nil {
		t.Fatal("a suspended merchant was allowed to create a payment")
	}
	if apierror.CodeOf(err) != apierror.CodeMerchantSuspended {
		t.Fatalf("got %s, want MERCHANT_SUSPENDED", apierror.CodeOf(err))
	}
	if n := len(h.adapterA.Calls()); n != 0 {
		t.Fatalf("a gateway was called %d times for a suspended merchant", n)
	}
}

// TestRiskDeclineNeverReachesAGateway.
//
// A payment that risk declines must never have existed as far as the gateway is concerned, and
// must not appear as a phantom failure on the merchant's dashboard either — which is why the
// aggregate is constructed before risk but persisted after it.
func TestRiskDeclineNeverReachesAGateway(t *testing.T) {
	// Verifies: BR-15, FR-61.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.riskStub.decision = risk.Decision{
		Outcome: risk.OutcomeDecline,
		Reasons: []risk.Reason{{Check: risk.CheckPlatformBlocklist, Severity: risk.SeverityCritical}},
	}

	_, err := h.svc.Create(context.Background(), createCommand())
	if err == nil {
		t.Fatal("a risk-declined payment was dispatched")
	}
	if apierror.CodeOf(err) != apierror.CodeRiskDeclined {
		t.Fatalf("got %s, want RISK_DECLINED", apierror.CodeOf(err))
	}
	if n := len(h.adapterA.Calls()); n != 0 {
		t.Fatalf("a gateway was called %d times for a declined payment", n)
	}
	if len(h.store.AllPayments()) != 0 {
		t.Fatal("a declined payment was persisted; it should never have existed")
	}
}

// TestRiskEngineFailureIsRetryableNotADecline.
//
// A risk *engine* failure is infrastructure. Reporting it as a decline would tell the merchant
// their customer was refused, which is both wrong and unfixable by them; reporting it as
// retryable lets the client do the one thing that might work.
func TestRiskEngineFailureIsRetryableNotADecline(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.riskStub.err = apierror.New(apierror.CodeDependencyFailure, "scorer unreachable")

	_, err := h.svc.Create(context.Background(), createCommand())
	if err == nil {
		t.Fatal("a risk engine failure was swallowed")
	}
	if !apierror.IsRetryable(err) {
		t.Fatalf("error %v is not retryable; a risk engine failure is infrastructure", err)
	}
	if apierror.CodeOf(err) == apierror.CodeRiskDeclined {
		t.Fatal("an engine failure was reported as a risk decline")
	}
}

// TestTwoConcurrentCreatesWithOneIdempotencyKeyProduceOnePayment.
//
// The claim, not the handler, is what makes this true: an ON CONFLICT DO NOTHING insert against a
// unique index resolves the race deterministically, and the loser is told to retry rather than
// blocking on the winner's lease (baseline §14.3, ADR-009). This test drives both goroutines
// through the same claim-then-create sequence the transport layer performs, and asserts that
// exactly one payment exists afterwards.
func TestTwoConcurrentCreatesWithOneIdempotencyKeyProduceOnePayment(t *testing.T) {
	// Verifies: BR-21, FR-55.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})

	cmd := createCommand()
	key := ports.IdempotencyKey{
		TenantID: cmd.TenantID, MerchantID: cmd.MerchantID,
		Method: "POST", PathTemplate: "/v1/payments", Key: cmd.IdempotencyKey,
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		outcomes []ports.ClaimOutcome
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var claim ports.ClaimResult
			err := h.uow.Within(context.Background(), func(ctx context.Context, r ports.Repositories) error {
				var err error
				claim, err = r.Idempotency.Claim(ctx, ports.IdempotencyRecord{
					Key: key, Fingerprint: "fp-1",
					LeaseExpiresAt: h.clock.Now().Add(time.Minute),
				})
				return err
			})
			if err != nil {
				return
			}
			mu.Lock()
			outcomes = append(outcomes, claim.Outcome)
			mu.Unlock()
			if claim.Outcome != ports.ClaimNew {
				return
			}
			if _, err := h.svc.Create(context.Background(), cmd); err != nil {
				t.Errorf("Create: %v", err)
			}
		}()
	}
	wg.Wait()

	if n := len(h.store.AllPayments()); n != 1 {
		t.Fatalf("got %d payments for one idempotency key, want 1", n)
	}
	newCount := 0
	for _, o := range outcomes {
		if o == ports.ClaimNew {
			newCount++
		}
	}
	if newCount != 1 {
		t.Fatalf("%d callers were told they owned the operation, want exactly 1 (outcomes=%v)", newCount, outcomes)
	}
}

// TestIdempotencyKeyReusedWithADifferentBodyIsRejected.
//
// Honouring the key would be worse than rejecting it: the caller believes they are retrying and
// is in fact asking for a different payment.
func TestIdempotencyKeyReusedWithADifferentBodyIsRejected(t *testing.T) {
	// Verifies: FR-56.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	key := ports.IdempotencyKey{
		TenantID: testTenant, MerchantID: testMerchant,
		Method: "POST", PathTemplate: "/v1/payments", Key: "reused",
	}
	claim := func(fp string) ports.ClaimOutcome {
		var out ports.ClaimResult
		if err := h.uow.Within(context.Background(), func(ctx context.Context, r ports.Repositories) error {
			var err error
			out, err = r.Idempotency.Claim(ctx, ports.IdempotencyRecord{Key: key, Fingerprint: fp})
			return err
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		return out.Outcome
	}
	if got := claim("fp-a"); got != ports.ClaimNew {
		t.Fatalf("first claim = %s, want NEW", got)
	}
	if got := claim("fp-b"); got != ports.ClaimFingerprintMismatch {
		t.Fatalf("second claim = %s, want FINGERPRINT_MISMATCH", got)
	}
}

// TestCaptureGoesToTheAuthorizingGatewayAndNeverRoutes.
//
// Routing a capture would be meaningless at best and, at a gateway that happened to accept it, a
// second charge at worst. The assertion is therefore twofold: the call lands on the gateway that
// holds the authorization, and the router is never consulted at all.
func TestCaptureGoesToTheAuthorizingGatewayAndNeverRoutes(t *testing.T) {
	// Verifies: BR-24, FR-69.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	// Authorize on the *fallback* gateway, so that "went to the authorizing gateway" and "went to
	// the primary" are different answers and the test can tell them apart.
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{
		Result: declinedResult(dpayment.DeclineIssuerUnavailable),
	})
	h.adapterB.Script(shared.OpAuthorize, apptest.GatewayScript{Result: authorizedResult(mustEUR(8450))})
	h.adapterB.Script(shared.OpCapture, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})

	cmd := createCommand()
	cmd.CaptureMethod = dpayment.CaptureManual
	res, err := h.svc.Create(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	routesBefore := h.candidates.calls

	amount := mustEUR(8450)
	out, err := h.svc.Capture(context.Background(), CaptureCommand{
		TenantID: testTenant, PaymentID: res.Payment.ID(), Amount: &amount, Final: true,
		IdempotencyKey: "cap-1",
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if h.candidates.calls != routesBefore {
		t.Fatalf("the router was consulted %d extra times for a capture", h.candidates.calls-routesBefore)
	}
	if n := captureCalls(h.adapterA); n != 0 {
		t.Fatalf("the capture went to gw-a %d times; gw-b holds the authorization", n)
	}
	if n := captureCalls(h.adapterB); n != 1 {
		t.Fatalf("gw-b received %d captures, want 1", n)
	}
	if out.Payment.State() != dpayment.StateCaptured {
		t.Fatalf("state = %s, want CAPTURED", out.Payment.State())
	}
}

// TestRefundGoesToTheGatewayThatTookTheMoney, and is permitted while the merchant is suspended.
//
// A suspension stops a merchant taking money, not returning it. Blocking refunds during a
// suspension converts a merchant problem into a consumer-harm problem and, in several
// jurisdictions, a regulatory one.
func TestRefundGoesToTheGatewayThatTookTheMoney(t *testing.T) {
	// Verifies: BR-25, FR-70.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{
		Result: declinedResult(dpayment.DeclineIssuerUnavailable),
	})
	h.adapterB.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})
	h.adapterB.Script(shared.OpRefund, apptest.GatewayScript{
		Result: &spi.Result{Status: spi.StatusRefunded, GatewayRef: "gwref_ref"},
	})

	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Suspend the merchant *after* the payment succeeded: money-out must still work.
	snap := defaultSnapshot()
	snap.Status = "SUSPENDED"
	h.loader.snap = snap
	routesBefore := h.candidates.calls

	amount := mustEUR(8450)
	out, err := h.svc.Refund(context.Background(), RefundCommand{
		TenantID: testTenant, PaymentID: res.Payment.ID(), Amount: &amount,
		Reason: dpayment.RefundReasonRequestedByCustomer, IdempotencyKey: "ref-1",
	})
	if err != nil {
		t.Fatalf("Refund on a suspended merchant: %v", err)
	}
	if h.candidates.calls != routesBefore {
		t.Fatal("the router was consulted for a refund")
	}
	if n := refundCalls(h.adapterA); n != 0 {
		t.Fatalf("the refund went to gw-a %d times; gw-b took the money", n)
	}
	if n := refundCalls(h.adapterB); n != 1 {
		t.Fatalf("gw-b received %d refunds, want 1", n)
	}
	if out.Payment.State() != dpayment.StateRefunded {
		t.Fatalf("state = %s, want REFUNDED", out.Payment.State())
	}
}

// TestRefundWithAnUnknownOutcomeIsNotRetried.
//
// A duplicate refund is a duplicate payout. The refund stays where it is and enters
// reconciliation; nothing retries it automatically.
func TestRefundWithAnUnknownOutcomeIsNotRetried(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})
	h.adapterA.Script(shared.OpRefund, apptest.GatewayScript{Err: spi.ErrOutcomeUnknown})

	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	amount := mustEUR(1000)
	if _, err := h.svc.Refund(context.Background(), RefundCommand{
		TenantID: testTenant, PaymentID: res.Payment.ID(), Amount: &amount,
		Reason: dpayment.RefundReasonRequestedByCustomer, IdempotencyKey: "ref-unknown",
	}); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if n := refundCalls(h.adapterA); n != 1 {
		t.Fatalf("the refund was sent %d times, want exactly 1", n)
	}
	if !hasEvent(h.store.OutboxTypes(), string(dpayment.EventPaymentReconciliationRequired)) {
		t.Fatalf("an unknown refund outcome did not raise reconciliation_required; outbox=%v",
			h.store.OutboxTypes())
	}
}

// TestVoidReleasesTheHoldAtTheAuthorizingGateway.
func TestVoidReleasesTheHoldAtTheAuthorizingGateway(t *testing.T) {
	// Verifies: BR-26, FR-71.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Result: authorizedResult(mustEUR(8450))})
	h.adapterA.Script(shared.OpVoid, apptest.GatewayScript{
		Result: &spi.Result{Status: spi.StatusVoided, GatewayRef: "gwref_void"},
	})

	cmd := createCommand()
	cmd.CaptureMethod = dpayment.CaptureManual
	res, err := h.svc.Create(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	out, err := h.svc.Void(context.Background(), VoidCommand{
		TenantID: testTenant, PaymentID: res.Payment.ID(), IdempotencyKey: "void-1",
	})
	if err != nil {
		t.Fatalf("Void: %v", err)
	}
	if out.Payment.State() != dpayment.StateVoided {
		t.Fatalf("state = %s, want VOIDED", out.Payment.State())
	}
}

// TestCaptureIsRefusedForAMethodWithNoSeparateCaptureStep. The gateway would reject it later with
// a much worse error; catching it at the boundary is what turns a vendor error into an answer.
func TestCaptureIsRefusedForAMethodWithNoSeparateCaptureStep(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})

	cmd := createCommand()
	cmd.Method = shared.MethodSEPADebit
	cmd.CaptureMethod = dpayment.CaptureAutomatic
	res, err := h.svc.Create(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := h.svc.Capture(context.Background(), CaptureCommand{
		TenantID: testTenant, PaymentID: res.Payment.ID(), IdempotencyKey: "cap-x",
	}); err == nil {
		t.Fatal("a capture was accepted for a method that settles in one step")
	}
}

// TestL5FailureStopsBeforeThePaymentExists asserts the ordering of validation against creation:
// the aggregate must not be persisted for a request the validation plane rejected.
func TestL5FailureStopsBeforeThePaymentExists(t *testing.T) {
	// Verifies: FR-60.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.validator.createErr = apierror.New(apierror.CodeCurrencyNotSupported, "EUR is not enabled")

	if _, err := h.svc.Create(context.Background(), createCommand()); err == nil {
		t.Fatal("an L5 failure did not stop the request")
	}
	if len(h.store.AllPayments()) != 0 {
		t.Fatal("a payment was persisted for a request that failed validation")
	}
	if n := len(h.adapterA.Calls()); n != 0 {
		t.Fatalf("a gateway was called %d times after an L5 failure", n)
	}
}

// TestDegradedIsReportedWhenTheConfigurationSnapshotIsStale.
//
// "We processed forty minutes of traffic degraded" must be a fact in the response record rather
// than an inference from a dashboard.
func TestDegradedIsReportedWhenTheConfigurationSnapshotIsStale(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	snap := defaultSnapshot()
	snap.SnapshotAge = 90 * time.Second
	h.loader.snap = snap
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})

	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !res.Degraded {
		t.Fatal("a payment served from a stale snapshot was not reported as degraded")
	}
}

// TestNoEligibleGatewayIsAnAnswerNotJustARefusal.
//
// "No gateway is available" is an answer a merchant can do nothing with. The plan's rejections
// carry the reason each candidate was dropped, which is the difference between a support ticket
// with a resolution and one without.
func TestNoEligibleGatewayIsAnAnswerNotJustARefusal(t *testing.T) {
	// Verifies: FR-67.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	set := eligibleCandidates(gwA, gwB)
	for i := range set {
		set[i].Certified = false
	}
	h.candidates.set = set

	_, err := h.svc.Create(context.Background(), createCommand())
	if err == nil {
		t.Fatal("a payment with no eligible gateway was dispatched")
	}
	if apierror.CodeOf(err) != apierror.CodeNoEligibleGateway {
		t.Fatalf("got %s, want NO_ELIGIBLE_GATEWAY", apierror.CodeOf(err))
	}
	e := apierror.From(err)
	if e == nil || len(e.Details) == 0 {
		t.Fatal("the error carried no rejection reasons; the merchant cannot act on it")
	}
}

// TestGetAndListAreTenantScoped exercises the read path.
func TestGetAndListAreTenantScoped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})

	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := h.svc.Get(context.Background(), res.Payment.ID())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID() != res.Payment.ID() {
		t.Fatalf("Get returned %s, want %s", got.ID(), res.Payment.ID())
	}
	list, _, err := h.svc.List(context.Background(), ports.PaymentFilter{MerchantID: testMerchant}, ports.Page{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List returned %d payments, want 1", len(list))
	}
}

func captureCalls(g *apptest.Gateway) int { return callsOf(g, shared.OpCapture) }

func refundCalls(g *apptest.Gateway) int { return callsOf(g, shared.OpRefund) }

func callsOf(g *apptest.Gateway, op shared.Operation) int {
	n := 0
	for _, c := range g.Calls() {
		if c.Op == op {
			n++
		}
	}
	return n
}
