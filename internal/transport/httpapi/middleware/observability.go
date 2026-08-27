package middleware

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
)

// Recover is the outermost stage: it converts a panic into a 500 and keeps the process alive.
//
// Budget: none — it costs a deferred function per request, which is nanoseconds.
// Fails with: 500 INTERNAL_ERROR.
//
// # Why the stack never reaches the body
//
// A Go stack trace names package paths, file names, line numbers and, through argument
// registers in some formats, values. Publishing it tells an attacker exactly which library
// versions are deployed and where the parsing happens, and it occasionally publishes a token
// that happened to be a function argument. The stack goes to the log, at ERROR, where the
// people who need it are; the caller gets a stable code and the request id that finds the log
// line.
//
// http.ErrAbortHandler is re-panicked rather than handled: net/http raises it to abort a
// response deliberately (a reverse proxy giving up on a dead upstream), and swallowing it would
// convert an intentional abort into a 500 that looks like our bug.
func Recover(metrics MetricsSink, service string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { //nolint:contextcheck // the recover path reads r.Context() for its log fields only; there is no downstream call to propagate a context to
				rec := recover()
				if rec == nil {
					return
				}
				if e, ok := rec.(error); ok && errors.Is(e, http.ErrAbortHandler) {
					panic(rec)
				}
				stack := debug.Stack()
				telemetry.Logger(r.Context()).Error("panic in handler",
					slog.String(telemetry.KeyRoute, httpapi.RouteTemplate(r.Context())),
					slog.String(telemetry.KeyMethod, r.Method),
					slog.String(telemetry.KeyStack, string(stack)),
				)
				if metrics != nil {
					metrics.ObserveHTTPRequest(r.Context(), service,
						httpapi.RouteTemplate(r.Context()), r.Method,
						http.StatusInternalServerError, tierOf(r), 0)
				}
				// The panic may have happened after the handler wrote a header, in which case
				// there is nothing left to say and writing again would corrupt the response.
				if rr, ok := w.(*ResponseRecorder); ok && rr.Wrote() {
					return
				}
				// This stage is outermost, so it holds the request as it was *before* the
				// requestid stage decorated the context — and a problem document with no
				// requestId is one an operator cannot trace to a log line. The id is recovered
				// from the response header, which requestid stamps eagerly for exactly this
				// case: a response the handler never reached.
				if id := w.Header().Get(httpapi.HeaderRequestID); id != "" &&
					httpapi.RequestID(r.Context()) == "" {
					r = r.WithContext(httpapi.WithRequestID(r.Context(), id))
				}
				httpapi.WriteProblem(w, r, apierror.New(apierror.CodeInternalError,
					"the request could not be completed"))
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// RequestID stamps the correlation identifiers on the context and echoes them on the response.
//
// Budget: 1 ms (§12 stage 2). Fails with: nothing — it cannot fail.
//
// A caller-supplied X-Request-Id is honoured but bounded and sanitised: an unbounded header
// copied into a log line is a log-injection primitive, and a 64 KiB request id in every log line
// for a request is a log-volume incident. Anything outside the bound or containing a control
// character is replaced by a generated `req_` ULID rather than rejected, because failing a
// payment over a malformed correlation header would be a self-inflicted outage.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqID := sanitiseID(r.Header.Get(httpapi.HeaderRequestID))
			if reqID == "" {
				reqID = string(ids.New(ids.PrefixRequest))
			}
			corrID := sanitiseID(r.Header.Get(httpapi.HeaderCorrelationID))
			if corrID == "" {
				corrID = reqID
			}

			ctx := httpapi.WithCorrelationID(httpapi.WithRequestID(r.Context(), reqID), corrID)
			ctx = telemetry.ContextWithFields(ctx, telemetry.Fields{
				RequestID:     reqID,
				CorrelationID: corrID,
			})

			// Echoed before the handler runs so it is present even on a response the handler
			// never reaches — a 429 or a panic still carries the id an operator needs.
			w.Header().Set(httpapi.HeaderRequestID, reqID)
			w.Header().Set(httpapi.HeaderCorrelationID, corrID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// maxIDLength bounds a caller-supplied correlation header. 128 matches the contract's own
// maxLength for X-Request-Id, so a value this platform accepts is a value the contract declares.
const maxIDLength = 128

func sanitiseID(s string) string {
	if s == "" || len(s) > maxIDLength {
		return ""
	}
	for i := 0; i < len(s); i++ {
		// Printable ASCII only. A newline here is a forged log line; a byte above 0x7E is a
		// header a downstream proxy may re-encode differently from us, which breaks correlation
		// in the one place correlation matters.
		if s[i] < 0x20 || s[i] > 0x7E {
			return ""
		}
	}
	return s
}

// Tracing continues the caller's W3C trace or starts a new one, and names the span with the
// route *template*.
//
// Budget: 1 ms (§12 stage 2). Fails with: nothing.
//
// # The span name
//
// `POST /v1/payments/{paymentId}/capture`, never `POST /v1/payments/pay_01JB.../capture`. The
// raw path produces one span name per payment. That is unbounded cardinality — which in most
// tracing backends means the operation-name index grows without limit and service maps become
// unusable — and it writes a customer's payment identifier into a field that is indexed,
// retained for months and shared with a vendor. The template is resolved here, before the mux
// runs, precisely so that 404s and requests rejected by authentication are also inside a span:
// those are where the interesting attacks are, and a chain that only traced matched routes
// would not see them.
func Tracing(routes RouteResolver, service string) Middleware {
	propagator := otel.GetTextMapPropagator()
	if propagator == nil {
		propagator = propagation.TraceContext{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			template := "unmatched"
			if routes != nil {
				template = routes.Resolve(r)
			}
			ctx := httpapi.WithRouteTemplate(r.Context(), template)
			ctx = httpapi.WithPriority(ctx, httpapi.PriorityOfRoute(r.Method, template))

			ctx = propagator.Extract(ctx, propagation.HeaderCarrier(r.Header))
			ctx, span := telemetry.StartSpan(ctx, r.Method+" "+template,
				attribute.String("http.request.method", r.Method),
				attribute.String("http.route", template),
				attribute.String("server.address", r.Host),
				attribute.String("url.scheme", schemeOf(r)),
				attribute.String("service.name", service),
			)
			defer span.End()

			// The traceparent goes back on the response so a client can put the id in a support
			// ticket. It is the *server's* span context, not the caller's, because that is the
			// span an operator will search for.
			propagator.Inject(ctx, propagation.HeaderCarrier(w.Header()))

			rec, _ := recorderFor(w)
			next.ServeHTTP(rec, r.WithContext(ctx))

			span.SetAttributes(attribute.Int("http.response.status_code", rec.Status()))
			// Only 5xx sets the span status to Error. A 402, a 409 replay-in-progress and a 422
			// risk decline are correct outcomes; marking them as span errors makes the
			// collector's keep-all-errors tail policy retain every business decline, which
			// destroys both the trace budget and the meaning of every error-rate panel built on
			// span status. Same classification rule as telemetry.RecordError, for the same
			// reason.
			if rec.Status() >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, http.StatusText(rec.Status()))
			}
		})
	}
}

