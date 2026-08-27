package payment

import (
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// newTestAttempt returns a bare attempt in PENDING, as Payment.StartAttempt would have created it.
func newTestAttempt(t *testing.T) *Attempt {
	t.Helper()
	return newAttempt(shared.NewPaymentID(), shared.NewTenantID(), "stripe",
		shared.OpAuthorize, 1, usd(100_00), testEpoch)
}

func TestDeriveGatewayIdempotencyKey(t *testing.T) {
	// Verifies: ADR-012 — the key that lets a transport retry be deduplicated by the gateway and
	// a deliberate failover be correctly treated as a new authorization.
	t.Parallel()

	const (
		attemptA = shared.AttemptID("att_01JB8ZQK3XN7VH2MPYT4RGWC5D")
		attemptB = shared.AttemptID("att_01JB8ZQK3XN7VH2MPYT4RGWC5E")
	)

	t.Run("stable across process runs", func(t *testing.T) {
		t.Parallel()
		// A literal expected value, not a recomputation. After a crash the reconciler recomputes
		// this key from the persisted attempt ID and asks the gateway "what happened to this" —
		// so a change to the derivation orphans every in-flight transaction the platform has, and
		// this assertion is what makes such a change impossible to land by accident.
		tests := []struct {
			attempt shared.AttemptID
			op      shared.Operation
			want    string
		}{
			{attemptA, shared.OpAuthorize, "3p6sfugde6hyrnfjzyosvxc5maxf6xan"},
			{attemptA, shared.OpCapture, "kwogm5b2mjzfmg57idd5fmseo33cliap"},
			{attemptB, shared.OpAuthorize, "7uqtsjpduqmn6x4hrtw366kc7fgsefmr"},
		}
		for _, tc := range tests {
			got := DeriveGatewayIdempotencyKey(tc.attempt, tc.op, defaultKeySalt)
			if got != tc.want {
				t.Errorf("DeriveGatewayIdempotencyKey(%s, %s) = %q, want %q",
					tc.attempt, tc.op, got, tc.want)
			}
		}
	})

	t.Run("deterministic for the same attempt and operation", func(t *testing.T) {
		t.Parallel()
		first := DeriveGatewayIdempotencyKey(attemptA, shared.OpAuthorize, defaultKeySalt)
		for i := 0; i < 100; i++ {
			if got := DeriveGatewayIdempotencyKey(attemptA, shared.OpAuthorize, defaultKeySalt); got != first {
				t.Fatalf("call %d returned %q, want %q", i, got, first)
			}
		}
	})

	t.Run("distinct per attempt, operation and salt", func(t *testing.T) {
		t.Parallel()
		base := DeriveGatewayIdempotencyKey(attemptA, shared.OpAuthorize, defaultKeySalt)
		// Failing over produces a different key, so the new gateway treats it as a new
		// authorization rather than deduplicating against something it never saw.
		if other := DeriveGatewayIdempotencyKey(attemptB, shared.OpAuthorize, defaultKeySalt); other == base {
			t.Fatal("two attempts derived the same key")
		}
		// A capture must not collide with the authorization it captures.
		if other := DeriveGatewayIdempotencyKey(attemptA, shared.OpCapture, defaultKeySalt); other == base {
			t.Fatal("two operations on one attempt derived the same key")
		}
		if other := DeriveGatewayIdempotencyKey(attemptA, shared.OpAuthorize, "other-salt"); other == base {
			t.Fatal("two salts derived the same key")
		}
		// The separator matters: without it, ("att_1", "xy") and ("att_1x", "y") would collide.
		a := DeriveGatewayIdempotencyKey("att_1", "xy", defaultKeySalt)
		b := DeriveGatewayIdempotencyKey("att_1x", "y", defaultKeySalt)
		if a == b {
			t.Fatal("the attempt id and the operation are concatenated without a separator")
		}
	})

	t.Run("bounded length and charset", func(t *testing.T) {
		t.Parallel()
		// Gateways vary in what they accept; 32 lowercase Base32 characters is what every
		// integrated gateway takes.
		for _, op := range []shared.Operation{
			shared.OpAuthorize, shared.OpCapture, shared.OpRefund, shared.OpVoid, shared.OpLookup,
		} {
			key := DeriveGatewayIdempotencyKey(shared.NewAttemptID(), op, defaultKeySalt)
			if len(key) != 32 {
				t.Errorf("%s: key length = %d, want 32 (%q)", op, len(key), key)
			}
			if key != strings.ToLower(key) {
				t.Errorf("%s: key is not lowercase: %q", op, key)
			}
			for i := 0; i < len(key); i++ {
				c := key[i]
				if !((c >= 'a' && c <= 'z') || (c >= '2' && c <= '7')) {
					t.Errorf("%s: key %q contains %q, which is outside Base32", op, key, string(c))
					break
				}
			}
		}
	})

	t.Run("an attempt carries the key it was created with", func(t *testing.T) {
		t.Parallel()
		att := newTestAttempt(t)
		want := DeriveGatewayIdempotencyKey(att.ID(), att.Operation(), defaultKeySalt)
		if att.IdempotencyKey() != want {
			t.Fatalf("attempt key = %q, want %q", att.IdempotencyKey(), want)
		}
		if att.Outcome() != OutcomePending || att.Sequence() != 1 {
			t.Fatalf("outcome = %s sequence = %d", att.Outcome(), att.Sequence())
		}
		if att.CreatedAt() != testEpoch || att.UpdatedAt() != testEpoch {
			t.Fatal("the attempt was not stamped from the supplied instant")
		}
		if att.DispatchedAt() != nil || att.ResolvedAt() != nil || att.Latency() != 0 {
			t.Fatal("a fresh attempt carries dispatch or resolution timing")
		}
	})
}

func TestAttemptOutcomeTransitions(t *testing.T) {
	// Verifies: docs/state-machines.md §4.1.
	t.Parallel()

	dispatchedAt := testEpoch.Add(time.Second)
	resolvedAt := dispatchedAt.Add(250 * time.Millisecond)

	t.Run("dispatch then succeed", func(t *testing.T) {
		t.Parallel()
		att := newTestAttempt(t)
		if err := att.Dispatch(dispatchedAt); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if att.Outcome() != OutcomeDispatched || att.DispatchedAt() == nil {
			t.Fatalf("outcome = %s dispatchedAt = %v", att.Outcome(), att.DispatchedAt())
		}
		// Dispatching twice is refused rather than treated as a no-op: a second dispatch is a
		// second request on the wire, and the first one's timing record must not be overwritten.
		if apierror.CodeOf(att.Dispatch(resolvedAt)) != apierror.CodeInvalidStateTransition {
			t.Fatal("an attempt was dispatched twice")
		}
		if !att.DispatchedAt().Equal(dispatchedAt) {
			t.Fatalf("a refused second dispatch overwrote dispatchedAt: %s", att.DispatchedAt())
		}
		if err := att.Succeed("gw_txn_1", "succeeded", resolvedAt); err != nil {
			t.Fatalf("Succeed: %v", err)
		}
		if att.Outcome() != OutcomeSuccess || att.GatewayRef() != "gw_txn_1" || att.RawStatus() != "succeeded" {
			t.Fatalf("attempt = %s / %q / %q", att.Outcome(), att.GatewayRef(), att.RawStatus())
		}
		// Latency is measured from dispatch, not from creation: the queueing time before the
		// request left is our problem, not the gateway's.
		if att.Latency() != 250*time.Millisecond {
			t.Fatalf("latency = %s, want 250ms", att.Latency())
		}
		if att.ResolvedAt() == nil || !att.ResolvedAt().Equal(resolvedAt) {
			t.Fatalf("resolvedAt = %v", att.ResolvedAt())
		}
		// SUCCESS is terminal: a correction is a void or a refund on the payment, not an edit.
		if err := att.Decline(DeclineFraudulent, "", "", false, resolvedAt); err == nil {
			t.Fatal("a successful attempt was declined afterwards")
		}
		if err := att.Fail("X", "y", resolvedAt); err == nil {
			t.Fatal("a successful attempt was failed afterwards")
		}
	})

	t.Run("a pre-dispatch failure never reaches the gateway", func(t *testing.T) {
		t.Parallel()
		// Circuit open, credentials missing, no bulkhead slot: the request provably never left,
		// so the attempt resolves as ERROR without a dispatch timestamp and retrying is safe.
		att := newTestAttempt(t)
		if err := att.Fail("GATEWAY_CIRCUIT_OPEN", "circuit is open", testEpoch); err != nil {
			t.Fatalf("Fail: %v", err)
		}
		if att.Outcome() != OutcomeError || att.ErrorCode() != "GATEWAY_CIRCUIT_OPEN" {
			t.Fatalf("attempt = %s / %q", att.Outcome(), att.ErrorCode())
		}
		if att.ErrorMessage() != "circuit is open" {
			t.Fatalf("errorMessage = %q", att.ErrorMessage())
		}
		if att.DispatchedAt() != nil || att.Latency() != 0 {
			t.Fatal("an undispatched attempt recorded a dispatch or a latency")
		}
		if !att.PermitsFailover() {
			t.Fatal("an ERROR attempt does not permit failover")
		}
	})

	t.Run("declining without a reason maps to UNKNOWN", func(t *testing.T) {
		t.Parallel()
		// An adapter that cannot classify a decline must produce UNKNOWN, which does not permit
		// failover. Silence must not be read as permission.
		att := newTestAttempt(t)
		if err := att.Dispatch(dispatchedAt); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if err := att.Decline("", "gw_txn_1", "refused", false, resolvedAt); err != nil {
			t.Fatalf("Decline: %v", err)
		}
		if att.DeclineReason() != DeclineUnknown {
			t.Fatalf("reason = %s, want UNKNOWN", att.DeclineReason())
		}
		if att.PermitsFailover() {
			t.Fatal("an unclassified decline permits failover")
		}
	})

	t.Run("a timeout leaves the attempt open", func(t *testing.T) {
		t.Parallel()
		att := newTestAttempt(t)
		if err := att.Dispatch(dispatchedAt); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if err := att.TimeOut("no response within 8s", resolvedAt); err != nil {
			t.Fatalf("TimeOut: %v", err)
		}
		if att.Outcome() != OutcomeTimeoutUnknown {
			t.Fatalf("outcome = %s", att.Outcome())
		}
		if att.ErrorCode() != string(apierror.CodeGatewayTimeout) {
			t.Fatalf("errorCode = %q", att.ErrorCode())
		}
		// Deliberately not resolved: the reconciler's work queue is exactly the set of attempts
		// with no resolution.
		if att.ResolvedAt() != nil {
			t.Fatalf("a timed-out attempt was stamped resolved at %v", att.ResolvedAt())
		}
		if att.Latency() != 250*time.Millisecond {
			t.Fatalf("latency = %s", att.Latency())
		}
		if !att.Outcome().RequiresReconciliation() || att.PermitsFailover() {
			t.Fatal("a timed-out attempt does not require reconciliation, or permits failover")
		}
	})

	t.Run("an undispatched attempt cannot succeed, decline or time out", func(t *testing.T) {
		t.Parallel()
		// PENDING → SUCCESS would mean a gateway confirmed a request that was never sent.
		for _, tc := range []struct {
			name string
			run  func(*Attempt) error
		}{
			{"succeed", func(a *Attempt) error { return a.Succeed("gw", "ok", testEpoch) }},
			{"decline", func(a *Attempt) error { return a.Decline(DeclineLostCard, "gw", "no", false, testEpoch) }},
			{"timeout", func(a *Attempt) error { return a.TimeOut("no answer", testEpoch) }},
		} {
			att := newTestAttempt(t)
			err := tc.run(att)
			if apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
				t.Errorf("%s from PENDING: code = %s, want INVALID_STATE_TRANSITION", tc.name, apierror.CodeOf(err))
			}
			if att.Outcome() != OutcomePending {
				t.Errorf("%s: a refused transition moved the attempt to %s", tc.name, att.Outcome())
			}
		}
	})
}

