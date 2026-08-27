package postgres_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
)

func TestStartIsIdempotentOnBusinessKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	def := linearDefinition(t, h, "a", "b")

	first := mustStart(t, h, def, "mrc_1", map[string]string{"merchantId": "mrc_1"})
	second := mustStart(t, h, def, "mrc_1", map[string]string{"merchantId": "mrc_1"})

	if first != second {
		t.Fatalf("starting twice with one business key produced two instances: %s and %s", first, second)
	}
	if got := h.repo.Calls("CreateInstance"); got != 1 {
		t.Fatalf("expected exactly one insert, got %d", got)
	}

	// A different merchant is a different saga.
	other := mustStart(t, h, def, "mrc_2", map[string]string{"merchantId": "mrc_2"})
	if other == first {
		t.Fatal("two merchants shared one workflow instance")
	}
}

func TestStartAfterTerminalInstanceCreatesANewOne(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	def := linearDefinition(t, h, "a")

	first := mustStart(t, h, def, "mrc_1", nil)
	h.resume(first)
	if got := h.instance(first).State; got != engine.InstanceCompleted {
		t.Fatalf("expected COMPLETED, got %s", got)
	}

	// A terminal instance must not block a fresh attempt: a merchant whose onboarding failed
	// gets a new, separately auditable instance rather than a resurrection of the old one.
	second := mustStart(t, h, def, "mrc_1", nil)
	if second == first {
		t.Fatal("a completed instance blocked a new start for the same business key")
	}
}

func TestDriveRunsEveryStepOnceAndCompletes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	def := linearDefinition(t, h, "a", "b", "c")
	id := mustStart(t, h, def, "mrc_1", nil)

	h.resume(id)

	inst := h.instance(id)
	if inst.State != engine.InstanceCompleted {
		t.Fatalf("expected COMPLETED, got %s (last error %q)", inst.State, inst.LastError)
	}
	for _, s := range []string{"a", "b", "c"} {
		if got := h.runs.Runs(s); got != 1 {
			t.Errorf("step %s executed %d times, want 1", s, got)
		}
	}
	if got := len(inst.CompletedSteps()); got != 3 {
		t.Fatalf("expected 3 checkpointed steps, got %d", got)
	}
	if got := h.metrics.Outcome("c"); got != engine.OutcomeSuccess {
		t.Fatalf("step c metric outcome = %q, want success", got)
	}
}

// TestCrashAndResumeAtEveryStep is the replay-freedom property, exercised at every crash point.
//
// The twelve-step workflow is stopped after each step in turn — simulating a worker that died
// between one step's checkpoint and the next step's first call — and then resumed. The assertion
// is not "it finished", which a replaying engine would also satisfy: it is that **no completed
// step ever runs a second time**. A replaying engine would re-execute steps 1..n here, and for
// the real definition that means a second KYC submission and a second gateway sub-account.
func TestCrashAndResumeAtEveryStep(t *testing.T) {
	t.Parallel()
	names := []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8", "s9", "s10", "s11", "s12"}

	for crashAfter := 1; crashAfter <= len(names); crashAfter++ {

		t.Run("crash-after-"+names[crashAfter-1], func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)

			// Steps beyond the crash point refuse to run until the test lets them, which stages
			// the crash deterministically instead of with a sleep and a hope.
			var mu sync.Mutex
			stopAfter := crashAfter
			steps := make([]engine.Step, 0, len(names))
			for i, n := range names {
				h.scripted(n, func(_ context.Context, in engine.Input, _ int) (engine.Output, error) {
					mu.Lock()
					limit := stopAfter
					mu.Unlock()
					if i >= limit {
						return nil, transient("the worker died before " + n)
					}
					out, _ := json.Marshal(map[string]any{"step": in.Step, "key": in.IdempotencyKey})
					return out, nil
				})
				steps = append(steps, engine.Step{
					Name: n, Activity: n, Timeout: 5 * time.Second, Idempotent: true,
					Retry: engine.RetryPolicy{MaxAttempts: 20, InitialInterval: time.Second,
						MaxInterval: 10 * time.Second, BackoffFactor: 2},
				})
			}
			def := &engine.Definition{Name: "twelve", Version: 1, Steps: steps}
			id := mustStart(t, h, def, "mrc_1", nil)

			h.resume(id)
			for _, n := range names[:crashAfter] {
				if got := h.runs.Runs(n); got != 1 {
					t.Fatalf("before the crash: step %s ran %d times, want 1", n, got)
				}
			}
			if crashAfter == len(names) {
				if got := h.instance(id).State; got != engine.InstanceCompleted {
					t.Fatalf("expected COMPLETED, got %s", got)
				}
				return
			}
			if got := h.instance(id).State; got != engine.InstanceRetryBackoff {
				t.Fatalf("expected RETRY_BACKOFF at the crash step, got %s", got)
			}

			// "The fleet came back": every step is willing to run again.
			h.runs.Reset()
			mu.Lock()
			stopAfter = len(names)
			mu.Unlock()
			h.clock.Advance(time.Minute)
			h.resume(id)

			inst := h.instance(id)
			if inst.State != engine.InstanceCompleted {
				t.Fatalf("expected COMPLETED after resume, got %s (%s)", inst.State, inst.LastError)
			}
			for _, n := range names[:crashAfter] {
				if got := h.runs.Runs(n); got != 0 {
					t.Errorf("completed step %s re-executed %d times after resume", n, got)
				}
			}
			for _, n := range names[crashAfter:] {
				if got := h.runs.Runs(n); got != 1 {
					t.Errorf("step %s ran %d times after resume, want 1", n, got)
				}
			}
		})
	}
}

