package telemetry_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// recordingTracer gives a test a real SDK tracer whose finished spans can be inspected, without
// an exporter, a collector or a network.
func recordingTracer(t *testing.T) (trace.Tracer, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Errorf("tracer provider shutdown: %v", err)
		}
	})
	return tp.Tracer("test"), sr
}

func TestRecordErrorStampsCodeAndCategory(t *testing.T) {
	t.Parallel()
	tracer, sr := recordingTracer(t)

	ctx, span := tracer.Start(context.Background(), "gateway.adyen.authorize")
	err := apierror.Wrap(errors.New("dial tcp: i/o timeout"), apierror.CodeGatewayTimeout, "gateway did not respond")
	telemetry.RecordError(span, err)
	span.End()
	_ = ctx

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	got := attrMap(spans[0].Attributes())

	if got[telemetry.AttrErrorCode] != string(apierror.CodeGatewayTimeout) {
		t.Errorf("%s = %v, want %s", telemetry.AttrErrorCode, got[telemetry.AttrErrorCode], apierror.CodeGatewayTimeout)
	}
	if got[telemetry.AttrErrorCategory] != string(apierror.CategoryTimeout) {
		t.Errorf("%s = %v, want %s", telemetry.AttrErrorCategory, got[telemetry.AttrErrorCategory], apierror.CategoryTimeout)
	}
	if got[telemetry.AttrErrorRetryable] != false {
		t.Errorf("%s = %v, want false", telemetry.AttrErrorRetryable, got[telemetry.AttrErrorRetryable])
	}
	if s := spans[0].Status(); s.Code != codes.Error {
		t.Errorf("span status = %v, want ERROR for a platform failure", s.Code)
	}
	if len(spans[0].Events()) == 0 {
		t.Error("no exception event was recorded; the error chain is what makes the span actionable")
	}
}

// TestBusinessDeclineIsNotASpanError is the other half of the rule, and the half that gets
// broken: a decline is the system working. Marking it ERROR makes the collector's keep-all-errors
// tail policy retain every decline and makes every error-rate panel meaningless.
func TestBusinessDeclineIsNotASpanError(t *testing.T) {
	t.Parallel()
	tracer, sr := recordingTracer(t)

	for _, code := range []apierror.Code{
		apierror.CodeRiskDeclined,
		apierror.CodeGatewayDeclined,
		apierror.CodeValidationFailed,
		apierror.CodeIdempotentRequestInProgress,
		apierror.CodeAmountExceedsLimit,
	} {
		_, span := tracer.Start(context.Background(), "pipeline.risk")
		telemetry.RecordError(span, apierror.New(code, ""))
		span.End()
	}

	for i, s := range sr.Ended() {
		if s.Status().Code == codes.Error {
			t.Errorf("span %d: a correct business outcome was recorded as a span error", i)
		}
		if attrMap(s.Attributes())[telemetry.AttrOutcome] != "declined" {
			t.Errorf("span %d: a business rejection must still be labelled with its outcome", i)
		}
	}
}

func TestRecordErrorIsSafeOnNilAndNonRecordingSpans(t *testing.T) {
	t.Parallel()
	telemetry.RecordError(nil, errors.New("boom"))
	tracer, _ := recordingTracer(t)
	_, span := tracer.Start(context.Background(), "s")
	telemetry.RecordError(span, nil)
	span.End()
	telemetry.RecordError(span, errors.New("after end")) // must not panic
}

// TestSamplerAlwaysKeepsImportantSpans covers the head half of the head/tail split: at a 0 %
// ratio nothing is sampled by chance, so anything that survives survives because it was marked.
func TestSamplerAlwaysKeepsImportantSpans(t *testing.T) {
	// Verifies: NFR-46.
	t.Parallel()
	sampler := telemetry.NewSampler(0)

	cases := []struct {
		name  string
		attrs []attribute.KeyValue
		want  sdktrace.SamplingDecision
	}{
		{"error at start", []attribute.KeyValue{attribute.Bool(telemetry.AttrErrorAtStart, true)}, sdktrace.RecordAndSample},
		{"forced by support", []attribute.KeyValue{attribute.Bool(telemetry.AttrForceSample, true)}, sdktrace.RecordAndSample},
		{"high value payment", []attribute.KeyValue{attribute.Bool(telemetry.AttrHighValue, true)}, sdktrace.RecordAndSample},
		{"flag present but false", []attribute.KeyValue{attribute.Bool(telemetry.AttrErrorAtStart, false)}, sdktrace.Drop},
		{"ordinary span", []attribute.KeyValue{attribute.String("pp.operation", "authorize")}, sdktrace.Drop},
		{"no attributes", nil, sdktrace.Drop},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := sampler.ShouldSample(sdktrace.SamplingParameters{
				ParentContext: context.Background(),
				TraceID:       mustTraceID(t, "4bf92f3577b34da6a3ce929d0e0e4736"),
				Name:          "POST /v1/payments",
				Attributes:    tc.attrs,
			})
			if res.Decision != tc.want {
				t.Fatalf("decision = %v, want %v", res.Decision, tc.want)
			}
		})
	}
}

