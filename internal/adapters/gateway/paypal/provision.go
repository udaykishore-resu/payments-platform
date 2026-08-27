package paypal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Provisioner is the onboarding half of the PayPal integration, via partner referrals.
//
// PayPal is the one gateway whose provisioning the platform cannot complete on its own. The
// referral call returns a hosted URL, and the merchant must open it and grant consent inside
// PayPal's own flow — there is no API that substitutes for that click. This is why
// spi.ProvisionResult carries RequiresAction and ActionURL at all: the field exists for this
// gateway, and the onboarding workflow parks on a signal wait rather than treating an incomplete
// provision as a failure.
type Provisioner struct {
	cfg    spi.Config
	client spi.HTTPDoer
	clock  shared.Clock
	tokens *tokenCache
	bound  spi.Credentials
}

var _ spi.GatewayProvisioner = (*Provisioner)(nil)

// NewProvisioner builds the onboarding adapter.
func NewProvisioner(cfg spi.Config) (*Provisioner, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Provisioner{cfg: cfg, client: cfg.HTTPClient, clock: cfg.Clock, tokens: newTokenCache(cfg.Clock)}, nil
}

// WithCredentials binds credentials for the SPI methods whose signatures carry none. See the
// identical note on the Stripe provisioner.
func (p *Provisioner) WithCredentials(creds spi.Credentials) *Provisioner {
	c := *p
	c.bound = creds
	return &c
}

func (p *Provisioner) credsFor(req spi.Credentials) spi.Credentials {
	if len(req.Values) > 0 {
		return req
	}
	return p.bound
}

// ID returns the registry slug.
func (p *Provisioner) ID() shared.GatewayID { return GatewayID }

type referralRequest struct {
	TrackingID     string           `json:"tracking_id"`
	Operations     []referralOp     `json:"operations"`
	Products       []string         `json:"products"`
	LegalConsents  []legalConsent   `json:"legal_consents"`
	PartnerConfig  *partnerOverride `json:"partner_config_override,omitempty"`
	BusinessEntity *businessEntity  `json:"business_entity,omitempty"`
}

type referralOp struct {
	Operation          string              `json:"operation"`
	APIIntegrationPref *apiIntegrationPref `json:"api_integration_preference,omitempty"`
}

type apiIntegrationPref struct {
	RestAPIIntegration restAPIIntegration `json:"rest_api_integration"`
}

type restAPIIntegration struct {
	IntegrationMethod string            `json:"integration_method"`
	IntegrationType   string            `json:"integration_type"`
	ThirdPartyDetails thirdPartyDetails `json:"third_party_details"`
}

type thirdPartyDetails struct {
	Features []string `json:"features"`
}

type legalConsent struct {
	Type    string `json:"type"`
	Granted bool   `json:"granted"`
}

type partnerOverride struct {
	ReturnURL            string `json:"return_url,omitempty"`
	ReturnURLDescription string `json:"return_url_description,omitempty"`
	PartnerLogoURL       string `json:"partner_logo_url,omitempty"`
	ShowAddCreditCard    bool   `json:"show_add_credit_card"`
}

type businessEntity struct {
	BusinessType     *namedType   `json:"business_type,omitempty"`
	BusinessIndustry *industry    `json:"business_industry,omitempty"`
	Names            []entityName `json:"names,omitempty"`
	Addresses        []entityAddr `json:"addresses,omitempty"`
	Emails           []entityMail `json:"emails,omitempty"`
	Website          string       `json:"website,omitempty"`
}

type namedType struct {
	Type string `json:"type"`
}

type industry struct {
	Category    string `json:"category,omitempty"`
	MCCCode     string `json:"mcc_code"`
	Subcategory string `json:"subcategory,omitempty"`
}

type entityName struct {
	Type             string `json:"type"`
	BusinessName     string `json:"business_name"`
	BusinessNameType string `json:"business_name_type,omitempty"`
}

type entityAddr struct {
	Type         string `json:"type"`
	AddressLine1 string `json:"address_line_1,omitempty"`
	AddressLine2 string `json:"address_line_2,omitempty"`
	AdminArea1   string `json:"admin_area_1,omitempty"`
	AdminArea2   string `json:"admin_area_2,omitempty"`
	PostalCode   string `json:"postal_code,omitempty"`
	CountryCode  string `json:"country_code"`
}

type entityMail struct {
	Type  string `json:"type"`
	Email string `json:"email"`
}

