// Package engine is the durable workflow/saga port behind the automation plane.
//
// It declares what a workflow engine must do and knows nothing about how: two implementations
// satisfy it — `engine/postgres`, the default, and `engine/temporal`, an adapter — and
// `internal/workflows/onboarding` is unchanged between them. That is the payoff of the port,
// and it is what makes the build-vs-buy decision in docs/architecture.md TR-4 reversible
// instead of a rewrite.
//
// The five guarantees the automation plane is built on (baseline §11, expanded in
// docs/automation-plane.md), and where each is implemented:
//
//   - **Start is idempotent on the business key.** Starting `merchant-onboarding@v1` twice for
//     one merchant returns the existing instance. This is the mechanism that guarantees one
//     live onboarding per merchant; it is not a convenience for retrying clients.
//   - **Every step's result is checkpointed before the next step begins**, so resume replays no
//     completed step. Resume reads the accumulated context and runs the *next* step; it does
//     not fold a history and does not require activity code to be deterministic.
//   - **An aborted instance runs the compensations of completed steps in strict reverse order**,
//     each idempotent, each retried — except past a pivot, where the engine refuses to roll back
//     and parks for a human instead.
//   - **A step that exhausts its retries** moves the instance to FAILED and its payload plus the
//     full error chain to the DLQ.
//   - **A manual gate blocks until an authorized principal signals it**, holding no worker
//     resource while it waits, and the signal is itself audited.
//
// This package may import the domain and internal/application/ports. It may not import
// infrastructure: the metric and audit hooks below are narrow interfaces the wiring satisfies,
// which is what keeps `go test ./internal/workflows/...` free of a database and a Prometheus
// registry.
package engine

