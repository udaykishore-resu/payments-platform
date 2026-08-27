package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/config"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Signature header names. They are constants rather than per-endpoint configuration because they
// are part of the platform's published contract: a merchant's verification code reads them, and
// changing one silently breaks every integration at once.
const (
	// HeaderSignature carries the hex HMAC-SHA256 over "<timestamp>.<body>".
	HeaderSignature = "X-Payments-Signature"
	// HeaderTimestamp carries the Unix second the signature was computed at. It is signed *with*
	// the body rather than sent alongside it, which is what makes a captured delivery
	// unreplayable: an attacker who changes the timestamp invalidates the signature.
	HeaderTimestamp = "X-Payments-Timestamp"
	// HeaderEventID lets a merchant deduplicate. Deliveries are at-least-once; a merchant that
	// does not deduplicate will double-ship.
	HeaderEventID = "X-Payments-Event-Id"
	// HeaderEventType is the event name, so a merchant can route without parsing.
	HeaderEventType = "X-Payments-Event-Type"
	// HeaderAttempt is the delivery attempt number, so a merchant can tell a retry from a first
	// delivery in their own logs.
	HeaderAttempt = "X-Payments-Attempt"
)

// DeliveryDefaults are the retry policy's defaults.
const (
	// DefaultMaxAttempts bounds the retry ladder. Six attempts across roughly ten minutes is long
	// enough to survive a merchant's deploy and short enough that a permanently broken endpoint
	// does not accumulate a queue nobody drains.
	DefaultMaxAttempts = 6
	// DefaultInitialBackoff is the first delay.
	DefaultInitialBackoff = 2 * time.Second
	// DefaultMaxBackoff caps the doubling.
	DefaultMaxBackoff = 5 * time.Minute
	// DefaultTimeout bounds one delivery attempt. A merchant endpoint that has not answered in
	// ten seconds is not going to.
	DefaultTimeout = 10 * time.Second
)

// Doer is the minimal HTTP surface the deliverer needs.
//
// Narrowed to one method so that a test double is three lines, and so that it is obvious the
// deliverer has no business reaching for transport-level knobs. The *transport* is built by
// NewGuardedTransport, which is where the SSRF control lives.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// DeliverDeps is what outbound delivery needs.
type DeliverDeps struct {
	HTTP  Doer
	UoW   ports.UnitOfWork
	Clock shared.Clock
	// Secrets resolves an endpoint's `secret://` reference to its signing key. The key is
	// resolved per delivery and never held on this struct — the same rule the gateway resolver
	// follows, for the same reasons.
	Secrets ports.SecretsProvider
	// MaxAttempts, InitialBackoff and MaxBackoff tune the ladder. Zero means the defaults.
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	Timeout        time.Duration
}

// Deliverer sends merchant-facing webhooks.
type Deliverer struct {
	deps DeliverDeps
}

// NewDeliverer constructs the deliverer.
func NewDeliverer(d DeliverDeps) *Deliverer {
	if d.Clock == nil {
		d.Clock = shared.SystemClock{}
	}
	if d.MaxAttempts <= 0 {
		d.MaxAttempts = DefaultMaxAttempts
	}
	if d.InitialBackoff <= 0 {
		d.InitialBackoff = DefaultInitialBackoff
	}
	if d.MaxBackoff <= 0 {
		d.MaxBackoff = DefaultMaxBackoff
	}
	if d.Timeout <= 0 {
		d.Timeout = DefaultTimeout
	}
	return &Deliverer{deps: d}
}

// Outbound is one event to deliver to a merchant.
type Outbound struct {
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	EventID    shared.EventID
	EventType  string
	Payload    []byte
	// Endpoints is the merchant's configured destination list. Delivery fans out over the ones
	// whose glob patterns match the event type.
	Endpoints []config.WebhookEndpoint
}

// DeliveryResult is the outcome of delivering one event to one endpoint.
type DeliveryResult struct {
	URL      string
	Attempts int
	Status   int
	// Delivered is true when the endpoint answered 2xx.
	Delivered bool
	Err       error
}

