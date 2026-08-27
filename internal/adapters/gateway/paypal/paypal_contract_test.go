package paypal_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/contract"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/paypal"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// PayPal's fixtures carry three shapes the other two vendors do not have, and each is a place an
// adapter goes wrong:
//
//   - Every script needs an OAuth token exchange first. An adapter with no token cache would issue
//     one per call, which the request count in the contract suite would notice.
//   - A declined card is HTTP 422 with `issue: INSTRUMENT_DECLINED`, or a 2xx order whose nested
//     capture carries `status: DECLINED` and a processor response code. Both are modelled.
//   - Amounts are decimal strings. "25.00", not 2500.

const (
	paypalBaseURL      = "https://api-m.sandbox.paypal.test"
	paypalClientID     = "AeContractSuiteClientIdentifier00000001"
	paypalClientSecret = "EFContractSuiteClientSecretValue00000002"
	paypalPartnerID    = "PARTNERCONTRACTSUITE01"
	paypalOrderID      = "gwref_contract_0001"
	paypalCaptureID    = "3CONTRACTSUITECAP001"
)

var paypalClock = shared.FixedClock{T: contract.WebhookNow}

func paypalConfig(d spi.HTTPDoer) spi.Config {
	return spi.Config{
		BaseURL:          paypalBaseURL,
		Timeout:          10 * time.Second,
		HTTPClient:       d,
		Clock:            paypalClock,
		Environment:      shared.EnvironmentSandbox,
		WebhookTolerance: 5 * time.Minute,
	}
}

func paypalCredentials() spi.Credentials {
	return spi.Credentials{
		Values: map[string]string{
			paypal.CredentialClientID:     paypalClientID,
			paypal.CredentialClientSecret: paypalClientSecret,
			paypal.CredentialPartnerID:    paypalPartnerID,
			paypal.CredentialWebhookID:    "WH-CONTRACT-PRIMARY-0001",
		},
		Version:     "v1",
		Environment: shared.EnvironmentSandbox,
	}
}

// paypalTokenExchange is the preamble every script needs. It is scripted with unlimited uses so a
// test that legitimately builds several gateways still works, and it matches only the token path so
// it cannot swallow the operation under test.
func paypalTokenExchange() httpx.Exchange {
	return httpx.Exchange{
		Match: httpx.MatchPath(paypal.TokenPath),
		Response: httpx.JSONResponse(200, `{
  "scope": "https://uri.paypal.com/services/payments/payment",
  "access_token": "A21AAContractSuiteAccessToken",
  "token_type": "Bearer",
  "app_id": "APP-CONTRACT-0001",
  "expires_in": 32400,
  "nonce": "2026-08-26T12:00:00Znonce"
}`),
	}
}

func paypalOrder(status string, currency, value string, key string, nested string) string {
	payments := ""
	if nested != "" {
		payments = fmt.Sprintf(`, "payments": {%s}`, nested)
	}
	return fmt.Sprintf(`{
  "id": %q,
  "status": %q,
  "intent": "CAPTURE",
  "create_time": "2026-08-26T11:59:00Z",
  "update_time": "2026-08-26T11:59:01Z",
  "purchase_units": [
    {
      "reference_id": %q,
      "custom_id": %q,
      "invoice_id": %q,
      "amount": {"currency_code": %q, "value": %q}%s
    }
  ],
  "links": [
    {"href": "https://api-m.sandbox.paypal.test/v2/checkout/orders/%s", "rel": "self", "method": "GET"},
    {"href": "https://www.sandbox.paypal.test/checkoutnow?token=%s", "rel": "payer-action", "method": "GET"}
  ]
}`, paypalOrderID, status, contract.SuitePaymentID, contract.SuiteAttemptID, key,
		currency, value, payments, paypalOrderID, paypalOrderID)
}

