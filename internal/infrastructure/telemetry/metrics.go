package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/trace"
)

// The metric registry is the single place a `pp_*` metric may be declared (observability.md
// §3.1); `scripts/metrics-lint.sh` fails the build if one appears anywhere else. Two properties
// follow from that and neither is achievable with metrics scattered across packages: the
// cardinality lint can see every label set before a series exists, and every metric can be
// checked against the dashboards, rules and runbooks that are supposed to read it.

// Metric names, exported so that alert rules, dashboard generators and the conformance test
// reference one definition rather than five string literals that drift apart.
const (
	MetricHTTPRequestsTotal        = "pp_http_requests_total"
	MetricHTTPRequestDuration      = "pp_http_request_duration_seconds"
	MetricPaymentsTotal            = "pp_payments_total"
	MetricPaymentAuthorizationRate = "pp_payment_authorization_rate"
	MetricGatewayRequestDuration   = "pp_gateway_request_duration_seconds"
	MetricGatewayErrorsTotal       = "pp_gateway_errors_total"
	MetricCircuitBreakerState      = "pp_circuit_breaker_state"
	MetricIdempotencyOutcomesTotal = "pp_idempotency_outcomes_total"
	MetricRoutingDecisionsTotal    = "pp_routing_decisions_total"
	MetricWorkflowStepDuration     = "pp_workflow_step_duration_seconds"
	MetricWorkflowInstances        = "pp_workflow_instances"
	MetricOnboardingDuration       = "pp_onboarding_duration_seconds"
	MetricOutboxBacklog            = "pp_outbox_backlog"
	MetricConsumerLag              = "pp_consumer_lag"
	MetricConfigSnapshotAge        = "pp_config_snapshot_age_seconds"
	MetricReconciliationExceptions = "pp_reconciliation_exceptions"
	MetricDLQDepth                 = "pp_dlq_depth"

	// Self-observability. These are not part of the baseline §22.2 contract; they exist so that
	// the three ways this package can silently lose data — a dropped log field, a suppressed log
	// line, a folded metric series — are visible as numbers instead of as an absence.
	MetricLogFieldRejectedTotal   = "pp_log_field_rejected_total"
	MetricLogLinesSuppressedTotal = "pp_log_lines_suppressed_total"
	MetricSeriesOverflowTotal     = "pp_metric_series_overflow_total"
)

// OverflowLabelValue replaces every label value of a series that exceeds a metric's cardinality
// budget. It is a deliberately ugly, greppable token: seeing it on a dashboard should read as
// "this metric is broken", not as a plausible dimension value.
const OverflowLabelValue = "__overflow__"

// DefaultMaxSeriesPerMetric is the budget from baseline §22.3: 10⁴ active series per metric per
// service. The static lint checks the declared enums against this number; the runtime guard
// below is the backstop for a value the code invents at runtime, which a lint cannot see.
const DefaultMaxSeriesPerMetric = 10_000

// --- closed label enums ---------------------------------------------------------------------
//
// Every label whose values are not obviously bounded gets a Go type here. The point is not
// tidiness: a `string` parameter is an invitation for a call site to pass `err.Error()` or a
// customer reference, and one such call site is a 100× cardinality incident. A typed enum makes
// the wrong value fail to compile.

// TenantTier is the only tenant dimension permitted on a metric (baseline §22.3). tenant_id
// itself lives in logs, spans and exemplars.
type TenantTier string

const (
	TierPooled     TenantTier = "pooled"
	TierSiloed     TenantTier = "siloed"
	TierEnterprise TenantTier = "enterprise"
)

// StatusClass is the HTTP status *class*, not the code. Three values instead of forty, and no
// alert has ever needed to distinguish 502 from 504 at the metric layer — the error model
// already tells the client what to do and the exact code is one exemplar hop away
// (observability.md §3.2).
type StatusClass string

const (
	Status2xx StatusClass = "2xx"
	Status3xx StatusClass = "3xx"
	Status4xx StatusClass = "4xx"
	Status5xx StatusClass = "5xx"
)

// StatusClassOf collapses a status code to its class so that call sites never format one
// themselves and never accidentally pass the code through.
func StatusClassOf(code int) StatusClass {
	switch {
	case code >= 200 && code < 300:
		return Status2xx
	case code >= 300 && code < 400:
		return Status3xx
	case code >= 400 && code < 500:
		return Status4xx
	default:
		return Status5xx
	}
}

