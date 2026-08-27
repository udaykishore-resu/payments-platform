package resilience

import (
	"context"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Rate-limiter parameters from docs/failure-handling.md §2.6.
const (
	// DefaultBurstMultiplier is 2: burst = 2 × rate. Two seconds' worth of allowance absorbs a
	// well-behaved client's natural clumping — an SDK flushing a batch, a page of webhooks
	// retried together — without permitting a sustained overrun, because the bucket still only
	// refills at `rate`.
	DefaultBurstMultiplier = 2

	// LocalFallbackMultiplier is 1.2. See DistributedLimiter for the arithmetic; the short
	// version is that it is a deliberate, bounded 20 % over-admission chosen over rejecting
	// valid traffic during a Redis outage.
	LocalFallbackMultiplier = 1.2
)

// Limit is one rate-limit configuration: a sustained rate in requests per second and a burst.
//
// It is a value, passed on every call, rather than state held by the limiter, because the
// authoritative limit lives in the tenant's configuration document (baseline §23) and changes
// under the limiter's feet on every config publish. A limiter that cached it would enforce
// yesterday's contract.
type Limit struct {
	// Rate is the sustained permitted requests per second.
	Rate float64
	// Burst is the bucket depth. Zero means DefaultBurstMultiplier × Rate.
	Burst int
}

// NewLimit returns a Limit at the given rate with the documented 2× burst.
func NewLimit(rate float64) Limit {
	return Limit{Rate: rate, Burst: int(math.Ceil(rate * DefaultBurstMultiplier))}
}

func (l Limit) normalized() Limit {
	if l.Rate < 0 {
		l.Rate = 0
	}
	if l.Burst <= 0 {
		l.Burst = int(math.Ceil(l.Rate * DefaultBurstMultiplier))
	}
	if l.Burst < 1 {
		l.Burst = 1
	}
	return l
}

// scaled returns the limit divided across replicas and multiplied by m, for local fallback.
func (l Limit) scaled(replicas int, m float64) Limit {
	l = l.normalized()
	if replicas < 1 {
		replicas = 1
	}
	out := Limit{
		Rate:  l.Rate / float64(replicas) * m,
		Burst: int(math.Ceil(float64(l.Burst) / float64(replicas) * m)),
	}
	if out.Burst < 1 {
		out.Burst = 1
	}
	return out
}

// Decision is the result of one rate-limit check. Its fields map one-to-one onto the response
// headers of baseline §19.3, which is deliberate: a decision type that does not carry
// Remaining and Reset forces the transport layer to recompute them from the limiter's internals,
// and the two computations drift.
type Decision struct {
	// Allowed is whether the request may proceed.
	Allowed bool
	// Limit is the configured burst — the value of the RateLimit-Limit header.
	Limit int
	// Remaining is whole tokens left after this decision — RateLimit-Remaining.
	Remaining int
	// ResetAfter is how long until the bucket is full again — RateLimit-Reset, in seconds.
	ResetAfter time.Duration
	// RetryAfter is how long until one token is available. Zero when Allowed. Sent as
	// Retry-After on a 429.
	RetryAfter time.Duration
}

// Headers renders the decision as the RateLimit-* response headers. Retry-After is present only
// on a rejection, because a Retry-After on a successful response tells a client to slow down
// when it has not been asked to.
//
// Both header values are in whole seconds and are rounded *up*: rounding down tells a client to
// retry before the token exists, which produces a second 429 and a second retry, which is the
// storm the limiter is there to prevent.
func (d Decision) Headers() map[string]string {
	h := map[string]string{
		"RateLimit-Limit":     strconv.Itoa(d.Limit),
		"RateLimit-Remaining": strconv.Itoa(d.Remaining),
		"RateLimit-Reset":     strconv.Itoa(ceilSeconds(d.ResetAfter)),
	}
	if !d.Allowed {
		h["Retry-After"] = strconv.Itoa(ceilSeconds(d.RetryAfter))
	}
	return h
}

// Err returns the platform error for a rejected decision, carrying the retry guidance, or nil
// when the decision allowed the request.
func (d Decision) Err() error {
	if d.Allowed {
		return nil
	}
	return apierror.New(apierror.CodeRateLimited, "rate limit exceeded").
		WithRetryAfter(ceilSeconds(d.RetryAfter))
}

func ceilSeconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	return int(math.Ceil(d.Seconds()))
}

