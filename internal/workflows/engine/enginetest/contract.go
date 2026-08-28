package enginetest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// The contract suite lives in this package rather than beside either implementation, and it
// imports `testing` from a non-test file the way net/http/httptest does. The reasoning is that a
// contract owned by one implementation is not a contract: it drifts towards whatever that
// implementation happens to do, and the second implementation is then judged against the first
// one's accidents rather than against the specification. Both engines import it; neither owns it.

// Contract mode values, carried in the workflow input, that steer the fixture's activities.
const (
	// ModeHappy runs every step to completion, pausing at the manual gate.
	ModeHappy = "happy"
	// ModeAbort fails `sandbox-validation` with a business rejection, which must unwind the saga.
	ModeAbort = "abort"
	// ModeDLQ fails `sandbox-validation` with a technical error, which must reach the DLQ without
	// unwinding anything.
	ModeDLQ = "dlq"
)

// ContractInput is the fixture workflow's input document.
type ContractInput struct {
	MerchantID string `json:"merchantId"`
	Mode       string `json:"mode"`
}

// Contract step and signal names. They mirror the real onboarding definition's shape — a pure
// step, two compensatable side-effecting steps, a step that can fail, a manual gate and an
// irreversible pivot — without depending on the onboarding package, so that the contract stays a
// statement about the *engine* rather than about merchants.
const (
	StepValidate  = "validate"
	StepProvision = "provision"
	StepConfigure = "configure"
	StepSandbox   = "sandbox-validation"
	StepReview    = "compliance-review"
	StepActivate  = "activate"

	SignalApprove = "compliance-approval"
)

// journal records what each instance's activities did, keyed by workflow ID.
//
// A package-level journal keyed by instance is what lets EngineContractSuite keep the signature
// the port dictates — `newEngine func() engine.Engine` and nothing else — while still asserting
// on *sequence* properties like reverse-order compensation, which no amount of reading the
// Instance can recover once two compensations share a checkpointed timestamp.
var journal = struct {
	mu sync.Mutex
	m  map[shared.WorkflowID][]string
}{m: make(map[shared.WorkflowID][]string)}

func note(id shared.WorkflowID, entry string) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	journal.m[id] = append(journal.m[id], entry)
}

// Journal returns what the fixture's activities recorded for one instance, in order.
func Journal(id shared.WorkflowID) []string {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return append([]string(nil), journal.m[id]...)
}

