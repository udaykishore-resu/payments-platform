package routing_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/routing"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

var decidedAt = time.Date(2026, 8, 26, 14, 3, 11, 402_000_000, time.UTC)

// eligible returns a candidate that passes every hard filter, so a test can express exactly the
// one thing it is about by mutating a single field.
func eligible(id shared.GatewayID) routing.Candidate {
	return routing.Candidate{
		GatewayID:          id,
		TenantEntitled:     true,
		ResidencyCompliant: true,
		MerchantConfigured: true,
		Certified:          true,
		CircuitOpen:        false,
		Healthy:            true,
		SupportsCurrency:   true,
		SupportsMethod:     true,
		SupportsCountry:    true,
		SupportsOperation:  true,
		SupportsThreeDS:    true,
		HealthScore:        routing.HealthScoreHealthy,
		SuccessRate:        0.95,
		CostMinorUnits:     200,
		LatencyP99MS:       500,
	}
}

func cardRequest() routing.RequestContext {
	return routing.RequestContext{
		TenantID:      "ten_x",
		MerchantID:    "mrc_x",
		PaymentID:     "pay_01JB8Z9K2QW3E4R5T6Y7U8I9O0",
		Amount:        usd(8450),
		PaymentMethod: shared.MethodCard,
		PayerCountry:  shared.Country("US"),
		RiskBand:      routing.RiskBandLow,
		Operation:     shared.OpAuthorize,
	}
}

func weightedPolicy() routing.Policy {
	return routing.Policy{
		Strategy:  routing.StrategyWeightedScore,
		Primary:   "stripe",
		Fallbacks: []shared.GatewayID{"adyen", "paypal"},
		Weights:   routing.DefaultWeights(),
	}
}

func gatewayOrder(t *testing.T, plan *routing.Plan) []shared.GatewayID {
	t.Helper()
	out := make([]shared.GatewayID, 0, len(plan.Selections()))
	for _, s := range plan.Selections() {
		out = append(out, s.GatewayID)
	}
	return out
}

func assertOrder(t *testing.T, plan *routing.Plan, want ...shared.GatewayID) {
	t.Helper()
	got := gatewayOrder(t, plan)
	if len(got) != len(want) {
		t.Fatalf("plan order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("plan order = %v, want %v", got, want)
		}
	}
	for i, s := range plan.Selections() {
		if s.Rank != i+1 {
			t.Fatalf("selection %s has rank %d, want %d", s.GatewayID, s.Rank, i+1)
		}
	}
}

// --- the documented worked example ------------------------------------------------------------

// docTolerance is the agreement required with the published figures in docs/data-plane.md §4.3.
//
// The document computes its final scores from intermediate values it has already rounded to
// three decimal places (S = 0.707, 0.768, 0.548), so a full-precision implementation cannot
// reproduce its fourth decimal exactly and should not try: agreeing to within 5e-4 means the
// implementation and the document agree on every digit the document actually determines.
const docTolerance = 5e-4

