package runtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/registry"
	apppayment "github.com/udaykishore-resu/payments-platform/internal/application/payment"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	domainconfig "github.com/udaykishore-resu/payments-platform/internal/domain/config"
	domaingateway "github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/domain/tenant"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	platformconfig "github.com/udaykishore-resu/payments-platform/internal/platform/config"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// PaymentStackConfig is what a composition root supplies to build the money path.
//
// Everything here is a thing the binary already had to construct for its own reasons — a pool, a
// telemetry registry, a gateway registry, a secrets provider. Nothing in this struct is a knob
// invented for this function.
type PaymentStackConfig struct {
	UoW ports.UnitOfWork
	// Pool backs the two reads that are deliberately not tenant-scoped: the platform-wide
	// configuration snapshot and the gateway catalogue's endpoints. Both happen at startup,
	// before any tenant context exists; see postgres.ConfigSnapshotReader for why inventing one
	// would be the wrong answer.
	Pool        *postgres.Pool
	Secrets     ports.SecretsProvider
	Gateways    *registry.Registry
	Telemetry   *telemetry.Telemetry
	Clock       shared.Clock
	Environment shared.Environment

	// Velocity is the rolling-counter store. Nil is legal: the L5 count checks degrade to the
	// risk policy's posture rather than to a pass, which is the documented fail-static behaviour
	// and is what lets a deployment run without Redis.
	Velocity ports.VelocityCounter

	// Snapshot tunes the fail-static configuration view. Its three thresholds are a ladder —
	// bounded < alert < cliff — and a deployment overriding one should override all three.
	Snapshot ConfigSnapshotEnv

	// Settings tunes the orchestrator. The zero value is replaced with apppayment.DefaultConfig.
	Settings apppayment.Config
}

// PaymentStack is the assembled money path plus the pieces a composition root still has to own.
//
// The service is what the transport mounts. The rest is returned rather than hidden because each
// has a lifecycle or a probe attached to it: the configuration provider needs starting and
// stopping, the breakers and bulkheads back readiness checks, and a composition root that could
// not reach them would have to construct its own — which is how two breaker registries end up in
// one process, each seeing half the failures.
type PaymentStack struct {
	Service   *apppayment.Service
	Config    *platformconfig.Provider
	Breakers  *resilience.BreakerRegistry
	Bulkheads *resilience.BulkheadRegistry
	Resolver  *apppayment.Resolver
	Loader    *apppayment.ContextLoader
}

