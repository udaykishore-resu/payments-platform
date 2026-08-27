package resilience

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// PriorityClass is the load-shedding priority of an operation. The six classes are those of
// docs/failure-handling.md §2.7, and the numeric order is the shed order reversed: the *lowest*
// numbered class is shed last.
//
// The ordering is the single most consequential decision in this file, and it is a business
// decision expressed as a constant block:
//
//	P0  refunds, voids, dispute handling, webhook ingest   never shed  (10 % reserved)
//	P1  captures                                           shed 5th
//	P2  authorizations / new payments                      shed 4th
//	P3  single-resource reads, status                      shed 3rd
//	P4  reports, list endpoints, exports, analytics        shed 1st
//	P5  background: rebuilds, cache warming, sweeps        shed 0th (paused, not shed)
//
// **Why money-out is shed last, and why the obvious alternative is wrong.** The intuitive
// ordering under load is to protect revenue: keep taking payments, drop the refunds, since
// refunds are the operation nobody is waiting on. That ordering converts an availability
// incident into a consumer-harm incident, and the two are not the same kind of problem.
//
// A merchant who cannot take a payment for ten minutes loses some sales. That is a revenue
// event, it is bounded by the duration of the incident, and the customer retries. A merchant
// who cannot issue a refund for ten minutes has a cardholder who has been charged for something
// they returned, and every minute of that is a minute in which the cardholder can open a
// chargeback — which costs the scheme fee, the merchant's dispute ratio, and, past a threshold,
// the merchant's ability to accept cards at all. Refund failures also carry a regulatory edge
// that authorization failures do not: consumer-protection regimes give cardholders a right to
// their money back on a timeline, and "our concurrency limiter was saturated" is not a defence.
// The harm outlives the incident, and it lands on a consumer rather than on us.
//
// Shedding money-in first is also the strictly safer failure: refusing to take money is
// reversible by the customer retrying, whereas refusing to give money back is not reversible by
// anybody. Baseline §8 states it as a rule — you must always be able to give money back — and
// the P0 class with its 10 % reserved capacity is that rule made operational.
//
// Webhook ingest sits in P0 for a related reason that is arithmetic rather than ethics:
// dropping an inbound webhook does not save work, it *creates* work. The event is the cheapest
// path by which a PROCESSING payment resolves; without it the payment falls to the reconciler,
// which costs a lookup call to the gateway, a reconciliation record and, past fifteen minutes,
// an operator. Shedding a 5 ms ingest to avoid load is trading a millisecond for a manual
// investigation.
type PriorityClass int

// The six classes. The order is load-bearing: an Admit decision is a comparison against
// thresholds indexed by these values.
const (
	// PriorityMoneyOut (P0) is refunds, voids, dispute handling and webhook ingest. Never shed.
	PriorityMoneyOut PriorityClass = iota
	// PriorityCapture (P1) is capture: money the merchant has already been promised.
	PriorityCapture
	// PriorityAuthorize (P2) is authorization and payment creation — money in.
	PriorityAuthorize
	// PriorityRead (P3) is single-resource reads and status.
	PriorityRead
	// PriorityReport (P4) is reports, list endpoints, exports and analytics.
	PriorityReport
	// PriorityBackground (P5) is non-essential background work: projection rebuilds, cache
	// warming, optional reconciliation sweeps. Paused rather than rejected — nobody is waiting
	// on a response to fail.
	PriorityBackground

	// numPriorityClasses is the array bound for the threshold table.
	numPriorityClasses = 6
)

// String returns the class label used in metrics and in the degradation ladder: "P0".."P5".
func (p PriorityClass) String() string {
	switch p {
	case PriorityMoneyOut:
		return "P0"
	case PriorityCapture:
		return "P1"
	case PriorityAuthorize:
		return "P2"
	case PriorityRead:
		return "P3"
	case PriorityReport:
		return "P4"
	case PriorityBackground:
		return "P5"
	default:
		return "UNKNOWN"
	}
}

// Operations describes what belongs in the class, so a caller wiring a route to a class does
// not have to guess.
func (p PriorityClass) Operations() string {
	switch p {
	case PriorityMoneyOut:
		return "refund, void, dispute handling, webhook ingest"
	case PriorityCapture:
		return "capture"
	case PriorityAuthorize:
		return "authorize, create payment"
	case PriorityRead:
		return "single-resource reads, payment status"
	case PriorityReport:
		return "reports, list endpoints, exports, analytics"
	case PriorityBackground:
		return "projection rebuilds, cache warming, optional reconciliation sweeps"
	default:
		return ""
	}
}

