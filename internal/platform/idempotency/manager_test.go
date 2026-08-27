package idempotency_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/idempotency"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// --- doubles ---------------------------------------------------------------------------------

// fakeStore is a faithful in-memory model of the Postgres implementation: one mutex standing in
// for the unique index, so a concurrent Claim resolves deterministically rather than racing, and
// an expired lease is reclaimed atomically under the same lock.
type fakeStore struct {
	mu      sync.Mutex
	clock   *shared.FixedClock
	records map[string]*ports.IdempotencyRecord
	state   map[string]string // NEW state: IN_FLIGHT | COMPLETED | FAILED_TERMINAL
	snaps   map[string]ports.ResponseSnapshot
	// inlineSnapshot mirrors the production choice of returning the stored body with the claim
	// result. Turning it off exercises the accelerator's recovery path.
	inlineSnapshot bool
	claimErr       error
	claims         int
}

func newFakeStore(clock *shared.FixedClock) *fakeStore {
	return &fakeStore{
		clock:          clock,
		records:        map[string]*ports.IdempotencyRecord{},
		state:          map[string]string{},
		snaps:          map[string]ports.ResponseSnapshot{},
		inlineSnapshot: true,
	}
}

func skey(k ports.IdempotencyKey) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s", k.TenantID, k.MerchantID, k.Method, k.PathTemplate, k.Key)
}

func (s *fakeStore) Claim(_ context.Context, rec ports.IdempotencyRecord) (ports.ClaimResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	if s.claimErr != nil {
		return ports.ClaimResult{}, s.claimErr
	}
	id := skey(rec.Key)
	existing, ok := s.records[id]
	if !ok {
		cp := rec
		s.records[id], s.state[id] = &cp, "IN_FLIGHT"
		return ports.ClaimResult{Outcome: ports.ClaimNew}, nil
	}
	if existing.Fingerprint != rec.Fingerprint {
		return ports.ClaimResult{Outcome: ports.ClaimFingerprintMismatch}, nil
	}
	switch s.state[id] {
	case "COMPLETED", "FAILED_TERMINAL":
		res := ports.ClaimResult{
			Outcome:       ports.ClaimReplay,
			OriginalReqID: existing.RequestID,
			OriginalTrace: existing.TraceID,
		}
		if s.inlineSnapshot {
			snap := s.snaps[id]
			res.Snapshot = &snap
		}
		return res, nil
	default:
		if s.clock.Now().After(existing.LeaseExpiresAt) {
			cp := rec
			s.records[id] = &cp
			s.state[id] = "IN_FLIGHT"
			return ports.ClaimResult{Outcome: ports.ClaimReclaimed}, nil
		}
		return ports.ClaimResult{
			Outcome:       ports.ClaimInProgress,
			RetryAfter:    time.Second,
			OriginalReqID: existing.RequestID,
			OriginalTrace: existing.TraceID,
		}, nil
	}
}

func (s *fakeStore) Complete(_ context.Context, k ports.IdempotencyKey, snap ports.ResponseSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := skey(k)
	s.state[id], s.snaps[id] = "COMPLETED", snap
	return nil
}

func (s *fakeStore) FailTerminal(_ context.Context, k ports.IdempotencyKey, snap ports.ResponseSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := skey(k)
	s.state[id], s.snaps[id] = "FAILED_TERMINAL", snap
	return nil
}

func (s *fakeStore) Release(_ context.Context, k ports.IdempotencyKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := skey(k)
	delete(s.records, id)
	delete(s.state, id)
	return nil
}

func (s *fakeStore) PurgeExpired(context.Context, time.Time, int) (int, error) { return 0, nil }

// lyingCache answers every read with a plausible but wrong value. It is the instrument for the
// package's central invariant: no cache hit may produce a decision Postgres would not have made.
type lyingCache struct {
	payload []byte
	gets    int
	mu      sync.Mutex
}

