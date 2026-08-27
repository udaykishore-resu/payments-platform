package resilience

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

func TestBulkheadBoundsConcurrencyExactly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		size int
	}{
		{"one", 1},
		{"four", 4},
		{"the per-(gateway,tenant) bound", GatewayTenantBulkhead},
		{"the per-gateway bound", GatewayBulkheadPerPod},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := NewBulkhead(BulkheadConfig{Name: tc.name, MaxConcurrent: tc.size})

			var (
				current int64
				peak    int64
				wg      sync.WaitGroup
			)
			// Far more goroutines than permits, so the semaphore is the only thing keeping the
			// concurrency down.
			for i := 0; i < tc.size*8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					release, err := b.TryAcquire()
					if err != nil {
						return
					}
					defer release()

					n := atomic.AddInt64(&current, 1)
					for {
						old := atomic.LoadInt64(&peak)
						if n <= old || atomic.CompareAndSwapInt64(&peak, old, n) {
							break
						}
					}
					time.Sleep(time.Millisecond)
					atomic.AddInt64(&current, -1)
				}()
			}
			wg.Wait()

			if got := atomic.LoadInt64(&peak); got > int64(tc.size) {
				t.Fatalf("peak concurrency = %d, exceeds the bound %d", got, tc.size)
			}
			if b.InFlight() != 0 {
				t.Fatalf("in-flight = %d after every release, want 0", b.InFlight())
			}
		})
	}
}

func TestBulkheadFullReturnsConcurrencyLimited(t *testing.T) {
	t.Parallel()

	b := NewBulkhead(BulkheadConfig{Name: "acquirer-a", MaxConcurrent: 2})

	r1, err := b.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	r2, err := b.Acquire(context.Background())
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}

	_, err = b.Acquire(context.Background())
	if err == nil {
		t.Fatal("the third acquire succeeded on a bulkhead of 2")
	}
	if got := apierror.CodeOf(err); got != apierror.CodeConcurrencyLimitExceeded {
		t.Errorf("code = %s, want %s", got, apierror.CodeConcurrencyLimitExceeded)
	}
	if !apierror.IsRetryable(err) {
		t.Error("a shed must be retryable: the client is told the truth so their backoff behaves")
	}
	var pe *apierror.Error
	if ok := asAPIError(err, &pe); ok && pe.RetryAfterSeconds == 0 {
		t.Error("no Retry-After on a shed response")
	}
	if got := apierror.HTTPStatusOf(err); got != 429 {
		t.Errorf("HTTP status = %d, want 429", got)
	}

	r1()
	if _, err := b.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire after a release: %v", err)
	}
	r2()
}

func asAPIError(err error, target **apierror.Error) bool {
	e := apierror.From(err)
	if e == nil {
		return false
	}
	*target = e
	return true
}

// TestBulkheadQueueOverflowReturnsTheRightCode: the queue is bounded, and overflowing it sheds
// with the same code as a full semaphore rather than blocking.
func TestBulkheadQueueOverflowReturnsTheRightCode(t *testing.T) {
	t.Parallel()

	const (
		size  = 2
		queue = 3
	)
	b := NewBulkhead(BulkheadConfig{
		Name:          "ingress",
		MaxConcurrent: size,
		MaxQueue:      queue,
		MaxWait:       2 * time.Second,
	})

	// Fill the semaphore.
	releases := make([]func(), 0, size)
	for i := 0; i < size; i++ {
		r, err := b.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		releases = append(releases, r)
	}

	// Fill the queue with waiters that will be released at the end.
	waiting := make(chan error, queue)
	var started sync.WaitGroup
	for i := 0; i < queue; i++ {
		started.Add(1)
		go func() {
			started.Done()
			r, err := b.Acquire(context.Background())
			if err == nil {
				r()
			}
			waiting <- err
		}()
	}
	started.Wait()

	// Wait until every waiter is actually queued before overflowing it, so the assertion is
	// about the bound rather than about scheduling.
	deadline := time.Now().Add(2 * time.Second)
	for b.Queued() < queue {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d waiters queued", b.Queued(), queue)
		}
		time.Sleep(time.Millisecond)
	}

	_, err := b.Acquire(context.Background())
	if err == nil {
		t.Fatal("the queue accepted a waiter beyond its bound")
	}
	if got := apierror.CodeOf(err); got != apierror.CodeConcurrencyLimitExceeded {
		t.Errorf("code = %s, want %s", got, apierror.CodeConcurrencyLimitExceeded)
	}

	for _, r := range releases {
		r()
	}
	for i := 0; i < queue; i++ {
		if err := <-waiting; err != nil {
			t.Errorf("a queued waiter failed once permits freed up: %v", err)
		}
	}
}

