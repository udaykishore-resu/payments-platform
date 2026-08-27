package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// BodyLimit buffers the request body under a hard ceiling and puts the exact bytes on the
// context.
//
// Budget: 1 ms for a typical body. Fails with: 413 REQUEST_TOO_LARGE, 400
// SENSITIVE_DATA_IN_REQUEST.
//
// # Why the body is read here rather than in the handler
//
// Three consumers need the *same* bytes and two of them run before the handler: the idempotency
// fingerprint, which must be computed over what the client actually sent so a semantically
// identical but differently-ordered body is recognised as the same request; the L1 PAN detector,
// which must see the raw text because a card number split across struct fields is still a card
// number in the bytes; and the gateway webhook verifier, whose HMAC is over the received octets
// and which a re-encoded parse silently invalidates. A streaming decode consumes the reader and
// leaves the other two with nothing.
//
// The size limit is what makes buffering safe: without it, this middleware would be a request to
// allocate whatever the client sends.
//
// GET and DELETE are skipped. A body on a safe method is not something this surface accepts, and
// reading one would be work done on behalf of a caller doing something the contract does not
// describe.
func BodyLimit(maxBytes int64) Middleware {
	if maxBytes <= 0 {
		maxBytes = httpapi.DefaultMaxBodyBytes
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodDelete, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}
			// Content-Length is a hint, not a guarantee — a chunked body has none and a
			// malicious one lies — so it is used only to reject early. http.MaxBytesReader is
			// what actually enforces the bound.
			if r.ContentLength > maxBytes {
				httpapi.WriteProblem(w, r, apierror.Newf(apierror.CodeRequestTooLarge,
					"request body exceeds the %d byte limit for this endpoint", maxBytes))
				return
			}
			raw, err := httpapi.ReadBody(w, r, maxBytes)
			if err != nil {
				httpapi.WriteProblem(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(httpapi.WithRawBody(r.Context(), raw)))
		})
	}
}

// ContentType enforces application/json on requests carrying a body.
//
// Budget: negligible. Fails with: 415 UNSUPPORTED_MEDIA_TYPE.
//
// Separate from decoding because the answer is a different status and names a different fix: 415
// says "change your Content-Type", 400 says "change your body". A proxy that rewrites the
// content type has to be caught before the bytes are parsed rather than after they fail to
// decode into something plausible, or the client spends a day debugging their JSON.
//
// `application/merge-patch+json` is accepted on PATCH because the contract declares it for
// updateMerchant; RFC 7396 merge-patch over a JSON object is byte-identical to the JSON the
// decoder already handles, so accepting it costs nothing and refusing it would break a client
// following the contract.
func ContentType() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodDelete, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}
			if r.ContentLength == 0 && r.Header.Get(httpapi.HeaderContentType) == "" {
				next.ServeHTTP(w, r)
				return
			}
			ct := mediaTypeOf(r.Header.Get(httpapi.HeaderContentType))
			if ct == httpapi.MediaJSON ||
				(r.Method == http.MethodPatch && ct == "application/merge-patch+json") {
				next.ServeHTTP(w, r)
				return
			}
			httpapi.WriteProblem(w, r, apierror.Newf(apierror.CodeUnsupportedMediaType,
				"this endpoint accepts %s; the request declared %q",
				httpapi.MediaJSON, r.Header.Get(httpapi.HeaderContentType)))
		})
	}
}

