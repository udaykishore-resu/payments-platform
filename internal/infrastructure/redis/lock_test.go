package redis

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

func TestLockKeyIsTenantScopedAndNamespaced(t *testing.T) {
	t.Parallel()
	l := NewLock(newFakeRedis())
	got, err := l.Key(tenantCtx(), "auth-expiry-sweep")
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	want := "pp:{" + testTenant + "}:lock:auth-expiry-sweep"
	if got != want {
		t.Fatalf("Key = %q, want %q", got, want)
	}
	// A lock key must never collide with a cache key of the same name.
	c := NewCache(newFakeRedis())
	cacheKey, _ := c.Key(tenantCtx(), "auth-expiry-sweep")
	if cacheKey == got {
		t.Fatal("lock and cache namespaces collide")
	}
}

func TestAcquireIsExclusive(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	l := NewLock(f)
	ctx := tenantCtx()

	release, ok, err := l.Acquire(ctx, "sweep", time.Minute)
	if err != nil || !ok {
		t.Fatalf("first Acquire = ok:%v err:%v", ok, err)
	}
	// SET NX must refuse the second holder.
	if _, ok, err := l.Acquire(ctx, "sweep", time.Minute); err != nil || ok {
		t.Fatalf("second Acquire = ok:%v err:%v, want not acquired", ok, err)
	}

	// The release script's reply is what decides; the fake returns whatever we tell it.
	f.setScriptReply(int64(1))
	if err := release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// TestAcquireUsesSetNXWithATTLInOneCommand. SETNX-then-EXPIRE has a window in which the process
// dies between them and leaves a lock with no expiry, held forever.
func TestAcquireUsesSetNXWithATTLInOneCommand(t *testing.T) {
	t.Parallel()
	f := &recordingSetArgs{fakeRedis: newFakeRedis()}
	l := NewLock(f)

	if _, ok, err := l.Acquire(tenantCtx(), "sweep", 90*time.Second); err != nil || !ok {
		t.Fatalf("Acquire = ok:%v err:%v", ok, err)
	}
	if !strings.EqualFold(f.mode, "NX") {
		t.Fatalf("SET mode = %q, want NX", f.mode)
	}
	if f.ttl != 90*time.Second {
		t.Fatalf("SET TTL = %v, want the requested 90s in the same command", f.ttl)
	}
	if f.token == "" {
		t.Fatal("no token was written; a plain DEL release would then be unavoidable")
	}
}

// TestTokensAreUniquePerAcquisition. Without this the token comparison in the release script is
// theatre: two processes would compare equal and each could release the other's lock.
func TestTokensAreUniquePerAcquisition(t *testing.T) {
	t.Parallel()
	seen := map[string]struct{}{}
	for i := 0; i < 500; i++ {
		f := &recordingSetArgs{fakeRedis: newFakeRedis()}
		if _, ok, err := NewLock(f).Acquire(tenantCtx(), "k", time.Minute); err != nil || !ok {
			t.Fatalf("Acquire: %v", err)
		}
		if _, dup := seen[f.token]; dup {
			t.Fatalf("token %q was minted twice", f.token)
		}
		seen[f.token] = struct{}{}
	}
}

// TestReleaseComparesTheToken pins the argument construction of the compare-and-delete script.
func TestReleaseComparesTheToken(t *testing.T) {
	t.Parallel()
	f := &recordingSetArgs{fakeRedis: newFakeRedis()}
	l := NewLock(f)
	ctx := tenantCtx()

	release, ok, err := l.Acquire(ctx, "sweep", time.Minute)
	if err != nil || !ok {
		t.Fatalf("Acquire: %v", err)
	}
	f.setScriptReply(int64(1))
	if err := release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}

	call := f.lastScript()
	if len(call.keys) != 1 || !strings.HasSuffix(call.keys[0], ":lock:sweep") {
		t.Fatalf("release script keys = %v", call.keys)
	}
	if len(call.args) != 1 || call.args[0] != f.token {
		t.Fatalf("release script args = %v, want the acquisition token", call.args)
	}
}

