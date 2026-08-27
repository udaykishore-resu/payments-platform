// Command gateway-simulator serves internal/adapters/gateway/simulator over HTTP: a deterministic,
// scenario-driven stand-in for a real payment gateway.
//
// # It is a test-only deployable and there are three independent controls saying so
//
//  1. The root Dockerfile's guard stage refuses to build this image unless
//     `--build-arg ALLOW_TEST_SERVICE=1` is passed explicitly.
//  2. The production cluster's Kyverno policy denies any Pod whose image name matches
//     `gateway-simulator`.
//  3. This binary refuses to start when `PP_ENVIRONMENT=production`, below.
//
// Three, because "it will never be deployed to production" is a statement about intent, and intent
// is not a mechanism. The consequence of being wrong is a payment that reports authorized and moved
// no money — an outcome that reconciles to nothing and is discovered by a customer.
//
// This file is wiring only.
package main

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/simulator"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/internal/platform/health"
	"github.com/udaykishore-resu/payments-platform/internal/platform/runtime"
	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

var (
	version = ""
	commit  = ""
	date    = ""
)

const serviceName = "gateway-simulator"

// env is this binary's configuration.
//
// There is no Postgres group and no Kafka group: the simulator holds its state in memory, on
// purpose. A simulator with a database is a simulator whose fixtures survive a restart, and a
// test suite that depends on state surviving a restart is a test suite that passes locally and
// fails in CI.
type env struct {
	runtime.ServiceEnv

	// HTTPAddr is the simulator's listener.
	HTTPAddr  string `env:"PP_HTTP_ADDR" required:"true"`
	AdminAddr string `env:"PP_ADMIN_ADDR" default:":8081"`

	// APIKey is the shared secret adapters present. It is required even here: an unauthenticated
	// simulator on a cluster network is an endpoint anything can drive, and a test that can be
	// driven by anything is a test whose failures nobody can attribute.
	APIKey secret.Secret[string] `env:"PP_SIMULATOR_API_KEY" required:"true"`
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
	// Control 3. It is checked before telemetry so that the refusal is the first and only thing a
	// production deployment sees, rather than being one line inside a healthy-looking startup.
	if environment.IsProduction() {
		return runtime.ReportStartupFailure("environment", refuseProduction())
	}

	ctx := context.Background()
	build := runtime.Stamp(version, commit, date)

	tel, err := runtime.SetupTelemetry(ctx, serviceName, build, cfg.ServiceEnv, telemetry.PlaneObservability)
	if err != nil {
		return runtime.ReportStartupFailure("telemetry", err)
	}
	log := tel.Logger
	log.Info("starting", build.LogAttrs()...)
	log.Warn("this is a test-only deployable and must never be promoted past staging")
	runtime.LogRedactedConfig(log, &cfg)

	// The engine holds every payment, account and webhook registration this process has seen. It
	// is process-local and unbounded by design — a scenario suite is finite — and it is why this
	// deployable must never be scaled beyond one replica: two replicas would answer a capture
	// against a payment the other one authorized with "unknown payment".
	engine := simulator.NewEngine(simulator.EngineOptions{Clock: shared.SystemClock{}})
	sim := simulator.NewServer(engine, simulator.ServerOptions{
		APIKey: cfg.APIKey.Expose(),
		Clock:  shared.SystemClock{},
	})

	probes := runtime.NewHealthRegistry(serviceName, build.Version)
	runtime.RegisterProcessLiveness(probes)
	// Readiness has nothing external to gate on: the simulator depends on nothing. Registering no
	// readiness check means /readyz is up as soon as the process is, which is the truth.

	lc := &runtime.Lifecycle{
		Service: serviceName, Version: build.Version, Logger: log,
		// The ingress budget: requests are milliseconds and the grace is almost entirely endpoint
		// propagation, the same shape as webhook-ingress.
		Budgets: runtime.IngressBudgets(), Telemetry: tel,
	}
	if err := lc.Budgets.Validate(45*time.Second, 15*time.Second); err != nil {
		return runtime.ReportStartupFailure("shutdown budget", err)
	}

	public := httpapi.NewServer(httpapi.ServerConfig{
		Addr:         cfg.HTTPAddr,
		Name:         "simulator",
		WriteTimeout: 20 * time.Second,
		Logger:       log,
	}, sim)

	admin := httpapi.NewServer(httpapi.ServerConfig{
		Addr: cfg.AdminAddr, Name: "admin", Logger: log,
	}, adminMux(probes, tel))

	lc.Add("admin-http", func(context.Context) error { return admin.Start() }, admin.Shutdown)
	lc.Add("simulator-http", func(context.Context) error { return public.Start() }, public.Shutdown)

	return lc.Run(ctx)
}

// adminMux serves the probes and the metrics exposition.
func adminMux(probes *health.Registry, tel *telemetry.Telemetry) http.Handler {
	mux := http.NewServeMux()
	probes.Mount(mux)
	mux.Handle("GET /metrics", tel.MetricsHandler())
	return mux
}

// refuseProduction is control 3 of the three that keep the simulator out of production.
//
// The message names the other two, because an operator hitting this is either running the wrong
// image in the wrong place — in which case they need to know the image should not exist — or has
// set PP_ENVIRONMENT wrongly in a sandbox, in which case they need to know it is one variable.
func refuseProduction() error {
	return apierror.New(apierror.CodeConfigurationInvalid,
		"the gateway simulator refuses to run with PP_ENVIRONMENT=production").
		WithDetail(apierror.Detail{
			Field: "PP_ENVIRONMENT",
			Code:  "FORBIDDEN_IN_PRODUCTION",
			Message: "This is a test-only deployable (baseline §5). Its image is refused by the " +
				"Dockerfile guard stage without ALLOW_TEST_SERVICE=1 and by the production " +
				"cluster's Kyverno policy. If this is a sandbox, correct PP_ENVIRONMENT.",
			RuleID: "L0.NO_SIMULATOR_IN_PRODUCTION",
		})
}
