package paypal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Headers PayPal sets on a webhook delivery.
const (
	HeaderTransmissionID   = "PayPal-Transmission-Id"
	HeaderTransmissionTime = "PayPal-Transmission-Time"
	HeaderTransmissionSig  = "PayPal-Transmission-Sig"
	HeaderCertURL          = "PayPal-Cert-Url"
	HeaderAuthAlgo         = "PayPal-Auth-Algo"
)

// VerifyPath is PayPal's server-side signature verification endpoint.
const VerifyPath = "/v1/notifications/verify-webhook-signature"

// Verifier authenticates inbound PayPal webhooks.
//
// PayPal is the odd one out: its webhooks are signed with an *asymmetric* key whose certificate is
// published at a URL named in the delivery's own headers. Two verification strategies exist, and
// this adapter models the first:
//
//  1. **Ask PayPal.** POST the headers and the raw event to
//     /v1/notifications/verify-webhook-signature and read `verification_status`. This is the
//     documented approach, it needs no certificate handling, and it is what is implemented here.
//     Its cost is a network round trip inside the ingress path and a dependency on PayPal being up
//     in order to accept PayPal's own events.
//  2. **Verify the certificate chain locally.** Fetch the certificate at `PayPal-Cert-Url`,
//     pin the host to `*.paypal.com`, validate the chain to a trusted root, check the CRL, then
//     verify the SHA256withRSA signature over
//     `transmissionId|transmissionTime|webhookId|crc32(rawBody)`. This removes the round trip and
//     the availability coupling, at the cost of owning certificate caching, rotation and revocation
//     — and a mistake in any of those is a silently-accepting verifier, which is worse than a slow
//     one. It is documented here rather than implemented because the platform's ingress already
//     tolerates the round trip, and because the local path should not be written without the
//     certificate-pinning tests that make it trustworthy.
//
// The `secrets` parameter carries webhook *ids*, not signing keys — PayPal's verification is
// performed against a registered webhook rather than a shared secret. Multiple entries are still
// meaningful and are still tried in turn: during a webhook re-registration two ids are live at
// once, and a verifier that knows only one drops every event signed against the other.
type Verifier struct {
	cfg       spi.Config
	client    spi.HTTPDoer
	clock     shared.Clock
	tokens    *tokenCache
	tolerance time.Duration
	// creds are the OAuth client used to call the verification endpoint. Verification is itself an
	// authenticated call, which is the one place this adapter's verifier — unlike Stripe's and
	// Adyen's — genuinely needs a credential.
	creds spi.Credentials
}

var _ spi.WebhookVerifier = (*Verifier)(nil)

// NewVerifier builds the webhook verifier. Bind the OAuth client with WithCredentials before use:
// PayPal's verification endpoint is authenticated, so a verifier with no credentials can only
// report that it cannot verify.
func NewVerifier(cfg spi.Config) (*Verifier, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Verifier{
		cfg: cfg, client: cfg.HTTPClient, clock: cfg.Clock,
		tokens: newTokenCache(cfg.Clock), tolerance: cfg.WebhookTolerance,
	}, nil
}

// WithCredentials binds the OAuth client credentials used to call the verification endpoint.
func (v *Verifier) WithCredentials(creds spi.Credentials) *Verifier {
	c := *v
	c.creds = creds
	return &c
}

// ID returns the registry slug.
func (v *Verifier) ID() shared.GatewayID { return GatewayID }

type verifyRequest struct {
	AuthAlgo         string          `json:"auth_algo"`
	CertURL          string          `json:"cert_url"`
	TransmissionID   string          `json:"transmission_id"`
	TransmissionSig  string          `json:"transmission_sig"`
	TransmissionTime string          `json:"transmission_time"`
	WebhookID        string          `json:"webhook_id"`
	WebhookEvent     json.RawMessage `json:"webhook_event"`
}

