package handlers

import (
	"context"
	"net/http"

	"github.com/udaykishore-resu/payments-platform/internal/platform/health"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
)

// HealthReporter answers the three probes. *health.Registry satisfies it.
type HealthReporter interface {
	Live(ctx context.Context) health.Response
	Ready(ctx context.Context) health.Response
}

// DrainSignal reports whether the process has begun shutting down.
//
// It is separate from the health registry because draining is a property of the *process
// lifecycle*, not of any dependency: a draining pod's database is perfectly healthy and its
// readiness must nevertheless be false. The composition root supplies a closure over its own
// shutdown flag.
type DrainSignal func() bool

func registerHealth(rt *httpapi.Router, d Deps) {
	h := &healthHandlers{
		reporter: d.Health,
		draining: d.Draining,
		service:  d.Service,
		version:  d.Version,
		region:   d.Region,
	}
	rt.Handle(http.MethodGet, httpapi.RouteHealthz, "getHealthz", h.healthz)
	rt.Handle(http.MethodGet, httpapi.RouteReadyz, "getReadyz", h.readyz)
	rt.Handle(http.MethodGet, httpapi.RouteLivez, "getLivez", h.livez)
}

type healthHandlers struct {
	reporter HealthReporter
	draining DrainSignal
	service  string
	version  string
	region   string
}

// livez implements `getLivez`: is this process wedged such that only a restart helps?
//
// # It never touches a dependency, and this is the single most consequential rule here
//
// Suppose /livez checked Postgres. Aurora fails over — a documented, expected, ≤60 s event. Every
// pod's liveness probe fails simultaneously. The kubelet kills **every pod in the fleet**. The
// database recovers forty seconds later into a cluster with zero warm pods, empty connection
// pools, cold configuration snapshots and a thundering herd of restarts, turning a sixty-second
// transparent failover into a ten-minute total outage.
//
// The correct behaviour is the opposite: readiness fails (traffic sheds, the load balancer stops
// sending, clients get 503 with Retry-After), liveness holds (pods stay alive, warm and
// connected), and when the writer returns readiness recovers within one probe interval. Readiness
// is reversible; liveness is not. Downstream state belongs only in the reversible one.
//
// A draining process still reports live. It is not wedged — it is shutting down on purpose — and
// restarting it would abort the very in-flight requests the drain exists to finish.
func (h *healthHandlers) livez(w http.ResponseWriter, r *http.Request) error {
	if h.reporter == nil {
		httpapi.WriteJSON(w, r, http.StatusOK, h.status("ok", nil))
		return nil
	}
	res := h.reporter.Live(r.Context())
	status := http.StatusOK
	if res.Status == health.StatusDown {
		status = http.StatusServiceUnavailable
	}
	httpapi.WriteJSON(w, r, status, h.status(statusLabel(res.Status), res.Checks))
	return nil
}

// readyz implements `getReadyz`: should this pod receive traffic right now?
//
// This is the probe that *may* depend on downstreams, because its failure is reversible: the pod
// is removed from Service endpoints and is put back one probe interval after it recovers. It
// checks the database writer, the configuration snapshot age against the §15 staleness cliff, and
// whatever else the composition root registered.
//
// # Draining fails readiness before anything is closed
//
// The drain sequence's first act is to fail readiness, and only then to wait out endpoint
// propagation and start closing things. The order matters: Kubernetes removes a pod from
// endpoints and sends SIGTERM concurrently and unordered, so a process that closed its listener
// first would refuse connections that kube-proxy on some nodes is still routing to it —
// connection resets that clients see as 502s on every deploy.
func (h *healthHandlers) readyz(w http.ResponseWriter, r *http.Request) error {
	if h.draining != nil && h.draining() {
		httpapi.WriteJSON(w, r, http.StatusServiceUnavailable,
			h.status("unavailable", []health.Result{{
				Name:   "drain",
				Status: health.StatusDown,
				Error:  "the process is draining and is not accepting new work",
			}}))
		return nil
	}
	if h.reporter == nil {
		httpapi.WriteJSON(w, r, http.StatusOK, h.status("ok", nil))
		return nil
	}
	res := h.reporter.Ready(r.Context())
	status := http.StatusOK
	if res.Status == health.StatusDown {
		status = http.StatusServiceUnavailable
	}
	httpapi.WriteJSON(w, r, status, h.status(statusLabel(res.Status), res.Checks))
	return nil
}

// healthz implements `getHealthz`: the human and dashboard view.
//
// It is the deep composite — readiness plus per-check detail — and it is never used as a liveness
// probe. It is also what the startup probe watches, because "has it finished booting?" is
// answered by the same aggregate, and gating the other two probes on it is what stops a slow
// start from being read as a wedged process.
//
// Unlike readyz it reports 200 while draining, with the drain visible in `checks`. A dashboard
// asking "what is this pod doing" during a deploy should get an answer, not a 503.
func (h *healthHandlers) healthz(w http.ResponseWriter, r *http.Request) error {
	if h.reporter == nil {
		httpapi.WriteJSON(w, r, http.StatusOK, h.status("ok", nil))
		return nil
	}
	res := h.reporter.Ready(r.Context())
	checks := res.Checks
	label := statusLabel(res.Status)
	if h.draining != nil && h.draining() {
		checks = append(append([]health.Result(nil), checks...), health.Result{
			Name: "drain", Status: health.StatusDegraded, Error: "draining",
		})
		if label == "ok" {
			label = "degraded"
		}
	}
	status := http.StatusOK
	if res.Status == health.StatusDown {
		status = http.StatusServiceUnavailable
	}
	httpapi.WriteJSON(w, r, status, h.status(label, checks))
	return nil
}

// status renders the contract's HealthStatus.
//
// The per-check `Error` becomes `detail`. That is the one field on this surface that can carry an
// internal message, and it is acceptable here because the probes are cluster-internal: /readyz and
// /livez are never routed from the public ingress, and /healthz is reached only by the load
// balancer and by an operator. A dependency error visible to an operator is the entire point of
// the endpoint.
func (h *healthHandlers) status(label string, checks []health.Result) httpapi.HealthStatus {
	out := httpapi.HealthStatus{
		Status:  label,
		Service: h.service,
		Version: h.version,
		Region:  h.region,
	}
	for _, c := range checks {
		out.Checks = append(out.Checks, httpapi.HealthCheck{
			Name:      c.Name,
			Status:    statusLabel(c.Status),
			Detail:    c.Error,
			LatencyMs: int(c.DurationMS),
		})
	}
	return out
}

// statusLabel maps the registry's status onto the contract's three-value enum.
func statusLabel(s health.Status) string {
	switch s {
	case health.StatusUp:
		return "ok"
	case health.StatusDegraded:
		return "degraded"
	default:
		return "unavailable"
	}
}
