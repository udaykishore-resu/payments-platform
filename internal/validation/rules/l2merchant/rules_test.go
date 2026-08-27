package l2merchant_test

import (
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/internal/ruletest"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/l2merchant"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

var now = time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)

func gbp(minor int64) money.Money { return money.MustNew(minor, "GBP") }

func deps() l2merchant.Deps {
	d := l2merchant.DefaultDeps()
	d.SupportedCountries = []shared.Country{"GB", "DE", "FR", "US", "IE"}
	d.LicensedCountries = []shared.Country{"GB", "DE", "FR", "US", "IE"}
	d.SanctionedCountries = []shared.Country{"IR", "KP", "SY"}
	d.HighRiskMCCs = []shared.MCC{"5967", "7995"}
	d.MonthlyVolumeCeiling = gbp(1_000_000_00)
	d.PermittedResidencyRegions = []string{"eu-west-1", "us-east-1"}
	d.KYCRequiresIDDocument = true
	return d
}

// base is a merchant submission that satisfies all forty L2 rules: a UK limited company with a
// completed KYB, an approved principal, a valid VAT registration, a checksum-clean IBAN and a
// clean screening result.
func base() l2merchant.Subject {
	return l2merchant.Subject{
		Profile: l2merchant.Profile{
			LegalName:                   "Acme Widgets Limited",
			TradingName:                 "Acme",
			BusinessType:                l2merchant.LLC,
			RegistrationNumber:          "12345678",
			IncorporationCountry:        "GB",
			OperatingCountries:          []shared.Country{"GB", "DE"},
			MCC:                         "5411",
			Description:                 "Online grocery retailer",
			ClassifierMCC:               "5411",
			ClassifierConfidencePercent: 85,
			Website:                     "https://acme.example.com",
			SettlementCurrency:          "GBP",
			DataResidencyRegion:         "eu-west-1",
		},
		Principals: []l2merchant.Principal{
			{
				ID:                   "prn_1",
				Name:                 "Jane Doe",
				Role:                 "DIRECTOR",
				IsControlRole:        true,
				OwnershipBasisPoints: 6000,
				DateOfBirth:          time.Date(1984, 5, 2, 0, 0, 0, 0, time.UTC),
				Address: l2merchant.Address{
					Line1: "1 High Street", City: "London", PostalCode: "EC1A 1BB", Country: "GB",
				},
				IDDocument: l2merchant.Document{
					Kind: "PASSPORT", Present: true,
					ValidUntil: now.AddDate(4, 0, 0),
				},
			},
		},
		TaxIdentifiers: []l2merchant.TaxIdentifier{
			{Kind: "VAT", Value: "GB123456782", Country: "GB"},
		},
		BankAccounts: []l2merchant.BankAccount{
			{
				Scheme: "IBAN", Country: "GB",
				IBAN:                 "GB82WEST12345698765432",
				Fingerprint:          "fp_acme_1",
				ReceivableCurrencies: []money.Currency{"GBP"},
			},
		},
		ProcessingProfile: l2merchant.ProcessingProfile{
			MonthlyVolume: gbp(100_000_00),
			AverageTicket: gbp(50_00),
		},
		Vendor: l2merchant.VendorResults{
			KYB: l2merchant.KYBResult{
				Completed: true, Decision: l2merchant.DecisionApproved,
				RegistryLegalName: "Acme Widgets Limited", RegistryNumber: "12345678",
				RegistryStatus: "ACTIVE", NameMatchedAfterNormalization: true,
			},
			KYC: map[string]l2merchant.KYCResult{
				"prn_1": {Completed: true, Decision: l2merchant.DecisionApproved},
			},
			Screening: l2merchant.ScreeningResult{Completed: true, AdverseMediaScore: 10},
			BankValidation: l2merchant.BankValidationResult{
				Completed: true, HolderNameMatches: true,
			},
			Website: l2merchant.WebsiteProbe{
				Attempted: true, StatusCode: 200,
				HasRefundPolicy: true, HasPrivacyPolicy: true, HasContactPage: true,
			},
			VIES: l2merchant.VIESResult{Attempted: true, Valid: true, NameMatches: true},
			Risk: l2merchant.RiskScore{Scored: true, Score: 20},
		},
		Documents: l2merchant.Documents{
			Attestation: l2merchant.Document{
				Present: true, SignedAt: now.Add(-24 * time.Hour),
				SignedByPrincipal: "prn_1", SignedFromIP: "203.0.113.4",
			},
		},
		Now: now,
	}
}

