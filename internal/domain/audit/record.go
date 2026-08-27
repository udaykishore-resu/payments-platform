// Package audit implements the platform's hash-chained audit record.
//
// An audit log's only value is that it is believed. A log that could have been edited proves
// nothing about what happened, and "we have logs" is worth exactly as much as the weakest
// control over them — which, for an ordinary table, is whoever holds UPDATE on it. This package
// implements the chain that makes editing detectable: each record's digest covers the previous
// record's digest, so altering, removing or reordering any record invalidates every digest
// after it, and the divergence points at the first record that changed.
//
// What the chain does and does not prove:
//
//   - It proves *ordering and integrity*: this sequence of records, in this order, has not been
//     altered since the head digest was fixed.
//   - It does not prove *authorship*. Anyone able to rewrite the whole table can recompute
//     every digest. That is closed by periodic external anchoring (docs/compliance.md §7.4):
//     the chain head is signed with a KMS key and written to Object-Locked storage in a second
//     account every fifteen minutes, so rewriting history requires forging the chain *and*
//     every anchor *and* the signatures *and* the external timestamps. This package implements
//     the chain; the anchoring lives in the application layer, which is why Chain exposes its
//     head digest as a first-class value.
//
// Chains are per tenant (docs/compliance.md §7.3). A single global chain would serialize every
// audited write across every tenant onto one contended row, letting one tenant's volume delay
// another's audit commit — and since an audited action whose audit record fails to write does
// not commit, that is a tenant-visible outage caused by a neighbour.
//
// This package imports only the standard library, pkg/* and internal/domain/shared, per the
// dependency rule in docs/spec/00-design-baseline.md §4.
package audit

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
)

// DigestDomain is the domain-separation tag that opens every pre-image in this package, per the
// formula in docs/compliance.md §7.3.
//
// It carries a version suffix so that a future change to the pre-image layout produces
// unmistakably different digests rather than digests that merely fail to verify: an auditor
// looking at a divergence must be able to tell "the format changed here" from "someone edited
// this record", and an unversioned tag cannot express the difference.
const DigestDomain = "pp.audit.v1"

// ActorType classifies who performed an action.
//
// The three cases are kept distinct because they have different investigative weight. A human
// doing something unusual is a question for that human; a service doing something unusual is a
// question for a deployment; the system doing something unusual is a question for a scheduler.
// Collapsing them into "who: string" makes "show me every human action on this merchant" a
// prefix match on a naming convention, which is not a control.
type ActorType string

const (
	// ActorUser is a human principal authenticated through the identity provider.
	ActorUser ActorType = "USER"
	// ActorService is a workload principal acting under its own identity (IRSA/SPIFFE).
	ActorService ActorType = "SERVICE"
	// ActorSystem is the platform acting with no external trigger: a scheduled job, a timer, a
	// workflow continuation. It is distinct from SERVICE because there is no upstream caller to
	// hold responsible, which is exactly what an investigator needs to know.
	ActorSystem ActorType = "SYSTEM"
)

var actorTypes = map[ActorType]struct{}{ActorUser: {}, ActorService: {}, ActorSystem: {}}

// IsValid reports whether t is a known actor type.
func (t ActorType) IsValid() bool { _, ok := actorTypes[t]; return ok }

// String satisfies fmt.Stringer.
func (t ActorType) String() string { return string(t) }

// Actor identifies who acted, with the minimum needed to investigate and no more.
//
// The IP address and user agent are here because an investigation of a compromised session
// starts with "was this the device and network this principal always uses". They are not here
// for analytics, and they are subject to the same retention as the record.
//
// Note what is absent: no token, no session secret, no credential material of any kind. An
// audit record is one of the most widely-read artifacts in the platform — auditors, tenant
// admins and support all read it — and a secret that reaches it has been disclosed to all of
// them. The session and token *identifiers* are safe (they name a session without granting it)
// and belong in the correlation fields.
type Actor struct {
	Type      ActorType
	ID        string
	Name      string
	IP        string
	UserAgent string
	// OnBehalfOf is set when platform support acts for a tenant. Impersonation is always
	// visible: there is no silent impersonation anywhere in this platform, and an empty value
	// here is a positive assertion that the actor was acting as themselves.
	OnBehalfOf string
}

