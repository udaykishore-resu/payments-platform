package simulator

import (
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

// Factory constructs the simulator's three faces from one configuration.
//
// It builds the *HTTP* adapter rather than the in-process Engine, because a factory is what the
// registry uses and the registry's job is to hand out something that behaves like a gateway —
// including having a transport, a timeout and a connection pool. A test that wants the in-process
// Engine reaches for NewEngine directly, which is honest about the fact that it is skipping the
// transport.
type Factory struct {
	// Engine is the deterministic core the verifier is built from. It is exported so a test can
	// drive scenarios and read emitted webhooks through the same object the factory hands out.
	Engine *Engine
}

var _ spi.Factory = Factory{}

// NewFactory returns a simulator factory backed by engine. A nil engine gets a default one, so the
// zero-configuration case works for local development.
func NewFactory(engine *Engine) Factory {
	if engine == nil {
		engine = NewEngine(EngineOptions{})
	}
	return Factory{Engine: engine}
}

// ID returns the registry slug.
func (Factory) ID() shared.GatewayID { return GatewayID }

// NewGateway builds the payment-path client.
func (Factory) NewGateway(cfg spi.Config) (spi.PaymentGateway, error) { return NewAdapter(cfg) }

// NewProvisioner builds the onboarding client.
func (Factory) NewProvisioner(cfg spi.Config) (spi.GatewayProvisioner, error) {
	return NewProvisionerAdapter(cfg)
}

// NewVerifier returns the Engine, which is the verifier: signature verification needs the signing
// scheme and the secret, both of which live on the engine that emitted the event. There is no
// network call in verification, so there is nothing for an HTTP client to add.
func (f Factory) NewVerifier(cfg spi.Config) (spi.WebhookVerifier, error) {
	engine := f.Engine
	if engine == nil {
		engine = NewEngine(EngineOptions{Clock: cfg.Clock})
	}
	return engine, nil
}
