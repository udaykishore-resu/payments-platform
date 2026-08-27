// Package events is the platform's published language on the wire.
//
// The domain raises plain structs (`payment.Event`, `merchant.Event`) that know nothing about
// CloudEvents, Kafka or JSON. This package is the one place that turns those facts into the
// envelope every bounded context has agreed to read, and the one place that turns them back.
// Keeping that translation here — rather than in each producer — is what makes the envelope a
// contract rather than a convention: there is exactly one implementation of it to get wrong,
// exactly one place CI has to test, and a producer physically cannot emit a shape nobody agreed
// to because it has no code path that constructs one.
//
// The authority for everything in this package is docs/events.md and
// api/events/envelope.schema.json. Where this code and those documents disagree, the documents
// are right and this code is a bug.
//
// Dependency posture: stdlib, the domain, application ports, pkg/*, and go.opentelemetry.io's
// trace API (for reading the ambient W3C trace context out of a context.Context). No broker
// client, no Redis, no database — a package that every producer imports must not drag a driver
// behind it.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
)

// Constants fixed by the envelope contract (docs/events.md §1.1). They are constants rather than
// configuration because a deployment that could change them could break every consumer in the
// platform by editing a YAML file.
const (
	// SpecVersion is the CloudEvents structural version. It identifies the shape of the
	// envelope, never the version of our event — see the note on Envelope.Type.
	SpecVersion = "1.0"

	// DataContentType is fixed: topics are archived to S3 as JSON and read by humans during
	// incidents, so a binary payload format would trade a permanent debugging capability for a
	// few percent of bandwidth that compression already recovers.
	DataContentType = "application/json"

	// SchemaBaseURI prefixes every dataschema value. The file in api/events/ is
	// `<type>.schema.json` and it is served at `<SchemaBaseURI><type>.json`; the two differ only
	// in extension and registry_test.go asserts the mapping.
	SchemaBaseURI = "https://schemas.example.com/events/"

	// TimeFormat is RFC 3339 with exactly millisecond precision, UTC, `Z` suffix (rule E6).
	// Exactly three fractional digits, always: Go's RFC3339Nano trims trailing zeros, which
	// produces `...:11.4Z` for a time that is 411 ms past the second and fails the schema's
	// pattern. That is precisely the kind of bug that only appears for one in a thousand events.
	TimeFormat = "2006-01-02T15:04:05.000Z"

	// MaxEnvelopeBytes is rule E8. Larger payloads carry a payloadRef object-storage URI
	// instead of the body. Enforced here so an oversized envelope is rejected by the producer
	// rather than by the broker, where it would wedge the outbox relay behind a record that can
	// never be delivered.
	MaxEnvelopeBytes = 256 * 1024
)

// Timestamp is an event time constrained to the envelope's wire format.
//
// It exists as a distinct type instead of a time.Time field with a custom struct tag because
// the format is load-bearing: `time` is the only ordering signal a consumer reading a compacted
// or archived topic has, and a producer that emits a local offset or an epoch integer breaks
// every one of them at once. Making the marshaller the only way to produce the field means the
// format cannot drift.
type Timestamp time.Time

// MarshalJSON renders the instant in UTC with exactly millisecond precision.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(t).UTC().Format(TimeFormat))
}

// UnmarshalJSON parses the wire format, rejecting anything that is not RFC 3339 with a `Z`
// suffix. It deliberately does not fall back to a lenient parse: accepting `+02:00` here would
// let a mis-implemented producer poison a consumer's ordering silently.
func (t *Timestamp) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.Parse(TimeFormat, s)
	if err != nil {
		return fmt.Errorf("events: time %q is not RFC 3339 with millisecond precision and a Z suffix: %w", s, err)
	}
	*t = Timestamp(parsed)
	return nil
}

// Time returns the instant as a UTC time.Time.
func (t Timestamp) Time() time.Time { return time.Time(t).UTC() }

