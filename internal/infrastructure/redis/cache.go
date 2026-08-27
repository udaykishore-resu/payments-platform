package redis

import (
	"context"
	"errors"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// KeyPrefix is the platform namespace. Every key this package writes starts with it, so a shared
// Redis can be swept, scoped by ACL, or measured by prefix without guessing which keys are ours.
const KeyPrefix = "pp"

// Namespace separates the key spaces of the components in this package, so a cache key can never
// collide with a lock key or a velocity window.
type Namespace string

const (
	// NamespaceCache holds generic cached values, all of them regenerable from Postgres.
	NamespaceCache Namespace = "cache"
	// NamespaceLock holds distributed-lock keys. See the warning on Lock.
	NamespaceLock Namespace = "lock"
	// NamespaceRateLimit holds token buckets.
	NamespaceRateLimit Namespace = "rl"
	// NamespaceVelocity holds the risk engine's sliding windows.
	NamespaceVelocity Namespace = "vel"
	// NamespaceIdempotency holds the non-authoritative idempotency accelerator's entries.
	NamespaceIdempotency Namespace = "idem"
)

// TenantResolver produces the tenant a request belongs to.
//
// It is injected rather than hard-wired so that this package does not decide where tenancy comes
// from — the platform does, at authentication — and so that a test can supply one without
// building a request pipeline.
type TenantResolver func(ctx context.Context) (shared.TenantID, bool)

// TenantFromTelemetry is the default resolver: the tenant established at authentication and
// carried in the context's correlation fields, which is the same value every log line for this
// request already carries.
func TenantFromTelemetry(ctx context.Context) (shared.TenantID, bool) {
	id := telemetry.FieldsFromContext(ctx).TenantID
	if id == "" {
		return "", false
	}
	return shared.TenantID(id), true
}

// ErrNoTenant is returned when a key would be built without a tenant.
//
// This is a hard error, never a fallback to a global key space, and that is the single most
// important line in this file. A cache entry written without a tenant prefix is readable by every
// tenant: the first request for merchant configuration would populate a key that the next
// tenant's request for their own configuration would then read. Under RLS the database makes that
// impossible; a cache has no RLS, so the key structure has to carry the isolation, and a missing
// tenant must therefore fail loudly rather than silently degrade to a shared namespace.
var ErrNoTenant = errors.New("redis: no tenant in context")

// BuildKey renders a tenant-scoped key: pp:{ten_...}:<namespace>:<parts...>.
//
// The braces are not decoration. In Redis Cluster, `{...}` is a hash tag: only the text inside it
// is hashed to choose a slot, so every key of one tenant lands on one slot. That gives two things
// we would otherwise have to give up — multi-key operations and Lua scripts across a tenant's
// keys (the rate limiter and velocity counter both need them, and Cluster refuses a script whose
// keys span slots) — at the cost of a hot slot for a very large tenant, which is a capacity
// problem with known remedies rather than a correctness one.
func BuildKey(tenant shared.TenantID, ns Namespace, parts ...string) (string, error) {
	if tenant.IsZero() {
		return "", apierror.Wrap(ErrNoTenant, apierror.CodeMissingTenantContext,
			"redis: refusing to build an untenanted key").
			WithDetail(apierror.Detail{
				Field: "tenant", Code: "MISSING_TENANT_CONTEXT",
				Message: "a cache key without a tenant prefix is readable by every tenant",
				RuleID:  "L1.TENANT_CONTEXT_PRESENT",
			})
	}
	if ns == "" {
		return "", apierror.New(apierror.CodeInternalError, "redis: key namespace is required")
	}
	var b strings.Builder
	b.WriteString(KeyPrefix)
	b.WriteString(":{")
	b.WriteString(tenant.String())
	b.WriteString("}:")
	b.WriteString(string(ns))
	for _, p := range parts {
		if p == "" {
			return "", apierror.New(apierror.CodeInternalError, "redis: empty key component")
		}
		b.WriteString(":")
		b.WriteString(p)
	}
	return b.String(), nil
}

// Cache is the tenant-scoped cache. It implements ports.Cache.
//
// Everything it caches is derived data that Postgres can regenerate, so every failure path here
// is "behave as though the key was absent". That is what makes it safe for the cache to be down.
type Cache struct {
	rdb    UniversalClient
	tenant TenantResolver

	// group is the single-flight coordinator; see GetOrLoad.
	group singleflight.Group

	// defaultTTL bounds an entry whose caller did not choose one. A cache entry with no TTL is a
	// memory leak with a friendly name, and eviction under memory pressure is the one Redis
	// behaviour that will surprise you at the worst time.
	defaultTTL time.Duration
	// maxTTL caps every entry. Staleness tolerance is a per-caller decision, but "forever" is not
	// a tolerance.
	maxTTL time.Duration
}

// CacheOption configures a Cache.
type CacheOption func(*Cache)

// WithTenantResolver replaces the default tenant source.
func WithTenantResolver(r TenantResolver) CacheOption {
	return func(c *Cache) {
		if r != nil {
			c.tenant = r
		}
	}
}

// WithDefaultTTL sets the TTL used when a caller passes zero.
func WithDefaultTTL(d time.Duration) CacheOption {
	return func(c *Cache) {
		if d > 0 {
			c.defaultTTL = d
		}
	}
}

// WithMaxTTL caps every entry's TTL.
func WithMaxTTL(d time.Duration) CacheOption {
	return func(c *Cache) {
		if d > 0 {
			c.maxTTL = d
		}
	}
}

// NewCache builds a cache over a client.
func NewCache(client UniversalClient, opts ...CacheOption) *Cache {
	c := &Cache{
		rdb:        client,
		tenant:     TenantFromTelemetry,
		defaultTTL: 5 * time.Minute,
		maxTTL:     1 * time.Hour,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Key renders the full Redis key for a caller-supplied logical key.
func (c *Cache) Key(ctx context.Context, key string) (string, error) {
	tenant, ok := c.tenant(ctx)
	if !ok {
		return "", apierror.Wrap(ErrNoTenant, apierror.CodeMissingTenantContext,
			"redis: cache access without a tenant in context")
	}
	return BuildKey(tenant, NamespaceCache, key)
}

// Get returns a cached value.
//
// The second return distinguishes "absent" from "error", and the caller is expected to treat
// both the same way — go to the origin. They are separate returns anyway because the *metric*
// differs: a miss rate is a tuning signal, an error rate is an incident signal, and collapsing
// them means a Redis outage looks like a cold cache.
func (c *Cache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	full, err := c.Key(ctx, key)
	if err != nil {
		return nil, false, err
	}
	b, err := c.rdb.Get(ctx, full).Bytes()
	switch {
	case errors.Is(err, goredis.Nil):
		return nil, false, nil
	case err != nil:
		return nil, false, wrapRedis(err, "get")
	}
	return b, true, nil
}

// Set stores a value with a bounded TTL.
func (c *Cache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	full, err := c.Key(ctx, key)
	if err != nil {
		return err
	}
	if err := c.rdb.Set(ctx, full, value, c.boundTTL(ttl)).Err(); err != nil {
		return wrapRedis(err, "set")
	}
	return nil
}

// Delete removes a key. A delete that fails is reported, because an invalidation that silently
// did not happen is how a suspended merchant keeps being served.
func (c *Cache) Delete(ctx context.Context, key string) error {
	full, err := c.Key(ctx, key)
	if err != nil {
		return err
	}
	if err := c.rdb.Del(ctx, full).Err(); err != nil {
		return wrapRedis(err, "del")
	}
	return nil
}

// GetOrLoad returns the cached value, or loads it exactly once across concurrent callers.
//
// # The stampede this prevents
//
// A hot key expires. In the next millisecond four hundred in-flight requests all miss, and all
// four hundred call the origin — which for merchant configuration is a database query with joins.
// The origin, which was comfortably serving one query per TTL, now serves four hundred at once
// and slows down; requests queue; more requests arrive and miss; the queue grows. This is
// cache-stampede collapse, and it is triggered by a *successful* cache doing exactly what it was
// asked to do.
//
// Single-flight fixes it by making the *first* caller for a key do the work while every other
// caller for the same key waits on that one result. Four hundred requests, one origin call.
//
// Implementation note (the deliverable asked which): this uses
// **golang.org/x/sync/singleflight**, which is already in go.mod (as an indirect requirement of
// the OpenTelemetry gRPC exporter's dependency graph) and therefore adds no new module. Writing
// the same thing over a sync.Map is about forty lines and gets the panic and error propagation
// subtly wrong the first time; using the standard implementation is strictly better here.
//
// Two properties worth stating because they are load-bearing:
//
//   - The flight key includes the tenant, because it is the *full* Redis key. Sharing a flight
//     across tenants would hand one tenant's value to another, which is the same isolation bug
//     BuildKey exists to prevent, one layer up.
//   - Single-flight is per-process. Four hundred pods still produce four hundred origin calls in
//     the worst case, not one. That is accepted: this collapses the 400x within a pod, which is
//     where the amplification actually is, and the cross-pod case is bounded by the pod count.
//     A distributed lock around the load would fix the remainder and is deliberately not used —
//     see the warning on Lock about never putting a lock on a correctness path; here it would
//     also make a Redis outage block the origin call rather than merely uncache it.
func (c *Cache) GetOrLoad(ctx context.Context, key string, ttl time.Duration, load func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	if load == nil {
		return nil, apierror.New(apierror.CodeInternalError, "redis: GetOrLoad needs a loader")
	}
	full, err := c.Key(ctx, key)
	if err != nil {
		return nil, err
	}

	// Fast path: a hit needs no coordination at all.
	if b, err := c.rdb.Get(ctx, full).Bytes(); err == nil {
		return b, nil
	} else if !errors.Is(err, goredis.Nil) && !IsUnavailable(err) {
		return nil, wrapRedis(err, "get")
	}

	v, err, _ := c.group.Do(full, func() (any, error) {
		// Re-check inside the flight. Between the miss above and winning the flight, the caller
		// ahead of us may already have populated the key, and re-reading is one round trip
		// against an origin call we would otherwise repeat.
		if b, gerr := c.rdb.Get(ctx, full).Bytes(); gerr == nil {
			return b, nil
		}

		loaded, lerr := load(ctx)
		if lerr != nil {
			return nil, lerr
		}
		// A failed write is not a failed load. The caller has its value; the next request pays
		// another miss. Reporting the error here would turn a cache degradation into a request
		// failure, which is the inversion this whole package avoids.
		_ = c.rdb.Set(ctx, full, loaded, c.boundTTL(ttl)).Err()
		return loaded, nil
	})
	if err != nil {
		return nil, err
	}
	b, _ := v.([]byte)
	return b, nil
}

// boundTTL applies the default and the cap.
func (c *Cache) boundTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		ttl = c.defaultTTL
	}
	if ttl > c.maxTTL {
		ttl = c.maxTTL
	}
	return ttl
}

// wrapRedis classifies a driver error.
//
// Everything from Redis is retryable and infrastructural: there is no such thing as a business
// rule violation in a cache. Classifying it as DEPENDENCY_FAILURE means a caller that does
// propagate it gets the retryable bit right, and a caller that swallows it (which is the normal
// case here) loses nothing.
func wrapRedis(err error, op string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return apierror.Wrapf(err, apierror.CodeServiceUnavailable, "redis: %s canceled", op)
	}
	return apierror.Wrapf(err, apierror.CodeDependencyFailure, "redis: %s failed", op)
}

var _ ports.Cache = (*Cache)(nil)
