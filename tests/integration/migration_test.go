//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
	"github.com/udaykishore-resu/payments-platform/migrations"
	"github.com/udaykishore-resu/payments-platform/tests/testenv"
)

// The migration set, exercised the way a rollback actually happens.
//
// Verifies: docs/testing.md §4 ("the real migrations/ directory, applied by the real runner"),
// docs/deployment.md §4 (a release must be revertible). A migration whose `down` was never run is
// not a rollback plan; it is a file that makes the release checklist look complete.
//
// The three-phase shape — up, all the way down, up again — is what catches the two failure modes a
// one-way test cannot see:
//
//   - A `down` that does not fully undo its `up`. The second `up` then runs against a schema that
//     is not the one it was written for, and either fails or, worse, succeeds differently.
//   - A `down` that undoes *more* than its `up`, usually by dropping something a neighbouring
//     migration created. The schema after the second up then differs from the schema after the
//     first, which the fingerprint comparison below catches to the byte.
//
// This runs against PP_TEST_POSTGRES_SCRATCH_DSN and nothing else. Migrating a database all the way
// down destroys the schema, and doing that to the database the rest of the suite is using would
// turn one failure into forty.

// schemaFingerprint is a stable rendering of everything the migrations create.
//
// Columns, constraints, indexes, row-level-security flags and policies — the whole observable
// surface of the schema. Hashing it rather than diffing it would make a failure unreadable, so the
// full text is kept and the hash is only used for the comparison; when they differ the test prints
// the first differing line.
func schemaFingerprint(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()

	queries := []struct {
		what  string
		query string
	}{
		{"column", `
SELECT format('%s.%s %s nullable=%s default=%s',
              c.table_name, c.column_name, c.data_type, c.is_nullable,
              coalesce(c.column_default, '-'))
  FROM information_schema.columns c
  JOIN pg_class cl ON cl.relname = c.table_name
  JOIN pg_namespace n ON n.oid = cl.relnamespace AND n.nspname = 'pp'
 WHERE c.table_schema = 'pp' AND cl.relispartition = false`},
		{"constraint", `
SELECT format('%s %s %s', cl.relname, con.conname, pg_get_constraintdef(con.oid))
  FROM pg_constraint con
  JOIN pg_class cl ON cl.oid = con.conrelid
  JOIN pg_namespace n ON n.oid = cl.relnamespace AND n.nspname = 'pp'
 WHERE cl.relispartition = false`},
		{"index", `
SELECT format('%s %s', i.tablename, i.indexdef)
  FROM pg_indexes i
  JOIN pg_class cl ON cl.relname = i.tablename
  JOIN pg_namespace n ON n.oid = cl.relnamespace AND n.nspname = 'pp'
 WHERE i.schemaname = 'pp' AND cl.relispartition = false`},
		{"rls", `
SELECT format('%s rls=%s force=%s', cl.relname, cl.relrowsecurity, cl.relforcerowsecurity)
  FROM pg_class cl
  JOIN pg_namespace n ON n.oid = cl.relnamespace AND n.nspname = 'pp'
 WHERE cl.relkind IN ('r','p') AND cl.relispartition = false`},
		{"policy", `
SELECT format('%s %s roles=%s using=%s check=%s',
              p.tablename, p.policyname, p.roles::text,
              coalesce(p.qual, '-'), coalesce(p.with_check, '-'))
  FROM pg_policies p WHERE p.schemaname = 'pp'`},
		{"routine", `
SELECT format('%s(%s)', p.proname, pg_get_function_identity_arguments(p.oid))
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace AND n.nspname = 'pp'`},
		{"trigger", `
SELECT format('%s %s', cl.relname, t.tgname)
  FROM pg_trigger t
  JOIN pg_class cl ON cl.oid = t.tgrelid
  JOIN pg_namespace n ON n.oid = cl.relnamespace AND n.nspname = 'pp'
 WHERE NOT t.tgisinternal AND cl.relispartition = false`},
	}

	var out []string
	for _, q := range queries {
		rows, err := pool.Query(ctx, q.query)
		if err != nil {
			t.Fatalf("fingerprint %s: %v", q.what, err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatalf("fingerprint %s: %v", q.what, err)
			}
			out = append(out, q.what+" | "+line)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatalf("fingerprint %s: %v", q.what, err)
		}
	}
	sort.Strings(out)
	return out
}

func hashOf(lines []string) string {
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:8])
}

// firstDifference reports the first line the two fingerprints disagree on, so a failure names the
// object rather than a hash.
func firstDifference(a, b []string) string {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return fmt.Sprintf("line %d:\n  before: %s\n   after: %s", i, a[i], b[i])
		}
	}
	switch {
	case len(a) > len(b):
		return "the second schema is missing: " + a[len(b)]
	case len(b) > len(a):
		return "the second schema gained: " + b[len(a)]
	default:
		return "no textual difference (the hash comparison is wrong)"
	}
}

