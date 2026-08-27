package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
)

// runVerifyAuditChain re-computes a tenant's audit hash chain.
//
// # Why this is a command rather than a background job
//
// The chain is verified on demand, by a person, because its output is evidence. A background job
// that verified continuously would produce an alert nobody could act on at three in the morning,
// and the correct response to a broken chain is an investigation rather than a restart.
//
// A break is reported with the first tampered sequence number, which is the fact an investigation
// starts from: everything before it is intact, everything after is suspect.
func runVerifyAuditChain(ctx context.Context, args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("verify-audit-chain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.Int64("from", 1, "first sequence number to verify")
	to := fs.Int64("to", 0, "last sequence number; 0 means the tail")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "verify-audit-chain requires exactly one tenant id")
		return 2
	}
	tenantID, err := shared.ParseTenantID(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 2
	}

	pool, code := openPool(ctx, stderr)
	if pool == nil {
		return code
	}
	defer func() { _ = pool.Close(context.Background()) }()

	reports := postgres.NewOperatorReports(pool)
	lo, hi, err := reports.AuditChainRange(ctx, tenantID.String())
	if err != nil {
		fmt.Fprintf(stderr, "could not read the audit chain range: %v\n", err)
		return 1
	}
	if hi == 0 {
		fmt.Fprintf(stderr, "tenant %s has no audit records (or this role cannot see them)\n", tenantID)
		return 1
	}
	// The bounds default to the whole chain rather than to a guess: an operator verifying a chain
	// during an investigation should not have to look up its extent first, and a default of 1..0
	// would silently verify nothing.
	if *from < lo {
		*from = lo
	}
	if *to == 0 || *to > hi {
		*to = hi
	}
	fmt.Fprintf(stdout, "verifying the audit chain for %s from %d to %d (chain spans %d..%d)\n",
		tenantID, *from, *to, lo, hi)

	// Through the repository and the unit of work, not through a direct query: the chain
	// recomputation is the repository's, and a second implementation here would be a second thing
	// that could disagree with the one the platform actually appends with.
	tctx, err := operatorTenantContext(ctx, tenantID)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 2
	}
	uow := postgres.NewUnitOfWork(pool, shared.SystemClock{}, false)
	var (
		intact bool
		broken int64
	)
	if err := uow.Within(tctx, func(ctx context.Context, r ports.Repositories) error {
		ok, seq, err := r.Audit.VerifyRange(ctx, tenantID, *from, *to)
		intact, broken = ok, seq
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "could not verify the chain: %v\n", err)
		return 1
	}
	if intact {
		fmt.Fprintf(stdout, "\nintact: %d record(s) verified, every digest links to its predecessor\n",
			*to-*from+1)
		return 0
	}
	// The first tampered sequence is the fact an investigation starts from: everything before it
	// is intact, everything after it is suspect. Reporting "the chain is broken" without the
	// sequence number would leave the investigator to bisect it by hand.
	fmt.Fprintf(stderr,
		"\nBROKEN at sequence %d.\n"+
			"  Records %d..%d verify. Record %d does not link to its predecessor, and every\n"+
			"  record after it is therefore unverifiable from this chain alone.\n"+
			"  Do not repair the chain. Snapshot the table, then follow\n"+
			"  docs/runbooks/security-credential-rotation.md's evidence-preservation steps.\n",
		broken, *from, broken-1, broken)
	return 1
}

// operatorTenantContext builds the tenant context the repositories read their scope from.
//
// The actor is recorded as the operator's own principal rather than as a service, because an
// audit-chain verification is a human action taken during an investigation and the audit trail of
// the investigation matters too.
func operatorTenantContext(ctx context.Context, tenantID shared.TenantID) (context.Context, error) {
	actor := firstNonEmpty(os.Getenv("PP_OPERATOR"), os.Getenv("USER"), "platformctl")
	return tenantctx.WithTenant(ctx, tenantctx.TenantContext{
		TenantID:    tenantID,
		Tier:        shared.TierPooled,
		Environment: shared.Environment(firstNonEmpty(os.Getenv("PP_ENVIRONMENT"), "sandbox")),
		Source:      tenantctx.SourceToken,
		Principal:   tenantctx.Principal{Type: tenantctx.PrincipalHuman, ID: actor, Name: actor},
	})
}

// truncate bounds a field so a table stays a table. An operator scanning a list needs the columns
// to line up more than they need the last forty characters of a stack trace.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
