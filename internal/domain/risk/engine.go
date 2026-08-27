package risk

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Counter is a velocity count that may be unavailable.
//
// The unavailability is modelled in the type rather than signalled by a sentinel value, because
// every sentinel anyone reaches for here is wrong. Zero means "no payments in the window",
// which is the *safest* possible reading and therefore the most dangerous thing an outage could
// silently produce. Negative one means the check has to remember to test for it, and one caller
// eventually will not. A struct with an explicit availability bit makes the question
// unavoidable: Value() is meaningless without IsAvailable(), and the engine's shape reflects
// that.
type Counter struct {
	value     int64
	available bool
}

// KnownCount wraps an observed count.
func KnownCount(v int64) Counter { return Counter{value: v, available: true} }

// UnavailableCount is the marker a caller passes when the counter store could not be read.
func UnavailableCount() Counter { return Counter{} }

// IsAvailable reports whether the count was actually observed.
func (c Counter) IsAvailable() bool { return c.available }

// Value returns the observed count. Meaningless unless IsAvailable is true.
func (c Counter) Value() int64 { return c.value }

// Volume is a money-valued counter that may be unavailable — the merchant's day-to-date
// processed volume.
type Volume struct {
	amount    money.Money
	available bool
}

// KnownVolume wraps an observed volume.
func KnownVolume(m money.Money) Volume { return Volume{amount: m, available: true} }

// UnavailableVolume is the marker a caller passes when the volume could not be read from either
// the counter store or the Postgres fallback.
func UnavailableVolume() Volume { return Volume{} }

// IsAvailable reports whether the volume was actually observed.
func (v Volume) IsAvailable() bool { return v.available }

// Amount returns the observed volume.
func (v Volume) Amount() money.Money { return v.amount }

// Score is an external risk score that may be unavailable.
type Score struct {
	value     int
	available bool
}

// KnownScore wraps a scorer result on 0..100, higher being riskier.
func KnownScore(v int) Score { return Score{value: v, available: true} }

// UnavailableScore is the marker for a scorer that timed out, was circuit-broken, or was never
// invoked because the policy's predicate did not match.
func UnavailableScore() Score { return Score{} }

// IsAvailable reports whether a score was obtained.
func (s Score) IsAvailable() bool { return s.available }

// Value returns the score.
func (s Score) Value() int { return s.value }

// Counters are the velocity observations for one payment, fetched by the caller in a single
// pipelined read before evaluation (docs/data-plane.md §6.2).
type Counters struct {
	// PaymentsThisMinute is the merchant's payment count in the trailing 60s.
	PaymentsThisMinute Counter
	// CardThisHour is the count against this card fingerprint in the trailing hour.
	CardThisHour Counter
	// CustomerToday is the count for this customer in the rolling 24h.
	CustomerToday Counter
	// DistinctCardsToday is the number of distinct card fingerprints this customer has
	// presented, from a HyperLogLog with ~0.8% error. The error is why this check should carry
	// a limit with headroom rather than an exact one.
	DistinctCardsToday Counter
	// VolumeToday is the merchant's day-to-date processed money.
	VolumeToday Volume
}

// CustomerHistory summarises what the platform knows about this payer, without retaining
// anything it is not entitled to.
//
// Everything here is a count, a flag or a duration. There is no email, no name and no address:
// the platform is not the merchant's CRM, and every personal field carried into a risk decision
// is a GDPR liability that has to be justified against the marginal accuracy it buys. These
// fields are the ones that actually change the decision.
type CustomerHistory struct {
	// SuccessfulPayments is the lifetime count of settled payments for this customer with this
	// merchant.
	SuccessfulPayments int
	// Chargebacks is the lifetime dispute count.
	Chargebacks int
	// DaysSinceFirstPayment is the age of the relationship. A five-year-old customer and a
	// five-minute-old one presenting the same card are not the same risk.
	DaysSinceFirstPayment int
	// TrustedBeneficiary is true when the payer has added this merchant to their issuer's
	// trusted-beneficiary list. It is the issuer's assertion, relayed by the gateway — never
	// something the platform infers, because the exemption's liability position depends on the
	// issuer having actually made the assertion.
	TrustedBeneficiary bool
	// ConsecutiveExemptSinceLastSCA counts low-value exemptions claimed since the last
	// authenticated payment on this card. The PSD2 counter: five consecutive, or a cumulative
	// amount ceiling, whichever comes first.
	ConsecutiveExemptSinceLastSCA int
	// CumulativeExemptAmount is the running total of low-value exemptions since the last SCA.
	CumulativeExemptAmount money.Money
}

