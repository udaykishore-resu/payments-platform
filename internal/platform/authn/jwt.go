package authn

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
)

// allowedAlgorithms is the algorithm allowlist. It is a package-level constant map, taken from
// configuration nowhere and from the token never.
//
// Two attacks die here, in one line:
//
//   - `alg: none`. A token with no signature is not in the allowlist, so it is rejected before
//     any verification is attempted. Historically this has been exploitable because a library's
//     "verify with the key in the header" path accepted `none` as a valid method.
//   - RS256 → HS256 key confusion. The classic form is: take a token signed with the issuer's
//     RSA private key, rewrite the header to `HS256`, and sign it with the issuer's *public* key
//     — which is, by definition, published — used as the HMAC secret. A verifier that reads the
//     algorithm from the header and looks up "the key for this kid" then verifies it happily.
//     Excluding every symmetric algorithm from the allowlist means there is no code path in
//     which a public key is interpreted as a shared secret, so the attack has no landing site.
//     This is stronger than "check that the key type matches the algorithm", because it removes
//     the branch rather than guarding it.
var allowedAlgorithms = map[string]bool{"ES256": true, "RS256": true}

// The JWT validation bounds from security.md §3.3.
const (
	// MaxClockSkew is the symmetric tolerance applied to exp, nbf and iat. Larger extends the
	// life of an expired token; smaller produces false rejections during an NTP step.
	MaxClockSkew = 60 * time.Second
	// MaxTokenAge bounds a token's age independently of its `exp`. It is a backstop against an
	// issuer misconfigured to mint eight-hour access tokens: our contract says fifteen minutes,
	// and this is the check that makes the contract enforceable at the consumer.
	MaxTokenAge = 15 * time.Minute
	// MaxTokenBytes bounds parsing work before any cryptography happens.
	MaxTokenBytes = 8 << 10
)

// Issuer is one entry in the issuer allowlist.
type Issuer struct {
	// Name is the exact `iss` value. Exact, because a prefix or suffix match on an issuer is a
	// cross-issuer confusion bug waiting for someone to register a lookalike.
	Name string
	// JWKSURL is where this issuer's keys live. It is configuration, never discovered from the
	// token.
	JWKSURL string
	// ExpectedAudience is compared for exact string equality against the token's single `aud`.
	ExpectedAudience string
	// MTLSBoundScopes lists the scopes for which a sender-constrained token is mandatory
	// (RFC 8705). security.md §3.1 requires it for `payments:refund`, `credentials:rotate` and
	// every control-plane write — the operations where a stolen bearer token is most expensive.
	MTLSBoundScopes []string
}

// ReplayStore records which token identifiers have been seen.
//
// One method, because the operation is genuinely atomic: "record this jti and tell me whether it
// was already there". Splitting it into a read and a write would make two concurrent replays
// both pass, which is precisely the case the store exists to catch.
type ReplayStore interface {
	// SeenBefore records the (issuer, jti) pair with a TTL of the token's remaining life and
	// reports whether it was already present.
	SeenBefore(ctx context.Context, issuer, jti string, expiry time.Time) (bool, error)
}

// RevocationChecker answers whether a token or its principal has been revoked.
//
// It is expected to be backed by a ≤30 s-stale cache with priority invalidation, not by a
// database read: this runs on every request, and a synchronous lookup here would put the
// control plane in the payment path, which the architecture forbids.
type RevocationChecker interface {
	IsRevoked(ctx context.Context, issuer, jti, subject string, tenant shared.TenantID) bool
}

// ValidatorConfig configures the JWT validator.
type ValidatorConfig struct {
	// Issuers is the allowlist. An `iss` absent from it is rejected before any key lookup.
	Issuers []Issuer
	// Environment is the deployment's environment. A token whose `env` claim differs is
	// rejected, which is what stops a sandbox credential authenticating against production.
	Environment shared.Environment
	// Keys resolves (issuer, kid) to a verification key.
	Keys *JWKS
	// Clock is injected so every time-dependent rule is testable without sleeping.
	Clock shared.Clock
	// Replay is optional. Absent, replay detection is not performed and every validation
	// records the degradation, so "we turned it off" is visible rather than assumed.
	Replay ReplayStore
	// Revocation is optional; absent, no token is treated as revoked.
	Revocation RevocationChecker
	// Observer is optional telemetry.
	Observer Observer
	// MaxTokenAge overrides the default backstop. Zero uses MaxTokenAge.
	MaxTokenAge time.Duration
}