// TokenBucket is a local token bucket.
//
// Token bucket, not a fixed window, and the reason is arithmetic: a fixed window permits 2× the
// limit across a boundary. A client with a 100 rps limit sends 100 requests in the last
// millisecond of one window and 100 in the first millisecond of the next — 200 requests in 2 ms,
// all of them "within the limit". A token bucket has no boundary to exploit; its worst case is
// the burst, which is a number we chose.
//
// Safe for concurrent use.
type TokenBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	clock  Clock
}

// NewTokenBucket returns a bucket starting full. Starting full is correct for a rate limiter —
// a client's first request must not be rejected because the process just booted — and is the
// opposite of the choice a *retry* budget would make.
func NewTokenBucket(limit Limit, clk Clock) *TokenBucket {
	limit = limit.normalized()
	clk = orSystem(clk)
	return &TokenBucket{
		rate:   limit.Rate,
		burst:  float64(limit.Burst),
		tokens: float64(limit.Burst),
		last:   clk.Now(),
		clock:  clk,
	}
}

// SetLimit updates the rate and burst in place, preserving the current balance (clamped to the
// new burst). Used when a configuration publish changes a tenant's contracted rate: rebuilding
// the bucket would hand the tenant a full burst on every config change, which is a free
// allowance for anyone who can trigger one.
func (t *TokenBucket) SetLimit(limit Limit) {
	limit = limit.normalized()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.refillLocked()
	t.rate = limit.Rate
	t.burst = float64(limit.Burst)
	t.tokens = min(t.tokens, t.burst)
}

// Allow takes one token, reporting whether it was available.
func (t *TokenBucket) Allow() bool { return t.Take().Allowed }

// Take takes one token and returns the full Decision, including the header values.
func (t *TokenBucket) Take() Decision {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.refillLocked()

	d := Decision{Limit: int(t.burst)}
	if t.tokens >= 1 {
		t.tokens--
		d.Allowed = true
	} else {
		// Time until one whole token exists. With rate 0 the bucket never refills, so the
		// honest answer is the reset horizon rather than an infinity or a divide by zero.
		d.RetryAfter = t.durationFor(1 - t.tokens)
	}
	d.Remaining = int(t.tokens)
	d.ResetAfter = t.durationFor(t.burst - t.tokens)
	return d
}

// Tokens returns the current balance, refilled to now. For metrics and tests.
func (t *TokenBucket) Tokens() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.refillLocked()
	return t.tokens
}

func (t *TokenBucket) durationFor(tokens float64) time.Duration {
	if tokens <= 0 {
		return 0
	}
	if t.rate <= 0 {
		// A zero-rate bucket is a closed door. Report an hour rather than infinity so the
		// header renders as an integer and the client backs off rather than hot-looping.
		return time.Hour
	}
	return time.Duration(tokens / t.rate * float64(time.Second))
}

func (t *TokenBucket) refillLocked() {
	now := t.clock.Now()
	elapsed := now.Sub(t.last)
	if elapsed <= 0 {
		return
	}
	t.last = now
	t.tokens = min(t.burst, t.tokens+t.rate*elapsed.Seconds())
}

// Limiter is what a request handler depends on: something that decides whether one key may
// proceed under one limit.
//
// Declared here rather than in the caller because both implementations in this package
// (TokenBucket via LocalLimiter, and DistributedLimiter) satisfy it and callers need to swap
// them at wiring time.
type Limiter interface {
	Allow(ctx context.Context, key string, limit Limit) (Decision, error)
}