// MerchantRiskRating is the merchant's own risk classification, assigned at underwriting.
//
// It is an input to risk decisions rather than an output of them: a high-risk merchant's
// traffic gets stricter fallback postures because the platform, not just the merchant, carries
// the consequence of their chargeback ratio.
type MerchantRiskRating string

const (
	// RatingLow is an established merchant with a clean history.
	RatingLow MerchantRiskRating = "LOW"
	// RatingStandard is the default.
	RatingStandard MerchantRiskRating = "STANDARD"
	// RatingElevated is a merchant under monitoring.
	RatingElevated MerchantRiskRating = "ELEVATED"
	// RatingHigh is a merchant whose category or history means the platform's own scheme
	// registrations are exposed by their traffic.
	RatingHigh MerchantRiskRating = "HIGH"
)

// IsValid reports whether r is a known rating.
func (r MerchantRiskRating) IsValid() bool {
	switch r {
	case RatingLow, RatingStandard, RatingElevated, RatingHigh:
		return true
	default:
		return false
	}
}

// String satisfies fmt.Stringer.
func (r MerchantRiskRating) String() string { return string(r) }

// Assessment is the complete input to a risk decision.
//
// Everything the engine may look at is on this struct — no clock, no repository, no cache. That
// is what makes Evaluate replayable: persisting the Assessment alongside the Decision means the
// decision can be re-derived byte for byte years later, which is what a scheme audit and a
// chargeback representment both actually ask for.
type Assessment struct {
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	PaymentID  shared.PaymentID

	// Amount and PaymentMethod describe the payment being assessed.
	Amount        money.Money
	PaymentMethod shared.PaymentMethod

	// MerchantRating is the merchant's underwriting classification.
	MerchantRating MerchantRiskRating

	// PayerCountry is the customer's billing country, IssuingCountry is the card's country of
	// issue, and IPCountry is the network origin. All three are separate inputs because their
	// *disagreement* is the signal: a US card, a German billing address and a Vietnamese IP is a
	// pattern no single field expresses.
	PayerCountry    shared.Country
	IssuingCountry  shared.Country
	IPCountry       shared.Country
	MerchantCountry shared.Country

	// SCAJurisdiction is true when strong-customer-authentication rules apply to this corridor.
	// The caller derives it from Country.IsSCAJurisdiction on both ends, because the rule is a
	// property of the corridor and not of either country alone.
	SCAJurisdiction bool

	// Counters are the velocity observations.
	Counters Counters
	// History summarises the customer relationship.
	History CustomerHistory

	// Blocklist and allowlist answers, resolved by the caller.
	OnPlatformBlocklist bool
	OnMerchantBlocklist bool
	OnMerchantAllowlist bool
	// BlocklistAvailable is false when the blocklist store could not be read. One flag covers
	// both lists because they are read from the same store in the same pipeline: they fail
	// together or not at all.
	BlocklistAvailable bool

	// ExternalScore is the ML scorer's result, if it ran.
	ExternalScore Score

	// --- exemption inputs -------------------------------------------------------------------

	// MerchantInitiated marks a payment the payer is not present for.
	MerchantInitiated bool
	// NetworkTransactionRef is the scheme reference from the original authenticated payment.
	// An MIT exemption without one is unclaimable: the exemption rests entirely on there having
	// been a prior SCA, and the reference is the proof.
	NetworkTransactionRef string
	// CorporateCard is true when the BIN table flags a secure corporate product on a lodged or
	// virtual account.
	CorporateCard bool
	// LowValueCeiling is the corridor equivalent of the EUR 30 low-value threshold, supplied by
	// the caller in the payment's currency. Supplied rather than hardcoded because it is a
	// regulatory figure in EUR and every other currency is a conversion the domain must not
	// improvise.
	LowValueCeiling money.Money
	// LowValueCumulativeCeiling is the corridor equivalent of the EUR 100 cumulative ceiling.
	LowValueCumulativeCeiling money.Money
	// TRABandCeiling is the amount ceiling the acquirer's measured fraud rate permits under
	// transaction risk analysis. A zero or invalid value means TRA is not claimable.
	TRABandCeiling money.Money
	// TRABandStale is true when the acquirer's published fraud rate is older than the 100-day
	// refresh window, which makes the band unusable regardless of the amount.
	TRABandStale bool
	// TRAScoreCeiling is the risk score below which TRA may be claimed.
	TRAScoreCeiling int

	// EvaluatedAt is the decision instant. A field rather than a clock call: see the package
	// comment.
	EvaluatedAt time.Time
}

