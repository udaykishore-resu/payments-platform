//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/tests/testenv"
)

// TestPaymentLifecyclePersistsEveryStage walks one payment through
// create → authorize → capture → refund → settle and asserts, at each stage, the four things a
// stage is allowed to have changed: the payment row, the attempt set, the ledger, and the outbox.
//
// Verifies: baseline §9 (payment FSM and invariants I1, I2, I3), §13.4 (outbox atomicity),
// docs/testing.md §4.1 (constraint enforcement, partition alignment).
//
// The value is in the negative half of each assertion. Checking that CAPTURED appears after a
// capture is nearly free of information — the update said CAPTURED. Checking that the attempt
// count did *not* change, that the ledger group balances, and that exactly one outbox row was
// appended is what catches the stage that quietly does something extra.
func TestPaymentLifecyclePersistsEveryStage(t *testing.T) {
	t.Parallel()
	_, s := setup(t)
	c := ctx(t)

	// The whole lifecycle runs inside one rolled-back transaction. Every constraint exercised
	// here (CHECK, unique index, RLS policy, the payments_guard trigger) is immediate, so the
	// rejection a commit would produce is identical to the one this observes — and nothing is
	// left behind in tables the application role is forbidden to delete from.
	err := s.Tenanted(c, s.TenantA, func(tx pgx.Tx) error {
		pay := s.NewPayment(s.TenantA, s.MerchantA, "lifecycle", 5_000)
		if err := pay.Insert(c, tx); err != nil {
			t.Fatalf("insert payment: %v", err)
		}

		type stage struct {
			name string
			// apply performs the stage's writes.
			apply func(tx pgx.Tx) error
			// wantState is the payment state the stage must leave behind.
			wantState string
			// wantAttempts, wantLedgerEntries and wantOutbox are cumulative counts, so a stage
			// that writes an extra row fails here and not three stages later.
			wantAttempts     int64
			wantLedgerRows   int64
			wantOutbox       int64
			wantAuthorized   int64
			wantCaptured     int64
			wantRefunded     int64
			wantLedgerAmount int64
		}

		authorizedAt := s.Clock.Now().Add(time.Minute)
		att := s.NewAttempt(pay, "lifecycle/att1", 1, "SUCCESS")

		stages := []stage{
			{
				name:      "created",
				apply:     func(pgx.Tx) error { return nil },
				wantState: "CREATED",
			},
			{
				name: "authorize",
				apply: func(tx pgx.Tx) error {
					// The attempt row is written before the state change, exactly as the
					// orchestrator does at §12 stage 13, so that a crash between the two leaves
					// evidence of a dispatch rather than nothing.
					if err := att.Insert(c, tx); err != nil {
						return err
					}
					if _, err := tx.Exec(c, `
UPDATE pp.payments
   SET state = 'PROCESSING', version = version + 1, updated_at = $2
 WHERE payment_id = $1 AND partition_month = $3`, pay.ID, s.Clock.Now(), pay.PartitionMonth); err != nil {
						return err
					}
					if _, err := tx.Exec(c, `
UPDATE pp.payments
   SET state = 'AUTHORIZED', authorized_amount = amount, authorized_at = $2,
       current_attempt_id = $4, version = version + 1, updated_at = $2
 WHERE payment_id = $1 AND partition_month = $3`,
						pay.ID, authorizedAt, pay.PartitionMonth, att.ID); err != nil {
						return err
					}
					_, err := s.InsertOutbox(c, tx, s.TenantA, "payment.authorized.v1",
						"pp.payments.payment.v1", pay.ID, pay.ID, "lifecycle/authorized")
					return err
				},
				wantState:      "AUTHORIZED",
				wantAttempts:   1,
				wantOutbox:     1,
				wantAuthorized: 5_000,
			},
			{
				name: "capture",
				apply: func(tx pgx.Tx) error {
					if _, err := tx.Exec(c, `
UPDATE pp.payments
   SET state = 'CAPTURED', captured_amount = 5000, captured_at = $2,
       version = version + 1, updated_at = $2
 WHERE payment_id = $1 AND partition_month = $3`, pay.ID, s.Clock.Now(), pay.PartitionMonth); err != nil {
						return err
					}
					if err := s.InsertLedgerPair(c, tx, pay, "grp-capture-"+pay.ID, "CAPTURE", 5_000); err != nil {
						return err
					}
					_, err := s.InsertOutbox(c, tx, s.TenantA, "payment.captured.v1",
						"pp.payments.payment.v1", pay.ID, pay.ID, "lifecycle/captured")
					return err
				},
				wantState:        "CAPTURED",
				wantAttempts:     1,
				wantLedgerRows:   2,
				wantOutbox:       2,
				wantAuthorized:   5_000,
				wantCaptured:     5_000,
				wantLedgerAmount: 5_000,
			},
			{
				name: "refund",
				apply: func(tx pgx.Tx) error {
					if _, err := tx.Exec(c, `
INSERT INTO pp.refunds (refund_id, tenant_id, payment_id, partition_month, amount, currency,
                        reason, status, idempotency_key, created_at, updated_at, settled_at)
VALUES ($1,$2,$3,$4,$5,'USD','REQUESTED_BY_CUSTOMER','SUCCEEDED',$6,$7,$7,$7)`,
						s.ID(testenv.PrefixRefund, "lifecycle/ref1"), s.TenantA, pay.ID, pay.PartitionMonth,
						2_000, "idem-ref-"+s.Nonce(), s.Clock.Now()); err != nil {
						return err
					}
					if _, err := tx.Exec(c, `
UPDATE pp.payments
   SET state = 'PARTIALLY_REFUNDED', refunded_amount = 2000,
       version = version + 1, updated_at = $2
 WHERE payment_id = $1 AND partition_month = $3`, pay.ID, s.Clock.Now(), pay.PartitionMonth); err != nil {
						return err
					}
					if err := s.InsertLedgerPair(c, tx, pay, "grp-refund-"+pay.ID, "REFUND", 2_000); err != nil {
						return err
					}
					_, err := s.InsertOutbox(c, tx, s.TenantA, "payment.refunded.v1",
						"pp.payments.payment.v1", pay.ID, pay.ID, "lifecycle/refunded")
					return err
				},
				wantState:        "PARTIALLY_REFUNDED",
				wantAttempts:     1,
				wantLedgerRows:   4,
				wantOutbox:       3,
				wantAuthorized:   5_000,
				wantCaptured:     5_000,
				wantRefunded:     2_000,
				wantLedgerAmount: 7_000,
			},
			{
				name: "settle",
				apply: func(tx pgx.Tx) error {
					// PARTIALLY_REFUNDED -> SETTLED is not in pp.payment_state_transitions, and
					// the settlement of a partially refunded payment therefore travels through
					// the refund's own terminal state. Asserting that here rather than working
					// around it is the point: the table is the FSM.
					if _, err := tx.Exec(c, `
UPDATE pp.payments
   SET state = 'REFUNDED', refunded_amount = 5000, version = version + 1, updated_at = $2
 WHERE payment_id = $1 AND partition_month = $3`, pay.ID, s.Clock.Now(), pay.PartitionMonth); err != nil {
						return err
					}
					if err := s.InsertLedgerPair(c, tx, pay, "grp-settle-"+pay.ID, "SETTLEMENT", 3_000); err != nil {
						return err
					}
					_, err := s.InsertOutbox(c, tx, s.TenantA, "payment.settled.v1",
						"pp.payments.payment.v1", pay.ID, pay.ID, "lifecycle/settled")
					return err
				},
				wantState:        "REFUNDED",
				wantAttempts:     1,
				wantLedgerRows:   6,
				wantOutbox:       4,
				wantAuthorized:   5_000,
				wantCaptured:     5_000,
				wantRefunded:     5_000,
				wantLedgerAmount: 10_000,
			},
		}

		for _, st := range stages {
			if err := st.apply(tx); err != nil {
				t.Fatalf("stage %s: %v", st.name, err)
			}

			var state string
			var authorized *int64
			var captured, refunded, version int64
			if err := tx.QueryRow(c, `
SELECT state, authorized_amount, captured_amount, refunded_amount, version
  FROM pp.payments WHERE payment_id = $1 AND partition_month = $2`,
				pay.ID, pay.PartitionMonth).Scan(&state, &authorized, &captured, &refunded, &version); err != nil {
				t.Fatalf("stage %s: read payment: %v", st.name, err)
			}
			if state != st.wantState {
				t.Fatalf("stage %s: payment state = %q, want %q", st.name, state, st.wantState)
			}
			gotAuthorized := int64(0)
			if authorized != nil {
				gotAuthorized = *authorized
			}
			if gotAuthorized != st.wantAuthorized || captured != st.wantCaptured || refunded != st.wantRefunded {
				t.Fatalf("stage %s: amounts authorized/captured/refunded = %d/%d/%d, want %d/%d/%d",
					st.name, gotAuthorized, captured, refunded,
					st.wantAuthorized, st.wantCaptured, st.wantRefunded)
			}
			// I1 and I2 restated as assertions rather than trusted: the CHECK constraints back
			// them, but a test that only ever writes values satisfying them never learns whether
			// the constraints are still there. invariants_test.go attacks them directly.
			if refunded > captured {
				t.Fatalf("stage %s: I1 violated in persisted state: refunded %d > captured %d", st.name, refunded, captured)
			}
			if authorized != nil && captured > *authorized {
				t.Fatalf("stage %s: I2 violated in persisted state: captured %d > authorized %d", st.name, captured, *authorized)
			}

			assertCount(t, c, tx, st.name, "attempts", st.wantAttempts,
				`SELECT count(*) FROM pp.payment_attempts WHERE payment_id = $1 AND partition_month = $2`,
				pay.ID, pay.PartitionMonth)
			assertCount(t, c, tx, st.name, "ledger entries", st.wantLedgerRows,
				`SELECT count(*) FROM pp.ledger_entries WHERE payment_id = $1`, pay.ID)
			assertCount(t, c, tx, st.name, "outbox rows", st.wantOutbox,
				`SELECT count(*) FROM pp.outbox_events WHERE aggregate_id = $1`, pay.ID)

			balanced, err := testenv.LedgerBalanced(c, tx, pay.ID)
			if err != nil {
				t.Fatalf("stage %s: ledger balance query: %v", st.name, err)
			}
			if !balanced {
				t.Fatalf("stage %s: a ledger transaction group does not balance", st.name)
			}

			var sum int64
			if err := tx.QueryRow(c, `
SELECT coalesce(sum(amount) FILTER (WHERE side = 'DEBIT'), 0)
  FROM pp.ledger_entries WHERE payment_id = $1`, pay.ID).Scan(&sum); err != nil {
				t.Fatalf("stage %s: ledger sum: %v", st.name, err)
			}
			if sum != st.wantLedgerAmount {
				t.Fatalf("stage %s: ledger debits total %d, want %d", st.name, sum, st.wantLedgerAmount)
			}
		}

		// I3, asserted on the finished payment rather than only on the attack in invariants_test:
		// exactly one attempt is SUCCESS at the end of a lifecycle with one successful dispatch.
		var successes int64
		if err := tx.QueryRow(c, `
SELECT count(*) FROM pp.payment_attempts
 WHERE payment_id = $1 AND partition_month = $2 AND outcome = 'SUCCESS'`,
			pay.ID, pay.PartitionMonth).Scan(&successes); err != nil {
			t.Fatalf("count successful attempts: %v", err)
		}
		if successes != 1 {
			t.Fatalf("I3: %d successful attempts on one payment, want exactly 1", successes)
		}

		// Partition alignment (amendment A-02): the attempt must live in the payment's partition,
		// not in the month it was created. Without this, I3's per-partition index would stop
		// constraining the full attempt set — silently.
		var attemptMonth, paymentMonth time.Time
		if err := tx.QueryRow(c, `
SELECT a.partition_month, p.partition_month
  FROM pp.payment_attempts a
  JOIN pp.payments p ON p.payment_id = a.payment_id AND p.partition_month = a.partition_month
 WHERE a.attempt_id = $1`, att.ID).Scan(&attemptMonth, &paymentMonth); err != nil {
			t.Fatalf("read partition alignment: %v", err)
		}
		if !attemptMonth.Equal(paymentMonth) {
			t.Fatalf("A-02: attempt partition %s != payment partition %s", attemptMonth, paymentMonth)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("lifecycle transaction: %v", err)
	}
}

// assertCount is the assertion the lifecycle test makes twenty times; naming it keeps each stage
// readable and makes every failure message carry the stage and the thing being counted.
func assertCount(t *testing.T, c context.Context, tx pgx.Tx, stage, what string, want int64, query string, args ...any) {
	t.Helper()
	var n int64
	if err := tx.QueryRow(c, query, args...).Scan(&n); err != nil {
		t.Fatalf("stage %s: count %s: %v", stage, what, err)
	}
	if n != want {
		t.Fatalf("stage %s: %s = %d, want %d", stage, what, n, want)
	}
}
