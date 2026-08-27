package config_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	domainconfig "github.com/udaykishore-resu/payments-platform/internal/domain/config"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/config"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

const (
	tenantA   = shared.TenantID("ten_01J0000000000000000000000A")
	merchantA = shared.MerchantID("mrc_01J000000000000000000000A")
	merchantB = shared.MerchantID("mrc_01J000000000000000000000B")
	newcomer  = shared.MerchantID("mrc_01J000000000000000000000C")
)

type memorySource struct {
	mu      sync.Mutex
	configs []*domainconfig.MerchantConfig
	err     error
	loads   int
}

func (s *memorySource) Load(context.Context, time.Time) ([]*domainconfig.MerchantConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.err != nil {
		return nil, s.err
	}
	return s.configs, nil
}

func (s *memorySource) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *memorySource) set(cs []*domainconfig.MerchantConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configs, s.err = cs, nil
}

type recordingObserver struct {
	mu       sync.Mutex
	ages     []time.Duration
	stalls   int
	refusals []shared.MerchantID
}

func (o *recordingObserver) ConfigSnapshotAge(age time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ages = append(o.ages, age)
}

func (o *recordingObserver) ConfigPropagationStalled(time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stalls++
}

func (o *recordingObserver) ConfigCliffRefusal(_ time.Duration, m shared.MerchantID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.refusals = append(o.refusals, m)
}

func (o *recordingObserver) counts() (stalls, refusals int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stalls, len(o.refusals)
}

func merchantConfig(m shared.MerchantID) *domainconfig.MerchantConfig {
	return &domainconfig.MerchantConfig{
		MerchantID:  m,
		TenantID:    tenantA,
		Version:     3,
		Environment: shared.EnvironmentProduction,
	}
}

