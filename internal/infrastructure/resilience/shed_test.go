package resilience

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// pressureVar is an atomically-settable pressure signal, so a test can drive the shedder
// without the shedder having to be told about tests.
type pressureVar struct{ v atomic.Uint64 }

func (p *pressureVar) set(f float64) { p.v.Store(uint64(f * 1e6)) }
func (p *pressureVar) get() float64  { return float64(p.v.Load()) / 1e6 }

func allClasses() []PriorityClass {
	return []PriorityClass{
		PriorityMoneyOut, PriorityCapture, PriorityAuthorize,
		PriorityRead, PriorityReport, PriorityBackground,
	}
}

func TestPriorityClassOrderingIsTheShedOrder(t *testing.T) {
	t.Parallel()

	// The numeric order encodes the policy: money-out is 0 and is shed last; background is 5
	// and is shed first. A reordering of this constant block would silently invert the whole
	// degradation ladder, so it is asserted directly.
	want := []struct {
		c    PriorityClass
		n    int
		s    string
		rung int
	}{
		{PriorityMoneyOut, 0, "P0", 8},
		{PriorityCapture, 1, "P1", 7},
		{PriorityAuthorize, 2, "P2", 6},
		{PriorityRead, 3, "P3", 4},
		{PriorityReport, 4, "P4", 2},
		{PriorityBackground, 5, "P5", 1},
	}
	for _, w := range want {
		if int(w.c) != w.n {
			t.Errorf("%s = %d, want %d", w.s, int(w.c), w.n)
		}
		if w.c.String() != w.s {
			t.Errorf("String() = %q, want %q", w.c.String(), w.s)
		}
		if got := rungOf(w.c); got != w.rung {
			t.Errorf("%s maps to rung %d, want %d (docs/failure-handling.md §4)", w.s, got, w.rung)
		}
		if !w.c.Valid() {
			t.Errorf("%s is not Valid()", w.s)
		}
		if w.c.Operations() == "" {
			t.Errorf("%s has no operation description", w.s)
		}
	}
	if PriorityMoneyOut.Operations() != "refund, void, dispute handling, webhook ingest" {
		t.Errorf("P0 operations = %q; refunds, voids, disputes and webhook ingest must all be P0",
			PriorityMoneyOut.Operations())
	}
}

// TestShedOrderUnderIncreasingPressure walks the pressure up and asserts, at every step,
// exactly which classes are still admitted. This is the whole policy in one table: refunds,
// voids, disputes and webhook ingest survive every level of pressure, and new payments are shed
// before them.
func TestShedOrderUnderIncreasingPressure(t *testing.T) {
	t.Parallel()

	steps := []struct {
		pressure float64
		rung     int
		// admitted lists the classes that must still be served at this pressure.
		admitted []PriorityClass
	}{
		{
			pressure: 0.0, rung: 0,
			admitted: allClasses(),
		},
		{
			pressure: 0.15, rung: 0,
			admitted: allClasses(),
		},
		{
			// rung 1: pause background work first — nobody is waiting on a response.
			pressure: 0.22, rung: 1,
			admitted: []PriorityClass{PriorityMoneyOut, PriorityCapture, PriorityAuthorize, PriorityRead, PriorityReport},
		},
		{
			// rung 2: reports and exports go next.
			pressure: 0.30, rung: 2,
			admitted: []PriorityClass{PriorityMoneyOut, PriorityCapture, PriorityAuthorize, PriorityRead},
		},
		{
			// rung 4: list and status reads.
			pressure: 0.60, rung: 4,
			admitted: []PriorityClass{PriorityMoneyOut, PriorityCapture, PriorityAuthorize},
		},
		{
			// rung 6: new payments. Money-in is shed before money-out.
			pressure: 0.80, rung: 6,
			admitted: []PriorityClass{PriorityMoneyOut, PriorityCapture},
		},
		{
			// rung 7: captures.
			pressure: 0.95, rung: 7,
			admitted: []PriorityClass{PriorityMoneyOut},
		},
		{
			// rung 8 territory: money-out only, and money-out still runs.
			pressure: 1.50, rung: 7,
			admitted: []PriorityClass{PriorityMoneyOut},
		},
		{
			// Catastrophic pressure. P0 is still admitted, because you must always be able to
			// give money back (baseline §8).
			pressure: 1000, rung: 7,
			admitted: []PriorityClass{PriorityMoneyOut},
		},
	}

	p := &pressureVar{}
	s := NewShedder(ShedderConfig{Pressure: p.get, Clock: NewManualClock(time.Time{})})

	for _, step := range steps {
		p.set(step.pressure)

		want := map[PriorityClass]bool{}
		for _, c := range step.admitted {
			want[c] = true
		}
		for _, c := range allClasses() {
			if got := s.Allow(c); got != want[c] {
				t.Errorf("pressure %.2f: Allow(%s) = %v, want %v", step.pressure, c, got, want[c])
			}
		}
		if got := s.Rung(); got != step.rung {
			t.Errorf("pressure %.2f: rung = %d, want %d", step.pressure, got, step.rung)
		}
	}
}

