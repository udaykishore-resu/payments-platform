// Package tenantctx carries the authenticated tenant, principal and correlation identity
// through a request, an event and a workflow activity, and provides the isolation guard.
//
// There is exactly one origin for tenant identity in this platform: the authenticated
// principal (baseline §16.2). Everything in this package exists to make that single origin
// survive every hop — an in-process function call, a gRPC call to the orchestrator, a Kafka
// envelope decoded by a consumer minutes later, a workflow activity resumed by a different
// pod days later — without any hop being able to introduce a *second* origin.
//
// Why a package rather than three struct fields threaded through every signature: because the
// interesting failure is not "we forgot to pass the tenant", which fails loudly, but "we
// passed a tenant that came from somewhere else", which does not fail at all. Confining the
// constructor to this package, refusing to derive a tenant from anything but an explicit
// restore, and returning an error rather than a zero value when the context is empty, together
// make the second failure expressible only by writing code that is obviously wrong.
//
// The package imports the standard library, the shared kernel and pkg/*. It deliberately knows
// nothing about HTTP, gRPC, Kafka or the workflow engine: those adapters call in here, never
// the reverse, so there is no transport whose quirks can leak into the isolation rule.
package tenantctx

import (
	"context"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
)

// PrincipalType names the class of identity that authenticated a request.
//
// It is recorded rather than inferred because several controls branch on it — dual control
// applies to humans, `svc:internal` is never tenant-scoped on its own, MFA freshness is
// meaningless for a machine client — and inferring the class from the shape of an ID at each
// of those call sites is how the classes drift apart.
type PrincipalType string

const (
	// PrincipalHuman is an operator or administrator authenticated through the corporate IdP
	// (security.md §3.2). Subject to MFA freshness, time-of-day and device-posture conditions.
	PrincipalHuman PrincipalType = "HUMAN"
	// PrincipalMachine is a tenant's own integration authenticated by OAuth2 client
	// credentials (security.md §3.1). Always bound to exactly one tenant.
	PrincipalMachine PrincipalType = "MACHINE"
	// PrincipalWorkload is one of our own services authenticated by mTLS with a SPIFFE ID
	// (security.md §3.5). Notably *not* tenant-scoped by its own identity, which is precisely
	// why a propagated tenant context is mandatory for it.
	PrincipalWorkload PrincipalType = "WORKLOAD"
)

// Principal is the minimal identity description the isolation layer needs.
//
// It is deliberately smaller than authn.Principal. This package is imported by persistence,
// events and the workflow engine; giving it the full authentication result would drag JWKS,
// certificates and API-client state into every one of those, and would tempt a caller into
// re-deciding an authentication question at a layer that has no business doing so.
type Principal struct {
	Type PrincipalType
	// ID is the stable subject identifier: `cli_…` for a machine client, the IdP subject for
	// a human, the SPIFFE ID for a workload.
	ID string
	// Name is the human-readable label used in audit records. It is display material only;
	// nothing branches on it.
	Name string
}

// Source records where a TenantContext was derived from.
//
// Recorded for audit, and load-bearing during an incident: "tenant B's data was read under
// tenant A's context" is a very different investigation depending on whether the context came
// from a token, an event envelope or a workflow row. Without this field the answer has to be
// reconstructed from surrounding log lines, which is guesswork.
type Source string

const (
	// SourceToken is the authenticated JWT or API client on a synchronous request.
	SourceToken Source = "TOKEN"
	// SourceEventEnvelope is the `tenantid` extension of a consumed CloudEvent.
	SourceEventEnvelope Source = "EVENT_ENVELOPE"
	// SourceWorkflowInstance is the tenant column on a leased workflow instance.
	SourceWorkflowInstance Source = "WORKFLOW_INSTANCE"
)

// IsValid reports whether s is one of the three sanctioned origins. Anything else is a bug in
// a caller, and is refused at construction rather than recorded.
func (s Source) IsValid() bool {
	return s == SourceToken || s == SourceEventEnvelope || s == SourceWorkflowInstance
}

// TenantContext is the identity every tenant-scoped operation runs under.
//
// It is a value, copied into the context, never a pointer: a pointer would let a downstream
// handler mutate the tenant of a request already in flight, which is the exact hazard this
// package exists to remove. The fields are exported so an adapter can build one at the
// boundary, and the constructor validates, so a TenantContext that exists is one whose tenant
// is well-formed and whose origin is known.
type TenantContext struct {
	// TenantID is the outermost isolation boundary. Everything else in this struct is
	// subordinate to it.
	TenantID shared.TenantID
	// Tier selects the isolation model (pooled vs siloed) and, downstream, which connection
	// pool, cache namespace and KMS key are used.
	Tier shared.TenantTier
	// Principal is who authenticated.
	Principal Principal
	// Scopes are the OAuth scopes granted to this principal, already normalized. Authorization
	// reads them; nothing else may.
	Scopes []string
	// MerchantScope optionally narrows a credential to a subset of the tenant's merchants —
	// the marketplace pattern where a tenant issues one credential per sub-merchant. Empty
	// means "every merchant in the tenant", which is why the ABAC condition must distinguish
	// empty from absent rather than treating the zero value as deny-all or allow-all by
	// accident.
	MerchantScope []shared.MerchantID
	// Environment must match the deployment's environment. Carrying it here as well as
	// checking it at token validation means an event replayed into the wrong environment is
	// caught at the consumer too, not only at the edge.
	Environment shared.Environment
	// RequestID is the per-request identifier echoed to the client as `X-Request-Id`.
	RequestID string
	// CorrelationID ties a whole causal chain together — the HTTP request, the events it
	// emitted, the workflow it started, the gateway calls that followed. It is the field that
	// makes a support question answerable.
	CorrelationID string
	// Source is where this context was derived from.
	Source Source
}