// ExemptionType is an SCA exemption the platform can claim on the payer's behalf.
//
// Each is a *claim*, not an assertion: the platform sends it, and the issuer may still soft
// decline with "SCA required", at which point the orchestrator retries the same gateway with a
// forced challenge. That is a same-attempt retry with the same gateway idempotency key, not a
// failover — which is why the exemption must be recorded on the Decision, because the retry
// logic needs to know what was claimed the first time.
type ExemptionType string

const (
	// ExemptionLowValue is the PSD2 low-value exemption: a small amount, subject to a cumulative
	// ceiling and a consecutive-use count since the last authentication. Liability stays with
	// the merchant and acquirer.
	ExemptionLowValue ExemptionType = "LOW_VALUE"
	// ExemptionTRA is transaction risk analysis: the acquirer's measured fraud rate buys an
	// amount band, and the platform's own score must be below the TRA threshold. Liability sits
	// with the acquirer, which is why the band is a per-gateway figure refreshed quarterly and
	// goes unclaimable when stale.
	ExemptionTRA ExemptionType = "TRA"
	// ExemptionMIT is a merchant-initiated transaction: the payer is not present, so SCA does
	// not apply at all — provided the initial transaction in the series was itself authenticated
	// and its network reference is carried forward.
	ExemptionMIT ExemptionType = "MIT"
	// ExemptionTrustedBeneficiary applies when the payer has added the merchant to their
	// issuer's trusted list. The issuer made the assertion; the platform relays it.
	ExemptionTrustedBeneficiary ExemptionType = "TRUSTED_BENEFICIARY"
	// ExemptionCorporate applies to secure corporate cards on lodged or virtual accounts, which
	// are out of SCA scope entirely.
	ExemptionCorporate ExemptionType = "CORPORATE"
)

// ExemptionPrecedence is the order exemptions are tried, and it is not arbitrary.
//
// The ordering runs from strongest liability position to weakest. MIT and CORPORATE are
// out-of-scope claims: the transaction is not subject to SCA at all, so there is nothing to be
// liable for. TRUSTED_BENEFICIARY rests on an issuer assertion, so liability sits with the
// issuer. TRA shifts liability to the acquirer and consumes the acquirer's fraud-rate budget,
// so it is claimed only when nothing better applies. LOW_VALUE is last because it leaves
// liability with the merchant *and* consumes a counter that, once exhausted, forces an
// authentication on a later payment the merchant would rather have had frictionless.
//
// Claiming the wrong one is not a small error: it moves who pays for the chargeback.
var ExemptionPrecedence = []ExemptionType{
	ExemptionMIT,
	ExemptionCorporate,
	ExemptionTrustedBeneficiary,
	ExemptionTRA,
	ExemptionLowValue,
}

