// Package stripe is the Stripe adapter: the translation between the platform's normalized
// gateway contract and Stripe's actual HTTP API.
//
// Everything Stripe-shaped stops at this package boundary. A PaymentIntent, a `decline_code`, a
// `Stripe-Account` header and the fact that Stripe speaks form encoding rather than JSON are all
// facts that exist inside these files and nowhere else in the repository; above them the platform
// sees only spi.Result and payment.DeclineReason. That is the whole point of the anti-corruption
// layer, and it is checked mechanically — a gateway-specific type escaping this package would
// show up as an import of this package from outside internal/adapters.
//
// Three Stripe behaviours shape most of the code here and are worth stating once at the top:
//
//  1. Stripe is form-encoded, with bracket syntax for nesting. See form.go.
//  2. Stripe expresses card declines as HTTP 402 with an error body, not as a 200 with a status
//     field. A 402 is therefore a successful *call* with a declined *outcome*, and the adapter
//     must not treat it as a transport failure — doing so would make the orchestrator fail over
//     on a hard decline, which is card testing with extra steps.
//  3. Stripe replays the original response for a reused Idempotency-Key for 24 hours. That makes
//     a retry after a timeout free and safe inside the window. Outside it, the key is forgotten,
//     which is why this adapter also stamps the key into `metadata[pp_idempotency_key]` so a
//     lookup by key alone still works months later during reconciliation.
package stripe

