package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/internal/platform/config"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Exit codes. They are constants because operators and CI both branch on them, and a numeric
// literal in nine main functions is nine chances to disagree.
const (
	// ExitOK is a clean shutdown.
	ExitOK = 0
	// ExitStartupFailure is a failure before serving began: bad configuration, an unreachable
	// dependency, a capability mismatch. It is 2 rather than 1 so that a crash-looping pod is
	// distinguishable in `kubectl describe` from a process that ran and then failed.
	ExitStartupFailure = 2
	// ExitRuntimeFailure is a failure after serving began.
	ExitRuntimeFailure = 1
)

// Budgets is one binary's shutdown budget, from docs/deployment.md §1.8.
//
// The numbers are per binary because the longest legitimate unit of work differs by an order of
// magnitude across the fleet: a webhook ingest is 50 ms and a payment orchestration is an 8-second
// gateway call plus retries. Using one number everywhere would either kill orchestrator work
// mid-flight or make a webhook pod take a minute to leave rotation.
type Budgets struct {
	// DrainDelay is the pause between failing readiness and stopping the acceptor, covering
	// endpoint propagation. It is *not* the preStop sleep — the preStop hook runs before SIGTERM
	// reaches us at all — it is the second, in-process half of the same race.
	DrainDelay time.Duration

	// InFlight is how long in-flight work may take to finish once the acceptor has stopped. It
	// must exceed the longest legitimate unit of work: for the orchestrator that is a full
	// gateway call plus a retry, because **an aborted gateway call is a TIMEOUT_UNKNOWN we then
	// have to reconcile** — the platform pays wall-clock grace to avoid manufacturing ambiguity.
	InFlight time.Duration

	// Resources bounds closing pools, producers and consumers.
	Resources time.Duration

	// TelemetryFlush bounds the final export. It is last because everything above it is worth
	// observing, and it is bounded because a collector that is down must not hold the process
	// past its termination grace period.
	TelemetryFlush time.Duration
}

// Total is the sum, which must stay below the pod's terminationGracePeriodSeconds minus its
// preStop sleep. [Budgets.Validate] checks that the caller has left headroom.
func (b Budgets) Total() time.Duration {
	return b.DrainDelay + b.InFlight + b.Resources + b.TelemetryFlush
}

// Validate reports a budget that cannot fit inside the pod's grace period.
//
// It is a startup check rather than a comment because the arithmetic is exactly the thing that
// silently stops holding when somebody raises a timeout: the manifest says 75 seconds, the code
// adds up to 80, and the difference is a SIGKILL in the middle of a drain that nobody notices
// until they read a graph of 502s during deploys.
func (b Budgets) Validate(gracePeriod, preStop time.Duration) error {
	if gracePeriod <= 0 {
		return nil
	}
	available := gracePeriod - preStop
	if b.Total() >= available {
		return apierror.Newf(apierror.CodeConfigurationInvalid,
			"shutdown budget %s does not fit in the %s available after a %s preStop sleep",
			b.Total(), available, preStop)
	}
	return nil
}

// Standard budget sets, one per deployable, transcribed from docs/deployment.md §1.8.
//
// Each is a function rather than a package variable so a caller cannot mutate the table for every
// other binary in the process by editing the value it was handed.

// APIBudgets is `payment-api` and `control-plane-api`: grace 75 s / preStop 15 s.
func APIBudgets() Budgets {
	return Budgets{DrainDelay: 5 * time.Second, InFlight: 40 * time.Second,
		Resources: 5 * time.Second, TelemetryFlush: 5 * time.Second}
}

// OrchestratorBudgets is `payment-orchestrator`: grace 90 s / preStop 10 s. The in-flight budget
// covers an 8-second gateway call plus two retries plus L6 and the commit, because aborting one
// manufactures a TIMEOUT_UNKNOWN.
func OrchestratorBudgets() Budgets {
	return Budgets{DrainDelay: 5 * time.Second, InFlight: 60 * time.Second,
		Resources: 5 * time.Second, TelemetryFlush: 5 * time.Second}
}

