package gateway

import (
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// declaredHealthEdges restates the health transition table from baseline §10, plus the two edges
// this implementation adds deliberately (HEALTHY → UNHEALTHY and DEGRADED → HEALTHY). The
// property test compares it against the machine over the full from × to cross product.
var declaredHealthEdges = map[HealthState]map[HealthState]bool{
	HealthHealthy: {
		HealthDegraded:  true,
		HealthUnhealthy: true,
	},
	HealthDegraded: {
		HealthUnhealthy: true,
		HealthHealthy:   true,
	},
	HealthUnhealthy: {
		HealthProbing: true,
	},
	HealthProbing: {
		HealthHealthy:   true,
		HealthUnhealthy: true,
	},
}

func TestHealthMachineAcceptsExactlyTheDeclaredEdges(t *testing.T) {
	t.Parallel()

	m := HealthMachine()
	states := m.States()
	if len(states) != len(AllHealthStates) {
		t.Fatalf("machine universe has %d states, AllHealthStates has %d", len(states), len(AllHealthStates))
	}

	for _, from := range states {
		for _, to := range states {
			want := declaredHealthEdges[from][to]
			got := m.CanTransition(from, to)
			if got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
			err := m.Transition(from, to)
			if want && err != nil {
				t.Errorf("Transition(%s, %s) = %v, want nil", from, to, err)
			}
			if !want && apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
				t.Errorf("Transition(%s, %s) code = %s, want INVALID_STATE_TRANSITION",
					from, to, apierror.CodeOf(err))
			}
		}
	}
}

func TestHealthMachineHasNoTerminalOrSelfTransitions(t *testing.T) {
	t.Parallel()

	m := HealthMachine()
	for _, s := range m.States() {
		if m.IsTerminal(s) {
			t.Errorf("%s is terminal; a circuit that can never change state again is a permanent outage", s)
		}
		if m.CanTransition(s, s) {
			t.Errorf("%s has an undeclared self-transition", s)
		}
	}
	// PROBING is reachable only from UNHEALTHY: there is no path that half-opens a circuit that
	// was never opened.
	if m.CanTransition(HealthHealthy, HealthProbing) || m.CanTransition(HealthDegraded, HealthProbing) {
		t.Fatal("PROBING must be reachable only from UNHEALTHY")
	}
}

func newTestHealth(t *testing.T) (*Health, *shared.FixedClock) {
	t.Helper()
	clk := &shared.FixedClock{T: testEpoch}
	h, err := NewHealth("stripe", shared.OpAuthorize, clk)
	if err != nil {
		t.Fatalf("NewHealth: %v", err)
	}
	return h, clk
}

// observeN folds n identical observations in at the same instant, returning how many of them
// changed the state.
func observeN(t *testing.T, h *Health, n int, o ObservationOutcome, latency time.Duration, at time.Time) int {
	t.Helper()
	changes := 0
	for i := 0; i < n; i++ {
		changed, err := h.Observe(o, latency, at)
		if err != nil {
			t.Fatalf("Observe: %v", err)
		}
		if changed {
			changes++
		}
	}
	return changes
}

func TestNewHealthValidation(t *testing.T) {
	t.Parallel()

	clk := &shared.FixedClock{T: testEpoch}
	if _, err := NewHealth("", shared.OpAuthorize, clk); apierror.CodeOf(err) != apierror.CodeValidationFailed {
		t.Fatalf("no gateway: code = %s", apierror.CodeOf(err))
	}
	if _, err := NewHealth("stripe", "", clk); apierror.CodeOf(err) != apierror.CodeValidationFailed {
		t.Fatalf("no operation: code = %s", apierror.CodeOf(err))
	}
	h, err := NewHealth("stripe", shared.OpAuthorize, clk)
	if err != nil {
		t.Fatalf("NewHealth: %v", err)
	}
	// Starting optimistic: the cost of one window of failures on a gateway that is down is far
	// lower than every deploy coming up refusing traffic on gateways that are fine.
	if h.State() != HealthHealthy {
		t.Fatalf("state = %s, want HEALTHY", h.State())
	}
	if h.Cooldown() != BaseCooldown {
		t.Fatalf("cooldown = %s, want %s", h.Cooldown(), BaseCooldown)
	}
}

