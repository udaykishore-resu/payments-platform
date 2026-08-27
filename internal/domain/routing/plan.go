package routing

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// RequestContext is everything a routing decision is allowed to depend on.
//
// It is a closed, flat, serializable value on purpose: it is persisted alongside the plan, and
// `platformctl routing explain rpl_…` re-runs Decide against exactly this struct to assert that
// the stored score is reproducible (docs/data-plane.md §4.7). If the engine could reach any
// input that is not in here — a package-level variable, a clock, a cache — that replay would be
// a different computation wearing the same name.
type RequestContext struct {
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	// PaymentID is carried so the plan can be attributed and so that a future
	// hash-based tie-break spread has a stable per-payment input.
	PaymentID shared.PaymentID

	// Amount carries both the value and the currency; the currency is not duplicated as a
	// separate field because two fields that must agree are two fields that eventually will not.
	Amount        money.Money
	PaymentMethod shared.PaymentMethod

	// PayerCountry is the customer's country; MerchantCountry is the merchant's country of
	// establishment. They are distinct inputs because gateway licensing is a function of the
	// corridor, not of either end alone, and because a cross-border corridor prices differently.
	PayerCountry    shared.Country
	MerchantCountry shared.Country

	// RiskBand is the risk engine's outcome coarsened for routing. See RiskBand.
	RiskBand RiskBand

	// ThreeDSRequired is true when the risk engine or the corridor demands strong customer
	// authentication. A gateway that cannot perform 3DS for this corridor is not a lower-scoring
	// candidate for such a payment — it is an ineligible one, and is dropped by a hard filter.
	ThreeDSRequired bool

	// IsRetry marks a failover or a resubmission rather than a first attempt.
	IsRetry bool
	// AttemptedGateways lists the gateways this payment has already been tried on. Anti-affinity
	// (docs/data-plane.md §4.5) removes them: sending a soft decline straight back to the
	// gateway that just produced it is a wasted interchange fee and, repeated, looks like abuse.
	AttemptedGateways []shared.GatewayID

	// Operation is the gateway operation being routed. Only authorize is genuinely routed;
	// capture, refund and void are forced onto the gateway holding the authorization and never
	// reach this engine. It is carried anyway because it is a circuit-breaker and capability key.
	Operation shared.Operation
}

// HasAttempted reports whether this payment already has an attempt on the given gateway.
func (r RequestContext) HasAttempted(g shared.GatewayID) bool {
	for _, a := range r.AttemptedGateways {
		if a == g {
			return true
		}
	}
	return false
}

// Currency returns the payment currency.
func (r RequestContext) Currency() money.Currency { return r.Amount.Currency() }

// RejectionReason is the closed taxonomy of "why this gateway was not used".
//
// A closed enum rather than free text, for the same reason decline reasons are normalized: this
// value is a metric label (`pp_routing_decisions_total{gateway,reason}`), a support answer, and
// the input to "which gateway should we certify next". Free text cannot be aggregated, and a
// reason nobody can count is a reason nobody acts on.
type RejectionReason string

