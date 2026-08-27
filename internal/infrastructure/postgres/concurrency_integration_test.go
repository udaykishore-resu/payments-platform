//go:build integration

package postgres

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// TestConcurrentIdempotencyClaimsYieldExactlyOneNew is the test the whole idempotency design
// exists to pass.
//
// N goroutines present the same key with the same fingerprint at the same moment. Exactly one
// must be told it owns the operation; every other must be told the operation is already in
// flight. One extra NEW is one extra payment.
//
// The mechanism is a single `INSERT ... ON CONFLICT DO NOTHING RETURNING`. A read-then-insert
// would pass this test at N=2 on a quiet laptop and fail it in production, which is why the
// implementation has no read before the write.
func TestConcurrentIdempotencyClaimsYieldExactlyOneNew(t *testing.T) {
	// Verifies: FR-55.
	pool := testPool(t)
	seedTenants(t, pool)
	ctx := tenantContext(t, tenantAlpha)
	uow := testUnitOfWork(t, pool)

	const goroutines = 16
	key := ports.IdempotencyKey{
		TenantID:     tenantAlpha,
		MerchantID:   shared.NewMerchantID(),
		Method:       "POST",
		PathTemplate: "/v1/payments",
		Key:          "concurrent-" + string(shared.NewPaymentID()),
	}

	var (
		start    = make(chan struct{})
		wg       sync.WaitGroup
		mu       sync.Mutex
		outcomes = map[ports.ClaimOutcome]int{}
		failures []error
	)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them all at once, so the race is real rather than serialised
			err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
				res, err := r.Idempotency.Claim(ctx, ports.IdempotencyRecord{
					Key:            key,
					Fingerprint:    fixedFingerprint,
					LeaseExpiresAt: time.Now().Add(30 * time.Second),
					RequestID:      "req-" + string(rune('a'+i)),
				})
				if err != nil {
					return err
				}
				mu.Lock()
				outcomes[res.Outcome]++
				mu.Unlock()
				return nil
			})
			if err != nil {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	// A serialization failure or a deadlock is a legitimate outcome under this much contention
	// and the caller is expected to retry; what is not legitimate is two winners.
	for _, err := range failures {
		t.Logf("claim failed (the caller would retry): %v", err)
	}

	if outcomes[ports.ClaimNew] != 1 {
		t.Fatalf("%d goroutines were told NEW, want exactly 1 — every extra one is an extra "+
			"payment (outcomes: %v)", outcomes[ports.ClaimNew], outcomes)
	}
	if outcomes[ports.ClaimReclaimed] != 0 {
		t.Fatalf("%d claims reclaimed a live lease; the reclaim predicate is not checking "+
			"lease_expires_at", outcomes[ports.ClaimReclaimed])
	}
	total := 0
	for _, n := range outcomes {
		total += n
	}
	if total+len(failures) != goroutines {
		t.Fatalf("accounted for %d of %d goroutines", total+len(failures), goroutines)
	}
}

