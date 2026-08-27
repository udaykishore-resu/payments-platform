package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// outcome is what one iteration of the drive loop decided.
type outcome int

const (
	// outcomeAdvanced means the instance moved on and the loop should continue.
	outcomeAdvanced outcome = iota
	// outcomeQuiescent means the instance is no longer immediately runnable: completed, failed,
	// parked, waiting on a gate, or backing off. The worker releases it and moves on.
	outcomeQuiescent
	// outcomeAbort means the saga must unwind.
	outcomeAbort
)

// parkedRunAfter pushes a parked instance out of the poller's view. A parked instance whose
// RunAfter is in the past would be re-leased on every poll, burning a slot every 250 ms to
// re-discover that a human has still not acted. A signal sets RunAfter back to now, so a late
// approval still resumes in milliseconds.
const parkedRunAfter = 24 * time.Hour

// drive advances one leased instance until it is no longer immediately runnable.
//
// The caller holds the lease and rec.LeaseEpoch is the fencing token for every write below. The
// loop is deliberately a loop rather than one-step-per-lease: a merchant onboarding whose first
// eight steps are sub-second should not pay eight lease acquisitions and eight poll intervals to
// get through them.
func (e *Engine) drive(ctx context.Context, rec ports.WorkflowInstanceRecord) error {
	def, err := e.defs.Lookup(rec.Definition, rec.Version)
	if err != nil {
		// A binary asked to run a workflow it does not contain is a deployment error, not an
		// instance error. Release the lease so a correctly-deployed pod can pick it up, and say
		// so loudly rather than failing a merchant's onboarding over our rollout.
		e.log.ErrorContext(ctx, "workflow definition not registered in this binary",
			"instance_id", rec.ID, "workflow", rec.Definition, "version", rec.Version)
		return e.releaseLease(ctx, &rec)
	}

	cctx, err := decodeContext(rec.Context)
	if err != nil {
		return e.failInstance(ctx, &rec, cctx, engine.NewFailureRecord(rec.CurrentStep, rec.Attempt, engine.ClassTerminalTechnical, err))
	}

	// Poison accounting, done *before* any activity runs.
	//
	// docs/automation-plane.md §4.3 puts this increment inside the lease-acquisition UPDATE, so
	// that an instance which kills its worker still gets counted: the code that would record a
	// failure died with the worker. ports.WorkflowRepository has no crash-count column, so the
	// engine writes it here — still before execution, which preserves the property. The literal
	// wording of §4.3 resets the count only on a *successful step commit*; this implementation
	// resets it on any committed step outcome, including a classified failure, because a worker
	// that lived long enough to record why a step failed is by definition not being damaged by
	// the instance. The literal rule would quarantine a step legitimately exhausting a five-
	// attempt retry policy against a sick vendor, which is precisely the case the DLQ exists for.
	cctx.Meta.CrashCount++
	if cctx.Meta.CrashCount > e.poisonThreshold {
		return e.quarantine(ctx, &rec, cctx)
	}
	if err := e.persist(ctx, &rec, cctx); err != nil {
		return err
	}

	for {
		if ctx.Err() != nil {
			// Cooperative shutdown: the current step has already finished (an activity is never
			// abandoned mid-call), so releasing the lease here is what makes a rolling deploy
			// cost ~0 s of takeover latency instead of a full lease expiry per instance.
			return e.releaseLease(ctx, &rec)
		}

		state := engine.InstanceState(rec.State)
		if state.IsFinal() || state == engine.InstancePoisoned {
			return nil
		}

		if state == engine.InstanceCompensating {
			return e.compensate(ctx, &rec, cctx, def)
		}

		if cctx.cancelRequested() {
			o, err := e.beginAbort(ctx, &rec, cctx, def, abortCancel, engine.FailureRecord{
				Step:    rec.CurrentStep,
				Class:   engine.ClassManual,
				Message: "cancelled: " + cctx.Cancel.Reason,
			})
			if err != nil || o != outcomeAbort {
				return err
			}
			continue
		}

		if rec.CurrentStep == "" {
			return e.complete(ctx, &rec, cctx)
		}
		step := def.StepByName(rec.CurrentStep)
		if step == nil {
			return e.failInstance(ctx, &rec, cctx, engine.NewFailureRecord(rec.CurrentStep, rec.Attempt,
				engine.ClassTerminalTechnical,
				apierror.Newf(apierror.CodeInternalError, "workflow %s has no step named %q", def.Key(), rec.CurrentStep)))
		}
		seq := def.StepIndex(step.Name)

		// Replay-freedom, enforced here and nowhere else.
		//
		// The question "has this step already run" is answered from the *step records*, not from
		// current_step. That matters because the step write and the instance write are two rows:
		// a crash between them leaves current_step pointing at a step whose output is already
		// checkpointed, and trusting current_step alone would re-execute it — a second call to a
		// KYC vendor, a second gateway sub-account. Reading the step record makes that window
		// harmless rather than merely narrow.
		done, err := e.stepAlreadyDone(ctx, rec.ID, step.Name)
		if err != nil {
			return err
		}
		if done {
			if err := e.advance(ctx, &rec, cctx, def, step); err != nil {
				return err
			}
			continue
		}

		var o outcome
		if step.ManualGate {
			o, err = e.runGate(ctx, &rec, cctx, def, step, seq)
		} else {
			o, err = e.runStep(ctx, &rec, cctx, def, step, seq, nil)
		}
		if err != nil {
			return err
		}
		switch o {
		case outcomeAdvanced:
			continue
		case outcomeQuiescent:
			return nil
		case outcomeAbort:
			continue
		}
	}
}

