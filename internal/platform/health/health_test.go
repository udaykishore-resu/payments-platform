package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/health"
)

func ok(context.Context) error  { return nil }
func bad(context.Context) error { return errors.New("dependency unavailable") }

func newRegistry(t *testing.T, ttl time.Duration) (*health.Registry, *shared.FixedClock) {
	t.Helper()
	clock := &shared.FixedClock{T: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
	return health.New(health.Options{
		Clock:    clock,
		CacheTTL: ttl,
		Service:  "payment-api",
		Version:  "1.2.3",
	}), clock
}

// The headline property: liveness must never depend on a downstream. Assert it by failing every
// dependency and checking liveness still passes.
//
// The failure this prevents: a database blip fails liveness on every pod at once, the kubelet
// restarts every pod at once, they all come up cold and hammer the database that was merely
// slow. The health check turns a degradation into an outage.
func TestLivenessDoesNotTouchDownstreams(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t, -1)

	var downstreamCalls atomic.Int64
	failing := func(context.Context) error {
		downstreamCalls.Add(1)
		return errors.New("everything is on fire")
	}

	// A realistic wiring: every dependency the service has, all of them failing.
	r.RegisterReadiness("postgres", true, failing)
	r.RegisterReadiness("redis", true, failing)
	r.RegisterReadiness("kafka", true, failing)
	r.RegisterReadiness("config-snapshot", true, failing)
	r.RegisterReadiness("risk-scorer", false, failing)
	r.RegisterStartup("warm-config", failing)
	// And a process-internal liveness check, which is the only kind allowed.
	r.RegisterLiveness("event-loop", ok)

	live := r.Live(context.Background())
	if live.Status != health.StatusUp {
		t.Fatalf("liveness must not fail because downstreams are down: %+v", live)
	}
	if downstreamCalls.Load() != 0 {
		t.Fatalf("liveness called %d downstream checks; it must call none", downstreamCalls.Load())
	}
	if len(live.Checks) != 1 || live.Checks[0].Name != "event-loop" {
		t.Fatalf("liveness ran the wrong checks: %+v", live.Checks)
	}

	// Readiness, by contrast, is supposed to notice.
	ready := r.Ready(context.Background())
	if ready.Status != health.StatusDown {
		t.Fatalf("readiness must fail when its dependencies are down: %+v", ready)
	}
	if downstreamCalls.Load() == 0 {
		t.Fatal("readiness should have called the dependencies")
	}

	// And the two must return different HTTP statuses, because they mean different things to
	// the kubelet.
	if got := probe(t, r, health.KindLiveness); got != http.StatusOK {
		t.Fatalf("/livez = %d, want 200", got)
	}
	if got := probe(t, r, health.KindReadiness); got != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, want 503", got)
	}
}

func TestNoChecksMeansAlive(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t, -1)
	// An unconfigured service must not restart forever.
	if resp := r.Live(context.Background()); resp.Status != health.StatusUp {
		t.Fatalf("no liveness checks must mean alive: %+v", resp)
	}
	if resp := r.Ready(context.Background()); resp.Status != health.StatusUp {
		t.Fatalf("no readiness checks must mean ready: %+v", resp)
	}
}

func TestCriticalityDecidesSheddingVersusDegrading(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t, -1)
	r.RegisterReadiness("postgres", true, ok)
	r.RegisterReadiness("risk-scorer", false, bad)

	resp := r.Ready(context.Background())
	if resp.Status != health.StatusDegraded {
		t.Fatalf("an optional dependency's failure must degrade, not shed: %+v", resp)
	}
	// Degraded still returns 200: shedding capacity over an optional dependency is a
	// self-inflicted brownout.
	if got := probe(t, r, health.KindReadiness); got != http.StatusOK {
		t.Fatalf("degraded = %d, want 200", got)
	}

	crit, _ := newRegistry(t, -1)
	crit.RegisterReadiness("postgres", true, bad)
	if resp := crit.Ready(context.Background()); resp.Status != health.StatusDown {
		t.Fatalf("a critical dependency's failure must shed: %+v", resp)
	}
}

