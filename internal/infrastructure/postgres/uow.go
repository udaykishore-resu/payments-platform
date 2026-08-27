package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// querier is the subset of pgx.Tx every repository in this package uses.
//
// Repositories take this rather than *pgxpool.Pool or pgx.Tx so that they are structurally
// incapable of beginning their own transaction, committing, or reaching the pool. A repository
// that could do any of those could perform half its work inside the caller's transaction and
// half outside it, which is the single most common source of "the state committed but the event
// didn't" — the exact failure the outbox exists to prevent.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// txKey marks a context as already inside a unit of work.
type txKey struct{}

// UnitOfWork runs a function inside one PostgreSQL transaction with the tenant GUC in force.
//
// It implements ports.UnitOfWork. The callback receives a Repositories bundle already bound to
// the transaction, and there is no way to obtain the transaction handle itself, so a use case
// cannot accidentally split its work across the transaction boundary.
type UnitOfWork struct {
	pool  *Pool
	clock shared.Clock
	// assertCleanGUC turns on the non-production runtime check described in
	// docs/multi-tenancy.md §2.4: after BEGIN and before setting it, `app.tenant_id` must be
	// empty. A non-empty value means a session GUC leaked across a pooled connection, which is
	// the PgBouncer bug this whole design exists to avoid. It is off in production because it
	// costs a round trip per transaction, and on everywhere else because that is where it can
	// still be cheap to fix.
	assertCleanGUC bool
}

// NewUnitOfWork constructs the transactional entry point.
//
// The clock is a parameter rather than time.Now because several rehydration paths take one and
// because the integration tests need to be able to place a lease expiry in the past without
// sleeping.
func NewUnitOfWork(pool *Pool, clock shared.Clock, assertCleanGUC bool) *UnitOfWork {
	if clock == nil {
		clock = shared.SystemClock{}
	}
	return &UnitOfWork{pool: pool, clock: clock, assertCleanGUC: assertCleanGUC}
}

// Within runs fn inside a READ COMMITTED transaction scoped to the caller's tenant.
//
// The order of operations is not arbitrary:
//
//  1. Resolve the tenant first, and fail *before* opening a transaction if there is none. A
//     transaction opened without a tenant would run every statement against an unset GUC, which
//     the RLS policies evaluate to zero rows — correct, but indistinguishable from "no such
//     row", so the caller would report a 404 for a missing-authentication bug.
//  2. BEGIN.
//  3. `SELECT set_config('app.tenant_id', $1, true)`. The third argument is `is_local`, exactly
//     equivalent to `SET LOCAL`, so the value is reverted at COMMIT or ROLLBACK *before* the
//     connection returns to PgBouncer's pool. A plain `SET` works perfectly in every unit test,
//     in every local run against a direct connection, and in session pooling; it fails only
//     under transaction pooling, under concurrency, across tenants — that is, only in
//     production, where the next borrower of the connection inherits the previous tenant's
//     identity and reads their data.
//     It is `set_config` with a bind parameter rather than `SET`, because `SET` does not accept
//     bind parameters and interpolating a value into it is a SQL injection sink.
//  4. Run fn.
//  5. Commit, or roll back on any error or panic.
func (u *UnitOfWork) Within(ctx context.Context, fn func(ctx context.Context, r ports.Repositories) error) error {
	return u.run(ctx, pgx.TxOptions{}, fn)
}

// WithinSerializable is Within at SERIALIZABLE isolation.
//
// It exists for the operations whose correctness depends on it — concurrent partial refunds
// against one payment being the case that matters, where two transactions each reading
// refunded_amount and each writing a legal-looking increment can together exceed the captured
// amount under READ COMMITTED. At SERIALIZABLE, PostgreSQL aborts one of them with 40001.
//
// The caller must retry. That is deliberate and it is not an oversight: this method does not
// retry internally because an automatic in-transaction retry of a money command can re-apply an
// effect that already happened (R-CC-5). Retries are the caller's decision and are gated by the
// idempotency record. What this method guarantees is that 40001 and 40P01 arrive as *retryable*
// apierrors, so the generic retry wrapper recognises them without parsing anything.
func (u *UnitOfWork) WithinSerializable(ctx context.Context, fn func(ctx context.Context, r ports.Repositories) error) error {
	return u.run(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable}, fn)
}