import (
	"context"
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

// GatewayID is this adapter's registry slug. It is exported because the registry, the routing
// configuration and the webhook ingress URL all name the gateway by this value, and a string
// literal repeated in four places is a typo waiting to be deployed.
const GatewayID shared.GatewayID = "stripe"

// Credential field names, as they appear in the resolved spi.Credentials map.
//
// Stripe's Connect model gives the platform no per-merchant secret: the platform's own secret key
// plus a `Stripe-Account` header *is* the credential for a connected account. That concentrates
// blast radius into one key, which is why it lives on its own rotation schedule — see
// docs/onboarding.md §5.1 — and why this adapter never accepts a per-connection key.
const (
	// CredentialSecretKey is the platform's `sk_live_…` / `sk_test_…` key.
	CredentialSecretKey = "secret_key"
)

// DefaultAPIVersion is the Stripe API version this adapter is written against.
//
// It is pinned on every request via the `Stripe-Version` header rather than left to the account
// default, because the account default is a dashboard setting that an operator can change without
// a deploy — and a silent API version bump changes response shapes underneath a running adapter.
// Pinning makes the version an artifact of the code, reviewable and revertable. Overriding it
// through spi.Config.APIVersion is supported so a certification run can exercise a new version
// before the code moves.
const DefaultAPIVersion = "2026-06-30.acacia"

// Metadata keys the adapter stamps on every object it creates at Stripe.
//
// These exist so that reconciliation can answer "what is this charge" from Stripe's side alone.
// The idempotency key in particular is what makes Lookup work after Stripe's 24-hour idempotency
// window has closed, which is precisely the window in which a stuck payment is still unresolved.
const (
	metaPaymentID      = "pp_payment_id"
	metaAttemptID      = "pp_attempt_id"
	metaRefundID       = "pp_refund_id"
	metaIdempotencyKey = "pp_idempotency_key"
)

// Gateway is the Stripe implementation of spi.PaymentGateway.
type Gateway struct {
	cfg    spi.Config
	client spi.HTTPDoer
	clock  shared.Clock
}

var _ spi.PaymentGateway = (*Gateway)(nil)

// NewGateway builds the payment-path adapter.
//
// It validates the configuration at construction rather than at first use. A gateway with no base
// URL or no HTTP client is a deployment error, and discovering it on the first real payment means
// discovering it with a payer waiting.
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
			"stripe: a base URL is required; it is environment-scoped and has no safe default")
	}
	if cfg.HTTPClient == nil {
		return cfg, apierror.New(apierror.CodeGatewayNotConfigured,
			"stripe: an HTTP client must be injected so the adapter runs inside the resilience envelope")
	}
	if !cfg.Environment.IsValid() {
		return cfg, apierror.New(apierror.CodeGatewayNotConfigured,
			"stripe: the environment must be sandbox or production; there is no default, because the wrong default charges a real card")
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

// Authorize creates and confirms a PaymentIntent.
//
// The whole operation is one call. Stripe supports create-then-confirm as two round trips, and
// this adapter deliberately does not use it: a crash between the two leaves an unconfirmed intent
// that nothing owns, and the second call would need its own idempotency key, doubling the number
// of unknown outcomes the reconciler has to resolve.
//
// req.Capture selects `capture_method`: automatic for a sale, manual for an authorization the
// platform will capture later. It is sent explicitly rather than relying on Stripe's default,
// because Stripe's default is `automatic` and an adapter that omitted the parameter would silently
// take the payer's money on every authorization-only payment.
func (g *Gateway) Authorize(ctx context.Context, req spi.AuthorizeRequest) (*spi.Result, error) {
	if err := requireKey(req.IdempotencyKey); err != nil {
		return nil, err
	}
	if !req.Amount.IsValid() {
		return nil, apierror.New(apierror.CodeAmountInvalid,
			"stripe: the amount carries an unsupported currency")
	}

	f := &form{}
	f.setInt("amount", req.Amount.Amount())
	f.set("currency", strings.ToLower(string(req.Amount.Currency())))
	f.set("payment_method", req.MethodRef.Token)
	f.setBool("confirm", true)
	if req.Capture {
		f.set("capture_method", "automatic")
	} else {
		f.set("capture_method", "manual")
	}
	// off_session tells Stripe the payer is not present, which changes both the SCA exemption
	// Stripe claims and the decline codes the issuer returns. The platform models "payer present"
	// as "there is somewhere to redirect them to", which is exactly the condition under which a
	// challenge can be completed; a merchant-initiated transaction has no return URL and no
	// challenge path, so it must be declared off-session or the issuer will soft-decline it with
	// authentication_required and there will be nobody to authenticate.
	offSession := req.ReturnURL == "" && !req.ThreeDS.Requested
	f.setBool("off_session", offSession)
	if req.ReturnURL != "" {
		f.set("return_url", req.ReturnURL)
	}
	if req.ThreeDS.Requested {
		f.set("payment_method_options[card][request_three_d_secure]", "any")
	}
	// Statement descriptors: Stripe splits these into a static prefix set on the account and a
	// per-payment suffix. Sending `statement_descriptor` on a card payment is rejected by newer
	// API versions for accounts that have a prefix configured, so the platform's per-payment text
	// goes in the suffix, which is the field that actually varies per transaction.
	f.set("statement_descriptor_suffix", req.StatementRef)
	f.set("description", req.Reference)
	if req.Descriptor != "" && req.StatementRef == "" {
		f.set("statement_descriptor_suffix", req.Descriptor)
	}
	// Expanding the charge in the create response saves a second round trip for the AVS/CVV and
	// network-token detail a dispute defence needs, and that detail is only reliably available at
	// authorization time.
	f.appendItem("expand", "latest_charge")

	meta := mergedMetadata(req.Metadata, map[string]string{
		metaPaymentID:      req.PaymentID.String(),
		metaAttemptID:      req.AttemptID.String(),
		metaIdempotencyKey: req.IdempotencyKey,
	})
	f.setMap("metadata", meta)

	resp, err := g.do(ctx, callSpec{
		op:             shared.OpAuthorize,
		method:         http.MethodPost,
		path:           "/v1/payment_intents",
		body:           f.bytes(),
		creds:          req.Credentials,
		account:        req.ExternalAccountID,
		idempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	res, err := g.intentResult(resp, req.Amount, req.IdempotencyKey, true)
	return res, escalateUnparseable(shared.OpAuthorize, err)
}

// Capture converts a hold into a debit.
//
// `final_capture` is sent when the platform knows no further capture will follow, because Stripe
// releases the uncaptured remainder of the authorization immediately when it is true and holds it
// until expiry when it is false. Getting that wrong keeps a payer's funds encumbered for days
// after the merchant has finished with them, which is a support contact rather than a bug report.
func (g *Gateway) Capture(ctx context.Context, req spi.CaptureRequest) (*spi.Result, error) {
	if err := requireKey(req.IdempotencyKey); err != nil {
		return nil, err
	}
	if req.GatewayRef == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"stripe: a capture requires the PaymentIntent reference from the authorization")
	}
	f := &form{}
	f.setInt("amount_to_capture", req.Amount.Amount())
	f.setBool("final_capture", req.Final)
	f.appendItem("expand", "latest_charge")
	f.setMap("metadata", mergedMetadata(req.Metadata, map[string]string{
		metaIdempotencyKey: req.IdempotencyKey,
	}))

	resp, err := g.do(ctx, callSpec{
		op:             shared.OpCapture,
		method:         http.MethodPost,
		path:           "/v1/payment_intents/" + url.PathEscape(req.GatewayRef) + "/capture",
		body:           f.bytes(),
		creds:          req.Credentials,
		account:        req.ExternalAccountID,
		idempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	res, err := g.intentResult(resp, req.Amount, req.IdempotencyKey, false)
	return res, escalateUnparseable(shared.OpCapture, err)
}

// Refund returns captured funds.
//
// The refund is created against the PaymentIntent rather than the Charge. Both work; the intent
// is used because it is the reference the platform already holds, and because refunding by intent
// lets Stripe pick the right charge when a multi-capture payment produced several.
func (g *Gateway) Refund(ctx context.Context, req spi.RefundRequest) (*spi.Result, error) {
	if err := requireKey(req.IdempotencyKey); err != nil {
		return nil, err
	}
	if req.GatewayRef == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"stripe: a refund requires the PaymentIntent reference from the capture")
	}
	f := &form{}
	f.set("payment_intent", req.GatewayRef)
	f.setInt("amount", req.Amount.Amount())
	// Stripe accepts only three reasons and rejects anything else outright. The platform's
	// taxonomy is richer, so anything outside Stripe's set is sent as no reason at all rather than
	// as a wrong one: the platform's own reason is preserved in metadata, where it is accurate.
	if r := stripeRefundReason(req.Reason); r != "" {
		f.set("reason", r)
	}
	f.setMap("metadata", mergedMetadata(req.Metadata, map[string]string{
		metaRefundID:       req.RefundID.String(),
		metaPaymentID:      req.PaymentID.String(),
		metaIdempotencyKey: req.IdempotencyKey,
		"pp_refund_reason": string(req.Reason),
	}))

	resp, err := g.do(ctx, callSpec{
		op:             shared.OpRefund,
		method:         http.MethodPost,
		path:           "/v1/refunds",
		body:           f.bytes(),
		creds:          req.Credentials,
		account:        req.ExternalAccountID,
		idempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	res, err := g.refundResult(resp, req.Amount, req.IdempotencyKey)
	return res, escalateUnparseable(shared.OpRefund, err)
}

// Void releases an uncaptured authorization by cancelling the PaymentIntent.
func (g *Gateway) Void(ctx context.Context, req spi.VoidRequest) (*spi.Result, error) {
	if err := requireKey(req.IdempotencyKey); err != nil {
		return nil, err
	}
	if req.GatewayRef == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"stripe: a void requires the PaymentIntent reference from the authorization")
	}
	f := &form{}
	f.set("cancellation_reason", "requested_by_customer")
	f.appendItem("expand", "latest_charge")

	resp, err := g.do(ctx, callSpec{
		op:             shared.OpVoid,
		method:         http.MethodPost,
		path:           "/v1/payment_intents/" + url.PathEscape(req.GatewayRef) + "/cancel",
		body:           f.bytes(),
		creds:          req.Credentials,
		account:        req.ExternalAccountID,
		idempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	// A void has no amount of its own: the released hold is whatever was authorized. Passing the
	// zero Money would trip the echo check, so voids skip it and report what Stripe says.
	res, err := g.intentResult(resp, money.Money{}, req.IdempotencyKey, false)
	return res, escalateUnparseable(shared.OpVoid, err)
}

// Lookup asks Stripe what happened, by reference or by idempotency key alone.
//
// The key-only path is the one that matters and the one usually missing from adapters. After a
// timeout the platform may hold no gateway reference at all — the crash can land between "sent
// the request" and "recorded the response" — and the reconciler's only handle is the deterministic
// idempotency key. Two mechanisms cover it, in order of fidelity:
//
//  1. Inside Stripe's 24-hour idempotency window, replaying the original POST with the same key
//     returns the original response byte for byte. That is the authoritative answer, and it is
//     what the orchestrator's transport-level retry already exploits.
//  2. Outside the window Stripe has forgotten the key, so this adapter falls back to listing
//     recent PaymentIntents and filtering on `metadata[pp_idempotency_key]`, which Authorize
//     stamped on the object for exactly this purpose. The filter is applied client-side as well as
//     server-side, so correctness does not depend on Stripe honouring a metadata filter on a list
//     endpoint — it depends only on the object carrying the key, which we control.
//
// A miss returns StatusNotFound with a nil error. That is a positive finding, not a failure:
// combined with a deterministic key it is evidence the operation never took effect and is safe to
// retry, which is the only way an unknown outcome ever gets resolved in the retry direction.
func (g *Gateway) Lookup(ctx context.Context, req spi.LookupRequest) (*spi.Result, error) {
	if req.GatewayRef == "" && req.IdempotencyKey == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"stripe: a lookup requires either a gateway reference or an idempotency key")
	}
	if req.Operation == shared.OpRefund {
		return g.lookupRefund(ctx, req)
	}
	if req.GatewayRef != "" {
		resp, err := g.do(ctx, callSpec{
			op:      shared.OpLookup,
			method:  http.MethodGet,
			path:    "/v1/payment_intents/" + url.PathEscape(req.GatewayRef) + "?expand[]=latest_charge",
			creds:   req.Credentials,
			account: req.ExternalAccountID,
		})
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusNotFound {
			return g.notFound(resp), nil
		}
		return g.intentResult(resp, money.Money{}, req.IdempotencyKey, false)
	}

	resp, err := g.do(ctx, callSpec{
		op:     shared.OpLookup,
		method: http.MethodGet,
		path: "/v1/payment_intents?limit=100&expand[]=data.latest_charge&" +
			url.QueryEscape("metadata["+metaIdempotencyKey+"]") + "=" + url.QueryEscape(req.IdempotencyKey),
		creds:   req.Credentials,
		account: req.ExternalAccountID,
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return g.notFound(resp), nil
	}
	if err := g.checkStatus(resp, shared.OpLookup); err != nil {
		return nil, err
	}
	var list listResponse[paymentIntent]
	if err := decode(resp.Body, &list); err != nil {
		return nil, err
	}
	for i := range list.Data {
		pi := &list.Data[i]
		if pi.Metadata[metaIdempotencyKey] != req.IdempotencyKey {
			continue
		}
		return g.resultFromIntent(pi, nil, money.Money{}, req.IdempotencyKey, resp)
	}
	return g.notFound(resp), nil
}

func (g *Gateway) lookupRefund(ctx context.Context, req spi.LookupRequest) (*spi.Result, error) {
	path := "/v1/refunds?limit=100&" +
		url.QueryEscape("metadata["+metaIdempotencyKey+"]") + "=" + url.QueryEscape(req.IdempotencyKey)
	if req.GatewayRef != "" {
		path = "/v1/refunds/" + url.PathEscape(req.GatewayRef)
	}
	resp, err := g.do(ctx, callSpec{
		op:      shared.OpLookup,
		method:  http.MethodGet,
		path:    path,
		creds:   req.Credentials,
		account: req.ExternalAccountID,
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return g.notFound(resp), nil
	}
	if err := g.checkStatus(resp, shared.OpLookup); err != nil {
		return nil, err
	}
	if req.GatewayRef != "" {
		var r refund
		if err := decode(resp.Body, &r); err != nil {
			return nil, err
		}
		return g.resultFromRefund(&r, money.Money{}, req.IdempotencyKey, resp)
	}
	var list listResponse[refund]
	if err := decode(resp.Body, &list); err != nil {
		return nil, err
	}
	for i := range list.Data {
		if list.Data[i].Metadata[metaIdempotencyKey] == req.IdempotencyKey {
			return g.resultFromRefund(&list.Data[i], money.Money{}, req.IdempotencyKey, resp)
		}
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
	account        string
	idempotencyKey string
}

// do issues one Stripe request and classifies the transport outcome.
//
// The classification is the load-bearing part, and it is centralised here so that no individual
// operation can get it subtly wrong:
//
//   - A flagged timeout on a money-moving operation becomes spi.ErrOutcomeUnknown. Stripe may
//     well have authorized the card; we simply did not hear the answer.
//   - A flagged timeout on a lookup becomes a plain timeout error. A lookup moves no money, so
//     parking the payment in reconciliation over it would be pure noise.
//   - A transport error — refused connection, DNS failure, a body over the cap — is returned as
//     it stands. The request never reached Stripe, so the operation is safe to retry, and
//     reporting it as unknown would strand a payment that is provably fine.
//   - HTTP 5xx on a money-moving operation is also unknown. Stripe does not guarantee that a 500
//     means nothing happened; it means the response failed, and the request may have been
//     processed first.
func (g *Gateway) do(ctx context.Context, spec callSpec) (*spi.HTTPResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	key, err := credential(spec.creds, CredentialSecretKey)
	if err != nil {
		return nil, err
	}

	headers := map[string]string{
		"Authorization":       "Bearer " + key,
		"Stripe-Version":      g.cfg.APIVersion,
		"Accept":              "application/json",
		httpx.OperationHeader: spec.op.String(),
	}
	if len(spec.body) > 0 {
		headers["Content-Type"] = "application/x-www-form-urlencoded"
	}
	if spec.idempotencyKey != "" {
		// Stripe's idempotency is header-based and applies to POST only; sending it on a GET is
		// harmless but meaningless, so it is only ever set where a call spec carries one.
		headers["Idempotency-Key"] = spec.idempotencyKey
	}
	if spec.account != "" {
		// The connected account is scoped per request rather than per client. One platform key
		// serves every merchant, and the account this call acts for is stated on the call — which
		// means a bug that omits it fails closed (the charge lands on the platform account and is
		// visible immediately) rather than silently charging the wrong merchant's customer.
		headers["Stripe-Account"] = spec.account
	}

	resp, err := g.client.Do(&spi.HTTPRequest{
		Ctx:     ctx,
		Method:  spec.method,
		URL:     g.cfg.BaseURL + spec.path,
		Headers: headers,
		Body:    spec.body,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"stripe: the transport returned neither a response nor an error")
	}
	if resp.Timeout {
		return nil, timeoutOutcome(spec.op, "stripe")
	}
	if resp.StatusCode >= 500 && spec.op.IsMoneyMoving() {
		return nil, apierror.Wrap(spi.ErrOutcomeUnknown, apierror.CodeGatewayTimeout,
			"stripe: the gateway returned "+strconv.Itoa(resp.StatusCode)+
				" and does not guarantee the request was not processed")
	}
	return resp, nil
}

// timeoutOutcome is the single place the unknown-versus-error decision is made for a timeout.
func timeoutOutcome(op shared.Operation, vendor string) error {
	if op.IsMoneyMoving() {
		return apierror.Wrap(spi.ErrOutcomeUnknown, apierror.CodeGatewayTimeout,
			vendor+": the request was written and the deadline expired; the outcome is unknown")
	}
	return apierror.New(apierror.CodeGatewayTimeout,
		vendor+": the request timed out")
}

// escalateUnparseable turns an unreadable response on a money-moving call into an unknown outcome.
//
// This is the SPI's rule applied literally: "any response the adapter cannot parse" belongs in the
// unknown bucket, because the gateway did act and only our reading of the answer failed. It is
// scoped to the parse sentinel rather than to every contract violation on purpose — an echo
// mismatch or an unrecognised status is a violation the platform *can* describe, and flattening
// those into "unknown" would lose the diagnosis and park payments the response validator could
// have explained.
func escalateUnparseable(op shared.Operation, err error) error {
	if err == nil || !op.IsMoneyMoving() || !errors.Is(err, errUnparseable) {
		return err
	}
	return apierror.Wrap(spi.ErrOutcomeUnknown, apierror.CodeGatewayTimeout,
		"stripe: the gateway responded but the body could not be parsed; the outcome is unknown")
}

// contextError classifies a context that was already done before anything was sent. Nothing left
// the process, so this is a plain error and never an unknown outcome.
func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return apierror.Wrap(err, apierror.CodeGatewayTimeout,
			"the deadline expired before the gateway call was made")
	}
	return apierror.Wrap(err, apierror.CodeServiceUnavailable,
		"the gateway call was cancelled before it was made")
}

// checkStatus converts a non-2xx into a platform error, leaving 402 alone: a 402 is a decline and
// is the caller's business, not an error.
func (g *Gateway) checkStatus(resp *spi.HTTPResponse, op shared.Operation) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var env errorEnvelope
	// A body that will not parse at all on an error status is still an error; falling back to the
	// status code keeps the failure attributable instead of turning into a parse error that hides
	// an authentication problem.
	_ = decode(resp.Body, &env)
	if env.Error == nil {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return apierror.Wrap(spi.ErrCredentialsInvalid, apierror.CodeGatewayAuthenticationFailed,
				"stripe: the API key was rejected")
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return apierror.New(apierror.CodeRateLimited, "stripe: rate limit exceeded")
		}
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"stripe: HTTP %d with an unparseable body", resp.StatusCode)
	}
	return mapErrorType(resp.StatusCode, env.Error)
}