func TestObserveRejectsUnknownOutcomes(t *testing.T) {
	t.Parallel()

	h, _ := newTestHealth(t)
	changed, err := h.Observe("PROBABLY_FINE", 10*time.Millisecond, testEpoch)
	if err == nil {
		t.Fatal("an unclassified outcome was accepted")
	}
	if changed {
		t.Fatal("a rejected observation reported a state change")
	}
	if apierror.CodeOf(err) != apierror.CodeInternalError {
		t.Fatalf("code = %s, want INTERNAL_ERROR", apierror.CodeOf(err))
	}
	total, _, _, _ := h.Counters()
	if total != 0 {
		t.Fatalf("a rejected observation was recorded: total = %d", total)
	}
}

func TestErrorRateThresholds(t *testing.T) {
	t.Parallel()

	fast := 100 * time.Millisecond

	tests := []struct {
		name      string
		successes int
		errors    int
		timeouts  int
		want      HealthState
		why       string
	}{
		{
			name:      "below the minimum sample count nothing happens",
			successes: 0, errors: 19,
			want: HealthHealthy,
			why:  "a 100% error rate over 19 samples is noise, not evidence",
		},
		{
			name:      "exactly at the minimum sample count with exactly 5% stays healthy",
			successes: 19, errors: 1,
			want: HealthHealthy,
			why:  "the threshold is strictly greater than 5%",
		},
		{
			name:      "just above 5% degrades",
			successes: 19, errors: 2,
			want: HealthDegraded,
			why:  "2/21 = 9.5%",
		},
		{
			name:      "exactly 25% degrades but does not open the circuit",
			successes: 15, errors: 5,
			want: HealthDegraded,
			why:  "the unhealthy threshold is strictly greater than 25%",
		},
		{
			name:      "above 25% opens the circuit",
			successes: 14, errors: 6,
			want: HealthUnhealthy,
			why:  "6/20 = 30%",
		},
		{
			name:      "timeouts count as errors",
			successes: 14, timeouts: 6,
			want: HealthUnhealthy,
			why:  "an unanswered call is indistinguishable from a gateway that is down",
		},
		{
			name:      "errors and timeouts share the numerator",
			successes: 14, errors: 3, timeouts: 3,
			want: HealthUnhealthy,
			why:  "6/20 = 30%",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestHealth(t)
			observeN(t, h, tc.successes, ObservationSuccess, fast, testEpoch)
			observeN(t, h, tc.errors, ObservationError, fast, testEpoch)
			observeN(t, h, tc.timeouts, ObservationTimeout, fast, testEpoch)
			if h.State() != tc.want {
				t.Fatalf("state = %s, want %s (%s); error rate = %.4f",
					h.State(), tc.want, tc.why, h.ErrorRate())
			}
		})
	}
}

func TestHardDeclinesDoNotOpenTheCircuit(t *testing.T) {
	// Verifies: BR-23.
	t.Parallel()

	fast := 100 * time.Millisecond

	t.Run("declines alone never trip anything", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHealth(t)
		// A merchant under a card-testing attack, or one with a genuinely bad customer base,
		// produces exactly this. If declines counted as errors it would open a circuit shared by
		// every other merchant on the gateway.
		observeN(t, h, 200, ObservationHardDecline, fast, testEpoch)
		if h.State() != HealthHealthy {
			t.Fatalf("state = %s after 200 hard declines, want HEALTHY", h.State())
		}
		if rate := h.ErrorRate(); rate != 0 {
			t.Fatalf("error rate = %v after 200 hard declines, want 0", rate)
		}
		total, errs, timeouts, declines := h.Counters()
		if total != 200 || errs != 0 || timeouts != 0 || declines != 200 {
			t.Fatalf("counters = total %d errors %d timeouts %d declines %d", total, errs, timeouts, declines)
		}
	})

	t.Run("soft declines are also excluded", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHealth(t)
		observeN(t, h, 200, ObservationSoftDecline, fast, testEpoch)
		if h.State() != HealthHealthy {
			t.Fatalf("state = %s after 200 soft declines, want HEALTHY", h.State())
		}
	})

	t.Run("declines sit in the denominator and dilute the rate", func(t *testing.T) {
		t.Parallel()
		// 15 errors alongside 35 successes is 30% and opens the circuit.
		open, _ := newTestHealth(t)
		observeN(t, open, 35, ObservationSuccess, fast, testEpoch)
		observeN(t, open, 15, ObservationError, fast, testEpoch)
		if open.State() != HealthUnhealthy {
			t.Fatalf("15 errors in 50 samples: state = %s, want UNHEALTHY", open.State())
		}

		// The same 15 errors alongside 85 hard declines is 15%: degraded, not open. Diluting errs
		// toward leaving the circuit closed, which is the safe direction.
		diluted, _ := newTestHealth(t)
		observeN(t, diluted, 85, ObservationHardDecline, fast, testEpoch)
		observeN(t, diluted, 15, ObservationError, fast, testEpoch)
		if diluted.State() != HealthDegraded {
			t.Fatalf("15 errors in 100 samples: state = %s, want DEGRADED", diluted.State())
		}
	})

	t.Run("a decline counts as a successful probe", func(t *testing.T) {
		t.Parallel()
		h, clk := newTestHealth(t)
		tripCircuit(t, h, testEpoch)
		at := clk.Advance(BaseCooldown)
		if allowed, changed := h.AdmitRequest(at); !allowed || !changed {
			t.Fatalf("AdmitRequest after cooldown = (%v, %v), want (true, true)", allowed, changed)
		}
		// The question a probe asks is "is the gateway answering". A gateway that declines a card
		// has answered.
		observeN(t, h, ProbeSuccessesToClose, ObservationHardDecline, fast, at)
		if h.State() != HealthHealthy {
			t.Fatalf("state = %s after %d declining probes, want HEALTHY", h.State(), ProbeSuccessesToClose)
		}
	})
}

