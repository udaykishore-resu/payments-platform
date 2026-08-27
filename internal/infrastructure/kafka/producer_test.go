package kafka

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/events"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

const (
	testTopic     = "pp.payments.payment.v1"
	testEventID   = "evt_01JB8Z9K2QW3E4R5T6Y7U8I9O0"
	testTenantID  = "ten_01JB8Z00000000000000000000"
	testPaymentID = "pay_01JB8Z9K2QW3E4R5T6Y7U8I9O0"
)

func testOutboxMessage() ports.OutboxMessage {
	return ports.OutboxMessage{
		ID:           shared.EventID(testEventID),
		TenantID:     shared.TenantID(testTenantID),
		Topic:        testTopic,
		Type:         "payment.captured.v1",
		AggregateID:  testPaymentID,
		PartitionKey: testPaymentID,
		Payload:      []byte(`{"specversion":"1.0"}`),
		Headers: map[string]string{
			events.HeaderEventID:   testEventID,
			events.HeaderEventType: "payment.captured.v1",
			events.HeaderTenantID:  testTenantID,
		},
		OccurredAt: time.Date(2026, 8, 26, 14, 3, 11, 412_000_000, time.UTC),
	}
}

// TestRecordForUsesThePartitionKey pins the single line that is the entire ordering guarantee.
func TestRecordForUsesThePartitionKey(t *testing.T) {
	t.Parallel()
	msg := testOutboxMessage()
	rec, err := recordFor(msg)
	if err != nil {
		t.Fatalf("recordFor: %v", err)
	}
	if string(rec.Key) != testPaymentID {
		t.Fatalf("key = %q, want the partition key", rec.Key)
	}
	if rec.Topic != testTopic {
		t.Fatalf("topic = %q", rec.Topic)
	}
	if !rec.Timestamp.Equal(msg.OccurredAt) {
		t.Fatalf("timestamp = %v, want the occurrence time so consumer time-lag is honest", rec.Timestamp)
	}
	found := map[string]string{}
	for _, h := range rec.Headers {
		found[h.Key] = string(h.Value)
	}
	if found[events.HeaderEventType] != "payment.captured.v1" || found[events.HeaderTenantID] != testTenantID {
		t.Fatalf("headers were not carried: %v", found)
	}
}

func TestRecordForRefusesAKeylessOrTopiclessMessage(t *testing.T) {
	t.Parallel()
	noKey := testOutboxMessage()
	noKey.PartitionKey = ""
	if _, err := recordFor(noKey); err == nil {
		t.Fatal("recordFor accepted a message with no partition key; ordering would be destroyed silently")
	}

	noTopic := testOutboxMessage()
	noTopic.Topic = ""
	if _, err := recordFor(noTopic); err == nil {
		t.Fatal("recordFor accepted a message with no topic")
	}
}

// TestClassifyProduceError is the table that decides whether the relay retries a row or parks it.
func TestClassifyProduceError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		err       error
		retryable bool
		code      apierror.Code
	}{
		{"context canceled", context.Canceled, true, apierror.CodeServiceUnavailable},
		{"record timeout", kgo.ErrRecordTimeout, false, apierror.CodeGatewayTimeout},
		{"client closed", kgo.ErrClientClosed, true, apierror.CodeServiceUnavailable},
		{"buffer full", kgo.ErrMaxBuffered, true, apierror.CodeServiceUnavailable},
		{"retries exhausted", kgo.ErrRecordRetries, true, apierror.CodeServiceUnavailable},
		{"message too large", kerr.MessageTooLarge, false, apierror.CodeRequestTooLarge},
		{"record list too large", kerr.RecordListTooLarge, false, apierror.CodeRequestTooLarge},
		{"topic acl", kerr.TopicAuthorizationFailed, false, apierror.CodeForbidden},
		{"sasl", kerr.SaslAuthenticationFailed, false, apierror.CodeUnauthenticated},
		{"invalid topic", kerr.InvalidTopicException, false, apierror.CodeConfigurationInvalid},
		{"unknown topic", kerr.UnknownTopicOrPartition, true, apierror.CodeServiceUnavailable},
		{"not enough replicas", kerr.NotEnoughReplicas, true, apierror.CodeServiceUnavailable},
		{"leader not available", kerr.LeaderNotAvailable, true, apierror.CodeServiceUnavailable},
		// Unknown errors are retryable on purpose: the relay parks the row after five attempts,
		// so guessing "retryable" costs five attempts and guessing "permanent" costs a manual
		// intervention on an event that would have gone through.
		{"unclassified", errors.New("something else"), true, apierror.CodeDependencyFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyProduceError(tc.err, &kgo.Record{Topic: testTopic})
			if apierror.IsRetryable(got) != tc.retryable {
				t.Errorf("retryable = %v, want %v (%v)", apierror.IsRetryable(got), tc.retryable, got)
			}
			if apierror.CodeOf(got) != tc.code {
				t.Errorf("code = %s, want %s", apierror.CodeOf(got), tc.code)
			}
		})
	}
}

// TestClassifyProduceErrorNeverLeaksACredential: the SASL failure path must not echo the broker's
// message, which can contain the principal.
func TestClassifyProduceErrorSaslMessageIsGeneric(t *testing.T) {
	t.Parallel()
	err := classifyProduceError(kerr.SaslAuthenticationFailed, nil)
	var pe *apierror.Error
	if !errors.As(err, &pe) {
		t.Fatalf("want a platform error, got %T", err)
	}
	if pe.Message != "kafka: SASL authentication failed" {
		t.Fatalf("message = %q; it must not carry broker detail", pe.Message)
	}
}

func TestClassifyProduceErrorIsNilForSuccess(t *testing.T) {
	t.Parallel()
	if got := classifyProduceError(nil, nil); got != nil {
		t.Fatalf("classifyProduceError(nil) = %v", got)
	}
}

// TestProducerConstantsMatchTheDocumentedTopology guards the numbers that are correctness
// properties rather than tuning.
func TestProducerConstantsMatchTheDocumentedTopology(t *testing.T) {
	t.Parallel()
	if MaxInFlightPerBroker != 5 {
		t.Errorf("max in flight = %d; 5 is the maximum the idempotent producer preserves ordering for",
			MaxInFlightPerBroker)
	}
	if ProducerLinger != 5*time.Millisecond {
		t.Errorf("linger = %v, docs say 5ms", ProducerLinger)
	}
	if ProducerBatchMaxBytes != 256*1024 {
		t.Errorf("batch max bytes = %d, docs say 256 KiB", ProducerBatchMaxBytes)
	}
	if MaxBufferedRecords <= 0 {
		t.Error("the producer buffer must be bounded; an unbounded buffer turns a broker outage into an OOM kill")
	}
	if RecordDeliveryTimeout >= 30*time.Second {
		t.Errorf("delivery timeout %v must stay under the outbox 30s stale-claim window", RecordDeliveryTimeout)
	}
}

func TestPublishWithNoMessagesIsANoOp(t *testing.T) {
	t.Parallel()
	// A nil client would panic if Publish tried to produce, which is the assertion.
	var p Producer
	if err := p.Publish(context.Background()); err != nil {
		t.Fatalf("Publish() with no messages: %v", err)
	}
}
