package engine

import "context"

// The combinators below are the entire composition vocabulary of the validation plane. There
// are four of them and there will not be a fifth without an argument.
//
// The alternative that was rejected: a rule DSL, where a configuration document describes
// predicates and the engine interprets them. It always starts reasonably — a `when` clause, a
// couple of matchers — and then someone needs `&&`, then a function call, then a loop, and the
// payment hot path is running an untrusted interpreter with an unbounded latency tail and no
// way to prove termination. Composition in Go, with four total functions and no reflection,
// keeps every rule a compiled predicate whose cost is knowable. Configuration supplies
// parameters to compiled rules; it never supplies logic (docs/validation-plane.md §2.4).

// All conjoins rules under a single reported ID: the composite passes only if every part
// passes, and reports the first failure with the composite's ID.
//
// Use it where several assertions are one idea to the caller — "this IBAN is well formed"
// is length, country prefix and mod-97 — and where reporting them separately would give an
// integrator three failures to fix that are in fact one typo. Where the parts are genuinely
// separate ideas, register them as separate rules instead: an ID is cheap, and a merged ID
// that hides which half failed costs a support conversation.
//
// Severity is taken from the first part, since a composite of mixed severities has no
// defensible single answer and the honest fix is to not compose them.
func All[T any](id RuleID, rules ...Rule[T]) Rule[T] {
	sev := Error
	if len(rules) > 0 {
		sev = rules[0].Severity()
	}
	return RuleFunc[T]{
		RuleID: id,
		Sev:    sev,
		Why:    "conjunction of " + JoinIDs(idsOf(rules)),
		Applies: func(s T) bool {
			// The composite applies if any part applies; a part whose precondition is false
			// simply contributes nothing, exactly as it would on its own.
			for _, r := range rules {
				if Applies(r, s) {
					return true
				}
			}
			return false
		},
		Eval: func(ctx context.Context, s T) Outcome {
			for _, r := range rules {
				if !Applies(r, s) {
					continue
				}
				out := r.Evaluate(ctx, s)
				if !out.Passed {
					out.Rule = id
					return out
				}
			}
			return Pass(id)
		},
	}
}

// When wraps a rule in a precondition: if p is false the rule produces no outcome at all.
//
// The distinction between "skipped" and "passed" is the reason this exists as a combinator
// rather than as an early `return Pass(...)` inside a rule body. A rule that returns Pass when
// it did not apply inflates its own pass rate to 100 % and makes coverage unmeasurable — you
// can no longer tell a rule that is protecting you from a rule that has silently stopped
// matching anything.
func When[T any](p func(T) bool, r Rule[T]) Rule[T] {
	inner := r
	wrapped := RuleFunc[T]{
		RuleID: inner.ID(),
		Sev:    inner.Severity(),
		Why:    Explanation(inner),
		Applies: func(s T) bool {
			return p(s) && Applies(inner, s)
		},
		Eval: inner.Evaluate,
	}
	if IsImpure(inner) {
		return MarkImpure[T](wrapped)
	}
	return wrapped
}

// Lift adapts a Rule[U] to a Rule[T] by projecting the subject with f, reporting under a new
// ID.
//
// This is what lets one assertion be reused at several levels without duplicating it. The
// ISO 4217 currency check is written once over a currency value; L1 lifts it from
// `/amount/currency` on the raw request, L4 lifts it over each element of
// `supportedCurrencies[]`, and L5 lifts it from the payment subject. Three documented rule IDs
// — because the caller-facing remediation genuinely differs at each level — one implementation
// and one test of the underlying predicate, plus three thin projection tests.
//
// The field path on the inner outcome is preserved when the inner rule set one, because the
// projection knows the value and the caller knows the path; overwriting it here would replace
// "/supportedCurrencies[2]" with "" and make the failure unlocatable.
func Lift[T, U any](id RuleID, f func(T) U, r Rule[U]) Rule[T] {
	inner := r
	wrapped := RuleFunc[T]{
		RuleID:  id,
		Sev:     inner.Severity(),
		Why:     Explanation(inner),
		Applies: func(s T) bool { return Applies(inner, f(s)) },
		Eval: func(ctx context.Context, s T) Outcome {
			out := inner.Evaluate(ctx, f(s))
			out.Rule = id
			return out
		},
	}
	if IsImpure(inner) {
		return MarkImpure[T](wrapped)
	}
	return wrapped
}

// Named returns r reported under a different ID, preserving severity, precondition, purity and
// explanation.
//
// It is Lift with the identity projection, and it exists separately because renaming is the
// common case and `Lift(id, func(s T) T { return s }, r)` at every call site is noise that
// obscures which calls are actually projecting.
func Named[T any](id RuleID, r Rule[T]) Rule[T] {
	return Lift[T, T](id, func(s T) T { return s }, r)
}

func idsOf[T any](rules []Rule[T]) []RuleID {
	out := make([]RuleID, 0, len(rules))
	for _, r := range rules {
		if r != nil {
			out = append(out, r.ID())
		}
	}
	return out
}