// --- result construction ---------------------------------------------------------------------

// intentResult builds a Result from a PaymentIntent response, including the 402 decline case.
func (g *Gateway) intentResult(resp *spi.HTTPResponse, requested money.Money, idemKey string, isAuthorize bool) (*spi.Result, error) {
	if resp.StatusCode == http.StatusPaymentRequired {
		// The decline path. Stripe returns 402 with the error object *and* the intent it was
		// raised against, which is what lets a decline carry a reference the platform can later
		// reconcile against.
		var env errorEnvelope
		if err := decode(resp.Body, &env); err != nil {
			return nil, err
		}
		if env.Error == nil {
			return nil, apierror.New(apierror.CodeGatewayContractViolation,
				"stripe: HTTP 402 with no error object")
		}
		return g.declineResult(env.Error, idemKey, resp), nil
	}
	if err := g.checkStatus(resp, shared.OpAuthorize); err != nil {
		return nil, err
	}
	var pi paymentIntent
	if err := decode(resp.Body, &pi); err != nil {
		return nil, err
	}
	if pi.ID == "" {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"stripe: the response carries no payment_intent id")
	}
	_ = isAuthorize
	return g.resultFromIntent(&pi, nil, requested, idemKey, resp)
}

func (g *Gateway) resultFromIntent(pi *paymentIntent, declineErr *stripeError, requested money.Money, idemKey string, resp *spi.HTTPResponse) (*spi.Result, error) {
	status, err := mapIntentStatus(pi)
	if err != nil {
		return nil, err
	}
	ch := pi.primaryCharge()

	res := &spi.Result{
		Status:            status,
		GatewayRef:        pi.ID,
		RawStatus:         pi.Status,
		ProcessorResponse: mapProcessorResponse(ch),
		ReceivedAt:        g.clock.Now(),
		Latency:           resp.Latency,
	}

	// The echo check. A gateway that answers in a different currency, or takes more than was
	// asked, is a contract violation the platform must surface rather than absorb: absorbing it
	// means the ledger records what we asked for and the bank records what happened.
	if requested.IsValid() {
		echoed, err := verifyEcho(requested, pi.Amount, pi.Currency, "payment_intent")
		if err != nil {
			return nil, err
		}
		if echoed.Amount() != requested.Amount() {
			// Less than requested is a legitimate partial authorization, reported rather than
			// rejected so the orchestrator can decide whether the partial amount is acceptable.
			a := echoed
			res.AuthorizedAmount = &a
		}
	}
	if pi.AmountReceived > 0 {
		if m, err := money.New(pi.AmountReceived, money.Currency(normalizeCurrency(pi.Currency))); err == nil {
			res.CapturedAmount = &m
		}
	}
	if pi.AmountCapturable > 0 && res.AuthorizedAmount == nil {
		if m, err := money.New(pi.AmountCapturable, money.Currency(normalizeCurrency(pi.Currency))); err == nil {
			res.AuthorizedAmount = &m
		}
	}

	switch status {
	case spi.StatusDeclined:
		e := declineErr
		if e == nil {
			e = pi.LastPaymentError
		}
		var out *chargeOutcome
		if ch != nil {
			out = ch.Outcome
		}
		res.DeclineReason, res.NetworkAdviceNoRetry = mapDecline(e, out)
		res.RawCode = safeCode(e)
		if e != nil {
			res.RawMessage = e.Message
		}
	case spi.StatusRequiresAction:
		res.NextAction = mapNextAction(pi.NextAction)
	default:
		// Every other status is fully described by the fields set above. A decline needs its reason
		// and a required action needs its redirect; an authorization, capture, refund or void does not
	}

	if res.GatewayRef == "" {
		res.GatewayRef = fallbackRef(idemKey)
	}
	return res, nil
}

// declineResult builds the declined Result from a 402 body.
func (g *Gateway) declineResult(e *stripeError, idemKey string, resp *spi.HTTPResponse) *spi.Result {
	reason, noRetry := mapDecline(e, nil)
	ref := ""
	if e.PaymentIntent != nil {
		ref = e.PaymentIntent.ID
	}
	if ref == "" {
		ref = e.Charge
	}
	if ref == "" {
		// A decline Stripe could not attach to an object still has to be referenceable, or the
		// platform can never ask "did that actually happen" again. The idempotency key is a
		// reference we own and that Lookup accepts, so it is used verbatim with a marker prefix
		// that makes its provenance obvious in a support conversation.
		ref = fallbackRef(idemKey)
	}
	res := &spi.Result{
		Status:               spi.StatusDeclined,
		GatewayRef:           ref,
		DeclineReason:        reason,
		NetworkAdviceNoRetry: noRetry,
		RawStatus:            "requires_payment_method",
		RawCode:              safeCode(e),
		RawMessage:           e.Message,
		ReceivedAt:           g.clock.Now(),
		Latency:              resp.Latency,
	}
	if e.PaymentIntent != nil {
		res.RawStatus = e.PaymentIntent.Status
	}
	return res
}

func (g *Gateway) refundResult(resp *spi.HTTPResponse, requested money.Money, idemKey string) (*spi.Result, error) {
	if err := g.checkStatus(resp, shared.OpRefund); err != nil {
		return nil, err
	}
	var r refund
	if err := decode(resp.Body, &r); err != nil {
		return nil, err
	}
	if r.ID == "" {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"stripe: the refund response carries no refund id")
	}
	return g.resultFromRefund(&r, requested, idemKey, resp)
}

func (g *Gateway) resultFromRefund(r *refund, requested money.Money, idemKey string, resp *spi.HTTPResponse) (*spi.Result, error) {
	status, err := mapRefundStatus(r)
	if err != nil {
		return nil, err
	}
	res := &spi.Result{
		Status:     status,
		GatewayRef: r.ID,
		RawStatus:  r.Status,
		RawCode:    r.FailureReason,
		ReceivedAt: g.clock.Now(),
		Latency:    resp.Latency,
	}
	if requested.IsValid() {
		if _, err := verifyEcho(requested, r.Amount, r.Currency, "refund"); err != nil {
			return nil, err
		}
	}
	if m, err := money.New(r.Amount, money.Currency(normalizeCurrency(r.Currency))); err == nil {
		res.CapturedAmount = &m
	}
	if res.GatewayRef == "" {
		res.GatewayRef = fallbackRef(idemKey)
	}
	return res, nil
}

func (g *Gateway) notFound(resp *spi.HTTPResponse) *spi.Result {
	return &spi.Result{
		Status:     spi.StatusNotFound,
		ReceivedAt: g.clock.Now(),
		Latency:    resp.Latency,
	}
}

// --- shared helpers --------------------------------------------------------------------------

// verifyEcho checks that the gateway acted on the amount and currency we asked it to.
//
// Two failures are treated very differently, because they are very different:
//
//   - A currency mismatch, or an amount *larger* than requested, is a contract violation. There is
//     no legitimate reason for either, and silently accepting them means the ledger and the bank
//     disagree by construction. It is surfaced as an error so the response validator's L6 alarm
//     fires and the payment is quarantined rather than posted.
//   - An amount *smaller* than requested is a partial authorization, which is a normal issuer
//     behaviour on prepaid and some debit products. It is returned to the caller, which decides
//     whether a partial amount is acceptable for this order.
func verifyEcho(requested money.Money, gotMinor int64, gotCurrency, object string) (money.Money, error) {
	cur := money.Currency(normalizeCurrency(gotCurrency))
	if cur != requested.Currency() {
		return money.Money{}, apierror.Newf(apierror.CodeGatewayContractViolation,
			"stripe: the %s echoed currency %s for a request in %s", object, cur, requested.Currency()).
			WithDetail(apierror.Detail{
				Field: "currency", Code: "CURRENCY_ECHO_MISMATCH",
				Message: "the gateway acted in a different currency from the one requested",
				RuleID:  "L6.GATEWAY_ECHOES_CURRENCY",
			})
	}
	if gotMinor > requested.Amount() {
		return money.Money{}, apierror.Newf(apierror.CodeGatewayContractViolation,
			"stripe: the %s echoed %d minor units for a request of %d", object, gotMinor, requested.Amount()).
			WithDetail(apierror.Detail{
				Field: "amount", Code: "AMOUNT_ECHO_EXCEEDS_REQUEST",
				Message: "the gateway acted on a larger amount than was requested",
				RuleID:  "L6.GATEWAY_ECHOES_AMOUNT",
			})
	}
	m, err := money.New(gotMinor, cur)
	if err != nil {
		return money.Money{}, apierror.Wrap(err, apierror.CodeGatewayContractViolation,
			"stripe: the response carries an amount this platform cannot represent")
	}
	return m, nil
}