func TestDecideReproducesTheDocumentedWorkedExample(t *testing.T) {
	// Verifies: BR-13, FR-62.
	t.Parallel()

	// docs/data-plane.md §4.3: merchant card sale, USD 84.50, customer country US, three
	// certified connections, default weights, merchant 30-day baseline prior = 0.930.
	const prior = 0.930

	stripe := eligible("stripe")
	stripe.HealthScore = routing.HealthScoreHealthy
	stripe.SuccessRate = routing.SmoothSuccessRate(3881, 4120, prior)
	stripe.CostMinorUnits = 275
	stripe.LatencyP99MS = 620

	adyen := eligible("adyen")
	adyen.HealthScore = routing.HealthScoreDegraded
	adyen.SuccessRate = routing.SmoothSuccessRate(837, 880, prior)
	adyen.CostMinorUnits = 219
	adyen.LatencyP99MS = 1180

	paypal := eligible("paypal")
	paypal.HealthScore = routing.HealthScoreHealthy
	paypal.SuccessRate = routing.SmoothSuccessRate(193, 210, prior)
	paypal.CostMinorUnits = 344
	paypal.LatencyP99MS = 890

	plan, err := routing.Decide(weightedPolicy(), cardRequest(),
		[]routing.Candidate{paypal, stripe, adyen}, decidedAt)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	// Plan: [stripe (0.8018), adyen (0.6511), paypal (0.6347)].
	assertOrder(t, plan, "stripe", "adyen", "paypal")

	wantScores := map[shared.GatewayID]float64{
		"stripe": 0.8018,
		"adyen":  0.6511,
		"paypal": 0.6347,
	}
	// The published per-factor normalizations, to the precision the document states them.
	//
	// factorTolerance is looser than docTolerance for one specific reason: the document's adyen
	// row states ŝ = 0.9499 where (837 + 50·0.930)/(880 + 50) = 883.5/930 is exactly 0.9500 — a
	// rounding slip in the document, not a disagreement about the formula. It moves the published
	// S by 0.0012. The final score still agrees to within 3.4e-4, which is why the score
	// assertion above can stay tight while this one cannot.
	const factorTolerance = 2e-3
	wantFactors := map[shared.GatewayID]routing.ScoreBreakdown{
		"stripe": {Health: 1.000, SuccessRate: 0.707, Cost: 0.552, Latency: 0.793},
		"adyen":  {Health: 0.400, SuccessRate: 0.768, Cost: 1.000, Latency: 0.607},
		"paypal": {Health: 1.000, SuccessRate: 0.548, Cost: 0.000, Latency: 0.703},
	}

	for _, s := range plan.Selections() {
		if diff := math.Abs(s.Score - wantScores[s.GatewayID]); diff > docTolerance {
			t.Errorf("%s score = %.6f, documented %.4f (diff %.6f > %.6f)",
				s.GatewayID, s.Score, wantScores[s.GatewayID], diff, docTolerance)
		}
		if s.ScoreBreakdown.Weighted != s.Score {
			t.Errorf("%s: breakdown weighted %.6f does not match recorded score %.6f",
				s.GatewayID, s.ScoreBreakdown.Weighted, s.Score)
		}
		want := wantFactors[s.GatewayID]
		for _, f := range []struct {
			name        string
			got, wanted float64
		}{
			{"H", s.ScoreBreakdown.Health, want.Health},
			{"S", s.ScoreBreakdown.SuccessRate, want.SuccessRate},
			{"C", s.ScoreBreakdown.Cost, want.Cost},
			{"L", s.ScoreBreakdown.Latency, want.Latency},
		} {
			if math.Abs(f.got-f.wanted) > factorTolerance {
				t.Errorf("%s %s = %.6f, documented %.3f", s.GatewayID, f.name, f.got, f.wanted)
			}
		}
	}

	// A 0.15 gap is far outside the tie tolerance, so no tie-break should be recorded — the
	// document's own persisted plan has "tieBreak": null.
	if plan.TieBreak != "" {
		t.Errorf("expected no tie-break, got %q", plan.TieBreak)
	}
	if plan.CreatedAt != decidedAt {
		t.Errorf("plan created at %v, want %v", plan.CreatedAt, decidedAt)
	}
	if plan.PaymentID != cardRequest().PaymentID {
		t.Errorf("plan attributed to %s", plan.PaymentID)
	}
}

// The instructive half of the worked example: Adyen has the best success rate *and* the lowest
// cost and still loses, because DEGRADED costs it 0.24 of a possible 0.40. Restore it to HEALTHY
// and nothing else changes and it scores 0.8911, taking first place.
func TestHealthDominatesTheScore(t *testing.T) {
	t.Parallel()

	const prior = 0.930

	stripe := eligible("stripe")
	stripe.SuccessRate = routing.SmoothSuccessRate(3881, 4120, prior)
	stripe.CostMinorUnits = 275
	stripe.LatencyP99MS = 620

	adyen := eligible("adyen")
	adyen.HealthScore = routing.HealthScoreHealthy // the only change
	adyen.SuccessRate = routing.SmoothSuccessRate(837, 880, prior)
	adyen.CostMinorUnits = 219
	adyen.LatencyP99MS = 1180

	paypal := eligible("paypal")
	paypal.SuccessRate = routing.SmoothSuccessRate(193, 210, prior)
	paypal.CostMinorUnits = 344
	paypal.LatencyP99MS = 890

	plan, err := routing.Decide(weightedPolicy(), cardRequest(),
		[]routing.Candidate{stripe, adyen, paypal}, decidedAt)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	assertOrder(t, plan, "adyen", "stripe", "paypal")

	top, _ := plan.Primary()
	if diff := math.Abs(top.Score - 0.8911); diff > docTolerance {
		t.Fatalf("healthy adyen scores %.6f, documented 0.8911", top.Score)
	}
}

