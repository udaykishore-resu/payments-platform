package contract

import (
	"context"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// WebhookNow is the instant the webhook assertions treat as "now". Fixed so the tolerance
// arithmetic is deterministic and a failure is reproducible rather than time-of-day dependent.
var WebhookNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// assertWebhookVerification is six assertions in one subtest, because they are six faces of one
// property: the ingress accepts exactly the events the gateway actually sent.
//
//	ValidSignaturePasses          — the baseline; a verifier that rejects everything is "secure"
//	                                and also an outage.
//	TamperedBodyFails             — the signature covers the body, not just the headers.
//	StaleTimestampFails           — a captured request stops being replayable.
//	RotatedSecretPasses           — a secret rotation does not drop events during the overlap.
//	                                Without this a rotation silently loses half the notifications
//	                                for the duration of the overlap window, and the platform never
//	                                learns those payments completed.
//	UnknownSecretFails            — the verifier is not accepting anything that looks signed.
//	VerificationPrecedesParsing   — the ordering property. An unauthenticated caller must not be
//	                                able to choose the input to a JSON parser, which is the largest
//	                                attack surface in the ingress path.
func assertWebhookVerification(t *testing.T, s Subject) {
	f := s.Webhook
	if f.Build == nil {
		t.Fatalf("%s: the subject supplies no webhook fixture", s.Name)
	}
	secrets := []string{f.Secret, f.RotatedSecret}

	newVerifier := func(t *testing.T, accept bool) spi.WebhookVerifier {
		t.Helper()
		var d spi.HTTPDoer
		if f.VerifierDoer != nil {
			d = f.VerifierDoer(accept)
		} else {
			d = httpx.NewRecordingDoer()
		}
		v, err := s.NewVerifier(d)
		if err != nil {
			t.Fatalf("%s: NewVerifier: %v", s.Name, err)
		}
		return v
	}

	t.Run("ValidSignaturePasses", func(t *testing.T) {
		body, headers := f.Build(f.Secret, WebhookNow)
		ev, err := newVerifier(t, true).Verify(context.Background(), body, headers, secrets, WebhookNow)
		if err != nil {
			t.Fatalf("%s: a validly signed webhook was rejected: %v", s.Name, err)
		}
		if ev == nil {
			t.Fatalf("%s: a validly signed webhook returned no event and no error", s.Name)
		}
		if ev.GatewayEventID == "" {
			t.Fatalf("%s: the verified event carries no deduplication key; the ingress cannot then suppress a redelivery", s.Name)
		}
	})

	t.Run("TamperedBodyFails", func(t *testing.T) {
		body, headers := f.Build(f.Secret, WebhookNow)
		tampered := append(append([]byte(nil), body...), ' ')
		if f.Tamper != nil {
			tampered = f.Tamper(body)
		}
		ev, err := newVerifier(t, false).Verify(context.Background(), tampered, headers, secrets, WebhookNow)
		if err == nil {
			t.Fatalf("%s: a tampered body was accepted (event %v); the signature does not cover the payload", s.Name, ev)
		}
		if apierror.CodeOf(err) != apierror.CodeWebhookSignatureInvalid {
			t.Fatalf("%s: a tampered body produced code %s, want %s", s.Name, apierror.CodeOf(err), apierror.CodeWebhookSignatureInvalid)
		}
	})

	t.Run("StaleTimestampFails", func(t *testing.T) {
		stale := WebhookNow.Add(-2 * time.Hour)
		body, headers := f.Build(f.Secret, stale)
		_, err := newVerifier(t, true).Verify(context.Background(), body, headers, secrets, WebhookNow)
		if err == nil {
			t.Fatalf("%s: a two-hour-old webhook was accepted; a captured request stays replayable indefinitely", s.Name)
		}
		if apierror.CodeOf(err) != apierror.CodeWebhookReplayDetected {
			t.Fatalf("%s: a stale webhook produced code %s, want %s", s.Name, apierror.CodeOf(err), apierror.CodeWebhookReplayDetected)
		}
	})

	t.Run("RotatedSecretPasses", func(t *testing.T) {
		body, headers := f.Build(f.RotatedSecret, WebhookNow)
		_, err := newVerifier(t, true).Verify(context.Background(), body, headers, secrets, WebhookNow)
		if err != nil {
			t.Fatalf("%s: an event signed with the second live secret was rejected (%v); a secret rotation would silently "+
				"drop every event signed with the other key for the whole overlap window", s.Name, err)
		}
	})

	t.Run("UnknownSecretFails", func(t *testing.T) {
		body, headers := f.Build(f.UnknownSecret, WebhookNow)
		_, err := newVerifier(t, false).Verify(context.Background(), body, headers, secrets, WebhookNow)
		if err == nil {
			t.Fatalf("%s: an event signed with an unconfigured secret was accepted", s.Name)
		}
		if apierror.CodeOf(err) != apierror.CodeWebhookSignatureInvalid {
			t.Fatalf("%s: an unknown-secret event produced code %s, want %s", s.Name,
				apierror.CodeOf(err), apierror.CodeWebhookSignatureInvalid)
		}
	})

	// The ordering property, proved by the difference between two failures rather than by
	// inspection. A body that is valid JSON with a bad signature must fail *as a signature failure*;
	// a body that is correctly signed but unparseable must fail as something else. If verification
	// happened after parsing, the first case would report a parse error for some inputs and the
	// distinction would collapse.
	t.Run("VerificationPrecedesParsing", func(t *testing.T) {
		validJSONBadSig, headers := f.Build(f.UnknownSecret, WebhookNow)
		_, sigErr := newVerifier(t, false).Verify(context.Background(), validJSONBadSig, headers, secrets, WebhookNow)
		if sigErr == nil {
			t.Fatalf("%s: valid JSON with an invalid signature was accepted", s.Name)
		}
		if apierror.CodeOf(sigErr) != apierror.CodeWebhookSignatureInvalid {
			t.Fatalf("%s: valid JSON with an invalid signature produced code %s, want %s; the signature is not being "+
				"checked before the body is trusted", s.Name, apierror.CodeOf(sigErr), apierror.CodeWebhookSignatureInvalid)
		}

		if f.BuildInvalidJSON == nil {
			t.Fatalf("%s: the subject supplies no invalid-JSON fixture, so the ordering cannot be proved", s.Name)
		}
		badBody, badHeaders := f.BuildInvalidJSON(f.Secret, WebhookNow)
		_, parseErr := newVerifier(t, true).Verify(context.Background(), badBody, badHeaders, secrets, WebhookNow)
		if parseErr == nil {
			t.Fatalf("%s: an unparseable body was accepted", s.Name)
		}
		if apierror.CodeOf(parseErr) == apierror.CodeWebhookSignatureInvalid {
			t.Fatalf("%s: a correctly signed but unparseable body was reported as a signature failure; the two conditions "+
				"are being conflated, which hides whichever one is actually happening in production", s.Name)
		}
	})
}

// assertWebhookEventIDStable proves the deduplication key is a function of the payload.
//
// Gateways redeliver. Stripe retries for three days, Adyen retries until acknowledged with
// `[accepted]`, PayPal retries for three days. If the event id moves between deliveries — because
// it was synthesised from the receipt time, say — deduplication does nothing and the platform
// applies the same capture twice.
func assertWebhookEventIDStable(t *testing.T, s Subject) {
	f := s.Webhook
	body, headers := f.Build(f.Secret, WebhookNow)
	secrets := []string{f.Secret, f.RotatedSecret}

	newVerifier := func() spi.WebhookVerifier {
		var d spi.HTTPDoer
		if f.VerifierDoer != nil {
			d = f.VerifierDoer(true)
		} else {
			d = httpx.NewRecordingDoer()
		}
		v, err := s.NewVerifier(d)
		if err != nil {
			t.Fatalf("%s: NewVerifier: %v", s.Name, err)
		}
		return v
	}

	first, err := newVerifier().Verify(context.Background(), body, headers, secrets, WebhookNow)
	if err != nil {
		t.Fatalf("%s: first verification failed: %v", s.Name, err)
	}
	// A different verifier instance and a later "now" inside the tolerance window: a stable id must
	// not depend on either.
	second, err := newVerifier().Verify(context.Background(), body, headers, secrets, WebhookNow.Add(30*time.Second))
	if err != nil {
		t.Fatalf("%s: second verification failed: %v", s.Name, err)
	}
	if first.GatewayEventID != second.GatewayEventID {
		t.Fatalf("%s: the same payload produced event ids %q and %q; deduplication cannot suppress a redelivery, "+
			"so a redelivered capture is applied twice", s.Name, first.GatewayEventID, second.GatewayEventID)
	}
	if first.GatewayEventID == "" {
		t.Fatalf("%s: the verified event carries an empty deduplication key", s.Name)
	}
}
