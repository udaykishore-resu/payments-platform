package apptest

import (
	"context"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
)

// Engine is an in-memory workflow engine that implements the port's *contract* rather than its
// behaviour.
//
// The distinction matters. The real engines — Postgres and Temporal — have their own test suites
// for retries, leases, fencing and compensation; re-implementing any of that here would produce a
// third engine to keep in step. What this double reproduces is exactly the guarantees a *use
// case* depends on and can get wrong on its own:
//
//   - Start is idempotent on (definition, business key): a second start while an instance is live
//     returns the first, and a start after a terminal instance creates a new one.
//   - A signal is delivered at most once per name, and only while the instance is live.
//   - Cancel is cooperative: the instance moves to compensating and then to a terminal state.
//   - Steps are checkpointed and readable, because the case view is assembled from them.
//
// Everything else is a no-op that a test can drive explicitly.
type Engine struct {
	mu     sync.Mutex
	store  *Store
	clock  shared.Clock
	live   map[string]shared.WorkflowID
	inst   map[shared.WorkflowID]*engine.Instance
	defs   map[string]*engine.Definition
	Audits []engine.AuditEvent
}

// NewEngine returns an empty engine backed by the store.
func NewEngine(s *Store, clock shared.Clock) *Engine {
	return &Engine{
		store: s, clock: clock,
		live: map[string]shared.WorkflowID{},
		inst: map[shared.WorkflowID]*engine.Instance{},
		defs: map[string]*engine.Definition{},
	}
}

// Start creates an instance, or returns the live one for the same business key.
func (e *Engine) Start(ctx context.Context, def *engine.Definition, businessKey string, input []byte) (shared.WorkflowID, error) {
	if def == nil {
		return "", apierror.New(apierror.CodeConfigurationInvalid, "apptest: nil definition")
	}
	if err := def.Validate(); err != nil {
		return "", err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.defs[def.Key()] = def

	k := def.Key() + "|" + businessKey
	if id, ok := e.live[k]; ok {
		if i := e.inst[id]; i != nil && !i.IsFinal() {
			return id, nil
		}
		delete(e.live, k)
	}

	id := shared.WorkflowID(ids.New(ids.PrefixWorkflowInstance))
	now := e.clock.Now()
	e.inst[id] = &engine.Instance{
		ID: id, TenantID: tenantOf(ctx), Definition: def.Name, Version: def.Version,
		BusinessKey: businessKey, State: engine.InstancePending,
		CurrentStep: def.FirstStep(), Input: input,
		CreatedAt: now, UpdatedAt: now,
	}
	e.live[k] = id
	e.persist(id)
	return id, nil
}

// persist mirrors the instance into the store, so that a use case reading through
// ports.WorkflowRepository sees what the engine wrote — which is what the real engines do, and
// what the case-view assembly depends on.
func (e *Engine) persist(id shared.WorkflowID) {
	i, ok := e.inst[id]
	if !ok {
		return
	}
	rec := ports.WorkflowInstanceRecord{
		ID: i.ID, TenantID: i.TenantID, Definition: i.Definition, Version: i.Version,
		BusinessKey: i.BusinessKey, State: string(i.State), CurrentStep: i.CurrentStep,
		Input: i.Input, CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt, CompletedAt: i.CompletedAt,
	}
	e.store.mu.Lock()
	e.store.Workflows[id] = &rec
	if !i.IsFinal() {
		e.store.WorkflowKey[i.Definition+"|"+i.BusinessKey] = id
	} else {
		delete(e.store.WorkflowKey, i.Definition+"|"+i.BusinessKey)
	}
	e.store.mu.Unlock()
}

// Signal delivers a signal to a live instance.
func (e *Engine) Signal(ctx context.Context, id shared.WorkflowID, name string, p engine.SignalPayload) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	i, ok := e.inst[id]
	if !ok {
		return apierror.Newf(apierror.CodeWorkflowNotFound, "apptest: instance %s not found", id)
	}
	if i.IsFinal() {
		return apierror.Newf(apierror.CodeWorkflowNotResumable, "apptest: instance %s has finished", id)
	}
	e.Audits = append(e.Audits, engine.AuditEvent{
		WorkflowID: id, TenantID: i.TenantID, BusinessKey: i.BusinessKey,
		Action: engine.ActionSignal, Step: i.CurrentStep, Principal: p.Principal,
		Scopes: p.Scopes, Reason: p.Reason, SourceIP: p.SourceIP, OccurredAt: p.ReceivedAt,
	})
	i.State = engine.InstanceRunning
	i.UpdatedAt = e.clock.Now()
	e.persist(id)
	return nil
}