func TestP99LatencyArmOpensTheCircuit(t *testing.T) {
	t.Parallel()

	h, _ := newTestHealth(t)
	// Every call succeeds, so the error rate is zero. The gateway is nonetheless unusable: six
	// seconds per request is down from the payer's point of view and it consumes a request slot
	// for the whole six seconds.
	observeN(t, h, MinSamples, ObservationSuccess, 6*time.Second, testEpoch)
	if h.ErrorRate() != 0 {
		t.Fatalf("error rate = %v, want 0", h.ErrorRate())
	}
	if h.State() != HealthUnhealthy {
		t.Fatalf("state = %s, want UNHEALTHY on the latency arm", h.State())
	}
	if h.P99Latency() != 6*time.Second {
		t.Fatalf("p99 = %s, want 6s", h.P99Latency())
	}

	// Exactly at the threshold is not above it.
	at, _ := newTestHealth(t)
	observeN(t, at, MinSamples, ObservationSuccess, UnhealthyP99, testEpoch)
	if at.State() != HealthHealthy {
		t.Fatalf("state = %s at exactly the p99 threshold, want HEALTHY", at.State())
	}

	// And the latency arm respects the minimum sample count too.
	sparse, _ := newTestHealth(t)
	observeN(t, sparse, MinSamples-1, ObservationSuccess, 30*time.Second, testEpoch)
	if sparse.State() != HealthHealthy {
		t.Fatalf("state = %s on %d slow samples, want HEALTHY", sparse.State(), MinSamples-1)
	}
}

func TestDegradedRecoversWithoutOpeningTheCircuit(t *testing.T) {
	t.Parallel()

	h, clk := newTestHealth(t)
	fast := 50 * time.Millisecond

	observeN(t, h, 19, ObservationSuccess, fast, testEpoch)
	observeN(t, h, 2, ObservationError, fast, testEpoch)
	if h.State() != HealthDegraded {
		t.Fatalf("state = %s, want DEGRADED", h.State())
	}

	// Move past the window so the two errors age out, then observe a clean window.
	recovered := clk.Advance(HealthWindow + time.Second)
	observeN(t, h, MinSamples, ObservationSuccess, fast, recovered)
	if h.State() != HealthHealthy {
		t.Fatalf("state = %s, want HEALTHY once the errors aged out of the window", h.State())
	}
	if total, _, _, _ := h.Counters(); total != MinSamples {
		t.Fatalf("window total = %d, want %d; observations did not age out", total, MinSamples)
	}
}

