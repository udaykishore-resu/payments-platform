// Package adyen is the Adyen adapter.
//
// Adyen differs from Stripe in three ways that shape everything below, and each one is a place an
// integration goes wrong quietly rather than loudly:
//
//  1. **A refusal is HTTP 200.** Adyen answers a declined card with `{"resultCode":"Refused"}` and
//     a 200, not with an error status. An adapter that branches on the HTTP status will report
//     every decline as a success. The decline path here reads the body of a 2xx.
//  2. **Every modification is asynchronous.** Capture, refund and cancel all answer
//     `{"status":"received"}` and the real outcome arrives later as a notification. Reporting a
//     capture as final because the call returned 200 is how a platform tells a merchant they have
//     been paid before Adyen has even tried.
//  3. **The webhook HMAC covers a pipe-delimited projection of the event, not the body.** The
//     signed string is built from eight named fields with a specific escaping rule, and getting
//     that rule wrong produces a verifier that works on every test payload and fails on the first
//     production merchant whose reference contains a colon. See webhook.go.
//
// Everything Adyen-shaped stops here. Above this package the platform sees only spi.Result.
package adyen

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
const GatewayID shared.GatewayID = "adyen"

// Credential field names as they appear in the resolved spi.Credentials map.
//
// Adyen's model is the opposite of Stripe's: the platform holds a *per-connection* API key rather
// than one platform key plus an account header. That spreads blast radius — a leaked key exposes
// one merchant — at the cost of a rotation workflow that has to walk every connection.
const (
	// CredentialAPIKey is the value sent in the X-API-Key header.
	CredentialAPIKey = "api_key"
	// CredentialMerchantAccount is the Adyen merchant account code. It is not a secret, but it is
	// carried alongside the key because the two are only ever valid together.
	CredentialMerchantAccount = "merchant_account"
	// CredentialBasicAuthUser and CredentialBasicAuthPassword authenticate Adyen *to us* on the
	// notification endpoint. See Verifier.VerifyBasicAuth.
	CredentialBasicAuthUser     = "webhook_basic_user"
	CredentialBasicAuthPassword = "webhook_basic_password"
)

// DefaultAPIVersion is the Checkout API version this adapter targets. Adyen embeds the version in
// the path rather than in a header, so pinning it means pinning a URL prefix.
const DefaultAPIVersion = "v71"

// Metadata keys stamped on every payment, so reconciliation can find a transaction from our side
// of the relationship without a gateway reference.
const (
	metaPaymentID      = "pp_payment_id"
	metaAttemptID      = "pp_attempt_id"
	metaRefundID       = "pp_refund_id"
	metaIdempotencyKey = "pp_idempotency_key"
)

// Gateway is the Adyen implementation of spi.PaymentGateway.
type Gateway struct {
	cfg    spi.Config
	client spi.HTTPDoer
	clock  shared.Clock
}

var _ spi.PaymentGateway = (*Gateway)(nil)

// NewGateway builds the payment-path adapter.
func NewGateway(cfg spi.Config) (*Gateway, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Gateway{cfg: cfg, client: cfg.HTTPClient, clock: cfg.Clock}, nil
}