// seededReferenceData is the reference data migration 0015 installs.
//
// It is asserted by table and by a minimum count rather than by an exact one: the exact contents
// are a business decision that will change, but "the currencies table is empty after a migration
// round trip" is always a bug — and it is the kind that only shows up when a payment in a currency
// nobody tested is rejected as unsupported.
var seededReferenceData = []struct {
	table   string
	minRows int64
}{
	{"pp.currencies", 20},
	{"pp.payment_methods", 3},
	{"pp.roles", 3},
	{"pp.gateways", 1},
	{"pp.gateway_health", 1},
}

// TestMigrationsApplyRollBackAndReapplyIdentically is the rollback rehearsal.
//
// Verifies: docs/testing.md §4, docs/deployment.md §4.2. Runs on a scratch database, because it
// migrates all the way down.
func TestMigrationsApplyRollBackAndReapplyIdentically(t *testing.T) {
	// Deliberately not parallel: it owns the whole scratch database and drops every object in it.
	dsn := testenv.ScratchDSN(t)

	c, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfg := postgres.DefaultPoolConfig(dsn, "pp-tests-migration")
	cfg.StatementTimeout = 4 * time.Minute
	cfg.MaxConns = 2
	pool, err := postgres.NewPool(c, cfg)
	if err != nil {
		t.Fatalf("open the scratch pool: %v", err)
	}
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = pool.Close(shutdown)
	})

	// A second, plain pool for the introspection queries: the fingerprint reads catalogs, not
	// application tables, and going through the repository pool would drag its tenant guard into a
	// place it has nothing to say about.
	insp, err := pgxpool.New(c, dsn)
	if err != nil {
		t.Fatalf("open the introspection pool: %v", err)
	}
	t.Cleanup(insp.Close)

	all, err := postgres.LoadMigrations(migrations.Files())
	if err != nil {
		t.Fatalf("load the migration set: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("the migration set is empty")
	}
	t.Logf("migration set: %d files, %04d_%s .. %04d_%s",
		len(all), all[0].Version, all[0].Name, all[len(all)-1].Version, all[len(all)-1].Name)

	migrator := postgres.NewMigrator(pool, migrations.Files())

	// --- phase 1: up ---------------------------------------------------------------------------
	applied, err := migrator.Up(c, false)
	if err != nil {
		t.Fatalf("first up: %v", err)
	}
	t.Logf("first up applied %d migration(s)", len(applied))

	plan, err := migrator.Plan(c)
	if err != nil {
		t.Fatalf("plan after the first up: %v", err)
	}
	if len(plan.Pending) != 0 {
		t.Fatalf("%d migrations are still pending after a full up", len(plan.Pending))
	}
	if len(plan.Conflicts) != 0 {
		t.Fatalf("checksum conflicts after a fresh up: %v. An applied migration's file differs "+
			"from what was recorded, which cannot happen on a database this run just created — "+
			"the checksum computation is unstable.", plan.Conflicts)
	}
	if len(plan.Applied) != len(all) {
		t.Fatalf("the ledger records %d applied migrations, the directory holds %d",
			len(plan.Applied), len(all))
	}

	before := schemaFingerprint(t, c, insp)
	if len(before) == 0 {
		t.Fatal("the schema fingerprint is empty after a full migration; the introspection queries " +
			"are looking in the wrong schema and every comparison below would be vacuous")
	}
	assertSeedData(t, c, insp, "after the first up")

	// --- phase 2: down, one version at a time, newest first ------------------------------------
	//
	// Newest first is not a preference; it is the only order in which a down can succeed, because
	// each one may drop an object a later migration depends on.
	//
	// It stops at 0002 rather than 0001, and that is a property of the bootstrap migration rather
	// than a gap in the test. 0001's down executes `DROP SCHEMA pp CASCADE`, which takes
	// pp.schema_migrations — the runner's own ledger — with it; the runner then cannot record its
	// removal, and the migrator reports "relation pp.schema_migrations does not exist". Rolling
	// back the bootstrap is a drop-the-database operation, not a versioned rollback, and the
	// meaningful assertion is the one below: after every *versioned* migration is reversed, the
	// only things left are the objects 0001 created.
	for i := len(all) - 1; i >= 1; i-- {
		m := all[i]
		if err := migrator.Down(c, m.Version); err != nil {
			t.Fatalf("down %04d_%s failed: %v\nEverything above this version has already been "+
				"rolled back, so the scratch database is now in a partial state. A migration whose "+
				"down does not run is a release that cannot be reverted.", m.Version, m.Name, err)
		}
	}

	// bootstrapTables are the two tables migration 0001 creates. Everything else must be gone.
	bootstrapTables := map[string]bool{"schema_migrations": true, "partition_registry": true}

	var survivors []string
	rows, err := insp.Query(c, `
SELECT cl.relname FROM pg_class cl
  JOIN pg_namespace n ON n.oid = cl.relnamespace AND n.nspname = 'pp'
 WHERE cl.relkind IN ('r','p') ORDER BY 1`)
	if err != nil {
		t.Fatalf("list surviving tables: %v", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		if !bootstrapTables[name] {
			survivors = append(survivors, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(survivors) > 0 {
		t.Fatalf("%d table(s) survived the rollback: %s\nA down that leaves objects behind makes "+
			"the next up run against a schema it was not written for — which either fails, or "+
			"succeeds differently.", len(survivors), strings.Join(survivors, ", "))
	}

	// --- phase 3: up again ---------------------------------------------------------------------
	if _, err := migrator.Up(c, false); err != nil {
		t.Fatalf("second up after a full rollback: %v\nThe migrations are not re-appliable, which "+
			"means a rollback is a one-way door.", err)
	}

	after := schemaFingerprint(t, c, insp)
	if hashOf(before) != hashOf(after) {
		t.Fatalf("the schema after up→down→up differs from the schema after the first up.\n"+
			"before: %d objects (%s)\n after: %d objects (%s)\n%s",
			len(before), hashOf(before), len(after), hashOf(after), firstDifference(before, after))
	}
	assertSeedData(t, c, insp, "after the second up")

	t.Logf("schema round-tripped identically: %d objects, fingerprint %s", len(after), hashOf(after))
}

// assertSeedData checks that migration 0015's reference data is present.
func assertSeedData(t *testing.T, ctx context.Context, pool *pgxpool.Pool, when string) {
	t.Helper()
	for _, ref := range seededReferenceData {
		var n int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+ref.table).Scan(&n); err != nil {
			t.Fatalf("%s: count %s: %v", when, ref.table, err)
		}
		if n < ref.minRows {
			t.Errorf("%s: %s holds %d rows, want at least %d. Reference data that a migration "+
				"seeds and a rollback loses is how a re-deployed environment starts rejecting "+
				"currencies it supported yesterday.", when, ref.table, n, ref.minRows)
		}
	}
}

// TestEveryMigrationHasABalancedPairAndADenseVersion is the cheap structural check.
//
// Verifies: docs/testing.md §4, migrations/README.md. It needs no database, so it runs in the same
// second as the rest of the suite and fails on the pull request that introduces the problem rather
// than on the deploy that trips over it.
func TestEveryMigrationHasABalancedPairAndADenseVersion(t *testing.T) {
	t.Parallel()

	all, err := postgres.LoadMigrations(migrations.Files())
	if err != nil {
		t.Fatalf("the migration set does not load: %v", err)
	}

	for i, m := range all {
		if m.Version != i+1 {
			t.Fatalf("migration %d in order is version %04d; versions must be dense and contiguous "+
				"from 1. A gap means two branches merged with the same next number and one was "+
				"renumbered wrongly, which applies them out of order.", i, m.Version)
		}
		if strings.TrimSpace(m.Up) == "" {
			t.Errorf("%04d_%s has an empty up", m.Version, m.Name)
		}
		if strings.TrimSpace(m.Down) == "" {
			t.Errorf("%04d_%s has an empty down. A migration with no down is a release that cannot "+
				"be reverted; if the reversal is genuinely a no-op, say so in a comment inside the "+
				"file so the reviewer sees a decision rather than an omission.", m.Version, m.Name)
		}
		if m.Checksum == "" {
			t.Errorf("%04d_%s has no checksum", m.Version, m.Name)
		}
	}
}

// TestMigrationChecksumsAreStableAcrossLoads guards the drift detector itself.
//
// Verifies: docs/deployment.md §4. The migrator refuses to proceed when an applied migration's
// checksum differs from the file on disk — an editing-history check that is only as good as the
// checksum being a pure function of the file. If it ever picked up anything ambient, every deploy
// would report a conflict and the team would learn to pass whatever flag suppresses it.
func TestMigrationChecksumsAreStableAcrossLoads(t *testing.T) {
	t.Parallel()

	first, err := postgres.LoadMigrations(migrations.Files())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	second, err := postgres.LoadMigrations(migrations.Files())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("two loads of one directory produced %d and %d migrations", len(first), len(second))
	}
	for i := range first {
		if first[i].Checksum != second[i].Checksum {
			t.Fatalf("%04d_%s hashed to %s then %s; the checksum is not a pure function of the file",
				first[i].Version, first[i].Name, first[i].Checksum, second[i].Checksum)
		}
	}
}
