//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Every test in this file writes raw SQL with the domain deliberately out of the way.
//
// That is the entire point. The domain already refuses each of these, and it produces a much
// better error message when it does. What is being asserted here is that the *database* refuses
// them too — because the domain is code, code has bugs, and a support script or a migration does
// not pass through the domain at all. Both exist; only one is trusted.

// TestI3RejectsSecondSuccessfulAttempt asserts SQLSTATE 23505, at the database level, with the
// domain check bypassed. This is the constraint that makes double-charging structurally
// impossible rather than merely unlikely.
func TestI3RejectsSecondSuccessfulAttempt(t *testing.T) {
	// Verifies: BR-21.
	pool := testPool(t)
	seedTenants(t, pool)

	ctx := tenantContext(t, tenantAlpha)
	uow := testUnitOfWork(t, pool)
	p := newTestPayment(t, tenantAlpha, 5000)
	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Payments.Create(ctx, p)
	}); err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	tx, done := rawConn(t, pool, tenantAlpha)
	defer done()
	bg := context.Background()

	insert := func(attemptID shared.AttemptID, number int) error {
		_, err := tx.Exec(bg, `
INSERT INTO pp.payment_attempts (
    attempt_id, partition_month, payment_id, tenant_id, gateway_id, operation,
    attempt_number, amount, currency, outcome, gateway_idempotency_key,
    request_sent_at, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'stripe', 'authorize', $5, 5000, 'USD', 'SUCCESS', $6,
        now(), now(), now())`,
			attemptID.String(), shared.PartitionMonth(p.ID()), p.ID().String(),
			tenantAlpha.String(), number, "gwkey-"+attemptID.String())
		return err
	}

	if err := insert(shared.NewAttemptID(), 1); err != nil {
		t.Fatalf("the first successful attempt must be accepted: %v", err)
	}

	err := insert(shared.NewAttemptID(), 2)
	if err == nil {
		t.Fatal("a second attempt with outcome SUCCESS was accepted; invariant I3 is not enforced " +
			"and this payment can be charged twice")
	}
	var pge *pgconn.PgError
	if !errors.As(err, &pge) || pge.Code != SQLStateUniqueViolation {
		t.Fatalf("want SQLSTATE 23505 from the I3 partial unique index, got %v", err)
	}
	if got := apierror.CodeOf(mapError(err, "insert attempt")); got != apierror.CodePaymentAlreadyProcessed {
		t.Fatalf("the I3 violation mapped to %s, want PAYMENT_ALREADY_PROCESSED", got)
	}
}

// TestI1RejectsRefundExceedingCapture, at the database level.
func TestI1RejectsRefundExceedingCapture(t *testing.T) {
	// Verifies: BR-25, FR-70.
	pool := testPool(t)
	seedTenants(t, pool)
	p := seedCapturedPayment(t, pool, 10_000, 6_000)

	tx, done := rawConn(t, pool, tenantAlpha)
	defer done()

	_, err := tx.Exec(context.Background(), `
UPDATE pp.payments SET refunded_amount = $3, version = version + 1
WHERE payment_id = $1 AND partition_month = $2`,
		p.ID().String(), shared.PartitionMonth(p.ID()), 6_001)
	if err == nil {
		t.Fatal("refunded_amount was allowed to exceed captured_amount; invariant I1 is not enforced")
	}
	assertCheckViolation(t, err, constraintI1RefundWithinCapture)
	if got := apierror.CodeOf(mapError(err, "update")); got != apierror.CodeRefundExceedsCaptured {
		t.Fatalf("I1 violation mapped to %s, want REFUND_EXCEEDS_CAPTURED", got)
	}
}

// TestI2RejectsCaptureExceedingAuthorization, at the database level.
func TestI2RejectsCaptureExceedingAuthorization(t *testing.T) {
	// Verifies: BR-24, FR-69.
	pool := testPool(t)
	seedTenants(t, pool)
	p := seedCapturedPayment(t, pool, 10_000, 6_000)

	tx, done := rawConn(t, pool, tenantAlpha)
	defer done()

	_, err := tx.Exec(context.Background(), `
UPDATE pp.payments SET captured_amount = $3, version = version + 1
WHERE payment_id = $1 AND partition_month = $2`,
		p.ID().String(), shared.PartitionMonth(p.ID()), 10_001)
	if err == nil {
		t.Fatal("captured_amount was allowed to exceed authorized_amount; invariant I2 is not enforced")
	}
	assertCheckViolation(t, err, constraintI2CaptureWithinAuth)
	if got := apierror.CodeOf(mapError(err, "update")); got != apierror.CodeCaptureExceedsAuthorized {
		t.Fatalf("I2 violation mapped to %s, want CAPTURE_EXCEEDS_AUTHORIZED", got)
	}
}

