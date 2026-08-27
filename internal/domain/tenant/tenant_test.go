package tenant

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

var testEpoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func testClock() *shared.FixedClock { return &shared.FixedClock{T: testEpoch} }

func testQuotas() Quotas {
	return Quotas{
		MaxMerchants:       100,
		RequestsPerSecond:  500,
		ConcurrentPayments: 50,
		CacheMemoryMB:      256,
		MaxPaymentAmount:   money.MustNew(1_000_000_00, "USD"),
	}
}

func testParams() NewParams {
	return NewParams{
		Name:              "Acme Commerce",
		Tier:              shared.TierPooled,
		Residency:         ResidencyEU,
		Environments:      []shared.Environment{shared.EnvironmentSandbox, shared.EnvironmentProduction},
		Quotas:            testQuotas(),
		EnabledGateways:   []shared.GatewayID{"stripe", "adyen"},
		EnabledCurrencies: []money.Currency{"USD", "EUR"},
		EnabledMethods:    []shared.PaymentMethod{shared.MethodCard, shared.MethodSEPADebit},
		FeatureFlags:      map[string]bool{"networkTokens": true},
	}
}

func newTestTenant(t *testing.T) (*Tenant, *shared.FixedClock) {
	t.Helper()
	clk := testClock()
	tn, err := New(testParams(), clk)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tn.DrainEvents()
	return tn, clk
}

// declaredTenantEdges restates the tenant transition table independently of the implementation.
var declaredTenantEdges = map[Status]map[Status]bool{
	StatusActive: {
		StatusSuspended:  true,
		StatusTerminated: true,
	},
	StatusSuspended: {
		StatusActive:     true,
		StatusTerminated: true,
	},
	StatusTerminated: {},
}

