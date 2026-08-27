//go:build chaos

package chaos

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// A fault is a decorator over a port, and every fault in this file has the same shape:
//
//	slow := SlowBy(200*time.Millisecond)
//	broken := Chain(realGateway, FailAfter(2, errUnavailable), slow)
//
// Decorators rather than a configurable mock, for two reasons that matter to what these tests are
// for. First, they compose: "slow *and* failing after two calls" is the combination that produces
// the interesting behaviour, and a mock with a `mode` field cannot express it. Second, each one is
// a single named behaviour with a single reason to exist, so a scenario table row reads as a
// sentence — `{"gateway 5xx storm", FailAfter(0, errServerError)}` — rather than as configuration.
//
// Every fault records how many calls it saw, because "the fault fired" is itself an assertion: a
// chaos test whose fault silently did nothing is a chaos test that passed for the wrong reason,
// and that failure mode is invisible without a counter.

// Errors the faults raise. They are apierrors so the production classifier — which decides
// retryability, and therefore whether a payment is retried at all — sees the same shapes it sees
// from a real adapter.
var (
	// errGatewayUnavailable models a 5xx. Per baseline §12.3 a 5xx from a money-moving call is an
	// *unknown* outcome, not a failure: no gateway guarantees a 500 means nothing happened.
	errGatewayUnavailable = apierror.Wrap(spi.ErrOutcomeUnknown, apierror.CodeGatewayTimeout,
		"chaos: the gateway returned 503 and does not guarantee the request was not processed")

	// errGatewayTimeout models a client timeout after the request was written. The single most
	// dangerous error in the platform.
	errGatewayTimeout = apierror.Wrap(spi.ErrOutcomeUnknown, apierror.CodeGatewayTimeout,
		"chaos: the request was written and no answer arrived; the outcome is unknown")

	// errPartitioned models a network partition on the path to the gateway. Unlike the two above
	// this one is *knowable*: a connection that was refused or a route that does not exist means
	// the request never reached the vendor, so it is a plain failure and not an unknown outcome.
	// Conflating the two is how a partition turns into a reconciliation queue nobody can drain.
	errPartitioned = apierror.New(apierror.CodeGatewayUnavailable,
		"chaos: no route to the gateway; the request was never sent")

	// errDatabaseUnavailable models the Postgres primary disappearing mid-transaction.
	errDatabaseUnavailable = apierror.New(apierror.CodeServiceUnavailable,
		"chaos: the database is unavailable")

	// errPoolExhausted models every connection being in use.
	errPoolExhausted = apierror.New(apierror.CodeServiceUnavailable,
		"chaos: connection pool exhausted; no connection available within the acquire timeout")

	// errRedisUnavailable models the accelerator being gone. Redis is non-authoritative, so this
	// must degrade latency and nothing else.
	errRedisUnavailable = apierror.New(apierror.CodeServiceUnavailable,
		"chaos: redis is unavailable")

	// errBrokerUnavailable models Kafka being down.
	errBrokerUnavailable = apierror.New(apierror.CodeServiceUnavailable,
		"chaos: no leader for partition; the broker is unavailable")
)

// GatewayFault decorates a gateway adapter.
type GatewayFault func(spi.PaymentGateway) spi.PaymentGateway

// Chain applies faults left to right, so the first named is the outermost.
//
// Outermost-first matters: `Chain(g, TimeoutAlways(), SlowBy(d))` times out without ever paying
// the latency, while `Chain(g, SlowBy(d), TimeoutAlways())` is slow *and then* times out. The two
// model different things — a dead gateway and an overloaded one — and reading the order off the
// call is how a scenario says which it means.
func Chain(g spi.PaymentGateway, faults ...GatewayFault) spi.PaymentGateway {
	for i := len(faults) - 1; i >= 0; i-- {
		g = faults[i](g)
	}
	return g
}

// Counter is the shared "did the fault actually fire" tally.
type Counter struct {
	calls   atomic.Int64
	injects atomic.Int64
}

// Calls is how many times the decorated port was invoked.
func (c *Counter) Calls() int { return int(c.calls.Load()) }

// Injections is how many of those the fault actually acted on.
func (c *Counter) Injections() int { return int(c.injects.Load()) }

// faultyGateway is the single implementation every gateway fault is built from.
type faultyGateway struct {
	inner spi.PaymentGateway
	count *Counter
	// before runs before the call. Returning a non-nil error short-circuits the inner adapter,
	// which is the difference between "the vendor never saw it" and "the vendor saw it and we did
	// not hear back" — the distinction the whole platform is organised around.
	before func(ctx context.Context, op shared.Operation) error
}

