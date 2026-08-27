package contract

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// RunSuite runs every conformance assertion against one adapter.
//
// Each assertion is a named subtest so a failure names the obligation rather than a line number:
// "adyen/HardDeclineMapsToNonFailoverReason" tells a reviewer what is broken and what it costs,
// which "adapter_test.go:412" does not.
func RunSuite(t *testing.T, s Subject) {
	t.Helper()
	if s.Name == "" {
		t.Fatal("contract: a subject requires a name")
	}
	if s.NewGateway == nil || s.NewProvisioner == nil || s.NewVerifier == nil {
		t.Fatalf("%s: a subject must supply all three constructors; an adapter that implements only the payment path is not integrated", s.Name)
	}

	t.Run(s.Name, func(t *testing.T) {
		t.Run("IdempotencyKeyIsSent", func(t *testing.T) { assertIdempotencyKeyIsSent(t, s) })
		t.Run("RepeatedCallWithSameKeyIsSafe", func(t *testing.T) { assertRepeatedCallIsSafe(t, s) })
		t.Run("TimeoutYieldsOutcomeUnknown", func(t *testing.T) { assertTimeoutIsUnknown(t, s) })
		t.Run("ConnectionRefusedYieldsError", func(t *testing.T) { assertConnectionRefusedIsError(t, s) })
		t.Run("HardDeclineMapsToNonFailoverReason", func(t *testing.T) { assertHardDecline(t, s) })
		t.Run("SoftDeclineMapsToFailoverReason", func(t *testing.T) { assertSoftDecline(t, s) })
		t.Run("UnmappedDeclineIsUnknownAndDoesNotFailover", func(t *testing.T) { assertUnmappedDecline(t, s) })
		t.Run("SuccessCarriesGatewayRef", func(t *testing.T) { assertSuccessCarriesRef(t, s) })
		t.Run("LookupFindsByIdempotencyKeyAlone", func(t *testing.T) { assertLookupByKeyAlone(t, s) })
		t.Run("AmountAndCurrencyEchoed", func(t *testing.T) { assertAmountEcho(t, s) })
		t.Run("ContextCancellationIsHonoured", func(t *testing.T) { assertContextCancellation(t, s) })
		t.Run("CredentialsNeverAppearInErrorsOrLogs", func(t *testing.T) { assertNoCredentialLeak(t, s) })
		t.Run("WebhookSignatureVerification", func(t *testing.T) { assertWebhookVerification(t, s) })
		t.Run("WebhookEventIDIsStableForSamePayload", func(t *testing.T) { assertWebhookEventIDStable(t, s) })
		t.Run("NilResultAndNilErrorNeverReturned", func(t *testing.T) { assertNeverNilNil(t, s) })
		t.Run("ProvisionIsIdempotent", func(t *testing.T) { assertProvisionIdempotent(t, s) })
		t.Run("DeprovisionToleratesMissingAccount", func(t *testing.T) { assertDeprovisionTolerant(t, s) })
	})
}

// AssertionCount is the number of named top-level assertions RunSuite executes. It is exported so a
// meta-test can notice an assertion being quietly deleted — the failure mode where a suite stays
// green by shrinking.
const AssertionCount = 17

// --- 1. IdempotencyKeyIsSent ---------------------------------------------------------------------

// assertIdempotencyKeyIsSent proves the key reaches the gateway in the form that gateway
// deduplicates on.
//
// Checking the *sent request* rather than the returned result is the entire point. An adapter that
// never sends the key returns a perfectly good Result, passes every response-shaped test, and then
// double-charges the first time the orchestrator retries a timeout — which is the one situation the
// key exists for.
func assertIdempotencyKeyIsSent(t *testing.T, s Subject) {
	for _, op := range moneyMovingOps() {
		if op.skip != nil && op.skip(s) {
			continue
		}
		t.Run(op.name, func(t *testing.T) {
			d := s.doer(httpx.Exchange{Response: op.success(s)})
			g := s.gateway(t, d)
			res, err := op.invoke(context.Background(), g, s, SuiteIdempotencyKey)
			requireNoDoubleAnswer(t, s.Name+"/"+op.name, res, err)
			if err != nil {
				t.Fatalf("%s %s: unexpected error: %v", s.Name, op.name, err)
			}
			req, ok := d.FirstMatching(isMutatingRequest)
			if !ok {
				t.Fatalf("%s %s: no money-moving request was recorded; the adapter did not call the gateway", s.Name, op.name)
			}
			got := s.IdempotencyKeyOf(req)
			if got != SuiteIdempotencyKey {
				t.Fatalf("%s %s: the gateway received idempotency key %q, want %q; without it a transport retry is a second charge",
					s.Name, op.name, got, SuiteIdempotencyKey)
			}
		})
	}
}

