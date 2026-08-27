package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// DefaultPageLimit and MaxPageLimit bound cursor pagination, matching the `limit` parameter in
// the contract. They are constants here as well as in the OpenAPI document because a server
// that trusts the document's `maximum` is a server with no bound at all.
const (
	DefaultPageLimit = 50
	MaxPageLimit     = 200
)

// WriteJSON renders a success response.
//
// Every response on this surface is `no-store`. That is not a performance oversight: a payment,
// a merchant and a configuration are all mutable, tenant-scoped and occasionally
// personal-data-bearing, and a cached copy at an intermediary is a copy this platform cannot
// revoke, cannot audit and did not authorise.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	h := w.Header()
	h.Set(HeaderContentType, MediaJSON)
	h.Set(HeaderCacheControl, "no-store")
	if id := RequestID(r.Context()); id != "" {
		h.Set(HeaderRequestID, id)
	}
	if body == nil {
		w.WriteHeader(status)
		return
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteNoContent renders a bodyless response, used for 304.
func WriteNoContent(w http.ResponseWriter, r *http.Request, status int) {
	if id := RequestID(r.Context()); id != "" {
		w.Header().Set(HeaderRequestID, id)
	}
	w.Header().Set(HeaderCacheControl, "no-store")
	w.WriteHeader(status)
}

// SetETag stamps the optimistic-concurrency token.
//
// The value is quoted because RFC 9110 requires it and because an unquoted ETag is silently
// dropped by several intermediaries — which turns every conditional write into a 428 and looks
// like a client bug.
func SetETag(w http.ResponseWriter, version int64) {
	w.Header().Set(HeaderETag, strconv.Quote(strconv.FormatInt(version, 10)))
}

// SetETagRaw stamps a non-numeric token, used for configuration documents whose ETag is a
// digest rather than an aggregate version.
func SetETagRaw(w http.ResponseWriter, token string) {
	if token == "" {
		return
	}
	if strings.HasPrefix(token, `"`) {
		w.Header().Set(HeaderETag, token)
		return
	}
	w.Header().Set(HeaderETag, strconv.Quote(token))
}

// SetLocation stamps the absolute URL of a created resource. The base comes from configuration
// rather than from the Host header: trusting Host lets a caller poison the Location of a
// resource they just created, which is a redirect primitive handed to an attacker.
func SetLocation(w http.ResponseWriter, base, path string) {
	if base == "" {
		w.Header().Set(HeaderLocation, path)
		return
	}
	w.Header().Set(HeaderLocation, strings.TrimRight(base, "/")+path)
}

// ETagMatches implements the If-Match / If-None-Match comparison.
//
// Comparison is on the unquoted token and treats the weak prefix `W/` as equal to its strong
// form. That is a deliberate relaxation of RFC 9110's strong-comparison rule for If-Match: the
// tokens here are aggregate versions, an intermediary that weakens one has not changed its
// meaning, and rejecting a weakened tag would fail a correct client's write for a reason it
// cannot see or fix.
func ETagMatches(header, current string) bool {
	if header == "" || current == "" {
		return false
	}
	cur := normaliseETag(current)
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" {
			return true
		}
		if normaliseETag(part) == cur {
			return true
		}
	}
	return false
}

func normaliseETag(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "W/")
	return strings.Trim(s, `"`)
}

// RequireIfMatch enforces the conditional-write precondition on PATCH and PUT.
//
// A missing header is 428, not 412, and the distinction matters operationally: 412 says "you
// read a stale version", which sends an engineer looking for a race, while 428 says "you did
// not read at all", which is a one-line fix in their client.
func RequireIfMatch(r *http.Request) (string, error) {
	v := strings.TrimSpace(r.Header.Get(HeaderIfMatch))
	if v == "" {
		// PRECONDITION_REQUIRED, not MISSING_REQUIRED_HEADER: the latter is catalogued as 400,
		// and 400 tells a client their request was malformed when in fact it was well-formed and
		// merely unconditional. 428 is the status that says "read first, then write".
		return "", apierror.New(apierror.CodePreconditionRequired,
			"this operation requires an If-Match header carrying the ETag you read").
			WithDetail(apierror.Detail{
				Field: HeaderIfMatch, Code: "PRECONDITION_REQUIRED",
				Message: "Read the resource, then repeat the write with its ETag in If-Match.",
				RuleID:  "L1.CONDITIONAL_WRITE_REQUIRED",
			})
	}
	return v, nil
}

// Page is the decoded cursor-pagination request.
type Page struct {
	Limit  int
	Cursor string
}

// DecodePage reads `limit` and `cursor` from the query string.
//
// An out-of-range limit is an error rather than a silent clamp. Clamping is friendlier right up
// until a client asks for 10 000 rows, receives 200, and builds a pagination loop on the
// assumption that a short page means the end — at which point they silently lose 98 % of their
// data and blame the platform six weeks later.
func DecodePage(r *http.Request) (Page, error) {
	p := Page{Limit: DefaultPageLimit, Cursor: r.URL.Query().Get("cursor")}
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return p, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > MaxPageLimit {
		return Page{}, apierror.Newf(apierror.CodeValidationFailed,
			"limit must be an integer between 1 and %d", MaxPageLimit).
			WithDetail(apierror.Detail{
				Field: "limit", Code: "OUT_OF_RANGE",
				Message: "Permitted range is 1 to " + strconv.Itoa(MaxPageLimit) + ".",
				RuleID:  "L1.PAGE_LIMIT_RANGE",
			})
	}
	p.Limit = n
	return p, nil
}

// PageOf renders the cursor-pagination envelope.
//
// `next_cursor` is always present and is null on the last page rather than omitted, so a client
// can branch on its value without a key-presence check — the contract says so explicitly, and
// the reason is that a key check and a null check disagree in exactly one language per team.
func PageOf[T any](data []T, nextCursor string) map[string]any {
	if data == nil {
		data = []T{}
	}
	out := map[string]any{"data": data, "next_cursor": nil}
	if nextCursor != "" {
		out["next_cursor"] = nextCursor
	}
	return out
}