// stepAlreadyDone reports whether any attempt of this step is checkpointed as SUCCEEDED.
func (e *Engine) stepAlreadyDone(ctx context.Context, id shared.WorkflowID, name string) (bool, error) {
	steps, err := e.repo.ListSteps(ctx, id)
	if err != nil {
		return false, err
	}
	for _, s := range steps {
		if s.Name == name && engine.StepState(s.State).IsComplete() {
			return true, nil
		}
	}
	return false, nil
}

// runStep executes one attempt of an activity step and records the result.
func (e *Engine) runStep(ctx context.Context, rec *ports.WorkflowInstanceRecord, cctx *instanceContext,
	def *engine.Definition, step *engine.Step, seq int, sig *signalRecord) (outcome, error) {

	act, err := e.acts.Get(step.Activity)
	if err != nil {
		return outcomeQuiescent, e.dlqAndFail(ctx, rec, cctx, step, seq,
			engine.NewFailureRecord(step.Name, rec.Attempt+1, engine.ClassTerminalTechnical, err))
	}

	attempt := rec.Attempt + 1
	now := e.now()
	rec.State = string(engine.InstanceRunning)

	in := engine.Input{
		WorkflowID:     rec.ID,
		TenantID:       rec.TenantID,
		BusinessKey:    rec.BusinessKey,
		Step:           step.Name,
		Attempt:        attempt,
		IdempotencyKey: e.idempotencyKey(rec.ID, step.Name),
		// Every step is handed the instance's original input and the accumulated outputs of its
		// predecessors. A per-step input document assembled by the engine would need the engine
		// to understand the definition's data flow, which is exactly the coupling the []byte
		// boundary exists to avoid — and this shape is also what makes a DLQ entry replayable
		// without reconstructing anything.
		Payload:     rec.Input,
		Context:     cctx.stepsJSON(),
		LookupFirst: cctx.Meta.LookupFirst,
	}
	if sig != nil {
		in.Signal = sig.Data
		in.SignalPrincipal = sig.Principal
	}
	// A fan-out step runs its branches concurrently and each branch checkpoints as it completes,
	// so these hooks are called from several goroutines at once. The mutex is the engine's
	// responsibility rather than the activity's: an activity author should not have to know that
	// Checkpoint mutates a shared document, and "we forgot to lock the progress map" is a data
	// race whose symptom is a lost checkpoint and therefore a re-provisioned gateway.
	var hooks sync.Mutex
	in = in.WithHooks(
		func(hctx context.Context, progress []byte) error {
			hooks.Lock()
			defer hooks.Unlock()
			return e.repo.Heartbeat(hctx, rec.ID, e.workerID, rec.LeaseEpoch, e.lease)
		},
		func(hctx context.Context, key string, value []byte) error {
			hooks.Lock()
			defer hooks.Unlock()
			return e.checkpointProgress(hctx, rec, cctx, step.Name, key, value)
		},
		func(_ context.Context, key string) ([]byte, bool, error) {
			hooks.Lock()
			defer hooks.Unlock()
			v, ok := cctx.progress(step.Name, key)
			return v, ok, nil
		},
	)

	// The RUNNING row is written before the activity runs. In the real schema it carries
	// `timeout_at = now + step.Timeout`, which is what the database-side reaper uses to time out
	// an activity blocked in a syscall or a non-context-aware library — the case the in-process
	// deadline below provably cannot reach. ports.WorkflowStepRecord has no column for it, so
	// here the in-process deadline is the only enforcement and the reaper's belt-and-braces half
	// lives in the repository implementation.
	running := ports.WorkflowStepRecord{
		WorkflowID: rec.ID,
		TenantID:   rec.TenantID,
		Name:       step.Name,
		Sequence:   seq,
		State:      string(engine.StepRunning),
		Attempt:    attempt,
		Input:      in.Payload,
		StartedAt:  &now,
	}
	if err := e.repo.SaveStep(ctx, running); err != nil {
		return outcomeQuiescent, err
	}

	started := e.now()
	out, execErr := e.invoke(ctx, *rec, step, in, act)
	elapsed := e.now().Sub(started)

	if execErr == nil {
		cctx.putStep(step.Name, out)
		cctx.Meta.LookupFirst = false
		cctx.Meta.CrashCount = 0
		completed := e.now()
		record := running
		record.State = string(engine.StepSucceeded)
		record.Output = out
		record.CompletedAt = &completed
		rec.LastError = ""
		if err := e.commitStep(ctx, rec, cctx, def, step, record); err != nil {
			if engine.IsLeaseLost(err) {
				return outcomeQuiescent, e.abandon(ctx, rec, running, step)
			}
			return outcomeQuiescent, err
		}
		e.metrics.ObserveStepDuration(ctx, def.Name, step.Name, engine.OutcomeSuccess, elapsed)
		e.log.InfoContext(ctx, "workflow step succeeded",
			"instance_id", rec.ID, "business_key", rec.BusinessKey, "step", step.Name,
			"attempt", attempt, "lease_epoch", rec.LeaseEpoch, "worker_id", e.workerID)
		return outcomeAdvanced, nil
	}

	if engine.IsLeaseLost(execErr) {
		return outcomeQuiescent, e.abandon(ctx, rec, running, step)
	}

	class := engine.ClassifyStep(execErr, step.SideEffecting)
	failure := engine.NewFailureRecord(step.Name, attempt, class, execErr)
	failure.OccurredAt = e.now().Format(time.RFC3339Nano)

	metricOutcome := engine.OutcomeFailed
	stepState := engine.StepFailed
	if errors.Is(execErr, engine.ErrStepTimeout) {
		metricOutcome = engine.OutcomeTimeout
		stepState = engine.StepTimedOut
	}
	if class == engine.ClassAmbiguous {
		stepState = engine.StepAmbiguous
	}
	e.metrics.ObserveStepDuration(ctx, def.Name, step.Name, metricOutcome, elapsed)
	e.log.WarnContext(ctx, "workflow step failed",
		"instance_id", rec.ID, "business_key", rec.BusinessKey, "step", step.Name,
		"attempt", attempt, "lease_epoch", rec.LeaseEpoch, "worker_id", e.workerID,
		"failure_class", string(class), "error_chain", engine.Summarize(failure.Chain))

	failed := running
	failed.State = string(stepState)
	failed.Error = engine.Summarize(failure.Chain)
	if err := e.repo.SaveStep(ctx, failed); err != nil {
		return outcomeQuiescent, err
	}
	// Recording an outcome proves the worker survived the instance.
	cctx.Meta.CrashCount = 0
	rec.LastError = failure.Message
	cctx.Meta.Failure = &failure

	switch class {
	case engine.ClassManual:
		return outcomeQuiescent, e.park(ctx, rec, cctx, step,
			"step "+step.Name+" needs a human: "+failure.Message)

	case engine.ClassAmbiguous:
		// Never a blind retry. The next attempt begins with lookup-before-act; if attempts are
		// exhausted the step is parked for an operator probe rather than guessed at, which is
		// the same rule payments use for a gateway timeout and for the same reason.
		if attempt < step.Retry.MaxAttempts {
			cctx.Meta.LookupFirst = true
			return outcomeQuiescent, e.scheduleRetry(ctx, rec, cctx, step, attempt, failed)
		}
		if err := e.pushDLQ(ctx, rec, step, failure); err != nil {
			return outcomeQuiescent, err
		}
		return outcomeQuiescent, e.park(ctx, rec, cctx, step,
			"step "+step.Name+" ended with an unknown outcome that lookup could not resolve; probe the vendor before requeueing")

	case engine.ClassTerminalTechnical:
		return outcomeQuiescent, e.dlqAndFail(ctx, rec, cctx, step, seq, failure)

	case engine.ClassTerminalBusiness:
		// A business "no" is not an engineering failure: no retry, unwind the saga, and let the
		// merchant be told the specific reason.
		o, err := e.beginAbort(ctx, rec, cctx, def, abortFailure, failure)
		return o, err

	default: // ClassTransient
		if attempt < step.Retry.MaxAttempts && step.Retry.Permits(class) {
			return outcomeQuiescent, e.scheduleRetry(ctx, rec, cctx, step, attempt, failed)
		}
		// Retries exhausted. The saga unwinds if there is anything to unwind — leaving a
		// half-provisioned merchant behind because the vendor was down would be a worse outcome
		// than the outage itself — and the DLQ entry is written once the unwind settles, so that
		// it carries the compensation outcomes an operator needs in the same row.
		dlqStep := failed
		dlqStep.State = string(engine.StepDLQ)
		if err := e.repo.SaveStep(ctx, dlqStep); err != nil {
			return outcomeQuiescent, err
		}
		return e.beginAbort(ctx, rec, cctx, def, abortFailure, failure)
	}
}

