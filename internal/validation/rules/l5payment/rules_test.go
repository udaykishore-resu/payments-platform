package l5payment_test

import (
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/internal/ruletest"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/l5payment"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

var now = time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)

func eur(minor int64) money.Money { return money.MustNew(minor, "EUR") }

const expectedScope = "ten_live_1|mrc_1|CARD|POST /v1/payments"

func deps() l5payment.Deps {
	d := l5payment.DefaultDeps()
	d.MethodMinimums = map[shared.PaymentMethod]money.Money{shared.MethodCard: eur(50)}
	d.SanctionedCountries = []shared.Country{"IR", "KP"}
	d.RefundElevatedThreshold = eur(1_000_00)
	d.LowValueCeiling = eur(30_00)
	return d
}

// base is a clean card payment creation for an active merchant on a fresh configuration
// snapshot: within every limit, under every velocity counter, on an unblocked country, with a
// token that belongs to the merchant and has not expired.
func base() l5payment.Subject {
	return l5payment.Subject{
		Op: l5payment.OpCreate,
		Request: l5payment.Request{
			Amount:              eur(100_00),
			Method:              shared.MethodCard,
			MinorDigitsSupplied: 2,
			CustomerCountry:     "DE",
			IPCountry:           "DE",
			TokenRef:            "tok_live_1",
			Token: l5payment.TokenMeta{
				Present:         true,
				OwnerMerchantID: "mrc_1",
				ExpiresAt:       now.AddDate(1, 0, 0),
				CardExpiryMonth: 12,
				CardExpiryYear:  2030,
				Fingerprint:     "fp_1",
			},
			CaptureMode:         payment.CaptureAutomatic,
			StatementDescriptor: "ACME STORE",
			Metadata:            map[string]string{"orderId": "A-1"},
			CustomerRef:         "cus_1",
			IdempotencyKey:      "0f2b6c1e-1c3d-4f0a-9a1b-2c3d4e5f6071",
		},
		Merchant: l5payment.MerchantSnapshot{
			Found: true, ID: "mrc_1", Status: merchant.StatusActive,
		},
		Config: l5payment.ConfigSnapshot{
			Present:              true,
			Version:              7,
			Age:                  30 * time.Second,
			Currencies:           []money.Currency{"EUR", "GBP"},
			Methods:              []shared.PaymentMethod{shared.MethodCard},
			Countries:            []shared.Country{"DE", "GB"},
			BlockedCountries:     []shared.Country{"IR"},
			MaxTransactionAmount: eur(5_000_00),
			Require3DSAbove:      eur(500_00),
			DailyVolumeLimit:     eur(50_000_00),
			MaxRefundWindowDays:  120,
			MaxPartialCaptures:   3,
			MaxPaymentsPerMinute: 100,
			MaxPerCardPerHour:    5,
			MaxPerCustomerPerDay: 20,
			MaxDistinctCards:     3,
			ManualCaptureAllowed: true,
			Candidates: []l5payment.RouteCandidate{
				{Method: shared.MethodCard, Currency: "EUR", Country: "DE"},
				{Method: shared.MethodCard, Currency: "EUR", Country: "GB"},
				{Method: shared.MethodCard, Currency: "GBP", Country: "GB"},
			},
			MerchantBlocklistConfigured: true,
			RiskDeclineAt:               90,
		},
		Velocity: l5payment.VelocityCounters{
			CountLastMinute:              3,
			TodayVolume:                  eur(1_000_00),
			CountForFingerprintLastHour:  1,
			CountForCustomerToday:        2,
			DistinctFingerprintsLastHour: 1,
			AttemptsLast15Min:            5,
			DeclinesLast15Min:            1,
		},
		Risk: l5payment.RiskInputs{Scored: true, Score: 10},
		Principal: l5payment.PrincipalView{
			ID: "api_client_1",
			Scopes: []string{
				"payments:write", "payments:capture", "payments:refund", "payments:void",
			},
		},
		ExpectedScope:      expectedScope,
		RequestFingerprint: "sha256:9f86d081884c7d659a2feaa0c55ad015",
		Now:                now,
	}
}

