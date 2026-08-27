package redis

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

func TestBuildKeyIsTenantScoped(t *testing.T) {
	t.Parallel()
	got, err := BuildKey(shared.TenantID(testTenant), NamespaceCache, "merchant", testMerchant)
	if err != nil {
		t.Fatalf("BuildKey: %v", err)
	}
	want := "pp:{" + testTenant + "}:cache:merchant:" + testMerchant
	if got != want {
		t.Fatalf("BuildKey = %q, want %q", got, want)
	}
	// The hash tag is what keeps a tenant's keys on one Cluster slot, which is what makes the Lua
	// scripts in this package legal at all.
	if !strings.Contains(got, "{"+testTenant+"}") {
		t.Fatalf("the tenant is not inside a Cluster hash tag: %q", got)
	}
}

// TestBuildKeyRejectsAnUntenantedKey is the negative test that matters most in this package.
//
// A key without a tenant prefix is readable by every tenant: one tenant's merchant configuration
// would be served to the next tenant that asks for theirs. The database has RLS; a cache does
// not, so the key structure carries the isolation and a missing tenant must fail loudly.
func TestBuildKeyRejectsAnUntenantedKey(t *testing.T) {
	t.Parallel()
	_, err := BuildKey("", NamespaceCache, "merchant")
	if err == nil {
		t.Fatal("BuildKey produced a key with no tenant")
	}
	if !errors.Is(err, ErrNoTenant) {
		t.Fatalf("error does not wrap ErrNoTenant: %v", err)
	}
	if apierror.CodeOf(err) != apierror.CodeMissingTenantContext {
		t.Fatalf("code = %s, want %s", apierror.CodeOf(err), apierror.CodeMissingTenantContext)
	}
}

func TestBuildKeyRejectsEmptyComponents(t *testing.T) {
	t.Parallel()
	if _, err := BuildKey(shared.TenantID(testTenant), "", "x"); err == nil {
		t.Error("BuildKey accepted an empty namespace")
	}
	if _, err := BuildKey(shared.TenantID(testTenant), NamespaceCache, "a", "", "b"); err == nil {
		t.Error("BuildKey accepted an empty key component")
	}
}

// TestEveryComponentRejectsAContextWithoutATenant walks the whole package, because a single
// component that forgot the check is a single component through which the isolation leaks.
func TestEveryComponentRejectsAContextWithoutATenant(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	ctx := context.Background() // no tenant

	if _, _, err := NewCache(f).Get(ctx, "k"); !errors.Is(err, ErrNoTenant) {
		t.Errorf("Cache.Get: %v", err)
	}
	if err := NewCache(f).Set(ctx, "k", []byte("v"), time.Minute); !errors.Is(err, ErrNoTenant) {
		t.Errorf("Cache.Set: %v", err)
	}
	if err := NewCache(f).Delete(ctx, "k"); !errors.Is(err, ErrNoTenant) {
		t.Errorf("Cache.Delete: %v", err)
	}
	if _, err := NewCache(f).GetOrLoad(ctx, "k", time.Minute, func(context.Context) ([]byte, error) {
		t.Error("the loader ran for an untenanted key")
		return nil, nil
	}); !errors.Is(err, ErrNoTenant) {
		t.Errorf("Cache.GetOrLoad: %v", err)
	}
	if _, _, err := NewLock(f).Acquire(ctx, "k", time.Minute); !errors.Is(err, ErrNoTenant) {
		t.Errorf("Lock.Acquire: %v", err)
	}
	if _, err := NewVelocityCounter(f).IncrementAndCount(ctx, "k", time.Minute); !errors.Is(err, ErrNoTenant) {
		t.Errorf("VelocityCounter.IncrementAndCount: %v", err)
	}
	if _, err := NewVelocityCounter(f).Count(ctx, "k", time.Minute); !errors.Is(err, ErrNoTenant) {
		t.Errorf("VelocityCounter.Count: %v", err)
	}
	if _, err := NewRateLimiter(f).Key(ctx, "k"); !errors.Is(err, ErrNoTenant) {
		t.Errorf("RateLimiter.Key: %v", err)
	}
}

