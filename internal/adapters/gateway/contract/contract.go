// Package contract is the shared conformance suite every gateway adapter must pass.
//
// A gateway integration is "done" when this suite is green against it. That is not a slogan: the
// SPI's obligations are prose, and prose is what an adapter author reads once and then approximates
// under deadline pressure. Each assertion below is one of those obligations turned into something
// that fails a build.
//
// The assertions were chosen by asking, for each one, *what does it cost when this is wrong*:
//
//   - TimeoutYieldsOutcomeUnknown — getting this wrong double-charges payers. It is asserted for
//     every money-moving operation, not just authorize, because the adapter author who remembered
//     it for authorize is exactly the one who forgot it for refund.
//   - HardDeclineMapsToNonFailoverReason — getting this wrong makes the platform re-present a
//     stolen card to a second acquirer, which is indistinguishable from card testing and gets the
//     platform's gateway accounts closed.
//   - UnmappedDeclineIsUnknownAndDoesNotFailover — the default arm is the one nobody tests, and a
//     default of "retry" is how a platform card-tests on an attacker's behalf.
//   - LookupFindsByIdempotencyKeyAlone — without it an unknown outcome can only be resolved by a
//     webhook that may never arrive.
//   - CredentialsNeverAppearInErrorsOrLogs — a leaked gateway key is a PCI incident, and the leak
//     is always in an error path nobody exercised.
//   - The webhook ordering assertions — verification before parsing is the difference between a
//     hardened ingress and one where an unauthenticated caller chooses the parser's input.
//
// The suite drives adapters through httpx.RecordingDoer, so it asserts on what the adapter *sends*
// as well as on what it returns. An adapter can otherwise pass every response-shaped test while
// putting the idempotency key in the wrong header, which is a bug that only appears the first time
// a retry happens in production.
package contract

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// SuiteAmount is the amount every assertion uses unless it needs a different one.
//
// 2500 minor units is chosen so that the simulator's amount-derived trigger table resolves to
// "approve": the suite must be able to script an approval for every subject, including the one
// whose behaviour is a function of the amount.
var SuiteAmount = money.MustNew(2500, "USD")

// Fixed identifiers, so a failure message names something stable and a recorded request can be
// compared across runs.
const (
	SuiteIdempotencyKey    = "ppcontract0000000000000000000001"
	SuiteAltIdempotencyKey = "ppcontract0000000000000000000002"
	SuiteGatewayRef        = "gwref_contract_0001"
	SuitePaymentID         = "pay_01JBCONTRACT0000000000001"
	SuiteAttemptID         = "att_01JBCONTRACT0000000000001"
	SuiteRefundID          = "ref_01JBCONTRACT0000000000001"
	SuiteAccountID         = "acct_contract_0001"
)

// Responses is the vendor-shaped payload set a subject supplies.
//
// It is a struct of functions rather than an interface because an adapter's test file should be
// able to write one field inline and leave the rest to defaults where the assertion does not apply.
// Every function returns a *realistic* vendor payload — the suite is worth exactly as much as the
// fidelity of these fixtures, and a fixture that omits the field the adapter reads turns a passing
// suite into a false negative.
type Responses struct {
	// AuthorizeApproved is a successful authorization carrying ref and echoing amount.
	AuthorizeApproved func(ref string, amount money.Money, key string) *spi.HTTPResponse
	// AuthorizeHardDecline is a decline whose reason must not permit failover — a stolen card.
	AuthorizeHardDecline func(ref string) *spi.HTTPResponse
	// AuthorizeSoftDecline is a decline whose reason permits failover — an issuer's discretionary
	// refusal or an unavailable issuer.
	AuthorizeSoftDecline func(ref string) *spi.HTTPResponse
	// AuthorizeUnmappedDecline carries a vendor code the adapter's table does not contain.
	AuthorizeUnmappedDecline func(ref string) *spi.HTTPResponse
	// AuthorizeAmountMismatch echoes an amount and currency that differ from the request.
	AuthorizeAmountMismatch func(ref string, requested money.Money) *spi.HTTPResponse

	CaptureAccepted func(ref string, amount money.Money) *spi.HTTPResponse
	RefundAccepted  func(ref string, amount money.Money) *spi.HTTPResponse
	VoidAccepted    func(ref string) *spi.HTTPResponse

	// LookupByRef answers a lookup that carries a gateway reference.
	LookupByRef func(ref string, key string, amount money.Money) *spi.HTTPResponse
	// LookupByKey answers a lookup that carries only an idempotency key. For most vendors this is a
	// list envelope the adapter filters client-side.
	LookupByKey func(ref string, key string, amount money.Money) *spi.HTTPResponse
	// LookupNotFound is the gateway saying it has never heard of the transaction.
	LookupNotFound func() *spi.HTTPResponse

	// AuthFailure is the vendor's 401.
	AuthFailure func() *spi.HTTPResponse

	// Provisioned is a successful sub-merchant creation.
	Provisioned func(accountID string) *spi.HTTPResponse
	// DeprovisionMissing is the gateway saying the account does not exist, which a compensation must
	// treat as success.
	DeprovisionMissing func() *spi.HTTPResponse
	// DeprovisionOK is a successful deprovision, where the vendor answers with a body.
	DeprovisionOK func(accountID string) *spi.HTTPResponse
}

