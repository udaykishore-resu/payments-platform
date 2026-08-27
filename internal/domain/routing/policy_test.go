package routing_test

import (
	"errors"
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/domain/routing"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

func usd(minor int64) money.Money { return money.MustNew(minor, "USD") }

func usdPtr(minor int64) *money.Money {
	m := usd(minor)
	return &m
}

// ruleIDs extracts the validation-plane rule IDs from an error's details, so a test can assert
// on the stable rule identity rather than on a message string that is free to be reworded.
func ruleIDs(t *testing.T, err error) []string {
	t.Helper()
	var e *apierror.Error
	if !errors.As(err, &e) {
		t.Fatalf("expected an *apierror.Error, got %T (%v)", err, err)
	}
	out := make([]string, 0, len(e.Details))
	for _, d := range e.Details {
		out = append(out, d.RuleID)
	}
	return out
}

func hasRule(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestWeightsValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		weights  routing.Weights
		wantOK   bool
		wantRule string
	}{
		{
			name:    "documented defaults are valid despite binary float representation",
			weights: routing.DefaultWeights(),
			wantOK:  true,
		},
		{
			name:    "an all-in-one-dimension split still sums to one",
			weights: routing.Weights{Health: 1.0},
			wantOK:  true,
		},
		{
			name:     "weights that sum above one are rejected",
			weights:  routing.Weights{Health: 0.5, SuccessRate: 0.3, Cost: 0.2, Latency: 0.1},
			wantRule: "L4.ROUTING_WEIGHTS_SUM_TO_ONE",
		},
		{
			name:     "weights that sum below one are rejected",
			weights:  routing.Weights{Health: 0.1, SuccessRate: 0.1, Cost: 0.1, Latency: 0.1},
			wantRule: "L4.ROUTING_WEIGHTS_SUM_TO_ONE",
		},
		{
			name:     "a negative weight inverts its dimension and is rejected",
			weights:  routing.Weights{Health: 1.2, SuccessRate: -0.2, Cost: 0.0, Latency: 0.0},
			wantRule: "L4.ROUTING_WEIGHTS_NON_NEGATIVE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.weights.Validate()
			if tt.wantOK {
				if err != nil {
					t.Fatalf("expected valid weights, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			if got := ruleIDs(t, err); !hasRule(got, tt.wantRule) {
				t.Fatalf("expected rule %s, got %v", tt.wantRule, got)
			}
		})
	}
}

func TestConditionMatches(t *testing.T) {
	t.Parallel()

	base := routing.RequestContext{
		Amount:        usd(8450),
		PaymentMethod: shared.MethodCard,
		PayerCountry:  shared.Country("US"),
		RiskBand:      routing.RiskBandLow,
	}

	tests := []struct {
		name string
		cond routing.Condition
		want bool
	}{
		{"an empty condition matches everything", routing.Condition{}, true},
		{"currency matches", routing.Condition{Currency: "USD"}, true},
		{"currency mismatches", routing.Condition{Currency: "EUR"}, false},
		{"method matches", routing.Condition{PaymentMethod: shared.MethodCard}, true},
		{"method mismatches", routing.Condition{PaymentMethod: shared.MethodPayPal}, false},
		{"country matches", routing.Condition{Country: shared.Country("US")}, true},
		{"country mismatches", routing.Condition{Country: shared.Country("DE")}, false},
		{"risk band matches", routing.Condition{RiskBand: routing.RiskBandLow}, true},
		{"risk band mismatches", routing.Condition{RiskBand: routing.RiskBandHigh}, false},
		{"amount above a lower bound", routing.Condition{AmountAbove: usdPtr(5000)}, true},
		{"amount not above a higher bound", routing.Condition{AmountAbove: usdPtr(9000)}, false},
		{"amount below a higher bound", routing.Condition{AmountBelow: usdPtr(9000)}, true},
		{"amount not below a lower bound", routing.Condition{AmountBelow: usdPtr(5000)}, false},
		{"amount inside a band", routing.Condition{AmountAbove: usdPtr(5000), AmountBelow: usdPtr(9000)}, true},
		{
			name: "all fields must agree, not just one",
			cond: routing.Condition{Currency: "USD", PaymentMethod: shared.MethodPayPal},
			want: false,
		},
		{
			name: "an amount bound in another currency never matches rather than comparing across currencies",
			cond: routing.Condition{AmountAbove: func() *money.Money { m := money.MustNew(1, "EUR"); return &m }()},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.cond.Matches(base); got != tt.want {
				t.Fatalf("Matches = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPolicyResolveForPrefersFirstMatchingRule(t *testing.T) {
	t.Parallel()

	p := routing.Policy{
		Strategy:  routing.StrategyPriorityWithFallback,
		Primary:   "stripe",
		Fallbacks: []shared.GatewayID{"adyen"},
		Rules: []routing.Rule{
			{ID: "eur-cards", Condition: routing.Condition{Currency: "EUR"},
				Action: routing.Action{Primary: "adyen", Fallbacks: []shared.GatewayID{"stripe"}}},
			{ID: "eur-cards-again", Condition: routing.Condition{Currency: "EUR"},
				Action: routing.Action{Primary: "paypal"}},
		},
	}

	eur := routing.RequestContext{Amount: money.MustNew(1000, "EUR"), PaymentMethod: shared.MethodCard}
	action, ruleID := p.ResolveFor(eur)
	if ruleID != "eur-cards" {
		t.Fatalf("expected the first matching rule to win, got %q", ruleID)
	}
	if action.Primary != "adyen" {
		t.Fatalf("expected primary adyen, got %s", action.Primary)
	}

	dollars := routing.RequestContext{Amount: usd(1000), PaymentMethod: shared.MethodCard}
	action, ruleID = p.ResolveFor(dollars)
	if ruleID != "" {
		t.Fatalf("expected no rule to match, got %q", ruleID)
	}
	if action.Primary != "stripe" {
		t.Fatalf("expected the top-level primary, got %s", action.Primary)
	}
}

func TestPolicyValidate(t *testing.T) {
	// Verifies: FR-50.
	t.Parallel()

	valid := func() routing.Policy {
		return routing.Policy{
			Strategy:          routing.StrategyPriorityWithFallback,
			Primary:           "stripe",
			Fallbacks:         []shared.GatewayID{"adyen"},
			Weights:           routing.DefaultWeights(),
			ConnectedGateways: []shared.GatewayID{"stripe", "adyen", "paypal"},
		}
	}

	tests := []struct {
		name     string
		mutate   func(*routing.Policy)
		wantOK   bool
		wantRule string
	}{
		{
			name:   "the documented baseline configuration validates",
			mutate: func(*routing.Policy) {},
			wantOK: true,
		},
		{
			name:   "omitted weights are legal and mean the platform defaults",
			mutate: func(p *routing.Policy) { p.Weights = routing.Weights{} },
			wantOK: true,
		},
		{
			name:     "an unknown strategy is rejected rather than silently defaulted",
			mutate:   func(p *routing.Policy) { p.Strategy = "WEIGHTED-SCORE" },
			wantRule: "L4.ROUTING_STRATEGY_IS_KNOWN",
		},
		{
			name:     "weights that do not sum to one are rejected",
			mutate:   func(p *routing.Policy) { p.Weights = routing.Weights{Health: 0.9, Cost: 0.9} },
			wantRule: "L4.ROUTING_WEIGHTS_SUM_TO_ONE",
		},
		{
			name:     "a primary the merchant has no connection to is rejected",
			mutate:   func(p *routing.Policy) { p.Primary = "worldpay" },
			wantRule: "L4.ROUTING_PRIMARY_IS_CONNECTED",
		},
		{
			name:     "a fallback the merchant has no connection to is rejected",
			mutate:   func(p *routing.Policy) { p.Fallbacks = []shared.GatewayID{"worldpay"} },
			wantRule: "L4.ROUTING_FALLBACKS_ARE_CONNECTED",
		},
		{
			name:     "an empty fallback chain under a failover strategy is rejected",
			mutate:   func(p *routing.Policy) { p.Fallbacks = nil },
			wantRule: "L4.ROUTING_HAS_AT_LEAST_ONE_FALLBACK",
		},
		{
			name: "PINNED with fallbacks declares a chain that can never be walked",
			mutate: func(p *routing.Policy) {
				p.Strategy = routing.StrategyPinned
			},
			wantRule: "L4.ROUTING_RULES_ARE_REACHABLE",
		},
		{
			name:     "the primary repeated as a fallback wastes a failover attempt",
			mutate:   func(p *routing.Policy) { p.Fallbacks = []shared.GatewayID{"stripe"} },
			wantRule: "L4.ROUTING_FALLBACK_EXCLUDES_PRIMARY",
		},
		{
			name:     "a duplicated fallback spends two failover attempts on one gateway",
			mutate:   func(p *routing.Policy) { p.Fallbacks = []shared.GatewayID{"adyen", "adyen"} },
			wantRule: "L4.FALLBACK_DISTINCT",
		},
		{
			name: "a rule referencing an unknown gateway is rejected",
			mutate: func(p *routing.Policy) {
				p.Rules = []routing.Rule{{ID: "r1", Action: routing.Action{Primary: "worldpay"}}}
			},
			wantRule: "L4.ROUTING_PRIMARY_IS_CONNECTED",
		},
		{
			name: "a rule whose amount bounds cross can never match and is caught",
			mutate: func(p *routing.Policy) {
				p.Rules = []routing.Rule{{
					ID:        "big-but-small",
					Condition: routing.Condition{AmountAbove: usdPtr(50000), AmountBelow: usdPtr(10000)},
					Action:    routing.Action{Primary: "adyen"},
				}}
			},
			wantRule: "L4.ROUTING_RULES_ARE_REACHABLE",
		},
		{
			name: "equal amount bounds are also unsatisfiable because both bounds are strict",
			mutate: func(p *routing.Policy) {
				p.Rules = []routing.Rule{{
					ID:        "exactly",
					Condition: routing.Condition{AmountAbove: usdPtr(10000), AmountBelow: usdPtr(10000)},
					Action:    routing.Action{Primary: "adyen"},
				}}
			},
			wantRule: "L4.ROUTING_RULES_ARE_REACHABLE",
		},
		{
			name: "amount bounds in different currencies can never both be satisfied",
			mutate: func(p *routing.Policy) {
				eur := money.MustNew(10000, "EUR")
				p.Rules = []routing.Rule{{
					ID:        "mixed",
					Condition: routing.Condition{AmountAbove: usdPtr(100), AmountBelow: &eur},
					Action:    routing.Action{Primary: "adyen"},
				}}
			},
			wantRule: "L4.ROUTING_RULES_ARE_REACHABLE",
		},
		{
			name: "a rule shadowed by an earlier unconditional rule is dead code",
			mutate: func(p *routing.Policy) {
				p.Rules = []routing.Rule{
					{ID: "catch-all", Action: routing.Action{Primary: "adyen"}},
					{ID: "never-runs", Condition: routing.Condition{Currency: "EUR"},
						Action: routing.Action{Primary: "paypal"}},
				}
			},
			wantRule: "L4.ROUTING_RULES_ARE_REACHABLE",
		},
		{
			name: "a rule matching an unknown currency is rejected",
			mutate: func(p *routing.Policy) {
				p.Rules = []routing.Rule{{
					ID:        "moon-dollars",
					Condition: routing.Condition{Currency: "XMD"},
					Action:    routing.Action{Primary: "adyen"},
				}}
			},
			wantRule: "L4.ROUTING_RULE_MATCHER_VALUES_VALID",
		},
		{
			name: "a rule matching an unknown country is rejected",
			mutate: func(p *routing.Policy) {
				p.Rules = []routing.Rule{{
					ID:        "atlantis",
					Condition: routing.Condition{Country: shared.Country("ZZ")},
					Action:    routing.Action{Primary: "adyen"},
				}}
			},
			wantRule: "L4.ROUTING_RULE_MATCHER_VALUES_VALID",
		},
		{
			name: "a rule matching an unknown risk band is rejected",
			mutate: func(p *routing.Policy) {
				p.Rules = []routing.Rule{{
					ID:        "spicy",
					Condition: routing.Condition{RiskBand: routing.RiskBand("EXTREME")},
					Action:    routing.Action{Primary: "adyen"},
				}}
			},
			wantRule: "L4.ROUTING_RULE_MATCHER_VALUES_VALID",
		},
		{
			name:     "a missing primary is rejected",
			mutate:   func(p *routing.Policy) { p.Primary = "" },
			wantRule: "L4.ROUTING_PRIMARY_IS_CONNECTED",
		},
		{
			name: "more than sixty-four rules exceeds the latency budget",
			mutate: func(p *routing.Policy) {
				for i := 0; i < 65; i++ {
					p.Rules = append(p.Rules, routing.Rule{
						ID:        "r",
						Condition: routing.Condition{Currency: "EUR"},
						Action:    routing.Action{Primary: "adyen"},
					})
				}
			},
			wantRule: "L4.ROUTING_RULES_WITHIN_SIZE_BUDGET",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := valid()
			tt.mutate(&p)
			err := p.Validate()
			if tt.wantOK {
				if err != nil {
					t.Fatalf("expected a valid policy, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			if code := apierror.CodeOf(err); code != apierror.CodeConfigurationInvalid {
				t.Fatalf("expected CONFIGURATION_INVALID, got %s", code)
			}
			if got := ruleIDs(t, err); !hasRule(got, tt.wantRule) {
				t.Fatalf("expected rule %s, got %v", tt.wantRule, got)
			}
		})
	}
}

// A configuration publish is a batch operation against a form or a file. Reporting one problem
// per round-trip turns a five-mistake document into five failed publishes, so Validate must
// report every problem at once.
func TestPolicyValidateReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()

	p := routing.Policy{
		Strategy:          "NOPE",
		Primary:           "worldpay",
		Fallbacks:         []shared.GatewayID{"checkout", "checkout"},
		Weights:           routing.Weights{Health: 2},
		ConnectedGateways: []shared.GatewayID{"stripe", "adyen"},
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("expected a validation error")
	}
	got := ruleIDs(t, err)
	for _, want := range []string{
		"L4.ROUTING_STRATEGY_IS_KNOWN",
		"L4.ROUTING_WEIGHTS_SUM_TO_ONE",
		"L4.ROUTING_PRIMARY_IS_CONNECTED",
		"L4.ROUTING_FALLBACKS_ARE_CONNECTED",
		"L4.FALLBACK_DISTINCT",
	} {
		if !hasRule(got, want) {
			t.Errorf("expected rule %s in %v", want, got)
		}
	}
}

func TestPolicyReferencedGatewaysIsSortedAndDeduplicated(t *testing.T) {
	t.Parallel()

	p := routing.Policy{
		Primary:   "stripe",
		Fallbacks: []shared.GatewayID{"adyen", "paypal"},
		Rules: []routing.Rule{
			{ID: "r", Action: routing.Action{Primary: "adyen", Fallbacks: []shared.GatewayID{"stripe"}}},
		},
	}
	got := p.ReferencedGateways()
	want := []shared.GatewayID{"adyen", "paypal", "stripe"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestStrategyAndRiskBandValidity(t *testing.T) {
	t.Parallel()

	for _, s := range routing.AllStrategies {
		if !s.IsValid() {
			t.Errorf("%s should be valid", s)
		}
	}
	if routing.Strategy("MADE_UP").IsValid() {
		t.Error("an unregistered strategy must not validate")
	}
	if !routing.StrategyPriorityWithFallback.RequiresFallbackChain() {
		t.Error("PRIORITY_WITH_FALLBACK requires a fallback chain")
	}
	if routing.StrategyPinned.RequiresFallbackChain() {
		t.Error("PINNED never fails over and must not require a chain")
	}
	for _, b := range []routing.RiskBand{routing.RiskBandLow, routing.RiskBandMedium, routing.RiskBandHigh} {
		if !b.IsValid() {
			t.Errorf("%s should be a valid band", b)
		}
	}
	if routing.RiskBand("").IsValid() {
		t.Error("the empty band is the unset marker, not a valid band")
	}
}