func TestReconcileIsTheOnlyPathOutOfTimeoutUnknown(t *testing.T) {
	// Verifies: ADR-013, docs/state-machines.md §4.1 #8–#10 and §4.2.
	t.Parallel()

	dispatchedAt := testEpoch.Add(time.Second)
	resolvedAt := dispatchedAt.Add(time.Second)

	timedOut := func(t *testing.T) *Attempt {
		t.Helper()
		att := newTestAttempt(t)
		if err := att.Dispatch(dispatchedAt); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if err := att.TimeOut("no answer", dispatchedAt.Add(8*time.Second)); err != nil {
			t.Fatalf("TimeOut: %v", err)
		}
		return att
	}

	t.Run("the reconciler may resolve it in any direction the gateway reports", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name    string
			outcome AttemptOutcome
			reason  DeclineReason
			check   func(*testing.T, *Attempt)
		}{
			{
				name: "authorized after all", outcome: OutcomeSuccess,
				check: func(t *testing.T, a *Attempt) {
					if a.GatewayRef() != "gw_txn_1" {
						t.Fatalf("gatewayRef = %q", a.GatewayRef())
					}
				},
			},
			{
				name: "declined after all", outcome: OutcomeDeclined, reason: DeclineStolenCard,
				check: func(t *testing.T, a *Attempt) {
					if a.DeclineReason() != DeclineStolenCard || a.PermitsFailover() {
						t.Fatalf("reason = %s failover = %v", a.DeclineReason(), a.PermitsFailover())
					}
				},
			},
			{
				name: "never reached the gateway", outcome: OutcomeError,
				check: func(t *testing.T, a *Attempt) {
					// Proving the request never arrived is the only thing that makes a retry safe.
					if !a.PermitsFailover() {
						t.Fatal("an attempt proven never to have reached the gateway forbids failover")
					}
				},
			},
			{
				name: "declined with no reason falls back to UNKNOWN", outcome: OutcomeDeclined,
				check: func(t *testing.T, a *Attempt) {
					if a.DeclineReason() != DeclineUnknown || a.PermitsFailover() {
						t.Fatalf("reason = %s failover = %v", a.DeclineReason(), a.PermitsFailover())
					}
				},
			},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				att := timedOut(t)
				if err := att.Reconcile(tc.outcome, "gw_txn_1", "looked up", tc.reason, resolvedAt); err != nil {
					t.Fatalf("Reconcile: %v", err)
				}
				if att.Outcome() != tc.outcome {
					t.Fatalf("outcome = %s, want %s", att.Outcome(), tc.outcome)
				}
				if att.ResolvedAt() == nil {
					t.Fatal("a reconciled attempt is still unresolved")
				}
				if att.Outcome().RequiresReconciliation() {
					t.Fatal("a reconciled attempt still requires reconciliation")
				}
				tc.check(t, att)
			})
		}
	})

	t.Run("a reconciliation that reports nothing new leaves what is on file", func(t *testing.T) {
		t.Parallel()
		att := timedOut(t)
		if err := att.Reconcile(OutcomeSuccess, "", "", "", resolvedAt); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if att.GatewayRef() != "" || att.RawStatus() != "" {
			t.Fatalf("blank fields overwrote what was on file: %q / %q", att.GatewayRef(), att.RawStatus())
		}
	})

	t.Run("reconciliation is refused from every other outcome", func(t *testing.T) {
		t.Parallel()
		// Only an attempt whose outcome is genuinely unknown may be resolved by lookup. Anything
		// else already has an authoritative answer, and overwriting it would let a stale lookup
		// turn a decline into a success.
		reach := map[AttemptOutcome]func(*Attempt) error{
			OutcomePending:    func(*Attempt) error { return nil },
			OutcomeDispatched: func(a *Attempt) error { return a.Dispatch(dispatchedAt) },
			OutcomeSuccess: func(a *Attempt) error {
				if err := a.Dispatch(dispatchedAt); err != nil {
					return err
				}
				return a.Succeed("gw", "ok", resolvedAt)
			},
			OutcomeDeclined: func(a *Attempt) error {
				if err := a.Dispatch(dispatchedAt); err != nil {
					return err
				}
				return a.Decline(DeclineLostCard, "gw", "no", false, resolvedAt)
			},
			OutcomeError: func(a *Attempt) error { return a.Fail("X", "y", resolvedAt) },
		}
		for _, o := range AllAttemptOutcomes {
			if o == OutcomeTimeoutUnknown {
				continue
			}
			drive, ok := reach[o]
			if !ok {
				t.Fatalf("no way to reach %s in this test", o)
			}
			att := newTestAttempt(t)
			if err := drive(att); err != nil {
				t.Fatalf("reaching %s: %v", o, err)
			}
			err := att.Reconcile(OutcomeSuccess, "gw", "ok", "", resolvedAt)
			if apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
				t.Errorf("Reconcile from %s: code = %s, want INVALID_STATE_TRANSITION",
					o, apierror.CodeOf(err))
			}
			if att.Outcome() != o {
				t.Errorf("a refused Reconcile moved %s to %s", o, att.Outcome())
			}
		}
	})

	t.Run("reconciliation cannot park the attempt back in TIMEOUT_UNKNOWN", func(t *testing.T) {
		t.Parallel()
		att := timedOut(t)
		if err := att.Reconcile(OutcomeTimeoutUnknown, "gw", "still unknown", "", resolvedAt); err == nil {
			t.Fatal("an attempt was reconciled into TIMEOUT_UNKNOWN")
		}
		if err := att.Reconcile("MAYBE", "gw", "?", "", resolvedAt); err == nil {
			t.Fatal("an attempt was reconciled into an undeclared outcome")
		}
	})
}

