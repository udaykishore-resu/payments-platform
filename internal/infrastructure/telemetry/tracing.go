package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// ScopeName is the instrumentation scope every span from this platform's own code is emitted
// under. One scope, so a query can separate our spans from a library's without matching on names.
const ScopeName = "github.com/udaykishore-resu/payments-platform"

// Span attribute keys. `pp.` prefixed, because OpenTelemetry semantic conventions own the
// unprefixed namespace and a future spec version that defines `gateway_id` differently would
// silently change the meaning of our data.
const (
	AttrTenantID   = "pp.tenant_id"
	AttrTenantTier = "pp.tenant_tier"
	AttrMerchantID = "pp.merchant_id"
	AttrPaymentID  = "pp.payment_id"
	AttrAttemptID  = "pp.attempt_id"
	AttrGatewayID  = "pp.gateway_id"
	AttrOperation  = "pp.operation"
	AttrOutcome    = "pp.outcome"
	AttrPlane      = "pp.plane"

	AttrErrorCode      = "pp.error.code"
	AttrErrorCategory  = "pp.error.category"
	AttrErrorRetryable = "pp.error.retryable"

	// AttrForceSample is set by the ingress middleware when a support engineer sends
	// `X-Debug-Trace: true` and holds the `support:debug` scope. It is the documented escape
	// hatch for "reproduce it once and show me everything" (observability.md §2.3).
	AttrForceSample = "pp.force_sample"
	// AttrHighValue is set when the payment amount crosses the high-value threshold. A merchant
	// escalating about a six-figure payment is not satisfied by a 10 % chance that the trace exists.
	AttrHighValue = "pp.high_value"
	// AttrErrorAtStart is set when a span is started for work already known to be failing — a
	// retry of a failed attempt, a DLQ redrive, a compensation. Sampling decisions are made at
	// span start, so this is the only way head sampling can know about an error.
	AttrErrorAtStart = "pp.error"
)

// TracingOptions configures the tracer provider. Zero values give the production shape described
// in observability.md §2.1.
type TracingOptions struct {
	// Endpoint is the OTLP/gRPC target. In-cluster this is the node-local collector agent
	// ($(NODE_IP):4317): a node-local hop costs no cross-AZ traffic and survives a restart of the
	// gateway tier, which a direct-to-gateway exporter does not.
	Endpoint string
	// Insecure disables TLS to the collector. True in-cluster, where the mesh provides mTLS and a
	// second TLS layer buys nothing but CPU and a certificate to rotate.
	Insecure bool
	// Headers are added to every OTLP request, for a collector that authenticates.
	Headers map[string]string
	// SampleRatio is the head sampling ratio; see NewSampler.
	SampleRatio float64
	// Resource carries service.name and friends. Required.
	Resource *resource.Resource

	// Batch processor shape. The defaults are 8192/1024/2s/10s: roughly two seconds of buffer per
	// pod at peak, exported in batches large enough that the gRPC overhead is amortized. The
	// queue *drops* rather than blocking when full, which is the only acceptable behaviour —
	// telemetry must never be able to take down the money path.
	MaxQueueSize       int
	MaxExportBatchSize int
	BatchTimeout       time.Duration
	ExportTimeout      time.Duration

	// ShutdownTimeout bounds the final flush.
	ShutdownTimeout time.Duration
}

func (o *TracingOptions) applyDefaults() {
	if o.MaxQueueSize == 0 {
		o.MaxQueueSize = 8192
	}
	if o.MaxExportBatchSize == 0 {
		o.MaxExportBatchSize = 1024
	}
	if o.BatchTimeout == 0 {
		o.BatchTimeout = 2 * time.Second
	}
	if o.ExportTimeout == 0 {
		o.ExportTimeout = 10 * time.Second
	}
	if o.ShutdownTimeout == 0 {
		o.ShutdownTimeout = 5 * time.Second
	}
}

// Tracing owns the tracer provider and the goroutines its batch processor runs. It exists as a
// type rather than a bare provider so that the thing that started the background work is also
// the thing that stops it, with a deadline.
type Tracing struct {
	Provider        *sdktrace.TracerProvider
	shutdownTimeout time.Duration
}