// NewPaymentStack assembles the payment application service from ports and adapters.
//
// # Why this lives here rather than in each main.go
//
// The stack is eleven collaborators and four of them need adapting between an infrastructure
// signature and an application port. Nine binaries would each have to get that right, and the
// failure mode of getting it wrong is not a compile error — it is a process that comes up, passes
// readiness, and declines every payment because its candidate builder was handed a nil health
// provider and scored every gateway as unmeasured. scripts/check-architecture.sh caps a cmd file
// at 300 lines for the same reason this function exists: a composition root should be a list of
// constructor calls, and the moment it starts adapting interfaces it has started making decisions
// that belong somewhere they can be tested.
//
// # What it deliberately does not decide
//
// It does not open a pool, does not choose a secrets backend, does not register gateway adapters
// and does not start anything. Those are the binary's decisions — which stores it depends on and
// in what order it starts them is the part that is architecture rather than mechanism — and this
// function takes them as arguments.
func NewPaymentStack(cfg PaymentStackConfig) (*PaymentStack, error) {
	if cfg.UoW == nil {
		return nil, apierror.New(apierror.CodeInternalError, "the payment stack requires a unit of work")
	}
	if cfg.Pool == nil {
		return nil, apierror.New(apierror.CodeInternalError,
			"the payment stack requires a connection pool for the platform-wide configuration snapshot")
	}
	if cfg.Secrets == nil {
		// Named explicitly rather than tolerated: a nil secrets provider produces a process that
		// accepts payments and fails every dispatch on credential resolution, which looks like a
		// gateway outage from every dashboard.
		return nil, apierror.New(apierror.CodeInternalError,
			"the payment stack requires a secrets provider; every dispatch resolves a credential")
	}
	if cfg.Gateways == nil {
		return nil, apierror.New(apierror.CodeInternalError, "the payment stack requires a gateway registry")
	}
	if cfg.Telemetry == nil || cfg.Telemetry.Metrics == nil {
		return nil, apierror.New(apierror.CodeInternalError, "the payment stack requires a metrics registry")
	}
	if !cfg.Environment.IsValid() {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"the payment stack requires a valid environment, got %q", cfg.Environment)
	}
	if cfg.Clock == nil {
		cfg.Clock = shared.SystemClock{}
	}
	if cfg.Settings.MaxAttempts <= 0 {
		cfg.Settings = apppayment.DefaultConfig()
	}

	provider, err := platformconfig.NewProvider(platformconfig.ProviderConfig{
		Source:           &configSnapshotSource{reader: postgres.NewConfigSnapshotReader(cfg.Pool)},
		RefreshInterval:  cfg.Snapshot.RefreshInterval,
		BoundedStaleness: cfg.Snapshot.BoundedStaleness,
		MaxStaleness:     cfg.Snapshot.MaxStaleness,
		Clock:            cfg.Clock,
	})
	if err != nil {
		return nil, err
	}

	loader := apppayment.NewContextLoader(apppayment.ContextLoaderDeps{
		UoW:          cfg.UoW,
		Config:       provider,
		Clock:        cfg.Clock,
		MaxStaleness: cfg.Snapshot.MaxStaleness,
	})

	// One breaker registry and one bulkhead registry per process, keyed per gateway. Keyed rather
	// than global because one gateway's slowness must not consume the capacity another gateway's
	// traffic needs; one registry rather than several because a breaker that only some call sites
	// report to is a breaker that never trips.
	breakers := resilience.NewBreakerRegistry(resilience.BreakerRegistryConfig{Clock: resilience.SystemClock()})
	bulkheads := resilience.NewBulkheadRegistry(resilience.BulkheadRegistryConfig{})

	metrics := &paymentMetrics{reg: cfg.Telemetry.Metrics}
	breakerPort := &breakerPort{reg: breakers}

	resolver := apppayment.NewResolver(apppayment.ResolverDeps{
		Merchants:   loader,
		Secrets:     cfg.Secrets,
		Adapters:    cfg.Gateways,
		Environment: cfg.Environment,
	})

	candidates := apppayment.NewCandidateAssembler(apppayment.CandidateAssemblerDeps{
		Gateways: &gatewayCatalogRead{uow: cfg.UoW},
		Health:   &gatewayHealthPort{uow: cfg.UoW},
		Tenants:  &tenantPolicyPort{uow: cfg.UoW},
		Breakers: breakerPort,
		// Rates is nil: this platform has no windowed authorization-rate store yet, and the
		// candidate assembler treats a nil Rates as "no observation", which the routing engine
		// smooths to the merchant's prior. Supplying a fabricated sample would be worse — the
		// success term is a *ranking* input, and a made-up one silently reorders gateways.
		Rates: nil,
		// ProcessingRegionOf is nil, which answers "" for every gateway and makes the tenant's
		// residency policy refuse every non-GLOBAL tenant. That is the fail-closed direction and
		// the correct one: a gateway whose processing region nobody recorded is one we cannot
		// prove is compliant.
		ProcessingRegionOf: nil,
	})

	risk := apppayment.NewRiskAssessor(apppayment.RiskAssessorDeps{
		Velocity: cfg.Velocity,
		// Blocklists and Scorer are nil, and the risk domain distinguishes "never asked" from
		// "asked and failed" precisely so that a platform running without them claims no
		// exemptions rather than assuming clean answers.
		Clock: cfg.Clock,
	})

	validator := apppayment.NewValidator(apppayment.ValidatorDeps{
		Velocity:   cfg.Velocity,
		Principals: principalsPort{},
		Clock:      cfg.Clock,
	})

	svc := apppayment.NewService(apppayment.Deps{
		UoW:        cfg.UoW,
		Config:     provider,
		Merchants:  loader,
		Gateways:   resolver,
		Candidates: candidates,
		Risk:       risk,
		Breakers:   breakerPort,
		Bulkheads:  &bulkheadPort{reg: bulkheads},
		Metrics:    metrics,
		Audit:      NewAuditor(cfg.Clock),
		Clock:      cfg.Clock,
		Settings:   cfg.Settings,
	}, validator)

	return &PaymentStack{
		Service:   svc,
		Config:    provider,
		Breakers:  breakers,
		Bulkheads: bulkheads,
		Resolver:  resolver,
		Loader:    loader,
	}, nil
}