// A probe storm must not amplify load onto a dependency that is already struggling.
func TestCachingCollapsesAProbeStorm(t *testing.T) {
	t.Parallel()
	r, clock := newRegistry(t, time.Second)
	var calls atomic.Int64
	r.RegisterReadiness("postgres", true, func(context.Context) error {
		calls.Add(1)
		return nil
	})

	for i := 0; i < 50; i++ {
		if resp := r.Ready(context.Background()); resp.Status != health.StatusUp {
			t.Fatalf("probe %d: %+v", i, resp)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("50 sequential probes made %d dependency calls, want 1", calls.Load())
	}
	// The result must say it was cached rather than pretending to be fresh.
	resp := r.Ready(context.Background())
	if !resp.Checks[0].Cached {
		t.Fatal("a cached result must be labelled as one")
	}

	// Past the TTL the check runs again: the cache collapses bursts, it does not make the probe
	// stale.
	clock.Advance(2 * time.Second)
	if resp := r.Ready(context.Background()); resp.Checks[0].Cached {
		t.Fatal("past the TTL the check must run again")
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestConcurrentProbesShareOneExecution(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t, time.Minute)
	var (
		calls   atomic.Int64
		release = make(chan struct{})
	)
	r.RegisterReadiness("postgres", true, func(context.Context) error {
		calls.Add(1)
		<-release
		return nil
	})

	const n = 32
	var wg sync.WaitGroup
	results := make([]health.Response, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = r.Ready(context.Background())
		}(i)
	}
	// Give the goroutines a chance to pile onto the in-flight check, then let it finish.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("%d concurrent probes made %d dependency calls, want 1", n, calls.Load())
	}
	for i, resp := range results {
		if resp.Status != health.StatusUp {
			t.Fatalf("probe %d: %+v", i, resp)
		}
	}
}

func TestPerCheckTimeout(t *testing.T) {
	t.Parallel()
	r := health.New(health.Options{Clock: shared.SystemClock{}, CacheTTL: time.Nanosecond})
	r.Register(health.Check{
		Name: "hanging", Kind: health.KindReadiness, Critical: true, Timeout: 20 * time.Millisecond,
		Fn: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})
	fast := make(chan struct{})
	r.Register(health.Check{
		Name: "fast", Kind: health.KindReadiness, Critical: true, Timeout: time.Second,
		Fn: func(context.Context) error { close(fast); return nil },
	})

	start := time.Now()
	resp := r.Ready(context.Background())
	elapsed := time.Since(start)

	if resp.Status != health.StatusDown {
		t.Fatalf("a hanging check must fail rather than hang: %+v", resp)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the probe took %v; a per-check timeout must bound it", elapsed)
	}
	// The check after the slow one must still run on its own budget, rather than inheriting a
	// budget the slow one already spent.
	select {
	case <-fast:
	default:
		t.Fatal("a check after a timed-out one must still run")
	}
	byName := map[string]health.Result{}
	for _, c := range resp.Checks {
		byName[c.Name] = c
	}
	if byName["hanging"].Status != health.StatusDown || byName["fast"].Status != health.StatusUp {
		t.Fatalf("results = %+v", resp.Checks)
	}
}

// A bug in a diagnostic must not kill the thing being diagnosed.
func TestAPanickingCheckFailsRatherThanCrashing(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t, -1)
	r.RegisterReadiness("nil-map", true, func(context.Context) error {
		var m map[string]string
		m["boom"] = "x" //nolint:staticcheck,govet // SA5000/nilness: writing to a nil map is exactly the panic this test induces, and surviving it is the assertion
		return nil
	})
	resp := r.Ready(context.Background())
	if resp.Status != health.StatusDown {
		t.Fatalf("a panicking check must report down: %+v", resp)
	}
	if resp.Checks[0].Error == "" {
		t.Fatal("the failure must be explained")
	}
	// The process is still here, which is the assertion.
	if live := r.Live(context.Background()); live.Status != health.StatusUp {
		t.Fatalf("the process must still be alive: %+v", live)
	}
}

