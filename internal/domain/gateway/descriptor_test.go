package gateway

import (
	"errors"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

var testEpoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func testClock() *shared.FixedClock { return &shared.FixedClock{T: testEpoch} }

func fullCapabilities() Capabilities {
	return Capabilities{
		Countries:  []shared.Country{"US", "DE", "GB"},
		Currencies: []money.Currency{"USD", "EUR", "JPY"},
		Methods: []shared.PaymentMethod{
			shared.MethodCard, shared.MethodApplePay, shared.MethodSEPADebit,
		},
		Operations: []shared.Operation{
			shared.OpAuthorize, shared.OpCapture, shared.OpRefund, shared.OpVoid, shared.OpLookup,
		},
		SupportsPartialCapture:   true,
		SupportsMultipleCaptures: true,
		SupportsPartialRefund:    true,
		SupportsVoid:             true,
		Supports3DS2:             true,
		SupportsNetworkTokens:    true,
		SupportsIdempotencyKeys:  true,
		MaxRefundWindow:          180 * 24 * time.Hour,
		AuthorizationValidity:    7 * 24 * time.Hour,
		MinAmount: map[money.Currency]money.Money{
			"USD": money.MustNew(50, "USD"),
			"EUR": money.MustNew(50, "EUR"),
		},
		MaxAmount: map[money.Currency]money.Money{
			"USD": money.MustNew(100_000_00, "USD"),
			"EUR": money.MustNew(100_000_00, "EUR"),
		},
	}
}

func testGateway(t *testing.T) *Gateway {
	t.Helper()
	cm, err := NewCostModel(
		CostRate{Currency: "USD", Method: shared.MethodCard, FixedFee: money.MustNew(30, "USD"), BasisPoints: 290},
		CostRate{Currency: "USD", Method: AnyMethod, FixedFee: money.MustNew(25, "USD"), BasisPoints: 340},
		CostRate{Currency: "EUR", Method: AnyMethod, FixedFee: money.MustNew(25, "EUR"), BasisPoints: 140},
	)
	if err != nil {
		t.Fatalf("NewCostModel: %v", err)
	}
	g, err := NewGateway(NewGatewayParams{
		ID:          "stripe",
		DisplayName: "Stripe",
		Vendor:      "Stripe Payments Europe, Ltd.",
		APIVersion:  "2026-01-15",
		BaseURLs: map[shared.Environment]string{
			shared.EnvironmentSandbox:    "https://api.sandbox.example",
			shared.EnvironmentProduction: "https://api.example",
		},
		Capabilities:    fullCapabilities(),
		SignatureScheme: SchemeHMACSHA256,
		Status:          StatusActive,
		CostModel:       cm,
	}, testClock())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	return g
}

func TestNewGatewayValidation(t *testing.T) {
	// Verifies: BR-34, FR-33.
	t.Parallel()

	base := func() NewGatewayParams {
		return NewGatewayParams{
			ID:              "stripe",
			DisplayName:     "Stripe",
			BaseURLs:        map[shared.Environment]string{shared.EnvironmentSandbox: "https://s"},
			Capabilities:    fullCapabilities(),
			SignatureScheme: SchemeHMACSHA256,
			Status:          StatusActive,
		}
	}

	tests := []struct {
		name    string
		mutate  func(*NewGatewayParams)
		wantErr apierror.Code
	}{
		{name: "valid", mutate: func(*NewGatewayParams) {}},
		{
			name:    "missing id",
			mutate:  func(p *NewGatewayParams) { p.ID = "" },
			wantErr: apierror.CodeValidationFailed,
		},
		{
			name:    "id is not a slug",
			mutate:  func(p *NewGatewayParams) { p.ID = "Stripe Payments" },
			wantErr: apierror.CodeValidationFailed,
		},
		{
			name:    "missing display name",
			mutate:  func(p *NewGatewayParams) { p.DisplayName = "" },
			wantErr: apierror.CodeValidationFailed,
		},
		{
			name:    "unknown signature scheme",
			mutate:  func(p *NewGatewayParams) { p.SignatureScheme = "MAGIC" },
			wantErr: apierror.CodeValidationFailed,
		},
		{
			name:    "no base urls",
			mutate:  func(p *NewGatewayParams) { p.BaseURLs = nil },
			wantErr: apierror.CodeConfigurationInvalid,
		},
		{
			name: "empty base url",
			mutate: func(p *NewGatewayParams) {
				p.BaseURLs = map[shared.Environment]string{shared.EnvironmentSandbox: ""}
			},
			wantErr: apierror.CodeConfigurationInvalid,
		},
		{
			name: "unsigned webhooks are refused in production",
			mutate: func(p *NewGatewayParams) {
				p.SignatureScheme = SchemeNone
				p.BaseURLs = map[shared.Environment]string{
					shared.EnvironmentSandbox:    "https://s",
					shared.EnvironmentProduction: "https://p",
				}
			},
			wantErr: apierror.CodeConfigurationInvalid,
		},
		{
			name: "unsigned webhooks are permitted in sandbox only",
			mutate: func(p *NewGatewayParams) {
				p.SignatureScheme = SchemeNone
			},
		},
		{
			name:    "incoherent capabilities are rejected",
			mutate:  func(p *NewGatewayParams) { p.Capabilities.SupportsPartialCapture = false },
			wantErr: apierror.CodeConfigurationInvalid,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := base()
			tc.mutate(&p)
			g, err := NewGateway(p, testClock())
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				if g.Version() != 1 {
					t.Fatalf("new gateway version = %d, want 1", g.Version())
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %s, got nil", tc.wantErr)
			}
			if got := apierror.CodeOf(err); got != tc.wantErr {
				t.Fatalf("code = %s, want %s (%v)", got, tc.wantErr, err)
			}
		})
	}
}