// AddTo registers the configuration provider's refresh loop on the lifecycle.
//
// The provider is started *before* the listeners and stopped after them, which is the ordering the
// staleness cliff depends on: a pod that started serving before its first refresh would fail every
// payment closed for one refresh interval, because a merchant that is not in the snapshot is a
// merchant with no policy.
func (s *PaymentStack) AddTo(lc *Lifecycle) {
	lc.Add("config-snapshot",
		func(ctx context.Context) error {
			// Refresh once synchronously so that readiness reflects a real snapshot rather than an
			// empty one, then hand the cadence to the background loop.
			if err := s.Config.Refresh(ctx); err != nil {
				return err
			}
			s.Config.Start(context.WithoutCancel(ctx))
			return nil
		},
		func(context.Context) error { s.Config.Stop(); return nil })
}

// --- port adapters ------------------------------------------------------------------------------
//
// Each of the types below exists because an infrastructure signature and an application port
// describe the same capability with different vocabularies, and the application layer may not
// import infrastructure (code-conventions §4, enforced by scripts/check-architecture.sh). The
// adapters live here because internal/platform/runtime is the one package that legitimately knows
// both sides — the same reason cmd/*/gateways.go exists for the transport's interfaces.

// configSnapshotSource reads published configuration through the unit of work.
//
// It is the seam that makes the data plane's independence real rather than aspirational: the
// provider asks for "everything published since this watermark", holds the answer locally, and
// keeps serving it when the control plane is unreachable. In production the same interface is
// satisfied by a compacted-topic reader; the database read is what a deployment without Kafka
// uses, and the provider cannot tell the difference.
type configSnapshotSource struct {
	reader *postgres.ConfigSnapshotReader
}

// Load returns every configuration published since the watermark.
//
// The limit bounds one refresh, not the estate: the provider passes a watermark, so a large
// estate converges over several cycles rather than in one statement holding a snapshot open
// across every tenant.
func (s *configSnapshotSource) Load(ctx context.Context, since time.Time) ([]*domainconfig.MerchantConfig, error) {
	return s.reader.Load(ctx, since, 500)
}

// gatewayCatalogRead is the routing engine's read-only view of the gateway registry.
type gatewayCatalogRead struct{ uow ports.UnitOfWork }

// Get reads one gateway descriptor.
func (g *gatewayCatalogRead) Get(ctx context.Context, id shared.GatewayID) (*domaingateway.Gateway, error) {
	return NewGatewayDescriptors(g.uow).Get(ctx, id)
}

// List reads the platform-global catalogue.
func (g *gatewayCatalogRead) List(ctx context.Context) ([]*domaingateway.Gateway, error) {
	return NewGatewayDescriptors(g.uow).List(ctx)
}

// gatewayHealthPort is the per-(gateway, operation) health read the routing candidate needs.
type gatewayHealthPort struct{ uow ports.UnitOfWork }

// Get returns the measurement for one gateway and operation.
//
// A gateway with no recorded health for an operation is normal — nothing has been dispatched
// through it yet — and the candidate assembler treats the error as "unmeasured" rather than as a
// failure. Returning a fabricated healthy measurement instead would disable a hard filter, which
// is the specific failure mode the assembler's doc comment warns about.
func (g *gatewayHealthPort) Get(ctx context.Context, id shared.GatewayID, op shared.Operation) (*domaingateway.Health, error) {
	var out *domaingateway.Health
	err := g.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		h, err := r.Health.Get(ctx, id, op)
		if err != nil {
			return err
		}
		out = h
		return nil
	})
	return out, err
}

// tenantPolicyPort supplies entitlement and residency, both hard filters in the routing engine.
type tenantPolicyPort struct{ uow ports.UnitOfWork }

