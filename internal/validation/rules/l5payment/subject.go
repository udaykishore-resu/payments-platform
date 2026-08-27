// Package l5payment is validation level 5: everything asserted about a payment operation
// before it is dispatched to a gateway.
//
// L5 is the hot path, and every design decision here follows from one number: a 5 ms budget at
// stage 10 of the request pipeline. That budget is only achievable because L5 reads inputs
// rather than fetching them. The merchant snapshot, the configuration snapshot, the velocity
// counters, the idempotency record and the risk score were all read once, before evaluation
// started, in a small number of pipelined calls. Every rule in this package is therefore pure:
// same subject, same report, no clock read, no Redis call, no control-plane lookup.
//
// The subject is a snapshot, not a set of live handles, and that is what makes a rejection
// reproducible. It carries the configuration *version* and the merchant state *as read*, so
// re-running the report against the persisted subject in six months produces the same answer
// even though the merchant has since been suspended and the configuration published four more
// times.
//
// Mode is CollectAll. An integrator whose request fails on currency, amount and idempotency
// key should learn all three at once.
//
// See docs/validation-plane.md §3.5 and §2.3.
package l5payment

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Operation is the payment operation being validated.
//
// A separate type from shared.Operation because that one names *gateway* operations
// (authorize, capture, refund, void, lookup) while this one names *API* operations, and CREATE
// has no gateway equivalent: it is the request that decides whether the gateway call will be
// an authorization or an auto-capture.
type Operation string

// The API operations L5 validates.
const (
	OpCreate  Operation = "CREATE"
	OpCapture Operation = "CAPTURE"
	OpRefund  Operation = "REFUND"
	OpVoid    Operation = "VOID"
)

// String satisfies fmt.Stringer.
func (o Operation) String() string { return string(o) }

// TokenMeta is the stored metadata of a payment-method token. Never the instrument itself:
// this platform accepts tokens and nothing else, so there is no field here for a card number
// and no code path that could populate one.
type TokenMeta struct {
	Present         bool
	OwnerMerchantID shared.MerchantID
	ExpiresAt       time.Time
	CardExpiryMonth int
	CardExpiryYear  int
	// Fingerprint is the stable, non-reversible instrument identifier the velocity counters are
	// keyed on.
	Fingerprint string
}

// Request is the L1-clean payment request.
type Request struct {
	Amount money.Money
	Method shared.PaymentMethod
	// MinorDigitsSupplied is how many decimal places the caller's original representation used.
	// It is carried from the decoder because "1050" and "10.50" are the same money and a
	// different bug: a caller sending 10.50 for JPY has misunderstood the currency's exponent
	// and is about to be off by a factor of a hundred.
	MinorDigitsSupplied  int
	CustomerCountry      shared.Country
	IPCountry            shared.Country
	TokenRef             string
	Token                TokenMeta
	CaptureMode          payment.CaptureMethod
	StatementDescriptor  string
	Metadata             map[string]string
	CustomerRef          string
	IdempotencyKey       string
	MerchantInitiated    bool
	InitialTransactionID string
	Recurring            bool
	MandateRef           string
	MandateActive        bool
	// ClaimedSCAExemption is the exemption the caller asserts, if any.
	ClaimedSCAExemption string
	// SCAExemptionPreconditionsHold is the acquirer-side evaluation of whether the claimed
	// exemption's conditions are actually met (consecutive count, cumulative amount, acquirer
	// fraud-rate band). Computed before evaluation because it depends on counters.
	SCAExemptionPreconditionsHold bool
	// ThreeDSCompleted records that the payer already completed authentication.
	ThreeDSCompleted bool
}

// MerchantSnapshot is the merchant registry read, as of the snapshot instant.
type MerchantSnapshot struct {
	Found  bool
	ID     shared.MerchantID
	Status merchant.Status
}

// RouteCandidate is one compiled (method, currency, country) combination the configuration can
// route. L4 computed these at publish time; L5 counts matches.
type RouteCandidate struct {
	Method   shared.PaymentMethod
	Currency money.Currency
	Country  shared.Country
}

// ConfigSnapshot is the merchant's published configuration as cached in the data plane.
type ConfigSnapshot struct {
	Present bool
	// Version and Age make the report reproducible and let the staleness rule fail closed.
	Version int64
	Age     time.Duration

	Currencies       []money.Currency
	Methods          []shared.PaymentMethod
	Countries        []shared.Country
	BlockedCountries []shared.Country

	MaxTransactionAmount money.Money
	Require3DSAbove      money.Money
	DailyVolumeLimit     money.Money

	MaxRefundWindowDays int
	MaxPartialCaptures  int

	MaxPaymentsPerMinute int
	MaxPerCardPerHour    int
	MaxPerCustomerPerDay int
	MaxDistinctCards     int

	// ManualCaptureAllowed is the configuration's answer; the candidate descriptors' answer is
	// folded in by the control plane at publish time.
	ManualCaptureAllowed bool

	// Candidates is the compiled routing table's coverage.
	Candidates []RouteCandidate

	// MerchantBlocklistConfigured records that the merchant maintains a blocklist at all, which
	// is what makes NOT_ON_MERCHANT_BLOCKLIST's precondition meaningful.
	MerchantBlocklistConfigured bool
	// RiskDeclineAt is the merchant's own decline threshold, 0 meaning "use the platform
	// default".
	RiskDeclineAt int
}

