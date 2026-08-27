package resilience

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Gradient2 parameters from docs/failure-handling.md §2.8.
const (
	// AdaptiveInitialLimit is 100: a sane cold start that converges within a few seconds of
	// traffic in either direction.
	AdaptiveInitialLimit = 100

	// AdaptiveMinLimit is 20. Below this the service is useless anyway; it is better to shed
	// explicitly and stay responsive to the requests we do accept than to admit five at a time
	// and time out on all of them.
	AdaptiveMinLimit = 20

	// AdaptiveMaxLimit is 2 000. It bounds goroutine and memory growth regardless of what the
	// algorithm believes: the limiter's model of the world is an inference from latency, and an
	// inference is not a reason to allocate unboundedly.
	AdaptiveMaxLimit = 2000

	// AdaptiveSmoothing is α = 0.2 — five adjustment periods to converge. Fast enough to react
	// to a real change, slow enough not to chase the noise in a one-second p50.
	AdaptiveSmoothing = 0.2

	// AdaptiveGradientFloor is 0.5, capping any single adjustment at a 50 % reduction. A
	// sharper cut overshoots, which starves the service, which drops the latency, which raises
	// the limit again — the limiter oscillates instead of converging.
	AdaptiveGradientFloor = 0.5

	// AdaptiveQueueRatio is 0.5: the admission queue may be at most half the concurrency limit.
	// A queue longer than that adds more latency than throughput — the marginal request waits
	// longer than it would have taken to be served after a reject and a retry.
	AdaptiveQueueRatio = 0.5

	// AdaptiveSampleWindow is 1 s, the short window over which the current RTT p50 is taken.
	AdaptiveSampleWindow = time.Second

	// AdaptiveMinSamples is 10 per adjustment. Recomputing the limit from two samples is the
	// same statistical error as opening a circuit on two requests.
	AdaptiveMinSamples = 10

	// AdaptiveNoLoadWindow is 5 min: the horizon over which the best-observed RTT is retained
	// as the "no load" baseline. Long enough that a sustained brownout cannot quietly redefine
	// "fast" as the brownout's latency, short enough that a genuine improvement (a scale-up, a
	// gateway fixing itself) is adopted.
	AdaptiveNoLoadWindow = 5 * time.Minute

	// AdaptiveNoLoadBuckets is 10, so the no-load minimum ages out in 30-second steps rather
	// than all at once.
	AdaptiveNoLoadBuckets = 10

	// adaptiveSampleCapacity bounds the short-window reservoir. Fixed so that a burst of
	// 50 000 requests in one second costs the same memory as a burst of 50.
	adaptiveSampleCapacity = 256
)

// AdaptiveConfig parameterizes an AdaptiveLimiter. The zero value is filled from the constants
// above by NewAdaptiveLimiter.
type AdaptiveConfig struct {
	Name string

	InitialLimit  int
	MinLimit      int
	MaxLimit      int
	Smoothing     float64
	GradientFloor float64
	QueueRatio    float64
	SampleWindow  time.Duration
	MinSamples    int
	NoLoadWindow  time.Duration
	NoLoadBuckets int

	Clock Clock

	// LimitChanged is called outside the lock whenever the computed limit changes, for
	// pp_adaptive_limit. It must not block.
	LimitChanged func(name string, from, to int)
}

func (c AdaptiveConfig) normalized() AdaptiveConfig {
	if c.MinLimit <= 0 {
		c.MinLimit = AdaptiveMinLimit
	}
	if c.MaxLimit <= 0 {
		c.MaxLimit = AdaptiveMaxLimit
	}
	if c.MaxLimit < c.MinLimit {
		c.MaxLimit = c.MinLimit
	}
	if c.InitialLimit <= 0 {
		c.InitialLimit = AdaptiveInitialLimit
	}
	c.InitialLimit = min(max(c.InitialLimit, c.MinLimit), c.MaxLimit)
	if c.Smoothing <= 0 || c.Smoothing > 1 {
		c.Smoothing = AdaptiveSmoothing
	}
	if c.GradientFloor <= 0 || c.GradientFloor >= 1 {
		c.GradientFloor = AdaptiveGradientFloor
	}
	if c.QueueRatio <= 0 {
		c.QueueRatio = AdaptiveQueueRatio
	}
	if c.SampleWindow <= 0 {
		c.SampleWindow = AdaptiveSampleWindow
	}
	if c.MinSamples <= 0 {
		c.MinSamples = AdaptiveMinSamples
	}
	if c.NoLoadWindow <= 0 {
		c.NoLoadWindow = AdaptiveNoLoadWindow
	}
	if c.NoLoadBuckets <= 0 {
		c.NoLoadBuckets = AdaptiveNoLoadBuckets
	}
	c.Clock = orSystem(c.Clock)
	return c
}