func newProvider(t *testing.T, mutate func(*config.ProviderConfig)) (*config.Provider, *memorySource, *shared.FixedClock, *recordingObserver) {
	t.Helper()
	clock := &shared.FixedClock{T: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
	src := &memorySource{configs: []*domainconfig.MerchantConfig{merchantConfig(merchantA), merchantConfig(merchantB)}}
	obs := &recordingObserver{}
	cfg := config.ProviderConfig{Source: src, Clock: clock, Observer: obs}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := config.NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	return p, src, clock, obs
}

func code(err error) apierror.Code {
	var e *apierror.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Each rung of baseline §15's ladder, in order.
func TestStalenessLadder(t *testing.T) {
	// Verifies: FR-43, FR-47.
	t.Parallel()
	ctx := context.Background()
	p, src, clock, obs := newProvider(t, nil)
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	// The control plane goes away.
	src.fail(errors.New("control plane unreachable"))

	t.Run("0-30s: normal, no alert, no behaviour change", func(t *testing.T) {
		clock.Advance(20 * time.Second)
		if err := p.Refresh(ctx); err == nil {
			t.Fatal("the refresh should have failed")
		}
		if !p.Fresh() {
			t.Fatalf("age %v should be inside bounded staleness", p.SnapshotAge())
		}
		if _, err := p.Get(ctx, merchantA); err != nil {
			t.Fatalf("a known merchant must be served: %v", err)
		}
		if stalls, _ := obs.counts(); stalls != 0 {
			t.Fatalf("no alert should have fired yet: %d", stalls)
		}
	})

	t.Run("30s-5min: stale but usable, warning only", func(t *testing.T) {
		clock.Advance(2 * time.Minute)
		_ = p.Refresh(ctx)
		if p.Fresh() {
			t.Fatal("the snapshot should be past bounded staleness")
		}
		if p.PastCliff() {
			t.Fatal("the snapshot should be well short of the cliff")
		}
		if _, err := p.Get(ctx, merchantA); err != nil {
			t.Fatalf("a known merchant must still be served: %v", err)
		}
		if stalls, _ := obs.counts(); stalls != 0 {
			t.Fatalf("the page fires at 5 minutes, not before: %d", stalls)
		}
	})

	t.Run("5min: alert, behaviour still unchanged", func(t *testing.T) {
		clock.Advance(4 * time.Minute)
		_ = p.Refresh(ctx)
		if stalls, _ := obs.counts(); stalls != 1 {
			t.Fatalf("the stall alert must fire exactly once: %d", stalls)
		}
		// We alert BEFORE we degrade. Both merchants still serve, and so does an unknown one's
		// honest 404.
		if _, err := p.Get(ctx, merchantA); err != nil {
			t.Fatalf("behaviour must not have changed yet: %v", err)
		}
		if _, err := p.Get(ctx, newcomer); code(err) != apierror.CodeMerchantNotFound {
			t.Fatalf("inside the cliff, an absent merchant is honestly absent: %v", err)
		}
		// A latched alert must not page once per refresh interval.
		clock.Advance(time.Minute)
		_ = p.Refresh(ctx)
		if stalls, _ := obs.counts(); stalls != 1 {
			t.Fatalf("the stall alert must be latched: %d", stalls)
		}
	})

	t.Run("15min: the cliff falls unevenly", func(t *testing.T) {
		clock.Advance(11 * time.Minute)
		_ = p.Refresh(ctx)
		if !p.PastCliff() {
			t.Fatalf("age %v should be past the cliff", p.SnapshotAge())
		}
		// Merchants IN the snapshot continue normally. This is the whole point: refusing them
		// would be fail-closed, which inverts the plane model.
		for _, m := range []shared.MerchantID{merchantA, merchantB} {
			cfg, err := p.Get(ctx, m)
			if err != nil {
				t.Fatalf("a merchant in the snapshot must continue past the cliff: %v", err)
			}
			if cfg.MerchantID != m {
				t.Fatalf("wrong config served: %s", cfg.MerchantID)
			}
		}
		// Merchants NOT in the snapshot are refused: retryable, with guidance.
		_, err := p.Get(ctx, newcomer)
		if code(err) != apierror.CodeConfigurationStale {
			t.Fatalf("want CONFIGURATION_STALE, got %v", err)
		}
		var e *apierror.Error
		errors.As(err, &e)
		if e.HTTPStatus() != 503 || !e.Retryable || e.RetryAfterSeconds == 0 {
			t.Fatalf("the cliff refusal must be a retryable 503 with Retry-After: %d %v %d",
				e.HTTPStatus(), e.Retryable, e.RetryAfterSeconds)
		}
		if _, refusals := obs.counts(); refusals != 1 {
			t.Fatalf("the cliff refusal must be observable: %d", refusals)
		}
	})

	t.Run("recovery restores service without a restart", func(t *testing.T) {
		src.set([]*domainconfig.MerchantConfig{
			merchantConfig(merchantA), merchantConfig(merchantB), merchantConfig(newcomer),
		})
		if err := p.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		if p.SnapshotAge() != 0 || !p.Fresh() {
			t.Fatalf("a successful refresh must reset the age: %v", p.SnapshotAge())
		}
		if _, err := p.Get(ctx, newcomer); err != nil {
			t.Fatalf("the previously-unknown merchant must now serve: %v", err)
		}
		// The alert latch must reset, or the next stall is silent.
		src.fail(errors.New("down again"))
		clock.Advance(6 * time.Minute)
		_ = p.Refresh(ctx)
		if stalls, _ := obs.counts(); stalls != 2 {
			t.Fatalf("the latch must reset on recovery: %d", stalls)
		}
	})
}

// A failed refresh must leave the previous snapshot in place — that is fail-static — and must
// leave the age rising, so the ladder advances rather than the outage being hidden.
func TestFailedRefreshIsFailStatic(t *testing.T) {
	// Verifies: FR-48, NFR-22.
	t.Parallel()
	ctx := context.Background()
	p, src, clock, _ := newProvider(t, nil)
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	before := p.Snapshot()

	src.fail(errors.New("boom"))
	clock.Advance(time.Minute)
	err := p.Refresh(ctx)
	if code(err) != apierror.CodeDependencyFailure {
		t.Fatalf("want DEPENDENCY_FAILURE, got %v", err)
	}
	if p.Snapshot() != before {
		t.Fatalf("a failed refresh must not empty the snapshot: %d then %d", before, p.Snapshot())
	}
	if p.SnapshotAge() != time.Minute {
		t.Fatalf("a failed refresh must not stamp the refresh time: %v", p.SnapshotAge())
	}
}

func TestColdProviderRefusesRatherThanLying(t *testing.T) {
	t.Parallel()
	p, _, _, _ := newProvider(t, nil)
	// A pod that has not reached its high-water mark knows nothing about anyone. Answering
	// "no such merchant" would be a lie with a 404's finality.
	_, err := p.Get(context.Background(), merchantA)
	if code(err) != apierror.CodeConfigurationStale {
		t.Fatalf("want CONFIGURATION_STALE, got %v", err)
	}
	if p.SnapshotAge() != 0 {
		t.Fatalf("a cold provider must report zero age, not an enormous one: %v", p.SnapshotAge())
	}
}

// Priority invalidation on merchant.suspended.v1 must take effect on the very next request, not
// on the next natural refresh.
func TestPriorityInvalidationIsImmediate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, src, _, obs := newProvider(t, nil)
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(ctx, merchantA); err != nil {
		t.Fatalf("precondition: %v", err)
	}

	p.Invalidate(merchantA)

	// The very next call, with a snapshot that is otherwise perfectly fresh.
	if !p.Fresh() {
		t.Fatal("precondition: the snapshot is fresh; only the invalidation should matter")
	}
	_, err := p.Get(ctx, merchantA)
	if code(err) != apierror.CodeConfigurationStale {
		t.Fatalf("an invalidated merchant must not be served from the snapshot, got %v", err)
	}
	if _, refusals := obs.counts(); refusals != 1 {
		t.Fatalf("the refusal must be observable: %d", refusals)
	}
	// The invalidation is per-merchant: it must not cross to another merchant or another tenant.
	if _, err := p.Get(ctx, merchantB); err != nil {
		t.Fatalf("invalidating one merchant must not affect another: %v", err)
	}

	// And the next successful refresh answers it.
	src.set([]*domainconfig.MerchantConfig{merchantConfig(merchantA), merchantConfig(merchantB)})
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(ctx, merchantA); err != nil {
		t.Fatalf("a refresh must clear the invalidation it has answered: %v", err)
	}
}

// A merchant that was invalidated and then genuinely removed upstream must stay refused rather
// than silently reappearing as "unknown, but the snapshot is fresh, so 404".
func TestInvalidationSurvivesARefreshThatDoesNotRestoreTheMerchant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, src, _, _ := newProvider(t, nil)
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	p.Invalidate(merchantA)
	src.set([]*domainconfig.MerchantConfig{merchantConfig(merchantB)})
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(ctx, merchantA); code(err) != apierror.CodeConfigurationStale {
		t.Fatalf("want CONFIGURATION_STALE, got %v", err)
	}
}

