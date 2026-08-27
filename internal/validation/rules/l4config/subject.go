// Package l4config is validation level 4: the merchant configuration document, validated once
// at publish time in the control plane.
//
// L4 is what makes L5 cheap. Everything expensive about a merchant's configuration —
// cross-checking it against gateway capability descriptors, proving that every enabled
// (currency, method, country) triple has somewhere to go, checking the predicate table
// compiles and is bounded — happens here, once, on a control-plane write that nobody is timing
// in milliseconds. What reaches the payment hot path is a document already known to be
// internally consistent, so L5 can intersect bitsets instead of re-deriving the truth on every
// authorization.
//
// Mode is CollectAll and this is the level where it matters most: the reader is an operator or
// a merchant engineer editing a document, and a publish that reports one problem per attempt
// converts a ten-minute edit into a ten-round-trip afternoon.
//
// See docs/validation-plane.md §3.4 and §2.4.
package l4config

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Matcher is one predicate over one field of a routing rule's `when` clause.
//
// The grammar is closed and flat, and that is a decision rather than an omission. A `when`
// clause is a map of field → matcher, implicitly ANDed; there are no boolean operators, no
// nesting, no function calls and no regular expressions. Alternation is expressed as a second
// rule. The moment an expression grammar accepts `&&` someone asks for a call, then a loop,
// and the payment path is running an interpreter on merchant-supplied input with an unbounded
// tail. Regex specifically is excluded because catastrophic backtracking is a denial of
// service delivered through a configuration field.
type Matcher struct {
	// Op is eq | in | range | prefix.
	Op string
	// Values holds the operands for eq, in and prefix.
	Values []string
	// Min and Max are the inclusive bounds for range, in minor units.
	Min int64
	Max int64
}

// RoutingRule is one predicate and the gateway it selects.
type RoutingRule struct {
	When map[string]Matcher
	Then RoutingTarget
}

// RoutingTarget is what a matching routing rule selects.
type RoutingTarget struct {
	Primary  shared.GatewayID
	Fallback []shared.GatewayID
}

// Routing is the document's routing section.
type Routing struct {
	Strategy string
	Primary  shared.GatewayID
	Fallback []shared.GatewayID
	Rules    []RoutingRule
	// Weights are the scoring weights for the WEIGHTED strategy, keyed by
	// health | successRate | cost | latency. Floats are acceptable here and only here: these
	// are scoring coefficients, never money, and the platform's no-float rule is about amounts.
	Weights map[string]float64
}

// RiskConfig is the document's risk section.
type RiskConfig struct {
	MaxTransactionAmount money.Money
	Require3DSAbove      money.Money
	DailyVolumeLimit     money.Money
	BlockedCountries     []shared.Country
}

// Limits is the document's limits section.
type Limits struct {
	MaxRefundWindowDays int
	MaxPartialCaptures  int
}

// Velocity is the document's velocity section.
type Velocity struct {
	MaxPaymentsPerMinute  int
	MaxPerCardPerHour     int
	MaxPerCustomerPerDay  int
	MaxDistinctCardsPerHr int
}

// WebhookEndpoint is one merchant-facing webhook destination.
type WebhookEndpoint struct {
	URL           string
	EventPatterns []string
	MaxAttempts   int
	Backoff       string
}

// Settlement is the document's settlement section.
type Settlement struct {
	Present  bool
	Currency money.Currency
	HoldDays int
}

// Draft is the configuration document being published.
type Draft struct {
	SchemaVersion       string
	Version             int64
	Environment         shared.Environment
	SupportedCurrencies []money.Currency
	PaymentMethods      []shared.PaymentMethod
	Countries           []shared.Country
	Routing             Routing
	Risk                RiskConfig
	Limits              Limits
	Velocity            Velocity
	Webhooks            []WebhookEndpoint
	Settlement          Settlement
	FeatureFlags        map[string]bool
}

// Previous is the currently published document, when one exists.
type Previous struct {
	Present             bool
	Version             int64
	SupportedCurrencies []money.Currency
	PaymentMethods      []shared.PaymentMethod
}

// MerchantView is the part of the merchant aggregate L4 needs.
type MerchantView struct {
	Status merchant.Status
	// LicensedCountries is what L2 validated as the merchant's operating territory.
	LicensedCountries []shared.Country
	// BankAccountCurrencies is what the validated settlement accounts can receive.
	BankAccountCurrencies []money.Currency
	// ExpectedDailyPaymentCount comes from the declared processing profile.
	ExpectedDailyPaymentCount int
}

// ConnectionView is one gateway connection's state for this merchant and environment.
type ConnectionView struct {
	GatewayID           shared.GatewayID
	Environment         shared.Environment
	Status              gateway.ConnectionStatus
	CertificationStatus gateway.CertificationStatus
}