// PaymentOutcome is the business result dimension of pp_payments_total. The set mirrors the
// payment state machine's terminal and near-terminal outcomes (baseline §9.1) so that a panel
// built on this metric and a panel built on state transitions cannot disagree.
type PaymentOutcome string

const (
	OutcomeCreated       PaymentOutcome = "created"
	OutcomeAuthorized    PaymentOutcome = "authorized"
	OutcomeCaptured      PaymentOutcome = "captured"
	OutcomeDeclined      PaymentOutcome = "declined"
	OutcomeFailed        PaymentOutcome = "failed"
	OutcomeVoided        PaymentOutcome = "voided"
	OutcomeRefunded      PaymentOutcome = "refunded"
	OutcomeTimeoutUnknwn PaymentOutcome = "timeout_unknown"
)

// GatewayErrorClass answers "why is this gateway failing", which is a different question from
// "how often". The classes are the ones the gateway health FSM (baseline §10) branches on.
type GatewayErrorClass string

const (
	GatewayErrTimeout      GatewayErrorClass = "timeout"
	GatewayErrTransport    GatewayErrorClass = "transport"
	GatewayErrHTTP5xx      GatewayErrorClass = "http_5xx"
	GatewayErrHTTP4xx      GatewayErrorClass = "http_4xx"
	GatewayErrContractViol GatewayErrorClass = "contract_violation"
	GatewayErrAuth         GatewayErrorClass = "auth"
	GatewayErrRateLimited  GatewayErrorClass = "rate_limited"
)

// IdempotencyOutcome distinguishes correct client retries from client bugs. A `conflict` spike
// is IDEMPOTENCY_KEY_REUSED — the same key with a different body — and an `in_progress` spike is
// a retry storm; the two need different responses, so they are different label values rather
// than one "duplicate" bucket (baseline §14).
type IdempotencyOutcome string

const (
	IdempotencyNew        IdempotencyOutcome = "new"
	IdempotencyReplay     IdempotencyOutcome = "replay"
	IdempotencyInProgress IdempotencyOutcome = "in_progress"
	IdempotencyConflict   IdempotencyOutcome = "conflict"
)

// RoutingReason explains why traffic moved. Without it, a gateway share shift is indistinguishable
// from a health event, a merchant pin and a cost-policy change.
type RoutingReason string

const (
	RoutingPrimary        RoutingReason = "primary"
	RoutingFallbackHealth RoutingReason = "fallback_health"
	RoutingFallbackError  RoutingReason = "fallback_error"
	RoutingPinned         RoutingReason = "pinned"
	RoutingCapability     RoutingReason = "capability"
	RoutingCost           RoutingReason = "cost"
	RoutingResidency      RoutingReason = "residency"
	RoutingNoEligible     RoutingReason = "no_eligible"
)

// CircuitState is exported as an enum gauge (0/1/2) rather than three boolean gauges because a
// breaker is in exactly one state and a panel wants one line per (gateway, operation), not three
// that must be read together.
type CircuitState float64

const (
	CircuitClosed   CircuitState = 0 // HEALTHY — traffic flows
	CircuitHalfOpen CircuitState = 1 // PROBING — a trickle of probes
	CircuitOpen     CircuitState = 2 // UNHEALTHY — traffic is being shed
)

// StepOutcome is the third dimension of the workflow-step histogram: a step that took 30 s and
// succeeded and a step that took 30 s and then timed out are not the same measurement, and
// averaging them together hides the failure.
type StepOutcome string

const (
	StepSuccess StepOutcome = "success"
	StepFailed  StepOutcome = "failed"
	StepTimeout StepOutcome = "timeout"
	StepSkipped StepOutcome = "skipped"
	StepWaiting StepOutcome = "waiting"
)

// --- histogram buckets ----------------------------------------------------------------------
//
// prometheus.DefBuckets is wrong for every metric in this file. Its top bucket is 10 s, its
// bottom is 5 ms, and it has nothing near any threshold this platform is judged on — so the
// histogram_quantile interpolation is at its least accurate exactly at the number in the SLO.
// Buckets are an SLO decision, not a default.