// TestResumeAfterACrashBetweenTheStepWriteAndTheInstanceWrite covers the narrower window that
// the "read the step record, not current_step" rule exists for.
//
// The step's output is checkpointed but the instance still points at it — exactly what a crash
// between two rows leaves behind when the store cannot commit them together. Resuming must skip
// the step, not re-run it.
func TestResumeAfterACrashBetweenTheStepWriteAndTheInstanceWrite(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	def := linearDefinition(t, h, "a", "b")
	id := mustStart(t, h, def, "mrc_1", nil)
	h.resume(id)

	// Rewind the instance to before its advance, leaving the SUCCEEDED step row in place.
	rec := h.repo.Instance(id)
	if rec == nil {
		t.Fatal("instance vanished")
	}
	rec.State = string(engine.InstancePending)
	rec.CurrentStep = "a"
	rec.CompletedAt = nil
	rec.RunAfter = h.clock.Now()
	if err := h.repo.SaveInstance(context.Background(), *rec); err != nil {
		t.Fatalf("rewinding the instance: %v", err)
	}
	h.runs.Reset()

	h.resume(id)

	if got := h.runs.Runs("a"); got != 0 {
		t.Fatalf("step a re-executed %d times despite a checkpointed output", got)
	}
	if got := h.instance(id).State; got != engine.InstanceCompleted {
		t.Fatalf("expected COMPLETED, got %s", got)
	}
}

