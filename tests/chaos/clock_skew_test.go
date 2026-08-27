//go:build chaos

package chaos

import (
	"context"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/simulator"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

// Clock skew: C-20.
//
// Verifies: baseline §13.5 and §17 (webhook replay protection), docs/testing.md §6.3 C-20.
//
// A webhook signature is only a replay defence because it is bound to a timestamp and the
// timestamp is checked against a tolerance. Skew a pod's clock far enough and one of two things
// happens, and only one of them is acceptable: either legitimate webhooks start being rejected —
// noisy, recoverable, alertable — or the tolerance silently widens and a captured request from
// last week becomes replayable. This scenario is what keeps the failure on the first side.
//
// It runs against the *real* verifier in internal/adapters/gateway/simulator, which holds itself to
// the same webhook discipline the vendor adapters do: constant-time compare, tolerance before
// parse, rotation set tried in full.

// TestClockSkewBeyondTheWebhookToleranceFailsClosed is C-20.
//
// The table walks the receiver's clock across the tolerance boundary in both directions. Both
// directions matter and they are different bugs: a receiver running *behind* rejects fresh
// webhooks, a receiver running *ahead* would — if the check were one-sided — accept arbitrarily old
// ones, which is the replay the signature exists to stop.
func TestClockSkewBeyondTheWebhookToleranceFailsClosed(t *testing.T) {
	t.Parallel()

	const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	engine := simulator.NewEngine(simulator.EngineOptions{
		Clock:         shared.FixedClock{T: chaosEpoch},
		WebhookSecret: secret,
	})
	// The body carries the fields the verifier requires — an event id above all, because the id is
	// what the platform deduplicates on and a notification without one cannot be made idempotent.
	body := []byte(`{"id":"sim_evt_1","type":"payment.captured","reference":"sim_1","status":"captured","created":1787836800}`)

	// The tolerance the verifier enforces, transcribed from baseline §17 rather than read from the
	// implementation. Transcribing it is the point: if the implementation ever widens its window,
	// the "outside the tolerance" rows below start being accepted and this test fails — which is
	// the only change here that actually matters. A test that asked the implementation what its
	// tolerance was could not notice that.
	const tolerance = 5 * time.Minute

	cases := []struct {
		name string
		// skew is applied to the *receiver's* clock, which is what a skewed pod actually has.
		skew       time.Duration
		wantAccept bool
	}{
		{"no skew", 0, true},
		{"receiver a minute behind, well inside the tolerance", -time.Minute, true},
		{"receiver a minute ahead, well inside the tolerance", time.Minute, true},
		{"receiver four minutes behind, still inside", -4 * time.Minute, true},
		{"receiver six minutes behind, outside the tolerance", -(tolerance + time.Minute), false},
		{"receiver six minutes ahead, outside the tolerance", tolerance + time.Minute, false},
		{"a replay of a week-old capture", -7 * 24 * time.Hour, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The webhook is signed at the true instant by the gateway; only the receiver's clock
			// moves. Signing at the skewed time instead would model a skewed *gateway*, which is
			// somebody else's incident.
			signedAt := chaosEpoch
			headers := map[string]string{
				simulator.SignatureHeader: simulator.Sign(body, secret, signedAt),
			}
			receivedAt := chaosEpoch.Add(tc.skew)

			ev, err := engine.Verify(context.Background(), body, headers, []string{secret}, receivedAt)

			if tc.wantAccept {
				if err != nil {
					t.Fatalf("a webhook %s of the signing time was rejected: %v.\n"+
						"Rejecting legitimate notifications inside the tolerance drops real state "+
						"transitions, and the gateway will eventually stop retrying.",
						tc.skew, err)
				}
				if ev == nil {
					t.Fatal("the verifier accepted the webhook and returned no event")
				}
				return
			}

			if err == nil {
				t.Fatalf("a webhook signed %s from the receiver's now was accepted. The timestamp "+
					"tolerance is the whole replay defence: without it a captured request stays "+
					"valid forever.", tc.skew)
			}
			if ev != nil {
				t.Fatal("the verifier returned an event alongside a rejection; a caller that " +
					"checked the event rather than the error would act on a replay")
			}
		})
	}
}

// TestATamperedBodyIsRejectedRegardlessOfTheClock is the companion assertion.
//
// Verifies: baseline §17. It exists to stop the test above from being satisfied by an
// implementation that only ever checks the timestamp: a verifier that accepted any body whose
// timestamp was fresh would pass every row of that table and authenticate nothing.
func TestATamperedBodyIsRejectedRegardlessOfTheClock(t *testing.T) {
	t.Parallel()

	const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	engine := simulator.NewEngine(simulator.EngineOptions{
		Clock:         shared.FixedClock{T: chaosEpoch},
		WebhookSecret: secret,
	})

	original := []byte(`{"id":"sim_evt_2","type":"payment.captured","reference":"sim_2","status":"captured","amount":{"minorUnits":1000,"currency":"EUR"}}`)
	tampered := []byte(`{"id":"sim_evt_2","type":"payment.captured","reference":"sim_2","status":"captured","amount":{"minorUnits":9999,"currency":"EUR"}}`)
	headers := map[string]string{
		simulator.SignatureHeader: simulator.Sign(original, secret, chaosEpoch),
	}

	for _, skew := range []time.Duration{0, time.Minute, -time.Minute} {
		if _, err := engine.Verify(context.Background(), tampered, headers, []string{secret},
			chaosEpoch.Add(skew)); err == nil {
			t.Fatalf("a body whose amount was changed from 1000 to 9999 was accepted at a skew of "+
				"%s. The signature covers the body; if it did not, an attacker inside the tolerance "+
				"window could rewrite any amount.", skew)
		}
	}

	// And the untampered body still verifies, so the rejection above is attributable to the
	// tampering rather than to a broken fixture.
	if _, err := engine.Verify(context.Background(), original, headers, []string{secret}, chaosEpoch); err != nil {
		t.Fatalf("the untampered body was rejected: %v", err)
	}
}

// TestASecretRotationDoesNotDropWebhooks is the third clock-adjacent failure.
//
// Verifies: baseline §17. A rotation has an overlap window during which both secrets are live.
// A verifier that tried only the newest would drop every in-flight notification signed with the
// previous one — a silent, self-inflicted outage on the most exposed surface in the platform,
// visible only as merchants reporting that captures stopped arriving.
func TestASecretRotationDoesNotDropWebhooks(t *testing.T) {
	t.Parallel()

	const previous = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const current = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

	engine := simulator.NewEngine(simulator.EngineOptions{
		Clock:         shared.FixedClock{T: chaosEpoch},
		WebhookSecret: current,
	})
	body := []byte(`{"id":"sim_evt_3","type":"payment.captured","reference":"sim_rotate","status":"captured"}`)

	signedWithPrevious := map[string]string{
		simulator.SignatureHeader: simulator.Sign(body, previous, chaosEpoch),
	}

	if _, err := engine.Verify(context.Background(), body, signedWithPrevious,
		[]string{current, previous}, chaosEpoch); err != nil {
		t.Fatalf("a webhook signed with the previous secret was rejected during the rotation "+
			"overlap: %v", err)
	}
	// Once the previous secret is retired it must stop working, or a rotation protects nothing.
	if _, err := engine.Verify(context.Background(), body, signedWithPrevious,
		[]string{current}, chaosEpoch); err == nil {
		t.Fatal("a webhook signed with a retired secret was still accepted; rotating the secret " +
			"had no effect")
	}
}