// invoke runs the activity with its per-attempt deadline, a heartbeater, and panic containment.
//
// The deadline is enforced in-process here and, in the store, by `workflow_steps.timeout_at`
// which the reaper checks. Belt and braces, deliberately: an activity blocked in a syscall or a
// non-context-aware library will not observe an in-process deadline, and without the
// database-side check such a step would hang until the process died. What this function will
// *not* do is abandon the activity in a goroutine and return early — that leaks a goroutine per
// wedged step and, worse, leaves an unowned call in flight against a vendor.
func (e *Engine) invoke(ctx context.Context, rec ports.WorkflowInstanceRecord, step *engine.Step,
	in engine.Input, act engine.Activity) (out engine.Output, err error) {

	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()
	stepCtx, cancelStep := context.WithTimeout(execCtx, step.Timeout)
	defer cancelStep()

	// The heartbeater extends the lease while a long activity runs, and cancels the activity the
	// moment the lease is lost — an activity that keeps working after another worker has taken
	// over is doing work that will be thrown away at best.
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(e.heartbeat)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				// context.WithoutCancel: the heartbeat must still land while the step is being
				// wound down, or a graceful shutdown would drop the lease it is about to release
				// explicitly.
				if hbErr := e.repo.Heartbeat(context.WithoutCancel(ctx), rec.ID, e.workerID, rec.LeaseEpoch, e.lease); hbErr != nil {
					if engine.IsLeaseLost(hbErr) {
						cancelExec()
						return
					}
					e.log.WarnContext(ctx, "workflow heartbeat failed",
						"instance_id", rec.ID, "step", step.Name, "error", hbErr)
				}
			}
		}
	}()
	defer func() {
		close(done)
		wg.Wait()
	}()

	// Panic containment. Every activity runs inside a recover that converts the panic into
	// ClassTerminalTechnical with the stack in the error chain, so one bad payload takes down a
	// step rather than a worker. The crash counter is the backstop for what recover cannot
	// catch — an OOM kill, a SIGKILL, a stack overflow.
	defer func() {
		if r := recover(); r != nil {
			out = nil
			err = engine.WithClass(
				apierror.Newf(apierror.CodeInternalError, "activity %s panicked: %v\n%s",
					step.Activity, r, debug.Stack()),
				engine.ClassTerminalTechnical)
		}
	}()

	out, err = act.Execute(stepCtx, in)
	if err != nil {
		if execCtx.Err() != nil && ctx.Err() == nil {
			// The execution context was cancelled but the caller's was not: the heartbeater
			// found the lease gone.
			return nil, apierror.Wrapf(engine.ErrLeaseLost, apierror.CodeWorkflowNotResumable,
				"instance %s: the lease was reclaimed while step %s was running", rec.ID, step.Name)
		}
		if stepCtx.Err() != nil && errors.Is(stepCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w after %s: %w", engine.ErrStepTimeout, step.Timeout, err)
		}
	}
	return out, err
}

