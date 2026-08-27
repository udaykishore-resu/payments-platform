package adyen_test

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/adyen"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/contract"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// The fixtures below are Adyen's real response shapes. The two that matter most are the ones that
// look like successes: a refusal is HTTP 200 with `resultCode: Refused`, and every modification is
// HTTP 200 with `status: received`. An adapter that branches on the HTTP status passes nothing here.

const (
	adyenBaseURL         = "https://checkout-test.adyen.test"
	adyenAPIKey          = "AQEyhmfxKO7NaBFDw0m/n3Q5qf3VaY9UCJ1+XWZe1UsdWZgJRnRcontract"
	adyenMerchantAccount = "ContractSuiteECOM"
	// A merchant reference containing colons and a backslash. This is the fixture that catches the
	// escaping bug in Adyen's signed projection: without escaping, these characters shift every
	// following field one position and the HMAC no longer matches — or, worse, two different events
	// produce the same signed string.
	adyenMerchantReference = `order:2026:08\26`
	// Adyen's HMAC keys are hex and must be decoded before use. Using the hex string as key material
	// produces a verifier that happily verifies its own signatures and rejects every real one.
	adyenPrimaryHMAC = "0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF"
	adyenRotatedHMAC = "FEDCBA9876543210FEDCBA9876543210FEDCBA9876543210FEDCBA9876543210"
	adyenUnknownHMAC = "AAAAAAAABBBBBBBBCCCCCCCCDDDDDDDDEEEEEEEEFFFFFFFF0000000011111111"
)

var adyenClock = shared.FixedClock{T: contract.WebhookNow}

func adyenConfig(d spi.HTTPDoer) spi.Config {
	return spi.Config{
		BaseURL:          adyenBaseURL,
		APIVersion:       adyen.DefaultAPIVersion,
		Timeout:          10 * time.Second,
		HTTPClient:       d,
		Clock:            adyenClock,
		Environment:      shared.EnvironmentSandbox,
		WebhookTolerance: 5 * time.Minute,
	}
}

func adyenCredentials() spi.Credentials {
	return spi.Credentials{
		Values: map[string]string{
			adyen.CredentialAPIKey:            adyenAPIKey,
			adyen.CredentialMerchantAccount:   adyenMerchantAccount,
			adyen.CredentialBasicAuthUser:     "pp-ingress-contract",
			adyen.CredentialBasicAuthPassword: "pp-ingress-password-contract",
		},
		Version:     "v1",
		Environment: shared.EnvironmentSandbox,
	}
}

func adyenPayment(ref, resultCode string, value int64, currency string, key string) string {
	return fmt.Sprintf(`{
  "pspReference": %q,
  "resultCode": %q,
  "amount": {"value": %d, "currency": %q},
  "merchantReference": %q,
  "paymentMethod": {"brand": "visa", "type": "scheme"},
  "metadata": {"pp_idempotency_key": %q, "pp_payment_id": %q},
  "additionalData": {
    "authCode": "012345",
    "avsResult": "4 AVS not supported for this card type",
    "cvcResult": "1 Matches",
    "paymentMethod": "visa",
    "networkTxReference": "858435661859902",
    "recurringProcessingModel": "CardOnFile"
  }
}`, ref, resultCode, value, currency, adyenMerchantReference, key, contract.SuitePaymentID)
}

func adyenRefusal(ref, code, reason string, extraAdditional string) string {
	return fmt.Sprintf(`{
  "pspReference": %q,
  "resultCode": "Refused",
  "refusalReason": %q,
  "refusalReasonCode": %q,
  "merchantReference": %q,
  "amount": {"value": 2500, "currency": "USD"},
  "additionalData": {"refusalReasonRaw": "05 : Do not honour"%s}
}`, ref, reason, code, adyenMerchantReference, extraAdditional)
}