// String renders the wire form, which is also the form that belongs in a log line.
func (t Timestamp) String() string { return time.Time(t).UTC().Format(TimeFormat) }

// NewTimestamp truncates an instant to the envelope's precision.
//
// Truncation rather than rounding: rounding can move a timestamp *forward* past a later event's
// timestamp, which inverts the two in any consumer that sorts on `time`. Truncation can only
// collapse two events onto the same millisecond, which is a tie the consumer already has to
// break with `aggregateversion`.
func NewTimestamp(t time.Time) Timestamp { return Timestamp(t.UTC().Truncate(time.Millisecond)) }

// Envelope is the CloudEvents 1.0-compatible platform event envelope.
//
// # Why the version lives in the type name and not in a field
//
// `type` is `<aggregate>.<past-tense-verb>.v<major>` — `payment.captured.v1`. There is no
// `version` field and no version header, and that is a deliberate, load-bearing choice:
//
//   - A consumer subscribing to `payment.captured.v1` **cannot be handed a v2 payload**. With a
//     version field, every consumer has to remember to check it, and a consumer that forgets does
//     not fail loudly — it decodes a v2 body into a v1 struct, silently drops the fields it does
//     not know, and produces a wrong answer that looks like a data bug rather than a contract
//     violation. Two years later someone is bisecting the ledger to find it.
//   - It makes dual-publishing (docs/events.md §3.3) a routing problem instead of a filtering
//     problem: `.v1` and `.v2` are two distinct types on the same topic, both emitted from the
//     same transaction, and a consumer picks by name.
//   - It makes `dataschema` a pure function of `type`, so schema resolution needs no lookup
//     table and CI can assert the correspondence mechanically.
//
// The cost is that a breaking change means writing a new schema file and running a migration
// protocol rather than editing one line. That cost is the point: the friction is proportional to
// the blast radius.
//
// Field-level semantics are in docs/events.md §1.1; the JSON tags below are the wire contract and
// are lowercase-without-separators because that is what CloudEvents requires of extension
// attributes.
type Envelope struct {
	SpecVersion     string    `json:"specversion"`
	ID              string    `json:"id"`
	Type            string    `json:"type"`
	Source          string    `json:"source"`
	Subject         string    `json:"subject"`
	Time            Timestamp `json:"time"`
	DataContentType string    `json:"datacontenttype"`
	DataSchema      string    `json:"dataschema"`

	// TenantID is on every event without exception. Kafka ACLs and log views key off it, and a
	// consumer that cannot tell which tenant an event belongs to cannot enforce isolation.
	TenantID string `json:"tenantid"`
	// MerchantID is absent only on platform-scoped events (gateway.health_changed.v1) and
	// tenant-level audit records.
	MerchantID string `json:"merchantid,omitempty"`
	// CorrelationID is constant across an entire causal fan-out, so one API call's full
	// consequences are one query rather than a manual graph walk.
	CorrelationID string `json:"correlationid"`
	// CausationID is the id of the event that directly caused this one. Absent when the cause
	// was an external API call, where CorrelationID already carries the linkage.
	CausationID string `json:"causationid,omitempty"`
	TraceParent string `json:"traceparent"`

	AggregateID string `json:"aggregateid"`
	// AggregateVersion is the root's version *after* this change. Monotonic per aggregate but
	// NOT dense: a consumer of a subset of an aggregate's types legitimately sees gaps, so it is
	// a staleness detector, never a loss detector.
	AggregateVersion int64 `json:"aggregateversion"`
	// PartitionKey duplicates the Kafka message key into the body so a consumer reading from the
	// S3 archive — where the Kafka key no longer exists — still knows the ordering domain.
	PartitionKey string `json:"partitionkey"`

	// Data is the type-specific payload, validated against DataSchema. Never null: an event
	// with no payload carries `{}`, because `null` forces every consumer to write a nil check
	// that one of them will forget.
	Data json.RawMessage `json:"data"`
}

