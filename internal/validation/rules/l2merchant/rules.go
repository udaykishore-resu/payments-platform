package l2merchant

import (
	"strings"
	"unicode"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/validation/engine"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/internal/ruledef"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

func init() {
	ruledef.Register(defs(DefaultDeps()), "merchant-onboarding", "2026-01-01", engine.Enforce)
}

// Rules returns the L2 rule set.
//
// CollectAll: the report is rendered in the merchant portal as a to-do list, and a to-do list
// with one item that grows a new item each time you complete it is the single most reliable
// way to lose a merchant during onboarding.
func Rules(d Deps) engine.RuleSet[Subject] {
	return engine.RuleSet[Subject]{
		Name:  "L2.merchant",
		Mode:  engine.CollectAll,
		Rules: ruledef.Build(defs(d)),
	}
}

func defs(d Deps) []ruledef.Def[Subject] {
	return []ruledef.Def[Subject]{
		{
			ID: "L2.LEGAL_NAME_PRESENT", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/profile/legalName", Pure: true,
			Desc:        "the registered legal name is 2–200 characters after normalization and trimming",
			Remediation: "Enter the registered legal name exactly as it appears on the incorporation document.",
			Check: func(s Subject) string {
				n := len([]rune(normalizeName(s.Profile.LegalName)))
				if n < 2 {
					return "the legal name is empty or shorter than 2 characters"
				}
				if n > 200 {
					return "the legal name is longer than 200 characters"
				}
				return ""
			},
		},
		{
			ID: "L2.BUSINESS_TYPE_IS_KNOWN", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/profile/businessType", Pure: true,
			Desc:        "the business type is one of the supported legal forms",
			Remediation: "Select a supported business type.",
			Check: func(s Subject) string {
				if s.Profile.BusinessType.IsKnown() {
					return ""
				}
				return "business type " + quote(string(s.Profile.BusinessType)) + " is not supported"
			},
		},
		{
			ID: "L2.REGISTRATION_NUMBER_FORMAT_VALID", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/profile/registrationNumber", Pure: true,
			Desc:        "the registration number matches the incorporation country's registry format",
			Remediation: "`registrationNumber` does not match the format used by the company registry of your incorporation country.",
			Applies:     func(s Subject) bool { return s.Profile.BusinessType != SoleTrader },
			Check: func(s Subject) string {
				if registrationNumberOK(s.Profile.IncorporationCountry, s.Profile.RegistrationNumber) {
					return ""
				}
				return "registration number " + quote(s.Profile.RegistrationNumber) +
					" does not match the " + string(s.Profile.IncorporationCountry) + " registry format"
			},
		},
		{
			ID: "L2.REGISTRY_RECORD_MATCHES", Severity: engine.Error,
			Code: "KYB_MISMATCH", Field: "/profile/legalName", Pure: false,
			Desc:        "the registry's legal name and registration number match the submission",
			Remediation: "The company registry shows a different legal name. Correct the submission or upload an amended certificate.",
			Applies:     func(s Subject) bool { return s.Vendor.KYB.Completed },
			Check: func(s Subject) string {
				k := s.Vendor.KYB
				if !k.NameMatchedAfterNormalization &&
					normalizeName(k.RegistryLegalName) != normalizeName(s.Profile.LegalName) {
					return "the registry records a different legal name for this registration"
				}
				if k.RegistryNumber != "" && !strings.EqualFold(
					stripNonAlnum(k.RegistryNumber), stripNonAlnum(s.Profile.RegistrationNumber)) {
					return "the registry records a different registration number"
				}
				return ""
			},
		},
		{
			ID: "L2.REGISTRY_STATUS_IS_ACTIVE", Severity: engine.Error,
			Code: "KYB_REJECTED", Field: "/profile/registrationNumber", Pure: false,
			Desc:        "the registry reports the company as active or in good standing",
			Remediation: "We cannot onboard a company that is not in good standing with its registry.",
			Applies:     func(s Subject) bool { return s.Vendor.KYB.Completed },
			Check: func(s Subject) string {
				switch strings.ToUpper(s.Vendor.KYB.RegistryStatus) {
				case "ACTIVE", "GOOD_STANDING":
					return ""
				case "":
					return "the registry returned no company status"
				default:
					return "the registry reports this company as " + s.Vendor.KYB.RegistryStatus
				}
			},
		},
		{
			ID: "L2.INCORPORATION_COUNTRY_SUPPORTED", Severity: engine.Error,
			Code: "COUNTRY_NOT_SUPPORTED", Field: "/profile/incorporationCountry", Pure: true,
			Desc:        "the incorporation country is in the tenant's supported country set",
			Remediation: "We do not currently onboard merchants incorporated in this country.",
			Check: func(s Subject) string {
				if containsCountry(d.SupportedCountries, s.Profile.IncorporationCountry) {
					return ""
				}
				return "merchants incorporated in " + string(s.Profile.IncorporationCountry) +
					" cannot be onboarded on this platform"
			},
		},
		{
			ID: "L2.COUNTRY_NOT_SANCTIONED", Severity: engine.Error,
			Code: "COUNTRY_BLOCKED", Field: "/profile/incorporationCountry", Pure: true,
			Desc:        "neither the incorporation country nor any operating country is on the sanctions list",
			Remediation: "We cannot onboard merchants in a sanctioned country.",
			Check: func(s Subject) string {
				if containsCountry(d.SanctionedCountries, s.Profile.IncorporationCountry) {
					return "the incorporation country is on the platform sanctions list"
				}
				for _, c := range s.Profile.OperatingCountries {
					if containsCountry(d.SanctionedCountries, c) {
						return "operating country " + string(c) + " is on the platform sanctions list"
					}
				}
				return ""
			},
		},
		{
			ID: "L2.OPERATING_COUNTRIES_SUBSET_OF_TENANT", Severity: engine.Error,
			Code: "COUNTRY_NOT_SUPPORTED", Field: "/profile/operatingCountries", Pure: true,
			Desc:        "every declared operating country is inside the tenant's licensed territory",
			Remediation: "Remove operating countries outside your platform's licensed territory.",
			Applies:     func(s Subject) bool { return len(s.Profile.OperatingCountries) > 0 },
			Check: func(s Subject) string {
				for _, c := range s.Profile.OperatingCountries {
					if !containsCountry(d.LicensedCountries, c) {
						return string(c) + " is outside your platform's licensed territory"
					}
				}
				return ""
			},
		},
		{
			ID: "L2.MCC_IS_VALID", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/profile/mcc", Pure: true,
			Desc:        "the merchant category code is four digits and present in the ISO 18245 table",
			Remediation: "Provide a valid four-digit merchant category code.",
			Check: func(s Subject) string {
				if _, err := shared.ParseMCC(string(s.Profile.MCC)); err != nil {
					return quote(string(s.Profile.MCC)) + " is not a valid merchant category code"
				}
				return ""
			},
		},
		{
			ID: "L2.MCC_NOT_PROHIBITED", Severity: engine.Error,
			Code: "MCC_PROHIBITED", Field: "/profile/mcc", Pure: true,
			Desc:        "the category is not on the platform's prohibited set without a tenant exception",
			Remediation: "This category is not supported. If you believe this is incorrect, request a category exception.",
			Check: func(s Subject) string {
				if containsMCC(d.MCCExceptions, s.Profile.MCC) {
					return ""
				}
				if prohibited, why := s.Profile.MCC.IsProhibited(); prohibited {
					return "category " + string(s.Profile.MCC) + " is prohibited (" + why + ")"
				}
				if containsMCC(d.ProhibitedMCCs, s.Profile.MCC) {
					return "category " + string(s.Profile.MCC) + " is prohibited on this platform"
				}
				return ""
			},
		},
		{
			ID: "L2.MCC_CONSISTENT_WITH_DESCRIPTION", Severity: engine.Warning,
			Code: "", Field: "/profile/mcc", Pure: true,
			Desc:        "the description classifier agrees with the declared category above the confidence floor",
			Remediation: "Your business description suggests a different category. An operator will confirm.",
			Applies:     func(s Subject) bool { return strings.TrimSpace(s.Profile.Description) != "" },
			Check: func(s Subject) string {
				floor := defaultInt(d.MinClassifierConfidencePercent, 40)
				if s.Profile.ClassifierMCC == "" || s.Profile.ClassifierMCC == s.Profile.MCC {
					return ""
				}
				if s.Profile.ClassifierConfidencePercent >= floor {
					return "your description suggests category " + string(s.Profile.ClassifierMCC) +
						"; you selected " + string(s.Profile.MCC)
				}
				return ""
			},
		},
		{
			ID: "L2.WEBSITE_IS_HTTPS", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/profile/website", Pure: true,
			Desc:        "the storefront URL is https, on a public hostname, not an IP literal",
			Remediation: "Provide a public HTTPS URL for your storefront.",
			Applies:     func(s Subject) bool { return s.Profile.Website != "" },
			Check: func(s Subject) string {
				return publicHTTPSProblem(s.Profile.Website)
			},
		},
		{
			ID: "L2.WEBSITE_REACHABLE", Severity: engine.Warning,
			Code: "", Field: "/profile/website", Pure: false,
			Desc:        "the storefront answered the probe with a 2xx or 3xx status",
			Remediation: "We could not reach your storefront. Ensure it is publicly reachable before certification.",
			Applies:     func(s Subject) bool { return s.Profile.Website != "" && s.Vendor.Website.Attempted },
			Check: func(s Subject) string {
				if code := s.Vendor.Website.StatusCode; code >= 200 && code < 400 {
					return ""
				}
				return "the storefront probe did not return a success status"
			},
		},
		{
			ID: "L2.WEBSITE_HAS_POLICY_PAGES", Severity: engine.Warning,
			Code: "", Field: "/profile/website", Pure: false,
			Desc:        "refund, privacy and contact pages were discoverable on the storefront",
			Remediation: "Card scheme rules require published refund, privacy and contact information.",
			Applies: func(s Subject) bool {
				return s.Vendor.Website.Attempted &&
					s.Vendor.Website.StatusCode >= 200 && s.Vendor.Website.StatusCode < 400
			},
			Check: func(s Subject) string {
				var missing []string
				if !s.Vendor.Website.HasRefundPolicy {
					missing = append(missing, "refund policy")
				}
				if !s.Vendor.Website.HasPrivacyPolicy {
					missing = append(missing, "privacy policy")
				}
				if !s.Vendor.Website.HasContactPage {
					missing = append(missing, "contact page")
				}
				if len(missing) == 0 {
					return ""
				}
				return "the storefront is missing: " + strings.Join(missing, ", ")
			},
		},
		{
			ID: "L2.TAX_ID_PRESENT_FOR_COUNTRY", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/taxIdentifiers", Pure: true,
			Desc:        "an identifier of the kind the incorporation country mandates is declared",
			Remediation: "Provide the tax identifier your incorporation country requires.",
			Applies: func(s Subject) bool {
				_, required := d.TaxIDRequiredKind[s.Profile.IncorporationCountry]
				return required
			},
			Check: func(s Subject) string {
				want := d.TaxIDRequiredKind[s.Profile.IncorporationCountry]
				for _, t := range s.TaxIdentifiers {
					if strings.EqualFold(t.Kind, want) && strings.TrimSpace(t.Value) != "" {
						return ""
					}
				}
				return "a " + want + " is required for merchants in " +
					string(s.Profile.IncorporationCountry)
			},
		},
		{
			ID: "L2.TAX_ID_CHECKSUM_VALID", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/taxIdentifiers", Pure: true,
			Desc:        "each declared tax identifier passes its country-specific checksum",
			Remediation: "A tax identifier failed its country checksum. Check for a transposed digit.",
			Applies:     func(s Subject) bool { return len(s.TaxIdentifiers) > 0 },
			Check: func(s Subject) string {
				for _, t := range s.TaxIdentifiers {
					if !taxIDChecksumOK(t) {
						return t.Kind + " " + quote(t.Value) + " fails the " + string(t.Country) + " checksum"
					}
				}
				return ""
			},
		},
		{
			ID: "L2.VAT_NUMBER_VERIFIED", Severity: engine.Warning,
			Code: "", Field: "/taxIdentifiers", Pure: false,
			Desc:        "VIES confirmed the EU VAT number and the name matched",
			Remediation: "VIES could not confirm this VAT number. Reverse-charge treatment may not apply.",
			Applies:     func(s Subject) bool { return s.Vendor.VIES.Attempted },
			Check: func(s Subject) string {
				if s.Vendor.VIES.Valid && s.Vendor.VIES.NameMatches {
					return ""
				}
				return "VIES did not confirm the declared VAT registration"
			},
		},
		{
			ID: "L2.AT_LEAST_ONE_PRINCIPAL", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/principals", Pure: true,
			Desc:        "at least one principal holds a control role",
			Remediation: "Add at least one director or controlling officer.",
			Check: func(s Subject) string {
				for _, p := range s.Principals {
					if p.IsControlRole {
						return ""
					}
				}
				return "no principal with a control role was declared"
			},
		},
		{
			ID: "L2.UBO_COVERAGE_COMPLETE", Severity: engine.Error,
			Code: "KYB_INCOMPLETE", Field: "/principals", Pure: true,
			Desc: "every natural person holding 25 % or more is declared, or a no-qualifying-UBO " +
				"attestation is on file",
			Remediation: "Declare every beneficial owner holding 25 % or more, or attest that none exists.",
			Applies:     func(s Subject) bool { return s.Profile.BusinessType != SoleTrader },
			Check: func(s Subject) string {
				declared := 0
				for _, p := range s.Principals {
					if p.OwnershipBasisPoints >= 2500 {
						declared++
					}
				}
				if declared > 0 || s.Documents.Attestation.Present {
					return ""
				}
				return "no beneficial owner at or above 25 % is declared and no attestation is on file"
			},
		},
		{
			ID: "L2.UBO_OWNERSHIP_SUMS_PLAUSIBLE", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/principals", Pure: true,
			Desc:        "declared ownership sums to no more than 100.5 %",
			Remediation: "Correct the ownership breakdown so the declared percentages are consistent.",
			Applies: func(s Subject) bool {
				for _, p := range s.Principals {
					if p.OwnershipBasisPoints > 0 {
						return true
					}
				}
				return false
			},
			Check: func(s Subject) string {
				total := 0
				for _, p := range s.Principals {
					if p.OwnershipBasisPoints < 0 {
						return "an ownership percentage is negative"
					}
					total += p.OwnershipBasisPoints
				}
				// 100.5 % of tolerance, because independently rounded shareholdings legitimately
				// sum a little above 100 and rejecting that would reject correct submissions.
				if total > 10050 {
					return "declared ownership percentages sum to " + bpsToPercent(total) + " %"
				}
				return ""
			},
		},
		{
			ID: "L2.PRINCIPAL_IS_ADULT", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/principals", Pure: true,
			Desc:        "every principal with a declared date of birth is at least 18 at the evaluation instant",
			Remediation: "All principals must be at least 18 years old.",
			Applies: func(s Subject) bool {
				for _, p := range s.Principals {
					if !p.DateOfBirth.IsZero() {
						return true
					}
				}
				return false
			},
			Check: func(s Subject) string {
				minAge := defaultInt(d.MinPrincipalAgeYears, 18)
				for _, p := range s.Principals {
					if p.DateOfBirth.IsZero() {
						continue
					}
					if p.DateOfBirth.AddDate(minAge, 0, 0).After(s.Now) {
						return "principal " + quote(p.Name) + " is younger than " + itoa(minAge)
					}
				}
				return ""
			},
		},
		{
			ID: "L2.PRINCIPAL_ADDRESS_COMPLETE", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/principals", Pure: true,
			Desc: "each principal has line 1, city, country, and a postal code where the country " +
				"uses one",
			Remediation: "Complete the residential address for every principal.",
			Applies:     func(s Subject) bool { return len(s.Principals) > 0 },
			Check: func(s Subject) string {
				for _, p := range s.Principals {
					a := p.Address
					switch {
					case strings.TrimSpace(a.Line1) == "":
						return "principal " + quote(p.Name) + " has no address line 1"
					case strings.TrimSpace(a.City) == "":
						return "principal " + quote(p.Name) + " has no city"
					case !a.Country.IsValid():
						return "principal " + quote(p.Name) + " has no valid country"
					case containsCountry(d.PostalCodeCountries, a.Country) &&
						strings.TrimSpace(a.PostalCode) == "":
						return "principal " + quote(p.Name) + " has no postal code, which " +
							string(a.Country) + " requires"
					}
				}
				return ""
			},
		},
		{
			ID: "L2.PRINCIPAL_ID_DOCUMENT_PRESENT", Severity: engine.Error,
			Code: "KYC_DOCUMENT_REQUIRED", Field: "/principals", Pure: true,
			Desc:        "an unexpired identity document of an accepted type is on file for each principal",
			Remediation: "Upload a valid government ID for every principal.",
			Applies:     func(s Subject) bool { return d.KYCRequiresIDDocument && len(s.Principals) > 0 },
			Check: func(s Subject) string {
				for _, p := range s.Principals {
					doc := p.IDDocument
					switch {
					case !doc.Present:
						return "no identity document for principal " + quote(p.Name)
					case !doc.ValidUntil.IsZero() && !doc.ValidUntil.After(s.Now):
						return "the identity document for principal " + quote(p.Name) + " has expired"
					case doc.RequiresBothSides && !doc.BothSidesProvided:
						return "only one side of the identity document for principal " +
							quote(p.Name) + " was uploaded"
					}
				}
				return ""
			},
		},
		{
			ID: "L2.KYC_DECISION_IS_APPROVED", Severity: engine.Error,
			Code: "KYC_REJECTED", Field: "/principals", Pure: false,
			Desc:        "every completed KYC decision is APPROVED, not REVIEW and not REJECTED",
			Remediation: "Identity verification did not pass. See the onboarding case for the vendor's reason code.",
			Applies:     func(s Subject) bool { return len(s.Vendor.KYC) > 0 },
			Check: func(s Subject) string {
				for _, p := range s.Principals {
					r, ok := s.Vendor.KYC[p.ID]
					if !ok || !r.Completed {
						continue
					}
					if r.Decision != DecisionApproved {
						return "identity verification for principal " + quote(p.Name) +
							" returned " + string(r.Decision)
					}
				}
				return ""
			},
		},
		{
			ID: "L2.KYB_DECISION_IS_APPROVED", Severity: engine.Error,
			Code: "KYB_REJECTED", Field: "/profile", Pure: false,
			Desc:        "the completed KYB decision is APPROVED",
			Remediation: "Business verification did not pass. See the onboarding case for details.",
			Applies:     func(s Subject) bool { return s.Vendor.KYB.Completed },
			Check: func(s Subject) string {
				if s.Vendor.KYB.Decision == DecisionApproved {
					return ""
				}
				return "business verification returned " + string(s.Vendor.KYB.Decision)
			},
		},
		{
			ID: "L2.NO_SANCTIONS_HIT", Severity: engine.Error,
			Code: "SANCTIONS_HIT", Field: "/screening", Pure: false,
			// The remediation is deliberately uninformative. Telling a merchant that they
			// matched a sanctions list is tipping-off, which is a criminal offence in several
			// of the jurisdictions this platform operates in. The detail lives in the
			// compliance case, not in the API response.
			Desc:        "no unresolved sanctions hit exists for the entity or any principal",
			Remediation: "We cannot proceed with this application. Contact compliance.",
			Applies:     func(s Subject) bool { return s.Vendor.Screening.Completed },
			Check: func(s Subject) string {
				if s.Vendor.Screening.UnresolvedSanctionHits == 0 {
					return ""
				}
				return "screening returned an unresolved hit"
			},
		},
		{
			ID: "L2.PEP_HIT_IS_MITIGATED", Severity: engine.Error,
			Code: "COMPLIANCE_REVIEW_REQUIRED", Field: "/screening", Pure: false,
			Desc:        "a politically-exposed-person hit has an approved enhanced-due-diligence record",
			Remediation: "Additional review is required before activation.",
			Applies:     func(s Subject) bool { return s.Vendor.Screening.Completed && s.Vendor.Screening.PEPHit },
			Check: func(s Subject) string {
				if s.Vendor.Screening.PEPEDDApproved {
					return ""
				}
				return "an enhanced-due-diligence approval is outstanding"
			},
		},
		{
			ID: "L2.ADVERSE_MEDIA_WITHIN_TOLERANCE", Severity: engine.Warning,
			Code: "", Field: "/screening", Pure: false,
			Desc:        "the adverse-media score is at or below the tenant threshold",
			Remediation: "Adverse media was found; an operator will review before activation.",
			Applies:     func(s Subject) bool { return s.Vendor.Screening.Completed },
			Check: func(s Subject) string {
				if s.Vendor.Screening.AdverseMediaScore <= d.AdverseMediaThreshold {
					return ""
				}
				return "the adverse-media score is above your platform's tolerance"
			},
		},
		{
			ID: "L2.BANK_ACCOUNT_FORMAT_VALID", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/bankAccounts", Pure: true,
			Desc:        "each account passes its scheme's checksum: IBAN mod-97, ABA, sort code, BSB",
			Remediation: "A bank account failed its scheme checksum. Re-enter the account details.",
			Applies:     func(s Subject) bool { return len(s.BankAccounts) > 0 },
			Check: func(s Subject) string {
				for _, a := range s.BankAccounts {
					if msg := bankAccountProblem(a); msg != "" {
						return msg
					}
				}
				return ""
			},
		},
		{
			ID: "L2.BANK_ACCOUNT_COUNTRY_SUPPORTS_CURRENCY", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/bankAccounts", Pure: true,
			Desc:        "at least one account can receive the declared settlement currency",
			Remediation: "Add a bank account that can receive your settlement currency.",
			Applies:     func(s Subject) bool { return len(s.BankAccounts) > 0 && s.Profile.SettlementCurrency != "" },
			Check: func(s Subject) string {
				for _, a := range s.BankAccounts {
					for _, c := range a.ReceivableCurrencies {
						if c == s.Profile.SettlementCurrency {
							return ""
						}
					}
				}
				return "no declared account can receive " + string(s.Profile.SettlementCurrency)
			},
		},
		{
			ID: "L2.BANK_ACCOUNT_OWNERSHIP_VERIFIED", Severity: engine.Error,
			Code: "BANK_VALIDATION_FAILED", Field: "/bankAccounts", Pure: false,
			Desc:        "the vendor confirmed the account holder name matches the legal name",
			Remediation: "The bank account holder name does not match your legal name. Use a business account in the registered name.",
			Applies:     func(s Subject) bool { return s.Vendor.BankValidation.Completed },
			Check: func(s Subject) string {
				if s.Vendor.BankValidation.HolderNameMatches {
					return ""
				}
				return "the account holder name does not match the registered legal name"
			},
		},
		{
			ID: "L2.BANK_ACCOUNT_NOT_SHARED", Severity: engine.Error,
			Code: "BANK_ACCOUNT_IN_USE", Field: "/bankAccounts", Pure: false,
			Desc:        "the account fingerprint is not already bound to another live merchant in this tenant",
			Remediation: "This bank account is already registered to another merchant.",
			Applies:     func(s Subject) bool { return len(s.BankAccounts) > 0 },
			Check: func(s Subject) string {
				// The other merchant's identity is deliberately not disclosed: answering "which
				// merchant" would turn the onboarding form into a lookup service for who banks
				// where.
				if s.Vendor.BankValidation.FingerprintBoundToOtherMerchant == "" {
					return ""
				}
				return "this account is already registered to a different merchant"
			},
		},
		{
			ID: "L2.EXPECTED_VOLUME_WITHIN_TIER", Severity: engine.Error,
			Code: "VOLUME_EXCEEDS_TIER", Field: "/processingProfile/monthlyVolume", Pure: true,
			Desc:        "declared monthly volume is at or below the tenant tier ceiling",
			Remediation: "Declared volume exceeds your tier limit. Contact your platform administrator to raise it.",
			Applies: func(s Subject) bool {
				return !s.ProcessingProfile.MonthlyVolume.IsZero() && !d.MonthlyVolumeCeiling.IsZero()
			},
			Check: func(s Subject) string {
				over, err := s.ProcessingProfile.MonthlyVolume.GreaterThan(d.MonthlyVolumeCeiling)
				if err != nil {
					return "declared volume is in " + string(s.ProcessingProfile.MonthlyVolume.Currency()) +
						", which is not your platform's tier currency"
				}
				if over {
					return "declared volume of " + s.ProcessingProfile.MonthlyVolume.String() +
						" exceeds the tier ceiling of " + d.MonthlyVolumeCeiling.String()
				}
				return ""
			},
		},
		{
			ID: "L2.AVERAGE_TICKET_CONSISTENT", Severity: engine.Warning,
			Code: "", Field: "/processingProfile/averageTicket", Pure: true,
			Desc:        "volume divided by average ticket implies a plausible monthly transaction count",
			Remediation: "Confirm your declared volume and average ticket: together they imply an implausible transaction count.",
			Applies: func(s Subject) bool {
				return !s.ProcessingProfile.MonthlyVolume.IsZero() &&
					s.ProcessingProfile.AverageTicket.Amount() > 0
			},
			Check: func(s Subject) string {
				n := s.ProcessingProfile.MonthlyVolume.Amount() / s.ProcessingProfile.AverageTicket.Amount()
				if n >= 1 && n <= 10_000_000 {
					return ""
				}
				return "the declared volume and average ticket imply " + itoa64(n) +
					" transactions per month"
			},
		},
		{
			ID: "L2.HIGH_RISK_PROFILE_HAS_RESERVE", Severity: engine.Error,
			Code: "COMPLIANCE_REVIEW_REQUIRED", Field: "/processingProfile", Pure: true,
			Desc:        "a high-risk category has a rolling-reserve term configured",
			Remediation: "Your category requires a rolling reserve. An operator will configure it.",
			Applies:     func(s Subject) bool { return containsMCC(d.HighRiskMCCs, s.Profile.MCC) },
			Check: func(s Subject) string {
				if s.ProcessingProfile.RollingReserveConfigured {
					return ""
				}
				return "no rolling reserve is configured for a high-risk category"
			},
		},
		{
			ID: "L2.RISK_SCORE_BELOW_AUTO_DECLINE", Severity: engine.Error,
			Code: "MERCHANT_RISK_DECLINED", Field: "/riskProfile", Pure: false,
			Desc:        "the composite onboarding risk score is below the auto-decline threshold",
			Remediation: "We are unable to onboard this business at this time.",
			Applies:     func(s Subject) bool { return s.Vendor.Risk.Scored },
			Check: func(s Subject) string {
				if s.Vendor.Risk.Score < defaultInt(d.RiskAutoDeclineAt, 85) {
					return ""
				}
				return "the onboarding risk score is at or above the auto-decline threshold"
			},
		},
		{
			ID: "L2.RISK_SCORE_BELOW_REVIEW", Severity: engine.Warning,
			Code: "", Field: "/riskProfile", Pure: false,
			Desc:        "the composite onboarding risk score is below the manual-review threshold",
			Remediation: "Your application requires manual review; expect up to 2 business days.",
			Applies:     func(s Subject) bool { return s.Vendor.Risk.Scored },
			Check: func(s Subject) string {
				if s.Vendor.Risk.Score < defaultInt(d.RiskReviewAt, 60) {
					return ""
				}
				return "the onboarding risk score routes this application to manual review"
			},
		},
		{
			ID: "L2.COMPLIANCE_ATTESTATION_SIGNED", Severity: engine.Error,
			Code: "COMPLIANCE_REVIEW_REQUIRED", Field: "/documents/attestation", Pure: true,
			Desc:        "the terms and card-scheme attestation is signed by an authorized principal, with timestamp and IP",
			Remediation: "An authorized principal must accept the merchant agreement.",
			Check: func(s Subject) string {
				a := s.Documents.Attestation
				switch {
				case !a.Present:
					return "the merchant agreement has not been accepted"
				case a.SignedAt.IsZero():
					return "the attestation carries no signature timestamp"
				case a.SignedByPrincipal == "":
					return "the attestation is not attributed to a principal"
				case a.SignedFromIP == "":
					return "the attestation carries no originating IP address"
				case !principalExists(s.Principals, a.SignedByPrincipal):
					return "the attestation was signed by someone who is not a declared principal"
				}
				return ""
			},
		},
		{
			ID: "L2.PCI_SAQ_ON_FILE", Severity: engine.Error,
			Code: "COMPLIANCE_REVIEW_REQUIRED", Field: "/documents/pciSaq", Pure: true,
			Desc:        "a current PCI SAQ of the correct type is on file for a merchant that touches card data",
			Remediation: "Upload a current PCI SAQ. Using our hosted fields keeps you on SAQ-A.",
			Applies:     func(s Subject) bool { return s.Profile.HandlesCardDataDirectly },
			Check: func(s Subject) string {
				q := s.Documents.PCISAQ
				switch {
				case !q.Present:
					return "no PCI self-assessment questionnaire is on file"
				case s.Profile.SAQType == "":
					return "the PCI SAQ type has not been declared"
				case q.ValidUntil.IsZero() || !q.ValidUntil.After(s.Now):
					return "the PCI SAQ on file has expired"
				}
				return ""
			},
		},
		{
			ID: "L2.DATA_RESIDENCY_DECLARED", Severity: engine.Error,
			Code: string(apierror.CodeConfigurationInvalid), Field: "/profile/dataResidencyRegion", Pure: true,
			Desc:        "a residency region is declared and permitted for this tenant",
			Remediation: "Select a data residency region your platform permits.",
			Check: func(s Subject) string {
				if s.Profile.DataResidencyRegion == "" {
					return "no data residency region was declared"
				}
				if len(d.PermittedResidencyRegions) > 0 &&
					!containsString(d.PermittedResidencyRegions, s.Profile.DataResidencyRegion) {
					return "region " + quote(s.Profile.DataResidencyRegion) +
						" is not permitted for your platform"
				}
				return ""
			},
		},
	}
}

// --- helpers ---------------------------------------------------------------------------------

// normalizeName folds a submitted name to a comparable form: trimmed, internally
// space-collapsed, case-folded.
//
// Deliberately not full Unicode NFKC. That would need golang.org/x/text, and this package is
// stdlib-only by the architecture rule. The residual risk is a name that differs only by a
// compatibility-equivalent code point comparing unequal against the registry, which surfaces
// as a KYB mismatch an operator resolves — visible and recoverable, unlike the alternative of
// pulling a transitive dependency into every binary that validates a merchant.
func normalizeName(s string) string {
	fields := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(s)), unicode.IsSpace)
	return strings.Join(fields, " ")
}

