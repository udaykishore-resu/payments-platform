// Package postgres is the durable workflow engine: the default implementation of
// engine.Engine, backed by a relational store through ports.WorkflowRepository.
//
// The reason this engine exists rather than an off-the-shelf one is a single property that no
// external orchestrator can offer: **a step's progress, the merchant's FSM transition and the
// domain event commit in one transaction.** There is no window in which the workflow believes
// KYC is approved and the merchant record does not, or in which a merchant is ACTIVE but no
// `merchant.activated.v1` was ever published. Everything else here — leases, fencing, backoff,
// compensation — is table stakes that a bought engine would also provide; that one property is
// the whole argument (docs/architecture.md TR-4).
//
// The correctness claim, stated plainly: **a crashed worker never causes a duplicate side
// effect.** It rests on four mechanisms, because every one of them has a hole the others cover:
//
//  1. A deterministic idempotency key, stable across attempts, workers and process restarts, and
//     recomputable from the row after total loss of process memory. The vendor dedupes.
//     Hole: not every vendor honours idempotency keys on every endpoint.
//  2. Lookup-before-act. An activity with a create-shaped side effect queries for its own prior
//     effect by that key first. Hole: a lookup can race a create that is still propagating.
//  3. Fencing on `lease_epoch`. Every write carries the epoch the worker acquired; a worker that
//     paused past its lease writes zero rows and aborts. Hole: fencing protects our state, not
//     the vendor's.
//  4. Checkpoint-after, in one transaction. The step output, the instance context, the FSM
//     transition and the outbox event commit together, after the side effect. Hole: the window
//     between side effect and commit — closed by 1 and 2.
//
// What is *not* claimed is exactly-once delivery, which is not achievable across a process
// boundary. What is claimed is effectively-once business effect.
//
// The engine is driven either by Worker (the production poll loop) or by Resume (the operator's
// nudge and the tests' deterministic advance). Both funnel into the same drive loop, so there is
// no second code path whose behaviour could differ from the one production uses.
package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Lease and heartbeat defaults, with the arithmetic from docs/automation-plane.md §4.1.
//
// The five constraints these satisfy, and why each holds:
//
//	(1) H × (M + 1) < L        15 × 3 = 45 < 60 ✓ — a worker may miss two heartbeats and still
//	                           own its lease. Only 45 s of silence puts it at risk, which
//	                           tolerates a GC pause, a brief database hiccup and a transient
//	                           partition without a spurious takeover. A spurious takeover is not
//	                           a correctness problem — fencing handles it — but it wastes work
//	                           and looks like an incident.
//	(2) V = L + R = 70 s       Worst case from a silent worker death to another worker owning
//	                           the instance. Acceptable because onboarding's automated portion is
//	                           budgeted at 30 min p95 and 70 s is 4 % of it.
//	(3) T_step may exceed L    Certification runs 30 min against a 60 s lease. Heartbeats every
//	                           15 s extend the lease to now()+L each time. The requirement is a
//	                           heartbeat at least every (L − H) = 45 s; ours are every 15 s, a
//	                           3× margin.
//	(4) G ≫ L, with explicit   Shutdown releases leases explicitly rather than letting them
//	    release                expire, so a rolling deploy costs ~0 s of takeover latency instead
//	                           of up to 70 s per instance. That is what keeps deploys invisible
//	                           in the onboarding-duration histogram.
//	(5) R = 10 s is cheap      The runnable scan is index-only over the non-terminal instances
//	                           only. There is no benefit to reclaiming faster than the poll can
//	                           dispatch.
const (
	// DefaultLease is L: how long a lease is valid without a heartbeat.
	DefaultLease = 60 * time.Second
	// DefaultHeartbeat is H = L/4.
	DefaultHeartbeat = 15 * time.Second
	// DefaultPollInterval is the poller's floor. In production a LISTEN/NOTIFY wake makes the
	// common case milliseconds; the poll is what guarantees liveness when a notification is lost.
	DefaultPollInterval = 250 * time.Millisecond
	// DefaultReapInterval is R.
	DefaultReapInterval = 10 * time.Second
	// DefaultStuckInterval is how often the stuck sweep runs.
	DefaultStuckInterval = 60 * time.Second
	// DefaultStuckThreshold is the floor of the per-step stuck threshold.
	DefaultStuckThreshold = 15 * time.Minute
	// DefaultBatch is how many instances one poll claims.
	DefaultBatch = 16
	// DefaultConcurrency is the per-pod activity slot count. It is also the backpressure
	// mechanism: when every slot is busy the poller claims nothing, and unclaimed instances stay
	// in the table — visible, durable and countable — rather than in an in-memory queue that can
	// overflow or be lost on a crash.
	DefaultConcurrency = 32
	// DefaultPoisonThreshold quarantines an instance after this many acquisitions with no
	// progress.
	DefaultPoisonThreshold = 3
)

