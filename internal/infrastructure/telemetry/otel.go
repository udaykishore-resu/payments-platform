// Package telemetry is the platform's single observability composition root.
//
// One function, Setup, wires logging, tracing and metrics from one Config, and every
// `cmd/*/main.go` gets exactly one line of observability code plus one deferred Shutdown. The
// alternative — each binary assembling its own exporters, resource attributes and handler chain
// — is how seven deployables end up with six different service.name conventions, four log
// schemas and one service that quietly never exported a span.
//
// What lives here and why it is one package:
//
//   - logging.go: slog in the §5.1 JSON schema, an allowlist handler that can only emit
//     registered field names, a context-bound logger constructor, and volume sampling.
//   - tracing.go: the OTLP tracer provider, the head sampler and its relationship to the
//     collector's tail sampler, and the error-recording helper that stamps apierror codes.
//   - metrics.go: every `pp_*` metric in the baseline §22.2 contract, with typed recorders and a
//     cardinality guard at both declaration and runtime.
//
// The three are one package because they share the correlation spine — a trace ID in a log line,
// a trace exemplar on a histogram, a tenant tier on both — and splitting them would mean either
// duplicating that spine or exporting it from a fourth package that both depend on.
//
// Layering: this package imports the standard library, the OpenTelemetry and Prometheus SDKs, and
// `pkg/apierror`. It does not import `internal/domain` or `internal/application`, and must not:
// infrastructure depends on the domain, never the reverse, and a telemetry package that knew
// about a Payment would make the domain untestable without a collector.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// SampleRatio returns a pointer to r, for setting Config.TraceSampleRatio inline.
func SampleRatio(r float64) *float64 { return &r }

// Plane names the deployment plane a service belongs to. It is a resource attribute and a
// dashboard variable, and it is also what selects the default trace sampling ratio: the data
// plane is high volume and samples at 10 %, the automation plane is low volume, long running and
// customer-visible when it fails, so it samples at 100 %.
type Plane string

const (
	PlaneData          Plane = "data"
	PlaneControl       Plane = "control"
	PlaneAutomation    Plane = "automation"
	PlaneObservability Plane = "observability"
)

// Config is the complete observability configuration, read from the environment per 12-factor.
//
// Every field has a default that is correct for a local `go run`, so a developer who sets nothing
// gets a working process that logs to stderr and exports nothing. The fields that must not have a
// default — the ones where a wrong value silently mislabels every signal the service emits — are
// Service, Environment and Region, and Validate refuses to start without them.
type Config struct {
	// Service is the deployable name and becomes service.name, the `service` metric label and the
	// `service` log field. It is required: a service that mislabels itself pollutes every other
	// service's dashboards, and the mistake is invisible until someone is debugging the wrong pod.
	Service string
	// Version is the git SHA injected at build time. It is how a regression is tied to a deploy.
	Version string
	// Environment is dev | staging | prod.
	Environment string
	// Region is the AWS region, e.g. eu-west-1.
	Region string
	// Plane selects the default sampling posture; see Plane.
	Plane Plane

	// Kubernetes downward-API values. They are resource attributes rather than log fields
	// computed per line, because they are constant for the life of the process and paying to
	// serialize them per line at 2 500 lines/s is a measurable cost for zero information.
	PodName      string
	PodNamespace string
	NodeName     string

	// OTLPEndpoint is the collector agent, host:port. Empty disables span *export* while still
	// creating spans, so trace IDs still appear in logs and TraceIDFromContext still works. That
	// is the right behaviour for tests and for platformctl, and a much better default than
	// silently not tracing at all.
	OTLPEndpoint string
	OTLPInsecure bool
	OTLPHeaders  map[string]string

	// TraceSampleRatio is the head sampling ratio, 0..1. Nil means "use the plane default"
	// (observability.md §2.3). It is a pointer rather than a float with a sentinel because the
	// two values a sentinel cannot distinguish — "not configured" and "deliberately never sample"
	// — have opposite meanings, and getting that wrong is a service whose traces silently vanish.
	TraceSampleRatio *float64

	// LogLevel is the initial minimum level.
	LogLevel slog.Level
	// LogSamplePerSecond bounds repeated INFO/DEBUG lines per (level, message) per second. Zero
	// disables sampling.
	LogSamplePerSecond int
	// LogOutput defaults to os.Stdout. Stdout, not stderr: the log pipeline tails stdout and
	// stderr is where the runtime writes panics that are not ours.
	LogOutput io.Writer

	// MetricsMaxSeries is the per-metric runtime cardinality bound.
	MetricsMaxSeries int
	// MetricsRuntimeCollectors adds the Go and process collectors.
	MetricsRuntimeCollectors bool

	// OTelMetricsEnabled turns on the OpenTelemetry meter provider.
	//
	// It is off by default, and that is a considered decision rather than an oversight: the
	// `pp_*` metrics are Prometheus-native because client_golang gives native exemplar support
	// and a scrape endpoint that keeps working when the collector is down, and a push pipeline
	// for the same numbers would be a second thing to keep alive for no gain. The meter provider
	// exists for third-party instrumentation that only speaks OTel.
	OTelMetricsEnabled     bool
	OTelMetricExportPeriod time.Duration

	// ShutdownTimeout bounds the whole teardown.
	ShutdownTimeout time.Duration
}