func TestSmoothSuccessRate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		successes, samples int64
		prior, want        float64
	}{
		{"documented stripe window", 3881, 4120, 0.930, 0.9418},
		{"documented paypal window", 193, 210, 0.930, 0.9212},
		{
			// Without smoothing this would be a perfect 1.0 and would outrank a gateway with
			// four thousand samples at 94.2%. With alpha = 50 the six observations move the
			// merchant's own baseline by less than a point.
			name:      "six samples and six successes cannot buy a perfect rate",
			successes: 6, samples: 6, prior: 0.930, want: 0.9375,
		},
		{"no observations fall back to the merchant's own baseline", 0, 0, 0.930, 0.930},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := routing.SmoothSuccessRate(tt.successes, tt.samples, tt.prior)
			if math.Abs(got-tt.want) > 1e-3 {
				t.Fatalf("SmoothSuccessRate = %.6f, want %.4f", got, tt.want)
			}
		})
	}

	fresh := routing.SmoothSuccessRate(6, 6, 0.930)
	established := routing.SmoothSuccessRate(3881, 4120, 0.930)
	if fresh >= established {
		t.Fatalf("a six-sample gateway (%.4f) must not outrank a four-thousand-sample one (%.4f)",
			fresh, established)
	}
}

// --- degenerate normalisation -------------------------------------------------------------------

// The naive expression 1 - (c-min)/(max-min) evaluates 0/0 to NaN when every candidate has the
// same cost. A NaN weighted score makes every comparison false and the sort order arbitrary, so
// the engine appears to work while routing at random.
func TestIdenticalCostsProduceNoNaN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		candidates []routing.Candidate
	}{
		{
			name: "every candidate on the same flat rate",
			candidates: func() []routing.Candidate {
				a, b, c := eligible("adyen"), eligible("braintree"), eligible("stripe")
				a.CostMinorUnits, b.CostMinorUnits, c.CostMinorUnits = 250, 250, 250
				return []routing.Candidate{a, b, c}
			}(),
		},
		{
			name:       "a single candidate, where min and max are trivially equal",
			candidates: []routing.Candidate{eligible("stripe")},
		},
		{
			name: "a zero-amount verification where every cost is zero",
			candidates: func() []routing.Candidate {
				a, b := eligible("adyen"), eligible("stripe")
				a.CostMinorUnits, b.CostMinorUnits = 0, 0
				return []routing.Candidate{a, b}
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plan, err := routing.Decide(weightedPolicy(), cardRequest(), tt.candidates, decidedAt)
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			for _, s := range plan.Selections() {
				for name, v := range map[string]float64{
					"health": s.ScoreBreakdown.Health, "success": s.ScoreBreakdown.SuccessRate,
					"cost": s.ScoreBreakdown.Cost, "latency": s.ScoreBreakdown.Latency,
					"weighted": s.Score,
				} {
					if math.IsNaN(v) || math.IsInf(v, 0) {
						t.Fatalf("%s %s factor is %v", s.GatewayID, name, v)
					}
				}
				// When cost cannot distinguish candidates it must not penalize any of them.
				if s.ScoreBreakdown.Cost != 1.0 {
					t.Errorf("%s cost factor = %v, want 1.0 when every cost is identical",
						s.GatewayID, s.ScoreBreakdown.Cost)
				}
			}
		})
	}
}

// A caller can hand the engine a NaN health score from a zero-sample window. One NaN in the
// weighted sum makes the whole ordering arbitrary, so it must be clamped rather than propagated.
func TestNaNInputIsClampedRatherThanPropagated(t *testing.T) {
	t.Parallel()

	bad := eligible("stripe")
	bad.HealthScore = math.NaN()
	bad.SuccessRate = math.NaN()

	plan, err := routing.Decide(weightedPolicy(), cardRequest(), []routing.Candidate{bad}, decidedAt)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	top, _ := plan.Primary()
	if math.IsNaN(top.Score) {
		t.Fatal("a NaN input produced a NaN score")
	}
}

// --- hard filters ------------------------------------------------------------------------------

