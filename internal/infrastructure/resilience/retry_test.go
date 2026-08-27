package resilience

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

func retryableErr() error {
	return apierror.New(apierror.CodeGatewayUnavailable, "gateway is unavailable")
}

func nonRetryableErr() error {
	return apierror.New(apierror.CodeGatewayDeclined, "declined")
}

// zeroBackoff removes the wait from tests that are about the loop, not the timing.
func zeroBackoff() Backoff { return BackoffFunc(func(int) time.Duration { return 0 }) }

func TestDoAttemptCounting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		maxAttempts  int
		results      []error // one per call; the loop repeats the last
		wantAttempts int
		wantErr      bool
		wantCode     apierror.Code
	}{
		{
			name: "succeeds first time", maxAttempts: 3,
			results: []error{nil}, wantAttempts: 1,
		},
		{
			name: "succeeds on the second attempt", maxAttempts: 3,
			results: []error{retryableErr(), nil}, wantAttempts: 2,
		},
		{
			name: "succeeds on the last permitted attempt", maxAttempts: 3,
			results: []error{retryableErr(), retryableErr(), nil}, wantAttempts: 3,
		},
		{
			name: "stops at max attempts", maxAttempts: 3,
			results: []error{retryableErr()}, wantAttempts: 3,
			wantErr: true, wantCode: apierror.CodeGatewayUnavailable,
		},
		{
			name: "max attempts of one never retries", maxAttempts: 1,
			results: []error{retryableErr()}, wantAttempts: 1,
			wantErr: true, wantCode: apierror.CodeGatewayUnavailable,
		},
		{
			name: "zero max attempts is corrected to one", maxAttempts: 0,
			results: []error{retryableErr()}, wantAttempts: 1,
			wantErr: true, wantCode: apierror.CodeGatewayUnavailable,
		},
		{
			name: "non-retryable error is not retried", maxAttempts: 5,
			results: []error{nonRetryableErr()}, wantAttempts: 1,
			wantErr: true, wantCode: apierror.CodeGatewayDeclined,
		},
		{
			name: "a retryable error followed by a non-retryable one stops immediately", maxAttempts: 5,
			results: []error{retryableErr(), nonRetryableErr()}, wantAttempts: 2,
			wantErr: true, wantCode: apierror.CodeGatewayDeclined,
		},
		{
			name: "an unclassified error defaults to non-retryable", maxAttempts: 5,
			results: []error{errors.New("something nobody has reasoned about")}, wantAttempts: 1,
			wantErr: true, wantCode: apierror.CodeInternalError,
		},
		{
			name: "a gateway timeout is never retried (baseline A7)", maxAttempts: 5,
			results:      []error{apierror.New(apierror.CodeGatewayTimeout, "no response")},
			wantAttempts: 1, wantErr: true, wantCode: apierror.CodeGatewayTimeout,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := DefaultPolicy()
			p.MaxAttempts = tc.maxAttempts
			p.Backoff = zeroBackoff()

			calls := 0
			attempts, err := DoAttempts(context.Background(), p, func(context.Context) error {
				i := calls
				calls++
				if i >= len(tc.results) {
					i = len(tc.results) - 1
				}
				return tc.results[i]
			})

			if attempts != tc.wantAttempts {
				t.Errorf("attempts = %d, want %d", attempts, tc.wantAttempts)
			}
			if calls != tc.wantAttempts {
				t.Errorf("fn called %d times, want %d", calls, tc.wantAttempts)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatal("want an error, got nil")
				}
				if got := apierror.CodeOf(err); got != tc.wantCode {
					t.Errorf("code = %s, want %s", got, tc.wantCode)
				}
			} else if err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}

// TestDoWrappedErrorPreservesIdentity: the returned error must still unwrap to what the callee
// returned, or every `errors.Is` in the calling code silently stops matching.
func TestDoWrappedErrorPreservesIdentity(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("the original cause")
	p := DefaultPolicy()
	p.MaxAttempts = 2
	p.Backoff = zeroBackoff()
	p.RetryableFunc = func(error) bool { return true }

	err := Do(context.Background(), p, func(context.Context) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is lost the cause: %v", err)
	}

	var pe *apierror.Error
	if !errors.As(err, &pe) {
		t.Fatal("the returned error is not an *apierror.Error")
	}
	if len(pe.Details) == 0 || pe.Details[0].Code != "RETRY_GAVE_UP" {
		t.Fatalf("the attempt count detail is missing: %+v", pe.Details)
	}
}