// Startup exists so a slow initialisation is not killed by liveness's tighter deadline, and it
// latches so that it does not become a second liveness probe.
func TestStartupLatches(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t, -1)
	var warm atomic.Bool
	var calls atomic.Int64
	r.RegisterStartup("warm-config", func(context.Context) error {
		calls.Add(1)
		if !warm.Load() {
			return errors.New("snapshot not yet warm")
		}
		return nil
	})

	if resp := r.Started(context.Background()); resp.Status != health.StatusDown {
		t.Fatalf("startup must fail while initialising: %+v", resp)
	}
	if got := probe(t, r, health.KindStartup); got != http.StatusServiceUnavailable {
		t.Fatalf("/startupz = %d, want 503", got)
	}

	warm.Store(true)
	if resp := r.Started(context.Background()); resp.Status != health.StatusUp {
		t.Fatalf("startup must pass once warm: %+v", resp)
	}
	before := calls.Load()

	// Now the dependency breaks. Startup must keep reporting up: initialisation finished once,
	// and a startup check that kept evaluating would be a liveness probe with no discipline
	// about dependencies.
	warm.Store(false)
	for i := 0; i < 5; i++ {
		if resp := r.Started(context.Background()); resp.Status != health.StatusUp {
			t.Fatalf("startup must latch: %+v", resp)
		}
	}
	if calls.Load() != before {
		t.Fatalf("a latched startup probe must not re-run its checks: %d then %d", before, calls.Load())
	}
}

func TestJSONResponseShape(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t, -1)
	r.RegisterReadiness("postgres", true, ok)
	r.RegisterReadiness("redis", true, bad)

	rec := httptest.NewRecorder()
	r.Handler(health.KindReadiness).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	// A cached readiness response is a load balancer routing to a pod that said yes a minute ago.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}

	var resp health.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not decodable: %v (%s)", err, rec.Body.String())
	}
	switch {
	case resp.Status != health.StatusDown:
		t.Fatalf("status = %q", resp.Status)
	case resp.Kind != health.KindReadiness:
		t.Fatalf("kind = %q; a response pasted into a ticket must be self-describing", resp.Kind)
	case resp.Service != "payment-api" || resp.Version != "1.2.3":
		t.Fatalf("service/version = %q/%q", resp.Service, resp.Version)
	case len(resp.Checks) != 2:
		t.Fatalf("checks = %+v", resp.Checks)
	}
	// Stable ordering, so two responses diff cleanly.
	if resp.Checks[0].Name != "postgres" || resp.Checks[1].Name != "redis" {
		t.Fatalf("checks are not stably ordered: %+v", resp.Checks)
	}
	if resp.Checks[1].Error == "" {
		t.Fatal("a failing check must say why")
	}
	if resp.Checks[0].Error != "" {
		t.Fatal("a passing check must not carry an error")
	}
}

func TestMountsTheThreeConventionalPaths(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t, -1)
	r.RegisterReadiness("postgres", true, bad)
	r.RegisterLiveness("event-loop", ok)

	mux := http.NewServeMux()
	r.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for path, want := range map[string]int{
		"/livez":    http.StatusOK,
		"/readyz":   http.StatusServiceUnavailable,
		"/startupz": http.StatusOK,
	} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s = %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func TestRegisterRejectsProgrammingErrors(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t, -1)
	for _, c := range []health.Check{
		{Name: "", Kind: health.KindLiveness, Fn: ok},
		{Name: "x", Kind: health.KindLiveness},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("registering %+v must panic at init", c)
				}
			}()
			r.Register(c)
		}()
	}
}

func TestNamesAreStable(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t, -1)
	r.RegisterReadiness("redis", true, ok)
	r.RegisterReadiness("kafka", true, ok)
	r.RegisterReadiness("postgres", true, ok)
	r.RegisterLiveness("event-loop", ok)

	got := r.Names(health.KindReadiness)
	want := []string{"kafka", "postgres", "redis"}
	if len(got) != len(want) {
		t.Fatalf("Names = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names = %v, want %v", got, want)
		}
	}
	if live := r.Names(health.KindLiveness); len(live) != 1 || live[0] != "event-loop" {
		t.Fatalf("liveness names = %v", live)
	}
}

func probe(t *testing.T, r *health.Registry, kind health.Kind) int {
	t.Helper()
	rec := httptest.NewRecorder()
	r.Handler(kind).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe", nil))
	return rec.Code
}