// --- 2. RepeatedCallWithSameKeyIsSafe ------------------------------------------------------------

// assertRepeatedCallIsSafe proves a second call with the same key is safe.
//
// Safety here is a property of the pair (adapter, gateway): the gateway deduplicates and replays,
// and the adapter must present the same key both times and must build its answer from the replayed
// response rather than from its own request. The second condition is the one that catches an
// adapter which synthesises a result optimistically — it would return a *different* reference the
// second time, and the platform would end up holding two references to one charge.
func assertRepeatedCallIsSafe(t *testing.T, s Subject) {
	for _, op := range moneyMovingOps() {
		if op.skip != nil && op.skip(s) {
			continue
		}
		t.Run(op.name, func(t *testing.T) {
			d := s.doer(httpx.Exchange{Response: op.success(s)})
			g := s.gateway(t, d)

			first, err := op.invoke(context.Background(), g, s, SuiteIdempotencyKey)
			if err != nil {
				t.Fatalf("%s %s: first call: %v", s.Name, op.name, err)
			}
			second, err := op.invoke(context.Background(), g, s, SuiteIdempotencyKey)
			if err != nil {
				t.Fatalf("%s %s: second call: %v", s.Name, op.name, err)
			}
			if first.GatewayRef != second.GatewayRef {
				t.Fatalf("%s %s: the replayed call produced reference %q, the first produced %q; the adapter is not reading the gateway's replayed answer",
					s.Name, op.name, second.GatewayRef, first.GatewayRef)
			}
			if first.Status != second.Status {
				t.Fatalf("%s %s: the replayed call produced status %q, the first produced %q",
					s.Name, op.name, second.Status, first.Status)
			}

			// Every money-moving request the adapter issued must carry the key. One that does not is
			// a call the gateway will treat as new.
			mutating := 0
			d.CountMatching(func(r httpx.RecordedRequest) bool {
				if !isMutatingRequest(r) {
					return false
				}
				mutating++
				if s.IdempotencyKeyOf(r) != SuiteIdempotencyKey {
					t.Errorf("%s %s: a money-moving request was sent without the idempotency key", s.Name, op.name)
				}
				return true
			})
			if mutating != 2 {
				t.Fatalf("%s %s: expected exactly 2 money-moving requests for 2 calls, got %d", s.Name, op.name, mutating)
			}
		})
	}
}

// --- 3. TimeoutYieldsOutcomeUnknown ---------------------------------------------------------------

