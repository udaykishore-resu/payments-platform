package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
)

// fakeAccounting stands in for the metric registry so the handler tests can assert on the
// counters without gathering a whole Prometheus registry.
type fakeAccounting struct {
	mu         sync.Mutex
	rejected   []string
	suppressed map[string]int
}

func newFakeAccounting() *fakeAccounting {
	return &fakeAccounting{suppressed: map[string]int{}}
}

func (f *fakeAccounting) RecordLogFieldRejected(field string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected = append(f.rejected, field)
}

func (f *fakeAccounting) RecordLogLinesSuppressed(level string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suppressed[level] += n
}

func (f *fakeAccounting) rejectedFields() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rejected...)
}

func (f *fakeAccounting) suppressedCount(level string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.suppressed[level]
}

func decodeLines(t *testing.T, b *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON (%v): %s", err, line)
		}
		out = append(out, m)
	}
	return out
}

func TestAllowlistDropsUnregisteredKeys(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	acct := newFakeAccounting()
	log := telemetry.NewLogger(&buf, telemetry.LogOptions{Metrics: acct})

	log.Info("payment authorized",
		telemetry.KeyGatewayID, "gw_adyen",
		telemetry.KeyPaymentID, "pay_01H",
		// The three shapes of leak the allowlist exists to stop: a field nobody registered, a
		// field named after cardholder data, and a raw credential-looking field.
		"card_number", "4111111111111111",
		"customer_email", "merchant@example.com",
		"authorization", "Bearer abc.def",
	)

	lines := decodeLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	line := lines[0]

	for _, want := range []string{telemetry.KeyGatewayID, telemetry.KeyPaymentID, telemetry.KeyMessage, telemetry.KeyLevel, telemetry.KeyTime} {
		if _, ok := line[want]; !ok {
			t.Errorf("registered key %q was dropped", want)
		}
	}
	for _, forbidden := range []string{"card_number", "customer_email", "authorization"} {
		if _, ok := line[forbidden]; ok {
			t.Errorf("unregistered key %q was emitted", forbidden)
		}
	}
	if raw := buf.String(); strings.Contains(raw, "4111111111111111") || strings.Contains(raw, "Bearer") {
		t.Fatalf("a dropped value still reached the output: %s", raw)
	}

	got := strings.Join(acct.rejectedFields(), ",")
	for _, want := range []string{"card_number", "customer_email", "authorization"} {
		if !strings.Contains(got, want) {
			t.Errorf("drop of %q was not counted; a silent drop is indistinguishable from a bug", want)
		}
	}
}

func TestAllowlistNormalizesCamelCaseAliases(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := telemetry.NewLogger(&buf, telemetry.LogOptions{Metrics: newFakeAccounting()})

	// The API surface is camelCase and developers type what they see; the wire schema is
	// snake_case. Accepting both and normalizing is what keeps the allowlist trusted.
	log.Info("payment authorized", "gatewayId", "gw_adyen", "tenantTier", "siloed")

	line := decodeLines(t, &buf)[0]
	if line[telemetry.KeyGatewayID] != "gw_adyen" {
		t.Errorf("gatewayId was not normalized to %s: %v", telemetry.KeyGatewayID, line)
	}
	if line[telemetry.KeyTenantTier] != "siloed" {
		t.Errorf("tenantTier was not normalized to %s: %v", telemetry.KeyTenantTier, line)
	}
	if _, ok := line["gatewayId"]; ok {
		t.Error("the camelCase spelling reached the output; there must be exactly one wire name")
	}
}

// TestAllowlistKeepsTheConventionalErrorKey is the regression test for a defect that made every
// ERROR line in the Postgres workflow engine useless.
//
// Those call sites write `log.ErrorContext(ctx, "lease acquisition failed", "error", err)` — the
// spelling in the standard library's own examples. "error" was not a registered key, so the
// allowlist dropped it, and the line that reached the operator said a lease acquisition had
// failed and nothing whatsoever about why. Twice a second, indefinitely.
func TestAllowlistKeepsTheConventionalErrorKey(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"error", "err"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			acct := newFakeAccounting()
			log := telemetry.NewLogger(&buf, telemetry.LogOptions{Metrics: acct})

			log.Error("lease acquisition failed", key, errors.New("relation pp.workflow_instances does not exist"))

			line := decodeLines(t, &buf)[0]
			got, ok := line[telemetry.KeyErrorMessage]
			if !ok {
				t.Fatalf("%q did not reach the output as %s: %v", key, telemetry.KeyErrorMessage, line)
			}
			if s, _ := got.(string); !strings.Contains(s, "workflow_instances") {
				t.Errorf("the error text was lost: %v", got)
			}
			if _, ok := line[key]; ok {
				t.Errorf("the %q spelling reached the output; there must be exactly one wire name", key)
			}
			if fields := strings.Join(acct.rejectedFields(), ","); strings.Contains(fields, key) {
				t.Errorf("%q was counted as rejected while also being emitted", key)
			}
		})
	}
}