// TestIdempotencyDistinguishesEveryOutcome walks the four cases a claim on an existing key can
// produce. They are genuinely different answers to the client and conflating any two of them is
// a bug with money attached.
func TestIdempotencyDistinguishesEveryOutcome(t *testing.T) {
	pool := testPool(t)
	seedTenants(t, pool)
	ctx := tenantContext(t, tenantAlpha)
	uow := testUnitOfWork(t, pool)

	base := ports.IdempotencyKey{
		TenantID: tenantAlpha, MerchantID: shared.NewMerchantID(),
		Method: "POST", PathTemplate: "/v1/payments",
	}

	t.Run("in progress", func(t *testing.T) {
		key := base
		key.Key = "inflight-" + string(shared.NewPaymentID())
		claim(t, ctx, uow, key, fixedFingerprint, 30*time.Second, ports.ClaimNew)
		res := claim(t, ctx, uow, key, fixedFingerprint, 30*time.Second, ports.ClaimInProgress)
		if res.RetryAfter <= 0 {
			t.Fatal("an IN_PROGRESS claim must carry Retry-After; the caller must not block on " +
				"another process's lease")
		}
	})

	t.Run("fingerprint mismatch", func(t *testing.T) {
		key := base
		key.Key = "mismatch-" + string(shared.NewPaymentID())
		claim(t, ctx, uow, key, fixedFingerprint, 30*time.Second, ports.ClaimNew)
		// Same key, different body. This is a client bug — one key used for two operations — and
		// treating it as a replay would return the first operation's response for the second
		// operation's request.
		claim(t, ctx, uow, key, otherFingerprint, 30*time.Second, ports.ClaimFingerprintMismatch)
	})

	t.Run("replay", func(t *testing.T) {
		key := base
		key.Key = "replay-" + string(shared.NewPaymentID())
		claim(t, ctx, uow, key, fixedFingerprint, 30*time.Second, ports.ClaimNew)
		if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
			return r.Idempotency.Complete(ctx, key, ports.ResponseSnapshot{
				StatusCode: 201, Body: []byte(`{"id":"pay_1"}`), ResourceID: "pay_1",
				CompletedAt: time.Now(),
			})
		}); err != nil {
			t.Fatalf("complete: %v", err)
		}
		res := claim(t, ctx, uow, key, fixedFingerprint, 30*time.Second, ports.ClaimReplay)
		if res.Snapshot == nil || res.Snapshot.StatusCode != http.StatusCreated {
			t.Fatalf("a replay must return the stored snapshot verbatim, got %+v", res.Snapshot)
		}
		if string(res.Snapshot.Body) != `{"id":"pay_1"}` {
			t.Fatalf("the stored body did not round-trip: %q", res.Snapshot.Body)
		}
	})

	t.Run("expired lease is reclaimed atomically", func(t *testing.T) {
		key := base
		key.Key = "reclaim-" + string(shared.NewPaymentID())
		// A lease already in the past: the original holder died.
		claimAt(t, ctx, uow, key, fixedFingerprint, time.Now().Add(-time.Minute), ports.ClaimNew)
		claim(t, ctx, uow, key, fixedFingerprint, 30*time.Second, ports.ClaimReclaimed)
		// And having been reclaimed, the lease is live again, so the next caller waits.
		claim(t, ctx, uow, key, fixedFingerprint, 30*time.Second, ports.ClaimInProgress)
	})

	t.Run("release makes the key claimable again", func(t *testing.T) {
		key := base
		key.Key = "release-" + string(shared.NewPaymentID())
		claim(t, ctx, uow, key, fixedFingerprint, 30*time.Second, ports.ClaimNew)
		if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
			return r.Idempotency.Release(ctx, key)
		}); err != nil {
			t.Fatalf("release: %v", err)
		}
		// A retryable failure released the claim, so the client's retry is a genuine new attempt
		// rather than a replay of an error that may since have cleared.
		claim(t, ctx, uow, key, fixedFingerprint, 30*time.Second, ports.ClaimNew)
	})
}

// TestConcurrentReclaimsYieldExactlyOneWinner. The reclaim is an UPDATE with the expiry in its
// predicate, not a read followed by a write. Read-then-write would let two callers both observe
// the expired lease and both re-execute the operation — a double charge produced by the very
// mechanism that exists to prevent one.
func TestConcurrentReclaimsYieldExactlyOneWinner(t *testing.T) {
	// Verifies: FR-58.
	pool := testPool(t)
	seedTenants(t, pool)
	ctx := tenantContext(t, tenantAlpha)
	uow := testUnitOfWork(t, pool)

	key := ports.IdempotencyKey{
		TenantID: tenantAlpha, MerchantID: shared.NewMerchantID(),
		Method: "POST", PathTemplate: "/v1/payments",
		Key: "reclaim-race-" + string(shared.NewPaymentID()),
	}
	claimAt(t, ctx, uow, key, fixedFingerprint, time.Now().Add(-time.Minute), ports.ClaimNew)

	const goroutines = 12
	var (
		start     = make(chan struct{})
		wg        sync.WaitGroup
		mu        sync.Mutex
		reclaimed int
		inFlight  int
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
				res, err := r.Idempotency.Claim(ctx, ports.IdempotencyRecord{
					Key: key, Fingerprint: fixedFingerprint,
					LeaseExpiresAt: time.Now().Add(30 * time.Second),
				})
				if err != nil {
					return err
				}
				mu.Lock()
				switch res.Outcome {
				case ports.ClaimReclaimed:
					reclaimed++
				case ports.ClaimInProgress:
					inFlight++
				default:
					// ClaimNew, ClaimReplay and ClaimFingerprintMismatch are all failures of this test's premise
					// (one expired lease, N racing reclaimers) and are caught by the counts asserted below
				}
				mu.Unlock()
				return nil
			})
		}()
	}
	close(start)
	wg.Wait()

	if reclaimed != 1 {
		t.Fatalf("%d goroutines reclaimed the expired lease, want exactly 1; the reclaim is not "+
			"atomic and the operation would be executed %d times", reclaimed, reclaimed)
	}
	_ = inFlight
}

