// Package l2merchant is validation level 2: everything the platform asserts about a merchant
// before it may exist as an activatable entity — business identity, country, tax, KYC/KYB,
// bank, risk profile and compliance.
//
// Two design decisions dominate this level.
//
// First, mode. L2 is CollectAll, because the reader of the result is a human filling in an
// onboarding form. A merchant who fixes one field, resubmits, and is told about the next field
// takes nine business days to onboard and files a complaint on day three.
//
// Second, vendor determinism. A third of these rules depend on a decision taken by a KYB
// registry, a KYC vendor, a sanctions screener or a bank-account validator. None of them make
// that call. Each takes the vendor's *persisted decision* as a field of the subject, so
// re-running L2 over the stored subject months later produces the same report — which is what
// makes an onboarding rejection defensible to a regulator, and what makes every rule here
// testable without a network. The rules are marked impure because the decision came from
// outside; they are deterministic because the decision is now an input.
//
// See docs/validation-plane.md §3.2.
package l2merchant

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// BusinessType is the merchant's legal form. The set is closed because downstream KYB
// requirements — whether a registration number exists at all, whether UBOs must be declared —
// are keyed on it.
type BusinessType string

// The supported legal forms.
const (
	SoleTrader  BusinessType = "SOLE_TRADER"
	Partnership BusinessType = "PARTNERSHIP"
	LLC         BusinessType = "LLC"
	Corporation BusinessType = "CORPORATION"
	NonProfit   BusinessType = "NON_PROFIT"
	PublicBody  BusinessType = "PUBLIC_BODY"
)

// IsKnown reports whether b is a supported legal form.
func (b BusinessType) IsKnown() bool {
	switch b {
	case SoleTrader, Partnership, LLC, Corporation, NonProfit, PublicBody:
		return true
	}
	return false
}

// Address is a postal address as submitted.
type Address struct {
	Line1      string
	Line2      string
	City       string
	PostalCode string
	Country    shared.Country
}

// Document is an uploaded document's metadata. The content never reaches validation: a rule
// that needed to read a passport scan would be a rule that needed the document in memory, and
// the platform's answer to "where is the ID document" must stay "in the document store, behind
// its own access control".
type Document struct {
	Kind              string
	Present           bool
	RequiresBothSides bool
	BothSidesProvided bool
	ValidUntil        time.Time
	SignedAt          time.Time
	SignedByPrincipal string
	SignedFromIP      string
}

// Principal is a natural person with control of, or ownership in, the merchant.
type Principal struct {
	ID   string
	Name string
	Role string
	// IsControlRole marks a director, officer or other controlling person, as distinct from a
	// beneficial owner who holds shares but does not direct the business.
	IsControlRole bool
	// OwnershipBasisPoints is ownership in basis points (10000 = 100 %). Basis points rather
	// than a float because a percentage that is summed and compared against a threshold has the
	// same rounding hazards as money, and the fix is the same: integers.
	OwnershipBasisPoints int
	DateOfBirth          time.Time
	Address              Address
	IDDocument           Document
}

// TaxIdentifier is one declared tax registration.
type TaxIdentifier struct {
	Kind    string // EIN, VAT, UTR, ABN, GST …
	Value   string
	Country shared.Country
}

// BankAccount is a settlement account as submitted.
type BankAccount struct {
	Scheme  string // IBAN | ACH | BACS | BECS
	Country shared.Country
	// IBAN, RoutingNumber, SortCode, BSB and AccountNumber are structural identifiers, not
	// credentials. The account number is stored as submitted because the checksum rules need
	// it; the fingerprint is what is compared across merchants.
	IBAN          string
	RoutingNumber string
	SortCode      string
	BSB           string
	AccountNumber string
	Fingerprint   string
	// ReceivableCurrencies is what this account's country and scheme can actually receive.
	ReceivableCurrencies []money.Currency
}

// ProcessingProfile is the merchant's declared trading expectation.
type ProcessingProfile struct {
	MonthlyVolume money.Money
	AverageTicket money.Money
	// RollingReserveConfigured records whether a reserve term exists on the agreement.
	RollingReserveConfigured bool
}

// Decision is a vendor's verdict, normalized.
type Decision string

// The normalized vendor verdicts. REVIEW is distinct from REJECTED because a platform that
// treats "the vendor wants a human to look" as "no" declines businesses it would have
// onboarded, and one that treats it as "yes" onboards businesses it should not have.
const (
	DecisionApproved Decision = "APPROVED"
	DecisionReview   Decision = "REVIEW"
	DecisionRejected Decision = "REJECTED"
	DecisionPending  Decision = "PENDING"
)

// KYBResult is the persisted company-registry lookup.
type KYBResult struct {
	Completed                     bool
	Decision                      Decision
	RegistryLegalName             string
	RegistryNumber                string
	RegistryStatus                string // ACTIVE | GOOD_STANDING | DISSOLVED | STRUCK_OFF | LIQUIDATION
	NameMatchedAfterNormalization bool
}

// KYCResult is one principal's persisted identity-verification decision.
type KYCResult struct {
	Completed      bool
	Decision       Decision
	VendorReason   string
	DocumentNeeded bool
}

// ScreeningResult is the persisted sanctions, PEP and adverse-media screening.
type ScreeningResult struct {
	Completed              bool
	UnresolvedSanctionHits int
	PEPHit                 bool
	PEPEDDApproved         bool
	AdverseMediaScore      int
}

