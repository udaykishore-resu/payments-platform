package telemetry

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Log field names. They are constants because three separate systems parse them — the Loki
// pipeline, the alert queries and the runbooks — and a renamed field is a silently broken alert
// rather than a compile error. The set is baseline §22.1 (mandatory context) plus the
// observability.md §5.1 schema.
const (
	KeyTime          = "ts"
	KeyLevel         = "level"
	KeyMessage       = "msg"
	KeyService       = "service"
	KeyVersion       = "version"
	KeyEnvironment   = "environment"
	KeyRegion        = "region"
	KeyPod           = "pod"
	KeyTraceID       = "trace_id"
	KeySpanID        = "span_id"
	KeySampled       = "sampled"
	KeyCorrelationID = "correlation_id"
	KeyRequestID     = "request_id"
	KeyCausationID   = "causation_id"
	KeyTenantID      = "tenant_id"
	KeyTenantTier    = "tenant_tier"
	KeyMerchantID    = "merchant_id"
	KeyPaymentID     = "payment_id"
	KeyAttemptID     = "attempt_id"
	KeyGatewayID     = "gateway_id"
	KeyWorkflowID    = "workflow_id"
	KeyWorkflowStep  = "workflow_step"
	KeyEventID       = "event_id"
	KeyEventType     = "event_type"
	KeyRoute         = "route"
	KeyMethod        = "method"
	KeyStatus        = "status"
	KeyDurationMS    = "duration_ms"
	KeyOutcome       = "outcome"
	KeyAmountMinor   = "amount_minor"
	KeyCurrency      = "currency"
	KeyErrorCode     = "error.code"
	KeyErrorCategory = "error.category"
	KeyErrorRetry    = "error.retryable"
	KeyErrorMessage  = "error.message"
	KeyErrorChain    = "error.chain"
	KeyRuleID        = "rule_id"
	KeyIdemKeyHash   = "idempotency_key_hash"
	KeyAuditID       = "audit_id"
	KeyStack         = "stack"
	KeyCount         = "count"
	KeyAttemptNumber = "attempt_number"
	KeyTopic         = "topic"
	KeyPartition     = "partition"
	KeyOffset        = "offset"
	KeyQueue         = "queue"
	KeyReason        = "reason"
	KeyState         = "state"
	KeyPreviousState = "previous_state"

	// KeySuppressedCount appears on the first line emitted after the sampler dropped others of
	// the same shape, so that a reader can tell "this happened once" from "this happened 4 000
	// times and you are seeing one of them".
	KeySuppressedCount = "suppressed_count"
)

// DefaultLogKeys is the serializer allowlist: the complete set of attribute keys this platform
// is permitted to emit.
//
// Allowlist, not denylist, and the reasoning is not symmetric. A denylist enumerates the fields
// you have thought of — `pan`, `cvv`, `authorization` — and admits everything else, so it fails
// open every time someone adds a field, which is every week. The failure is silent and its
// consequence is a PCI-scope breach found by a log-pipeline detector months later. An allowlist
// enumerates what is permitted and drops the rest, so it fails *closed* on the new field: the
// engineer sees their field missing, looks at pp_log_field_rejected_total, adds the key here,
// and that addition is a diff a reviewer reads. The cost is one review per new field; the
// alternative price is a breach. For a payments platform that is not a close call.
func DefaultLogKeys() []string {
	return []string{
		KeyService, KeyVersion, KeyEnvironment, KeyRegion, KeyPod,
		KeyTraceID, KeySpanID, KeySampled,
		KeyCorrelationID, KeyRequestID, KeyCausationID,
		KeyTenantID, KeyTenantTier, KeyMerchantID, KeyPaymentID, KeyAttemptID, KeyGatewayID,
		KeyWorkflowID, KeyWorkflowStep, KeyEventID, KeyEventType,
		KeyRoute, KeyMethod, KeyStatus, KeyDurationMS, KeyOutcome,
		KeyAmountMinor, KeyCurrency,
		KeyErrorCode, KeyErrorCategory, KeyErrorRetry, KeyErrorMessage, KeyErrorChain,
		KeyRuleID, KeyIdemKeyHash, KeyAuditID, KeyStack,
		KeyCount, KeyAttemptNumber, KeyTopic, KeyPartition, KeyOffset, KeyQueue,
		KeyReason, KeyState, KeyPreviousState,
		KeySuppressedCount,
	}
}

// --- request-scoped context -------------------------------------------------------------------

