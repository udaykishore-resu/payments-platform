package postgres_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
)

func gatedDefinition(t *testing.T, h *harness) *engine.Definition {
	t.Helper()
	h.ok("before")
	h.ok("after")
	return &engine.Definition{Name: "gated", Version: 1, Steps: []engine.Step{
		{Name: "before", Activity: "before", Timeout: time.Second, Idempotent: true,
			Retry: engine.RetryPolicy{MaxAttempts: 1}},
		{Name: "compliance-review", ManualGate: true, Signal: "compliance-approval",
			Timeout: 5 * 24 * time.Hour},
		{Name: "after", Activity: "after", Timeout: time.Second, Idempotent: true,
			Retry: engine.RetryPolicy{MaxAttempts: 1}},
	}}
}

func TestManualGateBlocksAndReleasesTheLease(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	def := gatedDefinition(t, h)
	id := mustStart(t, h, def, "mrc_1", nil)

	h.resume(id)

	inst := h.instance(id)
	if inst.State != engine.InstanceWaitingSignal {
		t.Fatalf("expected WAITING_SIGNAL, got %s", inst.State)
	}
	// A five-day compliance review must hold no worker resource at all. Holding a lease across
	// it would be a leak measured in days and would make the lease arithmetic meaningless.
	if inst.LeaseOwner != "" || inst.LeaseUntil != nil {
		t.Fatalf("the gate held a lease: owner=%q until=%v", inst.LeaseOwner, inst.LeaseUntil)
	}
	if got := h.runs.Runs("after"); got != 0 {
		t.Fatalf("the step after the gate ran %d times before the gate was signalled", got)
	}
	if got := inst.RunAfter.Sub(h.clock.Now()); got != 5*24*time.Hour {
		t.Fatalf("gate deadline = %s from now, want the step timeout of 120h", got)
	}
}

func TestManualGateResumesOnSignalAndAuditsThePrincipal(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	def := gatedDefinition(t, h)
	id := mustStart(t, h, def, "mrc_1", nil)
	h.resume(id)

	body, _ := json.Marshal(map[string]string{"decision": "APPROVE", "attestationRef": "att_9"})
	if err := h.eng.Signal(context.Background(), id, "compliance-approval", engine.SignalPayload{
		Data:      body,
		Principal: "usr_compliance_7",
		Scopes:    []string{"onboarding:approve"},
		Reason:    "reviewed the sanctions screening and the MCC",
		SourceIP:  "10.1.2.3",
	}); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	h.resume(id)

	inst := h.instance(id)
	if inst.State != engine.InstanceCompleted {
		t.Fatalf("expected COMPLETED after the signal, got %s", inst.State)
	}
	if got := h.runs.Runs("after"); got != 1 {
		t.Fatalf("the step after the gate ran %d times, want 1", got)
	}

	// A manual gate whose approval cannot be attributed to a person is not a control.
	ev := h.audit.Find(engine.ActionSignal)
	if ev == nil {
		t.Fatal("the signal was not audited")
	}
	if ev.Principal != "usr_compliance_7" {
		t.Errorf("audited principal = %q, want usr_compliance_7", ev.Principal)
	}
	if ev.Reason == "" || ev.SourceIP != "10.1.2.3" {
		t.Errorf("the audit record lost the reason or the source IP: %+v", ev)
	}

	// The gate's checkpointed output carries the decision, so the operator surface can render
	// who approved without joining another table.
	var gateOut struct {
		Principal string          `json:"principal"`
		Payload   json.RawMessage `json:"payload"`
	}
	for _, s := range inst.Steps {
		if s.Name == "compliance-review" {
			if err := json.Unmarshal(s.Output, &gateOut); err != nil {
				t.Fatalf("gate output does not decode: %v", err)
			}
		}
	}
	if gateOut.Principal != "usr_compliance_7" {
		t.Errorf("gate output principal = %q", gateOut.Principal)
	}
	if !strings.Contains(string(gateOut.Payload), "APPROVE") {
		t.Errorf("gate output lost the decision payload: %s", gateOut.Payload)
	}
}

// TestSignalArrivingBeforeTheWaitIsNotLost is the racing-signal case. The KYC vendor's webhook
// routinely beats our own commit, and an engine that only accepts a signal while a wait is
// active silently drops the decision.
func TestSignalArrivingBeforeTheWaitIsNotLost(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	def := gatedDefinition(t, h)
	id := mustStart(t, h, def, "mrc_1", nil)

	if err := h.eng.Signal(context.Background(), id, "compliance-approval", engine.SignalPayload{
		Data: []byte(`{"decision":"APPROVE"}`), Principal: "usr_1", Reason: "pre-approved",
	}); err != nil {
		t.Fatalf("Signal before the wait: %v", err)
	}
	h.resume(id)

	if got := h.instance(id).State; got != engine.InstanceCompleted {
		t.Fatalf("a signal delivered before the wait was lost; state = %s", got)
	}
}

