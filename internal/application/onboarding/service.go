// Package onboarding holds the merchant-onboarding use cases (BC-3): the thin, auditable surface
// an operator and the merchant portal drive the twelve-step saga through.
//
// It is deliberately thin. The saga's behaviour — retries, backoff, leases, fencing,
// compensation, the pivots, the DLQ — belongs to `internal/workflows/engine` and its definition,
// and re-implementing any of it here would produce two places that disagree about what a
// workflow does. What lives here is what a *use case* owes: the tenant guard, the authorization
// check on a manual gate, the audit record, and the assembly of a case view a human can read.
//
// One property is worth stating up front because every caller depends on it: **Start is
// idempotent on the merchant.** Starting an onboarding twice for one merchant returns the
// existing case. That is not a convenience for retrying clients — it is the mechanism that
// guarantees one live onboarding per merchant, and it is enforced by a partial unique index on
// the business key rather than by a check-then-insert this layer could race.
package onboarding

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/onboarding"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Auditor records an auditable action inside the caller's transaction.
type Auditor interface {
	Record(ctx context.Context, r ports.Repositories, action, resourceType, resourceID, outcome string, detail map[string]any) error
}

// SignalScope is the OAuth scope a principal must hold to signal a manual gate.
//
// A manual gate is the point at which a human takes responsibility for a decision the platform
// would not make on its own. Without an authorization check the platform has taken it instead,
// and the audit record would name whichever service happened to make the call.
const SignalScope = "onboarding:approve"

// Deps is the onboarding service's dependency set.
type Deps struct {
	// Engine is the workflow port. It is the port, never an implementation: the same use cases
	// run on the Postgres engine today and on Temporal tomorrow without a line of this file
	// changing (ADR-014).
	Engine engine.Engine
	UoW    ports.UnitOfWork
	Audit  Auditor
	Clock  shared.Clock
}

// Service is the onboarding use-case facade.
type Service struct {
	deps Deps
	def  *engine.Definition
}

// NewService wires the service and pins the definition it drives.
//
// The definition is resolved once, at construction, rather than per call. A workflow instance
// persists only its type and version, and a use case that rebuilt the definition per request
// could hand the engine a *different* definition under the same key — which the registry rejects,
// but only after the caller has been told their onboarding failed.
func NewService(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = shared.SystemClock{}
	}
	return &Service{deps: d, def: onboarding.Definition()}
}

// StartCommand begins an onboarding.
type StartCommand struct {
	TenantID shared.TenantID
	Input    onboarding.Input
	Actor    Actor
}

// Actor identifies who is driving the operation, for the audit trail.
type Actor struct {
	ID     string
	Name   string
	Scopes []string
	Reason string
	IP     string
}

// HasScope reports whether the actor presented a scope.
func (a Actor) HasScope(want string) bool {
	for _, s := range a.Scopes {
		if s == want {
			return true
		}
	}
	return false
}

// Case is the operator-facing view of one onboarding.
//
// It carries the per-step history as well as the instance's own state because the question a
// human actually asks is "where is this and what is it waiting for", and the instance state alone
// answers neither: WAITING_SIGNAL says a gate is open, and only the step list says which one.
type Case struct {
	WorkflowID  shared.WorkflowID
	MerchantID  shared.MerchantID
	TenantID    shared.TenantID
	Definition  string
	Version     int
	State       engine.InstanceState
	CurrentStep string
	// AwaitingSignal names the signal a parked or waiting instance needs, or "".
	AwaitingSignal string
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    *time.Time
	Steps          []StepView
}

// StepView is one step's status, in the order the saga declares it.
type StepView struct {
	Name        string
	Sequence    int
	State       string
	Attempt     int
	Error       string
	StartedAt   *time.Time
	CompletedAt *time.Time
	// ManualGate marks the steps a human must act on, so the operator surface can distinguish
	// "the platform is working" from "the platform is waiting for you".
	ManualGate bool
	Pivot      bool
}

// IsTerminal reports whether the case has finished, successfully or otherwise.
func (c *Case) IsTerminal() bool { return c.State.IsFinal() }

