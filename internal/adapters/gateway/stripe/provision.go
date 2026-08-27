package stripe

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Provisioner is the onboarding half of the Stripe integration: Connect account creation,
// beneficial owners, the payout destination and the webhook subscription.
//
// It is a distinct type from Gateway because the two have different consumers and different
// authority. The workflow worker provisions and never charges; the orchestrator charges and can
// never create an account. Splitting the interfaces means that separation is enforced by what a
// component can *hold*, not by what it remembers not to call.
type Provisioner struct {
	cfg    spi.Config
	client spi.HTTPDoer
	clock  shared.Clock
	// bound holds the credentials for the two SPI methods whose signatures carry none:
	// Deprovision and UnregisterWebhook. See WithCredentials for why that is acceptable here and
	// would not be on the payment path.
	bound spi.Credentials
}

var _ spi.GatewayProvisioner = (*Provisioner)(nil)

// NewProvisioner builds the onboarding adapter.
func NewProvisioner(cfg spi.Config) (*Provisioner, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Provisioner{cfg: cfg, client: cfg.HTTPClient, clock: cfg.Clock}, nil
}

// WithCredentials returns a copy bound to resolved credentials.
//
// Two SPI methods — Deprovision and UnregisterWebhook — are compensations, and a compensation
// signature that carried credentials would mean the saga had to keep them alive for the entire
// duration of an onboarding case, which can be days. Binding them to a short-lived provisioner
// instead means the material lives exactly as long as the workflow activity that resolved it.
//
// This is deliberately *not* how the payment path works: spi.PaymentGateway takes credentials on
// every request, because an orchestrator's gateway objects are process-lifetime singletons and a
// secret key in a singleton's field is a secret in every heap dump for as long as the pod runs.
func (p *Provisioner) WithCredentials(creds spi.Credentials) *Provisioner {
	c := *p
	c.bound = creds
	return &c
}

// credsFor prefers the credentials supplied on the request and falls back to the bound set, so a
// caller that has them need not bind, and a compensation that has no request still works.
func (p *Provisioner) credsFor(req spi.Credentials) spi.Credentials {
	if len(req.Values) > 0 {
		return req
	}
	return p.bound
}

// ID returns the registry slug.
func (p *Provisioner) ID() shared.GatewayID { return GatewayID }