// Valid reports whether p is one of the six classes. An unrecognised class is treated by the
// Shedder as the lowest priority, on the principle that unclassified work is work nobody has
// argued for.
func (p PriorityClass) Valid() bool { return p >= PriorityMoneyOut && p < numPriorityClasses }

// DefaultShedThresholds maps each class to the pressure at which it starts being shed. The
// values are the degradation ladder's triggers (docs/failure-handling.md §4) expressed as a
// single normalized pressure signal:
//
//	P5 0.20  rung 1 — limiter reducing / CPU > 70 %
//	P4 0.25  rung 2 — limiter queue > 25 %
//	P3 0.50  rung 4 — limiter queue > 50 %
//	P2 0.75  rung 6 — limiter at min, gateway bulkheads saturated
//	P1 0.90  rung 7 — sustained saturation
//	P0 +Inf  rung 8 — money-out only; P0 is never shed at any pressure
//
// P0's threshold is positive infinity rather than 1.0 deliberately: pressure is in-flight ÷
// limit and can legitimately exceed 1 during a limit reduction, and a threshold of 1.0 would
// quietly start shedding refunds at exactly the moment the system is most stressed — which is
// the failure this whole ordering exists to prevent.
var DefaultShedThresholds = [numPriorityClasses]float64{
	PriorityMoneyOut:   math.Inf(1),
	PriorityCapture:    0.90,
	PriorityAuthorize:  0.75,
	PriorityRead:       0.50,
	PriorityReport:     0.25,
	PriorityBackground: 0.20,
}

// DefaultShedHysteresis is 2 minutes, from docs/failure-handling.md §4: a rung is climbed down
// only after its trigger has been clear for two minutes. Without hysteresis the system
// oscillates — shedding lowers pressure, lower pressure stops the shedding, the load returns,
// pressure rises — and a merchant sees intermittent 503s rather than a clean degradation.
const DefaultShedHysteresis = 2 * time.Minute

// ShedderConfig parameterizes a Shedder.
type ShedderConfig struct {
	// Pressure returns the current normalized pressure, conventionally
	// AdaptiveLimiter.Pressure or Bulkhead.Saturation. Required; a nil Pressure is treated as
	// constant zero, which admits everything.
	//
	// It is a function rather than a value because the shedder must read the signal at the
	// moment of the decision. A pressure pushed in on a timer is a pressure that is stale by up
	// to the timer's period, and during the seconds that matter the signal moves faster than
	// any timer worth having.
	Pressure func() float64

	// Thresholds defaults to DefaultShedThresholds.
	Thresholds [numPriorityClasses]float64

	// Hysteresis defaults to DefaultShedHysteresis.
	Hysteresis time.Duration

	Clock Clock

	// Shed is called when a request is rejected, for pp_load_shed_total{class}.
	Shed func(class PriorityClass, pressure float64)
	// RungChanged is called when the engaged degradation rung changes, outside the lock.
	RungChanged func(from, to int)
}

// Shedder admits or rejects work by priority class against a live pressure signal.
//
// It is the mechanism behind the degradation ladder: the rungs of §4 are exactly "which classes
// are currently engaged". Shed responses are 503 with Retry-After and retryable: true, because
// telling a client the truth is what makes their own backoff behave — a client told a shed is
// permanent gives up on work that would have succeeded thirty seconds later, and a client told
// nothing at all retries immediately and deepens the pressure that caused the shed.
//
// Safe for concurrent use. Owns no goroutine.
type Shedder struct {
	cfg ShedderConfig

	mu           sync.Mutex
	engaged      [numPriorityClasses]bool
	clearedAt    [numPriorityClasses]time.Time
	rung         int
	pendingRungs []limitChange

	shedCount [numPriorityClasses]atomic.Uint64
}

// NewShedder returns a shedder reading cfg.Pressure.
func NewShedder(cfg ShedderConfig) *Shedder {
	if cfg.Pressure == nil {
		cfg.Pressure = func() float64 { return 0 }
	}
	if cfg.Thresholds == ([numPriorityClasses]float64{}) {
		cfg.Thresholds = DefaultShedThresholds
	}
	if cfg.Hysteresis <= 0 {
		cfg.Hysteresis = DefaultShedHysteresis
	}
	cfg.Clock = orSystem(cfg.Clock)
	return &Shedder{cfg: cfg}
}

