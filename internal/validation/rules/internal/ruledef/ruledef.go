// Package ruledef is the shared table format the seven rule levels declare their catalogs in.
//
// Why a table rather than a type per rule. There are 243 rules. Written as structs with an
// ID method, a Severity method and an Evaluate method, that is roughly two thousand lines of
// boilerplate whose only job is to restate the catalog row the rule implements — and, worse,
// the metadata that documentation and the registry need would live in a second place, free to
// disagree with the first. One table per level, read straight down against the catalog in
// docs/validation-plane.md, is reviewable; a rule whose remediation text drifts from its
// registry entry is not expressible, because there is only one of them.
//
// The table is a function of the level's Deps, so the same declaration both registers the
// rule's static metadata at init (with zero Deps, which the metadata does not depend on) and
// builds the evaluating rule at wiring time.
package ruledef

import (
	"context"

	"github.com/udaykishore-resu/payments-platform/internal/validation/engine"
)

// Def is one catalog row: the metadata the registry needs and the predicate the engine runs.
type Def[T any] struct {
	// ID is the catalog rule ID. It must match docs/validation-plane.md exactly.
	ID engine.RuleID

	// Severity is ERROR (rejects) or WARNING (surfaced, does not reject).
	Severity engine.Severity

	// Code is the apierror code a failure maps to. Empty is legal only for warnings, which the
	// catalog documents with an em dash.
	Code string

	// Field is the default JSON pointer or dotted path reported with a failure. A rule that
	// locates the problem more precisely per subject overrides it by returning a path-prefixed
	// message; most rules have exactly one field and this is it.
	Field string

	// Desc is the one-line statement of what the rule asserts, for generated documentation.
	Desc string

	// Remediation is the caller-facing fix. Mandatory for ERROR rules.
	Remediation string

	// Pure declares the rule total, deterministic and free of network, clock and shared-counter
	// reads. An impure rule takes its vendor or counter result as a field of the subject, which
	// is what keeps it testable without a network and reproducible from a stored subject.
	Pure bool

	// Applies is the precondition. Nil means the rule runs on every evaluation of its set. A
	// rule whose precondition is false produces no outcome at all — not a pass.
	Applies func(subject T) bool

	// Check returns "" when the subject satisfies the rule, or the caller-facing message
	// stating what is wrong. Returning a message rather than a bool is what keeps the failure
	// specific ("EUR is not enabled for this merchant") while the remediation stays generic.
	//
	// Check must be total: no panic on a zero-value subject, no nil-map write, no index out of
	// range. The level's test suite runs every rule against the zero subject for exactly this.
	Check func(subject T) string
}

// Rule turns the definition into an engine rule, applying the impurity marker when the
// definition declares itself impure.
func (d Def[T]) Rule() engine.Rule[T] {
	r := engine.RuleFunc[T]{
		RuleID:  d.ID,
		Sev:     d.Severity,
		Why:     d.Desc,
		Applies: d.Applies,
		Eval: func(_ context.Context, subject T) engine.Outcome {
			msg := ""
			if d.Check != nil {
				msg = d.Check(subject)
			}
			if msg == "" {
				return engine.Pass(d.ID)
			}
			if d.Severity == engine.Error {
				return engine.Fail(d.ID, d.Code, d.Field, msg, d.Remediation)
			}
			return engine.FailWarning(d.ID, d.Code, d.Field, msg, d.Remediation)
		},
	}
	if !d.Pure {
		return engine.MarkImpure[T](r)
	}
	return r
}

// Build converts a table into the rule slice a RuleSet carries, preserving order.
func Build[T any](defs []Def[T]) []engine.Rule[T] {
	out := make([]engine.Rule[T], 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Rule())
	}
	return out
}

// Register files every definition's metadata in the process-wide registry.
//
// Called from a level's init with the zero Deps: the metadata a registration carries is static
// per rule, so building the table with empty dependencies is enough to read it, and doing it
// this way means the table cannot be registered as one thing and evaluated as another.
//
// Owner and since are the accountability fields; the CI check that every rule has an owner is
// what stops a rule outliving the team that understood it.
func Register[T any](defs []Def[T], owner, since string, stage engine.Stage) {
	for _, d := range defs {
		engine.Register(engine.Registration{
			ID:          d.ID,
			Severity:    d.Severity,
			Code:        d.Code,
			Description: d.Desc,
			Remediation: d.Remediation,
			Pure:        d.Pure,
			Stage:       stage,
			Owner:       owner,
			Since:       since,
			Status:      engine.StatusActive,
		})
	}
}

// IDs returns the table's rule IDs in declaration order, for tests that enumerate a level.
func IDs[T any](defs []Def[T]) []engine.RuleID {
	out := make([]engine.RuleID, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.ID)
	}
	return out
}