// assertTimeoutIsUnknown proves a client timeout after the request was written returns
// ErrOutcomeUnknown rather than a failure.
//
// This is the single most consequential assertion in the suite. A gateway that does not answer may
// still have authorized the card; reporting that as a failure lets the orchestrator fail over, and
// the payer is charged twice. It is asserted for every money-moving operation because the adapter
// author who remembered it for authorize is exactly the one who forgot it for refund — and a
// duplicated refund is money out of the merchant's account.
func assertTimeoutIsUnknown(t *testing.T, s Subject) {
	for _, op := range moneyMovingOps() {
		if op.skip != nil && op.skip(s) {
			continue
		}
		t.Run(op.name, func(t *testing.T) {
			d := s.doer(httpx.Exchange{Response: httpx.TimeoutResponse()})
			g := s.gateway(t, d)
			res, err := op.invoke(context.Background(), g, s, SuiteIdempotencyKey)
			requireNoDoubleAnswer(t, s.Name+"/"+op.name, res, err)
			if err == nil {
				t.Fatalf("%s %s: a timeout produced a result (%v) and no error; the adapter invented an outcome",
					s.Name, op.name, res.Status)
			}
			if !isOutcomeUnknown(err) {
				t.Fatalf("%s %s: a timeout produced %v, want an error satisfying errors.Is(err, spi.ErrOutcomeUnknown); "+
					"reporting a timeout as a failure is how a platform double-charges", s.Name, op.name, err)
			}
		})
	}

	// A timeout on a *lookup* must not be an unknown outcome: a lookup moves no money, and parking a
	// payment in reconciliation because a status read timed out is pure noise.
	t.Run("LookupTimeoutIsNotUnknown", func(t *testing.T) {
		d := s.doer(httpx.Exchange{Response: httpx.TimeoutResponse()})
		g := s.gateway(t, d)
		res, err := g.Lookup(context.Background(), s.lookupRequest(SuiteGatewayRef, SuiteIdempotencyKey))
		requireNoDoubleAnswer(t, s.Name+"/Lookup", res, err)
		if err == nil {
			t.Fatalf("%s: a lookup timeout produced no error", s.Name)
		}
		if isOutcomeUnknown(err) {
			t.Fatalf("%s: a lookup timeout was reported as an unknown outcome; a lookup moves no money and cannot leave one ambiguous", s.Name)
		}
	})
}

// --- 4. ConnectionRefusedYieldsError ---------------------------------------------------------------

// assertConnectionRefusedIsError proves a pre-flight failure is a plain error.
//
// The mirror image of the timeout assertion, and just as consequential in the other direction. A
// refused connection means the request never left the process, so the gateway provably did not act
// and the operation is safe to retry. Reporting it as unknown parks a healthy payment in
// reconciliation, where it waits for a human.
func assertConnectionRefusedIsError(t *testing.T, s Subject) {
	for _, op := range moneyMovingOps() {
		if op.skip != nil && op.skip(s) {
			continue
		}
		t.Run(op.name, func(t *testing.T) {
			// The refusal is a fallback so it also covers a subject whose preamble would otherwise
			// answer first: every call fails pre-flight, which is what a refused host looks like.
			d := httpx.NewRecordingDoer().WithFallback(httpx.Exchange{Err: httpx.ConnectionRefused()})
			g := s.gateway(t, d)
			res, err := op.invoke(context.Background(), g, s, SuiteIdempotencyKey)
			requireNoDoubleAnswer(t, s.Name+"/"+op.name, res, err)
			if err == nil {
				t.Fatalf("%s %s: a refused connection produced no error", s.Name, op.name)
			}
			if isOutcomeUnknown(err) {
				t.Fatalf("%s %s: a refused connection was reported as an unknown outcome; the request never left the process, "+
					"so the payment is provably untouched and needlessly parking it costs a sale", s.Name, op.name)
			}
		})
	}
}

// --- 5, 6, 7. decline mapping ----------------------------------------------------------------------

// assertHardDecline proves a hard decline normalizes to a reason that forbids failover.
func assertHardDecline(t *testing.T, s Subject) {
	res := runDecline(t, s, s.Responses.AuthorizeHardDecline(SuiteGatewayRef))
	if res.Status != spi.StatusDeclined {
		t.Fatalf("%s: a hard decline produced status %q, want DECLINED", s.Name, res.Status)
	}
	if res.DeclineReason == "" {
		t.Fatalf("%s: a decline carried no normalized reason; the routing engine cannot decide without one", s.Name)
	}
	if res.DeclineReason.PermitsFailover() {
		t.Fatalf("%s: a hard decline normalized to %q, which permits failover; re-presenting a hard decline to another acquirer "+
			"is indistinguishable from card testing and gets the platform's accounts closed", s.Name, res.DeclineReason)
	}
}

