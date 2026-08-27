package adyen

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Verifier authenticates inbound Adyen notifications.
//
// Adyen's scheme differs from Stripe's in a way that matters for the security argument, and the
// difference is stated plainly rather than papered over:
//
//   - Stripe signs the raw body, so the signature can be checked before a parser ever sees the
//     bytes. Adyen signs a *projection* of eight named fields, which means the body must be parsed
//     far enough to extract them before the HMAC can be computed. There is no way to verify first.
//   - The mitigation is to keep that pre-authentication parse as small and as boring as possible:
//     a fixed envelope, decoded by encoding/json with a hard size cap already applied by the
//     transport, into a struct with no custom unmarshallers. That is a much smaller surface than
//     the full event normalization, which runs only after the HMAC checks out.
//   - Adyen's signed projection carries no timestamp, so the HMAC alone does not stop a replay.
//     The replay control is therefore two-part: an `eventDate` freshness window enforced here, and
//     the platform's event-ID deduplication above. This is genuinely weaker than Stripe's
//     construction and is called out so nobody assumes otherwise.
type Verifier struct {
	tolerance time.Duration
}

var _ spi.WebhookVerifier = (*Verifier)(nil)

// NewVerifier builds the notification verifier.
func NewVerifier(cfg spi.Config) (*Verifier, error) {
	tol := cfg.WebhookTolerance
	if tol <= 0 {
		tol = 5 * time.Minute
	}
	return &Verifier{tolerance: tol}, nil
}

// ID returns the registry slug.
func (v *Verifier) ID() shared.GatewayID { return GatewayID }

// AcknowledgementBody is what the notification endpoint must return to Adyen.
//
// Adyen requires the literal string `[accepted]` in the response body. An HTTP 200 with any other
// body — including an empty one — is treated as a failed delivery, and Adyen will redeliver the
// notification on an escalating schedule for days and eventually disable the endpoint. It is
// returned as a method on the verifier so the ingress cannot get it right for one gateway and
// wrong for another: whatever verified the event also says how to acknowledge it.
func (v *Verifier) AcknowledgementBody() []byte { return []byte("[accepted]") }

// Verify authenticates an Adyen notification batch and normalizes its first item.
//
// Adyen batches notifications: one POST can carry several NotificationRequestItems, each with its
// own HMAC. This method authenticates *every* item and returns the first — an ingress that
// accepted a batch because item one verified while item two was forged would be exactly the hole
// the signature exists to close. Callers that need the whole batch use VerifyAll.
func (v *Verifier) Verify(ctx context.Context, raw []byte, headers map[string]string, secrets []string, now time.Time) (*spi.WebhookEvent, error) {
	events, err := v.VerifyAll(ctx, raw, headers, secrets, now)
	if err != nil {
		return nil, err
	}
	return events[0], nil
}

// VerifyAll authenticates and normalizes every item in a notification batch.
//
// It returns an error if any item fails, and never returns a partially-verified batch. The
// alternative — returning the good items and reporting the bad ones — would leave the ingress
// deciding whether to acknowledge, and an acknowledgement tells Adyen never to send the batch
// again, including the item that failed.
func (v *Verifier) VerifyAll(ctx context.Context, raw []byte, headers map[string]string, secrets []string, now time.Time) ([]*spi.WebhookEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	if len(secrets) == 0 {
		return nil, apierror.New(apierror.CodeGatewayNotConfigured,
			"adyen: no webhook HMAC key is configured for this gateway")
	}
	keys, err := decodeHMACKeys(secrets)
	if err != nil {
		return nil, err
	}

	// The minimal pre-authentication parse. See the type comment for why it is unavoidable and
	// what keeps it small.
	var batch notification
	if err := decode(raw, &batch); err != nil {
		return nil, apierror.Wrap(err, apierror.CodeMalformedRequest,
			"adyen: the notification body is not a parseable notification envelope")
	}
	if len(batch.NotificationItems) == 0 {
		return nil, apierror.New(apierror.CodeMalformedRequest,
			"adyen: the notification body carries no notificationItems")
	}

	out := make([]*spi.WebhookEvent, 0, len(batch.NotificationItems))
	for i, wrapper := range batch.NotificationItems {
		item := wrapper.Item
		if item == nil {
			return nil, apierror.Newf(apierror.CodeMalformedRequest,
				"adyen: notificationItems[%d] carries no NotificationRequestItem", i)
		}
		signed := SignedPayload(item.PSPReference, item.OriginalReference, item.MerchantAccount,
			item.MerchantReference, amountValueString(item.Amount), amountCurrencyString(item.Amount),
			item.EventCode, item.Success)

		presented := item.AdditionalData["hmacSignature"]
		if presented == "" {
			return nil, apierror.Newf(apierror.CodeWebhookSignatureInvalid,
				"adyen: notificationItems[%d] carries no additionalData.hmacSignature", i)
		}
		presentedBytes, decErr := base64.StdEncoding.DecodeString(presented)
		if decErr != nil {
			return nil, apierror.Newf(apierror.CodeWebhookSignatureInvalid,
				"adyen: notificationItems[%d] carries a signature that is not base64", i)
		}
		if !anyKeyMatches([]byte(signed), presentedBytes, keys) {
			return nil, apierror.Newf(apierror.CodeWebhookSignatureInvalid,
				"adyen: no configured HMAC key produces the signature on notificationItems[%d]", i)
		}

		// Freshness. Adyen's signed projection has no timestamp, so this is checked against the
		// event's own eventDate — which is inside the authenticated set only in the sense that a
		// forger who cannot produce a valid HMAC cannot change it either. An item with no parseable
		// eventDate is rejected rather than accepted: an event with no time is an event with no
		// replay protection at all.
		occurred, tsErr := parseEventDate(item.EventDate)
		if tsErr != nil {
			return nil, tsErr
		}
		age := now.Sub(occurred)
		if age < 0 {
			age = -age
		}
		if age > v.tolerance {
			return nil, apierror.Newf(apierror.CodeWebhookReplayDetected,
				"adyen: the notification eventDate is %s outside the %s tolerance",
				age.Truncate(time.Second), v.tolerance)
		}

		out = append(out, normalizeItem(item, signed, occurred, raw))
	}
	return out, nil
}