// Backend is the distributed counter — in production, Redis with the atomic Lua script of
// docs/failure-handling.md §2.6 (read, refill, decrement, write in one round trip, ~1 ms).
//
// It is an interface declared *here*, and this package does not import a Redis client, because
// a resilience primitive that drags a driver dependency into every consumer is a resilience
// primitive nobody can unit-test. The Redis implementation lives in
// internal/infrastructure/redis and is injected.
//
// An implementation must return a non-nil error only for infrastructure failure. Returning an
// error to mean "denied" would make every denial trigger the local fallback, which would make
// the fallback the normal path.
type Backend interface {
	Allow(ctx context.Context, key string, limit Limit) (Decision, error)
}

// LocalLimiter is a Limiter backed purely by per-key local token buckets, with the same bounded
// key space as the registries. Useful on its own for single-pod limits, and it is the mechanism
// DistributedLimiter falls back to.
//
// Safe for concurrent use.
type LocalLimiter struct {
	reg   *keyedRegistry[*TokenBucket]
	clock Clock
	scale func(Limit) Limit
}

// NewLocalLimiter returns a local limiter. maxKeys and idleTTL bound the key space for the same
// reason the registries do: the key is (tenant, merchant, route_class) and a caller who can
// invent merchant identifiers can otherwise invent buckets until the pod dies.
func NewLocalLimiter(maxKeys int, idleTTL time.Duration, clk Clock) *LocalLimiter {
	clk = orSystem(clk)
	return &LocalLimiter{
		reg:   newKeyedRegistry[*TokenBucket](maxKeys, idleTTL, clk, nil),
		clock: clk,
		scale: func(l Limit) Limit { return l },
	}
}

// Allow implements Limiter. The error is always nil; the signature matches Limiter so a local
// limiter can be substituted for a distributed one without a wrapper.
func (l *LocalLimiter) Allow(_ context.Context, key string, limit Limit) (Decision, error) {
	effective := l.scale(limit).normalized()
	b := l.reg.getOrCreate(key, func(string) *TokenBucket { return NewTokenBucket(effective, l.clock) })
	b.SetLimit(effective)
	return b.Take(), nil
}

// Forget discards the bucket for key. DistributedLimiter uses it to drop local state the moment
// the backend is authoritative again.
func (l *LocalLimiter) Forget(key string) { l.reg.remove(key) }

// Len returns the number of live local buckets.
func (l *LocalLimiter) Len() int { return l.reg.len() }

// DistributedLimiterConfig parameterizes DistributedLimiter.
type DistributedLimiterConfig struct {
	// Backend is the distributed counter. Required.
	Backend Backend

	// Replicas is the number of pods sharing the global limit, used to size the local fallback
	// bucket. It comes from the deployment (the Deployment's replica count, surfaced through
	// the downward API), not from a guess: a wrong replica count scales the fallback wrongly in
	// exactly the direction of the error.
	Replicas int

	// FallbackMultiplier defaults to LocalFallbackMultiplier (1.2).
	FallbackMultiplier float64

	// MaxLocalKeys and LocalIdleTTL bound the fallback bucket map.
	MaxLocalKeys int
	LocalIdleTTL time.Duration

	Clock Clock

	// OnFallback is called whenever a backend error forces a local decision, so the caller can
	// increment pp_rate_limit_fallback_total and alert. A fallback nobody can see is a silent
	// change in the enforced limit.
	OnFallback func(key string, err error)
}

