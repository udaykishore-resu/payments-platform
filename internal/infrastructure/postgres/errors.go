package postgres

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// PostgreSQL SQLSTATE codes this package classifies.
//
// They are named constants rather than string literals at the call site because the difference
// between 40001 and 40P01 is the difference between "retry this and it will probably work" and
// "retry this and it will probably work", while the difference between either of those and 23505
// is the difference between a retry and a double charge. A mistyped literal in a switch compiles
// and produces the wrong answer for exactly one code path; a named constant does not.
const (
	// SQLStateUniqueViolation — a unique index rejected the row. In this schema that is almost
	// never an accident: each unique index encodes an invariant, and which index fired tells you
	// which one.
	SQLStateUniqueViolation = "23505"
	// SQLStateForeignKeyViolation — the referenced row does not exist. On the money path this is
	// most often an attempt written with a partition month that does not match its payment.
	SQLStateForeignKeyViolation = "23503"
	// SQLStateNotNullViolation — a required column was omitted.
	SQLStateNotNullViolation = "23502"
	// SQLStateCheckViolation — a CHECK constraint or one of the guard triggers in migration 0013
	// refused the write. I1, I2, I4 and the state-transition tables all surface here.
	SQLStateCheckViolation = "23514"
	// SQLStateExclusionViolation — an exclusion constraint rejected the row.
	SQLStateExclusionViolation = "23P01"
	// SQLStateRestrictViolation — an ON DELETE RESTRICT foreign key refused a delete.
	SQLStateRestrictViolation = "23001"

	// SQLStateSerializationFailure — SERIALIZABLE could not order this transaction against a
	// concurrent one. Retryable, and the caller is *expected* to retry: at SERIALIZABLE this is
	// a normal outcome under contention, not an error condition.
	SQLStateSerializationFailure = "40001"
	// SQLStateDeadlockDetected — two transactions waited on each other and PostgreSQL broke the
	// cycle by aborting this one. Retryable for the same reason.
	SQLStateDeadlockDetected = "40P01"

	// SQLStateQueryCanceled — statement_timeout fired, or a client cancellation propagated.
	SQLStateQueryCanceled = "57014"
	// SQLStateAdminShutdown — the backend was terminated by an administrator or a failover.
	SQLStateAdminShutdown = "57P01"
	// SQLStateCrashShutdown — the server is recovering from a crash.
	SQLStateCrashShutdown = "57P02"
	// SQLStateCannotConnectNow — the server is starting up and not yet accepting connections,
	// which is precisely what a pod sees during an Aurora failover.
	SQLStateCannotConnectNow = "57P03"
	// SQLStateTooManyConnections — the connection budget is exhausted. Retryable, but the right
	// response upstream is to shed load, not to add more concurrency.
	SQLStateTooManyConnections = "53300"
	// SQLStateOutOfMemory — the server could not allocate. Retryable in the sense that a smaller
	// query later may succeed.
	SQLStateOutOfMemory = "53200"
	// SQLStateDiskFull — no space. Retryable only in the sense that an operator can fix it.
	SQLStateDiskFull = "53100"
	// SQLStateInsufficientPrivilege — the role lacks a grant. Never retryable: this is the
	// signature of an append-only REVOKE doing its job, or of a misconfigured role.
	SQLStateInsufficientPrivilege = "42501"
	// SQLStateReadOnlySQLTransaction — a write reached a read replica. Not retryable against the
	// same connection; the caller must be routed to the writer.
	SQLStateReadOnlySQLTransaction = "25006"
	// SQLStateUndefinedTable — the relation does not exist. On this schema the realistic cause is
	// an insert into a month with no partition, which is a hard failure on the payment path and
	// the reason the provisioning job carries thirteen months of lead time. Note that "no
	// partition of relation found for row" is reported as a check violation (23514), not as this
	// code, which is why mapCheckViolation carries the partition case too.
	SQLStateUndefinedTable = "42P01"
)

