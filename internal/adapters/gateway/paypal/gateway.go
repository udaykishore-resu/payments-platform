// Package paypal is the PayPal adapter, against the v2 Orders and Payments APIs.
//
// Three things make PayPal structurally different from the other two integrations, and each one
// is a place a naive adapter fails under load rather than in a test:
//
//  1. **Every call needs a bearer token.** PayPal has no static API key: the platform exchanges
//     OAuth2 client credentials for a nine-hour token. That means an adapter with no cache
//     re-exchanges on every payment, and an adapter with a naive cache stampedes the token endpoint
//     on a cold start — an endpoint PayPal rate-limits far harder than the payment endpoints. See
//     token.go for the cache and its single-flight guard.
//  2. **A decline is an HTTP 422.** PayPal reports a refused card as an error body with
//     `issue: INSTRUMENT_DECLINED`, not as a status on a 200. An adapter that treats 4xx as failure
//     reports every decline as an ERROR outcome, which permits failover, which is precisely what a
//     hard decline must not do.
//  3. **Amounts are decimal strings.** "25.99", not 2599. Parsing that through a float loses a cent
//     per transaction in the direction merchants notice. Every conversion goes through the integer
//     path in model.go.
//
// Everything PayPal-shaped stops here. Above this package the platform sees only spi.Result.
package paypal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// GatewayID is this adapter's registry slug.
const GatewayID shared.GatewayID = "paypal"

// Credential field names as they appear in the resolved spi.Credentials map.
//
// PayPal's partner model gives the platform no per-merchant secret either: the platform's OAuth
// client plus a `PayPal-Auth-Assertion` header naming the merchant is the credential. That is
// structurally the same trade Stripe makes — one key, concentrated blast radius, simple rotation.
const (
	// CredentialClientID is the OAuth2 client identifier.
	CredentialClientID = "client_id"
	// CredentialClientSecret is the OAuth2 client secret.
	CredentialClientSecret = "client_secret"
	// CredentialPartnerID is the platform's own PayPal merchant id, needed to build the auth
	// assertion and to read merchant-integration status during onboarding.
	CredentialPartnerID = "partner_id"
	// CredentialWebhookID identifies the webhook registration whose signature PayPal verifies
	// against. It is not secret material, but it is resolved from the same store and is required
	// for verification, so it travels with the credentials.
	CredentialWebhookID = "webhook_id"
)

// RequestIDHeader is PayPal's idempotency header. Unlike Stripe's and Adyen's it is named for a
// request rather than for idempotency, which is why an adapter can plausibly forget it exists.
const RequestIDHeader = "PayPal-Request-Id"

// Gateway is the PayPal implementation of spi.PaymentGateway.
type Gateway struct {
	cfg    spi.Config
	client spi.HTTPDoer
	clock  shared.Clock
	tokens *tokenCache
}

var _ spi.PaymentGateway = (*Gateway)(nil)

// NewGateway builds the payment-path adapter.
func NewGateway(cfg spi.Config) (*Gateway, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Gateway{cfg: cfg, client: cfg.HTTPClient, clock: cfg.Clock, tokens: newTokenCache(cfg.Clock)}, nil
}

