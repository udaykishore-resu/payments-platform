// Package idempotency implements the request-idempotency contract of baseline §14 on top of
// ports.IdempotencyStore.
//
// The contract in one paragraph: a client sends `Idempotency-Key` with a mutating request; the
// platform claims that key atomically before doing any work; a duplicate arriving while the
// first is still running gets 409 with `Retry-After`; a duplicate arriving after it finished
// gets the stored response replayed verbatim; a duplicate carrying a *different* body gets 422,
// because that is a client bug rather than a retry. Everything in this package is in service of
// those four outcomes being decided by one linearizable operation against Postgres.
//
// # Postgres is authoritative; Redis is a latency accelerator and nothing more
//
// This is the load-bearing rule (baseline §14.3, ADR-009), and it is stated here as a contract
// on the code rather than as advice:
//
//	There is no path in this package where a cache hit produces a decision Postgres would
//	not have made.
//
// Concretely, every Begin calls ports.IdempotencyStore.Claim, unconditionally, before anything
// else is decided. The cache is consulted only *after* the store has already returned REPLAY,
// only to recover the response body when the store did not inline it, and even then the cached
// entry is accepted only if its fingerprint matches the one just computed for this request. A
// cache that returns stale data, another key's data, corrupt data, or an error therefore
// changes latency and nothing else. TestCacheCannotChangeAnyOutcome runs the whole outcome
// matrix against a cache that lies about every entry and asserts the outcomes are identical.
//
// The reason for the severity: making Redis authoritative means that an eviction under memory
// pressure converts a duplicate request into a second payment. Eviction under memory pressure
// is not a rare event, and a double charge is not a recoverable one.
package idempotency

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Outcome is what Begin decided. It is exposed on the handle so that the transport layer can
// set `Idempotent-Replay`, the metric can be labelled, and an operator reading a log line can
// tell a replay from a fresh execution without inferring it from the status code.
type Outcome string

const (
	// OutcomeNew means this caller owns the operation and must execute it.
	OutcomeNew Outcome = "NEW"
	// OutcomeReclaimed means a previous holder's lease expired — its process died mid-flight —
	// and this caller has atomically taken over. Semantically identical to NEW for the caller;
	// distinguished because a rising reclaim rate is a signal that something is crashing.
	OutcomeReclaimed Outcome = "RECLAIMED"
	// OutcomeReplay means the operation already completed and the stored response must be
	// returned verbatim.
	OutcomeReplay Outcome = "REPLAY"
	// OutcomeInProgress means another caller holds a live lease.
	OutcomeInProgress Outcome = "IN_PROGRESS"
	// OutcomeMismatch means the key was reused with a different request body.
	OutcomeMismatch Outcome = "MISMATCH"
)

// Owns reports whether this outcome means the caller must execute the operation. It exists so
// that the two owning outcomes are handled together at every call site; enumerating them
// inline is how RECLAIMED gets forgotten and a recovered request silently returns nothing.
func (o Outcome) Owns() bool { return o == OutcomeNew || o == OutcomeReclaimed }

// MetricLabel is the value of the `outcome` label on pp_idempotency_outcomes_total.
//
// RECLAIMED reports as "new" because from the metric's point of view — how many requests were
// executed versus deduplicated — a reclaim is an execution. The reclaim rate itself is visible
// through the log field and does not need its own series in a metric whose consumers are
// dashboards about deduplication effectiveness.
func (o Outcome) MetricLabel() string {
	switch o {
	case OutcomeNew, OutcomeReclaimed:
		return "new"
	case OutcomeReplay:
		return "replay"
	case OutcomeInProgress:
		return "in_progress"
	case OutcomeMismatch:
		return "conflict"
	default:
		return "unknown"
	}
}

// MetricSink receives the idempotency outcome counter.
//
// It is declared here, by the consumer, and has one method, so a test double is three lines and
// the package does not import the telemetry registry — which would drag Prometheus into a
// package the application layer depends on. The production wiring passes an adapter over
// telemetry.Registry.RecordIdempotencyOutcome.
type MetricSink interface {
	// RecordIdempotencyOutcome increments pp_idempotency_outcomes_total{outcome}. The label is
	// one of "new", "replay", "in_progress", "conflict".
	RecordIdempotencyOutcome(outcome string)
}