// A hard filter is absolute. No weight, and no amount of cheapness or speed, may resurrect a
// filtered candidate — because scoring implies the dimensions are commensurable and eligibility
// is not one of them.
func TestHardFiltersAreNeverOverriddenByAHighScore(t *testing.T) {
	t.Parallel()

	// The disqualified candidate is perfect on every scoring dimension: free, instant, healthy
	// and with a flawless success rate. The eligible one is expensive, slow and mediocre.
	perfectButIneligible := func(mutate func(*routing.Candidate)) routing.Candidate {
		c := eligible("adyen")
		c.CostMinorUnits = 0
		c.LatencyP99MS = 0
		c.SuccessRate = 1.0
		c.HealthScore = routing.HealthScoreHealthy
		mutate(&c)
		return c
	}
	mediocreButEligible := func() routing.Candidate {
		c := eligible("stripe")
		c.CostMinorUnits = 100000
		c.LatencyP99MS = 2900
		c.SuccessRate = 0.86
		c.HealthScore = routing.HealthScoreProbing
		return c
	}

	tests := []struct {
		name   string
		mutate func(*routing.Candidate)
		reason routing.RejectionReason
	}{
		{"tenant allowlist", func(c *routing.Candidate) { c.TenantEntitled = false }, routing.ReasonTenantNotEntitled},
		{"data residency", func(c *routing.Candidate) { c.ResidencyCompliant = false }, routing.ReasonResidencyViolation},
		{"merchant configuration", func(c *routing.Candidate) { c.MerchantConfigured = false }, routing.ReasonMerchantNotConfigured},
		{"certification", func(c *routing.Candidate) { c.Certified = false }, routing.ReasonNotCertified},
		{"circuit state", func(c *routing.Candidate) { c.CircuitOpen = true }, routing.ReasonCircuitOpen},
		{"health", func(c *routing.Candidate) { c.Healthy = false }, routing.ReasonUnhealthy},
		{"currency", func(c *routing.Candidate) { c.SupportsCurrency = false }, routing.ReasonCurrencyUnsupported},
		{"method", func(c *routing.Candidate) { c.SupportsMethod = false }, routing.ReasonMethodUnsupported},
		{"country", func(c *routing.Candidate) { c.SupportsCountry = false }, routing.ReasonCountryUnsupported},
		{"operation capability", func(c *routing.Candidate) { c.SupportsOperation = false }, routing.ReasonCapabilityMismatch},
		{"amount floor", func(c *routing.Candidate) { c.MinAmountMinorUnits = 900000 }, routing.ReasonAmountOutOfBounds},
		{"amount ceiling", func(c *routing.Candidate) { c.MaxAmountMinorUnits = 100 }, routing.ReasonAmountOutOfBounds},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plan, err := routing.Decide(weightedPolicy(), cardRequest(),
				[]routing.Candidate{perfectButIneligible(tt.mutate), mediocreButEligible()}, decidedAt)
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			assertOrder(t, plan, "stripe")
			rej, ok := plan.RejectionFor("adyen")
			if !ok {
				t.Fatal("the filtered candidate was not recorded as a rejection")
			}
			if rej.Reason != tt.reason {
				t.Fatalf("rejection reason = %s, want %s", rej.Reason, tt.reason)
			}
			if rej.Detail == "" {
				t.Error("a rejection must carry a detail; the reason is for counting, the detail is for the human")
			}
		})
	}
}

func TestEveryRejectionReasonIsReachable(t *testing.T) {
	t.Parallel()

	// Each case must produce exactly the named reason for the "adyen" candidate, which proves
	// no reason in the enum is documentation that lies.
	type scenario struct {
		policy    routing.Policy
		request   routing.RequestContext
		candidate routing.Candidate
	}
	base := func() scenario {
		return scenario{policy: weightedPolicy(), request: cardRequest(), candidate: eligible("adyen")}
	}

	tests := []struct {
		reason routing.RejectionReason
		setup  func(*scenario)
	}{
		{routing.ReasonTenantNotEntitled, func(s *scenario) { s.candidate.TenantEntitled = false }},
		{routing.ReasonResidencyViolation, func(s *scenario) { s.candidate.ResidencyCompliant = false }},
		{routing.ReasonMerchantNotConfigured, func(s *scenario) { s.candidate.MerchantConfigured = false }},
		{routing.ReasonNotCertified, func(s *scenario) { s.candidate.Certified = false }},
		{routing.ReasonCircuitOpen, func(s *scenario) { s.candidate.CircuitOpen = true }},
		{routing.ReasonUnhealthy, func(s *scenario) { s.candidate.Healthy = false }},
		{routing.ReasonCurrencyUnsupported, func(s *scenario) { s.candidate.SupportsCurrency = false }},
		{routing.ReasonMethodUnsupported, func(s *scenario) { s.candidate.SupportsMethod = false }},
		{routing.ReasonCountryUnsupported, func(s *scenario) { s.candidate.SupportsCountry = false }},
		{routing.ReasonCapabilityMismatch, func(s *scenario) { s.candidate.SupportsOperation = false }},
		{routing.ReasonAmountOutOfBounds, func(s *scenario) { s.candidate.MaxAmountMinorUnits = 100 }},
		{routing.ReasonThreeDSUnsupported, func(s *scenario) {
			s.request.ThreeDSRequired = true
			s.candidate.SupportsThreeDS = false
		}},
		{routing.ReasonAlreadyAttempted, func(s *scenario) {
			s.request.IsRetry = true
			s.request.AttemptedGateways = []shared.GatewayID{"adyen"}
		}},
		{routing.ReasonPinnedElsewhere, func(s *scenario) {
			s.policy.Strategy = routing.StrategyPinned
			s.policy.Primary = "stripe"
			s.policy.Fallbacks = nil
		}},
	}

	if len(tests) != len(routing.AllRejectionReasons) {
		t.Fatalf("%d reasons covered but the enum has %d; a reason nobody can produce is documentation that lies",
			len(tests), len(routing.AllRejectionReasons))
	}

	for _, tt := range tests {
		t.Run(string(tt.reason), func(t *testing.T) {
			t.Parallel()
			s := base()
			tt.setup(&s)
			plan, _ := routing.Decide(s.policy, s.request,
				[]routing.Candidate{s.candidate, eligible("stripe")}, decidedAt)
			rej, ok := plan.RejectionFor("adyen")
			if !ok {
				t.Fatalf("adyen was not rejected; plan = %v", gatewayOrder(t, plan))
			}
			if rej.Reason != tt.reason {
				t.Fatalf("reason = %s, want %s", rej.Reason, tt.reason)
			}
			if !rej.Reason.IsValid() {
				t.Fatalf("%s is not in AllRejectionReasons", rej.Reason)
			}
		})
	}
}

