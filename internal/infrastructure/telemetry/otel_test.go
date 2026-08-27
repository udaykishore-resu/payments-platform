package telemetry_test

import (
	"bytes"
	"context"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
)

func TestConfigValidationRejectsMislabellingConfigurations(t *testing.T) {
	t.Parallel()
	base := telemetry.Config{Service: "payment-api", Environment: "prod", Region: "eu-west-1", Plane: telemetry.PlaneData}

	cases := []struct {
		name string
		mut  func(*telemetry.Config)
		want string
	}{
		{"missing service", func(c *telemetry.Config) { c.Service = "" }, "PP_SERVICE"},
		{"missing environment", func(c *telemetry.Config) { c.Environment = "" }, "PP_ENVIRONMENT"},
		{"missing region", func(c *telemetry.Config) { c.Region = "" }, "PP_REGION"},
		{"unknown plane", func(c *telemetry.Config) { c.Plane = "sideways" }, "PP_PLANE"},
		{"ratio above one", func(c *telemetry.Config) { c.TraceSampleRatio = telemetry.SampleRatio(1.5) }, "PP_TRACE_SAMPLE_RATIO"},
		{"negative ratio", func(c *telemetry.Config) { c.TraceSampleRatio = telemetry.SampleRatio(-0.5) }, "PP_TRACE_SAMPLE_RATIO"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := base
			tc.mut(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("configuration was accepted; a mislabelled service is invisible at runtime")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
		})
	}

	if err := base.Validate(); err != nil {
		t.Fatalf("a valid configuration was rejected: %v", err)
	}
}

func TestSetupWiresEverythingAndShutsDownCleanly(t *testing.T) {
	// Not parallel: Setup installs the process-wide logger and the global OTel providers.
	var buf bytes.Buffer
	before := goroutinesAtRest()

	tel, err := telemetry.Setup(context.Background(), telemetry.Config{
		Service:                  "payment-api",
		Version:                  "abc1234",
		Environment:              "prod",
		Region:                   "eu-west-1",
		Plane:                    telemetry.PlaneData,
		PodName:                  "payment-api-7d9f-2xk4",
		LogOutput:                &buf,
		LogLevel:                 slog.LevelInfo,
		MetricsRuntimeCollectors: true,
		// No OTLP endpoint: spans are created (so trace IDs reach logs) but nothing is exported
		// and no exporter goroutine runs. That is the shape a test wants and, deliberately, the
		// same code path a local `go run` takes.
		ShutdownTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	if tel.Metrics == nil || tel.Logger == nil || tel.Tracer == nil || tel.Tracing == nil {
		t.Fatal("Setup returned an incomplete Telemetry")
	}
	if tel.Meter != nil {
		t.Error("the OTel meter provider must stay off unless explicitly enabled")
	}
	if tel.MetricsHandler() == nil {
		t.Error("MetricsHandler returned nil")
	}

	// The correlation spine works end to end: a span started through the package helper puts a
	// trace ID into a line written through the package logger.
	ctx, span := telemetry.StartSpan(context.Background(), "POST /v1/payments")
	ctx = telemetry.ContextWithFields(ctx, telemetry.Fields{TenantID: "tnt_1", TenantTier: telemetry.TierPooled})
	telemetry.Logger(ctx).Info("payment authorized", telemetry.KeyGatewayID, "gw_adyen")
	span.End()

	out := buf.String()
	for _, want := range []string{`"service":"payment-api"`, `"version":"abc1234"`, `"environment":"prod"`,
		`"region":"eu-west-1"`, `"pod":"payment-api-7d9f-2xk4"`, `"tenant_id":"tnt_1"`, `"gateway_id":"gw_adyen"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log output is missing %s\n%s", want, out)
		}
	}
	if !strings.Contains(out, `"trace_id":"`) {
		t.Errorf("no trace_id reached the log line; the correlation spine is broken\n%s", out)
	}

	tel.SetLevel(slog.LevelDebug)
	if tel.Level.Level() != slog.LevelDebug {
		t.Error("SetLevel did not take effect")
	}

	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Shutdown is idempotent: a composition root that defers it and also calls it on a signal
	// path must not double-fail.
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}

	if after := goroutinesAtRest(); after > before+2 {
		t.Errorf("goroutines went from %d to %d across Setup/Shutdown; background work must have an owner", before, after)
	}
}

func TestSetupRejectsAnInvalidConfig(t *testing.T) {
	if _, err := telemetry.Setup(context.Background(), telemetry.Config{}); err == nil {
		t.Fatal("Setup accepted an empty configuration")
	}
}

// goroutinesAtRest lets short-lived goroutines finish before counting, so the leak assertion
// measures a leak rather than a scheduling race.
func goroutinesAtRest() int {
	prev := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		time.Sleep(10 * time.Millisecond)
		runtime.GC()
		n := runtime.NumGoroutine()
		if n == prev {
			return n
		}
		prev = n
	}
	return prev
}
