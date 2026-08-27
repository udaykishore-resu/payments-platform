//go:build temporal

// This file compiles only with `-tags temporal` and requires `go get go.temporal.io/sdk`.
// See doc.go for the concept mapping and the criteria for choosing this implementation.

package temporal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	sdktemporal "go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Options configures the Temporal-backed engine.
type Options struct {
	// Client is a connected Temporal client. The caller owns its lifecycle: this adapter does
	// not dial, because connection parameters, TLS material and namespace selection belong to
	// the process's wiring, not to a workflow engine.
	Client client.Client

	// Namespace is used only for describe/list calls that take it explicitly.
	Namespace string

	// TaskQueue is the queue both the workflow and its activities run on. One queue for the
	// whole onboarding workflow: the fan-out is bounded at four and the activities are all
	// I/O-bound against vendors, so splitting queues would buy nothing but two more things to
	// size.
	TaskQueue string

	// Definitions is the same registry the Postgres engine uses. The generated workflow function
	// looks a definition up by (name, version) and walks its steps, which is what keeps the
	// definition the single source of truth across both implementations.
	Definitions *engine.Registry

	// Activities is the same activity registry. Each engine.Activity is registered with Temporal
	// under its own name, so `Step.Activity` resolves identically in both engines.
	Activities *engine.Activities

	// Auditor receives signals and cancellations. Temporal records them in Event History as
	// well, but Event History is not our hash-chained audit store and is not under our retention
	// policy, so the audit obligation is satisfied here rather than there.
	Auditor engine.Auditor

	// Metrics receives the same two series as the Postgres engine.
	Metrics engine.Metrics
}

// Engine implements engine.Engine on top of Temporal.
type Engine struct {
	c       client.Client
	ns      string
	queue   string
	defs    *engine.Registry
	acts    *engine.Activities
	audit   engine.Auditor
	metrics engine.Metrics
}

var _ engine.Engine = (*Engine)(nil)

// New builds the adapter.
func New(o Options) (*Engine, error) {
	if o.Client == nil {
		return nil, apierror.New(apierror.CodeInternalError, "temporal engine: a client is required")
	}
	if o.TaskQueue == "" {
		return nil, apierror.New(apierror.CodeInternalError, "temporal engine: a task queue is required")
	}
	e := &Engine{
		c:       o.Client,
		ns:      o.Namespace,
		queue:   o.TaskQueue,
		defs:    o.Definitions,
		acts:    o.Activities,
		audit:   o.Auditor,
		metrics: o.Metrics,
	}
	if e.defs == nil {
		e.defs = engine.NewRegistry()
	}
	if e.acts == nil {
		e.acts = engine.NewActivities()
	}
	if e.audit == nil {
		e.audit = engine.NopAuditor{}
	}
	if e.metrics == nil {
		e.metrics = engine.NopMetrics{}
	}
	return e, nil
}

// Register validates a definition and stores it, exactly as the Postgres engine does.
func (e *Engine) Register(def *engine.Definition) error {
	if err := e.defs.Register(def); err != nil {
		return err
	}
	return e.acts.VerifyDefinition(def)
}

// NewWorker builds a Temporal worker that serves the generated saga workflow and every
// registered activity.
//
// The workflow function is registered once, under the fixed type name sagaWorkflowType, and
// dispatches on the (definition, version) carried in its input. Registering one function per
// definition would put the definition list into the worker's registration code, which is exactly
// the duplication the Definition type exists to remove.
func (e *Engine) NewWorker() (worker.Worker, error) {
	w := worker.New(e.c, e.queue, worker.Options{
		// Mirrors the Postgres engine's DefaultConcurrency, so the two implementations present
		// the same backpressure shape to the vendors they call.
		MaxConcurrentActivityExecutionSize: 32,
		EnableSessionWorker:                false,
	})

	deps := &workflowDeps{defs: e.defs}
	w.RegisterWorkflowWithOptions(deps.saga, workflow.RegisterOptions{Name: sagaWorkflowType})

	for _, name := range e.acts.Names() {
		act, err := e.acts.Get(name)
		if err != nil {
			return nil, err
		}
		act := act
		w.RegisterActivityWithOptions(
			func(ctx context.Context, in activityInput) (activityOutput, error) {
				return runActivity(ctx, act, in)
			},
			activity.RegisterOptions{Name: name},
		)
	}
	return w, nil
}