// Start begins an onboarding, or returns the case that already exists.
//
// It is idempotent on the merchant. A second call while a case is live returns the first one
// unchanged and creates nothing; a second call after a *terminal* case is a genuinely new,
// separately auditable attempt, which is what lets a merchant whose onboarding failed be
// onboarded again without resurrecting the old case and losing the record of why it failed.
func (s *Service) Start(ctx context.Context, cmd StartCommand) (*Case, error) {
	if err := assertTenant(cmd.TenantID); err != nil {
		return nil, err
	}
	if cmd.Input.TenantID.IsZero() {
		cmd.Input.TenantID = cmd.TenantID
	}
	if cmd.Input.TenantID != cmd.TenantID {
		return nil, apierror.New(apierror.CodeTenantMismatch,
			"the onboarding input names a different tenant from the request")
	}
	if err := cmd.Input.Validate(); err != nil {
		return nil, err
	}

	raw, err := json.Marshal(cmd.Input)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "the onboarding input does not encode")
	}

	// Whether a live case already exists is read *before* Start, because afterwards the two
	// cases are indistinguishable: Start returns the same identifier either way, which is exactly
	// what makes it idempotent. This read decides only whether to audit; the guarantee itself is
	// the engine's unique index, which a check-then-act in this layer could not provide.
	existed := s.hasLiveCase(ctx, cmd.Input.BusinessKey())

	id, err := s.deps.Engine.Start(ctx, s.def, cmd.Input.BusinessKey(), raw)
	if err != nil {
		return nil, err
	}

	c, err := s.Get(ctx, cmd.TenantID, id)
	if err != nil {
		return nil, err
	}
	// The audit record is written only for a case this call actually created. Recording a
	// "started" event for the idempotent replay would make the trail claim a merchant was
	// onboarded twice, which is exactly what the idempotency exists to prevent.
	if !existed {
		if err := s.record(ctx, "onboarding.started", c, cmd.Actor, map[string]any{
			"merchantId":  cmd.Input.MerchantID.String(),
			"gateways":    gatewayStrings(cmd.Input.Gateways),
			"environment": string(cmd.Input.Environment),
		}); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// hasLiveCase reports whether a live onboarding already exists for the business key.
func (s *Service) hasLiveCase(ctx context.Context, key string) bool {
	var found bool
	_ = s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		rec, err := r.Workflows.GetInstanceByBusinessKey(ctx, s.def.Name, key)
		if err == nil && rec != nil {
			found = !engine.InstanceState(rec.State).IsFinal()
		}
		return nil
	})
	return found
}

// Get returns the case and its per-step status.
func (s *Service) Get(ctx context.Context, tenantID shared.TenantID, id shared.WorkflowID) (*Case, error) {
	if err := assertTenant(tenantID); err != nil {
		return nil, err
	}
	inst, err := s.deps.Engine.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if inst == nil {
		return nil, apierror.Newf(apierror.CodeOnboardingCaseNotFound, "onboarding case %s not found", id)
	}
	if inst.TenantID != tenantID {
		// Not-found rather than forbidden, for the reason it always is: distinguishing the two
		// leaks the existence of another tenant's identifiers.
		return nil, apierror.Newf(apierror.CodeOnboardingCaseNotFound, "onboarding case %s not found", id)
	}

	c := &Case{
		WorkflowID:  inst.ID,
		MerchantID:  shared.MerchantID(inst.BusinessKey),
		TenantID:    inst.TenantID,
		Definition:  inst.Definition,
		Version:     inst.Version,
		State:       inst.State,
		CurrentStep: inst.CurrentStep,
		LastError:   inst.LastError,
		CreatedAt:   inst.CreatedAt,
		UpdatedAt:   inst.UpdatedAt,
		CompletedAt: inst.CompletedAt,
	}
	if step := s.def.StepByName(inst.CurrentStep); step != nil && step.ManualGate {
		if inst.State == engine.InstanceWaitingSignal || inst.State == engine.InstanceParked {
			c.AwaitingSignal = step.Signal
		}
	}

	steps, err := s.listSteps(ctx, id)
	if err != nil {
		return nil, err
	}
	c.Steps = steps
	return c, nil
}