// Fields are the correlation dimensions that belong on every line of a request or an event, and
// that a call site must never be asked to remember. They travel in the context because that is
// the only carrier that already crosses every function boundary in the request path; passing
// them explicitly would mean threading eleven parameters through the application layer, which is
// how they end up omitted from exactly the line you need during an incident.
//
// merchant_id, payment_id and attempt_id are legitimate *here* and forbidden as metric labels
// (baseline §22.3). Logs are a per-event store where an unbounded dimension costs storage; a
// metric is a time series where it costs the whole system.
type Fields struct {
	CorrelationID string
	RequestID     string
	CausationID   string
	TenantID      string
	TenantTier    TenantTier
	MerchantID    string
	PaymentID     string
	AttemptID     string
	GatewayID     string
	WorkflowID    string
	WorkflowStep  string
}

type fieldsKey struct{}

// ContextWithFields merges f into ctx, leaving any dimension f does not set intact.
//
// Merge rather than replace, because these fields are established at different points in the
// pipeline: tenant at authentication, merchant when the merchant is loaded, payment when the
// aggregate is created. A replacing setter would mean each of those stages had to know and
// re-state everything an earlier stage learned, and the first stage to forget one silently drops
// it from every subsequent log line.
func ContextWithFields(ctx context.Context, f Fields) context.Context {
	cur, _ := ctx.Value(fieldsKey{}).(Fields)
	merged := cur
	if f.CorrelationID != "" {
		merged.CorrelationID = f.CorrelationID
	}
	if f.RequestID != "" {
		merged.RequestID = f.RequestID
	}
	if f.CausationID != "" {
		merged.CausationID = f.CausationID
	}
	if f.TenantID != "" {
		merged.TenantID = f.TenantID
	}
	if f.TenantTier != "" {
		merged.TenantTier = f.TenantTier
	}
	if f.MerchantID != "" {
		merged.MerchantID = f.MerchantID
	}
	if f.PaymentID != "" {
		merged.PaymentID = f.PaymentID
	}
	if f.AttemptID != "" {
		merged.AttemptID = f.AttemptID
	}
	if f.GatewayID != "" {
		merged.GatewayID = f.GatewayID
	}
	if f.WorkflowID != "" {
		merged.WorkflowID = f.WorkflowID
	}
	if f.WorkflowStep != "" {
		merged.WorkflowStep = f.WorkflowStep
	}
	return context.WithValue(ctx, fieldsKey{}, merged)
}

// FieldsFromContext returns the correlation fields established so far. The zero value is a
// perfectly good answer: a background job that has not entered a request has none.
func FieldsFromContext(ctx context.Context) Fields {
	f, _ := ctx.Value(fieldsKey{}).(Fields)
	return f
}

// --- the logger -------------------------------------------------------------------------------

// baseLogger holds the process-wide logger built by Setup. It is an atomic pointer rather than a
// plain variable because tests and the `platformctl log-level` path replace it while requests are
// in flight, and a torn read of an interface value is a crash in the logging path — the one place
// a crash destroys the evidence of why it happened.
var baseLogger atomic.Pointer[slog.Logger]

func init() {
	// Until Setup runs, log to nothing rather than to stderr. A partially initialized process
	// writing unstructured lines into the same stream the pipeline parses is worse than silence,
	// and every line worth keeping happens after Setup.
	baseLogger.Store(slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

// SetBaseLogger installs the process logger. Setup calls it once; the only other legitimate
// caller is a test.
func SetBaseLogger(l *slog.Logger) {
	if l != nil {
		baseLogger.Store(l)
	}
}

// Logger returns a logger with every correlation dimension available in ctx already bound.
//
// This is the only logger constructor in the platform and it takes a context, which makes it
// impossible to obtain a logger that cannot be correlated. That is the entire design: correlation
// is a property of the constructor, not a convention that each call site must remember. A call
// site writes
//
//	telemetry.Logger(ctx).Info("payment authorized", "gateway_id", gw)
//
// and trace_id, span_id, tenant_id, payment_id and the rest are already there. The alternative —
// a package-level logger plus a discipline of passing IDs — decays within one quarter, and it
// decays first on the error paths, which are the lines you need.
func Logger(ctx context.Context) *slog.Logger {
	l := baseLogger.Load()
	if ctx == nil {
		return l
	}

	attrs := make([]any, 0, 16)
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs,
			slog.String(KeyTraceID, sc.TraceID().String()),
			slog.String(KeySpanID, sc.SpanID().String()),
			// sampled lets a query select only the lines whose trace can actually be fetched.
			// Without it, half the "open the trace" links in an incident lead nowhere.
			slog.Bool(KeySampled, sc.IsSampled()),
		)
	}
	f := FieldsFromContext(ctx)
	attrs = appendIf(attrs, KeyCorrelationID, f.CorrelationID)
	attrs = appendIf(attrs, KeyRequestID, f.RequestID)
	attrs = appendIf(attrs, KeyCausationID, f.CausationID)
	attrs = appendIf(attrs, KeyTenantID, f.TenantID)
	attrs = appendIf(attrs, KeyTenantTier, string(f.TenantTier))
	attrs = appendIf(attrs, KeyMerchantID, f.MerchantID)
	attrs = appendIf(attrs, KeyPaymentID, f.PaymentID)
	attrs = appendIf(attrs, KeyAttemptID, f.AttemptID)
	attrs = appendIf(attrs, KeyGatewayID, f.GatewayID)
	attrs = appendIf(attrs, KeyWorkflowID, f.WorkflowID)
	attrs = appendIf(attrs, KeyWorkflowStep, f.WorkflowStep)

	if len(attrs) == 0 {
		return l
	}
	return l.With(attrs...)
}

