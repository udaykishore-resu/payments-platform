//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/udaykishore-resu/payments-platform/tests/testenv"
)

// setup is the entry point of every test in this package.
//
// It skips before it does anything expensive, refuses to run as a role that would make the
// negative assertions vacuous, and hands back a per-test namespace whose cleanup is asserted.
// Three lines at the top of each test rather than a TestMain that configures process-wide state:
// process-wide state is what makes t.Parallel() unsafe, and t.Parallel() is what makes the
// isolation properties under test actually get exercised.
func setup(t *testing.T) (*pgxpool.Pool, *testenv.Scope) {
	t.Helper()
	pool := testenv.Postgres(t)
	testenv.RequireNonBypassRLS(t, pool)
	return pool, testenv.Isolate(t, pool)
}

// ctx returns a context with the suite's default per-test budget.
//
// 30 seconds is generous for a database round trip and mean for a deadlock: a test that exceeds
// it has almost certainly blocked on a lock, and failing with a deadline is a far better report
// than hanging until CI's global timeout kills the whole run and names nothing.
func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}
