package l4config

import (
	"math"
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/validation/engine"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/internal/ruledef"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

func init() {
	ruledef.Register(defs(DefaultDeps()), "control-plane", "2026-01-01", engine.Enforce)
}

// Rules returns the L4 rule set.
//
// CollectAll, unconditionally. A configuration publish is the one place in the platform where
// a caller is holding a document with several independent problems in it, and the difference
// between reporting one and reporting all of them is the difference between a single review
// cycle and a morning.
func Rules(d Deps) engine.RuleSet[Subject] {
	return engine.RuleSet[Subject]{
		Name:  "L4.configuration",
		Mode:  engine.CollectAll,
		Rules: ruledef.Build(defs(d)),
	}
}

func defs(d Deps) []ruledef.Def[Subject] {
	hasRules := func(s Subject) bool { return len(s.Draft.Routing.Rules) > 0 }
	hasWeights := func(s Subject) bool { return len(s.Draft.Routing.Weights) > 0 }
	hasFallback := func(s Subject) bool { return len(s.Draft.Routing.Fallback) > 0 }
	hasEndpoints := func(s Subject) bool { return len(s.Draft.Webhooks) > 0 }
	hasSettlement := func(s Subject) bool { return s.Draft.Settlement.Present }
	invalidCfg := string(apierror.CodeConfigurationInvalid)

	return []ruledef.Def[Subject]{
		{
			ID: "L4.SCHEMA_VERSION_KNOWN", Severity: engine.Error,
			Code: invalidCfg, Field: "/schemaVersion", Pure: true,
			Desc:        "the document's schema version is one this platform version understands",
			Remediation: "Publish with a supported configuration schema version.",
			Check: func(s Subject) string {
				if containsString(d.SupportedSchemaVersions, s.Draft.SchemaVersion) {
					return ""
				}
				return "unsupported configuration schema version " + quote(s.Draft.SchemaVersion)
			},
		},
		{
			ID: "L4.VERSION_IS_SUCCESSOR", Severity: engine.Error,
			Code: string(apierror.CodeConfigurationVersionConflict), Field: "/version", Pure: true,
			Desc:        "the draft version is exactly one past the published version and the ETag matched",
			Remediation: "The configuration changed since you read it. Re-read and re-apply.",
			Applies:     func(s Subject) bool { return s.Previous.Present },
			Check: func(s Subject) string {
				if !s.ETagMatched {
					return "the If-Match precondition did not hold"
				}
				if s.Draft.Version != s.Previous.Version+1 {
					return "version " + itoa64(s.Draft.Version) + " does not succeed the published version " +
						itoa64(s.Previous.Version)
				}
				return ""
			},
		},
		{
			ID: "L4.ENVIRONMENT_MATCHES_MERCHANT_STATE", Severity: engine.Error,
			Code: invalidCfg, Field: "/environment", Pure: true,
			Desc:        "a production configuration is published only for a merchant at or past APPROVED",
			Remediation: "A production configuration cannot be published before approval.",
			Check: func(s Subject) string {
				if !s.Draft.Environment.IsProduction() {
					return ""
				}
				if merchantApproved(s.Merchant.Status) {
					return ""
				}
				return "merchant is " + string(s.Merchant.Status) +
					", which is before approval, so no production configuration may be published"
			},
		},
		{
			ID: "L4.CURRENCIES_NON_EMPTY", Severity: engine.Error,
			Code: invalidCfg, Field: "/supportedCurrencies", Pure: true,
			Desc:        "at least one currency is enabled",
			Remediation: "Enable at least one currency.",
			Check: func(s Subject) string {
				if len(s.Draft.SupportedCurrencies) >= 1 {
					return ""
				}
				return "no currency is enabled"
			},
		},
		{
			ID: "L4.CURRENCIES_ARE_ISO4217", Severity: engine.Error,
			Code: string(apierror.CodeCurrencyNotSupported), Field: "/supportedCurrencies", Pure: true,
			Desc:        "every enabled currency is in the ISO 4217 table",
			Remediation: "Use ISO 4217 alpha-3 currency codes.",
			Applies:     func(s Subject) bool { return len(s.Draft.SupportedCurrencies) > 0 },
			Check: func(s Subject) string {
				for _, c := range s.Draft.SupportedCurrencies {
					if !c.IsSupported() {
						return quote(string(c)) + " is not a valid currency code"
					}
				}
				return ""
			},
		},
		{
			ID: "L4.CURRENCIES_WITHIN_TENANT_ALLOWLIST", Severity: engine.Error,
			Code: invalidCfg, Field: "/supportedCurrencies", Pure: true,
			Desc:        "every enabled currency is on the tenant's allowlist",
			Remediation: "Remove currencies your platform has not enabled.",
			Applies:     func(s Subject) bool { return len(d.CurrencyAllowlist) > 0 },
			Check: func(s Subject) string {
				for _, c := range s.Draft.SupportedCurrencies {
					if !containsCurrency(d.CurrencyAllowlist, c) {
						return string(c) + " is not enabled for your platform"
					}
				}
				return ""
			},
		},
		{
			ID: "L4.METHODS_NON_EMPTY", Severity: engine.Error,
			Code: invalidCfg, Field: "/paymentMethods", Pure: true,
			Desc:        "at least one payment method is enabled",
			Remediation: "Enable at least one payment method.",
			Check: func(s Subject) string {
				if len(s.Draft.PaymentMethods) >= 1 {
					return ""
				}
				return "no payment method is enabled"
			},
		},
		{
			ID: "L4.METHODS_ARE_KNOWN", Severity: engine.Error,
			Code: string(apierror.CodePaymentMethodNotSupported), Field: "/paymentMethods", Pure: true,
			Desc:        "every enabled method is in the platform payment-method enum",
			Remediation: "Use a recognized payment method identifier.",
			Applies:     func(s Subject) bool { return len(s.Draft.PaymentMethods) > 0 },
			Check: func(s Subject) string {
				for _, m := range s.Draft.PaymentMethods {
					if !m.IsValid() {
						return quote(string(m)) + " is not a recognized payment method"
					}
				}
				return ""
			},
		},
		{
			ID: "L4.COUNTRIES_ARE_ISO3166", Severity: engine.Error,
			Code: invalidCfg, Field: "/countries", Pure: true,
			Desc:        "every enabled country is in the ISO 3166-1 alpha-2 table",
			Remediation: "Use ISO 3166-1 alpha-2 country codes.",
			Applies:     func(s Subject) bool { return len(s.Draft.Countries) > 0 },
			Check: func(s Subject) string {
				for _, c := range s.Draft.Countries {
					if !c.IsValid() {
						return quote(string(c)) + " is not a valid country code"
					}
				}
				return ""
			},
		},
		{
			ID: "L4.COUNTRIES_SUBSET_OF_MERCHANT_LICENSED", Severity: engine.Error,
			Code: invalidCfg, Field: "/countries", Pure: true,
			Desc:        "every enabled country is inside the merchant's L2-validated operating territory",
			Remediation: "Remove countries outside your validated operating territory.",
			Check: func(s Subject) string {
				for _, c := range s.Draft.Countries {
					if !containsCountry(s.Merchant.LicensedCountries, c) {
						return string(c) + " is not in your validated operating territory"
					}
				}
				return ""
			},
		},
		{
			ID: "L4.ROUTING_STRATEGY_IS_KNOWN", Severity: engine.Error,
			Code: invalidCfg, Field: "/routing/strategy", Pure: true,
			Desc:        "the routing strategy is one the router implements",
			Remediation: "Use a supported routing strategy.",
			Check: func(s Subject) string {
				if containsString(d.RoutingStrategies, s.Draft.Routing.Strategy) {
					return ""
				}
				return "unknown routing strategy " + quote(s.Draft.Routing.Strategy)
			},
		},
		{
			ID: "L4.ROUTING_PRIMARY_IS_CONNECTED", Severity: engine.Error,
			Code: invalidCfg, Field: "/routing/primary", Pure: true,
			Desc:        "the primary gateway has a certified connection for this merchant and environment",
			Remediation: "Name a gateway with a certified connection as your primary route.",
			Check: func(s Subject) string {
				if certified(s, s.Draft.Routing.Primary) {
					return ""
				}
				return quote(string(s.Draft.Routing.Primary)) +
					" is not a certified connection for this merchant"
			},
		},
		{
			ID: "L4.ROUTING_FALLBACKS_ARE_CONNECTED", Severity: engine.Error,
			Code: invalidCfg, Field: "/routing/fallback", Pure: true,
			Desc:        "every fallback gateway has a certified connection",
			Remediation: "Every fallback gateway must have a certified connection.",
			Applies:     hasFallback,
			Check: func(s Subject) string {
				for _, g := range s.Draft.Routing.Fallback {
					if !certified(s, g) {
						return "fallback " + quote(string(g)) + " is not certified"
					}
				}
				return ""
			},
		},
		{
			ID: "L4.ROUTING_FALLBACK_EXCLUDES_PRIMARY", Severity: engine.Error,
			Code: invalidCfg, Field: "/routing/fallback", Pure: true,
			Desc:        "the primary gateway does not also appear in the fallback list",
			Remediation: "The primary gateway must not also appear in the fallback list.",
			Applies:     hasFallback,
			Check: func(s Subject) string {
				for _, g := range s.Draft.Routing.Fallback {
					if g == s.Draft.Routing.Primary {
						// A primary that is also its own fallback means a failover that retries
						// the gateway that just failed, which is indistinguishable from no
						// failover at all except that it doubles the latency.
						return "primary " + string(g) + " also appears in the fallback list"
					}
				}
				return ""
			},
		},
		{
			ID: "L4.ROUTING_HAS_AT_LEAST_ONE_FALLBACK", Severity: engine.Warning,
			Code: "", Field: "/routing/fallback", Pure: true,
			Desc:        "a priority-with-fallback strategy declares at least one fallback",
			Remediation: "With no fallback, a single gateway outage stops all payments.",
			Applies:     func(s Subject) bool { return s.Draft.Routing.Strategy == "PRIORITY_WITH_FALLBACK" },
			Check: func(s Subject) string {
				if len(s.Draft.Routing.Fallback) >= 1 {
					return ""
				}
				return "no fallback gateway is configured"
			},
		},
		{
			ID: "L4.ROUTING_RULE_PREDICATE_FIELDS_KNOWN", Severity: engine.Error,
			Code: invalidCfg, Field: "/routing/rules", Pure: true,
			Desc:        "every `when` key is in the closed predicate-field enum",
			Remediation: "Routing predicates may only use the closed set of predicate fields.",
			Applies:     hasRules,
			Check: func(s Subject) string {
				for i, r := range s.Draft.Routing.Rules {
					for field := range r.When {
						if !containsString(d.PredicateFields, field) {
							return "rule " + itoa(i+1) + ": " + quote(field) +
								" is not a routing predicate field. Allowed: " +
								strings.Join(d.PredicateFields, ", ")
						}
					}
				}
				return ""
			},
		},
		{
			ID: "L4.ROUTING_RULE_MATCHER_VALUES_VALID", Severity: engine.Error,
			Code: invalidCfg, Field: "/routing/rules", Pure: true,
			Desc:        "each matcher's operator is known and its values are type-correct and in domain",
			Remediation: "Correct the routing rule matcher: its operator or its values are not valid for that field.",
			Applies:     hasRules,
			Check: func(s Subject) string {
				for i, r := range s.Draft.Routing.Rules {
					for field, m := range r.When {
						if msg := matcherProblem(d, field, m); msg != "" {
							return "rule " + itoa(i+1) + ": " + field + " " + msg
						}
					}
				}
				return ""
			},
		},
		{
			ID: "L4.ROUTING_RULES_WITHIN_SIZE_BUDGET", Severity: engine.Error,
			Code: invalidCfg, Field: "/routing/rules", Pure: true,
			Desc:        "the rule count, predicates per rule and values per `in` are within the compiled-table budget",
			Remediation: "Routing rules exceed the size budget. Consolidate them.",
			Applies:     hasRules,
			Check: func(s Subject) string {
				maxRules := defaultInt(d.MaxRoutingRules, 64)
				maxPreds := defaultInt(d.MaxPredicatesPerRule, 16)
				maxVals := defaultInt(d.MaxValuesPerIn, 256)
				if len(s.Draft.Routing.Rules) > maxRules {
					return "the document declares " + itoa(len(s.Draft.Routing.Rules)) +
						" routing rules; the limit is " + itoa(maxRules)
				}
				for i, r := range s.Draft.Routing.Rules {
					if len(r.When) > maxPreds {
						return "rule " + itoa(i+1) + " has " + itoa(len(r.When)) +
							" predicates; the limit is " + itoa(maxPreds)
					}
					for field, m := range r.When {
						if m.Op == "in" && len(m.Values) > maxVals {
							return "rule " + itoa(i+1) + ": " + field + " lists " +
								itoa(len(m.Values)) + " values; the limit is " + itoa(maxVals)
						}
					}
				}
				return ""
			},
		},
		{
			ID: "L4.ROUTING_RULES_ARE_REACHABLE", Severity: engine.Warning,
			Code: "", Field: "/routing/rules", Pure: true,
			Desc:        "no rule is fully shadowed by an earlier rule over the field domains",
			Remediation: "A routing rule can never match because an earlier rule already covers it; remove or reorder it.",
			Applies:     func(s Subject) bool { return len(s.Draft.Routing.Rules) >= 2 },
			Check: func(s Subject) string {
				rules := s.Draft.Routing.Rules
				for n := 1; n < len(rules); n++ {
					for m := 0; m < n; m++ {
						if shadows(rules[m], rules[n]) {
							return "rule " + itoa(n+1) + " can never match; rule " + itoa(m+1) +
								" already covers it"
						}
					}
				}
				return ""
			},
		},
		{
			ID: "L4.ROUTING_WEIGHTS_NON_NEGATIVE", Severity: engine.Error,
			Code: invalidCfg, Field: "/routing/weights", Pure: true,
			Desc:        "every routing weight is at least zero",
			Remediation: "Routing weights must not be negative.",
			Applies:     hasWeights,
			Check: func(s Subject) string {
				for k, w := range s.Draft.Routing.Weights {
					if w < 0 || math.IsNaN(w) {
						return "weight " + quote(k) + " is negative or not a number"
					}
				}
				return ""
			},
		},
		{
			ID: "L4.ROUTING_WEIGHTS_SUM_TO_ONE", Severity: engine.Error,
			Code: invalidCfg, Field: "/routing/weights", Pure: true,
			Desc:        "the routing weights sum to 1.0 within 1e−6",
			Remediation: "Routing weights must sum to 1.0.",
			Applies:     hasWeights,
			Check: func(s Subject) string {
				sum := 0.0
				for _, w := range s.Draft.Routing.Weights {
					sum += w
				}
				if math.Abs(sum-1.0) <= 1e-6 {
					return ""
				}
				return "routing weights sum to " + formatFloat(sum) + ", not 1.0"
			},
		},
		{
			ID: "L4.EVERY_CURRENCY_METHOD_PAIR_ROUTABLE", Severity: engine.Error,
			Code: string(apierror.CodeNoEligibleGateway), Field: "/supportedCurrencies", Pure: true,
			Desc: "for every enabled (currency, method, country) triple at least one certified " +
				"connection's descriptor supports it",
			Remediation: "No certified gateway can process one of your enabled combinations. Add a gateway or remove the combination.",
			Applies: func(s Subject) bool {
				return len(s.Draft.SupportedCurrencies) > 0 && len(s.Draft.PaymentMethods) > 0
			},
			Check: func(s Subject) string {
				countries := s.Draft.Countries
				if len(countries) == 0 {
					countries = []shared.Country{""}
				}
				for _, cur := range s.Draft.SupportedCurrencies {
					for _, m := range s.Draft.PaymentMethods {
						for _, ctry := range countries {
							if !anyDescriptorSupports(s, cur, m, ctry) {
								return "no certified gateway can process " + string(m) + " in " +
									string(cur) + " from " + countryOrAny(ctry)
							}
						}
					}
				}
				return ""
			},
		},
		{
			ID: "L4.ROUTED_GATEWAY_SUPPORTS_ITS_PREDICATE", Severity: engine.Error,
			Code: invalidCfg, Field: "/routing/rules", Pure: true,
			Desc:        "each rule's target gateway supports everything its `when` clause can select",
			Remediation: "A routing rule sends traffic to a gateway that cannot process it. Change the target or narrow the predicate.",
			Applies:     hasRules,
			Check: func(s Subject) string {
				for i, r := range s.Draft.Routing.Rules {
					desc, ok := descriptorFor(s, r.Then.Primary)
					if !ok {
						return "rule " + itoa(i+1) + " routes to " + quote(string(r.Then.Primary)) +
							", which has no capability descriptor"
					}
					for _, cur := range selectedCurrencies(r, s.Draft.SupportedCurrencies) {
						if !containsCurrency(desc.Currencies, cur) {
							return "rule " + itoa(i+1) + " routes " + string(cur) + " to " +
								string(r.Then.Primary) + ", which does not support it"
						}
					}
					for _, m := range selectedMethods(r, s.Draft.PaymentMethods) {
						if !containsMethod(desc.Methods, m) {
							return "rule " + itoa(i+1) + " routes " + string(m) + " to " +
								string(r.Then.Primary) + ", which does not support it"
						}
					}
				}
				return ""
			},
		},
		{
			ID: "L4.RISK_LIMIT_CURRENCY_SUPPORTED", Severity: engine.Error,
			Code: invalidCfg, Field: "/risk", Pure: true,
			Desc: "each risk limit is denominated in an enabled currency or the tenant base " +
				"currency",
			Remediation: "State each risk limit in a currency this merchant has enabled.",
			Applies: func(s Subject) bool {
				return !s.Draft.Risk.MaxTransactionAmount.IsZero() ||
					!s.Draft.Risk.Require3DSAbove.IsZero() ||
					!s.Draft.Risk.DailyVolumeLimit.IsZero()
			},
			Check: func(s Subject) string {
				for name, m := range map[string]money.Money{
					"maxTransactionAmount": s.Draft.Risk.MaxTransactionAmount,
					"require3DSAbove":      s.Draft.Risk.Require3DSAbove,
					"dailyVolumeLimit":     s.Draft.Risk.DailyVolumeLimit,
				} {
					if m.IsZero() && m.Currency() == "" {
						continue
					}
					c := m.Currency()
					if containsCurrency(s.Draft.SupportedCurrencies, c) || c == d.TenantBaseCurrency {
						continue
					}
					return "limit currency " + quote(string(c)) + " on " + name +
						" is not enabled for this merchant"
				}
				return ""
			},
		},
		{
			ID: "L4.THREEDS_THRESHOLD_BELOW_MAX_AMOUNT", Severity: engine.Error,
			Code: invalidCfg, Field: "/risk/require3DSAbove", Pure: true,
			Desc:        "the 3-D Secure threshold is at or below the maximum transaction amount",
			Remediation: "Lower the 3-D Secure threshold below your maximum transaction amount, or it will never trigger.",
			Applies: func(s Subject) bool {
				return !s.Draft.Risk.Require3DSAbove.IsZero() && !s.Draft.Risk.MaxTransactionAmount.IsZero()
			},
			Check: func(s Subject) string {
				over, err := s.Draft.Risk.Require3DSAbove.GreaterThan(s.Draft.Risk.MaxTransactionAmount)
				if err != nil {
					return "the 3-D Secure threshold and the maximum transaction amount are in different currencies"
				}
				if over {
					return "the 3-D Secure threshold is above the maximum transaction amount, so 3-D Secure would never trigger"
				}
				return ""
			},
		},
		{
			ID: "L4.THREEDS_THRESHOLD_MEETS_SCA_FLOOR", Severity: engine.Error,
			Code: invalidCfg, Field: "/risk/require3DSAbove", Pure: true,
			Desc:        "the 3-D Secure threshold is at or below the regulatory SCA floor for an enabled EEA/UK corridor",
			Remediation: "PSD2 requires strong customer authentication above the low-value exemption ceiling. Lower the threshold.",
			Applies: func(s Subject) bool {
				if s.Draft.Risk.Require3DSAbove.IsZero() || d.SCAFloor.IsZero() {
					return false
				}
				for _, c := range s.Draft.Countries {
					if containsCountry(d.SCACorridors, c) || c.IsSCAJurisdiction() {
						return true
					}
				}
				return false
			},
			Check: func(s Subject) string {
				over, err := s.Draft.Risk.Require3DSAbove.GreaterThan(d.SCAFloor)
				if err != nil {
					return "the 3-D Secure threshold is not denominated in the SCA floor's currency"
				}
				if over {
					return "the 3-D Secure threshold is above the regulatory floor of " + d.SCAFloor.String()
				}
				return ""
			},
		},
		{
			ID: "L4.DAILY_LIMIT_AT_LEAST_MAX_TRANSACTION", Severity: engine.Error,
			Code: invalidCfg, Field: "/risk/dailyVolumeLimit", Pure: true,
			Desc:        "the daily volume limit is at least the maximum single transaction",
			Remediation: "Raise the daily volume limit to at least your maximum transaction amount.",
			Applies: func(s Subject) bool {
				return !s.Draft.Risk.DailyVolumeLimit.IsZero() && !s.Draft.Risk.MaxTransactionAmount.IsZero()
			},
			Check: func(s Subject) string {
				less, err := s.Draft.Risk.DailyVolumeLimit.LessThan(s.Draft.Risk.MaxTransactionAmount)
				if err != nil {
					return "the daily volume limit and the maximum transaction amount are in different currencies"
				}
				if less {
					return "a single maximum-size payment would exceed the daily limit"
				}
				return ""
			},
		},
		{
			ID: "L4.VELOCITY_LIMITS_POSITIVE", Severity: engine.Error,
			Code: invalidCfg, Field: "/velocity", Pure: true,
			Desc:        "every declared velocity limit is at least one",
			Remediation: "Velocity limits must be at least 1.",
			Applies: func(s Subject) bool {
				v := s.Draft.Velocity
				return v != Velocity{}
			},
			Check: func(s Subject) string {
				v := s.Draft.Velocity
				for name, n := range map[string]int{
					"maxPaymentsPerMinute":    v.MaxPaymentsPerMinute,
					"maxPerCardPerHour":       v.MaxPerCardPerHour,
					"maxPerCustomerPerDay":    v.MaxPerCustomerPerDay,
					"maxDistinctCardsPerHour": v.MaxDistinctCardsPerHr,
				} {
					if n < 0 || (n == 0 && name == "maxPaymentsPerMinute") {
						return name + " must be at least 1"
					}
				}
				return ""
			},
		},
		{
			ID: "L4.VELOCITY_CONSISTENT_WITH_VOLUME", Severity: engine.Warning,
			Code: "", Field: "/velocity/maxPaymentsPerMinute", Pure: true,
			Desc:        "the per-minute velocity limit can carry the declared daily volume",
			Remediation: "Your velocity limit is below your declared volume; legitimate traffic may be throttled.",
			Applies: func(s Subject) bool {
				return s.Draft.Velocity.MaxPaymentsPerMinute > 0 &&
					s.Merchant.ExpectedDailyPaymentCount > 0
			},
			Check: func(s Subject) string {
				capacity := s.Draft.Velocity.MaxPaymentsPerMinute * 60 * 24
				if capacity >= s.Merchant.ExpectedDailyPaymentCount {
					return ""
				}
				return "the velocity limit permits " + itoa(capacity) +
					" payments per day against a declared " + itoa(s.Merchant.ExpectedDailyPaymentCount)
			},
		},
		{
			ID: "L4.BLOCKED_COUNTRIES_DISJOINT", Severity: engine.Error,
			Code: invalidCfg, Field: "/risk/blockedCountries", Pure: true,
			Desc:        "no country appears in both the enabled and the blocked list",
			Remediation: "Remove the country from either the enabled list or the blocked list.",
			Applies: func(s Subject) bool {
				return len(s.Draft.Countries) > 0 && len(s.Draft.Risk.BlockedCountries) > 0
			},
			Check: func(s Subject) string {
				for _, c := range s.Draft.Countries {
					if containsCountry(s.Draft.Risk.BlockedCountries, c) {
						return string(c) + " appears in both enabled and blocked countries"
					}
				}
				return ""
			},
		},
		{
			ID: "L4.BLOCKED_COUNTRIES_INCLUDE_MANDATORY", Severity: engine.Error,
			Code: invalidCfg, Field: "/risk/blockedCountries", Pure: true,
			Desc:        "the blocked list contains the platform's mandatory sanctions set",
			Remediation: "A mandatory blocked country cannot be removed from your blocked list.",
			Check: func(s Subject) string {
				for _, c := range d.MandatoryBlockedCountries {
					if !containsCountry(s.Draft.Risk.BlockedCountries, c) {
						return string(c) + " must remain blocked; it cannot be removed"
					}
				}
				return ""
			},
		},
		{
			ID: "L4.REFUND_WINDOW_WITHIN_GATEWAY_MAX", Severity: engine.Error,
			Code: invalidCfg, Field: "/limits/maxRefundWindowDays", Pure: true,
			Desc:        "the configured refund window is within every routable gateway's window",
			Remediation: "Shorten the refund window to what every routable gateway supports.",
			Applies:     func(s Subject) bool { return s.Draft.Limits.MaxRefundWindowDays > 0 },
			Check: func(s Subject) string {
				for _, g := range routableGateways(s) {
					desc, ok := descriptorFor(s, g)
					if !ok {
						continue
					}
					if desc.RefundWindowDays < s.Draft.Limits.MaxRefundWindowDays {
						return string(g) + " allows refunds for only " + itoa(desc.RefundWindowDays) + " days"
					}
				}
				return ""
			},
		},
		{
			ID: "L4.MAX_PARTIAL_CAPTURES_SUPPORTED", Severity: engine.Error,
			Code: invalidCfg, Field: "/limits/maxPartialCaptures", Pure: true,
			Desc:        "every routable gateway supports at least the configured number of partial captures",
			Remediation: "Lower the partial-capture limit to what every routable gateway supports.",
			Applies:     func(s Subject) bool { return s.Draft.Limits.MaxPartialCaptures > 1 },
			Check: func(s Subject) string {
				for _, g := range routableGateways(s) {
					desc, ok := descriptorFor(s, g)
					if !ok {
						continue
					}
					if desc.MaxPartialCaptures < s.Draft.Limits.MaxPartialCaptures {
						return string(g) + " supports at most " + itoa(desc.MaxPartialCaptures) +
							" partial captures"
					}
				}
				return ""
			},
		},
		{
			ID: "L4.WEBHOOK_ENDPOINTS_HTTPS", Severity: engine.Error,
			Code: invalidCfg, Field: "/webhooks", Pure: true,
			Desc:        "each webhook endpoint is an https URL on a public host, within the length ceiling",
			Remediation: "Webhook endpoints must be public HTTPS URLs of at most 2048 characters.",
			Applies:     hasEndpoints,
			Check: func(s Subject) string {
				for i, w := range s.Draft.Webhooks {
					if len(w.URL) > 2048 {
						return "webhook " + itoa(i+1) + " URL is longer than 2048 characters"
					}
					if msg := publicHTTPSProblem(w.URL); msg != "" {
						return "webhook " + itoa(i+1) + ": " + msg
					}
				}
				return ""
			},
		},
		{
			ID: "L4.WEBHOOK_EVENT_PATTERNS_KNOWN", Severity: engine.Error,
			Code: invalidCfg, Field: "/webhooks", Pure: true,
			Desc:        "each event pattern matches at least one event type in the platform catalog",
			Remediation: "Every webhook event pattern must match at least one known event type.",
			Applies:     hasEndpoints,
			Check: func(s Subject) string {
				for i, w := range s.Draft.Webhooks {
					for _, p := range w.EventPatterns {
						if !patternMatchesAny(p, d.KnownEventTypes) {
							return "webhook " + itoa(i+1) + ": pattern " + quote(p) +
								" matches no known event type"
						}
					}
				}
				return ""
			},
		},
		{
			ID: "L4.WEBHOOK_RETRY_POLICY_WITHIN_BOUNDS", Severity: engine.Error,
			Code: invalidCfg, Field: "/webhooks", Pure: true,
			Desc:        "each endpoint's retry policy is within bounds and names a known backoff",
			Remediation: "`maxAttempts` must be between 1 and 12, with a supported backoff strategy.",
			Applies:     hasEndpoints,
			Check: func(s Subject) string {
				for i, w := range s.Draft.Webhooks {
					if w.MaxAttempts < 1 || w.MaxAttempts > 12 {
						return "webhook " + itoa(i+1) + ": maxAttempts is " + itoa(w.MaxAttempts) +
							"; it must be between 1 and 12"
					}
					if w.Backoff != "" && !containsString(d.KnownBackoffs, w.Backoff) {
						return "webhook " + itoa(i+1) + ": backoff " + quote(w.Backoff) + " is not supported"
					}
				}
				return ""
			},
		},
		{
			ID: "L4.SETTLEMENT_CURRENCY_HAS_BANK_ACCOUNT", Severity: engine.Error,
			Code: invalidCfg, Field: "/settlement/currency", Pure: true,
			Desc:        "a validated bank account can receive the settlement currency",
			Remediation: "Add a validated bank account that can receive your settlement currency.",
			Applies:     hasSettlement,
			Check: func(s Subject) string {
				if containsCurrency(s.Merchant.BankAccountCurrencies, s.Draft.Settlement.Currency) {
					return ""
				}
				return "no validated bank account can receive " + string(s.Draft.Settlement.Currency)
			},
		},
		{
			ID: "L4.SETTLEMENT_HOLD_DAYS_WITHIN_POLICY", Severity: engine.Error,
			Code: invalidCfg, Field: "/settlement/holdDays", Pure: true,
			Desc:        "the settlement hold is between zero and the tenant maximum",
			Remediation: "`holdDays` must be between 0 and your platform's maximum.",
			Applies:     hasSettlement,
			Check: func(s Subject) string {
				maxHold := defaultInt(d.MaxSettlementHoldDays, 30)
				if s.Draft.Settlement.HoldDays < 0 || s.Draft.Settlement.HoldDays > maxHold {
					return "holdDays is " + itoa(s.Draft.Settlement.HoldDays) +
						"; it must be between 0 and " + itoa(maxHold)
				}
				return ""
			},
		},
		{
			ID: "L4.FEATURE_FLAGS_ARE_KNOWN", Severity: engine.Error,
			Code: invalidCfg, Field: "/featureFlags", Pure: true,
			Desc:        "every feature-flag key is in the registered flag set",
			Remediation: "Remove the unknown feature flag.",
			Applies:     func(s Subject) bool { return len(s.Draft.FeatureFlags) > 0 },
			Check: func(s Subject) string {
				for k := range s.Draft.FeatureFlags {
					if !containsString(d.KnownFeatureFlags, k) {
						return "unknown feature flag " + quote(k)
					}
				}
				return ""
			},
		},
		{
			ID: "L4.FEATURE_FLAG_HAS_CAPABILITY", Severity: engine.Error,
			Code: invalidCfg, Field: "/featureFlags", Pure: true,
			Desc:        "every routable gateway supports the capability each enabled flag implies",
			Remediation: "Disable the feature flag or remove the gateway that does not support it.",
			Applies: func(s Subject) bool {
				for _, on := range s.Draft.FeatureFlags {
					if on {
						return true
					}
				}
				return false
			},
			Check: func(s Subject) string {
				for flag, on := range s.Draft.FeatureFlags {
					if !on {
						continue
					}
					capability, ok := d.FlagCapability[flag]
					if !ok {
						continue
					}
					for _, g := range routableGateways(s) {
						desc, found := descriptorFor(s, g)
						if !found {
							continue
						}
						if !desc.Capabilities[capability] {
							return string(g) + " does not support " + capability +
								"; disable the flag or remove the gateway"
						}
					}
				}
				return ""
			},
		},
		{
			ID: "L4.TENANT_GATEWAY_ALLOWLIST", Severity: engine.Error,
			Code: invalidCfg, Field: "/routing", Pure: true,
			Desc:        "every referenced gateway is on the tenant allowlist",
			Remediation: "Remove gateways your platform has not enabled.",
			Applies:     func(s Subject) bool { return len(d.GatewayAllowlist) > 0 },
			Check: func(s Subject) string {
				for _, g := range routableGateways(s) {
					if !containsGateway(d.GatewayAllowlist, g) {
						return string(g) + " is not enabled for your platform"
					}
				}
				return ""
			},
		},
		{
			ID: "L4.TENANT_RESIDENCY_COMPATIBLE", Severity: engine.Error,
			Code: invalidCfg, Field: "/routing", Pure: true,
			Desc:        "no routable gateway processes or stores outside the tenant's residency policy",
			Remediation: "Remove the gateway that processes outside your data residency policy.",
			Applies:     func(s Subject) bool { return len(d.ResidencyRegions) > 0 },
			Check: func(s Subject) string {
				for _, g := range routableGateways(s) {
					desc, ok := descriptorFor(s, g)
					if !ok || desc.ProcessingRegion == "" {
						continue
					}
					if !containsString(d.ResidencyRegions, desc.ProcessingRegion) {
						return string(g) + " processes in " + desc.ProcessingRegion +
							", which violates your data residency policy"
					}
				}
				return ""
			},
		},
		{
			ID: "L4.TENANT_LIMIT_CEILING_RESPECTED", Severity: engine.Error,
			Code: string(apierror.CodeAmountExceedsLimit), Field: "/risk", Pure: true,
			Desc:        "the merchant's own limits are at or below the tenant's ceilings",
			Remediation: "Lower the limit to at or below the maximum your platform permits.",
			Applies: func(s Subject) bool {
				return !d.MaxTransactionCeiling.IsZero() || !d.DailyVolumeCeiling.IsZero()
			},
			Check: func(s Subject) string {
				if !d.MaxTransactionCeiling.IsZero() && !s.Draft.Risk.MaxTransactionAmount.IsZero() {
					over, err := s.Draft.Risk.MaxTransactionAmount.GreaterThan(d.MaxTransactionCeiling)
					if err == nil && over {
						return "maxTransactionAmount exceeds the platform ceiling of " +
							d.MaxTransactionCeiling.String()
					}
				}
				if !d.DailyVolumeCeiling.IsZero() && !s.Draft.Risk.DailyVolumeLimit.IsZero() {
					over, err := s.Draft.Risk.DailyVolumeLimit.GreaterThan(d.DailyVolumeCeiling)
					if err == nil && over {
						return "dailyVolumeLimit exceeds the platform ceiling of " +
							d.DailyVolumeCeiling.String()
					}
				}
				return ""
			},
		},
		{
			ID: "L4.NO_SILENT_CAPABILITY_REGRESSION", Severity: engine.Warning,
			Code: "", Field: "/paymentMethods", Pure: true,
			Desc: "removing a currency or method that in-flight payments still reference warns " +
				"rather than blocks",
			Remediation: "In-flight payments still use a capability you are removing; they are unaffected, but new payments will be rejected.",
			Applies:     func(s Subject) bool { return s.Previous.Present },
			Check: func(s Subject) string {
				for _, m := range s.Previous.PaymentMethods {
					if containsMethod(s.Draft.PaymentMethods, m) {
						continue
					}
					if n := s.InFlight.ByMethod[m]; n > 0 {
						return itoa(n) + " in-flight payments use " + string(m) +
							"; removing it will not affect them but new ones will be rejected"
					}
				}
				for _, c := range s.Previous.SupportedCurrencies {
					if containsCurrency(s.Draft.SupportedCurrencies, c) {
						continue
					}
					if n := s.InFlight.ByCurrency[c]; n > 0 {
						return itoa(n) + " in-flight payments use " + string(c) +
							"; removing it will not affect them but new ones will be rejected"
					}
				}
				return ""
			},
		},
	}
}

// --- helpers ---------------------------------------------------------------------------------

// merchantApproved reports whether the merchant has reached a state at or past APPROVED.
//
// Derived from the merchant lifecycle rather than restated as a list of strings, so that a
// change to the state machine cannot leave this rule reasoning about a state universe that no
// longer exists.
func merchantApproved(s merchant.Status) bool {
	switch s {
	case merchant.StatusApproved, merchant.StatusProductionReady, merchant.StatusActive,
		merchant.StatusSuspended:
		return true
	default:
		// Every state before APPROVED, and every failure or termination state, is not approved.
		// Enumerating them would make this rule restate the lifecycle it deliberately derives from
	}
	return false
}

func certified(s Subject, g shared.GatewayID) bool {
	if g == "" {
		return false
	}
	for _, c := range s.Connections {
		if c.GatewayID != g {
			continue
		}
		if c.Environment != "" && s.Draft.Environment != "" && c.Environment != s.Draft.Environment {
			continue
		}
		if c.IsCertified() {
			return true
		}
	}
	return false
}

// routableGateways is every gateway the document can send traffic to: the primary, the
// fallbacks, and every rule target.
func routableGateways(s Subject) []shared.GatewayID {
	seen := map[shared.GatewayID]struct{}{}
	var out []shared.GatewayID
	add := func(g shared.GatewayID) {
		if g == "" {
			return
		}
		if _, ok := seen[g]; ok {
			return
		}
		seen[g] = struct{}{}
		out = append(out, g)
	}
	add(s.Draft.Routing.Primary)
	for _, g := range s.Draft.Routing.Fallback {
		add(g)
	}
	for _, r := range s.Draft.Routing.Rules {
		add(r.Then.Primary)
		for _, g := range r.Then.Fallback {
			add(g)
		}
	}
	return out
}

func descriptorFor(s Subject, g shared.GatewayID) (DescriptorView, bool) {
	for _, d := range s.Descriptors {
		if d.GatewayID == g {
			return d, true
		}
	}
	return DescriptorView{}, false
}

func anyDescriptorSupports(s Subject, cur money.Currency, m shared.PaymentMethod, ctry shared.Country) bool {
	for _, c := range s.Connections {
		if !c.IsCertified() {
			continue
		}
		desc, ok := descriptorFor(s, c.GatewayID)
		if !ok {
			continue
		}
		if !containsCurrency(desc.Currencies, cur) || !containsMethod(desc.Methods, m) {
			continue
		}
		if ctry != "" && !containsCountry(desc.Countries, ctry) {
			continue
		}
		return true
	}
	return false
}

// selectedCurrencies returns the currencies a rule's predicate can select. A rule with no
// currency predicate can select every enabled currency, which is why the fallback is the whole
// enabled set rather than the empty set — the permissive reading is the one that catches the
// misconfiguration.
func selectedCurrencies(r RoutingRule, enabled []money.Currency) []money.Currency {
	m, ok := r.When["currency"]
	if !ok {
		return enabled
	}
	out := make([]money.Currency, 0, len(m.Values))
	for _, v := range m.Values {
		out = append(out, money.Currency(v))
	}
	return out
}

func selectedMethods(r RoutingRule, enabled []shared.PaymentMethod) []shared.PaymentMethod {
	m, ok := r.When["paymentMethod"]
	if !ok {
		return enabled
	}
	out := make([]shared.PaymentMethod, 0, len(m.Values))
	for _, v := range m.Values {
		out = append(out, shared.PaymentMethod(v))
	}
	return out
}

// matcherProblem validates one matcher against its field's domain.
func matcherProblem(d Deps, field string, m Matcher) string {
	if !containsString(d.MatcherOps, m.Op) {
		return "uses unknown matcher " + quote(m.Op)
	}
	switch m.Op {
	case "range":
		if field != "amountRange" {
			return "may not use a range matcher"
		}
		if m.Max < m.Min {
			return "has an inverted range"
		}
		if m.Min < 0 {
			return "has a negative lower bound"
		}
		return ""
	case "eq":
		if len(m.Values) != 1 {
			return "uses eq with " + itoa(len(m.Values)) + " values"
		}
	case "in":
		if len(m.Values) == 0 {
			return "uses in with no values"
		}
	case "prefix":
		if len(m.Values) == 0 {
			return "uses prefix with no values"
		}
	}
	for _, v := range m.Values {
		switch field {
		case "currency":
			if !money.Currency(v).IsSupported() {
				return "names unknown currency " + quote(v)
			}
		case "country":
			if !shared.Country(v).IsValid() {
				return "names unknown country " + quote(v)
			}
		case "paymentMethod":
			if !shared.PaymentMethod(v).IsValid() {
				return "names unknown payment method " + quote(v)
			}
		default:
			if v == "" {
				return "has an empty value"
			}
		}
	}
	return ""
}

// shadows reports whether `earlier` fully covers `later`, so `later` can never match.
//
// The test is deliberately conservative: earlier shadows later only when every field earlier
// constrains is also constrained by later, with later's values a subset of earlier's. Anything
// more sophisticated would need a real domain model per field, and a false "this rule is dead"
// warning on a rule that is not dead is a warning operators learn to ignore.
func shadows(earlier, later RoutingRule) bool {
	if len(earlier.When) == 0 {
		return true
	}
	for field, em := range earlier.When {
		lm, ok := later.When[field]
		if !ok {
			return false
		}
		if em.Op == "range" || lm.Op == "range" {
			if em.Op != "range" || lm.Op != "range" {
				return false
			}
			if lm.Min < em.Min || lm.Max > em.Max {
				return false
			}
			continue
		}
		if !subset(lm.Values, em.Values) {
			return false
		}
	}
	return true
}

func subset(inner, outer []string) bool {
	if len(inner) == 0 {
		return false
	}
	for _, v := range inner {
		if !containsString(outer, v) {
			return false
		}
	}
	return true
}

// patternMatchesAny supports exactly one wildcard form, a trailing `*`. Anything richer would
// be a regular expression, which this package does not accept anywhere.
func patternMatchesAny(pattern string, known []string) bool {
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		for _, k := range known {
			if strings.HasPrefix(k, prefix) {
				return true
			}
		}
		return false
	}
	return containsString(known, pattern)
}