func appendIf(dst []any, key, val string) []any {
	if val == "" {
		return dst
	}
	return append(dst, slog.String(key, val))
}

// LogOptions configures the handler chain. Zero values give the production shape.
type LogOptions struct {
	// Level is the minimum level emitted. It is a *slog.LevelVar so that `platformctl
	// log-level` can raise verbosity on a running pod for a bounded window without a restart —
	// a restart loses the very state you turned DEBUG on to look at.
	Level *slog.LevelVar
	// AllowedKeys is the serializer allowlist. Nil means DefaultLogKeys.
	AllowedKeys []string
	// Sampling bounds high-volume lines. The zero value disables sampling.
	Sampling SamplingOptions
	// Metrics receives the drop and suppression counts. Nil is allowed — the handlers then lose
	// their accounting, which is why Setup always passes one.
	Metrics LogAccounting
	// Base are attributes bound to every line: service, version, environment, region, pod.
	Base []slog.Attr
}

// LogAccounting is the narrow slice of the metric registry the log handlers need. It is declared
// here, by the consumer, so that the handlers can be tested with a two-line fake and so that
// logging does not depend on the whole registry.
type LogAccounting interface {
	RecordLogFieldRejected(field string)
	RecordLogLinesSuppressed(level string, n int)
}

// NewLogger builds the handler chain and returns a logger writing newline-delimited JSON to w.
//
// The chain is sampler → allowlist → JSON, in that order, and the order is load-bearing. The
// sampler is outermost so a suppressed line costs one map lookup instead of a full attribute
// filter and an encode. The allowlist sits immediately above the encoder so that nothing can add
// an attribute after the filter has run.
func NewLogger(w io.Writer, opts LogOptions) *slog.Logger {
	level := opts.Level
	if level == nil {
		level = new(slog.LevelVar)
	}
	keys := opts.AllowedKeys
	if keys == nil {
		keys = DefaultLogKeys()
	}

	var h slog.Handler = slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceAttr,
	})
	h = newAllowlistHandler(h, keys, opts.Metrics)
	if opts.Sampling.PerSecond > 0 {
		h = newSamplingHandler(h, opts.Sampling, opts.Metrics)
	}
	l := slog.New(h)
	if len(opts.Base) > 0 {
		l = l.With(attrsToAny(opts.Base)...)
	}
	return l
}

func attrsToAny(attrs []slog.Attr) []any {
	out := make([]any, len(attrs))
	for i, a := range attrs {
		out[i] = a
	}
	return out
}

// replaceAttr renames slog's built-in keys to the §5.1 schema and pins the timestamp format.
// RFC 3339 with milliseconds in UTC, because the pipeline sorts on this string and a local-zone
// or nanosecond-precision timestamp sorts wrong across pods.
func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.TimeKey:
		if t, ok := a.Value.Any().(time.Time); ok {
			return slog.String(KeyTime, t.UTC().Format("2006-01-02T15:04:05.000Z07:00"))
		}
	case slog.LevelKey:
		return slog.String(KeyLevel, a.Value.String())
	case slog.MessageKey:
		return slog.String(KeyMessage, a.Value.String())
	}
	return a
}

// --- allowlist handler --------------------------------------------------------------------------

type allowlistHandler struct {
	next    slog.Handler
	allowed map[string]string // canonical or alias -> canonical
	metrics LogAccounting
}