// Claims is the token payload this platform understands.
//
// Fields beyond the registered set are the platform's own contract (security.md §3.1). The two
// that carry security weight are `tenant_id`, which is the *only* source of tenant identity, and
// `env`, which is the only thing preventing a sandbox credential from working in production.
type Claims struct {
	jwt.RegisteredClaims

	// TenantID is `ten_…`. Baseline §16.2: this claim, and nothing else, decides the tenant.
	TenantID string `json:"tenant_id"`
	// TenantTier lets the tenant context be built without a lookup.
	TenantTier string `json:"tenant_tier"`
	// Scope is the space-delimited OAuth scope string.
	Scope string `json:"scope"`
	// MerchantScope optionally narrows the credential to specific merchants.
	MerchantScope []string `json:"merchant_scope"`
	// Env must equal the deployment environment.
	Env string `json:"env"`
	// Roles are the RBAC roles. For humans they are derived by the issuer from an explicit
	// group→role mapping table, never from a free-form IdP group name.
	Roles []string `json:"roles"`
	// Name is display material for audit records.
	Name string `json:"name"`
	// AuthTime is when the human actually authenticated (OIDC `auth_time`), which the MFA
	// freshness condition reads instead of `iat`.
	AuthTime int64 `json:"auth_time"`
	// AMR is the OIDC authentication-methods-references claim; a second factor appears here.
	AMR []string `json:"amr"`
	// Confirmation carries the mTLS certificate thumbprint of a sender-constrained token.
	Confirmation *Confirmation `json:"cnf"`
	// Device is the IdP's device-posture assertion.
	Device *DeviceClaim `json:"device"`
}

// Confirmation is RFC 8705's `cnf` claim. Only the certificate thumbprint form is supported;
// other confirmation methods are ignored rather than half-understood.
type Confirmation struct {
	X5TS256 string `json:"x5t#S256"`
}

// DeviceClaim is the IdP's assertion about the device a human is using.
type DeviceClaim struct {
	Managed   bool `json:"managed"`
	Compliant bool `json:"compliant"`
}

// Validator validates access tokens.
//
// It is safe for concurrent use and holds no per-request state.
type Validator struct {
	cfg     ValidatorConfig
	issuers map[string]Issuer
	maxAge  time.Duration
}

// NewValidator builds a validator, failing fast on a configuration that cannot be safe.
//
// The constructor is strict on purpose. Every one of these misconfigurations produces a
// validator that accepts tokens it should not, and every one of them is silent at runtime: no
// issuers means nothing validates, no environment means the sandbox check is vacuous, no key
// source means signature verification has nothing to verify against. Failing at construction
// puts the error in a deploy log instead of in an incident report.
func NewValidator(cfg ValidatorConfig) (*Validator, error) {
	if len(cfg.Issuers) == 0 {
		return nil, apierror.New(apierror.CodeInternalError, "the JWT validator requires at least one trusted issuer")
	}
	if !cfg.Environment.IsValid() {
		return nil, apierror.Newf(apierror.CodeInternalError, "the JWT validator requires a valid environment, got %q", cfg.Environment)
	}
	if cfg.Keys == nil {
		return nil, apierror.New(apierror.CodeInternalError, "the JWT validator requires a key source")
	}
	if cfg.Clock == nil {
		cfg.Clock = shared.SystemClock{}
	}
	v := &Validator{
		cfg:     cfg,
		issuers: make(map[string]Issuer, len(cfg.Issuers)),
		maxAge:  cfg.MaxTokenAge,
	}
	if v.maxAge <= 0 {
		v.maxAge = MaxTokenAge
	}
	for _, iss := range cfg.Issuers {
		if iss.Name == "" || iss.ExpectedAudience == "" {
			return nil, apierror.New(apierror.CodeInternalError,
				"every trusted issuer requires a name and an expected audience")
		}
		v.issuers[iss.Name] = iss
		cfg.Keys.Register(iss.Name, iss.JWKSURL)
	}
	return v, nil
}

// Validate turns a raw bearer token into a Principal, or returns the uniform 401.
//
// # The order of the checks is part of the design
//
// Cheap structural rejections precede cryptography, which precedes anything that could touch the
// network. Concretely: a length check before parsing, an algorithm check before key resolution,
// an issuer allowlist check before key resolution, and signature verification before any claim
// is believed. The ordering is not an optimization — it is what makes each check unable to be
// used as a lever against the one after it. The clearest case is the issuer: resolving `iss`
// against the allowlist *before* looking up a key is what stops an attacker-chosen issuer from
// driving an outbound request, which would be a server-side request forgery with the identity
// provider's credibility attached.
func (v *Validator) Validate(ctx context.Context, raw string) (*Principal, error) {
	p, err := v.validate(ctx, raw)
	if err != nil {
		if v.cfg.Observer != nil {
			v.cfg.Observer.AuthenticationFailed(MethodJWT, ReasonOf(err))
		}
		return nil, err
	}
	return p, nil
}

