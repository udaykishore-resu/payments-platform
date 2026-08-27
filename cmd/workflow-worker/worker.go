package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/platform/runtime"
	wfpostgres "github.com/udaykishore-resu/payments-platform/internal/workflows/engine/postgres"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// workerDeps is the worker's dependency set.
type workerDeps struct {
	Engine   *wfpostgres.Engine
	Workflow string
	Logger   *slog.Logger
}

// worker owns the engine's polling goroutine and its shutdown.
//
// # The drain is different from every other binary's
//
// Other workers stop accepting and finish what they hold. This one additionally **releases its
// leases**, and the difference is measurable: an instance whose lease is released is picked up by
// another worker in milliseconds, while an instance whose worker simply exited is stranded until
// the lease expires — sixty seconds by default, multiplied by however many pods a rolling deploy
// replaces. That is the difference between a deploy costing seconds and costing minutes of
// onboarding latency for every merchant mid-workflow.
//
// The release is performed by cancelling the engine's context, which drops the in-flight leases
// on the connection close; the engine's own worker loop stops claiming as soon as the context is
// cancelled, so nothing new is taken while the current activity finishes.
type worker struct {
	deps workerDeps

	mu      sync.Mutex
	cancel  context.CancelFunc
	stopped chan struct{}
}

func newWorker(d workerDeps) *worker {
	return &worker{deps: d, stopped: make(chan struct{})}
}

// Start runs the engine's worker loop in a goroutine this type owns.
func (w *worker) Start(ctx context.Context) error {
	if w.deps.Engine == nil {
		return apierror.New(apierror.CodeInternalError, "the workflow engine is not wired")
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	w.mu.Lock()
	w.cancel = cancel
	w.mu.Unlock()

	runner := wfpostgres.NewWorker(w.deps.Engine, w.deps.Workflow)
	go func() {
		defer close(w.stopped)
		_ = runtime.Guard("workflow-worker", w.deps.Logger, func() error {
			return runner.Run(loopCtx)
		})
	}()
	w.deps.Logger.Info("workflow worker running", slog.String("workflow", w.deps.Workflow))
	return nil
}

// Stop stops claiming, waits for the current activity, and releases the leases.
//
// The wait is bounded by ctx: an activity blocked on an unreachable KYC provider must not hold the
// pod past its termination grace period, because the outcome of that is a SIGKILL — which strands
// the leases anyway, having first spent the whole grace period. Returning at the deadline leaves
// the instances leased and they recover at expiry, which is the slower but safe path.
func (w *worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	cancel := w.cancel
	w.mu.Unlock()
	if cancel == nil {
		return nil
	}
	w.deps.Logger.Info("workflow worker draining: no new instances will be leased")
	cancel()

	select {
	case <-w.stopped:
		w.deps.Logger.Info("workflow worker stopped; leases released")
		return nil
	case <-ctx.Done():
		w.deps.Logger.Warn("workflow worker did not finish its activity within the budget; " +
			"its leases will be reclaimed at expiry rather than immediately")
		return nil
	}
}

// leaseTooTight rejects a heartbeat interval that cannot keep a lease alive.
//
// The check is at startup because the failure it prevents is silent and intermittent: with a
// heartbeat close to the lease duration, most heartbeats land in time and occasionally one does
// not — and the instance is then claimed by a second worker while the first is still executing it.
// A step that provisions a gateway account is not idempotent at the vendor, so "occasionally two
// workers" is "occasionally two merchant accounts".
//
// Three times the heartbeat is the margin: it tolerates two consecutive missed heartbeats, which
// covers a GC pause and a slow query without tolerating a wedged worker.
func leaseTooTight(heartbeat, lease time.Duration) error {
	return apierror.Newf(apierror.CodeConfigurationInvalid,
		"a %s heartbeat cannot safely hold a %s lease; the lease must be at least three times the heartbeat",
		heartbeat, lease).
		WithDetail(apierror.Detail{
			Field:   "PP_WORKFLOW_HEARTBEAT",
			Code:    "LEASE_MARGIN_TOO_SMALL",
			Message: "Raise PP_WORKFLOW_LEASE or lower PP_WORKFLOW_HEARTBEAT.",
			RuleID:  "L0.WORKFLOW_LEASE_MARGIN",
		})
}