// newAllowlistHandler wraps next so that only registered attribute keys survive.
//
// Aliases: the HTTP API surface is camelCase and developers type what they see, so `gatewayId`
// is accepted and normalized to `gateway_id` rather than dropped. One canonical wire name, two
// spellings at the call site; the alternative is a stream of correct-looking log calls whose
// fields silently vanish, which teaches people to distrust the allowlist rather than use it.
func newAllowlistHandler(next slog.Handler, keys []string, m LogAccounting) *allowlistHandler {
	allowed := make(map[string]string, len(keys)*2)
	for _, k := range keys {
		allowed[k] = k
		if alias := lowerCamel(k); alias != k {
			allowed[alias] = k
		}
	}
	return &allowlistHandler{next: next, allowed: allowed, metrics: m}
}

func (h *allowlistHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *allowlistHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		if kept, ok := h.filter(a); ok {
			out.AddAttrs(kept)
		}
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *allowlistHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	kept := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		if k, ok := h.filter(a); ok {
			kept = append(kept, k)
		}
	}
	return &allowlistHandler{next: h.next.WithAttrs(kept), allowed: h.allowed, metrics: h.metrics}
}

// WithGroup is passed through unchanged. Groups are namespaces, not values: filtering the group
// name itself would drop `error.code` and friends, which are exactly the fields an ERROR line
// exists to carry.
func (h *allowlistHandler) WithGroup(name string) slog.Handler {
	return &allowlistHandler{next: h.next.WithGroup(name), allowed: h.allowed, metrics: h.metrics}
}

// filter returns the attribute to emit, canonicalized, or false if the key is not registered.
//
// A grouped attribute is looked up under both its bare key and its dotted path, so that
// slog.Group("error", slog.String("code", …)) and slog.String("error.code", …) reach the same
// allowlist entry. The lookups are side-effect free and the rejection is counted once, at the
// end: counting a key as rejected while also emitting it would make pp_log_field_rejected_total
// unreadable, and that counter is the only signal that the allowlist is dropping something.
func (h *allowlistHandler) filter(a slog.Attr) (slog.Attr, bool) {
	if a.Value.Kind() == slog.KindGroup {
		inner := a.Value.Group()
		kept := make([]slog.Attr, 0, len(inner))
		for _, g := range inner {
			switch {
			case g.Value.Kind() == slog.KindGroup:
				if k, ok := h.filter(g); ok {
					kept = append(kept, k)
				}
			case h.permits(g.Key):
				kept = append(kept, slog.Attr{Key: h.allowed[g.Key], Value: g.Value})
			case h.permits(a.Key + "." + g.Key):
				// Keep the short name inside the group; the encoder re-joins them.
				kept = append(kept, slog.Attr{Key: g.Key, Value: g.Value})
			default:
				h.reject(a.Key + "." + g.Key)
			}
		}
		if len(kept) == 0 {
			return slog.Attr{}, false
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(kept...)}, true
	}

	canonical, ok := h.allowed[a.Key]
	if !ok {
		h.reject(a.Key)
		return slog.Attr{}, false
	}
	if canonical != a.Key {
		return slog.Attr{Key: canonical, Value: a.Value}, true
	}
	return a, true
}

// permits reports whether key is registered, without counting a rejection.
func (h *allowlistHandler) permits(key string) bool {
	_, ok := h.allowed[key]
	return ok
}

func (h *allowlistHandler) reject(field string) {
	if h.metrics != nil {
		h.metrics.RecordLogFieldRejected(field)
	}
}