// TestMoneyOutIsNeverShed is the property stated on its own so it cannot be lost in a table.
func TestMoneyOutIsNeverShed(t *testing.T) {
	t.Parallel()

	p := &pressureVar{}
	s := NewShedder(ShedderConfig{Pressure: p.get, Clock: NewManualClock(time.Time{})})

	for _, pressure := range []float64{0, 0.5, 0.9, 1.0, 1.0000001, 2, 10, 1e6} {
		p.set(pressure)
		if !s.Allow(PriorityMoneyOut) {
			t.Fatalf("a refund was shed at pressure %v: an availability incident has just been "+
				"converted into a consumer-harm incident", pressure)
		}
		if err := s.Admit(PriorityMoneyOut); err != nil {
			t.Fatalf("Admit(P0) at pressure %v returned %v", pressure, err)
		}
	}
	if got := s.ShedCount(PriorityMoneyOut); got != 0 {
		t.Fatalf("P0 shed count = %d, want 0", got)
	}
}

// TestMoneyInIsShedBeforeMoneyOut states the ordering directly: there must exist a pressure at
// which authorizations are rejected and refunds are not.
func TestMoneyInIsShedBeforeMoneyOut(t *testing.T) {
	t.Parallel()

	p := &pressureVar{}
	s := NewShedder(ShedderConfig{Pressure: p.get, Clock: NewManualClock(time.Time{})})

	p.set(0.80)
	if s.Allow(PriorityAuthorize) {
		t.Fatal("a new payment was admitted at pressure 0.80")
	}
	if !s.Allow(PriorityMoneyOut) {
		t.Fatal("a refund was shed at the same pressure at which a new payment was shed")
	}
	if !s.Allow(PriorityCapture) {
		t.Fatal("a capture was shed before an authorization: money already promised outranks money not yet taken")
	}
}

func TestShedderAdmitReturnsAServiceUnavailable(t *testing.T) {
	t.Parallel()

	p := &pressureVar{}
	s := NewShedder(ShedderConfig{Pressure: p.get, Clock: NewManualClock(time.Time{})})
	p.set(0.60)

	err := s.Admit(PriorityRead)
	if err == nil {
		t.Fatal("want a shed error")
	}
	if got := apierror.CodeOf(err); got != apierror.CodeServiceUnavailable {
		t.Errorf("code = %s, want %s", got, apierror.CodeServiceUnavailable)
	}
	if got := apierror.HTTPStatusOf(err); got != 503 {
		t.Errorf("status = %d, want 503", got)
	}
	if !apierror.IsRetryable(err) {
		t.Error("a shed must be retryable: clients are told the truth so their own backoff behaves")
	}
	pe := apierror.From(err)
	if pe.RetryAfterSeconds <= 0 {
		t.Error("no Retry-After on a shed response")
	}
}

