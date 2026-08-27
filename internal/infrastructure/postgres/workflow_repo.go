package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// WorkflowRepository is the durable store behind the owned workflow engine.
//
// Its methods are storage-shaped rather than domain-shaped, and that is deliberate: the engine
// *is* the domain logic here (ADR-014), so the repository's job is to make leases, epochs and
// checkpoints durable rather than to express a business concept.
type WorkflowRepository struct {
	q      querier
	tenant shared.TenantID
	clock  shared.Clock
}

var _ ports.WorkflowRepository = (*WorkflowRepository)(nil)

const selectInstance = `
SELECT instance_id, tenant_id, workflow_name, workflow_version, business_key, state,
       current_step, input, checkpoint, attempt, lease_owner, attempt_epoch, lease_expires_at,
       run_after, last_error, correlation_id, created_at, updated_at, completed_at
FROM pp.workflow_instances`

// CreateInstance starts a workflow.
//
// The partial unique index on (tenant, workflow_name, business_key) WHERE state is non-terminal
// means starting a workflow twice is a conflict rather than two instances. The caller is
// expected to catch that conflict and read the incumbent — baseline §11 defines starting twice
// as a no-op returning the existing instance, and the index is what makes that true under
// concurrency rather than under a read-then-write race.
func (r *WorkflowRepository) CreateInstance(ctx context.Context, i ports.WorkflowInstanceRecord) error {
	if err := requireOwner(ctx, r.tenant, i.TenantID); err != nil {
		return err
	}
	const q = `
INSERT INTO pp.workflow_instances (
    instance_id, tenant_id, workflow_name, workflow_version, business_key, state,
    current_step, input, checkpoint, attempt, lease_owner, attempt_epoch, lease_expires_at,
    run_after, last_error, correlation_id, version, created_at, updated_at, completed_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,0,$17,$18,$19)`

	if _, err := r.q.Exec(ctx, q,
		i.ID.String(), i.TenantID.String(), i.Definition, orOne(i.Version), i.BusinessKey,
		orString(i.State, "PENDING"), i.CurrentStep, jsonOrEmpty(i.Input),
		jsonOrEmpty(i.Context), i.Attempt,
		nullIfEmpty(i.LeaseOwner), i.LeaseEpoch, i.LeaseUntil,
		orTime(i.RunAfter, r.clock.Now()), i.LastError, i.CorrelationID,
		orTime(i.CreatedAt, r.clock.Now()), orTime(i.UpdatedAt, r.clock.Now()), i.CompletedAt,
	); err != nil {
		return mapError(err, "create workflow instance")
	}
	return nil
}

// GetInstance loads one instance by identifier.
func (r *WorkflowRepository) GetInstance(
	ctx context.Context, id shared.WorkflowID,
) (*ports.WorkflowInstanceRecord, error) {
	return r.oneInstance(ctx, id.String(),
		selectInstance+" WHERE tenant_id = $1 AND instance_id = $2",
		r.tenant.String(), id.String())
}

// GetInstanceByBusinessKey finds the live instance for a definition and key, and falls back to
// the most recent terminal one when there is none live.
//
// The fallback matters: "start this workflow" needs to know whether it already ran and finished,
// not only whether it is running now. Returning nothing for a completed workflow would restart
// an onboarding that already succeeded.
func (r *WorkflowRepository) GetInstanceByBusinessKey(
	ctx context.Context, def, key string,
) (*ports.WorkflowInstanceRecord, error) {
	return r.oneInstance(ctx, def+"/"+key, selectInstance+`
WHERE tenant_id = $1 AND workflow_name = $2 AND business_key = $3
ORDER BY (state NOT IN ('COMPLETED','FAILED','ABORTED')) DESC, created_at DESC
LIMIT 1`, r.tenant.String(), def, key)
}

func (r *WorkflowRepository) oneInstance(
	ctx context.Context, subject, q string, args ...any,
) (*ports.WorkflowInstanceRecord, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	rec, err := scanInstance(r.q.QueryRow(ctx, q, args...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFound(apierror.CodeWorkflowNotFound, "workflow instance", subject)
		}
		return nil, mapError(err, "get workflow instance")
	}
	return rec, nil
}

