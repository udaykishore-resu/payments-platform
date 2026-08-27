//go:build chaos

package chaos

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/apptest"
	apppayment "github.com/udaykishore-resu/payments-platform/internal/application/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/ledger"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	dpayment "github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/routing"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// The two gateways every scenario routes between, and the fixed instant every clock starts at.
//
// Named rather than generated, so a failure message says "the second attempt went to gw-b", which
// is the fact being asserted rather than an opaque identifier.
const (
	gwPrimary  shared.GatewayID  = "gw-a"
	gwFallback shared.GatewayID  = "gw-b"
	tenantID   shared.TenantID   = "ten_01JCHAOS0000000000000000A"
	merchantID shared.MerchantID = "mrc_01JCHAOS0000000000000000B"
)

var chaosEpoch = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// env is one scenario's world: a real payment orchestrator wired to in-memory ports, with the
// gateway adapters replaceable so a fault can be composed over them.
//
// It is a near-copy of the harness in internal/application/payment's own tests, and deliberately
// so. Those tests assert the orchestrator's *sequencing* with cooperative doubles; this package
// asserts what the same orchestrator does when a dependency misbehaves. Sharing the wiring means
// the two are demonstrably testing one system, and the only way to share it is to rebuild it here:
// the original lives in a `_test.go` file and is not importable, which is the correct visibility
// for it.
type env struct {
	t *testing.T

	Store     *apptest.Store
	Recorder  *apptest.Recorder
	Clock     *apptest.Clock
	UoW       *FaultyUnitOfWork
	Velocity  *FaultyVelocity
	Publisher *FaultyPublisher
	RiskEval  *riskThroughVelocity
	Breaker   *apptest.Breaker
	Bulkhead  *apptest.Bulkhead
	Metrics   *apptest.Metrics

	// Primary and Fallback are the underlying scripted adapters. A scenario decorates them with
	// faults through Route.
	Primary  *apptest.Gateway
	Fallback *apptest.Gateway

	resolver *faultResolver
	Service  *apppayment.Service
}

// faultResolver hands the orchestrator whatever adapter a scenario has installed for a gateway.
type faultResolver struct {
	mu       sync.RWMutex
	adapters map[shared.GatewayID]spi.PaymentGateway
}

func (r *faultResolver) Resolve(_ context.Context, _ shared.MerchantID, g shared.GatewayID) (spi.PaymentGateway, spi.Credentials, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[g]
	if !ok {
		return nil, spi.Credentials{}, "", spi.ErrNotSupported
	}
	return a, spi.Credentials{
		Values:      map[string]string{"api_key": "chaos"},
		Environment: shared.EnvironmentSandbox,
	}, "acct_" + g.String(), nil
}

func (r *faultResolver) install(g shared.GatewayID, a spi.PaymentGateway) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[g] = a
}

// permissiveValidator answers yes to every level.
//
// The validation plane has its own suite; a chaos scenario that also had to satisfy forty rules
// would fail for reasons that have nothing to do with the fault, and the reader would have to
// decide which. Making validation a constant keeps the fault the only variable.
type permissiveValidator struct{}

func (permissiveValidator) ValidateCreate(context.Context, apppayment.CreateCommand, apppayment.MerchantSnapshot) error {
	return nil
}

func (permissiveValidator) ValidateCapture(context.Context, *dpayment.Payment, money.Money, apppayment.MerchantSnapshot) error {
	return nil
}

func (permissiveValidator) ValidateRefund(context.Context, *dpayment.Payment, money.Money, apppayment.MerchantSnapshot) error {
	return nil
}

func (permissiveValidator) ValidateVoid(context.Context, *dpayment.Payment, apppayment.MerchantSnapshot) error {
	return nil
}

func (permissiveValidator) ValidateGatewayResponse(context.Context, apppayment.GatewayResponse, apppayment.ExpectedResponse) error {
	return nil
}

type fixedLoader struct{ snap apppayment.MerchantSnapshot }

