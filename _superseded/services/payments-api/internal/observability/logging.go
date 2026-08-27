package observability

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"
)

// NewLogger builds the process-wide structured JSON logger. JSON (not human-readable text) is
// used unconditionally, including in local dev, so log parsing tooling behaves identically
// everywhere and engineers debug against the same format they'll see in production.
func NewLogger(level, serviceName, env string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler).With("service", serviceName, "env", env)
}

// WithTraceContext returns a logger enriched with trace_id/span_id extracted from ctx, so every
// log line emitted during a request can be correlated with its distributed trace (see
// docs/06-observability.md, "Logs" section). Safe to call even when no span is active — it's then
// a no-op and returns logger unchanged.
func WithTraceContext(ctx context.Context, logger *slog.Logger) *slog.Logger {
	span := trace.SpanContextFromContext(ctx)
	if !span.IsValid() {
		return logger
	}
	return logger.With(
		"trace_id", span.TraceID().String(),
		"span_id", span.SpanID().String(),
	)
}
