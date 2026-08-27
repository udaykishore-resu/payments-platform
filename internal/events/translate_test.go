package events

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

func paymentEvent(typ payment.EventType, payload map[string]any) payment.Event {
	return payment.Event{
		Type:        typ,
		PaymentID:   shared.PaymentID(testPayment),
		TenantID:    shared.TenantID(testTenant),
		MerchantID:  shared.MerchantID(testMerchant),
		OccurredAt:  time.Date(2026, 8, 26, 14, 3, 11, 412_000_000, time.UTC),
		Version:     4,
		Payload:     payload,
		Correlation: testRequest,
	}
}

func merchantEvent(typ merchant.EventType, payload map[string]any) merchant.Event {
	return merchant.Event{
		Type:       typ,
		MerchantID: shared.MerchantID(testMerchant),
		TenantID:   shared.TenantID(testTenant),
		OccurredAt: time.Date(2026, 8, 27, 19, 0, 0, 512_000_000, time.UTC),
		Version:    7,
		Payload:    payload,
	}
}

func decodeData(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	return m
}

func TestTranslatePaymentProducesAnOutboxMessage(t *testing.T) {
	t.Parallel()
	msg, published, err := TranslatePayment(provenanceCtx(), paymentEvent(payment.EventPaymentCaptured, map[string]any{
		"attemptId": testAttempt,
	}))
	if err != nil || !published {
		t.Fatalf("TranslatePayment: published=%v err=%v", published, err)
	}
	if msg.Topic != "pp.payments.payment.v1" {
		t.Errorf("topic = %q", msg.Topic)
	}
	if msg.PartitionKey != testPayment {
		t.Errorf("partition key = %q, want the payment id", msg.PartitionKey)
	}
	if msg.Type != "payment.captured.v1" {
		t.Errorf("type = %q", msg.Type)
	}
	if msg.TenantID != shared.TenantID(testTenant) {
		t.Errorf("tenant = %q", msg.TenantID)
	}
	if msg.Headers[HeaderEventType] != msg.Type ||
		msg.Headers[HeaderTenantID] != testTenant ||
		msg.Headers[HeaderTraceParent] != testTrace {
		t.Errorf("broker headers do not carry type/tenant/traceparent: %v", msg.Headers)
	}
	if !msg.AvailableAt.IsZero() {
		t.Errorf("a first publication must not be delayed: %v", msg.AvailableAt)
	}

	env, err := Decode(msg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if env.CorrelationID != testRequest {
		t.Errorf("correlation from the domain event was not used: %q", env.CorrelationID)
	}
	if env.AggregateVersion != 4 {
		t.Errorf("aggregateversion = %d", env.AggregateVersion)
	}
}

// TestAttemptEventsCarryTheAttemptAsSubject pins the one case where subject and aggregateid
// legitimately differ.
func TestAttemptEventsCarryTheAttemptAsSubject(t *testing.T) {
	t.Parallel()
	msg, _, err := TranslatePayment(provenanceCtx(), paymentEvent(payment.EventPaymentAttempted, map[string]any{
		"attemptId": testAttempt,
	}))
	if err != nil {
		t.Fatalf("TranslatePayment: %v", err)
	}
	env, err := Decode(msg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if env.Subject != testAttempt {
		t.Errorf("subject = %q, want the attempt id", env.Subject)
	}
	if env.AggregateID != testPayment || env.PartitionKey != testPayment {
		t.Errorf("an attempt event must still be ordered under its payment: %+v", env)
	}
}

// TestRemappedTypesCarryTheirDerivedFields covers the domain types that reach the wire as a
// different catalog type.
func TestRemappedTypesCarryTheirDerivedFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		domain   payment.EventType
		wantType string
		wantData map[string]any
	}{
		{"canceled", payment.EventPaymentCanceled, "payment.failed.v1",
			map[string]any{"failureStage": "CANCELED", "terminal": true}},
		{"expired", payment.EventPaymentExpired, "payment.failed.v1",
			map[string]any{"failureStage": "EXPIRY", "terminal": true}},
		{"requires action", payment.EventPaymentRequiresAction, "payment.attempted.v1",
			map[string]any{"state": "DISPATCHED"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg, published, err := TranslatePayment(provenanceCtx(), paymentEvent(tc.domain, map[string]any{
				"attemptId": testAttempt,
			}))
			if err != nil || !published {
				t.Fatalf("published=%v err=%v", published, err)
			}
			if msg.Type != tc.wantType {
				t.Fatalf("type = %q, want %q", msg.Type, tc.wantType)
			}
			env, err := Decode(msg)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			data := decodeData(t, env.Data)
			for k, want := range tc.wantData {
				if data[k] != want {
					t.Errorf("data[%s] = %v, want %v", k, data[k], want)
				}
			}
		})
	}
}

// TestDerivedDefaultsDoNotOverwriteTheDomain is the difference between a default and a lie.
func TestDerivedDefaultsDoNotOverwriteTheDomain(t *testing.T) {
	t.Parallel()
	msg, _, err := TranslatePayment(provenanceCtx(), paymentEvent(payment.EventPaymentFailed, map[string]any{
		"failureStage": "GATEWAY",
		"errorCode":    "GATEWAY_DECLINED",
	}))
	if err != nil {
		t.Fatalf("TranslatePayment: %v", err)
	}
	env, _ := Decode(msg)
	if got := decodeData(t, env.Data)["failureStage"]; got != "GATEWAY" {
		t.Fatalf("failureStage was overwritten to %v", got)
	}
}

