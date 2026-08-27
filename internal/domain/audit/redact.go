package audit

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Snapshot builds an audit before/after snapshot containing only the named fields.
//
// # Allowlist, not denylist
//
// The obvious design is a denylist: serialize everything, then strip the fields known to be
// sensitive. It is the wrong one, and the reason is a property of how each fails.
//
// A denylist fails open. The default for a new field is "included", so every field anyone adds
// anywhere — a `webhookSecret` on a gateway connection, a `bankAccountNumber` on a payout
// profile, an `apiKey` on a tenant — is captured into the audit record from the moment it is
// declared, and stays captured until somebody notices and adds it to the list. The failure is
// silent, it is discovered late, and it is discovered in the worst possible place: the audit
// record is one of the most widely-read artifacts in the platform (auditors, tenant admins,
// support, the WORM export, the tenant's own audit feed), it is retained for seven years in
// storage that cannot be deleted early by anyone including root (docs/compliance.md §6), and it
// is hash-chained — so the leaked secret cannot be redacted afterwards without destroying the
// chain that makes the whole table evidential. A denylist miss is therefore not a bug you fix;
// it is a secret you rotate and an incident you write up.
//
// An allowlist fails closed. The default for a new field is "absent", so the failure mode is a
// missing field in an audit record — visible the first time someone reads it, fixable in a
// deploy, and harmless in the meantime. The cost is that every field worth auditing has to be
// named, which is real work and is the correct amount of friction for deciding that something
// belongs in a seven-year immutable record.
//
// The same reasoning produces the allowlist log serializer and `Secret[T]` (docs/compliance.md
// §1.3): three independent layers, all defaulting to absence.
//
// # What it accepts
//
// A struct, a pointer to one, or a map[string]any. Field names match on the Go field name and
// on the field's `json` tag name, so an allowlist can be written against whichever spelling the
// caller thinks in. Matching is case-insensitive to stop a capitalization slip from silently
// dropping an audited field — the failure mode of over-matching here is a field that was
// already explicitly allowed, which is not a leak.
//
// Values are normalized to the kinds the canonical encoder can render deterministically
// (strings, integers, floats, bools, times, and nested maps and slices of those). Anything
// exotic becomes its string form rather than being dropped, because a field the caller
// deliberately allowed should appear in some legible form rather than vanish.
//
// A nil input, or an input of an unsupported kind, returns nil rather than an error: a
// before-snapshot of a resource that did not exist yet is legitimately empty, and forcing every
// creation path to handle an error for that would be noise.
func Snapshot(v any, allowed []string) map[string]any {
	if v == nil || len(allowed) == 0 {
		return nil
	}
	allow := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		allow[strings.ToLower(strings.TrimSpace(a))] = struct{}{}
	}

	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}

	out := make(map[string]any, len(allow))
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return nil
		}
		for _, k := range rv.MapKeys() {
			name := k.String()
			if _, ok := allow[strings.ToLower(name)]; !ok {
				continue
			}
			out[name] = normalize(rv.MapIndex(k))
		}
	case reflect.Struct:
		t := rv.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				// An unexported field cannot be read without unsafe, and reaching for unsafe to
				// put more data into an audit record is precisely the wrong instinct.
				continue
			}
			name, ok := matchName(f, allow)
			if !ok {
				continue
			}
			out[name] = normalize(rv.Field(i))
		}
	default:
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// matchName reports whether a struct field is allowed, and under which name it should appear.
// The json tag name wins when present, so a snapshot reads the way the API does.
func matchName(f reflect.StructField, allow map[string]struct{}) (string, bool) {
	name := f.Name
	if tag := f.Tag.Get("json"); tag != "" && tag != "-" {
		if comma := strings.IndexByte(tag, ','); comma >= 0 {
			tag = tag[:comma]
		}
		if tag != "" {
			name = tag
		}
	}
	if _, ok := allow[strings.ToLower(name)]; ok {
		return name, true
	}
	if _, ok := allow[strings.ToLower(f.Name)]; ok {
		return name, true
	}
	return "", false
}

// normalize converts a reflected value into one of the kinds canonicalJSON renders
// deterministically. Nested structs are flattened to their string form rather than descended
// into: descending would re-introduce the denylist problem one level down, where the allowlist
// no longer applies and a nested credential field would be captured wholesale.
func normalize(rv reflect.Value) any {
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return nil
	}
	if rv.Type() == reflect.TypeOf(time.Time{}) {
		t, _ := rv.Interface().(time.Time)
		return t.UTC().Format(time.RFC3339Nano)
	}
	switch rv.Kind() {
	case reflect.String:
		return rv.String()
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint()
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	case reflect.Slice, reflect.Array:
		out := make([]any, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out = append(out, normalize(rv.Index(i)))
		}
		return out
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return stringify(rv.Interface())
		}
		out := make(map[string]any, rv.Len())
		keys := make([]string, 0, rv.Len())
		for _, k := range rv.MapKeys() {
			keys = append(keys, k.String())
		}
		sort.Strings(keys)
		for _, k := range keys {
			out[k] = normalize(rv.MapIndex(reflect.ValueOf(k).Convert(rv.Type().Key())))
		}
		return out
	default:
		return stringify(rv.Interface())
	}
}

// stringify renders a value for which there is no deterministic structured form. fmt.Stringer
// is preferred where the type offers it, since a type that knows how to describe itself
// describes itself better than %v does — and, importantly, a redacting wrapper type's String
// method is what makes it redact.
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	case error:
		return t.Error()
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprintf("%v", v)
	}
}
