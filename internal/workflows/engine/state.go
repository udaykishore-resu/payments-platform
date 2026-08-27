package engine

import "github.com/udaykishore-resu/payments-platform/internal/domain/shared"

// InstanceState is the lifecycle state of one workflow instance
// (docs/automation-plane.md §6.1 and §6.3).
//
// The values are the strings persisted in `workflow_instances.state`, so a state added here
// without a migration is a state the database's CHECK constraint will reject — which is the
// intended failure mode. A workflow whose state column can hold anything is a workflow whose
// invariants are advisory.
type InstanceState string

const (
	// InstancePending is created-or-requeued and awaiting its first lease. It holds no worker.
	InstancePending InstanceState = "PENDING"
	// InstanceRunning means a worker owns the lease and is executing a step.
	InstanceRunning InstanceState = "RUNNING"
	// InstanceRetryBackoff means a transient failure is waiting out jittered backoff. The wait
	// lives in RunAfter — a column — never in an in-memory timer, so a worker that dies during
	// backoff loses nothing.
	InstanceRetryBackoff InstanceState = "RETRY_BACKOFF"
	// InstanceWaitingSignal means the instance is parked on a manual or external gate with the
	// lease *released*. A seven-day KYC wait therefore holds zero worker resource; holding a
	// lease across it would both leak a slot and make the lease arithmetic in §4.1 meaningless.
	InstanceWaitingSignal InstanceState = "WAITING_SIGNAL"
	// InstanceParked means a gate timed out or an outcome is ambiguous and a human must act.
	// It is deliberately not a failure: a compliance review nobody performed is a late human,
	// not a broken system, and a late signal resumes the instance normally.
	InstanceParked InstanceState = "PARKED"
	// InstanceCompensating means the engine is walking completed steps in reverse order.
	InstanceCompensating InstanceState = "COMPENSATING"
	// InstanceCompleted is terminal success.
	InstanceCompleted InstanceState = "COMPLETED"
	// InstanceCompensated is terminal: everything undoable was undone.
	InstanceCompensated InstanceState = "COMPENSATED"
	// InstanceCanceled is terminal: cancelled by an operator and compensated.
	InstanceCanceled InstanceState = "CANCELED"
	// InstanceFailed is terminal-with-a-DLQ-entry. It is not a *state-machine* terminal state
	// because an operator requeue moves it back to PENDING.
	InstanceFailed InstanceState = "FAILED"
	// InstancePoisoned means the instance damaged its workers and has been quarantined: it is
	// invisible to every poller until an operator requeues it with a reset crash count. This
	// bounds the blast radius of a worker-killing instance to three worker deaths instead of
	// an indefinite cycle through the whole fleet (§4.3).
	InstancePoisoned InstanceState = "POISONED"
)

// AllInstanceStates is the complete state universe, for the exhaustive transition property
// test and for the CHECK-constraint generator.
var AllInstanceStates = []InstanceState{
	InstancePending, InstanceRunning, InstanceRetryBackoff, InstanceWaitingSignal,
	InstanceParked, InstanceCompensating, InstanceCompleted, InstanceCompensated,
	InstanceCanceled, InstanceFailed, InstancePoisoned,
}