// Provision creates a partner referral and returns the hosted consent URL.
//
// `tracking_id` is the merchant identifier and is what makes this idempotent: PayPal rejects a
// second referral for a tracking id that already has one, and the readiness lookup is keyed off the
// same value. That, plus the `PayPal-Request-Id` header, means a retry after a crash converges on
// the existing referral rather than creating a second merchant relationship — which, as at the
// other two gateways, is a thing PayPal will not let the platform undo.
//
// The result deliberately carries RequiresAction: this provisioning is genuinely incomplete until a
// human clicks through PayPal's consent flow, and reporting it as done would let the router select
// a gateway that cannot yet accept a payment.
func (p *Provisioner) Provision(ctx context.Context, req spi.ProvisionRequest) (*spi.ProvisionResult, error) {
	if req.IdempotencyKey == "" {
		return nil, apierror.New(apierror.CodeIdempotencyKeyRequired,
			"paypal: provisioning requires an idempotency key so a retry after a crash cannot create a second referral")
	}
	body := referralRequest{
		TrackingID: req.MerchantID.String(),
		Products:   []string{"PPCP"},
		Operations: []referralOp{{
			Operation: "API_INTEGRATION",
			APIIntegrationPref: &apiIntegrationPref{
				RestAPIIntegration: restAPIIntegration{
					IntegrationMethod: "PAYPAL",
					IntegrationType:   "THIRD_PARTY",
					ThirdPartyDetails: thirdPartyDetails{Features: []string{
						"PAYMENT", "REFUND", "PARTNER_FEE",
						"DELAY_FUNDS_DISBURSEMENT", "ACCESS_MERCHANT_INFORMATION",
					}},
				},
			},
		}},
		// SHARE_DATA_CONSENT is what lets the platform read the merchant's integration status
		// afterwards. Without it the referral succeeds and every readiness poll returns nothing,
		// which presents as an onboarding that never completes.
		LegalConsents: []legalConsent{{Type: "SHARE_DATA_CONSENT", Granted: true}},
		BusinessEntity: &businessEntity{
			BusinessType:     &namedType{Type: "CORPORATION"},
			BusinessIndustry: &industry{MCCCode: string(req.MCC)},
			Names: []entityName{{
				Type: "LEGAL_NAME", BusinessName: req.LegalName, BusinessNameType: "LEGAL_NAME",
			}},
			Emails:  []entityMail{{Type: "CUSTOMER_SERVICE", Email: req.SupportEmail}},
			Website: req.WebsiteURL,
		},
	}
	addr := entityAddr{
		Type:        "WORK",
		AdminArea1:  req.Region,
		AdminArea2:  req.City,
		PostalCode:  req.PostalCode,
		CountryCode: string(req.Country),
	}
	if len(req.AddressLines) > 0 {
		addr.AddressLine1 = req.AddressLines[0]
	}
	if len(req.AddressLines) > 1 {
		addr.AddressLine2 = req.AddressLines[1]
	}
	body.BusinessEntity.Addresses = []entityAddr{addr}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "paypal: the request could not be encoded")
	}
	resp, err := p.do(ctx, callSpec{
		op:        shared.OpProvision,
		method:    http.MethodPost,
		path:      "/v2/customer/partner-referrals",
		body:      raw,
		creds:     req.Credentials,
		requestID: req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	if err := p.checkStatus(resp); err != nil {
		return nil, err
	}
	var pr partnerReferral
	if err := decode(resp.Body, &pr); err != nil {
		return nil, err
	}
	actionURL := linkByRel(pr.Links, "action_url")
	if actionURL == "" {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"paypal: the referral response carries no action_url, so the merchant has nowhere to consent")
	}
	return &spi.ProvisionResult{
		// The tracking id is the platform's handle until PayPal issues a merchant id, which happens
		// only after consent. Using it as the external account id means the readiness poll and the
		// compensation both have something to work with in the meantime.
		ExternalAccountID: req.MerchantID.String(),
		Status:            "AWAITING_MERCHANT_CONSENT",
		RequiresAction:    true,
		ActionURL:         actionURL,
		PendingRequirements: []string{
			"merchant must complete PayPal's hosted consent flow",
		},
		RawStatus: "referral_created",
	}, nil
}