func TestTenantMachineAcceptsExactlyTheDeclaredEdges(t *testing.T) {
	t.Parallel()

	m := Machine()
	for _, from := range m.States() {
		for _, to := range m.States() {
			want := declaredTenantEdges[from][to]
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
	if !m.IsTerminal(StatusTerminated) {
		t.Fatal("TERMINATED must be terminal")
	}
}

// declaredClientEdges restates the API client transition table.
var declaredClientEdges = map[ClientStatus]map[ClientStatus]bool{
	ClientActive: {
		ClientDisabled: true,
		ClientRevoked:  true,
	},
	ClientDisabled: {
		ClientActive:  true,
		ClientRevoked: true,
	},
	ClientRevoked: {},
}

func TestClientMachineAcceptsExactlyTheDeclaredEdges(t *testing.T) {
	t.Parallel()

	m := ClientMachine()
	for _, from := range m.States() {
		for _, to := range m.States() {
			want := declaredClientEdges[from][to]
			if got := m.CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
	// Revocation is only useful as an incident-response action if it is irreversible.
	if !m.IsTerminal(ClientRevoked) {
		t.Fatal("REVOKED must be terminal")
	}
}

func TestNewTenantValidation(t *testing.T) {
	// Verifies: BR-01, FR-01, FR-02.
	// Verifies: BR-01, FR-01.
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(*NewParams)
		wantCode apierror.Code
		check    func(*testing.T, *Tenant)
	}{
		{name: "valid", mutate: func(*NewParams) {}},
		{
			name:     "no name",
			mutate:   func(p *NewParams) { p.Name = "   " },
			wantCode: apierror.CodeValidationFailed,
		},
		{
			name:     "unknown tier",
			mutate:   func(p *NewParams) { p.Tier = "PLATINUM" },
			wantCode: apierror.CodeValidationFailed,
		},
		{
			name:     "unknown residency",
			mutate:   func(p *NewParams) { p.Residency = "MARS" },
			wantCode: apierror.CodeValidationFailed,
		},
		{
			// The siloed tier is *defined* by a dedicated key. Without one the isolation claim on
			// the invoice is simply false, and it stays false until an auditor asks.
			name:     "siloed tenant without a dedicated key",
			mutate:   func(p *NewParams) { p.Tier = shared.TierSiloed; p.KMSKeyRef = "" },
			wantCode: apierror.CodeConfigurationInvalid,
		},
		{
			name:   "siloed tenant with a key",
			mutate: func(p *NewParams) { p.Tier = shared.TierSiloed; p.KMSKeyRef = "arn:aws:kms:eu-west-1:1:key/abc" },
			check: func(t *testing.T, tn *Tenant) {
				if tn.Tier() != shared.TierSiloed {
					t.Fatalf("tier = %s", tn.Tier())
				}
			},
		},
		{
			name:     "unsupported currency",
			mutate:   func(p *NewParams) { p.EnabledCurrencies = []money.Currency{"XYZ"} },
			wantCode: apierror.CodeCurrencyNotSupported,
		},
		{
			name:     "unsupported method",
			mutate:   func(p *NewParams) { p.EnabledMethods = []shared.PaymentMethod{"CRYPTO"} },
			wantCode: apierror.CodePaymentMethodNotSupported,
		},
		{
			name:     "unknown environment",
			mutate:   func(p *NewParams) { p.Environments = []shared.Environment{"staging"} },
			wantCode: apierror.CodeValidationFailed,
		},
		{
			name:     "negative quota",
			mutate:   func(p *NewParams) { p.Quotas.MaxMerchants = -1 },
			wantCode: apierror.CodeConfigurationInvalid,
		},
		{
			// An unstated environment set is an unread contract, and the safe reading of an unread
			// contract is "not live yet".
			name:   "no environments defaults to sandbox only",
			mutate: func(p *NewParams) { p.Environments = nil },
			check: func(t *testing.T, tn *Tenant) {
				if !tn.PermitsEnvironment(shared.EnvironmentSandbox) {
					t.Fatal("sandbox not permitted")
				}
				if tn.PermitsEnvironment(shared.EnvironmentProduction) {
					t.Fatal("production permitted for a tenant with no declared environments")
				}
			},
		},
		{
			name:   "tier defaults to pooled",
			mutate: func(p *NewParams) { p.Tier = "" },
			check: func(t *testing.T, tn *Tenant) {
				if tn.Tier() != shared.TierPooled {
					t.Fatalf("tier = %s, want POOLED", tn.Tier())
				}
			},
		},
		{
			name:   "residency defaults to global",
			mutate: func(p *NewParams) { p.Residency = "" },
			check: func(t *testing.T, tn *Tenant) {
				if tn.Residency() != ResidencyGlobal {
					t.Fatalf("residency = %s, want GLOBAL", tn.Residency())
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := testParams()
			tc.mutate(&p)
			tn, err := New(p, testClock())
			if tc.wantCode != "" {
				if apierror.CodeOf(err) != tc.wantCode {
					t.Fatalf("code = %s, want %s (%v)", apierror.CodeOf(err), tc.wantCode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if tn.Status() != StatusActive || tn.Version() != 1 {
				t.Fatalf("status = %s version = %d", tn.Status(), tn.Version())
			}
			evts := tn.PendingEvents()
			if len(evts) != 1 || evts[0].Type != EventTenantCreated {
				t.Fatalf("events = %+v", evts)
			}
			if evts[0].AggregateID() != tn.ID().String() {
				t.Fatalf("partition key = %q", evts[0].AggregateID())
			}
			if tc.check != nil {
				tc.check(t, tn)
			}
		})
	}
}

func TestTenantLifecycle(t *testing.T) {
	t.Parallel()

	tn, clk := newTestTenant(t)
	if !tn.IsOperational() {
		t.Fatal("a new tenant is not operational")
	}

	if err := tn.Suspend("", clk); apierror.CodeOf(err) != apierror.CodeValidationFailed {
		t.Fatalf("suspend without a reason: code = %s", apierror.CodeOf(err))
	}
	if err := tn.Suspend("non-payment of invoice 4471", clk); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if tn.IsOperational() || tn.SuspendedAt() == nil || tn.StatusReason() == "" {
		t.Fatalf("suspension not recorded: %s %v %q", tn.Status(), tn.SuspendedAt(), tn.StatusReason())
	}
	evts := tn.DrainEvents()
	if len(evts) != 1 || evts[0].Type != EventTenantSuspended {
		t.Fatalf("events = %+v", evts)
	}
	if !evts[0].Type.IsUrgentInvalidation() || !evts[0].RequiresOperatorAttention() {
		t.Fatal("suspension must be an urgent invalidation and raise an operational signal")
	}
	if evts[0].Status != StatusSuspended {
		t.Fatalf("event carries status %s", evts[0].Status)
	}

	// A suspended tenant's entitlements and quotas are frozen: granting capacity during a
	// compliance hold is exactly the audit finding this refusal prevents.
	if err := tn.UpdateQuotas(testQuotas(), clk); err == nil {
		t.Fatal("quotas were updated on a suspended tenant")
	}
	if err := tn.EnableGateway("checkout", clk); err == nil {
		t.Fatal("a gateway was enabled on a suspended tenant")
	}
	if err := tn.Permits("stripe", "USD", shared.MethodCard); apierror.CodeOf(err) != apierror.CodeForbidden {
		t.Fatalf("Permits on a suspended tenant: code = %s, want FORBIDDEN", apierror.CodeOf(err))
	}

	if err := tn.Reinstate(clk); err != nil {
		t.Fatalf("Reinstate: %v", err)
	}
	if !tn.IsOperational() || tn.SuspendedAt() != nil || tn.StatusReason() != "" {
		t.Fatalf("reinstatement did not clear the suspension: %v %q", tn.SuspendedAt(), tn.StatusReason())
	}
	tn.DrainEvents()

	if err := tn.Terminate("contract ended", 3, clk); apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
		t.Fatalf("terminate with live merchants: code = %s", apierror.CodeOf(err))
	}
	if tn.Status() != StatusActive {
		t.Fatalf("a refused Terminate changed the status to %s", tn.Status())
	}
	if err := tn.Terminate("contract ended", 0, clk); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if tn.Status() != StatusTerminated || tn.TerminatedAt() == nil {
		t.Fatalf("termination not recorded: %s %v", tn.Status(), tn.TerminatedAt())
	}
	if err := tn.Reinstate(clk); err == nil {
		t.Fatal("a terminated tenant was reinstated")
	}
}

func TestSuspendedTenantCanBeTerminatedDirectly(t *testing.T) {
	t.Parallel()

	// The ordinary path is suspend for non-payment, wait out the cure period, terminate. Forcing
	// a reinstatement first would briefly re-enable processing for a tenant being shut down.
	tn, clk := newTestTenant(t)
	if err := tn.Suspend("non-payment", clk); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := tn.Terminate("cure period elapsed", 0, clk); err != nil {
		t.Fatalf("Terminate from SUSPENDED: %v", err)
	}
}

func TestPermitsIsTheTenantCeiling(t *testing.T) {
	// Verifies: BR-01.
	t.Parallel()

	tn, _ := newTestTenant(t)

	tests := []struct {
		name      string
		gateway   shared.GatewayID
		currency  money.Currency
		method    shared.PaymentMethod
		wantCode  apierror.Code
		wantField string
		wantRule  string
	}{
		{name: "fully entitled", gateway: "stripe", currency: "USD", method: shared.MethodCard},
		{
			name: "gateway not entitled", gateway: "checkout", currency: "USD", method: shared.MethodCard,
			wantCode: apierror.CodeGatewayNotConfigured, wantField: "routing.gateway",
			wantRule: "L4.MERCHANT_WITHIN_TENANT_GATEWAYS",
		},
		{
			name: "currency not entitled", gateway: "stripe", currency: "JPY", method: shared.MethodCard,
			wantCode: apierror.CodeCurrencyNotSupported, wantField: "supportedCurrencies",
			wantRule: "L4.MERCHANT_WITHIN_TENANT_CURRENCIES",
		},
		{
			name: "method not entitled", gateway: "stripe", currency: "USD", method: shared.MethodUPI,
			wantCode: apierror.CodePaymentMethodNotSupported, wantField: "paymentMethods",
			wantRule: "L4.MERCHANT_WITHIN_TENANT_METHODS",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tn.Permits(tc.gateway, tc.currency, tc.method)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("expected permitted, got %v", err)
				}
				return
			}
			if apierror.CodeOf(err) != tc.wantCode {
				t.Fatalf("code = %s, want %s", apierror.CodeOf(err), tc.wantCode)
			}
			var ae *apierror.Error
			if !errors.As(err, &ae) || len(ae.Details) != 1 {
				t.Fatalf("expected exactly one detail: %v", err)
			}
			if ae.Details[0].Field != tc.wantField || ae.Details[0].RuleID != tc.wantRule {
				t.Fatalf("detail = %+v, want field %s rule %s", ae.Details[0], tc.wantField, tc.wantRule)
			}
		})
	}
}

func TestEmptyEntitlementSetsPermitNothing(t *testing.T) {
	t.Parallel()

	// A tenant whose entitlement lists nobody has populated is a tenant whose contract has not
	// been transcribed. Reading that as "everything permitted" is a licensing and billing
	// exposure that only surfaces months later on an acquirer's report.
	p := testParams()
	p.EnabledGateways = nil
	p.EnabledCurrencies = nil
	p.EnabledMethods = nil
	tn, err := New(p, testClock())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := tn.Permits("stripe", "USD", shared.MethodCard); err == nil {
		t.Fatal("an empty entitlement set permitted a gateway")
	}
}

func TestEnableAndDisableGateway(t *testing.T) {
	t.Parallel()

	tn, clk := newTestTenant(t)

	if err := tn.EnableGateway("Not A Slug", clk); err == nil {
		t.Fatal("an invalid gateway slug was accepted")
	}

	before := tn.Version()
	// Idempotent: the caller is usually a declarative provisioning job reconciling a contract,
	// and forcing it to read-then-write introduces a race for no benefit.
	if err := tn.EnableGateway("stripe", clk); err != nil {
		t.Fatalf("EnableGateway (already enabled): %v", err)
	}
	if tn.Version() != before {
		t.Fatal("a no-op enable bumped the version")
	}

	if err := tn.EnableGateway("checkout", clk); err != nil {
		t.Fatalf("EnableGateway: %v", err)
	}
	if err := tn.Permits("checkout", "USD", shared.MethodCard); err != nil {
		t.Fatalf("newly enabled gateway is not permitted: %v", err)
	}
	evts := tn.DrainEvents()
	if len(evts) != 1 || evts[0].Type != EventTenantGatewayEnabled {
		t.Fatalf("events = %+v", evts)
	}

	if err := tn.DisableGateway("checkout", clk); err != nil {
		t.Fatalf("DisableGateway: %v", err)
	}
	if err := tn.Permits("checkout", "USD", shared.MethodCard); err == nil {
		t.Fatal("a disabled gateway is still permitted")
	}
	// The remaining entitlements survive the removal intact.
	if err := tn.Permits("stripe", "USD", shared.MethodCard); err != nil {
		t.Fatalf("disabling one gateway broke another: %v", err)
	}
	if err := tn.Permits("adyen", "EUR", shared.MethodSEPADebit); err != nil {
		t.Fatalf("disabling one gateway broke another: %v", err)
	}

	before = tn.Version()
	if err := tn.DisableGateway("never-enabled", clk); err != nil {
		t.Fatalf("DisableGateway (absent): %v", err)
	}
	if tn.Version() != before {
		t.Fatal("a no-op disable bumped the version")
	}
}

func TestQuotasCheck(t *testing.T) {
	t.Parallel()

	q := testQuotas()

	tests := []struct {
		name      string
		resource  Resource
		requested int64
		wantCode  apierror.Code
		retryable bool
	}{
		{name: "within the merchant quota", resource: ResourceMerchants, requested: 99},
		{name: "exactly at the merchant quota", resource: ResourceMerchants, requested: 100},
		{
			name: "over the merchant quota", resource: ResourceMerchants, requested: 101,
			wantCode: apierror.CodeAmountExceedsLimit,
		},
		{
			// Retryable: a client that has hit its rate limit should back off, and the SDK
			// branches on that bit.
			name: "over the rate limit", resource: ResourceRequestsPerSecond, requested: 501,
			wantCode: apierror.CodeRateLimited, retryable: true,
		},
		{
			name: "over the concurrency limit", resource: ResourceConcurrentPayments, requested: 51,
			wantCode: apierror.CodeConcurrencyLimitExceeded, retryable: true,
		},
		{
			name: "over the cache quota", resource: ResourceCacheMemoryMB, requested: 257,
			wantCode: apierror.CodeAmountExceedsLimit,
		},
		{
			name: "unknown resource", resource: "DISK", requested: 1,
			wantCode: apierror.CodeInternalError,
		},
		{
			name: "negative request", resource: ResourceMerchants, requested: -1,
			wantCode: apierror.CodeValidationFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := q.Check(tc.resource, tc.requested)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("expected within quota, got %v", err)
				}
				return
			}
			if apierror.CodeOf(err) != tc.wantCode {
				t.Fatalf("code = %s, want %s (%v)", apierror.CodeOf(err), tc.wantCode, err)
			}
			if apierror.IsRetryable(err) != tc.retryable {
				t.Fatalf("retryable = %v, want %v", apierror.IsRetryable(err), tc.retryable)
			}
		})
	}
}

