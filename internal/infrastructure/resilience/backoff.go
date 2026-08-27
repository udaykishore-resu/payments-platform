package resilience

import (
	"math/rand/v2"
	"sync"
	"time"
)

// The backoff parameters from docs/failure-handling.md §2.3. Each cap is derived from the
// budget of the layer that uses it, not picked for roundness.
const (
	// DefaultBackoffBase is 100 ms: below the typical recovery time of a gateway that dropped a
	// single request, so the first retry is cheap and usually succeeds.
	DefaultBackoffBase = 100 * time.Millisecond

	// DefaultBackoffMultiplier is 2: each attempt doubles the interval, which is what makes the
	// expected number of in-flight retries fall geometrically rather than linearly while a
	// dependency is down.
	DefaultBackoffMultiplier = 2.0

	// InRequestBackoffCap is 2 s. Derivation: the orchestrator's internal deadline is 18 s and
	// two gateway attempts consume 2 × 8 s = 16 s of it. What remains for *waiting* is ~2 s.
	// A larger cap would be a cap that the deadline check in Do can never honour.
	InRequestBackoffCap = 2 * time.Second

	// WorkflowBackoffCap is 60 s, for workflow activity retries (baseline §11). A workflow step
	// is not holding a request thread, so a longer wait costs nothing but latency on an
	// asynchronous path.
	WorkflowBackoffCap = 60 * time.Second

	// DLQBackoffCap is 30 m, the last rung of the retry-topic escalation
	// (5 s, 30 s, 2 m, 10 m, 30 m) in docs/failure-handling.md §6.1.
	DLQBackoffCap = 30 * time.Minute
)

// Backoff computes how long to wait before attempt n+1, given that attempt n failed.
//
// Attempt indices are zero-based: Delay(0) is the wait between the first and second calls.
// Implementations must be safe for concurrent use, because one Backoff is shared by every
// goroutine retrying against the same dependency — which is exactly the population whose
// synchronisation the jitter exists to break.
type Backoff interface {
	Delay(attempt int) time.Duration
}

// BackoffFunc adapts a function to the Backoff interface. Useful for fixed or scripted delays
// in tests, and for the header-driven delays of the DLQ retry topic.
type BackoffFunc func(attempt int) time.Duration

// Delay implements Backoff.
func (f BackoffFunc) Delay(attempt int) time.Duration { return f(attempt) }

// ExponentialBackoff implements exponential backoff with **full jitter**:
//
//	delay(n) = uniform_random(0, min(cap, base × multiplier^n))
//
// Why full jitter, and not no jitter or equal jitter, with the arithmetic:
//
// Take a fleet of 500 pods all calling one gateway, and suppose the gateway drops every
// request for 2 s and then recovers. All 500 calls fail inside the same few milliseconds,
// because they all failed for the same reason at the same instant. Now consider the retry:
//
//   - **No jitter** (delay = 100 ms exactly): all 500 retries land in the same millisecond.
//     Instantaneous arrival rate at the moment of recovery is 500 000 rps against a service
//     that has just come back with cold connection pools and cold caches. It fails again, and
//     the cohort — still perfectly in phase — retries together at 200 ms, then 400 ms. The
//     synchronisation is never broken; the dependency is re-broken by its own clients on every
//     doubling. This is the thundering herd, and it is *caused* by the retry policy.
//
//   - **Equal jitter** (delay = d/2 + uniform(0, d/2)): the 500 retries spread over
//     [50 ms, 100 ms]. Peak arrival density is 500 / 50 ms = 10 per ms — half the peak of no
//     jitter, which is an improvement. But two properties remain bad. First, the deterministic
//     floor means *nothing* arrives in [0, 50 ms): the first half of the dependency's recovered
//     capacity is provably wasted, and every client's completion time is 50 ms longer than it
//     needed to be. Second, the floor keeps the cohort phase-locked across attempts — the
//     window each round is a band whose position is fixed by the attempt number, so clients
//     that failed together keep retrying in overlapping bands round after round.
//
//   - **Full jitter** (delay = uniform(0, d)): the 500 retries spread over [0, 100 ms]. Peak
//     arrival density is 500 / 100 ms = 5 per ms, half of equal jitter and a hundredth of no
//     jitter, and the recovered capacity is used from the first millisecond. More importantly
//     the *variance* decorrelates the cohort permanently: after one round, the clients' phases
//     are independent draws, so a second failure does not re-synchronise them. AWS's published
//     analysis of the three strategies finds full jitter minimises both client-observed
//     completion time and server-observed contention simultaneously.
//
// The cost of full jitter is higher variance on any single retry — a particular call might wait
// 3 ms or 99 ms. That is irrelevant here: the alternative is a synchronized stampede against a
// dependency that has just recovered, and one client's p50 retry latency is not worth a second
// outage. Decorrelated jitter was also considered and rejected: it carries state per client,
// which makes it stateful across attempts and awkward to reason about at the point where
// somebody is trying to work out why a retry took as long as it did.
//
// Safe for concurrent use.
type ExponentialBackoff struct {
	base       time.Duration
	ceiling    time.Duration
	multiplier float64

	// mu guards rnd and is taken only in the deterministic (seeded) mode. In production mode
	// rnd is nil and Delay draws from math/rand/v2's per-P generator, which takes no lock at
	// all. docs/failure-handling.md §2.3 calls this out explicitly: a global, mutex-guarded RNG
	// becomes a contention point precisely during a retry storm, which is the one moment the
	// backoff must be cheap.
	mu  sync.Mutex
	rnd *rand.Rand
}

