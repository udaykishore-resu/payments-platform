package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// zeroTraceID is the all-zero W3C trace identifier.
//
// The contract declares `traceId` required and constrains it to 32 hex characters, so an
// un-traced request — a probe, a unit test, a request that arrived before the tracing
// middleware was wired — still has to render *something* that matches. The all-zero value is
// the trace-context specification's own "invalid trace id", which is the honest answer: it
// tells a reader there was no trace rather than inventing one that resolves to nothing.
const zeroTraceID = "00000000000000000000000000000000"

// Problem is the RFC 9457 document, with this platform's extensions.
//
// It is a distinct type from apierror.Error rather than a set of JSON tags on it, and that is
// deliberate. apierror.Error is an internal value that carries a wrapped cause, an unexported
// field whose whole purpose is to reach the logs and never a response. Serializing the internal
// type directly is exactly the mistake that leaks a table name or a DSN fragment to a caller
// the first time somebody adds a field and forgets a `json:"-"`. Marshalling a separate,
// explicitly-populated struct makes the leak impossible by construction rather than by
// vigilance.
type Problem struct {
	Type      string          `json:"type"`
	Title     string          `json:"title"`
	Status    int             `json:"status"`
	Code      string          `json:"code"`
	Detail    string          `json:"detail,omitempty"`
	Instance  string          `json:"instance,omitempty"`
	Category  string          `json:"category"`
	Retryable bool            `json:"retryable"`
	RequestID string          `json:"requestId"`
	TraceID   string          `json:"traceId"`
	Details   []ProblemDetail `json:"details,omitempty"`
	DocsURL   string          `json:"docsUrl,omitempty"`
	// RetryAfterSeconds mirrors the Retry-After header. It is duplicated into the body because
	// a problem document pasted into a ticket has to be self-contained: a reader who has the
	// body but not the headers still needs to know the operation was retryable and when.
	RetryAfterSeconds int `json:"retryAfterSeconds,omitempty"`
}

