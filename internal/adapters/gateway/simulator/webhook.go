package simulator

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/adyen"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/stripe"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// WebhookScheme names the signature construction the simulator emulates when it emits an event.
//
// Emulating a real vendor's scheme is the point: a simulator that invented its own signing would
// exercise the ingress plumbing but never the vendor verifier that actually runs in production, and
// the vendor verifiers are where the subtle bugs live — Adyen's field escaping, Stripe's timestamp
// tolerance, the constant-time compare.
type WebhookScheme string

const (
	// SchemeSimulator is the simulator's own construction: HMAC-SHA256 over `timestamp + "." + body`
	// in an X-Sim-Signature header. Structurally Stripe's, under a different header, so the
	// simulator can be verified without pretending to be Stripe.
	SchemeSimulator WebhookScheme = "simulator"
	// SchemeStripe emits a genuine `Stripe-Signature` header, produced by the Stripe adapter's own
	// signer. Using that signer rather than a reimplementation is deliberate: a test cannot pass
	// against a simulator that agrees with a wrong idea of the scheme.
	SchemeStripe WebhookScheme = "stripe"
	// SchemeAdyen emits a genuine Adyen notification with `additionalData.hmacSignature`, produced
	// by the Adyen adapter's own signer over the escaped, pipe-delimited projection.
	SchemeAdyen WebhookScheme = "adyen"
)

// SignatureHeader is the simulator's own signature header.
const SignatureHeader = "X-Sim-Signature"

// EmittedWebhook is one event the simulator produced, ready to be posted at an ingress endpoint.
type EmittedWebhook struct {
	Scheme  WebhookScheme
	Headers map[string]string
	Body    []byte
	// EventID is the identifier the platform will deduplicate on. It is exposed so a test can assert
	// that a duplicate emission really does carry the same id rather than merely the same amount.
	EventID string
}

// simEvent is the simulator's own event envelope. It carries an explicit id so the deduplication
// key is stable by construction rather than derived, which is what makes ScenarioDuplicateWebhook a
// meaningful test: the two emissions are byte-identical.
type simEvent struct {
	ID             string      `json:"id"`
	Type           string      `json:"type"`
	CreatedUnix    int64       `json:"created"`
	Reference      string      `json:"reference"`
	IdempotencyKey string      `json:"idempotencyKey,omitempty"`
	PaymentID      string      `json:"paymentId,omitempty"`
	RefundID       string      `json:"refundId,omitempty"`
	Status         string      `json:"status"`
	DeclineCode    string      `json:"declineCode,omitempty"`
	Amount         *WireAmount `json:"amount,omitempty"`
}

// EmitWebhook produces a signed event for a stored transaction.
//
// `duplicate` asks for the event to be produced twice, byte for byte and signature for signature.
// That is what a real gateway's redelivery looks like — not a second event with a new id — and it is
// the only form of duplicate that actually tests the platform's deduplication.
func (e *Engine) EmitWebhook(reference, eventType string, duplicate bool) ([]EmittedWebhook, error) {
	e.mu.Lock()
	rec, ok := e.byRef[reference]
	var key string
	for k, v := range e.byKey {
		if v == rec {
			key = k
			break
		}
	}
	e.mu.Unlock()
	if !ok {
		return nil, apierror.Newf(apierror.CodePaymentNotFound,
			"simulator: no stored transaction with reference %q", reference)
	}

	now := e.opts.Clock.Now()
	ev := simEvent{
		ID:             "sim_evt_" + hexOf(reference+"|"+eventType),
		Type:           eventType,
		CreatedUnix:    now.Unix(),
		Reference:      reference,
		IdempotencyKey: key,
		Status:         rec.Status,
		DeclineCode:    rec.DeclineCode,
		Amount:         firstAmount(rec),
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "simulator: the event could not be encoded")
	}

	one, err := e.sign(ev, body, now)
	if err != nil {
		return nil, err
	}
	out := []EmittedWebhook{one}
	if duplicate {
		out = append(out, one)
	}

	e.mu.Lock()
	e.emitted = append(e.emitted, out...)
	e.mu.Unlock()
	return out, nil
}

// Emitted returns every webhook the simulator has produced, for assertions.
func (e *Engine) Emitted() []EmittedWebhook {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]EmittedWebhook(nil), e.emitted...)
}

