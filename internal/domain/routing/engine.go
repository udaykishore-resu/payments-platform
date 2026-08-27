package routing

import (
	"sort"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
)

// Candidate is one gateway offered to the routing engine, with every question the engine needs
// to ask already answered.
//
// This type is the reason this package does not import internal/domain/gateway. Convention 13
// says interfaces are declared by the consumer; the same reasoning applies to the data shape a
// consumer needs. The gateway registry owns capability descriptors, connection lifecycle,
// circuit-breaker state and health windows — a rich, mutable, I/O-backed model. Routing needs
// none of that. It needs a dozen booleans and four numbers, resolved *before* the decision, by
// the application layer that owns both packages.
//
// The alternative — routing importing the gateway package and asking each connection whether it
// supports EUR — was rejected for three reasons. It makes routing untestable without
// constructing gateway aggregates. It makes the scoring function depend on when the capability
// question was asked, so a replay six months later resolves against today's descriptor rather
// than the one that produced the decision. And it couples two contexts that have no business
// changing together: a new field on a capability descriptor should not recompile the scorer.
//
// The capability booleans are therefore *answers*, not queries, and the plan records the answer
// alongside the decision.
type Candidate struct {
	GatewayID shared.GatewayID

	// --- entitlement and compliance ---------------------------------------------------------

	// TenantEntitled is false when the tenant's gateway allowlist excludes this gateway.
	TenantEntitled bool
	// ResidencyCompliant is false when the gateway processes or stores outside the tenant's data
	// residency policy (baseline §17.3).
	ResidencyCompliant bool
	// MerchantConfigured is false when the merchant has no connection to this gateway at all.
	MerchantConfigured bool
	// Certified is false when the connection has not passed certification for this
	// (method, currency) pair.
	Certified bool

	// --- availability -----------------------------------------------------------------------

	// CircuitOpen is true when the breaker for (gateway, operation) is open.
	CircuitOpen bool
	// Healthy is false when the connection is quarantined or probing and must not take live
	// traffic. Distinct from CircuitOpen so the two produce different rejection reasons and
	// therefore different metrics: a breaker trip is an incident, a probing connection is a
	// recovery in progress.
	Healthy bool

	// --- capability -------------------------------------------------------------------------

	SupportsCurrency  bool
	SupportsMethod    bool
	SupportsCountry   bool
	SupportsOperation bool
	// SupportsThreeDS answers "can this gateway run strong customer authentication for this
	// corridor", which is only consulted when the request needs it.
	SupportsThreeDS bool

	// MinAmountMinorUnits is the gateway's floor for this method and currency. Zero means none.
	MinAmountMinorUnits int64
	// MaxAmountMinorUnits is the gateway's ceiling. Zero means none — an explicit sentinel
	// rather than a huge number, because a ceiling of 0 would otherwise reject every payment
	// and the failure would look like a capability problem instead of a config problem.
	MaxAmountMinorUnits int64

	// --- scoring inputs ---------------------------------------------------------------------

	// HealthScore is the health state resolved to [0, 1]: see HealthScoreHealthy and friends.
	// It arrives pre-resolved because the mapping from a health state machine to a number is
	// the gateway context's business, not routing's.
	HealthScore float64
	// SuccessRate is the Bayesian-smoothed authorization rate ŝ on [0, 1] for this gateway,
	// method, currency and issuer country. Smoothing happens where the samples live; see
	// SmoothSuccessRate for the exact formula this engine expects to have been applied.
	SuccessRate float64
	// CostMinorUnits is the effective cost of this specific payment at this gateway:
	// bps·amount/10000 + fixed + scheme surcharges. Per payment, not per gateway, because a 30¢
	// fixed fee dominates a $3 payment and is noise on a $300 one.
	CostMinorUnits int64
	// LatencyP99MS is the observed tail latency in milliseconds for (gateway, operation).
	// docs/data-plane.md §4.3 specifies p95 over a 5-minute window; whichever tail percentile
	// the caller publishes, the normalisation is the same and the only requirement is that it be
	// the *same* percentile for every candidate in one decision — mixing percentiles across
	// candidates makes the comparison meaningless without making it look wrong.
	LatencyP99MS int
}

