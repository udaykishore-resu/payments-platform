package config

import (
	"context"
	"sync"
	"time"

	domainconfig "github.com/udaykishore-resu/payments-platform/internal/domain/config"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// SnapshotSource supplies the current desired state for every merchant.
//
// In production this reads the compacted `configuration.published.v1` topic: a pod starting cold
// reads the log to its high-water mark and has the current configuration for every merchant
// without a single call to the control plane. That is what makes rule P1 — the data plane has no
// synchronous dependency on the control plane — physically true rather than aspirational.
//
// The interface is one method and takes a watermark so an incremental refresh is expressible;
// passing the zero time means "everything".
type SnapshotSource interface {
	Load(ctx context.Context, since time.Time) ([]*domainconfig.MerchantConfig, error)
}

// ProviderObserver receives the snapshot's health signals.
//
// Three methods rather than a generic gauge because the three carry different operational
// meanings and are alerted on differently: the age is a gauge, the stall is a page, and the cliff
// is a business-impacting refusal that belongs in an incident timeline.
type ProviderObserver interface {
	// ConfigSnapshotAge is exported as pp_config_snapshot_age_seconds.
	ConfigSnapshotAge(age time.Duration)
	// ConfigPropagationStalled fires once the age passes the alert threshold. Behaviour has not
	// changed yet — we alert *before* we degrade, so an operator has the whole window between
	// the alert and the cliff to fix it.
	ConfigPropagationStalled(age time.Duration)
	// ConfigCliffRefusal fires each time the cliff causes a refusal, naming the merchant.
	ConfigCliffRefusal(age time.Duration, merchant shared.MerchantID)
}

// ProviderConfig configures the fail-static snapshot.
//
// The three durations are the rungs of baseline §15's ladder, and their relationship is the
// design: BoundedStaleness < AlertAfter < MaxStaleness. Compressing them changes the character of
// the degradation, so a deployment overriding one should override all three deliberately.
type ProviderConfig struct {
	Source SnapshotSource
	// RefreshInterval is the background refresh cadence. Set well below BoundedStaleness so a
	// single missed refresh does not immediately breach it.
	RefreshInterval time.Duration
	// BoundedStaleness is the ≤30 s window inside which the snapshot is considered normal. Past
	// it the snapshot is stale-but-usable and the age gauge is rising; nothing else changes.
	BoundedStaleness time.Duration
	// AlertAfter is when "config propagation stalled" pages, at 5 minutes. Deliberately ten
	// times the bounded-staleness window and a third of the cliff: the gap is the operator's
	// budget to fix the problem before behaviour changes.
	AlertAfter time.Duration
	// MaxStaleness is the cliff (default 15 minutes). It is a *deploy-time* setting precisely so
	// that a control-plane outage cannot change it — a cliff the failing component could move is
	// not a bound.
	MaxStaleness time.Duration
	Clock        shared.Clock
	Observer     ProviderObserver
}

// The staleness ladder's defaults, from baseline §15 and control-plane.md §4.3.
const (
	DefaultRefreshInterval  = 10 * time.Second
	DefaultBoundedStaleness = 30 * time.Second
	DefaultAlertAfter       = 5 * time.Minute
	DefaultMaxStaleness     = 15 * time.Minute
)

// Provider is the data plane's read-only, fail-static view of merchant configuration.
//
// # Fail-static, not fail-open, not fail-closed — with a defined cliff
//
// When the control plane is unreachable the snapshot stops being refreshed, and the question is
// what to do with payments. Three answers, two of them wrong:
//
//   - Fail open — process without limits. A compliance and financial-exposure breach, at any
//     duration. Never acceptable, so it is not implemented and there is no flag that enables it.
//   - Fail closed — stop processing. A component with a 99.9 % target would zero out revenue on a
//     path with a 99.99 % target, inverting the entire plane model. Also not implemented.
//   - Fail static — keep serving the last approved configuration, with a bound.
//
// The bound is what makes the third answer graceful degradation rather than unbounded drift, and
// it falls unevenly on purpose:
//
//	0–30 s      normal; inside bounded staleness; no alert
//	30 s–5 min  stale but usable; the age gauge rises; warning only
//	5 min       ALERT. Behaviour still unchanged — we alert before we degrade
//	15 min      CLIFF:
//	              merchants IN the snapshot        → continue normally
//	              merchants NOT in the snapshot    → 503, retryable, Retry-After
//
// The asymmetry at the cliff is the whole idea. A merchant we have configuration for continues
// under the limits their tenant approved; those limits were correct fifteen minutes ago and are
// almost certainly still correct. A merchant we have *no* configuration for is one whose limits
// nobody has decided — we cannot tell whether they are new, or suspended, or do not exist — and
// processing money for them would be fail-open for exactly the population where it is most
// dangerous.
//
// What the cliff never does: fail a payment already in PROCESSING, block a refund, stop webhook
// processing, or fail a capture against an existing authorization. Money already in motion always
// completes. Those paths do not consult this provider for permission at all; they consult it for
// the limits of a merchant already in the snapshot.
type Provider struct {
	cfg ProviderConfig

	mu sync.RWMutex
	// byMerchant is the snapshot. A map read under an RWMutex rather than an atomic.Value
	// holding an immutable map, because Invalidate mutates a single entry and the read volume
	// here (one per payment) does not justify copy-on-write for the whole tenant estate.
	byMerchant map[shared.MerchantID]*domainconfig.MerchantConfig
	// invalidated records merchants dropped by the priority path. Kept separately from simple
	// absence so that Get can distinguish "we never had this merchant" from "we deliberately
	// dropped this merchant and have not yet re-learned it" — two situations with the same
	// symptom and different correct answers.
	invalidated map[shared.MerchantID]time.Time
	refreshedAt time.Time
	// alerted latches the stall alert so a five-minute outage pages once rather than once per
	// refresh interval.
	alerted bool

	life    sync.Mutex
	running bool
	stopped bool
	stop    chan struct{}
	done    chan struct{}
	// refreshed is signalled after each background cycle so tests need no sleeps.
	refreshed chan struct{}
}

// NewProvider builds a fail-static provider. It does not fetch; call Refresh or Start.
func NewProvider(cfg ProviderConfig) (*Provider, error) {
	if cfg.Source == nil {
		return nil, apierror.New(apierror.CodeInternalError, "the config provider requires a snapshot source")
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = DefaultRefreshInterval
	}
	if cfg.BoundedStaleness <= 0 {
		cfg.BoundedStaleness = DefaultBoundedStaleness
	}
	if cfg.AlertAfter <= 0 {
		cfg.AlertAfter = DefaultAlertAfter
	}
	if cfg.MaxStaleness <= 0 {
		cfg.MaxStaleness = DefaultMaxStaleness
	}
	if cfg.Clock == nil {
		cfg.Clock = shared.SystemClock{}
	}
	return &Provider{
		cfg:         cfg,
		byMerchant:  map[shared.MerchantID]*domainconfig.MerchantConfig{},
		invalidated: map[shared.MerchantID]time.Time{},
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		refreshed:   make(chan struct{}, 1),
	}, nil
}

// Get returns the merchant's effective configuration, applying the staleness ladder.
//
// It never performs I/O. That is not an optimization, it is the architectural rule: there is no
// method on this type that can reach the control plane, so nobody can add a synchronous
// configuration fetch to the payment hot path without first changing this interface, which is a
// reviewed change rather than an accident.
func (p *Provider) Get(_ context.Context, m shared.MerchantID) (*domainconfig.MerchantConfig, error) {
	p.mu.RLock()
	cfg, present := p.byMerchant[m]
	_, dropped := p.invalidated[m]
	refreshedAt := p.refreshedAt
	p.mu.RUnlock()

	age := p.age(refreshedAt)

	// A merchant dropped by the priority path is not served from the stale snapshot, whatever
	// its age. That is the entire value of priority invalidation: a merchant.suspended.v1 event
	// must take effect on the very next request, not on the next natural refresh.
	if dropped {
		p.observeCliff(age, m)
		return nil, staleError(age)
	}
	if present {
		// Merchants in the snapshot continue normally, at any age. Their limits were approved by
		// their tenant and are almost certainly still correct; refusing them would be
		// fail-closed, which inverts the plane model.
		return cfg, nil
	}
	if refreshedAt.IsZero() {
		// The snapshot has never loaded. A cold pod that has not reached its high-water mark
		// knows nothing about anyone, and answering "no such merchant" would be a lie with a
		// 404's finality. This is retryable, and readiness should be failing anyway.
		return nil, staleError(0)
	}
	if age > p.cfg.MaxStaleness {
		// Past the cliff, and we have no configuration for this merchant. We cannot tell whether
		// they are new, suspended, or nonexistent, and processing money without limits for an
		// unknown merchant is fail-open.
		p.observeCliff(age, m)
		return nil, staleError(age)
	}
	// Inside the cliff and genuinely absent: the merchant does not exist. A 404 here is honest
	// and final, which is what a client needs.
	return nil, apierror.Newf(apierror.CodeMerchantNotFound, "no configuration for merchant %s", m)
}

// SnapshotAge reports how long ago the local view was last refreshed. Exported as
// pp_config_snapshot_age_seconds and alerted on past five minutes.
//
// A provider that has never refreshed reports a zero age rather than an enormous one, because an
// enormous age on a cold pod would fire the stall alert during every deploy. Readiness is the
// signal for "this pod has not warmed yet"; this gauge is the signal for "propagation has
// stopped".
func (p *Provider) SnapshotAge() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.age(p.refreshedAt)
}