// String satisfies fmt.Stringer.
func (e ExemptionType) String() string { return string(e) }

// IsValid reports whether e is a known exemption.
func (e ExemptionType) IsValid() bool {
	for _, x := range ExemptionPrecedence {
		if x == e {
			return true
		}
	}
	return false
}

// Applies reports whether this exemption's preconditions hold for the assessment.
//
// Every branch fails closed: a missing input means the exemption is not claimable, never that
// it is. That asymmetry is the whole design. An exemption wrongly withheld costs a conversion;
// an exemption wrongly claimed is an unauthenticated payment the merchant is liable for and,
// repeated, a regulatory finding.
func (e ExemptionType) Applies(a Assessment) bool {
	switch e {
	case ExemptionMIT:
		// The reference is not optional. Without it there is no evidence the series began with
		// an authenticated payment, and "the merchant told us it did" is not evidence.
		return a.MerchantInitiated && a.NetworkTransactionRef != ""

	case ExemptionCorporate:
		return a.CorporateCard

	case ExemptionTrustedBeneficiary:
		return a.History.TrustedBeneficiary

	case ExemptionTRA:
		// Staleness alone disqualifies: the band is bought with a fraud rate the acquirer
		// published, and a rate nobody has refreshed in a hundred days is not a rate.
		if a.TRABandStale || !a.TRABandCeiling.IsValid() || !a.TRABandCeiling.IsPositive() {
			return false
		}
		if cmp, ok := compareAmounts(a.Amount, a.TRABandCeiling); !ok || cmp > 0 {
			return false
		}
		// An unavailable score makes TRA unclaimable. This is the one place where "we could not
		// score it" must not degrade to "claim anyway": TRA is *defined* as a claim about the
		// risk score, and a claim about a number you did not compute is one you cannot defend.
		if !a.ExternalScore.IsAvailable() {
			return false
		}
		return a.ExternalScore.Value() < a.TRAScoreCeiling

	case ExemptionLowValue:
		if !a.LowValueCeiling.IsValid() || !a.LowValueCeiling.IsPositive() {
			return false
		}
		if cmp, ok := compareAmounts(a.Amount, a.LowValueCeiling); !ok || cmp > 0 {
			return false
		}
		if a.History.ConsecutiveExemptSinceLastSCA >= maxConsecutiveLowValue {
			return false
		}
		if a.LowValueCumulativeCeiling.IsValid() && a.LowValueCumulativeCeiling.IsPositive() {
			projected, err := a.History.CumulativeExemptAmount.Add(a.Amount)
			if err != nil {
				return false
			}
			if cmp, ok := compareAmounts(projected, a.LowValueCumulativeCeiling); !ok || cmp > 0 {
				return false
			}
		}
		return true

	default:
		return false
	}
}

// maxConsecutiveLowValue is the PSD2 cap on consecutive low-value exemptions before an
// authentication is required. Five, from the regulation, not from a configuration file: a
// merchant cannot negotiate it.
const maxConsecutiveLowValue = 5

// ClaimableExemption returns the strongest exemption that applies, in ExemptionPrecedence order.
func ClaimableExemption(a Assessment) (ExemptionType, bool) {
	for _, e := range ExemptionPrecedence {
		if e.Applies(a) {
			return e, true
		}
	}
	return "", false
}

