package resilience

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Circuit breaker parameters from docs/failure-handling.md §2.4 and baseline §10. Every one of
// them is derived; the derivation is on the constant.
const (
	// BreakerWindow is 30 s rolling. Long enough to accumulate the 20-sample minimum at low
	// volume, short enough to react before the SLO error budget burns.
	BreakerWindow = 30 * time.Second

	// BreakerBuckets is 10, giving 3 s of resolution. Fixed-size and preallocated: a window
	// implemented as a slice of timestamped events allocates in proportion to traffic, which
	// means the breaker's own memory grows fastest exactly when the dependency is failing.
	BreakerBuckets = 10

	// BreakerMinimumSamples is 20, and it is the single most important parameter in this file.
	// Without it, one failure out of two requests is a 50 % error rate and opens the circuit on
	// a gateway that is completely fine — a breaker that trips on 2 of 2 is not a safety device,
	// it is an outage generator with a threshold. At 20 samples the binomial confidence
	// interval around a 25 % observed rate is narrow enough that acting on it is a decision
	// rather than a coin flip.
	BreakerMinimumSamples = 20

	// BreakerFailureRateToOpen is 0.25. Gateways decline 5–15 % of authorizations for ordinary
	// business reasons; those are excluded from the numerator entirely (see Classifier), so
	// what remains is transport failures, 5xx and timeouts. A quarter of *those* is
	// unambiguously broken.
	BreakerFailureRateToOpen = 0.25

	// BreakerDegradedRate is 0.05: advisory only. It feeds the routing engine's scores so a
	// wobbling gateway is deprioritized, and it maps to baseline §10's HEALTHY → DEGRADED edge.
	// It never opens the circuit, because excluding a gateway at a 5 % error rate throws away
	// 95 % of working capacity.
	BreakerDegradedRate = 0.05

	// BreakerSlowCallDuration is 5 s, the p99 latency threshold from baseline §10. A gateway at
	// a 5 s p99 will blow the 8 s hard timeout for a meaningful share of traffic, and each of
	// those becomes a TIMEOUT_UNKNOWN and a reconciliation cycle. Excluding it early is
	// strictly cheaper than timing out.
	BreakerSlowCallDuration = 5 * time.Second

	// BreakerSlowCallRate is 0.01, which is how "p99 > 5 s" is evaluated here: the p99 exceeds
	// 5 s exactly when more than 1 % of calls exceed 5 s. Counting calls over a threshold
	// costs one counter per bucket; computing a true p99 costs a histogram whose memory grows
	// with cardinality, for a number that is only ever compared against one threshold anyway.
	BreakerSlowCallRate = 0.01

	// BreakerBaseCooldown is 30 s: enough to cover a transient blip without hammering a
	// provider that is genuinely down.
	BreakerBaseCooldown = 30 * time.Second

	// BreakerMaxCooldown is 5 min. The doubling avoids hammering; the cap bounds how long a
	// recovered gateway stays excluded, because an uncapped doubling turns a ten-minute vendor
	// incident into an hour of self-inflicted exclusion.
	BreakerMaxCooldown = 5 * time.Minute

	// BreakerHalfOpenProbes is 1 concurrent probe. A half-open state that admits full traffic
	// re-breaks a gateway that is coming back with cold pools — the recovery attempt becomes
	// the second outage.
	BreakerHalfOpenProbes = 1

	// BreakerProbeSuccessesToClose is 3. One success could be luck: a single request landing on
	// the one healthy node behind a load balancer proves nothing about the other nine.
	BreakerProbeSuccessesToClose = 3
)

// State is the breaker's state, and its numeric values are the exported gauge values of
// pp_circuit_breaker_state{gateway,operation} — 0 CLOSED, 1 OPEN, 2 HALF_OPEN. They are fixed
// by the metric contract (baseline §22.2) and must not be reordered.
type State int32

const (
	// StateClosed passes traffic and counts outcomes.
	StateClosed State = 0
	// StateOpen fails fast with GATEWAY_CIRCUIT_OPEN and counts nothing: there is no traffic to
	// count, which is why recovery needs probes rather than observation.
	StateOpen State = 1
	// StateHalfOpen admits a bounded number of concurrent probes.
	StateHalfOpen State = 2
)

// String renders the state as it appears in baseline §10 and in the health event payload.
func (s State) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF_OPEN"
	default:
		return "UNKNOWN"
	}
}

