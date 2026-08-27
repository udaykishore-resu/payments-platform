package runtime

import (
	"context"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

// WorkflowRepo adapts the transactional Repositories bundle to the standalone
// ports.WorkflowRepository the workflow engine takes.
//
// # Why an adapter rather than a repository the engine could hold directly
//
// The postgres repositories are reachable only through a unit of work, deliberately: a repository
// that could open its own transaction could perform half its work inside the caller's and half
// outside, which is the exact failure the outbox pattern exists to prevent. The workflow engine,
// on the other hand, is a long-running loop that owns its own transaction boundaries — it leases,
// executes and saves as separate units, because holding one transaction across an activity that
// calls a gateway would hold a database connection for the duration of an HTTP call.
//
// Something has to bridge those two shapes. This does, one method per call, each its own smallest
// possible transaction. The cost is a transaction per operation instead of one per step; the
// benefit is that no connection is ever held across an activity, which is the property that keeps
// a slow gateway from exhausting the pool.
type WorkflowRepo struct {
	uow ports.UnitOfWork
}

// NewWorkflowRepo builds the adapter.
func NewWorkflowRepo(uow ports.UnitOfWork) *WorkflowRepo { return &WorkflowRepo{uow: uow} }

// CreateInstance inserts a new workflow instance.
func (w *WorkflowRepo) CreateInstance(ctx context.Context, i ports.WorkflowInstanceRecord) error {
	return w.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Workflows.CreateInstance(ctx, i)
	})
}

// GetInstance reads one instance.
func (w *WorkflowRepo) GetInstance(ctx context.Context, id shared.WorkflowID) (*ports.WorkflowInstanceRecord, error) {
	var out *ports.WorkflowInstanceRecord
	err := w.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		found, err := r.Workflows.GetInstance(ctx, id)
		out = found
		return err
	})
	return out, err
}

// GetInstanceByBusinessKey resolves an instance by its definition and business key, which is what
// makes "start this workflow twice" a no-op rather than a second instance.
func (w *WorkflowRepo) GetInstanceByBusinessKey(ctx context.Context, def, key string) (*ports.WorkflowInstanceRecord, error) {
	var out *ports.WorkflowInstanceRecord
	err := w.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		found, err := r.Workflows.GetInstanceByBusinessKey(ctx, def, key)
		out = found
		return err
	})
	return out, err
}

// LeaseRunnable claims instances ready to advance.
func (w *WorkflowRepo) LeaseRunnable(ctx context.Context, workerID string,
	lease time.Duration, limit int) ([]ports.WorkflowInstanceRecord, error) {
	var out []ports.WorkflowInstanceRecord
	err := w.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		found, err := r.Workflows.LeaseRunnable(ctx, workerID, lease, limit)
		out = found
		return err
	})
	return out, err
}

// Heartbeat extends a lease this worker holds. The fencing epoch is what stops a paused worker
// that wakes up from acting on an instance another worker has since taken over.
func (w *WorkflowRepo) Heartbeat(ctx context.Context, id shared.WorkflowID,
	workerID string, epoch int64, extend time.Duration) error {
	return w.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Workflows.Heartbeat(ctx, id, workerID, epoch, extend)
	})
}

// SaveInstance persists instance state.
func (w *WorkflowRepo) SaveInstance(ctx context.Context, i ports.WorkflowInstanceRecord) error {
	return w.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Workflows.SaveInstance(ctx, i)
	})
}

// SaveStep persists one step's state.
func (w *WorkflowRepo) SaveStep(ctx context.Context, s ports.WorkflowStepRecord) error {
	return w.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Workflows.SaveStep(ctx, s)
	})
}

// ListSteps reads an instance's step history.
func (w *WorkflowRepo) ListSteps(ctx context.Context, id shared.WorkflowID) ([]ports.WorkflowStepRecord, error) {
	var out []ports.WorkflowStepRecord
	err := w.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		found, err := r.Workflows.ListSteps(ctx, id)
		out = found
		return err
	})
	return out, err
}

// PushDLQ parks a poisoned step for operator attention rather than retrying it forever.
func (w *WorkflowRepo) PushDLQ(ctx context.Context, id shared.WorkflowID,
	step string, payload []byte, reason string) error {
	return w.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Workflows.PushDLQ(ctx, id, step, payload, reason)
	})
}

// CountByState feeds the instance-count gauge.
func (w *WorkflowRepo) CountByState(ctx context.Context) (map[string]int, error) {
	var out map[string]int
	err := w.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		found, err := r.Workflows.CountByState(ctx)
		out = found
		return err
	})
	return out, err
}

// FindStuck returns instances that have made no progress within the threshold.
//
// A workflow that is neither running, nor failed, nor complete is the failure mode nobody alerts
// on until a merchant calls — which is why the engine polls for it rather than waiting to be told.
func (w *WorkflowRepo) FindStuck(ctx context.Context, noProgressFor time.Duration,
	limit int) ([]ports.WorkflowInstanceRecord, error) {
	var out []ports.WorkflowInstanceRecord
	err := w.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		found, err := r.Workflows.FindStuck(ctx, noProgressFor, limit)
		out = found
		return err
	})
	return out, err
}

// WorkflowRepo satisfies the port; the assertion is here so a change to the port is a compile
// error in this file rather than a runtime surprise in a composition root.
var _ ports.WorkflowRepository = (*WorkflowRepo)(nil)