func (v *Validator) validate(ctx context.Context, raw string) (*Principal, error) {
	raw = strings.TrimPrefix(raw, "Bearer ")
	raw = strings.TrimPrefix(raw, "bearer ")
	if raw == "" {
		return nil, reject(ReasonMalformed)
	}
	// 0. Size, before any parsing. A multi-megabyte "token" is a cheap way to make us allocate
	//    and base64-decode on an unauthenticated path.
	if len(raw) > MaxTokenBytes {
		return nil, reject(ReasonTokenTooLarge)
	}

	// 1. The header, decoded and checked before any cryptography and before the library sees
	//    the token. Doing this here rather than inside the key function is not defensiveness
	//    for its own sake: it means the algorithm allowlist, the embedded-key rejection and the
	//    kid requirement are evaluated in a fixed order that no library's internal ordering can
	//    change under us, and it means an attacker-chosen header costs one base64 decode.
	hdr, err := decodeHeader(raw)
	if err != nil {
		return nil, reject(ReasonMalformed)
	}
	// 1a. Algorithm. The header is attacker-controlled, so only the allowlist decides.
	if !allowedAlgorithms[hdr.Alg] {
		return nil, reject(ReasonAlgNotAllowed)
	}
	// 1b. A token may not nominate the key that verifies it. `jwk` embeds one outright; `jku`
	//     and `x5u` point at one; `x5c` carries a chain. All of them turn signature
	//     verification into a tautology — of course it verifies, the token chose the key — and
	//     the pointer forms are an SSRF primitive as well. All are refused rather than
	//     "validated".
	if hdr.nominatesKey() {
		return nil, reject(ReasonEmbeddedKey)
	}
	// 1c. Without a kid there is nothing to resolve, and "try every key" is how a verifier
	//     ends up accepting a token signed by a key retired for cause.
	if hdr.Kid == "" {
		return nil, reject(ReasonMissingKID)
	}

	var claims Claims
	// WithValidMethods enforces the allowlist inside the library as well, so a future refactor
	// of the checks above cannot accidentally re-open `alg: none`. Claims validation is turned
	// off because the library's exp/nbf handling does not implement our skew and max-age rules;
	// doing it here keeps all the time reasoning in one readable block.
	parser := jwt.NewParser(
		jwt.WithValidMethods(allowedAlgorithmNames()),
		jwt.WithoutClaimsValidation(),
	)

	var keyErr error
	tok, err := parser.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		// 2. Issuer allowlist BEFORE the key lookup. An unknown `iss` must never cause an
		//    outbound request, and it must never reach the key cache at all.
		if _, ok := v.issuers[claims.Issuer]; !ok {
			keyErr = reject(ReasonUntrustedIssuer)
			return nil, keyErr
		}
		key, err := v.cfg.Keys.Key(ctx, claims.Issuer, hdr.Kid)
		if err != nil {
			keyErr = reject(ReasonUnknownKey)
			return nil, keyErr
		}
		// 3. The key's declared algorithm must match the header's. A key published as ES256 may
		//    not be used to verify an RS256 token, even though both are on the allowlist.
		if key.Algorithm != hdr.Alg {
			keyErr = reject(ReasonAlgKeyMismatch)
			return nil, keyErr
		}
		return key.Public, nil
	})
	if keyErr != nil {
		return nil, keyErr
	}
	if err != nil || tok == nil || !tok.Valid {
		// Everything that reaches here is either a malformed token or a bad signature. They are
		// distinguished for the log only; the caller sees the same 401 either way.
		if errors.Is(err, jwt.ErrTokenSignatureInvalid) {
			return nil, reject(ReasonBadSignature)
		}
		return nil, reject(ReasonMalformed)
	}

	// 4. The signature is verified. Only now is any claim believed.
	issuer := v.issuers[claims.Issuer]
	now := v.cfg.Clock.Now()

	// 5. Audience: exact equality against exactly one value. A `strings.Contains` check, or
	//    accepting a token whose audience list merely *includes* ours, is a published
	//    cross-service replay bug: a token minted for a lower-value service is then good here.
	if len(claims.Audience) != 1 || claims.Audience[0] != issuer.ExpectedAudience {
		return nil, reject(ReasonBadAudience)
	}

	// 6. Time. ±60 s of skew, applied symmetrically, plus the independent max-age backstop.
	switch {
	case claims.ExpiresAt == nil:
		return nil, reject(ReasonExpired)
	case now.After(claims.ExpiresAt.Add(MaxClockSkew)):
		return nil, reject(ReasonExpired)
	case claims.NotBefore != nil && now.Add(MaxClockSkew).Before(claims.NotBefore.Time):
		return nil, reject(ReasonNotYetValid)
	case claims.IssuedAt == nil:
		return nil, reject(ReasonStale)
	case now.Sub(claims.IssuedAt.Time) > v.maxAge+MaxClockSkew:
		return nil, reject(ReasonStale)
	case claims.IssuedAt.After(now.Add(MaxClockSkew)):
		return nil, reject(ReasonIssuedInFuture)
	}

	// 7. Identity claims the platform requires.
	if claims.ID == "" {
		// Without a jti there is nothing to replay-check and nothing to revoke individually, so
		// a token without one is not a token this platform can police.
		return nil, reject(ReasonMissingJTI)
	}
	if claims.TenantID == "" || ids.Validate(claims.TenantID, ids.PrefixTenant) != nil {
		return nil, reject(ReasonMissingTenant)
	}
	// 8. Environment. A sandbox token must not authenticate against production, and a
	//    production token must not be usable in sandbox either — the check is equality, not a
	//    one-way "production is stricter" rule, because a production credential leaking into a
	//    lower-assurance environment is also an incident.
	if shared.Environment(claims.Env) != v.cfg.Environment {
		return nil, reject(ReasonEnvMismatch)
	}

	// 9. Replay. A captured token is single-use for its remaining lifetime.
	//
	//    This is the one control in this package that fails open. If the replay store is
	//    unreachable, the check is skipped and the degradation is recorded, because a Redis
	//    outage must not stop payments (baseline §24) — and because the token is still
	//    signature-valid, still unexpired, still audience-correct and still revocation-checked,
	//    so skipping replay detection loses one layer rather than the wall.
	if v.cfg.Replay != nil {
		seen, err := v.cfg.Replay.SeenBefore(ctx, claims.Issuer, claims.ID, claims.ExpiresAt.Time)
		switch {
		case err != nil:
			if v.cfg.Observer != nil {
				v.cfg.Observer.ReplayCheckDegraded()
			}
		case seen:
			return nil, reject(ReasonReplayed)
		}
	}

	// 10. Revocation, at both token and principal level.
	if v.cfg.Revocation != nil &&
		v.cfg.Revocation.IsRevoked(ctx, claims.Issuer, claims.ID, claims.Subject, shared.TenantID(claims.TenantID)) {
		return nil, reject(ReasonRevoked)
	}

	scopes := splitScope(claims.Scope)

	// 11. Sender constraining, where the scope demands it. The thumbprint of the TLS client
	//     certificate on *this* connection must equal the one the token was bound to; a stolen
	//     bearer token is then useless without the corresponding private key.
	if requiresBinding(issuer.MTLSBoundScopes, scopes) {
		want := ""
		if claims.Confirmation != nil {
			want = claims.Confirmation.X5TS256
		}
		got := PeerThumbprint(ctx)
		if want == "" || got == "" || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
			return nil, reject(ReasonNotBoundToClient)
		}
	}

	return v.principal(claims, scopes), nil
}

