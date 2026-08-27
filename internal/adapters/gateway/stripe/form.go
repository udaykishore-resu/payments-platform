package stripe

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// form builds Stripe's request bodies.
//
// Stripe's API is `application/x-www-form-urlencoded`, not JSON, and has been since 2011. That
// is not a detail an adapter can paper over with json.Marshal: Stripe expresses nesting with
// bracket syntax — `metadata[order_id]=x`, `company[address][city]=Berlin`,
// `payment_method_options[card][request_three_d_secure]=any` — and arrays with a repeated
// `expand[]=` key. Encoding a struct to JSON and posting it produces a 400 with an unhelpful
// message, and encoding a map with Go's url.Values loses the ordering guarantees a test needs.
//
// Ordering: Encode sorts by key. Stripe does not care about parameter order, but a deterministic
// body makes an adapter test assert on an exact string rather than on a set of substrings, and it
// makes a captured request diffable when a certification run disagrees with production.
type form struct {
	pairs []formPair
}

type formPair struct {
	key   string
	value string
}

// set writes a scalar parameter, replacing any previous value for the same key. Empty values are
// dropped rather than sent: Stripe treats an empty string as an explicit "unset this field",
// which on an update call is a destructive difference from omitting the parameter.
func (f *form) set(key, value string) {
	if value == "" {
		return
	}
	for i := range f.pairs {
		if f.pairs[i].key == key {
			f.pairs[i].value = value
			return
		}
	}
	f.pairs = append(f.pairs, formPair{key, value})
}

// setRaw writes a parameter even when the value is empty, for the rare field where the empty
// string is the intended instruction.
func (f *form) setRaw(key, value string) {
	for i := range f.pairs {
		if f.pairs[i].key == key {
			f.pairs[i].value = value
			return
		}
	}
	f.pairs = append(f.pairs, formPair{key, value})
}

func (f *form) setInt(key string, v int64) { f.setRaw(key, strconv.FormatInt(v, 10)) }

// setBool writes Stripe's boolean encoding. Stripe accepts "true"/"false"; it does not accept
// "1"/"0" for every field, so the literal spelling is used everywhere for consistency.
func (f *form) setBool(key string, v bool) { f.setRaw(key, strconv.FormatBool(v)) }

// setMap writes a nested map as `prefix[key]=value`.
//
// Keys are sorted so the body is deterministic, and empty values are skipped because a Stripe
// metadata key with an empty value deletes that key on an update — a behaviour that turns
// "the caller passed a blank field" into silent data loss.
func (f *form) setMap(prefix string, m map[string]string) {
	if len(m) == 0 {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if m[k] == "" {
			continue
		}
		f.set(prefix+"["+k+"]", m[k])
	}
}

// appendItem writes one element of a Stripe array parameter, spelled `key[]`. Stripe accepts the
// repeated-key form for every array field this adapter sends — `expand[]` and `enabled_events[]`,
// both arrays of scalars. The indexed form (`key[0][field]=`) is required only for arrays of
// *objects*, and nothing on this integration's surface sends one: persons are separate POSTs and
// the external account is a single nested object, both written with set(). A helper for the
// indexed form is therefore not carried here; add it with its first caller, so the encoding and
// the request that needs it are reviewed together.
func (f *form) appendItem(key, value string) {
	if value == "" {
		return
	}
	f.pairs = append(f.pairs, formPair{key + "[]", value})
}

// encode renders the body.
//
// url.QueryEscape is applied to both key and value. The key needs escaping too: a metadata key
// supplied by a merchant can legitimately contain characters that would otherwise change the
// bracket structure, and an unescaped `metadata[a=b]` would silently create a different field.
func (f *form) encode() string {
	if len(f.pairs) == 0 {
		return ""
	}
	sorted := make([]formPair, len(f.pairs))
	copy(sorted, f.pairs)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].key < sorted[j].key })

	var b strings.Builder
	for i, p := range sorted {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(p.key))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(p.value))
	}
	return b.String()
}

// bytes renders the body for transport.
//
// An empty form renders as an empty body, which is what `do` keys off when it decides whether to
// send a Content-Type header — so no separate emptiness predicate is needed here, and having one
// would give two answers to the same question.
func (f *form) bytes() []byte { return []byte(f.encode()) }