func TestAttemptPermitsFailoverTakesTheMostRestrictiveAnswer(t *testing.T) {
	// Verifies: docs/spec/00-design-baseline.md §9.1, ADR-012. Three independent sources of
	// judgement — the outcome, the normalized decline reason, and the scheme's own advice — and
	// the answer is the *most restrictive* of them. Any other combination rule eventually
	// produces a retry the schemes read as card testing.
	t.Parallel()

	dispatchedAt := testEpoch.Add(time.Second)
	resolvedAt := dispatchedAt.Add(time.Second)

	tests := []struct {
		name          string
		drive         func(*Attempt) error
		wantFailover  bool
		why           string
		wantNoRetry   bool
		wantOutcomeIs AttemptOutcome
	}{
		{
			name:          "pending",
			drive:         func(*Attempt) error { return nil },
			wantOutcomeIs: OutcomePending,
			why:           "nothing has been sent",
		},
		{
			name:          "dispatched",
			drive:         func(a *Attempt) error { return a.Dispatch(dispatchedAt) },
			wantOutcomeIs: OutcomeDispatched,
			why:           "the call has not come back",
		},
		{
			name: "success",
			drive: func(a *Attempt) error {
				if err := a.Dispatch(dispatchedAt); err != nil {
					return err
				}
				return a.Succeed("gw", "ok", resolvedAt)
			},
			wantOutcomeIs: OutcomeSuccess,
			why:           "money moved",
		},
		{
			name:          "pre-dispatch error",
			drive:         func(a *Attempt) error { return a.Fail("CIRCUIT_OPEN", "open", resolvedAt) },
			wantFailover:  true,
			wantOutcomeIs: OutcomeError,
			why:           "the gateway never made a decision",
		},
		{
			name: "soft decline",
			drive: func(a *Attempt) error {
				if err := a.Dispatch(dispatchedAt); err != nil {
					return err
				}
				return a.Decline(DeclineIssuerUnavailable, "gw", "no", false, resolvedAt)
			},
			wantFailover:  true,
			wantOutcomeIs: OutcomeDeclined,
			why:           "the issuer might approve the same instruction elsewhere",
		},
		{
			name: "hard decline",
			drive: func(a *Attempt) error {
				if err := a.Dispatch(dispatchedAt); err != nil {
					return err
				}
				return a.Decline(DeclineStolenCard, "gw", "no", false, resolvedAt)
			},
			wantOutcomeIs: OutcomeDeclined,
			why:           "retrying a stolen card elsewhere is card testing",
		},
		{
			// The case that matters: the outcome and the reason both say "you may retry", and the
			// scheme says do not. The scheme wins.
			name: "soft decline with scheme advice not to retry",
			drive: func(a *Attempt) error {
				if err := a.Dispatch(dispatchedAt); err != nil {
					return err
				}
				return a.Decline(DeclineIssuerUnavailable, "gw", "no", true, resolvedAt)
			},
			wantOutcomeIs: OutcomeDeclined,
			wantNoRetry:   true,
			why:           "scheme-level advice overrides our judgement in the restrictive direction only",
		},
		{
			name: "timed out",
			drive: func(a *Attempt) error {
				if err := a.Dispatch(dispatchedAt); err != nil {
					return err
				}
				return a.TimeOut("no answer", resolvedAt)
			},
			wantOutcomeIs: OutcomeTimeoutUnknown,
			why:           "money may already have moved",
		},
		{
			name: "reconciled as never delivered",
			drive: func(a *Attempt) error {
				if err := a.Dispatch(dispatchedAt); err != nil {
					return err
				}
				if err := a.TimeOut("no answer", resolvedAt); err != nil {
					return err
				}
				return a.Reconcile(OutcomeError, "", "no such transaction", "", resolvedAt)
			},
			wantFailover:  true,
			wantOutcomeIs: OutcomeError,
			why:           "the gateway's own lookup proved the request never arrived",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			att := newTestAttempt(t)
			if err := tc.drive(att); err != nil {
				t.Fatalf("driving the attempt: %v", err)
			}
			if att.Outcome() != tc.wantOutcomeIs {
				t.Fatalf("outcome = %s, want %s", att.Outcome(), tc.wantOutcomeIs)
			}
			if att.NetworkAdviceNoRetry() != tc.wantNoRetry {
				t.Fatalf("networkAdviceNoRetry = %v, want %v", att.NetworkAdviceNoRetry(), tc.wantNoRetry)
			}
			if got := att.PermitsFailover(); got != tc.wantFailover {
				t.Fatalf("PermitsFailover() = %v, want %v (%s)", got, tc.wantFailover, tc.why)
			}
		})
	}

	t.Run("scheme advice suppresses failover on every outcome", func(t *testing.T) {
		t.Parallel()
		// Even an ERROR — which normally permits failover unconditionally — is suppressed. The
		// advice is a statement about the card, not about the call.
		att := newTestAttempt(t)
		if err := att.Dispatch(dispatchedAt); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if err := att.Decline(DeclineTryAgainLater, "gw", "no", true, resolvedAt); err != nil {
			t.Fatalf("Decline: %v", err)
		}
		if att.PermitsFailover() {
			t.Fatal("scheme advice was overridden by a soft decline reason")
		}
		// The reason-level rule alone would have said yes.
		if !DeclineTryAgainLater.PermitsFailover() {
			t.Fatal("TRY_AGAIN_LATER is not a soft decline")
		}
	})
}

