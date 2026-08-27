package payment

import (
	"context"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/apptest"
	"github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	dpayment "github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/routing"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// The two gateways every test routes between. Named rather than generated so that a failure
// message says "the second attempt went to gw-b", which is the fact being asserted.
const (
	gwA shared.GatewayID = "gw-a"
	gwB shared.GatewayID = "gw-b"
)

var testEpoch = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)

// stubLoader returns one fixed snapshot. The context loader has its own tests; the orchestrator
// tests must not depend on them.
type stubLoader struct {
	snap MerchantSnapshot
	err  error
}

func (l *stubLoader) Load(context.Context, shared.MerchantID) (MerchantSnapshot, error) {
	return l.snap, l.err
}

// stubCandidates returns a fixed candidate set and counts its invocations.
//
// The count is the assertion behind "capture and refund never route": a follow-up operation that
// consulted the router at all would be a defect, and counting is the only way to prove it did
// not — the *result* would look identical either way.
type stubCandidates struct {
	set   []routing.Candidate
	calls int
}

func (c *stubCandidates) Build(context.Context, routing.RequestContext, MerchantSnapshot) ([]routing.Candidate, error) {
	c.calls++
	return c.set, nil
}

// stubRisk returns a fixed decision.
type stubRisk struct {
	decision risk.Decision
	err      error
}

func (r *stubRisk) Evaluate(context.Context, RiskInput) (risk.Decision, error) {
	return r.decision, r.err
}

// stubResolver maps a gateway slug to a scripted adapter.
type stubResolver struct {
	adapters map[shared.GatewayID]spi.PaymentGateway
	err      error
}

func (r *stubResolver) Resolve(_ context.Context, _ shared.MerchantID, g shared.GatewayID) (spi.PaymentGateway, spi.Credentials, string, error) {
	if r.err != nil {
		return nil, spi.Credentials{}, "", r.err
	}
	a, ok := r.adapters[g]
	if !ok {
		return nil, spi.Credentials{}, "", spi.ErrNotSupported
	}
	return a, spi.Credentials{Values: map[string]string{"apiKey": "k"}, Environment: shared.EnvironmentSandbox}, "acct_" + g.String(), nil
}

// stubValidator is permissive by default. Each hook, when set, replaces one level's answer, so a
// test can force exactly one failure without constructing a complete validation subject.
type stubValidator struct {
	createErr   error
	captureErr  error
	refundErr   error
	voidErr     error
	responseErr error
	// responseErrOn limits the L6 failure to one gateway, so a test can assert that a contract
	// violation at the first gateway does not merely fail over to the second.
	responseErrOn shared.GatewayID
}

func (v *stubValidator) ValidateCreate(context.Context, CreateCommand, MerchantSnapshot) error {
	return v.createErr
}

func (v *stubValidator) ValidateCapture(context.Context, *dpayment.Payment, money.Money, MerchantSnapshot) error {
	return v.captureErr
}

func (v *stubValidator) ValidateRefund(context.Context, *dpayment.Payment, money.Money, MerchantSnapshot) error {
	return v.refundErr
}

func (v *stubValidator) ValidateVoid(context.Context, *dpayment.Payment, MerchantSnapshot) error {
	return v.voidErr
}

func (v *stubValidator) ValidateGatewayResponse(_ context.Context, _ GatewayResponse, e ExpectedResponse) error {
	if v.responseErr == nil {
		return nil
	}
	if v.responseErrOn != "" && e.GatewayID != v.responseErrOn {
		return nil
	}
	return v.responseErr
}

// harness wires a complete service against in-memory doubles.
type harness struct {
	t *testing.T

	store    *apptest.Store
	uow      *apptest.UnitOfWork
	rec      *apptest.Recorder
	clock    *apptest.Clock
	breaker  *apptest.Breaker
	bulkhead *apptest.Bulkhead
	metrics  *apptest.Metrics
	audit    *apptest.Auditor

	adapterA *apptest.Gateway
	adapterB *apptest.Gateway

	loader     *stubLoader
	candidates *stubCandidates
	riskStub   *stubRisk
	validator  *stubValidator
	resolver   *stubResolver

	svc  *Service
	orch *Orchestrator
	deps Deps
}

