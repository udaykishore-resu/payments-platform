package redis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// IdempotencyCache is a **non-authoritative** read-through accelerator in front of the Postgres
// idempotency store.
//
// # What this type is and is not — read before changing anything here
//
// **Postgres is the authority. This is a cache. A miss, a stale entry, a corrupted entry or a
// total Redis outage degrades latency and never correctness.**
//
// That is not aspirational wording; it is enforced by the shape of the API. This type can produce
// exactly one kind of answer — "this operation completed, and here is the response Postgres
// already stored" — and it can produce it only for a record Postgres itself wrote as COMPLETED.
// It cannot say "new", it cannot say "in progress", it cannot claim an operation, and it cannot
// report a fingerprint mismatch. Every one of those decisions requires a write that only the
// database can serialise, so every one of them goes to the database, always.
//
// Concretely, there is no path through this file that returns a decision Postgres would not have
// made:
//
//   - Lookup returns a stored response only when the cached record's fingerprint matches the
//     caller's. A mismatch is not reported as a conflict — the mismatch decision belongs to
//     Postgres, which has the durable record — it is reported as a miss, and the caller falls
//     through and gets the authoritative answer.
//   - There is no Claim. Two concurrent identical requests must be resolved by the unique index
//     in Postgres (`ON CONFLICT DO NOTHING`), because that is the only mechanism that gives a
//     deterministic winner. A Redis SET NX would look like it worked and would silently stop
//     working the moment a key is evicted under memory pressure — and eviction under memory
//     pressure is not a rare event, it is the normal end of a Redis key's life.
//   - Only COMPLETED and terminal-failure records are stored. An IN_FLIGHT record cached here
//     would let a replay be answered "in progress" after the operation had finished.
//   - Every Redis error is swallowed into a miss. A cache that can fail a request is a cache that
//     has become a dependency.
//
// The saving is real: the replay path for a retried payment creation is one Redis GET instead of
// a Postgres round trip on the request's critical path, and clients retry far more often than
// people expect. But the saving is the only thing this type is allowed to affect.
//
// See ADR-009 and ports.IdempotencyStore, whose own comment says the same thing from the other
// side: making the cache authoritative would mean a Redis eviction converts a duplicate request
// into a second payment.
//
// Safe for concurrent use.
type IdempotencyCache struct {
	rdb UniversalClient

	// ttl bounds an entry. It is shorter than the Postgres record's retention on purpose: an
	// entry that outlived its authoritative record would answer for an operation the database has
	// already forgotten, which is the one way a cache like this could contradict its origin.
	ttl time.Duration
}

// cachedResponse is the stored shape.
//
// The fingerprint is stored alongside the response so that a lookup can verify the caller is
// asking about the *same* request, not merely the same key. Without it, a client that reused a
// key for a different body would be served the first body's response out of cache — an answer
// Postgres would have refused with a fingerprint mismatch, which is exactly the class of
// divergence this type must not be capable of.
type cachedResponse struct {
	Fingerprint string    `json:"fingerprint"`
	StatusCode  int       `json:"statusCode"`
	Body        []byte    `json:"body"`
	ResourceID  string    `json:"resourceId"`
	CompletedAt time.Time `json:"completedAt"`
}

// IdempotencyCacheOption configures the accelerator.
type IdempotencyCacheOption func(*IdempotencyCache)

// WithIdempotencyTTL bounds how long an entry lives. It must stay under the Postgres record's
// retention; the constructor does not know that value, so this is documented rather than checked,
// and the wiring is expected to pass a value derived from the store's configuration.
func WithIdempotencyTTL(d time.Duration) IdempotencyCacheOption {
	return func(c *IdempotencyCache) {
		if d > 0 {
			c.ttl = d
		}
	}
}