// assertSoftDecline proves a soft decline normalizes to a reason that permits failover.
//
// The failure this catches is an adapter that maps everything to a safe-looking hard reason: it
// never card-tests, and it also never fails over, so every issuer blip becomes a lost sale.
func assertSoftDecline(t *testing.T, s Subject) {
	res := runDecline(t, s, s.Responses.AuthorizeSoftDecline(SuiteGatewayRef))
	if res.Status != spi.StatusDeclined {
		t.Fatalf("%s: a soft decline produced status %q, want DECLINED", s.Name, res.Status)
	}
	if !res.DeclineReason.PermitsFailover() {
		t.Fatalf("%s: a soft decline normalized to %q, which forbids failover; an issuer-unavailable refusal that cannot "+
			"be retried elsewhere is a lost sale for a transient condition", s.Name, res.DeclineReason)
	}
	if res.NetworkAdviceNoRetry {
		t.Fatalf("%s: a soft decline was marked no-retry by scheme advice, which contradicts the reason", s.Name)
	}
}

// assertUnmappedDecline proves an unrecognised vendor code becomes UNKNOWN and does not fail over.
//
// This is the default arm, which is the arm nobody exercises. Defaulting to a retryable reason is
// how a platform ends up card testing on an attacker's behalf: the attacker supplies cards until
// one is approved, and every unmapped refusal along the way is helpfully re-presented to a second
// acquirer.
func assertUnmappedDecline(t *testing.T, s Subject) {
	res := runDecline(t, s, s.Responses.AuthorizeUnmappedDecline(SuiteGatewayRef))
	if res.Status != spi.StatusDeclined {
		t.Fatalf("%s: an unmapped decline produced status %q, want DECLINED", s.Name, res.Status)
	}
	if res.DeclineReason != payment.DeclineUnknown {
		t.Fatalf("%s: an unmapped vendor code normalized to %q, want UNKNOWN; inventing a reason for a code nobody has "+
			"reasoned about is worse than admitting ignorance", s.Name, res.DeclineReason)
	}
	if res.DeclineReason.PermitsFailover() {
		t.Fatalf("%s: UNKNOWN permits failover for this adapter, which inverts the safe default", s.Name)
	}
}

// runDecline drives one authorization against a scripted decline and insists it is a *result*, not
// an error. A decline is a successful call with a refused outcome; an adapter that returns an error
// makes the orchestrator record ERROR, which permits failover — the exact behaviour a decline must
// suppress.
func runDecline(t *testing.T, s Subject, resp *spi.HTTPResponse) *spi.Result {
	t.Helper()
	d := s.doer(httpx.Exchange{Response: resp})
	g := s.gateway(t, d)
	res, err := g.Authorize(context.Background(), s.authorizeRequest(SuiteIdempotencyKey))
	requireNoDoubleAnswer(t, s.Name+"/Authorize", res, err)
	if err != nil {
		t.Fatalf("%s: a decline was returned as an error (%v); a decline is a successful call with a refused outcome, "+
			"and reporting it as an error lets the orchestrator fail over", s.Name, err)
	}
	return res
}

// --- 8. SuccessCarriesGatewayRef --------------------------------------------------------------------

// assertSuccessCarriesRef proves every actionable result carries something the platform can look up.
//
// An authorization the platform cannot reference is an authorization it cannot capture, void or
// reconcile — the payer's funds are held and nobody can release them.
func assertSuccessCarriesRef(t *testing.T, s Subject) {
	for _, op := range moneyMovingOps() {
		if op.skip != nil && op.skip(s) {
			continue
		}
		t.Run(op.name, func(t *testing.T) {
			d := s.doer(httpx.Exchange{Response: op.success(s)})
			g := s.gateway(t, d)
			res, err := op.invoke(context.Background(), g, s, SuiteIdempotencyKey)
			if err != nil {
				t.Fatalf("%s %s: %v", s.Name, op.name, err)
			}
			assertRefPresent(t, s.Name+"/"+op.name, res)
		})
	}
	t.Run("Decline", func(t *testing.T) {
		res := runDecline(t, s, s.Responses.AuthorizeHardDecline(SuiteGatewayRef))
		assertRefPresent(t, s.Name+"/Decline", res)
	})
	t.Run("Lookup", func(t *testing.T) {
		d := s.doer(httpx.Exchange{Response: s.Responses.LookupByRef(SuiteGatewayRef, SuiteIdempotencyKey, SuiteAmount)})
		g := s.gateway(t, d)
		res, err := g.Lookup(context.Background(), s.lookupRequest(SuiteGatewayRef, SuiteIdempotencyKey))
		if err != nil {
			t.Fatalf("%s Lookup: %v", s.Name, err)
		}
		assertRefPresent(t, s.Name+"/Lookup", res)
	})
}