func TestBindConnection(t *testing.T) {
	// Verifies: a connection reference is evidence about which credential signed a request that
	// has already gone out. A value that can be edited afterwards is not evidence.
	t.Parallel()

	t.Run("bound before dispatch, and idempotently", func(t *testing.T) {
		t.Parallel()
		att := newTestAttempt(t)
		if att.ConnectionID() != "" {
			t.Fatalf("connectionId = %q on a fresh attempt", att.ConnectionID())
		}
		if err := att.BindConnection("gwc_1"); err != nil {
			t.Fatalf("BindConnection: %v", err)
		}
		// A retried save must be safe.
		if err := att.BindConnection("gwc_1"); err != nil {
			t.Fatalf("re-binding the same connection: %v", err)
		}
		if att.ConnectionID() != "gwc_1" {
			t.Fatalf("connectionId = %q", att.ConnectionID())
		}
	})

	t.Run("a blank reference is refused", func(t *testing.T) {
		t.Parallel()
		att := newTestAttempt(t)
		if apierror.CodeOf(att.BindConnection("")) != apierror.CodeValidationFailed {
			t.Fatal("a blank connection reference was accepted")
		}
	})

	t.Run("re-binding to a different connection is refused", func(t *testing.T) {
		t.Parallel()
		// A failover creates a *new* attempt; an attempt whose connection changed would mean a
		// previous try's record had been overwritten.
		att := newTestAttempt(t)
		if err := att.BindConnection("gwc_1"); err != nil {
			t.Fatalf("BindConnection: %v", err)
		}
		if apierror.CodeOf(att.BindConnection("gwc_2")) != apierror.CodeInvalidStateTransition {
			t.Fatal("an attempt was re-bound to a different connection")
		}
		if att.ConnectionID() != "gwc_1" {
			t.Fatalf("connectionId = %q after a refused re-bind", att.ConnectionID())
		}
	})

	t.Run("binding after dispatch is refused", func(t *testing.T) {
		t.Parallel()
		att := newTestAttempt(t)
		if err := att.Dispatch(testEpoch); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if apierror.CodeOf(att.BindConnection("gwc_1")) != apierror.CodeInvalidStateTransition {
			t.Fatal("a dispatched attempt accepted a connection binding")
		}
	})
}

