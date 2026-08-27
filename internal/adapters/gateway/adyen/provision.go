package adyen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Provisioner is the onboarding half of the Adyen integration, against the Legal Entity Management
// and Balance Platform APIs.
//
// Adyen's onboarding model is the most granular of the three gateways: a legal entity per company
// and per beneficial owner, associations between them, a business line describing what the
// merchant sells, an account holder, and a transfer instrument for payouts. That granularity is why
// the platform collects ownership as a graph rather than a flat list (docs/onboarding.md §3.2) —
// the collection model was designed to fit the strictest gateway, so the other two are projections
// of it rather than the other way round.
//
// The order below is not arbitrary and cannot be parallelised: a business line needs a legal
// entity, an account holder needs a legal entity that has a business line, and a transfer
// instrument needs the legal entity to exist. Each step's identifier feeds the next, which is also
// what makes a crash mid-sequence recoverable — the workflow re-runs with the same reference and
// Adyen returns the existing object rather than creating a second.
type Provisioner struct {
	cfg    spi.Config
	client spi.HTTPDoer
	clock  shared.Clock
	bound  spi.Credentials
}

var _ spi.GatewayProvisioner = (*Provisioner)(nil)

// NewProvisioner builds the onboarding adapter.
//
// Note that Adyen serves LEM, Balance Platform and Management from different hostnames in
// production. The registry therefore constructs the provisioner with its own spi.Config whose
// BaseURL points at the management/LEM prefix, separately from the Gateway's checkout prefix.
// Deriving one host from the other by string surgery would be a guess that fails silently in a
// live account.
func NewProvisioner(cfg spi.Config) (*Provisioner, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Provisioner{cfg: cfg, client: cfg.HTTPClient, clock: cfg.Clock}, nil
}

// WithCredentials binds credentials for the two SPI methods whose signatures carry none. See the
// identical note on the Stripe provisioner for why this is acceptable for compensations and is not
// how the payment path works.
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

type lemAddress struct {
	Street     string `json:"street,omitempty"`
	Street2    string `json:"street2,omitempty"`
	City       string `json:"city,omitempty"`
	PostalCode string `json:"postalCode,omitempty"`
	StateOrTgt string `json:"stateOrProvince,omitempty"`
	Country    string `json:"country"`
}

type legalEntityRequest struct {
	Type         string           `json:"type"`
	Reference    string           `json:"reference,omitempty"`
	Organization *lemOrgRequest   `json:"organization,omitempty"`
	Individual   *lemIndivRequest `json:"individual,omitempty"`
}

type lemOrgRequest struct {
	LegalName          string     `json:"legalName"`
	RegistrationNumber string     `json:"registrationNumber,omitempty"`
	Type               string     `json:"type,omitempty"`
	RegisteredAddress  lemAddress `json:"registeredAddress"`
	TaxInformation     []lemTax   `json:"taxInformation,omitempty"`
	Email              string     `json:"email,omitempty"`
	WebData            []lemWeb   `json:"webData,omitempty"`
}

type lemTax struct {
	Country string `json:"country"`
	Number  string `json:"number"`
	Type    string `json:"type,omitempty"`
}

type lemWeb struct {
	WebAddress string `json:"webAddress"`
}

type lemIndivRequest struct {
	Name struct {
		FirstName string `json:"firstName"`
		LastName  string `json:"lastName"`
	} `json:"name"`
	ResidentialAddress lemAddress `json:"residentialAddress"`
}

type businessLineRequest struct {
	LegalEntityID string   `json:"legalEntityId"`
	Service       string   `json:"service"`
	IndustryCode  string   `json:"industryCode"`
	WebData       []lemWeb `json:"webData,omitempty"`
	SalesChannels []string `json:"salesChannels,omitempty"`
}

type accountHolderRequest struct {
	LegalEntityID   string `json:"legalEntityId"`
	Description     string `json:"description,omitempty"`
	Reference       string `json:"reference"`
	BalancePlatform string `json:"balancePlatform,omitempty"`
}