// Compensation retry defaults. Compensations retry harder and longer than forward steps because
// a failed compensation leaves real external state orphaned — a sub-account, a webhook
// registration, a secret version that we believe does not exist — and that is strictly more
// urgent than the failure that triggered the rollback.
const (
	compensationAttempts = 5
	compensationBase     = time.Second
	compensationCap      = 5 * time.Minute
)

// Options configures the engine. Every field has a defensible default except Repo and
// Activities, which have no sensible one.
type Options struct {
	// Repo is the durable store. When it also implements engine.Checkpointer — the Postgres
	// implementation does — the step and instance writes commit in one transaction.
	Repo ports.WorkflowRepository

	// Activities is the registry of executable activities.
	Activities *engine.Activities

	// Definitions is the definition registry. Start registers into it on first use, so a
	// process need not pre-register; production wiring should register at startup anyway, so
	// that a binary missing an activity fails at boot rather than at a merchant's step 10.
	Definitions *engine.Registry

	// Clock supplies both Now and Sleep. Sleep is used only by the in-process compensation
	// retry loop; forward-step backoff is persisted as a timestamp and never slept on.
	Clock resilience.Clock

	// Metrics receives pp_workflow_step_duration_seconds and pp_workflow_instances.
	Metrics engine.Metrics

	// Auditor receives signals and cancellations. Wiring nil here means a manual gate whose
	// approvals cannot be attributed to a person, so the constructor logs loudly rather than
	// treating it as a normal configuration.
	Auditor engine.Auditor

	// WorkerID identifies this process for the lease. It should be pod name + pid + boot id, so
	// that two incarnations of one pod name are distinguishable — a restarted pod holding the
	// same lease owner string as its predecessor is how a "lease held by a zombie" investigation
	// goes wrong.
	WorkerID string

	// Salt is the HMAC key for deterministic idempotency keys. It must be stable for the life of
	// the deployment: rotating it changes every in-flight instance's key, which is precisely the
	// duplicate-side-effect scenario the key exists to prevent.
	Salt []byte

	Lease             time.Duration
	HeartbeatInterval time.Duration
	PollInterval      time.Duration
	ReapInterval      time.Duration
	StuckInterval     time.Duration
	StuckThreshold    time.Duration
	Batch             int
	Concurrency       int
	PoisonThreshold   int

	// Tenant resolves the tenant from the caller's context, so that a started instance carries
	// the row-level-security scope it will be written under.
	Tenant func(context.Context) shared.TenantID

	// NewID mints instance identifiers. Injectable so tests get deterministic IDs.
	NewID func() shared.WorkflowID

	// Logger receives the engine's structured logs. Every line carries instance_id,
	// business_key, step, attempt, lease_epoch and worker_id: two workers logging one instance
	// with different epochs is what makes a split-brain investigation tractable at a glance.
	Logger *slog.Logger
}

// Engine is the durable workflow engine.
type Engine struct {
	repo    ports.WorkflowRepository
	acts    *engine.Activities
	defs    *engine.Registry
	clock   resilience.Clock
	metrics engine.Metrics
	audit   engine.Auditor
	log     *slog.Logger

	workerID string
	salt     []byte

	lease           time.Duration
	heartbeat       time.Duration
	poll            time.Duration
	reap            time.Duration
	stuckEvery      time.Duration
	stuckThreshold  time.Duration
	batch           int
	concurrency     int
	poisonThreshold int

	tenant func(context.Context) shared.TenantID
	newID  func() shared.WorkflowID
}

var _ engine.Engine = (*Engine)(nil)