func TestCooldownProbeAndClose(t *testing.T) {
	// Verifies: BR-35, FR-36.
	t.Parallel()

	h, clk := newTestHealth(t)
	tripCircuit(t, h, testEpoch)
	if h.State() != HealthUnhealthy {
		t.Fatalf("state = %s, want UNHEALTHY", h.State())
	}
	if got, want := h.CooldownUntil(), testEpoch.Add(BaseCooldown); !got.Equal(want) {
		t.Fatalf("cooldownUntil = %s, want %s", got, want)
	}

	// Nothing is dispatched while the circuit is open.
	early := clk.Advance(BaseCooldown - time.Second)
	if allowed, changed := h.AdmitRequest(early); allowed || changed {
		t.Fatalf("AdmitRequest before the cooldown = (%v, %v), want (false, false)", allowed, changed)
	}

	// The cooldown expires on a clock, not on an observation: nothing is calling Observe while
	// the circuit is open, so AdmitRequest is the only thing that can notice.
	ready := clk.Advance(time.Second)
	allowed, changed := h.AdmitRequest(ready)
	if !allowed || !changed {
		t.Fatalf("AdmitRequest at the cooldown = (%v, %v), want (true, true)", allowed, changed)
	}
	if h.State() != HealthProbing {
		t.Fatalf("state = %s, want PROBING", h.State())
	}
	evts := h.DrainEvents()
	last := evts[len(evts)-1]
	if last.Type != EventGatewayHealthChanged || last.Payload["state"] != string(HealthProbing) {
		t.Fatalf("expected a health-changed event into PROBING, got %+v", last)
	}

	// Two successes are not enough — a recovering gateway often serves one request correctly from
	// a healthy instance behind a load balancer.
	for i := 1; i < ProbeSuccessesToClose; i++ {
		if _, err := h.Observe(ObservationSuccess, 20*time.Millisecond, ready); err != nil {
			t.Fatalf("Observe: %v", err)
		}
		if h.State() != HealthProbing {
			t.Fatalf("state = %s after %d probe successes, want PROBING", h.State(), i)
		}
		if h.ConsecutiveProbeSuccesses() != i {
			t.Fatalf("consecutive probe successes = %d, want %d", h.ConsecutiveProbeSuccesses(), i)
		}
	}
	changed, err := h.Observe(ObservationSuccess, 20*time.Millisecond, ready)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !changed || h.State() != HealthHealthy {
		t.Fatalf("state = %s (changed %v) after %d successes, want HEALTHY",
			h.State(), changed, ProbeSuccessesToClose)
	}
	// Closing the circuit resets the backoff and discards the window that opened it — otherwise
	// the rate that tripped the circuit would immediately re-trip it and the gateway could never
	// recover.
	if h.Cooldown() != BaseCooldown {
		t.Fatalf("cooldown = %s after recovery, want %s", h.Cooldown(), BaseCooldown)
	}
	if total, _, _, _ := h.Counters(); total != 0 {
		t.Fatalf("window total = %d after closing the circuit, want 0", total)
	}
	if !h.CooldownUntil().IsZero() {
		t.Fatalf("cooldownUntil = %s after recovery, want zero", h.CooldownUntil())
	}
}

func TestCooldownDoublesAndIsCapped(t *testing.T) {
	t.Parallel()

	h, clk := newTestHealth(t)
	tripCircuit(t, h, testEpoch)
	if h.Cooldown() != BaseCooldown {
		t.Fatalf("first cooldown = %s, want %s", h.Cooldown(), BaseCooldown)
	}

	want := []time.Duration{
		2 * BaseCooldown, // 1m
		4 * BaseCooldown, // 2m
		8 * BaseCooldown, // 4m
		MaxCooldown,      // capped from 8m
		MaxCooldown,      // stays capped
	}
	for i, expected := range want {
		at := clk.Advance(h.Cooldown())
		if allowed, _ := h.AdmitRequest(at); !allowed {
			t.Fatalf("probe %d: AdmitRequest at the cooldown was refused", i+1)
		}
		if _, err := h.Observe(ObservationError, time.Second, at); err != nil {
			t.Fatalf("Observe: %v", err)
		}
		if h.State() != HealthUnhealthy {
			t.Fatalf("probe %d: state = %s after a failed probe, want UNHEALTHY", i+1, h.State())
		}
		if h.Cooldown() != expected {
			t.Fatalf("probe %d: cooldown = %s, want %s", i+1, h.Cooldown(), expected)
		}
		if got, wantUntil := h.CooldownUntil(), at.Add(expected); !got.Equal(wantUntil) {
			t.Fatalf("probe %d: cooldownUntil = %s, want %s", i+1, got, wantUntil)
		}
	}

	// A fresh trip after a recovery starts over at the base cooldown rather than inheriting the
	// backoff from an outage the gateway has already recovered from.
	at := clk.Advance(h.Cooldown())
	if allowed, _ := h.AdmitRequest(at); !allowed {
		t.Fatal("AdmitRequest at the cooldown was refused")
	}
	observeN(t, h, ProbeSuccessesToClose, ObservationSuccess, 20*time.Millisecond, at)
	if h.State() != HealthHealthy {
		t.Fatalf("state = %s, want HEALTHY", h.State())
	}
	later := clk.Advance(time.Hour)
	tripCircuit(t, h, later)
	if h.Cooldown() != BaseCooldown {
		t.Fatalf("cooldown after a fresh trip = %s, want %s", h.Cooldown(), BaseCooldown)
	}
}