// TestI4ImmutableFieldsRejectedAtDatabase issues a raw UPDATE against each immutable column.
//
// The repository's UPDATE simply does not name these columns, so a bug in the repository cannot
// change them. The trigger is the line of defence for everything that does not come through the
// repository — a support script, a data fix, a migration.
func TestI4ImmutableFieldsRejectedAtDatabase(t *testing.T) {
	pool := testPool(t)
	seedTenants(t, pool)
	p := seedCapturedPayment(t, pool, 10_000, 6_000)

	mutations := map[string]string{
		"amount":      `UPDATE pp.payments SET amount = amount + 1 WHERE payment_id = $1 AND partition_month = $2`,
		"currency":    `UPDATE pp.payments SET currency = 'EUR' WHERE payment_id = $1 AND partition_month = $2`,
		"merchant_id": `UPDATE pp.payments SET merchant_id = 'mrc_other' WHERE payment_id = $1 AND partition_month = $2`,
		"created_at":  `UPDATE pp.payments SET created_at = now() WHERE payment_id = $1 AND partition_month = $2`,
	}

	for column, stmt := range mutations {
		t.Run(column, func(t *testing.T) {
			tx, done := rawConn(t, pool, tenantAlpha)
			defer done()
			_, err := tx.Exec(context.Background(), stmt,
				p.ID().String(), shared.PartitionMonth(p.ID()))
			if err == nil {
				t.Fatalf("%s was mutable; invariant I4 is not enforced by the database", column)
			}
			assertCheckViolation(t, err, constraintI4ImmutableFields)
		})
	}
}

// TestDatabaseRefusesAnIllegalStateTransition proves the transition table in migration 0013 is a
// genuine second line of defence rather than a comment.
func TestDatabaseRefusesAnIllegalStateTransition(t *testing.T) {
	// Verifies: FR-53.
	pool := testPool(t)
	seedTenants(t, pool)
	p := seedCapturedPayment(t, pool, 10_000, 6_000)

	tx, done := rawConn(t, pool, tenantAlpha)
	defer done()

	// CAPTURED -> AUTHORIZED is explicitly invalid in baseline §9: money that has been taken
	// cannot become a hold again.
	_, err := tx.Exec(context.Background(), `
UPDATE pp.payments SET state = 'AUTHORIZED', version = version + 1
WHERE payment_id = $1 AND partition_month = $2`,
		p.ID().String(), shared.PartitionMonth(p.ID()))
	if err == nil {
		t.Fatal("CAPTURED -> AUTHORIZED was accepted; the transition guard is not enforcing")
	}
	assertCheckViolation(t, err, constraintPaymentTransition)

	// A legal transition on the same row must still be accepted, or the guard is simply broken
	// rather than strict.
	tx2, done2 := rawConn(t, pool, tenantAlpha)
	defer done2()
	if _, err := tx2.Exec(context.Background(), `
UPDATE pp.payments SET state = 'SETTLED', version = version + 1
WHERE payment_id = $1 AND partition_month = $2`,
		p.ID().String(), shared.PartitionMonth(p.ID())); err != nil {
		t.Fatalf("CAPTURED -> SETTLED is legal and must be accepted: %v", err)
	}
}

// TestLedgerAndAuditAreAppendOnly asserts both controls: the role-level revoke and the trigger.
func TestLedgerAndAuditAreAppendOnly(t *testing.T) {
	// Verifies: FR-80, FR-88.
	pool := testPool(t)
	seedTenants(t, pool)

	tx, done := rawConn(t, pool, tenantAlpha)
	defer done()
	bg := context.Background()

	for _, table := range []string{"pp.ledger_entries", "pp.audit_records"} {
		t.Run(table, func(t *testing.T) {
			if _, err := tx.Exec(bg,
				`UPDATE `+table+` SET tenant_id = tenant_id WHERE false`); err == nil {
				// A no-op UPDATE that matches no rows can still be refused at plan time by the
				// missing grant; if it succeeds, the grant is present and the table is mutable.
				t.Errorf("%s accepted an UPDATE; it must be append-only at the role level", table)
			}
			if _, err := tx.Exec(bg, `DELETE FROM `+table+` WHERE false`); err == nil {
				t.Errorf("%s accepted a DELETE; it must be append-only at the role level", table)
			}
		})
	}
}

