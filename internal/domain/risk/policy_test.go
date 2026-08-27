package risk_test

import (
	"errors"
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

func usd(minor int64) money.Money { return money.MustNew(minor, "USD") }
func eur(minor int64) money.Money { return money.MustNew(minor, "EUR") }

func ruleIDs(t *testing.T, err error) []string {
	t.Helper()
	var e *apierror.Error
	if !errors.As(err, &e) {
		t.Fatalf("expected an *apierror.Error, got %T (%v)", err, err)
	}
	out := make([]string, 0, len(e.Details))
	for _, d := range e.Details {
		out = append(out, d.RuleID)
	}
	return out
}

func hasRule(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// validPolicy is the baseline §23 risk block, in minor units of USD.
func validPolicy() risk.Policy {
	return risk.Policy{
		MaxTransactionAmount: usd(1_000_000),
		Require3DSAbove:      usd(50_000),
		DailyVolumeLimit:     usd(50_000_000),
		Velocity: risk.Velocity{
			MaxPaymentsPerMinute: 300,
			MaxPerCardPerHour:    5,
			MaxPerCustomerPerDay: 20,
		},
		BlockedCountries:          []shared.Country{"KP", "IR"},
		MaxCardsPerCustomerPerDay: 3,
		Version:                   7,
	}
}

func TestPolicyValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(*risk.Policy)
		wantOK   bool
		wantRule string
	}{
		{
			name:   "the documented baseline configuration validates",
			mutate: func(*risk.Policy) {},
			wantOK: true,
		},
		{
			name: "an empty allowlist means all countries and is legal",
			mutate: func(p *risk.Policy) {
				p.AllowedCountries = nil
			},
			wantOK: true,
		},
		{
			name: "omitted velocity limits leave the checks unenforced and are legal",
			mutate: func(p *risk.Policy) {
				p.Velocity = risk.Velocity{}
				p.MaxCardsPerCustomerPerDay = 0
			},
			wantOK: true,
		},
		{
			name:     "an unsupported limit currency is rejected",
			mutate:   func(p *risk.Policy) { p.MaxTransactionAmount = money.Money{} },
			wantRule: "L4.RISK_LIMIT_CURRENCY_SUPPORTED",
		},
		{
			name:     "limits in different currencies cannot be compared and are rejected",
			mutate:   func(p *risk.Policy) { p.Require3DSAbove = eur(50_000) },
			wantRule: "L4.RISK_CURRENCY_CONSISTENT",
		},
		{
			name:     "a zero transaction ceiling would decline every payment",
			mutate:   func(p *risk.Policy) { p.MaxTransactionAmount = usd(0) },
			wantRule: "L4.RISK_LIMIT_ORDERING",
		},
		{
			name:     "a 3DS threshold above the transaction ceiling can never fire",
			mutate:   func(p *risk.Policy) { p.Require3DSAbove = usd(2_000_000) },
			wantRule: "L4.THREEDS_THRESHOLD_BELOW_MAX_AMOUNT",
		},
		{
			name:     "a daily limit below the transaction limit is breached by one permitted payment",
			mutate:   func(p *risk.Policy) { p.DailyVolumeLimit = usd(500) },
			wantRule: "L4.DAILY_LIMIT_AT_LEAST_MAX_TRANSACTION",
		},
		{
			name:     "a negative velocity limit is rejected",
			mutate:   func(p *risk.Policy) { p.Velocity.MaxPerCardPerHour = -1 },
			wantRule: "L4.VELOCITY_LIMITS_POSITIVE",
		},
		{
			name:     "a negative distinct-card limit is rejected",
			mutate:   func(p *risk.Policy) { p.MaxCardsPerCustomerPerDay = -4 },
			wantRule: "L4.VELOCITY_LIMITS_POSITIVE",
		},
		{
			name:     "an unknown blocked country is rejected",
			mutate:   func(p *risk.Policy) { p.BlockedCountries = []shared.Country{"ZZ"} },
			wantRule: "L4.BLOCKED_COUNTRIES_VALID",
		},
		{
			name:     "an unknown allowed country is rejected",
			mutate:   func(p *risk.Policy) { p.AllowedCountries = []shared.Country{"QQ"} },
			wantRule: "L4.COUNTRIES_ARE_ISO3166",
		},
		{
			name: "a country in both lists is misleading because the block always wins",
			mutate: func(p *risk.Policy) {
				p.AllowedCountries = []shared.Country{"US", "KP"}
			},
			wantRule: "L4.BLOCKED_COUNTRIES_DISJOINT",
		},
		{
			name: "an unknown failure posture is rejected",
			mutate: func(p *risk.Policy) {
				p.Postures = map[risk.CheckID]risk.FailurePosture{
					risk.CheckVelocityPerCard: "SHRUG",
				}
			},
			wantRule: "L4.RISK_FAILURE_POSTURE_KNOWN",
		},
		{
			name: "a posture on a check that cannot be unavailable describes a mode that does not exist",
			mutate: func(p *risk.Policy) {
				p.Postures = map[risk.CheckID]risk.FailurePosture{
					risk.CheckSanctionedCountry: risk.PostureRequire3DS,
				}
			},
			wantRule: "L4.RISK_FAILURE_POSTURE_KNOWN",
		},
		{
			name: "score thresholds out of order shadow one another",
			mutate: func(p *risk.Policy) {
				p.DeclineScoreAtOrAbove = 40
				p.ReviewScoreAtOrAbove = 70
				p.ThreeDSScoreAtOrAbove = 20
			},
			wantRule: "L4.RISK_SCORE_THRESHOLDS_ORDERED",
		},
		{
			name: "a score threshold outside 0..100 is rejected",
			mutate: func(p *risk.Policy) {
				p.DeclineScoreAtOrAbove = 140
				p.ReviewScoreAtOrAbove = 75
				p.ThreeDSScoreAtOrAbove = 50
			},
			wantRule: "L4.RISK_SCORE_THRESHOLDS_ORDERED",
		},
		{
			name: "correctly ordered thresholds validate",
			mutate: func(p *risk.Policy) {
				p.DeclineScoreAtOrAbove = 95
				p.ReviewScoreAtOrAbove = 80
				p.ThreeDSScoreAtOrAbove = 45
			},
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := validPolicy()
			tt.mutate(&p)
			err := p.Validate()
			if tt.wantOK {
				if err != nil {
					t.Fatalf("expected a valid policy, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			if code := apierror.CodeOf(err); code != apierror.CodeConfigurationInvalid {
				t.Fatalf("expected CONFIGURATION_INVALID, got %s", code)
			}
			if got := ruleIDs(t, err); !hasRule(got, tt.wantRule) {
				t.Fatalf("expected rule %s, got %v", tt.wantRule, got)
			}
		})
	}
}

func TestPolicyValidateReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()

	p := validPolicy()
	p.Require3DSAbove = usd(2_000_000)
	p.DailyVolumeLimit = usd(100)
	p.Velocity.MaxPaymentsPerMinute = -3
	p.BlockedCountries = []shared.Country{"ZZ"}

	err := p.Validate()
	if err == nil {
		t.Fatal("expected a validation error")
	}
	got := ruleIDs(t, err)
	for _, want := range []string{
		"L4.THREEDS_THRESHOLD_BELOW_MAX_AMOUNT",
		"L4.DAILY_LIMIT_AT_LEAST_MAX_TRANSACTION",
		"L4.VELOCITY_LIMITS_POSITIVE",
		"L4.BLOCKED_COUNTRIES_VALID",
	} {
		if !hasRule(got, want) {
			t.Errorf("expected rule %s in %v", want, got)
		}
	}
}