// authorized returns a payment view for a payment that has been authorized and not captured.
func authorized() l5payment.PaymentView {
	return l5payment.PaymentView{
		Found:            true,
		State:            payment.StateAuthorized,
		Currency:         "EUR",
		AuthorizedAmount: eur(100_00),
		CapturedTotal:    eur(0),
		RefundedTotal:    eur(0),
		AuthorizedAt:     now.AddDate(0, 0, -1),
	}
}

// captured returns a payment view for a fully captured, refundable payment.
func captured() l5payment.PaymentView {
	return l5payment.PaymentView{
		Found:            true,
		State:            payment.StateCaptured,
		Currency:         "EUR",
		AuthorizedAmount: eur(100_00),
		CapturedTotal:    eur(100_00),
		RefundedTotal:    eur(0),
		AuthorizedAt:     now.AddDate(0, 0, -2),
		CapturedAt:       now.AddDate(0, 0, -1),
	}
}

func asCapture(s *l5payment.Subject, p l5payment.PaymentView) {
	s.Op = l5payment.OpCapture
	s.Payment = &p
}

func asRefund(s *l5payment.Subject, p l5payment.PaymentView) {
	s.Op = l5payment.OpRefund
	s.Payment = &p
}

func asVoid(s *l5payment.Subject, p l5payment.PaymentView) {
	s.Op = l5payment.OpVoid
	s.Payment = &p
}

