//go:build chaos

package chaos

import (
	"fmt"
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/application/apptest"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
	"github.com/udaykishore-resu/payments-platform/tests/testenv"
)

// Infrastructure faults: C-6 (Postgres primary loss), C-7 (Redis loss), C-8 (Kafka loss), plus
// connection-pool exhaustion.
//
// The three dependencies fail in three different ways *on purpose*, and the difference is the
// architecture:
//
//   - Postgres is the authority. Losing it must fail closed: a retryable error, no partial write,
//     and nothing in an indeterminate state (baseline §1.3 A4, FS-4).
//   - Redis is an accelerator. Losing it must change latency and nothing else (§14.3, FS-6).
//   - Kafka is downstream of the transaction. Losing it must be invisible to the request path,
//     because the outbox absorbs it (§13.4, FS-5).
//
// A system that treated all three the same would be wrong twice.

// TestDatabaseUnavailableMidTransactionFailsClosed is C-6 and FS-4.
//
// Verifies: baseline §1.3 A4 (CP behaviour), §13.4, docs/testing.md §7 FS-4.
//
// Fault: the transaction runs and the commit fails, which is what a primary disappearing
// mid-transaction produces. Injected at the commit rather than at the connection because those are
// two different scenarios and only this one can leave work half-done.
//
// The table walks the fault across each of the orchestrator's transactions, because the window
// matters: failing the first commit means the payment never existed, while failing the commit that
// records the gateway's answer means the vendor may have acted and we have no record of it. The
// second is the case FS-9 and the reconciler exist for, and the assertion is that it never
// resolves itself by *guessing*.
func TestDatabaseUnavailableMidTransactionFailsClosed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// failNth is which transaction fails to commit: 1 is the payment's creation, 2 is the
		// attempt row, 3 is the one that records the gateway's answer.
		failNth int
	}{
		{"the primary dies before the payment is created", 1},
		{"the primary dies while the attempt row is being written", 2},
		{"the primary dies while the gateway's answer is being recorded", 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			h := e.Hypothesis()
			h.HoldsNow(t, "before the fault")

			e.Primary.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(9_000, "EUR"))})

			// Drive the earlier transactions cleanly, then arm the fault so it lands on the one
			// this row is about.
			passThrough(t, e, tc.failNth-1)
			e.UoW.FailDuring(1, errDatabaseUnavailable)

			_, err := e.Create(e.Ctx(), fmt.Sprintf("dbloss-%d", tc.failNth), 9_000)

			// Fail closed, retryably. Never a 500, and never a success.
			if err == nil {
				t.Fatal("a payment succeeded while the database was refusing to commit; something " +
					"reported success for work that was rolled back")
			}
			if !apierror.IsRetryable(err) {
				t.Fatalf("the database outage produced a non-retryable error: %v.\n"+
					"A client that cannot retry a transient database failure has lost a payment it "+
					"was entitled to make.", err)
			}

			// No partial write. The unit of work rolled back, so whatever the callback did is gone.
			for _, p := range e.Store.AllPayments() {
				if p.State() == "CAPTURED" || p.State() == "AUTHORIZED" {
					t.Fatalf("payment %s is %s after a failed commit; the rollback did not take",
						p.ID(), p.State())
				}
			}
			if e.UoW.Faults.Injections() == 0 {
				t.Fatal("the database fault never fired; this scenario asserted nothing")
			}
			h.HoldsNow(t, "after the database outage")
		})
	}
}

// passThrough drives n transactions' worth of work so a later fault lands on the intended one.
//
// It creates and discards throwaway payments rather than counting transactions internally, because
// the number of transactions one payment takes is the orchestrator's business and a test that
// encoded it would break on every refactor that split or merged one.
func passThrough(t *testing.T, e *env, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := e.Create(e.Ctx(), fmt.Sprintf("warmup-%d", i), 1_000+int64(i)); err != nil {
			t.Fatalf("warm-up payment %d failed before any fault was injected: %v", i, err)
		}
	}
}