// BankValidationResult is the persisted account-ownership check.
type BankValidationResult struct {
	Completed         bool
	HolderNameMatches bool
	// FingerprintBoundToOtherMerchant names the other merchant an account is already bound to,
	// or "" when the account is unused. A named other merchant is the finding; the name is not
	// disclosed to the caller, because it would be a directory of who banks where.
	FingerprintBoundToOtherMerchant string
}

// WebsiteProbe is the persisted result of fetching the merchant's storefront.
type WebsiteProbe struct {
	Attempted        bool
	StatusCode       int
	HasRefundPolicy  bool
	HasPrivacyPolicy bool
	HasContactPage   bool
}

// VIESResult is the persisted EU VAT verification.
type VIESResult struct {
	Attempted   bool
	Valid       bool
	NameMatches bool
}

// RiskScore is the persisted composite onboarding risk score, 0–100, higher is worse.
type RiskScore struct {
	Scored bool
	Score  int
}

// VendorResults collects every externally-obtained decision, so the rules that depend on them
// depend on a value rather than on a call.
type VendorResults struct {
	KYB            KYBResult
	KYC            map[string]KYCResult
	Screening      ScreeningResult
	BankValidation BankValidationResult
	Website        WebsiteProbe
	VIES           VIESResult
	Risk           RiskScore
}

// Profile is the merchant's declared identity.
type Profile struct {
	LegalName            string
	TradingName          string
	BusinessType         BusinessType
	RegistrationNumber   string
	IncorporationCountry shared.Country
	OperatingCountries   []shared.Country
	MCC                  shared.MCC
	Description          string
	// ClassifierMCC and ClassifierConfidencePercent are the description classifier's output,
	// computed before evaluation. Percent as an integer, not a float: a threshold comparison
	// against 0.4 that is sometimes 0.39999 is a rule nobody can reproduce.
	ClassifierMCC               shared.MCC
	ClassifierConfidencePercent int
	Website                     string
	SettlementCurrency          money.Currency
	DataResidencyRegion         string
	HandlesCardDataDirectly     bool
	SAQType                     string
}

// Subject is everything L2 evaluates: the submission, the vendor decisions, and the instant
// the evaluation is anchored to.
type Subject struct {
	Profile           Profile
	Principals        []Principal
	TaxIdentifiers    []TaxIdentifier
	BankAccounts      []BankAccount
	ProcessingProfile ProcessingProfile
	Vendor            VendorResults
	Documents         Documents
	// Now is the injected clock reading; no rule calls time.Now.
	Now time.Time
}

// Documents holds the compliance artefacts whose existence, not content, is validated.
type Documents struct {
	Attestation Document
	PCISAQ      Document
}

// Deps is the tenant's policy: the ceilings, allowlists and thresholds that make the same
// submission acceptable on one platform and not on another. Pure data — no repository, no
// vendor client.
type Deps struct {
	// SupportedCountries is where this tenant may onboard at all.
	SupportedCountries []shared.Country
	// LicensedCountries is where this tenant is licensed to operate. A superset test against
	// the merchant's declared operating countries.
	LicensedCountries []shared.Country
	// SanctionedCountries is the platform's versioned sanctions list.
	SanctionedCountries []shared.Country
	// ProhibitedMCCs beyond the domain's own prohibited set, and MCCExceptions the tenant holds.
	ProhibitedMCCs []shared.MCC
	MCCExceptions  []shared.MCC
	// HighRiskMCCs require a rolling reserve.
	HighRiskMCCs []shared.MCC
	// TaxIDRequiredKind maps a country to the identifier kind it mandates.
	TaxIDRequiredKind map[shared.Country]string
	// PostalCodeCountries are the countries whose addresses must carry a postal code. Not every
	// country uses one, and demanding one from those that do not is how an onboarding form
	// becomes impossible to complete.
	PostalCodeCountries []shared.Country
	// MonthlyVolumeCeiling is the tenant tier's declared-volume limit.
	MonthlyVolumeCeiling money.Money
	// PermittedResidencyRegions is the tenant's data-residency allowlist.
	PermittedResidencyRegions []string
	// MinClassifierConfidencePercent is the MCC classifier agreement floor (40 = 0.4).
	MinClassifierConfidencePercent int
	// AdverseMediaThreshold, RiskAutoDeclineAt and RiskReviewAt are the risk bands.
	AdverseMediaThreshold int
	RiskAutoDeclineAt     int
	RiskReviewAt          int
	// KYCRequiresIDDocument records whether the tenant's KYC vendor demands a document upload.
	KYCRequiresIDDocument bool
	// MinPrincipalAgeYears is the adulthood threshold, 18 everywhere the platform operates.
	MinPrincipalAgeYears int
}

// DefaultDeps returns the platform defaults from docs/validation-plane.md §3.2.
func DefaultDeps() Deps {
	return Deps{
		MinClassifierConfidencePercent: 40,
		AdverseMediaThreshold:          50,
		RiskAutoDeclineAt:              85,
		RiskReviewAt:                   60,
		MinPrincipalAgeYears:           18,
		TaxIDRequiredKind: map[shared.Country]string{
			"US": "EIN", "GB": "VAT", "DE": "VAT", "FR": "VAT", "NL": "VAT",
			"IE": "VAT", "ES": "VAT", "IT": "VAT", "AU": "ABN",
		},
		PostalCodeCountries: []shared.Country{
			"US", "GB", "DE", "FR", "NL", "IE", "ES", "IT", "AU", "CA", "JP", "SE", "PL",
		},
	}
}