func paypalCapture(status, currency, value, responseCode string) string {
	return fmt.Sprintf(`"captures": [
        {
          "id": %q,
          "status": %q,
          "amount": {"currency_code": %q, "value": %q},
          "final_capture": true,
          "invoice_id": %q,
          "create_time": "2026-08-26T11:59:01Z",
          "processor_response": {"avs_code": "Y", "cvv_code": "M", "response_code": %q},
          "seller_receivable_breakdown": {
            "gross_amount": {"currency_code": %q, "value": %q},
            "paypal_fee": {"currency_code": %q, "value": "1.05"},
            "net_amount": {"currency_code": %q, "value": "23.95"}
          }
        }
      ]`, paypalCaptureID, status, currency, value, contract.SuiteIdempotencyKey, responseCode,
		currency, value, currency, currency)
}

func paypalResponses() contract.Responses {
	return contract.Responses{
		AuthorizeApproved: func(ref string, amount money.Money, key string) *spi.HTTPResponse {
			return httpx.JSONResponse(201, paypalOrder("COMPLETED", "USD", "25.00", key,
				paypalCapture("COMPLETED", "USD", "25.00", "0000")))
		},
		// A wallet-funded refusal: HTTP 422 with an issue code and no processor beneath it.
		// PAYER_ACCOUNT_RESTRICTED is hard — no other acquirer will change the payer's account state.
		AuthorizeHardDecline: func(ref string) *spi.HTTPResponse {
			return httpx.JSONResponse(422, `{
  "name": "UNPROCESSABLE_ENTITY",
  "message": "The requested action could not be performed, semantically incorrect, or failed business validation.",
  "debug_id": "b8c3fdeae1e9a",
  "details": [
    {"issue": "PAYER_ACCOUNT_RESTRICTED", "description": "The payer account cannot be used for this transaction."}
  ],
  "links": []
}`)
		},
		// A card-funded refusal: a 2xx order whose nested capture is DECLINED with an acquirer
		// response code. 0500 is "do not honour", an issuer's discretionary refusal that another
		// acquirer can legitimately re-present.
		AuthorizeSoftDecline: func(ref string) *spi.HTTPResponse {
			return httpx.JSONResponse(201, paypalOrder("CREATED", "USD", "25.00", contract.SuiteIdempotencyKey,
				paypalCapture("DECLINED", "USD", "25.00", "0500")))
		},
		AuthorizeUnmappedDecline: func(ref string) *spi.HTTPResponse {
			return httpx.JSONResponse(201, paypalOrder("CREATED", "USD", "25.00", contract.SuiteIdempotencyKey,
				paypalCapture("DECLINED", "USD", "25.00", "ZZ99")))
		},
		AuthorizeAmountMismatch: func(ref string, requested money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(201, paypalOrder("COMPLETED", "EUR", "99.00", contract.SuiteIdempotencyKey, ""))
		},
		CaptureAccepted: func(ref string, amount money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(201, paypalOrder("COMPLETED", "USD", "25.00", contract.SuiteIdempotencyKey,
				paypalCapture("COMPLETED", "USD", "25.00", "0000")))
		},
		RefundAccepted: func(ref string, amount money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(201, fmt.Sprintf(`{
  "id": "1CONTRACTSUITEREF001",
  "status": "COMPLETED",
  "amount": {"currency_code": "USD", "value": "25.00"},
  "invoice_id": %q,
  "custom_id": %q,
  "create_time": "2026-08-26T11:59:02Z"
}`, contract.SuiteIdempotencyKey, contract.SuiteRefundID))
		},
		// PayPal answers a void with 204 and no body. An adapter that insists on a body reports every
		// successful void as a contract violation.
		VoidAccepted: func(ref string) *spi.HTTPResponse {
			return &spi.HTTPResponse{StatusCode: 204, Headers: map[string]string{}, Body: nil}
		},
		LookupByRef: func(ref, key string, amount money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(200, paypalOrder("COMPLETED", "USD", "25.00", key,
				paypalCapture("COMPLETED", "USD", "25.00", "0000")))
		},
		// The key-only lookup searches on `invoice_id`, which Authorize set. That the key is
		// discoverable from PayPal's side is what makes an unknown outcome resolvable here at all:
		// PayPal's PayPal-Request-Id replay window is far shorter than Stripe's or Adyen's.
		LookupByKey: func(ref, key string, amount money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(200, fmt.Sprintf(`{"orders":[%s]}`,
				paypalOrder("COMPLETED", "USD", "25.00", key,
					paypalCapture("COMPLETED", "USD", "25.00", "0000"))))
		},
		LookupNotFound: func() *spi.HTTPResponse {
			return httpx.JSONResponse(200, `{"orders":[]}`)
		},
		AuthFailure: func() *spi.HTTPResponse {
			return httpx.JSONResponse(401, `{
  "error": "invalid_client",
  "error_description": "Client Authentication failed",
  "name": "NOT_AUTHORIZED",
  "message": "Authorization failed due to insufficient permissions.",
  "debug_id": "c9d4feabf2f0b"
}`)
		},
		Provisioned: func(accountID string) *spi.HTTPResponse {
			return httpx.JSONResponse(201, `{
  "links": [
    {
      "href": "https://api-m.sandbox.paypal.test/v2/customer/partner-referrals/ZjcyODU4ZTUtY2M5Ny00",
      "rel": "self",
      "method": "GET",
      "description": "Read Referral Data shared by the Caller."
    },
    {
      "href": "https://www.sandbox.paypal.test/merchantsignup/partner/onboardingentry?token=CONTRACTSUITE",
      "rel": "action_url",
      "method": "GET",
      "description": "Target Web Redirect URL"
    }
  ]
}`)
		},
		DeprovisionMissing: func() *spi.HTTPResponse {
			return httpx.JSONResponse(404, `{
  "name": "RESOURCE_NOT_FOUND",
  "message": "The specified resource does not exist.",
  "debug_id": "d0e5affbc3f1c",
  "details": [{"issue": "INVALID_RESOURCE_ID", "description": "Specified resource ID does not exist."}]
}`)
		},
		DeprovisionOK: func(accountID string) *spi.HTTPResponse {
			return httpx.JSONResponse(200, `{
  "merchant_id": "CONTRACTMERCHANT01",
  "tracking_id": "mer_01JBCONTRACT0000000000001",
  "payments_receivable": true,
  "primary_email_confirmed": true,
  "products": [{"name": "PPCP", "vetting_status": "SUBSCRIBED", "capabilities": ["PAYMENT", "REFUND"]}],
  "capabilities": [{"name": "PAYMENT", "status": "ACTIVE"}],
  "oauth_integrations": [{"integration_type": "OAUTH_THIRD_PARTY"}]
}`)
		},
	}
}