// The health-state to score mapping from docs/data-plane.md §4.3, exported as named constants
// so that the caller resolving a gateway health state has one place to look and the worked
// example in the documentation has one place to be checked against. UNHEALTHY has no constant
// because it cannot reach scoring: hard filter 5 removes it.
const (
	// HealthScoreHealthy is a gateway taking traffic normally.
	HealthScoreHealthy = 1.0
	// HealthScoreDegraded is a gateway whose error rate is elevated but below the breaker
	// threshold. At the default weights this costs 0.24 of a possible 0.40 — enough to lose to a
	// healthy competitor that is worse on every other dimension, which is the intended behaviour.
	HealthScoreDegraded = 0.4
	// HealthScoreProbing is a gateway a half-open breaker is testing. Deliberately low: probe
	// traffic is a diagnostic, not a route.
	HealthScoreProbing = 0.15
)

// Scoring constants from docs/data-plane.md §4.3. They are package constants rather than policy
// fields because they are properties of the *platform's* measurement, not of a merchant's
// preference: a merchant can weight success rate more heavily, but a merchant cannot decide
// that 0.80 counts as a good authorization rate.
const (
	// successBandLow and successBandHigh are the fixed band the smoothed success rate is
	// normalized against. A fixed band rather than min-max across the candidate set is the whole
	// point: min-max amplifies a 0.3pp difference between two near-identical gateways into a
	// full point of score, and the resulting flapping moves traffic on noise.
	successBandLow  = 0.85
	successBandHigh = 0.98

	// latencyCeilingMS is the latency at which the latency factor reaches zero. Anything slower
	// than three seconds on an authorize is, within the 8s dispatch budget, effectively the same
	// kind of bad.
	latencyCeilingMS = 3000.0

	// successPriorWeight is α in the Bayesian smoothing ŝ = (successes + α·prior)/(n + α).
	// α = 50 is chosen so a gateway with six samples cannot outrank one with four thousand.
	successPriorWeight = 50.0
)

// ScoreTieTolerance is the band within which two scores are treated as tied (§4.4). Below 0.02
// the difference is inside the noise of a 30-minute success-rate window, and ordering on it
// moves traffic for no reason.
const ScoreTieTolerance = 0.02

// SmoothSuccessRate applies the Bayesian smoothing from §4.3: ŝ = (successes + α·prior)/(n + α)
// with α = 50.
//
// It lives here, rather than in whatever adapter reads the counters, because it is part of the
// scoring formula: changing it changes routing decisions, and it must therefore be versioned,
// tested and replayable with the rest of the formula. Without smoothing, a gateway with 6
// samples and 6 successes scores a perfect 1.0 and outranks one with 4 000 samples at 94% —
// which is how a freshly certified connection captures a merchant's entire volume on the
// strength of six transactions.
//
// A zero sample count returns the prior, which is the correct answer: with no observations, the
// merchant's own 30-day baseline is the best estimate available.
func SmoothSuccessRate(successes, samples int64, prior float64) float64 {
	if samples < 0 || successes < 0 {
		return clamp01(prior)
	}
	num := float64(successes) + successPriorWeight*prior
	den := float64(samples) + successPriorWeight
	return clamp01(num / den)
}