// runGate parks the instance on a manual or external gate, or consumes a delivered signal.
//
// Entering the gate **releases the lease**. That is not an optimization: a seven-day KYC wait or
// a five-day compliance review holding a worker slot would be a resource leak measured in days,
// and would make the lease/heartbeat arithmetic meaningless — a lease is a sixty-second promise
// that a worker is actively doing something.
func (e *Engine) runGate(ctx context.Context, rec *ports.WorkflowInstanceRecord, cctx *instanceContext,
	def *engine.Definition, step *engine.Step, seq int) (outcome, error) {

	sig, delivered := cctx.Signals[step.Signal]
	if delivered {
		now := e.now()
		if sig.ConsumedAt == nil {
			sig.ConsumedAt = &now
			cctx.Signals[step.Signal] = sig
		}
		if step.Activity != "" {
			// The gate declares an activity that applies the human's decision. Consuming the
			// signal is persisted first — the signal is at-most-once, and it must stay consumed
			// even if the activity then fails — and the activity is handed the payload and the
			// principal so that "who approved this" is recorded from the audited delivery rather
			// than from a field the caller controls.
			cctx.Meta.WaitingFor = ""
			rec.State = string(engine.InstanceRunning)
			if err := e.persist(ctx, rec, cctx); err != nil {
				return outcomeQuiescent, err
			}
			return e.runStep(ctx, rec, cctx, def, step, seq, &sig)
		}
		payload := map[string]any{
			"signal":     step.Signal,
			"principal":  sig.Principal,
			"reason":     sig.Reason,
			"receivedAt": sig.ReceivedAt,
			"consumedAt": sig.ConsumedAt,
			"payload":    json.RawMessage(orNull(sig.Data)),
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return outcomeQuiescent, apierror.Wrap(err, apierror.CodeInternalError, "signal payload does not encode")
		}
		cctx.putStep(step.Name, encoded)
		cctx.Meta.WaitingFor = ""
		cctx.Meta.CrashCount = 0
		rec.State = string(engine.InstanceRunning)

		record := ports.WorkflowStepRecord{
			WorkflowID:  rec.ID,
			TenantID:    rec.TenantID,
			Name:        step.Name,
			Sequence:    seq,
			State:       string(engine.StepSucceeded),
			Attempt:     1,
			Input:       rec.Input,
			Output:      encoded,
			StartedAt:   &sig.ReceivedAt,
			CompletedAt: &now,
		}
		if err := e.commitStep(ctx, rec, cctx, def, step, record); err != nil {
			if engine.IsLeaseLost(err) {
				return outcomeQuiescent, e.abandon(ctx, rec, record, step)
			}
			return outcomeQuiescent, err
		}
		e.metrics.ObserveStepDuration(ctx, def.Name, step.Name, engine.OutcomeSuccess, now.Sub(sig.ReceivedAt))
		e.log.InfoContext(ctx, "workflow gate signalled",
			"instance_id", rec.ID, "business_key", rec.BusinessKey, "step", step.Name,
			"signal", step.Signal, "principal", sig.Principal)
		return outcomeAdvanced, nil
	}

	if cctx.Meta.WaitingFor == step.Signal && !e.now().Before(rec.RunAfter) {
		// We were already waiting, the deadline has arrived, and there is still no signal. Park,
		// do not fail. A compliance review that nobody performed is not a system failure, and a
		// signal delivered an hour later must still resume this instance normally.
		//
		// The deadline is re-checked against RunAfter rather than trusted from the fact that we
		// hold a lease, because anything that makes an instance claimable early — an operator
		// nudge, a reaper, a clock that moved — would otherwise park a gate that still has days
		// left on it.
		if err := e.pushDLQ(ctx, rec, step, engine.FailureRecord{
			Step:    step.Name,
			Class:   engine.ClassManual,
			Message: "signal " + step.Signal + " was not delivered within " + step.Timeout.String(),
		}); err != nil {
			return outcomeQuiescent, err
		}
		return outcomeQuiescent, e.park(ctx, rec, cctx, step,
			"gate "+step.Name+" timed out waiting for signal "+step.Signal)
	}
	if cctx.Meta.WaitingFor == step.Signal {
		// Still waiting, still inside the window: release the lease again and leave the deadline
		// exactly where it was.
		rec.State = string(engine.InstanceWaitingSignal)
		rec.LeaseOwner = ""
		rec.LeaseUntil = nil
		return outcomeQuiescent, e.persist(ctx, rec, cctx)
	}

	cctx.Meta.WaitingFor = step.Signal
	rec.State = string(engine.InstanceWaitingSignal)
	rec.RunAfter = e.now().Add(step.Timeout)
	rec.LeaseOwner = ""
	rec.LeaseUntil = nil
	e.metrics.ObserveStepDuration(ctx, def.Name, step.Name, engine.OutcomeWaiting, 0)
	e.log.InfoContext(ctx, "workflow gate opened",
		"instance_id", rec.ID, "business_key", rec.BusinessKey, "step", step.Name,
		"signal", step.Signal, "deadline", rec.RunAfter)
	return outcomeQuiescent, e.persist(ctx, rec, cctx)
}