func TestDuplicateSignalIsANoOp(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	def := gatedDefinition(t, h)
	id := mustStart(t, h, def, "mrc_1", nil)
	h.resume(id)

	send := func(principal string) {
		t.Helper()
		if err := h.eng.Signal(context.Background(), id, "compliance-approval", engine.SignalPayload{
			Data: []byte(`{"decision":"APPROVE"}`), Principal: principal, Reason: "r",
		}); err != nil {
			t.Fatalf("Signal: %v", err)
		}
	}
	send("usr_1")
	send("usr_2") // a duplicate vendor webhook, or a client retry

	h.resume(id)

	if got := h.runs.Runs("after"); got != 1 {
		t.Fatalf("a duplicate signal advanced the workflow twice: after ran %d times", got)
	}
	if ev := h.audit.Find(engine.ActionSignal); ev == nil || ev.Principal != "usr_1" {
		t.Fatalf("the first delivery is the decision of record; audited principal = %v", ev)
	}
}

func TestGateTimeoutParksRatherThanFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	def := gatedDefinition(t, h)
	id := mustStart(t, h, def, "mrc_1", nil)
	h.resume(id)

	h.clock.Advance(6 * 24 * time.Hour)
	h.resume(id)

	inst := h.instance(id)
	// A compliance review nobody performed is a late human, not a system failure — and the
	// instance must still be here when they get to it.
	if inst.State != engine.InstanceParked {
		t.Fatalf("expected PARKED after the gate timed out, got %s", inst.State)
	}
	if len(h.repo.DLQ()) != 1 {
		t.Fatalf("a gate timeout must raise a DLQ entry for escalation; got %d", len(h.repo.DLQ()))
	}

	// The late signal still works.
	if err := h.eng.Signal(context.Background(), id, "compliance-approval", engine.SignalPayload{
		Data: []byte(`{"decision":"APPROVE"}`), Principal: "usr_late", Reason: "sorry",
	}); err != nil {
		t.Fatalf("late Signal: %v", err)
	}
	h.resume(id)

	if got := h.instance(id).State; got != engine.InstanceCompleted {
		t.Fatalf("a late signal did not resume the parked instance; state = %s", got)
	}
}

func TestCancellationCompensatesAndReachesCanceled(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ok("provision")
	h.ok("undo-provision")
	def := &engine.Definition{Name: "cancellable", Version: 1, Steps: []engine.Step{
		{Name: "provision", Activity: "provision", Compensation: "undo-provision",
			Timeout: time.Second, Idempotent: true, SideEffecting: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
		{Name: "gate", ManualGate: true, Signal: "go-ahead", Timeout: 24 * time.Hour},
		{Name: "never", Activity: "never", Timeout: time.Second, Idempotent: true, Retry: engine.RetryPolicy{MaxAttempts: 1}},
	}}
	h.ok("never")
	id := mustStart(t, h, def, "mrc_1", nil)
	h.resume(id)
	if got := h.instance(id).State; got != engine.InstanceWaitingSignal {
		t.Fatalf("setup: expected WAITING_SIGNAL, got %s", got)
	}

	if err := h.eng.Cancel(context.Background(), id, "merchant withdrew the application"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	h.resume(id)

	inst := h.instance(id)
	if inst.State != engine.InstanceCanceled {
		t.Fatalf("expected CANCELED, got %s", inst.State)
	}
	if got := h.runs.Runs("undo-provision"); got != 1 {
		t.Fatalf("cancellation ran the compensation %d times, want 1", got)
	}
	if got := h.runs.Runs("never"); got != 0 {
		t.Fatalf("a cancelled instance ran a step past the cancellation point %d times", got)
	}
	if ev := h.audit.Find(engine.ActionCancel); ev == nil || ev.Reason == "" {
		t.Fatalf("the cancellation was not audited with its reason: %v", ev)
	}
}

func TestCancelIsRejectedForATerminalInstance(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	def := linearDefinition(t, h, "a")
	id := mustStart(t, h, def, "mrc_1", nil)
	h.resume(id)

	err := h.eng.Cancel(context.Background(), id, "too late")
	if err == nil {
		t.Fatal("cancelling a completed instance should be rejected, not silently accepted")
	}
}