// newHarness builds the default arrangement: an active merchant with two certified connections,
// a plan that prefers gw-a and falls back to gw-b, and an approving risk decision.
func newHarness(t *testing.T) *harness {
	t.Helper()
	store := apptest.NewStore()
	rec := apptest.NewRecorder()
	h := &harness{
		t:        t,
		store:    store,
		rec:      rec,
		uow:      apptest.NewUnitOfWork(store, rec),
		clock:    apptest.NewClock(testEpoch),
		breaker:  apptest.NewBreaker(),
		bulkhead: apptest.NewBulkhead(),
		metrics:  apptest.NewMetrics(),
		audit:    &apptest.Auditor{Store: store},
		adapterA: apptest.NewGateway(gwA, rec),
		adapterB: apptest.NewGateway(gwB, rec),
	}

	h.loader = &stubLoader{snap: defaultSnapshot()}
	h.candidates = &stubCandidates{set: eligibleCandidates(gwA, gwB)}
	h.riskStub = &stubRisk{decision: risk.Decision{Outcome: risk.OutcomeApprove}}
	h.validator = &stubValidator{}
	h.resolver = &stubResolver{adapters: map[shared.GatewayID]spi.PaymentGateway{
		gwA: h.adapterA, gwB: h.adapterB,
	}}

	h.deps = Deps{
		UoW:        h.uow,
		Config:     apptest.NewConfigProvider(),
		Merchants:  h.loader,
		Gateways:   h.resolver,
		Candidates: h.candidates,
		Risk:       h.riskStub,
		Breakers:   h.breaker,
		Bulkheads:  h.bulkhead,
		Metrics:    h.metrics,
		Audit:      h.audit,
		Clock:      h.clock,
		Settings:   DefaultConfig(),
	}
	h.svc = NewService(h.deps, h.validator)
	h.orch = NewOrchestrator(h.deps, h.validator)
	return h
}

// finish asserts the invariants every test shares: no bulkhead slot leaked, and no payment ended
// up with two successful authorization attempts.
//
// Both are checked here rather than in each test because both are properties of *every* run, and
// a leaked bulkhead slot in particular is invisible in the test that caused it and fatal in
// production three weeks later.
func (h *harness) finish() {
	h.t.Helper()
	if h.bulkhead.InFlight != 0 {
		h.t.Fatalf("bulkhead leaked %d slots; a release did not run", h.bulkhead.InFlight)
	}
	for _, p := range h.store.AllPayments() {
		n := 0
		for _, a := range p.Attempts() {
			if a.Outcome() == dpayment.OutcomeSuccess && a.Operation() == shared.OpAuthorize {
				n++
			}
		}
		if n > 1 {
			h.t.Fatalf("payment %s has %d successful authorization attempts; invariant I3 broken", p.ID(), n)
		}
	}
}

func defaultSnapshot() MerchantSnapshot {
	return MerchantSnapshot{
		MerchantID:    testMerchant,
		TenantID:      testTenant,
		Environment:   shared.EnvironmentSandbox,
		Country:       "DE",
		RiskRating:    string(risk.RatingStandard),
		Status:        merchant.StatusActive,
		ConfigPresent: true,
		ConfigVersion: 4,
		SupportedCurrencies: []money.Currency{
			money.Currency("EUR"), money.Currency("USD"),
		},
		PaymentMethods:       []shared.PaymentMethod{shared.MethodCard},
		ManualCaptureAllowed: true,
		Routing: routing.Policy{
			Strategy:          routing.StrategyPriorityWithFallback,
			Primary:           gwA,
			Fallbacks:         []shared.GatewayID{gwB},
			ConnectedGateways: []shared.GatewayID{gwA, gwB},
		},
		Risk: risk.Policy{
			MaxTransactionAmount: money.MustNew(1000000, "EUR"),
			Version:              4,
		},
		MaxRefundWindow:    180 * 24 * time.Hour,
		MaxPartialCaptures: 4,
		Connections: map[shared.GatewayID]ConnectionSnapshot{
			gwA: {ConnectionID: connA, GatewayID: gwA, ExternalAccountID: "acct_a", Status: gateway.StatusCertified, Certified: true, SecretRef: "secret://sandbox/t/m/gw-a"},
			gwB: {ConnectionID: connB, GatewayID: gwB, ExternalAccountID: "acct_b", Status: gateway.StatusCertified, Certified: true, SecretRef: "secret://sandbox/t/m/gw-b"},
		},
	}
}