// commitStep is the one-transaction checkpoint: the step's output, the instance's advance, and
// (in the real store) the merchant FSM transition and the outbox row, together.
func (e *Engine) commitStep(ctx context.Context, rec *ports.WorkflowInstanceRecord, cctx *instanceContext,
	def *engine.Definition, step *engine.Step, record ports.WorkflowStepRecord) error {

	next := def.NextStep(step.Name)
	rec.CurrentStep = next
	rec.Attempt = 0
	rec.RunAfter = e.now()
	if next == "" {
		completed := e.now()
		rec.State = string(engine.InstanceCompleted)
		rec.CompletedAt = &completed
		rec.LeaseOwner = ""
		rec.LeaseUntil = nil
	} else {
		rec.State = string(engine.InstanceRunning)
	}
	encoded, err := cctx.encode()
	if err != nil {
		return err
	}
	rec.Context = encoded
	return engine.Checkpoint(ctx, e.repo, *rec, record)
}

// advance moves past a step whose output is already checkpointed, without executing it. This is
// the resume path for the crash window between the step write and the instance write.
func (e *Engine) advance(ctx context.Context, rec *ports.WorkflowInstanceRecord, cctx *instanceContext,
	def *engine.Definition, step *engine.Step) error {
	next := def.NextStep(step.Name)
	rec.CurrentStep = next
	rec.Attempt = 0
	if next == "" {
		return e.complete(ctx, rec, cctx)
	}
	rec.State = string(engine.InstanceRunning)
	e.metrics.ObserveStepDuration(ctx, def.Name, step.Name, engine.OutcomeSkipped, 0)
	return e.persist(ctx, rec, cctx)
}