func (c *lyingCache) Get(context.Context, string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	return c.payload, true, nil
}
func (c *lyingCache) Set(context.Context, string, []byte, time.Duration) error { return nil }
func (c *lyingCache) Delete(context.Context, string) error                     { return nil }
func (c *lyingCache) GetOrLoad(ctx context.Context, _ string, _ time.Duration, load func(context.Context) ([]byte, error)) ([]byte, error) {
	return load(ctx)
}

type countingMetrics struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCountingMetrics() *countingMetrics { return &countingMetrics{counts: map[string]int{}} }

func (m *countingMetrics) RecordIdempotencyOutcome(outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[outcome]++
}

func (m *countingMetrics) get(k string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[k]
}

// --- fixtures --------------------------------------------------------------------------------

const tenantOne = shared.TenantID("ten_01J0000000000000000000000A")

func testKey(k string) ports.IdempotencyKey {
	return ports.IdempotencyKey{
		TenantID:     tenantOne,
		MerchantID:   "mrc_01J000000000000000000000A",
		Method:       "POST",
		PathTemplate: "/v1/payments",
		Key:          k,
	}
}

func newManager(t *testing.T, cfg idempotency.Config) (*idempotency.Manager, *fakeStore, *shared.FixedClock) {
	t.Helper()
	clock := &shared.FixedClock{T: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	store := newFakeStore(clock)
	cfg.Clock = clock
	m, err := idempotency.NewManager(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m, store, clock
}

func code(err error) apierror.Code {
	var e *apierror.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// --- the outcome matrix ------------------------------------------------------------------------

func TestOutcomeMatrix(t *testing.T) {
	// Verifies: FR-54, FR-57.
	t.Parallel()
	ctx := context.Background()
	body := []byte(`{"amount":1050,"currency":"USD"}`)

	t.Run("first call is new", func(t *testing.T) {
		t.Parallel()
		m, _, _ := newManager(t, idempotency.Config{})
		h, err := m.Begin(ctx, testKey("k"), body)
		if err != nil {
			t.Fatal(err)
		}
		if h.Outcome() != idempotency.OutcomeNew || !h.Outcome().Owns() {
			t.Fatalf("outcome = %s, want NEW", h.Outcome())
		}
	})

	t.Run("duplicate while in flight is 409 with retry-after", func(t *testing.T) {
		t.Parallel()
		m, _, _ := newManager(t, idempotency.Config{})
		if _, err := m.Begin(ctx, testKey("k"), body); err != nil {
			t.Fatal(err)
		}
		h, err := m.Begin(ctx, testKey("k"), body)
		if code(err) != apierror.CodeIdempotentRequestInProgress {
			t.Fatalf("want IDEMPOTENT_REQUEST_IN_PROGRESS, got %v", err)
		}
		if h.Outcome() != idempotency.OutcomeInProgress {
			t.Fatalf("outcome = %s", h.Outcome())
		}
		var e *apierror.Error
		errors.As(err, &e)
		if e.HTTPStatus() != 409 {
			t.Fatalf("status = %d, want 409", e.HTTPStatus())
		}
		if e.RetryAfterSeconds < 1 {
			t.Fatalf("a 409 in-progress must carry Retry-After, got %d", e.RetryAfterSeconds)
		}
		if !e.Retryable {
			t.Fatal("in-progress is a 'come back shortly', so it must be classified retryable")
		}
	})

	t.Run("duplicate after completion replays the stored response", func(t *testing.T) {
		t.Parallel()
		m, _, _ := newManager(t, idempotency.Config{})
		h, err := m.Begin(ctx, testKey("k"), body)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Complete(ctx, 201, []byte(`{"id":"pay_1"}`), "pay_1"); err != nil {
			t.Fatal(err)
		}
		dup, err := m.Begin(ctx, testKey("k"), body)
		if err != nil {
			t.Fatal(err)
		}
		if !dup.IsReplay() {
			t.Fatalf("outcome = %s, want REPLAY", dup.Outcome())
		}
		snap := dup.Snapshot()
		if snap == nil || snap.StatusCode != http.StatusCreated || string(snap.Body) != `{"id":"pay_1"}` || snap.ResourceID != "pay_1" {
			t.Fatalf("replayed snapshot is not verbatim: %+v", snap)
		}
	})

	t.Run("duplicate after terminal failure replays the stored error", func(t *testing.T) {
		t.Parallel()
		m, _, _ := newManager(t, idempotency.Config{})
		h, _ := m.Begin(ctx, testKey("k"), body)
		if err := h.FailTerminal(ctx, 422, []byte(`{"code":"RISK_DECLINED"}`), ""); err != nil {
			t.Fatal(err)
		}
		dup, err := m.Begin(ctx, testKey("k"), body)
		if err != nil {
			t.Fatal(err)
		}
		if !dup.IsReplay() || dup.Snapshot().StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("a terminal failure must replay, got %s %+v", dup.Outcome(), dup.Snapshot())
		}
	})

	t.Run("same key different body is 422", func(t *testing.T) {
		t.Parallel()
		m, _, _ := newManager(t, idempotency.Config{})
		if _, err := m.Begin(ctx, testKey("k"), body); err != nil {
			t.Fatal(err)
		}
		h, err := m.Begin(ctx, testKey("k"), []byte(`{"amount":9999,"currency":"USD"}`))
		if code(err) != apierror.CodeIdempotencyKeyReused {
			t.Fatalf("want IDEMPOTENCY_KEY_REUSED, got %v", err)
		}
		if h.Outcome() != idempotency.OutcomeMismatch {
			t.Fatalf("outcome = %s", h.Outcome())
		}
		var e *apierror.Error
		errors.As(err, &e)
		if e.HTTPStatus() != 422 {
			t.Fatalf("status = %d, want 422", e.HTTPStatus())
		}
		// The error must not echo either body back: the key holder would otherwise be able to
		// read the request that created the record.
		for _, d := range e.Details {
			if d.Message == string(body) {
				t.Fatal("the mismatch error echoed the original request body")
			}
		}
	})

	t.Run("release makes the retry a genuine new attempt", func(t *testing.T) {
		t.Parallel()
		m, _, _ := newManager(t, idempotency.Config{})
		h, _ := m.Begin(ctx, testKey("k"), body)
		if err := h.Release(ctx); err != nil {
			t.Fatal(err)
		}
		retry, err := m.Begin(ctx, testKey("k"), body)
		if err != nil {
			t.Fatal(err)
		}
		if retry.Outcome() != idempotency.OutcomeNew {
			t.Fatalf("outcome after release = %s, want NEW", retry.Outcome())
		}
	})

	t.Run("expired lease is reclaimed", func(t *testing.T) {
		t.Parallel()
		m, _, clock := newManager(t, idempotency.Config{Lease: 30 * time.Second})
		if _, err := m.Begin(ctx, testKey("k"), body); err != nil {
			t.Fatal(err)
		}
		// Still inside the lease: the second caller waits its turn with a 409.
		if _, err := m.Begin(ctx, testKey("k"), body); code(err) != apierror.CodeIdempotentRequestInProgress {
			t.Fatalf("inside the lease: want 409, got %v", err)
		}
		clock.Advance(31 * time.Second)
		h, err := m.Begin(ctx, testKey("k"), body)
		if err != nil {
			t.Fatal(err)
		}
		if h.Outcome() != idempotency.OutcomeReclaimed || !h.Outcome().Owns() {
			t.Fatalf("outcome = %s, want RECLAIMED (and owning)", h.Outcome())
		}
	})

	t.Run("scope separates identical keys", func(t *testing.T) {
		t.Parallel()
		m, _, _ := newManager(t, idempotency.Config{})
		if _, err := m.Begin(ctx, testKey("k"), body); err != nil {
			t.Fatal(err)
		}
		other := testKey("k")
		other.TenantID = shared.TenantID("ten_01J0000000000000000000000B")
		h, err := m.Begin(ctx, other, body)
		if err != nil {
			t.Fatalf("another tenant's identical key must be a separate record: %v", err)
		}
		if h.Outcome() != idempotency.OutcomeNew {
			t.Fatalf("outcome = %s", h.Outcome())
		}
	})
}

func TestBeginRejectsUnusableKeys(t *testing.T) {
	// Verifies: FR-54.
	t.Parallel()
	ctx := context.Background()
	m, _, _ := newManager(t, idempotency.Config{})

	missing := testKey("")
	if _, err := m.Begin(ctx, missing, nil); code(err) != apierror.CodeIdempotencyKeyRequired {
		t.Fatalf("want IDEMPOTENCY_KEY_REQUIRED, got %v", err)
	}
	long := testKey(string(make([]byte, idempotency.MaxKeyLength+1)))
	if _, err := m.Begin(ctx, long, nil); code(err) != apierror.CodeValidationFailed {
		t.Fatalf("want VALIDATION_FAILED, got %v", err)
	}
	noTenant := testKey("k")
	noTenant.TenantID = ""
	if _, err := m.Begin(ctx, noTenant, nil); code(err) != apierror.CodeMissingTenantContext {
		t.Fatalf("want MISSING_TENANT_CONTEXT, got %v", err)
	}
}

func TestHandleSettlementIsSingleShot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, _, _ := newManager(t, idempotency.Config{})

	h, _ := m.Begin(ctx, testKey("k"), []byte(`{}`))
	if err := h.Complete(ctx, 201, []byte(`{"id":"pay_1"}`), "pay_1"); err != nil {
		t.Fatal(err)
	}
	// The idiomatic call site is `defer h.Release(ctx)` plus an explicit Complete. The deferred
	// Release must not delete the record that was just written.
	if err := h.Release(ctx); err != nil {
		t.Fatalf("Release after Complete must be a no-op: %v", err)
	}
	dup, err := m.Begin(ctx, testKey("k"), []byte(`{}`))
	if err != nil || !dup.IsReplay() {
		t.Fatalf("the completed record was destroyed by the deferred Release: %s %v", dup.Outcome(), err)
	}
	if err := h.Complete(ctx, 201, nil, ""); code(err) != apierror.CodeInternalError {
		t.Fatalf("double Complete must be reported, got %v", err)
	}
	// A replay handle owns nothing, so it must refuse to write.
	if err := dup.Complete(ctx, 200, nil, ""); code(err) != apierror.CodeInternalError {
		t.Fatalf("Complete on a replay handle must be refused, got %v", err)
	}
	if err := dup.Release(ctx); err != nil {
		t.Fatalf("Release on a non-owning handle must be a silent no-op: %v", err)
	}
}

func TestStoreFailureIsRetryableDependencyFailure(t *testing.T) {
	t.Parallel()
	m, store, _ := newManager(t, idempotency.Config{})
	store.claimErr = errors.New("connection refused")
	_, err := m.Begin(context.Background(), testKey("k"), []byte(`{}`))
	if code(err) != apierror.CodeDependencyFailure {
		t.Fatalf("want DEPENDENCY_FAILURE, got %v", err)
	}
	if !apierror.IsRetryable(err) {
		t.Fatal("a store outage must be retryable")
	}
}

// --- concurrency ---------------------------------------------------------------------------------

func TestConcurrentBeginYieldsExactlyOneNew(t *testing.T) {
	// Verifies: FR-55, NFR-12.
	t.Parallel()
	const n = 64
	ctx := context.Background()
	m, store, _ := newManager(t, idempotency.Config{})
	body := []byte(`{"amount":1050,"currency":"USD"}`)

	var (
		mu         sync.Mutex
		owners     int
		inProgress int
		others     []string
		start      = make(chan struct{})
		wg         sync.WaitGroup
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			h, err := m.Begin(ctx, testKey("k"), body)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && h.Outcome().Owns():
				owners++
			case code(err) == apierror.CodeIdempotentRequestInProgress:
				inProgress++
			default:
				others = append(others, fmt.Sprintf("%s/%v", h.Outcome(), err))
			}
		}()
	}
	close(start)
	wg.Wait()

	if owners != 1 {
		t.Fatalf("exactly one caller must own the operation, got %d", owners)
	}
	if inProgress != n-1 {
		t.Fatalf("the other %d callers must all get 409, got %d (%v)", n-1, inProgress, others)
	}
	if store.claims != n {
		t.Fatalf("every Begin must reach the authoritative store: %d of %d", store.claims, n)
	}
}

