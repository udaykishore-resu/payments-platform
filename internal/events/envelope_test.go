package events

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// schemaDir is the repository's event schema directory, relative to this package.
const schemaDir = "../../api/events"

// --- a minimal JSON Schema checker -------------------------------------------------------------
//
// Deliberately hand-rolled against encoding/json rather than pulled from a dependency. The test
// only needs the four assertions the envelope contract actually turns on — required, type, const
// and pattern, plus additionalProperties:false — and a full 2020-12 validator would be a new
// module in go.mod for a check that fits in sixty lines. If the schema ever grows a construct
// this cannot express, the right answer is to reconsider the dependency, not to silently skip.

type jsonSchema struct {
	Type                 string                     `json:"type"`
	Required             []string                   `json:"required"`
	AdditionalProperties *bool                      `json:"additionalProperties"`
	Properties           map[string]schemaProp      `json:"properties"`
	Examples             []map[string]any           `json:"examples"`
	Defs                 map[string]json.RawMessage `json:"$defs"`
}

type schemaProp struct {
	Type    string `json:"type"`
	Const   any    `json:"const"`
	Pattern string `json:"pattern"`
	Minimum *int   `json:"minimum"`
	MinLen  *int   `json:"minLength"`
	MaxLen  *int   `json:"maxLength"`
}

