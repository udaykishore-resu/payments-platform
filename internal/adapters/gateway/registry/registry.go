// Package registry maps a gateway slug to the adapter that implements it.
//
// The whole reason this package exists is a negative one: so that no file above
// internal/adapters ever contains `switch gatewayID { case "stripe": ... }`. That switch is the
// thing every payments platform grows, in more than one place, and each copy is a place a new
// gateway gets forgotten — usually the refund path, discovered months later by a merchant. Adding
// a gateway here is a registration, and registry_test.go asserts mechanically that no
// gateway-name switch has reappeared elsewhere.
//
// The registry also owns the translation from the *domain's* view of a gateway — the
// gateway.Gateway descriptor, with its per-environment base URLs and pinned API version — into the
// adapter's spi.Config. That translation lives here rather than in each adapter because it is
// where the environment is selected, and selecting the wrong environment is the failure mode where
// a certification run charges a real card.
package registry

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	domaingateway "github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// DefaultTimeout is the per-request ceiling applied when a record does not state one. It is
// deliberately short: a gateway call that has not answered in fifteen seconds has, from the payer's
// point of view, already failed, and holding the connection open past that only consumes a
// bulkhead slot that a fresh payment could use.
const DefaultTimeout = 15 * time.Second

// DefaultWebhookTolerance is the replay window applied when a record does not state one.
const DefaultWebhookTolerance = 5 * time.Minute

// Record is everything the registry needs to build an adapter for one gateway.
//
// It is a flat struct rather than the domain's gateway.Gateway because the registry is an adapter
// concern and must not force every caller to construct a domain aggregate. FromDescriptor converts
// one into the other for the normal path, where the record comes from the gateway registry table.
type Record struct {
	GatewayID shared.GatewayID
	// BaseURL is the vendor endpoint for this environment. It is per environment on the descriptor
	// and is resolved before it reaches here, because a registry that carried both and picked one
	// later is a registry that can pick the wrong one.
	BaseURL string
	// ProvisioningBaseURL is the endpoint for the onboarding APIs where the vendor serves them from
	// a different host — Adyen does, Stripe and PayPal do not. Empty means "same as BaseURL".
	ProvisioningBaseURL string
	APIVersion          string
	Environment         shared.Environment
	Timeout             time.Duration
	WebhookTolerance    time.Duration
	// HTTPClient is the transport this gateway's adapter must use. It is per gateway so that
	// connection pools, timeouts and the circuit breaker are scoped per vendor; sharing one across
	// vendors lets a wedged gateway starve a healthy one.
	HTTPClient spi.HTTPDoer
	Clock      shared.Clock
}

func (r Record) validate() error {
	if r.GatewayID.IsZero() {
		return apierror.New(apierror.CodeConfigurationInvalid,
			"registry: a record requires a gateway id")
	}
	if r.BaseURL == "" {
		return apierror.Newf(apierror.CodeGatewayNotConfigured,
			"registry: gateway %s has no base URL for this environment", r.GatewayID)
	}
	if !r.Environment.IsValid() {
		return apierror.Newf(apierror.CodeConfigurationInvalid,
			"registry: gateway %s has no valid environment", r.GatewayID)
	}
	if r.HTTPClient == nil {
		return apierror.Newf(apierror.CodeConfigurationInvalid,
			"registry: gateway %s has no HTTP client; adapters must not build their own", r.GatewayID)
	}
	return nil
}