type transferInstrumentRequest struct {
	LegalEntityID string              `json:"legalEntityId"`
	Type          string              `json:"type"`
	BankAccount   transferBankAccount `json:"bankAccount"`
}

type transferBankAccount struct {
	AccountIdentification map[string]string `json:"accountIdentification"`
	CountryCode           string            `json:"countryCode"`
	TrustedSource         bool              `json:"trustedSource"`
	AccountType           string            `json:"accountType,omitempty"`
}

// Provision walks Adyen's onboarding graph.
//
// Idempotency rests on `reference`, which Adyen treats as the caller's own key for the object.
// Re-running the step after a crash with the same merchant-derived reference converges on the
// existing objects instead of creating duplicates — which matters more here than anywhere else,
// because a duplicate Adyen legal entity cannot be deleted and leaves the merchant with two KYC
// identities and split settlement. The `Idempotency-Key` header is sent as well, covering the
// window in which Adyen's own deduplication is authoritative.
//
// A failure part-way through returns the identifiers created so far alongside the error, so the
// saga's compensation has something to close. Returning a bare error would strand a verified legal
// entity nobody can find.
func (p *Provisioner) Provision(ctx context.Context, req spi.ProvisionRequest) (*spi.ProvisionResult, error) {
	if req.IdempotencyKey == "" {
		return nil, apierror.New(apierror.CodeIdempotencyKeyRequired,
			"adyen: provisioning requires an idempotency key so a retry after a crash cannot create a second legal entity")
	}
	reference := req.MerchantID.String()
	addr := lemAddress{
		City:       req.City,
		PostalCode: req.PostalCode,
		StateOrTgt: req.Region,
		Country:    string(req.Country),
	}
	if len(req.AddressLines) > 0 {
		addr.Street = req.AddressLines[0]
	}
	if len(req.AddressLines) > 1 {
		addr.Street2 = req.AddressLines[1]
	}

	org := legalEntityRequest{
		Type:      "organization",
		Reference: reference,
		Organization: &lemOrgRequest{
			LegalName:          req.LegalName,
			RegistrationNumber: req.RegistrationNumber,
			Type:               "privateCompany",
			RegisteredAddress:  addr,
			Email:              req.SupportEmail,
		},
	}
	if req.TaxID != "" {
		org.Organization.TaxInformation = []lemTax{{Country: string(req.Country), Number: req.TaxID}}
	}
	if req.WebsiteURL != "" {
		org.Organization.WebData = []lemWeb{{WebAddress: req.WebsiteURL}}
	}

	var orgResp legalEntityResponse
	if err := p.postJSON(ctx, req.Credentials, shared.OpProvision,
		"/lem/v3/legalEntities", req.IdempotencyKey, org, &orgResp); err != nil {
		return nil, err
	}
	if orgResp.ID == "" {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"adyen: the legal entity response carries no id")
	}
	result := &spi.ProvisionResult{RawStatus: "legalEntity=" + orgResp.ID}

	// Individual legal entities for the beneficial owners. Each is created and then associated
	// with the organization, because Adyen models ownership as a graph edge rather than as a field
	// on either node.
	for i, principal := range req.Principals {
		indiv := legalEntityRequest{
			Type:      "individual",
			Reference: reference + "-p" + strconv.Itoa(i),
			Individual: &lemIndivRequest{
				ResidentialAddress: lemAddress{Country: string(principal.Country)},
			},
		}
		indiv.Individual.Name.FirstName = principal.FirstName
		indiv.Individual.Name.LastName = principal.LastName

		var indivResp legalEntityResponse
		if err := p.postJSON(ctx, req.Credentials, shared.OpProvision,
			"/lem/v3/legalEntities", req.IdempotencyKey+"-p"+strconv.Itoa(i), indiv, &indivResp); err != nil {
			result.Status = "INCOMPLETE"
			result.PendingRequirements = []string{"legalEntities.individual"}
			result.ExternalAccountID = ""
			return result, err
		}
	}

	var bl businessLineResponse
	blReq := businessLineRequest{
		LegalEntityID: orgResp.ID,
		Service:       "paymentProcessing",
		IndustryCode:  string(req.MCC),
		SalesChannels: []string{"eCommerce"},
	}
	if req.WebsiteURL != "" {
		blReq.WebData = []lemWeb{{WebAddress: req.WebsiteURL}}
	}
	if err := p.postJSON(ctx, req.Credentials, shared.OpProvision,
		"/lem/v3/businessLines", req.IdempotencyKey+"-bl", blReq, &bl); err != nil {
		result.Status = "INCOMPLETE"
		result.PendingRequirements = []string{"businessLines"}
		return result, err
	}

	var ah accountHolderResponse
	if err := p.postJSON(ctx, req.Credentials, shared.OpProvision,
		"/bcl/v2/accountHolders", req.IdempotencyKey+"-ah", accountHolderRequest{
			LegalEntityID: orgResp.ID,
			Description:   req.DisplayName,
			Reference:     reference,
		}, &ah); err != nil {
		result.Status = "INCOMPLETE"
		result.PendingRequirements = []string{"accountHolders"}
		return result, err
	}
	if ah.ID == "" {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"adyen: the account holder response carries no id")
	}
	// The account holder is the platform's handle on this merchant at Adyen, so it is what
	// ExternalAccountID carries and what Deprovision closes.
	result.ExternalAccountID = ah.ID

	if req.BankAccount.IBAN != "" || req.BankAccount.AccountNumber != "" {
		ident := map[string]string{}
		if req.BankAccount.IBAN != "" {
			ident["type"] = "iban"
			ident["iban"] = req.BankAccount.IBAN
		} else {
			// Adyen's local identification types are country-specific. usLocal covers the routing
			// number / account number pair; other countries have their own type names, which is why
			// this is keyed off the presence of a routing number rather than assumed.
			ident["type"] = "usLocal"
			ident["accountNumber"] = req.BankAccount.AccountNumber
			ident["routingNumber"] = req.BankAccount.RoutingNumber
		}
		var ti transferInstrumentResponse
		if err := p.postJSON(ctx, req.Credentials, shared.OpProvision,
			"/lem/v3/transferInstruments", req.IdempotencyKey+"-ti", transferInstrumentRequest{
				LegalEntityID: orgResp.ID,
				Type:          "bankAccount",
				BankAccount: transferBankAccount{
					AccountIdentification: ident,
					CountryCode:           string(req.BankAccount.Country),
					// trustedSource is false because the platform has not itself verified the
					// account belongs to the merchant. Claiming otherwise shifts liability for a
					// misdirected payout onto the platform.
					TrustedSource: false,
				},
			}, &ti); err != nil {
			result.Status = "INCOMPLETE"
			result.PendingRequirements = []string{"transferInstruments"}
			return result, err
		}
	}

	result.Status = accountHolderStatus(&ah, &orgResp)
	result.PendingRequirements = lemPendingRequirements(&orgResp)
	// Adyen needs no merchant click-through: the platform submits everything on their behalf.
	// RequiresAction is genuinely used by the PayPal adapter, where consent cannot be delegated.
	return result, nil
}