func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v == "https" {
		return "https"
	}
	return "http"
}

// Logging writes one structured access line per request.
//
// Budget: 1 ms. Fails with: nothing.
//
// # What is deliberately absent
//
// No request body, no response body, no Authorization header, no cookie, no query string. The
// query string is excluded even though it looks harmless: `?externalReference=` carries a
// merchant's own identifier for a customer, and `?cursor=` carries a signed token that is a
// bearer credential for one page of one tenant's data. Only the *names* of the query parameters
// are logged, which is enough to answer "is anyone still using the deprecated filter?" and not
// enough to reconstruct a request.
//
// The line is emitted after the handler so it carries the status and duration. Emitting one
// line before and one after doubles log volume for no information a single line does not have.
func Logging(base *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec, _ := recorderFor(w)
			next.ServeHTTP(rec, r)
			dur := time.Since(start)

			lg := telemetry.Logger(r.Context())
			if lg == nil {
				lg = base
			}
			attrs := []any{
				slog.String(telemetry.KeyMethod, r.Method),
				slog.String(telemetry.KeyRoute, httpapi.RouteTemplate(r.Context())),
				slog.Int(telemetry.KeyStatus, rec.Status()),
				slog.Int64(telemetry.KeyDurationMS, dur.Milliseconds()),
			}
			switch {
			case rec.Status() >= http.StatusInternalServerError:
				lg.Error("http request", attrs...)
			case rec.Status() >= http.StatusBadRequest:
				// 4xx at DEBUG: a client sending malformed requests is not an operational
				// event, and logging it at INFO turns a scripted probe into a log-volume
				// incident that costs money and hides the real errors.
				lg.Debug("http request", attrs...)
			default:
				lg.Info("http request", attrs...)
			}
		})
	}
}

// Metrics records the RED series for this request.
//
// Budget: 1 ms. Fails with: nothing.
//
// Separate from Logging on purpose. Logs are sampled — telemetry.SamplingOptions drops
// repetitive lines under load — and a metric derived from a sampled log undercounts exactly
// when the count matters. The label set is (service, route template, method, status class,
// tenant tier): five bounded dimensions, no merchant id and no payment id, per baseline §22.3.
func Metrics(sink MetricsSink, service string) Middleware {
	return func(next http.Handler) http.Handler {
		if sink == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec, _ := recorderFor(w)
			next.ServeHTTP(rec, r)
			sink.ObserveHTTPRequest(r.Context(), service,
				httpapi.RouteTemplate(r.Context()), r.Method, rec.Status(),
				tierOf(r), time.Since(start))
		})
	}
}

// tierOf reads the tenant tier for the metric label, defaulting to pooled.
//
// Defaulting rather than omitting: an empty label value produces a distinct series that looks
// like a bug in the instrumentation and breaks every `sum by (tenant_tier)` panel.
func tierOf(r *http.Request) telemetry.TenantTier {
	tc, err := tenantctx.FromContext(r.Context())
	if err != nil {
		return telemetry.TierPooled
	}
	switch tc.Tier {
	case shared.TierSiloed:
		return telemetry.TierSiloed
	default:
		return telemetry.TierPooled
	}
}

// recorderFor returns w as a ResponseRecorder, wrapping it when an outer stage has not already.
//
// One recorder per request, not one per stage: each wrapper adds an indirection to every Write,
// and five of them on a streaming response is measurable. The bool reports whether this call
// created the wrapper, which the caller uses to decide whether to pass the wrapper down.
func recorderFor(w http.ResponseWriter) (*ResponseRecorder, bool) {
	if rr, ok := w.(*ResponseRecorder); ok {
		return rr, false
	}
	return NewRecorder(w), true
}
