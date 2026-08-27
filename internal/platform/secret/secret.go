// Package secret carries credential material through the process in a wrapper that cannot be
// printed, marshalled or logged.
//
// The control this implements is baseline §17.2 ("Secrets cannot be logged") and security.md
// §6.1, and it is a PCI-scope control rather than a hygiene preference. The threat it answers
// is T-7 in the threat model: a gateway API key, a webhook signing secret or an unwrapped DEK
// reaching a log line, an error string or an event payload, from where it is copied into a
// search index that a much larger set of people can read than can read the secret store.
//
// The design principle is that redaction must be the *default rendering*, not a discipline.
// A bare `string` credential is safe only while every developer who touches it remembers; a
// `Secret[string]` is safe unless someone deliberately calls Expose. That inversion is the
// whole value: it moves the leak from "one forgotten %+v" to "one reviewable call site".
//
// Depends only on the standard library, so the domain, the adapters and the config loader can
// all speak this type without importing anything else.
package secret

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
)

// Redacted is the single placeholder every rendering path emits. It is a constant rather than a
// per-call string so that a log-pipeline detector, a test and a human reading a dashboard all
// match on exactly one token. Changing it changes a published operational contract.
const Redacted = "[REDACTED]"

// Secret wraps a value whose plaintext must never reach a log, an error, a response body or an
// event payload.
//
// The wrapped value is unexported, which is what makes the guarantee hold: reflection-based
// serializers (encoding/json, and fmt's %#v path for nested values) cannot read an unexported
// field, and every path that *can* render the value is overridden below. The type is a value
// type, not a pointer, so copying it is cheap and there is no nil case for a caller to get
// wrong; the trade-off is that the plaintext lives in as many places as the value is copied,
// which is acceptable because the alternative — a pointer with a Close/zeroize lifecycle — buys
// nothing against an attacker who can already read process memory, and costs a nil check at
// every use.
//
// Known limitation, stated because it is the one gap and pretending otherwise is worse than
// documenting it: if a Secret is stored in an *unexported* field of some other struct and that
// struct is printed with %#v, fmt cannot call this type's Format method (it cannot take an
// interface of an unexported field) and prints the struct's field layout instead. That layout
// shows `{v:...}` for the Secret, not the plaintext, because the plaintext is itself in an
// unexported field one level down — but the safe pattern remains: declare Secret-typed fields
// exported, or give the containing type its own LogValue.
type Secret[T any] struct {
	v T
}

// New wraps a plaintext value. Call it at the boundary where the plaintext is produced — the
// secrets-manager client, the config decoder, the KMS unwrap — so that the plaintext exists as
// a bare value for the fewest possible statements.
func New[T any](v T) Secret[T] { return Secret[T]{v: v} }

// Expose returns the plaintext. It is the only accessor, and it is deliberately named to be
// greppable: `grep -rn '\.Expose()'` is a complete inventory of the places this process handles
// plaintext credential material, and CI counts them so that the number moving is a reviewable
// event rather than a silent drift.
//
// Legitimate call sites, exhaustively:
//
//   - At the moment of use, passed directly into the thing that needs it: an HTTP header value
//     on an outbound gateway request, an Authorization builder, an HMAC key, a database driver's
//     connection parameter, a KMS/crypto primitive.
//   - Inside a comparison that is itself constant-time.
//
// Never: into a log call, into fmt.Errorf or any error message, into a struct that is later
// JSON-encoded or written to Kafka, into a span or metric attribute, into a context value that
// crosses a process boundary, or into a local variable that outlives the call that needs it.
// The rule of thumb is that Expose and the call that consumes it belong on the same line.
func (s Secret[T]) Expose() T { return s.v }

// String satisfies fmt.Stringer so that a Secret reaching any %s/%v path — including one inside
// a third-party library we do not control — renders the placeholder.
func (Secret[T]) String() string { return Redacted }

// GoString satisfies fmt.GoStringer, which covers %#v for callers that reach it through the
// GoStringer path rather than the Formatter path.
func (Secret[T]) GoString() string { return Redacted }

// MarshalJSON keeps a Secret from leaking through a response body, an event envelope or a
// persisted document. Returning the placeholder rather than an error is deliberate: an error
// here would fail the whole encode, and a handler that cannot serialize its response tends to
// get "fixed" by someone reaching for the plaintext.
func (Secret[T]) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }

// MarshalText covers encoders that prefer encoding.TextMarshaler — YAML, TOML, url.Values and
// the map-key path of encoding/json, which does not consult MarshalJSON for keys.
func (Secret[T]) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// LogValue implements slog.LogValuer, so a Secret passed as a slog attribute value is redacted
// by the logging package itself rather than by the handler. This matters because it holds even
// for a handler that has no knowledge of this type.
func (Secret[T]) LogValue() slog.Value { return slog.StringValue(Redacted) }

// Format is the load-bearing method. fmt consults Formatter *before* Stringer, GoStringer and
// the reflection walk, and it consults it for every verb — so %v, %s, %d, %x, %+v and %#v all
// land here, and a developer deliberately trying to print the plaintext with an exotic verb
// still gets the placeholder. Implementing only String() would leave %#v and %d open, which is
// the failure mode that makes a redaction wrapper actively harmful: it creates confidence
// without coverage.
//
// %q is quoted so that the output is still valid Go/JSON-ish syntax where the caller expected a
// quoted string; every other verb gets the bare placeholder.
func (Secret[T]) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = io.WriteString(f, strconv.Quote(Redacted))
		return
	}
	_, _ = io.WriteString(f, Redacted)
}

// UnmarshalJSON lets a Secret be decoded straight out of a config document or a secrets-manager
// payload, so the plaintext never exists as a bare field on an intermediate struct that someone
// might later log. The asymmetry with MarshalJSON is intentional and is the point: values go in,
// they do not come out.
func (s *Secret[T]) UnmarshalJSON(b []byte) error {
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		// The decoder's error text can quote the offending input, which here is the secret.
		// Replace it rather than wrapping it.
		return fmt.Errorf("secret: value could not be decoded into %T", v)
	}
	s.v = v
	return nil
}

// Compile-time proof that every redacting interface is actually satisfied. If someone changes a
// receiver from value to pointer, the build breaks here rather than in production.
var (
	_ fmt.Stringer   = Secret[string]{}
	_ fmt.GoStringer = Secret[string]{}
	_ fmt.Formatter  = Secret[string]{}
	_ json.Marshaler = Secret[string]{}
	_ slog.LogValuer = Secret[string]{}
)

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: NFR-32.
//
// The secret wrapper that redacts through every formatting, JSON and logging path
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
