package idempotency_test

import (
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/platform/idempotency"
)

func TestCanonicalize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"member order", `{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{"nested member order", `{"z":{"y":1,"x":2},"a":3}`, `{"a":3,"z":{"x":2,"y":1}}`},
		{"insignificant whitespace", "{\n  \"a\" : 1 ,\n  \"b\": [ 1, 2 ]\n}", `{"a":1,"b":[1,2]}`},
		{"array order is semantic", `[3,1,2]`, `[3,1,2]`},
		{"null is preserved", `{"a":null}`, `{"a":null}`},
		{"booleans", `{"a":true,"b":false}`, `{"a":true,"b":false}`},
		{"empty object and array", `{"a":{},"b":[]}`, `{"a":{},"b":[]}`},

		// Number canonicalization: the same value written five ways is one canonical form.
		{"integer forms", `{"a":1,"b":1.0,"c":1e0,"d":1.0e+0,"e":10e-1}`, `{"a":1,"b":1,"c":1,"d":1,"e":1}`},
		{"negative zero", `{"a":-0,"b":-0.0,"c":0}`, `{"a":0,"b":0,"c":0}`},
		{"minor units stay integers", `{"amount":1050}`, `{"amount":1050}`},
		{"fraction", `{"a":0.5}`, `{"a":0.5}`},
		{"large integer", `{"a":9007199254740992}`, `{"a":9007199254740992}`},
		// An int64 literal is rendered from the integer, not through a float, so a value above
		// 2^53 survives exactly instead of being rounded into a neighbour's fingerprint.
		{"integer above 2^53", `{"a":9007199254740993}`, `{"a":9007199254740993}`},
		// ECMAScript switches to exponential at 1e21, and writes a one-digit exponent where Go's
		// %g would write "1e+21"/"1e-07".
		{"exponential upper threshold", `{"a":1e21}`, `{"a":1e+21}`},
		{"just below the threshold", `{"a":1e20}`, `{"a":100000000000000000000}`},
		{"exponential lower threshold", `{"a":1e-7}`, `{"a":1e-7}`},
		{"just above the lower threshold", `{"a":1e-6}`, `{"a":0.000001}`},
		{"multi-digit mantissa", `{"a":1.234e22}`, `{"a":1.234e+22}`},
		{"negative exponential", `{"a":-1.5e-9}`, `{"a":-1.5e-9}`},

		// String escaping: the two mandatory escapes, the short forms, \u00xx for other control
		// characters, and literal UTF-8 for everything else.
		{"escapes", `{"a":"q\"b\\c\nd\te"}`, `{"a":"q\"b\\c\nd\te"}`},
		{"control character", `{"a":"\u0001"}`, `{"a":"\u0001"}`},
		{"control character short form", `{"a":"\u000a"}`, `{"a":"\n"}`},
		{"solidus is not escaped", `{"a":"a/b"}`, `{"a":"a/b"}`},
		// encoding/json would emit < here; JCS must not.
		{"html characters stay literal", `{"a":"<&>"}`, `{"a":"<&>"}`},
		{"non-ascii stays literal", `{"a":"Ünïcodé"}`, `{"a":"Ünïcodé"}`},

		// RFC 8785 §3.2.3: keys sort by UTF-16 code unit, so a supplementary-plane key (encoded
		// as a surrogate pair, 0xD800–0xDBFF) sorts BELOW U+E000–U+FFFF, the opposite of what a
		// UTF-8 byte sort would give.
		{"utf-16 key order", "{\"\":1,\"\U0001f600\":2}", "{\"\U0001f600\":2,\"\":1}"},
		{"ascii key order", `{"B":1,"a":2,"A":3}`, `{"A":3,"B":1,"a":2}`},

		{"scalar top level", `42`, `42`},
		{"string top level", `"hi"`, `"hi"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := idempotency.Canonicalize([]byte(tc.in))
			if err != nil {
				t.Fatalf("Canonicalize(%s): %v", tc.in, err)
			}
			if string(got) != tc.want {
				t.Fatalf("Canonicalize(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestCanonicalizeIsIdempotent(t *testing.T) {
	t.Parallel()
	in := []byte(`{"z":[{"b":1.0,"a":"x"}],"y":1e2,"a":null}`)
	once, err := idempotency.Canonicalize(in)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := idempotency.Canonicalize(once)
	if err != nil {
		t.Fatal(err)
	}
	if string(once) != string(twice) {
		t.Fatalf("canonicalization is not a fixed point: %s vs %s", once, twice)
	}
}

func TestCanonicalizeRejects(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, in string }{
		{"empty", ``},
		{"truncated object", `{"a":1`},
		{"trailing content", `{"a":1} {"b":2}`},
		{"duplicate member", `{"a":1,"a":2}`},
		{"duplicate member nested", `{"x":{"a":1,"a":2}}`},
		{"not json", `nope`},
		{"integer beyond int64 that loses precision", `{"a":123456789012345678901234567890123}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := idempotency.Canonicalize([]byte(tc.in)); err == nil {
				t.Fatalf("Canonicalize(%q) must fail", tc.in)
			}
		})
	}
}