// Merchants returns a copy of the merchant scope. Returning the live slice would let a caller
// widen a credential's scope by appending to it — a mutation with no audit trail and no way to
// notice.
func (tc TenantContext) Merchants() []shared.MerchantID {
	return append([]shared.MerchantID(nil), tc.MerchantScope...)
}

// AllScopes returns a copy of the granted scopes, for the same reason as Merchants.
func (tc TenantContext) AllScopes() []string {
	return append([]string(nil), tc.Scopes...)
}

// CoversMerchant reports whether this principal may act on the given merchant.
//
// An empty MerchantScope means the whole tenant. That default is safe here only because the
// tenant boundary has already been enforced: "every merchant" means every merchant *of this
// tenant*, never every merchant on the platform.
func (tc TenantContext) CoversMerchant(m shared.MerchantID) bool {
	if len(tc.MerchantScope) == 0 {
		return true
	}
	for _, allowed := range tc.MerchantScope {
		if allowed == m {
			return true
		}
	}
	return false
}

// Validate checks the invariants a TenantContext must satisfy to be usable.
//
// It is called by WithTenant and by every restore path. The tenant prefix check is not
// decoration: an envelope or a workflow row carrying a merchant ID in the tenant column would
// otherwise silently produce a context that matches no rows and looks like a data-loss bug
// rather than a corruption bug.
func (tc TenantContext) Validate() error {
	if tc.TenantID.IsZero() {
		return apierror.New(apierror.CodeMissingTenantContext, "tenant context requires a tenant id")
	}
	if err := ids.Validate(tc.TenantID.String(), ids.PrefixTenant); err != nil {
		return apierror.Wrapf(err, apierror.CodeMissingTenantContext,
			"tenant context carries a malformed tenant id %q", tc.TenantID)
	}
	if !tc.Tier.IsValid() {
		return apierror.Newf(apierror.CodeMissingTenantContext, "unknown tenant tier %q", tc.Tier)
	}
	if !tc.Environment.IsValid() {
		return apierror.Newf(apierror.CodeMissingTenantContext, "unknown environment %q", tc.Environment)
	}
	if !tc.Source.IsValid() {
		return apierror.Newf(apierror.CodeMissingTenantContext, "unknown tenant context source %q", tc.Source)
	}
	return nil
}

// ctxKey is an unexported struct type so no other package can collide with it or fabricate a
// tenant context by writing to the same key with its own type.
type ctxKey struct{}

// WithTenant returns a context carrying tc.
//
// This is the only constructor, and it is called from exactly three places: the authentication
// middleware, the event consumer's envelope decoder, and the workflow lease-acquisition path.
// Anywhere else, the correct thing to pass down is the context you were given.
//
// It returns an error rather than accepting an invalid TenantContext because the alternative —
// a silently-defaulted or partially-populated context — produces queries that return zero rows
// and are diagnosed as missing data for hours before anyone suspects the tenant.
func WithTenant(ctx context.Context, tc TenantContext) (context.Context, error) {
	if err := tc.Validate(); err != nil {
		return ctx, err
	}
	// Copy the slices in. The caller may reuse or mutate the slice it passed; a tenant context
	// whose scope set changes after the authorization decision was taken is a vulnerability
	// with a very short window and no trace.
	tc.Scopes = append([]string(nil), tc.Scopes...)
	tc.MerchantScope = append([]shared.MerchantID(nil), tc.MerchantScope...)
	return context.WithValue(ctx, ctxKey{}, tc), nil
}

// FromContext returns the tenant context, or apierror.CodeMissingTenantContext if there is none.
//
// The error is deliberately not a sentinel bool. A caller that gets `(TenantContext{}, false)`
// has to remember to check; a caller that gets an error has to remember to ignore it, and
// ignoring an error is visible in review and to the linter. The distinction matters because
// the consequence of proceeding without a tenant is an unscoped query.
func FromContext(ctx context.Context) (TenantContext, error) {
	tc, ok := ctx.Value(ctxKey{}).(TenantContext)
	if !ok {
		return TenantContext{}, apierror.New(apierror.CodeMissingTenantContext,
			"tenant context was not established for this operation")
	}
	return tc, nil
}

