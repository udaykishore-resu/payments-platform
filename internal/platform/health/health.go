// Package health implements the three Kubernetes probe types with the semantics that make them
// safe, and a registry of named checks behind them.
//
// # The three probes are not three names for one thing
//
// The single most expensive mistake in this area is wiring one handler to all three probe paths.
// It looks harmless — "is the service healthy?" — and it converts every dependency blip into a
// self-inflicted outage. The three answer genuinely different questions, and the difference is
// what each probe's failure *does*:
//
//   - Liveness → "restart me". Must depend on nothing outside this process. If liveness touches
//     the database, then a database blip fails liveness on every pod at once, the kubelet
//     restarts every pod at once, every pod comes up cold with an empty cache and an unwarmed
//     connection pool, and they all hammer the database that was merely slow. A degradation
//     becomes an outage, and the outage is caused by the health check. This is not hypothetical;
//     it is one of the most common ways a platform amplifies its own incident.
//
//   - Readiness → "stop sending me traffic". This one *does* depend on downstreams, because a
//     pod that cannot reach the database genuinely cannot serve. Failing readiness removes the
//     pod from the load balancer and leaves the process running, so it recovers without a cold
//     start. The cost of a false readiness failure is bounded: less capacity. The cost of a false
//     liveness failure is a restart.
//
//   - Startup → "not yet, and do not count this against liveness". It exists so that a service
//     with slow initialisation (a configuration snapshot to warm, a JWKS set to fetch, a
//     connection pool to fill) can take the time it needs without the liveness probe's much
//     tighter deadline killing it mid-warmup — the failure mode where a pod is restarted
//     forever and never finishes starting.
//
// # Caching, because a probe storm must not become load
//
// Probes are frequent and their number scales with the fleet. Three probes per pod every few
// seconds, across a few hundred pods, is a meaningful query rate on its own — and it lands
// precisely when a dependency is already struggling, because that is when everything is being
// probed hardest. Every check's result is therefore cached for a short TTL and shared across
// concurrent probes, so a burst of readiness requests produces one dependency call, not one per
// request.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

// Kind is the probe a check belongs to.
type Kind string

const (
	// KindLiveness answers "should this process be restarted?". A liveness check must be
	// process-internal: a deadlock detector, a "the main loop is still ticking" heartbeat, a
	// panic latch. It must never call a dependency.
	KindLiveness Kind = "liveness"
	// KindReadiness answers "can this pod serve traffic?". Readiness checks name the
	// dependencies required to serve, and only those.
	KindReadiness Kind = "readiness"
	// KindStartup answers "has initialisation finished?". Startup checks stop reporting once
	// the pod is up, which is what lets the liveness probe's tighter deadline take over safely.
	KindStartup Kind = "startup"
)

// Status is one check's result.
type Status string

const (
	// StatusUp means the check passed.
	StatusUp Status = "up"
	// StatusDown means the check failed. For readiness this removes the pod from service.
	StatusDown Status = "down"
	// StatusDegraded means the check passed but something is wrong — a snapshot past its
	// bounded staleness, a circuit half-open. It does not fail the probe. The distinction
	// exists because "worse than ideal" and "cannot serve" have different remedies, and
	// collapsing them means either shedding a pod that works or hiding a problem that matters.
	StatusDegraded Status = "degraded"
)

// CheckFunc is one health check. It must respect ctx, which carries the per-check timeout, and
// it must not panic — a panic in a probe handler takes down the process the probe was meant to
// protect.
type CheckFunc func(ctx context.Context) error

// Check is a named, registered check.
type Check struct {
	Name string
	Kind Kind
	Fn   CheckFunc
	// Timeout bounds this check. Zero uses the registry's default. A check without a timeout is
	// a check that can hang the probe handler, and a hung probe handler is indistinguishable
	// from a failing one to the kubelet — except that it also occupies a goroutine per probe.
	Timeout time.Duration
	// Critical marks a readiness check whose failure fails the probe. A non-critical failure
	// reports degraded and keeps the pod in service; that is the right default for a dependency
	// the service can work without, such as an optional risk scorer, because shedding capacity
	// over an optional dependency is a self-inflicted brownout.
	Critical bool
}

