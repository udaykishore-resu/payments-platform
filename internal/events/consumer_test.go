package events

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

const testGroup = "pp.ledger.projection.v1"

// fakeUOW is a unit of work that records whether each attempt committed or rolled back.
//
// It is the assertion vehicle for the property the protocol turns on: the dedup row and the
// handler's work either commit together or not at all.
type fakeUOW struct {
	mu        sync.Mutex
	commits   int
	rollbacks int
}

func (u *fakeUOW) Within(ctx context.Context, fn func(context.Context, ports.Repositories) error) error {
	err := fn(ctx, ports.Repositories{})

	u.mu.Lock()
	defer u.mu.Unlock()
	if err != nil {
		u.rollbacks++
		return err
	}
	u.commits++
	return nil
}

func (u *fakeUOW) WithinSerializable(ctx context.Context, fn func(context.Context, ports.Repositories) error) error {
	return u.Within(ctx, fn)
}

func (u *fakeUOW) counts() (commits, rollbacks int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.commits, u.rollbacks
}

// fakeDedup is an in-memory event_dedup table with the same primary key semantics.
type fakeDedup struct {
	mu    sync.Mutex
	seen  map[string]bool
	err   error
	calls int
}

func newFakeDedup() *fakeDedup {
	return &fakeDedup{seen: map[string]bool{}}
}

func (d *fakeDedup) MarkProcessed(_ context.Context, group string, id shared.EventID) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.err != nil {
		return false, d.err
	}
	k := group + "|" + id.String()
	if d.seen[k] {
		return false, nil
	}
	d.seen[k] = true
	return true, nil
}

func (d *fakeDedup) Purge(context.Context, time.Time, int) (int, error) { return 0, nil }

func (d *fakeDedup) has(group string, id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.seen[group+"|"+id]
}

func (d *fakeDedup) rollback(group, id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.seen, group+"|"+id)
}

// rollbackAwareUOW rolls the fake dedup table back with the transaction, so the tests exercise
// the real semantics rather than a leaky in-memory approximation.
type rollbackAwareUOW struct {
	*fakeUOW
	dedup *fakeDedup
	group string
	id    string
}

func (u *rollbackAwareUOW) Within(ctx context.Context, fn func(context.Context, ports.Repositories) error) error {
	err := u.fakeUOW.Within(ctx, fn)
	if err != nil {
		u.dedup.rollback(u.group, u.id)
	}
	return err
}

func (u *rollbackAwareUOW) WithinSerializable(ctx context.Context, fn func(context.Context, ports.Repositories) error) error {
	return u.Within(ctx, fn)
}

// recordingMetrics captures outcomes so a test can assert the metric, not just the return value.
type recordingMetrics struct {
	mu        sync.Mutex
	outcomes  []Outcome
	conflicts []string
}

func (m *recordingMetrics) RecordConsumerOutcome(_, _ string, o Outcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outcomes = append(m.outcomes, o)
}

func (m *recordingMetrics) RecordInvariantConflict(_, _, resolution string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conflicts = append(m.conflicts, resolution)
}

func (m *recordingMetrics) last() Outcome {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.outcomes) == 0 {
		return ""
	}
	return m.outcomes[len(m.outcomes)-1]
}

// countingHandler records how many times it ran and what it should return.
type countingHandler struct {
	mu       sync.Mutex
	calls    int
	err      error
	endState bool
	checkErr error
}

func (h *countingHandler) HandleTx(context.Context, ports.Repositories, Envelope) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	return h.err
}