func TestZeroQuotaMeansUnlimited(t *testing.T) {
	t.Parallel()

	// Quotas protect the platform from a tenant, not a tenant from itself. A missing quota row
	// that silently throttled a paying tenant to zero is a worse outcome than the one it prevents.
	var q Quotas
	for _, r := range []Resource{
		ResourceMerchants, ResourceRequestsPerSecond, ResourceConcurrentPayments, ResourceCacheMemoryMB,
	} {
		if err := q.Check(r, 1_000_000); err != nil {
			t.Fatalf("unset %s quota rejected a request: %v", r, err)
		}
		if _, set := q.Limit(r); set {
			t.Fatalf("unset %s quota reports a limit", r)
		}
	}
}

func TestQuotasCheckAmount(t *testing.T) {
	t.Parallel()

	q := testQuotas()

	tests := []struct {
		name     string
		amount   money.Money
		wantCode apierror.Code
	}{
		{name: "within the ceiling", amount: money.MustNew(500_000_00, "USD")},
		{name: "exactly at the ceiling", amount: money.MustNew(1_000_000_00, "USD")},
		{
			name: "above the ceiling", amount: money.MustNew(1_000_000_01, "USD"),
			wantCode: apierror.CodeAmountExceedsLimit,
		},
		{
			// The ceiling is denominated in one currency and there is no exchange rate here, on
			// purpose. Silently converting at some hardcoded rate would produce a limit that is
			// wrong in a way nobody can see; the per-currency merchant limits do the real work.
			name: "a different currency is not compared", amount: money.MustNew(999_999_999, "EUR"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := q.CheckAmount(tc.amount)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("expected within the ceiling, got %v", err)
				}
				return
			}
			if apierror.CodeOf(err) != tc.wantCode {
				t.Fatalf("code = %s, want %s", apierror.CodeOf(err), tc.wantCode)
			}
		})
	}

	// No ceiling configured means no tenant-level ceiling.
	var none Quotas
	if err := none.CheckAmount(money.MustNew(999_999_999_99, "USD")); err != nil {
		t.Fatalf("unset ceiling rejected an amount: %v", err)
	}
}

