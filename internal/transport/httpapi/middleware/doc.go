// Package middleware is the data-plane request pipeline of docs/spec/00-design-baseline.md §12,
// expressed as ordinary `func(http.Handler) http.Handler` decorators.
//
// # The order is the design
//
// §12 states the pipeline as a numbered table and says the order is load-bearing. It is, and
// each adjacency is a decision with a failure mode behind it:
//
//	recover          → nothing below it may take the process down, including the panic that
//	                   happens while rendering another panic.
//	requestid        → every line the rest of the chain writes needs a correlation id, so it
//	                   must exist before the first log line, not after authentication.
//	tracing          → the span must enclose authentication, because "the 401s are slow" is a
//	                   question you can only answer if the failing requests have spans.
//	logging          → wraps the *outcome*, so it observes the status every later stage
//	                   produces, including the 429 the rate limiter writes.
//	metrics          → same, and separate from logging because logs are sampled and metrics
//	                   are not; a shared implementation eventually samples the metric.
//	bodylimit        → the byte ceiling has to be in force before anything reads the body,
//	                   and in particular before authentication reads a form-encoded credential.
//	contenttype      → 415 is cheaper than a decode failure and names a different fix.
//	cors             → the preflight answer must precede authentication: a browser preflight
//	                   carries no credentials by design, so authenticating it returns 401 to a
//	                   request that was only asking whether it may send the real one.
//	securityheaders  → set on every response including the ones later stages reject, which is
//	                   why it is above authentication and not in the handler.
//	authn            → §12 stage 3.
//	tenant           → §12 stage 4. Strictly after authn: the tenant comes from the token.
//	authz            → §12 stage 5. Strictly after tenant: an ABAC rule compares the resource's
//	                   tenant to the principal's.
//	ratelimit        → §12 stage 6. After authz because the limit is per tenant and per
//	                   merchant, and before the handler because the point is to not do work.
//	concurrency      → adaptive limit plus shedding by priority class. Below the rate limiter
//	                   because a rate limit is a contract and shedding is an emergency: an
//	                   in-contract request should be rejected by load, not by quota.
//	idempotency      → last, and therefore innermost. The claim must be the final thing that
//	                   happens before the handler runs, so that a request rejected by any
//	                   earlier stage never consumes a key. A key burned by a 401 is a key the
//	                   client must not reuse and cannot tell apart from one that did work.
//
// # Latency budgets
//
// Each middleware's doc comment states its §12 budget. The budgets sum to the p99 SLO minus
// gateway time; a middleware that exceeds its budget is a defect even when the request
// succeeds, because the budget is what makes the SLO arithmetic hold.
//
// # What a middleware may write
//
// Errors are rendered through [httpapi.WriteProblem] and nothing else. A middleware never
// writes an ad-hoc body: a 401 whose body is `plain text` breaks every SDK that parses
// problem+json, and the breakage appears only for the error paths nobody tests.
package middleware