// Provision creates the Connect account, its persons and its payout destination.
//
// Idempotency here is not a nicety. The onboarding workflow retries after a crash, and a second
// connected account for one merchant is a manual-cleanup incident at Stripe — the account cannot
// be deleted once it has any activity, and the merchant ends up with two identities and split
// settlement. Three mechanisms stack:
//
//  1. `Idempotency-Key` on the create, derived from the merchant, so Stripe itself deduplicates
//     within its 24-hour window.
//  2. `metadata[platform_merchant_id]`, so a search can find an account created before a crash
//     even after the window closes.
//  3. Every subsequent call is keyed off the account id the create returned, so a retry that
//     finds the existing account converges rather than forking.
//
// Persons and the external account are created after the account, in that order, and a failure
// part-way through is *not* rolled back here: it returns the account id along with the error so
// the saga's compensation has something to deprovision. Swallowing the id to return a clean error
// would strand the account.
func (p *Provisioner) Provision(ctx context.Context, req spi.ProvisionRequest) (*spi.ProvisionResult, error) {
	if req.IdempotencyKey == "" {
		return nil, apierror.New(apierror.CodeIdempotencyKeyRequired,
			"stripe: provisioning requires an idempotency key so a retry after a crash cannot create a second connected account")
	}

	f := &form{}
	// `custom` gives the platform full control of the onboarding experience and makes the platform
	// liable for the account's obligations. `express` hands the KYC UI to Stripe. The platform
	// collects KYB itself (see docs/onboarding.md §3), so `custom` is what the collected data is
	// for; the type is nonetheless a parameter of the request in spirit, and is chosen here rather
	// than being configurable because mixing the two under one integration produces accounts with
	// inconsistent requirement sets.
	f.set("type", "custom")
	f.set("country", string(req.Country))
	f.set("email", req.SupportEmail)
	f.set("business_type", "company")
	f.set("business_profile[mcc]", string(req.MCC))
	f.set("business_profile[url]", req.WebsiteURL)
	f.set("business_profile[name]", req.DisplayName)
	f.set("company[name]", req.LegalName)
	f.set("company[tax_id]", req.TaxID)
	f.set("company[registration_number]", req.RegistrationNumber)
	if len(req.AddressLines) > 0 {
		f.set("company[address][line1]", req.AddressLines[0])
	}
	if len(req.AddressLines) > 1 {
		f.set("company[address][line2]", req.AddressLines[1])
	}
	f.set("company[address][city]", req.City)
	f.set("company[address][state]", req.Region)
	f.set("company[address][postal_code]", req.PostalCode)
	f.set("company[address][country]", string(req.Country))
	f.setBool("capabilities[card_payments][requested]", true)
	f.setBool("capabilities[transfers][requested]", true)
	f.set("metadata[platform_merchant_id]", req.MerchantID.String())
	f.set("metadata["+metaIdempotencyKey+"]", req.IdempotencyKey)

	resp, err := p.do(ctx, callSpec{
		op:             shared.OpProvision,
		method:         http.MethodPost,
		path:           "/v1/accounts",
		body:           f.bytes(),
		creds:          req.Credentials,
		idempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	if err := p.checkStatus(resp); err != nil {
		return nil, err
	}
	var acct account
	if err := decode(resp.Body, &acct); err != nil {
		return nil, err
	}
	if acct.ID == "" {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"stripe: the account response carries no account id")
	}

	result := &spi.ProvisionResult{
		ExternalAccountID: acct.ID,
		RawStatus:         accountRawStatus(&acct),
	}

	for i, principal := range req.Principals {
		if err := p.addPerson(ctx, req, acct.ID, i, principal); err != nil {
			// The account exists. Returning it alongside the error is what lets the saga
			// compensate; returning only the error would leak a connected account nobody owns.
			result.Status = "INCOMPLETE"
			result.PendingRequirements = []string{"persons"}
			return result, err
		}
	}

	if req.BankAccount.AccountNumber != "" || req.BankAccount.IBAN != "" {
		if err := p.attachExternalAccount(ctx, req, acct.ID); err != nil {
			result.Status = "INCOMPLETE"
			result.PendingRequirements = []string{"external_account"}
			return result, err
		}
	}

	result.Status = provisionStatus(&acct)
	result.PendingRequirements = pendingRequirements(&acct)
	// Stripe's Custom accounts need no hosted click-through: the platform collected the data and
	// accepted the terms on the merchant's behalf. RequiresAction stays false here and is genuinely
	// used by the PayPal adapter, where the merchant must consent in PayPal's own flow.
	return result, nil
}

func (p *Provisioner) addPerson(ctx context.Context, req spi.ProvisionRequest, acctID string, index int, principal spi.PrincipalData) error {
	f := &form{}
	f.set("first_name", principal.FirstName)
	f.set("last_name", principal.LastName)
	f.set("relationship[title]", principal.Role)
	f.setBool("relationship[owner]", principal.OwnershipPct > 0)
	// Stripe requires exactly one representative per account and rejects a second. The first
	// principal is nominated; the platform's own onboarding orders them with the signatory first,
	// which is why index zero is the right choice rather than an arbitrary one.
	f.setBool("relationship[representative]", index == 0)
	f.setBool("relationship[director]", strings.EqualFold(principal.Role, "director"))
	if principal.OwnershipPct > 0 {
		f.set("relationship[percent_ownership]", strconv.Itoa(principal.OwnershipPct))
	}
	f.set("address[country]", string(principal.Country))
	// The KYC provider's reference rather than a re-collected identity document: gateways
	// increasingly accept verified-elsewhere evidence, which is both faster and less personal data
	// for this platform to hold and later have to erase.
	f.set("metadata[platform_verification_ref]", principal.VerificationRef)
	f.set("metadata[platform_merchant_id]", req.MerchantID.String())

	resp, err := p.do(ctx, callSpec{
		op:             shared.OpProvision,
		method:         http.MethodPost,
		path:           "/v1/accounts/" + url.PathEscape(acctID) + "/persons",
		body:           f.bytes(),
		creds:          req.Credentials,
		idempotencyKey: req.IdempotencyKey + "-person-" + strconv.Itoa(index),
	})
	if err != nil {
		return err
	}
	return p.checkStatus(resp)
}

func (p *Provisioner) attachExternalAccount(ctx context.Context, req spi.ProvisionRequest, acctID string) error {
	b := req.BankAccount
	f := &form{}
	f.set("external_account[object]", "bank_account")
	f.set("external_account[country]", string(b.Country))
	f.set("external_account[currency]", strings.ToLower(string(b.Currency)))
	f.set("external_account[account_holder_name]", b.HolderName)
	f.set("external_account[account_holder_type]", "company")
	if b.IBAN != "" {
		// For SEPA countries Stripe takes the IBAN in `account_number` and no routing number. The
		// two shapes are mutually exclusive and sending both is rejected.
		f.set("external_account[account_number]", b.IBAN)
	} else {
		f.set("external_account[account_number]", b.AccountNumber)
		f.set("external_account[routing_number]", b.RoutingNumber)
	}
	f.setBool("default_for_currency", true)

	resp, err := p.do(ctx, callSpec{
		op:             shared.OpProvision,
		method:         http.MethodPost,
		path:           "/v1/accounts/" + url.PathEscape(acctID) + "/external_accounts",
		body:           f.bytes(),
		creds:          req.Credentials,
		idempotencyKey: req.IdempotencyKey + "-external-account",
	})
	if err != nil {
		return err
	}
	return p.checkStatus(resp)
}

// Deprovision deletes the connected account, tolerating its absence.
//
// Tolerating 404 is not defensive programming, it is the contract: compensation runs after a
// crash, and the crash may have happened before the account was created, or after a previous
// compensation attempt already deleted it. A compensation that fails because the thing it was
// undoing does not exist leaves the saga permanently stuck, which is a worse outcome than the
// failure it was compensating for.
func (p *Provisioner) Deprovision(ctx context.Context, externalAccountID string) error {
	if externalAccountID == "" {
		// Nothing was created, so nothing needs undoing. Reporting success is correct and keeps
		// the saga moving.
		return nil
	}
	resp, err := p.do(ctx, callSpec{
		op:     shared.OpProvision,
		method: http.MethodDelete,
		path:   "/v1/accounts/" + url.PathEscape(externalAccountID),
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

// RegisterWebhook subscribes the platform's ingress endpoint to this account's events.
//
// `connect=true` matters and is easy to miss: without it the endpoint receives only the platform
// account's own events, and every connected account's payment events go nowhere. The failure is
// silent — registration succeeds, payments succeed, and the platform simply never learns any of
// them completed.
func (p *Provisioner) RegisterWebhook(ctx context.Context, req spi.WebhookRegistrationRequest) (*spi.WebhookRegistration, error) {
	if req.URL == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"stripe: a webhook registration requires the platform ingress URL")
	}
	f := &form{}
	f.set("url", req.URL)
	f.setBool("connect", true)
	f.set("api_version", p.cfg.APIVersion)
	events := req.EventTypes
	if len(events) == 0 {
		events = DefaultWebhookEvents()
	}
	for _, e := range events {
		f.appendItem("enabled_events", e)
	}
	f.set("metadata["+metaIdempotencyKey+"]", req.IdempotencyKey)

	resp, err := p.do(ctx, callSpec{
		op:             shared.OpWebhook,
		method:         http.MethodPost,
		path:           "/v1/webhook_endpoints",
		body:           f.bytes(),
		creds:          req.Credentials,
		idempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	if err := p.checkStatus(resp); err != nil {
		return nil, err
	}
	var we webhookEndpoint
	if err := decode(resp.Body, &we); err != nil {
		return nil, err
	}
	if we.ID == "" {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"stripe: the webhook endpoint response carries no id")
	}
	// `secret` is returned exactly once, here. It goes straight into the secrets store; it is
	// never written to a database row, never returned by an API and never logged.
	return &spi.WebhookRegistration{
		RegistrationID: we.ID,
		SigningSecret:  we.Secret,
		URL:            we.URL,
	}, nil
}

// UnregisterWebhook removes the subscription, tolerating its absence for the same reason
// Deprovision does.
func (p *Provisioner) UnregisterWebhook(ctx context.Context, externalAccountID, registrationID string) error {
	if registrationID == "" {
		return nil
	}
	resp, err := p.do(ctx, callSpec{
		op:      shared.OpWebhook,
		method:  http.MethodDelete,
		path:    "/v1/webhook_endpoints/" + url.PathEscape(registrationID),
		creds:   p.bound,
		account: externalAccountID,
	})
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return p.checkStatus(resp)
}

// VerifyCredentials performs the cheapest authenticated call Stripe offers.
//
// `GET /v1/balance` is chosen because it is read-only, is not rate-limited aggressively, exists in
// every account state, and — unlike listing charges — returns a bounded body regardless of account
// size. It has no side effects, which the SPI requires: this runs as a scheduled probe, and a
// probe that creates objects is a probe that pollutes a merchant's dashboard every five minutes.
func (p *Provisioner) VerifyCredentials(ctx context.Context, creds spi.Credentials) error {
	resp, err := p.do(ctx, callSpec{
		op:     shared.OpLookup,
		method: http.MethodGet,
		path:   "/v1/balance",
		creds:  creds,
	})
	if err != nil {
		return err
	}
	return p.checkStatus(resp)
}

// DefaultWebhookEvents is the event set the platform subscribes to.
//
// It is a fixed list rather than a wildcard: a wildcard subscription means every Stripe product
// launch starts delivering events the ingress has to parse and discard, and the ingress is billed
// for and rate-limited on all of them.
func DefaultWebhookEvents() []string {
	return []string{
		"payment_intent.succeeded",
		"payment_intent.payment_failed",
		"payment_intent.canceled",
		"payment_intent.amount_capturable_updated",
		"charge.succeeded",
		"charge.captured",
		"charge.failed",
		"charge.refunded",
		"charge.refund.updated",
		"charge.dispute.created",
		"charge.dispute.closed",
		"payout.paid",
		"payout.failed",
		"account.updated",
	}
}

func (p *Provisioner) do(ctx context.Context, spec callSpec) (*spi.HTTPResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	key, err := credential(p.credsFor(spec.creds), CredentialSecretKey)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{
		"Authorization":       "Bearer " + key,
		"Stripe-Version":      p.cfg.APIVersion,
		"Accept":              "application/json",
		httpx.OperationHeader: spec.op.String(),
	}
	if len(spec.body) > 0 {
		headers["Content-Type"] = "application/x-www-form-urlencoded"
	}
	if spec.idempotencyKey != "" {
		headers["Idempotency-Key"] = spec.idempotencyKey
	}
	if spec.account != "" {
		headers["Stripe-Account"] = spec.account
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
			"stripe: the transport returned neither a response nor an error")
	}
	if resp.Timeout {
		// Provisioning moves no money, but it does create durable state, so an unknown outcome
		// here is resolved by the workflow's own lookup step rather than by reconciliation. The
		// error is a plain timeout, which the workflow retries with the same idempotency key.
		return nil, apierror.New(apierror.CodeGatewayTimeout, "stripe: the provisioning call timed out")
	}
	return resp, nil
}

func (p *Provisioner) checkStatus(resp *spi.HTTPResponse) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var env errorEnvelope
	_ = decode(resp.Body, &env)
	if env.Error == nil {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return apierror.Wrap(spi.ErrCredentialsInvalid, apierror.CodeGatewayAuthenticationFailed,
				"stripe: the API key was rejected")
		}
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"stripe: HTTP %d with an unparseable body", resp.StatusCode)
	}
	return mapErrorType(resp.StatusCode, env.Error)
}