const sagaWorkflowType = "pp.saga.v1"

// sagaInput is what ExecuteWorkflow is given: enough to recover the definition and the original
// caller input, and nothing else. Everything else is derived deterministically inside the
// workflow function, because a value smuggled in from the client would not survive replay.
type sagaInput struct {
	Definition  string `json:"definition"`
	Version     int    `json:"version"`
	BusinessKey string `json:"businessKey"`
	TenantID    string `json:"tenantId"`
	Input       []byte `json:"input"`
}

type sagaResult struct {
	CompletedSteps []string          `json:"completedSteps"`
	Outputs        map[string][]byte `json:"outputs"`
}

// activityInput is the wire form of engine.Input. It is a separate type because engine.Input
// carries function fields (the heartbeat and checkpoint hooks) that cannot cross a task
// boundary; they are reconstructed on the worker side from Temporal's own primitives.
type activityInput struct {
	WorkflowID     string `json:"workflowId"`
	TenantID       string `json:"tenantId"`
	BusinessKey    string `json:"businessKey"`
	Step           string `json:"step"`
	IdempotencyKey string `json:"idempotencyKey"`
	Payload        []byte `json:"payload"`
	Context        []byte `json:"context"`
	LookupFirst    bool   `json:"lookupFirst"`
}

type activityOutput struct {
	Payload []byte `json:"payload"`
}

// Start maps to ExecuteWorkflow with the Workflow ID set to (definition, business key).
//
// WorkflowIDReusePolicy ALLOW_DUPLICATE_FAILED_ONLY is what reproduces our idempotency
// guarantee: a *live* execution for that ID rejects a second start, and the adapter turns that
// rejection into "here is the existing instance" — the same answer the Postgres engine's partial
// unique index produces. A failed or completed execution may be restarted, which matches the
// Postgres predicate excluding terminal states from the live-key index.
func (e *Engine) Start(ctx context.Context, def *engine.Definition, businessKey string, input []byte) (shared.WorkflowID, error) {
	if def == nil {
		return "", apierror.New(apierror.CodeInternalError, "cannot start a nil workflow definition")
	}
	if err := e.Register(def); err != nil {
		return "", err
	}
	if businessKey == "" && def.BusinessKeyOf != nil {
		k, err := def.BusinessKeyOf(input)
		if err != nil {
			return "", err
		}
		businessKey = k
	}
	if businessKey == "" {
		return "", apierror.Newf(apierror.CodeValidationFailed,
			"workflow %s requires a business key", def.Key())
	}

	id := workflowID(def, businessKey)
	run, err := e.c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                    id,
		TaskQueue:             e.queue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		// The whole-workflow timeout is deliberately generous: onboarding legitimately waits
		// seven days for KYC and five more for compliance. The bounds that matter are per-step.
		WorkflowExecutionTimeout: 30 * 24 * time.Hour,
		WorkflowTaskTimeout:      10 * time.Second,
	}, sagaWorkflowType, sagaInput{
		Definition:  def.Name,
		Version:     def.Version,
		BusinessKey: businessKey,
		Input:       input,
	})
	if err != nil {
		var already *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &already) {
			// A live execution exists. From the caller's point of view "an onboarding exists for
			// this merchant" is success, and it is the same answer they would have got a
			// millisecond earlier.
			return shared.WorkflowID(id), nil
		}
		return "", apierror.Wrapf(err, apierror.CodeDependencyFailure,
			"could not start workflow %s for %s", def.Key(), businessKey)
	}
	return shared.WorkflowID(run.GetID()), nil
}

