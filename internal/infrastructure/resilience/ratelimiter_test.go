package resilience

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

func TestNewLimitBurstIsTwiceTheRate(t *testing.T) {
	t.Parallel()

	l := NewLimit(100)
	if l.Rate != 100 || l.Burst != 200 {
		t.Fatalf("NewLimit(100) = %+v, want rate 100 burst 200 (docs/failure-handling.md §2.6)", l)
	}
}

func TestTokenBucketBurstThenRate(t *testing.T) {
	// Verifies: NFR-36.
	t.Parallel()

	tests := []struct {
		name  string
		limit Limit
		// after draining the burst, advance by this and expect exactly `then` more admissions.
		advance time.Duration
		then    int
	}{
		{"100 rps, burst 200, one second refills 100", NewLimit(100), time.Second, 100},
		{"100 rps, half a second refills 50", NewLimit(100), 500 * time.Millisecond, 50},
		{"10 rps, burst 20, one second refills 10", NewLimit(10), time.Second, 10},
		{"1 rps, burst 2, three seconds refills 2 (capped by the burst)", NewLimit(1), 3 * time.Second, 2},
		{"explicit burst is honoured", Limit{Rate: 50, Burst: 5}, time.Second, 5},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk := NewManualClock(time.Time{})
			b := NewTokenBucket(tc.limit, clk)

			// The burst is available immediately and not one token more.
			want := tc.limit.normalized().Burst
			for i := 0; i < want; i++ {
				if !b.Allow() {
					t.Fatalf("burst token %d of %d was refused", i+1, want)
				}
			}
			if b.Allow() {
				t.Fatalf("token %d was admitted: the burst is not bounded", want+1)
			}

			clk.Advance(tc.advance)
			admitted := 0
			for b.Allow() {
				admitted++
			}
			if admitted != tc.then {
				t.Fatalf("after %v the bucket admitted %d, want %d", tc.advance, admitted, tc.then)
			}
		})
	}
}

// TestTokenBucketHasNoWindowBoundaryToExploit is the reason this is a token bucket and not a
// fixed window: a fixed window would admit 2× the limit across a boundary.
func TestTokenBucketHasNoWindowBoundaryToExploit(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := NewTokenBucket(Limit{Rate: 100, Burst: 100}, clk)

	// Drain the burst at the end of "window one".
	for i := 0; i < 100; i++ {
		if !b.Allow() {
			t.Fatalf("burst token %d refused", i)
		}
	}
	// One millisecond later — a fixed window would have rolled over and handed out 100 more.
	clk.Advance(time.Millisecond)
	admitted := 0
	for b.Allow() {
		admitted++
	}
	if admitted > 1 {
		t.Fatalf("%d tokens admitted 1ms after draining the burst: this is a fixed window", admitted)
	}
}

func TestDecisionMapsToHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		d    Decision
		want map[string]string
	}{
		{
			name: "allowed",
			d:    Decision{Allowed: true, Limit: 200, Remaining: 199, ResetAfter: 1500 * time.Millisecond},
			want: map[string]string{
				"RateLimit-Limit":     "200",
				"RateLimit-Remaining": "199",
				"RateLimit-Reset":     "2", // rounded up
			},
		},
		{
			name: "rejected carries Retry-After",
			d: Decision{
				Allowed: false, Limit: 200, Remaining: 0,
				ResetAfter: 2 * time.Second, RetryAfter: 10 * time.Millisecond,
			},
			want: map[string]string{
				"RateLimit-Limit":     "200",
				"RateLimit-Remaining": "0",
				"RateLimit-Reset":     "2",
				"Retry-After":         "1", // never zero: a client told to retry now retries now
			},
		},
		{
			name: "a long reset horizon",
			d:    Decision{Allowed: false, Limit: 10, Remaining: 0, ResetAfter: 95 * time.Second, RetryAfter: 9500 * time.Millisecond},
			want: map[string]string{
				"RateLimit-Limit":     "10",
				"RateLimit-Remaining": "0",
				"RateLimit-Reset":     "95",
				"Retry-After":         "10",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.d.Headers()
			if len(got) != len(tc.want) {
				t.Fatalf("headers = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestDecisionErr(t *testing.T) {
	t.Parallel()

	if err := (Decision{Allowed: true}).Err(); err != nil {
		t.Fatalf("an allowed decision produced an error: %v", err)
	}
	err := Decision{Allowed: false, RetryAfter: 3 * time.Second}.Err()
	if got := apierror.CodeOf(err); got != apierror.CodeRateLimited {
		t.Fatalf("code = %s, want %s", got, apierror.CodeRateLimited)
	}
	if got := apierror.HTTPStatusOf(err); got != 429 {
		t.Fatalf("status = %d, want 429", got)
	}
	var pe *apierror.Error
	if errors.As(err, &pe) && pe.RetryAfterSeconds != 3 {
		t.Fatalf("RetryAfterSeconds = %d, want 3", pe.RetryAfterSeconds)
	}
}

func TestTokenBucketDecisionFields(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := NewTokenBucket(Limit{Rate: 10, Burst: 10}, clk)

	d := b.Take()
	if !d.Allowed || d.Limit != 10 || d.Remaining != 9 {
		t.Fatalf("first decision = %+v, want allowed with 9 remaining of 10", d)
	}
	// 1 token consumed at 10/s → the bucket is full again in 100ms.
	if d.ResetAfter != 100*time.Millisecond {
		t.Fatalf("ResetAfter = %v, want 100ms", d.ResetAfter)
	}
	if d.RetryAfter != 0 {
		t.Fatalf("RetryAfter = %v on an allowed decision, want 0", d.RetryAfter)
	}

	for i := 0; i < 9; i++ {
		b.Take()
	}
	d = b.Take()
	if d.Allowed || d.Remaining != 0 {
		t.Fatalf("decision on an empty bucket = %+v, want rejected with 0 remaining", d)
	}
	if d.RetryAfter != 100*time.Millisecond {
		t.Fatalf("RetryAfter = %v, want 100ms (one token at 10/s)", d.RetryAfter)
	}
	if d.ResetAfter != time.Second {
		t.Fatalf("ResetAfter = %v, want 1s (10 tokens at 10/s)", d.ResetAfter)
	}
}

func TestTokenBucketSetLimitPreservesBalance(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := NewTokenBucket(Limit{Rate: 10, Burst: 10}, clk)
	for i := 0; i < 8; i++ {
		b.Take()
	}
	// A configuration publish must not hand the tenant a fresh burst.
	b.SetLimit(Limit{Rate: 100, Burst: 100})
	if got := b.Tokens(); got != 2 {
		t.Fatalf("tokens after SetLimit = %v, want the preserved balance 2", got)
	}
}

func TestTokenBucketConcurrentUseIsRaceFree(t *testing.T) {
	t.Parallel()

	b := NewTokenBucket(NewLimit(10000), SystemClock())
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				_ = b.Take()
				_ = b.Tokens()
			}
		}()
	}
	wg.Wait()
}

// --- distributed limiter ---------------------------------------------------------------------

type stubBackend struct {
	mu       sync.Mutex
	fail     error
	calls    int
	decision Decision
}

func (s *stubBackend) Allow(_ context.Context, _ string, limit Limit) (Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.fail != nil {
		return Decision{}, s.fail
	}
	d := s.decision
	if d.Limit == 0 {
		d = Decision{Allowed: true, Limit: limit.normalized().Burst, Remaining: 1}
	}
	return d, nil
}

func (s *stubBackend) setFail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = err
}

func (s *stubBackend) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestDistributedLimiterPrefersTheBackend(t *testing.T) {
	t.Parallel()

	be := &stubBackend{decision: Decision{Allowed: true, Limit: 200, Remaining: 42}}
	l := NewDistributedLimiter(DistributedLimiterConfig{Backend: be, Replicas: 4, Clock: NewManualClock(time.Time{})})

	d, err := l.Allow(context.Background(), "tenant:merchant:payment_write", NewLimit(100))
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if d.Remaining != 42 {
		t.Fatalf("decision = %+v, want the backend's", d)
	}
	if l.Fallbacks() != 0 || l.LocalKeys() != 0 {
		t.Fatalf("the local fallback was used while the backend was healthy (fallbacks=%d keys=%d)",
			l.Fallbacks(), l.LocalKeys())
	}
}

