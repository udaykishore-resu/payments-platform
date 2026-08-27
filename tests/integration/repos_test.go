//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
	"github.com/udaykishore-resu/payments-platform/tests/testenv"
)

// This file is the bridge between testenv's raw-SQL harness and the *real* repositories.
//
// Both halves are necessary and they assert different things. The raw-SQL half (testenv.Scope)
// exists to attack the database with the application layer removed, which is the only way to learn
// whether a constraint is still there. This half exists to exercise the production adapters —
// the idempotency store's claim SQL, the outbox relay's SKIP LOCKED claim, the webhook repository's
// dedup insert — because a test that re-implements the production statement can pass while
// production is broken, and those three statements are exactly where the money-safety properties
// live.

// repoWritesJSONB records a defect this suite found the first time it was run against a real
// PostgreSQL, and explains why several tests below seed with raw SQL instead of the repository.
//
// postgres.DefaultPoolConfig sets `DefaultQueryExecMode = pgx.QueryExecModeExec`, which skips the
// protocol-level parameter Describe and makes pgx infer each parameter's PostgreSQL type from the
// Go value. A `[]byte` is inferred as `bytea`. Every repository method that passes a `[]byte` into
// a `jsonb` column therefore fails with SQLSTATE 22P02 (`invalid input syntax for type json`,
// "Token \ is invalid") — the bytea escape form arriving where JSON was expected. Confirmed
// against PostgreSQL 16 for OutboxRepository.Append ($9::jsonb[], the headers array); the same
// shape appears in WebhookRepository.Record, WorkflowRepository.CreateInstance / SaveStep /
// SaveInstance and ReconciliationRepository.OpenException.
//
// It is a production defect, not a test-only one: the same pool configuration is what the
// composition root builds. The fix belongs in internal/infrastructure/postgres — either an
// explicit `::jsonb` cast plus a `string` parameter, or `QueryExecModeCacheDescribe` — and this
// suite deliberately does not work around it silently: the tests that need those rows write them
// with raw SQL and say so at the call site, so the coverage that remains is honest about what it
// is exercising and what it is not.
const repoWritesJSONB = "see the comment above: repository writes into jsonb columns are broken " +
	"under QueryExecModeExec; these tests seed with raw SQL and exercise the read side"

// tenantContextKey is *this package's* key for the tenant.
//
// internal/infrastructure/postgres deliberately refuses to import a tenant package and instead
// takes a resolver function, so that a tenant identity can enter the system from exactly one
// place. A test binary is entitled to be that place for itself, and doing it with a
// package-private key means no other package — including a future one in this repository — can
// forge a tenant into a context this suite's repositories will honour.
type tenantContextKey struct{}

// withTenant returns a context the repositories will accept.
func withTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, tenantContextKey{}, tenant)
}

// init installs the resolver once, for the whole test binary.
//
// It is a pure function of the context, which is what makes `t.Parallel()` safe here: nothing in
// the resolution path is mutable after this point, so two tests running concurrently under two
// different tenants cannot observe each other's identity. That property is worth stating because
// the older suite in internal/infrastructure/postgres runs sequentially for exactly the opposite
// reason — it rebinds the resolver per test.
func init() {
	postgres.UseTenantResolver(func(ctx context.Context) (string, bool) {
		v, ok := ctx.Value(tenantContextKey{}).(string)
		return v, ok && v != ""
	})
}

var (
	appPoolOnce sync.Once
	appPoolVal  *postgres.Pool
	appPoolErr  error
)

// appPool returns the process-wide *postgres.Pool the real repositories run on.
//
// One pool per process rather than per test: the repositories are stateless over it, and opening
// sixteen pools against one PostgreSQL would exhaust max_connections long before it exhausted the
// suite. It is deliberately not closed — the process exiting closes it, and a t.Cleanup that
// closed a shared pool would break every test still running in parallel.
func appPool(t *testing.T) *postgres.Pool {
	t.Helper()
	dsn := testenv.PostgresDSN(t)
	// Migrations first, through testenv, so a suite that starts at this file rather than at
	// setup() still meets a migrated schema.
	testenv.Postgres(t)

	appPoolOnce.Do(func() {
		cfg := postgres.DefaultPoolConfig(dsn, "pp-tests-integration")
		// The negative tests here deliberately provoke lock waits and the concurrency tests run
		// sixteen goroutines at once; the money path's 3 s statement timeout would turn a slow CI
		// runner into a spurious failure that reads like a product bug.
		cfg.StatementTimeout = 30 * time.Second
		cfg.LockTimeout = 10 * time.Second
		cfg.MaxConns = 16

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		appPoolVal, appPoolErr = postgres.NewPool(ctx, cfg)
	})
	if appPoolErr != nil {
		t.Fatalf("open the application pool: %v", appPoolErr)
	}
	return appPoolVal
}

