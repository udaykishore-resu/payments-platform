//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/tests/testenv"
)

// The money invariants, attacked directly with the domain removed.
//
// Verifies: baseline §9 invariants I1 (refunds never exceed captures), I2 (captures never exceed
// the authorization) and I3 (at most one successful attempt per payment); docs/testing.md §4.1;
// critical paths CP-01 and CP-03.
//
// The domain enforces all three, and internal/domain/payment has the unit tests that prove it. So
// why assert them again here? Because the domain's enforcement is a property of *this* binary and
// of every call site remembering to go through the aggregate. The database's enforcement is a
// property of the data. The day someone writes a repair script, a backfill, or a new service in
// another language, only one of those two is still standing — and these tests are what say it is.
//
// Every statement below is raw SQL issued as pp_app. Nothing constructs an aggregate; that is the
// point. The complementary tests in internal/infrastructure/postgres assert the same constraints
// from inside that package; these add the concurrency cases, which are where a constraint that is
// merely *checked* rather than *enforced* actually fails.

// invariantAttack is one deliberately illegal write and the rejection it must provoke.
type invariantAttack struct {
	name string
	// invariant names the baseline rule, so a failure message says which promise broke.
	invariant string
	// arrange leaves the database in a legal state the attack then violates.
	arrange func(ctx context.Context, tx pgx.Tx, s *testenv.Scope) (testenv.Payment, error)
	// attack issues the illegal statement.
	attack func(ctx context.Context, tx pgx.Tx, s *testenv.Scope, p testenv.Payment) error
	// accept lists the SQLSTATEs that count as the database refusing.
	accept []string
	// constraint, when set, is the constraint the rejection must name. Asserting the name matters:
	// a CHECK that happens to reject for an unrelated reason would otherwise be mistaken for the
	// invariant holding.
	constraint string
}

