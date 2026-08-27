package secrets

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
)

// Material is the platform's ports.SecretMaterial: a resolved credential, held so that every
// rendering path in the standard library produces the redaction placeholder instead of the
// plaintext.
//
// # Why this type exists at all rather than a map[string]string
//
// The port could have returned a map. It does not, and the reason is the same inversion
// internal/platform/secret is built on: a bare map is safe only while every developer who
// touches it remembers not to log it, and the moment one of them writes `slog.Any("creds", m)`
// or an error wraps `%+v` of a struct containing it, the gateway API key is in the log pipeline
// and from there in a search index that far more people can read than can read the secret store.
// A type whose *default rendering* is `[REDACTED]` moves that from "one forgotten verb" to "one
// reviewable call site" — the call site being [Material.Value].
//
// # Why the values are individually wrapped
//
// Each field is a secret.Secret[string] rather than the map being wrapped as a whole. Wrapping
// the map would give one Expose() returning a live, mutable map — a caller could retain it, and
// a retained map of plaintext is exactly the heap-resident credential the resolver's
// per-call-no-caching rule exists to prevent. Per-field wrapping means the only way to obtain a
// plaintext is to name the field you need, one at a time, at the moment you need it.
//
// # Why the data hangs off a pointer
//
// This is the one non-obvious choice in the file and it closes a real gap. fmt handles `%p`
// before it consults Formatter, and `%p` on a non-pointer argument takes fmt's `badVerb` path —
// which sets an internal `erroring` flag that *disables* Formatter and Stringer and then prints
// the argument by reflection. fmt's reflection can read unexported fields, so a struct holding
// its values inline renders them in full: `%!p(secrets.Material={map[api_key:{sk_live_…}]})`.
// The same is true of any value type, secret.Secret[T] included.
//
// Holding the values behind an unexported pointer closes it, because fmt's reflection printer
// renders a pointer field at depth as an address rather than following it. The cost is a nil
// case, which is handled: the zero Material has no fields, no version, and redacts.
type Material struct {
	d *materialData
}

// materialData is the payload, reachable only through Material's methods.
type materialData struct {
	// values holds each field individually wrapped, so the only way to plaintext is to name one
	// field at a time through Value.
	values map[string]secret.Secret[string]
	// order is the field name set, sorted, so Fields() is deterministic. A non-deterministic
	// field order would make a credential fingerprint non-deterministic, and the fingerprint is
	// what the rotation workflow compares to prove the right material was staged.
	order   []string
	version string
}

// NewMaterial wraps resolved plaintext into a redacting Material.
//
// It copies both the map and its values on the way in, per code-conventions §11: retaining the
// caller's map would leave the plaintext reachable through a reference the caller still holds
// and might later log. Call it at the boundary where the plaintext is produced — the Secrets
// Manager response decode, the development file load — so the plaintext exists as a bare value
// for the fewest possible statements.
func NewMaterial(version string, values map[string]string) Material {
	d := &materialData{
		values:  make(map[string]secret.Secret[string], len(values)),
		order:   make([]string, 0, len(values)),
		version: version,
	}
	for k, v := range values {
		d.values[k] = secret.New(v)
		d.order = append(d.order, k)
	}
	sort.Strings(d.order)
	return Material{d: d}
}

// Value returns one field's plaintext, and is the only accessor.
//
// It is deliberately greppable, exactly like secret.Expose: `grep -rn '\.Value('` over the
// adapters is a complete inventory of where this process handles plaintext credential material.
// The legitimate call sites are the same short list — into an outbound request's header, into an
// HMAC key, into a constant-time comparison — and the rule of thumb is unchanged: Value and the
// call that consumes it belong on the same line.
//
// The boolean distinguishes "the field is absent" from "the field is present and empty". They
// need different handling: an absent field is a provisioning defect, and an empty one is a
// gateway that legitimately has no secret half.
func (m Material) Value(field string) (string, bool) {
	if m.d == nil {
		return "", false
	}
	s, ok := m.d.values[field]
	if !ok {
		return "", false
	}
	return s.Expose(), true
}

// Fields lists the field names, sorted. Names are not secret — `api_key`, `webhook_hmac` — and
// listing them is what lets the gateway resolver build a credential set without knowing which
// vendor it is talking to.
//
// The returned slice is a copy: returning the backing array would let a caller reorder the field
// list under a concurrent reader.
func (m Material) Fields() []string {
	if m.d == nil {
		return nil
	}
	out := make([]string, len(m.d.order))
	copy(out, m.d.order)
	return out
}

// Version is the store's version identifier for this material. It travels onto spi.Credentials
// so that a gateway rejection can be attributed to a specific credential version — which is what
// makes "the rotation broke it" answerable without guessing.
func (m Material) Version() string {
	if m.d == nil {
		return ""
	}
	return m.d.version
}

// String satisfies fmt.Stringer so a Material reaching any %s or %v path — including one inside
// a library we do not control — renders the placeholder.
func (Material) String() string { return secret.Redacted }

// GoString satisfies fmt.GoStringer, covering %#v for callers that reach it through the
// GoStringer path rather than the Formatter path.
func (Material) GoString() string { return secret.Redacted }

// MarshalJSON keeps a Material out of a response body, an event envelope or a persisted
// document. It returns the placeholder rather than an error for the reason secret.Secret does:
// an error here fails the whole encode, and a handler that cannot serialize its response tends
// to get "fixed" by someone reaching for the plaintext.
func (Material) MarshalJSON() ([]byte, error) { return []byte(`"` + secret.Redacted + `"`), nil }

// MarshalText covers the encoders that prefer encoding.TextMarshaler — YAML, TOML, url.Values,
// and encoding/json's map-key path, which does not consult MarshalJSON for keys.
func (Material) MarshalText() ([]byte, error) { return []byte(secret.Redacted), nil }

// LogValue implements slog.LogValuer, so a Material passed as an attribute value is redacted by
// the logging package itself. This holds even for a handler that has never heard of this type,
// which is the property that makes it worth implementing separately from String.
func (Material) LogValue() slog.Value { return slog.StringValue(secret.Redacted) }

// Format is the load-bearing method, for the same reason it is on secret.Secret: fmt consults
// Formatter before Stringer, GoStringer and the reflection walk, and consults it for *every*
// verb. Implementing only String would leave %#v, %d and %x open — which is the failure mode
// that makes a redaction wrapper actively harmful, because it creates confidence without
// coverage.
//
// %q is quoted so output remains valid where a quoted string was expected; every other verb gets
// the bare placeholder.
func (Material) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = io.WriteString(f, strconv.Quote(secret.Redacted))
		return
	}
	_, _ = io.WriteString(f, secret.Redacted)
}

// Compile-time proof that every redacting interface is satisfied and that Material is the port's
// SecretMaterial. If someone changes a receiver from value to pointer, the build breaks here
// rather than in production.
var (
	_ fmt.Stringer         = Material{}
	_ fmt.GoStringer       = Material{}
	_ fmt.Formatter        = Material{}
	_ json.Marshaler       = Material{}
	_ slog.LogValuer       = Material{}
	_ ports.SecretMaterial = Material{}
)

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: NFR-32.
//
// The resolved-credential carrier that redacts through every formatting, JSON and logging path
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