// Constraint names this package maps to specific error codes.
//
// Mapping the *name* rather than just the SQLSTATE is what makes the difference between a useful
// error and a shrug. Every unique violation in this schema is a 23505; without the name, a
// duplicate idempotency key, a second successful attempt on one payment and a re-used refund key
// would all surface as the same opaque CONFLICT, and the client would be told to retry an
// operation that must never be retried.
const (
	constraintIdempotencyClaim   = "uq_idem_claim"
	constraintAttemptSuccessPfx  = "uq_attempt_success" // per-partition: uq_attempt_success_2026_08
	constraintAttemptNumber      = "uq_attempt_number"
	constraintAttemptGatewayIdem = "uq_attempt_gw_idem"
	constraintRefundIdempotency  = "uq_refund_idem"
	constraintPaymentEventLog    = "uq_payment_event_version"
	constraintWebhookDedup       = "uq_webhook_gateway_event"
	constraintMerchantExternal   = "uq_merchant_external_ref"
	constraintMerchantTenant     = "uq_merchant_tenant"
	constraintConfigVersion      = "uq_config_version"
	constraintConfigActive       = "uq_config_active_version"
	constraintConnectionScope    = "uq_gw_connection"
	constraintLiveCase           = "uq_case_live"
	constraintWorkflowKey        = "uq_wf_business_key"
	constraintLedgerSource       = "uq_ledger_source"
	constraintAuditSequence      = "uq_audit_sequence"
	constraintAuditDigest        = "uq_audit_digest"
	constraintBankPrimary        = "uq_bank_primary"
	constraintPolicyActive       = "uq_policy_active"
	constraintReconIdentity      = "uq_recon_exception_identity"

	constraintI1RefundWithinCapture = "payments_i1_refund_within_capture"
	constraintI2CaptureWithinAuth   = "payments_i2_capture_within_auth"
	constraintI4ImmutableFields     = "payments_i4_immutable_fields"
	constraintPaymentTransition     = "payments_illegal_state_transition"
	constraintMerchantTransition    = "merchants_illegal_state_transition"
	constraintVersionMonotonic      = "payments_version_monotonic"
	constraintAppendOnly            = "append_only"
	constraintConfigImmutable       = "configuration_version_immutable"
	constraintCertificationSealed   = "certification_report_sealed"
	constraintTokenIsNotPAN         = "payments_token_is_not_a_pan"
	constraintPartitionMatch        = "payments_partition_matches_created_at"
)

// ErrNoRows is returned by the low-level helpers when a query that expected exactly one row
// found none. Repositories translate it into the aggregate-specific not-found code; it is
// exported so a caller composing several repository calls can distinguish "nothing there" from
// "the database is unhappy" without string matching.
var ErrNoRows = pgx.ErrNoRows

// mapError converts a driver or server error into a *apierror.Error with an accurate code,
// category and Retryable bit.
//
// The Retryable bit is the reason this function is worth its length. Every layer above — the
// generic retry wrapper, the event consumer deciding between the retry tier and the dead-letter
// topic, the HTTP handler deciding whether to send Retry-After — branches on it. Getting it
// wrong in the permissive direction retries a money command that already moved money; getting it
// wrong in the restrictive direction dead-letters a payment that would have succeeded on the
// next attempt. Neither is recoverable by a later fix.
//
// The original error is always attached as the cause, so it reaches the logs; apierror never
// serializes a cause into a response, which is what keeps a table name or a constraint name out
// of a caller's error body.
func mapError(err error, operation string) error {
	if err == nil {
		return nil
	}
	// Already ours — a repository that classified something more precisely than this function
	// could. Preserve it; re-wrapping a precise code as INTERNAL_ERROR at every layer is how a
	// retryable condition becomes a permanent one.
	var already *apierror.Error
	if errors.As(err, &already) {
		return already
	}

	if errors.Is(err, pgx.ErrNoRows) {
		// Deliberately generic. The repository knows which aggregate was missing and replaces
		// this with the specific code; a bare helper cannot, and guessing would produce
		// PAYMENT_NOT_FOUND for a missing merchant.
		return apierror.Wrapf(err, apierror.CodeInternalError, "%s: no rows", operation)
	}

	// Context cancellation and deadline are the caller's decision, not a database fault. They
	// are classified before the PgError branch because a cancelled query often *also* surfaces
	// as 57014, and reporting the caller's own timeout as a server-side statement timeout sends
	// the investigation to the wrong place.
	if errors.Is(err, context.Canceled) {
		return apierror.Wrapf(err, apierror.CodeServiceUnavailable,
			"%s: canceled by the caller", operation)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return apierror.Wrapf(err, apierror.CodeServiceUnavailable,
			"%s: deadline exceeded before the database answered", operation)
	}

	var pge *pgconn.PgError
	if errors.As(err, &pge) {
		return mapPgError(pge, err, operation)
	}

	// A connect failure, a broken pipe, a DNS failure, a pool that timed out handing out a
	// connection. All of these mean "the database was unreachable", none of them mean the
	// statement ran, and all of them are worth retrying against a possibly-different endpoint.
	if isConnectionError(err) {
		return apierror.Wrapf(err, apierror.CodeServiceUnavailable,
			"%s: database unreachable", operation)
	}

	return apierror.Wrapf(err, apierror.CodeInternalError, "%s failed", operation)
}

