package middleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authn"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authz"
	"github.com/udaykishore-resu/payments-platform/internal/platform/idempotency"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi/middleware"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// TestChainOrderMatchesBaselineSection12 pins the pipeline order.
//
// The order is load-bearing and every adjacency in it is a decision with a failure mode behind
// it. A reordering that looks harmless — moving the rate limiter above authentication, say —
// changes who is counted against which bucket, and that is not something a code review reliably
// catches. Asserting the list here turns a reordering into a failing test that names the stage.
func TestChainOrderMatchesBaselineSection12(t *testing.T) {
	t.Parallel()
	want := []string{
		"recover", "requestid", "tracing", "logging", "metrics",
		"bodylimit", "contenttype", "cors", "securityheaders",
		"authn", "tenant", "authz", "ratelimit", "concurrency", "idempotency",
	}
	got := middleware.Names()
	if len(got) != len(want) {
		t.Fatalf("chain length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("stage %d = %q, want %q", i, got[i], want[i])
		}
	}
	if n := len(middleware.New(middleware.Config{Service: "test"})); n != len(want) {
		t.Errorf("New built %d stages, Names lists %d", n, len(want))
	}
}

// TestRecoverReturns500WithoutStack is the assertion that a panic never leaks a stack trace.
//
// A Go stack names package paths, file names and line numbers, and occasionally a value that
// happened to be in an argument register. Publishing it tells an attacker which library versions
// are deployed and where the parsing happens. This test asserts the negative — that none of the
// panic text and none of the recognisable stack markers appear in the body.
func TestRecoverReturns500WithoutStack(t *testing.T) {
	t.Parallel()
	const secret = "goroutine-secret-marker"
	h := middleware.Chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(secret) }),
		middleware.Recover(nil, "test"),
		middleware.RequestID(),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/payments", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	for _, marker := range []string{secret, "goroutine ", ".go:", "runtime.", "panic("} {
		if strings.Contains(body, marker) {
			t.Errorf("response body leaks %q:\n%s", marker, body)
		}
	}
	var p httpapi.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a problem document: %v", err)
	}
	if p.Code != string(apierror.CodeInternalError) {
		t.Errorf("code = %q, want INTERNAL_ERROR", p.Code)
	}
	if p.RequestID == "" {
		t.Error("problem carries no requestId; the operator cannot find the log line")
	}
}

