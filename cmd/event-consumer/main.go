// Command event-consumer subscribes to the platform's topics and applies events idempotently.
//
// Delivery is at-least-once, which means duplicates are normal rather than exceptional. What makes
// the effect exactly-once in business terms is the dedup store, and what makes the dedup store
// trustworthy is that its row is written **in the same transaction as the effect it guards**
// (baseline §13.5). A dedup row committed separately from the effect is a dedup row that lies.
//
// The drain finishes the in-flight message, commits its offset, and leaves the group cleanly.
// Committing before leaving prevents reprocessing a batch; leaving cleanly triggers a cooperative
// rebalance instead of a session-timeout stall that costs every other consumer a pause.
//
// This file is wiring only.
package main

import (
	"context"
	"net/http"
	"os"
	"time"

	appwebhook "github.com/udaykishore-resu/payments-platform/internal/application/webhook"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/kafka"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/internal/platform/health"
	"github.com/udaykishore-resu/payments-platform/internal/platform/runtime"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

var (
	version = ""
	commit  = ""
	date    = ""
)

const serviceName = "event-consumer"

// env is this binary's configuration.
type env struct {
	runtime.ServiceEnv
	runtime.PostgresEnv
	runtime.KafkaEnv
	runtime.WorkerEnv
	// SecretsEnv and GatewayEnv arrived with the webhook projection: applying a stored delivery
	// re-verifies its signature, which needs the gateway's adapter and its signing secret.
	runtime.SecretsEnv
	runtime.GatewayEnv

	AdminAddr string `env:"PP_ADMIN_ADDR" default:":8081"`

	// Group is the consumer group. It is required and has no default, because a default would
	// mean two deployments that forgot to set it silently share a group — each seeing half the
	// partitions and each believing it saw everything.
	Group string `env:"PP_CONSUMER_GROUP" required:"true"`

	// Topics is the comma-separated subscription. Required for the same reason: a consumer with
	// no topics is a process that reports healthy and consumes nothing.
	Topics string `env:"PP_CONSUMER_TOPICS" required:"true"`

	// DedupRetention is how long a processed-event record is kept. It must exceed the broker's
	// own retention: a record purged while the event it guards can still be redelivered is a
	// record that has stopped guarding anything.
	DedupRetention time.Duration `env:"PP_CONSUMER_DEDUP_RETENTION" default:"168h"`
}

func main() { os.Exit(run()) }

func run() int {
	var cfg env
	if err := runtime.LoadConfig(&cfg); err != nil {
		return runtime.ReportStartupFailure("configuration", err)
	}
	topics := runtime.SplitList(cfg.Topics)
	if len(topics) == 0 {
		return runtime.ReportStartupFailure("configuration",
			apierror.New(apierror.CodeConfigurationInvalid,
				"PP_CONSUMER_TOPICS is empty; this process would consume nothing"))
	}
	environment, err := runtime.ParseEnvironment(cfg.Environment)
	if err != nil {
		return runtime.ReportStartupFailure("configuration", err)
	}
	if err := runtime.RefuseSimulatorInProduction(environment, cfg.EnableSimulator); err != nil {
		return runtime.ReportStartupFailure("gateway simulator", err)
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

	kafkaCfg, err := runtime.KafkaConfig(serviceName, cfg.KafkaEnv, cfg.Environment)
	if err != nil {
		return runtime.ReportStartupFailure("kafka configuration", err)
	}
	consumer, err := kafka.NewConsumer(kafkaCfg,
		kafka.WithConcurrency(cfg.Concurrency),
		kafka.WithMaxPollRecords(cfg.BatchSize),
		// The group instance id makes this a *static* member: a rolling restart re-joins with the
		// same identity and keeps its partitions, instead of triggering a full rebalance in which
		// every consumer in the group pauses. It comes from the pod name, which for a StatefulSet
		// is stable across restarts.
		kafka.WithGroupInstanceID(cfg.PodName),
		kafka.WithLagReporter(lagReporter{metrics: tel.Metrics}),
	)
	if err != nil {
		return runtime.ReportStartupFailure("kafka consumer", err)
	}
	defer func() { _ = consumer.Close() }()

	probes := runtime.NewHealthRegistry(serviceName, build.Version)
	runtime.RegisterProcessLiveness(probes)
	runtime.RegisterPostgresReadiness(probes, pool)

	lc := &runtime.Lifecycle{
		Service: serviceName, Version: build.Version, Logger: log,
		Budgets: runtime.ConsumerBudgets(), Telemetry: tel,
	}
	if err := lc.Budgets.Validate(90*time.Second, 5*time.Second); err != nil {
		return runtime.ReportStartupFailure("shutdown budget", err)
	}

	// The projection this deployment feeds: `webhook.received.v1` → the asynchronous webhook
	// processor. It re-verifies the stored delivery's signature before applying it, which needs
	// the gateway adapters and the secrets provider — the dependency that kept this handler
	// unwired until now.
	//
	// Every other event type is acknowledged with a DEBUG line rather than failed. A consumer that
	// errors on a type it does not project blocks its partition for every *other* type on that
	// partition, turning "somebody published a new event" into an outage of an unrelated
	// projection.
	clock := shared.SystemClock{}
	uow := postgres.NewUnitOfWork(pool, clock, !environment.IsProduction())
	gateways, err := newGatewayRegistry(cfg.EnableSimulator)
	if err != nil {
		return runtime.ReportStartupFailure("gateway registry", err)
	}
	secretsProvider, err := runtime.OpenSecrets(cfg.SecretsEnv, environment, log)
	if err != nil {
		return runtime.ReportStartupFailure("secrets", err)
	}
	processor := appwebhook.NewProcessor(appwebhook.ProcessDeps{
		UoW:       uow,
		Verifiers: runtime.NewWebhookVerifiers(gateways),
		Secrets:   runtime.NewWebhookSecrets(secretsProvider, environment),
		Clock:     clock,
	})
	runtime.LogPaymentStack(log, gateways.Registered(), runtime.SecretReferenceCount(secretsProvider))

	subscription := newSubscription(subscriptionDeps{
		Consumer: consumer,
		Handler:  webhookProjection{processor: processor, group: cfg.Group, logger: log},
		Topics:   topics,
		Group:    cfg.Group,
		Logger:   log,
	})

	admin := httpapi.NewServer(httpapi.ServerConfig{
		Addr: cfg.AdminAddr, Name: "admin", Logger: log,
	}, adminMux(probes, tel))

	lc.Add("admin-http", func(context.Context) error { return admin.Start() }, admin.Shutdown)
	lc.Add("kafka-consumer", subscription.Start, subscription.Stop)

	return lc.Run(ctx)
}

// adminMux serves the probes and the metrics exposition.
func adminMux(probes *health.Registry, tel *telemetry.Telemetry) http.Handler {
	mux := http.NewServeMux()
	probes.Mount(mux)
	mux.Handle("GET /metrics", tel.MetricsHandler())
	return mux
}