func stripNonAlnum(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToUpper(r)
		}
		return -1
	}, s)
}

// registrationNumberOK applies the incorporation country's registry format. Countries the
// platform has not encoded fall back to a length check rather than to rejection: a
// conservative default here rejects legitimate merchants from every country the table does not
// yet mention, which is a far larger error than accepting a malformed number that the KYB
// vendor will reject a step later.
func registrationNumberOK(country shared.Country, number string) bool {
	v := stripNonAlnum(number)
	switch country {
	case "GB":
		return len(v) == 8
	case "DE":
		return strings.HasPrefix(v, "HRA") || strings.HasPrefix(v, "HRB")
	case "US":
		return len(v) == 9 && allDigits(v)
	case "AU":
		return len(v) == 9 && allDigits(v)
	case "IE":
		return len(v) >= 5 && len(v) <= 8
	case "NL":
		return len(v) == 8 && allDigits(v)
	case "FR":
		return len(v) == 9 && allDigits(v)
	}
	return len(v) >= 3
}

// taxIDChecksumOK applies the identifier kind's checksum.
func taxIDChecksumOK(t TaxIdentifier) bool {
	v := stripNonAlnum(t.Value)
	if v == "" {
		return false
	}
	switch strings.ToUpper(t.Kind) {
	case "EIN":
		return len(v) == 9 && allDigits(v)
	case "VAT":
		return vatOK(t.Country, v)
	case "ABN":
		return abnOK(v)
	case "UTR":
		return len(v) == 10 && allDigits(v)
	}
	return len(v) >= 5
}