// NewTracing builds the tracer provider, installs it and the W3C propagators globally, and
// returns a handle that can shut it down.
//
// The global installation is deliberate despite globals being generally undesirable: every
// instrumentation library (otelhttp, otelgrpc, the Kafka client) reaches for the global provider,
// and a process with a correctly configured local provider and an unconfigured global one
// produces traces with holes exactly where the library boundaries are.
func NewTracing(ctx context.Context, opts TracingOptions) (*Tracing, error) {
	opts.applyDefaults()
	if opts.Resource == nil {
		return nil, errors.New("telemetry: tracing requires a resource")
	}

	tp, err := newTracerProvider(ctx, opts)
	if err != nil {
		return nil, err
	}

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		// W3C trace context is canonical: it is what merchants' own tooling emits, and honouring
		// an inbound traceparent is what lets a merchant correlate their trace with ours.
		propagation.TraceContext{},
		// Baggage carries tenant_tier and tenant_id only. It crosses the process boundary
		// verbatim, so anything put here is exported; the egress sanitizer strips it entirely on
		// calls to external gateways.
		propagation.Baggage{},
	))

	return &Tracing{Provider: tp, shutdownTimeout: opts.ShutdownTimeout}, nil
}

// newTracerProvider builds the provider, with or without an exporter.
//
// An empty endpoint produces a provider with no span processor: spans are still created, so
// trace IDs still reach log lines and the correlation spine still works, but nothing is
// exported and no background goroutine runs. That is the right shape for a unit test, for
// platformctl and for a local run, and it is strictly better than the two alternatives — a
// no-op tracer provider (which breaks log correlation) or a real exporter pointed at nothing
// (which retries a dead socket forever and fills the logs with its own failures).
func newTracerProvider(ctx context.Context, opts TracingOptions) (*sdktrace.TracerProvider, error) {
	// Bounds the worst case from a pathological gateway adapter that attaches an attribute per
	// response field. Without limits, one bad adapter turns a trace into a memory incident on
	// the collector.
	limits := sdktrace.SpanLimits{
		AttributeCountLimit:         64,
		EventCountLimit:             32,
		LinkCountLimit:              8,
		AttributeValueLengthLimit:   1024,
		AttributePerEventCountLimit: 16,
		AttributePerLinkCountLimit:  16,
	}
	base := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(opts.Resource),
		sdktrace.WithSampler(NewSampler(opts.SampleRatio)),
		// WithRawSpanLimits, not the deprecated WithSpanLimits: every field above is set to an
		// explicit positive value, so the two behave identically today, but WithSpanLimits
		// silently substitutes the SDK default for any field left at zero. Using the raw form
		// means a limit deliberately set to zero later stays zero instead of becoming 128.
		sdktrace.WithRawSpanLimits(limits),
	}

	if opts.Endpoint == "" {
		return sdktrace.NewTracerProvider(base...), nil
	}

	grpcOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(opts.Endpoint)}
	if opts.Insecure {
		grpcOpts = append(grpcOpts, otlptracegrpc.WithInsecure())
	}
	if len(opts.Headers) > 0 {
		grpcOpts = append(grpcOpts, otlptracegrpc.WithHeaders(opts.Headers))
	}
	// The exporter is created without blocking on a connection. A collector that is not yet up
	// must not stop the service from starting: the span queue absorbs the gap and the exporter
	// reconnects. Export failures never block or fail a request — telemetry must not be able to
	// take down the money path.
	exp, err := otlptracegrpc.New(ctx, grpcOpts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: otlp trace exporter: %w", err)
	}

	return sdktrace.NewTracerProvider(append(base,
		sdktrace.WithBatcher(exp,
			sdktrace.WithMaxQueueSize(opts.MaxQueueSize),
			sdktrace.WithMaxExportBatchSize(opts.MaxExportBatchSize),
			sdktrace.WithBatchTimeout(opts.BatchTimeout),
			sdktrace.WithExportTimeout(opts.ExportTimeout),
		),
	)...), nil
}

