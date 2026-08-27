package kafka

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/events"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// capturingPublisher records what the router published, so the routing decisions can be asserted
// with no broker.
type capturingPublisher struct {
	mu   sync.Mutex
	sent []ports.OutboxMessage
	err  error
}

func (p *capturingPublisher) Publish(_ context.Context, msgs ...ports.OutboxMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.sent = append(p.sent, msgs...)
	return nil
}

func (p *capturingPublisher) Close() error { return nil }

func (p *capturingPublisher) last() ports.OutboxMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sent) == 0 {
		return ports.OutboxMessage{}
	}
	return p.sent[len(p.sent)-1]
}

func testRecord(topic string, headers map[string]string) *kgo.Record {
	base := map[string]string{
		events.HeaderEventID:   testEventID,
		events.HeaderEventType: "payment.captured.v1",
		events.HeaderTenantID:  testTenantID,
		HeaderKafkaPartition:   "7",
		HeaderKafkaOffset:      "1234",
	}
	for k, v := range headers {
		base[k] = v
	}
	hs := make([]kgo.RecordHeader, 0, len(base))
	for k, v := range base {
		hs = append(hs, kgo.RecordHeader{Key: k, Value: []byte(v)})
	}
	return &kgo.Record{
		Topic:     topic,
		Partition: 7,
		Offset:    1234,
		Key:       []byte(testPaymentID),
		Value:     []byte(`{"specversion":"1.0"}`),
		Headers:   hs,
		Timestamp: time.Date(2026, 8, 26, 14, 3, 11, 412_000_000, time.UTC),
	}
}

func TestTopicNaming(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, base, dlq string }{
		{testTopic, testTopic, testTopic + ".dlq"},
		{testTopic + ".retry.5s", testTopic, testTopic + ".dlq"},
		{testTopic + ".retry.10m", testTopic, testTopic + ".dlq"},
		{testTopic + ".dlq", testTopic, testTopic + ".dlq"},
	}
	for _, tc := range cases {
		if got := BaseTopic(tc.in); got != tc.base {
			t.Errorf("BaseTopic(%q) = %q, want %q", tc.in, got, tc.base)
		}
		if got := DLQTopic(tc.in); got != tc.dlq {
			t.Errorf("DLQTopic(%q) = %q, want %q", tc.in, got, tc.dlq)
		}
	}
	if got := RetryTopic(testTopic+".retry.5s", DefaultTiers[1]); got != testTopic+".retry.1m" {
		t.Errorf("promoting a tier produced %q", got)
	}
}

func TestTierForMatchesTheDocumentedLadder(t *testing.T) {
	t.Parallel()
	want := map[int]string{
		1: ".retry.5s", 2: ".retry.5s",
		3: ".retry.1m", 4: ".retry.1m",
		5: ".retry.10m", 6: ".retry.10m",
	}
	for attempt, suffix := range want {
		tier, ok := TierFor(DefaultTiers, attempt)
		if !ok || tier.Suffix != suffix {
			t.Errorf("attempt %d -> %q (ok=%v), want %q", attempt, tier.Suffix, ok, suffix)
		}
	}
	for _, attempt := range []int{7, 8, 99} {
		if _, ok := TierFor(DefaultTiers, attempt); ok {
			t.Errorf("attempt %d still has a tier; 7+ is the DLQ", attempt)
		}
	}
	// The delays themselves, from the §9.1 table.
	wantDelays := []time.Duration{5 * time.Second, 60 * time.Second, 600 * time.Second}
	for i, tier := range DefaultTiers {
		if tier.Delay != wantDelays[i] {
			t.Errorf("tier %d delay = %v, docs say %v", i+1, tier.Delay, wantDelays[i])
		}
	}
	// And the whole ladder parks a record within the documented ~22 minutes rather than hours.
	var total time.Duration
	for _, tier := range DefaultTiers {
		total += tier.Delay * 2
	}
	if total > 23*time.Minute {
		t.Errorf("the ladder totals %v; docs say a record reaches the DLQ in about 22 minutes", total)
	}
}

