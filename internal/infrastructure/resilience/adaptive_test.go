package resilience

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// feed drives one adjustment window: `samples` requests at the given RTT, then enough clock to
// close the window. Using Update rather than Acquire keeps the test about the algorithm rather
// than about admission.
func feed(a *AdaptiveLimiter, clk *ManualClock, samples int, rtt time.Duration, dropped bool) {
	for i := 0; i < samples; i++ {
		a.Update(rtt, dropped)
	}
	clk.Advance(AdaptiveSampleWindow)
	// One more sample after the window has elapsed triggers the adjustment.
	a.Update(rtt, dropped)
}

func newTestLimiter(clk Clock, mutate func(*AdaptiveConfig)) *AdaptiveLimiter {
	cfg := AdaptiveConfig{Name: "payment-orchestrator:authorize", Clock: clk}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewAdaptiveLimiter(cfg)
}

func TestAdaptiveLimiterDefaultsMatchTheDocument(t *testing.T) {
	t.Parallel()

	a := newTestLimiter(NewManualClock(time.Time{}), nil)
	if got := a.Limit(); got != AdaptiveInitialLimit {
		t.Errorf("initial limit = %d, want %d", got, AdaptiveInitialLimit)
	}
	if AdaptiveMinLimit != 20 || AdaptiveMaxLimit != 2000 {
		t.Errorf("bounds are %d..%d, want 20..2000 (docs/failure-handling.md §2.8)", AdaptiveMinLimit, AdaptiveMaxLimit)
	}
	// Queue = limit × 0.5.
	if got := a.QueueCapacity(); got != AdaptiveInitialLimit/2 {
		t.Errorf("queue capacity = %d, want %d", got, AdaptiveInitialLimit/2)
	}
}

// TestAdaptiveLimitRisesUnderLowLatency: with the current RTT equal to the no-load minimum the
// gradient is 1, and the queue allowance is what makes the limit climb — otherwise a service
// that had just been scaled up would stay pinned at its pre-scale-up limit forever.
func TestAdaptiveLimitRisesUnderLowLatency(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	a := newTestLimiter(clk, nil)

	start := a.Limit()
	for i := 0; i < 20; i++ {
		feed(a, clk, AdaptiveMinSamples, 10*time.Millisecond, false)
	}
	if got := a.Limit(); got <= start {
		t.Fatalf("limit = %d after 20 windows of flat, fast latency, want above %d: the limiter cannot discover new capacity", got, start)
	}
}

// TestAdaptiveLimitFallsUnderRisingLatency: the no-load baseline is learned while the service is
// fast, and then latency rises tenfold. The gradient clamps at 0.5, so the reduction is at most
// 50 % per adjustment and the limit walks down instead of collapsing.
func TestAdaptiveLimitFallsUnderRisingLatency(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	a := newTestLimiter(clk, nil)

	// Learn the baseline.
	feed(a, clk, AdaptiveMinSamples, 10*time.Millisecond, false)
	if got := a.NoLoadRTT(); got != 10*time.Millisecond {
		t.Fatalf("no-load RTT = %v, want 10ms", got)
	}
	before := a.Limit()

	feed(a, clk, AdaptiveMinSamples, 200*time.Millisecond, false)
	after := a.Limit()
	if after >= before {
		t.Fatalf("limit went from %d to %d under a 20× latency rise, want a reduction", before, after)
	}
	// The gradient floor caps a single adjustment: limit×(1−α) + α×(limit×0.5 + queue).
	if float64(after) < float64(before)*0.5 {
		t.Fatalf("limit fell from %d to %d, more than the gradient floor permits in one step", before, after)
	}
}

func TestAdaptiveLimitStepArithmetic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		baseRTT time.Duration
		nowRTT  time.Duration
		dropped bool
		want    int
	}{
		{
			// gradient 1: 102×0.8 + 0.2×(102×1 + ceil(sqrt(102))=11) = 81.6 + 22.6 = 104.2
			name: "flat latency probes upward", baseRTT: 10 * time.Millisecond, nowRTT: 10 * time.Millisecond, want: 104,
		},
		{
			// gradient 0.1 clamped to 0.5: 81.6 + 0.2×(51 + 11) = 81.6 + 12.4 = 94
			name: "latency 10× the baseline clamps at the floor", baseRTT: 10 * time.Millisecond, nowRTT: 100 * time.Millisecond, want: 94,
		},
		{
			// gradient 30/40 = 0.75: 81.6 + 0.2×(76.5 + 11) = 81.6 + 17.5 = 99.1
			name: "latency 4/3 of the baseline", baseRTT: 30 * time.Millisecond, nowRTT: 40 * time.Millisecond, want: 99,
		},
		{
			// a drop forces the floor regardless of the measured RTT
			name: "a dropped request forces the gradient floor", baseRTT: 10 * time.Millisecond, nowRTT: 10 * time.Millisecond, dropped: true, want: 94,
		},
	}

	// The first window always establishes the no-load baseline at a gradient of 1, taking the
	// limit from 100 to 100×0.8 + 0.2×(100 + ceil(sqrt(100))) = 102. The window under test is
	// the second one, which is why every expectation above starts from 102.
	const afterBaselineWindow = 102

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk := NewManualClock(time.Time{})
			a := newTestLimiter(clk, nil)

			feed(a, clk, AdaptiveMinSamples, tc.baseRTT, false)
			if got := a.Limit(); got != afterBaselineWindow {
				t.Fatalf("precondition: limit after the baseline window = %d, want %d", got, afterBaselineWindow)
			}

			feed(a, clk, AdaptiveMinSamples, tc.nowRTT, tc.dropped)
			if got := a.Limit(); got != tc.want {
				t.Fatalf("limit = %d, want %d (no-load %v, current %v, dropped %v)",
					got, tc.want, tc.baseRTT, tc.nowRTT, tc.dropped)
			}
		})
	}
}