func (h *countingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// checkingHandler adds the optional §8.3 case-1 resolver.
type checkingHandler struct {
	*countingHandler
}

func (h checkingHandler) DesiredEndStateHolds(context.Context, ports.Repositories, Envelope) (bool, error) {
	return h.endState, h.checkErr
}

func testMessage(t *testing.T) ports.OutboxMessage {
	t.Helper()
	msg, err := EncodeFact(provenanceCtx(), validFact())
	if err != nil {
		t.Fatalf("EncodeFact: %v", err)
	}
	return msg
}

func TestIdempotentHandlerProcessesANewEvent(t *testing.T) {
	t.Parallel()
	uow, dedup := &fakeUOW{}, newFakeDedup()
	inner := &countingHandler{}
	m := &recordingMetrics{}
	h := NewIdempotentHandler(uow, dedup, testGroup, inner, WithMetrics(m))

	msg := testMessage(t)
	if err := h.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if inner.count() != 1 {
		t.Fatalf("handler ran %d times", inner.count())
	}
	if commits, rollbacks := uow.counts(); commits != 1 || rollbacks != 0 {
		t.Fatalf("commits=%d rollbacks=%d, want 1/0", commits, rollbacks)
	}
	if !dedup.has(testGroup, msg.ID.String()) {
		t.Fatal("the dedup row was not written")
	}
	if m.last() != OutcomeProcessed {
		t.Fatalf("outcome = %q", m.last())
	}
}

// TestIdempotentHandlerDropsDuplicates is the core of the protocol: a redelivery must not reach
// the handler at all.
func TestIdempotentHandlerDropsDuplicates(t *testing.T) {
	t.Parallel()
	uow, dedup := &fakeUOW{}, newFakeDedup()
	inner := &countingHandler{}
	m := &recordingMetrics{}
	h := NewIdempotentHandler(uow, dedup, testGroup, inner, WithMetrics(m))

	msg := testMessage(t)
	if err := h.Handle(context.Background(), msg); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	// The same envelope again — byte-identical, same id, as a real redelivery is.
	if err := h.Handle(context.Background(), msg); err != nil {
		t.Fatalf("redelivery must be acked, got %v", err)
	}
	if inner.count() != 1 {
		t.Fatalf("handler ran %d times on two deliveries; a duplicate was re-applied", inner.count())
	}
	if m.last() != OutcomeDuplicate {
		t.Fatalf("outcome = %q, want duplicate", m.last())
	}
	// And the duplicate rolled back rather than committing an empty transaction.
	if _, rollbacks := uow.counts(); rollbacks != 1 {
		t.Fatalf("rollbacks = %d, want 1 for the duplicate", rollbacks)
	}
}

// TestDedupRowRollsBackWithTheWork proves the property the whole design rests on: a handler
// failure must leave the event unprocessed, not marked-and-unapplied.
func TestDedupRowRollsBackWithTheWork(t *testing.T) {
	t.Parallel()
	dedup := newFakeDedup()
	base := &fakeUOW{}
	msg := testMessage(t)
	uow := &rollbackAwareUOW{fakeUOW: base, dedup: dedup, group: testGroup, id: msg.ID.String()}

	inner := &countingHandler{err: apierror.New(apierror.CodeDependencyFailure, "database unavailable")}
	h := NewIdempotentHandler(uow, dedup, testGroup, inner)

	if err := h.Handle(context.Background(), msg); err == nil {
		t.Fatal("a failing handler must not ack")
	}
	if dedup.has(testGroup, msg.ID.String()) {
		t.Fatal("the dedup row survived a rolled-back transaction: the event is now silently lost")
	}
	if commits, _ := base.counts(); commits != 0 {
		t.Fatalf("commits = %d, want 0 — nothing may commit after the handler failed", commits)
	}

	// The redelivery now genuinely reprocesses.
	inner.err = nil
	if err := h.Handle(context.Background(), msg); err != nil {
		t.Fatalf("redelivery after a failure: %v", err)
	}
	if inner.count() != 2 {
		t.Fatalf("handler ran %d times, want 2", inner.count())
	}
}

func TestRetryableAndNonRetryableErrorsArePropagated(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		err           error
		wantRetryable bool
		wantOutcome   Outcome
	}{
		{"infrastructure", apierror.New(apierror.CodeDependencyFailure, "down"), true, OutcomeRetryable},
		{"gateway unavailable", apierror.New(apierror.CodeGatewayUnavailable, "down"), true, OutcomeRetryable},
		// GATEWAY_TIMEOUT is deliberately NOT retryable: the outcome is unknown, so a blind retry
		// could double-charge. It goes to the DLQ and the reconciler resolves it.
		{"gateway timeout", apierror.New(apierror.CodeGatewayTimeout, "unknown outcome"), false, OutcomeNonRetryable},
		{"validation", apierror.New(apierror.CodeValidationFailed, "bad payload"), false, OutcomeNonRetryable},
		{"not found", apierror.New(apierror.CodePaymentNotFound, "gone"), false, OutcomeNonRetryable},
		{"unclassified", errors.New("boom"), false, OutcomeNonRetryable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			uow, dedup := &fakeUOW{}, newFakeDedup()
			m := &recordingMetrics{}
			h := NewIdempotentHandler(uow, dedup, testGroup, &countingHandler{err: tc.err}, WithMetrics(m))

			err := h.Handle(context.Background(), testMessage(t))
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := apierror.IsRetryable(err); got != tc.wantRetryable {
				t.Fatalf("retryable = %v, want %v (err %v)", got, tc.wantRetryable, err)
			}
			if m.last() != tc.wantOutcome {
				t.Fatalf("outcome = %q, want %q", m.last(), tc.wantOutcome)
			}
		})
	}
}

