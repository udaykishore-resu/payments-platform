package simulator

import (
	"context"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Provision creates a simulated sub-merchant.
//
// Idempotency is real rather than declared: the merchant id is the key, so a second call for the
// same merchant returns the *same* account id. That is the behaviour the onboarding saga depends
// on, and a simulator that minted a fresh id each time would let a workflow bug — retrying a
// provisioning step after a crash — pass every test and then create two connected accounts in
// production, which is a thing no gateway lets you undo.
func (e *Engine) Provision(ctx context.Context, req spi.ProvisionRequest) (*spi.ProvisionResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	if req.IdempotencyKey == "" {
		return nil, apierror.New(apierror.CodeIdempotencyKeyRequired,
			"simulator: provisioning requires an idempotency key")
	}
	if req.MerchantID.IsZero() {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"simulator: provisioning requires a merchant id")
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if existing, ok := e.byMerchant[req.MerchantID.String()]; ok {
		return &spi.ProvisionResult{
			ExternalAccountID: existing,
			Status:            "ACTIVE",
			RawStatus:         "existing",
		}, nil
	}
	id := e.nextRef("sim_acct")
	e.accounts[id] = req.MerchantID.String()
	e.byMerchant[req.MerchantID.String()] = id
	return &spi.ProvisionResult{ExternalAccountID: id, Status: "ACTIVE", RawStatus: "created"}, nil
}

// Deprovision removes the simulated account and tolerates its absence.
//
// Tolerating absence is the contract, not leniency: compensation runs after a crash, and the crash
// may well have happened before the account was created. A compensation that failed on a missing
// target would leave the saga permanently stuck — a worse outcome than the failure it is undoing.
func (e *Engine) Deprovision(ctx context.Context, externalAccountID string) error {
	if err := ctx.Err(); err != nil {
		return contextError(err)
	}
	if externalAccountID == "" {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if merchant, ok := e.accounts[externalAccountID]; ok {
		delete(e.accounts, externalAccountID)
		delete(e.byMerchant, merchant)
	}
	return nil
}

// RegisterWebhook subscribes an ingress endpoint and returns the signing secret the simulator will
// actually sign with — so an end-to-end test verifies against material the simulator is using
// rather than against a constant the test also hard-coded.
func (e *Engine) RegisterWebhook(ctx context.Context, req spi.WebhookRegistrationRequest) (*spi.WebhookRegistration, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	if req.URL == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"simulator: a webhook registration requires the ingress URL")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	id := e.nextRef("sim_whk")
	reg := WireWebhookResponse{RegistrationID: id, SigningSecret: e.opts.WebhookSecret, URL: req.URL}
	e.hooks[id] = reg
	return &spi.WebhookRegistration{
		RegistrationID: reg.RegistrationID,
		SigningSecret:  reg.SigningSecret,
		URL:            reg.URL,
	}, nil
}

// UnregisterWebhook removes the subscription, tolerating its absence.
func (e *Engine) UnregisterWebhook(ctx context.Context, externalAccountID, registrationID string) error {
	if err := ctx.Err(); err != nil {
		return contextError(err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.hooks, registrationID)
	return nil
}

// VerifyCredentials checks the API key is present. It has no side effects, which the SPI requires:
// this runs as a scheduled probe, and a probe with side effects pollutes the thing it is probing.
func (e *Engine) VerifyCredentials(ctx context.Context, creds spi.Credentials) error {
	if err := ctx.Err(); err != nil {
		return contextError(err)
	}
	v, ok := creds.Get(CredentialAPIKey)
	if !ok || v == "" {
		return apierror.Wrap(spi.ErrCredentialsInvalid, apierror.CodeGatewayAuthenticationFailed,
			"simulator: the api_key credential is missing")
	}
	return nil
}