func provisionStatus(a *account) string {
	switch {
	case a.ChargesEnabled && a.PayoutsEnabled:
		return "ACTIVE"
	case a.Requirements != nil && a.Requirements.DisabledReason != "":
		return "RESTRICTED"
	default:
		return "PENDING"
	}
}

func accountRawStatus(a *account) string {
	parts := make([]string, 0, 3)
	parts = append(parts, "charges_enabled="+strconv.FormatBool(a.ChargesEnabled))
	parts = append(parts, "payouts_enabled="+strconv.FormatBool(a.PayoutsEnabled))
	if a.Requirements != nil && a.Requirements.DisabledReason != "" {
		parts = append(parts, "disabled_reason="+a.Requirements.DisabledReason)
	}
	return strings.Join(parts, ",")
}

// pendingRequirements surfaces Stripe's outstanding requirements verbatim.
//
// Verbatim is the point: "provisioning failed" is not actionable, whereas
// "company.verification.document, person_XXX.id_number" tells the merchant exactly what to upload.
// The strings are Stripe's own requirement identifiers, which the console renders into prose.
func pendingRequirements(a *account) []string {
	if a.Requirements == nil {
		return nil
	}
	out := make([]string, 0, len(a.Requirements.CurrentlyDue)+len(a.Requirements.PastDue))
	out = append(out, a.Requirements.PastDue...)
	out = append(out, a.Requirements.CurrentlyDue...)
	if len(out) == 0 {
		return nil
	}
	return out
}