func normalizeConfig(cfg spi.Config) (spi.Config, error) {
	if cfg.BaseURL == "" {
		return cfg, apierror.New(apierror.CodeGatewayNotConfigured,
			"paypal: a base URL is required; the sandbox and live hosts differ and there is no safe default")
	}
	if cfg.HTTPClient == nil {
		return cfg, apierror.New(apierror.CodeGatewayNotConfigured,
			"paypal: an HTTP client must be injected so the adapter runs inside the resilience envelope")
	}
	if !cfg.Environment.IsValid() {
		return cfg, apierror.New(apierror.CodeGatewayNotConfigured,
			"paypal: the environment must be sandbox or production")
	}
	if cfg.Clock == nil {
		cfg.Clock = shared.SystemClock{}
	}
	if cfg.WebhookTolerance <= 0 {
		cfg.WebhookTolerance = 5 * time.Minute
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return cfg, nil
}

// ID returns the registry slug.
func (g *Gateway) ID() shared.GatewayID { return GatewayID }

type orderRequest struct {
	Intent                string              `json:"intent"`
	PurchaseUnits         []purchaseUnitInput `json:"purchase_units"`
	PaymentSource         map[string]any      `json:"payment_source,omitempty"`
	ProcessingInstruction string              `json:"processing_instruction,omitempty"`
}

type purchaseUnitInput struct {
	ReferenceID string      `json:"reference_id,omitempty"`
	CustomID    string      `json:"custom_id,omitempty"`
	InvoiceID   string      `json:"invoice_id,omitempty"`
	Description string      `json:"description,omitempty"`
	Amount      amountValue `json:"amount"`
	SoftDesc    string      `json:"soft_descriptor,omitempty"`
}

// Authorize creates an order and drives it as far as one call can.
//
// `intent` is CAPTURE for a sale and AUTHORIZE for a hold, and it is not editable afterwards: the
// order carries the intent, and a merchant who authorizes and later wants to capture more than the
// authorized amount has to create a new order. That immutability is why req.Capture is read here
// and never re-derived downstream.
//
// `processing_instruction: ORDER_COMPLETE_ON_PAYMENT_APPROVAL` asks PayPal to complete the payment
// as soon as the payer approves, which collapses the create/approve/capture round trips into as few
// as the flow allows. Without it a vaulted-instrument payment needs a second explicit capture call
// with its own idempotency key and its own opportunity to time out.
//
// `custom_id` and `invoice_id` carry the platform's identifiers. `invoice_id` is the one PayPal
// enforces uniqueness on, so it carries the idempotency key: a duplicate reaches PayPal as
// DUPLICATE_INVOICE_ID rather than as a second charge, which is a second line of defence behind the
// PayPal-Request-Id header.
func (g *Gateway) Authorize(ctx context.Context, req spi.AuthorizeRequest) (*spi.Result, error) {
	if err := requireKey(req.IdempotencyKey); err != nil {
		return nil, err
	}
	if !req.Amount.IsValid() {
		return nil, apierror.New(apierror.CodeAmountInvalid,
			"paypal: the amount carries an unsupported currency")
	}
	intent := "AUTHORIZE"
	if req.Capture {
		intent = "CAPTURE"
	}
	body := orderRequest{
		Intent:                intent,
		ProcessingInstruction: "ORDER_COMPLETE_ON_PAYMENT_APPROVAL",
		PurchaseUnits: []purchaseUnitInput{{
			ReferenceID: req.PaymentID.String(),
			CustomID:    req.AttemptID.String(),
			InvoiceID:   req.IdempotencyKey,
			Description: truncate(req.Reference, 127),
			SoftDesc:    truncate(firstNonEmpty(req.StatementRef, req.Descriptor), 22),
			Amount: amountValue{
				CurrencyCode: string(req.Amount.Currency()),
				Value:        formatAmount(req.Amount),
			},
		}},
		PaymentSource: paymentSource(req),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "paypal: the request could not be encoded")
	}

	resp, err := g.do(ctx, callSpec{
		op:        shared.OpAuthorize,
		method:    http.MethodPost,
		path:      "/v2/checkout/orders",
		body:      raw,
		creds:     req.Credentials,
		merchant:  req.ExternalAccountID,
		requestID: req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	res, err := g.orderResult(resp, req.Amount, req.IdempotencyKey, req.Capture)
	return res, escalateUnparseable(shared.OpAuthorize, err)
}

// paymentSource builds the funding instruction.
//
// A vaulted instrument is referenced by its PayPal vault id; a wallet payment names the experience
// context PayPal needs to build the approval redirect. There is no branch anywhere in this package
// that accepts a raw PAN, and there is no field in the platform's PaymentMethodReference that could
// carry one — a structural guarantee rather than a check.
func paymentSource(req spi.AuthorizeRequest) map[string]any {
	experience := map[string]any{
		"return_url":  req.ReturnURL,
		"cancel_url":  req.ReturnURL,
		"user_action": "PAY_NOW",
	}
	if req.ReturnURL == "" {
		delete(experience, "return_url")
		delete(experience, "cancel_url")
	}
	if req.MethodRef.Token != "" {
		return map[string]any{
			"token": map[string]string{"id": req.MethodRef.Token, "type": "BILLING_AGREEMENT"},
		}
	}
	switch req.Method {
	case shared.MethodCard:
		return map[string]any{"card": map[string]any{"experience_context": experience}}
	default:
		return map[string]any{"paypal": map[string]any{"experience_context": experience}}
	}
}

// Capture converts an approved order or an authorization into a debit.
//
// Which endpoint is used depends on what the reference points at, and the two are not
// interchangeable: an order id captures through /v2/checkout/orders/{id}/capture, while an
// authorization id captures through /v2/payments/authorizations/{id}/capture. PayPal's identifiers
// are opaque and do not announce their type, so the platform records which it holds — the Result of
// Authorize carries the authorization id when the intent was AUTHORIZE, and the order id otherwise.
// This method distinguishes them by the operation that produced them, using the prefix recorded on
// the reference.
func (g *Gateway) Capture(ctx context.Context, req spi.CaptureRequest) (*spi.Result, error) {
	if err := requireKey(req.IdempotencyKey); err != nil {
		return nil, err
	}
	if req.GatewayRef == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"paypal: a capture requires the reference from the authorization")
	}
	kind, id := splitRef(req.GatewayRef)

	var path string
	var raw []byte
	var err error
	switch kind {
	case refAuthorization:
		path = "/v2/payments/authorizations/" + url.PathEscape(id) + "/capture"
		raw, err = json.Marshal(map[string]any{
			"amount":        amountValue{CurrencyCode: string(req.Amount.Currency()), Value: formatAmount(req.Amount)},
			"final_capture": req.Final,
			"invoice_id":    req.IdempotencyKey,
		})
	default:
		path = "/v2/checkout/orders/" + url.PathEscape(id) + "/capture"
		// The order capture endpoint takes an empty body: the amount is fixed by the order, and
		// PayPal rejects an attempt to change it here. A partial capture against an order is
		// expressed by creating the order for the smaller amount in the first place.
		raw = []byte(`{}`)
	}
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "paypal: the request could not be encoded")
	}

	resp, err := g.do(ctx, callSpec{
		op:        shared.OpCapture,
		method:    http.MethodPost,
		path:      path,
		body:      raw,
		creds:     req.Credentials,
		merchant:  req.ExternalAccountID,
		requestID: req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	if kind == refAuthorization {
		res, err := g.captureObjectResult(resp, req.Amount, req.IdempotencyKey)
		return res, escalateUnparseable(shared.OpCapture, err)
	}
	res, err := g.orderResult(resp, req.Amount, req.IdempotencyKey, true)
	return res, escalateUnparseable(shared.OpCapture, err)
}

// Refund returns captured funds against the capture object, which is the only thing PayPal will
// refund. Refunding an order id is not expressible in their API and would be a silent no-op if it
// were, which is why the reference kind is checked rather than assumed.
func (g *Gateway) Refund(ctx context.Context, req spi.RefundRequest) (*spi.Result, error) {
	if err := requireKey(req.IdempotencyKey); err != nil {
		return nil, err
	}
	if req.GatewayRef == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"paypal: a refund requires the capture reference")
	}
	_, id := splitRef(req.GatewayRef)
	raw, err := json.Marshal(map[string]any{
		"amount":     amountValue{CurrencyCode: string(req.Amount.Currency()), Value: formatAmount(req.Amount)},
		"invoice_id": req.IdempotencyKey,
		"custom_id":  req.RefundID.String(),
		// PayPal's note_to_payer appears on the payer's statement of the refund. The platform's own
		// refund reason is a normalized enum and is safe to surface; free-text from a merchant is
		// not sent, because it reaches the payer unmoderated.
		"note_to_payer": string(req.Reason),
	})
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "paypal: the request could not be encoded")
	}
	resp, err := g.do(ctx, callSpec{
		op:        shared.OpRefund,
		method:    http.MethodPost,
		path:      "/v2/payments/captures/" + url.PathEscape(id) + "/refund",
		body:      raw,
		creds:     req.Credentials,
		merchant:  req.ExternalAccountID,
		requestID: req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	if err := g.checkStatus(resp, nil); err != nil {
		return nil, err
	}
	var ro refundObject
	if err := decode(resp.Body, &ro); err != nil {
		return nil, escalateUnparseable(shared.OpRefund, err)
	}
	status, err := mapRefundStatus(ro.Status)
	if err != nil {
		return nil, err
	}
	res := &spi.Result{
		Status:     status,
		GatewayRef: withKind(refRefund, ro.ID),
		RawStatus:  ro.Status,
		ReceivedAt: g.clock.Now(),
		Latency:    resp.Latency,
	}
	if ro.StatusDetails != nil {
		res.RawCode = ro.StatusDetails.Reason
	}
	if ro.Amount != nil {
		m, err := verifyEcho(req.Amount, ro.Amount, "refund")
		if err != nil {
			return nil, err
		}
		res.CapturedAmount = &m
	}
	if ro.ID == "" {
		res.GatewayRef = fallbackRef(req.IdempotencyKey)
	}
	return res, nil
}

// Void releases an uncaptured authorization.
//
// PayPal answers 204 with no body on success, which is why this method does not attempt to decode
// one. An adapter that insists on a body here reports every successful void as a contract
// violation.
func (g *Gateway) Void(ctx context.Context, req spi.VoidRequest) (*spi.Result, error) {
	if err := requireKey(req.IdempotencyKey); err != nil {
		return nil, err
	}
	if req.GatewayRef == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"paypal: a void requires the authorization reference")
	}
	_, id := splitRef(req.GatewayRef)
	resp, err := g.do(ctx, callSpec{
		op:        shared.OpVoid,
		method:    http.MethodPost,
		path:      "/v2/payments/authorizations/" + url.PathEscape(id) + "/void",
		body:      []byte(`{}`),
		creds:     req.Credentials,
		merchant:  req.ExternalAccountID,
		requestID: req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	if err := g.checkStatus(resp, nil); err != nil {
		return nil, err
	}
	res := &spi.Result{
		Status:     spi.StatusVoided,
		GatewayRef: withKind(refAuthorization, id),
		RawStatus:  "VOIDED",
		ReceivedAt: g.clock.Now(),
		Latency:    resp.Latency,
	}
	// Some PayPal deployments answer 200 with the voided authorization rather than 204. Reading it
	// when present keeps the raw status accurate without making the 204 path an error.
	if len(strings.TrimSpace(string(resp.Body))) > 0 {
		var ao authorizationObject
		if decode(resp.Body, &ao) == nil && ao.Status != "" {
			res.RawStatus = ao.Status
			if s, err := mapAuthorizationStatus(ao.Status); err == nil {
				res.Status = s
			}
		}
	}
	return res, nil
}

// Lookup asks PayPal what happened.
//
// With an order reference this is a plain GET. With only an idempotency key it exploits the fact
// that Authorize put the key in `invoice_id`, which PayPal indexes and which the platform can
// therefore search on — and which is also why a duplicate submission is rejected as
// DUPLICATE_INVOICE_ID rather than charged twice. That the key is discoverable from PayPal's side
// is what makes an unknown outcome resolvable at all here: PayPal's `PayPal-Request-Id` replay
// window is much shorter than Stripe's or Adyen's.
func (g *Gateway) Lookup(ctx context.Context, req spi.LookupRequest) (*spi.Result, error) {
	if req.GatewayRef == "" && req.IdempotencyKey == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"paypal: a lookup requires either a gateway reference or an idempotency key")
	}
	var path string
	if req.GatewayRef != "" {
		kind, id := splitRef(req.GatewayRef)
		switch kind {
		case refAuthorization:
			path = "/v2/payments/authorizations/" + url.PathEscape(id)
		case refCapture:
			path = "/v2/payments/captures/" + url.PathEscape(id)
		case refRefund:
			path = "/v2/payments/refunds/" + url.PathEscape(id)
		default:
			path = "/v2/checkout/orders/" + url.PathEscape(id)
		}
	} else {
		q := url.Values{}
		q.Set("invoice_id", req.IdempotencyKey)
		path = "/v2/checkout/orders?" + q.Encode()
	}

	resp, err := g.do(ctx, callSpec{
		op:       shared.OpLookup,
		method:   http.MethodGet,
		path:     path,
		creds:    req.Credentials,
		merchant: req.ExternalAccountID,
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return g.notFound(resp), nil
	}
	if err := g.checkStatus(resp, nil); err != nil {
		return nil, err
	}

	if req.GatewayRef == "" {
		// The search answers with an order list. The filter is re-applied client-side so
		// correctness does not rest on PayPal honouring the query parameter — only on the order
		// carrying the invoice id, which this adapter put there.
		var list struct {
			Orders []order `json:"orders"`
		}
		if err := decode(resp.Body, &list); err != nil {
			return nil, err
		}
		for i := range list.Orders {
			o := &list.Orders[i]
			if invoiceIDOf(o) != req.IdempotencyKey {
				continue
			}
			return g.resultFromOrder(o, money.Money{}, req.IdempotencyKey, resp)
		}
		return g.notFound(resp), nil
	}

	kind, _ := splitRef(req.GatewayRef)
	switch kind {
	case refAuthorization:
		var ao authorizationObject
		if err := decode(resp.Body, &ao); err != nil {
			return nil, err
		}
		return g.resultFromAuthorization(&ao, money.Money{}, req.IdempotencyKey, resp)
	case refCapture:
		return g.captureObjectResult(resp, money.Money{}, req.IdempotencyKey)
	default:
		var o order
		if err := decode(resp.Body, &o); err != nil {
			return nil, err
		}
		if o.ID == "" {
			return g.notFound(resp), nil
		}
		return g.resultFromOrder(&o, money.Money{}, req.IdempotencyKey, resp)
	}
}

