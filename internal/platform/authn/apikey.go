package authn

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/netip"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/domain/tenant"
	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// ClientLookup loads an API client by identifier.
//
// Narrow by design: this package needs one thing from the tenancy store, and taking the whole
// TenantRepository would make every test double implement methods it never calls.
type ClientLookup interface {
	GetAPIClient(ctx context.Context, id shared.APIClientID) (*tenant.APIClient, error)
}

// CredentialResolver resolves a credential reference to its material.
//
// The reference — `secretref://env/tenant/merchant/…#v3` — is what the platform stores and
// passes around; the material is fetched at the moment of use and comes back in a redacting
// wrapper. This interface exists so that this package holds the *comparison* logic while
// knowing nothing about Secrets Manager, IRSA or the DEK cache.
type CredentialResolver interface {
	Resolve(ctx context.Context, ref string) (secret.Secret[string], error)
}

// TierLookup supplies the tenant's isolation tier, which the tenant context needs and the API
// client does not carry.
type TierLookup interface {
	TenantTier(ctx context.Context, id shared.TenantID) (shared.TenantTier, error)
}

// APIKeyValidator authenticates a machine client presenting client credentials.
type APIKeyValidator struct {
	clients   ClientLookup
	secrets   CredentialResolver
	tiers     TierLookup
	clock     shared.Clock
	env       shared.Environment
	observer  Observer
	roleForKV []string
}

// APIKeyConfig configures the validator.
type APIKeyConfig struct {
	Clients ClientLookup
	Secrets CredentialResolver
	// Tiers is optional; without it every client is treated as pooled, which is the safe
	// default because pooled is the stricter isolation path (RLS applies either way) and a
	// siloed tenant mis-detected as pooled loses an optimization rather than a control.
	Tiers TierLookup
	Clock shared.Clock
	// Environment is the deployment's environment, stamped onto the principal so the ABAC
	// environment condition has something to compare.
	Environment shared.Environment
	Observer    Observer
	// Roles are the RBAC roles granted to a client-credentials principal. Defaults to
	// `svc:payment-client`, which is the data-plane machine role from security.md §4.1.
	Roles []string
}

// NewAPIKeyValidator builds a client-credentials validator.
func NewAPIKeyValidator(cfg APIKeyConfig) (*APIKeyValidator, error) {
	if cfg.Clients == nil || cfg.Secrets == nil {
		return nil, apierror.New(apierror.CodeInternalError,
			"client-credentials validation requires a client lookup and a credential resolver")
	}
	if !cfg.Environment.IsValid() {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"client-credentials validation requires a valid environment, got %q", cfg.Environment)
	}
	if cfg.Clock == nil {
		cfg.Clock = shared.SystemClock{}
	}
	roles := cfg.Roles
	if len(roles) == 0 {
		roles = []string{RoleServicePaymentClient}
	}
	return &APIKeyValidator{
		clients:   cfg.Clients,
		secrets:   cfg.Secrets,
		tiers:     cfg.Tiers,
		clock:     cfg.Clock,
		env:       cfg.Environment,
		observer:  cfg.Observer,
		roleForKV: append([]string(nil), roles...),
	}, nil
}

// RoleServicePaymentClient is the default role for a client-credentials principal. It is
// declared here rather than imported from authz because authn must not depend on authorization
// policy — the string is a shared vocabulary item, and authz's table is the authority on what it
// grants.
const RoleServicePaymentClient = "svc:payment-client"

// Credentials are what a client presents.
type Credentials struct {
	ClientID     shared.APIClientID
	ClientSecret string
	// SourceIP is the caller's address, used for the per-client network restriction. An invalid
	// address is treated as "unknown", which fails the restriction for a client that configured
	// one — the correct direction, because a restriction that silently does not apply is worse
	// than one that occasionally over-rejects.
	SourceIP netip.Addr
}

