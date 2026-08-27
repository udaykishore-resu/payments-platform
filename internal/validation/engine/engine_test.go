package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/validation/engine"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// subject is the trivial subject the engine tests evaluate over. The engine does not care what
// a subject is, so the tests deliberately use the smallest thing that can carry a decision.
type subject struct {
	n     int
	label string
}

func passing(id engine.RuleID) engine.Rule[subject] {
	return engine.RuleFunc[subject]{
		RuleID: id, Sev: engine.Error,
		Eval: func(context.Context, subject) engine.Outcome { return engine.Pass(id) },
	}
}

func failing(id engine.RuleID, code string) engine.Rule[subject] {
	return engine.RuleFunc[subject]{
		RuleID: id, Sev: engine.Error,
		Eval: func(context.Context, subject) engine.Outcome {
			return engine.Fail(id, code, "/n", "n is wrong", "Send a different n.")
		},
	}
}

func warning(id engine.RuleID) engine.Rule[subject] {
	return engine.RuleFunc[subject]{
		RuleID: id, Sev: engine.Warning,
		Eval: func(context.Context, subject) engine.Outcome {
			return engine.FailWarning(id, "", "/label", "label is odd", "Consider a different label.")
		},
	}
}

func TestRuleIDWellFormedness(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		id    engine.RuleID
		want  bool
		level int
	}{
		{"canonical", "L5.AMOUNT_WITHIN_MERCHANT_LIMIT", true, 5},
		{"shortest legal tail", "L1.ABCD", true, 1},
		{"level 7", "L7.LEDGER_IS_APPEND_ONLY", true, 7},
		{"digits and underscores", "L1.TLS_VERSION_AT_LEAST_1_2", true, 1},
		{"level 0 is not a level", "L0.SOMETHING", false, 0},
		{"level 8 is not a level", "L8.SOMETHING", false, 0},
		{"lower case tail", "L5.amount", false, 5},
		{"missing dot", "L5AMOUNT_TOO_BIG", false, 0},
		{"tail too short", "L5.ABC", false, 5},
		{"empty", "", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.id.IsWellFormed(); got != tc.want {
				t.Fatalf("IsWellFormed(%q) = %v, want %v", tc.id, got, tc.want)
			}
			if got := tc.id.Level(); got != tc.level {
				t.Fatalf("Level(%q) = %d, want %d", tc.id, got, tc.level)
			}
		})
	}
}

func TestCollectAllGathersEveryFailure(t *testing.T) {
	t.Parallel()
	set := engine.RuleSet[subject]{
		Name: "test.collect",
		Mode: engine.CollectAll,
		Rules: []engine.Rule[subject]{
			failing("L1.FIRST_FAILURE", "VALIDATION_FAILED"),
			passing("L1.MIDDLE_PASSES"),
			failing("L1.SECOND_FAILURE", "VALIDATION_FAILED"),
			warning("L1.A_WARNING_HERE"),
		},
		Stages: enforceAll(),
	}

	rep := set.Evaluate(context.Background(), subject{})

	if rep.OK() {
		t.Fatal("report is OK despite two error failures")
	}
	if got := len(rep.Outcomes); got != 4 {
		t.Fatalf("evaluated %d rules, want 4", got)
	}
	if got := len(rep.Errors()); got != 2 {
		t.Fatalf("Errors() returned %d, want 2", got)
	}
	if got := len(rep.Warnings()); got != 1 {
		t.Fatalf("Warnings() returned %d, want 1", got)
	}
	if got := len(rep.Failures()); got != 3 {
		t.Fatalf("Failures() returned %d, want 3 (errors + warnings)", got)
	}
	if got := len(rep.Passed()); got != 1 {
		t.Fatalf("Passed() returned %d, want 1", got)
	}
}

