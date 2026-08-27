// Package engine is the validation plane's evaluation core: the rule contract, the rule set,
// the report, the registry and the shadow→warn→enforce promotion mechanism.
//
// Why an engine at all, rather than `if` statements in handlers. A payment platform makes
// several hundred assertions about the things it is asked to do, and those assertions are the
// product: they are what a support engineer greps, what compliance answers questions with, and
// what an integrator reads when their request is rejected. Scattered across handlers they are
// unenumerable — nobody can answer "what do we actually check?" without reading every file —
// and they drift, so "the amount is too large" ends up meaning three different things in three
// packages. Making evaluation a plane buys five properties that are otherwise unobtainable: a
// single registry, stable rule IDs, deterministic outcomes, one error shape, and one place to
// audit why something was rejected. See docs/validation-plane.md §1 and baseline §21.
//
// Everything here is stdlib + pkg/apierror. The engine never touches a database, a clock or a
// network, and neither may a rule: a rule reads its subject and nothing else, which is what
// makes a rejection reproducible from a persisted subject snapshot months after the fact.
package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// RuleID is the permanent, greppable identifier of one assertion: "L<level>.<ASSERTION>".
//
// It is the join key between the error a client receives, the audit record, the metric label
// and the catalog entry in docs/validation-plane.md. Rule IDs are never reused for a different
// assertion — a rule may be retired, and stays documented after retirement, because old audit
// records still reference it. Identifying a rejection by prose instead would make it
// uncountable and unalertable the moment someone reworded a message.
type RuleID string

// String satisfies fmt.Stringer.
func (id RuleID) String() string { return string(id) }

// Level returns the validation level the ID declares (1–7), or 0 if the ID is malformed.
//
// The level is carried in the ID rather than alongside it so that a rule ID appearing alone —
// in a log line, a support ticket, a metric label — still says where in the pipeline it fired.
func (id RuleID) Level() int {
	s := string(id)
	if len(s) < 4 || s[0] != 'L' || s[2] != '.' {
		return 0
	}
	n := int(s[1] - '0')
	if n < 1 || n > 7 {
		return 0
	}
	return n
}

