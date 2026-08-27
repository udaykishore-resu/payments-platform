package onboarding

import (
	"context"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/apptest"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/onboarding"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

var testEpoch = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)

const (
	testTenant   shared.TenantID   = "ten_01HZTESTTENANT00000000000"
	testMerchant shared.MerchantID = "mrc_01HZTESTMERCHANT000000000"
)

type env struct {
	t      *testing.T
	store  *apptest.Store
	engine *apptest.Engine
	audit  *apptest.Auditor
	svc    *Service
	ctx    context.Context
}

func newEnv(t *testing.T) *env {
	t.Helper()
	store := apptest.NewStore()
	clock := apptest.NewClock(testEpoch)
	eng := apptest.NewEngine(store, clock)
	a := &apptest.Auditor{Store: store}
	return &env{
		t: t, store: store, engine: eng, audit: a,
		ctx: apptest.WithTenant(context.Background(), testTenant),
		svc: NewService(Deps{
			Engine: eng,
			UoW:    apptest.NewUnitOfWork(store, apptest.NewRecorder()),
			Audit:  a, Clock: clock,
		}),
	}
}

func startCmd() StartCommand {
	return StartCommand{
		TenantID: testTenant,
		Input: onboarding.Input{
			MerchantID:     testMerchant,
			TenantID:       testTenant,
			Environment:    shared.EnvironmentSandbox,
			Gateways:       []shared.GatewayID{"gw-a"},
			Currencies:     []money.Currency{"EUR"},
			PaymentMethods: []shared.PaymentMethod{shared.MethodCard},
			BankAccountID:  "ba_1",
			RequestedBy:    "usr_1",
		},
		Actor: Actor{ID: "usr_1", Name: "Operator", Scopes: []string{SignalScope}},
	}
}

// TestStartIsIdempotentOnTheMerchant.
//
// Starting twice returns the same case and creates nothing. This is the mechanism behind "one
// live onboarding per merchant" — not a convenience for retrying clients — and it is what stops a
// double-submitted form producing two KYC submissions and two provisioned gateway accounts.
func TestStartIsIdempotentOnTheMerchant(t *testing.T) {
	// Verifies: BR-04, FR-17.
	t.Parallel()
	e := newEnv(t)

	first, err := e.svc.Start(e.ctx, startCmd())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	second, err := e.svc.Start(e.ctx, startCmd())
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if first.WorkflowID != second.WorkflowID {
		t.Fatalf("two starts produced %s and %s; one live onboarding per merchant is broken",
			first.WorkflowID, second.WorkflowID)
	}
	// The replay must not be audited as a second start, or the trail claims the merchant was
	// onboarded twice.
	started := 0
	for _, a := range e.audit.Actions() {
		if a == "onboarding.started" {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("%d start records for one onboarding, want 1", started)
	}
}

// TestStartRejectsAnInputThatCannotPossiblyOnboard, before an instance exists to compensate.
func TestStartRejectsAnInputThatCannotPossiblyOnboard(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*StartCommand)
	}{
		{"no merchant", func(c *StartCommand) { c.Input.MerchantID = "" }},
		{"no gateways to provision", func(c *StartCommand) { c.Input.Gateways = nil }},
		{"no settlement currency", func(c *StartCommand) { c.Input.Currencies = nil }},
		{"no payment methods", func(c *StartCommand) { c.Input.PaymentMethods = nil }},
		{"an invalid environment", func(c *StartCommand) { c.Input.Environment = "staging" }},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			cmd := startCmd()
			tc.mutate(&cmd)
			if _, err := e.svc.Start(e.ctx, cmd); err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
		})
	}
}

// TestStartRefusesAnInputNamingAnotherTenant. Tenant identity has one origin; an input that
// disagrees with the authenticated context is a second origin, and the safe answer is refusal.
func TestStartRefusesAnInputNamingAnotherTenant(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cmd := startCmd()
	cmd.Input.TenantID = "ten_01HZOTHER00000000000000"

	if _, err := e.svc.Start(e.ctx, cmd); err == nil {
		t.Fatal("an input naming another tenant was accepted")
	} else if apierror.CodeOf(err) != apierror.CodeTenantMismatch {
		t.Fatalf("got %s, want TENANT_MISMATCH", apierror.CodeOf(err))
	}
}

