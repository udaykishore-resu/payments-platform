// Package secrets implements ports.SecretsProvider twice behind one port: a file/environment
// backed store for local development, tests and the gateway simulator, and an AWS Secrets
// Manager client written directly against net/http.
//
// # Why there is no AWS SDK here
//
// Four API calls are made against Secrets Manager — GetSecretValue, CreateSecret/PutSecretValue,
// UpdateSecretVersionStage and DeleteSecret — plus one against STS for the IRSA web-identity
// exchange. The official SDK brings roughly twenty transitive modules to serve them. On a
// platform whose supply-chain posture is written down (docs/security.md §5.2: no secret in an
// image, every layer scanned, every dependency justified) that is not a neutral cost: each
// module is a package that runs in the process that holds gateway credentials, and each is a
// thing to patch on a Friday. SigV4 is a two-hundred-line HMAC construction with a published
// test suite; implementing it and asserting against those vectors buys the same assurance the
// SDK would, with a dependency count of zero.
//
// The trade-off, stated plainly because it is real: the SDK also brings retry classification,
// endpoint resolution, and credential-chain behaviour that we now own. Those are implemented
// here — sigv4.go, awssm.go's credential chain, the throttling retry — and they are the parts
// most likely to need attention when AWS changes something.
//
// # What never leaves this package
//
// Plaintext credential material exists inside this package as a bare string for exactly as long
// as it takes to move it into a [Material], and nowhere else. Every error returned from here is
// constructed from the reference, the HTTP status and the AWS error code — never from a response
// body, which for GetSecretValue *is* the secret.
package secrets

import (
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Scheme is the URI scheme every credential reference in this platform carries.
//
// It is a constant rather than a literal because the domain's ValidateSecretRef, the L4
// configuration rules and this parser must agree on exactly one token; a configuration that
// passed validation and then failed resolution because two packages spelled the scheme
// differently is a defect that only appears in production.
const Scheme = "secret://"

// The reference grammar, from docs/control-plane.md §5.2:
//
//	secret://{environment}/{tenant_id}/{merchant_id}/{gateway_id}[/{purpose}][#v{n}]
//
// Two shorter forms are in circulation and are accepted, because the onboarding workflow emits
// them (internal/workflows/onboarding.SecretRef and SigningSecretRef) and a parser that refused
// them would fail every merchant provisioned before the tenant segment was added:
//
//	secret://{environment}/{merchant_id}/{gateway_id}
//	secret://{environment}/{merchant_id}/{gateway_id}/{purpose}
//
// Disambiguation is by shape, not by position alone: a three-segment path whose first segment
// carries the tenant prefix is (tenant, merchant, gateway); otherwise it is
// (merchant, gateway, purpose). Four segments are always the full form. The alternative — a
// strict positional grammar — would have required rewriting every stored reference, which is a
// data migration performed to satisfy a parser.
const (
	tenantPrefix = "ten_"
	maxSegments  = 4
)

// Reference is a parsed `secret://` URI.
//
// It is a value type with exported fields because it contains no material — only identifiers
// that docs/control-plane.md §5.2 explicitly documents as safe in logs, audit records and
// support tickets. That safety is the reason the platform passes references around instead of
// credentials, and it is why this struct may be printed.
type Reference struct {
	// Environment is the first path segment and the one that makes the classic "sandbox key in
	// production" incident structurally impossible: [Reference.Validate] refuses a reference
	// whose environment is not the process's own.
	Environment shared.Environment
	// TenantID is empty for the legacy two- and three-segment forms. When it is present it is
	// checked against the caller's tenant context, which is the control that stops a compromised
	// merchant configuration from naming another tenant's credential path.
	TenantID string
	// MerchantID and GatewayID are always present.
	MerchantID string
	GatewayID  string
	// Purpose distinguishes the several credentials one connection can hold — `api_key`,
	// `webhook_hmac`, `oauth_client_secret`. Empty means the connection's primary credential.
	Purpose string
	// Version pins a specific stored version. Empty means "whatever is current", which is what
	// the payment path always wants: pinning on the hot path would make a rotation invisible
	// until the next deploy.
	Version string
}

// ParseReference parses and validates a `secret://` reference.
//
// The validation is not cosmetic. A secret reference is a path that this process turns into an
// IAM-scoped lookup, and the three classes of input it refuses are the three that turn a
// reference into a traversal:
//
//   - `..` or `.` in any segment, which walks out of the merchant's IAM path prefix. The IAM
//     policy grants `/{env}/{tenant}/{merchant}/*`; a reference containing `..` that some future
//     path-joining code normalises would resolve outside it.
//   - `*` or `?`, which in a Secrets Manager `ListSecrets` filter or an IAM resource ARN is a
//     wildcard. A reference of `secret://prod/ten_x/*/stripe` must never become a query for
//     every merchant's Stripe key.
//   - Empty segments and control characters, which produce a path that means something
//     different to the parser than to the store.
func ParseReference(raw string) (Reference, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Reference{}, refErr(raw, "MISSING_SECRET_REF", "a credential reference is required")
	}
	if !strings.HasPrefix(s, Scheme) {
		return Reference{}, refErr(raw, "NOT_A_SECRET_REF",
			"a credential reference must be a secret:// URI, never credential material")
	}
	body := strings.TrimPrefix(s, Scheme)

	var version string
	if i := strings.IndexByte(body, '#'); i >= 0 {
		body, version = body[:i], body[i+1:]
		v, err := parseVersion(raw, version)
		if err != nil {
			return Reference{}, err
		}
		version = v
	}
	if body == "" {
		return Reference{}, refErr(raw, "EMPTY_SECRET_PATH", "the secret reference has an empty path")
	}

	segs := strings.Split(body, "/")
	for _, seg := range segs {
		if err := validateSegment(raw, seg); err != nil {
			return Reference{}, err
		}
	}
	if len(segs) < 3 || len(segs) > 1+maxSegments {
		return Reference{}, refErr(raw, "SECRET_REF_SHAPE",
			"expected secret://{env}/{tenant}/{merchant}/{gateway}[/{purpose}]")
	}

	env, err := shared.ParseEnvironment(segs[0])
	if err != nil {
		return Reference{}, refErr(raw, "SECRET_REF_ENVIRONMENT",
			"the first path segment must be sandbox or production")
	}
	ref := Reference{Environment: env, Version: version}

	rest := segs[1:]
	switch {
	case len(rest) == 4, len(rest) == 3 && strings.HasPrefix(rest[0], tenantPrefix):
		ref.TenantID, rest = rest[0], rest[1:]
	}
	ref.MerchantID, ref.GatewayID = rest[0], rest[1]
	if len(rest) > 2 {
		ref.Purpose = rest[2]
	}
	return ref, nil
}