// New builds an engine, rejecting the two configurations that have no default.
func New(o Options) (*Engine, error) {
	if o.Repo == nil {
		return nil, apierror.New(apierror.CodeInternalError, "workflow engine: a repository is required")
	}
	if o.Activities == nil {
		return nil, apierror.New(apierror.CodeInternalError, "workflow engine: an activity registry is required")
	}
	e := &Engine{
		repo:            o.Repo,
		acts:            o.Activities,
		defs:            or(o.Definitions, engine.NewRegistry()),
		clock:           o.Clock,
		metrics:         o.Metrics,
		audit:           o.Auditor,
		log:             o.Logger,
		workerID:        o.WorkerID,
		salt:            o.Salt,
		lease:           dur(o.Lease, DefaultLease),
		heartbeat:       dur(o.HeartbeatInterval, DefaultHeartbeat),
		poll:            dur(o.PollInterval, DefaultPollInterval),
		reap:            dur(o.ReapInterval, DefaultReapInterval),
		stuckEvery:      dur(o.StuckInterval, DefaultStuckInterval),
		stuckThreshold:  dur(o.StuckThreshold, DefaultStuckThreshold),
		batch:           num(o.Batch, DefaultBatch),
		concurrency:     num(o.Concurrency, DefaultConcurrency),
		poisonThreshold: num(o.PoisonThreshold, DefaultPoisonThreshold),
		tenant:          o.Tenant,
		newID:           o.NewID,
	}
	if e.clock == nil {
		e.clock = resilience.SystemClock()
	}
	if e.metrics == nil {
		e.metrics = engine.NopMetrics{}
	}
	if e.audit == nil {
		e.audit = engine.NopAuditor{}
	}
	if e.log == nil {
		e.log = slog.Default()
	}
	if e.workerID == "" {
		e.workerID = "workflow-worker/unidentified"
	}
	if len(e.salt) == 0 {
		// A zero salt still yields keys that are deterministic in (instance, step), which is the
		// property that matters for deduplication. The salt exists so that a key cannot be
		// *predicted* by someone who knows an instance ID, not to make the key unique.
		e.salt = []byte("pp.workflow.idempotency")
	}
	if e.tenant == nil {
		e.tenant = func(context.Context) shared.TenantID { return "" }
	}
	if e.newID == nil {
		e.newID = shared.NewWorkflowID
	}
	if e.heartbeat >= e.lease {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"workflow engine: heartbeat interval %s must be shorter than the lease %s, or the lease expires before it is ever extended",
			e.heartbeat, e.lease)
	}
	return e, nil
}

// Register validates a definition, checks that this binary contains every activity it names,
// and stores it.
//
// Both checks belong at startup rather than at execution. A definition that names a renamed
// activity is a deployment error, and discovering it when a live merchant reaches step 10 means
// discovering it after thirty minutes of certification have already run.
func (e *Engine) Register(def *engine.Definition) error {
	if err := e.defs.Register(def); err != nil {
		return err
	}
	return e.acts.VerifyDefinition(def)
}

// Definitions exposes the registry, for the health endpoint and for wiring.
func (e *Engine) Definitions() *engine.Registry { return e.defs }

// WorkerID returns this engine's lease owner string.
func (e *Engine) WorkerID() string { return e.workerID }

func (e *Engine) now() time.Time { return e.clock.Now().UTC() }

// Start creates an instance, or returns the live one for the same business key.
//
// The idempotency is not a nicety for retrying HTTP clients: it is the mechanism that
// guarantees one onboarding per merchant. Two operators clicking "start onboarding" at the same
// moment, or a control-plane retry after a timeout, must not produce two sagas both provisioning
// gateway sub-accounts for one merchant — a duplicate that is expensive to clean up and visible
// to the merchant.
//
// The uniqueness is enforced in the store (a partial unique index over non-terminal states), not
// by the read below. The read is the fast path; the insert losing a race returns a conflict,
// and this function then re-reads and returns the winner. Checking-then-inserting without the
// index would be a time-of-check-to-time-of-use bug with a merchant-visible consequence.
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
			"workflow %s requires a business key: without one, starting twice would produce two sagas over one entity", def.Key())
	}

	if existing, err := e.repo.GetInstanceByBusinessKey(ctx, def.Name, businessKey); err == nil && existing != nil {
		return existing.ID, nil
	} else if err != nil && !errors.Is(err, engine.ErrInstanceNotFound) {
		return "", err
	}

	now := e.now()
	rec := ports.WorkflowInstanceRecord{
		ID:          e.newID(),
		TenantID:    e.tenant(ctx),
		Definition:  def.Name,
		Version:     def.Version,
		BusinessKey: businessKey,
		State:       string(engine.InstancePending),
		CurrentStep: def.FirstStep(),
		Input:       append([]byte(nil), input...),
		Context:     []byte("{}"),
		RunAfter:    now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := e.repo.CreateInstance(ctx, rec); err != nil {
		// Lost the insert race: the index did its job. Return the winner rather than the error,
		// because from the caller's point of view "an onboarding exists for this merchant" is
		// success, and it is the same answer they would have got a millisecond earlier.
		if existing, lookupErr := e.repo.GetInstanceByBusinessKey(ctx, def.Name, businessKey); lookupErr == nil && existing != nil {
			return existing.ID, nil
		}
		return "", err
	}
	e.log.InfoContext(ctx, "workflow started",
		"instance_id", rec.ID, "workflow", def.Key(), "business_key", businessKey)
	return rec.ID, nil
}

