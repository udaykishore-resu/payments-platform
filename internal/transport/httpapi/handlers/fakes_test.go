package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	appconfig "github.com/udaykishore-resu/payments-platform/internal/application/config"
	appmerchant "github.com/udaykishore-resu/payments-platform/internal/application/merchant"
	apponboarding "github.com/udaykishore-resu/payments-platform/internal/application/onboarding"
	apppayment "github.com/udaykishore-resu/payments-platform/internal/application/payment"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	appwebhook "github.com/udaykishore-resu/payments-platform/internal/application/webhook"
	domainconfig "github.com/udaykishore-resu/payments-platform/internal/domain/config"
	domaingateway "github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/routing"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/health"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi/handlers"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// The test doubles.
//
// Each one is a function field per method, so a table row can script exactly the one call it
// cares about and leave the rest nil. A double with a single behaviour per type would force every
// test that needs a failure to construct a whole new type; a double with a per-method closure
// keeps the interesting part of the test inside the table row where a reader is already looking.

const (
	testTenantID   = shared.TenantID("ten_01JB8Z00000000000000000000")
	testMerchantID = shared.MerchantID("mrc_01JB8Z11111111111111111111")
	testPaymentID  = shared.PaymentID("pay_01JB8Z9K2QW3E4R5T6Y7A8B9C0")
	testGatewayID  = shared.GatewayID("stripe")
	// testConnectionID is the merchant-to-gateway binding the fixtures dispatch over. It is a
	// real `gwc_` ULID rather than a placeholder because the contract's ConnectionId schema
	// declares that pattern and the contract test validates the rendered response against it.
	testConnectionID = shared.ConnectionID("gwc_01JB8Z88888888888888888888")
)

var testClock = shared.FixedClock{T: time.Date(2026, 8, 26, 14, 3, 10, 0, time.UTC)}

type fakePayments struct {
	create  func(context.Context, apppayment.CreateCommand) (*apppayment.Result, error)
	get     func(context.Context, shared.PaymentID) (*payment.Payment, error)
	list    func(context.Context, ports.PaymentFilter, ports.Page) ([]*payment.Payment, string, error)
	capture func(context.Context, apppayment.CaptureCommand) (*apppayment.Result, error)
	refund  func(context.Context, apppayment.RefundCommand) (*apppayment.Result, error)
	void    func(context.Context, apppayment.VoidCommand) (*apppayment.Result, error)
}

func (f *fakePayments) Create(ctx context.Context, c apppayment.CreateCommand) (*apppayment.Result, error) {
	return f.create(ctx, c)
}
func (f *fakePayments) Get(ctx context.Context, id shared.PaymentID) (*payment.Payment, error) {
	return f.get(ctx, id)
}
func (f *fakePayments) List(ctx context.Context, fl ports.PaymentFilter, p ports.Page) ([]*payment.Payment, string, error) {
	return f.list(ctx, fl, p)
}
func (f *fakePayments) Capture(ctx context.Context, c apppayment.CaptureCommand) (*apppayment.Result, error) {
	return f.capture(ctx, c)
}
func (f *fakePayments) Refund(ctx context.Context, c apppayment.RefundCommand) (*apppayment.Result, error) {
	return f.refund(ctx, c)
}
func (f *fakePayments) Void(ctx context.Context, c apppayment.VoidCommand) (*apppayment.Result, error) {
	return f.void(ctx, c)
}

type fakeMerchants struct {
	create func(context.Context, appmerchant.CreateCommand) (*merchant.Merchant, error)
	get    func(context.Context, shared.TenantID, shared.MerchantID) (*merchant.Merchant, error)
	list   func(context.Context, shared.TenantID, ports.MerchantFilter, ports.Page) ([]*merchant.Merchant, string, error)
	update func(context.Context, appmerchant.UpdateCommand) (*merchant.Merchant, error)
}

func (f *fakeMerchants) Create(ctx context.Context, c appmerchant.CreateCommand) (*merchant.Merchant, error) {
	return f.create(ctx, c)
}
func (f *fakeMerchants) Get(ctx context.Context, t shared.TenantID, id shared.MerchantID) (*merchant.Merchant, error) {
	return f.get(ctx, t, id)
}
func (f *fakeMerchants) List(ctx context.Context, t shared.TenantID, fl ports.MerchantFilter, p ports.Page) ([]*merchant.Merchant, string, error) {
	return f.list(ctx, t, fl, p)
}
func (f *fakeMerchants) Update(ctx context.Context, c appmerchant.UpdateCommand) (*merchant.Merchant, error) {
	return f.update(ctx, c)
}

