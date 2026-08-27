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

func testBreaker(t *testing.T, clk Clock, mutate func(*BreakerConfig)) *Breaker {
	t.Helper()
	cfg := DefaultBreakerConfig("acquirer-a:authorize")
	cfg.Clock = clk
	if mutate != nil {
		mutate(&cfg)
	}
	return NewBreaker(cfg)
}

// drive records n calls with the given outcome through Execute.
func drive(t *testing.T, b *Breaker, n int, err error) {
	t.Helper()
	for i := 0; i < n; i++ {
		_ = b.Execute(context.Background(), func(context.Context) error { return err })
	}
}

// driveMix records failures and successes with the failures spread evenly through the sequence,
// which is what a steady error rate actually looks like on the wire.
//
// The spreading matters: the breaker re-evaluates on every sample, so a batch of failures
// followed by a batch of successes momentarily presents a 100 % error rate and would open a
// breaker that a genuinely steady 20 % rate leaves closed. Testing with the batched shape would
// assert a property the code does not have and real traffic does not produce.
func driveMix(t *testing.T, b *Breaker, failures, successes int, failErr error) {
	t.Helper()
	total := failures + successes
	emitted := 0
	for i := 0; i < total; i++ {
		if quota := (i + 1) * failures / total; quota > emitted {
			drive(t, b, 1, failErr)
			emitted++
			continue
		}
		drive(t, b, 1, nil)
	}
}

// TestBreakerDoesNotOpenBelowMinimumSampleSize is the most important test in this package.
//
// A breaker that opens on 2 of 2 failures is not a safety device: it is a mechanism for taking a
// perfectly healthy gateway out of the routing plan on the strength of two requests. At low
// volume — a newly-onboarded merchant, a quiet corridor, the first seconds after a deploy — two
// consecutive failures are ordinary noise. The minimum sample size is what makes the error rate
// a measurement rather than a coin flip.
func TestBreakerDoesNotOpenBelowMinimumSampleSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		failures  int
		successes int
	}{
		{"1 of 1 — a 100% error rate on one sample", 1, 0},
		{"2 of 2 — the classic liability", 2, 0},
		{"5 of 5", 5, 0},
		{"10 of 10 — still half the minimum", 10, 0},
		{"19 of 19 — one sample short", 19, 0},
		{"5 failures and 5 successes", 5, 5},
		{"15 failures and 4 successes — 19 samples at 79%", 15, 4},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk := NewManualClock(time.Time{})
			b := testBreaker(t, clk, nil)

			drive(t, b, tc.failures, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))
			drive(t, b, tc.successes, nil)

			if got := b.State(); got != StateClosed {
				t.Fatalf("state = %s after %d failures / %d successes (%d samples, minimum is %d): "+
					"the breaker acted on a sample too small to mean anything",
					got, tc.failures, tc.successes, tc.failures+tc.successes, BreakerMinimumSamples)
			}
			// And the very next request must still be admitted.
			if _, err := b.Allow(); err != nil {
				t.Fatalf("the breaker refused a call: %v", err)
			}
		})
	}
}

// TestBreakerOpensAtTheThreshold: at or above the minimum sample size, an error rate above 25 %
// opens the circuit, and one at or below it does not.
func TestBreakerOpensAtTheThreshold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		failures  int
		successes int
		wantState State
	}{
		{"20 samples at exactly 25% does not open (the threshold is strict)", 5, 15, StateClosed},
		{"20 samples at 30% opens", 6, 14, StateOpen},
		{"20 samples at 100% opens", 20, 0, StateOpen},
		{"40 samples at 20% stays closed", 8, 32, StateClosed},
		{"100 samples at 26% opens", 26, 74, StateOpen},
		{"100 samples at 5% stays closed — that is DEGRADED, not UNHEALTHY", 5, 95, StateClosed},
		{"1000 samples at 15% stays closed — a normal decline-free error rate for a wobbly gateway", 150, 850, StateClosed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk := NewManualClock(time.Time{})
			b := testBreaker(t, clk, nil)

			driveMix(t, b, tc.failures, tc.successes, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))

			if got := b.State(); got != tc.wantState {
				c := b.Counts()
				t.Fatalf("state = %s, want %s (samples=%d rate=%.3f threshold=%.2f)",
					got, tc.wantState, c.Total(), c.ErrorRate(), BreakerFailureRateToOpen)
			}
		})
	}
}

