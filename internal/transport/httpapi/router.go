package httpapi

import (
	"net/http"
	"sort"
	"strings"
)

// The route templates, in one place.
//
// They are constants because three subsystems consume them and must agree exactly: the mux
// (which matches on them), the metrics middleware (which uses them as a bounded label), and the
// idempotency scope (which includes the *template* in the key, so that the same client key on
// two different endpoints is two different operations). A literal repeated in three files is a
// label mismatch waiting for a release.
const (
	RouteCreateMerchant     = "/v1/merchants"
	RouteListMerchants      = "/v1/merchants"
	RouteGetMerchant        = "/v1/merchants/{merchantId}"
	RouteUpdateMerchant     = "/v1/merchants/{merchantId}"
	RouteStartOnboarding    = "/v1/merchants/{merchantId}/onboarding"
	RouteGetOnboarding      = "/v1/merchants/{merchantId}/onboarding"
	RouteOnboardingSignal   = "/v1/merchants/{merchantId}/onboarding/signals/{signal}"
	RouteGetConfiguration   = "/v1/merchants/{merchantId}/configuration"
	RoutePutConfiguration   = "/v1/merchants/{merchantId}/configuration"
	RouteListConfigVersions = "/v1/merchants/{merchantId}/configuration/versions"
	RouteRollbackConfig     = "/v1/merchants/{merchantId}/configuration/rollback"
	RouteListGateways       = "/v1/gateways"
	RouteGetGateway         = "/v1/gateways/{gatewayId}"
	RouteGatewayHealth      = "/v1/gateways/{gatewayId}/health"
	RouteRotateCredentials  = "/v1/gateways/{gatewayId}/credentials:rotate"
	RouteCreatePayment      = "/v1/payments"
	RouteListPayments       = "/v1/payments"
	RouteGetPayment         = "/v1/payments/{paymentId}"
	RouteCapturePayment     = "/v1/payments/{paymentId}/capture"
	RouteRefundPayment      = "/v1/payments/{paymentId}/refund"
	RouteVoidPayment        = "/v1/payments/{paymentId}/void"
	RouteReceiveWebhook     = "/v1/webhooks/{gateway}"
	RouteHealthz            = "/healthz"
	RouteReadyz             = "/readyz"
	RouteLivez              = "/livez"
	RouteMetrics            = "/metrics"
)

// Handler is a handler that reports failure by returning it.
//
// Returning the error rather than rendering it is what keeps [WriteProblem] the single writer
// of error bytes, and it is what lets the handlers package stay free of any dependency on
// problem rendering — which is what breaks the import cycle described in the package comment.
// The practical benefit is smaller: a handler that forgets to `return` after writing an error
// is a compile error rather than a double-write.
type Handler func(http.ResponseWriter, *http.Request) error

// Route is one registered endpoint.
type Route struct {
	Method   string
	Template string
	// OperationID is the OpenAPI operationId this route implements. It is recorded so the
	// contract test can assert the mapping in both directions — every declared operation has a
	// route, and every route implements a declared operation — rather than only the easy one.
	OperationID string
}

// Router is the platform's route table and mux.
//
// It wraps http.ServeMux rather than replacing it because Go 1.22's method-and-wildcard
// patterns are exactly the routing this surface needs, and a third-party router would be a
// dependency whose only contribution is a different syntax for the same table. What the wrapper
// adds is the route *template* on the request context — which the mux knows and does not
// expose to middleware running outside it — and a machine-readable route list for the contract
// test.
type Router struct {
	mux    *http.ServeMux
	routes []Route
}

// NewRouter returns an empty router. Resources register themselves into it; see
// handlers.Register.
func NewRouter() *Router {
	return &Router{mux: http.NewServeMux()}
}

// Handle registers h for method and template.
//
// It panics on a duplicate registration, and that is the one panic this package permits: a
// duplicate route is a programming error detectable at startup, before the listener binds, and
// a process that silently serves one of two conflicting handlers is worse than one that refuses
// to start.
func (rt *Router) Handle(method, template, operationID string, h Handler) {
	pattern := method + " " + template
	rt.mux.Handle(pattern, rt.wrap(template, h))
	rt.routes = append(rt.routes, Route{Method: method, Template: template, OperationID: operationID})
}

// HandleRaw registers a plain http.Handler, for the endpoints whose bodies are not ours to
// shape: the Prometheus exposition handler and the probe handlers.
func (rt *Router) HandleRaw(method, template, operationID string, h http.Handler) {
	rt.mux.Handle(method+" "+template, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(WithRouteTemplate(r.Context(), template)))
	}))
	rt.routes = append(rt.routes, Route{Method: method, Template: template, OperationID: operationID})
}

func (rt *Router) wrap(template string, h Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(WithRouteTemplate(r.Context(), template))
		if err := h(w, r); err != nil {
			WriteProblem(w, r, err)
		}
	})
}

// ServeHTTP dispatches to the registered handler.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) { rt.mux.ServeHTTP(w, r) }

// Routes returns the registered route table, sorted, for tests and for the startup log line
// that records what this binary actually serves.
func (rt *Router) Routes() []Route {
	out := append([]Route(nil), rt.routes...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Template != out[j].Template {
			return out[i].Template < out[j].Template
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// Has reports whether a route is registered for method and template.
func (rt *Router) Has(method, template string) bool {
	for _, r := range rt.routes {
		if r.Method == method && r.Template == template {
			return true
		}
	}
	return false
}

// Resolve returns the route template a request would match, without dispatching it.
//
// This is what lets the middleware chain — which runs *outside* the mux and therefore before
// matching — name a span and label a metric with the template rather than the raw path. The
// alternative, wrapping each handler individually, would leave 404s and requests rejected by
// authentication outside the chain entirely, which is where the interesting attacks are.
func (rt *Router) Resolve(r *http.Request) string {
	_, pattern := rt.mux.Handler(r)
	if pattern == "" {
		return "unmatched"
	}
	// The mux returns the full pattern, "POST /v1/payments"; the template is the second half.
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		return pattern[i+1:]
	}
	return pattern
}