// --- fingerprint ------------------------------------------------------------------------------

func TestFingerprintIsInsensitiveToSerialization(t *testing.T) {
	// Verifies: FR-56.
	t.Parallel()
	key := testKey("k")
	base := idempotency.Fingerprint([]byte(`{"amount":1050,"currency":"USD","meta":{"a":1,"b":2}}`), key)

	same := []struct{ name, body string }{
		{"reordered top-level keys", `{"currency":"USD","meta":{"a":1,"b":2},"amount":1050}`},
		{"reordered nested keys", `{"amount":1050,"currency":"USD","meta":{"b":2,"a":1}}`},
		{"pretty printed", "{\n  \"amount\": 1050,\n  \"currency\": \"USD\",\n  \"meta\": { \"a\": 1, \"b\": 2 }\n}"},
		{"equivalent number forms", `{"amount":1.050e3,"currency":"USD","meta":{"a":1.0,"b":2e0}}`},
	}
	for _, tc := range same {
		t.Run("same/"+tc.name, func(t *testing.T) {
			t.Parallel()
			if got := idempotency.Fingerprint([]byte(tc.body), key); got != base {
				t.Fatalf("%s must fingerprint identically:\n got %s\nwant %s", tc.name, got, base)
			}
		})
	}

	different := []struct{ name, body string }{
		{"changed value", `{"amount":1051,"currency":"USD","meta":{"a":1,"b":2}}`},
		{"changed nested value", `{"amount":1050,"currency":"USD","meta":{"a":1,"b":3}}`},
		{"added member", `{"amount":1050,"currency":"USD","meta":{"a":1,"b":2},"note":"x"}`},
		{"removed member", `{"amount":1050,"currency":"USD"}`},
		{"null is not absent", `{"amount":1050,"currency":"USD","meta":{"a":1,"b":2},"note":null}`},
		{"array order is semantic", `{"amount":1050,"currency":"USD","meta":{"a":1,"b":2},"x":[1,2]}`},
	}
	for _, tc := range different {
		t.Run("different/"+tc.name, func(t *testing.T) {
			t.Parallel()
			if got := idempotency.Fingerprint([]byte(tc.body), key); got == base {
				t.Fatalf("%s must fingerprint differently", tc.name)
			}
		})
	}
}

