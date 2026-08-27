package secrets

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// The AWS Signature Version 4 constants. They are protocol literals, not tunables: changing any
// of them produces a signature AWS will reject with a message that does not say which one.
const (
	sigV4Algorithm  = "AWS4-HMAC-SHA256"
	sigV4Terminator = "aws4_request"
	sigV4TimeFormat = "20060102T150405Z"
	sigV4DateFormat = "20060102"

	// headerAmzDate, headerAmzToken and headerAmzContentSHA256 are the three headers the signer
	// sets. The security token header is what makes IRSA work: STS returns a session token
	// alongside the key pair, and a request signed with the pair but missing the token is
	// rejected as an invalid key.
	headerAmzDate          = "X-Amz-Date"
	headerAmzToken         = "X-Amz-Security-Token" // a header name, not a credential
	headerAmzContentSHA256 = "X-Amz-Content-Sha256"

	// emptyPayloadSHA256 is sha256(""), precomputed because every GET this package makes has an
	// empty body and hashing nothing on each call is measurable on the payment path.
	emptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// Credentials is one set of AWS credentials, resolved from the chain in awssm.go.
//
// SecretAccessKey is a bare string rather than a secret.Secret[string], and that is a deliberate
// exception to the platform's rule, made here and nowhere else. The HMAC chain below needs the
// bytes on four consecutive lines; wrapping them would mean four Expose() calls in a row, which
// makes the greppable-inventory property of Expose useless precisely where it should be loudest.
// What protects this value instead is that Credentials never leaves this package, never reaches
// an error, and is redacted by [Credentials.LogValue] if it ever reaches a log.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// Expires is when these credentials stop working. Zero means "static credentials from the
	// environment", which do not expire and are refreshed by nothing.
	Expires time.Time
}

// signer computes SigV4 authorization for one region and service.
//
// It holds no credentials: they are passed per call, because the credential provider refreshes
// them independently and a signer holding a stale copy would produce signatures rejected as
// expired tokens — the single most confusing AWS failure to debug, since the error names the
// signature rather than the credential.
type signer struct {
	region  string
	service string
}