// Signal maps to SignalWorkflow. The empty run ID targets the latest run, which is what a late
// operator approval must reach.
func (e *Engine) Signal(ctx context.Context, id shared.WorkflowID, name string, payload engine.SignalPayload) error {
	if name == "" {
		return apierror.New(apierror.CodeValidationFailed, "a signal needs a name")
	}
	if err := e.c.SignalWorkflow(ctx, string(id), "", name, signalEnvelope{
		Data:       payload.Data,
		Principal:  payload.Principal,
		Reason:     payload.Reason,
		SourceIP:   payload.SourceIP,
		ReceivedAt: payload.ReceivedAt,
	}); err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return engine.NotFound(id)
		}
		return apierror.Wrapf(err, apierror.CodeDependencyFailure, "could not signal %s", id)
	}
	// Audited after the durable write, so the trail never claims an approval that was not
	// delivered. Temporal's Event History records the signal too, but it is not our hash-chained
	// store and not under our retention policy.
	if err := e.audit.Record(ctx, engine.AuditEvent{
		WorkflowID: id, Action: engine.ActionSignal, Principal: payload.Principal,
		Scopes: payload.Scopes, Reason: payload.Reason, SourceIP: payload.SourceIP,
		OccurredAt: payload.ReceivedAt, Payload: payload.Data,
	}); err != nil {
		return err
	}
	return nil
}

// signalEnvelope is the payload the generated workflow receives on a signal channel.
type signalEnvelope struct {
	Data       []byte    `json:"data"`
	Principal  string    `json:"principal"`
	Reason     string    `json:"reason"`
	SourceIP   string    `json:"sourceIp"`
	ReceivedAt time.Time `json:"receivedAt"`
}

// Cancel maps to CancelWorkflow, which is cooperative in Temporal as it is here: the in-flight
// activity is allowed to observe the cancellation rather than being killed, and the generated
// workflow's rollback stack then runs on a disconnected context so that the compensations
// themselves are not cancelled.
func (e *Engine) Cancel(ctx context.Context, id shared.WorkflowID, reason string) error {
	if err := e.c.CancelWorkflow(ctx, string(id), ""); err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return engine.NotFound(id)
		}
		return apierror.Wrapf(err, apierror.CodeDependencyFailure, "could not cancel %s", id)
	}
	return e.audit.Record(ctx, engine.AuditEvent{
		WorkflowID: id, Action: engine.ActionCancel, Reason: reason, OccurredAt: time.Now().UTC(),
	})
}

// Get reconstructs an engine.Instance from Temporal's describe call and its Event History.
//
// This is where the two implementations differ most visibly to an operator. Ours reads four
// columns; this walks an event history and maps activity-scheduled/completed/failed events back
// onto step records. The mapping is lossy in one direction — Temporal has no notion of our
// StepState AMBIGUOUS or LEASE_LOST, because it does not need them — so those degrade to FAILED.
func (e *Engine) Get(ctx context.Context, id shared.WorkflowID) (*engine.Instance, error) {
	desc, err := e.c.DescribeWorkflowExecution(ctx, string(id), "")
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return nil, engine.NotFound(id)
		}
		return nil, apierror.Wrapf(err, apierror.CodeDependencyFailure, "could not describe %s", id)
	}
	info := desc.GetWorkflowExecutionInfo()

	inst := &engine.Instance{
		ID:    id,
		State: mapExecutionStatus(info.GetStatus()),
	}
	if t := info.GetStartTime(); t != nil {
		inst.CreatedAt = t.AsTime()
	}
	if t := info.GetCloseTime(); t != nil {
		closed := t.AsTime()
		inst.CompletedAt = &closed
		inst.UpdatedAt = closed
	}

	steps, current, err := e.historySteps(ctx, string(id))
	if err != nil {
		return nil, err
	}
	inst.Steps = steps
	inst.CurrentStep = current
	return inst, nil
}