func (u *UnitOfWork) run(
	ctx context.Context,
	opts pgx.TxOptions,
	fn func(ctx context.Context, r ports.Repositories) error,
) error {
	// Nesting is an error, never a silent join. A caller who believes they hold a savepoint and
	// does not is worse off than one who gets an error: their "rollback" would leave the outer
	// transaction's writes in place, and they would find out at reconciliation.
	if ctx.Value(txKey{}) != nil {
		return apierror.New(apierror.CodeInternalError,
			"postgres: nested unit of work; pass the existing Repositories down rather than "+
				"opening a second transaction")
	}

	tenant, err := tenantOf(ctx)
	if err != nil {
		return err
	}

	tx, err := u.pool.pool.BeginTx(ctx, opts)
	if err != nil {
		return mapError(err, "postgres: begin")
	}

	committed := false
	defer func() {
		if committed {
			return
		}
		// Rollback on a background context: if ctx is already cancelled — the common case when
		// fn failed because the client hung up — a rollback on ctx would itself fail, leaving
		// the transaction to be cleaned up by idle_in_transaction_session_timeout five seconds
		// later while holding its locks.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()

	if u.assertCleanGUC {
		var residual string
		if err := tx.QueryRow(ctx,
			`SELECT coalesce(current_setting('app.tenant_id', true), '')`).Scan(&residual); err != nil {
			return mapError(err, "postgres: read tenant GUC")
		}
		if residual != "" {
			// A leaked session GUC. In a test run this is a hard failure by design: it means a
			// `SET` reached the connection somewhere, and the symptom in production would be one
			// tenant reading another's rows.
			return apierror.Newf(apierror.CodeInternalError,
				"postgres: app.tenant_id was already set to %q at BEGIN; a session-scoped SET "+
					"has leaked across a pooled connection", residual)
		}
	}

	if _, err := tx.Exec(ctx,
		`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()); err != nil {
		return mapError(err, "postgres: set tenant scope")
	}

	inner := context.WithValue(ctx, txKey{}, struct{}{})
	if err := fn(inner, u.repositories(tx, tenant)); err != nil {
		return err
	}

	if err := tx.Commit(inner); err != nil {
		// A commit that fails at SERIALIZABLE is the deferred conflict surfacing, and it must
		// classify as retryable exactly like a mid-transaction 40001 would.
		if errors.Is(err, pgx.ErrTxClosed) {
			return apierror.Wrap(err, apierror.CodeInternalError,
				"postgres: transaction was already closed at commit")
		}
		return mapError(err, "postgres: commit")
	}
	committed = true
	return nil
}

// repositories builds the bundle bound to one transaction.
//
// Everything is constructed eagerly rather than lazily. Constructing fourteen small structs is
// cheaper than the branch that would decide whether to, and lazy construction would need either
// a mutex or a promise that the callback is single-goroutine — and a promise that a callback is
// single-goroutine is a promise somebody eventually breaks.
func (u *UnitOfWork) repositories(tx querier, tenant shared.TenantID) ports.Repositories {
	outbox := &OutboxRepository{q: tx, tenant: tenant, clock: u.clock}
	return ports.Repositories{
		Payments:       &PaymentRepository{q: tx, tenant: tenant, clock: u.clock, outbox: outbox},
		Merchants:      &MerchantRepository{q: tx, tenant: tenant, clock: u.clock},
		Tenants:        &TenantRepository{q: tx, tenant: tenant},
		Gateways:       &GatewayRepository{q: tx},
		Connections:    &ConnectionRepository{q: tx, tenant: tenant},
		Health:         &HealthRepository{q: tx, clock: u.clock},
		Configs:        &ConfigRepository{q: tx, tenant: tenant},
		Idempotency:    &IdempotencyStore{q: tx, tenant: tenant, clock: u.clock},
		Outbox:         outbox,
		Ledger:         &LedgerRepository{q: tx, tenant: tenant, clock: u.clock},
		Audit:          &AuditRepository{q: tx, tenant: tenant},
		Webhooks:       &WebhookRepository{q: tx, tenant: tenant, clock: u.clock},
		Workflows:      &WorkflowRepository{q: tx, tenant: tenant, clock: u.clock},
		Reconciliation: &ReconciliationRepository{q: tx, tenant: tenant, clock: u.clock},
	}
}

// Compile-time proof that the concrete type satisfies the port. Placed here rather than in a
// test so that a signature drift in ports.UnitOfWork breaks the build immediately, at the file
// that has to change, instead of in a test run somebody might skip.
var _ ports.UnitOfWork = (*UnitOfWork)(nil)
