package gateway

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

// EventType is the versioned type name of a gateway domain event, matching the catalog in
// docs/spec/00-design-baseline.md §13.2 and the JSON Schemas in api/events/.
//
// The version is in the name rather than in a field, for the same reason it is in BC-6: a
// consumer subscribed to `gateway.certified.v1` cannot accidentally receive a v2 payload it does
// not understand. A version field puts the check on every consumer, and the one consumer that
// forgets fails in a way that looks like a data bug rather than a contract change.
type EventType string

const (
	// EventGatewayProvisioned records that a sub-merchant account now exists at the gateway and
	// that credentials for it are in the secret store. Onboarding waits on this; without it the
	// workflow cannot distinguish "provisioning succeeded" from "provisioning is still running".
	EventGatewayProvisioned EventType = "gateway.provisioned.v1"

	// EventGatewayCertified records a passing certification run. This is the event that unblocks
	// the merchant's activation, so it carries the report ID: an activation whose evidence cannot
	// be produced on request is an audit finding.
	EventGatewayCertified EventType = "gateway.certified.v1"

	// EventGatewayCertificationFailed records a failing run. It is a first-class event rather
	// than the absence of a success, because the onboarding workflow, the operator console and
	// the merchant notification all need to react to it, and "nothing arrived" is not something
	// a consumer can react to.
	EventGatewayCertificationFailed EventType = "gateway.certification_failed.v1"

	// EventGatewayCredentialsRotated records a credential rotation. Consumed by the audit sink
	// (rotation is an evidenced control, ≤90 days) and by any cache holding a credential handle.
	// The payload carries references, never material.
	EventGatewayCredentialsRotated EventType = "gateway.credentials_rotated.v1"

	// EventGatewayConnectionRevoked records that a connection is permanently dead. Consumed as an
	// urgent invalidation: a revoked connection whose credentials linger in a data-plane cache
	// for the usual 30-second staleness window is 30 seconds of traffic on credentials somebody
	// deliberately killed.
	EventGatewayConnectionRevoked EventType = "gateway.connection_revoked.v1"

	// EventGatewayHealthChanged records a health state transition. Baseline §10 calls this the
	// feedback loop from Observability back into Control: the routing engine consumes it to stop
	// selecting a gateway whose circuit has opened, and alerting consumes it to page.
	EventGatewayHealthChanged EventType = "gateway.health_changed.v1"
)

// AllEventTypes is the complete set this context publishes. The CI check
// TestEveryEventTypeHasASchema asserts each has a JSON Schema in api/events/.
var AllEventTypes = []EventType{
	EventGatewayProvisioned, EventGatewayCertified, EventGatewayCertificationFailed,
	EventGatewayCredentialsRotated, EventGatewayConnectionRevoked, EventGatewayHealthChanged,
}

// String satisfies fmt.Stringer.
func (t EventType) String() string { return string(t) }

// Topic returns the Kafka topic this event is published to.
//
// Health is split onto its own topic and it is not an arbitrary split. Health events are
// high-frequency, are compacted (only the current state matters to a consumer starting up), and
// have a one-day retention; connection lifecycle events are low-frequency, are not compacted
// (the sequence provisioned → certified → revoked is the audit trail) and are retained for
// thirty days. Those are incompatible topic configurations, and putting both on one topic means
// choosing which of the two guarantees to give up.
func (t EventType) Topic() string {
	if t == EventGatewayHealthChanged {
		return "pp.gateways.health.v1"
	}
	return "pp.gateways.connection.v1"
}

// IsUrgentInvalidation reports whether the data plane must drop its cached view on receiving
// this event rather than waiting for the snapshot's natural refresh. Revocation and rotation
// qualify: both mean the credentials the cache holds will fail at the gateway, and failing at the
// gateway costs a payment where invalidating early costs a cache miss.
func (t EventType) IsUrgentInvalidation() bool {
	return t == EventGatewayConnectionRevoked || t == EventGatewayCredentialsRotated
}

// Event is a domain event raised by an aggregate in this context.
//
// As in BC-6, this is deliberately not the wire envelope. CloudEvents fields, trace context, the
// schema reference and the partition key are assembled in internal/events; the domain raises a
// plain record of what happened and knows nothing about Kafka or JSON. That separation is what
// lets every test in this package run with no broker and no serialization.
//
// Connection events and health events share one struct because they share one consumer surface
// and because two nearly identical structs invite the bug where a field is added to one and not
// the other. The fields that apply to only one kind are zero on the other: Operation is set only
// on health events, ConnectionID only on connection events.
type Event struct {
	Type EventType

	// ConnectionID is set on connection lifecycle events and zero on health events.
	ConnectionID shared.ConnectionID
	GatewayID    shared.GatewayID
	// TenantID and MerchantID are zero on health events by design. Health is per
	// (gateway, operation) and deliberately not per merchant — baseline §10 — because
	// per-merchant samples are too sparse to be statistically meaningful, and stamping a
	// merchant onto a health event would invite a consumer to aggregate it that way.
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	// Operation is set only on health events.
	Operation shared.Operation

	OccurredAt time.Time
	Version    shared.Version
	Payload    map[string]any
}

// AggregateID returns the partition key for this event.
//
// For health this is `gateway:operation`, not the bare gateway ID that baseline §13.3 names.
// That is a deliberate refinement rather than a deviation: the health topic is log-compacted, and
// compaction on the bare gateway ID would retain only the most recently changed operation and
// silently discard the current state of every other one. A consumer rebuilding its view from the
// compacted log would then believe a gateway whose refund circuit is open is fully healthy.
func (e Event) AggregateID() string {
	if e.Type == EventGatewayHealthChanged {
		return e.GatewayID.String() + ":" + e.Operation.String()
	}
	return e.ConnectionID.String()
}

// Topic returns the topic for this event.
func (e Event) Topic() string { return e.Type.Topic() }

// RequiresOperatorAttention reports whether this event should raise an operational signal rather
// than merely being recorded. Certification failure and revocation both block a merchant from
// processing, and both need a human to decide what happens next.
func (e Event) RequiresOperatorAttention() bool {
	return e.Type == EventGatewayCertificationFailed || e.Type == EventGatewayConnectionRevoked
}