// newUoW builds a unit of work on the shared pool with the leaked-GUC assertion switched on.
//
// The assertion is on for the whole suite on purpose: this is the only environment where the
// transaction-pooling GUC leak of docs/multi-tenancy.md §2.4 is reproducible at all, and it costs
// one round trip per transaction to find it here rather than in production.
func newUoW(t *testing.T, clock shared.Clock) *postgres.UnitOfWork {
	t.Helper()
	if clock == nil {
		clock = shared.SystemClock{}
	}
	return postgres.NewUnitOfWork(appPool(t), clock, true)
}

// inTx runs fn in one real transaction as tenant, failing the test on error.
func inTx(t *testing.T, uow *postgres.UnitOfWork, tenant string, fn func(context.Context, ports.Repositories) error) {
	t.Helper()
	c, cancel := context.WithTimeout(withTenant(context.Background(), tenant), 30*time.Second)
	defer cancel()
	if err := uow.Within(c, fn); err != nil {
		t.Fatalf("transaction for tenant %s: %v", tenant, err)
	}
}

// tryTx runs fn in one real transaction and returns the error instead of failing, for the cases
// where the error *is* the assertion.
func tryTx(uow *postgres.UnitOfWork, tenant string, fn func(context.Context, ports.Repositories) error) error {
	c, cancel := context.WithTimeout(withTenant(context.Background(), tenant), 30*time.Second)
	defer cancel()
	return uow.Within(c, fn)
}

// movableClock is a clock a test can wind forward without sleeping.
//
// Mutex-guarded because several tests read it from the goroutines they fan out, and a data race
// on the clock would be reported by -race as a failure in whatever test happened to be running.
type movableClock struct {
	mu sync.Mutex
	t  time.Time
}

func newMovableClock(at time.Time) *movableClock {
	return &movableClock{t: at.UTC().Truncate(time.Millisecond)}
}

func (c *movableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock. It is the only sanctioned way to make time pass in this suite: a
// time.Sleep would be a bet that CI is no slower than the machine the test was written on.
func (c *movableClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

// runToken distinguishes one execution of this suite from the next.
//
// Every other identifier in these tests is deterministic, by design: the same test produces the
// same tenant on every run, so a failure leaves rows a human can find. That cannot hold for rows
// written to pp.payments, pp.payment_attempts or pp.ledger_entries, because DELETE is revoked on
// those tables for the application role — retention is a partition DETACH, never a DELETE — so a
// deterministic payment id would collide with the row the previous run left behind, and the suite
// would pass exactly once per database.
//
// The token is therefore used *only* in the seed of an identifier that will be committed to a
// non-deletable table. Everything else stays reproducible.
var runToken = func() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A failure here cannot be worked around by falling back to a fixed value, because a
		// fixed value is exactly the collision this exists to prevent.
		panic("integration: cannot read randomness for the run token: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}()

// committedSeed builds a seed for an id that will outlive the test.
func committedSeed(parts ...string) string {
	s := runToken
	for _, p := range parts {
		s += "/" + p
	}
	return s
}

// countRows is the small query almost every assertion below ends in. It runs on the harness pool
// with the tenant GUC set, so RLS applies exactly as it would to the application.
func countRows(t *testing.T, s *testenv.Scope, tenant, query string, args ...any) int64 {
	t.Helper()
	var n int64
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := s.Tenanted(c, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(c, query, args...).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// requireCount asserts a row count and says what it was counting when it fails.
func requireCount(t *testing.T, s *testenv.Scope, tenant string, want int64, what, query string, args ...any) {
	t.Helper()
	if got := countRows(t, s, tenant, query, args...); got != want {
		t.Fatalf("%s: %d rows, want %d", what, got, want)
	}
}

// describeOutcomes renders a tally for a failure message, sorted so two runs of the same failure
// produce the same text.
func describeOutcomes(counts map[string]int) string {
	return fmt.Sprintf("%v", counts)
}
