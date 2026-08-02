// Command payments-api is the entrypoint for the Payment Transaction Processing service. Its job
// here is purely composition root: load config, construct every dependency, wire the HTTP server
// and background outbox relay, and implement graceful startup/shutdown. All business logic lives
// in internal/service and internal/repository — this file should never grow domain logic.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/example/payments-platform/services/payments-api/internal/api"
	"github.com/example/payments-platform/services/payments-api/internal/config"
	"github.com/example/payments-platform/services/payments-api/internal/events"
	appmiddleware "github.com/example/payments-platform/services/payments-api/internal/middleware"
	"github.com/example/payments-platform/services/payments-api/internal/observability"
	"github.com/example/payments-platform/services/payments-api/internal/outbox"
	"github.com/example/payments-platform/services/payments-api/internal/repository"
	"github.com/example/payments-platform/services/payments-api/internal/service"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal startup error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := observability.NewLogger(cfg.LogLevel, cfg.ServiceName, cfg.Env)
	logger.Info("starting payments-api", "env", cfg.Env, "http_port", cfg.HTTPPort)

	// Root context cancelled on SIGTERM/SIGINT — the signal Kubernetes sends before a pod is
	// killed, giving the process a chance for graceful shutdown (see
	// docs/04-failure-recovery-design.md and the readiness-probe-driven drain below).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	_, shutdownTracer, err := observability.InitTracer(ctx, cfg.ServiceName, cfg.OTELExporterEndpoint)
	if err != nil {
		return fmt.Errorf("init tracer: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracer(shutdownCtx); err != nil {
			logger.Error("tracer shutdown error", "error", err)
		}
	}()

	registry := prometheus.NewRegistry()
	metrics := observability.NewMetrics(registry)

	repo, err := repository.New(ctx, cfg.DatabaseDSN, cfg.DBMaxConns, cfg.DBMinConns, cfg.DBConnTimeout)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer repo.Close()

	var publisher events.Publisher
	if cfg.AuthDisabled { // proxy for "local dev" — see config.validate()
		publisher = noopPublisher{logger: logger}
	} else {
		sqsPublisher, err := events.NewSQSPublisher(ctx, cfg.AWSRegion, cfg.PaymentEventsQueueURL)
		if err != nil {
			return fmt.Errorf("init sqs publisher: %w", err)
		}
		publisher = sqsPublisher
	}

	paymentService := service.NewPaymentService(repo, metrics, logger)
	handlers := api.NewHandlers(paymentService, logger)

	// The readiness probe goes through a circuit breaker on the database dependency (see
	// internal/middleware/circuitbreaker.go and docs/08-runbook.md section 3): once the DB has
	// failed enough consecutive readiness checks, the breaker opens and readiness fails fast
	// without hammering an already-struggling database with probe traffic every 5s, and
	// half-opens automatically to test recovery per CircuitBreakerOpenDuration.
	dbBreaker := appmiddleware.NewCircuitBreaker("database", cfg.CircuitBreakerFailureThreshold, cfg.CircuitBreakerOpenDuration,
		func(name string, s appmiddleware.BreakerState) {
			logger.Info("circuit breaker state change", "dependency", name, "state", s)
			metrics.CircuitBreakerState.WithLabelValues(name).Set(circuitStateMetricValue(s))
		})

	router := api.NewRouter(api.RouterConfig{
		Handlers:       handlers,
		Metrics:        metrics,
		Logger:         logger,
		DBPinger:       breakerDBPinger{inner: repo, breaker: dbBreaker},
		JWKSURL:        cfg.JWKSURL,
		JWTIssuer:      cfg.JWTIssuer,
		JWTAudience:    cfg.JWTAudience,
		AuthDisabled:   cfg.AuthDisabled,
		RateLimitRPS:   cfg.RateLimitPerClientRPS,
		RateLimitBurst: cfg.RateLimitPerClientBurst,
	}, promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	httpServer := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.HTTPPort),
		Handler:      router,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	relay := outbox.NewRelay(repo, publisher, metrics, logger, cfg.OutboxPollInterval, cfg.OutboxBatchSize, cfg.OutboxMaxAttempts)
	go relay.Run(ctx)

	// Periodically sample the DB connection pool so DBPoolInUseConns/DBPoolIdleConns
	// (docs/06-observability.md, "DBConnectionPoolSaturated" alert) reflect reality instead of
	// sitting at zero forever.
	go reportPoolStats(ctx, repo, metrics)

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, starting graceful shutdown")
	case err := <-serverErr:
		return fmt.Errorf("http server error: %w", err)
	}

	// Graceful shutdown: stop accepting new connections, let in-flight requests finish within
	// the grace period, THEN let the outbox relay and DB pool close via their own deferred
	// Close() calls / context cancellation. This ordering matters: killing the DB pool before
	// in-flight requests finish would turn a clean shutdown into a wave of 500s.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGracePeriod)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("http server graceful shutdown error", "error", err)
	}

	logger.Info("shutdown complete")
	return nil
}

// reportPoolStats samples the DB connection pool every few seconds until ctx is cancelled.
func reportPoolStats(ctx context.Context, repo *repository.Repository, metrics *observability.Metrics) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stat := repo.Stat()
			metrics.DBPoolInUseConns.Set(float64(stat.AcquiredConns()))
			metrics.DBPoolIdleConns.Set(float64(stat.IdleConns()))
		}
	}
}

// breakerDBPinger adapts repository.Repository's Ping through a CircuitBreaker so the readiness
// probe (see internal/api/health.go) stops hammering an already-struggling database every 5
// seconds once it's clearly down, per docs/08-runbook.md section 3.
type breakerDBPinger struct {
	inner   api.DBPinger
	breaker *appmiddleware.CircuitBreaker
}

func (p breakerDBPinger) Ping(ctx context.Context) error {
	return p.breaker.Execute(ctx, p.inner.Ping)
}

func circuitStateMetricValue(s appmiddleware.BreakerState) float64 {
	switch s {
	case appmiddleware.StateOpen:
		return 1
	case appmiddleware.StateHalfOpen:
		return 0.5
	default:
		return 0
	}
}

// noopPublisher is used only when AuthDisabled (local dev signal — see config.validate(), which
// refuses this combination in production). Logs instead of publishing so local development
// doesn't require live AWS credentials.
type noopPublisher struct {
	logger *slog.Logger
}

func (p noopPublisher) Publish(ctx context.Context, eventType string, payload []byte) error {
	p.logger.InfoContext(ctx, "noop publisher (local dev): would publish event", "event_type", eventType, "payload", string(payload))
	return nil
}
