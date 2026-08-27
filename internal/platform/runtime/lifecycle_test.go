package runtime_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/platform/runtime"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// TestDrainOrderFollowsLLDSection25 pins the shutdown sequence.
//
// The order is the whole design: readiness fails *before* anything closes, because Kubernetes
// removes a pod from endpoints and sends SIGTERM concurrently and unordered. A process that closed
// its listener first would refuse connections that kube-proxy on some nodes is still routing to
// it, producing connection resets clients see as 502s on every deploy.
func TestDrainOrderFollowsLLDSection25(t *testing.T) {
	t.Parallel()
	var (
		mu     sync.Mutex
		events []string
	)
	record := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, s)
	}

	lc := &runtime.Lifecycle{
		Service: "test", Version: "test", Logger: quiet(),
		Budgets: runtime.Budgets{
			DrainDelay: 10 * time.Millisecond, InFlight: time.Second,
			Resources: time.Second, TelemetryFlush: time.Second,
		},
		Telemetry:    telemetryDouble{onShutdown: func() { record("telemetry-flush") }},
		OnDrainStart: func() { record("readiness-failed") },
	}
	lc.Add("first",
		func(context.Context) error { record("start-first"); return nil },
		func(context.Context) error { record("stop-first"); return nil })
	lc.Add("second",
		func(context.Context) error { record("start-second"); return nil },
		func(context.Context) error { record("stop-second"); return nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- lc.Run(ctx) }()

	// Give the components time to start before signalling shutdown.
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) >= 2
	})
	cancel()

	select {
	case code := <-done:
		if code != runtime.ExitOK {
			t.Errorf("exit code = %d, want %d", code, runtime.ExitOK)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return; the drain is unbounded")
	}

	mu.Lock()
	got := strings.Join(events, ",")
	mu.Unlock()
	want := "start-first,start-second,readiness-failed,stop-second,stop-first,telemetry-flush"
	if got != want {
		t.Errorf("drain order:\n got %s\nwant %s", got, want)
	}
}

// TestStartFailureStopsWhatStarted asserts a half-started process closes what it opened.
//
// A process that fails halfway through startup and exits without closing leaves a database
// connection and a consumer-group membership behind, and the next pod inherits a rebalance.
func TestStartFailureStopsWhatStarted(t *testing.T) {
	t.Parallel()
	var stopped []string
	var mu sync.Mutex

	lc := &runtime.Lifecycle{
		Service: "test", Logger: quiet(),
		Budgets: runtime.Budgets{InFlight: time.Second, Resources: time.Second},
	}
	lc.Add("ok",
		func(context.Context) error { return nil },
		func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			stopped = append(stopped, "ok")
			return nil
		})
	lc.Add("broken",
		func(context.Context) error { return errors.New("cannot bind") },
		func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			stopped = append(stopped, "broken")
			return nil
		})
	lc.Add("never-started",
		func(context.Context) error { t.Error("a component after the failure was started"); return nil },
		func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			stopped = append(stopped, "never-started")
			return nil
		})

	if code := lc.Run(context.Background()); code != runtime.ExitStartupFailure {
		t.Fatalf("exit code = %d, want %d", code, runtime.ExitStartupFailure)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(stopped) != 1 || stopped[0] != "ok" {
		t.Errorf("stopped %v, want only the component that started", stopped)
	}
}

