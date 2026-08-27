// Package handlers is the REST surface's one-file-per-resource handler set.
//
// # What a handler is allowed to do
//
// Decode, map to a command, call a service, render. Four steps, in that order, and nothing else.
//
// The rule is not stylistic. A handler that branches on a payment's state has taken a decision
// that the gRPC surface, the workflow engine and the reconciler will each have to take again,
// and the four copies will diverge — usually at the moment somebody fixes a bug in one of them.
// Keeping the decision in internal/application means it is taken once, is unit-testable without
// an HTTP request, and applies to every caller.
//
// The mechanical consequence is that these files are boring, and boring is the property being
// bought: a reviewer reading a handler is checking a mapping against a schema, not auditing
// business logic hidden in a transport layer.
//
// # Service dependencies are interfaces declared here
//
// Each handler set names the narrow interface it needs — [PaymentService], [MerchantService] and
// so on — rather than taking the concrete *payment.Service. Consumer-declared interfaces are the
// repository's convention (code-conventions §13), and here they buy something specific: a
// handler test can supply a three-method double instead of constructing a service with a unit of
// work, a gateway registry, a risk evaluator and a clock.
package handlers

import (
	"net/http"
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Deps is everything the handler set needs. A nil service disables its routes rather than
// panicking at request time: a binary that serves only the control plane has no payment service,
// and [Register] must not mount routes it cannot serve.
type Deps struct {
	// Payments, Merchants, Onboarding, Configuration, Gateways, Webhooks are the application
	// services. Each may be nil, in which case its routes are not registered.
	Payments      PaymentService
	Merchants     MerchantService
	Onboarding    OnboardingService
	Configuration ConfigurationService
	Gateways      GatewayService
	Webhooks      WebhookService

	// OnboardingLookup bridges the REST surface's merchant-addressed URLs to the workflow
	// engine's instance-addressed API. Required whenever Onboarding is wired.
	OnboardingLookup MerchantCaseLookup

	// Health serves the three probes. Required by every binary.
	Health HealthReporter

	// Draining reports whether shutdown has begun, so /readyz can fail before anything closes.
	// Nil means "never draining", which is correct for a test and for a process with no drain.
	Draining DrainSignal

	// Metrics is the Prometheus exposition handler. Nil omits /metrics.
	Metrics http.Handler

	// Service and Version identify this binary in probe responses. They come from the build
	// stamp so that "which build is this pod running?" is answerable from a probe rather than
	// from a deployment manifest that may have moved on.
	Service string
	Version string

	// BaseURL is the public origin used to build Location headers. It comes from configuration
	// rather than from the request's Host header: trusting Host lets a caller poison the
	// Location of a resource they just created, which hands an attacker a redirect primitive.
	BaseURL string

	// Region names the deployment region in probe responses, so an operator reading a health
	// dump from a screenshot knows which region produced it.
	Region string
}

// Register mounts every route this binary can serve onto rt.
//
// Registration is conditional on the service being wired, which is what lets nine binaries share
// one handler set: `webhook-ingress` mounts the ingress route and the probes and nothing else,
// and a request to /v1/payments there is a 404 rather than a 500 from a nil service.
func Register(rt *httpapi.Router, d Deps) {
	if d.Payments != nil {
		registerPayments(rt, d)
	}
	if d.Merchants != nil {
		registerMerchants(rt, d)
	}
	if d.Onboarding != nil {
		registerOnboarding(rt, d)
	}
	if d.Configuration != nil {
		registerConfiguration(rt, d)
	}
	if d.Gateways != nil {
		registerGateways(rt, d)
	}
	if d.Webhooks != nil {
		registerWebhooks(rt, d)
	}
	registerHealth(rt, d)
	registerMetrics(rt, d)
}

// pathValue reads a path parameter, rejecting an empty one with a named error.
//
// Empty is possible even on a matched route: `/v1/merchants//configuration` matches the template
// with an empty segment, and the resulting lookup for merchant "" would be a full-table scan
// under row-level security that returns nothing and looks like a 404.
func pathValue(r *http.Request, name string) (string, error) {
	v := strings.TrimSpace(r.PathValue(name))
	if v == "" {
		return "", apierror.Newf(apierror.CodeValidationFailed, "path parameter %q is empty", name).
			WithDetail(apierror.Detail{
				Field: name, Code: "MISSING",
				Message: "This path segment is required.",
				RuleID:  "L1.PATH_PARAM_PRESENT",
			})
	}
	return v, nil
}

// expandSet decodes the `expand` query parameter into a lookup.
//
// A nil result means "the default set", which [httpapi.PaymentOf] renders as attempts and
// refunds embedded and the routing plan omitted. An explicit empty `?expand=` is *not* nil: it
// means the caller asked for nothing embedded, and honouring that is what lets a client polling
// a payment in a loop stop paying for its attempt history.
func expandSet(r *http.Request) map[string]bool {
	vals, ok := r.URL.Query()["expand"]
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for _, v := range vals {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out[part] = true
			}
		}
	}
	return out
}

// notModified answers a conditional read.
//
// It reports true when the caller's If-None-Match matches the current token, in which case the
// 304 has already been written. The ETag is stamped on the 304 as well as on the 200: RFC 9110
// requires it, and without it a client's next conditional request has no token to send, so it
// re-downloads the body it just avoided downloading.
func notModified(w http.ResponseWriter, r *http.Request, etag string) bool {
	if etag == "" {
		return false
	}
	if !httpapi.ETagMatches(r.Header.Get(httpapi.HeaderIfNoneMatch), etag) {
		return false
	}
	httpapi.SetETagRaw(w, etag)
	httpapi.WriteNoContent(w, r, http.StatusNotModified)
	return true
}

// tenantOf reads the resolved tenant context.
//
// It is a one-line wrapper so that a handler needing only the tenant does not import
// internal/platform/tenantctx just for the accessor — and, more usefully, so the failure mode
// ("the tenant middleware did not run") has one call site to look at rather than a dozen.
func tenantOf(r *http.Request) (tenantctx.TenantContext, error) {
	return tenantctx.FromContext(r.Context())
}

// decodeInto reads the buffered body and strictly decodes it.
//
// The bytes come from the context rather than from r.Body because the body-limit middleware
// already consumed and buffered them — the same bytes the PAN detector scanned and the
// idempotency fingerprint was computed over. Re-reading r.Body here would find it empty, which
// is the bug this helper exists to make impossible.
func decodeInto(r *http.Request, dst any) error {
	return httpapi.DecodeJSON(httpapi.RawBody(r.Context()), dst)
}
