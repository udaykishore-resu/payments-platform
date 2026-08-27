// Package config holds the merchant-configuration use cases (BC-5): the control-plane operations
// that read, publish and roll back the versioned document the data plane enforces.
//
// Three properties define this package, and all three follow from the same premise — that a
// configuration version is evidence, not a settings row:
//
//   - **Append-only.** Nothing is updated in place and nothing is deleted. A rollback publishes
//     the old document as a *new* version, so "what was the routing on 3 March" always has an
//     answer and "who rolled it back, and when" has one too.
//   - **Validated as a whole, once, at publish time.** Configuration defects are usually
//     *combinations* — a currency enabled with no gateway that supports it, a 3DS threshold above
//     the transaction ceiling — and per-field validation cannot see them. That is L4, and running
//     it here is what lets the payment hot path intersect bitsets instead of re-deriving the
//     truth on every authorization.
//   - **Concurrency is explicit.** Publishing takes an If-Match precondition. Two operators
//     editing one merchant's routing is not exotic, and a silent last-writer-wins loses a change
//     that nobody can afterwards find.
package config

import (
	"encoding/json"
	"sort"
	"time"

	"context"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/config"
	"github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// EventConfigurationPublished is the event type a publish emits. It is a compacted topic keyed by
// merchant: the payload is the full document rather than a diff, so a data-plane replica warming
// from the log needs one message per merchant rather than the whole history.
const (
	EventConfigurationPublished  = "configuration.published.v1"
	EventConfigurationRolledBack = "configuration.rolled_back.v1"
	topicConfiguration           = "pp.config.configuration.v1"
)

// Auditor records an auditable action inside the caller's transaction.
type Auditor interface {
	Record(ctx context.Context, r ports.Repositories, action, resourceType, resourceID, outcome string, detail map[string]any) error
}

// Descriptors is the read side of the gateway registry that L4 validation needs.
//
// Declared here, narrow, rather than taken from the adapter registry: the registry owns HTTP
// clients, base URLs and API versions, and a control-plane validation that could reach them would
// be a validation that makes network calls at publish time.
type Descriptors interface {
	Get(ctx context.Context, g shared.GatewayID) (*gateway.Gateway, error)
	List(ctx context.Context) ([]*gateway.Gateway, error)
}

// Deps is the configuration service's dependency set.
type Deps struct {
	UoW   ports.UnitOfWork
	Audit Auditor
	Clock shared.Clock
	// Gateways backs the CapabilityLookup the domain's own L4 checks consult.
	Gateways Descriptors
}

// Service is the configuration use-case facade.
type Service struct {
	deps Deps
}

// NewService constructs the service.
func NewService(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = shared.SystemClock{}
	}
	return &Service{deps: d}
}

// CapabilityLookup is the gateway-registry-backed implementation of the narrow interface
// `config.MerchantConfig.Validate` needs.
//
// It exists so that the two cross-field checks that actually catch production defects can run at
// publish time: that the merchant's refund window is within what every routed gateway will
// honour, and that every enabled (currency, method) pair is servable by at least one gateway in
// the routing policy. Both are invisible to a field-by-field validator and both surface, without
// it, as a failed payment at three in the morning.
//
// It is a request-scoped value rather than a long-lived service because it holds a context: the
// domain's interface is synchronous and context-free by design (a validation rule that could
// block on I/O is a validation rule with no budget), so the context is captured here, at the one
// call site that knows the request's deadline.
type CapabilityLookup struct {
	ctx        context.Context
	source     Descriptors
	cache      map[shared.GatewayID]*gateway.Gateway
	missing    map[shared.GatewayID]bool
	fetchError bool
}

// NewCapabilityLookup binds a lookup to one request.
func NewCapabilityLookup(ctx context.Context, src Descriptors) *CapabilityLookup {
	return &CapabilityLookup{
		ctx: ctx, source: src,
		cache:   map[shared.GatewayID]*gateway.Gateway{},
		missing: map[shared.GatewayID]bool{},
	}
}