func (f *faultyGateway) ID() shared.GatewayID { return f.inner.ID() }

func (f *faultyGateway) call(ctx context.Context, op shared.Operation, fn func() (*spi.Result, error)) (*spi.Result, error) {
	f.count.calls.Add(1)
	if f.before != nil {
		if err := f.before(ctx, op); err != nil {
			f.count.injects.Add(1)
			return nil, err
		}
	}
	return fn()
}

func (f *faultyGateway) Authorize(ctx context.Context, req spi.AuthorizeRequest) (*spi.Result, error) {
	return f.call(ctx, shared.OpAuthorize, func() (*spi.Result, error) { return f.inner.Authorize(ctx, req) })
}

func (f *faultyGateway) Capture(ctx context.Context, req spi.CaptureRequest) (*spi.Result, error) {
	return f.call(ctx, shared.OpCapture, func() (*spi.Result, error) { return f.inner.Capture(ctx, req) })
}

func (f *faultyGateway) Refund(ctx context.Context, req spi.RefundRequest) (*spi.Result, error) {
	return f.call(ctx, shared.OpRefund, func() (*spi.Result, error) { return f.inner.Refund(ctx, req) })
}

func (f *faultyGateway) Void(ctx context.Context, req spi.VoidRequest) (*spi.Result, error) {
	return f.call(ctx, shared.OpVoid, func() (*spi.Result, error) { return f.inner.Void(ctx, req) })
}

// Lookup is deliberately *not* faulted by the constructors below unless a scenario asks for it.
//
// Lookup is how a timed-out payment is resolved. A fault that also broke Lookup would model a
// gateway that is completely gone, which is a different and much less interesting scenario than
// "the call timed out and the reconciler can still ask what happened" — and it is the second that
// the never-retry-an-unknown-outcome rule exists to make survivable.
func (f *faultyGateway) Lookup(ctx context.Context, req spi.LookupRequest) (*spi.Result, error) {
	f.count.calls.Add(1)
	return f.inner.Lookup(ctx, req)
}

// TimeoutAlways makes every mutating call return an unknown outcome. C-1, FS-1.
func TimeoutAlways(c *Counter) GatewayFault {
	return func(inner spi.PaymentGateway) spi.PaymentGateway {
		return &faultyGateway{inner: inner, count: c,
			before: func(context.Context, shared.Operation) error { return errGatewayTimeout }}
	}
}

// FailAfter lets the first n calls through and fails every call after them. C-2.
//
// `FailAfter(0, err)` is a permanent fault and reads better than a separate constructor for it.
func FailAfter(c *Counter, n int, err error) GatewayFault {
	var seen atomic.Int64
	return func(inner spi.PaymentGateway) spi.PaymentGateway {
		return &faultyGateway{inner: inner, count: c,
			before: func(context.Context, shared.Operation) error {
				if seen.Add(1) <= int64(n) {
					return nil
				}
				return err
			}}
	}
}

// FailFor makes the first n calls fail and lets everything after them through.
//
// The mirror image of FailAfter, and the one a recovery scenario needs: a storm that ends is the
// case where "did the system come back" is a question with an answer.
func FailFor(c *Counter, n int, err error) GatewayFault {
	var seen atomic.Int64
	return func(inner spi.PaymentGateway) spi.PaymentGateway {
		return &faultyGateway{inner: inner, count: c,
			before: func(context.Context, shared.Operation) error {
				if seen.Add(1) <= int64(n) {
					return err
				}
				return nil
			}}
	}
}

// SlowBy adds latency before the call, honouring the caller's deadline. C-19.
//
// It waits on the context rather than sleeping blind, because the property most latency scenarios
// are about is whether the *caller's* timeout cascade fires — and an adapter that ignored the
// deadline would make the whole cascade untestable while looking correct.
func SlowBy(c *Counter, d time.Duration) GatewayFault {
	return func(inner spi.PaymentGateway) spi.PaymentGateway {
		return &faultyGateway{inner: inner, count: c,
			before: func(ctx context.Context, _ shared.Operation) error {
				c.injects.Add(1)
				timer := time.NewTimer(d)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					// The deadline expired while the vendor was thinking. The request was already
					// written, so the outcome is unknown — never a failure.
					return errGatewayTimeout
				case <-timer.C:
					// Not counted as an injection twice; the increment above already recorded it.
					return nil
				}
			}}
	}
}