func TestShortCircuitStopsAtFirstError(t *testing.T) {
	t.Parallel()
	set := engine.RuleSet[subject]{
		Name: "test.shortcircuit",
		Mode: engine.ShortCircuit,
		Rules: []engine.Rule[subject]{
			passing("L1.RUNS_FIRST_OK"),
			failing("L1.STOPS_HERE_NOW", "UNAUTHENTICATED"),
			failing("L1.NEVER_EVALUATED", "VALIDATION_FAILED"),
		},
		Stages: enforceAll(),
	}

	rep := set.Evaluate(context.Background(), subject{})

	if len(rep.Outcomes) != 2 {
		t.Fatalf("evaluated %d rules, want 2 (stop after the first error)", len(rep.Outcomes))
	}
	if _, ran := rep.For("L1.NEVER_EVALUATED"); ran {
		t.Fatal("a rule after the first error was evaluated")
	}
	if rep.OK() {
		t.Fatal("report is OK despite an error failure")
	}
}

func TestShortCircuitDoesNotStopOnWarning(t *testing.T) {
	t.Parallel()
	set := engine.RuleSet[subject]{
		Name: "test.shortcircuit.warning",
		Mode: engine.ShortCircuit,
		Rules: []engine.Rule[subject]{
			warning("L1.JUST_A_WARNING"),
			passing("L1.STILL_EVALUATED"),
		},
		Stages: enforceAll(),
	}

	rep := set.Evaluate(context.Background(), subject{})

	if len(rep.Outcomes) != 2 {
		t.Fatalf("evaluated %d rules, want 2: a warning must not short-circuit", len(rep.Outcomes))
	}
	if !rep.OK() {
		t.Fatal("a warning made the report not OK")
	}
}

func TestPreconditionSkipsRatherThanPasses(t *testing.T) {
	t.Parallel()
	skipped := engine.RuleFunc[subject]{
		RuleID:  "L1.ONLY_WHEN_LABELLED",
		Sev:     engine.Error,
		Applies: func(s subject) bool { return s.label != "" },
		Eval: func(context.Context, subject) engine.Outcome {
			return engine.Fail("L1.ONLY_WHEN_LABELLED", "VALIDATION_FAILED", "/label", "no", "Set a label.")
		},
	}
	set := engine.RuleSet[subject]{
		Name: "test.precondition", Mode: engine.CollectAll,
		Rules: []engine.Rule[subject]{skipped}, Stages: enforceAll(),
	}

	rep := set.Evaluate(context.Background(), subject{})

	if !rep.WasSkipped("L1.ONLY_WHEN_LABELLED") {
		t.Fatal("rule whose precondition was false is not recorded as skipped")
	}
	if _, ran := rep.For("L1.ONLY_WHEN_LABELLED"); ran {
		t.Fatal("a rule whose precondition was false produced an outcome")
	}
	if !rep.OK() {
		t.Fatal("a skipped failing rule made the report not OK")
	}
}

func TestAllCombinatorReportsUnderOneID(t *testing.T) {
	t.Parallel()
	composite := engine.All[subject]("L1.COMPOSITE_ASSERTION",
		passing("L1.PART_ONE_HERE"),
		failing("L1.PART_TWO_HERE", "VALIDATION_FAILED"),
		failing("L1.PART_THREE_HERE", "CURRENCY_NOT_SUPPORTED"),
	)

	out := composite.Evaluate(context.Background(), subject{})

	if out.Passed {
		t.Fatal("a composite with a failing part passed")
	}
	if out.Rule != "L1.COMPOSITE_ASSERTION" {
		t.Fatalf("composite reported under %q, want the composite ID", out.Rule)
	}
	if out.Code != "VALIDATION_FAILED" {
		t.Fatalf("composite reported code %q, want the first failing part's code", out.Code)
	}

	allPass := engine.All[subject]("L1.COMPOSITE_ALL_OK", passing("L1.PART_ONE_HERE"), passing("L1.PART_TWO_HERE"))
	if out := allPass.Evaluate(context.Background(), subject{}); !out.Passed {
		t.Fatal("a composite of passing parts failed")
	}
}

func TestWhenCombinatorAddsPrecondition(t *testing.T) {
	t.Parallel()
	r := engine.When(func(s subject) bool { return s.n > 10 },
		failing("L1.ONLY_FOR_BIG_N", "VALIDATION_FAILED"))

	if engine.Applies(r, subject{n: 5}) {
		t.Fatal("When did not suppress the rule below the threshold")
	}
	if !engine.Applies(r, subject{n: 11}) {
		t.Fatal("When suppressed the rule above the threshold")
	}
	if out := r.Evaluate(context.Background(), subject{n: 11}); out.Passed {
		t.Fatal("the wrapped rule passed when it should have failed")
	}
}