// TestDistributedLimiterFallsBackAtTheDocumentedMultiplier: on backend failure each replica
// enforces `global / replicas × 1.2` locally.
func TestDistributedLimiterFallsBackAtTheDocumentedMultiplier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		global    Limit
		replicas  int
		wantRate  float64
		wantBurst int
	}{
		{"1000 rps across 10 replicas", NewLimit(1000), 10, 120, 240},
		{"1000 rps across 4 replicas", NewLimit(1000), 4, 300, 600},
		{"100 rps on a single replica", NewLimit(100), 1, 120, 240},
		{"a rate that does not divide evenly rounds the burst up", Limit{Rate: 10, Burst: 7}, 3, 4, 3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			be := &stubBackend{}
			be.setFail(errors.New("redis is gone"))
			clk := NewManualClock(time.Time{})
			l := NewDistributedLimiter(DistributedLimiterConfig{
				Backend: be, Replicas: tc.replicas, Clock: clk,
			})

			eff := l.FallbackLimit(tc.global)
			if eff.Rate != tc.wantRate {
				t.Errorf("fallback rate = %v, want %v (global %v ÷ %d × %.1f)",
					eff.Rate, tc.wantRate, tc.global.Rate, tc.replicas, LocalFallbackMultiplier)
			}
			if eff.Burst != tc.wantBurst {
				t.Errorf("fallback burst = %d, want %d", eff.Burst, tc.wantBurst)
			}

			// And the limiter actually enforces it: exactly `burst` admissions, then refusal.
			admitted := 0
			for i := 0; i < tc.wantBurst+50; i++ {
				d, err := l.Allow(context.Background(), "k", tc.global)
				if err != nil {
					t.Fatalf("Allow returned an error during fallback: %v", err)
				}
				if d.Allowed {
					admitted++
				}
			}
			if admitted != tc.wantBurst {
				t.Fatalf("the fallback admitted %d, want the scaled burst %d", admitted, tc.wantBurst)
			}
			// One second of refill yields the scaled rate, capped by the scaled burst — the
			// bucket cannot hold more than its depth however fast it refills.
			clk.Advance(time.Second)
			wantRefill := min(int(tc.wantRate), tc.wantBurst)
			refilled := 0
			for i := 0; i < tc.wantBurst+50; i++ {
				if d, _ := l.Allow(context.Background(), "k", tc.global); d.Allowed {
					refilled++
				}
			}
			if refilled != wantRefill {
				t.Fatalf("one second of fallback refill admitted %d, want %d", refilled, wantRefill)
			}
		})
	}
}

// TestDistributedLimiterOverAdmitsBoundedlyInAggregate is the arithmetic the multiplier is a
// deliberate choice about: N replicas each enforcing global/N × 1.2 admit at most 1.2 × global.
func TestDistributedLimiterOverAdmitsBoundedlyInAggregate(t *testing.T) {
	t.Parallel()

	const replicas = 8
	global := Limit{Rate: 800, Burst: 800}

	total := 0
	for r := 0; r < replicas; r++ {
		be := &stubBackend{}
		be.setFail(errors.New("redis is gone"))
		l := NewDistributedLimiter(DistributedLimiterConfig{
			Backend: be, Replicas: replicas, Clock: NewManualClock(time.Time{}),
		})
		for i := 0; i < 1000; i++ {
			if d, _ := l.Allow(context.Background(), "tenant", global); d.Allowed {
				total++
			}
		}
	}

	want := int(float64(global.Burst) * LocalFallbackMultiplier)
	if total != want {
		t.Fatalf("aggregate admissions across %d replicas = %d, want %d (%.0f × %.1f)",
			replicas, total, want, float64(global.Burst), LocalFallbackMultiplier)
	}
	if total <= global.Burst {
		t.Error("the fallback did not over-admit at all: valid traffic would be rejected during a Redis outage")
	}
	if float64(total) > float64(global.Burst)*1.5 {
		t.Error("the over-admission is not bounded: the multiplier has become a decision to abandon the limit")
	}
}