// envelopeWire is the marshalling shape. It exists because Time needs a JSON tag that the
// exported struct cannot carry without making the Timestamp type's zero value ambiguous in
// reflection-based tools, and because Data must be normalised from nil to `{}` on the way out.
type envelopeWire struct {
	SpecVersion      string          `json:"specversion"`
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Source           string          `json:"source"`
	Subject          string          `json:"subject"`
	Time             Timestamp       `json:"time"`
	DataContentType  string          `json:"datacontenttype"`
	DataSchema       string          `json:"dataschema"`
	TenantID         string          `json:"tenantid"`
	MerchantID       string          `json:"merchantid,omitempty"`
	CorrelationID    string          `json:"correlationid"`
	CausationID      string          `json:"causationid,omitempty"`
	TraceParent      string          `json:"traceparent"`
	AggregateID      string          `json:"aggregateid"`
	AggregateVersion int64           `json:"aggregateversion"`
	PartitionKey     string          `json:"partitionkey"`
	Data             json.RawMessage `json:"data"`
}

// emptyObject is the payload of an event that carries no data (rule: never null).
var emptyObject = json.RawMessage(`{}`)

// MarshalJSON renders the envelope, normalising a nil payload to `{}`.
func (e Envelope) MarshalJSON() ([]byte, error) {
	data := e.Data
	if len(data) == 0 {
		data = emptyObject
	}
	return json.Marshal(envelopeWire{
		SpecVersion: e.SpecVersion, ID: e.ID, Type: e.Type, Source: e.Source,
		Subject: e.Subject, Time: e.Time, DataContentType: e.DataContentType,
		DataSchema: e.DataSchema, TenantID: e.TenantID, MerchantID: e.MerchantID,
		CorrelationID: e.CorrelationID, CausationID: e.CausationID,
		TraceParent: e.TraceParent, AggregateID: e.AggregateID,
		AggregateVersion: e.AggregateVersion, PartitionKey: e.PartitionKey, Data: data,
	})
}

// UnmarshalJSON decodes an envelope.
//
// It is deliberately *not* strict about unknown top-level fields (rule V6). A consumer that
// rejects an envelope carrying a field it has not been taught about turns an additive,
// safe-by-construction change into an outage, and additive changes are the ones we ship without
// coordinating a deploy across nine binaries.
func (e *Envelope) UnmarshalJSON(b []byte) error {
	var w envelopeWire
	if err := json.Unmarshal(b, &w); err != nil {
		return fmt.Errorf("events: decoding envelope: %w", err)
	}
	*e = Envelope(w)
	if len(e.Data) == 0 || string(e.Data) == "null" {
		e.Data = emptyObject
	}
	return nil
}

// EventID returns the envelope's id as the domain's typed identifier, which is the form the
// dedup store keys on.
func (e Envelope) EventID() shared.EventID { return shared.EventID(e.ID) }