func TestRetryExhaustionReachesTheDLQWithTheErrorChain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ok("first")
	h.scripted("flaky", func(_ context.Context, _ engine.Input, attempt int) (engine.Output, error) {
		return nil, transient("vendor is down (attempt " + string(rune('0'+attempt)) + ")")
	})
	def := &engine.Definition{Name: "retrying", Version: 1, Steps: []engine.Step{
		{Name: "first", Activity: "first", Timeout: time.Second, Idempotent: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
		{Name: "flaky", Activity: "flaky", Timeout: time.Second, Idempotent: true, SideEffecting: true,
			Retry: engine.RetryPolicy{MaxAttempts: 3, InitialInterval: time.Second, MaxInterval: 4 * time.Second, BackoffFactor: 2}},
	}}
	id := mustStart(t, h, def, "mrc_1", nil)

	// Each resume runs one attempt and schedules the next as a timestamp — the backoff lives in
	// a column, so advancing the clock is all it takes to become runnable again.
	for i := 0; i < 3; i++ {
		h.resume(id)
		h.clock.Advance(10 * time.Second)
	}

	if got := h.runs.Runs("flaky"); got != 3 {
		t.Fatalf("flaky ran %d times, want exactly MaxAttempts=3", got)
	}
	inst := h.instance(id)
	if inst.State != engine.InstanceFailed {
		t.Fatalf("expected FAILED after exhaustion, got %s", inst.State)
	}
	dlq := h.repo.DLQ()
	if len(dlq) == 0 {
		t.Fatal("retry exhaustion did not produce a DLQ entry")
	}
	var payload struct {
		Step  string              `json:"step"`
		Class string              `json:"class"`
		Chain []engine.ChainEntry `json:"chain"`
	}
	if err := json.Unmarshal([]byte(dlq[len(dlq)-1].Reason), &payload); err != nil {
		t.Fatalf("the DLQ reason is not the structured failure record: %v", err)
	}
	if payload.Step != "flaky" {
		t.Errorf("DLQ step = %q, want flaky", payload.Step)
	}
	if len(payload.Chain) == 0 {
		t.Fatal("the DLQ entry carries no error chain; triage's first question is unanswerable")
	}
	if !strings.Contains(payload.Chain[0].Message, "vendor is down") {
		t.Errorf("the chain's outermost link lost the original message: %q", payload.Chain[0].Message)
	}
	if payload.Chain[0].Code != string("SERVICE_UNAVAILABLE") {
		t.Errorf("the chain lost the error code: %q", payload.Chain[0].Code)
	}
}

