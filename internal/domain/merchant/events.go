package merchant

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

// EventType is the versioned type name of a merchant domain event.
type EventType string

const (
	EventMerchantCreated              EventType = "merchant.created.v1"
	EventMerchantValidated            EventType = "merchant.validated.v1"
	EventMerchantValidationFailed     EventType = "merchant.validation_failed.v1"
	EventMerchantKYCApproved          EventType = "merchant.kyc_approved.v1"
	EventMerchantKYCFailed            EventType = "merchant.kyc_failed.v1"
	EventMerchantBankValidated        EventType = "merchant.bank_validated.v1"
	EventMerchantBankValidationFailed EventType = "merchant.bank_validation_failed.v1"
	EventMerchantGatewayProvisioned   EventType = "merchant.gateway_provisioned.v1"
	EventMerchantProvisioningFailed   EventType = "merchant.provisioning_failed.v1"
	EventMerchantConfigurationFailed  EventType = "merchant.configuration_failed.v1"
	EventMerchantCertified            EventType = "merchant.certified.v1"
	EventMerchantCertificationFailed  EventType = "merchant.certification_failed.v1"
	EventMerchantComplianceRejected   EventType = "merchant.compliance_rejected.v1"
	EventMerchantActivated            EventType = "merchant.activated.v1"
	EventMerchantSuspended            EventType = "merchant.suspended.v1"
	EventMerchantReinstated           EventType = "merchant.reinstated.v1"
	EventMerchantTerminated           EventType = "merchant.terminated.v1"
)

// AllEventTypes is the complete set this context publishes.
var AllEventTypes = []EventType{
	EventMerchantCreated, EventMerchantValidated, EventMerchantValidationFailed,
	EventMerchantKYCApproved, EventMerchantKYCFailed,
	EventMerchantBankValidated, EventMerchantBankValidationFailed,
	EventMerchantGatewayProvisioned, EventMerchantProvisioningFailed,
	EventMerchantConfigurationFailed, EventMerchantCertified,
	EventMerchantCertificationFailed, EventMerchantComplianceRejected,
	EventMerchantActivated, EventMerchantSuspended, EventMerchantReinstated,
	EventMerchantTerminated,
}

// String satisfies fmt.Stringer.
func (t EventType) String() string { return string(t) }

// Topic returns the Kafka topic. Keyed by merchant ID so that a merchant's lifecycle events
// are strictly ordered relative to each other — which matters because a consumer that sees
// `activated` before `certified` would make the wrong decision about whether to enable traffic.
func (t EventType) Topic() string { return "pp.merchants.merchant.v1" }

// IsCacheInvalidating reports whether the data plane must invalidate its merchant snapshot on
// receiving this event.
//
// Suspension and termination are the two that carry real urgency: a suspended merchant that
// keeps processing for up to the 30-second staleness window is processing money it should not
// be. Those two are consumed on a priority path with an explicit invalidation, rather than
// waiting for the snapshot's natural refresh.
func (t EventType) IsCacheInvalidating() bool {
	switch t {
	case EventMerchantActivated, EventMerchantSuspended, EventMerchantReinstated,
		EventMerchantTerminated:
		return true
	default:
		return false
	}
}

// IsUrgentInvalidation reports whether the event must bypass the normal refresh cadence.
func (t EventType) IsUrgentInvalidation() bool {
	return t == EventMerchantSuspended || t == EventMerchantTerminated
}

// Event is a domain event raised by the Merchant aggregate.
type Event struct {
	Type       EventType
	MerchantID shared.MerchantID
	TenantID   shared.TenantID
	Status     Status
	OccurredAt time.Time
	Version    shared.Version
	Payload    map[string]any
}

// AggregateID returns the partition key.
func (e Event) AggregateID() string { return e.MerchantID.String() }
