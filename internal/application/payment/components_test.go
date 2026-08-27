package payment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/apptest"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	dconfig "github.com/udaykishore-resu/payments-platform/internal/domain/config"
	dgateway "github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	dmerchant "github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	dpayment "github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/routing"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	dtenant "github.com/udaykishore-resu/payments-platform/internal/domain/tenant"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// --- the fail-static cliff ------------------------------------------------------------------

// TestContextLoaderServesWithinTolerance.
func TestContextLoaderServesWithinTolerance(t *testing.T) {
	// Verifies: FR-47.
	t.Parallel()
	env := newLoaderEnv(t)
	env.provider.SetAge(2 * time.Minute)

	snap, err := env.loader.Load(context.Background(), env.merchantID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !snap.ConfigPresent {
		t.Fatal("the configuration was not resolved")
	}
	// The status is whatever the registry says, verbatim. The loader must not improve on it:
	// a snapshot that reported ACTIVE for a merchant the registry has in CREATED would let a
	// half-onboarded merchant process live money.
	if snap.Status != dmerchant.StatusCreated {
		t.Fatalf("status = %s, want the registry's own CREATED", snap.Status)
	}
	if len(snap.Connections) != 1 {
		t.Fatalf("got %d connections, want 1", len(snap.Connections))
	}
	if snap.SnapshotAge != 2*time.Minute {
		t.Fatalf("snapshot age = %s, want 2m", snap.SnapshotAge)
	}
}

// TestContextLoaderCliffRefusesUnknownMerchantsAndKeepsServingKnownOnes is baseline §15.
//
// The asymmetry is the design. Refusing everyone converts a control-plane outage into a total
// payment outage — which is precisely what the data plane's independence exists to prevent —
// while serving everyone means processing an unknown merchant's first payment with no limits, no
// blocked countries and no enabled-currency check at all.
func TestContextLoaderCliffRefusesUnknownMerchantsAndKeepsServingKnownOnes(t *testing.T) {
	// Verifies: FR-48, NFR-22.
	t.Parallel()
	env := newLoaderEnv(t)

	// Serve once while fresh, so this merchant becomes "already in the snapshot".
	if _, err := env.loader.Load(context.Background(), env.merchantID); err != nil {
		t.Fatalf("warm Load: %v", err)
	}

	env.provider.SetAge(30 * time.Minute)

	if _, err := env.loader.Load(context.Background(), env.merchantID); err != nil {
		t.Fatalf("a merchant already in the snapshot was refused past the cliff: %v", err)
	}

	_, err := env.loader.Load(context.Background(), shared.MerchantID("mrc_01HZUNSEENMERCHANT0000000"))
	if err == nil {
		t.Fatal("a merchant not in the snapshot was served past the cliff")
	}
	if apierror.CodeOf(err) != apierror.CodeConfigurationStale {
		t.Fatalf("got %s, want CONFIGURATION_STALE", apierror.CodeOf(err))
	}
	if !apierror.IsRetryable(err) {
		t.Fatal("the stale-configuration error is not retryable; a client cannot act on it")
	}
}

// TestContextLoaderInvalidateBeatsTheFailStaticBehaviour.
//
// A suspension must not be survivable by the last-known-good snapshot: a merchant suspended
// during a control-plane outage would otherwise keep processing on the strength of a document
// taken before they were stopped.
func TestContextLoaderInvalidateBeatsTheFailStaticBehaviour(t *testing.T) {
	t.Parallel()
	env := newLoaderEnv(t)
	if _, err := env.loader.Load(context.Background(), env.merchantID); err != nil {
		t.Fatalf("warm Load: %v", err)
	}
	env.loader.Invalidate(env.merchantID)
	env.provider.SetAge(30 * time.Minute)

	if _, err := env.loader.Load(context.Background(), env.merchantID); err == nil {
		t.Fatal("an invalidated merchant was still served past the cliff")
	}
}

// TestContextLoaderMemoisesWithinOneRequest.
//
// Every stage of the create pipeline reads the merchant context; they must all read the same one,
// or a configuration publish landing mid-request validates a payment under one version and routes
// it under another.
func TestContextLoaderMemoisesWithinOneRequest(t *testing.T) {
	t.Parallel()
	env := newLoaderEnv(t)
	ctx := WithRequestCache(context.Background())

	first, err := env.loader.Load(ctx, env.merchantID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Change the world underneath: without the memo the second read would see version 2.
	env.publish(2)

	second, err := env.loader.Load(ctx, env.merchantID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if first.ConfigVersion != second.ConfigVersion {
		t.Fatalf("two reads in one request saw versions %d and %d",
			first.ConfigVersion, second.ConfigVersion)
	}
	// A *different* request must see the new version, or the memo would be a cache.
	fresh, err := env.loader.Load(WithRequestCache(context.Background()), env.merchantID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if fresh.ConfigVersion != 2 {
		t.Fatalf("a new request saw version %d, want 2", fresh.ConfigVersion)
	}
}

type loaderEnv struct {
	t          *testing.T
	store      *apptest.Store
	provider   *apptest.ConfigProvider
	loader     *ContextLoader
	merchantID shared.MerchantID
	tenantID   shared.TenantID
}

func newLoaderEnv(t *testing.T) *loaderEnv {
	t.Helper()
	store := apptest.NewStore()
	clock := apptest.NewClock(testEpoch)
	m, err := dmerchant.New(dmerchant.NewParams{
		TenantID: testTenant, LegalName: "Test GmbH", Environment: shared.EnvironmentSandbox,
		Profile: dmerchant.BusinessProfile{Country: "DE", MCC: "5734"},
	}, clock)
	if err != nil {
		t.Fatalf("merchant.New: %v", err)
	}
	store.PutMerchant(m)

	conn := certifiedConnection(t, m.TenantID(), m.ID(), gwA, clock)
	store.PutConnection(conn)

	env := &loaderEnv{
		t: t, store: store, provider: apptest.NewConfigProvider(),
		merchantID: m.ID(), tenantID: m.TenantID(),
	}
	env.publish(1)
	env.loader = NewContextLoader(ContextLoaderDeps{
		UoW:    apptest.NewUnitOfWork(store, apptest.NewRecorder()),
		Config: env.provider,
		Clock:  clock,
	})
	return env
}

func (e *loaderEnv) publish(version int) {
	e.provider.Put(&dconfig.MerchantConfig{
		MerchantID: e.merchantID, TenantID: e.tenantID, Version: version,
		Status: dconfig.StatusActive, Environment: shared.EnvironmentSandbox,
		SupportedCurrencies: []money.Currency{"EUR"},
		PaymentMethods:      []shared.PaymentMethod{shared.MethodCard},
		Countries:           []shared.Country{"DE"},
		Routing: routing.Policy{
			Strategy: routing.StrategyPriorityWithFallback, Primary: gwA,
			Fallbacks: []shared.GatewayID{gwB},
		},
		Risk:   risk.Policy{MaxTransactionAmount: money.MustNew(500000, "EUR")},
		Limits: dconfig.Limits{MaxRefundWindowDays: 180, MaxPartialCaptures: 4},
	})
}

func certifiedConnection(t *testing.T, tenantID shared.TenantID, m shared.MerchantID,
	g shared.GatewayID, clock shared.Clock) *dgateway.Connection {
	t.Helper()
	c, err := dgateway.NewConnection(dgateway.NewConnectionParams{
		TenantID: tenantID, MerchantID: m, GatewayID: g, Environment: shared.EnvironmentSandbox,
	}, clock)
	if err != nil {
		t.Fatalf("NewConnection: %v", err)
	}
	if err := c.BeginProvisioning(clock); err != nil {
		t.Fatalf("BeginProvisioning: %v", err)
	}
	if err := c.CompleteProvisioning("acct_"+g.String(), "secret://sandbox/t/m/"+g.String(), clock); err != nil {
		t.Fatalf("CompleteProvisioning: %v", err)
	}
	if err := c.BeginCertification(clock); err != nil {
		t.Fatalf("BeginCertification: %v", err)
	}
	if err := c.Certify("crt_1", clock); err != nil {
		t.Fatalf("Certify: %v", err)
	}
	c.DrainEvents()
	return c
}

// --- candidate construction -----------------------------------------------------------------

// TestCandidateFieldsComeFromRealSources.
//
// Every boolean on routing.Candidate is a hard filter. A `true` that was not derived from a
// source does not make routing slightly less accurate — it disables a filter, and the filters are
// legal and contractual constraints. This table flips one source at a time and asserts the
// corresponding field went false; a hard-coded `true` anywhere in the builder fails one of these
// rows.
func TestCandidateFieldsComeFromRealSources(t *testing.T) {
	// Verifies: FR-62.
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*candidateEnv)
		field  func(routing.Candidate) bool
	}{
		{
			name:   "tenant entitlement",
			mutate: func(e *candidateEnv) { e.entitled = false },
			field:  func(c routing.Candidate) bool { return c.TenantEntitled },
		},
		{
			name:   "residency: an unrecorded processing region is refused",
			mutate: func(e *candidateEnv) { e.region = "" },
			field:  func(c routing.Candidate) bool { return c.ResidencyCompliant },
		},
		{
			name:   "connection is not certified",
			mutate: func(e *candidateEnv) { e.certified = false },
			field:  func(c routing.Candidate) bool { return c.Certified },
		},
		{
			name:   "no connection at all",
			mutate: func(e *candidateEnv) { e.connected = false },
			field:  func(c routing.Candidate) bool { return c.MerchantConfigured },
		},
		{
			name:   "the descriptor does not list the currency",
			mutate: func(e *candidateEnv) { e.currencies = []money.Currency{"USD"} },
			field:  func(c routing.Candidate) bool { return c.SupportsCurrency },
		},
		{
			name:   "the descriptor does not list the method",
			mutate: func(e *candidateEnv) { e.methods = []shared.PaymentMethod{shared.MethodPayPal} },
			field:  func(c routing.Candidate) bool { return c.SupportsMethod },
		},
		{
			name:   "the gateway is not licensed in the merchant's country",
			mutate: func(e *candidateEnv) { e.countries = []shared.Country{"FR"} },
			field:  func(c routing.Candidate) bool { return c.SupportsCountry },
		},
		{
			name:   "the integration does not implement the operation",
			mutate: func(e *candidateEnv) { e.operations = []shared.Operation{shared.OpRefund} },
			field:  func(c routing.Candidate) bool { return c.SupportsOperation },
		},
		{
			name:   "3DS is not supported for the corridor",
			mutate: func(e *candidateEnv) { e.threeDS = false },
			field:  func(c routing.Candidate) bool { return c.SupportsThreeDS },
		},
		{
			name:   "the gateway is UNHEALTHY",
			mutate: func(e *candidateEnv) { e.healthy = false },
			field:  func(c routing.Candidate) bool { return c.Healthy },
		},
	}

	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := newCandidateEnv()
			good := base.build(t)[0]
			if !tc.field(good) {
				t.Fatalf("the baseline candidate already answers false for %q; the test proves nothing", tc.name)
			}
			broken := newCandidateEnv()
			tc.mutate(broken)
			got := broken.build(t)[0]
			if tc.field(got) {
				t.Fatalf("%s: the field stayed true after its source said no — a hard filter is disabled", tc.name)
			}
		})
	}
}

