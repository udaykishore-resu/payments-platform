// Package l1api is validation level 1: the edge. Transport, authentication, tenancy,
// authorization, rate limiting, then schema, bounds and the PCI detectors.
//
// L1 exists to reject garbage before it costs a database round trip, and to reject anything
// carrying cardholder data before it reaches a system assessed at SAQ-A. Those are two
// different jobs with two different failure semantics, which is why one set has an ordered
// phase boundary: phase A (transport → auth → tenancy → authorization → rate limit) is
// ShortCircuit, phase B (schema → types → bounds → PAN detector → idempotency header) is
// CollectAll. A failure in phase A never reveals a phase B outcome, and that is the property
// that stops an unauthenticated request being used to probe which fields the schema accepts.
//
// Every subject field is a snapshot taken by the HTTP decoder. The four impure rules — mTLS
// chain validity and the three limiter checks — read a value that was fetched before
// evaluation started rather than fetching it themselves, so the whole level stays evaluable
// from a stored subject, in a unit test, with no network. See docs/validation-plane.md §3.1.
package l1api

import (
	"encoding/json"
	"strings"
	"time"
)

// TLS version numbers, mirrored from the wire encoding rather than imported from crypto/tls:
// this package must stay evaluable from a stored snapshot, and a snapshot carries a number.
const (
	TLS10 uint16 = 0x0301
	TLS11 uint16 = 0x0302
	TLS12 uint16 = 0x0303
	TLS13 uint16 = 0x0304
)

// AuthScheme is how the caller authenticated.
type AuthScheme string

const (
	// AuthNone means no credential was presented.
	AuthNone AuthScheme = ""
	// AuthBearer means an OAuth2/OIDC bearer token was presented.
	AuthBearer AuthScheme = "bearer"
	// AuthMTLS means a client certificate was presented.
	AuthMTLS AuthScheme = "mtls"
)

// JSONType is the JSON type a schema field declares. Types are checked without coercion:
// `"100"` is not `100`, because a platform that coerces once will coerce a currency exponent
// somewhere too.
type JSONType string

// The JSON type universe.
const (
	TypeString  JSONType = "string"
	TypeNumber  JSONType = "number"
	TypeInteger JSONType = "integer"
	TypeBoolean JSONType = "boolean"
	TypeObject  JSONType = "object"
	TypeArray   JSONType = "array"
)

// FieldKind marks fields that carry a platform-wide semantic — an amount, a currency code, a
// country code, a timestamp — so that the rules asserting those semantics can find their
// fields without hardcoding a name per route.
type FieldKind string

// The semantic field kinds.
const (
	KindPlain     FieldKind = ""
	KindAmount    FieldKind = "amount"
	KindCurrency  FieldKind = "currency"
	KindCountry   FieldKind = "country"
	KindTimestamp FieldKind = "timestamp"
)

// FieldSpec is one field's contract in a route's JSON Schema.
type FieldSpec struct {
	Type      JSONType
	Kind      FieldKind
	MinLength int
	MaxLength int
	Enum      []string
	// Multiline permits `\n` in an otherwise control-character-free string. Descriptions and
	// addresses need it; a statement descriptor must not have it.
	Multiline bool
}

// Schema is the route's request contract, flattened to dotted paths.
//
// Flattened rather than nested because every L1 rule that consults it does so while walking
// the decoded body, and a walk produces a path. Array elements collapse to `field[]`, so one
// spec covers every element of a list.
type Schema struct {
	// Required lists the dotted paths that must be present and non-null.
	Required []string
	// Fields maps a dotted path to its spec. A path absent from this map is an unknown field.
	Fields map[string]FieldSpec
	// AllowUnknown disables strict decoding for routes that accept a free-form object. It
	// exists for webhook ingress, where the sender owns the schema.
	AllowUnknown bool
}

