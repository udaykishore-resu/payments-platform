package resilience

import (
	"sync"
	"testing"
	"time"
)

func TestExponentialBackoffCeiling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		base       time.Duration
		ceiling    time.Duration
		multiplier float64
		attempt    int
		want       time.Duration
	}{
		{"attempt 0 is the base", 100 * time.Millisecond, 2 * time.Second, 2, 0, 100 * time.Millisecond},
		{"attempt 1 doubles", 100 * time.Millisecond, 2 * time.Second, 2, 1, 200 * time.Millisecond},
		{"attempt 2 doubles again", 100 * time.Millisecond, 2 * time.Second, 2, 2, 400 * time.Millisecond},
		{"attempt 3", 100 * time.Millisecond, 2 * time.Second, 2, 3, 800 * time.Millisecond},
		{"attempt 4", 100 * time.Millisecond, 2 * time.Second, 2, 4, 1600 * time.Millisecond},
		{"attempt 5 hits the cap", 100 * time.Millisecond, 2 * time.Second, 2, 5, 2 * time.Second},
		{"attempt 20 stays at the cap", 100 * time.Millisecond, 2 * time.Second, 2, 20, 2 * time.Second},
		{"absurd attempt count does not overflow", 100 * time.Millisecond, 2 * time.Second, 2, 1 << 30, 2 * time.Second},
		{"negative attempt is treated as zero", 100 * time.Millisecond, 2 * time.Second, 2, -3, 100 * time.Millisecond},
		{"workflow cap", time.Second, WorkflowBackoffCap, 2, 10, WorkflowBackoffCap},
		{"dlq cap", 5 * time.Second, DLQBackoffCap, 2, 20, DLQBackoffCap},
		{"multiplier below one is clamped to flat", 100 * time.Millisecond, 2 * time.Second, 0.5, 5, 100 * time.Millisecond},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := NewExponentialBackoff(tc.base, tc.ceiling, tc.multiplier)
			if got := b.Ceiling(tc.attempt); got != tc.want {
				t.Fatalf("Ceiling(%d) = %v, want %v", tc.attempt, got, tc.want)
			}
		})
	}
}

// TestExponentialBackoffBoundsHold is the safety property: no draw may ever exceed the
// documented ceiling, at any attempt, ever. A backoff that occasionally returns more than its
// cap silently blows the deadline arithmetic in Do.
func TestExponentialBackoffBoundsHold(t *testing.T) {
	t.Parallel()

	b := DefaultBackoff()
	for attempt := 0; attempt < 12; attempt++ {
		upper := b.Ceiling(attempt)
		for i := 0; i < 2000; i++ {
			d := b.Delay(attempt)
			if d < 0 {
				t.Fatalf("attempt %d: negative delay %v", attempt, d)
			}
			if d > upper {
				t.Fatalf("attempt %d: delay %v exceeds ceiling %v", attempt, d, upper)
			}
			if d > InRequestBackoffCap {
				t.Fatalf("attempt %d: delay %v exceeds the in-request cap %v", attempt, d, InRequestBackoffCap)
			}
		}
	}
}

// TestExponentialBackoffJitterDistributes asserts the property full jitter exists for: draws
// spread across the *whole* interval [0, ceiling], not a band at the top of it.
//
// Equal jitter would leave the bottom half of every histogram empty — which is precisely the
// wasted recovery capacity described on ExponentialBackoff — so the test asserts that every
// decile of the interval is populated and that no decile holds a wildly disproportionate share.
func TestExponentialBackoffJitterDistributes(t *testing.T) {
	t.Parallel()

	const (
		attempt = 3
		draws   = 20000
		deciles = 10
	)
	b := NewSeededExponentialBackoff(DefaultBackoffBase, InRequestBackoffCap, DefaultBackoffMultiplier, 0xC0FFEE)
	upper := b.Ceiling(attempt) // 800ms
	if upper != 800*time.Millisecond {
		t.Fatalf("precondition: ceiling = %v, want 800ms", upper)
	}

	var hist [deciles]int
	for i := 0; i < draws; i++ {
		d := b.Delay(attempt)
		bucket := int(int64(d) * deciles / int64(upper))
		if bucket >= deciles {
			bucket = deciles - 1 // the inclusive upper endpoint
		}
		hist[bucket]++
	}

	expected := draws / deciles
	for i, n := range hist {
		if n == 0 {
			t.Fatalf("decile %d of [0,%v] never drawn: this is equal jitter, not full jitter (hist=%v)", i, upper, hist)
		}
		// A uniform draw over 20 000 samples puts ~2 000 in each decile. A ±40 % band is far
		// wider than the sampling noise (σ ≈ 42) and far narrower than any non-uniform shape.
		if n < expected*6/10 || n > expected*14/10 {
			t.Fatalf("decile %d holds %d draws, want roughly %d (hist=%v)", i, n, expected, hist)
		}
	}
}

