package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

func soundDefinition() *Definition {
	return &Definition{
		Name:    "sound",
		Version: 1,
		Steps: []Step{
			{Name: "one", Activity: "one", Timeout: time.Second, Idempotent: true,
				Retry: RetryPolicy{MaxAttempts: 3, InitialInterval: time.Second, MaxInterval: 10 * time.Second, BackoffFactor: 2}},
			{Name: "gate", ManualGate: true, Signal: "approve", Timeout: 24 * time.Hour},
			{Name: "pivot", Activity: "pivot", Timeout: time.Second, Idempotent: true,
				Pivot: true, PivotKind: PivotIrreversible,
				Compensation: "suspend", CompensationKind: CompensationForward,
				Retry: RetryPolicy{MaxAttempts: 3}},
		},
	}
}

func TestValidateAcceptsASoundDefinition(t *testing.T) {
	t.Parallel()
	if err := soundDefinition().Validate(); err != nil {
		t.Fatalf("a sound definition was rejected: %v", err)
	}
}

// TestValidateRejectsEachUnsoundness is the table of defects that are invisible in a review of
// the definition and catastrophic at three in the morning. Each fails the *process at startup*,
// never an instance at runtime — a definition defect discovered by a live merchant's onboarding
// is a defect discovered after the side effects have already happened.
func TestValidateRejectsEachUnsoundness(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		mutate   func(*Definition)
		wantCode string
		why      string
	}{
		{
			name: "rollback compensation after an irreversible pivot",
			mutate: func(d *Definition) {
				d.Steps = append(d.Steps, Step{
					Name: "after", Activity: "after", Timeout: time.Second, Idempotent: true,
					Compensation: "undo-after", Retry: RetryPolicy{MaxAttempts: 1},
				})
			},
			wantCode: "COMPENSATION_AFTER_PIVOT",
			why:      "past the money pivot the world cannot be restored; only forward recovery is legal there",
		},
		{
			name: "manual gate with no timeout",
			mutate: func(d *Definition) {
				d.Steps[1].Timeout = 0
			},
			wantCode: "MANUAL_GATE_WITHOUT_TIMEOUT",
			why:      "a gate that waits forever raises no failure alert and is excluded from the stuck sweep by design",
		},
		{
			name: "non-idempotent step with retries enabled",
			mutate: func(d *Definition) {
				d.Steps[0].Idempotent = false
			},
			wantCode: "RETRY_WITHOUT_IDEMPOTENCE",
			why:      "the retry is the duplicate side effect; no amount of backoff makes a second non-deduplicated create safe",
		},
		{
			name: "unreachable step",
			mutate: func(d *Definition) {
				d.Steps = append(d.Steps, Step{
					Name: "one", Activity: "one", Timeout: time.Second, Idempotent: true,
					Retry: RetryPolicy{MaxAttempts: 1},
				})
			},
			wantCode: "UNREACHABLE_STEP",
			why:      "the engine resolves current_step to the first match, so the later step is a control that looks present and never runs",
		},
	}

	for _, tc := range cases {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := soundDefinition()
			tc.mutate(d)

			err := d.Validate()
			if err == nil {
				t.Fatalf("Validate accepted an unsound definition (%s)", tc.why)
			}
			if !errors.Is(err, ErrDefinitionInvalid) {
				t.Fatalf("error does not wrap ErrDefinitionInvalid: %v", err)
			}
			var ae *apierror.Error
			if !errors.As(err, &ae) {
				t.Fatalf("error is not an *apierror.Error: %v", err)
			}
			found := false
			for _, d := range ae.Details {
				if d.Code == tc.wantCode {
					found = true
				}
			}
			if !found {
				t.Fatalf("no detail carried code %s; got %+v", tc.wantCode, ae.Details)
			}
		})
	}
}

func TestValidateAllowsForwardRecoveryAfterAPivot(t *testing.T) {
	t.Parallel()
	d := soundDefinition()
	d.Steps = append(d.Steps, Step{
		Name: "after", Activity: "after", Timeout: time.Second, Idempotent: true,
		Compensation: "notify", CompensationKind: CompensationForward,
		Retry: RetryPolicy{MaxAttempts: 1},
	})
	if err := d.Validate(); err != nil {
		t.Fatalf("a forward recovery after the pivot was rejected: %v", err)
	}
}

func TestValidateRequiresATimeoutOnEveryActivityStep(t *testing.T) {
	t.Parallel()
	d := soundDefinition()
	d.Steps[0].Timeout = 0
	if err := d.Validate(); err == nil {
		t.Fatal("a step with no per-attempt timeout was accepted; a wedged activity would hold a slot until the process died")
	}
}

func TestPivotIndexFindsTheLastPivotOfEachKind(t *testing.T) {
	t.Parallel()
	d := &Definition{Name: "two-pivots", Version: 1, Steps: []Step{
		{Name: "a", Activity: "a", Timeout: time.Second},
		{Name: "kyc", Activity: "kyc", Timeout: time.Second, Pivot: true, PivotKind: PivotRetained},
		{Name: "b", Activity: "b", Timeout: time.Second},
		{Name: "activate", Activity: "activate", Timeout: time.Second, Pivot: true, PivotKind: PivotIrreversible},
	}}
	if got := d.PivotIndex(PivotRetained); got != 1 {
		t.Errorf("retained pivot index = %d, want 1", got)
	}
	if got := d.PivotIndex(PivotIrreversible); got != 3 {
		t.Errorf("irreversible pivot index = %d, want 3", got)
	}
	if got := d.PivotIndex(PivotNone); got != -1 {
		t.Errorf("PivotNone index = %d, want -1", got)
	}
}

func TestRegistryRejectsARedefinitionUnderTheSameKey(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Register(soundDefinition()); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	// Re-registering the same shape is a no-op: Start auto-registers, and a process that also
	// registers at boot must not fail on the second call.
	if err := r.Register(soundDefinition()); err != nil {
		t.Fatalf("re-registering an identical definition: %v", err)
	}
	changed := soundDefinition()
	changed.Steps[0].Activity = "something-else"
	err := r.Register(changed)
	if err == nil {
		t.Fatal("a different definition was silently accepted under the same key; that is a version bump somebody forgot")
	}
	if !strings.Contains(err.Error(), "bump the version") {
		t.Errorf("the error does not say what to do: %v", err)
	}
}

func TestRegistryLookupByNameAndVersion(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Register(soundDefinition()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := r.Lookup("sound", 1); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if _, err := r.Lookup("sound", 2); err == nil {
		t.Fatal("an unregistered version resolved; a worker must not run an instance on a definition this binary does not contain")
	}
	if got := r.Keys(); len(got) != 1 || got[0] != "sound@v1" {
		t.Fatalf("Keys() = %v", got)
	}
}

func TestRetryPolicyPermits(t *testing.T) {
	t.Parallel()
	p := RetryPolicy{MaxAttempts: 3, NonRetryable: []FailureClass{ClassAmbiguous}}
	if p.Permits(ClassTerminalBusiness) {
		t.Error("a terminal business failure must never be retried")
	}
	if p.Permits(ClassTerminalTechnical) {
		t.Error("a terminal technical failure must never be retried")
	}
	if p.Permits(ClassAmbiguous) {
		t.Error("a class listed as non-retryable was permitted")
	}
	if !p.Permits(ClassTransient) {
		t.Error("a transient failure must be retryable")
	}
}
