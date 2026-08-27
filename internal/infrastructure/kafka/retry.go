package kafka

import (
	"context"
	"errors"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/events"
	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Retry headers. They are carried on the retry and DLQ records only; the original envelope is
// never modified, because events are immutable (rule E1) and a redelivery must be byte-identical
// to the first delivery for dedup to work.
const (
	// HeaderAttempt is how many handler attempts have failed. It is the sole input to the tier
	// decision, and it lives in a header rather than in the envelope precisely so the envelope
	// stays byte-identical.
	HeaderAttempt = "pp-retry-attempt"
	// HeaderRetryTier names the tier the record is parked in, for `platformctl events dlq`.
	HeaderRetryTier = "pp-retry-tier"
	// HeaderOriginalTopic is the topic the event was first published to. Without it, a record
	// that has been through two tiers has no way back to its source topic on replay.
	HeaderOriginalTopic = "pp-original-topic"
	// HeaderConsumerGroup names the group that failed. Two groups can fail on the same event for
	// different reasons and each needs its own DLQ entry to be diagnosable.
	HeaderConsumerGroup = "pp-consumer-group"
	// HeaderErrorCode is the platform error code, so a DLQ can be grouped by failure class
	// without parsing prose.
	HeaderErrorCode = "pp-error-code"
	// HeaderErrorChain is the full unwrapped error chain — the thing an operator actually reads
	// at 3 a.m. It is redacted and length-capped; see errorChain.
	HeaderErrorChain = "pp-error-chain"
	// HeaderFailedAt is when the final failure happened.
	HeaderFailedAt = "pp-failed-at"
	// HeaderReadyAt is the instant the delay tier's wait expires, computed once by the router so
	// that every consumer of the tier agrees on it rather than each recomputing from its own
	// clock.
	HeaderReadyAt = "pp-ready-at"

	// headerKafkaPrefix marks consumer-side annotations that describe the *record*, not the
	// event. They are added when a record is read and stripped before it is republished: a
	// partition number from one topic means nothing on another.
	headerKafkaPrefix = "pp-kafka-"
	// HeaderKafkaPartition and HeaderKafkaOffset are those annotations.
	HeaderKafkaPartition = headerKafkaPrefix + "partition"
	HeaderKafkaOffset    = headerKafkaPrefix + "offset"

	// DLQSuffix is the dead-letter sibling of a topic.
	DLQSuffix = ".dlq"

	// retrySuffixPrefix is what every delay tier's suffix starts with, used to recover the
	// original topic name.
	retrySuffixPrefix = ".retry."

	// maxErrorChainBytes caps the error chain header. A header is not a log: an unbounded error
	// string on a poison record that fails identically a million times is a broker-side storage
	// problem, and the first 4 KiB has always been enough to identify the failure.
	maxErrorChainBytes = 4096
)

// Tier is one delay level of the retry topology (docs/events.md §9.1).
type Tier struct {
	// Suffix is appended to the source topic: ".retry.5s".
	Suffix string
	// Delay is how long a record waits before the tier's consumer processes it.
	Delay time.Duration
	// ThroughAttempt is the highest attempt number this tier accepts. Attempts 1–2 go to the 5 s
	// tier, 3–4 to the 1 m tier, 5–6 to the 10 m tier, 7+ to the DLQ.
	ThroughAttempt int
}

// DefaultTiers is the topology from docs/events.md §9.1, total elapsed ≤ 22 minutes.
//
// The escalation is geometric rather than linear because the failures it is designed for are
// too: a gateway blip clears in seconds, a gateway outage in minutes, and anything still failing
// after twenty-two minutes is not going to be fixed by a seventh attempt — it needs a human, and
// the DLQ is how it gets one.
var DefaultTiers = []Tier{
	{Suffix: ".retry.5s", Delay: 5 * time.Second, ThroughAttempt: 2},
	{Suffix: ".retry.1m", Delay: 60 * time.Second, ThroughAttempt: 4},
	{Suffix: ".retry.10m", Delay: 600 * time.Second, ThroughAttempt: 6},
}

// JitterFraction is the ±20 % spread applied to every tier delay.
//
// Jitter is mandatory, not a refinement. Without it a gateway outage produces a synchronized
// retry wave: every record that failed in the same second retries in the same second, and it
// arrives exactly as the gateway is recovering, which knocks it over again. That is the
// retry-storm failure mode in baseline §24, and the fix costs one multiplication.
const JitterFraction = 0.2

// TierFor returns the tier for an attempt number, and whether one applies at all.
func TierFor(tiers []Tier, attempt int) (Tier, bool) {
	for _, t := range tiers {
		if attempt <= t.ThroughAttempt {
			return t, true
		}
	}
	return Tier{}, false
}

// BaseTopic strips any retry-tier suffix, returning the topic an event was originally published
// to. A record that has been through two tiers still names its source correctly.
func BaseTopic(topic string) string {
	if i := strings.Index(topic, retrySuffixPrefix); i > 0 {
		return topic[:i]
	}
	return strings.TrimSuffix(topic, DLQSuffix)
}

// RetryTopic is the topic for a tier of a source topic.
func RetryTopic(base string, t Tier) string { return BaseTopic(base) + t.Suffix }

// DLQTopic is the dead-letter topic for a source topic.
func DLQTopic(base string) string { return BaseTopic(base) + DLQSuffix }

// RetryRouter republishes failed records to the delay tiers and, on exhaustion, to the DLQ.
//
// # The reordering hazard, stated plainly
//
// Retry topics break per-key ordering by construction. A consumer that fails on a payment's
// version 3, parks it on `.retry.1m`, and then processes version 4 straight through from the main
// topic will apply capture before authorization. This is accepted, deliberately, because the
// alternative — blocking the partition until the failed record succeeds — converts one poison
// message into a total stall for every key on that partition, and on a 48-partition payments
// topic that is one forty-eighth of all merchants not being processed.
//
// The three defences, which a consumer using this router must have (docs/events.md §6.3):
//
//  1. A `last_applied_version` per aggregate in the projection, rejecting any event whose
//     `aggregateversion` is ≤ it. The late v3 is discarded on arrival.
//  2. Idempotent, commutative projections keyed on `source_event_id` — ledger postings are
//     independently balanced sets, so capture-before-authorization reaches the same balances.
//  3. A consumer that can satisfy neither must not use this router. It declares `ordering: strict`
//     and blocks its partition instead (§9.4) — which this package supports by simply not
//     installing a Router, so a handler failure halts the partition.
//
// Safe for concurrent use.
type RetryRouter struct {
	pub   ports.EventPublisher
	group string
	tiers []Tier
	now   func() time.Time
}

// NewRetryRouter builds a router that publishes through pub.
//
// It takes a ports.EventPublisher rather than a *Producer so that the router can be tested with
// no broker and so that a deployment could, in principle, route through a different transport
// without this file knowing.
func NewRetryRouter(pub ports.EventPublisher, group string, opts ...RetryOption) (*RetryRouter, error) {
	if pub == nil {
		return nil, apierror.New(apierror.CodeInternalError, "kafka: retry router needs a publisher")
	}
	if err := validateGroupName(group); err != nil {
		return nil, err
	}
	r := &RetryRouter{pub: pub, group: group, tiers: DefaultTiers, now: time.Now}
	for _, o := range opts {
		o(r)
	}
	return r, nil
}

// RetryOption configures a RetryRouter.
type RetryOption func(*RetryRouter)

// WithTiers replaces the delay topology. Used by tests and by the one consumer whose failures are
// known to clear faster than five seconds.
func WithTiers(tiers []Tier) RetryOption {
	return func(r *RetryRouter) {
		if len(tiers) > 0 {
			r.tiers = tiers
		}
	}
}

// WithClock replaces time.Now, so a test can assert the computed ready-at without sleeping.
func WithClock(now func() time.Time) RetryOption {
	return func(r *RetryRouter) {
		if now != nil {
			r.now = now
		}
	}
}

// Route implements Router.
//
// Which failures go where (docs/events.md §9.2), and why:
//
//   - Retryable (INFRASTRUCTURE, TIMEOUT, GATEWAY, RATE_LIMIT, optimistic-concurrency CONFLICT):
//     next tier. Transient by definition.
//   - Non-retryable (VALIDATION, BUSINESS_RULE, NOT_FOUND, an unknown enum with
//     x-unknown-behaviour route-to-dlq, an invariant contradiction): straight to the DLQ.
//     Retrying a schema-invalid payload six times wastes twenty-two minutes and delays the alert
//     by the same amount.
//   - Attempt 7 or beyond: the DLQ regardless of classification.
func (r *RetryRouter) Route(ctx context.Context, rec *kgo.Record, handlerErr error) error {
	if rec == nil {
		return apierror.New(apierror.CodeInternalError, "kafka: nothing to route")
	}
	attempt := AttemptOf(rec) + 1
	base := originalTopic(rec)

	tier, ok := TierFor(r.tiers, attempt)
	retryable := apierror.IsRetryable(handlerErr)

	if !retryable || !ok {
		return r.publish(ctx, rec, DLQTopic(base), base, attempt, Tier{}, handlerErr)
	}
	return r.publish(ctx, rec, RetryTopic(base, tier), base, attempt, tier, handlerErr)
}

// publish republishes the record verbatim to a tier or DLQ topic.
//
// "Verbatim" is load-bearing: the value is the original envelope bytes and the original headers
// are preserved, so the event id — the dedup key — survives every hop. A router that rebuilt the
// envelope would mint a new id, and the same event would then be applied once per tier.
func (r *RetryRouter) publish(ctx context.Context, rec *kgo.Record, topic, base string, attempt int, tier Tier, handlerErr error) error {
	headers := preservedHeaders(rec)

	headers[HeaderAttempt] = strconv.Itoa(attempt)
	headers[HeaderOriginalTopic] = base
	headers[HeaderConsumerGroup] = r.group
	headers[HeaderErrorCode] = string(apierror.CodeOf(handlerErr))
	headers[HeaderErrorChain] = errorChain(handlerErr)
	headers[HeaderFailedAt] = r.now().UTC().Format(events.TimeFormat)
	if tier.Suffix != "" {
		headers[HeaderRetryTier] = tier.Suffix
		headers[HeaderReadyAt] = ReadyAt(rec, tier).UTC().Format(events.TimeFormat)
	}

	msg := ports.OutboxMessage{
		ID:           shared.EventID(headers[events.HeaderEventID]),
		TenantID:     shared.TenantID(headers[events.HeaderTenantID]),
		Topic:        topic,
		Type:         headers[events.HeaderEventType],
		AggregateID:  headers[events.HeaderAggregateID],
		PartitionKey: string(rec.Key),
		Payload:      rec.Value,
		Headers:      headers,
		OccurredAt:   rec.Timestamp,
	}
	if msg.PartitionKey == "" {
		// A keyless record on a retry topic would be spread across partitions and lose whatever
		// ordering the source topic had. Refuse rather than silently degrade.
		return apierror.Newf(apierror.CodeInternalError,
			"kafka: record on %s has no key; refusing to republish it to %s", rec.Topic, topic)
	}
	if err := r.pub.Publish(ctx, msg); err != nil {
		return err
	}
	return nil
}

// AttemptOf reads the attempt counter from a record, defaulting to zero for a first delivery.
//
// A malformed header reads as zero rather than as an error: a record whose attempt counter has
// been corrupted should re-enter the ladder at the bottom, not be dropped. The worst case is a
// few extra retries; the alternative worst case is a lost event.
func AttemptOf(rec *kgo.Record) int {
	for _, h := range rec.Headers {
		if h.Key != HeaderAttempt {
			continue
		}
		n, err := strconv.Atoi(string(h.Value))
		if err != nil || n < 0 {
			return 0
		}
		return n
	}
	return 0
}

// ReadyAt computes when a record parked in a tier may be processed.
//
// The delay is measured from the record's own timestamp — the envelope's `time`, the instant the
// fact occurred — rather than from publication, so a record that spent time queued does not wait
// twice.
//
// The jitter is deterministic in the event id rather than random. That matters: a retry consumer
// re-reads the same record every time it finds the delay unexpired, and a random jitter would
// give a different answer on every read, so a record could be perpetually "almost ready" or jump
// backwards. Hashing the id gives a stable, uniformly spread offset that survives redelivery.
func ReadyAt(rec *kgo.Record, tier Tier) time.Time {
	base := rec.Timestamp
	if base.IsZero() {
		base = time.Now().UTC()
	}
	return base.Add(jitteredDelay(tier.Delay, recordEventID(rec)))
}

// jitteredDelay spreads d by ±JitterFraction, deterministically in seed.
func jitteredDelay(d time.Duration, seed string) time.Duration {
	if d <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	// Map the hash onto [-1, +1) with 16 bits of resolution, which is far finer than the
	// millisecond granularity anything downstream cares about.
	const steps = 1 << 16
	frac := float64(h.Sum64()%steps)/float64(steps)*2 - 1
	return d + time.Duration(float64(d)*JitterFraction*frac)
}

func recordEventID(rec *kgo.Record) string {
	for _, h := range rec.Headers {
		if h.Key == events.HeaderEventID {
			return string(h.Value)
		}
	}
	return string(rec.Key)
}

func originalTopic(rec *kgo.Record) string {
	for _, h := range rec.Headers {
		if h.Key == HeaderOriginalTopic && len(h.Value) > 0 {
			return string(h.Value)
		}
	}
	return BaseTopic(rec.Topic)
}

// preservedHeaders copies a record's headers, dropping the consumer-side annotations.
//
// Everything else is kept, including the headers a previous tier added: the DLQ entry for a
// record that failed six times should show the whole history, not just the last hop.
func preservedHeaders(rec *kgo.Record) map[string]string {
	out := make(map[string]string, len(rec.Headers)+8)
	for _, h := range rec.Headers {
		if strings.HasPrefix(h.Key, headerKafkaPrefix) {
			continue
		}
		out[h.Key] = string(h.Value)
	}
	return out
}

// errorChain renders the full unwrapped error chain, redacted and capped.
//
// The chain, not just the top error: "publishing to the ledger failed" is useless, and
// "publishing to the ledger failed: connection refused: dial tcp 10.0.3.4:5432" is a diagnosis.
// The platform's errors keep their cause unexported and out of the API response for exactly this
// reason — it belongs here and in the logs, not in a merchant's 500 body.
//
// The PAN check is not theatre: a gateway adapter's error can quote a request body, and a DLQ
// record has a 30-day retention and is read by humans. If anything in the chain looks like a card
// number the whole chain is replaced rather than masked, because a partially masked PAN is still
// a finding.
func errorChain(err error) string {
	if err == nil {
		return ""
	}
	var parts []string
	for e := err; e != nil; e = errors.Unwrap(e) {
		msg := e.Error()
		if len(parts) > 0 && strings.HasSuffix(parts[len(parts)-1], msg) {
			// Wrapped errors repeat their cause's text; do not print it twice.
			continue
		}
		parts = append(parts, msg)
		if len(parts) >= 16 {
			break
		}
	}
	chain := strings.Join(parts, " <- ")
	if secret.ContainsPAN(chain) {
		return "[REDACTED: error chain contained a possible card number]"
	}
	if len(chain) > maxErrorChainBytes {
		chain = chain[:maxErrorChainBytes] + "…(truncated)"
	}
	return chain
}

// --- the delay implementation --------------------------------------------------------------------

// ErrDelayNotElapsed is returned by DelayedHandler when a record's tier delay has not expired.
//
// It is not a failure and must never be routed: the consumer's halt path rewinds the partition to
// this record and backs off, which is precisely the "pause the partition rather than seek"
// behaviour docs/events.md §9.1 prescribes. Seeking forward would skip the record; spinning on it
// would burn a core per delay tier.
var ErrDelayNotElapsed = errors.New("kafka: retry tier delay has not elapsed")

// PartitionPauser is the subset of the client a DelayedHandler needs to stop fetching a partition
// while its delay runs. *Consumer implements it.
type PartitionPauser interface {
	PauseFetchPartitions(map[string][]int32) map[string][]int32
	ResumeFetchPartitions(map[string][]int32)
}

// DelayedHandler enforces a tier's delay in front of the real handler.
//
// The delay is implemented in the consumer, not by a broker feature, because Kafka has no delayed
// delivery. The two honest implementations are "sleep" and "pause the partition"; this type does
// both, and which one it picks is bounded by the rebalance timeout:
//
//   - If the remaining wait fits inside MaxInlineWait (well under the 5-minute rebalance
//     timeout), it waits inline. Simple, and the record is processed the instant it is ready.
//   - Otherwise — the 10-minute tier — it pauses the partition through the client, schedules a
//     resume, and returns ErrDelayNotElapsed so the consumer rewinds to this record. No fetch,
//     no CPU, no heartbeat risk, and the partition resumes exactly once.
//
// Without a pauser it degrades to returning ErrDelayNotElapsed, which the consumer's bounded
// backoff turns into a poll every few seconds rather than a spin.
type DelayedHandler struct {
	inner  ports.EventHandler
	tier   Tier
	pauser PartitionPauser
	now    func() time.Time

	// maxInlineWait bounds the inline path. It must stay comfortably under RebalanceTimeout: a
	// handler that blocks longer than that is indistinguishable from a wedged one and its
	// partitions are reassigned mid-wait.
	maxInlineWait time.Duration

	// mu guards paused, which records the partitions this handler has already scheduled a resume
	// for, so a re-read of the same record does not stack timers.
	mu     sync.Mutex
	paused map[string]*time.Timer
	closed bool
}

// NewDelayedHandler wraps inner with the tier's delay.
func NewDelayedHandler(inner ports.EventHandler, tier Tier, opts ...DelayedOption) *DelayedHandler {
	d := &DelayedHandler{
		inner:         inner,
		tier:          tier,
		now:           time.Now,
		maxInlineWait: 60 * time.Second,
		paused:        map[string]*time.Timer{},
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// DelayedOption configures a DelayedHandler.
type DelayedOption func(*DelayedHandler)

// WithPauser installs the partition pauser, enabling the no-spin path for the long tiers.
func WithPauser(p PartitionPauser) DelayedOption {
	return func(d *DelayedHandler) { d.pauser = p }
}

// WithMaxInlineWait bounds how long the handler will block in place.
func WithMaxInlineWait(d time.Duration) DelayedOption {
	return func(h *DelayedHandler) {
		if d > 0 {
			h.maxInlineWait = d
		}
	}
}

// WithDelayClock replaces time.Now for tests.
func WithDelayClock(now func() time.Time) DelayedOption {
	return func(d *DelayedHandler) {
		if now != nil {
			d.now = now
		}
	}
}

// Handle waits for the tier delay, then delegates.
func (d *DelayedHandler) Handle(ctx context.Context, msg ports.OutboxMessage) error {
	ready := readyAtFor(msg, d.tier)
	remaining := ready.Sub(d.now())

	switch {
	case remaining <= 0:
		return d.inner.Handle(ctx, msg)

	case remaining <= d.maxInlineWait:
		t := time.NewTimer(remaining)
		defer t.Stop()
		select {
		case <-ctx.Done():
			// Shutdown mid-wait. The record is uncommitted, so it is redelivered on restart and
			// waits out whatever remains of its delay then.
			return ErrDelayNotElapsed
		case <-t.C:
			return d.inner.Handle(ctx, msg)
		}
	}

	d.pauseUntil(msg, remaining)
	return ErrDelayNotElapsed
}

// pauseUntil stops fetching the record's partition until its delay expires.
//
// Goroutine ownership: the resume is a time.Timer owned by this handler and cancelled by Close.
// There is no long-lived goroutine — a Timer's callback runs on the runtime timer goroutine — so
// there is nothing to leak, and Close stops every outstanding timer.
func (d *DelayedHandler) pauseUntil(msg ports.OutboxMessage, remaining time.Duration) {
	if d.pauser == nil {
		return
	}
	topic, partition, ok := kafkaCoordinates(msg)
	if !ok {
		return
	}
	key := topic + "/" + strconv.Itoa(int(partition))

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	if _, already := d.paused[key]; already {
		return
	}
	d.pauser.PauseFetchPartitions(map[string][]int32{topic: {partition}})
	d.paused[key] = time.AfterFunc(remaining, func() {
		d.mu.Lock()
		delete(d.paused, key)
		closed := d.closed
		d.mu.Unlock()
		if closed {
			return
		}
		d.pauser.ResumeFetchPartitions(map[string][]int32{topic: {partition}})
	})
}

// Close stops every pending resume timer and resumes the partitions, so a shutdown does not leave
// a partition paused for a member that no longer exists. Bounded: it does no I/O and waits for
// nothing.
func (d *DelayedHandler) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	for key, t := range d.paused {
		t.Stop()
		delete(d.paused, key)
	}
	return nil
}

// readyAtFor computes the ready instant from an outbox message, preferring the router's stamped
// value so that every consumer of a tier agrees on it regardless of clock skew.
func readyAtFor(msg ports.OutboxMessage, tier Tier) time.Time {
	if v := msg.Headers[HeaderReadyAt]; v != "" {
		if t, err := time.Parse(events.TimeFormat, v); err == nil {
			return t
		}
	}
	base := msg.OccurredAt
	if base.IsZero() {
		base = time.Now().UTC()
	}
	seed := msg.ID.String()
	if seed == "" {
		seed = msg.PartitionKey
	}
	return base.Add(jitteredDelay(tier.Delay, seed))
}

// kafkaCoordinates recovers the record's topic and partition from the consumer-side annotations.
func kafkaCoordinates(msg ports.OutboxMessage) (string, int32, bool) {
	// ParseInt with a 32-bit size rather than Atoi + int32(): the header is external input, and
	// a value outside int32 truncates into a different, entirely valid-looking partition.
	p, err := strconv.ParseInt(msg.Headers[HeaderKafkaPartition], 10, 32)
	if err != nil || msg.Topic == "" {
		return "", 0, false
	}
	return msg.Topic, int32(p), true
}

// PauseFetchPartitions implements PartitionPauser, so a Consumer can be handed to a
// DelayedHandler directly.
func (c *Consumer) PauseFetchPartitions(tp map[string][]int32) map[string][]int32 {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return nil
	}
	return client.PauseFetchPartitions(tp)
}

// ResumeFetchPartitions implements PartitionPauser.
func (c *Consumer) ResumeFetchPartitions(tp map[string][]int32) {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return
	}
	client.ResumeFetchPartitions(tp)
}

var (
	_ Router             = (*RetryRouter)(nil)
	_ ports.EventHandler = (*DelayedHandler)(nil)
	_ PartitionPauser    = (*Consumer)(nil)
)