// IsCertified reports whether the connection may carry production traffic.
func (c ConnectionView) IsCertified() bool {
	return c.CertificationStatus == gateway.CertificationPassed
}

// DescriptorView is one gateway's capability surface.
type DescriptorView struct {
	GatewayID          shared.GatewayID
	Currencies         []money.Currency
	Methods            []shared.PaymentMethod
	Countries          []shared.Country
	RefundWindowDays   int
	MaxPartialCaptures int
	// Capabilities names the optional features a feature flag can require:
	// networkTokens, partialCapture, incrementalAuth.
	Capabilities map[string]bool
	// ProcessingRegion is where the gateway processes and stores.
	ProcessingRegion string
}

// InFlightUsage counts payments in a non-terminal state by the capability they depend on, so
// that removing a capability can warn about what is still using it.
type InFlightUsage struct {
	ByMethod   map[shared.PaymentMethod]int
	ByCurrency map[money.Currency]int
}

// Subject is everything L4 evaluates.
type Subject struct {
	Draft       Draft
	Previous    Previous
	Merchant    MerchantView
	Connections []ConnectionView
	Descriptors []DescriptorView
	InFlight    InFlightUsage
	// ETagMatched records whether the If-Match precondition held. It is an input rather than a
	// check here because the comparison happened at the HTTP layer against a stored ETag.
	ETagMatched bool
	// Now is the injected clock reading.
	Now time.Time
}

// Deps is the tenant policy plus the platform's closed enums. Pure data.
type Deps struct {
	SupportedSchemaVersions []string
	RoutingStrategies       []string
	PredicateFields         []string
	MatcherOps              []string
	// Size budget for the compiled predicate table. It is what turns the routing stage's 5 ms
	// budget from a hope into an arithmetic statement.
	MaxRoutingRules      int
	MaxPredicatesPerRule int
	MaxValuesPerIn       int
	// CurrencyAllowlist and GatewayAllowlist restrict the tenant; empty means unrestricted.
	CurrencyAllowlist []money.Currency
	GatewayAllowlist  []shared.GatewayID
	// MandatoryBlockedCountries may never be removed from a merchant's blocked list.
	MandatoryBlockedCountries []shared.Country
	// SCAFloor is the regulatory strong-customer-authentication exemption ceiling, and
	// SCACorridors is where it applies.
	SCAFloor     money.Money
	SCACorridors []shared.Country
	// Ceilings the tenant imposes on a merchant's own limits.
	MaxTransactionCeiling money.Money
	DailyVolumeCeiling    money.Money
	MaxSettlementHoldDays int
	// ResidencyRegions is the tenant's permitted processing regions.
	ResidencyRegions []string
	// KnownFeatureFlags and FlagCapability map a flag to the gateway capability it implies.
	KnownFeatureFlags []string
	FlagCapability    map[string]string
	// KnownEventTypes is the platform event catalog; KnownBackoffs the retry strategies.
	KnownEventTypes []string
	KnownBackoffs   []string
	// TenantBaseCurrency is always acceptable as a limit currency even when not enabled for
	// payments, because a tenant states its ceilings in its own currency.
	TenantBaseCurrency money.Currency
}

// DefaultDeps returns the platform defaults from docs/validation-plane.md §3.4.
func DefaultDeps() Deps {
	return Deps{
		SupportedSchemaVersions: []string{"2026-01-01", "2025-06-01"},
		RoutingStrategies:       []string{"PRIORITY_WITH_FALLBACK", "WEIGHTED", "LEAST_COST", "LOWEST_LATENCY"},
		PredicateFields: []string{
			"currency", "paymentMethod", "country", "amountRange", "cardBrand", "customerSegment",
		},
		MatcherOps:            []string{"eq", "in", "range", "prefix"},
		MaxRoutingRules:       64,
		MaxPredicatesPerRule:  16,
		MaxValuesPerIn:        256,
		MaxSettlementHoldDays: 30,
		KnownFeatureFlags: []string{
			"networkTokens", "partialCapture", "incrementalAuth", "level3Data", "accountUpdater",
		},
		FlagCapability: map[string]string{
			"networkTokens":   "networkTokens",
			"partialCapture":  "partialCapture",
			"incrementalAuth": "incrementalAuth",
		},
		KnownEventTypes: []string{
			"payment.created.v1", "payment.authorized.v1", "payment.captured.v1",
			"payment.failed.v1", "payment.refunded.v1", "payment.disputed.v1",
			"payout.settled.v1", "merchant.activated.v1", "configuration.published.v1",
		},
		KnownBackoffs: []string{"EXPONENTIAL", "EXPONENTIAL_JITTER", "LINEAR", "FIXED"},
	}
}
