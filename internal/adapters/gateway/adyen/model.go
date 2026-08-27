package adyen

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// errUnparseable marks a response this adapter could not read. See the identical sentinel in the
// Stripe adapter: the SPI requires an unparseable response on a money-moving call to become
// spi.ErrOutcomeUnknown, and a sentinel is what makes that escalation precise rather than blanket.
var errUnparseable = errors.New("adyen: the gateway response could not be parsed")

// Decoding policy for Adyen: unknown fields are tolerated.
//
// Adyen's response contract is *open* within a major version. `/v71` freezes the request shape and
// the meaning of the fields that exist; it does not stop Adyen adding keys to `additionalData` —
// which is, by design, an open string map — or adding fields to a payment response. Adyen ships
// such additions in ordinary releases and documents them as non-breaking.
//
// So the adapter tolerates additions and validates what it depends on. The alternative,
// DisallowUnknownFields, would convert every Adyen release into an outage of this adapter, and it
// would do so on the *payment path*, where the failure mode is refusing traffic rather than
// missing a field.
//
// The one place this repository does reject unknown fields is the simulator's protocol, which we
// own on both ends: there, an unknown field means two of our own binaries disagree about the wire
// format, and that must fail loudly rather than be shrugged off.
func decode(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(v); err != nil {
		return apierror.Wrap(errors.Join(errUnparseable, err), apierror.CodeGatewayContractViolation,
			"adyen: the response body could not be parsed")
	}
	return nil
}

// amount is Adyen's money representation: minor units plus an ISO code, the same shape as
// money.Money. That correspondence is not a coincidence — both exist because a decimal amount on
// the wire is how a payment platform loses cents — but it is still translated explicitly, because
// Adyen's `value` is a JSON number and a JSON number is a float in every language that is not Go.
type amount struct {
	Value    int64  `json:"value"`
	Currency string `json:"currency"`
}

// paymentResponse is Adyen's /payments answer.
//
// `resultCode` is the field everything hangs off. Note what is *not* here: Adyen does not return
// an HTTP error for a refused card. A refusal is HTTP 200 with `resultCode: Refused`, which is why
// the adapter's decline path reads a 200 body rather than an error envelope — the exact opposite
// of Stripe, and the single most common source of a broken Adyen integration.
type paymentResponse struct {
	PSPReference      string            `json:"pspReference"`
	ResultCode        string            `json:"resultCode"`
	Amount            *amount           `json:"amount"`
	MerchantReference string            `json:"merchantReference"`
	RefusalReason     string            `json:"refusalReason"`
	RefusalReasonCode string            `json:"refusalReasonCode"`
	AdditionalData    map[string]string `json:"additionalData"`
	Metadata          map[string]string `json:"metadata"`
	Action            *action           `json:"action"`
	PaymentMethod     *paymentMethodOut `json:"paymentMethod"`
	// MerchantAccount is echoed on some responses and is used only for diagnostics; control flow
	// never reads it, because trusting a gateway's echo of the account we asked it to charge would
	// be trusting the answer to validate the question.
	MerchantAccount string `json:"merchantAccount"`
}

type paymentMethodOut struct {
	Brand string `json:"brand"`
	Type  string `json:"type"`
}

// action is Adyen's instruction for the shopper: a redirect, a 3DS2 fingerprint or a challenge.
type action struct {
	Type   string `json:"type"`
	URL    string `json:"url"`
	Method string `json:"method"`
	// PaymentData is the opaque state Adyen needs back on /payments/details. The platform stores
	// it against the payment; it is not a secret but it is not useful to anyone else either.
	PaymentData string            `json:"paymentData"`
	Data        map[string]string `json:"data"`
}

// modificationResponse is the answer to a capture, refund or cancel.
//
// `status` is always "received" on success: every Adyen modification is asynchronous, and the
// definitive result arrives as a CAPTURE / REFUND / CANCELLATION notification. The adapter's
// status mapping says so explicitly rather than reporting a modification as final.
type modificationResponse struct {
	PSPReference        string            `json:"pspReference"`
	PaymentPSPReference string            `json:"paymentPspReference"`
	Status              string            `json:"status"`
	Amount              *amount           `json:"amount"`
	Reference           string            `json:"reference"`
	MerchantAccount     string            `json:"merchantAccount"`
	Metadata            map[string]string `json:"metadata"`
}

