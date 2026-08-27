package simulator

import (
	"bytes"
	"encoding/json"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// The simulator's wire protocol is a *closed* contract, and this is the one place in the repository
// where JSON decoding rejects unknown fields.
//
// The reasoning is the mirror image of the vendor adapters'. Stripe, Adyen and PayPal are third
// parties who add response fields on their own schedule, so strictness there converts their routine
// release into our outage. The simulator's protocol has exactly two implementations, both in this
// repository, deployed from the same commit. An unknown field on this wire therefore does not mean
// "the vendor shipped something new" — it means a client and a server from different versions are
// talking to each other, which is a deployment bug that must fail loudly and immediately rather
// than be silently tolerated until somebody notices a field is being ignored.
//
// Both directions are strict: the server rejects an unknown request field, and the client rejects
// an unknown response field. Being strict in only one direction would let skew hide in the other.
func decodeStrict(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return apierror.Wrap(err, apierror.CodeGatewayContractViolation,
			"simulator: the body does not match this version of the simulator protocol")
	}
	return nil
}

// WireAmount is the protocol's money representation: minor units plus an ISO code, mirroring
// money.Money exactly so no conversion can lose a cent.
type WireAmount struct {
	MinorUnits int64  `json:"minorUnits"`
	Currency   string `json:"currency"`
}

// WireRequest is every mutating request the simulator accepts.
//
// One request type rather than five, because the simulator's job is to be a *behaviour*, not an
// API-shape exercise: the shapes that matter for adapter correctness are the vendors', and the
// simulator's own protocol should be as boring as possible so that a test failure is never about
// the simulator's serialization.
type WireRequest struct {
	Operation      string            `json:"operation"`
	IdempotencyKey string            `json:"idempotencyKey"`
	Reference      string            `json:"reference,omitempty"`
	Amount         *WireAmount       `json:"amount,omitempty"`
	Capture        bool              `json:"capture,omitempty"`
	Final          bool              `json:"final,omitempty"`
	PaymentID      string            `json:"paymentId,omitempty"`
	AttemptID      string            `json:"attemptId,omitempty"`
	RefundID       string            `json:"refundId,omitempty"`
	ReturnURL      string            `json:"returnUrl,omitempty"`
	ThreeDS        bool              `json:"threeDs,omitempty"`
	Reason         string            `json:"reason,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// WireNextAction is the simulator's redirect instruction.
type WireNextAction struct {
	Type        string `json:"type"`
	RedirectURL string `json:"redirectUrl,omitempty"`
	QRCodeData  string `json:"qrCodeData,omitempty"`
}

// WireResponse is every successful answer.
//
// `Scenario` is echoed so a failing test can say "the simulator was in decline_stolen_card" without
// the reader having to re-derive it from the amount. It is diagnostic only: no adapter branches on
// it, and an adapter that did would be branching on a field no real gateway has.
type WireResponse struct {
	Status               string          `json:"status"`
	Reference            string          `json:"reference,omitempty"`
	RawStatus            string          `json:"rawStatus,omitempty"`
	RawCode              string          `json:"rawCode,omitempty"`
	RawMessage           string          `json:"rawMessage,omitempty"`
	DeclineCode          string          `json:"declineCode,omitempty"`
	NetworkAdviceNoRetry bool            `json:"networkAdviceNoRetry,omitempty"`
	AuthorizedAmount     *WireAmount     `json:"authorizedAmount,omitempty"`
	CapturedAmount       *WireAmount     `json:"capturedAmount,omitempty"`
	NextAction           *WireNextAction `json:"nextAction,omitempty"`
	Scenario             string          `json:"scenario,omitempty"`
	// AuthCode and the verification results exist so the simulator can exercise the platform's
	// dispute-defence path, which otherwise has no test data at all.
	AuthCode  string `json:"authCode,omitempty"`
	AVSResult string `json:"avsResult,omitempty"`
	CVVResult string `json:"cvvResult,omitempty"`
}

// WireError is every failure answer. `code` is the platform's own error code rather than a
// simulated vendor code, because the simulator is not pretending to be a vendor here — it is
// telling its client that the *simulator* refused.
type WireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WireProvisionRequest creates a simulated sub-merchant.
type WireProvisionRequest struct {
	IdempotencyKey string   `json:"idempotencyKey"`
	MerchantID     string   `json:"merchantId"`
	LegalName      string   `json:"legalName,omitempty"`
	Country        string   `json:"country,omitempty"`
	MCC            string   `json:"mcc,omitempty"`
	Currencies     []string `json:"currencies,omitempty"`
}

// WireProvisionResponse is the simulated account.
type WireProvisionResponse struct {
	AccountID           string   `json:"accountId"`
	Status              string   `json:"status"`
	RequiresAction      bool     `json:"requiresAction,omitempty"`
	ActionURL           string   `json:"actionUrl,omitempty"`
	PendingRequirements []string `json:"pendingRequirements,omitempty"`
}

// WireWebhookRequest registers the platform's ingress endpoint.
type WireWebhookRequest struct {
	IdempotencyKey string   `json:"idempotencyKey"`
	AccountID      string   `json:"accountId,omitempty"`
	URL            string   `json:"url"`
	EventTypes     []string `json:"eventTypes,omitempty"`
}

// WireWebhookResponse is the registration, including the signing secret the simulator will use.
type WireWebhookResponse struct {
	RegistrationID string `json:"registrationId"`
	SigningSecret  string `json:"signingSecret"`
	URL            string `json:"url"`
}

// Wire operation names. They are the platform's own shared.Operation values, which keeps the
// simulator's protocol legible next to a trace.
const (
	opAuthorize = "authorize"
	opCapture   = "capture"
	opRefund    = "refund"
	opVoid      = "void"
	opLookup    = "lookup"
)