func TestRouteEscalatesThroughTheTiers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		attemptSoFar string
		err          error
		wantTopic    string
		wantAttempt  string
	}{
		{"first retryable failure", "", apierror.New(apierror.CodeServiceUnavailable, "down"),
			testTopic + ".retry.5s", "1"},
		{"second", "1", apierror.New(apierror.CodeServiceUnavailable, "down"),
			testTopic + ".retry.5s", "2"},
		{"third promotes", "2", apierror.New(apierror.CodeServiceUnavailable, "down"),
			testTopic + ".retry.1m", "3"},
		{"fifth promotes again", "4", apierror.New(apierror.CodeServiceUnavailable, "down"),
			testTopic + ".retry.10m", "5"},
		{"seventh exhausts", "6", apierror.New(apierror.CodeServiceUnavailable, "down"),
			testTopic + ".dlq", "7"},
		{"non-retryable goes straight to the dlq", "", apierror.New(apierror.CodeValidationFailed, "bad"),
			testTopic + ".dlq", "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pub := &capturingPublisher{}
			r, err := NewRetryRouter(pub, "pp.ledger.projection.v1")
			if err != nil {
				t.Fatalf("NewRetryRouter: %v", err)
			}
			headers := map[string]string{}
			topic := testTopic
			if tc.attemptSoFar != "" {
				headers[HeaderAttempt] = tc.attemptSoFar
				headers[HeaderOriginalTopic] = testTopic
				topic = testTopic + ".retry.5s"
			}
			if err := r.Route(context.Background(), testRecord(topic, headers), tc.err); err != nil {
				t.Fatalf("Route: %v", err)
			}
			got := pub.last()
			if got.Topic != tc.wantTopic {
				t.Fatalf("routed to %q, want %q", got.Topic, tc.wantTopic)
			}
			if got.Headers[HeaderAttempt] != tc.wantAttempt {
				t.Fatalf("attempt header = %q, want %q", got.Headers[HeaderAttempt], tc.wantAttempt)
			}
			if got.Headers[HeaderOriginalTopic] != testTopic {
				t.Fatalf("original topic = %q", got.Headers[HeaderOriginalTopic])
			}
		})
	}
}

// TestRouteRepublishesVerbatim: the envelope, and above all the event id, must survive every hop
// or dedup stops working and the event is applied once per tier.
func TestRouteRepublishesVerbatim(t *testing.T) {
	t.Parallel()
	pub := &capturingPublisher{}
	r, _ := NewRetryRouter(pub, "pp.ledger.projection.v1")
	rec := testRecord(testTopic, nil)

	if err := r.Route(context.Background(), rec, apierror.New(apierror.CodeServiceUnavailable, "down")); err != nil {
		t.Fatalf("Route: %v", err)
	}
	got := pub.last()
	if string(got.Payload) != string(rec.Value) {
		t.Fatalf("payload was rewritten: %s", got.Payload)
	}
	if got.ID.String() != testEventID {
		t.Fatalf("event id = %q, want the original %q", got.ID, testEventID)
	}
	if got.Headers[events.HeaderEventID] != testEventID {
		t.Fatalf("event id header was lost: %v", got.Headers)
	}
	if got.PartitionKey != testPaymentID {
		t.Fatalf("partition key = %q; a retry must stay in the same ordering domain", got.PartitionKey)
	}
	if !got.OccurredAt.Equal(rec.Timestamp) {
		t.Fatalf("the occurrence time was rewritten to %v", got.OccurredAt)
	}
}

