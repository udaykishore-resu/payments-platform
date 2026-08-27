package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Migration is one numbered schema change and its compensating script.
type Migration struct {
	// Version is the leading integer of the filename. It is dense and contiguous from 1;
	// a gap means two branches merged with the same next number and one of them was renumbered
	// wrong, which the parser refuses rather than applying out of order.
	Version int
	// Name is the slug between the version and the direction.
	Name string
	// Up and Down are the file contents.
	Up   string
	Down string
	// Checksum is the SHA-256 of Up, recorded when applied. A later run that computes a
	// different checksum for an applied version refuses to proceed: someone edited a migration
	// that production has already run, and the schema in front of you is no longer the schema
	// the file describes.
	Checksum string
}

// migrationLockID is the advisory-lock key the runner holds for the whole migration.
//
// A constant, because every pod must contend for the *same* lock. During a rolling deploy several
// pods start within milliseconds of each other; without the lock they all read an empty
// schema_migrations, all decide migration 7 is pending, and all run it. The second one gets a
// duplicate-object error that looks like a broken migration rather than a lost race, and
// somebody spends an hour reading the SQL.
const migrationLockID int64 = 0x7070_6D69_6772_0001

// Migrator applies embedded migrations to a database.
//
// It connects through the pool it is given, which for a migration Job is a pool configured with
// the pp_migrate role and a much longer statement timeout than the money path uses — a
// `CREATE INDEX` that trips the 3-second budget is not a slow query, it is a normal index build.
type Migrator struct {
	pool *Pool
	fsys fs.FS
}

// NewMigrator builds a runner over an embedded (or synthetic) filesystem of .sql files.
//
// It takes fs.FS rather than embed.FS so a test can hand it a broken set — an up with no down, a
// gap in the numbering, a checksum that changed — without writing files to disk.
func NewMigrator(pool *Pool, fsys fs.FS) *Migrator { return &Migrator{pool: pool, fsys: fsys} }

// Plan is what a run would do.
type Plan struct {
	Applied []Migration
	Pending []Migration
	// Conflicts lists versions whose recorded checksum differs from the file on disk. A non-empty
	// Conflicts is fatal: it means an applied migration was edited, so the database and the
	// repository disagree about what the schema is, and every subsequent migration was written
	// against one of the two.
	Conflicts []int
}