// mapPgError classifies a server-side error. It is separate from mapError so the table-driven
// test can drive it with synthetic *pgconn.PgError values and cover every SQLSTATE without a
// database.
func mapPgError(pge *pgconn.PgError, cause error, operation string) *apierror.Error {
	switch pge.Code {
	case SQLStateUniqueViolation:
		return mapUniqueViolation(pge, cause, operation)

	case SQLStateCheckViolation:
		return mapCheckViolation(pge, cause, operation)

	case SQLStateForeignKeyViolation:
		// On the money path the realistic cause is an attempt or refund whose partition_month
		// does not match its payment's — which is exactly the guard that keeps invariant I3
		// enforceable, so it deserves to be said out loud rather than reported as a generic
		// validation failure.
		return apierror.Wrapf(cause, apierror.CodeValidationFailed,
			"%s: referenced row does not exist (%s)", operation, pge.ConstraintName).
			WithDetail(apierror.Detail{
				Field:   pge.ColumnName,
				Code:    "FOREIGN_KEY_VIOLATION",
				Message: "the referenced row does not exist, or its partition key disagrees with its parent's",
				RuleID:  "L7.REFERENTIAL_INTEGRITY",
			})

	case SQLStateNotNullViolation:
		return apierror.Wrapf(cause, apierror.CodeValidationFailed,
			"%s: %s is required", operation, pge.ColumnName).
			WithDetail(apierror.Detail{
				Field:   pge.ColumnName,
				Code:    "MISSING_REQUIRED_FIELD",
				Message: "this column may not be null",
				RuleID:  "L1.REQUIRED_FIELDS_PRESENT",
			})

	case SQLStateExclusionViolation, SQLStateRestrictViolation:
		return apierror.Wrapf(cause, apierror.CodeValidationFailed,
			"%s: constraint %s refused the write", operation, pge.ConstraintName)

	case SQLStateSerializationFailure:
		// Retryable, and *expected* under contention at SERIALIZABLE. Concurrent partial refunds
		// against one payment are the case this exists for: the serialization failure is how
		// invariant I1 is enforced without a lock, and the correct response is to retry, not to
		// tell the merchant their refund failed.
		return apierror.Wrapf(cause, apierror.CodeConcurrencyLimitExceeded,
			"%s: serialization failure; retry the transaction", operation).
			WithRetryAfter(0)

	case SQLStateDeadlockDetected:
		return apierror.Wrapf(cause, apierror.CodeConcurrencyLimitExceeded,
			"%s: deadlock detected; retry the transaction", operation).
			WithRetryAfter(0)

	case SQLStateQueryCanceled:
		// statement_timeout. Deliberately *not* retryable: a query that exceeded its budget once
		// will exceed it again, and retrying it holds a second connection from the 400-connection
		// budget for the same duration. The fix is a better plan or a smaller request.
		return apierror.Wrapf(cause, apierror.CodeInternalError,
			"%s: statement timeout", operation).
			WithMessage("the database did not answer within its statement budget")

	case SQLStateTooManyConnections:
		return apierror.Wrapf(cause, apierror.CodeServiceUnavailable,
			"%s: connection limit reached", operation).
			WithRetryAfter(1)

	case SQLStateOutOfMemory, SQLStateDiskFull:
		return apierror.Wrapf(cause, apierror.CodeServiceUnavailable,
			"%s: database resource exhausted (%s)", operation, pge.Code)

	case SQLStateAdminShutdown, SQLStateCrashShutdown, SQLStateCannotConnectNow:
		// An Aurora failover looks exactly like this. Retryable, and by the time the retry runs
		// the pool will have reconnected to the promoted writer.
		return apierror.Wrapf(cause, apierror.CodeServiceUnavailable,
			"%s: database unavailable (%s)", operation, pge.Code).
			WithRetryAfter(1)

	case SQLStateReadOnlySQLTransaction:
		// A write reached a reader endpoint. Not the caller's fault and not fixable by retrying
		// on the same connection, but the pool will hand out a different one.
		return apierror.Wrapf(cause, apierror.CodeServiceUnavailable,
			"%s: write attempted against a read-only endpoint", operation)

	case SQLStateInsufficientPrivilege:
		// This is what an append-only REVOKE looks like from the application's side. It is never
		// retryable and it is never the caller's fault: report it as an internal error and let
		// the log line carry the constraint.
		return apierror.Wrapf(cause, apierror.CodeInternalError,
			"%s: the application role lacks the privilege for this write", operation)

	case SQLStateUndefinedTable:
		return apierror.Wrapf(cause, apierror.CodeInternalError,
			"%s: relation missing — a partition for this month may not have been provisioned",
			operation)
	}

	// Class 08 is "connection exception" and class 57 is "operator intervention"; both mean the
	// statement did not complete for reasons outside the query, and both are worth retrying.
	switch {
	case strings.HasPrefix(pge.Code, "08"), strings.HasPrefix(pge.Code, "57"):
		return apierror.Wrapf(cause, apierror.CodeServiceUnavailable,
			"%s: database connection fault (%s)", operation, pge.Code)
	case strings.HasPrefix(pge.Code, "40"):
		return apierror.Wrapf(cause, apierror.CodeConcurrencyLimitExceeded,
			"%s: transaction rolled back (%s); retry", operation, pge.Code)
	}

	return apierror.Wrapf(cause, apierror.CodeInternalError,
		"%s: unclassified database error (%s)", operation, pge.Code)
}