// Gauge returns the value to publish for pp_circuit_breaker_state.
func (s State) Gauge() int { return int(s) }

// Health maps the breaker onto the gateway health state machine of baseline §10, which is what
// gateway.health_changed.v1 carries and what the routing engine consumes.
type Health string

// The health values of baseline §10.
const (
	HealthHealthy   Health = "HEALTHY"
	HealthDegraded  Health = "DEGRADED"
	HealthUnhealthy Health = "UNHEALTHY"
	HealthProbing   Health = "PROBING"
)

// Outcome is how one call is counted. The three-way split — rather than a bool — is the whole
// reason this breaker is safe to put in front of a payment gateway.
type Outcome int

const (
	// OutcomeSuccess counts toward the denominator and toward probe success.
	OutcomeSuccess Outcome = iota
	// OutcomeFailure counts toward both numerator and denominator.
	OutcomeFailure
	// OutcomeIgnore counts toward neither. A business decline is the canonical case.
	OutcomeIgnore
)

// Classifier decides how one call's error counts toward the breaker's error rate. It is a hook
// rather than a fixed rule because only the caller knows what "failure" means for its
// dependency, and getting it wrong is not a tuning problem, it is an incident.
//
// The rule this hook exists to enforce: **a business decline is not an availability failure.**
// A gateway that declines a card for insufficient funds, a stolen-card block or a risk rule did
// its job perfectly; the transaction failed, the *gateway* did not. Counting declines as
// breaker failures means the breaker's error rate tracks the merchant's customer mix rather
// than the gateway's health. The consequences are concrete and all bad:
//
//   - Card-testing traffic against one merchant (a burst of stolen-card declines, which is a
//     security event, not an availability event) drives the observed error rate past 25 % and
//     opens the circuit for *every* merchant on that gateway. One merchant's bad cohort becomes
//     a platform-wide gateway exclusion.
//   - A merchant onboarding a subprime or high-risk segment, whose decline rate is legitimately
//     40 %, opens the circuit on a gateway with zero errors, permanently, by operating normally.
//   - Traffic shifts to the fallback gateway, which declines the same cards for the same
//     reasons, so its circuit opens too — and then routing returns an empty plan and the
//     platform serves 503 NO_ELIGIBLE_GATEWAY while every gateway involved is perfectly healthy.
//   - Worse still on the way back: retrying a hard decline on another gateway is card-testing
//     behaviour and is exactly what gets a platform de-registered by the schemes
//     (baseline §9.1). A breaker that treats declines as errors is a breaker that actively
//     drives traffic toward that behaviour.
//
// So: timeouts and 5xx count; declines do not. This is stated as a table row in
// docs/failure-handling.md §2.4 ("Failure counting") and as a note on the state diagram in §7.1,
// and it is the one line in this file that must never be "simplified".
type Classifier func(err error) Outcome

// DefaultClassifier implements the rule above against the platform error model.
//
// Business, validation, authorization, not-found and conflict categories are ignored: they are
// statements about the *request*, and a breaker that opens on bad requests is a breaker a
// client can open on purpose. Gateway, timeout, infrastructure and rate-limit categories count
// as failures — the dependency could not do the work. Anything unclassified counts as a failure,
// on the same principle as retryability defaulting to false: the safe default is the one that
// assumes we do not understand what happened.
//
// context.Canceled is ignored, because the caller walked away and the dependency's health is
// unknown. context.DeadlineExceeded counts, because a dependency that did not answer in time is
// the exact condition the latency threshold exists to catch.
//
// GATEWAY_DECLINED is the one code that cannot be classified by its category. It sits in
// CategoryGateway because the refusal originated at the third party, but a hard decline is the
// gateway working perfectly — api/errors/catalog.yaml says so on the entry itself ("a decline is
// the gateway working correctly, so it is neither a 502 nor an alerting condition"), which is
// also why it alone in that category carries `retryable: false` and a 402. Classifying it by
// category alone is the bug this carve-out exists to prevent, and it is the exact bug described
// at length in the Classifier doc above: a card-testing burst against one merchant would open
// the circuit for every merchant on a gateway with zero errors.
func DefaultClassifier(err error) Outcome {
	if err == nil {
		return OutcomeSuccess
	}
	if errors.Is(err, context.Canceled) {
		return OutcomeIgnore
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return OutcomeFailure
	}
	if apierror.CodeOf(err) == apierror.CodeGatewayDeclined {
		return OutcomeIgnore
	}
	switch apierror.CategoryOf(err) {
	case apierror.CategoryBusinessRule,
		apierror.CategoryValidation,
		apierror.CategoryAuthentication,
		apierror.CategoryAuthorization,
		apierror.CategoryNotFound,
		apierror.CategoryConflict:
		return OutcomeIgnore
	default:
		return OutcomeFailure
	}
}

