package postgres

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
)

// Worker is the poll-lease-execute loop that drives instances in production.
//
// Goroutine ownership is explicit and bounded, per docs/lld.md §2.5: the poller, the stuck
// detector and the instance-count publisher are one goroutine each and are owned by the Worker;
// activity execution runs inside an errgroup with a fixed limit; the heartbeater is one
// goroutine per *in-flight step*, created and joined inside the step that needs it. Run does not
// return until every one of them has exited, which is what makes "no goroutine leaks" a property
// the shutdown path enforces rather than a claim in a comment.
//
// **Backpressure is the lease model itself.** When every slot is busy the poller claims nothing,
// and unclaimed instances stay in the table with RunAfter in the past — visible, durable and
// countable. There is no in-memory queue to overflow, no work to lose on a crash, and no
// unbounded growth: the "queue" is a table with a partial index, and its depth is a first-class
// metric that the autoscaler reads.
type Worker struct {
	engine *Engine

	// Workflow names the definition this worker's gauges are published under.
	Workflow string

	mu      sync.Mutex
	running bool
}

// NewWorker returns a worker driving e.
func NewWorker(e *Engine, workflow string) *Worker {
	return &Worker{engine: e, Workflow: workflow}
}

// Run polls until ctx is cancelled, then finishes the steps already in flight and returns.
//
// Shutdown is deliberately *not* immediate. An activity is never abandoned mid-call: abandoning
// one produces exactly the ambiguity the whole engine is built to avoid — we would not know
// whether the vendor acted. The shutdown grace budget (120 s) is more than twice the lease (60 s)
// precisely so that finishing the current step and releasing the lease explicitly both fit
// inside it, which is what makes a rolling deploy cost ~0 s of takeover latency instead of up to
// 70 s per instance.
func (w *Worker) Run(ctx context.Context) error {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return nil
	}
	w.running = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	e := w.engine
	g, gctx := errgroup.WithContext(ctx)

	// Activity dispatch is bounded separately from the supervisory goroutines, so a saturated
	// dispatcher cannot starve the stuck detector — the one component whose whole job is to
	// notice that nothing is moving.
	dispatch := &errgroup.Group{}
	dispatch.SetLimit(e.concurrency)

	g.Go(func() error {
		ticker := time.NewTicker(e.poll)
		defer ticker.Stop()
		for {
			select {
			case <-gctx.Done():
				// Wait for in-flight steps before returning: their leases are released by the
				// drive loop's own shutdown path, and returning first would leave them writing
				// against a worker that no longer exists.
				_ = dispatch.Wait()
				return nil
			case <-ticker.C:
				w.pollOnce(gctx, dispatch)
			}
		}
	})

	g.Go(func() error {
		ticker := time.NewTicker(e.stuckEvery)
		defer ticker.Stop()
		for {
			select {
			case <-gctx.Done():
				return nil
			case <-ticker.C:
				stuck, err := e.DetectStuck(gctx, 100)
				if err != nil {
					e.log.ErrorContext(gctx, "stuck sweep failed", "error", err)
					continue
				}
				for _, s := range stuck {
					// The alert carries a pod name and a step, not a number: the responder
					// should start from "restart this pod" rather than from a query.
					e.log.WarnContext(gctx, "workflow instance is stuck",
						"instance_id", s.ID, "business_key", s.BusinessKey, "step", s.Step,
						"state", string(s.State), "lease_owner", s.LeaseOwner,
						"no_progress_for", s.NoProgressFor, "last_error", s.LastError)
				}
			}
		}
	})

	g.Go(func() error {
		ticker := time.NewTicker(e.reap)
		defer ticker.Stop()
		for {
			select {
			case <-gctx.Done():
				return nil
			case <-ticker.C:
				if err := e.PublishInstanceCounts(gctx, w.Workflow); err != nil {
					e.log.ErrorContext(gctx, "instance count publication failed", "error", err)
				}
			}
		}
	})

	return g.Wait()
}

// pollOnce claims a batch and dispatches it.
//
// `LeaseRunnable` uses FOR UPDATE SKIP LOCKED in the real store: row-level work distribution
// with no coordinator, no leader election and no partition assignment. Concurrent workers never
// block each other and each takes a disjoint set — and the epoch bump on every acquisition is
// what makes a worker that wakes up after its lease expired harmless rather than merely unlikely.
func (w *Worker) pollOnce(ctx context.Context, dispatch *errgroup.Group) {
	e := w.engine
	leased, err := e.repo.LeaseRunnable(ctx, e.workerID, e.lease, e.batch)
	if err != nil {
		e.log.ErrorContext(ctx, "lease acquisition failed", "worker_id", e.workerID, "error", err)
		return
	}
	for _, rec := range leased {

		dispatch.Go(func() error {
			// The drive loop's errors are operational, not fatal: one instance failing to write
			// must not take the worker down, because the worker is what would otherwise retry it.
			if driveErr := e.drive(ctx, rec); driveErr != nil && !engine.IsLeaseLost(driveErr) {
				e.log.ErrorContext(ctx, "workflow drive failed",
					"instance_id", rec.ID, "business_key", rec.BusinessKey,
					"lease_epoch", rec.LeaseEpoch, "worker_id", e.workerID, "error", driveErr)
			}
			return nil
		})
	}
}

// DriveOnce claims and drives at most one batch synchronously.
//
// It exists for tests and for `platformctl workflow reschedule`, and it runs the same drive loop
// the poller does. A second implementation of "advance an instance" would be a second set of
// bugs, found only by whichever of the two is exercised less.
func (w *Worker) DriveOnce(ctx context.Context) (int, error) {
	e := w.engine
	leased, err := e.repo.LeaseRunnable(ctx, e.workerID, e.lease, e.batch)
	if err != nil {
		return 0, err
	}
	for _, rec := range leased {
		if driveErr := e.drive(ctx, rec); driveErr != nil && !engine.IsLeaseLost(driveErr) {
			return len(leased), driveErr
		}
	}
	return len(leased), nil
}