// Result is one check's outcome, as rendered in the probe response.
type Result struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	// Error is the failure text. It is included because a probe response is read by operators
	// and by nothing else — the endpoint is not exposed at the edge (security.md §2.1) — and a
	// probe that says "down" without saying why costs an SSH session that this platform
	// deliberately does not have.
	Error string `json:"error,omitempty"`
	// DurationMS is how long the check took, which is how a "slow but passing" dependency shows
	// up before it becomes a failing one.
	DurationMS int64 `json:"durationMs"`
	// CheckedAt is when this result was produced. With caching it may be older than the request,
	// and saying so is more useful than pretending otherwise.
	CheckedAt time.Time `json:"checkedAt"`
	// Cached reports whether the result was served from the cache rather than freshly computed.
	Cached bool `json:"cached,omitempty"`
}

// Response is the probe's JSON body.
type Response struct {
	Status Status `json:"status"`
	// Kind names which probe produced this, so a response pasted into a ticket is
	// self-describing.
	Kind     Kind     `json:"kind"`
	Checks   []Result `json:"checks"`
	Service  string   `json:"service,omitempty"`
	Version  string   `json:"version,omitempty"`
	Duration int64    `json:"durationMs"`
}

// Registry holds the checks and serves the probes.
type Registry struct {
	clock          shared.Clock
	defaultTimeout time.Duration
	cacheTTL       time.Duration
	service        string
	version        string

	mu     sync.RWMutex
	checks []Check

	cacheMu sync.Mutex
	cache   map[string]*cacheEntry

	// started latches the startup probe. Once startup has succeeded once it keeps succeeding:
	// Kubernetes stops calling it, and a startup check that could fail later would be a second,
	// tighter liveness probe with none of liveness's discipline about dependencies.
	started sync.Once
	startOK bool
}

type cacheEntry struct {
	// inflight is what makes concurrent probes share one execution: the second caller waits on
	// the first's result rather than starting its own. This is the property that turns a probe
	// storm into one dependency call.
	inflight *sync.WaitGroup
	result   Result
	expires  time.Time
}

// Options configure a Registry.
type Options struct {
	Clock shared.Clock
	// DefaultTimeout bounds any check that does not specify one.
	DefaultTimeout time.Duration
	// CacheTTL is how long a result is reused. Short enough that a probe reflects reality, long
	// enough that a burst collapses into one call. Zero uses the default; a negative value
	// disables caching entirely, which is occasionally what a test wants and never what a
	// production deployment wants.
	CacheTTL time.Duration
	Service  string
	Version  string
}

// Defaults for Options. The cache TTL is deliberately well under a typical probe period so that
// a check still runs on most probes; its job is to collapse *bursts*, not to make the probe
// stale.
const (
	DefaultTimeout  = 2 * time.Second
	DefaultCacheTTL = 1 * time.Second
)

// New builds a registry.
func New(opts Options) *Registry {
	if opts.Clock == nil {
		opts.Clock = shared.SystemClock{}
	}
	if opts.DefaultTimeout <= 0 {
		opts.DefaultTimeout = DefaultTimeout
	}
	switch {
	case opts.CacheTTL < 0:
		opts.CacheTTL = 0 // explicitly disabled
	case opts.CacheTTL == 0:
		opts.CacheTTL = DefaultCacheTTL
	}
	return &Registry{
		clock:          opts.Clock,
		defaultTimeout: opts.DefaultTimeout,
		cacheTTL:       opts.CacheTTL,
		service:        opts.Service,
		version:        opts.Version,
		cache:          map[string]*cacheEntry{},
	}
}

