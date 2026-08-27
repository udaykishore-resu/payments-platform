package middleware

import (
	"net/http"
	"net/netip"
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authn"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authz"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Authenticate resolves the caller's identity.
//
// Budget: 2 ms (§12 stage 3) — the JWKS is cached with background refresh, so the common path is
// a signature verification and nothing else.
// Fails with: 401 UNAUTHENTICATED / INVALID_TOKEN / TOKEN_EXPIRED.
//
// # Failing closed on a missing authenticator
//
// A nil Authenticator rejects every non-anonymous request rather than passing them through. The
// alternative — skip the stage when unconfigured — makes a wiring omission in a composition root
// into a public payments API with no authentication, and it passes every smoke test because
// every smoke test succeeds.
//
// # The anonymous allowlist
//
// Probes and the gateway webhook ingress bypass this stage. That is an explicit set of route
// templates, not a path prefix: a prefix rule is one careless registration away from exposing a
// resource, whereas an allowlist requires somebody to type the route in.
//
// The webhook route is on the list because its caller *is* the gateway, which holds no platform
// credential. It is not unauthenticated — the handler verifies the gateway's own signature over
// the raw body before it does anything with the content — the authentication simply happens
// somewhere this middleware cannot do it.
func Authenticate(a Authenticator, anonymous map[string]bool) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if anonymous[httpapi.RouteTemplate(r.Context())] {
				next.ServeHTTP(w, r)
				return
			}
			if a == nil {
				httpapi.WriteProblem(w, r, apierror.New(apierror.CodeUnauthenticated,
					"this endpoint requires authentication and no authenticator is configured"))
				return
			}
			p, err := a.Authenticate(r.Context(), r)
			if err != nil {
				httpapi.WriteProblem(w, r, err)
				return
			}
			if p == nil {
				httpapi.WriteProblem(w, r, apierror.New(apierror.CodeUnauthenticated,
					"the request carried no usable credential"))
				return
			}
			next.ServeHTTP(w, r.WithContext(httpapi.WithPrincipal(r.Context(), p)))
		})
	}
}

// Tenant derives the tenant context from the authenticated principal and enforces the isolation
// guard.
//
// Budget: 1 ms (§12 stage 4). Fails with: 403 TENANT_MISMATCH, 401 MISSING_TENANT_CONTEXT.
//
// # The tenant comes from the token, never from the path or the body
//
// Baseline §16.2. A request that carries `tenantId` anywhere a caller controls is a request that
// can name a tenant it does not own, and the failure is silent: the query runs, the row-level
// security policy is set to the attacker's chosen tenant, and the data is returned. So the
// tenant is read from the verified principal and written onto the context, and the persistence
// layer takes it from there and nowhere else.
//
// # The merchant isolation guard
//
// A merchant-scoped principal — the common case for a merchant's own API client — carries an
// explicit merchant allowlist. When the route has a `{merchantId}` path parameter, this stage
// checks it against that list *before* the handler runs, so a handler cannot forget. The
// rejection is 403 TENANT_MISMATCH rather than 404: within a tenant the merchant's existence is
// not a secret, and returning 404 for a merchant the caller simply lacks scope for sends an
// integrator looking for a data problem that does not exist.
//
// Cross-*tenant* access is the opposite: the repository returns 404 there, deliberately, because
// distinguishing "not yours" from "does not exist" leaks the existence of another tenant's
// identifiers to anyone who can guess one.
func Tenant(anonymous map[string]bool) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if anonymous[httpapi.RouteTemplate(r.Context())] {
				next.ServeHTTP(w, r)
				return
			}
			p := httpapi.Principal(r.Context())
			if p == nil {
				httpapi.WriteProblem(w, r, apierror.New(apierror.CodeMissingTenantContext,
					"no authenticated principal from which to derive a tenant"))
				return
			}
			tc, err := p.TenantContext(httpapi.RequestID(r.Context()), httpapi.CorrelationID(r.Context()))
			if err != nil {
				httpapi.WriteProblem(w, r, err)
				return
			}
			ctx, err := tenantctx.WithTenant(r.Context(), tc)
			if err != nil {
				httpapi.WriteProblem(w, r, err)
				return
			}

			// The isolation guard. r.PathValue is empty until the mux has matched, which has not
			// happened yet — so the value is parsed out of the path against the template the
			// tracing stage already resolved. That is why the template is on the context.
			if mid := merchantFromPath(httpapi.RouteTemplate(ctx), r.URL.EscapedPath()); mid != "" {
				if !tc.CoversMerchant(shared.MerchantID(mid)) {
					httpapi.WriteProblem(w, r, apierror.New(apierror.CodeTenantMismatch,
						"the authenticated principal is not scoped to this merchant").
						WithDetail(apierror.Detail{
							Field:   "merchantId",
							Code:    "MERCHANT_OUT_OF_SCOPE",
							Message: "Request a token whose merchant_scope includes this merchant.",
							RuleID:  "L5.TENANT_ISOLATION",
						}))
					return
				}
			}

			ctx = telemetry.ContextWithFields(ctx, telemetry.Fields{
				TenantID:      tc.TenantID.String(),
				TenantTier:    telemetry.TenantTier(strings.ToLower(string(tc.Tier))),
				RequestID:     httpapi.RequestID(ctx),
				CorrelationID: httpapi.CorrelationID(ctx),
			})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// merchantFromPath extracts the {merchantId} segment by position.
