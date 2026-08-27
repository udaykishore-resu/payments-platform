// Package authn turns a credential into a Principal, and is the only place in the platform
// where that conversion happens.
//
// Four mechanisms reach this package — an OAuth2 access token (security.md §3.1/§3.3), a client
// credential (§3.1), an mTLS client certificate carrying a SPIFFE ID (§3.5), and, indirectly,
// the human OIDC flow that mints the same shape of token as the machine flow. They all produce
// one type, Principal, and everything downstream — the tenant guard, authorization, rate
// limiting, the audit record — sees only that.
//
// # Why one output type matters more than it looks
//
// If each mechanism produced its own shape, every consumer would branch on the mechanism, and
// the branches would drift. The predictable consequence is a check that is present on the JWT
// path and missing on the mTLS one — which is how "internal callers are trusted" quietly
// reappears in a Zero Trust architecture. Collapsing to one type means a new mechanism has to
// fill in the same fields the old ones did, and a field it cannot fill is a conversation at
// review time rather than a silent zero value in production.
//
// # Failures are uniform
//
// Every authentication failure in this package produces the same 401 with the same body
// (security.md §3.3, last row). The specific reason travels as an unexported cause, reachable
// by ReasonOf for logs and metrics, and never reaches the caller. Distinguishing "unknown
// client" from "bad signature" in a response is an oracle: it tells an attacker which half of a
// guess was right, which is the difference between a search over the product of two spaces and a
// search over their sum.
package authn

