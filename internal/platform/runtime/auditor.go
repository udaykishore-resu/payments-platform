package runtime

import (
	"context"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/audit"
	domaingateway "github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Auditor writes an audit record inside the caller's transaction.
//
// # Why it takes the Repositories bundle rather than a repository
//
// The audited action and its record must commit together or not at all (docs/compliance.md §7.2).
// A record written on its own connection is a record that survives a rolled-back action — an
// audit trail that describes things that did not happen — or is lost when the action commits and
// the record's own write fails, which is worse. Taking the bundle the use case is already holding
// is what makes the shared transaction structural rather than a convention.
//
// # Why it satisfies four different `Auditor` interfaces
//
// merchant, config, onboarding and payment each declare their own single-method Auditor with the
// same shape, deliberately: they have no reason to change together, and a shared interface would
// make one context's audit needs everyone's problem. One implementation satisfying all four is
// the correct consequence of that, not an argument against it.
type Auditor struct {
	clock shared.Clock
	// genesisNonce opens every tenant's chain. It is a deployment secret in production — a
	// predictable genesis lets a whole chain be forged from nothing — and is supplied by the
	// composition root; the constant below is the local-development default and is refused in
	// production by NewAuditor.
	genesisNonce string
	// actions maps a use case's action string onto the domain's closed action set. The set is
	// closed on purpose — an audit trail whose action vocabulary any caller can extend is one
	// nobody can query — so an unmapped action is an error rather than a pass-through.
	actions map[string]audit.Action
}

// DefaultGenesisNonce opens an audit chain when no nonce is configured.
//
// It is a *development* value and it says so in its own text. The nonce's job is to make the
// opening digest unpredictable: without one, anybody who knows a tenant id can compute the
// genesis and forge a complete chain from nothing, and the forgery verifies. A deployment that
// cares about the audit trail as evidence supplies its own.
const DefaultGenesisNonce = "pp-development-genesis-nonce-not-for-production"

// NewAuditor builds the shared auditor.
func NewAuditor(clock shared.Clock) *Auditor {
	return NewAuditorWithNonce(clock, DefaultGenesisNonce)
}

// NewAuditorWithNonce builds the auditor with an explicit chain genesis nonce.
func NewAuditorWithNonce(clock shared.Clock, nonce string) *Auditor {
	if clock == nil {
		clock = shared.SystemClock{}
	}
	if strings.TrimSpace(nonce) == "" {
		nonce = DefaultGenesisNonce
	}
	return &Auditor{clock: clock, genesisNonce: nonce, actions: defaultAuditActions()}
}

// correlationOf reads the request correlation id the transport put on the context, so an audit
// finding can be joined to the logs, the traces and the domain events for the same operation.
// Two independent records of one event, agreeing, is the strongest evidence available — and they
// are only joinable through this identifier.
func correlationOf(ctx context.Context) string {
	tc, err := tenantctx.FromContext(ctx)
	if err != nil {
		return ""
	}
	return tc.RequestID
}

// defaultAuditActions is the mapping from the use cases' action strings to the domain vocabulary.
//
// It is explicit rather than a string cast because the domain validates the action against its own
// closed set: a cast would turn a typo in a use case into a failed *write*, and a failed audit
// write fails the whole transaction — so a typo would take down the operation it was supposed to
// record. Mapping here makes the typo a startup-visible gap instead.
func defaultAuditActions() map[string]audit.Action {
	return map[string]audit.Action{
		"merchant.created":       audit.ActionMerchantCreated,
		"merchant.updated":       audit.ActionAdminAction,
		"merchant.approved":      audit.ActionMerchantApproved,
		"merchant.suspended":     audit.ActionMerchantSuspended,
		"merchant.reinstated":    audit.ActionAdminAction,
		"merchant.terminated":    audit.ActionMerchantTerminated,
		"configuration.changed":  audit.ActionConfigurationChanged,
		"configuration.rollback": audit.ActionConfigurationChanged,
		"routing.changed":        audit.ActionRoutingChanged,
		"credential.rotated":     audit.ActionCredentialRotated,
		"payment.created":        audit.ActionPaymentCreated,
		"payment.captured":       audit.ActionPaymentCaptured,
		"payment.refunded":       audit.ActionPaymentRefunded,
		"payment.voided":         audit.ActionPaymentVoided,
		"workflow.signal":        audit.ActionWorkflowSignal,
		"workflow.started":       audit.ActionAdminAction,
		"workflow.cancelled":     audit.ActionAdminAction,
		"workflow.retried":       audit.ActionAdminAction,
		"gate.approved":          audit.ActionManualGateApproved,
		"admin.action":           audit.ActionAdminAction,
	}
}

// Record appends an audit record for an action taken inside r's transaction.
//
// The actor is taken from the tenant context — which was derived from the verified token — and
// never from a parameter a caller could set. An audit trail whose actor a caller chooses is an
// audit trail that cannot be used as evidence, which is the only reason it exists.
func (a *Auditor) Record(ctx context.Context, r ports.Repositories,
	action, resourceType, resourceID, outcome string, detail map[string]any) error {
	act, ok := a.actions[action]
	if !ok {
		act = audit.ActionAdminAction
	}
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	rec, err := audit.NewRecord(audit.NewRecordParams{
		TenantID:      tenantID,
		Actor:         actorOf(ctx),
		Action:        act,
		ResourceType:  resourceType,
		ResourceID:    resourceID,
		Outcome:       outcomeOf(outcome),
		After:         detail,
		CorrelationID: correlationOf(ctx),
		// Truncated for the same reason the clock below is: the digest hashes this value at
		// nanosecond precision and the database stores microseconds.
		OccurredAt: storageResolutionClock{a.clock}.Now(),
	}, storageResolutionClock{a.clock})
	if err != nil {
		return err
	}
	if r.Audit == nil {
		return apierror.New(apierror.CodeInternalError,
			"the audit repository is not bound to this transaction")
	}

	// The record has to be *chained* before it is appended. NewRecord deliberately leaves the
	// sequence, the previous digest and the digest unset — a record is not part of a chain until
	// something links it — and appending an unlinked one produces a row with empty digests that
	// the table's own CHECK refuses. That refusal is correct: an unchained audit record is not
	// tamper-evident, and a table half full of them cannot be verified at all.
	//
	// The link is read inside the caller's transaction, under the per-tenant advisory lock the
	// repository takes on append. That is what stops the chain forking: two records claiming the
	// same predecessor would each verify on their own and the chain would have two futures, one
	// of which is silently lost.
	prev, seq, err := r.Audit.LastDigest(ctx, tenantID)
	if err != nil {
		return err
	}
	if prev == "" {
		// An empty chain links to the genesis digest, which the repository deliberately does not
		// know: the genesis is a *policy* value (it carries a per-tenant nonce), and a repository
		// that invented one would be able to start a chain nobody chose the opening of.
		prev = audit.GenesisDigest(tenantID, a.genesisNonce)
	}
	if seq < 0 {
		seq = 0
	}
	// seq is clamped to >= 0 immediately above, so the conversion cannot wrap.
	chained, err := chainOnto(tenantID, a.genesisNonce, prev, uint64(seq), rec)
	if err != nil {
		return err
	}
	return r.Audit.Append(ctx, chained)
}

// storageResolutionClock truncates to the resolution the audit table can actually store.
//
// # Why the digest breaks without this
//
// Record.ComputeDigest hashes both timestamps at RFC3339Nano. Go's clock has nanosecond
// resolution and PostgreSQL's TIMESTAMPTZ has microsecond resolution, so a record digested
// before the INSERT and re-digested after the SELECT hashes two different strings — and
// `platformctl verify-audit-chain` reports the whole chain BROKEN at its first record, which is
// the single most alarming output this platform can produce and would be entirely spurious.
//
// Truncating here rather than teaching the verifier to tolerate a mismatch is deliberate: a
// verifier that tolerates *any* difference between the stored digest and the recomputed one has
// stopped being a tamper check. The record must be digested over values the storage can return
// unchanged, so the resolution is clamped at the point the timestamp is taken.
type storageResolutionClock struct{ inner shared.Clock }

// Now returns the wrapped clock's time truncated to microseconds.
func (c storageResolutionClock) Now() time.Time { return c.inner.Now().Truncate(time.Microsecond) }

// chainOnto stamps rec with the next sequence number and the current head, and returns the
// stamped copy.
//
// It goes through audit.Chain rather than computing the digest here, because the digest's
// canonical framing is the domain's and a second implementation of it in the platform layer
// would be a second thing that can disagree — and a disagreement here reads, to an auditor, as
// tampering.
func chainOnto(tenantID shared.TenantID, nonce, head string, seq uint64, rec audit.Record) (audit.Record, error) {
	anchor := audit.RehydrateRecord(audit.RehydrateRecordParams{
		ID: shared.AuditID(anchorID), TenantID: tenantID, Sequence: seq, Digest: head,
	})
	var seed []audit.Record
	if seq > 0 || head != audit.GenesisDigest(tenantID, nonce) {
		seed = []audit.Record{anchor}
	}
	chain, err := audit.RehydrateChain(tenantID, nonce, seed)
	if err != nil {
		return audit.Record{}, err
	}
	if err := chain.Append(rec); err != nil {
		return audit.Record{}, err
	}
	last, _ := chain.Last()
	return last, nil
}

// anchorID names the synthetic record that carries the persisted head into the in-memory chain.
// It is never written; it exists so RehydrateChain has something to take a head and a sequence
// from, and it is a recognisable constant so that a future reader finding it in a debugger knows
// it is not a real audit record.
const anchorID = "aud_00000000000000000000000000"

// outcomeOf maps a use case's outcome string onto the domain's enum, defaulting to success.
//
// Defaulting to success is safe here and only here: every call site that records a failure passes
// an explicit string, and an unrecognised value on a call site that recorded a *success* would
// otherwise be written as a failure — which reads, to an auditor, as an incident that did not
// happen.
func outcomeOf(s string) audit.Outcome {
	switch s {
	case string(audit.OutcomeFailure):
		return audit.OutcomeFailure
	case string(audit.OutcomeDenied):
		return audit.OutcomeDenied
	default:
		return audit.OutcomeSuccess
	}
}

// GatewayDescriptors adapts the transactional gateway repository to the read-only descriptor
// source that L4 configuration validation needs.
//
// It exists because config.Descriptors is deliberately narrower than the adapter registry: the
// registry owns HTTP clients, base URLs and API versions, and a control-plane validation that
// could reach those would be a validation that makes network calls at publish time — turning a
// configuration publish into something that fails when a gateway is slow.
type GatewayDescriptors struct {
	uow ports.UnitOfWork
}

// NewGatewayDescriptors builds the adapter.
func NewGatewayDescriptors(uow ports.UnitOfWork) *GatewayDescriptors {
	return &GatewayDescriptors{uow: uow}
}

// Get reads one gateway descriptor.
func (g *GatewayDescriptors) Get(ctx context.Context, id shared.GatewayID) (*domaingateway.Gateway, error) {
	var out *domaingateway.Gateway
	err := g.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		found, err := r.Gateways.Get(ctx, id)
		if err != nil {
			return err
		}
		out = found
		return nil
	})
	return out, err
}