//
// Positional rather than regular-expression matching: the template and the path have the same
// segment count by construction (the template is what the mux matched, or would match), so the
// index of `{merchantId}` in one is the index of the value in the other. A regex over the raw
// path would also match `mrc_…` appearing in a query string or in a different parameter, which
// is how a guard ends up rejecting a legitimate request for a reason nobody can reproduce.
func merchantFromPath(template, path string) string {
	if !strings.Contains(template, "{merchantId}") {
		return ""
	}
	tp := strings.Split(strings.Trim(template, "/"), "/")
	pp := strings.Split(strings.Trim(path, "/"), "/")
	if len(tp) != len(pp) {
		return ""
	}
	for i, seg := range tp {
		if seg == "{merchantId}" {
			return pp[i]
		}
	}
	return ""
}

// Authorize evaluates RBAC and ABAC for the matched route.
//
// Budget: 2 ms (§12 stage 5). Fails with: 403 FORBIDDEN / INSUFFICIENT_SCOPE /
// DUAL_CONTROL_REQUIRED / RESIDENCY_POLICY_VIOLATION.
//
// # Why the permission comes from a table rather than the handler
//
// A handler that checks its own scope is a handler that can forget, and the forgetting is
// invisible: the endpoint works, returns data, and passes its tests. Deriving the permission
// from the (method, template) pair means a new route with no table entry is *denied*, and
// [TestEveryRouteHasAPermission] turns that into a failing test rather than a production 403.
//
// A nil Authorizer denies everything, for the same fail-closed reason as Authenticate.
func Authorize(a Authorizer, anonymous map[string]bool) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			template := httpapi.RouteTemplate(r.Context())
			if anonymous[template] {
				next.ServeHTTP(w, r)
				return
			}
			perm, ok := PermissionFor(r.Method, template)
			if !ok {
				httpapi.WriteProblem(w, r, apierror.Newf(apierror.CodeForbidden,
					"no authorization policy is defined for %s %s", r.Method, template))
				return
			}
			if a == nil {
				httpapi.WriteProblem(w, r, apierror.New(apierror.CodeForbidden,
					"no authorization policy engine is configured"))
				return
			}
			p := httpapi.Principal(r.Context())
			tc, _ := tenantctx.FromContext(r.Context())

			req := authz.Request{
				Principal:  p,
				Permission: perm,
				Operation:  r.Method + " " + template,
				Resource: authz.Resource{
					TenantID: tc.TenantID,
					// The environment comes from the tenant context, which took it from the
					// verified `env` claim. It is not decoration: authz.EnvironmentMatch requires
					// the principal's environment, the resource's environment and the
					// deployment's to be the same value, and an unset resource environment fails
					// that check — which denies every authenticated request on every route with
					// a bare 403. Setting it here is what makes the sandbox/production separation
					// an assertion rather than an unsatisfiable condition.
					Environment: tc.Environment,
					// The merchant is knowable only where the route carries it in the path. For
					// the payment routes it does not — `POST /v1/payments` names its merchant in
					// the body, and the rest are addressed by payment id — and authorization runs
					// before the body is parsed on purpose, so an unauthorized caller cannot make
					// the server decode attacker-controlled input. The merchant-scoped grant and
					// the merchant-state condition handle the empty case explicitly; see
					// authz.MerchantState.
					MerchantID: shared.MerchantID(merchantFromPath(template, r.URL.EscapedPath())),
				},
				SourceIP:       sourceIP(r),
				PeerThumbprint: authn.PeerThumbprint(r.Context()),
				// ApprovalRef carries the dual-control approval a caller obtained out of band.
				// It is a header rather than a body field so that the policy engine can see it
				// without parsing a body whose shape differs per endpoint.
				ApprovalRef: r.Header.Get(HeaderApprovalRef),
			}
			if d := a.Evaluate(r.Context(), req); d.Denied() {
				httpapi.WriteProblem(w, r, d.Error())
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// HeaderApprovalRef carries the reference to a dual-control approval obtained out of band.
//
// It is a request header rather than a body field because the policy engine evaluates before the
// body is parsed, and because the same reference applies to endpoints whose bodies have nothing
// else in common.
const HeaderApprovalRef = "X-Approval-Ref"

// sourceIP resolves the caller's address for the ABAC source constraint.
//
// It reads RemoteAddr and deliberately does *not* trust X-Forwarded-For. A forwarded header is
// caller-controlled unless every hop between the client and here is trusted and rewrites it, and
// on this deployment the load balancer terminates into a mesh sidecar whose RemoteAddr is the
// real peer. Trusting the header instead would let a caller assert any source address it liked,
// which turns an IP allowlist into decoration.
func sourceIP(r *http.Request) netip.Addr {
	host := r.RemoteAddr
	if h, _, err := splitHostPort(host); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr
}

func splitHostPort(v string) (string, string, error) {
	i := strings.LastIndexByte(v, ':')
	if i < 0 {
		return v, "", errNoPort
	}
	// An IPv6 literal without brackets has several colons and no port.
	if strings.Count(v, ":") > 1 && !strings.HasPrefix(v, "[") {
		return v, "", errNoPort
	}
	return strings.Trim(v[:i], "[]"), v[i+1:], nil
}

var errNoPort = apierror.New(apierror.CodeInternalError, "address carries no port")