// LeaseRunnable claims instances that are ready to advance.
//
// Three mechanisms, each doing something the others cannot:
//
//   - `FOR UPDATE SKIP LOCKED` lets several workers claim disjoint batches without contending.
//   - The lease deadline is how a crashed worker's instance gets picked up: the predicate takes
//     rows whose lease is null or expired, so a pod that died mid-step releases its work after
//     the lease elapses rather than holding it until someone notices.
//   - `attempt_epoch` is the fencing token, and it is the one that makes the other two safe. A
//     worker that paused past its lease — a long stop-the-world pause, a network partition, a
//     suspended container — will wake up believing it still holds the instance. Nothing about a
//     lease deadline stops it acting: the deadline only stops a *polite* worker. Because every
//     step write carries the epoch the worker believes it holds, and this claim increments the
//     epoch, the stale worker's write matches zero rows and it learns it has been superseded.
func (r *WorkflowRepository) LeaseRunnable(
	ctx context.Context, workerID string, lease time.Duration, limit int,
) ([]ports.WorkflowInstanceRecord, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	const q = `
WITH claimed AS (
    SELECT instance_id FROM pp.workflow_instances
    WHERE tenant_id = $1
      AND state IN ('PENDING','RUNNING','COMPENSATING')
      AND run_after <= now()
      AND (lease_expires_at IS NULL OR lease_expires_at < now())
    ORDER BY run_after
    FOR UPDATE SKIP LOCKED
    LIMIT $2
)
UPDATE pp.workflow_instances w
SET lease_owner      = $3,
    lease_expires_at = now() + make_interval(secs => $4),
    attempt_epoch    = w.attempt_epoch + 1,
    version          = w.version + 1,
    updated_at       = now()
FROM claimed c
WHERE w.instance_id = c.instance_id
RETURNING w.instance_id, w.tenant_id, w.workflow_name, w.workflow_version, w.business_key,
          w.state, w.current_step, w.input, w.checkpoint, w.attempt, w.lease_owner,
          w.attempt_epoch, w.lease_expires_at, w.run_after, w.last_error, w.correlation_id,
          w.created_at, w.updated_at, w.completed_at`

	rows, err := r.q.Query(ctx, q, r.tenant.String(), pageLimit(limit),
		workerID, lease.Seconds())
	if err != nil {
		return nil, mapError(err, "lease workflow instances")
	}
	defer rows.Close()
	var out []ports.WorkflowInstanceRecord
	for rows.Next() {
		rec, err := scanInstance(rows)
		if err != nil {
			return nil, mapError(err, "lease workflow instances")
		}
		out = append(out, *rec)
	}
	return out, mapError(rows.Err(), "lease workflow instances")
}

// Heartbeat extends a lease the caller still holds.
//
// The epoch is in the predicate, so a worker whose instance was taken over cannot extend a lease
// it no longer owns. Zero rows affected means "you have been superseded" and the worker must
// abandon the step rather than finish it — finishing it would apply a side effect on behalf of
// an instance another worker is already advancing.
func (r *WorkflowRepository) Heartbeat(
	ctx context.Context, id shared.WorkflowID, workerID string, epoch int64, extend time.Duration,
) error {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return err
	}
	const q = `
UPDATE pp.workflow_instances
SET lease_expires_at = now() + make_interval(secs => $5), updated_at = now()
WHERE tenant_id = $1 AND instance_id = $2 AND lease_owner = $3 AND attempt_epoch = $4`

	tag, err := r.q.Exec(ctx, q, r.tenant.String(), id.String(), workerID, epoch, extend.Seconds())
	if err != nil {
		return mapError(err, "heartbeat workflow lease")
	}
	if tag.RowsAffected() == 0 {
		return apierror.Newf(apierror.CodeWorkflowNotResumable,
			"workflow %s is no longer leased by %s at epoch %d; abandon this step",
			id, workerID, epoch)
	}
	return nil
}