// instanceMachine mirrors the state diagram in docs/automation-plane.md §6.1.
//
// Only COMPLETED and CANCELED are declared terminal, even though FAILED, COMPENSATED and
// POISONED all read as endings. The reason is the operator surface: a requeue moves FAILED and
// POISONED back to PENDING, and a cancellation that finishes compensating moves COMPENSATED to
// CANCELED. Declaring those terminal would make the transition table disagree with the runbook,
// and shared.NewStateMachine panics on an outgoing edge from a terminal state — so the
// disagreement would be a startup panic rather than a quiet lie. Use IsFinal for the
// "no longer live" question; that is the predicate the unique business-key index uses.
var instanceMachine = shared.NewStateMachine("workflow_instance", InstancePending,
	AllInstanceStates,
	[]InstanceState{InstanceCompleted, InstanceCanceled},
	[]shared.Transition[InstanceState]{
		{From: InstancePending, To: InstanceRunning},

		// Declared explicitly because shared.StateMachine refuses implicit self-transitions:
		// RUNNING → RUNNING is the ordinary "step succeeded, next step begins" edge and it must
		// be legal rather than accidentally permitted.
		{From: InstanceRunning, To: InstanceRunning},

		{From: InstanceRunning, To: InstanceRetryBackoff},
		{From: InstanceRetryBackoff, To: InstanceRunning},

		{From: InstanceRunning, To: InstanceWaitingSignal},
		{From: InstanceWaitingSignal, To: InstanceRunning},
		{From: InstanceWaitingSignal, To: InstanceParked},
		{From: InstanceParked, To: InstanceRunning},
		{From: InstanceParked, To: InstanceCompensating},

		// A step classified ClassManual — an ambiguous outcome a lookup could not resolve, or a
		// refusal to roll back past the money pivot — parks rather than fails.
		{From: InstanceRunning, To: InstanceParked},

		{From: InstanceRunning, To: InstanceCompensating},
		{From: InstanceRetryBackoff, To: InstanceCompensating},

		{From: InstanceCompensating, To: InstanceCompensated},
		{From: InstanceCompensating, To: InstanceFailed},
		{From: InstanceCompensating, To: InstanceParked},

		{From: InstanceRunning, To: InstanceFailed},
		{From: InstanceRunning, To: InstancePoisoned},
		{From: InstanceRunning, To: InstanceCompleted},

		{From: InstanceCompensated, To: InstanceCanceled},

		{From: InstancePoisoned, To: InstancePending},
		{From: InstanceFailed, To: InstancePending},
	})

// InstanceMachine exposes the instance transition table so that the operator surface, the
// documentation generator and the tests all read the same source of truth.
func InstanceMachine() *shared.StateMachine[InstanceState] { return instanceMachine }

// String satisfies fmt.Stringer.
func (s InstanceState) String() string { return string(s) }

// IsKnown reports whether s is a state this binary understands. A row carrying an unknown state
// means a rollback landed on data written by a newer version, and must fail loudly.
func (s InstanceState) IsKnown() bool { return instanceMachine.IsKnown(s) }

// IsFinal reports whether the instance is no longer live.
//
// This is deliberately broader than the state machine's IsTerminal and it is the predicate that
// matters operationally: it is exactly the set excluded by the partial unique index
// `wfi_live_business_key`, so "one live onboarding per merchant" and "this instance is finished"
// are the same question answered by the same list.
func (s InstanceState) IsFinal() bool {
	switch s {
	case InstanceCompleted, InstanceFailed, InstanceCompensated, InstanceCanceled:
		return true
	default:
		return false
	}
}

// IsLeased reports whether a worker holds the instance while it is in this state. Used by the
// lease reaper and by the shutdown path that releases leases explicitly.
func (s InstanceState) IsLeased() bool {
	return s == InstanceRunning || s == InstanceCompensating
}

// IsRunnable reports whether a poller may lease an instance in this state once its RunAfter has
// elapsed. POISONED is excluded on purpose: a quarantined instance must be invisible to every
// poller, which is the whole mechanism that stops it cycling the fleet.
func (s InstanceState) IsRunnable() bool {
	switch s {
	case InstancePending, InstanceRunning, InstanceRetryBackoff, InstanceCompensating,
		InstanceWaitingSignal, InstanceParked:
		return true
	default:
		return false
	}
}

// StepState is the lifecycle state of one step of one instance (§6.2 and §6.4).
type StepState string