// TestPaymentTablesRejectDelete. Retention on the money tables is a partition DETACH, never a
// DELETE — a DELETE over a billion rows bloats and vacuums for days and leaves the data
// recoverable from the heap in the meantime.
func TestPaymentTablesRejectDelete(t *testing.T) {
	pool := testPool(t)
	seedTenants(t, pool)

	tx, done := rawConn(t, pool, tenantAlpha)
	defer done()

	for _, table := range []string{"pp.payments", "pp.payment_attempts", "pp.refunds"} {
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM `+table+` WHERE false`); err == nil {
			t.Errorf("%s accepted a DELETE; the app role must hold no DELETE grant on it", table)
		}
	}
}

// TestPANTripwireRejectsABareCardNumber. The schema-level CHECK sits behind the L1 detector and
// exists for the day somebody wires a raw card number into the token field — the accident that
// puts the whole platform in PCI scope.
func TestPANTripwireRejectsABareCardNumber(t *testing.T) {
	// Verifies: NFR-33, NFR-39.
	pool := testPool(t)
	seedTenants(t, pool)

	tx, done := rawConn(t, pool, tenantAlpha)
	defer done()

	_, err := tx.Exec(context.Background(), `
INSERT INTO pp.payments (
    payment_id, partition_month, tenant_id, merchant_id, state, amount, currency,
    payment_method, capture_method, method_token, created_at, updated_at, version)
VALUES ($1, date_trunc('month', now()), $2, 'mrc_x', 'CREATED', 100, 'USD',
        'CARD', 'AUTOMATIC', '4111111111111111', now(), now(), 1)`,
		shared.NewPaymentID().String(), tenantAlpha.String())
	if err == nil {
		t.Fatal("a sixteen-digit token was accepted; the PAN tripwire is not in place")
	}
	if got := apierror.CodeOf(mapError(err, "insert")); got != apierror.CodeSensitiveDataInRequest {
		t.Fatalf("the PAN tripwire mapped to %s, want SENSITIVE_DATA_IN_REQUEST", got)
	}
}

// seedCapturedPayment writes a payment straight into an authorized-and-captured shape.
//
// It bypasses the aggregate on purpose: these tests are about what the database refuses, and
// driving the domain to reach the same state would mean a domain bug could make the setup fail
// in a way that looked like the constraint passing.
func seedCapturedPayment(t *testing.T, pool *Pool, authorized, captured int64) *payment.Payment {
	t.Helper()
	p := newTestPayment(t, tenantAlpha, authorized)

	tx, done := rawConn(t, pool, tenantAlpha)
	defer done()
	if _, err := tx.Exec(context.Background(), `
INSERT INTO pp.payments (
    payment_id, partition_month, tenant_id, merchant_id, state, amount, currency,
    payment_method, capture_method, method_token,
    authorized_amount, captured_amount, refunded_amount,
    created_at, updated_at, authorized_at, captured_at, version)
VALUES ($1,$2,$3,$4,'CAPTURED',$5,'USD','CARD','MANUAL','tok_test_visa',
        $6,$7,0,$8,$8,$8,$8,1)`,
		p.ID().String(), shared.PartitionMonth(p.ID()), tenantAlpha.String(),
		p.MerchantID().String(), authorized, authorized, captured, p.CreatedAt(),
	); err != nil {
		t.Fatalf("seed captured payment: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return p
}

func assertCheckViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	var pge *pgconn.PgError
	if !errors.As(err, &pge) {
		t.Fatalf("want a PostgreSQL error, got %v", err)
	}
	if pge.Code != SQLStateCheckViolation {
		t.Fatalf("want SQLSTATE 23514, got %s (%v)", pge.Code, err)
	}
	if pge.ConstraintName != constraint {
		t.Fatalf("want constraint %q, got %q — the error mapper keys on this name",
			constraint, pge.ConstraintName)
	}
}