func TestObserveDuringCooldownExpiryReportsTheChange(t *testing.T) {
	// Verifies: FR-36.
	t.Parallel()

	// A scheduled L3 probe or a webhook delivery can observe a record whose circuit is open. That
	// observation has to move the record to PROBING and report the change, or the router's local
	// view stays stale until the next AdmitRequest.
	h, clk := newTestHealth(t)
	tripCircuit(t, h, testEpoch)
	at := clk.Advance(BaseCooldown)
	changed, err := h.Observe(ObservationSuccess, 20*time.Millisecond, at)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !changed {
		t.Fatal("an observation that half-opened the circuit reported no change")
	}
	if h.State() != HealthProbing {
		t.Fatalf("state = %s, want PROBING", h.State())
	}
	if h.ConsecutiveProbeSuccesses() != 1 {
		t.Fatalf("consecutive probe successes = %d, want 1", h.ConsecutiveProbeSuccesses())
	}
}

func TestHealthEventsAreKeyedByGatewayAndOperation(t *testing.T) {
	t.Parallel()

	h, _ := newTestHealth(t)
	tripCircuit(t, h, testEpoch)
	evts := h.DrainEvents()
	if len(evts) == 0 {
		t.Fatal("tripping the circuit raised no event")
	}
	e := evts[len(evts)-1]
	if e.Type != EventGatewayHealthChanged {
		t.Fatalf("event type = %s", e.Type)
	}
	if e.Topic() != "pp.gateways.health.v1" {
		t.Fatalf("topic = %q", e.Topic())
	}
	// The compacted health topic must be keyed per (gateway, operation): keyed by the bare
	// gateway, compaction would retain only the most recently changed operation and a consumer
	// rebuilding from the log would believe every other circuit is closed.
	if want := "stripe:" + shared.OpAuthorize.String(); e.AggregateID() != want {
		t.Fatalf("partition key = %q, want %q", e.AggregateID(), want)
	}
	if e.TenantID != "" || e.MerchantID != "" {
		t.Fatal("health events must not be stamped with a tenant or merchant; health is not per merchant")
	}
	if len(h.DrainEvents()) != 0 {
		t.Fatal("draining twice returned events the second time")
	}
}