// TestBreakerDeclinesDoNotOpenIt is the rule that keeps a healthy gateway in the routing plan
// when a merchant's customer cohort goes bad.
func TestBreakerDeclinesDoNotOpenIt(t *testing.T) {
	t.Parallel()

	declines := []struct {
		name string
		err  error
	}{
		{"gateway hard decline", apierror.New(apierror.CodeGatewayDeclined, "stolen card")},
		{"risk decline", apierror.New(apierror.CodeRiskDeclined, "risk policy")},
		{"velocity limit", apierror.New(apierror.CodeVelocityLimitExceeded, "too many")},
		{"3DS required", apierror.New(apierror.CodeThreeDsRequired, "sca")},
		{"country blocked", apierror.New(apierror.CodeCountryBlocked, "blocked")},
		{"amount exceeds limit", apierror.New(apierror.CodeAmountExceedsLimit, "limit")},
	}

	for _, tc := range declines {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk := NewManualClock(time.Time{})
			b := testBreaker(t, clk, nil)

			// A card-testing burst: 500 consecutive declines against a gateway with zero errors.
			drive(t, b, 500, tc.err)

			if got := b.State(); got != StateClosed {
				t.Fatalf("state = %s after 500 %s: business declines opened the circuit on a "+
					"healthy gateway, which is exactly how one merchant's bad cohort becomes a "+
					"platform-wide gateway exclusion", got, tc.name)
			}
			if c := b.Counts(); c.Total() != 0 {
				t.Fatalf("declines entered the sample window: %+v", c)
			}
		})
	}
}

// TestBreakerDeclinesDoNotDiluteTheErrorRate: an ignored outcome must not enter the denominator
// either. If declines were counted as successes, a gateway that is 100 % broken for the 10 % of
// traffic it can still reach would look like a 10 % error rate and stay in the plan.
func TestBreakerDeclinesDoNotDiluteTheErrorRate(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := testBreaker(t, clk, nil)

	// A merchant with a 90 % decline rate: 900 declines against 100 real calls.
	drive(t, b, 900, apierror.New(apierror.CodeGatewayDeclined, "declined"))
	if c := b.Counts(); c.Total() != 0 {
		t.Fatalf("sample total = %d after 900 declines, want 0: declines entered the window", c.Total())
	}

	// 19 genuine transport failures: one short of the minimum sample size. If the 900 declines
	// had been counted as *successes* the denominator would be 919 and this would look like a
	// 2 % error rate on a gateway that has failed every real call it was given.
	drive(t, b, 19, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))
	c := b.Counts()
	if c.Total() != 19 {
		t.Fatalf("sample total = %d, want 19: declines diluted the denominator", c.Total())
	}
	if got := c.ErrorRate(); got != 1.0 {
		t.Fatalf("error rate = %.3f, want 1.0", got)
	}
	if got := b.State(); got != StateClosed {
		t.Fatalf("state = %s at 19 samples, want CLOSED", got)
	}

	// The 20th real failure reaches the minimum sample size and opens it.
	drive(t, b, 1, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))
	if got := b.State(); got != StateOpen {
		t.Fatalf("state = %s at 20 samples of 100%% failure, want OPEN", got)
	}
}

func TestBreakerCustomClassifier(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("vendor says this is fine")
	clk := NewManualClock(time.Time{})
	b := testBreaker(t, clk, func(c *BreakerConfig) {
		c.Classify = func(err error) Outcome {
			if errors.Is(err, sentinel) {
				return OutcomeIgnore
			}
			return DefaultClassifier(err)
		}
	})

	drive(t, b, 100, sentinel)
	if got := b.State(); got != StateClosed {
		t.Fatalf("state = %s, want CLOSED: the Classify hook was not consulted", got)
	}
}

func TestBreakerLatencyThresholdOpensIt(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := testBreaker(t, clk, nil)

	// 100 successful calls, 2 of them over the 5 s slow threshold — a 2 % slow rate, above the
	// 1 % that corresponds to "p99 > 5s". Every call succeeds, so the error rate is zero.
	for i := 0; i < 100; i++ {
		slow := i < 2
		_ = b.Execute(context.Background(), func(context.Context) error {
			if slow {
				clk.Advance(BreakerSlowCallDuration + time.Millisecond)
			}
			return nil
		})
	}

	if got := b.State(); got != StateOpen {
		c := b.Counts()
		t.Fatalf("state = %s, want OPEN on the latency trigger (slow=%d total=%d rate=%.3f)",
			got, c.Slow, c.Total(), c.SlowRate())
	}
}