// Get reads one tenant.
func (t *tenantPolicyPort) Get(ctx context.Context, id shared.TenantID) (*tenant.Tenant, error) {
	var out *tenant.Tenant
	err := t.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		found, err := r.Tenants.Get(ctx, id)
		if err != nil {
			return err
		}
		out = found
		return nil
	})
	return out, err
}

// breakerPort adapts the resilience breaker registry to the application's two-method view.
//
// The `counted` parameter is the reason this adapter is not a one-liner. A business decline is an
// answer, not a failure: a gateway that declines a stolen card is working perfectly. Counting
// declines toward the breaker's error rate would trip it during a fraud attack — shedding traffic
// from a gateway that is healthy, at the exact moment the platform most needs it.
type breakerPort struct{ reg *resilience.BreakerRegistry }

// Allow reserves a slot on the breaker for key, returning the recorder for the outcome.
func (b *breakerPort) Allow(key string) (func(success bool, counted bool), error) {
	permit, err := b.reg.Get(key).Allow()
	if err != nil {
		return nil, err
	}
	return func(success bool, counted bool) {
		if !counted {
			// The call happened and the breaker must be told the slot is free, but the outcome
			// must not count against the error rate. Reporting it as a success is how that is
			// expressed with the breaker's own vocabulary.
			permit(true)
			return
		}
		permit(success)
	}, nil
}

// State returns the breaker's state in the application layer's convention: 0 closed, 1 open,
// 2 half-open.
//
// # Why this is not the metric's encoding
//
// There are two 0/1/2 encodings of a breaker state in this platform and they are *not* the same
// one. The application layer's (internal/application/payment's `circuitOpen`) puts open at 1,
// matching resilience.State. The metric's (telemetry.CircuitState) puts half-open at 1 and open
// at 2, because a Grafana panel reads the gauge as a severity ramp.
//
// Returning the metric's encoding here would be silently, dangerously wrong in both directions
// at once: a half-open breaker would read as open and be excluded from routing, and an *open*
// one would read as neither and be routed to. The conversion for the gauge happens in
// paymentMetrics.SetCircuitState, which is the one place that speaks the metric's language.
func (b *breakerPort) State(key string) int { return int(b.reg.Get(key).State()) }

// bulkheadPort adapts the keyed bulkhead registry to the application's single-method view.
type bulkheadPort struct{ reg *resilience.BulkheadRegistry }

// Acquire takes a slot for key, returning the release the caller must run exactly once.
func (b *bulkheadPort) Acquire(ctx context.Context, key string) (func(), error) {
	return b.reg.Get(key).Acquire(ctx)
}

// paymentMetrics adapts the Prometheus registry to the orchestrator's recorder.
//
// The registry's methods take typed enums so that a call site cannot invent a label value and
// turn a dashboard into a cardinality incident (baseline §22.3). The orchestrator, being in the
// application layer, speaks plain strings. Mapping between the two is exactly what this type is
// for, and mapping an unrecognised value to a known bucket rather than passing it through is what
// keeps the guarantee: an unmapped outcome becomes `failed`, not a new time series.
type paymentMetrics struct{ reg *telemetry.Registry }

// RecordPaymentOutcome records one payment reaching an outcome.
func (m *paymentMetrics) RecordPaymentOutcome(outcome string, currency money.Currency,
	method shared.PaymentMethod, gw shared.GatewayID) {
	m.reg.RecordPaymentOutcome(paymentOutcomeOf(outcome), string(currency), string(method),
		gw.String(), telemetry.TierPooled)
}

// ObserveGatewayRequest records the latency of one external gateway call.
//
// The outcome is not a dimension of the latency histogram — the registry does not accept one, by
// design, because splitting latency by outcome multiplies the series count for a question the
// error counter already answers — so a non-success is additionally recorded as a gateway error,
// which is the metric the health FSM branches on.
func (m *paymentMetrics) ObserveGatewayRequest(gw shared.GatewayID, op shared.Operation,
	outcome string, d time.Duration) {
	m.reg.ObserveGatewayRequest(context.Background(), gw.String(), string(op), d)
	switch outcome {
	case "unknown":
		m.reg.RecordGatewayError(gw.String(), string(op), telemetry.GatewayErrTimeout)
	case "error":
		m.reg.RecordGatewayError(gw.String(), string(op), telemetry.GatewayErrTransport)
	}
}