// PartitionFor fails every call for a window measured on an injected clock. C-18, FS-10.
//
// The window is read from a clock the test controls rather than from the wall, so "three minutes
// of partition" costs no wall-clock time and the healing moment is exact rather than approximate.
func PartitionFor(c *Counter, clk shared.Clock, d time.Duration) GatewayFault {
	var (
		once  sync.Once
		start time.Time
	)
	return func(inner spi.PaymentGateway) spi.PaymentGateway {
		return &faultyGateway{inner: inner, count: c,
			before: func(context.Context, shared.Operation) error {
				once.Do(func() { start = clk.Now() })
				if clk.Now().Sub(start) < d {
					return errPartitioned
				}
				return nil
			}}
	}
}

// --- faults over the persistence port ------------------------------------------------------------

// FaultyUnitOfWork decorates ports.UnitOfWork.
//
// The two faults it can inject are genuinely different and a test must say which it means:
//
//   - `FailBefore` refuses to open the transaction. Nothing was written; the request fails closed
//     with a retryable error and the client may safely retry. This is a pool exhaustion or a
//     refused connection.
//   - `FailDuring` lets the work run and fails the *commit*. Everything the callback did is rolled
//     back — which is the property the outbox pattern rests on, because the state row and the
//     event row are in that same transaction. This is the primary disappearing mid-transaction.
type FaultyUnitOfWork struct {
	Inner  ports.UnitOfWork
	Faults Counter

	mu          sync.Mutex
	failBefore  int
	failDuring  int
	beforeErr   error
	duringErr   error
	commitCount int
	onCommit    func()
}

// NewFaultyUnitOfWork wraps a unit of work.
func NewFaultyUnitOfWork(inner ports.UnitOfWork) *FaultyUnitOfWork {
	return &FaultyUnitOfWork{Inner: inner}
}

// FailBefore makes the next n transactions refuse to open.
func (u *FaultyUnitOfWork) FailBefore(n int, err error) *FaultyUnitOfWork {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.failBefore, u.beforeErr = n, err
	return u
}

// FailDuring makes the next n transactions run their body and then fail to commit.
func (u *FaultyUnitOfWork) FailDuring(n int, err error) *FaultyUnitOfWork {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.failDuring, u.duringErr = n, err
	return u
}

// Heal clears every pending fault.
func (u *FaultyUnitOfWork) Heal() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.failBefore, u.failDuring = 0, 0
}

// Commits is how many transactions actually committed.
func (u *FaultyUnitOfWork) Commits() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.commitCount
}

func (u *FaultyUnitOfWork) take() (before error, during error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.failBefore > 0 {
		u.failBefore--
		return u.beforeErr, nil
	}
	if u.failDuring > 0 {
		u.failDuring--
		return nil, u.duringErr
	}
	return nil, nil
}

// Within implements ports.UnitOfWork.
func (u *FaultyUnitOfWork) Within(ctx context.Context, fn func(context.Context, ports.Repositories) error) error {
	return u.run(ctx, u.Inner.Within, fn)
}

// WithinSerializable implements ports.UnitOfWork.
func (u *FaultyUnitOfWork) WithinSerializable(ctx context.Context, fn func(context.Context, ports.Repositories) error) error {
	return u.run(ctx, u.Inner.WithinSerializable, fn)
}

func (u *FaultyUnitOfWork) run(
	ctx context.Context,
	inner func(context.Context, func(context.Context, ports.Repositories) error) error,
	fn func(context.Context, ports.Repositories) error,
) error {
	u.Faults.calls.Add(1)
	before, during := u.take()
	if before != nil {
		u.Faults.injects.Add(1)
		return before
	}

	// The commit failure is expressed by returning an error *from the callback*, which is exactly
	// how the real unit of work turns a failure into a rollback. Injecting it any other way would
	// leave the callback's writes visible, which is the opposite of what a failed commit does and
	// would make the test assert the wrong thing.
	err := inner(ctx, func(c context.Context, r ports.Repositories) error {
		if err := fn(c, r); err != nil {
			return err
		}
		if during != nil {
			u.Faults.injects.Add(1)
			return during
		}
		return nil
	})
	if err == nil {
		u.mu.Lock()
		u.commitCount++
		hook := u.onCommit
		u.mu.Unlock()
		// Outside the lock: the hook walks the store, and holding this mutex while it does would
		// make a hook that touched the unit of work deadlock rather than fail.
		if hook != nil {
			hook()
		}
	}
	return err
}