func (e *Engine) sign(ev simEvent, body []byte, now time.Time) (EmittedWebhook, error) {
	switch e.opts.Scheme {
	case SchemeStripe:
		// The Stripe adapter's own signer, over a Stripe-shaped envelope. Reusing stripe.Sign is
		// what makes "the simulator emits signatures the real verifier accepts" true by
		// construction rather than by coincidence.
		stripeBody, err := json.Marshal(map[string]any{
			"id":      ev.ID,
			"object":  "event",
			"type":    stripeEventType(ev.Type),
			"created": ev.CreatedUnix,
			"data": map[string]any{"object": map[string]any{
				"id":       ev.Reference,
				"object":   "payment_intent",
				"amount":   amountMinor(ev.Amount),
				"currency": strings.ToLower(amountCurrency(ev.Amount)),
				"status":   "succeeded",
				"metadata": map[string]string{"pp_idempotency_key": ev.IdempotencyKey},
			}},
		})
		if err != nil {
			return EmittedWebhook{}, apierror.Wrap(err, apierror.CodeInternalError,
				"simulator: the Stripe-shaped event could not be encoded")
		}
		return EmittedWebhook{
			Scheme:  SchemeStripe,
			Headers: map[string]string{stripe.SignatureHeader: stripe.Sign(stripeBody, e.opts.WebhookSecret, now)},
			Body:    stripeBody,
			EventID: ev.ID,
		}, nil

	case SchemeAdyen:
		item := map[string]any{
			"pspReference":        ev.Reference,
			"originalReference":   "",
			"merchantAccountCode": e.opts.MerchantAccount,
			"merchantReference":   ev.IdempotencyKey,
			"amount": map[string]any{
				"value": amountMinor(ev.Amount), "currency": amountCurrency(ev.Amount),
			},
			"eventCode": adyenEventCode(ev.Type),
			"eventDate": now.UTC().Format(time.RFC3339),
			"success":   "true",
		}
		signed := adyen.SignedPayload(
			ev.Reference, "", e.opts.MerchantAccount, ev.IdempotencyKey,
			strconv.FormatInt(amountMinor(ev.Amount), 10), amountCurrency(ev.Amount),
			adyenEventCode(ev.Type), "true")
		sig, err := adyen.Sign(signed, e.opts.WebhookSecret)
		if err != nil {
			return EmittedWebhook{}, err
		}
		item["additionalData"] = map[string]string{
			"hmacSignature":      sig,
			"pp_idempotency_key": ev.IdempotencyKey,
		}
		adyenBody, err := json.Marshal(map[string]any{
			"live":              "false",
			"notificationItems": []any{map[string]any{"NotificationRequestItem": item}},
		})
		if err != nil {
			return EmittedWebhook{}, apierror.Wrap(err, apierror.CodeInternalError,
				"simulator: the Adyen-shaped event could not be encoded")
		}
		return EmittedWebhook{Scheme: SchemeAdyen, Headers: map[string]string{}, Body: adyenBody, EventID: ev.ID}, nil

	default:
		return EmittedWebhook{
			Scheme:  SchemeSimulator,
			Headers: map[string]string{SignatureHeader: Sign(body, e.opts.WebhookSecret, now)},
			Body:    body,
			EventID: ev.ID,
		}, nil
	}
}