func TestNewGatewayDefaultsStatusToActive(t *testing.T) {
	t.Parallel()
	g, err := NewGateway(NewGatewayParams{
		ID: "adyen", DisplayName: "Adyen",
		BaseURLs:        map[shared.Environment]string{shared.EnvironmentSandbox: "https://s"},
		Capabilities:    fullCapabilities(),
		SignatureScheme: SchemeHMACSHA512,
	}, testClock())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	if g.Status() != StatusActive {
		t.Fatalf("status = %s, want ACTIVE", g.Status())
	}
}

func TestCapabilitiesValidate(t *testing.T) {
	// Verifies: BR-34.
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*Capabilities)
		wantField string
	}{
		{name: "valid", mutate: func(*Capabilities) {}},
		{name: "no countries", mutate: func(c *Capabilities) { c.Countries = nil }, wantField: "capabilities.countries"},
		{name: "no currencies", mutate: func(c *Capabilities) { c.Currencies = nil }, wantField: "capabilities.currencies"},
		{name: "no methods", mutate: func(c *Capabilities) { c.Methods = nil }, wantField: "capabilities.paymentMethods"},
		{name: "no operations", mutate: func(c *Capabilities) { c.Operations = nil }, wantField: "capabilities.operations"},
		{
			name:      "unknown country",
			mutate:    func(c *Capabilities) { c.Countries = []shared.Country{"XX"} },
			wantField: "capabilities.countries",
		},
		{
			name:      "unknown currency",
			mutate:    func(c *Capabilities) { c.Currencies = []money.Currency{"XYZ"} },
			wantField: "capabilities.currencies",
		},
		{
			name:      "unknown method",
			mutate:    func(c *Capabilities) { c.Methods = []shared.PaymentMethod{"CRYPTO"} },
			wantField: "capabilities.paymentMethods",
		},
		{
			name:      "multiple captures without partial capture",
			mutate:    func(c *Capabilities) { c.SupportsPartialCapture = false },
			wantField: "capabilities.supportsMultipleCaptures",
		},
		{
			name:      "negative refund window",
			mutate:    func(c *Capabilities) { c.MaxRefundWindow = -time.Hour },
			wantField: "capabilities.maxRefundWindow",
		},
		{
			name:      "negative authorization validity",
			mutate:    func(c *Capabilities) { c.AuthorizationValidity = -time.Hour },
			wantField: "capabilities.authorizationValidity",
		},
		{
			name: "min bound denominated in the wrong currency",
			mutate: func(c *Capabilities) {
				c.MinAmount = map[money.Currency]money.Money{"USD": money.MustNew(50, "EUR")}
			},
			wantField: "capabilities.minAmount",
		},
		{
			name: "min above max",
			mutate: func(c *Capabilities) {
				c.MinAmount = map[money.Currency]money.Money{"USD": money.MustNew(900, "USD")}
				c.MaxAmount = map[money.Currency]money.Money{"USD": money.MustNew(100, "USD")}
			},
			wantField: "capabilities.minAmount",
		},
		{
			name: "max bound denominated in the wrong currency",
			mutate: func(c *Capabilities) {
				c.MinAmount = nil
				c.MaxAmount = map[money.Currency]money.Money{"USD": money.MustNew(100, "JPY")}
			},
			wantField: "capabilities.maxAmount",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := fullCapabilities()
			tc.mutate(&c)
			err := c.Validate()
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error naming %s", tc.wantField)
			}
			var ae *apierror.Error
			if !errors.As(err, &ae) {
				t.Fatalf("error is not an *apierror.Error: %T", err)
			}
			if len(ae.Details) != 1 || ae.Details[0].Field != tc.wantField {
				t.Fatalf("details = %+v, want field %s", ae.Details, tc.wantField)
			}
			if ae.Details[0].RuleID == "" {
				t.Fatal("detail carries no RuleID")
			}
		})
	}
}