func TestAdaptiveLimitBoundedAbove(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	a := newTestLimiter(clk, nil)

	// Hundreds of windows of perfect latency must not push the limit past the ceiling, whatever
	// the algorithm believes: the ceiling bounds memory and goroutine growth.
	for i := 0; i < 500; i++ {
		feed(a, clk, AdaptiveMinSamples, time.Millisecond, false)
		if got := a.Limit(); got > AdaptiveMaxLimit {
			t.Fatalf("window %d: limit = %d, above the ceiling %d", i, got, AdaptiveMaxLimit)
		}
	}
	if got := a.Limit(); got != AdaptiveMaxLimit {
		t.Fatalf("limit = %d after 500 fast windows, want it pinned at the ceiling %d", got, AdaptiveMaxLimit)
	}
}

func TestAdaptiveLimitBoundedBelow(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	a := newTestLimiter(clk, nil)

	feed(a, clk, AdaptiveMinSamples, time.Millisecond, false) // baseline
	for i := 0; i < 500; i++ {
		feed(a, clk, AdaptiveMinSamples, 10*time.Second, true)
		if got := a.Limit(); got < AdaptiveMinLimit {
			t.Fatalf("window %d: limit = %d, below the floor %d", i, got, AdaptiveMinLimit)
		}
	}
	if got := a.Limit(); got != AdaptiveMinLimit {
		t.Fatalf("limit = %d after 500 bad windows, want it pinned at the floor %d", got, AdaptiveMinLimit)
	}
}

func TestAdaptiveLimiterDoesNotAdjustOnTooFewSamples(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	a := newTestLimiter(clk, nil)

	// A very low-traffic route: one request every ten seconds. The 1 s window elapses again and
	// again, but the evidence never reaches MinSamples, so the window is held open instead of
	// being closed on a sample too small to mean anything — recomputing the limit from one
	// sample is the same statistical error as opening a circuit on one request. The limit on a
	// route like this is not the constraint anyway.
	for i := 0; i < AdaptiveMinSamples-1; i++ {
		a.Update(10*time.Millisecond, false)
		clk.Advance(10 * time.Second)
	}
	if got := a.Limit(); got != AdaptiveInitialLimit {
		t.Fatalf("limit = %d after %d samples, want %d unchanged", got, AdaptiveMinSamples-1, AdaptiveInitialLimit)
	}

	// The sample that reaches the minimum closes the stretched window.
	a.Update(10*time.Millisecond, false)
	if got := a.Limit(); got == AdaptiveInitialLimit {
		t.Fatalf("limit = %d once MinSamples was reached, want an adjustment", got)
	}
}

func TestAdaptiveLimiterAdjustsOnADropEvenWithFewSamples(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	a := newTestLimiter(clk, nil)

	a.Update(10*time.Millisecond, false)
	clk.Advance(AdaptiveSampleWindow)
	a.Update(0, true) // a drop has no RTT

	if got := a.Limit(); got >= AdaptiveInitialLimit {
		t.Fatalf("limit = %d after a drop, want a reduction: a drop is evidence even when the sample count is low", got)
	}
}

func TestAdaptiveNoLoadBaselineAgesOut(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	a := newTestLimiter(clk, nil)

	feed(a, clk, AdaptiveMinSamples, time.Millisecond, false)
	if got := a.NoLoadRTT(); got != time.Millisecond {
		t.Fatalf("no-load RTT = %v, want 1ms", got)
	}
	// Past the 5-minute horizon the old best must age out, or a one-off fast second recorded
	// during a warm cache would define "fast" forever and make every later window look like a
	// brownout.
	clk.Advance(AdaptiveNoLoadWindow + time.Minute)
	if got := a.NoLoadRTT(); got != 0 {
		t.Fatalf("no-load RTT = %v after the horizon elapsed, want it aged out", got)
	}
	feed(a, clk, AdaptiveMinSamples, 50*time.Millisecond, false)
	if got := a.NoLoadRTT(); got != 50*time.Millisecond {
		t.Fatalf("no-load RTT = %v, want the new baseline 50ms", got)
	}
}

