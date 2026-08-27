package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
)

// RateLimit applies the contract's per-tenant and per-merchant token buckets and emits the
// RateLimit-* headers.
//
// Budget: 2 ms (§12 stage 6) — one Redis round trip on the common path, with a local fallback
// bucket when Redis is unreachable so that a cache outage degrades the *accuracy* of the limit
// rather than removing it.
// Fails with: 429 RATE_LIMITED, carrying Retry-After.
//
// # Why the headers go on every response, not just the 429
//
// A client that only learns its budget when it has already exhausted it cannot pace itself; it
// can only back off after the damage. Emitting RateLimit-Remaining on every 200 lets a
// well-behaved integrator slow down before hitting the wall, which is the entire purpose of
// publishing a limit. The header names are the IETF draft's, matching baseline §19.3.
//
// # Why a limiter failure admits the request
//
// If the limiter itself errors — Redis down, script missing — the request proceeds. That is a
// deliberate fail-open, and it is the only fail-open in this chain. The reasoning: a rate limit
// protects capacity, and the failure of the limiter is not evidence that capacity is exhausted.
// Failing closed would convert a Redis blip into a total outage of the payments API, which is
// strictly worse than briefly serving above the limit — and the adaptive concurrency limiter
// immediately below is still in force, so the process cannot actually be overrun.
func RateLimit(limiter RateLimiter, table LimitTable) Middleware {
	if table == nil {
		table = ContractLimits{}
	}
	return func(next http.Handler) http.Handler {
		if limiter == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			template := httpapi.RouteTemplate(r.Context())
			rl, ok := table.LimitFor(r.Method, template)
			if !ok || rl.Scope == ScopeNone {
				next.ServeHTTP(w, r)
				return
			}
			key := limitKey(r, template, rl.Scope)
			if key == "" {
				// No identity to count against — the tenant middleware has not run, or the
				// route is anonymous. Counting every such request under one shared key would
				// let one caller exhaust the budget of all of them.
				next.ServeHTTP(w, r)
				return
			}
			decision, err := limiter.Allow(r.Context(), key, rl.AsLimit())
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			setRateLimitHeaders(w, decision)
			if !decision.Allowed {
				httpapi.WriteProblem(w, r, decision.Err())
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// setRateLimitHeaders stamps the budget on the response.
func setRateLimitHeaders(w http.ResponseWriter, d resilience.Decision) {
	h := w.Header()
	h.Set(httpapi.HeaderRateLimitLimit, strconv.Itoa(d.Limit))
	h.Set(httpapi.HeaderRateLimitLeft, strconv.Itoa(max(d.Remaining, 0)))
	h.Set(httpapi.HeaderRateLimitReset, strconv.Itoa(ceilSeconds(d.ResetAfter)))
	if !d.Allowed && d.RetryAfter > 0 {
		h.Set(httpapi.HeaderRetryAfter, strconv.Itoa(ceilSeconds(d.RetryAfter)))
	}
}

// ceilSeconds rounds up, never down and never to zero for a non-zero duration.
//
// Rounding 400 ms down to `Retry-After: 0` tells a client to retry immediately, which produces a
// tight retry loop against a limiter that is already rejecting — the exact amplification the
// limit exists to prevent.
func ceilSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}

// limitKey builds the counter key for a scope.
//
// The template is part of every key. Without it a merchant's payment creations and its payment
// reads share one bucket, so a polling loop starves the money path — which is the failure this
// whole per-route table exists to avoid.
func limitKey(r *http.Request, template string, scope Scope) string {
	switch scope {
	case ScopeTenant:
		tc, err := tenantctx.FromContext(r.Context())
		if err != nil {
			return ""
		}
		return "rl:t:" + tc.TenantID.String() + ":" + r.Method + ":" + template
	case ScopeMerchant:
		tc, err := tenantctx.FromContext(r.Context())
		if err != nil {
			return ""
		}
		m := merchantFromPath(template, r.URL.EscapedPath())
		if m == "" {
			m = r.URL.Query().Get("merchantId")
		}
		if m == "" {
			// A merchant-scoped route whose merchant is only in the body — createPayment — is
			// counted against the tenant until the handler knows better. Parsing the body here
			// to find the merchant would mean decoding before the L1 validator has run, which
			// inverts §12 for the sake of a slightly tighter bucket.
			return "rl:t:" + tc.TenantID.String() + ":" + r.Method + ":" + template
		}
		return "rl:m:" + tc.TenantID.String() + ":" + m + ":" + r.Method + ":" + template
	case ScopeGateway:
		gw := gatewayFromPath(template, r.URL.EscapedPath())
		if gw == "" {
			return ""
		}
		return "rl:g:" + gw
	default:
		// ScopeNone means the route is not rate limited. An empty key is how that is spelled;
		// the caller skips the counter entirely when the key is empty
	}
	return ""
}

func gatewayFromPath(template, path string) string {
	if !strings.Contains(template, "{gateway}") {
		return ""
	}
	tp := strings.Split(strings.Trim(template, "/"), "/")
	pp := strings.Split(strings.Trim(path, "/"), "/")
	if len(tp) != len(pp) {
		return ""
	}
	for i, seg := range tp {
		if seg == "{gateway}" {
			return pp[i]
		}
	}
	return ""
}

// Concurrency applies the adaptive in-flight limiter and sheds by priority class.
//
// Budget: negligible — two atomic operations and a comparison.
// Fails with: 503 CONCURRENCY_LIMIT_EXCEEDED / SERVICE_UNAVAILABLE, with Retry-After.
//
// # Two mechanisms, not one
//
// The shedder decides *whether this class of work is still being accepted* at the current
// pressure; the limiter decides *whether there is room right now*. They answer different
// questions and both are needed. A limiter alone rejects whatever arrives when it is full, which
// under load means it rejects refunds and authorizations in whatever ratio the traffic happens
// to have — so a flood of status polling can crowd out the money path. The shedder is what makes
// the rejection ordered: reports go first, then reads, then authorizations, and refunds and
// webhook ingest are never shed at any pressure, because failing to return a customer's money is
// a consumer-harm event while failing to accept a new payment is a lost sale.
//
// # Why the release closure feeds back the observed latency
//
// The adaptive limiter derives its limit from the ratio of the current round-trip time to the
// best observed round-trip time (a gradient limiter). Without the feedback it is a fixed limiter
// with extra steps, and a fixed limiter is a number somebody guessed at deploy time that is
// wrong on every instance type and every traffic mix afterwards.
func Concurrency(limiter ConcurrencyLimiter, shedder Shedder) Middleware {
	return func(next http.Handler) http.Handler {
		if limiter == nil && shedder == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			class := httpapi.Priority(r.Context())
			if shedder != nil {
				if err := shedder.Admit(class); err != nil {
					httpapi.WriteProblem(w, r, err)
					return
				}
			}
			if limiter == nil {
				next.ServeHTTP(w, r)
				return
			}
			release, err := limiter.Acquire(r.Context())
			if err != nil {
				httpapi.WriteProblem(w, r, err)
				return
			}
			start := time.Now()
			rec, _ := recorderFor(w)
			// The `dropped` flag is what teaches the limiter that it overshot. A 5xx or a 429
			// from below is evidence of overload; a 4xx is evidence of a bad request and must
			// not shrink the limit, or a client sending malformed JSON in a loop would throttle
			// everybody else.
			defer func() {
				release(time.Since(start), rec.Status() >= http.StatusInternalServerError ||
					rec.Status() == http.StatusTooManyRequests)
			}()
			next.ServeHTTP(rec, r)
		})
	}
}