// A gateway with no 3DS capability is a perfectly good route for a payment that does not need
// 3DS; rejecting it unconditionally would shrink the candidate set for the exempt majority.
func TestThreeDSCapabilityIsOnlyCheckedWhenThePaymentNeedsIt(t *testing.T) {
	// Verifies: FR-68.
	t.Parallel()

	noThreeDS := eligible("adyen")
	noThreeDS.SupportsThreeDS = false

	plan, err := routing.Decide(weightedPolicy(), cardRequest(), []routing.Candidate{noThreeDS}, decidedAt)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	assertOrder(t, plan, "adyen")
}

// --- deterministic tie-breaking -----------------------------------------------------------------

func TestDeterministicTieBreaking(t *testing.T) {
	t.Parallel()

	t.Run("identical candidates order by gateway ID", func(t *testing.T) {
		t.Parallel()
		a, b, c := eligible("zeta"), eligible("alpha"), eligible("mu")
		plan, err := routing.Decide(weightedPolicy(), cardRequest(),
			[]routing.Candidate{a, b, c}, decidedAt)
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		assertOrder(t, plan, "alpha", "mu", "zeta")
		if plan.TieBreak != "deterministic gateway-ID ordering" {
			t.Fatalf("tie-break = %q", plan.TieBreak)
		}
	})

	t.Run("input order does not change the plan", func(t *testing.T) {
		t.Parallel()
		a, b, c := eligible("zeta"), eligible("alpha"), eligible("mu")
		orders := [][]routing.Candidate{
			{a, b, c}, {c, b, a}, {b, a, c}, {c, a, b},
		}
		var first []shared.GatewayID
		for i, in := range orders {
			plan, err := routing.Decide(weightedPolicy(), cardRequest(), in, decidedAt)
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			got := gatewayOrder(t, plan)
			if i == 0 {
				first = got
				continue
			}
			for j := range got {
				if got[j] != first[j] {
					t.Fatalf("permutation %d produced %v, first produced %v", i, got, first)
				}
			}
		}
	})

	t.Run("the rejection list is deterministic too", func(t *testing.T) {
		t.Parallel()
		mk := func(id shared.GatewayID) routing.Candidate {
			c := eligible(id)
			c.Certified = false
			return c
		}
		var first []shared.GatewayID
		for i, in := range [][]routing.Candidate{
			{mk("zeta"), mk("alpha"), mk("mu")},
			{mk("mu"), mk("zeta"), mk("alpha")},
		} {
			plan, err := routing.Decide(weightedPolicy(), cardRequest(), in, decidedAt)
			if err == nil {
				t.Fatal("expected NO_ELIGIBLE_GATEWAY")
			}
			got := make([]shared.GatewayID, 0, 3)
			for _, r := range plan.Rejections() {
				got = append(got, r.GatewayID)
			}
			if i == 0 {
				first = got
				continue
			}
			for j := range got {
				if got[j] != first[j] {
					t.Fatalf("rejection order %v differs from %v", got, first)
				}
			}
		}
	})

	t.Run("a near-tie inside the tolerance defers to the merchant's declared primary", func(t *testing.T) {
		t.Parallel()
		// A 0.01 difference in latency-derived score: inside the 0.02 tie tolerance.
		leader := eligible("adyen")
		leader.LatencyP99MS = 100
		declared := eligible("stripe")
		declared.LatencyP99MS = 400

		policy := weightedPolicy()
		policy.Primary = "stripe"

		plan, err := routing.Decide(policy, cardRequest(),
			[]routing.Candidate{leader, declared}, decidedAt)
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		assertOrder(t, plan, "stripe", "adyen")
		if plan.TieBreak != "merchant-declared primary" {
			t.Fatalf("tie-break = %q, want the merchant-declared primary rung", plan.TieBreak)
		}
	})

	t.Run("a difference outside the tolerance is not a tie", func(t *testing.T) {
		t.Parallel()
		leader := eligible("adyen")
		leader.SuccessRate = 0.98
		declared := eligible("stripe")
		declared.SuccessRate = 0.86

		policy := weightedPolicy()
		policy.Primary = "stripe"

		plan, err := routing.Decide(policy, cardRequest(),
			[]routing.Candidate{leader, declared}, decidedAt)
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		assertOrder(t, plan, "adyen", "stripe")
		if plan.TieBreak != "" {
			t.Fatalf("tie-break = %q, want none", plan.TieBreak)
		}
	})
}