// Deprovision records that the referral is abandoned, and tolerates its absence.
//
// PayPal offers the partner no way to delete a referral or to revoke a merchant's account: consent
// is the merchant's to grant and the merchant's to withdraw, and the PayPal account itself persists
// regardless. So this compensation does the only two honest things available: it confirms whether
// the integration exists, and it reports success either way so the saga can proceed. What it must
// not do is pretend to have deleted something.
//
// A 404 means the merchant never completed consent, which is the common case for this compensation
// — it runs precisely when a later onboarding step failed, which is usually before the merchant
// ever clicked through. Treating that as success is required, not lenient: a compensation that
// fails because its target does not exist leaves the saga permanently stuck.
func (p *Provisioner) Deprovision(ctx context.Context, externalAccountID string) error {
	if externalAccountID == "" {
		return nil
	}
	partnerID, err := credential(p.bound, CredentialPartnerID)
	if err != nil {
		// Without the partner id the confirmation call cannot be made. Since the compensation has
		// nothing it can actually undo, an unconfigured partner id must not block the saga.
		return nil //nolint:nilerr // an unconfigured partner id leaves this compensation with nothing to undo; reporting failure would wedge the saga permanently (see the doc comment)
	}
	resp, err := p.do(ctx, callSpec{
		op:     shared.OpProvision,
		method: http.MethodGet,
		path: "/v1/customer/partners/" + url.PathEscape(partnerID) +
			"/merchant-integrations/" + url.PathEscape(externalAccountID),
		creds: p.bound,
	})
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if err := p.checkStatus(resp); err != nil {
		return err
	}
	// The integration exists and cannot be removed by the partner. The connection is marked
	// abandoned by the caller; this method's contract is satisfied by having established the fact.
	return nil
}

// RegisterWebhook subscribes the platform's ingress endpoint to PayPal's events.
//
// PayPal returns no signing secret: verification is performed by calling PayPal back with the
// webhook *id*, so the id is what goes into SigningSecret. That is a deliberate reuse of the field
// rather than an abuse of it — the field's contract is "the material the verifier needs", and for
// PayPal that material is an identifier rather than a key. The verifier's doc comment says so.
func (p *Provisioner) RegisterWebhook(ctx context.Context, req spi.WebhookRegistrationRequest) (*spi.WebhookRegistration, error) {
	if req.URL == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"paypal: a webhook registration requires the platform ingress URL")
	}
	events := req.EventTypes
	if len(events) == 0 {
		events = DefaultWebhookEvents()
	}
	types := make([]map[string]string, 0, len(events))
	for _, e := range events {
		types = append(types, map[string]string{"name": e})
	}
	raw, err := json.Marshal(map[string]any{"url": req.URL, "event_types": types})
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "paypal: the request could not be encoded")
	}
	resp, err := p.do(ctx, callSpec{
		op:        shared.OpWebhook,
		method:    http.MethodPost,
		path:      "/v1/notifications/webhooks",
		body:      raw,
		creds:     req.Credentials,
		requestID: req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	if err := p.checkStatus(resp); err != nil {
		return nil, err
	}
	var wh registeredWebhook
	if err := decode(resp.Body, &wh); err != nil {
		return nil, err
	}
	if wh.ID == "" {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"paypal: the webhook response carries no id")
	}
	return &spi.WebhookRegistration{RegistrationID: wh.ID, SigningSecret: wh.ID, URL: wh.URL}, nil
}

// UnregisterWebhook removes the subscription, tolerating its absence.
func (p *Provisioner) UnregisterWebhook(ctx context.Context, externalAccountID, registrationID string) error {
	if registrationID == "" {
		return nil
	}
	resp, err := p.do(ctx, callSpec{
		op:     shared.OpWebhook,
		method: http.MethodDelete,
		path:   "/v1/notifications/webhooks/" + url.PathEscape(registrationID),
		creds:  p.bound,
	})
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return p.checkStatus(resp)
}

// VerifyCredentials exchanges an OAuth token and nothing else.
//
// For PayPal the token exchange *is* the cheapest authenticated call: it proves the client id and
// secret are valid, it has no side effects, and it is the call every other operation depends on. A
// probe that instead listed orders would be more expensive, would be rate-limited per merchant, and
// would prove strictly less — a valid token with no order-read scope would still pass.
func (p *Provisioner) VerifyCredentials(ctx context.Context, creds spi.Credentials) error {
	_, err := p.tokens.token(ctx, p.client, p.cfg.BaseURL, p.credsFor(creds))
	return err
}