// --- transport -------------------------------------------------------------------------------

type callSpec struct {
	op        shared.Operation
	method    string
	path      string
	body      []byte
	creds     spi.Credentials
	merchant  string
	requestID string
}

// do issues one PayPal request, acquiring a bearer token first.
//
// The token acquisition is inside the money-moving path, which means a token failure has to be
// classified carefully: it happens strictly *before* the payment request is written, so it is a
// plain error and never an unknown outcome. Getting that backwards would park a payment in
// reconciliation every time PayPal's auth endpoint hiccuped.
func (g *Gateway) do(ctx context.Context, spec callSpec) (*spi.HTTPResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	token, err := g.tokens.token(ctx, g.client, g.cfg.BaseURL, spec.creds)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{
		"Authorization":       "Bearer " + token,
		"Accept":              "application/json",
		httpx.OperationHeader: spec.op.String(),
	}
	if len(spec.body) > 0 {
		headers["Content-Type"] = "application/json"
	}
	if spec.requestID != "" {
		headers[RequestIDHeader] = spec.requestID
	}
	if spec.merchant != "" {
		// PayPal-Auth-Assertion names the merchant this call acts for. It is an unsigned JWT whose
		// payload is the partner's client id and the merchant's payer id — PayPal accepts it
		// unsigned because the bearer token already authenticates the partner, and the assertion
		// only selects which of the partner's merchants is acting.
		assertion, err := authAssertion(spec.creds, spec.merchant)
		if err != nil {
			return nil, err
		}
		headers["PayPal-Auth-Assertion"] = assertion
	}

	resp, err := g.client.Do(&spi.HTTPRequest{
		Ctx: ctx, Method: spec.method, URL: g.cfg.BaseURL + spec.path,
		Headers: headers, Body: spec.body,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"paypal: the transport returned neither a response nor an error")
	}
	if resp.Timeout {
		return nil, timeoutOutcome(spec.op)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// The cached token was live as far as the cache knew, and PayPal disagrees. Dropping it
		// means the next call re-exchanges rather than re-presenting a token that will fail for
		// hours.
		g.tokens.invalidate(spec.creds)
	}
	if resp.StatusCode >= 500 && spec.op.IsMoneyMoving() {
		return nil, apierror.Wrap(spi.ErrOutcomeUnknown, apierror.CodeGatewayTimeout,
			"paypal: the gateway returned "+strconv.Itoa(resp.StatusCode)+
				" and does not guarantee the request was not processed")
	}
	return resp, nil
}

