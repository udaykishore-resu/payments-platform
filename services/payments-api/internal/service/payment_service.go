// Package service contains the transport-agnostic business logic. It depends on the domain
// package and a narrow Repository interface (not the concrete pgx type), so it can be unit
// tested with a fake in-memory repository with no database at all — see tests/unit.
package service

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"time"

	"github.com/example/payments-platform/services/payments-api/internal/domain"
	"github.com/example/payments-platform/services/payments-api/internal/observability"
	"github.com/example/payments-platform/services/payments-api/internal/repository"
)

// Repository is the narrow persistence port the service depends on. The concrete implementation
// is internal/repository.Repository (pgx-backed); tests substitute a fake.
type Repository interface {
	CreatePayment(ctx context.Context, in domain.CreatePaymentInput) (repository.CreatePaymentResult, error)
	GetPayment(ctx context.Context, id string) (domain.Payment, error)
}

const (
	maxSerializationRetries = 4
	baseRetryBackoff        = 10 * time.Millisecond
)

type PaymentService struct {
	repo    Repository
	metrics *observability.Metrics
	logger  *slog.Logger
}

func NewPaymentService(repo Repository, metrics *observability.Metrics, logger *slog.Logger) *PaymentService {
	return &PaymentService{repo: repo, metrics: metrics, logger: logger}
}

// Create executes a payment. It retries automatically, with jittered exponential backoff, on
// SERIALIZABLE conflicts (SQLSTATE 40001) and on the benign "lost the idempotency-insert race"
// case — both are expected outcomes of correct concurrent operation, not bugs (see ADR-004 and
// docs/04-failure-recovery-design.md). All other errors are returned immediately to the caller.
func (s *PaymentService) Create(ctx context.Context, in domain.CreatePaymentInput) (domain.Payment, bool, error) {
	if err := in.Validate(); err != nil {
		return domain.Payment{}, false, err
	}

	var lastErr error
	for attempt := 0; attempt <= maxSerializationRetries; attempt++ {
		if attempt > 0 {
			backoff := jitteredBackoff(attempt)
			s.logger.WarnContext(ctx, "retrying payment creation after conflict",
				"attempt", attempt, "backoff_ms", backoff.Milliseconds())
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return domain.Payment{}, false, ctx.Err()
			}
		}

		result, err := s.repo.CreatePayment(ctx, in)
		if err == nil {
			if s.metrics != nil {
				s.metrics.PaymentsCreatedTotal.WithLabelValues(string(result.Payment.Status)).Inc()
			}
			return result.Payment, result.IdempotentHit, nil
		}

		lastErr = err
		if repository.IsSerializationFailure(err) || errors.Is(err, domain.ErrConcurrentModification) {
			continue // retryable, loop again
		}
		// Non-retryable domain/validation error — fail fast.
		return domain.Payment{}, false, err
	}

	s.logger.ErrorContext(ctx, "payment creation exhausted retries", "error", lastErr)
	return domain.Payment{}, false, lastErr
}

func (s *PaymentService) Get(ctx context.Context, id string) (domain.Payment, error) {
	if id == "" {
		return domain.Payment{}, domain.ErrInvalidRequest
	}
	return s.repo.GetPayment(ctx, id)
}

func jitteredBackoff(attempt int) time.Duration {
	base := baseRetryBackoff * time.Duration(1<<uint(attempt))
	jitter := time.Duration(rand.Int63n(int64(base)))
	return base + jitter
}
