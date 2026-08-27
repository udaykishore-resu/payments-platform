package idempotency

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Canonicalize renders a JSON document in JCS form (RFC 8785).
//
// # Why this exists and why getting it wrong is expensive
//
// The idempotency fingerprint is SHA-256 over the request body (baseline §14.2), and a
// fingerprint that disagrees with a stored one produces 422 IDEMPOTENCY_KEY_REUSED. That error
// says "you reused one key for two different operations" — a serious accusation to make about a
// merchant's integration. If the hash is taken over the raw bytes, then a client that
// serializes its map with a different key order on the retry, or pretty-prints on one path and
// compacts on another, or emits `1.0` where it previously emitted `1`, gets accused of a bug it
// does not have. It will look, correctly, like a platform defect: the request "is the same" and
// the platform says it is not.
//
// Canonicalization is what makes the fingerprint a function of the request's *meaning* rather
// than of the client's serializer. The three rules that do the work:
//
//   - Object keys are sorted by UTF-16 code unit, so member order is not significant.
//   - No insignificant whitespace, so pretty-printing is not significant.
//   - Numbers are emitted in the canonical ECMAScript form, so `1`, `1.0` and `1e0` are one
//     value — as they are to every JSON parser the client might be using.
//
// # What it deliberately does not do
//
// It does not reorder arrays (order is semantic in JSON), does not drop null members (a member
// explicitly set to null differs from an absent one), and does not normalize Unicode (NFC vs
// NFD is a different string, and silently equating them would make two distinguishable payment
// descriptors share a fingerprint).
//
// Duplicate object keys are an error rather than a last-wins merge: two parsers disagree about
// which value wins, so a document with duplicates has no single meaning to canonicalize, and
// guessing is how one side of a system fingerprints a different document than the other side
// processes.
func Canonicalize(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	// Reject trailing content. `{"a":1} {"b":2}` is two documents; accepting it would mean the
	// fingerprint covers only the first, and everything after it would be invisible to the
	// duplicate check.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("jcs: trailing content after the top-level JSON value")
	}

	var buf bytes.Buffer
	if err := writeValue(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// member is one object entry, kept as a slice rather than a map so duplicate keys are
// detectable and so the sort is explicit and reviewable.
type member struct {
	key   string
	utf16 []uint16
	value any
}

// object is a decoded JSON object with its members in input order.
type object struct{ members []member }

func parseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("jcs: empty document")
		}
		return nil, fmt.Errorf("jcs: %w", err)
	}
	return parseFromToken(dec, tok)
}

func parseFromToken(dec *json.Decoder, tok json.Token) (any, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return parseObject(dec)
		case '[':
			return parseArray(dec)
		default:
			return nil, fmt.Errorf("jcs: unexpected %q", t)
		}
	default:
		return tok, nil
	}
}

func parseObject(dec *json.Decoder) (any, error) {
	obj := &object{}
	seen := make(map[string]struct{})
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("jcs: %w", err)
		}
		if d, ok := tok.(json.Delim); ok && d == '}' {
			return obj, nil
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("jcs: object key is not a string")
		}
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("jcs: duplicate object member %q", key)
		}
		seen[key] = struct{}{}

		val, err := parseValue(dec)
		if err != nil {
			return nil, err
		}
		obj.members = append(obj.members, member{key: key, utf16: utf16.Encode([]rune(key)), value: val})
	}
}

func parseArray(dec *json.Decoder) (any, error) {
	arr := []any{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("jcs: %w", err)
		}
		if d, ok := tok.(json.Delim); ok && d == ']' {
			return arr, nil
		}
		val, err := parseFromToken(dec, tok)
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)
	}
}

func writeValue(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeString(buf, t)
	case json.Number:
		s, err := canonicalNumber(t)
		if err != nil {
			return err
		}
		buf.WriteString(s)
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeValue(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case *object:
		// Sorted by UTF-16 code unit, per RFC 8785 §3.2.3. This is not the same as sorting the
		// UTF-8 bytes: a supplementary-plane character encodes as a surrogate pair in the
		// 0xD800–0xDFFF range, which sorts *below* U+E000–U+FFFF, whereas its UTF-8 bytes sort
		// above. Getting that wrong produces a fingerprint that differs from every other
		// conforming implementation's, which only shows up when a client canonicalizes too.
		sort.SliceStable(t.members, func(i, j int) bool {
			return compareUTF16(t.members[i].utf16, t.members[j].utf16) < 0
		})
		buf.WriteByte('{')
		for i, m := range t.members {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, m.key)
			buf.WriteByte(':')
			if err := writeValue(buf, m.value); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("jcs: unsupported value of type %T", v)
	}
	return nil
}