// SaveInstance persists an instance's advance, fenced by the lease epoch.
func (r *WorkflowRepository) SaveInstance(ctx context.Context, i ports.WorkflowInstanceRecord) error {
	if err := requireOwner(ctx, r.tenant, i.TenantID); err != nil {
		return err
	}
	const q = `
UPDATE pp.workflow_instances
SET state            = $4,
    current_step     = $5,
    checkpoint       = $6,
    attempt          = $7,
    lease_owner      = $8,
    lease_expires_at = $9,
    run_after        = $10,
    last_error       = $11,
    version          = version + 1,
    updated_at       = $12,
    completed_at     = $13
WHERE tenant_id = $1 AND instance_id = $2 AND attempt_epoch = $3`

	tag, err := r.q.Exec(ctx, q,
		i.TenantID.String(), i.ID.String(), i.LeaseEpoch,
		i.State, i.CurrentStep, jsonOrEmpty(i.Context), i.Attempt,
		nullIfEmpty(i.LeaseOwner), i.LeaseUntil, orTime(i.RunAfter, r.clock.Now()),
		i.LastError, r.clock.Now(), i.CompletedAt,
	)
	if err != nil {
		return mapError(err, "save workflow instance")
	}
	if tag.RowsAffected() == 0 {
		return apierror.Newf(apierror.CodeWorkflowNotResumable,
			"workflow %s has moved past epoch %d; another worker has taken it over",
			i.ID, i.LeaseEpoch)
	}
	return nil
}

// SaveStep checkpoints one step's result.
//
// Every completed step is written here before the next one begins. That ordering is the whole
// resumability guarantee: a crash after a step's side effect but before its checkpoint replays
// the step, which is why step activities must be idempotent; a crash after the checkpoint
// resumes at the next step, which is why the checkpoint must not be batched with the next one's.
func (r *WorkflowRepository) SaveStep(ctx context.Context, s ports.WorkflowStepRecord) error {
	if err := requireOwner(ctx, r.tenant, s.TenantID); err != nil {
		return err
	}
	const q = `
INSERT INTO pp.workflow_steps (
    step_id, instance_id, tenant_id, name, sequence, state, attempt,
    input, output, error, started_at, completed_at, compensated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
ON CONFLICT (step_id) DO UPDATE SET
    state = EXCLUDED.state, attempt = EXCLUDED.attempt, output = EXCLUDED.output,
    error = EXCLUDED.error, started_at = EXCLUDED.started_at,
    completed_at = EXCLUDED.completed_at, compensated_at = EXCLUDED.compensated_at`

	if _, err := r.q.Exec(ctx, q,
		s.ID.String(), s.WorkflowID.String(), s.TenantID.String(), s.Name, s.Sequence,
		orString(s.State, "PENDING"), s.Attempt,
		jsonOrEmpty(s.Input), jsonOrEmpty(s.Output), s.Error,
		s.StartedAt, s.CompletedAt, s.CompensatedAt,
	); err != nil {
		return mapError(err, "save workflow step")
	}
	return nil
}

// ListSteps returns an instance's steps in execution order.
//
// Order matters beyond presentation: compensations run in strict reverse order of completion,
// so the sequence this returns is the sequence the compensator walks backwards.
func (r *WorkflowRepository) ListSteps(
	ctx context.Context, id shared.WorkflowID,
) ([]ports.WorkflowStepRecord, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	const q = `
SELECT step_id, instance_id, tenant_id, name, sequence, state, attempt,
       input, output, error, started_at, completed_at, compensated_at
FROM pp.workflow_steps
WHERE tenant_id = $1 AND instance_id = $2
ORDER BY sequence ASC`

	rows, err := r.q.Query(ctx, q, r.tenant.String(), id.String())
	if err != nil {
		return nil, mapError(err, "list workflow steps")
	}
	defer rows.Close()
	var out []ports.WorkflowStepRecord
	for rows.Next() {
		var (
			s                   ports.WorkflowStepRecord
			stepID, wfID, tenID string
		)
		if err := rows.Scan(&stepID, &wfID, &tenID, &s.Name, &s.Sequence, &s.State, &s.Attempt,
			&s.Input, &s.Output, &s.Error, &s.StartedAt, &s.CompletedAt,
			&s.CompensatedAt); err != nil {
			return nil, mapError(err, "list workflow steps")
		}
		s.ID = shared.StepID(stepID)
		s.WorkflowID = shared.WorkflowID(wfID)
		s.TenantID = shared.TenantID(tenID)
		out = append(out, s)
	}
	return out, mapError(rows.Err(), "list workflow steps")
}

