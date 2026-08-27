package payment

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

// EventType is the versioned type name of a payment domain event, matching the catalog in
// docs/spec/00-design-baseline.md §13.2 and the JSON Schemas in api/events/.
//
// The version is in the name, not in a separate field, for one practical reason: a consumer
// subscribing to `payment.captured.v1` cannot accidentally receive a v2 payload it does not
// understand. Making the version a field means every consumer must remember to check it, and
// one that forgets fails in a way that looks like a data bug rather than a contract change.
type EventType string

const (
	EventPaymentCreated                EventType = "payment.created.v1"
	EventPaymentAttempted              EventType = "payment.attempted.v1"
	EventPaymentRequiresAction         EventType = "payment.requires_action.v1"
	EventPaymentAuthorized             EventType = "payment.authorized.v1"
	EventPaymentCaptured               EventType = "payment.captured.v1"
	EventPaymentFailed                 EventType = "payment.failed.v1"
	EventPaymentVoided                 EventType = "payment.voided.v1"
	EventPaymentCanceled               EventType = "payment.canceled.v1"
	EventPaymentExpired                EventType = "payment.expired.v1"
	EventPaymentRefunded               EventType = "payment.refunded.v1"
	EventPaymentSettled                EventType = "payment.settled.v1"
	EventPaymentDisputed               EventType = "payment.disputed.v1"
	EventPaymentDisputeResolved        EventType = "payment.dispute_resolved.v1"
	EventPaymentReconciliationRequired EventType = "payment.reconciliation_required.v1"
)

// AllEventTypes is the complete set this context publishes. The CI check
// TestEveryEventTypeHasASchema asserts each has a JSON Schema in api/events/.
var AllEventTypes = []EventType{
	EventPaymentCreated, EventPaymentAttempted, EventPaymentRequiresAction,
	EventPaymentAuthorized, EventPaymentCaptured, EventPaymentFailed,
	EventPaymentVoided, EventPaymentCanceled, EventPaymentExpired,
	EventPaymentRefunded, EventPaymentSettled, EventPaymentDisputed,
	EventPaymentDisputeResolved, EventPaymentReconciliationRequired,
}

// String satisfies fmt.Stringer.
func (t EventType) String() string { return string(t) }

// Topic returns the Kafka topic this event is published to. All payment events share one topic
// keyed by payment ID, which is what gives per-payment ordering; splitting them across topics
// would lose that ordering for no benefit, because every consumer that cares about one payment
// event cares about the others.
func (t EventType) Topic() string { return "pp.payments.payment.v1" }

// Event is a domain event raised by the Payment aggregate.
//
// It is deliberately *not* the wire envelope. The envelope (CloudEvents fields, trace context,
// schema reference, partition key) is an infrastructure concern assembled in
// internal/events; the domain raises a plain record of what happened and knows nothing about
// Kafka, CloudEvents or JSON. That separation is what lets the domain package be tested with
// no broker and no serialization.
type Event struct {
	Type        EventType
	PaymentID   shared.PaymentID
	TenantID    shared.TenantID
	MerchantID  shared.MerchantID
	OccurredAt  time.Time
	Version     shared.Version
	Payload     map[string]any
	Correlation string
}

// AggregateID returns the partition key for this event. Using the payment ID means every event
// for one payment lands on one partition and is therefore strictly ordered relative to its
// siblings — which is the only ordering guarantee a consumer is permitted to rely on.
func (e Event) AggregateID() string { return e.PaymentID.String() }

// IsTerminalOutcome reports whether this event represents the payment reaching a state from
// which no further money movement is expected. Notification consumers use it to decide when to
// send a final receipt.
func (e Event) IsTerminalOutcome() bool {
	switch e.Type {
	case EventPaymentFailed, EventPaymentVoided, EventPaymentCanceled,
		EventPaymentExpired, EventPaymentRefunded:
		return true
	default:
		return false
	}
}

// RequiresOperatorAttention reports whether this event should raise an operational signal
// rather than merely being recorded. Exactly one event type qualifies: a payment whose outcome
// we do not know is the only condition in this domain that a human may need to resolve.
func (e Event) RequiresOperatorAttention() bool {
	return e.Type == EventPaymentReconciliationRequired
}