const (
	// ReasonCapabilityMismatch: the gateway cannot perform this operation for this corridor —
	// no partial capture, no MIT, no exemption support.
	ReasonCapabilityMismatch RejectionReason = "CAPABILITY_MISMATCH"
	// ReasonCurrencyUnsupported: the gateway does not settle this currency for this connection.
	ReasonCurrencyUnsupported RejectionReason = "CURRENCY_UNSUPPORTED"
	// ReasonMethodUnsupported: the gateway does not offer this tender type here.
	ReasonMethodUnsupported RejectionReason = "METHOD_UNSUPPORTED"
	// ReasonCountryUnsupported: the gateway is not licensed for this payer country.
	ReasonCountryUnsupported RejectionReason = "COUNTRY_UNSUPPORTED"
	// ReasonAmountOutOfBounds: the amount is outside the gateway's floor or ceiling for this
	// method and currency. A common and unobvious one: many gateways have a minimum that a
	// micro-payment merchant trips constantly.
	ReasonAmountOutOfBounds RejectionReason = "AMOUNT_OUT_OF_BOUNDS"
	// ReasonCircuitOpen: the circuit breaker for (gateway, operation) is open.
	ReasonCircuitOpen RejectionReason = "CIRCUIT_OPEN"
	// ReasonUnhealthy: health is UNHEALTHY without the breaker being open — a probing or
	// quarantined connection.
	ReasonUnhealthy RejectionReason = "UNHEALTHY"
	// ReasonNotCertified: the connection has not passed certification for this (method,
	// currency). Certification is a gate, not a preference.
	ReasonNotCertified RejectionReason = "NOT_CERTIFIED"
	// ReasonResidencyViolation: the gateway processes or stores outside the tenant's data
	// residency policy.
	ReasonResidencyViolation RejectionReason = "RESIDENCY_VIOLATION"
	// ReasonTenantNotEntitled: the tenant's allowlist does not include this gateway.
	ReasonTenantNotEntitled RejectionReason = "TENANT_NOT_ENTITLED"
	// ReasonMerchantNotConfigured: the merchant has no configured connection to this gateway.
	ReasonMerchantNotConfigured RejectionReason = "MERCHANT_NOT_CONFIGURED"
	// ReasonAlreadyAttempted: this payment already has an attempt on this gateway (§4.5
	// anti-affinity).
	ReasonAlreadyAttempted RejectionReason = "ALREADY_ATTEMPTED"
	// ReasonThreeDSUnsupported: the payment needs strong customer authentication and this
	// gateway cannot provide it for this corridor.
	ReasonThreeDSUnsupported RejectionReason = "THREE_DS_UNSUPPORTED"
	// ReasonPinnedElsewhere: the merchant's strategy is PINNED to a different gateway.
	ReasonPinnedElsewhere RejectionReason = "PINNED_ELSEWHERE"
)

// AllRejectionReasons is the complete reason universe, used to validate persisted plans, to
// generate the OpenAPI enum, and to assert in tests that every reason is actually reachable —
// a reason no code path can produce is documentation that lies.
var AllRejectionReasons = []RejectionReason{
	ReasonAlreadyAttempted, ReasonAmountOutOfBounds, ReasonCapabilityMismatch,
	ReasonCircuitOpen, ReasonCountryUnsupported, ReasonCurrencyUnsupported,
	ReasonMerchantNotConfigured, ReasonMethodUnsupported, ReasonNotCertified,
	ReasonPinnedElsewhere, ReasonResidencyViolation, ReasonTenantNotEntitled,
	ReasonThreeDSUnsupported, ReasonUnhealthy,
}

// IsValid reports whether r is a known rejection reason.
func (r RejectionReason) IsValid() bool {
	for _, x := range AllRejectionReasons {
		if x == r {
			return true
		}
	}
	return false
}

// String satisfies fmt.Stringer.
func (r RejectionReason) String() string { return string(r) }

// IsTransient reports whether the rejection could resolve on its own within minutes. It drives
// the `Retry-After` hint on a 503: telling a merchant to retry in 30 seconds is correct when
// every candidate was dropped for CIRCUIT_OPEN and actively misleading when they were all
// dropped for NOT_CERTIFIED, which will still be true tomorrow.
func (r RejectionReason) IsTransient() bool {
	switch r {
	case ReasonCircuitOpen, ReasonUnhealthy:
		return true
	default:
		return false
	}
}