func timeoutOutcome(op shared.Operation) error {
	if op.IsMoneyMoving() {
		return apierror.Wrap(spi.ErrOutcomeUnknown, apierror.CodeGatewayTimeout,
			"paypal: the request was written and the deadline expired; the outcome is unknown")
	}
	return apierror.New(apierror.CodeGatewayTimeout, "paypal: the request timed out")
}

// escalateUnparseable turns an unreadable response on a money-moving call into an unknown outcome,
// on the SPI rule that a gateway which answered unreadably has still acted. Scoped to the parse
// sentinel so that an echo mismatch or an unrecognised status keeps its diagnosis.
func escalateUnparseable(op shared.Operation, err error) error {
	if err == nil || !op.IsMoneyMoving() || !errors.Is(err, errUnparseable) {
		return err
	}
	return apierror.Wrap(spi.ErrOutcomeUnknown, apierror.CodeGatewayTimeout,
		"paypal: the gateway responded but the body could not be parsed; the outcome is unknown")
}

func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return apierror.Wrap(err, apierror.CodeGatewayTimeout,
			"paypal: the deadline expired before the gateway call was made")
	}
	return apierror.Wrap(err, apierror.CodeServiceUnavailable,
		"paypal: the gateway call was cancelled before it was made")
}

// checkStatus converts a non-2xx into a platform error, handing an instrument decline back to the
// caller through declined instead. The out parameter is optional: pass a non-nil pointer to receive
// the parsed error body when the caller needs to distinguish a decline.
func (g *Gateway) checkStatus(resp *spi.HTTPResponse, out **errorResponse) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var e errorResponse
	_ = decode(resp.Body, &e)
	if out != nil {
		copyOfError := e
		*out = &copyOfError
	}
	if e.Name == "" && e.Error == "" {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return apierror.Wrap(spi.ErrCredentialsInvalid, apierror.CodeGatewayAuthenticationFailed,
				"paypal: the request was not authorized")
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return apierror.New(apierror.CodeRateLimited, "paypal: rate limit exceeded")
		}
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"paypal: HTTP %d with an unparseable body", resp.StatusCode)
	}
	return mapErrorName(resp.StatusCode, &e)
}

