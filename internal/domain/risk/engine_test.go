package risk_test

import (
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

var evaluatedAt = time.Date(2026, 8, 26, 14, 3, 11, 0, time.UTC)

// cleanAssessment is a payment that passes every check: a modest US card sale from an
// established customer, with every counter available and comfortably under its limit. Each test
// changes exactly the one thing it is about.
func cleanAssessment() risk.Assessment {
	return risk.Assessment{
		TenantID:       "ten_x",
		MerchantID:     "mrc_x",
		PaymentID:      "pay_x",
		Amount:         usd(4_000),
		PaymentMethod:  shared.MethodCard,
		MerchantRating: risk.RatingStandard,
		PayerCountry:   shared.Country("US"),
		IssuingCountry: shared.Country("US"),
		IPCountry:      shared.Country("US"),
		Counters: risk.Counters{
			PaymentsThisMinute: risk.KnownCount(10),
			CardThisHour:       risk.KnownCount(1),
			CustomerToday:      risk.KnownCount(2),
			DistinctCardsToday: risk.KnownCount(1),
			VolumeToday:        risk.KnownVolume(usd(100_000)),
		},
		History: risk.CustomerHistory{
			SuccessfulPayments:     42,
			DaysSinceFirstPayment:  400,
			CumulativeExemptAmount: usd(0),
		},
		BlocklistAvailable: true,
		EvaluatedAt:        evaluatedAt,
	}
}

func TestEvaluateApprovesACleanPayment(t *testing.T) {
	// Verifies: BR-15, FR-61.
	t.Parallel()

	d := risk.Evaluate(validPolicy(), cleanAssessment())
	if d.Outcome != risk.OutcomeApprove {
		t.Fatalf("outcome = %s, reasons %v", d.Outcome, d.ReasonCodes())
	}
	if d.Require3DS {
		t.Error("a USD 40 domestic card sale needs no authentication")
	}
	if d.Degraded {
		t.Error("nothing was unavailable")
	}
	if d.EvaluatedAt != evaluatedAt {
		t.Errorf("EvaluatedAt = %v", d.EvaluatedAt)
	}
	if d.PolicyVersion != 7 {
		t.Errorf("PolicyVersion = %d; a replay must know which policy decided", d.PolicyVersion)
	}
	if d.Err() != nil {
		t.Errorf("an approved decision must produce no error, got %v", d.Err())
	}
}

// --- each check triggers in isolation --------------------------------------------------------

func TestEachCheckTriggersInIsolation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mutate      func(*risk.Policy, *risk.Assessment)
		wantOutcome risk.Outcome
		wantCheck   risk.CheckID
	}{
		{
			name: "a sanctioned payer country declines",
			mutate: func(_ *risk.Policy, a *risk.Assessment) {
				a.PayerCountry = shared.Country("KP")
			},
			wantOutcome: risk.OutcomeDecline,
			wantCheck:   risk.CheckSanctionedCountry,
		},
		{
			name: "a sanctioned network origin declines even when the billing country is clean",
			mutate: func(_ *risk.Policy, a *risk.Assessment) {
				a.IPCountry = shared.Country("IR")
			},
			wantOutcome: risk.OutcomeDecline,
			wantCheck:   risk.CheckIPCountrySanctioned,
		},
		{
			name: "a country outside the merchant's positive allowlist declines",
			mutate: func(p *risk.Policy, a *risk.Assessment) {
				p.AllowedCountries = []shared.Country{"DE", "FR"}
			},
			wantOutcome: risk.OutcomeDecline,
			wantCheck:   risk.CheckCountrySupported,
		},
		{
			name: "a platform blocklist hit declines",
			mutate: func(_ *risk.Policy, a *risk.Assessment) {
				a.OnPlatformBlocklist = true
			},
			wantOutcome: risk.OutcomeDecline,
			wantCheck:   risk.CheckPlatformBlocklist,
		},
		{
			name: "a merchant blocklist hit declines",
			mutate: func(_ *risk.Policy, a *risk.Assessment) {
				a.OnMerchantBlocklist = true
			},
			wantOutcome: risk.OutcomeDecline,
			wantCheck:   risk.CheckMerchantBlocklist,
		},
		{
			name: "an amount above the merchant's ceiling declines",
			mutate: func(_ *risk.Policy, a *risk.Assessment) {
				a.Amount = usd(2_000_000)
			},
			wantOutcome: risk.OutcomeDecline,
			wantCheck:   risk.CheckAmountWithinLimit,
		},
		{
			name: "a payment that would breach the daily volume limit declines",
			mutate: func(_ *risk.Policy, a *risk.Assessment) {
				a.Counters.VolumeToday = risk.KnownVolume(usd(49_999_999))
			},
			wantOutcome: risk.OutcomeDecline,
			wantCheck:   risk.CheckDailyVolume,
		},
		{
			name: "the merchant's per-minute rate limit declines",
			mutate: func(p *risk.Policy, a *risk.Assessment) {
				a.Counters.PaymentsThisMinute = risk.KnownCount(300)
			},
			wantOutcome: risk.OutcomeDecline,
			wantCheck:   risk.CheckVelocityPerMinute,
		},
		{
			name: "the per-card hourly limit declines",
			mutate: func(_ *risk.Policy, a *risk.Assessment) {
				a.Counters.CardThisHour = risk.KnownCount(5)
			},
			wantOutcome: risk.OutcomeDecline,
			wantCheck:   risk.CheckVelocityPerCard,
		},
		{
			name: "the per-customer daily limit declines",
			mutate: func(_ *risk.Policy, a *risk.Assessment) {
				a.Counters.CustomerToday = risk.KnownCount(20)
			},
			wantOutcome: risk.OutcomeDecline,
			wantCheck:   risk.CheckVelocityPerCustomer,
		},
		{
			name: "distinct cards per customer, the card-testing signature, declines",
			mutate: func(_ *risk.Policy, a *risk.Assessment) {
				a.Counters.DistinctCardsToday = risk.KnownCount(3)
			},
			wantOutcome: risk.OutcomeDecline,
			wantCheck:   risk.CheckDistinctCards,
		},
		{
			name: "a score at the decline threshold declines",
			mutate: func(_ *risk.Policy, a *risk.Assessment) {
				a.ExternalScore = risk.KnownScore(90)
			},
			wantOutcome: risk.OutcomeDecline,
			wantCheck:   risk.CheckRiskScore,
		},
		{
			name: "a score at the review threshold holds for a human",
			mutate: func(_ *risk.Policy, a *risk.Assessment) {
				a.ExternalScore = risk.KnownScore(80)
			},
			wantOutcome: risk.OutcomeReview,
			wantCheck:   risk.CheckRiskScore,
		},
		{
			name: "an amount above the merchant's 3DS threshold forces authentication",
			mutate: func(_ *risk.Policy, a *risk.Assessment) {
				a.Amount = usd(60_000)
			},
			wantOutcome: risk.OutcomeRequire3DS,
			wantCheck:   risk.CheckThreeDSThreshold,
		},
		{
			name: "an allowlisted payer records the skip",
			mutate: func(_ *risk.Policy, a *risk.Assessment) {
				a.OnMerchantAllowlist = true
			},
			wantOutcome: risk.OutcomeApprove,
			wantCheck:   risk.CheckMerchantAllowlist,
		},
		{
			name: "an SCA-jurisdiction corridor with no claimable exemption forces authentication",
			mutate: func(_ *risk.Policy, a *risk.Assessment) {
				a.SCAJurisdiction = true
			},
			wantOutcome: risk.OutcomeRequire3DS,
			wantCheck:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, a := validPolicy(), cleanAssessment()
			tt.mutate(&p, &a)
			d := risk.Evaluate(p, a)
			if d.Outcome != tt.wantOutcome {
				t.Fatalf("outcome = %s, want %s; reasons %v", d.Outcome, tt.wantOutcome, d.ReasonCodes())
			}
			if tt.wantCheck != "" && !d.HasReason(tt.wantCheck) {
				t.Fatalf("expected reason %s, got %v", tt.wantCheck, d.ReasonCodes())
			}
			if tt.wantCheck != "" {
				r, _ := d.ReasonFor(tt.wantCheck)
				if r.Detail == "" {
					t.Error("a reason must carry a detail an operator can act on")
				}
			}
		})
	}
}