func publicHTTPSProblem(raw string) string {
	if !strings.HasPrefix(strings.ToLower(raw), "https://") {
		return "the URL is not https"
	}
	host := raw[len("https://"):]
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	switch {
	case host == "":
		return "the URL has no host"
	case strings.EqualFold(host, "localhost"):
		return "the URL host is not publicly resolvable"
	case !strings.Contains(strings.TrimSuffix(host, "."), "."):
		return "the URL host is not a public domain name"
	}
	return ""
}

func countryOrAny(c shared.Country) string {
	if c == "" {
		return "any country"
	}
	return string(c)
}

func containsString(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func containsCurrency(set []money.Currency, v money.Currency) bool {
	for _, c := range set {
		if c == v {
			return true
		}
	}
	return false
}

func containsMethod(set []shared.PaymentMethod, v shared.PaymentMethod) bool {
	for _, m := range set {
		if m == v {
			return true
		}
	}
	return false
}

func containsCountry(set []shared.Country, v shared.Country) bool {
	for _, c := range set {
		if c == v {
			return true
		}
	}
	return false
}

func containsGateway(set []shared.GatewayID, v shared.GatewayID) bool {
	for _, g := range set {
		if g == v {
			return true
		}
	}
	return false
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func quote(s string) string {
	if s == "" {
		return "(empty)"
	}
	return "`" + s + "`"
}

func itoa(n int) string { return itoa64(int64(n)) }

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
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

// formatFloat renders a weight sum to six decimal places without importing a formatter, which
// keeps the message stable across Go versions.
func formatFloat(f float64) string {
	neg := f < 0
	if neg {
		f = -f
	}
	scaled := int64(f*1e6 + 0.5)
	whole := scaled / 1e6
	frac := scaled % 1e6
	out := itoa64(whole) + "." + pad6(frac)
	if neg {
		return "-" + out
	}
	return out
}

func pad6(n int64) string {
	s := itoa64(n)
	for len(s) < 6 {
		s = "0" + s
	}
	return s
}