// --- strategies ---------------------------------------------------------------------------------

func TestStrategyApplication(t *testing.T) {
	t.Parallel()

	// stripe is expensive but healthy and fast; adyen is cheap and degraded; paypal is
	// mid-priced, healthy and only slightly slower. Under WEIGHTED_SCORE paypal leads — being
	// mid-priced beats being the most expensive by more than being fast makes up for; under
	// LEAST_COST adyen does; under PRIORITY_WITH_FALLBACK the declared order does.
	stripe := eligible("stripe")
	stripe.CostMinorUnits = 300
	stripe.LatencyP99MS = 400

	adyen := eligible("adyen")
	adyen.CostMinorUnits = 100
	adyen.HealthScore = routing.HealthScoreDegraded
	adyen.LatencyP99MS = 1500

	paypal := eligible("paypal")
	paypal.CostMinorUnits = 200
	paypal.LatencyP99MS = 900

	candidates := []routing.Candidate{stripe, adyen, paypal}

	tests := []struct {
		name     string
		policy   routing.Policy
		want     []shared.GatewayID
		wantSize int
	}{
		{
			name: "WEIGHTED_SCORE orders by the weighted score",
			policy: routing.Policy{
				Strategy: routing.StrategyWeightedScore, Primary: "adyen",
				Weights: routing.DefaultWeights(),
			},
			want: []shared.GatewayID{"paypal", "stripe", "adyen"},
		},
		{
			name: "LEAST_COST orders by effective cost for this amount",
			policy: routing.Policy{
				Strategy: routing.StrategyLeastCost, Primary: "stripe",
				Weights: routing.DefaultWeights(),
			},
			want: []shared.GatewayID{"adyen", "paypal", "stripe"},
		},
		{
			name: "PRIORITY_WITH_FALLBACK walks the declared chain regardless of score",
			policy: routing.Policy{
				Strategy: routing.StrategyPriorityWithFallback,
				Primary:  "paypal", Fallbacks: []shared.GatewayID{"adyen", "stripe"},
				Weights: routing.DefaultWeights(),
			},
			want: []shared.GatewayID{"paypal", "adyen", "stripe"},
		},
		{
			name: "PINNED produces a one-entry plan and rejects the rest",
			policy: routing.Policy{
				Strategy: routing.StrategyPinned, Primary: "paypal",
				Weights: routing.DefaultWeights(),
			},
			want: []shared.GatewayID{"paypal"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plan, err := routing.Decide(tt.policy, cardRequest(), candidates, decidedAt)
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			assertOrder(t, plan, tt.want...)
			// Scores are recorded under every strategy, including the ones that ignore them.
			for _, s := range plan.Selections() {
				if s.Score <= 0 {
					t.Errorf("%s has no recorded score under %s", s.GatewayID, tt.policy.Strategy)
				}
				if s.Reason == "" {
					t.Errorf("%s has no recorded reason", s.GatewayID)
				}
			}
		})
	}
}

// A gateway outside the declared chain is still a better answer than a 503, but it must never
// outrank a gateway the merchant actually asked for.
func TestPriorityWithFallbackRanksUndeclaredGatewaysLast(t *testing.T) {
	t.Parallel()

	declared := eligible("stripe")
	declared.SuccessRate = 0.86
	declared.HealthScore = routing.HealthScoreDegraded
	undeclared := eligible("braintree")
	undeclared.SuccessRate = 0.98

	policy := routing.Policy{
		Strategy: routing.StrategyPriorityWithFallback,
		Primary:  "stripe", Fallbacks: []shared.GatewayID{"adyen"},
		Weights: routing.DefaultWeights(),
	}
	plan, err := routing.Decide(policy, cardRequest(),
		[]routing.Candidate{undeclared, declared}, decidedAt)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	assertOrder(t, plan, "stripe", "braintree")
}

