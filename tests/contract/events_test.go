package contract

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/events"
)

// The fixed identifiers every produced event carries.
//
// Constants rather than generated values, and deliberately readable: when a schema assertion
// fails, the message quotes the offending value, and "ten_01JCONTRACT0000000000000" tells the
// reader immediately that this came from the contract suite rather than from a live system.
// They satisfy the envelope's `<prefix>_<26 chars of Crockford base32>` patterns exactly.
const (
	// fixtureBody is the shared 25-character body; each identifier appends one distinguishing
	// character to reach the 26 the ULID patterns require. Every character is in the Crockford
	// alphabet the id CHECK constraints accept, so these ids are also legal as payload values.
	// TestFixtureIdentifiersAreWellFormed asserts the length rather than trusting the count.
	fixtureBody = "01JC0NTRACT00000000000000"

	fixtureTenant   = "ten_" + fixtureBody + "A"
	fixtureMerchant = "mrc_" + fixtureBody + "B"
	fixtureRequest  = "req_" + fixtureBody + "C"
	fixturePayment  = "pay_" + fixtureBody + "D"
	fixtureWebhook  = "whk_" + fixtureBody + "E"
	fixtureTrace    = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	fixtureSource   = "/payments-platform/payment-orchestrator"
)

// fixtureTime is the instant every produced event is stamped with. Fixed, so a failure diff shows
// only what actually changed.
var fixtureTime = time.Date(2026, 8, 26, 15, 20, 44, 771_000_000, time.UTC)

// TestFixtureIdentifiersAreWellFormed guards the constants above.
//
// A fixture id one character short does not fail loudly: NewEnvelope rejects it, every subtest
// fails with an envelope-validation error, and the reader spends twenty minutes looking for a
// codec bug. Asserting the shape here makes that mistake name itself.
func TestFixtureIdentifiersAreWellFormed(t *testing.T) {
	t.Parallel()
	for _, id := range []string{fixtureTenant, fixtureMerchant, fixtureRequest, fixturePayment, fixtureWebhook} {
		prefix, body, ok := strings.Cut(id, "_")
		if !ok || len(prefix) != 3 {
			t.Errorf("%q is not <3-char prefix>_<body>", id)
			continue
		}
		if len(body) != 26 {
			t.Errorf("%q has a %d-character body, want 26", id, len(body))
		}
		for _, r := range body {
			if strings.ContainsRune("ILOU", r) {
				t.Errorf("%q contains %q, which is not in the Crockford base32 alphabet the id "+
					"CHECK constraints accept", id, r)
			}
		}
	}
}

// exampleFact adapts a schema's declared example instance to the events.Fact interface, which is
// what the real producer path consumes.
//
// Using the schema's own example as the payload is the point rather than a shortcut: the example
// is the producer's published claim about what an event of this type looks like, and running it
// through the real codec is what turns that claim into something CI can falsify.
type exampleFact struct {
	reg  events.Registration
	data map[string]any
}

func (f exampleFact) EventType() string       { return f.reg.Type }
func (f exampleFact) Subject() string         { return f.AggregateID() }
func (f exampleFact) AggregateVersion() int64 { return 3 }
func (f exampleFact) PartitionKey() string    { return f.AggregateID() }
func (f exampleFact) OccurredAt() time.Time   { return fixtureTime }
func (f exampleFact) TenantID() string        { return fixtureTenant }
func (f exampleFact) Data() map[string]any    { return f.data }

// AggregateID is derived from the registration's declared partition-key field, so the fixture and
// the catalog cannot disagree about what the ordering domain of an event type is.
func (f exampleFact) AggregateID() string {
	switch f.reg.PartitionKeyField {
	case "payment_id":
		return fixturePayment
	case "merchant_id":
		return fixtureMerchant
	case "gateway_ref":
		return fixtureWebhook
	case "gateway_id":
		return "simulator"
	case "tenant_id":
		return fixtureTenant
	default:
		return fixtureMerchant
	}
}

