package engine

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Output is an activity's checkpointed result, as JSON.
//
// It is bytes rather than `any` because it is written to a jsonb column and read back by a
// process that may be a different binary a week later. A typed value would have to be
// round-tripped through the same marshaller anyway, and hiding that inside the engine would
// make the engine's storage format depend on the activity's Go types — which is exactly the
// coupling that stops a definition from outliving a refactor.
type Output []byte

// Input is everything an activity is given.
//
// It is a struct rather than a bare `[]byte` because an activity needs its identity to compute
// a deterministic idempotency key, and needs the accumulated context to read the outputs of
// earlier steps. Passing those through context values would make them invisible in signatures
// and impossible to type-check.
type Input struct {
	// WorkflowID and TenantID identify the instance.
	WorkflowID shared.WorkflowID
	TenantID   shared.TenantID

	// BusinessKey is the deduplication key — the merchant ID, for onboarding.
	BusinessKey string

	// Step is the step name, and Attempt is the 1-based attempt number.
	Step    string
	Attempt int

	// IdempotencyKey is deterministic in (WorkflowID, Step) and **does not vary with Attempt**.
	//
	// The instinct is to include the attempt so that each try is distinguishable. That instinct
	// produces duplicate side effects: a worker that crashes after calling a vendor but before
	// checkpointing will retry, and with an attempt-varying key the vendor sees a *new* request.
	// With a stable key the vendor dedupes and returns the original result — which is also what
	// makes lookup-before-act possible, since the key is the handle the lookup uses.
	IdempotencyKey string

	// Payload is the step's input document.
	Payload []byte

	// Context is the accumulated JSON object of completed step outputs, keyed by step name. An
	// activity reads its predecessors' results from here rather than being handed a fold of
	// history, which is what makes resume replay-free.
	Context []byte

	// Signal carries the payload of the signal that opened a manual gate, for the gate steps
	// that declare an Activity to apply the human's decision.
	//
	// A gate with no activity simply advances; a gate with one gets to turn "APPROVE" into a
	// domain transition inside the same checkpoint that consumes the signal. Splitting those —
	// gate step, then a separate "apply the decision" step — would put a crash window between
	// the decision being consumed and being acted on, and the decision is consumed at most once.
	Signal []byte

	// SignalPrincipal is who sent that signal. An activity that records a compliance decision
	// needs the reviewer's identity, and it must come from the audited delivery rather than from
	// the payload, which the caller controls.
	SignalPrincipal string

	// LookupFirst is true when the previous attempt ended ambiguously. The activity must then
	// query the external system for its own prior effect by IdempotencyKey *before* acting.
	// An ambiguous outcome is never resolved by guessing.
	LookupFirst bool

	// heartbeat, checkpoint and lookup are supplied by the engine. They are unexported with
	// method accessors so that a zero Input — the one a unit test constructs by hand — is
	// usable without a nil check at every call site.
	heartbeat  func(ctx context.Context, progress []byte) error
	checkpoint func(ctx context.Context, key string, value []byte) error
	lookup     func(ctx context.Context, key string) ([]byte, bool, error)
}

// Heartbeat extends the instance's lease and reports optional progress.
//
// An activity that runs longer than the heartbeat interval MUST call it: the lease arithmetic
// (docs/automation-plane.md §4.1) assumes it. Certification's per-attempt timeout is thirty
// minutes against a sixty-second lease, and it is heartbeating every fifteen seconds that keeps
// the lease alive across it rather than having the instance reclaimed mid-run.
//
// It returns an error wrapping ErrLeaseLost when this worker no longer owns the instance. An
// activity that sees it must abandon its work immediately — another worker is already redoing it.
func (in Input) Heartbeat(ctx context.Context, progress []byte) error {
	if in.heartbeat == nil {
		return nil
	}
	return in.heartbeat(ctx, progress)
}

// Checkpoint persists intra-activity progress so a resumed attempt can skip completed sub-work.
//
// This is what makes a fan-out safe: step 5 provisions four gateways and checkpoints each as it
// completes, so a crash after the second does not re-provision the first two. Without it the
// only granularity available is the whole step, and the whole step includes external creates.
func (in Input) Checkpoint(ctx context.Context, key string, value []byte) error {
	if in.checkpoint == nil {
		return nil
	}
	return in.checkpoint(ctx, key, value)
}

