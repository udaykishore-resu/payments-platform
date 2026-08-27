package handlers

import (
	"net/http"

	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
)

// registerMetrics mounts the Prometheus exposition endpoint.
//
// It is registered with [httpapi.Router.HandleRaw] rather than as a [httpapi.Handler] because the
// body is not ours to shape: the OpenMetrics text format is the client library's output, and
// wrapping it in this platform's JSON conventions would produce something no scraper can read.
//
// # Why it is a route at all rather than a separate listener
//
// Both shapes are defensible. A separate listener on a second port cannot be reached from the
// public ingress by construction, which is a stronger guarantee than a routing rule. This
// deployment mounts it on the same mux and relies on two controls instead: the ingress never
// routes /metrics, and the mesh requires mTLS for it. That choice buys one listener, one set of
// timeouts and one middleware chain per process — and, more importantly, it means the metrics
// endpoint is covered by the same panic recovery and the same request logging as everything else,
// rather than being the one surface where a panic takes the process down.
//
// The route is on the anonymous allowlist because a Prometheus scrape carries no OAuth2
// credential and never will; its authentication is the mesh's peer certificate.
func registerMetrics(rt *httpapi.Router, d Deps) {
	if d.Metrics == nil {
		return
	}
	rt.HandleRaw(http.MethodGet, httpapi.RouteMetrics, "getMetrics", d.Metrics)
}