// Register adds a check.
//
// It panics on a liveness check registered with a nil function or an empty name, because both are
// programming errors detectable at init — the one place baseline §4 rule 9 permits a panic. It
// does not, and cannot, verify that a liveness check touches no dependency: that is enforced by
// review and by TestLivenessDoesNotTouchDownstreams, which fails every dependency and asserts
// liveness still passes.
func (r *Registry) Register(c Check) {
	if c.Name == "" || c.Fn == nil {
		panic("health: a check requires a name and a function")
	}
	if c.Timeout <= 0 {
		c.Timeout = r.defaultTimeout
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks = append(r.checks, c)
}

// RegisterLiveness registers a process-internal check.
//
// Liveness checks are always critical — there is no such thing as a partially alive process —
// and the separate constructor exists so that the Critical field cannot be set to false by
// accident on the one probe where the setting would be meaningless.
func (r *Registry) RegisterLiveness(name string, fn CheckFunc) {
	r.Register(Check{Name: name, Kind: KindLiveness, Fn: fn, Critical: true})
}

// RegisterReadiness registers a dependency required to serve.
func (r *Registry) RegisterReadiness(name string, critical bool, fn CheckFunc) {
	r.Register(Check{Name: name, Kind: KindReadiness, Fn: fn, Critical: critical})
}

// RegisterStartup registers a slow-initialisation check.
func (r *Registry) RegisterStartup(name string, fn CheckFunc) {
	r.Register(Check{Name: name, Kind: KindStartup, Fn: fn, Critical: true})
}

// Live runs the liveness checks.
//
// With no liveness checks registered, the process is alive: it is running this code. That default
// is correct and is worth stating, because the alternative — "no checks means unknown means fail"
// — would make an unconfigured service restart forever.
func (r *Registry) Live(ctx context.Context) Response { return r.run(ctx, KindLiveness) }

// Ready runs the readiness checks.
func (r *Registry) Ready(ctx context.Context) Response { return r.run(ctx, KindReadiness) }

// Started runs the startup checks, latching success.
//
// Once every startup check has passed, this reports up forever without running them again. That
// is deliberate: the startup probe's contract is "has initialisation finished", and initialisation
// finishes once. A startup check that kept evaluating would be a second liveness probe with a
// looser deadline and no restriction on touching dependencies — which is exactly the amplifier
// the liveness rules exist to prevent.
func (r *Registry) Started(ctx context.Context) Response {
	if r.hasStarted() {
		return Response{Status: StatusUp, Kind: KindStartup, Service: r.service, Version: r.version}
	}
	resp := r.run(ctx, KindStartup)
	if resp.Status == StatusUp {
		r.started.Do(func() { r.setStarted() })
	}
	return resp
}

func (r *Registry) hasStarted() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.startOK
}

func (r *Registry) setStarted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startOK = true
}

