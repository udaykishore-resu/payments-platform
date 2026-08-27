package runtime

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authn"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authz"
	"github.com/udaykishore-resu/payments-platform/internal/platform/idempotency"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// BearerAuthenticator turns a request into a verified principal.
//
// # Why this adapter exists
//
// authn.Validator validates a *token*: it knows nothing about HTTP, which is what lets the same
// validator serve the REST edge, the gRPC surface and the event consumer's envelope decoder. The
// middleware needs "authenticate this request". Bridging the two is four lines of header
// extraction, and putting those four lines here rather than in each composition root is what
// keeps them identical — an edge that accepted a token from a query parameter "just for testing"
// is an edge that logs credentials in every access log it passes through.
type BearerAuthenticator struct {
	validator *authn.Validator
	keys      *authn.JWKS
}

// NewAuthenticator builds the JWT validator and its JWKS cache from configuration.
//
// # Why the JWKS cache refreshes in the background and never on the request path
//
// A synchronous JWKS fetch on a cache miss is a self-inflicted denial of service: one key
// rotation plus a burst of traffic becomes a thundering herd against the identity provider, and
// every request that arrived during it waits on the network. The cache refreshes on a timer,
// serves stale for up to a day if refreshes start failing, and negative-caches an unknown `kid`
// — so an IdP outage degrades to "keep validating with the keys we have" rather than "reject all
// traffic" (docs/security.md §3.3).
func NewAuthenticator(env AuthEnv, environment shared.Environment, clock shared.Clock) (*BearerAuthenticator, error) {
	if clock == nil {
		clock = shared.SystemClock{}
	}
	keys := authn.NewJWKS(authn.NewHTTPFetcher(), authn.JWKSConfig{Clock: clock})
	// Registration is what tells the cache which URL an issuer's keys live at. Without it every
	// token is rejected with UNKNOWN_KEY — a 401 that names the key rather than the wiring, which
	// is a genuinely hard failure to diagnose from the client's side.
	keys.Register(env.Issuer, env.JWKSURL)

	validator, err := authn.NewValidator(authn.ValidatorConfig{
		Issuers: []authn.Issuer{{
			Name:             env.Issuer,
			JWKSURL:          env.JWKSURL,
			ExpectedAudience: env.Audience,
			// The scopes that must be presented on a sender-constrained token. They are the
			// operations where a stolen bearer token is most expensive: a refund moves money out,
			// and a credential rotation can lock the platform out of a gateway.
			MTLSBoundScopes: []string{"payments:refund", "credentials:rotate"},
		}},
		Environment: environment,
		Keys:        keys,
		Clock:       clock,
		MaxTokenAge: env.MaxTokenAge,
	})
	if err != nil {
		return nil, err
	}
	return &BearerAuthenticator{validator: validator, keys: keys}, nil
}

// Start warms the key set and begins the background refresh, and Stop ends it.
//
// The first fetch is synchronous and its failure is a *startup* failure: a pod that came up with
// no key set authenticates nobody, and it is far better to crash-loop with "the IdP is
// unreachable" than to report ready and 401 every request. After that the refresh is entirely in
// the background — the request path never fetches — which is what stops one key rotation plus a
// traffic burst from becoming a thundering herd against the identity provider.
func (a *BearerAuthenticator) Start(ctx context.Context, issuer string) error {
	if err := a.keys.Refresh(ctx, issuer); err != nil {
		return apierror.Wrapf(err, apierror.CodeDependencyFailure,
			"could not fetch the signing keys for issuer %s", issuer)
	}
	a.keys.Start(context.WithoutCancel(ctx))
	return nil
}

// Stop ends the background refresh.
func (a *BearerAuthenticator) Stop() { a.keys.Stop() }

// KeySetAge reports how long ago the issuer's key set was last fetched, for the readiness probe.
// A pod beyond the stale-if-error bound cannot authenticate anyone and should leave rotation.
func (a *BearerAuthenticator) KeySetAge(issuer string) (time.Duration, bool) {
	return a.keys.SnapshotAge(issuer)
}

// Authenticate extracts and validates the bearer token.
//
// The scheme comparison is case-insensitive because RFC 7235 says the scheme is, and a client
// sending `bearer` rather than `Bearer` is conformant. Nothing else about the header is tolerated:
// a token in a query parameter or a cookie is refused, because both end up in access logs and
// browser history.
func (a *BearerAuthenticator) Authenticate(ctx context.Context, r *http.Request) (*authn.Principal, error) {
	raw := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(raw, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return nil, apierror.New(apierror.CodeUnauthenticated,
			"a bearer token is required in the Authorization header")
	}
	return a.validator.Validate(ctx, strings.TrimSpace(token))
}

// NewAuthorizer builds the RBAC+ABAC policy evaluator.
//
// The approvals store is left nil deliberately. Nil means dual control can never be satisfied,
// which is the correct default for a deployment that has not wired one: a platform that could
// perform a dual-controlled operation without a second approver has the control in name only. A
// deployment that needs dual control wires an approvals store explicitly, and that wiring is the
// reviewable event.
func NewAuthorizer(environment shared.Environment, region string, clock shared.Clock) (*authz.Policy, error) {
	return authz.NewPolicy(authz.PolicyConfig{
		Environment: environment,
		Region:      region,
		Clock:       clock,
	})
}

// NewIdempotency builds the manager the transport's idempotency middleware runs.
//
// # Why the store is Postgres and never Redis
//
// ADR-009: Redis is a read-through accelerator in front of the authoritative record, never the
// record itself. Making the cache authoritative means a Redis eviction under memory pressure
// silently converts a duplicate request into a second payment — and eviction under memory
// pressure is not a rare event. The cache is passed in where one exists, and the behaviour with
// and without it is identical apart from latency.
func NewIdempotency(store ports.IdempotencyStore, cache ports.Cache, clock shared.Clock) (*idempotency.Manager, error) {
	return idempotency.NewManager(store, idempotency.Config{
		// The lease must exceed the endpoint's own timeout budget. A lease shorter than the work
		// it guards means a second caller reclaims a claim whose first holder is still running,
		// and both execute — which on the payment path is a double charge. Ninety seconds is
		// above the API's in-flight budget with room to spare.
		Lease:     90 * time.Second,
		Retention: idempotency.DefaultRetention,
		Clock:     clock,
		Cache:     cache,
	})
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: NFR-29, NFR-32.
//
// The edge's authentication, authorization and idempotency wiring, shared by every binary that
// terminates a public request so the three controls cannot differ between deployables
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