// The counter holds the state *before* this payment — counters are incremented after the L7
// commit — so the limit is breached when the count already equals it.
func TestVelocityBoundaryIsInclusiveOfTheLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		count int64
		want  risk.Outcome
	}{
		{"one below the limit passes", 4, risk.OutcomeApprove},
		{"at the limit declines, because this payment would be the sixth", 5, risk.OutcomeDecline},
		{"above the limit declines", 6, risk.OutcomeDecline},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := cleanAssessment()
			a.Counters.CardThisHour = risk.KnownCount(tt.count)
			if got := risk.Evaluate(validPolicy(), a).Outcome; got != tt.want {
				t.Fatalf("outcome = %s, want %s", got, tt.want)
			}
		})
	}
}

// An unconfigured velocity limit means "unenforced", not "zero permitted". Reading an absent
// field as a limit of zero would decline 100% of a merchant's traffic.
func TestUnconfiguredVelocityLimitIsUnenforced(t *testing.T) {
	t.Parallel()

	p := validPolicy()
	p.Velocity = risk.Velocity{}
	p.MaxCardsPerCustomerPerDay = 0

	a := cleanAssessment()
	a.Counters.CardThisHour = risk.KnownCount(9999)
	// An unenforced check must not even consult its counter, so an unavailable counter for a
	// check nobody configured produces no friction and no degradation.
	a.Counters.PaymentsThisMinute = risk.UnavailableCount()

	d := risk.Evaluate(p, a)
	if d.Outcome != risk.OutcomeApprove {
		t.Fatalf("outcome = %s, reasons %v", d.Outcome, d.ReasonCodes())
	}
	if d.Degraded {
		t.Error("an unenforced check does not degrade the evaluation")
	}
}

