package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/internal/platform/runtime"
)

// relayDeps is the relay loop's dependency set, named explicitly rather than assembled by a
// container — the same rule as main.go, for the same reason (ADR-023).
type relayDeps struct {
	UoW         ports.UnitOfWork
	Publisher   ports.EventPublisher
	Metrics     *telemetry.Registry
	Logger      *slog.Logger
	Shard       int
	TotalShards int
	Batch       int
	Interval    time.Duration
}

// relay is the claim-publish-mark loop.
//
// # Why the loop lives here rather than in internal/application
//
// It is not a use case: it makes no business decision, validates nothing, and has no domain
// vocabulary. It is a transport concern — move rows to a broker — parameterised by a shard. Putting
// it in the application layer would give that layer a Kafka dependency it has no other reason to
// hold, and putting it in main.go would put a `for` loop and a goroutine in a file the architecture
// check requires to be a flat list of constructor calls.
//
// # Why the order is claim → publish → mark and never claim → mark → publish
//
// Marking first would lose an event whenever the publish that followed it failed: the row is gone,
// the broker never saw it, and nothing will ever notice. Publishing first risks a *duplicate* when
// the mark fails — the row is reclaimed and published again — and a duplicate is what the
// consumers' dedup store exists to absorb. At-least-once with dedup is recoverable; at-most-once
// with silent loss is not.
type relay struct {
	deps relayDeps

	mu      sync.Mutex
	stop    context.CancelFunc
	stopped chan struct{}
}

func newRelay(d relayDeps) *relay {
	if d.Batch <= 0 {
		d.Batch = 100
	}
	if d.Interval <= 0 {
		d.Interval = time.Second
	}
	return &relay{deps: d, stopped: make(chan struct{})}
}

// Start begins the loop in a goroutine this type owns.
func (r *relay) Start(ctx context.Context) error {
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.mu.Lock()
	r.stop = cancel
	r.mu.Unlock()

	go func() {
		defer close(r.stopped)
		_ = runtime.Guard("outbox-relay", r.deps.Logger, func() error {
			r.loop(loopCtx)
			return nil
		})
	}()
	return nil
}

// Stop cancels the loop and waits for the current batch to finish, bounded by ctx.
//
// Bounded, because an unbounded wait on a batch blocked against an unreachable broker is a pod
// that Kubernetes eventually SIGKILLs — which abandons the batch anyway, having first consumed the
// whole grace period. Returning at the deadline leaves the rows unmarked, which is the safe state:
// another replica reclaims them.
func (r *relay) Stop(ctx context.Context) error {
	r.mu.Lock()
	cancel := r.stop
	r.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-r.stopped:
		return nil
	case <-ctx.Done():
		r.deps.Logger.Warn("outbox relay did not finish its batch within the budget; " +
			"the rows stay unmarked and another replica will reclaim them")
		return nil
	}
}

// loop polls until cancelled. A batch that returned a full page is followed immediately by
// another: sleeping between full batches would cap throughput at batch ÷ interval regardless of
// backlog, which is exactly the wrong behaviour during the incident that created the backlog.
func (r *relay) loop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		published := r.drainOnce(ctx)
		delay := r.deps.Interval
		if published >= r.deps.Batch {
			delay = 0
		}
		timer.Reset(delay)
	}
}

// drainOnce claims one batch, publishes it and marks it, returning how many were published.
func (r *relay) drainOnce(ctx context.Context) int {
	var claimed []ports.OutboxMessage
	err := r.deps.UoW.Within(ctx, func(ctx context.Context, repo ports.Repositories) error {
		reader, ok := repo.Outbox.(ports.OutboxReader)
		if !ok {
			return nil
		}
		batch, err := reader.Claim(ctx, r.deps.Shard, r.deps.TotalShards, r.deps.Batch)
		if err != nil {
			return err
		}
		claimed = batch
		if len(batch) == 0 {
			return nil
		}
		if err := r.deps.Publisher.Publish(ctx, batch...); err != nil {
			// Returning the error rolls back the claim, so the rows are immediately available to
			// this replica's next pass or to another. Marking them failed here instead would
			// impose a retry delay on a broker blip that may already have cleared.
			return err
		}
		ids := make([]shared.EventID, 0, len(batch))
		for _, m := range batch {
			ids = append(ids, m.ID)
		}
		return reader.MarkPublished(ctx, ids)
	})
	if err != nil {
		r.deps.Logger.Error("outbox batch failed",
			slog.String(telemetry.KeyErrorMessage, err.Error()),
			slog.Int(telemetry.KeyCount, len(claimed)))
		return 0
	}
	return len(claimed)
}