func TestGetPerformsNoIO(t *testing.T) {
	// Verifies: FR-43.
	t.Parallel()
	ctx := context.Background()
	p, src, _, _ := newProvider(t, nil)
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	src.mu.Lock()
	before := src.loads
	src.mu.Unlock()

	for i := 0; i < 100; i++ {
		_, _ = p.Get(ctx, merchantA)
		_, _ = p.Get(ctx, newcomer)
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.loads != before {
		t.Fatalf("Get performed %d loads; the payment hot path must never reach the control plane",
			src.loads-before)
	}
}

func TestBackgroundRefreshIsBoundedAndStoppable(t *testing.T) {
	t.Parallel()
	p, src, _, _ := newProvider(t, func(c *config.ProviderConfig) {
		c.Clock = shared.SystemClock{}
		c.RefreshInterval = 5 * time.Millisecond
	})
	p.Start(context.Background())
	p.Start(context.Background()) // idempotent

	select {
	case <-p.Refreshed():
	case <-time.After(2 * time.Second):
		t.Fatal("the background refresher never ran")
	}
	src.mu.Lock()
	loads := src.loads
	src.mu.Unlock()
	if loads == 0 {
		t.Fatal("the refresher should have loaded")
	}
	if p.Snapshot() != 2 {
		t.Fatalf("snapshot = %d", p.Snapshot())
	}
	p.Stop()
	p.Stop() // idempotent
	p.Start(context.Background())
	p.Stop()
}

func TestProviderStopWithoutStart(t *testing.T) {
	t.Parallel()
	p, _, _, _ := newProvider(t, nil)
	p.Stop()
	p.Stop()
}

func TestNewProviderRequiresASource(t *testing.T) {
	t.Parallel()
	if _, err := config.NewProvider(config.ProviderConfig{}); err == nil {
		t.Fatal("a provider with no source can never load a snapshot")
	}
}

func TestRefreshIgnoresUnusableEntries(t *testing.T) {
	t.Parallel()
	p, src, _, _ := newProvider(t, nil)
	src.set([]*domainconfig.MerchantConfig{
		nil,
		{TenantID: tenantA}, // no merchant id
		merchantConfig(merchantA),
	})
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.Snapshot() != 1 {
		t.Fatalf("snapshot = %d, want only the usable entry", p.Snapshot())
	}
}