// Evaluate runs the merchant's risk policy against an assessment.
//
// Pure, deterministic, no I/O, no clock. The same inputs always produce the same Decision,
// which is what makes the decision replayable years later and what makes this function
// exhaustively testable in microseconds.
//
// # Check ordering
//
// The order below is docs/data-plane.md §6.1: cheapest and most decisive first, stopping at the
// first terminal decision. Two separate criteria are at work and they mostly agree:
//
//   - *Cheapest first*, because the whole engine has a 15ms budget on the money path and there
//     is no reason to spend a Redis round-trip's worth of parsing on a payment a 2µs in-process
//     set membership test is going to decline anyway.
//   - *Most decisive first*, because only DECLINE is terminal, and reaching it early skips
//     everything after it. A sanctions hit is decisive in a way no later check can soften.
//
// Where the two criteria disagree, decisiveness wins. Concretely:
//
//  1. Sanctions and blocked country — in-process, ~2µs, unappealable.
//  2. IP-origin sanctions — same list, different field.
//  3. Merchant allowlist — cheap, and short-circuits the fraud-signal checks below.
//  4. Merchant country allowlist — positive-set membership.
//  5. Platform blocklist — known-bad, DECLINE.
//  6. Merchant blocklist — merchant's own known-bad, DECLINE.
//  7. Amount ceiling — contractual, fails closed, needs no external data.
//  8. Daily volume — money limit, fails closed by default.
//  9. Count velocity — four checks, fail to the configured posture.
//  10. External score — thresholds, falls back to the configured posture.
//  11. SCA and exemptions — never terminal, decides Require3DS.
//
// The one deliberate departure from §6.1: the merchant allowlist skips the *fraud-signal*
// checks (velocity, score) but not the amount and daily-volume limits. §6.1 has the allowlist
// skipping checks 5–9 outright. That would let an allowlisted customer exceed the merchant's
// contractual per-transaction ceiling, and a limit that a merchant's own allowlist can waive is
// not a limit. An allowlist is an assertion about *fraud* — "I know this customer" — and it is
// not an assertion about the merchant's processing agreement.
func Evaluate(policy Policy, a Assessment) Decision {
	d := Decision{
		Outcome:       OutcomeApprove,
		EvaluatedAt:   a.EvaluatedAt.UTC(),
		PolicyVersion: policy.Version,
	}
	if a.ExternalScore.IsAvailable() {
		d.Score = a.ExternalScore.Value()
	}

	// 1 & 2. Sanctions. Fail-closed by construction: the compiled list is in-process and cannot
	// be unavailable, and a configuration snapshot stale past the cliff stops new payments
	// upstream of this engine entirely (baseline §15). Sanctions breaches are not survivable, so
	// there is no posture to configure and no degraded mode to reach.
	if policy.IsCountryBlocked(a.PayerCountry) {
		return decline(d, CheckSanctionedCountry,
			"payer country "+a.PayerCountry.String()+" is on the blocked-country list")
	}
	if a.IPCountry != "" && policy.IsCountryBlocked(a.IPCountry) {
		return decline(d, CheckIPCountrySanctioned,
			"network origin country "+a.IPCountry.String()+" is on the blocked-country list")
	}

	// 3. Merchant allowlist. Recorded even though it produces no escalation, because "this
	// payment skipped the velocity checks" must be visible in the decision rather than inferred
	// from the absence of reasons.
	allowlisted := a.OnMerchantAllowlist && a.BlocklistAvailable
	if allowlisted {
		d.Reasons = append(d.Reasons, Reason{
			Check:    CheckMerchantAllowlist,
			Detail:   "payer is on the merchant's allowlist; fraud-signal checks skipped, contractual limits still enforced",
			Severity: SeverityInfo,
		})
	}

	// 4. Positive country allowlist.
	if !policy.IsCountryAllowed(a.PayerCountry) {
		return decline(d, CheckCountrySupported,
			"payer country "+a.PayerCountry.String()+" is not in the merchant's supported set")
	}

	// 5 & 6. Blocklists. Unavailability is where the posture machinery first bites.
	if !allowlisted {
		if a.BlocklistAvailable {
			if a.OnPlatformBlocklist {
				return decline(d, CheckPlatformBlocklist, "payer matches an entry on the platform blocklist")
			}
			if a.OnMerchantBlocklist {
				return decline(d, CheckMerchantBlocklist, "payer matches an entry on the merchant's blocklist")
			}
		} else {
			// The blocklist store is down. The default posture is REQUIRE_3DS, not APPROVE: the
			// list exists precisely because these entries are known-bad, and treating unknown as
			// clean during a cache blip is how a known fraudster gets through. It is escalated to
			// DECLINE for a high-risk merchant, where the platform's own scheme registrations are
			// what is exposed by getting this wrong.
			posture := policy.PostureFor(CheckPlatformBlocklist)
			if a.MerchantRating == RatingHigh {
				posture = PostureFailClosed
			}
			d = applyPosture(d, CheckPlatformBlocklist, posture,
				"blocklist store unavailable; applied posture "+posture.String())
			if d.Outcome.IsTerminal() {
				return d
			}
		}
	}

	// 7. Amount ceiling. Fails closed unconditionally and has no posture, because there is
	// nothing to fail: the amount is on the request and the limit is compiled into the policy.
	// A currency mismatch between the two is the one way this check can fail to evaluate, and it
	// declines — a limit that cannot be checked is a limit that has not been satisfied.
	if policy.MaxTransactionAmount.IsPositive() {
		cmp, ok := compareAmounts(a.Amount, policy.MaxTransactionAmount)
		switch {
		case !ok:
			return decline(d, CheckAmountWithinLimit,
				"cannot compare "+a.Amount.String()+" against the merchant limit "+
					policy.MaxTransactionAmount.String()+"; the limit cannot be shown to be satisfied")
		case cmp > 0:
			return decline(d, CheckAmountWithinLimit,
				a.Amount.String()+" exceeds the merchant's per-transaction limit of "+
					policy.MaxTransactionAmount.String())
		}
	}

	// 8. Daily volume. A money limit, so the default posture is FAIL_CLOSED — but only after the
	// caller has exhausted the Postgres fallback (§6.2), which is why UnavailableVolume means
	// "both sources failed" and not merely "Redis missed".
	if policy.DailyVolumeLimit.IsPositive() {
		if !a.Counters.VolumeToday.IsAvailable() {
			posture := policy.PostureFor(CheckDailyVolume)
			d = applyPosture(d, CheckDailyVolume, posture,
				"day-to-date volume unavailable from both the counter store and the database fallback; applied posture "+posture.String())
			if d.Outcome.IsTerminal() {
				return d
			}
		} else {
			projected, err := a.Counters.VolumeToday.Amount().Add(a.Amount)
			if err != nil {
				return decline(d, CheckDailyVolume,
					"day-to-date volume and payment amount cannot be summed; the daily limit cannot be shown to be satisfied")
			}
			if cmp, ok := compareAmounts(projected, policy.DailyVolumeLimit); !ok || cmp > 0 {
				return decline(d, CheckDailyVolume,
					"this payment would bring day-to-date volume to "+projected.String()+
						", exceeding the daily limit of "+policy.DailyVolumeLimit.String())
			}
		}
	}

	// 9. Count velocity. Skipped for an allowlisted payer — this is the fraud-signal band the
	// allowlist is an assertion about.
	if !allowlisted {
		d = evaluateVelocity(d, policy, a)
		if d.Outcome.IsTerminal() {
			return d
		}
	}

	// 10. External score. An unavailable scorer falls back to the policy posture and never to
	// APPROVE: the scorer was invoked at all only because the compiled policy's predicate said
	// this payment warranted a look, so "we could not look" is not evidence that it was fine.
	if !allowlisted {
		decl, review, _ := policy.Thresholds()
		switch {
		case a.ExternalScore.IsAvailable():
			s := a.ExternalScore.Value()
			switch {
			case s >= decl:
				return decline(d, CheckRiskScore,
					"risk score "+itoa(s)+" is at or above the decline threshold of "+itoa(decl))
			case s >= review:
				d.Outcome = Escalate(d.Outcome, OutcomeReview)
				d.Reasons = append(d.Reasons, Reason{
					Check:    CheckRiskScore,
					Detail:   "risk score " + itoa(s) + " is at or above the review threshold of " + itoa(review),
					Severity: SeverityWarning,
				})
			}
		case a.ScorerWasInvoked():
			posture := policy.PostureFor(CheckRiskScore)
			d = applyPosture(d, CheckRiskScore, posture,
				"external scorer unavailable; applied posture "+posture.String())
			if d.Outcome.IsTerminal() {
				return d
			}
		}
	}

	// 11. SCA and exemptions. Never terminal — it decides friction, not permission.
	d = decideSCA(d, policy, a)
	return d
}