func TestAdaptiveAcquireBoundsConcurrency(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	a := newTestLimiter(clk, func(c *AdaptiveConfig) {
		c.InitialLimit = 3
		c.MinLimit = 1
	})

	releases := make([]func(time.Duration, bool), 0, 3)
	for i := 0; i < 3; i++ {
		r, err := a.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		releases = append(releases, r)
	}
	if got := a.InFlight(); got != 3 {
		t.Fatalf("in-flight = %d, want 3", got)
	}
	if got := a.Pressure(); got != 1 {
		t.Fatalf("pressure = %v, want 1", got)
	}

	_, err := a.Acquire(context.Background())
	if err == nil {
		t.Fatal("the limiter admitted a fourth request at a limit of 3")
	}
	if got := apierror.CodeOf(err); got != apierror.CodeConcurrencyLimitExceeded {
		t.Errorf("code = %s, want %s", got, apierror.CodeConcurrencyLimitExceeded)
	}
	if !apierror.IsRetryable(err) {
		t.Error("a concurrency rejection must be retryable")
	}

	for _, r := range releases {
		r(10*time.Millisecond, false)
	}
	if got := a.InFlight(); got != 0 {
		t.Fatalf("in-flight = %d after every release, want 0", got)
	}
}

func TestAdaptiveReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	a := newTestLimiter(clk, func(c *AdaptiveConfig) { c.InitialLimit = 2; c.MinLimit = 1 })

	r, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	for i := 0; i < 5; i++ {
		r(time.Millisecond, false)
	}
	if got := a.InFlight(); got != 0 {
		t.Fatalf("in-flight = %d after five releases, want 0: a double release raises the effective limit", got)
	}
}

func TestAdaptiveAcquireRejectsCancelledContext(t *testing.T) {
	t.Parallel()

	a := newTestLimiter(NewManualClock(time.Time{}), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Acquire(ctx); err == nil {
		t.Fatal("a cancelled context was admitted")
	}
	if got := a.InFlight(); got != 0 {
		t.Fatalf("in-flight = %d, want 0", got)
	}
}

func TestAdaptiveLimitChangedCallback(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	var mu sync.Mutex
	var changes []limitChange
	a := newTestLimiter(clk, func(c *AdaptiveConfig) {
		c.LimitChanged = func(_ string, from, to int) {
			mu.Lock()
			defer mu.Unlock()
			changes = append(changes, limitChange{from: from, to: to})
		}
	})

	feed(a, clk, AdaptiveMinSamples, 10*time.Millisecond, false)
	feed(a, clk, AdaptiveMinSamples, 10*time.Millisecond, false)

	mu.Lock()
	defer mu.Unlock()
	if len(changes) == 0 {
		t.Fatal("no limit change was reported: pp_adaptive_limit would never move")
	}
	if changes[0].from != AdaptiveInitialLimit {
		t.Fatalf("first change was from %d, want %d", changes[0].from, AdaptiveInitialLimit)
	}
}

func TestAdaptiveSampleReservoirIsBounded(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	a := newTestLimiter(clk, nil)

	// Far more samples than the reservoir holds. The point is that this neither allocates
	// without bound nor breaks the median: a burst of 50 000 requests in one second must cost
	// the same memory as a burst of 50.
	for i := 0; i < adaptiveSampleCapacity*10; i++ {
		a.Update(10*time.Millisecond, false)
	}
	clk.Advance(AdaptiveSampleWindow)
	a.Update(10*time.Millisecond, false)

	if got := a.Limit(); got != 102 {
		t.Fatalf("limit = %d, want 102: the wrapped reservoir produced a wrong median", got)
	}
}

func TestAdaptiveConcurrentUseIsRaceFree(t *testing.T) {
	t.Parallel()

	a := newTestLimiter(SystemClock(), func(c *AdaptiveConfig) { c.InitialLimit = 64 })
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				r, err := a.Acquire(context.Background())
				if err != nil {
					continue
				}
				_ = a.Limit()
				_ = a.Pressure()
				_ = a.QueueCapacity()
				_ = a.NoLoadRTT()
				r(time.Duration(i%50)*time.Millisecond, (g+i)%17 == 0)
			}
		}(g)
	}
	wg.Wait()
	if got := a.InFlight(); got != 0 {
		t.Fatalf("in-flight = %d, want 0", got)
	}
	if got := a.Limit(); got < AdaptiveMinLimit || got > AdaptiveMaxLimit {
		t.Fatalf("limit = %d, outside [%d, %d]", got, AdaptiveMinLimit, AdaptiveMaxLimit)
	}
}