// Config configures a Manager.
type Config struct {
	// Lease bounds how long one caller may hold a claim before another may reclaim it. It must
	// exceed the endpoint's own timeout budget: a lease shorter than the work it guards means
	// a second caller reclaims a claim whose first holder is still running, and both execute.
	// The default is generous for the same reason — an over-long lease costs a client one extra
	// retry, an under-short one costs a double charge.
	Lease time.Duration
	// Retention is how long a completed record stays replayable. It must exceed the longest
	// client retry window (baseline §14.3 mandates 7 days), because a record that expires
	// before the client stops retrying turns the last retry into a fresh execution.
	Retention time.Duration
	// RetryAfter is the guidance sent with a 409 IN_PROGRESS.
	RetryAfter time.Duration
	// Clock is injected so lease expiry and retention are testable without sleeping.
	Clock shared.Clock
	// Cache is the optional Redis accelerator. Nil is a fully supported configuration and the
	// behaviour is identical; see the package comment.
	Cache ports.Cache
	// Metrics is optional.
	Metrics MetricSink
}

// Defaults for Config. They are exported so that a deployment overriding one can see what it is
// departing from.
const (
	DefaultLease      = 60 * time.Second
	DefaultRetention  = 7 * 24 * time.Hour
	DefaultRetryAfter = 1 * time.Second
	// MaxKeyLength is the baseline §14.1 bound. It is enforced here rather than only at the
	// transport edge because an unbounded key is an unbounded index entry, and the store's
	// unique index is the mechanism the whole contract rests on.
	MaxKeyLength = 255
)

// Manager decides the outcome of an idempotent request and owns the record's lifecycle.
//
// It holds no per-request state: a Handle does. That split is what makes the Manager safe to
// share across every request goroutine in the process without a lock.
type Manager struct {
	store ports.IdempotencyStore
	cfg   Config
}

// NewManager builds a Manager, filling in defaults for anything the caller left zero.
func NewManager(store ports.IdempotencyStore, cfg Config) (*Manager, error) {
	if store == nil {
		return nil, apierror.New(apierror.CodeInternalError, "idempotency requires an authoritative store")
	}
	if cfg.Lease <= 0 {
		cfg.Lease = DefaultLease
	}
	if cfg.Retention <= 0 {
		cfg.Retention = DefaultRetention
	}
	if cfg.RetryAfter <= 0 {
		cfg.RetryAfter = DefaultRetryAfter
	}
	if cfg.Clock == nil {
		cfg.Clock = shared.SystemClock{}
	}
	return &Manager{store: store, cfg: cfg}, nil
}

// Handle is one caller's relationship to one idempotency record.
//
// It is returned even for the failing outcomes, so the transport layer has the outcome to log
// and to label a metric with. The methods that mutate the record (Complete, FailTerminal,
// Release) are legal only on an owning handle; calling one on a replay is a programming error
// and is reported as an internal error rather than a panic, because a panic in a request path
// takes down requests that had nothing to do with the bug.
type Handle struct {
	mgr         *Manager
	key         ports.IdempotencyKey
	fingerprint string
	outcome     Outcome
	snapshot    *ports.ResponseSnapshot
	retryAfter  time.Duration

	// originalRequestID and originalTraceID identify the request that first claimed the key.
	// On a replay they are the single most useful thing to log: "this is the same operation as
	// req_X" turns an unexplainable duplicate into a two-line investigation.
	originalRequestID string
	originalTraceID   string

	// settled makes Complete/FailTerminal/Release idempotent among themselves. A deferred
	// Release paired with an explicit Complete is the normal shape at the call site, and
	// without this the deferred Release would delete the record that was just written.
	settled atomic.Bool
}

// Outcome reports what Begin decided.
func (h *Handle) Outcome() Outcome { return h.outcome }

// Key returns the full idempotency scope this handle claimed.
func (h *Handle) Key() ports.IdempotencyKey { return h.key }

// Fingerprint returns the request fingerprint computed for this call.
func (h *Handle) Fingerprint() string { return h.fingerprint }

// IsReplay reports whether the stored response should be returned instead of executing.
func (h *Handle) IsReplay() bool { return h.outcome == OutcomeReplay }

// Snapshot returns the stored response for a replay, or nil.
func (h *Handle) Snapshot() *ports.ResponseSnapshot {
	if h.snapshot == nil {
		return nil
	}
	c := *h.snapshot
	c.Body = append([]byte(nil), h.snapshot.Body...)
	return &c
}

// RetryAfter is the guidance to send with a 409 IN_PROGRESS.
func (h *Handle) RetryAfter() time.Duration { return h.retryAfter }

// Origin returns the request and trace identifiers of the call that first claimed this key.
func (h *Handle) Origin() (requestID, traceID string) {
	return h.originalRequestID, h.originalTraceID
}

