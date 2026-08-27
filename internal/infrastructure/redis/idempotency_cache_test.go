package redis

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

func idemKey() ports.IdempotencyKey {
	return ports.IdempotencyKey{
		TenantID:     shared.TenantID(testTenant),
		MerchantID:   shared.MerchantID(testMerchant),
		Method:       "POST",
		PathTemplate: "/v1/payments",
		Key:          "client-chosen-key-0001",
	}
}

func snapshot() ports.ResponseSnapshot {
	return ports.ResponseSnapshot{
		StatusCode:  201,
		Body:        []byte(`{"id":"pay_01JB8Z9K2QW3E4R5T6Y7U8I9O0"}`),
		ResourceID:  "pay_01JB8Z9K2QW3E4R5T6Y7U8I9O0",
		CompletedAt: time.Date(2026, 8, 26, 14, 3, 12, 0, time.UTC),
	}
}

func TestIdempotencyKeyIsTenantScopedAndHashed(t *testing.T) {
	t.Parallel()
	c := NewIdempotencyCache(newFakeRedis())
	got, err := c.Key(idemKey())
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if !strings.HasPrefix(got, "pp:{"+testTenant+"}:idem:") {
		t.Fatalf("Key = %q, want a tenant-scoped idem key", got)
	}
	// The client's key is opaque, client-chosen text that may carry their identifiers. It must
	// not appear in a key name that shows up in SCAN output or a slow log.
	if strings.Contains(got, "client-chosen-key-0001") {
		t.Fatalf("the client's idempotency key is embedded verbatim: %q", got)
	}

	// The scope is part of the hash: the same client key on a different endpoint is a different
	// operation.
	other := idemKey()
	other.PathTemplate = "/v1/refunds"
	otherKey, _ := c.Key(other)
	if otherKey == got {
		t.Fatal("two different scopes hash to the same key")
	}
}

func TestIdempotencyKeyRejectsAnUntenantedOrEmptyKey(t *testing.T) {
	t.Parallel()
	c := NewIdempotencyCache(newFakeRedis())

	noTenant := idemKey()
	noTenant.TenantID = ""
	if _, err := c.Key(noTenant); !errors.Is(err, ErrNoTenant) {
		t.Errorf("Key with no tenant: %v", err)
	}

	noKey := idemKey()
	noKey.Key = ""
	if _, err := c.Key(noKey); err == nil {
		t.Error("Key accepted an empty idempotency key")
	}
}

func TestStoreThenLookupReturnsTheSnapshot(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	c := NewIdempotencyCache(f)
	ctx := context.Background() // no tenant needed: the key carries its own scope

	if err := c.Store(ctx, idemKey(), "fingerprint-a", snapshot()); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, ok, err := c.Lookup(ctx, idemKey(), "fingerprint-a")
	if err != nil || !ok {
		t.Fatalf("Lookup = ok:%v err:%v", ok, err)
	}
	if got.StatusCode != http.StatusCreated || got.ResourceID != snapshot().ResourceID {
		t.Fatalf("snapshot = %+v", got)
	}
	if string(got.Body) != string(snapshot().Body) {
		t.Fatalf("body = %s", got.Body)
	}
	if !got.CompletedAt.Equal(snapshot().CompletedAt) {
		t.Fatalf("completedAt = %v", got.CompletedAt)
	}
}

// TestLookupReportsAFingerprintMismatchAsAMiss is the central safety property.
//
// A mismatch means the client reused the key with a different body. That decision — reporting
// IDEMPOTENCY_KEY_REUSED — belongs to Postgres, which has the durable record. The cache must send
// the caller there rather than answering, and it must certainly never serve the first body's
// response for the second body's request.
func TestLookupReportsAFingerprintMismatchAsAMiss(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	c := NewIdempotencyCache(f)
	ctx := context.Background()

	if err := c.Store(ctx, idemKey(), "fingerprint-a", snapshot()); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, ok, err := c.Lookup(ctx, idemKey(), "fingerprint-b")
	if err != nil {
		t.Fatalf("Lookup returned an error where a miss was required: %v", err)
	}
	if ok {
		t.Fatalf("the cache answered for a different request body: %+v", got)
	}
}