// TestGetAssemblesTheCaseFromTheCheckpointedSteps.
//
// The instance state alone answers neither of the questions a human asks. WAITING_SIGNAL says a
// gate is open; only the step list says which one, and only the definition says whether it is a
// gate a person has to act on or a pivot they cannot undo.
func TestGetAssemblesTheCaseFromTheCheckpointedSteps(t *testing.T) {
	// Verifies: FR-30.
	t.Parallel()
	e := newEnv(t)
	c, err := e.svc.Start(e.ctx, startCmd())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	e.engine.Advance(c.WorkflowID, onboarding.StepComplianceReview, engine.InstanceWaitingSignal,
		onboarding.StepValidateMerchant, onboarding.StepSubmitKYC, onboarding.StepAwaitKYCDecision)

	got, err := e.svc.Get(e.ctx, testTenant, c.WorkflowID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CurrentStep != onboarding.StepComplianceReview {
		t.Fatalf("current step = %q", got.CurrentStep)
	}
	if got.AwaitingSignal != onboarding.SignalComplianceApproval {
		t.Fatalf("awaiting signal = %q, want %q", got.AwaitingSignal, onboarding.SignalComplianceApproval)
	}
	if len(got.Steps) != 3 {
		t.Fatalf("got %d steps, want 3", len(got.Steps))
	}
	if !got.Steps[2].Pivot {
		t.Fatalf("step %q is not marked as a pivot; the operator cannot see what is irreversible",
			got.Steps[2].Name)
	}
	if got.MerchantID != testMerchant {
		t.Fatalf("merchant = %s, want %s", got.MerchantID, testMerchant)
	}
}

// TestGetRefusesACaseFromAnotherTenantAsNotFound.
func TestGetRefusesACaseFromAnotherTenantAsNotFound(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c, err := e.svc.Start(e.ctx, startCmd())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.svc.Get(e.ctx, "ten_01HZOTHER00000000000000", c.WorkflowID); err == nil {
		t.Fatal("a case from another tenant was returned")
	} else if apierror.CodeOf(err) != apierror.CodeOnboardingCaseNotFound {
		t.Fatalf("got %s, want ONBOARDING_CASE_NOT_FOUND", apierror.CodeOf(err))
	}
}

// TestSignalRequiresAnAuthorizedNamedPrincipalWithAReason.
//
// Each of the three is a control. Without the scope, a gate anyone can signal is not a gate;
// without the principal, an approval that cannot be attributed is not evidence; without the
// reason, "approved" is unreviewable six months later, which is exactly when it is read.
func TestSignalRequiresAnAuthorizedNamedPrincipalWithAReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		actor Actor
		code  apierror.Code
	}{
		{"no principal", Actor{Scopes: []string{SignalScope}, Reason: "ok"}, apierror.CodeUnauthenticated},
		{"no scope", Actor{ID: "usr_1", Reason: "ok"}, apierror.CodeInsufficientScope},
		{"no reason", Actor{ID: "usr_1", Scopes: []string{SignalScope}}, apierror.CodeValidationFailed},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			c, err := e.svc.Start(e.ctx, startCmd())
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			e.engine.Advance(c.WorkflowID, onboarding.StepComplianceReview, engine.InstanceWaitingSignal)

			_, err = e.svc.Signal(e.ctx, SignalCommand{
				TenantID: testTenant, WorkflowID: c.WorkflowID,
				Name: onboarding.SignalComplianceApproval, Actor: tc.actor,
			})
			if err == nil {
				t.Fatalf("a signal with %s was accepted", tc.name)
			}
			if apierror.CodeOf(err) != tc.code {
				t.Fatalf("got %s, want %s", apierror.CodeOf(err), tc.code)
			}
			if len(e.engine.Audits) != 0 {
				t.Fatal("a refused signal reached the engine")
			}
		})
	}
}

// TestSignalDeliversTheDecisionAndAuditsThePrincipal.
func TestSignalDeliversTheDecisionAndAuditsThePrincipal(t *testing.T) {
	// Verifies: BR-19, FR-20.
	t.Parallel()
	e := newEnv(t)
	c, err := e.svc.Start(e.ctx, startCmd())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	e.engine.Advance(c.WorkflowID, onboarding.StepComplianceReview, engine.InstanceWaitingSignal)

	out, err := e.svc.Signal(e.ctx, SignalCommand{
		TenantID: testTenant, WorkflowID: c.WorkflowID,
		Name: onboarding.SignalComplianceApproval, Data: []byte(`{"decision":"APPROVED"}`),
		IdempotencyKey: "sig-1",
		Actor: Actor{
			ID: "usr_compliance", Name: "Compliance Officer",
			Scopes: []string{SignalScope}, Reason: "file reviewed, no adverse findings",
			IP: "203.0.113.7",
		},
	})
	if err != nil {
		t.Fatalf("Signal: %v", err)
	}
	if out.State != engine.InstanceRunning {
		t.Fatalf("state = %s, want RUNNING after the gate was signalled", out.State)
	}
	if len(e.engine.Audits) != 1 {
		t.Fatalf("the engine recorded %d audit events, want 1", len(e.engine.Audits))
	}
	if got := e.engine.Audits[0].Principal; got != "usr_compliance" {
		t.Fatalf("the engine's audit names %q", got)
	}
	if !containsAction(e.audit.Actions(), "onboarding.gate_signalled") {
		t.Fatalf("audit = %v, want a gate_signalled record", e.audit.Actions())
	}
}