// BreakerConfig parameterizes a Breaker. The zero value is not useful; start from
// DefaultBreakerConfig, RedisBreakerConfig or VendorBreakerConfig.
type BreakerConfig struct {
	// Name labels the breaker in state-change callbacks and errors, conventionally
	// "gateway:operation".
	Name string

	Window                time.Duration
	Buckets               int
	MinimumSamples        int
	FailureRateToOpen     float64
	DegradedRate          float64
	SlowCallDuration      time.Duration
	SlowCallRateToOpen    float64
	BaseCooldown          time.Duration
	MaxCooldown           time.Duration
	HalfOpenProbes        int
	ProbeSuccessesToClose int

	// Classify decides what counts as a failure. Defaults to DefaultClassifier. Read the
	// Classifier doc before overriding it.
	Classify Classifier

	// Clock defaults to SystemClock.
	Clock Clock

	// StateChanged is invoked, outside the breaker's lock, on every state transition. It is how
	// the caller emits pp_circuit_breaker_state and publishes gateway.health_changed.v1
	// (baseline §10). It is called synchronously on the goroutine that caused the transition,
	// so it must not block: publishing an event from inside it means a slow broker adds latency
	// to a payment.
	StateChanged func(name string, from, to State)
}

// DefaultBreakerConfig is the gateway breaker: per (gateway_id, operation), with every
// parameter from docs/failure-handling.md §2.4.
func DefaultBreakerConfig(name string) BreakerConfig {
	return BreakerConfig{
		Name:                  name,
		Window:                BreakerWindow,
		Buckets:               BreakerBuckets,
		MinimumSamples:        BreakerMinimumSamples,
		FailureRateToOpen:     BreakerFailureRateToOpen,
		DegradedRate:          BreakerDegradedRate,
		SlowCallDuration:      BreakerSlowCallDuration,
		SlowCallRateToOpen:    BreakerSlowCallRate,
		BaseCooldown:          BreakerBaseCooldown,
		MaxCooldown:           BreakerMaxCooldown,
		HalfOpenProbes:        BreakerHalfOpenProbes,
		ProbeSuccessesToClose: BreakerProbeSuccessesToClose,
		Classify:              DefaultClassifier,
	}
}

// RedisBreakerConfig is the aggressive preset for Redis: a 5 s window, 10 samples, a 50 %
// failure rate and a 5 s cool-down.
//
// It is aggressive because the fallback is good — idempotency falls back to Postgres, which is
// authoritative anyway, and rate limiting falls back to a local bucket (F-7). When the fallback
// is nearly as correct as the primary, opening early costs almost nothing and saves the 50 ms
// per-operation timeout on every request.
//
// The document phrases the trigger as "10 failures in 5 s". It is implemented here as ≥ 10
// samples with > 50 % failures in a 5 s window, deliberately: an absolute count would open the
// breaker on 10 failures out of 10 000 healthy operations, which is a 0.1 % error rate and a
// working Redis.
func RedisBreakerConfig(name string) BreakerConfig {
	c := DefaultBreakerConfig(name)
	c.Window = 5 * time.Second
	c.Buckets = 5
	c.MinimumSamples = 10
	c.FailureRateToOpen = 0.50
	c.SlowCallDuration = 200 * time.Millisecond
	c.BaseCooldown = 5 * time.Second
	c.MaxCooldown = 30 * time.Second
	return c
}

// VendorBreakerConfig is the patient preset for external vendors (KYC, bank validation): 50 %
// over 60 s with a minimum of 10 samples.
//
// Patient because these calls are low-volume — 20 samples might take minutes to accumulate —
// and because there is no fallback. Opening a circuit you cannot route around only converts a
// slow onboarding into a failed one.
func VendorBreakerConfig(name string) BreakerConfig {
	c := DefaultBreakerConfig(name)
	c.Window = 60 * time.Second
	c.Buckets = 12
	c.MinimumSamples = 10
	c.FailureRateToOpen = 0.50
	c.SlowCallDuration = 30 * time.Second
	c.BaseCooldown = 30 * time.Second
	return c
}