func TestFingerprintCoversTheScope(t *testing.T) {
	t.Parallel()
	body := []byte(`{"amount":1050}`)
	base := idempotency.Fingerprint(body, testKey("k"))

	mutations := map[string]func(*ports.IdempotencyKey){
		"tenant":   func(k *ports.IdempotencyKey) { k.TenantID = "ten_01J0000000000000000000000B" },
		"merchant": func(k *ports.IdempotencyKey) { k.MerchantID = "mrc_01J000000000000000000000B" },
		"method":   func(k *ports.IdempotencyKey) { k.Method = "PUT" },
		"path":     func(k *ports.IdempotencyKey) { k.PathTemplate = "/v1/payments/{id}/capture" },
		"key":      func(k *ports.IdempotencyKey) { k.Key = "k2" },
	}
	for name, mut := range mutations {
		k := testKey("k")
		mut(&k)
		if idempotency.Fingerprint(body, k) == base {
			t.Fatalf("a different %s must fingerprint differently", name)
		}
	}

	// Length prefixing: "ab"+"c" must not collide with "a"+"bc".
	l := testKey("k")
	l.TenantID, l.MerchantID = "ten_ab", "mrc_c"
	r := testKey("k")
	r.TenantID, r.MerchantID = "ten_a", "mrc_bc"
	if idempotency.Fingerprint(body, l) == idempotency.Fingerprint(body, r) {
		t.Fatal("adjacent scope fields must not be able to blur into each other")
	}
}