// IngressBudgets is `webhook-ingress`: grace 45 s / preStop 15 s. Requests are ≤ 50 ms, so the
// grace is almost entirely endpoint propagation.
func IngressBudgets() Budgets {
	return Budgets{DrainDelay: 5 * time.Second, InFlight: 10 * time.Second,
		Resources: 5 * time.Second, TelemetryFlush: 5 * time.Second}
}

// WorkerBudgets is `workflow-worker`: grace 120 s / preStop 5 s. The long in-flight budget lets
// the current activity finish; leases are released explicitly during the resource phase so
// another worker resumes in milliseconds rather than waiting out the lease TTL.
func WorkerBudgets() Budgets {
	return Budgets{DrainDelay: 0, InFlight: 90 * time.Second,
		Resources: 10 * time.Second, TelemetryFlush: 5 * time.Second}
}

// RelayBudgets is `outbox-relay`: grace 60 s / preStop 5 s. An unfinished batch is safe — rows
// stay unmarked and another relay reclaims them when the lock is released by the connection
// closing — so the budget only needs to cover finishing and marking the current batch.
func RelayBudgets() Budgets {
	return Budgets{DrainDelay: 0, InFlight: 30 * time.Second,
		Resources: 10 * time.Second, TelemetryFlush: 5 * time.Second}
}

// ConsumerBudgets is `event-consumer`: grace 90 s / preStop 5 s. The in-flight budget covers
// finishing the current message and committing its offset; leaving the group cleanly during the
// resource phase triggers a cooperative rebalance instead of a session-timeout stall.
func ConsumerBudgets() Budgets {
	return Budgets{DrainDelay: 0, InFlight: 45 * time.Second,
		Resources: 20 * time.Second, TelemetryFlush: 5 * time.Second}
}

// Component is one thing the process starts and stops.
//
// Start must not block: it either begins serving in a goroutine it owns and returns, or it
// returns the error that stopped it from doing so. A Start that blocks would make the
// registration order into an execution order, and the process would never reach its second
// component.
type Component struct {
	// Name identifies the component in the startup and shutdown log lines.
	Name string
	// Start begins the work. It must return promptly.
	Start func(ctx context.Context) error
	// Stop halts the work under the supplied deadline. It must be safe to call on a component
	// that never started and safe to call twice, because a composition root that stops from both
	// a signal path and a defer is the normal shape.
	Stop func(ctx context.Context) error
}

// Lifecycle runs a process: start components in order, wait for a signal, drain in reverse.
type Lifecycle struct {
	// Service and Version identify the binary in logs. They come from the build stamp.
	Service string
	Version string

	// Logger receives lifecycle lines. Request logging is the transport's job.
	Logger *slog.Logger

	// Budgets is this binary's drain arithmetic.
	Budgets Budgets

	// Telemetry is flushed last. Nil skips the flush, which is correct only in a test.
	Telemetry interface {
		Shutdown(ctx context.Context) error
	}

	// OnDrainStart is called the instant a shutdown signal arrives, before anything is closed.
	// It is where a composition root fails readiness. It must not block.
	OnDrainStart func()

	components []Component
	draining   atomic.Bool
}

// Add registers a component. Registration is not start: nothing runs until [Lifecycle.Run].
//
// The order matters and is the order of the calls: components start in registration order and
// stop in reverse, which is what makes "close in reverse dependency order" a property of the
// wiring rather than something each binary has to remember.
func (l *Lifecycle) Add(name string, start, stop func(context.Context) error) {
	l.components = append(l.components, Component{Name: name, Start: start, Stop: stop})
}

// Draining reports whether shutdown has begun. The readiness handler reads it.
func (l *Lifecycle) Draining() bool { return l.draining.Load() }