// AdaptiveLimiter is a Gradient2 adaptive concurrency limiter.
//
// A static concurrency limit is wrong at least half the time: set too low it wastes capacity
// that exists, set too high it queues work the service cannot do and converts a throughput
// problem into a timeout problem. The right number changes with the instance type, the
// co-tenants on the node, the gateway's current latency and whether the cache is warm — none of
// which a configuration file knows. So the limiter measures and adapts.
//
// **The algorithm**, per docs/failure-handling.md §2.8:
//
//	rtt_noload  = the minimum RTT observed over a long window (5 min, in 30 s buckets)
//	rtt_current = the p50 RTT over a short window (1 s)
//	gradient    = clamp(rtt_noload / rtt_current, GradientFloor, 1.0)
//	queue       = ceil(sqrt(limit))
//	limit(t+1)  = limit(t) × (1 − α) + α × (limit(t) × gradient + queue)
//
// Reading each term:
//
//   - The **gradient** is the ratio of the service's best-ever response time to its current
//     one. At gradient 1 the service is answering as fast as it ever has, so there is no
//     evidence of queueing and the limit may grow. At gradient 0.5 requests are taking twice as
//     long as they can, which by Little's Law means work is queued somewhere, and the limit must
//     come down. It is clamped at GradientFloor so no single adjustment cuts by more than half.
//   - The **queue allowance** is what makes the limiter *probe*. With gradient exactly 1 the
//     bare formula is a fixed point and the limit would never grow, so a service that scaled up
//     would stay pinned at the limit it discovered before the scale-up. Adding ceil(sqrt(limit))
//     makes the limit creep upward whenever latency is flat: +10 at limit 100, +45 at limit
//     2 000 — sub-linear, so the probe is a large fraction of a small limit and a small fraction
//     of a large one, which is the right shape for a search.
//   - **α = 0.2** smooths. Without it, one slow second would halve the limit and one fast second
//     would restore it, and the limiter would spend its life oscillating around the answer
//     instead of sitting on it.
//   - A **dropped** request (a timeout, a rejection, a connection failure) forces the gradient
//     to the floor for that period regardless of the measured RTT. A dropped request has no RTT
//     to measure and its absence would otherwise read as "no evidence", when it is the
//     strongest evidence available.
//
// **Why latency and not error rate is the signal.** Rising latency precedes errors: a saturated
// service queues before it times out, and it times out before it 500s. A limiter driven by
// errors reduces load only after the failures have started, which is after the point where
// reducing load still helps — the timeouts have already produced retries, the retries have
// already produced more load, and the amplification is running. Latency is the leading
// indicator, and acting on a leading indicator is the entire reason to have an adaptive limiter
// rather than a circuit breaker.
//
// Safe for concurrent use. Owns no goroutine: every window advances lazily inside Acquire and
// the release closure, which is what makes it deterministic under a ManualClock.
type AdaptiveLimiter struct {
	cfg AdaptiveConfig

	mu       sync.Mutex
	limit    float64
	inFlight int

	// short window
	samples     [adaptiveSampleCapacity]time.Duration
	nSamples    int
	writeIdx    int
	dropped     int
	windowStart time.Time

	// long-window rolling minimum
	minBuckets []minBucket
	minWidth   time.Duration

	pendingLimit []limitChange
	scratch      []time.Duration
}

type minBucket struct {
	idx int64
	min time.Duration
}

type limitChange struct{ from, to int }