// DistributedLimiter enforces a global limit through a Backend, falling back to a local token
// bucket when the backend fails.
//
// **Why the ×1.2 multiplier exists.** The global limit is enforced in one place — Redis — so
// each of the N replicas sees the whole tenant's traffic accounted centrally. When Redis is
// gone, every replica must decide alone, and the only thing it can enforce is its own share:
// `global_limit / replicas`. If every replica enforced exactly that share, the arithmetic would
// be perfect *only if load balancing were perfect*. It is not. A tenant's traffic arrives over
// keep-alive connections that pin a client to a pod for minutes at a time; an AZ failure
// redistributes traffic unevenly; a deploy leaves one replica warm and another cold. In
// practice a replica routinely sees 15–20 % more than its nominal share, and a replica
// enforcing exactly `limit/N` would reject perfectly valid traffic from a tenant who is,
// globally, well under their contracted rate — during an incident, which is when a merchant
// least wants to discover that their payments are being 429'd because *our* cache is down.
//
// The multiplier is the explicit trade. With N replicas each admitting `limit/N × 1.2`, the
// worst case aggregate is `limit × 1.2` — a bounded 20 % over-admission — and the realistic
// case, with uneven balancing, is close to the intended limit. Rejecting valid traffic is a
// merchant-visible correctness-adjacent failure; admitting 20 % extra for the duration of a
// Redis outage is a capacity question, and the platform is provisioned with 3× headroom
// (baseline §18). The number is deliberately small: 2× would be a decision to abandon the
// limit, and 1.0 would be a decision to punish tenants for our outage. It is documented as a
// merchant-visible effect in F-7 ("rate limits slightly coarser") rather than hidden.
//
// **Exit from fallback is immediate, not gradual.** The first successful backend call discards
// the local bucket for that key. A gradual handover would mean a window in which a request is
// counted locally *and* in Redis, or in neither — split-brain accounting whose error is
// unbounded and undebuggable. A discontinuity is worse to look at on a graph and better to
// reason about.
//
// Safe for concurrent use. Owns no goroutine.
type DistributedLimiter struct {
	backend    Backend
	local      *LocalLimiter
	replicas   int
	multiplier float64
	onFallback func(string, error)

	fallbacks atomic.Uint64
}

var _ Limiter = (*DistributedLimiter)(nil)

// NewDistributedLimiter returns a limiter that prefers cfg.Backend and falls back locally.
func NewDistributedLimiter(cfg DistributedLimiterConfig) *DistributedLimiter {
	if cfg.Replicas < 1 {
		cfg.Replicas = 1
	}
	if cfg.FallbackMultiplier <= 0 {
		cfg.FallbackMultiplier = LocalFallbackMultiplier
	}
	clk := orSystem(cfg.Clock)
	l := &DistributedLimiter{
		backend:    cfg.Backend,
		local:      NewLocalLimiter(cfg.MaxLocalKeys, cfg.LocalIdleTTL, clk),
		replicas:   cfg.Replicas,
		multiplier: cfg.FallbackMultiplier,
		onFallback: cfg.OnFallback,
	}
	replicas, mult := cfg.Replicas, cfg.FallbackMultiplier
	l.local.scale = func(lim Limit) Limit { return lim.scaled(replicas, mult) }
	return l
}

// Allow returns the backend's decision, or a local one when the backend fails.
//
// It never returns the backend's error to the caller. A rate limiter that fails the request
// because its *counter* is unavailable has converted a latency accelerator into a hard
// dependency, which is precisely the coupling F-7 says must not exist: Redis loss costs
// latency and precision, never correctness and never availability.
func (l *DistributedLimiter) Allow(ctx context.Context, key string, limit Limit) (Decision, error) {
	if l.backend != nil {
		d, err := l.backend.Allow(ctx, key, limit)
		if err == nil {
			// Authoritative again: drop the local bucket at once rather than blending.
			l.local.Forget(key)
			return d, nil
		}
		l.fallbacks.Add(1)
		if l.onFallback != nil {
			l.onFallback(key, err)
		}
	}
	return l.local.Allow(ctx, key, limit)
}

// Fallbacks returns how many decisions were made locally because the backend failed. This backs
// pp_rate_limit_fallback_total, and F-7's manual step "verify the fallback is off before
// declaring recovery" is this counter going flat.
func (l *DistributedLimiter) Fallbacks() uint64 { return l.fallbacks.Load() }

// LocalKeys returns the number of live fallback buckets. It should be zero in steady state.
func (l *DistributedLimiter) LocalKeys() int { return l.local.Len() }

// FallbackLimit returns the effective per-replica limit used when the backend is unavailable,
// so an operator can see the number the pod is actually enforcing rather than deriving it.
func (l *DistributedLimiter) FallbackLimit(limit Limit) Limit {
	return limit.scaled(l.replicas, l.multiplier)
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: NFR-36.
//
// Rate limiting and abuse resistance, local and distributed
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
