// Command webhook-ingress is the gateway webhook receiver: `POST /v1/webhooks/{gateway}`.
//
// It does one thing and does it in 50 milliseconds: verify the gateway's signature, deduplicate on
// the gateway's event id, persist the raw body, publish `webhook.received.v1`, return 202. All
// interpretation happens elsewhere, asynchronously.
//
// The budget is a stability control rather than a performance target. Every major gateway retries
// a webhook that is slow or fails, with escalating concurrency, and several disable an endpoint
// that stays slow — so a handler that interpreted events inline would, during exactly the incident
// that made it slow, be handed a multiplying retry storm and would then disable itself. Returning
// fast means our degradation never recruits the gateway into amplifying it.
//
// This file is wiring only.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/redis"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/internal/platform/health"
	"github.com/udaykishore-resu/payments-platform/internal/platform/runtime"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi/handlers"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi/middleware"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

var (
	version = ""
	commit  = ""
	date    = ""
)

const serviceName = "webhook-ingress"

// env is this binary's configuration.
//
// There is no auth group: the caller is a gateway, which holds no platform credential and is
// authenticated by its own signature scheme inside the handler. Requiring a JWKS URL here would be
// requiring an operator to configure a control that this deployable never uses.
type env struct {
	runtime.ServiceEnv
	runtime.PostgresEnv
	runtime.RedisEnv
	runtime.HTTPEnv
	runtime.ShedEnv
	// SecretsEnv and GatewayEnv arrived with the ingest route. Verifying a signature means
	// resolving the gateway's signing secret and holding the adapter that knows the gateway's
	// scheme, so this deployable needs both groups for the same reason payment-api does.
	runtime.SecretsEnv
	runtime.GatewayEnv
}

func main() { os.Exit(run()) }

