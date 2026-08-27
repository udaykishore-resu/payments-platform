package resilience

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Bulkhead sizes from docs/failure-handling.md §2.5. Each is Little's Law — L = λ × W — applied
// to a measured arrival rate and a p99 service time, then divided by the replica count.
const (
	// GatewayBulkheadPerPod is 200. Derivation:
	//
	//	λ = 5 000 TPS ÷ 3 gateways         ≈ 1 667 requests/second to one gateway
	//	W = 1.5 s                            the p99 gateway service time
	//	L = λ × W = 1 667 × 1.5            ≈ 2 500 concurrent requests, fleet-wide
	//	per pod = 2 500 ÷ 12 orchestrators ≈ 208 → 200
	//
	// The point of sizing on *concurrency* rather than rate is that a rate limit does not
	// protect a resource pool at all: 500 rps of 10-second requests is 5 000 concurrent
	// connections, sockets and goroutines, and a limiter set to 500 rps will admit every one of
	// them. Only a semaphore bounds what is actually scarce.
	GatewayBulkheadPerPod = 200

	// GatewayTenantBulkhead is 32: 200 ÷ ~6 concurrently-active large tenants, rounded down.
	// It exists so one tenant's burst cannot consume the whole per-gateway allowance and
	// starve every other tenant on the pod.
	GatewayTenantBulkhead = 32

	// IngressTenantBulkheadPerPod is 64. Derivation: λ = 500 rps (the tenant's quota),
	// W(p99) = 0.25 s excluding the gateway call → L = 125 across 8 API pods ≈ 16, then ×4
	// headroom for requests that do include a gateway call.
	IngressTenantBulkheadPerPod = 64

	// RedisPoolBulkhead is 50: at a 50 ms per-operation timeout, 50 concurrent operations is
	// 1 000 ops/s per pod, which is above anything the request pipeline asks of Redis.
	RedisPoolBulkhead = 50

	// WorkflowActivityBulkhead is 16 per tenant per worker.
	WorkflowActivityBulkhead = 16

	// WebhookMerchantBulkhead is 4 and WebhookTenantBulkhead is 64: one merchant's slow
	// endpoint must not consume the delivery pool. A merchant answering in 5 s at 4 concurrent
	// deliveries costs us 4 goroutines, not the pool.
	WebhookMerchantBulkhead = 4
	WebhookTenantBulkhead   = 64
)

// BulkheadConfig parameterizes a Bulkhead.
type BulkheadConfig struct {
	// Name labels the bulkhead in metrics and errors.
	Name string

	// MaxConcurrent is the semaphore size — the L from Little's Law. Defaults to
	// GatewayBulkheadPerPod.
	MaxConcurrent int

	// MaxQueue bounds how many callers may wait for a permit. Zero means no queue: Acquire
	// either takes a permit immediately or sheds.
	//
	// docs/failure-handling.md §5.3 sets the gateway bulkhead's waiter bound to zero and says a
	// waiter is a bug worth investigating, and baseline A6 states the principle: blocking a
	// request thread on a resource held by another process is how thread pools die under retry
	// storms. The queue exists for the paths where a very short wait genuinely beats a reject —
	// an ingress admission queue bounded at 128 — and nowhere else.
	MaxQueue int

	// MaxWait bounds a queued caller's wait. Zero with a positive MaxQueue is meaningless and
	// is treated as "no queue": a wait with no bound is the unbounded queue this type exists to
	// prevent.
	MaxWait time.Duration
}

// Bulkhead is a counting semaphore that bounds concurrency, with an optional bounded wait
// queue.
//
// A full bulkhead sheds immediately with CONCURRENCY_LIMIT_EXCEEDED (429, retryable) rather
// than queueing indefinitely, because an unbounded queue converts a throughput problem into a
// latency problem, then into a timeout problem, which produces retries, which makes the
// throughput problem worse (docs/failure-handling.md §5.2). Fast, explicit rejection is the
// only backpressure that does not amplify.
//
// Safe for concurrent use. The zero value is not usable; use NewBulkhead.
type Bulkhead struct {
	cfg   BulkheadConfig
	sem   chan struct{}
	queue chan struct{}

	inFlight atomic.Int64
	queued   atomic.Int64
	rejected atomic.Uint64
	admitted atomic.Uint64
}

