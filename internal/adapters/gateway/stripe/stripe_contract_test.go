package stripe_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/contract"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/stripe"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// The fixtures below are Stripe's real response shapes, trimmed to the fields this platform reads
// plus enough surrounding structure that a field the adapter should be reading but is not shows up
// as a failure rather than as an absence. A fixture that omits `latest_charge` would let an adapter
// that never asks for the expansion pass, and the AVS/CVV data a dispute defence needs would be
// silently missing in production.

const (
	stripeBaseURL   = "https://api.stripe.test"
	stripeSecretKey = "sk_test_contract_suite_51ABCDEF0123456789"
	stripeChargeID  = "ch_3ContractSuite0001"
)

var stripeClock = shared.FixedClock{T: contract.WebhookNow}

func stripeConfig(d spi.HTTPDoer) spi.Config {
	return spi.Config{
		BaseURL:          stripeBaseURL,
		APIVersion:       stripe.DefaultAPIVersion,
		Timeout:          10 * time.Second,
		HTTPClient:       d,
		Clock:            stripeClock,
		Environment:      shared.EnvironmentSandbox,
		WebhookTolerance: 5 * time.Minute,
	}
}

func stripeCredentials() spi.Credentials {
	return spi.Credentials{
		Values:      map[string]string{stripe.CredentialSecretKey: stripeSecretKey},
		Version:     "v1",
		Environment: shared.EnvironmentSandbox,
	}
}

// stripeIntent renders a PaymentIntent with an expanded charge, which is what the adapter asks for.
func stripeIntent(ref, status string, amount, received int64, currency, key string) string {
	return fmt.Sprintf(`{
  "id": %q,
  "object": "payment_intent",
  "amount": %d,
  "amount_received": %d,
  "amount_capturable": 0,
  "capture_method": "automatic",
  "currency": %q,
  "status": %q,
  "created": 1787654321,
  "livemode": false,
  "metadata": {"pp_idempotency_key": %q, "pp_payment_id": %q},
  "latest_charge": {
    "id": %q,
    "object": "charge",
    "amount": %d,
    "currency": %q,
    "status": "succeeded",
    "paid": true,
    "captured": true,
    "outcome": {
      "network_status": "approved_by_network",
      "reason": null,
      "risk_level": "normal",
      "seller_message": "Payment complete.",
      "type": "authorized"
    },
    "payment_method_details": {
      "type": "card",
      "card": {
        "brand": "visa",
        "country": "US",
        "last4": "4242",
        "network": "visa",
        "network_transaction_id": "104102978678771",
        "authorization_code": "104102",
        "checks": {
          "address_line1_check": "pass",
          "address_postal_code_check": "pass",
          "cvc_check": "pass"
        },
        "three_d_secure": null
      }
    }
  }
}`, ref, amount, received, currency, status, key, contract.SuitePaymentID, stripeChargeID, amount, currency)
}

// stripeDecline renders Stripe's 402 card-decline body, which carries both the error object and the
// PaymentIntent it was raised against. An adapter that reads only the error loses the reference and
// can never reconcile the decline.
func stripeDecline(ref, declineCode string, advice string) string {
	adviceFields := ""
	if advice != "" {
		adviceFields = fmt.Sprintf(`"network_advice_code": %q, "network_decline_code": "59",`, advice)
	}
	return fmt.Sprintf(`{
  "error": {
    "type": "card_error",
    "code": "card_declined",
    "decline_code": %q,
    %s
    "message": "Your card was declined.",
    "charge": %q,
    "doc_url": "https://stripe.com/docs/error-codes/card-declined",
    "request_log_url": "https://dashboard.stripe.com/test/logs/req_x",
    "payment_intent": {
      "id": %q,
      "object": "payment_intent",
      "amount": 2500,
      "currency": "usd",
      "status": "requires_payment_method",
      "created": 1787654321,
      "last_payment_error": {
        "type": "card_error",
        "code": "card_declined",
        "decline_code": %q,
        "message": "Your card was declined."
      }
    }
  }
}`, declineCode, adviceFields, stripeChargeID, ref, declineCode)
}