func assertRefPresent(t *testing.T, where string, res *spi.Result) {
	t.Helper()
	if res.Status == spi.StatusNotFound || res.Status == spi.StatusFailed {
		return
	}
	if res.GatewayRef == "" {
		t.Fatalf("%s: a %s result carries no gateway reference; the platform can never capture, void or reconcile it",
			where, res.Status)
	}
}

// --- 9. LookupFindsByIdempotencyKeyAlone -----------------------------------------------------------

// assertLookupByKeyAlone proves a lookup resolves with an empty GatewayRef.
//
// This is the method that makes a timeout survivable and the one most often omitted, because the
// happy path never needs it. After a crash between "sent the request" and "recorded the response"
// the platform holds no reference at all; the deterministic idempotency key is the only handle, and
// without this an unknown outcome can only be resolved by a webhook that may never come.
func assertLookupByKeyAlone(t *testing.T, s Subject) {
	d := s.doer(httpx.Exchange{Response: s.Responses.LookupByKey(SuiteGatewayRef, SuiteIdempotencyKey, SuiteAmount)})
	g := s.gateway(t, d)

	res, err := g.Lookup(context.Background(), s.lookupRequest("", SuiteIdempotencyKey))
	requireNoDoubleAnswer(t, s.Name+"/Lookup", res, err)
	if err != nil {
		t.Fatalf("%s: a lookup by idempotency key alone failed: %v", s.Name, err)
	}
	if res.Status == spi.StatusNotFound {
		t.Fatalf("%s: a lookup by idempotency key alone reported NOT_FOUND for a transaction the gateway knows about; "+
			"an unknown outcome would then be unresolvable except by a webhook", s.Name)
	}
	if res.GatewayRef == "" {
		t.Fatalf("%s: a lookup by idempotency key alone found the transaction but returned no reference", s.Name)
	}

	// The miss must be a NOT_FOUND result, not an error. NOT_FOUND combined with a deterministic key
	// is positive evidence the operation never took effect, and it is the only way an unknown
	// outcome is ever resolved in the safe-to-retry direction.
	d2 := s.doer(httpx.Exchange{Response: s.Responses.LookupNotFound()})
	g2 := s.gateway(t, d2)
	res2, err2 := g2.Lookup(context.Background(), s.lookupRequest("", SuiteAltIdempotencyKey))
	requireNoDoubleAnswer(t, s.Name+"/LookupMiss", res2, err2)
	if err2 != nil {
		t.Fatalf("%s: a lookup miss was returned as an error (%v); NOT_FOUND is a finding, not a failure", s.Name, err2)
	}
	if res2.Status != spi.StatusNotFound {
		t.Fatalf("%s: a lookup miss produced status %q, want NOT_FOUND", s.Name, res2.Status)
	}
}

// --- 10. AmountAndCurrencyEchoed -------------------------------------------------------------------