// TestTranslationDoesNotMutateTheAggregatesPayload guards the copy in derive(). A translation
// that mutated the map would change what the domain believes it raised.
func TestTranslationDoesNotMutateTheAggregatesPayload(t *testing.T) {
	t.Parallel()
	payload := map[string]any{"attemptId": testAttempt}
	ev := paymentEvent(payment.EventPaymentCanceled, payload)

	if _, _, err := TranslatePayment(provenanceCtx(), ev); err != nil {
		t.Fatalf("TranslatePayment: %v", err)
	}
	if len(payload) != 1 {
		t.Fatalf("the domain event's payload was mutated: %v", payload)
	}
	if _, injected := payload["failureStage"]; injected {
		t.Fatal("derive wrote into the aggregate's map")
	}
}

func TestSuppressedDomainEventsAreNotPublished(t *testing.T) {
	t.Parallel()
	for _, dt := range []merchant.EventType{
		merchant.EventMerchantValidationFailed,
		merchant.EventMerchantProvisioningFailed,
		merchant.EventMerchantCertificationFailed,
		merchant.EventMerchantComplianceRejected,
	} {
		msg, published, err := TranslateMerchant(provenanceCtx(), merchantEvent(dt, nil))
		if err != nil {
			t.Errorf("%s: unexpected error %v", dt, err)
		}
		if published {
			t.Errorf("%s: suppressed type was published as %s", dt, msg.Type)
		}
	}
}

func TestReinstatementIsPublishedAsAnActivation(t *testing.T) {
	t.Parallel()
	msg, published, err := TranslateMerchant(provenanceCtx(), merchantEvent(merchant.EventMerchantReinstated, nil))
	if err != nil || !published {
		t.Fatalf("published=%v err=%v", published, err)
	}
	if msg.Type != "merchant.activated.v1" {
		t.Fatalf("type = %q", msg.Type)
	}
	env, _ := Decode(msg)
	if got := decodeData(t, env.Data)["previousState"]; got != "SUSPENDED" {
		t.Fatalf("previousState = %v, want SUSPENDED", got)
	}
	if msg.PartitionKey != testMerchant {
		t.Fatalf("partition key = %q, want the merchant id", msg.PartitionKey)
	}
}

func TestTranslateEventListsDropSuppressedTypes(t *testing.T) {
	t.Parallel()
	msgs, err := TranslateMerchantEvents(provenanceCtx(), []merchant.Event{
		merchantEvent(merchant.EventMerchantCreated, nil),
		merchantEvent(merchant.EventMerchantValidationFailed, nil),
		merchantEvent(merchant.EventMerchantActivated, nil),
	})
	if err != nil {
		t.Fatalf("TranslateMerchantEvents: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
}

func TestEncodeRejectsAnOversizedEnvelope(t *testing.T) {
	t.Parallel()
	// E8: an envelope over the cap must be refused by the producer, not by the broker, where it
	// would wedge the relay behind a record that can never be delivered.
	big := make([]byte, MaxEnvelopeBytes)
	for i := range big {
		big[i] = 'x'
	}
	f := validFact()
	f.data = map[string]any{"blob": string(big)}
	env, err := NewEnvelope(provenanceCtx(), f)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if _, err := Encode(env); err == nil {
		t.Fatal("Encode accepted an envelope over the 256 KiB cap")
	}
}

func TestDecodeRejectsAPoisonBody(t *testing.T) {
	t.Parallel()
	msg, _, err := TranslatePayment(provenanceCtx(), paymentEvent(payment.EventPaymentCaptured, nil))
	if err != nil {
		t.Fatalf("TranslatePayment: %v", err)
	}
	msg.Payload = []byte(`{"specversion":`)
	if _, err := Decode(msg); err == nil {
		t.Fatal("Decode accepted a truncated body")
	}
}

func TestDecodeDataIgnoresUnknownFields(t *testing.T) {
	t.Parallel()
	// Rule V6: a strict-decode consumer turns an additive change into an outage.
	env := mustEnvelope(t)
	env.Data = json.RawMessage(`{"gatewayCode":"stripe","settlementCurrency":"EUR"}`)
	var into struct {
		GatewayCode string `json:"gatewayCode"`
	}
	if err := DecodeData(env, &into); err != nil {
		t.Fatalf("DecodeData rejected an unknown field: %v", err)
	}
	if into.GatewayCode != "stripe" {
		t.Fatalf("gatewayCode = %q", into.GatewayCode)
	}
}

func TestEncodeFactIsNewEnvelopePlusEncode(t *testing.T) {
	t.Parallel()
	msg, err := EncodeFact(provenanceCtx(), validFact())
	if err != nil {
		t.Fatalf("EncodeFact: %v", err)
	}
	if msg.Headers[HeaderContentType] != DataContentType {
		t.Fatalf("content type header missing: %v", msg.Headers)
	}
	if msg.Headers[HeaderMerchantID] != testMerchant {
		t.Fatalf("merchant header missing: %v", msg.Headers)
	}
}

func TestPlatformScopedEventsOmitTheMerchantHeader(t *testing.T) {
	t.Parallel()
	// An empty header value would silently match every filter written against it.
	f := validFact()
	f.typ = "gateway.health_changed.v1"
	f.merchant = ""
	f.subject = "gw_01JB8Z99999999999999999999"
	f.aggID = f.subject
	f.key = f.subject
	msg, err := EncodeFact(provenanceCtx(), f)
	if err != nil {
		t.Fatalf("EncodeFact: %v", err)
	}
	if _, present := msg.Headers[HeaderMerchantID]; present {
		t.Fatalf("platform-scoped event carries a merchant header: %v", msg.Headers)
	}
}