// RecordRoutingDecision records which gateway was chosen and why.
func (m *paymentMetrics) RecordRoutingDecision(gw shared.GatewayID, reason string) {
	m.reg.RecordRoutingDecision(gw.String(), routingReasonOf(reason))
}

// SetCircuitState publishes the breaker gauge, translating from the application layer's encoding
// into the metric's. See breakerPort.State for why the two differ and why the translation lives
// here rather than there.
func (m *paymentMetrics) SetCircuitState(gw shared.GatewayID, op shared.Operation, state int) {
	m.reg.SetCircuitState(gw.String(), string(op), circuitGaugeOf(state))
}

// circuitGaugeOf maps resilience's state values onto the metric's severity ramp.
func circuitGaugeOf(state int) telemetry.CircuitState {
	// The application layer passes the state as a plain int across the port; anything outside the
	// enum's range is a wiring bug, and the safe reading of a wiring bug on a breaker gauge is
	// "closed" — an operator who sees OPEN reaches for a runbook, and inventing that from a
	// corrupt value is worse than under-reporting.
	if state < int(resilience.StateClosed) || state > int(resilience.StateHalfOpen) {
		return telemetry.CircuitClosed
	}
	// state is range-checked against the enum bounds immediately above.
	switch resilience.State(state) {
	case resilience.StateOpen:
		return telemetry.CircuitOpen
	case resilience.StateHalfOpen:
		return telemetry.CircuitHalfOpen
	case resilience.StateClosed:
		return telemetry.CircuitClosed
	default:
		return telemetry.CircuitClosed
	}
}

// RecordIdempotencyOutcome records the result of one idempotency claim.
func (m *paymentMetrics) RecordIdempotencyOutcome(outcome string) {
	m.reg.RecordIdempotencyOutcome(idempotencyOutcomeOf(outcome))
}

// ObserveStage is intentionally not a metric.
//
// Per-stage timings inside one request — merchant context, L5, risk, routing, persist, dispatch —
// are a *trace* concern: they are only useful next to each other for a single request, which is
// what a span tree shows and what a histogram cannot. Adding six more histograms would add six
// series per gateway per tenant tier to answer a question the trace already answers exactly, and
// the metric registry lint (scripts/check-metrics-cardinality.sh) exists to stop precisely that.
//
// This is a no-op rather than a removed interface method because the orchestrator's call sites are
// where a future span event would be emitted from, and deleting them would mean re-deriving the
// stage boundaries later.
func (m *paymentMetrics) ObserveStage(string, time.Duration) {}

func paymentOutcomeOf(s string) telemetry.PaymentOutcome {
	switch s {
	case "created":
		return telemetry.OutcomeCreated
	case "authorized":
		return telemetry.OutcomeAuthorized
	case "captured", "success":
		return telemetry.OutcomeCaptured
	case "declined":
		return telemetry.OutcomeDeclined
	case "voided":
		return telemetry.OutcomeVoided
	case "refunded":
		return telemetry.OutcomeRefunded
	case "unknown", "timeout_unknown":
		return telemetry.OutcomeTimeoutUnknwn
	default:
		return telemetry.OutcomeFailed
	}
}

func routingReasonOf(s string) telemetry.RoutingReason {
	switch s {
	case "primary":
		return telemetry.RoutingPrimary
	case "fallback_health":
		return telemetry.RoutingFallbackHealth
	case "fallback_error":
		return telemetry.RoutingFallbackError
	case "pinned":
		return telemetry.RoutingPinned
	case "capability":
		return telemetry.RoutingCapability
	case "cost":
		return telemetry.RoutingCost
	case "residency":
		return telemetry.RoutingResidency
	default:
		return telemetry.RoutingNoEligible
	}
}

func idempotencyOutcomeOf(s string) telemetry.IdempotencyOutcome {
	switch s {
	case "new":
		return telemetry.IdempotencyNew
	case "replay":
		return telemetry.IdempotencyReplay
	case "in_progress":
		return telemetry.IdempotencyInProgress
	default:
		return telemetry.IdempotencyConflict
	}
}

