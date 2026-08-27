// Command payment-api is the data plane's public REST edge: the `/v1/payments` surface, the
// gateway webhook ingress' sibling, and the probes.
//
// It is the money path's front door. Its availability target is 99.99 % and its p99 is 250 ms
// excluding gateway time, which is why it holds a Redis-backed rate limiter, an adaptive
// concurrency limiter and a priority shedder — and why its shutdown budget is long enough to let
// an in-flight gateway call finish rather than manufacture a TIMEOUT_UNKNOWN to reconcile.
//
// This file is wiring only: every statement is a constructor call or a registration, in the order
// docs/lld.md §2.3 prescribes. The reasoning for each dependency lives with the dependency.
package main

import (
	"context"
	"os"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/internal/platform/runtime"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi/handlers"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi/middleware"
)

// Build stamp, set with -ldflags "-X main.version=… -X main.commit=… -X main.date=…".
//
// Variables rather than constants because the linker can only rewrite a variable, and three of
// them because -X takes one symbol at a time.
var (
	version = ""
	commit  = ""
	date    = ""
)

const serviceName = "payment-api"

// env is everything this binary reads from the environment.
//
// The embedded groups are the ones this deployable actually needs and nothing more: no Kafka,
// because the API writes to the transactional outbox and never publishes directly; no mesh
// group, because it terminates public HTTP rather than internal gRPC. A variable a binary does
// not need is a variable an operator must nevertheless set correctly.
type env struct {
	runtime.ServiceEnv
	runtime.PostgresEnv
	runtime.RedisEnv
	runtime.HTTPEnv
	runtime.AuthEnv
	runtime.ShedEnv
	runtime.ConfigSnapshotEnv
	// SecretsEnv and GatewayEnv arrived with the money path. The API dispatches through the same
	// orchestrator the workers do, so it resolves credentials and holds gateway adapters exactly
	// as they do; a variable set for one and not the other would be a fleet where the same
	// payment behaves differently depending on which pod took it.
	runtime.SecretsEnv
	runtime.GatewayEnv
}

func main() { os.Exit(run()) }

