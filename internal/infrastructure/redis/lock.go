package redis

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// releaseScript deletes a lock key only if it still holds our token.
//
// # Why this cannot be a plain DEL
//
// The failure is not exotic, it is routine. A holder takes the lock with a 30-second TTL. Its
// process stops for 31 seconds — a stop-the-world GC pause, a CPU-throttled container, a blocked
// disk write, a VM live-migration. The TTL expires; another instance takes the lock legitimately;
// the first instance wakes up, believes it still holds the lock, and calls DEL. It has now
// released *someone else's* lock, and a third instance can take it while the second is still
// running. Two holders, no error, no log line.
//
// Comparing the token makes the release conditional on still being the owner, and the comparison
// and the delete happen inside one Lua script so that nothing can intervene between them — a
// GET-then-DEL from the client has exactly the same race one round trip smaller.
//
// Returns 1 if the lock was released by us, 0 if it was not ours (or had already expired).
const releaseScript = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
end
return 0
`

// extendScript refreshes the TTL only if we still hold the lock. Same reasoning as release: an
// unconditional PEXPIRE would extend whatever holder happens to be there now.
//
// Returns 1 if extended, 0 if the lock is no longer ours.
const extendScript = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`

// tokenBytes is the length of a lock token. 16 bytes from crypto/rand is 128 bits: the chance of
// two holders minting the same token is not a thing that happens. A counter or a hostname would
// be cheaper and would defeat the entire point of the token comparison, because two processes on
// one host would compare equal.
const tokenBytes = 16

// Lock is a single-instance Redlock-style distributed lock. It implements ports.DistributedLock.
//
// # READ THIS BEFORE USING IT
//
// **This lock is an efficiency mechanism. It is never a correctness mechanism, and it must never
// appear on the money path.**
//
// The reason is not that this implementation is weak — the token comparison and the atomic
// release below are the correct way to build one. The reason is that *no* lease-based lock can be
// a correctness mechanism, and the argument is short:
//
//  1. A lock is held for a TTL. The holder cannot know whether it still holds it, because between
//     checking and acting an arbitrary amount of wall-clock time can pass — a GC pause, a CPU
//     quota exhaustion, a hypervisor migration, a network partition that delays the write.
//  2. So the lock's guarantee is "at most one holder, unless something paused", and "unless
//     something paused" is not a guarantee. It is a probability.
//  3. Anything whose correctness depends on it therefore has a failure mode that is invisible in
//     testing, rare in staging, and expensive exactly once in production.
//
// What makes the payment path safe is not this type. It is the database: the partial unique index
// that permits at most one successful attempt per payment (I3), the check that refunds cannot
// exceed captures (I1), the optimistic-concurrency version column. Those do not evaporate when a
// TTL expires mid-operation, and they hold no matter how many processes believe they hold a lock.
//
// Legitimate uses: making sure a scheduled sweep usually runs on one replica rather than on
// thirty; keeping a cache-warm job from being done six times; serialising a report generation.
// In every one of those, the worst case of two holders is wasted work, not wrong money.
//
// Illegitimate uses, without exception: guarding a capture, a refund, a ledger posting, an
// idempotency decision, or any state transition whose double execution would move money.
//
// Safe for concurrent use.
type Lock struct {
	rdb    UniversalClient
	tenant TenantResolver

	release *goredis.Script
	extend  *goredis.Script

	// minTTL floors the TTL. A lock with a one-millisecond TTL is not a lock, it is a race with
	// extra steps, and it is the kind of value that arrives from a misconfigured duration.
	minTTL time.Duration
	// maxTTL caps it. A lock held for an hour by a crashed process blocks the work for an hour.
	maxTTL time.Duration
}

// LockOption configures a Lock.
type LockOption func(*Lock)

// WithLockTenantResolver replaces the default tenant source.
func WithLockTenantResolver(r TenantResolver) LockOption {
	return func(l *Lock) {
		if r != nil {
			l.tenant = r
		}
	}
}