// TenantID returns just the tenant identifier, for the many call sites that need nothing else.
func TenantID(ctx context.Context) (shared.TenantID, error) {
	tc, err := FromContext(ctx)
	if err != nil {
		return "", err
	}
	return tc.TenantID, nil
}

// MustTenantID returns the tenant identifier or the empty string.
//
// It exists for the two places where a missing tenant genuinely is not an error: a log or
// metric enrichment path that runs before authentication and must not fail the request in
// order to add a label. It must never be used to build a query, a cache key or a secret path —
// those take TenantID and handle the error, because an empty tenant there is the bug this
// package exists to prevent. The name is a warning, not a convenience.
func MustTenantID(ctx context.Context) shared.TenantID {
	tc, err := FromContext(ctx)
	if err != nil {
		return ""
	}
	return tc.TenantID
}

// AssertTenant is the isolation guard (baseline §16.2).
//
// Call it wherever a tenant identifier arrives from anywhere other than the authenticated
// principal — a row loaded by primary key, a payload field, a path parameter that was resolved
// before the tenant scope was applied — and before acting on it.
//
// # Tenant identity has exactly one origin
//
// The tenant comes from the authenticated principal and from nothing else. It is never read
// from a request body, a query string, a header, or a field of a resource the caller named.
// This is not a preference about API design; it is what makes the rest of the isolation stack
// meaningful. Row-level security scopes a transaction to `app.tenant_id`; if that value could
// originate in a request body, RLS would be enforcing the attacker's choice.
//
// # A disagreeing body tenant is a security event, not a validation error
//
// If a request body carries a `tenant_id` that disagrees with the token, the correct response
// is 403 TENANT_MISMATCH plus an audit record plus an alert — not 400 with a field detail. The
// distinction is deliberate and worth stating because the natural instinct is the other one:
//
//   - A validation error says "you made a mistake, here is how to fix it". It is expected
//     traffic, it is not alerted on, and it hands the caller a corrected-input oracle.
//   - A disagreeing tenant is not a mistake a legitimate integration makes. The field is
//     ignored on the success path, so no correct client ever populates it with a foreign value.
//     Reaching this branch means either a compromised credential being probed against other
//     tenants' identifiers, or a platform bug that has already lost the isolation property.
//     Both need a human, and neither needs the caller told which tenant would have been right.
//
// Consequently the error carries no detail about the expected tenant: telling a caller that
// their guess was wrong, and by implication that some other guess would be right, turns this
// guard into an enumeration oracle for tenant identifiers.
func AssertTenant(ctx context.Context, resourceTenantID shared.TenantID) error {
	tc, err := FromContext(ctx)
	if err != nil {
		return err
	}
	if resourceTenantID.IsZero() {
		// An unstamped resource is not "probably ours". Treating a missing tenant as a match
		// is how a migration that forgot to backfill a column becomes a cross-tenant read.
		return apierror.New(apierror.CodeTenantMismatch,
			"the requested resource belongs to a different tenant")
	}
	if resourceTenantID != tc.TenantID {
		return apierror.New(apierror.CodeTenantMismatch,
			"the requested resource belongs to a different tenant")
	}
	return nil
}

// AssertBodyTenant checks a tenant identifier that arrived in a request body or query string.
//
// The contract is exactly the one in baseline §16.2: an absent value is ignored, an agreeing
// value is ignored, and a disagreeing value is a security event. "Ignored" is literal — the
// value never becomes the tenant, even when it agrees, because accepting it in the agreeing
// case would create a code path where the body matters, and a code path that exists is a code
// path that can be reached with a different input.
//
// It takes a string rather than a shared.TenantID so that a caller cannot get here having
// already parsed and half-trusted the value.
func AssertBodyTenant(ctx context.Context, claimed string) error {
	if claimed == "" {
		return nil
	}
	return AssertTenant(ctx, shared.TenantID(claimed))
}

// Detached returns a context that keeps the tenant, principal and correlation values but drops
// cancellation and deadlines.
//
// # The only sanctioned use
//
// A background flush that must complete after the request that produced it has ended: writing
// an audit record, emitting a security event, draining a metrics buffer. Nothing else. In
// particular it is not for "this call is slow so let us escape the deadline", and it is not for
// starting a worker.
//
// The rule exists because the two obvious alternatives are both wrong in ways that are hard to
// see in review:
//
//   - `context.Background()` in a goroutine drops the tenant along with the deadline. The
//     background write then runs with no tenant, the repository refuses it, and the audit
//     record is silently missing — discovered during an investigation, when it is needed.
//   - Passing the live request context means the work is cancelled the instant the client
//     disconnects, which is precisely when an audit record is most interesting.
//
// Every call site must be paired with a bound: a worker pool, a timeout of its own, or a
// shutdown barrier. `context.WithoutCancel` removes the deadline; it does not create a budget,
// and unbounded detached work is a goroutine leak that only manifests under load.
func Detached(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: FR-06, NFR-29.
//
// Tenant resolution and the assertion that every path carries one; the tenant comes from the
// credential and from nowhere else
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