// httpLatencyBuckets brackets the two numbers in baseline §18: p50 ≤ 60 ms and p99 ≤ 250 ms,
// excluding gateway time. `.06` and `.25` are bucket *boundaries*, which makes the quantile
// estimate exact at the thresholds rather than interpolated across a bucket that straddles them.
// The tail out to 10 s exists to make a pathological request visible rather than clipped.
var httpLatencyBuckets = []float64{.005, .01, .025, .05, .06, .1, .25, .5, 1, 2.5, 5, 10}

// gatewayLatencyBuckets is shaped by the gateway, not by us. `5` is the p99 threshold at which
// the gateway health FSM declares UNHEALTHY (baseline §10) and `8` is the hard call timeout
// (§12), so both transitions land on a boundary. The low end starts at 50 ms because no external
// gateway answers faster than that and buckets below it would be dead weight in every scrape.
var gatewayLatencyBuckets = []float64{.05, .1, .25, .5, 1, 2, 3, 5, 8}

// workflowStepBuckets spans five orders of magnitude because onboarding steps genuinely do: a
// config write is milliseconds, a certification suite is minutes, an external KYC callback is
// longer. The boundaries mirror the §11 per-step timeouts so that "did this step blow its
// budget" is a bucket comparison rather than a quantile guess.
var workflowStepBuckets = []float64{.1, .5, 1, 5, 30, 60, 300, 900, 1800}

// onboardingBuckets is anchored on the §18 target: the automated portion of onboarding at
// p95 ≤ 30 min. 1800 is therefore a boundary. Below it the buckets are coarse, because nobody
// asks whether onboarding took 4 or 6 minutes; above it they run to two hours so that a stuck
// run is a visible number instead of +Inf.
var onboardingBuckets = []float64{30, 60, 120, 300, 600, 900, 1800, 3600, 7200}

// --- cardinality guard ----------------------------------------------------------------------

// forbiddenLabelNames is the registration-time half of baseline §22.3. Each entry is a label
// that is unbounded by construction, and the map value is the reason — kept next to the rule
// because "why can't I add merchant_id" is asked by every new engineer, and the answer belongs
// in the code that says no.
// Keys are written in canonical snake_case, but matching is neither case- nor
// separator-sensitive: see normalizeLabelName. `merchantId` and `merchant_id` are the same
// label wearing different clothes, and a guard that rejects one and accepts the other is a
// speed bump rather than a rule — the author who was told no simply renames and merges.
var forbiddenLabelNames = map[string]string{
	// Baseline §22.3 names these six explicitly.
	"merchant_id":     "unbounded in the tenant dimension; belongs in logs, spans and exemplars (§22.3)",
	"payment_id":      "one series per payment; belongs in logs, spans and exemplars (§22.3)",
	"attempt_id":      "one series per attempt, strictly worse than payment_id",
	"idempotency_key": "client-chosen and unbounded; log the SHA-256 prefix instead",
	"email":           "personal data, and unbounded",
	"ip":              "personal data under GDPR, and unbounded",

	"user_agent":    "attacker-controlled and effectively unbounded",
	"url":           "use the route template, never the concrete path",
	"path":          "use the route template, never the concrete path",
	"error_message": "free text; use the error code, which is a closed set (§20.2)",

	// Correlation identifiers. Each of these is unique per request by construction, so a
	// single one of them turns every metric into a per-request series. They exist precisely
	// so that a metric can stay low-cardinality and still be joined to a trace: the join key
	// belongs in the exemplar, the span and the log line, never in the label set.
	"trace_id":       "unique per request; carry it in the exemplar and the span, never in a label",
	"span_id":        "unique per span; strictly worse than trace_id",
	"correlation_id": "unique per request chain; belongs in logs and baggage",
	"request_id":     "unique per request; belongs in logs and the X-Request-Id header",

	// Subject identifiers. Unbounded in the customer dimension and personal data besides.
	"user_id":     "unbounded in the user dimension, and personal data",
	"session_id":  "unbounded and personal data; a session is a request chain, not a dimension",
	"customer_id": "unbounded in the customer dimension; belongs in logs, spans and exemplars",

	// Network locators. Personal data under GDPR and unbounded in practice (§22.3 names `ip`;
	// these are the spellings people reach for once `ip` is refused).
	"client_ip":   "personal data under GDPR, and unbounded; same rule as `ip`",
	"remote_addr": "personal data under GDPR, and unbounded; same rule as `ip`",

	// Cardholder data. Unbounded is the least of the problems: a PAN in a label set is a
	// PCI-scope violation in a store that is scraped, replicated and retained for a year.
	"pan":           "cardholder data; never leaves the gateway edge, and never enters a metric",
	"card_number":   "cardholder data; never leaves the gateway edge, and never enters a metric",
	"email_address": "personal data, and unbounded; same rule as `email`",
}

// normalizeLabelName folds a label name to the form the forbidden-label check compares on:
// lower-cased with `_`, `-` and `.` removed. This is what makes `merchantId`, `merchant_id`,
// `MerchantID` and `merchant-id` one name rather than four, on both sides of the check — the
// pull-request lint (scripts/check-metrics-cardinality.sh) probes camelCase spellings, and a
// runtime guard that only knew the snake_case ones would let exactly those through.
func normalizeLabelName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '_' || r == '-' || r == '.':
			// separator: dropped
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r - 'A' + 'a')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// forbiddenLabelIndex is forbiddenLabelNames keyed by normalized name. Built once at init so
// ValidateLabels stays a map lookup on the registration path.
var forbiddenLabelIndex = func() map[string]string {
	m := make(map[string]string, len(forbiddenLabelNames))
	for name, reason := range forbiddenLabelNames {
		m[normalizeLabelName(name)] = reason
	}
	return m
}()

// ErrForbiddenLabel is returned by the registry constructor when a declaration violates §22.3.
// It is an error rather than a panic because the check has a test that must be able to observe
// it; the composition root turns it into a startup failure, which is the right blast radius —
// a cardinality bug that reaches production costs more than a pod that will not start.
type ErrForbiddenLabel struct {
	Metric string
	Label  string
	Reason string
}

func (e *ErrForbiddenLabel) Error() string {
	return fmt.Sprintf("telemetry: metric %q declares forbidden label %q: %s", e.Metric, e.Label, e.Reason)
}

// ValidateLabels enforces the forbidden-label rule for one metric declaration. Exported so the
// cardinality lint and the conformance test can run it over a candidate declaration without
// building a whole registry.
func ValidateLabels(metric string, labels []string) error {
	for _, l := range labels {
		if reason, bad := forbiddenLabelIndex[normalizeLabelName(l)]; bad {
			return &ErrForbiddenLabel{Metric: metric, Label: l, Reason: reason}
		}
	}
	return nil
}

// seriesGuard bounds the number of distinct label tuples one metric may create at runtime.
//
// The three available behaviours on overflow are: grow without limit (the classic incident —
// the scrape gets slower, then the remote-write queue backs up, then the whole tenant's metrics
// stop), drop silently (the metric quietly becomes a lie and nobody knows when), or fold. This
// folds: every label of an over-budget series becomes OverflowLabelValue, so the observations
// are still counted, the total is still correct, and the dimension is visibly destroyed. A
// dedicated counter records how often it happened, per metric, so the folding is an alert rather
// than an archaeological discovery.
type seriesGuard struct {
	metric   string
	max      int
	overflow prometheus.Counter

	mu   sync.Mutex
	seen map[string]struct{}
}

func newSeriesGuard(metric string, maxSeries int, overflow *prometheus.CounterVec) *seriesGuard {
	return &seriesGuard{
		metric:   metric,
		max:      maxSeries,
		overflow: overflow.WithLabelValues(metric),
		seen:     make(map[string]struct{}),
	}
}

// bound returns the label values to use: the originals while the metric is inside its budget,
// or an all-overflow tuple once it is not. The lock is held for a map lookup on a joined string;
// at 5 000 TPS with a handful of recorder calls per request this is nanoseconds against a metric
// write that is already atomic-contended, and it buys a hard bound on process memory.
func (g *seriesGuard) bound(vals []string) []string {
	key := strings.Join(vals, "\x1f")

	g.mu.Lock()
	if _, ok := g.seen[key]; ok {
		g.mu.Unlock()
		return vals
	}
	if len(g.seen) >= g.max {
		g.mu.Unlock()
		g.overflow.Inc()
		out := make([]string, len(vals))
		for i := range out {
			out[i] = OverflowLabelValue
		}
		return out
	}
	g.seen[key] = struct{}{}
	g.mu.Unlock()
	return vals
}

// --- registry -------------------------------------------------------------------------------

// RegistryOptions configures a Registry. Every field has a working default so that a caller who
// does not care gets the production behaviour rather than a broken one.
type RegistryOptions struct {
	// MaxSeriesPerMetric bounds distinct label tuples per metric. Zero means
	// DefaultMaxSeriesPerMetric.
	MaxSeriesPerMetric int
	// IncludeRuntimeCollectors adds the Go runtime and process collectors. On by default; the
	// only reason to turn it off is a test that wants a deterministic /metrics body.
	IncludeRuntimeCollectors bool
}

// Registry owns every `pp_*` metric this process exposes.
//
// It wraps a private *prometheus.Registry rather than using the default one, because the default
// registry is global mutable state that any imported library can write to: a transitive
// dependency registering its own `http_requests_total` would collide with ours at scrape time
// and take the whole endpoint down with it. A private registry makes the metric surface a
// property of the composition root.
//
// Every recorder method takes typed arguments and every label value is either an enum or an
// identifier from a bounded set, which is what makes "call sites cannot invent a label" true
// rather than aspirational.
type Registry struct {
	reg       *prometheus.Registry
	maxSeries int

	httpRequests   *prometheus.CounterVec
	httpDuration   *prometheus.HistogramVec
	payments       *prometheus.CounterVec
	authRate       *prometheus.GaugeVec
	gatewayLatency *prometheus.HistogramVec
	gatewayErrors  *prometheus.CounterVec
	circuitState   *prometheus.GaugeVec
	idempotency    *prometheus.CounterVec
	routing        *prometheus.CounterVec
	workflowStep   *prometheus.HistogramVec
	workflowInst   *prometheus.GaugeVec
	onboarding     *prometheus.HistogramVec
	outboxBacklog  *prometheus.GaugeVec
	consumerLag    *prometheus.GaugeVec
	configAge      *prometheus.GaugeVec
	reconExcept    *prometheus.GaugeVec
	dlqDepth       *prometheus.GaugeVec

	logFieldRejected *prometheus.CounterVec
	logSuppressed    *prometheus.CounterVec
	seriesOverflow   *prometheus.CounterVec

	guards map[string]*seriesGuard
}

// NewRegistry declares every metric in the baseline §22.2 contract and returns a registry ready
// to be scraped. It returns an error rather than panicking so that the cardinality guard is
// testable and so that a composition root can report a startup failure through its normal path.
func NewRegistry(opts RegistryOptions) (*Registry, error) {
	maxSeries := opts.MaxSeriesPerMetric
	if maxSeries <= 0 {
		maxSeries = DefaultMaxSeriesPerMetric
	}
	r := &Registry{
		reg:       prometheus.NewRegistry(),
		maxSeries: maxSeries,
		guards:    make(map[string]*seriesGuard),
	}

	// Declared first, because every other guard needs it.
	r.seriesOverflow = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: MetricSeriesOverflowTotal,
		Help: "Observations folded into " + OverflowLabelValue + " because the metric exceeded its cardinality budget.",
	}, []string{"metric"})
	if err := r.reg.Register(r.seriesOverflow); err != nil {
		return nil, fmt.Errorf("telemetry: register %s: %w", MetricSeriesOverflowTotal, err)
	}

	var err error
	counter := func(name, help string, labels []string) *prometheus.CounterVec {
		if err != nil {
			return nil
		}
		if err = r.declare(name, labels); err != nil {
			return nil
		}
		c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
		err = r.reg.Register(c)
		return c
	}
	gauge := func(name, help string, labels []string) *prometheus.GaugeVec {
		if err != nil {
			return nil
		}
		if err = r.declare(name, labels); err != nil {
			return nil
		}
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
		err = r.reg.Register(g)
		return g
	}
	histogram := func(name, help string, labels []string, buckets []float64) *prometheus.HistogramVec {
		if err != nil {
			return nil
		}
		if err = r.declare(name, labels); err != nil {
			return nil
		}
		h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, labels)
		err = r.reg.Register(h)
		return h
	}

	// RED — the request/error/duration triad for every HTTP surface.
	r.httpRequests = counter(MetricHTTPRequestsTotal,
		"HTTP requests served, by route template and status class. Numerator and denominator of the availability SLI.",
		[]string{"service", "route", "method", "status", "tenant_tier"})
	r.httpDuration = histogram(MetricHTTPRequestDuration,
		"HTTP server-side latency in seconds, excluding gateway time.",
		[]string{"service", "route", "method"}, httpLatencyBuckets)

	// Business volume and mix.
	r.payments = counter(MetricPaymentsTotal,
		"Payments by terminal outcome, currency, method, gateway and tenant tier.",
		[]string{"outcome", "currency", "payment_method", "gateway", "tenant_tier"})
	r.authRate = gauge(MetricPaymentAuthorizationRate,
		"Authorization success ratio 0-1. Normally produced by a Prometheus recording rule; declared here so the registry remains the single enumerable source of truth for the §22.2 contract.",
		[]string{"gateway", "currency"})

	// Gateway health.
	r.gatewayLatency = histogram(MetricGatewayRequestDuration,
		"Latency in seconds of one call to an external payment gateway.",
		[]string{"gateway", "operation"}, gatewayLatencyBuckets)
	r.gatewayErrors = counter(MetricGatewayErrorsTotal,
		"Gateway call failures by class. Drives the gateway health state machine.",
		[]string{"gateway", "operation", "class"})
	r.circuitState = gauge(MetricCircuitBreakerState,
		"Circuit breaker state: 0=CLOSED(HEALTHY) 1=HALF_OPEN(PROBING) 2=OPEN(UNHEALTHY).",
		[]string{"gateway", "operation"})

	// Request-pipeline behaviour.
	r.idempotency = counter(MetricIdempotencyOutcomesTotal,
		"Idempotency claim outcomes: new, replay, in_progress, conflict.",
		[]string{"outcome"})
	r.routing = counter(MetricRoutingDecisionsTotal,
		"Routing decisions by selected gateway and the reason it was selected.",
		[]string{"gateway", "reason"})

	// Automation plane.
	r.workflowStep = histogram(MetricWorkflowStepDuration,
		"Duration in seconds of one workflow step, by outcome.",
		[]string{"workflow", "step", "outcome"}, workflowStepBuckets)
	r.workflowInst = gauge(MetricWorkflowInstances,
		"Workflow instances currently in each state.",
		[]string{"workflow", "state"})
	r.onboarding = histogram(MetricOnboardingDuration,
		"End-to-end duration in seconds of the automated portion of onboarding.",
		[]string{"outcome"}, onboardingBuckets)

	// Data movement and staleness.
	r.outboxBacklog = gauge(MetricOutboxBacklog,
		"Undispatched rows in the transactional outbox, by destination topic.",
		[]string{"topic"})
	r.consumerLag = gauge(MetricConsumerLag,
		"Consumer group lag in messages.",
		[]string{"topic", "group"})
	r.configAge = gauge(MetricConfigSnapshotAge,
		"Age in seconds of the locally cached configuration snapshot.",
		[]string{"service"})
	r.reconExcept = gauge(MetricReconciliationExceptions,
		"Unresolved reconciliation exceptions by severity.",
		[]string{"severity"})
	r.dlqDepth = gauge(MetricDLQDepth,
		"Messages parked in a dead-letter queue awaiting human action.",
		[]string{"queue"})

	// Self-observability.
	r.logFieldRejected = counter(MetricLogFieldRejectedTotal,
		"Log attributes dropped because the key is not on the serializer allowlist.",
		[]string{"field"})
	r.logSuppressed = counter(MetricLogLinesSuppressedTotal,
		"Log lines suppressed by the high-volume sampler.",
		[]string{"level"})

	if err != nil {
		return nil, err
	}

	if opts.IncludeRuntimeCollectors {
		if err := r.reg.Register(collectors.NewGoCollector()); err != nil {
			return nil, fmt.Errorf("telemetry: register go collector: %w", err)
		}
		if err := r.reg.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
			return nil, fmt.Errorf("telemetry: register process collector: %w", err)
		}
	}
	return r, nil
}

// declare runs the registration-time cardinality check and installs the runtime guard.
func (r *Registry) declare(name string, labels []string) error {
	if err := ValidateLabels(name, labels); err != nil {
		return err
	}
	r.guards[name] = newSeriesGuard(name, r.maxSeries, r.seriesOverflow)
	return nil
}

// Prometheus exposes the underlying registry for the few things that legitimately need it: the
// conformance test, the cardinality integration test, and a push gateway in the load harness.
func (r *Registry) Prometheus() *prometheus.Registry { return r.reg }

// Handler returns the /metrics handler.
//
// OpenMetrics negotiation is enabled because it is the only exposition format that carries
// exemplars, and exemplars are what turn "p99 is 2 s" into "here is the trace of a request that
// took 2 s" (observability.md §1.4). Errors during a scrape are logged by the caller's handler
// rather than served as a 500, so one broken collector cannot blind every other metric.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
		ErrorHandling:     promhttp.ContinueOnError,
		// A scrape that hangs is worse than a scrape that fails: Prometheus will retry, but a
		// hung handler holds a goroutine and a connection until the scrape timeout.
		Timeout: 10 * time.Second,
	})
}

// --- recorders ------------------------------------------------------------------------------

// ObserveHTTPRequest records one served HTTP request against both RED metrics.
//
// route must be the OpenAPI route template ("/v1/payments/{paymentId}/capture"), never the
// concrete path: a concrete path makes the label unbounded and is a lint failure. The status
// code is collapsed to its class here so that no call site has the opportunity to pass the code.
func (r *Registry) ObserveHTTPRequest(ctx context.Context, service, route, method string, status int, tier TenantTier, d time.Duration) {
	r.httpRequests.WithLabelValues(
		r.bound(MetricHTTPRequestsTotal, service, route, method, string(StatusClassOf(status)), string(tier))...,
	).Inc()
	observe(ctx, r.httpDuration, r.bound(MetricHTTPRequestDuration, service, route, method), d.Seconds())
}

// RecordPaymentOutcome records one payment reaching an outcome. This is the business metric the
// executive dashboard and the authorization-rate SLI are built from, which is why the label set
// is mix (currency, method, gateway, tier) and not identity.
func (r *Registry) RecordPaymentOutcome(outcome PaymentOutcome, currency, paymentMethod, gateway string, tier TenantTier) {
	r.payments.WithLabelValues(
		r.bound(MetricPaymentsTotal, string(outcome), currency, paymentMethod, gateway, string(tier))...,
	).Inc()
}

// SetPaymentAuthorizationRate sets the derived authorization ratio.
//
// In production this series is produced by a Prometheus recording rule from pp_payments_total,
// because a ratio computed in-process is wrong the moment a pod restarts or a scrape is missed.
// The setter exists for the offline backfill and the gateway simulator, and calling it from a
// service is a review finding.
func (r *Registry) SetPaymentAuthorizationRate(gateway, currency string, ratio float64) {
	r.authRate.WithLabelValues(r.bound(MetricPaymentAuthorizationRate, gateway, currency)...).Set(ratio)
}

// ObserveGatewayRequest records the latency of one external gateway call. Carries an exemplar,
// because the first question after "gateway p99 is up" is always "show me a slow one".
func (r *Registry) ObserveGatewayRequest(ctx context.Context, gateway, operation string, d time.Duration) {
	observe(ctx, r.gatewayLatency, r.bound(MetricGatewayRequestDuration, gateway, operation), d.Seconds())
}

// RecordGatewayError records one failed gateway call, classified. The class, not the count, is
// what selects the runbook branch: a timeout storm and a 4xx storm have nothing in common.
func (r *Registry) RecordGatewayError(gateway, operation string, class GatewayErrorClass) {
	r.gatewayErrors.WithLabelValues(r.bound(MetricGatewayErrorsTotal, gateway, operation, string(class))...).Inc()
}

// SetCircuitState publishes the current breaker state. It is a gauge set on every transition
// rather than a counter of transitions, because the question this answers is "is traffic being
// shed right now", which a counter cannot answer.
func (r *Registry) SetCircuitState(gateway, operation string, state CircuitState) {
	r.circuitState.WithLabelValues(r.bound(MetricCircuitBreakerState, gateway, operation)...).Set(float64(state))
}

// RecordIdempotencyOutcome records the result of one idempotency claim.
func (r *Registry) RecordIdempotencyOutcome(outcome IdempotencyOutcome) {
	r.idempotency.WithLabelValues(r.bound(MetricIdempotencyOutcomesTotal, string(outcome))...).Inc()
}

// RecordRoutingDecision records which gateway was chosen and why.
func (r *Registry) RecordRoutingDecision(gateway string, reason RoutingReason) {
	r.routing.WithLabelValues(r.bound(MetricRoutingDecisionsTotal, gateway, string(reason))...).Inc()
}

// ObserveWorkflowStep records one workflow step completing, with its outcome.
func (r *Registry) ObserveWorkflowStep(ctx context.Context, workflow, step string, outcome StepOutcome, d time.Duration) {
	observe(ctx, r.workflowStep, r.bound(MetricWorkflowStepDuration, workflow, step, string(outcome)), d.Seconds())
}