const paypalEventBody = `{"id":"WH-CONTRACTSUITE-EVENT-0001","event_version":"1.0","create_time":"2026-08-26T12:00:00Z","resource_type":"capture","event_type":"PAYMENT.CAPTURE.COMPLETED","summary":"Payment completed for USD 25.0 USD","resource":{"id":"3CONTRACTSUITECAP001","status":"COMPLETED","amount":{"currency_code":"USD","value":"25.00"},"invoice_id":"ppcontract0000000000000000000001","custom_id":"att_01JBCONTRACT0000000000001","create_time":"2026-08-26T11:59:01Z"},"links":[]}`

// paypalWebhookHeaders builds the PayPal-Transmission-* set. The signature value itself is opaque
// here because PayPal verifies it server-side; which way the verification goes is controlled by the
// scripted verify response, not by the header content.
func paypalWebhookHeaders(at time.Time, transmissionID string) map[string]string {
	return map[string]string{
		paypal.HeaderTransmissionID:   transmissionID,
		paypal.HeaderTransmissionTime: at.UTC().Format(time.RFC3339),
		paypal.HeaderTransmissionSig:  "thisIsAnOpaqueBase64SignatureFromPayPal==",
		paypal.HeaderCertURL:          "https://api.sandbox.paypal.com/v1/notifications/certs/CERT-360caa42",
		paypal.HeaderAuthAlgo:         "SHA256withRSA",
	}
}

