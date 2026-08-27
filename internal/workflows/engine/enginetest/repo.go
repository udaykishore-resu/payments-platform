// Package enginetest provides the in-memory doubles that make the workflow engine's semantics
// testable without a database.
//
// The engine's interesting properties — replay-free resume, fencing, compensation ordering,
// retry exhaustion, manual gates — are properties of the *engine*, not of Postgres. Proving them
// against a real database would mean every one of those tests needs a container, a migration and
// a cleanup, which in practice means they get written once and then skipped. The fake here
// reproduces the two storage behaviours the engine's correctness actually rests on:
//
//   - **Fencing.** Every write carries a lease epoch, and a write whose epoch does not match the
//     stored one affects zero rows and returns engine.ErrLeaseLost — exactly what
//     `UPDATE ... WHERE lease_epoch = $n` does in the real store.
//   - **Skip-locked leasing.** LeaseRunnable claims only instances that are runnable, whose
//     RunAfter has elapsed and whose lease has expired, bumping the epoch on every acquisition.
//
// Everything else is a map. Integration coverage of the real SQL lives in the Postgres
// repository's own `_integration_test.go`; this package exists so that the *engine's* tests do
// not have to be integration tests to be meaningful.
package enginetest

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Clock is a mutex-guarded manual clock.
//
// shared.FixedClock exists but its Advance is unsynchronized, and the worker loop reads the
// clock from goroutines the test advances it from — which is a data race the race detector
// will (correctly) fail the build over. Time in a concurrent test is shared mutable state and
// has to be guarded like any other.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock returns a clock fixed at t.
func NewClock(t time.Time) *Clock { return &Clock{t: t.UTC()} }