// --- fail-open vs fail-closed posture ----------------------------------------------------------

func TestFailurePostureBehavesAsDocumented(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		posture risk.FailurePosture
		want    risk.Outcome
	}{
		{
			// The documented default. Friction, not a decline and not a free pass.
			name:    "the default posture converts a lost counter into forced authentication",
			posture: "", want: risk.OutcomeRequire3DS,
		},
		{
			name:    "an explicit fail-closed posture declines",
			posture: risk.PostureFailClosed, want: risk.OutcomeDecline,
		},
		{
			name:    "an explicit review posture holds for a human",
			posture: risk.PostureReview, want: risk.OutcomeReview,
		},
		{
			// Legitimate only for genuinely additive signals; configuring it for a velocity
			// counter is how a platform discovers card testing from its chargeback statement.
			name:    "an explicit fail-open posture approves and is still recorded as degraded",
			posture: risk.PostureFailOpen, want: risk.OutcomeApprove,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := validPolicy()
			if tt.posture != "" {
				p.Postures = map[risk.CheckID]risk.FailurePosture{risk.CheckVelocityPerCard: tt.posture}
			}
			a := cleanAssessment()
			a.Counters.CardThisHour = risk.UnavailableCount()

			d := risk.Evaluate(p, a)
			if d.Outcome != tt.want {
				t.Fatalf("outcome = %s, want %s; reasons %v", d.Outcome, tt.want, d.ReasonCodes())
			}
			if !d.Degraded {
				t.Error("a check that ran on unavailable data must mark the evaluation degraded, " +
					"or 'how much traffic did we process without velocity checks' has no answer")
			}
			if !d.HasReason(risk.CheckVelocityPerCard) {
				t.Fatalf("the degraded check must be recorded, got %v", d.ReasonCodes())
			}
		})
	}
}

// The amount check knows the amount and the limit; there is nothing to fail. A currency mismatch
// is the one way it can fail to evaluate, and it declines — a limit that cannot be checked is a
// limit that has not been satisfied.
func TestAmountLimitFailsClosed(t *testing.T) {
	// Verifies: BR-14.
	t.Parallel()

	a := cleanAssessment()
	a.Amount = eur(4_000) // the policy's limits are in USD

	d := risk.Evaluate(validPolicy(), a)
	if d.Outcome != risk.OutcomeDecline {
		t.Fatalf("outcome = %s, want DECLINE; reasons %v", d.Outcome, d.ReasonCodes())
	}
	if !d.HasReason(risk.CheckAmountWithinLimit) {
		t.Fatalf("reasons = %v", d.ReasonCodes())
	}
}