// Sign produces the simulator's own signature header value: `t=<unix>,v1=<hex hmac>` over
// `timestamp + "." + body`. Exported so the contract suite can construct valid, tampered and stale
// fixtures against the same implementation the verifier uses.
func Sign(body []byte, secret string, at time.Time) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify authenticates a simulator webhook.
//
// It implements the same discipline the vendor verifiers do, and for the same reasons: the
// signature is checked with a constant-time compare before the body is parsed, the timestamp
// tolerance rejects replays, and the whole rotation set is tried so a secret rotation does not drop
// events. The simulator holds itself to the contract suite's webhook assertions rather than being
// exempt from them, which is what makes a green simulator run meaningful.
func (e *Engine) Verify(ctx context.Context, raw []byte, headers map[string]string, secrets []string, now time.Time) (*spi.WebhookEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	if len(secrets) == 0 {
		return nil, apierror.New(apierror.CodeGatewayNotConfigured,
			"simulator: no webhook signing secret is configured")
	}
	header := headerValue(headers, SignatureHeader)
	if header == "" {
		return nil, apierror.New(apierror.CodeWebhookSignatureInvalid,
			"simulator: the X-Sim-Signature header is absent")
	}
	ts, candidate, err := parseSignatureHeader(header)
	if err != nil {
		return nil, err
	}

	signed := make([]byte, 0, len(ts)+1+len(raw))
	signed = append(signed, ts...)
	signed = append(signed, '.')
	signed = append(signed, raw...)

	matched := false
	for _, s := range secrets {
		if s == "" {
			continue
		}
		mac := hmac.New(sha256.New, []byte(s))
		mac.Write(signed)
		// Every secret is evaluated rather than short-circuiting, so the work done does not depend
		// on which secret matched and the rotation state stays out of the timing channel.
		if hmac.Equal(mac.Sum(nil), candidate) {
			matched = true
		}
	}
	if !matched {
		return nil, apierror.New(apierror.CodeWebhookSignatureInvalid,
			"simulator: no configured signing secret produces this signature")
	}

	secs, convErr := strconv.ParseInt(string(ts), 10, 64)
	if convErr != nil {
		return nil, apierror.New(apierror.CodeWebhookSignatureInvalid,
			"simulator: the signature timestamp is not an integer")
	}
	tolerance := e.opts.SlowDelay
	if tolerance < 5*time.Minute {
		tolerance = 5 * time.Minute
	}
	age := now.Sub(time.Unix(secs, 0))
	if age < 0 {
		age = -age
	}
	if age > tolerance {
		return nil, apierror.Newf(apierror.CodeWebhookReplayDetected,
			"simulator: the webhook timestamp is %s outside the %s tolerance",
			age.Truncate(time.Second), tolerance)
	}

	var ev simEvent
	if err := decodeStrict(raw, &ev); err != nil {
		return nil, err
	}
	if ev.ID == "" {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"simulator: the webhook body carries no event id")
	}
	out := &spi.WebhookEvent{
		GatewayEventID: ev.ID,
		EventType:      ev.Type,
		Kind:           webhookKind(ev.Type),
		GatewayRef:     ev.Reference,
		IdempotencyKey: ev.IdempotencyKey,
		PaymentID:      shared.PaymentID(ev.PaymentID),
		RefundID:       shared.RefundID(ev.RefundID),
		Status:         spi.Status(ev.Status),
		OccurredAt:     time.Unix(ev.CreatedUnix, 0).UTC(),
		Raw:            append([]byte(nil), raw...),
	}
	if m, ok := toMoney(ev.Amount); ok {
		out.Amount = &m
	}
	if out.Status == spi.StatusDeclined {
		out.DeclineReason = mapDecline(ev.DeclineCode)
	}
	return out, nil
}

func parseSignatureHeader(h string) (ts []byte, candidate []byte, err error) {
	for _, part := range strings.Split(h, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = []byte(v)
		case "v1":
			b, decErr := hex.DecodeString(v)
			if decErr == nil {
				candidate = b
			}
		}
	}
	if len(ts) == 0 || len(candidate) == 0 {
		return nil, nil, apierror.New(apierror.CodeWebhookSignatureInvalid,
			"simulator: the signature header is malformed")
	}
	return ts, candidate, nil
}

// webhookKind classifies a simulator event type. Unrecognised types are ignored rather than
// rejected, mirroring the vendor adapters — an ingress that errors on an unknown type turns a new
// event into an incident.
func webhookKind(eventType string) spi.WebhookKind {
	switch eventType {
	case "payment.authorized":
		return spi.KindAuthorizationSucceeded
	case "payment.declined":
		return spi.KindAuthorizationFailed
	case "payment.captured":
		return spi.KindCaptureSucceeded
	case "payment.capture_failed":
		return spi.KindCaptureFailed
	case "refund.accepted", "refund.settled":
		return spi.KindRefundSucceeded
	case "refund.failed":
		return spi.KindRefundFailed
	case "payment.voided":
		return spi.KindVoidSucceeded
	case "dispute.opened":
		return spi.KindDisputeOpened
	case "dispute.closed":
		return spi.KindDisputeClosed
	case "payout.settled":
		return spi.KindPayoutSettled
	case "account.updated":
		return spi.KindAccountUpdated
	default:
		return spi.KindIgnored
	}
}

func stripeEventType(t string) string {
	switch t {
	case "payment.captured":
		return "payment_intent.succeeded"
	case "payment.declined":
		return "payment_intent.payment_failed"
	case "payment.voided":
		return "payment_intent.canceled"
	default:
		return "payment_intent.succeeded"
	}
}

func adyenEventCode(t string) string {
	switch t {
	case "payment.captured":
		return "CAPTURE"
	case "refund.accepted", "refund.settled":
		return "REFUND"
	case "payment.voided":
		return "CANCELLATION"
	default:
		return "AUTHORISATION"
	}
}

func firstAmount(r *WireResponse) *WireAmount {
	if r.CapturedAmount != nil {
		return r.CapturedAmount
	}
	return r.AuthorizedAmount
}

func amountMinor(a *WireAmount) int64 {
	if a == nil {
		return 0
	}
	return a.MinorUnits
}

func amountCurrency(a *WireAmount) string {
	if a == nil {
		return string(money.Currency("USD"))
	}
	return a.Currency
}

func hexOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:12])
}

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