func TestCapabilitiesSupportsNamesTheFailedDimension(t *testing.T) {
	t.Parallel()

	caps := fullCapabilities()

	tests := []struct {
		name      string
		country   shared.Country
		currency  money.Currency
		method    shared.PaymentMethod
		op        shared.Operation
		wantCode  apierror.Code
		wantField string
		wantRule  string
	}{
		{
			name: "everything supported", country: "US", currency: "USD",
			method: shared.MethodCard, op: shared.OpAuthorize,
		},
		{
			name: "country", country: "BR", currency: "USD",
			method: shared.MethodCard, op: shared.OpAuthorize,
			wantCode: apierror.CodeCountryBlocked, wantField: "country",
			wantRule: "L5.GATEWAY_SUPPORTS_COUNTRY",
		},
		{
			name: "currency", country: "US", currency: "BRL",
			method: shared.MethodCard, op: shared.OpAuthorize,
			wantCode: apierror.CodeCurrencyNotSupported, wantField: "currency",
			wantRule: "L5.GATEWAY_SUPPORTS_CURRENCY",
		},
		{
			name: "method", country: "US", currency: "USD",
			method: shared.MethodUPI, op: shared.OpAuthorize,
			wantCode: apierror.CodePaymentMethodNotSupported, wantField: "paymentMethod",
			wantRule: "L5.GATEWAY_SUPPORTS_METHOD",
		},
		{
			name: "operation", country: "US", currency: "USD",
			method: shared.MethodCard, op: shared.OpProvision,
			wantCode: apierror.CodeGatewayNotConfigured, wantField: "operation",
			wantRule: "L5.GATEWAY_SUPPORTS_OPERATION",
		},
		{
			// The check order is fixed so the reported reason is stable: with both the country and
			// the currency wrong, the country is always the one reported.
			name: "country wins over currency", country: "BR", currency: "BRL",
			method: shared.MethodCard, op: shared.OpAuthorize,
			wantCode: apierror.CodeCountryBlocked, wantField: "country",
			wantRule: "L5.GATEWAY_SUPPORTS_COUNTRY",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := caps.Supports(tc.country, tc.currency, tc.method, tc.op)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("expected support, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %s, got nil", tc.wantCode)
			}
			if got := apierror.CodeOf(err); got != tc.wantCode {
				t.Fatalf("code = %s, want %s", got, tc.wantCode)
			}
			var ae *apierror.Error
			if !errors.As(err, &ae) || len(ae.Details) != 1 {
				t.Fatalf("expected exactly one detail, got %v", err)
			}
			if ae.Details[0].Field != tc.wantField {
				t.Fatalf("detail field = %q, want %q", ae.Details[0].Field, tc.wantField)
			}
			if ae.Details[0].RuleID != tc.wantRule {
				t.Fatalf("rule id = %q, want %q", ae.Details[0].RuleID, tc.wantRule)
			}
		})
	}
}

