package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// DefaultMaxBodyBytes is the ceiling on a request body: 256 KiB.
//
// The number is chosen against the largest legitimate document on this surface, which is a
// merchant configuration with a long routing rule set and a full webhook endpoint list — a few
// tens of kilobytes. 256 KiB leaves an order of magnitude of headroom and still means a
// thousand concurrent requests cannot buffer more than 256 MiB, which is what stops a body-size
// attack from being an out-of-memory kill. The webhook ingress raises it separately, because a
// gateway's event body is not ours to bound.
const DefaultMaxBodyBytes int64 = 256 << 10

// ReadBody buffers the request body under a hard size limit and scans it for cardholder data.
//
// # Why the body is buffered rather than streamed into the decoder
//
// Three consumers need the exact bytes: the idempotency fingerprint (which must be computed
// over what the client sent, not over a re-encoding), the PAN detector (which must see the raw
// text, because a card number split across a struct's fields is still a card number in the
// bytes), and the webhook signature verifier. A streaming decode would consume the reader and
// leave the other two with nothing. The size limit is what makes buffering safe.
//
// # Why the PAN scan runs before parsing
//
// Baseline §17: the L1 detector is the outermost control, and it must run before any code path
// that could log, store or echo the value. Parsing first means the value exists in a struct
// field, and a struct field is one `slog.Any` away from a log line. Scanning the raw bytes
// costs a single pass and removes the whole class.
func ReadBody(w http.ResponseWriter, r *http.Request, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBodyBytes
	}
	if r.Body == nil {
		return nil, nil
	}
	limited := http.MaxBytesReader(w, r.Body, maxBytes)
	raw, err := io.ReadAll(limited)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, apierror.Newf(apierror.CodeRequestTooLarge,
				"request body exceeds the %d byte limit for this endpoint", maxBytes)
		}
		return nil, apierror.Wrap(err, apierror.CodeMalformedRequest, "the request body could not be read")
	}
	if err := ScanForPAN(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ScanForPAN rejects a body containing a Luhn-valid primary account number.
//
// The rejection deliberately says nothing about *what* matched. Echoing the offending value —
// even truncated, even masked — would put cardholder data into a response body, a client's log
// and this platform's own access log, which is exactly the outcome the detector exists to
// prevent. The caller is told which field, and that the value was discarded.
func ScanForPAN(raw []byte) error {
	if len(raw) == 0 || !secret.ContainsPAN(string(raw)) {
		return nil
	}
	field := panField(raw)
	return apierror.New(apierror.CodeSensitiveDataInRequest,
		"a field in this request matched the primary account number detector; the value has been "+
			"discarded and was not logged. Submit a gateway token or a vault reference instead of card data").
		WithDetail(apierror.Detail{
			Field:   field,
			Code:    "PAN_DETECTED",
			Message: "Value redacted. Cardholder data must never reach this API.",
			RuleID:  "L1.NO_CARDHOLDER_DATA",
		})
}

// panField makes a best effort to name the offending field so the caller can fix their
// integration, without ever quoting its value.
//
// It re-parses the body as a generic map and re-scans each leaf. A parse failure is not an
// error here: the body already failed the detector, and refusing to name the field is strictly
// better than refusing to reject it.
func panField(raw []byte) string {
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return ""
	}
	return walkForPAN("", generic)
}

func walkForPAN(path string, v any) string {
	switch t := v.(type) {
	case string:
		if secret.ContainsPAN(t) {
			return path
		}
	case map[string]any:
		for k, child := range t {
			if hit := walkForPAN(joinPath(path, k), child); hit != "" {
				return hit
			}
		}
	case []any:
		for i, child := range t {
			if hit := walkForPAN(fmt.Sprintf("%s[%d]", path, i), child); hit != "" {
				return hit
			}
		}
	}
	return ""
}