// WebhookFixture builds signed notifications for the verification assertions.
type WebhookFixture struct {
	// Secret is the primary signing secret.
	Secret string
	// RotatedSecret is a second live secret, as exists during a rotation overlap. A verifier that
	// knows only one silently drops every event signed with the other.
	RotatedSecret string
	// UnknownSecret is a secret the verifier is not configured with; events signed with it must be
	// rejected.
	UnknownSecret string

	// Build returns a valid body and headers signed with secret at the given instant.
	Build func(secret string, at time.Time) (body []byte, headers map[string]string)
	// BuildInvalidJSON returns a correctly signed body that is not parseable, which is how the suite
	// proves verification happens before parsing.
	BuildInvalidJSON func(secret string, at time.Time) (body []byte, headers map[string]string)
	// Tamper mutates an authenticated body so its signature no longer matches.
	Tamper func(body []byte) []byte

	// VerifierDoer supplies the transport a verifier needs. Most verifiers make no network call and
	// return a doer that fails loudly if one is attempted; PayPal's verification is a server-side
	// call, so its doer is scripted to accept or reject.
	VerifierDoer func(accept bool) spi.HTTPDoer
}

// Subject is one adapter under test.
type Subject struct {
	Name      string
	GatewayID shared.GatewayID

	// Credentials are handed to every request. The values are also the strings the credential-leak
	// assertion searches for, so they must be distinctive enough that an accidental match is not
	// plausible.
	Credentials spi.Credentials

	NewGateway     func(spi.HTTPDoer) (spi.PaymentGateway, error)
	NewProvisioner func(spi.HTTPDoer) (spi.GatewayProvisioner, error)
	NewVerifier    func(spi.HTTPDoer) (spi.WebhookVerifier, error)

	Responses Responses
	Webhook   WebhookFixture

	// IdempotencyKeyOf extracts the key from a recorded request in the vendor's expected form.
	// Returning "" means the adapter did not send it, which is a failure.
	IdempotencyKeyOf func(httpx.RecordedRequest) string

	// Preamble are exchanges every script needs before the operation under test — PayPal's OAuth
	// token exchange, for instance. They are prepended, so an operation-matching exchange later in
	// the script cannot swallow them.
	Preamble func() []httpx.Exchange

	// SupportsVoid lets a gateway that has no void endpoint skip the void assertions rather than
	// fail them. A gateway without void is a real and representable thing (see the capability
	// descriptor); pretending otherwise would force an adapter to fake an endpoint.
	SupportsVoid bool
}

// secretValues returns the credential strings that must never surface.
func (s Subject) secretValues() []string {
	out := make([]string, 0, len(s.Credentials.Values))
	for _, v := range s.Credentials.Values {
		if len(v) >= 8 {
			out = append(out, v)
		}
	}
	return out
}

