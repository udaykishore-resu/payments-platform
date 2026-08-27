package postgres

import (
	"context"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// TenantResolver reads the current tenant out of a request context.
//
// This package deliberately does not import internal/platform/tenantctx. Two reasons, and the
// second is the one that matters:
//
//   - The persistence layer needs exactly one fact about the tenant — its identifier — and
//     importing a package to obtain one string couples the storage adapter to the shape of an
//     unrelated platform type.
//   - More importantly, the context key must have exactly one owner. If this package could
//     construct a tenant context, it would become a second place from which a tenant identity
//     can enter the system, and "the tenant comes from the authentication middleware, the event
//     envelope decoder, or the workflow lease — and from nowhere else" would stop being true.
//     A function variable can only *read*.
//
// main wires the real resolver at startup with UseTenantResolver. Until it does, the default
// resolver reports no tenant and every repository method fails closed with
// apierror.CodeMissingTenantContext — which is the correct behaviour for a misconfigured binary, and
// is loudly wrong rather than quietly permissive.
type TenantResolver func(ctx context.Context) (tenantID string, ok bool)

// tenantFrom is the resolver in force. It is a package-level variable rather than a field on
// each repository because it is a property of the process, set once at wiring time, and
// threading it through fourteen constructors would obscure that.
var tenantFrom TenantResolver = func(context.Context) (string, bool) { return "", false }

// UseTenantResolver installs the process's tenant resolver. It is called once, from main,
// immediately after the context package is initialised and before any pool is opened.
//
// Passing nil restores the fail-closed default rather than panicking: a nil resolver would
// otherwise crash on the first query, in a request path, which is the worst possible place to
// discover a wiring mistake.
func UseTenantResolver(r TenantResolver) {
	if r == nil {
		tenantFrom = func(context.Context) (string, bool) { return "", false }
		return
	}
	tenantFrom = r
}

// tenantOf returns the context's tenant or the error that must be returned *without querying*.
//
// Returning an error rather than an empty string is the whole point (baseline §16.2, R-TX-5).
// An empty tenant reaching a query would be filtered to zero rows by the RLS policy — which is
// correct but indistinguishable from "no such row", so the caller would report a 404 for what is
// actually a missing-authentication bug, and it would take an incident to notice.
func tenantOf(ctx context.Context) (shared.TenantID, error) {
	id, ok := tenantFrom(ctx)
	if !ok || id == "" {
		return "", apierror.New(apierror.CodeMissingTenantContext,
			"no tenant in context; the persistence layer refuses to query without one")
	}
	return shared.TenantID(id), nil
}

// requireTenantCtx is the guard every repository method calls before it issues a statement.
//
// Two checks, in this order and for two different reasons:
//
//  1. There must be a tenant in the context at all. Without it the method returns
//     CodeMissingTenantContext *without querying* (R-TX-5, baseline §16.2). Querying anyway would be
//     "safe" — the RLS policy evaluates an unset GUC to NULL and returns zero rows — but the
//     caller would then report a 404 for what is actually a missing-authentication bug, and it
//     would take an incident to notice.
//  2. It must be the same tenant the transaction was opened for. A context that changed tenant
//     between BEGIN and the query means a context crossed a transaction boundary somewhere; the
//     statement would run under the transaction's GUC while the caller believed they were
//     reading someone else's data, and the result would be silently wrong rather than empty.
func requireTenantCtx(ctx context.Context, bound shared.TenantID) error {
	t, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	if t != bound {
		return apierror.Newf(apierror.CodeTenantMismatch,
			"postgres: context tenant %s differs from the transaction's tenant %s", t, bound)
	}
	return nil
}

// requireOwner is requireTenantCtx plus a check that the aggregate being written belongs to the
// transaction's tenant.
//
// The RLS policy's WITH CHECK clause would refuse a cross-tenant write anyway, and that is the
// control. This exists to produce a TENANT_MISMATCH with a usable message rather than a bare
// policy violation, and to fail *before* the statement — which matters when the caller is a
// batch loop and the difference is one rejected row versus an aborted transaction.
func requireOwner(ctx context.Context, bound, owner shared.TenantID) error {
	if err := requireTenantCtx(ctx, bound); err != nil {
		return err
	}
	if owner != bound {
		return apierror.Newf(apierror.CodeTenantMismatch,
			"postgres: refusing to write an aggregate owned by tenant %s under tenant %s",
			owner, bound)
	}
	return nil
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: FR-06, NFR-29.
//
// The database half of tenant isolation: the per-transaction tenant setting that row-level
// security reads, so a missing guard returns nothing rather than another tenant's rows
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