func TestSamplerHonoursTheParentDecision(t *testing.T) {
	t.Parallel()
	sampler := telemetry.NewSampler(0)

	// A parent that was sampled at the edge must be honoured all the way down, or the trace is
	// kept in halves — which is worse than not keeping it at all.
	parent := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    mustTraceID(t, "4bf92f3577b34da6a3ce929d0e0e4736"),
		SpanID:     mustSpanID(t, "00f067aa0ba902b7"),
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	}))
	res := sampler.ShouldSample(sdktrace.SamplingParameters{
		ParentContext: parent,
		TraceID:       mustTraceID(t, "4bf92f3577b34da6a3ce929d0e0e4736"),
		Name:          "pipeline.route",
	})
	if res.Decision != sdktrace.RecordAndSample {
		t.Fatalf("decision = %v, want RecordAndSample: ParentBased must honour a sampled parent", res.Decision)
	}
}

func TestSamplerRatioIsClamped(t *testing.T) {
	t.Parallel()
	// A misconfigured ratio must not produce an invalid sampler; 1.0 is the safe clamp because
	// too much telemetry is a bill and too little is an outage you cannot explain.
	if d := telemetry.NewSampler(7).Description(); d == "" {
		t.Fatal("sampler has no description")
	}
	res := telemetry.NewSampler(7).ShouldSample(sdktrace.SamplingParameters{
		ParentContext: context.Background(),
		TraceID:       mustTraceID(t, "4bf92f3577b34da6a3ce929d0e0e4736"),
	})
	if res.Decision != sdktrace.RecordAndSample {
		t.Fatalf("a ratio above 1 must clamp to always-sample, got %v", res.Decision)
	}
}

func TestTraceIDFromContext(t *testing.T) {
	// Verifies: NFR-43, NFR-46.
	t.Parallel()
	if got := telemetry.TraceIDFromContext(context.Background()); got != "" {
		t.Errorf("TraceIDFromContext outside a span = %q, want empty — never a zeroed ID", got)
	}
	if got := telemetry.SpanIDFromContext(context.Background()); got != "" {
		t.Errorf("SpanIDFromContext outside a span = %q, want empty", got)
	}
	if telemetry.IsSampled(context.Background()) {
		t.Error("IsSampled outside a span must be false")
	}

	tracer, _ := recordingTracer(t)
	ctx, span := tracer.Start(context.Background(), "s")
	defer span.End()

	if got := telemetry.TraceIDFromContext(ctx); len(got) != 32 {
		t.Errorf("trace id = %q, want 32 hex characters", got)
	}
	if got := telemetry.SpanIDFromContext(ctx); len(got) != 16 {
		t.Errorf("span id = %q, want 16 hex characters", got)
	}
	if !telemetry.IsSampled(ctx) {
		t.Error("IsSampled = false for an always-sampled tracer")
	}
}

func attrMap(attrs []attribute.KeyValue) map[string]any {
	out := make(map[string]any, len(attrs))
	for _, a := range attrs {
		out[string(a.Key)] = a.Value.AsInterface()
	}
	return out
}

func mustTraceID(t *testing.T, s string) trace.TraceID {
	t.Helper()
	id, err := trace.TraceIDFromHex(s)
	if err != nil {
		t.Fatalf("trace id: %v", err)
	}
	return id
}

func mustSpanID(t *testing.T, s string) trace.SpanID {
	t.Helper()
	id, err := trace.SpanIDFromHex(s)
	if err != nil {
		t.Fatalf("span id: %v", err)
	}
	return id
}