func TestPerStepTimeoutFiresAndIsClassifiedAsAmbiguousForSideEffectingSteps(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.scripted("slow", func(ctx context.Context, _ engine.Input, _ int) (engine.Output, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	def := &engine.Definition{Name: "timing-out", Version: 1, Steps: []engine.Step{
		{Name: "slow", Activity: "slow", Timeout: 20 * time.Millisecond, Idempotent: true,
			SideEffecting: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
	}}
	id := mustStart(t, h, def, "mrc_1", nil)

	h.resume(id)

	if got := h.metrics.Outcome("slow"); got != engine.OutcomeTimeout {
		t.Fatalf("timeout metric outcome = %q, want %q", got, engine.OutcomeTimeout)
	}
	inst := h.instance(id)
	// A timeout on a step that may have acted externally is ambiguous, never transient: the
	// instance is parked for a lookup-before-act probe rather than blindly retried.
	if inst.State != engine.InstanceParked {
		t.Fatalf("expected PARKED after an ambiguous timeout, got %s", inst.State)
	}
	states := h.repo.StepStates(id)
	if states["slow"] != string(engine.StepAmbiguous) {
		t.Fatalf("step state = %q, want AMBIGUOUS", states["slow"])
	}
	if len(h.repo.DLQ()) != 1 {
		t.Fatalf("an unresolvable ambiguity must reach the DLQ for an operator probe; got %d entries", len(h.repo.DLQ()))
	}
}

func TestPerStepTimeoutOnAPureStepRetries(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.scripted("pure", func(ctx context.Context, in engine.Input, attempt int) (engine.Output, error) {
		if attempt == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return engine.Output(`{"ok":true}`), nil
	})
	def := &engine.Definition{Name: "pure-timeout", Version: 1, Steps: []engine.Step{
		{Name: "pure", Activity: "pure", Timeout: 20 * time.Millisecond, Idempotent: true,
			Retry: engine.RetryPolicy{MaxAttempts: 2, InitialInterval: time.Second, MaxInterval: time.Second, BackoffFactor: 2}},
	}}
	id := mustStart(t, h, def, "mrc_1", nil)

	h.resume(id)
	if got := h.instance(id).State; got != engine.InstanceRetryBackoff {
		t.Fatalf("a timeout with no external side effect should retry; state = %s", got)
	}
	h.clock.Advance(5 * time.Second)
	h.resume(id)

	if got := h.instance(id).State; got != engine.InstanceCompleted {
		t.Fatalf("expected COMPLETED on the second attempt, got %s", got)
	}
}

func TestAttemptCountSurvivesACrash(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.scripted("flaky", func(_ context.Context, _ engine.Input, _ int) (engine.Output, error) {
		return nil, transient("still down")
	})
	def := &engine.Definition{Name: "persisted-attempts", Version: 1, Steps: []engine.Step{
		{Name: "flaky", Activity: "flaky", Timeout: time.Second, Idempotent: true,
			Retry: engine.RetryPolicy{MaxAttempts: 5, InitialInterval: time.Second, MaxInterval: 2 * time.Second, BackoffFactor: 2}},
	}}
	id := mustStart(t, h, def, "mrc_1", nil)

	h.resume(id)
	if got := h.instance(id).Attempt; got != 1 {
		t.Fatalf("attempt after one failure = %d, want 1", got)
	}
	h.clock.Advance(time.Minute)
	h.resume(id)
	// A crash between attempts must not hand the vendor a fresh retry budget.
	if got := h.instance(id).Attempt; got != 2 {
		t.Fatalf("attempt after a second failure = %d, want 2 — the count reset across the resume", got)
	}
}

func TestFencingRejectsAStaleEpochWrite(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// The activity pauses long enough for "another worker" to take the instance over, which is
	// what a 90-second GC pause looks like from the outside.
	takenOver := make(chan struct{})
	h.scripted("slow", func(_ context.Context, in engine.Input, _ int) (engine.Output, error) {
		h.repo.ForceEpoch(in.WorkflowID, "worker-b/pid9/boot9")
		close(takenOver)
		return engine.Output(`{"done":true}`), nil
	})
	def := &engine.Definition{Name: "fenced", Version: 1, Steps: []engine.Step{
		{Name: "slow", Activity: "slow", Timeout: time.Second, Idempotent: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
		{Name: "next", Activity: "next", Timeout: time.Second, Idempotent: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
	}}
	h.ok("next")
	id := mustStart(t, h, def, "mrc_1", nil)

	h.resume(id)
	<-takenOver

	// The zombie's checkpoint matched zero rows, so it never advanced the instance and never ran
	// the following step. Nothing was corrupted; the work is simply redone by the new owner.
	if got := h.runs.Runs("next"); got != 0 {
		t.Fatalf("a fenced-out worker advanced the instance and ran the next step %d times", got)
	}
	inst := h.instance(id)
	if inst.LeaseOwner != "worker-b/pid9/boot9" {
		t.Fatalf("lease owner = %q, want the new owner", inst.LeaseOwner)
	}
	if inst.CurrentStep != "slow" {
		t.Fatalf("current step = %q; the fenced write must not have advanced it", inst.CurrentStep)
	}
	if len(inst.CompletedSteps()) != 0 {
		t.Fatalf("the fenced write checkpointed a step: %v", inst.CompletedSteps())
	}
}

func TestFencedHeartbeatCancelsTheActivity(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	def := &engine.Definition{Name: "hb", Version: 1, Steps: []engine.Step{
		{Name: "long", Activity: "long", Timeout: 2 * time.Second, Idempotent: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
	}}
	h.scripted("long", func(ctx context.Context, in engine.Input, _ int) (engine.Output, error) {
		h.repo.ForceEpoch(in.WorkflowID, "worker-b")
		// The activity's own heartbeat is the fastest way it learns it has been fenced out; it
		// must abandon its work immediately rather than finish and try to commit.
		if err := in.Heartbeat(ctx, nil); err != nil {
			return nil, err
		}
		return engine.Output(`{"unreachable":true}`), nil
	})
	id := mustStart(t, h, def, "mrc_1", nil)

	h.resume(id)

	inst := h.instance(id)
	if len(inst.CompletedSteps()) != 0 {
		t.Fatalf("a fenced-out activity committed a step: %v", inst.CompletedSteps())
	}
	if got := h.repo.StepStates(id)["long"]; got != string(engine.StepLeaseLost) {
		t.Fatalf("step state = %q, want LEASE_LOST", got)
	}
}
