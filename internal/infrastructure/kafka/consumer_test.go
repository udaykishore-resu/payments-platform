package kafka

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

func TestValidateGroupName(t *testing.T) {
	t.Parallel()
	valid := []string{
		"pp.ledger.projection.v1",
		"pp.audit.sink.v1",
		"pp.config-cache.invalidation.v1",
		"pp.webhook-processor.v1",
		"pp.routing-feedback.v2",
	}
	for _, g := range valid {
		if err := validateGroupName(g); err != nil {
			t.Errorf("%s was rejected: %v", g, err)
		}
	}
	invalid := []string{
		"",
		"ledger",
		// The v<n> suffix is the replay lever; without it a replay has to invent a new identity.
		"pp.ledger.projection",
		"ledger.projection.v1",
		"pp.Ledger.projection.v1",
		"pp.ledger.projection.v0",
	}
	for _, g := range invalid {
		if err := validateGroupName(g); err == nil {
			t.Errorf("%q was accepted", g)
		}
	}
}

func TestSubscribeValidatesItsArguments(t *testing.T) {
	t.Parallel()
	c, err := NewConsumer(localConfig())
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	ctx := context.Background()
	h := &recordingHandler{}

	if err := c.Subscribe(ctx, nil, "pp.ledger.projection.v1", h); err == nil {
		t.Error("Subscribe accepted no topics")
	}
	if err := c.Subscribe(ctx, []string{testTopic}, "ledger", h); err == nil {
		t.Error("Subscribe accepted a malformed group name")
	}
	if err := c.Subscribe(ctx, []string{testTopic}, "pp.ledger.projection.v1", nil); err == nil {
		t.Error("Subscribe accepted a nil handler")
	}
}

func TestNewConsumerRejectsAnInvalidConfig(t *testing.T) {
	t.Parallel()
	c := prodConfig()
	c.Brokers = nil
	if _, err := NewConsumer(c); err == nil {
		t.Fatal("NewConsumer accepted an invalid config")
	}
}

// TestCloseWithoutSubscribeIsANoOp: a process that fails before subscribing still runs its
// deferred Close, and that must not block or panic.
func TestCloseWithoutSubscribeIsANoOp(t *testing.T) {
	t.Parallel()
	c, err := NewConsumer(localConfig())
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on a consumer that never subscribed")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestOutboxMessageForCarriesHeadersAndCoordinates(t *testing.T) {
	t.Parallel()
	rec := testRecord(testTopic, nil)
	msg := outboxMessageFor(rec)

	if msg.ID.String() != testEventID {
		t.Errorf("id = %q", msg.ID)
	}
	if msg.TenantID.String() != testTenantID {
		t.Errorf("tenant = %q", msg.TenantID)
	}
	if msg.Type != "payment.captured.v1" {
		t.Errorf("type = %q", msg.Type)
	}
	if msg.PartitionKey != testPaymentID {
		t.Errorf("partition key = %q", msg.PartitionKey)
	}
	if string(msg.Payload) != string(rec.Value) {
		t.Errorf("payload was rewritten")
	}
	if msg.Headers[HeaderKafkaPartition] != "7" || msg.Headers[HeaderKafkaOffset] != "1234" {
		t.Errorf("consumer-side coordinates missing: %v", msg.Headers)
	}
}

func TestClassifyFetchError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		err       error
		retryable bool
		code      apierror.Code
	}{
		{"shutdown", context.Canceled, true, apierror.CodeServiceUnavailable},
		{"client closed", kgo.ErrClientClosed, true, apierror.CodeServiceUnavailable},
		{"topic acl", kerr.TopicAuthorizationFailed, false, apierror.CodeForbidden},
		{"group acl", kerr.GroupAuthorizationFailed, false, apierror.CodeForbidden},
		{"sasl", kerr.SaslAuthenticationFailed, false, apierror.CodeUnauthenticated},
		{"leader moved", kerr.NotLeaderForPartition, true, apierror.CodeServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyFetchError(tc.err, testTopic, 3)
			if apierror.IsRetryable(got) != tc.retryable {
				t.Errorf("retryable = %v, want %v (%v)", apierror.IsRetryable(got), tc.retryable, got)
			}
			if apierror.CodeOf(got) != tc.code {
				t.Errorf("code = %s, want %s", apierror.CodeOf(got), tc.code)
			}
		})
	}
	if classifyFetchError(nil, testTopic, 0) != nil {
		t.Error("classifyFetchError(nil) is not nil")
	}
}

// TestHandleRecordHaltsWithoutARouter. Safe by default: a consumer whose operator has not decided
// what happens to failures must not skip them.
func TestHandleRecordHaltsWithoutARouter(t *testing.T) {
	t.Parallel()
	c, _ := NewConsumer(localConfig())
	h := &recordingHandler{err: apierror.New(apierror.CodeServiceUnavailable, "down")}

	err := c.handleRecord(context.Background(), h, testRecord(testTopic, nil))
	if err == nil {
		t.Fatal("a handler failure with no router must halt the partition")
	}
	if !strings.Contains(err.Error(), "halting the partition") {
		t.Fatalf("error does not explain the halt: %v", err)
	}
}

// TestHandleRecordRoutesAFailure and commits the record only because the router disposed of it.
func TestHandleRecordRoutesAFailure(t *testing.T) {
	t.Parallel()
	pub := &capturingPublisher{}
	router, err := NewRetryRouter(pub, "pp.ledger.projection.v1")
	if err != nil {
		t.Fatalf("NewRetryRouter: %v", err)
	}
	c, _ := NewConsumer(localConfig(), WithRouter(router))
	h := &recordingHandler{err: apierror.New(apierror.CodeServiceUnavailable, "down")}

	if err := c.handleRecord(context.Background(), h, testRecord(testTopic, nil)); err != nil {
		t.Fatalf("a routed failure must not halt the partition: %v", err)
	}
	if got := pub.last().Topic; got != testTopic+".retry.5s" {
		t.Fatalf("routed to %q", got)
	}
}