// scheduleRetry persists the backoff as a timestamp.
//
// The delay is exponential with **full jitter**, computed by the shared
// resilience.ExponentialBackoff so that the workflow engine and the gateway dispatcher cannot
// drift apart on the formula. Full jitter matters most exactly here: when a vendor blips, every
// affected instance across every worker retries, and a deterministic backoff synchronizes them
// into waves that hit the recovering vendor at the worst possible moment.
//
// The wait lives in RunAfter — a column — rather than in a timer. A worker that dies during a
// backoff loses nothing: the instance is simply not runnable until then, and any worker may pick
// it up. Timers in memory are lost on restart; a column is not.
func (e *Engine) scheduleRetry(ctx context.Context, rec *ports.WorkflowInstanceRecord, cctx *instanceContext,
	step *engine.Step, attempt int, record ports.WorkflowStepRecord) error {

	backoff := resilience.NewExponentialBackoff(step.Retry.InitialInterval, step.Retry.MaxInterval, step.Retry.BackoffFactor)
	delay := backoff.Delay(attempt - 1)

	record.State = string(engine.StepRetryScheduled)
	if err := e.repo.SaveStep(ctx, record); err != nil {
		return err
	}

	rec.Attempt = attempt
	rec.State = string(engine.InstanceRetryBackoff)
	rec.RunAfter = e.now().Add(delay)
	rec.LeaseOwner = ""
	rec.LeaseUntil = nil
	e.log.InfoContext(ctx, "workflow step retry scheduled",
		"instance_id", rec.ID, "step", step.Name, "attempt", attempt,
		"next_attempt_in", delay, "run_after", rec.RunAfter)
	return e.persist(ctx, rec, cctx)
}

// park stops the instance and asks for a human, without calling it a failure.
func (e *Engine) park(ctx context.Context, rec *ports.WorkflowInstanceRecord, cctx *instanceContext,
	step *engine.Step, reason string) error {

	cctx.Meta.ParkReason = reason
	cctx.Meta.WaitingFor = ""
	rec.State = string(engine.InstanceParked)
	rec.RunAfter = e.now().Add(parkedRunAfter)
	rec.LeaseOwner = ""
	rec.LeaseUntil = nil
	rec.LastError = reason
	if err := e.persist(ctx, rec, cctx); err != nil {
		return err
	}
	stepName := ""
	if step != nil {
		stepName = step.Name
	}
	if err := e.audit.Record(ctx, engine.AuditEvent{
		WorkflowID: rec.ID, TenantID: rec.TenantID, BusinessKey: rec.BusinessKey,
		Action: engine.ActionPark, Step: stepName, Reason: reason, OccurredAt: e.now(),
	}); err != nil {
		e.log.ErrorContext(ctx, "workflow park audit failed", "instance_id", rec.ID, "error", err)
	}
	e.log.WarnContext(ctx, "workflow instance parked",
		"instance_id", rec.ID, "business_key", rec.BusinessKey, "step", stepName, "reason", reason)
	return nil
}