func mediaTypeOf(v string) string {
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// CORSPolicy is the cross-origin configuration. Its zero value denies everything.
type CORSPolicy struct {
	// AllowedOrigins is an exact-match allowlist. There is deliberately no wildcard and no
	// pattern: `*` on a credentialed API is a same-origin policy switched off, and a prefix or
	// suffix pattern is how `evil-example.com.attacker.net` gets allowed by a rule that was
	// meant to say `example.com`.
	AllowedOrigins []string
	// AllowCredentials permits cookies and Authorization on a cross-origin request. It is
	// separate from the origin list because the combination `*` + credentials is forbidden by
	// the specification and silently ignored by browsers, so a policy that sets both is a
	// policy that does not do what its author believes.
	AllowCredentials bool
	// MaxAgeSeconds caps preflight caching. Zero means the browser's default (5 s in Chrome),
	// which is conservative and correct until measurement says otherwise.
	MaxAgeSeconds int
}

// allowedCORSHeaders is the fixed request-header allowlist for a preflight. It is a constant
// rather than configuration because every header on it is one this API actually reads, and a
// configurable list is one deploy away from `*`.
var allowedCORSHeaders = strings.Join([]string{
	httpapi.HeaderContentType, httpapi.HeaderIdempotencyKey, httpapi.HeaderRequestID,
	httpapi.HeaderCorrelationID, httpapi.HeaderTraceparent, httpapi.HeaderTracestate,
	httpapi.HeaderIfMatch, httpapi.HeaderIfNoneMatch, "Authorization",
}, ", ")

// exposedCORSHeaders is what a browser client may read off a response. Without this list a
// cross-origin caller sees the body and none of the headers, which means no ETag, no
// Idempotent-Replay and no rate-limit budget — and therefore a client that cannot implement
// conditional writes or backoff.
var exposedCORSHeaders = strings.Join([]string{
	httpapi.HeaderETag, httpapi.HeaderLocation, httpapi.HeaderRequestID,
	httpapi.HeaderCorrelationID, httpapi.HeaderIdempotentReply, httpapi.HeaderRetryAfter,
	httpapi.HeaderRateLimitLimit, httpapi.HeaderRateLimitLeft, httpapi.HeaderRateLimitReset,
}, ", ")

// CORS answers preflights and stamps the cross-origin headers, denying by default.
//
// Budget: negligible. Fails with: 403 FORBIDDEN on a preflight from an origin not on the list.
//
// # Why it runs above authentication
//
// A browser preflight is an unauthenticated OPTIONS by specification — it carries no
// Authorization header and no cookies, because its whole purpose is to ask permission *before*
// sending credentials. Authenticating it returns 401 to a request that was only asking whether
// it may send the real one, and the browser reports that as a CORS failure with no useful
// detail. So the preflight is answered here and never reaches the authentication stage.
//
// # Why a non-matching origin is answered rather than ignored
//
// Omitting Access-Control-Allow-Origin is the specification-correct denial, and it is what a
// non-preflight request gets. A *preflight* from a disallowed origin gets an explicit 403
// instead, because a silent 204-with-no-headers is indistinguishable in a browser console from
// a network fault, and the resulting support ticket says "your API is down".
func CORS(policy CORSPolicy) Middleware {
	allowed := make(map[string]bool, len(policy.AllowedOrigins))
	for _, o := range policy.AllowedOrigins {
		allowed[strings.ToLower(strings.TrimSpace(o))] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			preflight := r.Method == http.MethodOptions &&
				r.Header.Get("Access-Control-Request-Method") != ""

			if origin == "" {
				// No Origin at all: a server-to-server call, which is the overwhelming majority
				// of this API's traffic. Nothing to decide.
				next.ServeHTTP(w, r)
				return
			}
			ok := allowed[strings.ToLower(origin)]
			if ok {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				// Vary is not optional: without it a shared cache can serve one origin's
				// response, complete with its Allow-Origin header, to a different origin.
				h.Add("Vary", "Origin")
				h.Set("Access-Control-Expose-Headers", exposedCORSHeaders)
				if policy.AllowCredentials {
					h.Set("Access-Control-Allow-Credentials", "true")
				}
			}
			if !preflight {
				next.ServeHTTP(w, r)
				return
			}
			if !ok {
				httpapi.WriteProblem(w, r, apierror.New(apierror.CodeForbidden,
					"this origin is not permitted to call the API from a browser"))
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, OPTIONS")
			h.Set("Access-Control-Allow-Headers", allowedCORSHeaders)
			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")
			if policy.MaxAgeSeconds > 0 {
				h.Set("Access-Control-Max-Age", strconv.Itoa(policy.MaxAgeSeconds))
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}
}

// SecurityHeaders stamps the fixed response headers on every response, including errors.
//
// Budget: negligible. Fails with: nothing.
//
// # Why these headers on a JSON API
//
// It is tempting to argue that an API returning `application/json` needs none of this. The
// argument fails on the error paths and on the paths nobody planned:
//
//   - X-Content-Type-Options: nosniff stops a browser from re-interpreting a JSON body that
//     happens to begin with `<` as HTML. That body exists: a misconfigured upstream proxy
//     returning an error page through our route is exactly the case, and the resulting XSS runs
//     on our origin.
//   - Content-Security-Policy: default-src 'none' means that if a body is ever rendered, it can
//     load nothing. It is one header that makes a whole class of "but the API returned HTML"
//     bug inert.
//   - Referrer-Policy: no-referrer stops a payment id in a URL from leaking to a third party
//     through a redirect the 3DS flow performs.
//   - X-Frame-Options: DENY costs nothing and covers the case where somebody puts a browser UI
//     on a path under this host.
//   - Strict-Transport-Security is emitted only when configured, because sending it from a
//     plaintext listener behind a TLS-terminating sidecar pins clients to HTTPS for a host that
//     may legitimately be reached over HTTP in a cluster-internal context.
func SecurityHeaders(hstsMaxAgeSeconds int) Middleware {
	hsts := ""
	if hstsMaxAgeSeconds > 0 {
		hsts = "max-age=" + strconv.Itoa(hstsMaxAgeSeconds) + "; includeSubDomains"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			if hsts != "" {
				h.Set("Strict-Transport-Security", hsts)
			}
			next.ServeHTTP(w, r)
		})
	}
}