// ConfigFromEnv reads Config from the process environment.
//
// Variable names are prefixed PP_ for platform settings and left as the OTel spec's own names
// where one exists (OTEL_EXPORTER_OTLP_ENDPOINT), so that the standard tooling and our own agree.
// The Kubernetes values come from the downward API, which is the only source that is correct in
// every environment — deriving the pod name from the hostname works until a sidecar or a job
// changes it.
func ConfigFromEnv() (Config, error) {
	c := Config{
		Service:      os.Getenv("PP_SERVICE"),
		Version:      envOr("PP_VERSION", "unknown"),
		Environment:  os.Getenv("PP_ENVIRONMENT"),
		Region:       os.Getenv("PP_REGION"),
		Plane:        Plane(envOr("PP_PLANE", string(PlaneData))),
		PodName:      os.Getenv("POD_NAME"),
		PodNamespace: os.Getenv("POD_NAMESPACE"),
		NodeName:     os.Getenv("NODE_NAME"),

		OTLPEndpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		OTLPHeaders:  parseHeaders(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS")),

		LogOutput: os.Stdout,

		MetricsMaxSeries:         DefaultMaxSeriesPerMetric,
		MetricsRuntimeCollectors: true,
		OTelMetricExportPeriod:   60 * time.Second,
		ShutdownTimeout:          10 * time.Second,
	}

	c.OTLPInsecure = envBool("OTEL_EXPORTER_OTLP_INSECURE", true)
	c.OTelMetricsEnabled = envBool("PP_OTEL_METRICS_ENABLED", false)

	if v := os.Getenv("PP_TRACE_SAMPLE_RATIO"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return Config{}, fmt.Errorf("telemetry: PP_TRACE_SAMPLE_RATIO %q: %w", v, err)
		}
		c.TraceSampleRatio = &f
	}
	if v := os.Getenv("PP_LOG_LEVEL"); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(strings.ToUpper(v))); err != nil {
			return Config{}, fmt.Errorf("telemetry: PP_LOG_LEVEL %q: %w", v, err)
		}
	}
	if v := os.Getenv("PP_LOG_SAMPLE_PER_SECOND"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Config{}, fmt.Errorf("telemetry: PP_LOG_SAMPLE_PER_SECOND %q must be a non-negative integer", v)
		}
		c.LogSamplePerSecond = n
	}
	if v := os.Getenv("PP_METRICS_MAX_SERIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("telemetry: PP_METRICS_MAX_SERIES %q must be a positive integer", v)
		}
		c.MetricsMaxSeries = n
	}
	if v := os.Getenv("PP_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("telemetry: PP_SHUTDOWN_TIMEOUT %q must be a positive duration", v)
		}
		c.ShutdownTimeout = d
	}

	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate rejects a configuration that would produce mislabelled or unbounded telemetry.
//
// It fails at startup rather than degrading, because every failure mode here is invisible at
// runtime: a service with an empty service.name does not crash, it just contaminates a shared
// dashboard for a week before someone notices.
func (c *Config) Validate() error {
	var problems []string
	if c.Service == "" {
		problems = append(problems, "PP_SERVICE is required (becomes service.name and the `service` label)")
	}
	if c.Environment == "" {
		problems = append(problems, "PP_ENVIRONMENT is required (dev|staging|prod)")
	}
	if c.Region == "" {
		problems = append(problems, "PP_REGION is required (becomes cloud.region)")
	}
	switch c.Plane {
	case PlaneData, PlaneControl, PlaneAutomation, PlaneObservability:
	default:
		problems = append(problems, fmt.Sprintf("PP_PLANE %q is not one of data|control|automation|observability", c.Plane))
	}
	if c.TraceSampleRatio != nil && (*c.TraceSampleRatio < 0 || *c.TraceSampleRatio > 1) {
		problems = append(problems, "PP_TRACE_SAMPLE_RATIO must be between 0 and 1 inclusive")
	}
	if len(problems) > 0 {
		return fmt.Errorf("telemetry: invalid configuration: %s", strings.Join(problems, "; "))
	}
	return nil
}