func TestLiftProjectsSubject(t *testing.T) {
	t.Parallel()
	// The underlying assertion is written once, over an int.
	positive := engine.RuleFunc[int]{
		RuleID: "L1.INNER_IS_POSITIVE", Sev: engine.Error,
		Eval: func(_ context.Context, n int) engine.Outcome {
			if n > 0 {
				return engine.Pass("L1.INNER_IS_POSITIVE")
			}
			return engine.Fail("L1.INNER_IS_POSITIVE", "VALIDATION_FAILED", "/n",
				"n must be positive", "Send a positive n.")
		},
	}

	lifted := engine.Lift[subject, int]("L5.AMOUNT_IS_POSITIVE_TEST", func(s subject) int { return s.n }, positive)

	if out := lifted.Evaluate(context.Background(), subject{n: 1}); !out.Passed {
		t.Fatal("lifted rule failed on a positive projection")
	}
	out := lifted.Evaluate(context.Background(), subject{n: -1})
	if out.Passed {
		t.Fatal("lifted rule passed on a negative projection")
	}
	if out.Rule != "L5.AMOUNT_IS_POSITIVE_TEST" {
		t.Fatalf("lifted rule reported %q, want the lifted ID", out.Rule)
	}
	if out.Field != "/n" {
		t.Fatalf("lifted rule dropped the inner field path, got %q", out.Field)
	}
}

func TestNamedPreservesBehaviourAndPurityMarker(t *testing.T) {
	t.Parallel()
	impure := engine.MarkImpure[subject](failing("L3.NETWORK_THING_HERE", "SERVICE_UNAVAILABLE"))
	renamed := engine.Named[subject]("L3.RENAMED_NETWORK_THING", impure)

	if renamed.ID() != "L3.RENAMED_NETWORK_THING" {
		t.Fatalf("Named produced ID %q", renamed.ID())
	}
	if !engine.IsImpure(renamed) {
		t.Fatal("Named lost the impurity marker")
	}
	if out := renamed.Evaluate(context.Background(), subject{}); out.Passed {
		t.Fatal("Named changed the outcome")
	}
}

func TestReportAsErrorCarriesEveryRuleID(t *testing.T) {
	t.Parallel()
	set := engine.RuleSet[subject]{
		Name: "test.aserror", Mode: engine.CollectAll,
		Rules: []engine.Rule[subject]{
			failing("L1.FIRST_PROBLEM_ID", "CURRENCY_NOT_SUPPORTED"),
			failing("L1.SECOND_PROBLEM_ID", "VALIDATION_FAILED"),
			warning("L1.THIRD_PROBLEM_ID"),
			passing("L1.NOT_A_PROBLEM_ID"),
		},
		Stages: enforceAll(),
	}

	err := set.Evaluate(context.Background(), subject{}).AsError()
	if err == nil {
		t.Fatal("AsError returned nil for a failing report")
	}
	if err.Code != apierror.CodeCurrencyNotSupported {
		t.Fatalf("top-level code is %q, want the first error's code", err.Code)
	}
	if len(err.Details) != 3 {
		t.Fatalf("AsError produced %d details, want 3 (two errors and a warning)", len(err.Details))
	}
	want := map[string]bool{
		"L1.FIRST_PROBLEM_ID":  false,
		"L1.SECOND_PROBLEM_ID": false,
		"L1.THIRD_PROBLEM_ID":  false,
	}
	for _, d := range err.Details {
		if _, ok := want[d.RuleID]; !ok {
			t.Fatalf("detail carries unexpected rule ID %q", d.RuleID)
		}
		want[d.RuleID] = true
		if d.Message == "" {
			t.Fatalf("detail for %s has no message", d.RuleID)
		}
	}
	for id, seen := range want {
		if !seen {
			t.Fatalf("detail for %s is missing", id)
		}
	}

	var platform *apierror.Error
	if !errors.As(error(err), &platform) {
		t.Fatal("AsError did not produce an *apierror.Error")
	}
}

