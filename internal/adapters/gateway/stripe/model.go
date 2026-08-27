package stripe

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// errUnparseable marks a response this adapter could not read.
//
// It exists as a sentinel rather than as one more apierror code because of a specific SPI
// obligation: a response the adapter cannot parse, on a money-moving call, must become
// spi.ErrOutcomeUnknown rather than a failure. Stripe acted; we simply cannot read what it said,
// and reporting that as an error would let the orchestrator fail over onto a charge that may
// already exist. Distinguishing "unreadable" from every other contract violation is what makes
// that escalation precise instead of blanket.
var errUnparseable = errors.New("stripe: the gateway response could not be parsed")

// Decoding policy for Stripe: unknown fields are tolerated.
//
// Stripe's response contract is *open*. They add fields to existing objects continuously and
// consider that backwards compatible — `payment_method_details.card.network_transaction_id`,
// `network_advice_code` and `presentment_details` all appeared on objects this adapter already
// consumed. Pinning `Stripe-Version` freezes request-shape and removal semantics, not additions.
// Rejecting unknown fields would therefore turn a routine Stripe release into a total outage of
// this adapter, which is a far worse failure than silently ignoring a field we do not use.
//
// The safety this gives up — catching a typo in a field name at decode time — is bought back by
// the response validator (L6) above the adapter, which asserts on the fields the platform
// actually depends on rather than on the ones the vendor happens to send.
//
// Contrast the simulator adapter, whose protocol we own on both ends and which therefore *does*
// reject unknown fields: there, an unknown field means a version skew between two of our own
// binaries, and failing loudly is the point.
func decode(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return apierror.Wrap(errors.Join(errUnparseable, err), apierror.CodeGatewayContractViolation,
			"stripe: the response body could not be parsed")
	}
	return nil
}

// paymentIntent is the subset of Stripe's PaymentIntent this platform depends on.
//
// It is deliberately a subset. Modelling every field would mean tracking Stripe's release notes
// for fields nobody reads, and each unused field is another thing a reviewer has to check is not
// being used to make a decision it should not drive.
type paymentIntent struct {
	ID       string `json:"id"`
	Object   string `json:"object"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Status   string `json:"status"`
	// AmountReceived is what Stripe actually took. It differs from Amount on a partial capture and
	// is zero on an uncaptured authorization, which is why the two are never conflated below.
	AmountReceived   int64             `json:"amount_received"`
	AmountCapturable int64             `json:"amount_capturable"`
	CaptureMethod    string            `json:"capture_method"`
	Created          int64             `json:"created"`
	Metadata         map[string]string `json:"metadata"`
	// LatestCharge is a string id unless `expand[]=latest_charge` was sent, in which case it is
	// the object. json.RawMessage defers that decision to chargeFrom, which is the only place the
	// polymorphism has to be understood.
	LatestCharge     json.RawMessage   `json:"latest_charge"`
	NextAction       *nextAction       `json:"next_action"`
	LastPaymentError *stripeError      `json:"last_payment_error"`
	ClientSecret     string            `json:"client_secret"`
	CancellationRsn  string            `json:"cancellation_reason"`
	ProcessingRaw    json.RawMessage   `json:"processing"`
	Livemode         bool              `json:"livemode"`
	Charges          *chargeCollection `json:"charges"`
}

type chargeCollection struct {
	Data []charge `json:"data"`
}

type charge struct {
	ID                   string                `json:"id"`
	Amount               int64                 `json:"amount"`
	Currency             string                `json:"currency"`
	Status               string                `json:"status"`
	Paid                 bool                  `json:"paid"`
	Captured             bool                  `json:"captured"`
	Outcome              *chargeOutcome        `json:"outcome"`
	PaymentMethodDetails *paymentMethodDetails `json:"payment_method_details"`
	BalanceTransaction   json.RawMessage       `json:"balance_transaction"`
	FailureCode          string                `json:"failure_code"`
	FailureMessage       string                `json:"failure_message"`
}

type chargeOutcome struct {
	NetworkStatus      string `json:"network_status"`
	Reason             string `json:"reason"`
	RiskLevel          string `json:"risk_level"`
	SellerMessage      string `json:"seller_message"`
	Type               string `json:"type"`
	NetworkAdviceCode  string `json:"network_advice_code"`
	NetworkDeclineCode string `json:"network_decline_code"`
}

type paymentMethodDetails struct {
	Type string      `json:"type"`
	Card *cardDetail `json:"card"`
}

type cardDetail struct {
	Brand               string       `json:"brand"`
	Country             string       `json:"country"`
	Last4               string       `json:"last4"`
	Network             string       `json:"network"`
	NetworkTransaction  string       `json:"network_transaction_id"`
	Checks              *cardChecks  `json:"checks"`
	ThreeDSecure        *threeDSInfo `json:"three_d_secure"`
	AuthorizationCode   string       `json:"authorization_code"`
	ExtendedAuthEnabled bool         `json:"extended_authorization"`
}

type cardChecks struct {
	AddressLine1Check      string `json:"address_line1_check"`
	AddressPostalCodeCheck string `json:"address_postal_code_check"`
	CVCCheck               string `json:"cvc_check"`
}

type threeDSInfo struct {
	Result             string `json:"result"`
	ElectronicCommerce string `json:"electronic_commerce_indicator"`
	Version            string `json:"version"`
	AuthenticationFlow string `json:"authentication_flow"`
}

type nextAction struct {
	Type          string `json:"type"`
	RedirectToURL *struct {
		URL       string `json:"url"`
		ReturnURL string `json:"return_url"`
	} `json:"redirect_to_url"`
	UseStripeSDK json.RawMessage `json:"use_stripe_sdk"`
}

// refund is Stripe's Refund object.
type refund struct {
	ID            string            `json:"id"`
	Object        string            `json:"object"`
	Amount        int64             `json:"amount"`
	Currency      string            `json:"currency"`
	Status        string            `json:"status"`
	Reason        string            `json:"reason"`
	PaymentIntent string            `json:"payment_intent"`
	Charge        string            `json:"charge"`
	Created       int64             `json:"created"`
	Metadata      map[string]string `json:"metadata"`
	FailureReason string            `json:"failure_reason"`
}

// listResponse is Stripe's pagination envelope.
type listResponse[T any] struct {
	Object  string `json:"object"`
	Data    []T    `json:"data"`
	HasMore bool   `json:"has_more"`
	URL     string `json:"url"`
}

// errorEnvelope is Stripe's error body. Every non-2xx from Stripe has this shape, including the
// 402 that carries a card decline — which is why the decline path reads `error.payment_intent`
// rather than treating a 402 as a transport failure.
type errorEnvelope struct {
	Error *stripeError `json:"error"`
}

// stripeError is Stripe's error object, and doubles as `payment_intent.last_payment_error`.
type stripeError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param"`
	// DeclineCode is the issuer's reason, present only for `type: card_error` with
	// `code: card_declined`. It is the field the whole decline taxonomy hangs off.
	DeclineCode string `json:"decline_code"`
	// NetworkAdviceCode and NetworkDeclineCode are the scheme's own retry guidance, surfaced by
	// Stripe since the 2024 Visa/Mastercard retry mandates. "02" and "03" mean do not retry, ever,
	// and a platform that ignores them accrues per-attempt fines.
	NetworkAdviceCode  string          `json:"network_advice_code"`
	NetworkDeclineCode string          `json:"network_decline_code"`
	Charge             string          `json:"charge"`
	DocURL             string          `json:"doc_url"`
	PaymentIntent      *paymentIntent  `json:"payment_intent"`
	Source             json.RawMessage `json:"source"`
	RequestLogURL      string          `json:"request_log_url"`
}