// Run starts every component, blocks until a shutdown signal or a component failure, then drains.
//
// It returns the process exit code, so a `main` is `os.Exit(run())` and nothing else — which is
// what lets every deferred cleanup in `run` actually execute. `os.Exit` inside `run` would skip
// them, and the skipped one is always the pool close.
func (l *Lifecycle) Run(ctx context.Context) int {
	if l.Logger == nil {
		l.Logger = slog.Default()
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	failed := make(chan error, len(l.components))
	started := 0
	for _, c := range l.components {
		if c.Start == nil {
			started++
			continue
		}
		if err := c.Start(ctx); err != nil {
			l.Logger.Error("component failed to start",
				slog.String("component", c.Name),
				slog.String(telemetry.KeyErrorMessage, err.Error()))
			// Stop what did start, in reverse, before returning. A process that fails halfway
			// through startup and exits without closing leaves a database connection and a
			// consumer-group membership behind, and the next pod inherits a rebalance.
			l.shutdown(context.WithoutCancel(ctx), started)
			return ExitStartupFailure
		}
		started++
	}

	l.Logger.Info("started",
		slog.String("service", l.Service),
		slog.String("version", l.Version),
		slog.Int("components", started),
		slog.Duration("shutdown_budget", l.Budgets.Total()),
	)

	select {
	case <-ctx.Done():
		l.Logger.Info("shutdown signal received; draining")
	case err := <-failed:
		l.Logger.Error("component failed at runtime",
			slog.String(telemetry.KeyErrorMessage, err.Error()))
		l.shutdown(context.WithoutCancel(ctx), started)
		return ExitRuntimeFailure
	}

	l.shutdown(context.WithoutCancel(ctx), started)
	return ExitOK
}

// shutdown performs the §2.5 sequence over the first n registered components.
func (l *Lifecycle) shutdown(ctx context.Context, n int) {
	// 1. Fail readiness *first*, before anything is closed. Endpoint removal is asynchronous, so
	//    the announcement has to lead.
	l.draining.Store(true)
	if l.OnDrainStart != nil {
		l.OnDrainStart()
	}

	// 2. Wait out endpoint propagation. During this window the process is still serving normally:
	//    requests that arrive are answered, they are simply no longer being sent.
	if l.Budgets.DrainDelay > 0 {
		l.Logger.Info("readiness failed; waiting for endpoint propagation",
			slog.Duration("delay", l.Budgets.DrainDelay))
		timer := time.NewTimer(l.Budgets.DrainDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
	}

	// 3. Stop components in reverse registration order, which is reverse dependency order.
	deadline := time.Now().Add(l.Budgets.InFlight + l.Budgets.Resources)
	for i := n - 1; i >= 0; i-- {
		c := l.components[i]
		if c.Stop == nil {
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// The budget is spent. Stopping with an already-expired context still lets a
			// component take its "we are out of time" path — closing hard rather than draining —
			// which is strictly better than skipping the call and leaving the resource open.
			remaining = time.Millisecond
		}
		stopCtx, cancel := context.WithTimeout(ctx, remaining)
		start := time.Now()
		err := c.Stop(stopCtx)
		cancel()

		attrs := []any{
			slog.String("component", c.Name),
			slog.Duration("took", time.Since(start)),
		}
		if err != nil {
			l.Logger.Warn("component did not stop cleanly",
				append(attrs, slog.String(telemetry.KeyErrorMessage, err.Error()))...)
			continue
		}
		l.Logger.Info("component stopped", attrs...)
	}

	// 4. Telemetry last, so every line above was exported.
	if l.Telemetry != nil {
		flushCtx, cancel := context.WithTimeout(ctx, l.Budgets.TelemetryFlush)
		defer cancel()
		if err := l.Telemetry.Shutdown(flushCtx); err != nil {
			// Logged, not returned: a failed trace export is not a failed shutdown, and exiting
			// non-zero because a collector was down would make every deploy during a collector
			// outage look like a crash.
			l.Logger.Warn("telemetry flush did not complete",
				slog.String(telemetry.KeyErrorMessage, err.Error()))
		}
	}
	l.Logger.Info("stopped", slog.String("service", l.Service))
}

// BuildInfo is the build stamp, populated from -ldflags.
//
// It is a struct rather than three loose variables because it is logged as a unit, reported by
// the probes as a unit, and compared against a deployment manifest as a unit — and because three
// package-level strings in nine main packages is nine places to misspell `-X`.
type BuildInfo struct {
	// Version is the semantic version or the image tag.
	Version string
	// Commit is the git SHA the binary was built from. This is the field that answers "is the
	// running pod the code I am reading?", which no version string reliably answers once a tag
	// has been moved.
	Commit string
	// Date is the build timestamp, RFC 3339.
	Date string
	// GoVersion and Modified come from the embedded build info rather than from ldflags.
	// Modified is true when the working tree was dirty, which is the fact that turns "it works on
	// my machine" into a question with an answer.
	GoVersion string
	Modified  bool
}

// Stamp resolves the build information, preferring the ldflags values and falling back to Go's
// own embedded VCS stamp.
//
// The fallback matters for a locally-built binary: `go build ./cmd/payment-api` with no ldflags
// still knows its commit, because the toolchain embeds it. Without the fallback every local build
// reports "dev/unknown" and the first question of every local debugging session is unanswerable.
func Stamp(version, commit, date string) BuildInfo {
	b := BuildInfo{Version: orDefault(version, "dev"), Commit: commit, Date: date}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		b.Commit = orDefault(b.Commit, "unknown")
		return b
	}
	b.GoVersion = info.GoVersion
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Commit = orDefault(b.Commit, s.Value)
		case "vcs.time":
			b.Date = orDefault(b.Date, s.Value)
		case "vcs.modified":
			b.Modified = s.Value == "true"
		}
	}
	b.Commit = orDefault(b.Commit, "unknown")
	b.Date = orDefault(b.Date, "unknown")
	return b
}