// Route is what the caller asked for, and what the platform requires of that endpoint.
type Route struct {
	Method                 string
	Path                   string
	HasBody                bool
	Authenticated          bool
	RequiresIdempotencyKey bool
	RequiredScope          string
	RequiredAction         string
	IsList                 bool
	// MutatesResource marks a PATCH/PUT on a resource with an ETag, where a blind write would
	// silently clobber a concurrent edit.
	MutatesResource bool
	// IsWebhook raises the body size ceiling: gateways send large settlement payloads and we do
	// not control their shape.
	IsWebhook bool
	Schema    Schema
}

// TokenClaims is the decoded bearer token.
//
// SignatureVerified is a field rather than a computed property because verification needs the
// JWKS, and reaching for a key set from inside a rule would make the rule impure and the
// rejection irreproducible. The edge verifies against its cached JWKS and records the answer;
// the rule asserts on it.
type TokenClaims struct {
	Raw               string
	Present           bool
	Algorithm         string
	KeyID             string
	Issuer            string
	Audience          []string
	Subject           string
	TenantID          string
	Scopes            []string
	IssuedAt          time.Time
	NotBefore         time.Time
	ExpiresAt         time.Time
	SignatureVerified bool
}

// ClientCert is the mTLS peer certificate as the terminating proxy saw it.
type ClientCert struct {
	Presented       bool
	ChainVerified   bool
	Revoked         bool
	SANMapsToClient bool
	Subject         string
}

// Principal is the authenticated caller and what the RBAC/ABAC evaluation said it may do.
type Principal struct {
	ID               string
	Roles            []string
	PermittedActions []string
}

// RateLimits carries the limiter decisions taken before evaluation.
//
// The limiters are Redis token buckets, which is why the three rules that read this are the
// impure ones at L1. Fetching them once, before the rule set runs, is what keeps them off the
// critical path of every other rule and keeps the whole set reproducible.
type RateLimits struct {
	TenantTokenAvailable   bool
	MerchantTokenAvailable bool
	ConcurrencyInFlight    int
	ConcurrencyLimit       int
	RetryAfterSeconds      int
}

// CursorState is the result of decoding and authenticating a pagination cursor.
type CursorState struct {
	Present       bool
	Decodable     bool
	HMACValid     bool
	TenantMatches bool
}

// Subject is everything L1 evaluates.
type Subject struct {
	Route      Route
	TLSVersion uint16
	// Headers is canonicalized to lower-case keys by the decoder; lookups here are
	// case-insensitive anyway, because a client that sends `idempotency-key` is not wrong.
	Headers map[string]string
	RawBody []byte
	// Body is the decoded body. Decoding with json.Number preserves the distinction between
	// `1050` and `1050.0`, which is the difference between a valid minor-unit amount and a
	// caller who has been doing currency arithmetic in a float.
	Body      map[string]any
	Query     map[string]string
	Auth      AuthScheme
	Token     TokenClaims
	Cert      ClientCert
	Principal Principal
	// TenantActive is the tenant-registry lookup performed at the edge.
	TenantActive bool
	Limits       RateLimits
	Cursor       CursorState
	// Now is the injected clock reading. No rule calls time.Now.
	Now time.Time
}

// Deps carries the level's configuration: limits and trust anchors, nothing that performs I/O.
type Deps struct {
	// TrustedIssuers is the environment's issuer allowlist.
	TrustedIssuers []string
	// Audience is this API's expected `aud` value.
	Audience string
	// AllowedAlgorithms is the JWT `alg` allowlist. It is an allowlist because `alg` is
	// attacker-controlled: accepting whatever the header says is how `none` and the
	// RS256→HS256 confusion attack get in.
	AllowedAlgorithms []string
	// MaxBodyBytes and MaxWebhookBodyBytes bound the request body (256 KiB / 1 MiB).
	MaxBodyBytes        int
	MaxWebhookBodyBytes int
	// MaxNestingDepth bounds JSON nesting (12). It is what makes the PAN detector's own depth
	// cap unreachable, so a deeply nested document is rejected rather than partially scanned.
	MaxNestingDepth int
	// ClockSkew is the tolerance applied to `exp` and `nbf` (60 s).
	ClockSkew time.Duration
	// FutureTimestampTolerance rejects timestamps further ahead than this (24 h).
	FutureTimestampTolerance time.Duration
	// Metadata quota.
	MaxMetadataKeys     int
	MaxMetadataKeyLen   int
	MaxMetadataValueLen int
	MaxMetadataBytes    int
	// MaxPageSize bounds `limit` on list routes (100).
	MaxPageSize int
}