func TestUpdateQuotas(t *testing.T) {
	t.Parallel()

	tn, clk := newTestTenant(t)
	before := tn.Version()

	bad := testQuotas()
	bad.RequestsPerSecond = -1
	if err := tn.UpdateQuotas(bad, clk); err == nil {
		t.Fatal("a negative quota was accepted")
	}
	if tn.Version() != before {
		t.Fatal("a refused update bumped the version")
	}

	raised := testQuotas()
	raised.RequestsPerSecond = 2000
	if err := tn.UpdateQuotas(raised, clk); err != nil {
		t.Fatalf("UpdateQuotas: %v", err)
	}
	if tn.Quotas().RequestsPerSecond != 2000 {
		t.Fatalf("rate limit = %d", tn.Quotas().RequestsPerSecond)
	}
	evts := tn.DrainEvents()
	if len(evts) != 1 || evts[0].Type != EventTenantQuotasUpdated {
		t.Fatalf("events = %+v", evts)
	}
	// The rate limiter holds the limits in memory; without the previous value in the payload it
	// cannot tell a raise from a lowering, which changes whether it drains its buckets.
	if evts[0].Payload["previousRequestsPerSecond"] != 500 {
		t.Fatalf("payload = %+v", evts[0].Payload)
	}
}

func TestResidencyPermitsGatewayRegion(t *testing.T) {
	// Verifies: BR-36, FR-07, NFR-37.
	t.Parallel()

	tests := []struct {
		residency ResidencyRegion
		region    string
		want      bool
	}{
		{ResidencyGlobal, "us-east", true},
		{ResidencyGlobal, "anything-at-all", true},
		{ResidencyGlobal, "", true},
		{ResidencyEU, "eu", true},
		{ResidencyEU, "EU", true},
		{ResidencyEU, "  eu-central  ", true},
		{ResidencyEU, "us-east", false},
		{ResidencyEU, "uk", false},
		// UK adequacy is a distinct legal instrument: an EU-processing gateway does not satisfy a
		// UK residency commitment.
		{ResidencyUK, "eu", false},
		{ResidencyUK, "uk", true},
		{ResidencyUS, "us-west", true},
		{ResidencyUS, "eu", false},
		{ResidencyAPAC, "ap-southeast", true},
		{ResidencyAPAC, "us", false},
		// Fail closed. A gateway whose processing region nobody recorded is a gateway whose
		// processing region we do not know, and "we do not know" does not permit routing EU
		// personal data through it.
		{ResidencyEU, "", false},
		{ResidencyEU, "atlantis", false},
		{"MARS", "eu", false},
	}

	for _, tc := range tests {
		t.Run(string(tc.residency)+"/"+tc.region, func(t *testing.T) {
			t.Parallel()
			if got := tc.residency.PermitsGatewayRegion(tc.region); got != tc.want {
				t.Fatalf("%s.PermitsGatewayRegion(%q) = %v, want %v",
					tc.residency, tc.region, got, tc.want)
			}
		})
	}
}

