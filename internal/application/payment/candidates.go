package payment

import (
	"context"

	"github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/routing"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/domain/tenant"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// DefaultSuccessPrior is the smoothing prior used when a merchant has no measured baseline.
//
// 0.90 rather than 1.0: a brand-new connection with no observations should score as "an ordinary
// gateway", not as a perfect one. Starting at 1.0 would hand a merchant's entire volume to
// whichever connection was certified most recently, on the strength of no evidence at all.
const DefaultSuccessPrior = 0.90

// CandidateAssemblerDeps is everything needed to answer a routing candidate's questions.
//
// There are five sources and each one answers a different question. The reason they are five
// rather than one is that they change at wildly different rates — a capability descriptor
// monthly, a connection a few times a year, health thousands of times a second — and fusing them
// would put a health observation in the same read as a capability edit.
type CandidateAssemblerDeps struct {
	// Gateways supplies capability descriptors and cost models.
	Gateways GatewayCatalog
	// Health supplies the live circuit state, health score and tail latency.
	Health HealthProvider
	// Tenants supplies entitlement and residency, both of which are hard filters.
	Tenants TenantPolicy
	// Rates supplies the observed authorization counts the success term is smoothed from.
	Rates SuccessRates
	// Breakers is the in-process breaker registry. It is consulted in addition to health because
	// the two are not the same fact: health is the platform's shared, persisted view, and the
	// breaker is this process's own, which can be open while the shared view has not yet caught
	// up. Taking the more restrictive of the two is the only safe combination.
	Breakers CircuitBreaker
	// ProcessingRegionOf resolves a gateway's processing region for the residency filter. It is a
	// function rather than a field on the descriptor because the region is an operational fact
	// about where we run the integration, not a capability the vendor declares.
	//
	// A nil function answers "" for every gateway, which the tenant's residency policy refuses
	// for every non-GLOBAL tenant — the fail-closed direction, and the correct one: a gateway
	// whose processing region nobody recorded is one we cannot prove is compliant.
	ProcessingRegionOf func(shared.GatewayID) string
}

// CandidateAssembler is the production CandidateBuilder: the impure counterpart to the pure
// routing engine.
//
// Its contract is unusually strict and worth stating plainly, because the failure mode is
// silent. Every boolean on routing.Candidate is a *hard filter* in the domain: a `true` that was
// not derived from a real source does not make routing slightly less accurate, it disables a
// filter. A hard-coded `Certified: true` routes an uncertified merchant's live traffic; a
// hard-coded `ResidencyCompliant: true` exports personal data outside a tenant's contractual
// region. So every field below is assigned from exactly one named source, and where a source
// cannot answer, the answer is false.
type CandidateAssembler struct {
	deps CandidateAssemblerDeps
}

// NewCandidateAssembler constructs the builder.
func NewCandidateAssembler(d CandidateAssemblerDeps) *CandidateAssembler {
	return &CandidateAssembler{deps: d}
}

// Build assembles the candidate set for one routing decision.
//
// The candidate universe is the merchant's *connections*, not the gateway registry: a gateway
// the merchant has no connection to cannot be routed to, and including it would produce a
// rejection reason (MERCHANT_NOT_CONFIGURED) for every gateway the platform has ever integrated,
// on every payment, which buries the reasons that matter.
//
// A gateway named in the merchant's routing policy but absent from their connections *is*
// included, deliberately, and is rejected as MERCHANT_NOT_CONFIGURED. That rejection is the
// answer to "my configuration says primary=adyen, why is nothing going there", and it is worth
// one candidate's worth of work to be able to give it.
func (b *CandidateAssembler) Build(ctx context.Context, req routing.RequestContext, m MerchantSnapshot) ([]routing.Candidate, error) {
	tn, err := b.deps.Tenants.Get(ctx, req.TenantID)
	if err != nil {
		return nil, err
	}

	universe := make(map[shared.GatewayID]struct{}, len(m.Connections)+2)
	for id := range m.Connections {
		universe[id] = struct{}{}
	}
	for _, id := range m.Routing.ReferencedGateways() {
		universe[id] = struct{}{}
	}

	out := make([]routing.Candidate, 0, len(universe))
	for id := range universe {
		c, err := b.candidate(ctx, id, req, m, tn)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// candidate answers every question about one gateway.
func (b *CandidateAssembler) candidate(ctx context.Context, id shared.GatewayID,
	req routing.RequestContext, m MerchantSnapshot, tn *tenant.Tenant) (routing.Candidate, error) {

	// Start from the all-false candidate. Every field below has to be *earned* from a source;
	// anything a source cannot answer stays false and the domain's hard filter removes the
	// candidate with a reason.
	c := routing.Candidate{GatewayID: id}

	// --- entitlement and compliance, from the tenant aggregate --------------------------------
	if tn != nil {
		c.TenantEntitled = containsGateway(tn.EnabledGateways(), id) && tn.IsOperational()
		c.ResidencyCompliant = tn.Residency().PermitsGatewayRegion(b.region(id))
	}

	// --- the merchant's connection ------------------------------------------------------------
	conn, hasConn := m.Connections[id]
	c.MerchantConfigured = hasConn && conn.UsableForPayments()
	// Certification is a gate, not a preference: the connection must both carry a passing
	// certification and be in a status that may take live traffic. A CERTIFIED connection whose
	// certification record says FAILED is a data defect, and the conjunction refuses it rather
	// than trusting whichever field was written last.
	c.Certified = hasConn && conn.Certified && conn.UsableForPayments()

	// --- the gateway descriptor ---------------------------------------------------------------
	g, err := b.deps.Gateways.Get(ctx, id)
	if err != nil || g == nil {
		// A descriptor we cannot read leaves every capability false. The candidate is dropped
		// with CAPABILITY_MISMATCH rather than the request failing: one unreadable descriptor
		// must not take out a merchant who has three other perfectly good gateways.
		return c, nil //nolint:nilerr // an unreadable descriptor drops this one candidate with every capability false; propagating it would fail a request whose other three gateways are healthy
	}
	caps := g.Capabilities()
	accepting := g.Status().AcceptsNewTraffic()
	c.SupportsCurrency = accepting && containsCurrency(caps.Currencies, req.Currency())
	c.SupportsMethod = accepting && containsMethod(caps.Methods, req.PaymentMethod)
	// Licensing is a property of the *corridor*, so both ends are checked: the payer's country
	// where we know it, and the merchant's country of establishment, which is what the acquiring
	// licence actually covers.
	c.SupportsCountry = accepting && supportsCorridor(caps, req.PayerCountry, m.Country)
	c.SupportsOperation = accepting && caps.SupportsOperation(req.Operation)
	c.SupportsThreeDS = caps.Supports3DS2
	if minAmt, ok := caps.MinAmount[req.Currency()]; ok {
		c.MinAmountMinorUnits = minAmt.Amount()
	}
	if maxAmt, ok := caps.MaxAmount[req.Currency()]; ok {
		c.MaxAmountMinorUnits = maxAmt.Amount()
	}

	// The breaker is read before the cost estimate, because the early return on an unpriced
	// gateway needs a truthful CircuitOpen to report a truthful reason.
	breakerKey := id.String() + ":" + string(req.Operation)

	// --- cost, from the gateway's own price list for *this* payment ---------------------------
	//
	// Per payment rather than per gateway: a 30¢ fixed fee dominates a $3 payment and is noise on
	// a $300 one, so a per-gateway cost figure would rank the candidates correctly for exactly
	// one amount. An unpriced gateway is not free — EstimateCost errors, and leaving the cost at
	// zero would make the gateway nobody has priced the cheapest candidate in every decision.
	cost, costErr := g.EstimateCost(req.Amount, req.PaymentMethod)
	if costErr != nil {
		// The gateway has no price for this (currency, method), so it is dropped — but it is
		// dropped as a *capability* mismatch, which is what it is, rather than being left with
		// the zero-valued Healthy field it would otherwise carry out of this early return.
		//
		// That distinction is not cosmetic. The routing engine checks health before capability,
		// so a candidate that returned here with Healthy unset was excluded with the reason
		// "connection health does not permit live traffic" — which sends whoever is debugging it
		// to the gateway's status page for a problem that is a missing row in a price list.
		// Marking health explicitly costs nothing and makes the reported reason true.
		c.SupportsOperation = false
		c.Healthy = !c.CircuitOpen
		return c, nil //nolint:nilerr // an unpriced gateway is dropped as a capability mismatch, which is what it is; propagating would fail the whole routing decision
	}
	c.CostMinorUnits = cost.Amount()

	// --- availability, from live health and the local breaker ---------------------------------
	if b.deps.Breakers != nil {
		// State() is a read, not a claim: it does not consume the breaker's probe budget, which
		// Allow() would. Routing must be able to ask "is this open" thousands of times a second
		// without becoming the thing that decides which request is the half-open probe.
		c.CircuitOpen = b.deps.Breakers.State(breakerKey) == circuitOpen
	}
	h, err := b.deps.Health.Get(ctx, id, req.Operation)
	switch {
	case err != nil || h == nil:
		// No health record is not evidence of health. The candidate scores neutral and is
		// treated as usable — a freshly started replica has no records at all, and refusing
		// every gateway on boot would make every deployment an outage — but the breaker's own
		// state above still applies.
		c.Healthy = !c.CircuitOpen
		c.HealthScore = gateway.NeutralScore
	default:
		c.Healthy = h.State().AllowsDispatch() && !c.CircuitOpen
		c.CircuitOpen = c.CircuitOpen || h.State() == gateway.HealthUnhealthy
		c.HealthScore = h.Score()
		c.LatencyP99MS = int(h.P99Latency().Milliseconds())
	}

	// --- the observed success rate, smoothed by the domain's own formula ----------------------
	if b.deps.Rates != nil {
		s, err := b.deps.Rates.For(ctx, id, req.PaymentMethod, req.Currency(), m.Country)
		if err == nil {
			prior := s.Prior
			if prior <= 0 {
				prior = DefaultSuccessPrior
			}
			c.SuccessRate = routing.SmoothSuccessRate(s.Successes, s.Samples, prior)
		} else {
			c.SuccessRate = DefaultSuccessPrior
		}
	} else {
		c.SuccessRate = DefaultSuccessPrior
	}
	return c, nil
}

// circuitOpen is the CircuitBreaker.State value meaning "open". The interface returns an int
// rather than a typed state so that the application layer does not have to import the
// resilience package to name it; the constant lives here so the comparison has a name.
const circuitOpen = 1

func (b *CandidateAssembler) region(id shared.GatewayID) string {
	if b.deps.ProcessingRegionOf == nil {
		return ""
	}
	return b.deps.ProcessingRegionOf(id)
}

// supportsCorridor reports whether the gateway is licensed for both ends of the corridor.
//
// An unknown payer country is not a failure: a great deal of legitimate traffic arrives without
// a resolved payer country, and refusing it would be a much larger outage than the licensing
// risk it avoids. The merchant's country of establishment, by contrast, is always known and is
// what the acquiring licence is actually issued against, so it is checked unconditionally.
func supportsCorridor(caps gateway.Capabilities, payer, merchantCountry shared.Country) bool {
	if merchantCountry != "" && !containsCountry(caps.Countries, merchantCountry) {
		return false
	}
	if payer == "" {
		return true
	}
	return containsCountry(caps.Countries, payer) || merchantCountry != ""
}

func containsGateway(set []shared.GatewayID, v shared.GatewayID) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

func containsCountry(set []shared.Country, v shared.Country) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

func containsCurrency(set []money.Currency, v money.Currency) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

func containsMethod(set []shared.PaymentMethod, v shared.PaymentMethod) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}