func (l *CapabilityLookup) get(g shared.GatewayID) *gateway.Gateway {
	if got, ok := l.cache[g]; ok {
		return got
	}
	if l.missing[g] || l.source == nil {
		return nil
	}
	desc, err := l.source.Get(l.ctx, g)
	if err != nil || desc == nil {
		l.missing[g] = true
		l.fetchError = true
		return nil
	}
	l.cache[g] = desc
	return desc
}

// CanRefundAfter reports whether the gateway will still accept a refund d after capture.
//
// A gateway whose descriptor cannot be read answers false. That is the fail-closed direction and
// the correct one: promising a 365-day refund window on a gateway nobody can describe produces a
// failure at the worst possible moment — when a customer is owed money.
func (l *CapabilityLookup) CanRefundAfter(g shared.GatewayID, d time.Duration) bool {
	desc := l.get(g)
	if desc == nil {
		return false
	}
	return desc.CanRefundAfter(d)
}

// AnySupports reports whether any of the named gateways can serve the (currency, method) pair.
func (l *CapabilityLookup) AnySupports(gs []shared.GatewayID, c money.Currency, m shared.PaymentMethod) bool {
	for _, g := range gs {
		desc := l.get(g)
		if desc == nil {
			continue
		}
		caps := desc.Capabilities()
		if containsCurrency(caps.Currencies, c) && containsMethod(caps.Methods, m) {
			return true
		}
	}
	return false
}

// GetActive returns the version currently in force.
func (s *Service) GetActive(ctx context.Context, tenantID shared.TenantID, m shared.MerchantID) (*config.MerchantConfig, error) {
	if err := assertTenant(tenantID); err != nil {
		return nil, err
	}
	var out *config.MerchantConfig
	err := s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		c, err := r.Configs.GetActive(ctx, m)
		if err != nil {
			return err
		}
		if err := assertOwner(tenantID, c); err != nil {
			return err
		}
		out = c
		return nil
	})
	return out, err
}

// GetVersion returns one historical version.
//
// Every version stays readable forever, which is the point of the append-only store: a routing
// dispute six months later is decidable only if the document that produced the decision still
// exists in the form it had at the time.
func (s *Service) GetVersion(ctx context.Context, tenantID shared.TenantID, m shared.MerchantID, version int) (*config.MerchantConfig, error) {
	if err := assertTenant(tenantID); err != nil {
		return nil, err
	}
	var out *config.MerchantConfig
	err := s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		c, err := r.Configs.GetVersion(ctx, m, version)
		if err != nil {
			return err
		}
		if err := assertOwner(tenantID, c); err != nil {
			return err
		}
		out = c
		return nil
	})
	return out, err
}

// ListVersions returns the history, newest first.
func (s *Service) ListVersions(ctx context.Context, tenantID shared.TenantID, m shared.MerchantID, page ports.Page) ([]*config.MerchantConfig, string, error) {
	if err := assertTenant(tenantID); err != nil {
		return nil, "", err
	}
	var (
		out    []*config.MerchantConfig
		cursor string
	)
	err := s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		all, c, err := r.Configs.ListVersions(ctx, m, page)
		if err != nil {
			return err
		}
		cursor = c
		for _, v := range all {
			if v.TenantID == tenantID || v.TenantID.IsZero() {
				out = append(out, v)
			}
		}
		return nil
	})
	return out, cursor, err
}

// PublishCommand publishes a new configuration version.
type PublishCommand struct {
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	// Draft is the document to publish. Its Version is ignored: the next version is derived from
	// the store, never from the caller, because a caller-chosen version is a caller-chosen
	// overwrite.
	Draft *config.MerchantConfig
	// IfMatch is the ETag of the version the caller read. Empty is refused on a merchant that
	// already has a configuration; the very first publish has nothing to match against.
	IfMatch string
	// Comment is the author's reason for the change, required by the domain. A configuration
	// history with no reasons is a list of diffs nobody can interpret six months later.
	Comment string
	Actor   Actor
}

// Actor identifies who published, for the audit record.
type Actor struct {
	ID   string
	Name string
}