func TestTenantAccessorsReturnCopies(t *testing.T) {
	t.Parallel()

	tn, _ := newTestTenant(t)

	gws := tn.EnabledGateways()
	gws[0] = "evil"
	if tn.EnabledGateways()[0] == "evil" {
		t.Fatal("mutating the returned gateway slice changed the aggregate")
	}

	flags := tn.FeatureFlags()
	flags["networkTokens"] = false
	flags["backdoor"] = true
	if !tn.FeatureEnabled("networkTokens") || tn.FeatureEnabled("backdoor") {
		t.Fatal("mutating the returned flag map changed the aggregate")
	}

	envs := tn.Environments()
	envs[0] = "staging"
	if !tn.PermitsEnvironment(shared.EnvironmentSandbox) {
		t.Fatal("mutating the returned environment slice changed the aggregate")
	}
}

func TestRehydrateTenantRejectsUnknownEnums(t *testing.T) {
	t.Parallel()

	base := RehydrateParams{
		ID: shared.NewTenantID(), Name: "Acme", Tier: shared.TierPooled,
		Status: StatusSuspended, Residency: ResidencyEU, Version: 9,
	}
	tn, err := Rehydrate(base)
	if err != nil {
		t.Fatalf("Rehydrate: %v", err)
	}
	if tn.Status() != StatusSuspended || tn.Version() != 9 {
		t.Fatalf("status = %s version = %d", tn.Status(), tn.Version())
	}

	bad := base
	bad.Status = "HIBERNATING"
	if _, err := Rehydrate(bad); apierror.CodeOf(err) != apierror.CodeInternalError {
		t.Fatalf("unknown status: code = %s, want INTERNAL_ERROR", apierror.CodeOf(err))
	}

	bad = base
	bad.Tier = "PLATINUM"
	if _, err := Rehydrate(bad); apierror.CodeOf(err) != apierror.CodeInternalError {
		t.Fatalf("unknown tier: code = %s, want INTERNAL_ERROR", apierror.CodeOf(err))
	}
}

// --- API client -------------------------------------------------------------------------------

const (
	testClientRef = "secret://production/ten_1/api-clients/cli_1"
	testNewRef    = "secret://production/ten_1/api-clients/cli_1#v2"
)

func newTestClient(t *testing.T) (*APIClient, *shared.FixedClock) {
	t.Helper()
	clk := testClock()
	c, err := NewAPIClient(NewAPIClientParams{
		TenantID:      shared.NewTenantID(),
		Name:          "checkout-service",
		Scopes:        []string{"payments:*", "refunds:read"},
		AllowedCIDRs:  []string{"203.0.113.0/24", "2001:db8::/32"},
		CredentialRef: testClientRef,
	}, clk)
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	c.DrainEvents()
	return c, clk
}