// assertAmountEcho proves a mismatched echo is surfaced rather than swallowed.
//
// A gateway that acts on a different amount or in a different currency from the one requested is a
// contract violation, and absorbing it means the ledger records what the platform asked for while
// the bank records what happened. The two are then reconciled by a human, months later, per
// transaction.
func assertAmountEcho(t *testing.T, s Subject) {
	t.Run("MatchingEchoIsAccepted", func(t *testing.T) {
		d := s.doer(httpx.Exchange{Response: s.Responses.AuthorizeApproved(SuiteGatewayRef, SuiteAmount, SuiteIdempotencyKey)})
		g := s.gateway(t, d)
		res, err := g.Authorize(context.Background(), s.authorizeRequest(SuiteIdempotencyKey))
		if err != nil {
			t.Fatalf("%s: a correctly echoed authorization was rejected: %v", s.Name, err)
		}
		for _, m := range []*money.Money{res.AuthorizedAmount, res.CapturedAmount} {
			if m == nil {
				continue
			}
			if m.Currency() != SuiteAmount.Currency() {
				t.Fatalf("%s: the result reports currency %s for a request in %s", s.Name, m.Currency(), SuiteAmount.Currency())
			}
			if m.Amount() > SuiteAmount.Amount() {
				t.Fatalf("%s: the result reports %d minor units for a request of %d", s.Name, m.Amount(), SuiteAmount.Amount())
			}
		}
	})

	t.Run("MismatchedEchoIsSurfaced", func(t *testing.T) {
		d := s.doer(httpx.Exchange{Response: s.Responses.AuthorizeAmountMismatch(SuiteGatewayRef, SuiteAmount)})
		g := s.gateway(t, d)
		res, err := g.Authorize(context.Background(), s.authorizeRequest(SuiteIdempotencyKey))
		requireNoDoubleAnswer(t, s.Name+"/Authorize", res, err)
		if err == nil {
			t.Fatalf("%s: a gateway that echoed a different amount and currency was accepted silently (status %q); "+
				"the ledger and the bank would then disagree by construction", s.Name, res.Status)
		}
		if apierror.CodeOf(err) != apierror.CodeGatewayContractViolation {
			t.Fatalf("%s: a mismatched echo produced code %s, want %s", s.Name,
				apierror.CodeOf(err), apierror.CodeGatewayContractViolation)
		}
	})
}

// --- 11. ContextCancellationIsHonoured ---------------------------------------------------------------

// assertContextCancellation proves a cancelled context is answered promptly and as a plain error.
//
// Promptly, because the orchestrator's timeout cascade and its bulkhead sizing both assume an
// adapter releases its slot when the deadline passes. As a plain error, because a context cancelled
// before the request was written means nothing was sent — so the operation is provably untouched
// and must not be parked as unknown.
func assertContextCancellation(t *testing.T, s Subject) {
	for _, op := range moneyMovingOps() {
		if op.skip != nil && op.skip(s) {
			continue
		}
		t.Run(op.name, func(t *testing.T) {
			d := s.doer(httpx.Exchange{Response: op.success(s), Latency: time.Second})
			g := s.gateway(t, d)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			done := make(chan struct{})
			var res *spi.Result
			var err error
			go func() {
				res, err = op.invoke(ctx, g, s, SuiteIdempotencyKey)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("%s %s: the adapter did not return within 2s of a cancelled context; it is ignoring the deadline, "+
					"which breaks the orchestrator's timeout cascade", s.Name, op.name)
			}
			requireNoDoubleAnswer(t, s.Name+"/"+op.name, res, err)
			if err == nil {
				t.Fatalf("%s %s: a cancelled context produced a result and no error", s.Name, op.name)
			}
			if isOutcomeUnknown(err) {
				t.Fatalf("%s %s: a context cancelled before the request was sent was reported as an unknown outcome",
					s.Name, op.name)
			}
		})
	}
}

// --- 12. CredentialsNeverAppearInErrorsOrLogs ---------------------------------------------------------

