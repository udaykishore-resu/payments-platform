package postgres

import (
	"context"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Compensation state labels stored in the instance context.
const (
	compDone   = "COMPENSATED"
	compFailed = "COMPENSATION_FAILED"
)

// beginAbort moves the instance into COMPENSATING — or refuses to.
//
// The refusal is the interesting half. A saga's pivot is the point after which the transaction
// is not reversible, and onboarding's money pivot is `activate`: once the merchant is ACTIVE,
// real payments can exist, each with its own lifecycle that must complete. De-provisioning the
// gateway sub-accounts of a merchant with live authorizations does not "undo" anything — it
// strands money mid-flight and turns a workflow failure into a customer-harm incident.
//
// So past that pivot the engine parks the instance, writes a DLQ entry and asks for a human.
// Recovery there is roll-forward only: suspend (which deliberately still permits refunds and
// voids), raise an operator case, and leave termination to the separate guarded process that
// waits for payment quiescence.
func (e *Engine) beginAbort(ctx context.Context, rec *ports.WorkflowInstanceRecord, cctx *instanceContext,
	def *engine.Definition, cause string, failure engine.FailureRecord) (outcome, error) {

	if irr := def.PivotIndex(engine.PivotIrreversible); irr >= 0 {
		passed, err := e.stepAlreadyDone(ctx, rec.ID, def.Steps[irr].Name)
		if err != nil {
			return outcomeQuiescent, err
		}
		if passed {
			pivotFailure := engine.NewFailureRecord(failure.Step, failure.Attempt, engine.ClassManual,
				apierror.Wrapf(engine.ErrPivotPassed, apierror.CodeWorkflowNotResumable,
					"refused to roll back past the irreversible pivot %s: %s",
					def.Steps[irr].Name, failure.Message))
			if err := e.pushDLQ(ctx, rec, nil, pivotFailure); err != nil {
				return outcomeQuiescent, err
			}
			cctx.Meta.AbortCause = cause
			cctx.Meta.Failure = &pivotFailure
			e.log.ErrorContext(ctx, "workflow abort refused past the irreversible pivot",
				"instance_id", rec.ID, "business_key", rec.BusinessKey,
				"pivot", def.Steps[irr].Name, "reason", failure.Message)
			return outcomeQuiescent, e.park(ctx, rec, cctx, nil, pivotFailure.Message)
		}
	}

	cctx.Meta.AbortCause = cause
	if failure.Message != "" {
		cctx.Meta.Failure = &failure
	}
	rec.State = string(engine.InstanceCompensating)
	rec.RunAfter = e.now()
	rec.LastError = failure.Message
	if err := e.persist(ctx, rec, cctx); err != nil {
		return outcomeQuiescent, err
	}
	e.log.InfoContext(ctx, "workflow compensating",
		"instance_id", rec.ID, "business_key", rec.BusinessKey,
		"cause", cause, "reason", failure.Message)
	return outcomeAbort, nil
}

// compensate walks completed steps in strict reverse order and undoes each.
//
// Four properties, each of which is a bug if dropped:
//
//   - **Strict reverse order.** A webhook registration must be deleted before the sub-account it
//     belongs to is de-provisioned; doing it the other way round leaves the gateway rejecting the
//     delete and the registration orphaned.
//   - **The compensation receives the step's checkpointed OUTPUT, not its input.** Undoing a
//     provisioning step needs the external account reference the step produced. A compensation
//     that only saw the input would have to re-derive or re-discover it, which is fragile in the
//     ordinary case and impossible in the crash case.
//   - **A failed compensation does not stop the remaining ones.** Skipping them would orphan
//     more state, not less. Each failure is recorded separately and pages separately.
//   - **Compensations at or before a completed retained pivot are skipped.** Once a KYC decision
//     has landed the record is retained by law; "cancel the case" is meaningful only while the
//     case is still pending, and running it afterwards would be a no-op at best and an audit
//     inconsistency at worst.
func (e *Engine) compensate(ctx context.Context, rec *ports.WorkflowInstanceRecord, cctx *instanceContext,
	def *engine.Definition) error {

	steps, err := e.repo.ListSteps(ctx, rec.ID)
	if err != nil {
		return err
	}
	latest := make(map[string]ports.WorkflowStepRecord, len(steps))
	for _, s := range steps {
		if prev, ok := latest[s.Name]; ok {
			if engine.StepState(prev.State).IsComplete() && !engine.StepState(s.State).IsComplete() {
				continue
			}
			if s.Attempt < prev.Attempt && !engine.StepState(s.State).IsComplete() {
				continue
			}
		}
		latest[s.Name] = s
	}

	floor := 0
	if ret := def.PivotIndex(engine.PivotRetained); ret >= 0 {
		if r, ok := latest[def.Steps[ret].Name]; ok && engine.StepState(r.State).IsComplete() {
			floor = ret + 1
		}
	}

	anyFailed := false
	for i := len(def.Steps) - 1; i >= floor; i-- {
		if ctx.Err() != nil {
			// Shutdown mid-walk is safe: compensations are idempotent on K‖"compensate" and the
			// per-step state is durable, so the next worker resumes at the right place.
			return e.releaseLease(ctx, rec)
		}
		step := def.Steps[i]
		if step.Compensation == "" {
			continue // a positive declaration that nothing needs undoing
		}
		record, ok := latest[step.Name]
		if !ok || !engine.StepState(record.State).NeedsCompensation() {
			continue
		}
		if cctx.Meta.Compensations[step.Name] == compDone {
			continue
		}
		if failed := e.runCompensation(ctx, rec, cctx, def, step, record); failed {
			anyFailed = true
		}
	}

	return e.finishAbort(ctx, rec, cctx, def, anyFailed)
}

// runCompensation executes one compensation with its own retry policy, returning whether it
// ultimately failed.
//
// Compensations retry harder than forward steps — five attempts, capped at five minutes — because
// a failed compensation leaves real external state orphaned, and that is strictly more urgent
// than the failure that triggered the rollback. The idempotency key is derived from
// (instance, step, "compensate"), deterministic like every other key here, so a crash
// mid-compensation re-runs into a vendor-side no-op rather than a second delete.
func (e *Engine) runCompensation(ctx context.Context, rec *ports.WorkflowInstanceRecord, cctx *instanceContext,
	def *engine.Definition, step engine.Step, record ports.WorkflowStepRecord) bool {

	act, err := e.acts.Get(step.Compensation)
	if err != nil {
		e.markCompensationFailed(ctx, rec, cctx, def, step, record, err)
		return true
	}

	running := record
	running.State = string(engine.StepCompensating)
	if saveErr := e.repo.SaveStep(ctx, running); saveErr != nil {
		e.log.ErrorContext(ctx, "could not record compensation start",
			"instance_id", rec.ID, "step", step.Name, "error", saveErr)
	}

	in := engine.Input{
		WorkflowID:  rec.ID,
		TenantID:    rec.TenantID,
		BusinessKey: rec.BusinessKey,
		Step:        step.Name,
		Attempt:     1,
		// The "compensate" discriminator keeps the undo's key distinct from the forward step's,
		// so a vendor that dedupes on client reference cannot mistake the rollback for a replay
		// of the create.
		IdempotencyKey: e.idempotencyKey(rec.ID, step.Name+":compensate"),
		Payload:        record.Output,
		Context:        cctx.stepsJSON(),
	}

	timeout := step.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	policy := resilience.Policy{
		MaxAttempts: compensationAttempts,
		Backoff:     resilience.NewExponentialBackoff(compensationBase, compensationCap, resilience.DefaultBackoffMultiplier),
		Timeout:     timeout,
		Clock:       e.clock,
		RetryableFunc: func(err error) bool {
			// A compensation retries on anything that is not a definitive "no". A vendor 404
			// classified as terminal-business means the resource is already gone, which is
			// success from the compensation's point of view and is handled by the activity
			// returning nil — not by retrying.
			return !engine.ClassifyStep(err, true).IsTerminal()
		},
	}

	attempts := 0
	execErr := resilience.Do(ctx, policy, func(c context.Context) error {
		attempts++
		callIn := in
		callIn.Attempt = attempts
		_, err := act.Execute(c, callIn)
		return err
	})

	now := e.now()
	if execErr != nil {
		e.markCompensationFailed(ctx, rec, cctx, def, step, record, execErr)
		return true
	}

	done := record
	done.State = string(engine.StepCompensated)
	done.CompensatedAt = &now
	if saveErr := e.repo.SaveStep(ctx, done); saveErr != nil {
		e.log.ErrorContext(ctx, "could not record compensation completion",
			"instance_id", rec.ID, "step", step.Name, "error", saveErr)
	}
	cctx.markCompensation(step.Name, compDone)
	if err := e.persist(ctx, rec, cctx); err != nil {
		e.log.ErrorContext(ctx, "could not persist compensation progress",
			"instance_id", rec.ID, "step", step.Name, "error", err)
	}
	e.metrics.ObserveStepDuration(ctx, def.Name, step.Name, engine.OutcomeSuccess, 0)
	e.log.InfoContext(ctx, "workflow compensation succeeded",
		"instance_id", rec.ID, "business_key", rec.BusinessKey, "step", step.Name,
		"compensation", step.Compensation, "attempts", attempts)
	if err := e.audit.Record(ctx, engine.AuditEvent{
		WorkflowID: rec.ID, TenantID: rec.TenantID, BusinessKey: rec.BusinessKey,
		Action: engine.ActionCompStep, Step: step.Name, OccurredAt: now,
	}); err != nil {
		e.log.ErrorContext(ctx, "compensation audit failed", "instance_id", rec.ID, "error", err)
	}
	return false
}

// markCompensationFailed records the highest-severity outcome the engine produces.
//
// Real external state is now orphaned: a sub-account, a webhook registration or a secret version
// exists that the platform believes does not. The DLQ entry is what carries the external
// reference to the operator, and the runbook's rule is that the row may not be resolved without
// either cleaning the resource up or registering it in the reconciliation exception register.
func (e *Engine) markCompensationFailed(ctx context.Context, rec *ports.WorkflowInstanceRecord, cctx *instanceContext,
	def *engine.Definition, step engine.Step, record ports.WorkflowStepRecord, err error) {

	failed := record
	failed.State = string(engine.StepCompensationFailed)
	failed.Error = engine.Summarize(engine.Chain(err))
	if saveErr := e.repo.SaveStep(ctx, failed); saveErr != nil {
		e.log.ErrorContext(ctx, "could not record compensation failure",
			"instance_id", rec.ID, "step", step.Name, "error", saveErr)
	}
	cctx.markCompensation(step.Name, compFailed)

	failure := engine.NewFailureRecord(step.Name, 0, engine.ClassTerminalTechnical, err)
	failure.Message = "COMPENSATION_FAILED: " + failure.Message
	if pushErr := e.pushDLQ(ctx, rec, &step, failure); pushErr != nil {
		e.log.ErrorContext(ctx, "could not write compensation failure to the DLQ",
			"instance_id", rec.ID, "step", step.Name, "error", pushErr)
	}
	e.metrics.ObserveStepDuration(ctx, def.Name, step.Name, engine.OutcomeFailed, 0)
	e.log.ErrorContext(ctx, "workflow compensation FAILED — external state is orphaned",
		"instance_id", rec.ID, "business_key", rec.BusinessKey, "step", step.Name,
		"compensation", step.Compensation, "error_chain", engine.Summarize(failure.Chain))
}

// finishAbort settles the instance after the compensation walk.
//
// Cancellation and failure compensate identically and end differently, and keeping them distinct
// is what stops the operator surface from lying about why a merchant's onboarding stopped: a
// cancelled instance reaches CANCELED, a failed one reaches FAILED with a DLQ entry, and a run
// whose compensations themselves failed reaches FAILED regardless of which started it — because
// orphaned external state is the fact that matters, not the intent that led there.
func (e *Engine) finishAbort(ctx context.Context, rec *ports.WorkflowInstanceRecord, cctx *instanceContext,
	def *engine.Definition, anyFailed bool) error {

	now := e.now()
	rec.LeaseOwner = ""
	rec.LeaseUntil = nil
	rec.CurrentStep = ""

	if !anyFailed && cctx.Meta.AbortCause == abortCancel {
		rec.State = string(engine.InstanceCompensated)
		if err := e.persist(ctx, rec, cctx); err != nil {
			return err
		}
		rec.State = string(engine.InstanceCanceled)
		rec.CompletedAt = &now
		e.log.InfoContext(ctx, "workflow cancelled and fully compensated",
			"instance_id", rec.ID, "business_key", rec.BusinessKey)
		return e.persist(ctx, rec, cctx)
	}

	failure := engine.FailureRecord{Step: rec.CurrentStep, Class: engine.ClassTerminalBusiness,
		Message: "workflow aborted"}
	if cctx.Meta.Failure != nil {
		failure = *cctx.Meta.Failure
	}
	if anyFailed {
		failure.Class = engine.ClassTerminalTechnical
		failure.Message = "COMPENSATION_FAILED: " + failure.Message
	}
	// The DLQ entry is written here rather than at the point of failure so that it lands after
	// the compensation walk, carrying the outcome an operator actually needs: not just "step 9
	// failed" but "step 9 failed and four compensations succeeded", or the far louder
	// "and one of them did not".
	if err := e.pushDLQ(ctx, rec, nil, failure); err != nil {
		return err
	}
	rec.State = string(engine.InstanceFailed)
	rec.CompletedAt = &now
	rec.LastError = failure.Message
	e.log.ErrorContext(ctx, "workflow failed after compensation",
		"instance_id", rec.ID, "business_key", rec.BusinessKey,
		"compensations_failed", anyFailed, "reason", failure.Message)
	return e.persist(ctx, rec, cctx)
}