// historySteps folds the Event History into our step records.
func (e *Engine) historySteps(ctx context.Context, id string) ([]engine.StepRecord, string, error) {
	iter := e.c.GetWorkflowHistory(ctx, id, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)

	byActivityID := make(map[int64]*engine.StepRecord, 16)
	var steps []engine.StepRecord
	current := ""
	seq := 0

	for iter.HasNext() {
		ev, err := iter.Next()
		if err != nil {
			return nil, "", apierror.Wrapf(err, apierror.CodeDependencyFailure,
				"could not read the history of %s", id)
		}
		switch ev.GetEventType() {
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
			attr := ev.GetActivityTaskScheduledEventAttributes()
			started := ev.GetEventTime().AsTime()
			rec := engine.StepRecord{
				Name:      attr.GetActivityType().GetName(),
				Sequence:  seq,
				State:     engine.StepRunning,
				Attempt:   1,
				StartedAt: &started,
			}
			seq++
			steps = append(steps, rec)
			byActivityID[ev.GetEventId()] = &steps[len(steps)-1]
			current = rec.Name

		case enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED:
			attr := ev.GetActivityTaskCompletedEventAttributes()
			if rec, ok := byActivityID[attr.GetScheduledEventId()]; ok {
				done := ev.GetEventTime().AsTime()
				rec.State = engine.StepSucceeded
				rec.CompletedAt = &done
			}

		case enumspb.EVENT_TYPE_ACTIVITY_TASK_FAILED:
			attr := ev.GetActivityTaskFailedEventAttributes()
			if rec, ok := byActivityID[attr.GetScheduledEventId()]; ok {
				rec.State = engine.StepFailed
				rec.Error = attr.GetFailure().GetMessage()
			}

		case enumspb.EVENT_TYPE_ACTIVITY_TASK_TIMED_OUT:
			attr := ev.GetActivityTaskTimedOutEventAttributes()
			if rec, ok := byActivityID[attr.GetScheduledEventId()]; ok {
				rec.State = engine.StepTimedOut
				rec.Error = attr.GetFailure().GetMessage()
			}
		}
	}
	return steps, current, nil
}

// Resume has no Temporal equivalent, and that is the correct answer rather than a gap.
//
// Temporal's service drives an execution itself: there is no lease to reclaim and no poller to
// nudge. A running execution needs nothing; a *failed* one is resumed by ResetWorkflowExecution,
// which is an operator action with its own authorization and its own audit record and must not
// be reachable from a generic "try again" call. Resume therefore verifies the execution is
// running and returns.
func (e *Engine) Resume(ctx context.Context, id shared.WorkflowID) error {
	desc, err := e.c.DescribeWorkflowExecution(ctx, string(id), "")
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return engine.NotFound(id)
		}
		return apierror.Wrapf(err, apierror.CodeDependencyFailure, "could not describe %s", id)
	}
	status := desc.GetWorkflowExecutionInfo().GetStatus()
	if status == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		return nil
	}
	return apierror.Wrapf(engine.ErrNotRunnable, apierror.CodeWorkflowNotResumable,
		"instance %s is %s; use a reset to resume a closed execution", id, status)
}

func mapExecutionStatus(s enumspb.WorkflowExecutionStatus) engine.InstanceState {
	switch s {
	case enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING:
		return engine.InstanceRunning
	case enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED:
		return engine.InstanceCompleted
	case enumspb.WORKFLOW_EXECUTION_STATUS_FAILED:
		return engine.InstanceFailed
	case enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED:
		return engine.InstanceCanceled
	case enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED:
		return engine.InstanceFailed
	case enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT:
		return engine.InstanceFailed
	default:
		return engine.InstancePending
	}
}

func workflowID(def *engine.Definition, businessKey string) string {
	// The Workflow ID *is* the deduplication key. Encoding the definition name into it keeps two
	// different workflows over the same merchant from colliding, which the Postgres index does
	// with a composite key.
	return def.Name + ":" + businessKey
}

// --- the generated workflow ---------------------------------------------------------------------

type workflowDeps struct {
	defs *engine.Registry
}

