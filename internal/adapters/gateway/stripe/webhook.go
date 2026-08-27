package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// SignatureHeader is the header Stripe signs its webhooks with.
const SignatureHeader = "Stripe-Signature"

// Verifier authenticates inbound Stripe webhooks.
//
// It is a separate type from Gateway on purpose: the webhook ingress is the most exposed surface
// in the platform, and the blast radius of compromising it must not include the ability to move
// money. A Verifier holds no credential and can issue no request — it can only say yes or no
// about a body somebody else received.
type Verifier struct {
	tolerance time.Duration
}

var _ spi.WebhookVerifier = (*Verifier)(nil)

// NewVerifier builds the webhook verifier. The tolerance defaults to five minutes: long enough to
// survive clock skew and a slow ingress queue, short enough that a captured request stops being
// replayable while the attacker is still reading it.
func NewVerifier(cfg spi.Config) (*Verifier, error) {
	tol := cfg.WebhookTolerance
	if tol <= 0 {
		tol = 5 * time.Minute
	}
	return &Verifier{tolerance: tol}, nil
}

// ID returns the registry slug.
func (v *Verifier) ID() shared.GatewayID { return GatewayID }

// Verify authenticates a Stripe webhook and normalizes it.
//
// The order of operations is the security property, and it is deliberate:
//
//  1. Read and structurally validate the `Stripe-Signature` header. This touches only the header,
//     which is a fixed, small grammar.
//  2. Recompute the HMAC over `t + "." + raw` and compare with hmac.Equal against every candidate
//     signature and every candidate secret. Constant-time, because a variable-time compare on an
//     HMAC is a timing oracle that lets an attacker extract a valid signature byte by byte.
//  3. Only then check the replay window, and only then hand the body to a JSON parser.
//
// Step 3 is the part adapters get wrong. Parsing first means an unauthenticated attacker chooses
// the input to the parser, and the parser is the largest attack surface in the whole path. The
// contract suite proves the ordering directly: a body that is not valid JSON but carries a valid
// signature must fail with a *parse* error, and a body that is valid JSON with a bad signature
// must fail with a *signature* error.
//
// Multiple secrets are accepted because Stripe's rotation model creates two live signing secrets
// for the overlap window. A verifier that knows only one drops every webhook signed with the other
// — silently, from the merchant's point of view, since the platform simply never learns the
// payment succeeded.
func (v *Verifier) Verify(ctx context.Context, raw []byte, headers map[string]string, secrets []string, now time.Time) (*spi.WebhookEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	if len(secrets) == 0 {
		return nil, apierror.New(apierror.CodeGatewayNotConfigured,
			"stripe: no webhook signing secret is configured for this gateway")
	}
	sigHeader := headerValue(headers, SignatureHeader)
	if sigHeader == "" {
		return nil, apierror.New(apierror.CodeWebhookSignatureInvalid,
			"stripe: the Stripe-Signature header is absent")
	}
	ts, candidates, err := parseSignatureHeader(sigHeader)
	if err != nil {
		return nil, err
	}

	signedPayload := make([]byte, 0, len(ts)+1+len(raw))
	signedPayload = append(signedPayload, ts...)
	signedPayload = append(signedPayload, '.')
	signedPayload = append(signedPayload, raw...)

	if !anySecretMatches(signedPayload, candidates, secrets) {
		return nil, apierror.New(apierror.CodeWebhookSignatureInvalid,
			"stripe: no configured signing secret produces this signature")
	}

	// Replay window. Checked after authentication so that an unauthenticated caller cannot learn
	// anything from the difference between "stale" and "forged" — both are rejected, and the one
	// that leaks less is the one an attacker sees first.
	secs, convErr := strconv.ParseInt(string(ts), 10, 64)
	if convErr != nil {
		return nil, apierror.New(apierror.CodeWebhookSignatureInvalid,
			"stripe: the signature timestamp is not an integer")
	}
	age := now.Sub(time.Unix(secs, 0))
	if age < 0 {
		age = -age
	}
	if age > v.tolerance {
		return nil, apierror.Newf(apierror.CodeWebhookReplayDetected,
			"stripe: the webhook timestamp is %s outside the %s tolerance", age.Truncate(time.Second), v.tolerance)
	}

	return parseEvent(raw)
}

// anySecretMatches performs the constant-time comparison across the rotation set.
//
// Every (secret, candidate) pair is evaluated rather than short-circuiting on the first match.
// The cost is a handful of HMACs; the benefit is that the total work does not depend on which
// secret matched, which keeps the rotation state itself out of the timing channel.
func anySecretMatches(signedPayload []byte, candidates [][]byte, secrets []string) bool {
	matched := false
	for _, s := range secrets {
		if s == "" {
			continue
		}
		mac := hmac.New(sha256.New, []byte(s))
		mac.Write(signedPayload)
		expected := mac.Sum(nil)
		for _, c := range candidates {
			if hmac.Equal(expected, c) {
				matched = true
			}
		}
	}
	return matched
}

