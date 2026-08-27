package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
)

// runOutbox dispatches `outbox status`.
func runOutbox(ctx context.Context, args []string, stdout, stderr *os.File) int {
	if len(args) == 0 || args[0] != "status" {
		fmt.Fprintln(stderr, "outbox requires a sub-command: status")
		return 2
	}
	pool, code := openPool(ctx, stderr)
	if pool == nil {
		return code
	}
	defer func() { _ = pool.Close(context.Background()) }()

	// Backlog is the SLI for every asynchronous path in the platform: a growing outbox means
	// events are late, and events being late means projections, webhooks and the ledger are all
	// behind — visible to a merchant long before it is visible on a dashboard nobody is watching.
	reports := postgres.NewOperatorReports(pool)
	printRole(ctx, stdout, reports)

	st, err := reports.Outbox(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "could not read the outbox: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "unpublished  %d\n", st.Unpublished)
	fmt.Fprintf(stdout, "failed       %d (at least one publish attempt)\n", st.Failed)
	fmt.Fprintf(stdout, "claimed      %d (held by a relay replica)\n", st.Claimed)
	if st.OldestUnpublished.IsZero() {
		fmt.Fprintln(stdout, "oldest       none — the outbox is drained")
	} else {
		age := time.Since(st.OldestUnpublished).Round(time.Second)
		// The age, not the count, is the number that matters: ten thousand rows published within
		// a second of each other is a healthy burst, and one row stuck for an hour is an incident.
		fmt.Fprintf(stdout, "oldest       %s (age %s)\n",
			st.OldestUnpublished.Format(time.RFC3339), age)
	}
	if len(st.ByTopic) > 0 {
		fmt.Fprintln(stdout, "\nby topic:")
		for _, t := range sortedKeys(st.ByTopic) {
			fmt.Fprintf(stdout, "  %-40s %d\n", t, st.ByTopic[t])
		}
	}
	// Exit 1 on a stalled outbox, not merely on a non-empty one. A backlog is normal; a backlog
	// whose oldest entry predates the relay's retry ladder is a relay that is not making progress,
	// and an operator running this from a check wants that to be a failure.
	if !st.OldestUnpublished.IsZero() && time.Since(st.OldestUnpublished) > outboxStallThreshold {
		fmt.Fprintf(stderr, "\nthe oldest unpublished event is older than %s: the relay is not draining\n",
			outboxStallThreshold)
		return 1
	}
	return 0
}

// outboxStallThreshold is where a backlog stops being a burst and starts being an incident.
//
// Five minutes: the relay's retry ladder tops out well inside it, so anything older has failed
// repeatedly rather than merely waited.
const outboxStallThreshold = 5 * time.Minute

// printRole reports the connected role and whether it can see across tenants.
//
// Every platform-wide count this tool prints is subject to row-level security, and "the backlog
// is zero" and "this role cannot see the backlog" are the same character on a terminal at three
// in the morning. Saying which one it is costs one line.
func printRole(ctx context.Context, stdout *os.File, r *postgres.OperatorReports) {
	name, bypasses, err := r.Role(ctx)
	if err != nil {
		return
	}
	scope := "TENANT-SCOPED — row-level security applies, counts below may be partial"
	if bypasses {
		scope = "platform-wide"
	}
	fmt.Fprintf(stdout, "connected as %s (%s)\n\n", name, scope)
}
