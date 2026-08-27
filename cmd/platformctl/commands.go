// The operator sub-commands are one file per command family — seed.go, config.go, certify.go,
// drdrill.go, outbox.go, workflow.go — so that a reader looking for `outbox status` opens
// outbox.go rather than scrolling. This file holds only what they share.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
)

// openPool resolves the DSN and opens a connection pool.
//
// Both `PP_DSN` and `DATABASE_URL` are accepted, in that order: the former is the platform's own
// name and the latter is what every hosting environment and every operator already has exported.
// Accepting both costs three lines and removes the most common reason this tool fails to run.
func openPool(ctx context.Context, stderr *os.File) (*postgres.Pool, int) {
	dsn := firstNonEmpty(os.Getenv("PP_DSN"), os.Getenv("DATABASE_URL"))
	if dsn == "" {
		fmt.Fprintln(stderr,
			"no database connection string: set PP_DSN or DATABASE_URL")
		return nil, 2
	}
	// A small pool: this is a CLI, not a server, and a tool that opens twenty connections during
	// an incident is a tool competing with the fleet for the writer it is trying to inspect.
	cfg := postgres.DefaultPoolConfig(dsn, "platformctl")
	cfg.MaxConns = 2
	cfg.MinConns = 1

	pool, err := postgres.NewPool(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "could not connect: %v\n", err)
		return nil, 2
	}
	// The repositories fail closed without a tenant resolver, and this CLI reaches them for the
	// audit-chain verification and the seed's configuration publish. Installing it here mirrors
	// what runtime.OpenPostgres does for every server binary, for the same reason: a database
	// connection that is not usable by the repositories is a connection that produces
	// MISSING_TENANT_CONTEXT at the first call, from a request that plainly carried a tenant.
	postgres.UseTenantResolver(func(ctx context.Context) (string, bool) {
		t, err := tenantctx.TenantID(ctx)
		if err != nil || t == "" {
			return "", false
		}
		return t.String(), true
	})
	return pool, 0
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