// lowerCamel converts a snake_case key to its lowerCamelCase alias. Dotted keys (error.code) are
// converted segment-wise so that `error.retryable` aliases to `error.retryable` unchanged and
// `idempotency_key_hash` aliases to `idempotencyKeyHash`.
func lowerCamel(k string) string {
	if !strings.ContainsRune(k, '_') {
		return k
	}
	var b strings.Builder
	b.Grow(len(k))
	up := false
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c == '_':
			up = true
		case up:
			if c >= 'a' && c <= 'z' {
				c -= 'a' - 'A'
			}
			b.WriteByte(c)
			up = false
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// --- sampling handler ---------------------------------------------------------------------------

// SamplingOptions bounds the volume of repetitive lines.
type SamplingOptions struct {
	// PerSecond is how many lines of each (level, message) shape are emitted per second before
	// the rest are suppressed and counted. Zero disables sampling entirely.
	PerSecond int
	// Levels are the levels subject to sampling. Nil means DEBUG and INFO only: ERROR and WARN
	// are never sampled, because the rare ones are the ones you need and the cheap ones are the
	// ones you can afford (observability.md §5.3).
	Levels []slog.Level
	// MaxKeys bounds the tracking map. A process that produces more distinct message shapes than
	// this has a bigger problem than log volume, and the sampler fails open rather than becoming
	// the memory leak it exists to prevent. Zero means 4096.
	MaxKeys int
	// Now is injected so the sampler's behaviour is testable without sleeping. Nil means
	// time.Now.
	Now func() time.Time
}

type sampleBucket struct {
	epoch      int64
	emitted    int
	suppressed int
	// pending carries last second's suppressed count onto the next emitted line, so a reader
	// sees "and 3 812 more" attached to a real line rather than as a separate synthetic one.
	pending int
}

// sampleState is the counting state, held behind a pointer and shared by every handler derived
// from one sampler. Sharing is required rather than incidental: every Logger(ctx) call produces
// a derived handler through With, and a sampler whose state were copied per derivation would
// count each request's lines separately and suppress nothing.
type sampleState struct {
	mu      sync.Mutex
	buckets map[string]*sampleBucket
	maxKeys int
}

type samplingHandler struct {
	next      slog.Handler
	perSecond int
	metrics   LogAccounting
	levels    map[slog.Level]struct{}
	now       func() time.Time
	state     *sampleState
}

func newSamplingHandler(next slog.Handler, opts SamplingOptions, m LogAccounting) *samplingHandler {
	levels := map[slog.Level]struct{}{}
	if opts.Levels == nil {
		levels[slog.LevelDebug] = struct{}{}
		levels[slog.LevelInfo] = struct{}{}
	} else {
		for _, l := range opts.Levels {
			levels[l] = struct{}{}
		}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	maxKeys := opts.MaxKeys
	if maxKeys <= 0 {
		maxKeys = 4096
	}
	return &samplingHandler{
		next:      next,
		perSecond: opts.PerSecond,
		metrics:   m,
		levels:    levels,
		now:       now,
		state:     &sampleState{buckets: make(map[string]*sampleBucket), maxKeys: maxKeys},
	}
}

func (h *samplingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

// Handle emits the first PerSecond lines of each (level, message) shape in each wall-clock
// second and suppresses the rest, counting them.
//
// Keying on the *static* message rather than the formatted line is what makes this work, and it
// is why §5.1 requires messages to be constant strings with the variables in fields: a message
// built by interpolation has a different key on every call and defeats sampling entirely, in
// addition to being the place PANs end up.
//
// The epoch is recomputed from the clock on each call rather than advanced by a background
// ticker. That is deliberate: a ticker would be a goroutine per logger with a lifetime nobody
// owns, and the whole package is built on the rule that background work has an owner and a
// bounded shutdown.
func (h *samplingHandler) Handle(ctx context.Context, r slog.Record) error {
	if _, sampled := h.levels[r.Level]; !sampled {
		return h.next.Handle(ctx, r)
	}

	key := r.Level.String() + "\x1f" + r.Message
	epoch := h.now().Unix()
	st := h.state

	st.mu.Lock()
	b, ok := st.buckets[key]
	if !ok {
		if len(st.buckets) >= st.maxKeys {
			st.mu.Unlock()
			return h.next.Handle(ctx, r) // fail open: never drop what we cannot account for
		}
		b = &sampleBucket{epoch: epoch}
		st.buckets[key] = b
	}
	if b.epoch != epoch {
		b.epoch = epoch
		b.pending += b.suppressed
		b.suppressed = 0
		b.emitted = 0
	}
	if b.emitted >= h.perSecond {
		b.suppressed++
		st.mu.Unlock()
		if h.metrics != nil {
			h.metrics.RecordLogLinesSuppressed(r.Level.String(), 1)
		}
		return nil
	}
	b.emitted++
	pending := b.pending
	b.pending = 0
	st.mu.Unlock()

	if pending > 0 {
		r = r.Clone()
		r.AddAttrs(slog.Int(KeySuppressedCount, pending))
	}
	return h.next.Handle(ctx, r)
}

func (h *samplingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.derive(h.next.WithAttrs(attrs))
}

func (h *samplingHandler) WithGroup(name string) slog.Handler {
	return h.derive(h.next.WithGroup(name))
}

func (h *samplingHandler) derive(next slog.Handler) slog.Handler {
	d := *h
	d.next = next
	return &d
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: NFR-43.
//
// The mandatory telemetry context on every log line: tenant, request and trace identifiers
// bound from context, through a key allowlist that keeps unbounded fields out
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