// DefaultDeps returns the platform defaults from docs/validation-plane.md §3.1, so that a
// caller wiring the level states only what it overrides.
func DefaultDeps() Deps {
	return Deps{
		AllowedAlgorithms:        []string{"RS256", "ES256"},
		MaxBodyBytes:             256 * 1024,
		MaxWebhookBodyBytes:      1024 * 1024,
		MaxNestingDepth:          12,
		ClockSkew:                60 * time.Second,
		FutureTimestampTolerance: 24 * time.Hour,
		MaxMetadataKeys:          40,
		MaxMetadataKeyLen:        40,
		MaxMetadataValueLen:      500,
		MaxMetadataBytes:         8 * 1024,
		MaxPageSize:              100,
	}
}

// header returns the header value, case-insensitively.
func (s Subject) header(name string) string {
	if v, ok := s.Headers[name]; ok {
		return v
	}
	lower := strings.ToLower(name)
	for k, v := range s.Headers {
		if strings.ToLower(k) == lower {
			return v
		}
	}
	return ""
}

func (s Subject) hasHeader(name string) bool { return s.header(name) != "" }

// bodyValue returns the decoded value at a top-level key.
func (s Subject) bodyValue(key string) (any, bool) {
	if s.Body == nil {
		return nil, false
	}
	v, ok := s.Body[key]
	return v, ok
}

// walkValues visits every value in the decoded body, depth-first, reporting a dotted path.
// Array elements are indexed (`items[2].reference`) because a merchant fixing an integration
// needs to know which element, and collapsing the index would leave them grepping.
func walkValues(v any, path string, depth int, maxDepth int, fn func(path string, v any)) {
	if depth > maxDepth {
		return
	}
	fn(path, v)
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			walkValues(child, join(path, k), depth+1, maxDepth, fn)
		}
	case []any:
		for i, child := range t {
			walkValues(child, path+"["+itoa(i)+"]", depth+1, maxDepth, fn)
		}
	}
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// normalizePath collapses array indices so `items[2].reference` looks up the schema entry
// `items[].reference`: one spec covers every element of a list.
func normalizePath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '[' {
			b.WriteString("[]")
			for i < len(p) && p[i] != ']' {
				i++
			}
			continue
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

// depthOf returns the maximum nesting depth of a decoded document. A scalar is depth 1.
func depthOf(v any) int {
	switch t := v.(type) {
	case map[string]any:
		best := 0
		for _, child := range t {
			if d := depthOf(child); d > best {
				best = d
			}
		}
		return best + 1
	case []any:
		best := 0
		for _, child := range t {
			if d := depthOf(child); d > best {
				best = d
			}
		}
		return best + 1
	default:
		return 1
	}
}

// lookupPath resolves a dotted path (no indices) against the decoded body.
func lookupPath(body map[string]any, path string) (any, bool) {
	var cur any = body
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

// jsonTypeOf reports the JSON type of a decoded value, distinguishing integer from number so
// that a schema can require an integer and mean it.
func jsonTypeOf(v any) JSONType {
	switch t := v.(type) {
	case string:
		return TypeString
	case bool:
		return TypeBoolean
	case map[string]any:
		return TypeObject
	case []any:
		return TypeArray
	case json.Number:
		if strings.ContainsAny(string(t), ".eE") {
			return TypeNumber
		}
		return TypeInteger
	case float64:
		if t == float64(int64(t)) {
			return TypeInteger
		}
		return TypeNumber
	case int, int64:
		return TypeInteger
	}
	return ""
}

// typeSatisfies reports whether an actual JSON type satisfies a declared one. An integer
// satisfies `number`; a number does not satisfy `integer`.
func typeSatisfies(want, got JSONType) bool {
	if want == got {
		return true
	}
	return want == TypeNumber && got == TypeInteger
}
