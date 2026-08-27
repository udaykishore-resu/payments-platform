package httpx_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// TestTimeoutBecomesAFlaggedResponse pins the split every adapter depends on.
//
// A request that was written and then timed out is a *response* with Timeout set, because the
// gateway may have acted. A request that never left the process is an *error*, because it provably
// did not. Collapsing the two here would make every adapter above wrong at once — in one direction
// double charges, in the other needless reconciliation.
func TestTimeoutBecomesAFlaggedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := httpx.New(httpx.Options{Timeout: 150 * time.Millisecond})
	resp, err := c.Do(&spi.HTTPRequest{Ctx: context.Background(), Method: http.MethodGet, URL: srv.URL})
	if err != nil {
		t.Fatalf("a timeout was returned as an error (%v); the adapter can no longer tell it from a refused connection", err)
	}
	if resp == nil || !resp.Timeout {
		t.Fatalf("a timeout did not set Timeout on the response: %+v", resp)
	}
}

// TestRefusedConnectionBecomesAnError is the other half of the same rule.
func TestRefusedConnectionBecomesAnError(t *testing.T) {
	// Bind and immediately close, so the port is almost certainly refusing.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	c := httpx.New(httpx.Options{Timeout: 2 * time.Second})
	resp, err := c.Do(&spi.HTTPRequest{Ctx: context.Background(), Method: http.MethodGet, URL: "http://" + addr})
	if err == nil {
		t.Fatalf("a refused connection produced a response rather than an error: %+v", resp)
	}
	if resp != nil && resp.Timeout {
		t.Fatal("a refused connection was flagged as a timeout; the payment would be parked in reconciliation for nothing")
	}
	if apierror.CodeOf(err) != apierror.CodeGatewayUnavailable {
		t.Fatalf("code = %s, want %s", apierror.CodeOf(err), apierror.CodeGatewayUnavailable)
	}
}