// TestDrainingFlagIsSetBeforeAnythingCloses asserts the readiness handler can observe the drain
// before a single component has been stopped.
func TestDrainingFlagIsSetBeforeAnythingCloses(t *testing.T) {
	t.Parallel()
	lc := &runtime.Lifecycle{
		Service: "test", Logger: quiet(),
		Budgets: runtime.Budgets{InFlight: time.Second, Resources: time.Second},
	}
	var drainingAtStop bool
	lc.Add("component",
		func(context.Context) error { return nil },
		func(context.Context) error { drainingAtStop = lc.Draining(); return nil })

	if lc.Draining() {
		t.Fatal("Draining is true before the process has started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- lc.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	if !drainingAtStop {
		t.Error("Draining was false when the first component was stopped; readiness must fail first")
	}
	if !lc.Draining() {
		t.Error("Draining is false after the drain")
	}
}

// TestShutdownIsBoundedByTheBudget asserts a component that will not stop cannot hold the process
// past its budget — the failure being a SIGKILL that abandons in-flight work *after* consuming the
// whole termination grace period.
func TestShutdownIsBoundedByTheBudget(t *testing.T) {
	t.Parallel()
	lc := &runtime.Lifecycle{
		Service: "test", Logger: quiet(),
		Budgets: runtime.Budgets{
			InFlight: 50 * time.Millisecond, Resources: 50 * time.Millisecond,
			TelemetryFlush: 50 * time.Millisecond,
		},
	}
	lc.Add("wedged",
		func(context.Context) error { return nil },
		func(ctx context.Context) error {
			<-ctx.Done() // never returns on its own
			return ctx.Err()
		})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	start := time.Now()
	go func() { done <- lc.Run(ctx) }()
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("drain took %s despite a 150 ms budget", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return; the drain is not bounded by the budget")
	}
}

// TestBudgetValidateCatchesAnOverrun is the arithmetic check.
//
// It is a startup check rather than a comment because the sum is exactly the thing that silently
// stops holding when somebody raises a timeout: the manifest says 75 seconds, the code adds up to
// 80, and the difference is a SIGKILL mid-drain that nobody notices until they graph 502s during
// deploys.
func TestBudgetValidateCatchesAnOverrun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		budgets runtime.Budgets
		grace   time.Duration
		preStop time.Duration
		wantErr bool
	}{
		{"api budget fits its manifest", runtime.APIBudgets(), 75 * time.Second, 15 * time.Second, false},
		{"orchestrator budget fits", runtime.OrchestratorBudgets(), 90 * time.Second, 10 * time.Second, false},
		{"ingress budget fits", runtime.IngressBudgets(), 45 * time.Second, 15 * time.Second, false},
		{"worker budget fits", runtime.WorkerBudgets(), 120 * time.Second, 5 * time.Second, false},
		{"relay budget fits", runtime.RelayBudgets(), 60 * time.Second, 5 * time.Second, false},
		{"consumer budget fits", runtime.ConsumerBudgets(), 90 * time.Second, 5 * time.Second, false},
		{"an over-long budget is rejected", runtime.OrchestratorBudgets(), 30 * time.Second, 15 * time.Second, true},
		{"an unset grace period is not checked", runtime.OrchestratorBudgets(), 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.budgets.Validate(tc.grace, tc.preStop)
			if tc.wantErr && err == nil {
				t.Fatalf("budget of %s inside %s was accepted", tc.budgets.Total(), tc.grace-tc.preStop)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("budget of %s inside %s was rejected: %v",
					tc.budgets.Total(), tc.grace-tc.preStop, err)
			}
		})
	}
}

// TestGuardConvertsAPanicIntoAnError asserts a bug in a background loop does not take the process
// down with no drain: in-flight payments abandoned, leases held, offsets uncommitted.
func TestGuardConvertsAPanicIntoAnError(t *testing.T) {
	t.Parallel()
	err := runtime.Guard("test-component", quiet(), func() error { panic("boom") })
	if err == nil {
		t.Fatal("Guard swallowed a panic without reporting it")
	}
	if apierror.CodeOf(err) != apierror.CodeInternalError {
		t.Errorf("code = %s, want INTERNAL_ERROR", apierror.CodeOf(err))
	}
	if strings.Contains(err.Error(), "goroutine ") {
		t.Error("the stack reached the error text; it belongs in the log only")
	}
}

// TestSuperviseRestartsButNotDuringShutdown covers both halves of the supervision contract.
func TestSuperviseRestartsButNotDuringShutdown(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	runs := 0

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := runtime.Supervise(ctx, "flaky", quiet(), time.Millisecond,
		func(ctx context.Context) error {
			mu.Lock()
			runs++
			n := runs
			mu.Unlock()
			if n < 3 {
				return errors.New("transient")
			}
			<-ctx.Done()
			return ctx.Err()
		})

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return runs >= 3
	})

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := stop(stopCtx); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// Context cancellation must not be retried: restarting during shutdown would stop the process
	// from ever stopping.
	mu.Lock()
	after := runs
	mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if runs != after {
		t.Errorf("the loop restarted after cancellation: %d -> %d", after, runs)
	}
}

// TestStampFallsBackToTheEmbeddedVCSStamp asserts a locally-built binary still knows its commit.
//
// Without the fallback every local build reports "dev/unknown", and the first question of every
// local debugging session — "is this the code I am reading?" — is unanswerable.
func TestStampFallsBackToTheEmbeddedVCSStamp(t *testing.T) {
	t.Parallel()
	explicit := runtime.Stamp("1.2.3", "abc123", "2026-08-26T00:00:00Z")
	if explicit.Version != "1.2.3" || explicit.Commit != "abc123" {
		t.Errorf("ldflags values were not preferred: %+v", explicit)
	}
	fallback := runtime.Stamp("", "", "")
	if fallback.Version != "dev" {
		t.Errorf("version = %q, want dev", fallback.Version)
	}
	if fallback.Commit == "" {
		t.Error("commit is empty; it must fall back to the embedded stamp or to \"unknown\"")
	}
	if len(fallback.LogAttrs()) < 3 {
		t.Error("LogAttrs omits part of the stamp")
	}
}