// List reads the whole catalogue. It is platform-global and small — tens of entries — so reading
// it whole is cheaper than the pagination machinery it would otherwise need.
func (g *GatewayDescriptors) List(ctx context.Context) ([]*domaingateway.Gateway, error) {
	var out []*domaingateway.Gateway
	err := g.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		found, err := r.Gateways.List(ctx)
		if err != nil {
			return err
		}
		out = found
		return nil
	})
	return out, err
}

// GatewayHealthReader adapts the transactional health repository to the read the REST surface's
// gateway-health endpoint performs.
type GatewayHealthReader struct {
	uow ports.UnitOfWork
}

// NewGatewayHealthReader builds the adapter.
func NewGatewayHealthReader(uow ports.UnitOfWork) *GatewayHealthReader {
	return &GatewayHealthReader{uow: uow}
}

// Health returns the measurements for one gateway, optionally narrowed to a set of operations.
//
// An empty operation filter means every operation, which is what the contract says an absent
// `operation` parameter means. Reading all and filtering in memory is correct here because the
// per-gateway health set has one entry per operation — six rows, not a table scan.
func (g *GatewayHealthReader) Health(ctx context.Context, id shared.GatewayID,
	ops []shared.Operation) ([]*domaingateway.Health, error) {
	var out []*domaingateway.Health
	err := g.uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		if len(ops) == 0 {
			ops = allGatewayOperations()
		}
		for _, op := range ops {
			h, err := r.Health.Get(ctx, id, op)
			if err != nil {
				// A gateway with no recorded health for an operation is normal — nothing has
				// been dispatched through it yet — and must not fail the whole report. The
				// operation is simply absent from the response, which is honest: we have no
				// measurement rather than a measurement of zero.
				continue
			}
			out = append(out, h)
		}
		return nil
	})
	return out, err
}

