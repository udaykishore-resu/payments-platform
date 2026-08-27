// Package merchant is the merchant registry bounded context's domain model (BC-2), together
// with the lifecycle state machine that the onboarding automation drives (BC-3).
//
// The lifecycle lives here rather than in the onboarding context deliberately: the merchant
// *is* the thing whose state changes, and putting the state machine next to the workflow that
// drives it would mean two contexts both believe they own merchant status. The workflow
// requests transitions; the aggregate decides whether they are legal.
package merchant

import "github.com/udaykishore-resu/payments-platform/internal/domain/shared"

// Status is the merchant lifecycle state (docs/spec/00-design-baseline.md §8).
type Status string

const (
	StatusCreated              Status = "CREATED"
	StatusValidating           Status = "VALIDATING"
	StatusValidationFailed     Status = "VALIDATION_FAILED"
	StatusKYCPending           Status = "KYC_PENDING"
	StatusKYCApproved          Status = "KYC_APPROVED"
	StatusKYCFailed            Status = "KYC_FAILED"
	StatusBankValidated        Status = "BANK_VALIDATED"
	StatusBankValidationFailed Status = "BANK_VALIDATION_FAILED"
	StatusGatewayProvisioning  Status = "GATEWAY_PROVISIONING"
	StatusProvisioningFailed   Status = "PROVISIONING_FAILED"
	StatusConfiguring          Status = "CONFIGURING"
	StatusConfigurationFailed  Status = "CONFIGURATION_FAILED"
	StatusSandboxValidation    Status = "SANDBOX_VALIDATION"
	StatusCertification        Status = "CERTIFICATION"
	StatusCertificationFailed  Status = "CERTIFICATION_FAILED"
	StatusComplianceRejected   Status = "COMPLIANCE_REJECTED"
	StatusApproved             Status = "APPROVED"
	StatusProductionReady      Status = "PRODUCTION_READY"
	StatusActive               Status = "ACTIVE"
	StatusSuspended            Status = "SUSPENDED"
	StatusTerminated           Status = "TERMINATED"
)

// AllStatuses is the complete state universe.
var AllStatuses = []Status{
	StatusCreated, StatusValidating, StatusValidationFailed,
	StatusKYCPending, StatusKYCApproved, StatusKYCFailed,
	StatusBankValidated, StatusBankValidationFailed,
	StatusGatewayProvisioning, StatusProvisioningFailed,
	StatusConfiguring, StatusConfigurationFailed,
	StatusSandboxValidation, StatusCertification, StatusCertificationFailed,
	StatusComplianceRejected, StatusApproved, StatusProductionReady,
	StatusActive, StatusSuspended, StatusTerminated,
}

var machine = shared.NewStateMachine("merchant", StatusCreated, AllStatuses,
	[]Status{StatusTerminated},
	[]shared.Transition[Status]{
		{From: StatusCreated, To: StatusValidating},
		{From: StatusCreated, To: StatusTerminated},

		{From: StatusValidating, To: StatusKYCPending},
		{From: StatusValidating, To: StatusValidationFailed},

		// A failed state is not a dead end. Every failure that a merchant can correct routes
		// back into the pipeline, because the alternative — forcing them to start a new
		// merchant record — loses the audit trail of what was wrong and duplicates the entity.
		{From: StatusValidationFailed, To: StatusValidating},
		{From: StatusValidationFailed, To: StatusTerminated},

		{From: StatusKYCPending, To: StatusKYCApproved},
		{From: StatusKYCPending, To: StatusKYCFailed},
		{From: StatusKYCFailed, To: StatusKYCPending},
		{From: StatusKYCFailed, To: StatusTerminated},

		{From: StatusKYCApproved, To: StatusBankValidated},
		{From: StatusKYCApproved, To: StatusBankValidationFailed},
		{From: StatusBankValidationFailed, To: StatusKYCApproved},
		{From: StatusBankValidationFailed, To: StatusTerminated},

		{From: StatusBankValidated, To: StatusGatewayProvisioning},

		{From: StatusGatewayProvisioning, To: StatusConfiguring},
		{From: StatusGatewayProvisioning, To: StatusProvisioningFailed},
		{From: StatusProvisioningFailed, To: StatusGatewayProvisioning},
		{From: StatusProvisioningFailed, To: StatusTerminated},

		{From: StatusConfiguring, To: StatusSandboxValidation},
		{From: StatusConfiguring, To: StatusConfigurationFailed},
		{From: StatusConfigurationFailed, To: StatusConfiguring},
		{From: StatusConfigurationFailed, To: StatusTerminated},

		{From: StatusSandboxValidation, To: StatusCertification},
		{From: StatusSandboxValidation, To: StatusConfigurationFailed},

		// Amendment A-01: the manual compliance gate needs an exit that is neither approval nor
		// a lie about the integration having failed.
		{From: StatusCertification, To: StatusApproved},
		{From: StatusCertification, To: StatusCertificationFailed},
		{From: StatusCertification, To: StatusComplianceRejected},
		{From: StatusCertificationFailed, To: StatusCertification},
		{From: StatusCertificationFailed, To: StatusConfiguring},
		{From: StatusCertificationFailed, To: StatusTerminated},
		{From: StatusComplianceRejected, To: StatusConfiguring},
		{From: StatusComplianceRejected, To: StatusKYCPending},
		{From: StatusComplianceRejected, To: StatusTerminated},

		{From: StatusApproved, To: StatusProductionReady},
		{From: StatusApproved, To: StatusSuspended},

		{From: StatusProductionReady, To: StatusActive},
		{From: StatusProductionReady, To: StatusSuspended},

		{From: StatusActive, To: StatusSuspended},
		{From: StatusActive, To: StatusTerminated},

		{From: StatusSuspended, To: StatusActive},
		{From: StatusSuspended, To: StatusTerminated},
	})