// PaymentView is the existing payment, for operations other than CREATE.
type PaymentView struct {
	Found            bool
	State            payment.State
	Currency         money.Currency
	AuthorizedAmount money.Money
	CapturedTotal    money.Money
	RefundedTotal    money.Money
	CaptureCount     int
	AuthorizedAt     time.Time
	CapturedAt       time.Time
	HasOpenDispute   bool
}

// VelocityCounters are the pre-read counters. Reading them once, at stage 9, is what lets the
// five velocity rules stay pure and share a single pipelined Redis round trip.
type VelocityCounters struct {
	CountLastMinute              int
	TodayVolume                  money.Money
	CountForFingerprintLastHour  int
	CountForCustomerToday        int
	DistinctFingerprintsLastHour int
	AttemptsLast15Min            int
	DeclinesLast15Min            int
}

// RiskInputs are the pre-computed risk signals.
type RiskInputs struct {
	Scored              bool
	Score               int
	OnMerchantBlocklist bool
	OnPlatformBlocklist bool
}

// IdempotencyRecord is the stored claim for this request's key.
type IdempotencyRecord struct {
	Exists bool
	// Scope is the stored (tenant, merchant, method, path template) tuple, rendered.
	Scope string
	// Fingerprint is the SHA-256 of the JCS-canonicalized body that first used this key.
	Fingerprint string
	// InFlight and LeaseExpiresAt describe an unfinished claim.
	InFlight          bool
	LeaseExpiresAt    time.Time
	RetryAfterSeconds int
}

// PrincipalView is the authenticated caller's authorization state.
type PrincipalView struct {
	ID     string
	Scopes []string
}

// Subject is everything L5 evaluates: the operation, the request, the snapshots and the
// injected instant.
type Subject struct {
	Op       Operation
	Request  Request
	Merchant MerchantSnapshot
	Config   ConfigSnapshot
	// Payment is nil on CREATE.
	Payment     *PaymentView
	Velocity    VelocityCounters
	Risk        RiskInputs
	Idempotency IdempotencyRecord
	Principal   PrincipalView
	// ExpectedScope is the (tenant, merchant, method, path template) tuple this request would
	// claim, rendered the same way the stored record is.
	ExpectedScope string
	// RequestFingerprint is the SHA-256 of this request's canonicalized body.
	RequestFingerprint string
	// Now is the injected clock reading. No rule calls time.Now.
	Now time.Time
}

// Deps carries the platform and tenant policy L5 needs. Pure data: no repository, no cache
// client, no HTTP client. That constraint is what keeps the level's budget a statement rather
// than an aspiration.
type Deps struct {
	// MaxConfigStaleness is how old a cached configuration snapshot may be (15 min).
	MaxConfigStaleness time.Duration
	// MethodMinimums is the scheme minimum per method, denominated in a currency. A minimum in
	// a currency the request does not use is not applied.
	MethodMinimums map[shared.PaymentMethod]money.Money
	// SanctionedCountries is the platform sanctions set, applied to the resolved IP country.
	SanctionedCountries []shared.Country
	// RiskDeclineAt is the platform default decline threshold (90).
	RiskDeclineAt int
	// ScopeForOperation maps an operation to the OAuth scope it requires.
	ScopeForOperation map[Operation]string
	// ElevatedRefundScope and RefundElevatedThreshold gate large refunds behind a second role.
	ElevatedRefundScope     string
	RefundElevatedThreshold money.Money
	// AuthValidityDays is how long an authorization stays capturable (card default 7).
	AuthValidityDays int
	// CustomerDailyLimit is the default per-customer daily payment count (20).
	CustomerDailyLimit int
	// DistinctCardsPerCustomerHour is the card-testing signal threshold (3).
	DistinctCardsPerCustomerHour int
	// DeclineRatio* configure the merchant-level decline-rate circuit.
	DeclineRatioMinAttempts int
	DeclineRatioPercent     int
	// ClaimableExemptions is the closed set of SCA exemptions the platform will forward.
	ClaimableExemptions []string
	// LowValueCeiling is the PSD2 low-value exemption ceiling (EUR 30).
	LowValueCeiling money.Money
}

// DefaultDeps returns the platform defaults from docs/validation-plane.md §3.5.
func DefaultDeps() Deps {
	return Deps{
		MaxConfigStaleness: 15 * time.Minute,
		RiskDeclineAt:      90,
		ScopeForOperation: map[Operation]string{
			OpCreate:  "payments:write",
			OpCapture: "payments:capture",
			OpRefund:  "payments:refund",
			OpVoid:    "payments:void",
		},
		ElevatedRefundScope:          "payments:refund:elevated",
		AuthValidityDays:             7,
		CustomerDailyLimit:           20,
		DistinctCardsPerCustomerHour: 3,
		DeclineRatioMinAttempts:      20,
		DeclineRatioPercent:          60,
		ClaimableExemptions:          []string{"LOW_VALUE", "TRA", "MIT", "RECURRING", "CORPORATE"},
	}
}