func TestReportAsErrorIsNilWhenOK(t *testing.T) {
	t.Parallel()
	set := engine.RuleSet[subject]{
		Name: "test.ok", Mode: engine.CollectAll,
		Rules:  []engine.Rule[subject]{passing("L1.EVERYTHING_IS_OK"), warning("L1.ONLY_A_WARNING")},
		Stages: enforceAll(),
	}
	if err := set.Evaluate(context.Background(), subject{}).AsError(); err != nil {
		t.Fatalf("AsError returned %v for a report with no error failures", err)
	}
}

// recorder captures what the metric hook was told, which is the only observable effect a
// shadow rule is permitted to have.
type recorder struct {
	outcomes []engine.Outcome
}

func (r *recorder) RecordOutcome(_ context.Context, _ string, o engine.Outcome) {
	r.outcomes = append(r.outcomes, o)
}

func TestShadowStageRecordsButNeverFails(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	set := engine.RuleSet[subject]{
		Name: "test.shadow", Mode: engine.CollectAll,
		Rules: []engine.Rule[subject]{
			failing("L5.NEW_RULE_IN_SHADOW", "AMOUNT_EXCEEDS_LIMIT"),
			passing("L5.ESTABLISHED_RULE_OK"),
		},
		Stages: engine.StageFunc(func(id engine.RuleID) engine.Stage {
			if id == "L5.NEW_RULE_IN_SHADOW" {
				return engine.Shadow
			}
			return engine.Enforce
		}),
		Metrics: rec,
	}

	rep := set.Evaluate(context.Background(), subject{})

	if !rep.OK() {
		t.Fatal("a shadow rule's failure made the report not OK")
	}
	if err := rep.AsError(); err != nil {
		t.Fatalf("a shadow rule's failure reached AsError: %v", err)
	}
	if _, visible := rep.For("L5.NEW_RULE_IN_SHADOW"); visible {
		t.Fatal("a shadow outcome is visible in Report.Outcomes")
	}
	shadowed, ok := rep.ShadowFor("L5.NEW_RULE_IN_SHADOW")
	if !ok {
		t.Fatal("the shadow outcome was not recorded in Report.Shadowed")
	}
	if shadowed.Passed {
		t.Fatal("the shadow outcome lost its failure")
	}
	if len(rec.outcomes) != 2 {
		t.Fatalf("metric hook saw %d outcomes, want 2 (shadow outcomes must still be recorded)", len(rec.outcomes))
	}
	sawShadow := false
	for _, o := range rec.outcomes {
		if o.Rule == "L5.NEW_RULE_IN_SHADOW" {
			sawShadow = true
			if o.Stage != engine.Shadow {
				t.Fatalf("recorded outcome stage is %v, want shadow", o.Stage)
			}
		}
	}
	if !sawShadow {
		t.Fatal("the shadow rule's outcome never reached the metric hook")
	}
}

func TestShadowRuleDoesNotShortCircuit(t *testing.T) {
	t.Parallel()
	set := engine.RuleSet[subject]{
		Name: "test.shadow.shortcircuit", Mode: engine.ShortCircuit,
		Rules: []engine.Rule[subject]{
			failing("L6.SHADOWED_FAILURE_ID", "GATEWAY_CONTRACT_VIOLATION"),
			passing("L6.MUST_STILL_EVALUATE"),
		},
		Stages: engine.StageFunc(func(id engine.RuleID) engine.Stage {
			if id == "L6.SHADOWED_FAILURE_ID" {
				return engine.Shadow
			}
			return engine.Enforce
		}),
	}

	rep := set.Evaluate(context.Background(), subject{})

	if _, ran := rep.For("L6.MUST_STILL_EVALUATE"); !ran {
		t.Fatal("a shadow failure short-circuited the set")
	}
}

