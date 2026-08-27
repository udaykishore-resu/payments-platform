package resilience

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Retry budget parameters from docs/failure-handling.md §2.2.
const (
	// DefaultRetryRatio is 0.10: retries may add at most 10 % to the base request rate. A
	// healthy system retries far below this. A system sitting at the cap is already degraded,
	// and more retries would only deepen the hole.
	DefaultRetryRatio = 0.10

	// DefaultRetryFloorPerSecond is 3/s. Without a floor a route serving 2 rps would earn
	// 0.2 tokens/s and could never retry at all — the ratio alone silently disables retries on
	// exactly the low-traffic paths (control plane, vendor callbacks) where a single transient
	// failure is most visible.
	DefaultRetryFloorPerSecond = 3.0

	// DefaultRetryBudgetWindow is 10 s: long enough to smooth an ordinary burst, short enough
	// that a storm cannot bank tokens for minutes and then spend them all at once.
	DefaultRetryBudgetWindow = 10 * time.Second

	// DefaultMaxAttempts is 3 — one original call plus at most two retries, the "retry ≤ 2"
	// of failure catalog F-2. The arithmetic that fixes it at 2 rather than 3 is in the timeout
	// table: 8 s + jitter + 8 s + jitter + 8 s ≈ 26 s worst case against an 18 s orchestrator
	// deadline. MaxAttempts is the count bound; the deadline check in Do is what actually stops
	// the third call when the budget has already been spent.
	DefaultMaxAttempts = 3
)

// Policy configures Do. The zero value is not useful; use DefaultPolicy or GatewayPolicy and
// override.
//
// A Policy is read-only once passed to Do and may be shared by any number of goroutines. The
// Budget it points at is explicitly *meant* to be shared: a per-goroutine budget bounds
// nothing.
type Policy struct {
	// MaxAttempts is the total number of calls, not the number of retries. 1 means "no retry".
	MaxAttempts int

	// Backoff computes the wait between attempts. Defaults to DefaultBackoff (100 ms base,
	// 2 s cap, full jitter).
	Backoff Backoff

	// RetryableFunc decides whether an error may be retried at all. Defaults to
	// apierror.IsRetryable.
	//
	// The default treats an *unclassified* error — anything that is not an *apierror.Error — as
	// non-retryable, and that default is deliberate. An unclassified error is one nobody has
	// reasoned about. Retrying it is a guess about whether the failed operation had a side
	// effect, and on this platform the operations that produce unclassified errors are the ones
	// that talk to money-moving systems. The retry-safety table
	// (docs/failure-handling.md §3) exists because "retry when a side effect is either
	// impossible or provably deduplicated" is a per-operation judgement, and the absence of a
	// judgement must read as "no", never as "probably fine". Getting this backwards is how a
	// gateway timeout — outcome unknown — becomes a double charge.
	RetryableFunc func(error) bool

	// Timeout bounds a single attempt. Zero means "inherit the caller's deadline only".
	// Set it to the per-attempt budget from the cascade (8 s for a gateway call), never to the
	// whole-operation budget: a per-attempt timeout equal to the parent deadline leaves the
	// second attempt with nothing.
	Timeout time.Duration

	// Budget, when non-nil, gates every retry (never the first attempt) against the shared
	// retry budget. Both the count and the budget must permit a retry; they compose, they do
	// not override each other.
	Budget *Budget

	// Clock supplies Now and Sleep. Defaults to SystemClock.
	//
	// It must agree with whatever set the context's deadline: Do compares ctx.Deadline()
	// against Clock.Now() to decide whether a backoff fits, and a ManualClock paired with a
	// real context deadline compares two unrelated timelines.
	Clock Clock
}

// DefaultPolicy is the general-purpose in-request policy: 3 attempts, 100 ms/2 s full-jitter
// backoff, apierror.IsRetryable, no per-attempt timeout, no budget.
//
// It carries no budget because a budget must be shared across a client to mean anything, and a
// function that manufactures a fresh one per call would look like protection while providing
// none. Callers wire in a long-lived Budget explicitly.
func DefaultPolicy() Policy {
	return Policy{
		MaxAttempts:   DefaultMaxAttempts,
		Backoff:       DefaultBackoff(),
		RetryableFunc: apierror.IsRetryable,
		Clock:         SystemClock(),
	}
}