// Begin claims the operation, or reports why it cannot be claimed.
//
// # Why the second concurrent caller gets a 409 rather than blocking
//
// The obvious alternative — have the duplicate wait on the lease and then return the first
// caller's result — is rejected deliberately (baseline §14.3, ADR-009), for four reasons:
//
//  1. It converts a cheap rejection into an occupied connection, worker slot and database
//     session for the duration of the original request. A client retrying aggressively during a
//     slowdown then multiplies the very resource the slowdown is starving, which is how a
//     latency blip becomes an outage.
//  2. The waiter has its own deadline, and it is usually shorter than the work it is waiting
//     for. It times out, the client retries, and the queue grows. Waiting turns a bounded
//     failure into an unbounded one.
//  3. Blocking is not implementable across replicas without a distributed condition variable —
//     that is, a lock whose TTL can expire mid-wait, which reintroduces exactly the double
//     execution the lease was preventing.
//  4. 409 with `Retry-After: 1` is a complete, actionable answer. The client's own retry is one
//     round trip later, by which time the original has usually completed and the retry replays
//     the real response. The client waits the same wall-clock time either way; the difference is
//     that it waits in its own process rather than in ours.
//
// The same reasoning is why IN_PROGRESS is classified retryable in the error catalog: it is a
// "come back shortly", not a "you did something wrong".
func (m *Manager) Begin(ctx context.Context, key ports.IdempotencyKey, body []byte) (*Handle, error) {
	if err := m.validateKey(ctx, key); err != nil {
		return nil, err
	}
	fp := Fingerprint(body, key)
	now := m.cfg.Clock.Now()

	rec := ports.IdempotencyRecord{
		Key:            key,
		Fingerprint:    fp,
		LeaseExpiresAt: now.Add(m.cfg.Lease),
		ExpiresAt:      now.Add(m.cfg.Retention),
		RequestID:      requestIDFrom(ctx),
		TraceID:        correlationIDFrom(ctx),
	}

	// The authoritative step. It happens on every call, before any cache is consulted, and its
	// result is never overridden by anything below.
	res, err := m.store.Claim(ctx, rec)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeDependencyFailure,
			"the idempotency store is unavailable")
	}

	h := &Handle{
		mgr:               m,
		key:               key,
		fingerprint:       fp,
		originalRequestID: res.OriginalReqID,
		originalTraceID:   res.OriginalTrace,
	}

	switch res.Outcome {
	case ports.ClaimNew, ports.ClaimReclaimed:
		h.outcome = OutcomeNew
		if res.Outcome == ports.ClaimReclaimed {
			h.outcome = OutcomeReclaimed
		}
		m.record(h.outcome)
		return h, nil

	case ports.ClaimReplay:
		h.outcome = OutcomeReplay
		h.snapshot = m.resolveSnapshot(ctx, key, fp, res.Snapshot)
		m.record(h.outcome)
		if h.snapshot == nil {
			// The store said this completed but produced no body, and the accelerator could not
			// supply a fingerprint-matching one either. Returning a fabricated success would be
			// worse than failing: the client would believe an operation succeeded on evidence
			// we do not have. This is retryable, because the row exists and the next attempt
			// will read it.
			return h, apierror.New(apierror.CodeDependencyFailure,
				"the stored response for this idempotency key could not be read")
		}
		return h, nil

	case ports.ClaimInProgress:
		h.outcome = OutcomeInProgress
		h.retryAfter = res.RetryAfter
		if h.retryAfter <= 0 {
			h.retryAfter = m.cfg.RetryAfter
		}
		m.record(h.outcome)
		return h, apierror.New(apierror.CodeIdempotentRequestInProgress, "").
			WithRetryAfter(int(roundUpSeconds(h.retryAfter)))

	case ports.ClaimFingerprintMismatch:
		h.outcome = OutcomeMismatch
		m.record(h.outcome)
		// 422. The detail names the header rather than echoing either body: telling the client
		// what the original body was would let anyone holding a key read back the request that
		// created it.
		return h, apierror.New(apierror.CodeIdempotencyKeyReused, "").
			WithDetail(apierror.Detail{
				Field:   "Idempotency-Key",
				Code:    "KEY_REUSED_WITH_DIFFERENT_BODY",
				Message: "this idempotency key was already used with a different request body; use a new key for a new operation",
				RuleID:  "L5.IDEMPOTENCY_FINGERPRINT_MATCHES",
			})

	default:
		return nil, apierror.Newf(apierror.CodeInternalError,
			"the idempotency store returned an unknown outcome %q", res.Outcome)
	}
}