// listSteps reads the checkpointed history.
//
// It reads through the repository rather than through the instance the engine returned because
// the two answer different questions: the instance carries the engine's runtime view, and the
// repository carries what was actually checkpointed. When they disagree, the checkpoint is the
// one a human should be shown — it is what a resume will replay from.
func (s *Service) listSteps(ctx context.Context, id shared.WorkflowID) ([]StepView, error) {
	var records []ports.WorkflowStepRecord
	if err := s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		var err error
		records, err = r.Workflows.ListSteps(ctx, id)
		return err
	}); err != nil {
		return nil, err
	}

	out := make([]StepView, 0, len(records))
	for _, rec := range records {
		v := StepView{
			Name: rec.Name, Sequence: rec.Sequence, State: rec.State, Attempt: rec.Attempt,
			Error: rec.Error, StartedAt: rec.StartedAt, CompletedAt: rec.CompletedAt,
		}
		if def := s.def.StepByName(rec.Name); def != nil {
			v.ManualGate = def.ManualGate
			v.Pivot = def.Pivot
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out, nil
}

// SignalCommand delivers a decision to a manual gate.
type SignalCommand struct {
	TenantID   shared.TenantID
	WorkflowID shared.WorkflowID
	// Name is the signal, e.g. `compliance-approval`. It is explicit rather than inferred from
	// the instance's current step so that a signal arriving for a gate the instance has already
	// left is rejected rather than silently applied to whatever gate is open now.
	Name string
	// Data is the decision body: the compliance verdict, the KYC outcome.
	Data []byte
	// IdempotencyKey lets a duplicate delivery be recognised as a duplicate rather than as a
	// second decision.
	IdempotencyKey string
	Actor          Actor
}

// Signal delivers a signal to a manual gate.
//
// Three things must hold, and each is a control rather than a formality:
//
//   - the principal holds the approval scope, because a gate anyone can signal is not a gate;
//   - the principal is named, because an approval that cannot be attributed to a person is not
//     evidence of anything;
//   - a reason is recorded, because "approved" with no stated basis is unreviewable six months
//     later, which is exactly when it is read.
func (s *Service) Signal(ctx context.Context, cmd SignalCommand) (*Case, error) {
	if err := assertTenant(cmd.TenantID); err != nil {
		return nil, err
	}
	if cmd.Name == "" {
		return nil, apierror.New(apierror.CodeValidationFailed, "a signal requires a name")
	}
	if cmd.Actor.ID == "" {
		return nil, apierror.New(apierror.CodeUnauthenticated,
			"a manual gate signal must name the principal that sent it").
			WithDetail(apierror.Detail{
				Field: "principal", Code: "UNATTRIBUTED_SIGNAL",
				Message: "an approval nobody can be held to is not a control",
				RuleID:  "L7.AUDIT_ACTOR_IDENTIFIED",
			})
	}
	if !cmd.Actor.HasScope(SignalScope) {
		return nil, apierror.Newf(apierror.CodeInsufficientScope,
			"signalling a manual gate requires the `%s` scope", SignalScope)
	}
	if cmd.Actor.Reason == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"a manual gate signal requires a stated reason").
			WithDetail(apierror.Detail{
				Field: "reason", Code: "REASON_REQUIRED",
				Message: "the reason is what makes the decision reviewable later",
				RuleID:  "L7.AUDIT_REASON_REQUIRED",
			})
	}

	// The tenant check happens before the signal, not after: delivering a signal and then
	// discovering it belonged to another tenant would have already changed that tenant's state.
	current, err := s.Get(ctx, cmd.TenantID, cmd.WorkflowID)
	if err != nil {
		return nil, err
	}
	if current.IsTerminal() {
		return nil, apierror.Newf(apierror.CodeWorkflowNotResumable,
			"onboarding case %s has finished (%s) and cannot be signalled", cmd.WorkflowID, current.State)
	}

	if err := s.deps.Engine.Signal(ctx, cmd.WorkflowID, cmd.Name, engine.SignalPayload{
		Data:           cmd.Data,
		Principal:      cmd.Actor.ID,
		Scopes:         cmd.Actor.Scopes,
		Reason:         cmd.Actor.Reason,
		SourceIP:       cmd.Actor.IP,
		ReceivedAt:     s.deps.Clock.Now(),
		IdempotencyKey: cmd.IdempotencyKey,
	}); err != nil {
		return nil, err
	}

	if err := s.record(ctx, "onboarding.gate_signalled", current, cmd.Actor, map[string]any{
		"signal":         cmd.Name,
		"step":           current.CurrentStep,
		"idempotencyKey": cmd.IdempotencyKey,
	}); err != nil {
		return nil, err
	}

	// Nudge the instance rather than waiting for a poller: an operator who has just approved
	// something expects to see it move, and the alternative is a support conversation about
	// whether the approval landed.
	if err := s.deps.Engine.Resume(ctx, cmd.WorkflowID); err != nil {
		return nil, err
	}
	return s.Get(ctx, cmd.TenantID, cmd.WorkflowID)
}

// CancelCommand aborts an onboarding.
type CancelCommand struct {
	TenantID   shared.TenantID
	WorkflowID shared.WorkflowID
	Actor      Actor
}

