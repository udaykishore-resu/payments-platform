// Package routing is the smart-routing bounded context's domain model (BC-7).
//
// It answers exactly one question — *which gateway should this payment go to, and in what
// order should the alternatives be tried* — and it answers it as a pure function. There is no
// I/O here, no clock, no gateway SDK and, deliberately, no import of the gateway registry: the
// application layer resolves a gateway's capabilities, health and observed performance into the
// flat Candidate value declared in this package (convention 13, consumer-declared interfaces
// applied to a data shape), and hands the domain a slice of them.
//
// That inversion is the reason this package can be tested exhaustively. A routing decision is
// one of the few places in a payment platform where "we cannot reproduce what it did six months
// ago" is a commercial problem rather than an engineering annoyance: merchants dispute routing,
// gateways dispute volume commitments, and auditors ask why a payment went where it went. A
// scoring function that reaches out to a health store cannot be replayed. This one can.
//
// See docs/data-plane.md §4 and docs/spec/00-design-baseline.md §23.
package routing

import (
	"sort"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Strategy selects how the surviving candidates are ordered.
//
// The four strategies exist because merchants genuinely want different things and expressing
// all of them through one weighted score does not work: a merchant with a negotiated rate at
// one acquirer wants PINNED and will not accept "the score said otherwise", and a merchant
// under a volume commitment wants PRIORITY_WITH_FALLBACK. The scoring machinery still runs
// under every strategy — the scores are recorded on the plan either way — but only
// WEIGHTED_SCORE lets the score decide the order. Recording the scores under a strategy that
// ignores them is what makes "your pinned gateway was the worst of the three all quarter" a
// provable statement rather than an opinion.
type Strategy string

const (
	// StrategyPriorityWithFallback tries the merchant's declared primary first and walks the
	// declared fallback chain on failover. The default, and the only strategy whose behaviour
	// a merchant can predict without reading a scoring table.
	StrategyPriorityWithFallback Strategy = "PRIORITY_WITH_FALLBACK"

	// StrategyWeightedScore orders purely by the weighted score. Highest expected success for
	// the merchant, least predictable for the merchant's support team.
	StrategyWeightedScore Strategy = "WEIGHTED_SCORE"

	// StrategyLeastCost orders by effective cost for this specific amount, breaking ties on
	// health. It is not "cheapest wins unconditionally" — the hard filters still apply, so an
	// unhealthy or uncertified gateway is never reachable no matter how cheap it is.
	StrategyLeastCost Strategy = "LEAST_COST"

	// StrategyPinned sends everything to one gateway with no failover. Used during a migration,
	// a certification window, or under a contractual exclusivity clause. It is the only
	// strategy that can legitimately produce a one-entry plan, and it is the strategy most
	// likely to produce a 503, which is why L4 validation makes the operator declare it
	// explicitly rather than arriving at it by leaving the fallback list empty.
	StrategyPinned Strategy = "PINNED"
)

// AllStrategies is the complete strategy universe, used for configuration validation and for
// generating the OpenAPI enum.
var AllStrategies = []Strategy{
	StrategyLeastCost, StrategyPinned, StrategyPriorityWithFallback, StrategyWeightedScore,
}

// IsValid reports whether s is a strategy this binary understands. A configuration carrying an
// unknown strategy must be rejected at publish time rather than silently defaulted: silently
// defaulting a typo'd "WEIGHTED-SCORE" to PRIORITY_WITH_FALLBACK routes a merchant's traffic
// somewhere they did not ask for and gives them no signal that it happened.
func (s Strategy) IsValid() bool {
	switch s {
	case StrategyPriorityWithFallback, StrategyWeightedScore, StrategyLeastCost, StrategyPinned:
		return true
	default:
		return false
	}
}

// String satisfies fmt.Stringer.
func (s Strategy) String() string { return string(s) }

// RequiresFallbackChain reports whether the strategy is meaningless without at least one
// declared fallback. PRIORITY_WITH_FALLBACK with an empty fallback list is a configuration that
// says "fail over" and then cannot; catching it at L4 turns a 3am 503 into a rejected config
// publish.
func (s Strategy) RequiresFallbackChain() bool { return s == StrategyPriorityWithFallback }

// RiskBand is the coarse risk classification a routing rule can match on.
//
// It is defined here rather than imported from internal/domain/risk on purpose. The risk
// engine's output is an outcome plus a 0–100 score; the band is a three-valued *routing input*
// that the application layer derives from that score. Coupling the routing configuration schema
// to the risk decision type would make every change to the risk model a routing-configuration
// migration, and the two evolve on completely different schedules.
type RiskBand string

const (
	// RiskBandLow is the ordinary case.
	RiskBandLow RiskBand = "LOW"
	// RiskBandMedium is elevated but not blocking.
	RiskBandMedium RiskBand = "MEDIUM"
	// RiskBandHigh is the band a merchant typically routes to a gateway with stronger 3DS or
	// better chargeback representment.
	RiskBandHigh RiskBand = "HIGH"
)

// IsValid reports whether b is a known band. The empty band is the "unset" marker on a rule
// condition and is handled by the caller, not here.
func (b RiskBand) IsValid() bool {
	return b == RiskBandLow || b == RiskBandMedium || b == RiskBandHigh
}

// String satisfies fmt.Stringer.
func (b RiskBand) String() string { return string(b) }

// Weights are the relative importance of the four scoring dimensions.
//
// They must sum to 1.0 so that a score is always on [0, 1] and therefore comparable across
// merchants, across time, and against the tie tolerance. Weights that sum to 1.3 still rank
// candidates correctly *within one decision* but make every recorded score incomparable with
// every other, which destroys the only thing the persisted plan is for.
type Weights struct {
	Health      float64
	SuccessRate float64
	Cost        float64
	Latency     float64
}

// weightEpsilon is the tolerance on the sum-to-one check. It exists because 0.4 + 0.3 + 0.2 +
// 0.1 is not exactly 1.0 in binary floating point — the documented default configuration would
// fail its own validation with an exact comparison.
const weightEpsilon = 1e-9

// DefaultWeights are the platform defaults from docs/data-plane.md §4.3: health 0.4,
// successRate 0.3, cost 0.2, latency 0.1.
//
// Health dominates because a failing gateway costs 100% of the transactions it touches, while a
// 40bps cost difference costs 0.4%. Latency is weighted lowest because within the 8s dispatch
// budget a slower gateway that approves is worth strictly more than a fast one that declines.
func DefaultWeights() Weights {
	return Weights{Health: 0.4, SuccessRate: 0.3, Cost: 0.2, Latency: 0.1}
}

// IsZero reports whether no weights were configured, so the caller can substitute the defaults
// rather than score every candidate at zero. A zero-weight Weights is indistinguishable from
// "the operator left the block out of the document", and treating that as "everything scores 0"
// would make the tie-break the entire decision.
func (w Weights) IsZero() bool {
	return w.Health == 0 && w.SuccessRate == 0 && w.Cost == 0 && w.Latency == 0
}

// Sum returns the total weight.
func (w Weights) Sum() float64 { return w.Health + w.SuccessRate + w.Cost + w.Latency }

// Validate reports whether the weights are usable, as a CONFIGURATION_INVALID error carrying
// the specific rule IDs from the validation plane.
func (w Weights) Validate() error {
	details := w.validationDetails()
	if len(details) == 0 {
		return nil
	}
	return apierror.New(apierror.CodeConfigurationInvalid,
		"routing weights are not valid").WithDetails(details...)
}

func (w Weights) validationDetails() []apierror.Detail {
	var details []apierror.Detail
	neg := func(field string, v float64) {
		if v < 0 {
			details = append(details, apierror.Detail{
				Field:   "routing.weights." + field,
				Code:    "NEGATIVE_WEIGHT",
				Message: "a scoring weight may not be negative; a negative weight inverts the dimension it names",
				RuleID:  "L4.ROUTING_WEIGHTS_NON_NEGATIVE",
			})
		}
	}
	neg("health", w.Health)
	neg("successRate", w.SuccessRate)
	neg("cost", w.Cost)
	neg("latency", w.Latency)

	if diff := w.Sum() - 1.0; diff > weightEpsilon || diff < -weightEpsilon {
		details = append(details, apierror.Detail{
			Field:   "routing.weights",
			Code:    "WEIGHTS_DO_NOT_SUM_TO_ONE",
			Message: "health + successRate + cost + latency must sum to 1.0 so that scores are comparable across decisions",
			RuleID:  "L4.ROUTING_WEIGHTS_SUM_TO_ONE",
		})
	}
	return details
}

// Condition is a routing rule's when-clause. Every field is optional and the populated fields
// are AND-combined; an entirely empty condition matches every request.
//
// The fields are the ones a merchant can reason about without knowing anything about the
// platform's internals. Deliberately absent: gateway health, score, or anything else the
// platform observes. A rule that says "when adyen is degraded, use stripe" is a rule that
// duplicates — and then contradicts — the scoring engine, and the contradiction only shows up
// under load.
type Condition struct {
	// Currency matches the payment currency exactly. Empty means "any".
	Currency money.Currency
	// PaymentMethod matches the tender type exactly. Empty means "any".
	PaymentMethod shared.PaymentMethod
	// Country matches the payer's country exactly. Empty means "any".
	Country shared.Country
	// AmountAbove matches when the payment amount is strictly greater. Nil means "no floor".
	// A pointer rather than a zero value because "above zero" and "no floor" are different
	// rules and money.Money's zero value is a valid amount in an invalid currency.
	AmountAbove *money.Money
	// AmountBelow matches when the payment amount is strictly less. Nil means "no ceiling".
	AmountBelow *money.Money
	// RiskBand matches the band the risk engine's score fell into. Empty means "any".
	RiskBand RiskBand
}

// IsEmpty reports whether the condition constrains nothing and therefore matches every request.
// Used by Validate to catch the shadowing case: any rule after an unconditional one is dead
// code, and dead routing rules are how a merchant ends up convinced the platform is ignoring
// their configuration.
func (c Condition) IsEmpty() bool {
	return c.Currency == "" && c.PaymentMethod == "" && c.Country == "" &&
		c.AmountAbove == nil && c.AmountBelow == nil && c.RiskBand == ""
}

// Matches reports whether every populated field of the condition agrees with the request.
//
// A currency mismatch on an amount bound is treated as *not matching* rather than as an error.
// The alternative — comparing across currencies — would silently apply a "above USD 500" rule
// to a JPY 500 payment. L4 validation rejects such a rule at publish time; this is the runtime
// belt to that braces.
func (c Condition) Matches(req RequestContext) bool {
	if c.Currency != "" && c.Currency != req.Amount.Currency() {
		return false
	}
	if c.PaymentMethod != "" && c.PaymentMethod != req.PaymentMethod {
		return false
	}
	if c.Country != "" && c.Country != req.PayerCountry {
		return false
	}
	if c.RiskBand != "" && c.RiskBand != req.RiskBand {
		return false
	}
	if c.AmountAbove != nil {
		if c.AmountAbove.Currency() != req.Amount.Currency() {
			return false
		}
		if above, err := req.Amount.GreaterThan(*c.AmountAbove); err != nil || !above {
			return false
		}
	}
	if c.AmountBelow != nil {
		if c.AmountBelow.Currency() != req.Amount.Currency() {
			return false
		}
		if below, err := req.Amount.LessThan(*c.AmountBelow); err != nil || !below {
			return false
		}
	}
	return true
}

// Action is a routing rule's then-clause: the primary gateway and the ordered fallback chain to
// use when the condition matches.
type Action struct {
	Primary   shared.GatewayID
	Fallbacks []shared.GatewayID
}

// Chain returns the primary followed by the fallbacks, as a copy. This is the ordering
// PRIORITY_WITH_FALLBACK walks. Returning a copy matters: the Action is reachable from a Policy
// that the application layer caches for the lifetime of a configuration version, and a caller
// that sorts the returned slice in place would silently rewrite every subsequent decision.
func (a Action) Chain() []shared.GatewayID {
	out := make([]shared.GatewayID, 0, len(a.Fallbacks)+1)
	if !a.Primary.IsZero() {
		out = append(out, a.Primary)
	}
	out = append(out, a.Fallbacks...)
	return out
}

// Rule is one condition/action pair from the merchant's configuration document (§23).
type Rule struct {
	// ID is the operator-visible identifier of the rule, recorded on the plan when the rule
	// matches. Without it, "the plan says primary=adyen but my config says primary=stripe" has
	// no answer short of re-deriving the rule evaluation by hand.
	ID string
	// Condition is the when-clause.
	Condition Condition
	// Action is the then-clause.
	Action Action
}

// Matches reports whether the rule's condition holds for the request.
func (r Rule) Matches(req RequestContext) bool { return r.Condition.Matches(req) }

// maxRules bounds the rule list. Rules are evaluated linearly inside a 5ms routing budget
// (docs/data-plane.md §12 stage 12); an unbounded list is a way for a merchant to turn their
// own configuration into a latency incident on the money path.
const maxRules = 64

// Policy is the merchant's routing configuration as a domain value.
//
// It is a value, not an aggregate: it has no identity of its own and no lifecycle. Its identity
// and lifecycle belong to the configuration document that contains it (BC-3), which is
// versioned, audited and published as a whole. Modelling it as an aggregate here would give it
// two owners.
type Policy struct {
	// Strategy selects how survivors are ordered.
	Strategy Strategy
	// Primary is the merchant's declared first choice, and under StrategyPinned it *is* the pin.
	Primary shared.GatewayID
	// Fallbacks is the ordered chain tried after the primary.
	Fallbacks []shared.GatewayID
	// Rules are evaluated in order; the first match wins and replaces Primary/Fallbacks.
	// First-match-wins rather than most-specific-wins because "most specific" requires a
	// specificity metric that operators consistently disagree with, whereas list order is
	// something they can see.
	Rules []Rule
	// Weights are the scoring weights. Zero means "use DefaultWeights".
	Weights Weights
	// ConnectedGateways is the set of gateways the merchant actually has a connection to. It is
	// carried on the policy so that Validate can be a pure function of the policy value: an L4
	// check that has to go and ask a repository which gateways exist is an L4 check that cannot
	// run at config-publish time in a unit test.
	ConnectedGateways []shared.GatewayID
}

// ResolveFor returns the Action that governs this request and the ID of the rule that produced
// it, or the empty string when no rule matched and the top-level primary/fallbacks apply.
func (p Policy) ResolveFor(req RequestContext) (Action, string) {
	for _, r := range p.Rules {
		if r.Matches(req) {
			return r.Action, r.ID
		}
	}
	return Action{Primary: p.Primary, Fallbacks: append([]shared.GatewayID(nil), p.Fallbacks...)}, ""
}

// EffectiveWeights returns the configured weights, substituting the platform defaults when none
// were configured.
func (p Policy) EffectiveWeights() Weights {
	if p.Weights.IsZero() {
		return DefaultWeights()
	}
	return p.Weights
}

// Validate is the L4 configuration-validation entry point for the routing block.
//
// It reports *every* problem it finds in one error rather than the first, because a
// configuration publish is a batch operation performed by a human against a form or a YAML
// file: returning one problem per round-trip turns a five-mistake document into five failed
// publishes. Each detail carries the validation-plane rule ID so the operator's error message
// links to documentation rather than to a stack trace.
//
// The checks that earn their place here are the silent ones — the misconfigurations that
// produce no error at publish time and no error at request time, just traffic quietly going
// somewhere the merchant did not intend:
//
//   - a rule whose amount bounds cross, which can never match and so silently leaves the
//     merchant on the default route they wrote the rule to escape;
//   - a rule that references a gateway the merchant has no connection to, which produces a 503
//     only for the subset of traffic the rule matches;
//   - a rule shadowed by an earlier unconditional rule;
//   - a fallback chain that is empty under a strategy whose entire purpose is to fail over.
func (p Policy) Validate() error {
	var details []apierror.Detail

	if !p.Strategy.IsValid() {
		details = append(details, apierror.Detail{
			Field:   "routing.strategy",
			Code:    "UNKNOWN_STRATEGY",
			Message: "strategy must be one of PRIORITY_WITH_FALLBACK, WEIGHTED_SCORE, LEAST_COST, PINNED",
			RuleID:  "L4.ROUTING_STRATEGY_IS_KNOWN",
		})
	}

	if !p.Weights.IsZero() {
		details = append(details, p.Weights.validationDetails()...)
	}

	connected := make(map[shared.GatewayID]struct{}, len(p.ConnectedGateways))
	for _, g := range p.ConnectedGateways {
		connected[g] = struct{}{}
	}
	knowsConnections := len(connected) > 0

	if p.Primary.IsZero() {
		details = append(details, apierror.Detail{
			Field:   "routing.primary",
			Code:    "MISSING_PRIMARY",
			Message: "a routing policy must name a primary gateway",
			RuleID:  "L4.ROUTING_PRIMARY_IS_CONNECTED",
		})
	} else if knowsConnections {
		if _, ok := connected[p.Primary]; !ok {
			details = append(details, apierror.Detail{
				Field:   "routing.primary",
				Code:    "GATEWAY_NOT_CONNECTED",
				Message: "primary gateway " + p.Primary.String() + " has no connection for this merchant",
				RuleID:  "L4.ROUTING_PRIMARY_IS_CONNECTED",
			})
		}
	}

	details = append(details, validateChain("routing.fallback", p.Primary, p.Fallbacks, connected, knowsConnections)...)

	if p.Strategy.RequiresFallbackChain() && len(p.Fallbacks) == 0 {
		details = append(details, apierror.Detail{
			Field: "routing.fallback",
			Code:  "EMPTY_FALLBACK_CHAIN",
			Message: "strategy " + p.Strategy.String() + " fails over, so it requires at least one fallback gateway; " +
				"use PINNED if a single gateway with no failover is what you intend",
			RuleID: "L4.ROUTING_HAS_AT_LEAST_ONE_FALLBACK",
		})
	}
	if p.Strategy == StrategyPinned && len(p.Fallbacks) > 0 {
		details = append(details, apierror.Detail{
			Field:   "routing.fallback",
			Code:    "FALLBACK_UNREACHABLE_UNDER_PINNED",
			Message: "strategy PINNED never fails over, so the declared fallbacks can never be used",
			RuleID:  "L4.ROUTING_RULES_ARE_REACHABLE",
		})
	}

	if len(p.Rules) > maxRules {
		details = append(details, apierror.Detail{
			Field:   "routing.rules",
			Code:    "TOO_MANY_RULES",
			Message: "a routing policy may declare at most 64 rules; they are evaluated linearly inside a 5ms budget",
			RuleID:  "L4.ROUTING_RULES_WITHIN_SIZE_BUDGET",
		})
	}

	unconditionalAt := -1
	for i, r := range p.Rules {
		field := "routing.rules[" + itoa(i) + "]"
		details = append(details, validateRule(field, r, connected, knowsConnections)...)

		if unconditionalAt >= 0 {
			details = append(details, apierror.Detail{
				Field:   field,
				Code:    "RULE_SHADOWED",
				Message: "this rule can never match: rule " + itoa(unconditionalAt) + " has an empty condition and matches every request",
				RuleID:  "L4.ROUTING_RULES_ARE_REACHABLE",
			})
		} else if r.Condition.IsEmpty() {
			unconditionalAt = i
		}
	}

	if len(details) == 0 {
		return nil
	}
	return apierror.New(apierror.CodeConfigurationInvalid,
		"the routing configuration is not valid").WithDetails(details...)
}

// validateRule checks one rule's matcher values, its reachability and its gateway references.
func validateRule(field string, r Rule, connected map[shared.GatewayID]struct{}, knowsConnections bool) []apierror.Detail {
	var details []apierror.Detail

	if r.Condition.Currency != "" && !r.Condition.Currency.IsSupported() {
		details = append(details, apierror.Detail{
			Field: field + ".when.currency", Code: "UNKNOWN_CURRENCY",
			Message: "must be a supported ISO 4217 code",
			RuleID:  "L4.ROUTING_RULE_MATCHER_VALUES_VALID",
		})
	}
	if r.Condition.PaymentMethod != "" && !r.Condition.PaymentMethod.IsValid() {
		details = append(details, apierror.Detail{
			Field: field + ".when.paymentMethod", Code: "UNKNOWN_PAYMENT_METHOD",
			Message: "must be one of the platform's supported payment methods",
			RuleID:  "L4.ROUTING_RULE_MATCHER_VALUES_VALID",
		})
	}
	if r.Condition.Country != "" && !r.Condition.Country.IsValid() {
		details = append(details, apierror.Detail{
			Field: field + ".when.country", Code: "UNKNOWN_COUNTRY",
			Message: "must be a valid ISO 3166-1 alpha-2 code",
			RuleID:  "L4.ROUTING_RULE_MATCHER_VALUES_VALID",
		})
	}
	if r.Condition.RiskBand != "" && !r.Condition.RiskBand.IsValid() {
		details = append(details, apierror.Detail{
			Field: field + ".when.riskBand", Code: "UNKNOWN_RISK_BAND",
			Message: "must be one of LOW, MEDIUM, HIGH",
			RuleID:  "L4.ROUTING_RULE_MATCHER_VALUES_VALID",
		})
	}

	// The unsatisfiable-bounds check. This is the one that matters most, because it is
	// completely silent at runtime: a rule with amountAbove 500 and amountBelow 100 never
	// matches, the merchant's traffic quietly stays on the default route, and nothing anywhere
	// says so. Note that equality is also unsatisfiable — both bounds are strict — so the
	// comparison is >= and not >.
	above, below := r.Condition.AmountAbove, r.Condition.AmountBelow
	if above != nil && below != nil {
		switch {
		case above.Currency() != below.Currency():
			details = append(details, apierror.Detail{
				Field: field + ".when", Code: "AMOUNT_BOUNDS_CURRENCY_MISMATCH",
				Message: "amountAbove is in " + above.Currency().String() + " and amountBelow is in " +
					below.Currency().String() + "; a rule whose bounds are in different currencies can never match",
				RuleID: "L4.ROUTING_RULES_ARE_REACHABLE",
			})
		default:
			if cmp, err := above.Cmp(*below); err == nil && cmp >= 0 {
				details = append(details, apierror.Detail{
					Field: field + ".when", Code: "AMOUNT_BOUNDS_UNSATISFIABLE",
					Message: "amountAbove (" + above.String() + ") is not below amountBelow (" + below.String() +
						"), so no amount can satisfy both bounds and this rule can never match",
					RuleID: "L4.ROUTING_RULES_ARE_REACHABLE",
				})
			}
		}
	}
	if above != nil && !above.IsValid() {
		details = append(details, apierror.Detail{
			Field: field + ".when.amountAbove", Code: "UNKNOWN_CURRENCY",
			Message: "must carry a supported ISO 4217 code",
			RuleID:  "L4.ROUTING_RULE_MATCHER_VALUES_VALID",
		})
	}
	if below != nil && !below.IsValid() {
		details = append(details, apierror.Detail{
			Field: field + ".when.amountBelow", Code: "UNKNOWN_CURRENCY",
			Message: "must carry a supported ISO 4217 code",
			RuleID:  "L4.ROUTING_RULE_MATCHER_VALUES_VALID",
		})
	}

	if r.Action.Primary.IsZero() {
		details = append(details, apierror.Detail{
			Field: field + ".then.primary", Code: "MISSING_PRIMARY",
			Message: "a routing rule must name a primary gateway",
			RuleID:  "L4.ROUTING_PRIMARY_IS_CONNECTED",
		})
	} else if knowsConnections {
		if _, ok := connected[r.Action.Primary]; !ok {
			details = append(details, apierror.Detail{
				Field: field + ".then.primary", Code: "GATEWAY_NOT_CONNECTED",
				Message: "gateway " + r.Action.Primary.String() + " has no connection for this merchant",
				RuleID:  "L4.ROUTING_PRIMARY_IS_CONNECTED",
			})
		}
	}
	details = append(details, validateChain(field+".then.fallback", r.Action.Primary, r.Action.Fallbacks, connected, knowsConnections)...)
	return details
}

// validateChain checks a fallback list for unknown gateways, duplicates, and inclusion of the
// primary. A duplicated fallback is not merely untidy: the failover budget is two attempts, and
// a chain of [adyen, adyen] spends both of them on the same gateway.
func validateChain(field string, primary shared.GatewayID, chain []shared.GatewayID, connected map[shared.GatewayID]struct{}, knowsConnections bool) []apierror.Detail {
	var details []apierror.Detail
	seen := make(map[shared.GatewayID]struct{}, len(chain))
	for i, g := range chain {
		at := field + "[" + itoa(i) + "]"
		if g.IsZero() {
			details = append(details, apierror.Detail{
				Field: at, Code: "EMPTY_GATEWAY",
				Message: "a fallback entry may not be empty",
				RuleID:  "L4.ROUTING_FALLBACKS_ARE_CONNECTED",
			})
			continue
		}
		if knowsConnections {
			if _, ok := connected[g]; !ok {
				details = append(details, apierror.Detail{
					Field: at, Code: "GATEWAY_NOT_CONNECTED",
					Message: "gateway " + g.String() + " has no connection for this merchant",
					RuleID:  "L4.ROUTING_FALLBACKS_ARE_CONNECTED",
				})
			}
		}
		if g == primary {
			details = append(details, apierror.Detail{
				Field: at, Code: "FALLBACK_IS_PRIMARY",
				Message: "gateway " + g.String() + " is already the primary; listing it as a fallback wastes a failover attempt",
				RuleID:  "L4.ROUTING_FALLBACK_EXCLUDES_PRIMARY",
			})
		}
		if _, dup := seen[g]; dup {
			details = append(details, apierror.Detail{
				Field: at, Code: "DUPLICATE_FALLBACK",
				Message: "gateway " + g.String() + " appears more than once in the fallback chain",
				RuleID:  "L4.FALLBACK_DISTINCT",
			})
		}
		seen[g] = struct{}{}
	}
	return details
}

// ReferencedGateways returns every gateway the policy names, sorted and de-duplicated. Used by
// the configuration layer to check the routing block against the merchant's connections and to
// warm capability descriptors before the first payment arrives.
func (p Policy) ReferencedGateways() []shared.GatewayID {
	set := make(map[shared.GatewayID]struct{})
	add := func(a Action) {
		if !a.Primary.IsZero() {
			set[a.Primary] = struct{}{}
		}
		for _, g := range a.Fallbacks {
			if !g.IsZero() {
				set[g] = struct{}{}
			}
		}
	}
	add(Action{Primary: p.Primary, Fallbacks: p.Fallbacks})
	for _, r := range p.Rules {
		add(r.Action)
	}
	out := make([]shared.GatewayID, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// itoa is a tiny local integer formatter. strconv would be a fine dependency; this exists so
// that the error-message construction in this file reads as one expression per detail rather
// than being interrupted by imports of a formatting package in a hot validation path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-13, FR-50.
//
// The merchant's routing policy — primary, fallback chain and conditional rules
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