// TestRouteStripsConsumerSideAnnotations: a partition number from one topic means nothing on
// another.
func TestRouteStripsConsumerSideAnnotations(t *testing.T) {
	t.Parallel()
	pub := &capturingPublisher{}
	r, _ := NewRetryRouter(pub, "pp.ledger.projection.v1")
	if err := r.Route(context.Background(), testRecord(testTopic, nil),
		apierror.New(apierror.CodeServiceUnavailable, "down")); err != nil {
		t.Fatalf("Route: %v", err)
	}
	for k := range pub.last().Headers {
		if strings.HasPrefix(k, headerKafkaPrefix) {
			t.Fatalf("consumer-side annotation %q was republished", k)
		}
	}
}

func TestDLQCarriesTheFullErrorChain(t *testing.T) {
	t.Parallel()
	pub := &capturingPublisher{}
	r, _ := NewRetryRouter(pub, "pp.ledger.projection.v1")

	root := errors.New("dial tcp 10.0.3.4:5432: connection refused")
	wrapped := apierror.Wrap(root, apierror.CodeValidationFailed, "ledger projection rejected the event")

	if err := r.Route(context.Background(), testRecord(testTopic, nil), wrapped); err != nil {
		t.Fatalf("Route: %v", err)
	}
	got := pub.last()
	if got.Topic != testTopic+".dlq" {
		t.Fatalf("topic = %q", got.Topic)
	}
	chain := got.Headers[HeaderErrorChain]
	if !strings.Contains(chain, "connection refused") || !strings.Contains(chain, "ledger projection rejected") {
		t.Fatalf("error chain lost a link: %q", chain)
	}
	if got.Headers[HeaderErrorCode] != string(apierror.CodeValidationFailed) {
		t.Fatalf("error code header = %q", got.Headers[HeaderErrorCode])
	}
	if got.Headers[HeaderConsumerGroup] != "pp.ledger.projection.v1" {
		t.Fatalf("consumer group header = %q", got.Headers[HeaderConsumerGroup])
	}
	if got.Headers[HeaderFailedAt] == "" {
		t.Fatal("no failure timestamp")
	}
}

// TestErrorChainRedactsAPossiblePAN. A DLQ record has a 30-day retention and is read by humans.
func TestErrorChainRedactsAPossiblePAN(t *testing.T) {
	t.Parallel()
	chain := errorChain(errors.New("gateway rejected card 4242424242424242"))
	if strings.Contains(chain, "4242424242424242") {
		t.Fatalf("a card number reached the DLQ header: %q", chain)
	}
	if !strings.Contains(chain, "REDACTED") {
		t.Fatalf("redaction was silent: %q", chain)
	}
}

func TestErrorChainIsCapped(t *testing.T) {
	t.Parallel()
	chain := errorChain(errors.New(strings.Repeat("x", maxErrorChainBytes*2)))
	if len(chain) > maxErrorChainBytes+32 {
		t.Fatalf("error chain is %d bytes, cap is %d", len(chain), maxErrorChainBytes)
	}
}

func TestAttemptOfDefaultsToZeroOnAMalformedHeader(t *testing.T) {
	t.Parallel()
	if got := AttemptOf(testRecord(testTopic, nil)); got != 0 {
		t.Fatalf("AttemptOf on a first delivery = %d", got)
	}
	if got := AttemptOf(testRecord(testTopic, map[string]string{HeaderAttempt: "not-a-number"})); got != 0 {
		t.Fatalf("AttemptOf on a corrupt header = %d; a corrupt counter must re-enter the ladder, not drop the event", got)
	}
	if got := AttemptOf(testRecord(testTopic, map[string]string{HeaderAttempt: "3"})); got != 3 {
		t.Fatalf("AttemptOf = %d", got)
	}
}

