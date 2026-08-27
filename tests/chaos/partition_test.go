//go:build chaos

package chaos

import (
	"fmt"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/apptest"
	dpayment "github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Network partition between the orchestrator and the gateways: C-18 and FS-10.
//
// Verifies: baseline §1.3 A4 (CP behaviour), §12.3, docs/testing.md §6.3 C-18 and §7 FS-10.
//
// A partition is the failure that most rewards being precise about *what the platform knows*. A
// route that does not exist, a connection refused, a TLS handshake that never completes: in all of
// them the request provably never reached the vendor. That is a plain failure, not an unknown
// outcome, and the difference is worth money in both directions —
//
//   - Treating a partition as unknown parks every affected payment in reconciliation, and the
//     reconciler then asks a gateway that never heard of any of them. A three-minute partition at
//     100 TPS produces eighteen thousand exceptions nobody can drain.
//   - Treating an unknown outcome as a partition fails over and charges the card twice.
//
// The fault below is therefore *not* spi.ErrOutcomeUnknown, and that choice is the scenario.

// TestPartitionFailsClosedAndHealsWithoutSplitBrain is C-18 and FS-10.
//
// Fault: every gateway is unreachable for a window measured on the harness's injected clock, then
// the route heals. No wall-clock time passes: a partition expressed as "three minutes of an
// injected clock" is exact, whereas one expressed as a sleep is a bet about how busy the machine is.
//
// Hypothesis, held throughout: no payment reaches a successful state while nothing can reach a
// gateway; no payment has two successful attempts; no gateway idempotency key is shared. The last
// one is the split-brain assertion — two attempts believing they own the same charge would show up
// as one key on two attempts.
func TestPartitionFailsClosedAndHealsWithoutSplitBrain(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	h := e.Hypothesis()
	h.HoldsNow(t, "before the partition")

	var primaryFaults, fallbackFaults Counter
	e.Primary.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(5_500, "EUR"))})
	e.Fallback.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(5_500, "EUR"))})

	// The partition is total: both gateways are on the far side of it. A partition that spared the
	// fallback would be a failover test, which is a different and much easier scenario.
	const window = 3 * time.Minute
	e.Route(gwPrimary, Chain(e.Primary, PartitionFor(&primaryFaults, e.Clock, window)))
	e.Route(gwFallback, Chain(e.Fallback, PartitionFor(&fallbackFaults, e.Clock, window)))

	const during = 5
	var failedClosed int
	for i := 0; i < during; i++ {
		res, err := e.Create(e.Ctx(), fmt.Sprintf("partition-%d", i), 5_500)
		switch {
		case err != nil:
			// Fail closed. The client must be able to act on this, which means retryable.
			if !apierror.IsRetryable(err) {
				t.Fatalf("payment %d failed with a non-retryable error during a partition: %v.\n"+
					"The request never reached the vendor, so retrying it is provably safe and the "+
					"client is entitled to be told so.", i, err)
			}
			failedClosed++
		case res.Payment.State() == dpayment.StateCaptured || res.Payment.State() == dpayment.StateAuthorized:
			t.Fatalf("payment %d reached %s while nothing could reach a gateway. Money cannot have "+
				"moved; something reported a success it did not observe.", i, res.Payment.State())
		default:
			// A non-terminal state is acceptable: the platform has recorded an attempt it could
			// not complete. What is not acceptable is a *success*, asserted above, or a duplicate,
			// asserted by the hypothesis.
			failedClosed++
		}
	}
	if failedClosed != during {
		t.Fatalf("%d of %d payments neither failed closed nor stayed non-terminal", during-failedClosed, during)
	}
	if primaryFaults.Injections() == 0 {
		t.Fatal("the partition never fired; the scenario asserted nothing")
	}
	h.HoldsNow(t, "during the partition")

	// Heal. The route comes back on the injected clock, with no manual intervention: the assertion
	// in FS-10 is that recovery is automatic, so nothing here resets a breaker or restarts a pod.
	e.Clock.Advance(window + time.Second)

	stop := e.Watch(t, h)
	res, err := e.Create(e.Ctx(), "partition-healed", 5_500)
	stop()
	if err != nil {
		t.Fatalf("a payment failed after the partition healed: %v.\n"+
			"Recovery must need no human: a partition that leaves the path broken after the network "+
			"returns is an outage that outlives its cause.", err)
	}
	if got := res.Payment.State(); got != dpayment.StateCaptured {
		t.Fatalf("payment state = %s after the partition healed, want CAPTURED", got)
	}

	// Nothing that was attempted during the partition acquired a success afterwards, and nothing
	// was charged twice. The hypothesis covers the second; this covers the first, because a
	// payment that failed closed and then quietly succeeded would mean the platform acted on a
	// request its caller has already been told failed.
	successes := 0
	for _, p := range e.Store.AllPayments() {
		if p.State() == dpayment.StateCaptured {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("%d payments are CAPTURED, want 1: only the one submitted after the heal", successes)
	}
	h.HoldsNow(t, "after the partition healed")
}

// TestAPartitionIsNotReportedAsAnUnknownOutcome is the classification assertion, and it is the one
// that keeps the reconciliation queue drainable.
//
// Verifies: baseline §12.3. A request that provably never left the process must not create a
// reconciliation exception. The distinguishing observable is the attempt's outcome: ERROR, which
// is terminal for that attempt and permits a fresh one, versus TIMEOUT_UNKNOWN, which is not
// terminal and parks the payment.
func TestAPartitionIsNotReportedAsAnUnknownOutcome(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	h := e.Hypothesis()

	var faults Counter
	e.Primary.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(5_500, "EUR"))})
	e.Fallback.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(5_500, "EUR"))})
	e.Route(gwPrimary, Chain(e.Primary, PartitionFor(&faults, e.Clock, time.Hour)))

	res, err := e.Create(e.Ctx(), "partition-classify", 5_500)
	if err != nil && !apierror.IsRetryable(err) {
		t.Fatalf("a partitioned primary produced a non-retryable error: %v", err)
	}

	if res == nil || res.Payment == nil {
		// The request failed closed before a payment existed, which is also a correct answer to a
		// partition and leaves nothing to classify.
		h.HoldsNow(t, "after a partition that failed closed before creating a payment")
		return
	}
	for _, a := range res.Payment.Attempts() {
		if a.GatewayID() != gwPrimary {
			continue
		}
		if a.Outcome() == dpayment.OutcomeTimeoutUnknown {
			t.Fatalf("the attempt against the partitioned gateway is TIMEOUT_UNKNOWN.\n" +
				"The request never left the process, so the outcome is known to be 'nothing " +
				"happened'. Classifying it as unknown parks the payment and sends the reconciler to " +
				"ask a gateway that never heard of it — at 100 TPS a three-minute partition then " +
				"produces eighteen thousand exceptions nobody can drain.")
		}
	}
	h.HoldsNow(t, "after the partition")
}