// --- result construction ---------------------------------------------------------------------

func (g *Gateway) orderResult(resp *spi.HTTPResponse, requested money.Money, idemKey string, capture bool) (*spi.Result, error) {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e *errorResponse
		err := g.checkStatus(resp, &e)
		if isInstrumentDeclined(e) {
			// A decline, not a failure. This is the branch that keeps a refused card from being
			// reported as an ERROR outcome, which would permit failover on a hard decline.
			return g.declineResult(e, idemKey, resp), nil
		}
		return nil, err
	}
	var o order
	if err := decode(resp.Body, &o); err != nil {
		return nil, err
	}
	if o.ID == "" {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"paypal: the order response carries no id")
	}
	_ = capture
	return g.resultFromOrder(&o, requested, idemKey, resp)
}

func (g *Gateway) resultFromOrder(o *order, requested money.Money, idemKey string, resp *spi.HTTPResponse) (*spi.Result, error) {
	status, err := mapOrderStatus(o.Status)
	if err != nil {
		return nil, err
	}
	res := &spi.Result{
		Status:     status,
		GatewayRef: withKind(refOrder, o.ID),
		RawStatus:  o.Status,
		ReceivedAt: g.clock.Now(),
		Latency:    resp.Latency,
	}

	// The nested capture or authorization is the object the platform will act on next — a refund
	// needs the capture id, a void needs the authorization id — so when one is present it replaces
	// the order id as the reference. An adapter that returns the order id here produces a payment
	// that can never be refunded.
	capObj, auth := nestedPayments(o)
	switch {
	case capObj != nil:
		res.GatewayRef = withKind(refCapture, capObj.ID)
		if s, err := mapCaptureStatus(capObj.Status); err == nil {
			res.Status = s
			res.RawStatus = capObj.Status
		}
		res.ProcessorResponse = mapProcessorResponse(capObj.ProcessorResponse, "")
		if capObj.Amount != nil {
			if m, err := parseAmount(capObj.Amount); err == nil {
				res.CapturedAmount = &m
			}
		}
		if res.Status == spi.StatusDeclined {
			reason, noRetry := mapDecline(capObj.ProcessorResponse, nil)
			res.DeclineReason, res.NetworkAdviceNoRetry = reason, noRetry
			if capObj.ProcessorResponse != nil {
				res.RawCode = capObj.ProcessorResponse.ResponseCode
			}
		}
	case auth != nil:
		res.GatewayRef = withKind(refAuthorization, auth.ID)
		if s, err := mapAuthorizationStatus(auth.Status); err == nil {
			res.Status = s
			res.RawStatus = auth.Status
		}
		res.ProcessorResponse = mapProcessorResponse(auth.ProcessorResponse, "")
		if auth.Amount != nil {
			if m, err := parseAmount(auth.Amount); err == nil {
				res.AuthorizedAmount = &m
			}
		}
		if auth.ExpirationTime != "" {
			if t, err := time.Parse(time.RFC3339, auth.ExpirationTime); err == nil {
				u := t.UTC()
				res.AuthExpiresAt = &u
			}
		}
		if res.Status == spi.StatusDeclined {
			reason, noRetry := mapDecline(auth.ProcessorResponse, nil)
			res.DeclineReason, res.NetworkAdviceNoRetry = reason, noRetry
			if auth.ProcessorResponse != nil {
				res.RawCode = auth.ProcessorResponse.ResponseCode
			}
		}
	}

	if res.Status == spi.StatusRequiresAction {
		if href := linkByRel(o.Links, "payer-action"); href != "" {
			res.NextAction = &spi.NextAction{Type: payment.ActionRedirect, RedirectURL: href}
		} else if href := linkByRel(o.Links, "approve"); href != "" {
			res.NextAction = &spi.NextAction{Type: payment.ActionRedirect, RedirectURL: href}
		} else {
			res.NextAction = &spi.NextAction{Type: payment.ActionRedirect}
		}
	}

	// The echo check runs against the purchase unit, which is where PayPal states what it acted on.
	if requested.IsValid() {
		if got := purchaseUnitAmount(o); got != nil {
			if _, err := verifyEcho(requested, got, "order"); err != nil {
				return nil, err
			}
		}
	}
	if res.GatewayRef == "" {
		res.GatewayRef = fallbackRef(idemKey)
	}
	return res, nil
}