func (p *Provider) age(refreshedAt time.Time) time.Duration {
	if refreshedAt.IsZero() {
		return 0
	}
	d := p.cfg.Clock.Now().Sub(refreshedAt)
	if d < 0 {
		return 0
	}
	return d
}

// Fresh reports whether the snapshot is inside the bounded-staleness window. It exists for the
// readiness probe and for the log field that explains a degraded decision.
func (p *Provider) Fresh() bool { return p.SnapshotAge() <= p.cfg.BoundedStaleness }

// PastCliff reports whether the snapshot is beyond max staleness.
//
// Readiness uses this together with a fleet-wide comparison: a pod that is the *only* one lagging
// should be shed, but a fleet-wide stall must not shed the whole fleet — that would be fail-closed
// by accident, which is precisely the outcome the cliff design exists to avoid.
func (p *Provider) PastCliff() bool { return p.SnapshotAge() > p.cfg.MaxStaleness }

// Invalidate drops a merchant from the local view.
//
// This is the priority path for `merchant.suspended.v1`. It does not merely delete the entry: a
// deleted entry inside the cliff would read as "this merchant does not exist" and produce a 404,
// which is the wrong answer for a merchant we know exists and whose current state we no longer
// know. Instead the merchant is marked, and Get refuses with a retryable CONFIGURATION_STALE
// until the next refresh supplies the current state — including, when that is what happened, a
// suspended status that the caller can act on properly.
//
// Suspending a merchant must take effect within 30 seconds (security.md §1). Waiting for a
// natural refresh would mean the merchant keeps processing for up to that window after the
// suspension decision, which for a fraud-driven suspension is exactly the window that matters.
func (p *Provider) Invalidate(m shared.MerchantID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byMerchant, m)
	p.invalidated[m] = p.cfg.Clock.Now()
}