var invariantAttacks = []invariantAttack{
	{
		name:      "a refund larger than the captured amount",
		invariant: "I1: sum(refunds) <= captured_amount",
		arrange: func(c context.Context, tx pgx.Tx, s *testenv.Scope) (testenv.Payment, error) {
			p := s.NewPayment(s.TenantA, s.MerchantA, "i1/base", 10_000)
			auth := int64(10_000)
			at := s.Clock.Now()
			p.State = "CAPTURED"
			p.AuthorizedAmount = &auth
			p.AuthorizedAt = &at
			p.CapturedAmount = 4_000
			return p, p.Insert(c, tx)
		},
		attack: func(c context.Context, tx pgx.Tx, _ *testenv.Scope, p testenv.Payment) error {
			// One minor unit over. The interesting boundary is not "wildly wrong" — nobody writes
			// that — it is "one more than is left", which is what an off-by-one in a refund
			// aggregation produces.
			_, err := tx.Exec(c, `
UPDATE pp.payments SET refunded_amount = 4001
 WHERE payment_id = $1 AND partition_month = $2`, p.ID, p.PartitionMonth)
			return err
		},
		accept:     []string{testenv.SQLStateCheckViolation},
		constraint: "payments_i1_refund_within_capture",
	},
	{
		name:      "a capture larger than the authorization",
		invariant: "I2: captured_amount <= authorized_amount",
		arrange: func(c context.Context, tx pgx.Tx, s *testenv.Scope) (testenv.Payment, error) {
			p := s.NewPayment(s.TenantA, s.MerchantA, "i2/base", 10_000)
			auth := int64(6_000)
			at := s.Clock.Now()
			p.State = "AUTHORIZED"
			p.AuthorizedAmount = &auth
			p.AuthorizedAt = &at
			return p, p.Insert(c, tx)
		},
		attack: func(c context.Context, tx pgx.Tx, _ *testenv.Scope, p testenv.Payment) error {
			_, err := tx.Exec(c, `
UPDATE pp.payments SET state = 'CAPTURED', captured_amount = 6001
 WHERE payment_id = $1 AND partition_month = $2`, p.ID, p.PartitionMonth)
			return err
		},
		accept:     []string{testenv.SQLStateCheckViolation},
		constraint: "payments_i2_capture_within_auth",
	},
	{
		name:      "a capture on a payment that was never authorized is unbounded and must stay NULL-guarded",
		invariant: "I2: an absent authorization is not a zero authorization",
		arrange: func(c context.Context, tx pgx.Tx, s *testenv.Scope) (testenv.Payment, error) {
			// authorized_amount NULL means there was never an authorization step — a single-step
			// automatic capture. Writing 0 there instead of NULL would make every auto-captured
			// payment violate I2 the instant it captured, so the constraint is NULL-guarded and
			// this row must be *accepted*.
			//
			// The payment starts in PROCESSING because CREATED -> CAPTURED is not in the §9
			// transition table: an automatic capture still passes through PROCESSING, and seeding
			// CREATED would make the FSM guard refuse the write for a reason that has nothing to
			// do with I2.
			p := s.NewPayment(s.TenantA, s.MerchantA, "i2/autocapture", 10_000)
			p.State = "PROCESSING"
			return p, p.Insert(c, tx)
		},
		attack: func(c context.Context, tx pgx.Tx, _ *testenv.Scope, p testenv.Payment) error {
			_, err := tx.Exec(c, `
UPDATE pp.payments SET state = 'CAPTURED', captured_amount = 10000
 WHERE payment_id = $1 AND partition_month = $2`, p.ID, p.PartitionMonth)
			return err
		},
		accept: nil, // nil means "this write must succeed"
	},
	{
		name:      "an authorized amount recorded without an authorization time",
		invariant: "I2 support: an amount with no moment is not evidence of anything",
		arrange: func(c context.Context, tx pgx.Tx, s *testenv.Scope) (testenv.Payment, error) {
			p := s.NewPayment(s.TenantA, s.MerchantA, "i2/notime", 10_000)
			return p, p.Insert(c, tx)
		},
		attack: func(c context.Context, tx pgx.Tx, _ *testenv.Scope, p testenv.Payment) error {
			_, err := tx.Exec(c, `
UPDATE pp.payments SET authorized_amount = 9000, authorized_at = NULL
 WHERE payment_id = $1 AND partition_month = $2`, p.ID, p.PartitionMonth)
			return err
		},
		accept:     []string{testenv.SQLStateCheckViolation},
		constraint: "payments_auth_amount_needs_auth_time",
	},
	{
		name:      "a second successful attempt on one payment",
		invariant: "I3: at most one SUCCESS attempt per payment",
		arrange: func(c context.Context, tx pgx.Tx, s *testenv.Scope) (testenv.Payment, error) {
			p := s.NewPayment(s.TenantA, s.MerchantA, "i3/base", 10_000)
			if err := p.Insert(c, tx); err != nil {
				return p, err
			}
			a := s.NewAttempt(p, "i3/att1", 1, "SUCCESS")
			return p, a.Insert(c, tx)
		},
		attack: func(c context.Context, tx pgx.Tx, s *testenv.Scope, p testenv.Payment) error {
			// A *different* attempt number and a different gateway key, so nothing but the I3
			// index itself can refuse it. Reusing the attempt number would trip uq_attempt_number
			// and the test would pass while I3 was gone.
			second := s.NewAttempt(p, "i3/att2", 2, "SUCCESS")
			second.GatewayID = "other-gateway"
			return second.Insert(c, tx)
		},
		accept:     []string{testenv.SQLStateUniqueViolation},
		constraint: "", // the index is created per partition, so its name carries the month
	},
	{
		name:      "a second attempt that failed is legitimate and must be accepted",
		invariant: "I3 is about SUCCESS, not about attempts",
		arrange: func(c context.Context, tx pgx.Tx, s *testenv.Scope) (testenv.Payment, error) {
			p := s.NewPayment(s.TenantA, s.MerchantA, "i3/failover", 10_000)
			if err := p.Insert(c, tx); err != nil {
				return p, err
			}
			retryable := true
			a := s.NewAttempt(p, "i3/failover/att1", 1, "DECLINED")
			a.Retryable = &retryable
			return p, a.Insert(c, tx)
		},
		attack: func(c context.Context, tx pgx.Tx, s *testenv.Scope, p testenv.Payment) error {
			// The failover case: attempt 1 soft-declined, attempt 2 succeeded at another gateway.
			// A partial unique index widened to all outcomes would refuse this and break every
			// legitimate failover, so the negative case is as load-bearing as the positive one.
			second := s.NewAttempt(p, "i3/failover/att2", 2, "SUCCESS")
			second.GatewayID = "other-gateway"
			return second.Insert(c, tx)
		},
		accept: nil,
	},
	{
		name:      "an attempt written into a month other than its payment's",
		invariant: "A-02: an attempt shares its payment's partition, or I3 stops constraining the set",
		arrange: func(c context.Context, tx pgx.Tx, s *testenv.Scope) (testenv.Payment, error) {
			p := s.NewPayment(s.TenantA, s.MerchantA, "a02/base", 10_000)
			return p, p.Insert(c, tx)
		},
		attack: func(c context.Context, tx pgx.Tx, s *testenv.Scope, p testenv.Payment) error {
			next := s.Clock.Now().AddDate(0, 1, 0)
			a := s.NewAttempt(p, "a02/att", 1, "SUCCESS")
			a.PartitionMonth = testenv.PartitionMonth(next)
			return a.Insert(c, tx)
		},
		// The composite foreign key to (payment_id, partition_month) is what makes this
		// unwritable. Without it the row would land in a neighbouring partition where the
		// per-partition I3 index cannot see the first success — silently, with no error.
		accept: []string{testenv.SQLStateForeignKeyViolation},
	},
	{
		name:      "a refund row for a payment in a different month",
		invariant: "A-02 applied to refunds",
		arrange: func(c context.Context, tx pgx.Tx, s *testenv.Scope) (testenv.Payment, error) {
			p := s.NewPayment(s.TenantA, s.MerchantA, "a02/ref/base", 10_000)
			return p, p.Insert(c, tx)
		},
		attack: func(c context.Context, tx pgx.Tx, s *testenv.Scope, p testenv.Payment) error {
			next := testenv.PartitionMonth(s.Clock.Now().AddDate(0, 1, 0))
			_, err := tx.Exec(c, `
INSERT INTO pp.refunds (refund_id, tenant_id, payment_id, partition_month, amount, currency,
                        reason, status, created_at, updated_at)
VALUES ($1,$2,$3,$4,100,'USD','OTHER','PENDING',$5,$5)`,
				s.ID(testenv.PrefixRefund, "a02/ref"), s.TenantA, p.ID, next, s.Clock.Now())
			return err
		},
		accept: []string{testenv.SQLStateForeignKeyViolation},
	},
	{
		name:      "a payment whose partition does not match its own creation month",
		invariant: "A-02: a by-id lookup must prune to the partition the row is actually in",
		arrange: func(c context.Context, tx pgx.Tx, s *testenv.Scope) (testenv.Payment, error) {
			return s.NewPayment(s.TenantA, s.MerchantA, "a02/mismatch", 10_000), nil
		},
		attack: func(c context.Context, tx pgx.Tx, s *testenv.Scope, p testenv.Payment) error {
			p.PartitionMonth = testenv.PartitionMonth(s.Clock.Now().AddDate(0, 1, 0))
			return p.Insert(c, tx)
		},
		accept:     []string{testenv.SQLStateCheckViolation},
		constraint: "payments_partition_matches_created_at",
	},
}

