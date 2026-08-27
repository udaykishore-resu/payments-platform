package l4config_test

import (
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/internal/ruletest"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/l4config"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

var now = time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)

func eur(minor int64) money.Money { return money.MustNew(minor, "EUR") }

func deps() l4config.Deps {
	d := l4config.DefaultDeps()
	d.CurrencyAllowlist = []money.Currency{"EUR", "GBP", "USD"}
	d.GatewayAllowlist = []shared.GatewayID{"stripe", "adyen"}
	d.MandatoryBlockedCountries = []shared.Country{"IR", "KP"}
	d.SCAFloor = eur(30_00)
	d.SCACorridors = []shared.Country{"DE", "GB"}
	d.MaxTransactionCeiling = eur(10_000_00)
	d.DailyVolumeCeiling = eur(100_000_00)
	d.ResidencyRegions = []string{"eu-west-1"}
	d.TenantBaseCurrency = "EUR"
	return d
}

// base is a publishable configuration for an active merchant with two certified connections:
// every currency/method/country combination it enables is routable, its limits are internally
// consistent, and its routing table compiles within the size budget.
func base() l4config.Subject {
	return l4config.Subject{
		Draft: l4config.Draft{
			SchemaVersion:       "2026-01-01",
			Version:             2,
			Environment:         shared.EnvironmentProduction,
			SupportedCurrencies: []money.Currency{"EUR", "GBP"},
			PaymentMethods:      []shared.PaymentMethod{shared.MethodCard},
			Countries:           []shared.Country{"DE", "GB"},
			Routing: l4config.Routing{
				Strategy: "PRIORITY_WITH_FALLBACK",
				Primary:  "stripe",
				Fallback: []shared.GatewayID{"adyen"},
				Rules: []l4config.RoutingRule{
					{
						When: map[string]l4config.Matcher{
							"currency": {Op: "eq", Values: []string{"EUR"}},
						},
						Then: l4config.RoutingTarget{Primary: "stripe"},
					},
				},
				Weights: map[string]float64{
					"health": 0.25, "successRate": 0.25, "cost": 0.25, "latency": 0.25,
				},
			},
			Risk: l4config.RiskConfig{
				MaxTransactionAmount: eur(5_000_00),
				Require3DSAbove:      eur(30_00),
				DailyVolumeLimit:     eur(50_000_00),
				BlockedCountries:     []shared.Country{"IR", "KP"},
			},
			Limits: l4config.Limits{MaxRefundWindowDays: 120, MaxPartialCaptures: 3},
			Velocity: l4config.Velocity{
				MaxPaymentsPerMinute: 100, MaxPerCardPerHour: 5,
				MaxPerCustomerPerDay: 20, MaxDistinctCardsPerHr: 3,
			},
			Webhooks: []l4config.WebhookEndpoint{
				{
					URL:           "https://merchant.example.com/hooks/payments",
					EventPatterns: []string{"payment.*"},
					MaxAttempts:   6,
					Backoff:       "EXPONENTIAL_JITTER",
				},
			},
			Settlement:   l4config.Settlement{Present: true, Currency: "EUR", HoldDays: 2},
			FeatureFlags: map[string]bool{"partialCapture": true},
		},
		Previous: l4config.Previous{
			Present: true, Version: 1,
			SupportedCurrencies: []money.Currency{"EUR", "GBP"},
			PaymentMethods:      []shared.PaymentMethod{shared.MethodCard},
		},
		Merchant: l4config.MerchantView{
			Status:                    merchant.StatusActive,
			LicensedCountries:         []shared.Country{"DE", "GB", "FR"},
			BankAccountCurrencies:     []money.Currency{"EUR"},
			ExpectedDailyPaymentCount: 5_000,
		},
		Connections: []l4config.ConnectionView{
			{GatewayID: "stripe", Environment: shared.EnvironmentProduction, Status: gateway.StatusCertified, CertificationStatus: gateway.CertificationPassed},
			{GatewayID: "adyen", Environment: shared.EnvironmentProduction, Status: gateway.StatusCertified, CertificationStatus: gateway.CertificationPassed},
		},
		Descriptors: []l4config.DescriptorView{
			{
				GatewayID:        "stripe",
				Currencies:       []money.Currency{"EUR", "GBP", "USD"},
				Methods:          []shared.PaymentMethod{shared.MethodCard},
				Countries:        []shared.Country{"DE", "GB", "US"},
				RefundWindowDays: 180, MaxPartialCaptures: 4,
				Capabilities:     map[string]bool{"partialCapture": true, "networkTokens": true},
				ProcessingRegion: "eu-west-1",
			},
			{
				GatewayID:        "adyen",
				Currencies:       []money.Currency{"EUR", "GBP"},
				Methods:          []shared.PaymentMethod{shared.MethodCard},
				Countries:        []shared.Country{"DE", "GB"},
				RefundWindowDays: 180, MaxPartialCaptures: 4,
				Capabilities:     map[string]bool{"partialCapture": true},
				ProcessingRegion: "eu-west-1",
			},
		},
		ETagMatched: true,
		Now:         now,
	}
}