// fallbackRef synthesises a reference from the idempotency key.
//
// Every non-NOT_FOUND, non-FAILED result must carry something the platform can look the
// transaction up by, and the idempotency key satisfies that: Lookup accepts it on its own. The
// `idemkey:` prefix exists so nobody mistakes it for a vendor identifier when it appears in a
// support conversation or a reconciliation report.
func fallbackRef(idemKey string) string {
	if idemKey == "" {
		return ""
	}
	return "idemkey:" + idemKey
}

func requireKey(k string) error {
	if k == "" {
		return apierror.New(apierror.CodeIdempotencyKeyRequired,
			"stripe: every mutating gateway call requires an idempotency key")
	}
	return nil
}

// credential reads a required credential field.
//
// The error deliberately names the *field* and never the value, and it does not render the
// Credentials struct — which redacts anyway, but relying on that would be relying on a second
// layer for a mistake this layer should not make.
func credential(c spi.Credentials, field string) (string, error) {
	v, ok := c.Get(field)
	if !ok || v == "" {
		return "", apierror.Wrapf(spi.ErrCredentialsInvalid, apierror.CodeGatewayNotConfigured,
			"stripe: the credential field %q is missing", field)
	}
	return v, nil
}

// mergedMetadata combines caller metadata with the adapter's own keys.
//
// The adapter's keys win. A caller who sets `pp_payment_id` themselves would otherwise break
// reconciliation for that payment, and the platform's own bookkeeping is not something a merchant
// gets to overwrite through a metadata field.
func mergedMetadata(caller map[string]string, own map[string]string) map[string]string {
	out := make(map[string]string, len(caller)+len(own))
	for k, v := range caller {
		out[k] = v
	}
	for k, v := range own {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// stripeRefundReason maps the platform's refund taxonomy onto the three values Stripe accepts.
// Anything else returns the empty string, which omits the parameter — Stripe rejects an unknown
// reason outright, and a rejected refund is worse than an unlabelled one.
func stripeRefundReason(r payment.RefundReason) string {
	switch r {
	case payment.RefundReasonRequestedByCustomer, payment.RefundReasonProductUnavailable,
		payment.RefundReasonServiceNotProvided:
		return "requested_by_customer"
	case payment.RefundReasonDuplicate:
		return "duplicate"
	case payment.RefundReasonFraudulent:
		return "fraudulent"
	default:
		return ""
	}
}