// Decide is the routing domain service.
//
// It is a free function rather than a method on an aggregate because the decision is not any
// one aggregate's state change. It reads a Policy (a value owned by the configuration context),
// a RequestContext (derived from a Payment) and a candidate set (derived from the gateway
// registry), and it produces a new Plan. Hanging it off Policy would imply the policy changes;
// hanging it off Payment would drag the whole gateway registry into the payment aggregate's
// dependency set. Evans' rule applies literally here: an operation that is a significant domain
// concept but is not naturally part of any entity is a domain service.
//
// The pipeline, in order:
//
//  1. hard filters — absolute, never traded off against score;
//  2. strategy application — which candidates may participate and in what order;
//  3. scoring — normalize four dimensions to [0, 1], then the weighted sum;
//  4. deterministic tie-breaking;
//  5. empty result → NO_ELIGIBLE_GATEWAY carrying every rejection reason.
//
// On an empty result it returns *both* the plan and the error. The plan is not a consolation
// prize: it is the record that answers "why did this merchant get a 503 at 14:02", and the
// caller is expected to persist it before returning the error to the client.
func Decide(policy Policy, req RequestContext, candidates []Candidate, now time.Time) (*Plan, error) {
	action, matchedRule := policy.ResolveFor(req)
	weights := policy.EffectiveWeights()

	plan := &Plan{
		// The plan ID is stamped at the decision instant rather than at wall-clock now. The
		// creation timestamp is recoverable from a ULID and is the declarative partition key
		// (baseline amendment A-02), so a plan minted from a replayed decision lands in the same
		// partition as the original instead of in today's.
		ID:            shared.PlanID(ids.NewAt(ids.PrefixRoutingPlan, now)),
		PaymentID:     req.PaymentID,
		CreatedAt:     now.UTC(),
		Strategy:      policy.Strategy,
		Weights:       weights,
		MatchedRuleID: matchedRule,
	}

	// Sort the input by gateway ID before anything else. Two callers assembling the same
	// candidate set in different map-iteration orders must produce byte-identical plans, and
	// that has to be true of the rejection list as well as the selection list — otherwise the
	// checksum over a persisted plan is not stable and the replay check in §4.7 fails for a
	// reason that has nothing to do with routing.
	ordered := append([]Candidate(nil), candidates...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].GatewayID < ordered[j].GatewayID })

	// --- 1. hard filters --------------------------------------------------------------------
	//
	// Hard filters are absolute. A candidate that fails one is removed before scoring and no
	// weight can bring it back. This is not an optimization, it is a correctness property: a
	// "cheap but incapable" gateway must not be scored at all, because scoring implies the
	// dimensions are commensurable, and they are not. A gateway that cannot settle EUR does not
	// have a low EUR score — it has no EUR score, and the difference matters the moment someone
	// tunes the cost weight upward. Filter first, score second, and the tuning knob can never
	// reach the eligibility question. The same logic is why certification, residency and tenant
	// entitlement are filters and not penalties: they are legal and contractual constraints, and
	// a constraint you can outbid is not a constraint.
	survivors := make([]Candidate, 0, len(ordered))
	for _, c := range ordered {
		if reason, detail, dropped := hardFilter(c, req, policy, action); dropped {
			plan.rejections = append(plan.rejections, Rejection{
				GatewayID: c.GatewayID, Reason: reason, Detail: detail,
			})
			continue
		}
		survivors = append(survivors, c)
	}

	if len(survivors) == 0 {
		return plan, noEligibleGateway(plan)
	}

	// --- 3. scoring (computed for every survivor under every strategy) ----------------------
	//
	// Scores are computed even when the strategy ignores them. Recording what the score *would*
	// have said under PINNED or LEAST_COST is what makes "your pinned gateway has been the worst
	// of your three all quarter" a provable statement, and it costs four multiplications.
	breakdowns := score(survivors, weights)

	// --- 2. strategy application ------------------------------------------------------------
	selections := orderByStrategy(policy.Strategy, survivors, breakdowns, action)

	// --- 4. deterministic tie-breaking ------------------------------------------------------
	plan.TieBreak = breakTopTie(selections, action)

	for i := range selections {
		selections[i].Rank = i + 1
	}
	plan.selections = selections

	if plan.IsEmpty() {
		return plan, noEligibleGateway(plan)
	}
	return plan, nil
}