func loadSchema(t *testing.T, name string) jsonSchema {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(schemaDir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	var s jsonSchema
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	return s
}

// checkAgainstSchema reports every way doc violates s.
func checkAgainstSchema(s jsonSchema, doc map[string]any) []string {
	var problems []string
	for _, req := range s.Required {
		if _, ok := doc[req]; !ok {
			problems = append(problems, "missing required field "+req)
		}
	}
	if s.AdditionalProperties != nil && !*s.AdditionalProperties {
		for k := range doc {
			if _, declared := s.Properties[k]; !declared {
				problems = append(problems, "undeclared field "+k+" (additionalProperties is false)")
			}
		}
	}
	for name, prop := range s.Properties {
		v, present := doc[name]
		if !present {
			continue
		}
		if prop.Const != nil && !reflect.DeepEqual(v, prop.Const) {
			problems = append(problems, name+" must equal the schema const")
		}
		switch prop.Type {
		case "string":
			str, ok := v.(string)
			if !ok {
				problems = append(problems, name+" must be a string")
				continue
			}
			if prop.Pattern != "" && !regexp.MustCompile(prop.Pattern).MatchString(str) {
				problems = append(problems, name+" does not match "+prop.Pattern)
			}
			if prop.MinLen != nil && len(str) < *prop.MinLen {
				problems = append(problems, name+" is shorter than minLength")
			}
			if prop.MaxLen != nil && len(str) > *prop.MaxLen {
				problems = append(problems, name+" is longer than maxLength")
			}
		case "integer":
			f, ok := v.(float64)
			if !ok || f != float64(int64(f)) {
				problems = append(problems, name+" must be an integer")
				continue
			}
			if prop.Minimum != nil && int(f) < *prop.Minimum {
				problems = append(problems, name+" is below minimum")
			}
		case "object":
			if _, ok := v.(map[string]any); !ok {
				problems = append(problems, name+" must be an object")
			}
		}
	}
	return problems
}

// --- fixtures ------------------------------------------------------------------------------------

const (
	testTenant   = "ten_01JB8Z00000000000000000000"
	testMerchant = "mrc_01JB8Z11111111111111111111"
	testRequest  = "req_01JB8Z22222222222222222222"
	testCause    = "evt_01JB8Z33333333333333333333"
	testPayment  = "pay_01JB8Z9K2QW3E4R5T6Y7U8I9O0"
	testAttempt  = "att_01JB8Z44444444444444444444"
	testTrace    = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
)

// testFact is a Fact with settable fields, so a table test can knock out one field at a time.
type testFact struct {
	typ      string
	subject  string
	aggID    string
	version  int64
	key      string
	occurred time.Time
	tenant   string
	merchant string
	data     map[string]any
}

func (f testFact) EventType() string       { return f.typ }
func (f testFact) Subject() string         { return f.subject }
func (f testFact) AggregateID() string     { return f.aggID }
func (f testFact) AggregateVersion() int64 { return f.version }
func (f testFact) PartitionKey() string    { return f.key }
func (f testFact) OccurredAt() time.Time   { return f.occurred }
func (f testFact) TenantID() string        { return f.tenant }
func (f testFact) MerchantID() string      { return f.merchant }
func (f testFact) Data() map[string]any    { return f.data }

func validFact() testFact {
	return testFact{
		typ:      "payment.authorized.v1",
		subject:  testPayment,
		aggID:    testPayment,
		version:  4,
		key:      testPayment,
		occurred: time.Date(2026, 8, 26, 14, 3, 11, 412_000_000, time.UTC),
		tenant:   testTenant,
		merchant: testMerchant,
		data: map[string]any{
			"attemptId":   testAttempt,
			"gatewayCode": "stripe",
		},
	}
}

func provenanceCtx() context.Context {
	return ContextWithProvenance(context.Background(), Provenance{
		Source:        "/payments-platform/payment-orchestrator",
		CorrelationID: testRequest,
		CausationID:   testCause,
		TraceParent:   testTrace,
	})
}

func mustEnvelope(t *testing.T) Envelope {
	t.Helper()
	env, err := NewEnvelope(provenanceCtx(), validFact())
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	return env
}

// --- tests ---------------------------------------------------------------------------------------

func TestNewEnvelopeFillsContextFields(t *testing.T) {
	t.Parallel()
	env := mustEnvelope(t)

	if env.SpecVersion != SpecVersion || env.DataContentType != DataContentType {
		t.Fatalf("fixed fields not set: %+v", env)
	}
	if !reEventID.MatchString(env.ID) {
		t.Fatalf("id %q is not a prefixed ULID", env.ID)
	}
	if env.Source != "/payments-platform/payment-orchestrator" {
		t.Fatalf("source not taken from provenance: %q", env.Source)
	}
	if env.CorrelationID != testRequest || env.CausationID != testCause || env.TraceParent != testTrace {
		t.Fatalf("correlation fields not taken from context: %+v", env)
	}
	if env.DataSchema != SchemaURIFor(env.Type) {
		t.Fatalf("dataschema %q is not derived from type %q", env.DataSchema, env.Type)
	}
	if got, want := env.Time.String(), "2026-08-26T14:03:11.412Z"; got != want {
		t.Fatalf("time = %q, want %q", got, want)
	}
}

func TestNewEnvelopeGeneratesAFreshIDEachTime(t *testing.T) {
	t.Parallel()
	// The id is the dedup key; two facts must never share one, or one consumer's dedup table
	// suppresses the other's event.
	a, b := mustEnvelope(t), mustEnvelope(t)
	if a.ID == b.ID {
		t.Fatalf("two envelopes share id %s", a.ID)
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	t.Parallel()
	env := mustEnvelope(t)

	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Envelope
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(env, back) {
		t.Fatalf("round trip changed the envelope:\n got %+v\nwant %+v", back, env)
	}

	// And re-marshalling is byte-identical, which is what makes a redelivery byte-identical.
	b2, err := json.Marshal(back)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(b) != string(b2) {
		t.Fatalf("re-marshal is not byte-identical:\n%s\n%s", b, b2)
	}
}

func TestEnvelopeEmptyPayloadIsAnObjectNotNull(t *testing.T) {
	t.Parallel()
	f := validFact()
	f.data = nil
	env, err := NewEnvelope(provenanceCtx(), f)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if string(env.Data) != "{}" {
		t.Fatalf("empty payload rendered as %q, want {}", env.Data)
	}
	b, _ := json.Marshal(env)
	if strings.Contains(string(b), `"data":null`) {
		t.Fatalf("data marshalled as null: %s", b)
	}

	// And an inbound null decodes to {} rather than to a nil a handler will dereference.
	var back Envelope
	raw := strings.Replace(string(b), `"data":{}`, `"data":null`, 1)
	if err := json.Unmarshal([]byte(raw), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(back.Data) != "{}" {
		t.Fatalf("null payload decoded as %q, want {}", back.Data)
	}
}

func TestTimestampRejectsNonMillisecondUTC(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		`"2026-08-26T14:03:11Z"`,          // no fraction
		`"2026-08-26T14:03:11.4Z"`,        // one digit
		`"2026-08-26T14:03:11.412+02:00"`, // local offset
		`"2026-08-26T14:03:11.412345Z"`,   // microseconds
	} {
		var ts Timestamp
		if err := ts.UnmarshalJSON([]byte(in)); err == nil {
			t.Errorf("accepted out-of-contract time %s", in)
		}
	}
}

func TestTimestampTruncatesRatherThanRounds(t *testing.T) {
	t.Parallel()
	// 999_600 µs rounds up to the next second and would sort after a later event.
	in := time.Date(2026, 8, 26, 14, 3, 11, 999_600_000, time.UTC)
	if got := NewTimestamp(in).String(); got != "2026-08-26T14:03:11.999Z" {
		t.Fatalf("NewTimestamp truncation = %s", got)
	}
}

func TestValidateRejectsEachMissingRequiredField(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*Envelope){
		"specversion":      func(e *Envelope) { e.SpecVersion = "" },
		"id":               func(e *Envelope) { e.ID = "" },
		"type":             func(e *Envelope) { e.Type = "" },
		"source":           func(e *Envelope) { e.Source = "" },
		"subject":          func(e *Envelope) { e.Subject = "" },
		"time":             func(e *Envelope) { e.Time = Timestamp(time.Time{}) },
		"datacontenttype":  func(e *Envelope) { e.DataContentType = "" },
		"dataschema":       func(e *Envelope) { e.DataSchema = "" },
		"tenantid":         func(e *Envelope) { e.TenantID = "" },
		"correlationid":    func(e *Envelope) { e.CorrelationID = "" },
		"traceparent":      func(e *Envelope) { e.TraceParent = "" },
		"aggregateid":      func(e *Envelope) { e.AggregateID = "" },
		"aggregateversion": func(e *Envelope) { e.AggregateVersion = 0 },
		"partitionkey":     func(e *Envelope) { e.PartitionKey = "" },
		"data":             func(e *Envelope) { e.Data = nil },
	}
	base := mustEnvelope(t)
	for field, mutate := range cases {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			env := base
			mutate(&env)
			err := env.Validate()
			if err == nil {
				t.Fatalf("Validate accepted an envelope with no %s", field)
			}
			if !strings.Contains(err.Error(), "invalid") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateRejectsMalformedIdentifiers(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*Envelope){
		"id without prefix":         func(e *Envelope) { e.ID = "01JB8Z00000000000000000000" },
		"tenant with wrong prefix":  func(e *Envelope) { e.TenantID = testMerchant },
		"merchant with bad shape":   func(e *Envelope) { e.MerchantID = "mrc_short" },
		"causation not an event id": func(e *Envelope) { e.CausationID = testRequest },
		"traceparent truncated":     func(e *Envelope) { e.TraceParent = "00-4bf92f-00f067aa0ba902b7-01" },
		"type without version":      func(e *Envelope) { e.Type = "payment.authorized" },
		"source not a deployable":   func(e *Envelope) { e.Source = "payment-orchestrator" },
		"schema not from type":      func(e *Envelope) { e.DataSchema = SchemaURIFor("payment.captured.v1") },
		"data is not an object":     func(e *Envelope) { e.Data = json.RawMessage(`[1,2]`) },
	}
	base := mustEnvelope(t)
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := base
			mutate(&env)
			if err := env.Validate(); err == nil {
				t.Fatalf("Validate accepted %s", name)
			}
		})
	}
}