func stripeResponses() contract.Responses {
	return contract.Responses{
		AuthorizeApproved: func(ref string, amount money.Money, key string) *spi.HTTPResponse {
			return httpx.JSONResponse(200, stripeIntent(ref, "succeeded", amount.Amount(), amount.Amount(), "usd", key))
		},
		// stolen_card: hard, and one of the codes the scheme mandates must never be re-presented.
		AuthorizeHardDecline: func(ref string) *spi.HTTPResponse {
			return httpx.JSONResponse(402, stripeDecline(ref, "stolen_card", "02"))
		},
		// issuer_not_available: the issuer could not be reached, so another acquirer may well
		// succeed. This is one of the very few codes that legitimately permits failover.
		AuthorizeSoftDecline: func(ref string) *spi.HTTPResponse {
			return httpx.JSONResponse(402, stripeDecline(ref, "issuer_not_available", ""))
		},
		AuthorizeUnmappedDecline: func(ref string) *spi.HTTPResponse {
			return httpx.JSONResponse(402, stripeDecline(ref, "a_decline_code_stripe_has_not_shipped_yet", ""))
		},
		AuthorizeAmountMismatch: func(ref string, requested money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(200, stripeIntent(ref, "succeeded",
				requested.Amount()+7400, requested.Amount()+7400, "eur", contract.SuiteIdempotencyKey))
		},
		CaptureAccepted: func(ref string, amount money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(200, stripeIntent(ref, "succeeded", amount.Amount(), amount.Amount(), "usd", contract.SuiteIdempotencyKey))
		},
		RefundAccepted: func(ref string, amount money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(200, fmt.Sprintf(`{
  "id": "re_3ContractSuite0001",
  "object": "refund",
  "amount": %d,
  "currency": "usd",
  "status": "succeeded",
  "reason": "requested_by_customer",
  "payment_intent": %q,
  "charge": %q,
  "created": 1787654321,
  "metadata": {"pp_idempotency_key": %q}
}`, amount.Amount(), ref, stripeChargeID, contract.SuiteIdempotencyKey))
		},
		VoidAccepted: func(ref string) *spi.HTTPResponse {
			return httpx.JSONResponse(200, stripeIntent(ref, "canceled", 2500, 0, "usd", contract.SuiteIdempotencyKey))
		},
		LookupByRef: func(ref, key string, amount money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(200, stripeIntent(ref, "succeeded", amount.Amount(), amount.Amount(), "usd", key))
		},
		// The key-only lookup answers with Stripe's list envelope; the adapter filters client-side on
		// the metadata key it stamped at authorization time.
		LookupByKey: func(ref, key string, amount money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(200, fmt.Sprintf(`{
  "object": "list",
  "url": "/v1/payment_intents",
  "has_more": false,
  "data": [%s]
}`, stripeIntent(ref, "succeeded", amount.Amount(), amount.Amount(), "usd", key)))
		},
		LookupNotFound: func() *spi.HTTPResponse {
			return httpx.JSONResponse(200, `{"object":"list","url":"/v1/payment_intents","has_more":false,"data":[]}`)
		},
		// Note that the message does not echo the key. Stripe's real message includes a masked
		// fragment; reproducing an unmasked one here would make the credential-leak assertion pass
		// against a fixture that is kinder than production.
		AuthFailure: func() *spi.HTTPResponse {
			return httpx.JSONResponse(401, `{
  "error": {
    "type": "authentication_error",
    "code": "api_key_invalid",
    "message": "Invalid API Key provided",
    "doc_url": "https://stripe.com/docs/error-codes/api-key-invalid"
  }
}`)
		},
		Provisioned: func(accountID string) *spi.HTTPResponse {
			return httpx.JSONResponse(200, fmt.Sprintf(`{
  "id": %q,
  "object": "account",
  "country": "US",
  "type": "custom",
  "charges_enabled": true,
  "payouts_enabled": true,
  "requirements": {
    "currently_due": [],
    "eventually_due": [],
    "past_due": [],
    "pending_verification": [],
    "disabled_reason": null
  }
}`, accountID))
		},
		DeprovisionMissing: func() *spi.HTTPResponse {
			return httpx.JSONResponse(404, `{
  "error": {
    "type": "invalid_request_error",
    "code": "resource_missing",
    "param": "id",
    "message": "No such account"
  }
}`)
		},
		DeprovisionOK: func(accountID string) *spi.HTTPResponse {
			return httpx.JSONResponse(200, fmt.Sprintf(`{"id":%q,"object":"account","deleted":true}`, accountID))
		},
	}
}

const stripeEventBody = `{"id":"evt_1ContractSuite0001","object":"event","api_version":"2026-06-30.acacia","created":1787654321,"type":"payment_intent.succeeded","livemode":false,"data":{"object":{"id":"gwref_contract_0001","object":"payment_intent","amount":2500,"currency":"usd","status":"succeeded","metadata":{"pp_idempotency_key":"ppcontract0000000000000000000001","pp_payment_id":"pay_01JBCONTRACT0000000000001"}}}}`