func run() int {
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

	tel, err := runtime.SetupTelemetry(ctx, serviceName, build, cfg.ServiceEnv, telemetry.PlaneData)
	if err != nil {
		return runtime.ReportStartupFailure("telemetry", err)
	}
	log := tel.Logger
	log.Info("starting", build.LogAttrs()...)
	runtime.LogRedactedConfig(log, &cfg)

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

	limiter := resilience.NewAdaptiveLimiter(resilience.AdaptiveConfig{
		Name:         serviceName,
		InitialLimit: cfg.ConcurrencyInitial,
		MinLimit:     cfg.ConcurrencyMin,
		MaxLimit:     cfg.ConcurrencyMax,
	})
	// Webhook ingest is PriorityMoneyOut and is never shed at any pressure: a webhook we refuse is
	// a payment outcome we do not learn, which turns a resolvable PROCESSING into a reconciliation
	// exception. The shedder is still wired so that the *metric* exists and so the classification
	// is visible, not because P0 will ever trip.
	shedder := resilience.NewShedder(resilience.ShedderConfig{
		Pressure:   limiter.Pressure,
		Thresholds: resilience.DefaultShedThresholds,
		Hysteresis: resilience.DefaultShedHysteresis,
	})

	// The ingest path: the unit of work that stores the raw delivery, the adapter registry that
	// knows each gateway's signature scheme, and the secrets provider that resolves the signing
	// secret. All three are startup dependencies rather than lazy ones, because a process missing
	// any of them cannot verify a signature — and an ingress that accepts unverified webhooks lets
	// anyone who can reach it assert that a payment succeeded.
	if err := runtime.RefuseSimulatorInProduction(environment, cfg.EnableSimulator); err != nil {
		return runtime.ReportStartupFailure("gateway simulator", err)
	}
	clock := shared.SystemClock{}
	uow := postgres.NewUnitOfWork(pool, clock, !environment.IsProduction())
	gateways, err := newGatewayRegistry(cfg.EnableSimulator)
	if err != nil {
		return runtime.ReportStartupFailure("gateway registry", err)
	}
	// Registration compiles the adapter in; configuration binds it to this environment's record.
	// The ingress needs both even though it never dispatches: ResolveVerifier goes through the
	// same registry lookup as Resolve, so an adapter with a factory and no record refuses with
	// GATEWAY_NOT_CONFIGURED — and a 422 on every delivery is indistinguishable, from the
	// gateway's side, from an endpoint that is broken. Several gateways disable an endpoint that
	// keeps rejecting, so the cost of skipping this step is measured in deliveries never retried.
	if n, err := runtime.ConfigureGateways(ctx, pool, gateways, environment, cfg.GatewayTimeout, log); err != nil {
		return runtime.ReportStartupFailure("gateway configuration", err)
	} else if n == 0 {
		// Not fatal: a fresh environment whose gateway catalogue has not been published yet is a
		// legitimate state, and readiness already gates on having adapters. It is logged at WARN
		// because the pod will answer every delivery with a 422 until a row exists, and that is
		// worth seeing in the log rather than inferring from the response codes.
		log.Warn("no gateway has an endpoint configured for this environment; every delivery will be refused",
			slog.String("environment", string(environment)))
	}
	secretsProvider, err := runtime.OpenSecrets(cfg.SecretsEnv, environment, log)
	if err != nil {
		return runtime.ReportStartupFailure("secrets", err)
	}
	// The recorder is the platform-scoped write of a delivery whose tenant nobody knows yet. It
	// is constructed here, in the one binary that terminates an unauthenticated gateway request,
	// so that the untenanted write path exists exactly where it is needed and nowhere else.
	recorder := postgres.NewWebhookIngestStore(pool, clock)
	ingester, err := runtime.NewWebhookIngress(uow, recorder, gateways, secretsProvider, environment, clock)
	if err != nil {
		return runtime.ReportStartupFailure("webhook ingress", err)
	}
	runtime.LogPaymentStack(log, gateways.Registered(), runtime.SecretReferenceCount(secretsProvider))

	probes := runtime.NewHealthRegistry(serviceName, build.Version)
	runtime.RegisterProcessLiveness(probes)
	runtime.RegisterPostgresReadiness(probes, pool)
	runtime.RegisterRedisReadiness(probes, rdb)
	registerGatewayReadiness(probes, gateways)

	lc := &runtime.Lifecycle{
		Service: serviceName, Version: build.Version, Logger: log,
		Budgets: runtime.IngressBudgets(), Telemetry: tel,
	}
	if err := lc.Budgets.Validate(45*time.Second, 15*time.Second); err != nil {
		return runtime.ReportStartupFailure("shutdown budget", err)
	}

	router := httpapi.NewRouter()
	handlers.Register(router, handlers.Deps{
		Webhooks: ingester,
		Health:   probes,
		Draining: lc.Draining,
		Metrics:  tel.MetricsHandler(),
		Service:  serviceName,
		Version:  build.Version,
		BaseURL:  cfg.PublicBaseURL,
		Region:   cfg.Region,
	})

	chain := middleware.New(middleware.Config{
		Service:         serviceName,
		Routes:          router,
		Metrics:         tel.Metrics,
		Logger:          log,
		AnonymousRoutes: middleware.AnonymousRoutes(),
		Limits:          middleware.ContractLimits{},
		RateLimiter:     newRateLimiter(rdb),
		Shedder:         shedder,
		Concurrency:     limiter,
		// The ingest cap is four times the general API limit: a gateway's event body is not ours
		// to bound, and a settlement notification with a hundred line items is a legitimate
		// document. Past the cap the contract's 413 tells the gateway to use its own
		// payload-reference mechanism, which every major gateway has.
		MaxBodyBytes:      handlers.MaxWebhookBodyBytes,
		HSTSMaxAgeSeconds: cfg.HSTSMaxAgeSeconds,
	})

	// The smoke assertion: handlers.Register mounts the ingest route only when a service is
	// wired, so "the endpoint is served" is a fact about this process rather than about this file.
	if !router.Has(http.MethodPost, httpapi.RouteReceiveWebhook) {
		return runtime.ReportStartupFailure("routes",
			apierror.New(apierror.CodeInternalError,
				"POST /v1/webhooks/{gateway} is not mounted; the ingress would 404 every delivery"))
	}

	public := httpapi.NewServer(httpapi.ServerConfig{
		Addr: cfg.HTTPAddr,
		Name: "ingress",
		// The write timeout is far shorter than the general API's: this surface answers in 50 ms,
		// and a connection held open for 35 seconds by a slow gateway is a worker this pod cannot
		// use for the traffic it exists to absorb.
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       cfg.IdleTimeout,
		Logger:            log,
	}, middleware.Chain(router, chain...))

	admin := httpapi.NewServer(httpapi.ServerConfig{
		Addr: cfg.AdminAddr, Name: "admin", Logger: log,
	}, adminMux(probes, tel))

	lc.Add("admin-http", func(context.Context) error { return admin.Start() }, admin.Shutdown)
	lc.Add("ingress-http", func(context.Context) error { return public.Start() }, public.Shutdown)

	return lc.Run(ctx)
}

// newRateLimiter builds the per-gateway limiter, falling back to a local bucket without Redis.
func newRateLimiter(rdb *redis.Client) middleware.RateLimiter {
	if rdb == nil {
		return resilience.NewLocalLimiter(10_000, 10*time.Minute, resilience.SystemClock())
	}
	return resilience.NewDistributedLimiter(resilience.DistributedLimiterConfig{
		Backend: redis.NewRateLimiter(rdb.Redis()), Replicas: 1,
	})
}

// adminMux serves the probes and the metrics exposition on the admin listener.
func adminMux(probes *health.Registry, tel *telemetry.Telemetry) http.Handler {
	mux := http.NewServeMux()
	probes.Mount(mux)
	mux.Handle("GET /metrics", tel.MetricsHandler())
	return mux
}