func TestL4Rules(t *testing.T) {
	// Verifies: BR-07, BR-08, FR-49, FR-51, FR-52.
	t.Parallel()
	set := l4config.Rules(deps())

	ruletest.Run(t, set, base, []ruletest.Case[l4config.Subject]{
		{
			ID:   "L4.SCHEMA_VERSION_KNOWN",
			Pass: func(s *l4config.Subject) { s.Draft.SchemaVersion = "2025-06-01" },
			Fail: func(s *l4config.Subject) { s.Draft.SchemaVersion = "1999-01-01" },
		},
		{
			ID:   "L4.VERSION_IS_SUCCESSOR",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) { s.Draft.Version = 5 },
		},
		{
			ID:   "L4.ENVIRONMENT_MATCHES_MERCHANT_STATE",
			Pass: func(s *l4config.Subject) { s.Merchant.Status = merchant.StatusApproved },
			Fail: func(s *l4config.Subject) { s.Merchant.Status = merchant.StatusConfiguring },
		},
		{
			ID:   "L4.CURRENCIES_NON_EMPTY",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) { s.Draft.SupportedCurrencies = nil },
		},
		{
			ID:   "L4.CURRENCIES_ARE_ISO4217",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) {
				s.Draft.SupportedCurrencies = []money.Currency{"EUR", "XBT"}
			},
		},
		{
			ID:   "L4.CURRENCIES_WITHIN_TENANT_ALLOWLIST",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) {
				s.Draft.SupportedCurrencies = []money.Currency{"EUR", "JPY"}
			},
		},
		{
			ID:   "L4.METHODS_NON_EMPTY",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) { s.Draft.PaymentMethods = nil },
		},
		{
			ID:   "L4.METHODS_ARE_KNOWN",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) {
				s.Draft.PaymentMethods = []shared.PaymentMethod{"CARDS"}
			},
		},
		{
			ID:   "L4.COUNTRIES_ARE_ISO3166",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) { s.Draft.Countries = []shared.Country{"DE", "XX"} },
		},
		{
			ID:   "L4.COUNTRIES_SUBSET_OF_MERCHANT_LICENSED",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) { s.Draft.Countries = []shared.Country{"DE", "US"} },
		},
		{
			ID:   "L4.ROUTING_STRATEGY_IS_KNOWN",
			Pass: func(s *l4config.Subject) { s.Draft.Routing.Strategy = "LEAST_COST" },
			Fail: func(s *l4config.Subject) { s.Draft.Routing.Strategy = "RANDOM" },
		},
		{
			ID:   "L4.ROUTING_PRIMARY_IS_CONNECTED",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) {
				s.Connections[0].CertificationStatus = gateway.CertificationFailed
			},
		},
		{
			ID:   "L4.ROUTING_FALLBACKS_ARE_CONNECTED",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) {
				s.Connections[1].CertificationStatus = gateway.CertificationInProgress
			},
		},
		{
			ID:   "L4.ROUTING_FALLBACK_EXCLUDES_PRIMARY",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) {
				s.Draft.Routing.Fallback = []shared.GatewayID{"stripe"}
			},
		},
		{
			ID:   "L4.ROUTING_HAS_AT_LEAST_ONE_FALLBACK",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) { s.Draft.Routing.Fallback = nil },
		},
		{
			ID:   "L4.ROUTING_RULE_PREDICATE_FIELDS_KNOWN",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) {
				s.Draft.Routing.Rules[0].When = map[string]l4config.Matcher{
					"cardIssuer": {Op: "eq", Values: []string{"barclays"}},
				}
			},
		},
		{
			ID: "L4.ROUTING_RULE_MATCHER_VALUES_VALID",
			Pass: func(s *l4config.Subject) {
				s.Draft.Routing.Rules[0].When = map[string]l4config.Matcher{
					"amountRange": {Op: "range", Min: 100, Max: 10_000},
				}
			},
			Fail: func(s *l4config.Subject) {
				s.Draft.Routing.Rules[0].When = map[string]l4config.Matcher{
					"amountRange": {Op: "range", Min: 10_000, Max: 100},
				}
			},
		},
		{
			ID:   "L4.ROUTING_RULES_WITHIN_SIZE_BUDGET",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) {
				s.Draft.Routing.Rules = repeatRule(s.Draft.Routing.Rules[0], 65)
			},
		},
		{
			ID: "L4.ROUTING_RULES_ARE_REACHABLE",
			Pass: func(s *l4config.Subject) {
				s.Draft.Routing.Rules = []l4config.RoutingRule{
					rule("currency", l4config.Matcher{Op: "eq", Values: []string{"EUR"}}, "stripe"),
					rule("currency", l4config.Matcher{Op: "eq", Values: []string{"GBP"}}, "stripe"),
				}
			},
			Fail: func(s *l4config.Subject) {
				s.Draft.Routing.Rules = []l4config.RoutingRule{
					rule("currency", l4config.Matcher{Op: "in", Values: []string{"EUR", "GBP"}}, "stripe"),
					rule("currency", l4config.Matcher{Op: "eq", Values: []string{"EUR"}}, "stripe"),
				}
			},
		},
		{
			ID:   "L4.ROUTING_WEIGHTS_NON_NEGATIVE",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) { s.Draft.Routing.Weights["cost"] = -0.25 },
		},
		{
			ID:   "L4.ROUTING_WEIGHTS_SUM_TO_ONE",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) { s.Draft.Routing.Weights["cost"] = 0.15 },
		},
		{
			ID:   "L4.EVERY_CURRENCY_METHOD_PAIR_ROUTABLE",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) {
				s.Draft.SupportedCurrencies = []money.Currency{"EUR", "JPY"}
			},
		},
		{
			ID:   "L4.ROUTED_GATEWAY_SUPPORTS_ITS_PREDICATE",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) {
				s.Draft.Routing.Rules = []l4config.RoutingRule{
					rule("currency", l4config.Matcher{Op: "eq", Values: []string{"USD"}}, "adyen"),
				}
			},
		},
		{
			ID:   "L4.RISK_LIMIT_CURRENCY_SUPPORTED",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) {
				s.Draft.Risk.MaxTransactionAmount = money.MustNew(5_000_00, "USD")
			},
		},
		{
			ID:   "L4.THREEDS_THRESHOLD_BELOW_MAX_AMOUNT",
			Pass: func(s *l4config.Subject) { s.Draft.Risk.Require3DSAbove = eur(5_000_00) },
			Fail: func(s *l4config.Subject) { s.Draft.Risk.Require3DSAbove = eur(6_000_00) },
		},
		{
			ID:   "L4.THREEDS_THRESHOLD_MEETS_SCA_FLOOR",
			Pass: func(s *l4config.Subject) { s.Draft.Risk.Require3DSAbove = eur(29_99) },
			Fail: func(s *l4config.Subject) { s.Draft.Risk.Require3DSAbove = eur(30_01) },
		},
		{
			ID:   "L4.DAILY_LIMIT_AT_LEAST_MAX_TRANSACTION",
			Pass: func(s *l4config.Subject) { s.Draft.Risk.DailyVolumeLimit = eur(5_000_00) },
			Fail: func(s *l4config.Subject) { s.Draft.Risk.DailyVolumeLimit = eur(4_999_99) },
		},
		{
			ID:   "L4.VELOCITY_LIMITS_POSITIVE",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) { s.Draft.Velocity.MaxPerCardPerHour = -1 },
		},
		{
			ID:   "L4.VELOCITY_CONSISTENT_WITH_VOLUME",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) { s.Draft.Velocity.MaxPaymentsPerMinute = 1 },
		},
		{
			ID:   "L4.BLOCKED_COUNTRIES_DISJOINT",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) {
				s.Draft.Risk.BlockedCountries = []shared.Country{"IR", "KP", "DE"}
			},
		},
		{
			ID:   "L4.BLOCKED_COUNTRIES_INCLUDE_MANDATORY",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) {
				s.Draft.Risk.BlockedCountries = []shared.Country{"IR"}
			},
		},
		{
			ID:   "L4.REFUND_WINDOW_WITHIN_GATEWAY_MAX",
			Pass: func(s *l4config.Subject) { s.Draft.Limits.MaxRefundWindowDays = 180 },
			Fail: func(s *l4config.Subject) { s.Draft.Limits.MaxRefundWindowDays = 181 },
		},
		{
			ID:   "L4.MAX_PARTIAL_CAPTURES_SUPPORTED",
			Pass: func(s *l4config.Subject) { s.Draft.Limits.MaxPartialCaptures = 4 },
			Fail: func(s *l4config.Subject) { s.Draft.Limits.MaxPartialCaptures = 5 },
		},
		{
			ID:   "L4.WEBHOOK_ENDPOINTS_HTTPS",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) {
				s.Draft.Webhooks[0].URL = "http://merchant.example.com/hooks/payments"
			},
		},
		{
			ID: "L4.WEBHOOK_EVENT_PATTERNS_KNOWN",
			Pass: func(s *l4config.Subject) {
				s.Draft.Webhooks[0].EventPatterns = []string{"payment.captured.v1"}
			},
			Fail: func(s *l4config.Subject) {
				s.Draft.Webhooks[0].EventPatterns = []string{"invoice.*"}
			},
		},
		{
			ID:   "L4.WEBHOOK_RETRY_POLICY_WITHIN_BOUNDS",
			Pass: func(s *l4config.Subject) { s.Draft.Webhooks[0].MaxAttempts = 12 },
			Fail: func(s *l4config.Subject) { s.Draft.Webhooks[0].MaxAttempts = 13 },
		},
		{
			ID:   "L4.SETTLEMENT_CURRENCY_HAS_BANK_ACCOUNT",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) { s.Draft.Settlement.Currency = "GBP" },
		},
		{
			ID:   "L4.SETTLEMENT_HOLD_DAYS_WITHIN_POLICY",
			Pass: func(s *l4config.Subject) { s.Draft.Settlement.HoldDays = 30 },
			Fail: func(s *l4config.Subject) { s.Draft.Settlement.HoldDays = 31 },
		},
		{
			ID:   "L4.FEATURE_FLAGS_ARE_KNOWN",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) { s.Draft.FeatureFlags["timeTravel"] = true },
		},
		{
			ID:   "L4.FEATURE_FLAG_HAS_CAPABILITY",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) { s.Draft.FeatureFlags["networkTokens"] = true },
		},
		{
			ID:   "L4.TENANT_GATEWAY_ALLOWLIST",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) { s.Draft.Routing.Primary = "worldpay" },
		},
		{
			ID:   "L4.TENANT_RESIDENCY_COMPATIBLE",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) { s.Descriptors[1].ProcessingRegion = "ap-southeast-1" },
		},
		{
			ID:   "L4.TENANT_LIMIT_CEILING_RESPECTED",
			Pass: func(s *l4config.Subject) { s.Draft.Risk.MaxTransactionAmount = eur(10_000_00) },
			Fail: func(s *l4config.Subject) { s.Draft.Risk.MaxTransactionAmount = eur(10_000_01) },
		},
		{
			ID:   "L4.NO_SILENT_CAPABILITY_REGRESSION",
			Pass: func(s *l4config.Subject) {},
			Fail: func(s *l4config.Subject) {
				s.Previous.PaymentMethods = []shared.PaymentMethod{shared.MethodCard, shared.MethodSEPADebit}
				s.InFlight.ByMethod = map[shared.PaymentMethod]int{shared.MethodSEPADebit: 7}
			},
		},
	})
}

