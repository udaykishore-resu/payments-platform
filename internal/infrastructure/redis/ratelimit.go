package redis

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// tokenBucketScript is the whole limiter: read, refill, decide, write, in one atomic step.
//
// # Why this is Lua and not four commands
//
// A token bucket is read-modify-write. Done from the client it is GET tokens, GET timestamp,
// compute the refill, SET both — and between the read and the write, another pod does the same
// thing. Both see the same starting tokens, both decrement from it, and the bucket has issued two
// tokens where it had one. Under load — which is exactly when a rate limiter matters — that error
// compounds, and a limiter that leaks worst under load is a limiter that does not limit.
//
// WATCH/MULTI would be correct but costs a round trip plus a retry loop on every request, on the
// hot path, to serialise against a conflict that Lua avoids by construction: Redis runs a script
// to completion with nothing interleaved.
//
// KEYS[1] — the bucket key.
// ARGV[1] — rate, tokens per second (may be fractional).
// ARGV[2] — burst, the bucket depth.
// ARGV[3] — now, in milliseconds. Passed in rather than read from the server, so the whole
//
//	platform shares one clock source per request and a test can be deterministic. TIME
//	inside a script is also non-deterministic, which historically made scripts
//	non-replicable to replicas.
//
// ARGV[4] — cost, tokens this request consumes.
// ARGV[5] — TTL in milliseconds. Bounds memory: an idle key evaporates instead of living forever,
//
//	and it is derived from the refill time so a key can never expire while it still holds
//	state a caller would miss.
//
// Returns { allowed, remaining_whole_tokens, reset_ms, retry_after_ms }.
const tokenBucketScript = `
local key    = KEYS[1]
local rate   = tonumber(ARGV[1])
local burst  = tonumber(ARGV[2])
local now    = tonumber(ARGV[3])
local cost   = tonumber(ARGV[4])
local ttl    = tonumber(ARGV[5])

local state  = redis.call("HMGET", key, "t", "ts")
local tokens = tonumber(state[1])
local ts     = tonumber(state[2])

if tokens == nil or ts == nil then
    tokens = burst
    ts = now
end

-- Refill for the elapsed time, capped at the burst.
local elapsed = math.max(0, now - ts)
tokens = math.min(burst, tokens + (elapsed / 1000.0) * rate)
ts = now

local allowed = 0
if tokens >= cost then
    allowed = 1
    tokens = tokens - cost
end

redis.call("HSET", key, "t", tokens, "ts", ts)
redis.call("PEXPIRE", key, ttl)

-- reset: time until the bucket is full again. retry_after: time until one token exists.
local reset_ms = 0
local retry_ms = 0
if rate > 0 then
    reset_ms = math.ceil(((burst - tokens) / rate) * 1000)
    if allowed == 0 then
        retry_ms = math.ceil(((cost - tokens) / rate) * 1000)
    end
else
    reset_ms = 3600000
    if allowed == 0 then
        retry_ms = 3600000
    end
end

return { allowed, math.floor(tokens), reset_ms, retry_ms }
`

// RateLimiter is the Redis backend for resilience.DistributedLimiter. It implements
// resilience.Backend.
//
// It is a *backend*, not a limiter: the resilience package owns the policy, the local fallback
// and the decision to use one. That split is deliberate — a limiter that imports a Redis client
// is a limiter nobody can unit-test, and a resilience primitive that cannot be unit-tested is not
// one.
//
// Safe for concurrent use.
type RateLimiter struct {
	rdb    UniversalClient
	tenant TenantResolver
	script *goredis.Script

	// minTTL floors the key TTL. A bucket key that expires between two requests of the same
	// caller resets their allowance to full, which turns a rate limit into a suggestion.
	minTTL time.Duration
}

// RateLimiterOption configures a RateLimiter.
type RateLimiterOption func(*RateLimiter)

// WithRateLimitTenantResolver replaces the default tenant source.
func WithRateLimitTenantResolver(r TenantResolver) RateLimiterOption {
	return func(l *RateLimiter) {
		if r != nil {
			l.tenant = r
		}
	}
}