func (c BreakerConfig) normalized() BreakerConfig {
	if c.Window <= 0 {
		c.Window = BreakerWindow
	}
	if c.Buckets <= 0 {
		c.Buckets = BreakerBuckets
	}
	if c.MinimumSamples <= 0 {
		c.MinimumSamples = BreakerMinimumSamples
	}
	if c.FailureRateToOpen <= 0 || c.FailureRateToOpen > 1 {
		c.FailureRateToOpen = BreakerFailureRateToOpen
	}
	if c.DegradedRate <= 0 || c.DegradedRate > 1 {
		c.DegradedRate = BreakerDegradedRate
	}
	if c.SlowCallDuration <= 0 {
		c.SlowCallDuration = BreakerSlowCallDuration
	}
	if c.SlowCallRateToOpen <= 0 || c.SlowCallRateToOpen > 1 {
		c.SlowCallRateToOpen = BreakerSlowCallRate
	}
	if c.BaseCooldown <= 0 {
		c.BaseCooldown = BreakerBaseCooldown
	}
	if c.MaxCooldown < c.BaseCooldown {
		c.MaxCooldown = max(BreakerMaxCooldown, c.BaseCooldown)
	}
	if c.HalfOpenProbes <= 0 {
		c.HalfOpenProbes = BreakerHalfOpenProbes
	}
	if c.ProbeSuccessesToClose <= 0 {
		c.ProbeSuccessesToClose = BreakerProbeSuccessesToClose
	}
	if c.Classify == nil {
		c.Classify = DefaultClassifier
	}
	c.Clock = orSystem(c.Clock)
	return c
}

// Counts is a snapshot of the rolling window.
type Counts struct {
	Successes int64
	Failures  int64
	Slow      int64
}

// Total returns the sample count the minimum-sample-size rule is applied to. Ignored outcomes
// are not in it, which is the point.
func (c Counts) Total() int64 { return c.Successes + c.Failures }

// ErrorRate returns failures/total, or 0 when there are no samples. Zero, not NaN: this value
// is compared against thresholds and exported as a metric, and NaN silently poisons both.
func (c Counts) ErrorRate() float64 {
	if c.Total() == 0 {
		return 0
	}
	return float64(c.Failures) / float64(c.Total())
}

// SlowRate returns slow-calls/total, the proxy for "p99 > threshold".
func (c Counts) SlowRate() float64 {
	if c.Total() == 0 {
		return 0
	}
	return float64(c.Slow) / float64(c.Total())
}

// Breaker is a circuit breaker over one dependency, keyed in practice by
// (gateway_id, operation) — the granularity of baseline §10, chosen because per-merchant
// samples are too sparse to be statistically meaningful and a global breaker would let one bad
// operation exclude a gateway's other, working operations.
//
// Safe for concurrent use. All state lives under one mutex; the critical sections are counter
// arithmetic over a fixed-size ring and are short enough that a mutex beats the sharded
// alternatives at the concurrency levels a single pod reaches (a 200-slot bulkhead sits in
// front of it).
type Breaker struct {
	cfg BreakerConfig

	mu     sync.Mutex
	state  State
	window *slidingWindow

	// generation invalidates permits issued before a state change. Without it, a probe issued
	// in HALF_OPEN that returns after the breaker has already reopened would be counted against
	// the new generation and could close a circuit on the strength of a call made under the old
	// one.
	generation uint64

	openedAt             time.Time
	cooldown             time.Duration
	probesInFlight       int
	consecutiveSuccesses int

	pending []transition
}

type transition struct {
	from, to State
}

// NewBreaker returns a breaker in CLOSED state.
func NewBreaker(cfg BreakerConfig) *Breaker {
	cfg = cfg.normalized()
	return &Breaker{
		cfg:      cfg,
		state:    StateClosed,
		window:   newSlidingWindow(cfg.Window, cfg.Buckets),
		cooldown: cfg.BaseCooldown,
	}
}

