package main

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/registry"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/simulator"
	apppayment "github.com/udaykishore-resu/payments-platform/internal/application/payment"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	domaingateway "github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/redis"
	"github.com/udaykishore-resu/payments-platform/internal/platform/health"
	"github.com/udaykishore-resu/payments-platform/internal/platform/runtime"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi/handlers"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// gatewayCatalog adapts the platform's gateway catalogue to the transport's GatewayService.
//
// # Why this adapter exists rather than the transport interface living in the platform
//
// handlers.GatewayService is declared by its consumer — the REST handlers — per the repository's
// consumer-declared-interface convention. internal/platform must not import internal/transport,
// because that would invert the dependency the layering exists to keep. Something has to bridge
// the two, and a four-method adapter in the composition root's own package is the smallest thing
// that can: it is the one place that legitimately knows both sides.
//
// It is in its own file rather than in main.go because main.go is wiring, and a type with methods
// is not wiring. scripts/check-architecture.sh bounds main.go's length for exactly that reason.
type gatewayCatalog struct {
	inner *runtime.GatewayCatalog
}

// Get reads one gateway from the catalogue.
func (g gatewayCatalog) Get(ctx context.Context, id shared.GatewayID) (*domaingateway.Gateway, error) {
	return g.inner.Get(ctx, id)
}

// List reads the platform-global catalogue.
func (g gatewayCatalog) List(ctx context.Context) ([]*domaingateway.Gateway, error) {
	return g.inner.List(ctx)
}

// Health reads the per-operation health measurements for one gateway.
func (g gatewayCatalog) Health(ctx context.Context, id shared.GatewayID,
	ops []shared.Operation) ([]*domaingateway.Health, error) {
	return g.inner.Health(ctx, id, ops)
}

// RotateCredentials forwards the rotation, which this deployment refuses because it has no
// secrets provider. See runtime.GatewayCatalog for why refusing beats a silent no-op.
func (g gatewayCatalog) RotateCredentials(ctx context.Context,
	cmd handlers.RotateCommand) (*handlers.RotationAccepted, error) {
	return nil, g.inner.Rotate(ctx, runtime.RotationRequest{
		TenantID:    cmd.TenantID,
		GatewayID:   cmd.GatewayID,
		MerchantID:  cmd.MerchantID,
		Environment: cmd.Environment,
		Reason:      cmd.Reason,
		Note:        cmd.Note,
		ActorID:     cmd.ActorID,
	})
}

// newGatewayRegistry builds the adapter registry this API dispatches through.
//
// The simulator engine is constructed only when enabled, so a production process holds no
// simulator state at all — not a disabled one. A disabled feature that is nevertheless
// constructed is a feature one configuration mistake away from being enabled, and the mistake
// here produces payments that report authorized and move no money.
func newGatewayRegistry(withSimulator bool) (*registry.Registry, error) {
	var engine *simulator.Engine
	if withSimulator {
		engine = simulator.NewEngine(simulator.EngineOptions{})
	}
	return registry.NewWithBuiltIn(engine)
}

// registerGatewayReadiness gates readiness on at least one adapter being registered.
//
// Zero usable gateways means every payment returns 503 NO_ELIGIBLE_GATEWAY, so it is better for
// the pod to leave rotation than to accept traffic it will certainly fail. Critical, unlike the
// Redis check: a pod that can reach no gateway is not degraded, it is useless.
func registerGatewayReadiness(reg *health.Registry, gateways *registry.Registry) {
	reg.RegisterReadiness("gateways", true, func(context.Context) error {
		if len(gateways.Registered()) == 0 {
			return apierror.New(apierror.CodeNoEligibleGateway,
				"no gateway adapters are registered; every payment would fail")
		}
		return nil
	})
}

// newVelocityCounter returns the rolling-counter store, or nil when Redis is absent.
//
// Nil is a supported configuration and is not a silent downgrade to "no limits": the L5 count
// checks degrade to the risk policy's posture rather than to a pass, and the database still holds
// every guard that actually protects money. Returning a typed nil would defeat that — the
// application layer checks the interface for nil — so the untyped nil is deliberate.
func newVelocityCounter(rdb *redis.Client) ports.VelocityCounter {
	if rdb == nil {
		return nil
	}
	return redis.NewVelocityCounter(rdb.Redis())
}

// paymentSettings applies the operator's gateway timeout to the orchestrator defaults.
//
// Only the timeout is configurable, and the rest are not knobs by design: MaxAttempts is fixed at
// two because the timeout cascade proves a third gateway call cannot fit inside the orchestrator's
// budget, and an operator raising it under pressure would create attempts that are started only to
// be abandoned.
func paymentSettings(timeout time.Duration) apppayment.Config {
	cfg := apppayment.DefaultConfig()
	if timeout > 0 {
		cfg.GatewayTimeout = timeout
	}
	return cfg
}

// assertMoneyPathMounted is the startup smoke assertion.
//
// handlers.Register mounts a resource's routes only when its service is wired, which is what lets
// nine binaries share one handler set — and which means "the money path is served" is a fact about
// this process's wiring, not about this source file. A build that silently 404s POST /v1/payments
// because a dependency came back nil is exactly the failure an operator discovers from a merchant
// rather than from a log, so it is checked here, before the listener binds.
func assertMoneyPathMounted(rt *httpapi.Router) error {
	required := []struct{ method, template string }{
		{http.MethodPost, httpapi.RouteCreatePayment},
		{http.MethodGet, httpapi.RouteListPayments},
		{http.MethodGet, httpapi.RouteGetPayment},
		{http.MethodPost, httpapi.RouteCapturePayment},
		{http.MethodPost, httpapi.RouteRefundPayment},
		{http.MethodPost, httpapi.RouteVoidPayment},
	}
	var missing []string
	for _, r := range required {
		if !rt.Has(r.method, r.template) {
			missing = append(missing, r.method+" "+r.template)
		}
	}
	if len(missing) > 0 {
		return apierror.Newf(apierror.CodeInternalError,
			"the money path is not mounted: %s", strings.Join(missing, ", "))
	}
	return nil
}

// newIdempotencyCache returns the Redis read-through accelerator, or nil when Redis is absent.
//
// Nil is fully supported and the behaviour is identical apart from latency: Postgres is the
// authoritative idempotency record and Redis never is (ADR-009). Making the cache authoritative
// would mean an eviction under memory pressure silently converts a duplicate request into a
// second payment, and eviction under memory pressure is not a rare event.
func newIdempotencyCache(rdb *redis.Client) ports.Cache {
	if rdb == nil {
		return nil
	}
	return redis.NewCache(rdb.Redis())
}