func TestNewAPIClientValidation(t *testing.T) {
	// Verifies: BR-02, FR-03.
	t.Parallel()

	valid := NewAPIClientParams{
		TenantID: shared.NewTenantID(), Name: "svc", Scopes: []string{"payments:write"},
		CredentialRef: testClientRef,
	}

	tests := []struct {
		name     string
		mutate   func(*NewAPIClientParams)
		wantCode apierror.Code
	}{
		{name: "valid", mutate: func(*NewAPIClientParams) {}},
		{name: "no tenant", mutate: func(p *NewAPIClientParams) { p.TenantID = "" }, wantCode: apierror.CodeMissingTenantContext},
		{name: "no name", mutate: func(p *NewAPIClientParams) { p.Name = " " }, wantCode: apierror.CodeValidationFailed},
		{
			// A credential in circulation whose blast radius nobody decided gets resolved, in
			// practice, by somebody granting it everything at three in the morning.
			name: "no scopes", mutate: func(p *NewAPIClientParams) { p.Scopes = nil },
			wantCode: apierror.CodeValidationFailed,
		},
		{
			name:     "credential material instead of a reference",
			mutate:   func(p *NewAPIClientParams) { p.CredentialRef = "sk_test_FAKE_NOT_A_REAL_KEY_51H8xVf" },
			wantCode: apierror.CodeValidationFailed,
		},
		{
			name:     "malformed cidr",
			mutate:   func(p *NewAPIClientParams) { p.AllowedCIDRs = []string{"203.0.113.0"} },
			wantCode: apierror.CodeValidationFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := valid
			tc.mutate(&p)
			c, err := NewAPIClient(p, testClock())
			if tc.wantCode != "" {
				if apierror.CodeOf(err) != tc.wantCode {
					t.Fatalf("code = %s, want %s (%v)", apierror.CodeOf(err), tc.wantCode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if c.Status() != ClientActive {
				t.Fatalf("status = %s", c.Status())
			}
			evts := c.PendingEvents()
			if len(evts) != 1 || evts[0].Type != EventTenantAPIClientCreated {
				t.Fatalf("events = %+v", evts)
			}
			// Keyed by the tenant, never by the client: a client event on a different partition
			// from the tenant suspension that ought to precede it would be applied out of order.
			if evts[0].AggregateID() != c.TenantID().String() {
				t.Fatalf("partition key = %q, want the tenant id", evts[0].AggregateID())
			}
		})
	}
}

func TestHasScope(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t)

	tests := []struct {
		scope string
		want  bool
	}{
		{"payments:write", true}, // via the payments:* grant
		{"payments:read", true},
		{"PAYMENTS:WRITE", true}, // scopes are compared case-insensitively
		{"refunds:read", true},   // exact grant
		{"refunds:write", false},
		{"payments:*", true}, // literal match on the grant itself
		// The wildcard expands in the grant, never in the requirement: a middleware asking
		// "does this client have any refund scope" must not be told yes for a read-only client.
		{"refunds:*", false},
		{"payments:", false}, // the wildcard must cover at least one character
		{"payment", false},
		{"", false},
	}

	for _, tc := range tests {
		t.Run(tc.scope, func(t *testing.T) {
			t.Parallel()
			if got := c.HasScope(tc.scope); got != tc.want {
				t.Fatalf("HasScope(%q) = %v, want %v", tc.scope, got, tc.want)
			}
		})
	}
}

func TestScopeGrantAndRevoke(t *testing.T) {
	t.Parallel()

	c, clk := newTestClient(t)
	if err := c.GrantScopes([]string{"webhooks:read", "PAYMENTS:*"}, clk); err != nil {
		t.Fatalf("GrantScopes: %v", err)
	}
	if !c.HasScope("webhooks:read") {
		t.Fatal("granted scope not present")
	}
	if len(c.Scopes()) != 3 {
		t.Fatalf("scopes = %v; the duplicate grant was not de-duplicated", c.Scopes())
	}

	if err := c.RevokeScopes([]string{"webhooks:read"}, clk); err != nil {
		t.Fatalf("RevokeScopes: %v", err)
	}
	if c.HasScope("webhooks:read") {
		t.Fatal("revoked scope still present")
	}

	// A method that could empty the scope set by subtraction would make the constructor's
	// guarantee worthless.
	err := c.RevokeScopes(c.Scopes(), clk)
	if apierror.CodeOf(err) != apierror.CodeValidationFailed {
		t.Fatalf("revoking every scope: code = %s, want VALIDATION_FAILED", apierror.CodeOf(err))
	}
	if len(c.Scopes()) != 2 {
		t.Fatalf("a refused revocation changed the scope set: %v", c.Scopes())
	}
}

func TestAllowsIP(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t)

	tests := []struct {
		addr string
		want bool
	}{
		{"203.0.113.4", true},
		{"203.0.113.255", true},
		{"203.0.114.1", false},
		{"2001:db8::1", true},
		{"2001:dc8::1", false},
		// A dual-stack listener reports an IPv4 caller as an IPv4-mapped IPv6 address. Without
		// unmapping, the restriction silently blocks every caller behind such a load balancer.
		{"::ffff:203.0.113.4", true},
	}

	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			t.Parallel()
			addr, err := netip.ParseAddr(tc.addr)
			if err != nil {
				t.Fatalf("ParseAddr: %v", err)
			}
			if got := c.AllowsIP(addr); got != tc.want {
				t.Fatalf("AllowsIP(%s) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}

	// No restriction configured means no restriction: most integrations run from dynamic
	// infrastructure and cannot name their egress addresses.
	open, err := NewAPIClient(NewAPIClientParams{
		TenantID: shared.NewTenantID(), Name: "svc", Scopes: []string{"payments:read"},
		CredentialRef: testClientRef,
	}, testClock())
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	if !open.AllowsIP(netip.MustParseAddr("198.51.100.1")) {
		t.Fatal("an unrestricted client refused an address")
	}
	// An unknown peer address is only fatal where a restriction actually exists: a client with no
	// restriction configured must not be blocked because the edge could not determine the source,
	// or every caller behind a proxy that does not forward it stops working.
	if !open.AllowsIP(netip.Addr{}) {
		t.Fatal("an unrestricted client was blocked by an undeterminable address")
	}
	if c.AllowsIP(netip.Addr{}) {
		t.Fatal("a restricted client accepted an undeterminable address")
	}
}

func TestRotationOverlap(t *testing.T) {
	// Verifies: BR-12, FR-08, NFR-31.
	t.Parallel()

	c, clk := newTestClient(t)
	overlap := testEpoch.Add(72 * time.Hour)

	if err := c.Rotate("not-a-ref", overlap, clk); err == nil {
		t.Fatal("rotation accepted credential material")
	}
	if err := c.Rotate(testClientRef, overlap, clk); err == nil {
		t.Fatal("rotation to the identical reference was accepted")
	}
	// An already-expired overlap makes the rotation an immediate cutover under a name that
	// promises otherwise, which is the worst of both worlds for the caller.
	if err := c.Rotate(testNewRef, testEpoch.Add(-time.Hour), clk); err == nil {
		t.Fatal("rotation accepted an overlap deadline in the past")
	}
	if err := c.Rotate(testNewRef, testEpoch, clk); err == nil {
		t.Fatal("rotation accepted an overlap deadline of exactly now")
	}
	if c.CredentialRef() != testClientRef {
		t.Fatalf("a refused rotation changed the credential to %q", c.CredentialRef())
	}

	if err := c.Rotate(testNewRef, overlap, clk); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if c.CredentialRef() != testNewRef || c.PreviousCredentialRef() != testClientRef {
		t.Fatalf("references = %q / %q", c.CredentialRef(), c.PreviousCredentialRef())
	}
	if c.LastRotatedAt() == nil {
		t.Fatal("lastRotatedAt not stamped")
	}

	evts := c.DrainEvents()
	if len(evts) != 1 || evts[0].Type != EventTenantAPIClientRotated {
		t.Fatalf("events = %+v", evts)
	}
	if evts[0].Payload["overlapUntil"] == nil {
		t.Fatalf("the rotation event does not carry the overlap deadline: %+v", evts[0].Payload)
	}

	tests := []struct {
		name string
		ref  string
		at   time.Time
		want bool
	}{
		{name: "new credential now", ref: testNewRef, at: testEpoch, want: true},
		{name: "new credential after the overlap", ref: testNewRef, at: overlap.Add(time.Hour), want: true},
		{name: "old credential inside the overlap", ref: testClientRef, at: testEpoch.Add(time.Hour), want: true},
		{name: "old credential one nanosecond before the deadline", ref: testClientRef, at: overlap.Add(-1), want: true},
		// The boundary is exclusive: a deadline that has been reached is a deadline that has
		// passed, so the ninety-day clock in the control evidence means what it says.
		{name: "old credential exactly at the deadline", ref: testClientRef, at: overlap},
		{name: "old credential after the deadline", ref: testClientRef, at: overlap.Add(time.Second)},
		{name: "an unrelated reference", ref: "secret://elsewhere/x", at: testEpoch},
		{name: "empty", ref: "", at: testEpoch},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := c.IsCredentialValid(tc.ref, tc.at); got != tc.want {
				t.Fatalf("IsCredentialValid(%q, %s) = %v, want %v", tc.ref, tc.at, got, tc.want)
			}
		})
	}
}