// MerchantIntegrationStatus reads the readiness of a referred merchant.
//
// It is exported beyond the SPI because the onboarding workflow polls it while waiting for the
// merchant's consent, and because the assertions it backs — payments_receivable,
// primary_email_confirmed, a non-empty oauth_integrations — are the L3 checks that decide whether
// the connection may be certified. The SPI has no method for "is provisioning finished yet",
// because only PayPal needs one.
func (p *Provisioner) MerchantIntegrationStatus(ctx context.Context, creds spi.Credentials, trackingOrMerchantID string) (*spi.ProvisionResult, error) {
	partnerID, err := credential(p.credsFor(creds), CredentialPartnerID)
	if err != nil {
		return nil, err
	}
	resp, err := p.do(ctx, callSpec{
		op:     shared.OpLookup,
		method: http.MethodGet,
		path: "/v1/customer/partners/" + url.PathEscape(partnerID) +
			"/merchant-integrations/" + url.PathEscape(trackingOrMerchantID),
		creds: creds,
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return &spi.ProvisionResult{
			ExternalAccountID:   trackingOrMerchantID,
			Status:              "AWAITING_MERCHANT_CONSENT",
			RequiresAction:      true,
			PendingRequirements: []string{"merchant has not completed PayPal's hosted consent flow"},
			RawStatus:           "not_found",
		}, nil
	}
	if err := p.checkStatus(resp); err != nil {
		return nil, err
	}
	var mi merchantIntegration
	if err := decode(resp.Body, &mi); err != nil {
		return nil, err
	}
	out := &spi.ProvisionResult{
		ExternalAccountID: firstNonEmpty(mi.MerchantID, trackingOrMerchantID),
		RawStatus:         "payments_receivable=" + boolString(mi.PaymentsReceivable),
	}
	var pending []string
	if !mi.PaymentsReceivable {
		pending = append(pending, "payments_receivable")
	}
	if !mi.PrimaryEmailConfirmed {
		pending = append(pending, "primary_email_confirmed")
	}
	if len(mi.OAuthIntegrations) == 0 {
		pending = append(pending, "oauth_integrations")
	}
	for _, prod := range mi.Products {
		if prod.VettingStatus != "" && prod.VettingStatus != "SUBSCRIBED" {
			pending = append(pending, "product:"+prod.Name+":"+prod.VettingStatus)
		}
	}
	out.PendingRequirements = pending
	if len(pending) == 0 {
		out.Status = "ACTIVE"
	} else {
		out.Status = "PENDING"
		out.RequiresAction = !mi.PrimaryEmailConfirmed || len(mi.OAuthIntegrations) == 0
	}
	return out, nil
}

// DefaultWebhookEvents is the event set the platform subscribes to.
//
// MERCHANT.PARTNER-CONSENT.REVOKED is in the list deliberately: a merchant can withdraw the
// platform's access from inside PayPal at any moment, and without this event the platform discovers
// it on the next payment — as a hard failure, on live traffic.
func DefaultWebhookEvents() []string {
	return []string{
		"PAYMENT.AUTHORIZATION.CREATED",
		"PAYMENT.AUTHORIZATION.VOIDED",
		"PAYMENT.CAPTURE.COMPLETED",
		"PAYMENT.CAPTURE.DENIED",
		"PAYMENT.CAPTURE.REFUNDED",
		"PAYMENT.CAPTURE.REVERSED",
		"CHECKOUT.ORDER.APPROVED",
		"CHECKOUT.ORDER.COMPLETED",
		"CUSTOMER.DISPUTE.CREATED",
		"CUSTOMER.DISPUTE.RESOLVED",
		"MERCHANT.ONBOARDING.COMPLETED",
		"MERCHANT.PARTNER-CONSENT.REVOKED",
	}
}

func (p *Provisioner) do(ctx context.Context, spec callSpec) (*spi.HTTPResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	creds := p.credsFor(spec.creds)
	token, err := p.tokens.token(ctx, p.client, p.cfg.BaseURL, creds)
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
	resp, err := p.client.Do(&spi.HTTPRequest{
		Ctx: ctx, Method: spec.method, URL: p.cfg.BaseURL + spec.path,
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
		return nil, apierror.New(apierror.CodeGatewayTimeout, "paypal: the provisioning call timed out")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		p.tokens.invalidate(creds)
	}
	return resp, nil
}

func (p *Provisioner) checkStatus(resp *spi.HTTPResponse) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var e errorResponse
	_ = decode(resp.Body, &e)
	if e.Name == "" && e.Error == "" {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return apierror.Wrap(spi.ErrCredentialsInvalid, apierror.CodeGatewayAuthenticationFailed,
				"paypal: the request was not authorized")
		}
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"paypal: HTTP %d with an unparseable body", resp.StatusCode)
	}
	return mapErrorName(resp.StatusCode, &e)
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