// Config renders the adapter configuration for the payment path.
func (r Record) Config() spi.Config {
	clock := r.Clock
	if clock == nil {
		clock = shared.SystemClock{}
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	tolerance := r.WebhookTolerance
	if tolerance <= 0 {
		tolerance = DefaultWebhookTolerance
	}
	return spi.Config{
		BaseURL:          r.BaseURL,
		APIVersion:       r.APIVersion,
		Timeout:          timeout,
		HTTPClient:       r.HTTPClient,
		Clock:            clock,
		Environment:      r.Environment,
		WebhookTolerance: tolerance,
	}
}

// ProvisioningConfig renders the adapter configuration for the onboarding path.
//
// It differs from Config in exactly one field, and that field matters: Adyen serves Legal Entity
// Management and the Balance Platform from different hostnames than Checkout. Deriving one from the
// other by string surgery would be a guess that works in sandbox and fails in a live account, so
// the second URL is carried explicitly and defaults to the first only when a vendor genuinely uses
// one host.
func (r Record) ProvisioningConfig() spi.Config {
	cfg := r.Config()
	if r.ProvisioningBaseURL != "" {
		cfg.BaseURL = r.ProvisioningBaseURL
	}
	return cfg
}

// FromDescriptor builds a Record from the domain's gateway aggregate.
//
// The environment is an argument rather than a field of the descriptor because the descriptor
// carries a URL per environment and the caller is the only one who knows which one this process is
// running in. Resolving it here, once, means a missing production URL is a startup failure rather
// than a first-payment failure.
func FromDescriptor(g *domaingateway.Gateway, env shared.Environment, client spi.HTTPDoer, clock shared.Clock) (Record, error) {
	if g == nil {
		return Record{}, apierror.New(apierror.CodeConfigurationInvalid,
			"registry: a nil gateway descriptor cannot be registered")
	}
	base, err := g.BaseURL(env)
	if err != nil {
		return Record{}, err
	}
	caps := g.Capabilities()
	rec := Record{
		GatewayID:        g.ID(),
		BaseURL:          base,
		APIVersion:       g.APIVersion(),
		Environment:      env,
		HTTPClient:       client,
		Clock:            clock,
		Timeout:          DefaultTimeout,
		WebhookTolerance: DefaultWebhookTolerance,
	}
	// The authorization validity is not adapter configuration, but reading it here documents that
	// the descriptor is the source of truth for it and keeps the conversion honest about what it
	// does and does not carry across.
	_ = caps.AuthorizationValidity
	return rec, nil
}

// Registry holds the factories and the records, and builds adapters on demand.
//
// Adapters are built per resolve rather than cached, with one deliberate exception noted on
// Resolve: construction is cheap for every adapter here, and a cached adapter is a place for
// per-merchant state to accumulate in a process-lifetime object.
type Registry struct {
	mu        sync.RWMutex
	factories map[shared.GatewayID]spi.Factory
	records   map[shared.GatewayID]Record
	// cache holds constructed payment gateways. It exists for one reason: the PayPal adapter owns an
	// OAuth token cache, and rebuilding the adapter per request would re-exchange a token per
	// payment against an endpoint PayPal rate-limits hard. Everything cached here must therefore be
	// safe for concurrent use and must hold no per-merchant state — which is why credentials travel
	// on the request rather than on the adapter.
	cache map[shared.GatewayID]spi.PaymentGateway
}

// New builds an empty registry.
func New() *Registry {
	return &Registry{
		factories: make(map[shared.GatewayID]spi.Factory),
		records:   make(map[shared.GatewayID]Record),
		cache:     make(map[shared.GatewayID]spi.PaymentGateway),
	}
}

// Register adds a factory.
//
// A duplicate registration is an error rather than an overwrite. Two factories for one slug means
// two pieces of wiring disagree about which adapter serves a gateway, and silently letting the last
// one win makes the answer depend on package initialisation order — which is exactly the kind of
// bug that behaves differently in a test binary than in production.
func (r *Registry) Register(f spi.Factory) error {
	if f == nil {
		return apierror.New(apierror.CodeConfigurationInvalid, "registry: a nil factory cannot be registered")
	}
	id := f.ID()
	if id.IsZero() {
		return apierror.New(apierror.CodeConfigurationInvalid, "registry: a factory must declare a gateway id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[id]; exists {
		return apierror.Newf(apierror.CodeConfigurationInvalid,
			"registry: a factory for gateway %s is already registered", id)
	}
	r.factories[id] = f
	return nil
}

// MustRegister is Register for process wiring, where a duplicate registration is a programming
// error that should stop the binary at startup rather than surface on a payment.
func (r *Registry) MustRegister(f spi.Factory) {
	if err := r.Register(f); err != nil {
		panic(err)
	}
}

// Configure binds a record to an already-registered factory.
//
// Registration and configuration are separate because they arrive from different places and at
// different times: the factory is compiled in, and the record comes from the gateway registry table
// and can be updated while the process runs. Keeping them apart means a configuration reload does
// not have to re-register anything.
func (r *Registry) Configure(rec Record) error {
	if err := rec.validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.factories[rec.GatewayID]; !ok {
		return apierror.Newf(apierror.CodeGatewayNotConfigured,
			"registry: no adapter is registered for gateway %s", rec.GatewayID)
	}
	r.records[rec.GatewayID] = rec
	// A reconfiguration invalidates anything built from the previous record — otherwise a base-URL
	// change would be accepted and then ignored, which is worse than rejecting it.
	delete(r.cache, rec.GatewayID)
	return nil
}

// Resolve returns the payment-path adapter for a gateway.
//
// It is the only way the orchestrator obtains a gateway, and it takes a slug rather than anything
// gateway-specific. That is what keeps the dispatch path free of vendor knowledge: the router picks
// a slug from configuration, the registry turns it into behaviour, and nothing in between names a
// vendor.
func (r *Registry) Resolve(ctx context.Context, gatewayID shared.GatewayID) (spi.PaymentGateway, error) {
	if err := ctx.Err(); err != nil {
		return nil, apierror.Wrap(err, apierror.CodeServiceUnavailable,
			"registry: the context was cancelled before the gateway could be resolved")
	}
	r.mu.RLock()
	if g, ok := r.cache[gatewayID]; ok {
		r.mu.RUnlock()
		return g, nil
	}
	factory, hasFactory := r.factories[gatewayID]
	record, hasRecord := r.records[gatewayID]
	r.mu.RUnlock()

	if !hasFactory {
		return nil, unknownGateway(gatewayID)
	}
	if !hasRecord {
		return nil, apierror.Newf(apierror.CodeGatewayNotConfigured,
			"registry: gateway %s is registered but has no configuration for this environment", gatewayID).
			WithDetail(apierror.Detail{
				Field: "gateway", Code: "GATEWAY_NOT_CONFIGURED",
				Message: "the adapter exists but no base URL or transport has been bound to it",
				RuleID:  "L4.GATEWAY_BASE_URL_REQUIRED",
			})
	}

	gw, err := factory.NewGateway(record.Config())
	if err != nil {
		return nil, err
	}
	if gw == nil {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"registry: the factory for gateway %s returned neither an adapter nor an error", gatewayID)
	}

	r.mu.Lock()
	// Re-check under the write lock: two goroutines can miss the cache concurrently, and returning
	// two adapters where one is cached would give the losers an object the registry will never
	// invalidate on reconfiguration.
	if existing, ok := r.cache[gatewayID]; ok {
		r.mu.Unlock()
		return existing, nil
	}
	r.cache[gatewayID] = gw
	r.mu.Unlock()
	return gw, nil
}

// ResolveProvisioner returns the onboarding adapter for a gateway.
//
// Provisioners are not cached. They are built per onboarding case, often bound to resolved
// credentials for the duration of a compensation, and caching one would extend a credential's
// lifetime from an activity to the life of the process.
func (r *Registry) ResolveProvisioner(ctx context.Context, gatewayID shared.GatewayID) (spi.GatewayProvisioner, error) {
	factory, record, err := r.lookup(ctx, gatewayID)
	if err != nil {
		return nil, err
	}
	p, err := factory.NewProvisioner(record.ProvisioningConfig())
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"registry: the factory for gateway %s returned neither a provisioner nor an error", gatewayID)
	}
	return p, nil
}