// TestRecoverPassesThroughErrAbortHandler asserts that net/http's deliberate abort is not
// converted into a 500. Swallowing it would turn an intentional abort — a proxy giving up on a
// dead upstream — into an error attributed to us.
func TestRecoverPassesThroughErrAbortHandler(t *testing.T) {
	t.Parallel()
	h := middleware.Chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }),
		middleware.Recover(nil, "test"),
	)
	defer func() {
		rec := recover()
		if e, ok := rec.(error); !ok || !errors.Is(e, http.ErrAbortHandler) {
			t.Errorf("recovered %v, want ErrAbortHandler to propagate", rec)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

// TestRequestIDEchoesAndSanitises covers the correlation-header contract.
func TestRequestIDEchoesAndSanitises(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		supplied   string
		wantEcho   bool
		wantMinted bool
	}{
		{"absent mints one", "", false, true},
		{"well-formed is echoed", "req_01JB8Z22222222222222222222", true, false},
		{"newline is rejected and replaced", "abc\ndef", false, true},
		{"oversized is rejected and replaced", strings.Repeat("x", 200), false, true},
		{"high byte is rejected and replaced", "caf\u00e9", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var seen string
			h := middleware.RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = httpapi.RequestID(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.supplied != "" {
				req.Header.Set(httpapi.HeaderRequestID, tc.supplied)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if seen == "" {
				t.Fatal("no request id on the context")
			}
			if tc.wantEcho && seen != tc.supplied {
				t.Errorf("request id = %q, want the supplied %q", seen, tc.supplied)
			}
			if tc.wantMinted && seen == tc.supplied {
				t.Errorf("request id = %q, want a freshly minted one", seen)
			}
			if tc.wantMinted && !strings.HasPrefix(seen, "req_") {
				t.Errorf("minted id %q is not a req_ ULID", seen)
			}
			if got := rec.Header().Get(httpapi.HeaderRequestID); got != seen {
				t.Errorf("echoed header = %q, context = %q", got, seen)
			}
		})
	}
}

// TestSecurityHeaders asserts the fixed header set is present on every response, including the
// ones a later stage rejects.
func TestSecurityHeaders(t *testing.T) {
	t.Parallel()
	h := middleware.SecurityHeaders(31536000)(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	want := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Resource-Policy": "same-origin",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if !strings.HasPrefix(rec.Header().Get("Strict-Transport-Security"), "max-age=31536000") {
		t.Errorf("HSTS = %q", rec.Header().Get("Strict-Transport-Security"))
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("CSP = %q", rec.Header().Get("Content-Security-Policy"))
	}
}

// TestCORSDeniesByDefault asserts that an unconfigured policy admits no cross-origin caller and
// that a preflight from a disallowed origin is answered explicitly rather than silently.
func TestCORSDeniesByDefault(t *testing.T) {
	t.Parallel()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	tests := []struct {
		name       string
		policy     middleware.CORSPolicy
		origin     string
		preflight  bool
		wantStatus int
		wantAllow  string
	}{
		{"no origin passes through", middleware.CORSPolicy{}, "", false, http.StatusOK, ""},
		{"disallowed origin gets no allow header", middleware.CORSPolicy{}, "https://evil.example", false, http.StatusOK, ""},
		{"disallowed preflight is 403", middleware.CORSPolicy{}, "https://evil.example", true, http.StatusForbidden, ""},
		{
			"allowed origin is echoed",
			middleware.CORSPolicy{AllowedOrigins: []string{"https://app.example"}},
			"https://app.example", false, http.StatusOK, "https://app.example",
		},
		{
			"allowed preflight is 204",
			middleware.CORSPolicy{AllowedOrigins: []string{"https://app.example"}},
			"https://app.example", true, http.StatusNoContent, "https://app.example",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			method := http.MethodGet
			if tc.preflight {
				method = http.MethodOptions
			}
			req := httptest.NewRequest(method, "/v1/payments", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.preflight {
				req.Header.Set("Access-Control-Request-Method", "POST")
			}
			rec := httptest.NewRecorder()
			middleware.CORS(tc.policy)(next).ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.wantAllow {
				t.Errorf("Allow-Origin = %q, want %q", got, tc.wantAllow)
			}
			if tc.wantAllow != "" && !strings.Contains(rec.Header().Get("Vary"), "Origin") {
				t.Error("Vary does not list Origin; a shared cache could cross-serve the response")
			}
		})
	}
}

// TestContentTypeRejectsNonJSON covers the 415 boundary.
func TestContentTypeRejectsNonJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		method      string
		contentType string
		body        string
		wantStatus  int
	}{
		{"json accepted", http.MethodPost, "application/json", `{}`, http.StatusOK},
		{"json with charset accepted", http.MethodPost, "application/json; charset=utf-8", `{}`, http.StatusOK},
		{"merge-patch accepted on PATCH", http.MethodPatch, "application/merge-patch+json", `{}`, http.StatusOK},
		{"merge-patch rejected on POST", http.MethodPost, "application/merge-patch+json", `{}`, http.StatusUnsupportedMediaType},
		{"form rejected", http.MethodPost, "application/x-www-form-urlencoded", `a=b`, http.StatusUnsupportedMediaType},
		{"xml rejected", http.MethodPost, "application/xml", `<a/>`, http.StatusUnsupportedMediaType},
		{"GET without body passes", http.MethodGet, "", "", http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			req := httptest.NewRequest(tc.method, "/v1/payments", strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set(httpapi.HeaderContentType, tc.contentType)
			}
			rec := httptest.NewRecorder()
			middleware.Chain(next, middleware.RequestID(), middleware.ContentType()).ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

// TestBodyLimitRejectsOversizeAndPAN covers both controls the stage owns.
func TestBodyLimitRejectsOversizeAndPAN(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		body       string
		max        int64
		wantStatus int
		wantCode   apierror.Code
	}{
		{"within limit", `{"a":"b"}`, 1024, http.StatusOK, ""},
		{"over limit", strings.Repeat("x", 200), 32, http.StatusRequestEntityTooLarge, apierror.CodeRequestTooLarge},
		{
			// A Luhn-valid test PAN. The rejection must name the field and never echo the value.
			"pan detected", `{"paymentMethod":{"token":"4111111111111111"}}`, 1024,
			http.StatusBadRequest, apierror.CodeSensitiveDataInRequest,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []byte
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = httpapi.RawBody(r.Context())
				w.WriteHeader(http.StatusOK)
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/payments", strings.NewReader(tc.body))
			req.Header.Set(httpapi.HeaderContentType, "application/json")
			rec := httptest.NewRecorder()
			middleware.Chain(next, middleware.RequestID(), middleware.BodyLimit(tc.max)).ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body)
			}
			if tc.wantCode == "" {
				if string(got) != tc.body {
					t.Errorf("buffered body = %q, want the exact bytes %q", got, tc.body)
				}
				return
			}
			var p httpapi.Problem
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
				t.Fatalf("not a problem document: %v", err)
			}
			if p.Code != string(tc.wantCode) {
				t.Errorf("code = %q, want %q", p.Code, tc.wantCode)
			}
			if strings.Contains(rec.Body.String(), "4111111111111111") {
				t.Error("the rejection echoed the card number back to the caller")
			}
		})
	}
}