// TestCandidateCostIsPerPaymentAndComesFromTheCostModel.
//
// Cost is per payment rather than per gateway because a 30¢ fixed fee dominates a €3 payment and
// is noise on a €300 one. A per-gateway figure would rank candidates correctly for exactly one
// amount.
func TestCandidateCostIsPerPaymentAndComesFromTheCostModel(t *testing.T) {
	t.Parallel()
	env := newCandidateEnv()
	small := env.buildAmount(t, money.MustNew(300, "EUR"))[0]
	large := env.buildAmount(t, money.MustNew(30000, "EUR"))[0]

	// 290 bps + 25 fixed: 300 → 9+25 = 34; 30000 → 870+25 = 895. The fixed fee is 74% of the
	// small payment's cost and 3% of the large one's, which is exactly why cost is per payment.
	if small.CostMinorUnits != 34 {
		t.Fatalf("small payment cost = %d, want 34 (9 proportional + 25 fixed)", small.CostMinorUnits)
	}
	if large.CostMinorUnits != 895 {
		t.Fatalf("large payment cost = %d, want 895", large.CostMinorUnits)
	}
}

// TestCandidateSuccessRateIsSmoothed.
//
// Without smoothing, a gateway with six samples and six successes scores a perfect 1.0 and
// outranks one with four thousand samples at 94% — which is how a freshly certified connection
// captures a merchant's entire volume on the strength of six transactions.
func TestCandidateSuccessRateIsSmoothed(t *testing.T) {
	t.Parallel()
	env := newCandidateEnv()
	env.sample = SuccessSample{Successes: 6, Samples: 6, Prior: 0.90}
	got := env.build(t)[0]

	want := routing.SmoothSuccessRate(6, 6, 0.90)
	if got.SuccessRate != want {
		t.Fatalf("success rate = %v, want the smoothed %v", got.SuccessRate, want)
	}
	if got.SuccessRate >= 1.0 {
		t.Fatalf("six samples produced a perfect success rate of %v", got.SuccessRate)
	}
}