func (s Subject) preamble() []httpx.Exchange {
	if s.Preamble == nil {
		return nil
	}
	return s.Preamble()
}

// doer builds a RecordingDoer with the subject's preamble followed by the given exchanges.
func (s Subject) doer(exchanges ...httpx.Exchange) *httpx.RecordingDoer {
	all := append(s.preamble(), exchanges...)
	return httpx.NewRecordingDoer(all...)
}

func (s Subject) gateway(t *testing.T, d spi.HTTPDoer) spi.PaymentGateway {
	t.Helper()
	g, err := s.NewGateway(d)
	if err != nil {
		t.Fatalf("%s: NewGateway: %v", s.Name, err)
	}
	if g == nil {
		t.Fatalf("%s: NewGateway returned nil with no error", s.Name)
	}
	return g
}

// --- request builders --------------------------------------------------------------------------

func (s Subject) authorizeRequest(key string) spi.AuthorizeRequest {
	return spi.AuthorizeRequest{
		IdempotencyKey:    key,
		Credentials:       s.Credentials,
		ExternalAccountID: SuiteAccountID,
		PaymentID:         shared.PaymentID(SuitePaymentID),
		AttemptID:         shared.AttemptID(SuiteAttemptID),
		Amount:            SuiteAmount,
		Method:            shared.MethodCard,
		MethodRef:         payment.PaymentMethodReference{Token: "tok_contract_suite", Brand: "visa", Last4: "4242"},
		Capture:           true,
		Reference:         "contract-suite-order",
		StatementRef:      "CONTRACT",
		Metadata:          map[string]string{"pp_contract_suite": "true"},
	}
}

func (s Subject) captureRequest(key string) spi.CaptureRequest {
	return spi.CaptureRequest{
		IdempotencyKey:    key,
		Credentials:       s.Credentials,
		ExternalAccountID: SuiteAccountID,
		PaymentID:         shared.PaymentID(SuitePaymentID),
		AttemptID:         shared.AttemptID(SuiteAttemptID),
		GatewayRef:        SuiteGatewayRef,
		Amount:            SuiteAmount,
		Final:             true,
	}
}

func (s Subject) refundRequest(key string) spi.RefundRequest {
	return spi.RefundRequest{
		IdempotencyKey:    key,
		Credentials:       s.Credentials,
		ExternalAccountID: SuiteAccountID,
		PaymentID:         shared.PaymentID(SuitePaymentID),
		RefundID:          shared.RefundID(SuiteRefundID),
		GatewayRef:        SuiteGatewayRef,
		Amount:            SuiteAmount,
		Reason:            payment.RefundReasonRequestedByCustomer,
	}
}

func (s Subject) voidRequest(key string) spi.VoidRequest {
	return spi.VoidRequest{
		IdempotencyKey:    key,
		Credentials:       s.Credentials,
		ExternalAccountID: SuiteAccountID,
		PaymentID:         shared.PaymentID(SuitePaymentID),
		GatewayRef:        SuiteGatewayRef,
	}
}

func (s Subject) lookupRequest(ref, key string) spi.LookupRequest {
	return spi.LookupRequest{
		Credentials:       s.Credentials,
		ExternalAccountID: SuiteAccountID,
		GatewayRef:        ref,
		IdempotencyKey:    key,
		Operation:         shared.OpAuthorize,
	}
}

func (s Subject) provisionRequest(key string) spi.ProvisionRequest {
	country, _ := shared.ParseCountry("US")
	mcc, _ := shared.ParseMCC("5734")
	return spi.ProvisionRequest{
		IdempotencyKey: key,
		Credentials:    s.Credentials,
		MerchantID:     shared.MerchantID("mer_01JBCONTRACT0000000000001"),
		LegalName:      "Contract Suite Ltd",
		DisplayName:    "Contract Suite",
		Country:        country,
		MCC:            mcc,
		WebsiteURL:     "https://contract.invalid",
		SupportEmail:   "support@contract.invalid",
		AddressLines:   []string{"1 Test Street"},
		City:           "Testville",
		PostalCode:     "12345",
		Currencies:     []money.Currency{"USD"},
		Methods:        []shared.PaymentMethod{shared.MethodCard},
		Environment:    shared.EnvironmentSandbox,
	}
}