// VerifyBasicAuth authenticates Adyen to the platform on the notification endpoint.
//
// It is a separate method from Verify, not folded into it, because it is a different control at a
// different layer: basic auth is checked by the ingress before the body is read at all, and it is
// what stops an unauthenticated caller making the platform do the HMAC work in the first place.
// Folding it into Verify would mean the transport credential had to travel through the SPI's
// `secrets []string` parameter, which is documented as signing material — a type confusion nobody
// benefits from.
//
// Both comparisons are constant-time. A variable-time password compare on an endpoint an attacker
// can call at will is a practical oracle, not a theoretical one.
func (v *Verifier) VerifyBasicAuth(headers map[string]string, expectedUser, expectedPassword string) error {
	if expectedUser == "" && expectedPassword == "" {
		return apierror.New(apierror.CodeGatewayNotConfigured,
			"adyen: no basic-auth credentials are configured for the notification endpoint")
	}
	authz := headerValue(headers, "Authorization")
	const prefix = "Basic "
	if !strings.HasPrefix(authz, prefix) {
		return apierror.New(apierror.CodeUnauthenticated,
			"adyen: the notification endpoint requires basic authentication")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(authz[len(prefix):]))
	if err != nil {
		return apierror.New(apierror.CodeUnauthenticated,
			"adyen: the Authorization header is not valid basic authentication")
	}
	user, pass, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return apierror.New(apierror.CodeUnauthenticated,
			"adyen: the Authorization header is not valid basic authentication")
	}
	userOK := hmac.Equal([]byte(user), []byte(expectedUser))
	passOK := hmac.Equal([]byte(pass), []byte(expectedPassword))
	if !userOK || !passOK {
		return apierror.New(apierror.CodeUnauthenticated,
			"adyen: the notification credentials were rejected")
	}
	return nil
}

// SignedPayload builds Adyen's signed projection.
//
// The construction is: eight fields, each escaped, joined with ':'. The escaping rule is the part
// that is easy to get wrong and impossible to notice: inside each *value*, a backslash becomes
// `\\` and a colon becomes `\:`. Without it, a merchant reference containing a colon — which is
// legal, and which appears the moment somebody uses a namespaced order id like `shop:1234` —
// shifts every subsequent field one position left in the joined string, the HMAC no longer
// matches, and every notification for that merchant is rejected as forged. Worse in the other
// direction: without escaping, two different events can produce the *same* signed string, which is
// a signature-collision bug rather than merely a availability one.
//
// The order is fixed by Adyen and is not alphabetical:
// pspReference, originalReference, merchantAccountCode, merchantReference, amount.value,
// amount.currency, eventCode, success.
func SignedPayload(pspReference, originalReference, merchantAccountCode, merchantReference,
	amountValue, amountCurrency, eventCode, success string) string {

	parts := []string{
		escapeField(pspReference),
		escapeField(originalReference),
		escapeField(merchantAccountCode),
		escapeField(merchantReference),
		escapeField(amountValue),
		escapeField(amountCurrency),
		escapeField(eventCode),
		escapeField(success),
	}
	return strings.Join(parts, ":")
}

