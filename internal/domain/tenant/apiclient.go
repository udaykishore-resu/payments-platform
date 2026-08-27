package tenant

import (
	"crypto/subtle"
	"net/netip"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// ClientStatus is the lifecycle of a machine identity.
type ClientStatus string

const (
	// ClientActive is a usable credential.
	ClientActive ClientStatus = "ACTIVE"
	// ClientDisabled is a reversible stop — an unused integration parked, or a client held during
	// an investigation. The credential still exists in the secret store and can be re-enabled.
	ClientDisabled ClientStatus = "DISABLED"
	// ClientRevoked is permanent: the credential is destroyed. Terminal, because a revoked
	// credential that could be resurrected is not revoked, and the entire value of revocation as
	// an incident-response action is that it is irreversible.
	ClientRevoked ClientStatus = "REVOKED"
)

// AllClientStatuses is the complete state universe.
var AllClientStatuses = []ClientStatus{ClientActive, ClientDisabled, ClientRevoked}

var clientMachine = shared.NewStateMachine("api_client", ClientActive,
	AllClientStatuses, []ClientStatus{ClientRevoked},
	[]shared.Transition[ClientStatus]{
		{From: ClientActive, To: ClientDisabled},
		{From: ClientDisabled, To: ClientActive},
		{From: ClientActive, To: ClientRevoked},
		{From: ClientDisabled, To: ClientRevoked},
	})

// ClientMachine exposes the API client state machine for the documentation generator and the
// exhaustive property test.
func ClientMachine() *shared.StateMachine[ClientStatus] { return clientMachine }

// IsKnown reports whether s is a status this binary understands.
func (s ClientStatus) IsKnown() bool { return clientMachine.IsKnown(s) }

// String satisfies fmt.Stringer.
func (s ClientStatus) String() string { return string(s) }

// scopeWildcard is the suffix that makes a granted scope hierarchical: `payments:*` grants
// `payments:read` and `payments:write`.
const scopeWildcard = ":*"

// APIClient is a machine identity within a tenant.
//
// Every request to the platform is authenticated as one of these, and the tenant is derived from
// it and from nothing else (baseline §16.2). A `tenantId` in a request body is ignored, or, if it
// disagrees, treated as a security event — which is only sound because the client *is* the
// authoritative statement of tenancy. That is why this aggregate has no method that can change
// its tenant: a client whose tenant could be reassigned would turn the platform's entire
// isolation guarantee into a mutable field.
//
// As everywhere else in the platform, there is no credential material here. The struct holds
// references into the secret store, and a full dump of it is safe to log.
type APIClient struct {
	id       shared.APIClientID
	tenantID shared.TenantID
	name     string

	scopes []string

	// allowedCIDRs restricts where the credential may be used from. Parsed prefixes rather than
	// strings, because a check that has to parse on every request either becomes a hot-path
	// allocation or gets skipped, and a network restriction that is skipped under load is worse
	// than one that does not exist — it is a control somebody is relying on.
	allowedCIDRs []netip.Prefix

	// credentialRef points at the current credential in the secret store.
	credentialRef string

	// previousCredentialRef and rotationOverlapUntil are how a ≤90-day rotation (baseline §17.2)
	// happens with no downtime. During the overlap both credentials authenticate, so the merchant
	// can redeploy at their own pace; after the deadline the old one stops working whether or not
	// they did. Without the overlap, rotation is a synchronised cutover between us and the
	// merchant's deployment pipeline, which is why platforms that model it that way quietly stop
	// rotating.
	previousCredentialRef string
	rotationOverlapUntil  time.Time

	status ClientStatus
	// statusReason records why the client was disabled or revoked. Kept because the first
	// question about a credential that stopped working is always "who turned this off and why",
	// and reconstructing that from surrounding audit records is guesswork.
	statusReason string

	createdAt     time.Time
	updatedAt     time.Time
	lastRotatedAt *time.Time
	revokedAt     *time.Time

	version shared.Version

	events []Event
}

// NewAPIClientParams are the inputs to creating a machine identity.
type NewAPIClientParams struct {
	TenantID      shared.TenantID
	Name          string
	Scopes        []string
	AllowedCIDRs  []string
	CredentialRef string
}

// NewAPIClient creates an ACTIVE client.
//
// It requires at least one scope. A scopeless client is one that can authenticate but do nothing,
// which sounds harmless and is not: it is a valid credential in circulation whose blast radius
// nobody has decided, and the way that gets resolved in practice is somebody granting it
// everything at three in the morning.
func NewAPIClient(p NewAPIClientParams, clock shared.Clock) (*APIClient, error) {
	if p.TenantID.IsZero() {
		return nil, apierror.New(apierror.CodeMissingTenantContext, "an API client requires a tenant")
	}
	if strings.TrimSpace(p.Name) == "" {
		return nil, apierror.New(apierror.CodeValidationFailed, "an API client requires a name").
			WithDetail(apierror.Detail{
				Field: "name", Code: "MISSING",
				Message: "a name is how an operator identifies which integration a credential belongs to",
				RuleID:  "L4.API_CLIENT_NAME_REQUIRED",
			})
	}
	if len(p.Scopes) == 0 {
		return nil, apierror.New(apierror.CodeValidationFailed, "an API client requires at least one scope").
			WithDetail(apierror.Detail{
				Field: "scopes", Code: "MISSING",
				Message: "a credential with no scopes is a credential whose blast radius nobody has decided",
				RuleID:  "L4.API_CLIENT_SCOPES_REQUIRED",
			})
	}
	if err := validateCredentialRef(p.CredentialRef); err != nil {
		return nil, err
	}
	cidrs, err := parseCIDRs(p.AllowedCIDRs)
	if err != nil {
		return nil, err
	}

	now := clock.Now()
	c := &APIClient{
		id:            shared.NewAPIClientID(),
		tenantID:      p.TenantID,
		name:          strings.TrimSpace(p.Name),
		scopes:        normalizeScopes(p.Scopes),
		allowedCIDRs:  cidrs,
		credentialRef: p.CredentialRef,
		status:        ClientActive,
		createdAt:     now,
		updatedAt:     now,
		version:       1,
	}
	c.raise(EventTenantAPIClientCreated, now, map[string]any{
		"apiClientId": c.id.String(),
		"name":        c.name,
		"scopes":      append([]string(nil), c.scopes...),
	})
	return c, nil
}

// Accessors.

func (c *APIClient) ID() shared.APIClientID          { return c.id }
func (c *APIClient) TenantID() shared.TenantID       { return c.tenantID }
func (c *APIClient) Name() string                    { return c.name }
func (c *APIClient) CredentialRef() string           { return c.credentialRef }
func (c *APIClient) PreviousCredentialRef() string   { return c.previousCredentialRef }
func (c *APIClient) RotationOverlapUntil() time.Time { return c.rotationOverlapUntil }
func (c *APIClient) Status() ClientStatus            { return c.status }
func (c *APIClient) StatusReason() string            { return c.statusReason }
func (c *APIClient) CreatedAt() time.Time            { return c.createdAt }
func (c *APIClient) UpdatedAt() time.Time            { return c.updatedAt }
func (c *APIClient) LastRotatedAt() *time.Time       { return c.lastRotatedAt }
func (c *APIClient) RevokedAt() *time.Time           { return c.revokedAt }
func (c *APIClient) Version() shared.Version         { return c.version }

// Scopes returns a copy of the granted scope set.
func (c *APIClient) Scopes() []string { return append([]string(nil), c.scopes...) }

// AllowedCIDRs returns a copy of the network restriction set.
func (c *APIClient) AllowedCIDRs() []netip.Prefix {
	return append([]netip.Prefix(nil), c.allowedCIDRs...)
}

// HasScope reports whether the client is granted a scope.
//
// A granted scope ending in `:*` covers everything below it, so `payments:*` satisfies a
// requirement for `payments:write`. The hierarchy is one level and matches on the prefix before
// the wildcard, which keeps the check to a string comparison with no parsing — this runs on every
// authenticated request.
//
// The wildcard expands only in the *grant*, never in the *requirement*: a caller asking whether
// the client has `payments:*` is asking a question about a specific grant and gets a literal
// answer. Letting a requirement wildcard would mean a middleware could accidentally ask "does
// this client have any payment scope at all" and be told yes for a read-only client.
func (c *APIClient) HasScope(scope string) bool {
	if scope == "" {
		return false
	}
	want := strings.ToLower(strings.TrimSpace(scope))
	for _, granted := range c.scopes {
		if granted == want {
			return true
		}
		if strings.HasSuffix(granted, scopeWildcard) {
			prefix := strings.TrimSuffix(granted, "*")
			if strings.HasPrefix(want, prefix) && len(want) > len(prefix) {
				return true
			}
		}
	}
	return false
}

// AllowsIP reports whether the credential may be used from this address.
//
// No CIDRs configured means no restriction. That is the pragmatic default — most integrations run
// from dynamic infrastructure and cannot name their egress addresses — and it is safe because the
// restriction is defence in depth on top of the credential, never a substitute for it. What it
// buys is that a leaked credential for a client that *did* pin its egress range is useless from
// anywhere else.
func (c *APIClient) AllowsIP(addr netip.Addr) bool {
	if len(c.allowedCIDRs) == 0 {
		return true
	}
	if !addr.IsValid() {
		return false
	}
	// Unmap so that an IPv4-mapped IPv6 address (::ffff:203.0.113.4, which is what a dual-stack
	// listener reports) matches an IPv4 prefix. Without this the restriction silently fails
	// closed for every caller behind a dual-stack load balancer.
	a := addr.Unmap()
	for _, p := range c.allowedCIDRs {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Rotate issues a new credential and keeps the previous one valid until overlapUntil.
//
// This is the mechanism behind the ≤90-day rotation control. The overlap window is what makes the
// control achievable rather than aspirational: the merchant's systems keep working on the old
// credential while they roll out the new one, and the old one expires on our schedule regardless
// of whether they finished. A rotation with no overlap is a coordinated outage, and a control
// that causes outages is a control that gets waived.
//
// The deadline is required and must be in the future. An overlap that has already expired makes
// the rotation an immediate cutover under a name that promises it is not one, which is the worst
// of both: the caller believes they have a window and they do not.
func (c *APIClient) Rotate(newRef string, overlapUntil time.Time, clock shared.Clock) error {
	if c.status == ClientRevoked {
		return apierror.New(apierror.CodeInvalidStateTransition,
			"cannot rotate the credentials of a revoked API client")
	}
	if err := validateCredentialRef(newRef); err != nil {
		return err
	}
	if newRef == c.credentialRef {
		return apierror.New(apierror.CodeValidationFailed,
			"the new credential reference is identical to the current one").
			WithDetail(apierror.Detail{
				Field: "credentialRef", Code: "UNCHANGED",
				Message: "a rotation that does not change the reference is not a rotation",
				RuleID:  "L4.ROTATION_CHANGES_CREDENTIAL",
			})
	}
	now := clock.Now()
	if !overlapUntil.After(now) {
		return apierror.New(apierror.CodeValidationFailed,
			"the rotation overlap deadline must be in the future").
			WithDetail(apierror.Detail{
				Field: "overlapUntil", Code: "NOT_IN_FUTURE",
				Message: "an already-expired overlap makes the rotation an immediate cutover; pass a future deadline or revoke instead",
				RuleID:  "L4.ROTATION_OVERLAP_IN_FUTURE",
			})
	}

	previous := c.credentialRef
	c.previousCredentialRef = previous
	c.rotationOverlapUntil = overlapUntil.UTC()
	c.credentialRef = newRef
	c.lastRotatedAt = &now
	c.touch(now)
	c.raise(EventTenantAPIClientRotated, now, map[string]any{
		"apiClientId":           c.id.String(),
		"previousCredentialRef": previous,
		"credentialRef":         newRef,
		"overlapUntil":          c.rotationOverlapUntil.Format(time.RFC3339Nano),
	})
	return nil
}

// IsCredentialValid reports whether the presented credential reference authenticates this client
// at time now.
//
// The current reference always works while the client is usable. The previous one works only
// strictly before the overlap deadline — the boundary is exclusive, so a deadline that has been
// reached is a deadline that has passed, and the ninety-day clock in the control evidence means
// what it says.
//
// A disabled or revoked client authenticates nothing, with either reference. Checking the status
// here rather than only at the call site means there is no path that validates a credential
// without also asking whether the identity is still allowed to exist.
//
// The comparison is constant-time. These are references, not secrets, so a timing side channel
// would leak very little — but the value is compared against caller-supplied input on the
// authentication path, and the cost of removing the question entirely is one function call.
func (c *APIClient) IsCredentialValid(ref string, now time.Time) bool {
	if c.status != ClientActive || ref == "" {
		return false
	}
	if constantTimeEqual(ref, c.credentialRef) {
		return true
	}
	if c.previousCredentialRef == "" {
		return false
	}
	if !now.Before(c.rotationOverlapUntil) {
		return false
	}
	return constantTimeEqual(ref, c.previousCredentialRef)
}

// RotationIsOverdue reports whether the current credential is older than the platform's maximum
// credential age. The threshold is a parameter rather than a constant here because the domain
// should not be the place the ninety days is written down — it is a compliance policy that lives
// in configuration and is applied by the scheduler that scans for overdue clients.
func (c *APIClient) RotationIsOverdue(maxAge time.Duration, now time.Time) bool {
	if c.status != ClientActive || maxAge <= 0 {
		return false
	}
	since := c.createdAt
	if c.lastRotatedAt != nil {
		since = *c.lastRotatedAt
	}
	return now.Sub(since) > maxAge
}

// GrantScopes adds scopes to the client.
func (c *APIClient) GrantScopes(scopes []string, clock shared.Clock) error {
	if c.status != ClientActive {
		return apierror.Newf(apierror.CodeInvalidStateTransition,
			"cannot change the scopes of a %s API client", c.status)
	}
	added := false
	for _, s := range normalizeScopes(scopes) {
		if !containsString(c.scopes, s) {
			c.scopes = append(c.scopes, s)
			added = true
		}
	}
	if !added {
		return nil
	}
	c.touch(clock.Now())
	return nil
}

// RevokeScopes removes scopes, refusing to leave the client with none — a scopeless client is
// exactly the state NewAPIClient refuses to create, and a method that could produce it by
// subtraction would make the constructor's guarantee worthless.
func (c *APIClient) RevokeScopes(scopes []string, clock shared.Clock) error {
	if c.status != ClientActive {
		return apierror.Newf(apierror.CodeInvalidStateTransition,
			"cannot change the scopes of a %s API client", c.status)
	}
	remove := make(map[string]struct{}, len(scopes))
	for _, s := range normalizeScopes(scopes) {
		remove[s] = struct{}{}
	}
	kept := make([]string, 0, len(c.scopes))
	for _, s := range c.scopes {
		if _, drop := remove[s]; !drop {
			kept = append(kept, s)
		}
	}
	if len(kept) == len(c.scopes) {
		return nil
	}
	if len(kept) == 0 {
		return apierror.New(apierror.CodeValidationFailed,
			"an API client must retain at least one scope; revoke the client instead").
			WithDetail(apierror.Detail{
				Field: "scopes", Code: "WOULD_LEAVE_NO_SCOPES",
				Message: "a credential with no scopes should be revoked, not left in circulation",
				RuleID:  "L4.API_CLIENT_SCOPES_REQUIRED",
			})
	}
	c.scopes = kept
	c.touch(clock.Now())
	return nil
}

// Disable stops the client reversibly.
func (c *APIClient) Disable(reason string, clock shared.Clock) error {
	if err := c.transition(ClientDisabled, clock, nil); err != nil {
		return err
	}
	c.statusReason = reason
	return nil
}

// Enable re-activates a disabled client, clearing the recorded reason.
func (c *APIClient) Enable(clock shared.Clock) error {
	if err := c.transition(ClientActive, clock, nil); err != nil {
		return err
	}
	c.statusReason = ""
	return nil
}

// Revoke permanently destroys the credential.
//
// The references are cleared on the aggregate as well as in the secret store. Leaving them would
// mean a revoked client still names the paths of credentials we intend nobody to resolve, and
// clearing them costs two assignments.
func (c *APIClient) Revoke(reason string, clock shared.Clock) error {
	if reason == "" {
		return apierror.New(apierror.CodeValidationFailed, "revoking an API client requires a reason").
			WithDetail(apierror.Detail{
				Field: "reason", Code: "MISSING",
				Message: "revocation is irreversible and must record why it happened",
				RuleID:  "L4.REVOCATION_REQUIRES_REASON",
			})
	}
	now := clock.Now()
	if err := c.transition(ClientRevoked, clock, map[string]any{
		"apiClientId": c.id.String(),
		"reason":      reason,
	}); err != nil {
		return err
	}
	c.credentialRef = ""
	c.previousCredentialRef = ""
	c.rotationOverlapUntil = time.Time{}
	c.statusReason = reason
	c.revokedAt = &now
	return nil
}

func (c *APIClient) transition(to ClientStatus, clock shared.Clock, revokePayload map[string]any) error {
	if err := clientMachine.Transition(c.status, to); err != nil {
		return err
	}
	now := clock.Now()
	c.status = to
	c.touch(now)
	if to == ClientRevoked {
		c.raise(EventTenantAPIClientRevoked, now, revokePayload)
	}
	return nil
}

func (c *APIClient) touch(now time.Time) {
	c.updatedAt = now
	c.version = c.version.Next()
}

func (c *APIClient) raise(e EventType, at time.Time, payload map[string]any) {
	c.events = append(c.events, Event{
		Type:        e,
		TenantID:    c.tenantID,
		APIClientID: c.id,
		OccurredAt:  at,
		Version:     c.version,
		Payload:     payload,
	})
}

// PendingEvents returns the domain events raised in this unit of work.
func (c *APIClient) PendingEvents() []Event { return append([]Event(nil), c.events...) }

// DrainEvents returns and clears the pending events.
func (c *APIClient) DrainEvents() []Event {
	out := c.events
	c.events = nil
	return out
}

// validateCredentialRef enforces that the value is a `secret://` reference rather than credential
// material. It is the same guard the gateway connection applies, restated here rather than
// imported so that BC-1 does not take a dependency on BC-4 for a four-line string check — the
// shared kernel's bar is "two contexts cannot express themselves without it", and both can.
func validateCredentialRef(ref string) error {
	const scheme = "secret://"
	if ref == "" {
		return apierror.New(apierror.CodeValidationFailed, "a credential reference is required").
			WithDetail(apierror.Detail{
				Field: "credentialRef", Code: "MISSING_SECRET_REF",
				Message: "expected a secret:// URI pointing at the secret store",
				RuleID:  "L4.CREDENTIAL_IS_A_REFERENCE",
			})
	}
	if !strings.HasPrefix(ref, scheme) || strings.TrimSpace(strings.TrimPrefix(ref, scheme)) == "" {
		return apierror.New(apierror.CodeValidationFailed,
			"a credential reference must be a secret:// URI, never credential material").
			WithDetail(apierror.Detail{
				Field: "credentialRef", Code: "NOT_A_SECRET_REF",
				Message: "expected a value of the form secret://{env}/{tenant}/api-clients/{clientId}",
				RuleID:  "L4.CREDENTIAL_IS_A_REFERENCE",
			})
	}
	return nil
}

func parseCIDRs(in []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		p, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil {
			return nil, apierror.Newf(apierror.CodeValidationFailed,
				"invalid CIDR %q in the allowed address list", s).
				WithDetail(apierror.Detail{
					Field: "allowedIpCidrs", Code: "INVALID_CIDR",
					Message: "expected CIDR notation, for example 203.0.113.0/24",
					RuleID:  "L4.API_CLIENT_CIDR_WELL_FORMED",
				})
		}
		// Masking discards host bits, so 203.0.113.4/24 is stored as 203.0.113.0/24. Without it,
		// netip.Prefix.Contains reports false for every address including the one that was typed,
		// which is a network restriction that silently blocks everything.
		out = append(out, p.Masked())
	}
	return out, nil
}

// normalizeScopes lowercases, trims and de-duplicates. Scopes are compared on the hot path and
// case-normalising there would be a per-request allocation; doing it once at the boundary means
// the comparison is a plain string equality.
func normalizeScopes(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		v := strings.ToLower(strings.TrimSpace(s))
		if v == "" || containsString(out, v) {
			continue
		}
		out = append(out, v)
	}
	return out
}

func containsString(set []string, v string) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

func constantTimeEqual(a, b string) bool {
	// ConstantTimeCompare is only constant-time for equal-length inputs, so the length check has
	// to happen first regardless; it leaks the length of a value that is not secret.
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// RehydrateAPIClientParams carries the persisted state of an APIClient.
type RehydrateAPIClientParams struct {
	ID                    shared.APIClientID
	TenantID              shared.TenantID
	Name                  string
	Scopes                []string
	AllowedCIDRs          []string
	CredentialRef         string
	PreviousCredentialRef string
	RotationOverlapUntil  time.Time
	Status                ClientStatus
	StatusReason          string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	LastRotatedAt         *time.Time
	RevokedAt             *time.Time
	Version               shared.Version
}

// RehydrateAPIClient reconstructs an APIClient from persisted state.
//
// The CIDR list is re-parsed rather than trusted, because a malformed row would otherwise produce
// a client whose network restriction silently matches nothing — a control that appears configured
// and blocks every legitimate caller.
func RehydrateAPIClient(p RehydrateAPIClientParams) (*APIClient, error) {
	if !p.Status.IsKnown() {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"api client %s has unknown status %q; this row may have been written by a newer version of the service",
			p.ID, p.Status)
	}
	cidrs, err := parseCIDRs(p.AllowedCIDRs)
	if err != nil {
		// Deliberately a fresh INTERNAL_ERROR rather than apierror.Wrap: Wrap preserves the
		// innermost code, which would surface a corrupt database row to a caller as
		// VALIDATION_FAILED and send them looking at their own request. A malformed row is our
		// problem, not theirs.
		return nil, apierror.Newf(apierror.CodeInternalError,
			"api client %s has a malformed allowed-CIDR list: %v", p.ID, err)
	}
	return &APIClient{
		id: p.ID, tenantID: p.TenantID, name: p.Name,
		scopes: normalizeScopes(p.Scopes), allowedCIDRs: cidrs,
		credentialRef: p.CredentialRef, previousCredentialRef: p.PreviousCredentialRef,
		rotationOverlapUntil: p.RotationOverlapUntil, status: p.Status,
		statusReason: p.StatusReason,
		createdAt:    p.CreatedAt, updatedAt: p.UpdatedAt,
		lastRotatedAt: p.LastRotatedAt, revokedAt: p.RevokedAt, version: p.Version,
	}, nil
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-02, BR-12, FR-03, FR-08, NFR-31.
//
// API clients, their least-privilege scopes, and credential rotation with a bounded overlap
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
