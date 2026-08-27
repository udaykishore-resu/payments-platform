package testenv

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
	"github.com/udaykishore-resu/payments-platform/migrations"
)

// migrateOnce guards the one-time migration of the shared database. Every test package that uses
// this harness calls Postgres(t); running the migrator once per test would serialize the suite
// behind an advisory lock for no benefit.
var (
	migrateOnce sync.Once
	migrateErr  error
)

// Postgres opens a connection pool against the shared test database, applying migrations the
// first time it is called in the process, and skips the test when no DSN is configured.
//
// The pool is pgx's rather than the repository's *postgres.Pool for one reason: half of what
// these suites assert is what the database does when the application layer is *not* in the way,
// and that requires issuing SQL the repositories would never issue. The repository pool is still
// used for the migration run, because testing against a schema built by anything other than the
// real runner proves nothing about production.
func Postgres(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := PostgresDSN(t)

	migrateOnce.Do(func() { migrateErr = migrate(dsn) })
	if migrateErr != nil {
		t.Fatalf("apply migrations to %s: %v", EnvPostgresDSN, migrateErr)
	}
	return openPool(t, dsn)
}

// openPool opens a pgxpool and registers its close.
func openPool(t testing.TB, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns = 12
	// A statement timeout on the harness pool, not the money path's: the negative tests here
	// deliberately provoke lock waits, and a test that hangs forever on a lock is indistinguishable
	// from a test that is simply slow until CI's global timeout kills the whole run.
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "30000"
	cfg.ConnConfig.RuntimeParams["application_name"] = "pp-tests"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping %s: %v", EnvPostgresDSN, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// migrate applies every up migration using the production runner.
func migrate(dsn string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cfg := postgres.DefaultPoolConfig(dsn, "pp-tests-migrate")
	cfg.StatementTimeout = 2 * time.Minute
	cfg.MaxConns = 2
	pool, err := postgres.NewPool(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open migration pool: %w", err)
	}
	defer func() {
		shutdown, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = pool.Close(shutdown)
	}()

	if _, err := postgres.NewMigrator(pool, migrations.Files()).Up(ctx, false); err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// EnsurePartitions creates the monthly partitions the fixtures need.
//
// It is called from Isolate rather than from the migration path because the set of months a test
// needs depends on the test: the partition-alignment cases deliberately write into a neighbouring
// month, and a harness that only ever provisioned "now" would make those untestable.
func EnsurePartitions(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `SELECT pp.create_future_partitions(3)`); err != nil {
		t.Fatalf("create partitions: %v", err)
	}
}

// RequireNonBypassRLS skips unless the connected role is subject to row-level security.
//
// This is not a nicety. Every isolation and invariant negative test in this suite asserts that
// the *database* refuses something. Run as a superuser or a BYPASSRLS role, those tests pass by
// reading rows they were supposed to be denied and then asserting on a policy that never ran —
// a green result that means the opposite of what it appears to mean. Skipping loudly is the only
// honest outcome.
func RequireNonBypassRLS(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var super, bypass bool
	var role string
	err := pool.QueryRow(ctx, `
SELECT current_user, rolsuper, rolbypassrls
FROM pg_roles WHERE rolname = current_user`).Scan(&role, &super, &bypass)
	if err != nil {
		t.Fatalf("inspect current role: %v", err)
	}
	if super || bypass {
		t.Skipf("connected as %q which is superuser=%v bypassrls=%v: the RLS and constraint negative "+
			"tests would pass without the database doing anything. Point %s at the pp_app role instead.",
			role, super, bypass, EnvPostgresDSN)
	}
}

// Scope is one test's private namespace in the shared database.
type Scope struct {
	Pool *pgxpool.Pool
	// TenantA and TenantB are two distinct tenants owned by this test. Two rather than one
	// because the isolation tests need a foreign tenant, and a foreign tenant borrowed from
	// another test would couple the two.
	TenantA string
	TenantB string
	// MerchantA and MerchantB belong to TenantA and TenantB respectively.
	MerchantA string
	MerchantB string

	// Clock is the fixed clock every fixture timestamp comes from.
	Clock *Clock

	nonce  string
	t      testing.TB
	before map[string]int64
}

// Isolate builds a per-test namespace, seeds its two tenants and merchants, and registers the
// cleanup assertion.
//
// Cleanup here is deliberately *not* "delete everything this test wrote". The money tables have
// DELETE revoked from the application role by design (retention is a partition DETACH, never a
// DELETE), so a harness that promised to delete payment rows would either be lying or would have
// to run as a role that makes every other assertion in the suite meaningless. What it asserts
// instead is the property that actually protects the next test: every deletable artifact is gone,
// and the *shared* namespace — the rows belonging to no test's tenant — is byte-for-byte as it
// was found. A test that wrote outside its own tenant fails here, in its own failure message,
// rather than as an inexplicable flake in an unrelated test three days later.
func Isolate(t testing.TB, pool *pgxpool.Pool) *Scope {
	t.Helper()
	EnsurePartitions(t, pool)

	nonce := Nonce(t)
	base := BaseTime()
	s := &Scope{
		Pool:      pool,
		TenantA:   DeterministicID(PrefixTenant, base, nonce+"/A"),
		TenantB:   DeterministicID(PrefixTenant, base, nonce+"/B"),
		MerchantA: DeterministicID(PrefixMerchant, base, nonce+"/mA"),
		MerchantB: DeterministicID(PrefixMerchant, base, nonce+"/mB"),
		Clock:     NewClock(base),
		nonce:     nonce,
		t:         t,
	}

	s.SeedTenant(s.TenantA)
	s.SeedTenant(s.TenantB)
	s.SeedMerchant(s.TenantA, s.MerchantA)
	s.SeedMerchant(s.TenantB, s.MerchantB)

	s.before = s.sharedSnapshot()
	t.Cleanup(s.assertClean)
	return s
}

// ID mints a deterministic id inside this scope's namespace.
func (s *Scope) ID(prefix, seed string) string {
	return DeterministicID(prefix, s.Clock.Now(), s.nonce+"/"+seed)
}

// IDAt mints a deterministic id anchored to a chosen instant, for the partition-alignment cases.
func (s *Scope) IDAt(prefix string, at time.Time, seed string) string {
	return DeterministicID(prefix, at, s.nonce+"/"+seed)
}

// Tenanted runs fn on a connection whose session GUC is set to tenant, inside a transaction that
// is always rolled back.
//
// Rollback rather than commit is the default because every constraint this suite exercises is
// immediate: a CHECK, a unique index and an RLS policy all fire at statement time, so a rolled-back
// transaction observes exactly the same rejection a committed one would, and leaves nothing behind
// in tables the role cannot delete from. Tests that genuinely need cross-connection visibility use
// TenantedCommitted instead, and pay for it by owning their own cleanup.
func (s *Scope) Tenanted(ctx context.Context, tenant string, fn func(pgx.Tx) error) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if tenant != "" {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
			return fmt.Errorf("set app.tenant_id: %w", err)
		}
	}
	return fn(tx)
}