type fakeOnboarding struct {
	start  func(context.Context, apponboarding.StartCommand) (*apponboarding.Case, error)
	get    func(context.Context, shared.TenantID, shared.WorkflowID) (*apponboarding.Case, error)
	signal func(context.Context, apponboarding.SignalCommand) (*apponboarding.Case, error)
}

func (f *fakeOnboarding) Start(ctx context.Context, c apponboarding.StartCommand) (*apponboarding.Case, error) {
	return f.start(ctx, c)
}
func (f *fakeOnboarding) Get(ctx context.Context, t shared.TenantID, id shared.WorkflowID) (*apponboarding.Case, error) {
	return f.get(ctx, t, id)
}
func (f *fakeOnboarding) Signal(ctx context.Context, c apponboarding.SignalCommand) (*apponboarding.Case, error) {
	return f.signal(ctx, c)
}

type fakeLookup struct {
	workflow func(context.Context, shared.TenantID, shared.MerchantID) (shared.WorkflowID, error)
}

func (f *fakeLookup) WorkflowFor(ctx context.Context, t shared.TenantID, m shared.MerchantID) (shared.WorkflowID, error) {
	if f.workflow == nil {
		return "wfr_01JB8Z33333333333333333333", nil
	}
	return f.workflow(ctx, t, m)
}

type fakeConfig struct {
	getActive    func(context.Context, shared.TenantID, shared.MerchantID) (*domainconfig.MerchantConfig, error)
	listVersions func(context.Context, shared.TenantID, shared.MerchantID, ports.Page) ([]*domainconfig.MerchantConfig, string, error)
	publish      func(context.Context, appconfig.PublishCommand) (*domainconfig.MerchantConfig, error)
	rollback     func(context.Context, appconfig.RollbackCommand) (*domainconfig.MerchantConfig, error)
}

func (f *fakeConfig) GetActive(ctx context.Context, t shared.TenantID, m shared.MerchantID) (*domainconfig.MerchantConfig, error) {
	return f.getActive(ctx, t, m)
}
func (f *fakeConfig) ListVersions(ctx context.Context, t shared.TenantID, m shared.MerchantID, p ports.Page) ([]*domainconfig.MerchantConfig, string, error) {
	return f.listVersions(ctx, t, m, p)
}
func (f *fakeConfig) Publish(ctx context.Context, c appconfig.PublishCommand) (*domainconfig.MerchantConfig, error) {
	return f.publish(ctx, c)
}
func (f *fakeConfig) Rollback(ctx context.Context, c appconfig.RollbackCommand) (*domainconfig.MerchantConfig, error) {
	return f.rollback(ctx, c)
}

type fakeGateways struct {
	get    func(context.Context, shared.GatewayID) (*domaingateway.Gateway, error)
	list   func(context.Context) ([]*domaingateway.Gateway, error)
	health func(context.Context, shared.GatewayID, []shared.Operation) ([]*domaingateway.Health, error)
	rotate func(context.Context, handlers.RotateCommand) (*handlers.RotationAccepted, error)
}

func (f *fakeGateways) Get(ctx context.Context, id shared.GatewayID) (*domaingateway.Gateway, error) {
	return f.get(ctx, id)
}
func (f *fakeGateways) List(ctx context.Context) ([]*domaingateway.Gateway, error) {
	return f.list(ctx)
}
func (f *fakeGateways) Health(ctx context.Context, id shared.GatewayID, ops []shared.Operation) ([]*domaingateway.Health, error) {
	return f.health(ctx, id, ops)
}
func (f *fakeGateways) RotateCredentials(ctx context.Context, c handlers.RotateCommand) (*handlers.RotationAccepted, error) {
	return f.rotate(ctx, c)
}

type fakeWebhooks struct {
	ingest func(context.Context, appwebhook.InboundRequest) (*appwebhook.Accepted, error)
}

func (f *fakeWebhooks) Ingest(ctx context.Context, r appwebhook.InboundRequest) (*appwebhook.Accepted, error) {
	return f.ingest(ctx, r)
}

type fakeHealth struct {
	live  health.Response
	ready health.Response
}

func (f *fakeHealth) Live(context.Context) health.Response  { return f.live }
func (f *fakeHealth) Ready(context.Context) health.Response { return f.ready }

// --- fixtures ------------------------------------------------------------------------------------