// §6.2: the money-based daily limit fails closed, because exceeding a contractual volume limit is
// a compliance finding while losing count sensitivity for minutes is not.
func TestDailyVolumeFailsClosedByDefaultAndCountVelocityDoesNot(t *testing.T) {
	t.Parallel()

	t.Run("an unavailable volume declines", func(t *testing.T) {
		t.Parallel()
		a := cleanAssessment()
		a.Counters.VolumeToday = risk.UnavailableVolume()
		d := risk.Evaluate(validPolicy(), a)
		if d.Outcome != risk.OutcomeDecline {
			t.Fatalf("outcome = %s, want DECLINE", d.Outcome)
		}
		if !d.Degraded || !d.HasReason(risk.CheckDailyVolume) {
			t.Fatalf("degraded=%v reasons=%v", d.Degraded, d.ReasonCodes())
		}
	})

	t.Run("every count counter unavailable is friction, not an outage", func(t *testing.T) {
		t.Parallel()
		a := cleanAssessment()
		a.Counters.PaymentsThisMinute = risk.UnavailableCount()
		a.Counters.CardThisHour = risk.UnavailableCount()
		a.Counters.CustomerToday = risk.UnavailableCount()
		a.Counters.DistinctCardsToday = risk.UnavailableCount()

		d := risk.Evaluate(validPolicy(), a)
		if d.Outcome != risk.OutcomeRequire3DS {
			t.Fatalf("outcome = %s, want REQUIRE_3DS", d.Outcome)
		}
		// Each independent check is recorded: "which limit did we lose" is the first question an
		// operator asks.
		for _, c := range []risk.CheckID{
			risk.CheckVelocityPerMinute, risk.CheckVelocityPerCard,
			risk.CheckVelocityPerCustomer, risk.CheckDistinctCards,
		} {
			if !d.HasReason(c) {
				t.Errorf("expected %s to be recorded, got %v", c, d.ReasonCodes())
			}
		}
	})
}

func TestBlocklistUnavailability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		rating risk.MerchantRiskRating
		want   risk.Outcome
	}{
		{"a standard merchant gets friction", risk.RatingStandard, risk.OutcomeRequire3DS},
		{"an elevated merchant gets friction", risk.RatingElevated, risk.OutcomeRequire3DS},
		{
			// The platform's own scheme registrations are what is exposed by getting this wrong.
			name: "a high-risk merchant fails closed", rating: risk.RatingHigh, want: risk.OutcomeDecline,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := cleanAssessment()
			a.BlocklistAvailable = false
			a.MerchantRating = tt.rating
			d := risk.Evaluate(validPolicy(), a)
			if d.Outcome != tt.want {
				t.Fatalf("outcome = %s, want %s; reasons %v", d.Outcome, tt.want, d.ReasonCodes())
			}
			if !d.Degraded {
				t.Error("expected a degraded evaluation")
			}
		})
	}
}

// The scorer was invoked at all only because the compiled policy's predicate said this payment
// warranted a look, so "we could not look" is not evidence that it was fine.
func TestExternalScorerNeverFailsToApprove(t *testing.T) {
	t.Parallel()

	a := cleanAssessment()
	a.ExternalScore = risk.UnavailableScore()
	a.TRAScoreCeiling = 40 // marks the payment as in scope for scoring

	d := risk.Evaluate(validPolicy(), a)
	if d.Outcome != risk.OutcomeRequire3DS {
		t.Fatalf("outcome = %s, want REQUIRE_3DS; reasons %v", d.Outcome, d.ReasonCodes())
	}
	if !d.Degraded || !d.HasReason(risk.CheckRiskScore) {
		t.Fatalf("degraded=%v reasons=%v", d.Degraded, d.ReasonCodes())
	}
}

// Only ~3% of payments meet the scoring predicate. Applying the scorer-unavailable posture to
// the other 97% would force 3DS on the whole platform.
func TestAScorerThatWasNeverInvokedIsNotADegradation(t *testing.T) {
	t.Parallel()

	a := cleanAssessment()
	a.ExternalScore = risk.UnavailableScore()

	d := risk.Evaluate(validPolicy(), a)
	if d.Outcome != risk.OutcomeApprove {
		t.Fatalf("outcome = %s, reasons %v", d.Outcome, d.ReasonCodes())
	}
	if d.Degraded {
		t.Error("a scorer that was never asked is not a degraded evaluation")
	}
}

// --- allowlist scope ------------------------------------------------------------------------