// PushDLQ parks a step that has exhausted its retries.
//
// Parking is deliberately not deleting. The payload and the reason are what an operator replays
// from, and a workflow that silently gave up is indistinguishable from one that never started.
func (r *WorkflowRepository) PushDLQ(
	ctx context.Context, id shared.WorkflowID, step string, payload []byte, reason string,
) error {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return err
	}
	const q = `
INSERT INTO pp.workflow_dlq (tenant_id, instance_id, step_key, payload, reason, parked_at)
VALUES ($1,$2,$3,$4,$5,$6)`
	if _, err := r.q.Exec(ctx, q,
		r.tenant.String(), id.String(), step, jsonOrEmpty(payload), reason,
		r.clock.Now()); err != nil {
		return mapError(err, "push workflow dlq")
	}
	return nil
}

// CountByState returns instance counts per state, for the engine's gauges.
func (r *WorkflowRepository) CountByState(ctx context.Context) (map[string]int, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	const q = `
SELECT state, count(*) FROM pp.workflow_instances
WHERE tenant_id = $1
GROUP BY state`
	rows, err := r.q.Query(ctx, q, r.tenant.String())
	if err != nil {
		return nil, mapError(err, "count workflows by state")
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, mapError(err, "count workflows by state")
		}
		out[state] = n
	}
	return out, mapError(rows.Err(), "count workflows by state")
}

// FindStuck returns instances that have made no progress within the threshold.
//
// A workflow that is neither running nor failed nor complete is the failure mode nobody alerts
// on until a merchant calls to ask why their onboarding has not moved for two days. The query is
// on updated_at rather than created_at: a long workflow is normal, a *stalled* one is not.
func (r *WorkflowRepository) FindStuck(
	ctx context.Context, noProgressFor time.Duration, limit int,
) ([]ports.WorkflowInstanceRecord, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	q := selectInstance + `
WHERE tenant_id = $1
  AND state IN ('PENDING','RUNNING','WAITING_SIGNAL','COMPENSATING')
  AND updated_at < $2
ORDER BY updated_at ASC
LIMIT $3`
	rows, err := r.q.Query(ctx, q, r.tenant.String(),
		r.clock.Now().Add(-noProgressFor), pageLimit(limit))
	if err != nil {
		return nil, mapError(err, "find stuck workflows")
	}
	defer rows.Close()
	var out []ports.WorkflowInstanceRecord
	for rows.Next() {
		rec, err := scanInstance(rows)
		if err != nil {
			return nil, mapError(err, "find stuck workflows")
		}
		out = append(out, *rec)
	}
	return out, mapError(rows.Err(), "find stuck workflows")
}

func scanInstance(row scanRow) (*ports.WorkflowInstanceRecord, error) {
	var (
		rec           ports.WorkflowInstanceRecord
		id, tenant    string
		leaseOwner    *string
		correlationID string
	)
	if err := row.Scan(&id, &tenant, &rec.Definition, &rec.Version, &rec.BusinessKey,
		&rec.State, &rec.CurrentStep, &rec.Input, &rec.Context, &rec.Attempt,
		&leaseOwner, &rec.LeaseEpoch, &rec.LeaseUntil, &rec.RunAfter, &rec.LastError,
		&correlationID, &rec.CreatedAt, &rec.UpdatedAt, &rec.CompletedAt); err != nil {
		return nil, err
	}
	rec.ID = shared.WorkflowID(id)
	rec.TenantID = shared.TenantID(tenant)
	rec.CorrelationID = correlationID
	if leaseOwner != nil {
		rec.LeaseOwner = *leaseOwner
	}
	return &rec, nil
}

// jsonOrEmpty substitutes an empty JSON object for a nil document, because the columns are
// NOT NULL with a jsonb type: a nil would be rejected, and a stored SQL NULL would force every
// reader to distinguish "no checkpoint" from "empty checkpoint" when the engine treats them
// identically.
func jsonOrEmpty(b []byte) []byte {
	if len(b) == 0 {
		return []byte("{}")
	}
	return b
}

func orString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func orOne(v int) int {
	if v <= 0 {
		return 1
	}
	return v
}

func orTime(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t.UTC()
}