// vatOK checks the country prefix and the country-specific length/checksum.
func vatOK(country shared.Country, v string) bool {
	if len(v) >= 2 && v[0] >= 'A' && v[0] <= 'Z' && v[1] >= 'A' && v[1] <= 'Z' {
		prefix := shared.Country(v[:2])
		// The GB→XI Northern Ireland prefix and the EL→GR Greek prefix are the two places
		// where the VAT prefix is legitimately not the ISO country code.
		if prefix != country && (country != "GR" || prefix != "EL") && (country != "GB" || prefix != "XI") {
			return false
		}
		v = v[2:]
	}
	switch country {
	case "GB":
		return (len(v) == 9 || len(v) == 12) && allDigits(v) && mod97Check(v[:9], 9)
	case "DE":
		return len(v) == 9 && allDigits(v)
	case "NL":
		return len(v) == 12
	case "FR":
		return len(v) == 11
	case "IE":
		return len(v) >= 8 && len(v) <= 9
	case "ES", "IT":
		return len(v) >= 9 && len(v) <= 11
	}
	return len(v) >= 8
}

// mod97Check applies the UK VAT 97-55 rule to the first `n` digits.
func mod97Check(digits string, n int) bool {
	if len(digits) < n {
		return false
	}
	weights := []int{8, 7, 6, 5, 4, 3, 2}
	sum := 0
	for i := 0; i < 7; i++ {
		sum += int(digits[i]-'0') * weights[i]
	}
	check := int(digits[7]-'0')*10 + int(digits[8]-'0')
	for sum > 0 {
		sum -= 97
	}
	return -sum == check || -sum-55 == check || (-sum+42)%97 == check
}