// NewRateLimiter builds the backend.
func NewRateLimiter(client UniversalClient, opts ...RateLimiterOption) *RateLimiter {
	l := &RateLimiter{
		rdb:    client,
		tenant: TenantFromTelemetry,
		script: goredis.NewScript(tokenBucketScript),
		minTTL: time.Minute,
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Key renders the Redis key for a limiter key.
func (l *RateLimiter) Key(ctx context.Context, key string) (string, error) {
	tenant, ok := l.tenant(ctx)
	if !ok {
		return "", apierror.Wrap(ErrNoTenant, apierror.CodeMissingTenantContext,
			"redis: rate limit without a tenant in context")
	}
	return BuildKey(tenant, NamespaceRateLimit, key)
}

// Allow implements resilience.Backend.
//
// The contract there is explicit and worth restating, because getting it wrong is subtle: a
// non-nil error means *infrastructure failure*, never "denied". Returning an error for a denial
// would make every rejection look like a Redis outage and trip the caller's local fallback, which
// would make the fallback the normal path — and the fallback's limits are per-pod, so the
// effective global limit would silently multiply by the pod count.
func (l *RateLimiter) Allow(ctx context.Context, key string, limit resilience.Limit) (resilience.Decision, error) {
	full, err := l.Key(ctx, key)
	if err != nil {
		return resilience.Decision{}, err
	}
	args := ScriptArgs(limit, time.Now(), 1, l.minTTL)

	raw, err := l.script.Run(ctx, l.rdb, []string{full}, args...).Slice()
	if err != nil && !errors.Is(err, goredis.Nil) {
		return resilience.Decision{}, wrapRedis(err, "rate limit")
	}
	return ParseDecision(raw, limit)
}

// ScriptArgs builds the token-bucket script's arguments.
//
// It is exported and pure so that the argument construction — the part with the unit conversions,
// which is where these bugs live — is testable without a Redis. Passing seconds where the script
// expects milliseconds produces a limiter that is a thousand times too permissive and looks
// completely normal in a log.
func ScriptArgs(limit resilience.Limit, now time.Time, cost int, minTTL time.Duration) []any {
	rate := limit.Rate
	if rate < 0 {
		rate = 0
	}
	burst := limit.Burst
	if burst <= 0 {
		// Mirrors resilience.Limit.normalized(): a zero burst means the documented multiple of
		// the rate, and at minimum one, so a bucket is never a closed door by accident.
		burst = int(math.Ceil(rate * resilience.DefaultBurstMultiplier))
	}
	if burst < 1 {
		burst = 1
	}
	if cost < 1 {
		cost = 1
	}

	// The TTL must outlive a full refill, or an idle key expires with tokens still owed and the
	// caller silently gets a fresh bucket. Two full refills plus the floor is comfortable and
	// still bounds memory to (active callers x window).
	ttl := minTTL
	if rate > 0 {
		refill := time.Duration(float64(burst) / rate * float64(time.Second))
		if candidate := 2 * refill; candidate > ttl {
			ttl = candidate
		}
	}

	return []any{
		strconv.FormatFloat(rate, 'f', -1, 64),
		strconv.Itoa(burst),
		strconv.FormatInt(now.UnixMilli(), 10),
		strconv.Itoa(cost),
		strconv.FormatInt(ttl.Milliseconds(), 10),
	}
}

// ParseDecision converts the script's reply into a resilience.Decision.
//
// It fills every field — Limit, Remaining, ResetAfter, RetryAfter — because they are rendered
// straight into the RateLimit-* response headers, and a client that receives a limit with no
// reset has been told to back off for an unknown period, which it will interpret as "immediately".
func ParseDecision(raw []any, limit resilience.Limit) (resilience.Decision, error) {
	if len(raw) < 4 {
		return resilience.Decision{}, apierror.Newf(apierror.CodeDependencyFailure,
			"redis: rate limit script returned %d values, want 4", len(raw))
	}
	allowed, err := toInt64(raw[0])
	if err != nil {
		return resilience.Decision{}, err
	}
	remaining, err := toInt64(raw[1])
	if err != nil {
		return resilience.Decision{}, err
	}
	resetMs, err := toInt64(raw[2])
	if err != nil {
		return resilience.Decision{}, err
	}
	retryMs, err := toInt64(raw[3])
	if err != nil {
		return resilience.Decision{}, err
	}

	burst := limit.Burst
	if burst <= 0 {
		burst = int(math.Ceil(limit.Rate * resilience.DefaultBurstMultiplier))
	}
	if burst < 1 {
		burst = 1
	}
	if remaining < 0 {
		remaining = 0
	}

	return resilience.Decision{
		Allowed:    allowed == 1,
		Limit:      burst,
		Remaining:  int(remaining),
		ResetAfter: time.Duration(resetMs) * time.Millisecond,
		RetryAfter: time.Duration(retryMs) * time.Millisecond,
	}, nil
}

// toInt64 accepts the shapes Redis returns for an integer in a script reply.
func toInt64(v any) (int64, error) {
	switch t := v.(type) {
	case int64:
		return t, nil
	case int:
		return int64(t), nil
	case float64:
		return int64(t), nil
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			return 0, apierror.Wrapf(err, apierror.CodeDependencyFailure,
				"redis: script returned %q where an integer was expected", t)
		}
		return n, nil
	default:
		return 0, apierror.Newf(apierror.CodeDependencyFailure,
			"redis: script returned %T where an integer was expected", v)
	}
}

var _ resilience.Backend = (*RateLimiter)(nil)
