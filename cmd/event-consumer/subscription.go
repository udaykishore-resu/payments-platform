package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/internal/platform/runtime"
)

// subscriptionDeps is the subscription's dependency set.
type subscriptionDeps struct {
	Consumer ports.EventConsumer
	Handler  ports.EventHandler
	Topics   []string
	Group    string
	Logger   *slog.Logger
}

// subscription owns the consumer's polling goroutine and its clean group departure.
type subscription struct {
	deps subscriptionDeps

	mu      sync.Mutex
	cancel  context.CancelFunc
	stopped chan struct{}
}

func newSubscription(d subscriptionDeps) *subscription {
	return &subscription{deps: d, stopped: make(chan struct{})}
}

// Start subscribes and begins polling in a goroutine this type owns.
func (s *subscription) Start(ctx context.Context) error {
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	go func() {
		defer close(s.stopped)
		_ = runtime.Guard("kafka-consumer", s.deps.Logger, func() error {
			return s.deps.Consumer.Subscribe(loopCtx, s.deps.Topics, s.deps.Group, s.deps.Handler)
		})
	}()
	s.deps.Logger.Info("subscribed",
		slog.String("group", s.deps.Group),
		slog.Any("topics", s.deps.Topics))
	return nil
}

// Stop finishes the in-flight message, commits its offset and leaves the group.
//
// # The order matters and it is commit-then-leave
//
// Committing before leaving prevents the next member from reprocessing a batch this one already
// applied — which the dedup store would absorb, but at the cost of a full batch of wasted work
// during every deploy. Leaving cleanly, rather than letting the session time out, triggers a
// cooperative rebalance: the group re-partitions in milliseconds instead of pausing every consumer
// for the session timeout, which is tens of seconds and is felt as consumer lag across the fleet.
//
// The wait is bounded. A handler blocked on an unreachable dependency must not hold the pod past
// its grace period: the SIGKILL that follows would leave the offset uncommitted *and* the group
// membership to time out, which is both failure modes at once.
func (s *subscription) Stop(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	s.deps.Logger.Info("consumer draining: no new fetches, finishing the in-flight message")
	cancel()

	select {
	case <-s.stopped:
	case <-ctx.Done():
		s.deps.Logger.Warn("consumer did not finish within its budget; " +
			"the offset stays uncommitted and the message will be redelivered")
	}
	// Close leaves the group explicitly, which is what makes the rebalance cooperative. It runs
	// even after a budget overrun: leaving late is still better than not leaving.
	return s.deps.Consumer.Close()
}

// lagReporter forwards the consumer's lag measurements to the metrics registry.
//
// Consumer lag is the SLI for every asynchronous path in the platform — a projection that is
// behind is a merchant looking at stale data — and it is the one signal that cannot be derived
// from anything the consumer itself returns. The broker knows the high-water mark; only the
// consumer knows where it is.
type lagReporter struct {
	metrics *telemetry.Registry
}

// ReportLag records the per-topic, per-group lag.
//
// The partition is deliberately *not* a metric label. A topic with sixty-four partitions times a
// dozen groups is a series count that grows with the cluster rather than with the platform, and
// the operational question — "is this group behind?" — is answered by the maximum across
// partitions, which the aggregation already gives.
func (l lagReporter) ReportLag(topic, group string, _ int32, records int64, _ time.Duration) {
	if l.metrics == nil {
		return
	}
	l.metrics.SetConsumerLag(topic, group, float64(records))
}
