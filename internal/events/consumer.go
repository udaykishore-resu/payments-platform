package events

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Outcome is what happened to one delivery. Every one of them is a counter label on
// pp_consumer_events_total, because the ratios between them are the health signal: a duplicate
// rate above a few percent means the relay is re-publishing (a mark-published failure), and any
// non-zero already_applied rate means the dedup table and the database disagree somewhere.
type Outcome string

const (
	// OutcomeProcessed: the handler ran and committed with the dedup row.
	OutcomeProcessed Outcome = "processed"
	// OutcomeDuplicate: the dedup row already existed; dropped and acked.
	OutcomeDuplicate Outcome = "duplicate"
	// OutcomeAlreadyApplied: the handler's invariant rejected the work, but the desired end
	// state already holds, so only the dedup row was written. See case 1 below.
	OutcomeAlreadyApplied Outcome = "already_applied"
	// OutcomeInvariantConflict: the invariant rejected the work and the end state does not hold.
	// A genuine contradiction; routed to the DLQ, paged.
	OutcomeInvariantConflict Outcome = "invariant_conflict"
	// OutcomeRetryable: a transient failure; routed to the next retry tier.
	OutcomeRetryable Outcome = "retryable_error"
	// OutcomeNonRetryable: a permanent failure; routed straight to the DLQ.
	OutcomeNonRetryable Outcome = "non_retryable_error"
)

// Metrics is the narrow recorder the idempotent handler needs.
//
// Declared here, with the consumer, and satisfied by the telemetry registry at wiring time. The
// alternative — importing internal/infrastructure/telemetry from this package — would make every
// producer of an event link a Prometheus registry, and would make this package's tests need one.
type Metrics interface {
	// RecordConsumerOutcome counts one delivery. group and eventType are bounded label sets;
	// the event id is deliberately not a label, because it is unbounded and would melt the
	// time-series database (baseline §22.3).
	RecordConsumerOutcome(group, eventType string, outcome Outcome)
	// RecordInvariantConflict counts the §8.3 case-1 resolutions separately, because
	// "already_applied" is a warning and "contradiction" is a page.
	RecordInvariantConflict(group, eventType, resolution string)
}

// NopMetrics discards. It is the default so that a handler constructed without metrics still
// runs — a consumer that refuses to start because nobody wired a counter is worse than one that
// is temporarily unobservable.
type NopMetrics struct{}

// RecordConsumerOutcome discards the outcome.
func (NopMetrics) RecordConsumerOutcome(string, string, Outcome) {}

// RecordInvariantConflict discards the conflict resolution.
func (NopMetrics) RecordInvariantConflict(string, string, string) {}

// TxHandler is a handler whose work happens inside a transaction supplied by the decorator.
//
// The signature is what makes the protocol enforceable. A plain ports.EventHandler could open
// its own transaction, and then the dedup row and the business effect would commit separately —
// the single failure this whole design exists to prevent. By handing the handler a
// ports.Repositories bundle already bound to the decorator's transaction, there is no way to
// write outside it without going out of your way.
type TxHandler interface {
	HandleTx(ctx context.Context, r ports.Repositories, env Envelope) error
}

// TxHandlerFunc adapts a function to TxHandler.
type TxHandlerFunc func(ctx context.Context, r ports.Repositories, env Envelope) error

// HandleTx implements TxHandler.
func (f TxHandlerFunc) HandleTx(ctx context.Context, r ports.Repositories, env Envelope) error {
	return f(ctx, r, env)
}

// EndStateChecker is the optional half of the invariant-conflict protocol.
//
// A handler that can hit a business invariant — every handler that moves money can — implements
// this to answer the one question §8.3 case 1 asks: *is the effect I was asked to apply already
// applied?* Only the handler can answer it, because only the handler knows what its effect looks
// like in the database.
//
// A handler that does not implement it is treated as answering "no", which routes the conflict to
// the DLQ and pages. That is the safe default: refusing to guess is the whole point.
type EndStateChecker interface {
	// DesiredEndStateHolds reports whether the effect of env is already present. It runs in a
	// fresh read-only transaction, after the failed one rolled back.
	DesiredEndStateHolds(ctx context.Context, r ports.Repositories, env Envelope) (bool, error)
}

// errDuplicate is the sentinel that forces the transaction to roll back on a duplicate.
//
// The protocol says ROLLBACK on a duplicate, and it is worth being precise about why, because
// committing would also "work": the dedup insert affected no rows, so the transaction is empty.
// It is not empty in the presence of a buggy handler that wrote before the check, and it is not
// free — an empty commit still costs a round trip and a WAL flush at 60 000 events/s. Rolling
// back is both cheaper and strictly safer.
var errDuplicate = errors.New("events: duplicate delivery")