// ScorerWasInvoked reports whether the external scorer was supposed to run for this payment.
//
// The distinction between "the scorer ran and failed" and "the scorer was never asked" is the
// difference between a degraded evaluation and a normal one. Only ~3% of payments meet the
// policy's scoring predicate; applying the scorer-unavailable posture to the other 97% would
// force 3DS on the entire platform the moment the field was misread. The caller signals the
// distinction by leaving TRAScoreCeiling and the score both zero when no scoring was intended;
// a non-zero TRAScoreCeiling means the payment was in scope for scoring.
func (a Assessment) ScorerWasInvoked() bool {
	return a.ExternalScore.IsAvailable() || a.TRAScoreCeiling > 0
}

// evaluateVelocity runs the four count-based checks. Each is independent: a payment can trip
// two, and both are recorded, because "which limit did we hit" is the first question an
// operator asks and a decision that records only the first one makes them guess.
func evaluateVelocity(d Decision, policy Policy, a Assessment) Decision {
	type check struct {
		id      CheckID
		limit   int
		counter Counter
		label   string
	}
	checks := []check{
		{CheckVelocityPerMinute, policy.Velocity.MaxPaymentsPerMinute, a.Counters.PaymentsThisMinute, "payments for this merchant in the last minute"},
		{CheckVelocityPerCard, policy.Velocity.MaxPerCardPerHour, a.Counters.CardThisHour, "payments on this card in the last hour"},
		{CheckVelocityPerCustomer, policy.Velocity.MaxPerCustomerPerDay, a.Counters.CustomerToday, "payments for this customer today"},
		{CheckDistinctCards, policy.MaxCardsPerCustomerPerDay, a.Counters.DistinctCardsToday, "distinct cards presented by this customer today"},
	}

	for _, c := range checks {
		// A limit of zero is unenforced; see Velocity's doc comment for why that sentinel is the
		// right one here and nowhere else. An unenforced check does not consult its counter, so
		// an unavailable counter for a check the merchant never configured produces no
		// degradation and no friction.
		if c.limit <= 0 {
			continue
		}
		if !c.counter.IsAvailable() {
			posture := policy.PostureFor(c.id)
			d = applyPosture(d, c.id, posture,
				"velocity counter unavailable ("+c.label+"); applied posture "+posture.String())
			if d.Outcome.IsTerminal() {
				return d
			}
			continue
		}
		// The counter is the state *before* this payment: counters are incremented after the L7
		// commit, not before, so a payment that fails validation does not consume a customer's
		// allowance. The limit is therefore breached when the count already equals it.
		if c.counter.Value() >= int64(c.limit) {
			d.Outcome = Escalate(d.Outcome, OutcomeDecline)
			d.Reasons = append(d.Reasons, Reason{
				Check: c.id,
				Detail: itoa64(c.counter.Value()) + " " + c.label + " already, limit " +
					itoa(c.limit),
				Severity: SeverityCritical,
			})
		}
	}
	return d
}