// TestCandidateCircuitOpenIsTakenFromTheBreakerAndTheHealthRecord.
//
// The two are different facts: health is the platform's shared, persisted view; the breaker is
// this process's own, which can be open while the shared view has not caught up. Taking the more
// restrictive of the two is the only safe combination.
func TestCandidateCircuitOpenIsTakenFromTheBreakerAndTheHealthRecord(t *testing.T) {
	t.Parallel()
	env := newCandidateEnv()
	env.breakerOpen = true
	got := env.build(t)[0]
	if !got.CircuitOpen {
		t.Fatal("a locally open breaker did not mark the candidate's circuit open")
	}
	if got.Healthy {
		t.Fatal("a candidate with an open circuit was reported healthy")
	}
}

type candidateEnv struct {
	entitled    bool
	region      string
	certified   bool
	connected   bool
	healthy     bool
	threeDS     bool
	breakerOpen bool
	currencies  []money.Currency
	methods     []shared.PaymentMethod
	countries   []shared.Country
	operations  []shared.Operation
	sample      SuccessSample
}

func newCandidateEnv() *candidateEnv {
	return &candidateEnv{
		entitled: true, region: "eu", certified: true, connected: true,
		healthy: true, threeDS: true,
		currencies: []money.Currency{"EUR"},
		methods:    []shared.PaymentMethod{shared.MethodCard},
		countries:  []shared.Country{"DE"},
		operations: []shared.Operation{shared.OpAuthorize, shared.OpCapture, shared.OpRefund},
		sample:     SuccessSample{Successes: 950, Samples: 1000, Prior: 0.9},
	}
}

