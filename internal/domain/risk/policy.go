package risk

import (
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// FailurePosture is what a check does when the data it needs is unavailable.
//
// This type exists because "fail open" and "fail closed" are the two answers everyone reaches
// for and both of them are wrong most of the time. Fail open on a velocity counter during a
// Redis blip means a card tester gets an unbounded window. Fail closed on the same counter
// means a five-minute cache outage becomes a total payment outage for every merchant on the
// platform — a self-inflicted incident strictly worse than the fraud it prevented.
//
// The right answer for most checks is neither: it is to convert the lost signal into friction.
// REQUIRE_3DS keeps money moving, shifts chargeback liability to the issuer, and costs the
// merchant some conversion rather than their entire revenue for the duration of the outage. That
// is why it is the documented default, and why FAIL_OPEN exists as an explicit, named,
// deliberately awkward choice rather than as the behaviour you get by forgetting to configure
// something.
type FailurePosture string

const (
	// PostureFailClosed declines when the check's data is unavailable. Correct where the
	// consequence of being wrong is not survivable: sanctions, and contractual money limits.
	PostureFailClosed FailurePosture = "FAIL_CLOSED"
	// PostureRequire3DS proceeds with forced authentication. The default for count-based checks.
	PostureRequire3DS FailurePosture = "REQUIRE_3DS"
	// PostureReview holds for a human. Correct for a low-volume merchant with a review team;
	// a queue nobody staffs is just a slower decline.
	PostureReview FailurePosture = "REVIEW"
	// PostureFailOpen approves as if the check had passed. Legitimate only for genuinely
	// additive signals — a BIN lookup, a device fingerprint — where the absence of the signal
	// means the score is computed without it rather than the check being skipped. Configuring
	// it for a velocity counter is how a platform discovers card testing from its chargeback
	// statement.
	PostureFailOpen FailurePosture = "FAIL_OPEN"
)

// AllFailurePostures is the complete posture universe.
var AllFailurePostures = []FailurePosture{
	PostureFailClosed, PostureFailOpen, PostureRequire3DS, PostureReview,
}

// IsValid reports whether p is a known posture.
func (p FailurePosture) IsValid() bool {
	switch p {
	case PostureFailClosed, PostureRequire3DS, PostureReview, PostureFailOpen:
		return true
	default:
		return false
	}
}

// String satisfies fmt.Stringer.
func (p FailurePosture) String() string { return string(p) }

// Outcome maps the posture to the verdict it produces.
func (p FailurePosture) Outcome() Outcome {
	switch p {
	case PostureFailClosed:
		return OutcomeDecline
	case PostureReview:
		return OutcomeReview
	case PostureFailOpen:
		return OutcomeApprove
	default:
		return OutcomeRequire3DS
	}
}

// DefaultPostures is the platform's documented per-check fallback behaviour
// (docs/data-plane.md §6.5).
//
// The split it encodes, and the reasoning behind it:
//
//   - Count-based velocity → REQUIRE_3DS. Losing fraud sensitivity for a few minutes is
//     survivable; the friction is applied to the traffic that would have been counted, not to
//     everything.
//   - Money-based daily volume → FAIL_CLOSED. A contractual or regulatory volume limit that is
//     exceeded is a compliance finding, and unlike a count it has a Postgres fallback (a SUM
//     over today's payments, ~8ms) so failing closed is a genuine last resort rather than the
//     first response to a cache miss.
//   - Platform blocklist → REQUIRE_3DS by default, escalated to DECLINE for a high-risk merchant
//     by the engine. The list exists because these are known-bad; treating unknown as clean is
//     how a known fraudster gets through during a blip.
//   - External scorer → REQUIRE_3DS. Never APPROVE: the whole reason the scorer was invoked is
//     that the compiled policy's predicate said this payment warranted a look.
//
// Sanctions has no entry because it cannot be unavailable: the list is compiled in-process from
// the configuration snapshot, and a snapshot stale past the cliff stops new payments entirely
// (baseline §15). A posture for it would imply a degraded mode that does not and must not exist.
func DefaultPostures() map[CheckID]FailurePosture {
	return map[CheckID]FailurePosture{
		CheckPlatformBlocklist:   PostureRequire3DS,
		CheckMerchantBlocklist:   PostureRequire3DS,
		CheckDailyVolume:         PostureFailClosed,
		CheckVelocityPerMinute:   PostureRequire3DS,
		CheckVelocityPerCard:     PostureRequire3DS,
		CheckVelocityPerCustomer: PostureRequire3DS,
		CheckDistinctCards:       PostureRequire3DS,
		CheckRiskScore:           PostureRequire3DS,
	}
}

// Velocity holds the count-based limits from baseline §23.
//
// A zero limit means "unenforced", not "zero permitted". That sentinel choice is deliberate and
// is the one place in this package where a zero value is not the safe value: a merchant who
// does not configure maxPerCardPerHour has not asked for every card to be declined, and
// interpreting an absent field as a limit of zero would decline 100% of their traffic the first
// time the field was omitted from a config write. L4 validation rejects a *negative* limit, so
// the only way to reach here with an unenforced check is to have deliberately left it out.
type Velocity struct {
	// MaxPaymentsPerMinute bounds a merchant's overall rate. The blunt instrument that catches a
	// compromised API key before the per-card checks have enough data to notice.
	MaxPaymentsPerMinute int
	// MaxPerCardPerHour bounds attempts against one card fingerprint. The single most effective
	// card-testing control.
	MaxPerCardPerHour int
	// MaxPerCustomerPerDay bounds one customer's payment count.
	MaxPerCustomerPerDay int
}

// IsUnenforced reports whether no velocity limit is configured at all.
func (v Velocity) IsUnenforced() bool {
	return v.MaxPaymentsPerMinute <= 0 && v.MaxPerCardPerHour <= 0 && v.MaxPerCustomerPerDay <= 0
}

// Score thresholds. Higher is riskier, on 0..100, matching the external scorer's contract
// (docs/data-plane.md §6.6).
const (
	// DefaultDeclineScore is the score at or above which a payment is refused outright.
	DefaultDeclineScore = 90
	// DefaultReviewScore is the score at or above which a payment is held for a human.
	DefaultReviewScore = 75
	// DefaultThreeDSScore is the score at or above which authentication is forced. Set well
	// below the review threshold on purpose: authentication is cheap and shifts liability, so
	// the band where it is the right answer is wide.
	DefaultThreeDSScore = 50
)

// Policy is the merchant's risk configuration from baseline §23, as a domain value.
//
// Like routing.Policy it is a value rather than an aggregate: its identity and lifecycle belong
// to the versioned configuration document that contains it.
type Policy struct {
	// MaxTransactionAmount is the per-payment ceiling. Exceeding it is a decline, never a
	// review: this is a contractual limit, not a risk signal, and there is nothing for a human
	// to decide.
	MaxTransactionAmount money.Money
	// Require3DSAbove is the amount above which the merchant forces authentication regardless of
	// risk signals.
	Require3DSAbove money.Money
	// DailyVolumeLimit is the merchant's money-per-day ceiling.
	DailyVolumeLimit money.Money

	// Velocity holds the count-based limits.
	Velocity Velocity

	// BlockedCountries is the merchant's blocked set, unioned by the compiler with the
	// platform's mandatory sanctions list. It is checked before the allowlist so a merchant
	// cannot allowlist their way past a sanction.
	BlockedCountries []shared.Country
	// AllowedCountries is a positive allowlist. Empty means "all countries", which is the
	// overwhelmingly common configuration; a merchant who wants to sell only in three countries
	// declares those three.
	AllowedCountries []shared.Country

	// MaxCardsPerCustomerPerDay bounds distinct card fingerprints per customer. Zero is
	// unenforced. This is the card-testing check that survives an attacker rotating customer
	// identifiers less often than they rotate cards.
	MaxCardsPerCustomerPerDay int

	// DeclineScoreAtOrAbove, ReviewScoreAtOrAbove and ThreeDSScoreAtOrAbove are the external
	// scorer thresholds. All three zero means "unconfigured" and the defaults above apply; see
	// Thresholds.
	DeclineScoreAtOrAbove int
	ReviewScoreAtOrAbove  int
	ThreeDSScoreAtOrAbove int

	// Postures is the per-check fallback behaviour. Entries absent from the map fall back to
	// DefaultPostures, so a merchant configures only the checks whose posture they want to
	// differ from the platform's.
	Postures map[CheckID]FailurePosture

	// Version identifies the compiled configuration version this policy came from. Stamped onto
	// every Decision so a replay can use the policy that was actually in force.
	Version int
}

// Thresholds returns the effective score thresholds, substituting the platform defaults when
// none were configured.
//
// "All three zero" is the unconfigured marker rather than each field individually, because a
// merchant who sets only the decline threshold means "decline at 95, use the platform defaults
// for the rest" — and interpreting their unset review threshold as zero would send every
// payment to manual review.
func (p Policy) Thresholds() (decline, review, threeDS int) {
	if p.DeclineScoreAtOrAbove == 0 && p.ReviewScoreAtOrAbove == 0 && p.ThreeDSScoreAtOrAbove == 0 {
		return DefaultDeclineScore, DefaultReviewScore, DefaultThreeDSScore
	}
	decline, review, threeDS = p.DeclineScoreAtOrAbove, p.ReviewScoreAtOrAbove, p.ThreeDSScoreAtOrAbove
	if decline == 0 {
		decline = DefaultDeclineScore
	}
	if review == 0 {
		review = DefaultReviewScore
	}
	if threeDS == 0 {
		threeDS = DefaultThreeDSScore
	}
	return decline, review, threeDS
}

// PostureFor returns the configured posture for a check, or the platform default.
//
// It never returns the zero FailurePosture. A check that consulted an unset map entry and got
// "" would fall through every switch in the engine to whatever the default branch happened to
// be, and "whatever the default branch happened to be" is not a posture anyone signed off on.
func (p Policy) PostureFor(c CheckID) FailurePosture {
	if got, ok := p.Postures[c]; ok && got.IsValid() {
		return got
	}
	if def, ok := DefaultPostures()[c]; ok {
		return def
	}
	return PostureRequire3DS
}

// IsCountryBlocked reports whether the country is on the merchant's blocked list.
func (p Policy) IsCountryBlocked(c shared.Country) bool {
	for _, b := range p.BlockedCountries {
		if b == c {
			return true
		}
	}
	return false
}

// IsCountryAllowed reports whether the country passes the positive allowlist. An empty allowlist
// permits everything.
func (p Policy) IsCountryAllowed(c shared.Country) bool {
	if len(p.AllowedCountries) == 0 {
		return true
	}
	for _, a := range p.AllowedCountries {
		if a == c {
			return true
		}
	}
	return false
}

// Currency returns the currency the policy's money limits are denominated in.
func (p Policy) Currency() money.Currency { return p.MaxTransactionAmount.Currency() }

// Validate is the L4 configuration-validation entry point for the risk block.
//
// Like routing.Policy.Validate it reports every problem at once, with the validation-plane rule
// ID on each detail. The checks that matter most are the ordering ones: a Require3DSAbove above
// MaxTransactionAmount is a threshold that can never fire, and a DailyVolumeLimit below
// MaxTransactionAmount is a configuration in which a single permitted payment breaches the daily
// limit. Both publish cleanly and both are wrong in a way nobody notices until a real payment
// hits them.
func (p Policy) Validate() error {
	var details []apierror.Detail

	limits := []struct {
		field string
		value money.Money
		rule  string
	}{
		{"risk.maxTransactionAmount", p.MaxTransactionAmount, "L4.RISK_LIMIT_CURRENCY_SUPPORTED"},
		{"risk.require3DSAbove", p.Require3DSAbove, "L4.RISK_LIMIT_CURRENCY_SUPPORTED"},
		{"risk.dailyVolumeLimit", p.DailyVolumeLimit, "L4.RISK_LIMIT_CURRENCY_SUPPORTED"},
	}
	for _, l := range limits {
		if !l.value.IsValid() {
			details = append(details, apierror.Detail{
				Field: l.field, Code: "UNKNOWN_CURRENCY",
				Message: "must carry a supported ISO 4217 currency code",
				RuleID:  l.rule,
			})
			continue
		}
		if l.value.IsNegative() {
			details = append(details, apierror.Detail{
				Field: l.field, Code: "NEGATIVE_LIMIT",
				Message: "a risk limit may not be negative",
				RuleID:  "L4.MONEY_IS_MINOR_UNITS",
			})
		}
	}

	// All three limits must share a currency. Comparing an amount against a limit in another
	// currency is not a comparison, and there is no exchange rate in this domain on purpose:
	// FX is a business decision with a rate source and an audit trail, not something a risk
	// check should improvise.
	if p.MaxTransactionAmount.IsValid() {
		for _, l := range limits[1:] {
			if l.value.IsValid() && l.value.Currency() != p.MaxTransactionAmount.Currency() {
				details = append(details, apierror.Detail{
					Field: l.field, Code: "CURRENCY_MISMATCH",
					Message: "every risk limit must be denominated in the same currency as maxTransactionAmount (" +
						p.MaxTransactionAmount.Currency().String() + ")",
					RuleID: "L4.RISK_CURRENCY_CONSISTENT",
				})
			}
		}
	}

	if !p.MaxTransactionAmount.IsPositive() && p.MaxTransactionAmount.IsValid() {
		details = append(details, apierror.Detail{
			Field: "risk.maxTransactionAmount", Code: "NOT_POSITIVE",
			Message: "maxTransactionAmount must be greater than zero; a zero ceiling declines every payment",
			RuleID:  "L4.RISK_LIMIT_ORDERING",
		})
	}

	if cmp, ok := compareAmounts(p.Require3DSAbove, p.MaxTransactionAmount); ok && p.Require3DSAbove.IsPositive() && cmp > 0 {
		details = append(details, apierror.Detail{
			Field: "risk.require3DSAbove", Code: "THRESHOLD_ABOVE_CEILING",
			Message: "require3DSAbove (" + p.Require3DSAbove.String() + ") exceeds maxTransactionAmount (" +
				p.MaxTransactionAmount.String() + "), so the 3DS threshold can never be reached",
			RuleID: "L4.THREEDS_THRESHOLD_BELOW_MAX_AMOUNT",
		})
	}

	if cmp, ok := compareAmounts(p.DailyVolumeLimit, p.MaxTransactionAmount); ok && p.DailyVolumeLimit.IsPositive() && cmp < 0 {
		details = append(details, apierror.Detail{
			Field: "risk.dailyVolumeLimit", Code: "DAILY_LIMIT_BELOW_TRANSACTION_LIMIT",
			Message: "dailyVolumeLimit (" + p.DailyVolumeLimit.String() + ") is below maxTransactionAmount (" +
				p.MaxTransactionAmount.String() + "), so a single permitted payment breaches the daily limit",
			RuleID: "L4.DAILY_LIMIT_AT_LEAST_MAX_TRANSACTION",
		})
	}

	negVelocity := func(field string, v int) {
		if v < 0 {
			details = append(details, apierror.Detail{
				Field: "risk.velocity." + field, Code: "NEGATIVE_LIMIT",
				Message: "a velocity limit may not be negative; omit the field to leave the check unenforced",
				RuleID:  "L4.VELOCITY_LIMITS_POSITIVE",
			})
		}
	}
	negVelocity("maxPaymentsPerMinute", p.Velocity.MaxPaymentsPerMinute)
	negVelocity("maxPerCardPerHour", p.Velocity.MaxPerCardPerHour)
	negVelocity("maxPerCustomerPerDay", p.Velocity.MaxPerCustomerPerDay)
	if p.MaxCardsPerCustomerPerDay < 0 {
		details = append(details, apierror.Detail{
			Field: "risk.maxCardsPerCustomerPerDay", Code: "NEGATIVE_LIMIT",
			Message: "a velocity limit may not be negative; omit the field to leave the check unenforced",
			RuleID:  "L4.VELOCITY_LIMITS_POSITIVE",
		})
	}

	blocked := make(map[shared.Country]struct{}, len(p.BlockedCountries))
	for _, c := range p.BlockedCountries {
		if !c.IsValid() {
			details = append(details, apierror.Detail{
				Field: "risk.blockedCountries", Code: "UNKNOWN_COUNTRY",
				Message: "\"" + c.String() + "\" is not a valid ISO 3166-1 alpha-2 code",
				RuleID:  "L4.BLOCKED_COUNTRIES_VALID",
			})
		}
		blocked[c] = struct{}{}
	}
	for _, c := range p.AllowedCountries {
		if !c.IsValid() {
			details = append(details, apierror.Detail{
				Field: "risk.allowedCountries", Code: "UNKNOWN_COUNTRY",
				Message: "\"" + c.String() + "\" is not a valid ISO 3166-1 alpha-2 code",
				RuleID:  "L4.COUNTRIES_ARE_ISO3166",
			})
		}
		if _, isBlocked := blocked[c]; isBlocked {
			details = append(details, apierror.Detail{
				Field: "risk.allowedCountries", Code: "COUNTRY_BOTH_ALLOWED_AND_BLOCKED",
				Message: "\"" + c.String() + "\" appears in both allowedCountries and blockedCountries; " +
					"the block wins, so the allowlist entry is misleading",
				RuleID: "L4.BLOCKED_COUNTRIES_DISJOINT",
			})
		}
	}

	for check, posture := range p.Postures {
		if !check.IsValid() {
			details = append(details, apierror.Detail{
				Field: "risk.failurePosture", Code: "UNKNOWN_CHECK",
				Message: "\"" + check.String() + "\" is not a check this binary knows about",
				RuleID:  "L4.RISK_FAILURE_POSTURE_KNOWN",
			})
		}
		if !posture.IsValid() {
			details = append(details, apierror.Detail{
				Field: "risk.failurePosture." + string(check), Code: "UNKNOWN_POSTURE",
				Message: "must be one of FAIL_CLOSED, REQUIRE_3DS, REVIEW, FAIL_OPEN",
				RuleID:  "L4.RISK_FAILURE_POSTURE_KNOWN",
			})
		}
		// Sanctions cannot be unavailable, so declaring a posture for it describes a degraded
		// mode that does not exist. Rejecting it stops an operator from believing they have
		// configured a safety net that will never be used.
		if check == CheckSanctionedCountry {
			details = append(details, apierror.Detail{
				Field: "risk.failurePosture." + string(check), Code: "POSTURE_NOT_APPLICABLE",
				Message: "the sanctions list is compiled in-process and cannot be unavailable; it has no failure posture",
				RuleID:  "L4.RISK_FAILURE_POSTURE_KNOWN",
			})
		}
	}

	decline, review, threeDS := p.Thresholds()
	for _, t := range []struct {
		field string
		value int
	}{
		{"risk.declineScoreAtOrAbove", decline},
		{"risk.reviewScoreAtOrAbove", review},
		{"risk.threeDSScoreAtOrAbove", threeDS},
	} {
		if t.value < 0 || t.value > 100 {
			details = append(details, apierror.Detail{
				Field: t.field, Code: "SCORE_OUT_OF_RANGE",
				Message: "a risk score threshold must be on 0..100",
				RuleID:  "L4.RISK_SCORE_THRESHOLDS_ORDERED",
			})
		}
	}
	if decline < review || review < threeDS {
		details = append(details, apierror.Detail{
			Field: "risk", Code: "SCORE_THRESHOLDS_OUT_OF_ORDER",
			Message: "thresholds must satisfy declineScoreAtOrAbove >= reviewScoreAtOrAbove >= threeDSScoreAtOrAbove; " +
				"out of order, the stricter threshold shadows the looser one and the looser one never fires",
			RuleID: "L4.RISK_SCORE_THRESHOLDS_ORDERED",
		})
	}

	if len(details) == 0 {
		return nil
	}
	return apierror.New(apierror.CodeConfigurationInvalid,
		"the risk configuration is not valid").WithDetails(details...)
}

// compareAmounts compares two amounts, reporting false when the comparison is not possible
// because the currencies differ or one is not a valid Money.
//
// Every caller in this package must handle the false case explicitly. A cross-currency
// comparison silently answered as "not greater than" is a limit that does not apply, and a
// limit that silently does not apply is worse than no limit at all — the merchant believes they
// are protected.
func compareAmounts(a, b money.Money) (int, bool) {
	if !a.IsValid() || !b.IsValid() {
		return 0, false
	}
	c, err := a.Cmp(b)
	if err != nil {
		return 0, false
	}
	return c, true
}