// Snapshot returns the number of merchants currently held, for the readiness probe and for tests.
func (p *Provider) Snapshot() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.byMerchant)
}

// Refresh loads from the source and replaces the snapshot.
//
// A failed refresh leaves the previous snapshot in place — that is fail-static in one line — and
// leaves refreshedAt where it was, so the age keeps rising and the ladder advances. Silently
// stamping refreshedAt on a failure would hide the outage from the one metric that exists to
// reveal it.
func (p *Provider) Refresh(ctx context.Context) error {
	// The full state is requested rather than an increment. The source is a compacted log whose
	// tail *is* the current desired state, so a full read is cheap and is the only version with
	// no divergence risk: an incremental refresh that misses a deletion leaves a merchant in the
	// snapshot forever.
	configs, err := p.cfg.Source.Load(ctx, time.Time{})
	if err != nil {
		p.observeAge()
		return apierror.Wrap(err, apierror.CodeDependencyFailure, "refreshing the configuration snapshot")
	}
	next := make(map[shared.MerchantID]*domainconfig.MerchantConfig, len(configs))
	for _, c := range configs {
		if c == nil || c.MerchantID == "" {
			continue
		}
		next[c.MerchantID] = c
	}

	p.mu.Lock()
	p.byMerchant = next
	p.refreshedAt = p.cfg.Clock.Now()
	// A completed refresh clears the priority invalidations it has now answered: whatever the
	// current state of those merchants is, the snapshot now carries it.
	for m := range p.invalidated {
		if _, ok := next[m]; ok {
			delete(p.invalidated, m)
		}
	}
	p.alerted = false
	p.mu.Unlock()

	p.observeAge()
	return nil
}