// Now implements shared.Clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward and returns the new time.
func (c *Clock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

// Sleep satisfies the resilience package's Clock without importing it: it records nothing,
// blocks for no wall-clock time and advances the manual clock by d.
//
// That is what keeps the compensation-retry tests honest *and* fast. A compensation's retry
// policy caps at five minutes because a failed compensation leaves the world inconsistent and
// is more urgent than the original failure; a test that proved the five-minute cap by waiting
// five minutes would be deleted within a week.
func (c *Clock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d > 0 {
		c.Advance(d)
	}
	return nil
}

// DLQEntry is one row of the workflow dead-letter queue.
type DLQEntry struct {
	WorkflowID shared.WorkflowID
	Step       string
	Payload    []byte
	Reason     string
	CreatedAt  time.Time
}

// Repo is an in-memory ports.WorkflowRepository with fencing and skip-locked leasing.
//
// Safe for concurrent use: the worker loop tests run several goroutines against one Repo, which
// is the point — a fake with no lock would make the fencing test pass for the wrong reason.
type Repo struct {
	mu        sync.Mutex
	clock     shared.Clock
	instances map[shared.WorkflowID]ports.WorkflowInstanceRecord
	steps     map[shared.WorkflowID][]ports.WorkflowStepRecord
	dlq       []DLQEntry
	seq       int

	// PoisonThreshold mirrors the lease query's `crash_count < $threshold` predicate. The engine
	// tracks the count; the repository is where the predicate that hides a quarantined instance
	// from every poller would live in SQL.
	PoisonThreshold int

	// calls counts repository operations, so a test can assert that a resumed instance did not
	// re-write a step it had already completed.
	calls map[string]int
}

var (
	_ ports.WorkflowRepository = (*Repo)(nil)
	_ engine.Checkpointer      = (*Repo)(nil)
)

// NewRepo returns an empty repository reading time from clock.
func NewRepo(clock shared.Clock) *Repo {
	return &Repo{
		clock:           clock,
		instances:       make(map[shared.WorkflowID]ports.WorkflowInstanceRecord, 8),
		steps:           make(map[shared.WorkflowID][]ports.WorkflowStepRecord, 8),
		calls:           make(map[string]int, 8),
		PoisonThreshold: 3,
	}
}

func (r *Repo) now() time.Time { return r.clock.Now().UTC() }

func (r *Repo) count(op string) { r.calls[op]++ }

// Calls returns how many times an operation was invoked.
func (r *Repo) Calls(op string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[op]
}

// CreateInstance inserts a new instance, enforcing the partial unique index on the live business
// key: at most one non-final instance may exist per (definition, business key).
func (r *Repo) CreateInstance(ctx context.Context, i ports.WorkflowInstanceRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count("CreateInstance")
	if _, exists := r.instances[i.ID]; exists {
		return apierror.Newf(apierror.CodeOnboardingAlreadyInProgress, "workflow %s already exists", i.ID)
	}
	for _, existing := range r.instances {
		if existing.Definition == i.Definition && existing.BusinessKey == i.BusinessKey &&
			!engine.InstanceState(existing.State).IsFinal() {
			return apierror.Newf(apierror.CodeOnboardingAlreadyInProgress,
				"a live %s instance already exists for business key %s", i.Definition, i.BusinessKey)
		}
	}
	if i.CreatedAt.IsZero() {
		i.CreatedAt = r.now()
	}
	i.UpdatedAt = r.now()
	r.instances[i.ID] = clone(i)
	return nil
}

// GetInstance returns an instance by ID.
func (r *Repo) GetInstance(ctx context.Context, id shared.WorkflowID) (*ports.WorkflowInstanceRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count("GetInstance")
	rec, ok := r.instances[id]
	if !ok {
		return nil, engine.NotFound(string(id))
	}
	out := clone(rec)
	return &out, nil
}

// GetInstanceByBusinessKey returns the *live* instance for a business key.
//
// Terminal instances are invisible here on purpose: that is what makes a second onboarding
// attempt after a failure a new, separately auditable instance rather than a resurrection of the
// old one, and it is the same predicate as the partial unique index.
func (r *Repo) GetInstanceByBusinessKey(ctx context.Context, def, key string) (*ports.WorkflowInstanceRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count("GetInstanceByBusinessKey")
	for _, rec := range r.instances {
		if rec.Definition == def && rec.BusinessKey == key && !engine.InstanceState(rec.State).IsFinal() {
			out := clone(rec)
			return &out, nil
		}
	}
	return nil, apierror.Wrapf(engine.ErrInstanceNotFound, apierror.CodeWorkflowNotFound,
		"no live %s instance for business key %s", def, key)
}

// LeaseRunnable claims runnable instances, bumping the fencing epoch on each acquisition.
func (r *Repo) LeaseRunnable(ctx context.Context, workerID string, lease time.Duration, limit int) ([]ports.WorkflowInstanceRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count("LeaseRunnable")
	now := r.now()

	ids := make([]shared.WorkflowID, 0, len(r.instances))
	for id := range r.instances {
		ids = append(ids, id)
	}
	// Deterministic order standing in for `ORDER BY runnable_at`: the map's iteration order is
	// randomized, and a test that passes or fails depending on it is worse than no test.
	sort.Slice(ids, func(a, b int) bool {
		x, y := r.instances[ids[a]], r.instances[ids[b]]
		if !x.RunAfter.Equal(y.RunAfter) {
			return x.RunAfter.Before(y.RunAfter)
		}
		return ids[a] < ids[b]
	})

	out := make([]ports.WorkflowInstanceRecord, 0, limit)
	for _, id := range ids {
		if len(out) >= limit {
			break
		}
		rec := r.instances[id]
		if !engine.InstanceState(rec.State).IsRunnable() {
			continue
		}
		if rec.RunAfter.After(now) {
			continue
		}
		if rec.LeaseUntil != nil && rec.LeaseUntil.After(now) {
			continue // held by a live lease; SKIP LOCKED means we never block on it
		}
		until := now.Add(lease)
		rec.LeaseOwner = workerID
		rec.LeaseEpoch++
		rec.LeaseUntil = &until
		rec.UpdatedAt = now
		r.instances[id] = rec
		out = append(out, clone(rec))
	}
	return out, nil
}

// Heartbeat extends a lease, rejecting a holder whose epoch is stale.
func (r *Repo) Heartbeat(ctx context.Context, id shared.WorkflowID, workerID string, epoch int64, extend time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count("Heartbeat")
	rec, ok := r.instances[id]
	if !ok {
		return engine.NotFound(string(id))
	}
	if rec.LeaseEpoch != epoch || rec.LeaseOwner != workerID {
		return leaseLost(id, epoch, rec.LeaseEpoch)
	}
	until := r.now().Add(extend)
	rec.LeaseUntil = &until
	r.instances[id] = rec
	return nil
}

// SaveInstance persists an instance, fenced on its lease epoch.
func (r *Repo) SaveInstance(ctx context.Context, i ports.WorkflowInstanceRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count("SaveInstance")
	return r.saveInstanceLocked(i)
}

func (r *Repo) saveInstanceLocked(i ports.WorkflowInstanceRecord) error {
	stored, ok := r.instances[i.ID]
	if !ok {
		return engine.NotFound(string(i.ID))
	}
	if stored.LeaseEpoch != i.LeaseEpoch {
		// This is the whole fencing mechanism: the zombie's UPDATE matches zero rows.
		return leaseLost(i.ID, i.LeaseEpoch, stored.LeaseEpoch)
	}
	i.CreatedAt = stored.CreatedAt
	i.UpdatedAt = r.now()
	r.instances[i.ID] = clone(i)
	return nil
}

// SaveStep persists a step record, upserting on (instance, step, attempt) — the unique index
// that means even a bug in the engine cannot record one attempt twice.
func (r *Repo) SaveStep(ctx context.Context, s ports.WorkflowStepRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count("SaveStep")
	return r.saveStepLocked(s)
}

func (r *Repo) saveStepLocked(s ports.WorkflowStepRecord) error {
	list := r.steps[s.WorkflowID]
	for i := range list {
		if list[i].Name == s.Name && list[i].Attempt == s.Attempt {
			s.ID = list[i].ID
			list[i] = cloneStep(s)
			r.steps[s.WorkflowID] = list
			return nil
		}
	}
	if s.ID == "" {
		r.seq++
		s.ID = shared.StepID("wfs_test_" + strconv.Itoa(r.seq))
	}
	r.steps[s.WorkflowID] = append(list, cloneStep(s))
	return nil
}

// CheckpointStep implements engine.Checkpointer: the step and the instance commit together,
// fenced on the instance's epoch. A stale epoch writes neither.
func (r *Repo) CheckpointStep(ctx context.Context, inst ports.WorkflowInstanceRecord, step ports.WorkflowStepRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count("CheckpointStep")
	stored, ok := r.instances[inst.ID]
	if !ok {
		return engine.NotFound(string(inst.ID))
	}
	if stored.LeaseEpoch != inst.LeaseEpoch {
		return leaseLost(inst.ID, inst.LeaseEpoch, stored.LeaseEpoch)
	}
	if err := r.saveStepLocked(step); err != nil {
		return err
	}
	return r.saveInstanceLocked(inst)
}

// ListSteps returns an instance's step records ordered by sequence then attempt.
func (r *Repo) ListSteps(ctx context.Context, id shared.WorkflowID) ([]ports.WorkflowStepRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count("ListSteps")
	list := r.steps[id]
	out := make([]ports.WorkflowStepRecord, len(list))
	for i := range list {
		out[i] = cloneStep(list[i])
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Sequence != out[b].Sequence {
			return out[a].Sequence < out[b].Sequence
		}
		return out[a].Attempt < out[b].Attempt
	})
	return out, nil
}