const (
	// connA and connB are the merchant-to-gateway bindings the fixtures dispatch over. They exist
	// so that the orchestrator's connection stamping is exercised by every scenario rather than by
	// one dedicated test: a failover that lost the binding would show up as a wrong connection on
	// the second attempt, which is what these two make visible.
	connA shared.ConnectionID = "gwc_01HZTESTCONNECTIONA00000"
	connB shared.ConnectionID = "gwc_01HZTESTCONNECTIONB00000"

	testTenant   shared.TenantID   = "ten_01HZTESTTENANT00000000000"
	testMerchant shared.MerchantID = "mrc_01HZTESTMERCHANT000000000"
)

// eligibleCandidates returns candidates that pass every hard filter, so that a routing test that
// is not about eligibility cannot fail for an eligibility reason.
func eligibleCandidates(ids ...shared.GatewayID) []routing.Candidate {
	out := make([]routing.Candidate, 0, len(ids))
	for i, id := range ids {
		out = append(out, routing.Candidate{
			GatewayID:          id,
			TenantEntitled:     true,
			ResidencyCompliant: true,
			MerchantConfigured: true,
			Certified:          true,
			Healthy:            true,
			SupportsCurrency:   true,
			SupportsMethod:     true,
			SupportsCountry:    true,
			SupportsOperation:  true,
			SupportsThreeDS:    true,
			HealthScore:        1.0,
			SuccessRate:        0.95,
			CostMinorUnits:     int64(200 + i*10),
			LatencyP99MS:       300,
		})
	}
	return out
}

func createCommand() CreateCommand {
	return CreateCommand{
		TenantID:   testTenant,
		MerchantID: testMerchant,
		Amount:     money.MustNew(8450, "EUR"),
		Method:     shared.MethodCard,
		MethodRef: dpayment.PaymentMethodReference{
			Token: "tok_test", Brand: "visa", Last4: "4242",
			ExpMonth: 12, ExpYear: testEpoch.Year() + 2, Country: "DE",
		},
		CaptureMethod:  dpayment.CaptureAutomatic,
		Description:    "order 1",
		Customer:       dpayment.CustomerReference{MerchantCustomerID: "cus_1", Country: "DE"},
		IdempotencyKey: "idem-1",
		CorrelationID:  "corr-1",
	}
}

// authorizedResult is a well-formed authorization response.
func authorizedResult(amount money.Money) *spi.Result {
	return &spi.Result{
		Status: spi.StatusAuthorized, GatewayRef: "gwref_ok",
		AuthorizedAmount: &amount, RawStatus: "authorized",
	}
}

func capturedResult(amount money.Money) *spi.Result {
	return &spi.Result{
		Status: spi.StatusCaptured, GatewayRef: "gwref_cap",
		CapturedAmount: &amount, RawStatus: "captured",
	}
}

func declinedResult(reason dpayment.DeclineReason) *spi.Result {
	return &spi.Result{
		Status: spi.StatusDeclined, GatewayRef: "gwref_dec",
		DeclineReason: reason, RawStatus: "declined", RawCode: string(reason),
	}
}

// loadPayment reads a payment straight out of the store, bypassing the service, so an assertion
// describes what was actually committed rather than what the service chose to return.
func (h *harness) loadPayment(id shared.PaymentID) *dpayment.Payment {
	h.t.Helper()
	p := h.store.Payment(id)
	if p == nil {
		h.t.Fatalf("payment %s is not in the store", id)
	}
	return p
}

// successfulAttempts counts the attempts that succeeded, which is the number every failover test
// asserts is at most one.
func successfulAttempts(p *dpayment.Payment) int {
	n := 0
	for _, a := range p.Attempts() {
		if a.Outcome() == dpayment.OutcomeSuccess {
			n++
		}
	}
	return n
}

func mustEUR(n int64) money.Money { return money.MustNew(n, "EUR") }