// TenantedCommitted runs fn in a transaction that commits if fn returns nil.
func (s *Scope) TenantedCommitted(ctx context.Context, tenant string, fn func(pgx.Tx) error) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	if tenant != "" {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
			return fmt.Errorf("set app.tenant_id: %w", err)
		}
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// MustTenanted is Tenanted with the error turned into a fatal, for setup steps.
func (s *Scope) MustTenanted(tenant string, fn func(pgx.Tx) error) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Tenanted(ctx, tenant, fn); err != nil {
		s.t.Fatalf("tenanted transaction for %s: %v", tenant, err)
	}
}

// MustCommit is TenantedCommitted with the error turned into a fatal.
func (s *Scope) MustCommit(tenant string, fn func(pgx.Tx) error) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.TenantedCommitted(ctx, tenant, fn); err != nil {
		s.t.Fatalf("committed transaction for %s: %v", tenant, err)
	}
}

// SeedTenant inserts a tenant row. The insert runs with the GUC already set to the tenant being
// written, because pp.tenants is under FORCE RLS with a WITH CHECK clause — writing a tenant row
// the session is not scoped to is refused, which is itself an assertion elsewhere in this suite.
func (s *Scope) SeedTenant(id string) {
	s.t.Helper()
	s.MustCommit(id, func(tx pgx.Tx) error {
		// The column list is exactly what pp.tenants declares NOT NULL without a default, plus
		// the two timestamps. Naming a column the table does not have fails every test in the
		// suite with one confusing message from the harness, so this list is asserted by
		// TestSeedsMatchTheSchema in the suites that use it rather than trusted.
		_, err := tx.Exec(context.Background(), `
INSERT INTO pp.tenants (tenant_id, name, tier, status, residency_region, created_at, updated_at)
VALUES ($1, $2, 'POOLED', 'ACTIVE', 'GLOBAL', $3, $3)
ON CONFLICT (tenant_id) DO NOTHING`, id, "test-"+SanitizeName(s.t.Name()), s.Clock.Now())
		return err
	})
}

