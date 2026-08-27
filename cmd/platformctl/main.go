// Command platformctl is the platform's operator CLI: migrations, seeding, configuration
// validation, certification, DR drills, outbox and workflow inspection, and audit-chain
// verification.
//
// # Why the standard library's flag package and no CLI dependency
//
// A CLI framework buys sub-command routing, help generation and shell completion. This tool has
// nine sub-commands and is run by operators from a runbook, so the routing is a switch statement
// and the help is a printed string. What the dependency would cost is the thing that matters: this
// binary is what an operator reaches for during an incident, often from a debug container, and
// every dependency is a supply-chain surface on a tool that holds database credentials and can
// mutate money-adjacent state.
//
// # Every sub-command that writes states what it will do before doing it
//
// `migrate up` prints its plan, `seed` refuses production outright. The pattern is deliberate: a
// tool that acts first and reports afterwards is a tool nobody can safely run under pressure.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/udaykishore-resu/payments-platform/internal/platform/runtime"
)

var (
	version = ""
	commit  = ""
	date    = ""
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run dispatches a sub-command.
//
// It takes its streams as parameters so the CLI is testable without capturing global state: a test
// asserts on a bytes.Buffer rather than on whatever the process happened to write to fd 1.
func run(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	// The signal context is installed before any sub-command runs, so a long migration or a DR
	// drill can be interrupted cleanly rather than leaving a held advisory lock behind.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "migrate":
		return runMigrate(ctx, rest, stdout, stderr)
	case "seed":
		return runSeed(ctx, rest, stdout, stderr)
	case "config":
		return runConfig(ctx, rest, stdout, stderr)
	case "certify":
		return runCertify(ctx, rest, stdout, stderr)
	case "dr-drill":
		return runDRDrill(ctx, rest, stdout, stderr)
	case "outbox":
		return runOutbox(ctx, rest, stdout, stderr)
	case "workflow":
		return runWorkflow(ctx, rest, stdout, stderr)
	case "verify-audit-chain":
		return runVerifyAuditChain(ctx, rest, stdout, stderr)
	case "version":
		b := runtime.Stamp(version, commit, date)
		fmt.Fprintf(stdout, "platformctl %s (%s, built %s)\n", b.Version, b.Commit, b.Date)
		if b.Modified {
			fmt.Fprintln(stdout, "warning: built from a modified working tree")
		}
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
		usage(stderr)
		return 2
	}
}

// usage prints the command list.
//
// It is one string rather than generated help because the ordering is editorial: `migrate` first
// because it is what a deploy runs, `verify-audit-chain` last because it is what a compliance
// review runs, and the destructive ones flagged inline where an operator reading fast will see it.
func usage(w *os.File) {
	fmt.Fprint(w, `platformctl — payments platform operator CLI

Usage:
  platformctl <command> [flags]

Commands:
  migrate up            Apply pending migrations. Prints the plan first.
  migrate down          Roll back to a version. Requires --to and --confirm.
  migrate status        Show applied, pending and conflicting migrations.
  seed                  Generate a deterministic synthetic dataset. Refused in production.
  config validate FILE  Validate a configuration document against L4 without publishing it.
  certify MERCHANT_ID   Run the certification suite against a merchant's connections.
  dr-drill              Exercise the disaster-recovery runbook and report the measured RTO/RPO.
  outbox status         Report outbox backlog and the oldest unpublished message.
  workflow list         List workflow instances by state.
  workflow resume ID    Resume a stuck or failed workflow instance.
  workflow dlq          List dead-lettered workflow steps.
  verify-audit-chain T  Re-compute a tenant's audit hash chain and report the first break.
  version               Print the build stamp.

Environment:
  PP_DSN or DATABASE_URL   PostgreSQL connection string. Required by every command that
                           touches the database.

Exit codes:
  0  success
  1  the command ran and reported a failure (a broken chain, a failed drill)
  2  the command could not run (bad flags, missing configuration)
`)
}