// TestTenantIsolationRejectsOutOfScopeMerchant is the multi-tenancy guard.
//
// A merchant-scoped principal reaching another merchant's resource is the failure that this whole
// layer exists to prevent, and it must be caught before the handler so a handler cannot forget.
func TestTenantIsolationRejectsOutOfScopeMerchant(t *testing.T) {
	t.Parallel()
	const (
		mine     = "mrc_01JB8Z11111111111111111111"
		notMine  = "mrc_01JB8Z99999999999999999999"
		tenantID = "ten_01JB8Z00000000000000000000"
	)
	tests := []struct {
		name       string
		target     string
		scope      []shared.MerchantID
		wantStatus int
		wantCode   apierror.Code
	}{
		{"in scope", mine, []shared.MerchantID{mine}, http.StatusOK, ""},
		{"out of scope", notMine, []shared.MerchantID{mine}, http.StatusForbidden, apierror.CodeTenantMismatch},
		{"unscoped principal sees everything in its tenant", notMine, nil, http.StatusOK, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &authn.Principal{
				Method: authn.MethodJWT, Type: tenantctx.PrincipalMachine,
				ID: "cli_test", TenantID: tenantID, TenantTier: shared.TierPooled,
				Environment: shared.EnvironmentSandbox, Scopes: []string{"merchants:read"},
				MerchantScope: tc.scope,
			}
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			h := middleware.Chain(next,
				middleware.RequestID(),
				withTemplate(httpapi.RouteGetMerchant),
				withPrincipal(p),
				middleware.Tenant(nil),
			)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/merchants/"+tc.target, nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body)
			}
			if tc.wantCode == "" {
				return
			}
			var p2 httpapi.Problem
			_ = json.Unmarshal(rec.Body.Bytes(), &p2)
			if p2.Code != string(tc.wantCode) {
				t.Errorf("code = %q, want %q", p2.Code, tc.wantCode)
			}
		})
	}
}

// TestAuthenticationFailsClosed asserts that an unwired authenticator rejects rather than admits.
func TestAuthenticationFailsClosed(t *testing.T) {
	t.Parallel()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := middleware.Chain(next,
		middleware.RequestID(),
		withTemplate(httpapi.RouteGetPayment),
		middleware.Authenticate(nil, nil),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/payments/pay_x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — a missing authenticator must not admit traffic", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("401 carries no WWW-Authenticate header")
	}
}

// TestAuthorizationDeniesUnknownRoute asserts the fail-closed permission table: a route with no
// entry is denied rather than allowed.
func TestAuthorizationDeniesUnknownRoute(t *testing.T) {
	t.Parallel()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := middleware.Chain(next,
		middleware.RequestID(),
		withTemplate("/v1/something-new"),
		middleware.Authorize(allowAll{}, nil),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/something-new", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a route with no permission entry", rec.Code)
	}
}

