package payment

import (
	"context"
	"errors"
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/apptest"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	dpayment "github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/routing"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// TestAttemptIsCommittedBeforeTheGatewayCall is the single most important test in the repository.
//
// Reversing the two operations produces a system that passes every other test here: the payment
// still authorizes, the attempt still records, the events still fire. What it loses is the only
// thing that makes a crash survivable — a durable record, written before the money could have
// moved, that reconciliation can act on. A crash between an un-committed attempt and the gateway
// leaves a charge that nothing in the platform knows exists, and nothing can find what it does
// not know exists.
//
// The assertion is therefore about *order*, not about outcome, and it reads an interleaved log of
// repository writes and adapter calls because nothing else can distinguish the two arrangements.
func TestAttemptIsCommittedBeforeTheGatewayCall(t *testing.T) {
	// Verifies: FR-63.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})

	if _, err := h.svc.Create(context.Background(), createCommand()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	save := h.rec.IndexOf("payment.save")
	call := h.rec.IndexOf("gateway.call")
	if save < 0 {
		t.Fatalf("no payment save was recorded; ops=%v", h.rec.Ops())
	}
	if call < 0 {
		t.Fatalf("the gateway was never called; ops=%v", h.rec.Ops())
	}
	if save > call {
		t.Fatalf("the gateway was called before the attempt was committed; ops=%v", h.rec.Ops())
	}
}

// TestTimeoutLeavesPaymentProcessingAndDoesNotFailOver is ADR-013 stated as an executable rule.
//
// Three things must all be true, and each of them is a separate way the platform could
// double-charge or lose a sale:
//
//   - the payment stays PROCESSING, because "we do not know" has exactly one honest state;
//   - the attempt is TIMEOUT_UNKNOWN rather than ERROR, because ERROR would permit a retry;
//   - the second gateway is never called, because money may already have moved at the first.
func TestTimeoutLeavesPaymentProcessingAndDoesNotFailOver(t *testing.T) {
	// Verifies: BR-28, FR-66.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Err: spi.ErrOutcomeUnknown})
	h.adapterB.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})

	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create returned an error; an unknown outcome is a 202, not a failure: %v", err)
	}

	p := h.loadPayment(res.Payment.ID())
	if p.State() != dpayment.StateProcessing {
		t.Fatalf("state = %s, want PROCESSING", p.State())
	}
	attempts := p.Attempts()
	if len(attempts) != 1 {
		t.Fatalf("got %d attempts, want exactly 1: an unknown outcome must not create a second", len(attempts))
	}
	if attempts[0].Outcome() != dpayment.OutcomeTimeoutUnknown {
		t.Fatalf("attempt outcome = %s, want TIMEOUT_UNKNOWN", attempts[0].Outcome())
	}
	if n := len(h.adapterB.Calls()); n != 0 {
		t.Fatalf("the fallback gateway was called %d times after an unknown outcome", n)
	}
	if !hasEvent(h.store.OutboxTypes(), string(dpayment.EventPaymentReconciliationRequired)) {
		t.Fatalf("no reconciliation_required event was published; outbox=%v", h.store.OutboxTypes())
	}
}

// TestSoftDeclineFailsOverAndCreatesASecondAttempt.
//
// The second attempt is the point. Failover must never mutate the first attempt's record: that
// record is the only evidence that a charge may exist at the first gateway, and its idempotency
// key is what a reconciler would use to ask.
func TestSoftDeclineFailsOverAndCreatesASecondAttempt(t *testing.T) {
	// Verifies: BR-22, FR-64.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{
		Result: declinedResult(dpayment.DeclineIssuerUnavailable),
	})
	h.adapterB.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})

	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	p := h.loadPayment(res.Payment.ID())
	attempts := p.Attempts()
	if len(attempts) != 2 {
		t.Fatalf("got %d attempts, want 2 (the failover must create a new one)", len(attempts))
	}
	if attempts[0].GatewayID() != gwA || attempts[1].GatewayID() != gwB {
		t.Fatalf("attempts went to %s then %s, want gw-a then gw-b", attempts[0].GatewayID(), attempts[1].GatewayID())
	}
	if attempts[0].Outcome() != dpayment.OutcomeDeclined {
		t.Fatalf("first attempt = %s, want DECLINED (failover must not rewrite it)", attempts[0].Outcome())
	}
	if attempts[0].IdempotencyKey() == attempts[1].IdempotencyKey() {
		t.Fatal("the two attempts share a gateway idempotency key; the second gateway would deduplicate against a charge it never saw")
	}
	if p.State() != dpayment.StateCaptured {
		t.Fatalf("state = %s, want CAPTURED", p.State())
	}
	if n := successfulAttempts(p); n != 1 {
		t.Fatalf("got %d successful attempts, want exactly 1", n)
	}
}