// Validate authenticates a client credential.
//
// # Rotation overlap
//
// A client may hold two valid secrets at once: the current one, and — until
// `rotationOverlapUntil` — the previous one (security.md §3.1, §5.3). Both are checked. That
// overlap is not a convenience; it is what makes a 90-day rotation policy achievable. Without
// it, every rotation is a synchronised cutover between our deploy and the merchant's, which
// means every rotation is an outage risk, which means rotation quietly stops happening and the
// control exists only on paper.
//
// # Constant time, and why both candidates are always evaluated
//
// The comparison is over SHA-256 digests with crypto/subtle, so neither the content nor the
// length of the stored secret leaks through timing. Both candidates are evaluated even after
// the first one matches, and the results are combined with a bitwise OR rather than a
// short-circuit `||`. A short circuit would make a request that matched the *current* secret
// measurably faster than one that matched the *previous* one, which tells an attacker whether a
// rotation is in progress — a small leak, but the cost of removing it is one extra hash of a
// value already in memory.
func (v *APIKeyValidator) Validate(ctx context.Context, creds Credentials) (*Principal, error) {
	p, err := v.validate(ctx, creds)
	if err != nil {
		if v.observer != nil {
			v.observer.AuthenticationFailed(MethodAPIKey, ReasonOf(err))
		}
		return nil, err
	}
	return p, nil
}

func (v *APIKeyValidator) validate(ctx context.Context, creds Credentials) (*Principal, error) {
	if creds.ClientID == "" || creds.ClientSecret == "" {
		return nil, reject(ReasonUnknownClient)
	}
	client, err := v.clients.GetAPIClient(ctx, creds.ClientID)
	if err != nil || client == nil {
		// A client that does not exist and a client whose lookup failed produce the same
		// answer. Distinguishing them would let an attacker enumerate valid client identifiers
		// by watching which ones behave differently.
		return nil, reject(ReasonUnknownClient)
	}
	if client.Status() != tenant.ClientActive {
		return nil, reject(ReasonClientNotActive)
	}

	now := v.clock.Now()
	presented := sha256.Sum256([]byte(creds.ClientSecret))

	// Evaluate both candidates unconditionally; combine without short-circuiting.
	match := v.matches(ctx, client.CredentialRef(), presented)
	if prev := client.PreviousCredentialRef(); prev != "" && now.Before(client.RotationOverlapUntil()) {
		match |= v.matches(ctx, prev, presented)
	}
	if match != 1 {
		return nil, reject(ReasonBadSecret)
	}

	// The network restriction is checked after the secret, not before. Checking it first would
	// turn the allowlist into an oracle: an attacker could learn which source addresses a
	// client permits without ever holding a valid secret.
	if !client.AllowsIP(creds.SourceIP) {
		return nil, reject(ReasonSourceNotAllowed)
	}

	tier := shared.TierPooled
	if v.tiers != nil {
		if t, err := v.tiers.TenantTier(ctx, client.TenantID()); err == nil && t.IsValid() {
			tier = t
		}
	}

	return &Principal{
		Method:      MethodAPIKey,
		Type:        tenantctx.PrincipalMachine,
		ID:          client.ID().String(),
		Name:        client.Name(),
		TenantID:    client.TenantID(),
		TenantTier:  tier,
		Scopes:      client.Scopes(),
		Roles:       append([]string(nil), v.roleForKV...),
		Environment: v.env,
		// A client credential is not a token: there is no jti to replay-check and no exp to
		// honour. That is precisely why the platform prefers `private_key_jwt` and why the
		// access token it mints lives fifteen minutes — a replayable long-lived secret is a
		// worse primitive, and this path exists for legacy tenants on a migration deadline.
		ExpiresAt: now,
		AuthTime:  now,
	}, nil
}

// matches returns 1 when the presented digest equals the material behind ref, and 0 otherwise.
// It returns an int rather than a bool so the caller can combine results with OR without a
// branch, keeping the "both candidates always evaluated" property visible at the call site.
func (v *APIKeyValidator) matches(ctx context.Context, ref string, presented [32]byte) int {
	if ref == "" {
		return 0
	}
	material, err := v.secrets.Resolve(ctx, ref)
	if err != nil {
		return 0
	}
	// Hash before comparing so that unequal lengths cannot be distinguished by the comparison
	// itself: ConstantTimeCompare returns 0 immediately for different lengths, which leaks the
	// stored secret's length. Comparing fixed-width digests removes the question.
	stored := sha256.Sum256([]byte(material.Expose()))
	return subtle.ConstantTimeCompare(presented[:], stored[:])
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: FR-08, NFR-31.
//
// API-key authentication, including the dual-run overlap that makes a rotation non-breaking
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
