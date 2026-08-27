package contract

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// parseSchema builds a Schema from a literal, for the checker's own table tests.
func parseSchema(t *testing.T, name, body string) *Schema {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return &Schema{Name: name, Root: root}
}

// baseSchema is the "previous major version" every compatibility case starts from.
const baseSchema = `{
  "type": "object",
  "required": ["paymentId", "amount"],
  "additionalProperties": false,
  "properties": {
    "paymentId": { "type": "string", "pattern": "^pay_[0-9A-Z]{26}$" },
    "amount":    { "$ref": "#/$defs/money" },
    "reason":    { "type": "string", "enum": ["A", "B"] },
    "tags":      { "type": "array", "items": { "type": "string" }, "maxItems": 8 }
  },
  "$defs": {
    "money": {
      "type": "object",
      "required": ["amount", "currency"],
      "additionalProperties": false,
      "properties": {
        "amount":   { "type": "integer", "minimum": 0 },
        "currency": { "type": "string", "pattern": "^[A-Z]{3}$" }
      }
    }
  }
}`

// TestCompatibilityCheckerDetectsEachBreakingChange exercises the checker itself.
//
// Verifies: baseline §13.1 (additive-only within a major version), docs/testing.md §5.2.
//
// This test exists because of an uncomfortable fact about the schemas as they stand today: every
// published type is at `.v1`, so there is no previous major to compare against and
// TestPublishedSchemasAreBackwardCompatible below has nothing to do. A compatibility gate that
// asserts nothing until the day someone ships a `.v2` is a gate that will be discovered to be
// broken on exactly that day. Table-driving the checker against synthetic pairs keeps it honest in
// the meantime, and each row names the class of break it represents.
func TestCompatibilityCheckerDetectsEachBreakingChange(t *testing.T) {
	t.Parallel()

	// mutate applies a JSON patch expressed as a whole replacement document, keeping the cases
	// readable as "the previous schema, but ...".
	cases := []struct {
		name string
		// cur is the candidate next version.
		cur string
		// wantBreak is a substring of the expected finding; empty means the change is compatible.
		wantBreak string
	}{
		{
			name: "adding an optional property is additive",
			cur: `{"type":"object","required":["paymentId","amount"],"additionalProperties":false,
			       "properties":{
			         "paymentId":{"type":"string","pattern":"^pay_[0-9A-Z]{26}$"},
			         "amount":{"$ref":"#/$defs/money"},
			         "reason":{"type":"string","enum":["A","B"]},
			         "tags":{"type":"array","items":{"type":"string"},"maxItems":8},
			         "note":{"type":"string"}},
			       "$defs":{"money":{"type":"object","required":["amount","currency"],
			         "additionalProperties":false,
			         "properties":{"amount":{"type":"integer","minimum":0},
			                       "currency":{"type":"string","pattern":"^[A-Z]{3}$"}}}}}`,
		},
		{
			name: "adding an enum value to an existing field is additive",
			cur: `{"type":"object","required":["paymentId","amount"],"additionalProperties":false,
			       "properties":{
			         "paymentId":{"type":"string","pattern":"^pay_[0-9A-Z]{26}$"},
			         "amount":{"$ref":"#/$defs/money"},
			         "reason":{"type":"string","enum":["A","B","C"]},
			         "tags":{"type":"array","items":{"type":"string"},"maxItems":8}},
			       "$defs":{"money":{"type":"object","required":["amount","currency"],
			         "additionalProperties":false,
			         "properties":{"amount":{"type":"integer","minimum":0},
			                       "currency":{"type":"string","pattern":"^[A-Z]{3}$"}}}}}`,
		},
		{
			name: "removing a property breaks every consumer that reads it",
			cur: `{"type":"object","required":["paymentId","amount"],"additionalProperties":false,
			       "properties":{
			         "paymentId":{"type":"string","pattern":"^pay_[0-9A-Z]{26}$"},
			         "amount":{"$ref":"#/$defs/money"},
			         "tags":{"type":"array","items":{"type":"string"},"maxItems":8}},
			       "$defs":{"money":{"type":"object","required":["amount","currency"],
			         "additionalProperties":false,
			         "properties":{"amount":{"type":"integer","minimum":0},
			                       "currency":{"type":"string","pattern":"^[A-Z]{3}$"}}}}}`,
			wantBreak: `property "reason" was removed`,
		},
		{
			name: "adding a required property breaks the archive",
			cur: `{"type":"object","required":["paymentId","amount","note"],"additionalProperties":false,
			       "properties":{
			         "paymentId":{"type":"string","pattern":"^pay_[0-9A-Z]{26}$"},
			         "amount":{"$ref":"#/$defs/money"},
			         "reason":{"type":"string","enum":["A","B"]},
			         "tags":{"type":"array","items":{"type":"string"},"maxItems":8},
			         "note":{"type":"string"}},
			       "$defs":{"money":{"type":"object","required":["amount","currency"],
			         "additionalProperties":false,
			         "properties":{"amount":{"type":"integer","minimum":0},
			                       "currency":{"type":"string","pattern":"^[A-Z]{3}$"}}}}}`,
			wantBreak: `new property "note" was added as required`,
		},
		{
			name: "demoting a required property to optional breaks readers",
			cur: `{"type":"object","required":["paymentId"],"additionalProperties":false,
			       "properties":{
			         "paymentId":{"type":"string","pattern":"^pay_[0-9A-Z]{26}$"},
			         "amount":{"$ref":"#/$defs/money"},
			         "reason":{"type":"string","enum":["A","B"]},
			         "tags":{"type":"array","items":{"type":"string"},"maxItems":8}},
			       "$defs":{"money":{"type":"object","required":["amount","currency"],
			         "additionalProperties":false,
			         "properties":{"amount":{"type":"integer","minimum":0},
			                       "currency":{"type":"string","pattern":"^[A-Z]{3}$"}}}}}`,
			wantBreak: `property "amount" was demoted from required to optional`,
		},
		{
			name: "changing a property's type breaks every decode",
			cur: `{"type":"object","required":["paymentId","amount"],"additionalProperties":false,
			       "properties":{
			         "paymentId":{"type":"string","pattern":"^pay_[0-9A-Z]{26}$"},
			         "amount":{"$ref":"#/$defs/money"},
			         "reason":{"type":"string","enum":["A","B"]},
			         "tags":{"type":"string"}},
			       "$defs":{"money":{"type":"object","required":["amount","currency"],
			         "additionalProperties":false,
			         "properties":{"amount":{"type":"integer","minimum":0},
			                       "currency":{"type":"string","pattern":"^[A-Z]{3}$"}}}}}`,
			wantBreak: `type changed from "array" to "string"`,
		},
		{
			name: "removing an enum value invalidates already-published events",
			cur: `{"type":"object","required":["paymentId","amount"],"additionalProperties":false,
			       "properties":{
			         "paymentId":{"type":"string","pattern":"^pay_[0-9A-Z]{26}$"},
			         "amount":{"$ref":"#/$defs/money"},
			         "reason":{"type":"string","enum":["A"]},
			         "tags":{"type":"array","items":{"type":"string"},"maxItems":8}},
			       "$defs":{"money":{"type":"object","required":["amount","currency"],
			         "additionalProperties":false,
			         "properties":{"amount":{"type":"integer","minimum":0},
			                       "currency":{"type":"string","pattern":"^[A-Z]{3}$"}}}}}`,
			wantBreak: "enum value B was removed",
		},
		{
			name: "narrowing a numeric bound rejects previously valid instances",
			cur: `{"type":"object","required":["paymentId","amount"],"additionalProperties":false,
			       "properties":{
			         "paymentId":{"type":"string","pattern":"^pay_[0-9A-Z]{26}$"},
			         "amount":{"$ref":"#/$defs/money"},
			         "reason":{"type":"string","enum":["A","B"]},
			         "tags":{"type":"array","items":{"type":"string"},"maxItems":4}},
			       "$defs":{"money":{"type":"object","required":["amount","currency"],
			         "additionalProperties":false,
			         "properties":{"amount":{"type":"integer","minimum":0},
			                       "currency":{"type":"string","pattern":"^[A-Z]{3}$"}}}}}`,
			wantBreak: "maxItems was lowered from 8 to 4",
		},
		{
			name: "tightening a nested $def is just as breaking as tightening the root",
			cur: `{"type":"object","required":["paymentId","amount"],"additionalProperties":false,
			       "properties":{
			         "paymentId":{"type":"string","pattern":"^pay_[0-9A-Z]{26}$"},
			         "amount":{"$ref":"#/$defs/money"},
			         "reason":{"type":"string","enum":["A","B"]},
			         "tags":{"type":"array","items":{"type":"string"},"maxItems":8}},
			       "$defs":{"money":{"type":"object","required":["amount","currency","exponent"],
			         "additionalProperties":false,
			         "properties":{"amount":{"type":"integer","minimum":1},
			                       "currency":{"type":"string","pattern":"^[A-Z]{3}$"},
			                       "exponent":{"type":"integer"}}}}}`,
			wantBreak: "minimum was raised from 0 to 1",
		},
	}

	prev := parseSchema(t, "previous", baseSchema)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cur := parseSchema(t, "current", tc.cur)
			found := BreakingChanges(prev, cur)

			if tc.wantBreak == "" {
				if len(found) > 0 {
					t.Fatalf("an additive change was reported as breaking:\n  %s",
						strings.Join(found, "\n  "))
				}
				return
			}
			for _, f := range found {
				if strings.Contains(f, tc.wantBreak) {
					return
				}
			}
			t.Fatalf("the checker did not detect this break.\n  want a finding containing: %s\n  got: %v",
				tc.wantBreak, found)
		})
	}
}