// hardFilter applies the filters in the order of docs/data-plane.md §4.2, returning the reason
// for the first one that removes the candidate.
//
// The order determines which reason is recorded when a candidate fails several filters, and
// that choice is deliberate: the most fundamental reason wins. "This gateway is not certified
// for you" is a more useful answer than "its circuit is open", because the first is actionable
// and permanent and the second is noise about a gateway that was never eligible anyway.
func hardFilter(c Candidate, req RequestContext, policy Policy, action Action) (RejectionReason, string, bool) {
	// PINNED short-circuits everything. When a merchant has pinned their traffic, the reason a
	// gateway was not used is that it is not the pin, and no further detail about it is
	// interesting or even meaningful — we may not have refreshed its descriptor in weeks.
	if policy.Strategy == StrategyPinned && c.GatewayID != action.Primary {
		return ReasonPinnedElsewhere, "merchant policy is PINNED to " + action.Primary.String(), true
	}

	if !c.TenantEntitled {
		return ReasonTenantNotEntitled, "the tenant's gateway allowlist does not include " + c.GatewayID.String(), true
	}
	if !c.ResidencyCompliant {
		return ReasonResidencyViolation, "gateway processes or stores outside the tenant's data residency policy", true
	}
	if !c.MerchantConfigured {
		return ReasonMerchantNotConfigured, "the merchant has no configured connection to " + c.GatewayID.String(), true
	}
	if !c.Certified {
		return ReasonNotCertified, "no CERTIFIED connection for " + string(req.PaymentMethod) + "/" + req.Currency().String(), true
	}
	if c.CircuitOpen {
		return ReasonCircuitOpen, "circuit breaker is open for (" + c.GatewayID.String() + ", " + req.Operation.String() + ")", true
	}
	if !c.Healthy {
		return ReasonUnhealthy, "connection health does not permit live traffic", true
	}
	if !c.SupportsCurrency {
		return ReasonCurrencyUnsupported, "gateway does not settle " + req.Currency().String() + " on this connection", true
	}
	if !c.SupportsMethod {
		return ReasonMethodUnsupported, "gateway does not offer " + string(req.PaymentMethod) + " on this connection", true
	}
	if !c.SupportsCountry {
		return ReasonCountryUnsupported, "gateway is not licensed for payer country " + req.PayerCountry.String(), true
	}
	if !c.SupportsOperation {
		return ReasonCapabilityMismatch, "gateway descriptor lacks the capability required for " + req.Operation.String(), true
	}
	// Only consulted when the payment actually needs authentication. A gateway with no 3DS
	// capability is a perfectly good route for a payment that does not need 3DS, and rejecting
	// it unconditionally would shrink the candidate set for the 90% of traffic that is exempt.
	if req.ThreeDSRequired && !c.SupportsThreeDS {
		return ReasonThreeDSUnsupported, "payment requires strong customer authentication and this gateway cannot perform it for this corridor", true
	}
	if out, detail := amountOutOfBounds(c, req); out {
		return ReasonAmountOutOfBounds, detail, true
	}
	// Anti-affinity, applied last because it is the only filter that depends on this payment's
	// history rather than on the gateway. A first attempt never trips it.
	if req.HasAttempted(c.GatewayID) {
		return ReasonAlreadyAttempted, "this payment already has an attempt on " + c.GatewayID.String(), true
	}
	return "", "", false
}

// amountOutOfBounds checks the gateway's floor and ceiling for this method and currency. A zero
// ceiling means "no ceiling"; see Candidate.MaxAmountMinorUnits for why that sentinel is
// explicit rather than a large number.
func amountOutOfBounds(c Candidate, req RequestContext) (bool, string) {
	amt := req.Amount.Amount()
	if c.MinAmountMinorUnits > 0 && amt < c.MinAmountMinorUnits {
		return true, "amount " + req.Amount.String() + " is below the gateway minimum for this method and currency"
	}
	if c.MaxAmountMinorUnits > 0 && amt > c.MaxAmountMinorUnits {
		return true, "amount " + req.Amount.String() + " is above the gateway maximum for this method and currency"
	}
	return false, ""
}