// saga walks a Definition's steps as a Temporal workflow.
//
// **Determinism.** Everything this function reads is either its input or a value returned by a
// Temporal API: the step list comes from a registry populated at process start from code, and it
// is a slice, so the iteration order is fixed. There is no time.Now, no rand, no map iteration
// and no I/O. Breaking any of those breaks replay, and replay is how Temporal recovers an
// execution — the constraint the Postgres engine does not have because it never replays.
//
// **Compensation.** Temporal has no first-class compensation, so the rollback is an explicit
// stack maintained here. It runs on a *disconnected* context so that a cancellation which
// triggered the rollback does not also cancel the rollback — the single most common way a
// hand-rolled Temporal saga leaves orphaned state.
func (d *workflowDeps) saga(ctx workflow.Context, in sagaInput) (sagaResult, error) {
	log := workflow.GetLogger(ctx)
	def, err := d.defs.Lookup(in.Definition, in.Version)
	if err != nil {
		// A non-retryable application error: this worker does not contain the definition, and no
		// number of retries will change that. It must be redeployed.
		return sagaResult{}, sdktemporal.NewNonRetryableApplicationError(
			fmt.Sprintf("definition %s@v%d is not registered in this worker", in.Definition, in.Version),
			"DefinitionNotRegistered", err)
	}

	result := sagaResult{Outputs: make(map[string][]byte, len(def.Steps))}
	accumulated := make(map[string]json.RawMessage, len(def.Steps))

	// rollback is the saga stack: each entry undoes one completed step, and it is walked in
	// reverse. A webhook registration must be deleted before the sub-account it belongs to.
	type undo struct {
		step   engine.Step
		output []byte
	}
	var rollback []undo

	compensate := func(cause error) {
		// Disconnected: a cancelled workflow must still be able to run its rollback.
		dctx, cancel := workflow.NewDisconnectedContext(ctx)
		defer cancel()

		irr := def.PivotIndex(engine.PivotIrreversible)
		if irr >= 0 && len(rollback) > 0 {
			for _, u := range rollback {
				if u.step.Pivot && u.step.PivotKind == engine.PivotIrreversible {
					// Past the money pivot the saga may not unwind: the merchant may have live
					// payments. Fail loudly and leave it to the operator case.
					log.Error("refusing to roll back past the irreversible pivot",
						"pivot", u.step.Name, "cause", cause)
					return
				}
			}
		}

		retained := def.PivotIndex(engine.PivotRetained)
		for i := len(rollback) - 1; i >= 0; i-- {
			u := rollback[i]
			if retained >= 0 && def.StepIndex(u.step.Name) <= retained {
				// The record past a retained pivot is kept by law; there is nothing to undo.
				continue
			}
			if u.step.Compensation == "" {
				continue
			}
			cctx := workflow.WithActivityOptions(dctx, workflow.ActivityOptions{
				StartToCloseTimeout: maxDuration(u.step.Timeout, 30*time.Second),
				HeartbeatTimeout:    45 * time.Second,
				RetryPolicy: &sdktemporal.RetryPolicy{
					InitialInterval:    time.Second,
					BackoffCoefficient: 2,
					MaximumInterval:    5 * time.Minute,
					MaximumAttempts:    5,
				},
			})
			var out activityOutput
			cerr := workflow.ExecuteActivity(cctx, u.step.Compensation, activityInput{
				WorkflowID:  workflow.GetInfo(ctx).WorkflowExecution.ID,
				TenantID:    in.TenantID,
				BusinessKey: in.BusinessKey,
				Step:        u.step.Name,
				// Deterministic in (workflow, step, "compensate"), matching the Postgres engine's
				// derivation so a vendor sees the same client reference from either engine.
				IdempotencyKey: workflow.GetInfo(ctx).WorkflowExecution.ID + ":" + u.step.Name + ":compensate",
				Payload:        u.output,
			}).Get(dctx, &out)
			if cerr != nil {
				// A failed compensation does not stop the remaining ones: skipping them would
				// orphan more state, not less. Each failure is logged separately and pages.
				log.Error("COMPENSATION FAILED — external state is orphaned",
					"step", u.step.Name, "compensation", u.step.Compensation, "error", cerr)
			}
		}
	}

	for _, step := range def.Steps {
		if step.ManualGate {
			decision, gerr := awaitSignal(ctx, step)
			if gerr != nil {
				compensate(gerr)
				return result, gerr
			}
			encoded, _ := json.Marshal(decision)
			accumulated[step.Name] = encoded
			result.Outputs[step.Name] = encoded
			result.CompletedSteps = append(result.CompletedSteps, step.Name)
			continue
		}

		contextJSON, merr := json.Marshal(accumulated)
		if merr != nil {
			return result, sdktemporal.NewNonRetryableApplicationError(
				"workflow context does not encode", "ContextEncoding", merr)
		}

		actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			// Ours is one lease covering the instance; Temporal's are three per-activity
			// timeouts. StartToClose is the direct analogue of Step.Timeout — per *attempt*.
			StartToCloseTimeout:    step.Timeout,
			ScheduleToStartTimeout: 5 * time.Minute,
			// Heartbeat timeout is three times the activities' 15 s heartbeat interval, matching
			// constraint (1) of the Postgres engine's lease arithmetic: two missed heartbeats are
			// survivable, three are not.
			HeartbeatTimeout: 45 * time.Second,
			RetryPolicy:      temporalRetry(step.Retry),
		})

		var out activityOutput
		aerr := workflow.ExecuteActivity(actx, step.Activity, activityInput{
			WorkflowID:     workflow.GetInfo(ctx).WorkflowExecution.ID,
			TenantID:       in.TenantID,
			BusinessKey:    in.BusinessKey,
			Step:           step.Name,
			IdempotencyKey: workflow.GetInfo(ctx).WorkflowExecution.ID + ":" + step.Name,
			Payload:        in.Input,
			Context:        contextJSON,
		}).Get(ctx, &out)

		if aerr != nil {
			if sdktemporal.IsCanceledError(aerr) {
				log.Info("workflow cancelled; unwinding", "step", step.Name)
			}
			compensate(aerr)
			return result, aerr
		}

		accumulated[step.Name] = json.RawMessage(out.Payload)
		result.Outputs[step.Name] = out.Payload
		result.CompletedSteps = append(result.CompletedSteps, step.Name)
		if step.Compensation != "" {
			rollback = append(rollback, undo{step: step, output: out.Payload})
		}

		// Cancellation is checked between steps as well as during them, so a cancel that lands
		// while no activity is in flight is not deferred to the next external call.
		if ctx.Err() != nil {
			compensate(ctx.Err())
			return result, ctx.Err()
		}
	}
	return result, nil
}

// awaitSignal is the manual gate: a signal channel raced against a timer.
//
// Temporal's signal channels are durable and buffer a signal that arrives before the wait
// begins, which is the same early-arrival guarantee the Postgres engine gets from a uniquely
// indexed row. Reaching the timer is not a failure: it is a late human, and the workflow returns
// a distinguishable error so the caller parks rather than fails.
func awaitSignal(ctx workflow.Context, step engine.Step) (signalEnvelope, error) {
	ch := workflow.GetSignalChannel(ctx, step.Signal)
	timer := workflow.NewTimer(ctx, step.Timeout)

	var got signalEnvelope
	received := false

	sel := workflow.NewSelector(ctx)
	sel.AddReceive(ch, func(c workflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, &got)
		received = true
	})
	sel.AddFuture(timer, func(workflow.Future) {})
	sel.Select(ctx)

	if !received {
		return signalEnvelope{}, sdktemporal.NewNonRetryableApplicationError(
			fmt.Sprintf("gate %s timed out waiting for signal %s", step.Name, step.Signal),
			"SignalTimeout", nil)
	}
	return got, nil
}

// temporalRetry maps our RetryPolicy onto Temporal's.
//
// Near 1:1, with one behavioural difference worth knowing about: **Temporal's jitter is fixed at
// ±20 %**, while ours is full jitter. Under a vendor-wide blip that difference is real — ±20 %
// leaves a cohort largely in phase, so the recovering vendor sees waves rather than a spread.
// There is no knob for it; it is a cost of the platform, recorded here rather than discovered
// during an incident.
//
// NonRetryableErrorTypes carries the classes our classifier would refuse. Temporal matches on
// error *type strings*, not on error content, so an activity must encode its class in the type
// it returns — which is why every activity that means "business no" returns an ApplicationError
// typed TerminalBusiness rather than merely a non-retryable one.
func temporalRetry(p engine.RetryPolicy) *sdktemporal.RetryPolicy {
	nonRetryable := []string{
		string(engine.ClassTerminalBusiness),
		string(engine.ClassTerminalTechnical),
	}
	for _, c := range p.NonRetryable {
		nonRetryable = append(nonRetryable, string(c))
	}
	return &sdktemporal.RetryPolicy{
		InitialInterval:        p.InitialInterval,
		BackoffCoefficient:     p.BackoffFactor,
		MaximumInterval:        p.MaxInterval,
		MaximumAttempts:        int32(p.MaxAttempts),
		NonRetryableErrorTypes: nonRetryable,
	}
}