// Complete stores the successful response and marks the record COMPLETED, so every subsequent
// duplicate replays it.
//
// The store write is authoritative and its failure is returned; the cache write that follows is
// best-effort and its failure is swallowed, because a cache that did not accept a mirror is a
// slower platform, and a caller that fails a successful payment because Redis was busy is a
// broken one.
func (h *Handle) Complete(ctx context.Context, status int, body []byte, resourceID string) error {
	if err := h.settle("Complete"); err != nil {
		return err
	}
	snap := ports.ResponseSnapshot{
		StatusCode:  status,
		Body:        append([]byte(nil), body...),
		ResourceID:  resourceID,
		CompletedAt: h.mgr.cfg.Clock.Now(),
	}
	if err := h.mgr.store.Complete(ctx, h.key, snap); err != nil {
		return apierror.Wrap(err, apierror.CodeDependencyFailure,
			"the idempotency record could not be completed")
	}
	h.mgr.mirrorToCache(ctx, h.key, h.fingerprint, snap)
	return nil
}

// FailTerminal stores a non-retryable failure, so a client retrying a request that can never
// succeed receives the same error rather than a fresh execution.
//
// The distinction between this and Release is the whole reason both exist. A declined payment
// is terminal: re-running it produces the same decline, consumes another gateway call and
// another risk check, and looks to the scheme like retry abuse. A database timeout is not
// terminal: the next attempt may well succeed, and replaying the error would strand the client
// on a failure that has since cleared.
func (h *Handle) FailTerminal(ctx context.Context, status int, body []byte, resourceID string) error {
	if err := h.settle("FailTerminal"); err != nil {
		return err
	}
	snap := ports.ResponseSnapshot{
		StatusCode:  status,
		Body:        append([]byte(nil), body...),
		ResourceID:  resourceID,
		CompletedAt: h.mgr.cfg.Clock.Now(),
	}
	if err := h.mgr.store.FailTerminal(ctx, h.key, snap); err != nil {
		return apierror.Wrap(err, apierror.CodeDependencyFailure,
			"the idempotency record could not be finalized")
	}
	h.mgr.mirrorToCache(ctx, h.key, h.fingerprint, snap)
	return nil
}

// Release drops the claim after a retryable failure, so the client's retry is a genuine new
// attempt.
//
// It is designed to be `defer`red immediately after a successful Begin. Once Complete or
// FailTerminal has run, Release is a no-op, so the deferred call cannot undo the record that
// was just written — the alternative, requiring the call site to clear a flag on every success
// path, is the kind of discipline that holds until the third early return is added.
func (h *Handle) Release(ctx context.Context) error {
	if !h.outcome.Owns() {
		// Releasing a claim you do not hold would delete another caller's live lease.
		return nil
	}
	if !h.settled.CompareAndSwap(false, true) {
		return nil
	}
	if err := h.mgr.store.Release(ctx, h.key); err != nil {
		return apierror.Wrap(err, apierror.CodeDependencyFailure,
			"the idempotency claim could not be released")
	}
	return nil
}

func (h *Handle) settle(op string) error {
	if !h.outcome.Owns() {
		return apierror.Newf(apierror.CodeInternalError,
			"%s called on an idempotency handle with outcome %s", op, h.outcome)
	}
	if !h.settled.CompareAndSwap(false, true) {
		return apierror.Newf(apierror.CodeInternalError,
			"%s called on an already-settled idempotency handle", op)
	}
	return nil
}