// Machine exposes the merchant lifecycle state machine.
func Machine() *shared.StateMachine[Status] { return machine }

// String satisfies fmt.Stringer.
func (s Status) String() string { return string(s) }

// IsKnown reports whether s is a state this binary understands.
func (s Status) IsKnown() bool { return machine.IsKnown(s) }

// IsTerminal reports whether the merchant can never change state again.
func (s Status) IsTerminal() bool { return machine.IsTerminal(s) }

// CanAcceptPayments reports whether new payments may be created for a merchant in this state.
//
// Exactly one state qualifies. In particular SUSPENDED does not: a suspended merchant is
// stopped from taking money, not from returning it.
func (s Status) CanAcceptPayments() bool { return s == StatusActive }

// CanIssueRefunds reports whether refunds and voids may still be processed.
//
// This is deliberately broader than CanAcceptPayments. A merchant suspended for a risk breach
// still has customers owed money, and a platform that blocks refunds during a suspension
// converts a merchant problem into a consumer-harm problem and, in several jurisdictions, a
// regulatory one. Only termination stops refunds, and termination requires that no payment is
// in a non-terminal state.
func (s Status) CanIssueRefunds() bool {
	switch s {
	case StatusActive, StatusSuspended, StatusProductionReady, StatusApproved:
		return true
	default:
		return false
	}
}

// IsFailureState reports whether the merchant is parked awaiting a correction.
func (s Status) IsFailureState() bool {
	switch s {
	case StatusValidationFailed, StatusKYCFailed, StatusBankValidationFailed,
		StatusProvisioningFailed, StatusConfigurationFailed, StatusCertificationFailed,
		StatusComplianceRejected:
		return true
	default:
		return false
	}
}

// IsOnboarding reports whether the merchant is still moving through the onboarding pipeline.
// Used to decide whether an onboarding workflow should exist for this merchant.
func (s Status) IsOnboarding() bool {
	switch s {
	case StatusActive, StatusSuspended, StatusTerminated:
		return false
	default:
		return true
	}
}

// KYCStatus is the outcome of know-your-business verification, kept separately from the
// lifecycle because a merchant can be re-verified while ACTIVE (periodic refresh, adverse
// media hit) without leaving the ACTIVE state.
type KYCStatus string

const (
	KYCNotStarted   KYCStatus = "NOT_STARTED"
	KYCInProgress   KYCStatus = "IN_PROGRESS"
	KYCApproved     KYCStatus = "APPROVED"
	KYCRejected     KYCStatus = "REJECTED"
	KYCReviewNeeded KYCStatus = "MANUAL_REVIEW"
	KYCExpired      KYCStatus = "EXPIRED"
)

// String satisfies fmt.Stringer.
func (k KYCStatus) String() string { return string(k) }

// IsSatisfied reports whether verification currently permits processing.
func (k KYCStatus) IsSatisfied() bool { return k == KYCApproved }

// SuspensionReason records why a merchant was suspended. It is a closed set because the reason
// drives what the merchant is told, what an operator must do to lift it, and whether the
// suspension is reportable.
type SuspensionReason string

const (
	SuspendRiskBreach       SuspensionReason = "RISK_THRESHOLD_BREACHED"
	SuspendComplianceExpiry SuspensionReason = "COMPLIANCE_DOCUMENT_EXPIRED"
	SuspendKYCExpiry        SuspensionReason = "KYC_REVERIFICATION_REQUIRED"
	SuspendGatewayRevoked   SuspensionReason = "GATEWAY_ACCESS_REVOKED"
	SuspendChargebackRate   SuspensionReason = "EXCESSIVE_CHARGEBACK_RATE"
	SuspendMerchantRequest  SuspensionReason = "REQUESTED_BY_MERCHANT"
	SuspendOperatorAction   SuspensionReason = "OPERATOR_ACTION"
	SuspendNonPayment       SuspensionReason = "NON_PAYMENT_OF_FEES"
	SuspendSanctionsHit     SuspensionReason = "SANCTIONS_SCREENING_HIT"
)

// String satisfies fmt.Stringer.
func (r SuspensionReason) String() string { return string(r) }

// RequiresOperatorReviewToLift reports whether an automated process may lift this suspension.
// Sanctions and compliance suspensions never lift automatically; a risk-threshold suspension
// may, once the underlying metric recovers.
func (r SuspensionReason) RequiresOperatorReviewToLift() bool {
	switch r {
	case SuspendRiskBreach, SuspendNonPayment, SuspendMerchantRequest:
		return false
	default:
		return true
	}
}
