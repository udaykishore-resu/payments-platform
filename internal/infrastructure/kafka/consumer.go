package kafka

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/events"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Consumer-group settings from docs/events.md §10.2.
const (
	// SessionTimeout — long enough to survive a GC pause or a brief database stall without
	// triggering a rebalance; short enough that a genuinely dead consumer's partitions move
	// inside a minute.
	SessionTimeout = 45 * time.Second

	// HeartbeatInterval is a third of a third of the session timeout: the standard ratio, chosen
	// so that two consecutive missed heartbeats do not evict a healthy consumer.
	HeartbeatInterval = 3 * time.Second

	// RebalanceTimeout is the real liveness bound. Heartbeats run on a background goroutine, so
	// they keep reporting healthy while a handler is wedged; this is the setting that actually
	// detects one. A consumer that cannot finish a batch in five minutes is stuck and its
	// partitions should move.
	RebalanceTimeout = 5 * time.Minute

	// MaxPollRecords — with a database write per record, 100 must complete well inside
	// RebalanceTimeout. 500 would risk a rebalance loop during a database slowdown, which is the
	// classic "consumer group rebalances forever under load" failure: every rebalance makes the
	// next batch later, which makes the next rebalance more likely.
	MaxPollRecords = 100

	// DefaultConcurrency is how many partitions one instance processes at once. It bounds
	// database connection demand: concurrency × instances must stay under the pool size, or the
	// consumers starve the API of connections during a backlog drain.
	DefaultConcurrency = 8

	// commitTimeout bounds an offset commit. A commit that hangs holds the poll loop and
	// eventually trips the rebalance timeout.
	commitTimeout = 15 * time.Second

	// haltBackoffMin and haltBackoffMax bound the wait after a partition halts. The cap is short
	// because the halt is per-partition, not per-group: the other partitions keep flowing, so the
	// cost of waiting is bounded to one ordering domain.
	haltBackoffMin = 50 * time.Millisecond
	haltBackoffMax = 5 * time.Second
)

// groupNamePattern is docs/events.md §10.1: pp.<service>[.<purpose>].v<n>.
var groupNamePattern = regexp.MustCompile(`^pp\.[a-z0-9-]+(\.[a-z0-9-]+)*\.v[1-9][0-9]*$`)

// Router decides where a record goes when the handler could not process it.
//
// It is an interface so that the consumer does not depend on the retry topology: a strict-
// ordering consumer (docs/events.md §9.4) installs a router that pauses the partition instead of
// republishing, and the consumer needs no knowledge of which it got.
type Router interface {
	// Route disposes of a failed record. Returning nil means the record has been dealt with and
	// its offset may be committed; returning an error means it has not, and the consumer must
	// not advance past it.
	Route(ctx context.Context, rec *kgo.Record, handlerErr error) error
}

// LagReporter receives lag observations. Implemented by the telemetry registry.
//
// Both dimensions are reported because they mean different things: 10 000 records of lag on a
// 6-partition topic is a different situation from the same number on a 48-partition one, and the
// SLOs in docs/events.md §10.3 are stated in time.
type LagReporter interface {
	ReportLag(topic, group string, partition int32, records int64, age time.Duration)
}

// ConsumerObserver receives per-record outcomes for logging. It exists so this package does not
// import the logging package, and so tests can assert on behaviour without a log scraper.
type ConsumerObserver interface {
	RecordHandled(topic string, partition int32, offset int64, err error)
	RebalanceAssigned(assigned map[string][]int32)
	RebalanceRevoked(revoked map[string][]int32)
}

// Consumer subscribes to topics and delivers records to a ports.EventHandler.
//
// It implements ports.EventConsumer. Its shape is dictated by three requirements that pull
// against each other:
//
//  1. Offsets commit only after the handler succeeds. Auto-commit is disabled, permanently, and
//     the reason is worth stating plainly: auto-commit acknowledges offsets on a timer, without
//     any knowledge of whether the handler's transaction committed. A crash between the timer
//     firing and the handler finishing means those events are never redelivered and never
//     applied — silently skipped, no error, no metric, discovered weeks later by a
//     reconciliation. There is no configuration to turn it on.
//  2. Throughput requires concurrency. One record at a time across 48 partitions would cap a
//     consumer instance at a few hundred records a second.
//  3. Ordering must survive that concurrency. So the unit of parallelism is the *partition*: one
//     goroutine per assigned partition per poll, records within a partition processed strictly
//     in offset order. Two payments never contend, and one payment's events never race.
type Consumer struct {
	cfg  Config
	opts consumerOptions

	mu     sync.Mutex
	client *kgo.Client
	closed bool

	// done is closed when the poll loop has exited, so Close can wait for it rather than racing
	// the client shutdown against an in-flight handler.
	done chan struct{}
}

