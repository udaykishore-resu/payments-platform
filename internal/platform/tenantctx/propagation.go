package tenantctx

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Event envelope header names.
//
// They are constants rather than string literals at the call sites because these names are a
// wire contract: a producer and a consumer that disagree about the spelling do not fail — the
// consumer simply finds no tenant and rejects every message, which looks like a broker problem
// for the first hour of the incident.
//
// The `pp-` prefix keeps them out of the way of CloudEvents' own attribute names and of
// broker-managed headers.
const (
	// HeaderTenantID is the required envelope extension (baseline §13.1). An event without it
	// is refused at encode time and dead-lettered at decode time.
	HeaderTenantID = "pp-tenant-id"
	// HeaderTenantTier lets a consumer pick the right pool and cache namespace without a
	// lookup on the hot path.
	HeaderTenantTier = "pp-tenant-tier"
	// HeaderEnvironment catches a sandbox event replayed into production.
	HeaderEnvironment = "pp-environment"
	// HeaderPrincipalType, HeaderPrincipalID and HeaderPrincipalName carry the originating
	// identity so an asynchronous effect can be attributed in the audit trail to the human or
	// client that caused it, rather than to "the consumer".
	HeaderPrincipalType = "pp-principal-type"
	HeaderPrincipalID   = "pp-principal-id"
	HeaderPrincipalName = "pp-principal-name"
	// HeaderRequestID and HeaderCorrelationID keep the causal chain intact across the broker.
	HeaderRequestID     = "pp-request-id"
	HeaderCorrelationID = "pp-correlation-id"
	// HeaderMerchantScope is a comma-separated list, empty when the principal covers the whole
	// tenant.
	HeaderMerchantScope = "pp-merchant-scope"
)

// EventHeaders renders the tenant context as envelope headers for an outbound event.
//
// Scopes are deliberately *not* propagated. An event is a statement that something happened,
// and a consumer acting on it is not acting on behalf of the original caller's authorization —
// it is performing a platform-owned effect (a projection, a ledger append, a notification)
// whose authority comes from the consumer's own workload identity. Carrying the producer's
// scopes into the envelope would invite a consumer to treat them as a grant, which would make
// every event a bearer token with no expiry and no signature.
//
// It returns an error when there is no tenant context, which is what makes "an event cannot be
// published without a tenant" a property of the codec rather than a convention.
func EventHeaders(ctx context.Context) (map[string]string, error) {
	tc, err := FromContext(ctx)
	if err != nil {
		return nil, err
	}
	h := map[string]string{
		HeaderTenantID:      tc.TenantID.String(),
		HeaderTenantTier:    string(tc.Tier),
		HeaderEnvironment:   string(tc.Environment),
		HeaderPrincipalType: string(tc.Principal.Type),
		HeaderPrincipalID:   tc.Principal.ID,
	}
	// Optional fields are omitted rather than written empty: an empty header is
	// indistinguishable from a header the producer could not populate, and the consumer would
	// have to guess which it was.
	if tc.Principal.Name != "" {
		h[HeaderPrincipalName] = tc.Principal.Name
	}
	if tc.RequestID != "" {
		h[HeaderRequestID] = tc.RequestID
	}
	if tc.CorrelationID != "" {
		h[HeaderCorrelationID] = tc.CorrelationID
	}
	if len(tc.MerchantScope) > 0 {
		parts := make([]string, 0, len(tc.MerchantScope))
		for _, m := range tc.MerchantScope {
			parts = append(parts, m.String())
		}
		h[HeaderMerchantScope] = strings.Join(parts, ",")
	}
	return h, nil
}

// FromEventHeaders restores a tenant context on the consumer side.
//
// The consumer gets the same guarantees an HTTP request had: the tenant is validated for shape,
// the environment and tier must be ones this binary understands, and the result is refused
// rather than defaulted. The Source is stamped EVENT_ENVELOPE, so an audit record written by a
// consumer is distinguishable from one written by the API — which is the first thing anyone
// asks when a record looks wrong.
//
// The base ctx is the consumer's own context (carrying its deadline and its trace span); only
// the identity comes from the headers.
func FromEventHeaders(ctx context.Context, headers map[string]string) (context.Context, error) {
	if len(headers) == 0 {
		return ctx, apierror.New(apierror.CodeMissingTenantContext,
			"event envelope carries no tenant headers")
	}
	tc := TenantContext{
		TenantID:      shared.TenantID(headers[HeaderTenantID]),
		Tier:          shared.TenantTier(headers[HeaderTenantTier]),
		Environment:   shared.Environment(headers[HeaderEnvironment]),
		RequestID:     headers[HeaderRequestID],
		CorrelationID: headers[HeaderCorrelationID],
		Principal: Principal{
			Type: PrincipalType(headers[HeaderPrincipalType]),
			ID:   headers[HeaderPrincipalID],
			Name: headers[HeaderPrincipalName],
		},
		MerchantScope: parseMerchantScope(headers[HeaderMerchantScope]),
		Source:        SourceEventEnvelope,
	}
	return WithTenant(ctx, tc)
}