// NewIdempotencyCache builds the accelerator.
func NewIdempotencyCache(client UniversalClient, opts ...IdempotencyCacheOption) *IdempotencyCache {
	c := &IdempotencyCache{rdb: client, ttl: 15 * time.Minute}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Key renders the Redis key for an idempotency scope.
//
// The tenant comes from the key itself rather than from the context, because an idempotency key
// is already fully scoped — tenant, merchant, method, path template, client key — and reading the
// tenant from ambient context here would let a mismatch between the two produce a lookup in the
// wrong tenant's namespace.
//
// The client's key is hashed rather than embedded. It is opaque, client-chosen text that may
// contain their own identifiers, arbitrary length and arbitrary bytes; hashing bounds the key
// length, keeps the value out of anything that dumps key names (Redis SCAN output, slow logs,
// keyspace notifications), and costs nothing.
func (c *IdempotencyCache) Key(k ports.IdempotencyKey) (string, error) {
	if k.TenantID.IsZero() {
		return "", apierror.Wrap(ErrNoTenant, apierror.CodeMissingTenantContext,
			"redis: idempotency lookup without a tenant")
	}
	if k.Key == "" {
		return "", apierror.New(apierror.CodeIdempotencyKeyRequired, "redis: no idempotency key")
	}
	sum := sha256.Sum256([]byte(k.MerchantID.String() + "\x00" + k.Method + "\x00" + k.PathTemplate + "\x00" + k.Key))
	return BuildKey(k.TenantID, NamespaceIdempotency, hex.EncodeToString(sum[:]))
}

// Lookup returns the stored response for a completed operation.
//
// The boolean is "the cache can answer this". It is false for a miss, for a fingerprint mismatch,
// for a malformed entry and for every Redis failure — all of which mean the same thing to the
// caller: ask Postgres. The error return exists only for a caller-side mistake (an unusable key);
// an infrastructure failure is never surfaced, because a caller that can distinguish "Redis is
// down" from "not cached" is a caller that will eventually branch on it.
func (c *IdempotencyCache) Lookup(ctx context.Context, k ports.IdempotencyKey, fingerprint string) (ports.ResponseSnapshot, bool, error) {
	full, err := c.Key(k)
	if err != nil {
		return ports.ResponseSnapshot{}, false, err
	}

	b, err := c.rdb.Get(ctx, full).Bytes()
	if err != nil {
		// Miss, outage, timeout: all the same answer. Deliberately not distinguished — see above.
		return ports.ResponseSnapshot{}, false, nil //nolint:nilerr // miss, outage and timeout are one answer by contract: ask Postgres (see the doc comment)
	}

	var stored cachedResponse
	if json.Unmarshal(b, &stored) != nil {
		// A corrupted entry is a miss. Deleting it would be tidier and is not worth a write on
		// the request path; its TTL will do it.
		return ports.ResponseSnapshot{}, false, nil //nolint:nilerr // a corrupted entry is a miss; the caller's next stop is Postgres either way
	}

	// The fingerprint check. A mismatch means the client reused the key with a different body,
	// which is a decision Postgres owns — it must be reported to the client as
	// IDEMPOTENCY_KEY_REUSED, and that reporting has to come from the durable record. Answering
	// "miss" sends the caller there.
	if stored.Fingerprint == "" || stored.Fingerprint != fingerprint {
		return ports.ResponseSnapshot{}, false, nil
	}

	return ports.ResponseSnapshot{
		StatusCode:  stored.StatusCode,
		Body:        stored.Body,
		ResourceID:  stored.ResourceID,
		CompletedAt: stored.CompletedAt,
	}, true, nil
}

// Store caches a completed operation's response.
//
// It is called *after* Postgres has committed the COMPLETED record, never before and never
// instead. Calling it before would create a window in which the cache can answer for an operation
// the database has not recorded — and if the transaction then rolled back, the cache would be
// serving a response for something that never happened.
//
// It returns an error only for an unusable key. A Redis failure is swallowed: failing the request
// because the cache could not be warmed would make the accelerator a source of outages, and the
// next request simply pays a database lookup, which is the behaviour we had before this type
// existed.
func (c *IdempotencyCache) Store(ctx context.Context, k ports.IdempotencyKey, fingerprint string, snap ports.ResponseSnapshot) error {
	full, err := c.Key(k)
	if err != nil {
		return err
	}
	if fingerprint == "" {
		// Without a fingerprint, Lookup could never match, so the entry would be dead weight. It
		// is a programming error at the call site rather than a runtime condition.
		return apierror.New(apierror.CodeInternalError,
			"redis: refusing to cache an idempotency response with no request fingerprint")
	}
	if snap.CompletedAt.IsZero() {
		return apierror.New(apierror.CodeInternalError,
			"redis: refusing to cache an idempotency response that is not marked completed")
	}

	payload, err := json.Marshal(cachedResponse{
		Fingerprint: fingerprint,
		StatusCode:  snap.StatusCode,
		Body:        snap.Body,
		ResourceID:  snap.ResourceID,
		CompletedAt: snap.CompletedAt.UTC(),
	})
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "redis: encoding idempotency response")
	}

	_ = c.rdb.Set(ctx, full, payload, c.ttl).Err()
	return nil
}

// Invalidate drops an entry.
//
// Used by the release path: when an operation fails retryably, Postgres removes the IN_FLIGHT
// claim so the client's retry is a genuine new attempt, and any cached response for that scope
// must go with it. A failure here is swallowed for the same reason as in Store — but note that
// this is the one operation where a swallowed failure is worth a metric, because a stale entry
// that outlives its authoritative record is the only way this type could ever answer for
// something Postgres has forgotten. The TTL is the backstop.
func (c *IdempotencyCache) Invalidate(ctx context.Context, k ports.IdempotencyKey) error {
	full, err := c.Key(k)
	if err != nil {
		return err
	}
	if err := c.rdb.Del(ctx, full).Err(); err != nil && !errors.Is(err, goredis.Nil) {
		return wrapRedis(err, "idempotency invalidate")
	}
	return nil
}

// TTL reports the configured entry lifetime, so wiring can assert it against the Postgres store's
// retention rather than assuming.
func (c *IdempotencyCache) TTL() time.Duration { return c.ttl }