// An allowlist is an assertion about fraud — "I know this customer" — and it is not an assertion
// about the merchant's processing agreement.
func TestAllowlistSkipsFraudSignalsButNotContractualLimits(t *testing.T) {
	t.Parallel()

	t.Run("velocity is skipped", func(t *testing.T) {
		t.Parallel()
		a := cleanAssessment()
		a.OnMerchantAllowlist = true
		a.Counters.CardThisHour = risk.KnownCount(500)
		if got := risk.Evaluate(validPolicy(), a).Outcome; got != risk.OutcomeApprove {
			t.Fatalf("outcome = %s, want APPROVE", got)
		}
	})

	t.Run("the per-transaction ceiling is not skipped", func(t *testing.T) {
		t.Parallel()
		a := cleanAssessment()
		a.OnMerchantAllowlist = true
		a.Amount = usd(2_000_000)
		d := risk.Evaluate(validPolicy(), a)
		if d.Outcome != risk.OutcomeDecline || !d.HasReason(risk.CheckAmountWithinLimit) {
			t.Fatalf("outcome = %s, reasons %v", d.Outcome, d.ReasonCodes())
		}
	})

	t.Run("the daily volume limit is not skipped", func(t *testing.T) {
		t.Parallel()
		a := cleanAssessment()
		a.OnMerchantAllowlist = true
		a.Counters.VolumeToday = risk.KnownVolume(usd(49_999_999))
		d := risk.Evaluate(validPolicy(), a)
		if d.Outcome != risk.OutcomeDecline || !d.HasReason(risk.CheckDailyVolume) {
			t.Fatalf("outcome = %s, reasons %v", d.Outcome, d.ReasonCodes())
		}
	})

	t.Run("sanctions are not skipped", func(t *testing.T) {
		t.Parallel()
		a := cleanAssessment()
		a.OnMerchantAllowlist = true
		a.PayerCountry = shared.Country("KP")
		d := risk.Evaluate(validPolicy(), a)
		if d.Outcome != risk.OutcomeDecline || !d.HasReason(risk.CheckSanctionedCountry) {
			t.Fatalf("outcome = %s, reasons %v", d.Outcome, d.ReasonCodes())
		}
	})
}

// --- evaluation stops at the first terminal decision --------------------------------------------

func TestEvaluationStopsAtTheFirstDecline(t *testing.T) {
	t.Parallel()

	a := cleanAssessment()
	a.PayerCountry = shared.Country("KP") // check 1
	a.Amount = usd(2_000_000)             // check 7, would also decline
	a.Counters.CardThisHour = risk.KnownCount(500)

	d := risk.Evaluate(validPolicy(), a)
	if len(d.Reasons) != 1 {
		t.Fatalf("expected evaluation to stop at the first terminal decision, got %v", d.ReasonCodes())
	}
	if d.Reasons[0].Check != risk.CheckSanctionedCountry {
		t.Fatalf("first reason = %s", d.Reasons[0].Check)
	}
	if d.Reasons[0].Severity != risk.SeverityCritical {
		t.Errorf("a decline reason should be critical, got %s", d.Reasons[0].Severity)
	}
}

// --- SCA and exemptions ------------------------------------------------------------------------

func TestEachExemptionAppliesAndIsRecorded(t *testing.T) {
	t.Parallel()

	// Every case is an SCA-jurisdiction card payment that would otherwise require
	// authentication, so the only thing under test is whether the exemption waives it.
	base := func() risk.Assessment {
		a := cleanAssessment()
		a.SCAJurisdiction = true
		a.PayerCountry = shared.Country("DE")
		a.IssuingCountry = shared.Country("DE")
		a.IPCountry = shared.Country("DE")
		return a
	}

	tests := []struct {
		name   string
		setup  func(*risk.Assessment)
		expect risk.ExemptionType
	}{
		{
			name: "MIT with a network reference from the authenticated original",
			setup: func(a *risk.Assessment) {
				a.MerchantInitiated = true
				a.NetworkTransactionRef = "MCC0012345678901"
			},
			expect: risk.ExemptionMIT,
		},
		{
			name:   "a secure corporate card is out of SCA scope",
			setup:  func(a *risk.Assessment) { a.CorporateCard = true },
			expect: risk.ExemptionCorporate,
		},
		{
			name:   "a trusted beneficiary listing asserted by the issuer",
			setup:  func(a *risk.Assessment) { a.History.TrustedBeneficiary = true },
			expect: risk.ExemptionTrustedBeneficiary,
		},
		{
			name: "TRA within the acquirer's fraud-rate band and below the score ceiling",
			setup: func(a *risk.Assessment) {
				a.TRABandCeiling = usd(10_000)
				a.TRAScoreCeiling = 30
				a.ExternalScore = risk.KnownScore(12)
			},
			expect: risk.ExemptionTRA,
		},
		{
			name: "a low-value payment within the cumulative ceiling and consecutive count",
			setup: func(a *risk.Assessment) {
				a.LowValueCeiling = usd(5_000)
				a.LowValueCumulativeCeiling = usd(10_000)
				a.History.ConsecutiveExemptSinceLastSCA = 2
				a.History.CumulativeExemptAmount = usd(1_000)
			},
			expect: risk.ExemptionLowValue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := base()
			tt.setup(&a)

			if !tt.expect.Applies(a) {
				t.Fatalf("%s.Applies should be true for this assessment", tt.expect)
			}

			d := risk.Evaluate(validPolicy(), a)
			if d.Require3DS {
				t.Fatalf("a claimable %s exemption must waive the regulatory requirement", tt.expect)
			}
			if d.ExemptionApplied != tt.expect {
				t.Fatalf("ExemptionApplied = %q, want %s", d.ExemptionApplied, tt.expect)
			}
			// An exemption that is not recorded is an exemption you cannot defend.
			if !d.HasReason(risk.CheckSCAExemption) {
				t.Fatalf("the exemption claim must be recorded as a reason, got %v", d.ReasonCodes())
			}
			r, _ := d.ReasonFor(risk.CheckSCAExemption)
			if r.Detail == "" {
				t.Error("the exemption reason must name what was claimed")
			}
		})
	}
}