// abnOK applies the Australian Business Number weighted mod-89 checksum.
func abnOK(v string) bool {
	if len(v) != 11 || !allDigits(v) {
		return false
	}
	weights := []int{10, 1, 3, 5, 7, 9, 11, 13, 15, 17, 19}
	sum := 0
	for i := 0; i < 11; i++ {
		d := int(v[i] - '0')
		if i == 0 {
			d--
		}
		sum += d * weights[i]
	}
	return sum%89 == 0
}

// bankAccountProblem states what is wrong with a submitted account, or "".
func bankAccountProblem(a BankAccount) string {
	switch strings.ToUpper(a.Scheme) {
	case "IBAN":
		if !ibanOK(a.IBAN) {
			return "IBAN " + quote(mask(a.IBAN)) + " fails the mod-97 checksum"
		}
	case "ACH":
		if !abaOK(a.RoutingNumber) {
			return "routing number " + quote(a.RoutingNumber) + " fails the ABA checksum"
		}
		if len(stripNonAlnum(a.AccountNumber)) < 4 {
			return "the account number is too short to be an ACH account number"
		}
	case "BACS":
		if len(stripNonAlnum(a.SortCode)) != 6 || !allDigits(stripNonAlnum(a.SortCode)) {
			return "sort code " + quote(a.SortCode) + " is not six digits"
		}
		if n := stripNonAlnum(a.AccountNumber); len(n) != 8 || !allDigits(n) {
			return "the UK account number is not eight digits"
		}
	case "BECS":
		if len(stripNonAlnum(a.BSB)) != 6 || !allDigits(stripNonAlnum(a.BSB)) {
			return "BSB " + quote(a.BSB) + " is not six digits"
		}
	default:
		return "bank account scheme " + quote(a.Scheme) + " is not recognized"
	}
	return ""
}