// complete marks a finished instance.
func (e *Engine) complete(ctx context.Context, rec *ports.WorkflowInstanceRecord, cctx *instanceContext) error {
	now := e.now()
	rec.State = string(engine.InstanceCompleted)
	rec.CompletedAt = &now
	rec.CurrentStep = ""
	rec.LeaseOwner = ""
	rec.LeaseUntil = nil
	cctx.Meta.CrashCount = 0
	e.log.InfoContext(ctx, "workflow completed",
		"instance_id", rec.ID, "business_key", rec.BusinessKey)
	return e.persist(ctx, rec, cctx)
}

// failInstance is the terminal-technical path: DLQ entry plus FAILED, no compensation.
//
// A technical failure does not unwind the saga. Compensating away a merchant's provisioned
// gateways because *our* credentials were misconfigured destroys work that a redeploy would have
// made succeed; the DLQ triage path for ClassTerminalTechnical is "fix forward, then requeue".
func (e *Engine) failInstance(ctx context.Context, rec *ports.WorkflowInstanceRecord, cctx *instanceContext,
	failure engine.FailureRecord) error {

	if cctx == nil {
		cctx = &instanceContext{}
	}
	cctx.Meta.Failure = &failure
	if cctx.Meta.AbortCause == "" {
		cctx.Meta.AbortCause = abortFailure
	}
	now := e.now()
	rec.State = string(engine.InstanceFailed)
	rec.CompletedAt = &now
	rec.LastError = failure.Message
	rec.LeaseOwner = ""
	rec.LeaseUntil = nil
	e.log.ErrorContext(ctx, "workflow failed",
		"instance_id", rec.ID, "business_key", rec.BusinessKey, "step", failure.Step,
		"failure_class", string(failure.Class), "error_chain", engine.Summarize(failure.Chain))
	return e.persist(ctx, rec, cctx)
}

func (e *Engine) dlqAndFail(ctx context.Context, rec *ports.WorkflowInstanceRecord, cctx *instanceContext,
	step *engine.Step, seq int, failure engine.FailureRecord) error {

	if err := e.pushDLQ(ctx, rec, step, failure); err != nil {
		return err
	}
	if step != nil {
		record := ports.WorkflowStepRecord{
			WorkflowID: rec.ID, TenantID: rec.TenantID, Name: step.Name, Sequence: seq,
			State: string(engine.StepDLQ), Attempt: failure.Attempt,
			Input: rec.Input, Error: engine.Summarize(failure.Chain),
		}
		if err := e.repo.SaveStep(ctx, record); err != nil {
			return err
		}
	}
	return e.failInstance(ctx, rec, cctx, failure)
}

// pushDLQ writes the step payload and the full ordered error chain to the workflow DLQ.
//
// The chain, not a message. Triage's first question is "what actually broke", and a five-deep
// wrapped error answers it with a sentence that has to be read backwards. An ordered array is
// greppable and groupable, which is what turns seventeen DLQ entries into one vendor outage.
func (e *Engine) pushDLQ(ctx context.Context, rec *ports.WorkflowInstanceRecord, step *engine.Step, failure engine.FailureRecord) error {
	stepName := failure.Step
	if step != nil {
		stepName = step.Name
	}
	reason, err := json.Marshal(struct {
		engine.FailureRecord
		InstanceID  string `json:"instanceId"`
		BusinessKey string `json:"businessKey"`
		TenantID    string `json:"tenantId"`
		Workflow    string `json:"workflow"`
	}{
		FailureRecord: failure,
		InstanceID:    string(rec.ID),
		BusinessKey:   rec.BusinessKey,
		TenantID:      string(rec.TenantID),
		Workflow:      rec.Definition,
	})
	if err != nil {
		reason = []byte(`{"message":"failure record does not encode"}`)
	}
	if err := e.repo.PushDLQ(ctx, rec.ID, stepName, rec.Input, string(reason)); err != nil {
		return err
	}
	if auditErr := e.audit.Record(ctx, engine.AuditEvent{
		WorkflowID: rec.ID, TenantID: rec.TenantID, BusinessKey: rec.BusinessKey,
		Action: engine.ActionDLQ, Step: stepName, Reason: failure.Message, OccurredAt: e.now(),
	}); auditErr != nil {
		e.log.ErrorContext(ctx, "workflow DLQ audit failed", "instance_id", rec.ID, "error", auditErr)
	}
	return nil
}