func TestCredentialIsInvalidWhenTheClientIsNot(t *testing.T) {
	t.Parallel()

	c, clk := newTestClient(t)
	if !c.IsCredentialValid(testClientRef, testEpoch) {
		t.Fatal("an active client's credential was rejected")
	}

	if err := c.Disable("integration parked", clk); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if c.IsCredentialValid(testClientRef, testEpoch) {
		t.Fatal("a disabled client authenticated")
	}
	if c.StatusReason() != "integration parked" {
		t.Fatalf("status reason = %q", c.StatusReason())
	}

	if err := c.Enable(clk); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if !c.IsCredentialValid(testClientRef, testEpoch) || c.StatusReason() != "" {
		t.Fatalf("re-enable did not restore the client: %v %q",
			c.IsCredentialValid(testClientRef, testEpoch), c.StatusReason())
	}

	if err := c.Revoke("", clk); apierror.CodeOf(err) != apierror.CodeValidationFailed {
		t.Fatalf("revoke without a reason: code = %s", apierror.CodeOf(err))
	}
	if err := c.Revoke("credential leaked in a public repository", clk); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if c.IsCredentialValid(testClientRef, testEpoch) {
		t.Fatal("a revoked client authenticated")
	}
	if c.CredentialRef() != "" || c.PreviousCredentialRef() != "" || !c.RotationOverlapUntil().IsZero() {
		t.Fatal("revocation left credential references behind")
	}
	if c.RevokedAt() == nil {
		t.Fatal("revokedAt not stamped")
	}
	evts := c.DrainEvents()
	last := evts[len(evts)-1]
	if last.Type != EventTenantAPIClientRevoked || !last.Type.IsUrgentInvalidation() {
		t.Fatalf("expected an urgent revocation event, got %+v", last)
	}

	// Terminal.
	if err := c.Enable(clk); err == nil {
		t.Fatal("a revoked client was re-enabled")
	}
	if err := c.Rotate(testNewRef, testEpoch.Add(time.Hour), clk); err == nil {
		t.Fatal("a revoked client accepted a rotation")
	}
}