// IdempotentHandler makes at-least-once delivery produce an effectively-once business effect.
//
// It implements docs/events.md §8.1 exactly:
//
//	receive
//	  → BEGIN
//	  → INSERT INTO event_dedup (consumer_group, event_id) ON CONFLICT DO NOTHING
//	  → 0 rows affected: ROLLBACK; ACK; drop
//	  → else: handle the event IN THE SAME TRANSACTION
//	  → COMMIT
//	  → ACK
//
// Three properties carry the guarantee, and each is a line of code here:
//
//  1. The dedup insert and the business effect share one transaction. Separate transactions leave
//     a crash window in which the event is either marked processed without its effect (lost) or
//     applied without being marked (duplicated). One transaction makes both unreachable.
//  2. The ACK happens after the commit. The Kafka consumer in
//     internal/infrastructure/kafka commits offsets only after Handle returns nil, which is why
//     auto-commit is disabled there.
//  3. The database invariants are the last line of defence. This decorator is an optimisation
//     that prevents wasted work and non-idempotent side effects; I1 (refunded ≤ captured), I2 and
//     I3 (the partial unique index on successful attempts) are what make double-charging
//     structurally impossible even if every line below is wrong.
//
// # The four dedup-vs-invariant disagreements (docs/events.md §8.3)
//
// The rule underneath all four is that **the invariant always wins**. Dedup is a performance and
// side-effect optimisation; the invariant is the correctness guarantee. A conflict is resolved by
// refusing the write and escalating, never by relaxing the invariant.
//
//	Case 1 — dedup says NEW, the invariant rejects.
//	  Either a duplicate the dedup table lost (expiry, restore, a group rename) or a real
//	  inconsistency. The transaction rolls back, and the dedup row rolls back with it, so the
//	  event is NOT marked processed. This code then asks the handler's EndStateChecker whether
//	  the desired end state already holds:
//	    - yes → the effect is already applied. Log WARN, count
//	      pp_consumer_invariant_conflicts_total{resolution="already_applied"}, insert the dedup
//	      row alone in its own transaction, ACK. Nothing is re-applied.
//	    - no, or no checker → a genuine contradiction. Return a NON-retryable error so the
//	      consumer routes it to .dlq, count resolution="contradiction", and page. The write is
//	      never forced.
//
//	Case 2 — dedup says DUPLICATE, the invariant would have accepted.
//	  The event was processed before, possibly by a buggy earlier handler. This code drops it, as
//	  the protocol says, and does NOT re-apply "just in case" — re-applying is exactly how a
//	  duplicate ledger posting is created. Re-application after a handler fix is a deliberate
//	  replay with a NEW consumer group (§9.5), which this decorator supports by construction
//	  because the dedup key is (group, event id).
//
//	Case 3 — the dedup row exists but the business effect is missing.
//	  Only reachable by a bug that committed the dedup row outside the effect's transaction — which
//	  this type makes unreachable — or by restoring one table and not the other. It is therefore
//	  NOT detectable here at all, and this code does not pretend to detect it: it is found by the
//	  nightly LEDGER_BALANCE reconciliation, which compares the ledger against the payments table
//	  independently of dedup state and opens a CRITICAL exception. Resolution is a targeted replay
//	  with a fresh consumer group, never a manual INSERT.
//
//	Case 4 — two consumers in the same group process the same event.
//	  Reachable during a rebalance when an ACK is delayed past the session timeout. The dedup
//	  table's PRIMARY KEY (consumer_group, event_id) serializes them: one inserts, the other
//	  conflicts and takes the duplicate path. This is the case the dedup table exists for on the
//	  happy path, and it needs no code here beyond using the primary key as the arbiter — which
//	  is why MarkProcessed returns a boolean rather than swallowing the conflict.
//
// Safe for concurrent use: Handle holds no mutable state beyond the metrics recorder.
type IdempotentHandler struct {
	uow     ports.UnitOfWork
	dedup   ports.DedupStore
	group   string
	inner   TxHandler
	checker EndStateChecker
	metrics Metrics

	// logMu guards nothing but the once-per-process warning about a missing EndStateChecker, so
	// that a poison stream of conflicts does not produce a million identical log lines.
	logMu    sync.Mutex
	warned   map[string]struct{}
	onWarn   func(string)
	onDetail func(Outcome, Envelope, error)
}