// Cancel requests cooperative cancellation.
//
// Cooperative, not immediate: an in-flight external call is never abandoned, because abandoning
// it produces exactly the ambiguity the rest of this platform spends its effort avoiding — we
// would not know whether the side effect happened. Waiting out a call is cheaper than owning an
// unknown.
func (s *Service) Cancel(ctx context.Context, cmd CancelCommand) (*Case, error) {
	if err := assertTenant(cmd.TenantID); err != nil {
		return nil, err
	}
	if cmd.Actor.Reason == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"cancelling an onboarding requires a stated reason")
	}
	current, err := s.Get(ctx, cmd.TenantID, cmd.WorkflowID)
	if err != nil {
		return nil, err
	}
	if current.IsTerminal() {
		return nil, apierror.Newf(apierror.CodeWorkflowNotResumable,
			"onboarding case %s has already finished (%s)", cmd.WorkflowID, current.State)
	}
	if err := s.deps.Engine.Cancel(ctx, cmd.WorkflowID, cmd.Actor.Reason); err != nil {
		return nil, err
	}
	if err := s.record(ctx, "onboarding.cancelled", current, cmd.Actor, map[string]any{
		"step":  current.CurrentStep,
		"state": string(current.State),
	}); err != nil {
		return nil, err
	}
	return s.Get(ctx, cmd.TenantID, cmd.WorkflowID)
}

// RetryCommand re-drives a stalled case.
type RetryCommand struct {
	TenantID   shared.TenantID
	WorkflowID shared.WorkflowID
	Actor      Actor
}

// Retry drives a case forward now rather than waiting for a poller.
//
// It is the operator's "try again", and it is deliberately a *resume* rather than a restart: the
// engine replays no completed step, so a retry after a failure at step nine does not re-submit a
// KYC case or re-provision a gateway account. Restarting would.
func (s *Service) Retry(ctx context.Context, cmd RetryCommand) (*Case, error) {
	if err := assertTenant(cmd.TenantID); err != nil {
		return nil, err
	}
	current, err := s.Get(ctx, cmd.TenantID, cmd.WorkflowID)
	if err != nil {
		return nil, err
	}
	if current.State == engine.InstanceCompleted || current.State == engine.InstanceCanceled {
		return nil, apierror.Newf(apierror.CodeWorkflowNotResumable,
			"onboarding case %s is %s and cannot be retried", cmd.WorkflowID, current.State)
	}
	if err := s.deps.Engine.Resume(ctx, cmd.WorkflowID); err != nil {
		return nil, err
	}
	if err := s.record(ctx, "onboarding.retried", current, cmd.Actor, map[string]any{
		"step":      current.CurrentStep,
		"fromState": string(current.State),
	}); err != nil {
		return nil, err
	}
	return s.Get(ctx, cmd.TenantID, cmd.WorkflowID)
}

// record writes the audit line in its own transaction.
//
// It is a separate transaction from the engine's own write, and that is a real trade-off rather
// than an oversight: the engine owns its persistence and does not expose a transaction to join.
// The consequence is that a crash between the two leaves a signal applied with no audit line —
// which the engine's own Auditor hook, invoked inside its transaction, is what actually closes.
// The record here is the *use case's* view of the operation, and it is written second on purpose:
// an audit line for an operation that did not happen is worse than a missing one for an operation
// that did, because the first is believed.
func (s *Service) record(ctx context.Context, action string, c *Case, actor Actor, detail map[string]any) error {
	if s.deps.Audit == nil {
		return nil
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["actorId"] = actor.ID
	detail["actorName"] = actor.Name
	if actor.Reason != "" {
		detail["reason"] = actor.Reason
	}
	detail["workflowId"] = c.WorkflowID.String()
	return s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return s.deps.Audit.Record(ctx, r, action, "onboarding", c.MerchantID.String(), "SUCCESS", detail)
	})
}

func assertTenant(t shared.TenantID) error {
	if t.IsZero() {
		return apierror.New(apierror.CodeMissingTenantContext, "the request carries no tenant context")
	}
	return nil
}

func gatewayStrings(in []shared.GatewayID) []string {
	out := make([]string, 0, len(in))
	for _, g := range in {
		out = append(out, g.String())
	}
	return out
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-04, BR-19, FR-17, FR-20, FR-30, FR-31, FR-32.
//
// The onboarding case: starting it under a business key, signalling a waiting step, reading
// it, cancelling it and resuming it
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