// TestConnectionPoolExhaustionRejectsRatherThanQueues is the second half of C-6.
//
// Verifies: baseline §1.3 A4, docs/failure-handling.md. A pool with no free connection must refuse
// the request, not queue it: queueing converts a saturated database into a saturated *application*,
// and the thread that would have been freed by a fast rejection is instead held for the acquire
// timeout while the client's own deadline expires.
//
// The distinguishing assertion is that the gateway was never called. A request refused before the
// transaction opens has provably not moved money, which is what makes it safe for the client to
// retry immediately.
func TestConnectionPoolExhaustionRejectsRatherThanQueues(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	h := e.Hypothesis()

	e.Primary.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(4_000, "EUR"))})
	e.UoW.FailBefore(1, errPoolExhausted)

	_, err := e.Create(e.Ctx(), "pool", 4_000)
	if err == nil {
		t.Fatal("a payment succeeded with no database connection available")
	}
	if !apierror.IsRetryable(err) {
		t.Fatalf("pool exhaustion produced a non-retryable error: %v", err)
	}
	if n := len(e.Primary.Calls()); n != 0 {
		t.Fatalf("the gateway was called %d times for a request that could not open a transaction. "+
			"A request refused before the transaction has provably not moved money; one that "+
			"reached the vendor first has not.", n)
	}
	if len(e.Store.AllPayments()) != 0 {
		t.Fatalf("%d payments exist after a request that never opened a transaction",
			len(e.Store.AllPayments()))
	}
	h.HoldsNow(t, "after pool exhaustion")
}

// TestRedisLossDegradesLatencyNotCorrectness is C-7 and FS-6, and it is the important one.
//
// Verifies: baseline §14.3 (Postgres is the authority for idempotency, Redis is an accelerator),
// ADR-009, docs/testing.md §6.3 C-7 and §7 FS-6.
//
// This test is the whole justification for treating Redis as non-authoritative. The claim in the
// architecture is that Redis can disappear entirely and the platform stays *correct* — only slower
// and coarser. A claim like that is worth exactly as much as the test that would fail if it stopped
// being true, and this is that test.
//
// Fault: the velocity counter — the one Redis-backed thing on the payment path — is unavailable for
// the whole window.
//
// Hypothesis: unchanged. Not "mostly unchanged": every payment still reaches the same terminal
// state it would have reached, every invariant still holds, and no payment is duplicated. The only
// permitted difference is that the risk assessment records itself as degraded.
func TestRedisLossDegradesLatencyNotCorrectness(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	h := e.Hypothesis()
	h.HoldsNow(t, "before the fault")

	e.Primary.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(3_000, "EUR"))})

	// Baseline: two payments with the accelerator healthy.
	const healthy = 2
	for i := 0; i < healthy; i++ {
		if _, err := e.Create(e.Ctx(), fmt.Sprintf("redis-up-%d", i), 3_000); err != nil {
			t.Fatalf("a payment failed with Redis healthy: %v", err)
		}
	}
	if got := e.RiskEval.Degraded(); got != 0 {
		t.Fatalf("%d assessments were degraded while Redis was healthy, want 0", got)
	}

	// The fault.
	e.Velocity.Down()
	stop := e.Watch(t, h)
	const during = 5
	for i := 0; i < during; i++ {
		res, err := e.Create(e.Ctx(), fmt.Sprintf("redis-down-%d", i), 3_000)
		if err != nil {
			t.Fatalf("payment %d failed with Redis down: %v.\n"+
				"Redis is a non-authoritative accelerator. A payment that cannot be made without it "+
				"is a payment that depends on it, and the architecture says it must not.", i, err)
		}
		if got := res.Payment.State(); got != "CAPTURED" {
			t.Fatalf("payment %d reached %s with Redis down, want CAPTURED", i, got)
		}
	}
	stop()

	if got := e.RiskEval.Degraded(); got != during {
		t.Fatalf("%d assessments recorded themselves as degraded, want %d. A fallback that is not "+
			"counted is a fallback nobody knows they are running on.", got, during)
	}
	if e.Velocity.Faults.Injections() < during {
		t.Fatalf("the Redis fault fired %d times for %d payments; the accelerator was not actually "+
			"on the path and this scenario proved nothing", e.Velocity.Faults.Injections(), during)
	}

	// Recovery.
	e.Velocity.Up()
	if _, err := e.Create(e.Ctx(), "redis-recovered", 3_000); err != nil {
		t.Fatalf("a payment failed after Redis recovered: %v", err)
	}

	if got := len(e.Store.AllPayments()); got != healthy+during+1 {
		t.Fatalf("%d payments exist, want %d. Redis being unavailable changed how many payments "+
			"were created, which is a correctness difference and not a latency one.",
			got, healthy+during+1)
	}
	h.HoldsNow(t, "after Redis recovered")
}