// TestInvariantConflictAlreadyApplied is §8.3 case 1, "yes" branch.
func TestInvariantConflictAlreadyApplied(t *testing.T) {
	t.Parallel()
	dedup := newFakeDedup()
	base := &fakeUOW{}
	msg := testMessage(t)
	uow := &rollbackAwareUOW{fakeUOW: base, dedup: dedup, group: testGroup, id: msg.ID.String()}

	inner := &countingHandler{
		err:      apierror.New(apierror.CodeRefundExceedsCaptured, "I1 would be violated"),
		endState: true,
	}
	m := &recordingMetrics{}
	h := NewIdempotentHandler(uow, dedup, testGroup, checkingHandler{inner}, WithMetrics(m))

	if err := h.Handle(context.Background(), msg); err != nil {
		t.Fatalf("an already-applied effect must be acked, got %v", err)
	}
	if m.last() != OutcomeAlreadyApplied {
		t.Fatalf("outcome = %q, want already_applied", m.last())
	}
	if len(m.conflicts) != 1 || m.conflicts[0] != "already_applied" {
		t.Fatalf("conflict metric = %v", m.conflicts)
	}
	if !dedup.has(testGroup, msg.ID.String()) {
		t.Fatal("the dedup row was not written after resolving the conflict")
	}
	if inner.count() != 1 {
		t.Fatalf("the handler ran %d times; the effect must never be re-applied", inner.count())
	}
}

// TestInvariantConflictContradiction is §8.3 case 1, "no" branch: never force the write.
func TestInvariantConflictContradiction(t *testing.T) {
	t.Parallel()
	dedup := newFakeDedup()
	base := &fakeUOW{}
	msg := testMessage(t)
	uow := &rollbackAwareUOW{fakeUOW: base, dedup: dedup, group: testGroup, id: msg.ID.String()}

	inner := &countingHandler{
		err:      apierror.New(apierror.CodeRefundExceedsCaptured, "I1 would be violated"),
		endState: false,
	}
	m := &recordingMetrics{}
	h := NewIdempotentHandler(uow, dedup, testGroup, checkingHandler{inner}, WithMetrics(m))

	err := h.Handle(context.Background(), msg)
	if err == nil {
		t.Fatal("a contradiction must not be acked")
	}
	if apierror.IsRetryable(err) {
		t.Fatalf("a contradiction must be non-retryable so it goes straight to the DLQ: %v", err)
	}
	if m.last() != OutcomeInvariantConflict {
		t.Fatalf("outcome = %q", m.last())
	}
	if len(m.conflicts) != 1 || m.conflicts[0] != "contradiction" {
		t.Fatalf("conflict metric = %v", m.conflicts)
	}
	if dedup.has(testGroup, msg.ID.String()) {
		t.Fatal("a contradiction must leave the event unprocessed")
	}
}

// TestInvariantConflictWithoutACheckerRefusesToGuess.
func TestInvariantConflictWithoutACheckerRefusesToGuess(t *testing.T) {
	t.Parallel()
	dedup := newFakeDedup()
	base := &fakeUOW{}
	msg := testMessage(t)
	uow := &rollbackAwareUOW{fakeUOW: base, dedup: dedup, group: testGroup, id: msg.ID.String()}

	inner := &countingHandler{err: apierror.New(apierror.CodeCaptureExceedsAuthorized, "I2 would be violated")}
	m := &recordingMetrics{}
	h := NewIdempotentHandler(uow, dedup, testGroup, inner, WithMetrics(m))

	err := h.Handle(context.Background(), msg)
	if err == nil || apierror.IsRetryable(err) {
		t.Fatalf("want a non-retryable error, got %v", err)
	}
	if m.last() != OutcomeInvariantConflict {
		t.Fatalf("outcome = %q", m.last())
	}
}