// PushDLQ appends a dead-letter entry.
func (r *Repo) PushDLQ(ctx context.Context, id shared.WorkflowID, step string, payload []byte, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count("PushDLQ")
	r.dlq = append(r.dlq, DLQEntry{
		WorkflowID: id,
		Step:       step,
		Payload:    append([]byte(nil), payload...),
		Reason:     reason,
		CreatedAt:  r.now(),
	})
	return nil
}

// CountByState returns the live distribution across the instance state machine.
func (r *Repo) CountByState(ctx context.Context) (map[string]int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count("CountByState")
	out := make(map[string]int, 8)
	for _, rec := range r.instances {
		out[rec.State]++
	}
	return out, nil
}

// FindStuck returns non-final instances that have not been updated within the threshold and
// whose RunAfter has already passed — they should be moving and are not.
func (r *Repo) FindStuck(ctx context.Context, noProgressFor time.Duration, limit int) ([]ports.WorkflowInstanceRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count("FindStuck")
	now := r.now()
	var out []ports.WorkflowInstanceRecord
	for _, rec := range r.instances {
		st := engine.InstanceState(rec.State)
		if st.IsFinal() || st == engine.InstanceWaitingSignal || st == engine.InstancePoisoned {
			// A signal wait is *supposed* to be long and has its own metric and timeout;
			// counting it as stuck would drown the real signal in noise. A quarantined instance
			// has its own, louder alert.
			continue
		}
		if rec.RunAfter.After(now) {
			continue
		}
		if now.Sub(rec.UpdatedAt) <= noProgressFor {
			continue
		}
		out = append(out, clone(rec))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

// --- inspection helpers used by tests ---------------------------------------------------------

// DLQ returns a copy of the dead-letter queue.
func (r *Repo) DLQ() []DLQEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]DLQEntry(nil), r.dlq...)
}