func TestBreakerOpenFailsFastWithTheRightCode(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := testBreaker(t, clk, nil)
	drive(t, b, 25, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))

	called := false
	err := b.Execute(context.Background(), func(context.Context) error {
		called = true
		return nil
	})
	if called {
		t.Fatal("the call was dispatched while the circuit was open")
	}
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("errors.Is(err, ErrCircuitOpen) = false: %v", err)
	}
	if got := apierror.CodeOf(err); got != apierror.CodeGatewayCircuitOpen {
		t.Errorf("code = %s, want %s", got, apierror.CodeGatewayCircuitOpen)
	}
	if !apierror.IsRetryable(err) {
		t.Error("an open circuit must be retryable: the caller should route elsewhere or try again")
	}
}

// TestBreakerHalfOpenAdmitsLimitedProbes: exactly one concurrent probe. Admitting full traffic
// into a recovering gateway re-breaks it, and the recovery attempt becomes the second outage.
func TestBreakerHalfOpenAdmitsLimitedProbes(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := testBreaker(t, clk, nil)
	drive(t, b, 25, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))
	clk.Advance(BreakerBaseCooldown)

	first, err := b.Allow()
	if err != nil {
		t.Fatalf("the first probe was refused after the cool-down: %v", err)
	}
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("state = %s, want HALF_OPEN", got)
	}
	for i := 0; i < 5; i++ {
		if _, err := b.Allow(); err == nil {
			t.Fatalf("a second concurrent probe was admitted (attempt %d): "+
				"half-open must admit exactly %d", i, BreakerHalfOpenProbes)
		}
	}
	// Releasing the probe frees the slot for the next one.
	first(true)
	if _, err := b.Allow(); err != nil {
		t.Fatalf("the probe slot was not released: %v", err)
	}
}

func TestBreakerHalfOpenProbeCountIsConfigurable(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := testBreaker(t, clk, func(c *BreakerConfig) { c.HalfOpenProbes = 3 })
	drive(t, b, 25, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))
	clk.Advance(BreakerBaseCooldown)

	for i := 0; i < 3; i++ {
		if _, err := b.Allow(); err != nil {
			t.Fatalf("probe %d refused: %v", i, err)
		}
	}
	if _, err := b.Allow(); err == nil {
		t.Fatal("a fourth probe was admitted with HalfOpenProbes=3")
	}
}

// TestBreakerClosesAfterConsecutiveSuccesses: three successes, not one. One success could be a
// single healthy node behind a load balancer.
func TestBreakerClosesAfterConsecutiveSuccesses(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := testBreaker(t, clk, nil)
	drive(t, b, 25, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))
	clk.Advance(BreakerBaseCooldown)

	for i := 1; i <= BreakerProbeSuccessesToClose; i++ {
		permit, err := b.Allow()
		if err != nil {
			t.Fatalf("probe %d refused: %v", i, err)
		}
		permit(true)

		want := StateHalfOpen
		if i == BreakerProbeSuccessesToClose {
			want = StateClosed
		}
		if got := b.State(); got != want {
			t.Fatalf("after %d consecutive successes state = %s, want %s", i, got, want)
		}
	}

	// A closed breaker starts from a clean window: the failures that opened it must not be
	// carried across, or the first error after recovery reopens it.
	if c := b.Counts(); c.Total() != 0 {
		t.Fatalf("the window was not reset on close: %+v", c)
	}
	if got := b.Cooldown(); got != BreakerBaseCooldown {
		t.Fatalf("cool-down after close = %v, want the base %v", got, BreakerBaseCooldown)
	}
}