func TestL2Rules(t *testing.T) {
	// Verifies: BR-05, BR-06, FR-22.
	t.Parallel()
	set := l2merchant.Rules(deps())

	ruletest.Run(t, set, base, []ruletest.Case[l2merchant.Subject]{
		{
			ID:   "L2.LEGAL_NAME_PRESENT",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.Profile.LegalName = "  " },
		},
		{
			ID:   "L2.BUSINESS_TYPE_IS_KNOWN",
			Pass: func(s *l2merchant.Subject) { s.Profile.BusinessType = l2merchant.Corporation },
			Fail: func(s *l2merchant.Subject) { s.Profile.BusinessType = "COOPERATIVE" },
		},
		{
			ID:   "L2.REGISTRATION_NUMBER_FORMAT_VALID",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.Profile.RegistrationNumber = "123" },
		},
		{
			ID:   "L2.REGISTRY_RECORD_MATCHES",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) {
				s.Vendor.KYB.NameMatchedAfterNormalization = false
				s.Vendor.KYB.RegistryLegalName = "Different Holdings Limited"
			},
		},
		{
			ID:   "L2.REGISTRY_STATUS_IS_ACTIVE",
			Pass: func(s *l2merchant.Subject) { s.Vendor.KYB.RegistryStatus = "GOOD_STANDING" },
			Fail: func(s *l2merchant.Subject) { s.Vendor.KYB.RegistryStatus = "DISSOLVED" },
		},
		{
			ID:   "L2.INCORPORATION_COUNTRY_SUPPORTED",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.Profile.IncorporationCountry = "BR" },
		},
		{
			ID:   "L2.COUNTRY_NOT_SANCTIONED",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) {
				s.Profile.OperatingCountries = []shared.Country{"GB", "IR"}
			},
		},
		{
			ID:   "L2.OPERATING_COUNTRIES_SUBSET_OF_TENANT",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) {
				s.Profile.OperatingCountries = []shared.Country{"GB", "BR"}
			},
		},
		{
			ID:   "L2.MCC_IS_VALID",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.Profile.MCC = "54" },
		},
		{
			ID:   "L2.MCC_NOT_PROHIBITED",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.Profile.MCC = "7995" },
		},
		{
			ID:   "L2.MCC_CONSISTENT_WITH_DESCRIPTION",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) {
				s.Profile.ClassifierMCC = "5812"
				s.Profile.ClassifierConfidencePercent = 75
			},
		},
		{
			ID:   "L2.WEBSITE_IS_HTTPS",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.Profile.Website = "http://acme.example.com" },
		},
		{
			ID:   "L2.WEBSITE_REACHABLE",
			Pass: func(s *l2merchant.Subject) { s.Vendor.Website.StatusCode = 301 },
			Fail: func(s *l2merchant.Subject) { s.Vendor.Website.StatusCode = 503 },
		},
		{
			ID:   "L2.WEBSITE_HAS_POLICY_PAGES",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.Vendor.Website.HasRefundPolicy = false },
		},
		{
			ID:   "L2.TAX_ID_PRESENT_FOR_COUNTRY",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.TaxIdentifiers = nil },
		},
		{
			ID:   "L2.TAX_ID_CHECKSUM_VALID",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.TaxIdentifiers[0].Value = "GB123456789" },
		},
		{
			ID:   "L2.VAT_NUMBER_VERIFIED",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.Vendor.VIES.Valid = false },
		},
		{
			ID:   "L2.AT_LEAST_ONE_PRINCIPAL",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.Principals[0].IsControlRole = false },
		},
		{
			ID:   "L2.UBO_COVERAGE_COMPLETE",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) {
				s.Principals[0].OwnershipBasisPoints = 1000
				s.Documents.Attestation.Present = false
			},
		},
		{
			ID:   "L2.UBO_OWNERSHIP_SUMS_PLAUSIBLE",
			Pass: func(s *l2merchant.Subject) { s.Principals[0].OwnershipBasisPoints = 10000 },
			Fail: func(s *l2merchant.Subject) { s.Principals[0].OwnershipBasisPoints = 11000 },
		},
		{
			ID: "L2.PRINCIPAL_IS_ADULT",
			Pass: func(s *l2merchant.Subject) {
				s.Principals[0].DateOfBirth = now.AddDate(-18, 0, -1)
			},
			Fail: func(s *l2merchant.Subject) {
				s.Principals[0].DateOfBirth = now.AddDate(-17, 0, 0)
			},
		},
		{
			ID:   "L2.PRINCIPAL_ADDRESS_COMPLETE",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.Principals[0].Address.City = "" },
		},
		{
			ID:   "L2.PRINCIPAL_ID_DOCUMENT_PRESENT",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.Principals[0].IDDocument.Present = false },
		},
		{
			ID:   "L2.KYC_DECISION_IS_APPROVED",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) {
				s.Vendor.KYC["prn_1"] = l2merchant.KYCResult{
					Completed: true, Decision: l2merchant.DecisionReview,
				}
			},
		},
		{
			ID:   "L2.KYB_DECISION_IS_APPROVED",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.Vendor.KYB.Decision = l2merchant.DecisionRejected },
		},
		{
			ID:   "L2.NO_SANCTIONS_HIT",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.Vendor.Screening.UnresolvedSanctionHits = 1 },
		},
		{
			ID: "L2.PEP_HIT_IS_MITIGATED",
			Pass: func(s *l2merchant.Subject) {
				s.Vendor.Screening.PEPHit = true
				s.Vendor.Screening.PEPEDDApproved = true
			},
			Fail: func(s *l2merchant.Subject) { s.Vendor.Screening.PEPHit = true },
		},
		{
			ID:   "L2.ADVERSE_MEDIA_WITHIN_TOLERANCE",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.Vendor.Screening.AdverseMediaScore = 90 },
		},
		{
			ID:   "L2.BANK_ACCOUNT_FORMAT_VALID",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.BankAccounts[0].IBAN = "GB00WEST12345698765432" },
		},
		{
			ID:   "L2.BANK_ACCOUNT_COUNTRY_SUPPORTS_CURRENCY",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) {
				s.BankAccounts[0].ReceivableCurrencies = []money.Currency{"EUR"}
			},
		},
		{
			ID:   "L2.BANK_ACCOUNT_OWNERSHIP_VERIFIED",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.Vendor.BankValidation.HolderNameMatches = false },
		},
		{
			ID:   "L2.BANK_ACCOUNT_NOT_SHARED",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) {
				s.Vendor.BankValidation.FingerprintBoundToOtherMerchant = "mrc_other"
			},
		},
		{
			ID:   "L2.EXPECTED_VOLUME_WITHIN_TIER",
			Pass: func(s *l2merchant.Subject) { s.ProcessingProfile.MonthlyVolume = gbp(1_000_000_00) },
			Fail: func(s *l2merchant.Subject) { s.ProcessingProfile.MonthlyVolume = gbp(1_000_000_01) },
		},
		{
			ID:   "L2.AVERAGE_TICKET_CONSISTENT",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.ProcessingProfile.AverageTicket = gbp(200_000_00) },
		},
		{
			ID: "L2.HIGH_RISK_PROFILE_HAS_RESERVE",
			Pass: func(s *l2merchant.Subject) {
				s.Profile.MCC = "5967"
				s.ProcessingProfile.RollingReserveConfigured = true
			},
			Fail: func(s *l2merchant.Subject) { s.Profile.MCC = "5967" },
		},
		{
			ID:   "L2.RISK_SCORE_BELOW_AUTO_DECLINE",
			Pass: func(s *l2merchant.Subject) { s.Vendor.Risk.Score = 84 },
			Fail: func(s *l2merchant.Subject) { s.Vendor.Risk.Score = 85 },
		},
		{
			ID:   "L2.RISK_SCORE_BELOW_REVIEW",
			Pass: func(s *l2merchant.Subject) { s.Vendor.Risk.Score = 59 },
			Fail: func(s *l2merchant.Subject) { s.Vendor.Risk.Score = 60 },
		},
		{
			ID:   "L2.COMPLIANCE_ATTESTATION_SIGNED",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.Documents.Attestation.SignedFromIP = "" },
		},
		{
			ID: "L2.PCI_SAQ_ON_FILE",
			Pass: func(s *l2merchant.Subject) {
				s.Profile.HandlesCardDataDirectly = true
				s.Profile.SAQType = "D"
				s.Documents.PCISAQ = l2merchant.Document{
					Kind: "SAQ-D", Present: true, ValidUntil: now.AddDate(0, 6, 0),
				}
			},
			Fail: func(s *l2merchant.Subject) {
				s.Profile.HandlesCardDataDirectly = true
				s.Profile.SAQType = "D"
				s.Documents.PCISAQ = l2merchant.Document{
					Kind: "SAQ-D", Present: true, ValidUntil: now.AddDate(0, -1, 0),
				}
			},
		},
		{
			ID:   "L2.DATA_RESIDENCY_DECLARED",
			Pass: func(s *l2merchant.Subject) {},
			Fail: func(s *l2merchant.Subject) { s.Profile.DataResidencyRegion = "ap-south-1" },
		},
	})
}