// GatewayPolicy is DefaultPolicy with the 8 s per-attempt hard timeout from baseline §12
// stage 14 and the shared budget wired in. One of these per (gateway, operation).
func GatewayPolicy(budget *Budget) Policy {
	p := DefaultPolicy()
	p.Timeout = TimeoutGatewayAttempt
	p.Budget = budget
	return p
}

func (p Policy) normalized() Policy {
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	if p.Backoff == nil {
		p.Backoff = DefaultBackoff()
	}
	if p.RetryableFunc == nil {
		p.RetryableFunc = apierror.IsRetryable
	}
	p.Clock = orSystem(p.Clock)
	return p
}

// Do runs fn, retrying while the policy, the context and the budget all permit it.
//
// Semantics, in the order they are checked, because the order is the contract:
//
//   - The context is checked before every attempt. A cancelled or expired context stops the
//     loop immediately; no attempt is started that the caller has already given up on.
//   - A non-retryable error is returned at once. There is no "one more try just in case".
//   - The final attempt's error is returned without a backoff. Sleeping after the last attempt
//     is pure added latency.
//   - The budget is consulted before every retry. On exhaustion the underlying error surfaces
//     immediately, with its Retryable bit intact: we stop amplifying, but we do not lie to the
//     client about whether *they* may retry after their own backoff.
//   - A backoff that would not fit inside the remaining deadline is not slept. Waking up past
//     the deadline to discover the context is dead wastes the entire remaining budget and,
//     on a gateway path, burns a bulkhead slot for nothing.
//
// The returned error is the last error, wrapped as an *apierror.Error carrying a detail with
// the attempt count and the reason the loop stopped, so a log line answers "why did this give
// up" without a stack trace. errors.Is and errors.As still reach the original.
func Do(ctx context.Context, p Policy, fn func(context.Context) error) error {
	_, err := DoAttempts(ctx, p, fn)
	return err
}

// DoAttempts is Do, additionally returning how many times fn was actually called.
//
// The count is a first-class result rather than something recovered from the error, because it
// is what the caller emits as pp_retry_attempts and what the retry-storm detector in
// docs/failure-handling.md §5.1 compares against the count of unique idempotency keys — a
// request rate that rises while unique keys stay flat is the definitive signature of retries
// rather than new work.
func DoAttempts(ctx context.Context, p Policy, fn func(context.Context) error) (int, error) {
	p = p.normalized()

	// The original request deposits into the budget exactly once, whatever its outcome. Only
	// retries withdraw. This is the accounting that makes the budget a *ratio* of traffic
	// rather than a fixed allowance.
	if p.Budget != nil {
		p.Budget.Deposit()
	}

	var last error
	for attempt := 0; attempt < p.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if last == nil {
				return attempt, contextError(err)
			}
			return attempt, gaveUp(last, attempt, reasonContext)
		}

		callCtx, cancel := p.attemptContext(ctx)
		err := fn(callCtx)
		cancel()

		if err == nil {
			return attempt + 1, nil
		}
		last = err

		if !p.RetryableFunc(err) {
			return attempt + 1, gaveUp(err, attempt+1, reasonNonRetryable)
		}
		if attempt == p.MaxAttempts-1 {
			break
		}
		if p.Budget != nil && !p.Budget.Allow() {
			return attempt + 1, gaveUp(err, attempt+1, reasonBudget)
		}

		delay := p.Backoff.Delay(attempt)
		if !fitsBeforeDeadline(ctx, p.Clock, delay) {
			return attempt + 1, gaveUp(err, attempt+1, reasonDeadline)
		}
		if err := p.Clock.Sleep(ctx, delay); err != nil {
			return attempt + 1, gaveUp(last, attempt+1, reasonContext)
		}
	}
	return p.MaxAttempts, gaveUp(last, p.MaxAttempts, reasonExhausted)
}

func (p Policy) attemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if p.Timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, p.Timeout)
}

