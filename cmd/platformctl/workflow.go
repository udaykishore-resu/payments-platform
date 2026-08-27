package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
)

// runWorkflow dispatches `workflow list|resume|dlq`.
func runWorkflow(ctx context.Context, args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "workflow requires a sub-command: list, resume or dlq")
		return 2
	}
	pool, code := openPool(ctx, stderr)
	if pool == nil {
		return code
	}
	defer func() { _ = pool.Close(context.Background()) }()

	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("workflow list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		state := fs.String("state", "", "filter by instance state")
		stuckFor := fs.Duration("stuck-for", 0, "only instances with no progress for this long")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		limit := fs.Int("limit", 50, "maximum instances to print")
		fmt.Fprintf(stdout, "workflow list state=%q stuck-for=%s\n\n", *state, *stuckFor)
		reports := postgres.NewOperatorReports(pool)
		printRole(ctx, stdout, reports)

		counts, err := reports.WorkflowCounts(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "could not count workflow instances: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "instances by state:")
		for _, k := range sortedKeys(counts) {
			fmt.Fprintf(stdout, "  %-16s %d\n", k, counts[k])
		}

		rows, err := reports.WorkflowList(ctx, *state, *stuckFor, *limit)
		if err != nil {
			fmt.Fprintf(stderr, "could not list workflow instances: %v\n", err)
			return 1
		}
		if len(rows) == 0 {
			fmt.Fprintln(stdout, "\nno instances match")
			return 0
		}
		// Ordered oldest-updated first, because the interesting instance is always the one that
		// has not moved. A list ordered by creation would bury it under whatever started most
		// recently.
		fmt.Fprintf(stdout, "\n%-30s %-22s %-15s %-18s %-8s %s\n",
			"INSTANCE", "DEFINITION", "STATE", "STEP", "ATTEMPT", "IDLE")
		for _, r := range rows {
			fmt.Fprintf(stdout, "%-30s %-22s %-15s %-18s %-8d %s\n",
				r.ID, truncate(r.Definition, 22), r.State, truncate(r.CurrentStep, 18),
				r.Attempt, time.Since(r.UpdatedAt).Round(time.Second))
			if r.LastError != "" {
				fmt.Fprintf(stdout, "  last error: %s\n", truncate(r.LastError, 140))
			}
		}
		return 0

	case "resume":
		fs := flag.NewFlagSet("workflow resume", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 1 {
			fmt.Fprintln(stderr, "workflow resume requires exactly one instance id")
			return 2
		}
		id, err := shared.ParseWorkflowID(fs.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 2
		}
		// Resume is safe to run twice: the engine's step records make re-execution a no-op for a
		// step that already succeeded. That property is why this command needs no --confirm while
		// `migrate down` does.
		fmt.Fprintf(stdout, "resuming %s\n", id)
		reports := postgres.NewOperatorReports(pool)

		before, found, err := reports.WorkflowState(ctx, id.String())
		if err != nil {
			fmt.Fprintf(stderr, "could not read the instance: %v\n", err)
			return 1
		}
		if !found {
			fmt.Fprintf(stderr, "no workflow instance %s (or this role cannot see it)\n", id)
			return 1
		}
		resumed, state, err := reports.ResumeWorkflow(ctx, id.String())
		if err != nil {
			fmt.Fprintf(stderr, "could not resume: %v\n", err)
			return 1
		}
		if !resumed {
			// COMPLETED and ABORTED are finished. "Resuming" one would mean re-running a workflow
			// whose effects — a provisioned gateway account, a posted ledger entry — have already
			// happened, so the refusal is the whole point rather than a limitation.
			fmt.Fprintf(stderr,
				"instance %s is %s and is not resumable: its effects have already happened.\n"+
					"If it needs to run again, start a new instance rather than replaying this one.\n",
				id, before)
			return 1
		}
		fmt.Fprintf(stdout,
			"  %s → %s; lease cleared, fencing epoch bumped, runnable now.\n"+
				"  A worker will pick it up within its poll interval. Re-running this command is\n"+
				"  harmless: a step that already succeeded replays as a no-op.\n",
			before, state)
		return 0

	case "dlq":
		fs := flag.NewFlagSet("workflow dlq", flag.ContinueOnError)
		fs.SetOutput(stderr)
		limit := fs.Int("limit", 50, "maximum entries to print")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		fmt.Fprintln(stdout, "workflow dlq")
		reports := postgres.NewOperatorReports(pool)
		printRole(ctx, stdout, reports)

		entries, err := reports.WorkflowDLQ(ctx, *limit)
		if err != nil {
			fmt.Fprintf(stderr, "could not list the dead-letter queue: %v\n", err)
			return 1
		}
		if len(entries) == 0 {
			fmt.Fprintln(stdout, "empty — no unreplayed dead-lettered steps")
			return 0
		}
		// The payload is summarised by size and never printed. It is the step's input, which for
		// an onboarding or payment workflow contains merchant data; triage needs the reason and
		// the counts, and reading the contents is a separate, audited action.
		fmt.Fprintf(stdout, "\n%-10s %-30s %-18s %-8s %-10s %s\n",
			"DLQ", "INSTANCE", "STEP", "REPLAYS", "PAYLOAD", "PARKED / REASON")
		for _, e := range entries {
			fmt.Fprintf(stdout, "%-10d %-30s %-18s %-8d %-10s %s\n",
				e.ID, e.InstanceID, truncate(e.StepKey, 18), e.ReplayCount,
				fmt.Sprintf("%dB", e.PayloadSize),
				time.Since(e.ParkedAt).Round(time.Second))
			fmt.Fprintf(stdout, "  %s\n", truncate(e.Reason, 140))
		}
		// A non-empty DLQ is a finding, not a status. Exiting 1 makes this usable as a check.
		fmt.Fprintf(stderr, "\n%d dead-lettered step(s) awaiting an operator\n", len(entries))
		return 1

	default:
		fmt.Fprintf(stderr, "unknown workflow sub-command %q\n", args[0])
		return 2
	}
}

// reportBlocked states that a command cannot complete here, what it would need, and what the
// operator should run instead.
//
// # Why this exists, now that every sub-command has a body
//
// Its predecessor, reportUnimplemented, meant "nobody has written this yet" and is gone: every
// sub-command is implemented. This one means something different and permanent — "this is written
// and it genuinely cannot run from this process". A DR drill needs AWS control-plane credentials
// and a second region; a certification run needs a live gateway credential and outbound network
// to the vendor's sandbox. Neither is a missing feature, and neither will ever be a thing this
// CLI does on its own, so the message names the capability and points at the command that has it.
//
// Both exit 2 rather than 0. A CLI sub-command that prints a plausible header and exits 0 without
// doing anything is worse than one that does not exist: an operator runs it during an incident,
// sees success, and concludes the thing it was supposed to do has happened.
func reportBlocked(stderr *os.File, command, missing, instead string) int {
	fmt.Fprintf(stderr,
		"\n%s cannot complete from this process.\nWhat it needs: %s.\nWhat to run instead: %s\n"+
			"Exiting 2 rather than reporting success, because a command that prints a header and\n"+
			"exits 0 is a command an operator will trust during an incident.\n",
		command, missing, instead)
	return 2
}