// mapUniqueViolation turns a 23505 into the error that describes what actually happened.
//
// This is the function that makes "a duplicate idempotency key" and "a second successful attempt
// on one payment" produce different, accurate errors. They are both unique violations, and they
// could not be more different: the first is a client replay to be answered with the stored
// response, and the second is invariant I3 preventing a double charge — a condition the client
// must never be told to retry.
func mapUniqueViolation(pge *pgconn.PgError, cause error, operation string) *apierror.Error {
	name := pge.ConstraintName

	// The I3 index is created per partition and therefore carries a month suffix
	// (uq_attempt_success_2026_08). Matching on the prefix rather than the exact name is not
	// laziness: hard-coding the suffixes would mean the classification silently degrades to a
	// generic conflict the first month nobody remembered to add.
	if strings.HasPrefix(name, constraintAttemptSuccessPfx) {
		return apierror.Wrapf(cause, apierror.CodePaymentAlreadyProcessed,
			"%s: this payment already has a successful attempt (invariant I3)", operation).
			WithDetail(apierror.Detail{
				Code: "DUPLICATE_SUCCESSFUL_ATTEMPT",
				Message: "a payment may have at most one attempt with outcome SUCCESS; " +
					"this is the constraint that makes double-charging structurally impossible",
				RuleID: "I3.ONE_SUCCESSFUL_ATTEMPT",
			})
	}

	switch name {
	case constraintIdempotencyClaim:
		// Not an error to the caller in the usual sense: it is the claim losing a race, and the
		// store's Claim path handles it by reading the winner's record. It is classified as
		// IDEMPOTENT_REQUEST_IN_PROGRESS so that a caller that *does* surface it says the right
		// thing rather than "duplicate key value violates unique constraint".
		return apierror.Wrapf(cause, apierror.CodeIdempotentRequestInProgress,
			"%s: this idempotency key is already claimed", operation).
			WithRetryAfter(1)

	case constraintRefundIdempotency:
		return apierror.Wrapf(cause, apierror.CodeIdempotencyKeyReused,
			"%s: a refund with this idempotency key already exists", operation)

	case constraintAttemptNumber:
		// Two writers computed the same next attempt number. The loser must reload and recompute;
		// retrying with the same number would collide again forever.
		return apierror.Wrapf(cause, apierror.CodeInvalidStateTransition,
			"%s: attempt number already used for this payment", operation).
			WithDetail(apierror.Detail{
				Code:    "DUPLICATE_ATTEMPT_NUMBER",
				Message: "reload the payment and recompute the next attempt number",
				RuleID:  "I3.DENSE_ATTEMPT_NUMBERING",
			})

	case constraintAttemptGatewayIdem:
		return apierror.Wrapf(cause, apierror.CodePaymentAlreadyProcessed,
			"%s: this gateway idempotency key has already been used", operation)

	case constraintPaymentEventLog:
		// Invariant I5. Two writers both believed they were producing the same version; the
		// optimistic UPDATE should have caught it first, so reaching here means the version check
		// was skipped somewhere. Report it as a conflict — it is one — and let the log carry the
		// constraint so the missing WHERE clause can be found.
		return apierror.Wrapf(cause, apierror.CodePaymentAlreadyProcessed,
			"%s: a state change for this aggregate version is already recorded (invariant I5)",
			operation)

	case constraintWebhookDedup:
		return apierror.Wrapf(cause, apierror.CodeWebhookReplayDetected,
			"%s: this gateway event has already been received", operation)

	case constraintMerchantExternal:
		return apierror.Wrapf(cause, apierror.CodeValidationFailed,
			"%s: another merchant in this tenant already uses that external reference", operation).
			WithDetail(apierror.Detail{
				Field: "externalReference", Code: "DUPLICATE_EXTERNAL_REFERENCE",
				Message: "external references are unique within a tenant",
				RuleID:  "L2.MERCHANT_EXTERNAL_REF_UNIQUE",
			})

	case constraintMerchantTenant:
		return apierror.Wrapf(cause, apierror.CodeValidationFailed,
			"%s: a merchant with this identifier already exists in this tenant", operation)

	case constraintConfigVersion, constraintConfigActive:
		return apierror.Wrapf(cause, apierror.CodeConfigurationVersionConflict,
			"%s: another publish won this version number", operation)

	case constraintConnectionScope:
		return apierror.Wrapf(cause, apierror.CodeValidationFailed,
			"%s: this merchant already has a connection to that gateway in that environment",
			operation)

	case constraintLiveCase:
		return apierror.Wrapf(cause, apierror.CodeOnboardingAlreadyInProgress,
			"%s: this merchant already has a live onboarding case", operation)

	case constraintWorkflowKey:
		// Starting a workflow twice is defined as a no-op returning the existing instance, so
		// the caller is expected to catch this and read the incumbent.
		return apierror.Wrapf(cause, apierror.CodeOnboardingAlreadyInProgress,
			"%s: a live workflow instance already exists for this business key", operation)

	case constraintLedgerSource:
		// Idempotent posting doing its job: a redelivered event tried to post the same entry
		// twice. The consumer treats this as success, which is why it must be distinguishable.
		return apierror.Wrapf(cause, apierror.CodePaymentAlreadyProcessed,
			"%s: this source event has already been posted to that account", operation)

	case constraintAuditSequence, constraintAuditDigest:
		return apierror.Wrapf(cause, apierror.CodeInternalError,
			"%s: audit chain position already occupied; the per-tenant advisory lock was not held",
			operation)

	case constraintBankPrimary:
		return apierror.Wrapf(cause, apierror.CodeValidationFailed,
			"%s: this merchant already has a primary account for that currency", operation).
			WithDetail(apierror.Detail{
				Field: "isDefault", Code: "DUPLICATE_PRIMARY_ACCOUNT",
				Message: "exactly one primary bank account per settlement currency",
				RuleID:  "L2.ONE_PRIMARY_PER_CURRENCY",
			})

	case constraintPolicyActive:
		return apierror.Wrapf(cause, apierror.CodeConfigurationInvalid,
			"%s: an active policy of that type already exists for this scope", operation)

	case constraintReconIdentity:
		return apierror.Wrapf(cause, apierror.CodePaymentAlreadyProcessed,
			"%s: this discrepancy is already recorded as an open exception", operation)
	}

	return apierror.Wrapf(cause, apierror.CodePaymentAlreadyProcessed,
		"%s: unique constraint %s violated", operation, name)
}