import (
	"errors"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Method names the mechanism that authenticated a principal.
//
// It is recorded rather than erased because several policies legitimately depend on it: a
// sender-constrained scope requires the JWT path, `svc:internal` requires the mTLS path, and an
// audit record that cannot say how someone authenticated is missing the first fact an
// investigator asks for.
type Method string

const (
	// MethodJWT is an OAuth2 access token, for both machine clients and humans.
	MethodJWT Method = "JWT"
	// MethodAPIKey is direct client-credentials validation against tenant.APIClient.
	MethodAPIKey Method = "API_KEY"
	// MethodMTLS is a verified client certificate carrying a SPIFFE ID.
	MethodMTLS Method = "MTLS"
)

// DevicePosture is what the identity provider asserts about the machine a human is using.
//
// It is a claim from the IdP, not something this platform measures, and it is modelled as two
// booleans with an explicit "unknown" (both false) rather than one tri-state because the ABAC
// condition that reads it must fail closed: an absent posture assertion denies an admin action
// exactly as a non-compliant one does.
type DevicePosture struct {
	Managed   bool
	Compliant bool
}

// Principal is the unified authenticated identity.
//
// It is produced only by this package and is never constructed from request data. Downstream
// code treats it as a read-only fact; the accessors that return slices copy, so a handler
// cannot append a scope to the principal that authorized it.
type Principal struct {
	// Method is how this principal authenticated.
	Method Method
	// Type is the class of identity, which is what dual control, MFA freshness and the
	// tenant-scoping rules branch on.
	Type tenantctx.PrincipalType
	// ID is the stable subject: `cli_…`, the IdP subject, or the full SPIFFE ID.
	ID string
	// Name is display material for audit records.
	Name string

	// TenantID is the tenant this principal acts for. It is empty for a workload identity,
	// deliberately: a SPIFFE ID identifies the workload and never the tenant (security.md
	// §3.5), so `svc:internal` cannot satisfy a tenant-scoped read on its own authority and
	// must carry a propagated tenant context instead.
	TenantID shared.TenantID
	// TenantTier is carried so the tenant context can be built without a lookup on the hot
	// path.
	TenantTier shared.TenantTier
	// MerchantScope optionally narrows the principal to a subset of the tenant's merchants.
	// Empty means the whole tenant.
	MerchantScope []shared.MerchantID

	// Scopes are the OAuth scopes granted. A scope ending in `:*` covers everything below it.
	Scopes []string
	// Roles are the RBAC role names resolved from the token's role claim or the client's
	// configuration. Authorization reads these; authentication only carries them.
	Roles []string

	// Environment is the environment the credential is valid for. It must equal the
	// deployment's environment; that check happens here, at validation, so a sandbox
	// credential is rejected before it can touch production data.
	Environment shared.Environment

	// TokenID is the `jti` for a JWT, empty otherwise. Kept for replay accounting and so a
	// revocation can name exactly one token.
	TokenID string
	// ExpiresAt is when this authentication stops being valid.
	ExpiresAt time.Time
	// AuthTime is when the human actually authenticated, which is not the same as when the
	// token was issued: a refreshed token has a recent `iat` and an old `auth_time`. The MFA
	// freshness condition reads this one, because reading `iat` would let an attacker holding a
	// refresh token satisfy a freshness requirement by refreshing.
	AuthTime time.Time
	// MFA reports whether a second factor was used, from the IdP's `amr` claim.
	MFA bool
	// Device is the IdP's posture assertion.
	Device DevicePosture

	// ConfirmationThumbprint is the `cnf.x5t#S256` value of a sender-constrained token
	// (RFC 8705). Non-empty means the token is bound to a TLS client certificate and is
	// useless to anyone who does not hold that key.
	ConfirmationThumbprint string

	// TrustDomain is the SPIFFE trust domain for a workload identity.
	TrustDomain string
	// Service is the resolved internal service name for a workload identity.
	Service string

	// Revoked is re-checked per request against a ≤30 s-stale cache. It is a field on the
	// principal rather than a separate call so that authorization cannot forget to consult it.
	Revoked bool
}

// HasScope reports whether the principal holds a scope.
//
// The wildcard expands only in the *grant*, never in the requirement — the same rule as
// tenant.APIClient.HasScope, and for the same reason: a requirement wildcard would let a caller
// ask "does this principal have any payments scope" and be told yes for a read-only credential.
func (p *Principal) HasScope(scope string) bool {
	if p == nil || scope == "" {
		return false
	}
	want := strings.ToLower(strings.TrimSpace(scope))
	for _, granted := range p.Scopes {
		if granted == want {
			return true
		}
		if strings.HasSuffix(granted, ":*") {
			prefix := strings.TrimSuffix(granted, "*")
			if strings.HasPrefix(want, prefix) && len(want) > len(prefix) {
				return true
			}
		}
	}
	return false
}

// HasRole reports whether the principal holds a role.
func (p *Principal) HasRole(role string) bool {
	if p == nil {
		return false
	}
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// AllScopes returns a copy of the granted scopes.
func (p *Principal) AllScopes() []string {
	if p == nil {
		return nil
	}
	return append([]string(nil), p.Scopes...)
}

// AllRoles returns a copy of the resolved roles.
func (p *Principal) AllRoles() []string {
	if p == nil {
		return nil
	}
	return append([]string(nil), p.Roles...)
}

// Merchants returns a copy of the merchant scope.
func (p *Principal) Merchants() []shared.MerchantID {
	if p == nil {
		return nil
	}
	return append([]shared.MerchantID(nil), p.MerchantScope...)
}

// TenantContext builds the tenant context this principal implies.
//
// It is the single bridge from authentication to isolation, and it exists as a method so the
// authentication middleware has exactly one line to write and no opportunity to assemble the
// context from anything but the validated credential. A workload principal carries no tenant
// and is refused here: the caller must restore the tenant from the propagated context instead,
// which is the rule that stops an internal service from acting as an ambient super-user.
func (p *Principal) TenantContext(requestID, correlationID string) (tenantctx.TenantContext, error) {
	if p == nil {
		return tenantctx.TenantContext{}, apierror.New(apierror.CodeUnauthenticated, "")
	}
	if p.TenantID.IsZero() {
		return tenantctx.TenantContext{}, apierror.New(apierror.CodeMissingTenantContext,
			"this principal is not tenant-scoped; the tenant must be propagated explicitly")
	}
	tier := p.TenantTier
	if tier == "" {
		tier = shared.TierPooled
	}
	return tenantctx.TenantContext{
		TenantID:    p.TenantID,
		Tier:        tier,
		Environment: p.Environment,
		Principal: tenantctx.Principal{
			Type: p.Type,
			ID:   p.ID,
			Name: p.Name,
		},
		Scopes:        p.AllScopes(),
		MerchantScope: p.Merchants(),
		RequestID:     requestID,
		CorrelationID: correlationID,
		Source:        tenantctx.SourceToken,
	}, nil
}

// Reason is the machine-readable cause of an authentication failure.
//
// It never reaches the caller. It exists so that the log line and the
// pp_auth_failures_total{reason} metric can say precisely what happened while the response says
// only "unauthenticated" — which is what lets an operator debug an integration without handing
// an attacker a search oracle.
type Reason string

// The complete failure-reason set. Each one corresponds to a rule in security.md §3.3.
const (
	ReasonTokenTooLarge     Reason = "TOKEN_TOO_LARGE"
	ReasonMalformed         Reason = "MALFORMED"
	ReasonAlgNotAllowed     Reason = "ALG_NOT_ALLOWED"
	ReasonEmbeddedKey       Reason = "EMBEDDED_KEY_REJECTED"
	ReasonMissingKID        Reason = "MISSING_KID"
	ReasonUntrustedIssuer   Reason = "UNTRUSTED_ISSUER"
	ReasonUnknownKey        Reason = "UNKNOWN_KEY"
	ReasonAlgKeyMismatch    Reason = "ALG_KEY_MISMATCH"
	ReasonBadSignature      Reason = "BAD_SIGNATURE"
	ReasonBadAudience       Reason = "BAD_AUDIENCE"
	ReasonExpired           Reason = "EXPIRED"
	ReasonNotYetValid       Reason = "NOT_YET_VALID"
	ReasonStale             Reason = "STALE"
	ReasonIssuedInFuture    Reason = "ISSUED_IN_FUTURE"
	ReasonMissingJTI        Reason = "MISSING_JTI"
	ReasonMissingTenant     Reason = "MISSING_TENANT"
	ReasonEnvMismatch       Reason = "ENV_MISMATCH"
	ReasonReplayed          Reason = "TOKEN_REPLAYED"
	ReasonRevoked           Reason = "REVOKED"
	ReasonNotBoundToClient  Reason = "TOKEN_NOT_BOUND_TO_CLIENT"
	ReasonUnknownClient     Reason = "UNKNOWN_CLIENT"
	ReasonClientNotActive   Reason = "CLIENT_NOT_ACTIVE"
	ReasonBadSecret         Reason = "BAD_SECRET"
	ReasonSourceNotAllowed  Reason = "SOURCE_ADDRESS_NOT_ALLOWED"
	ReasonNoPeerCertificate Reason = "NO_PEER_CERTIFICATE"
	ReasonBadSPIFFEID       Reason = "BAD_SPIFFE_ID"
	ReasonWrongTrustDomain  Reason = "WRONG_TRUST_DOMAIN"
	ReasonUnknownWorkload   Reason = "UNKNOWN_WORKLOAD"
)

// authError is the unexported cause carrying the reason. It is unexported so that no caller can
// construct one and pretend a failure had a different cause than it did.
type authError struct{ reason Reason }

func (e *authError) Error() string { return "authn: " + string(e.reason) }

// reject builds the uniform 401. The reason is attached as the wrapped cause, which
// pkg/apierror logs and never serializes.
//
// Note that an expired token gets the same code as a forged one. apierror does register
// TOKEN_EXPIRED, and it is tempting to use it here because it is genuinely more helpful to a
// legitimate integration. It is not used, deliberately: the moment one failure is
// distinguishable from another, an attacker can partition the space of guesses, and the
// integration that needs the hint can read `exp` out of the token it is already holding.
func reject(reason Reason) *apierror.Error {
	return apierror.Wrap(&authError{reason: reason}, apierror.CodeInvalidToken, "")
}

// ReasonOf extracts the failure reason from an authentication error, for logging and metrics.
// It returns the empty Reason for anything that is not one of ours.
func ReasonOf(err error) Reason {
	var ae *authError
	if errors.As(err, &ae) {
		return ae.reason
	}
	return ""
}

// Observer receives authentication telemetry.
//
// Declared here, by the consumer, with two methods. The production implementation increments
// pp_auth_failures_total{reason} and pp_auth_replay_check_degraded_total; a nil Observer is a
// supported configuration so a test or a tool need not supply one.
type Observer interface {
	// AuthenticationFailed is called once per rejected credential.
	AuthenticationFailed(method Method, reason Reason)
	// ReplayCheckDegraded is called when the jti replay store was unreachable and the check was
	// therefore skipped. This is the one control in this package that fails open, and it is
	// the reason this method exists: the degradation must be loud even though it is not fatal.
	ReplayCheckDegraded()
}