// TestEveryContractRouteHasAPermissionAndLimit asserts the two policy tables agree with each
// other, so a route can never carry a rate limit and no authorization decision.
func TestEveryContractRouteHasAPermissionAndLimit(t *testing.T) {
	t.Parallel()
	limits := middleware.AllContractLimits()
	perms := middleware.AllPermissions()
	anonymous := middleware.AnonymousRoutes()

	for route := range perms {
		if _, ok := limits[route]; !ok {
			t.Errorf("%s has a permission but no rate limit", route)
		}
	}
	for route, limit := range limits {
		template := route[strings.IndexByte(route, ' ')+1:]
		if anonymous[template] || limit.Scope == middleware.ScopeNone {
			continue
		}
		if _, ok := perms[route]; !ok {
			t.Errorf("%s is rate-limited but carries no permission", route)
		}
	}
}

// TestRateLimitHeaders asserts the RateLimit-* family is emitted on both the allowed and the
// rejected path — a client that only learns its budget after exhausting it cannot pace itself.
func TestRateLimitHeaders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		decision   resilience.Decision
		wantStatus int
		wantRetry  string
	}{
		{
			"allowed",
			resilience.Decision{Allowed: true, Limit: 300, Remaining: 271, ResetAfter: time.Second},
			http.StatusOK, "",
		},
		{
			"rejected",
			resilience.Decision{Allowed: false, Limit: 300, Remaining: 0, ResetAfter: 2 * time.Second, RetryAfter: 2 * time.Second},
			http.StatusTooManyRequests, "2",
		},
		{
			"sub-second retry rounds up rather than to zero",
			resilience.Decision{Allowed: false, Limit: 300, Remaining: 0, ResetAfter: 400 * time.Millisecond, RetryAfter: 400 * time.Millisecond},
			http.StatusTooManyRequests, "1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			h := middleware.Chain(next,
				middleware.RequestID(),
				withTemplate(httpapi.RouteGetPayment),
				withTenant(),
				middleware.RateLimit(fixedLimiter{tc.decision}, middleware.ContractLimits{}),
			)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/payments/pay_01JB8Z9K2QW3E4R5T6Y7U8I9O0", nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := rec.Header().Get(httpapi.HeaderRateLimitLimit); got != "300" {
				t.Errorf("RateLimit-Limit = %q, want 300", got)
			}
			if rec.Header().Get(httpapi.HeaderRateLimitLeft) == "" {
				t.Error("RateLimit-Remaining is absent")
			}
			if rec.Header().Get(httpapi.HeaderRateLimitReset) == "" {
				t.Error("RateLimit-Reset is absent")
			}
			if got := rec.Header().Get(httpapi.HeaderRetryAfter); got != tc.wantRetry {
				t.Errorf("Retry-After = %q, want %q", got, tc.wantRetry)
			}
		})
	}
}

// TestRateLimiterFailureAdmits documents the one deliberate fail-open in the chain: a limiter
// that errors must not take the payments API down, because the failure of the limiter is not
// evidence that capacity is exhausted.
func TestRateLimiterFailureAdmits(t *testing.T) {
	t.Parallel()
	served := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	})
	h := middleware.Chain(next,
		middleware.RequestID(),
		withTemplate(httpapi.RouteGetPayment),
		withTenant(),
		middleware.RateLimit(brokenLimiter{}, middleware.ContractLimits{}),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/payments/pay_x", nil))
	if !served || rec.Code != http.StatusOK {
		t.Fatalf("a limiter error must admit the request; status = %d, served = %v", rec.Code, served)
	}
}