// principalsPort exposes the verified caller to the validation plane.
//
// It reads from the context rather than from a command field for the reason the port's doc
// comment gives: tenant and principal identity has exactly one origin in this platform, and a
// command field would be a second one that a caller can set.
type principalsPort struct{}

// FromContext returns the principal's subject and scopes, or false when the request carries no
// verified principal — which every mutating entry point treats as a refusal rather than as an
// empty scope set.
func (principalsPort) FromContext(ctx context.Context) (string, []string, bool) {
	tc, err := tenantctx.FromContext(ctx)
	if err != nil {
		return "", nil, false
	}
	return tc.Principal.ID, tc.AllScopes(), true
}

// LogPaymentStack writes the one startup line that says what the money path is actually wired to.
//
// It exists because "why is every payment returning NO_ELIGIBLE_GATEWAY" has, more than once,
// turned out to be an image built without an adapter or a process started without a credential
// document — and the answer is one grep away only if the process said so at start.
func LogPaymentStack(log *slog.Logger, gateways []shared.GatewayID, secretRefs int) {
	names := make([]string, 0, len(gateways))
	for _, g := range gateways {
		names = append(names, g.String())
	}
	log.Info("payment path wired",
		slog.Int("gateways", len(names)),
		slog.Any("gateway_ids", names),
		slog.Int("credential_references", secretRefs))
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: FR-40, NFR-32.
//
// Composition of the payment application service from ports, adapters and the secrets provider,
// so that nine composition roots wire the money path identically
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.

// ConfigureGateways binds each registered adapter to its endpoint for this environment.
//
// # Why registration and configuration are two steps
//
// Registration compiles a vendor's adapter into the binary; configuration says where its endpoint
// is, which transport to use and what the timeouts are. They arrive from different places and
// change at different rates — the factory is compiled in and the record comes from the gateway
// registry table — so the registry keeps them apart, and something has to bridge them at startup.
// This is that something.
//
// # Why an adapter without a record is left unconfigured rather than defaulted
//
// A gateway registered but not configured refuses at Resolve with GATEWAY_NOT_CONFIGURED, which is
// the honest answer: the adapter exists, but nobody has said where its endpoint is. Defaulting to
// the vendor's public URL would be worse in the direction that matters — it would let a sandbox
// deployment dispatch at a live endpoint because a row was missing.
//
// The per-gateway HTTP client is deliberate. Connection pools, timeouts and header limits are
// scoped per vendor so that a wedged gateway cannot starve a healthy one of connections, which a
// single shared client would allow.
func ConfigureGateways(ctx context.Context, pool *postgres.Pool, reg *registry.Registry,
	env shared.Environment, timeout time.Duration, log *slog.Logger) (int, error) {
	// The catalogue is read straight from the pool rather than through the repository, because
	// `pp.gateways` is platform-global and this read happens at startup, before any tenant
	// exists. See postgres.OperatorReports.GatewayEndpoints for why inventing one would be worse.
	catalogue, err := postgres.NewOperatorReports(pool).GatewayEndpoints(ctx, string(env))
	if err != nil {
		return 0, err
	}
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	configured := 0
	for _, g := range catalogue {
		id := shared.GatewayID(g.GatewayID)
		if g.BaseURL == "" {
			log.Warn("gateway has no endpoint for this environment and will refuse dispatch",
				slog.String("gateway", g.GatewayID),
				slog.String("environment", string(env)))
			continue
		}
		err := reg.Configure(registry.Record{
			GatewayID:   id,
			BaseURL:     g.BaseURL,
			APIVersion:  g.APIVersion,
			Environment: env,
			Timeout:     timeout,
			HTTPClient:  httpx.New(httpx.Options{GatewayID: id, Timeout: timeout}),
		})
		if err != nil {
			// An adapter this build does not carry is a catalogue row for a gateway somebody else
			// deploys, which is normal in a fleet where not every binary carries every adapter. It
			// is logged rather than fatal for that reason; a *missing endpoint* for an adapter we
			// do carry is the case worth warning about, and it is handled above.
			log.Debug("gateway in the catalogue has no adapter in this build",
				slog.String("gateway", g.GatewayID), slog.String("reason", err.Error()))
			continue
		}
		configured++
	}
	return configured, nil
}