// quarantine hides a worker-damaging instance from every poller.
func (e *Engine) quarantine(ctx context.Context, rec *ports.WorkflowInstanceRecord, cctx *instanceContext) error {
	failure := engine.FailureRecord{
		Step:    rec.CurrentStep,
		Class:   engine.ClassTerminalTechnical,
		Message: fmt.Sprintf("instance quarantined after %d acquisitions without progress", cctx.Meta.CrashCount),
	}
	if err := e.pushDLQ(ctx, rec, nil, failure); err != nil {
		return err
	}
	rec.State = string(engine.InstancePoisoned)
	rec.LastError = failure.Message
	rec.LeaseOwner = ""
	rec.LeaseUntil = nil
	cctx.Meta.Failure = &failure
	e.log.ErrorContext(ctx, "workflow instance quarantined as poison",
		"instance_id", rec.ID, "business_key", rec.BusinessKey, "crash_count", cctx.Meta.CrashCount)
	return e.persist(ctx, rec, cctx)
}

// checkpointProgress persists one intra-activity checkpoint, fenced like every other write.
func (e *Engine) checkpointProgress(ctx context.Context, rec *ports.WorkflowInstanceRecord, cctx *instanceContext,
	step, key string, value []byte) error {
	cctx.putProgress(step, key, value)
	encoded, err := cctx.encode()
	if err != nil {
		return err
	}
	saved := *rec
	saved.Context = encoded
	if err := e.repo.SaveInstance(ctx, saved); err != nil {
		return err
	}
	rec.Context = encoded
	return nil
}

// persist writes the instance, fenced on its lease epoch.
func (e *Engine) persist(ctx context.Context, rec *ports.WorkflowInstanceRecord, cctx *instanceContext) error {
	encoded, err := cctx.encode()
	if err != nil {
		return err
	}
	rec.Context = encoded
	if err := e.repo.SaveInstance(ctx, *rec); err != nil {
		if engine.IsLeaseLost(err) {
			e.log.WarnContext(ctx, "workflow write fenced out",
				"instance_id", rec.ID, "lease_epoch", rec.LeaseEpoch, "worker_id", e.workerID)
			return nil
		}
		return err
	}
	return nil
}

// releaseLease hands the instance back explicitly rather than waiting for the lease to expire.
// This is what makes a rolling deploy invisible in the onboarding-duration histogram.
func (e *Engine) releaseLease(ctx context.Context, rec *ports.WorkflowInstanceRecord) error {
	rec.LeaseOwner = ""
	rec.LeaseUntil = nil
	if err := e.repo.SaveInstance(context.WithoutCancel(ctx), *rec); err != nil && !engine.IsLeaseLost(err) {
		return err
	}
	return nil
}

// abandon records that this worker was fenced out and stops touching the instance.
//
// Nothing else is written: the instance's own row already belongs to the new owner, and a write
// from here is precisely the split-brain the epoch exists to prevent. The step row is marked
// LEASE_LOST so that the investigation has an artefact — two workers logging one instance with
// different epochs is what makes a split-brain diagnosable at a glance, and a silently discarded
// attempt leaves nothing to correlate.
func (e *Engine) abandon(ctx context.Context, rec *ports.WorkflowInstanceRecord, record ports.WorkflowStepRecord, step *engine.Step) error {
	record.State = string(engine.StepLeaseLost)
	if err := e.repo.SaveStep(ctx, record); err != nil {
		e.log.ErrorContext(ctx, "could not record a lease-lost step", "instance_id", rec.ID, "error", err)
	}
	e.log.WarnContext(ctx, "workflow lease lost — abandoning without recording progress",
		"instance_id", rec.ID, "business_key", rec.BusinessKey, "step", step.Name,
		"lease_epoch", rec.LeaseEpoch, "worker_id", e.workerID)
	return nil
}

func orNull(b []byte) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}