// run is the composition root.
//
// It returns an exit code rather than calling os.Exit, so that every deferred cleanup below
// actually executes. os.Exit skips deferred functions, and the skipped one is always the pool
// close.
func run() int {
	// 1. Configuration. Every missing variable is reported at once: a loader that stops at the
	//    first turns a six-variable mistake into six failed rollouts and an afternoon.
	var cfg env
	if err := runtime.LoadConfig(&cfg); err != nil {
		return runtime.ReportStartupFailure("configuration", err)
	}
	environment, err := runtime.ParseEnvironment(cfg.Environment)
	if err != nil {
		return runtime.ReportStartupFailure("configuration", err)
	}

	ctx := context.Background()
	build := runtime.Stamp(version, commit, date)

	// 2. Telemetry first, so every failure below is observable. Setting it up after the database
	//    means a database failure is invisible: exit 2, nothing in the log pipeline.
	tel, err := runtime.SetupTelemetry(ctx, serviceName, build, cfg.ServiceEnv, telemetry.PlaneData)
	if err != nil {
		return runtime.ReportStartupFailure("telemetry", err)
	}
	log := tel.Logger
	log.Info("starting", build.LogAttrs()...)
	runtime.LogRedactedConfig(log, &cfg)

	// 3. Data stores, in dependency order. pgxpool performs an eager connectivity check, so an
	//    unreachable writer is a startup failure rather than a first-request failure.
	pool, err := runtime.OpenPostgres(ctx, serviceName, cfg.PostgresEnv)
	if err != nil {
		return runtime.ReportStartupFailure("postgres", err)
	}
	defer func() { _ = pool.Close(context.Background()) }()

	rdb, err := runtime.OpenRedis(cfg.RedisEnv, cfg.Environment)
	if err != nil {
		return runtime.ReportStartupFailure("redis", err)
	}
	if rdb != nil {
		defer func() { _ = rdb.Close() }()
	}

	// 4. Ports to implementations. The unit of work asserts a clean tenant GUC outside production,
	//    where the round trip it costs is affordable and a leaked GUC is still cheap to fix.
	clock := shared.SystemClock{}
	uow := postgres.NewUnitOfWork(pool, clock, !environment.IsProduction())

	// 4a. The credential store, and the gateway adapters that consume it. Both are startup
	//     failures rather than lazily-resolved dependencies: a process that cannot choose a
	//     secrets backend, or that holds no adapter, can accept payments and complete none of
	//     them, and a pod that crash-loops with a clear error is strictly better than one that
	//     reports ready and 503s the money path.
	if err := runtime.RefuseSimulatorInProduction(environment, cfg.EnableSimulator); err != nil {
		return runtime.ReportStartupFailure("gateway simulator", err)
	}
	secretsProvider, err := runtime.OpenSecrets(cfg.SecretsEnv, environment, log)
	if err != nil {
		return runtime.ReportStartupFailure("secrets", err)
	}
	gateways, err := newGatewayRegistry(cfg.EnableSimulator)
	if err != nil {
		return runtime.ReportStartupFailure("gateway registry", err)
	}

	// Endpoints come from the gateway catalogue, not from a constant: which URL a gateway is
	// reached at is deployment state, and an adapter with no configured endpoint refuses at
	// dispatch with GATEWAY_NOT_CONFIGURED rather than guessing the vendor's live URL.
	if n, err := runtime.ConfigureGateways(ctx, pool, gateways, environment, cfg.GatewayTimeout, log); err != nil {
		return runtime.ReportStartupFailure("gateway endpoints", err)
	} else if n == 0 {
		log.Warn("no gateway has a configured endpoint for this environment; every payment will fail to dispatch")
	}

	// 4b. The money path itself. Every collaborator it needs is above; see
	//     runtime.NewPaymentStack for why the assembly is not written out here.
	stack, err := runtime.NewPaymentStack(runtime.PaymentStackConfig{
		UoW:         uow,
		Pool:        pool,
		Secrets:     secretsProvider,
		Gateways:    gateways,
		Telemetry:   tel,
		Clock:       clock,
		Environment: environment,
		Velocity:    newVelocityCounter(rdb),
		Snapshot:    cfg.ConfigSnapshotEnv,
		Settings:    paymentSettings(cfg.GatewayTimeout),
	})
	if err != nil {
		return runtime.ReportStartupFailure("payment stack", err)
	}
	runtime.LogPaymentStack(log, gateways.Registered(), runtime.SecretReferenceCount(secretsProvider))

	// 5. Resilience: the adaptive in-flight limiter, the shedder that orders what is dropped when
	//    it saturates, and the rate limiter with its local fallback.
	limiter := resilience.NewAdaptiveLimiter(resilience.AdaptiveConfig{
		Name:         serviceName,
		InitialLimit: cfg.ConcurrencyInitial,
		MinLimit:     cfg.ConcurrencyMin,
		MaxLimit:     cfg.ConcurrencyMax,
	})
	shedder := resilience.NewShedder(resilience.ShedderConfig{
		Pressure:   limiter.Pressure,
		Thresholds: resilience.DefaultShedThresholds,
		Hysteresis: resilience.DefaultShedHysteresis,
	})
	rateLimiter := newRateLimiter(rdb)

	// 6. Probes. Readiness gates on what this pod needs to do useful work; liveness gates on
	//    nothing external, because a liveness check that touches Postgres restarts the whole
	//    fleet during an Aurora failover.
	probes := runtime.NewHealthRegistry(serviceName, build.Version)
	runtime.RegisterProcessLiveness(probes)
	runtime.RegisterPostgresReadiness(probes, pool)
	runtime.RegisterRedisReadiness(probes, rdb)
	// The snapshot cliff expressed as a probe: a pod whose configuration has aged past it would
	// fail every payment closed, so it is better for it to leave rotation than to take traffic.
	runtime.RegisterConfigSnapshotReadiness(probes, stack.Config.SnapshotAge, cfg.MaxStaleness)
	registerGatewayReadiness(probes, gateways)

	// 7. The lifecycle, constructed before the transport so the readiness handler can read its
	//    drain flag.
	lc := &runtime.Lifecycle{
		Service: serviceName, Version: build.Version, Logger: log,
		Budgets: runtime.APIBudgets(), Telemetry: tel,
	}
	if err := lc.Budgets.Validate(75*time.Second, 15*time.Second); err != nil {
		return runtime.ReportStartupFailure("shutdown budget", err)
	}

	// 8. The HTTP surface. Payments are mounted: POST /v1/payments, the two reads, and the
	//    capture/refund/void follow-ups all run through the service built above.
	router := httpapi.NewRouter()
	handlers.Register(router, handlers.Deps{
		Payments: stack.Service,
		Gateways: gatewayCatalog{inner: runtime.NewGatewayCatalog(uow, true)},
		Health:   probes,
		Draining: lc.Draining,
		Metrics:  tel.MetricsHandler(),
		Service:  serviceName,
		Version:  build.Version,
		BaseURL:  cfg.PublicBaseURL,
		Region:   cfg.Region,
	})
	// The smoke assertion. Mounting is conditional on a non-nil service, so "the routes are
	// mounted" is a fact about this process rather than about this source file — and a build that
	// silently 404s the money path is the failure this catches before the first request.
	if err := assertMoneyPathMounted(router); err != nil {
		return runtime.ReportStartupFailure("routes", err)
	}

	// 8a. The edge controls: §12 stages 3, 5 and 8. Each is a startup dependency rather than an
	//     optional extra, because middleware.Config fails closed on a nil one — a nil
	//     Authenticator rejects every request with 401 and a nil Authorizer with 403 — and a
	//     binary that came up "successfully" in that state would pass every probe and serve
	//     nothing.
	authenticator, err := runtime.NewAuthenticator(cfg.AuthEnv, environment, clock)
	if err != nil {
		return runtime.ReportStartupFailure("authentication", err)
	}
	authorizer, err := runtime.NewAuthorizer(environment, cfg.Region, clock)
	if err != nil {
		return runtime.ReportStartupFailure("authorization", err)
	}
	idem, err := runtime.NewIdempotency(postgres.NewIdempotencyStore(pool, clock), newIdempotencyCache(rdb), clock)
	if err != nil {
		return runtime.ReportStartupFailure("idempotency", err)
	}

	chain := middleware.New(middleware.Config{
		Service:           serviceName,
		Routes:            router,
		Metrics:           tel.Metrics,
		Logger:            log,
		Authenticator:     authenticator,
		Authorizer:        authorizer,
		Idempotency:       idem,
		AnonymousRoutes:   middleware.AnonymousRoutes(),
		Limits:            middleware.ContractLimits{},
		RateLimiter:       rateLimiter,
		Shedder:           shedder,
		Concurrency:       limiter,
		MaxBodyBytes:      httpapi.DefaultMaxBodyBytes,
		CORS:              middleware.CORSPolicy{AllowedOrigins: runtime.SplitList(cfg.CORSOrigins)},
		HSTSMaxAgeSeconds: cfg.HSTSMaxAgeSeconds,
	})

	public := httpapi.NewServer(httpapi.ServerConfig{
		Addr:              cfg.HTTPAddr,
		Name:              "public",
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		Logger:            log,
	}, middleware.Chain(router, chain...))

	// The admin listener is a second port so that an ingress routing the public port cannot reach
	// /metrics or the probes by construction — a stronger guarantee than a routing rule.
	admin := httpapi.NewServer(httpapi.ServerConfig{
		Addr: cfg.AdminAddr, Name: "admin", Logger: log,
	}, adminMux(probes, tel))

	// 9. Registration. Nothing has started; this order is the start order and its reverse is the
	//    stop order, which is what makes "close in reverse dependency order" a property of the
	//    wiring rather than something to remember.
	// The configuration snapshot is registered first, so it is warm before the listeners open and
	// is stopped after they close. A pod serving before its first refresh would fail every
	// payment closed: a merchant absent from the snapshot is a merchant with no policy.
	// The key set is warmed before the listeners open and refreshed in the background after: a
	// pod that started serving with no keys would 401 every request while reporting ready.
	lc.Add("jwks",
		func(ctx context.Context) error { return authenticator.Start(ctx, cfg.Issuer) },
		func(context.Context) error { authenticator.Stop(); return nil })
	stack.AddTo(lc)
	lc.Add("admin-http", func(context.Context) error { return admin.Start() }, admin.Shutdown)
	lc.Add("public-http", func(context.Context) error { return public.Start() }, public.Shutdown)

	return lc.Run(ctx)
}