// assertNoCredentialLeak runs every operation against a failing gateway and proves no credential
// value appears in the returned error's full rendering or in a captured log buffer.
//
// The error path is where credentials leak, because it is the path nobody exercises and the one
// where a developer reaches for "%+v of everything" while debugging. A leaked gateway key is a PCI
// incident: it reaches a log index that far more people can read than can read the secret store.
//
// Both %+v and %#v are rendered, because they take different routes through fmt and a value that
// survives one and not the other is still a value in a log line.
func assertNoCredentialLeak(t *testing.T, s Subject) {
	secrets := s.secretValues()
	if len(secrets) == 0 {
		t.Fatalf("%s: the subject supplies no credential values long enough to search for; the assertion would pass vacuously", s.Name)
	}

	// slog's default logger is process-global, so this subtest cannot run in parallel with anything
	// that logs. It does not call t.Parallel, and it restores the previous default on the way out.
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)

	type call struct {
		name string
		run  func(spi.PaymentGateway) (any, error)
	}
	calls := []call{
		{"Authorize", func(g spi.PaymentGateway) (any, error) {
			return g.Authorize(context.Background(), s.authorizeRequest(SuiteIdempotencyKey))
		}},
		{"Capture", func(g spi.PaymentGateway) (any, error) {
			return g.Capture(context.Background(), s.captureRequest(SuiteIdempotencyKey))
		}},
		{"Refund", func(g spi.PaymentGateway) (any, error) {
			return g.Refund(context.Background(), s.refundRequest(SuiteIdempotencyKey))
		}},
		{"Void", func(g spi.PaymentGateway) (any, error) {
			return g.Void(context.Background(), s.voidRequest(SuiteIdempotencyKey))
		}},
		{"Lookup", func(g spi.PaymentGateway) (any, error) {
			return g.Lookup(context.Background(), s.lookupRequest(SuiteGatewayRef, SuiteIdempotencyKey))
		}},
	}

	// Two failing gateways, because the two failure classes render differently: an authenticated
	// rejection carries a vendor error body, and a transport failure carries a Go error string that
	// may embed the URL.
	failures := []struct {
		name string
		doer func() *httpx.RecordingDoer
	}{
		{"AuthRejected", func() *httpx.RecordingDoer {
			return httpx.NewRecordingDoer().WithFallback(httpx.Exchange{Response: s.Responses.AuthFailure()})
		}},
		{"TransportFailure", func() *httpx.RecordingDoer {
			return httpx.NewRecordingDoer().WithFallback(httpx.Exchange{Err: httpx.ConnectionRefused()})
		}},
		{"MalformedBody", func() *httpx.RecordingDoer {
			return httpx.NewRecordingDoer().WithFallback(httpx.Exchange{Response: MalformedResponse()})
		}},
	}

	for _, f := range failures {
		for _, c := range calls {
			g, err := s.NewGateway(f.doer())
			if err != nil {
				if leaked, found := containsAny(fullRender(err), secrets); found {
					t.Fatalf("%s: constructing the gateway leaked credential %q into an error", s.Name, mask(leaked))
				}
				continue
			}
			_, callErr := c.run(g)
			if leaked, found := containsAny(fullRender(callErr), secrets); found {
				t.Fatalf("%s: %s/%s leaked credential %q into the returned error; a credential in an error string reaches "+
					"a log index that far more people can read than can read the secret store",
					s.Name, f.name, c.name, mask(leaked))
			}
		}
	}

	if leaked, found := containsAny(logs.String(), secrets); found {
		t.Fatalf("%s: a credential %q reached the log buffer", s.Name, mask(leaked))
	}

	// The Credentials value itself must redact when printed, which is the last line of defence for
	// a caller who logs the request struct.
	rendered := fmt.Sprintf("%v|%+v|%#v|%s", s.Credentials, s.Credentials, s.Credentials, s.Credentials.String())
	if leaked, found := containsAny(rendered, secrets); found {
		t.Fatalf("%s: spi.Credentials rendered credential %q rather than redacting", s.Name, mask(leaked))
	}
}

// mask renders a leaked value in the failure message without repeating the leak into CI output.
func mask(v string) string {
	if len(v) <= 4 {
		return "****"
	}
	return v[:2] + "…" + v[len(v)-2:]
}

// MalformedResponse is a 200 whose body is not parseable by any of the adapters. Exported because
// the assertion that an unreadable answer becomes an unknown outcome needs the same body for every
// subject.
func MalformedResponse() *spi.HTTPResponse {
	return httpx.JSONResponse(200, `{"this document is": "not terminated"`)
}