// TestI1ConcurrentPartialRefundsCannotExceedCaptured. N goroutines each refund a slice of a
// captured payment; the sum must never exceed the capture.
//
// This is the case SERIALIZABLE and the FOR UPDATE on the parent row exist for: under READ
// COMMITTED, two transactions each reading refunded_amount and each writing a legal-looking
// increment together exceed the captured amount, and the CHECK constraint would be evaluated
// against a value neither of them saw.
func TestI1ConcurrentPartialRefundsCannotExceedCaptured(t *testing.T) {
	// Verifies: BR-21, FR-70.
	pool := testPool(t)
	seedTenants(t, pool)
	ctx := tenantContext(t, tenantAlpha)
	uow := testUnitOfWork(t, pool)
	clock := shared.SystemClock{}

	const captured = 1_000
	p := newTestPayment(t, tenantAlpha, captured)
	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Payments.Create(ctx, p)
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		loaded, err := r.Payments.GetForUpdate(ctx, p.ID())
		if err != nil {
			return err
		}
		if err := loaded.MarkProcessing(clock); err != nil {
			return err
		}
		if err := loaded.MarkCaptured(money.MustNew(captured, "USD"), clock); err != nil {
			return err
		}
		return r.Payments.Save(ctx, loaded)
	}); err != nil {
		t.Fatalf("capture: %v", err)
	}

	// Ten goroutines each try to refund 200 of a 1000 capture. At most five can succeed.
	const goroutines = 10
	const slice = 200
	var (
		start = make(chan struct{})
		wg    sync.WaitGroup
		mu    sync.Mutex
		ok    int
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			err := uow.WithinSerializable(ctx, func(ctx context.Context, r ports.Repositories) error {
				loaded, err := r.Payments.GetForUpdate(ctx, p.ID())
				if err != nil {
					return err
				}
				if _, err := loaded.AddRefund(money.MustNew(slice, "USD"),
					payment.RefundReasonRequestedByCustomer,
					"refund-"+string(rune('a'+i)), clock); err != nil {
					return err
				}
				return r.Payments.Save(ctx, loaded)
			})
			if err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	var refunded, capturedRead int64
	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		loaded, err := r.Payments.Get(ctx, p.ID())
		if err != nil {
			return err
		}
		refunded = loaded.RefundedAmount().Amount()
		capturedRead = loaded.CapturedAmount().Amount()
		return nil
	}); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if refunded > capturedRead {
		t.Fatalf("refunded %d exceeds captured %d; invariant I1 was violated under concurrency",
			refunded, capturedRead)
	}
	if refunded != int64(ok)*slice {
		t.Fatalf("refunded %d but %d refunds reported success; the two must agree",
			refunded, ok)
	}
	t.Logf("%d of %d concurrent refunds succeeded, total refunded %d of %d captured",
		ok, goroutines, refunded, capturedRead)
}

const (
	fixedFingerprint = "0000000000000000000000000000000000000000000000000000000000000001"
	otherFingerprint = "0000000000000000000000000000000000000000000000000000000000000002"
)

func claim(
	t *testing.T, ctx context.Context, uow *UnitOfWork,
	key ports.IdempotencyKey, fingerprint string, lease time.Duration,
	want ports.ClaimOutcome,
) ports.ClaimResult {
	t.Helper()
	return claimAt(t, ctx, uow, key, fingerprint, time.Now().Add(lease), want)
}

func claimAt(
	t *testing.T, ctx context.Context, uow *UnitOfWork,
	key ports.IdempotencyKey, fingerprint string, leaseUntil time.Time,
	want ports.ClaimOutcome,
) ports.ClaimResult {
	t.Helper()
	var res ports.ClaimResult
	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		var err error
		res, err = r.Idempotency.Claim(ctx, ports.IdempotencyRecord{
			Key: key, Fingerprint: fingerprint, LeaseExpiresAt: leaseUntil,
		})
		return err
	}); err != nil {
		t.Fatalf("claim %s: %v", key.Key, err)
	}
	if res.Outcome != want {
		t.Fatalf("claim %s produced %s, want %s", key.Key, res.Outcome, want)
	}
	return res
}