// TestDoWrapDoesNotMutateTheCallersError: WithDetail copies, and it must, or a shared sentinel
// error value would accumulate a detail on every retry in the process.
func TestDoWrapDoesNotMutateTheCallersError(t *testing.T) {
	t.Parallel()

	shared := apierror.New(apierror.CodeGatewayUnavailable, "shared sentinel")
	p := DefaultPolicy()
	p.MaxAttempts = 2
	p.Backoff = zeroBackoff()

	for i := 0; i < 3; i++ {
		_ = Do(context.Background(), p, func(context.Context) error { return shared })
	}
	if len(shared.Details) != 0 {
		t.Fatalf("the caller's error was mutated: %+v", shared.Details)
	}
}

func TestDoRespectsAlreadyCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	p := DefaultPolicy()
	p.Backoff = zeroBackoff()
	attempts, err := DoAttempts(ctx, p, func(context.Context) error {
		calls++
		return nil
	})
	if calls != 0 {
		t.Errorf("fn was called %d times on a cancelled context, want 0", calls)
	}
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0", attempts)
	}
	if err == nil {
		t.Fatal("want an error for a cancelled context")
	}
}

// TestDoRespectsCancellationMidBackoff cancels the context from inside the sleep, which is the
// interleaving that matters: the attempt has failed, the retry has been authorized, and the
// caller gives up while we are waiting.
func TestDoRespectsCancellationMidBackoff(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clk.OnSleep(func(time.Duration) { cancel() })

	p := DefaultPolicy()
	p.Clock = clk
	p.MaxAttempts = 5
	p.Backoff = BackoffFunc(func(int) time.Duration { return time.Second })

	calls := 0
	attempts, err := DoAttempts(ctx, p, func(context.Context) error {
		calls++
		return retryableErr()
	})

	if calls != 1 {
		t.Errorf("fn called %d times, want 1: the cancellation during the backoff was ignored", calls)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if err == nil {
		t.Fatal("want an error")
	}
	if slept := clk.Slept(); len(slept) != 1 || slept[0] != time.Second {
		t.Errorf("slept = %v, want exactly one 1s backoff", slept)
	}
}

// TestDoDoesNotSleepPastTheDeadline is the arithmetic from the timeout cascade: a backoff that
// does not fit inside the remaining budget must not be slept at all. Sleeping to the deadline
// converts a usable, classified error into a bare DeadlineExceeded and burns the whole
// remaining budget to do it.
func TestDoDoesNotSleepPastTheDeadline(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	p := DefaultPolicy()
	p.MaxAttempts = 5
	// A backoff far larger than the remaining budget: the deadline check, not the sleep, must
	// be what stops the loop.
	p.Backoff = BackoffFunc(func(int) time.Duration { return 30 * time.Second })

	start := time.Now()
	calls := 0
	attempts, err := DoAttempts(ctx, p, func(context.Context) error {
		calls++
		return retryableErr()
	})
	elapsed := time.Since(start)

	if calls != 1 {
		t.Errorf("fn called %d times, want 1", calls)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if elapsed > time.Second {
		t.Errorf("Do took %v: it slept past the deadline instead of refusing to start the wait", elapsed)
	}
	if err == nil {
		t.Fatal("want an error")
	}
	if got := apierror.CodeOf(err); got != apierror.CodeGatewayUnavailable {
		t.Errorf("code = %s, want the underlying %s rather than a deadline error",
			got, apierror.CodeGatewayUnavailable)
	}
}

// TestDoSleepsWhenTheBackoffFits is the other half of the same rule: a backoff that comfortably
// fits must actually be taken.
func TestDoSleepsWhenTheBackoffFits(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	clk := NewManualClock(time.Now())
	p := DefaultPolicy()
	p.Clock = clk
	p.MaxAttempts = 3
	p.Backoff = BackoffFunc(func(attempt int) time.Duration {
		return time.Duration(attempt+1) * 100 * time.Millisecond
	})

	_, err := DoAttempts(ctx, p, func(context.Context) error { return retryableErr() })
	if err == nil {
		t.Fatal("want an error")
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}
	got := clk.Slept()
	if len(got) != len(want) {
		t.Fatalf("slept %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("slept %v, want %v", got, want)
		}
	}
}

func TestDoNoSleepAfterTheFinalAttempt(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	p := DefaultPolicy()
	p.Clock = clk
	p.MaxAttempts = 3
	p.Backoff = BackoffFunc(func(int) time.Duration { return time.Second })

	_, _ = DoAttempts(context.Background(), p, func(context.Context) error { return retryableErr() })

	if n := len(clk.Slept()); n != 2 {
		t.Fatalf("slept %d times for 3 attempts, want 2: a sleep after the last attempt is pure latency", n)
	}
}

func TestDoAppliesPerAttemptTimeout(t *testing.T) {
	t.Parallel()

	p := DefaultPolicy()
	p.MaxAttempts = 1
	p.Timeout = 25 * time.Millisecond

	var deadlineSeen bool
	_, _ = DoAttempts(context.Background(), p, func(ctx context.Context) error {
		d, ok := ctx.Deadline()
		if ok && time.Until(d) <= 30*time.Millisecond {
			deadlineSeen = true
		}
		return nil
	})
	if !deadlineSeen {
		t.Fatal("the per-attempt timeout was not applied to the attempt context")
	}
}

func TestDoPerAttemptTimeoutDoesNotOutliveTheAttempt(t *testing.T) {
	t.Parallel()

	p := DefaultPolicy()
	p.MaxAttempts = 3
	p.Timeout = time.Hour
	p.Backoff = zeroBackoff()

	var contexts []context.Context
	_, _ = DoAttempts(context.Background(), p, func(ctx context.Context) error {
		contexts = append(contexts, ctx)
		return retryableErr()
	})

	// Every attempt context but the last must already be cancelled: Do cancels each one as soon
	// as the attempt returns, so a leaked timer cannot survive the loop.
	for i, ctx := range contexts[:len(contexts)-1] {
		if ctx.Err() == nil {
			t.Errorf("attempt %d context was not cancelled after the attempt returned", i)
		}
	}
}

// --- budget -------------------------------------------------------------------------------

func TestBudgetDocumentedParameters(t *testing.T) {
	t.Parallel()

	b := DefaultRetryBudget(NewManualClock(time.Time{}))
	// ratio 0.10, floor 3/s, window 10s → capacity 30 tokens.
	if got := b.Capacity(); got != 30 {
		t.Fatalf("capacity = %v, want 30 (3/s × 10s, docs/failure-handling.md §2.2)", got)
	}
	if got := b.Tokens(); got != 30 {
		t.Fatalf("initial tokens = %v, want a full bucket", got)
	}
}

func TestBudgetExhaustionAndRefill(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := DefaultRetryBudget(clk)

	// Drain the initial 30 tokens.
	for i := 0; i < 30; i++ {
		if !b.Allow() {
			t.Fatalf("withdrawal %d was refused with a full bucket", i)
		}
	}
	if b.Allow() {
		t.Fatal("the 31st withdrawal was permitted: the bucket has no bound")
	}
	if b.Exhausted() != 1 {
		t.Errorf("exhausted count = %d, want 1", b.Exhausted())
	}

	// The floor refills at 3/s regardless of traffic.
	clk.Advance(time.Second)
	for i := 0; i < 3; i++ {
		if !b.Allow() {
			t.Fatalf("withdrawal %d after 1s was refused: the 3/s floor did not accrue", i)
		}
	}
	if b.Allow() {
		t.Fatal("a fourth withdrawal after 1s was permitted: the floor accrued more than 3/s")
	}

	// Deposits add ratio tokens each: 10 originals fund exactly 1 retry.
	for i := 0; i < 10; i++ {
		b.Deposit()
	}
	if !b.Allow() {
		t.Fatal("10 deposits at ratio 0.10 did not fund one retry")
	}
	if b.Allow() {
		t.Fatal("10 deposits at ratio 0.10 funded more than one retry")
	}
}

func TestBudgetCapsBankedAllowance(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := DefaultRetryBudget(clk)
	// An hour of idleness must not bank an hour of retries.
	clk.Advance(time.Hour)
	if got := b.Tokens(); got != b.Capacity() {
		t.Fatalf("tokens after an idle hour = %v, want the capacity %v", got, b.Capacity())
	}
}

// TestBudgetBoundsAggregateRetryLoad is the property the budget exists for, stated as
// arithmetic: whatever the failure rate, retries add at most ratio × traffic + floor.
//
// The comparison case is a plain retry count, which multiplies the load by the attempt count
// regardless of how much traffic there is — the 4× amplification described on Budget.
func TestBudgetBoundsAggregateRetryLoad(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	// A long window so the 100 retries this funds fit inside the capacity; the clock never
	// advances, so the floor contributes nothing and the assertion is purely about the ratio.
	b := NewBudget(DefaultRetryRatio, DefaultRetryFloorPerSecond, 100*time.Second, clk)
	// Start from an empty bucket so the initial burst does not confuse the ratio arithmetic.
	for b.Allow() {
	}

	const originals = 1000
	for i := 0; i < originals; i++ {
		b.Deposit()
	}

	permitted := 0
	for b.Allow() {
		permitted++
	}
	want := int(originals * DefaultRetryRatio) // 100
	if permitted != want {
		t.Fatalf("1000 original requests funded %d retries, want %d (ratio %.2f)", permitted, want, DefaultRetryRatio)
	}
	// A per-request count of 3 would have permitted 3000 here — 4× the offered load — which is
	// the amplification the budget replaces with a bounded 10%.
	if permitted >= originals {
		t.Fatal("the budget permitted at least as many retries as originals")
	}
}

func TestBudgetFloorLetsLowTrafficPathsRetry(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	b := DefaultRetryBudget(clk)
	for b.Allow() {
	}
	// A route serving 2 rps earns 0.2 tokens/s from the ratio alone and would never retry.
	for i := 0; i < 2; i++ {
		b.Deposit()
	}
	clk.Advance(time.Second)
	if !b.Allow() {
		t.Fatal("a 2 rps route could not retry: the 3/s floor is not doing its job")
	}
}

// TestDoBudgetExhaustionBlocksRetries wires the budget into the retry loop and asserts both
// halves: exhaustion stops the retry, and the *underlying* error surfaces with its retryable
// bit intact, because we stop amplifying without lying to the client.
func TestDoBudgetExhaustionBlocksRetries(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	budget := DefaultRetryBudget(clk)
	for budget.Allow() { // drain
	}

	p := DefaultPolicy()
	p.Clock = clk
	p.MaxAttempts = 3
	p.Backoff = zeroBackoff()
	p.Budget = budget

	calls := 0
	attempts, err := DoAttempts(context.Background(), p, func(context.Context) error {
		calls++
		return retryableErr()
	})

	if calls != 1 || attempts != 1 {
		t.Fatalf("calls=%d attempts=%d, want 1 and 1: the exhausted budget did not block the retry", calls, attempts)
	}
	if !apierror.IsRetryable(err) {
		t.Error("the surfaced error is not retryable: the client was told not to retry on their own")
	}
	if got := apierror.CodeOf(err); got != apierror.CodeGatewayUnavailable {
		t.Errorf("code = %s, want the underlying %s", got, apierror.CodeGatewayUnavailable)
	}
	if budget.Exhausted() == 0 {
		t.Error("pp_retry_budget_exhausted_total would not have been incremented")
	}
}

func TestDoBudgetRefillsAndPermitsRetriesAgain(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	budget := DefaultRetryBudget(clk)
	for budget.Allow() {
	}

	p := DefaultPolicy()
	p.Clock = clk
	p.MaxAttempts = 3
	p.Backoff = zeroBackoff()
	p.Budget = budget

	clk.Advance(time.Second) // the 3/s floor accrues 3 tokens

	calls := 0
	_, _ = DoAttempts(context.Background(), p, func(context.Context) error {
		calls++
		return retryableErr()
	})
	if calls != 3 {
		t.Fatalf("fn called %d times after the budget refilled, want 3", calls)
	}
}

func TestDoDepositsOncePerLogicalOperation(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	budget := DefaultRetryBudget(clk)

	p := DefaultPolicy()
	p.Clock = clk
	p.MaxAttempts = 3
	p.Backoff = zeroBackoff()
	p.Budget = budget

	_, _ = DoAttempts(context.Background(), p, func(context.Context) error { return retryableErr() })

	if got := budget.Deposits(); got != 1 {
		t.Fatalf("deposits = %d for one Do with 3 attempts, want 1: retries must not fund themselves", got)
	}
	if got := budget.Withdrawals(); got != 2 {
		t.Fatalf("withdrawals = %d, want 2 (one per retry)", got)
	}
}

func TestBudgetConcurrentUseIsRaceFree(t *testing.T) {
	t.Parallel()

	b := DefaultRetryBudget(SystemClock())
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				b.Deposit()
				b.Allow()
				_ = b.Tokens()
			}
		}()
	}
	wg.Wait()
}

func TestDoConcurrentUseIsRaceFree(t *testing.T) {
	t.Parallel()

	budget := DefaultRetryBudget(SystemClock())
	p := DefaultPolicy()
	p.MaxAttempts = 3
	p.Backoff = zeroBackoff()
	p.Budget = budget

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = Do(context.Background(), p, func(context.Context) error {
					if (i+j)%3 == 0 {
						return nil
					}
					return retryableErr()
				})
			}
		}(i)
	}
	wg.Wait()
}

func TestGatewayPolicyMatchesTheCascade(t *testing.T) {
	t.Parallel()

	p := GatewayPolicy(nil)
	if p.Timeout != TimeoutGatewayAttempt {
		t.Errorf("per-attempt timeout = %v, want the 8s hard timeout of baseline §12 stage 14", p.Timeout)
	}
	if p.MaxAttempts != DefaultMaxAttempts {
		t.Errorf("max attempts = %d, want %d", p.MaxAttempts, DefaultMaxAttempts)
	}
}