func TestL5Rules(t *testing.T) {
	// Verifies: BR-14.
	t.Parallel()
	set := l5payment.Rules(deps())

	ruletest.Run(t, set, base, []ruletest.Case[l5payment.Subject]{
		{
			ID:   "L5.MERCHANT_EXISTS",
			Pass: func(s *l5payment.Subject) {},
			Fail: func(s *l5payment.Subject) { s.Merchant.Found = false },
		},
		{
			ID:   "L5.MERCHANT_IS_ACTIVE",
			Pass: func(s *l5payment.Subject) {},
			Fail: func(s *l5payment.Subject) { s.Merchant.Status = merchant.StatusProductionReady },
		},
		{
			ID: "L5.SUSPENDED_PERMITS_MONEY_OUT",
			Pass: func(s *l5payment.Subject) {
				s.Merchant.Status = merchant.StatusSuspended
				asRefund(s, captured())
			},
			Fail: func(s *l5payment.Subject) { s.Merchant.Status = merchant.StatusSuspended },
		},
		{
			ID:   "L5.CONFIG_SNAPSHOT_FRESH_ENOUGH",
			Pass: func(s *l5payment.Subject) { s.Config.Age = 15 * time.Minute },
			Fail: func(s *l5payment.Subject) { s.Config.Age = 16 * time.Minute },
		},
		{
			ID:   "L5.AMOUNT_IS_POSITIVE",
			Pass: func(s *l5payment.Subject) { s.Request.Amount = eur(1) },
			Fail: func(s *l5payment.Subject) { s.Request.Amount = money.Zero("EUR") },
		},
		{
			ID:   "L5.AMOUNT_RESPECTS_CURRENCY_EXPONENT",
			Pass: func(s *l5payment.Subject) {},
			Fail: func(s *l5payment.Subject) { s.Request.MinorDigitsSupplied = 3 },
		},
		{
			ID:   "L5.AMOUNT_WITHIN_MERCHANT_LIMIT",
			Pass: func(s *l5payment.Subject) { s.Request.Amount = eur(5_000_00) },
			Fail: func(s *l5payment.Subject) { s.Request.Amount = eur(5_000_01) },
		},
		{
			ID:   "L5.AMOUNT_ABOVE_METHOD_MINIMUM",
			Pass: func(s *l5payment.Subject) { s.Request.Amount = eur(50) },
			Fail: func(s *l5payment.Subject) { s.Request.Amount = eur(49) },
		},
		{
			ID:   "L5.CURRENCY_IS_ENABLED",
			Pass: func(s *l5payment.Subject) { s.Request.Amount = money.MustNew(100_00, "GBP") },
			Fail: func(s *l5payment.Subject) { s.Request.Amount = money.MustNew(100_00, "USD") },
		},
		{
			ID:   "L5.PAYMENT_METHOD_IS_ENABLED",
			Pass: func(s *l5payment.Subject) {},
			Fail: func(s *l5payment.Subject) { s.Request.Method = shared.MethodSEPADebit },
		},
		{
			ID:   "L5.METHOD_CURRENCY_PAIR_ROUTABLE",
			Pass: func(s *l5payment.Subject) {},
			Fail: func(s *l5payment.Subject) { s.Config.Candidates = nil },
		},
		{
			ID:   "L5.CUSTOMER_COUNTRY_IN_SUPPORTED_SET",
			Pass: func(s *l5payment.Subject) { s.Request.CustomerCountry = "GB" },
			Fail: func(s *l5payment.Subject) { s.Request.CustomerCountry = "FR" },
		},
		{
			ID:   "L5.CUSTOMER_COUNTRY_NOT_BLOCKED",
			Pass: func(s *l5payment.Subject) {},
			Fail: func(s *l5payment.Subject) {
				s.Config.BlockedCountries = []shared.Country{"IR", "DE"}
			},
		},
		{
			ID:   "L5.IP_COUNTRY_NOT_SANCTIONED",
			Pass: func(s *l5payment.Subject) {},
			Fail: func(s *l5payment.Subject) { s.Request.IPCountry = "IR" },
		},
		{
			ID:   "L5.TOKEN_REFERENCE_PRESENT",
			Pass: func(s *l5payment.Subject) {},
			Fail: func(s *l5payment.Subject) {
				s.Request.TokenRef = ""
				s.Request.Token = l5payment.TokenMeta{}
			},
		},
		{
			ID:   "L5.TOKEN_BELONGS_TO_MERCHANT",
			Pass: func(s *l5payment.Subject) {},
			Fail: func(s *l5payment.Subject) { s.Request.Token.OwnerMerchantID = "mrc_someone_else" },
		},
		{
			ID:   "L5.TOKEN_NOT_EXPIRED",
			Pass: func(s *l5payment.Subject) {},
			Fail: func(s *l5payment.Subject) {
				s.Request.Token.CardExpiryMonth = 2
				s.Request.Token.CardExpiryYear = 2026
			},
		},
		{
			ID:   "L5.CAPTURE_MODE_IS_SUPPORTED",
			Pass: func(s *l5payment.Subject) { s.Request.CaptureMode = payment.CaptureManual },
			Fail: func(s *l5payment.Subject) {
				s.Request.CaptureMode = payment.CaptureManual
				s.Config.ManualCaptureAllowed = false
			},
		},
		{
			ID: "L5.IDEMPOTENCY_SCOPE_MATCHES",
			Pass: func(s *l5payment.Subject) {
				s.Idempotency = l5payment.IdempotencyRecord{Exists: true, Scope: expectedScope}
			},
			Fail: func(s *l5payment.Subject) {
				s.Idempotency = l5payment.IdempotencyRecord{
					Exists: true, Scope: "ten_live_1|mrc_1|CARD|POST /v1/refunds",
				}
			},
		},
		{
			ID: "L5.IDEMPOTENCY_FINGERPRINT_MATCHES",
			Pass: func(s *l5payment.Subject) {
				s.Idempotency = l5payment.IdempotencyRecord{
					Exists: true, Scope: expectedScope, Fingerprint: s.RequestFingerprint,
				}
			},
			Fail: func(s *l5payment.Subject) {
				s.Idempotency = l5payment.IdempotencyRecord{
					Exists: true, Scope: expectedScope, Fingerprint: "sha256:something-else",
				}
			},
		},
		{
			ID: "L5.NO_INFLIGHT_DUPLICATE",
			Pass: func(s *l5payment.Subject) {
				s.Idempotency = l5payment.IdempotencyRecord{
					Exists: true, Scope: expectedScope, InFlight: true,
					LeaseExpiresAt: s.Now.Add(-time.Second),
				}
			},
			Fail: func(s *l5payment.Subject) {
				s.Idempotency = l5payment.IdempotencyRecord{
					Exists: true, Scope: expectedScope, InFlight: true,
					LeaseExpiresAt: s.Now.Add(20 * time.Second), RetryAfterSeconds: 20,
				}
			},
		},
		{
			ID:   "L5.OPERATION_SCOPE_AUTHORIZED",
			Pass: func(s *l5payment.Subject) {},
			Fail: func(s *l5payment.Subject) { s.Principal.Scopes = []string{"payments:read"} },
		},
		{
			ID: "L5.REFUND_REQUIRES_ELEVATED_ROLE_ABOVE_THRESHOLD",
			Pass: func(s *l5payment.Subject) {
				asRefund(s, captured())
				s.Request.Amount = eur(2_000_00)
				s.Principal.Scopes = append(s.Principal.Scopes, "payments:refund:elevated")
			},
			Fail: func(s *l5payment.Subject) {
				asRefund(s, captured())
				s.Request.Amount = eur(2_000_00)
			},
		},
		{
			ID:   "L5.DAILY_VOLUME_WITHIN_LIMIT",
			Pass: func(s *l5payment.Subject) { s.Velocity.TodayVolume = eur(49_900_00) },
			Fail: func(s *l5payment.Subject) { s.Velocity.TodayVolume = eur(49_900_01) },
		},
		{
			ID:   "L5.VELOCITY_PAYMENTS_PER_MINUTE",
			Pass: func(s *l5payment.Subject) { s.Velocity.CountLastMinute = 99 },
			Fail: func(s *l5payment.Subject) { s.Velocity.CountLastMinute = 100 },
		},
		{
			ID:   "L5.VELOCITY_PER_CARD_PER_HOUR",
			Pass: func(s *l5payment.Subject) { s.Velocity.CountForFingerprintLastHour = 4 },
			Fail: func(s *l5payment.Subject) { s.Velocity.CountForFingerprintLastHour = 5 },
		},
		{
			ID:   "L5.VELOCITY_PER_CUSTOMER_PER_DAY",
			Pass: func(s *l5payment.Subject) { s.Velocity.CountForCustomerToday = 19 },
			Fail: func(s *l5payment.Subject) { s.Velocity.CountForCustomerToday = 20 },
		},
		{
			ID:   "L5.VELOCITY_DISTINCT_CARDS_PER_CUSTOMER",
			Pass: func(s *l5payment.Subject) { s.Velocity.DistinctFingerprintsLastHour = 3 },
			Fail: func(s *l5payment.Subject) { s.Velocity.DistinctFingerprintsLastHour = 4 },
		},
		{
			ID: "L5.VELOCITY_DECLINE_RATIO",
			Pass: func(s *l5payment.Subject) {
				s.Velocity.AttemptsLast15Min = 20
				s.Velocity.DeclinesLast15Min = 12
			},
			Fail: func(s *l5payment.Subject) {
				s.Velocity.AttemptsLast15Min = 20
				s.Velocity.DeclinesLast15Min = 13
			},
		},
		{
			ID:   "L5.NOT_ON_MERCHANT_BLOCKLIST",
			Pass: func(s *l5payment.Subject) {},
			Fail: func(s *l5payment.Subject) { s.Risk.OnMerchantBlocklist = true },
		},
		{
			ID:   "L5.NOT_ON_PLATFORM_BLOCKLIST",
			Pass: func(s *l5payment.Subject) {},
			Fail: func(s *l5payment.Subject) { s.Risk.OnPlatformBlocklist = true },
		},
		{
			ID:   "L5.RISK_SCORE_BELOW_DECLINE_THRESHOLD",
			Pass: func(s *l5payment.Subject) { s.Risk.Score = 89 },
			Fail: func(s *l5payment.Subject) { s.Risk.Score = 90 },
		},
		{
			ID:   "L5.THREE_DS_REQUIRED_ABOVE_THRESHOLD",
			Pass: func(s *l5payment.Subject) { s.Request.Amount = eur(500_00) },
			Fail: func(s *l5payment.Subject) { s.Request.Amount = eur(500_01) },
		},
		{
			ID: "L5.SCA_EXEMPTION_IS_CLAIMABLE",
			Pass: func(s *l5payment.Subject) {
				s.Request.ClaimedSCAExemption = "TRA"
				s.Request.SCAExemptionPreconditionsHold = true
			},
			Fail: func(s *l5payment.Subject) {
				s.Request.ClaimedSCAExemption = "LOW_VALUE"
				s.Request.SCAExemptionPreconditionsHold = true
				s.Request.Amount = eur(30_01)
			},
		},
		{
			ID: "L5.MIT_HAS_INITIAL_REFERENCE",
			Pass: func(s *l5payment.Subject) {
				s.Request.MerchantInitiated = true
				s.Request.InitialTransactionID = "ntw_ref_1"
			},
			Fail: func(s *l5payment.Subject) { s.Request.MerchantInitiated = true },
		},
		{
			ID: "L5.RECURRING_HAS_MANDATE",
			Pass: func(s *l5payment.Subject) {
				s.Request.Recurring = true
				s.Request.MandateRef = "mnd_1"
				s.Request.MandateActive = true
			},
			Fail: func(s *l5payment.Subject) { s.Request.Recurring = true },
		},
		{
			ID:   "L5.PAYMENT_EXISTS",
			Pass: func(s *l5payment.Subject) { asCapture(s, authorized()) },
			Fail: func(s *l5payment.Subject) { s.Op = l5payment.OpCapture },
		},
		{
			ID:   "L5.PAYMENT_STATE_PERMITS_OPERATION",
			Pass: func(s *l5payment.Subject) { asCapture(s, authorized()) },
			Fail: func(s *l5payment.Subject) { asCapture(s, captured()) },
		},
		{
			ID: "L5.CAPTURE_AMOUNT_WITHIN_AUTHORIZED",
			Pass: func(s *l5payment.Subject) {
				asCapture(s, authorized())
				s.Request.Amount = eur(100_00)
			},
			Fail: func(s *l5payment.Subject) {
				asCapture(s, authorized())
				s.Request.Amount = eur(100_01)
			},
		},
		{
			ID: "L5.CAPTURE_COUNT_WITHIN_MAX_PARTIALS",
			Pass: func(s *l5payment.Subject) {
				p := authorized()
				p.CaptureCount = 2
				asCapture(s, p)
			},
			Fail: func(s *l5payment.Subject) {
				p := authorized()
				p.CaptureCount = 3
				asCapture(s, p)
			},
		},
		{
			ID: "L5.CAPTURE_WITHIN_AUTH_VALIDITY",
			Pass: func(s *l5payment.Subject) {
				p := authorized()
				p.AuthorizedAt = s.Now.AddDate(0, 0, -6)
				asCapture(s, p)
			},
			Fail: func(s *l5payment.Subject) {
				p := authorized()
				p.AuthorizedAt = s.Now.AddDate(0, 0, -8)
				asCapture(s, p)
			},
		},
		{
			ID: "L5.REFUND_AMOUNT_WITHIN_CAPTURED",
			Pass: func(s *l5payment.Subject) {
				asRefund(s, captured())
				s.Request.Amount = eur(100_00)
			},
			Fail: func(s *l5payment.Subject) {
				asRefund(s, captured())
				s.Request.Amount = eur(100_01)
			},
		},
		{
			ID:   "L5.REFUND_CURRENCY_MATCHES_PAYMENT",
			Pass: func(s *l5payment.Subject) { asRefund(s, captured()) },
			Fail: func(s *l5payment.Subject) {
				asRefund(s, captured())
				s.Request.Amount = money.MustNew(100_00, "GBP")
			},
		},
		{
			ID: "L5.REFUND_WITHIN_WINDOW",
			Pass: func(s *l5payment.Subject) {
				p := captured()
				p.CapturedAt = s.Now.AddDate(0, 0, -119)
				asRefund(s, p)
			},
			Fail: func(s *l5payment.Subject) {
				p := captured()
				p.CapturedAt = s.Now.AddDate(0, 0, -121)
				asRefund(s, p)
			},
		},
		{
			ID:   "L5.VOID_ONLY_WHEN_UNCAPTURED",
			Pass: func(s *l5payment.Subject) { asVoid(s, authorized()) },
			Fail: func(s *l5payment.Subject) {
				p := authorized()
				p.CapturedTotal = eur(50_00)
				asVoid(s, p)
			},
		},
		{
			ID:   "L5.NO_OPEN_DISPUTE_BLOCKS_REFUND",
			Pass: func(s *l5payment.Subject) { asRefund(s, captured()) },
			Fail: func(s *l5payment.Subject) {
				p := captured()
				p.HasOpenDispute = true
				asRefund(s, p)
			},
		},
		{
			ID:   "L5.STATEMENT_DESCRIPTOR_WELL_FORMED",
			Pass: func(s *l5payment.Subject) {},
			Fail: func(s *l5payment.Subject) { s.Request.StatementDescriptor = "AC*" },
		},
		{
			ID:   "L5.METADATA_LOOKS_NON_PII",
			Pass: func(s *l5payment.Subject) {},
			Fail: func(s *l5payment.Subject) {
				s.Request.Metadata["contact"] = "jane.doe@example.com"
			},
		},
	})
}