func adyenModification(ref, paymentRef, status string, value int64, currency string) string {
	amountField := ""
	if value > 0 {
		amountField = fmt.Sprintf(`"amount": {"value": %d, "currency": %q},`, value, currency)
	}
	return fmt.Sprintf(`{
  "pspReference": %q,
  "paymentPspReference": %q,
  %s
  "merchantAccount": %q,
  "reference": %q,
  "status": %q
}`, ref, paymentRef, amountField, adyenMerchantAccount, contract.SuitePaymentID, status)
}

func adyenResponses() contract.Responses {
	return contract.Responses{
		AuthorizeApproved: func(ref string, amount money.Money, key string) *spi.HTTPResponse {
			return httpx.JSONResponse(200, adyenPayment(ref, "Authorised", amount.Amount(), string(amount.Currency()), key))
		},
		// 5 = Blocked Card. Hard, and in Adyen's do-not-retry set.
		AuthorizeHardDecline: func(ref string) *spi.HTTPResponse {
			return httpx.JSONResponse(200, adyenRefusal(ref, "5", "Blocked Card", ""))
		},
		// 9 = Issuer Unavailable. The issuer could not be reached, so another acquirer may succeed —
		// one of the few genuinely soft refusals.
		AuthorizeSoftDecline: func(ref string) *spi.HTTPResponse {
			return httpx.JSONResponse(200, adyenRefusal(ref, "9", "Issuer Unavailable", ""))
		},
		AuthorizeUnmappedDecline: func(ref string) *spi.HTTPResponse {
			return httpx.JSONResponse(200, adyenRefusal(ref, "998", "A refusal reason Adyen has not shipped yet", ""))
		},
		AuthorizeAmountMismatch: func(ref string, requested money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(200, adyenPayment(ref, "Authorised",
				requested.Amount()+7400, "EUR", contract.SuiteIdempotencyKey))
		},
		CaptureAccepted: func(ref string, amount money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(201, adyenModification(ref+"_capture", ref, "received",
				amount.Amount(), string(amount.Currency())))
		},
		RefundAccepted: func(ref string, amount money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(201, adyenModification(ref+"_refund", ref, "received",
				amount.Amount(), string(amount.Currency())))
		},
		VoidAccepted: func(ref string) *spi.HTTPResponse {
			return httpx.JSONResponse(201, adyenModification(ref+"_cancel", ref, "received", 0, ""))
		},
		LookupByRef: func(ref, key string, amount money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(200, adyenPayment(ref, "Authorised", amount.Amount(), string(amount.Currency()), key))
		},
		LookupByKey: func(ref, key string, amount money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(200, fmt.Sprintf(`{"data":[%s]}`,
				adyenPayment(ref, "Authorised", amount.Amount(), string(amount.Currency()), key)))
		},
		LookupNotFound: func() *spi.HTTPResponse {
			return httpx.JSONResponse(200, `{"data":[]}`)
		},
		AuthFailure: func() *spi.HTTPResponse {
			return httpx.JSONResponse(401, `{
  "status": 401,
  "errorCode": "000",
  "message": "HTTP Status Response - Unauthorized",
  "errorType": "security"
}`)
		},
		// Provisioning walks legalEntities → businessLines → accountHolders. One fixture serves all
		// three because each response carries an `id`, and the account holder's is what becomes the
		// external account id.
		Provisioned: func(accountID string) *spi.HTTPResponse {
			return httpx.JSONResponse(200, fmt.Sprintf(`{
  "id": %q,
  "type": "organization",
  "reference": "mer_01JBCONTRACT0000000000001",
  "status": "active",
  "legalEntityId": "LE00000000000000000000CONTRACT",
  "service": "paymentProcessing",
  "capabilities": {
    "receivePayments": {"allowed": true, "requested": true, "verificationStatus": "valid"},
    "sendToTransferInstrument": {"allowed": true, "requested": true, "verificationStatus": "valid"}
  },
  "problems": []
}`, accountID))
		},
		DeprovisionMissing: func() *spi.HTTPResponse {
			return httpx.JSONResponse(404, `{
  "status": 404,
  "errorCode": "30_012",
  "message": "Account holder not found",
  "errorType": "validation"
}`)
		},
		DeprovisionOK: func(accountID string) *spi.HTTPResponse {
			return httpx.JSONResponse(200, fmt.Sprintf(`{"id":%q,"status":"closed","legalEntityId":"LE00000000000000000000CONTRACT"}`, accountID))
		},
	}
}