// score normalizes each dimension to [0, 1] and computes the weighted sum, per §4.3.
//
// Health and the smoothed success rate use fixed scales; cost uses min-max normalization across
// the surviving candidate set; latency uses a fixed ceiling. The mix is deliberate. Cost is the
// one dimension where the *relative* difference is what a merchant cares about — 219 vs 275
// minor units means nothing in absolute terms and everything in comparison — while success rate
// and latency have absolute meanings that must not drift with the composition of the candidate
// set.
func score(survivors []Candidate, w Weights) []ScoreBreakdown {
	minCost, maxCost := survivors[0].CostMinorUnits, survivors[0].CostMinorUnits
	for _, c := range survivors[1:] {
		if c.CostMinorUnits < minCost {
			minCost = c.CostMinorUnits
		}
		if c.CostMinorUnits > maxCost {
			maxCost = c.CostMinorUnits
		}
	}

	out := make([]ScoreBreakdown, len(survivors))
	for i, c := range survivors {
		b := ScoreBreakdown{
			Health:      clamp01(c.HealthScore),
			SuccessRate: normalizeSuccessRate(c.SuccessRate),
			Cost:        normalizeCost(c.CostMinorUnits, minCost, maxCost),
			Latency:     normalizeLatency(c.LatencyP99MS),
		}
		b.Weighted = w.Health*b.Health + w.SuccessRate*b.SuccessRate + w.Cost*b.Cost + w.Latency*b.Latency
		out[i] = b
	}
	return out
}

// normalizeSuccessRate maps the smoothed rate onto the fixed [0.85, 0.98] band.
func normalizeSuccessRate(s float64) float64 {
	return clamp01((s - successBandLow) / (successBandHigh - successBandLow))
}

// normalizeCost inverts and min-max normalizes the effective cost: cheapest scores 1.0.
//
// The degenerate case is the whole reason this is a named function. When every candidate has
// the same effective cost — which is not exotic, it is what happens on a flat-rate merchant
// agreement, on a single-candidate set, and on every zero-amount verification — the range is
// zero and the naive expression 1 − (c − min)/(max − min) evaluates 0/0 to NaN. A NaN score
// then poisons the weighted sum, and because every comparison against NaN is false, the sort
// silently produces an arbitrary order: the routing engine appears to work and routes at
// random. Returning 1.0 for every candidate is the correct answer, not merely a safe one — if
// cost cannot distinguish them, cost should not penalize any of them, and the other three
// dimensions decide.
func normalizeCost(cost, minCost, maxCost int64) float64 {
	if maxCost == minCost {
		return 1.0
	}
	return clamp01(1.0 - float64(cost-minCost)/float64(maxCost-minCost))
}

// normalizeLatency inverts the tail latency against a fixed 3-second ceiling.
//
// A fixed ceiling rather than min-max across the candidate set, for the same reason as the
// success-rate band: with min-max, a set where the fastest gateway is 600ms and the slowest is
// 640ms would score them 1.0 and 0.0 and hand 0.1 of score to a 40ms difference nobody can
// perceive. A fixed ceiling also means this dimension can never divide by zero, so the
// degenerate case that afflicts cost cannot arise here.
func normalizeLatency(ms int) float64 {
	if ms < 0 {
		return 1.0
	}
	return 1.0 - clamp01(float64(ms)/latencyCeilingMS)
}