func normalizeConfig(cfg spi.Config) (spi.Config, error) {
	if cfg.BaseURL == "" {
		return cfg, apierror.New(apierror.CodeGatewayNotConfigured,
			"adyen: a base URL is required; Adyen issues a per-company live endpoint prefix and there is no safe default")
	}
	if cfg.HTTPClient == nil {
		return cfg, apierror.New(apierror.CodeGatewayNotConfigured,
			"adyen: an HTTP client must be injected so the adapter runs inside the resilience envelope")
	}
	if !cfg.Environment.IsValid() {
		return cfg, apierror.New(apierror.CodeGatewayNotConfigured,
			"adyen: the environment must be sandbox or production")
	}
	if cfg.Clock == nil {
		cfg.Clock = shared.SystemClock{}
	}
	if cfg.APIVersion == "" {
		cfg.APIVersion = DefaultAPIVersion
	}
	if cfg.WebhookTolerance <= 0 {
		cfg.WebhookTolerance = 5 * time.Minute
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return cfg, nil
}

// ID returns the registry slug.
func (g *Gateway) ID() shared.GatewayID { return GatewayID }

// authorizeRequestBody is the /payments request.
//
// It is a typed struct rather than a map so the field names are checked by the compiler. Adyen
// ignores unknown request fields silently, which means a typo in a map key produces a payment that
// is missing `recurringProcessingModel` — and therefore mispriced and, in the EEA, missing its SCA
// exemption — with no error anywhere.
type authorizeRequestBody struct {
	Reference                string            `json:"reference"`
	Amount                   amount            `json:"amount"`
	PaymentMethod            map[string]string `json:"paymentMethod"`
	MerchantAccount          string            `json:"merchantAccount"`
	ShopperInteraction       string            `json:"shopperInteraction,omitempty"`
	RecurringProcessingModel string            `json:"recurringProcessingModel,omitempty"`
	ReturnURL                string            `json:"returnUrl,omitempty"`
	ShopperReference         string            `json:"shopperReference,omitempty"`
	ShopperEmail             string            `json:"shopperEmail,omitempty"`
	ShopperIP                string            `json:"shopperIP,omitempty"`
	CountryCode              string            `json:"countryCode,omitempty"`
	ShopperStatement         string            `json:"shopperStatement,omitempty"`
	CaptureDelayHours        *int              `json:"captureDelayHours,omitempty"`
	AdditionalData           map[string]string `json:"additionalData,omitempty"`
	Metadata                 map[string]string `json:"metadata,omitempty"`
	Channel                  string            `json:"channel,omitempty"`
	Origin                   string            `json:"origin,omitempty"`
	AuthenticationData       *authData         `json:"authenticationData,omitempty"`
}

type authData struct {
	AttemptAuthentication string `json:"attemptAuthentication,omitempty"`
	ThreeDSRequestData    *struct {
		NativeThreeDS string `json:"nativeThreeDS,omitempty"`
	} `json:"threeDSRequestData,omitempty"`
}

// Authorize creates a payment.
//
// `shopperInteraction` and `recurringProcessingModel` are sent explicitly on every call. They are
// not optional in practice: together they tell the scheme whether this is a cardholder-initiated
// transaction or a merchant-initiated one, which determines the interchange rate, whether an SCA
// exemption applies in the EEA, and whether the issuer expects authentication data. Omitting them
// gets the payment authorized at a worse rate and, in the EEA, soft-declined for authentication
// that nobody is present to perform.
//
// Manual capture is requested through `additionalData.manualCapture` rather than by leaving
// `captureDelayHours` unset, because the account-level default is "capture immediately" and an
// omitted parameter inherits it — silently turning every authorization-only payment into a sale.
func (g *Gateway) Authorize(ctx context.Context, req spi.AuthorizeRequest) (*spi.Result, error) {
	if err := requireKey(req.IdempotencyKey); err != nil {
		return nil, err
	}
	if !req.Amount.IsValid() {
		return nil, apierror.New(apierror.CodeAmountInvalid,
			"adyen: the amount carries an unsupported currency")
	}
	merchantAccount, err := credential(req.Credentials, CredentialMerchantAccount)
	if err != nil {
		return nil, err
	}

	body := authorizeRequestBody{
		Reference:       referenceOr(req.Reference, req.PaymentID.String()),
		Amount:          amount{Value: req.Amount.Amount(), Currency: string(req.Amount.Currency())},
		PaymentMethod:   paymentMethodPayload(req),
		MerchantAccount: merchantAccount,
		ReturnURL:       req.ReturnURL,
		ShopperEmail:    "",
		ShopperIP:       req.Customer.IPAddress,
		CountryCode:     string(req.Customer.Country),
		// Adyen truncates the shopper statement itself, but truncating here keeps what the merchant
		// sees on the payment identical to what the cardholder sees on their statement.
		ShopperStatement: truncate(firstNonEmpty(req.StatementRef, req.Descriptor), 22),
		Metadata: mergedMetadata(req.Metadata, map[string]string{
			metaPaymentID:      req.PaymentID.String(),
			metaAttemptID:      req.AttemptID.String(),
			metaIdempotencyKey: req.IdempotencyKey,
		}),
		AdditionalData: map[string]string{},
	}
	// The shopper reference must never be the raw customer identifier: Adyen stores it and it
	// appears in their console. The platform's own opaque customer id is used where present.
	if req.Customer.ID != "" {
		body.ShopperReference = req.Customer.ID
	}
	if req.ReturnURL != "" || req.ThreeDS.Requested {
		body.ShopperInteraction = "Ecommerce"
		body.RecurringProcessingModel = "CardOnFile"
	} else {
		// No return URL and no challenge requested means the payer is not present. Declaring it as
		// a merchant-initiated transaction is what stops the issuer soft-declining for
		// authentication that nobody can complete.
		body.ShopperInteraction = "ContAuth"
		body.RecurringProcessingModel = "UnscheduledCardOnFile"
	}
	if req.Capture {
		zero := 0
		body.CaptureDelayHours = &zero
	} else {
		body.AdditionalData["manualCapture"] = "true"
	}
	if req.ThreeDS.Requested {
		body.AuthenticationData = &authData{AttemptAuthentication: "always"}
	}
	if req.ThreeDS.ExemptionType != "" {
		// Recording the claimed exemption on the request is what makes it defensible later. An
		// exemption applied but not recorded is one the platform cannot prove it was entitled to.
		body.AdditionalData["scaExemption"] = req.ThreeDS.ExemptionType
	}
	if len(body.AdditionalData) == 0 {
		body.AdditionalData = nil
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "adyen: the request could not be encoded")
	}
	resp, err := g.do(ctx, callSpec{
		op:             shared.OpAuthorize,
		method:         http.MethodPost,
		path:           "/" + g.cfg.APIVersion + "/payments",
		body:           raw,
		creds:          req.Credentials,
		idempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	if err := g.checkStatus(resp); err != nil {
		return nil, err
	}
	var pr paymentResponse
	if err := decode(resp.Body, &pr); err != nil {
		return nil, escalateUnparseable(shared.OpAuthorize, err)
	}
	res, err := g.paymentResult(&pr, req.Amount, req.Capture, req.IdempotencyKey, resp)
	return res, escalateUnparseable(shared.OpAuthorize, err)
}

// paymentMethodPayload builds Adyen's paymentMethod object.
//
// The token from client-side tokenization is a `storedPaymentMethodId` for a saved instrument and
// an encrypted blob for a fresh one; Adyen accepts the stored id under that key with
// `type: scheme`. No branch here ever handles a raw PAN, and there is no field in the platform's
// PaymentMethodReference that could hold one — that is a structural guarantee, not a check.
func paymentMethodPayload(req spi.AuthorizeRequest) map[string]string {
	pm := map[string]string{"type": adyenMethodType(req.Method)}
	if req.MethodRef.Token != "" {
		pm["storedPaymentMethodId"] = req.MethodRef.Token
	}
	return pm
}

// adyenMethodType maps the platform's coarse payment methods onto Adyen's taxonomy.
//
// The platform's categories are deliberately coarse (see shared.PaymentMethod); Adyen's are not.
// An unmapped method falls back to "scheme", which is Adyen's card type, because every method the
// platform models that is not explicitly listed is card-backed.
func adyenMethodType(m shared.PaymentMethod) string {
	switch m {
	case shared.MethodIdeal:
		return "ideal"
	case shared.MethodSofort:
		return "directEbanking"
	case shared.MethodBancontact:
		return "bcmc"
	case shared.MethodSEPADebit:
		return "sepadirectdebit"
	case shared.MethodPayPal:
		return "paypal"
	case shared.MethodApplePay:
		return "applepay"
	case shared.MethodGooglePay:
		return "googlepay"
	case shared.MethodBLIK:
		return "blik"
	case shared.MethodUPI:
		return "upi_collect"
	default:
		return "scheme"
	}
}

type modificationBody struct {
	MerchantAccount string            `json:"merchantAccount"`
	Amount          *amount           `json:"amount,omitempty"`
	Reference       string            `json:"reference,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

// Capture converts a hold into a debit.
//
// Adyen captures against the payment's pspReference and answers with a *new* pspReference for the
// capture itself. The Result carries the capture's reference, not the payment's, because that is
// the identifier the CAPTURE notification will arrive under and therefore the one reconciliation
// has to match on.
func (g *Gateway) Capture(ctx context.Context, req spi.CaptureRequest) (*spi.Result, error) {
	if err := requireKey(req.IdempotencyKey); err != nil {
		return nil, err
	}
	if req.GatewayRef == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"adyen: a capture requires the pspReference from the authorization")
	}
	merchantAccount, err := credential(req.Credentials, CredentialMerchantAccount)
	if err != nil {
		return nil, err
	}
	amt := amount{Value: req.Amount.Amount(), Currency: string(req.Amount.Currency())}
	raw, err := json.Marshal(modificationBody{
		MerchantAccount: merchantAccount,
		Amount:          &amt,
		Reference:       req.PaymentID.String(),
		Metadata: mergedMetadata(req.Metadata, map[string]string{
			metaIdempotencyKey: req.IdempotencyKey,
			metaPaymentID:      req.PaymentID.String(),
		}),
	})
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "adyen: the request could not be encoded")
	}
	return g.modify(ctx, modificationCapture, shared.OpCapture,
		"/"+g.cfg.APIVersion+"/payments/"+url.PathEscape(req.GatewayRef)+"/captures",
		raw, req.Credentials, req.IdempotencyKey, req.Amount)
}

// Refund returns captured funds.
func (g *Gateway) Refund(ctx context.Context, req spi.RefundRequest) (*spi.Result, error) {
	if err := requireKey(req.IdempotencyKey); err != nil {
		return nil, err
	}
	if req.GatewayRef == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"adyen: a refund requires the pspReference from the payment")
	}
	merchantAccount, err := credential(req.Credentials, CredentialMerchantAccount)
	if err != nil {
		return nil, err
	}
	amt := amount{Value: req.Amount.Amount(), Currency: string(req.Amount.Currency())}
	raw, err := json.Marshal(modificationBody{
		MerchantAccount: merchantAccount,
		Amount:          &amt,
		Reference:       req.RefundID.String(),
		Metadata: mergedMetadata(req.Metadata, map[string]string{
			metaIdempotencyKey: req.IdempotencyKey,
			metaRefundID:       req.RefundID.String(),
			metaPaymentID:      req.PaymentID.String(),
			"pp_refund_reason": string(req.Reason),
		}),
	})
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "adyen: the request could not be encoded")
	}
	return g.modify(ctx, modificationRefund, shared.OpRefund,
		"/"+g.cfg.APIVersion+"/payments/"+url.PathEscape(req.GatewayRef)+"/refunds",
		raw, req.Credentials, req.IdempotencyKey, req.Amount)
}

// Void releases an uncaptured authorization.
func (g *Gateway) Void(ctx context.Context, req spi.VoidRequest) (*spi.Result, error) {
	if err := requireKey(req.IdempotencyKey); err != nil {
		return nil, err
	}
	if req.GatewayRef == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"adyen: a void requires the pspReference from the authorization")
	}
	merchantAccount, err := credential(req.Credentials, CredentialMerchantAccount)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(modificationBody{
		MerchantAccount: merchantAccount,
		Reference:       req.PaymentID.String(),
		Metadata: mergedMetadata(req.Metadata, map[string]string{
			metaIdempotencyKey: req.IdempotencyKey,
			metaPaymentID:      req.PaymentID.String(),
		}),
	})
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "adyen: the request could not be encoded")
	}
	// A cancel carries no amount, so there is nothing to echo-check.
	return g.modify(ctx, modificationCancel, shared.OpVoid,
		"/"+g.cfg.APIVersion+"/payments/"+url.PathEscape(req.GatewayRef)+"/cancels",
		raw, req.Credentials, req.IdempotencyKey, money.Money{})
}

// Lookup asks Adyen what happened, by pspReference or by idempotency key alone.
//
// Adyen's own idempotency guarantee is the primary mechanism: replaying a POST with the same
// `Idempotency-Key` returns the stored original response, and Adyen keeps that record far longer
// than Stripe's 24 hours. The key-only path here is the fallback for the case the reconciler
// actually faces — a crash that lost the pspReference *and* an idempotency record that has since
// expired — and it works because Authorize stamps the key into `metadata`, which Adyen returns on
// the payment and exposes to the reporting endpoints.
func (g *Gateway) Lookup(ctx context.Context, req spi.LookupRequest) (*spi.Result, error) {
	if req.GatewayRef == "" && req.IdempotencyKey == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"adyen: a lookup requires either a pspReference or an idempotency key")
	}
	merchantAccount, _ := req.Credentials.Get(CredentialMerchantAccount)

	path := "/" + g.cfg.APIVersion + "/payments/" + url.PathEscape(req.GatewayRef)
	if req.GatewayRef == "" {
		q := url.Values{}
		q.Set("merchantAccount", merchantAccount)
		q.Set("idempotencyKey", req.IdempotencyKey)
		path = "/" + g.cfg.APIVersion + "/payments?" + q.Encode()
	}
	resp, err := g.do(ctx, callSpec{
		op:     shared.OpLookup,
		method: http.MethodGet,
		path:   path,
		creds:  req.Credentials,
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return g.notFound(resp), nil
	}
	if err := g.checkStatus(resp); err != nil {
		return nil, err
	}
	if req.GatewayRef != "" {
		var pr paymentResponse
		if err := decode(resp.Body, &pr); err != nil {
			return nil, err
		}
		if pr.PSPReference == "" && pr.ResultCode == "" {
			return g.notFound(resp), nil
		}
		return g.paymentResult(&pr, money.Money{}, false, req.IdempotencyKey, resp)
	}

	var list listResponse
	if err := decode(resp.Body, &list); err != nil {
		return nil, err
	}
	for i := range list.Data {
		// The filter is applied client-side as well as in the query string. Correctness must not
		// depend on Adyen honouring a filter parameter on a reporting endpoint; it depends only on
		// the payment carrying the key, which this adapter put there.
		if list.Data[i].Metadata[metaIdempotencyKey] != req.IdempotencyKey {
			continue
		}
		return g.paymentResult(&list.Data[i], money.Money{}, false, req.IdempotencyKey, resp)
	}
	return g.notFound(resp), nil
}

// --- transport -------------------------------------------------------------------------------

type callSpec struct {
	op             shared.Operation
	method         string
	path           string
	body           []byte
	creds          spi.Credentials
	idempotencyKey string
}

// do issues one Adyen request and classifies the transport outcome, on the same rules as every
// other adapter: a flagged timeout on a money-moving call is an unknown outcome, a pre-flight
// failure is a plain error, and a 5xx on a money-moving call is unknown because Adyen does not
// promise that a 500 means nothing happened.
func (g *Gateway) do(ctx context.Context, spec callSpec) (*spi.HTTPResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	key, err := credential(spec.creds, CredentialAPIKey)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{
		"X-API-Key":           key,
		"Accept":              "application/json",
		httpx.OperationHeader: spec.op.String(),
	}
	if len(spec.body) > 0 {
		headers["Content-Type"] = "application/json"
	}
	if spec.idempotencyKey != "" {
		// Adyen's idempotency is header-based like Stripe's, but the stored response is returned
		// for far longer, which makes a transport-level retry after a timeout the cheapest way to
		// resolve an unknown outcome for this gateway.
		headers["Idempotency-Key"] = spec.idempotencyKey
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
			"adyen: the transport returned neither a response nor an error")
	}
	if resp.Timeout {
		return nil, timeoutOutcome(spec.op)
	}
	if resp.StatusCode >= 500 && spec.op.IsMoneyMoving() {
		return nil, apierror.Wrap(spi.ErrOutcomeUnknown, apierror.CodeGatewayTimeout,
			"adyen: the gateway returned "+strconv.Itoa(resp.StatusCode)+
				" and does not guarantee the request was not processed")
	}
	return resp, nil
}

func timeoutOutcome(op shared.Operation) error {
	if op.IsMoneyMoving() {
		return apierror.Wrap(spi.ErrOutcomeUnknown, apierror.CodeGatewayTimeout,
			"adyen: the request was written and the deadline expired; the outcome is unknown")
	}
	return apierror.New(apierror.CodeGatewayTimeout, "adyen: the request timed out")
}

// escalateUnparseable turns an unreadable response on a money-moving call into an unknown outcome,
// on the SPI rule that a gateway which answered unreadably has still acted. Scoped to the parse
// sentinel so that an echo mismatch or an unrecognised resultCode keeps its diagnosis.
func escalateUnparseable(op shared.Operation, err error) error {
	if err == nil || !op.IsMoneyMoving() || !errors.Is(err, errUnparseable) {
		return err
	}
	return apierror.Wrap(spi.ErrOutcomeUnknown, apierror.CodeGatewayTimeout,
		"adyen: the gateway responded but the body could not be parsed; the outcome is unknown")
}

func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return apierror.Wrap(err, apierror.CodeGatewayTimeout,
			"adyen: the deadline expired before the gateway call was made")
	}
	return apierror.Wrap(err, apierror.CodeServiceUnavailable,
		"adyen: the gateway call was cancelled before it was made")
}

func (g *Gateway) checkStatus(resp *spi.HTTPResponse) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var e serviceError
	_ = decode(resp.Body, &e)
	if e.ErrorCode == "" && e.ErrorType == "" {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return apierror.Wrap(spi.ErrCredentialsInvalid, apierror.CodeGatewayAuthenticationFailed,
				"adyen: the API key was rejected")
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return apierror.New(apierror.CodeRateLimited, "adyen: rate limit exceeded")
		}
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"adyen: HTTP %d with an unparseable body", resp.StatusCode)
	}
	return mapErrorCode(resp.StatusCode, &e)
}

// --- result construction ---------------------------------------------------------------------

func (g *Gateway) modify(ctx context.Context, kind modificationKind, op shared.Operation,
	path string, body []byte, creds spi.Credentials, idemKey string, requested money.Money) (*spi.Result, error) {

	resp, err := g.do(ctx, callSpec{
		op: op, method: http.MethodPost, path: path, body: body,
		creds: creds, idempotencyKey: idemKey,
	})
	if err != nil {
		return nil, err
	}
	if err := g.checkStatus(resp); err != nil {
		return nil, err
	}
	var mr modificationResponse
	if err := decode(resp.Body, &mr); err != nil {
		return nil, escalateUnparseable(op, err)
	}
	status, err := mapModificationStatus(mr.Status, kind)
	if err != nil {
		return nil, err
	}
	res := &spi.Result{
		Status:     status,
		GatewayRef: mr.PSPReference,
		RawStatus:  mr.Status,
		ReceivedAt: g.clock.Now(),
		Latency:    resp.Latency,
	}
	if requested.IsValid() && mr.Amount != nil {
		echoed, err := verifyEcho(requested, mr.Amount, "modification")
		if err != nil {
			return nil, err
		}
		if kind == modificationCapture || kind == modificationRefund {
			m := echoed
			res.CapturedAmount = &m
		}
	}
	if res.GatewayRef == "" {
		res.GatewayRef = fallbackRef(idemKey)
	}
	return res, nil
}

func (g *Gateway) paymentResult(pr *paymentResponse, requested money.Money, capture bool, idemKey string, resp *spi.HTTPResponse) (*spi.Result, error) {
	status, err := mapResultCode(pr.ResultCode, capture)
	if err != nil {
		return nil, err
	}
	res := &spi.Result{
		Status:            status,
		GatewayRef:        pr.PSPReference,
		RawStatus:         pr.ResultCode,
		ProcessorResponse: mapProcessorResponse(pr.AdditionalData),
		ReceivedAt:        g.clock.Now(),
		Latency:           resp.Latency,
	}
	if requested.IsValid() && pr.Amount != nil {
		echoed, err := verifyEcho(requested, pr.Amount, "payment")
		if err != nil {
			return nil, err
		}
		if echoed.Amount() != requested.Amount() {
			a := echoed
			res.AuthorizedAmount = &a
		}
	}
	if pr.ResultCode == "PartiallyAuthorised" && pr.Amount != nil {
		if m, err := money.New(pr.Amount.Value, money.Currency(upperTrim(pr.Amount.Currency))); err == nil {
			res.AuthorizedAmount = &m
		}
	}

	switch status {
	case spi.StatusDeclined:
		res.DeclineReason, res.NetworkAdviceNoRetry = mapRefusal(pr.RefusalReasonCode, pr.AdditionalData)
		res.RawCode = pr.RefusalReasonCode
		res.RawMessage = pr.RefusalReason
	case spi.StatusRequiresAction:
		res.NextAction = mapAction(pr.Action)
	case spi.StatusFailed:
		res.RawCode = pr.RefusalReasonCode
		res.RawMessage = pr.RefusalReason
	default:
		// Every other status is fully described by the fields already set above: an authorization,
		// a capture, a refund or a void carries no gateway-specific detail this switch could add.
		// Listed as a default rather than as cases so a new spi.Status does not need an edit here
	}

	if res.GatewayRef == "" && status != spi.StatusFailed {
		res.GatewayRef = fallbackRef(idemKey)
	}
	return res, nil
}

func (g *Gateway) notFound(resp *spi.HTTPResponse) *spi.Result {
	return &spi.Result{Status: spi.StatusNotFound, ReceivedAt: g.clock.Now(), Latency: resp.Latency}
}

// --- shared helpers --------------------------------------------------------------------------

// verifyEcho checks Adyen acted on the amount and currency we asked for.
//
// A currency mismatch or a larger amount is a contract violation surfaced as an error, so the
// payment is quarantined rather than posted against a figure the bank will disagree with. A
// smaller amount is a partial authorization, which is legitimate and is reported to the caller.
func verifyEcho(requested money.Money, got *amount, object string) (money.Money, error) {
	cur := money.Currency(upperTrim(got.Currency))
	if cur != requested.Currency() {
		return money.Money{}, apierror.Newf(apierror.CodeGatewayContractViolation,
			"adyen: the %s echoed currency %s for a request in %s", object, cur, requested.Currency()).
			WithDetail(apierror.Detail{
				Field: "amount.currency", Code: "CURRENCY_ECHO_MISMATCH",
				Message: "the gateway acted in a different currency from the one requested",
				RuleID:  "L6.GATEWAY_ECHOES_CURRENCY",
			})
	}
	if got.Value > requested.Amount() {
		return money.Money{}, apierror.Newf(apierror.CodeGatewayContractViolation,
			"adyen: the %s echoed %d minor units for a request of %d", object, got.Value, requested.Amount()).
			WithDetail(apierror.Detail{
				Field: "amount.value", Code: "AMOUNT_ECHO_EXCEEDS_REQUEST",
				Message: "the gateway acted on a larger amount than was requested",
				RuleID:  "L6.GATEWAY_ECHOES_AMOUNT",
			})
	}
	m, err := money.New(got.Value, cur)
	if err != nil {
		return money.Money{}, apierror.Wrap(err, apierror.CodeGatewayContractViolation,
			"adyen: the response carries an amount this platform cannot represent")
	}
	return m, nil
}

// fallbackRef synthesises a reference from the idempotency key, so every actionable result carries
// something Lookup can resolve. See the identical note in the Stripe adapter.
func fallbackRef(idemKey string) string {
	if idemKey == "" {
		return ""
	}
	return "idemkey:" + idemKey
}

func requireKey(k string) error {
	if k == "" {
		return apierror.New(apierror.CodeIdempotencyKeyRequired,
			"adyen: every mutating gateway call requires an idempotency key")
	}
	return nil
}

// credential reads a required credential field, naming the field and never the value.
func credential(c spi.Credentials, field string) (string, error) {
	v, ok := c.Get(field)
	if !ok || v == "" {
		return "", apierror.Wrapf(spi.ErrCredentialsInvalid, apierror.CodeGatewayNotConfigured,
			"adyen: the credential field %q is missing", field)
	}
	return v, nil
}

// mergedMetadata combines caller metadata with the adapter's own keys, the adapter's winning.
//
// Adyen caps metadata at 20 entries and rejects the whole request when it is exceeded, so the
// platform's own keys are added last and the map is truncated deterministically rather than
// letting a merchant's metadata push out the reconciliation keys.
func mergedMetadata(caller map[string]string, own map[string]string) map[string]string {
	const adyenMetadataLimit = 20
	out := make(map[string]string, len(own)+len(caller))
	for k, v := range own {
		if v != "" {
			out[k] = v
		}
	}
	keys := make([]string, 0, len(caller))
	for k := range caller {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		if len(out) >= adyenMetadataLimit {
			break
		}
		if _, taken := out[k]; taken || caller[k] == "" {
			continue
		}
		out[k] = caller[k]
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func referenceOr(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
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

// declineReasonForKind maps a webhook's failure into the decline taxonomy where Adyen supplies a
// refusal code, and to DeclineUnknown where it does not. Exported behaviour lives in Verify; this
// is here so the mapping is stated once for both the synchronous and the asynchronous path.
func declineReasonForItem(item *notificationRequestItem) payment.DeclineReason {
	if item == nil {
		return payment.DeclineUnknown
	}
	code := item.AdditionalData["refusalReasonCode"]
	if code == "" {
		return payment.DeclineUnknown
	}
	reason, _ := mapRefusal(code, item.AdditionalData)
	return reason
}