// MerchantID is empty for the platform-scoped types, matching the envelope contract: a
// gateway-health or tenant-level audit event belongs to no merchant, and inventing one would make
// every merchant-filtered consumer see events that are not theirs.
func (f exampleFact) MerchantID() string {
	switch f.reg.PartitionKeyField {
	case "gateway_id", "tenant_id":
		return ""
	default:
		return fixtureMerchant
	}
}

func producerContext() context.Context {
	return events.ContextWithProvenance(context.Background(), events.Provenance{
		Source:        fixtureSource,
		CorrelationID: fixtureRequest,
		TraceParent:   fixtureTrace,
	})
}

// loadSchemaFor loads the schema file a registration names.
func loadSchemaFor(t *testing.T, reg events.Registration) *Schema {
	t.Helper()
	s, err := LoadSchema(SchemaDir, reg.SchemaFile)
	if err != nil {
		t.Fatalf("%s: %v", reg.Type, err)
	}
	return s
}

// TestEveryRegisteredEventTypeProducesASchemaConformingEvent is the producer half of the
// consumer-driven contract.
//
// Verifies: baseline §13.1 (the published event catalog is a contract), §13.2 (every type has a
// schema), docs/testing.md §5.2. It produces a *real* event for every registered type — through
// events.EncodeFact, which is the same NewEnvelope → Validate → Encode path the orchestrator uses
// — and validates the result against the published JSON Schema.
//
// The two halves are both load-bearing. Validating the payload catches a producer whose data
// drifted from the schema. Validating the whole envelope against envelope.schema.json catches the
// subtler one: a codec change that alters a field the payload schema says nothing about, such as
// the timestamp format or the dataschema URI, which no per-type schema would ever notice.
func TestEveryRegisteredEventTypeProducesASchemaConformingEvent(t *testing.T) {
	t.Parallel()

	envelopeSchema, err := LoadSchema(SchemaDir, "envelope.schema.json")
	if err != nil {
		t.Fatalf("loading the envelope schema: %v", err)
	}

	registrations := events.AllRegistered()
	if len(registrations) == 0 {
		t.Fatal("the event registry is empty; this suite would pass by asserting nothing")
	}

	for _, reg := range registrations {
		t.Run(reg.Type, func(t *testing.T) {
			t.Parallel()
			schema := loadSchemaFor(t, reg)
			examples := schema.Examples()
			if len(examples) == 0 {
				t.Fatalf("%s declares no examples; there is no instance to produce and therefore "+
					"nothing this suite can verify about the type", reg.Type)
			}

			for i, example := range examples {
				fact := exampleFact{reg: reg, data: example}
				msg, err := events.EncodeFact(producerContext(), fact)
				if err != nil {
					t.Fatalf("example %d: the real codec refused to produce this event: %v", i, err)
				}

				// The routing facts are part of the contract too: a type published to the wrong
				// topic or keyed on the wrong field is not a schema problem and would pass every
				// payload assertion, while silently destroying per-aggregate ordering.
				if msg.Topic != reg.Topic {
					t.Errorf("example %d: published to topic %q, want %q", i, msg.Topic, reg.Topic)
				}
				if msg.Type != reg.Type {
					t.Errorf("example %d: published as type %q, want %q", i, msg.Type, reg.Type)
				}
				if msg.PartitionKey != fact.PartitionKey() {
					t.Errorf("example %d: partition key %q, want %q", i, msg.PartitionKey, fact.PartitionKey())
				}

				// Decode rather than reuse the envelope we built: this is the shape a consumer
				// actually receives, and a marshalling asymmetry is invisible from the producer side.
				env, err := events.Decode(msg)
				if err != nil {
					t.Fatalf("example %d: a consumer cannot decode what the producer wrote: %v", i, err)
				}

				var payload map[string]any
				if err := json.Unmarshal(env.Data, &payload); err != nil {
					t.Fatalf("example %d: event data is not a JSON object: %v", i, err)
				}
				if problems := schema.Validate(payload); len(problems) > 0 {
					t.Errorf("example %d: the produced payload violates %s:\n  %s",
						i, reg.SchemaFile, strings.Join(problems, "\n  "))
				}

				var whole map[string]any
				if err := json.Unmarshal(msg.Payload, &whole); err != nil {
					t.Fatalf("example %d: the outbox payload is not a JSON object: %v", i, err)
				}
				if problems := envelopeSchema.Validate(whole); len(problems) > 0 {
					t.Errorf("example %d: the produced envelope violates envelope.schema.json:\n  %s",
						i, strings.Join(problems, "\n  "))
				}
			}
		})
	}
}