// SeedMerchant inserts an ACTIVE merchant.
func (s *Scope) SeedMerchant(tenant, id string) {
	s.t.Helper()
	s.MustCommit(tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `
INSERT INTO pp.merchants (merchant_id, tenant_id, legal_name, display_name, environment,
                          status, created_at, updated_at, activated_at)
VALUES ($1, $2, $3, $3, 'sandbox', 'ACTIVE', $4, $4, $4)
ON CONFLICT (merchant_id) DO NOTHING`, id, tenant, "Test Merchant "+s.nonce[:8], s.Clock.Now())
		return err
	})
}

// sharedSnapshot counts the rows that belong to no test tenant, per table.
//
// "No test tenant" is defined as "not this scope's two tenants". A test that writes a row under
// some *other* tenant — the failure mode this whole mechanism exists to catch — shows up here as
// a count that moved.
func (s *Scope) sharedSnapshot() map[string]int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out := make(map[string]int64, len(sharedCountTables))
	for _, tbl := range sharedCountTables {
		var n int64
		// Counted with no tenant GUC set: RLS then filters to zero for tenant-scoped tables,
		// so a non-zero count is itself a finding. The tables listed are the ones that are
		// either platform-scoped or that a leaked write would land in.
		err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n)
		if err != nil {
			// A table the role cannot read is not a table this assertion can be made about.
			// Recording -1 keeps the map shape stable so the diff below stays readable.
			n = -1
		}
		out[tbl] = n
	}
	return out
}

// sharedCountTables is the set whose global row count must not move across a test. Reference data
// and the platform-scoped registries are here; the tenant-scoped money tables are not, because RLS
// makes their unscoped count zero by construction and the meaningful assertion about them is the
// per-tenant one below.
// pp.webhook_dedup and pp.event_dedup were here and are deliberately not: both are legitimately
// written by tests (a webhook dedup claim, a consumer dedup row) and neither carries a tenant_id,
// because the key is the *gateway's* namespace and the *consumer group's* namespace respectively.
// In a suite where every test runs in parallel, a global count of a table other tests are entitled
// to write is not an invariant of any one test — it reports the neighbour's work as this test's
// leak, and the failure lands on whichever test happened to finish first. The tests that write
// those tables assert their own rows are gone instead, which is the property that actually
// protects the next test.
var sharedCountTables = []string{
	"pp.gateways",
	"pp.currencies",
	"pp.payment_methods",
	"pp.roles",
	"pp.gateway_health",
	"pp.schema_migrations",
}