func TestNewEnvelopeRejectsAnUnregisteredType(t *testing.T) {
	t.Parallel()
	f := validFact()
	f.typ = "payment.teleported.v1"
	if _, err := NewEnvelope(provenanceCtx(), f); err == nil {
		t.Fatal("NewEnvelope accepted an unregistered type")
	}
}

func TestNewEnvelopeRefusesAnUncorrelatedEvent(t *testing.T) {
	t.Parallel()
	// No provenance in context: an event nobody can trace back to a request is not a slightly
	// worse event, it is one that cannot be explained during an incident.
	if _, err := NewEnvelope(context.Background(), validFact()); err == nil {
		t.Fatal("NewEnvelope produced an envelope with no source, correlation or trace")
	}
}

func TestEnvelopeConformsToTheJSONSchema(t *testing.T) {
	t.Parallel()
	s := loadSchema(t, "envelope.schema.json")

	env := mustEnvelope(t)
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("unmarshal to generic: %v", err)
	}
	if problems := checkAgainstSchema(s, doc); len(problems) > 0 {
		t.Fatalf("envelope violates api/events/envelope.schema.json:\n  %s", strings.Join(problems, "\n  "))
	}
}

func TestSchemaExamplesConformToTheirOwnSchema(t *testing.T) {
	t.Parallel()
	// The examples in the schema are the contract as documented; if they do not validate, either
	// the schema or the documentation is wrong and both are load-bearing.
	s := loadSchema(t, "envelope.schema.json")
	if len(s.Examples) == 0 {
		t.Fatal("envelope.schema.json carries no examples")
	}
	for i, ex := range s.Examples {
		if problems := checkAgainstSchema(s, ex); len(problems) > 0 {
			t.Errorf("example %d violates its own schema:\n  %s", i, strings.Join(problems, "\n  "))
		}
	}
}

