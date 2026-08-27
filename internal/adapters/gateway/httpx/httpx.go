// Package httpx is the shared transport every gateway adapter dials through.
//
// It exists so that exactly one file in the repository owns the answers to the questions that
// decide whether a gateway integration survives a bad afternoon: how many connections we keep
// open to a vendor, how long we wait for the first response byte, how big a body we are willing
// to read, and — most importantly — how a transport failure is classified. An adapter that
// builds its own http.Client is an adapter outside that envelope, and it is invariably the one
// that exhausts file descriptors or turns a slow gateway into a double charge.
//
// The one classification rule worth reading twice: a request that was written and then timed out
// produces a *response* with Timeout set, not an error. A request that never left the process —
// DNS failure, refused connection, TLS handshake rejection — produces an error. The adapters
// depend on that split to decide between spi.ErrOutcomeUnknown ("money may have moved") and a
// plain retryable error ("the gateway provably did not act"). Collapsing the two here would make
// every adapter above wrong at once, which is why the split lives at the bottom of the stack
// rather than being re-derived in three places.
//
// Nothing in this package logs a request body or an Authorization header. See the comment on
// SafeHeaders: the redaction is structural — the only header-rendering function in the package
// drops credential-bearing names — rather than a discipline each caller has to remember.
package httpx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Default transport limits.
//
// These are the values a payment adapter wants rather than the values net/http ships with, and
// each one is a decision:
//
//   - DefaultMaxIdleConnsPerHost is 64, against net/http's 2. Two idle connections per host means
//     a service doing a few hundred authorizations a second spends most of its gateway latency
//     budget in TLS handshakes, and the handshake cost lands on the payer. 64 is sized for one
//     pod's steady-state concurrency to a single vendor host; the bulkhead above caps concurrency
//     well below it, so the pool is never the binding constraint.
//   - DefaultIdleConnTimeout is 90s, matching the idle timeout the major gateways' load balancers
//     use. Holding a connection longer than the far end does produces a race where we write a
//     request onto a socket the peer is closing, which surfaces as a spurious EOF on a
//     money-moving call — the worst possible place for an ambiguous failure.
//   - DefaultTLSHandshakeTimeout is 5s. A handshake that has not completed in five seconds is a
//     network problem, and failing fast here is safe because nothing has been sent.
//   - DefaultResponseHeaderTimeout is deliberately *not* the whole-request timeout: it bounds the
//     wait for the first response byte, which is the part of the call that indicates the gateway
//     is wedged. The overall deadline comes from the caller's context, because the orchestrator
//     owns the timeout cascade.
const (
	DefaultMaxIdleConnsPerHost   = 64
	DefaultIdleConnTimeout       = 90 * time.Second
	DefaultTLSHandshakeTimeout   = 5 * time.Second
	DefaultResponseHeaderTimeout = 10 * time.Second
	DefaultExpectContinueTimeout = 1 * time.Second

	// DefaultMaxRequestBytes caps what an adapter may send. A gateway request is a few kilobytes;
	// anything approaching a megabyte means a caller has put a document, a log or an entire
	// customer record into a field, and shipping it to a third party is a data-protection
	// incident as well as a bug.
	DefaultMaxRequestBytes int64 = 1 << 20 // 1 MiB

	// DefaultMaxResponseBytes caps what we read back. Unbounded io.ReadAll against a hostile or
	// malfunctioning peer is an out-of-memory kill, and a pod that OOMs mid-authorization leaves
	// exactly the ambiguous state this platform is built to avoid.
	DefaultMaxResponseBytes int64 = 8 << 20 // 8 MiB
)

// LatencySample is one observation handed to the latency hook.
//
// It carries no body and no headers, on purpose: the hook's consumers are a histogram and a span,
// and neither has any business being able to see a card token or an API key. Making the hook
// structurally incapable of leaking is cheaper than auditing every implementation of it.
type LatencySample struct {
	GatewayID shared.GatewayID
	// Operation is the adapter's own label for the call ("authorize", "capture"), used as a metric
	// dimension. It is supplied by the adapter through the X-PP-Operation pseudo-header, which is
	// stripped before the request is sent.
	Operation string
	Method    string
	// Host is the request's host. The full URL is not carried because gateway URLs embed
	// identifiers (a PaymentIntent id, a pspReference) that make a high-cardinality metric label.
	Host string
	// StatusCode is 0 when no response was received.
	StatusCode int
	Duration   time.Duration
	// Timeout reports the classification this package applied, so a dashboard can separate
	// "the gateway was slow" from "the gateway said no".
	Timeout bool
	// Err is the transport error, if any. Transport errors from net/http never contain request
	// bodies or headers, so this is safe to record; it is still the caller's job not to render it
	// into a customer-visible message.
	Err error
}