var _ Backoff = (*ExponentialBackoff)(nil)

// NewExponentialBackoff returns a full-jitter exponential backoff drawing from the runtime's
// per-P random source: no shared lock, and no reproducibility.
//
// Invalid parameters are corrected rather than rejected, because a backoff is constructed on
// startup paths where returning an error only moves the problem: a non-positive base becomes
// DefaultBackoffBase, a ceiling below the base becomes the base, and a multiplier below 1
// becomes 1 (which degenerates to a flat, fully-jittered interval — still safe).
func NewExponentialBackoff(base, ceiling time.Duration, multiplier float64) *ExponentialBackoff {
	if base <= 0 {
		base = DefaultBackoffBase
	}
	if ceiling < base {
		ceiling = base
	}
	if multiplier < 1 {
		multiplier = 1
	}
	return &ExponentialBackoff{base: base, ceiling: ceiling, multiplier: multiplier}
}

// NewSeededExponentialBackoff returns a backoff whose jitter is drawn from a PCG generator
// seeded with seed, making every Delay sequence exactly reproducible.
//
// This exists for tests and for the deterministic replay of an incident, not for production:
// seeding a fleet identically would reintroduce the very synchronisation the jitter removes.
// Two pods with the same seed retry in lockstep, which is worse than no jitter because it also
// looks random in the logs.
func NewSeededExponentialBackoff(base, ceiling time.Duration, multiplier float64, seed uint64) *ExponentialBackoff {
	b := NewExponentialBackoff(base, ceiling, multiplier)
	// The second PCG stream selector is derived from the seed so that a caller passing 1 and a
	// caller passing 2 get genuinely different streams rather than adjacent ones.
	b.rnd = rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))
	return b
}

// DefaultBackoff is the in-request policy: 100 ms base, 2 s cap, doubling. These are the
// numbers in the failover diagram (docs/failure-handling.md §7.2):
// backoff = uniform(0, min(2s, 100ms × 2^n)).
func DefaultBackoff() *ExponentialBackoff {
	return NewExponentialBackoff(DefaultBackoffBase, InRequestBackoffCap, DefaultBackoffMultiplier)
}

// Ceiling returns min(cap, base × multiplier^attempt): the exclusive upper bound of the
// interval Delay samples from, before jitter.
//
// Exported because it is the only honest way to assert a bound on a jittered value — a test
// that hard-codes "the third delay is under 800 ms" duplicates the formula and will disagree
// with it the first time a parameter changes.
func (b *ExponentialBackoff) Ceiling(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	// With multiplier ≥ 1 and a finite ceiling, 63 doublings is already past any representable
	// duration; clamping here keeps the loop O(1) for an absurd attempt count rather than
	// spinning for billions of iterations on a caller's arithmetic bug.
	if attempt > 63 {
		attempt = 63
	}
	limit := float64(b.ceiling)
	d := float64(b.base)
	for i := 0; i < attempt; i++ {
		d *= b.multiplier
		if d >= limit {
			return b.ceiling
		}
	}
	if d >= limit {
		return b.ceiling
	}
	return time.Duration(d)
}

// Delay returns uniform_random(0, Ceiling(attempt)] — full jitter. The interval is inclusive of
// the ceiling and of zero: a zero delay is a legitimate draw and is the reason full jitter uses
// the dependency's capacity from the first millisecond of its recovery.
func (b *ExponentialBackoff) Delay(attempt int) time.Duration {
	upper := b.Ceiling(attempt)
	if upper <= 0 {
		return 0
	}
	return time.Duration(b.int64n(int64(upper) + 1))
}

// Base returns the configured first-interval ceiling.
func (b *ExponentialBackoff) Base() time.Duration { return b.base }

// Cap returns the configured maximum interval.
func (b *ExponentialBackoff) Cap() time.Duration { return b.ceiling }

func (b *ExponentialBackoff) int64n(n int64) int64 {
	if b.rnd == nil {
		return rand.Int64N(n)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rnd.Int64N(n)
}

// FullJitter is the formula on its own, for callers that already own their delay bookkeeping —
// the DLQ scheduled consumer, which carries the attempt number in a message header and holds no
// Backoff value between messages.
//
// It is safe for concurrent use and takes no lock.
func FullJitter(attempt int, base, ceiling time.Duration) time.Duration {
	return NewExponentialBackoff(base, ceiling, DefaultBackoffMultiplier).Delay(attempt)
}