// Allow reports whether a request of class c may proceed at the current pressure.
func (s *Shedder) Allow(c PriorityClass) bool {
	if !c.Valid() {
		c = PriorityBackground
	}
	pressure := s.cfg.Pressure()

	s.mu.Lock()
	s.evaluateLocked(pressure)
	engaged := s.engaged[c]
	pend := s.pendingRungs
	s.pendingRungs = nil
	s.mu.Unlock()
	s.notifyRung(pend)

	if !engaged {
		return true
	}
	s.shedCount[c].Add(1)
	if s.cfg.Shed != nil {
		s.cfg.Shed(c, pressure)
	}
	return false
}

// Admit is Allow returning the platform error for a rejection: SERVICE_UNAVAILABLE, which is
// 503, retryable, with a Retry-After.
//
// The Retry-After is scaled by how deep the shedding is — 1 s at the first rung, up to 30 s at
// the deepest — so a client backing off during a mild trim returns quickly, while one backing
// off during a money-out-only incident does not spend the incident re-asking.
func (s *Shedder) Admit(c PriorityClass) error {
	if s.Allow(c) {
		return nil
	}
	return apierror.Newf(apierror.CodeServiceUnavailable,
		"load shedding is active: priority class %s (%s) is currently rejected",
		c, c.Operations()).WithRetryAfter(s.retryAfterSeconds())
}

func (s *Shedder) retryAfterSeconds() int {
	switch r := s.Rung(); {
	case r >= 8:
		return 30
	case r >= 6:
		return 10
	case r >= 4:
		return 5
	default:
		return 1
	}
}

// evaluateLocked engages classes whose threshold the pressure exceeds, and disengages classes
// whose threshold has been clear for the hysteresis window.
func (s *Shedder) evaluateLocked(pressure float64) {
	now := s.cfg.Clock.Now()
	for c := PriorityClass(0); c < numPriorityClasses; c++ {
		if pressure > s.cfg.Thresholds[c] {
			s.engaged[c] = true
			s.clearedAt[c] = time.Time{}
			continue
		}
		if !s.engaged[c] {
			continue
		}
		if s.clearedAt[c].IsZero() {
			s.clearedAt[c] = now
			continue
		}
		if now.Sub(s.clearedAt[c]) >= s.cfg.Hysteresis {
			s.engaged[c] = false
			s.clearedAt[c] = time.Time{}
		}
	}
	if r := s.computeRungLocked(); r != s.rung {
		s.pendingRungs = append(s.pendingRungs, limitChange{from: s.rung, to: r})
		s.rung = r
	}
}

// rungOf maps a class to the ladder rung at which it is shed (docs/failure-handling.md §4).
// Rungs 3, 5, 9 and 10 are not shedding rungs — they are "serve stale", "drop enrichment",
// "read-only" and "fail closed" — so they do not appear here.
func rungOf(c PriorityClass) int {
	switch c {
	case PriorityBackground:
		return 1
	case PriorityReport:
		return 2
	case PriorityRead:
		return 4
	case PriorityAuthorize:
		return 6
	case PriorityCapture:
		return 7
	case PriorityMoneyOut:
		return 8
	default:
		return 0
	}
}

func (s *Shedder) computeRungLocked() int {
	r := 0
	for c := PriorityClass(0); c < numPriorityClasses; c++ {
		if s.engaged[c] {
			r = max(r, rungOf(c))
		}
	}
	return r
}

// Rung returns the deepest engaged rung of the degradation ladder, 0 meaning normal service.
// This is what an operator reads on the dashboard and what the incident channel quotes.
func (s *Shedder) Rung() int {
	pressure := s.cfg.Pressure()
	s.mu.Lock()
	s.evaluateLocked(pressure)
	r := s.rung
	pend := s.pendingRungs
	s.pendingRungs = nil
	s.mu.Unlock()
	s.notifyRung(pend)
	return r
}

// Engaged reports whether class c is currently being shed, without counting a shed. For metric
// export and for the readiness endpoint.
func (s *Shedder) Engaged(c PriorityClass) bool {
	if !c.Valid() {
		c = PriorityBackground
	}
	pressure := s.cfg.Pressure()
	s.mu.Lock()
	s.evaluateLocked(pressure)
	e := s.engaged[c]
	pend := s.pendingRungs
	s.pendingRungs = nil
	s.mu.Unlock()
	s.notifyRung(pend)
	return e
}

// ShedCount returns how many requests of class c have been rejected, backing
// pp_load_shed_total{class}.
func (s *Shedder) ShedCount(c PriorityClass) uint64 {
	if !c.Valid() {
		return 0
	}
	return s.shedCount[c].Load()
}

func (s *Shedder) notifyRung(changes []limitChange) {
	cb := s.cfg.RungChanged
	if cb == nil {
		return
	}
	for _, ch := range changes {
		cb(ch.from, ch.to)
	}
}