// account is Stripe's connected Account, trimmed to the readiness signals onboarding asserts on.
type account struct {
	ID             string        `json:"id"`
	Object         string        `json:"object"`
	Country        string        `json:"country"`
	Type           string        `json:"type"`
	ChargesEnabled bool          `json:"charges_enabled"`
	PayoutsEnabled bool          `json:"payouts_enabled"`
	Requirements   *requirements `json:"requirements"`
	Deleted        bool          `json:"deleted"`
}

type requirements struct {
	CurrentlyDue   []string `json:"currently_due"`
	EventuallyDue  []string `json:"eventually_due"`
	PastDue        []string `json:"past_due"`
	PendingVerif   []string `json:"pending_verification"`
	DisabledReason string   `json:"disabled_reason"`
}

// webhookEndpoint is the response to POST /v1/webhook_endpoints. `secret` is returned exactly
// once, at creation; it goes straight to the secrets store and is never persisted anywhere else.
type webhookEndpoint struct {
	ID            string   `json:"id"`
	Object        string   `json:"object"`
	URL           string   `json:"url"`
	Secret        string   `json:"secret"`
	Status        string   `json:"status"`
	EnabledEvents []string `json:"enabled_events"`
}

// event is Stripe's webhook envelope.
type event struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Type    string `json:"type"`
	Created int64  `json:"created"`
	Account string `json:"account"`
	Data    struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
	Livemode bool `json:"livemode"`
}

// chargeFrom resolves the `latest_charge` polymorphism.
//
// Stripe returns an id string unless the caller expanded the field. The adapters always request
// the expansion, but a response replayed from an idempotency cache created by an older adapter
// version will not have it, so both shapes have to work. Returning (nil, nil) for the string form
// is correct: the charge simply is not available, and the processor detail it would have carried
// is optional everywhere it is consumed.
func chargeFrom(raw json.RawMessage) *charge {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '"' {
		return nil
	}
	var c charge
	if err := json.Unmarshal(trimmed, &c); err != nil {
		return nil
	}
	return &c
}

// primaryCharge picks the charge that describes this intent, preferring the expanded
// `latest_charge` and falling back to the legacy `charges.data[0]` that older API versions used.
func (p *paymentIntent) primaryCharge() *charge {
	if c := chargeFrom(p.LatestCharge); c != nil {
		return c
	}
	if p.Charges != nil && len(p.Charges.Data) > 0 {
		return &p.Charges.Data[0]
	}
	return nil
}

// normalizeCurrency uppercases Stripe's lowercase ISO codes so they can be compared with
// money.Currency without a conversion at every call site.
func normalizeCurrency(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }
