package simulator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Adapter is the HTTP client for the simulator's protocol, and is itself an spi.PaymentGateway.
//
// It exists so the simulator is not exempt from the rules it is used to check. The contract suite
// runs against this adapter exactly as it runs against Stripe, Adyen and PayPal, which means the
// simulator's own idempotency, timeout classification, decline normalization and webhook
// verification are all held to the same standard. A simulator that passed by being special would be
// a simulator whose green tests meant nothing.
type Adapter struct {
	cfg    spi.Config
	client spi.HTTPDoer
	clock  shared.Clock
}

var _ spi.PaymentGateway = (*Adapter)(nil)

// NewAdapter builds the HTTP client adapter.
func NewAdapter(cfg spi.Config) (*Adapter, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Adapter{cfg: cfg, client: cfg.HTTPClient, clock: cfg.Clock}, nil
}

func normalizeConfig(cfg spi.Config) (spi.Config, error) {
	if cfg.BaseURL == "" {
		return cfg, apierror.New(apierror.CodeGatewayNotConfigured,
			"simulator: a base URL is required")
	}
	if cfg.HTTPClient == nil {
		return cfg, apierror.New(apierror.CodeGatewayNotConfigured,
			"simulator: an HTTP client must be injected so the adapter runs inside the resilience envelope")
	}
	if cfg.Clock == nil {
		cfg.Clock = shared.SystemClock{}
	}
	if cfg.Environment == "" {
		// The simulator is the one gateway with a safe default environment: it never moves money, so
		// a wrong guess here cannot charge a card. Every real adapter refuses to default this.
		cfg.Environment = shared.EnvironmentSandbox
	}
	if cfg.WebhookTolerance <= 0 {
		cfg.WebhookTolerance = 5 * time.Minute
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return cfg, nil
}

// ID returns the registry slug.
func (a *Adapter) ID() shared.GatewayID { return GatewayID }

// Authorize places a hold, or takes funds when req.Capture is true.
func (a *Adapter) Authorize(ctx context.Context, req spi.AuthorizeRequest) (*spi.Result, error) {
	return a.send(ctx, shared.OpAuthorize, http.MethodPost, PathPrefix+"/payments",
		req.Credentials, req.IdempotencyKey, req.Amount, WireRequest{
			Operation:      opAuthorize,
			IdempotencyKey: req.IdempotencyKey,
			Amount:         wireAmount(req.Amount),
			Capture:        req.Capture,
			PaymentID:      req.PaymentID.String(),
			AttemptID:      req.AttemptID.String(),
			ReturnURL:      req.ReturnURL,
			ThreeDS:        req.ThreeDS.Requested,
			Metadata:       req.Metadata,
		})
}

// Capture converts a hold into a debit.
func (a *Adapter) Capture(ctx context.Context, req spi.CaptureRequest) (*spi.Result, error) {
	if req.GatewayRef == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"simulator: a capture requires the reference from the authorization")
	}
	return a.send(ctx, shared.OpCapture, http.MethodPost,
		PathPrefix+"/payments/"+url.PathEscape(req.GatewayRef)+"/capture",
		req.Credentials, req.IdempotencyKey, req.Amount, WireRequest{
			Operation:      opCapture,
			IdempotencyKey: req.IdempotencyKey,
			Reference:      req.GatewayRef,
			Amount:         wireAmount(req.Amount),
			Final:          req.Final,
			PaymentID:      req.PaymentID.String(),
			Metadata:       req.Metadata,
		})
}

// Refund returns captured funds.
func (a *Adapter) Refund(ctx context.Context, req spi.RefundRequest) (*spi.Result, error) {
	if req.GatewayRef == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"simulator: a refund requires the reference from the capture")
	}
	return a.send(ctx, shared.OpRefund, http.MethodPost,
		PathPrefix+"/payments/"+url.PathEscape(req.GatewayRef)+"/refund",
		req.Credentials, req.IdempotencyKey, req.Amount, WireRequest{
			Operation:      opRefund,
			IdempotencyKey: req.IdempotencyKey,
			Reference:      req.GatewayRef,
			Amount:         wireAmount(req.Amount),
			PaymentID:      req.PaymentID.String(),
			RefundID:       req.RefundID.String(),
			Reason:         string(req.Reason),
			Metadata:       req.Metadata,
		})
}

// Void releases an uncaptured authorization.
func (a *Adapter) Void(ctx context.Context, req spi.VoidRequest) (*spi.Result, error) {
	if req.GatewayRef == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"simulator: a void requires the reference from the authorization")
	}
	return a.send(ctx, shared.OpVoid, http.MethodPost,
		PathPrefix+"/payments/"+url.PathEscape(req.GatewayRef)+"/void",
		req.Credentials, req.IdempotencyKey, money.Money{}, WireRequest{
			Operation:      opVoid,
			IdempotencyKey: req.IdempotencyKey,
			Reference:      req.GatewayRef,
			PaymentID:      req.PaymentID.String(),
			Metadata:       req.Metadata,
		})
}

// Lookup resolves a transaction by reference or by idempotency key alone.
//
// The key-only form is the one reconciliation needs and the one adapters usually omit, so it is the
// form the URL is built for: the reference is a query parameter, not a path segment, precisely so
// that an empty reference is expressible.
func (a *Adapter) Lookup(ctx context.Context, req spi.LookupRequest) (*spi.Result, error) {
	if req.GatewayRef == "" && req.IdempotencyKey == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"simulator: a lookup requires either a reference or an idempotency key")
	}
	q := url.Values{}
	if req.GatewayRef != "" {
		q.Set("reference", req.GatewayRef)
	}
	if req.IdempotencyKey != "" {
		q.Set("idempotencyKey", req.IdempotencyKey)
	}
	return a.send(ctx, shared.OpLookup, http.MethodGet, PathPrefix+"/payments?"+q.Encode(),
		req.Credentials, "", money.Money{}, WireRequest{})
}

func (a *Adapter) send(ctx context.Context, op shared.Operation, method, path string,
	creds spi.Credentials, idemKey string, requested money.Money, body WireRequest) (*spi.Result, error) {

	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	if op != shared.OpLookup && idemKey == "" {
		return nil, apierror.New(apierror.CodeIdempotencyKeyRequired,
			"simulator: every mutating gateway call requires an idempotency key")
	}
	key, err := credential(creds, CredentialAPIKey)
	if err != nil {
		return nil, err
	}

	var raw []byte
	if method != http.MethodGet {
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, apierror.Wrap(err, apierror.CodeInternalError, "simulator: the request could not be encoded")
		}
	}
	headers := map[string]string{
		APIKeyHeader:          key,
		"Accept":              "application/json",
		httpx.OperationHeader: op.String(),
	}
	if len(raw) > 0 {
		headers["Content-Type"] = "application/json"
	}
	if idemKey != "" {
		headers[IdempotencyHeader] = idemKey
	}

	resp, err := a.client.Do(&spi.HTTPRequest{
		Ctx: ctx, Method: method, URL: a.cfg.BaseURL + path, Headers: headers, Body: raw,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"simulator: the transport returned neither a response nor an error")
	}
	if resp.Timeout {
		if op.IsMoneyMoving() {
			return nil, apierror.Wrap(spi.ErrOutcomeUnknown, apierror.CodeGatewayTimeout,
				"simulator: the request was written and the deadline expired; the outcome is unknown")
		}
		return nil, apierror.New(apierror.CodeGatewayTimeout, "simulator: the request timed out")
	}
	if resp.StatusCode >= 500 && op.IsMoneyMoving() {
		return nil, apierror.Wrap(spi.ErrOutcomeUnknown, apierror.CodeGatewayTimeout,
			"simulator: the gateway returned "+strconv.Itoa(resp.StatusCode)+
				" and does not guarantee the request was not processed")
	}
	if err := checkStatus(resp); err != nil {
		return nil, err
	}

	var wire WireResponse
	if err := decodeStrict(resp.Body, &wire); err != nil {
		if op.IsMoneyMoving() {
			// An answer we cannot read, on a call that moves money, is the SPI's definition of an
			// unknown outcome: the gateway acted and only our reading failed.
			return nil, apierror.Wrap(spi.ErrOutcomeUnknown, apierror.CodeGatewayTimeout,
				"simulator: the gateway responded but the body could not be parsed; the outcome is unknown")
		}
		return nil, err
	}
	return toResult(&wire, requested, idemKey, a.clock.Now(), resp.Latency)
}

func checkStatus(resp *spi.HTTPResponse) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var e WireError
	_ = json.Unmarshal(resp.Body, &e)
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return apierror.Wrap(spi.ErrCredentialsInvalid, apierror.CodeGatewayAuthenticationFailed,
			"simulator: the credentials were rejected")
	case http.StatusNotFound:
		return apierror.New(apierror.CodePaymentNotFound, "simulator: no such resource")
	case http.StatusTooManyRequests:
		return apierror.New(apierror.CodeRateLimited, "simulator: rate limit exceeded")
	default:
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"simulator: HTTP %d (%s)", resp.StatusCode, e.Code)
	}
}

// credential reads a required credential field, naming the field and never the value.
func credential(c spi.Credentials, field string) (string, error) {
	v, ok := c.Get(field)
	if !ok || v == "" {
		return "", apierror.Wrapf(spi.ErrCredentialsInvalid, apierror.CodeGatewayNotConfigured,
			"simulator: the credential field %q is missing", field)
	}
	return v, nil
}

// ProvisionerAdapter is the HTTP client for the simulator's onboarding endpoints.
type ProvisionerAdapter struct {
	cfg    spi.Config
	client spi.HTTPDoer
	bound  spi.Credentials
}

var _ spi.GatewayProvisioner = (*ProvisionerAdapter)(nil)

// NewProvisionerAdapter builds the onboarding client.
func NewProvisionerAdapter(cfg spi.Config) (*ProvisionerAdapter, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &ProvisionerAdapter{cfg: cfg, client: cfg.HTTPClient}, nil
}

// WithCredentials binds credentials for the compensations, whose SPI signatures carry none.
func (p *ProvisionerAdapter) WithCredentials(creds spi.Credentials) *ProvisionerAdapter {
	c := *p
	c.bound = creds
	return &c
}

// ID returns the registry slug.
func (p *ProvisionerAdapter) ID() shared.GatewayID { return GatewayID }

// Provision creates a simulated sub-merchant. It is idempotent on the merchant id.
func (p *ProvisionerAdapter) Provision(ctx context.Context, req spi.ProvisionRequest) (*spi.ProvisionResult, error) {
	if req.IdempotencyKey == "" {
		return nil, apierror.New(apierror.CodeIdempotencyKeyRequired,
			"simulator: provisioning requires an idempotency key")
	}
	currencies := make([]string, 0, len(req.Currencies))
	for _, c := range req.Currencies {
		currencies = append(currencies, string(c))
	}
	raw, err := json.Marshal(WireProvisionRequest{
		IdempotencyKey: req.IdempotencyKey,
		MerchantID:     req.MerchantID.String(),
		LegalName:      req.LegalName,
		Country:        string(req.Country),
		MCC:            string(req.MCC),
		Currencies:     currencies,
	})
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "simulator: the request could not be encoded")
	}
	resp, err := p.do(ctx, shared.OpProvision, http.MethodPost, PathPrefix+"/accounts",
		req.Credentials, req.IdempotencyKey, raw)
	if err != nil {
		return nil, err
	}
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	var out WireProvisionResponse
	if err := decodeStrict(resp.Body, &out); err != nil {
		return nil, err
	}
	if out.AccountID == "" {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"simulator: the provision response carries no account id")
	}
	return &spi.ProvisionResult{
		ExternalAccountID:   out.AccountID,
		Status:              out.Status,
		RequiresAction:      out.RequiresAction,
		ActionURL:           out.ActionURL,
		PendingRequirements: out.PendingRequirements,
		RawStatus:           out.Status,
	}, nil
}