// TestJitterIsDeterministicAndBounded. Determinism matters because a retry consumer re-reads the
// same record until its delay expires, and a moving target never expires.
func TestJitterIsDeterministicAndBounded(t *testing.T) {
	t.Parallel()
	const d = 60 * time.Second
	seen := map[time.Duration]int{}
	for i := 0; i < 500; i++ {
		seed := "evt_" + strconv.Itoa(i)
		got := jitteredDelay(d, seed)
		if again := jitteredDelay(d, seed); again != got {
			t.Fatalf("jitter for %s is not deterministic: %v then %v", seed, got, again)
		}
		lo := time.Duration(float64(d) * (1 - JitterFraction))
		hi := time.Duration(float64(d) * (1 + JitterFraction))
		if got < lo || got > hi {
			t.Fatalf("jittered delay %v is outside ±20%% of %v", got, d)
		}
		seen[got]++
	}
	if len(seen) < 100 {
		t.Fatalf("jitter produced only %d distinct delays across 500 seeds; a synchronized retry wave is exactly what this prevents", len(seen))
	}
	if jitteredDelay(0, "x") != 0 {
		t.Fatal("a zero delay must stay zero")
	}
}

func TestRouteRefusesAKeylessRecord(t *testing.T) {
	t.Parallel()
	pub := &capturingPublisher{}
	r, _ := NewRetryRouter(pub, "pp.ledger.projection.v1")
	rec := testRecord(testTopic, nil)
	rec.Key = nil
	if err := r.Route(context.Background(), rec, apierror.New(apierror.CodeServiceUnavailable, "x")); err == nil {
		t.Fatal("Route republished a keyless record")
	}
}

func TestRoutePropagatesAPublishFailure(t *testing.T) {
	t.Parallel()
	// If the router cannot publish, the consumer must halt the partition rather than commit past
	// a record that has gone nowhere.
	pub := &capturingPublisher{err: apierror.New(apierror.CodeServiceUnavailable, "broker down")}
	r, _ := NewRetryRouter(pub, "pp.ledger.projection.v1")
	if err := r.Route(context.Background(), testRecord(testTopic, nil),
		apierror.New(apierror.CodeServiceUnavailable, "x")); err == nil {
		t.Fatal("Route reported success while the publish failed")
	}
}

func TestNewRetryRouterValidatesItsInputs(t *testing.T) {
	t.Parallel()
	if _, err := NewRetryRouter(nil, "pp.ledger.projection.v1"); err == nil {
		t.Error("NewRetryRouter accepted a nil publisher")
	}
	if _, err := NewRetryRouter(&capturingPublisher{}, "ledger"); err == nil {
		t.Error("NewRetryRouter accepted a group name with no v<n> suffix")
	}
}

// --- the delay implementation --------------------------------------------------------------------

type recordingHandler struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (h *recordingHandler) Handle(context.Context, ports.OutboxMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	return h.err
}

func (h *recordingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

type fakePauser struct {
	mu      sync.Mutex
	paused  []string
	resumed []string
}

func (p *fakePauser) PauseFetchPartitions(tp map[string][]int32) map[string][]int32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	for topic, parts := range tp {
		for _, part := range parts {
			p.paused = append(p.paused, topic+"/"+strconv.Itoa(int(part)))
		}
	}
	return tp
}

func (p *fakePauser) ResumeFetchPartitions(tp map[string][]int32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for topic, parts := range tp {
		for _, part := range parts {
			p.resumed = append(p.resumed, topic+"/"+strconv.Itoa(int(part)))
		}
	}
}

func (p *fakePauser) pausedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.paused)
}

func delayedMessage(readyAt time.Time) ports.OutboxMessage {
	return ports.OutboxMessage{
		ID:           testEventID,
		Topic:        testTopic + ".retry.10m",
		PartitionKey: testPaymentID,
		OccurredAt:   time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC),
		Headers: map[string]string{
			HeaderReadyAt:        readyAt.UTC().Format(events.TimeFormat),
			HeaderKafkaPartition: "7",
		},
	}
}

func TestDelayedHandlerRunsWhenTheDelayHasElapsed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC)
	inner := &recordingHandler{}
	d := NewDelayedHandler(inner, DefaultTiers[2], WithDelayClock(func() time.Time { return now }))

	msg := delayedMessage(now.Add(-time.Second))
	if err := d.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if inner.count() != 1 {
		t.Fatalf("inner ran %d times", inner.count())
	}
}

