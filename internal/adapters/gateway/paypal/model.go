package paypal

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// errUnparseable marks a response this adapter could not read. See the identical sentinel in the
// Stripe adapter: the SPI requires an unparseable response on a money-moving call to become
// spi.ErrOutcomeUnknown, and a sentinel keeps that escalation precise rather than blanket.
var errUnparseable = errors.New("paypal: the gateway response could not be parsed")

// Decoding policy for PayPal: unknown fields are tolerated.
//
// PayPal's v2 contract is *open*. They add fields to orders, captures and the `processor_response`
// object without a version bump, and their own SDKs ignore unknown members. The `links` array in
// particular is explicitly documented as extensible — HATEOAS is the whole design — so rejecting
// unknown fields would break on any new link relation.
//
// What this adapter validates instead is the small set of fields it actually acts on: the order
// id, the status, and the amount echo. Those are checked explicitly, which is stronger than a
// blanket strictness that would also reject harmless additions.
//
// (The simulator, whose protocol this repository owns on both ends, does reject unknown fields.
// There an unknown field means our own binaries disagree, and that must fail loudly.)
func decode(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(v); err != nil {
		return apierror.Wrap(errors.Join(errUnparseable, err), apierror.CodeGatewayContractViolation,
			"paypal: the response body could not be parsed")
	}
	return nil
}

// amountValue is PayPal's money representation.
//
// PayPal is the only one of the three gateways that puts amounts on the wire as *decimal strings*.
// That is a genuine hazard: parsing "25.99" through a float64 and multiplying by 100 yields
// 2598.9999999999995, and truncating that gives 2598 — a cent lost per transaction, silently, in
// the direction the merchant notices at the end of the month. Every conversion in this package goes
// through parseAmount, which is integer arithmetic on the string's digits and never touches a
// float.
type amountValue struct {
	CurrencyCode string `json:"currency_code"`
	Value        string `json:"value"`
}

// order is PayPal's v2 Checkout order.
type order struct {
	ID            string         `json:"id"`
	Status        string         `json:"status"`
	Intent        string         `json:"intent"`
	PurchaseUnits []purchaseUnit `json:"purchase_units"`
	Links         []link         `json:"links"`
	CreateTime    string         `json:"create_time"`
	UpdateTime    string         `json:"update_time"`
}

type purchaseUnit struct {
	ReferenceID string       `json:"reference_id"`
	CustomID    string       `json:"custom_id"`
	InvoiceID   string       `json:"invoice_id"`
	Amount      *amountValue `json:"amount"`
	Payments    *payments    `json:"payments"`
}

type payments struct {
	Captures       []captureObject       `json:"captures"`
	Authorizations []authorizationObject `json:"authorizations"`
	Refunds        []refundObject        `json:"refunds"`
}

type captureObject struct {
	ID                string             `json:"id"`
	Status            string             `json:"status"`
	StatusDetails     *statusDetails     `json:"status_details"`
	Amount            *amountValue       `json:"amount"`
	FinalCapture      bool               `json:"final_capture"`
	CustomID          string             `json:"custom_id"`
	InvoiceID         string             `json:"invoice_id"`
	ProcessorResponse *processorResponse `json:"processor_response"`
	SellerReceivable  *sellerReceivable  `json:"seller_receivable_breakdown"`
	CreateTime        string             `json:"create_time"`
}

type authorizationObject struct {
	ID                string             `json:"id"`
	Status            string             `json:"status"`
	StatusDetails     *statusDetails     `json:"status_details"`
	Amount            *amountValue       `json:"amount"`
	ExpirationTime    string             `json:"expiration_time"`
	CustomID          string             `json:"custom_id"`
	InvoiceID         string             `json:"invoice_id"`
	ProcessorResponse *processorResponse `json:"processor_response"`
	CreateTime        string             `json:"create_time"`
}