// NewBulkhead returns a bulkhead. Invalid sizes are corrected rather than rejected: this is
// constructed on a startup path, and a returned error there only moves the decision somewhere
// less able to make it.
func NewBulkhead(cfg BulkheadConfig) *Bulkhead {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = GatewayBulkheadPerPod
	}
	if cfg.MaxQueue < 0 {
		cfg.MaxQueue = 0
	}
	if cfg.MaxWait <= 0 {
		// A queue with no wait bound is an unbounded queue with extra bookkeeping.
		cfg.MaxQueue = 0
	}
	b := &Bulkhead{
		cfg: cfg,
		sem: make(chan struct{}, cfg.MaxConcurrent),
	}
	if cfg.MaxQueue > 0 {
		b.queue = make(chan struct{}, cfg.MaxQueue)
	}
	return b
}

// Acquire takes a permit, returning the release function that gives it back.
//
// Order of attempts:
//
//  1. Non-blocking take. The overwhelmingly common path, and the only one on a gateway
//     bulkhead, which is configured with no queue at all.
//  2. If a queue is configured, take a queue slot; if the queue is full, shed at once.
//  3. Wait for a permit until MaxWait elapses or the context is done, whichever comes first.
//
// The returned release is idempotent — it is guarded by a sync.Once — so `defer release()` is
// correct even on a path that also releases explicitly, and a panic between acquire and return
// still gives the permit back exactly once. A double release would return a permit that was
// never taken and silently raise the effective concurrency limit above the bound this whole
// type exists to enforce; that is why it is a Once and not a bare send.
//
// On rejection the error is CONCURRENCY_LIMIT_EXCEEDED (429, retryable, with Retry-After). On
// context cancellation the caller's own error is preserved.
func (b *Bulkhead) Acquire(ctx context.Context) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}

	select {
	case b.sem <- struct{}{}:
		return b.permit(), nil
	default:
	}

	if b.queue == nil {
		return nil, b.full()
	}

	select {
	case b.queue <- struct{}{}:
	default:
		return nil, b.full()
	}
	b.queued.Add(1)
	defer func() {
		b.queued.Add(-1)
		<-b.queue
	}()

	t := time.NewTimer(b.cfg.MaxWait)
	defer t.Stop()

	select {
	case b.sem <- struct{}{}:
		return b.permit(), nil
	case <-t.C:
		return nil, b.full()
	case <-ctx.Done():
		return nil, contextError(ctx.Err())
	}
}

// TryAcquire is Acquire with the queue skipped entirely: it takes a permit or sheds. This is
// the form docs/failure-handling.md §2.5 prescribes for the gateway bulkheads.
func (b *Bulkhead) TryAcquire() (release func(), err error) {
	select {
	case b.sem <- struct{}{}:
		return b.permit(), nil
	default:
		return nil, b.full()
	}
}

func (b *Bulkhead) permit() func() {
	b.inFlight.Add(1)
	b.admitted.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			b.inFlight.Add(-1)
			<-b.sem
		})
	}
}

func (b *Bulkhead) full() error {
	b.rejected.Add(1)
	// Retry-After of 1 s: the bulkhead's own service time is sub-second by construction (it is
	// sized on a p99 of 1.5 s), so a client that waits a second is very likely to find a slot,
	// and one that waits less is very likely to be rejected again.
	return apierror.Newf(apierror.CodeConcurrencyLimitExceeded,
		"bulkhead %q is full (%d in flight, %d queued)",
		b.cfg.Name, b.inFlight.Load(), b.queued.Load()).WithRetryAfter(1)
}

// InFlight returns the number of held permits.
func (b *Bulkhead) InFlight() int { return int(b.inFlight.Load()) }

// Queued returns the number of callers waiting for a permit. On a gateway bulkhead this should
// always be zero; docs/failure-handling.md §5.3 alerts on any waiter because a waiter means a
// code path is blocking where it was designed not to.
func (b *Bulkhead) Queued() int { return int(b.queued.Load()) }

// Capacity returns MaxConcurrent.
func (b *Bulkhead) Capacity() int { return b.cfg.MaxConcurrent }