// --- money-moving operation table ----------------------------------------------------------------

// moneyMoving describes one money-moving operation generically, so an assertion can be applied to
// all of them rather than to whichever one the author thought of.
//
// This table is the reason TimeoutYieldsOutcomeUnknown covers refunds and voids and not just
// authorizations. Every adapter bug of this class found in the wild has been in the operation
// nobody wrote the test for.
type moneyMoving struct {
	name string
	// invoke performs the operation against the gateway.
	invoke func(ctx context.Context, g spi.PaymentGateway, s Subject, key string) (*spi.Result, error)
	// success is the scripted successful response for this operation.
	success func(s Subject) *spi.HTTPResponse
	// skip reports that the subject does not implement this operation.
	skip func(s Subject) bool
}

func moneyMovingOps() []moneyMoving {
	return []moneyMoving{
		{
			name: "Authorize",
			invoke: func(ctx context.Context, g spi.PaymentGateway, s Subject, key string) (*spi.Result, error) {
				return g.Authorize(ctx, s.authorizeRequest(key))
			},
			success: func(s Subject) *spi.HTTPResponse {
				return s.Responses.AuthorizeApproved(SuiteGatewayRef, SuiteAmount, SuiteIdempotencyKey)
			},
		},
		{
			name: "Capture",
			invoke: func(ctx context.Context, g spi.PaymentGateway, s Subject, key string) (*spi.Result, error) {
				return g.Capture(ctx, s.captureRequest(key))
			},
			success: func(s Subject) *spi.HTTPResponse {
				return s.Responses.CaptureAccepted(SuiteGatewayRef, SuiteAmount)
			},
		},
		{
			name: "Refund",
			invoke: func(ctx context.Context, g spi.PaymentGateway, s Subject, key string) (*spi.Result, error) {
				return g.Refund(ctx, s.refundRequest(key))
			},
			success: func(s Subject) *spi.HTTPResponse {
				return s.Responses.RefundAccepted(SuiteGatewayRef, SuiteAmount)
			},
		},
		{
			name: "Void",
			invoke: func(ctx context.Context, g spi.PaymentGateway, s Subject, key string) (*spi.Result, error) {
				return g.Void(ctx, s.voidRequest(key))
			},
			success: func(s Subject) *spi.HTTPResponse {
				return s.Responses.VoidAccepted(SuiteGatewayRef)
			},
			skip: func(s Subject) bool { return !s.SupportsVoid },
		},
	}
}

// isMutatingRequest reports whether a recorded request is one of the money-moving calls.
//
// It reads the adapter's own operation label rather than guessing from the URL, which keeps the
// assertion working for a vendor whose money-moving endpoint happens to share a path prefix with
// its token endpoint — PayPal's does.
func isMutatingRequest(r httpx.RecordedRequest) bool {
	switch r.Operation {
	case string(shared.OpAuthorize), string(shared.OpCapture), string(shared.OpRefund), string(shared.OpVoid):
		return true
	default:
		return false
	}
}

// --- assertion helpers ---------------------------------------------------------------------------

func requireNoDoubleAnswer(t *testing.T, where string, res *spi.Result, err error) {
	t.Helper()
	if res == nil && err == nil {
		t.Fatalf("%s: returned a nil result and a nil error, which the SPI forbids: a caller has no way to tell what happened", where)
	}
}

func containsAny(haystack string, needles []string) (string, bool) {
	for _, n := range needles {
		if n != "" && strings.Contains(haystack, n) {
			return n, true
		}
	}
	return "", false
}

func fullRender(err error) string {
	if err == nil {
		return ""
	}
	// Both verbs, because they render through different paths: %+v goes through Error() for a type
	// implementing error, while %#v walks the struct. A credential that survives one and not the
	// other is still a credential in a log line.
	return fmt.Sprintf("%+v\n%#v\n%s", err, err, err.Error())
}

func isOutcomeUnknown(err error) bool { return errors.Is(err, spi.ErrOutcomeUnknown) }