type consumerOptions struct {
	router         Router
	lag            LagReporter
	observer       ConsumerObserver
	concurrency    int
	maxPollRecords int
	instanceID     string
	sessionTimeout time.Duration
	rebalanceTO    time.Duration
	fromLatest     bool
	extra          []kgo.Opt
}

// ConsumerOption configures a Consumer.
type ConsumerOption func(*consumerOptions)

// WithRouter installs the failure router (retry tiers, or partition blocking).
//
// Without one, a handler failure halts the partition — which is safe but is the wrong default
// for most consumers, so wiring is expected to install NewRetryRouter.
func WithRouter(r Router) ConsumerOption {
	return func(o *consumerOptions) { o.router = r }
}

// WithLagReporter installs the lag gauge sink.
func WithLagReporter(l LagReporter) ConsumerOption {
	return func(o *consumerOptions) { o.lag = l }
}

// WithConsumerObserver installs the per-record and rebalance callbacks.
func WithConsumerObserver(obs ConsumerObserver) ConsumerOption {
	return func(o *consumerOptions) { o.observer = obs }
}

// WithConcurrency bounds how many partitions are processed simultaneously.
func WithConcurrency(n int) ConsumerOption {
	return func(o *consumerOptions) {
		if n > 0 {
			o.concurrency = n
		}
	}
}

// WithMaxPollRecords bounds a single poll.
func WithMaxPollRecords(n int) ConsumerOption {
	return func(o *consumerOptions) {
		if n > 0 {
			o.maxPollRecords = n
		}
	}
}

// WithGroupInstanceID enables static membership, from the StatefulSet ordinal.
//
// This is what turns a rolling restart of an N-instance consumer group from N rebalances into
// zero: a member that returns within the session timeout with the same instance id keeps its
// partitions instead of triggering a reassignment.
func WithGroupInstanceID(id string) ConsumerOption {
	return func(o *consumerOptions) { o.instanceID = id }
}

// WithConsumerOptions appends raw franz-go options, for integration tests.
func WithConsumerOptions(opts ...kgo.Opt) ConsumerOption {
	return func(o *consumerOptions) { o.extra = append(o.extra, opts...) }
}

// NewConsumer builds a consumer. It connects on Subscribe, not here, so that constructing one at
// wiring time cannot fail because a broker was briefly unreachable.
func NewConsumer(cfg Config, opts ...ConsumerOption) (*Consumer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	o := consumerOptions{
		concurrency:    DefaultConcurrency,
		maxPollRecords: MaxPollRecords,
		sessionTimeout: SessionTimeout,
		rebalanceTO:    RebalanceTimeout,
	}
	for _, fn := range opts {
		fn(&o)
	}
	return &Consumer{cfg: cfg, opts: o, done: make(chan struct{})}, nil
}

// Subscribe joins the group and runs the poll loop until ctx is canceled or a fatal error occurs.
//
// It blocks. The caller — a `cmd/event-consumer` main — runs it on the main goroutine and cancels
// ctx from its SIGTERM handler, so there is exactly one owner of this loop and its lifetime is
// visibly the process's.
func (c *Consumer) Subscribe(ctx context.Context, topics []string, group string, h ports.EventHandler) error {
	if len(topics) == 0 {
		return apierror.New(apierror.CodeConfigurationInvalid, "kafka: no topics to subscribe to")
	}
	if err := validateGroupName(group); err != nil {
		return err
	}
	if h == nil {
		return apierror.New(apierror.CodeInternalError, "kafka: no handler")
	}

	client, err := c.newGroupClient(topics, group)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		client.Close()
		return apierror.New(apierror.CodeServiceUnavailable, "kafka: consumer is closed")
	}
	c.client = client
	c.mu.Unlock()

	defer close(c.done)
	defer c.leaveGroup(client) //nolint:contextcheck // leaveGroup runs on a fresh bounded context: Subscribe's ctx is already canceled when this defer fires, and leaving the group is the work that must still happen

	return c.pollLoop(ctx, client, group, h)
}

