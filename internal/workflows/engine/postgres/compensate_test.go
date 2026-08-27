package postgres_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
)

// compensatingDefinition builds four compensatable steps followed by one that fails, optionally
// with a pivot at the given index.
func compensatingDefinition(t *testing.T, h *harness, pivotAt int, kind engine.PivotKind) *engine.Definition {
	t.Helper()
	names := []string{"provision", "credentials", "webhooks", "configure"}
	steps := make([]engine.Step, 0, len(names)+1)
	for i, n := range names {
		h.ok(n)
		h.ok("undo-" + n)
		s := engine.Step{
			Name: n, Activity: n, Compensation: "undo-" + n,
			Timeout: time.Second, Idempotent: true, SideEffecting: true,
			Retry: engine.RetryPolicy{MaxAttempts: 1},
		}
		if i == pivotAt {
			s.Pivot = true
			s.PivotKind = kind
			if kind == engine.PivotIrreversible {
				s.CompensationKind = engine.CompensationForward
			}
		}
		steps = append(steps, s)
	}
	h.scripted("sandbox", func(_ context.Context, _ engine.Input, _ int) (engine.Output, error) {
		return nil, business("refund round trip returned UNSUPPORTED for EUR/CARD on adyen")
	})
	steps = append(steps, engine.Step{
		Name: "sandbox", Activity: "sandbox", Timeout: time.Second, Idempotent: true,
		Retry: engine.RetryPolicy{MaxAttempts: 1},
	})
	return &engine.Definition{Name: "saga", Version: 1, Steps: steps}
}

func TestCompensationsRunInStrictReverseOrder(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	def := compensatingDefinition(t, h, -1, engine.PivotNone)
	id := mustStart(t, h, def, "mrc_1", nil)

	h.resume(id)

	inst := h.instance(id)
	if inst.State != engine.InstanceFailed {
		t.Fatalf("expected FAILED after a business rejection, got %s", inst.State)
	}

	// Reverse order is a *sequence* property: a webhook registration must be deleted before the
	// sub-account it belongs to is de-provisioned, and a set of counts cannot express that.
	var undo []string
	for _, name := range h.runs.Order() {
		if strings.HasPrefix(name, "undo-") {
			undo = append(undo, name)
		}
	}
	want := []string{"undo-configure", "undo-webhooks", "undo-credentials", "undo-provision"}
	if len(undo) != len(want) {
		t.Fatalf("ran %d compensations %v, want %d %v", len(undo), undo, len(want), want)
	}
	for i := range want {
		if undo[i] != want[i] {
			t.Fatalf("compensation order = %v, want %v", undo, want)
		}
	}

	states := h.repo.StepStates(id)
	for _, n := range []string{"provision", "credentials", "webhooks", "configure"} {
		if states[n] != string(engine.StepCompensated) {
			t.Errorf("step %s state = %q, want COMPENSATED", n, states[n])
		}
	}
	if len(h.repo.DLQ()) == 0 {
		t.Fatal("an aborted instance must leave a DLQ entry carrying the failure and the compensation outcomes")
	}
}

// TestCompensationStopsAtACompletedRetainedPivot covers dimension-1 irreversibility.
//
// Once the KYC decision has landed, the vendor's record is retained for five years under a
// legal-obligation basis and there is nothing to undo. Compensating the steps at or before it
// would be a no-op at best and an audit inconsistency at worst — the trail would show us
// "cancelling" a case that had already been decided.
func TestCompensationStopsAtACompletedRetainedPivot(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	def := compensatingDefinition(t, h, 1, engine.PivotRetained) // "credentials" stands in for the KYC decision
	id := mustStart(t, h, def, "mrc_1", nil)

	h.resume(id)

	var undo []string
	for _, name := range h.runs.Order() {
		if strings.HasPrefix(name, "undo-") {
			undo = append(undo, name)
		}
	}
	want := []string{"undo-configure", "undo-webhooks"}
	if strings.Join(undo, ",") != strings.Join(want, ",") {
		t.Fatalf("compensations = %v, want %v — steps at or before a completed retained pivot must be skipped", undo, want)
	}
}

