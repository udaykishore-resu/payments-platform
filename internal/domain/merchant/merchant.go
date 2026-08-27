package merchant

import (
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Merchant is the aggregate root for a business onboarded under a tenant.
//
// What it owns: identity, lifecycle status, the business profile, bank accounts, beneficial
// owners, and the compliance attestations that gate activation.
//
// What it deliberately does not own: the merchant's *configuration* (currencies, routing,
// risk limits) and its *gateway connections*. Those are separate aggregates in separate
// contexts — configuration because it is versioned and rolled back independently of the
// merchant's lifecycle, and gateway connections because they have their own provisioning
// lifecycle and a merchant can gain or lose one without changing status. Putting all three in
// one aggregate would create a transactional boundary spanning three different rates of change
// and force a config edit to contend with a lifecycle transition.
type Merchant struct {
	id       shared.MerchantID
	tenantID shared.TenantID

	legalName   string
	displayName string
	// externalRef is the tenant's own identifier for this merchant. It is unique within a
	// tenant and is what lets a tenant's systems find a merchant without storing our IDs.
	externalRef string

	status      Status
	version     shared.Version
	environment shared.Environment

	profile      BusinessProfile
	bankAccounts []BankAccount
	principals   []Principal

	kycStatus       KYCStatus
	kycProviderRef  string
	kycCompletedAt  *time.Time
	kycExpiresAt    *time.Time
	riskRating      RiskRating
	attestations    []ComplianceAttestation
	certificationID string

	suspensionReason SuspensionReason
	suspendedAt      *time.Time
	statusReason     string

	// activeConfigVersion points at the configuration version currently in force. The
	// configuration document itself lives in BC-5; this is the reference the data plane
	// resolves.
	activeConfigVersion int

	createdAt   time.Time
	updatedAt   time.Time
	activatedAt *time.Time

	events []Event
}

// RiskRating is the merchant's assessed risk band, which drives limits, reserve requirements
// and monitoring frequency.
type RiskRating string

const (
	RiskLow      RiskRating = "LOW"
	RiskStandard RiskRating = "STANDARD"
	RiskElevated RiskRating = "ELEVATED"
	RiskHigh     RiskRating = "HIGH"
)

// IsValid reports whether r is a known rating.
func (r RiskRating) IsValid() bool {
	switch r {
	case RiskLow, RiskStandard, RiskElevated, RiskHigh:
		return true
	default:
		return false
	}
}

// BusinessProfile is the KYB-relevant description of the business.
type BusinessProfile struct {
	LegalEntityType    string
	RegistrationNumber string
	TaxID              string
	// TaxIDLast4 is what is safe to display and log; the full TaxID is encrypted at rest and
	// is never returned by the read API.
	TaxIDLast4        string
	IncorporationDate *time.Time
	Country           shared.Country
	AddressLine1      string
	AddressLine2      string
	City              string
	Region            string
	PostalCode        string
	WebsiteURL        string
	SupportEmail      string
	SupportPhone      string
	MCC               shared.MCC
	Description       string
	// ExpectedMonthlyVolume and ExpectedAverageTicket size the risk assessment and the initial
	// limits. A merchant whose actual volume diverges by an order of magnitude from what they
	// declared is a monitoring signal, which is why the declared figure is retained.
	ExpectedMonthlyVolume money.Money
	ExpectedAverageTicket money.Money
}

// BankAccount is a settlement destination.
//
// Note what is stored: a token or a masked reference, never a full account number in the clear.
// The full number goes to the gateway during provisioning and to the encrypted store; the
// platform's own reads see the mask.
type BankAccount struct {
	ID            string
	Country       shared.Country
	Currency      money.Currency
	HolderName    string
	AccountLast4  string
	RoutingLast4  string
	IBANLast4     string
	SecretRef     string // reference into the secrets store for the full details
	Status        BankAccountStatus
	ValidationRef string
	ValidatedAt   *time.Time
	IsDefault     bool
	FailureReason string
}

// BankAccountStatus tracks micro-deposit or instant-verification progress.
type BankAccountStatus string

const (
	BankUnverified       BankAccountStatus = "UNVERIFIED"
	BankPendingVerify    BankAccountStatus = "PENDING_VERIFICATION"
	BankVerified         BankAccountStatus = "VERIFIED"
	BankVerificationFail BankAccountStatus = "VERIFICATION_FAILED"
)

// Principal is a beneficial owner, director or authorised representative.
//
// Personal data here is minimised to what KYB legally requires. Notably there is no date of
// birth in the clear and no government ID number: both go straight to the KYC provider during
// verification and are referenced afterwards by the provider's own identifier.
type Principal struct {
	ID              string
	Role            PrincipalRole
	FirstName       string
	LastName        string
	OwnershipPct    int
	Country         shared.Country
	VerificationRef string
	Verified        bool
}

// PrincipalRole names a principal's relationship to the business.
type PrincipalRole string

const (
	RoleBeneficialOwner PrincipalRole = "BENEFICIAL_OWNER"
	RoleDirector        PrincipalRole = "DIRECTOR"
	RoleRepresentative  PrincipalRole = "AUTHORISED_REPRESENTATIVE"
	RoleExecutive       PrincipalRole = "EXECUTIVE"
)

// ComplianceAttestation records a compliance obligation being met, with an expiry. Expiry is
// mandatory: an attestation without one silently becomes stale, and a stale attestation is
// indistinguishable from a missing one at audit time.
type ComplianceAttestation struct {
	Type       string
	Reference  string
	AttestedBy string
	AttestedAt time.Time
	ExpiresAt  time.Time
	DocumentID string
}

// IsCurrent reports whether the attestation is still valid at t.
func (a ComplianceAttestation) IsCurrent(t time.Time) bool { return t.Before(a.ExpiresAt) }

// NewParams are the inputs to registering a merchant.
type NewParams struct {
	TenantID    shared.TenantID
	LegalName   string
	DisplayName string
	ExternalRef string
	Environment shared.Environment
	Profile     BusinessProfile
}

// New registers a merchant in CREATED.
//
// The checks here are the ones that are true of every merchant in every tenant. Tenant-specific
// policy (which MCCs this tenant may onboard, which countries) is a validation-plane L2 concern
// evaluated with the tenant's configuration as an input, and is deliberately not hard-coded
// into the constructor.
func New(p NewParams, clock shared.Clock) (*Merchant, error) {
	if p.TenantID.IsZero() {
		return nil, apierror.New(apierror.CodeMissingTenantContext, "merchant requires a tenant")
	}
	legal := strings.TrimSpace(p.LegalName)
	if legal == "" {
		return nil, apierror.New(apierror.CodeValidationFailed, "legal name is required").
			WithDetail(apierror.Detail{Field: "legalName", Code: "REQUIRED",
				Message: "the registered legal name of the business", RuleID: "L2.LEGAL_NAME_PRESENT"})
	}
	if len(legal) > 256 {
		return nil, apierror.New(apierror.CodeValidationFailed, "legal name exceeds 256 characters")
	}
	if !p.Environment.IsValid() {
		return nil, apierror.Newf(apierror.CodeValidationFailed, "invalid environment %q", p.Environment)
	}
	if !p.Profile.Country.IsValid() {
		return nil, apierror.New(apierror.CodeValidationFailed, "a valid business country is required").
			WithDetail(apierror.Detail{Field: "profile.country", Code: "INVALID_COUNTRY",
				Message: "must be an ISO 3166-1 alpha-2 code", RuleID: "L2.COUNTRY_IS_VALID_ISO"})
	}
	if prohibited, why := p.Profile.MCC.IsProhibited(); prohibited {
		return nil, apierror.Newf(apierror.CodeValidationFailed,
			"merchant category %s cannot be onboarded on this platform", p.Profile.MCC).
			WithDetail(apierror.Detail{Field: "profile.mcc", Code: "PROHIBITED_CATEGORY",
				Message: why, RuleID: "L2.MCC_NOT_PROHIBITED"})
	}

	now := clock.Now()
	display := strings.TrimSpace(p.DisplayName)
	if display == "" {
		display = legal
	}
	m := &Merchant{
		id:          shared.NewMerchantID(),
		tenantID:    p.TenantID,
		legalName:   legal,
		displayName: display,
		externalRef: strings.TrimSpace(p.ExternalRef),
		status:      StatusCreated,
		version:     1,
		environment: p.Environment,
		profile:     p.Profile,
		kycStatus:   KYCNotStarted,
		riskRating:  RiskStandard,
		createdAt:   now,
		updatedAt:   now,
	}
	m.raise(EventMerchantCreated, now, map[string]any{
		"legalName":   legal,
		"country":     string(p.Profile.Country),
		"mcc":         string(p.Profile.MCC),
		"environment": string(p.Environment),
	})
	return m, nil
}

// Accessors.

func (m *Merchant) ID() shared.MerchantID              { return m.id }
func (m *Merchant) TenantID() shared.TenantID          { return m.tenantID }
func (m *Merchant) LegalName() string                  { return m.legalName }
func (m *Merchant) DisplayName() string                { return m.displayName }
func (m *Merchant) ExternalRef() string                { return m.externalRef }
func (m *Merchant) Status() Status                     { return m.status }
func (m *Merchant) Version() shared.Version            { return m.version }
func (m *Merchant) Environment() shared.Environment    { return m.environment }
func (m *Merchant) Profile() BusinessProfile           { return m.profile }
func (m *Merchant) KYCStatus() KYCStatus               { return m.kycStatus }
func (m *Merchant) KYCProviderRef() string             { return m.kycProviderRef }
func (m *Merchant) KYCExpiresAt() *time.Time           { return m.kycExpiresAt }
func (m *Merchant) RiskRating() RiskRating             { return m.riskRating }
func (m *Merchant) CertificationID() string            { return m.certificationID }
func (m *Merchant) SuspensionReason() SuspensionReason { return m.suspensionReason }
func (m *Merchant) StatusReason() string               { return m.statusReason }
func (m *Merchant) ActiveConfigVersion() int           { return m.activeConfigVersion }
func (m *Merchant) CreatedAt() time.Time               { return m.createdAt }
func (m *Merchant) UpdatedAt() time.Time               { return m.updatedAt }
func (m *Merchant) ActivatedAt() *time.Time            { return m.activatedAt }
func (m *Merchant) BankAccounts() []BankAccount        { return append([]BankAccount(nil), m.bankAccounts...) }
func (m *Merchant) Principals() []Principal            { return append([]Principal(nil), m.principals...) }
func (m *Merchant) Attestations() []ComplianceAttestation {
	return append([]ComplianceAttestation(nil), m.attestations...)
}

// CanAcceptPayments reports whether this merchant may create new payments right now. It
// combines the lifecycle state with the KYC freshness check, because an ACTIVE merchant whose
// periodic re-verification lapsed must stop processing even though nothing moved its status.
func (m *Merchant) CanAcceptPayments(now time.Time) (bool, error) {
	if !m.status.CanAcceptPayments() {
		if m.status == StatusSuspended {
			return false, apierror.Newf(apierror.CodeMerchantSuspended,
				"merchant is suspended (%s)", m.suspensionReason)
		}
		return false, apierror.Newf(apierror.CodeMerchantNotActive,
			"merchant is in state %s and cannot accept payments", m.status)
	}
	if m.kycExpiresAt != nil && !now.Before(*m.kycExpiresAt) {
		return false, apierror.New(apierror.CodeKYCRequired,
			"the merchant's verification has expired and must be renewed before processing resumes").
			WithDetail(apierror.Detail{Field: "kycStatus", Code: "EXPIRED",
				Message: "periodic re-verification is overdue", RuleID: "L5.MERCHANT_KYC_CURRENT"})
	}
	return true, nil
}

// DefaultBankAccount returns the settlement account marked default, or the first verified one.
func (m *Merchant) DefaultBankAccount() *BankAccount {
	for i := range m.bankAccounts {
		if m.bankAccounts[i].IsDefault && m.bankAccounts[i].Status == BankVerified {
			return &m.bankAccounts[i]
		}
	}
	for i := range m.bankAccounts {
		if m.bankAccounts[i].Status == BankVerified {
			return &m.bankAccounts[i]
		}
	}
	return nil
}

// --- lifecycle transitions -----------------------------------------------------------------

// StartValidation moves CREATED → VALIDATING.
func (m *Merchant) StartValidation(clock shared.Clock) error {
	return m.transition(StatusValidating, "", clock, "", nil)
}

// FailValidation records that L2 validation rejected the submission.
func (m *Merchant) FailValidation(reason string, clock shared.Clock) error {
	return m.transition(StatusValidationFailed, reason, clock, EventMerchantValidationFailed,
		map[string]any{"reason": reason})
}

// PassValidation moves VALIDATING → KYC_PENDING and records the KYC submission.
func (m *Merchant) PassValidation(clock shared.Clock) error {
	if err := m.transition(StatusKYCPending, "", clock, EventMerchantValidated, nil); err != nil {
		return err
	}
	m.kycStatus = KYCInProgress
	return nil
}

// ApproveKYC records a successful verification with its expiry.
//
// The expiry is required. Verification that never expires is not verification; every
// jurisdiction the platform operates in requires periodic re-verification, and an attestation
// with no end date will eventually be presented at audit as evidence of a control that does not
// exist.
func (m *Merchant) ApproveKYC(providerRef string, expiresAt time.Time, rating RiskRating, clock shared.Clock) error {
	if providerRef == "" {
		return apierror.New(apierror.CodeValidationFailed, "a KYC provider reference is required")
	}
	now := clock.Now()
	if !expiresAt.After(now) {
		return apierror.New(apierror.CodeValidationFailed, "KYC expiry must be in the future")
	}
	if !rating.IsValid() {
		rating = RiskStandard
	}
	if err := m.transition(StatusKYCApproved, "", clock, EventMerchantKYCApproved, map[string]any{
		"providerRef": providerRef,
		"riskRating":  string(rating),
		"expiresAt":   expiresAt.UTC().Format(time.RFC3339),
	}); err != nil {
		return err
	}
	m.kycStatus = KYCApproved
	m.kycProviderRef = providerRef
	m.kycCompletedAt = &now
	m.kycExpiresAt = &expiresAt
	m.riskRating = rating
	return nil
}

// RejectKYC records a failed verification.
func (m *Merchant) RejectKYC(providerRef, reason string, clock shared.Clock) error {
	if err := m.transition(StatusKYCFailed, reason, clock, EventMerchantKYCFailed, map[string]any{
		"providerRef": providerRef,
		"reason":      reason,
	}); err != nil {
		return err
	}
	m.kycStatus = KYCRejected
	m.kycProviderRef = providerRef
	return nil
}

// ResubmitKYC returns a KYC_FAILED merchant to KYC_PENDING after they correct their submission.
func (m *Merchant) ResubmitKYC(clock shared.Clock) error {
	if err := m.transition(StatusKYCPending, "", clock, "", nil); err != nil {
		return err
	}
	m.kycStatus = KYCInProgress
	return nil
}

// AddBankAccount attaches a settlement account. The first account added becomes the default.
func (m *Merchant) AddBankAccount(acct BankAccount, clock shared.Clock) error {
	if acct.ID == "" {
		return apierror.New(apierror.CodeValidationFailed, "bank account requires an identifier")
	}
	if !acct.Country.IsValid() {
		return apierror.New(apierror.CodeValidationFailed, "bank account requires a valid country")
	}
	if !acct.Currency.IsSupported() {
		return apierror.Newf(apierror.CodeCurrencyNotSupported,
			"settlement currency %q is not supported", acct.Currency)
	}
	for _, existing := range m.bankAccounts {
		if existing.ID == acct.ID {
			return apierror.Newf(apierror.CodeValidationFailed, "bank account %s already exists", acct.ID)
		}
	}
	if len(m.bankAccounts) == 0 {
		acct.IsDefault = true
	}
	if acct.Status == "" {
		acct.Status = BankUnverified
	}
	m.bankAccounts = append(m.bankAccounts, acct)
	m.touch(clock.Now())
	return nil
}

// ValidateBankAccount records that a settlement account passed verification and, when all
// required accounts are verified, advances the lifecycle.
func (m *Merchant) ValidateBankAccount(accountID, validationRef string, clock shared.Clock) error {
	now := clock.Now()
	found := false
	for i := range m.bankAccounts {
		if m.bankAccounts[i].ID == accountID {
			m.bankAccounts[i].Status = BankVerified
			m.bankAccounts[i].ValidationRef = validationRef
			m.bankAccounts[i].ValidatedAt = &now
			m.bankAccounts[i].FailureReason = ""
			found = true
			break
		}
	}
	if !found {
		return apierror.Newf(apierror.CodeValidationFailed, "bank account %s not found", accountID)
	}
	if m.status != StatusKYCApproved && m.status != StatusBankValidationFailed {
		// Verifying an additional account after onboarding is legitimate and must not attempt
		// a lifecycle transition.
		m.touch(now)
		return nil
	}
	return m.transition(StatusBankValidated, "", clock, EventMerchantBankValidated, map[string]any{
		"bankAccountId": accountID,
	})
}

// FailBankValidation records that verification failed.
func (m *Merchant) FailBankValidation(accountID, reason string, clock shared.Clock) error {
	for i := range m.bankAccounts {
		if m.bankAccounts[i].ID == accountID {
			m.bankAccounts[i].Status = BankVerificationFail
			m.bankAccounts[i].FailureReason = reason
		}
	}
	return m.transition(StatusBankValidationFailed, reason, clock, EventMerchantBankValidationFailed,
		map[string]any{"bankAccountId": accountID, "reason": reason})
}

// StartProvisioning moves BANK_VALIDATED → GATEWAY_PROVISIONING.
func (m *Merchant) StartProvisioning(clock shared.Clock) error {
	return m.transition(StatusGatewayProvisioning, "", clock, "", nil)
}

// CompleteProvisioning moves GATEWAY_PROVISIONING → CONFIGURING.
func (m *Merchant) CompleteProvisioning(gatewayIDs []string, clock shared.Clock) error {
	return m.transition(StatusConfiguring, "", clock, EventMerchantGatewayProvisioned,
		map[string]any{"gateways": gatewayIDs})
}

// FailProvisioning records a provisioning failure.
func (m *Merchant) FailProvisioning(reason string, clock shared.Clock) error {
	return m.transition(StatusProvisioningFailed, reason, clock, EventMerchantProvisioningFailed,
		map[string]any{"reason": reason})
}

// ApplyConfiguration records the configuration version now in force and advances to sandbox
// validation.
func (m *Merchant) ApplyConfiguration(version int, clock shared.Clock) error {
	if version <= 0 {
		return apierror.New(apierror.CodeConfigurationInvalid, "configuration version must be positive")
	}
	if err := m.transition(StatusSandboxValidation, "", clock, "", nil); err != nil {
		return err
	}
	m.activeConfigVersion = version
	return nil
}

// SetActiveConfigVersion updates the in-force configuration without a lifecycle transition.
// Used when a merchant that is already ACTIVE publishes a new configuration version.
func (m *Merchant) SetActiveConfigVersion(version int, clock shared.Clock) error {
	if version <= 0 {
		return apierror.New(apierror.CodeConfigurationInvalid, "configuration version must be positive")
	}
	m.activeConfigVersion = version
	m.touch(clock.Now())
	return nil
}

// FailConfiguration records a configuration failure.
func (m *Merchant) FailConfiguration(reason string, clock shared.Clock) error {
	return m.transition(StatusConfigurationFailed, reason, clock, EventMerchantConfigurationFailed,
		map[string]any{"reason": reason})
}

// StartCertification moves SANDBOX_VALIDATION → CERTIFICATION.
func (m *Merchant) StartCertification(clock shared.Clock) error {
	return m.transition(StatusCertification, "", clock, "", nil)
}

// Approve records a passing certification report and a compliance sign-off.
//
// It requires the certification report reference, which is what makes "certified" an artifact
// rather than an opinion: PRODUCTION_READY is unreachable without a stored, signed report.
func (m *Merchant) Approve(certificationReportID, approvedBy string, clock shared.Clock) error {
	if certificationReportID == "" {
		return apierror.New(apierror.CodeCertificationFailed,
			"a passing certification report is required before approval")
	}
	if err := m.transition(StatusApproved, "", clock, EventMerchantCertified, map[string]any{
		"certificationReportId": certificationReportID,
		"approvedBy":            approvedBy,
	}); err != nil {
		return err
	}
	m.certificationID = certificationReportID
	return nil
}

// FailCertification records that the certification matrix did not pass.
func (m *Merchant) FailCertification(reason string, clock shared.Clock) error {
	return m.transition(StatusCertificationFailed, reason, clock, EventMerchantCertificationFailed,
		map[string]any{"reason": reason})
}

// RejectForCompliance records a compliance officer's rejection at the manual gate
// (amendment A-01). It is distinct from FailCertification: the integration works, the business
// decision was no.
func (m *Merchant) RejectForCompliance(reasonCode, detail, rejectedBy string, clock shared.Clock) error {
	if reasonCode == "" {
		return apierror.New(apierror.CodeValidationFailed,
			"a compliance rejection requires a reason code for the audit trail")
	}
	return m.transition(StatusComplianceRejected, detail, clock, EventMerchantComplianceRejected,
		map[string]any{"reasonCode": reasonCode, "detail": detail, "rejectedBy": rejectedBy})
}

// MarkProductionReady moves APPROVED → PRODUCTION_READY.
func (m *Merchant) MarkProductionReady(clock shared.Clock) error {
	return m.transition(StatusProductionReady, "", clock, "", nil)
}

// Activate is the final gate. It re-checks every precondition rather than trusting that the
// workflow got here legitimately, because this is the transition after which real money moves
// and it is the one an operator is most likely to try to force manually.
func (m *Merchant) Activate(clock shared.Clock) error {
	now := clock.Now()
	if m.kycStatus != KYCApproved {
		return apierror.New(apierror.CodeKYCRequired,
			"cannot activate: verification is not approved")
	}
	if m.kycExpiresAt == nil || !now.Before(*m.kycExpiresAt) {
		return apierror.New(apierror.CodeKYCRequired,
			"cannot activate: verification has no future expiry")
	}
	if m.DefaultBankAccount() == nil {
		return apierror.New(apierror.CodeValidationFailed,
			"cannot activate: no verified settlement account").
			WithDetail(apierror.Detail{Field: "bankAccounts", Code: "NONE_VERIFIED",
				Message: "at least one settlement account must be verified",
				RuleID:  "L2.SETTLEMENT_ACCOUNT_VERIFIED"})
	}
	if m.certificationID == "" {
		return apierror.New(apierror.CodeCertificationFailed,
			"cannot activate: no passing certification report is on file")
	}
	if m.activeConfigVersion <= 0 {
		return apierror.New(apierror.CodeConfigurationInvalid,
			"cannot activate: no configuration version is in force")
	}
	if err := m.hasCurrentAttestations(now); err != nil {
		return err
	}
	if err := m.transition(StatusActive, "", clock, EventMerchantActivated, map[string]any{
		"configVersion":   m.activeConfigVersion,
		"certificationId": m.certificationID,
	}); err != nil {
		return err
	}
	m.activatedAt = &now
	m.suspensionReason = ""
	m.suspendedAt = nil
	return nil
}

func (m *Merchant) hasCurrentAttestations(now time.Time) error {
	required := []string{"PCI_SAQ", "TERMS_ACCEPTANCE"}
	for _, want := range required {
		ok := false
		for _, a := range m.attestations {
			if a.Type == want && a.IsCurrent(now) {
				ok = true
				break
			}
		}
		if !ok {
			return apierror.Newf(apierror.CodeValidationFailed,
				"cannot activate: a current %s attestation is required", want).
				WithDetail(apierror.Detail{Field: "attestations", Code: "MISSING_OR_EXPIRED",
					Message: want + " is missing or expired", RuleID: "L2.COMPLIANCE_ATTESTATIONS_CURRENT"})
		}
	}
	return nil
}

// AddAttestation records a compliance obligation being met.
func (m *Merchant) AddAttestation(a ComplianceAttestation, clock shared.Clock) error {
	if a.Type == "" || a.AttestedBy == "" {
		return apierror.New(apierror.CodeValidationFailed,
			"an attestation requires a type and an attesting party")
	}
	if !a.ExpiresAt.After(a.AttestedAt) {
		return apierror.New(apierror.CodeValidationFailed,
			"an attestation must expire after it was made")
	}
	// Replace an existing attestation of the same type rather than accumulating: the current
	// one is what matters, and history is in the audit trail.
	for i := range m.attestations {
		if m.attestations[i].Type == a.Type {
			m.attestations[i] = a
			m.touch(clock.Now())
			return nil
		}
	}
	m.attestations = append(m.attestations, a)
	m.touch(clock.Now())
	return nil
}

// AddPrincipal attaches a beneficial owner or officer.
func (m *Merchant) AddPrincipal(p Principal, clock shared.Clock) error {
	if p.ID == "" || p.LastName == "" {
		return apierror.New(apierror.CodeValidationFailed, "a principal requires an identifier and a surname")
	}
	if p.OwnershipPct < 0 || p.OwnershipPct > 100 {
		return apierror.New(apierror.CodeValidationFailed, "ownership percentage must be between 0 and 100")
	}
	total := p.OwnershipPct
	for _, existing := range m.principals {
		if existing.ID == p.ID {
			return apierror.Newf(apierror.CodeValidationFailed, "principal %s already exists", p.ID)
		}
		total += existing.OwnershipPct
	}
	if total > 100 {
		return apierror.Newf(apierror.CodeValidationFailed,
			"declared ownership totals %d%%, which exceeds 100%%", total).
			WithDetail(apierror.Detail{Field: "principals", Code: "OWNERSHIP_EXCEEDS_100",
				Message: "beneficial ownership percentages must not sum above 100",
				RuleID:  "L2.OWNERSHIP_SUMS_CORRECTLY"})
	}
	m.principals = append(m.principals, p)
	m.touch(clock.Now())
	return nil
}

// Suspend stops the merchant accepting new payments. Refunds continue.
func (m *Merchant) Suspend(reason SuspensionReason, detail string, clock shared.Clock) error {
	now := clock.Now()
	if err := m.transition(StatusSuspended, detail, clock, EventMerchantSuspended, map[string]any{
		"reason": string(reason),
		"detail": detail,
	}); err != nil {
		return err
	}
	m.suspensionReason = reason
	m.suspendedAt = &now
	return nil
}

// Reinstate lifts a suspension. Suspensions whose reason requires operator review cannot be
// lifted by an automated caller; the actorIsOperator flag makes that requirement explicit at
// the call site rather than implicit in whoever happens to be calling.
func (m *Merchant) Reinstate(actorIsOperator bool, clock shared.Clock) error {
	if m.status != StatusSuspended {
		return apierror.Newf(apierror.CodeInvalidStateTransition,
			"merchant is in state %s, not SUSPENDED", m.status)
	}
	if m.suspensionReason.RequiresOperatorReviewToLift() && !actorIsOperator {
		return apierror.Newf(apierror.CodeForbidden,
			"a suspension for %s can only be lifted after operator review", m.suspensionReason).
			WithDetail(apierror.Detail{Field: "suspensionReason", Code: "REQUIRES_OPERATOR_REVIEW",
				Message: "this suspension reason cannot be cleared automatically",
				RuleID:  "L2.SUSPENSION_LIFT_AUTHORITY"})
	}
	if err := m.transition(StatusActive, "", clock, EventMerchantReinstated, map[string]any{
		"previousReason": string(m.suspensionReason),
	}); err != nil {
		return err
	}
	m.suspensionReason = ""
	m.suspendedAt = nil
	return nil
}

// Terminate permanently closes the merchant. The caller supplies openPayments because the
// aggregate cannot see the payment context; the check lives here so that the rule is stated in
// the domain rather than only in the use case.
func (m *Merchant) Terminate(reason string, openPayments int, clock shared.Clock) error {
	if openPayments > 0 {
		return apierror.Newf(apierror.CodeInvalidStateTransition,
			"cannot terminate: %d payments are still in a non-terminal state", openPayments).
			WithDetail(apierror.Detail{Field: "status", Code: "OPEN_PAYMENTS",
				Message: "settle, refund or void all open payments before terminating",
				RuleID:  "L7.TERMINATION_REQUIRES_NO_OPEN_PAYMENTS"})
	}
	return m.transition(StatusTerminated, reason, clock, EventMerchantTerminated,
		map[string]any{"reason": reason})
}

func (m *Merchant) transition(to Status, reason string, clock shared.Clock, evt EventType, payload map[string]any) error {
	if err := machine.Transition(m.status, to); err != nil {
		return err
	}
	now := clock.Now()
	m.status = to
	m.statusReason = reason
	m.touch(now)
	if evt != "" {
		m.raise(evt, now, payload)
	}
	return nil
}

func (m *Merchant) touch(now time.Time) {
	m.updatedAt = now
	m.version = m.version.Next()
}

func (m *Merchant) raise(t EventType, at time.Time, payload map[string]any) {
	m.events = append(m.events, Event{
		Type: t, MerchantID: m.id, TenantID: m.tenantID,
		Status: m.status, OccurredAt: at, Version: m.version, Payload: payload,
	})
}

// PendingEvents returns events raised in this unit of work.
func (m *Merchant) PendingEvents() []Event { return append([]Event(nil), m.events...) }

// DrainEvents returns and clears pending events.
func (m *Merchant) DrainEvents() []Event {
	out := m.events
	m.events = nil
	return out
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-31, FR-09, FR-13, FR-14.
//
// The merchant aggregate and its lifecycle guards, including the suspension that still permits
// money out and the termination that will not strand an in-flight payment
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