// Lookup reads back a value written by Checkpoint on an earlier attempt.
func (in Input) Lookup(ctx context.Context, key string) ([]byte, bool, error) {
	if in.lookup == nil {
		return nil, false, nil
	}
	return in.lookup(ctx, key)
}

// WithHooks returns a copy of in wired to the engine's heartbeat and checkpoint plumbing. The
// engine calls it; tests call it to supply doubles.
func (in Input) WithHooks(
	heartbeat func(ctx context.Context, progress []byte) error,
	checkpoint func(ctx context.Context, key string, value []byte) error,
	lookup func(ctx context.Context, key string) ([]byte, bool, error),
) Input {
	in.heartbeat, in.checkpoint, in.lookup = heartbeat, checkpoint, lookup
	return in
}

// StepOutput decodes the checkpointed output of an earlier step into v.
//
// Returning "not found" as a bool rather than an error is deliberate: a step reading an
// optional predecessor's output should not have to distinguish "absent" from "corrupt", and
// conflating them is how a missing optional value becomes a retry loop.
func (in Input) StepOutput(step string, v any) (bool, error) {
	if len(in.Context) == 0 {
		return false, nil
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(in.Context, &all); err != nil {
		return false, apierror.Wrap(err, apierror.CodeInternalError,
			"the workflow context is not a JSON object")
	}
	raw, ok := all[step]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return false, nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return false, apierror.Wrapf(err, apierror.CodeInternalError,
			"the checkpointed output of step %q does not match the type this binary expects", step)
	}
	return true, nil
}

// Activity is the unit of side effect.
//
// It receives a checkpointed, immutable input and returns an output the engine checkpoints
// before the next step begins. Everything an activity needs to be safe under retry — the stable
// idempotency key, the ambiguity flag, the heartbeat — arrives in Input, so an activity has no
// excuse to consult wall-clock time, a package-level variable, or the attempt number.
type Activity interface {
	// Name is the registry key. It matches Step.Activity in a definition.
	Name() string

	// Execute performs the step. Returning an error hands classification to the engine; use
	// WithClass to override the inference where only the activity can know the answer.
	Execute(ctx context.Context, in Input) (Output, error)
}

// ActivityFunc adapts a function to the Activity interface, for the small activities where a
// named type would be ceremony.
type ActivityFunc struct {
	ActivityName string
	Fn           func(ctx context.Context, in Input) (Output, error)
}

// Name implements Activity.
func (a ActivityFunc) Name() string { return a.ActivityName }

// Execute implements Activity.
func (a ActivityFunc) Execute(ctx context.Context, in Input) (Output, error) { return a.Fn(ctx, in) }

// TypedActivity wraps a type-safe implementation in the byte-oriented Activity interface.
//
// This is the seam that lets the engine stay definition-agnostic — it moves `[]byte` and knows
// nothing about merchants — while activity code keeps real types and compile-time checking. The
// alternative, letting activities take `any` and type-assert, moves a whole class of error from
// compile time to the middle of a production onboarding.
//
// The JSON round trip is not free, and it is bought deliberately: it is what allows a step
// output checkpointed by last week's binary to be read by this week's, and it is what makes the
// operator surface able to render a step's result without linking the activity's package.
type TypedActivity[I any, O any] struct {
	name string
	fn   func(ctx context.Context, meta Input, in I) (O, error)
}

// NewTypedActivity builds a TypedActivity. The meta parameter carries the identity, the
// idempotency key and the heartbeat hooks; the typed parameter carries the decoded payload.
func NewTypedActivity[I any, O any](name string, fn func(ctx context.Context, meta Input, in I) (O, error)) *TypedActivity[I, O] {
	return &TypedActivity[I, O]{name: name, fn: fn}
}

// Name implements Activity.
func (t *TypedActivity[I, O]) Name() string { return t.name }