func TestExemptionPreconditionsFailClosed(t *testing.T) {
	t.Parallel()

	sca := func() risk.Assessment {
		a := cleanAssessment()
		a.SCAJurisdiction = true
		return a
	}

	tests := []struct {
		name      string
		exemption risk.ExemptionType
		setup     func(*risk.Assessment)
	}{
		{
			// The exemption rests entirely on there having been a prior SCA, and "the merchant
			// told us it did" is not evidence.
			name:      "MIT without a network reference is unclaimable",
			exemption: risk.ExemptionMIT,
			setup:     func(a *risk.Assessment) { a.MerchantInitiated = true },
		},
		{
			name:      "TRA with a stale acquirer band is unclaimable",
			exemption: risk.ExemptionTRA,
			setup: func(a *risk.Assessment) {
				a.TRABandCeiling = usd(10_000)
				a.TRABandStale = true
				a.TRAScoreCeiling = 30
				a.ExternalScore = risk.KnownScore(12)
			},
		},
		{
			// TRA is defined as a claim about the risk score; a claim about a number you did not
			// compute is one you cannot defend.
			name:      "TRA without a score is unclaimable",
			exemption: risk.ExemptionTRA,
			setup: func(a *risk.Assessment) {
				a.TRABandCeiling = usd(10_000)
				a.TRAScoreCeiling = 30
			},
		},
		{
			name:      "TRA above the acquirer's band ceiling is unclaimable",
			exemption: risk.ExemptionTRA,
			setup: func(a *risk.Assessment) {
				a.TRABandCeiling = usd(1_000)
				a.TRAScoreCeiling = 30
				a.ExternalScore = risk.KnownScore(12)
			},
		},
		{
			name:      "TRA with a score at the ceiling is unclaimable",
			exemption: risk.ExemptionTRA,
			setup: func(a *risk.Assessment) {
				a.TRABandCeiling = usd(10_000)
				a.TRAScoreCeiling = 30
				a.ExternalScore = risk.KnownScore(30)
			},
		},
		{
			name:      "a low-value payment above the corridor ceiling is unclaimable",
			exemption: risk.ExemptionLowValue,
			setup: func(a *risk.Assessment) {
				a.LowValueCeiling = usd(1_000)
				a.LowValueCumulativeCeiling = usd(10_000)
			},
		},
		{
			name:      "the fifth consecutive low-value exemption exhausts the counter",
			exemption: risk.ExemptionLowValue,
			setup: func(a *risk.Assessment) {
				a.LowValueCeiling = usd(10_000)
				a.LowValueCumulativeCeiling = usd(100_000)
				a.History.ConsecutiveExemptSinceLastSCA = 5
			},
		},
		{
			name:      "exceeding the cumulative ceiling exhausts the exemption",
			exemption: risk.ExemptionLowValue,
			setup: func(a *risk.Assessment) {
				a.LowValueCeiling = usd(10_000)
				a.LowValueCumulativeCeiling = usd(10_000)
				a.History.CumulativeExemptAmount = usd(9_000)
			},
		},
		{
			name:      "a low-value ceiling in another currency cannot be compared and is unclaimable",
			exemption: risk.ExemptionLowValue,
			setup: func(a *risk.Assessment) {
				a.LowValueCeiling = eur(10_000)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := sca()
			tt.setup(&a)
			if tt.exemption.Applies(a) {
				t.Fatalf("%s must not be claimable here", tt.exemption)
			}
			d := risk.Evaluate(validPolicy(), a)
			if !d.Require3DS {
				t.Fatal("with no claimable exemption the corridor requirement stands")
			}
			if d.ExemptionApplied != "" {
				t.Fatalf("nothing should have been claimed, got %s", d.ExemptionApplied)
			}
		})
	}
}