// TestL2CleanSubmissionProducesNoFailures anchors the base subject: if it ever stops being
// valid, every case above is testing something other than what it claims to.
func TestL2CleanSubmissionProducesNoFailures(t *testing.T) {
	t.Parallel()
	rep := l2merchant.Rules(deps()).Evaluate(t.Context(), base())
	if !rep.OK() {
		t.Fatalf("the reference submission was rejected: %v", rep.Errors())
	}
	if len(rep.Failures()) != 0 {
		t.Fatalf("the reference submission produced warnings: %v", rep.Failures())
	}
}

// TestL2SanctionsRemediationWithholdsDetail: telling a merchant they matched a sanctions list
// is tipping-off, which is a criminal offence in several jurisdictions the platform operates in.
func TestL2SanctionsRemediationWithholdsDetail(t *testing.T) {
	t.Parallel()
	s := base()
	s.Vendor.Screening.UnresolvedSanctionHits = 2

	rule, ok := l2merchant.Rules(deps()).Rule("L2.NO_SANCTIONS_HIT")
	if !ok {
		t.Fatal("the sanctions rule is not in the L2 set")
	}
	out := rule.Evaluate(t.Context(), s)
	if out.Passed {
		t.Fatal("an unresolved sanctions hit passed")
	}
	for _, forbidden := range []string{"2", "OFAC", "list", "match"} {
		if containsFold(out.Remediation, forbidden) {
			t.Fatalf("the sanctions remediation discloses %q: %q", forbidden, out.Remediation)
		}
	}
}

func containsFold(haystack, needle string) bool {
	h, n := lower(haystack), lower(needle)
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}
