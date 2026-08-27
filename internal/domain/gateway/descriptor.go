// Package gateway is the gateway integration bounded context's domain model (BC-4).
//
// Three things live here, and they are deliberately three separate aggregates rather than one:
//
//   - Gateway (descriptor.go) — what a gateway *can* do. Global, tenant-independent, and it
//     changes when a vendor ships an API version: roughly monthly.
//   - Connection (connection.go) — the binding of one merchant to one gateway. Per
//     (tenant, merchant, gateway), and it changes on the onboarding and credential-rotation
//     cadence: a handful of times per merchant per year.
//   - Health (health.go) — what a gateway is *currently* doing. Per (gateway, operation), and it
//     changes on every dispatch: thousands of times per second.
//
// Those three rates of change are four orders of magnitude apart. Fusing them into one aggregate
// would put a health observation in the same write transaction as a capability edit, and would
// make every routing decision read a row that a credential rotation is contending for. The cost
// of the split is that the routing engine assembles its answer from three reads instead of one;
// it caches all three, so it pays that cost approximately never.
//
// This package imports only the standard library, pkg/* and the shared kernel — no SQL, no HTTP,
// no vendor SDK types. See docs/spec/00-design-baseline.md §4 and §10.
package gateway

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Status is the registry lifecycle of a gateway integration.
//
// This is about *our* integration, not the vendor's uptime — a gateway that is down is HEALTHY
// or UNHEALTHY in health.go, never DISABLED here. Conflating the two is how an operator ends up
// deleting a registry entry to stop traffic and discovers that the existing connections, the
// in-flight refunds and the settlement reconciliation all referenced it.
type Status string

const (
	// StatusActive means the integration is supported and may be selected for new traffic.
	StatusActive Status = "ACTIVE"

	// StatusDeprecated means the integration still works and existing connections still route
	// through it, but the router must not select it for a merchant that has an alternative.
	// This is the state an integration sits in for the months between "we intend to remove
	// this" and "no merchant depends on it any more"; without it, removal is a flag day.
	StatusDeprecated Status = "DEPRECATED"

	// StatusDisabled means no new operation may be dispatched. Refund and lookup traffic for
	// historical payments is still expressible — the descriptor is not deleted — but the router
	// will not choose it.
	StatusDisabled Status = "DISABLED"
)

// IsValid reports whether s is a known registry status.
func (s Status) IsValid() bool {
	return s == StatusActive || s == StatusDeprecated || s == StatusDisabled
}

// String satisfies fmt.Stringer.
func (s Status) String() string { return string(s) }

// AcceptsNewTraffic reports whether the router may select this gateway for a new payment.
// DEPRECATED answers true: a deprecated gateway that stops accepting traffic before its
// merchants have been migrated is an outage, not a deprecation.
func (s Status) AcceptsNewTraffic() bool { return s == StatusActive || s == StatusDeprecated }

// SignatureScheme names how a gateway signs the webhooks it sends us.
//
// It lives on the descriptor rather than being hardcoded in each adapter because the webhook
// ingress must decide *which verifier to run* before it has parsed a body it does not yet trust.
// Reading the scheme from the registry keyed by the URL's gateway slug is what makes that
// decision safe.
type SignatureScheme string

const (
	// SchemeHMACSHA256 is a shared-secret HMAC over a timestamp and the raw body. Stripe-style.
	SchemeHMACSHA256 SignatureScheme = "HMAC_SHA256"
	// SchemeHMACSHA512 is the same construction with a wider digest. Adyen-style.
	SchemeHMACSHA512 SignatureScheme = "HMAC_SHA512"
	// SchemeECDSAP256 is an asymmetric signature verified against the vendor's published key,
	// which removes the shared secret from our side of the boundary entirely.
	SchemeECDSAP256 SignatureScheme = "ECDSA_P256"
	// SchemeNone means the gateway does not sign. A gateway with this scheme may only be
	// registered in sandbox: an unsigned production webhook is an unauthenticated instruction to
	// change money state, and it is validated in NewGateway rather than left to policy.
	SchemeNone SignatureScheme = "NONE"
)

// IsValid reports whether s is a known signature scheme.
func (s SignatureScheme) IsValid() bool {
	switch s {
	case SchemeHMACSHA256, SchemeHMACSHA512, SchemeECDSAP256, SchemeNone:
		return true
	default:
		return false
	}
}

// String satisfies fmt.Stringer.
func (s SignatureScheme) String() string { return string(s) }