// Execute implements Activity: decode, call, encode.
//
// A payload that does not decode is ClassTerminalTechnical, not a retry. It means the step input
// this binary was handed is not the shape this binary understands — a definition drift or a
// rollback onto older code — and no number of retries will change the bytes.
func (t *TypedActivity[I, O]) Execute(ctx context.Context, in Input) (Output, error) {
	var decoded I
	if len(in.Payload) > 0 {
		if err := json.Unmarshal(in.Payload, &decoded); err != nil {
			return nil, WithClass(apierror.Wrapf(err, apierror.CodeMalformedRequest,
				"step %q input does not decode into %T", in.Step, decoded), ClassTerminalTechnical)
		}
	}
	out, err := t.fn(ctx, in, decoded)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, WithClass(apierror.Wrapf(err, apierror.CodeInternalError,
			"step %q output does not encode", in.Step), ClassTerminalTechnical)
	}
	return encoded, nil
}

// Activities is the registry of executable activities, keyed by name.
//
// It is separate from the definition Registry because the two have different lifetimes and
// different owners: a definition is data that can be printed, diffed and stored, while an
// activity is code holding live ports. Keeping them apart is what lets a definition be
// validated in a test with no dependencies at all.
//
// Safe for concurrent use.
type Activities struct {
	mu sync.RWMutex
	m  map[string]Activity
}

// NewActivities returns an empty registry.
func NewActivities() *Activities { return &Activities{m: make(map[string]Activity, 16)} }

// Register adds an activity. A duplicate name is an error rather than an overwrite: silently
// replacing a registered activity means the behaviour of a step depends on package
// initialization order, which is not a thing anyone should have to debug.
func (a *Activities) Register(act Activity) error {
	if act == nil || act.Name() == "" {
		return apierror.New(apierror.CodeInternalError, "cannot register an unnamed activity")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.m == nil {
		a.m = make(map[string]Activity, 16)
	}
	if _, ok := a.m[act.Name()]; ok {
		return apierror.Newf(apierror.CodeInternalError, "activity %q is already registered", act.Name())
	}
	a.m[act.Name()] = act
	return nil
}

// MustRegister is Register for package initialization, where a duplicate is a programming error
// that must stop the process rather than be returned into a func init with nowhere to go.
func (a *Activities) MustRegister(acts ...Activity) {
	for _, act := range acts {
		if err := a.Register(act); err != nil {
			panic("engine: " + err.Error())
		}
	}
}

// Get returns the named activity.
func (a *Activities) Get(name string) (Activity, error) {
	a.mu.RLock()
	act, ok := a.m[name]
	a.mu.RUnlock()
	if !ok {
		return nil, apierror.Wrapf(ErrUnknownActivity, apierror.CodeInternalError,
			"activity %q is not registered in this binary", name)
	}
	return act, nil
}

// Has reports whether the named activity is registered, without building an error.
func (a *Activities) Has(name string) bool {
	a.mu.RLock()
	_, ok := a.m[name]
	a.mu.RUnlock()
	return ok
}

// Names returns every registered activity name, sorted.
func (a *Activities) Names() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, 0, len(a.m))
	for n := range a.m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// VerifyDefinition checks that every activity and compensation a definition names is present.
//
// It runs at startup, next to Register, because the failure it catches — a definition
// referencing an activity that was renamed or never wired — is otherwise discovered by the
// first instance to reach that step, which for `certification` is thirty minutes into a real
// merchant's onboarding.
func (a *Activities) VerifyDefinition(d *Definition) error {
	var missing []apierror.Detail
	for _, s := range d.Steps {
		if s.Activity != "" && !a.Has(s.Activity) {
			missing = append(missing, detailOf("steps."+s.Name+".activity", "UNREGISTERED_ACTIVITY",
				"activity "+s.Activity+" is not registered"))
		}
		if s.Compensation != "" && !a.Has(s.Compensation) {
			missing = append(missing, detailOf("steps."+s.Name+".compensation", "UNREGISTERED_ACTIVITY",
				"compensation "+s.Compensation+" is not registered"))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return apierror.Wrapf(ErrUnknownActivity, apierror.CodeInternalError,
		"workflow %s names %d activities this binary does not contain", d.Key(), len(missing)).
		WithDetails(missing...)
}