// NewAdaptiveLimiter returns a limiter at cfg.InitialLimit.
func NewAdaptiveLimiter(cfg AdaptiveConfig) *AdaptiveLimiter {
	cfg = cfg.normalized()
	a := &AdaptiveLimiter{
		cfg:         cfg,
		limit:       float64(cfg.InitialLimit),
		windowStart: cfg.Clock.Now(),
		minBuckets:  make([]minBucket, cfg.NoLoadBuckets),
		minWidth:    cfg.NoLoadWindow / time.Duration(cfg.NoLoadBuckets),
		scratch:     make([]time.Duration, 0, adaptiveSampleCapacity),
	}
	if a.minWidth <= 0 {
		a.minWidth = time.Millisecond
	}
	for i := range a.minBuckets {
		a.minBuckets[i] = minBucket{idx: -1}
	}
	return a
}

// Acquire admits a request if in-flight work is below the current limit, returning the release
// function that reports the outcome.
//
// There is no wait: a caller that cannot be admitted is rejected immediately with
// CONCURRENCY_LIMIT_EXCEEDED (429, retryable). Queueing here would defeat the purpose — the
// limit exists because the service is at capacity, and a queue in front of a service at
// capacity is the latency-then-timeout-then-retry chain in docs/failure-handling.md §5.2. The
// admission queue that *does* exist lives at ingress, is bounded at limit × QueueRatio, and is
// a Bulkhead.
//
// release takes the measured round-trip time and whether the request was dropped. It is
// idempotent, so `defer release(...)` is safe on every path; a leaked permit would lower the
// effective limit forever, and a double release would raise it.
func (a *AdaptiveLimiter) Acquire(ctx context.Context) (release func(rtt time.Duration, dropped bool), err error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}

	a.mu.Lock()
	if float64(a.inFlight) >= a.limit {
		limit := int(a.limit)
		inFlight := a.inFlight
		a.mu.Unlock()
		return nil, apierror.Newf(apierror.CodeConcurrencyLimitExceeded,
			"adaptive limiter %q is at its concurrency limit (%d in flight, limit %d)",
			a.cfg.Name, inFlight, limit).WithRetryAfter(1)
	}
	a.inFlight++
	a.mu.Unlock()

	var once sync.Once
	return func(rtt time.Duration, dropped bool) {
		once.Do(func() {
			a.mu.Lock()
			a.inFlight--
			a.mu.Unlock()
			a.Update(rtt, dropped)
		})
	}, nil
}

// Update records one sample without going through Acquire, for callers whose concurrency is
// bounded elsewhere but who still want the limiter's view of latency — a gateway adapter behind
// a bulkhead, for instance.
func (a *AdaptiveLimiter) Update(rtt time.Duration, dropped bool) {
	a.mu.Lock()
	now := a.cfg.Clock.Now()

	if rtt > 0 {
		a.samples[a.writeIdx] = rtt
		a.writeIdx = (a.writeIdx + 1) % adaptiveSampleCapacity
		if a.nSamples < adaptiveSampleCapacity {
			a.nSamples++
		}
		a.recordMinLocked(now, rtt)
	}
	if dropped {
		a.dropped++
	}

	if !a.windowElapsedLocked(now) {
		a.mu.Unlock()
		return
	}
	// Not enough evidence yet: leave the window open rather than adjusting on noise. The window
	// therefore stretches on a low-traffic path, which is correct — the limit there is not the
	// constraint anyway.
	if a.nSamples < a.cfg.MinSamples && a.dropped == 0 {
		a.mu.Unlock()
		return
	}

	a.adjustLocked(now)
	pend := a.pendingLimit
	a.pendingLimit = nil
	a.mu.Unlock()
	a.notifyLimit(pend)
}

func (a *AdaptiveLimiter) windowElapsedLocked(now time.Time) bool {
	return now.Sub(a.windowStart) >= a.cfg.SampleWindow
}

