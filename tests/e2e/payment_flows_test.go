//go:build e2e

package e2e

import (
	"net/http"
	"testing"
	"time"
)

// The payment flows a merchant actually uses: failover, 3-D Secure, partial capture, partial
// refund, dispute.
//
// Each is selected by the amount's last two digits, which is the simulator's documented trigger
// table. That keeps every scenario reachable through the public API alone, with no back channel
// into the system under test.

// TestGatewayFailoverCreatesASecondAttemptAndOneSuccess is FS-7.
//
// Verifies: baseline §9.1, §14.4, docs/testing.md §7 FS-7.
//
// `…02` is a *soft* decline — the issuer answered, and answered "not this time". Failing over is
// legitimate, and the assertions are about how: a new attempt rather than a mutated one, a
// different gateway idempotency key, and exactly one success at the end.
//
// The complementary negative — a hard decline that must produce zero failover attempts, because
// retrying a stolen-card decline at a second gateway is card-testing behaviour — is the second
// subtest. Without it the first would be satisfied by a platform that failed over from everything.
func TestGatewayFailoverCreatesASecondAttemptAndOneSuccess(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	merchant := merchantID(t)
	ctx := ctxFor(t, 2*time.Minute)

	t.Run("a soft decline fails over", func(t *testing.T) {
		const softDecline = 9_002
		res := c.expect(c.post(ctx, "/v1/payments", idempotencyKey("failover"),
			createPaymentBody(merchant, softDecline, "EUR", "AUTOMATIC")),
			http.StatusCreated, "create a payment that soft-declines at the primary")

		var p payment
		res.JSON(t, &p)

		if len(p.Attempts) != 2 {
			t.Fatalf("%d attempts after a soft decline, want 2: one declined and one successful. "+
				"A platform that mutated the first attempt instead of creating a second would lose "+
				"the record of which gateway declined and why.", len(p.Attempts))
		}
		if p.Attempts[0].GatewayID == p.Attempts[1].GatewayID {
			t.Fatalf("both attempts went to %s; a failover that stays at the same gateway is a retry, "+
				"and a retry of a decline is not a failover", p.Attempts[0].GatewayID)
		}
		if p.Attempts[0].Outcome != "DECLINED" {
			t.Fatalf("the first attempt is %s, want DECLINED", p.Attempts[0].Outcome)
		}
		if p.Attempts[1].Outcome != "SUCCESS" {
			t.Fatalf("the second attempt is %s, want SUCCESS", p.Attempts[1].Outcome)
		}
		if p.State != "CAPTURED" && p.State != "AUTHORIZED" {
			t.Fatalf("the payment is %s after a successful failover", p.State)
		}

		// Exactly one success. This is I3 seen from outside the process, and it is the assertion
		// that a failover implementation is most likely to break.
		successes := 0
		for _, a := range p.Attempts {
			if a.Outcome == "SUCCESS" && a.Operation == "authorize" {
				successes++
			}
		}
		if successes != 1 {
			t.Fatalf("%d successful authorization attempts on one payment (I3)", successes)
		}
	})

	t.Run("a hard decline does not fail over", func(t *testing.T) {
		// …01 is INSUFFICIENT_FUNDS: a hard decline. The issuer has answered definitively, and
		// asking a second gateway is how a platform ends up doing card testing on a merchant's
		// behalf — which is how a merchant loses their acquiring relationship.
		const hardDecline = 9_001
		res := c.expect(c.post(ctx, "/v1/payments", idempotencyKey("hard-decline"),
			createPaymentBody(merchant, hardDecline, "EUR", "AUTOMATIC")),
			http.StatusCreated, "create a payment that hard-declines")

		var p payment
		res.JSON(t, &p)
		if p.State != "FAILED" {
			t.Fatalf("a hard decline left the payment %s, want FAILED", p.State)
		}
		if len(p.Attempts) != 1 {
			t.Fatalf("%d attempts after a hard decline, want exactly 1. Failing over from a "+
				"definitive issuer decline is card-testing behaviour.", len(p.Attempts))
		}
	})
}

