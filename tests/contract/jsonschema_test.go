package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestValidatorRejectsWhatTheSchemaForbids is the validator's own negative suite.
//
// Verifies: nothing about the platform — everything about the tool the rest of this package trusts.
// A schema checker that returns "no problems" for every input passes every conformance test in this
// directory while proving nothing, and that failure mode is silent by construction. Each row below
// is one keyword the event schemas actually depend on, with an instance that must be rejected.
func TestValidatorRejectsWhatTheSchemaForbids(t *testing.T) {
	t.Parallel()

	const schemaBody = `{
      "type": "object",
      "required": ["id", "amount", "state"],
      "additionalProperties": false,
      "properties": {
        "id":     { "type": "string", "pattern": "^pay_[0-9A-Z]{26}$", "maxLength": 30 },
        "amount": { "$ref": "#/$defs/money" },
        "state":  { "type": "string", "enum": ["OPEN", "CLOSED"] },
        "count":  { "type": "integer", "minimum": 1, "maximum": 4 },
        "when":   { "type": "string", "format": "date-time" },
        "tags":   { "type": "array", "items": { "type": "string" }, "maxItems": 2 },
        "kind":   { "const": "payment" }
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

	schema := parseSchema(t, "self-test", schemaBody)
	const validDoc = `{"id":"pay_01JC0NTRACT00000000000000D","amount":{"amount":1050,"currency":"USD"},
	                   "state":"OPEN","count":2,"when":"2026-08-26T15:20:44.771Z","tags":["a"],
	                   "kind":"payment"}`

	cases := []struct {
		name string
		doc  string
		want string // substring of the expected problem; empty means the document must be accepted
	}{
		{name: "the valid document is accepted", doc: validDoc},
		{
			name: "a missing required field is reported",
			doc:  `{"amount":{"amount":1,"currency":"USD"},"state":"OPEN"}`,
			want: "data.id: required field is missing",
		},
		{
			name: "an undeclared field is reported when additionalProperties is false",
			doc:  `{"id":"pay_01JC0NTRACT00000000000000D","amount":{"amount":1,"currency":"USD"},"state":"OPEN","surprise":1}`,
			want: "undeclared field",
		},
		{
			name: "a wrong type is reported",
			doc:  `{"id":7,"amount":{"amount":1,"currency":"USD"},"state":"OPEN"}`,
			want: `data.id: is integer, want type string`,
		},
		{
			name: "a non-integral number is not an integer",
			doc:  `{"id":"pay_01JC0NTRACT00000000000000D","amount":{"amount":10.5,"currency":"USD"},"state":"OPEN"}`,
			want: "data.amount.amount: is number, want type integer",
		},
		{
			name: "a pattern violation is reported",
			doc:  `{"id":"nope","amount":{"amount":1,"currency":"USD"},"state":"OPEN"}`,
			want: "does not match",
		},
		{
			name: "a value outside the enum is reported",
			doc:  `{"id":"pay_01JC0NTRACT00000000000000D","amount":{"amount":1,"currency":"USD"},"state":"HALF"}`,
			want: "which is not in the enum",
		},
		{
			name: "a numeric bound violation is reported",
			doc:  `{"id":"pay_01JC0NTRACT00000000000000D","amount":{"amount":1,"currency":"USD"},"state":"OPEN","count":9}`,
			want: "above maximum",
		},
		{
			name: "a malformed date-time is reported",
			doc:  `{"id":"pay_01JC0NTRACT00000000000000D","amount":{"amount":1,"currency":"USD"},"state":"OPEN","when":"yesterday"}`,
			want: "is not an RFC 3339 date-time",
		},
		{
			name: "an array element of the wrong type is reported",
			doc:  `{"id":"pay_01JC0NTRACT00000000000000D","amount":{"amount":1,"currency":"USD"},"state":"OPEN","tags":["a",3]}`,
			want: "data.tags[1]: is integer, want type string",
		},
		{
			name: "too many array items is reported",
			doc:  `{"id":"pay_01JC0NTRACT00000000000000D","amount":{"amount":1,"currency":"USD"},"state":"OPEN","tags":["a","b","c"]}`,
			want: "above maxItems",
		},
		{
			name: "a const mismatch is reported",
			doc:  `{"id":"pay_01JC0NTRACT00000000000000D","amount":{"amount":1,"currency":"USD"},"state":"OPEN","kind":"refund"}`,
			want: "want the const",
		},
		{
			name: "a violation inside a $ref'd definition is reported",
			doc:  `{"id":"pay_01JC0NTRACT00000000000000D","amount":{"amount":1,"currency":"usd"},"state":"OPEN"}`,
			want: "data.amount.currency",
		},
		{
			name: "a missing field inside a $ref'd definition is reported",
			doc:  `{"id":"pay_01JC0NTRACT00000000000000D","amount":{"amount":1},"state":"OPEN"}`,
			want: "data.amount.currency: required field is missing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var doc any
			if err := json.Unmarshal([]byte(tc.doc), &doc); err != nil {
				t.Fatalf("test document is not valid JSON: %v", err)
			}
			problems := schema.Validate(doc)

			if tc.want == "" {
				if len(problems) > 0 {
					t.Fatalf("a conforming document was rejected:\n  %s", strings.Join(problems, "\n  "))
				}
				return
			}
			for _, p := range problems {
				if strings.Contains(p, tc.want) {
					return
				}
			}
			t.Fatalf("the validator did not report this violation.\n  want a problem containing: %s\n  got: %v",
				tc.want, problems)
		})
	}
}