// Capabilities is the declarative answer to "could this gateway, in principle, do this".
//
// It is declarative data rather than code in an adapter for one reason: the routing engine must
// evaluate eligibility for every candidate gateway on the payment hot path, and it must be able
// to explain a rejection. A capability expressed as an `if` inside an adapter can only be
// discovered by calling the adapter, which is exactly the network round trip routing exists to
// avoid, and it produces "the gateway said no" rather than "adyen is not licensed for BR".
//
// What this is not: a promise. A gateway that declares SupportsPartialCapture can still refuse a
// specific partial capture. Capabilities narrow the candidate set before dispatch; the response
// validation plane (L6) handles the gateway disagreeing after the fact.
type Capabilities struct {
	// Countries is the set of merchant countries the gateway is licensed to acquire for. Empty
	// means "no country", not "all countries" — an unpopulated capability set must fail closed,
	// because the failure mode of the other choice is routing a Brazilian merchant to a gateway
	// with no Brazilian licence and finding out at settlement.
	Countries []shared.Country

	// Currencies is the set of presentment currencies the gateway accepts.
	Currencies []money.Currency

	// Methods is the set of platform payment methods the gateway supports. These are the
	// platform's coarse categories, not the vendor's taxonomy; the adapter translates.
	Methods []shared.PaymentMethod

	// Operations is the set of gateway operations the integration implements. A gateway that
	// authorizes but has no void endpoint is normal and must be representable, because routing a
	// manual-capture payment to it is a decision the router has to be able to avoid.
	Operations []shared.Operation

	// SupportsPartialCapture permits capturing less than the authorized amount.
	SupportsPartialCapture bool
	// SupportsMultipleCaptures permits more than one capture against one authorization. Distinct
	// from SupportsPartialCapture: several gateways allow one smaller capture but not two.
	SupportsMultipleCaptures bool
	// SupportsPartialRefund permits refunding less than the captured amount.
	SupportsPartialRefund bool
	// SupportsVoid permits releasing an authorization before capture. A gateway without it forces
	// the platform to wait for the authorization to expire, which holds the payer's funds and
	// generates support contacts.
	SupportsVoid bool
	// Supports3DS2 permits EMV 3-D Secure 2.x authentication. A gateway without it cannot be
	// selected for a payment in an SCA jurisdiction that requires a challenge.
	Supports3DS2 bool
	// SupportsNetworkTokens permits scheme network tokens rather than gateway-local tokens.
	// Materially changes both interchange and the portability of a saved instrument.
	SupportsNetworkTokens bool
	// SupportsIdempotencyKeys reports whether the gateway deduplicates on a caller-supplied key.
	// A gateway without it cannot have a transport-level retry issued against it safely, and the
	// dispatcher degrades to "one shot, then reconcile".
	SupportsIdempotencyKeys bool

	// MaxRefundWindow is how long after capture the gateway will still accept a refund. Zero
	// means unbounded. Beyond this the money has to go back by bank transfer, which is a
	// merchant operations problem rather than an API call, so predicting it before accepting the
	// refund request is worth the field.
	MaxRefundWindow time.Duration

	// AuthorizationValidity is how long an authorization is held before the gateway or the issuer
	// releases it. It seeds Payment.authExpiresAt, which is what makes "capture before it
	// expires" an alertable condition rather than a discovered one.
	AuthorizationValidity time.Duration

	// MinAmount and MaxAmount are per-currency bounds. Keyed by currency because the numbers are
	// not conversions of one another: a gateway's JPY floor is a separately negotiated figure,
	// and deriving it from the USD floor with an exchange rate would be wrong by construction.
	// A currency absent from either map is unbounded in that direction.
	MinAmount map[money.Currency]money.Money
	MaxAmount map[money.Currency]money.Money
}

// Clone returns a deep copy. The aggregate holds Capabilities by value, but its slices and maps
// are reference types: without this, handing a caller the capabilities would hand them the live
// backing arrays and let them edit a gateway's licensed countries without going through a method.
func (c Capabilities) Clone() Capabilities {
	out := c
	out.Countries = append([]shared.Country(nil), c.Countries...)
	out.Currencies = append([]money.Currency(nil), c.Currencies...)
	out.Methods = append([]shared.PaymentMethod(nil), c.Methods...)
	out.Operations = append([]shared.Operation(nil), c.Operations...)
	out.MinAmount = cloneAmounts(c.MinAmount)
	out.MaxAmount = cloneAmounts(c.MaxAmount)
	return out
}

