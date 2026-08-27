package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// PivotKind qualifies a saga pivot — the point after which the transaction is no longer
// reversible.
//
// One boolean is not enough, and conflating the two kinds produces a wrong design. Onboarding
// has two *independent* irreversibility dimensions (docs/automation-plane.md §2.3): a regulated
// record that must be retained, and a state after which real money can move. They have
// different consequences, so they need different markers.
type PivotKind string

const (
	// PivotNone is an ordinary step.
	PivotNone PivotKind = ""

	// PivotRetained marks external/regulatory irreversibility: the step created a record a
	// third party is legally required to keep. Submitting a merchant's principals to a KYC
	// vendor is the example — cancelling the case stops the *process*, but the submitted data
	// is retained for five years under a legal-obligation basis and there is nothing to undo.
	//
	// Consequence at runtime: once a retained pivot has *completed*, the engine skips the
	// compensations of that step and every step before it. An abort after it produces a
	// merchant in a failed terminal state with the record intact — never a merchant that looks
	// as though they never applied.
	PivotRetained PivotKind = "RETAINED"

	// PivotIrreversible marks money-path irreversibility: after this step, payments can exist,
	// and each has its own lifecycle that the saga may not unilaterally unwind.
	//
	// Consequence at runtime: once an irreversible pivot has completed, the engine **refuses to
	// abort at all**. It parks the instance for manual intervention rather than de-provisioning
	// a merchant that may have live payments. Recovery past this point is roll-forward only.
	PivotIrreversible PivotKind = "IRREVERSIBLE"
)

// CompensationKind distinguishes an undo from a forward recovery.
//
// This distinction is what lets the same field express "de-provision the sub-account" and
// "suspend the merchant" without the validator having to guess which one is safe after a pivot.
// A rollback restores the prior world; a forward recovery moves to a *new* safe state and is
// the only kind permitted after an irreversible pivot. Suspending an active merchant blocks new
// payments while deliberately continuing to permit refunds and voids — "undoing" activation by
// blocking refunds would trap merchant money, which is why rollback is the wrong verb here.
type CompensationKind string

const (
	// CompensationRollback restores the world as it was.
	CompensationRollback CompensationKind = "ROLLBACK"
	// CompensationForward moves to a new safe state instead of restoring the old one.
	CompensationForward CompensationKind = "FORWARD_RECOVERY"
)

// RetryPolicy is a step's retry configuration.
//
// MaxAttempts counts *attempts*, not retries: `3 × 200 ms` in baseline §11 means three total
// executions. Off-by-one here is not cosmetic — it is the difference between a vendor seeing
// three requests and four during an outage, multiplied by every instance in the backlog.
type RetryPolicy struct {
	// MaxAttempts is the total number of executions. 1 means "no retry".
	MaxAttempts int
	// InitialInterval is the first backoff ceiling.
	InitialInterval time.Duration
	// MaxInterval caps the exponential growth.
	MaxInterval time.Duration
	// BackoffFactor is the growth multiplier, 2.0 everywhere in this platform.
	BackoffFactor float64
	// NonRetryable lists classes that skip the retry loop regardless of attempts remaining.
	// ClassTerminalBusiness and ClassTerminalTechnical are always non-retryable; listing a
	// class here adds to that set rather than replacing it.
	NonRetryable []FailureClass
}

// NoRetry is the policy for a step that must execute exactly once.
func NoRetry() RetryPolicy { return RetryPolicy{MaxAttempts: 1} }

// Retries reports whether the policy permits more than one execution. A step that is not
// idempotent and whose policy returns true here is unsound, and Validate rejects it.
func (p RetryPolicy) Retries() bool { return p.MaxAttempts > 1 }

// Permits reports whether class may be retried under this policy.
func (p RetryPolicy) Permits(class FailureClass) bool {
	if class.IsTerminal() {
		return false
	}
	for _, c := range p.NonRetryable {
		if c == class {
			return false
		}
	}
	return true
}

func (p RetryPolicy) normalized() RetryPolicy {
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	if p.InitialInterval <= 0 {
		p.InitialInterval = 200 * time.Millisecond
	}
	if p.MaxInterval < p.InitialInterval {
		p.MaxInterval = p.InitialInterval
	}
	if p.BackoffFactor < 1 {
		p.BackoffFactor = 2.0
	}
	return p
}