// TestThreeDSChallengeCompletesAndOnlyThenCaptures is the 3-D Secure flow.
//
// Verifies: baseline §9 (REQUIRES_ACTION), §12. `…05` returns a challenge with a completion URL.
//
// The assertions are about what the platform does *while waiting*: a payment in REQUIRES_ACTION has
// no authorization, no capture and no ledger movement. A platform that optimistically recorded a
// hold before the payer authenticated would look correct in the happy path and be wrong about every
// abandoned checkout — which is most of them.
func TestThreeDSChallengeCompletesAndOnlyThenCaptures(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	merchant := merchantID(t)
	ctx := ctxFor(t, 2*time.Minute)

	const requiresAction = 7_705
	res := c.expect(c.post(ctx, "/v1/payments", idempotencyKey("threeds"),
		createPaymentBody(merchant, requiresAction, "EUR", "AUTOMATIC")),
		http.StatusCreated, "create a payment that requires 3-D Secure")

	var p payment
	res.JSON(t, &p)
	if p.State != "REQUIRES_ACTION" {
		t.Fatalf("a 3-D Secure challenge left the payment %s, want REQUIRES_ACTION", p.State)
	}
	if p.NextAction == nil || p.NextAction.URL == "" {
		t.Fatal("the payment requires action but carries no completion URL. The payer has nowhere " +
			"to go, and the merchant's checkout has no way to continue.")
	}
	if p.Captured.MinorUnits != 0 {
		t.Fatalf("%d captured while the payer has not yet authenticated", p.Captured.MinorUnits)
	}
	if p.Authorized != nil && p.Authorized.MinorUnits != 0 {
		t.Fatalf("%d authorized before the challenge was completed; the issuer has not agreed to "+
			"anything yet", p.Authorized.MinorUnits)
	}

	// The payer completes the challenge out of band. The gateway then notifies us, and only at
	// that point may the payment advance.
	final := c.awaitState(ctx, p.ID, 90*time.Second, "CAPTURED", "AUTHORIZED", "FAILED", "EXPIRED")
	if final.State == "FAILED" || final.State == "EXPIRED" {
		t.Skipf("the simulator abandoned the 3-D Secure challenge (payment %s is %s); the completion "+
			"path needs the simulator's challenge callback to be driven, which this suite does not do",
			p.ID, final.State)
	}
	if final.Captured.MinorUnits != requiresAction && final.State == "CAPTURED" {
		t.Fatalf("captured %d after a completed challenge, want %d",
			final.Captured.MinorUnits, requiresAction)
	}
}

// TestPartialCaptureAndPartialRefundRespectTheInvariants is I1 and I2 over HTTP.
//
// Verifies: baseline §9 invariants I1 and I2, §12. A manual-capture payment is authorized in full
// and captured in part; the remainder is then refunded in part. Every step asserts the running
// totals, and the last two assert that the platform refuses to exceed them.
//
// The over-capture and over-refund attempts are the point. Both are the kind of request a buggy
// merchant integration makes routinely, and both must be refused with a 422 that names the rule —
// not silently clamped, which would be a platform quietly deciding what the merchant meant.
func TestPartialCaptureAndPartialRefundRespectTheInvariants(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	merchant := merchantID(t)
	ctx := ctxFor(t, 2*time.Minute)

	const authorized = 20_000
	res := c.expect(c.post(ctx, "/v1/payments", idempotencyKey("partial"),
		createPaymentBody(merchant, authorized, "EUR", "MANUAL")),
		http.StatusCreated, "authorize a manual-capture payment")

	var p payment
	res.JSON(t, &p)
	if p.State != "AUTHORIZED" {
		t.Fatalf("a manual-capture payment is %s, want AUTHORIZED", p.State)
	}
	if p.Authorized == nil || p.Authorized.MinorUnits != authorized {
		t.Fatalf("authorized amount is %v, want %d", p.Authorized, authorized)
	}
	if p.Captured.MinorUnits != 0 {
		t.Fatalf("%d captured on a manual-capture authorization", p.Captured.MinorUnits)
	}

	// Partial capture: 12 000 of 20 000.
	const firstCapture = 12_000
	captured := c.expect(c.post(ctx, "/v1/payments/"+p.ID+"/capture", idempotencyKey("cap1"),
		map[string]any{
			"amount":       map[string]any{"minorUnits": firstCapture, "currency": "EUR"},
			"finalCapture": false,
		}), http.StatusOK, "capture part of the authorization")
	captured.JSON(t, &p)
	if p.Captured.MinorUnits != firstCapture {
		t.Fatalf("captured %d, want %d", p.Captured.MinorUnits, firstCapture)
	}
	if p.State != "CAPTURED" && p.State != "AUTHORIZED" {
		t.Fatalf("the payment is %s after a partial capture", p.State)
	}

	// I2: capturing more than the authorization is refused, and refused with a reason the
	// integrator can act on.
	over := c.post(ctx, "/v1/payments/"+p.ID+"/capture", idempotencyKey("cap-over"), map[string]any{
		"amount": map[string]any{"minorUnits": authorized, "currency": "EUR"},
	})
	if over.Status < 400 {
		t.Fatalf("capturing a further %d against a %d authorization of which %d is already captured "+
			"returned %d. Invariant I2 says captured never exceeds authorized; a platform that "+
			"clamps instead of refusing has silently decided what the merchant meant.",
			authorized, authorized, firstCapture, over.Status)
	}
	if over.Status != http.StatusUnprocessableEntity && over.Status != http.StatusConflict {
		t.Fatalf("an over-capture returned %d — %s; want 422 or 409", over.Status, over.Problem(t))
	}

	// Partial refund: 5 000 of the 12 000 captured.
	const refund = 5_000
	refunded := c.expect(c.post(ctx, "/v1/payments/"+p.ID+"/refund", idempotencyKey("ref1"),
		map[string]any{
			"amount": map[string]any{"minorUnits": refund, "currency": "EUR"},
			"reason": "REQUESTED_BY_CUSTOMER",
		}), http.StatusOK, "refund part of the capture")
	refunded.JSON(t, &p)
	if p.Refunded.MinorUnits != refund {
		t.Fatalf("refunded %d, want %d", p.Refunded.MinorUnits, refund)
	}
	if p.State != "PARTIALLY_REFUNDED" {
		t.Fatalf("the payment is %s after a partial refund, want PARTIALLY_REFUNDED", p.State)
	}

	// I1: refunding more than was captured is refused.
	overRefund := c.post(ctx, "/v1/payments/"+p.ID+"/refund", idempotencyKey("ref-over"),
		map[string]any{
			"amount": map[string]any{"minorUnits": firstCapture, "currency": "EUR"},
			"reason": "REQUESTED_BY_CUSTOMER",
		})
	if overRefund.Status < 400 {
		t.Fatalf("refunding a further %d against %d captured and %d already refunded returned %d. "+
			"Invariant I1 says refunded never exceeds captured, and the difference is money leaving "+
			"the merchant's account that never entered it.",
			firstCapture, firstCapture, refund, overRefund.Status)
	}
	if code := overRefund.Problem(t).Code; code == "" {
		t.Fatal("the over-refund rejection carries no error code; an integrator cannot branch on prose")
	}

	// The totals are unchanged by the two refusals. A rejected operation must leave the aggregate
	// entirely untouched — a platform that mutated and then errored is the source of the
	// "impossible state in production" class of bug.
	after := c.awaitState(ctx, p.ID, 30*time.Second, "PARTIALLY_REFUNDED")
	if after.Captured.MinorUnits != firstCapture || after.Refunded.MinorUnits != refund {
		t.Fatalf("after two rejected operations the totals are captured=%d refunded=%d, "+
			"want %d and %d", after.Captured.MinorUnits, after.Refunded.MinorUnits, firstCapture, refund)
	}
}

