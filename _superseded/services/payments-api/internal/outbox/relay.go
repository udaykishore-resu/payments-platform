// Package outbox implements the relay half of the transactional outbox pattern (ADR-004): it
// polls the outbox_events table for unpublished rows and publishes them to the messaging fabric,
// closing the loop opened by repository.Repository.CreatePayment's atomic outbox write.
package outbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/example/payments-platform/services/payments-api/internal/domain"
	"github.com/example/payments-platform/services/payments-api/internal/events"
	"github.com/example/payments-platform/services/payments-api/internal/observability"
)

// Repository is the narrow persistence port the relay depends on.
type Repository interface {
	ClaimOutboxBatch(ctx context.Context, limit int) ([]domain.OutboxEvent, error)
	MarkOutboxPublished(ctx context.Context, id string) error
	UnpublishedOutboxCount(ctx context.Context) (int64, error)
}

type Relay struct {
	repo         Repository
	publisher    events.Publisher
	metrics      *observability.Metrics
	logger       *slog.Logger
	pollInterval time.Duration
	batchSize    int
	maxAttempts  int
}

func NewRelay(repo Repository, publisher events.Publisher, metrics *observability.Metrics, logger *slog.Logger, pollInterval time.Duration, batchSize, maxAttempts int) *Relay {
	return &Relay{
		repo:         repo,
		publisher:    publisher,
		metrics:      metrics,
		logger:       logger,
		pollInterval: pollInterval,
		batchSize:    batchSize,
		maxAttempts:  maxAttempts,
	}
}

// Run polls until ctx is cancelled (wired to the process's graceful-shutdown context in
// cmd/server/main.go). Every pod runs its own Relay goroutine; concurrent relays across pods
// safely compete for rows via `FOR UPDATE SKIP LOCKED` in the repository layer (ADR-003) — there
// is deliberately no leader election here (see docs/04-failure-recovery-design.md).
func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.logger.Info("outbox relay stopping")
			return
		case <-ticker.C:
			r.pollOnce(ctx)
		}
	}
}

func (r *Relay) pollOnce(ctx context.Context) {
	// Named `claimed`, not `events`, to avoid shadowing the imported `events` package within
	// this function — purely a readability choice, both compile fine since Go scopes shadowing
	// to the block it occurs in.
	claimed, err := r.repo.ClaimOutboxBatch(ctx, r.batchSize)
	if err != nil {
		r.logger.ErrorContext(ctx, "outbox relay: claim batch failed", "error", err)
		return
	}

	for _, e := range claimed {
		r.publishOne(ctx, e)
	}

	if r.metrics != nil {
		if count, err := r.repo.UnpublishedOutboxCount(ctx); err == nil {
			r.metrics.OutboxUnpublishedCount.Set(float64(count))
		}
	}
}

func (r *Relay) publishOne(ctx context.Context, e domain.OutboxEvent) {
	if e.Attempts > r.maxAttempts {
		// Past the retry budget. Left unpublished deliberately (not dropped) — the
		// DLQDepthNonZero-equivalent signal for the outbox itself is
		// `outbox_unpublished_count` staying nonzero combined with high `attempts`; an operator
		// investigates via docs/08-runbook.md section 4/5 rather than the event silently
		// vanishing. A production system would additionally move this to a dedicated
		// `outbox_events_failed` table after N attempts to keep the hot polling query fast;
		// omitted here for scope.
		r.logger.ErrorContext(ctx, "outbox event exceeded max publish attempts",
			"event_id", e.ID, "payment_id", e.AggregateID, "attempts", e.Attempts)
		return
	}

	start := time.Now()
	err := r.publisher.Publish(ctx, e.EventType, e.Payload)
	if err != nil {
		r.logger.WarnContext(ctx, "outbox event publish failed, will retry on next poll",
			"event_id", e.ID, "payment_id", e.AggregateID, "attempts", e.Attempts, "error", err)
		return
	}

	if err := r.repo.MarkOutboxPublished(ctx, e.ID); err != nil {
		r.logger.ErrorContext(ctx, "outbox event published but failed to mark published — will republish (safe, consumers are idempotent)",
			"event_id", e.ID, "error", err)
		return
	}

	if r.metrics != nil {
		r.metrics.OutboxPublishLatency.Observe(time.Since(start).Seconds())
	}
	r.logger.InfoContext(ctx, "outbox event published", "event_id", e.ID, "payment_id", e.AggregateID, "event_type", e.EventType)
}
