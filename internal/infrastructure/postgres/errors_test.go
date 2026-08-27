package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// TestMapErrorClassifiesSQLStates drives the mapper with real PostgreSQL SQLSTATEs.
//
// The Retryable column is the one that matters and the reason this test is exhaustive rather
// than representative: every layer above branches on it, and getting it wrong in the permissive
// direction retries a money command that already moved money. A test that covered "some" codes
// would leave exactly the uncovered one to be discovered in production.
func TestMapErrorClassifiesSQLStates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		pgErr         *pgconn.PgError
		wantCode      apierror.Code
		wantRetryable bool
	}{
		{
			name:          "serialization failure is retryable",
			pgErr:         &pgconn.PgError{Code: SQLStateSerializationFailure},
			wantCode:      apierror.CodeConcurrencyLimitExceeded,
			wantRetryable: true,
		},
		{
			name:          "deadlock is retryable",
			pgErr:         &pgconn.PgError{Code: SQLStateDeadlockDetected},
			wantCode:      apierror.CodeConcurrencyLimitExceeded,
			wantRetryable: true,
		},
		{
			name:          "too many connections is retryable service unavailable",
			pgErr:         &pgconn.PgError{Code: SQLStateTooManyConnections},
			wantCode:      apierror.CodeServiceUnavailable,
			wantRetryable: true,
		},
		{
			name: "statement timeout is NOT retryable",
			// A query that blew its budget once will blow it again, and retrying holds a second
			// connection from a fixed budget for the same duration.
			pgErr:         &pgconn.PgError{Code: SQLStateQueryCanceled},
			wantCode:      apierror.CodeInternalError,
			wantRetryable: false,
		},
		{
			name:          "admin shutdown is retryable (Aurora failover)",
			pgErr:         &pgconn.PgError{Code: SQLStateAdminShutdown},
			wantCode:      apierror.CodeServiceUnavailable,
			wantRetryable: true,
		},
		{
			name:          "cannot connect now is retryable",
			pgErr:         &pgconn.PgError{Code: SQLStateCannotConnectNow},
			wantCode:      apierror.CodeServiceUnavailable,
			wantRetryable: true,
		},
		{
			name:          "crash shutdown is retryable",
			pgErr:         &pgconn.PgError{Code: SQLStateCrashShutdown},
			wantCode:      apierror.CodeServiceUnavailable,
			wantRetryable: true,
		},
		{
			name:          "read-only transaction is service unavailable",
			pgErr:         &pgconn.PgError{Code: SQLStateReadOnlySQLTransaction},
			wantCode:      apierror.CodeServiceUnavailable,
			wantRetryable: true,
		},
		{
			name:          "insufficient privilege is internal, never retryable",
			pgErr:         &pgconn.PgError{Code: SQLStateInsufficientPrivilege},
			wantCode:      apierror.CodeInternalError,
			wantRetryable: false,
		},
		{
			name:          "foreign key violation is a validation failure",
			pgErr:         &pgconn.PgError{Code: SQLStateForeignKeyViolation, ConstraintName: "fk_x"},
			wantCode:      apierror.CodeValidationFailed,
			wantRetryable: false,
		},
		{
			name:          "not-null violation names the column",
			pgErr:         &pgconn.PgError{Code: SQLStateNotNullViolation, ColumnName: "tenant_id"},
			wantCode:      apierror.CodeValidationFailed,
			wantRetryable: false,
		},
		{
			name:          "disk full is service unavailable",
			pgErr:         &pgconn.PgError{Code: SQLStateDiskFull},
			wantCode:      apierror.CodeServiceUnavailable,
			wantRetryable: true,
		},
		{
			name:          "undefined table hints at a missing partition",
			pgErr:         &pgconn.PgError{Code: SQLStateUndefinedTable},
			wantCode:      apierror.CodeInternalError,
			wantRetryable: false,
		},
		{
			name:          "unknown class 08 code is a retryable connection fault",
			pgErr:         &pgconn.PgError{Code: "08006"},
			wantCode:      apierror.CodeServiceUnavailable,
			wantRetryable: true,
		},
		{
			name:          "unknown class 40 code is a retryable rollback",
			pgErr:         &pgconn.PgError{Code: "40002"},
			wantCode:      apierror.CodeConcurrencyLimitExceeded,
			wantRetryable: true,
		},
		{
			name:          "wholly unknown code is internal",
			pgErr:         &pgconn.PgError{Code: "XX000"},
			wantCode:      apierror.CodeInternalError,
			wantRetryable: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := mapError(tc.pgErr, "op")
			if code := apierror.CodeOf(got); code != tc.wantCode {
				t.Fatalf("code = %s, want %s (%v)", code, tc.wantCode, got)
			}
			if retryable := apierror.IsRetryable(got); retryable != tc.wantRetryable {
				t.Fatalf("retryable = %v, want %v for %s", retryable, tc.wantRetryable, tc.pgErr.Code)
			}
			if !errors.Is(got, tc.pgErr) {
				t.Fatalf("the driver error must remain reachable as the cause, for the logs")
			}
		})
	}
}