// sampleRatio resolves the head sampling ratio, applying the plane defaults from
// observability.md §2.3 when the operator has not set one.
func (c *Config) sampleRatio() float64 {
	if c.TraceSampleRatio != nil {
		return *c.TraceSampleRatio
	}
	if c.Environment != "prod" {
		// Outside production the volume is trivial and the cost of a missing trace is a wasted
		// afternoon, so sample everything.
		return 1.0
	}
	switch c.Plane {
	case PlaneData:
		return 0.10
	case PlaneControl:
		return 0.25
	default:
		// Automation and observability planes: low volume, long-lived, customer-visible when they
		// fail, and someone asks about a specific onboarding a week later. Sample all of it.
		return 1.0
	}
}

// applyDefaults fills in the fields a hand-built Config (a test, a tool) is allowed to omit.
func (c *Config) applyDefaults() {
	if c.LogOutput == nil {
		c.LogOutput = os.Stdout
	}
	if c.Plane == "" {
		c.Plane = PlaneData
	}
	if c.Version == "" {
		c.Version = "unknown"
	}
	if c.MetricsMaxSeries <= 0 {
		c.MetricsMaxSeries = DefaultMaxSeriesPerMetric
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = 10 * time.Second
	}
	if c.OTelMetricExportPeriod <= 0 {
		c.OTelMetricExportPeriod = 60 * time.Second
	}
}

// Telemetry is everything a service needs to be observable, and the handle that stops it.
type Telemetry struct {
	// Logger is the base logger, already carrying service, version, environment, region and pod.
	// Request-path code should call Logger(ctx) instead, which adds the correlation spine.
	Logger *slog.Logger
	// Level allows the log level to be changed on a running process.
	Level *slog.LevelVar
	// Tracer is the platform instrumentation scope's tracer.
	Tracer trace.Tracer
	// Tracing owns the tracer provider and its export goroutines.
	Tracing *Tracing
	// Meter is the OpenTelemetry meter. Nil unless OTelMetricsEnabled; see Config.
	Meter otelmetric.Meter
	// Metrics is the Prometheus registry carrying every `pp_*` metric.
	Metrics *Registry
	// Config is the resolved configuration, kept so a /debug endpoint and the startup log line
	// can report what the process actually decided.
	Config Config

	shutdown []func(context.Context) error
}

// Setup builds the whole observability stack and installs the global OpenTelemetry state.
//
// The ordering matters and is not arbitrary. Metrics are built first, because the log handlers
// need the registry to count what they drop; the logger is built second, so that a tracing
// failure can be logged in the platform's own format; tracing last, because it is the only part
// that opens a socket and therefore the only part likely to fail.
//
// Everything that starts a goroutine registers a shutdown function as it is created, and a
// failure part-way through runs the ones registered so far before returning. Without that, a
// process that fails to start its meter provider leaks the tracer's batch-processor goroutines
// for as long as it takes the supervisor to notice — which, for a crash-looping pod, is a slow
// memory leak in the node's most constrained resource.
func Setup(ctx context.Context, cfg Config) (*Telemetry, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	t := &Telemetry{Config: cfg}
	fail := func(err error) (*Telemetry, error) {
		_ = t.Shutdown(context.WithoutCancel(ctx))
		return nil, err
	}

	// 1. Metrics.
	reg, err := NewRegistry(RegistryOptions{
		MaxSeriesPerMetric:       cfg.MetricsMaxSeries,
		IncludeRuntimeCollectors: cfg.MetricsRuntimeCollectors,
	})
	if err != nil {
		return nil, err
	}
	t.Metrics = reg

	// 2. Logging.
	level := new(slog.LevelVar)
	level.Set(cfg.LogLevel)
	t.Level = level
	t.Logger = NewLogger(cfg.LogOutput, LogOptions{
		Level:    level,
		Metrics:  reg,
		Sampling: SamplingOptions{PerSecond: cfg.LogSamplePerSecond},
		Base: []slog.Attr{
			slog.String(KeyService, cfg.Service),
			slog.String(KeyVersion, cfg.Version),
			slog.String(KeyEnvironment, cfg.Environment),
			slog.String(KeyRegion, cfg.Region),
			slog.String(KeyPod, cfg.PodName),
		},
	})
	SetBaseLogger(t.Logger)

	// 3. Resource, then tracing, then the optional meter.
	res, err := newResource(ctx, cfg)
	if err != nil {
		return fail(err)
	}

	tracing, err := NewTracing(ctx, TracingOptions{
		Endpoint:        cfg.OTLPEndpoint,
		Insecure:        cfg.OTLPInsecure,
		Headers:         cfg.OTLPHeaders,
		SampleRatio:     cfg.sampleRatio(),
		Resource:        res,
		ShutdownTimeout: cfg.ShutdownTimeout,
	})
	if err != nil {
		return fail(err)
	}
	t.Tracing = tracing
	t.Tracer = otel.Tracer(ScopeName)
	t.shutdown = append(t.shutdown, tracing.Shutdown)

	if cfg.OTelMetricsEnabled && cfg.OTLPEndpoint != "" {
		mp, err := newMeterProvider(ctx, cfg, res)
		if err != nil {
			return fail(err)
		}
		otel.SetMeterProvider(mp)
		t.Meter = mp.Meter(ScopeName)
		t.shutdown = append(t.shutdown, func(ctx context.Context) error { return mp.Shutdown(ctx) })
	}

	// otel's internal errors (a failed export, a dropped batch) go to a global handler that
	// defaults to writing unstructured lines to stderr — outside the log schema, unparseable by
	// the pipeline, and invisible to every alert. Route them into our own logger instead.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		t.Logger.Warn("opentelemetry sdk error", slog.String(KeyErrorMessage, err.Error()))
	}))

	t.Logger.Info("telemetry initialised",
		slog.String(KeyOutcome, "success"),
		slog.String(KeyReason, string(cfg.Plane)),
	)
	return t, nil
}

