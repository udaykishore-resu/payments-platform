//go:build chaos

package chaos

import (
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/apptest"
	dpayment "github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Gateway-side faults: C-1 (timeout), C-2 (5xx storm), C-19 (latency).
//
// All three are the same event from the orchestrator's point of view — a call that did not come
// back with an answer — and the platform's behaviour must be different for each. That is the whole
// content of baseline §12.3 and it is what these scenarios exist to hold in place.

// TestSteadyStateHoldsWithNoFault is the control.
//
// A chaos suite whose scenarios all pass because the harness never gets as far as moving money is
// a suite that proves nothing. This asserts the world works before anything is broken, so a
// failure in a fault scenario is attributable to the fault.
func TestSteadyStateHoldsWithNoFault(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	h := e.Hypothesis()
	h.HoldsNow(t, "before the scenario")

	e.Primary.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(5_000, "EUR"))})

	res, err := e.Create(e.Ctx(), "control", 5_000)
	if err != nil {
		t.Fatalf("a payment with no fault injected failed: %v", err)
	}
	if got := res.Payment.State(); got != dpayment.StateCaptured {
		t.Fatalf("payment state = %s, want CAPTURED", got)
	}
	if n := len(e.Primary.Calls()); n != 1 {
		t.Fatalf("the primary gateway was called %d times for one payment, want 1", n)
	}
	h.HoldsNow(t, "after one clean payment")
}

// TestGatewayTimeoutLeavesPaymentProcessingAndNeverRetries is C-1 and FS-1.
//
// Verifies: baseline §12.3, ADR-013 ("a timeout leaves the payment PROCESSING"), critical path
// CP-02, docs/testing.md §6.3 C-1 and §7 FS-1.
//
// Steady-state hypothesis: no payment reaches FAILED on an unknown outcome; no payment has two
// successful attempts; no gateway idempotency key is shared. Sampled throughout the fault window.
//
// Fault: every authorize call returns an unknown outcome — the request was written and no answer
// arrived.
//
// The assertion that matters is the *negative* one: exactly one dispatch. A timeout is the one
// error the platform must not react to, because reacting means either failing a payment the issuer
// may have approved or dispatching a second one to a card that may already be authorized. Every
// other error class here would legitimately produce a second call.
func TestGatewayTimeoutLeavesPaymentProcessingAndNeverRetries(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	h := e.Hypothesis()
	h.HoldsNow(t, "before the fault")

	var faults Counter
	e.Primary.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(5_000, "EUR"))})
	e.Fallback.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(5_000, "EUR"))})
	e.Route(gwPrimary, Chain(e.Primary, TimeoutAlways(&faults)))

	stop := e.Watch(t, h)
	res, err := e.Create(e.Ctx(), "timeout", 5_000)
	stop()

	// §12.3: the caller is told the payment is processing. It is not an error — the response is a
	// payment in a legitimate, non-terminal state.
	if err != nil {
		t.Fatalf("a gateway timeout was reported to the caller as an error: %v.\n"+
			"An unknown outcome is not a failure; the payment is PROCESSING and the reconciler owns it.", err)
	}
	if got := res.Payment.State(); got != dpayment.StateProcessing {
		t.Fatalf("payment state = %s after a gateway timeout, want PROCESSING", got)
	}

	attempts := res.Payment.Attempts()
	if len(attempts) != 1 {
		t.Fatalf("%d attempts were created for a timed-out payment, want exactly 1. "+
			"A second attempt is a second authorization on a card the first may already have held.",
			len(attempts))
	}
	if got := attempts[0].Outcome(); got != dpayment.OutcomeTimeoutUnknown {
		t.Fatalf("attempt outcome = %s, want TIMEOUT_UNKNOWN", got)
	}

	// The dispatch count is read from the fault decorator rather than from the adapter, because
	// the decorator sits where the network would be: it counts what was *sent*, which is the
	// number that determines whether the card was charged twice.
	if faults.Calls() != 1 {
		t.Fatalf("the gateway was called %d times, want exactly 1. A retry or a failover happened "+
			"on an unknown outcome.", faults.Calls())
	}
	if n := len(e.Fallback.Calls()); n != 0 {
		t.Fatalf("the fallback gateway was called %d times after a timeout; failover on an unknown "+
			"outcome is a double charge", n)
	}

	h.HoldsNow(t, "after the timeout")
}

