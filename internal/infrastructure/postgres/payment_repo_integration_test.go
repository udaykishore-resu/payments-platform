//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// TestAggregateRoundTripFidelity writes a payment with attempts and refunds and asserts that
// what comes back is the same aggregate, field for field.
//
// Fidelity is not a nicety here. The aggregate's fields are unexported and reachable only
// through Rehydrate, so a column silently dropped by the mapper does not fail to compile and
// does not fail to load — it produces a payment whose decline reason, or whose authorization
// expiry, or whose gateway reference is quietly empty, and the first symptom is a reconciliation
// that cannot find the transaction at the gateway.
func TestAggregateRoundTripFidelity(t *testing.T) {
	pool := testPool(t)
	seedTenants(t, pool)
	ctx := tenantContext(t, tenantAlpha)
	uow := testUnitOfWork(t, pool)
	clock := shared.SystemClock{}

	p := newTestPayment(t, tenantAlpha, 12_345)
	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Payments.Create(ctx, p)
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Drive the aggregate through a realistic lifecycle: an attempt that errors, a failover
	// attempt that succeeds, an authorization, a capture and a partial refund.
	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		loaded, err := r.Payments.GetForUpdate(ctx, p.ID())
		if err != nil {
			return err
		}
		a1, err := loaded.StartAttempt("stripe", shared.NewPlanID(), shared.OpAuthorize, clock)
		if err != nil {
			return err
		}
		if err := a1.Dispatch(clock.Now()); err != nil {
			return err
		}
		if err := a1.Fail("gateway_unreachable", "connection reset", clock.Now()); err != nil {
			return err
		}
		a2, err := loaded.StartAttempt("adyen", shared.NewPlanID(), shared.OpAuthorize, clock)
		if err != nil {
			return err
		}
		if err := a2.Dispatch(clock.Now()); err != nil {
			return err
		}
		if err := a2.Succeed("adyen-ref-1", "Authorised", clock.Now()); err != nil {
			return err
		}
		expiry := clock.Now().Add(7 * 24 * time.Hour)
		if err := loaded.MarkAuthorized(money.MustNew(12_345, "USD"), &expiry, clock); err != nil {
			return err
		}
		if err := loaded.MarkCaptured(money.MustNew(12_345, "USD"), clock); err != nil {
			return err
		}
		if _, err := loaded.AddRefund(money.MustNew(2_345, "USD"),
			payment.RefundReasonRequestedByCustomer, "refund-key-1", clock); err != nil {
			return err
		}
		return r.Payments.Save(ctx, loaded)
	}); err != nil {
		t.Fatalf("lifecycle: %v", err)
	}

	var got *payment.Payment
	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		var err error
		got, err = r.Payments.Get(ctx, p.ID())
		return err
	}); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if got.ID() != p.ID() {
		t.Fatalf("id = %s, want %s", got.ID(), p.ID())
	}
	if !got.Amount().Equal(p.Amount()) {
		t.Fatalf("amount = %s, want %s", got.Amount(), p.Amount())
	}
	if got.State() != payment.StatePartiallyRefunded {
		t.Fatalf("state = %s, want PARTIALLY_REFUNDED", got.State())
	}
	if got.CaptureMethod() != p.CaptureMethod() {
		t.Fatalf("captureMethod = %s, want %s", got.CaptureMethod(), p.CaptureMethod())
	}
	if got.MethodRef().Last4 != "4242" || got.MethodRef().Brand != "visa" {
		t.Fatalf("payment method reference did not round-trip: %+v", got.MethodRef())
	}
	if got.AuthExpiresAt() == nil {
		t.Fatal("the authorization expiry did not round-trip; the auth-expiry sweeper would " +
			"never find this payment")
	}
	if len(got.Attempts()) != 2 {
		t.Fatalf("%d attempts, want 2", len(got.Attempts()))
	}

	// Attempts must come back in sequence order, because the failover logic reads the latest
	// one and "latest" is defined by that ordering.
	attempts := got.Attempts()
	if attempts[0].Sequence() != 1 || attempts[1].Sequence() != 2 {
		t.Fatalf("attempts out of order: %d, %d", attempts[0].Sequence(), attempts[1].Sequence())
	}
	if attempts[0].Outcome() != payment.OutcomeError {
		t.Fatalf("attempt 1 outcome = %s, want ERROR", attempts[0].Outcome())
	}
	if attempts[1].Outcome() != payment.OutcomeSuccess {
		t.Fatalf("attempt 2 outcome = %s, want SUCCESS", attempts[1].Outcome())
	}
	if attempts[1].GatewayRef() != "adyen-ref-1" {
		t.Fatalf("the gateway reference did not round-trip: %q — the reconciler uses this to "+
			"ask the gateway what happened", attempts[1].GatewayRef())
	}
	if attempts[1].IdempotencyKey() == "" {
		t.Fatal("the gateway idempotency key did not round-trip; it cannot be recomputed after " +
			"a crash without it")
	}
	if attempts[0].ErrorCode() != "gateway_unreachable" {
		t.Fatalf("attempt error code = %q, want gateway_unreachable", attempts[0].ErrorCode())
	}

	refunds := got.Refunds()
	if len(refunds) != 1 {
		t.Fatalf("%d refunds, want 1", len(refunds))
	}
	if refunds[0].Amount().Amount() != 2_345 {
		t.Fatalf("refund amount = %d, want 2345", refunds[0].Amount().Amount())
	}
	if refunds[0].Reason() != payment.RefundReasonRequestedByCustomer {
		t.Fatalf("refund reason = %s, want REQUESTED_BY_CUSTOMER", refunds[0].Reason())
	}
	if got.RefundedAmount().Amount() != 2_345 {
		t.Fatalf("refunded total = %d, want 2345", got.RefundedAmount().Amount())
	}
}