// TestValidatorUnderstandsEverySchemaKeyword is the tripwire that makes the hand-written validator
// safe to rely on.
//
// A hand-rolled schema checker's characteristic failure is silence: a schema grows a construct the
// checker does not implement, the checker ignores it, and every event validates against a rule
// that is no longer enforced. This test converts that into a build failure naming the file and the
// keyword.
func TestValidatorUnderstandsEverySchemaKeyword(t *testing.T) {
	t.Parallel()
	for _, name := range schemaFiles(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, err := LoadSchema(SchemaDir, name)
			if err != nil {
				t.Fatalf("%v", err)
			}
			if unsupported := s.UnsupportedKeywords(); len(unsupported) > 0 {
				t.Fatalf("uses JSON Schema keywords this validator does not implement: %s.\n"+
					"Every event validated against this file is currently NOT being checked for "+
					"those constraints. Implement them in jsonschema.go, or reconsider the "+
					"dependency — do not widen supportedKeywords to make this pass.",
					strings.Join(unsupported, ", "))
			}
		})
	}
}

// TestSchemaExamplesConformToTheirOwnSchema catches the cheapest possible contract defect: an
// example that the schema it illustrates would reject. Consumers copy examples into their fixtures.
func TestSchemaExamplesConformToTheirOwnSchema(t *testing.T) {
	t.Parallel()
	for _, name := range schemaFiles(t) {
		if name == "envelope.schema.json" {
			continue // covered by the producer test, which builds real envelopes
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, err := LoadSchema(SchemaDir, name)
			if err != nil {
				t.Fatalf("%v", err)
			}
			for i, ex := range s.Examples() {
				if problems := s.Validate(ex); len(problems) > 0 {
					t.Errorf("example %d does not satisfy its own schema:\n  %s",
						i, strings.Join(problems, "\n  "))
				}
			}
		})
	}
}

// --- the consumer half ---------------------------------------------------------------------------

// fieldRequirement is one field a consumer reads, and the JSON type it expects.
type fieldRequirement struct {
	// Path is dotted within the event's `data` object, e.g. "capturedAmount.amount".
	Path string
	// Kind is the JSON type: string, integer, number, boolean, object or array.
	Kind string
}

// consumerContract is what one consumer needs from one event type.
//
// Declared in Go rather than in YAML files so that the compiler and `go vet` see it, and so that a
// consumer contract is reviewed in the same diff as the code that depends on it. The `Why` field is
// not decoration: it is the argument for keeping the field, which is the thing a future
// schema-slimming PR needs to read before deleting it.
type consumerContract struct {
	Consumer  string
	EventType string
	Requires  []fieldRequirement
	Why       string
}

