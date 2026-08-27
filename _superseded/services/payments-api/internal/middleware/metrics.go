package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/example/payments-platform/services/payments-api/internal/observability"
)

// HTTPMetrics records the RED metrics (Rate, Errors, Duration) for every request, labeled by
// route (not raw path — raw paths would blow up cardinality for path-parameterized routes like
// /v1/payments/{id}) and status class. See docs/06-observability.md.
func HTTPMetrics(m *observability.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			duration := time.Since(start).Seconds()
			m.HTTPRequestDuration.WithLabelValues(route, r.Method).Observe(duration)
			m.HTTPRequestsTotal.WithLabelValues(route, r.Method, strconv.Itoa(rec.status)).Inc()
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