// TestDatabaseRefusesEveryInvariantViolation is CP-01 and CP-03.
//
// Each row arranges a legal state, issues one illegal statement, and asserts the SQLSTATE — never
// the message. A message is prose a future migration may reword; 23505 is the contract.
//
// The whole case runs in a transaction that is rolled back. Every constraint exercised is
// immediate — a CHECK, a unique index and a foreign key all fire at statement time — so the
// rejection a commit would produce is identical to the one observed here, and nothing is left in
// the tables where DELETE is revoked.
func TestDatabaseRefusesEveryInvariantViolation(t *testing.T) {
	t.Parallel()
	_, s := setup(t)

	for _, tc := range invariantAttacks {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := ctx(t)

			err := s.Tenanted(c, s.TenantA, func(tx pgx.Tx) error {
				p, err := tc.arrange(c, tx, s)
				if err != nil {
					t.Fatalf("arrange a legal state: %v", err)
				}
				attackErr := tc.attack(c, tx, s, p)

				if tc.accept == nil {
					if attackErr != nil {
						t.Fatalf("%s: this write is legitimate and the database refused it "+
							"(SQLSTATE %s, constraint %q): %v\nA constraint that is too strict "+
							"breaks correct behaviour just as surely as one that is too loose.",
							tc.invariant, testenv.PgErrCode(attackErr),
							testenv.PgConstraint(attackErr), attackErr)
					}
					return nil
				}

				testenv.RequireDBRejection(t, attackErr, tc.invariant, tc.accept...)
				if tc.constraint != "" && testenv.PgConstraint(attackErr) != tc.constraint {
					t.Fatalf("%s: rejected by constraint %q, want %q. A rejection from the wrong "+
						"constraint means the invariant's own guard may already be gone.",
						tc.invariant, testenv.PgConstraint(attackErr), tc.constraint)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("case transaction: %v", err)
			}
		})
	}
}