func TestShedderRetryAfterDeepensWithTheRung(t *testing.T) {
	t.Parallel()

	p := &pressureVar{}
	s := NewShedder(ShedderConfig{Pressure: p.get, Clock: NewManualClock(time.Time{})})

	p.set(0.22) // rung 1
	shallow := apierror.From(s.Admit(PriorityBackground)).RetryAfterSeconds

	p.set(0.95) // rung 7
	deep := apierror.From(s.Admit(PriorityBackground)).RetryAfterSeconds

	if deep <= shallow {
		t.Fatalf("Retry-After at rung 7 (%ds) is not longer than at rung 1 (%ds): a client backing "+
			"off during a severe incident would spend it re-asking", deep, shallow)
	}
}

func TestShedderCountsSheds(t *testing.T) {
	t.Parallel()

	p := &pressureVar{}
	s := NewShedder(ShedderConfig{Pressure: p.get, Clock: NewManualClock(time.Time{})})
	p.set(0.60)

	for i := 0; i < 7; i++ {
		s.Allow(PriorityRead)
	}
	if got := s.ShedCount(PriorityRead); got != 7 {
		t.Fatalf("shed count = %d, want 7", got)
	}
	// Engaged must not count a shed: it is a metrics read, not a decision.
	for i := 0; i < 3; i++ {
		s.Engaged(PriorityRead)
	}
	if got := s.ShedCount(PriorityRead); got != 7 {
		t.Fatalf("shed count = %d after three Engaged calls, want 7", got)
	}
}

// TestShedderHysteresis: a rung is climbed down only after its trigger has been clear for the
// hysteresis window. Without it the system oscillates — shedding lowers pressure, lower pressure
// stops the shedding, the load returns — and a merchant sees intermittent 503s instead of a
// clean, stable degradation.
func TestShedderHysteresis(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	p := &pressureVar{}
	s := NewShedder(ShedderConfig{Pressure: p.get, Clock: clk})

	p.set(0.60)
	if s.Allow(PriorityRead) {
		t.Fatal("reads were admitted at pressure 0.60")
	}

	// Pressure clears, but not for long enough.
	p.set(0.0)
	if s.Allow(PriorityRead) {
		t.Fatal("reads were readmitted the instant pressure cleared: there is no hysteresis")
	}
	clk.Advance(DefaultShedHysteresis - time.Second)
	if s.Allow(PriorityRead) {
		t.Fatalf("reads were readmitted %v early", time.Second)
	}
	clk.Advance(2 * time.Second)
	if !s.Allow(PriorityRead) {
		t.Fatal("reads were still shed after the hysteresis window elapsed")
	}
}

func TestShedderHysteresisResetsOnAReTrigger(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	p := &pressureVar{}
	s := NewShedder(ShedderConfig{Pressure: p.get, Clock: clk})

	p.set(0.60)
	s.Allow(PriorityRead)

	p.set(0.0)
	clk.Advance(DefaultShedHysteresis - time.Second)
	p.set(0.60) // pressure returns just before the rung would have been climbed down
	s.Allow(PriorityRead)
	p.set(0.0)
	clk.Advance(2 * time.Second)

	if s.Allow(PriorityRead) {
		t.Fatal("the hysteresis timer was not restarted by the re-trigger")
	}
}

func TestShedderRungChangedCallback(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	p := &pressureVar{}
	var mu sync.Mutex
	var rungs []int
	s := NewShedder(ShedderConfig{
		Pressure: p.get,
		Clock:    clk,
		RungChanged: func(_, to int) {
			mu.Lock()
			defer mu.Unlock()
			rungs = append(rungs, to)
		},
	})

	for _, pressure := range []float64{0.22, 0.30, 0.60, 0.80, 0.95} {
		p.set(pressure)
		s.Allow(PriorityMoneyOut)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []int{1, 2, 4, 6, 7}
	if len(rungs) != len(want) {
		t.Fatalf("rungs = %v, want %v", rungs, want)
	}
	for i := range want {
		if rungs[i] != want[i] {
			t.Fatalf("rungs = %v, want %v", rungs, want)
		}
	}
}

func TestShedderShedCallback(t *testing.T) {
	t.Parallel()

	p := &pressureVar{}
	var mu sync.Mutex
	var seen []PriorityClass
	s := NewShedder(ShedderConfig{
		Pressure: p.get,
		Clock:    NewManualClock(time.Time{}),
		Shed: func(c PriorityClass, _ float64) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, c)
		},
	})

	p.set(0.60)
	s.Allow(PriorityReport)
	s.Allow(PriorityMoneyOut)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != PriorityReport {
		t.Fatalf("shed callback saw %v, want [P4]", seen)
	}
}