// TestHandleRecordHaltsWhenRoutingFails: better to stall one ordering domain than to commit past
// a record that has gone nowhere.
func TestHandleRecordHaltsWhenRoutingFails(t *testing.T) {
	t.Parallel()
	pub := &capturingPublisher{err: apierror.New(apierror.CodeServiceUnavailable, "broker down")}
	router, _ := NewRetryRouter(pub, "pp.ledger.projection.v1")
	c, _ := NewConsumer(localConfig(), WithRouter(router))
	h := &recordingHandler{err: apierror.New(apierror.CodeServiceUnavailable, "down")}

	err := c.handleRecord(context.Background(), h, testRecord(testTopic, nil))
	if err == nil {
		t.Fatal("routing failed but the record was treated as handled")
	}
	if !strings.Contains(err.Error(), "rather than skipping it") {
		t.Fatalf("error does not explain the halt: %v", err)
	}
}

// TestHandleRecordNeverRoutesAnUnelapsedDelay: routing it would advance the record to the next
// tier without ever having tried it.
func TestHandleRecordNeverRoutesAnUnelapsedDelay(t *testing.T) {
	t.Parallel()
	pub := &capturingPublisher{}
	router, _ := NewRetryRouter(pub, "pp.ledger.projection.v1")
	c, _ := NewConsumer(localConfig(), WithRouter(router))

	h := &recordingHandler{err: ErrDelayNotElapsed}
	err := c.handleRecord(context.Background(), h, testRecord(testTopic, nil))
	if !errors.Is(err, ErrDelayNotElapsed) {
		t.Fatalf("handleRecord = %v, want ErrDelayNotElapsed", err)
	}
	if len(pub.sent) != 0 {
		t.Fatal("an unelapsed delay was routed to the next tier")
	}
}

func TestReportLagComputesBothDimensions(t *testing.T) {
	t.Parallel()
	lag := &capturingLag{}
	c, _ := NewConsumer(localConfig(), WithLagReporter(lag))

	rec := &kgo.Record{Offset: 100, Timestamp: time.Now().Add(-30 * time.Second)}
	c.reportLag("pp.ledger.projection.v1", testTopic, 3, 250, rec)

	if lag.records != 149 {
		t.Errorf("record lag = %d, want highWatermark-(offset+1) = 149", lag.records)
	}
	if lag.age < 25*time.Second || lag.age > 60*time.Second {
		t.Errorf("time lag = %v, want about 30s", lag.age)
	}

	// A negative computed lag (the watermark moved backwards after a truncation) clamps to zero
	// rather than exporting a nonsense gauge that breaks every alert expression.
	c.reportLag("pp.ledger.projection.v1", testTopic, 3, 10, rec)
	if lag.records != 0 {
		t.Errorf("record lag = %d, want 0", lag.records)
	}
}

func TestNextBackoffGrowsAndResets(t *testing.T) {
	t.Parallel()
	if got := nextBackoff(0, 0); got != 0 {
		t.Fatalf("a poll with no halts must reset the backoff, got %v", got)
	}
	if got := nextBackoff(time.Second, 0); got != 0 {
		t.Fatalf("progress must reset the backoff, got %v", got)
	}
	first := nextBackoff(0, 1)
	if first != haltBackoffMin {
		t.Fatalf("first backoff = %v, want %v", first, haltBackoffMin)
	}
	second := nextBackoff(first, 1)
	if second != 2*first {
		t.Fatalf("backoff did not grow: %v -> %v", first, second)
	}
	if got := nextBackoff(haltBackoffMax, 1); got != haltBackoffMax {
		t.Fatalf("backoff exceeded its cap: %v", got)
	}
}

func TestConsumerConstantsMatchTheDocumentedSettings(t *testing.T) {
	t.Parallel()
	if SessionTimeout != 45*time.Second {
		t.Errorf("session timeout = %v, docs say 45s", SessionTimeout)
	}
	if HeartbeatInterval != 3*time.Second {
		t.Errorf("heartbeat = %v, docs say 3s", HeartbeatInterval)
	}
	if RebalanceTimeout != 5*time.Minute {
		t.Errorf("max poll interval = %v, docs say 300s", RebalanceTimeout)
	}
	if MaxPollRecords != 100 {
		t.Errorf("max poll records = %d, docs say 100", MaxPollRecords)
	}
	if haltBackoffMax >= RebalanceTimeout {
		t.Error("the halt backoff must stay well under the rebalance timeout")
	}
}

func TestConsumerStringIsSelfDescribing(t *testing.T) {
	t.Parallel()
	c, _ := NewConsumer(localConfig(), WithConcurrency(4), WithMaxPollRecords(50))
	s := c.String()
	if !strings.Contains(s, "concurrency=4") || !strings.Contains(s, "maxPoll=50") {
		t.Fatalf("String() = %q", s)
	}
}

// --- helpers ---------------------------------------------------------------------------------

func localConfig() Config {
	c := DefaultConfig()
	c.Brokers = []string{"localhost:9092"}
	c.Protocol = ProtocolPlaintext
	c.Environment = "local"
	return c
}

type capturingLag struct {
	records int64
	age     time.Duration
}

func (l *capturingLag) ReportLag(_, _ string, _ int32, records int64, age time.Duration) {
	l.records = records
	l.age = age
}