const (
	// StepPending means the step has not been attempted.
	StepPending StepState = "PENDING"
	// StepRunning means attempt n is in flight, with timeout_at set and the lease epoch stamped.
	StepRunning StepState = "RUNNING"
	// StepSucceeded means the output was checkpointed. The same transaction moved the merchant
	// FSM and wrote the outbox row, which is why a resumed instance never has to reconcile
	// "the step is done but the domain effect is missing".
	StepSucceeded StepState = "SUCCEEDED"
	// StepFailed means the attempt returned an error awaiting classification.
	StepFailed StepState = "FAILED"
	// StepTimedOut means the per-attempt deadline elapsed.
	StepTimedOut StepState = "TIMED_OUT"
	// StepAmbiguous means the step timed out while it may have had an external side effect. The
	// next attempt begins with lookup-before-act; it is never a blind retry, because an unknown
	// outcome resolved by assumption is how duplicate side effects reach production.
	StepAmbiguous StepState = "AMBIGUOUS"
	// StepLeaseLost means a fenced write matched zero rows: another worker owns the instance and
	// this one abandoned it without having corrupted anything.
	StepLeaseLost StepState = "LEASE_LOST"
	// StepRetryScheduled means next_retry_at is set and the same deterministic idempotency key
	// will be reused on the next attempt.
	StepRetryScheduled StepState = "RETRY_SCHEDULED"
	// StepDLQ means the step is parked for operator triage with its full error chain.
	StepDLQ StepState = "DLQ"
	// StepCompensating, StepCompensated and StepCompensationFailed are the compensation
	// lifecycle. COMPENSATION_FAILED is the highest-severity workflow state there is: real
	// external state is now orphaned and nothing else will clean it up.
	StepCompensating       StepState = "COMPENSATING"
	StepCompensated        StepState = "COMPENSATED"
	StepCompensationFailed StepState = "COMPENSATION_FAILED"
	// StepSkipped means the step does not apply to this instance — a fan-out branch for a
	// gateway the merchant did not select.
	StepSkipped StepState = "SKIPPED"
)

// AllStepStates is the complete step state universe.
var AllStepStates = []StepState{
	StepPending, StepRunning, StepSucceeded, StepFailed, StepTimedOut, StepAmbiguous,
	StepLeaseLost, StepRetryScheduled, StepDLQ, StepCompensating, StepCompensated,
	StepCompensationFailed, StepSkipped,
}

// stepMachine mirrors docs/automation-plane.md §6.2.
var stepMachine = shared.NewStateMachine("workflow_step", StepPending,
	AllStepStates,
	[]StepState{StepCompensated, StepCompensationFailed, StepSkipped},
	[]shared.Transition[StepState]{
		{From: StepPending, To: StepRunning},
		{From: StepPending, To: StepSkipped},

		{From: StepRunning, To: StepSucceeded},
		{From: StepRunning, To: StepFailed},
		{From: StepRunning, To: StepTimedOut},
		{From: StepRunning, To: StepLeaseLost},

		{From: StepLeaseLost, To: StepPending},

		{From: StepFailed, To: StepRetryScheduled},
		{From: StepFailed, To: StepDLQ},

		{From: StepTimedOut, To: StepRetryScheduled},
		{From: StepTimedOut, To: StepAmbiguous},
		{From: StepTimedOut, To: StepDLQ},

		{From: StepAmbiguous, To: StepRunning},
		{From: StepAmbiguous, To: StepDLQ},

		{From: StepRetryScheduled, To: StepRunning},

		{From: StepDLQ, To: StepPending},

		{From: StepSucceeded, To: StepCompensating},
		{From: StepCompensating, To: StepCompensated},
		{From: StepCompensating, To: StepCompensationFailed},
	})

// StepMachine exposes the step transition table.
func StepMachine() *shared.StateMachine[StepState] { return stepMachine }

// String satisfies fmt.Stringer.
func (s StepState) String() string { return string(s) }

// IsKnown reports whether s is a step state this binary understands.
func (s StepState) IsKnown() bool { return stepMachine.IsKnown(s) }

// IsComplete reports whether the step's forward work is done and its output is checkpointed.
// This is the predicate that makes resume replay-free: the engine skips every step for which it
// is true and runs the next one.
func (s StepState) IsComplete() bool { return s == StepSucceeded }

// NeedsCompensation reports whether an aborting instance must run this step's compensation.
// Only a step that actually succeeded created anything worth undoing.
func (s StepState) NeedsCompensation() bool { return s == StepSucceeded }