func TestScore(t *testing.T) {
	t.Parallel()

	t.Run("never observed is exactly neutral", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHealth(t)
		if got := h.Score(); got != NeutralScore {
			t.Fatalf("Score() = %v, want %v", got, NeutralScore)
		}
	})

	t.Run("healthy and busy scores near one", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHealth(t)
		observeN(t, h, 100, ObservationSuccess, 50*time.Millisecond, testEpoch)
		if got := h.Score(); got < 0.95 {
			t.Fatalf("Score() = %v, want > 0.95", got)
		}
	})

	t.Run("unhealthy scores zero and does not decay back toward neutral", func(t *testing.T) {
		t.Parallel()
		h, clk := newTestHealth(t)
		tripCircuit(t, h, testEpoch)
		if got := h.Score(); got != 0 {
			t.Fatalf("Score() = %v, want 0", got)
		}
		// The reason an open circuit has no recent traffic is the open circuit. Decaying toward
		// neutral would let a gateway climb the rankings precisely because it is broken.
		clk.Advance(10 * ScoreDecayHorizon)
		if got := h.Score(); got != 0 {
			t.Fatalf("Score() after %s = %v, want 0", 10*ScoreDecayHorizon, got)
		}
	})

	t.Run("degraded ranks below healthy", func(t *testing.T) {
		t.Parallel()
		healthy, _ := newTestHealth(t)
		observeN(t, healthy, 100, ObservationSuccess, 50*time.Millisecond, testEpoch)

		degraded, _ := newTestHealth(t)
		observeN(t, degraded, 90, ObservationSuccess, 50*time.Millisecond, testEpoch)
		observeN(t, degraded, 10, ObservationError, 50*time.Millisecond, testEpoch)
		if degraded.State() != HealthDegraded {
			t.Fatalf("state = %s, want DEGRADED", degraded.State())
		}
		if degraded.Score() >= healthy.Score() {
			t.Fatalf("degraded score %v is not below healthy score %v", degraded.Score(), healthy.Score())
		}
	})

	t.Run("a stale score decays toward neutral", func(t *testing.T) {
		t.Parallel()
		h, clk := newTestHealth(t)
		observeN(t, h, 100, ObservationSuccess, 50*time.Millisecond, testEpoch)
		fresh := h.Score()

		clk.Advance(ScoreDecayHorizon / 2)
		half := h.Score()
		if !(half < fresh && half > NeutralScore) {
			t.Fatalf("half-decayed score = %v, want strictly between %v and %v", half, NeutralScore, fresh)
		}

		clk.Advance(ScoreDecayHorizon)
		stale := h.Score()
		if stale != NeutralScore {
			t.Fatalf("fully decayed score = %v, want exactly %v", stale, NeutralScore)
		}
	})

	t.Run("slow but correct scores below fast and correct", func(t *testing.T) {
		t.Parallel()
		fast, _ := newTestHealth(t)
		observeN(t, fast, 100, ObservationSuccess, 20*time.Millisecond, testEpoch)

		slow, _ := newTestHealth(t)
		observeN(t, slow, 100, ObservationSuccess, 4*time.Second, testEpoch)
		if slow.State() != HealthHealthy {
			t.Fatalf("state = %s, want HEALTHY below the p99 threshold", slow.State())
		}
		if slow.Score() >= fast.Score() {
			t.Fatalf("slow score %v is not below fast score %v", slow.Score(), fast.Score())
		}
	})
}

func TestSuccessRateAndCountersUseTheRollingWindow(t *testing.T) {
	t.Parallel()

	h, clk := newTestHealth(t)
	if h.SuccessRate() != 1 {
		t.Fatalf("empty window success rate = %v, want 1", h.SuccessRate())
	}

	observeN(t, h, 75, ObservationSuccess, 10*time.Millisecond, testEpoch)
	observeN(t, h, 25, ObservationError, 10*time.Millisecond, testEpoch)
	if got := h.SuccessRate(); got != 0.75 {
		t.Fatalf("success rate = %v, want 0.75", got)
	}

	// Observations older than the window are gone, without a background goroutine having run.
	clk.Advance(HealthWindow + time.Second)
	if total, _, _, _ := h.Counters(); total != 0 {
		t.Fatalf("window total = %d after %s, want 0", total, HealthWindow+time.Second)
	}
	if got := h.SuccessRate(); got != 1 {
		t.Fatalf("aged-out window success rate = %v, want 1", got)
	}
}

func TestRollingWindowAgesOutBucketByBucket(t *testing.T) {
	t.Parallel()

	h, _ := newTestHealth(t)
	// One observation per second across two full windows. At every point the window must hold at
	// most windowBuckets observations, which is what proves the ring is bounded rather than
	// growing.
	for i := 0; i < 2*windowBuckets; i++ {
		at := testEpoch.Add(time.Duration(i) * bucketWidth)
		if _, err := h.Observe(ObservationHardDecline, time.Millisecond, at); err != nil {
			t.Fatalf("Observe: %v", err)
		}
		total, _, _, _ := h.Counters()
		want := i + 1
		if want > windowBuckets {
			want = windowBuckets
		}
		// Counters() reads at the clock's now, which is still the epoch; read at the observation
		// instant instead by observing and re-checking via the window directly.
		got := h.window.totals(at).total
		if got != want {
			t.Fatalf("at +%d buckets: window total = %d, want %d (Counters reported %d)",
				i, got, want, total)
		}
	}
}

