// Command control-plane-api is the control plane's public REST edge: merchants, onboarding,
// configuration and the gateway catalogue.
//
// It is never on the payment hot path. Its availability target is 99.9 % rather than 99.99 %, and
// the difference is deliberate: a control-plane outage stops new merchants from being onboarded
// and configurations from being published, and it must not stop money from moving. That
// independence (ADR-007) is why the data plane reads configuration from a local snapshot rather
// than from this service.
//
// This file is wiring only.
package main

import (
	"context"
	"net/http"
	"os"
	"time"

	appconfig "github.com/udaykishore-resu/payments-platform/internal/application/config"
	appmerchant "github.com/udaykishore-resu/payments-platform/internal/application/merchant"
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
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

var (
	version = ""
	commit  = ""
	date    = ""
)

const serviceName = "control-plane-api"

// env is this binary's configuration.
//
// It carries the tenant-policy group that the data-plane binaries do not: the supported and
// sanctioned country lists and the declared-volume ceiling are L2 inputs, and L2 runs only where
// merchants are created.
type env struct {
	runtime.ServiceEnv
	runtime.PostgresEnv
	runtime.RedisEnv
	runtime.HTTPEnv
	runtime.AuthEnv
	runtime.ShedEnv

	// SupportedCountries is where this tenant may onboard at all, comma-separated ISO 3166-1
	// alpha-2 codes. Required: an empty list would mean "nowhere", and defaulting it to "anywhere"
	// would let a merchant be onboarded in a jurisdiction the platform holds no licence for.
	SupportedCountries string `env:"PP_L2_SUPPORTED_COUNTRIES" required:"true"`
	// SanctionedCountries is the platform's versioned sanctions list. Also required, and for the
	// same reason in the opposite direction.
	SanctionedCountries string `env:"PP_L2_SANCTIONED_COUNTRIES" required:"true"`
	// MonthlyVolumeCeilingMinor is the tenant tier's declared-volume limit in minor units, with
	// its currency. Zero disables the ceiling.
	MonthlyVolumeCeilingMinor int64  `env:"PP_L2_MONTHLY_VOLUME_CEILING" default:"0"`
	VolumeCeilingCurrency     string `env:"PP_L2_VOLUME_CEILING_CURRENCY" default:"EUR"`
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
	l2, err := tenantPolicy(cfg)
	if err != nil {
		return runtime.ReportStartupFailure("tenant policy", err)
	}

	ctx := context.Background()
	build := runtime.Stamp(version, commit, date)

	tel, err := runtime.SetupTelemetry(ctx, serviceName, build, cfg.ServiceEnv, telemetry.PlaneControl)
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

	// Ports to implementations, then use cases. Every dependency below is named at its call site;
	// there is no container and no reflection, so "who provides this?" is answered by reading up.
	clock := shared.SystemClock{}
	uow := postgres.NewUnitOfWork(pool, clock, !environment.IsProduction())
	auditor := runtime.NewAuditor(clock)
	descriptors := runtime.NewGatewayDescriptors(uow)

	merchants := appmerchant.NewService(appmerchant.Deps{
		UoW: uow, Audit: auditor, Clock: clock, L2: l2,
	})
	configuration := appconfig.NewService(appconfig.Deps{
		UoW: uow, Audit: auditor, Clock: clock, Gateways: descriptors,
	})

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

	probes := runtime.NewHealthRegistry(serviceName, build.Version)
	runtime.RegisterProcessLiveness(probes)
	runtime.RegisterPostgresReadiness(probes, pool)
	runtime.RegisterRedisReadiness(probes, rdb)

	lc := &runtime.Lifecycle{
		Service: serviceName, Version: build.Version, Logger: log,
		// NOT runtime.APIBudgets(). That set totals 55 s and is sized for payment-api's 75 s
		// grace period; this deployable's is 60 s (docs/deployment.md §1.8, and
		// helm/charts/control-plane-api/values.yaml), which leaves 45 s after the 15 s preStop
		// sleep. Pairing the two made Validate below fail unconditionally, so this binary could
		// not start in any environment — the arithmetic check working exactly as its own comment
		// says it should, on a transcription error.
		//
		// The numbers: nothing here is an 8-second gateway call. The longest legitimate unit of
		// work is a configuration validate-and-publish, which is interactive and well under ten
		// seconds, so 25 s of in-flight budget is generous. Total 40 s, five seconds of headroom
		// below the 45 s available.
		Budgets: runtime.Budgets{
			DrainDelay:     5 * time.Second,
			InFlight:       25 * time.Second,
			Resources:      5 * time.Second,
			TelemetryFlush: 5 * time.Second,
		},
		Telemetry: tel,
	}
	if err := lc.Budgets.Validate(60*time.Second, 15*time.Second); err != nil {
		return runtime.ReportStartupFailure("shutdown budget", err)
	}

	router := httpapi.NewRouter()
	handlers.Register(router, handlers.Deps{
		Merchants:     merchants,
		Configuration: configuration,
		Gateways:      gatewayCatalog{inner: runtime.NewGatewayCatalog(uow, false)},
		// Onboarding is not wired here: apponboarding.NewService needs a workflow engine, and the
		// engine belongs to `workflow-worker` — running one inside the API would mean two
		// processes leasing the same instances. The API's onboarding routes are served by the
		// deployment that also runs the engine.
		Health:   probes,
		Draining: lc.Draining,
		Metrics:  tel.MetricsHandler(),
		Service:  serviceName,
		Version:  build.Version,
		BaseURL:  cfg.PublicBaseURL,
		Region:   cfg.Region,
	})

	chain := middleware.New(middleware.Config{
		Service:           serviceName,
		Routes:            router,
		Metrics:           tel.Metrics,
		Logger:            log,
		AnonymousRoutes:   middleware.AnonymousRoutes(),
		Limits:            middleware.ContractLimits{},
		RateLimiter:       newRateLimiter(rdb),
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

	admin := httpapi.NewServer(httpapi.ServerConfig{
		Addr: cfg.AdminAddr, Name: "admin", Logger: log,
	}, adminMux(probes, tel))

	lc.Add("admin-http", func(context.Context) error { return admin.Start() }, admin.Shutdown)
	lc.Add("public-http", func(context.Context) error { return public.Start() }, public.Shutdown)

	return lc.Run(ctx)
}

// newRateLimiter builds the distributed limiter, falling back to a local bucket without Redis.
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

// tenantPolicy assembles the L2 validation inputs from configuration.
//
// It is a function rather than inline wiring because it *parses* — and parsing is the one thing a
// composition root may do beyond construction, since the alternative is passing raw strings into
// a validator that would then have to reject them at request time instead of at startup.
func tenantPolicy(cfg env) (l2Deps, error) {
	supported, err := runtime.ParseCountries(cfg.SupportedCountries)
	if err != nil {
		return l2Deps{}, err
	}
	sanctioned, err := runtime.ParseCountries(cfg.SanctionedCountries)
	if err != nil {
		return l2Deps{}, err
	}
	out := l2Deps{
		SupportedCountries:  supported,
		LicensedCountries:   supported,
		SanctionedCountries: sanctioned,
	}
	if cfg.MonthlyVolumeCeilingMinor > 0 {
		cur, err := money.ParseCurrency(cfg.VolumeCeilingCurrency)
		if err != nil {
			return l2Deps{}, err
		}
		ceiling, err := money.New(cfg.MonthlyVolumeCeilingMinor, cur)
		if err != nil {
			return l2Deps{}, err
		}
		out.MonthlyVolumeCeiling = ceiling
	}
	return out, nil
}