func (e *candidateEnv) build(t *testing.T) []routing.Candidate {
	return e.buildAmount(t, money.MustNew(8450, "EUR"))
}

func (e *candidateEnv) buildAmount(t *testing.T, amount money.Money) []routing.Candidate {
	t.Helper()
	clock := apptest.NewClock(testEpoch)

	costs, err := dgateway.NewCostModel(dgateway.CostRate{
		Currency: "EUR", Method: dgateway.AnyMethod,
		FixedFee: money.MustNew(25, "EUR"), BasisPoints: 290,
	})
	if err != nil {
		t.Fatalf("NewCostModel: %v", err)
	}
	g, err := dgateway.NewGateway(dgateway.NewGatewayParams{
		ID: gwA, DisplayName: "Gateway A", SignatureScheme: dgateway.SchemeHMACSHA256,
		BaseURLs: map[shared.Environment]string{shared.EnvironmentSandbox: "https://a.example"},
		Capabilities: dgateway.Capabilities{
			Countries: e.countries, Currencies: e.currencies, Methods: e.methods,
			Operations: e.operations, Supports3DS2: e.threeDS,
		},
		CostModel: costs,
	}, clock)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	tn, err := dtenant.New(dtenant.NewParams{
		Name: "Acme", Residency: dtenant.ResidencyEU,
		Environments:      []shared.Environment{shared.EnvironmentSandbox},
		EnabledCurrencies: []money.Currency{"EUR"},
		EnabledMethods:    []shared.PaymentMethod{shared.MethodCard},
	}, clock)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	if e.entitled {
		if err := tn.EnableGateway(gwA, clock); err != nil {
			t.Fatalf("EnableGateway: %v", err)
		}
	}

	health, err := dgateway.NewHealth(gwA, shared.OpAuthorize, clock)
	if err != nil {
		t.Fatalf("NewHealth: %v", err)
	}
	if !e.healthy {
		for i := 0; i < dgateway.MinSamples+5; i++ {
			if _, err := health.Observe(dgateway.ObservationError, time.Second, clock.Now()); err != nil {
				t.Fatalf("Observe: %v", err)
			}
		}
	}

	breaker := apptest.NewBreaker()
	if e.breakerOpen {
		breaker.Open[gwA.String()+":authorize"] = true
	}

	snap := defaultSnapshot()
	snap.Connections = map[shared.GatewayID]ConnectionSnapshot{}
	if e.connected {
		snap.Connections[gwA] = ConnectionSnapshot{
			GatewayID: gwA, ExternalAccountID: "acct", Certified: e.certified,
			Status: dgateway.StatusCertified, SecretRef: "secret://x",
		}
	}
	snap.Routing = routing.Policy{Strategy: routing.StrategyWeightedScore, Primary: gwA}

	b := NewCandidateAssembler(CandidateAssemblerDeps{
		Gateways:           stubCatalog{g: g},
		Health:             stubHealth{h: health},
		Tenants:            stubTenants{t: tn},
		Rates:              stubRates{s: e.sample},
		Breakers:           breaker,
		ProcessingRegionOf: func(shared.GatewayID) string { return e.region },
	})
	out, err := b.Build(context.Background(), routing.RequestContext{
		TenantID: tn.ID(), MerchantID: snap.MerchantID, Amount: amount,
		PaymentMethod: shared.MethodCard, PayerCountry: "DE", MerchantCountry: "DE",
		Operation: shared.OpAuthorize,
	}, snap)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("Build returned no candidates")
	}
	return out
}

