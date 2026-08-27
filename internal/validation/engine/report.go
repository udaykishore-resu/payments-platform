package engine

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Report is everything one evaluation of a rule set produced.
//
// It is designed to be persisted next to the thing it judged — the payment, the onboarding
// case, the configuration version — because the question compliance eventually asks is "why
// did you decline this on 2026-03-14", and the answer must be a list of rule IDs rather than
// an archaeology project across 30-day log retention.
type Report struct {
	// Set is the rule set's name.
	Set string

	// Mode is the mode the set ran under. Recorded because a report with one failure means
	// something different under ShortCircuit (there may be more) than under CollectAll (there
	// are not).
	Mode Mode

	// Outcomes holds every rule that ran and was allowed to be seen, in evaluation order.
	// Shadow outcomes are deliberately absent.
	Outcomes []Outcome

	// Skipped lists rules whose precondition was false. They did not pass.
	Skipped []RuleID

	// Shadowed holds outcomes from rules in Shadow stage. They are kept for the audit record
	// and the promotion dashboard and are invisible to OK, Errors, Warnings and AsError.
	Shadowed []Outcome

	// Elapsed is evaluation wall time, when the set was configured to measure it.
	Elapsed time.Duration
}

// OK reports whether the operation may proceed: no blocking failure was recorded.
func (r Report) OK() bool {
	for _, o := range r.Outcomes {
		if o.IsBlocking() {
			return false
		}
	}
	return true
}

// Errors returns the failing outcomes at Error severity, in evaluation order. These are the
// reasons the operation was rejected.
func (r Report) Errors() []Outcome {
	return r.filter(func(o Outcome) bool { return !o.Passed && o.Severity == Error })
}

// Warnings returns the failing outcomes at Warning severity: surfaced to the caller, not
// blocking.
func (r Report) Warnings() []Outcome {
	return r.filter(func(o Outcome) bool { return !o.Passed && o.Severity == Warning })
}

// Failures returns every non-passing outcome, errors and warnings together, in evaluation
// order. This is what goes into `details[]`: a caller fixing an integration wants to see the
// warnings alongside the errors, in one pass.
func (r Report) Failures() []Outcome {
	return r.filter(func(o Outcome) bool { return !o.Passed })
}

// Passed returns the outcomes that passed. Used by the audit record, which stores what was
// checked and not only what failed — "we checked sanctions and it passed" is the answer to a
// different regulatory question than "we did not reject it".
func (r Report) Passed() []Outcome {
	return r.filter(func(o Outcome) bool { return o.Passed })
}

func (r Report) filter(keep func(Outcome) bool) []Outcome {
	var out []Outcome
	for _, o := range r.Outcomes {
		if keep(o) {
			out = append(out, o)
		}
	}
	return out
}

// For returns the outcome for id, if the rule ran and was not shadowed.
func (r Report) For(id RuleID) (Outcome, bool) {
	for _, o := range r.Outcomes {
		if o.Rule == id {
			return o, true
		}
	}
	return Outcome{}, false
}

// ShadowFor returns the shadow outcome for id, if the rule ran in Shadow stage. The promotion
// dashboard reads through here; nothing on a request path may.
func (r Report) ShadowFor(id RuleID) (Outcome, bool) {
	for _, o := range r.Shadowed {
		if o.Rule == id {
			return o, true
		}
	}
	return Outcome{}, false
}

// WasSkipped reports whether id's precondition was false.
func (r Report) WasSkipped(id RuleID) bool {
	for _, s := range r.Skipped {
		if s == id {
			return true
		}
	}
	return false
}

// FailedRuleIDs returns the IDs of every non-passing outcome, which is the compact form stored
// in the audit trail and quoted in a support conversation.
func (r Report) FailedRuleIDs() []RuleID {
	var out []RuleID
	for _, o := range r.Outcomes {
		if !o.Passed {
			out = append(out, o.Rule)
		}
	}
	return out
}

// Merge appends other's outcomes to r's, producing the report for a two-phase set such as L1
// (ShortCircuit auth phase followed by CollectAll body phase).
//
// It is a function on the report rather than a feature of RuleSet because the two phases have
// different subjects' worth of trust: phase B must not run at all if phase A failed, and that
// decision belongs to the caller who knows what it means, not to a generic combinator.
func (r Report) Merge(other Report) Report {
	out := r
	out.Outcomes = append(append([]Outcome(nil), r.Outcomes...), other.Outcomes...)
	out.Skipped = append(append([]RuleID(nil), r.Skipped...), other.Skipped...)
	out.Shadowed = append(append([]Outcome(nil), r.Shadowed...), other.Shadowed...)
	out.Elapsed = r.Elapsed + other.Elapsed
	return out
}

// AsError converts the report into the single platform error, or nil if the report is OK.
//
// This is the only conversion point from validation to transport, and centralising it is the
// reason every rejection in the platform has the same shape. Three decisions are made here:
//
//   - The top-level Code comes from the first Error-severity failure. Something has to be the
//     headline for a client that branches on one code, and "the first rule that objected, in
//     the order the rules were written" is the only choice that is both deterministic and
//     explainable. Every other failure survives in Details.
//   - Every failure, warnings included, becomes a Detail carrying its RuleID. A caller fixing
//     an integration gets the whole list in one round trip; that is the entire argument for
//     CollectAll, and it would be wasted if the conversion dropped all but the first.
//   - Shadow outcomes are already gone. They never reach this function, so there is no way for
//     a rule under evaluation to leak into a response by accident.
func (r Report) AsError() *apierror.Error {
	errs := r.Errors()
	if len(errs) == 0 {
		return nil
	}

	head := errs[0]
	code := apierror.Code(head.Code)
	if code == "" {
		code = apierror.CodeValidationFailed
	}

	message := head.Message
	if len(errs) > 1 {
		message = pluralMessage(len(errs))
	}

	err := apierror.New(code, message)

	failures := r.Failures()
	details := make([]apierror.Detail, 0, len(failures))
	for _, o := range failures {
		details = append(details, o.Detail())
	}
	return err.WithDetails(details...)
}

func pluralMessage(n int) string {
	return itoa(n) + " validation rules were not satisfied; see details for the rule that " +
		"produced each failure and how to fix it"
}

// itoa avoids importing strconv for one call site in a package that is otherwise free of
// formatting dependencies on the hot path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