// The precedence runs from strongest liability position to weakest, and claiming the wrong one
// moves who pays for the chargeback.
func TestExemptionPrecedence(t *testing.T) {
	t.Parallel()

	a := cleanAssessment()
	a.SCAJurisdiction = true
	// Everything is claimable at once.
	a.MerchantInitiated = true
	a.NetworkTransactionRef = "MCC001"
	a.CorporateCard = true
	a.History.TrustedBeneficiary = true
	a.TRABandCeiling = usd(10_000)
	a.TRAScoreCeiling = 30
	a.ExternalScore = risk.KnownScore(12)
	a.LowValueCeiling = usd(10_000)
	a.LowValueCumulativeCeiling = usd(100_000)

	got, ok := risk.ClaimableExemption(a)
	if !ok || got != risk.ExemptionMIT {
		t.Fatalf("ClaimableExemption = %q, want MIT (the strongest liability position)", got)
	}

	// Removing each in precedence order should reveal the next.
	want := []risk.ExemptionType{
		risk.ExemptionMIT, risk.ExemptionCorporate, risk.ExemptionTrustedBeneficiary,
		risk.ExemptionTRA, risk.ExemptionLowValue,
	}
	for i, w := range want {
		got, ok := risk.ClaimableExemption(a)
		if !ok || got != w {
			t.Fatalf("step %d: got %q, want %s", i, got, w)
		}
		switch w {
		case risk.ExemptionMIT:
			a.NetworkTransactionRef = ""
		case risk.ExemptionCorporate:
			a.CorporateCard = false
		case risk.ExemptionTrustedBeneficiary:
			a.History.TrustedBeneficiary = false
		case risk.ExemptionTRA:
			a.TRABandStale = true
		case risk.ExemptionLowValue:
			a.LowValueCeiling = usd(1)
		}
	}
	if _, ok := risk.ClaimableExemption(a); ok {
		t.Fatal("nothing should be claimable once every precondition is removed")
	}
}

// An exemption is an argument about regulation, not an argument about the fraud signal that just
// fired. Getting this backwards produces an engine that applies the most friction to its safest
// traffic and none to its riskiest.
func TestAnExemptionCannotWaiveRiskDrivenAuthentication(t *testing.T) {
	// Verifies: NFR-40.
	t.Parallel()

	tests := []struct {
		name  string
		setup func(*risk.Assessment)
	}{
		{
			name: "a score above the 3DS threshold",
			setup: func(a *risk.Assessment) {
				a.ExternalScore = risk.KnownScore(60)
				a.TRAScoreCeiling = 0 // TRA not in play
			},
		},
		{
			name: "an unavailable velocity counter that fell back to REQUIRE_3DS",
			setup: func(a *risk.Assessment) {
				a.Counters.CardThisHour = risk.UnavailableCount()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := cleanAssessment()
			a.SCAJurisdiction = true
			a.CorporateCard = true // the strongest waiver available short of MIT
			tt.setup(&a)

			d := risk.Evaluate(validPolicy(), a)
			if !d.Require3DS {
				t.Fatal("a risk-driven authentication requirement must survive an exemption claim")
			}
		})
	}
}

// A method with no 3DS step cannot be made to have one.
func TestMethodsOutOfSCAScopeNeverRequireAuthentication(t *testing.T) {
	t.Parallel()

	for _, m := range []shared.PaymentMethod{shared.MethodSEPADebit, shared.MethodPayPal, shared.MethodUPI} {
		t.Run(string(m), func(t *testing.T) {
			t.Parallel()
			a := cleanAssessment()
			a.PaymentMethod = m
			a.SCAJurisdiction = true
			a.Amount = usd(500_000) // well above the merchant's 3DS threshold

			d := risk.Evaluate(validPolicy(), a)
			if d.Require3DS {
				t.Fatalf("%s has no 3DS step to force", m)
			}
		})
	}
}

// Require3DS and Outcome are not the same decision, and conflating them loses the ordinary case:
// an approved payment that nonetheless needs authentication.
func TestApproveWithAuthenticationIsRepresentable(t *testing.T) {
	// Verifies: FR-68.
	t.Parallel()

	a := cleanAssessment()
	a.Amount = usd(60_000)

	d := risk.Evaluate(validPolicy(), a)
	if !d.Require3DS {
		t.Fatal("the amount is above the merchant's threshold")
	}
	if !d.Approved() {
		t.Fatal("forcing authentication is not refusing the payment")
	}
	if d.Outcome != risk.OutcomeRequire3DS {
		t.Fatalf("outcome = %s", d.Outcome)
	}
}