// Deliver sends an event to every endpoint whose patterns match it.
//
// Matching is a glob over the event type ("payment.*", "*"), evaluated per endpoint, because a
// merchant who subscribed one endpoint to payment events and another to merchant events should
// not receive both on both. An endpoint with no patterns receives nothing — L4 rejects that
// configuration at publish time, and honouring it here rather than defaulting to "everything"
// keeps the two consistent.
func (d *Deliverer) Deliver(ctx context.Context, out Outbound) ([]DeliveryResult, error) {
	var results []DeliveryResult
	for _, ep := range out.Endpoints {
		if !ep.Active || !MatchesAny(ep.Events, out.EventType) {
			continue
		}
		results = append(results, d.deliverOne(ctx, out, ep))
	}
	return results, nil
}

// deliverOne runs the retry ladder against one endpoint.
func (d *Deliverer) deliverOne(ctx context.Context, out Outbound, ep config.WebhookEndpoint) DeliveryResult {
	res := DeliveryResult{URL: ep.URL}

	secret, err := d.signingKey(ctx, ep)
	if err != nil {
		res.Err = err
		return res
	}

	backoff := d.deps.InitialBackoff
	for attempt := 1; attempt <= d.deps.MaxAttempts; attempt++ {
		res.Attempts = attempt
		status, err := d.attempt(ctx, out, ep, secret, attempt)
		res.Status, res.Err = status, err
		if err == nil && status >= 200 && status < 300 {
			res.Delivered, res.Err = true, nil
			return res
		}
		// A 4xx other than 408 and 429 is the merchant's endpoint telling us the request is
		// wrong, and retrying an unchanged request against it is pure load. 5xx, timeouts and
		// transport failures are retried.
		if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
			return res
		}
		if attempt == d.deps.MaxAttempts {
			return res
		}
		select {
		case <-ctx.Done():
			res.Err = ctx.Err()
			return res
		case <-time.After(jitter(backoff)):
		}
		backoff *= 2
		if backoff > d.deps.MaxBackoff {
			backoff = d.deps.MaxBackoff
		}
	}
	return res
}

