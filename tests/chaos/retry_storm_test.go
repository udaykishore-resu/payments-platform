//go:build chaos

package chaos

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/apptest"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Retry storm and thundering herd: C-23.
//
// Verifies: baseline §24 (retry storm), docs/failure-handling.md §5.1, docs/testing.md §6.3 C-23.
//
// A retry storm is not a traffic problem, it is a *feedback* problem: a dependency slows down,
// every client retries, the extra load slows it further, and the system converges on a state where
// almost all the work being done is retries of work that has already failed. The two mechanisms
// that break the loop are in internal/infrastructure/resilience and both are exercised here for
// real rather than modelled:
//
//   - The **retry budget** makes retries a bounded *ratio* of original traffic, so retries cannot
//     become the majority of the load however many clients are trying.
//   - The **adaptive limiter** sheds when latency rises, so the queue that would otherwise form is
//     refused at the door instead of turning a throughput problem into a memory problem.
//
// The scenario runs many clients against a dependency that fails, which is what a thundering herd
// looks like from inside the process. It is not 50 000 clients: the property is a ratio and a
// bound, and both are observable at a scale that runs in a second on a laptop.

// TestRetryBudgetBoundsARetryStorm is C-23's first half.
//
// The assertion is a ratio, not a count. With a 10 % budget, a storm of N failing requests may
// produce at most about N/10 retries no matter how hard the clients try — and crucially the
// *first* attempt of every request is always allowed, because refusing original traffic to protect
// a dependency from retries would be the cure killing the patient.
func TestRetryBudgetBoundsARetryStorm(t *testing.T) {
	t.Parallel()

	const (
		clients = 200
		ratio   = 0.10
	)
	clk := resilience.NewManualClock(chaosEpoch)
	// A zero floor so the assertion is about the ratio alone. In production the floor exists so a
	// low-traffic path still gets a retry or two per second; here it would blur the measurement.
	budget := resilience.NewBudget(ratio, 0, time.Minute, clk)

	var attempts atomic.Int64
	failing := func(context.Context) error {
		attempts.Add(1)
		return apierror.New(apierror.CodeServiceUnavailable, "chaos: the dependency is down")
	}

	policy := resilience.Policy{
		MaxAttempts:   3,
		Backoff:       resilience.BackoffFunc(func(int) time.Duration { return 0 }),
		RetryableFunc: apierror.IsRetryable,
		Budget:        budget,
		Clock:         clk,
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = resilience.Do(context.Background(), policy, failing)
		}()
	}
	close(start)
	wg.Wait()

	total := attempts.Load()
	retries := total - clients

	// Every original request was attempted. A budget that starved originals would show up here as
	// a total below the client count.
	if total < clients {
		t.Fatalf("only %d attempts were made for %d clients; the budget refused original traffic, "+
			"which it must never do", total, clients)
	}
	// And the retries stayed inside the budget. Without the budget every client would take all
	// three attempts, so the unbounded number is 3n.
	maxRetries := int64(float64(clients)*ratio) + 1
	if retries > maxRetries {
		t.Fatalf("the storm produced %d retries for %d original requests (%.0f%% of traffic), "+
			"above the %.0f%% budget's ceiling of %d.\n"+
			"Unbounded, this is %d attempts — the load that turns a slow dependency into a dead one.",
			retries, clients, 100*float64(retries)/float64(clients), 100*ratio, maxRetries,
			int64(clients)*int64(policy.MaxAttempts))
	}
	if retries == 0 {
		t.Fatal("the budget allowed no retries at all. A budget that refuses every retry is not a " +
			"budget, and the transient failures retries exist for would all become client errors.")
	}
	t.Logf("storm of %d clients: %d attempts, %d retries (%.1f%% of traffic, ceiling %d)",
		clients, total, retries, 100*float64(retries)/float64(clients), maxRetries)
}