// LoadMigrations parses and validates the migration set.
//
// The validation is part of the loader rather than a separate test helper, so a malformed set
// fails at startup — where a human is watching — instead of halfway through an apply.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "migrations: cannot read directory")
	}

	type pair struct {
		name string
		up   string
		down string
	}
	byVersion := map[int]*pair{}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, direction, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, apierror.Wrapf(err, apierror.CodeInternalError,
				"migrations: cannot read %s", e.Name())
		}
		p := byVersion[version]
		if p == nil {
			p = &pair{name: name}
			byVersion[version] = p
		}
		if p.name != name {
			return nil, apierror.Newf(apierror.CodeInternalError,
				"migrations: version %d has two different names, %q and %q", version, p.name, name)
		}
		switch direction {
		case "up":
			p.up = string(body)
		case "down":
			p.down = string(body)
		}
	}

	versions := make([]int, 0, len(byVersion))
	for v := range byVersion {
		versions = append(versions, v)
	}
	sort.Ints(versions)

	out := make([]Migration, 0, len(versions))
	for i, v := range versions {
		// Contiguity. A gap is not merely untidy: the runner applies in order and records what it
		// applied, so a set that jumps from 7 to 9 either lost a migration in a merge or has one
		// waiting on an unmerged branch, and both mean the schema this run produces is not the
		// schema anyone reviewed.
		if v != i+1 {
			return nil, apierror.Newf(apierror.CodeInternalError,
				"migrations: numbering is not contiguous; expected %04d, found %04d", i+1, v)
		}
		p := byVersion[v]
		if p.up == "" {
			return nil, apierror.Newf(apierror.CodeInternalError,
				"migrations: %04d_%s has no up script", v, p.name)
		}
		if p.down == "" {
			// Every up needs a down, even though production rollback is forward-only. The down
			// exists to tear down a test database and — more usefully — to force the author to
			// work out what undoing their change would take, which is where "this is a one-way
			// door" is normally discovered.
			return nil, apierror.Newf(apierror.CodeInternalError,
				"migrations: %04d_%s has no down script", v, p.name)
		}
		sum := sha256.Sum256([]byte(p.up))
		out = append(out, Migration{
			Version:  v,
			Name:     p.name,
			Up:       p.up,
			Down:     p.down,
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	return out, nil
}

func parseMigrationName(filename string) (version int, name, direction string, err error) {
	base := strings.TrimSuffix(filename, ".sql")
	dot := strings.LastIndexByte(base, '.')
	if dot < 0 {
		return 0, "", "", apierror.Newf(apierror.CodeInternalError,
			"migrations: %q is not NNNN_name.up.sql or NNNN_name.down.sql", filename)
	}
	direction = base[dot+1:]
	if direction != "up" && direction != "down" {
		return 0, "", "", apierror.Newf(apierror.CodeInternalError,
			"migrations: %q has direction %q, want up or down", filename, direction)
	}
	rest := base[:dot]
	underscore := strings.IndexByte(rest, '_')
	if underscore <= 0 {
		return 0, "", "", apierror.Newf(apierror.CodeInternalError,
			"migrations: %q has no version prefix", filename)
	}
	version, convErr := strconv.Atoi(rest[:underscore])
	if convErr != nil || version <= 0 {
		return 0, "", "", apierror.Newf(apierror.CodeInternalError,
			"migrations: %q has a non-numeric version prefix", filename)
	}
	return version, rest[underscore+1:], direction, nil
}

// Plan reports what Up would apply, without applying anything.
//
// It takes the same advisory lock a real run does, so the plan it prints is the plan a run would
// take rather than a plausible-looking guess taken while another pod was mid-apply.
func (m *Migrator) Plan(ctx context.Context) (Plan, error) {
	all, err := LoadMigrations(m.fsys)
	if err != nil {
		return Plan{}, err
	}

	conn, err := m.pool.pool.Acquire(ctx)
	if err != nil {
		return Plan{}, mapError(err, "migrations: acquire connection")
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return Plan{}, mapError(err, "migrations: acquire advisory lock")
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID) }()

	applied, err := readApplied(ctx, conn.Conn())
	if err != nil {
		return Plan{}, err
	}

	var p Plan
	for _, mig := range all {
		recorded, ok := applied[mig.Version]
		switch {
		case !ok:
			p.Pending = append(p.Pending, mig)
		case recorded != mig.Checksum:
			p.Conflicts = append(p.Conflicts, mig.Version)
			p.Applied = append(p.Applied, mig)
		default:
			p.Applied = append(p.Applied, mig)
		}
	}
	return p, nil
}

// Up applies every pending migration, in order, each in its own transaction.
//
// Each migration gets its own transaction rather than all of them sharing one. The trade is
// explicit: one big transaction would make a failure leave nothing behind, but it would also
// hold every lock every migration takes for the duration of the whole run — and a migration set
// that takes an ACCESS EXCLUSIVE lock on payments in step 3 and then runs for another minute has
// blocked the payment path for that minute. Per-migration transactions mean a failure leaves the
// database at a known, recorded version, which is exactly what the forward-only rollback policy
// needs.
//
// dryRun performs every check, acquires the lock, and applies nothing.
func (m *Migrator) Up(ctx context.Context, dryRun bool) ([]Migration, error) {
	all, err := LoadMigrations(m.fsys)
	if err != nil {
		return nil, err
	}

	conn, err := m.pool.pool.Acquire(ctx)
	if err != nil {
		return nil, mapError(err, "migrations: acquire connection")
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return nil, mapError(err, "migrations: acquire advisory lock")
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	// Bootstrap. The ledger table has to exist before it can be read, and it is created outside
	// the per-migration transactions so that a first run against an empty database does not
	// depend on migration 0001 having succeeded to record that migration 0001 succeeded.
	if !dryRun {
		if err := ensureLedgerTable(ctx, conn.Conn()); err != nil {
			return nil, err
		}
	}

	applied, err := readApplied(ctx, conn.Conn())
	if err != nil {
		return nil, err
	}

	var ran []Migration
	for _, mig := range all {
		recorded, ok := applied[mig.Version]
		if ok {
			if recorded != mig.Checksum {
				return ran, apierror.Newf(apierror.CodeInternalError,
					"migrations: %04d_%s was applied with a different checksum; an applied "+
						"migration has been edited. Fix it forward with a new migration rather "+
						"than changing history", mig.Version, mig.Name)
			}
			continue
		}
		if dryRun {
			ran = append(ran, mig)
			continue
		}

		started := time.Now()
		tx, err := conn.Begin(ctx)
		if err != nil {
			return ran, mapError(err, "migrations: begin")
		}
		if _, err := tx.Exec(ctx, mig.Up); err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return ran, apierror.Wrapf(err, apierror.CodeInternalError,
				"migrations: %04d_%s failed", mig.Version, mig.Name)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO pp.schema_migrations (version, name, checksum, applied_at, duration_ms)
VALUES ($1,$2,$3,now(),$4)`,
			mig.Version, mig.Name, mig.Checksum, time.Since(started).Milliseconds()); err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return ran, mapError(err, "migrations: record application")
		}
		if err := tx.Commit(ctx); err != nil {
			return ran, mapError(err, "migrations: commit")
		}
		ran = append(ran, mig)
	}
	return ran, nil
}

// Down applies one migration's compensating script and removes its ledger row.
//
// It is for tearing down a test database and for nothing else, which is why it takes exactly one
// version rather than a range: a caller that has to name each version it is undoing cannot
// accidentally unwind a schema by passing a bound that was larger than they thought. Production
// rollback is a new, higher-numbered, compensating migration — see migrations/README.md §3 for
// why, at length.
func (m *Migrator) Down(ctx context.Context, version int) error {
	all, err := LoadMigrations(m.fsys)
	if err != nil {
		return err
	}
	var target *Migration
	for i := range all {
		if all[i].Version == version {
			target = &all[i]
			break
		}
	}
	if target == nil {
		return apierror.Newf(apierror.CodeInternalError, "migrations: no migration %04d", version)
	}

	conn, err := m.pool.pool.Acquire(ctx)
	if err != nil {
		return mapError(err, "migrations: acquire connection")
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return mapError(err, "migrations: acquire advisory lock")
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return mapError(err, "migrations: begin")
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, target.Down); err != nil {
		return apierror.Wrapf(err, apierror.CodeInternalError,
			"migrations: down %04d_%s failed", target.Version, target.Name)
	}
	// The ledger row is removed only if the ledger still exists. Migration 0001 creates the
	// schema that holds `pp.schema_migrations`, so its own down script necessarily destroys the
	// table this statement would write to — and the resulting "relation does not exist" would
	// abort the transaction and roll the drop back, leaving `Down(ctx, 1)` permanently
	// impossible. Guarding on the table's presence rather than special-casing version 1 keeps
	// the rule stated in terms of the actual precondition: you cannot record the removal of the
	// thing that records removals.
	var ledgerExists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('pp.schema_migrations') IS NOT NULL`).
		Scan(&ledgerExists); err != nil {
		return mapError(err, "migrations: check ledger presence")
	}
	if ledgerExists {
		if _, err := tx.Exec(ctx,
			`DELETE FROM pp.schema_migrations WHERE version = $1`, target.Version); err != nil {
			return mapError(err, "migrations: remove ledger row")
		}
	}
	return mapError(tx.Commit(ctx), "migrations: commit")
}

func ensureLedgerTable(ctx context.Context, conn *pgx.Conn) error {
	const q = `
CREATE SCHEMA IF NOT EXISTS pp;
CREATE TABLE IF NOT EXISTS pp.schema_migrations (
    version     INTEGER      PRIMARY KEY,
    name        TEXT         NOT NULL,
    checksum    TEXT         NOT NULL,
    applied_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    applied_by  TEXT         NOT NULL DEFAULT current_user,
    duration_ms INTEGER      NOT NULL DEFAULT 0
);`
	if _, err := conn.Exec(ctx, q); err != nil {
		return mapError(err, "migrations: bootstrap ledger table")
	}
	return nil
}

func readApplied(ctx context.Context, conn *pgx.Conn) (map[int]string, error) {
	out := map[int]string{}
	rows, err := conn.Query(ctx,
		`SELECT version, checksum FROM pp.schema_migrations ORDER BY version`)
	if err != nil {
		var pgErr = err
		// An empty database has no ledger table yet, and that is not an error — it is the
		// starting state. Any other failure is.
		if strings.Contains(pgErr.Error(), "does not exist") {
			return out, nil
		}
		return nil, mapError(err, "migrations: read ledger")
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, mapError(err, "migrations: read ledger")
		}
		out[v] = sum
	}
	return out, mapError(rows.Err(), "migrations: read ledger")
}

// String renders a migration for a dry-run log line.
func (m Migration) String() string {
	return fmt.Sprintf("%04d_%s (%d bytes, sha256:%s)",
		m.Version, m.Name, len(m.Up), m.Checksum[:12])
}