func TestSchemaExamplesDecodeAsEnvelopes(t *testing.T) {
	t.Parallel()
	s := loadSchema(t, "envelope.schema.json")
	for i, ex := range s.Examples {
		raw, err := json.Marshal(ex)
		if err != nil {
			t.Fatalf("example %d: %v", i, err)
		}
		var env Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("example %d does not decode as an Envelope: %v", i, err)
		}
		if err := env.Validate(); err != nil {
			t.Errorf("example %d fails Validate: %v", i, err)
		}
	}
}

func TestValidatorPatternsMatchTheSchema(t *testing.T) {
	t.Parallel()
	// The compiled patterns in envelope.go duplicate the schema so the hot path needs no schema
	// walk. This is the test that keeps the duplicate honest.
	s := loadSchema(t, "envelope.schema.json")
	ours := map[string]*regexp.Regexp{
		"id":            reEventID,
		"type":          reType,
		"source":        reSource,
		"dataschema":    reDataSchema,
		"tenantid":      reTenantID,
		"merchantid":    reMerchantID,
		"correlationid": reRequestID,
		"causationid":   reEventID,
		"traceparent":   reTraceParent,
	}
	for field, re := range ours {
		prop, ok := s.Properties[field]
		if !ok {
			t.Errorf("schema has no property %s", field)
			continue
		}
		if prop.Pattern != re.String() {
			t.Errorf("%s: code pattern %q != schema pattern %q", field, re.String(), prop.Pattern)
		}
	}
}

func TestTraceParentFromContextIsEmptyWithoutASpan(t *testing.T) {
	t.Parallel()
	if got := TraceParentFromContext(context.Background()); got != "" {
		t.Fatalf("TraceParentFromContext = %q, want empty for a context with no span", got)
	}
}

func TestProvenanceMerges(t *testing.T) {
	t.Parallel()
	ctx := ContextWithProvenance(context.Background(), Provenance{Source: "/payments-platform/api"})
	ctx = ContextWithProvenance(ctx, Provenance{CorrelationID: testRequest})
	p := ProvenanceFromContext(ctx)
	if p.Source != "/payments-platform/api" {
		t.Fatalf("merge dropped source: %+v", p)
	}
	if p.CorrelationID != testRequest {
		t.Fatalf("merge dropped correlation: %+v", p)
	}
}
