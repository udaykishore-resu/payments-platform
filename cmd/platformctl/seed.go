package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
	"github.com/udaykishore-resu/payments-platform/internal/platform/runtime"
)

// runSeed generates a deterministic synthetic dataset.
//
// # It refuses production and there is no override
//
// deployment.md §6.1: non-production data is generated, production data is never generated.
// Anonymising a relational payment dataset is not reliably achievable — merchant names,
// bank-account fragments, amounts, timestamps and gateway references re-identify in combination —
// so there is no import path here and no `--from-dump`. Writing synthetic merchants and payments
// into tables that hold money is not something a flag should be able to authorise.
//
// # Determinism
//
// The same profile, scale and seed produce byte-identical data. That is what lets a test assert on
// a specific merchant's configuration without first querying for it, and what makes "reproduce it
// locally" a real instruction.
func runSeed(ctx context.Context, args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := fs.String("profile", "dev", "dev, integration, load, e2e or minimal")
	scale := fs.Int("scale", 25, "multiplier for the profile's base counts")
	seed := fs.Int64("seed", 1724680000000000000, "PRNG seed; the default is fixed so runs reproduce")
	reset := fs.Bool("reset", false, "truncate the seeded tables first")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch *profile {
	case "dev", "integration", "load", "e2e", "minimal":
	default:
		fmt.Fprintf(stderr, "unknown profile %q\n", *profile)
		return 2
	}
	if env := os.Getenv("PP_ENVIRONMENT"); env == "prod" || env == "production" {
		fmt.Fprintln(stderr,
			"refusing to seed production, and there is no override.\n\n"+
				"Seed data is synthetic data written into merchant, payment and ledger tables.\n"+
				"In production those tables hold money. If you need production-shaped data to\n"+
				"reproduce something, build a synthetic case from the structure of the production\n"+
				"case — states, amounts, currencies, gateway, timing — taken from traces and\n"+
				"metrics (deployment.md §6.1).")
		return 2
	}

	environment := firstNonEmpty(os.Getenv("PP_ENVIRONMENT"), "sandbox")
	env, err := shared.ParseEnvironment(environment)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 2
	}

	pool, code := openPool(ctx, stderr)
	if pool == nil {
		return code
	}
	defer func() { _ = pool.Close(context.Background()) }()

	fmt.Fprintf(stdout, "seeding profile=%s scale=%d seed=%d reset=%v environment=%s\n",
		*profile, *scale, *seed, *reset, env)
	if *reset {
		// Stated before it happens, per this tool's rule that every command that writes says what
		// it will do first. A reset is the one destructive thing seed does.
		fmt.Fprintf(stdout, "  --reset will TRUNCATE: %s\n", strings.Join(postgres.SeededTables(), ", "))
	}

	res, err := postgres.NewSeeder(pool, shared.SystemClock{}).Seed(ctx, postgres.SeedOptions{
		Profile:     postgres.SeedProfile(*profile),
		Scale:       *scale,
		Seed:        *seed,
		Environment: env,
		Gateways:    runtime.SplitList(firstNonEmpty(os.Getenv("PP_SEED_GATEWAYS"), "simulator")),
		// The endpoint the seeded catalogue rows point at. It has to be a real address for the
		// dataset to be usable: a gateway row with no base URL produces adapters that refuse at
		// dispatch, so the stack comes up healthy and fails every payment.
		GatewayBaseURL: firstNonEmpty(os.Getenv("PP_SEED_GATEWAY_URL"), "http://127.0.0.1:9090"),
		Reset:          *reset,
	})
	if err != nil {
		fmt.Fprintf(stderr, "seeding failed: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "\ntenant   %s\n", res.TenantID)
	fmt.Fprintf(stdout, "gateways %s\n", strings.Join(res.Gateways, ", "))
	fmt.Fprintf(stdout, "merchants (%d):\n", len(res.MerchantIDs))
	for i, m := range res.MerchantIDs {
		if i == shownMerchants {
			fmt.Fprintf(stdout, "  … and %d more\n", len(res.MerchantIDs)-shownMerchants)
			break
		}
		fmt.Fprintf(stdout, "  %s\n", m)
	}
	fmt.Fprintln(stdout, "\nrow counts:")
	for _, t := range sortedKeys(res.Counts) {
		fmt.Fprintf(stdout, "  %-24s %d\n", t, res.Counts[t])
	}

	// The credential references the seeded connections point at. They are printed because a
	// seeded database whose credentials nothing can resolve produces payments that all fail on
	// credential resolution — and the fix is to put these references in the local secrets
	// document, which the operator can only do if they know what they are.
	fmt.Fprintln(stdout, "\ncredential references to provide in PP_SECRETS_FILE:")
	for _, k := range sortedKeys(res.SecretRefs) {
		fmt.Fprintf(stdout, "  %s\n", res.SecretRefs[k])
	}
	return 0
}

// shownMerchants bounds the merchant list in the output. A seed at load scale writes hundreds,
// and a command whose output scrolls past the interesting part is a command nobody reads.
const shownMerchants = 10

// sortedKeys gives map output a stable order, so two runs of the same command diff cleanly.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
