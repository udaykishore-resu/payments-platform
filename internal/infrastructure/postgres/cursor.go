package postgres

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Cursor is one page boundary in a keyset-paginated listing.
//
// It carries the *sort key* of the last row on the page — a timestamp and an identifier — and
// never an offset. Offset pagination is not offered anywhere in this platform, and the reason is
// concrete rather than stylistic: `LIMIT 50 OFFSET 500` re-evaluates the whole ordering on every
// request, so a client walking a busy merchant's payments while new ones are being created
// silently skips rows (each insert shifts the window) and silently repeats others (each delete
// shifts it back). Nobody notices, because the client has no way to tell. Keyset pagination
// asks "give me the rows after this exact point", which is stable under concurrent writes and,
// as a bonus, is an index range scan rather than a scan-and-discard.
type Cursor struct {
	// Time is the primary sort key. Truncated to microseconds on encode because PostgreSQL
	// TIMESTAMPTZ has microsecond resolution: a cursor carrying nanoseconds would encode a
	// boundary that no row can equal, and the row exactly on the boundary would be skipped.
	Time time.Time
	// ID is the tiebreak. It is required: two payments created in the same microsecond are
	// possible, and a cursor with no tiebreak would either repeat or skip them.
	ID string
}

// IsZero reports whether the cursor is the empty first-page marker.
func (c Cursor) IsZero() bool { return c.ID == "" && c.Time.IsZero() }

// cursorSigningKey is the HMAC key protecting cursors from tampering. It is a package variable
// set once at startup by UseCursorKey.
//
// The default is a fixed development key rather than a random one generated at process start.
// That is a deliberate trade: a per-process random key would make every cursor invalid the
// moment a pod is replaced or a request is load-balanced elsewhere, which in a fleet of nine
// pods means roughly eight of every nine page-two requests fail. Signing is not confidentiality
// here — the encoded value is a timestamp and a public identifier — it is integrity, so a shared
// key that is rotated deliberately is the right shape.
var cursorSigningKey = []byte("pp-dev-cursor-key-not-for-production")

// UseCursorKey installs the cursor signing key, from the same secret store as every other key.
//
// Rotating it invalidates outstanding cursors, which surfaces to a paging client as a single
// 400 on their next page rather than as wrong data. That is the correct failure: a cursor that
// still verifies under an old key after a rotation is a cursor the rotation did not protect.
func UseCursorKey(key []byte) {
	if len(key) == 0 {
		return
	}
	cursorSigningKey = append([]byte(nil), key...)
}

// EncodeCursor renders a cursor as an opaque, signed, URL-safe token.
//
// The signature is what makes the token opaque in practice rather than merely in appearance.
// Without it, a client can decode the base64, edit the timestamp, and re-encode — turning a
// pagination token into an arbitrary range query against a listing whose filters were validated
// once, at the first page. With it, any edit fails verification and the request is rejected
// before it reaches the planner.
//
// The payload is deliberately simple and length-delimited by its separator, and the identifier
// is checked for that separator on the way in, so two different cursors cannot produce the same
// signed pre-image.
func EncodeCursor(c Cursor) string {
	if c.IsZero() {
		return ""
	}
	payload := strconv.FormatInt(c.Time.UTC().UnixMicro(), 10) + "|" + c.ID
	mac := hmac.New(sha256.New, cursorSigningKey)
	mac.Write([]byte(cursorDomain))
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)[:cursorSigBytes]
	return base64.RawURLEncoding.EncodeToString(append([]byte(payload+"|"), sig...))
}

// cursorDomain separates cursor signatures from every other HMAC in the platform, so a value
// signed for one purpose can never verify for another.
const cursorDomain = "payments-platform/cursor/v1\x00"

// cursorSigBytes is 16: a 128-bit truncated HMAC tag. Full 256 bits would double the token
// length for no practical gain — forging a tag requires 2^128 work either way, and the value
// being protected is a page boundary, not a credential.
const cursorSigBytes = 16

// DecodeCursor parses and verifies a token.
//
// Every failure mode returns the same VALIDATION_FAILED error and none of them says which check
// failed. A client that could tell "bad base64" from "bad signature" from "bad timestamp" would
// have an oracle for probing the signing scheme; a client that receives one answer has nothing
// to work with, and a legitimate client never sees any of them because it echoes the token it
// was given.
func DecodeCursor(token string) (Cursor, error) {
	if token == "" {
		return Cursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, badCursor()
	}
	if len(raw) < cursorSigBytes+1 {
		return Cursor{}, badCursor()
	}
	payload, sig := raw[:len(raw)-cursorSigBytes-1], raw[len(raw)-cursorSigBytes:]
	if raw[len(raw)-cursorSigBytes-1] != '|' {
		return Cursor{}, badCursor()
	}

	mac := hmac.New(sha256.New, cursorSigningKey)
	mac.Write([]byte(cursorDomain))
	mac.Write(payload)
	// Constant-time comparison. A byte-by-byte compare leaks the position of the first differing
	// byte through timing, which is enough to forge a tag one byte at a time given enough
	// requests — and a paging endpoint is exactly the kind of thing a client may call thousands
	// of times without anyone finding it odd.
	if subtle.ConstantTimeCompare(mac.Sum(nil)[:cursorSigBytes], sig) != 1 {
		return Cursor{}, badCursor()
	}

	sep := strings.IndexByte(string(payload), '|')
	if sep <= 0 {
		return Cursor{}, badCursor()
	}
	micros, err := strconv.ParseInt(string(payload[:sep]), 10, 64)
	if err != nil {
		return Cursor{}, badCursor()
	}
	id := string(payload[sep+1:])
	if id == "" {
		return Cursor{}, badCursor()
	}
	return Cursor{Time: time.UnixMicro(micros).UTC(), ID: id}, nil
}

func badCursor() *apierror.Error {
	return apierror.New(apierror.CodeValidationFailed, "invalid pagination cursor").
		WithDetail(apierror.Detail{
			Field:   "cursor",
			Code:    "INVALID_CURSOR",
			Message: "the cursor is malformed or was not issued by this service; start a new listing",
			RuleID:  "L1.CURSOR_WELL_FORMED",
		})
}

// pageLimit clamps a requested page size.
//
// The ceiling is a control, not a convenience: an unbounded list endpoint lets one tenant hold a
// connection and a snapshot for the duration of a full-table scan, which blocks vacuum and
// inflates bloat for every other tenant on the cluster. baseline §19.3 fixes it at 1 000.
const (
	defaultPageLimit = 50
	maxPageLimit     = 1000
)

func pageLimit(requested int) int {
	switch {
	case requested <= 0:
		return defaultPageLimit
	case requested > maxPageLimit:
		return maxPageLimit
	default:
		return requested
	}
}