// newGroupClient builds the consumer-group client with the settings from §10.2.
func (c *Consumer) newGroupClient(topics []string, group string) (*kgo.Client, error) {
	base, err := c.cfg.ClientOptions()
	if err != nil {
		return nil, err
	}

	// auto.offset.reset=earliest. A new group must see history: `latest` silently skips whatever
	// arrived before the consumer started, and invisible data loss is the worst kind.
	reset := kgo.NewOffset().AtStart()
	if c.opts.fromLatest {
		reset = kgo.NewOffset().AtEnd()
	}

	base = append(base,
		kgo.ConsumeTopics(topics...),
		kgo.ConsumerGroup(group),

		// CooperativeSticky. An eager assignor revokes every partition from every consumer on any
		// membership change, so a rolling deploy of a 30-instance group produces 30 stop-the-world
		// rebalances. Cooperative moves only the partitions that must move.
		kgo.Balancers(kgo.CooperativeStickyBalancer()),

		kgo.SessionTimeout(c.opts.sessionTimeout),
		kgo.HeartbeatInterval(HeartbeatInterval),
		kgo.RebalanceTimeout(c.opts.rebalanceTO),
		kgo.ConsumeResetOffset(reset),
		kgo.FetchIsolationLevel(kgo.ReadCommitted()),

		// The two that carry the correctness of this file.
		//
		// DisableAutoCommit: offsets are committed by CommitRecords after the handler's
		// transaction commits. See the note on the Consumer type — auto-commit plus a crash
		// equals silently skipped events.
		kgo.DisableAutoCommit(),
		// BlockRebalanceOnPoll: a rebalance cannot start while the records from a poll are still
		// being processed. Without it, partitions can be revoked mid-batch and this consumer
		// would commit offsets for a partition another member already owns — which either fails
		// loudly or, worse, succeeds and moves someone else's offset forward past unprocessed
		// records. AllowRebalance is called exactly once per poll, after processing and
		// committing.
		kgo.BlockRebalanceOnPoll(),

		kgo.OnPartitionsAssigned(func(_ context.Context, _ *kgo.Client, assigned map[string][]int32) {
			if c.opts.observer != nil {
				c.opts.observer.RebalanceAssigned(assigned)
			}
		}),
		kgo.OnPartitionsRevoked(func(ctx context.Context, cl *kgo.Client, revoked map[string][]int32) {
			// Commit what is already done for the partitions being taken away. Everything
			// in-flight has already finished, because BlockRebalanceOnPoll held the rebalance
			// until AllowRebalance. Failing to commit here is survivable — the new owner
			// reprocesses from the last committed offset and the dedup store drops the
			// duplicates — which is precisely why at-least-once is the delivery contract.
			if err := cl.CommitUncommittedOffsets(ctx); err != nil && c.opts.observer != nil {
				c.opts.observer.RecordHandled("", -1, -1,
					apierror.Wrap(err, apierror.CodeDependencyFailure,
						"kafka: committing offsets during revocation; the new owner will reprocess and dedup will drop the duplicates"))
			}
			if c.opts.observer != nil {
				c.opts.observer.RebalanceRevoked(revoked)
			}
		}),
	)
	if c.opts.instanceID != "" {
		base = append(base, kgo.InstanceID(c.opts.instanceID))
	}
	base = append(base, c.opts.extra...)

	client, err := kgo.NewClient(base...)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeDependencyFailure, "kafka: creating consumer client")
	}
	return client, nil
}

