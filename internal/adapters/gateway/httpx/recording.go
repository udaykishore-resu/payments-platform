package httpx

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// RecordingDoer is the test double every adapter is exercised against.
//
// It is in the production package rather than a _test.go file for two reasons. First, the shared
// contract suite lives in another package and has to construct one. Second — and this is the part
// that matters — a gateway adapter's correctness is almost entirely about what it *sends*, and a
// double that only plays back responses lets an adapter pass its tests while sending the
// idempotency key in the wrong header. RecordingDoer therefore keeps every request verbatim and
// exposes assertions over them.
//
// It is safe for concurrent use: adapters are called from many goroutines in the race-detector
// runs, and a doubles-are-single-threaded assumption would produce flaky failures that get
// blamed on the adapter.
type RecordingDoer struct {
	mu        sync.Mutex
	exchanges []*scriptedExchange
	requests  []RecordedRequest
	fallback  *Exchange
	// clock lets a test attribute a synthetic latency to a scripted response without sleeping.
	now func() time.Time
}

// Exchange is one scripted request/response pair.
//
// Match is what makes the script order-independent. A gateway adapter legitimately issues calls
// in an order the test should not have to predict — PayPal fetches an OAuth token before its
// first order, and only before its first order — so matching on the request rather than on
// position keeps the script readable and keeps a test from breaking when an adapter starts
// caching something.
type Exchange struct {
	// Match selects the requests this exchange answers. Nil matches anything.
	Match func(*spi.HTTPRequest) bool
	// Response is played back with its body copied, so an adapter that mutates the slice it is
	// handed cannot corrupt a later replay.
	Response *spi.HTTPResponse
	// Err is returned instead of Response, modelling a pre-flight transport failure: a refused
	// connection, a DNS miss, a rejected TLS handshake. The adapters must treat these as
	// "the gateway provably did not act".
	Err error
	// Times limits how often this exchange may be used. Zero means unlimited, which is the right
	// default for a token endpoint or a lookup; set it to 1 to prove an adapter did not retry.
	Times int
	// Latency is reported on the played-back response, so a test can assert an adapter propagates
	// the observed latency into spi.Result without any real waiting.
	Latency time.Duration
}

type scriptedExchange struct {
	spec Exchange
	used int
}

// RecordedRequest is a request the adapter issued, kept verbatim.
type RecordedRequest struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
	// Operation is the adapter's own label, taken from the OperationHeader pseudo-header before
	// it would have been stripped. It makes an assertion read "the capture call carried the key"
	// rather than "the third request carried the key".
	Operation string
}

// Header returns a request header case-insensitively, which is how HTTP header lookup actually
// works and how an assertion should therefore be written.
func (r RecordedRequest) Header(name string) string {
	for k, v := range r.Headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// HasHeader reports whether the request carried the named header with a non-empty value.
func (r RecordedRequest) HasHeader(name string) bool { return r.Header(name) != "" }

// BodyString renders the body for assertions. It is only ever called from tests; production code
// must not render a gateway request body anywhere.
func (r RecordedRequest) BodyString() string { return string(r.Body) }

var _ spi.HTTPDoer = (*RecordingDoer)(nil)

// NewRecordingDoer builds a doer from a script.
//
// A request that matches no exchange returns an error naming the unmatched request rather than a
// zero response. A silent zero response is how an adapter test passes while the adapter calls an
// endpoint nobody scripted — which is to say, an endpoint nobody has reviewed.
func NewRecordingDoer(exchanges ...Exchange) *RecordingDoer {
	d := &RecordingDoer{now: time.Now}
	for i := range exchanges {
		e := exchanges[i]
		d.exchanges = append(d.exchanges, &scriptedExchange{spec: e})
	}
	return d
}

// WithFallback sets the exchange used when nothing else matches. Use it sparingly: an explicit
// script is a specification, and a permissive fallback turns it back into a shrug.
func (d *RecordingDoer) WithFallback(e Exchange) *RecordingDoer {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fallback = &e
	return d
}

// Append adds an exchange after construction, for a test that reconfigures mid-scenario.
func (d *RecordingDoer) Append(e Exchange) *RecordingDoer {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.exchanges = append(d.exchanges, &scriptedExchange{spec: e})
	return d
}

// Reset clears the recorded requests and the per-exchange use counts, so one doer can drive
// several phases of a scenario without the assertions from phase one bleeding into phase two.
func (d *RecordingDoer) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = nil
	for _, e := range d.exchanges {
		e.used = 0
	}
}