// parseSignatureHeader splits `t=…,v1=…,v1=…` into the timestamp and the candidate signatures.
//
// Several `v1` entries are legal and are what Stripe emits during a secret rotation, so they are
// all collected. Schemes other than `v1` (Stripe's retired `v0`) are ignored rather than rejected:
// rejecting would break on a header Stripe is entitled to extend.
func parseSignatureHeader(h string) (ts []byte, candidates [][]byte, err error) {
	for _, part := range strings.Split(h, ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = []byte(val)
		case "v1":
			b, decErr := hex.DecodeString(val)
			if decErr != nil {
				// A malformed hex signature is not a candidate; it is also not, on its own, a
				// reason to reject the request, because another v1 entry may be well-formed.
				continue
			}
			candidates = append(candidates, b)
		}
	}
	if len(ts) == 0 {
		return nil, nil, apierror.New(apierror.CodeWebhookSignatureInvalid,
			"stripe: the Stripe-Signature header carries no timestamp")
	}
	if len(candidates) == 0 {
		return nil, nil, apierror.New(apierror.CodeWebhookSignatureInvalid,
			"stripe: the Stripe-Signature header carries no v1 signature")
	}
	return ts, candidates, nil
}

// eventObject is the union of the fields the platform reads from any Stripe event payload.
//
// One struct rather than a type switch on `object`, because the fields that matter — id, amount,
// currency, status, metadata — are spelled identically on PaymentIntent, Charge and Refund. The
// few that are not (a dispute's `charge`) are read from the raw message where they are needed.
type eventObject struct {
	ID               string            `json:"id"`
	Object           string            `json:"object"`
	Amount           int64             `json:"amount"`
	AmountRefunded   int64             `json:"amount_refunded"`
	Currency         string            `json:"currency"`
	Status           string            `json:"status"`
	Metadata         map[string]string `json:"metadata"`
	PaymentIntent    string            `json:"payment_intent"`
	LastPaymentError *stripeError      `json:"last_payment_error"`
	FailureCode      string            `json:"failure_code"`
	Created          int64             `json:"created"`
}

// parseEvent normalizes an authenticated Stripe event.
//
// An event type the platform does not model becomes KindIgnored rather than an error. Stripe ships
// new event types continuously; an adapter that errors on one turns their product launch into our
// webhook-ingress incident, complete with a retry storm from Stripe's own delivery retries.
func parseEvent(raw []byte) (*spi.WebhookEvent, error) {
	var ev event
	if err := decode(raw, &ev); err != nil {
		return nil, apierror.Wrap(err, apierror.CodeGatewayContractViolation,
			"stripe: the webhook body is authenticated but is not a parseable event")
	}
	if ev.ID == "" {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"stripe: the webhook body carries no event id")
	}

	out := &spi.WebhookEvent{
		GatewayEventID: ev.ID,
		EventType:      ev.Type,
		Kind:           webhookKind(ev.Type),
		OccurredAt:     time.Unix(ev.Created, 0).UTC(),
		Raw:            append([]byte(nil), raw...),
	}

	var obj eventObject
	if len(ev.Data.Object) > 0 {
		if err := json.Unmarshal(ev.Data.Object, &obj); err != nil {
			// The envelope parsed and is authenticated; a data object we cannot read is still a
			// deliverable event, and dropping it would lose the deduplication record. The
			// processor will resolve state by lookup instead.
			return out, nil //nolint:nilerr // the envelope is authenticated and deliverable; an unreadable data object must still produce the deduplication record, and the processor resolves state by lookup
		}
	}

	out.GatewayRef = obj.ID
	if obj.Object == "charge" || obj.Object == "refund" {
		if obj.PaymentIntent != "" {
			out.GatewayRef = obj.PaymentIntent
		}
	}
	out.IdempotencyKey = obj.Metadata[metaIdempotencyKey]
	out.PaymentID = shared.PaymentID(obj.Metadata[metaPaymentID])
	out.RefundID = shared.RefundID(obj.Metadata[metaRefundID])

	if obj.Currency != "" && obj.Amount != 0 {
		if m, err := money.New(obj.Amount, money.Currency(normalizeCurrency(obj.Currency))); err == nil {
			out.Amount = &m
		}
	}

	switch obj.Object {
	case "payment_intent":
		if s, err := mapIntentStatus(&paymentIntent{Status: obj.Status, LastPaymentError: obj.LastPaymentError}); err == nil {
			out.Status = s
		}
	case "refund":
		if s, err := mapRefundStatus(&refund{Status: obj.Status}); err == nil {
			out.Status = s
		}
	}
	if out.Status == spi.StatusDeclined || out.Kind == spi.KindAuthorizationFailed {
		reason, _ := mapDecline(obj.LastPaymentError, nil)
		out.DeclineReason = reason
		if out.Status == "" {
			out.Status = spi.StatusDeclined
		}
	}
	if out.DeclineReason == "" && out.Status == spi.StatusDeclined {
		out.DeclineReason = payment.DeclineUnknown
	}
	return out, nil
}

// headerValue reads a header case-insensitively. Ingress layers normalize header casing
// inconsistently — net/http canonicalizes, a proxy may not — and a verifier that misses the
// signature because of casing rejects every webhook from an otherwise healthy gateway.
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

// Sign produces the `Stripe-Signature` header for a body.
//
// It exists so the simulator can emit webhooks that this verifier accepts, and so the contract
// suite can construct valid, tampered and stale signatures without duplicating the construction.
// Having one implementation of the signing scheme means a test cannot pass against a signer that
// disagrees with the verifier.
func Sign(body []byte, secret string, at time.Time) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