// pollLoop is the consumer's only long-running loop. Its owner is Subscribe's caller, and it
// exits when ctx is canceled or the client is closed — there is no other exit and no goroutine
// outlives it.
func (c *Consumer) pollLoop(ctx context.Context, client *kgo.Client, group string, h ports.EventHandler) error {
	backoff := time.Duration(0)

	for {
		if ctx.Err() != nil {
			return nil //nolint:nilerr // a canceled context is this loop's only clean exit, not a failure to report
		}
		if backoff > 0 {
			// A halted partition would otherwise be re-fetched immediately and fail again at full
			// speed, turning one poison record into a hot loop against the database it just
			// failed against.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
		}

		fetches := client.PollRecords(ctx, c.opts.maxPollRecords)

		// A closed client or a canceled context is a normal shutdown, not a failure.
		if fetches.IsClientClosed() {
			return nil
		}
		if ctx.Err() != nil {
			client.AllowRebalance()
			return nil //nolint:nilerr // a canceled context is a normal shutdown; the rebalance is allowed first so the group does not wedge
		}

		fatal := c.reportFetchErrors(fetches)
		halted := c.processFetches(ctx, client, group, h, fetches)

		// AllowRebalance must run on every path out of the poll, including the error paths, or a
		// rebalance blocks forever and the group wedges.
		client.AllowRebalance()

		if fatal != nil {
			return fatal
		}
		backoff = nextBackoff(backoff, halted)
	}
}

// nextBackoff grows the halt backoff geometrically and resets it the moment a poll makes
// progress, so a transient database stall costs milliseconds and a genuine poison record settles
// at the cap instead of spinning.
func nextBackoff(current time.Duration, halted int) time.Duration {
	if halted == 0 {
		return 0
	}
	switch {
	case current == 0:
		return haltBackoffMin
	case current*2 >= haltBackoffMax:
		return haltBackoffMax
	default:
		return current * 2
	}
}

// processFetches runs one poll's records: one goroutine per partition, bounded by concurrency,
// records within a partition strictly in order, commit after the whole poll.
//
// It returns the number of partitions that halted, which the poll loop uses to decide whether to
// back off before fetching again.
func (c *Consumer) processFetches(ctx context.Context, client *kgo.Client, group string, h ports.EventHandler, fetches kgo.Fetches) int {
	type partitionWork struct {
		topic     string
		partition int32
		highWater int64
		records   []*kgo.Record
	}

	var work []partitionWork
	fetches.EachPartition(func(p kgo.FetchTopicPartition) {
		if len(p.Records) == 0 {
			return
		}
		work = append(work, partitionWork{
			topic: p.Topic, partition: p.Partition, highWater: p.HighWatermark, records: p.Records,
		})
	})
	if len(work) == 0 {
		return 0
	}

	sem := make(chan struct{}, c.opts.concurrency)
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		commit []*kgo.Record
		// rewind holds, per halted partition, the offset the next fetch must resume from.
		rewind = map[string]map[int32]kgo.EpochOffset{}
	)

	for _, w := range work {
		wg.Add(1)
		sem <- struct{}{}
		go func(w partitionWork) {
			defer wg.Done()
			defer func() { <-sem }()

			// Strictly sequential within the partition. The first record that cannot be disposed
			// of stops this partition: committing past it would skip it permanently, and
			// processing the next record would apply version n+1 before version n.
			var (
				lastOK *kgo.Record
				halted *kgo.Record
			)
			for _, rec := range w.records {
				if ctx.Err() != nil {
					halted = rec
					break
				}
				if err := c.handleRecord(ctx, h, rec); err != nil {
					halted = rec
					break
				}
				lastOK = rec
			}

			c.reportLag(group, w.topic, w.partition, w.highWater, lastOK)

			mu.Lock()
			defer mu.Unlock()
			if lastOK != nil {
				commit = append(commit, lastOK)
			}
			if halted != nil {
				// Rewind the *client's* fetch position to the failed record.
				//
				// This is the subtlety that makes "do not commit past a failure" actually work:
				// franz-go's read position advances with the fetch, not with the commit, so
				// simply declining to commit would leave the record fetched, unprocessed and
				// never seen again in this client's lifetime — a silently skipped event, which
				// is exactly what disabling auto-commit was supposed to prevent.
				if rewind[w.topic] == nil {
					rewind[w.topic] = map[int32]kgo.EpochOffset{}
				}
				rewind[w.topic][w.partition] = kgo.EpochOffset{
					Epoch: halted.LeaderEpoch, Offset: halted.Offset,
				}
			}
		}(w)
	}
	wg.Wait()

	if len(commit) > 0 {
		// CommitRecords commits offset+1 for the highest offset per partition, which is exactly
		// "everything up to and including this record is done". It runs after every handler has
		// returned, so no offset can be committed for work that is still in flight.
		//
		// context.WithoutCancel: on shutdown the work is already committed in the database, and
		// abandoning the offset commit because ctx just became Done means reprocessing it all on
		// the next start. Bounded separately so it cannot hang the shutdown.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
		err := client.CommitRecords(cctx, commit...)
		cancel()
		if err != nil {
			// A failed commit is not data loss: the records will be redelivered and dropped by
			// the dedup store. It is worth reporting because a *persistent* commit failure means
			// the group is not making progress.
			if c.opts.observer != nil {
				c.opts.observer.RecordHandled("", -1, -1, apierror.Wrap(err,
					apierror.CodeDependencyFailure, "kafka: offset commit failed; records will be redelivered"))
			}
		}
	}

	if len(rewind) > 0 {
		client.SetOffsets(rewind)
	}
	return len(rewind)
}