func (l fixedLoader) Load(context.Context, shared.MerchantID) (apppayment.MerchantSnapshot, error) {
	return l.snap, nil
}

type fixedCandidates struct{ set []routing.Candidate }

func (c fixedCandidates) Build(context.Context, routing.RequestContext, apppayment.MerchantSnapshot) ([]routing.Candidate, error) {
	return c.set, nil
}

// riskThroughVelocity is the risk evaluator, and it deliberately touches the velocity counter.
//
// It exists so the Redis scenario has something real to break. Redis is the platform's
// non-authoritative accelerator; the claim under test (C-7, FS-6) is that losing it degrades
// latency and nothing else, and that claim is only testable if something on the payment path
// actually reads it and is written to survive the read failing.
type riskThroughVelocity struct {
	velocity *FaultyVelocity
	// degraded counts how many evaluations fell back because the accelerator was unavailable.
	degraded int64
	mu       sync.Mutex
}

func (r *riskThroughVelocity) Evaluate(ctx context.Context, in apppayment.RiskInput) (risk.Decision, error) {
	if _, err := r.velocity.IncrementAndCount(ctx, "velocity:"+in.MerchantID.String(), time.Minute); err != nil {
		// Fail *open* on the accelerator, closed on the authority. A velocity counter that cannot
		// be read is a missing signal, not a reason to refuse a legitimate payment — and the
		// database still holds every guard that actually protects money.
		r.mu.Lock()
		r.degraded++
		r.mu.Unlock()
	}
	return risk.Decision{Outcome: risk.OutcomeApprove}, nil
}

// Degraded is how many payments were assessed without the accelerator.
func (r *riskThroughVelocity) Degraded() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return int(r.degraded)
}

// newEnv wires a complete orchestrator.
func newEnv(t *testing.T) *env {
	t.Helper()
	store := apptest.NewStore()
	rec := apptest.NewRecorder()

	e := &env{
		t:         t,
		Store:     store,
		Recorder:  rec,
		Clock:     apptest.NewClock(chaosEpoch),
		Breaker:   apptest.NewBreaker(),
		Bulkhead:  apptest.NewBulkhead(),
		Metrics:   apptest.NewMetrics(),
		Publisher: NewFaultyPublisher(),
		Primary:   apptest.NewGateway(gwPrimary, rec),
		Fallback:  apptest.NewGateway(gwFallback, rec),
	}
	e.UoW = NewFaultyUnitOfWork(apptest.NewUnitOfWork(store, rec))
	e.Velocity = NewFaultyVelocity(apptest.NewVelocity())
	e.RiskEval = &riskThroughVelocity{velocity: e.Velocity}
	e.resolver = &faultResolver{adapters: map[shared.GatewayID]spi.PaymentGateway{
		gwPrimary: e.Primary, gwFallback: e.Fallback,
	}}

	deps := apppayment.Deps{
		UoW:        e.UoW,
		Config:     apptest.NewConfigProvider(),
		Merchants:  fixedLoader{snap: chaosSnapshot()},
		Gateways:   e.resolver,
		Candidates: fixedCandidates{set: eligible(gwPrimary, gwFallback)},
		Risk:       e.RiskEval,
		Breakers:   e.Breaker,
		Bulkheads:  e.Bulkhead,
		Metrics:    e.Metrics,
		Audit:      &apptest.Auditor{Store: store},
		Clock:      e.Clock,
		Settings:   apppayment.DefaultConfig(),
	}
	e.Service = apppayment.NewService(deps, permissiveValidator{})

	// Asserted at the end of every scenario, because both are properties of *every* run and a
	// leaked bulkhead slot in particular is invisible in the test that caused it and fatal in
	// production three weeks later.
	t.Cleanup(func() {
		if e.Bulkhead.InFlight != 0 {
			t.Errorf("the bulkhead leaked %d slot(s); a release did not run. Under a fault this is "+
				"the difference between one degraded gateway and a stalled orchestrator.",
				e.Bulkhead.InFlight)
		}
		if err := e.Hypothesis().check(); err != nil {
			t.Errorf("the steady-state hypothesis does not hold at the end of the scenario: %v", err)
		}
	})
	return e
}

