package merchant

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// RehydrateParams carries the persisted state of a Merchant back into the aggregate.
//
// It exists for the same reason payment.RehydrateParams does: the aggregate's fields are
// unexported so that no repository can assemble a Merchant the constructor and the lifecycle
// methods would have refused, and this is the single reviewed doorway back in. Without it a
// repository would have to either export the fields — which would let any caller move a merchant
// to ACTIVE without passing the activation guard — or replay the lifecycle from CREATED on every
// read, which is both slow and wrong, since replaying raises the events again.
type RehydrateParams struct {
	ID          shared.MerchantID
	TenantID    shared.TenantID
	LegalName   string
	DisplayName string
	ExternalRef string
	Status      Status
	Version     shared.Version
	Environment shared.Environment

	Profile      BusinessProfile
	BankAccounts []BankAccount
	Principals   []Principal
	Attestations []ComplianceAttestation

	KYCStatus      KYCStatus
	KYCProviderRef string
	KYCCompletedAt *time.Time
	KYCExpiresAt   *time.Time
	RiskRating     RiskRating

	CertificationID     string
	SuspensionReason    SuspensionReason
	SuspendedAt         *time.Time
	StatusReason        string
	ActiveConfigVersion int

	CreatedAt   time.Time
	UpdatedAt   time.Time
	ActivatedAt *time.Time
}

// Rehydrate reconstructs a Merchant from persisted state.
//
// It validates the status rather than trusting the row. A status this binary does not recognise
// means a deployment rolled back over data written by a newer one, and the specific danger is
// that coercing an unknown value to something plausible would most likely coerce it towards
// ACTIVE — re-enabling payment processing for a merchant somebody deliberately stopped.
func Rehydrate(p RehydrateParams) (*Merchant, error) {
	if !p.Status.IsKnown() {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"merchant %s has unknown status %q; this row may have been written by a newer "+
				"version of the service", p.ID, p.Status)
	}
	if p.RiskRating != "" && !p.RiskRating.IsValid() {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"merchant %s has unknown risk rating %q", p.ID, p.RiskRating)
	}
	return &Merchant{
		id:          p.ID,
		tenantID:    p.TenantID,
		legalName:   p.LegalName,
		displayName: p.DisplayName,
		externalRef: p.ExternalRef,
		status:      p.Status,
		version:     p.Version,
		environment: p.Environment,
		profile:     p.Profile,
		// Copied on the way in, so the caller's slice cannot be mutated afterwards to change a
		// merchant's beneficial owners without going through a method.
		bankAccounts:        append([]BankAccount(nil), p.BankAccounts...),
		principals:          append([]Principal(nil), p.Principals...),
		attestations:        append([]ComplianceAttestation(nil), p.Attestations...),
		kycStatus:           p.KYCStatus,
		kycProviderRef:      p.KYCProviderRef,
		kycCompletedAt:      p.KYCCompletedAt,
		kycExpiresAt:        p.KYCExpiresAt,
		riskRating:          p.RiskRating,
		certificationID:     p.CertificationID,
		suspensionReason:    p.SuspensionReason,
		suspendedAt:         p.SuspendedAt,
		statusReason:        p.StatusReason,
		activeConfigVersion: p.ActiveConfigVersion,
		createdAt:           p.CreatedAt,
		updatedAt:           p.UpdatedAt,
		activatedAt:         p.ActivatedAt,
	}, nil
}

// KYCCompletedAt returns when verification concluded, or nil if it has not.
//
// It is exported because the repository must persist it: without it, a merchant reloaded after a
// restart has no record of when its KYC decision was made, and the refresh interval in the
// compliance policy is computed from that instant.
func (m *Merchant) KYCCompletedAt() *time.Time { return m.kycCompletedAt }

// SuspendedAt returns when the merchant was suspended, or nil if it is not suspended.
//
// Persisted for the same reason: suspension duration is an operational metric and a contractual
// one, and it cannot be recovered from the status alone.
func (m *Merchant) SuspendedAt() *time.Time { return m.suspendedAt }