// Step is one node of a saga.
//
// Steps execute in declaration order and are addressed by name, never by index: an instance
// persists `current_step` as a name so that adding a step to a *new* version of a definition
// cannot silently shift a live instance onto a different activity.
type Step struct {
	// Name identifies the step in the database, in metrics, in logs and in the operator
	// surface. It must be unique within the definition — see Validate.
	Name string

	// Activity names the registered Activity this step executes. Empty is legal only for a
	// manual gate, which waits for a signal rather than running code.
	Activity string

	// Signal is the signal name a manual gate waits for.
	Signal string

	// Retry is the per-step policy. Backoff is exponential with full jitter and is persisted as
	// a timestamp, never held in an in-memory timer.
	Retry RetryPolicy

	// Timeout bounds one *attempt*, not the step's lifetime. For a manual gate it bounds the
	// wait for the signal, and reaching it parks the instance rather than failing it.
	Timeout time.Duration

	// Compensation names the registered Activity that undoes this step. Empty is a positive
	// declaration that nothing needs undoing — reviewed like any other design decision — not an
	// oversight.
	Compensation string

	// CompensationKind says whether the compensation restores the prior world or moves forward
	// to a new safe state. Defaults to CompensationRollback when a compensation is declared.
	CompensationKind CompensationKind

	// Idempotent asserts that executing this step twice with the same deterministic idempotency
	// key produces one business effect. Only an idempotent step may be retried; Validate
	// enforces that, because a retry of a non-idempotent step is a duplicate side effect with a
	// configuration file for an excuse.
	Idempotent bool

	// ManualGate marks a step that waits for a human. The lease is released while it waits, so
	// a five-day compliance review holds no worker resource at all.
	ManualGate bool

	// Pivot marks the step as a saga pivot; PivotKind says which kind.
	Pivot bool

	// PivotKind qualifies Pivot. Ignored when Pivot is false.
	PivotKind PivotKind

	// SideEffecting declares that the step may change state in an external system. It drives
	// the ambiguity rule: a timeout on a side-effecting step is ClassAmbiguous and resolved by
	// lookup-before-act, while a timeout on a pure step is merely transient.
	SideEffecting bool

	// FanOut declares that the step performs bounded concurrent sub-executions and checkpoints
	// each branch as it completes, so a crash mid-fan-out does not repeat the branches that
	// already succeeded.
	FanOut bool

	// Description is the one-line human summary rendered by the operator surface.
	Description string
}

// IsRollback reports whether the step's compensation restores the prior world.
func (s Step) IsRollback() bool {
	return s.Compensation != "" && s.CompensationKind != CompensationForward
}

// Definition is a workflow: an ordered list of steps plus the rule for extracting the
// deduplication key from the input.
type Definition struct {
	// Name is the workflow type, e.g. "merchant-onboarding".
	Name string

	// Version is the definition version. A new version is a new definition: in-flight instances
	// finish on the version they started with, which is why `current_step` is a name and why
	// the version is part of the registry key. Patching a running instance onto new code is
	// Temporal's model, not ours, and it costs determinism constraints we do not want.
	Version int

	// Steps execute in order.
	Steps []Step

	// BusinessKeyOf extracts the deduplication key from the start input. Starting a workflow
	// twice with the same key returns the existing instance, which is the mechanism that
	// guarantees one live onboarding per merchant. Optional: a caller may pass the key
	// explicitly to Start instead.
	BusinessKeyOf func(input []byte) (string, error)

	// Description is the human summary.
	Description string
}

// Key is the registry key, "name@vN". It is the string persisted in `workflow_instances`
// (as type + version) and the one used in log fields and metric labels.
func (d *Definition) Key() string {
	return d.Name + "@v" + strconv.Itoa(d.Version)
}

// StepIndex returns the position of the named step, or -1.
func (d *Definition) StepIndex(name string) int {
	for i := range d.Steps {
		if d.Steps[i].Name == name {
			return i
		}
	}
	return -1
}

// StepByName returns the named step, or nil.
func (d *Definition) StepByName(name string) *Step {
	if i := d.StepIndex(name); i >= 0 {
		return &d.Steps[i]
	}
	return nil
}

// FirstStep returns the name of the step a new instance begins at.
func (d *Definition) FirstStep() string {
	if len(d.Steps) == 0 {
		return ""
	}
	return d.Steps[0].Name
}

// NextStep returns the step following name, or "" when name is the last step.
func (d *Definition) NextStep(name string) string {
	i := d.StepIndex(name)
	if i < 0 || i+1 >= len(d.Steps) {
		return ""
	}
	return d.Steps[i+1].Name
}

// PivotIndex returns the position of the last pivot of the given kind, or -1 when the
// definition declares none. The engine compares a failing step's position against this to
// decide whether an abort is permitted at all.
func (d *Definition) PivotIndex(kind PivotKind) int {
	idx := -1
	for i := range d.Steps {
		if d.Steps[i].Pivot && d.Steps[i].PivotKind == kind {
			idx = i
		}
	}
	return idx
}