// parseVersion accepts the two fragment forms the platform actually writes.
//
// `v{n}` is the reference scheme's own version pin. The three AWS staging labels are accepted
// because the rotation workflow's verification step resolves a staged-but-not-current version,
// and expressing that as `#AWSPENDING` is clearer at the call site than inventing a synonym —
// and clearer in an audit record, where an operator reading it already knows what AWSPENDING
// means.
func parseVersion(raw, v string) (string, error) {
	switch v {
	case StageCurrent, StagePrevious, StagePending:
		return v, nil
	}
	if len(v) < 2 || v[0] != 'v' {
		return "", refErr(raw, "SECRET_REF_VERSION", "a version fragment must be #v{n} or an AWS staging label")
	}
	for _, c := range v[1:] {
		if c < '0' || c > '9' {
			return "", refErr(raw, "SECRET_REF_VERSION", "a version fragment must be #v{n} or an AWS staging label")
		}
	}
	return v, nil
}

// validateSegment refuses the inputs that turn a reference into a traversal or a wildcard.
func validateSegment(raw, seg string) error {
	if seg == "" {
		return refErr(raw, "EMPTY_SECRET_PATH", "the secret reference has an empty path segment")
	}
	if seg == "." || seg == ".." || strings.Contains(seg, "..") {
		return refErr(raw, "SECRET_REF_TRAVERSAL",
			"a secret reference may not contain a path traversal")
	}
	if strings.ContainsAny(seg, "*?[]") {
		return refErr(raw, "SECRET_REF_WILDCARD",
			"a secret reference may not contain a wildcard; it names one credential, never a set")
	}
	for _, c := range seg {
		if c < 0x21 || c == 0x7f {
			return refErr(raw, "SECRET_REF_CHARSET",
				"a secret reference may not contain whitespace or control characters")
		}
	}
	return nil
}