// mapCheckViolation turns a 23514 into the invariant that fired.
//
// Both declarative CHECKs and the guard triggers in migration 0013 raise 23514; the triggers
// name their constraint explicitly with `USING CONSTRAINT` for exactly this reason, so that the
// mapping does not have to parse an error message.
func mapCheckViolation(pge *pgconn.PgError, cause error, operation string) *apierror.Error {
	switch pge.ConstraintName {
	case constraintI1RefundWithinCapture:
		return apierror.Wrapf(cause, apierror.CodeRefundExceedsCaptured,
			"%s: refunds would exceed the captured amount (invariant I1)", operation).
			WithDetail(apierror.Detail{
				Field: "amount", Code: "REFUND_EXCEEDS_CAPTURED",
				Message: "the total refunded may never exceed the total captured",
				RuleID:  "I1.REFUND_WITHIN_CAPTURE",
			})

	case constraintI2CaptureWithinAuth:
		return apierror.Wrapf(cause, apierror.CodeCaptureExceedsAuthorized,
			"%s: capture would exceed the authorized amount (invariant I2)", operation).
			WithDetail(apierror.Detail{
				Field: "amount", Code: "CAPTURE_EXCEEDS_AUTHORIZED",
				Message: "the total captured may never exceed the authorization it draws on",
				RuleID:  "I2.CAPTURE_WITHIN_AUTH",
			})

	case constraintI4ImmutableFields:
		return apierror.Wrapf(cause, apierror.CodeInvalidStateTransition,
			"%s: amount, currency, merchant and tenant are immutable after creation (invariant I4)",
			operation).
			WithDetail(apierror.Detail{
				Code:    "IMMUTABLE_FIELD",
				Message: "create a new payment rather than amending this one",
				RuleID:  "I4.PAYMENT_CORE_IMMUTABLE",
			})

	case constraintPaymentTransition, constraintMerchantTransition:
		return apierror.Wrapf(cause, apierror.CodeInvalidStateTransition,
			"%s: the database refused this state transition", operation).
			WithDetail(apierror.Detail{
				Code: "ILLEGAL_TRANSITION",
				Message: "this transition is not in the state machine; the domain should have " +
					"refused it first, so reaching the database check is itself a defect",
				RuleID: "L7.STATE_TRANSITION_LEGAL",
			})

	case constraintVersionMonotonic:
		return apierror.Wrapf(cause, apierror.CodePaymentAlreadyProcessed,
			"%s: aggregate version may not move backwards", operation)

	case constraintAppendOnly, constraintConfigImmutable, constraintCertificationSealed:
		return apierror.Wrapf(cause, apierror.CodeInternalError,
			"%s: this table is append-only; a correction is a new row, never an edit", operation)

	case constraintTokenIsNotPAN:
		// The schema-level PAN tripwire fired. This is a security event, not a validation
		// nicety: something upstream tried to write a bare card number into a token column.
		return apierror.Wrapf(cause, apierror.CodeSensitiveDataInRequest,
			"%s: the payment method token looks like a primary account number", operation).
			WithDetail(apierror.Detail{
				Field: "paymentMethod.token", Code: "PAN_DETECTED",
				Message: "card data must be tokenized at the gateway edge and never reaches this API",
				RuleID:  "L1.NO_PAN_IN_REQUEST",
			})

	case constraintPartitionMatch:
		return apierror.Wrapf(cause, apierror.CodeInternalError,
			"%s: partition key disagrees with the row's creation time; "+
				"created_at must be derived from the ULID, never from now()", operation)
	}

	return apierror.Wrapf(cause, apierror.CodeValidationFailed,
		"%s: check constraint %s violated", operation, pge.ConstraintName).
		WithDetail(apierror.Detail{
			Code:    "CHECK_VIOLATION",
			Message: "the database refused this value",
			RuleID:  "L7.DATABASE_INVARIANT",
		})
}

// isConnectionError reports whether err means the database was unreachable rather than unhappy.
//
// It checks pgconn's own connect error type first and falls back to net.Error, because a pool
// that fails to dial produces the former while a connection that dies mid-query produces the
// latter, and both must classify the same way: the statement did not run, so retrying it cannot
// double-apply anything.
func isConnectionError(err error) bool {
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	// puddle (the pool underneath pgxpool) reports exhaustion and closure as plain errors.
	msg := err.Error()
	return strings.Contains(msg, "closed pool") ||
		strings.Contains(msg, "conn closed") ||
		strings.Contains(msg, "unexpected EOF") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset by peer")
}

// notFound builds the aggregate-specific not-found error a repository returns for a missing row.
//
// It exists so that "not yours" and "does not exist" produce the *same* answer. Distinguishing
// them would leak the existence of another tenant's identifiers to anyone who can guess one, and
// the guess is cheap: identifiers are ordered by creation time. RLS already makes another
// tenant's row invisible, so the repository sees pgx.ErrNoRows for both cases and could not
// distinguish them even if it wanted to — which is the design working as intended.
func notFound(code apierror.Code, kind, id string) *apierror.Error {
	return apierror.Newf(code, "%s %s not found", kind, id)
}