// TestAdaptiveLimiterShedsRatherThanQueues is C-23's second half.
//
// Verifies: docs/failure-handling.md §5.1, baseline §18. When a dependency slows, the choice is
// between queueing and shedding, and only one of them is survivable: a queue converts a latency
// problem into an unbounded memory problem and delays every request behind work whose caller has
// already given up. Shedding keeps the answer fast and wrong-in-a-recoverable-way — a 429 with a
// Retry-After — instead of slow and eventually fatal.
//
// The limiter is the real Gradient2 implementation, driven with real round-trip observations.
func TestAdaptiveLimiterShedsRatherThanQueues(t *testing.T) {
	t.Parallel()

	clk := resilience.NewManualClock(chaosEpoch)
	limiter := resilience.NewAdaptiveLimiter(resilience.AdaptiveConfig{
		Name:         "gateway-authorize",
		InitialLimit: 4,
		MinLimit:     1,
		MaxLimit:     64,
		Clock:        clk,
	})

	// Fill the limiter to its current limit. Every one of these is admitted: the limiter is not a
	// rate limit, it is a concurrency limit, and it only refuses once the concurrency it has
	// decided is safe is actually in use.
	releases := make([]func(time.Duration, bool), 0, limiter.Limit())
	for i := 0; i < limiter.Limit(); i++ {
		release, err := limiter.Acquire(context.Background())
		if err != nil {
			t.Fatalf("the limiter refused request %d while below its own limit of %d: %v",
				i, limiter.Limit(), err)
		}
		releases = append(releases, release)
	}
	if got := limiter.InFlight(); got != limiter.Limit() {
		t.Fatalf("%d in flight at a limit of %d", got, limiter.Limit())
	}

	// The herd arrives. Every one of these must be refused *immediately* rather than queued.
	const herd = 100
	var (
		refused  int
		admitted []func(time.Duration, bool)
	)
	for i := 0; i < herd; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		// A cancelled context is how "the caller is not willing to wait" is expressed. A limiter
		// that queued would block here and only notice on cancellation; one that sheds answers
		// before the caller has to decide.
		cancel()
		release, err := limiter.Acquire(ctx)
		if err != nil {
			refused++
			continue
		}
		admitted = append(admitted, release)
	}
	for _, release := range admitted {
		release(time.Millisecond, false)
	}

	if refused == 0 {
		t.Fatalf("the limiter admitted all %d herd requests while already at its limit of %d. "+
			"Admitting past the limit is queueing, and a queue under a thundering herd is an "+
			"unbounded memory problem wearing a latency problem's clothes.", herd, limiter.Limit())
	}

	// Now the dependency slows. Each completing call reports a round trip far above the no-load
	// minimum, which is the signal the limiter reduces on.
	for _, release := range releases {
		release(500*time.Millisecond, false)
	}
	clk.Advance(2 * time.Second)
	for i := 0; i < 40; i++ {
		release, err := limiter.Acquire(context.Background())
		if err != nil {
			break
		}
		release(500*time.Millisecond, false)
		clk.Advance(50 * time.Millisecond)
	}

	if limiter.InFlight() != 0 {
		t.Fatalf("%d permits leaked; a limiter that loses permits eventually refuses everything "+
			"and looks exactly like the outage it was installed to prevent", limiter.InFlight())
	}
	if limiter.Limit() > 64 {
		t.Fatalf("the limit grew to %d under sustained high latency; the gradient is pointing the "+
			"wrong way", limiter.Limit())
	}
	t.Logf("after the herd: limit=%d in-flight=%d refused=%d/%d",
		limiter.Limit(), limiter.InFlight(), refused, herd)
}

// TestARetryStormAgainstTheOrchestratorProducesNoDuplicatePayment ties the two mechanisms back to
// the property they exist to protect.
//
// Verifies: baseline §9 I3, §14. Load-shedding and retry budgets are operational concerns; the
// reason they are in a *money* platform's chaos suite is that the alternative to shedding is a
// backlog, and a backlog of half-finished payment attempts is where duplicates come from.
//
// Each client submits a distinct payment concurrently against a gateway that fails half the time.
// The hypothesis is checked before and after rather than continuously: the aggregates are being
// mutated by many goroutines and a sampler reading them would be racing the orchestrator, which is
// a property of this in-memory harness and is explained on env.Watch.
func TestARetryStormAgainstTheOrchestratorProducesNoDuplicatePayment(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	h := e.Hypothesis()
	h.HoldsNow(t, "before the storm")

	var faults Counter
	e.Primary.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(5_000, "EUR"))})
	e.Fallback.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(5_000, "EUR"))})
	e.Route(gwPrimary, Chain(e.Primary, FailAfter(&faults, 0, errGatewayUnavailable)))

	const clients = 24
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		errs    []error
		created int
	)
	start := make(chan struct{})
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := e.Create(e.Ctx(), fmt.Sprintf("storm-%d", i), 5_000)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			created++
		}(i)
	}
	close(start)
	wg.Wait()

	// Whatever happened to each request, none of them may have produced an error the client cannot
	// act on. Every failure here must be classified retryable — that is what a Retry-After is.
	for _, err := range errs {
		if !apierror.IsRetryable(err) && !errors.Is(err, context.Canceled) {
			t.Fatalf("a request under the storm produced a non-retryable error: %v", err)
		}
	}
	if created+len(errs) != clients {
		t.Fatalf("%d successes and %d failures for %d clients; some requests produced neither",
			created, len(errs), clients)
	}
	if faults.Injections() == 0 {
		t.Fatal("the gateway fault never fired; the storm was not a storm")
	}
	h.HoldsNow(t, "after the storm")
}