// IsWellFormed reports whether the ID matches `^L[1-7]\.[A-Z0-9_]{4,60}$`.
//
// Enforced at registration rather than in review: a typo'd level prefix silently files a rule
// under the wrong level in every downstream report, and the failure is invisible until someone
// asks a question the data can no longer answer.
func (id RuleID) IsWellFormed() bool {
	if id.Level() == 0 {
		return false
	}
	tail := string(id)[3:]
	if len(tail) < 4 || len(tail) > 60 {
		return false
	}
	for i := 0; i < len(tail); i++ {
		c := tail[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// Severity says what a failing outcome does to the operation.
type Severity uint8

const (
	// Warning is recorded and surfaced to the caller but never stops the operation. It is the
	// severity a rule sits at while its false-positive rate is being measured against real
	// traffic, and the severity permanently correct for advice ("your statement descriptor was
	// normalized") that a caller should see but must not be blocked by.
	Warning Severity = iota

	// Error stops the operation according to its rule set's mode.
	Error
)

// String satisfies fmt.Stringer. The rendered form is the one that appears in `details[]` and
// in metric labels, so it is part of the published contract.
func (s Severity) String() string {
	if s == Error {
		return "ERROR"
	}
	return "WARNING"
}

// Param is a bounded, non-PII key/value pair attached to an outcome for message rendering and
// metric exemplars.
//
// Bounded and non-PII are the load-bearing words. Params reach logs, metrics and the audit
// trail; a rule that puts a card number, an email address or an unbounded merchant-supplied
// string in here has moved cardholder data into a 30-day-retention log pipeline, which is the
// exact failure the PAN detector at L1 exists to prevent.
type Param struct {
	Key   string
	Value string
}

// P constructs a Param. It exists so a rule reads `P("limit", "1000")` rather than a struct
// literal at every call site.
func P(key, value string) Param { return Param{Key: key, Value: value} }

// Outcome is the result of evaluating one rule against one subject.
//
// It is deliberately a value and not an error. Wrapping a business rejection in Go's error
// interface tempts callers into `errors.Is` control flow and, worse, loses multiplicity: a
// configuration publish can fail eight rules and the operator must see all eight, not the
// first. Conversion to an error happens once, at the transport boundary, in Report.AsError.
type Outcome struct {
	// Rule is the ID of the rule that produced this outcome.
	Rule RuleID

	// Passed is true when the subject satisfies the assertion.
	Passed bool

	// Severity is the effective severity, which may be lower than the rule's declared severity
	// when the rule is running in Warn stage. The report records the effective value because
	// that is what the caller saw.
	Severity Severity

	// Code is the error-catalog code (apierror.Code) this failure maps to. Empty on a pass.
	Code string

	// Field is a JSON pointer or dotted path into the subject: "/amount", "routing.primary".
	// It is what turns "the request is invalid" into "this field is invalid".
	Field string

	// Message states what is wrong, addressed to the caller.
	Message string

	// Remediation states what to do about it — the sentence a merchant's engineer reads to fix
	// their integration without opening a support ticket. Separated from Message because the
	// two have different lifetimes: the message describes this request, the remediation
	// describes the rule and is stable enough to publish in the catalog.
	Remediation string

	// Params carries bounded, non-PII values for rendering and metrics.
	Params map[string]string

	// Stage records which promotion stage the rule was in when it ran, so a shadow outcome in
	// an audit record is distinguishable from one that actually rejected a request.
	Stage Stage
}

// Pass returns a passing outcome for id.
func Pass(id RuleID) Outcome {
	return Outcome{Rule: id, Passed: true, Stage: Enforce}
}

// Fail returns a failing outcome at Error severity.
//
// Every argument is mandatory by convention, and remediation especially: a failing outcome
// without remediation text is a support ticket the platform chose to receive.
func Fail(id RuleID, code, field, message, remediation string, params ...Param) Outcome {
	return Outcome{
		Rule:        id,
		Passed:      false,
		Severity:    Error,
		Code:        code,
		Field:       field,
		Message:     message,
		Remediation: remediation,
		Params:      paramMap(params),
		Stage:       Enforce,
	}
}

// FailWarning returns a failing outcome at Warning severity: recorded and surfaced to the
// caller, never blocking. Named for the outcome rather than as a `Warn` twin of `Fail` because
// Warn is also a promotion stage, and one identifier meaning two things in one package is how
// a demotion gets mistaken for a severity.
func FailWarning(id RuleID, code, field, message, remediation string, params ...Param) Outcome {
	o := Fail(id, code, field, message, remediation, params...)
	o.Severity = Warning
	return o
}

func paramMap(params []Param) map[string]string {
	if len(params) == 0 {
		return nil
	}
	m := make(map[string]string, len(params))
	for _, p := range params {
		m[p.Key] = p.Value
	}
	return m
}

// IsBlocking reports whether this outcome stops the operation: a failure, at Error severity,
// that is not running in shadow.
func (o Outcome) IsBlocking() bool {
	return !o.Passed && o.Severity == Error && o.Stage != Shadow
}

// Detail renders the outcome as an apierror.Detail, carrying the rule ID so that "why was this
// rejected" has an answer with a documentation anchor.
func (o Outcome) Detail() apierror.Detail {
	msg := o.Message
	if o.Remediation != "" {
		if msg == "" {
			msg = o.Remediation
		} else {
			msg = msg + " " + o.Remediation
		}
	}
	return apierror.Detail{
		Field:   o.Field,
		Code:    o.Code,
		Message: msg,
		RuleID:  string(o.Rule),
	}
}

// String renders the outcome for logs and test failures.
func (o Outcome) String() string {
	if o.Passed {
		return string(o.Rule) + " PASS"
	}
	return fmt.Sprintf("%s %s %s(%s) %s", o.Rule, o.Severity, o.Code, o.Field, o.Message)
}

// Rule is the contract every assertion in the platform implements (baseline §21).
//
// Three methods and no more. A rule that needed a fourth — a name, a category, an owner —
// would be carrying documentation in code that the registry already holds, and the registry is
// where documentation belongs because it can be enumerated at build time.
type Rule[T any] interface {
	// ID returns the stable, documented identifier.
	ID() RuleID

	// Severity returns the declared severity, before any stage demotion.
	Severity() Severity

	// Evaluate is pure and total: same subject → same outcome, no clock read, no network call,
	// no panic. `now` arrives inside the subject precisely so this stays true.
	Evaluate(ctx context.Context, subject T) Outcome
}

// Preconditioned lets a rule declare that it does not apply to a subject at all.
//
// "Does not apply" is not "passes", and the distinction is why this is a separate interface
// rather than a rule returning Pass. A rule that does not apply produces no outcome, so
// "how often does L5.REFUND_WITHIN_WINDOW actually run" stays a different question from "how
// often does it pass" — which is the question you need answered when deciding whether a rule
// is earning its place.
type Preconditioned[T any] interface {
	AppliesTo(subject T) bool
}

// Impure marks a rule that depends on something outside its subject — a vendor decision, a
// shared counter, a certificate revocation list.
//
// The marker exists so that TestHotPathRulesArePure can fail the build when someone adds one
// to L1's body phase, L5, L6 or L7. Impurity is not forbidden; impurity on the payment hot
// path is, because it turns a 5 ms budget into an unbounded one and makes a rejection
// irreproducible.
type Impure interface {
	Impure()
}

// Explains gives a rule a one-line description for generated documentation and CI output.
type Explains interface {
	Explain() string
}

// Applies reports whether r applies to subject. A rule that does not implement Preconditioned
// applies to every subject of its type.
func Applies[T any](r Rule[T], subject T) bool {
	if p, ok := r.(Preconditioned[T]); ok {
		return p.AppliesTo(subject)
	}
	return true
}

// IsImpure reports whether r has declared itself impure.
func IsImpure[T any](r Rule[T]) bool {
	_, ok := r.(Impure)
	return ok
}

// Explanation returns r's one-line description, or "" if it does not provide one.
func Explanation[T any](r Rule[T]) string {
	if e, ok := r.(Explains); ok {
		return e.Explain()
	}
	return ""
}

// RuleFunc adapts a plain function to Rule[T], so that the overwhelming majority of rules —
// which are one predicate over a snapshot — are written as a function literal rather than as a
// type declaration, a receiver and three boilerplate methods.
//
// The struct fields are exported and set by name at construction, which reads at the call site
// like the catalog row the rule implements. Applies and Why are optional.
type RuleFunc[T any] struct {
	// RuleID is the catalog identifier. Required.
	RuleID RuleID

	// Sev is the declared severity. Required (Warning is the zero value and is a real choice,
	// so there is no "unset" to detect; the registry cross-check catches a mismatch with the
	// documented severity).
	Sev Severity

	// Eval is the assertion. Required, pure, total.
	Eval func(ctx context.Context, subject T) Outcome

	// Applies is the precondition. Nil means "always".
	Applies func(subject T) bool

	// Why is the one-line explanation exported to generated documentation.
	Why string
}

// ID satisfies Rule.
func (r RuleFunc[T]) ID() RuleID { return r.RuleID }

// Severity satisfies Rule.
func (r RuleFunc[T]) Severity() Severity { return r.Sev }

// Evaluate satisfies Rule. It defends against a nil Eval by returning a pass rather than
// panicking: a rule set assembled with a missing function is a programming error the registry
// cross-check catches at test time, and a panic in a request path is never the right way to
// report one (conventions rule 9).
func (r RuleFunc[T]) Evaluate(ctx context.Context, subject T) Outcome {
	if r.Eval == nil {
		return Pass(r.RuleID)
	}
	return r.Eval(ctx, subject)
}

// AppliesTo satisfies Preconditioned.
func (r RuleFunc[T]) AppliesTo(subject T) bool {
	if r.Applies == nil {
		return true
	}
	return r.Applies(subject)
}

// Explain satisfies Explains.
func (r RuleFunc[T]) Explain() string { return r.Why }

// impureRule wraps a rule to declare it impure while preserving its precondition.
type impureRule[T any] struct{ inner Rule[T] }

func (i impureRule[T]) ID() RuleID { return i.inner.ID() }
func (i impureRule[T]) Severity() Severity {
	return i.inner.Severity()
}
func (i impureRule[T]) Evaluate(ctx context.Context, s T) Outcome { return i.inner.Evaluate(ctx, s) }
func (i impureRule[T]) AppliesTo(s T) bool                        { return Applies(i.inner, s) }
func (i impureRule[T]) Explain() string                           { return Explanation(i.inner) }
func (i impureRule[T]) Impure()                                   {}

// MarkImpure returns r declared as impure.
//
// Wrapping rather than a field on RuleFunc is deliberate: purity is a property the type system
// can be asked about (`IsImpure`), and a bool field would let a rule claim purity it does not
// have without anything downstream being able to notice.
func MarkImpure[T any](r Rule[T]) Rule[T] { return impureRule[T]{inner: r} }

// SortIDs returns ids sorted lexically. Used by documentation generation and by tests that
// need a stable order without depending on registration order.
func SortIDs(ids []RuleID) []RuleID {
	out := append([]RuleID(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// JoinIDs renders ids for a message or a test failure.
func JoinIDs(ids []RuleID) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = string(id)
	}
	return strings.Join(parts, ", ")
}