func (v *Validator) principal(c Claims, scopes []string) *Principal {
	ptype := tenantPrincipalType(c)
	p := &Principal{
		Method:      MethodJWT,
		Type:        ptype,
		ID:          c.Subject,
		Name:        c.Name,
		TenantID:    shared.TenantID(c.TenantID),
		TenantTier:  shared.TenantTier(c.TenantTier),
		Scopes:      scopes,
		Roles:       append([]string(nil), c.Roles...),
		Environment: shared.Environment(c.Env),
		TokenID:     c.ID,
	}
	if !shared.TenantTier(c.TenantTier).IsValid() {
		p.TenantTier = shared.TierPooled
	}
	if c.ExpiresAt != nil {
		p.ExpiresAt = c.ExpiresAt.Time
	}
	for _, m := range c.MerchantScope {
		p.MerchantScope = append(p.MerchantScope, shared.MerchantID(m))
	}
	if c.AuthTime > 0 {
		p.AuthTime = time.Unix(c.AuthTime, 0).UTC()
	} else if c.IssuedAt != nil {
		// Fall back to `iat` so the freshness condition has something to measure. Machine
		// clients have no separate authentication event, and for them the two are the same
		// instant anyway.
		p.AuthTime = c.IssuedAt.Time
	}
	for _, m := range c.AMR {
		switch m {
		case "mfa", "hwk", "otp", "swk", "pop", "webauthn", "fido":
			p.MFA = true
		}
	}
	if c.Device != nil {
		p.Device = DevicePosture{Managed: c.Device.Managed, Compliant: c.Device.Compliant}
	}
	if c.Confirmation != nil {
		p.ConfirmationThumbprint = c.Confirmation.X5TS256
	}
	return p
}