// TestL5CleanPaymentIsAccepted anchors the base subject: every case above measures a single
// mutation away from a payment the platform is happy to dispatch.
func TestL5CleanPaymentIsAccepted(t *testing.T) {
	t.Parallel()
	rep := l5payment.Rules(deps()).Evaluate(t.Context(), base())
	if !rep.OK() {
		t.Fatalf("the reference payment was rejected: %v", rep.Errors())
	}
	if got := len(rep.Failures()); got != 0 {
		t.Fatalf("the reference payment produced %d warnings: %v", got, rep.Failures())
	}
}

// TestL5SuspendedMerchantCanStillRefund is the asymmetry the platform depends on: a suspension
// stops a merchant taking money, not returning it. Blocking refunds during a suspension turns a
// merchant problem into a consumer-harm problem.
func TestL5SuspendedMerchantCanStillRefund(t *testing.T) {
	t.Parallel()
	set := l5payment.Rules(deps())

	create := base()
	create.Merchant.Status = merchant.StatusSuspended
	if set.Evaluate(t.Context(), create).OK() {
		t.Fatal("a suspended merchant was allowed to take a new payment")
	}

	refund := base()
	refund.Merchant.Status = merchant.StatusSuspended
	asRefund(&refund, captured())
	rep := set.Evaluate(t.Context(), refund)
	if out, ran := rep.For("L5.SUSPENDED_PERMITS_MONEY_OUT"); !ran || !out.Passed {
		t.Fatalf("a suspended merchant was blocked from refunding: %v", out)
	}
}

// TestL5ReportCarriesEveryFailingRuleID is what CollectAll is for at this level.
func TestL5ReportCarriesEveryFailingRuleID(t *testing.T) {
	t.Parallel()
	s := base()
	s.Request.Amount = money.MustNew(9_000_00, "USD") // wrong currency and over the limit
	s.Request.Method = shared.MethodSEPADebit         // not enabled
	s.Risk.OnPlatformBlocklist = true

	err := l5payment.Rules(deps()).Evaluate(t.Context(), s).AsError()
	if err == nil {
		t.Fatal("a payment failing four rules produced no error")
	}
	seen := map[string]bool{}
	for _, d := range err.Details {
		seen[d.RuleID] = true
		if d.RuleID == "" {
			t.Fatalf("a detail carries no rule ID: %+v", d)
		}
	}
	for _, want := range []string{
		"L5.CURRENCY_IS_ENABLED",
		"L5.PAYMENT_METHOD_IS_ENABLED",
		"L5.NOT_ON_PLATFORM_BLOCKLIST",
	} {
		if !seen[want] {
			t.Errorf("%s is missing from details[]", want)
		}
	}
}