// listResponse is the envelope the reconciliation lookup reads.
type listResponse struct {
	Data []paymentResponse `json:"data"`
}

// serviceError is Adyen's error body. It is a flat document with a numeric-string `errorCode`,
// which is the field to branch on: `message` is prose and is localised.
type serviceError struct {
	Status       int    `json:"status"`
	ErrorCode    string `json:"errorCode"`
	Message      string `json:"message"`
	ErrorType    string `json:"errorType"`
	PSPReference string `json:"pspReference"`
}

// notification is Adyen's webhook envelope. Adyen batches: one POST can carry several items, and
// an adapter that reads only the first silently drops events under load.
type notification struct {
	Live              string             `json:"live"`
	NotificationItems []notificationItem `json:"notificationItems"`
}

type notificationItem struct {
	Item *notificationRequestItem `json:"NotificationRequestItem"`
}

// notificationRequestItem is one event.
//
// The field names are Adyen's, including the capitalised wrapper key above: their notification
// format predates their REST conventions and has never been changed, because changing it would
// break every integration ever built against it.
type notificationRequestItem struct {
	PSPReference      string            `json:"pspReference"`
	OriginalReference string            `json:"originalReference"`
	MerchantAccount   string            `json:"merchantAccountCode"`
	MerchantReference string            `json:"merchantReference"`
	Amount            *amount           `json:"amount"`
	EventCode         string            `json:"eventCode"`
	EventDate         string            `json:"eventDate"`
	Success           string            `json:"success"`
	Reason            string            `json:"reason"`
	PaymentMethod     string            `json:"paymentMethod"`
	AdditionalData    map[string]string `json:"additionalData"`
}

// --- Legal Entity Management / Balance Platform ------------------------------------------------

type legalEntityResponse struct {
	ID           string             `json:"id"`
	Type         string             `json:"type"`
	Reference    string             `json:"reference"`
	Capabilities map[string]lemCap  `json:"capabilities"`
	Problems     []lemProblem       `json:"problems"`
	Organization *lemOrganization   `json:"organization"`
	Associations []lemAssociation   `json:"entityAssociations"`
	Documents    []lemDocumentEntry `json:"documentDetails"`
}

type lemCap struct {
	Allowed            bool   `json:"allowed"`
	Requested          bool   `json:"requested"`
	VerificationStatus string `json:"verificationStatus"`
}

type lemProblem struct {
	Entity struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"entity"`
	VerificationErrors []struct {
		Code    string   `json:"code"`
		Message string   `json:"message"`
		Type    string   `json:"type"`
		Remedy  []string `json:"remediatingActions"`
	} `json:"verificationErrors"`
}

type lemOrganization struct {
	LegalName          string `json:"legalName"`
	RegistrationNumber string `json:"registrationNumber"`
}

type lemAssociation struct {
	LegalEntityID string `json:"legalEntityId"`
	Type          string `json:"type"`
	JobTitle      string `json:"jobTitle"`
}

type lemDocumentEntry struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type businessLineResponse struct {
	ID            string `json:"id"`
	LegalEntityID string `json:"legalEntityId"`
	Service       string `json:"service"`
	IndustryCode  string `json:"industryCode"`
}

type accountHolderResponse struct {
	ID            string            `json:"id"`
	LegalEntityID string            `json:"legalEntityId"`
	Status        string            `json:"status"`
	Reference     string            `json:"reference"`
	Description   string            `json:"description"`
	Capabilities  map[string]lemCap `json:"capabilities"`
}

type transferInstrumentResponse struct {
	ID            string `json:"id"`
	LegalEntityID string `json:"legalEntityId"`
	Type          string `json:"type"`
}

type webhookResponse struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	Active               bool   `json:"active"`
	CommunicationFormat  string `json:"communicationFormat"`
	Username             string `json:"username"`
	HMACKey              string `json:"hmacKey"`
	AcceptsExpiredCert   bool   `json:"acceptsExpiredCertificate"`
	AcceptsSelfSignedCrt bool   `json:"acceptsSelfSignedCertificate"`
}

type hmacResponse struct {
	HMACKey string `json:"hmacKey"`
}

func upperTrim(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }
