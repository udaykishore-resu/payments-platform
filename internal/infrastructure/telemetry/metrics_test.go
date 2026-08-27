package telemetry_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
)

// baselineMetric is one row of the baseline §22.2 registry, transcribed here rather than read
// from the implementation. That is the entire point of the table: if the test derived its
// expectations from the registry it would pass for a registry that had lost half its metrics.
// A metric deleted from metrics.go, a label renamed, a counter turned into a gauge — each fails
// here, which is where a published observability contract should break.
type baselineMetric struct {
	name   string
	typ    dto.MetricType
	labels []string
}

var baselineRegistry = []baselineMetric{
	{telemetry.MetricHTTPRequestsTotal, dto.MetricType_COUNTER, []string{"service", "route", "method", "status", "tenant_tier"}},
	{telemetry.MetricHTTPRequestDuration, dto.MetricType_HISTOGRAM, []string{"service", "route", "method"}},
	{telemetry.MetricPaymentsTotal, dto.MetricType_COUNTER, []string{"outcome", "currency", "payment_method", "gateway", "tenant_tier"}},
	{telemetry.MetricPaymentAuthorizationRate, dto.MetricType_GAUGE, []string{"gateway", "currency"}},
	{telemetry.MetricGatewayRequestDuration, dto.MetricType_HISTOGRAM, []string{"gateway", "operation"}},
	{telemetry.MetricGatewayErrorsTotal, dto.MetricType_COUNTER, []string{"gateway", "operation", "class"}},
	{telemetry.MetricCircuitBreakerState, dto.MetricType_GAUGE, []string{"gateway", "operation"}},
	{telemetry.MetricIdempotencyOutcomesTotal, dto.MetricType_COUNTER, []string{"outcome"}},
	{telemetry.MetricRoutingDecisionsTotal, dto.MetricType_COUNTER, []string{"gateway", "reason"}},
	{telemetry.MetricWorkflowStepDuration, dto.MetricType_HISTOGRAM, []string{"workflow", "step", "outcome"}},
	{telemetry.MetricWorkflowInstances, dto.MetricType_GAUGE, []string{"workflow", "state"}},
	{telemetry.MetricOnboardingDuration, dto.MetricType_HISTOGRAM, []string{"outcome"}},
	{telemetry.MetricOutboxBacklog, dto.MetricType_GAUGE, []string{"topic"}},
	{telemetry.MetricConsumerLag, dto.MetricType_GAUGE, []string{"topic", "group"}},
	{telemetry.MetricConfigSnapshotAge, dto.MetricType_GAUGE, []string{"service"}},
	{telemetry.MetricReconciliationExceptions, dto.MetricType_GAUGE, []string{"severity"}},
	{telemetry.MetricDLQDepth, dto.MetricType_GAUGE, []string{"queue"}},
}

// exerciseEveryRecorder touches every metric once, so Gather returns a family for each. A
// prometheus *Vec with no observations is invisible to Gather, which means "is it declared" can
// only be answered by declaring *and* using it — and that is a feature: a metric no recorder can
// reach is a metric no dashboard will ever show.
func exerciseEveryRecorder(r *telemetry.Registry) {
	ctx := context.Background()
	r.ObserveHTTPRequest(ctx, "payment-api", "/v1/payments", "POST", 201, telemetry.TierPooled, 42*time.Millisecond)
	r.RecordPaymentOutcome(telemetry.OutcomeAuthorized, "USD", "CARD", "adyen", telemetry.TierPooled)
	r.SetPaymentAuthorizationRate("adyen", "USD", 0.93)
	r.ObserveGatewayRequest(ctx, "adyen", "authorize", 310*time.Millisecond)
	r.RecordGatewayError("adyen", "authorize", telemetry.GatewayErrTimeout)
	r.SetCircuitState("adyen", "authorize", telemetry.CircuitClosed)
	r.RecordIdempotencyOutcome(telemetry.IdempotencyNew)
	r.RecordRoutingDecision("adyen", telemetry.RoutingPrimary)
	r.ObserveWorkflowStep(ctx, "merchant_onboarding", "kyb_check", telemetry.StepSuccess, 4*time.Second)
	r.SetWorkflowInstances("merchant_onboarding", "RUNNING", 7)
	r.ObserveOnboarding("success", 12*time.Minute)
	r.SetOutboxBacklog("payments.events.v1", 0)
	r.SetConsumerLag("payments.events.v1", "ledger-projector", 3)
	r.SetConfigSnapshotAge("payment-api", 12*time.Second)
	r.SetReconciliationExceptions("critical", 0)
	r.SetDLQDepth("workflow_dlq", 2)
}

func TestEveryBaselineMetricIsRegistered(t *testing.T) {
	// Verifies: NFR-45.
	t.Parallel()
	r := newTestRegistry(t, 0)
	exerciseEveryRecorder(r)

	families, err := r.Prometheus().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	got := make(map[string]baselineMetric, len(families))
	for _, f := range families {
		bm := baselineMetric{name: f.GetName(), typ: f.GetType()}
		if len(f.GetMetric()) > 0 {
			for _, l := range f.GetMetric()[0].GetLabel() {
				bm.labels = append(bm.labels, l.GetName())
			}
		}
		sort.Strings(bm.labels)
		got[bm.name] = bm
	}

	for _, want := range baselineRegistry {
		t.Run(want.name, func(t *testing.T) {
			have, ok := got[want.name]
			if !ok {
				t.Fatalf("metric %s is not registered (baseline §22.2)", want.name)
			}
			if have.typ != want.typ {
				t.Errorf("metric %s is a %s, baseline §22.2 says %s", want.name, have.typ, want.typ)
			}
			wantLabels := append([]string(nil), want.labels...)
			sort.Strings(wantLabels)
			if strings.Join(have.labels, ",") != strings.Join(wantLabels, ",") {
				t.Errorf("metric %s labels = [%s], baseline §22.2 says [%s]",
					want.name, strings.Join(have.labels, ","), strings.Join(wantLabels, ","))
			}
		})
	}
}

func TestHistogramBucketsMatchTheSLOThresholds(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t, 0)
	exerciseEveryRecorder(r)

	// The buckets that exist because a number in the spec sits on them. If one of these
	// disappears, a quantile at an SLO threshold silently becomes an interpolation.
	required := map[string][]float64{
		telemetry.MetricHTTPRequestDuration:    {0.06, 0.25},    // §18 p50 and p99 targets
		telemetry.MetricGatewayRequestDuration: {5, 8},          // §10 UNHEALTHY threshold, §12 hard timeout
		telemetry.MetricWorkflowStepDuration:   {30, 300, 1800}, // §11 step timeouts
		telemetry.MetricOnboardingDuration:     {1800},          // §18 p95 ≤ 30 min
	}

	families, err := r.Prometheus().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		want, ok := required[f.GetName()]
		if !ok {
			continue
		}
		have := map[float64]bool{}
		for _, b := range f.GetMetric()[0].GetHistogram().GetBucket() {
			have[b.GetUpperBound()] = true
		}
		for _, w := range want {
			if !have[w] {
				t.Errorf("%s is missing the %v bucket boundary", f.GetName(), w)
			}
		}
	}
}

func TestCardinalityGuardRejectsForbiddenLabels(t *testing.T) {
	// Verifies: NFR-44.
	t.Parallel()
	// Every name on the §22.3 / observability.md §3.3 forbidden list, plus a control.
	//
	// This list must stay in step with the pull-request lint's copy in
	// scripts/specdump/main.go: a label the lint refuses and the runtime accepts is a rule
	// that only applies to people who run CI, and the whole point of having two halves is
	// that neither half can be the only one that says no.
	for _, label := range []string{
		// Baseline §22.3.
		"merchant_id", "payment_id", "attempt_id", "idempotency_key", "email", "ip",
		"user_agent", "url", "path", "error_message",
		// Correlation identifiers — unique per request by construction.
		"trace_id", "span_id", "correlation_id", "request_id",
		// Subject identifiers.
		"user_id", "session_id", "customer_id",
		// Network locators.
		"client_ip", "remote_addr",
		// Cardholder and contact data.
		"pan", "card_number", "email_address",
		// The camelCase spellings of the §22.3 names: the same label, and the first thing
		// an author tries after the snake_case one is rejected.
		"merchantId", "paymentId", "attemptId", "idempotencyKey",
		// Case and separator folding, on names from every group above.
		"MERCHANT_ID", "TraceID", "merchant-id", "Card.Number", "RemoteAddr",
	} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			err := telemetry.ValidateLabels("pp_example_total", []string{"gateway", label})
			if err == nil {
				t.Fatalf("label %q was accepted; §22.3 forbids it", label)
			}
			var fe *telemetry.ErrForbiddenLabel
			if !errors.As(err, &fe) {
				t.Fatalf("error is %T, want *telemetry.ErrForbiddenLabel", err)
			}
			if fe.Label != label {
				t.Errorf("error names label %q, want %q", fe.Label, label)
			}
			if fe.Reason == "" {
				t.Error("the rejection carries no reason; the error is the only place the rule is explained")
			}
		})
	}

	if err := telemetry.ValidateLabels("pp_example_total", []string{"gateway", "operation", "tenant_tier"}); err != nil {
		t.Fatalf("a legitimate label set was rejected: %v", err)
	}
}

func TestSeriesOverflowFoldsRatherThanDroppingOrGrowing(t *testing.T) {
	t.Parallel()
	const budget = 3
	r := newTestRegistry(t, budget)

	// Five distinct gateways against a budget of three: the first three get real series, the
	// last two are folded.
	for _, gw := range []string{"adyen", "stripe", "paypal", "worldpay", "checkout"} {
		r.RecordRoutingDecision(gw, telemetry.RoutingPrimary)
	}

	series := map[string]float64{}
	var overflowCount float64
	families, err := r.Prometheus().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		switch f.GetName() {
		case telemetry.MetricRoutingDecisionsTotal:
			for _, m := range f.GetMetric() {
				var gw string
				for _, l := range m.GetLabel() {
					if l.GetName() == "gateway" {
						gw = l.GetValue()
					}
				}
				series[gw] = m.GetCounter().GetValue()
			}
		case telemetry.MetricSeriesOverflowTotal:
			for _, m := range f.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "metric" && l.GetValue() == telemetry.MetricRoutingDecisionsTotal {
						overflowCount = m.GetCounter().GetValue()
					}
				}
			}
		}
	}

	if len(series) != budget+1 {
		t.Fatalf("got %d series (%v), want %d real + 1 overflow", len(series), series, budget)
	}
	if got := series[telemetry.OverflowLabelValue]; got != 2 {
		t.Errorf("overflow series counted %v observations, want 2 — folded observations must still be counted, not dropped", got)
	}
	if overflowCount != 2 {
		t.Errorf("%s = %v, want 2 — folding must be visible as a number", telemetry.MetricSeriesOverflowTotal, overflowCount)
	}

	// The budget must hold no matter how much more is thrown at it: this is the property that
	// makes it a memory bound rather than a warning.
	for i := 0; i < 1000; i++ {
		r.RecordRoutingDecision("gw-"+string(rune('a'+i%26))+string(rune('a'+i/26)), telemetry.RoutingFallbackHealth)
	}
	families, err = r.Prometheus().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != telemetry.MetricRoutingDecisionsTotal {
			continue
		}
		if n := len(f.GetMetric()); n > budget+1 {
			t.Fatalf("series count grew to %d against a budget of %d", n, budget)
		}
	}
}

func TestStatusClassCollapsesTheCode(t *testing.T) {
	t.Parallel()
	cases := map[int]telemetry.StatusClass{
		200: telemetry.Status2xx, 201: telemetry.Status2xx,
		304: telemetry.Status3xx,
		400: telemetry.Status4xx, 422: telemetry.Status4xx, 429: telemetry.Status4xx,
		500: telemetry.Status5xx, 502: telemetry.Status5xx, 504: telemetry.Status5xx,
	}
	for code, want := range cases {
		if got := telemetry.StatusClassOf(code); got != want {
			t.Errorf("StatusClassOf(%d) = %s, want %s", code, got, want)
		}
	}
}

func TestHandlerServesOpenMetrics(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t, 0)
	exerciseEveryRecorder(r)
	if r.Handler() == nil {
		t.Fatal("Handler returned nil")
	}
}

func newTestRegistry(t *testing.T, maxSeries int) *telemetry.Registry {
	t.Helper()
	r, err := telemetry.NewRegistry(telemetry.RegistryOptions{MaxSeriesPerMetric: maxSeries})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}