// attempt performs one signed POST.
func (d *Deliverer) attempt(ctx context.Context, out Outbound, ep config.WebhookEndpoint,
	secret string, attempt int) (int, error) {

	callCtx, cancel := context.WithTimeout(ctx, d.deps.Timeout)
	defer cancel()

	ts := d.deps.Clock.Now().UTC().Unix()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, ep.URL, bytes.NewReader(out.Payload))
	if err != nil {
		return 0, apierror.Wrap(err, apierror.CodeValidationFailed, "the webhook endpoint is not a usable URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderSignature, Sign(secret, ts, out.Payload))
	req.Header.Set(HeaderEventID, out.EventID.String())
	req.Header.Set(HeaderEventType, out.EventType)
	req.Header.Set(HeaderAttempt, strconv.Itoa(attempt))

	resp, err := d.deps.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	return resp.StatusCode, nil
}

// signingKey resolves the endpoint's HMAC secret at the moment of use.
func (d *Deliverer) signingKey(ctx context.Context, ep config.WebhookEndpoint) (string, error) {
	if ep.SecretRef == "" {
		return "", apierror.New(apierror.CodeConfigurationInvalid,
			"the webhook endpoint has no signing secret reference").
			WithDetail(apierror.Detail{
				Field: "webhooks.endpoints.secretRef", Code: "MISSING_SECRET_REF",
				Message: "an unsigned outbound webhook cannot be authenticated by the merchant",
				RuleID:  "L4.CREDENTIAL_IS_A_REFERENCE",
			})
	}
	if d.deps.Secrets == nil {
		return "", apierror.New(apierror.CodeConfigurationInvalid, "no secrets provider is configured")
	}
	material, err := d.deps.Secrets.Get(ctx, ep.SecretRef)
	if err != nil {
		return "", apierror.Wrap(err, apierror.CodeDependencyFailure,
			"could not resolve the webhook signing secret")
	}
	v, ok := material.Value("signingSecret")
	if !ok || v == "" {
		return "", apierror.New(apierror.CodeConfigurationInvalid,
			"the webhook secret reference resolved to no signing secret")
	}
	return v, nil
}

// Sign computes the outbound signature: hex HMAC-SHA256 over "<timestamp>.<body>".
//
// The timestamp is inside the signed material rather than merely alongside it, and that is the
// entire replay defence: a captured delivery replayed an hour later still carries its original
// timestamp, so a merchant who checks freshness rejects it, and an attacker who updates the
// timestamp invalidates the signature. Signing only the body would make every captured delivery
// replayable forever.
func Sign(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature in constant time.
//
// Exported so the platform's own integration tests — and a merchant reading this repository —
// verify exactly the way the deliverer signs. A variable-time comparison on an HMAC is a timing
// oracle, and hmac.Equal is the only comparison used anywhere in this platform.
func Verify(secret string, timestamp int64, body []byte, signature string) bool {
	want := Sign(secret, timestamp, body)
	return hmac.Equal([]byte(want), []byte(signature))
}

// MatchesAny reports whether any pattern matches the event type.
func MatchesAny(patterns []string, eventType string) bool {
	for _, p := range patterns {
		if Matches(p, eventType) {
			return true
		}
	}
	return false
}

// Matches evaluates one glob pattern against an event type.
//
// path.Match rather than a regular expression, deliberately. The patterns come from merchant
// configuration, and a regular expression from a configuration field is a denial of service
// delivered through a form: catastrophic backtracking on a pattern a merchant typed would burn a
// delivery worker's CPU on every event. A glob has no backtracking to catastrophe on.
//
// The separator is `.` rather than `/`, so `payment.*` matches `payment.captured.v1`: path.Match
// treats `*` as "anything but the separator", and event types are dot-separated, so the pattern is
// evaluated against a slash-substituted copy.
func Matches(pattern, eventType string) bool {
	if pattern == "" {
		return false
	}
	if pattern == "*" || pattern == "**" {
		return true
	}
	p := strings.ReplaceAll(pattern, ".", "/")
	e := strings.ReplaceAll(eventType, ".", "/")
	if ok, err := path.Match(p, e); err == nil && ok {
		return true
	}
	// `payment.*` is idiomatically read as "every payment event", including `payment.captured.v1`
	// which has two segments after the prefix. Supporting the prefix form explicitly is what makes
	// the pattern mean what a merchant thinks it means.
	if strings.HasSuffix(pattern, ".*") {
		return strings.HasPrefix(eventType, strings.TrimSuffix(pattern, "*"))
	}
	return false
}

// jitter applies full jitter to a backoff interval.
//
// Full jitter, not "the interval plus a bit": a fleet of delivery workers that all failed against
// the same merchant endpoint at the same instant will otherwise all retry at the same instant,
// which is a synchronized thundering herd aimed at an endpoint that is already struggling.
//
// crypto/rand rather than math/rand because the workers are separate processes: a
// deterministically-seeded PRNG across a fleet re-synchronises the herd it was added to break.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(d)))
	if err != nil {
		return d
	}
	return time.Duration(n.Int64())
}

// NewGuardedTransport builds the HTTP transport outbound webhooks must use.
//
// This is the second of the three SSRF layers, and it is the one that actually closes the hole.
// The configuration-time check (config.validateWebhookURL) rejects a bad URL when it is saved;
// the NetworkPolicy restricts egress from the delivery pods. Neither catches **DNS rebinding**: a
// merchant registers `https://webhooks.example.com`, which resolves to a public address when the
// configuration is validated and to `169.254.169.254` an hour later when the delivery is made.
// Only a check at *dial* time sees the address that is actually about to be connected to.
//
// The Control hook is the right place for it because it runs after resolution and before the
// connect syscall, on every address the resolver returned — including the second one, when the
// first is public and the second is not. A check on `url.Host` cannot see any of that.
//
// The blocked-range list comes from the domain rather than being restated here. Two copies of a
// list like this diverge, and the divergence is a bypass.
func NewGuardedTransport(base *http.Transport) *http.Transport {
	t := base
	if t == nil {
		t = &http.Transport{
			MaxIdleConns:        64,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 5 * time.Second,
		}
	}
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			return GuardAddress(network, address)
		},
	}
	t.DialContext = dialer.DialContext
	return t
}

// ErrBlockedAddress is returned when a dial resolves into a reserved range.
var ErrBlockedAddress = errors.New("webhook: the endpoint resolved to a blocked address")

// GuardAddress reports whether a dial target is permitted.
//
// Exported so the guard is testable without a network and without a live rebinding attack: the
// Control hook is a closure over exactly this function, so testing it is testing the hook.
func GuardAddress(network, address string) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		// A merchant webhook is HTTPS over TCP. Anything else is not a delivery.
		return ErrBlockedAddress
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// The Control hook is called with a resolved literal. A host that does not parse as an IP
		// here means something upstream did not resolve, and dialling it would be dialling
		// something we cannot classify.
		return ErrBlockedAddress
	}
	if config.IsBlockedAddress(ip) {
		return ErrBlockedAddress
	}
	return nil
}