// TestOptimisticConcurrencyConflictsAreRetried: docs/events.md §9.2 routes CONFLICT to .retry.5s.
// It is genuinely transient — a competing writer won — and must not be confused with a business
// invariant refusing the work.
func TestOptimisticConcurrencyConflictsAreRetried(t *testing.T) {
	t.Parallel()
	uow, dedup := &fakeUOW{}, newFakeDedup()
	m := &recordingMetrics{}
	conflict := apierror.New(apierror.CodeConfigurationVersionConflict, "version moved")
	h := NewIdempotentHandler(uow, dedup, testGroup, &countingHandler{err: conflict}, WithMetrics(m))

	err := h.Handle(context.Background(), testMessage(t))
	if err == nil {
		t.Fatal("expected an error")
	}
	if m.last() == OutcomeInvariantConflict {
		t.Fatal("an optimistic-concurrency conflict was misclassified as an invariant contradiction")
	}
}

func TestPoisonEnvelopeIsNonRetryable(t *testing.T) {
	t.Parallel()
	uow, dedup := &fakeUOW{}, newFakeDedup()
	inner := &countingHandler{}
	m := &recordingMetrics{}
	h := NewIdempotentHandler(uow, dedup, testGroup, inner, WithMetrics(m))

	msg := testMessage(t)
	msg.Payload = []byte(`{"specversion":"1.0"`)
	err := h.Handle(context.Background(), msg)
	if err == nil || apierror.IsRetryable(err) {
		t.Fatalf("a malformed envelope must be non-retryable, got %v", err)
	}
	if inner.count() != 0 {
		t.Fatal("the handler ran on an undecodable envelope")
	}
	if m.last() != OutcomeNonRetryable {
		t.Fatalf("outcome = %q", m.last())
	}
}

// TestTwoConsumersInOneGroupSerialize is §8.3 case 4: the dedup primary key is the arbiter.
func TestTwoConsumersInOneGroupSerialize(t *testing.T) {
	t.Parallel()
	uow, dedup := &fakeUOW{}, newFakeDedup()
	inner := &countingHandler{}
	h := NewIdempotentHandler(uow, dedup, testGroup, inner)

	msg := testMessage(t)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = h.Handle(context.Background(), msg)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if inner.count() != 1 {
		t.Fatalf("handler ran %d times for one event across 8 concurrent deliveries", inner.count())
	}
}

// TestDifferentGroupsBothProcess: keying on event id alone would let the first group's dedup row
// suppress the second group's processing.
func TestDifferentGroupsBothProcess(t *testing.T) {
	t.Parallel()
	dedup := newFakeDedup()
	ledger := &countingHandler{}
	audit := &countingHandler{}
	hLedger := NewIdempotentHandler(&fakeUOW{}, dedup, "pp.ledger.projection.v1", ledger)
	hAudit := NewIdempotentHandler(&fakeUOW{}, dedup, "pp.audit.sink.v1", audit)

	msg := testMessage(t)
	if err := hLedger.Handle(context.Background(), msg); err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if err := hAudit.Handle(context.Background(), msg); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if ledger.count() != 1 || audit.count() != 1 {
		t.Fatalf("ledger ran %d, audit ran %d; both groups must process the event", ledger.count(), audit.count())
	}
}

func TestDedupStoreFailureIsRetryable(t *testing.T) {
	t.Parallel()
	uow, dedup := &fakeUOW{}, newFakeDedup()
	dedup.err = apierror.New(apierror.CodeDependencyFailure, "dedup table unavailable")
	inner := &countingHandler{}
	h := NewIdempotentHandler(uow, dedup, testGroup, inner)

	err := h.Handle(context.Background(), testMessage(t))
	if err == nil || !apierror.IsRetryable(err) {
		t.Fatalf("want a retryable error, got %v", err)
	}
	if inner.count() != 0 {
		t.Fatal("the handler ran despite the dedup insert failing")
	}
}

func TestHandlerIsAnEventHandlerAndNamesItsGroup(t *testing.T) {
	t.Parallel()
	h := NewIdempotentHandler(&fakeUOW{}, newFakeDedup(), testGroup, &countingHandler{})
	var _ ports.EventHandler = h
	if h.Group() != testGroup {
		t.Fatalf("Group() = %q", h.Group())
	}
	if h.String() == "" {
		t.Fatal("String() is empty")
	}
}

func TestTxHandlerFuncAdapts(t *testing.T) {
	t.Parallel()
	called := false
	var h TxHandler = TxHandlerFunc(func(context.Context, ports.Repositories, Envelope) error {
		called = true
		return nil
	})
	if err := h.HandleTx(context.Background(), ports.Repositories{}, Envelope{}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("TxHandlerFunc did not call through")
	}
}
