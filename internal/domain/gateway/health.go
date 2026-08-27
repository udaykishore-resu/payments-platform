package gateway

import (
	"math"
	"slices"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// HealthState is the circuit state for one (gateway, operation) pair.
//
// Per (gateway, operation) and deliberately not per merchant: baseline §10. Per-merchant samples
// are too sparse to be statistically meaningful — a merchant doing forty payments an hour never
// accumulates twenty samples in a thirty-second window, so a per-merchant circuit would either
// never trip or trip on noise. Splitting by operation, on the other hand, is essential: a gateway
// whose refund endpoint is broken while authorization is fine is a real and common failure, and a
// single circuit would either stop all traffic or none of it.
type HealthState string

const (
	// HealthHealthy is the normal state: dispatch freely.
	HealthHealthy HealthState = "HEALTHY"

	// HealthDegraded means the error rate has crossed the first threshold. Traffic still flows —
	// this is a scoring signal for the router, not a gate — because at a 6% error rate, 94% of
	// payments still succeed, and refusing all of them to avoid the 6% is a worse trade.
	HealthDegraded HealthState = "DEGRADED"

	// HealthUnhealthy is the open circuit: no traffic is dispatched. It exists to stop the
	// platform spending its own latency budget and its connection pool on a gateway that is
	// answering with errors, and to stop it hammering a gateway that may be recovering.
	HealthUnhealthy HealthState = "UNHEALTHY"

	// HealthProbing is the half-open circuit: a limited amount of traffic is allowed through to
	// find out whether the gateway has recovered. Without it, the only way out of an open circuit
	// is a timer, and a timer cannot tell the difference between a gateway that recovered in five
	// seconds and one that is still down.
	HealthProbing HealthState = "PROBING"
)

// AllHealthStates is the complete state universe, used to build the machine and to drive the
// exhaustive transition property test.
var AllHealthStates = []HealthState{
	HealthHealthy, HealthDegraded, HealthUnhealthy, HealthProbing,
}

// Health thresholds, verbatim from docs/spec/00-design-baseline.md §10. They are exported
// because the routing engine's documentation, the operator console's explanation of why a circuit
// opened, and the tests that assert the boundaries all have to agree with the implementation, and
// the only way to guarantee that is for there to be one copy of each number.
const (
	// HealthWindow is the rolling observation window.
	HealthWindow = 30 * time.Second

	// MinSamples is the smallest number of observations in the window that permits a rate-based
	// decision. Below it, no transition happens. A 100% error rate over two samples is not
	// evidence of anything, and a circuit that trips on it will trip constantly on every
	// low-volume operation in the platform.
	MinSamples = 20

	// DegradedErrorRate is the HEALTHY → DEGRADED threshold: error rate strictly above 5%.
	DegradedErrorRate = 0.05

	// UnhealthyErrorRate is the → UNHEALTHY threshold: error rate strictly above 25%.
	UnhealthyErrorRate = 0.25

	// UnhealthyP99 is the latency arm of the → UNHEALTHY threshold. A gateway that answers
	// correctly but takes six seconds is, from the payer's point of view, down; and it consumes
	// the platform's own request slots for the whole six seconds, which is how one slow dependency
	// takes out an unrelated one.
	UnhealthyP99 = 5 * time.Second

	// BaseCooldown is the first UNHEALTHY → PROBING delay.
	BaseCooldown = 30 * time.Second

	// MaxCooldown caps the exponential backoff. Capped rather than unbounded because an uncapped
	// doubling reaches hours, and a gateway that recovered twenty minutes ago should not stay
	// circuit-broken because it failed six probes during a long outage.
	MaxCooldown = 5 * time.Minute

	// ProbeSuccessesToClose is how many consecutive successful probes close the circuit. Three
	// rather than one, because a recovering gateway frequently serves one request correctly from
	// a healthy instance behind a load balancer while the rest are still failing.
	ProbeSuccessesToClose = 3
)

// healthMachine is the transition table from baseline §10.
var healthMachine = shared.NewStateMachine("gateway_health", HealthHealthy,
	AllHealthStates, nil,
	[]shared.Transition[HealthState]{
		{From: HealthHealthy, To: HealthDegraded},
		// The direct HEALTHY → UNHEALTHY edge is not in the baseline's diagram, which draws the
		// common path. It is here because a gateway that starts returning 500s for everything
		// crosses both thresholds between two consecutive evaluations, and forcing it through
		// DEGRADED would mean one further evaluation window of full-rate errors — thirty seconds
		// of dispatching into a wall — before the circuit opens.
		{From: HealthHealthy, To: HealthUnhealthy},

		{From: HealthDegraded, To: HealthUnhealthy},
		// Recovery from DEGRADED without passing through UNHEALTHY. Also absent from the
		// baseline's diagram, and also necessary: without it, every transient blip that touches
		// 6% is permanent until the gateway gets bad enough to trip the circuit, so the routing
		// engine would spend the rest of the day de-prioritising a gateway that is fine.
		{From: HealthDegraded, To: HealthHealthy},

		{From: HealthUnhealthy, To: HealthProbing},
		{From: HealthProbing, To: HealthHealthy},
		{From: HealthProbing, To: HealthUnhealthy},
	})

// HealthMachine exposes the health state machine for the documentation generator and the
// exhaustive property test.
func HealthMachine() *shared.StateMachine[HealthState] { return healthMachine }

// IsKnown reports whether s is a state this binary understands.
func (s HealthState) IsKnown() bool { return healthMachine.IsKnown(s) }

// String satisfies fmt.Stringer.
func (s HealthState) String() string { return string(s) }

// AllowsDispatch reports whether traffic may be sent in this state. UNHEALTHY is the only state
// that refuses; PROBING allows the probe itself.
func (s HealthState) AllowsDispatch() bool { return s != HealthUnhealthy }

// ObservationOutcome is what one gateway interaction is worth to the health calculation.
//
// This is a local vocabulary rather than a reuse of payment.AttemptOutcome, and that is a
// deliberate refusal to couple BC-4 to BC-6. Health is also fed by scheduled L3 probes, by
// webhook deliveries and by lookups — none of which are payment attempts — and the day the
// payment context adds an outcome value would otherwise be the day gateway health changes
// meaning. The dispatcher maps; this package classifies.
type ObservationOutcome string

const (
	// ObservationSuccess is a well-formed, timely response.
	ObservationSuccess ObservationOutcome = "SUCCESS"

	// ObservationError is a transport failure, a 5xx, or a malformed response. This is the
	// gateway's fault and it counts against it.
	ObservationError ObservationOutcome = "ERROR"

	// ObservationTimeout is no response within the deadline. Counted as an error for health
	// purposes even though the payment-level handling is completely different: for the payment,
	// the outcome is unknown and must not be retried; for the gateway, a timeout is
	// indistinguishable from being down and is exactly what the circuit exists to detect.
	ObservationTimeout ObservationOutcome = "TIMEOUT"

	// ObservationHardDecline is a definitive refusal caused by the instruction — a stolen card,
	// an expired card, an invalid account. The gateway worked perfectly.
	ObservationHardDecline ObservationOutcome = "HARD_DECLINE"

	// ObservationSoftDecline is a refusal that might succeed elsewhere or later — issuer
	// unavailable, try again. Attributable to the issuer, not to the gateway, and unknowable from
	// our side of the boundary, so it is treated like a hard decline for health purposes.
	ObservationSoftDecline ObservationOutcome = "SOFT_DECLINE"
)

// IsValid reports whether o is a known observation outcome.
func (o ObservationOutcome) IsValid() bool {
	switch o {
	case ObservationSuccess, ObservationError, ObservationTimeout,
		ObservationHardDecline, ObservationSoftDecline:
		return true
	default:
		return false
	}
}

// String satisfies fmt.Stringer.
func (o ObservationOutcome) String() string { return string(o) }

// CountsAsError reports whether this outcome belongs in the error rate's numerator.
//
// This method is the single most consequential four lines in the file. Declines — hard and soft —
// are excluded, and the reason is a specific, recurring production incident: a merchant with a
// bad customer base, or one under a card-testing attack, produces a torrent of declines. If
// declines counted as errors, that merchant's traffic would open the circuit on a gateway that is
// working perfectly, and the circuit is shared by every merchant on that gateway. One merchant's
// customer quality would take the gateway offline for everyone else.
//
// Declines still count in the *denominator*. Including them there dilutes the error rate, which
// errs toward leaving the circuit closed — the safe direction, because closing a circuit that
// should be open costs some failed payments while opening one that should be closed costs all of
// them.
func (o ObservationOutcome) CountsAsError() bool {
	return o == ObservationError || o == ObservationTimeout
}

// --- the rolling window ----------------------------------------------------------------------

// windowBuckets divides HealthWindow into fixed one-second slots.
//
// Bucketing rather than keeping a slice of timestamped observations: at the platform's target
// throughput a thirty-second window holds tens of thousands of observations, and a slice of them
// is both unbounded in the burst case and O(n) to age out. Thirty integers age out in O(1) by
// being overwritten, and the resulting error rate is exact to a one-second granularity, which is
// well inside the noise of the thing being measured.
const windowBuckets = 30

const bucketWidth = HealthWindow / windowBuckets

type observationBucket struct {
	// start is the bucket's own start instant in Unix nanoseconds. It is compared on every write:
	// a bucket whose start does not match the current slot is a stale bucket from the previous
	// revolution of the ring and is zeroed rather than added to. This is what makes the window
	// roll without a background goroutine.
	start        int64
	total        int
	errors       int
	timeouts     int
	hardDeclines int
}

type rollingWindow struct {
	buckets [windowBuckets]observationBucket
}

func slotStart(now time.Time) int64 {
	return (now.UnixNano() / int64(bucketWidth)) * int64(bucketWidth)
}

func (w *rollingWindow) record(now time.Time, o ObservationOutcome) {
	start := slotStart(now)
	idx := (start / int64(bucketWidth)) % windowBuckets
	if idx < 0 {
		idx += windowBuckets
	}
	b := &w.buckets[idx]
	if b.start != start {
		*b = observationBucket{start: start}
	}
	b.total++
	switch o {
	case ObservationError:
		b.errors++
	case ObservationTimeout:
		b.timeouts++
	case ObservationHardDecline:
		b.hardDeclines++
	default:
		// A success or a soft decline is counted in b.total and nowhere else. A soft decline is the
		// issuer answering correctly, so charging it against the gateway's health would open the
		// breaker on a merchant whose customers' cards are simply out of funds
	}
}

// counts is the aggregate over the live portion of the window.
type counts struct {
	total        int
	errors       int
	timeouts     int
	hardDeclines int
}

func (c counts) errorRate() float64 {
	if c.total == 0 {
		return 0
	}
	return float64(c.errors+c.timeouts) / float64(c.total)
}

// qualifies reports whether the window carries enough observations for a rate decision.
func (c counts) qualifies() bool { return c.total >= MinSamples }

func (w *rollingWindow) totals(now time.Time) counts {
	current := slotStart(now)
	cutoff := current - int64(windowBuckets-1)*int64(bucketWidth)
	var c counts
	for i := range w.buckets {
		b := &w.buckets[i]
		// A bucket from the future can only exist if the clock moved backwards; excluding it is
		// cheaper than reasoning about what it would mean.
		if b.start < cutoff || b.start > current {
			continue
		}
		c.total += b.total
		c.errors += b.errors
		c.timeouts += b.timeouts
		c.hardDeclines += b.hardDeclines
	}
	return c
}

func (w *rollingWindow) reset() { *w = rollingWindow{} }

// --- the latency ring ------------------------------------------------------------------------

// latencyCapacity is the number of retained latency samples.
//
// Fixed-size by construction: an unbounded sample slice on a per-(gateway, operation) record
// multiplied by the number of gateways and operations is a memory leak with a schedule. 256 is
// chosen so that an exact p99 has at least a couple of samples above the threshold to sit on —
// a p99 over 20 samples is really a p95 — while the sort that computes it stays trivially cheap.
//
// The alternative was a streaming quantile sketch (t-digest, HDR histogram). Rejected: both mean
// an external dependency in a domain package that is required to have none, and both trade exact
// answers for constant-time updates the platform does not need at this sample size.
const latencyCapacity = 256

type latencyRing struct {
	samples [latencyCapacity]time.Duration
	// n is how many slots hold real samples, saturating at latencyCapacity.
	n    int
	next int
}

func (r *latencyRing) add(d time.Duration) {
	if d <= 0 {
		return
	}
	r.samples[r.next] = d
	r.next = (r.next + 1) % latencyCapacity
	if r.n < latencyCapacity {
		r.n++
	}
}

// percentile returns the p-th percentile by the nearest-rank method, which needs no interpolation
// and therefore always returns a duration that was actually observed. An interpolated quantile
// reported as "the p99" that no request ever experienced is a bad thing to page somebody with.
func (r *latencyRing) percentile(p float64) time.Duration {
	if r.n == 0 {
		return 0
	}
	buf := make([]time.Duration, r.n)
	copy(buf, r.samples[:r.n])
	slices.Sort(buf)
	rank := int(math.Ceil(p*float64(r.n))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= r.n {
		rank = r.n - 1
	}
	return buf[rank]
}

func (r *latencyRing) reset() { *r = latencyRing{} }

// --- the aggregate ---------------------------------------------------------------------------

// Health is the health and circuit-breaker record for one (gateway, operation) pair.
//
// It is the feedback loop baseline §10 describes: observations from the data plane fold in here,
// state changes publish `gateway.health_changed.v1`, and the routing engine consumes both the
// event and Score() to decide where the next payment goes.
//
// Everything about it is bounded. The observation window is thirty fixed buckets, the latency
// sample set is a fixed ring, and there is no background goroutine — a record that is not being
// observed consumes exactly its own struct and ages out its own window the next time anybody
// looks at it. That matters because there is one of these per (gateway, operation) per process,
// and the design has to survive somebody registering fifty gateways.
type Health struct {
	gatewayID shared.GatewayID
	operation shared.Operation

	state  HealthState
	window rollingWindow
	// latency holds samples for the p99 arm of the UNHEALTHY threshold.
	latency latencyRing

	consecutiveProbeSuccesses int

	// cooldown is the *current* backoff, which doubles on each failed probe and resets when the
	// circuit closes. Stored rather than derived from a failure count so that the cap is applied
	// in one place and the value an operator sees is the value the code will use.
	cooldown      time.Duration
	cooldownUntil time.Time

	lastObservedAt time.Time
	lastChangedAt  time.Time

	version shared.Version

	// clock is held on the struct, which is unusual in this codebase and is confined to this
	// aggregate. Score() is called from the routing hot path by a scorer whose interface has no
	// business carrying a clock, and the alternative — a Score(now) that every caller has to
	// thread a clock into — pushes the decay parameter into every call site that has no opinion
	// about it. Observe still takes an explicit `now`, because the caller of Observe has just
	// measured a request and already knows exactly when it happened.
	clock shared.Clock

	events []Event
}

// NewHealth creates a health record in HEALTHY.
//
// Starting optimistic rather than in PROBING is deliberate: a freshly started process knows
// nothing about the gateway, and the cost of the two possible mistakes is asymmetric. Starting
// healthy risks one window of failed payments on a gateway that is genuinely down; starting
// closed guarantees that every deployment brings the platform up refusing traffic on gateways
// that are fine.
func NewHealth(gatewayID shared.GatewayID, op shared.Operation, clock shared.Clock) (*Health, error) {
	if gatewayID.IsZero() {
		return nil, apierror.New(apierror.CodeValidationFailed, "a health record requires a gateway")
	}
	if op == "" {
		return nil, apierror.New(apierror.CodeValidationFailed, "a health record requires an operation").
			WithDetail(apierror.Detail{
				Field: "operation", Code: "MISSING",
				Message: "health is tracked per (gateway, operation); an operation is required",
				RuleID:  "L4.HEALTH_KEY_COMPLETE",
			})
	}
	now := clock.Now()
	return &Health{
		gatewayID:     gatewayID,
		operation:     op,
		state:         HealthHealthy,
		cooldown:      BaseCooldown,
		lastChangedAt: now,
		version:       1,
		clock:         clock,
	}, nil
}

// Accessors.

func (h *Health) GatewayID() shared.GatewayID    { return h.gatewayID }
func (h *Health) Operation() shared.Operation    { return h.operation }
func (h *Health) State() HealthState             { return h.state }
func (h *Health) Cooldown() time.Duration        { return h.cooldown }
func (h *Health) CooldownUntil() time.Time       { return h.cooldownUntil }
func (h *Health) ConsecutiveProbeSuccesses() int { return h.consecutiveProbeSuccesses }
func (h *Health) LastObservedAt() time.Time      { return h.lastObservedAt }
func (h *Health) LastChangedAt() time.Time       { return h.lastChangedAt }
func (h *Health) Version() shared.Version        { return h.version }

// Counters returns the live window's aggregate. Exposed as a value copy so a caller charting it
// cannot reach into the buckets.
func (h *Health) Counters() (total, errors, timeouts, hardDeclines int) {
	c := h.window.totals(h.clock.Now())
	return c.total, c.errors, c.timeouts, c.hardDeclines
}

// ErrorRate returns errors plus timeouts over total observations in the window. Declines are in
// the denominator and not the numerator; see ObservationOutcome.CountsAsError.
func (h *Health) ErrorRate() float64 { return h.window.totals(h.clock.Now()).errorRate() }

// SuccessRate returns the proportion of window observations that were not errors or timeouts.
//
// An empty window returns 1. That is not a claim that the gateway is perfect — it is the
// statement that no failure has been observed, and it is Score's freshness decay, not this
// method, that stops "no failures observed" being read as "known good".
func (h *Health) SuccessRate() float64 {
	c := h.window.totals(h.clock.Now())
	if c.total == 0 {
		return 1
	}
	return 1 - c.errorRate()
}

// P99Latency returns the 99th percentile of the retained latency samples, or zero if there are
// none.
func (h *Health) P99Latency() time.Duration { return h.latency.percentile(0.99) }

// Observe folds one gateway interaction into the window and re-evaluates the state.
//
// It returns whether the state changed. The caller uses that to publish
// `gateway.health_changed.v1` and to invalidate its local routing view. The boolean exists
// alongside the raised event — rather than the caller simply draining events — because Observe
// runs on every single dispatch and the health record is not persisted on every observation. The
// in-process router reacts to the boolean immediately; only a genuine state change is worth a row
// in the outbox and a round trip to Kafka.
//
// An unrecognised outcome is rejected and the observation is dropped. Folding it into the error
// count would let one adapter's mapping defect open circuits across the fleet, and folding it
// into the success count would hide a real outage. Neither guess is better than a loud refusal.
func (h *Health) Observe(outcome ObservationOutcome, latency time.Duration, now time.Time) (bool, error) {
	if !outcome.IsValid() {
		return false, apierror.Newf(apierror.CodeInternalError,
			"unknown health observation outcome %q for gateway %s/%s", outcome, h.gatewayID, h.operation).
			WithDetail(apierror.Detail{
				Field: "outcome", Code: "UNKNOWN_OBSERVATION_OUTCOME",
				Message: "the gateway adapter produced an outcome this binary does not classify",
				RuleID:  "L6.HEALTH_OUTCOME_CLASSIFIED",
			})
	}

	h.window.record(now, outcome)
	h.latency.add(latency)
	h.lastObservedAt = now

	changed := false
	// The cooldown expiring is a state change in its own right and it is not driven by an
	// observation, so it is checked before the observation is interpreted. Doing it here as well
	// as in AdmitRequest means a record that is being observed through some other path — a
	// scheduled probe, a webhook delivery — still leaves UNHEALTHY on time.
	if h.state == HealthUnhealthy && !now.Before(h.cooldownUntil) {
		h.enter(HealthProbing, now, "cooldown elapsed")
		changed = true
		// Fall through: this very observation is the first probe.
	}

	if next := h.evaluate(outcome, now); next != h.state {
		h.enter(next, now, h.reasonFor(next))
		changed = true
	}
	return changed, nil
}

// evaluate computes the state this observation implies, given the current state.
func (h *Health) evaluate(outcome ObservationOutcome, now time.Time) HealthState {
	switch h.state {
	case HealthProbing:
		// In PROBING the decision is driven by consecutive outcomes, not by the window rate: the
		// window still contains the failures that opened the circuit, and a rate computed over
		// them would re-open it on the first probe no matter how well the probe went.
		if outcome.CountsAsError() {
			return HealthUnhealthy
		}
		// A decline counts as a successful probe. The question a probe asks is "is the gateway
		// answering", and a gateway that declines a card has answered.
		h.consecutiveProbeSuccesses++
		if h.consecutiveProbeSuccesses >= ProbeSuccessesToClose {
			return HealthHealthy
		}
		return HealthProbing

	case HealthUnhealthy:
		// The cooldown check happened in Observe; if we are still here it has not elapsed.
		return HealthUnhealthy

	case HealthHealthy, HealthDegraded:
		c := h.window.totals(now)
		if !c.qualifies() {
			return h.state
		}
		rate := c.errorRate()
		if rate > UnhealthyErrorRate || h.P99Latency() > UnhealthyP99 {
			return HealthUnhealthy
		}
		if rate > DegradedErrorRate {
			return HealthDegraded
		}
		return HealthHealthy

	default:
		return h.state
	}
}

// reasonFor renders the human-readable cause that rides on the health-changed event. Operators
// read this line first, so it names the arm of the threshold that fired rather than restating the
// transition they can already see.
func (h *Health) reasonFor(next HealthState) string {
	switch next {
	case HealthUnhealthy:
		if h.state == HealthProbing {
			return "probe failed"
		}
		if h.P99Latency() > UnhealthyP99 {
			return "p99 latency above threshold"
		}
		return "error rate above unhealthy threshold"
	case HealthDegraded:
		return "error rate above degraded threshold"
	case HealthHealthy:
		if h.state == HealthProbing {
			return "probe succeeded"
		}
		return "error rate recovered"
	default:
		return ""
	}
}

// enter performs the transition, applies the side effects that belong to each edge, and raises
// the health-changed event.
func (h *Health) enter(next HealthState, now time.Time, reason string) {
	if err := healthMachine.Transition(h.state, next); err != nil {
		// Unreachable: every caller computes `next` from the table above. Returning silently
		// rather than panicking because this runs on the payment dispatch path, and a panic there
		// is strictly worse than a health record that fails to advance.
		return
	}
	previous := h.state
	h.state = next

	switch next {
	case HealthUnhealthy:
		if previous == HealthProbing {
			// A failed probe doubles the backoff, capped. This is the mechanism that stops the
			// platform probing a gateway that is down for an hour every thirty seconds for the
			// whole hour.
			h.cooldown *= 2
			if h.cooldown > MaxCooldown {
				h.cooldown = MaxCooldown
			}
		} else {
			// A fresh trip starts at the base cooldown. Carrying a doubled value over from a
			// previous, resolved outage would punish a gateway for history it has already
			// recovered from.
			h.cooldown = BaseCooldown
		}
		h.cooldownUntil = now.Add(h.cooldown)
		h.consecutiveProbeSuccesses = 0

	case HealthProbing:
		h.consecutiveProbeSuccesses = 0

	case HealthHealthy:
		h.consecutiveProbeSuccesses = 0
		h.cooldown = BaseCooldown
		h.cooldownUntil = time.Time{}
		if previous == HealthProbing {
			// Closing the circuit discards the window and the latency samples. They describe the
			// outage that has just ended; leaving them in place would let the rate that opened the
			// circuit immediately re-open it, and the gateway would never be allowed to recover.
			h.window.reset()
			h.latency.reset()
		}
	default:
		// HealthHealthy, HealthDegraded and HealthProbing need no cool-down bookkeeping on entry:
		// the cool-down is a property of being unhealthy, and it is reset when the breaker closes
	}

	h.lastChangedAt = now
	h.version = h.version.Next()
	h.events = append(h.events, Event{
		Type:       EventGatewayHealthChanged,
		GatewayID:  h.gatewayID,
		Operation:  h.operation,
		OccurredAt: now,
		Version:    h.version,
		Payload: map[string]any{
			"previousState": string(previous),
			"state":         string(next),
			"reason":        reason,
			"errorRate":     h.window.totals(now).errorRate(),
			"p99LatencyMs":  h.P99Latency().Milliseconds(),
			"cooldownMs":    h.cooldown.Milliseconds(),
		},
	})
}

// AdmitRequest is the circuit breaker's gate, and it is the only path out of UNHEALTHY.
//
// It exists because the cooldown expires on a clock, not on an observation: while the circuit is
// open nothing is dispatched, so nothing calls Observe, so nothing would ever notice that thirty
// seconds have passed. The dispatcher calls this before every dispatch; when the cooldown has
// elapsed it moves the record to PROBING and admits the request as the probe.
//
// It returns whether the request may go, and whether the state changed (so the caller publishes
// the event). Limiting how many probes run concurrently is a bulkhead concern in the dispatcher,
// not a domain one: this record has no way to know how many requests are in flight, and inventing
// a counter here would make it wrong under a restart.
func (h *Health) AdmitRequest(now time.Time) (allowed bool, changed bool) {
	if h.state == HealthUnhealthy {
		if now.Before(h.cooldownUntil) {
			return false, false
		}
		h.enter(HealthProbing, now, "cooldown elapsed")
		return true, true
	}
	return h.state.AllowsDispatch(), false
}

// Score is the 0..1 health term the routing engine multiplies into its weighted decision
// (baseline §23 gives health a weight of 0.4).
//
// Three things are folded in, and the third is the one worth explaining.
//
//  1. Observed quality: the window's success rate and how far the p99 sits below the unhealthy
//     threshold, weighted 60/40. Latency matters but not as much as correctness.
//  2. The state itself, as a multiplier: DEGRADED and PROBING are ranked below HEALTHY even when
//     their windows look acceptable, because both mean something is known to be wrong.
//  3. Freshness decay. A gateway with no recent traffic must be neither trusted nor punished: its
//     window is empty, so its success rate reads 1.0, and without decay a gateway nobody has used
//     all morning would outrank one that has been serving perfectly at volume. So the score is
//     blended toward a neutral 0.5 as the last observation ages, reaching neutral at
//     ScoreDecayHorizon. A gateway that has never been observed scores exactly neutral, which is
//     the honest answer: we have no evidence either way, and the router should let the other
//     terms — cost, priority, the merchant's configuration — decide.
//
// UNHEALTHY is excluded from all of that and scores zero regardless of age. This is the important
// exception: the reason an unhealthy gateway has no recent observations is that its own open
// circuit is preventing them, so decaying it toward neutral would let the gateway climb back up
// the rankings precisely because it is broken.
func (h *Health) Score() float64 {
	if h.state == HealthUnhealthy {
		return 0
	}
	if h.lastObservedAt.IsZero() {
		return NeutralScore
	}

	observed := 0.6*h.SuccessRate() + 0.4*h.latencyScore()
	switch h.state {
	case HealthDegraded:
		observed *= 0.6
	case HealthProbing:
		observed *= 0.3
	default:
		// HealthHealthy takes the observed score unmodified, and HealthUnhealthy returned 0 above.
	}

	age := h.clock.Now().Sub(h.lastObservedAt)
	weight := 1 - float64(age)/float64(ScoreDecayHorizon)
	if weight > 1 {
		weight = 1
	}
	if weight < 0 {
		weight = 0
	}
	return clamp01(weight*observed + (1-weight)*NeutralScore)
}

// NeutralScore is what a gateway scores when there is no evidence about it: neither a
// recommendation nor a penalty.
const NeutralScore = 0.5

// ScoreDecayHorizon is how long it takes an unobserved gateway's score to decay all the way to
// neutral. Two minutes rather than the thirty-second observation window: the window is about
// detecting a fault fast, whereas this is about not over-trusting stale evidence, and decaying a
// perfectly healthy low-volume gateway to neutral thirty seconds after its last payment would
// make routing oscillate on every quiet merchant.
const ScoreDecayHorizon = 2 * time.Minute

// latencyScore maps the observed p99 onto 0..1 against the unhealthy threshold: at zero latency
// it is 1, at the threshold it is 0.
func (h *Health) latencyScore() float64 {
	p99 := h.P99Latency()
	if p99 <= 0 {
		return 1
	}
	return clamp01(1 - float64(p99)/float64(UnhealthyP99))
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// PendingEvents returns the health-changed events raised since the last drain.
func (h *Health) PendingEvents() []Event { return append([]Event(nil), h.events...) }

// DrainEvents returns and clears the pending events.
func (h *Health) DrainEvents() []Event {
	out := h.events
	h.events = nil
	return out
}

// RehydrateHealthParams carries the persisted portion of a Health record.
//
// Note what is not here: the observation window and the latency samples. Those are deliberately
// not persisted. They describe the last thirty seconds, a process that has just started has not
// observed anything, and restoring a window from a row written minutes ago would have the new
// process make decisions about traffic it never saw. What *is* restored is the circuit position —
// state, cooldown and deadline — because forgetting that on a restart means a rolling deploy
// re-opens every circuit the fleet had carefully closed.
type RehydrateHealthParams struct {
	GatewayID                 shared.GatewayID
	Operation                 shared.Operation
	State                     HealthState
	Cooldown                  time.Duration
	CooldownUntil             time.Time
	ConsecutiveProbeSuccesses int
	LastObservedAt            time.Time
	LastChangedAt             time.Time
	Version                   shared.Version
}

// RehydrateHealth reconstructs a Health record from persisted state.
func RehydrateHealth(p RehydrateHealthParams, clock shared.Clock) (*Health, error) {
	if !p.State.IsKnown() {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"gateway health %s/%s has unknown state %q; this row may have been written by a newer version of the service",
			p.GatewayID, p.Operation, p.State)
	}
	cooldown := p.Cooldown
	if cooldown <= 0 {
		cooldown = BaseCooldown
	}
	if cooldown > MaxCooldown {
		cooldown = MaxCooldown
	}
	return &Health{
		gatewayID:                 p.GatewayID,
		operation:                 p.Operation,
		state:                     p.State,
		consecutiveProbeSuccesses: p.ConsecutiveProbeSuccesses,
		cooldown:                  cooldown,
		cooldownUntil:             p.CooldownUntil,
		lastObservedAt:            p.LastObservedAt,
		lastChangedAt:             p.LastChangedAt,
		version:                   p.Version,
		clock:                     clock,
	}, nil
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-35, FR-36.
//
// Per (gateway, operation) health: the rolling window, the circuit breaker, and the rule that
// a business decline is not an availability signal
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
