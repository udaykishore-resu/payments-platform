package tenant

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

// EventType is the versioned type name of a tenancy domain event.
//
// The version is in the name rather than in a field, for the same reason as everywhere else in
// this platform: a consumer subscribed to `tenant.suspended.v1` cannot silently receive a v2
// payload with different semantics. Putting the version in a field puts the check on every
// consumer, and the consumer that forgets fails in a way that looks like a data bug.
type EventType string

const (
	// EventTenantCreated records a new tenant. Consumed by provisioning (which creates the
	// schema, the cache namespace and, for the siloed tier, the KMS key), by billing and by audit.
	EventTenantCreated EventType = "tenant.created.v1"

	// EventTenantSuspended stops every merchant under the tenant. Consumed by the data plane as
	// an urgent cache invalidation: a suspended tenant that keeps processing for the ordinary
	// 30-second staleness window is 30 seconds of money moving that should not be moving, and
	// unlike an ordinary configuration change that window is not an acceptable trade.
	EventTenantSuspended EventType = "tenant.suspended.v1"

	// EventTenantReinstated lifts a suspension.
	EventTenantReinstated EventType = "tenant.reinstated.v1"

	// EventTenantTerminated ends the relationship. Consumed by everything, including the
	// retention scheduler that begins the deletion clock — which is why it is a distinct event
	// from suspension rather than a suspension with a flag.
	EventTenantTerminated EventType = "tenant.terminated.v1"

	// EventTenantQuotasUpdated records a change to the tenant's resource limits. Consumed by the
	// rate limiter and the bulkhead, which hold the limits in memory and would otherwise not
	// learn about a change until their next scheduled refresh — during which a tenant that just
	// bought more capacity is still being throttled at the old figure.
	EventTenantQuotasUpdated EventType = "tenant.quotas_updated.v1"

	// EventTenantGatewayEnabled records a gateway entitlement grant.
	EventTenantGatewayEnabled EventType = "tenant.gateway_enabled.v1"

	// EventTenantGatewayDisabled records a gateway entitlement revocation. Because the tenant is
	// the ceiling, this one event invalidates every merchant configuration under the tenant that
	// names the gateway, without any of those documents being rewritten.
	EventTenantGatewayDisabled EventType = "tenant.gateway_disabled.v1"

	// EventTenantAPIClientCreated records a new machine identity. Audited because the creation of
	// a credential is a control-relevant act regardless of what it is later used for.
	EventTenantAPIClientCreated EventType = "tenant.api_client_created.v1"

	// EventTenantAPIClientRotated records a credential rotation, carrying the overlap deadline.
	// This is the evidence for the ≤90-day rotation control: an auditor asking "prove credentials
	// are rotated" is answered from this event stream rather than from a spreadsheet.
	EventTenantAPIClientRotated EventType = "tenant.api_client_credentials_rotated.v1"

	// EventTenantAPIClientRevoked records a permanent credential revocation. An urgent
	// invalidation: a revoked credential that any edge process still accepts is the one failure
	// this event exists to prevent.
	EventTenantAPIClientRevoked EventType = "tenant.api_client_revoked.v1"
)

// AllEventTypes is the complete set this context publishes. The CI check
// TestEveryEventTypeHasASchema asserts each has a JSON Schema in api/events/.
var AllEventTypes = []EventType{
	EventTenantCreated, EventTenantSuspended, EventTenantReinstated, EventTenantTerminated,
	EventTenantQuotasUpdated, EventTenantGatewayEnabled, EventTenantGatewayDisabled,
	EventTenantAPIClientCreated, EventTenantAPIClientRotated, EventTenantAPIClientRevoked,
}

// String satisfies fmt.Stringer.
func (t EventType) String() string { return string(t) }

// Topic returns the Kafka topic this event is published to.
//
// One topic for the whole context, keyed by tenant ID, which is what gives per-tenant ordering.
// API client events ride on it rather than on a topic of their own precisely because that
// ordering matters across the two: a consumer that saw `api_client_created` after
// `tenant_suspended` would enable a credential for a tenant that is stopped, and only a shared
// partition key makes that ordering guaranteed rather than likely.
func (t EventType) Topic() string { return "pp.tenants.tenant.v1" }

// IsUrgentInvalidation reports whether the event must bypass the data plane's normal refresh
// cadence. Three qualify, and each for the same reason: between the event and the refresh, the
// platform would be honouring an authorization somebody has just taken away.
func (t EventType) IsUrgentInvalidation() bool {
	switch t {
	case EventTenantSuspended, EventTenantTerminated, EventTenantAPIClientRevoked:
		return true
	default:
		return false
	}
}

// Event is a domain event raised by an aggregate in this context.
//
// As in every other context, this is not the wire envelope: CloudEvents fields, trace context and
// the schema reference are assembled in internal/events. The domain raises a plain record and
// knows nothing about Kafka or JSON, which is what lets every test here run with no broker.
type Event struct {
	Type     EventType
	TenantID shared.TenantID
	// APIClientID is set on the three api_client events and zero on the rest.
	APIClientID shared.APIClientID
	// Status is the tenant's status after the change, carried so a consumer can act on the event
	// alone without a read-back against a control plane that may be mid-deploy.
	Status     Status
	OccurredAt time.Time
	Version    shared.Version
	Payload    map[string]any
}

// AggregateID returns the partition key: always the tenant, never the API client.
//
// Keying an API client event by the client would put it on a different partition from the tenant
// suspension that ought to precede it, and per-partition ordering is the only ordering guarantee
// this platform offers (baseline §13.3). The cost is that a very large tenant's events all land
// on one partition; that is acceptable here because tenancy events are rare — measured in
// hundreds per day across the whole platform — and correctness under reordering is not.
func (e Event) AggregateID() string { return e.TenantID.String() }

// Topic returns the topic for this event.
func (e Event) Topic() string { return e.Type.Topic() }

// RequiresOperatorAttention reports whether the event should raise an operational signal rather
// than merely being recorded. Suspension and termination both stop a paying customer from
// transacting, and neither should happen without somebody knowing.
func (e Event) RequiresOperatorAttention() bool {
	return e.Type == EventTenantSuspended || e.Type == EventTenantTerminated
}
