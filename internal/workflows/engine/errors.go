package engine

import (
	"context"
	"errors"
	"strings"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Sentinel conditions the engine itself raises. They are sentinels rather than codes because a
// caller branches on identity (`errors.Is`) and the transport never sees them: every one of
// these is an internal control-flow signal, and the *apierror.Error wrapper carries the
// client-facing code when one is needed.
var (
	// ErrLeaseLost means a fenced write matched zero rows: this worker's lease_epoch is stale
	// because another worker has taken the instance over. It is the single most important error
	// in the engine. A worker that sees it must abandon the instance immediately and must not
	// record any result — that abandonment is what converts a 90-second GC pause from a
	// duplicate side effect into a wasted attempt.
	//
	// Repository implementations MUST return an error wrapping this value when an UPDATE
	// carrying `WHERE lease_epoch = $n` affects zero rows. Returning nil there would silently
	// discard a write and leave two workers believing they own one instance.
	ErrLeaseLost = errors.New("workflow: lease lost — the instance is owned by another worker")

	// ErrInstanceNotFound is returned by Get, Signal, Cancel and Resume for an unknown ID.
	ErrInstanceNotFound = errors.New("workflow: instance not found")

	// ErrNotRunnable means the instance is in a state from which the requested operation is
	// impossible — signalling a COMPLETED instance, resuming a POISONED one.
	ErrNotRunnable = errors.New("workflow: instance is not in a resumable state")

	// ErrPivotPassed means an abort was requested for an instance that has already committed an
	// irreversible step. The engine refuses: rolling back past the money pivot would try to
	// de-provision a merchant that may already have live payments. The instance is parked for
	// manual intervention instead.
	ErrPivotPassed = errors.New("workflow: cannot roll back past an irreversible pivot")

	// ErrUnknownActivity means the definition names an activity the registry does not hold.
	// This fails at Register time, at process start, never at runtime on a live instance.
	ErrUnknownActivity = errors.New("workflow: activity is not registered")

	// ErrDefinitionInvalid is the umbrella for Validate failures; the specific unsoundness is
	// carried in the wrapped detail.
	ErrDefinitionInvalid = errors.New("workflow: definition is not sound")
)

// FailureClass is the verdict a failure receives, and it — not the error type — decides what
// the engine does next.
//
// Classification is a first-class part of a step's definition rather than a property of the
// error, because the same underlying error means different things in different steps. A
// timeout calling a pure validation rule is transient; the identical timeout calling a KYC
// vendor is *ambiguous*, because we do not know whether the vendor acted.
type FailureClass string

const (
	// ClassTransient retries per the step's policy.
	ClassTransient FailureClass = "TRANSIENT"
	// ClassTerminalBusiness is a business "no": no retry, compensate. A KYC rejection is not an
	// engineering failure and must not be retried into the ground; it is an outcome to record
	// and surface.
	ClassTerminalBusiness FailureClass = "TERMINAL_BUSINESS"
	// ClassTerminalTechnical is a bug or a broken contract — a 401 to a vendor, a malformed
	// request, a panic. No retry; DLQ. Retrying it just re-fills the DLQ on the same binary.
	ClassTerminalTechnical FailureClass = "TERMINAL_TECHNICAL"
	// ClassAmbiguous means the outcome is unknown. No blind retry: the next attempt begins with
	// lookup-before-act, and if the lookup is inconclusive the step is parked as ClassManual.
	ClassAmbiguous FailureClass = "AMBIGUOUS"
	// ClassManual needs a human. The instance is parked, not failed.
	ClassManual FailureClass = "MANUAL"
)

// String satisfies fmt.Stringer.
func (c FailureClass) String() string { return string(c) }

// IsTerminal reports whether the class forbids another attempt of this step.
func (c FailureClass) IsTerminal() bool {
	return c == ClassTerminalBusiness || c == ClassTerminalTechnical
}

// Aborts reports whether the class aborts the saga and runs compensations. Only a business "no"
// does: a technical failure goes to the DLQ so that an operator can fix forward, because
// compensating away the merchant's provisioned state on account of *our* misconfiguration
// destroys work that a redeploy would have made succeed.
func (c FailureClass) Aborts() bool { return c == ClassTerminalBusiness }

// Classify maps an error to a FailureClass using the platform's error model.
//
// The default is driven by apierror.IsRetryable, which is the same bit the gateway dispatcher,
// the outbox relay and the event consumers branch on — so a code classified retryable in
// pkg/apierror is retryable everywhere, and there is exactly one place to change it.
//
// Two deliberate departures from "retryable ⇒ transient":
//
//   - A context deadline on a step declared side-effecting is ClassAmbiguous, never
//     ClassTransient. We do not know whether the vendor acted, and the correct response to an
//     unknown is a lookup, not another call.
//   - An *unclassified* error (not an *apierror.Error) is ClassTerminalTechnical. An error
//     nobody has reasoned about is not evidence that a retry is safe; treating it as transient
//     is how an unhandled panic becomes a duplicate provisioning call.
func Classify(err error, sideEffecting bool) FailureClass {
	if err == nil {
		return ClassTransient
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrStepTimeout) {
		if sideEffecting {
			return ClassAmbiguous
		}
		return ClassTransient
	}
	var ae *apierror.Error
	if !errors.As(err, &ae) {
		return ClassTerminalTechnical
	}
	switch ae.Code {
	case apierror.CodeGatewayTimeout:
		// The catalogue marks this non-retryable precisely because the outcome is unknown.
		// Unknown is not failure; it is a lookup obligation.
		if sideEffecting {
			return ClassAmbiguous
		}
		return ClassTransient
	case apierror.CodeGatewayAuthenticationFailed, apierror.CodeForbidden, apierror.CodeUnauthenticated,
		apierror.CodeInsufficientScope, apierror.CodeInternalError, apierror.CodeMalformedRequest:
		return ClassTerminalTechnical
	default:
		// Every other code is classified by its retryability and category below rather than by
		// identity. Only the codes above are exceptions to what the catalogue already says
	}
	if ae.Retryable {
		return ClassTransient
	}
	switch ae.Category {
	case apierror.CategoryValidation, apierror.CategoryBusinessRule, apierror.CategoryConflict,
		apierror.CategoryNotFound:
		return ClassTerminalBusiness
	case apierror.CategoryInternal:
		return ClassTerminalTechnical
	default:
		return ClassTerminalTechnical
	}
}

// ErrStepTimeout is returned when a step exceeds its per-attempt deadline.
//
// It is distinct from context.DeadlineExceeded so that a step which *observed* its deadline and
// returned promptly is distinguishable from one the engine had to abandon. The distinction
// matters for the ambiguity rule: an activity that returned on its own deadline may still have
// left a request in flight at the vendor.
var ErrStepTimeout = errors.New("workflow: step exceeded its per-attempt timeout")

// IsLeaseLost reports whether err is, or wraps, ErrLeaseLost.
func IsLeaseLost(err error) bool { return errors.Is(err, ErrLeaseLost) }

// ChainEntry is one link of a failure's causal chain, flattened for storage.
type ChainEntry struct {
	// Code is the apierror code at this link, or "" for a plain error.
	Code string `json:"code,omitempty"`
	// Category classifies the link for alert routing.
	Category string `json:"category,omitempty"`
	// Message is the link's own text.
	Message string `json:"message"`
	// Retryable records the bit as it was at this link. A chain in which an inner retryable
	// error is wrapped by an outer non-retryable one is the classic "why did this give up"
	// mystery, and it is only diagnosable if both bits are preserved.
	Retryable bool `json:"retryable"`
}

// Chain flattens an error's cause chain outermost-first.
//
// The DLQ stores this rather than a single message string, because triage's first question is
// always "what actually broke", and `Error()` on a five-deep wrap answers it with a sentence
// that has to be read backwards. An ordered array is greppable, diffable across instances, and
// groupable — which is what turns "seventeen DLQ entries" into "one vendor outage".
func Chain(err error) []ChainEntry {
	var out []ChainEntry
	for e := err; e != nil; e = errors.Unwrap(e) {
		entry := ChainEntry{Message: e.Error()}
		// A direct type assertion, not errors.As: this link's own code is wanted, not the code
		// of something it wraps — and errors.As walks the chain, which on a cyclic Unwrap (a
		// programming error, but one that must not hang a worker holding a lease) never returns.
		//nolint:errorlint // deliberate: this link's OWN code is wanted, not the code of anything
		// it wraps. errors.As walks the chain, which both reports the wrong link here and, on a
		// cyclic Unwrap, never returns — in a worker that is holding a lease.
		if ae, ok := e.(*apierror.Error); ok {
			entry.Code = string(ae.Code)
			entry.Category = string(ae.Category)
			entry.Retryable = ae.Retryable
		}
		out = append(out, entry)
		if len(out) > 32 {
			break
		}
	}
	return out
}

// Summarize renders a chain as a single greppable line for a log field.
func Summarize(chain []ChainEntry) string {
	parts := make([]string, 0, len(chain))
	for _, c := range chain {
		if c.Code != "" {
			parts = append(parts, c.Code)
			continue
		}
		parts = append(parts, strings.SplitN(c.Message, ":", 2)[0])
	}
	return strings.Join(parts, " <- ")
}

// FailureRecord is the durable description of why a step or an instance failed. It is what the
// DLQ stores and what the operator surface renders.
type FailureRecord struct {
	Step       string       `json:"step"`
	Attempt    int          `json:"attempt"`
	Class      FailureClass `json:"class"`
	Code       string       `json:"code,omitempty"`
	Message    string       `json:"message"`
	Chain      []ChainEntry `json:"chain"`
	OccurredAt string       `json:"occurredAt,omitempty"`
}

// NewFailureRecord builds a FailureRecord from an error.
func NewFailureRecord(step string, attempt int, class FailureClass, err error) FailureRecord {
	return FailureRecord{
		Step:    step,
		Attempt: attempt,
		Class:   class,
		Code:    string(apierror.CodeOf(err)),
		Message: err.Error(),
		Chain:   Chain(err),
	}
}

// stepFailure wraps an activity error with the engine's classification so that the class
// survives being handed to the retry machinery and back.
type stepFailure struct {
	class FailureClass
	step  string
	cause error
}

func (f *stepFailure) Error() string {
	return "step " + f.step + " failed (" + string(f.class) + "): " + f.cause.Error()
}

func (f *stepFailure) Unwrap() error { return f.cause }

// WithClass tags err with an explicit FailureClass, overriding what Classify would infer.
//
// Activities use it for the cases only they can know: a KYC vendor's 4xx that means "these
// documents are unreadable" is ClassTerminalBusiness, while the byte-identical 4xx shape from a
// malformed request of ours is ClassTerminalTechnical, and no generic classifier can tell them
// apart from the status code.
func WithClass(err error, class FailureClass) error {
	if err == nil {
		return nil
	}
	return &stepFailure{class: class, cause: err}
}

// ClassOf returns the explicit class attached by WithClass, if any.
func ClassOf(err error) (FailureClass, bool) {
	var f *stepFailure
	if errors.As(err, &f) {
		return f.class, true
	}
	return "", false
}

// ClassifyStep is Classify with the explicit override applied first. This is the function the
// engine actually calls.
func ClassifyStep(err error, sideEffecting bool) FailureClass {
	if c, ok := ClassOf(err); ok {
		return c
	}
	return Classify(err, sideEffecting)
}

// NotFound builds the client-facing error for an unknown instance, preserving the sentinel for
// internal branching.
//
// It is exported because the callers that produce this error are the repository and engine
// *implementations* — enginetest, temporal, postgres — which live outside this package. Each one
// previously rebuilt the same three-part expression by hand, and a wrap that drifts (a different
// code, or a message that stops naming the instance) turns `errors.Is(err, ErrInstanceNotFound)`
// at the call site into a silent false. One constructor is what keeps the sentinel and the
// client-visible code attached to each other.
func NotFound(id string) error {
	return apierror.Wrapf(ErrInstanceNotFound, apierror.CodeWorkflowNotFound,
		"workflow instance %s was not found", id)
}