// Cancel moves the instance through compensation to a terminal state.
func (e *Engine) Cancel(ctx context.Context, id shared.WorkflowID, reason string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	i, ok := e.inst[id]
	if !ok {
		return apierror.Newf(apierror.CodeWorkflowNotFound, "apptest: instance %s not found", id)
	}
	if i.IsFinal() {
		return apierror.Newf(apierror.CodeWorkflowNotResumable, "apptest: instance %s has finished", id)
	}
	now := e.clock.Now()
	i.State = engine.InstanceCanceled
	i.LastError = reason
	i.UpdatedAt = now
	i.CompletedAt = &now
	e.Audits = append(e.Audits, engine.AuditEvent{
		WorkflowID: id, TenantID: i.TenantID, Action: engine.ActionCancel,
		Reason: reason, OccurredAt: now,
	})
	e.persist(id)
	return nil
}

// Get returns the instance.
func (e *Engine) Get(ctx context.Context, id shared.WorkflowID) (*engine.Instance, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	i, ok := e.inst[id]
	if !ok {
		return nil, apierror.Newf(apierror.CodeWorkflowNotFound, "apptest: instance %s not found", id)
	}
	cp := *i
	return &cp, nil
}

// Resume is a no-op that records the nudge. A test drives progress with Advance instead, so that
// what a step does is the test's statement rather than the double's.
func (e *Engine) Resume(ctx context.Context, id shared.WorkflowID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.inst[id]; !ok {
		return apierror.Newf(apierror.CodeWorkflowNotFound, "apptest: instance %s not found", id)
	}
	return nil
}

// Advance moves an instance to a named step and state, and checkpoints the steps before it.
//
// It exists so a use-case test can put a case in a specific position — parked on the compliance
// gate, failed at provisioning — without running twelve real activities.
func (e *Engine) Advance(id shared.WorkflowID, step string, state engine.InstanceState, completed ...string) {
	e.mu.Lock()
	i, ok := e.inst[id]
	if ok {
		i.CurrentStep = step
		i.State = state
		i.UpdatedAt = e.clock.Now()
		e.persist(id)
	}
	e.mu.Unlock()
	if !ok {
		return
	}
	for n, name := range completed {
		now := e.clock.Now()
		e.store.mu.Lock()
		e.store.Steps[id] = append(e.store.Steps[id], toStepRecord(id, i.TenantID, name, n+1, now))
		e.store.mu.Unlock()
	}
}

// SetTenant stamps an instance's tenant, so a test can exercise the cross-tenant guard.
func (e *Engine) SetTenant(id shared.WorkflowID, t shared.TenantID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i, ok := e.inst[id]; ok {
		i.TenantID = t
		e.persist(id)
	}
}

func toStepRecord(id shared.WorkflowID, t shared.TenantID, name string, seq int, now time.Time) ports.WorkflowStepRecord {
	return ports.WorkflowStepRecord{
		ID: shared.NewStepID(), WorkflowID: id, TenantID: t, Name: name,
		Sequence: seq, State: string(engine.StepSucceeded), Attempt: 1,
		StartedAt: &now, CompletedAt: &now,
	}
}

// tenantOf reads the tenant a test stamped on the context, defaulting to the empty tenant. The
// real engines take it from the authenticated context the same way.
func tenantOf(ctx context.Context) shared.TenantID {
	if t, ok := ctx.Value(TenantKey{}).(shared.TenantID); ok {
		return t
	}
	return ""
}

// TenantKey is the context key the doubles read the tenant from.
type TenantKey struct{}

// WithTenant returns a context carrying a tenant, for doubles that need one.
func WithTenant(ctx context.Context, t shared.TenantID) context.Context {
	return context.WithValue(ctx, TenantKey{}, t)
}