// TestHardDeclineDoesNotFailOver.
//
// Retrying a hard decline elsewhere is, from the schemes' point of view, indistinguishable from
// card testing. It will not succeed, and doing it at volume gets the platform's gateway accounts
// closed — which is a worse outcome than the declined payment.
func TestHardDeclineDoesNotFailOver(t *testing.T) {
	// Verifies: BR-23, FR-65.
	t.Parallel()
	for _, reason := range []dpayment.DeclineReason{
		dpayment.DeclineStolenCard,
		dpayment.DeclineFraudulent,
		dpayment.DeclineIncorrectCVC,
		dpayment.DeclineUnknown, // an unmapped reason must not be read as permission
	} {

		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			defer h.finish()
			h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Result: declinedResult(reason)})
			h.adapterB.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})

			res, err := h.svc.Create(context.Background(), createCommand())
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			p := h.loadPayment(res.Payment.ID())
			if len(p.Attempts()) != 1 {
				t.Fatalf("got %d attempts, want 1: a hard decline must not fail over", len(p.Attempts()))
			}
			if n := len(h.adapterB.Calls()); n != 0 {
				t.Fatalf("the fallback gateway was called %d times after a hard decline", n)
			}
			if p.State() != dpayment.StateFailed {
				t.Fatalf("state = %s, want FAILED", p.State())
			}
		})
	}
}

// TestDeclineDoesNotCountAgainstTheCircuitBreaker.
//
// A decline is a business outcome, not a gateway failure. Counting it would let one merchant with
// a high-decline customer cohort — or one under a card-testing attack — open the breaker on a
// perfectly healthy gateway and take that gateway out for every other merchant sharing it.
func TestDeclineDoesNotCountAgainstTheCircuitBreaker(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Result: declinedResult(dpayment.DeclineStolenCard)})

	if _, err := h.svc.Create(context.Background(), createCommand()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if len(h.breaker.Outcomes) == 0 {
		t.Fatal("the breaker was never told the outcome")
	}
	for _, o := range h.breaker.Outcomes {
		if o.Counted {
			t.Fatalf("a decline was counted against the breaker for %s", o.Key)
		}
	}
}

// TestUnknownOutcomeDoesCountAgainstTheCircuitBreaker is the counterpart of the test above, and
// it is what stops that rule being over-applied: a gateway that stops answering is exactly what
// the circuit exists to detect, and a timeout is indistinguishable from being down.
func TestUnknownOutcomeDoesCountAgainstTheCircuitBreaker(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Err: spi.ErrOutcomeUnknown})

	if _, err := h.svc.Create(context.Background(), createCommand()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	counted := h.breaker.CountedOutcomes()
	if len(counted) == 0 {
		t.Fatal("an unknown outcome was not counted against the breaker")
	}
}

