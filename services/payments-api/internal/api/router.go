package api

import (
	"log/slog"
	"net/http"

	appmiddleware "github.com/example/payments-platform/services/payments-api/internal/middleware"
	"github.com/example/payments-platform/services/payments-api/internal/observability"
)

type RouterConfig struct {
	Handlers       *Handlers
	Metrics        *observability.Metrics
	Logger         *slog.Logger
	DBPinger       DBPinger
	JWKSURL        string
	JWTIssuer      string
	JWTAudience    string
	AuthDisabled   bool
	RateLimitRPS   float64
	RateLimitBurst int
}

// NewRouter builds the full middleware chain around the route table. Order matters and is
// deliberate:
//  1. RequestID   — every subsequent layer, including panic logs, gets a correlation id.
//  2. Recover     — must wrap everything below it so a panic anywhere is still caught.
//  3. HTTPMetrics — measures full request lifetime including auth/rate-limit overhead.
//  4. Auth        — authenticate before any business logic or rate-limit-by-identity runs.
//  5. RateLimit   — depends on the authenticated client_id from Auth.
//
// metricsHandler (built by the caller via promhttp.HandlerFor against the app's dedicated
// registry — see cmd/server/main.go) is mounted on its own path, deliberately outside the
// auth/rate-limit chain: it's scraped by Prometheus from inside the cluster network and is not
// exposed through the public ALB (enforced by the Kubernetes Service/Ingress split in
// deploy/k8s, not by this code).
func NewRouter(cfg RouterConfig, metricsHandler http.Handler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/payments", cfg.Handlers.CreatePayment)
	mux.HandleFunc("GET /v1/payments/{id}", cfg.Handlers.GetPayment)
	mux.HandleFunc("GET /healthz", Livez)
	mux.HandleFunc("GET /readyz", Readyz(cfg.DBPinger).ServeHTTP)

	var handler http.Handler = mux
	handler = appmiddleware.RateLimit(cfg.RateLimitRPS, cfg.RateLimitBurst)(handler)
	handler = appmiddleware.Auth(cfg.JWKSURL, cfg.JWTIssuer, cfg.JWTAudience, cfg.AuthDisabled)(handler)
	handler = appmiddleware.HTTPMetrics(cfg.Metrics)(handler)
	handler = appmiddleware.Recover(cfg.Logger)(handler)
	handler = appmiddleware.RequestID()(handler)

	root := http.NewServeMux()
	root.Handle("/", handler)
	root.Handle("/metrics", metricsHandler)
	return root
}