// NewIdempotentHandler wraps a transactional handler.
//
// group is the consumer group name (`pp.<service>.<purpose>.v<n>`, docs/events.md §10.1). It is a
// constructor parameter rather than being read from the Kafka client because the dedup key must
// be stable across a client reconnection and must be the *logical* group: bumping the group's
// `v<n>` is the replay lever, and that lever only works if the dedup rows are keyed by the same
// string the broker uses.
func NewIdempotentHandler(uow ports.UnitOfWork, dedup ports.DedupStore, group string, inner TxHandler, opts ...IdempotentOption) *IdempotentHandler {
	h := &IdempotentHandler{
		uow:     uow,
		dedup:   dedup,
		group:   group,
		inner:   inner,
		metrics: NopMetrics{},
		warned:  make(map[string]struct{}),
	}
	// A handler that can answer the end-state question usually is the handler.
	if c, ok := inner.(EndStateChecker); ok {
		h.checker = c
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// IdempotentOption configures an IdempotentHandler.
type IdempotentOption func(*IdempotentHandler)

// WithMetrics installs the outcome recorder.
func WithMetrics(m Metrics) IdempotentOption {
	return func(h *IdempotentHandler) {
		if m != nil {
			h.metrics = m
		}
	}
}

// WithEndStateChecker installs the §8.3 case-1 resolver explicitly, for a handler that keeps the
// check in a separate collaborator rather than implementing EndStateChecker itself.
func WithEndStateChecker(c EndStateChecker) IdempotentOption {
	return func(h *IdempotentHandler) { h.checker = c }
}

// WithObserver installs a callback invoked once per delivery with the outcome, the envelope and
// any error. It exists so the consumer can log with the platform logger without this package
// importing it. The callback runs on the handler's goroutine and must not block.
func WithObserver(fn func(Outcome, Envelope, error)) IdempotentOption {
	return func(h *IdempotentHandler) { h.onDetail = fn }
}

// Group returns the consumer group this handler dedupes under.
func (h *IdempotentHandler) Group() string { return h.group }

// Handle implements ports.EventHandler.
//
// The returned error is the routing decision the consumer acts on: nil acks, a retryable
// *apierror.Error goes to the next retry tier, and a non-retryable one goes straight to the DLQ
// (docs/events.md §9.2). Every path below therefore returns a classified error or nil — never a
// bare errors.New, whose retryability would default to false and silently DLQ a transient
// database blip.
func (h *IdempotentHandler) Handle(ctx context.Context, msg ports.OutboxMessage) error {
	env, err := Decode(msg)
	if err != nil {
		// A malformed envelope is poison: it will fail identically for every consumer forever.
		h.record(OutcomeNonRetryable, Envelope{Type: msg.Type, ID: string(msg.ID)}, err)
		return err
	}
	return h.HandleEnvelope(ctx, env)
}

// HandleEnvelope is Handle for a caller that has already decoded, which the retry consumer has.
func (h *IdempotentHandler) HandleEnvelope(ctx context.Context, env Envelope) error {
	duplicate := false

	err := h.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		inserted, err := h.dedup.MarkProcessed(ctx, h.group, env.EventID())
		if err != nil {
			return err
		}
		if !inserted {
			duplicate = true
			return errDuplicate
		}
		return h.inner.HandleTx(ctx, r, env)
	})

	switch {
	case duplicate && errors.Is(err, errDuplicate):
		// Case 2 and the happy half of case 4. Drop; do not re-apply.
		h.record(OutcomeDuplicate, env, nil)
		return nil

	case err == nil:
		h.record(OutcomeProcessed, env, nil)
		return nil

	case isInvariantConflict(err):
		// Case 1. The dedup row rolled back with the work, so the event is still unprocessed.
		return h.resolveInvariantConflict(ctx, env, err)

	case apierror.IsRetryable(err):
		h.record(OutcomeRetryable, env, err)
		return err

	default:
		h.record(OutcomeNonRetryable, env, err)
		return nonRetryable(err, env)
	}
}