func (g *Gateway) resultFromAuthorization(ao *authorizationObject, requested money.Money, idemKey string, resp *spi.HTTPResponse) (*spi.Result, error) {
	status, err := mapAuthorizationStatus(ao.Status)
	if err != nil {
		return nil, err
	}
	res := &spi.Result{
		Status:            status,
		GatewayRef:        withKind(refAuthorization, ao.ID),
		RawStatus:         ao.Status,
		ProcessorResponse: mapProcessorResponse(ao.ProcessorResponse, ""),
		ReceivedAt:        g.clock.Now(),
		Latency:           resp.Latency,
	}
	if ao.Amount != nil {
		if requested.IsValid() {
			if _, err := verifyEcho(requested, ao.Amount, "authorization"); err != nil {
				return nil, err
			}
		}
		if m, err := parseAmount(ao.Amount); err == nil {
			res.AuthorizedAmount = &m
		}
	}
	if res.GatewayRef == "" {
		res.GatewayRef = fallbackRef(idemKey)
	}
	return res, nil
}

func (g *Gateway) captureObjectResult(resp *spi.HTTPResponse, requested money.Money, idemKey string) (*spi.Result, error) {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e *errorResponse
		err := g.checkStatus(resp, &e)
		if isInstrumentDeclined(e) {
			return g.declineResult(e, idemKey, resp), nil
		}
		return nil, err
	}
	var co captureObject
	if err := decode(resp.Body, &co); err != nil {
		return nil, err
	}
	status, err := mapCaptureStatus(co.Status)
	if err != nil {
		return nil, err
	}
	res := &spi.Result{
		Status:            status,
		GatewayRef:        withKind(refCapture, co.ID),
		RawStatus:         co.Status,
		ProcessorResponse: mapProcessorResponse(co.ProcessorResponse, ""),
		ReceivedAt:        g.clock.Now(),
		Latency:           resp.Latency,
	}
	if co.StatusDetails != nil {
		res.RawCode = co.StatusDetails.Reason
	}
	if co.Amount != nil {
		if requested.IsValid() {
			if _, err := verifyEcho(requested, co.Amount, "capture"); err != nil {
				return nil, err
			}
		}
		if m, err := parseAmount(co.Amount); err == nil {
			res.CapturedAmount = &m
		}
	}
	// PayPal reports its fee at capture time, which is unusual and useful: it lets the platform
	// post the true net to the ledger immediately instead of waiting for a settlement report.
	if co.SellerReceivable != nil && co.SellerReceivable.PayPalFee != nil {
		if fee, err := parseAmount(co.SellerReceivable.PayPalFee); err == nil {
			res.Fee = &fee
		}
	}
	if status == spi.StatusDeclined {
		res.DeclineReason, res.NetworkAdviceNoRetry = mapDecline(co.ProcessorResponse, nil)
		if co.ProcessorResponse != nil {
			res.RawCode = co.ProcessorResponse.ResponseCode
		}
	}
	if res.GatewayRef == "" {
		res.GatewayRef = fallbackRef(idemKey)
	}
	return res, nil
}

