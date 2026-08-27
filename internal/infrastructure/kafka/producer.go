package kafka

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Producer settings from docs/events.md §5.3. Constants rather than configuration because each
// of them is a correctness property that a deployment must not be able to weaken by editing a
// ConfigMap.
const (
	// ProducerLinger coalesces a relay claim batch into few produce requests. 5 ms is inside the
	// relay, not inside the API request path, so it costs the caller nothing.
	ProducerLinger = 5 * time.Millisecond

	// ProducerBatchMaxBytes matches the relay's ~1 MiB claim batch (500 envelopes × ~2 KiB).
	ProducerBatchMaxBytes = 256 * 1024

	// MaxInFlightPerBroker is 5.
	//
	// This is the number that people get wrong, so: with `enable.idempotence=true` the broker
	// tracks a producer id and a per-partition sequence number, and it will reject an
	// out-of-sequence batch rather than writing it. That is what allows more than one in-flight
	// request per connection *without* losing ordering on a retry — the broker refuses the
	// reordered write and the client re-sends in sequence. Five is the maximum the idempotent
	// producer guarantees this for; above five the broker's sequence window is too small and
	// ordering is no longer guaranteed.
	//
	// Without idempotence the only ordering-safe value is 1, and relay throughput would collapse
	// to one round trip per batch — roughly a 5× reduction on a link with any latency at all.
	// That is the whole trade: idempotence buys us five in flight and keeps ordering.
	MaxInFlightPerBroker = 5

	// MaxBufferedRecords bounds the client's in-memory queue.
	//
	// Bounded is the point. An unbounded producer buffer converts a Kafka outage into an OOM kill
	// of the relay, which is strictly worse than backpressure: the relay's rows are already
	// durable in Postgres, so blocking is free and dying is not. At 10 000 records × ~2 KiB this
	// caps the buffer near 20 MiB.
	MaxBufferedRecords = 10_000

	// MaxBufferedBytes is the second half of the same bound, for the case where a few large
	// webhook envelopes fill the buffer long before the record count does.
	MaxBufferedBytes = 64 * 1024 * 1024

	// ProduceRequestTimeout is how long a broker may take to acknowledge.
	ProduceRequestTimeout = 10 * time.Second

	// RecordDeliveryTimeout bounds the total time a record may spend being retried inside the
	// client. It is under the outbox's 30-second stale-claim window so that a record either
	// succeeds or fails before another relay worker re-claims its row.
	RecordDeliveryTimeout = 25 * time.Second
)

// Producer publishes outbox messages to Kafka. It implements ports.EventPublisher.
//
// Durability posture, and what each setting buys:
//
//   - acks=all with min.insync.replicas=2 (3 for audit): a write is on two AZs before it is
//     acknowledged. acks=1 would lose records on a leader failover, and for payment.captured.v1
//     that means a ledger that permanently disagrees with the payments table. The 2–5 ms cost is
//     paid inside the relay, never inside the request path.
//   - Idempotent producer: franz-go enables it by default and this code never disables it. It
//     removes producer-side duplicates from internal retries and, more importantly, preserves
//     ordering across those retries (see MaxInFlightPerBroker).
//   - zstd: payment envelopes are repetitive JSON. ~4× on our fixtures at lower CPU than gzip for
//     the same ratio. Snappy is listed as the fallback preference for brokers too old to
//     negotiate zstd, because failing to produce is worse than producing larger records.
//   - StickyKeyPartitioner: the standard murmur2 key hash, so our partition assignment matches
//     every other Kafka client's. A custom partitioner here would mean a Java consumer computing
//     a different partition for the same key, which breaks ordering in a way that is invisible
//     until someone adds a second client.
//
// Safe for concurrent use; franz-go's client is.
type Producer struct {
	client *kgo.Client
	// closeOnce makes Close idempotent, because a graceful shutdown path and a defer in main
	// both plausibly call it and a double Close of a franz-go client panics.
	closeOnce sync.Once
	// closeTimeout bounds Close. An unbounded flush on shutdown turns a rolling deploy into a
	// stuck pod that the orchestrator eventually SIGKILLs mid-produce.
	closeTimeout time.Duration

	onDataLoss func(topic string, partition int32)
}

// ProducerOption configures a Producer.
type ProducerOption func(*producerOptions)

