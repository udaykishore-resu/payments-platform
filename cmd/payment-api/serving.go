package main

// The listener-side helpers: the rate limiter's construction and the admin mux. They are here
// rather than in main.go because main.go is the start order and the stop order, and a reader
// following that order should not have to step over a ServeMux.

import (
	"net/http"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/redis"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/internal/platform/health"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi/middleware"
)

// newRateLimiter builds the distributed limiter, or a purely local one when Redis is absent.
//
// The fallback is not a degradation to *no* limit: a local bucket still bounds one pod, and the
// distributed limiter falls back to exactly this when Redis becomes unreachable at runtime. The
// only difference is that the budget is enforced per pod rather than per fleet.
func newRateLimiter(rdb *redis.Client) middleware.RateLimiter {
	if rdb == nil {
		return resilience.NewLocalLimiter(10_000, 10*time.Minute, resilience.SystemClock())
	}
	return resilience.NewDistributedLimiter(resilience.DistributedLimiterConfig{
		Backend: redis.NewRateLimiter(rdb.Redis()),
		// Replicas is 1 because the Redis-backed bucket is already fleet-wide; the field exists
		// for the local-fallback path, where the per-pod share of a fleet budget is what a single
		// replica may spend on its own.
		Replicas: 1,
	})
}

// adminMux serves the probes and the metrics exposition on the admin listener.
//
// A plain ServeMux rather than the platform router: nothing here is part of the public contract.
// These paths are read by Kubernetes and Prometheus, which want the shapes those tools expect
// rather than this platform's conventions.
func adminMux(probes *health.Registry, tel *telemetry.Telemetry) http.Handler {
	mux := http.NewServeMux()
	probes.Mount(mux)
	mux.Handle("GET /metrics", tel.MetricsHandler())
	return mux
}