type refundObject struct {
	ID            string         `json:"id"`
	Status        string         `json:"status"`
	StatusDetails *statusDetails `json:"status_details"`
	Amount        *amountValue   `json:"amount"`
	CustomID      string         `json:"custom_id"`
	InvoiceID     string         `json:"invoice_id"`
	CreateTime    string         `json:"create_time"`
}

type statusDetails struct {
	Reason string `json:"reason"`
}

// processorResponse is the scheme-level answer PayPal passes through from the acquirer. It is only
// populated for card-funded transactions; a PayPal-wallet payment has no processor beneath it, and
// every read of this struct has to tolerate its absence.
type processorResponse struct {
	AVSCode           string `json:"avs_code"`
	CVVCode           string `json:"cvv_code"`
	ResponseCode      string `json:"response_code"`
	PaymentAdviceCode string `json:"payment_advice_code"`
}

type sellerReceivable struct {
	GrossAmount   *amountValue  `json:"gross_amount"`
	PayPalFee     *amountValue  `json:"paypal_fee"`
	NetAmount     *amountValue  `json:"net_amount"`
	ExchangeRate  *exchangeRate `json:"exchange_rate"`
	ReceivableAmt *amountValue  `json:"receivable_amount"`
}

type exchangeRate struct {
	SourceCurrency string `json:"source_currency"`
	TargetCurrency string `json:"target_currency"`
	Value          string `json:"value"`
}

// link is one HATEOAS relation. PayPal expresses "where the payer must go" and "how to complete
// this order" as links rather than as named fields, so the adapter has to look relations up by
// name — and must tolerate relations it does not recognise, because PayPal adds them.
type link struct {
	Href   string `json:"href"`
	Rel    string `json:"rel"`
	Method string `json:"method"`
}

func linkByRel(links []link, rel string) string {
	for _, l := range links {
		if strings.EqualFold(l.Rel, rel) {
			return l.Href
		}
	}
	return ""
}

// errorResponse is PayPal's error body.
//
// `debug_id` is the field PayPal support asks for and is safe to surface: it identifies the
// request in their logs and carries no caller data. `message` is prose and can echo request
// fields, so it is never rendered into a platform error.
type errorResponse struct {
	Name            string        `json:"name"`
	Message         string        `json:"message"`
	DebugID         string        `json:"debug_id"`
	Details         []errorDetail `json:"details"`
	Links           []link        `json:"links"`
	Error           string        `json:"error"`
	ErrorDescrption string        `json:"error_description"`
}

type errorDetail struct {
	Field       string `json:"field"`
	Issue       string `json:"issue"`
	Description string `json:"description"`
}

// issues returns the machine-readable issue codes, which are the only part of a PayPal error the
// adapter branches on.
func (e *errorResponse) issues() []string {
	out := make([]string, 0, len(e.Details))
	for _, d := range e.Details {
		if d.Issue != "" {
			out = append(out, d.Issue)
		}
	}
	return out
}

// tokenResponse is the OAuth2 client-credentials answer.
type tokenResponse struct {
	Scope       string `json:"scope"`
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	AppID       string `json:"app_id"`
	ExpiresIn   int64  `json:"expires_in"`
	Nonce       string `json:"nonce"`
}

// verifySignatureResponse is the answer to the webhook verification call.
type verifySignatureResponse struct {
	VerificationStatus string `json:"verification_status"`
}

// partnerReferral is the answer to POST /v2/customer/partner-referrals. The useful content is
// entirely in `links`: PayPal returns the hosted consent URL as `rel: action_url`.
type partnerReferral struct {
	Links []link `json:"links"`
}

// merchantIntegration is the readiness view of a referred merchant.
type merchantIntegration struct {
	MerchantID            string          `json:"merchant_id"`
	TrackingID            string          `json:"tracking_id"`
	PaymentsReceivable    bool            `json:"payments_receivable"`
	PrimaryEmailConfirmed bool            `json:"primary_email_confirmed"`
	Products              []mipProduct    `json:"products"`
	Capabilities          []mipCapability `json:"capabilities"`
	OAuthIntegrations     []struct {
		IntegrationType string `json:"integration_type"`
	} `json:"oauth_integrations"`
}