// runActivity adapts an engine.Activity to Temporal's activity signature.
//
// The two hooks that cannot cross the task boundary are rebuilt here from Temporal's own
// primitives: Heartbeat becomes activity.RecordHeartbeat, and Checkpoint/Lookup become heartbeat
// *details*, which Temporal replays to the next attempt via GetHeartbeatDetails. That is the
// same idea as our `progress` column and it gives the fan-out step the same property: a crash
// after two of four gateways resumes at branch three.
func runActivity(ctx context.Context, act engine.Activity, in activityInput) (activityOutput, error) {
	progress := map[string][]byte{}
	if activity.HasHeartbeatDetails(ctx) {
		_ = activity.GetHeartbeatDetails(ctx, &progress)
	}

	info := activity.GetInfo(ctx)
	engineIn := engine.Input{
		WorkflowID:     shared.WorkflowID(in.WorkflowID),
		TenantID:       shared.TenantID(in.TenantID),
		BusinessKey:    in.BusinessKey,
		Step:           in.Step,
		Attempt:        int(info.Attempt),
		IdempotencyKey: in.IdempotencyKey,
		Payload:        in.Payload,
		Context:        in.Context,
		// An attempt after the first begins with lookup-before-act, exactly as the Postgres
		// engine's LookupFirst does. Temporal cannot tell us *why* the previous attempt ended,
		// so this is deliberately conservative: a lookup is cheap, and a duplicate create is not.
		LookupFirst: info.Attempt > 1,
	}
	engineIn = engineIn.WithHooks(
		func(_ context.Context, p []byte) error {
			activity.RecordHeartbeat(ctx, progress)
			_ = p
			return nil
		},
		func(_ context.Context, key string, value []byte) error {
			progress[key] = value
			activity.RecordHeartbeat(ctx, progress)
			return nil
		},
		func(_ context.Context, key string) ([]byte, bool, error) {
			v, ok := progress[key]
			return v, ok, nil
		},
	)

	out, err := act.Execute(ctx, engineIn)
	if err != nil {
		return activityOutput{}, toApplicationError(in.Step, err)
	}
	return activityOutput{Payload: out}, nil
}

// toApplicationError encodes our FailureClass into a Temporal error *type*, because that is the
// only thing Temporal's retry policy can match on. Our classifier can read an HTTP status out of
// a wrapped chain; Temporal's cannot, so the class has to be lifted into the type string here or
// it is lost.
func toApplicationError(step string, err error) error {
	class := engine.ClassifyStep(err, true)
	if class.IsTerminal() {
		return sdktemporal.NewNonRetryableApplicationError(err.Error(), string(class), err)
	}
	return sdktemporal.NewApplicationError(err.Error(), string(class), err)
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