// consumerContracts is the registry. It is deliberately restricted to consumers whose correctness
// depends on the field: a consumer that merely displays a value does not belong here, because
// listing everything makes the list mean nothing.
var consumerContracts = []consumerContract{
	{
		Consumer:  "Ledger",
		EventType: "payment.authorized.v1",
		Why:       "The ledger opens the clearing position from the authorization and needs the amount as integer minor units.",
		Requires: []fieldRequirement{
			{"attemptId", "string"},
			{"gatewayCode", "string"},
			{"gatewayReference", "string"},
			{"authorizedAmount.amount", "integer"},
			{"authorizedAmount.currency", "string"},
		},
	},
	{
		Consumer:  "Ledger",
		EventType: "payment.captured.v1",
		Why:       "The ledger posts on the per-capture delta and reconciles on the cumulative total; deriving one from the other is wrong the first time a capture event is redelivered.",
		Requires: []fieldRequirement{
			{"attemptId", "string"},
			{"capturedAmount.amount", "integer"},
			{"capturedAmount.currency", "string"},
			{"capturedTotal.amount", "integer"},
			{"authorizedAmount.amount", "integer"},
			{"isFinalCapture", "boolean"},
		},
	},
	{
		Consumer:  "Ledger",
		EventType: "payment.refunded.v1",
		Why:       "Invariant I1 is asserted by the ledger against refundedTotal and capturedTotal rather than trusted.",
		Requires: []fieldRequirement{
			{"refundId", "string"},
			{"refundedAmount.amount", "integer"},
			{"refundedAmount.currency", "string"},
			{"refundedTotal.amount", "integer"},
			{"capturedTotal.amount", "integer"},
			{"isFullRefund", "boolean"},
		},
	},
	{
		Consumer:  "Ledger",
		EventType: "payment.settled.v1",
		Why:       "Settlement splits gross into fee and net; a consumer that had to compute one of the three would disagree with the gateway's own report by a rounding unit.",
		Requires: []fieldRequirement{
			{"settlementId", "string"},
			{"grossAmount.amount", "integer"},
			{"feeAmount.amount", "integer"},
			{"netAmount.amount", "integer"},
			{"settlementCurrency", "string"},
		},
	},
	{
		Consumer:  "Reconciliation",
		EventType: "payment.settled.v1",
		Why:       "Reconciliation matches our ledger against the gateway's settlement report and needs the report's identity and date.",
		Requires: []fieldRequirement{
			{"settlementId", "string"},
			{"settlementDate", "string"},
			{"reportUri", "string"},
		},
	},
	{
		Consumer:  "Reconciler (alerting)",
		EventType: "payment.reconciliation_required.v1",
		Why:       "The reconciler resolves an unknown outcome by asking the gateway with the deterministic idempotency key; without that key it cannot ask, and the payment stays PROCESSING forever.",
		Requires: []fieldRequirement{
			{"attemptId", "string"},
			{"gatewayCode", "string"},
			{"gatewayIdempotencyKey", "string"},
			{"reason", "string"},
			{"paymentState", "string"},
			{"amountAtRisk.amount", "integer"},
			{"resolveBy", "string"},
		},
	},
	{
		Consumer:  "Routing",
		EventType: "gateway.health_changed.v1",
		Why:       "Routing shifts traffic on the health transition; the circuit state and error rate are the inputs to that decision.",
		Requires: []fieldRequirement{
			{"gatewayId", "string"},
			{"operation", "string"},
			{"state", "string"},
			{"circuitState", "string"},
			{"errorRate", "number"},
			{"sampleCount", "integer"},
		},
	},
	{
		Consumer:  "Data plane cache",
		EventType: "merchant.activated.v1",
		Why:       "The data plane's fail-static snapshot is built from this event; a missing configuration version makes the snapshot unversionable and therefore unrollbackable.",
		Requires: []fieldRequirement{
			{"merchantId", "string"},
			{"configurationVersion", "integer"},
			{"certifiedConnections", "array"},
			{"supportedCurrencies", "array"},
		},
	},
	{
		Consumer:  "Webhook processor",
		EventType: "webhook.received.v1",
		Why:       "The processor re-verifies rather than trusting the ingress, and deduplicates on the gateway's own reference.",
		Requires: []fieldRequirement{
			{"webhookId", "string"},
			{"gatewayCode", "string"},
			{"gatewayRef", "string"},
			{"signatureValid", "boolean"},
			{"bodySha256", "string"},
		},
	},
	{
		Consumer:  "Audit sink",
		EventType: "audit.recorded.v1",
		Why:       "The hash chain is the audit log's tamper evidence; a sink that cannot see both links cannot verify it.",
		Requires: []fieldRequirement{
			{"auditId", "string"},
			{"actorId", "string"},
			{"action", "string"},
			{"prevHash", "string"},
			{"entryHash", "string"},
		},
	},
	{
		Consumer:  "Analytics",
		EventType: "payment.attempted.v1",
		Why:       "Attempt-level analytics is how a routing regression is noticed before a merchant reports it.",
		Requires: []fieldRequirement{
			{"paymentId", "string"},
			{"attemptId", "string"},
			{"attemptNumber", "integer"},
			{"gatewayCode", "string"},
			{"operation", "string"},
			{"state", "string"},
		},
	},
}

