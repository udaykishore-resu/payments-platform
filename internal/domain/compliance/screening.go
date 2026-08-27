package compliance

import (
	"sort"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// ScreeningOutcome is the result of a sanctions, PEP or adverse-media screening run.
type ScreeningOutcome string

const (
	// OutcomeClear means no hit against any configured list. It is a statement about the list
	// versions used on the day, which is why a screening run retains those versions: lists
	// change, so a bare "no hit" with no list version and no date is not evidence.
	OutcomeClear ScreeningOutcome = "CLEAR"
	// OutcomePotentialMatch means the provider returned one or more candidate matches that a
	// human must dispose of. Fuzzy name matching produces these constantly and most are false;
	// the platform's job is to make sure a human looks, not to guess.
	OutcomePotentialMatch ScreeningOutcome = "POTENTIAL_MATCH"
	// OutcomeConfirmedMatch means the subject is the listed party. This is a hard stop; see
	// AutomatedOverride.
	OutcomeConfirmedMatch ScreeningOutcome = "CONFIRMED_MATCH"
	// OutcomeError means the screening did not complete. It is deliberately distinct from
	// CLEAR: treating a provider outage as "no hit" is how an unscreened merchant goes live,
	// and the whole point of a separate value is that the workflow blocks on it rather than
	// proceeding.
	OutcomeError ScreeningOutcome = "ERROR"
)

var screeningOutcomes = map[ScreeningOutcome]struct{}{
	OutcomeClear: {}, OutcomePotentialMatch: {}, OutcomeConfirmedMatch: {}, OutcomeError: {},
}

// IsValid reports whether o is a known outcome.
func (o ScreeningOutcome) IsValid() bool { _, ok := screeningOutcomes[o]; return ok }

// String satisfies fmt.Stringer.
func (o ScreeningOutcome) String() string { return string(o) }

// ParseScreeningOutcome validates a persisted or vendor-mapped outcome.
func ParseScreeningOutcome(s string) (ScreeningOutcome, error) {
	v := ScreeningOutcome(strings.ToUpper(strings.TrimSpace(s)))
	if !v.IsValid() {
		return "", apierror.Newf(apierror.CodeValidationFailed, "unknown screening outcome %q", s).
			WithDetail(apierror.Detail{
				Field: "outcome", Code: "UNKNOWN_SCREENING_OUTCOME",
				Message: "must be CLEAR, POTENTIAL_MATCH, CONFIRMED_MATCH or ERROR",
				RuleID:  "L5.SCREENING_OUTCOME_KNOWN",
			})
	}
	return v, nil
}

// MatchDetail describes one candidate hit.
//
// The list name and the score are retained alongside the matched name because a disposition has
// to be reviewable years later by someone who was not there: "cleared as a false positive"
// means nothing without "matched OFAC SDN at 0.71 on a name that differs in the middle initial".
type MatchDetail struct {
	// MatchedName is the name as it appears on the list, not as the merchant supplied it.
	MatchedName string
	// List names the source: OFAC SDN, EU consolidated, UK HMT, UN, or a tenant's own additions.
	List string
	// ListVersion and ListAsOf pin the exact list content used. A screening decision made
	// against last year's list must not be defensible with this year's.
	ListVersion string
	ListAsOf    time.Time
	// Score is the provider's confidence, 0-100. It is carried as an integer because a
	// threshold comparison against a float is a threshold comparison that behaves differently
	// on either side of a serialization hop.
	Score int
	// Category distinguishes a sanctions hit from a PEP designation from adverse media; they
	// have entirely different consequences and only the first is a hard block.
	Category string
	// Reference is the provider's identifier for this hit, so the raw response can be retrieved.
	Reference string
}

// ScreeningResult is one screening run's outcome, shaped as a port value.
//
// It is what a ScreeningProvider adapter returns and what the compliance gate reasons about. It
// carries no vendor types and no vendor semantics: every provider has its own scoring scale,
// its own list naming and its own idea of what "match" means, and letting any of that into the
// domain means switching providers is a rewrite of the compliance rules rather than a new
// adapter.
//
// Immutable: unexported fields, no setters. A disposition produces a new value.
type ScreeningResult struct {
	provider  string
	reference string
	subject   string

	outcome ScreeningOutcome
	matches []MatchDetail

	screenedAt       time.Time
	nextScreeningDue time.Time

	// disposition records a human's decision about a match, where one has been made.
	disposition *Disposition
}

// Disposition is a human's recorded decision about a screening hit.
//
// Every field is required by AML record-keeping: who decided, when, what they decided, and why.
// The reasoning is not optional prose — it is what a regulator reads when asking why a listed
// party's near-namesake was onboarded, and "reviewed" is not an answer.
type Disposition struct {
	Decision   DispositionDecision
	ReviewerID string
	// ApproverID is the second principal in the dual-control pair. Self-approval is structurally
	// impossible: HumanDisposition rejects an approver equal to the reviewer, which mirrors the
	// database's CHECK (approver_id <> reviewer_id).
	ApproverID string
	Reason     string
	DecidedAt  time.Time
}

// DispositionDecision is what a reviewer concluded about a hit.
type DispositionDecision string

const (
	// DispositionFalsePositive means the subject is not the listed party. The merchant proceeds.
	DispositionFalsePositive DispositionDecision = "FALSE_POSITIVE"
	// DispositionTrueMatch means the subject is the listed party. The merchant is blocked and
	// the case escalates to the tenant's obliged entity.
	DispositionTrueMatch DispositionDecision = "TRUE_MATCH"
	// DispositionEscalated means the reviewer could not conclude and has referred the case
	// onward. It is a real outcome, not an absence of one: an unresolved case that sits in
	// "pending" forever is how a hit gets forgotten.
	DispositionEscalated DispositionDecision = "ESCALATED"
)

// IsValid reports whether d is a known decision.
func (d DispositionDecision) IsValid() bool {
	return d == DispositionFalsePositive || d == DispositionTrueMatch || d == DispositionEscalated
}

// String satisfies fmt.Stringer.
func (d DispositionDecision) String() string { return string(d) }

// NewScreeningResultParams are the inputs to recording a screening run.
type NewScreeningResultParams struct {
	Provider  string
	Reference string
	// Subject is the screened party as submitted — a merchant's legal name or a principal's
	// name. It is retained so a run can be reproduced against the same input.
	Subject          string
	Outcome          ScreeningOutcome
	Matches          []MatchDetail
	ScreenedAt       time.Time
	NextScreeningDue time.Time
}

// NewScreeningResult records a screening run.
//
// Two consistency rules are enforced here rather than left to the adapters, because every
// adapter would otherwise have to remember them and one eventually would not:
//
//   - a CLEAR outcome may not carry matches, since a run that found something is not clear;
//   - a POTENTIAL_MATCH or CONFIRMED_MATCH must carry at least one match, since a hit with no
//     detail cannot be disposed of by a human and would block a merchant with no way forward.
func NewScreeningResult(p NewScreeningResultParams, clock shared.Clock) (ScreeningResult, error) {
	if strings.TrimSpace(p.Provider) == "" {
		return ScreeningResult{}, apierror.New(apierror.CodeValidationFailed,
			"a screening result requires the provider that produced it").
			WithDetail(apierror.Detail{
				Field: "provider", Code: "MISSING_PROVIDER",
				Message: "a screening decision is only defensible with its source",
				RuleID:  "L5.SCREENING_ATTRIBUTED",
			})
	}
	if !p.Outcome.IsValid() {
		return ScreeningResult{}, apierror.Newf(apierror.CodeValidationFailed,
			"unknown screening outcome %q", p.Outcome).
			WithDetail(apierror.Detail{
				Field: "outcome", Code: "UNKNOWN_SCREENING_OUTCOME",
				Message: "must be CLEAR, POTENTIAL_MATCH, CONFIRMED_MATCH or ERROR",
				RuleID:  "L5.SCREENING_OUTCOME_KNOWN",
			})
	}
	if p.Outcome == OutcomeClear && len(p.Matches) > 0 {
		return ScreeningResult{}, apierror.New(apierror.CodeValidationFailed,
			"a CLEAR screening result may not carry matches").
			WithDetail(apierror.Detail{
				Field: "matches", Code: "CLEAR_WITH_MATCHES",
				Message: "a run that returned hits is not clear; map the vendor response to POTENTIAL_MATCH",
				RuleID:  "L5.SCREENING_RESULT_CONSISTENT",
			})
	}
	if (p.Outcome == OutcomePotentialMatch || p.Outcome == OutcomeConfirmedMatch) && len(p.Matches) == 0 {
		return ScreeningResult{}, apierror.Newf(apierror.CodeValidationFailed,
			"a %s screening result must carry at least one match detail", p.Outcome).
			WithDetail(apierror.Detail{
				Field: "matches", Code: "MATCH_WITHOUT_DETAIL",
				Message: "a hit with no detail cannot be disposed of and blocks the merchant with no way forward",
				RuleID:  "L5.SCREENING_RESULT_CONSISTENT",
			})
	}

	now := clock.Now().UTC()
	screened := p.ScreenedAt
	if screened.IsZero() {
		screened = now
	}
	return ScreeningResult{
		provider:         strings.TrimSpace(p.Provider),
		reference:        strings.TrimSpace(p.Reference),
		subject:          strings.TrimSpace(p.Subject),
		outcome:          p.Outcome,
		matches:          append([]MatchDetail(nil), p.Matches...),
		screenedAt:       screened.UTC(),
		nextScreeningDue: p.NextScreeningDue.UTC(),
	}, nil
}

// Provider returns the screening vendor.
func (r ScreeningResult) Provider() string { return r.provider }

// Reference returns the vendor's identifier for the run, which is how the raw response is
// retrieved from the evidence store.
func (r ScreeningResult) Reference() string { return r.reference }

// Subject returns the screened party as submitted.
func (r ScreeningResult) Subject() string { return r.subject }

// Outcome returns the run's result.
func (r ScreeningResult) Outcome() ScreeningOutcome { return r.outcome }

// Matches returns a copy of the candidate hits.
func (r ScreeningResult) Matches() []MatchDetail { return append([]MatchDetail(nil), r.matches...) }

// ScreenedAt returns when the run happened.
func (r ScreeningResult) ScreenedAt() time.Time { return r.screenedAt }

// NextScreeningDue returns when the subject must be re-screened. Screening is not a
// one-off gate: lists change daily, and a merchant cleared last year against last year's list
// is not cleared today. A zero value means no schedule was set, which is itself a finding.
func (r ScreeningResult) NextScreeningDue() time.Time { return r.nextScreeningDue }

// Disposition returns the human decision recorded against this result, if any.
func (r ScreeningResult) Disposition() (Disposition, bool) {
	if r.disposition == nil {
		return Disposition{}, false
	}
	return *r.disposition, true
}

// IsClear reports whether the subject may proceed without human involvement.
func (r ScreeningResult) IsClear() bool { return r.outcome == OutcomeClear }

// IsOverdue reports whether the subject is past its re-screening date.
func (r ScreeningResult) IsOverdue(now time.Time) bool {
	return !r.nextScreeningDue.IsZero() && now.UTC().After(r.nextScreeningDue)
}

// RequiresHumanReview reports whether the result cannot be resolved by any automated path.
func (r ScreeningResult) RequiresHumanReview() bool {
	return r.outcome == OutcomePotentialMatch || r.outcome == OutcomeConfirmedMatch
}

// BlocksOnboarding reports whether the merchant may not proceed to ACTIVE on this result. An
// ERROR blocks: a screening that did not complete is not a screening that passed.
func (r ScreeningResult) BlocksOnboarding() bool {
	switch r.outcome {
	case OutcomeClear:
		return false
	case OutcomePotentialMatch:
		d, ok := r.Disposition()
		return !ok || d.Decision != DispositionFalsePositive
	default:
		return true
	}
}

// AutomatedOverride reports whether an automated path may clear this result, and returns a
// typed error when it may not.
//
// A CONFIRMED_MATCH is never overridable by any automated path. Not by a retry, not by a
// re-screen that happens to come back clear, not by a configuration flag, not by a workflow
// step, not by an operator script that calls the same code path. The method exists — and
// returns an error rather than a bool — precisely so that every automated caller has to handle
// the refusal explicitly and no caller can express "clear it anyway" as an omission.
//
// The reasoning is the reasoning behind the PAN detector having no per-merchant configuration
// (docs/compliance.md §1.4): scope and hard controls are enforced by *absence of capability*,
// not by policy. Onboarding a confirmed sanctions match is a criminal matter for the obliged
// entity, and the pressure to make it go away arrives exactly when a large merchant is blocked
// on a Friday afternoon. Where the capability does not exist, nobody can be persuaded to use it
// under that pressure — and there is no code path to review later to find out whether they did.
//
// A POTENTIAL_MATCH is refused too, for a weaker reason: it needs a human, and an automated
// path that could dispose of it would make the human optional. The disposition path is
// HumanDisposition, which requires two named principals and a stated reason.
//
// An ERROR is refused because a screening that did not complete has not cleared anything.
func (r ScreeningResult) AutomatedOverride() error {
	switch r.outcome {
	case OutcomeClear:
		return nil
	case OutcomeConfirmedMatch:
		return apierror.New(apierror.CodeForbidden,
			"a confirmed sanctions or PEP match cannot be cleared by any automated path").
			WithDetail(apierror.Detail{
				Field: "outcome", Code: "CONFIRMED_MATCH_NOT_OVERRIDABLE",
				Message: "a confirmed match is a hard stop; escalate to the obliged entity's compliance function",
				RuleID:  "L5.CONFIRMED_MATCH_IS_TERMINAL",
			})
	case OutcomePotentialMatch:
		return apierror.New(apierror.CodeForbidden,
			"a potential match requires a dual-controlled human disposition").
			WithDetail(apierror.Detail{
				Field: "outcome", Code: "REQUIRES_HUMAN_DISPOSITION",
				Message: "record a disposition with a reviewer, an approver and a stated reason",
				RuleID:  "L5.SCREENING_HIT_REQUIRES_DISPOSITION",
			})
	default:
		return apierror.New(apierror.CodeDependencyFailure,
			"the screening run did not complete and has cleared nothing").
			WithDetail(apierror.Detail{
				Field: "outcome", Code: "SCREENING_INCOMPLETE",
				Message: "re-run the screening; a provider outage is not a clear result",
				RuleID:  "L5.SCREENING_MUST_COMPLETE",
			})
	}
}

// HumanDisposition records a reviewer's decision and returns the updated result. The receiver is
// unchanged.
//
// It enforces three things the process depends on:
//
//   - dual control: the approver may not be the reviewer, mirroring the database's
//     CHECK (approver_id <> reviewer_id);
//   - a stated reason, because a disposition with no reasoning is unreviewable years later;
//   - that a CONFIRMED_MATCH may not be dispositioned as a false positive. A human may escalate
//     it or confirm it, and that is all. The distinction between this and AutomatedOverride is
//     worth being explicit about: the automated path is refused entirely, and the human path is
//     narrowed to the decisions a human is actually entitled to make. Someone with better
//     information than the screening provider had can conclude a *potential* match is the wrong
//     person; nobody gets to conclude that a party who is on the list is not on the list.
func (r ScreeningResult) HumanDisposition(d Disposition, clock shared.Clock) (ScreeningResult, error) {
	if !d.Decision.IsValid() {
		return ScreeningResult{}, apierror.Newf(apierror.CodeValidationFailed,
			"unknown screening disposition %q", d.Decision).
			WithDetail(apierror.Detail{
				Field: "decision", Code: "UNKNOWN_DISPOSITION",
				Message: "must be FALSE_POSITIVE, TRUE_MATCH or ESCALATED",
				RuleID:  "L5.SCREENING_DISPOSITION_KNOWN",
			})
	}
	if !r.RequiresHumanReview() {
		return ScreeningResult{}, apierror.Newf(apierror.CodeValidationFailed,
			"a %s screening result has nothing to dispose of", r.outcome)
	}
	if strings.TrimSpace(d.ReviewerID) == "" || strings.TrimSpace(d.ApproverID) == "" {
		return ScreeningResult{}, apierror.New(apierror.CodeValidationFailed,
			"a screening disposition requires a reviewer and a separate approver").
			WithDetail(apierror.Detail{
				Field: "approverId", Code: "DUAL_CONTROL_REQUIRED",
				Message: "screening dispositions are dual-controlled",
				RuleID:  "L5.SCREENING_DISPOSITION_DUAL_CONTROL",
			})
	}
	if d.ReviewerID == d.ApproverID {
		return ScreeningResult{}, apierror.New(apierror.CodeForbidden,
			"a screening disposition may not be self-approved").
			WithDetail(apierror.Detail{
				Field: "approverId", Code: "SELF_APPROVAL",
				Message: "the approver must be a different principal from the reviewer",
				RuleID:  "L5.SCREENING_DISPOSITION_DUAL_CONTROL",
			})
	}
	if strings.TrimSpace(d.Reason) == "" {
		return ScreeningResult{}, apierror.New(apierror.CodeValidationFailed,
			"a screening disposition requires a stated reason").
			WithDetail(apierror.Detail{
				Field: "reason", Code: "REASON_REQUIRED",
				Message: "\"reviewed\" is not a reason; state what distinguished the subject from the listed party",
				RuleID:  "L5.SCREENING_DISPOSITION_REASONED",
			})
	}
	if r.outcome == OutcomeConfirmedMatch && d.Decision == DispositionFalsePositive {
		return ScreeningResult{}, apierror.New(apierror.CodeForbidden,
			"a confirmed match cannot be dispositioned as a false positive").
			WithDetail(apierror.Detail{
				Field: "decision", Code: "CONFIRMED_MATCH_NOT_OVERRIDABLE",
				Message: "a confirmed match may be escalated or confirmed, never cleared",
				RuleID:  "L5.CONFIRMED_MATCH_IS_TERMINAL",
			})
	}

	if d.DecidedAt.IsZero() {
		d.DecidedAt = clock.Now()
	}
	d.DecidedAt = d.DecidedAt.UTC()
	r.matches = append([]MatchDetail(nil), r.matches...)
	r.disposition = &d
	return r, nil
}

// HighestScore returns the strongest candidate score in the result, which is what the risk
// rating and the re-screening cadence key off.
func (r ScreeningResult) HighestScore() int {
	best := 0
	for _, m := range r.matches {
		if m.Score > best {
			best = m.Score
		}
	}
	return best
}

// Lists returns the distinct lists that produced hits, sorted. Reporting groups by this: a spike
// in hits from one list usually means that list was refreshed, not that the merchant base
// changed.
func (r ScreeningResult) Lists() []string {
	seen := make(map[string]struct{}, len(r.matches))
	for _, m := range r.matches {
		if m.List != "" {
			seen[m.List] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for l := range seen {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// RehydrateScreeningParams carries a persisted screening run back into the domain.
type RehydrateScreeningParams struct {
	Provider         string
	Reference        string
	Subject          string
	Outcome          ScreeningOutcome
	Matches          []MatchDetail
	ScreenedAt       time.Time
	NextScreeningDue time.Time
	Disposition      *Disposition
}

// RehydrateScreeningResult reconstructs a ScreeningResult from persisted state, refusing an
// outcome this binary does not know rather than coercing it into the nearest one — which, for
// this type, would risk coercing a match into a clear.
func RehydrateScreeningResult(p RehydrateScreeningParams) (ScreeningResult, error) {
	if !p.Outcome.IsValid() {
		return ScreeningResult{}, apierror.Newf(apierror.CodeInternalError,
			"screening run %s has unknown outcome %q; this row may have been written by a newer version of the service",
			p.Reference, p.Outcome)
	}
	var disp *Disposition
	if p.Disposition != nil {
		d := *p.Disposition
		disp = &d
	}
	return ScreeningResult{
		provider:         p.Provider,
		reference:        p.Reference,
		subject:          p.Subject,
		outcome:          p.Outcome,
		matches:          append([]MatchDetail(nil), p.Matches...),
		screenedAt:       p.ScreenedAt.UTC(),
		nextScreeningDue: p.NextScreeningDue.UTC(),
		disposition:      disp,
	}, nil
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: NFR-41.
//
// Sanctions and PEP screening, its dual-controlled dispositions and the evidence they leave
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
