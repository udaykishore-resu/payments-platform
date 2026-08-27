package payment

import (
	"context"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// DefaultScorerTimeout bounds the external fraud model's contribution to the request.
//
// 120 ms, from the 15 ms budget the risk *engine* has plus the slack the pipeline reserves for
// one optional network hop. The number matters less than the fact that there is one: an unbounded
// scorer call turns a vendor's bad afternoon into the platform's, and the scorer's own
// unavailability posture (REQUIRE_3DS, never APPROVE) is a strictly better outcome than waiting.
const DefaultScorerTimeout = 120 * time.Millisecond

// RiskAssessorDeps is what gathering a risk assessment requires.
type RiskAssessorDeps struct {
	// Velocity supplies the rolling counters.
	Velocity ports.VelocityCounter
	// Blocklists answers the four known-bad questions in one round trip.
	Blocklists Blocklist
	// History summarises the payer's relationship with this merchant, which is what the SCA
	// exemption rules rest on. Nil means "no history", which is a safe reading: it makes the
	// trusted-beneficiary and low-value exemptions unclaimable rather than claimable.
	History CustomerHistoryProvider
	// Scorer is the optional external fraud model. Nil is a supported configuration: the
	// platform runs without one, and the risk domain distinguishes "never asked" from "asked and
	// failed" so that the scorer-unavailable posture applies only to the ~3% of payments that
	// were in scope for scoring.
	Scorer ports.RiskScorer
	// ScorerTimeout bounds the scorer call. Zero means DefaultScorerTimeout.
	ScorerTimeout time.Duration
	// TRAScoreCeiling is the risk score below which transaction risk analysis may be claimed,
	// and is also the signal that a payment was in scope for scoring at all — see
	// risk.Assessment.ScorerWasInvoked. Zero means the platform does not claim TRA.
	TRAScoreCeiling int
	// LowValueCeiling and LowValueCumulativeCeiling are the PSD2 exemption thresholds expressed
	// in the payment's currency. They are supplied rather than derived because the regulatory
	// figure is in EUR and any other currency is a conversion the domain must not improvise.
	LowValueCeiling           map[money.Currency]money.Money
	LowValueCumulativeCeiling map[money.Currency]money.Money
	Clock                     shared.Clock
}

// RiskAssessor is the production RiskEvaluator: the impure gathering half of a risk decision.
//
// The division of labour is the point. risk.Evaluate is pure, deterministic and replayable —
// which is what a chargeback representment, a scheme audit and an unhappy merchant all actually
// ask for. Everything that could make it un-replayable (a Redis read, a vendor call, a clock)
// happens here, once, and is frozen onto the Assessment before Evaluate is called.
//
// The one non-obvious rule this type enforces: **an unavailable input becomes the domain's
// unavailability marker, never a zero.** Zero means "no payments in the window", which is the
// safest possible reading and therefore the most dangerous thing an outage can silently produce.
type RiskAssessor struct {
	deps RiskAssessorDeps
}

// NewRiskAssessor constructs the evaluator.
func NewRiskAssessor(d RiskAssessorDeps) *RiskAssessor {
	if d.Clock == nil {
		d.Clock = shared.SystemClock{}
	}
	if d.ScorerTimeout <= 0 {
		d.ScorerTimeout = DefaultScorerTimeout
	}
	return &RiskAssessor{deps: d}
}

// Evaluate gathers the inputs and runs the domain's decision function.
//
// The three lookups run concurrently under one bounded deadline. Concurrency here is not a
// micro-optimisation: the counters are a pipelined Redis round trip, the blocklist is another,
// and the scorer is a vendor HTTP call, so running them in series would spend the sum of three
// tail latencies inside a stage budgeted for one. What the concurrency must *not* do is let a
// slow scorer hold the other two — hence the scorer's own child deadline, and hence the fact
// that its failure degrades to the policy posture rather than to an error.
func (a *RiskAssessor) Evaluate(ctx context.Context, in RiskInput) (risk.Decision, error) {
	keys := KeysFor(in.TenantID, in.MerchantID, in.MethodRef, in.Customer)

	var (
		wg       sync.WaitGroup
		readout  Readout
		blocked  BlocklistAnswer
		history  risk.CustomerHistory
		external = risk.UnavailableScore()
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		readout = ReadVelocity(ctx, a.deps.Velocity, keys, in.Amount.Currency())
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if a.deps.Blocklists == nil {
			// No blocklist configured is not the same as a blocklist that failed to answer. With
			// no store at all there is nothing to be unavailable, and treating it as an outage
			// would force 3DS on every payment on a deployment that simply does not run one.
			blocked = BlocklistAnswer{Available: true}
			return
		}
		ans, err := a.deps.Blocklists.Lookup(ctx, BlocklistQuery{
			TenantID: in.TenantID, MerchantID: in.MerchantID,
			Fingerprint: keys.Fingerprint,
			EmailHash:   in.Customer.EmailHash,
			IPAddress:   in.Customer.IPAddress,
			CustomerRef: in.Customer.MerchantCustomerID,
		})
		if err != nil {
			blocked = BlocklistAnswer{Available: false}
			return
		}
		blocked = ans
	}()

	if a.deps.History != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := a.deps.History.History(ctx, in.MerchantID, in.Customer.MerchantCustomerID)
			if err == nil {
				history = h
			}
		}()
	}

	// The scorer runs concurrently with the rest under its own, shorter deadline. Cancelling it
	// is safe in a way that cancelling a gateway call is not: a scorer has no side effect, so an
	// abandoned request costs a vendor a wasted computation and costs us nothing ambiguous.
	if a.deps.Scorer != nil && a.deps.TRAScoreCeiling > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sctx, cancel := context.WithTimeout(ctx, a.deps.ScorerTimeout)
			defer cancel()
			res, err := a.deps.Scorer.Score(sctx, ports.RiskScoreRequest{
				PaymentID: in.PaymentID, MerchantID: in.MerchantID, Amount: in.Amount,
				PaymentMethod: in.Method, PayerCountry: in.Customer.Country,
				IssuerCountry: in.MethodRef.Country, EmailHash: in.Customer.EmailHash,
				IPAddress: in.Customer.IPAddress,
			})
			if err != nil {
				// Left as UnavailableScore. The domain then applies the scorer's configured
				// posture — REQUIRE_3DS by default, never APPROVE, because the only reason the
				// scorer was invoked is that the policy said this payment warranted a look.
				return
			}
			external = risk.KnownScore(res.Score)
		}()
	}

	wg.Wait()

	assessment := risk.Assessment{
		TenantID: in.TenantID, MerchantID: in.MerchantID, PaymentID: in.PaymentID,
		Amount: in.Amount, PaymentMethod: in.Method,
		MerchantRating: merchantRating(in.Merchant.RiskRating),

		PayerCountry:    in.Customer.Country,
		IssuingCountry:  in.MethodRef.Country,
		IPCountry:       in.Customer.Country,
		MerchantCountry: in.Merchant.Country,
		// SCA is a property of the corridor, not of either end: a German payer buying from a
		// German merchant and a German payer buying from a US merchant are different regulatory
		// situations, and asking either country alone answers the wrong question.
		SCAJurisdiction: in.Customer.Country.IsSCAJurisdiction() || in.Merchant.Country.IsSCAJurisdiction(),

		Counters: readout.Counters(),
		History:  history,

		OnPlatformBlocklist: blocked.OnPlatform,
		OnMerchantBlocklist: blocked.OnMerchant,
		OnMerchantAllowlist: blocked.OnMerchantAllowed,
		BlocklistAvailable:  blocked.Available,

		ExternalScore: external,

		MerchantInitiated:         false,
		CorporateCard:             false,
		LowValueCeiling:           a.ceiling(a.deps.LowValueCeiling, in.Amount.Currency()),
		LowValueCumulativeCeiling: a.ceiling(a.deps.LowValueCumulativeCeiling, in.Amount.Currency()),
		TRAScoreCeiling:           a.deps.TRAScoreCeiling,

		EvaluatedAt: in.Now,
	}
	if assessment.EvaluatedAt.IsZero() {
		assessment.EvaluatedAt = a.deps.Clock.Now()
	}
	return risk.Evaluate(in.Policy, assessment), nil
}

// ceiling resolves a regulatory threshold in the payment's currency, or the zero Money when the
// platform has not published one for it. A zero ceiling makes the exemption unclaimable, which
// is the fail-closed direction: an exemption wrongly withheld costs a conversion, an exemption
// wrongly claimed is an unauthenticated payment the merchant is liable for.
func (a *RiskAssessor) ceiling(m map[money.Currency]money.Money, c money.Currency) money.Money {
	if m == nil {
		return money.Money{}
	}
	v, ok := m[c]
	if !ok {
		return money.Money{}
	}
	return v
}

// merchantRating maps the merchant registry's rating string onto the risk domain's own type.
// An unrecognised rating becomes STANDARD rather than being passed through: the rating drives
// the blocklist escalation for high-risk merchants, and an unknown value silently disabling that
// escalation is the wrong direction to fail in.
func merchantRating(s string) risk.MerchantRiskRating {
	r := risk.MerchantRiskRating(s)
	if r.IsValid() {
		return r
	}
	return risk.RatingStandard
}