// --- determinism and error rendering -----------------------------------------------------------

func TestEvaluateIsDeterministic(t *testing.T) {
	t.Parallel()

	p, a := validPolicy(), cleanAssessment()
	a.Counters.CardThisHour = risk.UnavailableCount()
	a.SCAJurisdiction = true

	first := risk.Evaluate(p, a)
	for i := 0; i < 32; i++ {
		got := risk.Evaluate(p, a)
		if got.Outcome != first.Outcome || got.Require3DS != first.Require3DS ||
			got.ExemptionApplied != first.ExemptionApplied || got.Degraded != first.Degraded {
			t.Fatalf("run %d diverged: %+v vs %+v", i, got, first)
		}
		if len(got.Reasons) != len(first.Reasons) {
			t.Fatalf("run %d produced %v, first produced %v", i, got.ReasonCodes(), first.ReasonCodes())
		}
		for j := range got.Reasons {
			if got.Reasons[j] != first.Reasons[j] {
				t.Fatalf("run %d reason %d = %+v, want %+v", i, j, got.Reasons[j], first.Reasons[j])
			}
		}
	}
}

func TestDecisionErrCarriesTheRuleIDs(t *testing.T) {
	t.Parallel()

	a := cleanAssessment()
	a.Counters.CardThisHour = risk.KnownCount(50)

	d := risk.Evaluate(validPolicy(), a)
	err := d.Err()
	if err == nil {
		t.Fatal("a declined decision must render an error")
	}
	if err.Code != apierror.CodeRiskDeclined {
		t.Fatalf("code = %s, want RISK_DECLINED", err.Code)
	}
	if len(err.Details) == 0 {
		t.Fatal("the error must carry the reasons")
	}
	found := false
	for _, det := range err.Details {
		if det.RuleID == string(risk.CheckVelocityPerCard) {
			found = true
			if det.Code != risk.CheckVelocityPerCard.MetricLabel() {
				t.Errorf("detail code = %q, want the metric label", det.Code)
			}
		}
	}
	if !found {
		t.Fatalf("expected the velocity rule ID on the error, got %+v", err.Details)
	}
}

func TestMerchantRiskRatingValidity(t *testing.T) {
	t.Parallel()

	for _, r := range []risk.MerchantRiskRating{
		risk.RatingLow, risk.RatingStandard, risk.RatingElevated, risk.RatingHigh,
	} {
		if !r.IsValid() {
			t.Errorf("%s should be valid", r)
		}
	}
	if risk.MerchantRiskRating("SPICY").IsValid() {
		t.Error("an unregistered rating must not validate")
	}
	for _, e := range risk.ExemptionPrecedence {
		if !e.IsValid() {
			t.Errorf("%s should be valid", e)
		}
	}
	if risk.ExemptionType("BECAUSE_I_SAID_SO").IsValid() {
		t.Error("an unregistered exemption must not validate")
	}
}

func TestCounterMarkers(t *testing.T) {
	t.Parallel()

	if c := risk.KnownCount(7); !c.IsAvailable() || c.Value() != 7 {
		t.Fatalf("KnownCount = %+v", c)
	}
	// Zero is a legitimate observation and must be distinguishable from "unavailable"; reading
	// an outage as "no payments in the window" is the safest possible reading and therefore the
	// most dangerous thing an outage could silently produce.
	if c := risk.KnownCount(0); !c.IsAvailable() {
		t.Fatal("an observed zero is an observation")
	}
	if c := risk.UnavailableCount(); c.IsAvailable() {
		t.Fatal("UnavailableCount must not report as available")
	}
	if v := risk.KnownVolume(usd(0)); !v.IsAvailable() {
		t.Fatal("an observed zero volume is an observation")
	}
	if v := risk.UnavailableVolume(); v.IsAvailable() {
		t.Fatal("UnavailableVolume must not report as available")
	}
	if s := risk.KnownScore(0); !s.IsAvailable() || s.Value() != 0 {
		t.Fatal("a score of zero is a score")
	}
	if s := risk.UnavailableScore(); s.IsAvailable() {
		t.Fatal("UnavailableScore must not report as available")
	}
}