// Deprovision removes the simulated account, tolerating its absence — the compensation contract.
func (p *ProvisionerAdapter) Deprovision(ctx context.Context, externalAccountID string) error {
	if externalAccountID == "" {
		return nil
	}
	resp, err := p.do(ctx, shared.OpProvision, http.MethodDelete,
		PathPrefix+"/accounts/"+url.PathEscape(externalAccountID), p.bound, "", nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return checkStatus(resp)
}

// RegisterWebhook subscribes an ingress endpoint and returns the signing secret.
func (p *ProvisionerAdapter) RegisterWebhook(ctx context.Context, req spi.WebhookRegistrationRequest) (*spi.WebhookRegistration, error) {
	if req.URL == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"simulator: a webhook registration requires the ingress URL")
	}
	raw, err := json.Marshal(WireWebhookRequest{
		IdempotencyKey: req.IdempotencyKey,
		AccountID:      req.ExternalAccountID,
		URL:            req.URL,
		EventTypes:     req.EventTypes,
	})
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "simulator: the request could not be encoded")
	}
	resp, err := p.do(ctx, shared.OpWebhook, http.MethodPost, PathPrefix+"/webhooks",
		req.Credentials, req.IdempotencyKey, raw)
	if err != nil {
		return nil, err
	}
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	var out WireWebhookResponse
	if err := decodeStrict(resp.Body, &out); err != nil {
		return nil, err
	}
	return &spi.WebhookRegistration{
		RegistrationID: out.RegistrationID,
		SigningSecret:  out.SigningSecret,
		URL:            out.URL,
	}, nil
}

// UnregisterWebhook removes the subscription, tolerating its absence.
func (p *ProvisionerAdapter) UnregisterWebhook(ctx context.Context, externalAccountID, registrationID string) error {
	if registrationID == "" {
		return nil
	}
	resp, err := p.do(ctx, shared.OpWebhook, http.MethodDelete,
		PathPrefix+"/webhooks/"+url.PathEscape(registrationID), p.bound, "", nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return checkStatus(resp)
}

// VerifyCredentials performs the cheapest authenticated call the simulator offers, which has no
// side effects.
func (p *ProvisionerAdapter) VerifyCredentials(ctx context.Context, creds spi.Credentials) error {
	resp, err := p.do(ctx, shared.OpLookup, http.MethodGet, PathPrefix+"/ping", creds, "", nil)
	if err != nil {
		return err
	}
	return checkStatus(resp)
}

func (p *ProvisionerAdapter) do(ctx context.Context, op shared.Operation, method, path string,
	creds spi.Credentials, idemKey string, body []byte) (*spi.HTTPResponse, error) {

	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	effective := creds
	if len(effective.Values) == 0 {
		effective = p.bound
	}
	key, err := credential(effective, CredentialAPIKey)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{
		APIKeyHeader:          key,
		"Accept":              "application/json",
		httpx.OperationHeader: op.String(),
	}
	if len(body) > 0 {
		headers["Content-Type"] = "application/json"
	}
	if idemKey != "" {
		headers[IdempotencyHeader] = idemKey
	}
	resp, err := p.client.Do(&spi.HTTPRequest{
		Ctx: ctx, Method: method, URL: p.cfg.BaseURL + path, Headers: headers, Body: body,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"simulator: the transport returned neither a response nor an error")
	}
	if resp.Timeout {
		return nil, apierror.New(apierror.CodeGatewayTimeout, "simulator: the provisioning call timed out")
	}
	return resp, nil
}