// OperationHeader is the pseudo-header an adapter sets to label a call for the latency hook. It
// is removed before the request is written, so it never reaches a vendor.
const OperationHeader = "X-PP-Operation"

// Options configures a per-gateway client.
//
// One Client per gateway, not one per process: connection pools, idle timeouts and the latency
// hook are all things an operator wants to reason about for "our Stripe traffic" rather than for
// "our outbound HTTP", and sharing a pool across vendors means a wedged gateway's connections
// starve a healthy one.
type Options struct {
	GatewayID shared.GatewayID

	// Timeout is the whole-request ceiling applied when the caller's context carries no earlier
	// deadline. Zero leaves the client with no ceiling of its own and relies entirely on the
	// context, which is the normal case under the orchestrator.
	Timeout time.Duration

	MaxIdleConnsPerHost   int
	IdleConnTimeout       time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	ExpectContinueTimeout time.Duration

	MaxRequestBytes  int64
	MaxResponseBytes int64

	// OnLatency is invoked exactly once per Do, including on failure. It must not block: it is
	// called on the caller's goroutine, inside the payment's latency budget.
	OnLatency func(LatencySample)

	// Transport allows a test or a mTLS deployment to supply its own round tripper. When nil the
	// pool described by the fields above is built.
	Transport http.RoundTripper
}