// Validate rejects definitions that cannot produce a sound saga.
//
// Every check here is a defect that is invisible in a code review of the definition table and
// catastrophic at three in the morning, and every one of them fails the *process at startup*
// rather than an instance at runtime. That choice is the whole point: a definition defect
// discovered by a live merchant's onboarding is a defect discovered after the side effects have
// already happened.
//
// The four unsoundness classes:
//
//  1. **A rollback compensation declared after an irreversible pivot.** The pivot exists
//     precisely because the world past it cannot be restored. A compensation declared there
//     would be executed by the compensator, would fail or — worse — would succeed at
//     destroying state that live payments depend on. Only a forward recovery is legal there.
//
//  2. **A manual gate with no timeout.** A gate that waits forever is an instance nobody will
//     ever see again: it is not failed, so it raises no alert, and it is not stuck by the
//     detector's definition either, because WAITING_SIGNAL is excluded from the stuck sweep on
//     purpose. The timeout is what converts "a human forgot" into a parked instance with an
//     escalation.
//
//  3. **A non-idempotent step with retries enabled.** The retry is the duplicate. There is no
//     amount of backoff that makes a second non-deduplicated create safe.
//
//  4. **An unreachable step.** Steps are addressed by name, so a duplicated name makes every
//     occurrence after the first unreachable: the engine resolves `current_step` to the first
//     match and can never advance to the later one. The step would sit in the definition
//     looking like a control that exists — a certification or a compliance review that in fact
//     never runs.
func (d *Definition) Validate() error {
	var details []apierror.Detail

	if strings.TrimSpace(d.Name) == "" {
		details = append(details, detailOf("name", "REQUIRED", "a definition needs a name"))
	}
	if d.Version < 1 {
		details = append(details, detailOf("version", "INVALID", "version must be >= 1"))
	}
	if len(d.Steps) == 0 {
		details = append(details, detailOf("steps", "EMPTY", "a definition needs at least one step"))
	}

	irreversible := d.PivotIndex(PivotIrreversible)
	seen := make(map[string]int, len(d.Steps))

	for i, s := range d.Steps {
		field := "steps[" + strconv.Itoa(i) + "]"

		if strings.TrimSpace(s.Name) == "" {
			details = append(details, detailOf(field+".name", "REQUIRED", "a step needs a name"))
		}

		// (4) unreachable step.
		if first, dup := seen[s.Name]; dup && s.Name != "" {
			details = append(details, detailOf(field+".name", "UNREACHABLE_STEP",
				fmt.Sprintf("step %q is unreachable: the name is already used at index %d, and the engine resolves current_step to the first match", s.Name, first)))
		} else if s.Name != "" {
			seen[s.Name] = i
		}

		// (2) manual gate with no timeout.
		if s.ManualGate {
			if s.Timeout <= 0 {
				details = append(details, detailOf(field+".timeout", "MANUAL_GATE_WITHOUT_TIMEOUT",
					fmt.Sprintf("manual gate %q has no timeout: an unsignalled instance would wait forever, invisible to both the failure alerts and the stuck detector", s.Name)))
			}
			if s.Signal == "" {
				details = append(details, detailOf(field+".signal", "REQUIRED",
					fmt.Sprintf("manual gate %q must name the signal it waits for", s.Name)))
			}
		} else {
			if s.Activity == "" {
				details = append(details, detailOf(field+".activity", "REQUIRED",
					fmt.Sprintf("step %q is not a manual gate and must name an activity", s.Name)))
			}
			if s.Timeout <= 0 {
				details = append(details, detailOf(field+".timeout", "REQUIRED",
					fmt.Sprintf("step %q needs a per-attempt timeout; without one a wedged activity holds a slot until the process dies", s.Name)))
			}
		}

		// (3) non-idempotent step with retries enabled.
		if s.Retry.Retries() && !s.Idempotent {
			details = append(details, detailOf(field+".retry", "RETRY_WITHOUT_IDEMPOTENCE",
				fmt.Sprintf("step %q declares %d attempts but is not idempotent: the retry is the duplicate side effect", s.Name, s.Retry.MaxAttempts)))
		}

		// (1) rollback compensation after an irreversible pivot.
		if irreversible >= 0 && i > irreversible && s.IsRollback() {
			details = append(details, detailOf(field+".compensation", "COMPENSATION_AFTER_PIVOT",
				fmt.Sprintf("step %q declares the rollback %q but sits after the irreversible pivot %q; past that pivot recovery is roll-forward only",
					s.Name, s.Compensation, d.Steps[irreversible].Name)))
		}

		if s.Pivot && s.PivotKind == PivotNone {
			details = append(details, detailOf(field+".pivotKind", "REQUIRED",
				fmt.Sprintf("step %q is marked as a pivot but does not say which kind of irreversibility it introduces", s.Name)))
		}
		if !s.Pivot && s.PivotKind != PivotNone {
			details = append(details, detailOf(field+".pivot", "INCONSISTENT",
				fmt.Sprintf("step %q declares a pivot kind but is not marked as a pivot", s.Name)))
		}
	}

	if len(details) == 0 {
		return nil
	}
	return apierror.Wrapf(ErrDefinitionInvalid, apierror.CodeConfigurationInvalid,
		"workflow definition %s is not sound: %d problem(s)", d.Key(), len(details)).
		WithDetails(details...)
}