// ScoreBreakdown records how a score was reached, dimension by dimension.
//
// Storing only the final score would make the plan unverifiable: 0.6511 is a number nobody can
// check. Storing the four normalized factors makes the arithmetic reproducible offline, which
// is what turns "we think routing is behaving" into "routing provably computed this from these
// inputs" — and it is how a scoring regression is caught by replay rather than by a merchant.
type ScoreBreakdown struct {
	// Health is the normalized health factor, H in §4.3.
	Health float64
	// SuccessRate is the banded, smoothed authorization rate, S in §4.3.
	SuccessRate float64
	// Cost is the min-max-inverted effective cost for this amount, C in §4.3.
	Cost float64
	// Latency is the ceiling-inverted tail latency, L in §4.3.
	Latency float64
	// Weighted is the weighted sum: the value the ordering actually uses.
	Weighted float64
}

// Selection is one gateway on the plan, at its rank.
type Selection struct {
	GatewayID shared.GatewayID
	// Rank is 1-based. Rank 1 is dispatched; ranks 2..n are the failover order.
	Rank int
	// Score is the weighted score. It is recorded under every strategy, including the ones that
	// ignore it, so that a merchant on PINNED can still be shown what the score said.
	Score          float64
	ScoreBreakdown ScoreBreakdown
	// Reason is a short human-readable account of why this gateway landed at this rank, e.g.
	// "merchant-declared primary" or "highest weighted score".
	Reason string
}

// Rejection is one gateway that was considered and dropped.
type Rejection struct {
	GatewayID shared.GatewayID
	Reason    RejectionReason
	// Detail is the specific fact behind the reason — "no CERTIFIED connection for CARD/USD",
	// "health=UNHEALTHY since 14:01:52Z". The reason is for counting; the detail is for the
	// human reading the plan six months later.
	Detail string
}

// Plan is the persisted, auditable routing decision.
//
// Why the rejections are persisted, and why that is not optional:
//
// Six months after the fact, "why did this payment go to Adyen" is a question with a real
// answer only if the alternatives that were rejected, and the reason each was rejected, were
// written down at decision time. The inputs that produced the decision — health windows,
// circuit states, certification status, capability descriptors — are all mutable and all
// short-lived. By the time anyone asks, the health window has rolled over a hundred thousand
// times and the descriptor has been revised twice. Reconstructing the decision from current
// state does not reproduce it; it produces a *different* decision that happens to be about the
// same payment.
//
// So the plan records the counterfactual, not just the outcome. "Stripe was not used because
// its circuit was open at 14:01:52Z" is a fact. "Stripe is healthy now, so it should have been
// used" is an inference, and it is wrong. That difference is what makes a routing dispute
// decidable, and it is why an empty plan — every candidate rejected — is still persisted before
// the 503 goes out.
type Plan struct {
	ID        shared.PlanID
	PaymentID shared.PaymentID
	CreatedAt time.Time

	// Strategy and Weights are copied onto the plan rather than referenced, because the
	// merchant's configuration is versioned and mutable and the plan must survive its revision.
	Strategy Strategy
	Weights  Weights

	// MatchedRuleID names the conditional rule that produced the primary/fallback chain, or "".
	MatchedRuleID string

	// TieBreak records which tie-break rule decided the top two when their scores fell inside
	// the tie tolerance, or "" when no tie-break was needed.
	TieBreak string

	selections []Selection
	rejections []Rejection
}

// Selections returns the ordered candidate list. The slice is a copy: a plan is an audit record
// and a caller that reorders it in place would be rewriting evidence.
func (p *Plan) Selections() []Selection { return append([]Selection(nil), p.selections...) }

// Rejections returns every considered-and-dropped candidate with its reason, as a copy.
func (p *Plan) Rejections() []Rejection { return append([]Rejection(nil), p.rejections...) }

// IsEmpty reports whether no gateway survived. An empty plan is a valid, persisted plan; it is
// the record behind a 503 NO_ELIGIBLE_GATEWAY.
func (p *Plan) IsEmpty() bool { return len(p.selections) == 0 }

// Primary returns the rank-1 selection, which is the gateway to dispatch to.
func (p *Plan) Primary() (Selection, bool) {
	if len(p.selections) == 0 {
		return Selection{}, false
	}
	return p.selections[0], true
}