func TestCacheGetSetDelete(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	c := NewCache(f)
	ctx := tenantCtx()

	if _, ok, err := c.Get(ctx, "k"); err != nil || ok {
		t.Fatalf("cold Get = ok:%v err:%v", ok, err)
	}
	if err := c.Set(ctx, "k", []byte("value"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok || string(got) != "value" {
		t.Fatalf("Get = %q ok:%v err:%v", got, ok, err)
	}
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Fatal("the key survived Delete")
	}
}

func TestCacheTTLIsBounded(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	c := NewCache(f, WithDefaultTTL(30*time.Second), WithMaxTTL(2*time.Minute))
	ctx := tenantCtx()
	key, _ := c.Key(ctx, "k")

	// Zero means the default: an entry with no TTL is a memory leak with a friendly name.
	if err := c.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := f.ttlOf(key); got != 30*time.Second {
		t.Fatalf("default TTL = %v", got)
	}
	// And an over-long TTL is capped.
	if err := c.Set(ctx, "k", []byte("v"), time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := f.ttlOf(key); got != 2*time.Minute {
		t.Fatalf("capped TTL = %v", got)
	}
}

func TestCacheGetReportsAnOutageDistinctlyFromAMiss(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	f.setErr(errors.New("connection refused"))
	c := NewCache(f)

	_, ok, err := c.Get(tenantCtx(), "k")
	if ok {
		t.Fatal("Get reported a hit during an outage")
	}
	if err == nil {
		t.Fatal("an outage must be distinguishable from a miss; the metrics mean different things")
	}
	if !apierror.IsRetryable(err) {
		t.Fatalf("a Redis outage must be retryable: %v", err)
	}
}

// TestGetOrLoadSingleFlights is the stampede test: one origin call for many concurrent misses.
func TestGetOrLoadSingleFlights(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	c := NewCache(f)
	ctx := tenantCtx()

	var loads int64
	release := make(chan struct{})
	load := func(context.Context) ([]byte, error) {
		atomic.AddInt64(&loads, 1)
		<-release // hold the flight open so every caller piles onto it
		return []byte("loaded"), nil
	}

	const callers = 200
	var wg sync.WaitGroup
	results := make([][]byte, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = c.GetOrLoad(ctx, "hot", time.Minute, load)
		}(i)
	}
	// Give the goroutines time to arrive at the flight before releasing it.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(&loads); got != 1 {
		t.Fatalf("the origin was called %d times for one cold key; this is the stampede", got)
	}
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if string(results[i]) != "loaded" {
			t.Fatalf("caller %d got %q", i, results[i])
		}
	}
	if f.setCount() == 0 {
		t.Fatal("the loaded value was never cached")
	}
}

// TestGetOrLoadDoesNotShareFlightsAcrossTenants: sharing one would hand one tenant's value to
// another, which is the isolation bug BuildKey exists to prevent, one layer up.
func TestGetOrLoadDoesNotShareFlightsAcrossTenants(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	c := NewCache(f)

	ctxA := tenantCtx()
	ctxB := context.Background()
	otherTenant := "ten_01JB8Z99999999999999999999"
	c2 := NewCache(f, WithTenantResolver(func(context.Context) (shared.TenantID, bool) {
		return shared.TenantID(otherTenant), true
	}))

	a, err := c.GetOrLoad(ctxA, "same", time.Minute, func(context.Context) ([]byte, error) {
		return []byte("tenant-a"), nil
	})
	if err != nil {
		t.Fatalf("tenant A: %v", err)
	}
	b, err := c2.GetOrLoad(ctxB, "same", time.Minute, func(context.Context) ([]byte, error) {
		return []byte("tenant-b"), nil
	})
	if err != nil {
		t.Fatalf("tenant B: %v", err)
	}
	if string(a) == string(b) {
		t.Fatalf("both tenants got %q from one flight", a)
	}
}

func TestGetOrLoadServesAHitWithoutCallingTheOrigin(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	c := NewCache(f)
	ctx := tenantCtx()
	key, _ := c.Key(ctx, "warm")
	f.put(key, []byte("cached"))

	got, err := c.GetOrLoad(ctx, "warm", time.Minute, func(context.Context) ([]byte, error) {
		t.Error("the origin was called for a cache hit")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	if string(got) != "cached" {
		t.Fatalf("got %q", got)
	}
}

// TestGetOrLoadFallsThroughWhenRedisIsDown is the whole safety argument in one test: a Redis
// outage costs latency, never correctness.
func TestGetOrLoadFallsThroughWhenRedisIsDown(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	f.setErr(errors.New("connection refused"))
	c := NewCache(f)

	got, err := c.GetOrLoad(tenantCtx(), "k", time.Minute, func(context.Context) ([]byte, error) {
		return []byte("from-origin"), nil
	})
	if err != nil {
		t.Fatalf("a Redis outage failed the request: %v", err)
	}
	if string(got) != "from-origin" {
		t.Fatalf("got %q", got)
	}
}

func TestGetOrLoadPropagatesAnOriginFailure(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	c := NewCache(f)
	want := errors.New("database down")

	_, err := c.GetOrLoad(tenantCtx(), "k", time.Minute, func(context.Context) ([]byte, error) {
		return nil, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("GetOrLoad = %v, want the origin's error", err)
	}
	if f.setCount() != 0 {
		t.Fatal("a failed load was cached")
	}
}

func TestGetOrLoadRequiresALoader(t *testing.T) {
	t.Parallel()
	if _, err := NewCache(newFakeRedis()).GetOrLoad(tenantCtx(), "k", time.Minute, nil); err == nil {
		t.Fatal("GetOrLoad accepted a nil loader")
	}
}

// TestGetOrLoadCachesFailuresOfTheWriteSilently: a failed cache write is not a failed load.
func TestCacheWriteFailureDoesNotFailTheLoad(t *testing.T) {
	t.Parallel()
	f := &writeFailingRedis{fakeRedis: newFakeRedis()}
	c := NewCache(f)

	got, err := c.GetOrLoad(tenantCtx(), "k", time.Minute, func(context.Context) ([]byte, error) {
		return []byte("v"), nil
	})
	if err != nil {
		t.Fatalf("a failed cache write failed the request: %v", err)
	}
	if string(got) != "v" {
		t.Fatalf("got %q", got)
	}
}

// writeFailingRedis reads fine and fails every write, which is the shape of a Redis that has hit
// its maxmemory limit with no eviction policy.
type writeFailingRedis struct{ *fakeRedis }

func (w *writeFailingRedis) Set(ctx context.Context, key string, value any, ttl time.Duration) *goredis.StatusCmd {
	return goredis.NewStatusResult("", errors.New("OOM command not allowed when used memory > 'maxmemory'"))
}