// TestI3HoldsWhenTwoAttemptsSucceedConcurrently is the version of CP-01 a single-threaded test
// cannot express.
//
// Verifies: baseline §9 I3. Two transactions each insert a SUCCESS attempt for one payment, at the
// same instant, from different connections. A domain-level check would let both through — each
// reads a payment with no successful attempt and each writes a legal-looking row. The partial
// unique index is what makes the second one impossible, and it is only observable under exactly
// this race.
//
// The rows are committed, because a rolled-back transaction cannot contend with anything. That
// makes them permanent — DELETE is revoked on pp.payments and pp.payment_attempts — which is why
// the identifiers carry the run token.
func TestI3HoldsWhenTwoAttemptsSucceedConcurrently(t *testing.T) {
	t.Parallel()
	_, s := setup(t)
	c := ctx(t)

	pay := s.NewPayment(s.TenantA, s.MerchantA, committedSeed("i3/race/payment"), 10_000)
	if err := s.TenantedCommitted(c, s.TenantA, func(tx pgx.Tx) error {
		return pay.Insert(c, tx)
	}); err != nil {
		t.Fatalf("seed the payment: %v", err)
	}

	// Four, not more: pp.payment_attempts constrains attempt_number to 1..4, because the routing
	// plan can never produce a fifth. A racer with attempt_number 5 would be refused by that CHECK
	// rather than by the I3 index, and the test would pass while proving nothing.
	const racers = 4
	var (
		mu        sync.Mutex
		succeeded int
		rejected  int
		other     []error
	)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start

			attempt := s.NewAttempt(pay, committedSeed("i3/race/att", string(rune('a'+i))), i+1, "SUCCESS")
			attempt.GatewayID = "sim-" + string(rune('a'+i))
			runCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			err := s.TenantedCommitted(runCtx, s.TenantA, func(tx pgx.Tx) error {
				return attempt.Insert(runCtx, tx)
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case testenv.PgErrCode(err) == testenv.SQLStateUniqueViolation:
				rejected++
			default:
				other = append(other, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("%d racers failed for a reason other than the I3 index; the first was: %v",
			len(other), other[0])
	}
	if succeeded != 1 {
		t.Fatalf("%d of %d concurrent SUCCESS attempts were accepted, want exactly 1. "+
			"I3 is not enforced under concurrency, which is the only condition that matters: "+
			"two successful attempts on one payment is a double charge.", succeeded, racers)
	}
	if rejected != racers-1 {
		t.Fatalf("%d racers were rejected by the unique index, want %d", rejected, racers-1)
	}

	// And the state the database is actually left in, read back rather than inferred from the
	// outcome tally.
	requireCount(t, s, s.TenantA, 1, "SUCCESS attempts on the payment after the race",
		`SELECT count(*) FROM pp.payment_attempts
		  WHERE payment_id = $1 AND partition_month = $2 AND outcome = 'SUCCESS'`,
		pay.ID, pay.PartitionMonth)
}

// TestConcurrentPartialRefundsCannotExceedTheCapturedAmount is I1 under contention.
//
// Verifies: baseline §9 I1, docs/testing.md §4.1. Four callers each refund 30 % of a captured
// payment. Under READ COMMITTED, each reads the same `refunded_amount`, each computes a legal
// increment, and together they would refund 120 % — the classic lost-update. The CHECK constraint
// refuses the ones that would overshoot regardless of what the readers believed, which is why the
// constraint exists in addition to the domain rule.
func TestConcurrentPartialRefundsCannotExceedTheCapturedAmount(t *testing.T) {
	t.Parallel()
	_, s := setup(t)
	c := ctx(t)

	const captured = int64(10_000)
	const each = int64(3_000)
	const callers = 4

	pay := s.NewPayment(s.TenantA, s.MerchantA, committedSeed("i1/race/payment"), captured)
	auth := captured
	at := s.Clock.Now()
	pay.State = "CAPTURED"
	pay.AuthorizedAmount = &auth
	pay.AuthorizedAt = &at
	pay.CapturedAmount = captured
	if err := s.TenantedCommitted(c, s.TenantA, func(tx pgx.Tx) error {
		return pay.Insert(c, tx)
	}); err != nil {
		t.Fatalf("seed the captured payment: %v", err)
	}

	var (
		mu       sync.Mutex
		accepted int
		refused  int
		other    []error
	)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			runCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// A read-modify-write, exactly as a naive implementation would do it. The point is
			// that the database refuses the overshoot even though this code does not.
			err := s.TenantedCommitted(runCtx, s.TenantA, func(tx pgx.Tx) error {
				var current int64
				if err := tx.QueryRow(runCtx, `
SELECT refunded_amount FROM pp.payments
 WHERE payment_id = $1 AND partition_month = $2`, pay.ID, pay.PartitionMonth).Scan(&current); err != nil {
					return err
				}
				_, err := tx.Exec(runCtx, `
UPDATE pp.payments SET refunded_amount = $3, version = version + 1, updated_at = now()
 WHERE payment_id = $1 AND partition_month = $2`, pay.ID, pay.PartitionMonth, current+each)
				return err
			})

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case testenv.PgErrCode(err) == testenv.SQLStateCheckViolation:
				refused++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("%d callers failed for an unexpected reason; the first was: %v", len(other), other[0])
	}

	// The tally is not the assertion — the persisted total is. However the four transactions
	// interleaved, what must be true afterwards is that the refunded total never exceeded what was
	// captured.
	var refunded int64
	if err := s.Tenanted(c, s.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(c, `
SELECT refunded_amount FROM pp.payments
 WHERE payment_id = $1 AND partition_month = $2`, pay.ID, pay.PartitionMonth).Scan(&refunded)
	}); err != nil {
		t.Fatalf("read back the refunded total: %v", err)
	}
	if refunded > captured {
		t.Fatalf("I1 violated: refunded %d of a captured %d. %d writes were accepted and %d "+
			"refused by the CHECK.", refunded, captured, accepted, refused)
	}
	if refunded%each != 0 {
		t.Fatalf("refunded total %d is not a whole number of %d-unit refunds; a partial write got "+
			"through", refunded, each)
	}
}