func TestDistributedLimiterDiscardsLocalStateWhenTheBackendReturns(t *testing.T) {
	t.Parallel()

	be := &stubBackend{}
	be.setFail(errors.New("redis is gone"))
	l := NewDistributedLimiter(DistributedLimiterConfig{Backend: be, Replicas: 2, Clock: NewManualClock(time.Time{})})

	for i := 0; i < 5; i++ {
		if _, err := l.Allow(context.Background(), "k", NewLimit(100)); err != nil {
			t.Fatalf("Allow: %v", err)
		}
	}
	if l.LocalKeys() != 1 {
		t.Fatalf("local keys = %d during fallback, want 1", l.LocalKeys())
	}

	be.setFail(nil)
	if _, err := l.Allow(context.Background(), "k", NewLimit(100)); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	// There is no gradual handover: split-brain accounting between local and distributed is
	// worse than a discontinuity.
	if l.LocalKeys() != 0 {
		t.Fatalf("local keys = %d after the backend recovered, want 0", l.LocalKeys())
	}
}

func TestDistributedLimiterReportsFallbacks(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seen []string
	be := &stubBackend{}
	be.setFail(errors.New("redis is gone"))
	l := NewDistributedLimiter(DistributedLimiterConfig{
		Backend:  be,
		Replicas: 2,
		Clock:    NewManualClock(time.Time{}),
		OnFallback: func(key string, err error) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, key+":"+err.Error())
		},
	})

	for i := 0; i < 3; i++ {
		_, _ = l.Allow(context.Background(), "k", NewLimit(10))
	}
	if l.Fallbacks() != 3 {
		t.Fatalf("fallbacks = %d, want 3", l.Fallbacks())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("OnFallback fired %d times, want 3: a silent fallback is an invisible change to the enforced limit", len(seen))
	}
}

// TestDistributedLimiterNeverFailsTheRequest: Redis loss must cost latency and precision, never
// availability (F-7).
func TestDistributedLimiterNeverFailsTheRequest(t *testing.T) {
	t.Parallel()

	be := &stubBackend{}
	be.setFail(errors.New("redis is gone"))
	l := NewDistributedLimiter(DistributedLimiterConfig{Backend: be, Replicas: 1, Clock: NewManualClock(time.Time{})})

	if _, err := l.Allow(context.Background(), "k", NewLimit(100)); err != nil {
		t.Fatalf("the limiter failed the request because its counter was unavailable: %v", err)
	}
}

func TestLocalLimiterBoundsKeyCardinality(t *testing.T) {
	t.Parallel()

	l := NewLocalLimiter(16, time.Minute, NewManualClock(time.Time{}))
	for i := 0; i < 5000; i++ {
		if _, err := l.Allow(context.Background(), fmt.Sprintf("attacker-key-%d", i), NewLimit(10)); err != nil {
			t.Fatalf("Allow: %v", err)
		}
		if l.Len() > 16 {
			t.Fatalf("local limiter grew to %d keys, exceeding the bound", l.Len())
		}
	}
}

func TestDistributedLimiterConcurrentUseIsRaceFree(t *testing.T) {
	t.Parallel()

	be := &stubBackend{}
	l := NewDistributedLimiter(DistributedLimiterConfig{Backend: be, Replicas: 4, MaxLocalKeys: 8})

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if (g+i)%7 == 0 {
					be.setFail(errors.New("flap"))
				} else {
					be.setFail(nil)
				}
				_, _ = l.Allow(context.Background(), fmt.Sprintf("k%d", i%20), NewLimit(1000))
			}
		}(g)
	}
	wg.Wait()
	if be.callCount() == 0 {
		t.Fatal("the backend was never consulted")
	}
}

func TestDistributedLimiterWithoutBackendIsPurelyLocal(t *testing.T) {
	t.Parallel()

	l := NewDistributedLimiter(DistributedLimiterConfig{Replicas: 1, Clock: NewManualClock(time.Time{})})
	d, err := l.Allow(context.Background(), "k", NewLimit(1))
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !d.Allowed {
		t.Fatal("the first request was rejected by a purely local limiter")
	}
}
