//go:build integration

package postgres

import (
	"context"
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// TestCrossTenantAccessIsImpossible is the isolation test the whole pooled-tenancy design rests
// on, and it is written to prove the *database* is doing the filtering rather than the
// repository.
//
// That is why the SELECT below has no WHERE clause at all. A test that queried through the
// repository would pass just as happily against a schema with no policies, because the
// repository adds its own tenant predicate — and it is exactly the day someone forgets that
// predicate in one method out of forty that this test needs to fail.
func TestCrossTenantAccessIsImpossible(t *testing.T) {
	// Verifies: FR-06, NFR-29.
	pool := testPool(t)
	seedTenants(t, pool)

	// Seed one payment under each tenant, through the repositories.
	alphaCtx := tenantContext(t, tenantAlpha)
	uow := testUnitOfWork(t, pool)

	payA := newTestPayment(t, tenantAlpha, 1050)
	if err := uow.Within(alphaCtx, func(ctx context.Context, r ports.Repositories) error {
		return r.Payments.Create(ctx, payA)
	}); err != nil {
		t.Fatalf("seed tenant A payment: %v", err)
	}

	bravoCtx := tenantContext(t, tenantBravo)
	payB := newTestPayment(t, tenantBravo, 2000)
	if err := uow.Within(bravoCtx, func(ctx context.Context, r ports.Repositories) error {
		return r.Payments.Create(ctx, payB)
	}); err != nil {
		t.Fatalf("seed tenant B payment: %v", err)
	}

	tx, done := rawConn(t, pool, tenantBravo)
	defer done()
	ctx := context.Background()

	// Deliberately unqualified. If RLS is doing its job the database filters; if this test
	// passed only because a repository added a predicate, it would prove nothing.
	rows, err := tx.Query(ctx, `SELECT payment_id FROM pp.payments`)
	if err != nil {
		t.Fatalf("unqualified select: %v", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()

	for _, id := range ids {
		if id == payA.ID().String() {
			t.Fatal("tenant A's payment is visible under a session scoped to tenant B")
		}
	}
	if !contains(ids, payB.ID().String()) {
		t.Fatal("tenant B cannot see its own payment; the policy is too tight, not too loose")
	}

	// A direct read by primary key must return zero rows, not a permission error. An error would
	// itself be an existence oracle: "permission denied" for a real identifier and "no rows" for
	// a fabricated one tells a caller which identifiers exist in other tenants.
	var n int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM pp.payments WHERE payment_id = $1`,
		payA.ID().String()).Scan(&n); err != nil {
		t.Fatalf("count by id: %v", err)
	}
	if n != 0 {
		t.Fatalf("a by-id read of another tenant's payment returned %d rows, want 0", n)
	}
}

// TestRLSBlocksADirectUpdate. USING filters what a statement can *see*; without it an UPDATE
// with no tenant predicate would rewrite every tenant's rows. This asserts the update touches
// nothing, and it does so with a raw statement so no repository logic is involved.
func TestRLSBlocksADirectUpdate(t *testing.T) {
	pool := testPool(t)
	seedTenants(t, pool)

	alphaCtx := tenantContext(t, tenantAlpha)
	uow := testUnitOfWork(t, pool)
	payA := newTestPayment(t, tenantAlpha, 4200)
	if err := uow.Within(alphaCtx, func(ctx context.Context, r ports.Repositories) error {
		return r.Payments.Create(ctx, payA)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tx, done := rawConn(t, pool, tenantBravo)
	defer done()
	ctx := context.Background()

	tag, err := tx.Exec(ctx, `
UPDATE pp.payments SET statement_descriptor = 'HIJACKED' WHERE payment_id = $1`,
		payA.ID().String())
	if err != nil {
		t.Fatalf("the update must be filtered, not rejected with an error: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("an UPDATE under tenant B modified %d of tenant A's rows", tag.RowsAffected())
	}

	// And an unqualified UPDATE — the shape a forgotten WHERE actually takes — must be equally
	// inert against the other tenant's rows.
	tag, err = tx.Exec(ctx, `UPDATE pp.payments SET statement_descriptor = 'HIJACKED'`)
	if err != nil {
		t.Fatalf("unqualified update: %v", err)
	}
	_ = tag
	var descriptor string
	tx2, done2 := rawConn(t, pool, tenantAlpha)
	defer done2()
	if err := tx2.QueryRow(context.Background(),
		`SELECT statement_descriptor FROM pp.payments WHERE payment_id = $1`,
		payA.ID().String()).Scan(&descriptor); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if descriptor == "HIJACKED" {
		t.Fatal("an unqualified UPDATE under tenant B rewrote tenant A's payment")
	}
}

// TestWithCheckRejectsACrossTenantInsert. A policy with only USING would let tenant A insert a
// row stamped with tenant B's identifier: A cannot read it back, but it is there, it appears in
// B's queries, and it corrupts B's data.
func TestWithCheckRejectsACrossTenantInsert(t *testing.T) {
	pool := testPool(t)
	seedTenants(t, pool)

	tx, done := rawConn(t, pool, tenantBravo)
	defer done()

	_, err := tx.Exec(context.Background(), `
INSERT INTO pp.payments (
    payment_id, partition_month, tenant_id, merchant_id, state, amount, currency,
    payment_method, capture_method, method_token, created_at, updated_at, version)
VALUES ($1, date_trunc('month', now()), $2, 'mrc_x', 'CREATED', 100, 'USD',
        'CARD', 'AUTOMATIC', 'tok_x', now(), now(), 1)`,
		shared.NewPaymentID().String(), tenantAlpha.String())
	if err == nil {
		t.Fatal("WITH CHECK must reject an insert stamped with another tenant's identifier")
	}
}

// TestQueryWithoutTenantContextReturnsNoRows proves the fail-closed property of
// current_setting('app.tenant_id', true): the missing_ok argument makes an unset GUC evaluate to
// NULL, the comparison to NULL, and the policy to not-true — so the answer is zero rows rather
// than an exception, and rather than every row.
func TestQueryWithoutTenantContextReturnsNoRows(t *testing.T) {
	pool := testPool(t)
	seedTenants(t, pool)

	alphaCtx := tenantContext(t, tenantAlpha)
	uow := testUnitOfWork(t, pool)
	if err := uow.Within(alphaCtx, func(ctx context.Context, r ports.Repositories) error {
		return r.Payments.Create(ctx, newTestPayment(t, tenantAlpha, 700))
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tx, done := rawConn(t, pool, "") // no GUC at all
	defer done()

	var n int
	if err := tx.QueryRow(context.Background(),
		`SELECT count(*) FROM pp.payments`).Scan(&n); err != nil {
		t.Fatalf("an unset GUC must yield zero rows, not an exception: %v", err)
	}
	if n != 0 {
		t.Fatalf("an unset app.tenant_id returned %d rows; it must return zero, never all", n)
	}
}

// TestAppRoleLacksBypassRLS. BYPASSRLS makes every policy inert for that role, silently and
// globally: the schema would still look protected and would protect nothing. The service asserts
// this at startup and refuses to become ready if it fails; this is the same assertion in CI.
func TestAppRoleLacksBypassRLS(t *testing.T) {
	pool := testPool(t)
	tx, done := rawConn(t, pool, "")
	defer done()

	var bypass, super bool
	if err := tx.QueryRow(context.Background(), `
SELECT rolbypassrls, rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&bypass, &super); err != nil {
		t.Fatalf("read pg_roles: %v", err)
	}
	if bypass {
		t.Fatal("the connected role has BYPASSRLS; every policy in this schema is inert")
	}
	if super {
		t.Fatal("the connected role is a superuser; RLS does not apply and this suite proves nothing")
	}
}

// TestEveryTenantScopedTableHasForcedRLSInCatalog is the live counterpart to the text-based
// check in migrations_test.go: it enumerates the catalog, so it also catches a policy that
// exists in a migration file but failed to apply.
func TestEveryTenantScopedTableHasForcedRLSInCatalog(t *testing.T) {
	pool := testPool(t)
	tx, done := rawConn(t, pool, "")
	defer done()

	rows, err := tx.Query(context.Background(), `
SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
       (SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'pp'
  AND c.relkind IN ('r','p')
  AND EXISTS (
      SELECT 1 FROM pg_attribute a
      WHERE a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped)
ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	defer rows.Close()

	checked := 0
	for rows.Next() {
		var name string
		var enabled, forced bool
		var policies int
		if err := rows.Scan(&name, &enabled, &forced, &policies); err != nil {
			t.Fatalf("scan: %v", err)
		}
		checked++
		if !enabled {
			t.Errorf("pp.%s has a tenant_id column but ROW LEVEL SECURITY is not enabled", name)
		}
		if !forced {
			t.Errorf("pp.%s has RLS enabled but not FORCED; the table owner would bypass it", name)
		}
		if policies == 0 {
			t.Errorf("pp.%s has RLS enabled and no policy, which denies everything — "+
				"almost certainly a migration that enabled RLS and forgot the policy", name)
		}
	}
	if checked == 0 {
		t.Fatal("no tenant-scoped tables found; the schema is not migrated")
	}
}

// TestPartitionsCarryRLSAndTheI3Index is the live form of `platformctl partitions verify`.
//
// A partition created without its I3 index is a partition in which double-charging is possible
// and nothing says so, which is precisely why the provisioning function creates the table and
// its indexes in one transaction.
func TestPartitionsCarryRLSAndTheI3Index(t *testing.T) {
	// Verifies: NFR-15, NFR-29.
	pool := testPool(t)
	tx, done := rawConn(t, pool, "")
	defer done()

	rows, err := tx.Query(context.Background(), `
SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
       (SELECT count(*) FROM pg_indexes i
         WHERE i.schemaname = 'pp' AND i.tablename = c.relname
           AND i.indexdef LIKE '%WHERE (outcome = ''SUCCESS''::text)%')
FROM pg_class c
JOIN pg_inherits inh ON inh.inhrelid = c.oid
JOIN pg_class parent ON parent.oid = inh.inhparent
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'pp' AND parent.relname = 'payment_attempts'`)
	if err != nil {
		t.Fatalf("enumerate partitions: %v", err)
	}
	defer rows.Close()

	found := 0
	for rows.Next() {
		var name string
		var enabled, forced bool
		var i3 int
		if err := rows.Scan(&name, &enabled, &forced, &i3); err != nil {
			t.Fatalf("scan: %v", err)
		}
		found++
		if !enabled || !forced {
			t.Errorf("partition pp.%s does not have RLS enabled and forced", name)
		}
		if i3 == 0 {
			t.Errorf("partition pp.%s has no partial unique index on (payment_id) "+
				"WHERE outcome = 'SUCCESS'; invariant I3 does not hold in that month", name)
		}
	}
	if found < 14 {
		t.Fatalf("found %d payment_attempts partitions, want at least 14 "+
			"(the previous month plus thirteen ahead)", found)
	}
}

// TestUnitOfWorkRefusesNesting. A caller who believes they hold a savepoint and does not is
// worse off than one who gets an error: their "rollback" would leave the outer transaction's
// writes in place and they would find out at reconciliation.
func TestUnitOfWorkRefusesNesting(t *testing.T) {
	pool := testPool(t)
	seedTenants(t, pool)
	ctx := tenantContext(t, tenantAlpha)
	uow := testUnitOfWork(t, pool)

	err := uow.Within(ctx, func(inner context.Context, _ ports.Repositories) error {
		return uow.Within(inner, func(context.Context, ports.Repositories) error { return nil })
	})
	if err == nil {
		t.Fatal("a nested unit of work must return an error, never silently join the outer one")
	}
	if apierror.CodeOf(err) != apierror.CodeInternalError {
		t.Fatalf("nesting error = %s, want INTERNAL_ERROR", apierror.CodeOf(err))
	}
}

// TestSetLocalDoesNotLeakAcrossTransactions is the PgBouncer bug, reproduced.
//
// `SET` (no LOCAL) works perfectly in every unit test, in every local run against a direct
// connection, and in session pooling. It fails only under transaction pooling, under
// concurrency, across tenants — that is, only in production. This asserts the GUC is gone when
// the connection is next borrowed.
func TestSetLocalDoesNotLeakAcrossTransactions(t *testing.T) {
	pool := testPool(t)
	seedTenants(t, pool)

	ctx := tenantContext(t, tenantAlpha)
	uow := testUnitOfWork(t, pool)
	if err := uow.Within(ctx, func(context.Context, ports.Repositories) error { return nil }); err != nil {
		t.Fatalf("first transaction: %v", err)
	}

	// The assertion inside Within (assertCleanGUC) fires if the previous transaction left a
	// residue on the connection it returned to the pool. Running a second transaction is the
	// whole test; a leak makes it fail here rather than silently reading the wrong tenant's data
	// a week later.
	if err := uow.Within(ctx, func(context.Context, ports.Repositories) error { return nil }); err != nil {
		t.Fatalf("the second transaction saw a residual app.tenant_id: %v", err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