// Shutdown flushes buffered spans and stops the batch processor's goroutines, under a deadline
// of its own.
//
// The deadline is not taken from the caller's context alone: a process shutting down because
// Kubernetes sent SIGTERM has a 30-second grace period shared with connection draining, and a
// collector that has gone away would otherwise consume all of it retrying an export. Losing the
// last two seconds of spans is cheaper than being SIGKILLed mid-drain.
func (t *Tracing) Shutdown(ctx context.Context) error {
	if t == nil || t.Provider == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, t.shutdownTimeout)
	defer cancel()
	if err := t.Provider.Shutdown(ctx); err != nil {
		return fmt.Errorf("telemetry: tracer shutdown: %w", err)
	}
	return nil
}

// --- sampling -----------------------------------------------------------------------------------

// NewSampler builds the head sampler: ParentBased(TraceIDRatioBased(ratio)), wrapped so that
// spans marked important at start are always kept.
//
// The head/tail split, because it is the part that is easy to get wrong.
//
// *Head* sampling happens here, in the process, at span start, and its job is volume. At 10 %
// the agents ship a tenth of the spans and the gateway tier buffers a tenth of the traces, which
// is the difference between a collector fleet that fits in three pods and one that does not. It
// is ParentBased so a decision made at the edge is honoured by every downstream service, every
// Kafka consumer and every workflow step — we never keep half a trace, which is the failure mode
// that makes trace-based debugging useless.
//
// *Tail* sampling happens in the collector gateway, after the whole trace has arrived, and its
// job is selection: keep every error, every TIMEOUT_UNKNOWN, every trace slower than the 1.5 s
// e2e SLO, every onboarding run. It can do that because by then the outcome is known.
//
// A head sampler cannot know an outcome — at span start nothing has happened yet — so "always
// sample errors" at the head means exactly one thing: sample the spans that are *started* in a
// state we already know is interesting. That is what this wrapper does, and the three flags it
// honours (force-sample from a support debug header, high-value payment, work already known to
// be failing) are the three cases where the information exists at start. Everything else is the
// tail sampler's job, and the two together are why observability.md can claim ~100 % of errors
// are retained from a 10 % head sample.
func NewSampler(ratio float64) sdktrace.Sampler {
	switch {
	case ratio <= 0:
		// A zero ratio would mean "never sample", which combined with ParentBased still honours
		// an inbound sampled decision. That is a legitimate configuration for a service that
		// should never originate a trace, so it is not clamped upward.
	case ratio > 1:
		ratio = 1
	}
	return keepImportant{base: sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))}
}

// keepImportant is the sampler wrapper. It is checked before the ratio sampler, not after: a
// forced trace must be sampled whatever the dice say.
type keepImportant struct{ base sdktrace.Sampler }

func (k keepImportant) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	for _, a := range p.Attributes {
		switch string(a.Key) {
		case AttrForceSample, AttrHighValue, AttrErrorAtStart:
			if a.Value.AsBool() {
				return sdktrace.SamplingResult{
					Decision: sdktrace.RecordAndSample,
					// Preserving the parent's tracestate keeps any upstream vendor's sampling
					// annotations intact; dropping it here would corrupt the header for everyone
					// downstream.
					Tracestate: trace.SpanContextFromContext(p.ParentContext).TraceState(),
				}
			}
		}
	}
	return k.base.ShouldSample(p)
}

func (k keepImportant) Description() string {
	return "KeepImportant{" + k.base.Description() + "}"
}

// --- helpers ------------------------------------------------------------------------------------

// StartSpan starts a span under the platform's instrumentation scope.
//
// It exists so that no call site has to name the scope or reach for the global provider, and so
// that the attributes a span needs at *start* — the ones the head sampler can see — are passed
// as arguments rather than set afterwards, where the sampler has already decided.
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return otel.Tracer(ScopeName).Start(ctx, name, trace.WithAttributes(attrs...))
}

