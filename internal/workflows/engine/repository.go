package engine

import (
	"context"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
)

// Checkpointer is the optional extension of ports.WorkflowRepository that a store able to write
// a step and its instance in one transaction implements.
//
// Why it is an *extension* rather than a required method: ports.WorkflowRepository is the narrow,
// storage-shaped contract the application layer declares, and widening it would force every
// double and every future store to implement a transaction primitive. The Postgres store has one
// — the whole reason the engine lives in the same database as the domain is that
// `workflow_steps`, `workflow_instances`, `merchants` and `outbox_events` commit together — so
// the engine uses it when it is offered and degrades cleanly when it is not.
//
// The degraded path is safe, not merely tolerable, and the reason is worth stating: the engine
// decides whether a step still needs running by looking for a SUCCEEDED *step record*, not by
// trusting `current_step`. A crash between the step write and the instance write therefore
// resumes into "this step is already done, advance" rather than into a replay. The transaction
// buys atomicity with the merchant FSM transition and the outbox row, which is a real and
// separate benefit; it is not load-bearing for replay-freedom.
type Checkpointer interface {
	// CheckpointStep commits the step record and the instance record together, fenced on
	// inst.LeaseEpoch. It must return an error wrapping ErrLeaseLost when the fenced update
	// matches zero rows.
	CheckpointStep(ctx context.Context, inst ports.WorkflowInstanceRecord, step ports.WorkflowStepRecord) error
}

// Checkpoint writes a step and its instance, using the transactional path when the repository
// offers one.
func Checkpoint(ctx context.Context, repo ports.WorkflowRepository, inst ports.WorkflowInstanceRecord, step ports.WorkflowStepRecord) error {
	if tx, ok := repo.(Checkpointer); ok {
		return tx.CheckpointStep(ctx, inst, step)
	}
	// Step first: a step row written without its instance advance is re-read on resume and
	// recognised as complete. The reverse order would advance the instance past a step whose
	// output was never persisted, which *is* a replay — and a replay of a step that already
	// called a vendor.
	if err := repo.SaveStep(ctx, step); err != nil {
		return err
	}
	return repo.SaveInstance(ctx, inst)
}