func TestFingerprintIsTotalForNonJSONBodies(t *testing.T) {
	t.Parallel()
	k := testKey("k")
	empty := idempotency.Fingerprint(nil, k)
	if empty == "" {
		t.Fatal("an empty body must still fingerprint")
	}
	if idempotency.Fingerprint(nil, k) != empty {
		t.Fatal("fingerprinting must be deterministic")
	}
	if idempotency.Fingerprint([]byte("not json"), k) == empty {
		t.Fatal("distinct unparseable bodies must fingerprint differently")
	}
	// A raw-hashed body must never collide with a canonicalized one.
	if idempotency.Fingerprint([]byte(`{"a":1}`), k) == idempotency.Fingerprint([]byte(`{"a":1} trailing`), k) {
		t.Fatal("the raw and canonical domains must be separated")
	}
}

// --- the accelerator cannot decide anything -------------------------------------------------------

// TestCacheCannotChangeAnyOutcome runs the whole outcome matrix twice — once with no cache, once
// with a cache that answers every read with a plausible but wrong entry — and asserts the
// outcomes are identical. This is the executable form of the package's central invariant.
func TestCacheCannotChangeAnyOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	body := []byte(`{"amount":1050,"currency":"USD"}`)
	other := []byte(`{"amount":9999,"currency":"USD"}`)

	// The lie is a well-formed mirror of a *completed* request with a *different* fingerprint —
	// the most dangerous possible answer, because a naive implementation would turn it into a
	// replay and skip the payment entirely.
	lie := []byte(`{"fingerprint":"0000000000000000000000000000000000000000000000000000000000000000",` +
		`"status_code":200,"body":"eyJpZCI6InBheV9saWUifQ==","resource_id":"pay_lie","completed_at_unix_nano":0}`)

	type step struct {
		name string
		run  func(t *testing.T, m *idempotency.Manager, clock *shared.FixedClock) string
	}
	steps := []step{
		{"new", func(t *testing.T, m *idempotency.Manager, _ *shared.FixedClock) string {
			h, err := m.Begin(ctx, testKey("k"), body)
			return describe(h, err)
		}},
		{"in progress", func(t *testing.T, m *idempotency.Manager, _ *shared.FixedClock) string {
			_, _ = m.Begin(ctx, testKey("k"), body)
			h, err := m.Begin(ctx, testKey("k"), body)
			return describe(h, err)
		}},
		{"replay", func(t *testing.T, m *idempotency.Manager, _ *shared.FixedClock) string {
			h, _ := m.Begin(ctx, testKey("k"), body)
			_ = h.Complete(ctx, 201, []byte(`{"id":"pay_real"}`), "pay_real")
			d, err := m.Begin(ctx, testKey("k"), body)
			return describe(d, err) + "|" + snapshotOf(d)
		}},
		{"terminal replay", func(t *testing.T, m *idempotency.Manager, _ *shared.FixedClock) string {
			h, _ := m.Begin(ctx, testKey("k"), body)
			_ = h.FailTerminal(ctx, 422, []byte(`{"code":"RISK_DECLINED"}`), "")
			d, err := m.Begin(ctx, testKey("k"), body)
			return describe(d, err) + "|" + snapshotOf(d)
		}},
		{"mismatch", func(t *testing.T, m *idempotency.Manager, _ *shared.FixedClock) string {
			_, _ = m.Begin(ctx, testKey("k"), body)
			h, err := m.Begin(ctx, testKey("k"), other)
			return describe(h, err)
		}},
		{"mismatch after completion", func(t *testing.T, m *idempotency.Manager, _ *shared.FixedClock) string {
			h, _ := m.Begin(ctx, testKey("k"), body)
			_ = h.Complete(ctx, 201, []byte(`{"id":"pay_real"}`), "pay_real")
			d, err := m.Begin(ctx, testKey("k"), other)
			return describe(d, err)
		}},
		{"reclaim", func(t *testing.T, m *idempotency.Manager, clock *shared.FixedClock) string {
			_, _ = m.Begin(ctx, testKey("k"), body)
			clock.Advance(2 * time.Minute)
			h, err := m.Begin(ctx, testKey("k"), body)
			return describe(h, err)
		}},
		{"release then retry", func(t *testing.T, m *idempotency.Manager, _ *shared.FixedClock) string {
			h, _ := m.Begin(ctx, testKey("k"), body)
			_ = h.Release(ctx)
			d, err := m.Begin(ctx, testKey("k"), body)
			return describe(d, err)
		}},
	}

	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			t.Parallel()
			noCache, _, clockA := newManager(t, idempotency.Config{Lease: time.Minute})
			want := st.run(t, noCache, clockA)

			cache := &lyingCache{payload: lie}
			withCache, _, clockB := newManager(t, idempotency.Config{Lease: time.Minute, Cache: cache})
			got := st.run(t, withCache, clockB)

			if got != want {
				t.Fatalf("a lying cache changed the outcome:\n with cache: %s\nwithout    : %s", got, want)
			}
		})
	}

	// The same invariant when the store does not inline the response body, which is the only
	// path on which the accelerator is read at all. The lie's fingerprint does not match, so it
	// is discarded rather than served, and the caller gets a retryable failure instead of a
	// fabricated success.
	t.Run("wrong-fingerprint entry is discarded, never served", func(t *testing.T) {
		t.Parallel()
		cache := &lyingCache{payload: lie}
		clock := &shared.FixedClock{T: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
		store := newFakeStore(clock)
		store.inlineSnapshot = false
		m, err := idempotency.NewManager(store, idempotency.Config{Clock: clock, Cache: cache})
		if err != nil {
			t.Fatal(err)
		}
		h, _ := m.Begin(ctx, testKey("k"), body)
		_ = h.Complete(ctx, 201, []byte(`{"id":"pay_real"}`), "pay_real")

		d, err := m.Begin(ctx, testKey("k"), body)
		if d.Outcome() != idempotency.OutcomeReplay {
			t.Fatalf("Postgres decided REPLAY; the cache must not change that. got %s", d.Outcome())
		}
		if err == nil {
			t.Fatal("an unverifiable snapshot must fail rather than serve the cache's guess")
		}
		if code(err) != apierror.CodeDependencyFailure || !apierror.IsRetryable(err) {
			t.Fatalf("want a retryable DEPENDENCY_FAILURE, got %v", err)
		}
		if s := d.Snapshot(); s != nil {
			t.Fatalf("the cache's fabricated response was served: %+v", s)
		}
		if cache.gets == 0 {
			t.Fatal("precondition: the accelerator should have been consulted on this path")
		}
	})

	// And the honest case: a matching mirror is usable, which is the whole point of having one.
	t.Run("matching entry accelerates the replay", func(t *testing.T) {
		t.Parallel()
		cache := &mirrorCache{data: map[string][]byte{}}
		clock := &shared.FixedClock{T: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
		store := newFakeStore(clock)
		m, err := idempotency.NewManager(store, idempotency.Config{Clock: clock, Cache: cache})
		if err != nil {
			t.Fatal(err)
		}
		h, _ := m.Begin(ctx, testKey("k"), body)
		_ = h.Complete(ctx, 201, []byte(`{"id":"pay_real"}`), "pay_real")

		store.inlineSnapshot = false
		d, err := m.Begin(ctx, testKey("k"), body)
		if err != nil {
			t.Fatalf("a fingerprint-matching mirror must be usable: %v", err)
		}
		if s := d.Snapshot(); s == nil || string(s.Body) != `{"id":"pay_real"}` {
			t.Fatalf("mirror did not accelerate the replay: %+v", s)
		}
	})
}

// mirrorCache is an honest in-memory cache, used to show the accelerator working.
type mirrorCache struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (c *mirrorCache) Get(_ context.Context, k string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[k]
	return v, ok, nil
}
func (c *mirrorCache) Set(_ context.Context, k string, v []byte, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[k] = v
	return nil
}
func (c *mirrorCache) Delete(_ context.Context, k string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, k)
	return nil
}
func (c *mirrorCache) GetOrLoad(ctx context.Context, _ string, _ time.Duration, load func(context.Context) ([]byte, error)) ([]byte, error) {
	return load(ctx)
}