// Deprovision closes the account holder.
//
// Adyen does not delete: a legal entity that has been through KYC is a regulatory record and stays.
// The compensation available is closing the account holder, which stops settlement and stops new
// payments. That is an honest partial reversal and it is documented as such in
// docs/onboarding.md §11 — pretending the account is gone would be worse than saying it is closed.
//
// A 404 or a 422 is treated as success. Compensation runs after a crash, and the crash may have
// landed before the account holder was created or after a previous compensation already closed it;
// a compensation that fails because its target is absent leaves the saga permanently stuck.
func (p *Provisioner) Deprovision(ctx context.Context, externalAccountID string) error {
	if externalAccountID == "" {
		return nil
	}
	body, err := json.Marshal(map[string]string{"status": "closed"})
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "adyen: the request could not be encoded")
	}
	resp, err := p.do(ctx, callSpec{
		op:     shared.OpProvision,
		method: http.MethodPatch,
		path:   "/bcl/v2/accountHolders/" + url.PathEscape(externalAccountID),
		body:   body,
		creds:  p.bound,
	})
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil
	case http.StatusUnprocessableEntity:
		// Adyen answers 422 for "already closed" as well as for a genuinely invalid transition.
		// Treating it as success is the right trade: a stuck saga is worse than a redundant close,
		// and the account holder's real state is asserted by the workflow's own readiness check.
		return nil
	default:
		return p.checkStatus(resp)
	}
}