func paypalWebhookFixture() contract.WebhookFixture {
	return contract.WebhookFixture{
		Secret:        "WH-CONTRACT-PRIMARY-0001",
		RotatedSecret: "WH-CONTRACT-ROTATED-0002",
		UnknownSecret: "WH-CONTRACT-UNKNOWN-0003",
		Build: func(secret string, at time.Time) ([]byte, map[string]string) {
			return []byte(paypalEventBody), paypalWebhookHeaders(at, "contract-"+secret)
		},
		BuildInvalidJSON: func(secret string, at time.Time) ([]byte, map[string]string) {
			return []byte(`{"id":"WH-CONTRACTSUITE-EVENT-0001","event_type":`), paypalWebhookHeaders(at, "contract-"+secret)
		},
		Tamper: func(body []byte) []byte {
			return []byte(strings.Replace(string(body), `"value":"25.00"`, `"value":"9900.00"`, 1))
		},
		// PayPal's verification is a server-side call, so the verifier needs a scripted transport.
		// This is also where the certificate-chain alternative would remove a network round trip from
		// the ingress path — see the Verifier doc comment for why it is documented rather than
		// implemented.
		VerifierDoer: func(accept bool) spi.HTTPDoer {
			status := "FAILURE"
			if accept {
				status = "SUCCESS"
			}
			return httpx.NewRecordingDoer(
				paypalTokenExchange(),
				httpx.Exchange{
					Match:    httpx.MatchPath(paypal.VerifyPath),
					Response: httpx.JSONResponse(200, fmt.Sprintf(`{"verification_status":%q}`, status)),
				},
			)
		},
	}
}

func paypalSubject() contract.Subject {
	creds := paypalCredentials()
	return contract.Subject{
		Name:        "paypal",
		GatewayID:   paypal.GatewayID,
		Credentials: creds,
		NewGateway: func(d spi.HTTPDoer) (spi.PaymentGateway, error) {
			return paypal.NewGateway(paypalConfig(d))
		},
		NewProvisioner: func(d spi.HTTPDoer) (spi.GatewayProvisioner, error) {
			p, err := paypal.NewProvisioner(paypalConfig(d))
			if err != nil {
				return nil, err
			}
			return p.WithCredentials(creds), nil
		},
		NewVerifier: func(d spi.HTTPDoer) (spi.WebhookVerifier, error) {
			v, err := paypal.NewVerifier(paypalConfig(d))
			if err != nil {
				return nil, err
			}
			// PayPal's verification endpoint is itself authenticated, which is the one place a
			// verifier legitimately needs a credential.
			return v.WithCredentials(creds), nil
		},
		Responses: paypalResponses(),
		Webhook:   paypalWebhookFixture(),
		IdempotencyKeyOf: func(r httpx.RecordedRequest) string {
			return r.Header(paypal.RequestIDHeader)
		},
		Preamble:     func() []httpx.Exchange { return []httpx.Exchange{paypalTokenExchange()} },
		SupportsVoid: true,
	}
}

// TestPayPalContract runs the shared conformance suite against the PayPal adapter.
func TestPayPalContract(t *testing.T) {
	contract.RunSuite(t, paypalSubject())
}