// ResolveVerifier returns the webhook verifier for a gateway.
//
// The webhook ingress resolves by the slug in its own URL path, *before* it has parsed a body it
// does not yet trust. That is the whole reason the verifier is a separate interface reachable by
// slug: the decision about which signature scheme to apply must not depend on the content being
// verified.
func (r *Registry) ResolveVerifier(ctx context.Context, gatewayID shared.GatewayID) (spi.WebhookVerifier, error) {
	factory, record, err := r.lookup(ctx, gatewayID)
	if err != nil {
		return nil, err
	}
	v, err := factory.NewVerifier(record.Config())
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"registry: the factory for gateway %s returned neither a verifier nor an error", gatewayID)
	}
	return v, nil
}

func (r *Registry) lookup(ctx context.Context, gatewayID shared.GatewayID) (spi.Factory, Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, Record{}, apierror.Wrap(err, apierror.CodeServiceUnavailable,
			"registry: the context was cancelled before the gateway could be resolved")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	factory, ok := r.factories[gatewayID]
	if !ok {
		return nil, Record{}, unknownGateway(gatewayID)
	}
	record, ok := r.records[gatewayID]
	if !ok {
		return nil, Record{}, apierror.Newf(apierror.CodeGatewayNotConfigured,
			"registry: gateway %s is registered but has no configuration for this environment", gatewayID)
	}
	return factory, record, nil
}

// Registered returns the slugs that have a factory, sorted. Used by the control plane to validate
// that a routing policy names only gateways this binary can actually dispatch to — a check that
// turns a typo in configuration into a rejected config version rather than a failed payment.
func (r *Registry) Registered() []shared.GatewayID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]shared.GatewayID, 0, len(r.factories))
	for id := range r.factories {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Configured returns the slugs that have both a factory and a record, sorted.
func (r *Registry) Configured() []shared.GatewayID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]shared.GatewayID, 0, len(r.records))
	for id := range r.records {
		if _, ok := r.factories[id]; ok {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Has reports whether a factory is registered for the slug.
func (r *Registry) Has(gatewayID shared.GatewayID) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.factories[gatewayID]
	return ok
}

func unknownGateway(id shared.GatewayID) error {
	return apierror.Newf(apierror.CodeGatewayNotConfigured,
		"registry: no adapter is registered for gateway %q", id).
		WithDetail(apierror.Detail{
			Field: "gateway", Code: "GATEWAY_NOT_REGISTERED",
			Message: "add the gateway's factory to the registry wiring; there is no switch statement to edit",
			RuleID:  "L4.GATEWAY_REGISTERED",
		})
}