// decideSCA sets Require3DS and, where authentication can be waived, records which exemption
// waived it.
//
// The rule, from docs/data-plane.md §6.3, with one hard constraint layered on top:
//
//	Require3DS is true if the amount exceeds require3DSAbove, OR the corridor is an SCA
//	jurisdiction and no exemption applies, OR the risk score crosses the 3DS threshold.
//
// The constraint: an exemption may waive a *regulatory or threshold* requirement, and may never
// waive a *risk-driven* one. If this engine forced authentication because a velocity counter
// was unreadable or because the score crossed a threshold, then a low-value exemption must not
// then wave the payment through frictionless — the exemption is an argument about regulation,
// not an argument about the fraud signal that just fired. Getting this backwards produces an
// engine that applies the most friction to its safest traffic and none to its riskiest, and it
// does so silently.
func decideSCA(d Decision, policy Policy, a Assessment) Decision {
	// A method that is not in scope for SCA cannot require it. SEPA direct debit and PayPal do
	// not have a 3DS step to force.
	if !a.PaymentMethod.RequiresSCAConsideration() {
		return d
	}

	// Risk-driven authentication, which no exemption can waive.
	riskDriven := d.Outcome == OutcomeRequire3DS
	if _, _, threeDS := policy.Thresholds(); a.ExternalScore.IsAvailable() && a.ExternalScore.Value() >= threeDS {
		riskDriven = true
		d.Reasons = append(d.Reasons, Reason{
			Check:    CheckRiskScore,
			Detail:   "risk score " + itoa(a.ExternalScore.Value()) + " is at or above the 3DS threshold of " + itoa(threeDS),
			Severity: SeverityWarning,
		})
	}

	// Regulatory and threshold authentication, which an exemption may waive.
	regulatory := false
	if policy.Require3DSAbove.IsPositive() {
		if cmp, ok := compareAmounts(a.Amount, policy.Require3DSAbove); ok && cmp > 0 {
			regulatory = true
			d.Reasons = append(d.Reasons, Reason{
				Check:    CheckThreeDSThreshold,
				Detail:   a.Amount.String() + " exceeds the merchant's 3DS threshold of " + policy.Require3DSAbove.String(),
				Severity: SeverityInfo,
			})
		}
	}
	if a.SCAJurisdiction {
		regulatory = true
	}

	if regulatory {
		if e, ok := ClaimableExemption(a); ok {
			// Recorded, always. An exemption that is not recorded is an exemption you cannot
			// defend: when the issuer soft-declines with "SCA required" the orchestrator needs
			// to know what was claimed to retry correctly, and when a chargeback lands months
			// later the liability position rests entirely on which exemption was sent.
			d.ExemptionApplied = e
			d.Reasons = append(d.Reasons, Reason{
				Check:    CheckSCAExemption,
				Detail:   "claimed " + e.String() + " exemption; authentication waived for the regulatory requirement",
				Severity: SeverityInfo,
			})
			regulatory = false
		}
	}

	d.Require3DS = riskDriven || regulatory
	if d.Require3DS && d.Outcome == OutcomeApprove {
		d.Outcome = OutcomeRequire3DS
	}
	return d
}