// TestAllowlistStillRefusesAnUnregisteredErrorLikeKey pins that the alias above is exactly two
// spellings of one registered dimension, not a hole in the allowlist.
func TestAllowlistStillRefusesAnUnregisteredErrorLikeKey(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := telemetry.NewLogger(&buf, telemetry.LogOptions{Metrics: newFakeAccounting()})
	log.Error("x failed", "error_detail", "customer@example.com", "errors", "leak")

	line := decodeLines(t, &buf)[0]
	for _, forbidden := range []string{"error_detail", "errors"} {
		if _, ok := line[forbidden]; ok {
			t.Errorf("unregistered key %q was emitted", forbidden)
		}
	}
	if strings.Contains(buf.String(), "customer@example.com") {
		t.Fatalf("a dropped value still reached the output: %s", buf.String())
	}
}

func TestLoggerBindsContextFields(t *testing.T) {
	// Verifies: NFR-43.
	// Not parallel: SetBaseLogger is process-wide state, which is the price of a logger
	// constructor that call sites can reach without dependency injection.
	var buf bytes.Buffer
	log := telemetry.NewLogger(&buf, telemetry.LogOptions{
		Metrics: newFakeAccounting(),
		Base: []slog.Attr{
			slog.String(telemetry.KeyService, "payment-api"),
			slog.String(telemetry.KeyVersion, "abc1234"),
			slog.String(telemetry.KeyEnvironment, "prod"),
			slog.String(telemetry.KeyRegion, "eu-west-1"),
		},
	})
	telemetry.SetBaseLogger(log)
	t.Cleanup(func() { telemetry.SetBaseLogger(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))) })

	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))
	ctx = telemetry.ContextWithFields(ctx, telemetry.Fields{
		CorrelationID: "corr_1",
		RequestID:     "req_1",
		TenantID:      "tnt_1",
		TenantTier:    telemetry.TierEnterprise,
	})
	// A later stage learns the merchant and the payment; the earlier fields must survive.
	ctx = telemetry.ContextWithFields(ctx, telemetry.Fields{MerchantID: "mch_1", PaymentID: "pay_1"})

	telemetry.Logger(ctx).Info("payment authorized", telemetry.KeyGatewayID, "gw_adyen")

	line := decodeLines(t, &buf)[0]
	want := map[string]any{
		telemetry.KeyService:       "payment-api",
		telemetry.KeyVersion:       "abc1234",
		telemetry.KeyEnvironment:   "prod",
		telemetry.KeyRegion:        "eu-west-1",
		telemetry.KeyTraceID:       "4bf92f3577b34da6a3ce929d0e0e4736",
		telemetry.KeySpanID:        "00f067aa0ba902b7",
		telemetry.KeySampled:       true,
		telemetry.KeyCorrelationID: "corr_1",
		telemetry.KeyRequestID:     "req_1",
		telemetry.KeyTenantID:      "tnt_1",
		telemetry.KeyTenantTier:    "enterprise",
		telemetry.KeyMerchantID:    "mch_1",
		telemetry.KeyPaymentID:     "pay_1",
		telemetry.KeyGatewayID:     "gw_adyen",
		telemetry.KeyMessage:       "payment authorized",
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("field %s = %v, want %v", k, line[k], v)
		}
	}

	// The timestamp is the schema's, not slog's default.
	ts, _ := line[telemetry.KeyTime].(string)
	if _, err := time.Parse("2006-01-02T15:04:05.000Z07:00", ts); err != nil {
		t.Errorf("ts = %q, want RFC3339 with milliseconds: %v", ts, err)
	}
	if _, ok := line["time"]; ok {
		t.Error("slog's default `time` key leaked into the schema")
	}
}

func TestLoggerWithoutASpanStillWorks(t *testing.T) {
	var buf bytes.Buffer
	telemetry.SetBaseLogger(telemetry.NewLogger(&buf, telemetry.LogOptions{Metrics: newFakeAccounting()}))
	t.Cleanup(func() { telemetry.SetBaseLogger(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))) })

	telemetry.Logger(context.Background()).Info("outbox relay started")

	line := decodeLines(t, &buf)[0]
	if _, ok := line[telemetry.KeyTraceID]; ok {
		t.Error("a line outside a span must not claim a trace_id")
	}
	if line[telemetry.KeyMessage] != "outbox relay started" {
		t.Errorf("msg = %v", line[telemetry.KeyMessage])
	}
}