// ErrCircuitOpen is the sentinel behind the GATEWAY_CIRCUIT_OPEN error, so callers can branch
// with errors.Is without importing the apierror code.
var ErrCircuitOpen = errors.New("circuit breaker is open")

func (b *Breaker) openError() error {
	// GATEWAY_CIRCUIT_OPEN is retryable and maps to 503: the caller should route elsewhere or
	// try again, and the routing engine will already have excluded this (gateway, operation).
	return apierror.Wrapf(ErrCircuitOpen, apierror.CodeGatewayCircuitOpen,
		"circuit breaker %q is open", b.cfg.Name)
}

// Execute runs fn under the breaker, classifying the returned error with the configured
// Classifier.
//
// A panic inside fn is reported as a failure and re-panicked. Swallowing it would leave the
// half-open probe slot permanently occupied — the breaker would never close again, and the
// symptom would be "the gateway never recovers" rather than "there is a bug in the adapter".
func (b *Breaker) Execute(ctx context.Context, fn func(context.Context) error) (err error) {
	report, aerr := b.AllowClassified()
	if aerr != nil {
		return aerr
	}
	defer func() {
		if r := recover(); r != nil {
			report(apierror.New(apierror.CodeInternalError, "panic in circuit-protected call"))
			panic(r)
		}
		report(err)
	}()
	err = fn(ctx)
	return err
}

// Allow admits a call and returns the permit to report its outcome, for callers whose success
// or failure is not expressed as an error — a gateway adapter that has already normalized a
// response into a decision, for instance.
//
// permit(false) counts as a failure unconditionally: it bypasses the Classifier because the
// caller has already classified. A caller holding an error should use Execute or
// AllowClassified so the decline rule is applied for it.
//
// The permit is idempotent; calling it twice reports once. Callers must call it exactly once
// on every path, including panics — `defer permit(false)` and then overwrite is not possible,
// so prefer Execute unless the outcome genuinely arrives later.
func (b *Breaker) Allow() (permit func(success bool), err error) {
	report, err := b.allow()
	if err != nil {
		return nil, err
	}
	return func(success bool) {
		if success {
			report(OutcomeSuccess)
			return
		}
		report(OutcomeFailure)
	}, nil
}

// AllowClassified is Allow with the configured Classifier applied to the reported error, so a
// business decline reported through it does not count against the breaker.
func (b *Breaker) AllowClassified() (permit func(err error), err error) {
	report, err := b.allow()
	if err != nil {
		return nil, err
	}
	classify := b.cfg.Classify
	return func(callErr error) { report(classify(callErr)) }, nil
}

func (b *Breaker) allow() (func(Outcome), error) {
	b.mu.Lock()
	now := b.cfg.Clock.Now()
	b.maybeHalfOpenLocked(now)

	switch b.state {
	case StateOpen:
		pend := b.drainLocked()
		b.mu.Unlock()
		b.notify(pend)
		return nil, b.openError()
	case StateHalfOpen:
		if b.probesInFlight >= b.cfg.HalfOpenProbes {
			pend := b.drainLocked()
			b.mu.Unlock()
			b.notify(pend)
			return nil, b.openError()
		}
		b.probesInFlight++
	default:
		// StateClosed: the call is admitted with no accounting beyond the permit issued below
	}

	gen := b.generation
	half := b.state == StateHalfOpen
	pend := b.drainLocked()
	b.mu.Unlock()
	b.notify(pend)

	var once sync.Once
	return func(o Outcome) {
		once.Do(func() { b.report(gen, half, now, o) })
	}, nil
}