// ibanOK applies the ISO 13616 mod-97 checksum.
func ibanOK(iban string) bool {
	v := stripNonAlnum(iban)
	if len(v) < 15 || len(v) > 34 {
		return false
	}
	rearranged := v[4:] + v[:4]
	rem := 0
	for i := 0; i < len(rearranged); i++ {
		c := rearranged[i]
		switch {
		case c >= '0' && c <= '9':
			rem = rem*10 + int(c-'0')
		case c >= 'A' && c <= 'Z':
			rem = rem*100 + int(c-'A') + 10
		default:
			return false
		}
		rem %= 97
	}
	return rem == 1
}

// abaOK applies the ABA routing-number weighted checksum.
func abaOK(v string) bool {
	r := stripNonAlnum(v)
	if len(r) != 9 || !allDigits(r) {
		return false
	}
	sum := 3*(int(r[0]-'0')+int(r[3]-'0')+int(r[6]-'0')) +
		7*(int(r[1]-'0')+int(r[4]-'0')+int(r[7]-'0')) +
		(int(r[2]-'0') + int(r[5]-'0') + int(r[8]-'0'))
	return sum%10 == 0
}

// publicHTTPSProblem states why a URL is not a usable public storefront, or "".
func publicHTTPSProblem(raw string) string {
	if !strings.HasPrefix(strings.ToLower(raw), "https://") {
		return "the storefront URL is not https"
	}
	host := raw[len("https://"):]
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		return "the storefront URL has no host"
	}
	if isIPLiteral(host) {
		return "the storefront URL is an IP literal rather than a hostname"
	}
	if !strings.Contains(strings.TrimSuffix(host, "."), ".") {
		return "the storefront host is not a public domain name"
	}
	if strings.EqualFold(host, "localhost") {
		return "the storefront host is not publicly resolvable"
	}
	return ""
}