// Validate checks a parsed reference against the process's own environment and the caller's
// tenant.
//
// # Why both checks live here rather than at the call sites
//
// The environment check is docs/control-plane.md §5.2's structural guarantee: a sandbox
// reference can never resolve in production because the resolver refuses it, not because
// deployment happens to keep them apart.
//
// The tenant check is the one that stops the attack this platform's multi-tenancy actually has
// to survive. A merchant configuration is data the tenant supplies; `secretRef` is a field on
// it. Without this check, a compromised or malicious configuration for tenant A naming
// `secret://production/ten_B/mrc_.../stripe` would be resolved by a process holding an IAM role
// broad enough to read it, and tenant A would receive tenant B's gateway credentials in a
// dispatch. Checking at resolution — the one place every path funnels through — is the only
// placement that cannot be forgotten by a new caller.
//
// An empty callerTenant means "no tenant context", which is legal for the rotation workflow and
// the operator CLI and is refused for anything carrying a tenant segment, so the permissive case
// cannot be reached by an untrusted caller: the payment path always has a tenant.
func (r Reference) Validate(env shared.Environment, callerTenant shared.TenantID) error {
	if r.Environment != env {
		return apierror.Newf(apierror.CodeForbidden,
			"the credential reference names environment %s but this process runs %s", r.Environment, env).
			WithDetail(apierror.Detail{
				Field: "credentialRef", Code: "SECRET_REF_ENVIRONMENT_MISMATCH",
				Message: "A credential reference resolves only inside its own environment.",
				RuleID:  "L4.CREDENTIAL_IS_A_REFERENCE",
			})
	}
	if r.TenantID == "" {
		return nil
	}
	if callerTenant == "" {
		return apierror.New(apierror.CodeMissingTenantContext,
			"a tenant-scoped credential reference cannot be resolved without a tenant context").
			WithDetail(apierror.Detail{
				Field: "credentialRef", Code: "SECRET_REF_TENANT_UNKNOWN",
				Message: "This reference names a tenant; resolve it from a request that has one.",
				RuleID:  "L4.CREDENTIAL_IS_A_REFERENCE",
			})
	}
	if r.TenantID != callerTenant.String() {
		// The message names neither tenant. A cross-tenant probe that gets told *which* tenant
		// owns the path has learned something; one that gets told only that it may not have it
		// has not.
		return apierror.New(apierror.CodeTenantMismatch,
			"the credential reference belongs to a different tenant than the caller").
			WithDetail(apierror.Detail{
				Field: "credentialRef", Code: "SECRET_REF_TENANT_MISMATCH",
				Message: "A credential reference may only name the caller's own tenant.",
				RuleID:  "L4.CREDENTIAL_IS_A_REFERENCE",
			})
	}
	return nil
}

// String renders the reference back into its canonical URI form.
//
// Round-tripping matters because the reference is the cache key, the audit-record field and the
// metadata row's `secret_ref`. Two spellings of one credential would produce two cache entries,
// and a rotation that invalidated one of them would leave the other serving the old material
// past the overlap window.
func (r Reference) String() string {
	var b strings.Builder
	b.WriteString(Scheme)
	b.WriteString(string(r.Environment))
	for _, seg := range []string{r.TenantID, r.MerchantID, r.GatewayID, r.Purpose} {
		if seg == "" {
			continue
		}
		b.WriteByte('/')
		b.WriteString(seg)
	}
	if r.Version != "" {
		b.WriteByte('#')
		b.WriteString(r.Version)
	}
	return b.String()
}

// WithVersion returns a copy pinned to v. Used by the rotation workflow, which must read back
// exactly the version it staged rather than whatever is current at the moment it asks.
func (r Reference) WithVersion(v string) Reference { r.Version = v; return r }

// Base returns the reference without its version pin. It is the cache key and the Secrets
// Manager secret id: versions are addressed inside one secret, not as separate secrets.
func (r Reference) Base() Reference { r.Version = ""; return r }

// SecretID renders the reference as the hierarchical name the secret carries in the store.
//
// It mirrors the IAM path scheme in docs/security.md §5.1 exactly — `/{env}/{tenant}/{merchant}/
// {gateway}/{purpose}` — and that mirroring is the whole security design: an IAM policy with a
// path-prefix condition grants one service account exactly one merchant's credentials, so least
// privilege falls out of the naming rather than requiring a policy per secret. Changing this
// function silently widens or breaks every deployed IAM policy.
func (r Reference) SecretID() string {
	var b strings.Builder
	b.WriteByte('/')
	b.WriteString(string(r.Environment))
	for _, seg := range []string{r.TenantID, r.MerchantID, r.GatewayID, r.Purpose} {
		if seg == "" {
			continue
		}
		b.WriteByte('/')
		b.WriteString(seg)
	}
	return b.String()
}

// refErr builds the platform's validation error for a malformed reference.
//
// The offending reference is included, and that is safe by construction: docs/control-plane.md
// §5.2 requires a reference to contain no secret-derived data, which is what makes it printable
// in a log, a support ticket and this error.
func refErr(raw, code, msg string) error {
	return apierror.Newf(apierror.CodeValidationFailed, "%s: %q", msg, raw).
		WithDetail(apierror.Detail{
			Field: "credentialRef", Code: code, Message: msg,
			RuleID: "L4.CREDENTIAL_IS_A_REFERENCE",
		})
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: NFR-32, FR-40.
//
// The secret:// reference grammar, its traversal and wildcard rejection, and the tenant and
// environment scoping that make cross-tenant credential resolution impossible
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