// TestL6ContractViolationParksThePaymentAsUnknown.
//
// A response that fails validation is a statement about our *knowledge*, not about the payer's
// funds: the gateway may well have authorized, it simply told us something we cannot believe. So
// the attempt becomes TIMEOUT_UNKNOWN and the payment parks for reconciliation, rather than
// failing — and crucially it does *not* fail over, because money may have moved.
func TestL6ContractViolationParksThePaymentAsUnknown(t *testing.T) {
	// Verifies: FR-41.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.validator.responseErr = apierror.New(apierror.CodeGatewayContractViolation,
		"the gateway echoed an amount that does not match the request")
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Result: authorizedResult(mustEUR(9999))})
	h.adapterB.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})

	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	p := h.loadPayment(res.Payment.ID())
	if p.State() != dpayment.StateProcessing {
		t.Fatalf("state = %s, want PROCESSING: a contract violation parks, it does not fail", p.State())
	}
	if got := p.Attempts()[0].Outcome(); got != dpayment.OutcomeTimeoutUnknown {
		t.Fatalf("attempt outcome = %s, want TIMEOUT_UNKNOWN", got)
	}
	if n := len(h.adapterB.Calls()); n != 0 {
		t.Fatalf("the fallback gateway was called %d times after a contract violation", n)
	}
	if !hasEvent(h.store.OutboxTypes(), string(dpayment.EventPaymentReconciliationRequired)) {
		t.Fatalf("no reconciliation_required event; outbox=%v", h.store.OutboxTypes())
	}
}

// TestFailoverNeverProducesTwoSuccessfulAttempts.
//
// The scenario is the dangerous one: the first gateway soft-declines and the second succeeds, and
// then a *third* dispatch is attempted against the same payment. StartAttempt must refuse it,
// because a payment that already has a successful authorization is a payment that has been paid
// for.
func TestFailoverNeverProducesTwoSuccessfulAttempts(t *testing.T) {
	// Verifies: BR-21.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{
		Result: declinedResult(dpayment.DeclineIssuerUnavailable),
	})
	h.adapterB.Script(shared.OpAuthorize, apptest.GatewayScript{Result: authorizedResult(mustEUR(8450))})

	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	p := h.loadPayment(res.Payment.ID())
	if n := successfulAttempts(p); n != 1 {
		t.Fatalf("got %d successful attempts after failover, want 1", n)
	}

	// Re-dispatch the same payment. The aggregate, not the orchestrator, is what refuses.
	plan := planFor(t, res.Payment.ID())
	if _, err := p.StartAttempt(gwA, plan.ID, shared.OpAuthorize, h.clock); err == nil {
		t.Fatal("StartAttempt admitted a second authorization on an already-authorized payment")
	} else if apierror.CodeOf(err) != apierror.CodePaymentAlreadyProcessed {
		t.Fatalf("got %s, want PAYMENT_ALREADY_PROCESSED", apierror.CodeOf(err))
	}
}

// TestUnresolvedAttemptBlocksAnyFurtherAttempt is the other half of the no-double-charge rule:
// not merely "do not fail over now", but "do not let anything start a new attempt later either",
// including an operator retrying by hand.
func TestUnresolvedAttemptBlocksAnyFurtherAttempt(t *testing.T) {
	// Verifies: BR-21, BR-28.
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Err: spi.ErrOutcomeUnknown})

	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	p := h.loadPayment(res.Payment.ID())
	plan := planFor(t, p.ID())
	if _, err := p.StartAttempt(gwB, plan.ID, shared.OpAuthorize, h.clock); err == nil {
		t.Fatal("a new attempt was admitted while an earlier one is unresolved")
	}
	if err := p.MarkFailed(dpayment.DeclineProcessingError, "", h.clock); err == nil {
		t.Fatal("the payment was allowed to fail while an attempt is unresolved")
	}
}

// TestTransportFailureFailsOver.
//
// The distinction from a timeout is the whole reason spi.ErrOutcomeUnknown exists: a connection
// that was refused, a DNS failure or a documented pre-processing 4xx means the gateway provably
// did not act, so a second gateway is a first authorization and not a second charge.
func TestTransportFailureFailsOver(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{
		Err: apierror.New(apierror.CodeGatewayUnavailable, "connection refused"),
	})
	h.adapterB.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})

	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	p := h.loadPayment(res.Payment.ID())
	if len(p.Attempts()) != 2 {
		t.Fatalf("got %d attempts, want 2", len(p.Attempts()))
	}
	if p.Attempts()[0].Outcome() != dpayment.OutcomeError {
		t.Fatalf("first attempt = %s, want ERROR", p.Attempts()[0].Outcome())
	}
	if p.State() != dpayment.StateCaptured {
		t.Fatalf("state = %s, want CAPTURED", p.State())
	}
}