func TestConditionalRuleOverridesTheDefaultChain(t *testing.T) {
	t.Parallel()

	policy := routing.Policy{
		Strategy: routing.StrategyPriorityWithFallback,
		Primary:  "stripe", Fallbacks: []shared.GatewayID{"paypal"},
		Weights: routing.DefaultWeights(),
		Rules: []routing.Rule{{
			ID:        "high-value-cards-to-adyen",
			Condition: routing.Condition{PaymentMethod: shared.MethodCard, AmountAbove: usdPtr(5000)},
			Action:    routing.Action{Primary: "adyen", Fallbacks: []shared.GatewayID{"stripe"}},
		}},
	}
	plan, err := routing.Decide(policy, cardRequest(),
		[]routing.Candidate{eligible("stripe"), eligible("adyen"), eligible("paypal")}, decidedAt)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	assertOrder(t, plan, "adyen", "stripe", "paypal")
	if plan.MatchedRuleID != "high-value-cards-to-adyen" {
		t.Fatalf("matched rule = %q", plan.MatchedRuleID)
	}
}

// --- empty results ------------------------------------------------------------------------------

// "No gateway is available" is an answer a merchant can do nothing with. The 503 must carry the
// reason each candidate was dropped, and the plan must still be returned so it can be persisted.
func TestNoEligibleGatewayCarriesEveryRejectionReason(t *testing.T) {
	// Verifies: FR-67.
	t.Parallel()

	uncertified := eligible("stripe")
	uncertified.Certified = false
	broken := eligible("adyen")
	broken.CircuitOpen = true

	plan, err := routing.Decide(weightedPolicy(), cardRequest(),
		[]routing.Candidate{uncertified, broken}, decidedAt)
	if err == nil {
		t.Fatal("expected NO_ELIGIBLE_GATEWAY")
	}
	if plan == nil {
		t.Fatal("the plan must be returned alongside the error so it can be persisted")
	}
	if !plan.IsEmpty() {
		t.Fatal("expected an empty plan")
	}
	if code := apierror.CodeOf(err); code != apierror.CodeNoEligibleGateway {
		t.Fatalf("code = %s, want NO_ELIGIBLE_GATEWAY", code)
	}

	var e *apierror.Error
	if ok := asAPIError(err, &e); !ok {
		t.Fatal("expected an *apierror.Error")
	}
	if len(e.Details) != 2 {
		t.Fatalf("expected one detail per rejection, got %d", len(e.Details))
	}
	seen := map[string]bool{}
	for _, d := range e.Details {
		seen[d.Code] = true
		if d.Message == "" {
			t.Error("a rejection detail must explain itself")
		}
	}
	for _, want := range []string{string(routing.ReasonNotCertified), string(routing.ReasonCircuitOpen)} {
		if !seen[want] {
			t.Errorf("expected reason %s in the error details, got %v", want, seen)
		}
	}
	// A permanent rejection (NOT_CERTIFIED) is on the plan, so promising a retry in 30s would
	// convert one support ticket into a retry storm and then a support ticket.
	if e.RetryAfterSeconds != 0 {
		t.Errorf("Retry-After = %d; it is only honest when every rejection is transient", e.RetryAfterSeconds)
	}
}

func TestRetryAfterIsOfferedWhenEveryRejectionIsTransient(t *testing.T) {
	t.Parallel()

	a, b := eligible("stripe"), eligible("adyen")
	a.CircuitOpen = true
	b.Healthy = false

	_, err := routing.Decide(weightedPolicy(), cardRequest(), []routing.Candidate{a, b}, decidedAt)
	if err == nil {
		t.Fatal("expected NO_ELIGIBLE_GATEWAY")
	}
	var e *apierror.Error
	if ok := asAPIError(err, &e); !ok {
		t.Fatal("expected an *apierror.Error")
	}
	if e.RetryAfterSeconds == 0 {
		t.Fatal("every rejection is transient, so a Retry-After hint is warranted")
	}
}