// TestBreakerReopensOnProbeFailureWithDoubledCappedCooldown walks the whole doubling schedule:
// 30 s → 60 s → 120 s → 240 s → 300 s (capped), and asserts the cool-down is actually honoured
// at each step, not merely stored.
func TestBreakerReopensOnProbeFailureWithDoubledCappedCooldown(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := testBreaker(t, clk, nil)
	drive(t, b, 25, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))

	want := []time.Duration{
		30 * time.Second,
		60 * time.Second,
		120 * time.Second,
		240 * time.Second,
		300 * time.Second, // capped at BreakerMaxCooldown
		300 * time.Second,
		300 * time.Second,
	}

	for i, cd := range want {
		if got := b.Cooldown(); got != cd {
			t.Fatalf("cycle %d: cool-down = %v, want %v", i, got, cd)
		}
		// One nanosecond short of the cool-down, the breaker must still be open.
		clk.Advance(cd - time.Nanosecond)
		if got := b.State(); got != StateOpen {
			t.Fatalf("cycle %d: state = %s just before the cool-down elapsed, want OPEN", i, got)
		}
		clk.Advance(time.Nanosecond)
		if got := b.State(); got != StateHalfOpen {
			t.Fatalf("cycle %d: state = %s once the cool-down elapsed, want HALF_OPEN", i, got)
		}

		permit, err := b.Allow()
		if err != nil {
			t.Fatalf("cycle %d: probe refused: %v", i, err)
		}
		permit(false) // the probe fails: reopen, and double
		if got := b.State(); got != StateOpen {
			t.Fatalf("cycle %d: state = %s after a failed probe, want OPEN", i, got)
		}
	}

	if got := b.Cooldown(); got > BreakerMaxCooldown {
		t.Fatalf("cool-down %v exceeded the %v cap", got, BreakerMaxCooldown)
	}
}

// TestBreakerSingleProbeFailureReopensImmediately: HALF_OPEN → OPEN on *any* failure, without
// waiting for a sample-size quorum. The minimum sample size governs CLOSED, not HALF_OPEN,
// because a probe is a deliberate experiment rather than an observation of ambient traffic.
func TestBreakerSingleProbeFailureReopensImmediately(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := testBreaker(t, clk, nil)
	drive(t, b, 25, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))
	clk.Advance(BreakerBaseCooldown)

	// Two successes, then a failure: the consecutive count resets and the breaker reopens.
	for i := 0; i < 2; i++ {
		p, err := b.Allow()
		if err != nil {
			t.Fatalf("probe %d refused: %v", i, err)
		}
		p(true)
	}
	p, err := b.Allow()
	if err != nil {
		t.Fatalf("third probe refused: %v", err)
	}
	p(false)

	if got := b.State(); got != StateOpen {
		t.Fatalf("state = %s after a failed probe, want OPEN", got)
	}
}

// TestBreakerHalfOpenIgnoresDeclines: a probe that comes back declined proves nothing about
// availability. It must neither close the breaker nor reopen it — it releases the slot so the
// next probe can run.
func TestBreakerHalfOpenIgnoresDeclines(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := testBreaker(t, clk, nil)
	drive(t, b, 25, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))
	clk.Advance(BreakerBaseCooldown)

	for i := 0; i < 10; i++ {
		p, err := b.AllowClassified()
		if err != nil {
			t.Fatalf("probe %d refused: %v", i, err)
		}
		p(apierror.New(apierror.CodeGatewayDeclined, "declined"))
		if got := b.State(); got != StateHalfOpen {
			t.Fatalf("probe %d: state = %s after a declined probe, want HALF_OPEN", i, got)
		}
	}
}

func TestBreakerStaleProbeCannotCloseTheCircuit(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := testBreaker(t, clk, func(c *BreakerConfig) { c.ProbeSuccessesToClose = 1 })
	drive(t, b, 25, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))
	clk.Advance(BreakerBaseCooldown)

	stale, err := b.Allow()
	if err != nil {
		t.Fatalf("probe refused: %v", err)
	}
	// The breaker is reset under the in-flight probe, bumping the generation.
	b.Reset()
	drive(t, b, 25, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))
	if got := b.State(); got != StateOpen {
		t.Fatalf("precondition: state = %s, want OPEN", got)
	}

	stale(true) // a success from a previous generation

	if got := b.State(); got != StateOpen {
		t.Fatalf("state = %s: a stale permit from a previous generation closed the circuit", got)
	}
}