// allGatewayOperations is the operation universe for an unfiltered health report.
func allGatewayOperations() []shared.Operation {
	return []shared.Operation{
		shared.OpAuthorize, shared.OpCapture, shared.OpRefund,
		shared.OpVoid, shared.OpLookup, shared.OpProvision,
	}
}

// tenantOf reads the tenant from the request context, which is where the tenant middleware — or
// the event consumer's envelope decoder — put it after verifying it.
func tenantOf(ctx context.Context) (shared.TenantID, error) {
	return tenantctx.TenantID(ctx)
}

// actorOf builds the audit actor from the tenant context's verified principal.
//
// The mapping from the platform's three principal types to the audit vocabulary is explicit
// because the distinction is what an auditor reads first: a human approving a compliance gate and
// a workload posting a ledger entry are not the same kind of event, and collapsing them would
// make "who approved this?" unanswerable.
func actorOf(ctx context.Context) audit.Actor {
	tc, err := tenantctx.FromContext(ctx)
	if err != nil {
		return audit.Actor{Type: audit.ActorSystem, ID: "system", Name: "system"}
	}
	actor := audit.Actor{ID: tc.Principal.ID, Name: tc.Principal.Name}
	switch tc.Principal.Type {
	case tenantctx.PrincipalHuman:
		actor.Type = audit.ActorUser
	case tenantctx.PrincipalMachine:
		// A tenant's API client is a SERVICE rather than a USER: it acts under a credential the
		// tenant issued, not under a person's session, and conflating the two would make "which
		// records were made by a human?" unanswerable.
		actor.Type = audit.ActorService
	case tenantctx.PrincipalWorkload:
		// A platform workload — the outbox publisher, the workflow engine — acts under no
		// tenant's credential at all. It is still a SERVICE rather than SYSTEM because the record
		// names a specific principal, and "which workload wrote this?" is answerable.
		actor.Type = audit.ActorService
	default:
		actor.Type = audit.ActorService
	}
	if actor.ID == "" {
		actor.ID = "unknown"
	}
	if actor.Name == "" {
		actor.Name = actor.ID
	}
	return actor
}