// TestFailoverDisabledStopsAfterTheFirstAttempt exercises the kill switch. It is a switch rather
// than a feature flag: if failover ever misbehaves in production the safe response is to stop
// creating second attempts immediately, without a deploy.
func TestFailoverDisabledStopsAfterTheFirstAttempt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.deps.Settings.EnableFailover = false
	h.svc = NewService(h.deps, h.validator)
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{
		Result: declinedResult(dpayment.DeclineIssuerUnavailable),
	})
	h.adapterB.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})

	res, err := h.svc.Create(context.Background(), createCommand())
	if err == nil {
		p := h.loadPayment(res.Payment.ID())
		if len(p.Attempts()) != 1 {
			t.Fatalf("got %d attempts with failover disabled, want 1", len(p.Attempts()))
		}
	}
	if n := len(h.adapterB.Calls()); n != 0 {
		t.Fatalf("the fallback gateway was called %d times with failover disabled", n)
	}
}

// TestCircuitOpenSkipsToTheNextGateway.
//
// A pre-dispatch refusal leaves the gateway untouched, so moving on is safe — and it is the
// behaviour that makes a breaker useful rather than merely protective: the merchant's payment
// still goes through, on the gateway that is working.
func TestCircuitOpenSkipsToTheNextGateway(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.breaker.Open[gwA.String()+":authorize"] = true
	h.adapterB.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})

	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if n := len(h.adapterA.Calls()); n != 0 {
		t.Fatalf("the open-circuit gateway was called %d times", n)
	}
	p := h.loadPayment(res.Payment.ID())
	if p.State() != dpayment.StateCaptured {
		t.Fatalf("state = %s, want CAPTURED", p.State())
	}
}

// TestUnrecognisedGatewayStatusIsUnknownNotFailed.
//
// An adapter that returns a status the orchestrator does not model is describing a world the
// platform has no state for. Reading it as a failure would assert that no money moved, which is
// exactly the assertion we cannot make.
func TestUnrecognisedGatewayStatusIsUnknownNotFailed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{
		Result: &spi.Result{Status: spi.Status("PARTIALLY_SOMETHING"), GatewayRef: "x"},
	})

	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	p := h.loadPayment(res.Payment.ID())
	if p.State() != dpayment.StateProcessing {
		t.Fatalf("state = %s, want PROCESSING", p.State())
	}
	if got := p.Attempts()[0].Outcome(); got != dpayment.OutcomeTimeoutUnknown {
		t.Fatalf("attempt = %s, want TIMEOUT_UNKNOWN", got)
	}
}

// TestNilResultAndNilErrorIsTreatedAsUnknown covers the broken-adapter case the SPI contract
// forbids. The contract suite asserts it cannot happen; this asserts that if it ever does, the
// reading is "we do not know" rather than "success".
func TestNilResultAndNilErrorIsTreatedAsUnknown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{})

	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	p := h.loadPayment(res.Payment.ID())
	if got := p.Attempts()[0].Outcome(); got != dpayment.OutcomeTimeoutUnknown {
		t.Fatalf("attempt = %s, want TIMEOUT_UNKNOWN", got)
	}
}

// TestStateChangeAndEventCommitTogether asserts the outbox property directly: a transaction that
// fails leaves neither the state change nor the event.
func TestStateChangeAndEventCommitTogether(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})

	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	types := h.store.OutboxTypes()
	if !hasEvent(types, string(dpayment.EventPaymentCreated)) {
		t.Fatalf("payment.created.v1 missing from the outbox: %v", types)
	}
	if !hasEvent(types, string(dpayment.EventPaymentCaptured)) {
		t.Fatalf("payment.captured.v1 missing from the outbox: %v", types)
	}
	p := h.loadPayment(res.Payment.ID())
	if p.State() != dpayment.StateCaptured {
		t.Fatalf("state = %s, want CAPTURED", p.State())
	}
}