func TestGatewaySupportsChecksStatusFirst(t *testing.T) {
	t.Parallel()

	g := testGateway(t)
	if err := g.Supports("US", "USD", shared.MethodCard, shared.OpAuthorize); err != nil {
		t.Fatalf("active gateway should support this route: %v", err)
	}

	// DEPRECATED still accepts traffic: withdrawing it before merchants migrate is an outage.
	if err := g.SetStatus(StatusDeprecated, testClock()); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := g.Supports("US", "USD", shared.MethodCard, shared.OpAuthorize); err != nil {
		t.Fatalf("deprecated gateway should still accept traffic: %v", err)
	}

	if err := g.SetStatus(StatusDisabled, testClock()); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	err := g.Supports("US", "USD", shared.MethodCard, shared.OpAuthorize)
	if apierror.CodeOf(err) != apierror.CodeGatewayNotConfigured {
		t.Fatalf("disabled gateway: code = %s, want GATEWAY_NOT_CONFIGURED", apierror.CodeOf(err))
	}
	// Even a route the gateway cannot serve reports the status, not the capability: a disabled
	// gateway's capabilities are irrelevant and reporting them sends an operator to the wrong place.
	err = g.Supports("BR", "BRL", shared.MethodUPI, shared.OpAuthorize)
	if apierror.CodeOf(err) != apierror.CodeGatewayNotConfigured {
		t.Fatalf("disabled gateway with a bad route: code = %s, want GATEWAY_NOT_CONFIGURED", apierror.CodeOf(err))
	}
}

func TestCanRefundAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		window time.Duration
		age    time.Duration
		want   bool
	}{
		{name: "inside the window", window: 180 * 24 * time.Hour, age: 24 * time.Hour, want: true},
		{name: "exactly at the window", window: 180 * 24 * time.Hour, age: 180 * 24 * time.Hour, want: true},
		{name: "one nanosecond past", window: 180 * 24 * time.Hour, age: 180*24*time.Hour + 1, want: false},
		{name: "zero window means unbounded", window: 0, age: 10 * 365 * 24 * time.Hour, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := fullCapabilities()
			c.MaxRefundWindow = tc.window
			if got := c.CanRefundAfter(tc.age); got != tc.want {
				t.Fatalf("CanRefundAfter(%s) = %v, want %v", tc.age, got, tc.want)
			}
		})
	}
}

func TestAmountWithinBounds(t *testing.T) {
	t.Parallel()

	caps := fullCapabilities()

	tests := []struct {
		name     string
		amount   money.Money
		wantCode apierror.Code
		wantDtl  string
	}{
		{name: "inside bounds", amount: money.MustNew(1000, "USD")},
		{name: "exactly at the floor", amount: money.MustNew(50, "USD")},
		{name: "exactly at the ceiling", amount: money.MustNew(100_000_00, "USD")},
		{
			name: "below the floor", amount: money.MustNew(49, "USD"),
			wantCode: apierror.CodeAmountInvalid, wantDtl: "BELOW_GATEWAY_MINIMUM",
		},
		{
			name: "above the ceiling", amount: money.MustNew(100_000_01, "USD"),
			wantCode: apierror.CodeAmountExceedsLimit, wantDtl: "ABOVE_GATEWAY_MAXIMUM",
		},
		{
			// JPY has no configured bound, so it is unbounded in both directions rather than
			// silently compared against the USD figure.
			name: "unbounded currency", amount: money.MustNew(1, "JPY"),
		},
		{
			name: "invalid currency", amount: money.Money{},
			wantCode: apierror.CodeAmountInvalid, wantDtl: "UNKNOWN_CURRENCY",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := caps.AmountWithinBounds(tc.amount)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("expected within bounds, got %v", err)
				}
				return
			}
			if got := apierror.CodeOf(err); got != tc.wantCode {
				t.Fatalf("code = %s, want %s (%v)", got, tc.wantCode, err)
			}
			var ae *apierror.Error
			if !errors.As(err, &ae) || len(ae.Details) != 1 || ae.Details[0].Code != tc.wantDtl {
				t.Fatalf("details = %+v, want code %s", ae.Details, tc.wantDtl)
			}
		})
	}
}

