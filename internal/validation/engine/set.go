package engine

import (
	"context"
	"time"
)

// Mode is a rule set's evaluation semantics.
type Mode uint8

const (
	// CollectAll runs every applicable rule and returns every failure together.
	//
	// Used by: L1 phase B (schema, types, bounds, PAN detector), L2 (merchant onboarding),
	// L4 (configuration publish), L5 (pre-dispatch).
	//
	// Why: the reader of the result is a human or an integrator fixing a form. Returning one
	// failure at a time turns a nine-field fix into nine round trips, and — worse — into nine
	// support tickets, because after the third round trip the integrator concludes the API is
	// broken. The cost is bounded and known: every rule in these sets is pure and
	// sub-microsecond, so running all of them costs less than the round trip saved.
	CollectAll Mode = iota

	// ShortCircuit stops at the first blocking Error.
	//
	// Used by: L1 phase A (transport, auth, tenancy, authorization, rate limit), L3 (gateway
	// probing), L6 (gateway response), L7 (domain transitions).
	//
	// Why, in three separate flavours:
	//
	//   - Security. After L1.JWT_SIGNATURE_VERIFIES fails there is no authenticated subject,
	//     so every later rule would be evaluating attacker-controlled input and answering
	//     questions through its error messages. This is what stops an unauthenticated request
	//     being used to probe which fields the schema accepts.
	//   - Cost. L3 rules are network calls. Probing a gateway twelve more times with
	//     credentials that just failed to authenticate buys nothing and costs twelve seconds.
	//   - Correctness. L6 and L7 rules are sequentially dependent:
	//     L6.STATUS_IS_MAPPABLE must pass before L6.STATE_IS_REACHABLE_FROM_CURRENT has a
	//     status to reason about, and evaluating the second on an unmapped status produces a
	//     confident wrong answer.
	ShortCircuit
)

// String satisfies fmt.Stringer.
func (m Mode) String() string {
	if m == ShortCircuit {
		return "ShortCircuit"
	}
	return "CollectAll"
}

// RuleSet is an ordered collection of rules over one subject type, evaluated under one mode.
//
// The set is a value, not a service: it holds no state that changes between evaluations, so it
// is built once at wiring time and shared across goroutines. Deps are baked into the rules
// when the set is constructed, which is why a level's constructor takes configuration and
// counters rather than a repository — a set that could reach a database could not be evaluated
// on the hot path within a stated budget.
type RuleSet[T any] struct {
	// Name identifies the set in reports, metrics and logs, e.g. "L5.payment".
	Name string

	// Mode selects ShortCircuit or CollectAll semantics.
	Mode Mode

	// Rules run in slice order. Order is meaningful under ShortCircuit and is still worth
	// keeping stable under CollectAll, because it is the order failures appear in `details[]`
	// and the order a human reads them.
	Rules []Rule[T]

	// Stages resolves each rule's promotion stage. Nil means "use the registered stage".
	Stages StageLookup

	// Metrics receives every evaluated outcome, shadow outcomes included. Nil disables
	// recording, which is correct for tests and never correct in production.
	Metrics MetricHook

	// Elapsed measures evaluation wall time. It is an injected function rather than a direct
	// time.Now call so the set stays testable with a deterministic duration; nil means "do not
	// measure", which is what a rule-level unit test wants.
	Elapsed func() time.Time
}

// Evaluate runs the set against subject and returns a Report.
//
// The loop is the whole engine, and its four decisions are worth stating explicitly:
//
//  1. A rule whose precondition is false produces no outcome and is recorded in Skipped. It
//     did not pass; it did not run.
//  2. The rule's stage is resolved once, before evaluation, and stamped onto the outcome.
//  3. A Shadow outcome is recorded to metrics and diverted into Shadowed, where nothing that
//     builds a response can see it. A Warn outcome is demoted to Warning severity.
//  4. Under ShortCircuit, evaluation stops after the first blocking outcome — which by
//     construction a shadow outcome can never be.
func (rs RuleSet[T]) Evaluate(ctx context.Context, subject T) Report {
	var start time.Time
	if rs.Elapsed != nil {
		start = rs.Elapsed()
	}

	stages := rs.Stages
	if stages == nil {
		stages = RegistryStages
	}

	rep := Report{Set: rs.Name, Mode: rs.Mode}

	for _, r := range rs.Rules {
		if r == nil {
			continue
		}
		if !Applies(r, subject) {
			rep.Skipped = append(rep.Skipped, r.ID())
			continue
		}

		stage := stages.StageFor(r.ID())
		out := r.Evaluate(ctx, subject)
		out.Rule = r.ID()
		out.Stage = stage
		if out.Passed {
			out.Severity = r.Severity()
		} else if stage == Warn && out.Severity == Error {
			out.Severity = Warning
		}

		if rs.Metrics != nil {
			rs.Metrics.RecordOutcome(ctx, rs.Name, out)
		}

		if stage == Shadow {
			rep.Shadowed = append(rep.Shadowed, out)
			continue
		}

		rep.Outcomes = append(rep.Outcomes, out)
		if rs.Mode == ShortCircuit && out.IsBlocking() {
			break
		}
	}

	if rs.Elapsed != nil {
		rep.Elapsed = rs.Elapsed().Sub(start)
	}
	return rep
}

// Rule returns the rule with the given ID. Tests use it to evaluate one rule in isolation,
// which is the only way to get a genuine per-rule test out of a ShortCircuit set.
func (rs RuleSet[T]) Rule(id RuleID) (Rule[T], bool) {
	for _, r := range rs.Rules {
		if r != nil && r.ID() == id {
			return r, true
		}
	}
	return nil, false
}

// IDs returns the set's rule IDs in evaluation order.
func (rs RuleSet[T]) IDs() []RuleID {
	out := make([]RuleID, 0, len(rs.Rules))
	for _, r := range rs.Rules {
		if r != nil {
			out = append(out, r.ID())
		}
	}
	return out
}

// With returns a copy of the set carrying a stage lookup and a metric hook. It exists so that
// wiring code can build the pure set once at startup and bind per-tenant stages per request
// without mutating shared state.
func (rs RuleSet[T]) With(stages StageLookup, metrics MetricHook) RuleSet[T] {
	c := rs
	c.Stages = stages
	c.Metrics = metrics
	return c
}