// Publish validates, versions, persists, audits and announces a configuration change.
//
// The order is load-bearing and is the same order every mutating use case in this platform uses:
//
//	L4 validation → the document is internally consistent and servable
//	next version  → derived from the store, under the If-Match precondition
//	persist       → append-only
//	audit         → with the diff, in the same transaction
//	outbox        → the event, in the same transaction
//
// The last three sharing one transaction is what stops a configuration existing that no consumer
// was told about, and an announcement of a change that did not commit.
func (s *Service) Publish(ctx context.Context, cmd PublishCommand) (*config.MerchantConfig, error) {
	if err := assertTenant(cmd.TenantID); err != nil {
		return nil, err
	}
	if cmd.Draft == nil {
		return nil, apierror.New(apierror.CodeValidationFailed, "a configuration document is required")
	}
	if cmd.Comment == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"a change comment is required when publishing configuration").
			WithDetail(apierror.Detail{
				Field: "comment", Code: "REQUIRED",
				Message: "describe why this change is being made; the history is unreadable without it",
				RuleID:  "L4.PUBLISH_HAS_COMMENT",
			})
	}

	draft := *cmd.Draft
	draft.MerchantID = cmd.MerchantID
	draft.TenantID = cmd.TenantID

	// L4 runs against the *draft*, before a version number is allocated. Validating the versioned
	// copy would mean a rejected publish had already consumed a version number, and a gap in the
	// version sequence is indistinguishable from a deleted version.
	if err := draft.Validate(NewCapabilityLookup(ctx, s.deps.Gateways)); err != nil {
		return nil, err
	}

	var out *config.MerchantConfig
	err := s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		current, err := r.Configs.GetActive(ctx, cmd.MerchantID)
		if err != nil {
			current = nil
		}
		if err := checkPrecondition(current, cmd.IfMatch); err != nil {
			return err
		}

		base := &draft
		if current != nil {
			// Publish is called on the *current* version so that the new one links to it: the
			// chain is what a rollback walks, and a version with no predecessor is a history that
			// starts in the middle.
			base = current
		}
		next, err := base.Publish(cmd.Actor.ID, cmd.Comment, s.deps.Clock.Now())
		if err != nil {
			return err
		}
		// The content is the draft's; only the version metadata comes from the chain.
		content := draft
		content.Version = next.Version
		content.PreviousVersion = next.PreviousVersion
		content.Status = config.StatusActive
		content.CreatedAt = next.CreatedAt
		content.CreatedBy = next.CreatedBy
		content.PublishedAt = next.PublishedAt
		content.Comment = cmd.Comment

		expected := 0
		if current != nil {
			expected = current.Version
		}
		if err := r.Configs.Publish(ctx, &content, expected); err != nil {
			return err
		}
		out = &content

		diff := Diff(current, &content)
		if err := s.audit(ctx, r, cmd.Actor, "configuration.published", &content, map[string]any{
			"version":         content.Version,
			"previousVersion": content.PreviousVersion,
			"comment":         cmd.Comment,
			"patch":           diff,
		}); err != nil {
			return err
		}
		return s.announce(ctx, r, EventConfigurationPublished, &content)
	})
	return out, err
}

// RollbackCommand republishes an earlier version.
type RollbackCommand struct {
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	// ToVersion is the version whose content to restore.
	ToVersion int
	IfMatch   string
	Actor     Actor
}

