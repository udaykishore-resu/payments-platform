package main

import (
	"context"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/registry"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/simulator"
	"github.com/udaykishore-resu/payments-platform/internal/platform/health"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// newGatewayRegistry builds the adapter set whose verifiers authenticate inbound deliveries.
//
// This binary uses the registry for one thing — ResolveVerifier — and holds it through the
// narrow runtime.WebhookVerifiers adapter rather than handing it to the ingester directly. That
// narrowing is the security point rather than a style preference: the webhook ingress is the most
// exposed surface in the platform, and the blast radius of compromising it must not include the
// ability to initiate a payment.
//
// The simulator engine is constructed only when enabled, so a production process holds no
// simulator state at all — not a disabled one.
func newGatewayRegistry(withSimulator bool) (*registry.Registry, error) {
	var engine *simulator.Engine
	if withSimulator {
		engine = simulator.NewEngine(simulator.EngineOptions{})
	}
	return registry.NewWithBuiltIn(engine)
}

// registerGatewayReadiness gates readiness on at least one adapter being registered.
//
// Critical, and for a sharper reason than on the API: with no adapters, every delivery fails
// signature verification, the gateway retries it, and several gateways disable an endpoint that
// keeps rejecting. A pod in that state is not degraded — it is actively teaching the gateway to
// stop talking to us — so it must leave rotation.
func registerGatewayReadiness(reg *health.Registry, gateways *registry.Registry) {
	reg.RegisterReadiness("gateways", true, func(context.Context) error {
		if len(gateways.Registered()) == 0 {
			return apierror.New(apierror.CodeWebhookUnknownGateway,
				"no gateway adapters are registered; every webhook would fail verification")
		}
		return nil
	})
}
