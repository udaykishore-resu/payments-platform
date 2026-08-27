package paypal

import (
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

// Factory constructs the three faces of the PayPal integration from one configuration, so that
// adding PayPal to a deployment is a registry entry rather than an edit to the orchestrator.
type Factory struct{}

var _ spi.Factory = Factory{}

// NewFactory returns the PayPal factory.
func NewFactory() Factory { return Factory{} }

// ID returns the registry slug.
func (Factory) ID() shared.GatewayID { return GatewayID }

// NewGateway builds the payment-path adapter, including its own OAuth token cache. One Gateway per
// deployment, not per request: the token cache is the whole point, and a per-request adapter would
// exchange a token per payment.
func (Factory) NewGateway(cfg spi.Config) (spi.PaymentGateway, error) { return NewGateway(cfg) }

// NewProvisioner builds the onboarding adapter.
func (Factory) NewProvisioner(cfg spi.Config) (spi.GatewayProvisioner, error) {
	return NewProvisioner(cfg)
}

// NewVerifier builds the webhook verifier. Bind OAuth credentials with
// (*Verifier).WithCredentials: PayPal's verification endpoint is itself authenticated.
func (Factory) NewVerifier(cfg spi.Config) (spi.WebhookVerifier, error) { return NewVerifier(cfg) }
