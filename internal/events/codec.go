package events

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Broker-level headers.
//
// Every one of these duplicates a field that is already in the envelope, which looks like waste
// until you need it. Headers are readable without deserialising the body, and three things
// depend on that:
//
//   - A router or a mirror-maker filtering by tenant does not want to parse a 200 KiB webhook
//     body to learn the tenant it belongs to.
//   - A consumer that subscribes to one type on a shared topic can skip a record on the header
//     alone; at 60 000 records/s the JSON decode it avoids is real CPU.
//   - `traceparent` in a header is what lets a broker-level tool (and the OpenTelemetry Kafka
//     instrumentation) link the record to its producing trace without understanding our schema.
//
// The prefix is `pp-` rather than the bare CloudEvents `ce-` binary-mode names because we send
// the envelope in *structured* mode — the whole envelope is the value — and using `ce-` names
// for headers that are not a binary-mode CloudEvent would mislead any tool that knows the spec.
const (
	HeaderEventID          = "pp-event-id"
	HeaderEventType        = "pp-event-type"
	HeaderTenantID         = "pp-tenant-id"
	HeaderMerchantID       = "pp-merchant-id"
	HeaderAggregateID      = "pp-aggregate-id"
	HeaderAggregateVersion = "pp-aggregate-version"
	HeaderSchema           = "pp-schema"
	HeaderCorrelationID    = "pp-correlation-id"
	HeaderCausationID      = "pp-causation-id"
	HeaderTraceParent      = "traceparent"
	HeaderContentType      = "content-type"
)

// Encode renders an envelope as an outbox message ready for the relay.
//
// The envelope goes in the payload and a subset of it goes in the headers; the outbox row stores
// both, and the relay publishes them verbatim. Verbatim matters: a redelivery must be
// byte-identical to the first delivery (docs/events.md §6.2), which is only true if nothing
// rebuilds the envelope between the transaction that committed it and the broker.
//
// Encode validates. An invalid envelope caught here costs one failed request; the same envelope
// caught after commit is an immutable poison record that has to be parked and triaged.
func Encode(env Envelope) (ports.OutboxMessage, error) {
	if err := env.Validate(); err != nil {
		return ports.OutboxMessage{}, err
	}
	reg, err := Lookup(env.Type)
	if err != nil {
		return ports.OutboxMessage{}, err
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return ports.OutboxMessage{}, apierror.Wrap(err, apierror.CodeInternalError, "events: encoding envelope")
	}
	if len(payload) > MaxEnvelopeBytes {
		return ports.OutboxMessage{}, apierror.Newf(apierror.CodeRequestTooLarge,
			"event %s (%s) is %d bytes, over the %d byte envelope cap; carry a payloadRef instead",
			env.ID, env.Type, len(payload), MaxEnvelopeBytes)
	}

	headers := map[string]string{
		HeaderContentType:      DataContentType,
		HeaderEventID:          env.ID,
		HeaderEventType:        env.Type,
		HeaderTenantID:         env.TenantID,
		HeaderAggregateID:      env.AggregateID,
		HeaderAggregateVersion: strconv.FormatInt(env.AggregateVersion, 10),
		HeaderSchema:           env.DataSchema,
		HeaderCorrelationID:    env.CorrelationID,
		HeaderTraceParent:      env.TraceParent,
	}
	// Absent rather than empty: a header whose value is "" is indistinguishable from a header
	// that was set to the empty string, and a filter written against it silently matches every
	// platform-scoped event.
	if env.MerchantID != "" {
		headers[HeaderMerchantID] = env.MerchantID
	}
	if env.CausationID != "" {
		headers[HeaderCausationID] = env.CausationID
	}

	return ports.OutboxMessage{
		ID:            env.EventID(),
		TenantID:      shared.TenantID(env.TenantID),
		Topic:         reg.Topic,
		Type:          env.Type,
		AggregateID:   env.AggregateID,
		AggregateType: reg.Aggregate,
		PartitionKey:  env.PartitionKey,
		Payload:       payload,
		Headers:       headers,
		OccurredAt:    env.Time.Time(),
		// AvailableAt zero means "publish now". The retry tiers set it; nothing else does.
	}, nil
}

// EncodeFact is NewEnvelope followed by Encode, which is the whole producer path in one call.
func EncodeFact(ctx context.Context, f Fact) (ports.OutboxMessage, error) {
	env, err := NewEnvelope(ctx, f)
	if err != nil {
		return ports.OutboxMessage{}, err
	}
	return Encode(env)
}

// Decode parses an outbox message's payload back into an envelope and validates it.
//
// Validation on the way in is not paranoia about our own producers. A message may reach a
// consumer from a replay of a topic written by a binary that has since been fixed, from a
// mirror-maker, or from an operator's `platformctl events dlq replay`. Validating turns those
// into a classified non-retryable error with a readable reason instead of a nil-pointer panic
// three frames into a handler.
func Decode(msg ports.OutboxMessage) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(msg.Payload, &env); err != nil {
		return Envelope{}, apierror.Wrapf(err, apierror.CodeValidationFailed,
			"event %s payload is not a valid envelope", msg.ID).
			WithDetail(apierror.Detail{
				Field: "payload", Code: "ENVELOPE_UNPARSEABLE",
				Message: "the record body is not a JSON envelope; it is a poison message and belongs in the DLQ",
				RuleID:  "L6.EVENT_ENVELOPE_VALID",
			})
	}
	if err := env.Validate(); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

// DecodeData unmarshals the envelope's payload into a typed struct.
//
// It uses a lenient decoder on purpose (rule V6): unknown fields are ignored, because within a
// major version new optional fields are added without coordinating a deploy, and a consumer that
// rejects them converts a safe change into an outage.
func DecodeData(env Envelope, into any) error {
	if err := json.Unmarshal(env.Data, into); err != nil {
		return apierror.Wrapf(err, apierror.CodeValidationFailed,
			"event %s (%s) payload does not match the expected shape", env.ID, env.Type)
	}
	return nil
}