// tenantPrincipalType classifies the token's subject. The convention is the identifier prefix:
// a `cli_` subject is a machine client, anything else from a trusted issuer is a human. It is
// deliberately not derived from the roles, which are authorization data and could be empty.
func tenantPrincipalType(c Claims) tenantctx.PrincipalType {
	if strings.HasPrefix(c.Subject, string(ids.PrefixAPIClient)+"_") {
		return tenantctx.PrincipalMachine
	}
	return tenantctx.PrincipalHuman
}

func requiresBinding(boundScopes, granted []string) bool {
	for _, b := range boundScopes {
		for _, g := range granted {
			if g == b {
				return true
			}
		}
	}
	return false
}

func splitScope(s string) []string {
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, strings.ToLower(f))
	}
	return out
}

func allowedAlgorithmNames() []string {
	// Sorted for determinism; the set is two entries and this runs once per validation.
	names := make([]string, 0, len(allowedAlgorithms))
	for _, a := range []string{"ES256", "RS256"} {
		if allowedAlgorithms[a] {
			names = append(names, a)
		}
	}
	return names
}

// --- peer thumbprint plumbing ---------------------------------------------------------------

type peerThumbprintKey struct{}

// WithPeerThumbprint records the SHA-256 thumbprint of the TLS client certificate on this
// connection, so the sender-constraining check can compare it against the token's `cnf` claim.
//
// It is set by the transport, from the *verified* connection state, never from a header.
// `X-Forwarded-Client-Cert` is trustworthy only when it arrives on a connection whose peer is
// our own sidecar, and deciding that is the transport's job, not this package's.
func WithPeerThumbprint(ctx context.Context, thumbprint string) context.Context {
	return context.WithValue(ctx, peerThumbprintKey{}, thumbprint)
}

// PeerThumbprint returns the recorded client-certificate thumbprint, or the empty string.
func PeerThumbprint(ctx context.Context) string {
	s, _ := ctx.Value(peerThumbprintKey{}).(string)
	return s
}

// Thumbprint computes the RFC 8705 `x5t#S256` value of a DER-encoded certificate: the
// base64url, unpadded SHA-256 of the certificate bytes.
func Thumbprint(der []byte) string {
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// tokenHeader is the JOSE header, decoded before the token reaches the JWT library.
type tokenHeader struct {
	Alg string          `json:"alg"`
	Kid string          `json:"kid"`
	JWK json.RawMessage `json:"jwk"`
	JKU json.RawMessage `json:"jku"`
	X5U json.RawMessage `json:"x5u"`
	X5C json.RawMessage `json:"x5c"`
}

// nominatesKey reports whether the header tries to supply or point at its own verification key.
func (h tokenHeader) nominatesKey() bool {
	return h.JWK != nil || h.JKU != nil || h.X5U != nil || h.X5C != nil
}

// decodeHeader splits a compact JWS and decodes its first segment.
//
// It requires exactly three segments. A two-segment token is an unsecured JWS, which this
// platform does not accept in any form, and a five-segment one is JWE — a different format
// whose "decrypt then trust" shape is not what any caller here expects.
func decodeHeader(raw string) (tokenHeader, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return tokenHeader{}, errors.New("authn: token is not a three-part compact JWS")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return tokenHeader{}, err
	}
	// Bound the header before unmarshalling: a header is a few hundred bytes, and the token
	// size cap already applies, but decoding a deliberately pathological JSON header is work we
	// need not do.
	if len(b) > 4096 {
		return tokenHeader{}, errors.New("authn: token header is implausibly large")
	}
	var h tokenHeader
	if err := json.Unmarshal(b, &h); err != nil {
		return tokenHeader{}, err
	}
	return h, nil
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: FR-04, NFR-30.
//
// Bearer-token authentication and the algorithm, audience, issuer and skew constraints on it
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