func TestCostModelEstimate(t *testing.T) {
	t.Parallel()

	g := testGateway(t)

	tests := []struct {
		name     string
		amount   money.Money
		method   shared.PaymentMethod
		want     money.Money
		wantCode apierror.Code
	}{
		{
			// Exact (USD, CARD) rate: 2.90% of 10.00 = 0.29, plus the 0.30 fixed fee.
			name: "exact rate wins", amount: money.MustNew(1000, "USD"), method: shared.MethodCard,
			want: money.MustNew(59, "USD"),
		},
		{
			// No (USD, APPLE_PAY) rate, so the currency-wide fallback applies: 3.40% + 0.25.
			name: "falls back to the currency rate", amount: money.MustNew(1000, "USD"), method: shared.MethodApplePay,
			want: money.MustNew(59, "USD"),
		},
		{
			// 1.40% of 100.00 = 1.40, plus 0.25.
			name: "eur fallback", amount: money.MustNew(10000, "EUR"), method: shared.MethodCard,
			want: money.MustNew(165, "EUR"),
		},
		{
			// Rounding is half away from zero, in integers: 2.90% of 0.51 = 0.01479 → 1 minor unit.
			name: "basis points round half away from zero", amount: money.MustNew(51, "USD"), method: shared.MethodCard,
			want: money.MustNew(31, "USD"),
		},
		{
			name:   "unpriced currency is an error, not free",
			amount: money.MustNew(1000, "JPY"), method: shared.MethodCard,
			wantCode: apierror.CodeGatewayNotConfigured,
		},
		{
			name: "invalid amount", amount: money.Money{}, method: shared.MethodCard,
			wantCode: apierror.CodeAmountInvalid,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := g.EstimateCost(tc.amount, tc.method)
			if tc.wantCode != "" {
				if apierror.CodeOf(err) != tc.wantCode {
					t.Fatalf("code = %s, want %s (%v)", apierror.CodeOf(err), tc.wantCode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("EstimateCost: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("EstimateCost = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestNewCostModelValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		rates []CostRate
		valid bool
	}{
		{
			name:  "valid",
			rates: []CostRate{{Currency: "USD", Method: shared.MethodCard, FixedFee: money.MustNew(30, "USD"), BasisPoints: 290}},
			valid: true,
		},
		{name: "empty is legal", rates: nil, valid: true},
		{
			name:  "unknown currency",
			rates: []CostRate{{Currency: "XYZ", FixedFee: money.MustNew(0, "USD")}},
		},
		{
			name:  "unknown method",
			rates: []CostRate{{Currency: "USD", Method: "CRYPTO", FixedFee: money.MustNew(0, "USD")}},
		},
		{
			name:  "negative basis points",
			rates: []CostRate{{Currency: "USD", FixedFee: money.MustNew(0, "USD"), BasisPoints: -1}},
		},
		{
			name:  "fee in the wrong currency",
			rates: []CostRate{{Currency: "USD", FixedFee: money.MustNew(30, "EUR")}},
		},
		{
			name:  "negative fee",
			rates: []CostRate{{Currency: "USD", FixedFee: money.MustNew(-1, "USD")}},
		},
		{
			name: "duplicate rate",
			rates: []CostRate{
				{Currency: "USD", Method: shared.MethodCard, FixedFee: money.MustNew(30, "USD")},
				{Currency: "USD", Method: shared.MethodCard, FixedFee: money.MustNew(40, "USD")},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewCostModel(tc.rates...)
			if tc.valid && err != nil {
				t.Fatalf("expected valid, got %v", err)
			}
			if !tc.valid {
				if err == nil {
					t.Fatal("expected a validation error")
				}
				if apierror.CodeOf(err) != apierror.CodeConfigurationInvalid {
					t.Fatalf("code = %s, want CONFIGURATION_INVALID", apierror.CodeOf(err))
				}
			}
		})
	}
}

func TestBaseURLIsEnvironmentScoped(t *testing.T) {
	t.Parallel()

	g, err := NewGateway(NewGatewayParams{
		ID: "sandboxonly", DisplayName: "Sandbox Only",
		BaseURLs:        map[shared.Environment]string{shared.EnvironmentSandbox: "https://s"},
		Capabilities:    fullCapabilities(),
		SignatureScheme: SchemeHMACSHA256,
	}, testClock())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	if u, err := g.BaseURL(shared.EnvironmentSandbox); err != nil || u != "https://s" {
		t.Fatalf("sandbox base url = %q, %v", u, err)
	}
	_, err = g.BaseURL(shared.EnvironmentProduction)
	if apierror.CodeOf(err) != apierror.CodeGatewayNotConfigured {
		t.Fatalf("production base url: code = %s, want GATEWAY_NOT_CONFIGURED", apierror.CodeOf(err))
	}
}

func TestAccessorsReturnCopies(t *testing.T) {
	t.Parallel()

	g := testGateway(t)

	caps := g.Capabilities()
	caps.Countries[0] = "XX"
	caps.MinAmount["USD"] = money.MustNew(999999, "USD")
	if g.Capabilities().Countries[0] == "XX" {
		t.Fatal("mutating the returned capabilities changed the aggregate's country set")
	}
	if !g.Capabilities().MinAmount["USD"].Equal(money.MustNew(50, "USD")) {
		t.Fatal("mutating the returned capabilities changed the aggregate's bounds")
	}

	urls := g.BaseURLs()
	urls[shared.EnvironmentProduction] = "https://evil.example"
	if got, _ := g.BaseURL(shared.EnvironmentProduction); got != "https://api.example" {
		t.Fatalf("mutating the returned url map changed the aggregate: %q", got)
	}
}

func TestRehydrateGatewayRejectsUnknownEnums(t *testing.T) {
	t.Parallel()

	base := RehydrateGatewayParams{
		ID: "stripe", DisplayName: "Stripe",
		BaseURLs:        map[shared.Environment]string{shared.EnvironmentSandbox: "https://s"},
		Capabilities:    fullCapabilities(),
		SignatureScheme: SchemeHMACSHA256,
		Status:          StatusActive,
		Version:         7,
	}

	if _, err := RehydrateGateway(base); err != nil {
		t.Fatalf("valid rehydrate: %v", err)
	}

	bad := base
	bad.Status = "QUARANTINED"
	if _, err := RehydrateGateway(bad); apierror.CodeOf(err) != apierror.CodeInternalError {
		t.Fatalf("unknown status: code = %s, want INTERNAL_ERROR", apierror.CodeOf(err))
	}

	bad = base
	bad.SignatureScheme = "PQC_DILITHIUM"
	if _, err := RehydrateGateway(bad); apierror.CodeOf(err) != apierror.CodeInternalError {
		t.Fatalf("unknown scheme: code = %s, want INTERNAL_ERROR", apierror.CodeOf(err))
	}
}

func TestUpdateCapabilitiesRevalidates(t *testing.T) {
	// Verifies: FR-34.
	t.Parallel()

	g := testGateway(t)
	before := g.Version()

	broken := fullCapabilities()
	broken.Countries = nil
	if err := g.UpdateCapabilities(broken, testClock()); err == nil {
		t.Fatal("expected an invalid capability set to be refused")
	}
	if g.Version() != before {
		t.Fatal("a refused update bumped the version")
	}

	ok := fullCapabilities()
	ok.Countries = append(ok.Countries, "FR")
	if err := g.UpdateCapabilities(ok, testClock()); err != nil {
		t.Fatalf("UpdateCapabilities: %v", err)
	}
	if g.Version() != before.Next() {
		t.Fatalf("version = %d, want %d", g.Version(), before.Next())
	}
	if err := g.Supports("FR", "EUR", shared.MethodCard, shared.OpAuthorize); err != nil {
		t.Fatalf("newly licensed country should be supported: %v", err)
	}
}