// sign adds the Authorization, X-Amz-Date and (when present) X-Amz-Security-Token headers to req.
//
// payloadHash is the hex SHA-256 of the request body, computed by the caller because the caller
// already holds the body bytes and hashing them twice on the payment path is waste.
//
// It returns the canonical request and string-to-sign alongside the header. They are returned
// rather than discarded because the AWS test suite asserts on exactly those two intermediate
// values, and a signer whose intermediates cannot be observed can only be tested against a live
// endpoint — which is a test that does not run in CI.
func (s signer) sign(req *http.Request, payloadHash string, creds Credentials, now time.Time) (authorization, canonicalRequest, stringToSign string) {
	now = now.UTC()
	amzDate := now.Format(sigV4TimeFormat)
	dateStamp := now.Format(sigV4DateFormat)

	req.Header.Set(headerAmzDate, amzDate)
	if creds.SessionToken != "" {
		req.Header.Set(headerAmzToken, creds.SessionToken)
	}
	// The Host header is not in Go's Header map — net/http carries it on the Request — but it is
	// mandatory in the signed set, so it is spliced into the canonical headers below rather than
	// written into the map, which net/http would then send twice.

	signedHeaders, canonicalHeaders := canonicalHeaderSet(req)
	canonicalRequest = strings.Join([]string{
		req.Method,
		canonicalURIPath(req.URL),
		canonicalQuery(req.URL),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, s.region, s.service, sigV4Terminator}, "/")
	stringToSign = strings.Join([]string{
		sigV4Algorithm,
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(s.signingKey(creds.SecretAccessKey, dateStamp), []byte(stringToSign)))
	authorization = sigV4Algorithm +
		" Credential=" + creds.AccessKeyID + "/" + scope +
		", SignedHeaders=" + signedHeaders +
		", Signature=" + signature
	req.Header.Set("Authorization", authorization)
	return authorization, canonicalRequest, stringToSign
}

// signingKey derives the date/region/service-scoped key.
//
// The four-step derivation is the reason a leaked signature is not a leaked credential: the key
// that signed a request is valid for one day, one region and one service, so replaying it
// elsewhere fails. It is recomputed per request rather than cached; the four HMACs cost about
// four microseconds, and a cache keyed by date would need eviction logic to avoid holding a
// derived key past the day it is valid for.
func (s signer) signingKey(secretAccessKey, dateStamp string) []byte {
	k := hmacSHA256([]byte("AWS4"+secretAccessKey), []byte(dateStamp))
	k = hmacSHA256(k, []byte(s.region))
	k = hmacSHA256(k, []byte(s.service))
	return hmacSHA256(k, []byte(sigV4Terminator))
}

// canonicalHeaderSet builds the signed-header list and the canonical header block.
//
// The rules implemented here are the ones the AWS test suite exists to pin, and each of them is
// a place an implementation silently diverges:
//
//   - Header names are lowercased and sorted by the *lowercased* name.
//   - Values have leading and trailing whitespace stripped and internal runs of whitespace
//     collapsed to a single space. `get-header-value-trim` is the case that pins this, and it
//     pins something worth knowing: the collapsing applies *inside* quoted strings too. AWS's
//     prose has described quoted runs as preserved, but the published suite's expected canonical
//     request for `My-Header2: "a   b   c"` is `my-header2:"a b c"`, and the suite is what the
//     service actually verifies against. Implementing the prose instead of the vector produces a
//     403 on any request carrying a quoted header value — rare enough to ship and hard to find.
//   - Multiple values for one header are joined with a comma, in the order sent.
//   - Host is always signed, and comes from req.Host or the URL, never from the header map.
func canonicalHeaderSet(req *http.Request) (signedHeaders, canonicalHeaders string) {
	values := make(map[string][]string, len(req.Header)+1)
	for name, vs := range req.Header {
		lower := strings.ToLower(name)
		if lower == "authorization" {
			// Signing over a previous Authorization header would make a re-signed request
			// (a retry after a credential refresh) unverifiable.
			continue
		}
		values[lower] = append(values[lower], vs...)
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	values["host"] = []string{host}

	names := make([]string, 0, len(values))
	for n := range values {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, n := range names {
		trimmed := make([]string, 0, len(values[n]))
		for _, v := range values[n] {
			trimmed = append(trimmed, trimHeaderValue(v))
		}
		b.WriteString(n)
		b.WriteByte(':')
		b.WriteString(strings.Join(trimmed, ","))
		b.WriteByte('\n')
	}
	return strings.Join(names, ";"), b.String()
}

// trimHeaderValue strips the padding and collapses internal whitespace runs to one space.
//
// It is written as an explicit loop rather than strings.Join(strings.Fields(v), " ") for one
// reason: Fields splits on every Unicode space, including the non-breaking space, and a value
// containing one would be canonicalised differently from what the service does. Restricting the
// collapse to ASCII space and tab is the behaviour the suite pins.
func trimHeaderValue(v string) string {
	v = strings.Trim(v, " \t")
	var b strings.Builder
	b.Grow(len(v))
	lastWasSpace := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == ' ' || c == '\t' {
			if lastWasSpace {
				continue
			}
			lastWasSpace = true
			b.WriteByte(' ')
			continue
		}
		lastWasSpace = false
		b.WriteByte(c)
	}
	return b.String()
}

// canonicalURIPath renders the path segment of the canonical request.
//
// The path is URI-encoded with the AWS rules — unreserved characters pass through, everything
// else becomes %XX with uppercase hex — and `/` is preserved as the separator. An empty path is
// "/".
//
// Note the deliberate omission: S3 signs the path *once* and every other service signs it
// *twice*. This signer talks to Secrets Manager and STS, whose request paths are always "/", so
// the distinction never arises; encoding once is correct for them and is what the test suite's
// `service` vectors assert. A caller adding an S3 or path-addressed service here must revisit
// this function rather than assume it generalises.
func canonicalURIPath(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		return "/"
	}
	// EscapedPath preserves the original encoding where it was already valid, which is not the
	// AWS rule set — Go leaves sub-delimiters like `!$&'()*+,;=` and `:@` unescaped in a path.
	// Re-encoding from the decoded form is the only way to get a canonical answer.
	segments := strings.Split(u.Path, "/")
	for i, seg := range segments {
		segments[i] = uriEncode(seg, false)
	}
	return strings.Join(segments, "/")
}

// canonicalQuery renders the query string: parameters sorted by encoded name then encoded value,
// each `name=value`, with an empty value rendered as `name=`.
//
// Sorting by the *encoded* name is what `get-vanilla-query-order-key-case` pins: `Param1` sorts
// before `Param2` by byte order, and a sort by decoded or case-folded name would order them
// differently and produce a signature AWS rejects.
func canonicalQuery(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	type kv struct{ k, v string }
	pairs := make([]kv, 0, 8)
	for _, part := range strings.Split(u.RawQuery, "&") {
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		dn, err := url.QueryUnescape(name)
		if err != nil {
			dn = name
		}
		dv, err := url.QueryUnescape(value)
		if err != nil {
			dv = value
		}
		pairs = append(pairs, kv{uriEncode(dn, true), uriEncode(dv, true)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, p.k+"="+p.v)
	}
	return strings.Join(parts, "&")
}

// uriEncode implements the AWS percent-encoding rules.
//
// It is written out rather than delegated to url.QueryEscape because the two differ in three
// places that each produce a wrong signature: QueryEscape encodes a space as `+` (AWS wants
// `%20`), leaves `+` unencoded in some paths, and does not encode `~`... in the other direction,
// url.PathEscape leaves sub-delimiters alone. Getting this wrong fails only for requests
// containing those characters, which is why it is easy to ship and hard to find.
//
// encodeSlash is false for path segments (the separator survives) and true for query components.
func uriEncode(s string, encodeSlash bool) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'),
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0x0f])
		}
	}
	return b.String()
}

// hmacSHA256 is the one-line primitive the derivation chain is built from.
func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// hexSHA256 hashes and hex-encodes, which is the shape every SigV4 field wants.
func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: NFR-32.
//
// AWS Signature Version 4 request signing over the standard library, with session-token support
// for IRSA, verified against the published AWS SigV4 test-suite vectors
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