type producerOptions struct {
	closeTimeout time.Duration
	onDataLoss   func(string, int32)
	extra        []kgo.Opt
}

// WithCloseTimeout bounds how long Close waits for buffered records to flush.
func WithCloseTimeout(d time.Duration) ProducerOption {
	return func(o *producerOptions) {
		if d > 0 {
			o.closeTimeout = d
		}
	}
}

// WithDataLossHandler installs a callback for the broker telling us records were lost.
//
// This fires when the broker reports a sequence gap — which, with an idempotent producer, means
// records this client believed were written are not. It is not a warning, it is an incident: the
// outbox rows for those events are marked published and never will be. The default handler is
// nil and the wiring is expected to install one that pages.
func WithDataLossHandler(fn func(topic string, partition int32)) ProducerOption {
	return func(o *producerOptions) { o.onDataLoss = fn }
}

// WithProducerOptions appends raw franz-go options, for the integration tests and for the one
// deployment that needs a broker-specific workaround. Anything set here overrides the defaults
// above, which is exactly why it is not the normal path.
func WithProducerOptions(opts ...kgo.Opt) ProducerOption {
	return func(o *producerOptions) { o.extra = append(o.extra, opts...) }
}

// NewProducer connects a producer.
//
// It does not block on reaching a broker: franz-go connects lazily, and a producer that refused
// to construct because a broker was momentarily unreachable would turn a transient network blip
// into a crash loop at exactly the moment the cluster is recovering.
func NewProducer(cfg Config, opts ...ProducerOption) (*Producer, error) {
	o := producerOptions{closeTimeout: 15 * time.Second}
	for _, fn := range opts {
		fn(&o)
	}

	base, err := cfg.ClientOptions()
	if err != nil {
		return nil, err
	}

	p := &Producer{closeTimeout: o.closeTimeout, onDataLoss: o.onDataLoss}

	base = append(base,
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerBatchCompression(kgo.ZstdCompression(), kgo.SnappyCompression(), kgo.NoCompression()),
		kgo.ProducerLinger(ProducerLinger),
		kgo.ProducerBatchMaxBytes(ProducerBatchMaxBytes),
		kgo.MaxProduceRequestsInflightPerBroker(MaxInFlightPerBroker),
		kgo.MaxBufferedRecords(MaxBufferedRecords),
		kgo.MaxBufferedBytes(MaxBufferedBytes),
		kgo.ProduceRequestTimeout(ProduceRequestTimeout),
		kgo.RecordDeliveryTimeout(RecordDeliveryTimeout),
		kgo.RecordPartitioner(kgo.StickyKeyPartitioner(nil)),
		kgo.ProducerOnDataLossDetected(func(topic string, part int32) {
			if p.onDataLoss != nil {
				p.onDataLoss(topic, part)
			}
		}),
	)
	base = append(base, o.extra...)

	client, err := kgo.NewClient(base...)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeDependencyFailure, "kafka: creating producer client")
	}
	p.client = client
	return p, nil
}

// Publish sends a batch and waits for every record to be acknowledged.
//
// Synchronous on purpose. The caller is the outbox relay, and its correctness depends on knowing
// which records are durable before it marks their rows published: an asynchronous produce would
// let the relay mark a row published and then discover the write failed, which is the exact
// dual-write failure the outbox exists to eliminate. The latency this costs is inside the relay,
// where the budget is 20 ms of polling anyway.
//
// A partial failure is reported as an error, and the relay is expected to mark only the records
// it can prove succeeded (docs/events.md §7.5). The error names the count so an operator reading
// last_error can tell "the broker is down" from "one record is poison".
func (p *Producer) Publish(ctx context.Context, msgs ...ports.OutboxMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	records := make([]*kgo.Record, 0, len(msgs))
	for _, m := range msgs {
		r, err := recordFor(m)
		if err != nil {
			return err
		}
		records = append(records, r)
	}

	results := p.client.ProduceSync(ctx, records...)

	var (
		firstErr  error
		failed    int
		retryable = true
	)
	for _, res := range results {
		if res.Err == nil {
			continue
		}
		failed++
		classified := classifyProduceError(res.Err, res.Record)
		if firstErr == nil {
			firstErr = classified
		}
		// The batch is only retryable if every failure in it is. One poison record among
		// transient failures must not let the relay believe a plain retry will clear the batch.
		if !apierror.IsRetryable(classified) {
			retryable = false
		}
	}
	if firstErr == nil {
		return nil
	}
	if failed == len(records) && retryable {
		return firstErr
	}
	return apierror.Wrapf(firstErr, apierror.CodeOf(firstErr),
		"kafka: %d of %d records failed to produce", failed, len(records))
}