// Start launches the single background refresher.
func (p *Provider) Start(ctx context.Context) {
	p.life.Lock()
	defer p.life.Unlock()
	if p.running || p.stopped {
		return
	}
	p.running = true
	go p.loop(ctx)
}

// Stop halts the refresher and waits for it to exit. Idempotent, and safe without Start.
func (p *Provider) Stop() {
	p.life.Lock()
	if p.stopped {
		p.life.Unlock()
		<-p.done
		return
	}
	p.stopped = true
	close(p.stop)
	if !p.running {
		close(p.done)
	}
	p.life.Unlock()
	<-p.done
}

// Refreshed is signalled after each background refresh cycle, so a test can synchronise without
// sleeping.
func (p *Provider) Refreshed() <-chan struct{} { return p.refreshed }

func (p *Provider) loop(ctx context.Context) {
	defer close(p.done)
	ticker := time.NewTicker(p.cfg.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			// The error is deliberately dropped: a failed refresh is fail-static, already
			// reflected in the rising age, and there is nobody above this goroutine to return it
			// to. Logging it is the caller's job through the observer.
			_ = p.Refresh(ctx)
			select {
			case p.refreshed <- struct{}{}:
			default:
			}
		}
	}
}

// observeAge emits the gauge and, once, the stall alert.
func (p *Provider) observeAge() {
	if p.cfg.Observer == nil {
		return
	}
	age := p.SnapshotAge()
	p.cfg.Observer.ConfigSnapshotAge(age)

	p.mu.Lock()
	shouldAlert := age > p.cfg.AlertAfter && !p.alerted
	if shouldAlert {
		p.alerted = true
	}
	p.mu.Unlock()
	if shouldAlert {
		p.cfg.Observer.ConfigPropagationStalled(age)
	}
}

func (p *Provider) observeCliff(age time.Duration, m shared.MerchantID) {
	if p.cfg.Observer != nil {
		p.cfg.Observer.ConfigCliffRefusal(age, m)
	}
}

// staleError is the refusal at the cliff: retryable, with guidance, and carrying the registered
// CONFIGURATION_STALE code so a client SDK and the workflow engine both know to come back rather
// than to give up.
func staleError(_ time.Duration) *apierror.Error {
	// Retry-After is a fixed 30 seconds rather than a function of the age: the client cannot act
	// on how stale we are, only on when to come back, and a value derived from the outage's
	// duration would send every client back at the same instant once it ended.
	const retryAfterSeconds = 30
	return apierror.New(apierror.CodeConfigurationStale, "").
		WithRetryAfter(retryAfterSeconds).
		WithDetail(apierror.Detail{
			Code:    "SNAPSHOT_TOO_STALE",
			Message: "the local configuration snapshot is too stale to serve this merchant safely",
			RuleID:  "L0.CONFIG_SNAPSHOT_WITHIN_MAX_STALENESS",
		})
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: FR-47, FR-48, NFR-22.
//
// Configuration propagation to the data plane and the fail-static behaviour with a defined
// cliff when the control plane is unreachable
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
