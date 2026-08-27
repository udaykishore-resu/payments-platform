package resilience

import (
	"context"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// tolerance absorbs the microseconds between the two clock reads inside a Cascade call. It is
// three orders of magnitude below every duration under test, so it cannot hide an arithmetic
// error and cannot make the test flaky.
const tolerance = 20 * time.Millisecond

func closeEnough(got, want time.Duration) bool {
	d := got - want
	return d < tolerance && d > -tolerance
}

// TestTimeoutCascadeIsStrictlyDecreasing is the rule of docs/failure-handling.md §2.1 asserted
// against the constants themselves. Each layer must leave the one above it room to answer; a
// change that makes two of these equal is the equal-timeout mistake, and its symptom is a
// TIMEOUT_UNKNOWN on a payment that actually completed.
func TestTimeoutCascadeIsStrictlyDecreasing(t *testing.T) {
	t.Parallel()

	layers := []struct {
		name string
		d    time.Duration
	}{
		{"client → ALB idle", TimeoutClientALBIdle},
		{"ALB → payment-api", TimeoutALBToAPI},
		{"payment-api request deadline", TimeoutAPIRequest},
		{"payment-api → orchestrator", TimeoutAPIToOrchestrator},
		{"orchestrator internal", TimeoutOrchestratorInternal},
		{"orchestrator → gateway", TimeoutGatewayAttempt},
	}
	for i := 1; i < len(layers); i++ {
		if layers[i].d >= layers[i-1].d {
			t.Errorf("%s (%v) is not strictly less than %s (%v)",
				layers[i].name, layers[i].d, layers[i-1].name, layers[i-1].d)
		}
	}

	// The 8 s hard timeout is what makes the "at most 2 attempts" arithmetic work:
	// 8 + jitter + 8 + jitter + 8 ≈ 26 s does not fit inside 18 s, but two attempts do.
	twoAttempts := 2*TimeoutGatewayAttempt + InRequestBackoffCap
	if twoAttempts > TimeoutOrchestratorInternal {
		t.Errorf("two gateway attempts plus a full backoff is %v, which does not fit the %v orchestrator budget",
			twoAttempts, TimeoutOrchestratorInternal)
	}
	threeAttempts := 3*TimeoutGatewayAttempt + 2*InRequestBackoffCap
	if threeAttempts <= TimeoutOrchestratorInternal {
		t.Error("three gateway attempts fit inside the orchestrator budget; the retry count of 2 " +
			"is supposed to be forced by this arithmetic")
	}

	// The gateway connect and TLS budgets are sub-budgets of the attempt, not competitors to it.
	if TimeoutGatewayConnect+TimeoutGatewayTLSHandshake >= TimeoutGatewayAttempt {
		t.Error("connect + TLS consumes the whole gateway attempt budget")
	}
	// Redis must be faster than the Postgres fallback it accelerates, or it is worse than useless.
	if TimeoutRedisOp >= TimeoutPostgresRead {
		t.Errorf("the Redis timeout %v is not below the Postgres read timeout %v", TimeoutRedisOp, TimeoutPostgresRead)
	}
}

func TestCascadeBudget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		headroom  time.Duration
		minUseful time.Duration
		remaining time.Duration // parent budget; 0 means "no deadline"
		want      time.Duration
		wantErr   bool
		wantCode  apierror.Code
	}{
		{
			name: "the full ask fits", headroom: 2 * time.Second, minUseful: 500 * time.Millisecond,
			remaining: 18 * time.Second, want: TimeoutGatewayAttempt,
		},
		{
			name: "the parent's remaining budget clamps the ask", headroom: 2 * time.Second, minUseful: 500 * time.Millisecond,
			remaining: 6 * time.Second, want: 4 * time.Second,
		},
		{
			// Just above the floor: headroom + minUseful + 100 ms, so the call starts with the
			// smallest budget the cascade considers worth spending.
			name: "just above the floor still starts", headroom: 2 * time.Second, minUseful: 500 * time.Millisecond,
			remaining: 2600 * time.Millisecond, want: 600 * time.Millisecond,
		},
		{
			name: "300ms of budget refuses pre-emptively", headroom: 2 * time.Second, minUseful: 500 * time.Millisecond,
			remaining: 300 * time.Millisecond, wantErr: true, wantCode: apierror.CodeGatewayTimeout,
		},
		{
			name: "just below the floor refuses", headroom: 2 * time.Second, minUseful: 500 * time.Millisecond,
			remaining: 2400 * time.Millisecond, wantErr: true, wantCode: apierror.CodeGatewayTimeout,
		},
		{
			name: "an expired parent refuses", headroom: 0, minUseful: 0,
			remaining: -time.Second, wantErr: true,
		},
		{
			name: "no parent deadline hands back the ask", headroom: 2 * time.Second, minUseful: 500 * time.Millisecond,
			remaining: 0, want: TimeoutGatewayAttempt,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := NewCascade(tc.headroom, tc.minUseful)

			ctx := context.Background()
			if tc.remaining != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(tc.remaining))
				defer cancel()
			}

			got, err := c.Budget(ctx, TimeoutGatewayAttempt)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got a budget of %v", got)
				}
				if tc.wantCode != "" {
					if code := apierror.CodeOf(err); code != tc.wantCode {
						t.Fatalf("code = %s, want %s", code, tc.wantCode)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Budget: %v", err)
			}
			if !closeEnough(got, tc.want) {
				t.Fatalf("budget = %v, want %v (±%v)", got, tc.want, tolerance)
			}
		})
	}
}