// The wire patterns, mirroring api/events/envelope.schema.json exactly. They are duplicated here
// rather than derived from the schema at runtime because a producer must not depend on a network
// fetch to know whether it is allowed to emit an event, and because a compiled regexp is three
// orders of magnitude cheaper than a schema walk on a path that runs 60 000 times a second.
// envelope_test.go asserts that these and the schema file agree.
var (
	rePrefixedULID = regexp.MustCompile(`^[a-z]+_[0-9A-Z]{26}$`)
	reEventID      = regexp.MustCompile(`^evt_[0-9A-Z]{26}$`)
	reTenantID     = regexp.MustCompile(`^ten_[0-9A-Z]{26}$`)
	reMerchantID   = regexp.MustCompile(`^mrc_[0-9A-Z]{26}$`)
	reRequestID    = regexp.MustCompile(`^req_[0-9A-Z]{26}$`)
	reType         = regexp.MustCompile(`^[a-z][a-z_]*\.[a-z][a-z_]*\.v[1-9][0-9]*$`)
	reSource       = regexp.MustCompile(`^/payments-platform/[a-z0-9-]+$`)
	reDataSchema   = regexp.MustCompile(`^https://schemas\.example\.com/events/[a-z_]+\.[a-z_]+\.v[1-9][0-9]*\.json$`)
	reTraceParent  = regexp.MustCompile(`^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)
)

// Validate reports whether the envelope satisfies the published contract.
//
// It is called on the way out (in Encode) and on the way in (in Decode), and both matter for
// different reasons. Outbound, it is the last chance to stop a malformed event before it is
// committed to a topic where it is immutable and will be redelivered forever. Inbound, it turns
// "the handler panicked on a nil field" into a classified, non-retryable error that routes to
// the DLQ with a readable reason.
//
// Every failure is a non-retryable VALIDATION_FAILED: an envelope that is wrong now will be
// exactly as wrong on the sixth retry twenty-two minutes later, and the only thing retrying buys
// is a delayed alert.
func (e Envelope) Validate() error {
	var details []apierror.Detail
	bad := func(field, msg string) {
		details = append(details, apierror.Detail{
			Field: field, Code: "ENVELOPE_INVALID", Message: msg, RuleID: "L6.EVENT_ENVELOPE_VALID",
		})
	}
	must := func(field, value string, re *regexp.Regexp, shape string) {
		switch {
		case value == "":
			bad(field, "is required")
		case !re.MatchString(value):
			bad(field, "must match "+shape)
		}
	}

	if e.SpecVersion != SpecVersion {
		bad("specversion", "must be "+SpecVersion)
	}
	must("id", e.ID, reEventID, "evt_<ULID>")
	must("type", e.Type, reType, "<aggregate>.<verb>.v<major>")
	must("source", e.Source, reSource, "/payments-platform/<deployable>")
	if e.Subject == "" || len(e.Subject) > 128 {
		bad("subject", "is required and must be at most 128 characters")
	}
	if e.Time.Time().IsZero() {
		bad("time", "is required")
	}
	if e.DataContentType != DataContentType {
		bad("datacontenttype", "must be "+DataContentType)
	}
	must("dataschema", e.DataSchema, reDataSchema, SchemaBaseURI+"<type>.json")
	if want := SchemaURIFor(e.Type); e.DataSchema != "" && e.Type != "" && e.DataSchema != want {
		bad("dataschema", "must be derived from type: expected "+want)
	}
	must("tenantid", e.TenantID, reTenantID, "ten_<ULID>")
	if e.MerchantID != "" && !reMerchantID.MatchString(e.MerchantID) {
		bad("merchantid", "must match mrc_<ULID> when present")
	}
	must("correlationid", e.CorrelationID, reRequestID, "req_<ULID>")
	if e.CausationID != "" && !reEventID.MatchString(e.CausationID) {
		bad("causationid", "must match evt_<ULID> when present")
	}
	must("traceparent", e.TraceParent, reTraceParent, "W3C trace context")
	if e.AggregateID == "" || len(e.AggregateID) > 128 {
		bad("aggregateid", "is required and must be at most 128 characters")
	}
	if e.AggregateVersion < 1 {
		bad("aggregateversion", "must be at least 1")
	}
	if e.PartitionKey == "" || len(e.PartitionKey) > 128 {
		bad("partitionkey", "is required and must be at most 128 characters")
	}
	if len(e.Data) == 0 || string(e.Data) == "null" {
		bad("data", "is required; an event with no payload carries {}")
	} else if !json.Valid(e.Data) {
		bad("data", "must be valid JSON")
	} else if trimmed := strings.TrimSpace(string(e.Data)); !strings.HasPrefix(trimmed, "{") {
		bad("data", "must be a JSON object")
	}

	if len(details) == 0 {
		return nil
	}
	return apierror.Newf(apierror.CodeValidationFailed,
		"event envelope %s (%s) is invalid", e.ID, e.Type).WithDetails(details...)
}

// SchemaURIFor derives the dataschema URI from an event type. It is a pure function precisely
// because the version lives in the type name; there is nothing to look up.
func SchemaURIFor(eventType string) string { return SchemaBaseURI + eventType + ".json" }

// SchemaFileFor derives the repository filename for an event type. CI walks api/events/ and
// asserts this against the registry in both directions.
func SchemaFileFor(eventType string) string { return eventType + ".schema.json" }

// --- provenance -------------------------------------------------------------------------------

// Provenance is the request-scoped context an event inherits from whatever caused it.
//
// It travels in the context because it is established at points far from where events are
// raised — tenant at authentication, correlation at the API edge, causation when a consumer
// starts handling an inbound event — and threading six parameters through the application layer
// is how they end up omitted from exactly the event you need during an incident.
//
// Only Source is a property of the process; the rest belong to the request.
type Provenance struct {
	// Source is `/payments-platform/<deployable>`, set once at startup by the composition root.
	Source string
	// TenantID and MerchantID scope the event. A fact raised by an aggregate carries its own
	// ids; these are the fallback for facts that do not, and the cross-check for those that do.
	TenantID   string
	MerchantID string
	// CorrelationID is the originating request id (`req_<ULID>`).
	CorrelationID string
	// CausationID is the id of the event being handled, when this event is raised by a consumer.
	CausationID string
	// TraceParent overrides the ambient OpenTelemetry span, for the replay path where the
	// original trace context must be preserved verbatim rather than re-derived.
	TraceParent string
}

type provenanceKey struct{}

// ContextWithProvenance merges p into ctx, leaving any field p does not set intact.
//
// Merge rather than replace, for the same reason telemetry.ContextWithFields merges: each stage
// of the pipeline learns one more dimension, and a replacing setter means the first stage to
// forget one silently drops it from every event downstream.
func ContextWithProvenance(ctx context.Context, p Provenance) context.Context {
	cur, _ := ctx.Value(provenanceKey{}).(Provenance)
	if p.Source != "" {
		cur.Source = p.Source
	}
	if p.TenantID != "" {
		cur.TenantID = p.TenantID
	}
	if p.MerchantID != "" {
		cur.MerchantID = p.MerchantID
	}
	if p.CorrelationID != "" {
		cur.CorrelationID = p.CorrelationID
	}
	if p.CausationID != "" {
		cur.CausationID = p.CausationID
	}
	if p.TraceParent != "" {
		cur.TraceParent = p.TraceParent
	}
	return context.WithValue(ctx, provenanceKey{}, cur)
}

// ProvenanceFromContext returns the provenance established so far. The zero value is a valid
// answer for a background job that has not entered a request; NewEnvelope then fails validation
// loudly rather than emitting an uncorrelatable event.
func ProvenanceFromContext(ctx context.Context) Provenance {
	if ctx == nil {
		return Provenance{}
	}
	p, _ := ctx.Value(provenanceKey{}).(Provenance)
	return p
}

// TraceParentFromContext renders the ambient span as a W3C `traceparent` header value, or ""
// when ctx carries no valid span.
//
// The envelope carries the rendered string rather than the SpanContext because the consumer is
// in another process: it needs the wire form to continue the trace, and every caller wanting
// `.String()` is how one of them ends up writing an all-zero trace ID into a topic.
func TraceParentFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return fmt.Sprintf("00-%s-%s-%02x", sc.TraceID(), sc.SpanID(), byte(sc.TraceFlags()))
}

// --- facts ------------------------------------------------------------------------------------

// Fact is what a domain event must be able to say about itself to become an envelope.
//
// It is declared here, with the consumer, and satisfied by thin adapters in translate.go rather
// than by the domain types themselves. The domain must not grow a `PartitionKey()` method: a
// partition key is a Kafka concept, and the day it appears on a domain aggregate is the day the
// domain starts being shaped by the broker.
type Fact interface {
	// EventType is the published type, e.g. "payment.captured.v1".
	EventType() string
	// Subject is the business subject — usually the aggregate root, but the child for events
	// about a child published under their root.
	Subject() string
	// AggregateID is the root.
	AggregateID() string
	// AggregateVersion is the root's version after this change.
	AggregateVersion() int64
	// PartitionKey is the ordering domain, per the catalog.
	PartitionKey() string
	// OccurredAt is when the fact happened in the producer's transaction.
	OccurredAt() time.Time
	// TenantID scopes the event; required.
	TenantID() string
	// MerchantID scopes the event; "" for platform-scoped events.
	MerchantID() string
	// Data is the type-specific payload, already in its published shape.
	Data() map[string]any
}

// NewEnvelope builds a validated envelope from a domain fact and the ambient request context.
//
// What it fills in automatically, and why each is automatic rather than a parameter:
//
//   - `id`: a fresh `evt_<ULID>`. Minted here, once, and never regenerated — it is the
//     deduplication key, so a redelivery must carry the same id, which is why the relay
//     republishes the stored envelope verbatim instead of rebuilding it.
//   - `specversion`, `datacontenttype`: fixed by the contract.
//   - `dataschema`: derived from the type, so it cannot disagree with it.
//   - `source`: from the process's provenance.
//   - `tenantid` / `merchantid`: from the fact, falling back to the request's provenance. A fact
//     that knows its own tenant always wins: the ambient tenant is the *caller's*, and on the
//     rare path where a platform job acts on a tenant's aggregate those two differ, with the
//     fact being right.
//   - `correlationid`, `causationid`, `traceparent`: from the request context, so the causal
//     graph is complete without any call site remembering to pass it.
//
// It returns a validation error rather than a partly filled envelope. An event missing its
// correlation id is not a slightly worse event; it is an event that cannot be explained during
// an incident, and the failure belongs at the producer where someone can fix it.
func NewEnvelope(ctx context.Context, f Fact) (Envelope, error) {
	if f == nil {
		return Envelope{}, apierror.New(apierror.CodeInternalError, "events: nil fact")
	}
	reg, err := Lookup(f.EventType())
	if err != nil {
		return Envelope{}, err
	}
	p := ProvenanceFromContext(ctx)

	traceparent := p.TraceParent
	if traceparent == "" {
		traceparent = TraceParentFromContext(ctx)
	}

	tenant := f.TenantID()
	if tenant == "" {
		tenant = p.TenantID
	}
	merchant := f.MerchantID()
	if merchant == "" {
		merchant = p.MerchantID
	}

	data, err := marshalData(f.Data())
	if err != nil {
		return Envelope{}, err
	}

	env := Envelope{
		SpecVersion:      SpecVersion,
		ID:               string(ids.New(ids.PrefixEvent)),
		Type:             reg.Type,
		Source:           p.Source,
		Subject:          f.Subject(),
		Time:             NewTimestamp(f.OccurredAt()),
		DataContentType:  DataContentType,
		DataSchema:       reg.SchemaURI,
		TenantID:         tenant,
		MerchantID:       merchant,
		CorrelationID:    p.CorrelationID,
		CausationID:      p.CausationID,
		TraceParent:      traceparent,
		AggregateID:      f.AggregateID(),
		AggregateVersion: f.AggregateVersion(),
		PartitionKey:     f.PartitionKey(),
		Data:             data,
	}
	if env.Subject == "" {
		env.Subject = env.AggregateID
	}
	if env.PartitionKey == "" {
		env.PartitionKey = env.AggregateID
	}
	if err := env.Validate(); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

// marshalData renders a payload map, normalising nil to `{}`.
func marshalData(m map[string]any) (json.RawMessage, error) {
	if len(m) == 0 {
		return emptyObject, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "events: encoding event payload")
	}
	return b, nil
}

// ParsePrefixedID reports whether s is a well-formed prefixed ULID of any known kind. It exists
// so that Validate's subject and aggregateid checks can be tightened per event type by the
// registry without each producer inventing its own check.
func ParsePrefixedID(s string) bool { return rePrefixedULID.MatchString(s) }
