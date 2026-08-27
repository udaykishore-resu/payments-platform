// Command payment-orchestrator runs the payment pipeline: routing, gateway dispatch, L6 response
// validation, the L7 state transition and the outbox write, plus the reconciler that resolves
// ambiguous outcomes.
//
// It is the only binary that talks to a gateway, and that concentration is the point: one process
// holds the circuit breakers, the bulkheads and the per-gateway HTTP clients, so a gateway's
// degradation is bounded by one deployment's resources rather than spread across every pod that
// happens to serve a payment.
//
// Its shutdown budget is the longest on the fleet after `workflow-worker`, and the reason is
// stated in docs/lld.md §2.5: **never abort a gateway call at shutdown**. An aborted call is a
// TIMEOUT_UNKNOWN we then have to reconcile, so the platform pays sixty seconds of grace to avoid
// manufacturing ambiguity.
//
// This file is wiring only.
package main

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/registry"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/simulator"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/internal/platform/health"
	"github.com/udaykishore-resu/payments-platform/internal/platform/runtime"
	"github.com/udaykishore-resu/payments-platform/internal/transport/grpcapi"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
)

var (
	version = ""
	commit  = ""
	date    = ""
)

const serviceName = "payment-orchestrator"

// env is this binary's configuration.
//
// It carries the mesh group rather than the public HTTP group: the orchestrator is reached over
// internal gRPC with mTLS workload identity, never from the public ingress. It also carries the
// config-snapshot group, because it is the process that reads merchant configuration on the money
// path and therefore the one the §15 staleness cliff applies to.
type env struct {
	runtime.ServiceEnv
	runtime.PostgresEnv
	runtime.RedisEnv
	runtime.MeshEnv
	runtime.GatewayEnv
	runtime.ConfigSnapshotEnv
	// SecretsEnv arrived with the orchestration service: dispatch resolves a credential per call,
	// which is the whole reason this deployable could not be wired before.
	runtime.SecretsEnv

	// AdminAddr serves the probes and metrics. There is no public listener.
	AdminAddr string `env:"PP_ADMIN_ADDR" default:":8081"`
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
	// The second of the two guards on the gateway simulator; the first is the container image's
	// guard stage. Two, because the consequence of getting it wrong is a payment that reports
	// authorized and moved no money.
	if err := runtime.RefuseSimulatorInProduction(environment, cfg.EnableSimulator); err != nil {
		return runtime.ReportStartupFailure("gateway configuration", err)
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

	clock := shared.SystemClock{}
	uow := postgres.NewUnitOfWork(pool, clock, !environment.IsProduction())
	// The unit of work backs the gateway catalogue reads the gRPC surface serves, and now also the
	// orchestration service itself.
	catalog := runtime.NewGatewayCatalog(uow, true)

	// Gateway adapters. The registry is built with the platform's built-in factories; a
	// descriptor that claims a capability its adapter does not implement is a deploy-blocking
	// defect and must never reach traffic, which is why the registry validates at construction
	// rather than at first dispatch.
	gateways, err := newGatewayRegistry(cfg.EnableSimulator)
	if err != nil {
		return runtime.ReportStartupFailure("gateway registry", err)
	}
	log.Info("gateway adapters registered",
		attrGateways(gateways.Registered())...)

	// Endpoints come from the gateway catalogue, not from a constant: which URL a gateway is
	// reached at is deployment state, and an adapter with no configured endpoint refuses at
	// dispatch with GATEWAY_NOT_CONFIGURED rather than guessing the vendor's live URL.
	if n, err := runtime.ConfigureGateways(ctx, pool, gateways, environment, cfg.GatewayTimeout, log); err != nil {
		return runtime.ReportStartupFailure("gateway endpoints", err)
	} else if n == 0 {
		log.Warn("no gateway has a configured endpoint for this environment; every payment will fail to dispatch")
	}

	// The credential store, and the money path it makes possible. Both are startup dependencies:
	// a process that cannot resolve credentials accepts orchestration requests and fails every
	// dispatch, which reads from every dashboard as a gateway outage.
	secretsProvider, err := runtime.OpenSecrets(cfg.SecretsEnv, environment, log)
	if err != nil {
		return runtime.ReportStartupFailure("secrets", err)
	}
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

	// Resilience, keyed per gateway rather than per process: one gateway's slowness must not
	// consume the capacity another gateway's traffic needs, which is what a shared bulkhead would
	// allow. The registry mints a breaker per key on first use, so a newly-registered gateway
	// starts closed rather than inheriting another's state — and a breaker that inherited an open
	// state from an unrelated gateway would shed traffic nothing is wrong with.
	//
	// It comes from the payment stack rather than being constructed here, because two registries
	// in one process is two half-views of the same failures: the orchestrator would report to one
	// and the readiness probe would read the other.
	breakers := stack.Breakers

	probes := runtime.NewHealthRegistry(serviceName, build.Version)
	runtime.RegisterProcessLiveness(probes)
	runtime.RegisterPostgresReadiness(probes, pool)
	runtime.RegisterRedisReadiness(probes, rdb)
	// The §15 staleness cliff as a probe: past it, a merchant absent from the snapshot has no
	// policy, so every payment would fail closed and this pod should leave rotation instead.
	runtime.RegisterConfigSnapshotReadiness(probes, stack.Config.SnapshotAge, cfg.MaxStaleness)
	// Readiness also gates on at least one usable gateway: zero usable gateways means every
	// payment returns 503 NO_ELIGIBLE_GATEWAY, and it is better not to take the traffic.
	registerGatewayReadiness(probes, gateways)
	registerCatalogReadiness(probes, catalog)
	registerBreakerReadiness(probes, breakers, gateways)

	lc := &runtime.Lifecycle{
		Service: serviceName, Version: build.Version, Logger: log,
		Budgets: runtime.OrchestratorBudgets(), Telemetry: tel,
	}
	if err := lc.Budgets.Validate(90*time.Second, 10*time.Second); err != nil {
		return runtime.ReportStartupFailure("shutdown budget", err)
	}

	// The internal gRPC surface. Services are registered by the build-tagged
	// internal/transport/grpcapi/services.go once `buf generate` has produced the bindings; the
	// harness — interceptors, health, keepalive, bounded drain — is always compiled.
	grpcSrv := grpcapi.NewServer(grpcapi.Config{
		Addr:             cfg.GRPCAddr,
		Service:          serviceName,
		Metrics:          tel.Metrics,
		Logger:           log,
		EnableReflection: cfg.EnableReflection && !environment.IsProduction(),
		ShutdownTimeout:  lc.Budgets.InFlight,
	})
	grpcSrv.SetServingStatus("", true)

	admin := httpapi.NewServer(httpapi.ServerConfig{
		Addr: cfg.AdminAddr, Name: "admin", Logger: log,
	}, adminMux(probes, tel))

	lc.OnDrainStart = func() {
		// gRPC clients read the health service rather than the Kubernetes probe, so both have to
		// be told. Announcing before anything closes is what stops a client's load balancer from
		// picking this backend while it is still finishing an 8-second gateway call.
		grpcSrv.SetServingStatus("", false)
	}

	stack.AddTo(lc)
	lc.Add("admin-http", func(context.Context) error { return admin.Start() }, admin.Shutdown)
	lc.Add("grpc", func(context.Context) error { return grpcSrv.Start() }, grpcSrv.Shutdown)

	return lc.Run(ctx)
}

// adminMux serves the probes and the metrics exposition. The orchestrator has no public listener,
// so this is its only HTTP surface.
func adminMux(probes *health.Registry, tel *telemetry.Telemetry) http.Handler {
	mux := http.NewServeMux()
	probes.Mount(mux)
	mux.Handle("GET /metrics", tel.MetricsHandler())
	return mux
}

// newGatewayRegistry builds the adapter registry.
//
// The simulator engine is constructed only when enabled, so a production process holds no
// simulator state at all — not a disabled one. A disabled feature that is nevertheless
// constructed is a feature one configuration mistake away from being enabled.
func newGatewayRegistry(withSimulator bool) (*registry.Registry, error) {
	var engine *simulator.Engine
	if withSimulator {
		engine = simulator.NewEngine(simulator.EngineOptions{})
	}
	return registry.NewWithBuiltIn(engine)
}