// --- faults over the accelerator ports ----------------------------------------------------------

// FaultyVelocity decorates ports.VelocityCounter, which is Redis-backed in production.
//
// Redis is a non-authoritative accelerator (baseline §14.3, C-7): losing it must degrade latency
// and nothing else. This decorator is how that claim gets tested rather than asserted.
type FaultyVelocity struct {
	Inner  ports.VelocityCounter
	Faults Counter

	down atomic.Bool
	// Latency is added to every call while up, so a "Redis is slow" scenario and a "Redis is gone"
	// scenario use the same decorator.
	Latency time.Duration
}

// NewFaultyVelocity wraps a velocity counter.
func NewFaultyVelocity(inner ports.VelocityCounter) *FaultyVelocity {
	return &FaultyVelocity{Inner: inner}
}

// Down takes the accelerator away.
func (v *FaultyVelocity) Down() { v.down.Store(true) }

// Up brings it back.
func (v *FaultyVelocity) Up() { v.down.Store(false) }

func (v *FaultyVelocity) guard(ctx context.Context) error {
	v.Faults.calls.Add(1)
	if v.down.Load() {
		v.Faults.injects.Add(1)
		return errRedisUnavailable
	}
	if v.Latency > 0 {
		timer := time.NewTimer(v.Latency)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

// IncrementAndCount implements ports.VelocityCounter.
func (v *FaultyVelocity) IncrementAndCount(ctx context.Context, key string, window time.Duration) (int64, error) {
	if err := v.guard(ctx); err != nil {
		return 0, err
	}
	return v.Inner.IncrementAndCount(ctx, key, window)
}

// Count implements ports.VelocityCounter.
func (v *FaultyVelocity) Count(ctx context.Context, key string, window time.Duration) (int64, error) {
	if err := v.guard(ctx); err != nil {
		return 0, err
	}
	return v.Inner.Count(ctx, key, window)
}

// SumAndAdd implements ports.VelocityCounter.
func (v *FaultyVelocity) SumAndAdd(ctx context.Context, key string, window time.Duration, add money.Money) (money.Money, error) {
	if err := v.guard(ctx); err != nil {
		return add, err
	}
	return v.Inner.SumAndAdd(ctx, key, window, add)
}

// --- faults over the broker ----------------------------------------------------------------------

// FaultyPublisher decorates the event publisher the outbox relay drives.
//
// A publisher that is down must not lose anything: the relay's contract is that a failed publish
// leaves the row claimable, so the outbox absorbs the outage and drains afterwards (C-8, FS-5).
type FaultyPublisher struct {
	Faults Counter

	mu        sync.Mutex
	down      bool
	published []ports.OutboxMessage
}

// NewFaultyPublisher returns a publisher that records what it accepted.
func NewFaultyPublisher() *FaultyPublisher { return &FaultyPublisher{} }

// Down stops the broker.
func (p *FaultyPublisher) Down() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = true
}

// Up restarts it.
func (p *FaultyPublisher) Up() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = false
}

// Publish implements ports.EventPublisher.
func (p *FaultyPublisher) Publish(_ context.Context, msgs ...ports.OutboxMessage) error {
	p.Faults.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		p.Faults.injects.Add(1)
		return errBrokerUnavailable
	}
	p.published = append(p.published, msgs...)
	return nil
}

// Close implements ports.EventPublisher.
func (p *FaultyPublisher) Close() error { return nil }

// Published returns everything the broker accepted, in order.
func (p *FaultyPublisher) Published() []ports.OutboxMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ports.OutboxMessage(nil), p.published...)
}

// IsUnavailable reports whether err is one of this package's injected outages, so a scenario can
// distinguish "the fault fired" from "something else went wrong" without matching on message text.
func IsUnavailable(err error) bool {
	for _, sentinel := range []error{
		errGatewayUnavailable, errGatewayTimeout, errPartitioned,
		errDatabaseUnavailable, errPoolExhausted, errRedisUnavailable, errBrokerUnavailable,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// OnCommit installs a hook that runs after every successful commit, or clears it when fn is nil.
//
// It is on the unit of work rather than on the store because a commit is the only moment at which
// a state becomes observable to anything outside the transaction that produced it — which makes it
// the only moment at which an invariant can meaningfully be said to hold or not.
func (u *FaultyUnitOfWork) OnCommit(fn func()) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.onCommit = fn
}