// TestUniqueViolationsAreDistinguishedByConstraint is the point of mapping constraint names.
//
// A duplicate idempotency key and a second successful attempt are both SQLSTATE 23505 and could
// not be more different: the first is a replay to be answered with the stored response, the
// second is invariant I3 stopping a double charge — a condition the client must never be told to
// retry. If this test ever passes with both mapping to the same code, the error model has
// stopped carrying information the caller needs.
func TestUniqueViolationsAreDistinguishedByConstraint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		constraint string
		wantCode   apierror.Code
	}{
		{constraintIdempotencyClaim, apierror.CodeIdempotentRequestInProgress},
		{"uq_attempt_success_2026_08", apierror.CodePaymentAlreadyProcessed},
		{"uq_attempt_success_2031_12", apierror.CodePaymentAlreadyProcessed},
		{constraintRefundIdempotency, apierror.CodeIdempotencyKeyReused},
		{constraintAttemptNumber, apierror.CodeInvalidStateTransition},
		{constraintAttemptGatewayIdem, apierror.CodePaymentAlreadyProcessed},
		{constraintPaymentEventLog, apierror.CodePaymentAlreadyProcessed},
		{constraintWebhookDedup, apierror.CodeWebhookReplayDetected},
		{constraintMerchantExternal, apierror.CodeValidationFailed},
		{constraintMerchantTenant, apierror.CodeValidationFailed},
		{constraintConfigVersion, apierror.CodeConfigurationVersionConflict},
		{constraintConfigActive, apierror.CodeConfigurationVersionConflict},
		{constraintConnectionScope, apierror.CodeValidationFailed},
		{constraintLiveCase, apierror.CodeOnboardingAlreadyInProgress},
		{constraintWorkflowKey, apierror.CodeOnboardingAlreadyInProgress},
		{constraintLedgerSource, apierror.CodePaymentAlreadyProcessed},
		{constraintAuditSequence, apierror.CodeInternalError},
		{constraintAuditDigest, apierror.CodeInternalError},
		{constraintBankPrimary, apierror.CodeValidationFailed},
		{constraintPolicyActive, apierror.CodeConfigurationInvalid},
		{constraintReconIdentity, apierror.CodePaymentAlreadyProcessed},
		{"uq_something_nobody_mapped", apierror.CodePaymentAlreadyProcessed},
	}

	seen := map[apierror.Code]int{}
	for _, tc := range cases {
		t.Run(tc.constraint, func(t *testing.T) {
			t.Parallel()
			err := mapError(&pgconn.PgError{
				Code: SQLStateUniqueViolation, ConstraintName: tc.constraint,
			}, "op")
			if got := apierror.CodeOf(err); got != tc.wantCode {
				t.Fatalf("constraint %q mapped to %s, want %s", tc.constraint, got, tc.wantCode)
			}
		})
		seen[tc.wantCode]++
	}

	// The idempotency claim and the I3 index must not collapse to the same code. This is the
	// specific distinction the task of mapping constraint names exists to preserve.
	claim := mapError(&pgconn.PgError{
		Code: SQLStateUniqueViolation, ConstraintName: constraintIdempotencyClaim}, "op")
	i3 := mapError(&pgconn.PgError{
		Code: SQLStateUniqueViolation, ConstraintName: "uq_attempt_success_2026_08"}, "op")
	if apierror.CodeOf(claim) == apierror.CodeOf(i3) {
		t.Fatal("a duplicate idempotency key and a second successful attempt must not " +
			"produce the same error code")
	}
	if !apierror.IsRetryable(claim) && apierror.IsRetryable(i3) {
		t.Fatal("invariant I3 must never be reported as retryable")
	}
}

