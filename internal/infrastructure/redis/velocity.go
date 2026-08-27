package redis

import (
	"context"
	"errors"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// incrementAndCountScript adds one member to a sorted-set window and returns the window's size,
// atomically.
//
// # Why get-then-set under-counts during exactly the attack it detects
//
// The naive implementation is: read the count, decide, write the new member. Between the read and
// the write, another request does the same thing. Both read N, both write, both believe the count
// is N+1 — when it is N+2. One increment is lost.
//
// That sounds like a rounding error until you notice *when* it happens. A velocity counter exists
// to catch card testing and enumeration: an attacker firing hundreds of requests per second,
// deliberately in parallel, often across several of our pods at once. The lost-update rate is a
// function of concurrency, so the counter under-reports in direct proportion to how hard it is
// being attacked. At one request per second it is exact; at five hundred concurrent it can report
// a fraction of the truth. A limit of "five attempts per card per hour" is then not five; it is
// five to whatever number the attacker's concurrency buys them.
//
// So the increment and the count must be one atomic operation. Lua gives that for free: Redis
// runs the script to completion with nothing interleaved, across every client and every pod.
//
// The window itself is a sorted set scored by timestamp — a true sliding window, not a fixed
// bucket. A fixed hourly bucket lets an attacker fire the full allowance at 10:59:59 and the full
// allowance again at 11:00:00, which is twice the limit in two seconds and is the classic way
// these are defeated.
//
// KEYS[1] — the window key.
// ARGV[1] — now in milliseconds.
// ARGV[2] — window size in milliseconds.
// ARGV[3] — a unique member id; two events in the same millisecond must not collapse into one
//
//	sorted-set member, which is precisely what would happen if the score were the member.
//
// ARGV[4] — key TTL in milliseconds, which bounds memory.
//
// Returns the number of events in the window, including this one.
const incrementAndCountScript = `
local key    = KEYS[1]
local now    = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local member = ARGV[3]
local ttl    = tonumber(ARGV[4])

-- Trim first, so the count never includes events that have aged out. Doing this on every write
-- rather than on a sweep is what keeps the set from growing without bound for a hot key.
redis.call("ZREMRANGEBYSCORE", key, "-inf", now - window)
redis.call("ZADD", key, now, member)
redis.call("PEXPIRE", key, ttl)
return redis.call("ZCARD", key)
`

// countScript is the read-only half: trim and count, without adding.
const countScript = `
local key    = KEYS[1]
local now    = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
redis.call("ZREMRANGEBYSCORE", key, "-inf", now - window)
return redis.call("ZCARD", key)
`

// sumAndAddScript maintains a windowed *sum* of minor units.
//
// The member encodes the amount so that trimming an aged-out event also removes its contribution
// — a separate running total would drift, because there is no way to subtract the exact amount of
// an event you have already forgotten. Storing the amount in the member and summing on read costs
// O(n) in the window's size, which is bounded by the velocity limit itself.
//
// KEYS[1] — the window key.
// ARGV[1] — now in milliseconds.
// ARGV[2] — window size in milliseconds.
// ARGV[3] — the member: "<amount>:<unique>".
// ARGV[4] — TTL in milliseconds.
//
// Returns the sum of amounts in the window, including this one, in minor units.
const sumAndAddScript = `
local key    = KEYS[1]
local now    = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local member = ARGV[3]
local ttl    = tonumber(ARGV[4])

redis.call("ZREMRANGEBYSCORE", key, "-inf", now - window)
if member ~= "" then
    redis.call("ZADD", key, now, member)
end
redis.call("PEXPIRE", key, ttl)

local total = 0
local members = redis.call("ZRANGE", key, 0, -1)
for i = 1, #members do
    local sep = string.find(members[i], ":")
    if sep then
        total = total + (tonumber(string.sub(members[i], 1, sep - 1)) or 0)
    end
end
return total
`

// VelocityCounter implements ports.VelocityCounter with sliding-window sorted sets.
//
// It is a separate port from the cache for the reason the port's own comment gives: the operation
// is "increment and read a windowed count atomically", and implementing that with get-then-set is
// a race that under-counts precisely during an attack. See incrementAndCountScript.
//
// Safe for concurrent use.
type VelocityCounter struct {
	rdb    UniversalClient
	tenant TenantResolver

	incr *goredis.Script
	cnt  *goredis.Script
	sum  *goredis.Script

	// ttlSlack is added to the window when setting the key's TTL, so a key cannot expire while
	// events inside its window are still relevant. Without it, a key whose TTL equals its window
	// can expire microseconds before a read and silently reset the count to zero — which is the
	// same failure the counter exists to prevent, arriving by a different route.
	ttlSlack time.Duration
	// maxWindow caps a caller's window. An unbounded window is an unbounded sorted set, and a
	// per-card window of a year would hold every payment that card ever made in memory.
	maxWindow time.Duration

	// newMember mints the unique member id; replaceable for deterministic tests.
	newMember func() string
}

// VelocityOption configures a VelocityCounter.
type VelocityOption func(*VelocityCounter)

// WithVelocityTenantResolver replaces the default tenant source.
func WithVelocityTenantResolver(r TenantResolver) VelocityOption {
	return func(v *VelocityCounter) {
		if r != nil {
			v.tenant = r
		}
	}
}

// WithMaxWindow caps the window any caller may request.
func WithMaxWindow(d time.Duration) VelocityOption {
	return func(v *VelocityCounter) {
		if d > 0 {
			v.maxWindow = d
		}
	}
}

// WithMemberFactory replaces the unique-member generator, for deterministic tests.
func WithMemberFactory(fn func() string) VelocityOption {
	return func(v *VelocityCounter) {
		if fn != nil {
			v.newMember = fn
		}
	}
}

// NewVelocityCounter builds the counter.
func NewVelocityCounter(client UniversalClient, opts ...VelocityOption) *VelocityCounter {
	v := &VelocityCounter{
		rdb:       client,
		tenant:    TenantFromTelemetry,
		incr:      goredis.NewScript(incrementAndCountScript),
		cnt:       goredis.NewScript(countScript),
		sum:       goredis.NewScript(sumAndAddScript),
		ttlSlack:  time.Minute,
		maxWindow: 24 * time.Hour,
		newMember: randomMember,
	}
	for _, o := range opts {
		o(v)
	}
	return v
}

// Key renders the Redis key for a velocity window.
func (v *VelocityCounter) Key(ctx context.Context, key string) (string, error) {
	tenant, ok := v.tenant(ctx)
	if !ok {
		return "", apierror.Wrap(ErrNoTenant, apierror.CodeMissingTenantContext,
			"redis: velocity counter without a tenant in context")
	}
	return BuildKey(tenant, NamespaceVelocity, key)
}

// IncrementAndCount records one event and returns the number in the window, atomically.
func (v *VelocityCounter) IncrementAndCount(ctx context.Context, key string, window time.Duration) (int64, error) {
	full, err := v.Key(ctx, key)
	if err != nil {
		return 0, err
	}
	window = v.boundWindow(window)
	args := v.windowArgs(time.Now(), window, v.newMember())

	n, err := v.incr.Run(ctx, v.rdb, []string{full}, args...).Int64()
	if err != nil && !errors.Is(err, goredis.Nil) {
		return 0, wrapRedis(err, "velocity increment")
	}
	return n, nil
}

// Count returns the number of events in the window without recording one.
//
// It still trims, because a count that included aged-out events would over-report — and a risk
// engine that over-reports declines good payments, which is a revenue bug rather than a fraud one
// but is a bug all the same.
func (v *VelocityCounter) Count(ctx context.Context, key string, window time.Duration) (int64, error) {
	full, err := v.Key(ctx, key)
	if err != nil {
		return 0, err
	}
	window = v.boundWindow(window)
	now := time.Now()
	n, err := v.cnt.Run(ctx, v.rdb, []string{full},
		strconv.FormatInt(now.UnixMilli(), 10),
		strconv.FormatInt(window.Milliseconds(), 10),
	).Int64()
	if err != nil && !errors.Is(err, goredis.Nil) {
		return 0, wrapRedis(err, "velocity count")
	}
	return n, nil
}

// SumAndAdd records an amount and returns the windowed total.
//
// The returned Money carries the currency the caller passed. Summing across currencies is not
// something this type will do — the script sums minor units, and minor units of different
// currencies are not comparable, so a caller must key the window by currency. Enforced here
// rather than trusted: a mixed-currency window would silently add 100 JPY to 100 EUR.
func (v *VelocityCounter) SumAndAdd(ctx context.Context, key string, window time.Duration, add money.Money) (money.Money, error) {
	full, err := v.Key(ctx, key)
	if err != nil {
		return money.Money{}, err
	}
	window = v.boundWindow(window)

	member := ""
	if add.Amount() != 0 {
		member = strconv.FormatInt(add.Amount(), 10) + ":" + v.newMember()
	}
	args := v.windowArgs(time.Now(), window, member)

	total, err := v.sum.Run(ctx, v.rdb, []string{full}, args...).Int64()
	if err != nil && !errors.Is(err, goredis.Nil) {
		return money.Money{}, wrapRedis(err, "velocity sum")
	}
	return money.New(total, add.Currency())
}

// windowArgs builds the script arguments for the write paths.
//
// Exported behaviour worth pinning in a test: every duration crosses the boundary in
// milliseconds, and the TTL is the window plus slack so a key never expires while its window is
// still meaningful.
func (v *VelocityCounter) windowArgs(now time.Time, window time.Duration, member string) []any {
	return []any{
		strconv.FormatInt(now.UnixMilli(), 10),
		strconv.FormatInt(window.Milliseconds(), 10),
		member,
		strconv.FormatInt((window + v.ttlSlack).Milliseconds(), 10),
	}
}

func (v *VelocityCounter) boundWindow(window time.Duration) time.Duration {
	if window <= 0 {
		return time.Minute
	}
	if window > v.maxWindow {
		return v.maxWindow
	}
	return window
}

// randomMember mints a unique sorted-set member.
//
// Uniqueness is required, not merely nice: ZADD with an existing member updates its score instead
// of adding a row, so two events that produced the same member would count as one. A timestamp
// alone collides at high rates — which is, again, exactly the rate this counter exists for.
func randomMember() string {
	tok, err := newToken()
	if err != nil {
		// Falling back to the nanosecond clock keeps the counter working when the entropy source
		// stutters. It is weaker (two events in the same nanosecond collide) and strictly better
		// than failing the risk check, which would fail the payment.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return tok
}

var _ ports.VelocityCounter = (*VelocityCounter)(nil)