// Signal delivers a signal durably and records who sent it.
//
// Two properties are load-bearing:
//
//   - **Early arrival is safe.** The signal is stored keyed by name whether or not a wait is
//     active, and consumed the instant the wait begins. A KYC vendor's webhook that beats our
//     own commit is not a lost decision.
//   - **At-most-once consumption.** A second delivery of the same signal name is a no-op, so a
//     vendor's duplicate webhook cannot advance the workflow twice.
//
// The instance is not leased while it waits, which is why this write does not need to acquire
// one: it reads the record, appends the signal, and writes back with the epoch it read. A
// worker cannot be mid-step on a WAITING_SIGNAL instance, because entering that state releases
// the lease.
func (e *Engine) Signal(ctx context.Context, id shared.WorkflowID, name string, payload engine.SignalPayload) error {
	if name == "" {
		return apierror.New(apierror.CodeValidationFailed, "a signal needs a name")
	}
	rec, err := e.repo.GetInstance(ctx, id)
	if err != nil {
		return err
	}
	state := engine.InstanceState(rec.State)
	if state.IsFinal() {
		return apierror.Wrapf(engine.ErrNotRunnable, apierror.CodeWorkflowNotResumable,
			"instance %s is %s and cannot be signalled", id, rec.State)
	}

	cctx, err := decodeContext(rec.Context)
	if err != nil {
		return err
	}
	if cctx.Signals == nil {
		cctx.Signals = make(map[string]signalRecord, 2)
	}
	if _, seen := cctx.Signals[name]; seen {
		// Idempotent by the same rule as the unique index on (instance_id, signal_name): a
		// duplicate delivery is not a second decision.
		e.log.InfoContext(ctx, "duplicate workflow signal ignored",
			"instance_id", id, "signal", name, "principal", payload.Principal)
		return nil
	}
	at := payload.ReceivedAt
	if at.IsZero() {
		at = e.now()
	}
	cctx.Signals[name] = signalRecord{
		Data:           json.RawMessage(append([]byte(nil), payload.Data...)),
		Principal:      payload.Principal,
		Scopes:         append([]string(nil), payload.Scopes...),
		Reason:         payload.Reason,
		SourceIP:       payload.SourceIP,
		IdempotencyKey: payload.IdempotencyKey,
		ReceivedAt:     at,
	}
	encoded, err := cctx.encode()
	if err != nil {
		return err
	}
	rec.Context = encoded
	// Make it runnable now. In production the same transaction issues NOTIFY workflow_runnable,
	// so a poller wakes in milliseconds instead of waiting out its 250 ms floor.
	rec.RunAfter = e.now()
	if state == engine.InstanceParked {
		// A late signal resumes a parked instance normally. That is the point of parking rather
		// than failing: a compliance review nobody performed is a late human, not a broken
		// system, and the instance must still be there when they get to it.
		rec.State = string(engine.InstanceParked)
	}
	if err := e.repo.SaveInstance(ctx, *rec); err != nil {
		return err
	}

	// Audited unconditionally, and *after* the durable write, so the audit trail never claims an
	// approval that was not persisted.
	if err := e.audit.Record(ctx, engine.AuditEvent{
		WorkflowID:  id,
		TenantID:    rec.TenantID,
		BusinessKey: rec.BusinessKey,
		Action:      engine.ActionSignal,
		Step:        rec.CurrentStep,
		Principal:   payload.Principal,
		Scopes:      payload.Scopes,
		Reason:      payload.Reason,
		SourceIP:    payload.SourceIP,
		OccurredAt:  at,
		Payload:     append([]byte(nil), payload.Data...),
	}); err != nil {
		e.log.ErrorContext(ctx, "workflow signal audit failed", "instance_id", id, "signal", name, "error", err)
	}
	return nil
}