func TestBulkheadQueueWaitTimesOut(t *testing.T) {
	t.Parallel()

	b := NewBulkhead(BulkheadConfig{
		Name:          "ingress",
		MaxConcurrent: 1,
		MaxQueue:      4,
		MaxWait:       20 * time.Millisecond,
	})
	r, err := b.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer r()

	// The permit is never released, so the waiter must give up on MaxWait rather than block.
	_, err = b.Acquire(context.Background())
	if err == nil {
		t.Fatal("the waiter acquired a permit that was never released")
	}
	if got := apierror.CodeOf(err); got != apierror.CodeConcurrencyLimitExceeded {
		t.Errorf("code = %s, want %s", got, apierror.CodeConcurrencyLimitExceeded)
	}
	if b.Queued() != 0 {
		t.Errorf("queued = %d after the wait expired, want 0: the queue slot leaked", b.Queued())
	}
}

func TestBulkheadQueueRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	b := NewBulkhead(BulkheadConfig{MaxConcurrent: 1, MaxQueue: 4, MaxWait: time.Minute})
	r, err := b.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer r()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if _, err := b.Acquire(ctx); err == nil {
		t.Fatal("want an error on a cancelled context")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the waiter ignored the cancellation and waited %v", elapsed)
	}
	if b.Queued() != 0 {
		t.Errorf("queued = %d after cancellation, want 0", b.Queued())
	}
}

func TestBulkheadZeroQueueDoesNotWait(t *testing.T) {
	t.Parallel()

	b := NewBulkhead(BulkheadConfig{MaxConcurrent: 1})
	r, err := b.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer r()

	start := time.Now()
	if _, err := b.Acquire(context.Background()); err == nil {
		t.Fatal("want a shed")
	}
	// The gateway bulkheads are configured this way precisely so a request thread never blocks
	// on a resource held by another process (baseline A6).
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("the acquire blocked for %v on a bulkhead with no queue", elapsed)
	}
}

func TestBulkheadMaxWaitZeroDisablesTheQueue(t *testing.T) {
	t.Parallel()

	b := NewBulkhead(BulkheadConfig{MaxConcurrent: 1, MaxQueue: 16 /* no MaxWait */})
	r, _ := b.Acquire(context.Background())
	defer r()

	start := time.Now()
	if _, err := b.Acquire(context.Background()); err == nil {
		t.Fatal("want a shed")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("a queue with no wait bound blocked for %v: it is an unbounded queue", elapsed)
	}
}

// TestBulkheadReleaseUnderPanicDoesNotLeakAPermit is the property that keeps a bug in a gateway
// adapter from permanently shrinking the bulkhead. A permit lost to a panic is never recovered:
// the effective concurrency drops by one for the lifetime of the process, and after enough
// panics the bulkhead is closed and every request sheds.
func TestBulkheadReleaseUnderPanicDoesNotLeakAPermit(t *testing.T) {
	t.Parallel()

	const size = 3
	b := NewBulkhead(BulkheadConfig{Name: "panicky", MaxConcurrent: size})

	call := func() (panicked bool) {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		release, err := b.Acquire(context.Background())
		if err != nil {
			t.Errorf("acquire: %v", err)
			return false
		}
		defer release()
		panic("adapter bug")
	}

	for i := 0; i < size*10; i++ {
		if !call() {
			t.Fatalf("iteration %d did not panic", i)
		}
		if got := b.InFlight(); got != 0 {
			t.Fatalf("iteration %d: in-flight = %d after the deferred release ran, want 0", i, got)
		}
	}

	// Every permit must still be available.
	releases := make([]func(), 0, size)
	for i := 0; i < size; i++ {
		r, err := b.Acquire(context.Background())
		if err != nil {
			t.Fatalf("permit %d was leaked by the panics: %v", i, err)
		}
		releases = append(releases, r)
	}
	for _, r := range releases {
		r()
	}
}

// TestBulkheadDoubleReleaseDoesNotRaiseTheBound: the mirror-image bug. A permit returned twice
// would silently let the bulkhead admit more than MaxConcurrent, which defeats the whole type.
func TestBulkheadDoubleReleaseDoesNotRaiseTheBound(t *testing.T) {
	t.Parallel()

	b := NewBulkhead(BulkheadConfig{MaxConcurrent: 1})
	r, err := b.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	for i := 0; i < 10; i++ {
		r()
	}
	if got := b.InFlight(); got != 0 {
		t.Fatalf("in-flight = %d after ten releases, want 0", got)
	}

	r1, err := b.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer r1()
	if _, err := b.Acquire(context.Background()); err == nil {
		t.Fatal("the double release raised the effective bound above MaxConcurrent")
	}
}

func TestBulkheadCountersAndSaturation(t *testing.T) {
	t.Parallel()

	b := NewBulkhead(BulkheadConfig{MaxConcurrent: 4})
	if got := b.Capacity(); got != 4 {
		t.Fatalf("capacity = %d, want 4", got)
	}
	releases := make([]func(), 0, 4)
	for i := 0; i < 4; i++ {
		r, err := b.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		releases = append(releases, r)
		if got, want := b.Saturation(), float64(i+1)/4; got != want {
			t.Errorf("saturation after %d = %v, want %v", i+1, got, want)
		}
	}
	if got := b.Available(); got != 0 {
		t.Errorf("available = %d, want 0", got)
	}
	for i := 0; i < 3; i++ {
		_, _ = b.Acquire(context.Background())
	}
	if got := b.Rejected(); got != 3 {
		t.Errorf("rejected = %d, want 3", got)
	}
	if got := b.Admitted(); got != 4 {
		t.Errorf("admitted = %d, want 4", got)
	}
	for _, r := range releases {
		r()
	}
}