// adyenNotification builds a signed notification. It signs through adyen.Sign — the adapter's own
// signer — so a fixture cannot agree with a wrong idea of the scheme and hide a verifier bug.
func adyenNotification(hmacKey string, at time.Time, value int64) []byte {
	const psp = contract.SuiteGatewayRef
	signed := adyen.SignedPayload(psp, "", adyenMerchantAccount, adyenMerchantReference,
		fmt.Sprintf("%d", value), "USD", "AUTHORISATION", "true")
	sig, err := adyen.Sign(signed, hmacKey)
	if err != nil {
		panic(err)
	}
	return []byte(fmt.Sprintf(`{
  "live": "false",
  "notificationItems": [
    {
      "NotificationRequestItem": {
        "pspReference": %q,
        "originalReference": "",
        "merchantAccountCode": %q,
        "merchantReference": %q,
        "amount": {"value": %d, "currency": "USD"},
        "eventCode": "AUTHORISATION",
        "eventDate": %q,
        "success": "true",
        "paymentMethod": "visa",
        "reason": "012345:4242:08/2030",
        "additionalData": {
          "hmacSignature": %q,
          "pp_idempotency_key": %q,
          "pp_payment_id": %q
        }
      }
    }
  ]
}`, psp, adyenMerchantAccount, adyenMerchantReference, value,
		at.UTC().Format(time.RFC3339), sig, contract.SuiteIdempotencyKey, contract.SuitePaymentID))
}

func adyenWebhookFixture() contract.WebhookFixture {
	return contract.WebhookFixture{
		Secret:        adyenPrimaryHMAC,
		RotatedSecret: adyenRotatedHMAC,
		UnknownSecret: adyenUnknownHMAC,
		Build: func(secret string, at time.Time) ([]byte, map[string]string) {
			return adyenNotification(secret, at, 2500), map[string]string{}
		},
		BuildInvalidJSON: func(secret string, at time.Time) ([]byte, map[string]string) {
			// Adyen's signature lives *inside* the body, so an unparseable body cannot even be
			// authenticated. The adapter must report that as a parse failure, not as a signature
			// failure — otherwise a malformed delivery and a forged one look identical in the logs.
			return []byte(`{"live":"false","notificationItems":[{"NotificationRequestItem":{`), map[string]string{}
		},
		// Adyen signs a projection of eight fields, not the body, so appending whitespace would not
		// invalidate anything. The tamper an attacker would actually attempt is changing the amount,
		// which is inside the signed set.
		Tamper: func(body []byte) []byte {
			return bytes.Replace(body, []byte(`"value": 2500`), []byte(`"value": 990000`), 1)
		},
	}
}

func adyenSubject() contract.Subject {
	creds := adyenCredentials()
	return contract.Subject{
		Name:        "adyen",
		GatewayID:   adyen.GatewayID,
		Credentials: creds,
		NewGateway: func(d spi.HTTPDoer) (spi.PaymentGateway, error) {
			return adyen.NewGateway(adyenConfig(d))
		},
		NewProvisioner: func(d spi.HTTPDoer) (spi.GatewayProvisioner, error) {
			p, err := adyen.NewProvisioner(adyenConfig(d))
			if err != nil {
				return nil, err
			}
			return p.WithCredentials(creds), nil
		},
		NewVerifier: func(d spi.HTTPDoer) (spi.WebhookVerifier, error) {
			return adyen.NewVerifier(adyenConfig(d))
		},
		Responses: adyenResponses(),
		Webhook:   adyenWebhookFixture(),
		IdempotencyKeyOf: func(r httpx.RecordedRequest) string {
			return r.Header("Idempotency-Key")
		},
		SupportsVoid: true,
	}
}