// PublishAsync produces without waiting, invoking cb per record.
//
// It exists for the one caller that genuinely does not need synchronous durability — the health
// probe's canary record — and is deliberately not what the relay uses. The callback runs on a
// franz-go goroutine and must not block or panic.
func (p *Producer) PublishAsync(ctx context.Context, msg ports.OutboxMessage, cb func(error)) error {
	r, err := recordFor(msg)
	if err != nil {
		return err
	}
	p.client.Produce(ctx, r, func(rec *kgo.Record, err error) {
		if cb == nil {
			return
		}
		if err != nil {
			cb(classifyProduceError(err, rec))
			return
		}
		cb(nil)
	})
	return nil
}

// Ping checks the client can reach a broker. Used by the readiness probe, which must not report
// ready while the process cannot publish — a "healthy" relay that cannot produce is worse than a
// restarting one, because nothing pages.
func (p *Producer) Ping(ctx context.Context) error {
	if err := p.client.Ping(ctx); err != nil {
		return apierror.Wrap(err, apierror.CodeDependencyFailure, "kafka: broker unreachable")
	}
	return nil
}

// Close flushes buffered records and shuts the client down. It is idempotent and bounded.
//
// The flush is bounded by closeTimeout and Close returns an error rather than hanging: buffered
// records that cannot be flushed are still committed in the outbox and will be re-claimed after
// the stale-claim window, so giving up is safe. Hanging is not — it turns a rolling deploy into
// a SIGKILL mid-produce, which is the one way to get a torn batch.
func (p *Producer) Close() error {
	var err error
	p.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), p.closeTimeout)
		defer cancel()
		if ferr := p.client.Flush(ctx); ferr != nil {
			err = apierror.Wrap(ferr, apierror.CodeDependencyFailure,
				"kafka: buffered records were not flushed within the close timeout; they remain unpublished in the outbox and will be re-claimed")
		}
		p.client.Close()
	})
	return err
}

// BufferedRecords reports the current in-memory queue depth, exported as a gauge so that the
// bound above is observable rather than merely present.
func (p *Producer) BufferedRecords() int64 { return p.client.BufferedProduceRecords() }

// recordFor renders one outbox message as a Kafka record.
//
// The key is OutboxMessage.PartitionKey — never the event id, never a round-robin. That single
// line is the entire ordering guarantee: all events for one payment carry the payment id as the
// key, hash to one partition and are therefore delivered in order. A record with no key would be
// spread across partitions and silently destroy that.
func recordFor(m ports.OutboxMessage) (*kgo.Record, error) {
	if m.Topic == "" {
		return nil, apierror.Newf(apierror.CodeInternalError, "kafka: outbox message %s has no topic", m.ID)
	}
	if m.PartitionKey == "" {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"kafka: outbox message %s (%s) has no partition key; publishing it would destroy ordering for its aggregate",
			m.ID, m.Type)
	}
	headers := make([]kgo.RecordHeader, 0, len(m.Headers))
	for k, v := range m.Headers {
		headers = append(headers, kgo.RecordHeader{Key: k, Value: []byte(v)})
	}
	r := &kgo.Record{
		Topic:   m.Topic,
		Key:     []byte(m.PartitionKey),
		Value:   m.Payload,
		Headers: headers,
	}
	// The record timestamp is when the fact occurred, not when we published it. A consumer's
	// time-lag metric is computed from this, and stamping publication time would make a
	// twenty-minute retry-tier delay look like zero lag.
	if !m.OccurredAt.IsZero() {
		r.Timestamp = m.OccurredAt
	}
	return r, nil
}

