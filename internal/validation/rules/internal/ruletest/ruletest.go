// Package ruletest is the harness every level's rule tests run through.
//
// It exists so that the properties that make the validation plane trustworthy cannot be
// forgotten by whoever writes the next rule's test. A per-rule test written by hand asserts
// what its author remembered to assert; a harness asserts the same six things for all 243
// rules, every time:
//
//   - Coverage. Every rule in the set has a case, and every case names a rule in the set. This
//     is what makes "one test per rule" a fact rather than an intention.
//   - A passing case and a failing case for each rule. A rule with only a failing case may be
//     rejecting everything; a rule with only a passing case may be rejecting nothing.
//   - Precondition sanity. Both cases must make the rule actually apply, so a "failing" case
//     that silently skips the rule cannot be mistaken for coverage.
//   - Determinism. Each case is evaluated twice and the outcomes must agree, which catches a
//     rule that reads a map in iteration order or a clock.
//   - Totality. Every rule is evaluated against the zero subject; a panic fails the test.
//   - Message hygiene. A failing outcome must carry a message and, at ERROR severity, the
//     remediation its registry entry promises.
package ruletest

import (
	"context"
	"sort"
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/validation/engine"
)

// Case is one rule's test: a mutation producing a subject the rule accepts, and a mutation
// producing one it rejects.
type Case[T any] struct {
	// ID is the rule under test.
	ID engine.RuleID
	// Pass mutates the base subject into one the rule applies to and accepts.
	Pass func(*T)
	// Fail mutates the base subject into one the rule applies to and rejects.
	Fail func(*T)
}

// Run evaluates every case against the set and asserts the harness properties.
//
// base returns a fresh subject for each case, so a mutation in one case cannot leak into
// another through a shared map or slice — the single most common way a table-driven test
// starts passing for the wrong reason.
func Run[T any](t *testing.T, set engine.RuleSet[T], base func() T, cases []Case[T]) {
	t.Helper()

	assertCoverage(t, set, cases)

	ctx := context.Background()
	for _, tc := range cases {

		t.Run(string(tc.ID), func(t *testing.T) {
			t.Parallel()

			rule, ok := set.Rule(tc.ID)
			if !ok {
				t.Fatalf("%s is not in rule set %q", tc.ID, set.Name)
			}
			reg, registered := engine.Lookup(tc.ID)
			if !registered {
				t.Fatalf("%s is not in the rule registry", tc.ID)
			}

			// Passing case.
			passSubject := base()
			if tc.Pass != nil {
				tc.Pass(&passSubject)
			}
			if !engine.Applies(rule, passSubject) {
				t.Fatalf("%s: the passing case does not satisfy the rule's precondition, so the "+
					"case proves nothing", tc.ID)
			}
			out := rule.Evaluate(ctx, passSubject)
			if !out.Passed {
				t.Fatalf("%s: the passing case failed: %s", tc.ID, out.Message)
			}
			again := rule.Evaluate(ctx, passSubject)
			if again.Passed != out.Passed {
				t.Fatalf("%s: evaluating the same subject twice produced different outcomes", tc.ID)
			}

			// Failing case.
			failSubject := base()
			if tc.Fail != nil {
				tc.Fail(&failSubject)
			}
			if !engine.Applies(rule, failSubject) {
				t.Fatalf("%s: the failing case does not satisfy the rule's precondition, so the "+
					"rule was skipped rather than failed", tc.ID)
			}
			bad := rule.Evaluate(ctx, failSubject)
			if bad.Passed {
				t.Fatalf("%s: the failing case passed", tc.ID)
			}
			if bad.Rule != tc.ID {
				t.Fatalf("%s: outcome reported under %q", tc.ID, bad.Rule)
			}
			if bad.Message == "" {
				t.Fatalf("%s: a failing outcome carries no message", tc.ID)
			}
			if bad.Code != reg.Code {
				t.Fatalf("%s: outcome code %q does not match the registered code %q",
					tc.ID, bad.Code, reg.Code)
			}
			if reg.Severity == engine.Error {
				if bad.Severity != engine.Error {
					t.Fatalf("%s: registered ERROR but produced severity %v", tc.ID, bad.Severity)
				}
				if bad.Remediation == "" {
					t.Fatalf("%s: an ERROR outcome carries no remediation text", tc.ID)
				}
			} else if bad.Severity != engine.Warning {
				t.Fatalf("%s: registered WARNING but produced severity %v", tc.ID, bad.Severity)
			}
			badAgain := rule.Evaluate(ctx, failSubject)
			if badAgain.Passed != bad.Passed || badAgain.Code != bad.Code {
				t.Fatalf("%s: evaluating the same failing subject twice produced different outcomes", tc.ID)
			}
		})
	}

	t.Run("totality/zero subject", func(t *testing.T) {
		t.Parallel()
		// A rule that panics on the zero subject is a rule that panics on a request whose
		// decoder produced less than the rule assumed, and a panic in a request path is never
		// an acceptable way to report a validation failure.
		var zero T
		for _, r := range set.Rules {

			func() {
				defer func() {
					if v := recover(); v != nil {
						t.Errorf("%s panicked on the zero subject: %v", r.ID(), v)
					}
				}()
				if engine.Applies(r, zero) {
					_ = r.Evaluate(ctx, zero)
				}
			}()
		}
	})
}

// assertCoverage fails when a rule has no case or a case names no rule.
func assertCoverage[T any](t *testing.T, set engine.RuleSet[T], cases []Case[T]) {
	t.Helper()

	covered := map[engine.RuleID]int{}
	for _, c := range cases {
		covered[c.ID]++
	}

	var uncovered, duplicated []string
	for _, id := range set.IDs() {
		switch covered[id] {
		case 0:
			uncovered = append(uncovered, string(id))
		case 1:
		default:
			duplicated = append(duplicated, string(id))
		}
		delete(covered, id)
	}
	var unknown []string
	for id := range covered {
		unknown = append(unknown, string(id))
	}

	sort.Strings(uncovered)
	sort.Strings(duplicated)
	sort.Strings(unknown)

	if len(uncovered) > 0 {
		t.Errorf("%d rule(s) in %s have no test case: %v", len(uncovered), set.Name, uncovered)
	}
	if len(duplicated) > 0 {
		t.Errorf("rule(s) in %s have more than one case: %v", set.Name, duplicated)
	}
	if len(unknown) > 0 {
		t.Errorf("case(s) name rules that are not in %s: %v", set.Name, unknown)
	}
}