// TestNonSuccessStatusIsNotAnError proves the transport leaves interpretation to the adapter.
// Gateways express declines, idempotency conflicts and rate limits as HTTP statuses, and only the
// adapter knows which is which.
func TestNonSuccessStatusIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"type":"card_error"}}`))
	}))
	defer srv.Close()

	c := httpx.New(httpx.Options{Timeout: time.Second})
	resp, err := c.Do(&spi.HTTPRequest{Ctx: context.Background(), Method: http.MethodGet, URL: srv.URL})
	if err != nil {
		t.Fatalf("a 402 was returned as an error: %v", err)
	}
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", resp.StatusCode)
	}
}

// TestBodyCapsAreEnforced covers both directions. An unbounded response read is an out-of-memory
// kill, and a pod that dies mid-authorization leaves exactly the ambiguous state this platform is
// built to avoid.
func TestBodyCapsAreEnforced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 4096))
	}))
	defer srv.Close()

	c := httpx.New(httpx.Options{Timeout: time.Second, MaxResponseBytes: 1024})
	_, err := c.Do(&spi.HTTPRequest{Ctx: context.Background(), Method: http.MethodGet, URL: srv.URL})
	if err == nil {
		t.Fatal("an over-cap response body was accepted")
	}
	if apierror.CodeOf(err) != apierror.CodeGatewayContractViolation {
		t.Fatalf("code = %s, want %s", apierror.CodeOf(err), apierror.CodeGatewayContractViolation)
	}

	c2 := httpx.New(httpx.Options{Timeout: time.Second, MaxRequestBytes: 16})
	_, err = c2.Do(&spi.HTTPRequest{
		Ctx: context.Background(), Method: http.MethodPost, URL: srv.URL, Body: make([]byte, 1024),
	})
	if err == nil {
		t.Fatal("an over-cap request body was sent")
	}
	if apierror.CodeOf(err) != apierror.CodeRequestTooLarge {
		t.Fatalf("code = %s, want %s", apierror.CodeOf(err), apierror.CodeRequestTooLarge)
	}
}

// TestLatencyHookFiresExactlyOnce proves the observability seam is reliable, including on failure —
// a hook that fires only on success produces a latency histogram that looks healthy during an
// outage.
func TestLatencyHookFiresExactlyOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var samples []httpx.LatencySample
	c := httpx.New(httpx.Options{
		GatewayID: "stripe",
		Timeout:   time.Second,
		OnLatency: func(s httpx.LatencySample) { samples = append(samples, s) },
	})
	_, err := c.Do(&spi.HTTPRequest{
		Ctx: context.Background(), Method: http.MethodGet, URL: srv.URL,
		Headers: map[string]string{httpx.OperationHeader: "authorize"},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("the latency hook fired %d times, want 1", len(samples))
	}
	if samples[0].Operation != "authorize" {
		t.Fatalf("operation label = %q, want %q", samples[0].Operation, "authorize")
	}
	if samples[0].StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", samples[0].StatusCode)
	}
	if strings.Contains(samples[0].Host, "/") {
		t.Fatalf("the sample carries a path in Host (%q), which would make a high-cardinality metric label", samples[0].Host)
	}
}

// TestOperationPseudoHeaderIsStripped proves the platform's internal label never reaches a vendor.
func TestOperationPseudoHeaderIsStripped(t *testing.T) {
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
	}))
	defer srv.Close()

	c := httpx.New(httpx.Options{Timeout: time.Second})
	_, err := c.Do(&spi.HTTPRequest{
		Ctx: context.Background(), Method: http.MethodGet, URL: srv.URL,
		Headers: map[string]string{httpx.OperationHeader: "authorize", "Accept": "application/json"},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if seen.Get(httpx.OperationHeader) != "" {
		t.Fatalf("the %s pseudo-header reached the gateway", httpx.OperationHeader)
	}
	if seen.Get("Accept") != "application/json" {
		t.Fatal("a real header was dropped")
	}
}

// TestCancelledContextIsAnsweredWithoutSending proves the client does not write a request it
// already knows is dead — which is what makes a cancelled call a plain error rather than an
// ambiguous one.
func TestCancelledContextIsAnsweredWithoutSending(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := httpx.New(httpx.Options{Timeout: time.Second})
	if _, err := c.Do(&spi.HTTPRequest{Ctx: ctx, Method: http.MethodGet, URL: srv.URL}); err == nil {
		t.Fatal("a cancelled context produced no error")
	}
	if hits != 0 {
		t.Fatal("a request was sent despite a cancelled context")
	}
}

// TestSafeHeadersRedactsCredentialBearingNames pins the only header-rendering function in the
// package. There is deliberately no SafeBody: a gateway request body carries payment tokens and, in
// some vendors' flows, a raw PAN, and there is no safe rendering of it.
func TestSafeHeadersRedactsCredentialBearingNames(t *testing.T) {
	in := map[string]string{
		"Authorization":    "Bearer sk_test_FAKE_should_never_be_printed",
		"authorization":    "Bearer lowercase_variant",
		"X-API-Key":        "adyen_key_should_never_be_printed",
		"Stripe-Signature": "t=1,v1=deadbeef",
		"Stripe-Version":   "2026-06-30.acacia",
		"Content-Type":     "application/json",
	}
	out := httpx.SafeHeaders(in)
	for _, name := range []string{"Authorization", "authorization", "X-API-Key", "Stripe-Signature"} {
		if out[name] != "[REDACTED]" {
			t.Errorf("%s = %q, want [REDACTED]", name, out[name])
		}
	}
	if out["Stripe-Version"] != "2026-06-30.acacia" {
		t.Error("a non-credential header was redacted, which makes the output useless for debugging")
	}
	if out["Content-Type"] != "application/json" {
		t.Error("Content-Type was redacted")
	}
	if in["Authorization"] == "[REDACTED]" {
		t.Error("SafeHeaders mutated its input")
	}
}

// TestIsTimeoutRecognisesEveryShape covers the three unrelated types a timeout arrives as. Missing
// any one makes a slow gateway look like a refused one, and a refused one is safe to retry.
func TestIsTimeoutRecognisesEveryShape(t *testing.T) {
	if !httpx.IsTimeout(context.DeadlineExceeded) {
		t.Error("context.DeadlineExceeded was not recognised as a timeout")
	}
	if !httpx.IsTimeout(&net.OpError{Op: "dial", Err: &timeoutError{}}) {
		t.Error("a net.Error reporting Timeout was not recognised")
	}
	if !httpx.IsTimeout(errors.New(`Get "https://x": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`)) {
		t.Error("the http.Client timeout string was not recognised")
	}
	if httpx.IsTimeout(errors.New("connection refused")) {
		t.Error("a refused connection was misclassified as a timeout")
	}
	if httpx.IsTimeout(nil) {
		t.Error("nil was classified as a timeout")
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// --- RecordingDoer ---------------------------------------------------------------------------

// TestRecordingDoerMatchesAndRecords covers the double's two jobs: playing back a script, and
// keeping every request so a test can assert on what the adapter *sent*.
func TestRecordingDoerMatchesAndRecords(t *testing.T) {
	d := httpx.NewRecordingDoer(
		httpx.Exchange{Match: httpx.MatchPath("/token"), Response: httpx.JSONResponse(200, `{"access_token":"t"}`)},
		httpx.Exchange{Match: httpx.MatchMethodPath("POST", "/orders"), Response: httpx.JSONResponse(201, `{"id":"o1"}`), Times: 1},
	)

	if _, err := d.Do(&spi.HTTPRequest{Ctx: context.Background(), Method: "POST", URL: "https://x/token"}); err != nil {
		t.Fatalf("token: %v", err)
	}
	resp, err := d.Do(&spi.HTTPRequest{
		Ctx: context.Background(), Method: "POST", URL: "https://x/orders",
		Headers: map[string]string{"Idempotency-Key": "k1", httpx.OperationHeader: "authorize"},
		Body:    []byte(`{"amount":2500}`),
	})
	if err != nil {
		t.Fatalf("orders: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	// The exchange was scripted for one use; a second call must fail loudly rather than silently
	// replay, so a test that expects no retry actually proves it.
	if _, err := d.Do(&spi.HTTPRequest{Ctx: context.Background(), Method: "POST", URL: "https://x/orders"}); err == nil {
		t.Fatal("a used-up exchange answered a second time")
	}

	rec, ok := d.FirstMatching(func(r httpx.RecordedRequest) bool { return r.Operation == "authorize" })
	if !ok {
		t.Fatal("the authorize request was not recorded")
	}
	if rec.Header("idempotency-key") != "k1" {
		t.Fatalf("header lookup is case-sensitive; got %q", rec.Header("idempotency-key"))
	}
	if rec.BodyString() != `{"amount":2500}` {
		t.Fatalf("body = %q", rec.BodyString())
	}
	if d.Count() != 3 {
		t.Fatalf("recorded %d requests, want 3", d.Count())
	}
}

// TestRecordingDoerFailsOnAnUnscriptedRequest proves an adapter cannot quietly call an endpoint
// nobody wrote a fixture for — which is to say, an endpoint nobody has reviewed.
func TestRecordingDoerFailsOnAnUnscriptedRequest(t *testing.T) {
	d := httpx.NewRecordingDoer()
	if _, err := d.Do(&spi.HTTPRequest{Ctx: context.Background(), Method: "GET", URL: "https://x/unexpected"}); err == nil {
		t.Fatal("an unscripted request was answered")
	}
}

// TestRecordingDoerStringRedacts proves the double's own diagnostics do not undo the control the
// rest of the package implements: a test helper that dumps an Authorization header into a CI log
// has leaked it just as thoroughly as production code would.
func TestRecordingDoerStringRedacts(t *testing.T) {
	d := httpx.NewRecordingDoer(httpx.Exchange{Response: httpx.JSONResponse(200, `{}`)})
	_, _ = d.Do(&spi.HTTPRequest{
		Ctx: context.Background(), Method: "POST", URL: "https://x/pay",
		Headers: map[string]string{"Authorization": "Bearer sk_test_FAKE_leaked"},
		Body:    []byte(`{"card":"4242424242424242"}`),
	})
	rendered := d.String()
	if strings.Contains(rendered, "sk_test_FAKE_leaked") {
		t.Fatal("the doer rendered an Authorization header")
	}
	if strings.Contains(rendered, "4242424242424242") {
		t.Fatal("the doer rendered a request body")
	}
}

// TestRecordingDoerIsSafeUnderConcurrency: adapters are exercised under -race, and a double that
// assumed single-threaded use would produce flaky failures blamed on the adapter.
func TestRecordingDoerIsSafeUnderConcurrency(t *testing.T) {
	d := httpx.NewRecordingDoer().WithFallback(httpx.Exchange{Response: httpx.JSONResponse(200, `{}`)})
	const n = 32
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			_, _ = d.Do(&spi.HTTPRequest{Ctx: context.Background(), Method: "GET", URL: "https://x/p"})
			done <- struct{}{}
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
	if d.Count() != n {
		t.Fatalf("recorded %d of %d concurrent requests", d.Count(), n)
	}
}