func (r *Registry) run(ctx context.Context, kind Kind) Response {
	start := r.clock.Now()
	r.mu.RLock()
	selected := make([]Check, 0, len(r.checks))
	for _, c := range r.checks {
		if c.Kind == kind {
			selected = append(selected, c)
		}
	}
	r.mu.RUnlock()

	results := make([]Result, 0, len(selected))
	status := StatusUp
	for _, c := range selected {
		res := r.runOne(ctx, c)
		results = append(results, res)
		switch {
		case res.Status == StatusDown && c.Critical:
			status = StatusDown
		case res.Status == StatusDown || res.Status == StatusDegraded:
			// A non-critical failure degrades rather than sheds. Shedding capacity because an
			// optional dependency is unhappy is a self-inflicted brownout.
			if status == StatusUp {
				status = StatusDegraded
			}
		}
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	return Response{
		Status:   status,
		Kind:     kind,
		Checks:   results,
		Service:  r.service,
		Version:  r.version,
		Duration: r.clock.Now().Sub(start).Milliseconds(),
	}
}

// runOne executes a check, or serves a cached result, collapsing concurrent callers onto one
// execution.
func (r *Registry) runOne(ctx context.Context, c Check) Result {
	if r.cacheTTL <= 0 {
		return r.execute(ctx, c)
	}
	now := r.clock.Now()

	r.cacheMu.Lock()
	entry, ok := r.cache[c.Name]
	if ok && entry.inflight == nil && now.Before(entry.expires) {
		res := entry.result
		r.cacheMu.Unlock()
		res.Cached = true
		return res
	}
	if ok && entry.inflight != nil {
		// Another probe is already running this check. Wait for it rather than starting a
		// second: the whole point of the cache is that a burst produces one dependency call.
		wg := entry.inflight
		r.cacheMu.Unlock()
		wg.Wait()
		r.cacheMu.Lock()
		res := r.cache[c.Name].result
		r.cacheMu.Unlock()
		res.Cached = true
		return res
	}
	var wg sync.WaitGroup
	wg.Add(1)
	r.cache[c.Name] = &cacheEntry{inflight: &wg}
	r.cacheMu.Unlock()

	res := r.execute(ctx, c)

	r.cacheMu.Lock()
	r.cache[c.Name] = &cacheEntry{result: res, expires: r.clock.Now().Add(r.cacheTTL)}
	r.cacheMu.Unlock()
	wg.Done()
	return res
}

// execute runs one check under its own timeout, isolated from a panic.
func (r *Registry) execute(ctx context.Context, c Check) Result {
	start := r.clock.Now()
	// A per-check timeout derived from the caller's context, so a probe with its own deadline
	// still bounds each check independently. Without the per-check bound, one hanging check
	// consumes the whole probe budget and every check after it reports a timeout it did not
	// cause.
	cctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	err := runSafely(cctx, c.Fn)

	res := Result{Name: c.Name, Status: StatusUp, CheckedAt: start}
	if err != nil {
		res.Status, res.Error = StatusDown, err.Error()
	}
	res.DurationMS = r.clock.Now().Sub(start).Milliseconds()
	return res
}

// runSafely converts a panicking check into a failing one.
//
// A check is written by whoever owns the dependency, and a nil map or a closed channel in one of
// them must not take down the process that the probe exists to keep alive. This is the narrow
// case where recovering a panic is right: the alternative is that a bug in a diagnostic kills the
// thing being diagnosed.
func runSafely(ctx context.Context, fn CheckFunc) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &panicError{value: p}
		}
	}()
	return fn(ctx)
}

type panicError struct{ value any }

func (e *panicError) Error() string { return "health check panicked" }

// Handler returns an http.Handler for one probe kind.
//
// The status code is what the kubelet reads; the body is for humans. 200 for up and degraded,
// 503 for down — degraded deliberately returns 200, because a degraded pod can still serve and
// removing it would reduce capacity during exactly the incident where capacity matters.
func (r *Registry) Handler(kind Kind) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var resp Response
		switch kind {
		case KindLiveness:
			resp = r.Live(req.Context())
		case KindStartup:
			resp = r.Started(req.Context())
		default:
			resp = r.Ready(req.Context())
		}
		w.Header().Set("Content-Type", "application/json")
		// Probe responses must never be cached by anything in front of them. A cached readiness
		// response is a load balancer routing to a pod that said "yes" a minute ago.
		w.Header().Set("Cache-Control", "no-store")
		if resp.Status == StatusDown {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
}

// Mount registers the three probe paths on a mux, with the conventional names.
func (r *Registry) Mount(mux *http.ServeMux) {
	mux.Handle("/livez", r.Handler(KindLiveness))
	mux.Handle("/readyz", r.Handler(KindReadiness))
	mux.Handle("/startupz", r.Handler(KindStartup))
}

// Names returns the registered check names for a kind, in a stable order, for tests and for the
// operational documentation generator.
func (r *Registry) Names(kind Kind) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.checks))
	for _, c := range r.checks {
		if c.Kind == kind {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}