// Route installs a faulted adapter for a gateway.
func (e *env) Route(g shared.GatewayID, adapter spi.PaymentGateway) {
	e.resolver.install(g, adapter)
}

// Create submits one payment. The idempotency key is derived from the reference so a scenario that
// resubmits says so by reusing the reference.
func (e *env) Create(ctx context.Context, reference string, minor int64) (*apppayment.Result, error) {
	return e.Service.Create(ctx, apppayment.CreateCommand{
		TenantID:      tenantID,
		MerchantID:    merchantID,
		Amount:        money.MustNew(minor, "EUR"),
		Method:        shared.MethodCard,
		MethodRef:     dpayment.PaymentMethodReference{Token: "tok_chaos", Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: chaosEpoch.Year() + 2, Country: "DE"},
		CaptureMethod: dpayment.CaptureAutomatic,
		Description:   reference,
		Customer:      dpayment.CustomerReference{MerchantCustomerID: "cus_chaos", Country: "DE"},

		IdempotencyKey: "idem-" + reference,
		CorrelationID:  "corr-" + reference,
	})
}

// Ctx returns a context carrying the tenant the in-memory store scopes on.
func (e *env) Ctx() context.Context {
	return apptest.WithTenant(context.Background(), tenantID)
}

// --- the steady-state hypothesis -----------------------------------------------------------------

// Hypothesis is the set of properties that must hold before, during and after a fault.
//
// It is deliberately not "the system recovered". Recovery is observable at the end; these are
// observable continuously, and the difference is the whole point: a payment that briefly had two
// successful attempts and then had one is a double charge that has already happened.
type Hypothesis struct {
	name       string
	invariants []invariant
}

type invariant struct {
	name  string
	check func() error
}

// Hypothesis returns the money-safety hypothesis every scenario in this package shares.
func (e *env) Hypothesis() *Hypothesis {
	return &Hypothesis{
		name: "money safety",
		invariants: []invariant{
			{"no payment reaches FAILED on an unknown outcome", e.noFailedOnUnknown},
			{"no payment has two successful authorization attempts (I3)", e.noDuplicateSuccess},
			{"captured never exceeds authorized, refunded never exceeds captured (I1, I2)", e.amountsWithinBounds},
			{"every ledger transaction balances", e.ledgerBalances},
			{"no gateway idempotency key is used by two attempts", e.oneKeyPerAttempt},
		},
	}
}

func (h *Hypothesis) check() error {
	var broken []string
	for _, inv := range h.invariants {
		if err := inv.check(); err != nil {
			broken = append(broken, inv.name+": "+err.Error())
		}
	}
	if len(broken) == 0 {
		return nil
	}
	sort.Strings(broken)
	return fmt.Errorf("%s\n  %s", h.name, strings.Join(broken, "\n  "))
}

// HoldsNow asserts the hypothesis at this instant.
func (h *Hypothesis) HoldsNow(t *testing.T, when string) {
	t.Helper()
	if err := h.check(); err != nil {
		t.Fatalf("the steady-state hypothesis does not hold %s: %v", when, err)
	}
}