// Shutdown flushes and stops every background component, in reverse order of construction, under
// one bounded deadline.
//
// It is safe to call on a partially constructed Telemetry — that is what makes the rollback path
// in Setup work — and it returns every error rather than the first, because "the tracer flushed
// but the meter did not" is a different operational fact from "neither did".
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	timeout := t.Config.ShutdownTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var errs []error
	for i := len(t.shutdown) - 1; i >= 0; i-- {
		if err := t.shutdown[i](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	t.shutdown = nil
	return errors.Join(errs...)
}

// MetricsHandler is the /metrics endpoint. It is served on the admin port, never the public one:
// the metric surface names every gateway, every route and every tenant tier this service knows
// about, which is reconnaissance for anyone who can reach it.
func (t *Telemetry) MetricsHandler() http.Handler { return t.Metrics.Handler() }

// SetLevel changes the minimum log level on a running process. It exists for the audited,
// auto-reverting `platformctl log-level` command: turning on DEBUG by restarting a pod destroys
// the state you turned DEBUG on to look at.
func (t *Telemetry) SetLevel(l slog.Level) {
	if t.Level != nil {
		t.Level.Set(l)
	}
}

// newResource assembles the resource attributes that every dashboard variable joins on.
//
// resource.WithFromEnv is included so OTEL_RESOURCE_ATTRIBUTES set by the deployment can add
// attributes without a code change; the explicit attributes are applied after it and win, because
// a service that can be told it is a different service by an environment variable is a service
// whose telemetry cannot be trusted.
func newResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(cfg.Service),
		semconv.ServiceVersion(cfg.Version),
		semconv.ServiceNamespace("payments-platform"),
		semconv.DeploymentEnvironment(cfg.Environment),
		semconv.CloudRegion(cfg.Region),
		attribute.String(AttrPlane, string(cfg.Plane)),
	}
	if cfg.PodName != "" {
		attrs = append(attrs, semconv.K8SPodName(cfg.PodName))
	}
	if cfg.PodNamespace != "" {
		attrs = append(attrs, semconv.K8SNamespaceName(cfg.PodNamespace))
	}
	if cfg.NodeName != "" {
		attrs = append(attrs, semconv.K8SNodeName(cfg.NodeName))
	}

	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(attrs...),
	)
	// resource.New returns a usable resource alongside a partial-detection error. Losing the host
	// detector is not a reason to refuse to start; losing the service name is, and that one is
	// set explicitly above.
	if err != nil && res == nil {
		return nil, fmt.Errorf("telemetry: resource: %w", err)
	}
	return res, nil
}

// newMeterProvider builds the OTLP meter provider for third-party instrumentation. The periodic
// reader owns one goroutine, stopped by the provider's Shutdown.
func newMeterProvider(ctx context.Context, cfg Config, res *resource.Resource) (*sdkmetric.MeterProvider, error) {
	opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint)}
	if cfg.OTLPInsecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	if len(cfg.OTLPHeaders) > 0 {
		opts = append(opts, otlpmetricgrpc.WithHeaders(cfg.OTLPHeaders))
	}
	exp, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: otlp metric exporter: %w", err)
	}
	return sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp,
			sdkmetric.WithInterval(cfg.OTelMetricExportPeriod))),
	), nil
}

// --- environment helpers ------------------------------------------------------------------------

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// parseHeaders reads the OTel spec's `key=value,key=value` header encoding.
func parseHeaders(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
