package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
)

// runCertify runs the certification suite against a merchant's gateway connections.
func runCertify(ctx context.Context, args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("certify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	env := fs.String("environment", "sandbox", "sandbox or production")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "certify requires exactly one merchant id")
		return 2
	}
	merchantID, err := shared.ParseMerchantID(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 2
	}
	if *env == "production" {
		// Certification moves real money on a real account. It is legitimate — a production
		// certification is how a merchant is proven live — but it must be a decision, not a
		// default, which is why the flag defaults to sandbox and this line exists.
		fmt.Fprintln(stdout,
			"warning: production certification executes real authorizations and refunds")
	}
	fmt.Fprintf(stdout, "certifying %s in %s\n\n", merchantID, *env)

	pool, code := openPool(ctx, stderr)
	if pool == nil {
		return code
	}
	defer func() { _ = pool.Close(context.Background()) }()

	// What this command can do offline is report the certification *state* of every connection,
	// which is the question an operator actually asks first ("is this merchant certified, and if
	// not, where did it stop?"). What it cannot do is run the suite.
	rows, err := postgres.NewOperatorReports(pool).MerchantConnections(ctx, merchantID.String(), *env)
	if err != nil {
		fmt.Fprintf(stderr, "could not read the merchant's connections: %v\n", err)
		return 1
	}
	if len(rows) == 0 {
		fmt.Fprintf(stderr, "merchant %s has no %s connections; there is nothing to certify\n",
			merchantID, *env)
		return 1
	}
	fmt.Fprintln(stdout, "current connection state:")
	for _, c := range rows {
		fmt.Fprintf(stdout, "  %-16s status=%-14s certification=%-12s credential=%s\n",
			c.GatewayID, c.Status, c.CertificationStatus, c.CredentialRef)
	}

	// The honest exit. Certification is a live sequence of authorize/capture/refund/void calls
	// against the vendor's sandbox with a real credential — it is not a database operation, and a
	// command that printed "certified" without making those calls would be worse than one that
	// refuses, because a merchant would be marked live on the strength of it.
	return reportBlocked(stderr, "certify",
		"a live gateway credential and outbound network access to the vendor's sandbox: the "+
			"suite runs authorize → capture → refund → void per connection and asserts the "+
			"normalized outcomes",
		"platformctl certify runs the same steps the onboarding workflow's certification stage "+
			"runs, so trigger it there instead:\n"+
			"    POST /v1/merchants/"+merchantID.String()+"/certification\n"+
			"  and follow the workflow with:\n"+
			"    platformctl workflow list --state RUNNING")
}