// fitsBeforeDeadline reports whether sleeping for delay leaves any time at all to make the next
// call. The comparison is strict: a backoff that consumes the deadline exactly is a backoff
// whose only effect is to convert a usable error into a DeadlineExceeded.
func fitsBeforeDeadline(ctx context.Context, clk Clock, delay time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return delay < deadline.Sub(clk.Now())
}

const (
	reasonNonRetryable = "error is not retryable"
	reasonBudget       = "retry budget exhausted"
	reasonDeadline     = "backoff would not fit in the remaining deadline"
	reasonContext      = "context cancelled"
	reasonExhausted    = "max attempts reached"
)

func gaveUp(err error, attempts int, reason string) error {
	if err == nil {
		return nil
	}
	// From returns the value unchanged if it is already ours, and WithDetail copies, so the
	// caller's error value is never mutated by being retried.
	return apierror.From(err).WithDetail(apierror.Detail{
		Code:    "RETRY_GAVE_UP",
		Message: fmt.Sprintf("gave up after %d attempt(s): %s", attempts, reason),
		RuleID:  "RETRY.GAVE_UP",
	})
}

func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return apierror.Wrap(err, apierror.CodeGatewayTimeout, "the deadline elapsed before any attempt was started")
	}
	return apierror.Wrap(err, apierror.CodeServiceUnavailable, "the request was cancelled before any attempt was started")
}

// Budget is a shared retry budget: a token bucket whose sustained withdrawal rate is a fixed
// *fraction* of the successful request rate, with an absolute floor.
//
// Why a per-request retry count cannot prevent a retry storm, and a budget can — with the
// arithmetic that makes it inarguable:
//
// A retry count bounds one call. It says nothing about the system. Take N concurrent clients
// against a dependency in a brownout — half its capacity gone, so half the requests fail.
// With MaxAttempts = 4 (one original, three retries), each failing logical call becomes four
// wire requests. Aggregate offered load is therefore multiplied by up to 4× at the exact
// moment the dependency has *least* capacity to absorb it. Concretely at the platform's 5 000
// TPS: 2 500 failing calls × 4 = 10 000 requests, plus the 2 500 that succeeded, against a
// dependency serving 2 500. The overload is self-inflicted and it is monotone — the worse the
// dependency gets, the more of the traffic is retries, and the more of the traffic is retries
// the worse the dependency gets. The count is doing exactly what it was configured to do the
// whole way down.
//
// A budget bounds the *aggregate*. With ratio 0.10 and floor 3/s, retries can add at most
// 0.10 × 5 000 + 3 = 503 rps no matter how many calls are failing or how many clients are
// retrying. Offered load in the same brownout is 5 503 rps instead of 12 500 — a 10 % overshoot
// instead of a 150 % one — and it is bounded by a number that does not grow as the failure
// rate grows. That single property, that the retry load is a function of *traffic* rather than
// of *failures*, is the whole difference between a degraded dependency and an outage.
//
// The scope of one Budget is one (client, route_class) at ingress or one (gateway, operation)
// at egress, per docs/failure-handling.md §2.2: a platform-wide budget would let one
// misbehaving client consume everyone's allowance, which is the storm again with extra steps.
//
// Safe for concurrent use.
type Budget struct {
	mu sync.Mutex

	// The balance is kept in micro-tokens — integers, not floats — and the reason is that the
	// ratio is 0.10, which has no exact binary representation. Accumulating it as a float64
	// makes ten deposits sum to 0.9999999999999999, so a budget funded by exactly ten requests
	// refuses the one retry it was configured to allow, and the error compounds with traffic.
	// A retry budget that is off by an epsilon in the refusing direction is a retry budget that
	// silently disables retries on a low-traffic path.
	ratio    int64 // micro-tokens deposited per original request
	floor    float64
	capacity int64
	tokens   int64
	last     time.Time
	clock    Clock

	deposits  atomic.Uint64
	withdrawn atomic.Uint64
	exhausted atomic.Uint64
}

// microToken is the fixed-point scale of the budget's balance: one token is 1e6 micro-tokens,
// which resolves a ratio down to 1e-6 of a request and cannot drift.
const microToken = 1_000_000