// Verify authenticates a PayPal webhook and normalizes it.
//
// Ordering, and its limits, stated honestly: the signature is checked before the event is parsed
// into the platform's model, which is the property the SPI asks for. But PayPal's verification API
// requires the event to be embedded as JSON in the verification request, so the body must at least
// be *well-formed* JSON before it can be authenticated. That minimal well-formedness check is not
// a parse of the event's contents and it produces a MALFORMED_REQUEST rather than a signature
// error — which is exactly how the contract suite distinguishes the two: invalid JSON with a good
// signature must not be reported as a signature failure, and valid JSON with a bad signature must
// be.
func (v *Verifier) Verify(ctx context.Context, raw []byte, headers map[string]string, secrets []string, now time.Time) (*spi.WebhookEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	if len(secrets) == 0 {
		return nil, apierror.New(apierror.CodeGatewayNotConfigured,
			"paypal: no webhook id is configured for this gateway")
	}
	transmissionID := headerValue(headers, HeaderTransmissionID)
	transmissionSig := headerValue(headers, HeaderTransmissionSig)
	transmissionTime := headerValue(headers, HeaderTransmissionTime)
	certURL := headerValue(headers, HeaderCertURL)
	authAlgo := headerValue(headers, HeaderAuthAlgo)
	if transmissionID == "" || transmissionSig == "" || transmissionTime == "" {
		return nil, apierror.New(apierror.CodeWebhookSignatureInvalid,
			"paypal: the delivery is missing its PayPal-Transmission-* headers")
	}
	// Certificate host pinning happens before the URL is ever handed to PayPal, and before any
	// local fetch would occur. An attacker who can choose cert_url can otherwise point verification
	// at a certificate they control.
	if certURL != "" && !isPayPalCertURL(certURL) {
		return nil, apierror.New(apierror.CodeWebhookSignatureInvalid,
			"paypal: the certificate URL is not on a PayPal host")
	}
	if authAlgo == "" {
		authAlgo = "SHA256withRSA"
	}

	// Replay window, checked before spending a network round trip on verification. A stale delivery
	// is rejected whether or not it is authentic: PayPal retries for three days, and an event three
	// days old that the platform has not already deduplicated is one it should not act on blind.
	occurred, tsErr := time.Parse(time.RFC3339, transmissionTime)
	if tsErr != nil {
		return nil, apierror.New(apierror.CodeWebhookSignatureInvalid,
			"paypal: the PayPal-Transmission-Time header is not a valid RFC 3339 timestamp")
	}
	age := now.Sub(occurred)
	if age < 0 {
		age = -age
	}
	if age > v.tolerance {
		return nil, apierror.Newf(apierror.CodeWebhookReplayDetected,
			"paypal: the delivery is %s outside the %s tolerance", age.Truncate(time.Second), v.tolerance)
	}

	// The minimal well-formedness check. json.Valid does not build a Go value and does not run any
	// of the platform's own decoding; it only establishes that the bytes can be embedded in the
	// verification request at all.
	if !json.Valid(raw) {
		return nil, apierror.New(apierror.CodeMalformedRequest,
			"paypal: the delivery body is not well-formed JSON and cannot be submitted for verification")
	}

	verified := false
	var lastErr error
	for _, webhookID := range secrets {
		if strings.TrimSpace(webhookID) == "" {
			continue
		}
		ok, err := v.askPayPal(ctx, verifyRequest{
			AuthAlgo:         authAlgo,
			CertURL:          certURL,
			TransmissionID:   transmissionID,
			TransmissionSig:  transmissionSig,
			TransmissionTime: transmissionTime,
			WebhookID:        webhookID,
			WebhookEvent:     json.RawMessage(raw),
		})
		if err != nil {
			// A transport failure while verifying is not a signature failure. Recording it and
			// continuing lets a second configured webhook id still succeed; if none does, the
			// transport error is what surfaces, so the ingress retries rather than discarding a
			// possibly-genuine event.
			lastErr = err
			continue
		}
		if ok {
			verified = true
			break
		}
	}
	if !verified {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, apierror.New(apierror.CodeWebhookSignatureInvalid,
			"paypal: the delivery signature was not accepted for any configured webhook id")
	}

	return parseEvent(raw, occurred)
}

func (v *Verifier) askPayPal(ctx context.Context, body verifyRequest) (bool, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return false, apierror.Wrap(err, apierror.CodeInternalError,
			"paypal: the verification request could not be encoded")
	}
	token, err := v.tokens.token(ctx, v.client, v.cfg.BaseURL, v.creds)
	if err != nil {
		return false, err
	}
	resp, err := v.client.Do(&spi.HTTPRequest{
		Ctx:    ctx,
		Method: http.MethodPost,
		URL:    v.cfg.BaseURL + VerifyPath,
		Headers: map[string]string{
			"Authorization":       "Bearer " + token,
			"Content-Type":        "application/json",
			"Accept":              "application/json",
			httpx.OperationHeader: "webhook_verify",
		},
		Body: raw,
	})
	if err != nil {
		return false, err
	}
	if resp == nil {
		return false, apierror.New(apierror.CodeGatewayContractViolation,
			"paypal: the transport returned neither a response nor an error")
	}
	if resp.Timeout {
		return false, apierror.New(apierror.CodeGatewayTimeout,
			"paypal: the webhook verification call timed out")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e errorResponse
		_ = decode(resp.Body, &e)
		return false, mapErrorName(resp.StatusCode, &e)
	}
	var vr verifySignatureResponse
	if err := decode(resp.Body, &vr); err != nil {
		return false, err
	}
	// Anything other than the exact string SUCCESS is a failure. This is compared with == rather
	// than with a case-insensitive or prefix match on purpose: a verifier that accepts
	// "SUCCESS_PENDING" or an empty string because of a loose comparison is a verifier that accepts
	// forged events.
	return vr.VerificationStatus == "SUCCESS", nil
}