// LogAttrs renders the stamp for a startup line.
func (b BuildInfo) LogAttrs() []any {
	attrs := []any{
		slog.String(telemetry.KeyVersion, b.Version),
		slog.String("commit", b.Commit),
		slog.String("build_date", b.Date),
	}
	if b.GoVersion != "" {
		attrs = append(attrs, slog.String("go_version", b.GoVersion))
	}
	if b.Modified {
		// Surfaced deliberately: a binary built from a dirty tree is a binary whose commit hash
		// does not describe it, and knowing that at the top of an incident saves an hour.
		attrs = append(attrs, slog.Bool("vcs_modified", true))
	}
	return attrs
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// LoadConfig loads a configuration struct from the environment and reports *every* problem.
//
// # Why every problem rather than the first
//
// A loader that stops at the first missing variable turns a misconfigured deployment into a
// sequence of failed rollouts: fix one, redeploy, discover the next. With six missing variables
// that is six deploys and an afternoon. internal/platform/config collects them; this function's
// job is to render the collection as something an operator can act on without reading Go.
//
// The rendering goes to stderr rather than to the logger because the logger may not exist yet:
// configuration is loaded before telemetry, precisely so that a telemetry misconfiguration is
// itself reportable.
func LoadConfig(dest any) error {
	if err := config.LoadFromEnv(dest); err != nil {
		return err
	}
	return nil
}

// ReportStartupFailure prints an actionable message to stderr and returns the exit code.
//
// "Actionable" means it names each missing or malformed variable on its own line, because the
// person reading it is looking at a crash-looping pod and needs to know what to set, not what
// went wrong internally.
func ReportStartupFailure(stage string, err error) int {
	e := apierror.From(err)
	fmt.Fprintf(os.Stderr, "startup failed during %s: %s\n", stage, e.Message)
	for _, d := range e.Details {
		field := d.Field
		if field == "" {
			field = "(request)"
		}
		fmt.Fprintf(os.Stderr, "  - %s: %s\n", field, d.Message)
	}
	if len(e.Details) > 0 {
		fmt.Fprintln(os.Stderr,
			"\nset the variables above and restart; every problem found is listed, "+
				"so one fix-and-redeploy cycle is enough")
	}
	return ExitStartupFailure
}

// LogRedactedConfig writes the effective configuration at startup, with secrets masked.
//
// # Why log it at all
//
// "Which configuration is this pod actually running?" is the first question of most incidents,
// and the honest answer is not in the manifest — a manifest describes what was applied, not what
// the process parsed, and the two differ whenever a default filled in or a value was overridden.
// Logging the parsed values makes the answer a grep.
//
// # Why it is safe
//
// internal/platform/config masks on the *name*, using the same pattern the admission policy and
// the CI secret scanner use, so a value that would be rejected in a pod spec is masked here.
// Matching on the name rather than the value is the right way round: entropy heuristics produce
// both false negatives (a short password) and false positives (a base64 build hash), whereas a
// name was chosen by a developer who knew what the field was for.
func LogRedactedConfig(logger *slog.Logger, cfg any) {
	for _, entry := range config.Redacted(cfg) {
		logger.Info("config", slog.String("name", entry.Name), slog.String("value", entry.Value))
	}
}

// Guard runs fn and converts a panic into an error, so a background loop started by a composition
// root cannot take the process down.
//
// The composition roots start goroutines that outlive the function that created them — a poll
// loop, a refresh ticker — and an unrecovered panic in one of those is a process exit with no
// drain: in-flight payments abandoned, leases held until they expire, offsets uncommitted. This
// wrapper is what makes a bug in a background loop a logged error instead.
func Guard(name string, logger *slog.Logger, fn func() error) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			logger.Error("panic in background component",
				slog.String("component", name),
				slog.String(telemetry.KeyStack, string(debug.Stack())))
			err = apierror.Newf(apierror.CodeInternalError, "component %s panicked", name)
		}
	}()
	return fn()
}