func describe(h *idempotency.Handle, err error) string {
	out := "nil-handle"
	if h != nil {
		out = string(h.Outcome())
	}
	return out + "/" + string(code(err))
}

func snapshotOf(h *idempotency.Handle) string {
	s := h.Snapshot()
	if s == nil {
		return "no-snapshot"
	}
	return fmt.Sprintf("%d:%s:%s", s.StatusCode, s.Body, s.ResourceID)
}

// --- metrics ------------------------------------------------------------------------------------

func TestMetricHooks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	metrics := newCountingMetrics()
	m, _, clock := newManager(t, idempotency.Config{Lease: 30 * time.Second, Metrics: metrics})
	body := []byte(`{"a":1}`)

	h, _ := m.Begin(ctx, testKey("k"), body)             // new
	_, _ = m.Begin(ctx, testKey("k"), body)              // in_progress
	_, _ = m.Begin(ctx, testKey("k"), []byte(`{"a":2}`)) // conflict
	_ = h.Complete(ctx, 201, []byte(`{}`), "pay_1")
	_, _ = m.Begin(ctx, testKey("k"), body) // replay

	clock.Advance(time.Hour)
	h2, _ := m.Begin(ctx, testKey("k2"), body)
	clock.Advance(time.Hour)
	_, _ = m.Begin(ctx, testKey("k2"), body) // reclaimed, reported as "new"
	_ = h2.Release(ctx)

	for label, want := range map[string]int{"new": 3, "in_progress": 1, "conflict": 1, "replay": 1} {
		if got := metrics.get(label); got != want {
			t.Fatalf("pp_idempotency_outcomes_total{outcome=%q} = %d, want %d", label, got, want)
		}
	}
	if got := metrics.get("unknown"); got != 0 {
		t.Fatalf("unclassified outcomes: %d", got)
	}
}

func TestOutcomeMetricLabels(t *testing.T) {
	t.Parallel()
	// The label set must match telemetry's IdempotencyOutcome constants exactly; a new label
	// here would be a new series nobody's dashboard reads.
	want := map[idempotency.Outcome]string{
		idempotency.OutcomeNew:        "new",
		idempotency.OutcomeReclaimed:  "new",
		idempotency.OutcomeReplay:     "replay",
		idempotency.OutcomeInProgress: "in_progress",
		idempotency.OutcomeMismatch:   "conflict",
	}
	for o, label := range want {
		if got := o.MetricLabel(); got != label {
			t.Fatalf("%s.MetricLabel() = %q, want %q", o, got, label)
		}
	}
}