// isPayPalCertURL pins the certificate host.
//
// The check is on the host component after parsing, not on a substring of the URL: an attacker can
// register `paypal.com.evil.test` or embed `https://api.paypal.com@evil.test/` and a substring
// match accepts both.
func isPayPalCertURL(raw string) bool {
	if !strings.HasPrefix(raw, "https://") {
		return false
	}
	rest := raw[len("https://"):]
	// Userinfo before an '@' is the classic bypass; a URL carrying one is rejected outright rather
	// than parsed, because there is no legitimate PayPal certificate URL with credentials in it.
	if i := strings.IndexByte(rest, '@'); i >= 0 {
		if j := strings.IndexAny(rest, "/?#"); j < 0 || i < j {
			return false
		}
	}
	host := rest
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(host)
	return host == "paypal.com" || strings.HasSuffix(host, ".paypal.com")
}

// parseEvent normalizes an authenticated PayPal event.
func parseEvent(raw []byte, occurred time.Time) (*spi.WebhookEvent, error) {
	var ev webhookEvent
	if err := decode(raw, &ev); err != nil {
		return nil, apierror.Wrap(err, apierror.CodeGatewayContractViolation,
			"paypal: the webhook body is authenticated but is not a parseable event")
	}
	if ev.ID == "" {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"paypal: the webhook body carries no event id")
	}
	out := &spi.WebhookEvent{
		GatewayEventID: ev.ID,
		EventType:      ev.EventType,
		Kind:           webhookKind(ev.EventType),
		OccurredAt:     occurred,
		Raw:            append([]byte(nil), raw...),
	}
	if t, err := time.Parse(time.RFC3339, ev.CreateTime); err == nil {
		out.OccurredAt = t.UTC()
	}

	var res webhookResource
	if len(ev.Resource) > 0 {
		if err := json.Unmarshal(ev.Resource, &res); err != nil {
			// The envelope is authenticated and deliverable; a resource we cannot read still has to
			// be recorded so the deduplication key exists. The processor resolves state by lookup.
			return out, nil //nolint:nilerr // the envelope is authenticated and deliverable; an unreadable resource must still produce the deduplication record, and the processor resolves state by lookup
		}
	}
	out.GatewayRef = withKind(referenceKindFor(ev.ResourceType), res.ID)
	// `invoice_id` carries the idempotency key this adapter set at authorization time, which is
	// what lets the processor match an event to an attempt without a gateway-reference lookup.
	out.IdempotencyKey = res.InvoiceID
	out.RefundID = shared.RefundID(res.CustomID)
	if ev.ResourceType != "refund" {
		out.RefundID = ""
		out.PaymentID = shared.PaymentID("")
	}
	if res.Amount != nil {
		if m, err := parseAmount(res.Amount); err == nil {
			out.Amount = &m
		}
	}
	switch out.Kind {
	case spi.KindAuthorizationSucceeded:
		out.Status = spi.StatusAuthorized
	case spi.KindCaptureSucceeded:
		out.Status = spi.StatusCaptured
	case spi.KindRefundSucceeded:
		out.Status = spi.StatusRefundAccepted
	case spi.KindVoidSucceeded:
		out.Status = spi.StatusVoided
	case spi.KindCaptureFailed:
		out.Status = spi.StatusDeclined
		reason, _ := mapDecline(res.ProcessorResponse, nil)
		out.DeclineReason = reason
	default:
		// The remaining kinds carry no payment status: a failed authorization arrives from PayPal as a
		// separate resource the processor resolves by lookup, and payouts, disputes and account
		// updates are not payment transitions at all
	}
	return out, nil
}

func referenceKindFor(resourceType string) string {
	switch resourceType {
	case "capture":
		return refCapture
	case "authorization":
		return refAuthorization
	case "refund":
		return refRefund
	default:
		return refOrder
	}
}

// headerValue reads a header case-insensitively; proxies normalize casing inconsistently and a
// verifier that misses a header because of casing rejects every delivery.
func headerValue(h map[string]string, name string) string {
	if v, ok := h[name]; ok {
		return v
	}
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// jsonEscape escapes a value for embedding in a hand-built JSON document.
//
// Hand-built because the auth assertion must be a specific unsigned JWT and marshalling a map would
// not guarantee key order; escaping by hand because the values are identifiers that can, in a
// pathological configuration, contain a quote or a backslash, and an unescaped one would produce a
// header PayPal rejects with an error that names neither field.
func jsonEscape(s string) string {
	b, err := json.Marshal(s)
	if err != nil || len(b) < 2 {
		return ""
	}
	return string(b[1 : len(b)-1])
}

// base64URL encodes a JWT segment: URL alphabet, no padding.
func base64URL(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