func stripeWebhookFixture() contract.WebhookFixture {
	return contract.WebhookFixture{
		Secret:        "whsec_contract_primary_0000000000001",
		RotatedSecret: "whsec_contract_rotated_0000000000002",
		UnknownSecret: "whsec_contract_unknown_0000000000003",
		Build: func(secret string, at time.Time) ([]byte, map[string]string) {
			body := []byte(stripeEventBody)
			return body, map[string]string{stripe.SignatureHeader: stripe.Sign(body, secret, at)}
		},
		// Correctly signed and unparseable: the proof that verification runs before the parser sees
		// the bytes. The signature is over exactly these bytes, so it passes; the JSON does not.
		BuildInvalidJSON: func(secret string, at time.Time) ([]byte, map[string]string) {
			body := []byte(`{"id":"evt_1ContractSuite0001","object":"event",`)
			return body, map[string]string{stripe.SignatureHeader: stripe.Sign(body, secret, at)}
		},
		// Stripe signs the raw body, so any mutation invalidates the signature. Changing the amount
		// is the mutation an attacker would actually make.
		Tamper: func(body []byte) []byte {
			return []byte(string(body[:len(body)-1]) + ` }`)
		},
	}
}

func stripeSubject() contract.Subject {
	creds := stripeCredentials()
	return contract.Subject{
		Name:        "stripe",
		GatewayID:   stripe.GatewayID,
		Credentials: creds,
		NewGateway: func(d spi.HTTPDoer) (spi.PaymentGateway, error) {
			return stripe.NewGateway(stripeConfig(d))
		},
		NewProvisioner: func(d spi.HTTPDoer) (spi.GatewayProvisioner, error) {
			p, err := stripe.NewProvisioner(stripeConfig(d))
			if err != nil {
				return nil, err
			}
			// Compensations carry no credentials in their SPI signature, so they are bound here for
			// the lifetime of this provisioner — see (*Provisioner).WithCredentials.
			return p.WithCredentials(creds), nil
		},
		NewVerifier: func(d spi.HTTPDoer) (spi.WebhookVerifier, error) {
			return stripe.NewVerifier(stripeConfig(d))
		},
		Responses: stripeResponses(),
		Webhook:   stripeWebhookFixture(),
		IdempotencyKeyOf: func(r httpx.RecordedRequest) string {
			return r.Header("Idempotency-Key")
		},
		SupportsVoid: true,
	}
}

// TestStripeContract runs the shared conformance suite against the Stripe adapter. A gateway
// integration is done when this is green; there is no other definition.
func TestStripeContract(t *testing.T) {
	contract.RunSuite(t, stripeSubject())
}

// TestStripeSendsVendorSpecificHeaders covers the three headers that are not part of the shared
// contract but whose absence is silently catastrophic: the pinned API version, the connected
// account scope, and the form content type.
func TestStripeSendsVendorSpecificHeaders(t *testing.T) {
	s := stripeSubject()
	d := httpx.NewRecordingDoer(httpx.Exchange{
		Response: s.Responses.AuthorizeApproved(contract.SuiteGatewayRef, contract.SuiteAmount, contract.SuiteIdempotencyKey),
	})
	g, err := stripe.NewGateway(stripeConfig(d))
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	if _, err := g.Authorize(t.Context(), authorizeRequestFor(s)); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	req, ok := d.Last()
	if !ok {
		t.Fatal("no request recorded")
	}
	if got := req.Header("Stripe-Version"); got != stripe.DefaultAPIVersion {
		t.Errorf("Stripe-Version = %q, want %q; an unpinned version lets a dashboard setting change response shapes under a running adapter",
			got, stripe.DefaultAPIVersion)
	}
	if got := req.Header("Stripe-Account"); got != contract.SuiteAccountID {
		t.Errorf("Stripe-Account = %q, want %q; without it the charge lands on the platform account rather than the merchant's",
			got, contract.SuiteAccountID)
	}
	if got := req.Header("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want form encoding; Stripe does not accept JSON", got)
	}
	body := req.BodyString()
	for _, want := range []string{"amount=2500", "currency=usd", "confirm=true", "capture_method=automatic"} {
		if !contains(body, want) {
			t.Errorf("request body is missing %q; body was %q", want, body)
		}
	}
	if !contains(body, "metadata%5Bpp_idempotency_key%5D") {
		t.Errorf("the idempotency key was not stamped into metadata; a lookup by key alone would fail once Stripe's 24-hour window closes")
	}
}

func authorizeRequestFor(s contract.Subject) spi.AuthorizeRequest {
	return spi.AuthorizeRequest{
		IdempotencyKey:    contract.SuiteIdempotencyKey,
		Credentials:       s.Credentials,
		ExternalAccountID: contract.SuiteAccountID,
		PaymentID:         shared.PaymentID(contract.SuitePaymentID),
		AttemptID:         shared.AttemptID(contract.SuiteAttemptID),
		Amount:            contract.SuiteAmount,
		Method:            shared.MethodCard,
		Capture:           true,
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