func TestDelayedHandlerPausesRatherThanSpinningOnALongWait(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC)
	inner := &recordingHandler{}
	pauser := &fakePauser{}
	d := NewDelayedHandler(inner, DefaultTiers[2],
		WithDelayClock(func() time.Time { return now }),
		WithPauser(pauser),
		WithMaxInlineWait(time.Second))
	defer func() { _ = d.Close() }()

	msg := delayedMessage(now.Add(9 * time.Minute))
	err := d.Handle(context.Background(), msg)
	if !errors.Is(err, ErrDelayNotElapsed) {
		t.Fatalf("Handle = %v, want ErrDelayNotElapsed", err)
	}
	if inner.count() != 0 {
		t.Fatal("the inner handler ran before the delay elapsed")
	}
	if pauser.pausedCount() != 1 {
		t.Fatalf("the partition was paused %d times, want 1", pauser.pausedCount())
	}

	// A re-read of the same record must not stack a second timer.
	_ = d.Handle(context.Background(), msg)
	if pauser.pausedCount() != 1 {
		t.Fatalf("a re-read stacked pauses: %d", pauser.pausedCount())
	}
}

func TestDelayedHandlerWaitsInlineForAShortDelay(t *testing.T) {
	t.Parallel()
	inner := &recordingHandler{}
	d := NewDelayedHandler(inner, DefaultTiers[0], WithMaxInlineWait(time.Second))

	msg := delayedMessage(time.Now().Add(20 * time.Millisecond))
	start := time.Now()
	if err := d.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Fatalf("Handle returned after %v; it did not wait", elapsed)
	}
	if inner.count() != 1 {
		t.Fatalf("inner ran %d times", inner.count())
	}
}

func TestDelayedHandlerReturnsOnContextCancellation(t *testing.T) {
	t.Parallel()
	inner := &recordingHandler{}
	d := NewDelayedHandler(inner, DefaultTiers[0], WithMaxInlineWait(time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := d.Handle(ctx, delayedMessage(time.Now().Add(30*time.Second)))
	if !errors.Is(err, ErrDelayNotElapsed) {
		t.Fatalf("Handle = %v, want ErrDelayNotElapsed on shutdown", err)
	}
	if inner.count() != 0 {
		t.Fatal("the handler ran after the context was canceled")
	}
}

func TestDelayedHandlerCloseIsIdempotentAndStopsTimers(t *testing.T) {
	t.Parallel()
	now := time.Now()
	pauser := &fakePauser{}
	d := NewDelayedHandler(&recordingHandler{}, DefaultTiers[2],
		WithPauser(pauser), WithMaxInlineWait(time.Millisecond))
	_ = d.Handle(context.Background(), delayedMessage(now.Add(10*time.Minute)))

	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	d.mu.Lock()
	remaining := len(d.paused)
	d.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d resume timers survived Close", remaining)
	}
}

func TestReadyAtPrefersTheStampedHeader(t *testing.T) {
	t.Parallel()
	// Every consumer of a tier must agree on the ready instant regardless of its own clock.
	want := time.Date(2026, 8, 26, 14, 10, 0, 0, time.UTC)
	got := readyAtFor(delayedMessage(want), DefaultTiers[2])
	if !got.Equal(want) {
		t.Fatalf("readyAtFor = %v, want the stamped %v", got, want)
	}
}

func TestReadyAtFallsBackToTheRecordTimestamp(t *testing.T) {
	t.Parallel()
	rec := testRecord(testTopic, nil)
	got := ReadyAt(rec, DefaultTiers[0])
	lo := rec.Timestamp.Add(4 * time.Second)
	hi := rec.Timestamp.Add(6 * time.Second)
	if got.Before(lo) || got.After(hi) {
		t.Fatalf("ReadyAt = %v, want within ±20%% of 5s after %v", got, rec.Timestamp)
	}
}