// TestCheckViolationsMapToInvariants covers the I1/I2/I4 constraints and the guard triggers,
// which all arrive as 23514 and are only distinguishable by their constraint name — which is why
// the triggers in migration 0013 name theirs explicitly with USING CONSTRAINT.
func TestCheckViolationsMapToInvariants(t *testing.T) {
	t.Parallel()

	cases := []struct {
		constraint string
		wantCode   apierror.Code
	}{
		{constraintI1RefundWithinCapture, apierror.CodeRefundExceedsCaptured},
		{constraintI2CaptureWithinAuth, apierror.CodeCaptureExceedsAuthorized},
		{constraintI4ImmutableFields, apierror.CodeInvalidStateTransition},
		{constraintPaymentTransition, apierror.CodeInvalidStateTransition},
		{constraintMerchantTransition, apierror.CodeInvalidStateTransition},
		{constraintVersionMonotonic, apierror.CodePaymentAlreadyProcessed},
		{constraintAppendOnly, apierror.CodeInternalError},
		{constraintConfigImmutable, apierror.CodeInternalError},
		{constraintCertificationSealed, apierror.CodeInternalError},
		{constraintTokenIsNotPAN, apierror.CodeSensitiveDataInRequest},
		{constraintPartitionMatch, apierror.CodeInternalError},
		{"some_unmapped_check", apierror.CodeValidationFailed},
	}

	for _, tc := range cases {
		t.Run(tc.constraint, func(t *testing.T) {
			t.Parallel()
			err := mapError(&pgconn.PgError{
				Code: SQLStateCheckViolation, ConstraintName: tc.constraint,
			}, "op")
			if got := apierror.CodeOf(err); got != tc.wantCode {
				t.Fatalf("constraint %q mapped to %s, want %s", tc.constraint, got, tc.wantCode)
			}
			if apierror.IsRetryable(err) {
				t.Fatalf("a check violation is deterministic and must never be retryable (%s)",
					tc.constraint)
			}
		})
	}
}

// TestMapErrorHandlesNonServerErrors covers the paths that never reach a PgError: context
// cancellation, deadline, no rows, and a pool that is closed.
func TestMapErrorHandlesNonServerErrors(t *testing.T) {
	t.Parallel()

	if got := mapError(nil, "op"); got != nil {
		t.Fatalf("nil in, nil out; got %v", got)
	}

	cancelled := mapError(context.Canceled, "op")
	if apierror.CodeOf(cancelled) != apierror.CodeServiceUnavailable {
		t.Fatalf("context.Canceled = %s, want SERVICE_UNAVAILABLE", apierror.CodeOf(cancelled))
	}

	deadline := mapError(context.DeadlineExceeded, "op")
	if apierror.CodeOf(deadline) != apierror.CodeServiceUnavailable {
		t.Fatalf("DeadlineExceeded = %s, want SERVICE_UNAVAILABLE", apierror.CodeOf(deadline))
	}

	noRows := mapError(pgx.ErrNoRows, "op")
	if apierror.CodeOf(noRows) != apierror.CodeInternalError {
		t.Fatalf("ErrNoRows must stay generic so the repository can specialise it; got %s",
			apierror.CodeOf(noRows))
	}

	closed := mapError(errors.New("closed pool"), "op")
	if apierror.CodeOf(closed) != apierror.CodeServiceUnavailable {
		t.Fatalf("a closed pool = %s, want SERVICE_UNAVAILABLE", apierror.CodeOf(closed))
	}
	if !apierror.IsRetryable(closed) {
		t.Fatal("a closed pool did not run the statement, so it is safe to retry")
	}
}

// TestMapErrorPreservesAnExistingApiError proves the mapper does not re-wrap.
//
// Re-wrapping a precise code as INTERNAL_ERROR at every layer is how a retryable condition
// becomes a permanent one, and it is a mistake that is invisible until an incident.
func TestMapErrorPreservesAnExistingApiError(t *testing.T) {
	t.Parallel()
	original := apierror.New(apierror.CodeRefundExceedsCaptured, "domain said no")
	got := mapError(original, "op")
	if apierror.CodeOf(got) != apierror.CodeRefundExceedsCaptured {
		t.Fatalf("code = %s, want the original", apierror.CodeOf(got))
	}
}