func TestLatencyRingIsBounded(t *testing.T) {
	t.Parallel()

	var r latencyRing
	if r.percentile(0.99) != 0 {
		t.Fatal("an empty ring reported a non-zero percentile")
	}

	// Fill the ring twice over. Only the most recent latencyCapacity samples survive, which is
	// what makes the memory footprint of one health record a constant.
	for i := 0; i < latencyCapacity; i++ {
		r.add(time.Millisecond)
	}
	if got := r.percentile(0.99); got != time.Millisecond {
		t.Fatalf("p99 = %s, want 1ms", got)
	}
	for i := 0; i < latencyCapacity; i++ {
		r.add(10 * time.Millisecond)
	}
	if got := r.percentile(0.99); got != 10*time.Millisecond {
		t.Fatalf("p99 after overwriting the ring = %s, want 10ms; old samples were retained", got)
	}
	if r.n != latencyCapacity {
		t.Fatalf("ring holds %d samples, want %d", r.n, latencyCapacity)
	}

	// Nearest rank, no interpolation: every reported percentile is a duration that was actually
	// observed.
	var mixed latencyRing
	for i := 1; i <= 100; i++ {
		mixed.add(time.Duration(i) * time.Millisecond)
	}
	if got := mixed.percentile(0.99); got != 99*time.Millisecond {
		t.Fatalf("p99 of 1..100ms = %s, want 99ms", got)
	}
	if got := mixed.percentile(0.5); got != 50*time.Millisecond {
		t.Fatalf("p50 of 1..100ms = %s, want 50ms", got)
	}

	// Non-positive samples are ignored rather than recorded as instantaneous responses.
	var zeros latencyRing
	zeros.add(0)
	zeros.add(-time.Second)
	if zeros.n != 0 {
		t.Fatalf("ring recorded %d non-positive samples", zeros.n)
	}
}

func TestRehydrateHealthRestoresTheCircuitButNotTheWindow(t *testing.T) {
	t.Parallel()

	clk := &shared.FixedClock{T: testEpoch}
	h, err := RehydrateHealth(RehydrateHealthParams{
		GatewayID:     "stripe",
		Operation:     shared.OpRefund,
		State:         HealthUnhealthy,
		Cooldown:      2 * time.Minute,
		CooldownUntil: testEpoch.Add(time.Minute),
		Version:       12,
	}, clk)
	if err != nil {
		t.Fatalf("RehydrateHealth: %v", err)
	}
	// A rolling deploy that forgot the circuit position would re-open every circuit the fleet had
	// carefully closed.
	if h.State() != HealthUnhealthy || h.Cooldown() != 2*time.Minute {
		t.Fatalf("state = %s cooldown = %s", h.State(), h.Cooldown())
	}
	if allowed, _ := h.AdmitRequest(testEpoch); allowed {
		t.Fatal("a rehydrated open circuit admitted a request before its cooldown")
	}
	// The window is deliberately not restored: a new process has not observed anything.
	if total, _, _, _ := h.Counters(); total != 0 {
		t.Fatalf("rehydrated window total = %d, want 0", total)
	}

	if _, err := RehydrateHealth(RehydrateHealthParams{
		GatewayID: "stripe", Operation: shared.OpRefund, State: "MELTED",
	}, clk); apierror.CodeOf(err) != apierror.CodeInternalError {
		t.Fatalf("unknown state: code = %s, want INTERNAL_ERROR", apierror.CodeOf(err))
	}

	// A persisted cooldown outside the legal range is clamped rather than trusted.
	clamped, err := RehydrateHealth(RehydrateHealthParams{
		GatewayID: "stripe", Operation: shared.OpRefund, State: HealthHealthy,
		Cooldown: 10 * MaxCooldown,
	}, clk)
	if err != nil {
		t.Fatalf("RehydrateHealth: %v", err)
	}
	if clamped.Cooldown() != MaxCooldown {
		t.Fatalf("cooldown = %s, want the cap %s", clamped.Cooldown(), MaxCooldown)
	}
}

// tripCircuit drives a healthy record straight to UNHEALTHY with an unambiguous error rate.
func tripCircuit(t *testing.T, h *Health, at time.Time) {
	t.Helper()
	observeN(t, h, MinSamples*2, ObservationError, time.Second, at)
	if h.State() != HealthUnhealthy {
		t.Fatalf("failed to trip the circuit: state = %s", h.State())
	}
}
