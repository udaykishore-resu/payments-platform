package compliance

import (
	"errors"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

func sanctionsMatch(score int) MatchDetail {
	return MatchDetail{
		MatchedName: "OKAFOR, J", List: "OFAC_SDN", ListVersion: "2026-03-02",
		ListAsOf: time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		Score:    score, Category: "SANCTIONS", Reference: "hit_1",
	}
}

func mustScreening(t *testing.T, outcome ScreeningOutcome, matches ...MatchDetail) ScreeningResult {
	t.Helper()
	r, err := NewScreeningResult(NewScreeningResultParams{
		Provider: "acme-screening", Reference: "scr_1", Subject: "J Okafor Ltd",
		Outcome: outcome, Matches: matches,
		NextScreeningDue: time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC),
	}, testClock())
	if err != nil {
		t.Fatalf("NewScreeningResult(%s): %v", outcome, err)
	}
	return r
}

// TestConfirmedMatchIsNotOverridableByAutomation is the hard control: no automated path may
// clear a confirmed sanctions or PEP match, and the refusal is a typed error the caller must
// handle rather than a boolean it can ignore.
func TestConfirmedMatchIsNotOverridableByAutomation(t *testing.T) {
	// Verifies: NFR-41.
	t.Parallel()

	tests := []struct {
		name     string
		result   func(t *testing.T) ScreeningResult
		wantErr  bool
		wantCode apierror.Code
	}{
		{
			name:   "clear passes",
			result: func(t *testing.T) ScreeningResult { return mustScreening(t, OutcomeClear) },
		},
		{
			name:     "confirmed match is refused",
			result:   func(t *testing.T) ScreeningResult { return mustScreening(t, OutcomeConfirmedMatch, sanctionsMatch(98)) },
			wantErr:  true,
			wantCode: apierror.CodeForbidden,
		},
		{
			name:     "potential match needs a human",
			result:   func(t *testing.T) ScreeningResult { return mustScreening(t, OutcomePotentialMatch, sanctionsMatch(71)) },
			wantErr:  true,
			wantCode: apierror.CodeForbidden,
		},
		{
			name:     "an incomplete run has cleared nothing",
			result:   func(t *testing.T) ScreeningResult { return mustScreening(t, OutcomeError) },
			wantErr:  true,
			wantCode: apierror.CodeDependencyFailure,
		},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.result(t).AutomatedOverride()
			if tc.wantErr != (err != nil) {
				t.Fatalf("AutomatedOverride() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			var pe *apierror.Error
			if !errors.As(err, &pe) {
				t.Fatalf("error is not an *apierror.Error: %T", err)
			}
			if pe.Code != tc.wantCode {
				t.Errorf("code = %s, want %s", pe.Code, tc.wantCode)
			}
			if len(pe.Details) == 0 || pe.Details[0].RuleID == "" {
				t.Error("refusal carries no rule ID for the caller to act on")
			}
		})
	}
}

// TestConfirmedMatchCannotBeClearedByAHumanEither — a human may escalate or confirm a confirmed
// match. Nobody gets to conclude that a party on the list is not on the list.
func TestConfirmedMatchCannotBeClearedByAHumanEither(t *testing.T) {
	t.Parallel()

	confirmed := mustScreening(t, OutcomeConfirmedMatch, sanctionsMatch(98))
	base := Disposition{
		ReviewerID: "usr_reviewer", ApproverID: "usr_approver",
		Reason: "identity documents match the listed party",
	}

	falsePositive := base
	falsePositive.Decision = DispositionFalsePositive
	if _, err := confirmed.HumanDisposition(falsePositive, testClock()); err == nil {
		t.Fatal("a confirmed match was dispositioned as a false positive")
	}

	for _, decision := range []DispositionDecision{DispositionTrueMatch, DispositionEscalated} {
		d := base
		d.Decision = decision
		got, err := confirmed.HumanDisposition(d, testClock())
		if err != nil {
			t.Fatalf("HumanDisposition(%s): %v", decision, err)
		}
		rec, ok := got.Disposition()
		if !ok || rec.Decision != decision || rec.DecidedAt.IsZero() {
			t.Fatalf("disposition not recorded: %+v", rec)
		}
		if _, had := confirmed.Disposition(); had {
			t.Fatal("HumanDisposition mutated the receiver")
		}
		if !got.BlocksOnboarding() {
			t.Error("a confirmed match still blocks onboarding whatever the disposition")
		}
	}
}

