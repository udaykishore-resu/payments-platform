// Command workflow-worker drives the durable onboarding workflow: it leases instances, executes
// steps, delivers signals and runs compensations.
//
// It is the only process that leases workflow instances, and that exclusivity is load-bearing: two
// processes leasing the same instance would execute the same step twice, and a step that
// provisions a gateway account is not idempotent at the vendor.
//
// Its termination grace is the longest on the fleet — 120 seconds — and the shape of its drain is
// different from every other binary's: it stops leasing *new* instances immediately, lets the
// current activity finish, and then **releases its leases explicitly**. Releasing is what makes a
// rolling deploy cost seconds rather than `lease_duration × pods`, because another worker picks
// the instance up in milliseconds instead of waiting out the expiry.
//
// This file is wiring only.
package main

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/internal/platform/health"
	"github.com/udaykishore-resu/payments-platform/internal/platform/runtime"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	wfengine "github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	wfpostgres "github.com/udaykishore-resu/payments-platform/internal/workflows/engine/postgres"
)

var (
	version = ""
	commit  = ""
	date    = ""
)

const serviceName = "workflow-worker"

// env is this binary's configuration.
type env struct {
	runtime.ServiceEnv
	runtime.PostgresEnv
	runtime.WorkerEnv

	AdminAddr string `env:"PP_ADMIN_ADDR" default:":8081"`

	// WorkflowName is the definition this worker drives. One definition per deployment, so a
	// slow onboarding workflow cannot starve a fast one of leases — the isolation a shared worker
	// pool would not give.
	WorkflowName string `env:"PP_WORKFLOW_NAME" default:"merchant-onboarding"`

	// LeaseDuration is how long a claimed instance stays claimed without a heartbeat. It bounds
	// how long a crashed worker's instances are stranded, which is why the drain releases them
	// explicitly rather than relying on this expiring.
	LeaseDuration time.Duration `env:"PP_WORKFLOW_LEASE" default:"60s"`
	// HeartbeatInterval must be comfortably shorter than LeaseDuration: a heartbeat that races
	// the expiry produces an instance claimed by two workers, which is the one thing the lease
	// exists to prevent.
	HeartbeatInterval time.Duration `env:"PP_WORKFLOW_HEARTBEAT" default:"15s"`
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
	if cfg.HeartbeatInterval*3 > cfg.LeaseDuration {
		return runtime.ReportStartupFailure("configuration", leaseTooTight(cfg.HeartbeatInterval, cfg.LeaseDuration))
	}

	ctx := context.Background()
	build := runtime.Stamp(version, commit, date)

	tel, err := runtime.SetupTelemetry(ctx, serviceName, build, cfg.ServiceEnv, telemetry.PlaneAutomation)
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

	clock := shared.SystemClock{}
	uow := postgres.NewUnitOfWork(pool, clock, !environment.IsProduction())

	// The workflow engine. Its worker identity is the pod name, so a stranded lease names the pod
	// that holds it — which is the difference between "some worker has it" and a kubectl command.
	workerID := cfg.PodName
	if workerID == "" {
		workerID = serviceName
	}
	engine, err := wfpostgres.New(wfpostgres.Options{
		Repo:              runtime.NewWorkflowRepo(uow),
		Definitions:       wfengine.NewRegistry(),
		Activities:        wfengine.NewActivities(),
		Clock:             resilience.SystemClock(),
		WorkerID:          workerID,
		Lease:             cfg.LeaseDuration,
		HeartbeatInterval: cfg.HeartbeatInterval,
		PollInterval:      cfg.PollInterval,
		Batch:             cfg.BatchSize,
		Concurrency:       cfg.Concurrency,
		Logger:            log,
	})
	if err != nil {
		return runtime.ReportStartupFailure("workflow engine", err)
	}

	probes := runtime.NewHealthRegistry(serviceName, build.Version)
	runtime.RegisterProcessLiveness(probes)
	runtime.RegisterPostgresReadiness(probes, pool)

	lc := &runtime.Lifecycle{
		Service: serviceName, Version: build.Version, Logger: log,
		Budgets: runtime.WorkerBudgets(), Telemetry: tel,
	}
	if err := lc.Budgets.Validate(120*time.Second, 5*time.Second); err != nil {
		return runtime.ReportStartupFailure("shutdown budget", err)
	}

	worker := newWorker(workerDeps{
		Engine:   engine,
		Workflow: cfg.WorkflowName,
		Logger:   log,
	})

	admin := httpapi.NewServer(httpapi.ServerConfig{
		Addr: cfg.AdminAddr, Name: "admin", Logger: log,
	}, adminMux(probes, tel))

	lc.Add("admin-http", func(context.Context) error { return admin.Start() }, admin.Shutdown)
	lc.Add("workflow-worker", worker.Start, worker.Stop)

	return lc.Run(ctx)
}

// adminMux serves the probes and the metrics exposition.
func adminMux(probes *health.Registry, tel *telemetry.Telemetry) http.Handler {
	mux := http.NewServeMux()
	probes.Mount(mux)
	mux.Handle("GET /metrics", tel.MetricsHandler())
	return mux
}