func (a *AdaptiveLimiter) adjustLocked(now time.Time) {
	gradient := 1.0
	switch {
	case a.dropped > 0:
		// A drop is the strongest signal available and it has no RTT. Treat it as the maximum
		// permitted reduction rather than letting the surviving (fast) requests argue that
		// everything is fine.
		gradient = a.cfg.GradientFloor
	default:
		short := a.medianLocked()
		noLoad := a.noLoadMinLocked(now)
		if short > 0 && noLoad > 0 {
			gradient = float64(noLoad) / float64(short)
			gradient = min(max(gradient, a.cfg.GradientFloor), 1.0)
		}
	}

	queue := math.Ceil(math.Sqrt(a.limit))
	next := a.limit*(1-a.cfg.Smoothing) + a.cfg.Smoothing*(a.limit*gradient+queue)
	next = min(max(next, float64(a.cfg.MinLimit)), float64(a.cfg.MaxLimit))

	before := int(a.limit)
	a.limit = next
	if after := int(a.limit); after != before {
		a.pendingLimit = append(a.pendingLimit, limitChange{from: before, to: after})
	}

	a.nSamples = 0
	a.writeIdx = 0
	a.dropped = 0
	a.windowStart = now
}

// medianLocked returns the p50 of the short window. It sorts a fixed-capacity scratch slice
// reused across adjustments, so the p50 costs no allocation on a path that runs once a second
// per route class.
func (a *AdaptiveLimiter) medianLocked() time.Duration {
	if a.nSamples == 0 {
		return 0
	}
	a.scratch = a.scratch[:0]
	a.scratch = append(a.scratch, a.samples[:a.nSamples]...)
	sort.Slice(a.scratch, func(i, j int) bool { return a.scratch[i] < a.scratch[j] })
	return a.scratch[len(a.scratch)/2]
}

func (a *AdaptiveLimiter) recordMinLocked(now time.Time, rtt time.Duration) {
	idx := now.UnixNano() / int64(a.minWidth)
	n := int64(len(a.minBuckets))
	slot := &a.minBuckets[((idx%n)+n)%n]
	if slot.idx != idx {
		*slot = minBucket{idx: idx, min: rtt}
		return
	}
	if rtt < slot.min {
		slot.min = rtt
	}
}

func (a *AdaptiveLimiter) noLoadMinLocked(now time.Time) time.Duration {
	idx := now.UnixNano() / int64(a.minWidth)
	oldest := idx - int64(len(a.minBuckets)) + 1
	var best time.Duration
	for i := range a.minBuckets {
		b := a.minBuckets[i]
		if b.idx < oldest || b.idx > idx || b.min <= 0 {
			continue
		}
		if best == 0 || b.min < best {
			best = b.min
		}
	}
	return best
}

func (a *AdaptiveLimiter) notifyLimit(changes []limitChange) {
	cb := a.cfg.LimitChanged
	if cb == nil {
		return
	}
	for _, c := range changes {
		cb(a.cfg.Name, c.from, c.to)
	}
}

// Limit returns the current concurrency ceiling.
func (a *AdaptiveLimiter) Limit() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return int(a.limit)
}

// InFlight returns the number of admitted, unreleased requests.
func (a *AdaptiveLimiter) InFlight() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inFlight
}

// NoLoadRTT returns the current long-window minimum — the "as fast as this service has ever
// been" baseline the gradient is measured against. Exported because when the limiter behaves
// unexpectedly this is always the first number to look at: a baseline learned during a warm
// cache makes every subsequent second look like a brownout.
func (a *AdaptiveLimiter) NoLoadRTT() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.noLoadMinLocked(a.cfg.Clock.Now())
}

// Pressure returns in-flight ÷ limit, in [0, ∞). This is the signal a Shedder consumes: the
// degradation ladder's rungs are expressed as limiter queue percentages, and a queue of
// limit × QueueRatio at pressure p is the same statement as this ratio.
func (a *AdaptiveLimiter) Pressure() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.limit <= 0 {
		return 1
	}
	return float64(a.inFlight) / a.limit
}

// QueueCapacity returns limit × QueueRatio: how many callers may wait at ingress before the
// shedder starts rejecting. Half the concurrency, per §2.8 — beyond that a queue adds more
// latency than throughput.
func (a *AdaptiveLimiter) QueueCapacity() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return int(math.Ceil(a.limit * a.cfg.QueueRatio))
}

// Name returns the limiter's label.
func (a *AdaptiveLimiter) Name() string { return a.cfg.Name }