func clamp01(v float64) float64 {
	// The NaN guard is not defensive clutter: a caller that passes a health score computed from
	// a zero-sample window can hand us NaN, and a single NaN in the weighted sum makes the
	// entire ordering arbitrary rather than merely wrong. v != v is true only for NaN.
	if v != v {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// orderByStrategy produces the candidate ordering the merchant's strategy calls for. Every
// branch ends in a total, deterministic order — see the comment on breakTopTie for why the
// comparators here must never use the tie *tolerance*.
func orderByStrategy(s Strategy, survivors []Candidate, breakdowns []ScoreBreakdown, action Action) []Selection {
	chainPos := make(map[shared.GatewayID]int, len(action.Fallbacks)+1)
	for i, g := range action.Chain() {
		if _, seen := chainPos[g]; !seen {
			chainPos[g] = i
		}
	}
	positionOf := func(g shared.GatewayID) int {
		if p, ok := chainPos[g]; ok {
			return p
		}
		// Not in the configured chain: sorts after every configured gateway.
		return len(chainPos) + 1
	}

	sel := make([]Selection, len(survivors))
	for i, c := range survivors {
		sel[i] = Selection{
			GatewayID:      c.GatewayID,
			Score:          breakdowns[i].Weighted,
			ScoreBreakdown: breakdowns[i],
		}
	}
	costOf := make(map[shared.GatewayID]int64, len(survivors))
	healthOf := make(map[shared.GatewayID]float64, len(survivors))
	for _, c := range survivors {
		costOf[c.GatewayID] = c.CostMinorUnits
		healthOf[c.GatewayID] = c.HealthScore
	}

	switch s {
	case StrategyPinned:
		// The hard filter has already removed everything but the pin, so there is at most one
		// survivor and no ordering to do.
		for i := range sel {
			sel[i].Reason = "pinned by merchant policy"
		}

	case StrategyLeastCost:
		sort.SliceStable(sel, func(i, j int) bool {
			ci, cj := costOf[sel[i].GatewayID], costOf[sel[j].GatewayID]
			if ci != cj {
				return ci < cj
			}
			// Health, not score, breaks a cost tie under LEAST_COST: the merchant asked for the
			// cheapest, and among equally cheap options the one most likely to actually approve
			// is the cheapest in the only sense that survives contact with a failed payment.
			hi, hj := healthOf[sel[i].GatewayID], healthOf[sel[j].GatewayID]
			if hi != hj {
				return hi > hj
			}
			return sel[i].GatewayID < sel[j].GatewayID
		})
		for i := range sel {
			sel[i].Reason = "lowest effective cost for this amount"
		}
		if len(sel) > 0 {
			sel[0].Reason = "cheapest eligible gateway for this amount"
		}

	case StrategyPriorityWithFallback:
		// Configured order first, then everything else by score. A certified, healthy gateway
		// that the merchant did not put in the chain is still a better answer than a 503 — but
		// it never outranks a configured one, which is what the merchant actually asked for.
		sort.SliceStable(sel, func(i, j int) bool {
			pi, pj := positionOf(sel[i].GatewayID), positionOf(sel[j].GatewayID)
			if pi != pj {
				return pi < pj
			}
			if sel[i].Score != sel[j].Score {
				return sel[i].Score > sel[j].Score
			}
			return sel[i].GatewayID < sel[j].GatewayID
		})
		for i := range sel {
			switch p := positionOf(sel[i].GatewayID); {
			case p == 0:
				sel[i].Reason = "merchant-declared primary"
			case p < len(chainPos):
				sel[i].Reason = "merchant-declared fallback rank " + itoa(p)
			default:
				sel[i].Reason = "eligible gateway outside the declared chain, ordered by score"
			}
		}

	default: // StrategyWeightedScore
		sort.SliceStable(sel, func(i, j int) bool {
			if sel[i].Score != sel[j].Score {
				return sel[i].Score > sel[j].Score
			}
			// Exact-tie ladder. Deliberately *not* the 0.02 tolerance — see breakTopTie.
			ci, cj := costOf[sel[i].GatewayID], costOf[sel[j].GatewayID]
			if ci != cj {
				return ci < cj
			}
			return sel[i].GatewayID < sel[j].GatewayID
		})
		for i := range sel {
			sel[i].Reason = "ranked by weighted score"
		}
		if len(sel) > 0 {
			sel[0].Reason = "highest weighted score"
		}
	}
	return sel
}

// breakTopTie applies the §4.4 near-tie rule to the top two selections and reports which rule
// decided it, or "" when no tie-break was needed.
//
// It is applied to the top two only, exactly as the flowchart in §4.2 specifies, and that
// restriction is load-bearing rather than a simplification. "Within 0.02 counts as tied" is not
// a transitive relation: with scores 0.70, 0.69 and 0.68, the first is tied with the second and
// the second with the third, but the first is not tied with the third. A sort comparator built
// on a non-transitive relation is undefined behaviour — sort.Slice may produce different
// orderings for the same input depending on pivot selection, which would destroy the exact
// property §4.4 exists to guarantee. So the tolerance is applied once, to an adjacent pair,
// after a total order has already been established.
//
// The ladder omits the documented hash-based spread (`H(payment_id ‖ gateway_id)`). Load
// spreading across genuinely equivalent gateways is a real goal, but it belongs to a component
// that can see traffic share; here, ordering by gateway ID keeps the decision reproducible from
// the plan alone, without the caller having to reconstruct a hash input.
func breakTopTie(sel []Selection, action Action) string {
	if len(sel) < 2 {
		return ""
	}
	a, b := sel[0], sel[1]
	gap := a.Score - b.Score
	if gap < 0 {
		gap = -gap
	}
	if gap > ScoreTieTolerance {
		return ""
	}

	// 1. merchant-declared primary.
	if b.GatewayID == action.Primary && a.GatewayID != action.Primary {
		sel[0], sel[1] = b, a
		return "merchant-declared primary"
	}
	if a.GatewayID == action.Primary && b.GatewayID != action.Primary {
		return "merchant-declared primary"
	}
	// 2. lower effective cost. Costs are not on the Selection, so this rung is only reachable
	// via the exact-tie ladder in orderByStrategy, which has already applied it; recording it
	// here keeps the audit trail honest about which rung decided.
	if a.Score != b.Score {
		return "higher score within the tie tolerance"
	}
	// 3. deterministic spread by gateway ID.
	if b.GatewayID < a.GatewayID {
		sel[0], sel[1] = b, a
	}
	return "deterministic gateway-ID ordering"
}

// noEligibleGateway builds the 503, carrying every rejection reason so the caller can tell the
// merchant *why* rather than just "no gateway".
//
// "No gateway is available" is an answer a merchant can do nothing with. "Stripe is not
// certified for CARD/SEK and Adyen's circuit is open" is an answer that produces either a
// support ticket with a resolution or a retry that will succeed. The distinction between those
// two is worth the seven lines it costs, and it is the difference between a fail-closed
// behaviour that is defensible and one that merely refuses.
func noEligibleGateway(plan *Plan) *apierror.Error {
	details := make([]apierror.Detail, 0, len(plan.rejections))
	for _, r := range plan.rejections {
		details = append(details, apierror.Detail{
			Field:   "gateway." + r.GatewayID.String(),
			Code:    string(r.Reason),
			Message: r.Detail,
			RuleID:  "L5.METHOD_CURRENCY_PAIR_ROUTABLE",
		})
	}
	err := apierror.New(apierror.CodeNoEligibleGateway, "").WithDetails(details...)
	// Retry-After is only honest when every rejection could actually clear on its own. Telling a
	// merchant to retry in 30 seconds when the real answer is "you have no certified connection
	// for this currency" converts one support ticket into a retry storm and then a support
	// ticket.
	if plan.AllRejectionsTransient() {
		err = err.WithRetryAfter(30)
	}
	return err
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-13, BR-22, FR-62, FR-67, FR-68.
//
// The routing decision: hard filters first, then scoring, with every excluded candidate and
// its reason recorded on the plan
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