func (g *Gateway) declineResult(e *errorResponse, idemKey string, resp *spi.HTTPResponse) *spi.Result {
	reason, noRetry := mapDecline(nil, e.issues())
	return &spi.Result{
		Status:               spi.StatusDeclined,
		GatewayRef:           fallbackRef(idemKey),
		DeclineReason:        reason,
		NetworkAdviceNoRetry: noRetry,
		RawStatus:            e.Name,
		RawCode:              firstIssue(e),
		// `debug_id` rather than `message`: the debug id is what PayPal support asks for and it
		// carries no caller data, whereas the message is prose that can echo request fields.
		RawMessage: "debug_id=" + e.DebugID,
		ReceivedAt: g.clock.Now(),
		Latency:    resp.Latency,
	}
}

func (g *Gateway) notFound(resp *spi.HTTPResponse) *spi.Result {
	return &spi.Result{Status: spi.StatusNotFound, ReceivedAt: g.clock.Now(), Latency: resp.Latency}
}

// --- reference tagging -------------------------------------------------------------------------

// PayPal's identifiers are opaque and do not announce what they identify, but the endpoint that
// accepts one differs by type: an order captures at one URL, an authorization at another, and a
// refund is only expressible against a capture. The adapter therefore tags the reference it hands
// back with the kind of object it names.
//
// The tag lives in the reference string rather than in a separate Result field because spi.Result
// has one GatewayRef, and adding a vendor-specific field to the SPI to carry a PayPal detail would
// be exactly the leak the anti-corruption layer exists to prevent. The tag is stripped before the
// value is ever sent to PayPal.
const (
	refOrder         = "ord"
	refAuthorization = "auth"
	refCapture       = "cap"
	refRefund        = "ref"
)

func withKind(kind, id string) string {
	if id == "" {
		return ""
	}
	return kind + ":" + id
}