func isIPLiteral(host string) bool {
	if strings.HasPrefix(host, "[") {
		return true
	}
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || !allDigits(p) {
			return false
		}
	}
	return true
}

func principalExists(ps []Principal, id string) bool {
	for _, p := range ps {
		if p.ID == id {
			return true
		}
	}
	return false
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func containsCountry(set []shared.Country, v shared.Country) bool {
	for _, c := range set {
		if c == v {
			return true
		}
	}
	return false
}

func containsMCC(set []shared.MCC, v shared.MCC) bool {
	for _, m := range set {
		if m == v {
			return true
		}
	}
	return false
}

func containsString(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// mask reduces an account identifier to a shape: the scheme prefix and the last two characters.
// A rejection message must let a merchant recognize which account failed without republishing
// the account number into a log.
func mask(v string) string {
	c := stripNonAlnum(v)
	if len(c) <= 6 {
		return "****"
	}
	return c[:4] + "****" + c[len(c)-2:]
}

func bpsToPercent(bps int) string {
	whole := bps / 100
	frac := bps % 100
	if frac == 0 {
		return itoa(whole)
	}
	if frac < 10 {
		return itoa(whole) + ".0" + itoa(frac)
	}
	return itoa(whole) + "." + itoa(frac)
}

func quote(s string) string {
	if s == "" {
		return "(empty)"
	}
	return "`" + s + "`"
}

func itoa(n int) string { return itoa64(int64(n)) }

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