// Rollback republishes an earlier version's content as a new version.
//
// Forward-only, never a deletion, and the two reasons are independent:
//
//   - The audit trail must show that a rollback happened and who did it. Deleting the bad version
//     erases the fact that anybody ever published it, which is the single most interesting thing
//     about the incident.
//   - The data plane caches snapshots by version number. Deleting a version leaves every replica
//     pointing at a version that no longer exists, and the failure surfaces as a stale snapshot
//     that never refreshes.
func (s *Service) Rollback(ctx context.Context, cmd RollbackCommand) (*config.MerchantConfig, error) {
	if err := assertTenant(cmd.TenantID); err != nil {
		return nil, err
	}
	var out *config.MerchantConfig
	err := s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		current, err := r.Configs.GetActive(ctx, cmd.MerchantID)
		if err != nil {
			return err
		}
		if err := assertOwner(cmd.TenantID, current); err != nil {
			return err
		}
		if err := checkPrecondition(current, cmd.IfMatch); err != nil {
			return err
		}
		target, err := r.Configs.GetVersion(ctx, cmd.MerchantID, cmd.ToVersion)
		if err != nil {
			return err
		}
		if err := assertOwner(cmd.TenantID, target); err != nil {
			return err
		}

		next, err := current.RollbackTo(target, cmd.Actor.ID, s.deps.Clock.Now())
		if err != nil {
			return err
		}
		if err := r.Configs.Publish(ctx, next, current.Version); err != nil {
			return err
		}
		out = next

		if err := s.audit(ctx, r, cmd.Actor, "configuration.rolled_back", next, map[string]any{
			"version":         next.Version,
			"restoredFrom":    target.Version,
			"previousVersion": next.PreviousVersion,
			"patch":           Diff(current, next),
		}); err != nil {
			return err
		}
		return s.announce(ctx, r, EventConfigurationRolledBack, next)
	})
	return out, err
}

// announce writes the outbox row, inside the caller's transaction.
func (s *Service) announce(ctx context.Context, r ports.Repositories, eventType string, c *config.MerchantConfig) error {
	payload, err := json.Marshal(map[string]any{
		"merchantId":      c.MerchantID.String(),
		"version":         c.Version,
		"previousVersion": c.PreviousVersion,
		"environment":     string(c.Environment),
		"comment":         c.Comment,
	})
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "the configuration event does not encode")
	}
	now := s.deps.Clock.Now()
	return r.Outbox.Append(ctx, ports.OutboxMessage{
		ID:            shared.EventID(ids.NewAt(ids.PrefixEvent, now)),
		TenantID:      c.TenantID,
		Topic:         topicConfiguration,
		Type:          eventType,
		AggregateID:   c.MerchantID.String(),
		AggregateType: "configuration",
		// Keyed by merchant so that a merchant's configuration events are strictly ordered
		// relative to one another, which is the only ordering a consumer may rely on.
		PartitionKey: c.MerchantID.String(),
		Payload:      payload,
		OccurredAt:   now,
		AvailableAt:  now,
	})
}

func (s *Service) audit(ctx context.Context, r ports.Repositories, actor Actor,
	action string, c *config.MerchantConfig, detail map[string]any) error {

	if s.deps.Audit == nil {
		return nil
	}
	detail["actorId"] = actor.ID
	detail["actorName"] = actor.Name
	return s.deps.Audit.Record(ctx, r, action, "configuration", c.MerchantID.String(), "SUCCESS", detail)
}

// PatchOp is one JSON-Patch-shaped operation in a configuration diff.
//
// JSON Patch rather than a prose summary or a pair of full documents, for three reasons that all
// bite in practice: it is diffable by a human reading an audit record, it is small enough to
// store on every publish forever, and it is mechanically comparable — "which publishes touched
// routing.primary" is a query rather than an archaeology project.
type PatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// Diff computes the JSON-Patch-shaped difference between two configuration versions.
//
// A nil `from` produces an `add` for every field, which is the honest description of a first
// publish: nothing existed, and now this does.
func Diff(from, to *config.MerchantConfig) []PatchOp {
	before := flatten(from)
	after := flatten(to)

	keys := make([]string, 0, len(before)+len(after))
	seen := map[string]struct{}{}
	for k := range before {
		if _, ok := seen[k]; !ok {
			keys = append(keys, k)
			seen[k] = struct{}{}
		}
	}
	for k := range after {
		if _, ok := seen[k]; !ok {
			keys = append(keys, k)
			seen[k] = struct{}{}
		}
	}
	// Sorted so that two runs over the same pair of documents produce byte-identical patches. An
	// audit record whose content depends on map iteration order cannot be compared with itself.
	sort.Strings(keys)

	var out []PatchOp
	for _, k := range keys {
		b, hadBefore := before[k]
		a, hasAfter := after[k]
		switch {
		case !hadBefore && hasAfter:
			out = append(out, PatchOp{Op: "add", Path: k, Value: a})
		case hadBefore && !hasAfter:
			out = append(out, PatchOp{Op: "remove", Path: k})
		case b != a:
			out = append(out, PatchOp{Op: "replace", Path: k, Value: a})
		}
	}
	return out
}