func TestBreakerStateChangedCallback(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	var mu sync.Mutex
	var seen []string
	b := testBreaker(t, clk, func(c *BreakerConfig) {
		c.StateChanged = func(name string, from, to State) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, fmt.Sprintf("%s:%s->%s", name, from, to))
		}
	})

	drive(t, b, 25, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))
	clk.Advance(BreakerBaseCooldown)
	for i := 0; i < BreakerProbeSuccessesToClose; i++ {
		p, err := b.Allow()
		if err != nil {
			t.Fatalf("probe %d refused: %v", i, err)
		}
		p(true)
	}

	want := []string{
		"acquirer-a:authorize:CLOSED->OPEN",
		"acquirer-a:authorize:OPEN->HALF_OPEN",
		"acquirer-a:authorize:HALF_OPEN->CLOSED",
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(want) {
		t.Fatalf("transitions = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("transitions = %v, want %v", seen, want)
		}
	}
}

func TestBreakerStateGaugeValues(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		s    State
		n    int
		name string
	}{
		{StateClosed, 0, "CLOSED"},
		{StateOpen, 1, "OPEN"},
		{StateHalfOpen, 2, "HALF_OPEN"},
	} {
		if tc.s.Gauge() != tc.n {
			t.Errorf("%s gauge = %d, want %d (pp_circuit_breaker_state is a published contract)", tc.name, tc.s.Gauge(), tc.n)
		}
		if tc.s.String() != tc.name {
			t.Errorf("String() = %q, want %q", tc.s.String(), tc.name)
		}
	}
}

func TestBreakerHealthProjection(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := testBreaker(t, clk, nil)

	if got := b.Health(); got != HealthHealthy {
		t.Fatalf("health = %s, want HEALTHY", got)
	}

	// 100 samples at 10 %: over the 5 % advisory threshold, under the 25 % opening threshold.
	driveMix(t, b, 10, 90, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))
	if got := b.Health(); got != HealthDegraded {
		t.Fatalf("health = %s at a 10%% error rate, want DEGRADED (advisory, not excluded)", got)
	}
	if got := b.State(); got != StateClosed {
		t.Fatalf("state = %s, want CLOSED: DEGRADED must not exclude the gateway", got)
	}

	drive(t, b, 50, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))
	if got := b.Health(); got != HealthUnhealthy {
		t.Fatalf("health = %s, want UNHEALTHY", got)
	}
	clk.Advance(BreakerBaseCooldown)
	if got := b.Health(); got != HealthProbing {
		t.Fatalf("health = %s after the cool-down, want PROBING", got)
	}
}

func TestBreakerWindowRollsOff(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := testBreaker(t, clk, nil)

	drive(t, b, 19, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))
	if c := b.Counts(); c.Total() != 19 {
		t.Fatalf("counts = %+v, want 19 samples", c)
	}
	// Past the whole 30 s window, everything must have aged out — including the failures that
	// were one sample short of opening it.
	clk.Advance(BreakerWindow + time.Second)
	if c := b.Counts(); c.Total() != 0 {
		t.Fatalf("counts = %+v after the window elapsed, want 0", c)
	}
	// So 19 more failures still do not open it: the old ones cannot be resurrected.
	drive(t, b, 19, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))
	if got := b.State(); got != StateClosed {
		t.Fatalf("state = %s, want CLOSED: aged-out samples were counted", got)
	}
}

func TestBreakerExecuteRepanicsAndReleasesTheProbeSlot(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := testBreaker(t, clk, nil)
	drive(t, b, 25, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))
	clk.Advance(BreakerBaseCooldown)

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("the panic was swallowed")
			}
		}()
		_ = b.Execute(context.Background(), func(context.Context) error { panic("adapter bug") })
	}()

	// The panicking probe counted as a failure, so the breaker reopened; after the (doubled)
	// cool-down a fresh probe must be admitted rather than blocked by a leaked slot.
	if got := b.State(); got != StateOpen {
		t.Fatalf("state = %s after a panicking probe, want OPEN", got)
	}
	clk.Advance(b.Cooldown())
	if _, err := b.Allow(); err != nil {
		t.Fatalf("the probe slot leaked: %v", err)
	}
}

func TestBreakerPermitIsIdempotent(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := testBreaker(t, clk, nil)
	p, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	for i := 0; i < 5; i++ {
		p(false)
	}
	if c := b.Counts(); c.Total() != 1 {
		t.Fatalf("counts = %+v after five permit calls, want exactly one sample", c)
	}
}

