package main

import (
	"context"
	"log/slog"
	"time"

	apppayment "github.com/udaykishore-resu/payments-platform/internal/application/payment"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/registry"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/redis"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/platform/health"
	"github.com/udaykishore-resu/payments-platform/internal/platform/runtime"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// attrGateways renders the registered adapter set for the startup log line.
//
// Logging *which* adapters a process actually holds is worth the line: "why is every payment for
// this merchant returning NO_ELIGIBLE_GATEWAY" has, more than once, turned out to be an image
// built without an adapter, and the answer is one grep away only if the process said so at start.
func attrGateways(ids []shared.GatewayID) []any {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		names = append(names, id.String())
	}
	return []any{
		slog.Int("count", len(names)),
		slog.Any("gateways", names),
	}
}

// registerGatewayReadiness gates readiness on at least one adapter being registered.
//
// Zero usable gateways means every payment returns 503 NO_ELIGIBLE_GATEWAY, so it is better for
// the pod to leave rotation than to accept traffic it will certainly fail. It is a readiness check
// rather than a startup failure because the registered set can change — a gateway can be
// de-registered by configuration — and a reversible signal is the right shape for a reversible
// condition.
//
// Critical, unlike the Redis check: a pod that can reach nothing is not degraded, it is useless.
func registerGatewayReadiness(reg *health.Registry, gateways *registry.Registry) {
	reg.RegisterReadiness("gateways", true, func(context.Context) error {
		if len(gateways.Registered()) == 0 {
			return apierror.New(apierror.CodeNoEligibleGateway,
				"no gateway adapters are registered; every payment would fail")
		}
		return nil
	})
}

// registerCatalogReadiness gates readiness on the gateway catalogue being readable.
//
// It is a *database* read, not a network call to a gateway, so it fails when persistence is
// unhealthy rather than when a vendor is. That distinction is what keeps it a useful signal:
// a vendor being down is what the gateway health machine is for, and conflating the two would
// make this check flap for reasons a restart could not fix.
//
// Non-critical: the Postgres readiness check above already covers an unreachable writer, and
// marking this critical too would double-count one failure.
func registerCatalogReadiness(reg *health.Registry, catalog *runtime.GatewayCatalog) {
	reg.RegisterReadiness("gateway-catalogue", false, func(ctx context.Context) error {
		_, err := catalog.List(ctx)
		return err
	})
}

// registerBreakerReadiness gates readiness on at least one gateway's breaker being closed.
//
// # Why this is readiness and not an alert
//
// Every breaker open means every payment fails with GATEWAY_CIRCUIT_OPEN. The pod is working
// perfectly and can do nothing useful, which is exactly the condition readiness exists to express:
// shed the traffic, keep the process warm, and recover within one probe interval when a breaker
// half-opens and its probe succeeds.
//
// Critical, because a pod in this state has no successful path for a payment. Non-critical would
// mean keeping it in rotation to fail requests that another pod might have served.
func registerBreakerReadiness(reg *health.Registry, breakers *resilience.BreakerRegistry,
	gateways *registry.Registry) {
	reg.RegisterReadiness("gateway-breakers", true, func(context.Context) error {
		ids := gateways.Registered()
		if len(ids) == 0 {
			// Already reported by registerGatewayReadiness; reporting it twice would make one
			// misconfiguration look like two failures.
			return nil
		}
		for _, id := range ids {
			if breakers.Get(id.String()).State() != resilience.StateOpen {
				return nil
			}
		}
		return apierror.New(apierror.CodeGatewayCircuitOpen,
			"every gateway's circuit is open; no payment can succeed on this pod")
	})
}

// newVelocityCounter returns the rolling-counter store, or nil when Redis is absent.
//
// Nil is a supported configuration and is not a silent downgrade to "no limits": the L5 count
// checks degrade to the risk policy's posture rather than to a pass, and the database still holds
// every guard that actually protects money.
func newVelocityCounter(rdb *redis.Client) ports.VelocityCounter {
	if rdb == nil {
		return nil
	}
	return redis.NewVelocityCounter(rdb.Redis())
}

// paymentSettings applies the operator's gateway timeout to the orchestrator defaults.
//
// Only the timeout is configurable. MaxAttempts is fixed at two because the timeout cascade proves
// a third gateway call cannot fit inside the orchestrator's budget, so an operator raising it
// under pressure would create attempts that are started only to be abandoned.
func paymentSettings(timeout time.Duration) apppayment.Config {
	cfg := apppayment.DefaultConfig()
	if timeout > 0 {
		cfg.GatewayTimeout = timeout
	}
	return cfg
}
