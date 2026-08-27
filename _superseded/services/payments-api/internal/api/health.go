package api

import (
	"context"
	"net/http"
	"time"
)

// DBPinger is the narrow dependency the readiness probe checks. Kept as an interface (not the
// concrete repository) so this file has no import-time dependency on pgx.
type DBPinger interface {
	Ping(ctx context.Context) error
}

// Livez is the liveness probe: deliberately does NOT check the database (see
// docs/06-observability.md, "Health Checks" — a DB hiccup should not cause Kubernetes to kill and
// reschedule an otherwise-healthy pod; that's what Readyz is for).
func Livez(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// Readyz is the readiness probe: takes the pod out of the Service's endpoint list if it can't
// currently reach the database within budget, without killing the process.
func Readyz(pinger DBPinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := pinger.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("db unreachable"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
