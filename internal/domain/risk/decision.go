// Package risk is the risk and fraud bounded context's domain model (BC-8).
//
// It contains the merchant's risk policy, the SCA exemption model, and a pure evaluation
// function that turns an Assessment into a Decision. There is no I/O in this package and no
// clock: every counter, blocklist answer and external score arrives on the Assessment, already
// fetched, and the evaluation instant arrives as a field. That is not architectural purity for
// its own sake — a risk decision is the thing a chargeback representment, a scheme audit and an
// unhappy merchant all want re-run, and a decision function that reads Redis cannot be re-run.
//
// Two properties are worth stating up front because everything else in the package follows from
// them:
//
//   - **Fail-open means fail to the policy default, never to "approve".** Every check that
//     depends on data that can be unavailable declares what its unavailability *means*, and the
//     merchant's policy declares what to do about it. The default posture is REQUIRE_3DS, not
//     APPROVE, and not DECLINE. See FailurePosture.
//   - **An exemption that is not recorded is an exemption you cannot defend.** When strong
//     customer authentication is waived, the Decision names which exemption was claimed and on
//     what basis. A frictionless payment with no recorded reason is, to an auditor and to an
//     issuer disputing liability, indistinguishable from a bug.
//
// See docs/data-plane.md §6 and docs/spec/00-design-baseline.md §23.
package risk