// Instance returns a copy of an instance record, or nil.
func (r *Repo) Instance(id shared.WorkflowID) *ports.WorkflowInstanceRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.instances[id]
	if !ok {
		return nil
	}
	out := clone(rec)
	return &out
}

// StepStates returns each step's latest state, keyed by name.
func (r *Repo) StepStates(id shared.WorkflowID) map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.steps[id]))
	best := make(map[string]int, len(r.steps[id]))
	for _, s := range r.steps[id] {
		if prev, ok := best[s.Name]; !ok || s.Attempt >= prev {
			best[s.Name] = s.Attempt
			out[s.Name] = s.State
		}
	}
	return out
}

// ForceEpoch bumps an instance's lease epoch without going through a lease, simulating another
// worker taking the instance over — or an operator's `workflow release-lease`. It is how the
// fencing test creates a zombie deterministically instead of by sleeping.
func (r *Repo) ForceEpoch(id shared.WorkflowID, owner string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.instances[id]
	if !ok {
		return 0
	}
	rec.LeaseEpoch++
	rec.LeaseOwner = owner
	until := r.now().Add(time.Minute)
	rec.LeaseUntil = &until
	r.instances[id] = rec
	return rec.LeaseEpoch
}

func leaseLost(id shared.WorkflowID, have, want int64) error {
	return apierror.Wrapf(engine.ErrLeaseLost, apierror.CodeWorkflowNotResumable,
		"instance %s: write carried lease epoch %d but the instance is at %d", id, have, want)
}

func clone(i ports.WorkflowInstanceRecord) ports.WorkflowInstanceRecord {
	i.Input = append([]byte(nil), i.Input...)
	i.Context = append([]byte(nil), i.Context...)
	if i.LeaseUntil != nil {
		t := *i.LeaseUntil
		i.LeaseUntil = &t
	}
	if i.CompletedAt != nil {
		t := *i.CompletedAt
		i.CompletedAt = &t
	}
	return i
}

func cloneStep(s ports.WorkflowStepRecord) ports.WorkflowStepRecord {
	s.Input = append([]byte(nil), s.Input...)
	s.Output = append([]byte(nil), s.Output...)
	for _, p := range []**time.Time{&s.StartedAt, &s.CompletedAt, &s.CompensatedAt} {
		if *p != nil {
			t := **p
			*p = &t
		}
	}
	return s
}