// TestGatewayFiveHundredStormFailsOverWithoutDuplicating is C-2 and FS-7.
//
// Verifies: baseline §12.3, §14.4, docs/testing.md §6.3 C-2. A 5xx is also an unknown outcome —
// no gateway guarantees a 500 means nothing happened — so it must not fail over either. This is
// the scenario that catches the tempting and wrong optimisation of treating a 5xx as "the gateway
// definitely did not act".
//
// Fault: the primary answers 503 for every call.
func TestGatewayFiveHundredStormDoesNotFailOverOnAnUnknownOutcome(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	h := e.Hypothesis()

	var faults Counter
	e.Primary.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(6_000, "EUR"))})
	e.Fallback.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(6_000, "EUR"))})
	e.Route(gwPrimary, Chain(e.Primary, FailAfter(&faults, 0, errGatewayUnavailable)))

	stop := e.Watch(t, h)
	res, err := e.Create(e.Ctx(), "storm", 6_000)
	stop()

	if err != nil {
		t.Fatalf("a 5xx storm was reported to the caller as an error: %v", err)
	}
	if got := res.Payment.State(); got != dpayment.StateProcessing {
		t.Fatalf("payment state = %s after a 5xx storm, want PROCESSING", got)
	}
	if n := len(e.Fallback.Calls()); n != 0 {
		t.Fatalf("the fallback gateway was called %d times after a 5xx. A 5xx on a money-moving "+
			"call is an unknown outcome, and failing over to a second gateway is how the same "+
			"payment gets authorized twice.", n)
	}
	h.HoldsNow(t, "after the 5xx storm")
}

// TestSoftDeclineFailsOverAndProducesExactlyOneSuccess is the positive counterpart to the two above,
// and it is what stops them from being satisfied by a system that simply never fails over.
//
// Verifies: baseline §9.1, §14.4, FS-7. A soft decline is a *known* outcome — the issuer answered
// — so failover is legitimate, it creates a new attempt, and the new attempt carries a different
// gateway idempotency key because the key is a pure function of the attempt id.
func TestSoftDeclineFailsOverAndProducesExactlyOneSuccess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	h := e.Hypothesis()

	e.Primary.Script(shared.OpAuthorize, apptest.GatewayScript{Result: softDecline()})
	e.Fallback.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(7_000, "EUR"))})

	stop := e.Watch(t, h)
	res, err := e.Create(e.Ctx(), "failover", 7_000)
	stop()

	if err != nil {
		t.Fatalf("a soft decline followed by an approval failed: %v", err)
	}
	if got := res.Payment.State(); got != dpayment.StateCaptured {
		t.Fatalf("payment state = %s after failing over to an approving gateway, want CAPTURED", got)
	}

	attempts := res.Payment.Attempts()
	if len(attempts) != 2 {
		t.Fatalf("%d attempts, want 2: one declined at the primary and one successful at the "+
			"fallback", len(attempts))
	}
	if attempts[0].GatewayID() != gwPrimary || attempts[1].GatewayID() != gwFallback {
		t.Fatalf("attempts went to %s then %s, want %s then %s",
			attempts[0].GatewayID(), attempts[1].GatewayID(), gwPrimary, gwFallback)
	}
	if attempts[0].IdempotencyKey() == attempts[1].IdempotencyKey() {
		t.Fatal("both attempts carry the same gateway idempotency key. A failover is a new " +
			"authorization at a different vendor and must not be deduplicated against the first.")
	}
	h.HoldsNow(t, "after a legitimate failover")
}

// TestSlowGatewayDegradesLatencyAndTimesOutSafely is C-19.
//
// Verifies: baseline §12 stage 14 (the 8 s hard timeout), docs/testing.md §6.3 C-19. Latency is
// injected, not failure. Two outcomes are acceptable and the test says which it got: the call
// completes late, or the caller's deadline fires — and if it fires, the outcome is unknown, never
// a failure.
//
// The scenario is table-driven over the injected latency because the interesting boundary is the
// deadline, and a single latency value would only ever exercise one side of it.
func TestSlowGatewayDegradesLatencyAndTimesOutSafely(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		latency time.Duration
		budget  time.Duration
		// wantProcessing records whether the caller's deadline is expected to fire.
		wantProcessing bool
	}{
		{"slow but inside the budget", 20 * time.Millisecond, time.Second, false},
		{"slower than the budget", 300 * time.Millisecond, 40 * time.Millisecond, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			h := e.Hypothesis()

			var faults Counter
			e.Primary.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(8_000, "EUR"))})
			e.Route(gwPrimary, Chain(e.Primary, SlowBy(&faults, tc.latency)))

			ctx, cancel := contextWithBudget(e, tc.budget)
			defer cancel()

			stop := e.Watch(t, h)
			res, err := e.Create(ctx, "slow-"+tc.name, 8_000)
			stop()

			if err != nil {
				t.Fatalf("a slow gateway produced an error rather than a degraded response: %v", err)
			}
			state := res.Payment.State()
			switch {
			case tc.wantProcessing && state != dpayment.StateProcessing:
				t.Fatalf("payment state = %s after the deadline expired mid-call, want PROCESSING. "+
					"A deadline that fires after the request was written is an unknown outcome.", state)
			case !tc.wantProcessing && state != dpayment.StateCaptured:
				t.Fatalf("payment state = %s for a call that completed inside its budget, want CAPTURED", state)
			}
			if faults.Injections() == 0 {
				t.Fatal("the latency fault never fired; this scenario passed without injecting anything")
			}
			h.HoldsNow(t, "after the latency window")
		})
	}
}
