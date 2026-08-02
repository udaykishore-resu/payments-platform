package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/example/payments-platform/services/payments-api/internal/domain"
	"github.com/example/payments-platform/services/payments-api/internal/observability"
	"github.com/example/payments-platform/services/payments-api/internal/repository"
)

// fakeRepository lets us test PaymentService's orchestration logic (validation, retry-on-conflict
// behavior) without a real database — see docs/09-production-checklist.md, which requires the
// domain/service layer to be unit-testable in isolation.
type fakeRepository struct {
	calls   int32
	results []fakeResult
}

type fakeResult struct {
	result repository.CreatePaymentResult
	err    error
}

func (f *fakeRepository) CreatePayment(ctx context.Context, in domain.CreatePaymentInput) (repository.CreatePaymentResult, error) {
	i := atomic.AddInt32(&f.calls, 1) - 1
	if int(i) >= len(f.results) {
		return repository.CreatePaymentResult{}, errors.New("fakeRepository: ran out of scripted results")
	}
	r := f.results[i]
	return r.result, r.err
}

func (f *fakeRepository) GetPayment(ctx context.Context, id string) (domain.Payment, error) {
	return domain.Payment{ID: id, Status: domain.PaymentStatusCompleted}, nil
}

func serializationFailureErr() error {
	return &pgconn.PgError{Code: "40001", Message: "could not serialize access due to concurrent update"}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testMetrics() *observability.Metrics {
	return observability.NewMetrics(prometheus.NewRegistry())
}

func validInput() domain.CreatePaymentInput {
	return domain.CreatePaymentInput{
		IdempotencyKey:  "idem-1",
		RequestHash:     "hash-1",
		SourceAccountID: "acct-src",
		DestAccountID:   "acct-dst",
		Amount:          domain.Money{AmountMinor: 500, Currency: "USD"},
		ClientID:        "client-1",
	}
}

func TestPaymentService_Create_ValidationFailsFast(t *testing.T) {
	repo := &fakeRepository{} // no scripted results — must never be called
	svc := NewPaymentService(repo, testMetrics(), testLogger())

	in := validInput()
	in.Amount = domain.Money{AmountMinor: 0, Currency: "USD"} // invalid

	_, _, err := svc.Create(context.Background(), in)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if atomic.LoadInt32(&repo.calls) != 0 {
		t.Fatalf("repository should not be called for invalid input, got %d calls", repo.calls)
	}
}

func TestPaymentService_Create_SuccessFirstTry(t *testing.T) {
	repo := &fakeRepository{
		results: []fakeResult{
			{result: repository.CreatePaymentResult{Payment: domain.Payment{ID: "pay-1", Status: domain.PaymentStatusCompleted}}},
		},
	}
	svc := NewPaymentService(repo, testMetrics(), testLogger())

	payment, idempotentHit, err := svc.Create(context.Background(), validInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idempotentHit {
		t.Fatal("expected idempotentHit=false on first creation")
	}
	if payment.ID != "pay-1" {
		t.Fatalf("expected payment ID pay-1, got %s", payment.ID)
	}
	if repo.calls != 1 {
		t.Fatalf("expected exactly 1 repository call, got %d", repo.calls)
	}
}

// TestPaymentService_Create_RetriesOnSerializationConflict is the test that validates the core
// claim in ADR-004: a SERIALIZABLE conflict (expected, benign, concurrent-access artifact) is
// retried automatically rather than surfaced to the caller as a failure.
func TestPaymentService_Create_RetriesOnSerializationConflict(t *testing.T) {
	repo := &fakeRepository{
		results: []fakeResult{
			{err: serializationFailureErr()},
			{err: serializationFailureErr()},
			{result: repository.CreatePaymentResult{Payment: domain.Payment{ID: "pay-2", Status: domain.PaymentStatusCompleted}}},
		},
	}
	svc := NewPaymentService(repo, testMetrics(), testLogger())

	start := time.Now()
	payment, _, err := svc.Create(context.Background(), validInput())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected eventual success after retries, got error: %v", err)
	}
	if payment.ID != "pay-2" {
		t.Fatalf("expected payment ID pay-2, got %s", payment.ID)
	}
	if repo.calls != 3 {
		t.Fatalf("expected 3 repository calls (2 failures + 1 success), got %d", repo.calls)
	}
	if elapsed <= 0 {
		t.Fatalf("expected backoff to introduce measurable delay")
	}
}

func TestPaymentService_Create_NonRetryableErrorFailsFast(t *testing.T) {
	repo := &fakeRepository{
		results: []fakeResult{
			{err: domain.ErrInsufficientFunds},
		},
	}
	svc := NewPaymentService(repo, testMetrics(), testLogger())

	_, _, err := svc.Create(context.Background(), validInput())
	if !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("expected ErrInsufficientFunds, got %v", err)
	}
	if repo.calls != 1 {
		t.Fatalf("non-retryable errors must fail fast with exactly 1 call, got %d", repo.calls)
	}
}

func TestPaymentService_Create_ExhaustsRetriesAndFails(t *testing.T) {
	results := make([]fakeResult, maxSerializationRetries+1)
	for i := range results {
		results[i] = fakeResult{err: serializationFailureErr()}
	}
	repo := &fakeRepository{results: results}
	svc := NewPaymentService(repo, testMetrics(), testLogger())

	_, _, err := svc.Create(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if int(repo.calls) != maxSerializationRetries+1 {
		t.Fatalf("expected %d calls, got %d", maxSerializationRetries+1, repo.calls)
	}
}

func TestPaymentService_Create_IdempotentReplayShortCircuits(t *testing.T) {
	repo := &fakeRepository{
		results: []fakeResult{
			{result: repository.CreatePaymentResult{
				Payment:       domain.Payment{ID: "pay-3", Status: domain.PaymentStatusCompleted},
				IdempotentHit: true,
			}},
		},
	}
	svc := NewPaymentService(repo, testMetrics(), testLogger())

	payment, idempotentHit, err := svc.Create(context.Background(), validInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !idempotentHit {
		t.Fatal("expected idempotentHit=true")
	}
	if payment.ID != "pay-3" {
		t.Fatalf("expected the original committed payment to be returned, got %s", payment.ID)
	}
}

func TestPaymentService_Get_RejectsEmptyID(t *testing.T) {
	svc := NewPaymentService(&fakeRepository{}, testMetrics(), testLogger())
	_, err := svc.Get(context.Background(), "")
	if !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
}
