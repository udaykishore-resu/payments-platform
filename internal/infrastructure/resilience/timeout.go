package resilience

import (
	"context"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// The timeout cascade of docs/failure-handling.md §2.1. Every value is derived from the one
// above it; the derivation is on the constant so that changing one without changing its
// neighbours is visibly wrong.
const (
	// TimeoutClientALBIdle is 65 s, deliberately above the ALB's own 60 s idle timeout so the
	// ALB closes first. If the client closed first, the ALB would hold a half-open connection
	// and the request would complete into nothing.
	TimeoutClientALBIdle = 65 * time.Second

	// TimeoutALBToAPI is 30 s: a generous ceiling nothing should approach.
	TimeoutALBToAPI = 30 * time.Second

	// TimeoutAPIRequest is 25 s — the budget everything inside must fit within.
	TimeoutAPIRequest = 25 * time.Second

	// TimeoutAPIToOrchestrator is 20 s: 25 s minus 5 s of ingress-side headroom for
	// authentication, validation, idempotency claim and response rendering.
	TimeoutAPIToOrchestrator = 20 * time.Second

	// TimeoutOrchestratorInternal is 18 s: 20 s minus 2 s for response serialization and the
	// outbox write, which happen after the gateway call returns and must not be squeezed.
	TimeoutOrchestratorInternal = 18 * time.Second

	// TimeoutGatewayAttempt is the 8 s hard timeout of baseline §12 stage 14. It covers the
	// p99.9 of every gateway's authorization latency; beyond it the marginal probability of
	// success is lower than the cost of holding a bulkhead slot.
	TimeoutGatewayAttempt = 8 * time.Second

	// TimeoutGatewayConnect is 2 s, separate from the overall timeout: a slow TCP connect is a
	// dead host, not a slow gateway, and waiting 8 s to discover that wastes 6 s of budget.
	TimeoutGatewayConnect = 2 * time.Second

	// TimeoutGatewayTLSHandshake is 3 s.
	TimeoutGatewayTLSHandshake = 3 * time.Second

	// TimeoutPostgresRead is 250 ms (statement_timeout). The data-plane stage budgets of
	// baseline §12 total ~60 ms, so this is 4× headroom.
	TimeoutPostgresRead = 250 * time.Millisecond

	// TimeoutPostgresWrite is 2 s: covers a checkpoint or a brief lock wait without holding a
	// connection indefinitely.
	TimeoutPostgresWrite = 2 * time.Second

	// TimeoutRedisOp is 50 ms. Redis is a latency accelerator; if it is slower than the
	// Postgres fallback it is worse than useless.
	TimeoutRedisOp = 50 * time.Millisecond

	// TimeoutKafkaProduce is 10 s with acks=all: durability over latency, because the relay is
	// asynchronous and latency there costs nothing on the payment path.
	TimeoutKafkaProduce = 10 * time.Second

	// TimeoutMerchantWebhook is 5 s connect+read. A merchant's slow endpoint must not consume
	// our delivery pool.
	TimeoutMerchantWebhook = 5 * time.Second

	// DefaultCascadeHeadroom is 2 s — what TimeoutOrchestratorInternal reserves out of
	// TimeoutAPIToOrchestrator for the work that happens after the child call returns.
	DefaultCascadeHeadroom = 2 * time.Second

	// DefaultMinUsefulTime is 500 ms: the least remaining budget in which starting a gateway
	// call is worth doing. See Cascade for why refusing early beats trying.
	DefaultMinUsefulTime = 500 * time.Millisecond
)

// Cascade derives a child deadline from a parent deadline, reserving headroom, and refuses to
// start work that cannot finish.
//
// **Why a child timeout must be strictly less than the parent's remaining budget.** The rule
// from docs/failure-handling.md §2.1 is that a caller's timeout must exceed the sum of its
// callee's timeout plus its retries plus overhead. Violate it and the outer caller gives up
// while the inner work continues — and every consequence of that is bad in a specific way:
//
//   - **Orphaned work.** The gateway call is still in flight, holding a bulkhead slot, a
//     socket and a goroutine, doing work for a request whose response nobody will read. Under
//     load these accumulate: the outer layer times out and retries, the inner layer never stops,
//     and the pod's concurrency is consumed by work that has already been abandoned.
//   - **Wasted capacity at the worst moment.** Every orphan is a slot not available to a
//     request that could still succeed, and orphans are produced fastest exactly when the
//     system is slowest.
//   - **Genuine ambiguity about money.** This is the one that matters. If the parent gives up
//     at 18 s while the gateway call it started at 12 s is still running, we do not know
//     whether the authorization happened. The payment must go to TIMEOUT_UNKNOWN and stay
//     PROCESSING until a webhook, a status lookup or a settlement report resolves it
//     (baseline A7, §12.3). The equal-timeout mistake manufactures that ambiguity out of
//     nothing but arithmetic — and each instance costs a reconciliation cycle and, past fifteen
//     minutes, an operator.
//
// The headroom is what makes "strictly less" true with a margin rather than by a nanosecond:
// the parent needs time after the child returns to serialize a response, write the outbox row
// and commit. A child deadline equal to the parent's is a child that finishes exactly when the
// parent must already have answered.
//
// The second half of the discipline is refusing to start. Beginning a gateway call with 300 ms
// of budget left burns a bulkhead slot and produces a TIMEOUT_UNKNOWN — the most expensive
// outcome in the failure catalog — for a call that had no chance. Returning GATEWAY_TIMEOUT
// pre-emptively costs nothing and, crucially, is *unambiguous*: no request was sent, so no money
// moved, so no reconciliation is required.
//
// Safe for concurrent use; a Cascade is immutable after construction.
type Cascade struct {
	headroom  time.Duration
	minUseful time.Duration
	clock     Clock
}

// NewCascade returns a Cascade reserving headroom out of the parent's remaining budget and
// refusing to start work with less than minUseful left.
//
// The clock is the system clock, and it must be: Cascade compares against deadlines carried on
// a context.Context, and those are set against wall time by whatever created them.
func NewCascade(headroom, minUseful time.Duration) *Cascade {
	if headroom < 0 {
		headroom = 0
	}
	if minUseful < 0 {
		minUseful = 0
	}
	return &Cascade{headroom: headroom, minUseful: minUseful, clock: SystemClock()}
}

// DefaultCascade is the orchestrator's cascade: 2 s of headroom, 500 ms of minimum useful time.
func DefaultCascade() *Cascade {
	return NewCascade(DefaultCascadeHeadroom, DefaultMinUsefulTime)
}

// GatewayCascade is the cascade for a gateway call: 2 s of headroom for the outbox write and
// response, and a 500 ms floor below which a call is refused rather than attempted.
func GatewayCascade() *Cascade {
	return NewCascade(DefaultCascadeHeadroom, DefaultMinUsefulTime)
}

// Remaining returns the time left on ctx's deadline and whether ctx has one at all. A context
// with no deadline reports (0, false) — not a large number, because a caller that cannot tell
// "unbounded" from "a long time" will eventually treat one as the other.
func (c *Cascade) Remaining(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	return deadline.Sub(c.clock.Now()), true
}

// Check reports whether there is enough budget left to start any work at all, returning
// GATEWAY_TIMEOUT when there is not.
//
// GATEWAY_TIMEOUT rather than a generic error because the code's registered semantics are
// exactly right here: category TIMEOUT, Retryable=false, "the outcome is unknown and is being
// reconciled". The Retryable=false is the important half — baseline A7 forbids automatically
// retrying an operation whose outcome is unknown, and although *this* particular refusal
// happened before anything was sent, a caller must not learn to treat timeouts as retryable
// from the one case where it would have been safe.
func (c *Cascade) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return contextError(err)
	}
	remaining, ok := c.Remaining(ctx)
	if !ok {
		return nil
	}
	if remaining-c.headroom < c.minUseful {
		return apierror.Newf(apierror.CodeGatewayTimeout,
			"insufficient remaining budget to start work: %s left, %s reserved as headroom, %s required",
			remaining.Round(time.Millisecond), c.headroom, c.minUseful)
	}
	return nil
}