// TestPublishedSchemasAreBackwardCompatible is the gate on the real schemas.
//
// Verifies: baseline §13.1. For every published type with more than one major version, the newer
// major must be a superset of the older one in the additive sense — because §13.1's migration
// protocol dual-publishes `.v1` and `.v2` from the same transaction, and a consumer that has not
// yet migrated is still reading the older type off the same topic.
//
// With only `.v1` types present the loop is empty, and the test says so out loud rather than
// reporting a silent pass: a gate that cannot distinguish "nothing to check" from "everything
// checked" is a gate nobody can trust when it matters.
func TestPublishedSchemasAreBackwardCompatible(t *testing.T) {
	t.Parallel()

	byBase := map[string]map[int]string{} // base name -> major -> file
	for _, name := range schemaFiles(t) {
		if name == "envelope.schema.json" {
			continue
		}
		eventType := strings.TrimSuffix(name, ".schema.json")
		base, major, ok := SplitVersion(eventType)
		if !ok {
			t.Errorf("%s: the file name does not carry a major version; the version lives in the "+
				"type name and every schema file must therefore end .v<major>.schema.json", name)
			continue
		}
		if byBase[base] == nil {
			byBase[base] = map[int]string{}
		}
		byBase[base][major] = name
	}

	compared := 0
	bases := make([]string, 0, len(byBase))
	for b := range byBase {
		bases = append(bases, b)
	}
	sort.Strings(bases)

	for _, base := range bases {
		majors := byBase[base]
		versions := make([]int, 0, len(majors))
		for v := range majors {
			versions = append(versions, v)
		}
		sort.Ints(versions)

		for _, v := range versions {
			if v == 1 {
				continue
			}
			// A major bump must keep the previous major on disk: §13.1's protocol is
			// dual-publish, and deleting the old schema strands every consumer that has not
			// migrated with a `dataschema` URI that resolves to nothing.
			prevName, ok := majors[v-1]
			if !ok {
				t.Errorf("%s.v%d exists but %s.v%d does not. A new major is published *alongside* "+
					"its predecessor for the whole migration window, never instead of it.",
					base, v, base, v-1)
				continue
			}
			t.Run(fmt.Sprintf("%s.v%d->v%d", base, v-1, v), func(t *testing.T) {
				prev, err := LoadSchema(SchemaDir, prevName)
				if err != nil {
					t.Fatalf("%v", err)
				}
				cur, err := LoadSchema(SchemaDir, majors[v])
				if err != nil {
					t.Fatalf("%v", err)
				}
				if problems := BreakingChanges(prev, cur); len(problems) > 0 {
					t.Fatalf("%s.v%d is not backward compatible with v%d:\n  %s\n"+
						"Within a major version only additive changes to optional fields are "+
						"permitted; a breaking change is a new major published alongside this one.",
						base, v, v-1, strings.Join(problems, "\n  "))
				}
			})
			compared++
		}
	}

	if compared == 0 {
		t.Logf("every published type is at .v1, so there is no previous major to compare against. "+
			"The checker itself is covered by TestCompatibilityCheckerDetectsEachBreakingChange; "+
			"this test starts doing work the moment a .v2 appears. (%d types inspected)", len(byBase))
	}
}