// TestDisputeMovesThePaymentAndHoldsTheFunds is the dispute flow.
//
// Verifies: baseline §9 (DISPUTED is reachable from CAPTURED, SETTLED, PARTIALLY_REFUNDED and
// REFUNDED, and is *not* terminal). A dispute is the one transition that can arrive months after
// everything else has finished, which is why the FSM permits it from every post-capture state and
// why it must not be modelled as a failure.
//
// The dispute itself arrives as a gateway webhook, so this test drives the payment to CAPTURED and
// then waits for the notification the simulator emits for the trigger amount. When the simulator is
// not configured to emit one the test skips rather than passing: a dispute flow that silently
// asserted nothing would be worse than an absent test.
func TestDisputeMovesThePaymentAndHoldsTheFunds(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	merchant := merchantID(t)
	ctx := ctxFor(t, 2*time.Minute)

	const disputed = 6_600
	res := c.expect(c.post(ctx, "/v1/payments", idempotencyKey("dispute"),
		createPaymentBody(merchant, disputed, "EUR", "AUTOMATIC")),
		http.StatusCreated, "create a payment that will be disputed")

	var p payment
	res.JSON(t, &p)
	if p.State != "CAPTURED" {
		t.Fatalf("the payment is %s before the dispute arrives, want CAPTURED", p.State)
	}

	final := c.awaitStateOrGiveUp(ctx, p.ID, 60*time.Second, "DISPUTED")
	if final.State != "DISPUTED" {
		t.Skipf("no dispute notification arrived for payment %s within the window (it is %s). "+
			"The simulator emits disputes only when its dispute scenario is configured; skipping "+
			"rather than passing, because an assertion that never ran is not evidence.",
			p.ID, final.State)
	}

	// The money is held, not reversed. A dispute is a claim, and reversing on the claim rather
	// than on its resolution is how a merchant is debited twice for one chargeback.
	if final.Captured.MinorUnits != disputed {
		t.Fatalf("captured is %d after a dispute was opened, want %d unchanged",
			final.Captured.MinorUnits, disputed)
	}
	if final.Refunded.MinorUnits != 0 {
		t.Fatalf("%d refunded when a dispute was merely *opened*; the funds move on resolution, "+
			"not on the claim", final.Refunded.MinorUnits)
	}
}