// assertClean is the registered cleanup. It deletes what the role is permitted to delete and then
// asserts the shared namespace is exactly as it was found.
func (s *Scope) assertClean() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, tenant := range []string{s.TenantA, s.TenantB} {
		// Deletable artifacts, in dependency order. Failures are reported rather than fatal:
		// a cleanup error must not mask the test's own failure, but it must be visible.
		stmts := []string{
			`DELETE FROM pp.outbox_events WHERE tenant_id = $1`,
			`DELETE FROM pp.idempotency_records WHERE tenant_id = $1`,
			`DELETE FROM pp.reconciliation_exceptions WHERE tenant_id = $1`,
			`DELETE FROM pp.inbound_webhooks WHERE tenant_id = $1`,
			`DELETE FROM pp.workflow_steps WHERE tenant_id = $1`,
			`DELETE FROM pp.workflow_dlq WHERE tenant_id = $1`,
			`DELETE FROM pp.workflow_instances WHERE tenant_id = $1`,
			`DELETE FROM pp.gateway_connections WHERE tenant_id = $1`,
		}
		if err := s.TenantedCommitted(ctx, tenant, func(tx pgx.Tx) error {
			for _, q := range stmts {
				if _, err := tx.Exec(ctx, q, tenant); err != nil {
					return fmt.Errorf("cleanup %q: %w", q, err)
				}
			}
			return nil
		}); err != nil {
			s.t.Errorf("cleanup for %s: %v", tenant, err)
		}

		// Assert the deletable set is actually empty, per table, so a silently-failing DELETE
		// is a failure here rather than a mystery in the next test.
		s.assertEmpty(ctx, tenant,
			"pp.outbox_events", "pp.idempotency_records", "pp.reconciliation_exceptions",
			"pp.inbound_webhooks", "pp.workflow_steps", "pp.workflow_instances")
	}

	after := s.sharedSnapshot()
	var drift []string
	keys := make([]string, 0, len(after))
	for k := range after {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s.before[k] != after[k] {
			drift = append(drift, fmt.Sprintf("%s: %d -> %d", k, s.before[k], after[k]))
		}
	}
	if len(drift) > 0 {
		s.t.Errorf("test mutated shared state outside its own tenants:\n  %s", strings.Join(drift, "\n  "))
	}
}

// assertEmpty checks that a tenant owns no rows in the named tables.
func (s *Scope) assertEmpty(ctx context.Context, tenant string, tables ...string) {
	_ = s.Tenanted(ctx, tenant, func(tx pgx.Tx) error {
		for _, tbl := range tables {
			var n int64
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+tbl+` WHERE tenant_id = $1`, tenant).Scan(&n); err != nil {
				s.t.Errorf("count %s for %s: %v", tbl, tenant, err)
				continue
			}
			if n != 0 {
				s.t.Errorf("cleanup left %d row(s) in %s for tenant %s", n, tbl, tenant)
			}
		}
		return nil
	})
}

// --- error classification ---------------------------------------------------------------------

// PgErrCode returns the SQLSTATE of err, or "" when err is not a database error.
//
// Tests assert on the SQLSTATE rather than the message. A message is prose that a future
// migration may reword; 23505 is the contract.
func PgErrCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// PgConstraint returns the constraint name a database error names, or "".
func PgConstraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// SQLSTATEs the suite asserts on.
const (
	SQLStateUniqueViolation     = "23505"
	SQLStateCheckViolation      = "23514"
	SQLStateForeignKeyViolation = "23503"
	SQLStateInsufficientPriv    = "42501"
	// SQLStateRLSViolation is what a WITH CHECK failure raises: "new row violates row-level
	// security policy". Postgres reports it as check_violation's sibling 42501, and as 23514 for
	// a table-level policy on some versions, so tests accept either and say why.
	SQLStateRLSViolation = "42501"
)

// RequireDBRejection fails the test unless err is a database rejection carrying one of the
// accepted SQLSTATEs.
//
// The helper exists so that "the database refused this" is asserted as a single fact rather than
// re-derived at twenty call sites, each of which would be free to accept a *different* error and
// so to pass while the property was broken.
func RequireDBRejection(t testing.TB, err error, what string, accept ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected the DATABASE to reject this, got no error — the constraint or policy that "+
			"enforces it is missing or has been widened", what)
	}
	code := PgErrCode(err)
	if code == "" {
		t.Fatalf("%s: expected a database rejection, got a non-database error: %v", what, err)
	}
	for _, a := range accept {
		if code == a {
			return
		}
	}
	t.Fatalf("%s: database rejected with SQLSTATE %s (constraint %q), want one of %v: %v",
		what, code, PgConstraint(err), accept, err)
}

// Nonce returns the scope's stable per-test namespace token, for tests that need to build a
// unique-but-reproducible string the harness does not already mint for them.
func (s *Scope) Nonce() string { return s.nonce }

// T returns the testing handle the scope was built with.
func (s *Scope) T() testing.TB { return s.t }