// TestEverySchemaIsSelfDescribingForCompatibility asserts the two properties that make an additive
// change *possible* to make safely.
//
// Without `additionalProperties: false` a consumer cannot tell a new field from a typo, so the
// additive-change protocol has no meaning; without a `required` list that is a subset of the
// declared properties, the schema is internally inconsistent and the compatibility checker's
// reasoning about required-ness is comparing against a fiction.
func TestEverySchemaIsSelfDescribingForCompatibility(t *testing.T) {
	t.Parallel()
	for _, name := range schemaFiles(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, err := LoadSchema(SchemaDir, name)
			if err != nil {
				t.Fatalf("%v", err)
			}
			if additionalAllowed(s.Root) {
				t.Errorf("the root object does not set additionalProperties:false, so a consumer " +
					"cannot distinguish an additive change from a producer typo")
			}
			props := propsOf(s.Root)
			for _, req := range requiredOf(s.Root) {
				if _, declared := props[req]; !declared {
					t.Errorf("required names %q, which is not among the declared properties", req)
				}
			}
			if _, ok := s.Root["$id"].(string); !ok {
				t.Errorf("no $id: the dataschema URI in every envelope of this type points here, " +
					"and a schema that does not name itself cannot be resolved from an archived event")
			}
		})
	}
}