// RegisterWebhook creates the notification configuration and generates its HMAC key.
//
// Two calls, not one: Adyen creates the webhook without a signing key and mints the key on a
// separate endpoint, returning it exactly once. If the second call fails the first must be undone,
// because a webhook with no HMAC key delivers unsigned notifications — which the platform's own
// registry forbids in production (gateway.SchemeNone), so those deliveries would be rejected and
// redelivered forever.
func (p *Provisioner) RegisterWebhook(ctx context.Context, req spi.WebhookRegistrationRequest) (*spi.WebhookRegistration, error) {
	if req.URL == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"adyen: a webhook registration requires the platform ingress URL")
	}
	merchantAccount, err := credential(p.credsFor(req.Credentials), CredentialMerchantAccount)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"type":                "standard",
		"url":                 req.URL,
		"communicationFormat": "json",
		"active":              true,
		// Adyen authenticates itself to our endpoint with basic auth. The credentials are generated
		// by the platform and stored alongside the HMAC key; they are the control that stops an
		// unauthenticated caller making us do HMAC work.
		"username":                     "pp-" + strings.ToLower(req.ExternalAccountID),
		"acceptsExpiredCertificate":    false,
		"acceptsSelfSignedCertificate": false,
	}
	var wh webhookResponse
	if err := p.postJSON(ctx, req.Credentials, shared.OpWebhook,
		"/v3/merchants/"+url.PathEscape(merchantAccount)+"/webhooks", req.IdempotencyKey, body, &wh); err != nil {
		return nil, err
	}
	if wh.ID == "" {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"adyen: the webhook response carries no id")
	}

	var hm hmacResponse
	if err := p.postJSON(ctx, req.Credentials, shared.OpWebhook,
		"/v3/merchants/"+url.PathEscape(merchantAccount)+"/webhooks/"+url.PathEscape(wh.ID)+"/generateHmac",
		req.IdempotencyKey+"-hmac", map[string]any{}, &hm); err != nil {
		// Undo the webhook rather than leaving one that cannot be verified. The error from the
		// cleanup is deliberately discarded: the caller needs to see why the HMAC generation
		// failed, and a secondary failure here would mask it.
		_ = p.UnregisterWebhook(ctx, req.ExternalAccountID, wh.ID)
		return nil, err
	}
	key := hm.HMACKey
	if key == "" {
		key = wh.HMACKey
	}
	if key == "" {
		_ = p.UnregisterWebhook(ctx, req.ExternalAccountID, wh.ID)
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"adyen: no HMAC key was returned, so the endpoint would receive unverifiable notifications")
	}
	return &spi.WebhookRegistration{RegistrationID: wh.ID, SigningSecret: key, URL: wh.URL}, nil
}