// handleRecord runs the handler for one record and routes a failure.
func (c *Consumer) handleRecord(ctx context.Context, h ports.EventHandler, rec *kgo.Record) error {
	msg := outboxMessageFor(rec)

	err := h.Handle(ctx, msg)

	// A tier delay that has not elapsed is not a failure and must never be routed: routing it
	// would advance the record to the next tier without ever having tried it. Halting the
	// partition here is what "pause rather than seek" means at this level — the caller rewinds to
	// this record and backs off, and a DelayedHandler with a pauser has already stopped the fetch.
	if errors.Is(err, ErrDelayNotElapsed) {
		return err
	}

	if c.opts.observer != nil {
		c.opts.observer.RecordHandled(rec.Topic, rec.Partition, rec.Offset, err)
	}
	if err == nil {
		return nil
	}

	if c.opts.router == nil {
		// No router: halt the partition. Safe, and the right default for a consumer whose
		// operator has not decided what should happen to failures yet.
		return apierror.Wrapf(err, apierror.CodeOf(err),
			"kafka: %s[%d]@%d failed and no router is configured; halting the partition",
			rec.Topic, rec.Partition, rec.Offset)
	}
	if rerr := c.opts.router.Route(ctx, rec, err); rerr != nil {
		return apierror.Wrapf(rerr, apierror.CodeDependencyFailure,
			"kafka: %s[%d]@%d could not be routed after a handler failure; halting the partition rather than skipping it",
			rec.Topic, rec.Partition, rec.Offset)
	}
	return nil
}

// reportLag publishes the two lag dimensions for a partition.
func (c *Consumer) reportLag(group, topic string, partition int32, highWater int64, last *kgo.Record) {
	if c.opts.lag == nil || last == nil {
		return
	}
	records := highWater - (last.Offset + 1)
	if records < 0 {
		records = 0
	}
	age := time.Duration(0)
	if !last.Timestamp.IsZero() {
		age = time.Since(last.Timestamp)
		if age < 0 {
			age = 0
		}
	}
	c.opts.lag.ReportLag(topic, group, partition, records, age)
}

// reportFetchErrors surfaces per-partition fetch errors and reports whether any is fatal.
//
// A fetch error is not a handler error: it means this consumer could not read, so there is
// nothing to route. Most are transient (leader elections); the fatal ones — an authorization
// failure, a partition that will never be fetched again — must stop the loop rather than spin.
func (c *Consumer) reportFetchErrors(fetches kgo.Fetches) error {
	var fatal error
	for _, fe := range fetches.Errors() {
		err := classifyFetchError(fe.Err, fe.Topic, fe.Partition)
		if c.opts.observer != nil {
			c.opts.observer.RecordHandled(fe.Topic, fe.Partition, -1, err)
		}
		if !apierror.IsRetryable(err) && fatal == nil {
			fatal = err
		}
	}
	return fatal
}

// leaveGroup leaves explicitly on shutdown.
//
// Leaving explicitly triggers reassignment immediately instead of after the 45-second session
// timeout, which is the difference between a rolling deploy costing seconds of lag and costing
// forty-five seconds per instance.
func (c *Consumer) leaveGroup(client *kgo.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), commitTimeout)
	defer cancel()
	// Best effort. If it fails the session timeout takes over, which is slower but correct.
	_ = client.LeaveGroupContext(ctx)
}