// ContractDefinition is the workflow both implementations are judged on.
func ContractDefinition() *engine.Definition {
	retry := engine.RetryPolicy{MaxAttempts: 2, InitialInterval: 10 * time.Millisecond,
		MaxInterval: 50 * time.Millisecond, BackoffFactor: 2}
	return &engine.Definition{
		Name:        "engine-contract",
		Version:     1,
		Description: "the behavioural contract every engine.Engine implementation must satisfy",
		BusinessKeyOf: func(input []byte) (string, error) {
			var in ContractInput
			if err := json.Unmarshal(input, &in); err != nil {
				return "", apierror.Wrap(err, apierror.CodeMalformedRequest, "contract input does not decode")
			}
			return in.MerchantID, nil
		},
		Steps: []engine.Step{
			{Name: StepValidate, Activity: StepValidate, Timeout: time.Second,
				Idempotent: true, Retry: retry, Description: "pure; nothing to undo"},
			{Name: StepProvision, Activity: StepProvision, Compensation: "undo-" + StepProvision,
				Timeout: time.Second, Idempotent: true, SideEffecting: true, Retry: retry},
			{Name: StepConfigure, Activity: StepConfigure, Compensation: "undo-" + StepConfigure,
				Timeout: time.Second, Idempotent: true, SideEffecting: true, Retry: retry},
			{Name: StepSandbox, Activity: StepSandbox, Timeout: time.Second,
				Idempotent: true, SideEffecting: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
			{Name: StepReview, ManualGate: true, Signal: SignalApprove, Timeout: 24 * time.Hour},
			{Name: StepActivate, Activity: StepActivate, Compensation: "suspend",
				CompensationKind: engine.CompensationForward,
				Pivot:            true, PivotKind: engine.PivotIrreversible,
				Timeout: time.Second, Idempotent: true, Retry: retry},
		},
	}
}

// ContractActivities returns a fresh registry holding the fixture's activities.
//
// Behaviour is driven entirely by the *workflow input*, never by a captured variable, so the
// same fixture works against an engine that executes in-process and against one that executes on
// a remote worker with only the serialized input to go on.
func ContractActivities() *engine.Activities {
	acts := engine.NewActivities()
	simple := func(name string) engine.Activity {
		return engine.ActivityFunc{ActivityName: name, Fn: func(_ context.Context, in engine.Input) (engine.Output, error) {
			note(in.WorkflowID, name)
			out, err := json.Marshal(map[string]any{
				"step":    in.Step,
				"attempt": in.Attempt,
				"key":     in.IdempotencyKey,
				"ref":     name + "-ref",
			})
			if err != nil {
				return nil, err
			}
			return out, nil
		}}
	}
	acts.MustRegister(
		simple(StepValidate),
		simple(StepProvision),
		simple(StepConfigure),
		simple(StepActivate),
		simple("undo-"+StepProvision),
		simple("undo-"+StepConfigure),
		simple("suspend"),
		engine.ActivityFunc{ActivityName: StepSandbox, Fn: func(_ context.Context, in engine.Input) (engine.Output, error) {
			note(in.WorkflowID, StepSandbox)
			var input ContractInput
			_ = json.Unmarshal(in.Payload, &input)
			switch input.Mode {
			case ModeAbort:
				return nil, apierror.New(apierror.CodeValidationFailed,
					"refund round trip returned UNSUPPORTED for EUR/CARD")
			case ModeDLQ:
				return nil, apierror.New(apierror.CodeGatewayAuthenticationFailed,
					"our sandbox credentials were rejected")
			default:
				return engine.Output(`{"passed":true}`), nil
			}
		}},
	)
	return acts
}

// EngineContractSuite is the behavioural contract every engine.Engine implementation must pass.
//
// newEngine must return an engine already wired with ContractActivities(); the suite registers
// ContractDefinition() itself through Start. Each subtest gets a fresh engine, because a
// contract that shares state between cases tests the sharing rather than the behaviour.
func EngineContractSuite(t *testing.T, newEngine func() engine.Engine) {
	t.Helper()

	t.Run("start is idempotent on the business key", func(t *testing.T) {
		eng := newEngine()
		ctx := context.Background()
		def := ContractDefinition()

		first, err := eng.Start(ctx, def, "", inputFor("mrc_idem", ModeHappy))
		if err != nil {
			t.Fatalf("first Start: %v", err)
		}
		second, err := eng.Start(ctx, def, "", inputFor("mrc_idem", ModeHappy))
		if err != nil {
			t.Fatalf("second Start: %v", err)
		}
		if first != second {
			// One live onboarding per merchant is not a nicety for retrying clients; it is what
			// stops two sagas provisioning gateway sub-accounts for one merchant.
			t.Fatalf("starting twice with one business key produced %s and %s", first, second)
		}
	})

	t.Run("a completed step is not replayed on resume", func(t *testing.T) {
		eng := newEngine()
		id, _ := start(t, eng, "mrc_replay", ModeHappy)

		settle(t, eng, id)
		before := Journal(id)
		if len(before) == 0 {
			t.Fatal("no step ran at all")
		}

		// Resuming a workflow parked on its gate must do nothing at all.
		settle(t, eng, id)
		after := Journal(id)

		if strings.Join(before, ",") != strings.Join(after, ",") {
			t.Fatalf("resume replayed completed steps:\n before: %v\n  after: %v", before, after)
		}
		for _, step := range []string{StepValidate, StepProvision, StepConfigure, StepSandbox} {
			if n := count(after, step); n != 1 {
				t.Errorf("step %s executed %d times across two resumes, want 1", step, n)
			}
		}
	})

	t.Run("a manual gate blocks until signalled", func(t *testing.T) {
		eng := newEngine()
		ctx := context.Background()
		id, _ := start(t, eng, "mrc_gate", ModeHappy)
		settle(t, eng, id)

		if n := count(Journal(id), StepActivate); n != 0 {
			t.Fatalf("the step after the gate ran %d times before the gate was signalled", n)
		}
		inst, err := eng.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if inst.IsFinal() {
			t.Fatalf("the instance reached %s without the gate ever being signalled", inst.State)
		}

		if err := eng.Signal(ctx, id, SignalApprove, engine.SignalPayload{
			Data:      []byte(`{"decision":"APPROVE"}`),
			Principal: "usr_compliance_1",
			Scopes:    []string{"onboarding:approve"},
			Reason:    "reviewed",
		}); err != nil {
			t.Fatalf("Signal: %v", err)
		}
		settle(t, eng, id)

		if n := count(Journal(id), StepActivate); n != 1 {
			t.Fatalf("after the signal the final step ran %d times, want 1", n)
		}
		final, err := eng.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if final.State != engine.InstanceCompleted {
			t.Fatalf("expected COMPLETED after the signal, got %s", final.State)
		}
	})

	t.Run("compensations run in reverse order", func(t *testing.T) {
		eng := newEngine()
		ctx := context.Background()
		id, _ := start(t, eng, "mrc_undo", ModeAbort)
		settle(t, eng, id)

		var undo []string
		for _, entry := range Journal(id) {
			if strings.HasPrefix(entry, "undo-") {
				undo = append(undo, entry)
			}
		}
		want := []string{"undo-" + StepConfigure, "undo-" + StepProvision}
		if strings.Join(undo, ",") != strings.Join(want, ",") {
			t.Fatalf("compensations ran %v, want %v — a webhook registration must be deleted before the sub-account it belongs to", undo, want)
		}
		inst, err := eng.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !inst.IsFinal() {
			t.Fatalf("an aborted instance is still live in state %s", inst.State)
		}
	})

	t.Run("cancellation is honoured", func(t *testing.T) {
		eng := newEngine()
		ctx := context.Background()
		id, _ := start(t, eng, "mrc_cancel", ModeHappy)
		settle(t, eng, id) // parks on the gate

		if err := eng.Cancel(ctx, id, "the merchant withdrew the application"); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		settle(t, eng, id)

		inst, err := eng.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !inst.IsFinal() {
			t.Fatalf("a cancelled instance is still live in state %s", inst.State)
		}
		if n := count(Journal(id), StepActivate); n != 0 {
			t.Fatalf("a cancelled instance ran a step past the cancellation point %d times", n)
		}
		if n := count(Journal(id), "undo-"+StepProvision); n != 1 {
			t.Fatalf("cancellation ran the compensation %d times, want 1: cancel must not abandon side effects", n)
		}
	})

	t.Run("a failed non-retryable step reaches the DLQ", func(t *testing.T) {
		eng := newEngine()
		ctx := context.Background()
		id, _ := start(t, eng, "mrc_dlq", ModeDLQ)
		settle(t, eng, id)

		inst, err := eng.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if inst.State != engine.InstanceFailed {
			t.Fatalf("expected FAILED, got %s", inst.State)
		}
		// The DLQ is observable through the port as a step state, which is the only part of it
		// both implementations can be expected to expose: ours is a table, Temporal's is a
		// terminally failed execution plus a custom archival handler.
		found := false
		for _, s := range inst.Steps {
			if s.Name == StepSandbox && s.State == engine.StepDLQ {
				found = true
			}
		}
		if !found {
			t.Fatalf("the failing step is not parked in the DLQ; steps = %+v", inst.Steps)
		}
		// A technical failure must not unwind the saga: compensating away the merchant's
		// provisioned state because *our* credentials were wrong destroys work a redeploy fixes.
		if n := count(Journal(id), "undo-"+StepProvision); n != 0 {
			t.Fatalf("a technical failure ran %d compensations; it must go to the DLQ for a fix-forward", n)
		}
	})
}

func inputFor(merchantID, mode string) []byte {
	b, _ := json.Marshal(ContractInput{MerchantID: merchantID, Mode: mode})
	return b
}

func start(t *testing.T, eng engine.Engine, merchantID, mode string) (shared.WorkflowID, *engine.Definition) {
	t.Helper()
	def := ContractDefinition()
	id, err := eng.Start(context.Background(), def, "", inputFor(merchantID, mode))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return id, def
}

// settle drives an instance until it stops moving.
//
// It is written against the port alone, so it works for an engine that advances on Resume and
// for one whose service advances it independently: Resume is called, a not-resumable answer is
// treated as "nothing to do", and the loop exits when two consecutive observations agree. The
// iteration bound is what stops a broken implementation from hanging the suite instead of
// failing it.
func settle(t *testing.T, eng engine.Engine, id shared.WorkflowID) {
	t.Helper()
	ctx := context.Background()
	prev := ""
	for i := 0; i < 64; i++ {
		if err := eng.Resume(ctx, id); err != nil {
			if !errors.Is(err, engine.ErrNotRunnable) {
				t.Fatalf("Resume: %v", err)
			}
		}
		inst, err := eng.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		fingerprint := string(inst.State) + "/" + inst.CurrentStep + "/" +
			strings.Join(inst.CompletedSteps(), ",")
		if fingerprint == prev {
			return
		}
		prev = fingerprint
	}
	t.Fatalf("instance %s never settled", id)
}

func count(entries []string, want string) int {
	n := 0
	for _, e := range entries {
		if e == want {
			n++
		}
	}
	return n
}