// NewBudget returns a retry budget.
//
//   - ratio is the fraction of an original request deposited as retry allowance (0.10).
//   - floorPerSecond is the absolute allowance accrued regardless of traffic (3/s).
//   - window sizes the burst: capacity = floorPerSecond × window (3/s × 10 s = 30 tokens).
//     The window bounds how much allowance an idle path may bank, so a route that was quiet for
//     an hour cannot open with thirty minutes' worth of retries.
//
// The bucket starts full. A cold client is not the failure mode this defends against; a hot one
// is, and a hot one drains the initial 30 tokens in the first fraction of a second.
func NewBudget(ratio, floorPerSecond float64, window time.Duration, clk Clock) *Budget {
	if ratio < 0 {
		ratio = 0
	}
	if floorPerSecond < 0 {
		floorPerSecond = 0
	}
	if window <= 0 {
		window = DefaultRetryBudgetWindow
	}
	capacity := int64(floorPerSecond * window.Seconds() * microToken)
	if capacity < microToken {
		capacity = microToken
	}
	clk = orSystem(clk)
	return &Budget{
		ratio:    int64(ratio * microToken),
		floor:    floorPerSecond,
		capacity: capacity,
		tokens:   capacity,
		last:     clk.Now(),
		clock:    clk,
	}
}

// DefaultRetryBudget is the documented budget: ratio 0.10, floor 3/s, 10 s window (30 tokens
// of burst).
func DefaultRetryBudget(clk Clock) *Budget {
	return NewBudget(DefaultRetryRatio, DefaultRetryFloorPerSecond, DefaultRetryBudgetWindow, clk)
}

// Deposit records one original request, crediting `ratio` tokens. Call it once per logical
// operation, not once per attempt: crediting on every attempt would let retries fund
// themselves, which is a budget that never runs out.
func (b *Budget) Deposit() { b.DepositN(1) }

// DepositN records n original requests at once, for callers that batch.
func (b *Budget) DepositN(n int) {
	if n <= 0 {
		return
	}
	b.deposits.Add(uint64(n))
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	b.tokens = min(b.capacity, b.tokens+b.ratio*int64(n))
}

// Allow withdraws one token for a retry, reporting whether the retry may proceed.
//
// On refusal it increments the counter behind Exhausted, which is what backs
// pp_retry_budget_exhausted_total — the metric that distinguishes "the dependency is failing"
// from "we are the ones making it fail".
func (b *Budget) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	if b.tokens < microToken {
		b.exhausted.Add(1)
		return false
	}
	b.tokens -= microToken
	b.withdrawn.Add(1)
	return true
}

// Tokens returns the current balance. For metrics and tests; a decision made on this value
// rather than on Allow is a decision made outside the mutex and is therefore a race.
func (b *Budget) Tokens() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	return float64(b.tokens) / microToken
}

// Capacity returns the maximum banked allowance, in whole tokens.
func (b *Budget) Capacity() float64 { return float64(b.capacity) / microToken }

// Deposits, Withdrawals and Exhausted expose the counters for metric export.
func (b *Budget) Deposits() uint64    { return b.deposits.Load() }
func (b *Budget) Withdrawals() uint64 { return b.withdrawn.Load() }
func (b *Budget) Exhausted() uint64   { return b.exhausted.Load() }

// refillLocked accrues the time-based floor. The floor and the ratio are additive by design:
// the ratio scales with traffic, the floor guarantees a minimum, and a path with both gets
// 0.10 × rps + 3/s of retry allowance.
func (b *Budget) refillLocked() {
	now := b.clock.Now()
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		return
	}
	accrued := int64(b.floor * elapsed.Seconds() * microToken)
	if accrued <= 0 {
		// Leave `last` where it is so the elapsed time keeps accumulating. Advancing it here
		// would discard every interval too short to earn a whole micro-token, which on a path
		// polled frequently is every interval — and the floor would silently never accrue.
		return
	}
	b.last = now
	b.tokens = min(b.capacity, b.tokens+accrued)
}