func TestBreakerConcurrentExecuteIsRaceFree(t *testing.T) {
	t.Parallel()

	b := testBreaker(t, SystemClock(), nil)
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = b.Execute(context.Background(), func(context.Context) error {
					switch (g + i) % 4 {
					case 0:
						return apierror.New(apierror.CodeGatewayUnavailable, "5xx")
					case 1:
						return apierror.New(apierror.CodeGatewayDeclined, "declined")
					default:
						return nil
					}
				})
				_ = b.State()
				_ = b.Counts()
				_ = b.Degraded()
			}
		}(g)
	}
	wg.Wait()
}

func TestBreakerPresets(t *testing.T) {
	t.Parallel()

	redis := RedisBreakerConfig("redis").normalized()
	if redis.MinimumSamples != 10 || redis.Window != 5*time.Second || redis.BaseCooldown != 5*time.Second {
		t.Errorf("redis preset = %+v, want the aggressive 5s/10-sample/5s-cooldown shape", redis)
	}
	vendor := VendorBreakerConfig("kyc").normalized()
	if vendor.MinimumSamples != 10 || vendor.Window != 60*time.Second || vendor.FailureRateToOpen != 0.50 {
		t.Errorf("vendor preset = %+v, want the patient 60s/10-sample/50%% shape", vendor)
	}
}

// --- registry -------------------------------------------------------------------------------

func TestBreakerRegistryReturnsTheSameBreakerPerKey(t *testing.T) {
	t.Parallel()

	r := NewBreakerRegistry(BreakerRegistryConfig{Clock: NewManualClock(time.Time{})})
	defer r.Close()

	a := r.Get(BreakerKey("acquirer-a", "authorize"))
	b := r.Get(BreakerKey("acquirer-a", "authorize"))
	if a != b {
		t.Fatal("the registry handed out two breakers for one key: their windows would each see half the traffic")
	}
	if c := r.Get(BreakerKey("acquirer-a", "refund")); c == a {
		t.Fatal("two operations share one breaker: one failing operation would exclude the gateway's others")
	}
	if r.Len() != 2 {
		t.Fatalf("len = %d, want 2", r.Len())
	}
}

func TestBreakerRegistryEvictsIdleKeys(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	var evicted []string
	r := NewBreakerRegistry(BreakerRegistryConfig{
		Clock:   clk,
		IdleTTL: 10 * time.Minute,
		Evicted: func(key string) { evicted = append(evicted, key) },
	})
	defer r.Close()

	r.Get("a:authorize")
	r.Get("b:authorize")
	clk.Advance(5 * time.Minute)
	r.Get("a:authorize") // refreshes a only
	clk.Advance(6 * time.Minute)

	if n := r.EvictIdle(); n != 1 {
		t.Fatalf("evicted %d keys, want 1", n)
	}
	if r.Len() != 1 {
		t.Fatalf("len = %d, want 1", r.Len())
	}
	if len(evicted) != 1 || evicted[0] != "b:authorize" {
		t.Fatalf("evicted = %v, want [b:authorize]: the recently-used key was dropped instead", evicted)
	}
}

// TestBreakerRegistryBoundsCardinality is the memory-exhaustion defence: an attacker who can
// influence the key must not be able to grow the registry without limit.
func TestBreakerRegistryBoundsCardinality(t *testing.T) {
	t.Parallel()

	const maxKeys = 16
	r := NewBreakerRegistry(BreakerRegistryConfig{
		Clock:   NewManualClock(time.Time{}),
		MaxKeys: maxKeys,
	})
	defer r.Close()

	for i := 0; i < 10000; i++ {
		r.Get(fmt.Sprintf("attacker-supplied-gateway-%d:authorize", i))
		if got := r.Len(); got > maxKeys {
			t.Fatalf("after %d distinct keys the registry holds %d, exceeding MaxKeys=%d", i, got, maxKeys)
		}
	}
	if got := r.Len(); got != maxKeys {
		t.Fatalf("len = %d, want %d", got, maxKeys)
	}
}

func TestBreakerRegistryEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	r := NewBreakerRegistry(BreakerRegistryConfig{Clock: clk, MaxKeys: 3})
	defer r.Close()

	r.Get("k1")
	r.Get("k2")
	r.Get("k3")
	kept := r.Get("k1") // k1 becomes most recent; k2 is now the oldest
	r.Get("k4")         // must evict k2

	if again := r.Get("k1"); again != kept {
		t.Fatal("k1 was evicted despite being the most recently used")
	}
	states := r.States()
	if _, ok := states["k2"]; ok {
		t.Fatal("k2 survived: eviction is not LRU")
	}
}

