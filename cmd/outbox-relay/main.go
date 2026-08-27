// Command outbox-relay publishes the transactional outbox to Kafka.
//
// It is the second half of the outbox pattern and the reason the platform has no dual-write
// problem: a state change and its event row commit together, and this process moves the row to
// the broker afterwards. The event is therefore never lost and never published for a change that
// rolled back — at the cost of being published *late* when the relay is down, which is the trade
// the pattern exists to make.
//
// An unfinished batch is safe. Rows stay unmarked, the advisory lock is released when the
// connection closes, and another replica reclaims them — which is why the shutdown budget only has
// to cover finishing and marking the batch in flight, not draining a queue.
//
// This file is wiring only.
package main

import (
	"context"
	"net/http"
	"os"
	"time"

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

const serviceName = "outbox-relay"

// env is this binary's configuration.
type env struct {
	runtime.ServiceEnv
	runtime.PostgresEnv
	runtime.KafkaEnv
	runtime.WorkerEnv

	AdminAddr string `env:"PP_ADMIN_ADDR" default:":8081"`

	// Shard and TotalShards partition the claim across replicas.
	//
	// The partitioning is what preserves per-aggregate ordering when the relay is scaled out: one
	// aggregate's messages always fall in the same shard and are therefore always claimed by the
	// same replica, so they reach the broker in the order they were written. Without it, two
	// replicas racing on the same aggregate publish `payment.captured` before `payment.authorized`
	// often enough to matter, and a consumer that trusts ordering then builds a wrong projection.
	Shard       int `env:"PP_RELAY_SHARD" default:"0"`
	TotalShards int `env:"PP_RELAY_TOTAL_SHARDS" default:"1"`
}

func main() { os.Exit(run()) }

func run() int {
	var cfg env
	if err := runtime.LoadConfig(&cfg); err != nil {
		return runtime.ReportStartupFailure("configuration", err)
	}
	if cfg.TotalShards < 1 || cfg.Shard < 0 || cfg.Shard >= cfg.TotalShards {
		return runtime.ReportStartupFailure("configuration",
			apierror.Newf(apierror.CodeConfigurationInvalid,
				"shard %d is not within [0, %d)", cfg.Shard, cfg.TotalShards))
	}
	environment, err := runtime.ParseEnvironment(cfg.Environment)
	if err != nil {
		return runtime.ReportStartupFailure("configuration", err)
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

	// Kafka is validated at startup rather than at first publish. A misconfiguration that
	// surfaced on the first publish would surface *after* a state change had already committed:
	// the outbox row exists, the relay cannot move it, and the event is late. Late is survivable
	// — that is the pattern's guarantee — but discovering it at startup is strictly better.
	kafkaCfg, err := runtime.KafkaConfig(serviceName, cfg.KafkaEnv, cfg.Environment)
	if err != nil {
		return runtime.ReportStartupFailure("kafka configuration", err)
	}
	producer, err := kafka.NewProducer(kafkaCfg,
		// The close timeout must exceed the resource budget's own, so the producer gets a real
		// chance to flush buffered records before the lifecycle gives up on it. A producer closed
		// with records still buffered is events silently dropped after their transaction
		// committed — the one failure the outbox exists to prevent.
		kafka.WithCloseTimeout(8*time.Second),
		kafka.WithDataLossHandler(func(topic string, partition int32) {
			log.Error("kafka reported data loss on a partition",
				"topic", topic, "partition", partition)
		}),
	)
	if err != nil {
		return runtime.ReportStartupFailure("kafka producer", err)
	}
	defer func() { _ = producer.Close() }()

	clock := shared.SystemClock{}
	uow := postgres.NewUnitOfWork(pool, clock, !environment.IsProduction())

	probes := runtime.NewHealthRegistry(serviceName, build.Version)
	runtime.RegisterProcessLiveness(probes)
	runtime.RegisterPostgresReadiness(probes, pool)
	// The producer's connectivity is a readiness gate for this binary specifically: a relay that
	// cannot reach the broker has nothing to do, and shedding it lets an operator see backlog
	// growth attributed to the right cause.
	probes.RegisterReadiness("kafka", true, func(ctx context.Context) error {
		return producer.Ping(ctx)
	})

	lc := &runtime.Lifecycle{
		Service: serviceName, Version: build.Version, Logger: log,
		Budgets: runtime.RelayBudgets(), Telemetry: tel,
	}
	if err := lc.Budgets.Validate(60*time.Second, 5*time.Second); err != nil {
		return runtime.ReportStartupFailure("shutdown budget", err)
	}

	relay := newRelay(relayDeps{
		UoW:         uow,
		Publisher:   producer,
		Metrics:     tel.Metrics,
		Logger:      log,
		Shard:       cfg.Shard,
		TotalShards: cfg.TotalShards,
		Batch:       cfg.BatchSize,
		Interval:    cfg.PollInterval,
	})

	admin := httpapi.NewServer(httpapi.ServerConfig{
		Addr: cfg.AdminAddr, Name: "admin", Logger: log,
	}, adminMux(probes, tel))

	// The relay is registered *after* the admin server so that it stops *before* it: the drain
	// order is the reverse of the registration order, and the probe endpoint has to outlive the
	// worker so a shutting-down pod can still be observed while it finishes its batch.
	lc.Add("admin-http", func(context.Context) error { return admin.Start() }, admin.Shutdown)
	lc.Add("outbox-relay", relay.Start, relay.Stop)

	return lc.Run(ctx)
}

// adminMux serves the probes and the metrics exposition.
func adminMux(probes *health.Registry, tel *telemetry.Telemetry) http.Handler {
	mux := http.NewServeMux()
	probes.Mount(mux)
	mux.Handle("GET /metrics", tel.MetricsHandler())
	return mux
}
