// Package postgres implements the application's persistence ports against PostgreSQL 15+.
//
// It is an adapter, in the hexagonal sense: nothing here is imported by the domain or the
// application layer, and every type it exports exists to satisfy an interface declared in
// internal/application/ports. If a change to this package requires a change to a domain type,
// the arrow is pointing the wrong way and one of the two is wrong.
//
// # Three rules that hold everywhere in this package
//
// First, **every query is tenant-scoped, and the scope comes from the database, not from the
// query text.** A repository method reads the tenant from context and refuses — with
// apierror.CodeMissingTenantContext, before issuing any statement — if there is none. The
// transaction then executes `SELECT set_config('app.tenant_id', $1, true)` and PostgreSQL's
// row-level security does the enforcing. The `WHERE tenant_id = $n` predicates that appear in
// the SQL below are an *optimization* so the planner can use a tenant-leading index; they are
// not the control. Treating them as the control is how a single forgotten predicate in one
// method out of forty becomes a cross-tenant read.
//
// Second, **no SELECT *, anywhere.** Every column is named. The cost is verbose SQL; the benefit
// is that adding a column to a table cannot silently shift a positional Scan and start writing
// the gateway reference into the decline reason.
//
// Third, **no string concatenation into SQL, including for dynamic filters.** The filter builder
// in sqlbuild.go appends parameter placeholders and collects arguments; there is no code path in
// this package where a caller-supplied value reaches the statement text.
//
// # What is deliberately not here
//
// No caching, no retry loop, and no circuit breaker. Retrying a money command is the caller's
// decision, gated by the idempotency record (R-CC-5): a blind in-transaction retry can re-apply
// an effect that already happened. Errors are classified — errors.go maps SQLSTATEs onto
// *apierror.Error with an accurate Retryable bit — and the decision about what to do with a
// retryable error is made a layer up, by code that knows whether the operation was idempotent.
package postgres