// TestAbortIsRefusedPastAnIrreversiblePivot covers dimension-2 irreversibility.
//
// Once the merchant is ACTIVE, real payments can exist and each has its own lifecycle. Rolling
// back would not undo anything; it would strand money mid-flight. The engine parks the instance
// and asks for a human instead.
func TestAbortIsRefusedPastAnIrreversiblePivot(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ok("provision")
	h.ok("undo-provision")
	h.ok("activate")
	h.ok("suspend")
	h.scripted("post", func(_ context.Context, _ engine.Input, _ int) (engine.Output, error) {
		return nil, business("a reconciliation exception opened after activation")
	})
	def := &engine.Definition{Name: "pivoted", Version: 1, Steps: []engine.Step{
		{Name: "provision", Activity: "provision", Compensation: "undo-provision",
			Timeout: time.Second, Idempotent: true, SideEffecting: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
		{Name: "activate", Activity: "activate", Compensation: "suspend",
			CompensationKind: engine.CompensationForward, Pivot: true, PivotKind: engine.PivotIrreversible,
			Timeout: time.Second, Idempotent: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
		{Name: "post", Activity: "post", Timeout: time.Second, Idempotent: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
	}}
	id := mustStart(t, h, def, "mrc_1", nil)

	h.resume(id)

	inst := h.instance(id)
	if inst.State != engine.InstanceParked {
		t.Fatalf("expected PARKED past the irreversible pivot, got %s", inst.State)
	}
	if got := h.runs.Runs("undo-provision"); got != 0 {
		t.Fatalf("a rollback ran %d times past the money pivot; recovery there is roll-forward only", got)
	}
	if got := h.runs.Runs("suspend"); got != 0 {
		t.Fatalf("the engine ran the pivot's forward recovery itself (%d times); that is an operator decision", got)
	}
	dlq := h.repo.DLQ()
	if len(dlq) == 0 {
		t.Fatal("a refusal to roll back must reach the DLQ so a human sees it")
	}
	if !strings.Contains(dlq[len(dlq)-1].Reason, "refused to roll back past the irreversible pivot") {
		t.Fatalf("the DLQ entry does not explain the refusal: %s", dlq[len(dlq)-1].Reason)
	}
}

func TestAFailedCompensationDoesNotStopTheRest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ok("provision")
	h.ok("undo-provision")
	h.ok("webhooks")
	h.scripted("undo-webhooks", func(_ context.Context, _ engine.Input, _ int) (engine.Output, error) {
		return nil, transient("the gateway is down and cannot delete the registration")
	})
	h.scripted("fail", func(_ context.Context, _ engine.Input, _ int) (engine.Output, error) {
		return nil, business("assertion failed")
	})
	def := &engine.Definition{Name: "partial-undo", Version: 1, Steps: []engine.Step{
		{Name: "provision", Activity: "provision", Compensation: "undo-provision",
			Timeout: time.Second, Idempotent: true, SideEffecting: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
		{Name: "webhooks", Activity: "webhooks", Compensation: "undo-webhooks",
			Timeout: time.Second, Idempotent: true, SideEffecting: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
		{Name: "fail", Activity: "fail", Timeout: time.Second, Idempotent: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
	}}
	id := mustStart(t, h, def, "mrc_1", nil)

	h.resume(id)

	// Skipping the remaining compensations after one fails would orphan more state, not less.
	if got := h.runs.Runs("undo-provision"); got != 1 {
		t.Fatalf("undo-provision ran %d times; a failed compensation must not stop the walk", got)
	}
	states := h.repo.StepStates(id)
	if states["webhooks"] != string(engine.StepCompensationFailed) {
		t.Fatalf("webhooks compensation state = %q, want COMPENSATION_FAILED", states["webhooks"])
	}
	if h.instance(id).State != engine.InstanceFailed {
		t.Fatalf("an orphaned resource must leave the instance FAILED, got %s", h.instance(id).State)
	}
	found := false
	for _, entry := range h.repo.DLQ() {
		if strings.Contains(entry.Reason, "COMPENSATION_FAILED") {
			found = true
		}
	}
	if !found {
		t.Fatal("a failed compensation must produce a DLQ entry naming it: real external state is now orphaned")
	}
}

func TestCompensationRetriesBeforeGivingUp(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ok("provision")
	h.scripted("undo-provision", func(_ context.Context, _ engine.Input, attempt int) (engine.Output, error) {
		if attempt < 3 {
			return nil, transient("gateway 503")
		}
		return engine.Output(`{"deprovisioned":true}`), nil
	})
	h.scripted("fail", func(_ context.Context, _ engine.Input, _ int) (engine.Output, error) {
		return nil, business("no")
	})
	def := &engine.Definition{Name: "retrying-undo", Version: 1, Steps: []engine.Step{
		{Name: "provision", Activity: "provision", Compensation: "undo-provision",
			Timeout: time.Second, Idempotent: true, SideEffecting: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
		{Name: "fail", Activity: "fail", Timeout: time.Second, Idempotent: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
	}}
	id := mustStart(t, h, def, "mrc_1", nil)

	h.resume(id)

	if got := h.runs.Runs("undo-provision"); got != 3 {
		t.Fatalf("the compensation ran %d times, want 3 (two failures then success)", got)
	}
	if got := h.repo.StepStates(id)["provision"]; got != string(engine.StepCompensated) {
		t.Fatalf("provision state = %q, want COMPENSATED", got)
	}
}

func TestCompensationReceivesTheStepsOutputNotItsInput(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.scripted("provision", func(_ context.Context, _ engine.Input, _ int) (engine.Output, error) {
		return engine.Output(`{"externalAccountId":"acct_live_42"}`), nil
	})
	var seen string
	h.scripted("undo-provision", func(_ context.Context, in engine.Input, _ int) (engine.Output, error) {
		var payload struct {
			ExternalAccountID string `json:"externalAccountId"`
		}
		if err := json.Unmarshal(in.Payload, &payload); err != nil {
			return nil, err
		}
		seen = payload.ExternalAccountID
		return nil, nil
	})
	h.scripted("fail", func(_ context.Context, _ engine.Input, _ int) (engine.Output, error) {
		return nil, business("no")
	})
	def := &engine.Definition{Name: "output-fed", Version: 1, Steps: []engine.Step{
		{Name: "provision", Activity: "provision", Compensation: "undo-provision",
			Timeout: time.Second, Idempotent: true, SideEffecting: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
		{Name: "fail", Activity: "fail", Timeout: time.Second, Idempotent: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
	}}
	id := mustStart(t, h, def, "mrc_1", nil)

	h.resume(id)

	// Undoing a provisioning step needs the external reference the step *produced*. A
	// compensation that only saw the input would have to re-discover it, which is fragile in the
	// ordinary case and impossible after a crash.
	if seen != "acct_live_42" {
		t.Fatalf("the compensation received %q; it must receive the step's checkpointed output", seen)
	}
}