// TestRefuseSimulatorInProduction is the second of the three controls keeping the simulator out of
// production. The consequence of it failing is a payment that reports authorized and moved no
// money.
func TestRefuseSimulatorInProduction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		env     string
		enabled bool
		wantErr bool
	}{
		{"sandbox with simulator", "sandbox", true, false},
		{"sandbox without simulator", "sandbox", false, false},
		{"production without simulator", "production", false, false},
		{"production with simulator", "production", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, err := runtime.ParseEnvironment(tc.env)
			if err != nil {
				t.Fatalf("ParseEnvironment: %v", err)
			}
			err = runtime.RefuseSimulatorInProduction(env, tc.enabled)
			if tc.wantErr && err == nil {
				t.Fatal("the simulator was permitted in production")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
		})
	}
}

// TestParseEnvironmentRefusesAnUnknownValue asserts a typo in a manifest is a startup failure
// rather than a silent default.
//
// Defaulting an unrecognised environment to sandbox would run production traffic under sandbox
// rules — accepting test tokens, permitting the simulator — while every dashboard said production.
func TestParseEnvironmentRefusesAnUnknownValue(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "prod", "PRODUCTION ", "staging", "dev"} {
		_, err := runtime.ParseEnvironment(in)
		if in == "PRODUCTION " {
			if err != nil {
				t.Errorf("ParseEnvironment(%q) rejected a value that trims and lowercases to production", in)
			}
			continue
		}
		if err == nil {
			t.Errorf("ParseEnvironment(%q) was accepted", in)
		}
	}
}

// TestParseCountriesRejectsATypo asserts a bad country code fails at startup rather than silently
// narrowing where merchants may be onboarded.
func TestParseCountriesRejectsATypo(t *testing.T) {
	t.Parallel()
	got, err := runtime.ParseCountries(" de , FR ,gb ")
	if err != nil {
		t.Fatalf("valid list rejected: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("parsed %d countries, want 3", len(got))
	}
	if _, err := runtime.ParseCountries("DE,XX"); err == nil {
		t.Error("an unknown country code was accepted")
	}
	if _, err := runtime.ParseCountries(" , "); err == nil {
		t.Error("an empty list was accepted; onboarding would be impossible everywhere")
	}
}

// TestSplitListDropsEmpties asserts a trailing comma is not a broker named "".
func TestSplitListDropsEmpties(t *testing.T) {
	t.Parallel()
	got := runtime.SplitList("a, b ,,c,")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("SplitList returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("element %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestReportStartupFailureNamesEveryProblem asserts the operator-facing output enumerates every
// missing variable rather than the first.
//
// A loader that stops at the first turns a six-variable mistake into six failed rollouts.
func TestReportStartupFailureNamesEveryProblem(t *testing.T) {
	t.Parallel()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	code := runtime.ReportStartupFailure("configuration",
		apierror.New(apierror.CodeConfigurationInvalid, "configuration is incomplete").
			WithDetails(
				apierror.Detail{Field: "PP_DATABASE_URL", Message: "required environment variable is not set"},
				apierror.Detail{Field: "PP_REGION", Message: "required environment variable is not set"},
			))
	_ = w.Close()
	os.Stderr = orig

	out, _ := io.ReadAll(r)
	text := string(out)
	if code != runtime.ExitStartupFailure {
		t.Errorf("exit code = %d, want %d", code, runtime.ExitStartupFailure)
	}
	for _, want := range []string{"PP_DATABASE_URL", "PP_REGION", "configuration"} {
		if !strings.Contains(text, want) {
			t.Errorf("output does not mention %q:\n%s", want, text)
		}
	}
}

// --- helpers -------------------------------------------------------------------------------------

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// telemetryDouble records that the flush ran, and is the last thing the drain does.
type telemetryDouble struct{ onShutdown func() }

func (t telemetryDouble) Shutdown(context.Context) error {
	if t.onShutdown != nil {
		t.onShutdown()
	}
	return nil
}

// waitFor polls a condition rather than sleeping a fixed duration. A fixed sleep is either flaky on
// a loaded CI machine or slow on every run; polling is neither.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met within the deadline")
}