// TestExponentialBackoffFullJitterReachesBothEnds asserts the two endpoints that distinguish
// full jitter from every alternative: a draw may be (near) zero, and a draw may be (near) the
// full interval.
func TestExponentialBackoffFullJitterReachesBothEnds(t *testing.T) {
	t.Parallel()

	b := NewSeededExponentialBackoff(DefaultBackoffBase, InRequestBackoffCap, DefaultBackoffMultiplier, 7)
	upper := b.Ceiling(2) // 400ms
	var sawLow, sawHigh bool
	for i := 0; i < 50000 && (!sawLow || !sawHigh); i++ {
		d := b.Delay(2)
		if d < upper/20 {
			sawLow = true
		}
		if d > upper*19/20 {
			sawHigh = true
		}
	}
	if !sawLow {
		t.Error("no draw in the bottom 5% of the interval: the deterministic floor of equal jitter is present")
	}
	if !sawHigh {
		t.Error("no draw in the top 5% of the interval")
	}
}

// TestExponentialBackoffDeterministicWithFixedSeed is what makes every other timing test in this
// package reproducible.
func TestExponentialBackoffDeterministicWithFixedSeed(t *testing.T) {
	t.Parallel()

	seq := func(seed uint64) []time.Duration {
		b := NewSeededExponentialBackoff(DefaultBackoffBase, InRequestBackoffCap, DefaultBackoffMultiplier, seed)
		out := make([]time.Duration, 0, 20)
		for i := 0; i < 20; i++ {
			out = append(out, b.Delay(i%6))
		}
		return out
	}

	a, b := seq(42), seq(42)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("same seed produced different sequences at %d: %v vs %v", i, a[i], b[i])
		}
	}

	c := seq(43)
	same := true
	for i := range a {
		if a[i] != c[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different seeds produced identical sequences: the seed is not reaching the generator")
	}
}

// TestExponentialBackoffConcurrentDelayIsRaceFree covers both modes: the seeded one, whose
// generator is behind a mutex, and the unseeded one, which must be safe without holding a lock
// at all — the property that keeps it cheap during a retry storm.
func TestExponentialBackoffConcurrentDelayIsRaceFree(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		b    *ExponentialBackoff
	}{
		{"unseeded (per-P source, no lock)", DefaultBackoff()},
		{"seeded (mutex-guarded)", NewSeededExponentialBackoff(DefaultBackoffBase, InRequestBackoffCap, 2, 99)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var wg sync.WaitGroup
			for g := 0; g < 16; g++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < 500; i++ {
						if d := tc.b.Delay(i % 8); d < 0 || d > InRequestBackoffCap {
							t.Errorf("out-of-range delay %v", d)
							return
						}
					}
				}()
			}
			wg.Wait()
		})
	}
}

func TestBackoffFuncAdapter(t *testing.T) {
	t.Parallel()
	f := BackoffFunc(func(attempt int) time.Duration { return time.Duration(attempt) * time.Second })
	if got := f.Delay(3); got != 3*time.Second {
		t.Fatalf("Delay(3) = %v, want 3s", got)
	}
}

func TestFullJitterHelperRespectsCap(t *testing.T) {
	t.Parallel()
	for i := 0; i < 1000; i++ {
		if d := FullJitter(9, DefaultBackoffBase, InRequestBackoffCap); d > InRequestBackoffCap || d < 0 {
			t.Fatalf("FullJitter out of range: %v", d)
		}
	}
}

func TestDefaultBackoffMatchesDocumentedParameters(t *testing.T) {
	t.Parallel()
	b := DefaultBackoff()
	if b.Base() != 100*time.Millisecond {
		t.Errorf("base = %v, want 100ms (docs/failure-handling.md §2.3)", b.Base())
	}
	if b.Cap() != 2*time.Second {
		t.Errorf("cap = %v, want 2s (docs/failure-handling.md §2.3)", b.Cap())
	}
}