func TestNoCandidatesAtAllStillProducesAPlan(t *testing.T) {
	t.Parallel()

	plan, err := routing.Decide(weightedPolicy(), cardRequest(), nil, decidedAt)
	if err == nil {
		t.Fatal("expected NO_ELIGIBLE_GATEWAY")
	}
	if plan == nil || !plan.IsEmpty() {
		t.Fatal("an empty candidate set must still produce a persistable empty plan")
	}
	if len(plan.Rejections()) != 0 {
		t.Fatal("there was nothing to reject")
	}
}

// --- the plan as an audit artifact ----------------------------------------------------------------

func TestPlanNextIsTheFailoverPicker(t *testing.T) {
	// Verifies: BR-22, FR-64.
	t.Parallel()

	plan, err := routing.Decide(weightedPolicy(), cardRequest(),
		[]routing.Candidate{eligible("alpha"), eligible("mu"), eligible("zeta")}, decidedAt)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	tests := []struct {
		name      string
		excluding []shared.GatewayID
		want      shared.GatewayID
		wantOK    bool
	}{
		{"nothing excluded returns rank 1", nil, "alpha", true},
		{"the attempted gateway is skipped", []shared.GatewayID{"alpha"}, "mu", true},
		{"two attempts in, the third is next", []shared.GatewayID{"alpha", "mu"}, "zeta", true},
		{"everything attempted leaves nothing", []shared.GatewayID{"alpha", "mu", "zeta"}, "", false},
		{"an unknown exclusion changes nothing", []shared.GatewayID{"worldpay"}, "alpha", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := plan.Next(tt.excluding)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got.GatewayID != tt.want {
				t.Fatalf("next = %s, want %s", got.GatewayID, tt.want)
			}
		})
	}
}

// A plan is an audit record. A caller that reorders the returned slice must not be rewriting
// evidence.
func TestPlanAccessorsReturnCopies(t *testing.T) {
	t.Parallel()

	unhealthy := eligible("worldpay")
	unhealthy.Healthy = false

	plan, err := routing.Decide(weightedPolicy(), cardRequest(),
		[]routing.Candidate{eligible("alpha"), eligible("mu"), unhealthy}, decidedAt)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	sel := plan.Selections()
	sel[0], sel[1] = sel[1], sel[0]
	sel[0].GatewayID = "tampered"
	if top, _ := plan.Primary(); top.GatewayID != "alpha" {
		t.Fatalf("mutating the returned slice changed the plan: primary is now %s", top.GatewayID)
	}

	rej := plan.Rejections()
	rej[0].Reason = "TAMPERED"
	if got, _ := plan.RejectionFor("worldpay"); got.Reason != routing.ReasonUnhealthy {
		t.Fatalf("mutating the returned rejections changed the plan: %s", got.Reason)
	}
}

func TestRehydratePlanRoundTrips(t *testing.T) {
	t.Parallel()

	original, err := routing.Decide(weightedPolicy(), cardRequest(),
		[]routing.Candidate{eligible("alpha"), eligible("mu")}, decidedAt)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	restored := routing.RehydratePlan(routing.RehydratePlanParams{
		ID: original.ID, PaymentID: original.PaymentID, CreatedAt: original.CreatedAt,
		Strategy: original.Strategy, Weights: original.Weights,
		MatchedRuleID: original.MatchedRuleID, TieBreak: original.TieBreak,
		Selections: original.Selections(), Rejections: original.Rejections(),
	})
	assertOrder(t, restored, gatewayOrder(t, original)...)
	if restored.ID != original.ID || restored.TieBreak != original.TieBreak {
		t.Fatal("rehydration lost plan identity")
	}
}

func TestRejectionReasonTransience(t *testing.T) {
	t.Parallel()

	for _, r := range routing.AllRejectionReasons {
		if !r.IsValid() {
			t.Errorf("%s is in AllRejectionReasons but does not validate", r)
		}
	}
	if !routing.ReasonCircuitOpen.IsTransient() || !routing.ReasonUnhealthy.IsTransient() {
		t.Error("availability rejections are transient")
	}
	for _, r := range []routing.RejectionReason{
		routing.ReasonNotCertified, routing.ReasonCurrencyUnsupported, routing.ReasonResidencyViolation,
	} {
		if r.IsTransient() {
			t.Errorf("%s will still be true tomorrow and must not be advertised as transient", r)
		}
	}
	if routing.RejectionReason("MADE_UP").IsValid() {
		t.Error("an unregistered reason must not validate")
	}
}

// asAPIError is a local errors.As wrapper kept out of the assertion sites so the tests read as
// assertions rather than as type gymnastics.
func asAPIError(err error, target **apierror.Error) bool {
	var e *apierror.Error
	ok := errors.As(err, &e)
	if ok {
		*target = e
	}
	return ok
}
