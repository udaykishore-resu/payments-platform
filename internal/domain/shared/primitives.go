package shared

import (
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Clock abstracts time so that aggregates and workflows are testable without sleeping.
//
// It is an interface in the domain layer rather than a call to time.Now() scattered through
// the code because a payment platform is full of time-dependent business rules — authorization
// expiry, refund windows, idempotency leases, signature freshness, velocity windows — and
// every one of those rules needs a deterministic test. A test that has to wait 180 days to
// check the refund window is a test nobody runs.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production implementation. It returns UTC exclusively: every timestamp
// that crosses a boundary in this platform is UTC, and local time appears only at the
// presentation edge.
type SystemClock struct{}

// Now returns the current UTC time.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// FixedClock is a deterministic clock for tests. It is in the domain package rather than a
// test helper because workflow definitions and validation rules take a Clock in their
// constructors, and those live in non-test code.
type FixedClock struct{ T time.Time }

// Now returns the fixed instant.
func (c FixedClock) Now() time.Time { return c.T.UTC() }

// Advance moves the fixed clock forward and returns the new value, so a test can express
// "180 days and one second later" without a sleep.
func (c *FixedClock) Advance(d time.Duration) time.Time {
	c.T = c.T.Add(d)
	return c.T
}

// Country is an ISO 3166-1 alpha-2 code, uppercase.
//
// Country is load-bearing in three separate ways here, which is why it is a validated type
// rather than a string: it drives gateway eligibility (a gateway is licensed in a set of
// countries), it drives compliance (sanctions, data residency), and it drives risk scoring.
type Country string

// countries is the ISO 3166-1 alpha-2 set. Kept as a membership set rather than a full table
// because the only question the domain asks is "is this a real country code".
var countries = map[Country]struct{}{}

func init() {
	const list = "AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ " +
		"CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR " +
		"GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP " +
		"KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT " +
		"MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW " +
		"SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG " +
		"UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW"
	for _, c := range strings.Fields(list) {
		countries[Country(c)] = struct{}{}
	}
}

// ParseCountry validates and normalises a country code.
func ParseCountry(s string) (Country, error) {
	c := Country(strings.ToUpper(strings.TrimSpace(s)))
	if _, ok := countries[c]; !ok {
		return "", apierror.Newf(apierror.CodeValidationFailed, "unknown country code %q", s).
			WithDetail(apierror.Detail{
				Field:   "country",
				Code:    "UNKNOWN_COUNTRY",
				Message: "must be a valid ISO 3166-1 alpha-2 code",
				RuleID:  "L2.COUNTRY_IS_VALID_ISO",
			})
	}
	return c, nil
}

// IsValid reports whether c is a known ISO 3166-1 alpha-2 code.
func (c Country) IsValid() bool { _, ok := countries[c]; return ok }

// String satisfies fmt.Stringer.
func (c Country) String() string { return string(c) }

// eeaCountries is the European Economic Area plus the United Kingdom, which is the set where
// PSD2-derived strong customer authentication rules apply to the platform's traffic. The UK is
// included because UK-implemented SCA survived its departure from the EU; treating the UK as
// out of scope is a compliance defect, not a simplification.
var eeaCountries = map[Country]struct{}{}

func init() {
	const list = "AT BE BG HR CY CZ DK EE FI FR DE GR HU IS IE IT LV LI LT LU MT NL NO PL PT RO SK SI ES SE GB"
	for _, c := range strings.Fields(list) {
		eeaCountries[Country(c)] = struct{}{}
	}
}

// IsSCAJurisdiction reports whether strong-customer-authentication rules apply to transactions
// in this country. Used by the risk engine to decide whether 3DS is a regulatory requirement
// rather than merely a risk-reduction choice.
func (c Country) IsSCAJurisdiction() bool { _, ok := eeaCountries[c]; return ok }

// MCC is a four-digit ISO 18245 Merchant Category Code. It determines interchange, gateway
// eligibility, and whether a merchant falls into a prohibited or high-risk category.
type MCC string

// ParseMCC validates a merchant category code.
func ParseMCC(s string) (MCC, error) {
	s = strings.TrimSpace(s)
	if len(s) != 4 {
		return "", mccErr(s, "must be exactly four digits")
	}
	for i := 0; i < 4; i++ {
		if s[i] < '0' || s[i] > '9' {
			return "", mccErr(s, "must contain only digits")
		}
	}
	return MCC(s), nil
}

func mccErr(s, why string) error {
	return apierror.Newf(apierror.CodeValidationFailed, "invalid merchant category code %q", s).
		WithDetail(apierror.Detail{
			Field: "mcc", Code: "INVALID_MCC", Message: why, RuleID: "L2.MCC_WELL_FORMED",
		})
}

// prohibitedMCCs are categories the platform will not onboard under any tenant. This is a
// business and licensing constraint, not a technical one: acquiring relationships forbid them,
// and processing them puts the platform's own registrations at risk.
var prohibitedMCCs = map[MCC]string{
	"5967": "inbound teleservices / adult",
	"7273": "dating and escort services",
	"7995": "gambling and betting transactions",
	"6051": "quasi-cash and cryptocurrency",
	"5122": "drugs and pharmaceutical proprietaries",
	"5912": "drug stores and pharmacies requiring specific licensing",
	"5993": "cigar stores and stands",
	"7994": "video game arcades and gambling-adjacent establishments",
}

// IsProhibited reports whether the category may not be onboarded, and why.
func (m MCC) IsProhibited() (bool, string) {
	reason, ok := prohibitedMCCs[m]
	return ok, reason
}

// PaymentMethod enumerates the tender types the platform orchestrates.
//
// These are deliberately coarse. A gateway's own taxonomy is far more granular (Stripe alone
// has dozens of `payment_method_types`), and mapping that granularity into the core domain
// would couple us to whichever gateway happened to be integrated first. The adapters translate;
// the core reasons about the categories that actually change platform behaviour — whether the
// method supports authorization-then-capture, whether it settles asynchronously, whether it is
// subject to SCA.
type PaymentMethod string

const (
	MethodCard       PaymentMethod = "CARD"
	MethodApplePay   PaymentMethod = "APPLE_PAY"
	MethodGooglePay  PaymentMethod = "GOOGLE_PAY"
	MethodPayPal     PaymentMethod = "PAYPAL"
	MethodSEPADebit  PaymentMethod = "SEPA_DEBIT"
	MethodACHDebit   PaymentMethod = "ACH_DEBIT"
	MethodIdeal      PaymentMethod = "IDEAL"
	MethodSofort     PaymentMethod = "SOFORT"
	MethodBancontact PaymentMethod = "BANCONTACT"
	MethodUPI        PaymentMethod = "UPI"
	MethodBLIK       PaymentMethod = "BLIK"
)

var paymentMethods = map[PaymentMethod]methodTraits{
	MethodCard:       {separateCapture: true, async: false, sca: true, refundable: true},
	MethodApplePay:   {separateCapture: true, async: false, sca: true, refundable: true},
	MethodGooglePay:  {separateCapture: true, async: false, sca: true, refundable: true},
	MethodPayPal:     {separateCapture: true, async: false, sca: false, refundable: true},
	MethodSEPADebit:  {separateCapture: false, async: true, sca: false, refundable: true},
	MethodACHDebit:   {separateCapture: false, async: true, sca: false, refundable: true},
	MethodIdeal:      {separateCapture: false, async: false, sca: false, refundable: true},
	MethodSofort:     {separateCapture: false, async: true, sca: false, refundable: true},
	MethodBancontact: {separateCapture: false, async: false, sca: false, refundable: true},
	MethodUPI:        {separateCapture: false, async: false, sca: false, refundable: true},
	MethodBLIK:       {separateCapture: false, async: false, sca: false, refundable: true},
}

type methodTraits struct {
	separateCapture bool // supports authorize now, capture later
	async           bool // final outcome may arrive minutes to days later
	sca             bool // subject to strong customer authentication where the jurisdiction requires it
	refundable      bool
}

// ParsePaymentMethod validates a payment method.
func ParsePaymentMethod(s string) (PaymentMethod, error) {
	m := PaymentMethod(strings.ToUpper(strings.TrimSpace(s)))
	if _, ok := paymentMethods[m]; !ok {
		return "", apierror.Newf(apierror.CodeValidationFailed, "unsupported payment method %q", s).
			WithDetail(apierror.Detail{
				Field: "paymentMethod", Code: "UNKNOWN_PAYMENT_METHOD",
				Message: "must be one of the platform's supported payment methods",
				RuleID:  "L1.PAYMENT_METHOD_KNOWN",
			})
	}
	return m, nil
}

// IsValid reports whether m is a known method.
func (m PaymentMethod) IsValid() bool { _, ok := paymentMethods[m]; return ok }

// String satisfies fmt.Stringer.
func (m PaymentMethod) String() string { return string(m) }

// SupportsSeparateCapture reports whether the method can be authorized now and captured later.
// A capture request against a method that cannot do this is a business-rule error, not a
// gateway error, and is rejected before dispatch.
func (m PaymentMethod) SupportsSeparateCapture() bool { return paymentMethods[m].separateCapture }

// IsAsynchronous reports whether the final outcome may arrive well after the API response.
// Asynchronous methods legitimately sit in PENDING; synchronous ones sitting in PENDING are a
// reconciliation signal.
func (m PaymentMethod) IsAsynchronous() bool { return paymentMethods[m].async }

// RequiresSCAConsideration reports whether the method is in scope for strong customer
// authentication where the jurisdiction requires it.
func (m PaymentMethod) RequiresSCAConsideration() bool { return paymentMethods[m].sca }

// IsRefundable reports whether the method supports refunds through the platform.
func (m PaymentMethod) IsRefundable() bool { return paymentMethods[m].refundable }

// AllPaymentMethods returns the supported set, sorted, for configuration validation and for
// generating the OpenAPI enum.
func AllPaymentMethods() []PaymentMethod {
	out := make([]PaymentMethod, 0, len(paymentMethods))
	for m := range paymentMethods {
		out = append(out, m)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Operation names a gateway operation. It is used as a circuit-breaker key, a bulkhead key and
// a metric label, so the set is closed and small.
type Operation string

const (
	OpAuthorize Operation = "authorize"
	OpCapture   Operation = "capture"
	OpRefund    Operation = "refund"
	OpVoid      Operation = "void"
	OpLookup    Operation = "lookup"
	OpProvision Operation = "provision"
	OpWebhook   Operation = "webhook_register"
)

// String satisfies fmt.Stringer.
func (o Operation) String() string { return string(o) }

// IsMoneyMoving reports whether a failure of this operation could leave money in an ambiguous
// state. Money-moving operations are the ones that must never be blindly retried after an
// unknown outcome (baseline §12.3).
func (o Operation) IsMoneyMoving() bool {
	switch o {
	case OpAuthorize, OpCapture, OpRefund, OpVoid:
		return true
	default:
		return false
	}
}