func TestHumanDispositionRequiresDualControlAndAReason(t *testing.T) {
	// Verifies: NFR-34, NFR-41.
	t.Parallel()

	potential := mustScreening(t, OutcomePotentialMatch, sanctionsMatch(71))
	valid := Disposition{
		Decision: DispositionFalsePositive, ReviewerID: "usr_reviewer",
		ApproverID: "usr_approver", Reason: "middle name and date of birth both differ",
	}
	mutate := func(f func(*Disposition)) Disposition {
		d := valid
		f(&d)
		return d
	}

	tests := []struct {
		name    string
		d       Disposition
		wantErr bool
	}{
		{"valid", valid, false},
		{"unknown decision", mutate(func(d *Disposition) { d.Decision = "MAYBE" }), true},
		{"no reviewer", mutate(func(d *Disposition) { d.ReviewerID = "" }), true},
		{"no approver", mutate(func(d *Disposition) { d.ApproverID = "" }), true},
		{"self-approved", mutate(func(d *Disposition) { d.ApproverID = d.ReviewerID }), true},
		{"no reason", mutate(func(d *Disposition) { d.Reason = "  " }), true},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := potential.HumanDisposition(tc.d, testClock())
			if tc.wantErr != (err != nil) {
				t.Fatalf("HumanDisposition() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}

	// There is nothing to dispose of on a clear result.
	if _, err := mustScreening(t, OutcomeClear).HumanDisposition(valid, testClock()); err == nil {
		t.Error("a clear result accepted a disposition")
	}
}

func TestBlocksOnboarding(t *testing.T) {
	t.Parallel()

	clock := testClock()
	cleared, err := mustScreening(t, OutcomePotentialMatch, sanctionsMatch(60)).
		HumanDisposition(Disposition{
			Decision: DispositionFalsePositive, ReviewerID: "a", ApproverID: "b",
			Reason: "different registered company number",
		}, clock)
	if err != nil {
		t.Fatalf("HumanDisposition: %v", err)
	}

	tests := []struct {
		name   string
		result ScreeningResult
		want   bool
	}{
		{"clear", mustScreening(t, OutcomeClear), false},
		{"undisposed potential match", mustScreening(t, OutcomePotentialMatch, sanctionsMatch(60)), true},
		{"potential match cleared by a human", cleared, false},
		{"confirmed match", mustScreening(t, OutcomeConfirmedMatch, sanctionsMatch(99)), true},
		{"provider error", mustScreening(t, OutcomeError), true},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.result.BlocksOnboarding(); got != tc.want {
				t.Fatalf("BlocksOnboarding() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewScreeningResultConsistency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		params  NewScreeningResultParams
		wantErr bool
	}{
		{
			name:   "clear with no matches",
			params: NewScreeningResultParams{Provider: "acme", Outcome: OutcomeClear},
		},
		{
			name: "clear with matches is a contradiction",
			params: NewScreeningResultParams{
				Provider: "acme", Outcome: OutcomeClear, Matches: []MatchDetail{sanctionsMatch(50)},
			},
			wantErr: true,
		},
		{
			name:    "potential match with no detail cannot be disposed of",
			params:  NewScreeningResultParams{Provider: "acme", Outcome: OutcomePotentialMatch},
			wantErr: true,
		},
		{
			name:    "confirmed match with no detail",
			params:  NewScreeningResultParams{Provider: "acme", Outcome: OutcomeConfirmedMatch},
			wantErr: true,
		},
		{
			name:    "no provider",
			params:  NewScreeningResultParams{Outcome: OutcomeClear},
			wantErr: true,
		},
		{
			name:    "unknown outcome",
			params:  NewScreeningResultParams{Provider: "acme", Outcome: "PROBABLY_FINE"},
			wantErr: true,
		},
		{
			name: "error outcome needs no matches",
			params: NewScreeningResultParams{
				Provider: "acme", Outcome: OutcomeError,
			},
		},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewScreeningResult(tc.params, testClock())
			if tc.wantErr != (err != nil) {
				t.Fatalf("NewScreeningResult() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestScreeningResultAccessorsCopy(t *testing.T) {
	t.Parallel()

	r := mustScreening(t, OutcomePotentialMatch, sanctionsMatch(71), sanctionsMatch(55))
	got := r.Matches()
	got[0].Score = 1
	if r.Matches()[0].Score != 71 {
		t.Fatal("Matches() returned the live slice")
	}
	if r.HighestScore() != 71 {
		t.Errorf("HighestScore() = %d, want 71", r.HighestScore())
	}
	if lists := r.Lists(); len(lists) != 1 || lists[0] != "OFAC_SDN" {
		t.Errorf("Lists() = %v", lists)
	}
	if !r.RequiresHumanReview() {
		t.Error("a potential match must require human review")
	}
	if r.IsOverdue(time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)) {
		t.Error("reported overdue before the next screening date")
	}
	if !r.IsOverdue(time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)) {
		t.Error("not reported overdue after the next screening date")
	}
}

func TestRehydrateScreeningRejectsUnknownOutcome(t *testing.T) {
	t.Parallel()

	if _, err := RehydrateScreeningResult(RehydrateScreeningParams{
		Reference: "scr_1", Outcome: "SUPERSEDED",
	}); err == nil {
		t.Fatal("RehydrateScreeningResult accepted an outcome this binary does not know")
	}
	got, err := RehydrateScreeningResult(RehydrateScreeningParams{
		Reference: "scr_1", Outcome: OutcomeConfirmedMatch, Matches: []MatchDetail{sanctionsMatch(99)},
	})
	if err != nil {
		t.Fatalf("RehydrateScreeningResult: %v", err)
	}
	if err := got.AutomatedOverride(); err == nil {
		t.Fatal("a rehydrated confirmed match lost its non-overridability")
	}
}