// Do implements spi.HTTPDoer.
func (d *RecordingDoer) Do(req *spi.HTTPRequest) (*spi.HTTPResponse, error) {
	if req == nil {
		return nil, apierror.New(apierror.CodeInternalError, "httpx: nil request")
	}
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	// The double honours cancellation exactly as the real client does, so a contract assertion
	// about cancellation exercises the same code path in the adapter either way.
	if err := ctx.Err(); err != nil {
		return nil, classifyContext(err)
	}

	rec := RecordedRequest{
		Method:    req.Method,
		URL:       req.URL,
		Headers:   copyHeaders(req.Headers),
		Body:      append([]byte(nil), req.Body...),
		Operation: req.Headers[OperationHeader],
	}

	d.mu.Lock()
	d.requests = append(d.requests, rec)
	var chosen *scriptedExchange
	for _, e := range d.exchanges {
		if e.spec.Times > 0 && e.used >= e.spec.Times {
			continue
		}
		if e.spec.Match != nil && !e.spec.Match(req) {
			continue
		}
		chosen = e
		break
	}
	var spec Exchange
	switch {
	case chosen != nil:
		chosen.used++
		spec = chosen.spec
	case d.fallback != nil:
		spec = *d.fallback
	default:
		d.mu.Unlock()
		return nil, apierror.Newf(apierror.CodeInternalError,
			"httpx: no scripted exchange matched %s %s", req.Method, req.URL)
	}
	d.mu.Unlock()

	if spec.Err != nil {
		return nil, spec.Err
	}
	if spec.Response == nil {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"httpx: scripted exchange for %s %s has neither a response nor an error",
			req.Method, req.URL)
	}
	return cloneResponse(spec.Response, spec.Latency), nil
}

// Requests returns a copy of everything the adapter sent, in order.
func (d *RecordingDoer) Requests() []RecordedRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]RecordedRequest(nil), d.requests...)
}

// Count returns the number of requests issued.
func (d *RecordingDoer) Count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.requests)
}

// CountMatching returns how many recorded requests satisfy pred. It is the assertion behind
// "the adapter did not charge twice": script one exchange, call twice, expect one matching
// mutating request.
func (d *RecordingDoer) CountMatching(pred func(RecordedRequest) bool) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, r := range d.requests {
		if pred(r) {
			n++
		}
	}
	return n
}

// Last returns the most recent request and whether there was one.
func (d *RecordingDoer) Last() (RecordedRequest, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.requests) == 0 {
		return RecordedRequest{}, false
	}
	return d.requests[len(d.requests)-1], true
}

// FirstMatching returns the earliest request satisfying pred.
func (d *RecordingDoer) FirstMatching(pred func(RecordedRequest) bool) (RecordedRequest, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.requests {
		if pred(r) {
			return r, true
		}
	}
	return RecordedRequest{}, false
}

// String renders the script's traffic for a failure message, with credential headers redacted and
// bodies omitted. A test double that dumps an Authorization header into a CI log has undone the
// control the rest of this package implements.
func (d *RecordingDoer) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var b strings.Builder
	b.WriteString("RecordingDoer{")
	for i, r := range d.requests {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s %s", r.Method, r.URL)
	}
	b.WriteString("}")
	return b.String()
}

// --- scripted response constructors ----------------------------------------------------------

// JSONResponse builds a scripted JSON response.
func JSONResponse(status int, body string) *spi.HTTPResponse {
	return &spi.HTTPResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       []byte(body),
	}
}

// FormResponse builds a scripted response with an arbitrary content type, for vendors that do not
// answer in JSON.
func FormResponse(status int, contentType, body string) *spi.HTTPResponse {
	return &spi.HTTPResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": contentType},
		Body:       []byte(body),
	}
}

// TimeoutResponse is the shape the real client produces when a request was written and the
// deadline then expired. Adapters must turn this into spi.ErrOutcomeUnknown for money-moving
// operations; the contract suite asserts exactly that, for every such operation.
func TimeoutResponse() *spi.HTTPResponse {
	return &spi.HTTPResponse{Timeout: true}
}

// ConnectionRefused is the shape the real client produces when the request never left the
// process. It is an error rather than a response precisely so that an adapter cannot confuse it
// with a timeout: nothing was sent, so nothing happened, so the operation is safe to retry.
func ConnectionRefused() error {
	return apierror.New(apierror.CodeGatewayUnavailable,
		"httpx: connection refused (scripted)")
}

// MatchPath matches any request whose URL path contains the given substring, which is how a test
// names an endpoint without having to reproduce the base URL and the query string.
func MatchPath(substr string) func(*spi.HTTPRequest) bool {
	return func(r *spi.HTTPRequest) bool { return strings.Contains(r.URL, substr) }
}

// MatchMethodPath narrows MatchPath by HTTP method, which is what separates "create the order"
// from "read the order" at the same URL prefix.
func MatchMethodPath(method, substr string) func(*spi.HTTPRequest) bool {
	return func(r *spi.HTTPRequest) bool {
		return strings.EqualFold(r.Method, method) && strings.Contains(r.URL, substr)
	}
}

// MatchOperation matches on the adapter's own operation label, which survives a change to a
// vendor's URL layout.
func MatchOperation(op string) func(*spi.HTTPRequest) bool {
	return func(r *spi.HTTPRequest) bool { return r.Headers[OperationHeader] == op }
}

// MatchAll requires every predicate to hold.
func MatchAll(preds ...func(*spi.HTTPRequest) bool) func(*spi.HTTPRequest) bool {
	return func(r *spi.HTTPRequest) bool {
		for _, p := range preds {
			if p != nil && !p(r) {
				return false
			}
		}
		return true
	}
}

func copyHeaders(h map[string]string) map[string]string {
	if h == nil {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}

func cloneResponse(r *spi.HTTPResponse, latency time.Duration) *spi.HTTPResponse {
	out := &spi.HTTPResponse{
		StatusCode: r.StatusCode,
		Headers:    copyHeaders(r.Headers),
		Body:       append([]byte(nil), r.Body...),
		Timeout:    r.Timeout,
		Latency:    r.Latency,
	}
	if latency > 0 {
		out.Latency = latency
	}
	return out
}