func TestBulkheadRejectsAlreadyCancelledContext(t *testing.T) {
	t.Parallel()

	b := NewBulkhead(BulkheadConfig{MaxConcurrent: 4})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Acquire(ctx); err == nil {
		t.Fatal("a cancelled context was admitted")
	}
	if got := b.InFlight(); got != 0 {
		t.Fatalf("in-flight = %d, want 0", got)
	}
}

func TestBulkheadConcurrentUseIsRaceFree(t *testing.T) {
	t.Parallel()

	b := NewBulkhead(BulkheadConfig{MaxConcurrent: 8, MaxQueue: 8, MaxWait: 5 * time.Millisecond})
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				r, err := b.Acquire(context.Background())
				if err != nil {
					continue
				}
				_ = b.InFlight()
				_ = b.Saturation()
				r()
			}
		}()
	}
	wg.Wait()
	if got := b.InFlight(); got != 0 {
		t.Fatalf("in-flight = %d, want 0", got)
	}
	if got := b.Queued(); got != 0 {
		t.Fatalf("queued = %d, want 0", got)
	}
}

func TestBulkheadDefaultSizeMatchesLittlesLaw(t *testing.T) {
	t.Parallel()

	// L = λ × W = (5000/3) × 1.5 ≈ 2500 fleet-wide; ÷ 12 pods ≈ 200.
	if got := NewBulkhead(BulkheadConfig{}).Capacity(); got != GatewayBulkheadPerPod {
		t.Fatalf("default capacity = %d, want %d", got, GatewayBulkheadPerPod)
	}
	if GatewayBulkheadPerPod/GatewayTenantBulkhead != 6 {
		t.Errorf("the per-tenant bound %d is not 200 ÷ ~6 active tenants", GatewayTenantBulkhead)
	}
}

// --- registry -------------------------------------------------------------------------------

func TestBulkheadRegistryPerKeyIsolation(t *testing.T) {
	t.Parallel()

	r := NewBulkheadRegistry(BulkheadRegistryConfig{
		Clock: NewManualClock(time.Time{}),
		Configure: func(key string) BulkheadConfig {
			return BulkheadConfig{Name: key, MaxConcurrent: 1}
		},
	})
	defer r.Close()

	a := r.Get("acquirer-a")
	if a != r.Get("acquirer-a") {
		t.Fatal("two bulkheads for one key")
	}

	ra, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire on a: %v", err)
	}
	defer ra()

	// Saturating one gateway must not shed the other.
	rb, err := r.Get("acquirer-b").Acquire(context.Background())
	if err != nil {
		t.Fatalf("a saturated gateway bulkhead shed traffic to a different gateway: %v", err)
	}
	rb()
}

func TestBulkheadRegistryBoundsCardinalityAndEvictsIdle(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	r := NewBulkheadRegistry(BulkheadRegistryConfig{
		Clock:   clk,
		MaxKeys: 8,
		IdleTTL: time.Minute,
	})
	defer r.Close()

	for i := 0; i < 5000; i++ {
		r.Get(fmt.Sprintf("tenant-%d", i))
		if r.Len() > 8 {
			t.Fatalf("registry grew to %d, exceeding MaxKeys=8", r.Len())
		}
	}
	clk.Advance(2 * time.Minute)
	if n := r.EvictIdle(); n != 8 {
		t.Fatalf("evicted %d, want 8", n)
	}
	if r.Len() != 0 {
		t.Fatalf("len = %d after evicting everything idle, want 0", r.Len())
	}
}

func TestBulkheadRegistrySweeperStopsOnClose(t *testing.T) {
	assertNoGoroutineLeaks(t)

	clk := NewManualClock(time.Time{})
	r := NewBulkheadRegistry(BulkheadRegistryConfig{
		Clock:         clk,
		IdleTTL:       time.Millisecond,
		SweepInterval: time.Millisecond,
	})
	r.Get("acquirer-a")
	clk.Advance(time.Second)
	time.Sleep(20 * time.Millisecond)

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestBulkheadRegistrySnapshot(t *testing.T) {
	t.Parallel()

	r := NewBulkheadRegistry(BulkheadRegistryConfig{Clock: NewManualClock(time.Time{})})
	defer r.Close()
	r.Get("acquirer-a")
	r.Get("acquirer-b")

	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot has %d entries, want 2", len(snap))
	}
	if snap["acquirer-a"].Name() != "acquirer-a" {
		t.Errorf("name = %q, want acquirer-a", snap["acquirer-a"].Name())
	}
}