// TestLookupNeverFailsARequest: a miss, a corrupt entry and a total outage are all the same
// answer — ask Postgres.
func TestLookupNeverFailsARequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("miss", func(t *testing.T) {
		t.Parallel()
		_, ok, err := NewIdempotencyCache(newFakeRedis()).Lookup(ctx, idemKey(), "fp")
		if ok || err != nil {
			t.Fatalf("ok:%v err:%v", ok, err)
		}
	})

	t.Run("outage", func(t *testing.T) {
		t.Parallel()
		f := newFakeRedis()
		f.setErr(errors.New("connection refused"))
		_, ok, err := NewIdempotencyCache(f).Lookup(ctx, idemKey(), "fp")
		if ok {
			t.Fatal("the cache answered during an outage")
		}
		if err != nil {
			t.Fatalf("a Redis outage failed the request: %v", err)
		}
	})

	t.Run("corrupt entry", func(t *testing.T) {
		t.Parallel()
		f := newFakeRedis()
		c := NewIdempotencyCache(f)
		key, _ := c.Key(idemKey())
		f.put(key, []byte("not json"))

		_, ok, err := c.Lookup(ctx, idemKey(), "fp")
		if ok || err != nil {
			t.Fatalf("a corrupt entry must be a miss: ok:%v err:%v", ok, err)
		}
	})

	t.Run("entry with no fingerprint", func(t *testing.T) {
		t.Parallel()
		f := newFakeRedis()
		c := NewIdempotencyCache(f)
		key, _ := c.Key(idemKey())
		raw, _ := json.Marshal(cachedResponse{StatusCode: 201, CompletedAt: time.Now()})
		f.put(key, raw)

		_, ok, err := c.Lookup(ctx, idemKey(), "fp")
		if ok || err != nil {
			t.Fatalf("an entry with no fingerprint must be a miss: ok:%v err:%v", ok, err)
		}
	})
}

// TestStoreRefusesAnIncompleteRecord. Only a COMPLETED record may be cached: an IN_FLIGHT entry
// here would let a replay be answered "in progress" after the operation had finished.
func TestStoreRefusesAnIncompleteRecord(t *testing.T) {
	t.Parallel()
	c := NewIdempotencyCache(newFakeRedis())
	ctx := context.Background()

	if err := c.Store(ctx, idemKey(), "", snapshot()); err == nil {
		t.Error("Store accepted a snapshot with no fingerprint; Lookup could never match it")
	}
	incomplete := snapshot()
	incomplete.CompletedAt = time.Time{}
	if err := c.Store(ctx, idemKey(), "fp", incomplete); err == nil {
		t.Error("Store accepted a record that is not marked completed")
	}
}

// TestStoreNeverFailsARequestOnARedisError: failing the request because the cache could not be
// warmed would make the accelerator a source of outages.
func TestStoreNeverFailsARequestOnARedisError(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	f.setErr(errors.New("connection refused"))
	if err := NewIdempotencyCache(f).Store(context.Background(), idemKey(), "fp", snapshot()); err != nil {
		t.Fatalf("Store failed the request because Redis was down: %v", err)
	}
}

func TestStoreBoundsTheEntryWithATTL(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	c := NewIdempotencyCache(f, WithIdempotencyTTL(90*time.Second))
	key, _ := c.Key(idemKey())

	if err := c.Store(context.Background(), idemKey(), "fp", snapshot()); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if got := f.ttlOf(key); got != 90*time.Second {
		t.Fatalf("ttl = %v, want 90s", got)
	}
	if c.TTL() != 90*time.Second {
		t.Fatalf("TTL() = %v", c.TTL())
	}
}

func TestInvalidateRemovesTheEntry(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	c := NewIdempotencyCache(f)
	ctx := context.Background()
	key, _ := c.Key(idemKey())

	if err := c.Store(ctx, idemKey(), "fp", snapshot()); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if !f.has(key) {
		t.Fatal("Store wrote nothing")
	}
	if err := c.Invalidate(ctx, idemKey()); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if f.has(key) {
		t.Fatal("the entry survived Invalidate; it could outlive its authoritative record")
	}
}

// TestTheAcceleratorHasNoClaimPath is a structural assertion of the design contract: this type
// can produce exactly one kind of answer, and it must be impossible for it to claim an operation.
// Two concurrent identical requests are resolved by the unique index in Postgres, because that is
// the only mechanism that gives a deterministic winner; a Redis SET NX would look like it worked
// and would silently stop working the moment a key is evicted under memory pressure.
func TestTheAcceleratorHasNoClaimPath(t *testing.T) {
	t.Parallel()
	var c any = NewIdempotencyCache(newFakeRedis())

	if _, hasClaim := c.(interface {
		Claim(context.Context, ports.IdempotencyRecord) (ports.ClaimResult, error)
	}); hasClaim {
		t.Fatal("the accelerator grew a Claim method; claiming is Postgres's job and only Postgres can serialise it")
	}
	if _, isStore := c.(ports.IdempotencyStore); isStore {
		t.Fatal("the accelerator satisfies ports.IdempotencyStore; it must never be substitutable for the authoritative store")
	}
}