func cloneAmounts(in map[money.Currency]money.Money) map[money.Currency]money.Money {
	if in == nil {
		return nil
	}
	out := make(map[money.Currency]money.Money, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Validate checks the capability set is internally coherent.
//
// It runs at registration time, in the control plane, where a rejection costs an operator a
// second attempt. The alternative — discovering that a gateway declares MULTIPLE_CAPTURES but
// not PARTIAL_CAPTURE at the moment a router selects it for a split shipment — costs a payment.
func (c Capabilities) Validate() error {
	if len(c.Countries) == 0 {
		return capErr("capabilities.countries", "MISSING_COUNTRIES",
			"a gateway must declare at least one licensed country", "L4.GATEWAY_CAPABILITIES_COMPLETE")
	}
	if len(c.Currencies) == 0 {
		return capErr("capabilities.currencies", "MISSING_CURRENCIES",
			"a gateway must declare at least one presentment currency", "L4.GATEWAY_CAPABILITIES_COMPLETE")
	}
	if len(c.Methods) == 0 {
		return capErr("capabilities.paymentMethods", "MISSING_METHODS",
			"a gateway must declare at least one payment method", "L4.GATEWAY_CAPABILITIES_COMPLETE")
	}
	if len(c.Operations) == 0 {
		return capErr("capabilities.operations", "MISSING_OPERATIONS",
			"a gateway must declare at least one operation", "L4.GATEWAY_CAPABILITIES_COMPLETE")
	}
	for _, ctry := range c.Countries {
		if !ctry.IsValid() {
			return capErr("capabilities.countries", "UNKNOWN_COUNTRY",
				"unknown ISO 3166-1 alpha-2 code "+string(ctry), "L4.GATEWAY_CAPABILITIES_COMPLETE")
		}
	}
	for _, cur := range c.Currencies {
		if !cur.IsSupported() {
			return capErr("capabilities.currencies", "UNKNOWN_CURRENCY",
				"unsupported currency "+string(cur), "L4.GATEWAY_CAPABILITIES_COMPLETE")
		}
	}
	for _, m := range c.Methods {
		if !m.IsValid() {
			return capErr("capabilities.paymentMethods", "UNKNOWN_METHOD",
				"unknown payment method "+string(m), "L4.GATEWAY_CAPABILITIES_COMPLETE")
		}
	}
	// Multiple captures without partial captures is not expressible: every capture after the
	// first is necessarily partial. A descriptor that claims both-but-not-one is a data entry
	// error, and accepting it would let the router promise a split shipment it cannot deliver.
	if c.SupportsMultipleCaptures && !c.SupportsPartialCapture {
		return capErr("capabilities.supportsMultipleCaptures", "INCOHERENT_CAPTURE_FLAGS",
			"multiple captures imply partial capture; declare supportsPartialCapture as well",
			"L4.GATEWAY_CAPABILITIES_COHERENT")
	}
	if c.MaxRefundWindow < 0 {
		return capErr("capabilities.maxRefundWindow", "NEGATIVE_DURATION",
			"the refund window may not be negative; use zero for unbounded",
			"L4.GATEWAY_CAPABILITIES_COHERENT")
	}
	if c.AuthorizationValidity < 0 {
		return capErr("capabilities.authorizationValidity", "NEGATIVE_DURATION",
			"the authorization validity may not be negative", "L4.GATEWAY_CAPABILITIES_COHERENT")
	}
	for cur, minAmt := range c.MinAmount {
		if minAmt.Currency() != cur {
			return capErr("capabilities.minAmount", "CURRENCY_KEY_MISMATCH",
				"the bound for "+string(cur)+" is denominated in "+string(minAmt.Currency()),
				"L4.GATEWAY_CAPABILITIES_COHERENT")
		}
		if maxAmt, ok := c.MaxAmount[cur]; ok {
			if over, err := minAmt.GreaterThan(maxAmt); err == nil && over {
				return capErr("capabilities.minAmount", "MIN_EXCEEDS_MAX",
					"the minimum for "+string(cur)+" is greater than the maximum",
					"L4.GATEWAY_CAPABILITIES_COHERENT")
			}
		}
	}
	for cur, maxAmt := range c.MaxAmount {
		if maxAmt.Currency() != cur {
			return capErr("capabilities.maxAmount", "CURRENCY_KEY_MISMATCH",
				"the bound for "+string(cur)+" is denominated in "+string(maxAmt.Currency()),
				"L4.GATEWAY_CAPABILITIES_COHERENT")
		}
	}
	return nil
}

func capErr(field, code, msg, ruleID string) *apierror.Error {
	return apierror.New(apierror.CodeConfigurationInvalid, "gateway capabilities are not valid").
		WithDetail(apierror.Detail{Field: field, Code: code, Message: msg, RuleID: ruleID})
}

// Supports answers the eligibility question the routing engine asks once per candidate gateway
// per payment, and answers it with the dimension that failed.
//
// Returning a typed error rather than a bool is the whole point. The router aggregates these
// into the NO_ELIGIBLE_GATEWAY response, and a merchant who is told "no gateway was eligible"
// files a support ticket, whereas a merchant told "stripe: BRL is not supported; adyen: UPI is
// not supported" fixes their own configuration. The rejection reason is the product.
//
// The dimensions are checked in a fixed order — country, currency, method, operation — so the
// reported reason is stable across releases and across gateways. An unstable "first failure
// wins" order makes two runs of the same routing decision produce different explanations, which
// makes the explanation untrustworthy.
func (c Capabilities) Supports(country shared.Country, currency money.Currency, method shared.PaymentMethod, op shared.Operation) error {
	if !containsCountry(c.Countries, country) {
		return apierror.Newf(apierror.CodeCountryBlocked,
			"the gateway is not licensed to acquire in %s", country).
			WithDetail(apierror.Detail{
				Field:   "country",
				Code:    "COUNTRY_NOT_SUPPORTED",
				Message: "this is a licensing restriction of the gateway, not a sanctions block",
				RuleID:  "L5.GATEWAY_SUPPORTS_COUNTRY",
			})
	}
	if !containsCurrency(c.Currencies, currency) {
		return apierror.Newf(apierror.CodeCurrencyNotSupported,
			"the gateway does not accept %s", currency).
			WithDetail(apierror.Detail{
				Field:   "currency",
				Code:    "CURRENCY_NOT_SUPPORTED",
				Message: "presentment currency is not in the gateway's declared set",
				RuleID:  "L5.GATEWAY_SUPPORTS_CURRENCY",
			})
	}
	if !containsMethod(c.Methods, method) {
		return apierror.Newf(apierror.CodePaymentMethodNotSupported,
			"the gateway does not support %s", method).
			WithDetail(apierror.Detail{
				Field:   "paymentMethod",
				Code:    "METHOD_NOT_SUPPORTED",
				Message: "payment method is not in the gateway's declared set",
				RuleID:  "L5.GATEWAY_SUPPORTS_METHOD",
			})
	}
	if !containsOperation(c.Operations, op) {
		return apierror.Newf(apierror.CodeGatewayNotConfigured,
			"the gateway integration does not implement %s", op).
			WithDetail(apierror.Detail{
				Field:   "operation",
				Code:    "OPERATION_NOT_SUPPORTED",
				Message: "this gateway integration does not implement the requested operation",
				RuleID:  "L5.GATEWAY_SUPPORTS_OPERATION",
			})
	}
	return nil
}

// SupportsOperation is the single-dimension check, used by the refund and void paths where the
// country, currency and method were already fixed when the payment was authorized.
func (c Capabilities) SupportsOperation(op shared.Operation) bool {
	return containsOperation(c.Operations, op)
}

// CanRefundAfter reports whether a refund is still within the gateway's window d after capture.
//
// A zero MaxRefundWindow means unbounded, which is the honest representation of "the vendor
// documents no limit". Modelling that as an enormous duration would work until somebody wrote a
// comparison against it; modelling it as zero forces the caller through this method, which is
// the only place the convention is encoded.
func (c Capabilities) CanRefundAfter(d time.Duration) bool {
	if c.MaxRefundWindow == 0 {
		return true
	}
	return d <= c.MaxRefundWindow
}

// AmountWithinBounds checks the amount against the gateway's per-currency floor and ceiling.
//
// Both directions are real and both are worth catching before dispatch: below the floor the
// gateway rejects with a vendor-specific error the merchant cannot act on, and above the ceiling
// several gateways silently truncate or split rather than refusing, which produces a capture
// that does not match the authorization.
//
// The two failures get different codes deliberately. Above the ceiling is AMOUNT_EXCEEDS_LIMIT,
// a business-rule failure the merchant can resolve by splitting the order. Below the floor is
// AMOUNT_INVALID, a validation failure — a EUR 0.01 charge is not a limit the merchant can
// negotiate, it is an amount no acquirer will process.
func (c Capabilities) AmountWithinBounds(amount money.Money) error {
	if !amount.IsValid() {
		return apierror.New(apierror.CodeAmountInvalid, "amount carries an unsupported currency").
			WithDetail(apierror.Detail{
				Field: "amount.currency", Code: "UNKNOWN_CURRENCY",
				Message: "the amount's currency is not in the platform's supported set",
				RuleID:  "L5.AMOUNT_CURRENCY_SUPPORTED",
			})
	}
	if minAmt, ok := c.MinAmount[amount.Currency()]; ok {
		if under, err := amount.LessThan(minAmt); err == nil && under {
			return apierror.Newf(apierror.CodeAmountInvalid,
				"amount %s is below the gateway minimum of %s", amount, minAmt).
				WithDetail(apierror.Detail{
					Field: "amount", Code: "BELOW_GATEWAY_MINIMUM",
					Message: "the gateway will not process an amount below " + minAmt.String(),
					RuleID:  "L5.AMOUNT_WITHIN_GATEWAY_BOUNDS",
				})
		}
	}
	if maxAmt, ok := c.MaxAmount[amount.Currency()]; ok {
		if over, err := amount.GreaterThan(maxAmt); err == nil && over {
			return apierror.Newf(apierror.CodeAmountExceedsLimit,
				"amount %s exceeds the gateway maximum of %s", amount, maxAmt).
				WithDetail(apierror.Detail{
					Field: "amount", Code: "ABOVE_GATEWAY_MAXIMUM",
					Message: "the gateway will not process an amount above " + maxAmt.String(),
					RuleID:  "L5.AMOUNT_WITHIN_GATEWAY_BOUNDS",
				})
		}
	}
	return nil
}

func containsCountry(set []shared.Country, v shared.Country) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

func containsCurrency(set []money.Currency, v money.Currency) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

func containsMethod(set []shared.PaymentMethod, v shared.PaymentMethod) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

func containsOperation(set []shared.Operation, v shared.Operation) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

// AnyMethod is the wildcard method in a CostRate: a rate carrying it applies to every method in
// its currency for which no exact rate exists. It is the empty string because
// shared.ParsePaymentMethod rejects the empty string, so a wildcard can never be produced by
// parsing caller input — only by a deliberate constant in a registry document.
const AnyMethod shared.PaymentMethod = ""

// CostRate is one line of a gateway's pricing: a fixed per-transaction fee plus a proportion in
// basis points, for one (currency, method) pair.
//
// Basis points rather than a percentage float, for the reason money.MulBasisPoints exists: the
// estimate the router scores on and the invoice the finance team reconciles against must agree
// to the minor unit, and a float64 percentage cannot promise that.
type CostRate struct {
	Currency money.Currency
	// Method may be AnyMethod, which makes this the fallback rate for the currency.
	Method shared.PaymentMethod
	// FixedFee is the per-transaction component, denominated in Currency.
	FixedFee money.Money
	// BasisPoints is the proportional component: 290 is 2.90%.
	BasisPoints int64
}

// CostModel is a gateway's price list, and it is an input to the routing score rather than an
// output of it.
//
// Cost is modelled here — coarsely, as published card-present-style pricing — and not as true
// interchange-plus, deliberately. True cost depends on interchange category, which depends on the
// issuer's BIN, the authentication result and the settlement timing, none of which are known
// before dispatch. A router that waits for exact cost never routes. What the score needs is a
// stable ordering between gateways for the same payment, and published pricing gives that.
type CostModel struct {
	rates []CostRate
}

// NewCostModel builds and validates a price list. It rejects a rate whose fixed fee is in a
// different currency from the rate itself, because that combination silently produces an
// estimate in the wrong currency and there is no downstream check that would catch it.
func NewCostModel(rates ...CostRate) (CostModel, error) {
	seen := make(map[[2]string]struct{}, len(rates))
	for _, r := range rates {
		if !r.Currency.IsSupported() {
			return CostModel{}, costErr("costModel.currency", "UNKNOWN_CURRENCY",
				"unsupported currency "+string(r.Currency))
		}
		if r.Method != AnyMethod && !r.Method.IsValid() {
			return CostModel{}, costErr("costModel.paymentMethod", "UNKNOWN_METHOD",
				"unknown payment method "+string(r.Method))
		}
		if r.BasisPoints < 0 {
			return CostModel{}, costErr("costModel.basisPoints", "NEGATIVE_RATE",
				"basis points may not be negative")
		}
		if !r.FixedFee.IsValid() || r.FixedFee.Currency() != r.Currency {
			return CostModel{}, costErr("costModel.fixedFee", "FEE_CURRENCY_MISMATCH",
				"the fixed fee for "+string(r.Currency)+" must be denominated in "+string(r.Currency))
		}
		if r.FixedFee.IsNegative() {
			return CostModel{}, costErr("costModel.fixedFee", "NEGATIVE_FEE",
				"the fixed fee may not be negative")
		}
		k := [2]string{string(r.Currency), string(r.Method)}
		if _, dup := seen[k]; dup {
			return CostModel{}, costErr("costModel", "DUPLICATE_RATE",
				"two rates declared for "+string(r.Currency)+"/"+string(r.Method))
		}
		seen[k] = struct{}{}
	}
	return CostModel{rates: append([]CostRate(nil), rates...)}, nil
}

func costErr(field, code, msg string) *apierror.Error {
	return apierror.New(apierror.CodeConfigurationInvalid, "gateway cost model is not valid").
		WithDetail(apierror.Detail{
			Field: field, Code: code, Message: msg, RuleID: "L4.GATEWAY_COST_MODEL_VALID",
		})
}

// Rates returns a copy of the price list.
func (m CostModel) Rates() []CostRate { return append([]CostRate(nil), m.rates...) }

// IsEmpty reports whether the model carries no rates. An empty model is legal at registration —
// pricing is often negotiated after the integration is built — and the router treats a gateway
// with no price list as cost-neutral rather than free.
func (m CostModel) IsEmpty() bool { return len(m.rates) == 0 }

// EstimateCost returns the fee the gateway is expected to charge for this transaction.
//
// The exact rate for (currency, method) wins over the currency-wide fallback, because that is
// the direction pricing is actually negotiated: a blended card rate with a separately quoted
// wallet or bank-debit rate on top.
//
// A missing rate is an error rather than a zero. Zero would make an unpriced gateway the cheapest
// candidate in every routing decision, which is precisely backwards: the gateway nobody has
// priced is the one we know least about.
func (m CostModel) EstimateCost(amount money.Money, method shared.PaymentMethod) (money.Money, error) {
	if !amount.IsValid() {
		return money.Money{}, apierror.New(apierror.CodeAmountInvalid,
			"cannot estimate cost for an amount with an unsupported currency")
	}
	var rate *CostRate
	for i := range m.rates {
		r := &m.rates[i]
		if r.Currency != amount.Currency() {
			continue
		}
		if r.Method == method {
			rate = r
			break
		}
		if r.Method == AnyMethod && rate == nil {
			rate = r
		}
	}
	if rate == nil {
		return money.Money{}, apierror.Newf(apierror.CodeGatewayNotConfigured,
			"no cost rate is configured for %s/%s", amount.Currency(), method).
			WithDetail(apierror.Detail{
				Field:   "costModel",
				Code:    "RATE_NOT_CONFIGURED",
				Message: "add a rate for this currency, or a currency-wide fallback rate",
				RuleID:  "L4.GATEWAY_COST_MODEL_COVERS_ROUTE",
			})
	}
	proportional, err := amount.MulBasisPoints(rate.BasisPoints)
	if err != nil {
		return money.Money{}, apierror.Wrap(err, apierror.CodeInternalError, "cost estimate overflowed")
	}
	total, err := proportional.Add(rate.FixedFee)
	if err != nil {
		return money.Money{}, apierror.Wrap(err, apierror.CodeInternalError, "cost estimate overflowed")
	}
	return total, nil
}

// Gateway is the registry entry for one payment gateway integration.
//
// It is global, not per tenant: the fact that Adyen supports iDEAL in EUR is a property of Adyen,
// and duplicating it per tenant would mean a vendor capability change had to be applied a
// thousand times, with a thousand chances to apply it inconsistently. What is per tenant is
// *permission* to use the gateway, which lives on the Tenant, and *credentials*, which live on
// the Connection.
type Gateway struct {
	id          shared.GatewayID
	displayName string
	// vendor is the legal counterparty ("Stripe Payments Europe, Ltd."), which is not always
	// derivable from the slug and is what appears on the contract the integration operates under.
	vendor string
	// apiVersion is the vendor API version this integration is pinned to. It is on the descriptor
	// rather than in the adapter's source because a version bump is an operational change with a
	// certification requirement, and it has to be visible to the control plane that schedules it.
	apiVersion string

	// baseURLs is per environment. Keeping sandbox and production endpoints on the same record,
	// keyed by a validated Environment, is what makes "the certification run pointed at
	// production" a lookup miss rather than a real charge.
	baseURLs map[shared.Environment]string

	capabilities    Capabilities
	signatureScheme SignatureScheme
	status          Status
	costModel       CostModel

	createdAt time.Time
	updatedAt time.Time
	version   shared.Version
}

// NewGatewayParams are the inputs to registering a gateway. A parameter struct rather than a
// nine-argument constructor: most of the arguments are strings, and a positional swap of vendor
// and apiVersion is a bug the compiler cannot catch.
type NewGatewayParams struct {
	ID              shared.GatewayID
	DisplayName     string
	Vendor          string
	APIVersion      string
	BaseURLs        map[shared.Environment]string
	Capabilities    Capabilities
	SignatureScheme SignatureScheme
	Status          Status
	CostModel       CostModel
}

// NewGateway registers a gateway, validating everything that must be true of every gateway
// everywhere. Rules that depend on a tenant's entitlements are the Tenant's business (L4) and are
// checked there.
func NewGateway(p NewGatewayParams, clock shared.Clock) (*Gateway, error) {
	if p.ID.IsZero() {
		return nil, apierror.New(apierror.CodeValidationFailed, "a gateway requires an identifier").
			WithDetail(apierror.Detail{
				Field: "gatewayId", Code: "MISSING", Message: "a gateway slug is required",
				RuleID: "L4.GATEWAY_ID_REQUIRED",
			})
	}
	if _, err := shared.ParseGatewayID(p.ID.String()); err != nil {
		return nil, err
	}
	if p.DisplayName == "" {
		return nil, apierror.New(apierror.CodeValidationFailed, "a gateway requires a display name").
			WithDetail(apierror.Detail{
				Field: "displayName", Code: "MISSING",
				Message: "operators identify gateways by display name in the console",
				RuleID:  "L4.GATEWAY_DISPLAY_NAME_REQUIRED",
			})
	}
	if !p.SignatureScheme.IsValid() {
		return nil, apierror.Newf(apierror.CodeValidationFailed,
			"unknown webhook signature scheme %q", p.SignatureScheme)
	}
	status := p.Status
	if status == "" {
		status = StatusActive
	}
	if !status.IsValid() {
		return nil, apierror.Newf(apierror.CodeValidationFailed, "unknown gateway status %q", p.Status)
	}
	if err := p.Capabilities.Validate(); err != nil {
		return nil, err
	}
	if len(p.BaseURLs) == 0 {
		return nil, apierror.New(apierror.CodeConfigurationInvalid, "a gateway requires at least one base URL").
			WithDetail(apierror.Detail{
				Field: "baseUrls", Code: "MISSING",
				Message: "declare a base URL for each environment this gateway serves",
				RuleID:  "L4.GATEWAY_BASE_URL_REQUIRED",
			})
	}
	for env, u := range p.BaseURLs {
		if !env.IsValid() {
			return nil, apierror.Newf(apierror.CodeConfigurationInvalid,
				"base URL declared for unknown environment %q", env)
		}
		if u == "" {
			return nil, apierror.Newf(apierror.CodeConfigurationInvalid,
				"base URL for environment %s is empty", env)
		}
	}
	// An unsigned webhook is an unauthenticated instruction to change money state. Permitting
	// SchemeNone in production would mean anyone who learns the URL can mark a payment captured,
	// so the restriction is a constructor invariant rather than a policy document.
	if p.SignatureScheme == SchemeNone {
		if _, ok := p.BaseURLs[shared.EnvironmentProduction]; ok {
			return nil, apierror.New(apierror.CodeConfigurationInvalid,
				"a gateway with no webhook signature scheme may not be registered for production").
				WithDetail(apierror.Detail{
					Field: "webhookSignatureScheme", Code: "UNSIGNED_IN_PRODUCTION",
					Message: "unsigned webhooks are accepted in sandbox only",
					RuleID:  "L4.GATEWAY_WEBHOOKS_SIGNED_IN_PRODUCTION",
				})
		}
	}

	now := clock.Now()
	return &Gateway{
		id:              p.ID,
		displayName:     p.DisplayName,
		vendor:          p.Vendor,
		apiVersion:      p.APIVersion,
		baseURLs:        copyURLs(p.BaseURLs),
		capabilities:    p.Capabilities.Clone(),
		signatureScheme: p.SignatureScheme,
		status:          status,
		costModel:       p.CostModel,
		createdAt:       now,
		updatedAt:       now,
		version:         1,
	}, nil
}

func copyURLs(in map[shared.Environment]string) map[shared.Environment]string {
	out := make(map[shared.Environment]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Accessors. The fields are unexported so that no code outside this package can register a
// capability set that Validate would have refused.

func (g *Gateway) ID() shared.GatewayID             { return g.id }
func (g *Gateway) DisplayName() string              { return g.displayName }
func (g *Gateway) Vendor() string                   { return g.vendor }
func (g *Gateway) APIVersion() string               { return g.apiVersion }
func (g *Gateway) SignatureScheme() SignatureScheme { return g.signatureScheme }
func (g *Gateway) Status() Status                   { return g.status }
func (g *Gateway) CostModel() CostModel             { return g.costModel }
func (g *Gateway) CreatedAt() time.Time             { return g.createdAt }
func (g *Gateway) UpdatedAt() time.Time             { return g.updatedAt }
func (g *Gateway) Version() shared.Version          { return g.version }

// Capabilities returns a deep copy, so a caller cannot edit the licensed country set in place.
func (g *Gateway) Capabilities() Capabilities { return g.capabilities.Clone() }

// BaseURLs returns a copy of the per-environment endpoint map.
func (g *Gateway) BaseURLs() map[shared.Environment]string { return copyURLs(g.baseURLs) }

// BaseURL returns the endpoint for one environment.
//
// A miss is an error, not an empty string. An empty base URL concatenated with a path produces a
// relative request that some HTTP clients will happily resolve against something unexpected;
// failing here means a gateway registered for sandbox only can never be dispatched to in
// production, which is the entire reason the map is keyed by environment.
func (g *Gateway) BaseURL(env shared.Environment) (string, error) {
	u, ok := g.baseURLs[env]
	if !ok || u == "" {
		return "", apierror.Newf(apierror.CodeGatewayNotConfigured,
			"gateway %s has no base URL configured for the %s environment", g.id, env).
			WithDetail(apierror.Detail{
				Field: "environment", Code: "NO_BASE_URL",
				Message: "this gateway is not registered for the requested environment",
				RuleID:  "L4.GATEWAY_BASE_URL_REQUIRED",
			})
	}
	return u, nil
}

// Supports is the gateway-level eligibility check: the registry status first, then the
// capability dimensions.
//
// Status is checked first because a DISABLED gateway's capabilities are irrelevant and reporting
// "adyen does not support BRL" for a gateway that is switched off would send an operator looking
// in the wrong place.
func (g *Gateway) Supports(country shared.Country, currency money.Currency, method shared.PaymentMethod, op shared.Operation) error {
	if !g.status.AcceptsNewTraffic() {
		return apierror.Newf(apierror.CodeGatewayNotConfigured,
			"gateway %s is %s and is not accepting new traffic", g.id, g.status).
			WithDetail(apierror.Detail{
				Field: "gateway", Code: "GATEWAY_NOT_ACCEPTING_TRAFFIC",
				Message: "the gateway integration is disabled in the registry",
				RuleID:  "L5.GATEWAY_ACCEPTS_TRAFFIC",
			})
	}
	return g.capabilities.Supports(country, currency, method, op)
}

// CanRefundAfter reports whether a refund d after capture is within the gateway's window.
func (g *Gateway) CanRefundAfter(d time.Duration) bool { return g.capabilities.CanRefundAfter(d) }

// AmountWithinBounds checks the amount against this gateway's per-currency bounds.
func (g *Gateway) AmountWithinBounds(amount money.Money) error {
	return g.capabilities.AmountWithinBounds(amount)
}

// EstimateCost returns the expected fee, which the routing score consumes as its cost term.
func (g *Gateway) EstimateCost(amount money.Money, method shared.PaymentMethod) (money.Money, error) {
	return g.costModel.EstimateCost(amount, method)
}

// UpdateCapabilities replaces the capability set after re-validating it. Used when a vendor
// enables a country or a method for us; it bumps the version so a concurrent edit is rejected
// rather than merged.
func (g *Gateway) UpdateCapabilities(c Capabilities, clock shared.Clock) error {
	if err := c.Validate(); err != nil {
		return err
	}
	g.capabilities = c.Clone()
	g.touch(clock.Now())
	return nil
}

// SetStatus moves the registry status. Any status may follow any other: unlike the connection
// and health machines, this is an operator's switch with no ordering to enforce — re-enabling a
// disabled gateway is exactly as legitimate as disabling an active one, and a transition table
// here would encode ceremony rather than an invariant.
func (g *Gateway) SetStatus(s Status, clock shared.Clock) error {
	if !s.IsValid() {
		return apierror.Newf(apierror.CodeValidationFailed, "unknown gateway status %q", s)
	}
	g.status = s
	g.touch(clock.Now())
	return nil
}

// SetCostModel replaces the price list, typically after a commercial renegotiation.
func (g *Gateway) SetCostModel(m CostModel, clock shared.Clock) {
	g.costModel = m
	g.touch(clock.Now())
}

func (g *Gateway) touch(now time.Time) {
	g.updatedAt = now
	g.version = g.version.Next()
}

// RehydrateGatewayParams carries the persisted state of a Gateway back into the aggregate.
type RehydrateGatewayParams struct {
	ID              shared.GatewayID
	DisplayName     string
	Vendor          string
	APIVersion      string
	BaseURLs        map[shared.Environment]string
	Capabilities    Capabilities
	SignatureScheme SignatureScheme
	Status          Status
	CostModel       CostModel
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Version         shared.Version
}

// RehydrateGateway reconstructs a Gateway from persisted state.
//
// It validates the status rather than trusting the row, for the same reason the aggregates in
// BC-6 do: a status this binary does not recognise means a rollback landed on data written by a
// newer version, and a registry entry silently coerced to ACTIVE would put traffic on an
// integration somebody deliberately switched off.
func RehydrateGateway(p RehydrateGatewayParams) (*Gateway, error) {
	if !p.Status.IsValid() {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"gateway %s has unknown status %q; this row may have been written by a newer version of the service",
			p.ID, p.Status)
	}
	if !p.SignatureScheme.IsValid() {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"gateway %s has unknown webhook signature scheme %q", p.ID, p.SignatureScheme)
	}
	return &Gateway{
		id: p.ID, displayName: p.DisplayName, vendor: p.Vendor, apiVersion: p.APIVersion,
		baseURLs: copyURLs(p.BaseURLs), capabilities: p.Capabilities.Clone(),
		signatureScheme: p.SignatureScheme, status: p.Status, costModel: p.CostModel,
		createdAt: p.CreatedAt, updatedAt: p.UpdatedAt, version: p.Version,
	}, nil
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-34, FR-33, FR-34.
//
// The gateway catalogue entry and its capability descriptor, which is what makes routing a
// decision over declared facts rather than over a hand-maintained list
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