func detailOf(field, code, msg string) apierror.Detail {
	return apierror.Detail{Field: field, Code: code, Message: msg, RuleID: "WF." + code}
}

// normalize fills defaults that only matter to the engine, leaving the declared values alone.
// Called once at registration so that the runtime never has to ask "is this zero on purpose".
func (d *Definition) normalize() {
	for i := range d.Steps {
		d.Steps[i].Retry = d.Steps[i].Retry.normalized()
		if d.Steps[i].Compensation != "" && d.Steps[i].CompensationKind == "" {
			d.Steps[i].CompensationKind = CompensationRollback
		}
	}
}

// Registry holds the definitions a process knows about, keyed by `name@vN`.
//
// It exists because an instance persists only its type and version: a worker that leases an
// instance must be able to recover the definition from those two strings alone, having no
// memory of the process that started it. A definition missing from the registry is therefore a
// deployment error — a binary asked to run a workflow it does not contain — and the engine
// reports it as one rather than failing the instance.
//
// Safe for concurrent use: definitions are registered at startup and read by every worker
// goroutine thereafter.
type Registry struct {
	mu   sync.RWMutex
	defs map[string]*Definition
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{defs: make(map[string]*Definition, 4)} }

// Register validates a definition and stores it.
//
// Re-registering an identical key is a no-op rather than an error, because Start auto-registers
// the definition it is given: a process that starts an instance and also registers the
// definition at boot must not fail on the second call. Registering a *different* definition
// under the same key is an error — that is a version bump somebody forgot to make.
func (r *Registry) Register(d *Definition) error {
	if d == nil {
		return apierror.New(apierror.CodeConfigurationInvalid, "cannot register a nil definition")
	}
	if err := d.Validate(); err != nil {
		return err
	}
	d.normalize()

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.defs[d.Key()]; ok {
		if !sameShape(existing, d) {
			return apierror.Newf(apierror.CodeConfigurationInvalid,
				"workflow %s is already registered with a different step list; bump the version instead of editing a live definition", d.Key())
		}
		return nil
	}
	if r.defs == nil {
		r.defs = make(map[string]*Definition, 4)
	}
	r.defs[d.Key()] = d
	return nil
}

// Get returns the definition for `name@vN`.
func (r *Registry) Get(key string) (*Definition, error) {
	r.mu.RLock()
	d, ok := r.defs[key]
	r.mu.RUnlock()
	if !ok {
		return nil, apierror.Newf(apierror.CodeWorkflowNotFound,
			"workflow definition %s is not registered in this binary", key)
	}
	return d, nil
}

// Lookup returns the definition for a persisted (type, version) pair.
func (r *Registry) Lookup(name string, version int) (*Definition, error) {
	return r.Get(name + "@v" + strconv.Itoa(version))
}

// Keys returns every registered key, sorted, for the health endpoint and for diagnostics.
func (r *Registry) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.defs))
	for k := range r.defs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sameShape compares the parts of two definitions whose divergence would change how a live
// instance executes. Function fields are excluded deliberately: they cannot be compared, and a
// process that registers the same steps with a differently-built closure is the normal case.
func sameShape(a, b *Definition) bool {
	if len(a.Steps) != len(b.Steps) {
		return false
	}
	for i := range a.Steps {
		x, y := a.Steps[i], b.Steps[i]
		if x.Name != y.Name || x.Activity != y.Activity || x.Compensation != y.Compensation ||
			x.Signal != y.Signal || x.ManualGate != y.ManualGate || x.Pivot != y.Pivot ||
			x.PivotKind != y.PivotKind || x.Timeout != y.Timeout ||
			x.Retry.MaxAttempts != y.Retry.MaxAttempts {
			return false
		}
	}
	return true
}