// classifyProduceError maps a broker or client error onto the platform's error model.
//
// The retryable bit is the whole point of doing this. The relay branches on it: retryable means
// leave the row unpublished and back off, non-retryable means park the row after five attempts
// and alert. Getting it backwards either hammers a permanent failure forever or parks a row that
// would have succeeded on the next attempt.
func classifyProduceError(err error, rec *kgo.Record) error {
	if err == nil {
		return nil
	}
	topic := ""
	if rec != nil {
		topic = rec.Topic
	}

	switch {
	case errors.Is(err, context.Canceled):
		// Shutdown, not a failure of the broker. Retryable: the row is still unpublished.
		return apierror.Wrapf(err, apierror.CodeServiceUnavailable,
			"kafka: produce to %s canceled during shutdown", topic)

	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, kgo.ErrRecordTimeout):
		return apierror.Wrapf(err, apierror.CodeGatewayTimeout,
			"kafka: produce to %s timed out", topic).WithMessage("kafka produce timed out")

	case errors.Is(err, kgo.ErrClientClosed), errors.Is(err, kgo.ErrAborting):
		return apierror.Wrapf(err, apierror.CodeServiceUnavailable,
			"kafka: producer is closed")

	case errors.Is(err, kgo.ErrMaxBuffered):
		// Backpressure. The bound is doing its job; the relay should slow down, not park rows.
		return apierror.Wrapf(err, apierror.CodeServiceUnavailable,
			"kafka: producer buffer is full; applying backpressure")

	case errors.Is(err, kgo.ErrRecordRetries):
		// Exhausted internal retries. Retryable at the relay level, where the backoff is longer
		// and the row is durable.
		return apierror.Wrapf(err, apierror.CodeServiceUnavailable,
			"kafka: produce to %s exhausted client retries", topic)

	// --- non-retryable broker errors: the record itself is the problem -----------------------
	case errors.Is(err, kerr.MessageTooLarge), errors.Is(err, kerr.RecordListTooLarge):
		return apierror.Wrapf(err, apierror.CodeRequestTooLarge,
			"kafka: record for %s exceeds the broker's max.message.bytes; it is poison and must be parked, not retried", topic)

	case errors.Is(err, kerr.TopicAuthorizationFailed), errors.Is(err, kerr.ClusterAuthorizationFailed):
		return apierror.Wrapf(err, apierror.CodeForbidden,
			"kafka: not authorized to produce to %s", topic)

	case errors.Is(err, kerr.SaslAuthenticationFailed):
		// Never include the credential or the broker's message, which can echo the principal.
		return apierror.Wrap(err, apierror.CodeUnauthenticated, "kafka: SASL authentication failed")

	case errors.Is(err, kerr.InvalidTopicException), errors.Is(err, kerr.InvalidRequiredAcks):
		return apierror.Wrapf(err, apierror.CodeConfigurationInvalid,
			"kafka: invalid produce request for %s", topic)

	case errors.Is(err, kerr.UnknownTopicOrPartition):
		// Retryable: a topic being created, or metadata not yet propagated after a leader change.
		// It becomes an alert only because the relay's attempt counter runs out.
		return apierror.Wrapf(err, apierror.CodeServiceUnavailable,
			"kafka: topic %s is unknown to the broker", topic)

	case errors.Is(err, kerr.NotEnoughReplicas), errors.Is(err, kerr.NotEnoughReplicasAfterAppend):
		// min.insync.replicas is not satisfied. Exactly the condition acks=all exists to surface:
		// the cluster is degraded and we would rather stall than write a record that can vanish.
		return apierror.Wrapf(err, apierror.CodeServiceUnavailable,
			"kafka: fewer than min.insync.replicas are in sync for %s; refusing to write a record that could be lost", topic)
	}

	// Anything the broker itself marks retriable — leader elections, throttling, coordinator
	// moves — is retryable.
	//
	// So is everything left over, via DEPENDENCY_FAILURE. That is the deliberate choice for the
	// unknown case: the alternative, treating an unrecognised error as permanent, parks a row
	// that would have succeeded on the next attempt, and a parked payment event is a manual
	// intervention. Retrying an unknown *permanent* failure is bounded anyway — the relay parks
	// the row after five attempts (docs/events.md §7.5) — so the cost of guessing "retryable" is
	// five attempts and the cost of guessing "permanent" is an operator's afternoon.
	if kerr.IsRetriable(err) {
		return apierror.Wrapf(err, apierror.CodeServiceUnavailable,
			"kafka: retriable broker error producing to %s", topic)
	}
	return apierror.Wrapf(err, apierror.CodeDependencyFailure,
		"kafka: produce to %s failed", topic)
}

var _ ports.EventPublisher = (*Producer)(nil)