// Next is the failover picker: it returns the highest-ranked selection that is not in the
// excluded set.
//
// The exclusion set is a parameter rather than state on the plan because the plan is immutable
// once persisted, and because the caller — the orchestrator — is the only component that knows
// which gateways this payment has actually been attempted on, including attempts made by an
// earlier process that crashed. A plan that tracked its own cursor would give a wrong answer
// after a restart, which is precisely the moment a double charge becomes possible.
func (p *Plan) Next(excluding []shared.GatewayID) (Selection, bool) {
	skip := make(map[shared.GatewayID]struct{}, len(excluding))
	for _, g := range excluding {
		skip[g] = struct{}{}
	}
	for _, s := range p.selections {
		if _, ok := skip[s.GatewayID]; ok {
			continue
		}
		return s, true
	}
	return Selection{}, false
}

// SelectionFor returns the selection for a gateway, if it is on the plan.
func (p *Plan) SelectionFor(g shared.GatewayID) (Selection, bool) {
	for _, s := range p.selections {
		if s.GatewayID == g {
			return s, true
		}
	}
	return Selection{}, false
}

// RejectionFor returns the rejection recorded for a gateway, if there is one. This is the
// function behind "why wasn't X used".
func (p *Plan) RejectionFor(g shared.GatewayID) (Rejection, bool) {
	for _, r := range p.rejections {
		if r.GatewayID == g {
			return r, true
		}
	}
	return Rejection{}, false
}

// RejectionReasons returns the distinct reasons on this plan, in the canonical order of
// AllRejectionReasons so the set is stable across runs. Used to build the 503's error details
// and to decide whether a Retry-After is honest.
func (p *Plan) RejectionReasons() []RejectionReason {
	seen := make(map[RejectionReason]struct{}, len(p.rejections))
	for _, r := range p.rejections {
		seen[r.Reason] = struct{}{}
	}
	out := make([]RejectionReason, 0, len(seen))
	for _, r := range AllRejectionReasons {
		if _, ok := seen[r]; ok {
			out = append(out, r)
		}
	}
	return out
}

// RehydratePlanParams carries a persisted plan back into the domain.
//
// It exists for the same reason payment.RehydrateParams does: the selection and rejection
// slices are unexported so that nothing outside this package can assemble a plan that the
// engine would not have produced. A repository reading a `routing_plans` row needs a single,
// explicit, reviewed doorway rather than the ability to construct any Plan it likes — otherwise
// "the plan is what the engine decided" stops being true the first time a backfill script runs.
type RehydratePlanParams struct {
	ID            shared.PlanID
	PaymentID     shared.PaymentID
	CreatedAt     time.Time
	Strategy      Strategy
	Weights       Weights
	MatchedRuleID string
	TieBreak      string
	Selections    []Selection
	Rejections    []Rejection
}

// RehydratePlan reconstructs a Plan from persisted state.
func RehydratePlan(p RehydratePlanParams) *Plan {
	return &Plan{
		ID:            p.ID,
		PaymentID:     p.PaymentID,
		CreatedAt:     p.CreatedAt.UTC(),
		Strategy:      p.Strategy,
		Weights:       p.Weights,
		MatchedRuleID: p.MatchedRuleID,
		TieBreak:      p.TieBreak,
		selections:    append([]Selection(nil), p.Selections...),
		rejections:    append([]Rejection(nil), p.Rejections...),
	}
}

// AllRejectionsTransient reports whether every recorded rejection could plausibly clear within
// minutes, which is the condition under which a Retry-After on the 503 is honest rather than
// hopeful.
func (p *Plan) AllRejectionsTransient() bool {
	if len(p.rejections) == 0 {
		return false
	}
	for _, r := range p.rejections {
		if !r.Reason.IsTransient() {
			return false
		}
	}
	return true
}
