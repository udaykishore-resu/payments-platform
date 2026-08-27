package httpapi

import (
	"context"
	"net/http"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authn"
)

// Canonical request and response header names.
//
// They are constants rather than string literals at each use site because two of them —
// Idempotency-Key and X-Request-Id — are load-bearing across four packages, and a typo in one
// of those produces a silently unscoped idempotency record or a correlation id that appears in
// the response and in no log line.
const (
	HeaderRequestID       = "X-Request-Id"
	HeaderCorrelationID   = "X-Correlation-Id"
	HeaderTraceparent     = "traceparent"
	HeaderTracestate      = "tracestate"
	HeaderIdempotencyKey  = "Idempotency-Key"
	HeaderIdempotentReply = "Idempotent-Replay"
	HeaderIfMatch         = "If-Match"
	HeaderIfNoneMatch     = "If-None-Match"
	HeaderETag            = "ETag"
	HeaderLocation        = "Location"
	HeaderRetryAfter      = "Retry-After"
	HeaderRateLimitLimit  = "RateLimit-Limit"
	HeaderRateLimitLeft   = "RateLimit-Remaining"
	HeaderRateLimitReset  = "RateLimit-Reset"
	HeaderGatewaySig      = "X-Gateway-Signature"
	HeaderContentType     = "Content-Type"
	HeaderCacheControl    = "Cache-Control"
)

// Media types. `application/problem+json` is RFC 9457; every error in this platform is rendered
// with it, including the ones produced before a handler runs.
const (
	MediaJSON    = "application/json"
	MediaProblem = "application/problem+json"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyCorrelationID
	ctxKeyRouteTemplate
	ctxKeyPrincipal
	ctxKeyPriority
	ctxKeyRawBody
)

// WithRequestID stamps the per-request identifier.
//
// It is set by the requestid middleware, from the caller's X-Request-Id when one is supplied
// and from a freshly minted `req_` ULID otherwise. Everything downstream — the log spine, the
// problem document, the audit record, the idempotency record's origin — reads it from here
// rather than from the header, so that a handler cannot be handed one value while the logs
// carry another.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// RequestID returns the per-request identifier, or "" before the requestid middleware has run.
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(ctxKeyRequestID).(string)
	return s
}

// WithCorrelationID stamps the business correlation identifier that spans a whole causal
// fan-out. It defaults to the request id when the caller supplies none.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyCorrelationID, id)
}

// CorrelationID returns the causal-chain identifier, falling back to the request id so that a
// caller who never sets one still gets a chain that can be queried.
func CorrelationID(ctx context.Context) string {
	if s, _ := ctx.Value(ctxKeyCorrelationID).(string); s != "" {
		return s
	}
	return RequestID(ctx)
}

// WithRouteTemplate records the matched route *template* — `/v1/payments/{paymentId}` — on the
// context.
//
// This exists for one reason: span names and metric labels must never carry the raw path. A
// span named `/v1/payments/pay_01JB8Z9K2QW3E4R5T6Y7U8I9O0` produces one distinct span name per
// payment, which is unbounded cardinality in the tracing backend and, worse, puts a customer's
// payment identifier into a field that is indexed, retained and shared with a vendor. The
// template is the only form either subsystem is allowed to see.
func WithRouteTemplate(ctx context.Context, tmpl string) context.Context {
	return context.WithValue(ctx, ctxKeyRouteTemplate, tmpl)
}

// RouteTemplate returns the matched route template, or "unmatched" when no route matched.
//
// "unmatched" rather than "" so that a 404's metric series is a real, greppable label value
// instead of an empty string that looks like a bug in the instrumentation.
func RouteTemplate(ctx context.Context) string {
	if s, _ := ctx.Value(ctxKeyRouteTemplate).(string); s != "" {
		return s
	}
	return "unmatched"
}

// WithPrincipal stores the authenticated identity for the duration of the request.
//
// The tenant context is carried separately, by internal/platform/tenantctx, because persistence
// and the event consumer need the tenant and must not be given the full authentication result.
// This value is for the transport layer's own use: the authorization middleware's ABAC request,
// the audit actor, and the L5 scope check.
func WithPrincipal(ctx context.Context, p *authn.Principal) context.Context {
	return context.WithValue(ctx, ctxKeyPrincipal, p)
}

// Principal returns the authenticated identity, or nil before authentication has run.
func Principal(ctx context.Context) *authn.Principal {
	p, _ := ctx.Value(ctxKeyPrincipal).(*authn.Principal)
	return p
}

// WithPriority records the load-shedding priority class this request belongs to.
func WithPriority(ctx context.Context, c resilience.PriorityClass) context.Context {
	return context.WithValue(ctx, ctxKeyPriority, c)
}

// Priority returns the request's shedding class, defaulting to the least protected class.
//
// Defaulting to background rather than to money-out is the fail-safe direction: an unclassified
// route is one nobody has reasoned about, and admitting it ahead of a refund during an incident
// would be a decision made by omission.
func Priority(ctx context.Context) resilience.PriorityClass {
	if c, ok := ctx.Value(ctxKeyPriority).(resilience.PriorityClass); ok {
		return c
	}
	return resilience.PriorityBackground
}

// WithRawBody carries the exact bytes the client sent.
//
// Two consumers need the raw bytes rather than a re-encoding: the idempotency fingerprint,
// which must be computed over what the client actually sent so that a reordered-but-equivalent
// body is recognised as the same request, and the gateway webhook verifier, whose signature is
// computed over the received octets and which a re-encoded parse would silently invalidate.
func WithRawBody(ctx context.Context, body []byte) context.Context {
	return context.WithValue(ctx, ctxKeyRawBody, body)
}

// RawBody returns the buffered request body, or nil when the body was not buffered.
func RawBody(ctx context.Context) []byte {
	b, _ := ctx.Value(ctxKeyRawBody).([]byte)
	return b
}

// PriorityOfRoute classifies a route template into a shedding class.
//
// The mapping is a table rather than a heuristic on the method because the ordering is a
// business decision, not a technical one: money *out* (refunds, voids, webhook ingest)
// outranks money *in* (authorizations), because failing to refund a customer is a
// consumer-harm event while failing to accept a new payment is a lost sale. Listings are
// classified below single reads because a list is a report and a read is usually a client
// polling a payment it is waiting on.
func PriorityOfRoute(method, template string) resilience.PriorityClass {
	switch template {
	case RouteRefundPayment, RouteVoidPayment, RouteReceiveWebhook:
		return resilience.PriorityMoneyOut
	case RouteCapturePayment:
		return resilience.PriorityCapture
	case RouteCreatePayment:
		// Create and list share a template and are separated by method: POST /v1/payments is
		// an authorization, GET /v1/payments is a report.
		if method == http.MethodPost {
			return resilience.PriorityAuthorize
		}
		return resilience.PriorityReport
	case RouteListMerchants, RouteListGateways, RouteListConfigVersions:
		if method == http.MethodGet {
			return resilience.PriorityReport
		}
		return resilience.PriorityBackground
	}
	if method == http.MethodGet {
		return resilience.PriorityRead
	}
	return resilience.PriorityBackground
}