// SetWorkflowInstances publishes the number of workflow instances in a state. The workflow
// engine recomputes and sets this on a schedule; it is a gauge because instances leave states
// without an event the metric layer sees.
func (r *Registry) SetWorkflowInstances(workflow, state string, n float64) {
	r.workflowInst.WithLabelValues(r.bound(MetricWorkflowInstances, workflow, state)...).Set(n)
}

// ObserveOnboarding records one completed onboarding run. Time parked in external KYC and manual
// gates is excluded by the caller, so the SLO measures the part of the process this platform
// controls (observability.md §3.1).
func (r *Registry) ObserveOnboarding(outcome string, d time.Duration) {
	r.onboarding.WithLabelValues(r.bound(MetricOnboardingDuration, outcome)...).Observe(d.Seconds())
}

// SetOutboxBacklog publishes undispatched outbox rows per topic. Rising and non-zero is the
// earliest signal that Kafka is unhappy or the relay has stalled (baseline §24).
func (r *Registry) SetOutboxBacklog(topic string, rows float64) {
	r.outboxBacklog.WithLabelValues(r.bound(MetricOutboxBacklog, topic)...).Set(rows)
}

// SetConsumerLag publishes consumer group lag in messages.
func (r *Registry) SetConsumerLag(topic, group string, messages float64) {
	r.consumerLag.WithLabelValues(r.bound(MetricConsumerLag, topic, group)...).Set(messages)
}

// SetConfigSnapshotAge publishes how stale the local configuration snapshot is. The data plane
// serves from a cached snapshot and refuses to serve past max_config_staleness, so this gauge is
// the difference between a soft alert and a hard cliff.
func (r *Registry) SetConfigSnapshotAge(service string, age time.Duration) {
	r.configAge.WithLabelValues(r.bound(MetricConfigSnapshotAge, service)...).Set(age.Seconds())
}

// SetReconciliationExceptions publishes unresolved money ambiguity by severity. A critical
// exception blocks a merchant's transition to ACTIVE, so this gauge has a business consequence.
func (r *Registry) SetReconciliationExceptions(severity string, n float64) {
	r.reconExcept.WithLabelValues(r.bound(MetricReconciliationExceptions, severity)...).Set(n)
}

// SetDLQDepth publishes what is parked and needs a human.
func (r *Registry) SetDLQDepth(queue string, messages float64) {
	r.dlqDepth.WithLabelValues(r.bound(MetricDLQDepth, queue)...).Set(messages)
}

// RecordLogFieldRejected counts one log attribute dropped by the allowlist handler. It makes the
// allowlist's silence auditable: a field that a developer expects to see and never does shows up
// here as a number instead of as an afternoon of confusion.
func (r *Registry) RecordLogFieldRejected(field string) {
	r.logFieldRejected.WithLabelValues(r.bound(MetricLogFieldRejectedTotal, field)...).Inc()
}

// RecordLogLinesSuppressed counts lines dropped by the high-volume log sampler.
func (r *Registry) RecordLogLinesSuppressed(level string, n int) {
	r.logSuppressed.WithLabelValues(r.bound(MetricLogLinesSuppressedTotal, level)...).Add(float64(n))
}

// bound applies the runtime cardinality guard for a metric.
func (r *Registry) bound(metric string, vals ...string) []string {
	g, ok := r.guards[metric]
	if !ok {
		return vals
	}
	return g.bound(vals)
}

// observe records a histogram sample, attaching a trace exemplar when the current span is
// sampled. The sampled check is not an optimization: an exemplar pointing at a trace ID that the
// tail sampler discarded is a link to a 404, which is worse than no link because an operator
// learns to stop clicking.
func observe(ctx context.Context, h *prometheus.HistogramVec, labels []string, v float64) {
	obs, err := h.GetMetricWithLabelValues(labels...)
	if err != nil {
		return
	}
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() && sc.IsSampled() {
		if eo, ok := obs.(prometheus.ExemplarObserver); ok {
			eo.ObserveWithExemplar(v, prometheus.Labels{"trace_id": sc.TraceID().String()})
			return
		}
	}
	obs.Observe(v)
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: NFR-44, NFR-45.
//
// The metric registry, its RED plus business coverage, and the cardinality guard
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
