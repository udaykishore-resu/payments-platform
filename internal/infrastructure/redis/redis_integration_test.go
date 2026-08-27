//go:build integration

// Integration tests against a real Redis. They are excluded from the default build; CI runs them
// with `go test -tags integration`, and `go vet -tags integration` keeps them compiling even when
// nobody runs them — a build-tagged test that does not compile is a test that silently stopped
// existing.
//
// These are the tests that exercise the Lua scripts. The unit tests deliberately do not: a
// hand-written approximation of Redis's semantics would only test the approximation, and the
// properties that matter here — atomicity across concurrent clients, sorted-set trimming, TTL
// behaviour — are exactly the ones an approximation gets wrong.
package redis

import (
	"context"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

func integrationClient(t *testing.T) *Client {
	t.Helper()
	addr := os.Getenv(EnvAddr)
	if addr == "" {
		t.Skip("set REDIS_ADDR to run the Redis integration tests")
	}
	cfg := DefaultConfig()
	cfg.Addr = addr
	cfg.TLS = false
	cfg.Environment = "local"
	cfg.ReadTimeout = 2 * time.Second

	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Health(ctx); err != nil {
		t.Skipf("redis at %s is not reachable: %v", addr, err)
	}
	return c
}

// uniqueTenantCtx gives each test its own tenant, so runs do not collide and nothing has to be
// flushed between them.
func uniqueTenantCtx(t *testing.T) context.Context {
	t.Helper()
	return telemetry.ContextWithFields(context.Background(),
		telemetry.Fields{TenantID: string(ids.New(ids.PrefixTenant))})
}

func TestIntegrationCacheRoundTrip(t *testing.T) {
	c := integrationClient(t)
	cache := NewCache(c.Redis())
	ctx := uniqueTenantCtx(t)

	if err := cache.Set(ctx, "k", []byte("v"), 30*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := cache.Get(ctx, "k")
	if err != nil || !ok || string(got) != "v" {
		t.Fatalf("Get = %q ok:%v err:%v", got, ok, err)
	}
	if err := cache.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := cache.Get(ctx, "k"); ok {
		t.Fatal("the key survived Delete")
	}
}

func TestIntegrationGetOrLoadCollapsesAStampede(t *testing.T) {
	c := integrationClient(t)
	cache := NewCache(c.Redis())
	ctx := uniqueTenantCtx(t)

	var loads int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cache.GetOrLoad(ctx, "hot", 30*time.Second, func(context.Context) ([]byte, error) {
				atomic.AddInt64(&loads, 1)
				time.Sleep(20 * time.Millisecond)
				return []byte("loaded"), nil
			})
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&loads); got > 2 {
		t.Fatalf("the origin was called %d times for one cold key", got)
	}
}

func TestIntegrationLockIsExclusiveAndTokenSafe(t *testing.T) {
	c := integrationClient(t)
	lock := NewLock(c.Redis())
	ctx := uniqueTenantCtx(t)

	release, ok, err := lock.Acquire(ctx, "sweep", 10*time.Second)
	if err != nil || !ok {
		t.Fatalf("Acquire = ok:%v err:%v", ok, err)
	}
	if _, ok, err := lock.Acquire(ctx, "sweep", 10*time.Second); err != nil || ok {
		t.Fatalf("a second holder acquired the lock: ok:%v err:%v", ok, err)
	}
	if err := release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	// After release the lock is free again.
	release2, ok, err := lock.Acquire(ctx, "sweep", 10*time.Second)
	if err != nil || !ok {
		t.Fatalf("re-Acquire = ok:%v err:%v", ok, err)
	}
	_ = release2(ctx)
}

// TestIntegrationReleaseDoesNotDeleteAnotherHoldersLock is the whole reason the release is a Lua
// compare-and-delete rather than a DEL.
func TestIntegrationReleaseDoesNotDeleteAnotherHoldersLock(t *testing.T) {
	c := integrationClient(t)
	lock := NewLock(c.Redis(), WithLockTTLBounds(time.Second, time.Minute))
	ctx := uniqueTenantCtx(t)

	// Holder A takes a one-second lock and then "pauses" past its TTL.
	releaseA, ok, err := lock.Acquire(ctx, "sweep", time.Second)
	if err != nil || !ok {
		t.Fatalf("A Acquire: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)

	// Holder B legitimately takes it.
	releaseB, ok, err := lock.Acquire(ctx, "sweep", 10*time.Second)
	if err != nil || !ok {
		t.Fatalf("B Acquire = ok:%v err:%v", ok, err)
	}

	// A wakes up and releases. It must NOT delete B's lock.
	if err := releaseA(ctx); err == nil {
		t.Fatal("A's release reported success; a plain DEL would have deleted B's lock")
	}
	if _, ok, _ := lock.Acquire(ctx, "sweep", time.Second); ok {
		t.Fatal("B's lock was deleted by A's release")
	}
	_ = releaseB(ctx)
}

func TestIntegrationRateLimiterIsAtomicUnderConcurrency(t *testing.T) {
	c := integrationClient(t)
	limiter := NewRateLimiter(c.Redis())
	ctx := uniqueTenantCtx(t)

	// A bucket of exactly 20 tokens with no meaningful refill during the test.
	limit := resilience.Limit{Rate: 0.001, Burst: 20}

	var allowed int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := limiter.Allow(ctx, "burst", limit)
			if err != nil {
				t.Errorf("Allow: %v", err)
				return
			}
			if d.Allowed {
				atomic.AddInt64(&allowed, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&allowed); got != 20 {
		t.Fatalf("%d of 100 concurrent requests were allowed against a 20-token bucket; the script is not atomic", got)
	}
}

func TestIntegrationVelocitySlidingWindow(t *testing.T) {
	c := integrationClient(t)
	v := NewVelocityCounter(c.Redis())
	ctx := uniqueTenantCtx(t)

	// Three events inside a one-second window.
	for i := 1; i <= 3; i++ {
		n, err := v.IncrementAndCount(ctx, "card:abc", time.Second)
		if err != nil {
			t.Fatalf("IncrementAndCount: %v", err)
		}
		if int(n) != i {
			t.Fatalf("count after %d events = %d", i, n)
		}
	}

	// After the window passes, they age out — a sliding window, not a fixed bucket.
	time.Sleep(1200 * time.Millisecond)
	n, err := v.Count(ctx, "card:abc", time.Second)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 0 {
		t.Fatalf("count after the window = %d, want 0", n)
	}
}

// TestIntegrationVelocityDoesNotUnderCountUnderConcurrency is the property the Lua script exists
// for: get-then-set loses updates in proportion to concurrency, which means it under-reports
// exactly during the attack it is meant to detect.
func TestIntegrationVelocityDoesNotUnderCountUnderConcurrency(t *testing.T) {
	c := integrationClient(t)
	v := NewVelocityCounter(c.Redis())
	ctx := uniqueTenantCtx(t)

	const events = 200
	var wg sync.WaitGroup
	for i := 0; i < events; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.IncrementAndCount(ctx, "card:attack", time.Minute); err != nil {
				t.Errorf("IncrementAndCount: %v", err)
			}
		}()
	}
	wg.Wait()

	n, err := v.Count(ctx, "card:attack", time.Minute)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != events {
		t.Fatalf("counted %d of %d concurrent events; the counter under-reports under load", n, events)
	}
}

func TestIntegrationVelocitySumAndAdd(t *testing.T) {
	c := integrationClient(t)
	v := NewVelocityCounter(c.Redis())
	ctx := uniqueTenantCtx(t)

	for i := 0; i < 3; i++ {
		add, err := money.New(1_000, money.Currency("EUR"))
		if err != nil {
			t.Fatalf("money.New: %v", err)
		}
		total, err := v.SumAndAdd(ctx, "merchant:daily:EUR", time.Minute, add)
		if err != nil {
			t.Fatalf("SumAndAdd: %v", err)
		}
		if want := int64(1_000 * (i + 1)); total.Amount() != want {
			t.Fatalf("total after %d adds = %d, want %d", i+1, total.Amount(), want)
		}
		if total.Currency() != money.Currency("EUR") {
			t.Fatalf("currency = %s", total.Currency())
		}
	}
}

func TestIntegrationIdempotencyAccelerator(t *testing.T) {
	c := integrationClient(t)
	cache := NewIdempotencyCache(c.Redis(), WithIdempotencyTTL(30*time.Second))
	ctx := context.Background()

	key := ports.IdempotencyKey{
		TenantID:     shared.TenantID(ids.New(ids.PrefixTenant)),
		MerchantID:   shared.MerchantID(ids.New(ids.PrefixMerchant)),
		Method:       "POST",
		PathTemplate: "/v1/payments",
		Key:          "integration-" + strconv.FormatInt(time.Now().UnixNano(), 36),
	}
	snap := ports.ResponseSnapshot{
		StatusCode:  201,
		Body:        []byte(`{"ok":true}`),
		ResourceID:  string(ids.New(ids.PrefixPayment)),
		CompletedAt: time.Now().UTC(),
	}

	if _, ok, err := cache.Lookup(ctx, key, "fp"); ok || err != nil {
		t.Fatalf("cold Lookup = ok:%v err:%v", ok, err)
	}
	if err := cache.Store(ctx, key, "fp", snap); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, ok, err := cache.Lookup(ctx, key, "fp")
	if err != nil || !ok {
		t.Fatalf("warm Lookup = ok:%v err:%v", ok, err)
	}
	if got.ResourceID != snap.ResourceID {
		t.Fatalf("resource = %q", got.ResourceID)
	}
	// A different fingerprint must read as a miss so the caller goes to Postgres.
	if _, ok, _ := cache.Lookup(ctx, key, "other-fp"); ok {
		t.Fatal("the accelerator answered for a different request body")
	}
	if err := cache.Invalidate(ctx, key); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if _, ok, _ := cache.Lookup(ctx, key, "fp"); ok {
		t.Fatal("the entry survived Invalidate")
	}
}