// TestAdyenContract runs the shared conformance suite against the Adyen adapter.
func TestAdyenContract(t *testing.T) {
	contract.RunSuite(t, adyenSubject())
}

// TestAdyenSignedPayloadEscaping pins the escaping rule directly, because it is the one place where
// a wrong implementation passes every round-trip test written against itself.
//
// The two cases below are the ones that matter: a colon inside a value must not be readable as a
// field separator, and a backslash must not escape the character after it. Without escaping,
// ("a:b", "c") and ("a", "b:c") produce the identical signed string — a signature collision, not
// merely an availability bug.
func TestAdyenSignedPayloadEscaping(t *testing.T) {
	tests := []struct {
		name             string
		a, b             string
		wantDistinctFrom []string
	}{
		{name: "colon in a value", a: "a:b", b: "c"},
		{name: "backslash in a value", a: `a\`, b: "b"},
		{name: "both", a: `x\:y`, b: "z"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			left := adyen.SignedPayload(tc.a, tc.b, "M", "R", "1", "USD", "AUTHORISATION", "true")
			right := adyen.SignedPayload(tc.b, tc.a, "M", "R", "1", "USD", "AUTHORISATION", "true")
			if left == right {
				t.Fatalf("swapping the field values produced the identical signed payload %q; the escaping rule is not being applied, "+
					"which makes two different events share one signature", left)
			}
		})
	}

	// The exact expected string for a known input, so a future "simplification" of the escaper has
	// something concrete to fail against.
	got := adyen.SignedPayload("psp:1", `orig\2`, "MERCH", "ref", "2500", "USD", "AUTHORISATION", "true")
	want := `psp\:1:orig\\2:MERCH:ref:2500:USD:AUTHORISATION:true`
	if got != want {
		t.Fatalf("SignedPayload = %q, want %q", got, want)
	}
}

// TestAdyenAcknowledgementBody pins the literal Adyen requires. Any other body — including an empty
// one with a 200 — is treated as a failed delivery, and Adyen redelivers on an escalating schedule
// for days before disabling the endpoint.
func TestAdyenAcknowledgementBody(t *testing.T) {
	v, err := adyen.NewVerifier(adyenConfig(httpx.NewRecordingDoer()))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if got := string(v.AcknowledgementBody()); got != "[accepted]" {
		t.Fatalf("AcknowledgementBody() = %q, want %q", got, "[accepted]")
	}
}

// TestAdyenBasicAuthOnNotificationEndpoint covers the transport-level control that runs before the
// HMAC: Adyen authenticates itself to us, which is what stops an unauthenticated caller making the
// ingress do HMAC work on demand.
func TestAdyenBasicAuthOnNotificationEndpoint(t *testing.T) {
	v, err := adyen.NewVerifier(adyenConfig(httpx.NewRecordingDoer()))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	const user, pass = "pp-ingress-contract", "pp-ingress-password-contract"
	good := map[string]string{"Authorization": "Basic " + base64Of(user+":"+pass)}
	if err := v.VerifyBasicAuth(good, user, pass); err != nil {
		t.Fatalf("valid basic auth was rejected: %v", err)
	}
	bad := map[string]string{"Authorization": "Basic " + base64Of(user+":wrong")}
	if err := v.VerifyBasicAuth(bad, user, pass); err == nil {
		t.Fatal("a wrong password was accepted on the notification endpoint")
	}
	if err := v.VerifyBasicAuth(map[string]string{}, user, pass); err == nil {
		t.Fatal("a delivery with no Authorization header was accepted")
	}
}

func base64Of(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out []byte
	b := []byte(s)
	for i := 0; i < len(b); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], b[i:])
		v := uint32(chunk[0])<<16 | uint32(chunk[1])<<8 | uint32(chunk[2])
		out = append(out, alphabet[(v>>18)&0x3F], alphabet[(v>>12)&0x3F])
		if n > 1 {
			out = append(out, alphabet[(v>>6)&0x3F])
		} else {
			out = append(out, '=')
		}
		if n > 2 {
			out = append(out, alphabet[v&0x3F])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}
