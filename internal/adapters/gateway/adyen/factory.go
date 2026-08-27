package adyen

import (
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

// Factory constructs the three faces of the Adyen integration from one configuration, so that
// adding Adyen to a deployment is a registry entry rather than an edit to the orchestrator.
type Factory struct{}

var _ spi.Factory = Factory{}

// NewFactory returns the Adyen factory.
func NewFactory() Factory { return Factory{} }

// ID returns the registry slug.
func (Factory) ID() shared.GatewayID { return GatewayID }

// NewGateway builds the payment-path adapter, whose BaseURL is Adyen's Checkout prefix.
func (Factory) NewGateway(cfg spi.Config) (spi.PaymentGateway, error) { return NewGateway(cfg) }

// NewProvisioner builds the onboarding adapter, whose BaseURL is Adyen's LEM / Balance Platform /
// Management prefix — a different hostname from Checkout in a live account.
func (Factory) NewProvisioner(cfg spi.Config) (spi.GatewayProvisioner, error) {
	return NewProvisioner(cfg)
}

// NewVerifier builds the notification verifier.
func (Factory) NewVerifier(cfg spi.Config) (spi.WebhookVerifier, error) { return NewVerifier(cfg) }