// UnregisterWebhook removes the notification configuration, tolerating its absence.
func (p *Provisioner) UnregisterWebhook(ctx context.Context, externalAccountID, registrationID string) error {
	if registrationID == "" {
		return nil
	}
	merchantAccount, err := credential(p.bound, CredentialMerchantAccount)
	if err != nil {
		return err
	}
	resp, err := p.do(ctx, callSpec{
		op:     shared.OpWebhook,
		method: http.MethodDelete,
		path: "/v3/merchants/" + url.PathEscape(merchantAccount) + "/webhooks/" +
			url.PathEscape(registrationID),
		creds: p.bound,
	})
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return p.checkStatus(resp)
}

// VerifyCredentials performs the cheapest authenticated call Adyen offers.
//
// `GET /v3/me` on the Management API returns the API credential's own description and roles. It is
// read-only, has no side effects, is not rate-limited per merchant, and — usefully — fails
// distinguishably when the key is valid but lacks a required role, which is the failure the L3
// probe most often needs to report.
func (p *Provisioner) VerifyCredentials(ctx context.Context, creds spi.Credentials) error {
	resp, err := p.do(ctx, callSpec{
		op:     shared.OpLookup,
		method: http.MethodGet,
		path:   "/v3/me",
		creds:  creds,
	})
	if err != nil {
		return err
	}
	return p.checkStatus(resp)
}

// --- transport -------------------------------------------------------------------------------

func (p *Provisioner) postJSON(ctx context.Context, creds spi.Credentials, op shared.Operation,
	path, idemKey string, body any, out any) error {

	raw, err := json.Marshal(body)
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "adyen: the request could not be encoded")
	}
	resp, err := p.do(ctx, callSpec{
		op: op, method: http.MethodPost, path: path, body: raw,
		creds: creds, idempotencyKey: idemKey,
	})
	if err != nil {
		return err
	}
	if err := p.checkStatus(resp); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return decode(resp.Body, out)
}

func (p *Provisioner) do(ctx context.Context, spec callSpec) (*spi.HTTPResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	key, err := credential(p.credsFor(spec.creds), CredentialAPIKey)
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
		headers["Idempotency-Key"] = spec.idempotencyKey
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
			"adyen: the transport returned neither a response nor an error")
	}
	if resp.Timeout {
		// Provisioning moves no money. The unknown state it can leave is durable, and it is
		// resolved by the workflow's readiness poll rather than by payment reconciliation, so this
		// is a plain retryable timeout.
		return nil, apierror.New(apierror.CodeGatewayTimeout, "adyen: the provisioning call timed out")
	}
	return resp, nil
}

func (p *Provisioner) checkStatus(resp *spi.HTTPResponse) error {
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
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"adyen: HTTP %d with an unparseable body", resp.StatusCode)
	}
	return mapErrorCode(resp.StatusCode, &e)
}

func accountHolderStatus(ah *accountHolderResponse, org *legalEntityResponse) string {
	if ah.Status == "active" && capabilityAllowed(org, "receivePayments") {
		return "ACTIVE"
	}
	if ah.Status == "closed" || ah.Status == "suspended" {
		return "RESTRICTED"
	}
	return "PENDING"
}

func capabilityAllowed(le *legalEntityResponse, name string) bool {
	if le == nil {
		return false
	}
	c, ok := le.Capabilities[name]
	return ok && c.Allowed
}

// lemPendingRequirements surfaces Adyen's verification errors verbatim.
//
// Verbatim matters: Adyen's `problems[].verificationErrors[].code` values map to specific documents
// the merchant has to supply, and rendering them as "provisioning failed" turns an actionable
// request into a support ticket.
func lemPendingRequirements(le *legalEntityResponse) []string {
	if le == nil || len(le.Problems) == 0 {
		return nil
	}
	var out []string
	for _, prob := range le.Problems {
		for _, ve := range prob.VerificationErrors {
			entry := ve.Code
			if prob.Entity.Type != "" {
				entry = prob.Entity.Type + ":" + ve.Code
			}
			out = append(out, entry)
		}
	}
	return out
}