// Budget returns the child timeout to use for a call that wants `want`, which is the smaller of
// what it asked for and what the parent can actually spare.
//
// A caller asking for the full 8 s gateway timeout with 3 s of parent budget left gets 1 s
// (3 s minus 2 s of headroom) — not 8 s, and not an error. The call is worth starting; it is
// simply not worth starting with a timeout that lies about when the caller will stop caring.
func (c *Cascade) Budget(ctx context.Context, want time.Duration) (time.Duration, error) {
	if err := c.Check(ctx); err != nil {
		return 0, err
	}
	if want <= 0 {
		want = c.minUseful
	}
	remaining, ok := c.Remaining(ctx)
	if !ok {
		// No parent deadline: the caller's own timeout is the whole budget. This is the
		// background-worker case; on the request path a context always carries a deadline,
		// because the request pipeline sets one at ingress.
		return want, nil
	}
	budget := min(want, remaining-c.headroom)
	if budget < c.minUseful {
		// Check already proved remaining-headroom >= minUseful, so this can only be reached if
		// `want` itself was below the floor — a caller asking for less time than it needs.
		return 0, apierror.Newf(apierror.CodeGatewayTimeout,
			"requested budget %s is below the %s minimum useful time", want, c.minUseful)
	}
	return budget, nil
}

// Child returns a context whose deadline is Budget(ctx, want) from now, along with its cancel
// function.
//
// On error the returned cancel is a non-nil no-op, so `ctx, cancel, err := …; defer cancel()`
// is safe to write before the error check. Returning a nil cancel would make the idiomatic
// ordering a nil-pointer panic in a request path, which is a worse bug than the one the error
// was reporting.
func (c *Cascade) Child(ctx context.Context, want time.Duration) (context.Context, context.CancelFunc, error) {
	budget, err := c.Budget(ctx, want)
	if err != nil {
		return ctx, func() {}, err
	}
	child, cancel := context.WithTimeout(ctx, budget)
	return child, cancel, nil
}

// Headroom returns the reserved headroom.
func (c *Cascade) Headroom() time.Duration { return c.headroom }

// MinUseful returns the minimum budget in which work will be started.
func (c *Cascade) MinUseful() time.Duration { return c.minUseful }