// Watch checks the hypothesis after every committed transaction until the returned function is
// called, and reports the first commit at which it did not hold.
//
// # Why this samples commits rather than the wall clock
//
// docs/testing.md §6.3 specifies `RequireHeldThroughout` as a 250 ms ticker, and against a running
// system reading a database that is exactly right: every read is a consistent snapshot, and
// sampling on a timer is the only way to observe a state the system passed through.
//
// In-process it is not merely different, it is unsound. The in-memory store hands out the live
// aggregate, and a payment aggregate is single-owner by construction — the orchestrator mutates it
// with no lock because nothing else is supposed to be looking. A timer goroutine reading it
// concurrently is a data race, and -race reports it as a failure of whichever test happened to be
// running. Suppressing that by taking a lock the production code does not take would be testing a
// system that does not exist.
//
// Committing is the natural sampling point and a strictly stronger one: it is the moment a state
// becomes durable and therefore the only moment at which a violated invariant could ever be
// observed by anything else. Every committed state is checked, so a violation that appeared and
// self-corrected across two transactions is still caught — which is the whole property a
// continuously-sampled hypothesis exists to provide.
//
// It is sound only while one goroutine is driving the orchestrator. Concurrent scenarios assert
// the hypothesis before and after instead, and say so at the call site.
func (e *env) Watch(t *testing.T, h *Hypothesis) (stop func()) {
	t.Helper()
	var (
		mu       sync.Mutex
		firstErr error
		commits  int
	)
	e.UoW.OnCommit(func() {
		mu.Lock()
		defer mu.Unlock()
		commits++
		if firstErr == nil {
			firstErr = h.check()
		}
	})
	return func() {
		e.UoW.OnCommit(nil)
		mu.Lock()
		defer mu.Unlock()
		if commits == 0 {
			t.Fatalf("the hypothesis was watched across zero committed transactions; the scenario " +
				"did not reach the money path and its assertions are about nothing")
		}
		if firstErr != nil {
			t.Fatalf("the steady-state hypothesis broke at a commit *during* the fault window and "+
				"may have self-corrected before the end: %v", firstErr)
		}
	}
}

func (e *env) noFailedOnUnknown() error {
	for _, p := range e.Store.AllPayments() {
		if p.State() != dpayment.StateFailed {
			continue
		}
		for _, a := range p.Attempts() {
			if a.Outcome() == dpayment.OutcomeTimeoutUnknown {
				return fmt.Errorf("payment %s is FAILED but attempt %s is TIMEOUT_UNKNOWN; an "+
					"unknown outcome was converted into a failure", p.ID(), a.ID())
			}
		}
	}
	return nil
}

func (e *env) noDuplicateSuccess() error {
	for _, p := range e.Store.AllPayments() {
		n := 0
		for _, a := range p.Attempts() {
			if a.Outcome() == dpayment.OutcomeSuccess && a.Operation() == shared.OpAuthorize {
				n++
			}
		}
		if n > 1 {
			return fmt.Errorf("payment %s has %d successful authorization attempts", p.ID(), n)
		}
	}
	return nil
}

func (e *env) amountsWithinBounds() error {
	for _, p := range e.Store.AllPayments() {
		captured := p.CapturedAmount()
		refunded := p.RefundedAmount()
		if refunded.Amount() > captured.Amount() {
			return fmt.Errorf("payment %s refunded %d of a captured %d (I1)",
				p.ID(), refunded.Amount(), captured.Amount())
		}
		if authorized := p.AuthorizedAmount(); authorized.Amount() > 0 &&
			captured.Amount() > authorized.Amount() {
			return fmt.Errorf("payment %s captured %d of an authorized %d (I2)",
				p.ID(), captured.Amount(), authorized.Amount())
		}
	}
	return nil
}

func (e *env) ledgerBalances() error {
	for _, tx := range e.Store.LedgerTransactions() {
		var sum int64
		for _, entry := range tx.Entries() {
			if entry.Side() == ledger.SideDebit {
				sum += entry.Amount().Amount()
			} else {
				sum -= entry.Amount().Amount()
			}
		}
		if sum != 0 {
			return fmt.Errorf("ledger transaction %s is out of balance by %d", tx.ID(), sum)
		}
	}
	return nil
}