func (o Options) withDefaults() Options {
	if o.MaxIdleConnsPerHost <= 0 {
		o.MaxIdleConnsPerHost = DefaultMaxIdleConnsPerHost
	}
	if o.IdleConnTimeout <= 0 {
		o.IdleConnTimeout = DefaultIdleConnTimeout
	}
	if o.TLSHandshakeTimeout <= 0 {
		o.TLSHandshakeTimeout = DefaultTLSHandshakeTimeout
	}
	if o.ResponseHeaderTimeout <= 0 {
		o.ResponseHeaderTimeout = DefaultResponseHeaderTimeout
	}
	if o.ExpectContinueTimeout <= 0 {
		o.ExpectContinueTimeout = DefaultExpectContinueTimeout
	}
	if o.MaxRequestBytes <= 0 {
		o.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if o.MaxResponseBytes <= 0 {
		o.MaxResponseBytes = DefaultMaxResponseBytes
	}
	return o
}

// Client is the spi.HTTPDoer implementation over net/http.
type Client struct {
	hc   *http.Client
	opts Options
}

// Compile-time proof the contract is satisfied, so a signature drift breaks the build here
// rather than at every adapter.
var _ spi.HTTPDoer = (*Client)(nil)

// New builds a client with the pool described by opts.
//
// The returned Client is safe for concurrent use and is expected to be long-lived: constructing
// one per request would defeat the connection pool entirely, which is the single most common way
// a Go service accidentally re-handshakes TLS on every payment.
func New(opts Options) *Client {
	opts = opts.withDefaults()
	rt := opts.Transport
	if rt == nil {
		// Derived from http.DefaultTransport rather than composed from scratch so that proxy
		// support, HTTP/2 negotiation and the dialer's keep-alive settings stay whatever the
		// standard library considers correct for this Go release.
		//
		// The assertion is checked rather than assumed. http.DefaultTransport is a package-level
		// variable any dependency can replace — an instrumentation wrapper installing its own
		// RoundTripper is the usual way — and an unchecked assertion turns that into a panic
		// while a gateway client is being constructed, which on this platform happens inside
		// service start-up and inside every adapter factory. Falling back to a zero-value
		// transport keeps the timeouts set below, which are the settings that actually matter.
		base, ok := http.DefaultTransport.(*http.Transport)
		if ok {
			base = base.Clone()
		} else {
			base = &http.Transport{Proxy: http.ProxyFromEnvironment}
		}
		base.MaxIdleConnsPerHost = opts.MaxIdleConnsPerHost
		base.MaxConnsPerHost = 0 // concurrency is capped by the bulkhead, not by the pool
		base.IdleConnTimeout = opts.IdleConnTimeout
		base.TLSHandshakeTimeout = opts.TLSHandshakeTimeout
		base.ResponseHeaderTimeout = opts.ResponseHeaderTimeout
		base.ExpectContinueTimeout = opts.ExpectContinueTimeout
		base.ForceAttemptHTTP2 = true
		rt = base
	}
	return &Client{
		hc:   &http.Client{Transport: rt, Timeout: opts.Timeout},
		opts: opts,
	}
}

// Do performs one gateway request.
//
// Contract, relied on by every adapter:
//
//   - A timeout — the context deadline, the client ceiling, or a net.Error reporting Timeout —
//     returns (&spi.HTTPResponse{Timeout: true}, nil). The adapter decides what that means for
//     the operation at hand; only the adapter knows whether the call moved money.
//   - Any other transport failure returns (nil, error). These are the failures where the request
//     provably did not reach the gateway, and reporting them as unknown would park healthy
//     payments in reconciliation.
//   - A non-2xx status is *not* an error. It is a response with a status code, because gateways
//     express declines, idempotency conflicts and rate limits as HTTP statuses and only the
//     adapter can tell those apart.
func (c *Client) Do(req *spi.HTTPRequest) (*spi.HTTPResponse, error) {
	if req == nil {
		return nil, apierror.New(apierror.CodeInternalError, "httpx: nil request")
	}
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	operation := req.Headers[OperationHeader]

	started := time.Now()
	emit := func(resp *spi.HTTPResponse, err error) {
		if c.opts.OnLatency == nil {
			return
		}
		s := LatencySample{
			GatewayID: c.opts.GatewayID,
			Operation: operation,
			Method:    req.Method,
			Host:      hostOf(req.URL),
			Duration:  time.Since(started),
			Err:       err,
		}
		if resp != nil {
			s.StatusCode, s.Timeout = resp.StatusCode, resp.Timeout
		}
		c.opts.OnLatency(s)
	}

	// Cancellation is checked before the body is even assembled. A caller whose context is
	// already done has, by construction, not reached the gateway, and answering promptly is what
	// keeps the orchestrator's timeout cascade honest.
	if err := ctx.Err(); err != nil {
		e := classifyContext(err)
		emit(nil, e)
		return nil, e
	}

	if int64(len(req.Body)) > c.opts.MaxRequestBytes {
		e := apierror.Newf(apierror.CodeRequestTooLarge,
			"httpx: request body of %d bytes exceeds the %d byte cap for this gateway",
			len(req.Body), c.opts.MaxRequestBytes)
		emit(nil, e)
		return nil, e
	}

	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	hr, err := http.NewRequestWithContext(ctx, req.Method, req.URL, body)
	if err != nil {
		// A malformed URL or method is a programming error in an adapter, not a gateway failure.
		// It is reported as an internal error and, critically, not as an unknown outcome.
		e := apierror.Wrap(err, apierror.CodeInternalError, "httpx: request could not be constructed")
		emit(nil, e)
		return nil, e
	}
	for k, v := range req.Headers {
		if strings.EqualFold(k, OperationHeader) {
			continue
		}
		hr.Header.Set(k, v)
	}
	if len(req.Body) > 0 {
		hr.ContentLength = int64(len(req.Body))
	}

	resp, err := c.hc.Do(hr)
	if err != nil {
		if IsTimeout(err) {
			out := &spi.HTTPResponse{Timeout: true, Latency: time.Since(started)}
			emit(out, nil)
			return out, nil
		}
		// The error string from net/http can contain the URL, which for some gateways embeds a
		// transaction identifier — never a credential, because credentials travel in headers and
		// net/http does not render headers into transport errors. It is wrapped rather than
		// returned bare so the caller-facing message is ours.
		e := apierror.Wrap(err, apierror.CodeGatewayUnavailable,
			"httpx: the gateway could not be reached")
		emit(nil, e)
		return nil, e
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	// LimitReader with one extra byte so an over-cap body is detected rather than silently
	// truncated. A truncated JSON document parses as malformed, which an adapter would report as
	// an unknown outcome — correct but unhelpfully vague; saying "too large" names the real fault.
	limited := io.LimitReader(resp.Body, c.opts.MaxResponseBytes+1)
	raw, readErr := io.ReadAll(limited)
	if readErr != nil {
		if IsTimeout(readErr) {
			out := &spi.HTTPResponse{Timeout: true, StatusCode: resp.StatusCode, Latency: time.Since(started)}
			emit(out, nil)
			return out, nil
		}
		// A body that started arriving and then failed is genuinely ambiguous: the gateway acted,
		// and we cannot read what it said. Surfacing it as a timeout-flagged response routes it
		// to the adapter's unknown-outcome path, which is the safe side of the ambiguity.
		out := &spi.HTTPResponse{Timeout: true, StatusCode: resp.StatusCode, Latency: time.Since(started)}
		emit(out, nil)
		return out, nil
	}
	if int64(len(raw)) > c.opts.MaxResponseBytes {
		e := apierror.Newf(apierror.CodeGatewayContractViolation,
			"httpx: response body exceeds the %d byte cap for this gateway", c.opts.MaxResponseBytes)
		emit(&spi.HTTPResponse{StatusCode: resp.StatusCode}, e)
		return nil, e
	}

	out := &spi.HTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    flattenHeaders(resp.Header),
		Body:       raw,
		Latency:    time.Since(started),
	}
	emit(out, nil)
	return out, nil
}

func hostOf(rawURL string) string {
	s := rawURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return s
}

// flattenHeaders keeps the first value of each response header.
//
// Gateways send one value per header for everything the adapters read (Stripe-Signature,
// Request-Id, Retry-After, Idempotency-Replayed). Collapsing to a map here rather than exposing
// http.Header keeps vendor transport types out of the SPI, which is the property that lets the
// contract suite drive an adapter with a three-line test double.
func flattenHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// IsTimeout reports whether err represents a deadline or an I/O timeout rather than a refusal.
//
// Three sources have to be recognised and they are genuinely different types: the context
// package's sentinel, net.Error's Timeout method (which os.ErrDeadlineExceeded and
// *net.OpError both satisfy), and http.Client's own wrapper around a Timeout field. Missing any
// one of them makes a slow gateway look like a refused one, and a refused one is safe to retry.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	// http.Client wraps its own Timeout expiry in a *url.Error whose Timeout() reports true, which
	// the net.Error branch catches. The string check below is the last resort for transports that
	// return an unwrapped sentinel; it is deliberately narrow.
	return strings.Contains(err.Error(), "Client.Timeout exceeded")
}