import (
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Outcome is the risk engine's verdict.
//
// Four values rather than the obvious two, because "allow" and "block" cannot express the two
// decisions that actually make a risk engine useful. REQUIRE_3DS converts a fraud risk into an
// authentication step and shifts chargeback liability to the issuer — it is a *better* outcome
// than declining for both the merchant and the payer. REVIEW parks the payment for a human
// instead of guessing, which is the right answer for the small band of transactions where the
// cost of a false decline exceeds the cost of a delay.
type Outcome string

const (
	// OutcomeApprove lets the payment proceed with no additional friction.
	OutcomeApprove Outcome = "APPROVE"
	// OutcomeRequire3DS proceeds but forces strong customer authentication.
	OutcomeRequire3DS Outcome = "REQUIRE_3DS"
	// OutcomeReview holds the payment for manual review rather than deciding.
	OutcomeReview Outcome = "REVIEW"
	// OutcomeDecline refuses the payment. Terminal: evaluation stops here.
	OutcomeDecline Outcome = "DECLINE"
)

// AllOutcomes is the complete outcome universe, in ascending severity.
var AllOutcomes = []Outcome{OutcomeApprove, OutcomeRequire3DS, OutcomeReview, OutcomeDecline}

// IsValid reports whether o is a known outcome.
func (o Outcome) IsValid() bool {
	switch o {
	case OutcomeApprove, OutcomeRequire3DS, OutcomeReview, OutcomeDecline:
		return true
	default:
		return false
	}
}

// String satisfies fmt.Stringer.
func (o Outcome) String() string { return string(o) }

// IsTerminal reports whether evaluation stops at this outcome. Only DECLINE is terminal: there
// is nothing a later check could say that would make a sanctioned country acceptable, and
// continuing to evaluate would spend budget to reach the same answer.
func (o Outcome) IsTerminal() bool { return o == OutcomeDecline }

// severity orders the outcomes so that combining two of them can pick the stricter. The
// numbering is internal; only the ordering is meaningful.
func (o Outcome) severity() int {
	switch o {
	case OutcomeDecline:
		return 3
	case OutcomeReview:
		return 2
	case OutcomeRequire3DS:
		return 1
	default:
		return 0
	}
}

// Escalate returns the stricter of two outcomes.
//
// Checks combine by escalation, never by averaging or by last-writer-wins. A payment that trips
// a velocity limit and then passes the amount check is still a payment that tripped a velocity
// limit; letting a later benign check relax an earlier concern is how a risk engine ends up
// approving everything that happens to end with a cheap check.
func Escalate(a, b Outcome) Outcome {
	if b.severity() > a.severity() {
		return b
	}
	return a
}

// Severity classifies a reason for operator triage and alerting. It is deliberately separate
// from the Outcome: a check can contribute a critical-severity reason without being the check
// that produced the final outcome, and an operator investigating a spike wants to see those.
type Severity string

const (
	// SeverityInfo records a check that fired without pushing the outcome — an exemption claim,
	// an allowlist hit.
	SeverityInfo Severity = "INFO"
	// SeverityWarning records a check that escalated the outcome short of declining.
	SeverityWarning Severity = "WARNING"
	// SeverityCritical records a check that declined, or a degraded evaluation.
	SeverityCritical Severity = "CRITICAL"
)

// CheckID is the stable identity of one risk check.
//
// The values are the validation-plane rule IDs verbatim (docs/data-plane.md §6.2, baseline
// §23), which is the point: the same string appears in the risk decision persisted with the
// payment, in the API error's `ruleId` when the check declines, in the documentation anchor the
// merchant is sent to, and — via MetricLabel — in the metric that counts how often the check
// fires. One identifier across all four means "L5.VELOCITY_PER_CARD_PER_HOUR is firing 40× more
// than last week" is a query anyone can run, and it means renaming a check is a deliberate,
// visible, catalogued act rather than a string edit.
//
// Free-text reasons were rejected for the reason free-text decline reasons were rejected
// elsewhere in this platform: text cannot be aggregated, and a reason nobody can count is a
// reason nobody acts on.
type CheckID string

const (
	// CheckSanctionedCountry is the compiled sanctions and blocked-country list.
	CheckSanctionedCountry CheckID = "L5.CUSTOMER_COUNTRY_NOT_BLOCKED"
	// CheckCountrySupported is the merchant's positive allowlist of countries, when they declare
	// one. An empty allowlist means "all countries", which is not the same as an allowlist that
	// happens to be empty because a config write truncated it — see Policy.AllowedCountries.
	CheckCountrySupported CheckID = "L5.CUSTOMER_COUNTRY_IN_SUPPORTED_SET"
	// CheckIPCountrySanctioned compares the payer's network origin against the sanctions list.
	CheckIPCountrySanctioned CheckID = "L5.IP_COUNTRY_NOT_SANCTIONED"
	// CheckPlatformBlocklist is the platform-wide known-bad set.
	CheckPlatformBlocklist CheckID = "L5.NOT_ON_PLATFORM_BLOCKLIST"
	// CheckMerchantBlocklist is the merchant's own blocked set.
	CheckMerchantBlocklist CheckID = "L5.NOT_ON_MERCHANT_BLOCKLIST"
	// CheckMerchantAllowlist is the merchant's own trusted set.
	CheckMerchantAllowlist CheckID = "L5.ON_MERCHANT_ALLOWLIST"
	// CheckAmountWithinLimit is the per-transaction ceiling.
	CheckAmountWithinLimit CheckID = "L5.AMOUNT_WITHIN_MERCHANT_LIMIT"
	// CheckDailyVolume is the merchant's daily money limit.
	CheckDailyVolume CheckID = "L5.DAILY_VOLUME_WITHIN_LIMIT"
	// CheckVelocityPerMinute counts payments per merchant per minute.
	CheckVelocityPerMinute CheckID = "L5.VELOCITY_PAYMENTS_PER_MINUTE"
	// CheckVelocityPerCard counts payments per card fingerprint per hour.
	CheckVelocityPerCard CheckID = "L5.VELOCITY_PER_CARD_PER_HOUR"
	// CheckVelocityPerCustomer counts payments per customer per rolling day.
	CheckVelocityPerCustomer CheckID = "L5.VELOCITY_PER_CUSTOMER_PER_DAY"
	// CheckDistinctCards counts distinct cards a customer has presented in the window — the
	// signature of card testing, and the check most worth having.
	CheckDistinctCards CheckID = "L5.VELOCITY_DISTINCT_CARDS_PER_CUSTOMER"
	// CheckRiskScore is the external scorer's verdict against the policy thresholds.
	CheckRiskScore CheckID = "L5.RISK_SCORE_BELOW_DECLINE_THRESHOLD"
	// CheckThreeDSThreshold is the amount above which the merchant forces authentication.
	CheckThreeDSThreshold CheckID = "L5.THREE_DS_REQUIRED_ABOVE_THRESHOLD"
	// CheckSCAExemption records which exemption was claimed, and is the reason the Decision can
	// be defended in an audit.
	CheckSCAExemption CheckID = "L5.SCA_EXEMPTION_IS_CLAIMABLE"
)

// AllCheckIDs is the complete check universe, in evaluation order. Used by the metric
// registration, the documentation generator, and the test that asserts every check is reachable.
var AllCheckIDs = []CheckID{
	CheckSanctionedCountry,
	CheckIPCountrySanctioned,
	CheckCountrySupported,
	CheckPlatformBlocklist,
	CheckMerchantBlocklist,
	CheckMerchantAllowlist,
	CheckAmountWithinLimit,
	CheckDailyVolume,
	CheckVelocityPerMinute,
	CheckVelocityPerCard,
	CheckVelocityPerCustomer,
	CheckDistinctCards,
	CheckRiskScore,
	CheckThreeDSThreshold,
	CheckSCAExemption,
}

// IsValid reports whether c is a known check.
func (c CheckID) IsValid() bool {
	for _, x := range AllCheckIDs {
		if x == c {
			return true
		}
	}
	return false
}

// String satisfies fmt.Stringer.
func (c CheckID) String() string { return string(c) }

// MetricLabel renders the check as a Prometheus label value: the rule ID with its validation
// level prefix stripped and lowercased, e.g. "velocity_per_card_per_hour".
//
// The level prefix is dropped because it is an artifact of where the rule is enforced, not of
// what the rule is, and a check that moves between validation levels must not silently become a
// different time series.
func (c CheckID) MetricLabel() string {
	s := string(c)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[i+1:]
	}
	return strings.ToLower(s)
}