// TestKafkaUnavailableLosesNoEvents is C-8 and FS-5.
//
// Verifies: baseline §13.4 (the outbox decouples publication from the request path), ADR-020,
// docs/testing.md §6.3 C-8 and §7 FS-5.
//
// Fault: the broker refuses every publish for the whole window, then comes back.
//
// Two assertions, and the first is the one the outbox pattern exists to make true: the payment
// success rate is *unaffected*. Publication is downstream of the commit, so a broker outage must
// be invisible to the caller. The second is that nothing is lost: every event raised during the
// outage is still queued afterwards and drains in per-aggregate order.
func TestKafkaUnavailableLosesNoEvents(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	h := e.Hypothesis()

	e.Primary.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(2_500, "EUR"))})
	e.Publisher.Down()

	const payments = 6
	for i := 0; i < payments; i++ {
		if _, err := e.Create(e.Ctx(), fmt.Sprintf("kafka-%d", i), 2_500); err != nil {
			t.Fatalf("payment %d failed while the broker was down: %v.\n"+
				"The outbox exists precisely so that publication cannot fail a request.", i, err)
		}
	}

	// The events are in the outbox, unpublished. The store is the outbox here; in production it is
	// a table, and the property is identical: the state change and its events committed together,
	// so an event queued is an event that will be published.
	queued := e.Store.OutboxTypes()
	if len(queued) == 0 {
		t.Fatal("no events were queued for six payments; the outbox is not on the path and this " +
			"scenario cannot say anything about losing events")
	}
	if len(e.Publisher.Published()) != 0 {
		t.Fatalf("%d events reached a broker that was refusing every publish",
			len(e.Publisher.Published()))
	}

	// The relay attempts to publish and fails; nothing is dropped.
	relayed, err := drainOutbox(e, queued)
	if err == nil {
		t.Fatal("the relay reported success against a broker that was down")
	}
	if relayed != 0 {
		t.Fatalf("%d events were marked published against a down broker", relayed)
	}

	// Recovery: the backlog drains completely, in order.
	e.Publisher.Up()
	relayed, err = drainOutbox(e, queued)
	if err != nil {
		t.Fatalf("the relay failed after the broker recovered: %v", err)
	}
	if relayed != len(queued) {
		t.Fatalf("the relay published %d of %d queued events after recovery; the rest are lost",
			relayed, len(queued))
	}

	published := e.Publisher.Published()
	if len(published) != len(queued) {
		t.Fatalf("the broker holds %d events, the outbox queued %d", len(published), len(queued))
	}
	for i := range queued {
		if published[i].Type != queued[i] {
			t.Fatalf("event %d published as %q, queued as %q; the backlog drained out of order",
				i, published[i].Type, queued[i])
		}
	}
	h.HoldsNow(t, "after the broker recovered")
}

// drainOutbox is a minimal relay: it publishes the queued events in order and stops at the first
// failure, exactly as the real relay does.
//
// It publishes from the recorded type list rather than from a real outbox table because the
// in-memory store is the outbox in this harness. That limits what this scenario can claim — it
// asserts the *relay contract* (stop on failure, lose nothing, preserve order), not the SQL that
// implements the claim. The SQL is covered against a real PostgreSQL by
// tests/integration/outbox_test.go, and the two together are the whole property.
func drainOutbox(e *env, queued []string) (int, error) {
	published := 0
	for _, typ := range queued {
		msg := ports.OutboxMessage{
			ID:           shared.EventID(testenv.DeterministicID("evt", chaosEpoch, typ+fmt.Sprint(published))),
			TenantID:     tenantID,
			Topic:        "pp.payments.payment.v1",
			Type:         typ,
			AggregateID:  "chaos",
			PartitionKey: "chaos",
			Payload:      []byte(`{}`),
		}
		if err := e.Publisher.Publish(e.Ctx(), msg); err != nil {
			// The row stays claimable. Stopping at the first failure rather than continuing is
			// what preserves per-aggregate order: publishing event 3 after event 2 failed would
			// deliver them out of order to every consumer.
			return published, err
		}
		published++
	}
	return published, nil
}