// TestRollbackLosesBothTheStateChangeAndTheEvent is the negative half of the pair above, and it
// is what makes the positive half mean something: if the double committed regardless of the
// callback's error, every "they commit together" assertion in this file would be vacuous.
func TestRollbackLosesBothTheStateChangeAndTheEvent(t *testing.T) {
	t.Parallel()
	store := apptest.NewStore()
	uow := apptest.NewUnitOfWork(store, apptest.NewRecorder())
	clock := apptest.NewClock(testEpoch)

	pay, err := dpayment.New(dpayment.NewPaymentParams{
		TenantID: testTenant, MerchantID: testMerchant, Amount: mustEUR(100),
		PaymentMethod:  shared.MethodCard,
		MethodRef:      dpayment.PaymentMethodReference{Token: "tok"},
		IdempotencyKey: "k",
	}, clock)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	injected := errors.New("injected")
	err = uow.Within(context.Background(), func(ctx context.Context, r ports.Repositories) error {
		if err := r.Payments.Create(ctx, pay); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("Within returned %v, want the injected error", err)
	}
	if store.Payment(pay.ID()) != nil {
		t.Fatal("the payment survived a rolled-back transaction")
	}
	if types := store.OutboxTypes(); len(types) != 0 {
		t.Fatalf("the event survived a rolled-back transaction: %v", types)
	}
}

// planFor builds a one-entry plan so a test can call StartAttempt directly.
func planFor(t *testing.T, id shared.PaymentID) *routing.Plan {
	t.Helper()
	plan, err := routing.Decide(
		defaultSnapshot().Routing,
		routing.RequestContext{
			TenantID: testTenant, MerchantID: testMerchant, PaymentID: id,
			Amount: mustEUR(8450), PaymentMethod: shared.MethodCard,
			PayerCountry: "DE", MerchantCountry: "DE", Operation: shared.OpAuthorize,
		},
		eligibleCandidates(gwA, gwB),
		testEpoch,
	)
	if err != nil {
		t.Fatalf("routing.Decide: %v", err)
	}
	return plan
}

func hasEvent(types []string, want string) bool {
	for _, x := range types {
		if x == want {
			return true
		}
	}
	return false
}

// TestAttemptCarriesTheConnectionItDispatchedOver closes the contract gap that
// PaymentAttempt.connectionId used to be. The assertions are about failover as much as about
// stamping: each attempt must name its *own* connection, because the whole reason attempts are
// separate entities is that a failover must not overwrite the previous try's record.
func TestAttemptCarriesTheConnectionItDispatchedOver(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	h.adapterA.Script(shared.OpAuthorize, apptest.GatewayScript{
		Result: declinedResult(dpayment.DeclineIssuerUnavailable),
	})
	h.adapterB.Script(shared.OpAuthorize, apptest.GatewayScript{Result: capturedResult(mustEUR(8450))})

	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	attempts := h.loadPayment(res.Payment.ID()).Attempts()
	if len(attempts) != 2 {
		t.Fatalf("expected a failover to a second attempt, got %d", len(attempts))
	}
	if got := attempts[0].ConnectionID(); got != connA {
		t.Errorf("attempt 1 connection = %q, want %q", got, connA)
	}
	if got := attempts[1].ConnectionID(); got != connB {
		t.Errorf("attempt 2 connection = %q, want %q", got, connB)
	}
}

// TestBindConnectionRefusesToRewriteEvidence pins the aggregate's two invariants: a dispatched
// attempt's connection is a statement about a request already sent, and a failover creates a new
// attempt rather than editing this one.
func TestBindConnectionRefusesToRewriteEvidence(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.finish()
	res, err := h.svc.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	att := h.loadPayment(res.Payment.ID()).Attempts()[0]

	if err := att.BindConnection(att.ConnectionID()); err != nil {
		t.Errorf("re-binding the same connection must be a no-op, got %v", err)
	}
	if err := att.BindConnection(connB); err == nil {
		t.Error("an attempt was re-bound to a different connection")
	}
	if err := att.BindConnection(""); err == nil {
		t.Error("an empty connection reference was accepted")
	}
}