// decline records a terminal DECLINE and returns immediately. Evaluation stops at the first
// terminal decision (§6.1): nothing a later check could say would change the answer, and the
// budget is better spent elsewhere.
func decline(d Decision, c CheckID, detail string) Decision {
	d.Outcome = OutcomeDecline
	d.Reasons = append(d.Reasons, Reason{Check: c, Detail: detail, Severity: SeverityCritical})
	return d
}

// applyPosture escalates the decision according to a check's configured failure posture and
// marks the evaluation degraded.
//
// The Degraded flag is set even when the posture is FAIL_OPEN — especially then. "We approved
// this because the counter store was down" and "we approved this because the counters were
// fine" must not look the same in the record, or the post-incident question "how much traffic
// did we process without velocity checks" has no answer.
func applyPosture(d Decision, c CheckID, posture FailurePosture, detail string) Decision {
	d.Degraded = true
	d.Outcome = Escalate(d.Outcome, posture.Outcome())
	sev := SeverityWarning
	if posture == PostureFailClosed {
		sev = SeverityCritical
	}
	d.Reasons = append(d.Reasons, Reason{Check: c, Detail: detail, Severity: sev})
	return d
}

// itoa and itoa64 are local integer formatters, kept here so that reason-detail construction
// reads as one expression rather than importing a formatting package into a path with a 15ms
// budget.
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

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-14, BR-15, FR-61, FR-68, NFR-40.
//
// Risk evaluation and the SCA decision, including the fail-closed postures that keep an
// unreachable scorer from becoming an unlimited merchant
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