// TestEveryConsumerContractIsSatisfied is the consumer half.
//
// Verifies: docs/testing.md §5.2 (consumer-driven contracts), baseline §13.1 (additive-only within
// a major). It produces a real event of each contracted type through the codec and asserts every
// field the consumer declared is present, with the declared JSON type.
//
// Schema conformance would not catch this. A producer may legally stop populating an *optional*
// field and remain schema-valid while every consumer that depended on it breaks. This test is the
// only place that says the field is actually load-bearing.
func TestEveryConsumerContractIsSatisfied(t *testing.T) {
	t.Parallel()
	for _, cc := range consumerContracts {
		t.Run(cc.Consumer+"/"+cc.EventType, func(t *testing.T) {
			t.Parallel()
			reg, err := events.Lookup(cc.EventType)
			if err != nil {
				t.Fatalf("consumer %q declares a contract on %q, which is not in the catalog: %v",
					cc.Consumer, cc.EventType, err)
			}
			schema := loadSchemaFor(t, reg)
			examples := schema.Examples()
			if len(examples) == 0 {
				t.Fatalf("%s has no example to produce", cc.EventType)
			}

			for i, example := range examples {
				msg, err := events.EncodeFact(producerContext(), exampleFact{reg: reg, data: example})
				if err != nil {
					t.Fatalf("example %d: encoding: %v", i, err)
				}
				env, err := events.Decode(msg)
				if err != nil {
					t.Fatalf("example %d: decoding: %v", i, err)
				}
				var payload map[string]any
				if err := json.Unmarshal(env.Data, &payload); err != nil {
					t.Fatalf("example %d: payload: %v", i, err)
				}

				for _, req := range cc.Requires {
					v, ok := lookupPath(payload, req.Path)
					if !ok {
						t.Errorf("example %d: %s reads %s, which the producer did not supply.\n  Why it matters: %s",
							i, cc.Consumer, req.Path, cc.Why)
						continue
					}
					if !typeNameMatches(req.Kind, v) {
						t.Errorf("example %d: %s reads %s as %s, but the producer supplied %s (%v)",
							i, cc.Consumer, req.Path, req.Kind, kindOf(v), v)
					}
				}
			}
		})
	}
}

// TestEveryContractedConsumerIsDeclaredBySchema closes the loop between the code and the document.
//
// A consumer contract asserted in Go and a consumer list published in the schema that disagree is
// the state where the schema stops being a usable description of who depends on the type — which
// is precisely the information a producer needs before changing it.
func TestEveryContractedConsumerIsDeclaredBySchema(t *testing.T) {
	t.Parallel()
	for _, cc := range consumerContracts {
		t.Run(cc.Consumer+"/"+cc.EventType, func(t *testing.T) {
			t.Parallel()
			reg, err := events.Lookup(cc.EventType)
			if err != nil {
				t.Fatalf("%v", err)
			}
			declared := loadSchemaFor(t, reg).StringSlice("x-consumers")
			for _, d := range declared {
				if d == cc.Consumer {
					return
				}
			}
			t.Fatalf("%s asserts a contract on %s, but the schema's x-consumers lists only %v.\n"+
				"Either the schema under-declares its dependants — in which case a producer will "+
				"break this consumer without ever seeing its name — or this contract names the "+
				"consumer differently from the schema.", cc.Consumer, cc.EventType, declared)
		})
	}
}

// lookupPath walks a dotted path through a decoded JSON object.
func lookupPath(doc map[string]any, path string) (any, bool) {
	cur := any(doc)
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// schemaFiles lists every *.schema.json in api/events, sorted.
func schemaFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(SchemaDir)
	if err != nil {
		t.Fatalf("reading %s: %v", SchemaDir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".schema.json") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatalf("no schema files under %s; this suite would pass by asserting nothing", SchemaDir)
	}
	return out
}
