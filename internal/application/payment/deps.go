// Package payment holds the payment orchestration use cases — the Data Plane's application
// layer (BC-6).
//
// This package is where the pieces meet: the validation plane decides whether a request is
// permissible, the risk engine decides whether it is wise, the routing engine decides where it
// should go, the gateway adapters make it happen, and the payment aggregate records what
// happened. None of those know about each other; this package is the only place that knows
// about all of them, and that is deliberate — it means each of them can be tested and changed
// in isolation, and it means the sequencing decisions that actually determine whether the
// platform double-charges live in one readable file rather than being emergent.
//
// The ordering constraints this package exists to enforce, stated once:
//
//  1. The attempt row is committed **before** the gateway call. A crash after this point leaves
//     evidence; a crash before it leaves nothing at the gateway either.
//  2. The gateway response, the state transition and the outbox event commit **together**. A
//     state change without its event, or an event without its state change, is the dual-write
//     bug the outbox pattern exists to remove.
//  3. An unknown outcome **never** advances the payment and **never** triggers failover.
//  4. Failover creates a **new attempt**; it never mutates the previous one.
package payment

import (
	"context"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/routing"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/domain/tenant"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// GatewayResolver produces a configured adapter for a merchant's connection to a gateway.
//
// Declared here rather than taken from the adapter registry package because the application
// layer must not depend on the registry's construction concerns — which HTTP client, which
// endpoint, which credential version. It asks for "the thing that can talk to Stripe on behalf
// of this merchant" and gets it.
type GatewayResolver interface {
	// Resolve returns an adapter bound to the merchant's connection, with credentials already
	// resolved from the secrets store. The returned Credentials are used for exactly this call
	// and are not retained.
	Resolve(ctx context.Context, m shared.MerchantID, g shared.GatewayID) (spi.PaymentGateway, spi.Credentials, string, error)
}

// CircuitBreaker is the narrow view of the breaker registry the orchestrator needs.
//
// The application layer declares it because `internal/infrastructure/resilience` is
// infrastructure and the dependency rule forbids importing it here. The concrete registry
// satisfies this shape without either package knowing about the other.
type CircuitBreaker interface {
	// Allow reports whether a call may proceed on this key. The returned function must be
	// called exactly once with the outcome; `counted` false means the outcome was a business
	// decline and must not count toward the breaker's error rate.
	Allow(key string) (record func(success bool, counted bool), err error)
	// State returns 0 (closed), 1 (open) or 2 (half-open), for the metric.
	State(key string) int
}

// Bulkhead bounds concurrent in-flight calls per gateway, so one slow gateway cannot consume
// the whole orchestrator's capacity.
type Bulkhead interface {
	Acquire(ctx context.Context, key string) (release func(), err error)
}

// Metrics is the orchestrator's telemetry surface. A narrow interface rather than a direct
// dependency on the Prometheus registry, so the use cases can be unit-tested with a recorder
// and so a call site cannot invent a label.
type Metrics interface {
	RecordPaymentOutcome(outcome string, currency money.Currency, method shared.PaymentMethod, gw shared.GatewayID)
	ObserveGatewayRequest(gw shared.GatewayID, op shared.Operation, outcome string, d time.Duration)
	RecordRoutingDecision(gw shared.GatewayID, reason string)
	SetCircuitState(gw shared.GatewayID, op shared.Operation, state int)
	RecordIdempotencyOutcome(outcome string)
	ObserveStage(stage string, d time.Duration)
}

// Auditor records an auditable action. The orchestrator writes one for every money-moving
// operation, inside the same transaction as the state change, so an audit trail cannot diverge
// from what actually happened.
type Auditor interface {
	Record(ctx context.Context, r ports.Repositories, action, resourceType, resourceID, outcome string, detail map[string]any) error
}

// RiskEvaluator wraps the pure risk domain function with the impure work of gathering its
// inputs — velocity counters, blocklist answers, an external score — so the use case does not
// have to. It is an interface so the gathering strategy can change (and can be short-circuited
// in tests) without touching the orchestrator.
type RiskEvaluator interface {
	Evaluate(ctx context.Context, in RiskInput) (risk.Decision, error)
}

// RiskInput is what the orchestrator knows at the moment of assessment.
type RiskInput struct {
	Policy     risk.Policy
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	PaymentID  shared.PaymentID
	Amount     money.Money
	Method     shared.PaymentMethod
	MethodRef  payment.PaymentMethodReference
	Customer   payment.CustomerReference
	Merchant   MerchantSnapshot
	Now        time.Time
}

// CandidateBuilder assembles the flat routing candidates from the gateway registry, the
// merchant's connections and live health.
//
// This is the impure counterpart to the pure routing engine: it does the I/O, resolves every
// capability question to a boolean, and hands the domain a slice it can score deterministically.
// Keeping the two apart is what makes a routing decision replayable six months later from the
// persisted plan.
type CandidateBuilder interface {
	Build(ctx context.Context, req routing.RequestContext, m MerchantSnapshot) ([]routing.Candidate, error)
}

// MerchantSnapshot is the orchestrator's read-only view of a merchant, assembled once per
// request from the merchant registry and the fail-static configuration snapshot.
//
// It is a value, not a pointer to a live aggregate, because the payment path must not be able
// to mutate merchant state, and because a snapshot taken once and used throughout gives the
// request a consistent view even if a configuration publish lands mid-flight.
type MerchantSnapshot struct {
	MerchantID    shared.MerchantID
	TenantID      shared.TenantID
	Environment   shared.Environment
	Country       shared.Country
	RiskRating    string
	ConfigVersion int

	// Status is the merchant's lifecycle state as read. It is on the snapshot rather than
	// re-read by each consumer because L5, the risk engine and the orchestrator must all judge
	// one request against *one* answer: a suspension landing between two reads would otherwise
	// produce a payment that passed validation as ACTIVE and dispatched as SUSPENDED.
	Status merchant.Status

	// ConfigPresent records whether a configuration snapshot resolved at all. False is not the
	// same as "an empty configuration": L5 fails closed on a missing snapshot, because
	// processing with no limits, no blocked countries and no enabled-currency set is worse than
	// a retryable 503.
	ConfigPresent bool

	SupportedCurrencies []money.Currency
	PaymentMethods      []shared.PaymentMethod
	// SupportedCountries is the merchant's enabled payer-country set. Empty means "all", which
	// is the overwhelmingly common configuration.
	SupportedCountries []shared.Country

	Routing routing.Policy
	Risk    risk.Policy

	// RoutableCombinations is the compiled coverage of the routing policy: which
	// (method, currency, country) triples have somewhere to go. L4 computed it at publish time
	// so that L5 can count matches instead of re-deriving the truth on every authorization.
	RoutableCombinations []RouteCombination

	// ManualCaptureAllowed is the configuration's answer to "may this merchant authorize now and
	// capture later". Folded in at publish time from both the merchant's limits and the
	// candidate gateways' descriptors, so the hot path asks one question instead of two.
	ManualCaptureAllowed bool

	MaxRefundWindow    time.Duration
	MaxPartialCaptures int
	FeatureFlags       map[string]bool

	// Connections maps a gateway to the merchant's binding, used to resolve external account
	// IDs and certification status without a second lookup.
	Connections map[shared.GatewayID]ConnectionSnapshot

	// SnapshotAge is how stale the configuration view is. Carried on the snapshot so that the
	// decision to serve, degrade or refuse is made once, visibly, rather than being implicit in
	// whichever component happened to check.
	SnapshotAge time.Duration
}

// RouteCombination is one (method, currency, country) triple the merchant's compiled
// configuration can route. An empty Country is a wildcard: the gateway is licensed for the
// corridor regardless of where the payer is.
//
// It is declared here rather than reused from the validation plane for the reason
// routing.Candidate is declared in the routing package: the application layer owns the shape it
// hands downward, and a change to a validation subject must not ripple into the orchestrator's
// dependency set.
type RouteCombination struct {
	Method   shared.PaymentMethod
	Currency money.Currency
	Country  shared.Country
}

// ConnectionSnapshot is the merchant's binding to one gateway, flattened for the hot path.
type ConnectionSnapshot struct {
	// ConnectionID identifies the binding itself, not the gateway. It travels onto the attempt at
	// dispatch so that "which credential signed this request" is answerable from the attempt row
	// rather than by re-deriving it from the merchant's current connections — which is the wrong
	// answer the moment a connection is revoked or re-provisioned.
	ConnectionID      shared.ConnectionID
	GatewayID         shared.GatewayID
	ExternalAccountID string
	Status            gateway.ConnectionStatus
	Certified         bool
	SecretRef         string
}

// UsableForPayments reports whether the router may dispatch over this connection. It mirrors
// gateway.Connection.IsUsableForPayments so that the flattened snapshot cannot answer the
// question differently from the aggregate it was flattened from.
func (c ConnectionSnapshot) UsableForPayments() bool {
	return c.Status == gateway.StatusCertified || c.Status == gateway.StatusDegraded
}

// Principals exposes the authenticated caller's identity to the parts of the application layer
// that must judge it — L5's scope rules, and the audit record on a manual gate.
//
// It is an interface, and it reads from the context rather than from a command field, for one
// reason: tenant and principal identity has exactly one origin in this platform (baseline
// §16.2), and a command field is a second origin that a caller can set. The concrete
// implementation is the platform's tenant-context package, which the dependency rule forbids
// this layer from importing directly — which is precisely why the narrow interface exists.
type Principals interface {
	// FromContext returns the principal's subject identifier and granted scopes. The boolean is
	// false when the context carries no authenticated principal, which every mutating entry
	// point treats as a refusal rather than as an empty scope set.
	FromContext(ctx context.Context) (id string, scopes []string, ok bool)
}

// BlocklistQuery is the set of identifiers a blocklist is asked about, in one round trip.
//
// One query rather than four is deliberate: the answers are read from the same store in the
// same pipeline, so they are available together or not at all, and the risk domain's
// Assessment models exactly that with a single BlocklistAvailable bit.
type BlocklistQuery struct {
	TenantID    shared.TenantID
	MerchantID  shared.MerchantID
	Fingerprint string
	EmailHash   string
	IPAddress   string
	CustomerRef string
}

// BlocklistAnswer is what the blocklist store said. Available false means the store could not
// be read; the other three fields are then meaningless and the risk policy's posture decides.
type BlocklistAnswer struct {
	Available         bool
	OnPlatform        bool
	OnMerchant        bool
	OnMerchantAllowed bool
}

// Blocklist is the narrow view of the platform's and the merchant's known-bad sets.
//
// Declared here because the implementation is a Redis-backed set in infrastructure and the
// dependency rule forbids importing it. The interface is one method because the failure mode
// that matters — a partial read that looks like a clean answer — is only expressible if the
// four lookups share a result.
type Blocklist interface {
	Lookup(ctx context.Context, q BlocklistQuery) (BlocklistAnswer, error)
}

// CustomerHistoryProvider supplies the relationship summary the SCA exemption rules rest on.
//
// Separate from Blocklist because it is a different store with a different failure posture: a
// missing history is genuinely "no relationship", which is a safe reading, whereas a missing
// blocklist answer is not.
type CustomerHistoryProvider interface {
	History(ctx context.Context, m shared.MerchantID, customerRef string) (risk.CustomerHistory, error)
}

// HealthProvider is the live circuit and health view for one (gateway, operation).
//
// The routing candidate needs three things from it — a state, a 0..1 score and a tail latency —
// and taking them from one aggregate read guarantees they describe the same instant. Three
// separate accessors would let a candidate be scored with a healthy score and a degraded
// latency, which is a decision nobody made.
type HealthProvider interface {
	Get(ctx context.Context, g shared.GatewayID, op shared.Operation) (*gateway.Health, error)
}

// GatewayCatalog is the read side of the gateway registry: descriptors and cost models.
//
// It is separate from ports.GatewayRepository because that one is transactional and
// control-plane shaped, and the candidate builder runs on the payment hot path against a warmed
// cache. Giving the hot path the repository would let someone add a synchronous database read to
// a 5 ms budget without noticing they had done it.
type GatewayCatalog interface {
	Get(ctx context.Context, g shared.GatewayID) (*gateway.Gateway, error)
	List(ctx context.Context) ([]*gateway.Gateway, error)
}

// TenantPolicy is the entitlement and residency view of a tenant.
//
// Both questions are hard filters in the routing engine, and both are properties of the tenant
// rather than of the merchant, which is why they are read here rather than folded into the
// merchant snapshot: revoking a tenant's entitlement must take effect everywhere at once,
// without a configuration republish per merchant.
type TenantPolicy interface {
	Get(ctx context.Context, t shared.TenantID) (*tenant.Tenant, error)
}

// SuccessSample is the observed authorization outcome count for one routing key, plus the
// merchant's own baseline. It is the input to routing.SmoothSuccessRate, and it carries the
// prior explicitly so that a gateway with six observations cannot outrank one with four
// thousand.
type SuccessSample struct {
	Successes int64
	Samples   int64
	// Prior is the merchant's 30-day baseline authorization rate on [0, 1]. Zero is treated as
	// "no baseline" by the caller and substituted with the platform default, because a literal
	// prior of zero would drag every freshly-certified connection to the bottom of the ranking.
	Prior float64
}

// SuccessRates supplies the observed authorization counts the routing score is computed from.
//
// A port rather than a direct read of the payments table: the counts are windowed, are kept in
// the counter store, and are read for every candidate on every payment. A SQL aggregate here
// would be the single most expensive thing in the dispatch budget.
type SuccessRates interface {
	For(ctx context.Context, g shared.GatewayID, m shared.PaymentMethod, c money.Currency, issuer shared.Country) (SuccessSample, error)
}

// AdapterSource resolves a gateway slug to the adapter that speaks its API.
//
// The concrete implementation is the adapter registry, which this layer may not import: the
// registry owns HTTP clients, base URLs and API versions, none of which the orchestrator has any
// business knowing. What it asks for is "the thing that can talk to this gateway", and it
// supplies the credentials per call.
type AdapterSource interface {
	Resolve(ctx context.Context, g shared.GatewayID) (spi.PaymentGateway, error)
}

// MerchantContextLoader assembles a MerchantSnapshot. Separated from the service so the
// fail-static behaviour (baseline §15) lives in one place and is tested on its own.
type MerchantContextLoader interface {
	Load(ctx context.Context, m shared.MerchantID) (MerchantSnapshot, error)
}

// Config tunes the orchestrator's behaviour. Every value here has a defensible default and a
// reason; none of them should be discovered by trial and error in production.
type Config struct {
	// GatewayTimeout is the hard per-call deadline. Derived from the timeout cascade in
	// docs/failure-handling.md: it must leave the orchestrator's own budget room for the
	// pre-flight stages and the post-response transaction.
	GatewayTimeout time.Duration

	// MaxAttempts bounds failover. Two is the documented default and it is not arbitrary: the
	// timeout cascade proves that three gateway calls cannot fit inside the orchestrator's
	// budget, so a third attempt would be started only to be abandoned.
	MaxAttempts int

	// SameGatewayRetries is how many times a *transport* failure is retried against the same
	// gateway before failing over. Distinct from MaxAttempts because a same-gateway retry
	// reuses the attempt's idempotency key and is therefore free of double-charge risk, whereas
	// a failover is a new authorization.
	SameGatewayRetries int

	// AuthorizationValidity is the default hold lifetime used when a gateway does not report
	// one. Deliberately conservative: expiring our record of a hold slightly early is harmless,
	// expiring it late means attempting a capture the issuer has already released.
	AuthorizationValidity time.Duration

	// EnableFailover allows failover to be switched off per environment. A kill switch, not a
	// feature flag: if failover ever starts behaving unexpectedly in production, the safe
	// response is to stop creating second attempts immediately.
	EnableFailover bool
}

// DefaultConfig returns the production defaults.
func DefaultConfig() Config {
	return Config{
		GatewayTimeout:        8 * time.Second,
		MaxAttempts:           2,
		SameGatewayRetries:    2,
		AuthorizationValidity: 7 * 24 * time.Hour,
		EnableFailover:        true,
	}
}

// Deps is the orchestrator's dependency set, supplied by the composition root.
//
// A struct rather than fifteen constructor arguments: the arguments are all interfaces, so a
// positional mistake would compile and misbehave, and a named-field literal in main.go is the
// clearest possible statement of what this service is wired to.
type Deps struct {
	UoW        ports.UnitOfWork
	Config     ports.ConfigProvider
	Merchants  MerchantContextLoader
	Gateways   GatewayResolver
	Candidates CandidateBuilder
	Risk       RiskEvaluator
	Breakers   CircuitBreaker
	Bulkheads  Bulkhead
	Metrics    Metrics
	Audit      Auditor
	Clock      shared.Clock
	Settings   Config
}