func TestWarnStageDemotesSeverity(t *testing.T) {
	t.Parallel()
	set := engine.RuleSet[subject]{
		Name: "test.warn", Mode: engine.CollectAll,
		Rules: []engine.Rule[subject]{failing("L5.PROMOTED_TO_WARN_ID", "AMOUNT_EXCEEDS_LIMIT")},
		Stages: engine.StageFunc(func(engine.RuleID) engine.Stage {
			return engine.Warn
		}),
	}

	rep := set.Evaluate(context.Background(), subject{})

	if !rep.OK() {
		t.Fatal("a warn-stage rule blocked the operation")
	}
	out, ok := rep.For("L5.PROMOTED_TO_WARN_ID")
	if !ok {
		t.Fatal("a warn-stage outcome is not visible in the report")
	}
	if out.Severity != engine.Warning {
		t.Fatalf("warn-stage outcome severity is %v, want WARNING", out.Severity)
	}
	if len(rep.Warnings()) != 1 {
		t.Fatalf("warn-stage outcome did not appear in Warnings()")
	}
}

func TestStagesResolveOverridesAndExpiry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)
	base := engine.StageFunc(func(engine.RuleID) engine.Stage { return engine.Enforce })

	stages := engine.NewStages(base).
		OverrideForTenant("tenant_migrating", "L5.SOME_RULE_HERE", engine.StageOverride{
			Stage: engine.Warn, ExpiresAt: now.Add(time.Hour), Reason: "migration window",
		}).
		Override("L5.OTHER_RULE_HERE", engine.StageOverride{
			Stage: engine.Shadow, ExpiresAt: now.Add(-time.Minute), Reason: "expired",
		})

	if got := stages.Resolve("tenant_migrating", "L5.SOME_RULE_HERE", now); got != engine.Warn {
		t.Fatalf("tenant override not applied: got %v", got)
	}
	if got := stages.Resolve("tenant_other", "L5.SOME_RULE_HERE", now); got != engine.Enforce {
		t.Fatalf("tenant override leaked to another tenant: got %v", got)
	}
	if got := stages.Resolve("tenant_migrating", "L5.SOME_RULE_HERE", now.Add(2*time.Hour)); got != engine.Enforce {
		t.Fatalf("expired tenant override still applied: got %v", got)
	}
	if got := stages.Resolve("tenant_other", "L5.OTHER_RULE_HERE", now); got != engine.Enforce {
		t.Fatalf("expired global override still applied: got %v", got)
	}

	bound := stages.Bind("tenant_migrating", now)
	if got := bound.StageFor("L5.SOME_RULE_HERE"); got != engine.Warn {
		t.Fatalf("bound lookup returned %v", got)
	}
	if active := stages.ActiveOverrides("tenant_migrating", now); len(active) != 1 ||
		active[0] != "L5.SOME_RULE_HERE" {
		t.Fatalf("ActiveOverrides = %v, want just the unexpired one", active)
	}
}

func TestParseStageFailsClosedOnUnknownInput(t *testing.T) {
	t.Parallel()
	if got, ok := engine.ParseStage("nonsense"); ok || got != engine.Shadow {
		t.Fatalf("ParseStage(nonsense) = %v, %v; want shadow, false", got, ok)
	}
	for _, s := range []engine.Stage{engine.Shadow, engine.Warn, engine.Enforce} {
		got, ok := engine.ParseStage(s.String())
		if !ok || got != s {
			t.Fatalf("ParseStage(%q) round trip failed", s.String())
		}
	}
}

func TestRegistryRejectsDuplicateAndMalformedIDs(t *testing.T) {
	t.Parallel()
	reg := engine.NewRegistry()
	reg.Register(engine.Registration{
		ID: "L1.SOME_ASSERTION", Severity: engine.Error, Remediation: "Fix it.",
		Description: "first", Pure: true, Stage: engine.Enforce,
	})

	if got := reg.Count(); got != 1 {
		t.Fatalf("registry holds %d entries after one registration", got)
	}

	assertPanics(t, "duplicate ID", func() {
		reg.Register(engine.Registration{
			ID: "L1.SOME_ASSERTION", Severity: engine.Error, Remediation: "Fix it.",
			Description: "second", Pure: true,
		})
	})
	assertPanics(t, "malformed ID", func() {
		reg.Register(engine.Registration{
			ID: "NOT_A_RULE_ID", Severity: engine.Error, Remediation: "Fix it.",
		})
	})
	assertPanics(t, "error rule with no remediation", func() {
		reg.Register(engine.Registration{ID: "L1.NO_REMEDIATION_HERE", Severity: engine.Error})
	})
}