func (b *Breaker) report(gen uint64, half bool, start time.Time, o Outcome) {
	b.mu.Lock()
	now := b.cfg.Clock.Now()

	// A permit from a previous generation says nothing about the current one: the breaker has
	// tripped or closed since the call started, and its probe accounting has already been reset.
	if b.generation != gen {
		b.mu.Unlock()
		return
	}
	if half && b.probesInFlight > 0 {
		b.probesInFlight--
	}

	// A slow call counts toward the latency trigger whether or not it succeeded. A gateway that
	// answers correctly in nine seconds is still a gateway whose calls will start timing out
	// against the 8 s hard limit.
	slow := now.Sub(start) >= b.cfg.SlowCallDuration

	switch b.state {
	case StateHalfOpen:
		switch o {
		case OutcomeFailure:
			b.tripLocked(now)
		case OutcomeSuccess:
			b.consecutiveSuccesses++
			if b.consecutiveSuccesses >= b.cfg.ProbeSuccessesToClose {
				b.setStateLocked(StateClosed, now)
			}
		default:
			// Ignored outcomes release the probe slot without advancing or resetting the
			// consecutive-success count. A declined probe proves nothing either way, and
			// treating it as a failure would keep a healthy gateway open forever on a
			// merchant whose cards are being declined.
		}
	case StateClosed:
		if o == OutcomeIgnore {
			break
		}
		b.window.record(now, o, slow)
		b.evaluateLocked(now)
	default:
		// StateOpen. The permit was issued before the breaker tripped, so its outcome describes a
		// call the breaker has already stopped counting: recording it would let calls that were
		// in flight at the moment of the trip re-close the window they tripped. The permit is
		// still released above, which is the only bookkeeping an open breaker owes it.
	}

	pend := b.drainLocked()
	b.mu.Unlock()
	b.notify(pend)
}

func (b *Breaker) evaluateLocked(now time.Time) {
	c := b.window.counts(now)
	// The minimum sample size is checked first and is not negotiable. Everything below it is
	// noise, and acting on noise is how a breaker takes down a healthy dependency.
	if c.Total() < int64(b.cfg.MinimumSamples) {
		return
	}
	if c.ErrorRate() > b.cfg.FailureRateToOpen || c.SlowRate() > b.cfg.SlowCallRateToOpen {
		b.tripLocked(now)
	}
}

func (b *Breaker) tripLocked(now time.Time) {
	if b.state == StateHalfOpen {
		// A failed probe cycle doubles the cool-down: 30 s, 60 s, 120 s, 240 s, 300 s (capped).
		// Doubling is what stops a genuinely-down provider being probed every 30 s for an hour;
		// the cap is what stops a recovered one staying excluded for the rest of the day.
		b.cooldown = min(b.cooldown*2, b.cfg.MaxCooldown)
	} else {
		b.cooldown = b.cfg.BaseCooldown
	}
	b.setStateLocked(StateOpen, now)
}

func (b *Breaker) maybeHalfOpenLocked(now time.Time) {
	if b.state != StateOpen {
		return
	}
	if now.Sub(b.openedAt) >= b.cooldown {
		b.setStateLocked(StateHalfOpen, now)
	}
}

func (b *Breaker) setStateLocked(s State, now time.Time) {
	if b.state == s {
		return
	}
	from := b.state
	b.state = s
	// Every transition invalidates outstanding permits and resets probe accounting, so a call
	// in flight across a transition can neither close nor reopen the breaker.
	b.generation++
	b.probesInFlight = 0
	b.consecutiveSuccesses = 0

	switch s {
	case StateOpen:
		b.openedAt = now
	case StateClosed:
		// A closed breaker starts from a clean window and a reset cool-down. Carrying the
		// failures that opened it into the closed state would reopen it on the first error.
		b.cooldown = b.cfg.BaseCooldown
		b.window.reset()
	default:
		// StateHalfOpen needs neither: it records no opened-at instant, and its window is the closed
		// state's window, which must survive so a failed probe reopens on the evidence already there
	}
	b.pending = append(b.pending, transition{from: from, to: s})
}

func (b *Breaker) drainLocked() []transition {
	if len(b.pending) == 0 {
		return nil
	}
	t := b.pending
	b.pending = nil
	return t
}

// notify fires StateChanged outside the lock. Inside it, a callback that published an event or
// took another lock would be a deadlock waiting for the right interleaving.
func (b *Breaker) notify(ts []transition) {
	cb := b.cfg.StateChanged
	if cb == nil {
		return
	}
	for _, t := range ts {
		cb(b.cfg.Name, t.from, t.to)
	}
}

// State returns the current state, advancing OPEN → HALF_OPEN if the cool-down has elapsed so
// that a metric scrape reports what the next caller would actually see rather than a stale OPEN.
func (b *Breaker) State() State {
	b.mu.Lock()
	b.maybeHalfOpenLocked(b.cfg.Clock.Now())
	s := b.state
	pend := b.drainLocked()
	b.mu.Unlock()
	b.notify(pend)
	return s
}

// Counts snapshots the rolling window.
func (b *Breaker) Counts() Counts {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.window.counts(b.cfg.Clock.Now())
}