// TestConcurrencyShedsByPriority asserts that shedding is ordered by class rather than by arrival:
// a report is shed while a refund is admitted at the same pressure.
func TestConcurrencyShedsByPriority(t *testing.T) {
	t.Parallel()
	pressure := 0.80 // above P2/authorize and P4/report, below P1/capture and P0/money-out
	shedder := resilience.NewShedder(resilience.ShedderConfig{
		Pressure:   func() float64 { return pressure },
		Thresholds: resilience.DefaultShedThresholds,
		Hysteresis: 0,
	})
	tests := []struct {
		name       string
		method     string
		template   string
		path       string
		wantStatus int
	}{
		{"refund is never shed", http.MethodPost, httpapi.RouteRefundPayment, "/v1/payments/pay_x/refund", http.StatusOK},
		{"capture survives at this pressure", http.MethodPost, httpapi.RouteCapturePayment, "/v1/payments/pay_x/capture", http.StatusOK},
		{"payment creation is shed", http.MethodPost, httpapi.RouteCreatePayment, "/v1/payments", http.StatusServiceUnavailable},
		{"listing is shed", http.MethodGet, httpapi.RouteListPayments, "/v1/payments", http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			h := middleware.Chain(next,
				middleware.RequestID(),
				withPriority(tc.method, tc.template),
				middleware.Concurrency(nil, shedder),
			)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body)
			}
		})
	}
}

// TestIdempotencyMatrix is the replay / in-progress / mismatch matrix.
//
// These four outcomes are the whole contract of the middleware, and three of them are error paths
// that a manual test never exercises.
func TestIdempotencyMatrix(t *testing.T) {
	t.Parallel()
	const key = "6b3f1a52-9e7c-4b4f-8f4a-1c2d3e4f5a6b"

	tests := []struct {
		name        string
		header      string
		seed        func(*fakeIdemStore)
		handler     http.HandlerFunc
		wantStatus  int
		wantCode    apierror.Code
		wantReplay  bool
		wantHandler bool
	}{
		{
			name:        "missing key is 400",
			header:      "",
			wantStatus:  http.StatusBadRequest,
			wantCode:    apierror.CodeIdempotencyKeyRequired,
			wantHandler: false,
		},
		{
			name:        "new claim runs the handler",
			header:      key,
			handler:     jsonHandler(http.StatusCreated, `{"id":"pay_1"}`),
			wantStatus:  http.StatusCreated,
			wantHandler: true,
		},
		{
			name:   "replay returns the stored response verbatim",
			header: key,
			seed: func(s *fakeIdemStore) {
				s.outcome = ports.ClaimReplay
				s.snapshot = &ports.ResponseSnapshot{
					StatusCode: http.StatusCreated,
					Body:       []byte(`{"id":"pay_original"}`),
				}
			},
			handler:     jsonHandler(http.StatusCreated, `{"id":"pay_different"}`),
			wantStatus:  http.StatusCreated,
			wantReplay:  true,
			wantHandler: false,
		},
		{
			name:        "in progress is 409 with Retry-After and does not block",
			header:      key,
			seed:        func(s *fakeIdemStore) { s.outcome = ports.ClaimInProgress; s.retryAfter = time.Second },
			wantStatus:  http.StatusConflict,
			wantCode:    apierror.CodeIdempotentRequestInProgress,
			wantHandler: false,
		},
		{
			name:        "fingerprint mismatch is 422",
			header:      key,
			seed:        func(s *fakeIdemStore) { s.outcome = ports.ClaimFingerprintMismatch },
			wantStatus:  http.StatusUnprocessableEntity,
			wantCode:    apierror.CodeIdempotencyKeyReused,
			wantHandler: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeIdemStore{outcome: ports.ClaimNew}
			if tc.seed != nil {
				tc.seed(store)
			}
			mgr, err := idempotency.NewManager(store, idempotency.Config{
				Lease: time.Minute, Retention: time.Hour, RetryAfter: time.Second,
				Clock: shared.FixedClock{T: time.Unix(1700000000, 0).UTC()},
			})
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			ran := false
			handler := tc.handler
			if handler == nil {
				handler = jsonHandler(http.StatusCreated, `{}`)
			}
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ran = true
				handler(w, r)
			})
			h := middleware.Chain(next,
				middleware.RequestID(),
				withTemplate(httpapi.RouteCreatePayment),
				withTenant(),
				middleware.BodyLimit(0),
				middleware.Idempotency(mgr, 0),
			)
			req := httptest.NewRequest(http.MethodPost, "/v1/payments", strings.NewReader(`{"amount":1}`))
			req.Header.Set(httpapi.HeaderContentType, "application/json")
			if tc.header != "" {
				req.Header.Set(httpapi.HeaderIdempotencyKey, tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body)
			}
			if ran != tc.wantHandler {
				t.Errorf("handler ran = %v, want %v", ran, tc.wantHandler)
			}
			replay := rec.Header().Get(httpapi.HeaderIdempotentReply) == "true"
			if replay != tc.wantReplay {
				t.Errorf("Idempotent-Replay = %v, want %v", replay, tc.wantReplay)
			}
			if tc.wantReplay && !strings.Contains(rec.Body.String(), "pay_original") {
				t.Errorf("replay did not return the stored bytes: %s", rec.Body)
			}
			if tc.wantStatus == http.StatusConflict && rec.Header().Get(httpapi.HeaderRetryAfter) == "" {
				t.Error("409 IDEMPOTENT_REQUEST_IN_PROGRESS carries no Retry-After")
			}
			if tc.wantCode != "" {
				var p httpapi.Problem
				_ = json.Unmarshal(rec.Body.Bytes(), &p)
				if p.Code != string(tc.wantCode) {
					t.Errorf("code = %q, want %q", p.Code, tc.wantCode)
				}
			}
		})
	}
}