func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// DecodeJSON strictly decodes raw into dst.
//
// Strict means three things, and each of them exists because its absence has caused a
// production incident somewhere:
//
//   - Unknown fields are rejected. A client that sends `statementDescripter` and is silently
//     ignored ships a bug to their customers and blames the platform. Rejecting names the typo.
//   - Exactly one JSON value per body. Two concatenated documents would otherwise be decoded as
//     the first, silently discarding the second — the request-smuggling shape of a bug.
//   - The reported field name is the *wire* name, from the decoder's own error, so a caller
//     reading VALIDATION_FAILED sees the name they sent rather than a Go field name they have
//     never heard of.
//
// An empty body is a distinct, named failure rather than a decode error: "unexpected end of
// JSON input" tells a caller nothing about what to do next.
func DecodeJSON(raw []byte, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return apierror.New(apierror.CodeMalformedRequest, "a JSON request body is required").
			WithDetail(apierror.Detail{
				Field: "", Code: "EMPTY_BODY", Message: "The request body was empty.",
				RuleID: "L1.BODY_REQUIRED",
			})
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	// One value per body. io.EOF here means the document ended where it should have.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return apierror.New(apierror.CodeMalformedRequest,
			"the request body must contain exactly one JSON value").
			WithDetail(apierror.Detail{
				Code: "TRAILING_CONTENT", Message: "Content was found after the first JSON value.",
				RuleID: "L1.SINGLE_JSON_VALUE",
			})
	}
	return nil
}

// decodeError converts encoding/json's error vocabulary into VALIDATION_FAILED details that
// name the offending field.
//
// json's errors are structured enough to do this properly for the two cases that matter — a
// wrong type and an unknown field — and those two are the overwhelming majority of real
// integration mistakes. Everything else degrades to MALFORMED_REQUEST with the parser's own
// text, which is safe to echo because it describes the caller's bytes and never ours.
func decodeError(err error) error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		field := typeErr.Field
		if field == "" {
			field = "(root)"
		}
		return apierror.Newf(apierror.CodeValidationFailed, "field %q has the wrong type", field).
			WithDetail(apierror.Detail{
				Field: field, Code: "WRONG_TYPE",
				Message: fmt.Sprintf("Expected %s, received %s.", typeErr.Type, typeErr.Value),
				RuleID:  "L1.FIELD_TYPE",
			})
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return apierror.Newf(apierror.CodeMalformedRequest,
			"the request body is not valid JSON (byte offset %d)", syntaxErr.Offset)
	}
	// DisallowUnknownFields reports through a plain error whose text is
	// `json: unknown field "foo"`. Parsing it is unattractive but the alternative is telling a
	// caller "malformed request" when the actual problem is one misspelled key.
	if name, ok := unknownFieldName(err); ok {
		return apierror.Newf(apierror.CodeValidationFailed, "unknown field %q", name).
			WithDetail(apierror.Detail{
				Field: name, Code: "UNKNOWN_FIELD",
				Message: "This field is not part of the request schema for this operation.",
				RuleID:  "L1.NO_UNKNOWN_FIELDS",
			})
	}
	return apierror.Wrap(err, apierror.CodeMalformedRequest, "the request body could not be decoded")
}

const unknownFieldPrefix = "json: unknown field "

func unknownFieldName(err error) (string, bool) {
	msg := err.Error()
	if !strings.HasPrefix(msg, unknownFieldPrefix) {
		return "", false
	}
	return strings.Trim(strings.TrimPrefix(msg, unknownFieldPrefix), `"`), true
}

// RequireContentType enforces `application/json` on requests that carry a body.
//
// It is separate from decoding because the answer is a different status — 415, not 400 — and
// because a proxy that rewrites a content type must be caught before the bytes are parsed
// rather than after they fail to decode into something plausible.
func RequireContentType(r *http.Request, want string) error {
	if r.ContentLength == 0 && r.Header.Get(HeaderContentType) == "" {
		return nil
	}
	ct := r.Header.Get(HeaderContentType)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	if !strings.EqualFold(strings.TrimSpace(ct), want) {
		return apierror.Newf(apierror.CodeUnsupportedMediaType,
			"this endpoint accepts %s; the request declared %q", want, r.Header.Get(HeaderContentType))
	}
	return nil
}