// Degraded reports the advisory 5 % condition from baseline §10: enough samples, error rate
// over the degraded threshold, circuit still closed. The routing engine deprioritizes a
// degraded gateway; it does not exclude it.
func (b *Breaker) Degraded() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != StateClosed {
		return false
	}
	c := b.window.counts(b.cfg.Clock.Now())
	return c.Total() >= int64(b.cfg.MinimumSamples) && c.ErrorRate() > b.cfg.DegradedRate
}

// Health projects the breaker onto the baseline §10 health state machine, which is the vocabulary
// gateway.health_changed.v1 and the routing engine speak.
func (b *Breaker) Health() Health {
	switch b.State() {
	case StateOpen:
		return HealthUnhealthy
	case StateHalfOpen:
		return HealthProbing
	default:
		if b.Degraded() {
			return HealthDegraded
		}
		return HealthHealthy
	}
}

// Cooldown returns the cool-down that will be applied on the next open. Exported so an operator
// can see how far the doubling has progressed without reading the logs.
func (b *Breaker) Cooldown() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cooldown
}

// Reset forces the breaker closed, clears the window and returns the cool-down to its base.
// For operator intervention ("the vendor says it is fixed, stop waiting") and for tests. It
// fires StateChanged like any other transition, because a manual reset the routing engine does
// not learn about is a manual reset that does nothing.
func (b *Breaker) Reset() {
	b.mu.Lock()
	b.setStateLocked(StateClosed, b.cfg.Clock.Now())
	b.window.reset()
	b.cooldown = b.cfg.BaseCooldown
	pend := b.drainLocked()
	b.mu.Unlock()
	b.notify(pend)
}

// Name returns the breaker's label.
func (b *Breaker) Name() string { return b.cfg.Name }

// --- sliding window ---------------------------------------------------------------------

// slidingWindow is a fixed-size ring of counter buckets. It allocates once, at construction,
// and never again: a window built from a slice of timestamped events grows with traffic, which
// means the breaker's memory footprint peaks at the same moment the dependency is failing and
// the pod is least able to absorb it.
//
// Not safe for concurrent use on its own; the Breaker's mutex is its synchronisation.
type slidingWindow struct {
	buckets []windowBucket
	width   time.Duration
	n       int64
}

type windowBucket struct {
	idx      int64 // absolute bucket index; -1 means never written
	success  int64
	failure  int64
	slowCall int64
}

func newSlidingWindow(window time.Duration, buckets int) *slidingWindow {
	if buckets <= 0 {
		buckets = BreakerBuckets
	}
	if window <= 0 {
		window = BreakerWindow
	}
	w := &slidingWindow{
		buckets: make([]windowBucket, buckets),
		width:   window / time.Duration(buckets),
		n:       int64(buckets),
	}
	if w.width <= 0 {
		w.width = time.Millisecond
	}
	w.reset()
	return w
}

func (w *slidingWindow) reset() {
	for i := range w.buckets {
		w.buckets[i] = windowBucket{idx: -1}
	}
}

func (w *slidingWindow) index(t time.Time) int64 {
	return t.UnixNano() / int64(w.width)
}

func (w *slidingWindow) slot(idx int64) *windowBucket {
	// The modulo is normalized because UnixNano is negative before 1970 and a negative slot
	// index panics. Cheap insurance against a clock that has been set catastrophically wrong.
	i := ((idx % w.n) + w.n) % w.n
	return &w.buckets[i]
}

func (w *slidingWindow) record(now time.Time, o Outcome, slow bool) {
	idx := w.index(now)
	b := w.slot(idx)
	if b.idx != idx {
		// Reusing the slot for a new bucket is the eviction: no allocation, no compaction, and
		// the oldest data disappears exactly when it leaves the window.
		*b = windowBucket{idx: idx}
	}
	switch o {
	case OutcomeSuccess:
		b.success++
	case OutcomeFailure:
		b.failure++
	default:
		return
	}
	if slow {
		b.slowCall++
	}
}

func (w *slidingWindow) counts(now time.Time) Counts {
	idx := w.index(now)
	oldest := idx - w.n + 1
	var c Counts
	for i := range w.buckets {
		b := &w.buckets[i]
		if b.idx < oldest || b.idx > idx {
			continue
		}
		c.Successes += b.success
		c.Failures += b.failure
		c.Slow += b.slowCall
	}
	return c
}