// TestCascadeChildDeadlineIsStrictlyLessThanTheParent is the property the whole type exists to
// guarantee. A child that outlives its parent produces orphaned work and, on a payment path,
// genuine ambiguity about whether money moved.
func TestCascadeChildDeadlineIsStrictlyLessThanTheParent(t *testing.T) {
	t.Parallel()

	for _, remaining := range []time.Duration{
		20 * time.Second, 18 * time.Second, 9 * time.Second, 5 * time.Second, 3 * time.Second,
	} {
		parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(remaining))

		c := DefaultCascade()
		child, childCancel, err := c.Child(parent, TimeoutGatewayAttempt)
		if err != nil {
			cancel()
			t.Fatalf("remaining %v: Child: %v", remaining, err)
		}

		pd, _ := parent.Deadline()
		cd, ok := child.Deadline()
		if !ok {
			t.Fatal("the child has no deadline")
		}
		if !cd.Before(pd) {
			t.Fatalf("remaining %v: the child deadline %v is not strictly before the parent's %v",
				remaining, cd, pd)
		}
		// And the gap is at least the headroom, not merely a nanosecond.
		if gap := pd.Sub(cd); gap < c.Headroom()-tolerance {
			t.Fatalf("remaining %v: the gap between the deadlines is %v, want at least the %v headroom",
				remaining, gap, c.Headroom())
		}
		childCancel()
		cancel()
	}
}

func TestCascadeChildReturnsAUsableCancelOnError(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	c := DefaultCascade()
	ctx, childCancel, err := c.Child(parent, TimeoutGatewayAttempt)
	if err == nil {
		t.Fatal("want a pre-emptive refusal")
	}
	if ctx == nil {
		t.Fatal("Child returned a nil context on error")
	}
	if childCancel == nil {
		t.Fatal("Child returned a nil cancel on error: `defer cancel()` before the error check would panic")
	}
	childCancel() // must not panic
}

func TestCascadeCheck(t *testing.T) {
	t.Parallel()

	c := NewCascade(2*time.Second, 500*time.Millisecond)

	t.Run("plenty of budget", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
		defer cancel()
		if err := c.Check(ctx); err != nil {
			t.Fatalf("Check: %v", err)
		}
	})

	t.Run("no deadline at all", func(t *testing.T) {
		t.Parallel()
		if err := c.Check(context.Background()); err != nil {
			t.Fatalf("Check: %v", err)
		}
	})

	t.Run("insufficient budget refuses with GATEWAY_TIMEOUT", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := c.Check(ctx)
		if err == nil {
			t.Fatal("want a refusal")
		}
		if got := apierror.CodeOf(err); got != apierror.CodeGatewayTimeout {
			t.Fatalf("code = %s, want %s", got, apierror.CodeGatewayTimeout)
		}
		// Retryable=false is the load-bearing half: baseline A7 forbids automatically retrying
		// an operation whose outcome is unknown, and a caller must not learn otherwise from the
		// one case where it would have been safe.
		if apierror.IsRetryable(err) {
			t.Error("a GATEWAY_TIMEOUT was reported as retryable")
		}
		if got := apierror.HTTPStatusOf(err); got != 504 {
			t.Errorf("status = %d, want 504", got)
		}
	})

	t.Run("a cancelled context refuses", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := c.Check(ctx); err == nil {
			t.Fatal("want a refusal on a cancelled context")
		}
	})
}

func TestCascadeRemaining(t *testing.T) {
	t.Parallel()

	c := DefaultCascade()

	if _, ok := c.Remaining(context.Background()); ok {
		t.Error("a context with no deadline reported one")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, ok := c.Remaining(ctx)
	if !ok {
		t.Fatal("a context with a deadline reported none")
	}
	if !closeEnough(got, 5*time.Second) {
		t.Fatalf("remaining = %v, want 5s (±%v)", got, tolerance)
	}
}

func TestCascadeAccessors(t *testing.T) {
	t.Parallel()

	c := NewCascade(-1, -1) // negatives are corrected, not rejected
	if c.Headroom() != 0 || c.MinUseful() != 0 {
		t.Fatalf("headroom=%v minUseful=%v, want both clamped to 0", c.Headroom(), c.MinUseful())
	}

	g := GatewayCascade()
	if g.Headroom() != DefaultCascadeHeadroom || g.MinUseful() != DefaultMinUsefulTime {
		t.Fatalf("gateway cascade = (%v, %v), want (%v, %v)",
			g.Headroom(), g.MinUseful(), DefaultCascadeHeadroom, DefaultMinUsefulTime)
	}
}

// TestCascadeComposesWithRetry: the per-attempt budget the cascade derives is what Do uses, and
// the two together must never start an attempt the parent cannot wait for.
func TestCascadeComposesWithRetry(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	c := DefaultCascade()
	budget, err := c.Budget(parent, TimeoutGatewayAttempt)
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}

	p := DefaultPolicy()
	p.Timeout = budget
	p.MaxAttempts = 3
	p.Backoff = zeroBackoff()

	var deadlines []time.Duration
	_, _ = DoAttempts(parent, p, func(ctx context.Context) error {
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("the attempt context has no deadline")
			return nil
		}
		deadlines = append(deadlines, time.Until(d))
		return retryableErr()
	})

	pd, _ := parent.Deadline()
	for i, d := range deadlines {
		if d > time.Until(pd)+tolerance {
			t.Fatalf("attempt %d had a deadline %v beyond the parent's remaining budget", i, d)
		}
	}
}