// resolveInvariantConflict implements §8.3 case 1.
func (h *IdempotentHandler) resolveInvariantConflict(ctx context.Context, env Envelope, cause error) error {
	if h.checker == nil {
		h.warnOnce("no-end-state-checker:" + env.Type)
		h.metrics.RecordInvariantConflict(h.group, env.Type, "contradiction")
		h.record(OutcomeInvariantConflict, env, cause)
		return contradiction(env, cause,
			"the handler does not implement EndStateChecker, so whether the effect is already applied cannot be decided; refusing to guess")
	}

	var holds bool
	// A fresh transaction: the previous one is rolled back, and this read must not be able to
	// write anything. It is a normal Within rather than WithinSerializable because a read that
	// says "already applied" is only acted on by inserting a dedup row, and a dedup row inserted
	// on a stale read is harmless — the effect is guarded by the invariant either way.
	checkErr := h.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		ok, err := h.checker.DesiredEndStateHolds(ctx, r, env)
		holds = ok
		return err
	})
	if checkErr != nil {
		// Could not decide. That is a transient condition, not a verdict: retry the tier rather
		// than DLQ-ing an event that may be perfectly fine.
		h.record(OutcomeRetryable, env, checkErr)
		return apierror.Wrapf(checkErr, apierror.CodeDependencyFailure,
			"event %s (%s): invariant conflict could not be resolved; end-state check failed", env.ID, env.Type)
	}

	if !holds {
		// A genuine contradiction between the event and the database. Never force the write.
		h.metrics.RecordInvariantConflict(h.group, env.Type, "contradiction")
		h.record(OutcomeInvariantConflict, env, cause)
		return contradiction(env, cause,
			"the event's effect is neither applicable nor already applied; open a CRITICAL reconciliation exception")
	}

	// The effect is already applied — a duplicate the dedup table lost. Mark it processed on its
	// own so the next redelivery takes the cheap duplicate path, and ack.
	markErr := h.uow.Within(ctx, func(ctx context.Context, _ ports.Repositories) error {
		_, err := h.dedup.MarkProcessed(ctx, h.group, env.EventID())
		return err
	})
	if markErr != nil {
		// The effect is applied and we failed only to record that. Retrying is safe and correct:
		// the next attempt takes this same path and reaches the same conclusion.
		h.record(OutcomeRetryable, env, markErr)
		return apierror.Wrapf(markErr, apierror.CodeDependencyFailure,
			"event %s (%s): effect already applied but the dedup row could not be written", env.ID, env.Type)
	}
	h.metrics.RecordInvariantConflict(h.group, env.Type, "already_applied")
	h.record(OutcomeAlreadyApplied, env, cause)
	return nil
}

// isInvariantConflict reports whether err is a business invariant refusing the work, as opposed
// to an infrastructure failure.
//
// The categories are the platform's own classification, which is the reason every layer bothers
// to classify: BUSINESS_RULE and VALIDATION mean "this will never succeed as written", CONFLICT
// on an optimistic-concurrency check means "a competing writer won" and is genuinely transient.
// The CONFLICT category is therefore *excluded* here and handled by the retryable branch, which
// is what docs/events.md §9.2 prescribes (`CONFLICT → .retry.5s`).
func isInvariantConflict(err error) bool {
	if err == nil {
		return false
	}
	switch apierror.CategoryOf(err) {
	case apierror.CategoryBusinessRule:
		return true
	case apierror.CategoryConflict:
		// A version conflict is transient; a "payment already processed" conflict is not.
		return apierror.CodeOf(err) == apierror.CodePaymentAlreadyProcessed && !apierror.IsRetryable(err)
	default:
		return false
	}
}

// contradiction builds the non-retryable error that routes an event to the DLQ and pages.
func contradiction(env Envelope, cause error, why string) error {
	return apierror.Wrapf(cause, apierror.CodeInvalidStateTransition,
		"event %s (%s) contradicts the aggregate's state: %s", env.ID, env.Type, why).
		WithDetail(apierror.Detail{
			Field:   "type",
			Code:    "EVENT_INVARIANT_CONTRADICTION",
			Message: why,
			RuleID:  "L7.EVENT_MATCHES_AGGREGATE_STATE",
		})
}

// nonRetryable ensures an unclassified handler error is still an *apierror with the retryable bit
// explicitly false, so the consumer's routing decision is never made by a nil check on a type
// assertion.
func nonRetryable(err error, env Envelope) error {
	var pe *apierror.Error
	if errors.As(err, &pe) {
		return err
	}
	return apierror.Wrapf(err, apierror.CodeInternalError,
		"event %s (%s) handler failed with an unclassified error", env.ID, env.Type)
}

func (h *IdempotentHandler) record(o Outcome, env Envelope, err error) {
	h.metrics.RecordConsumerOutcome(h.group, env.Type, o)
	if h.onDetail != nil {
		h.onDetail(o, env, err)
	}
}

func (h *IdempotentHandler) warnOnce(key string) {
	h.logMu.Lock()
	_, seen := h.warned[key]
	if !seen {
		h.warned[key] = struct{}{}
	}
	h.logMu.Unlock()
	if seen || h.onWarn == nil {
		return
	}
	h.onWarn(key)
}

// Assert the decorator satisfies the port it decorates.
var _ ports.EventHandler = (*IdempotentHandler)(nil)

// String makes a handler self-describing in a log line or a panic trace.
func (h *IdempotentHandler) String() string {
	return fmt.Sprintf("IdempotentHandler(group=%s)", h.group)
}
