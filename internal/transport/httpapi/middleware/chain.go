package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authn"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authz"
	"github.com/udaykishore-resu/payments-platform/internal/platform/idempotency"
)

// Middleware is one stage of the §12 pipeline.
//
// The signature is the standard `func(http.Handler) http.Handler` rather than a bespoke type
// with a Next field, because the standard shape composes with every third-party handler in the
// ecosystem and because a bespoke one invites a stage that forgets to call the next handler and
// silently returns 200 with an empty body.
type Middleware func(http.Handler) http.Handler

// Chain applies ms to h so that ms[0] is the *outermost* stage.
//
// Outermost-first is the order the pipeline table reads in, which matters more than it sounds:
// the version of this function that applied the slice in reverse was correct and unreviewable,
// because every reader had to mentally invert a list before they could check it against §12.
func Chain(h http.Handler, ms ...Middleware) http.Handler {
	for i := len(ms) - 1; i >= 0; i-- {
		h = ms[i](h)
	}
	return h
}

// RouteResolver reports the route *template* a request would match, before dispatch.
//
// [github.com/udaykishore-resu/payments-platform/internal/transport/httpapi.Router] implements
// it. The chain needs it because the mux does not run until after every stage here, and a span
// or a metric labelled with the raw path carries a payment id — unbounded cardinality in the
// backend and customer identifiers in a vendor's index.
type RouteResolver interface {
	Resolve(r *http.Request) string
}

// MetricsSink is the RED-metrics recorder. *telemetry.Registry satisfies it.
type MetricsSink interface {
	ObserveHTTPRequest(ctx context.Context, service, route, method string, status int,
		tier telemetry.TenantTier, d time.Duration)
}

// Authenticator turns a request into a principal, or fails with an AUTHENTICATION-category
// error. It is an interface rather than a concrete validator because a process serves more than
// one scheme — bearer tokens at the edge, mTLS peer identity inside the mesh — and the
// composition root picks.
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (*authn.Principal, error)
}

// Authorizer evaluates an RBAC+ABAC request. *authz.Policy satisfies it.
type Authorizer interface {
	Evaluate(ctx context.Context, req authz.Request) authz.Decision
}

// RateLimiter is the token-bucket front end. resilience.Limiter satisfies it, which is what
// gives the middleware the Redis-backed distributed limiter with its local fallback for free.
type RateLimiter interface {
	Allow(ctx context.Context, key string, limit resilience.Limit) (resilience.Decision, error)
}

// Shedder decides whether a request's priority class is still being admitted at the current
// pressure. *resilience.Shedder satisfies it.
type Shedder interface {
	Admit(class resilience.PriorityClass) error
}

// ConcurrencyLimiter is the adaptive in-flight limiter. *resilience.AdaptiveLimiter satisfies
// it; the returned release closure feeds the observed round-trip time back so the limit tracks
// the real service time rather than a number somebody guessed at deploy time.
type ConcurrencyLimiter interface {
	Acquire(ctx context.Context) (release func(rtt time.Duration, dropped bool), err error)
}

// IdempotencyManager claims an operation. *idempotency.Manager satisfies it.
type IdempotencyManager interface {
	Begin(ctx context.Context, key ports.IdempotencyKey, body []byte) (*idempotency.Handle, error)
}

// Config carries everything the standard chain needs.
//
// Every field that can be absent has a documented degradation, and none of the degradations is
// "skip the control silently". A nil Authenticator means the chain refuses to authenticate, not
// that it lets everyone through — an omission in the composition root must fail closed, because
// the failure mode of failing open is a public payments API with no authentication that passes
// every smoke test.
type Config struct {
	// Service names this binary in metrics and logs: "payment-api", "webhook-ingress".
	Service string

	// Routes resolves the template. Required; without it every metric is labelled "unmatched".
	Routes RouteResolver

	// Metrics records the RED series. Optional: a nil sink disables recording, which is right
	// for a unit test and wrong for a deployment, and the composition root is where that is
	// visible.
	Metrics MetricsSink

	// Logger is the base logger for the access log. Defaults to the telemetry context logger.
	Logger *slog.Logger

	// Authenticator, Authorizer: the §12 stage 3 and stage 5 controls. A nil Authenticator
	// rejects every request with 401; a nil Authorizer rejects every request with 403.
	Authenticator Authenticator
	Authorizer    Authorizer

	// AnonymousRoutes are the templates that bypass authentication entirely: the probes, and
	// the gateway webhook ingress, whose caller is a gateway holding a signature and no
	// platform credential. It is an explicit allowlist rather than a prefix rule, because a
	// prefix rule is one careless route registration away from exposing a resource.
	AnonymousRoutes map[string]bool

	// RateLimiter and Limits implement §12 stage 6.
	RateLimiter RateLimiter
	Limits      LimitTable

	// Shedder and Concurrency implement the adaptive limiter and priority shedding.
	Shedder     Shedder
	Concurrency ConcurrencyLimiter

	// Idempotency implements §12 stages 8 and 17. Nil disables the middleware, which is correct
	// only for a binary that serves no unsafe operation.
	Idempotency IdempotencyManager

	// MaxBodyBytes bounds a request body. Zero means httpapi.DefaultMaxBodyBytes.
	MaxBodyBytes int64

	// MaxSnapshotBytes bounds a buffered response for the idempotency snapshot. Zero means
	// DefaultMaxSnapshotBytes.
	MaxSnapshotBytes int

	// CORS is the cross-origin policy. The zero value denies every cross-origin request, which
	// is the correct default for an API whose clients are servers.
	CORS CORSPolicy

	// HSTSMaxAgeSeconds sets Strict-Transport-Security. Zero omits the header, which is right
	// for a plaintext listener behind a TLS-terminating mesh sidecar and wrong at the edge.
	HSTSMaxAgeSeconds int
}

// New returns the §12 chain in order, outermost first.
//
// Returning a slice rather than a composed handler is deliberate: the composition root logs the
// chain it built, and the middleware test asserts the order by name. A function that returned
// an opaque http.Handler would make both impossible, and "is the chain in the right order?"
// would be answerable only by reading this file.
func New(cfg Config) []Middleware {
	if cfg.Service == "" {
		cfg.Service = "http"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return []Middleware{
		Recover(cfg.Metrics, cfg.Service),
		RequestID(),
		Tracing(cfg.Routes, cfg.Service),
		Logging(cfg.Logger),
		Metrics(cfg.Metrics, cfg.Service),
		BodyLimit(cfg.MaxBodyBytes),
		ContentType(),
		CORS(cfg.CORS),
		SecurityHeaders(cfg.HSTSMaxAgeSeconds),
		Authenticate(cfg.Authenticator, cfg.AnonymousRoutes),
		Tenant(cfg.AnonymousRoutes),
		Authorize(cfg.Authorizer, cfg.AnonymousRoutes),
		RateLimit(cfg.RateLimiter, cfg.Limits),
		Concurrency(cfg.Concurrency, cfg.Shedder),
		Idempotency(cfg.Idempotency, cfg.MaxSnapshotBytes),
	}
}

// Names returns the stage names of the standard chain, in order. The middleware test asserts
// this against the §12 table, so a reordering is a failing test rather than a review comment
// somebody might not leave.
func Names() []string {
	return []string{
		"recover", "requestid", "tracing", "logging", "metrics",
		"bodylimit", "contenttype", "cors", "securityheaders",
		"authn", "tenant", "authz", "ratelimit", "concurrency", "idempotency",
	}
}