func TestRotationIsOverdue(t *testing.T) {
	t.Parallel()

	c, clk := newTestClient(t)
	const maxAge = 90 * 24 * time.Hour

	if c.RotationIsOverdue(maxAge, testEpoch.Add(89*24*time.Hour)) {
		t.Fatal("a credential 89 days old was reported overdue")
	}
	if !c.RotationIsOverdue(maxAge, testEpoch.Add(91*24*time.Hour)) {
		t.Fatal("a credential 91 days old was not reported overdue")
	}

	// Rotating restarts the clock from the rotation, not from creation.
	rotatedAt := clk.Advance(80 * 24 * time.Hour)
	if err := c.Rotate(testNewRef, rotatedAt.Add(time.Hour), clk); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if c.RotationIsOverdue(maxAge, testEpoch.Add(120*24*time.Hour)) {
		t.Fatal("a recently rotated credential was reported overdue")
	}
	// A disabled or revoked client is not "overdue for rotation"; it is not in service.
	if err := c.Revoke("decommissioned", clk); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if c.RotationIsOverdue(maxAge, testEpoch.Add(10*365*24*time.Hour)) {
		t.Fatal("a revoked client was reported overdue for rotation")
	}
}

func TestRehydrateAPIClient(t *testing.T) {
	t.Parallel()

	base := RehydrateAPIClientParams{
		ID: shared.NewAPIClientID(), TenantID: shared.NewTenantID(), Name: "svc",
		Scopes: []string{"Payments:Write"}, AllowedCIDRs: []string{"203.0.113.7/24"},
		CredentialRef: testClientRef, Status: ClientActive, Version: 4,
	}
	c, err := RehydrateAPIClient(base)
	if err != nil {
		t.Fatalf("RehydrateAPIClient: %v", err)
	}
	if !c.HasScope("payments:write") {
		t.Fatal("scopes were not normalised on rehydration")
	}
	// The prefix is masked on the way in; without it, Contains reports false for every address
	// including the one that was typed.
	if !c.AllowsIP(netip.MustParseAddr("203.0.113.7")) {
		t.Fatal("a rehydrated CIDR did not match the address it was written from")
	}

	bad := base
	bad.Status = "SLEEPING"
	if _, err := RehydrateAPIClient(bad); apierror.CodeOf(err) != apierror.CodeInternalError {
		t.Fatalf("unknown status: code = %s, want INTERNAL_ERROR", apierror.CodeOf(err))
	}

	bad = base
	bad.AllowedCIDRs = []string{"not-a-cidr"}
	if _, err := RehydrateAPIClient(bad); apierror.CodeOf(err) != apierror.CodeInternalError {
		t.Fatalf("malformed cidr: code = %s, want INTERNAL_ERROR", apierror.CodeOf(err))
	}
}

func TestEveryEventTypeHasATopicAndIsListed(t *testing.T) {
	t.Parallel()

	seen := make(map[EventType]bool, len(AllEventTypes))
	for _, e := range AllEventTypes {
		if seen[e] {
			t.Fatalf("%s is listed twice in AllEventTypes", e)
		}
		seen[e] = true
		if e.Topic() != "pp.tenants.tenant.v1" {
			t.Fatalf("%s topic = %q", e, e.Topic())
		}
	}
	for _, e := range []EventType{
		EventTenantCreated, EventTenantSuspended, EventTenantReinstated, EventTenantTerminated,
		EventTenantQuotasUpdated, EventTenantGatewayEnabled, EventTenantGatewayDisabled,
		EventTenantAPIClientCreated, EventTenantAPIClientRotated, EventTenantAPIClientRevoked,
	} {
		if !seen[e] {
			t.Fatalf("%s is not listed in AllEventTypes", e)
		}
	}
}