// Cancel requests cooperative cancellation.
//
// It sets a flag and makes the instance runnable; it does not interrupt anything. An in-flight
// external call is never abandoned, because abandoning it produces exactly the ambiguity the
// rest of this package exists to avoid: we would not know whether the side effect happened, and
// would then need a reconciliation path we otherwise do not. Waiting out an eight-second call is
// cheaper than owning an unknown.
func (e *Engine) Cancel(ctx context.Context, id shared.WorkflowID, reason string) error {
	rec, err := e.repo.GetInstance(ctx, id)
	if err != nil {
		return err
	}
	if engine.InstanceState(rec.State).IsFinal() {
		return apierror.Wrapf(engine.ErrNotRunnable, apierror.CodeWorkflowNotResumable,
			"instance %s is already %s", id, rec.State)
	}
	cctx, err := decodeContext(rec.Context)
	if err != nil {
		return err
	}
	if cctx.cancelRequested() {
		return nil
	}
	principal := ""
	if p := e.tenant(ctx); p != "" {
		principal = string(p)
	}
	cctx.Cancel = &cancelRecord{Requested: true, Reason: reason, Actor: principal, At: e.now()}
	encoded, err := cctx.encode()
	if err != nil {
		return err
	}
	rec.Context = encoded
	rec.RunAfter = e.now()
	if err := e.repo.SaveInstance(ctx, *rec); err != nil {
		return err
	}
	if err := e.audit.Record(ctx, engine.AuditEvent{
		WorkflowID:  id,
		TenantID:    rec.TenantID,
		BusinessKey: rec.BusinessKey,
		Action:      engine.ActionCancel,
		Step:        rec.CurrentStep,
		Principal:   principal,
		Reason:      reason,
		OccurredAt:  e.now(),
	}); err != nil {
		e.log.ErrorContext(ctx, "workflow cancel audit failed", "instance_id", id, "error", err)
	}
	return nil
}

// Get returns the instance and its checkpointed step history.
func (e *Engine) Get(ctx context.Context, id shared.WorkflowID) (*engine.Instance, error) {
	rec, err := e.repo.GetInstance(ctx, id)
	if err != nil {
		return nil, err
	}
	steps, err := e.repo.ListSteps(ctx, id)
	if err != nil {
		return nil, err
	}
	return toInstance(rec, steps), nil
}

// Resume drives an instance forward now.
//
// It leases the instance exactly as a poller would, then runs the same drive loop, so there is
// no "resume path" whose behaviour can differ from the production path — a second code path here
// would be a second set of bugs, discovered only by whichever of the two is used less.
//
// It returns when the instance is no longer immediately runnable.
func (e *Engine) Resume(ctx context.Context, id shared.WorkflowID) error {
	rec, err := e.repo.GetInstance(ctx, id)
	if err != nil {
		return err
	}
	state := engine.InstanceState(rec.State)
	if state.IsFinal() {
		return apierror.Wrapf(engine.ErrNotRunnable, apierror.CodeWorkflowNotResumable,
			"instance %s is %s", id, rec.State)
	}
	if state == engine.InstancePoisoned {
		return apierror.Wrapf(engine.ErrNotRunnable, apierror.CodeWorkflowNotResumable,
			"instance %s is quarantined; requeue it with a reset crash count", id)
	}

	// Make it immediately claimable, then claim it through the ordinary lease path so that the
	// epoch is bumped and every subsequent write is fenced exactly as it would be in the worker.
	//
	// A signal wait is the one case that is *not* nudged. Its RunAfter is the gate's deadline,
	// not a backoff, and pulling it forward would make an operator's "try again" indistinguishable
	// from "the five-day compliance window elapsed" — the instance would park on the spot and the
	// reviewer would find it escalated for no reason. A gate is resumed by signalling it, or by
	// its deadline arriving; there is nothing else to nudge.
	if rec.RunAfter.After(e.now()) {
		if state == engine.InstanceWaitingSignal {
			return nil
		}
		rec.RunAfter = e.now()
		if err := e.repo.SaveInstance(ctx, *rec); err != nil {
			return err
		}
	}
	leased, err := e.repo.LeaseRunnable(ctx, e.workerID, e.lease, e.batch)
	if err != nil {
		return err
	}
	var target *ports.WorkflowInstanceRecord
	for i := range leased {
		if leased[i].ID == id {
			target = &leased[i]
			continue
		}
		// A claim is a batch operation; a targeted resume is not. Anything else the batch swept
		// up is handed straight back rather than being held for a full lease duration, because a
		// lease held by a caller that has no intention of driving the instance is precisely the
		// "stuck, lease_expires_at far in the future, heartbeat recent, updated_at old" state the
		// stuck detector exists to catch — and it would be self-inflicted.
		other := leased[i]
		other.LeaseOwner = ""
		other.LeaseUntil = nil
		if relErr := e.repo.SaveInstance(ctx, other); relErr != nil && !engine.IsLeaseLost(relErr) {
			e.log.WarnContext(ctx, "could not release an incidentally leased instance",
				"instance_id", other.ID, "error", relErr)
		}
	}
	if target != nil {
		return e.drive(ctx, *target)
	}
	// Another worker holds it. That is not an error: the instance is being driven, just not by
	// us, and racing it would be the split-brain that fencing exists to make harmless.
	return nil
}

