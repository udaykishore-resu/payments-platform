package payment

import (
	"context"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// ResolverDeps is what turning a gateway slug into a usable client requires.
type ResolverDeps struct {
	// Merchants supplies the connection: the external account reference and the secret
	// reference. It is the context loader rather than a repository so that the connection the
	// dispatcher uses is the same one the router scored — a connection revoked between the two
	// reads would otherwise dispatch against a credential the router believed was live.
	Merchants MerchantContextLoader
	// Secrets resolves a `secret://` reference to material, at the moment of use.
	Secrets ports.SecretsProvider
	// Adapters resolves the slug to the client that speaks the vendor's API.
	Adapters AdapterSource
	// Environment selects the vendor's sandbox or live endpoints. It is a required field with no
	// default because getting it wrong is the failure mode where a certification run charges a
	// real card.
	Environment shared.Environment
}

// Resolver is the production GatewayResolver.
//
// It exists to hold one rule that is easy to state and easy to violate: **credentials are
// resolved per call and are never retained.** There is no field on this struct that could hold
// material, no cache keyed by merchant, and no memo on the returned adapter. The reasons are
// cumulative:
//
//   - A rotation must take effect on the next request, not on the next process restart. Caching
//     material makes the ≤90-day rotation control (baseline §17.2) a statement about deployment
//     cadence rather than about credentials.
//   - A cached credential is a credential in a heap dump, in a goroutine dump, and in whatever
//     the process writes when it panics.
//   - A per-merchant cache on a process-lifetime object is a per-merchant cache that outlives the
//     merchant's revocation.
//
// The adapter itself *is* cached, by the registry, and that is safe for exactly the same reason:
// it holds no per-merchant state, because the credentials travel on the request.
type Resolver struct {
	deps ResolverDeps
}

// NewResolver constructs the resolver.
func NewResolver(d ResolverDeps) *Resolver { return &Resolver{deps: d} }

// Resolve returns an adapter bound to the merchant's connection, with credentials resolved for
// exactly this call.
//
// The chain is: connection lookup → secret reference → secrets provider → spi.Credentials →
// adapter. Each link fails loudly rather than degrading, because every degraded outcome here is
// worse than a refusal: dispatching without an external account ID sends a payment to the
// platform's own account rather than the merchant's, and dispatching with an empty credential
// set produces an authentication failure the gateway will rate-limit us for.
func (r *Resolver) Resolve(ctx context.Context, m shared.MerchantID, g shared.GatewayID) (spi.PaymentGateway, spi.Credentials, string, error) {
	snap, err := r.deps.Merchants.Load(ctx, m)
	if err != nil {
		return nil, spi.Credentials{}, "", err
	}
	conn, ok := snap.Connections[g]
	if !ok {
		return nil, spi.Credentials{}, "", apierror.Newf(apierror.CodeGatewayNotConfigured,
			"merchant %s has no connection to gateway %s", m, g).
			WithDetail(apierror.Detail{
				Field: "gateway", Code: "NO_CONNECTION",
				Message: "the merchant is not provisioned on this gateway",
				RuleID:  "L5.GATEWAY_CONNECTION_EXISTS",
			})
	}
	if !conn.UsableForPayments() {
		return nil, spi.Credentials{}, "", apierror.Newf(apierror.CodeGatewayNotConfigured,
			"the connection to gateway %s is %s and may not carry payments", g, conn.Status).
			WithDetail(apierror.Detail{
				Field: "gateway", Code: "CONNECTION_NOT_USABLE",
				Message: "only CERTIFIED and DEGRADED connections may be dispatched over",
				RuleID:  "L5.GATEWAY_CONNECTION_USABLE",
			})
	}
	if err := gateway.ValidateSecretRef(conn.SecretRef, "credentialRef"); err != nil {
		return nil, spi.Credentials{}, "", err
	}

	material, err := r.deps.Secrets.Get(ctx, conn.SecretRef)
	if err != nil {
		// Wrapped as a dependency failure rather than as an authentication failure: the
		// credential was not rejected, it could not be read, and paging the on-call engineer for
		// a secrets-store outage is a different response from paging them for a revoked key.
		return nil, spi.Credentials{}, "", apierror.Wrapf(err, apierror.CodeDependencyFailure,
			"could not resolve credentials for gateway %s", g)
	}

	creds := spi.Credentials{
		Values:      make(map[string]string, len(material.Fields())),
		Version:     material.Version(),
		Environment: r.deps.Environment,
	}
	for _, f := range material.Fields() {
		if v, ok := material.Value(f); ok {
			creds.Values[f] = v
		}
	}
	if len(creds.Values) == 0 {
		return nil, spi.Credentials{}, "", apierror.Newf(apierror.CodeGatewayAuthenticationFailed,
			"the credential reference for gateway %s resolved to no fields", g)
	}

	client, err := r.deps.Adapters.Resolve(ctx, g)
	if err != nil {
		return nil, spi.Credentials{}, "", err
	}
	return client, creds, conn.ExternalAccountID, nil
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-11, FR-40, NFR-32.
//
// Credential resolution per call, from the secret store, never cached in the process
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