func TestRehydrateAttempt(t *testing.T) {
	t.Parallel()

	dispatchedAt := testEpoch.Add(time.Second)
	resolvedAt := dispatchedAt.Add(300 * time.Millisecond)

	base := RehydrateAttemptParams{
		ID: shared.NewAttemptID(), PaymentID: shared.NewPaymentID(), TenantID: shared.NewTenantID(),
		GatewayID: "adyen", ConnectionID: "gwc_1", Operation: shared.OpCapture, Sequence: 2,
		Amount: usd(75_00), Outcome: OutcomeDeclined, GatewayRef: "gw_txn_9",
		IdempotencyKey: "abcdefabcdefabcdefabcdefabcdefab", DeclineReason: DeclineFraudulent,
		ErrorCode: "", ErrorMessage: "", RawStatus: "Refused", NetworkAdviceNoRetry: true,
		DispatchedAt: &dispatchedAt, ResolvedAt: &resolvedAt, Latency: 300 * time.Millisecond,
		CreatedAt: testEpoch, UpdatedAt: resolvedAt,
	}

	att, err := RehydrateAttempt(base)
	if err != nil {
		t.Fatalf("RehydrateAttempt: %v", err)
	}
	checks := []struct {
		field string
		ok    bool
	}{
		{"id", att.ID() == base.ID},
		{"paymentId", att.PaymentID() == base.PaymentID},
		{"tenantId", att.TenantID() == base.TenantID},
		{"gatewayId", att.GatewayID() == base.GatewayID},
		{"connectionId", att.ConnectionID() == base.ConnectionID},
		{"operation", att.Operation() == base.Operation},
		{"sequence", att.Sequence() == base.Sequence},
		{"amount", att.Amount().Equal(base.Amount)},
		{"outcome", att.Outcome() == base.Outcome},
		{"gatewayRef", att.GatewayRef() == base.GatewayRef},
		{"idempotencyKey", att.IdempotencyKey() == base.IdempotencyKey},
		{"declineReason", att.DeclineReason() == base.DeclineReason},
		{"rawStatus", att.RawStatus() == base.RawStatus},
		{"networkAdviceNoRetry", att.NetworkAdviceNoRetry()},
		{"latency", att.Latency() == base.Latency},
		{"createdAt", att.CreatedAt().Equal(base.CreatedAt)},
		{"updatedAt", att.UpdatedAt().Equal(base.UpdatedAt)},
		// The scheme's advice is persisted, so a routing decision taken after a restart is the
		// same decision it would have taken before.
		{"failover", !att.PermitsFailover()},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s did not survive rehydration", c.field)
		}
	}

	// A blank connection is permitted: attempts written before the column existed have no value
	// for it, and refusing them would make the whole payment unreadable over a descriptive field.
	blank := base
	blank.ConnectionID = ""
	if _, err := RehydrateAttempt(blank); err != nil {
		t.Fatalf("an attempt with no connection was refused: %v", err)
	}

	bad := base
	bad.Outcome = "SCHRODINGER"
	if _, err := RehydrateAttempt(bad); apierror.CodeOf(err) != apierror.CodeInternalError {
		t.Fatalf("unknown outcome: code = %s, want INTERNAL_ERROR", apierror.CodeOf(err))
	}
}

