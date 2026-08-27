//go:build integration

package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/migrations"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// The integration suite runs against a real PostgreSQL 15+ and is built only under the
// `integration` tag, so `go test ./...` on a laptop with no database still passes.
//
// It connects as **pp_app**, never as the migration role and never as postgres. That is not
// incidental: half of what these tests assert — RLS filtering, the append-only revokes, the
// absence of BYPASSRLS — is invisible to a superuser, and a suite that ran as one would pass
// against a schema that protects nothing.

const (
	// tenantAlpha and tenantBravo are two tenants used by every isolation test. They are
	// constants rather than generated so that a failure message names a tenant a human can grep
	// the logs for.
	tenantAlpha = shared.TenantID("ten_01JB8Z9K2QW3E4R5T6Y7U8I9O0")
	tenantBravo = shared.TenantID("ten_01JB8Z9K2QW3E4R5T6Y7U8I9O1")
)

// testPool opens a pool as pp_app and applies the migrations if the schema is not current.
func testPool(t *testing.T) *Pool {
	t.Helper()
	dsn := os.Getenv("PP_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PP_TEST_DATABASE_URL is not set; skipping the integration suite")
	}

	cfg := DefaultPoolConfig(dsn, "pp-integration-test")
	// Migrations and the deliberately slow negative tests need more than the money path's 3 s.
	cfg.StatementTimeout = 30 * time.Second
	cfg.MaxConns = 8

	ctx := context.Background()
	pool, err := NewPool(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = pool.Close(shutdown)
	})

	if _, err := NewMigrator(pool, migrations.Files()).Up(ctx, false); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// testUnitOfWork returns a unit of work with the leaked-GUC assertion switched on.
//
// It is on for the whole suite deliberately: this is exactly the environment where a leaked
// session GUC is cheap to find and free to fix, and it is the only environment where the
// PgBouncer bug in docs/multi-tenancy.md §2.4 is reproducible at all.
func testUnitOfWork(t *testing.T, pool *Pool) *UnitOfWork {
	t.Helper()
	return NewUnitOfWork(pool, shared.SystemClock{}, true)
}

// tenantContext installs the process resolver and returns a context carrying the tenant.
//
// The resolver is process state, so tests using it must not run in parallel with each other.
// Every test in this suite is therefore sequential; the database round trips dominate the
// runtime anyway.
func tenantContext(t *testing.T, id shared.TenantID) context.Context {
	t.Helper()
	prev := tenantFrom
	t.Cleanup(func() { tenantFrom = prev })
	UseTenantResolver(func(context.Context) (string, bool) { return id.String(), true })
	return context.Background()
}

// seedTenants inserts the two fixture tenants, bypassing the repository so that a broken
// repository cannot make an isolation test pass by failing to write anything.
func seedTenants(t *testing.T, pool *Pool) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	for _, id := range []shared.TenantID{tenantAlpha, tenantBravo} {
		// The insert must run with the GUC set, because tenants is under FORCE RLS with a
		// WITH CHECK clause — writing a tenant row for a tenant the session is not scoped to is
		// refused, which is itself the behaviour under test elsewhere.
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, id.String()); err != nil {
			t.Fatalf("set tenant: %v", err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO pp.tenants (tenant_id, name, tier, status, residency_region,
                        created_at, updated_at)
VALUES ($1, $2, 'POOLED', 'ACTIVE', 'GLOBAL', now(), now())
ON CONFLICT (tenant_id) DO NOTHING`, id.String(), string(id)); err != nil {
			t.Fatalf("seed tenant %s: %v", id, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
}

// newTestPayment builds a valid payment for a tenant, with a deterministic clock so that a
// failure message carries a timestamp a human can correlate.
func newTestPayment(t *testing.T, tenant shared.TenantID, minor int64) *payment.Payment {
	t.Helper()
	p, err := payment.New(payment.NewPaymentParams{
		TenantID:      tenant,
		MerchantID:    shared.NewMerchantID(),
		Amount:        money.MustNew(minor, "USD"),
		PaymentMethod: shared.MethodCard,
		MethodRef: payment.PaymentMethodReference{
			Token: "tok_test_visa", Brand: "visa", Last4: "4242",
			ExpMonth: 12, ExpYear: 2030, Country: "US",
		},
		CaptureMethod:  payment.CaptureManual,
		IdempotencyKey: "idem-" + string(shared.NewPaymentID()),
	}, shared.SystemClock{})
	if err != nil {
		t.Fatalf("construct payment: %v", err)
	}
	return p
}

// rawConn returns a pooled connection scoped to a tenant, for the tests that must bypass the
// repositories entirely and assert at the database level with the domain out of the way.
func rawConn(t *testing.T, pool *Pool, tenant shared.TenantID) (pgx.Tx, func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		t.Fatalf("begin: %v", err)
	}
	if tenant != "" {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant.String()); err != nil {
			_ = tx.Rollback(ctx)
			conn.Release()
			t.Fatalf("set tenant: %v", err)
		}
	}
	return tx, func() {
		_ = tx.Rollback(context.Background())
		conn.Release()
	}
}