// Close stops the consumer. It is idempotent and bounded: it closes the client, which unblocks
// PollRecords, then waits for the poll loop to exit so no handler is still running when the
// process tears down its database pool.
func (c *Consumer) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	client := c.client
	c.mu.Unlock()

	if client == nil {
		// Never subscribed. Nothing to wait for.
		return nil
	}
	client.CloseAllowingRebalance()

	select {
	case <-c.done:
		return nil
	case <-time.After(commitTimeout):
		return apierror.New(apierror.CodeServiceUnavailable,
			"kafka: consumer poll loop did not exit within the close timeout")
	}
}

// outboxMessageFor rebuilds the port's message shape from a broker record.
//
// The envelope in the value is authoritative; the headers are a broker-level convenience that a
// mirror-maker or a misconfigured producer could have written incorrectly. So the fields that
// matter are read back out of the decoded envelope by the handler, and the header values here
// are carried for observability only.
func outboxMessageFor(rec *kgo.Record) ports.OutboxMessage {
	headers := make(map[string]string, len(rec.Headers))
	for _, h := range rec.Headers {
		headers[h.Key] = string(h.Value)
	}
	// Consumer-side annotations, so a delay tier can pause the right partition without the
	// handler being handed a *kgo.Record and thereby coupled to the broker. They are stripped
	// before any republish (see preservedHeaders) because a partition number from one topic is
	// meaningless on another.
	headers[HeaderKafkaPartition] = strconv.FormatInt(int64(rec.Partition), 10)
	headers[HeaderKafkaOffset] = strconv.FormatInt(rec.Offset, 10)

	return ports.OutboxMessage{
		ID:           shared.EventID(headers[events.HeaderEventID]),
		TenantID:     shared.TenantID(headers[events.HeaderTenantID]),
		Topic:        rec.Topic,
		Type:         headers[events.HeaderEventType],
		AggregateID:  headers[events.HeaderAggregateID],
		PartitionKey: string(rec.Key),
		Payload:      rec.Value,
		Headers:      headers,
		OccurredAt:   rec.Timestamp,
	}
}

// classifyFetchError maps a fetch error onto the platform's model.
func classifyFetchError(err error, topic string, partition int32) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, kgo.ErrClientClosed):
		return apierror.Wrap(err, apierror.CodeServiceUnavailable, "kafka: consumer shutting down")
	case errors.Is(err, kerr.TopicAuthorizationFailed):
		return apierror.Wrapf(err, apierror.CodeForbidden,
			"kafka: not authorized to consume %s[%d]", topic, partition)
	case errors.Is(err, kerr.GroupAuthorizationFailed):
		return apierror.Wrap(err, apierror.CodeForbidden, "kafka: not authorized to join the consumer group")
	case errors.Is(err, kerr.SaslAuthenticationFailed):
		return apierror.Wrap(err, apierror.CodeUnauthenticated, "kafka: SASL authentication failed")
	}
	return apierror.Wrapf(err, apierror.CodeServiceUnavailable,
		"kafka: fetching %s[%d]", topic, partition)
}

// validateGroupName enforces the naming rule from docs/events.md §10.1.
//
// It is enforced rather than documented because the `v<n>` suffix is the replay lever: a group
// named without it has no way to force a full re-read short of inventing a name, and a group
// named `pp.ledger.projection` will one day be joined by someone typing
// `pp.ledger.projection.v2` and quietly reprocessing the whole topic under a different identity.
func validateGroupName(group string) error {
	if !groupNamePattern.MatchString(group) {
		return apierror.Newf(apierror.CodeConfigurationInvalid,
			"kafka: consumer group %q must be pp.<service>[.<purpose>].v<n>", group).
			WithDetail(apierror.Detail{
				Field: "group", Code: "CONSUMER_GROUP_NAME_INVALID",
				Message: "the v<n> suffix is the replay lever; a group without it cannot be replayed without changing identity",
				RuleID:  "L4.CONSUMER_GROUP_NAME",
			})
	}
	return nil
}

// String makes a consumer self-describing in a log line.
func (c *Consumer) String() string {
	return fmt.Sprintf("kafka.Consumer(concurrency=%d maxPoll=%d)", c.opts.concurrency, c.opts.maxPollRecords)
}

var _ ports.EventConsumer = (*Consumer)(nil)