// ProblemDetail is one attributable cause within a problem document.
type ProblemDetail struct {
	Field   string `json:"field,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
	RuleID  string `json:"ruleId,omitempty"`
}

// WriteProblem renders err as application/problem+json and is the only function in this process
// permitted to write an error to a client.
//
// It takes the request so that `instance`, the request id and the trace id are read from one
// place rather than being threaded through every call site — the version of this function that
// took them as parameters had four call sites passing "" for the trace id.
//
// The internal cause is logged here, at ERROR for 5xx and at DEBUG otherwise, and never
// serialized. That asymmetry is the point of the whole file: the operator gets the cause, the
// caller gets a stable code they can branch on.
func WriteProblem(w http.ResponseWriter, r *http.Request, err error) {
	e := apierror.From(err)
	if e == nil {
		return
	}
	ctx := r.Context()
	status := e.HTTPStatus()

	p := Problem{
		Type:              e.TypeURI(),
		Title:             TitleFor(e.Code),
		Status:            status,
		Code:              string(e.Code),
		Detail:            e.Message,
		Instance:          r.URL.EscapedPath(),
		Category:          string(e.Category),
		Retryable:         e.Retryable,
		RequestID:         RequestID(ctx),
		TraceID:           traceIDOrZero(ctx),
		DocsURL:           e.DocsURL(),
		RetryAfterSeconds: e.RetryAfterSeconds,
	}
	for _, d := range e.Details {
		p.Details = append(p.Details, ProblemDetail{
			Field:   d.Field,
			Code:    orDefault(d.Code, string(e.Code)),
			Message: d.Message,
			RuleID:  d.RuleID,
		})
	}

	logProblem(r, e, status)

	h := w.Header()
	h.Set(HeaderContentType, MediaProblem)
	// Money-path errors are never cacheable: a cached 409 replayed by an intermediary would
	// tell a client its retry conflicted when it never reached us.
	h.Set(HeaderCacheControl, "no-store")
	if p.RequestID != "" {
		h.Set(HeaderRequestID, p.RequestID)
	}
	if e.RetryAfterSeconds > 0 {
		h.Set(HeaderRetryAfter, strconv.Itoa(e.RetryAfterSeconds))
	}
	if status == http.StatusUnauthorized && h.Get("WWW-Authenticate") == "" {
		h.Set("WWW-Authenticate", `Bearer realm="payments-platform", error="invalid_token"`)
	}
	w.WriteHeader(status)
	// A marshalling failure here is unreachable — every field is a string, int or bool — but
	// ignoring the error silently would leave a caller with a status and an empty body and no
	// explanation anywhere. Encode straight to the wire; if it fails the connection is already
	// broken and there is nothing left to say.
	_ = json.NewEncoder(w).Encode(p)
}

// logProblem writes the cause to the log, where it belongs.
//
// 5xx is ERROR because somebody is expected to look; 4xx is DEBUG because a client sending a
// malformed body is not an operational event and logging it at INFO turns a scripted attack
// into a log-volume incident. The error *text* uses err.Error(), which includes the wrapped
// cause — this is the one place that is correct.
func logProblem(r *http.Request, e *apierror.Error, status int) {
	lg := telemetry.Logger(r.Context()).With(
		slog.String(telemetry.KeyRoute, RouteTemplate(r.Context())),
		slog.String(telemetry.KeyMethod, r.Method),
		slog.Int(telemetry.KeyStatus, status),
		slog.String(telemetry.KeyErrorCode, string(e.Code)),
		slog.String(telemetry.KeyErrorCategory, string(e.Category)),
	)
	if status >= http.StatusInternalServerError {
		lg.Error("request failed", slog.String(telemetry.KeyErrorMessage, e.Error()))
		return
	}
	lg.Debug("request rejected", slog.String(telemetry.KeyErrorMessage, e.Error()))
}

// traceIDOrZero returns the request's W3C trace id, or the all-zero id when the request was
// never traced. See zeroTraceID for why the fallback is not the empty string.
func traceIDOrZero(ctx context.Context) string {
	if id := telemetry.TraceIDFromContext(ctx); id != "" {
		return id
	}
	return zeroTraceID
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// TitleFor returns the stable, never-parameterised human summary for an error code.
//
// The contract requires `title` to be constant for a given `code` — a client that renders it,
// or an alert that groups on it, breaks the moment the title starts carrying a payment id. The
// platform error's Message field is the opposite: instance-specific by design. So the title is
// derived from the code and the message becomes `detail`.
//
// Codes with a hand-written title are listed; anything else is humanised mechanically, which
// keeps a newly-registered code from rendering an empty title before somebody writes one.
func TitleFor(c apierror.Code) string {
	if t, ok := problemTitles[c]; ok {
		return t
	}
	return humanise(string(c))
}

// problemTitles are the hand-written titles. They exist for the codes whose mechanical
// humanisation would read badly ("PAN" and "3DS" are not words) or whose wording is quoted in
// the OpenAPI examples and in client documentation.
var problemTitles = map[apierror.Code]string{
	apierror.CodeValidationFailed:             "Request failed schema validation",
	apierror.CodeSensitiveDataInRequest:       "Request contained cardholder data",
	apierror.CodeMalformedRequest:             "Request body could not be parsed",
	apierror.CodeUnsupportedMediaType:         "Unsupported media type",
	apierror.CodeRequestTooLarge:              "Request body too large",
	apierror.CodeMissingRequiredHeader:        "A required header is missing",
	apierror.CodeIdempotencyKeyRequired:       "Idempotency-Key header is required",
	apierror.CodeIdempotencyKeyReused:         "Idempotency key reused with a different request",
	apierror.CodeIdempotentRequestInProgress:  "A request with this idempotency key is already in progress",
	apierror.CodeUnauthenticated:              "Authentication failed",
	apierror.CodeInvalidToken:                 "Access token is not valid",
	apierror.CodeTokenExpired:                 "Access token has expired",
	apierror.CodeForbidden:                    "Insufficient scope",
	apierror.CodeInsufficientScope:            "Insufficient scope",
	apierror.CodeTenantMismatch:               "Resource belongs to a different tenant",
	apierror.CodeMissingTenantContext:         "Tenant context is missing",
	apierror.CodeThreeDsRequired:              "Strong customer authentication is required",
	apierror.CodeRateLimited:                  "Rate limit exceeded",
	apierror.CodeConcurrencyLimitExceeded:     "Concurrency limit exceeded",
	apierror.CodeServiceUnavailable:           "Service temporarily unavailable",
	apierror.CodeInternalError:                "Internal server error",
	apierror.CodeGatewayContractViolation:     "Gateway response violated its contract",
	apierror.CodeConfigurationVersionConflict: "Configuration version conflict",
}

// humanise turns SCREAMING_SNAKE into "Sentence case", which is the shape every hand-written
// title above has.
func humanise(code string) string {
	parts := strings.Split(strings.ToLower(code), "_")
	if len(parts) == 0 || parts[0] == "" {
		return "Error"
	}
	parts[0] = strings.ToUpper(parts[0][:1]) + parts[0][1:]
	return strings.Join(parts, " ")
}