// TestIdempotencySettlement asserts the Complete / FailTerminal / Release fork.
//
// The distinction is the subtle part of the whole middleware: a declined payment is terminal and
// must replay, a 503 is not terminal and must release so the client's retry is a real attempt.
func TestIdempotencySettlement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		status     int
		wantAction string
	}{
		{"2xx completes", http.StatusCreated, "complete"},
		{"422 is terminal", http.StatusUnprocessableEntity, "fail_terminal"},
		{"409 is terminal", http.StatusConflict, "fail_terminal"},
		{"500 is terminal — a bug reproduces", http.StatusInternalServerError, "fail_terminal"},
		{"502 releases", http.StatusBadGateway, "release"},
		{"503 releases", http.StatusServiceUnavailable, "release"},
		{"504 releases", http.StatusGatewayTimeout, "release"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeIdemStore{outcome: ports.ClaimNew}
			mgr, err := idempotency.NewManager(store, idempotency.Config{
				Lease: time.Minute, Retention: time.Hour, RetryAfter: time.Second,
				Clock: shared.FixedClock{T: time.Unix(1700000000, 0).UTC()},
			})
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			h := middleware.Chain(jsonHandler(tc.status, `{"ok":true}`),
				middleware.RequestID(),
				withTemplate(httpapi.RouteCreatePayment),
				withTenant(),
				middleware.BodyLimit(0),
				middleware.Idempotency(mgr, 0),
			)
			req := httptest.NewRequest(http.MethodPost, "/v1/payments", strings.NewReader(`{"a":1}`))
			req.Header.Set(httpapi.HeaderContentType, "application/json")
			req.Header.Set(httpapi.HeaderIdempotencyKey, "k1")
			h.ServeHTTP(httptest.NewRecorder(), req)

			if got := store.lastAction(); got != tc.wantAction {
				t.Errorf("settled with %q, want %q", got, tc.wantAction)
			}
		})
	}
}

// TestRecorderPassesThroughFlusherAndHijacker is the wrapper's capability contract. A wrapper
// that loses Flush turns a streamed response into one lump at the end; one that loses Hijack
// makes every connection upgrade a 500.
func TestRecorderPassesThroughFlusherAndHijacker(t *testing.T) {
	t.Parallel()
	inner := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	rec := middleware.NewRecorder(inner)

	if _, ok := any(rec).(http.Flusher); !ok {
		t.Fatal("recorder does not implement http.Flusher")
	}
	if _, ok := any(rec).(http.Hijacker); !ok {
		t.Fatal("recorder does not implement http.Hijacker")
	}
	rec.Flush()
	if !inner.flushed {
		t.Error("Flush did not reach the underlying writer")
	}
	if _, _, err := rec.Hijack(); err == nil {
		t.Error("Hijack on a non-hijackable writer must report an error, not succeed")
	}
	if rec.Unwrap() != http.ResponseWriter(inner) {
		t.Error("Unwrap does not expose the wrapped writer")
	}
}