type stubCatalog struct{ g *dgateway.Gateway }

func (s stubCatalog) Get(context.Context, shared.GatewayID) (*dgateway.Gateway, error) {
	return s.g, nil
}

func (s stubCatalog) List(context.Context) ([]*dgateway.Gateway, error) {
	return []*dgateway.Gateway{s.g}, nil
}

type stubHealth struct{ h *dgateway.Health }

func (s stubHealth) Get(context.Context, shared.GatewayID, shared.Operation) (*dgateway.Health, error) {
	return s.h, nil
}

type stubTenants struct{ t *dtenant.Tenant }

func (s stubTenants) Get(context.Context, shared.TenantID) (*dtenant.Tenant, error) { return s.t, nil }

type stubRates struct{ s SuccessSample }

func (s stubRates) For(context.Context, shared.GatewayID, shared.PaymentMethod, money.Currency, shared.Country) (SuccessSample, error) {
	return s.s, nil
}

// --- risk gathering -------------------------------------------------------------------------

// TestUnavailableCounterBecomesTheDomainsMarkerNotAZero.
//
// Zero means "no payments in the window", which is the safest possible reading and therefore the
// most dangerous thing an outage can silently produce: a card tester gets an unbounded window the
// moment the counter store blips. The marker makes the policy's posture decide instead.
func TestUnavailableCounterBecomesTheDomainsMarkerNotAZero(t *testing.T) {
	t.Parallel()
	vel := apptest.NewVelocity()
	keys := KeysFor(testTenant, testMerchant, dpayment.PaymentMethodReference{Token: "tok", Brand: "visa", Last4: "4242"},
		dpayment.CustomerReference{MerchantCustomerID: "cus_1"})
	vel.FailKeys[keys.cardHour()] = true

	readout := ReadVelocity(context.Background(), vel, keys, money.Currency("EUR"))
	if readout.CardHour.IsAvailable() {
		t.Fatal("a failed counter read was reported as available")
	}
	if !readout.AnyUnavailable {
		t.Fatal("the readout did not record that a counter was unavailable")
	}

	assessor := NewRiskAssessor(RiskAssessorDeps{Velocity: vel, Clock: apptest.NewClock(testEpoch)})
	policy := risk.Policy{
		MaxTransactionAmount: money.MustNew(100000, "EUR"),
		Velocity:             risk.Velocity{MaxPerCardPerHour: 5},
	}
	d, err := assessor.Evaluate(context.Background(), RiskInput{
		Policy: policy, TenantID: testTenant, MerchantID: testMerchant,
		Amount: mustEUR(1000), Method: shared.MethodCard,
		MethodRef: dpayment.PaymentMethodReference{Token: "tok", Brand: "visa", Last4: "4242"},
		Customer:  dpayment.CustomerReference{MerchantCustomerID: "cus_1", Country: "DE"},
		Merchant:  defaultSnapshot(), Now: testEpoch,
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !d.Degraded {
		t.Fatal("an unavailable velocity counter did not mark the decision degraded")
	}
	if d.Outcome == risk.OutcomeApprove && !d.Require3DS {
		t.Fatal("an unavailable velocity counter degraded to a plain approval")
	}
}

// TestBlocklistUnavailabilityDegradesToFrictionNotApproval.
func TestBlocklistUnavailabilityDegradesToFrictionNotApproval(t *testing.T) {
	t.Parallel()
	assessor := NewRiskAssessor(RiskAssessorDeps{
		Velocity:   apptest.NewVelocity(),
		Blocklists: failingBlocklist{},
		Clock:      apptest.NewClock(testEpoch),
	})
	d, err := assessor.Evaluate(context.Background(), RiskInput{
		Policy:   risk.Policy{MaxTransactionAmount: money.MustNew(100000, "EUR")},
		Amount:   mustEUR(1000),
		Method:   shared.MethodCard,
		Customer: dpayment.CustomerReference{Country: "DE"},
		Merchant: defaultSnapshot(), Now: testEpoch,
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !d.Degraded {
		t.Fatal("an unavailable blocklist did not mark the decision degraded")
	}
	if d.Outcome == risk.OutcomeApprove && !d.Require3DS {
		t.Fatal("an unavailable blocklist degraded to a plain approval")
	}
}

// TestScorerTimeoutDoesNotFailThePaymentAndDoesNotApproveItEither.
//
// The bounded deadline is the point: an unbounded scorer call turns a vendor's bad afternoon into
// the platform's. Its unavailability posture is REQUIRE_3DS, which keeps money moving and shifts
// liability, rather than APPROVE, which would waste the whole reason the scorer was invoked.
func TestScorerTimeoutDoesNotFailThePaymentAndDoesNotApproveItEither(t *testing.T) {
	t.Parallel()
	assessor := NewRiskAssessor(RiskAssessorDeps{
		Velocity:        apptest.NewVelocity(),
		Scorer:          &apptest.Scorer{Delay: time.Second},
		ScorerTimeout:   5 * time.Millisecond,
		TRAScoreCeiling: 30,
		Clock:           apptest.NewClock(testEpoch),
	})
	start := time.Now()
	d, err := assessor.Evaluate(context.Background(), RiskInput{
		Policy:   risk.Policy{MaxTransactionAmount: money.MustNew(100000, "EUR")},
		Amount:   mustEUR(1000),
		Method:   shared.MethodCard,
		Customer: dpayment.CustomerReference{Country: "DE"},
		Merchant: defaultSnapshot(), Now: testEpoch,
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("the scorer was not bounded: evaluation took %s", elapsed)
	}
	if !d.Degraded {
		t.Fatal("a scorer timeout did not mark the decision degraded")
	}
	if d.Score != 0 {
		t.Fatalf("score = %d, want 0: an unavailable scorer must not produce a number", d.Score)
	}
}

type failingBlocklist struct{}

func (failingBlocklist) Lookup(context.Context, BlocklistQuery) (BlocklistAnswer, error) {
	return BlocklistAnswer{}, errors.New("blocklist unavailable")
}

// --- validation -----------------------------------------------------------------------------

// TestL5RefusesACallerWithoutTheOperationScope, and refuses one with no principal at all: an
// unauthenticated caller must not be treated as one with no restrictions.
func TestL5RefusesACallerWithoutTheOperationScope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		principal *apptest.Principal
	}{
		{"no principal", &apptest.Principal{Absent: true}},
		{"wrong scope", &apptest.Principal{ID: "cli_1", Scopes: []string{"payments:read"}}},
	} {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := NewValidator(ValidatorDeps{
				Velocity: apptest.NewVelocity(), Principals: tc.principal,
				Clock: apptest.NewClock(testEpoch),
			})
			err := v.ValidateCreate(context.Background(), createCommand(), validatableSnapshot())
			if err == nil {
				t.Fatal("a caller without the required scope was accepted")
			}
		})
	}
}

// TestL5FailsClosedOnAMissingConfigurationSnapshot.
//
// Proceeding on an empty configuration means processing with no limits, no blocked countries and
// no enabled-currency check, which is worse than a retryable 503.
func TestL5FailsClosedOnAMissingConfigurationSnapshot(t *testing.T) {
	t.Parallel()
	v := NewValidator(ValidatorDeps{
		Velocity:   apptest.NewVelocity(),
		Principals: &apptest.Principal{ID: "cli_1", Scopes: apptest.AllScopes()},
		Clock:      apptest.NewClock(testEpoch),
	})
	snap := validatableSnapshot()
	snap.ConfigPresent = false

	err := v.ValidateCreate(context.Background(), createCommand(), snap)
	if err == nil {
		t.Fatal("a payment was validated against a merchant with no configuration snapshot")
	}
	if apierror.CodeOf(err) != apierror.CodeConfigurationStale {
		t.Fatalf("got %s, want CONFIGURATION_STALE", apierror.CodeOf(err))
	}
}

// TestL5AcceptsAWellFormedCreate is the positive control: without it, the negative tests above
// would pass against a validator that rejects everything.
func TestL5AcceptsAWellFormedCreate(t *testing.T) {
	// Verifies: FR-60.
	t.Parallel()
	v := NewValidator(ValidatorDeps{
		Velocity:   apptest.NewVelocity(),
		Principals: &apptest.Principal{ID: "cli_1", Scopes: apptest.AllScopes()},
		Clock:      apptest.NewClock(testEpoch),
	})
	if err := v.ValidateCreate(context.Background(), createCommand(), validatableSnapshot()); err != nil {
		t.Fatalf("a well-formed create was rejected: %v", err)
	}
}

// TestL6RejectsAnEchoedAmountThatDoesNotMatch.
//
// A gateway that echoes a different amount is describing a world in which money may already have
// moved differently from what we are about to record. Applying it is how a platform tells a
// customer their payment failed while the issuer tells them it succeeded.
func TestL6RejectsAnEchoedAmountThatDoesNotMatch(t *testing.T) {
	// Verifies: FR-41.
	t.Parallel()
	v := NewValidator(ValidatorDeps{Clock: apptest.NewClock(testEpoch)})
	wrong := money.MustNew(9999, "EUR")
	err := v.ValidateGatewayResponse(context.Background(), GatewayResponse{
		Status: spi.StatusAuthorized, GatewayRef: "gwref", AuthorizedAmount: &wrong,
		RawStatus: "authorized",
	}, ExpectedResponse{
		Amount: mustEUR(8450), Currency: "EUR", Operation: shared.OpAuthorize,
		GatewayID: gwA, PaymentID: "pay_x", AttemptID: "att_x",
		CurrentState: dpayment.StateProcessing,
	})
	if err == nil {
		t.Fatal("a mismatched echoed amount was accepted")
	}
}

// TestL6AcceptsAWellFormedResponse is the positive control for the level.
func TestL6AcceptsAWellFormedResponse(t *testing.T) {
	t.Parallel()
	v := NewValidator(ValidatorDeps{Clock: apptest.NewClock(testEpoch)})
	amount := mustEUR(8450)
	err := v.ValidateGatewayResponse(context.Background(), GatewayResponse{
		Status: spi.StatusAuthorized, GatewayRef: "gwref", AuthorizedAmount: &amount,
		RawStatus: "authorized",
	}, ExpectedResponse{
		Amount: amount, Currency: "EUR", Operation: shared.OpAuthorize,
		GatewayID: gwA, PaymentID: "pay_x", AttemptID: "att_x",
		CurrentState: dpayment.StateProcessing,
	})
	if err != nil {
		t.Fatalf("a well-formed authorization response was rejected: %v", err)
	}
}

// TestL6RejectsAStatusTheAdapterCouldNotMap. Inventing a meaning for an unmapped status is
// exactly the thing this level exists to refuse.
func TestL6RejectsAStatusTheAdapterCouldNotMap(t *testing.T) {
	t.Parallel()
	v := NewValidator(ValidatorDeps{Clock: apptest.NewClock(testEpoch)})
	err := v.ValidateGatewayResponse(context.Background(), GatewayResponse{
		Status: spi.Status("SOMETHING_NEW"), GatewayRef: "gwref", RawStatus: "something_new",
	}, ExpectedResponse{
		Amount: mustEUR(8450), Currency: "EUR", Operation: shared.OpAuthorize,
		GatewayID: gwA, PaymentID: "pay_x", AttemptID: "att_x",
		CurrentState: dpayment.StateProcessing,
	})
	if err == nil {
		t.Fatal("an unmappable gateway status was accepted")
	}
}

// validatableSnapshot is the default snapshot with the fields L5 actually reads populated.
func validatableSnapshot() MerchantSnapshot {
	s := defaultSnapshot()
	s.SupportedCountries = []shared.Country{"DE"}
	s.RoutableCombinations = []RouteCombination{{Method: shared.MethodCard, Currency: "EUR"}}
	return s
}

// --- credential resolution ---------------------------------------------------------------------

// TestCredentialsAreResolvedPerCallAndNeverCached.
//
// A cached credential is a credential a rotation cannot reach, a credential in a heap dump, and a
// credential that outlives the connection's revocation. The assertion is that two calls produce
// two reads of the secrets store.
func TestCredentialsAreResolvedPerCallAndNeverCached(t *testing.T) {
	// Verifies: BR-11, NFR-32.
	t.Parallel()
	secrets := &countingSecrets{inner: apptest.NewSecrets()}
	secrets.inner.Seed("secret://sandbox/t/m/gw-a", map[string]string{"apiKey": "sk_test"})

	r := NewResolver(ResolverDeps{
		Merchants:   &stubLoader{snap: defaultSnapshot()},
		Secrets:     secrets,
		Adapters:    stubAdapters{g: apptest.NewGateway(gwA, apptest.NewRecorder())},
		Environment: shared.EnvironmentSandbox,
	})
	for i := 0; i < 3; i++ {
		_, creds, ext, err := r.Resolve(context.Background(), testMerchant, gwA)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if creds.Values["apiKey"] != "sk_test" {
			t.Fatalf("credentials were not resolved: %v", creds.Values)
		}
		if ext != "acct_a" {
			t.Fatalf("external account = %q, want acct_a", ext)
		}
	}
	if secrets.reads != 3 {
		t.Fatalf("the secrets store was read %d times for 3 calls; credentials are being cached", secrets.reads)
	}
}

// TestCredentialsRedactWhenPrinted. A credential that reaches a log line has been disclosed to
// everyone who can read logs, which is a much larger set than everyone who can read the secret.
func TestCredentialsRedactWhenPrinted(t *testing.T) {
	t.Parallel()
	c := spi.Credentials{Values: map[string]string{"apiKey": "sk_test_FAKE_supersecret"}}
	if got := c.String(); got != "spi.Credentials{[REDACTED]}" {
		t.Fatalf("Credentials.String() = %q; the material is reachable through a log line", got)
	}
}

// TestResolveRefusesAConnectionThatMayNotCarryPayments.
func TestResolveRefusesAConnectionThatMayNotCarryPayments(t *testing.T) {
	// Verifies: FR-40.
	t.Parallel()
	snap := defaultSnapshot()
	snap.Connections[gwA] = ConnectionSnapshot{
		GatewayID: gwA, ExternalAccountID: "acct_a",
		Status: dgateway.StatusProvisioned, Certified: false, SecretRef: "secret://x/y",
	}
	r := NewResolver(ResolverDeps{
		Merchants:   &stubLoader{snap: snap},
		Secrets:     apptest.NewSecrets(),
		Adapters:    stubAdapters{g: apptest.NewGateway(gwA, apptest.NewRecorder())},
		Environment: shared.EnvironmentSandbox,
	})
	if _, _, _, err := r.Resolve(context.Background(), testMerchant, gwA); err == nil {
		t.Fatal("an uncertified connection was resolved for dispatch")
	}
}

// countingSecrets wraps the in-memory provider and counts resolutions. Counting is the only way
// to assert "never cached": the *result* of a cached and an uncached resolution is identical.
type countingSecrets struct {
	inner *apptest.Secrets
	reads int
}

func (c *countingSecrets) Get(ctx context.Context, ref string) (ports.SecretMaterial, error) {
	c.reads++
	return c.inner.Get(ctx, ref)
}

func (c *countingSecrets) Put(ctx context.Context, ref string, m map[string]string) (string, error) {
	return c.inner.Put(ctx, ref, m)
}

func (c *countingSecrets) Rotate(ctx context.Context, ref string, m map[string]string, overlap time.Duration) (string, error) {
	return c.inner.Rotate(ctx, ref, m, overlap)
}

func (c *countingSecrets) Delete(ctx context.Context, ref string) error {
	return c.inner.Delete(ctx, ref)
}

type stubAdapters struct{ g spi.PaymentGateway }

func (s stubAdapters) Resolve(context.Context, shared.GatewayID) (spi.PaymentGateway, error) {
	return s.g, nil
}