// escapeField applies Adyen's escaping to one field value.
//
// Byte-wise rather than rune-wise on purpose: the two characters being escaped are both ASCII, and
// iterating bytes means a value carrying invalid UTF-8 — which a merchant reference copied from a
// legacy system genuinely can — is escaped identically to how Adyen's own implementation escapes
// it, rather than being mangled through the replacement character first.
func escapeField(s string) string {
	if !strings.ContainsAny(s, `\:`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' || c == ':' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

// Sign produces the base64 HMAC for a signed payload, given the hex-encoded key.
//
// Exported so the simulator can emit Adyen-shaped notifications this verifier accepts and so the
// contract suite can build valid, tampered and stale fixtures. One implementation of the scheme
// means a test cannot pass against a signer that disagrees with the verifier.
func Sign(signedPayload string, hexKey string) (string, error) {
	key, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil {
		return "", apierror.New(apierror.CodeConfigurationInvalid,
			"adyen: the HMAC key is not valid hexadecimal")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signedPayload))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// decodeHMACKeys hex-decodes the rotation set.
//
// Adyen's HMAC key is generated as a hex string and must be decoded to bytes before use. Using the
// hex *string* as the key material is the classic error: it produces a verifier that is
// self-consistent — it will happily verify anything it signed itself — and that rejects every
// genuine Adyen notification. A test that signs with the same helper never catches it, which is
// why the simulator signs through Sign above, which decodes identically.
func decodeHMACKeys(secrets []string) ([][]byte, error) {
	out := make([][]byte, 0, len(secrets))
	for _, s := range secrets {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		k, err := hex.DecodeString(s)
		if err != nil {
			// A key that will not decode is a configuration error, but it must not take down
			// verification for the other keys in the rotation set: skip it and fail only if
			// nothing usable remains.
			continue
		}
		out = append(out, k)
	}
	if len(out) == 0 {
		return nil, apierror.New(apierror.CodeConfigurationInvalid,
			"adyen: no configured HMAC key is valid hexadecimal")
	}
	return out, nil
}

// anyKeyMatches performs the constant-time comparison across the rotation set, evaluating every
// key so that the total work does not reveal which one matched.
func anyKeyMatches(signedPayload, presented []byte, keys [][]byte) bool {
	matched := false
	for _, k := range keys {
		mac := hmac.New(sha256.New, k)
		mac.Write(signedPayload)
		if hmac.Equal(mac.Sum(nil), presented) {
			matched = true
		}
	}
	return matched
}

// parseEventDate reads Adyen's eventDate, which is RFC 3339 with an offset.
func parseEventDate(s string) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, apierror.New(apierror.CodeWebhookReplayDetected,
			"adyen: the notification carries no eventDate, so its freshness cannot be established")
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, apierror.New(apierror.CodeWebhookReplayDetected,
			"adyen: the notification eventDate is not a valid RFC 3339 timestamp")
	}
	return t.UTC(), nil
}

// normalizeItem turns an authenticated notification item into the platform's event.
//
// GatewayEventID is synthesised, because Adyen does not supply one. It is the SHA-256 of the
// signed projection plus the eventDate, which gives the property the SPI requires: the same
// payload always yields the same identifier, so the ingress's deduplication actually deduplicates
// Adyen's own redeliveries — which are frequent, because Adyen redelivers anything not
// acknowledged with `[accepted]`.
func normalizeItem(item *notificationRequestItem, signed string, occurred time.Time, raw []byte) *spi.WebhookEvent {
	success := strings.EqualFold(item.Success, "true")
	sum := sha256.Sum256([]byte(signed + "|" + item.EventDate))

	ev := &spi.WebhookEvent{
		GatewayEventID: "adyen_" + hex.EncodeToString(sum[:16]),
		EventType:      item.EventCode,
		Kind:           webhookKind(item.EventCode, success),
		GatewayRef:     item.PSPReference,
		IdempotencyKey: item.AdditionalData[metaIdempotencyKey],
		PaymentID:      shared.PaymentID(item.AdditionalData[metaPaymentID]),
		RefundID:       shared.RefundID(item.AdditionalData[metaRefundID]),
		OccurredAt:     occurred,
		Raw:            append([]byte(nil), raw...),
	}
	if item.Amount != nil {
		if m, err := money.New(item.Amount.Value, money.Currency(upperTrim(item.Amount.Currency))); err == nil {
			ev.Amount = &m
		}
	}
	switch ev.Kind {
	case spi.KindAuthorizationSucceeded:
		ev.Status = spi.StatusAuthorized
	case spi.KindCaptureSucceeded:
		ev.Status = spi.StatusCaptured
	case spi.KindRefundSucceeded:
		ev.Status = spi.StatusRefundAccepted
	case spi.KindVoidSucceeded:
		ev.Status = spi.StatusVoided
	case spi.KindAuthorizationFailed:
		ev.Status = spi.StatusDeclined
		ev.DeclineReason = declineReasonForItem(item)
	case spi.KindCaptureFailed, spi.KindRefundFailed:
		ev.Status = spi.StatusFailed
	default:
		// The remaining kinds — payouts, disputes, account updates and the ignored bucket — carry no
		// payment status of their own. Leaving ev.Status empty is the accurate answer; inventing one
		// would make the processor advance a payment on a dispute notification
	}
	return ev
}

func amountValueString(a *amount) string {
	if a == nil {
		return ""
	}
	return strconv.FormatInt(a.Value, 10)
}

func amountCurrencyString(a *amount) string {
	if a == nil {
		return ""
	}
	return a.Currency
}

// headerValue reads a header case-insensitively; proxies normalize casing inconsistently and a
// verifier that misses a header because of casing rejects every notification.
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