// --- registry ---------------------------------------------------------------------------

// BreakerRegistryConfig parameterizes BreakerRegistry.
type BreakerRegistryConfig struct {
	// MaxKeys bounds live breakers. Defaults to DefaultRegistryMaxKeys.
	MaxKeys int
	// IdleTTL evicts breakers untouched for this long. Defaults to DefaultRegistryIdleTTL.
	IdleTTL time.Duration
	// SweepInterval, when positive, starts a background sweeper goroutine that the registry
	// owns and Close stops. Leave it zero to rely purely on the lazy sweep in Get, which is
	// sufficient for any registry that keeps receiving traffic and cannot leak anything.
	SweepInterval time.Duration
	// Clock defaults to SystemClock.
	Clock Clock
	// Configure builds the config for a key on first use. Defaults to DefaultBreakerConfig.
	Configure func(key string) BreakerConfig
	// Evicted, if set, is called when a breaker is dropped, so the caller can retire its
	// metric series rather than leaving a stale gauge exported forever.
	Evicted func(key string)
}

// BreakerRegistry holds one Breaker per key, with a bounded key space.
//
// The bound is a security property, not housekeeping. The key is "gateway:operation" today, but
// any per-key registry whose keys derive from request data is an unbounded allocation an
// attacker can drive: a few hundred thousand requests naming distinct operations would
// otherwise create a few hundred thousand breakers, each holding a ring of buckets, until the
// pod is OOM-killed — a memory-exhaustion denial of service delivered through the component
// whose job is to prevent denial of service. MaxKeys plus LRU eviction bounds it; IdleTTL
// returns the memory when the traffic stops.
//
// Safe for concurrent use.
type BreakerRegistry struct {
	reg *keyedRegistry[*Breaker]
	cfg BreakerRegistryConfig
}

// NewBreakerRegistry returns a registry. If cfg.SweepInterval is positive the returned registry
// owns a goroutine and Close must be called.
func NewBreakerRegistry(cfg BreakerRegistryConfig) *BreakerRegistry {
	if cfg.Configure == nil {
		cfg.Configure = DefaultBreakerConfig
	}
	cfg.Clock = orSystem(cfg.Clock)
	r := &BreakerRegistry{cfg: cfg}
	var onEvict func(string, *Breaker)
	if cfg.Evicted != nil {
		onEvict = func(k string, _ *Breaker) { cfg.Evicted(k) }
	}
	r.reg = newKeyedRegistry[*Breaker](cfg.MaxKeys, cfg.IdleTTL, cfg.Clock, onEvict)
	r.reg.startSweeper(cfg.SweepInterval)
	return r
}

// BreakerKey builds the canonical registry key. Per baseline §10 the granularity is
// (gateway_id, operation) — not per merchant, whose samples are too sparse to be statistically
// meaningful, and not global, which would let one failing operation exclude a gateway's others.
func BreakerKey(gateway, operation string) string { return gateway + ":" + operation }

// Get returns the breaker for key, creating it on first use and refreshing its recency.
func (r *BreakerRegistry) Get(key string) *Breaker {
	cfg := r.cfg
	return r.reg.getOrCreate(key, func(k string) *Breaker {
		c := cfg.Configure(k)
		if c.Clock == nil {
			c.Clock = cfg.Clock
		}
		if c.Name == "" {
			c.Name = k
		}
		return NewBreaker(c)
	})
}

// Len returns the number of live breakers.
func (r *BreakerRegistry) Len() int { return r.reg.len() }

// EvictIdle drops breakers untouched for longer than IdleTTL and returns how many went.
// Exposed so a caller without a sweeper can drive eviction from its own housekeeping loop.
func (r *BreakerRegistry) EvictIdle() int { return r.reg.evictIdle() }

// States snapshots every live breaker's state, which is what a metrics collector iterates to
// publish pp_circuit_breaker_state.
func (r *BreakerRegistry) States() map[string]State {
	live := r.reg.snapshot()
	out := make(map[string]State, len(live))
	for k, b := range live {
		out[k] = b.State()
	}
	return out
}

// Close stops the sweeper goroutine, if one was started, and waits for it. Safe to call more
// than once and safe to call on a registry with no sweeper.
func (r *BreakerRegistry) Close() error {
	r.reg.close()
	return nil
}
