package payment

import (
	"context"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// The velocity windows. They are constants rather than configuration because the *window* is
// part of the check's identity — "payments per minute" measured over five minutes is a different
// check wearing the same name — while the *limit* is the merchant's to configure.
const (
	windowPerMinute  = time.Minute
	windowPerHour    = time.Hour
	windowPerDay     = 24 * time.Hour
	windowDeclineObs = 15 * time.Minute
)

// VelocityKeys renders the counter keys for one payment.
//
// The key layout is a function rather than string concatenation at each call site because the
// same keys are written by the post-commit increment path and read by the pre-dispatch check,
// and a divergence between the two is silent: the check reads a key nobody increments, gets
// zero, and passes forever.
type VelocityKeys struct {
	TenantID    shared.TenantID
	MerchantID  shared.MerchantID
	Fingerprint string
	CustomerRef string
}

// KeysFor builds the velocity key set for a payment request.
func KeysFor(t shared.TenantID, m shared.MerchantID, ref payment.PaymentMethodReference, c payment.CustomerReference) VelocityKeys {
	fp := ref.Token
	if ref.Last4 != "" && ref.Brand != "" {
		// Prefer the instrument fingerprint where the tokenizer supplied one; the token itself is
		// per-merchant and would let an attacker rotate tokens to reset a per-card counter.
		fp = ref.Brand + ":" + ref.Last4 + ":" + ref.Token
	}
	return VelocityKeys{TenantID: t, MerchantID: m, Fingerprint: fp, CustomerRef: c.MerchantCustomerID}
}

func (k VelocityKeys) merchantMinute() string {
	return "vel:" + k.TenantID.String() + ":" + k.MerchantID.String() + ":pm"
}

func (k VelocityKeys) merchantVolume() string {
	return "vel:" + k.TenantID.String() + ":" + k.MerchantID.String() + ":vol"
}

func (k VelocityKeys) cardHour() string {
	return "vel:" + k.TenantID.String() + ":" + k.MerchantID.String() + ":card:" + k.Fingerprint
}

func (k VelocityKeys) customerDay() string {
	return "vel:" + k.TenantID.String() + ":" + k.MerchantID.String() + ":cust:" + k.CustomerRef
}

func (k VelocityKeys) distinctCards() string {
	return "vel:" + k.TenantID.String() + ":" + k.MerchantID.String() + ":cards:" + k.CustomerRef
}

func (k VelocityKeys) attempts15() string {
	return "vel:" + k.TenantID.String() + ":" + k.MerchantID.String() + ":att15"
}

func (k VelocityKeys) declines15() string {
	return "vel:" + k.TenantID.String() + ":" + k.MerchantID.String() + ":dec15"
}

// Readout is one pipelined read of every velocity counter a payment needs, carrying the
// availability of each answer alongside its value.
//
// This type exists because of a single rule that both the validation plane and the risk engine
// depend on and that neither can express on its own: **an unreadable counter is not a counter
// of zero**. Zero is the safest possible reading and therefore the most dangerous thing an
// outage can silently produce — a card tester gets an unbounded window the moment Redis blips.
//
// The two consumers need the same fact in two different shapes, and the Readout produces both
// from one read:
//
//   - The risk domain models unavailability explicitly (risk.Counter, risk.Volume), so the
//     marker is passed straight through and the merchant's configured failure posture decides.
//   - The L5 subject carries plain integers and has no marker. Passing zero there would make an
//     unreadable counter *pass* the rule, so instead the corresponding limit is zeroed, which
//     makes the rule's precondition false and records it as skipped rather than passed. The
//     decision then belongs entirely to the risk engine's posture, which is where the platform
//     has actually written down what an unavailable signal means.
//
// Recording the two differently is not duplication; it is the same decision expressed in the two
// vocabularies, taken once, here.
type Readout struct {
	PerMinute      risk.Counter
	CardHour       risk.Counter
	CustomerDay    risk.Counter
	DistinctCards  risk.Counter
	VolumeToday    risk.Volume
	Attempts15Min  risk.Counter
	Declines15Min  risk.Counter
	AnyUnavailable bool
}

// Counters renders the readout in the risk domain's vocabulary, unavailability included.
func (r Readout) Counters() risk.Counters {
	return risk.Counters{
		PaymentsThisMinute: r.PerMinute,
		CardThisHour:       r.CardHour,
		CustomerToday:      r.CustomerDay,
		DistinctCardsToday: r.DistinctCards,
		VolumeToday:        r.VolumeToday,
	}
}

// ReadVelocity performs the pipelined read.
//
// Every read is best-effort by design: a counter store failure degrades one check rather than
// failing the payment, because the alternative — a five-minute cache outage becoming a total
// payment outage for every merchant — is a self-inflicted incident strictly worse than the
// fraud it prevents. What is *not* best-effort is the record of which reads failed; that is what
// AnyUnavailable and the per-field markers carry forward.
func ReadVelocity(ctx context.Context, vc ports.VelocityCounter, k VelocityKeys, currency money.Currency) Readout {
	out := Readout{
		PerMinute:     risk.UnavailableCount(),
		CardHour:      risk.UnavailableCount(),
		CustomerDay:   risk.UnavailableCount(),
		DistinctCards: risk.UnavailableCount(),
		VolumeToday:   risk.UnavailableVolume(),
		Attempts15Min: risk.UnavailableCount(),
		Declines15Min: risk.UnavailableCount(),
	}
	if vc == nil {
		out.AnyUnavailable = true
		return out
	}

	count := func(key string, window time.Duration) risk.Counter {
		n, err := vc.Count(ctx, key, window)
		if err != nil {
			out.AnyUnavailable = true
			return risk.UnavailableCount()
		}
		return risk.KnownCount(n)
	}

	out.PerMinute = count(k.merchantMinute(), windowPerMinute)
	if k.Fingerprint != "" {
		out.CardHour = count(k.cardHour(), windowPerHour)
	} else {
		// No instrument fingerprint is not an unavailable counter: there is nothing to count.
		// Reporting it as known-zero keeps the check meaningful for the payments that do carry
		// one and stops the posture machinery firing for methods that have no card at all.
		out.CardHour = risk.KnownCount(0)
	}
	if k.CustomerRef != "" {
		out.CustomerDay = count(k.customerDay(), windowPerDay)
		out.DistinctCards = count(k.distinctCards(), windowPerHour)
	} else {
		out.CustomerDay = risk.KnownCount(0)
		out.DistinctCards = risk.KnownCount(0)
	}
	out.Attempts15Min = count(k.attempts15(), windowDeclineObs)
	out.Declines15Min = count(k.declines15(), windowDeclineObs)

	if currency.IsSupported() {
		// SumAndAdd with a zero addend is a read: the operation exists because increment-and-read
		// must be atomic on the write path, and reusing it here keeps one key format rather than
		// two.
		v, err := vc.SumAndAdd(ctx, k.merchantVolume(), windowPerDay, money.Zero(currency))
		if err != nil {
			out.AnyUnavailable = true
		} else {
			out.VolumeToday = risk.KnownVolume(v)
		}
	} else {
		out.AnyUnavailable = true
	}
	return out
}