import (
	"context"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

// Engine is the workflow port.
//
// Five methods, because a workflow engine's surface is genuinely small: you start one, you
// signal it, you cancel it, you read it, and you nudge it. Everything else — retries, backoff,
// leases, fencing, compensation, the DLQ — is the engine's job and not the caller's, and a port
// that exposed those would be a port that leaked its implementation into every use case.
type Engine interface {
	// Start creates an instance, or returns the existing one.
	//
	// It is idempotent on (definition, businessKey): if a *live* instance exists for that pair
	// it is returned unchanged and nothing is created. A terminal instance does not block a
	// restart, so a merchant whose onboarding failed can be onboarded again — as a new,
	// separately auditable attempt rather than by resurrecting the old one.
	//
	// The definition is registered on first use, so a caller need not register separately; a
	// definition that disagrees with one already registered under the same key is rejected
	// rather than silently replacing it.
	Start(ctx context.Context, def *Definition, businessKey string, input []byte) (shared.WorkflowID, error)

	// Signal delivers a signal to an instance.
	//
	// Delivery is durable and at-most-once, and a signal that arrives *before* its wait begins
	// is recorded and consumed the instant the wait starts. Losing a racing signal is the
	// classic bug in naive implementations — the KYC vendor's webhook routinely beats our own
	// commit — and the storage shape, not a lock, is what prevents it.
	//
	// The payload carries the principal, which is written to the audit trail: a manual gate
	// whose approval cannot be attributed to a person is not a control.
	Signal(ctx context.Context, id shared.WorkflowID, name string, payload SignalPayload) error

	// Cancel requests cooperative cancellation.
	//
	// Cooperative, not immediate: an in-flight external call is never abandoned. Abandoning it
	// produces exactly the ambiguity we spend the rest of this package avoiding — we would not
	// know whether the side effect happened. Waiting out an eight-second call is cheaper than
	// owning an unknown. After the current step settles, compensations run in reverse order.
	Cancel(ctx context.Context, id shared.WorkflowID, reason string) error

	// Get returns the instance's current state, including its step history.
	Get(ctx context.Context, id shared.WorkflowID) (*Instance, error)

	// Resume drives an instance forward now, rather than waiting for a poller to lease it.
	//
	// It is the operator's "try again", the API's post-signal nudge, and the tests' way of
	// advancing a workflow deterministically. It returns when the instance is no longer
	// immediately runnable: completed, failed, parked, waiting for a signal, or backing off.
	Resume(ctx context.Context, id shared.WorkflowID) error
}

// Instance is the runtime view of one workflow execution.
type Instance struct {
	ID          shared.WorkflowID
	TenantID    shared.TenantID
	Definition  string
	Version     int
	BusinessKey string

	State       InstanceState
	CurrentStep string

	// Input is the document Start was given. Immutable for the instance's life.
	Input []byte
	// Context is the accumulated JSON object of completed step outputs, keyed by step name.
	// It is what makes resume replay-free, and it is deliberately the *only* mutable state
	// besides the instance's own columns: there is no operator command to edit it, because
	// arbitrary state mutation makes the audit trail meaningless.
	Context []byte

	// Attempt is the attempt number of the current step, persisted so that a crash does not
	// reset a step's retry budget and let a failing vendor be hammered indefinitely.
	Attempt int

	LeaseOwner string
	// LeaseEpoch is the fencing token. Every write a worker makes carries the epoch it
	// acquired; a stale epoch writes zero rows and the worker aborts.
	LeaseEpoch int64
	LeaseUntil *time.Time

	// RunAfter is when the instance next becomes runnable: retry backoff, or a signal timeout.
	// It is a column rather than an in-memory timer precisely so that a worker dying during a
	// backoff loses nothing.
	RunAfter time.Time

	LastError   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time

	// Steps is the checkpointed history, ordered by sequence.
	Steps []StepRecord
}

// IsFinal reports whether the instance has reached a terminal outcome.
func (i *Instance) IsFinal() bool { return i.State.IsFinal() }

// CompletedSteps returns the names of steps whose output is checkpointed, in execution order.
// This is the list a resume skips.
func (i *Instance) CompletedSteps() []string {
	out := make([]string, 0, len(i.Steps))
	for _, s := range i.Steps {
		if s.State.IsComplete() {
			out = append(out, s.Name)
		}
	}
	return out
}

// StepRecord is the checkpointed result of one step attempt.
type StepRecord struct {
	ID         shared.StepID
	Name       string
	Sequence   int
	State      StepState
	Attempt    int
	Input      []byte
	Output     []byte
	Error      string
	LeaseEpoch int64

	StartedAt     *time.Time
	CompletedAt   *time.Time
	CompensatedAt *time.Time
}

// SignalPayload is a signal and everything the audit trail needs about it.
//
// The principal is a field rather than a context value because it is not optional: baseline §11
// requires that a manual gate's signal is itself audited, and an audit record whose actor is
// "unknown" is a record that fails the control it exists to evidence. Making it a struct field
// means a caller that forgot it is visible in review rather than at audit time.
type SignalPayload struct {
	// Data is the signal body as JSON — the KYC decision, the compliance verdict.
	Data []byte
	// Principal is who sent it: an operator's subject claim, or the vendor ACL's service
	// identity for a webhook-sourced signal.
	Principal string
	// Scopes are the authorization scopes the principal presented.
	Scopes []string
	// Reason is the free-text justification. Every mutating operator action requires one,
	// because an action with no recorded reason is unreviewable six months later.
	Reason string
	// SourceIP is the caller's address, for the audit record.
	SourceIP string
	// ReceivedAt is when the platform accepted the signal.
	ReceivedAt time.Time
	// IdempotencyKey is the caller's key, recorded so a duplicate delivery is provably a
	// duplicate rather than a second decision.
	IdempotencyKey string
}

// Metrics is the engine's metric hook.
//
// It is an interface with two methods rather than a dependency on the telemetry registry so
// that this package — and every test in it — needs no Prometheus registry, no exposition
// format, and no global state. The two series are the ones baseline §22.2 names for workflows;
// the implementation in the wiring layer forwards them to `pp_workflow_step_duration_seconds`
// and `pp_workflow_instances`.
//
// Cardinality rule: neither method takes a merchant or instance identifier, and that is
// enforced by the signature rather than by a review comment. Instance identity belongs in logs,
// traces and exemplars, never in a label.
type Metrics interface {
	// ObserveStepDuration records one step attempt completing. Outcome is one of
	// "success", "failed", "timeout", "skipped", "waiting".
	ObserveStepDuration(ctx context.Context, workflow, step, outcome string, d time.Duration)

	// SetInstances publishes the live distribution of instances across the state machine.
	SetInstances(ctx context.Context, workflow, state string, n float64)
}

// NopMetrics discards everything. It is the default so that an engine constructed without
// wiring still runs — a metric hook that panics on nil is a metric hook that turns an
// observability gap into an outage.
type NopMetrics struct{}

// ObserveStepDuration implements Metrics.
func (NopMetrics) ObserveStepDuration(context.Context, string, string, string, time.Duration) {}

// SetInstances implements Metrics.
func (NopMetrics) SetInstances(context.Context, string, string, float64) {}

// Step outcome label values, kept here so the engine and the telemetry registry cannot drift.
const (
	OutcomeSuccess = "success"
	OutcomeFailed  = "failed"
	OutcomeTimeout = "timeout"
	OutcomeSkipped = "skipped"
	OutcomeWaiting = "waiting"
)

// AuditEvent is one auditable engine action. Signals and cancellations produce them.
type AuditEvent struct {
	WorkflowID  shared.WorkflowID
	TenantID    shared.TenantID
	BusinessKey string
	Action      string
	Step        string
	Principal   string
	Scopes      []string
	Reason      string
	SourceIP    string
	OccurredAt  time.Time
	Payload     []byte
}

// Auditor receives auditable engine actions.
//
// Narrow on purpose: the engine must not know that audit records are hash-chained, or that they
// live in Postgres, or that they are retained for seven years. It knows that a signal and a
// cancellation are things a person did and that somebody downstream needs to be able to prove
// who did them.
type Auditor interface {
	Record(ctx context.Context, e AuditEvent) error
}

// NopAuditor discards audit events. Acceptable in tests; wiring an engine with it in production
// would mean a manual gate whose approvals are unattributable, which is why the constructor
// takes the auditor explicitly rather than defaulting to this.
type NopAuditor struct{}

// Record implements Auditor.
func (NopAuditor) Record(context.Context, AuditEvent) error { return nil }

// Audit action names.
const (
	ActionSignal   = "workflow.signal"
	ActionCancel   = "workflow.cancel"
	ActionPark     = "workflow.park"
	ActionDLQ      = "workflow.dlq"
	ActionRequeue  = "workflow.requeue"
	ActionCompStep = "workflow.compensate"
)

// Stuck describes an instance that is making no progress.
//
// A failing workflow is visible. A silently non-progressing one is not, and it is the failure
// mode that lets an onboarding sit for three days before a customer asks about it. The fields
// are chosen so that the alert carries a diagnosis — a pod name and a step — rather than a
// number somebody has to go and query.
type Stuck struct {
	ID          shared.WorkflowID
	Definition  string
	BusinessKey string
	State       InstanceState
	Step        string
	LeaseOwner  string
	// NoProgressFor is now − updated_at. The detector deliberately checks updated_at rather
	// than heartbeat_at: the worst case is a zombie that is heartbeating happily while doing no
	// work, and a heartbeat-based check is exactly blind to it.
	NoProgressFor time.Duration
	LastError     string
}