// newPayment builds an authorized payment with one successful attempt, which is the shape most
// response assertions need.
func newPayment() *payment.Payment {
	amount := money.MustNew(1050, "USD")
	p, err := payment.New(payment.NewPaymentParams{
		TenantID:       testTenantID,
		MerchantID:     testMerchantID,
		Amount:         amount,
		PaymentMethod:  shared.MethodCard,
		MethodRef:      payment.PaymentMethodReference{Token: "tok_visa_ok", Brand: "VISA", Last4: "4242"},
		CaptureMethod:  payment.CaptureManual,
		IdempotencyKey: "k1",
		CorrelationID:  "req_01JB8Z22222222222222222222",
	}, testClock)
	if err != nil {
		panic(err)
	}
	att, err := p.StartAttempt(testGatewayID, "rpl_01JB8ZEEEEEEEEEEEEEEEEEEEE", shared.OpAuthorize, testClock)
	if err != nil {
		panic(err)
	}
	// The orchestrator binds the connection before committing the attempt; the fixture does the
	// same, because the contract requires connectionId on every rendered attempt and a fixture
	// that skipped it would assert the contract against a shape production never produces.
	if err := att.BindConnection(testConnectionID); err != nil {
		panic(err)
	}
	if err := att.Dispatch(testClock.Now()); err != nil {
		panic(err)
	}
	// CREATED -> PROCESSING -> AUTHORIZED is the machine's only legal route to an authorization.
	// The intermediate state is not ceremony: it is the state a payment is in while the gateway
	// call is outstanding, and it is what a timeout leaves behind.
	if err := p.MarkProcessing(testClock); err != nil {
		panic(err)
	}
	if err := att.Succeed("gw_ref_1", "authorized", testClock.Now()); err != nil {
		panic(err)
	}
	if err := p.MarkAuthorized(amount, nil, testClock); err != nil {
		panic(err)
	}
	return p
}

// newProcessingPayment builds the ambiguous-outcome shape: a dispatched attempt that timed out,
// with the payment left PROCESSING. This is the §12.3 case the 202 mapping exists for.
func newProcessingPayment() *payment.Payment {
	amount := money.MustNew(1050, "USD")
	p, err := payment.New(payment.NewPaymentParams{
		TenantID: testTenantID, MerchantID: testMerchantID, Amount: amount,
		PaymentMethod:  shared.MethodCard,
		MethodRef:      payment.PaymentMethodReference{Token: "tok_visa_timeout"},
		CaptureMethod:  payment.CaptureAutomatic,
		IdempotencyKey: "k-timeout",
		CorrelationID:  "req_01JB8Z22222222222222222222",
	}, testClock)
	if err != nil {
		panic(err)
	}
	att, err := p.StartAttempt(testGatewayID, "rpl_1", shared.OpAuthorize, testClock)
	if err != nil {
		panic(err)
	}
	if err := att.BindConnection(testConnectionID); err != nil {
		panic(err)
	}
	if err := att.Dispatch(testClock.Now()); err != nil {
		panic(err)
	}
	if err := p.MarkProcessing(testClock); err != nil {
		panic(err)
	}
	if err := att.TimeOut("gateway did not respond", testClock.Now()); err != nil {
		panic(err)
	}
	return p
}

func newMerchant() *merchant.Merchant {
	m, err := merchant.New(merchant.NewParams{
		TenantID:    testTenantID,
		LegalName:   "Acme Trading Ltd",
		DisplayName: "Acme",
		ExternalRef: "acme-eu-01",
		Environment: shared.EnvironmentSandbox,
		Profile: merchant.BusinessProfile{
			LegalEntityType:       "PRIVATE_LIMITED",
			RegistrationNumber:    "12345678",
			TaxIDLast4:            "6789",
			Country:               "DE",
			MCC:                   "5411",
			WebsiteURL:            "https://acme.example",
			SupportEmail:          "support@acme.example",
			ExpectedMonthlyVolume: money.MustNew(1000000, "EUR"),
			ExpectedAverageTicket: money.MustNew(5000, "EUR"),
		},
	}, testClock)
	if err != nil {
		panic(err)
	}
	return m
}

func newConfig() *domainconfig.MerchantConfig {
	return &domainconfig.MerchantConfig{
		MerchantID:          testMerchantID,
		TenantID:            testTenantID,
		Version:             3,
		Status:              domainconfig.StatusActive,
		Environment:         shared.EnvironmentSandbox,
		SupportedCurrencies: []money.Currency{"EUR", "USD"},
		PaymentMethods:      []shared.PaymentMethod{shared.MethodCard},
		Countries:           []shared.Country{"DE", "FR"},
		Routing: routing.Policy{
			Strategy:  routing.StrategyPriorityWithFallback,
			Primary:   testGatewayID,
			Fallbacks: []shared.GatewayID{"adyen"},
		},
		Risk: risk.Policy{
			MaxTransactionAmount: money.MustNew(500000, "EUR"),
			DailyVolumeLimit:     money.MustNew(10000000, "EUR"),
			Velocity:             risk.Velocity{MaxPaymentsPerMinute: 100, MaxPerCardPerHour: 5},
		},
		Limits: domainconfig.Limits{MaxRefundWindowDays: 180, MaxPartialCaptures: 5},
		Webhook: domainconfig.WebhookConfig{
			MaxAttempts: 8,
			Backoff:     "EXPONENTIAL_JITTER",
			Endpoints: []domainconfig.WebhookEndpoint{{
				URL: "https://acme.example/hooks", Events: []string{"payment.*"},
				SecretRef: "secret://merchants/acme/webhook", Active: true,
			}},
		},
		Settle:    domainconfig.SettlementConfig{Schedule: "DAILY", Currency: "EUR", HoldDays: 2},
		CreatedAt: testClock.Now(),
		CreatedBy: "usr_test",
	}
}