func TestRegistryStageOfUnknownRuleIsShadow(t *testing.T) {
	t.Parallel()
	reg := engine.NewRegistry()
	if got := reg.StageOf("L1.NEVER_REGISTERED_ID"); got != engine.Shadow {
		t.Fatalf("unknown rule resolved to stage %v; an unregistered rule must not be able to reject", got)
	}
}

func TestRuleSetRuleLookupAndIDs(t *testing.T) {
	t.Parallel()
	set := engine.RuleSet[subject]{
		Name: "test.lookup", Mode: engine.CollectAll,
		Rules: []engine.Rule[subject]{passing("L1.FIRST_RULE_HERE"), passing("L1.SECOND_RULE_ONE")},
	}
	if _, ok := set.Rule("L1.SECOND_RULE_ONE"); !ok {
		t.Fatal("Rule lookup missed a rule in the set")
	}
	if _, ok := set.Rule("L1.NOT_IN_THE_SET"); ok {
		t.Fatal("Rule lookup found a rule that is not in the set")
	}
	if ids := set.IDs(); len(ids) != 2 || ids[0] != "L1.FIRST_RULE_HERE" {
		t.Fatalf("IDs() = %v, want evaluation order", ids)
	}
}

func TestReportMergeCombinesPhases(t *testing.T) {
	t.Parallel()
	phaseA := engine.RuleSet[subject]{
		Name: "L1.phaseA", Mode: engine.ShortCircuit,
		Rules: []engine.Rule[subject]{passing("L1.AUTH_IS_FINE_OK")}, Stages: enforceAll(),
	}.Evaluate(context.Background(), subject{})
	phaseB := engine.RuleSet[subject]{
		Name: "L1.phaseB", Mode: engine.CollectAll,
		Rules:  []engine.Rule[subject]{failing("L1.SCHEMA_IS_WRONG", "VALIDATION_FAILED")},
		Stages: enforceAll(),
	}.Evaluate(context.Background(), subject{})

	merged := phaseA.Merge(phaseB)

	if len(merged.Outcomes) != 2 {
		t.Fatalf("merged report holds %d outcomes, want 2", len(merged.Outcomes))
	}
	if merged.OK() {
		t.Fatal("merged report is OK despite a phase B failure")
	}
}

func TestOutcomeDetailNeverDropsRemediation(t *testing.T) {
	t.Parallel()
	out := engine.Fail("L5.AMOUNT_WITHIN_MERCHANT_LIMIT", "AMOUNT_EXCEEDS_LIMIT", "/amount",
		"amount 200 exceeds the limit of 100", "Contact your platform administrator to raise it.")
	d := out.Detail()
	if d.RuleID != "L5.AMOUNT_WITHIN_MERCHANT_LIMIT" {
		t.Fatalf("detail rule ID is %q", d.RuleID)
	}
	if d.Field != "/amount" || d.Code != "AMOUNT_EXCEEDS_LIMIT" {
		t.Fatalf("detail lost field or code: %+v", d)
	}
	if !contains(d.Message, "raise it") {
		t.Fatalf("detail message dropped the remediation: %q", d.Message)
	}
}

func TestEvaluateStampsElapsedWhenMeasuring(t *testing.T) {
	t.Parallel()
	tick := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	calls := 0
	set := engine.RuleSet[subject]{
		Name: "test.elapsed", Mode: engine.CollectAll,
		Rules:  []engine.Rule[subject]{passing("L1.SOMETHING_HAPPENS")},
		Stages: enforceAll(),
		Elapsed: func() time.Time {
			calls++
			return tick.Add(time.Duration(calls) * time.Millisecond)
		},
	}
	if got := set.Evaluate(context.Background(), subject{}).Elapsed; got != time.Millisecond {
		t.Fatalf("Elapsed = %v, want 1ms from the injected clock", got)
	}
}

func enforceAll() engine.StageLookup {
	return engine.StageFunc(func(engine.RuleID) engine.Stage { return engine.Enforce })
}

func assertPanics(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s did not panic", what)
		}
	}()
	fn()
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