// Validate checks the actor is identified well enough for the record to be worth keeping.
func (a Actor) Validate() error {
	if !a.Type.IsValid() {
		return apierror.Newf(apierror.CodeValidationFailed, "unknown actor type %q", a.Type).
			WithDetail(apierror.Detail{
				Field: "actor.type", Code: "UNKNOWN_ACTOR_TYPE",
				Message: "must be USER, SERVICE or SYSTEM",
				RuleID:  "L7.AUDIT_ACTOR_IDENTIFIED",
			})
	}
	if strings.TrimSpace(a.ID) == "" {
		return apierror.New(apierror.CodeValidationFailed, "an audit record requires an identified actor").
			WithDetail(apierror.Detail{
				Field: "actor.id", Code: "MISSING_ACTOR_ID",
				Message: "an unattributed audit record answers none of the questions an audit asks",
				RuleID:  "L7.AUDIT_ACTOR_IDENTIFIED",
			})
	}
	return nil
}

// Outcome is what happened.
type Outcome string

const (
	// OutcomeSuccess means the action completed.
	OutcomeSuccess Outcome = "SUCCESS"
	// OutcomeFailure means the action was attempted and failed for a non-authorization reason.
	OutcomeFailure Outcome = "FAILURE"
	// OutcomeDenied means authorization refused the action. Denials are audited deliberately and
	// are often the most interesting records in the table: a denied merchants:terminate is a
	// better lead than most successes.
	OutcomeDenied Outcome = "DENIED"
)

var outcomes = map[Outcome]struct{}{OutcomeSuccess: {}, OutcomeFailure: {}, OutcomeDenied: {}}

// IsValid reports whether o is a known outcome.
func (o Outcome) IsValid() bool { _, ok := outcomes[o]; return ok }

// String satisfies fmt.Stringer.
func (o Outcome) String() string { return string(o) }

// Action names an auditable operation.
//
// The set is closed. An open string field would let each caller invent its own spelling
// ("merchant_suspended", "SuspendMerchant", "merchant.suspend"), and a compliance query that
// has to guess the spellings is a compliance query that misses records — which is
// indistinguishable, to an auditor, from records that were never written.
type Action string

// The auditable actions. Derived from the trigger table in docs/compliance.md §7.1 and the
// baseline's list of auditable operations.
const (
	// Merchant lifecycle. Every transition that changes whether a merchant can take money.
	ActionMerchantCreated    Action = "merchant.created"
	ActionMerchantApproved   Action = "merchant.approved"
	ActionMerchantSuspended  Action = "merchant.suspended"
	ActionMerchantTerminated Action = "merchant.terminated"

	// Control-plane changes. These change how money is routed and what it costs, without ever
	// touching a payment, which is why they are audited as heavily as payments themselves.
	ActionConfigurationChanged Action = "configuration.changed"
	ActionRoutingChanged       Action = "routing.changed"
	ActionCredentialRotated    Action = "credential.rotated"

	// Money-affecting operations.
	ActionPaymentCreated  Action = "payment.created"
	ActionPaymentCaptured Action = "payment.captured"
	ActionPaymentRefunded Action = "payment.refunded"
	ActionPaymentVoided   Action = "payment.voided"

	// Human and workflow intervention. A manual gate approval is the point at which a human
	// took responsibility for a decision the platform would not make on its own; without a
	// record naming them, the platform has taken it instead.
	ActionWorkflowSignal     Action = "workflow.signal"
	ActionManualGateApproved Action = "gate.approved"
	ActionAdminAction        Action = "admin.action"

	// Identity.
	ActionLogin            Action = "auth.login"
	ActionPermissionDenied Action = "authz.permission_denied"
)

// actionSpec records what the platform requires of each action.
type actionSpec struct {
	// requiresReason marks the dual-controlled and high-consequence actions. A change of this
	// kind without a stated reason is an audit finding on its own (docs/compliance.md §7.2), so
	// the record refuses to exist rather than being written incomplete and discovered later.
	requiresReason bool
}

var actions = map[Action]actionSpec{
	ActionMerchantCreated:      {},
	ActionMerchantApproved:     {requiresReason: true},
	ActionMerchantSuspended:    {requiresReason: true},
	ActionMerchantTerminated:   {requiresReason: true},
	ActionConfigurationChanged: {requiresReason: true},
	ActionRoutingChanged:       {requiresReason: true},
	ActionCredentialRotated:    {requiresReason: true},
	ActionPaymentCreated:       {},
	ActionPaymentCaptured:      {},
	ActionPaymentRefunded:      {},
	ActionPaymentVoided:        {},
	ActionWorkflowSignal:       {},
	ActionManualGateApproved:   {requiresReason: true},
	ActionAdminAction:          {requiresReason: true},
	ActionLogin:                {},
	ActionPermissionDenied:     {},
}

