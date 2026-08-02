package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

// InitTracer wires up the OpenTelemetry TracerProvider described in docs/06-observability.md.
//
// If otlpEndpoint is empty (local dev with no Collector running), the TracerProvider is still
// created and spans are still generated — they simply aren't exported anywhere. This keeps the
// tracing code path identical across every environment instead of branching the whole app on
// "is tracing enabled," which is a common source of "works in staging, breaks in prod because
// tracing was never actually exercised" bugs.
//
// In production, otlpEndpoint points at the in-cluster OpenTelemetry Collector DaemonSet, which
// forwards to AWS X-Ray or a self-hosted Tempo/Jaeger backend (deployment detail lives in
// deploy/k8s, not here — this code is backend-agnostic by design).
func InitTracer(ctx context.Context, serviceName, otlpEndpoint string) (trace.Tracer, func(context.Context) error, error) {
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("observability: build resource: %w", err)
	}

	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}

	if otlpEndpoint != "" {
		exporter, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(otlpEndpoint),
			// TLS is terminated by the service mesh sidecar in-cluster (see ADR in
			// docs/05-security-architecture.md); the local hop to the sidecar is plaintext by
			// convention for OTLP exporters in a mesh, not a security gap.
			otlptracegrpc.WithInsecure(),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("observability: create otlp exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)

	return tp.Tracer(serviceName), tp.Shutdown, nil
}