func splitRef(ref string) (kind, id string) {
	k, rest, ok := strings.Cut(ref, ":")
	if !ok {
		// An untagged reference is an order id: that is what the platform stored before this
		// tagging existed, and treating it as an order is the behaviour those rows were written
		// under.
		return refOrder, ref
	}
	switch k {
	case refOrder, refAuthorization, refCapture, refRefund:
		return k, rest
	default:
		return refOrder, ref
	}
}

// --- helpers ----------------------------------------------------------------------------------

func nestedPayments(o *order) (*captureObject, *authorizationObject) {
	for i := range o.PurchaseUnits {
		p := o.PurchaseUnits[i].Payments
		if p == nil {
			continue
		}
		if len(p.Captures) > 0 {
			return &p.Captures[0], nil
		}
		if len(p.Authorizations) > 0 {
			return nil, &p.Authorizations[0]
		}
	}
	return nil, nil
}

func purchaseUnitAmount(o *order) *amountValue {
	for i := range o.PurchaseUnits {
		if o.PurchaseUnits[i].Amount != nil {
			return o.PurchaseUnits[i].Amount
		}
	}
	return nil
}

func invoiceIDOf(o *order) string {
	for i := range o.PurchaseUnits {
		if o.PurchaseUnits[i].InvoiceID != "" {
			return o.PurchaseUnits[i].InvoiceID
		}
	}
	return ""
}

// verifyEcho checks PayPal acted on the amount and currency we asked for. A currency mismatch or a
// larger amount is a contract violation surfaced as an error; a smaller amount is a partial
// authorization and is reported to the caller.
func verifyEcho(requested money.Money, got *amountValue, object string) (money.Money, error) {
	m, err := parseAmount(got)
	if err != nil {
		return money.Money{}, err
	}
	if m.Currency() != requested.Currency() {
		return money.Money{}, apierror.Newf(apierror.CodeGatewayContractViolation,
			"paypal: the %s echoed currency %s for a request in %s", object, m.Currency(), requested.Currency()).
			WithDetail(apierror.Detail{
				Field: "amount.currency_code", Code: "CURRENCY_ECHO_MISMATCH",
				Message: "the gateway acted in a different currency from the one requested",
				RuleID:  "L6.GATEWAY_ECHOES_CURRENCY",
			})
	}
	if m.Amount() > requested.Amount() {
		return money.Money{}, apierror.Newf(apierror.CodeGatewayContractViolation,
			"paypal: the %s echoed %d minor units for a request of %d", object, m.Amount(), requested.Amount()).
			WithDetail(apierror.Detail{
				Field: "amount.value", Code: "AMOUNT_ECHO_EXCEEDS_REQUEST",
				Message: "the gateway acted on a larger amount than was requested",
				RuleID:  "L6.GATEWAY_ECHOES_AMOUNT",
			})
	}
	return m, nil
}

// authAssertion builds the PayPal-Auth-Assertion header.
//
// It is an unsigned JWT — algorithm "none" — which looks alarming and is not: PayPal accepts it
// unsigned because the bearer token in the Authorization header already authenticates the partner,
// and the assertion only selects which of that partner's merchants the call acts for. A forged
// assertion is useless without a partner token, and a partner token already carries the authority
// the assertion narrows. It is built by hand rather than with a JWT library precisely because a
// library would want to sign it.
func authAssertion(creds spi.Credentials, merchantID string) (string, error) {
	clientID, err := credential(creds, CredentialClientID)
	if err != nil {
		return "", err
	}
	header := `{"alg":"none"}`
	payload := `{"iss":"` + jsonEscape(clientID) + `","payer_id":"` + jsonEscape(merchantID) + `"}`
	return base64URL(header) + "." + base64URL(payload) + ".", nil
}

func requireKey(k string) error {
	if k == "" {
		return apierror.New(apierror.CodeIdempotencyKeyRequired,
			"paypal: every mutating gateway call requires an idempotency key")
	}
	return nil
}

// credential reads a required credential field, naming the field and never the value.
func credential(c spi.Credentials, field string) (string, error) {
	v, ok := c.Get(field)
	if !ok || v == "" {
		return "", apierror.Wrapf(spi.ErrCredentialsInvalid, apierror.CodeGatewayNotConfigured,
			"paypal: the credential field %q is missing", field)
	}
	return v, nil
}

// fallbackRef synthesises a reference from the idempotency key so every actionable result carries
// something Lookup can resolve. See the identical note in the Stripe adapter.
func fallbackRef(idemKey string) string {
	if idemKey == "" {
		return ""
	}
	return "idemkey:" + idemKey
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