// AssertEnvelopeTenant is the defence-in-depth check from multi-tenancy.md §3.3: some payloads
// repeat the tenant for convenience, and a payload that disagrees with the envelope means
// either a producer bug or a forged message.
//
// It is separate from AssertTenant because the remedy differs: a synchronous request is
// refused with a 403, whereas an event is dead-lettered — retrying a forged message forever
// achieves nothing and blocks the partition behind it.
func AssertEnvelopeTenant(envelopeTenant, payloadTenant string) error {
	if payloadTenant == "" || payloadTenant == envelopeTenant {
		return nil
	}
	return apierror.New(apierror.CodeTenantMismatch,
		"event payload tenant disagrees with the envelope tenant")
}

func parseMerchantScope(s string) []shared.MerchantID {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]shared.MerchantID, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, shared.MerchantID(p))
		}
	}
	return out
}

// activityState is the serialized form of a tenant context stored on a workflow instance.
//
// It is a distinct, explicitly-tagged struct rather than `json.Marshal(TenantContext)` because
// this blob is *persisted* and read back by a future binary. Marshalling the live struct would
// make every field rename a silent data-compatibility break: a renamed field decodes as its
// zero value, and a zero tenant is a workflow that resumes with no isolation.
type activityState struct {
	Version       int      `json:"v"`
	TenantID      string   `json:"tenant_id"`
	Tier          string   `json:"tenant_tier"`
	Environment   string   `json:"environment"`
	PrincipalType string   `json:"principal_type"`
	PrincipalID   string   `json:"principal_id"`
	PrincipalName string   `json:"principal_name,omitempty"`
	MerchantScope []string `json:"merchant_scope,omitempty"`
	RequestID     string   `json:"request_id,omitempty"`
	CorrelationID string   `json:"correlation_id,omitempty"`
}

// activityStateVersion is the schema version of the persisted blob. An unknown version is
// refused rather than best-effort decoded: a workflow that resumes under a context this binary
// only partially understood is worse than one that refuses to resume and pages someone.
const activityStateVersion = 1

// MarshalActivity serializes the tenant context for storage on a workflow instance row.
//
// A workflow step may run hours or days after the request that started it, on a different pod,
// after a deploy. Persisting the identity alongside the instance — rather than re-deriving it
// from whatever the resuming worker happens to have — is what lets an activity carry exactly
// the guarantees the originating HTTP request had, no more and no less.
func MarshalActivity(ctx context.Context) ([]byte, error) {
	tc, err := FromContext(ctx)
	if err != nil {
		return nil, err
	}
	st := activityState{
		Version:       activityStateVersion,
		TenantID:      tc.TenantID.String(),
		Tier:          string(tc.Tier),
		Environment:   string(tc.Environment),
		PrincipalType: string(tc.Principal.Type),
		PrincipalID:   tc.Principal.ID,
		PrincipalName: tc.Principal.Name,
		RequestID:     tc.RequestID,
		CorrelationID: tc.CorrelationID,
	}
	for _, m := range tc.MerchantScope {
		st.MerchantScope = append(st.MerchantScope, m.String())
	}
	b, err := json.Marshal(st)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "serializing tenant context")
	}
	return b, nil
}

// UnmarshalActivity restores a tenant context when a workflow instance is leased.
//
// Source is stamped WORKFLOW_INSTANCE. As with the event path, the base ctx supplies the
// worker's deadline and trace; only the identity comes from the persisted blob.
func UnmarshalActivity(ctx context.Context, data []byte) (context.Context, error) {
	if len(data) == 0 {
		return ctx, apierror.New(apierror.CodeMissingTenantContext,
			"workflow instance carries no serialized tenant context")
	}
	var st activityState
	if err := json.Unmarshal(data, &st); err != nil {
		return ctx, apierror.Wrap(err, apierror.CodeMissingTenantContext,
			"workflow instance tenant context is not decodable")
	}
	if st.Version != activityStateVersion {
		return ctx, apierror.Newf(apierror.CodeMissingTenantContext,
			"workflow instance tenant context has unsupported version %d", st.Version)
	}
	tc := TenantContext{
		TenantID:      shared.TenantID(st.TenantID),
		Tier:          shared.TenantTier(st.Tier),
		Environment:   shared.Environment(st.Environment),
		RequestID:     st.RequestID,
		CorrelationID: st.CorrelationID,
		Principal: Principal{
			Type: PrincipalType(st.PrincipalType),
			ID:   st.PrincipalID,
			Name: st.PrincipalName,
		},
		Source: SourceWorkflowInstance,
	}
	for _, m := range st.MerchantScope {
		tc.MerchantScope = append(tc.MerchantScope, shared.MerchantID(m))
	}
	return WithTenant(ctx, tc)
}