// Reason is one check's contribution to a decision.
type Reason struct {
	// Check names the rule. Stable, countable, documented.
	Check CheckID
	// Detail is the specific fact — "7 payments on this card in the last hour, limit 5". It is
	// for the human reading the decision, never for a machine to parse, and it must never
	// contain a PAN, an email or any other identifier the platform is not allowed to retain.
	Detail string
	// Severity classifies the reason for triage.
	Severity Severity
}

// Decision is the risk engine's output, persisted with the payment and carried on
// payment.created.v1.
type Decision struct {
	// Outcome is the verdict.
	Outcome Outcome
	// Reasons are every check that contributed, in evaluation order. Kept even on APPROVE: the
	// record of which checks ran and passed is what distinguishes "we evaluated this and it was
	// fine" from "the risk engine was skipped".
	Reasons []Reason
	// Require3DS is the authentication decision, which is *not* implied by the Outcome. A
	// payment can be APPROVE with Require3DS true — that is the ordinary case for a regulated
	// corridor with no claimable exemption — and it can be REQUIRE_3DS from a risk signal on a
	// corridor with no regulatory requirement at all.
	Require3DS bool
	// ExemptionApplied names the SCA exemption claimed, or "" if none was. This field is the
	// difference between an exemption that can be defended in an audit and one that cannot.
	ExemptionApplied ExemptionType
	// Score is the risk score on 0..100, higher is riskier. Zero when no scorer ran; see
	// Degraded to distinguish "not scored" from "scored zero".
	Score int
	// Degraded is true when at least one check ran on unavailable data and fell back to its
	// configured posture. It is recorded so that "we processed forty minutes of traffic with
	// degraded risk" is a fact in the record rather than an inference from a dashboard.
	Degraded bool
	// EvaluatedAt is the instant the decision was made, supplied by the caller.
	EvaluatedAt time.Time
	// PolicyVersion identifies the compiled policy that produced the decision, so a replay uses
	// the policy that was in force rather than the one in force today.
	PolicyVersion int
}

// HasReason reports whether the given check contributed to the decision.
func (d Decision) HasReason(c CheckID) bool {
	for _, r := range d.Reasons {
		if r.Check == c {
			return true
		}
	}
	return false
}

// ReasonFor returns the reason recorded for a check, if there is one.
func (d Decision) ReasonFor(c CheckID) (Reason, bool) {
	for _, r := range d.Reasons {
		if r.Check == c {
			return r, true
		}
	}
	return Reason{}, false
}

// ReasonCodes returns the check IDs that contributed, in evaluation order. Used as the event
// payload and as the support answer to "why was this declined".
func (d Decision) ReasonCodes() []string {
	out := make([]string, 0, len(d.Reasons))
	for _, r := range d.Reasons {
		out = append(out, string(r.Check))
	}
	return out
}

// Approved reports whether the payment may proceed at all, with or without authentication.
func (d Decision) Approved() bool {
	return d.Outcome == OutcomeApprove || d.Outcome == OutcomeRequire3DS
}

// Err renders a blocking decision as a platform error, or nil when the payment may proceed.
//
// The mapping is not cosmetic. RISK_DECLINED and THREE_DS_REQUIRED are different HTTP statuses,
// different retryability, and — most importantly — different instructions to the merchant's
// integration: one means "stop", the other means "collect authentication and come back". A
// single generic error here would force every client to parse a detail string to tell those
// apart, and clients that parse strings get it wrong.
func (d Decision) Err() *apierror.Error {
	details := make([]apierror.Detail, 0, len(d.Reasons))
	for _, r := range d.Reasons {
		details = append(details, apierror.Detail{
			Field:   "risk",
			Code:    r.Check.MetricLabel(),
			Message: r.Detail,
			RuleID:  string(r.Check),
		})
	}
	switch d.Outcome {
	case OutcomeDecline:
		return apierror.New(apierror.CodeRiskDeclined, "").WithDetails(details...)
	case OutcomeReview:
		// A review hold is a conflict, not a decline: the payment exists and is being decided.
		// Modelling it as PAYMENT_ALREADY_PROCESSED would be wrong; it gets its own conflict
		// with an explicit message so a client does not treat it as a permanent failure.
		return apierror.New(apierror.CodeRiskDeclined,
			"the payment is held for manual review").WithDetails(details...)
	default:
		return nil
	}
}