// flatten renders a configuration as JSON-pointer paths to scalar values.
//
// Scalars rather than nested objects because a diff over nested values reports "routing changed",
// which is exactly as useful as reporting nothing. The path is what an operator greps for.
func flatten(c *config.MerchantConfig) map[string]string {
	out := map[string]string{}
	if c == nil {
		return out
	}
	raw, err := json.Marshal(struct {
		Environment    shared.Environment      `json:"environment"`
		Currencies     []money.Currency        `json:"supportedCurrencies"`
		PaymentMethods []shared.PaymentMethod  `json:"paymentMethods"`
		Countries      []shared.Country        `json:"countries"`
		Routing        any                     `json:"routing"`
		Risk           any                     `json:"risk"`
		Limits         config.Limits           `json:"limits"`
		Webhook        config.WebhookConfig    `json:"webhooks"`
		Settle         config.SettlementConfig `json:"settlement"`
		FeatureFlags   map[string]bool         `json:"featureFlags"`
	}{
		Environment: c.Environment, Currencies: c.SupportedCurrencies,
		PaymentMethods: c.PaymentMethods, Countries: c.Countries,
		Routing: c.Routing, Risk: c.Risk, Limits: c.Limits,
		Webhook: c.Webhook, Settle: c.Settle, FeatureFlags: c.FeatureFlags,
	})
	if err != nil {
		return out
	}
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return out
	}
	walk("", tree, out)
	return out
}

func walk(prefix string, v any, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walk(prefix+"/"+k, t[k], out)
		}
	case []any:
		// A list is rendered as one scalar rather than per index. An operator who reordered a
		// fallback chain wants to see "the chain changed", not five index-level replacements that
		// have to be mentally recombined.
		b, _ := json.Marshal(t)
		out[prefix] = string(b)
	case nil:
		// Absent, not present-and-null: an explicit null and a missing field mean the same thing
		// in this document and recording both would produce phantom diffs.
	default:
		b, _ := json.Marshal(t)
		out[prefix] = string(b)
	}
}

// checkPrecondition enforces If-Match.
func checkPrecondition(current *config.MerchantConfig, ifMatch string) error {
	if current == nil {
		// The first publish for a merchant has nothing to match against. Requiring a token here
		// would make bootstrapping impossible, and there is no concurrent edit to lose.
		return nil
	}
	if ifMatch == "" {
		return apierror.New(apierror.CodeMissingRequiredHeader,
			"an If-Match precondition is required to replace an existing configuration").
			WithDetail(apierror.Detail{
				Field: "If-Match", Code: "PRECONDITION_REQUIRED",
				Message: "read the active version, then send its ETag",
				RuleID:  "L1.IF_MATCH_REQUIRED",
			})
	}
	if got := current.ETag(); got != ifMatch {
		return apierror.Newf(apierror.CodeConfigurationVersionConflict,
			"the configuration has changed since you read it (If-Match %s, current %s)", ifMatch, got)
	}
	return nil
}

func assertTenant(t shared.TenantID) error {
	if t.IsZero() {
		return apierror.New(apierror.CodeMissingTenantContext, "the request carries no tenant context")
	}
	return nil
}

// assertOwner refuses a cross-tenant read as not-found, never as forbidden.
func assertOwner(t shared.TenantID, c *config.MerchantConfig) error {
	if c == nil {
		return apierror.New(apierror.CodeConfigurationInvalid, "no configuration exists")
	}
	if !c.TenantID.IsZero() && c.TenantID != t {
		return apierror.Newf(apierror.CodeMerchantNotFound,
			"merchant %s does not exist under your tenant", c.MerchantID)
	}
	return nil
}

func containsCurrency(set []money.Currency, v money.Currency) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

func containsMethod(set []shared.PaymentMethod, v shared.PaymentMethod) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-33, FR-43, FR-44, FR-45, FR-46.
//
// Configuration publication with optimistic concurrency, an append-only version history and
// forward-only rollback
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