// Available returns the number of free permits.
func (b *Bulkhead) Available() int { return b.cfg.MaxConcurrent - b.InFlight() }

// Rejected and Admitted expose the counters for metric export. Their ratio is the shed rate,
// which is the signal that feeds the degradation ladder.
func (b *Bulkhead) Rejected() uint64 { return b.rejected.Load() }
func (b *Bulkhead) Admitted() uint64 { return b.admitted.Load() }

// Saturation returns in-flight ÷ capacity, in [0, 1]. This is the pressure signal a Shedder
// consumes when the trigger is "gateway bulkheads saturated" (ladder rung 6).
func (b *Bulkhead) Saturation() float64 {
	return float64(b.InFlight()) / float64(b.cfg.MaxConcurrent)
}

// Name returns the bulkhead's label.
func (b *Bulkhead) Name() string { return b.cfg.Name }

// --- registry ---------------------------------------------------------------------------

// BulkheadRegistryConfig parameterizes BulkheadRegistry.
type BulkheadRegistryConfig struct {
	MaxKeys       int
	IdleTTL       time.Duration
	SweepInterval time.Duration
	Clock         Clock
	// Configure builds the config for a key on first use. Required in practice: the whole point
	// of a per-key bulkhead is that a per-gateway bound and a per-tenant bound are different
	// numbers (200 and 32).
	Configure func(key string) BulkheadConfig
	Evicted   func(key string)
}

// BulkheadRegistry holds one Bulkhead per key — per gateway, or per (gateway, tenant).
//
// Bounded and idle-evicting for the same reason BreakerRegistry is: the key derives from
// request data, so an unbounded registry is a memory-exhaustion vector aimed at the component
// that exists to prevent resource exhaustion. Eviction of a bulkhead with permits still held is
// safe — the release closures hold their own channel reference and complete normally; the
// entry simply stops being reachable by key.
//
// Safe for concurrent use.
type BulkheadRegistry struct {
	reg *keyedRegistry[*Bulkhead]
	cfg BulkheadRegistryConfig
}

// NewBulkheadRegistry returns a registry. If cfg.SweepInterval is positive the registry owns a
// goroutine and Close must be called.
func NewBulkheadRegistry(cfg BulkheadRegistryConfig) *BulkheadRegistry {
	if cfg.Configure == nil {
		cfg.Configure = func(key string) BulkheadConfig {
			return BulkheadConfig{Name: key, MaxConcurrent: GatewayBulkheadPerPod}
		}
	}
	cfg.Clock = orSystem(cfg.Clock)
	r := &BulkheadRegistry{cfg: cfg}
	var onEvict func(string, *Bulkhead)
	if cfg.Evicted != nil {
		onEvict = func(k string, _ *Bulkhead) { cfg.Evicted(k) }
	}
	r.reg = newKeyedRegistry[*Bulkhead](cfg.MaxKeys, cfg.IdleTTL, cfg.Clock, onEvict)
	r.reg.startSweeper(cfg.SweepInterval)
	return r
}

// BulkheadKey builds a two-part key, e.g. BulkheadKey(gateway, tenant).
func BulkheadKey(scope, sub string) string { return scope + ":" + sub }

// Get returns the bulkhead for key, creating it on first use.
func (r *BulkheadRegistry) Get(key string) *Bulkhead {
	cfg := r.cfg
	return r.reg.getOrCreate(key, func(k string) *Bulkhead {
		c := cfg.Configure(k)
		if c.Name == "" {
			c.Name = k
		}
		return NewBulkhead(c)
	})
}

// Len returns the number of live bulkheads.
func (r *BulkheadRegistry) Len() int { return r.reg.len() }

// EvictIdle drops bulkheads untouched for longer than IdleTTL and returns how many went.
func (r *BulkheadRegistry) EvictIdle() int { return r.reg.evictIdle() }

// Snapshot returns the live bulkheads by key, for metric export.
func (r *BulkheadRegistry) Snapshot() map[string]*Bulkhead { return r.reg.snapshot() }

// Close stops the sweeper goroutine, if one was started, and waits for it.
func (r *BulkheadRegistry) Close() error {
	r.reg.close()
	return nil
}