type mipProduct struct {
	Name          string   `json:"name"`
	VettingStatus string   `json:"vetting_status"`
	Capabilities  []string `json:"capabilities"`
}

type mipCapability struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// webhookEvent is PayPal's notification envelope.
type webhookEvent struct {
	ID           string          `json:"id"`
	EventVersion string          `json:"event_version"`
	CreateTime   string          `json:"create_time"`
	ResourceType string          `json:"resource_type"`
	EventType    string          `json:"event_type"`
	Summary      string          `json:"summary"`
	Resource     json.RawMessage `json:"resource"`
	Links        []link          `json:"links"`
}

// webhookResource is the union of the fields the platform reads from any PayPal event resource.
type webhookResource struct {
	ID                string             `json:"id"`
	Status            string             `json:"status"`
	Amount            *amountValue       `json:"amount"`
	CustomID          string             `json:"custom_id"`
	InvoiceID         string             `json:"invoice_id"`
	ProcessorResponse *processorResponse `json:"processor_response"`
	SupplementaryData *struct {
		RelatedIDs struct {
			OrderID         string `json:"order_id"`
			AuthorizationID string `json:"authorization_id"`
			CaptureID       string `json:"capture_id"`
		} `json:"related_ids"`
	} `json:"supplementary_data"`
	StatusDetails *statusDetails `json:"status_details"`
	CreateTime    string         `json:"create_time"`
}

// registeredWebhook is the answer to POST /v1/notifications/webhooks.
type registeredWebhook struct {
	ID         string `json:"id"`
	URL        string `json:"url"`
	EventTypes []struct {
		Name string `json:"name"`
	} `json:"event_types"`
}

// --- decimal-string money, without floats -----------------------------------------------------

// formatAmount renders a money.Money as PayPal's decimal string.
//
// The digits are produced by integer division, not by dividing a float. For a zero-exponent
// currency (JPY, KRW) PayPal expects no decimal point at all, and sending "1000.00" for JPY is
// rejected — which is why the exponent comes from money.Currency rather than being assumed to be
// two.
func formatAmount(m money.Money) string {
	exp := m.Currency().Exponent()
	a := m.Amount()
	sign := ""
	if a < 0 {
		sign, a = "-", -a
	}
	if exp == 0 {
		return sign + strconv.FormatInt(a, 10)
	}
	div := int64(1)
	for i := 0; i < exp; i++ {
		div *= 10
	}
	major, minor := a/div, a%div
	minorStr := strconv.FormatInt(minor, 10)
	for len(minorStr) < exp {
		minorStr = "0" + minorStr
	}
	return sign + strconv.FormatInt(major, 10) + "." + minorStr
}

// parseAmount converts PayPal's decimal string back to minor units, exactly.
//
// It rejects more decimal places than the currency permits rather than rounding. A response with
// three decimals for USD is not a rounding question, it is a contract violation: something upstream
// is doing arithmetic the platform does not understand, and quietly rounding it would hide the
// discrepancy until reconciliation.
func parseAmount(a *amountValue) (money.Money, error) {
	if a == nil {
		return money.Money{}, apierror.New(apierror.CodeGatewayContractViolation,
			"paypal: the response carries no amount")
	}
	cur, err := money.ParseCurrency(a.CurrencyCode)
	if err != nil {
		return money.Money{}, apierror.Wrapf(err, apierror.CodeGatewayContractViolation,
			"paypal: the response carries currency %q, which this platform does not support", a.CurrencyCode)
	}
	minor, err := money.ParseMinorUnits(a.Value, cur)
	if err != nil {
		return money.Money{}, apierror.Wrapf(err, apierror.CodeGatewayContractViolation,
			"paypal: the amount %q is not valid for %s", a.Value, cur)
	}
	if minor == math.MinInt64 {
		return money.Money{}, apierror.New(apierror.CodeGatewayContractViolation,
			"paypal: the amount overflows the platform's representation")
	}
	return money.New(minor, cur)
}