func TestPostureForFallsBackToTheDocumentedDefaults(t *testing.T) {
	t.Parallel()

	p := validPolicy()

	tests := []struct {
		name  string
		check risk.CheckID
		want  risk.FailurePosture
	}{
		{
			// §6.2: losing fraud sensitivity for minutes is survivable, so a lost counter
			// becomes friction rather than a decline or a free pass.
			name:  "count velocity defaults to forced authentication",
			check: risk.CheckVelocityPerCard, want: risk.PostureRequire3DS,
		},
		{
			// §6.2: exceeding a contractual volume limit is not survivable, and this check has
			// a database fallback before it gets here.
			name:  "money velocity defaults to failing closed",
			check: risk.CheckDailyVolume, want: risk.PostureFailClosed,
		},
		{
			name:  "the external scorer never defaults to approve",
			check: risk.CheckRiskScore, want: risk.PostureRequire3DS,
		},
		{
			name:  "a check with no documented default still never returns the zero posture",
			check: risk.CheckSCAExemption, want: risk.PostureRequire3DS,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := p.PostureFor(tt.check); got != tt.want {
				t.Fatalf("PostureFor(%s) = %s, want %s", tt.check, got, tt.want)
			}
		})
	}

	t.Run("a configured posture overrides the default", func(t *testing.T) {
		t.Parallel()
		q := validPolicy()
		q.Postures = map[risk.CheckID]risk.FailurePosture{
			risk.CheckVelocityPerCard: risk.PostureFailClosed,
		}
		if got := q.PostureFor(risk.CheckVelocityPerCard); got != risk.PostureFailClosed {
			t.Fatalf("got %s", got)
		}
		if got := q.PostureFor(risk.CheckDailyVolume); got != risk.PostureFailClosed {
			t.Fatalf("unconfigured checks must keep their default, got %s", got)
		}
	})

	t.Run("an invalid configured posture falls back rather than returning the empty string", func(t *testing.T) {
		t.Parallel()
		q := validPolicy()
		q.Postures = map[risk.CheckID]risk.FailurePosture{risk.CheckVelocityPerCard: "SHRUG"}
		if got := q.PostureFor(risk.CheckVelocityPerCard); got != risk.PostureRequire3DS {
			t.Fatalf("got %s", got)
		}
	})
}

func TestPostureOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		posture risk.FailurePosture
		want    risk.Outcome
	}{
		{risk.PostureFailClosed, risk.OutcomeDecline},
		{risk.PostureReview, risk.OutcomeReview},
		{risk.PostureRequire3DS, risk.OutcomeRequire3DS},
		{risk.PostureFailOpen, risk.OutcomeApprove},
	}
	for _, tt := range tests {
		t.Run(string(tt.posture), func(t *testing.T) {
			t.Parallel()
			if got := tt.posture.Outcome(); got != tt.want {
				t.Fatalf("Outcome = %s, want %s", got, tt.want)
			}
			if !tt.posture.IsValid() {
				t.Fatal("posture should be valid")
			}
		})
	}
	if risk.FailurePosture("MAYBE").IsValid() {
		t.Error("an unregistered posture must not validate")
	}
}

func TestThresholdDefaults(t *testing.T) {
	t.Parallel()

	t.Run("all three unset means the platform defaults", func(t *testing.T) {
		t.Parallel()
		d, r, s := risk.Policy{}.Thresholds()
		if d != risk.DefaultDeclineScore || r != risk.DefaultReviewScore || s != risk.DefaultThreeDSScore {
			t.Fatalf("got %d/%d/%d", d, r, s)
		}
	})

	t.Run("setting one keeps the platform defaults for the rest", func(t *testing.T) {
		t.Parallel()
		// Reading the unset review threshold as zero would send every payment to manual review.
		d, r, s := risk.Policy{DeclineScoreAtOrAbove: 95}.Thresholds()
		if d != 95 || r != risk.DefaultReviewScore || s != risk.DefaultThreeDSScore {
			t.Fatalf("got %d/%d/%d", d, r, s)
		}
	})
}

func TestCountryPredicates(t *testing.T) {
	t.Parallel()

	p := validPolicy()
	p.AllowedCountries = []shared.Country{"US", "DE"}

	tests := []struct {
		country        shared.Country
		blocked, allow bool
	}{
		{"US", false, true},
		{"DE", false, true},
		{"FR", false, false},
		{"KP", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.country.String(), func(t *testing.T) {
			t.Parallel()
			if got := p.IsCountryBlocked(tt.country); got != tt.blocked {
				t.Errorf("IsCountryBlocked = %v, want %v", got, tt.blocked)
			}
			if got := p.IsCountryAllowed(tt.country); got != tt.allow {
				t.Errorf("IsCountryAllowed = %v, want %v", got, tt.allow)
			}
		})
	}

	t.Run("an empty allowlist permits everything", func(t *testing.T) {
		t.Parallel()
		q := validPolicy()
		if !q.IsCountryAllowed("VN") {
			t.Fatal("an empty allowlist must not block")
		}
	})
}

func TestCheckIDMetricLabel(t *testing.T) {
	t.Parallel()

	// The validation level is an artifact of where the rule is enforced, not of what it is; a
	// check that moves between levels must not silently become a different time series.
	if got := risk.CheckVelocityPerCard.MetricLabel(); got != "velocity_per_card_per_hour" {
		t.Fatalf("MetricLabel = %q", got)
	}
	for _, c := range risk.AllCheckIDs {
		if !c.IsValid() {
			t.Errorf("%s is in AllCheckIDs but does not validate", c)
		}
		if c.MetricLabel() == "" {
			t.Errorf("%s has an empty metric label", c)
		}
	}
	if risk.CheckID("L5.MADE_UP").IsValid() {
		t.Error("an unregistered check must not validate")
	}
}

func TestOutcomeEscalation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a, b, want risk.Outcome
	}{
		{risk.OutcomeApprove, risk.OutcomeRequire3DS, risk.OutcomeRequire3DS},
		{risk.OutcomeRequire3DS, risk.OutcomeApprove, risk.OutcomeRequire3DS},
		{risk.OutcomeReview, risk.OutcomeRequire3DS, risk.OutcomeReview},
		{risk.OutcomeDecline, risk.OutcomeApprove, risk.OutcomeDecline},
		{risk.OutcomeApprove, risk.OutcomeDecline, risk.OutcomeDecline},
	}
	for _, tt := range tests {
		if got := risk.Escalate(tt.a, tt.b); got != tt.want {
			t.Errorf("Escalate(%s, %s) = %s, want %s", tt.a, tt.b, got, tt.want)
		}
	}
	if !risk.OutcomeDecline.IsTerminal() {
		t.Error("DECLINE is terminal")
	}
	for _, o := range []risk.Outcome{risk.OutcomeApprove, risk.OutcomeRequire3DS, risk.OutcomeReview} {
		if o.IsTerminal() {
			t.Errorf("%s must not be terminal", o)
		}
	}
	if risk.Outcome("MAYBE").IsValid() {
		t.Error("an unregistered outcome must not validate")
	}
}