func TestShedderUnknownClassIsTreatedAsLowestPriority(t *testing.T) {
	t.Parallel()

	p := &pressureVar{}
	s := NewShedder(ShedderConfig{Pressure: p.get, Clock: NewManualClock(time.Time{})})
	p.set(0.22)

	if s.Allow(PriorityClass(99)) {
		t.Fatal("an unclassified operation was admitted: unclassified work is work nobody has argued for")
	}
}

func TestShedderNilPressureAdmitsEverything(t *testing.T) {
	t.Parallel()

	s := NewShedder(ShedderConfig{Clock: NewManualClock(time.Time{})})
	for _, c := range allClasses() {
		if !s.Allow(c) {
			t.Fatalf("%s was shed with no pressure signal configured", c)
		}
	}
}

func TestShedderCustomThresholds(t *testing.T) {
	t.Parallel()

	th := DefaultShedThresholds
	th[PriorityReport] = 0.9
	p := &pressureVar{}
	s := NewShedder(ShedderConfig{Pressure: p.get, Thresholds: th, Clock: NewManualClock(time.Time{})})

	p.set(0.5)
	if !s.Allow(PriorityReport) {
		t.Fatal("the custom threshold was ignored")
	}
	if s.Allow(PriorityBackground) {
		t.Fatal("the other thresholds were not preserved")
	}
}

// TestShedderIntegratesWithTheAdaptiveLimiter wires the two together the way the request
// pipeline does: the limiter's pressure is the shedder's signal.
func TestShedderIntegratesWithTheAdaptiveLimiter(t *testing.T) {
	t.Parallel()

	clk := NewManualClock(time.Time{})
	limiter := newTestLimiter(clk, func(c *AdaptiveConfig) { c.InitialLimit = 10; c.MinLimit = 1 })
	s := NewShedder(ShedderConfig{Pressure: limiter.Pressure, Clock: clk})

	// Saturate the limiter.
	releases := make([]func(time.Duration, bool), 0, 10)
	for i := 0; i < 10; i++ {
		r, err := limiter.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		releases = append(releases, r)
	}
	if got := limiter.Pressure(); got != 1 {
		t.Fatalf("pressure = %v, want 1", got)
	}
	if s.Allow(PriorityAuthorize) {
		t.Fatal("a new payment was admitted with the limiter fully saturated")
	}
	if !s.Allow(PriorityMoneyOut) {
		t.Fatal("a refund was shed with the limiter fully saturated")
	}
	for _, r := range releases {
		r(time.Millisecond, false)
	}
}

func TestShedderConcurrentUseIsRaceFree(t *testing.T) {
	t.Parallel()

	p := &pressureVar{}
	s := NewShedder(ShedderConfig{Pressure: p.get, Clock: SystemClock(), Hysteresis: time.Millisecond})

	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				p.set(float64((g+i)%12) / 10)
				_ = s.Allow(PriorityClass((g + i) % 6))
				_ = s.Rung()
				_ = s.Engaged(PriorityAuthorize)
				_ = s.ShedCount(PriorityRead)
			}
		}(g)
	}
	wg.Wait()

	// However the pressure flapped, no refund may ever have been rejected.
	if got := s.ShedCount(PriorityMoneyOut); got != 0 {
		t.Fatalf("P0 shed count = %d under concurrent pressure changes, want 0", got)
	}
}