// TestRecorderBufferCap asserts an oversized response is not accumulated in memory.
func TestRecorderBufferCap(t *testing.T) {
	t.Parallel()
	rec := middleware.NewRecorder(httptest.NewRecorder())
	rec.Buffer(16)
	_, _ = rec.Write([]byte(strings.Repeat("x", 64)))

	body, complete := rec.Body()
	if complete {
		t.Error("an over-cap response reported itself as completely captured")
	}
	if len(body) != 0 {
		t.Errorf("over-cap buffer retained %d bytes; it must retain none", len(body))
	}
}

// --- helpers -------------------------------------------------------------------------------------

func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpapi.HeaderContentType, httpapi.MediaJSON)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// withTemplate stamps a route template on the context, standing in for the tracing stage in tests
// that exercise a single middleware.
func withTemplate(template string) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(httpapi.WithRouteTemplate(r.Context(), template)))
		})
	}
}

func withPriority(method, template string) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := httpapi.WithRouteTemplate(r.Context(), template)
			ctx = httpapi.WithPriority(ctx, httpapi.PriorityOfRoute(method, template))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func withPrincipal(p *authn.Principal) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(httpapi.WithPrincipal(r.Context(), p)))
		})
	}
}

// withTenant installs a fixed tenant context, standing in for the authn+tenant stages.
func withTenant() middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, err := tenantctx.WithTenant(r.Context(), tenantctx.TenantContext{
				TenantID:    "ten_01JB8Z00000000000000000000",
				Tier:        shared.TierPooled,
				Environment: shared.EnvironmentSandbox,
				Principal:   tenantctx.Principal{Type: tenantctx.PrincipalMachine, ID: "cli_test"},
				Scopes:      []string{"payments:read", "payments:write"},
				RequestID:   httpapi.RequestID(r.Context()),
				Source:      tenantctx.SourceToken,
			})
			if err != nil {
				httpapi.WriteProblem(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

type allowAll struct{}

func (allowAll) Evaluate(context.Context, authz.Request) authz.Decision {
	return authz.Decision{Allow: true}
}

type fixedLimiter struct{ d resilience.Decision }

func (f fixedLimiter) Allow(context.Context, string, resilience.Limit) (resilience.Decision, error) {
	return f.d, nil
}

type brokenLimiter struct{}

func (brokenLimiter) Allow(context.Context, string, resilience.Limit) (resilience.Decision, error) {
	return resilience.Decision{}, apierror.New(apierror.CodeDependencyFailure, "redis is unreachable")
}

// fakeIdemStore is a ports.IdempotencyStore that answers a scripted outcome and records which
// settlement method the middleware chose.
type fakeIdemStore struct {
	mu         sync.Mutex
	outcome    ports.ClaimOutcome
	snapshot   *ports.ResponseSnapshot
	retryAfter time.Duration
	action     string
}

func (s *fakeIdemStore) Claim(context.Context, ports.IdempotencyRecord) (ports.ClaimResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ports.ClaimResult{Outcome: s.outcome, Snapshot: s.snapshot, RetryAfter: s.retryAfter}, nil
}

func (s *fakeIdemStore) Complete(context.Context, ports.IdempotencyKey, ports.ResponseSnapshot) error {
	s.record("complete")
	return nil
}

func (s *fakeIdemStore) FailTerminal(context.Context, ports.IdempotencyKey, ports.ResponseSnapshot) error {
	s.record("fail_terminal")
	return nil
}

func (s *fakeIdemStore) Release(context.Context, ports.IdempotencyKey) error {
	s.record("release")
	return nil
}

func (s *fakeIdemStore) PurgeExpired(context.Context, time.Time, int) (int, error) { return 0, nil }

func (s *fakeIdemStore) record(a string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.action = a
}

func (s *fakeIdemStore) lastAction() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.action
}

// flushRecorder is an httptest.ResponseRecorder that reports whether Flush reached it. It
// deliberately does not implement http.Hijacker, so the Hijack path is exercised too.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flushRecorder) Flush() { f.flushed = true }