// TestReleaseReportsLosingTheLock. A release that finds the lock is no longer ours means the
// operation outlived its TTL, which is the condition under which two holders exist — worth
// surfacing, never swallowing.
func TestReleaseReportsLosingTheLock(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	l := NewLock(f)
	ctx := tenantCtx()

	release, ok, err := l.Acquire(ctx, "sweep", time.Minute)
	if err != nil || !ok {
		t.Fatalf("Acquire: %v", err)
	}
	f.setScriptReply(int64(0)) // the script found a different token, or no key
	err = release(ctx)
	if err == nil {
		t.Fatal("release silently accepted losing the lock")
	}
	if !strings.Contains(err.Error(), "no longer held") {
		t.Fatalf("error does not explain what happened: %v", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	l := NewLock(f)
	ctx := tenantCtx()

	release, _, err := l.Acquire(ctx, "sweep", time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	f.setScriptReply(int64(1))
	if err := release(ctx); err != nil {
		t.Fatalf("first release: %v", err)
	}
	before := f.scriptCount()
	if err := release(ctx); err != nil {
		t.Fatalf("second release: %v", err)
	}
	if f.scriptCount() != before {
		t.Fatal("a second release issued another compare-and-delete; it could delete a new holder's lock")
	}
}

// TestReleaseSurvivesACanceledContext: release must still run when the work's context ended,
// which is exactly the case where holding the lock to its TTL is most annoying.
func TestReleaseSurvivesACanceledContext(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	l := NewLock(f)

	release, ok, err := l.Acquire(tenantCtx(), "sweep", time.Minute)
	if err != nil || !ok {
		t.Fatalf("Acquire: %v", err)
	}
	ctx, cancel := context.WithCancel(tenantCtx())
	cancel()

	f.setScriptReply(int64(1))
	if err := release(ctx); err != nil {
		t.Fatalf("release on a canceled context: %v", err)
	}
	if f.scriptCount() == 0 {
		t.Fatal("the release script never ran")
	}
}

// TestAcquireFailsClosedOnAnOutage: returning "acquired" on error is the one way to make a lock
// outage dangerous.
func TestAcquireFailsClosedOnAnOutage(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	f.setErr(errors.New("connection refused"))

	release, ok, err := NewLock(f).Acquire(tenantCtx(), "sweep", time.Minute)
	if ok {
		t.Fatal("Acquire reported success during an outage")
	}
	if err == nil {
		t.Fatal("Acquire swallowed the outage")
	}
	if release != nil {
		t.Fatal("a non-nil release was returned for an unacquired lock")
	}
}

func TestLockTTLIsBounded(t *testing.T) {
	t.Parallel()
	f := &recordingSetArgs{fakeRedis: newFakeRedis()}
	l := NewLock(f, WithLockTTLBounds(2*time.Second, 30*time.Second))

	if _, _, err := l.Acquire(tenantCtx(), "a", time.Millisecond); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if f.ttl != 2*time.Second {
		t.Fatalf("floored TTL = %v, want 2s", f.ttl)
	}

	f2 := &recordingSetArgs{fakeRedis: newFakeRedis()}
	l2 := NewLock(f2, WithLockTTLBounds(2*time.Second, 30*time.Second))
	if _, _, err := l2.Acquire(tenantCtx(), "b", time.Hour); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if f2.ttl != 30*time.Second {
		t.Fatalf("capped TTL = %v, want 30s", f2.ttl)
	}
}

// TestAutoExtendingLockRenewsAndStops covers the goroutine's whole life: it renews while held and
// exits on release, leaving nothing behind.
func TestAutoExtendingLockRenewsAndStops(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	f.setScriptReply(int64(1))

	base := NewLock(f, WithLockTTLBounds(60*time.Millisecond, time.Minute))
	auto := NewAutoExtendingLock(base, nil)
	ctx := tenantCtx()

	release, ok, err := auto.Acquire(ctx, "long-job", 60*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("Acquire = ok:%v err:%v", ok, err)
	}

	// The renewal interval is a third of the TTL, so a few should land inside 150 ms.
	time.Sleep(150 * time.Millisecond)
	renewals := f.scriptCount()
	if renewals == 0 {
		t.Fatal("the lease was never renewed")
	}

	if err := release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	after := f.scriptCount()
	time.Sleep(120 * time.Millisecond)
	if f.scriptCount() != after {
		t.Fatal("the renewal goroutine outlived release")
	}
}

func TestAutoExtendingLockStopsWhenTheLockIsLost(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	f.setScriptReply(int64(1))
	lost := make(chan string, 1)

	base := NewLock(f, WithLockTTLBounds(60*time.Millisecond, time.Minute))
	auto := NewAutoExtendingLock(base, func(key string) {
		select {
		case lost <- key:
		default:
		}
	})
	ctx := tenantCtx()

	release, ok, err := auto.Acquire(ctx, "long-job", 60*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = release(ctx) }()

	// The next renewal finds a different token.
	f.setScriptReply(int64(0))
	select {
	case key := <-lost:
		if !strings.Contains(key, ":lock:long-job") {
			t.Fatalf("onLost got %q", key)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("losing the lock was never reported")
	}
}

// TestAutoExtendingLockStopsOnContextCancellation: the renewal goroutine's owner is the caller's
// context as well as release, so a canceled job leaves nothing running.
func TestAutoExtendingLockStopsOnContextCancellation(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	f.setScriptReply(int64(1))
	base := NewLock(f, WithLockTTLBounds(60*time.Millisecond, time.Minute))
	auto := NewAutoExtendingLock(base, nil)

	ctx, cancel := context.WithCancel(tenantCtx())
	release, ok, err := auto.Acquire(ctx, "long-job", 60*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("Acquire: %v", err)
	}
	cancel()
	time.Sleep(100 * time.Millisecond)
	before := f.scriptCount()
	time.Sleep(150 * time.Millisecond)
	if f.scriptCount() > before+1 {
		t.Fatal("the renewal goroutine kept running after its context was canceled")
	}
	_ = release(context.Background())
}

// recordingSetArgs captures what SetArgs was called with, which is where the NX/PX correctness
// lives.
type recordingSetArgs struct {
	*fakeRedis
	mode  string
	ttl   time.Duration
	token string
}

func (r *recordingSetArgs) SetArgs(ctx context.Context, key string, value any, a goredis.SetArgs) *goredis.StatusCmd {
	r.mode = a.Mode
	r.ttl = a.TTL
	if s, ok := value.(string); ok {
		r.token = s
	}
	return r.fakeRedis.SetArgs(ctx, key, value, a)
}