func newGateway() *domaingateway.Gateway {
	g, err := domaingateway.NewGateway(domaingateway.NewGatewayParams{
		ID:          testGatewayID,
		DisplayName: "Stripe",
		Vendor:      "stripe",
		APIVersion:  "2024-06-20",
		BaseURLs:    map[shared.Environment]string{shared.EnvironmentSandbox: "https://sandbox.stripe.example"},
		Capabilities: domaingateway.Capabilities{
			Countries:  []shared.Country{"DE", "US"},
			Currencies: []money.Currency{"EUR", "USD"},
			Methods:    []shared.PaymentMethod{shared.MethodCard},
			Operations: []shared.Operation{
				shared.OpAuthorize, shared.OpCapture, shared.OpRefund, shared.OpVoid,
			},
			SupportsPartialCapture: true,
			SupportsPartialRefund:  true,
			SupportsVoid:           true,
			Supports3DS2:           true,
			MaxRefundWindow:        180 * 24 * time.Hour,
			AuthorizationValidity:  7 * 24 * time.Hour,
		},
		SignatureScheme: domaingateway.SchemeHMACSHA256,
		Status:          domaingateway.StatusActive,
	}, testClock)
	if err != nil {
		panic(err)
	}
	return g
}

func newGatewayHealth() *domaingateway.Health {
	h, err := domaingateway.NewHealth(testGatewayID, shared.OpAuthorize, testClock)
	if err != nil {
		panic(err)
	}
	return h
}

func newCase() *apponboarding.Case {
	return &apponboarding.Case{
		WorkflowID:  "wfr_01JB8Z33333333333333333333",
		MerchantID:  testMerchantID,
		TenantID:    testTenantID,
		Definition:  "merchant-onboarding",
		Version:     1,
		CurrentStep: "validate-merchant",
		CreatedAt:   testClock.Now(),
		UpdatedAt:   testClock.Now(),
		Steps: []apponboarding.StepView{
			{Name: "validate-merchant", Sequence: 1, State: "RUNNING", Attempt: 1},
			{Name: "submit-kyc", Sequence: 2, State: "PENDING"},
		},
	}
}

// --- harness -------------------------------------------------------------------------------------

// newRouter builds a router with the supplied deps and the defaults every test needs.
func newRouter(d handlers.Deps) *httpapi.Router {
	if d.Health == nil {
		d.Health = &fakeHealth{
			live:  health.Response{Status: health.StatusUp},
			ready: health.Response{Status: health.StatusUp},
		}
	}
	if d.Service == "" {
		d.Service = "payment-api"
	}
	if d.Version == "" {
		d.Version = "test"
	}
	if d.BaseURL == "" {
		d.BaseURL = "https://api.example.com"
	}
	rt := httpapi.NewRouter()
	handlers.Register(rt, d)
	return rt
}

// do issues a request through the router with a tenant context already installed, standing in for
// the authentication and tenant stages that run above the handler in production.
func do(rt *httpapi.Router, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set(httpapi.HeaderContentType, httpapi.MediaJSON)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	ctx := httpapi.WithRequestID(req.Context(), "req_01JB8Z22222222222222222222")
	ctx = httpapi.WithRawBody(ctx, []byte(body))
	ctx, err := tenantctx.WithTenant(ctx, tenantctx.TenantContext{
		TenantID:    testTenantID,
		Tier:        shared.TierPooled,
		Environment: shared.EnvironmentSandbox,
		Principal:   tenantctx.Principal{Type: tenantctx.PrincipalMachine, ID: "cli_test", Name: "test client"},
		Scopes:      []string{"payments:write", "merchants:write", "config:write"},
		RequestID:   "req_01JB8Z22222222222222222222",
		Source:      tenantctx.SourceToken,
	})
	if err != nil {
		panic(err)
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

// okResult wraps a payment in the shape the service returns on success.
func okResult(p *payment.Payment) *apppayment.Result {
	return &apppayment.Result{
		Payment: p,
		Risk:    risk.Decision{Outcome: risk.OutcomeApprove, Score: 12},
	}
}

var _ = http.MethodGet