func compareUTF16(a, b []uint16) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return len(a) - len(b)
}

// writeString emits a JSON string with the minimal escaping RFC 8785 mandates: the two
// mandatory escapes, the six short forms, and \u00xx for the remaining control characters.
// Everything else is written as literal UTF-8. encoding/json's own encoder is not used here
// because it additionally escapes `<`, `>` and `&` for HTML safety, which is a correct default
// for a web response and wrong for a canonical form — it would make our fingerprint disagree
// with every other JCS implementation.
func writeString(buf *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				buf.WriteString(`\u00`)
				buf.WriteByte(hex[byte(r)>>4])
				buf.WriteByte(hex[byte(r)&0xF])
				continue
			}
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
}

// canonicalNumber renders a JSON number in the ECMAScript `Number::toString` form that RFC 8785
// requires.
//
// The subtlety, and the reason this is not `strconv.FormatFloat(v, 'g', -1, 64)`: Go's 'g'
// verb picks fixed vs exponential notation on different thresholds than ECMAScript, and writes
// a two-digit exponent (`1e-07`) where ECMAScript writes `1e-7`. Both are valid JSON and they
// hash differently, so a Go producer and a JavaScript producer of the same value would not
// agree on the fingerprint — which is exactly the class of spurious IDEMPOTENCY_KEY_REUSED this
// whole file exists to prevent.
//
// Integers are printed from the integer, not through the float, so that a value like
// 9007199254740993 in a `long` field survives if it arrived as an integer literal. A value that
// is not representable as a float64 is rejected rather than silently rounded: the alternative
// is two different amounts sharing a fingerprint, which in a payments platform is a
// double-charge with a plausible explanation.
func canonicalNumber(n json.Number) (string, error) {
	s := n.String()
	// Fast, exact path for integer literals within int64. It also covers the common case, so
	// the float round-trip below is rare.
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return strconv.FormatInt(i, 10), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return "", fmt.Errorf("jcs: number %q is not representable", s)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("jcs: number %q is not finite", s)
	}
	// An integer literal that exceeded int64 but is exactly representable as a float64 is
	// fine; one that is not exactly representable is rejected, because rounding it here would
	// make two distinct requests fingerprint identically.
	if !strings.ContainsAny(s, ".eE") {
		if big, ok := new(bigDecimalCheck).exact(s, f); !ok {
			return "", fmt.Errorf("jcs: integer %q loses precision as a float64 (%s)", s, big)
		}
	}
	return formatECMAScript(f), nil
}

// bigDecimalCheck round-trips an integer literal through float64 and compares the decimal
// rendering, which is enough to detect the loss without a bignum dependency.
type bigDecimalCheck struct{}

func (bigDecimalCheck) exact(literal string, f float64) (string, bool) {
	round := strconv.FormatFloat(f, 'f', -1, 64)
	trimmed := strings.TrimPrefix(literal, "+")
	return round, round == trimmed
}

// formatECMAScript implements ECMA-262 Number::toString for a finite float64.
func formatECMAScript(f float64) string {
	if f == 0 {
		// Covers -0, which ECMAScript renders as "0". A signed zero that hashed differently
		// from an unsigned one would be a fingerprint difference nobody could see in a diff.
		return "0"
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	// 'e' with precision -1 gives the shortest digit string that round-trips, which is exactly
	// the `s` and `k` of the specification, plus the exponent.
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, expPart, _ := strings.Cut(sci, "e")
	exp, err := strconv.Atoi(expPart)
	if err != nil {
		return sign + sci
	}
	digits := strings.Replace(mantissa, ".", "", 1)
	k := len(digits)
	n := exp + 1 // the position of the decimal point relative to the digit string

	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	}
	e := n - 1
	esign := "+"
	if e < 0 {
		esign, e = "-", -e
	}
	if k == 1 {
		return sign + digits + "e" + esign + strconv.Itoa(e)
	}
	return sign + digits[:1] + "." + digits[1:] + "e" + esign + strconv.Itoa(e)
}
