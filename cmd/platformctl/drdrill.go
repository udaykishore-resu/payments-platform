package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

// runDRDrill exercises the disaster-recovery runbook and reports the measured RTO and RPO.
func runDRDrill(ctx context.Context, args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("dr-drill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scenario := fs.String("scenario", "region-failover", "region-failover, writer-failover or restore")
	dryRun := fs.Bool("dry-run", true, "print the steps without executing them")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch *scenario {
	case "region-failover", "writer-failover", "restore":
	default:
		fmt.Fprintf(stderr, "unknown scenario %q: expected region-failover, writer-failover or restore\n", *scenario)
		return 2
	}
	fmt.Fprintf(stdout, "dr-drill scenario=%s dry-run=%v\n\n", *scenario, *dryRun)

	// The flag defaults to dry-run, unlike every other flag in this tool. A DR drill fails over a
	// region: the default has to be the harmless one, because the cost of accidentally running it
	// is an outage and the cost of accidentally not running it is a printed plan.
	//
	// The plan is printed in both modes, because the plan is the useful artifact even when the
	// drill runs: it is what an observer follows and what the report is written against.
	fmt.Fprintln(stdout, "plan (docs/disaster-recovery.md §3):")
	for i, step := range drillPlan(*scenario) {
		fmt.Fprintf(stdout, "  %d. %s\n", i+1, step)
	}
	if *dryRun {
		fmt.Fprintln(stdout, "\ndry run: nothing was executed. Re-run with --dry-run=false to perform the drill.")
		return 0
	}

	// The honest exit. Every step above is an AWS control-plane operation — a Route 53 record
	// change, an Aurora failover, a restore from a snapshot — performed with credentials this CLI
	// deliberately does not hold. Printing a header and exiting 0 here would produce a drill
	// report for a drill that never happened, which is worse than no report: the RTO figure in it
	// would be quoted in a compliance review.
	return reportBlocked(stderr, "dr-drill",
		"live AWS control-plane access (Route 53, RDS failover, S3 restore) and a second region "+
			"to fail over to, none of which this CLI holds credentials for",
		"run the drill from the runbook with an operator session:\n"+
			"    scripts/dr-drill.sh --scenario "+*scenario+"\n"+
			"  and record the observed RTO and RPO in docs/disaster-recovery.md's drill log")
}

// drillPlan is the step list for each scenario, from docs/disaster-recovery.md §3.
//
// It is printed rather than executed, and printing it is not a consolation prize: a drill's value
// is mostly in the plan being current, and a plan that lives in a runbook nobody opened since the
// last reorganisation is the usual reason a real failover goes badly.
func drillPlan(scenario string) []string {
	switch scenario {
	case "writer-failover":
		return []string{
			"snapshot the current writer endpoint and the replica lag",
			"trigger an Aurora failover to the reader in the same region",
			"observe the application's pool re-resolving the writer endpoint (expect ≤ 60 s)",
			"assert no payment reached a terminal state on a lost transaction (tests/chaos)",
			"record the measured RTO and the observed replication lag at cutover as the RPO",
		}
	case "restore":
		return []string{
			"identify the restore point and the snapshot that covers it",
			"restore into an isolated cluster — never over the live one",
			"run scripts/sql/dr-invariants.sql against the restored cluster",
			"compare the ledger balance and the payment count against the last known-good figures",
			"record the wall time from decision to a verified cluster as the RTO",
		}
	default: // region-failover
		return []string{
			"confirm the standby region's replica lag is inside the RPO budget",
			"drain the primary region: stop accepting new payments, let in-flight ones finish",
			"promote the standby's Aurora global cluster secondary to writer",
			"shift Route 53 weighted records to the standby region",
			"verify readiness in the standby: probes green, one synthetic payment end to end",
			"record RTO from the decision to the first successful synthetic payment",
			"record RPO from the replica lag observed at promotion",
		}
	}
}