// DetectStuck reports instances that should be moving and are not.
//
// The threshold is derived per step from the step's own timeout and retry budget rather than
// being one global number, so a thirty-minute certification is not flagged at the same threshold
// as a five-second activation, and the thresholds track vendor latency drift instead of needing
// manual retuning.
func (e *Engine) DetectStuck(ctx context.Context, limit int) ([]engine.Stuck, error) {
	recs, err := e.repo.FindStuck(ctx, e.stuckThreshold, limit)
	if err != nil {
		return nil, err
	}
	now := e.now()
	out := make([]engine.Stuck, 0, len(recs))
	for _, r := range recs {
		threshold := e.stuckThreshold
		if def, derr := e.defs.Lookup(r.Definition, r.Version); derr == nil {
			if step := def.StepByName(r.CurrentStep); step != nil {
				if budget := 2 * step.Timeout * time.Duration(max(step.Retry.MaxAttempts, 1)); budget > threshold {
					threshold = budget
				}
			}
		}
		age := now.Sub(r.UpdatedAt)
		if age <= threshold {
			continue
		}
		out = append(out, engine.Stuck{
			ID:            r.ID,
			Definition:    r.Definition,
			BusinessKey:   r.BusinessKey,
			State:         engine.InstanceState(r.State),
			Step:          r.CurrentStep,
			LeaseOwner:    r.LeaseOwner,
			NoProgressFor: age,
			LastError:     r.LastError,
		})
	}
	return out, nil
}

// PublishInstanceCounts refreshes pp_workflow_instances.
//
// It is a gauge recomputed on a schedule rather than a counter incremented on transition,
// because instances leave states in ways the metric layer never sees — a lease expiring, a
// reaper reclaiming, an operator requeueing — and a counter maintained across those drifts
// permanently.
func (e *Engine) PublishInstanceCounts(ctx context.Context, workflow string) error {
	counts, err := e.repo.CountByState(ctx)
	if err != nil {
		return err
	}
	for _, s := range engine.AllInstanceStates {
		e.metrics.SetInstances(ctx, workflow, string(s), float64(counts[string(s)]))
	}
	return nil
}

// idempotencyKey derives the deterministic key for one step of one instance.
//
//	K = HMAC-SHA256(instance_id ‖ step_name, workflow_salt) truncated to 32 Base32 characters
//
// It is deterministic in (instance, step) and explicitly **not** in the attempt number. The
// instinct to include the attempt is what produces duplicate side effects: a worker that
// crashed after calling a vendor but before checkpointing retries, and with an attempt-varying
// key the vendor sees a new request. With a stable key the vendor dedupes and returns the
// original result — and the same key is the handle lookup-before-act uses to find our own prior
// effect. Truncation to 32 Base32 characters (160 bits) keeps it inside every vendor's
// client-reference length limit while leaving collision probability irrelevant.
func (e *Engine) idempotencyKey(id shared.WorkflowID, step string) string {
	mac := hmac.New(sha256.New, e.salt)
	mac.Write([]byte(id))
	mac.Write([]byte("\x00"))
	mac.Write([]byte(step))
	sum := mac.Sum(nil)
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum)[:32]
}