// RecordError stamps err onto span as the platform error it is, and sets the span status.
//
// Two things happen here that a bare span.RecordError does not do.
//
// First, the apierror code and category become span attributes, so a trace query can find "every
// span that failed with GATEWAY_TIMEOUT" without regex over a message, and so the trace agrees
// with the metric and the log line about what went wrong.
//
// Second, and more importantly: the span status is set to ERROR only when the *platform* failed.
// A 422 RISK_DECLINED, a gateway hard decline and a 409 IDEMPOTENT_REQUEST_IN_PROGRESS are
// correct outcomes, recorded with an outcome attribute and status OK. Marking business declines
// as span errors would make the collector's keep-all-errors tail policy retain every decline —
// destroying both the trace budget and the meaning of every error-rate panel built on span
// status. This is the single most common way trace-based alerting gets abandoned, and it is
// avoided here by classifying on the error category rather than on "is err non-nil".
func RecordError(span trace.Span, err error) {
	if span == nil || err == nil || !span.IsRecording() {
		return
	}
	e := apierror.From(err)

	span.SetAttributes(
		attribute.String(AttrErrorCode, string(e.Code)),
		attribute.String(AttrErrorCategory, string(e.Category)),
		attribute.Bool(AttrErrorRetryable, e.Retryable),
	)
	// RecordError adds an exception event carrying the error's own text. That text is the
	// operator-facing chain, never request data — apierror keeps the wrapped cause out of the
	// serialized response for exactly this reason.
	span.RecordError(err, trace.WithAttributes(
		attribute.String(AttrErrorCode, string(e.Code)),
		attribute.String(AttrErrorCategory, string(e.Category)),
	))

	if isPlatformFailure(e.Code, e.Category) {
		span.SetStatus(codes.Error, string(e.Code))
		return
	}
	span.SetStatus(codes.Ok, "")
	span.SetAttributes(attribute.String(AttrOutcome, "declined"))
}

// isPlatformFailure separates "we failed" from "the answer was no". The four categories here are
// the ones where a human on call would want to know; the rest are the system working.
//
// GATEWAY_DECLINED is carved out of CategoryGateway rather than being reclassified, because its
// category is correct: the refusal did originate at the third party, and every routing and
// adapter path that switches on CategoryGateway needs it there. What is wrong is treating it as
// a *failure*. The published catalogue is explicit that a hard decline is the gateway working
// correctly and is deliberately not an alerting condition, which is why it alone in that
// category carries a 402 and `retryable: false`. Left uncarved, the collector's keep-all-errors
// tail policy would retain every decline on the platform — destroying both the trace budget and
// the meaning of every error-rate panel built on span status. The same carve-out, for the same
// reason, is in resilience.DefaultClassifier.
func isPlatformFailure(code apierror.Code, c apierror.Category) bool {
	if code == apierror.CodeGatewayDeclined {
		return false
	}
	switch c {
	case apierror.CategoryGateway, apierror.CategoryTimeout,
		apierror.CategoryInfrastructure, apierror.CategoryInternal:
		return true
	default:
		return false
	}
}

// TraceIDFromContext returns the 32-hex-character trace ID, or "" when ctx carries no valid span.
//
// It returns a string rather than a trace.TraceID because every consumer — the error envelope's
// traceId field, the log line, the exemplar label, the response header — wants the rendered form,
// and having each of them call .String() is how one of them ends up rendering an invalid ID as
// "00000000000000000000000000000000" and putting that in a support ticket.
func TraceIDFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// SpanIDFromContext returns the 16-hex-character span ID, or "".
func SpanIDFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.SpanID().String()
}

// IsSampled reports whether the current trace will actually be retained. Use it before doing
// expensive work solely for telemetry — building an exemplar, serializing a debug payload —
// because that work is wasted on a trace nobody will ever read.
func IsSampled(ctx context.Context) bool {
	sc := trace.SpanContextFromContext(ctx)
	return sc.IsValid() && sc.IsSampled()
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: NFR-43, NFR-46.
//
// Span attribution, the error-classification rule that keeps business declines out of the
// error budget, and the sampler that always keeps the spans an incident needs
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
