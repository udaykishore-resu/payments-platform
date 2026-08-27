package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recover converts a panic in any downstream handler into a clean 500 response instead of
// crashing the whole process. This is the concrete implementation of "a memory-safety bug in one
// request handler should never take down in-flight requests on other goroutines" — see
// docs/04-failure-recovery-design.md, "Pod crash (panic, OOM)".
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.ErrorContext(r.Context(), "panic recovered in HTTP handler",
						"panic", rec, "stack", string(debug.Stack()))
					writeJSONError(w, http.StatusInternalServerError, "internal_server_error", "an unexpected error occurred")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + code + `","message":"` + message + `"}`))
}