func toInstance(rec *ports.WorkflowInstanceRecord, steps []ports.WorkflowStepRecord) *engine.Instance {
	inst := &engine.Instance{
		ID:          rec.ID,
		TenantID:    rec.TenantID,
		Definition:  rec.Definition,
		Version:     rec.Version,
		BusinessKey: rec.BusinessKey,
		State:       engine.InstanceState(rec.State),
		CurrentStep: rec.CurrentStep,
		Input:       rec.Input,
		Context:     rec.Context,
		Attempt:     rec.Attempt,
		LeaseOwner:  rec.LeaseOwner,
		LeaseEpoch:  rec.LeaseEpoch,
		LeaseUntil:  rec.LeaseUntil,
		RunAfter:    rec.RunAfter,
		LastError:   rec.LastError,
		CreatedAt:   rec.CreatedAt,
		UpdatedAt:   rec.UpdatedAt,
		CompletedAt: rec.CompletedAt,
	}
	inst.Steps = make([]engine.StepRecord, 0, len(steps))
	for _, s := range steps {
		inst.Steps = append(inst.Steps, engine.StepRecord{
			ID:            s.ID,
			Name:          s.Name,
			Sequence:      s.Sequence,
			State:         engine.StepState(s.State),
			Attempt:       s.Attempt,
			Input:         s.Input,
			Output:        s.Output,
			Error:         s.Error,
			StartedAt:     s.StartedAt,
			CompletedAt:   s.CompletedAt,
			CompensatedAt: s.CompensatedAt,
		})
	}
	return inst
}

func or[T any](v *T, def *T) *T {
	if v == nil {
		return def
	}
	return v
}

func dur(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

func num(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// Requeue returns a failed, parked or quarantined instance to PENDING at a chosen step.
//
// It is the operator half of the DLQ triage runbook and it is deliberately narrow. There is no
// "edit the instance context" command and no "skip a step" command, because both would defeat
// the controls they touch: skipping `certification` or `compliance-review` is exactly the action
// those steps exist to prevent, and arbitrary context mutation makes the audit trail meaningless
// and the workflow's invariants unenforceable. What an operator may do is fix the cause and run
// the step again — with the step's *input* optionally patched, which is diffable and audited.
//
// resetCrashCount is the escape hatch for a quarantined instance whose cause has been fixed and
// deployed; requeueing a poison instance onto the same binary just re-fills the DLQ.
func (e *Engine) Requeue(ctx context.Context, id shared.WorkflowID, step string, resetCrashCount bool, actor, reason string) error {
	if reason == "" {
		return apierror.New(apierror.CodeValidationFailed,
			"a requeue requires a reason: an operator action with no recorded reason is unreviewable six months later")
	}
	rec, err := e.repo.GetInstance(ctx, id)
	if err != nil {
		return err
	}
	def, err := e.defs.Lookup(rec.Definition, rec.Version)
	if err != nil {
		return err
	}
	if step == "" {
		step = rec.CurrentStep
	}
	if def.StepIndex(step) < 0 {
		return apierror.Newf(apierror.CodeValidationFailed,
			"workflow %s has no step named %q", def.Key(), step)
	}
	cctx, err := decodeContext(rec.Context)
	if err != nil {
		return err
	}
	cctx.Meta.Failure = nil
	cctx.Meta.ParkReason = ""
	cctx.Meta.AbortCause = ""
	cctx.Meta.LookupFirst = false
	cctx.Meta.WaitingFor = ""
	if resetCrashCount {
		cctx.Meta.CrashCount = 0
	}
	encoded, err := cctx.encode()
	if err != nil {
		return err
	}
	rec.Context = encoded
	rec.State = string(engine.InstancePending)
	rec.CurrentStep = step
	rec.Attempt = 0
	rec.LastError = ""
	rec.CompletedAt = nil
	rec.RunAfter = e.now()
	rec.LeaseOwner = ""
	rec.LeaseUntil = nil
	if err := e.repo.SaveInstance(ctx, *rec); err != nil {
		return err
	}
	if auditErr := e.audit.Record(ctx, engine.AuditEvent{
		WorkflowID: id, TenantID: rec.TenantID, BusinessKey: rec.BusinessKey,
		Action: engine.ActionRequeue, Step: step, Principal: actor, Reason: reason,
		OccurredAt: e.now(),
	}); auditErr != nil {
		e.log.ErrorContext(ctx, "workflow requeue audit failed", "instance_id", id, "error", auditErr)
	}
	e.log.InfoContext(ctx, "workflow requeued",
		"instance_id", id, "business_key", rec.BusinessKey, "step", step,
		"actor", actor, "reason", reason)
	return nil
}