// IsValid reports whether a is a known action.
func (a Action) IsValid() bool { _, ok := actions[a]; return ok }

// String satisfies fmt.Stringer.
func (a Action) String() string { return string(a) }

// RequiresReason reports whether this action may not be recorded without a stated reason.
func (a Action) RequiresReason() bool { return actions[a].requiresReason }

// ParseAction validates a persisted or transported action name.
func ParseAction(s string) (Action, error) {
	v := Action(strings.TrimSpace(s))
	if !v.IsValid() {
		return "", apierror.Newf(apierror.CodeValidationFailed, "unknown audit action %q", s).
			WithDetail(apierror.Detail{
				Field: "action", Code: "UNKNOWN_ACTION",
				Message: "the auditable action set is closed; add the action to internal/domain/audit",
				RuleID:  "L7.AUDIT_ACTION_KNOWN",
			})
	}
	return v, nil
}

// AllActions returns every auditable action, sorted, for the query builder, the documentation
// generator and the exhaustive tests.
func AllActions() []Action {
	out := make([]Action, 0, len(actions))
	for a := range actions {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Record is one immutable, hash-chained audit entry.
//
// Every field is unexported and there is no setter. Immutability here is not a style choice: a
// record whose fields can be changed after the digest is computed is a record whose digest
// proves nothing, and the whole package exists to make the digest mean something. The chain
// sets the sequence, previous digest and digest during Append; nothing else may.
type Record struct {
	id       shared.AuditID
	tenantID shared.TenantID
	sequence uint64

	actor        Actor
	action       Action
	resourceType string
	resourceID   string
	outcome      Outcome

	// before and after are allowlisted snapshots, built by Snapshot in redact.go. They are
	// map[string]any rather than a typed struct because they describe arbitrary resources, and
	// map[string]any rather than json.RawMessage because the canonical encoder needs to sort
	// keys — and canonicalizing already-serialized JSON means parsing it back anyway.
	before map[string]any
	after  map[string]any

	reason        string
	correlationID string
	traceID       string

	occurredAt time.Time
	recordedAt time.Time

	prevDigest string
	digest     string
}

// NewRecordParams are the inputs to an audit record.
type NewRecordParams struct {
	TenantID     shared.TenantID
	Actor        Actor
	Action       Action
	ResourceType string
	ResourceID   string
	Outcome      Outcome
	Before       map[string]any
	After        map[string]any
	Reason       string
	// CorrelationID and TraceID tie the record to the request that caused it, so that an audit
	// finding can be joined to the logs, the traces and the domain events for the same
	// operation. docs/compliance.md §7.7 step 9 depends on this: two independent copies of the
	// same event, agreeing, is the strongest evidence available, and they are only joinable
	// through these.
	CorrelationID string
	TraceID       string
	// OccurredAt is when the audited thing happened. Zero means "now". It is kept separately
	// from the record time because a divergence between them measures buffering and clock skew,
	// both of which matter to an investigation.
	OccurredAt time.Time
}

// NewRecord constructs an unchained record: sequence, previous digest and digest are unset
// until the record is appended to a Chain.
//
// It validates everything that must be true of every audit record everywhere. A record that
// fails validation is not written in a degraded form — the audited action does not commit
// either, since the two share a transaction (docs/compliance.md §7.2). That is the intended
// consequence: an operation nobody can account for afterwards is one the platform declines to
// perform.
func NewRecord(p NewRecordParams, clock shared.Clock) (Record, error) {
	if p.TenantID.IsZero() {
		return Record{}, apierror.New(apierror.CodeMissingTenantContext, "an audit record requires a tenant")
	}
	if err := p.Actor.Validate(); err != nil {
		return Record{}, err
	}
	if !p.Action.IsValid() {
		return Record{}, apierror.Newf(apierror.CodeValidationFailed, "unknown audit action %q", p.Action).
			WithDetail(apierror.Detail{
				Field: "action", Code: "UNKNOWN_ACTION",
				Message: "the auditable action set is closed; add the action to internal/domain/audit",
				RuleID:  "L7.AUDIT_ACTION_KNOWN",
			})
	}
	if !p.Outcome.IsValid() {
		return Record{}, apierror.Newf(apierror.CodeValidationFailed, "unknown audit outcome %q", p.Outcome).
			WithDetail(apierror.Detail{
				Field: "outcome", Code: "UNKNOWN_OUTCOME",
				Message: "must be SUCCESS, FAILURE or DENIED",
				RuleID:  "L7.AUDIT_OUTCOME_KNOWN",
			})
	}
	if strings.TrimSpace(p.ResourceType) == "" || strings.TrimSpace(p.ResourceID) == "" {
		return Record{}, apierror.New(apierror.CodeValidationFailed,
			"an audit record requires the resource it acted on").
			WithDetail(apierror.Detail{
				Field: "resource", Code: "MISSING_RESOURCE",
				Message: "\"something was changed\" is not an audit record; name the resource",
				RuleID:  "L7.AUDIT_RESOURCE_IDENTIFIED",
			})
	}
	if p.Action.RequiresReason() && strings.TrimSpace(p.Reason) == "" {
		return Record{}, apierror.Newf(apierror.CodeValidationFailed,
			"action %s may not be recorded without a reason", p.Action).
			WithDetail(apierror.Detail{
				Field: "reason", Code: "REASON_REQUIRED",
				Message: "dual-controlled and high-consequence changes require a stated reason and a change reference",
				RuleID:  "L7.AUDIT_REASON_REQUIRED",
			})
	}

	now := clock.Now().UTC()
	occurred := p.OccurredAt
	if occurred.IsZero() {
		occurred = now
	}
	return Record{
		id:            shared.NewAuditID(),
		tenantID:      p.TenantID,
		actor:         p.Actor,
		action:        p.Action,
		resourceType:  strings.TrimSpace(p.ResourceType),
		resourceID:    strings.TrimSpace(p.ResourceID),
		outcome:       p.Outcome,
		before:        copySnapshot(p.Before),
		after:         copySnapshot(p.After),
		reason:        p.Reason,
		correlationID: p.CorrelationID,
		traceID:       p.TraceID,
		occurredAt:    occurred.UTC(),
		recordedAt:    now,
	}, nil
}

// Accessors. No setters: see the type comment.

// ID returns the record's identifier.
func (r Record) ID() shared.AuditID { return r.id }

// TenantID returns the chain this record belongs to.
func (r Record) TenantID() shared.TenantID { return r.tenantID }

// Sequence returns the record's position in its tenant's chain. It is monotonic per tenant and
// allocated in the same transaction as the record, so a gap is detectable — and a gap is a
// tamper signal until an error correlates it away.
func (r Record) Sequence() uint64 { return r.sequence }

// Actor returns who acted.
func (r Record) Actor() Actor { return r.actor }

// Action returns what was done.
func (r Record) Action() Action { return r.action }

// ResourceType returns the kind of thing acted on.
func (r Record) ResourceType() string { return r.resourceType }

// ResourceID returns which thing was acted on.
func (r Record) ResourceID() string { return r.resourceID }

// Outcome returns what happened.
func (r Record) Outcome() Outcome { return r.outcome }

// Before returns a copy of the pre-state snapshot. A copy, because handing out the live map
// would let a caller change a record after its digest was computed — which is the one thing
// this package exists to prevent.
func (r Record) Before() map[string]any { return copySnapshot(r.before) }

// After returns a copy of the post-state snapshot.
func (r Record) After() map[string]any { return copySnapshot(r.after) }

// Reason returns the stated justification.
func (r Record) Reason() string { return r.reason }

// CorrelationID returns the request correlation identifier.
func (r Record) CorrelationID() string { return r.correlationID }

// TraceID returns the distributed-trace identifier.
func (r Record) TraceID() string { return r.traceID }

// OccurredAt returns when the audited thing happened.
func (r Record) OccurredAt() time.Time { return r.occurredAt }

// RecordedAt returns when the platform wrote it down.
func (r Record) RecordedAt() time.Time { return r.recordedAt }

// PrevDigest returns the digest of the preceding record in the chain, or the genesis digest for
// the first record.
func (r Record) PrevDigest() string { return r.prevDigest }

// Digest returns this record's chain digest, or "" if the record has not been appended.
func (r Record) Digest() string { return r.digest }

// PartitionMonth returns the declarative range-partition key, derived from the immutable audit
// identifier. Deriving it from the ID rather than from a timestamp column keeps the partition a
// pure function of a value that cannot change (baseline amendment A-02) — which matters more
// here than anywhere, since the table has no UPDATE grant and a row must land in the right
// partition the first time.
func (r Record) PartitionMonth() time.Time { return ids.PartitionMonth(ids.ID(r.id)) }

// ComputeDigest returns the chain digest for this record, per docs/compliance.md §7.3:
//
//	digest[n] = SHA-256( digest[n-1] || canonical(record[n] minus {digest}) )
//
// # Why not fmt.Sprintf("%s%s%s", a, b, c)
//
// Because plain concatenation is forgeable. The pre-image "alice" + "admin" and the pre-image
// "ali" + "ceadmin" are the same bytes, so two records with different field values produce the
// same digest — and an attacker who controls any field adjacent to another can shift the
// boundary between them. Concretely: a record with actor name `j.okafor` acting on resource
// `mrc_123` and a record with actor name `j.okafor|mrc` acting on resource `_123` would hash
// identically, so the second could be substituted for the first with the chain still verifying.
// Separator characters do not fix this on their own either — any delimiter that can appear
// inside a value (and a user agent string can contain anything) reintroduces the ambiguity.
//
// What is done instead: every field is length-prefixed. The pre-image is a sequence of
// (8-byte big-endian length, bytes) frames in a fixed order, opened by a domain-separation tag.
// Because the length is fixed-width and precedes the value, the frame boundaries are determined
// by the bytes themselves and no value can be made to span one. The pre-image is therefore
// injective: two records with different field values cannot produce the same pre-image, so a
// digest collision requires a collision in SHA-256 itself.
//
// This is the same guarantee JCS canonicalization (RFC 8785) is named for in §7.3 — a
// byte-identical, unambiguous serialization — obtained directly rather than through a JSON
// round-trip. The JSON-valued fields (the before and after snapshots) are canonicalized with
// sorted keys before being framed, so the whole pre-image is deterministic. The chaining
// structure, the field set and their order are exactly as documented; the digest excludes the
// record's own digest field, which is obvious and worth stating.
func (r Record) ComputeDigest() string {
	h := sha256.New()
	writeFramed(h, []byte(DigestDomain))
	writeFramed(h, []byte(r.prevDigest))
	writeFramed(h, []byte(r.id))
	writeFramed(h, []byte(r.tenantID))
	writeFramed(h, []byte(strconv.FormatUint(r.sequence, 10)))
	writeFramed(h, []byte(r.actor.Type))
	writeFramed(h, []byte(r.actor.ID))
	writeFramed(h, []byte(r.actor.Name))
	writeFramed(h, []byte(r.actor.IP))
	writeFramed(h, []byte(r.actor.UserAgent))
	writeFramed(h, []byte(r.actor.OnBehalfOf))
	writeFramed(h, []byte(r.action))
	writeFramed(h, []byte(r.resourceType))
	writeFramed(h, []byte(r.resourceID))
	writeFramed(h, []byte(r.outcome))
	writeFramed(h, []byte(canonicalJSON(r.before)))
	writeFramed(h, []byte(canonicalJSON(r.after)))
	writeFramed(h, []byte(r.reason))
	writeFramed(h, []byte(r.correlationID))
	writeFramed(h, []byte(r.traceID))
	writeFramed(h, []byte(r.occurredAt.UTC().Format(time.RFC3339Nano)))
	writeFramed(h, []byte(r.recordedAt.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(h.Sum(nil))
}

// writeFramed appends one length-prefixed frame. The length is 8 bytes big-endian and fixed
// width: a varint would be shorter but its own encoding boundary would have to be parsed to
// find the value, and the point of the framing is that boundaries are unambiguous.
//
// hash.Hash's Write never returns an error, which is why none is checked here; that is
// documented behaviour of the interface, not an assumption about sha256.
func writeFramed(h interface{ Write([]byte) (int, error) }, b []byte) {
	var l [8]byte
	binary.BigEndian.PutUint64(l[:], uint64(len(b)))
	_, _ = h.Write(l[:])
	_, _ = h.Write(b)
}

// canonicalJSON renders a snapshot deterministically: object keys sorted, no insignificant
// whitespace, and a fixed rendering for each scalar kind.
//
// Go's map iteration order is randomized per run, so encoding/json's output for a map is not
// stable across processes — which would make a digest computed on one replica fail to verify on
// another. That is not a theoretical concern: it is a chain-wide false tamper alarm at Sev-1,
// raised by nothing worse than a rolling deploy.
func canonicalJSON(v any) string {
	var b strings.Builder
	writeCanonical(&b, v)
	return b.String()
}

func writeCanonical(b *strings.Builder, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(quoteJSON(k))
			b.WriteByte(':')
			writeCanonical(b, t[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonical(b, e)
		}
		b.WriteByte(']')
	case string:
		b.WriteString(quoteJSON(t))
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case int:
		b.WriteString(strconv.FormatInt(int64(t), 10))
	case int32:
		b.WriteString(strconv.FormatInt(int64(t), 10))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case uint64:
		b.WriteString(strconv.FormatUint(t, 10))
	case float64:
		// 'g' with -1 precision is the shortest representation that round-trips, and it is
		// deterministic for a given value on every platform Go supports.
		b.WriteString(strconv.FormatFloat(t, 'g', -1, 64))
	case time.Time:
		b.WriteString(quoteJSON(t.UTC().Format(time.RFC3339Nano)))
	default:
		// Anything else has been through Snapshot, which normalizes to the kinds above. Falling
		// back to the Go representation keeps the encoder total — a panic here would take down
		// a request path — and any value reaching this branch is a defect the tests catch.
		b.WriteString(quoteJSON(stringify(t)))
	}
}

// quoteJSON renders a JSON string literal with the minimal escaping RFC 8785 requires.
func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				const hexDigits = "0123456789abcdef"
				b.WriteByte(hexDigits[(r>>4)&0xf])
				b.WriteByte(hexDigits[r&0xf])
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func copySnapshot(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if m, ok := v.(map[string]any); ok {
			out[k] = copySnapshot(m)
			continue
		}
		if s, ok := v.([]any); ok {
			out[k] = append([]any(nil), s...)
			continue
		}
		out[k] = v
	}
	return out
}

// RehydrateRecordParams carries a persisted audit record back into the domain.
//
// The fields are unexported, so a repository cannot assemble a Record field by field — and must
// not be able to, or a read path could produce a record whose digest was never computed over
// its contents. This is the single reviewed doorway back in, and Chain.Verify is what checks
// that what came back through it is what went out.
type RehydrateRecordParams struct {
	ID            shared.AuditID
	TenantID      shared.TenantID
	Sequence      uint64
	Actor         Actor
	Action        Action
	ResourceType  string
	ResourceID    string
	Outcome       Outcome
	Before        map[string]any
	After         map[string]any
	Reason        string
	CorrelationID string
	TraceID       string
	OccurredAt    time.Time
	RecordedAt    time.Time
	PrevDigest    string
	Digest        string
}

// RehydrateRecord reconstructs a Record from persisted state. It deliberately does not verify
// the digest: verification is a chain-level property (a single record's digest is
// self-consistent by construction whoever wrote it), and Chain.Verify is where it belongs.
func RehydrateRecord(p RehydrateRecordParams) Record {
	return Record{
		id:            p.ID,
		tenantID:      p.TenantID,
		sequence:      p.Sequence,
		actor:         p.Actor,
		action:        p.Action,
		resourceType:  p.ResourceType,
		resourceID:    p.ResourceID,
		outcome:       p.Outcome,
		before:        copySnapshot(p.Before),
		after:         copySnapshot(p.After),
		reason:        p.Reason,
		correlationID: p.CorrelationID,
		traceID:       p.TraceID,
		occurredAt:    p.OccurredAt.UTC(),
		recordedAt:    p.RecordedAt.UTC(),
		prevDigest:    p.PrevDigest,
		digest:        p.Digest,
	}
}

// --- the chain ---------------------------------------------------------------------------------

// Chain is one tenant's audit sequence: an ordered set of records in which each digest covers
// the one before it.
//
// The head digest is the whole chain compressed into thirty-two bytes. Anyone who has been told
// the head at a point in time can later prove that every record up to that point is unchanged,
// which is why the head is what gets anchored, signed and published to the tenant
// (docs/compliance.md §7.4). Keeping the head as an explicit field, rather than recomputing it
// from the records on demand, is what lets the production implementation hold only the head in
// memory and stream the records from the database.
type Chain struct {
	tenantID     shared.TenantID
	genesisNonce string
	genesis      string
	head         string
	sequence     uint64
	records      []Record
}

// GenesisDigest computes the digest that opens a tenant's chain, per docs/compliance.md §7.3:
//
//	digest[0] = SHA-256( "pp.audit.v1" || tenant_id || genesis_nonce )
//
// The nonce is what stops a chain from being predictable before it has any records. Without it,
// the opening digest is a pure function of the tenant ID, so an attacker who wants to replace a
// tenant's entire history can compute the genesis themselves and forge a complete, internally
// consistent chain from nothing. With a nonce recorded at provisioning time and anchored, they
// cannot produce a chain that starts where the real one started.
//
// The three inputs are framed exactly as every other pre-image in this package: same domain
// tag, same length-prefixed framing, same reason.
func GenesisDigest(tenantID shared.TenantID, genesisNonce string) string {
	h := sha256.New()
	writeFramed(h, []byte(DigestDomain))
	writeFramed(h, []byte(tenantID))
	writeFramed(h, []byte(genesisNonce))
	return hex.EncodeToString(h.Sum(nil))
}

// NewChain opens a tenant's chain at its genesis digest.
func NewChain(tenantID shared.TenantID, genesisNonce string) (*Chain, error) {
	if tenantID.IsZero() {
		return nil, apierror.New(apierror.CodeMissingTenantContext, "an audit chain is per tenant")
	}
	if strings.TrimSpace(genesisNonce) == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"an audit chain requires a genesis nonce").
			WithDetail(apierror.Detail{
				Field: "genesisNonce", Code: "MISSING_GENESIS_NONCE",
				Message: "without a nonce the opening digest is predictable and a whole chain can be forged from nothing",
				RuleID:  "L7.AUDIT_CHAIN_GENESIS",
			})
	}
	g := GenesisDigest(tenantID, genesisNonce)
	return &Chain{
		tenantID:     tenantID,
		genesisNonce: genesisNonce,
		genesis:      g,
		head:         g,
	}, nil
}

// TenantID returns the tenant this chain belongs to.
func (c *Chain) TenantID() shared.TenantID { return c.tenantID }

// Genesis returns the opening digest.
func (c *Chain) Genesis() string { return c.genesis }

// Head returns the digest of the most recent record, or the genesis digest for an empty chain.
// This is the value that is anchored externally and published to the tenant.
func (c *Chain) Head() string { return c.head }

// Sequence returns the sequence number of the most recent record; zero for an empty chain.
func (c *Chain) Sequence() uint64 { return c.sequence }

// Len returns how many records the chain holds in memory.
func (c *Chain) Len() int { return len(c.records) }

// Records returns the chain's records in order. The slice is a copy and Record is immutable, so
// a caller cannot reach through the result to change history.
func (c *Chain) Records() []Record { return append([]Record(nil), c.records...) }

// Append stamps a record with the next sequence number and the current head, computes its
// digest, and extends the chain.
//
// The record is taken by value and its stamped copy is what the chain keeps: the caller's copy
// is not modified, so there is no way to hold a reference to a record and change it after its
// digest was computed. Read the stamped record back with Records or Last.
//
// In production this runs inside the same transaction as the audited state change, with the
// tenant's chain-head row locked FOR UPDATE. That lock is what stops the chain forking under
// concurrent writers: two records claiming the same predecessor would each verify on their own
// and the chain would have two futures, which is a silent loss of one of them.
func (c *Chain) Append(r Record) error {
	if r.tenantID != c.tenantID {
		return apierror.Newf(apierror.CodeTenantMismatch,
			"audit record for tenant %s cannot be appended to tenant %s's chain",
			r.tenantID, c.tenantID)
	}
	if r.id.String() == "" {
		return apierror.New(apierror.CodeValidationFailed, "an audit record requires an identifier")
	}
	r.sequence = c.sequence + 1
	r.prevDigest = c.head
	r.digest = r.ComputeDigest()

	c.records = append(c.records, r)
	c.sequence = r.sequence
	c.head = r.digest
	return nil
}

// Last returns the most recent record and whether there is one.
func (c *Chain) Last() (Record, bool) {
	if len(c.records) == 0 {
		return Record{}, false
	}
	return c.records[len(c.records)-1], true
}

// Verify walks the chain from genesis and reports whether it is intact.
//
// It returns (true, -1, nil) for an intact chain, and (false, i, err) naming the index of the
// first record that does not verify — the earliest point at which the stored chain and a
// recomputation disagree. The index matters as much as the boolean: the tamper runbook
// (docs/compliance.md §7.5) binary-searches from the last verified anchor to find exactly this
// number, because the range between the last good record and the first bad one is the range an
// investigation has to reconstruct, and a bare "the chain is broken" makes that the whole
// chain.
//
// Three separate things are checked per record, because they fail differently:
//
//   - the link (prevDigest equals the running head) catches a removed or reordered record;
//   - the digest (recomputation matches what is stored) catches an edited field;
//   - the sequence (monotonic, no gaps) catches a deleted record whose neighbours were relinked.
//
// A chain that passes all three has not been altered since its head was last anchored. It has
// not been shown to be *complete* — that requires the sequence-gap check against the database
// and the anchor comparison, both of which live outside the domain.
func (c *Chain) Verify() (bool, int, error) {
	head := c.genesis
	var seq uint64
	for i, r := range c.records {
		seq++
		if r.sequence != seq {
			return false, i, apierror.Newf(apierror.CodeInternalError,
				"audit chain for tenant %s: record at index %d has sequence %d, expected %d",
				c.tenantID, i, r.sequence, seq).
				WithDetail(apierror.Detail{
					Field: "sequence", Code: "AUDIT_SEQUENCE_GAP",
					Message: "a sequence gap is a deleted record until an error correlates it away",
					RuleID:  "L7.AUDIT_CHAIN_INTACT",
				})
		}
		if r.prevDigest != head {
			return false, i, apierror.Newf(apierror.CodeInternalError,
				"audit chain for tenant %s: record %s at index %d links to %s, expected %s",
				c.tenantID, r.id, i, shortDigest(r.prevDigest), shortDigest(head)).
				WithDetail(apierror.Detail{
					Field: "prevDigest", Code: "AUDIT_CHAIN_BROKEN",
					Message: "this record does not follow the one before it; a record was removed or reordered",
					RuleID:  "L7.AUDIT_CHAIN_INTACT",
				})
		}
		if want := r.ComputeDigest(); want != r.digest {
			return false, i, apierror.Newf(apierror.CodeInternalError,
				"audit chain for tenant %s: record %s at index %d has digest %s but recomputes to %s",
				c.tenantID, r.id, i, shortDigest(r.digest), shortDigest(want)).
				WithDetail(apierror.Detail{
					Field: "digest", Code: "AUDIT_RECORD_TAMPERED",
					Message: "this record's contents do not match its digest; a field was edited after it was written",
					RuleID:  "L7.AUDIT_CHAIN_INTACT",
				})
		}
		head = r.digest
	}
	if head != c.head {
		return false, len(c.records), apierror.Newf(apierror.CodeInternalError,
			"audit chain for tenant %s: head is %s but the records recompute to %s",
			c.tenantID, shortDigest(c.head), shortDigest(head)).
			WithDetail(apierror.Detail{
				Field: "head", Code: "AUDIT_HEAD_MISMATCH",
				Message: "records were removed from the end of the chain",
				RuleID:  "L7.AUDIT_CHAIN_INTACT",
			})
	}
	return true, -1, nil
}

// RehydrateChain reconstructs a chain from persisted records. It does not verify them —
// Verify is a separate, explicit call, because the read path and the integrity check have very
// different costs and a caller reading one record must not pay for a full-chain recomputation.
//
// The head is taken from the records rather than from a stored value on purpose: a stored head
// that disagreed with the records would make Verify compare a lie against a lie. The stored
// head is checked against this one by the application layer, which has both.
func RehydrateChain(tenantID shared.TenantID, genesisNonce string, records []Record) (*Chain, error) {
	c, err := NewChain(tenantID, genesisNonce)
	if err != nil {
		return nil, err
	}
	c.records = append([]Record(nil), records...)
	if n := len(c.records); n > 0 {
		c.head = c.records[n-1].digest
		c.sequence = c.records[n-1].sequence
	}
	return c, nil
}

// shortDigest renders a digest for an error message. Full digests in a message make it
// unreadable; the first and last eight hex characters are enough to tell two apart in an
// incident and the full values are in the record.
func shortDigest(d string) string {
	if len(d) <= 20 {
		return d
	}
	return d[:8] + "…" + d[len(d)-8:]
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-37, FR-88, FR-89.
//
// The hash-chained audit record and the chain verification that makes a deleted record visible
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