// TestPayPalTokenIsCachedAcrossCalls proves the OAuth token is exchanged once, not per payment.
//
// Without the cache a pod doing a hundred authorizations a second issues a hundred token exchanges
// a second against an endpoint PayPal rate-limits far harder than the payment endpoints — and the
// resulting 429s look like a payments outage rather than an auth one.
func TestPayPalTokenIsCachedAcrossCalls(t *testing.T) {
	s := paypalSubject()
	d := httpx.NewRecordingDoer(
		paypalTokenExchange(),
		httpx.Exchange{Response: s.Responses.AuthorizeApproved(contract.SuiteGatewayRef, contract.SuiteAmount, contract.SuiteIdempotencyKey)},
	)
	g, err := paypal.NewGateway(paypalConfig(d))
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := g.Authorize(t.Context(), paypalAuthorizeRequest(s)); err != nil {
			t.Fatalf("Authorize %d: %v", i, err)
		}
	}
	tokenCalls := d.CountMatching(func(r httpx.RecordedRequest) bool {
		return strings.Contains(r.URL, paypal.TokenPath)
	})
	if tokenCalls != 1 {
		t.Fatalf("the adapter exchanged %d tokens for 5 payments, want 1; an uncached token turns every payment into two "+
			"round trips and rate-limits the auth endpoint", tokenCalls)
	}
}

// TestPayPalTokenExchangeIsSingleFlight proves a burst of concurrent calls produces one exchange.
//
// This is the cold-start case: a pod comes up under load and every in-flight authorization needs a
// token at the same instant. Without a single-flight guard they all stampede the token endpoint,
// and PayPal answers most of them with 429.
func TestPayPalTokenExchangeIsSingleFlight(t *testing.T) {
	s := paypalSubject()
	d := httpx.NewRecordingDoer(
		httpx.Exchange{Match: httpx.MatchPath(paypal.TokenPath), Response: httpx.JSONResponse(200,
			`{"access_token":"A21AABurstToken","token_type":"Bearer","expires_in":32400}`), Latency: 5 * time.Millisecond},
		httpx.Exchange{Response: s.Responses.AuthorizeApproved(contract.SuiteGatewayRef, contract.SuiteAmount, contract.SuiteIdempotencyKey)},
	)
	g, err := paypal.NewGateway(paypalConfig(d))
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	const burst = 16
	done := make(chan error, burst)
	for i := 0; i < burst; i++ {
		go func() {
			_, err := g.Authorize(t.Context(), paypalAuthorizeRequest(s))
			done <- err
		}()
	}
	for i := 0; i < burst; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent Authorize: %v", err)
		}
	}
	tokenCalls := d.CountMatching(func(r httpx.RecordedRequest) bool {
		return strings.Contains(r.URL, paypal.TokenPath)
	})
	if tokenCalls != 1 {
		t.Fatalf("a burst of %d concurrent payments produced %d token exchanges, want 1", burst, tokenCalls)
	}
}

// TestPayPalProvisionRequiresMerchantAction pins the behaviour spi.ProvisionResult.RequiresAction
// exists for: PayPal onboarding cannot be completed by the platform alone, and reporting it as done
// would let the router select a gateway that cannot yet accept a payment.
func TestPayPalProvisionRequiresMerchantAction(t *testing.T) {
	s := paypalSubject()
	d := httpx.NewRecordingDoer(paypalTokenExchange(),
		httpx.Exchange{Response: s.Responses.Provisioned(contract.SuiteAccountID)})
	p, err := paypal.NewProvisioner(paypalConfig(d))
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	res, err := p.WithCredentials(s.Credentials).Provision(t.Context(), spi.ProvisionRequest{
		IdempotencyKey: contract.SuiteIdempotencyKey,
		Credentials:    s.Credentials,
		MerchantID:     shared.MerchantID("mer_01JBCONTRACT0000000000001"),
		LegalName:      "Contract Suite Ltd",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !res.RequiresAction {
		t.Fatal("PayPal provisioning reported no required action; the merchant's hosted consent is not optional")
	}
	if res.ActionURL == "" {
		t.Fatal("PayPal provisioning reported a required action with no URL to send the merchant to")
	}
	if !strings.Contains(res.ActionURL, "merchantsignup") {
		t.Fatalf("the action URL %q is not PayPal's hosted onboarding entry point", res.ActionURL)
	}
}

func paypalAuthorizeRequest(s contract.Subject) spi.AuthorizeRequest {
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
