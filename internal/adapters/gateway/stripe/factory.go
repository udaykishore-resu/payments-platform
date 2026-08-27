package stripe

import (
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

// Factory constructs the three faces of the Stripe integration from one configuration.
//
// It exists so that adding a gateway to the platform is a registration rather than an edit to a
// switch statement in the orchestrator. The registry holds factories keyed by slug; nothing above
// the adapter layer names Stripe in code.
type Factory struct{}

var _ spi.Factory = Factory{}

// NewFactory returns the Stripe factory. It is a value type with no state: everything that varies
// between deployments arrives in spi.Config, and everything that varies between merchants arrives
// on the request.
func NewFactory() Factory { return Factory{} }

// ID returns the registry slug.
func (Factory) ID() shared.GatewayID { return GatewayID }

// NewGateway builds the payment-path adapter.
func (Factory) NewGateway(cfg spi.Config) (spi.PaymentGateway, error) { return NewGateway(cfg) }

// NewProvisioner builds the onboarding adapter. Bind credentials with
// (*Provisioner).WithCredentials before invoking a compensation.
func (Factory) NewProvisioner(cfg spi.Config) (spi.GatewayProvisioner, error) {
	return NewProvisioner(cfg)
}

// NewVerifier builds the webhook verifier.
func (Factory) NewVerifier(cfg spi.Config) (spi.WebhookVerifier, error) { return NewVerifier(cfg) }
