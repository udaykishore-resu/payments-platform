package main

import (
	"context"

	domaingateway "github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/runtime"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi/handlers"
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