func (m *Manager) validateKey(ctx context.Context, key ports.IdempotencyKey) error {
	if key.Key == "" {
		return apierror.New(apierror.CodeIdempotencyKeyRequired, "").
			WithDetail(apierror.Detail{
				Field: "Idempotency-Key", Code: "MISSING",
				Message: "this operation requires an Idempotency-Key header",
				RuleID:  "L1.IDEMPOTENCY_KEY_PRESENT",
			})
	}
	if len(key.Key) > MaxKeyLength {
		return apierror.New(apierror.CodeValidationFailed, "the idempotency key is too long").
			WithDetail(apierror.Detail{
				Field: "Idempotency-Key", Code: "TOO_LONG",
				Message: "an idempotency key must be between 1 and 255 characters",
				RuleID:  "L1.IDEMPOTENCY_KEY_LENGTH",
			})
	}
	if key.TenantID.IsZero() {
		// The scope's tenant comes from the authenticated principal. A zero tenant here means a
		// caller built the key from something else, and the record would land outside every
		// tenant's namespace where any other tenant's key could collide with it.
		return apierror.New(apierror.CodeMissingTenantContext,
			"an idempotency key must be scoped to a tenant")
	}
	if key.Method == "" || key.PathTemplate == "" {
		return apierror.New(apierror.CodeInternalError,
			"an idempotency key must be scoped to a method and path template")
	}
	// Defence in depth: if a tenant context is established, the key's tenant must be that
	// tenant. This catches the one bug the type system cannot — a handler that built the scope
	// from a path parameter instead of from the token.
	if _, err := tenantctx.FromContext(ctx); err == nil {
		if err := tenantctx.AssertTenant(ctx, key.TenantID); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) record(o Outcome) {
	if m.cfg.Metrics != nil {
		m.cfg.Metrics.RecordIdempotencyOutcome(o.MetricLabel())
	}
}

// cachedSnapshot is the accelerator's mirror of a completed record. The fingerprint is stored
// alongside the response precisely so that a cached entry can be *verified* rather than
// trusted: an entry whose fingerprint does not match the request in hand is discarded, which is
// what makes a mis-keyed, stale or corrupted cache incapable of changing an outcome.
type cachedSnapshot struct {
	Fingerprint string `json:"fingerprint"`
	StatusCode  int    `json:"status_code"`
	Body        []byte `json:"body"`
	ResourceID  string `json:"resource_id"`
	CompletedAt int64  `json:"completed_at_unix_nano"`
}

// resolveSnapshot returns the response body for a replay.
//
// Note the order and the guard. The store's own snapshot always wins. The cache is consulted
// only when the store did not inline a body, and the entry is used only if its fingerprint
// matches. There is deliberately no branch in which a cache lookup is performed before the
// store has already decided REPLAY.
func (m *Manager) resolveSnapshot(ctx context.Context, key ports.IdempotencyKey, fp string, fromStore *ports.ResponseSnapshot) *ports.ResponseSnapshot {
	if fromStore != nil {
		m.mirrorToCache(ctx, key, fp, *fromStore)
		c := *fromStore
		c.Body = append([]byte(nil), fromStore.Body...)
		return &c
	}
	if m.cfg.Cache == nil {
		return nil
	}
	raw, ok, err := m.cfg.Cache.Get(ctx, cacheKey(key))
	if err != nil || !ok {
		return nil
	}
	var cs cachedSnapshot
	if err := json.Unmarshal(raw, &cs); err != nil {
		return nil
	}
	if cs.Fingerprint != fp {
		// Either a stale mirror or the wrong key. Both are the accelerator being wrong, and the
		// correct response to the accelerator being wrong is to ignore it.
		return nil
	}
	return &ports.ResponseSnapshot{
		StatusCode:  cs.StatusCode,
		Body:        cs.Body,
		ResourceID:  cs.ResourceID,
		CompletedAt: time.Unix(0, cs.CompletedAt).UTC(),
	}
}

func (m *Manager) mirrorToCache(ctx context.Context, key ports.IdempotencyKey, fp string, snap ports.ResponseSnapshot) {
	if m.cfg.Cache == nil {
		return
	}
	b, err := json.Marshal(cachedSnapshot{
		Fingerprint: fp,
		StatusCode:  snap.StatusCode,
		Body:        snap.Body,
		ResourceID:  snap.ResourceID,
		CompletedAt: snap.CompletedAt.UnixNano(),
	})
	if err != nil {
		return
	}
	// Errors are dropped on purpose: see Complete. The TTL is the retention window, so the
	// mirror can never outlive the record it mirrors.
	_ = m.cfg.Cache.Set(ctx, cacheKey(key), b, m.cfg.Retention)
}

// cacheKey is tenant-prefixed per the isolation matrix (baseline §16.1). The client's key is
// only unique within a tenant, a merchant and an endpoint, so every one of those is in the key;
// omitting any of them lets one tenant's choice of key collide with another's.
func cacheKey(k ports.IdempotencyKey) string {
	return "pp:" + string(k.TenantID) + ":idem:" + string(k.MerchantID) + ":" +
		k.Method + ":" + k.PathTemplate + ":" + k.Key
}

func roundUpSeconds(d time.Duration) int64 {
	s := int64(d / time.Second)
	if d%time.Second != 0 {
		s++
	}
	if s < 1 {
		s = 1
	}
	return s
}

func requestIDFrom(ctx context.Context) string {
	tc, err := tenantctx.FromContext(ctx)
	if err != nil {
		return ""
	}
	return tc.RequestID
}

func correlationIDFrom(ctx context.Context) string {
	tc, err := tenantctx.FromContext(ctx)
	if err != nil {
		return ""
	}
	return tc.CorrelationID
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-21, FR-54, FR-55, FR-56, FR-57, FR-58, NFR-12.
//
// The claim/replay contract: one logical operation per key, a fingerprint that catches a key
// reused with a different body, and a lease that a crashed caller cannot hold forever
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