// Supervise runs fn in a goroutine and restarts it with backoff when it returns an error.
//
// # Why a background loop is restarted rather than being allowed to stop
//
// A poll loop that returns an error and is never restarted is a silently dead subsystem: the
// process stays up, the probes stay green, and the outbox simply stops being relayed. Restarting
// with backoff turns that into a visible, self-healing degradation. Context cancellation is the
// one exit that is *not* retried, because that is shutdown and restarting during shutdown would
// prevent the process from ever stopping.
func Supervise(ctx context.Context, name string, logger *slog.Logger,
	backoff time.Duration, fn func(context.Context) error) func(context.Context) error {
	var wg sync.WaitGroup
	done := make(chan struct{})
	loopCtx, cancel := context.WithCancel(ctx)

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for {
			err := Guard(name, logger, func() error { return fn(loopCtx) })
			if loopCtx.Err() != nil || errors.Is(err, context.Canceled) {
				return
			}
			if err != nil {
				logger.Error("background component failed; restarting",
					slog.String("component", name),
					slog.Duration("backoff", backoff),
					slog.String(telemetry.KeyErrorMessage, err.Error()))
			}
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-loopCtx.Done():
				timer.Stop()
				return
			}
		}
	}()

	return func(stopCtx context.Context) error {
		cancel()
		select {
		case <-done:
			wg.Wait()
			return nil
		case <-stopCtx.Done():
			// The loop did not observe cancellation in time. Returning rather than waiting keeps
			// the drain bounded; the goroutine is still cancelled and will exit, and the process
			// is about to end regardless.
			return apierror.Newf(apierror.CodeInternalError,
				"component %s did not stop within its budget", name)
		}
	}
}