// TestOptimisticConcurrencyConflict. Two writers load the same version, both apply a legal
// change, and exactly one wins. The loser must be told it lost rather than silently overwriting
// the winner — a lost update on the money path is a capture or a refund that never happened as
// far as the aggregate is concerned.
func TestOptimisticConcurrencyConflict(t *testing.T) {
	pool := testPool(t)
	seedTenants(t, pool)
	ctx := tenantContext(t, tenantAlpha)
	uow := testUnitOfWork(t, pool)
	clock := shared.SystemClock{}

	p := newTestPayment(t, tenantAlpha, 5_000)
	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Payments.Create(ctx, p)
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Two independent loads of the same version.
	var first, second *payment.Payment
	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		var err error
		first, err = r.Payments.Get(ctx, p.ID())
		return err
	}); err != nil {
		t.Fatalf("load 1: %v", err)
	}
	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		var err error
		second, err = r.Payments.Get(ctx, p.ID())
		return err
	}); err != nil {
		t.Fatalf("load 2: %v", err)
	}
	if first.Version() != second.Version() {
		t.Fatalf("the two loads disagree about the version: %d vs %d",
			first.Version(), second.Version())
	}

	if err := first.MarkProcessing(clock); err != nil {
		t.Fatalf("first transition: %v", err)
	}
	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Payments.Save(ctx, first)
	}); err != nil {
		t.Fatalf("the first writer must win: %v", err)
	}

	if err := second.MarkProcessing(clock); err != nil {
		t.Fatalf("second transition: %v", err)
	}
	err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Payments.Save(ctx, second)
	})
	if err == nil {
		t.Fatal("the second writer overwrote the first; optimistic concurrency is not enforced")
	}
	if got := apierror.CodeOf(err); got != apierror.CodePaymentAlreadyProcessed {
		t.Fatalf("conflict = %s, want PAYMENT_ALREADY_PROCESSED", got)
	}
}

// TestEventsAndStateCommitTogether asserts the outbox row and the state row share a fate.
//
// Both halves matter and they fail differently. A committed state with no event is a consumer
// that never learns a payment was captured; an event with no state is a consumer acting on
// something that did not happen.
func TestEventsAndStateCommitTogether(t *testing.T) {
	pool := testPool(t)
	seedTenants(t, pool)
	ctx := tenantContext(t, tenantAlpha)
	uow := testUnitOfWork(t, pool)

	p := newTestPayment(t, tenantAlpha, 900)

	// A callback that fails after Create must leave neither the payment nor its event.
	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		if err := r.Payments.Create(ctx, p); err != nil {
			return err
		}
		return errDeliberate
	}); err == nil {
		t.Fatal("the unit of work must propagate the callback's error")
	}

	tx, done := rawConn(t, pool, tenantAlpha)
	defer done()
	bg := context.Background()

	var payments, events int
	if err := tx.QueryRow(bg,
		`SELECT count(*) FROM pp.payments WHERE payment_id = $1`, p.ID().String()).
		Scan(&payments); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	if err := tx.QueryRow(bg,
		`SELECT count(*) FROM pp.outbox_events WHERE aggregate_id = $1`, p.ID().String()).
		Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if payments != 0 || events != 0 {
		t.Fatalf("a rolled-back unit of work left %d payment(s) and %d event(s)", payments, events)
	}

	// And a successful one must leave exactly one of each.
	p2 := newTestPayment(t, tenantAlpha, 900)
	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Payments.Create(ctx, p2)
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	tx2, done2 := rawConn(t, pool, tenantAlpha)
	defer done2()
	if err := tx2.QueryRow(bg,
		`SELECT count(*) FROM pp.outbox_events WHERE aggregate_id = $1 AND event_type = $2`,
		p2.ID().String(), string(payment.EventPaymentCreated)).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Fatalf("%d payment.created.v1 rows in the outbox, want exactly 1", events)
	}

	// I5: exactly one event-log row per state change, keyed on the aggregate version.
	var logRows int
	if err := tx2.QueryRow(bg,
		`SELECT count(*) FROM pp.payment_event_log WHERE payment_id = $1`,
		p2.ID().String()).Scan(&logRows); err != nil {
		t.Fatalf("count event log: %v", err)
	}
	if logRows != 1 {
		t.Fatalf("%d payment_event_log rows, want exactly 1 (invariant I5)", logRows)
	}
}

// TestGetIsPartitionPruned asserts the by-ID query carries the partition month.
//
// Without it, `GET /v1/payments/{id}` would probe every one of eighty-four partitions. The
// assertion is on the plan rather than on the result, because a correct result is exactly what a
// full scan also produces — slowly, and only under production data volumes.
func TestGetIsPartitionPruned(t *testing.T) {
	pool := testPool(t)
	seedTenants(t, pool)
	ctx := tenantContext(t, tenantAlpha)
	uow := testUnitOfWork(t, pool)

	p := newTestPayment(t, tenantAlpha, 100)
	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Payments.Create(ctx, p)
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	tx, done := rawConn(t, pool, tenantAlpha)
	defer done()

	rows, err := tx.Query(context.Background(),
		"EXPLAIN "+selectPaymentAggregate,
		p.ID().String(), shared.PartitionMonth(p.ID()), tenantAlpha.String())
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()

	scans := 0
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		if containsAny(line, "payments_20") {
			scans++
		}
	}
	if scans > 3 {
		t.Fatalf("the by-id plan touches %d payment partitions; the partition_month equality "+
			"predicate is not pruning", scans)
	}
}

func containsAny(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// errDeliberate is the failure a test injects into a unit of work to prove the rollback path.
var errDeliberate = apierror.New(apierror.CodeInternalError, "deliberate test failure")