// TestSignalIsRefusedOnAFinishedCase. Delivering a decision to a case that has already ended
// would silently do nothing, which reads to the operator as an approval that landed.
func TestSignalIsRefusedOnAFinishedCase(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c, err := e.svc.Start(e.ctx, startCmd())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.svc.Cancel(e.ctx, CancelCommand{
		TenantID: testTenant, WorkflowID: c.WorkflowID,
		Actor: Actor{ID: "u", Reason: "merchant withdrew"},
	}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	_, err = e.svc.Signal(e.ctx, SignalCommand{
		TenantID: testTenant, WorkflowID: c.WorkflowID,
		Name:  onboarding.SignalComplianceApproval,
		Actor: Actor{ID: "u", Scopes: []string{SignalScope}, Reason: "late approval"},
	})
	if err == nil {
		t.Fatal("a signal was accepted on a finished case")
	}
	if apierror.CodeOf(err) != apierror.CodeWorkflowNotResumable {
		t.Fatalf("got %s, want WORKFLOW_NOT_RESUMABLE", apierror.CodeOf(err))
	}
}

// TestCancelRequiresAReasonAndAudits.
func TestCancelRequiresAReasonAndAudits(t *testing.T) {
	// Verifies: FR-31.
	t.Parallel()
	e := newEnv(t)
	c, err := e.svc.Start(e.ctx, startCmd())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.svc.Cancel(e.ctx, CancelCommand{
		TenantID: testTenant, WorkflowID: c.WorkflowID, Actor: Actor{ID: "u"},
	}); err == nil {
		t.Fatal("a cancellation with no reason was accepted")
	}

	out, err := e.svc.Cancel(e.ctx, CancelCommand{
		TenantID: testTenant, WorkflowID: c.WorkflowID,
		Actor: Actor{ID: "u", Reason: "merchant withdrew"},
	})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !out.IsTerminal() {
		t.Fatalf("state = %s, want a terminal state after cancellation", out.State)
	}
	if !containsAction(e.audit.Actions(), "onboarding.cancelled") {
		t.Fatalf("audit = %v, want a cancelled record", e.audit.Actions())
	}
}

// TestRetryIsAResumeNotARestart.
//
// The distinction is the whole point: a retry after a failure at step nine must not re-submit the
// KYC case from step two or re-provision the gateway accounts from step five. The engine replays
// no completed step, and the case view proves the earlier checkpoints survived.
func TestRetryIsAResumeNotARestart(t *testing.T) {
	// Verifies: FR-32.
	t.Parallel()
	e := newEnv(t)
	c, err := e.svc.Start(e.ctx, startCmd())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	e.engine.Advance(c.WorkflowID, onboarding.StepCertification, engine.InstanceFailed,
		onboarding.StepValidateMerchant, onboarding.StepSubmitKYC, onboarding.StepAwaitKYCDecision,
		onboarding.StepValidateBankAccount, onboarding.StepProvisionGateways)

	out, err := e.svc.Retry(e.ctx, RetryCommand{
		TenantID: testTenant, WorkflowID: c.WorkflowID, Actor: Actor{ID: "u", Reason: "vendor recovered"},
	})
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if len(out.Steps) != 5 {
		t.Fatalf("got %d checkpointed steps after a retry, want the 5 that already completed", len(out.Steps))
	}
	if !containsAction(e.audit.Actions(), "onboarding.retried") {
		t.Fatalf("audit = %v, want a retried record", e.audit.Actions())
	}
}

// TestRetryIsRefusedOnACompletedCase.
func TestRetryIsRefusedOnACompletedCase(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c, err := e.svc.Start(e.ctx, startCmd())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	e.engine.Advance(c.WorkflowID, onboarding.StepActivate, engine.InstanceCompleted)

	if _, err := e.svc.Retry(e.ctx, RetryCommand{
		TenantID: testTenant, WorkflowID: c.WorkflowID, Actor: Actor{ID: "u"},
	}); err == nil {
		t.Fatal("a completed case was retried")
	}
}

// TestEveryEntryPointAssertsTenantContext.
func TestEveryEntryPointAssertsTenantContext(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c, err := e.svc.Start(e.ctx, startCmd())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := map[string]func() error{
		"Start": func() error {
			cmd := startCmd()
			cmd.TenantID = ""
			_, err := e.svc.Start(e.ctx, cmd)
			return err
		},
		"Get":    func() error { _, err := e.svc.Get(e.ctx, "", c.WorkflowID); return err },
		"Signal": func() error { _, err := e.svc.Signal(e.ctx, SignalCommand{WorkflowID: c.WorkflowID}); return err },
		"Cancel": func() error { _, err := e.svc.Cancel(e.ctx, CancelCommand{WorkflowID: c.WorkflowID}); return err },
		"Retry":  func() error { _, err := e.svc.Retry(e.ctx, RetryCommand{WorkflowID: c.WorkflowID}); return err },
	}
	for name, call := range calls {
		err := call()
		if err == nil {
			t.Fatalf("%s accepted a request with no tenant context", name)
		}
		if apierror.CodeOf(err) != apierror.CodeMissingTenantContext {
			t.Fatalf("%s: got %s, want MISSING_TENANT_CONTEXT", name, apierror.CodeOf(err))
		}
	}
}

func containsAction(all []string, want string) bool {
	for _, a := range all {
		if a == want {
			return true
		}
	}
	return false
}