func TestBreakerRegistryStatesSnapshot(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	r := NewBreakerRegistry(BreakerRegistryConfig{
		Clock: clk,
		Configure: func(key string) BreakerConfig {
			c := DefaultBreakerConfig(key)
			c.Clock = clk
			return c
		},
	})
	defer r.Close()

	drive(t, r.Get("a:authorize"), 25, apierror.New(apierror.CodeGatewayUnavailable, "5xx"))
	r.Get("b:authorize")

	states := r.States()
	if states["a:authorize"] != StateOpen {
		t.Errorf("a:authorize = %s, want OPEN", states["a:authorize"])
	}
	if states["b:authorize"] != StateClosed {
		t.Errorf("b:authorize = %s, want CLOSED", states["b:authorize"])
	}
}

// TestBreakerRegistrySweeperStopsOnClose is the goroutine-ownership contract: the one
// background goroutine this package creates must not outlive its owner.
func TestBreakerRegistrySweeperStopsOnClose(t *testing.T) {
	assertNoGoroutineLeaks(t)

	clk := NewManualClock(time.Time{})
	r := NewBreakerRegistry(BreakerRegistryConfig{
		Clock:         clk,
		IdleTTL:       time.Millisecond,
		SweepInterval: time.Millisecond,
	})
	r.Get("a:authorize")
	clk.Advance(time.Second)

	// Give the sweeper a few cycles so the test exercises a running goroutine rather than one
	// that has not started.
	time.Sleep(20 * time.Millisecond)
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close is idempotent: a double Close in a shutdown path must not panic.
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestBreakerRegistryWithoutSweeperStartsNoGoroutine(t *testing.T) {
	assertNoGoroutineLeaks(t)

	r := NewBreakerRegistry(BreakerRegistryConfig{Clock: NewManualClock(time.Time{})})
	r.Get("a:authorize")
	// No Close at all: a registry with no sweeper owns nothing that could leak.
	_ = r.Len()
}

func TestBreakerRegistryConcurrentGetIsRaceFree(t *testing.T) {
	t.Parallel()

	r := NewBreakerRegistry(BreakerRegistryConfig{MaxKeys: 8})
	defer r.Close()

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				b := r.Get(fmt.Sprintf("gw-%d:op", (g+i)%20))
				_ = b.Execute(context.Background(), func(context.Context) error { return nil })
			}
		}(g)
	}
	wg.Wait()
	if r.Len() > 8 {
		t.Fatalf("len = %d, want at most 8", r.Len())
	}
}

func TestDefaultClassifier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want Outcome
	}{
		{"nil is success", nil, OutcomeSuccess},
		{"gateway unavailable is a failure", apierror.New(apierror.CodeGatewayUnavailable, ""), OutcomeFailure},
		{"gateway timeout is a failure", apierror.New(apierror.CodeGatewayTimeout, ""), OutcomeFailure},
		{"dependency failure is a failure", apierror.New(apierror.CodeDependencyFailure, ""), OutcomeFailure},
		{"gateway auth failure is a failure", apierror.New(apierror.CodeGatewayAuthenticationFailed, ""), OutcomeFailure},
		{"contract violation is a failure", apierror.New(apierror.CodeGatewayContractViolation, ""), OutcomeFailure},
		{"gateway decline is ignored", apierror.New(apierror.CodeGatewayDeclined, ""), OutcomeIgnore},
		{"risk decline is ignored", apierror.New(apierror.CodeRiskDeclined, ""), OutcomeIgnore},
		{"validation failure is ignored", apierror.New(apierror.CodeValidationFailed, ""), OutcomeIgnore},
		{"not found is ignored", apierror.New(apierror.CodePaymentNotFound, ""), OutcomeIgnore},
		{"conflict is ignored", apierror.New(apierror.CodePaymentAlreadyProcessed, ""), OutcomeIgnore},
		{"context cancellation is ignored", context.Canceled, OutcomeIgnore},
		{"deadline exceeded is a failure", context.DeadlineExceeded, OutcomeFailure},
		{"an unclassified error is a failure", errors.New("boom"), OutcomeFailure},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DefaultClassifier(tc.err); got != tc.want {
				t.Fatalf("DefaultClassifier(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
