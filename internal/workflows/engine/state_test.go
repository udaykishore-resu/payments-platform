package engine

import (
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

// exhaustive is the property test docs/spec/06-code-conventions.md rule 14 requires of every
// state machine: for every (from, to) pair in the state universe, the machine accepts exactly
// the pairs in its table.
//
// The value is not in the pairs it accepts — those are the ones someone thought about. It is in
// the several hundred it must reject, which is where an illegal transition nobody imagined would
// otherwise sit untested until the day it happens.
func exhaustive[S ~string](t *testing.T, sm *shared.StateMachine[S], all []S) {
	t.Helper()
	legal := make(map[S]map[S]bool, len(all))
	for _, e := range sm.Edges() {
		if legal[e.From] == nil {
			legal[e.From] = make(map[S]bool, 4)
		}
		legal[e.From][e.To] = true
	}
	for _, from := range all {
		for _, to := range all {
			want := legal[from][to]
			if got := sm.CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
			err := sm.Transition(from, to)
			if want && err != nil {
				t.Errorf("Transition(%s, %s) rejected a legal edge: %v", from, to, err)
			}
			if !want && err == nil {
				t.Errorf("Transition(%s, %s) accepted an illegal edge", from, to)
			}
		}
	}
}

func TestInstanceMachineIsExhaustivelyCorrect(t *testing.T) {
	t.Parallel()
	exhaustive(t, InstanceMachine(), AllInstanceStates)
}

func TestStepMachineIsExhaustivelyCorrect(t *testing.T) {
	t.Parallel()
	exhaustive(t, StepMachine(), AllStepStates)
}

// TestIsFinalMatchesTheLiveBusinessKeyPredicate pins the set that the partial unique index
// `wfi_live_business_key` excludes. "One live onboarding per merchant" and "this instance is
// finished" must be the same question answered by the same list, or a merchant whose onboarding
// failed can never start another one — or, worse, can start two.
func TestIsFinalMatchesTheLiveBusinessKeyPredicate(t *testing.T) {
	t.Parallel()
	final := map[InstanceState]bool{
		InstanceCompleted:   true,
		InstanceFailed:      true,
		InstanceCompensated: true,
		InstanceCanceled:    true,
	}
	for _, s := range AllInstanceStates {
		if got := s.IsFinal(); got != final[s] {
			t.Errorf("%s.IsFinal() = %v, want %v", s, got, final[s])
		}
	}
}

// TestPoisonedIsNeverRunnable is the property that bounds a poison instance's blast radius: it
// must be invisible to every poller, or it cycles the whole fleet.
func TestPoisonedIsNeverRunnable(t *testing.T) {
	t.Parallel()
	if InstancePoisoned.IsRunnable() {
		t.Fatal("a quarantined instance is claimable; it would take down a rolling series of workers")
	}
	for _, s := range []InstanceState{InstancePending, InstanceRunning, InstanceRetryBackoff,
		InstanceCompensating, InstanceWaitingSignal, InstanceParked} {
		if !s.IsRunnable() {
			t.Errorf("%s must be claimable once RunAfter has elapsed", s)
		}
	}
	for _, s := range AllInstanceStates {
		if s.IsFinal() && s.IsRunnable() {
			t.Errorf("terminal state %s is claimable", s)
		}
	}
}

func TestOnlySucceededStepsNeedCompensation(t *testing.T) {
	t.Parallel()
	for _, s := range AllStepStates {
		want := s == StepSucceeded
		if got := s.NeedsCompensation(); got != want {
			t.Errorf("%s.NeedsCompensation() = %v, want %v — only a step that succeeded created anything to undo", s, got, want)
		}
		if got := s.IsComplete(); got != want {
			t.Errorf("%s.IsComplete() = %v, want %v", s, got, want)
		}
	}
}

func TestWaitingSignalHoldsNoLease(t *testing.T) {
	t.Parallel()
	if InstanceWaitingSignal.IsLeased() {
		t.Fatal("a signal wait must hold no lease: a seven-day KYC wait would otherwise leak a worker slot for a week")
	}
	if !InstanceRunning.IsLeased() || !InstanceCompensating.IsLeased() {
		t.Fatal("RUNNING and COMPENSATING are the two states in which a worker owns the instance")
	}
}