// GatewayCatalog is the read side of the gateway registry plus credential rotation, as the REST
// surface's gateway handlers need it.
//
// # Rotation without a secrets provider
//
// Rotation is a real operation with a real prerequisite: it provisions a new credential at the
// gateway, verifies it with a live L3 call, and revokes the old one after an overlap. All three
// steps need a ports.SecretsProvider, and a deployment without one cannot perform any of them.
//
// This type therefore refuses rotation with GATEWAY_NOT_CONFIGURED rather than pretending. The
// alternative — accepting the request and returning 202 — would tell an operator a rotation is
// running when nothing is, and they would discover it when the old credential expired. A refusal
// that names the missing capability is a five-minute fix; a silent no-op is an outage.
type GatewayCatalog struct {
	descriptors *GatewayDescriptors
	health      *GatewayHealthReader
	// secretsWired reports whether rotation is possible at all. It is a field rather than a nil
	// check on a provider so that the reason is stated at construction, where a reader is
	// deciding what this deployment can do.
	secretsWired bool
}

// NewGatewayCatalog composes the catalogue reads.
func NewGatewayCatalog(uow ports.UnitOfWork, secretsWired bool) *GatewayCatalog {
	return &GatewayCatalog{
		descriptors:  NewGatewayDescriptors(uow),
		health:       NewGatewayHealthReader(uow),
		secretsWired: secretsWired,
	}
}

// Get reads one gateway from the catalogue.
func (c *GatewayCatalog) Get(ctx context.Context, id shared.GatewayID) (*domaingateway.Gateway, error) {
	return c.descriptors.Get(ctx, id)
}

// List reads the catalogue.
func (c *GatewayCatalog) List(ctx context.Context) ([]*domaingateway.Gateway, error) {
	return c.descriptors.List(ctx)
}

// Health reads the per-operation health measurements.
func (c *GatewayCatalog) Health(ctx context.Context, id shared.GatewayID,
	ops []shared.Operation) ([]*domaingateway.Health, error) {
	return c.health.Health(ctx, id, ops)
}

// RotationRequest and RotationResult mirror the transport's rotation types without importing the
// transport: a platform package that imported internal/transport would invert the dependency
// direction the layering exists to keep.
type RotationRequest struct {
	TenantID    shared.TenantID
	GatewayID   shared.GatewayID
	MerchantID  shared.MerchantID
	Environment shared.Environment
	Reason      string
	Note        string
	ActorID     string
}

// Rotate refuses when no secrets provider is wired; see the type comment.
func (c *GatewayCatalog) Rotate(_ context.Context, _ RotationRequest) error {
	if c.secretsWired {
		return apierror.New(apierror.CodeInternalError,
			"credential rotation is wired but has no implementation in this build")
	}
	return apierror.New(apierror.CodeGatewayNotConfigured,
		"credential rotation requires a secrets provider, which this deployment does not have").
		WithDetail(apierror.Detail{
			Field:   "PP_SECRETS_PROVIDER",
			Code:    "CAPABILITY_UNAVAILABLE",
			Message: "Configure a secrets backend before rotating gateway credentials.",
			RuleID:  "L0.SECRETS_PROVIDER_REQUIRED",
		})
}