// oneKeyPerAttempt is invariant I3 seen from the gateway's side.
//
// The database's partial unique index enforces "one successful attempt per payment". This enforces
// the property that makes a *transport retry* safe: the gateway idempotency key is a pure function
// of the attempt id, so a retry to the same gateway reuses it and is deduplicated there, and a
// failover creates a new attempt and therefore correctly a new key. Two attempts sharing a key
// would make a failover invisible to the gateway; one attempt using two keys would make a retry a
// second charge.
func (e *env) oneKeyPerAttempt() error {
	owner := map[string]string{}
	for _, p := range e.Store.AllPayments() {
		for _, a := range p.Attempts() {
			key := a.IdempotencyKey()
			if key == "" {
				continue
			}
			if prev, seen := owner[key]; seen && prev != a.ID().String() {
				return fmt.Errorf("gateway idempotency key %q is shared by attempts %s and %s",
					key, prev, a.ID())
			}
			owner[key] = a.ID().String()
		}
	}
	return nil
}

// --- fixtures ------------------------------------------------------------------------------------

func chaosSnapshot() apppayment.MerchantSnapshot {
	return apppayment.MerchantSnapshot{
		MerchantID:           merchantID,
		TenantID:             tenantID,
		Environment:          shared.EnvironmentSandbox,
		Country:              "DE",
		RiskRating:           string(risk.RatingStandard),
		Status:               merchant.StatusActive,
		ConfigPresent:        true,
		ConfigVersion:        7,
		SupportedCurrencies:  []money.Currency{money.Currency("EUR"), money.Currency("USD")},
		PaymentMethods:       []shared.PaymentMethod{shared.MethodCard},
		ManualCaptureAllowed: true,
		Routing: routing.Policy{
			Strategy:          routing.StrategyPriorityWithFallback,
			Primary:           gwPrimary,
			Fallbacks:         []shared.GatewayID{gwFallback},
			ConnectedGateways: []shared.GatewayID{gwPrimary, gwFallback},
		},
		Risk:               risk.Policy{MaxTransactionAmount: money.MustNew(10_000_000, "EUR"), Version: 7},
		MaxRefundWindow:    180 * 24 * time.Hour,
		MaxPartialCaptures: 4,
		Connections: map[shared.GatewayID]apppayment.ConnectionSnapshot{
			gwPrimary:  {GatewayID: gwPrimary, ExternalAccountID: "acct_a", Status: gateway.StatusCertified, Certified: true, SecretRef: "secret://sandbox/t/m/gw-a"},
			gwFallback: {GatewayID: gwFallback, ExternalAccountID: "acct_b", Status: gateway.StatusCertified, Certified: true, SecretRef: "secret://sandbox/t/m/gw-b"},
		},
	}
}

func eligible(ids ...shared.GatewayID) []routing.Candidate {
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

// authorized is a well-formed authorization response.
func authorized(amount money.Money) *spi.Result {
	return &spi.Result{
		Status: spi.StatusAuthorized, GatewayRef: "gwref_ok",
		AuthorizedAmount: &amount, RawStatus: "authorized",
	}
}

// captured is a well-formed sale response.
func captured(amount money.Money) *spi.Result {
	return &spi.Result{
		Status: spi.StatusCaptured, GatewayRef: "gwref_cap",
		CapturedAmount: &amount, RawStatus: "captured",
	}
}

// softDecline is a decline the scheme permits failing over from.
func softDecline() *spi.Result {
	return &spi.Result{
		Status: spi.StatusDeclined, GatewayRef: "gwref_dec",
		DeclineReason: dpayment.DeclineDoNotHonorSoft, RawStatus: "declined", RawCode: "do_not_honor",
	}
}

// contextWithBudget returns a context for one payment with an explicit deadline.
//
// Deadlines are how the latency scenarios express "the caller gave up", and they are set here
// rather than inside a scenario so that every one of them carries the tenant the in-memory store
// scopes on — a context built by hand and missing it fails with a tenancy error that reads
// nothing like the timeout the test was written for.
func contextWithBudget(e *env, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(e.Ctx(), d)
}

// discardLogger silences the workflow engine's structured logs.
//
// The engine logs every step transition at INFO, which is right in production and is noise in a
// test whose failure message is the thing the reader needs to see.
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