func TestSamplingSuppressesAndCounts(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	acct := newFakeAccounting()

	// A fake clock rather than a sleep: the sampler's unit is a wall-clock second, and a test
	// that waits for one is a test that takes a second and still races.
	now := time.Date(2024, 2, 11, 9, 31, 47, 0, time.UTC)
	clock := func() time.Time { return now }

	log := telemetry.NewLogger(&buf, telemetry.LogOptions{
		Metrics:  acct,
		Sampling: telemetry.SamplingOptions{PerSecond: 2, Now: clock},
	})

	for i := 0; i < 5; i++ {
		log.Info("http request served", telemetry.KeyRoute, "/v1/payments")
	}
	if lines := decodeLines(t, &buf); len(lines) != 2 {
		t.Fatalf("emitted %d lines, want 2 (the per-second allowance)", len(lines))
	}
	if got := acct.suppressedCount(slog.LevelInfo.String()); got != 3 {
		t.Errorf("suppressed count = %d, want 3", got)
	}

	// A different message is a different bucket: sampling must not blind one line because
	// another was noisy.
	buf.Reset()
	log.Info("payment authorized", telemetry.KeyGatewayID, "gw_adyen")
	if lines := decodeLines(t, &buf); len(lines) != 1 {
		t.Fatalf("a distinct message was suppressed by another message's bucket")
	}

	// Next second: the allowance resets and the first emitted line carries the debt.
	buf.Reset()
	now = now.Add(time.Second)
	log.Info("http request served", telemetry.KeyRoute, "/v1/payments")
	line := decodeLines(t, &buf)[0]
	if got := line[telemetry.KeySuppressedCount]; got != float64(3) {
		t.Errorf("%s = %v, want 3 — a reader must be able to tell one occurrence from three thousand",
			telemetry.KeySuppressedCount, got)
	}
}

func TestSamplingNeverTouchesWarnOrError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	acct := newFakeAccounting()
	now := time.Date(2024, 2, 11, 9, 31, 47, 0, time.UTC)

	log := telemetry.NewLogger(&buf, telemetry.LogOptions{
		Metrics:  acct,
		Sampling: telemetry.SamplingOptions{PerSecond: 1, Now: func() time.Time { return now }},
	})

	for i := 0; i < 4; i++ {
		log.Warn("gateway circuit opened", telemetry.KeyGatewayID, "gw_adyen")
		log.Error("outbox publish failed", telemetry.KeyErrorCode, "DEPENDENCY_FAILURE")
	}

	// The rare lines are the ones you need, and they are cheap. Sampling them is how an
	// incident timeline ends up with a hole in it.
	if lines := decodeLines(t, &buf); len(lines) != 8 {
		t.Fatalf("emitted %d lines, want 8 — WARN and ERROR are never sampled", len(lines))
	}
	if got := acct.suppressedCount(slog.LevelWarn.String()); got != 0 {
		t.Errorf("suppressed %d WARN lines", got)
	}
	if got := acct.suppressedCount(slog.LevelError.String()); got != 0 {
		t.Errorf("suppressed %d ERROR lines", got)
	}
}

func TestLevelIsAdjustableAtRuntime(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	level := new(slog.LevelVar)
	level.Set(slog.LevelInfo)
	log := telemetry.NewLogger(&buf, telemetry.LogOptions{Level: level, Metrics: newFakeAccounting()})

	log.Debug("routing weights computed", telemetry.KeyCount, 3)
	if buf.Len() != 0 {
		t.Fatal("DEBUG was emitted at level INFO")
	}

	level.Set(slog.LevelDebug)
	log.Debug("routing weights computed", telemetry.KeyCount, 3)
	if lines := decodeLines(t, &buf); len(lines) != 1 {
		t.Fatal("DEBUG was not emitted after the level was lowered on the running logger")
	}
}

func TestGroupedErrorAttributesSurviveTheAllowlist(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := telemetry.NewLogger(&buf, telemetry.LogOptions{Metrics: newFakeAccounting()})

	log.Error("gateway call failed", slog.Group("error",
		slog.String("code", "GATEWAY_TIMEOUT"),
		slog.String("category", "TIMEOUT"),
		slog.Bool("retryable", false),
		slog.String("stack_trace_of_doom", "not registered"),
	))

	line := decodeLines(t, &buf)[0]
	group, ok := line["error"].(map[string]any)
	if !ok {
		t.Fatalf("the error group was dropped entirely: %v", line)
	}
	if group["code"] != "GATEWAY_TIMEOUT" || group["category"] != "TIMEOUT" {
		t.Errorf("error group = %v, want code and category preserved", group)
	}
	if _, ok := group["stack_trace_of_doom"]; ok {
		t.Error("an unregistered key inside a group escaped the allowlist")
	}
}