// TestL4PublishableConfigurationIsClean anchors the base document.
func TestL4PublishableConfigurationIsClean(t *testing.T) {
	// Verifies: BR-09.
	t.Parallel()
	rep := l4config.Rules(deps()).Evaluate(t.Context(), base())
	if !rep.OK() {
		t.Fatalf("the reference configuration was rejected: %v", rep.Errors())
	}
	if got := len(rep.Failures()); got != 0 {
		t.Fatalf("the reference configuration produced %d warnings: %v", got, rep.Failures())
	}
}

// TestL4CollectAllReportsEveryProblemAtOnce is the reason this level is CollectAll: an operator
// editing a document must see the whole list, not the first item of it.
func TestL4CollectAllReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()
	s := base()
	s.Draft.SchemaVersion = "1999-01-01"
	s.Draft.Routing.Strategy = "RANDOM"
	s.Draft.Settlement.HoldDays = 99
	s.Draft.Webhooks[0].MaxAttempts = 44

	rep := l4config.Rules(deps()).Evaluate(t.Context(), s)

	want := map[string]bool{
		"L4.SCHEMA_VERSION_KNOWN":               false,
		"L4.ROUTING_STRATEGY_IS_KNOWN":          false,
		"L4.SETTLEMENT_HOLD_DAYS_WITHIN_POLICY": false,
		"L4.WEBHOOK_RETRY_POLICY_WITHIN_BOUNDS": false,
	}
	for _, o := range rep.Errors() {
		if _, ok := want[string(o.Rule)]; ok {
			want[string(o.Rule)] = true
		}
	}
	for id, seen := range want {
		if !seen {
			t.Errorf("%s did not appear in the report; CollectAll stopped early", id)
		}
	}

	err := rep.AsError()
	if err == nil || len(err.Details) < 4 {
		t.Fatalf("AsError did not carry every failure: %+v", err)
	}
}

func rule(field string, m l4config.Matcher, target shared.GatewayID) l4config.RoutingRule {
	return l4config.RoutingRule{
		When: map[string]l4config.Matcher{field: m},
		Then: l4config.RoutingTarget{Primary: target},
	}
}

func repeatRule(r l4config.RoutingRule, n int) []l4config.RoutingRule {
	out := make([]l4config.RoutingRule, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, r)
	}
	return out
}