func classifyContext(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return apierror.Wrap(err, apierror.CodeGatewayTimeout,
			"httpx: the deadline expired before the request was sent")
	}
	return apierror.Wrap(err, apierror.CodeServiceUnavailable,
		"httpx: the request was cancelled before it was sent")
}

// sensitiveHeaders are the header names whose values must never be rendered anywhere.
//
// The list is by name rather than by heuristic because a heuristic ("looks like base64") both
// misses X-API-Key and falsely redacts a pspReference. Names are matched case-insensitively
// because HTTP header names are case-insensitive and a vendor sending "authorization" in
// lowercase must not slip past.
var sensitiveHeaders = map[string]struct{}{
	"authorization":           {},
	"proxy-authorization":     {},
	"x-api-key":               {},
	"stripe-signature":        {},
	"paypal-auth-assertion":   {},
	"paypal-transmission-sig": {},
	"cookie":                  {},
	"set-cookie":              {},
}

// SafeHeaders returns a copy of h with credential-bearing values replaced by a fixed placeholder.
//
// It is the only header-rendering function in this package, and it is exported so that adapters,
// tests and any future debug tooling have a correct option that is easier to reach for than
// writing the wrong thing. Note what it does *not* offer: there is no SafeBody, because there is
// no safe rendering of a gateway request body — it carries payment tokens, and in some vendors'
// flows a raw PAN. Bodies are never logged, full stop.
func SafeHeaders(h map[string]string) map[string]string {
	if h == nil {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if _, bad := sensitiveHeaders[strings.ToLower(k)]; bad {
			out[k] = "[REDACTED]"
			continue
		}
		out[k] = v
	}
	return out
}

// IsSensitiveHeader reports whether a header name carries credential material.
func IsSensitiveHeader(name string) bool {
	_, ok := sensitiveHeaders[strings.ToLower(name)]
	return ok
}