// WithLockTTLBounds sets the floor and cap applied to every requested TTL.
func WithLockTTLBounds(minTTL, maxTTL time.Duration) LockOption {
	return func(l *Lock) {
		if minTTL > 0 {
			l.minTTL = minTTL
		}
		if maxTTL > 0 {
			l.maxTTL = maxTTL
		}
	}
}

// NewLock builds a lock over a client.
func NewLock(client UniversalClient, opts ...LockOption) *Lock {
	l := &Lock{
		rdb:     client,
		tenant:  TenantFromTelemetry,
		release: goredis.NewScript(releaseScript),
		extend:  goredis.NewScript(extendScript),
		minTTL:  time.Second,
		maxTTL:  5 * time.Minute,
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Key renders the Redis key for a lock name.
func (l *Lock) Key(ctx context.Context, name string) (string, error) {
	tenant, ok := l.tenant(ctx)
	if !ok {
		return "", apierror.Wrap(ErrNoTenant, apierror.CodeMissingTenantContext,
			"redis: lock without a tenant in context")
	}
	return BuildKey(tenant, NamespaceLock, name)
}

// Acquire attempts to take the lock, returning a release function and whether it was acquired.
//
// `SET key token NX PX ttl` is one round trip and is atomic: NX makes the write conditional on
// the key not existing, and PX sets the expiry in the same command. The two-command version —
// SETNX then EXPIRE — has a window in which the process dies between them and leaves a lock with
// no expiry, held forever, which requires a human with redis-cli to clear.
//
// A Redis failure returns (nil, false, err) — not acquired. The caller decides what that means,
// and for every legitimate use of this type the answer is "skip this run; the scheduler will call
// again". Returning "acquired" on error would be the one way to make a lock outage dangerous.
func (l *Lock) Acquire(ctx context.Context, key string, ttl time.Duration) (func(context.Context) error, bool, error) {
	full, err := l.Key(ctx, key)
	if err != nil {
		return nil, false, err
	}
	token, err := newToken()
	if err != nil {
		return nil, false, err
	}
	ttl = l.boundTTL(ttl)

	ok, err := l.rdb.SetArgs(ctx, full, token, goredis.SetArgs{Mode: "NX", TTL: ttl}).Result()
	switch {
	case errors.Is(err, goredis.Nil):
		// SET NX returning nil means the key existed: somebody else holds it. Not an error.
		return nil, false, nil
	case err != nil:
		return nil, false, wrapRedis(err, "lock acquire")
	case ok != "OK":
		return nil, false, nil
	}

	var once sync.Once
	release := func(rctx context.Context) error {
		var rerr error
		once.Do(func() {
			// context.WithoutCancel: release must still run when the work's context was canceled,
			// which is precisely the case where holding the lock until its TTL is most annoying.
			// Bounded so a Redis stall cannot hang a shutdown.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(rctx), 5*time.Second)
			defer cancel()
			rerr = l.releaseToken(rctx, full, token)
		})
		return rerr
	}
	return release, true, nil
}

// releaseToken runs the compare-and-delete script.
func (l *Lock) releaseToken(ctx context.Context, full, token string) error {
	n, err := l.release.Run(ctx, l.rdb, []string{full}, token).Int64()
	if err != nil && !errors.Is(err, goredis.Nil) {
		return wrapRedis(err, "lock release")
	}
	if n == 0 {
		// We no longer held it: the TTL expired while we worked, and somebody else may now hold
		// it. This is worth surfacing rather than swallowing — it means the operation took longer
		// than its lock's TTL, which is the condition under which two holders exist.
		return apierror.Newf(apierror.CodeConcurrencyLimitExceeded,
			"redis: lock %s was no longer held at release; the operation outlived its TTL and may have overlapped another holder", full)
	}
	return nil
}

// AutoExtendingLock is a lock that renews its own lease while the work runs.
//
// It exists for the legitimate case where the work's duration is unpredictable — a nightly
// reconciliation over a variable number of rows — and a TTL long enough for the worst case would
// mean a crashed holder blocks the job for that whole worst case.
//
// It changes nothing about the warning on Lock. Auto-extension makes the lock *less* likely to be
// held by two processes, not incapable of it: the renewal itself can be delayed by exactly the
// pause it is trying to survive, and the extension script is conditional precisely because it can
// arrive after somebody else has taken over.
//
// Goroutine ownership: Acquire starts exactly one renewal goroutine, and it is owned by the
// returned release function. It stops on release, on context cancellation, or when a renewal
// finds the lock is no longer ours — never later, and never on its own timer alone.
type AutoExtendingLock struct {
	lock *Lock
	// interval is how often the lease is renewed, as a fraction of the TTL. A third means two
	// renewals can be lost before the lease lapses.
	fraction int
	// onLost is called when a renewal finds the lock is no longer held. The work should stop; this
	// type cannot stop it, which is another way of saying the lock is not a correctness mechanism.
	onLost func(key string)
}

// NewAutoExtendingLock wraps a Lock with lease renewal.
func NewAutoExtendingLock(l *Lock, onLost func(key string)) *AutoExtendingLock {
	return &AutoExtendingLock{lock: l, fraction: 3, onLost: onLost}
}

// Acquire takes the lock and keeps renewing it until the returned release is called.
//
// The release function stops the renewal goroutine and then releases the lock, in that order: a
// release that ran first would race its own renewal and could re-create a key it had just
// deleted, leaving a lock nobody holds until its TTL expires.
func (a *AutoExtendingLock) Acquire(ctx context.Context, key string, ttl time.Duration) (func(context.Context) error, bool, error) {
	full, err := a.lock.Key(ctx, key)
	if err != nil {
		return nil, false, err
	}
	release, acquired, err := a.lock.Acquire(ctx, key, ttl)
	if !acquired || err != nil {
		return release, acquired, err
	}
	ttl = a.lock.boundTTL(ttl)

	token, err := a.currentToken(ctx, full)
	if err != nil {
		// We hold it but cannot read our own token back. Give the lock up rather than run
		// unrenewed and unaware.
		_ = release(ctx)
		return nil, false, err
	}

	stop := make(chan struct{})
	var stopOnce sync.Once
	done := make(chan struct{})

	go func() {
		defer close(done)
		interval := ttl / time.Duration(a.fraction)
		if interval <= 0 {
			interval = time.Millisecond
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				// The work's context ended; the renewal has no reason to continue. The lock's own
				// TTL is the backstop if release is never called.
				return
			case <-t.C:
				ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
				n, err := a.lock.extend.Run(ectx, a.lock.rdb, []string{full}, token, ttl.Milliseconds()).Int64()
				cancel()
				if err == nil && n == 1 {
					continue
				}
				// Either Redis is unreachable or the lock is somebody else's now. Stop renewing
				// and tell the owner; continuing to try would be pretending.
				if a.onLost != nil {
					a.onLost(full)
				}
				return
			}
		}
	}()

	return func(rctx context.Context) error {
		stopOnce.Do(func() {
			close(stop)
			<-done
		})
		return release(rctx)
	}, true, nil
}

// currentToken reads back the token we just wrote, so the renewal script can compare against it.
func (a *AutoExtendingLock) currentToken(ctx context.Context, full string) (string, error) {
	v, err := a.lock.rdb.Get(ctx, full).Result()
	if err != nil {
		return "", wrapRedis(err, "lock token read")
	}
	return v, nil
}

func (l *Lock) boundTTL(ttl time.Duration) time.Duration {
	if ttl < l.minTTL {
		return l.minTTL
	}
	if ttl > l.maxTTL {
		return l.maxTTL
	}
	return ttl
}

// newToken mints a random lock token.
func newToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", apierror.Wrap(err, apierror.CodeInternalError, "redis: generating a lock token")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

var (
	_ ports.DistributedLock = (*Lock)(nil)
	_ ports.DistributedLock = (*AutoExtendingLock)(nil)
)
