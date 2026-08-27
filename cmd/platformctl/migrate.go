package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
	"github.com/udaykishore-resu/payments-platform/migrations"
)

// runMigrate dispatches `migrate up|down|status`.
//
// Each sub-command gets its own FlagSet, so `--to` exists only where it means something. A shared
// flag set would accept `platformctl migrate status --to 12` and silently ignore it, which is how
// an operator becomes convinced a flag worked.
func runMigrate(ctx context.Context, args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "migrate requires a sub-command: up, down or status")
		return 2
	}
	switch args[0] {
	case "up":
		return migrateUp(ctx, args[1:], stdout, stderr)
	case "down":
		return migrateDown(ctx, args[1:], stdout, stderr)
	case "status":
		return migrateStatus(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown migrate sub-command %q\n", args[0])
		return 2
	}
}

// migrateUp applies pending migrations.
//
// # Why --dry-run is the interesting flag and not an afterthought
//
// A migration plan is the one artifact a reviewer can check before a deploy: it says exactly which
// files will run, in which order, and whether any already-applied migration's checksum has changed
// — the last being the signal that somebody edited a migration that has already run somewhere,
// which is the single most damaging thing that can happen to a schema history.
//
// The migrator takes an advisory lock, so two pods rolling out simultaneously do not both apply
// the same migration. The lock is why an interrupted run must be interruptible cleanly rather than
// killed: a killed process releases the lock when its connection closes, but a killed process
// mid-statement leaves a partially-applied migration that the checksum check will then flag.
func migrateUp(ctx context.Context, args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("migrate up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "print the plan and exit without applying anything")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall deadline for the migration run")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	pool, code := openPool(ctx, stderr)
	if pool == nil {
		return code
	}
	defer func() { _ = pool.Close(context.Background()) }()

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	m := postgres.NewMigrator(pool, migrations.FS)
	plan, err := m.Plan(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "could not read the migration state: %v\n", err)
		return 1
	}
	printPlan(stdout, plan)

	// A checksum conflict aborts before anything is applied. It means a migration that already ran
	// has been edited since, so the database and the repository disagree about history — and
	// applying more migrations on top of a disagreement makes it unrecoverable.
	if len(plan.Conflicts) > 0 {
		fmt.Fprintf(stderr,
			"\nrefusing to migrate: %d applied migration(s) have changed on disk.\n"+
				"An applied migration is immutable. Add a new migration instead of editing one.\n",
			len(plan.Conflicts))
		return 1
	}
	if len(plan.Pending) == 0 {
		fmt.Fprintln(stdout, "\nnothing to apply")
		return 0
	}
	if *dryRun {
		fmt.Fprintln(stdout, "\ndry run: nothing was applied")
		return 0
	}

	applied, err := m.Up(ctx, false)
	if err != nil {
		fmt.Fprintf(stderr, "migration failed after applying %d: %v\n", len(applied), err)
		return 1
	}
	fmt.Fprintf(stdout, "\napplied %d migration(s)\n", len(applied))
	return 0
}

// migrateDown rolls back to a version.
//
// # Why it demands --confirm and why the platform's real answer is forward-only
//
// deployment.md §5.4: rollback is forward-only with a compensating migration. A `down` script is
// kept for local development and for the narrow case of a migration that failed before its
// transaction committed — not as a production recovery path, because a down script that drops a
// column drops the data in it, and no amount of care makes that reversible.
//
// The `--confirm` flag is therefore not ceremony. It is the difference between an operator who
// typed a command and an operator who decided to.
func migrateDown(ctx context.Context, args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("migrate down", flag.ContinueOnError)
	fs.SetOutput(stderr)
	to := fs.Int("to", -1, "roll back to this version (required)")
	confirm := fs.Bool("confirm", false, "required: acknowledge that down migrations can destroy data")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *to < 0 {
		fmt.Fprintln(stderr, "migrate down requires --to VERSION")
		return 2
	}
	if !*confirm {
		fmt.Fprintln(stderr,
			"migrate down requires --confirm.\n\n"+
				"A down migration that drops a column drops the data in it, and this platform's\n"+
				"documented rollback path is forward-only with a compensating migration\n"+
				"(deployment.md §5.4). Use down only in development, or for a migration that\n"+
				"failed before its transaction committed.")
		return 2
	}

	pool, code := openPool(ctx, stderr)
	if pool == nil {
		return code
	}
	defer func() { _ = pool.Close(context.Background()) }()

	m := postgres.NewMigrator(pool, migrations.FS)
	if err := m.Down(ctx, *to); err != nil {
		fmt.Fprintf(stderr, "rollback failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "rolled back to version %d\n", *to)
	return 0
}

// migrateStatus prints the plan without applying anything.
//
// This is the command a readiness investigation runs: a pod refusing readiness because the schema
// is behind is answered by comparing this output against the binary's expectation.
func migrateStatus(ctx context.Context, args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("migrate status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	pool, code := openPool(ctx, stderr)
	if pool == nil {
		return code
	}
	defer func() { _ = pool.Close(context.Background()) }()

	plan, err := postgres.NewMigrator(pool, migrations.FS).Plan(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "could not read the migration state: %v\n", err)
		return 1
	}
	printPlan(stdout, plan)
	if len(plan.Conflicts) > 0 {
		return 1
	}
	return 0
}

// printPlan renders a migration plan.
//
// Conflicts are printed last and loudest: an operator scanning output stops at the first thing
// that looks wrong, and a checksum conflict buried above a list of forty applied migrations is a
// conflict nobody reads.
func printPlan(w *os.File, plan postgres.Plan) {
	fmt.Fprintf(w, "applied: %d\n", len(plan.Applied))
	if n := len(plan.Applied); n > 0 {
		fmt.Fprintf(w, "  latest: %s\n", plan.Applied[n-1])
	}
	fmt.Fprintf(w, "pending: %d\n", len(plan.Pending))
	for _, m := range plan.Pending {
		fmt.Fprintf(w, "  + %s\n", m)
	}
	if len(plan.Conflicts) == 0 {
		return
	}
	fmt.Fprintf(w, "\nCHECKSUM CONFLICTS: %d\n", len(plan.Conflicts))
	for _, v := range plan.Conflicts {
		fmt.Fprintf(w, "  ! version %d has changed since it was applied\n", v)
	}
}