func TestAttemptSequenceAndGatewayAreRecordedByTheAggregate(t *testing.T) {
	// Verifies: ADR-012 step 1 — failover must not mutate the record of the previous try, because
	// that record is the only evidence that a charge may exist at the previous gateway.
	t.Parallel()

	p, clk := newTestPayment(t)

	first, err := p.StartAttempt("stripe", "rpl_1", shared.OpAuthorize, clk)
	if err != nil {
		t.Fatalf("StartAttempt: %v", err)
	}
	if err := p.MarkProcessing(clk); err != nil {
		t.Fatalf("MarkProcessing: %v", err)
	}
	if err := first.Dispatch(clk.Now()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if err := first.Decline(DeclineIssuerUnavailable, "gw_txn_1", "issuer down", false, clk.Now()); err != nil {
		t.Fatalf("Decline: %v", err)
	}
	if !first.PermitsFailover() {
		t.Fatal("a soft decline does not permit failover")
	}

	clk.Advance(2 * time.Second)
	second, err := p.StartAttempt("adyen", "rpl_1", shared.OpAuthorize, clk)
	if err != nil {
		t.Fatalf("failover StartAttempt: %v", err)
	}

	if second.Sequence() != 2 || first.Sequence() != 1 {
		t.Fatalf("sequences = %d / %d", first.Sequence(), second.Sequence())
	}
	if second.GatewayID() != "adyen" || first.GatewayID() != "stripe" {
		t.Fatalf("gateways = %s / %s", first.GatewayID(), second.GatewayID())
	}
	// The previous try is untouched: its outcome, its gateway reference and its key are exactly
	// what they were, which is what makes it evidence.
	if first.Outcome() != OutcomeDeclined || first.GatewayRef() != "gw_txn_1" {
		t.Fatalf("the first attempt was mutated: %s / %q", first.Outcome(), first.GatewayRef())
	}
	if first.IdempotencyKey() == second.IdempotencyKey() {
		t.Fatal("the failover reused the first attempt's gateway idempotency key")
	}
	// The aggregate's derived view follows the live attempt.
	if p.SelectedGateway() != "adyen" || p.LatestAttempt().ID() != second.ID() {
		t.Fatalf("selectedGateway = %s latest = %s", p.SelectedGateway(), p.LatestAttempt().ID())
	}
	if len(p.Attempts()) != 2 {
		t.Fatalf("%d attempts, want 2", len(p.Attempts()))
	}

	evts := p.DrainEvents()
	last := evts[len(evts)-1]
	if last.Type != EventPaymentAttempted || last.Payload["gatewayId"] != "adyen" || last.Payload["sequence"] != 2 {
		t.Fatalf("attempt event = %+v", last)
	}

	// Every attempt is denominated in the payment's currency; a per-attempt currency would make
	// the money invariants unevaluable.
	for _, a := range p.Attempts() {
		if a.Amount().Currency() != p.Currency() {
			t.Fatalf("attempt %s is in %s, payment is in %s", a.ID(), a.Amount().Currency(), p.Currency())
		}
		if a.PaymentID() != p.ID() || a.TenantID() != p.TenantID() {
			t.Fatalf("attempt %s is not stamped with its payment and tenant", a.ID())
		}
	}
}
