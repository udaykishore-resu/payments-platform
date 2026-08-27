package l1api

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
	"github.com/udaykishore-resu/payments-platform/internal/validation/engine"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/internal/ruledef"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

func init() {
	ruledef.Register(defs(DefaultDeps()), "platform-edge", "2026-01-01", engine.Enforce)
}

// PhaseARules is the ShortCircuit prefix of L1: transport, authentication, tenancy,
// authorization and rate limiting.
//
// ShortCircuit, and in this order, for a security reason rather than a performance one. After
// the token's signature fails to verify there is no authenticated subject, so every later rule
// would be evaluating attacker-controlled input and answering questions through its error
// messages — which fields exist, which tenants exist, which merchants exist. Stopping at the
// first failure is what makes the edge silent about everything past the point of rejection.
func PhaseARules(d Deps) engine.RuleSet[Subject] {
	return engine.RuleSet[Subject]{
		Name:  "L1.phaseA",
		Mode:  engine.ShortCircuit,
		Rules: ruledef.Build(phaseA(d)),
	}
}

// PhaseBRules is the CollectAll remainder of L1: schema, types, bounds, the PCI detectors and
// the idempotency header.
//
// CollectAll because the reader is an integrator fixing a request. Nine failures returned one
// at a time is nine deploy cycles on their side, and by the third they have opened a ticket.
// Running all of them costs less than one round trip: every rule here is pure and operates on
// an already-decoded document.
func PhaseBRules(d Deps) engine.RuleSet[Subject] {
	return engine.RuleSet[Subject]{
		Name:  "L1.phaseB",
		Mode:  engine.CollectAll,
		Rules: ruledef.Build(phaseB(d)),
	}
}

// Rules returns both phases as one set, for callers that evaluate L1 in a single pass — the
// documentation generator, the catalog test and the benchmark.
//
// It is CollectAll, and production code must not use it for a request: running phase B on a
// request whose authentication failed is precisely the leak the phase split prevents. The
// edge middleware calls PhaseARules, checks OK, and only then calls PhaseBRules.
func Rules(d Deps) engine.RuleSet[Subject] {
	return engine.RuleSet[Subject]{
		Name:  "L1.api",
		Mode:  engine.CollectAll,
		Rules: ruledef.Build(defs(d)),
	}
}

func defs(d Deps) []ruledef.Def[Subject] {
	return append(phaseA(d), phaseB(d)...)
}

func phaseA(d Deps) []ruledef.Def[Subject] {
	bearer := func(s Subject) bool { return s.Auth == AuthBearer }
	authed := func(s Subject) bool { return s.Route.Authenticated }

	return []ruledef.Def[Subject]{
		{
			ID: "L1.TLS_VERSION_AT_LEAST_1_2", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "connection", Pure: true,
			Desc:        "the connection negotiated TLS 1.2 or later",
			Remediation: "Upgrade your TLS client. This API requires TLS 1.2 or later.",
			Check: func(s Subject) string {
				if s.TLSVersion >= TLS12 {
					return ""
				}
				return "the connection negotiated a TLS version older than 1.2"
			},
		},
		{
			ID: "L1.CONTENT_TYPE_IS_JSON", Severity: engine.Error,
			Code: string(apierror.CodeUnsupportedMediaType), Field: "Content-Type", Pure: true,
			Desc:        "Content-Type is application/json, optionally with charset=utf-8",
			Remediation: "Set `Content-Type: application/json`.",
			Applies:     func(s Subject) bool { return s.Route.HasBody },
			Check: func(s Subject) string {
				ct := strings.ToLower(strings.TrimSpace(s.header("Content-Type")))
				base, params, _ := strings.Cut(ct, ";")
				if strings.TrimSpace(base) != "application/json" {
					return "Content-Type is " + quoteOrEmpty(ct) + ", not application/json"
				}
				params = strings.TrimSpace(params)
				if params != "" && !strings.HasPrefix(params, "charset=utf-8") {
					return "the only accepted Content-Type parameter is charset=utf-8"
				}
				return ""
			},
		},
		{
			ID: "L1.BODY_WITHIN_SIZE_LIMIT", Severity: engine.Error,
			Code: string(apierror.CodeRequestTooLarge), Field: "body", Pure: true,
			Desc:        "the request body is within the route's size ceiling",
			Remediation: "Request body exceeds the permitted size. Split the request or remove metadata.",
			Applies:     func(s Subject) bool { return s.Route.HasBody },
			Check: func(s Subject) string {
				limit := d.MaxBodyBytes
				if s.Route.IsWebhook && d.MaxWebhookBodyBytes > limit {
					limit = d.MaxWebhookBodyBytes
				}
				if limit <= 0 || len(s.RawBody) <= limit {
					return ""
				}
				return "body is " + itoa(len(s.RawBody)) + " bytes; the limit is " + itoa(limit)
			},
		},
		{
			ID: "L1.BODY_IS_WELL_FORMED_JSON", Severity: engine.Error,
			Code: string(apierror.CodeMalformedRequest), Field: "body", Pure: true,
			Desc:        "the body parses as a JSON object, not an array and not a scalar",
			Remediation: "Body must be a well-formed JSON object.",
			Applies:     func(s Subject) bool { return s.Route.HasBody },
			Check: func(s Subject) string {
				if len(s.RawBody) == 0 {
					return "the request body is empty"
				}
				var v any
				if err := json.Unmarshal(s.RawBody, &v); err != nil {
					return "the request body is not well-formed JSON"
				}
				if _, ok := v.(map[string]any); !ok {
					return "the request body is a JSON value but not a JSON object"
				}
				return ""
			},
		},
		{
			ID: "L1.BODY_NESTING_WITHIN_LIMIT", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "body", Pure: true,
			Desc: "JSON nesting depth is within the configured limit",
			Remediation: "Reduce JSON nesting depth to " + itoa(defaultInt(d.MaxNestingDepth, 12)) +
				" or fewer levels.",
			Applies: func(s Subject) bool { return s.Body != nil },
			Check: func(s Subject) string {
				limit := defaultInt(d.MaxNestingDepth, 12)
				if got := depthOf(s.Body); got > limit {
					return "the document nests " + itoa(got) + " levels deep; the limit is " + itoa(limit)
				}
				return ""
			},
		},
		{
			ID: "L1.AUTH_CREDENTIAL_PRESENT", Severity: engine.Error,
			Code: string(apierror.CodeUnauthenticated), Field: "Authorization", Pure: true,
			Desc:        "a bearer token or a client certificate was presented",
			Remediation: "Provide a bearer token or a client certificate.",
			Applies:     authed,
			Check: func(s Subject) string {
				if s.Token.Present || s.Cert.Presented {
					return ""
				}
				return "no credential was presented on an authenticated route"
			},
		},
		{
			ID: "L1.JWT_IS_WELL_FORMED", Severity: engine.Error,
			Code: string(apierror.CodeInvalidToken), Field: "Authorization", Pure: true,
			Desc:        "the token has three base64url segments and a permitted `alg`",
			Remediation: "Malformed access token. Obtain a new token from the token endpoint.",
			Applies:     bearer,
			Check: func(s Subject) string {
				parts := strings.Split(s.Token.Raw, ".")
				if len(parts) != 3 {
					return "the access token does not have three segments"
				}
				for i, p := range parts[:2] {
					if p == "" {
						return "access token segment " + itoa(i+1) + " is empty"
					}
					if _, err := base64.RawURLEncoding.DecodeString(p); err != nil {
						return "access token segment " + itoa(i+1) + " is not valid base64url"
					}
				}
				// `alg` is attacker-controlled, so it is checked against an allowlist and never
				// used to select a verifier. This is the check that stops `none` and the
				// RS256→HS256 key-confusion attack.
				if !containsFold(defaultStrings(d.AllowedAlgorithms, []string{"RS256", "ES256"}), s.Token.Algorithm) {
					return "token algorithm " + quoteOrEmpty(s.Token.Algorithm) + " is not permitted"
				}
				return ""
			},
		},
		{
			ID: "L1.JWT_SIGNATURE_VERIFIES", Severity: engine.Error,
			Code: string(apierror.CodeUnauthenticated), Field: "Authorization", Pure: true,
			Desc:        "the token signature verifies against a cached JWKS key",
			Remediation: "Access token signature is invalid. Obtain a new token.",
			Applies:     bearer,
			Check: func(s Subject) string {
				if s.Token.SignatureVerified {
					return ""
				}
				return "the access token signature did not verify"
			},
		},
		{
			ID: "L1.JWT_NOT_EXPIRED", Severity: engine.Error,
			Code: string(apierror.CodeTokenExpired), Field: "Authorization", Pure: true,
			Desc:        "`exp` is in the future and `nbf` is in the past, within the permitted skew",
			Remediation: "Access token expired. Refresh and retry.",
			Applies:     bearer,
			Check: func(s Subject) string {
				skew := d.ClockSkew
				if skew == 0 {
					skew = 60 * time.Second
				}
				if s.Token.ExpiresAt.IsZero() {
					return "the access token carries no `exp` claim"
				}
				if !s.Token.ExpiresAt.After(s.Now.Add(-skew)) {
					return "the access token expired at " + s.Token.ExpiresAt.UTC().Format(time.RFC3339)
				}
				if !s.Token.NotBefore.IsZero() && s.Token.NotBefore.After(s.Now.Add(skew)) {
					return "the access token is not valid before " + s.Token.NotBefore.UTC().Format(time.RFC3339)
				}
				return ""
			},
		},
		{
			ID: "L1.JWT_ISSUER_TRUSTED", Severity: engine.Error,
			Code: string(apierror.CodeUnauthenticated), Field: "Authorization", Pure: true,
			Desc:        "`iss` is in this environment's issuer allowlist",
			Remediation: "Token was issued by an untrusted issuer.",
			Applies:     bearer,
			Check: func(s Subject) string {
				if contains(d.TrustedIssuers, s.Token.Issuer) {
					return ""
				}
				return "issuer " + quoteOrEmpty(s.Token.Issuer) + " is not trusted by this environment"
			},
		},
		{
			ID: "L1.JWT_AUDIENCE_MATCHES", Severity: engine.Error,
			Code: string(apierror.CodeUnauthenticated), Field: "Authorization", Pure: true,
			Desc:        "`aud` contains this API's audience",
			Remediation: "Token audience does not include this API.",
			Applies:     bearer,
			Check: func(s Subject) string {
				if contains(s.Token.Audience, d.Audience) {
					return ""
				}
				return "the token audience does not include " + quoteOrEmpty(d.Audience)
			},
		},
		{
			ID: "L1.MTLS_CHAIN_VALID", Severity: engine.Error,
			Code: string(apierror.CodeUnauthenticated), Field: "client-certificate", Pure: false,
			Desc: "the client certificate chains to the platform CA, is not revoked, and its " +
				"SAN maps to a registered API client",
			Remediation: "Client certificate is invalid, expired or revoked.",
			Applies:     func(s Subject) bool { return s.Auth == AuthMTLS },
			Check: func(s Subject) string {
				switch {
				case !s.Cert.Presented:
					return "no client certificate was presented"
				case !s.Cert.ChainVerified:
					return "the client certificate does not chain to the platform CA"
				case s.Cert.Revoked:
					return "the client certificate has been revoked"
				case !s.Cert.SANMapsToClient:
					return "the certificate SAN does not map to a registered API client"
				}
				return ""
			},
		},
		{
			ID: "L1.TENANT_CLAIM_PRESENT", Severity: engine.Error,
			Code: string(apierror.CodeTenantMismatch), Field: "Authorization", Pure: true,
			Desc:        "the token carries a `tenantid` claim resolving to an active tenant",
			Remediation: "Token carries no usable tenant. Contact your platform administrator.",
			Applies:     authed,
			Check: func(s Subject) string {
				if s.Token.TenantID == "" {
					return "the credential carries no tenant claim"
				}
				if !s.TenantActive {
					return "tenant " + s.Token.TenantID + " is not active"
				}
				return ""
			},
		},
		{
			ID: "L1.BODY_TENANT_MATCHES_TOKEN", Severity: engine.Error,
			Code: string(apierror.CodeTenantMismatch), Field: "/tenantId", Pure: true,
			Desc: "a `tenantId` in the body or query equals the credential's tenant; a mismatch " +
				"is a security event",
			Remediation: "`tenantId` in the request does not match your credentials. Omit it.",
			Applies: func(s Subject) bool {
				_, inBody := s.bodyValue("tenantId")
				_, inQuery := s.Query["tenantId"]
				return inBody || inQuery
			},
			Check: func(s Subject) string {
				want := s.Token.TenantID
				if v, ok := s.bodyValue("tenantId"); ok {
					if str, _ := v.(string); str != want {
						return "the request names a tenant other than the one your credentials belong to"
					}
				}
				if v, ok := s.Query["tenantId"]; ok && v != want {
					return "the query names a tenant other than the one your credentials belong to"
				}
				return ""
			},
		},
		{
			ID: "L1.TOKEN_SCOPE_COVERS_ROUTE", Severity: engine.Error,
			Code: string(apierror.CodeInsufficientScope), Field: "Authorization", Pure: true,
			Desc:        "the token's scopes include the scope this endpoint requires",
			Remediation: "Your token lacks the scope this endpoint requires. Request it from your platform administrator.",
			Applies:     func(s Subject) bool { return s.Route.Authenticated && s.Route.RequiredScope != "" },
			Check: func(s Subject) string {
				if contains(s.Token.Scopes, s.Route.RequiredScope) {
					return ""
				}
				return "this endpoint requires the `" + s.Route.RequiredScope + "` scope"
			},
		},
		{
			ID: "L1.PRINCIPAL_ROLE_PERMITS_ACTION", Severity: engine.Error,
			Code: string(apierror.CodeForbidden), Field: "principal", Pure: true,
			Desc:        "the principal's role binding grants the route's action",
			Remediation: "Your role does not permit this action.",
			Applies:     func(s Subject) bool { return s.Route.Authenticated && s.Route.RequiredAction != "" },
			Check: func(s Subject) string {
				if contains(s.Principal.PermittedActions, s.Route.RequiredAction) {
					return ""
				}
				return "the action `" + s.Route.RequiredAction + "` is not granted to your role"
			},
		},
		{
			ID: "L1.TENANT_RATE_LIMIT_NOT_EXCEEDED", Severity: engine.Error,
			Code: string(apierror.CodeRateLimited), Field: "tenant", Pure: false,
			Desc:        "the (tenant, route class) token bucket has a token available",
			Remediation: "Rate limit exceeded. Retry after the interval in `Retry-After`.",
			Applies:     authed,
			Check: func(s Subject) string {
				if s.Limits.TenantTokenAvailable {
					return ""
				}
				return "the request rate for your tenant is above its limit"
			},
		},
		{
			ID: "L1.MERCHANT_RATE_LIMIT_NOT_EXCEEDED", Severity: engine.Error,
			Code: string(apierror.CodeRateLimited), Field: "/merchantId", Pure: false,
			Desc:        "the (tenant, merchant, route class) token bucket has a token available",
			Remediation: "Per-merchant rate limit exceeded. Retry after `Retry-After`.",
			Applies: func(s Subject) bool {
				_, ok := s.bodyValue("merchantId")
				return ok
			},
			Check: func(s Subject) string {
				if s.Limits.MerchantTokenAvailable {
					return ""
				}
				return "the request rate for this merchant is above its limit"
			},
		},
		{
			ID: "L1.CONCURRENCY_BULKHEAD_AVAILABLE", Severity: engine.Error,
			Code: string(apierror.CodeConcurrencyLimitExceeded), Field: "tenant", Pure: false,
			Desc:        "in-flight requests for the tenant are below the bulkhead limit",
			Remediation: "Too many concurrent requests for your tenant. Retry after `Retry-After`.",
			Applies:     authed,
			Check: func(s Subject) string {
				if s.Limits.ConcurrencyLimit <= 0 || s.Limits.ConcurrencyInFlight < s.Limits.ConcurrencyLimit {
					return ""
				}
				return "your tenant has " + itoa(s.Limits.ConcurrencyInFlight) +
					" requests in flight; the bulkhead permits " + itoa(s.Limits.ConcurrencyLimit)
			},
		},
	}
}

func phaseB(d Deps) []ruledef.Def[Subject] {
	hasBody := func(s Subject) bool { return s.Body != nil }

	return []ruledef.Def[Subject]{
		{
			ID: "L1.REQUIRED_FIELDS_PRESENT", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "body", Pure: true,
			Desc:        "every schema-required field is present and non-null",
			Remediation: "Supply every required field.",
			Applies:     hasBody,
			Check: func(s Subject) string {
				var missing []string
				for _, path := range s.Route.Schema.Required {
					v, ok := lookupPath(s.Body, path)
					if !ok || v == nil {
						missing = append(missing, path)
					}
				}
				if len(missing) == 0 {
					return ""
				}
				return "required field(s) missing: " + strings.Join(missing, ", ")
			},
		},
		{
			ID: "L1.NO_UNKNOWN_FIELDS", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "body", Pure: true,
			Desc:        "the body contains no property outside the route's schema",
			Remediation: "Remove the unknown field. Check for a typo or an unsupported API version.",
			Applies:     func(s Subject) bool { return s.Body != nil && !s.Route.Schema.AllowUnknown },
			Check: func(s Subject) string {
				var unknown []string
				walkValues(s.Body, "", 0, maxWalkDepth, func(path string, v any) {
					if path == "" {
						return
					}
					norm := normalizePath(path)
					if _, ok := s.Route.Schema.Fields[norm]; ok {
						return
					}
					// A field under a declared free-form object (metadata) is not unknown:
					// the schema declared that its keys belong to the caller.
					if parent, _, found := strings.Cut(norm, "."); found {
						if spec, ok := s.Route.Schema.Fields[parent]; ok && spec.Type == TypeObject && spec.Kind == KindPlain {
							return
						}
					}
					unknown = append(unknown, norm)
				})
				if len(unknown) == 0 {
					return ""
				}
				return "unknown field(s): " + strings.Join(dedupe(unknown), ", ")
			},
		},
		{
			ID: "L1.FIELD_TYPES_MATCH_SCHEMA", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "body", Pure: true,
			Desc:        "each field's JSON type matches the schema, with no coercion",
			Remediation: "Send each field with the JSON type the schema declares; `\"100\"` is not `100`.",
			Applies:     hasBody,
			Check: func(s Subject) string {
				var bad []string
				walkValues(s.Body, "", 0, maxWalkDepth, func(path string, v any) {
					if path == "" || v == nil {
						return
					}
					spec, ok := s.Route.Schema.Fields[normalizePath(path)]
					if !ok || spec.Type == "" {
						return
					}
					if !typeSatisfies(spec.Type, jsonTypeOf(v)) {
						bad = append(bad, path+" must be of type "+string(spec.Type))
					}
				})
				if len(bad) == 0 {
					return ""
				}
				return strings.Join(bad, "; ")
			},
		},
		{
			ID: "L1.STRING_LENGTHS_WITHIN_BOUNDS", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "body", Pure: true,
			Desc: "each string is valid UTF-8, within its declared length bounds, and free of " +
				"control characters except newline in declared multiline fields",
			Remediation: "Send each string within the length bounds the schema declares.",
			Applies:     hasBody,
			Check: func(s Subject) string {
				var bad []string
				walkValues(s.Body, "", 0, maxWalkDepth, func(path string, v any) {
					str, ok := v.(string)
					if !ok {
						return
					}
					spec := s.Route.Schema.Fields[normalizePath(path)]
					if !utf8.ValidString(str) {
						bad = append(bad, path+" is not valid UTF-8")
						return
					}
					n := utf8.RuneCountInString(str)
					if spec.MinLength > 0 && n < spec.MinLength {
						bad = append(bad, path+" must be at least "+itoa(spec.MinLength)+" characters")
					}
					if spec.MaxLength > 0 && n > spec.MaxLength {
						bad = append(bad, path+" must be at most "+itoa(spec.MaxLength)+" characters")
					}
					for _, r := range str {
						if r < 0x20 && (!spec.Multiline || r != '\n') {
							bad = append(bad, path+" contains a control character")
							break
						}
					}
				})
				if len(bad) == 0 {
					return ""
				}
				return strings.Join(bad, "; ")
			},
		},
		{
			ID: "L1.ENUM_VALUES_ARE_KNOWN", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "body", Pure: true,
			Desc:        "each enum field's value is in the schema enum for this API version",
			Remediation: "Use one of the values this API version accepts for the field.",
			Applies: func(s Subject) bool {
				if s.Body == nil {
					return false
				}
				for _, spec := range s.Route.Schema.Fields {
					if len(spec.Enum) > 0 {
						return true
					}
				}
				return false
			},
			Check: func(s Subject) string {
				var bad []string
				walkValues(s.Body, "", 0, maxWalkDepth, func(path string, v any) {
					spec, ok := s.Route.Schema.Fields[normalizePath(path)]
					if !ok || len(spec.Enum) == 0 {
						return
					}
					str, isStr := v.(string)
					if !isStr {
						return
					}
					if !contains(spec.Enum, str) {
						bad = append(bad, path+" must be one of "+strings.Join(spec.Enum, ", "))
					}
				})
				if len(bad) == 0 {
					return ""
				}
				return strings.Join(bad, "; ")
			},
		},
		{
			ID: "L1.AMOUNT_IS_POSITIVE_MINOR_UNITS", Severity: engine.Error,
			Code: string(apierror.CodeAmountInvalid), Field: "/amount", Pure: true,
			Desc: "an amount is a positive JSON integer in minor units, no decimal point, no " +
				"exponent, not a string, within 2^53−1",
			Remediation: "`amount` must be a positive integer in minor units, e.g. `1050` for USD 10.50.",
			Applies:     func(s Subject) bool { return s.Body != nil && hasKind(s, KindAmount) },
			Check: func(s Subject) string {
				var bad []string
				forEachKind(s, KindAmount, func(path string, v any) {
					if msg := amountProblem(v); msg != "" {
						bad = append(bad, path+" "+msg)
					}
				})
				if len(bad) == 0 {
					return ""
				}
				return strings.Join(bad, "; ")
			},
		},
		{
			ID: "L1.CURRENCY_IS_ISO4217", Severity: engine.Error,
			Code: string(apierror.CodeCurrencyNotSupported), Field: "/currency", Pure: true,
			Desc:        "a currency field is an uppercase alpha-3 code present in the ISO 4217 table",
			Remediation: "Send an ISO 4217 alpha-3 currency code, e.g. `USD`.",
			Applies:     func(s Subject) bool { return s.Body != nil && hasKind(s, KindCurrency) },
			Check: func(s Subject) string {
				var bad []string
				forEachKind(s, KindCurrency, func(path string, v any) {
					str, ok := v.(string)
					if !ok {
						return
					}
					if !money.Currency(str).IsSupported() {
						bad = append(bad, path+": "+quoteOrEmpty(str)+" is not a valid ISO 4217 currency code")
					}
				})
				if len(bad) == 0 {
					return ""
				}
				return strings.Join(bad, "; ")
			},
		},
		{
			ID: "L1.COUNTRY_IS_ISO3166_ALPHA2", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/country", Pure: true,
			Desc:        "a country field is an uppercase alpha-2 code present in the ISO 3166-1 table",
			Remediation: "Send an ISO 3166-1 alpha-2 country code, e.g. `DE`.",
			Applies:     func(s Subject) bool { return s.Body != nil && hasKind(s, KindCountry) },
			Check: func(s Subject) string {
				var bad []string
				forEachKind(s, KindCountry, func(path string, v any) {
					str, ok := v.(string)
					if !ok {
						return
					}
					if !shared.Country(str).IsValid() {
						bad = append(bad, path+": "+quoteOrEmpty(str)+" is not a valid ISO 3166-1 alpha-2 country code")
					}
				})
				if len(bad) == 0 {
					return ""
				}
				return strings.Join(bad, "; ")
			},
		},
		{
			ID: "L1.TIMESTAMPS_ARE_RFC3339_UTC", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/timestamp", Pure: true,
			Desc:        "a timestamp field is RFC 3339 with an explicit offset and not far in the future",
			Remediation: "Send timestamps as RFC 3339 with an explicit UTC offset, e.g. `2026-03-14T10:00:00Z`.",
			Applies:     func(s Subject) bool { return s.Body != nil && hasKind(s, KindTimestamp) },
			Check: func(s Subject) string {
				tol := d.FutureTimestampTolerance
				if tol == 0 {
					tol = 24 * time.Hour
				}
				var bad []string
				forEachKind(s, KindTimestamp, func(path string, v any) {
					str, ok := v.(string)
					if !ok {
						return
					}
					ts, err := time.Parse(time.RFC3339, str)
					if err != nil {
						bad = append(bad, path+" is not an RFC 3339 timestamp with an offset")
						return
					}
					if ts.After(s.Now.Add(tol)) {
						bad = append(bad, path+" is more than 24 hours in the future")
					}
				})
				if len(bad) == 0 {
					return ""
				}
				return strings.Join(bad, "; ")
			},
		},
		{
			ID: "L1.NO_PAN_IN_ANY_STRING_FIELD", Severity: engine.Error,
			Code: string(apierror.CodeSensitiveDataInRequest), Field: "body", Pure: true,
			Desc: "no string field contains a Luhn-valid 13–19 digit card number, on any field, " +
				"allowlisted or not",
			Remediation: "This API does not accept card numbers. Tokenize with the gateway SDK and send the token.",
			Applies:     func(s Subject) bool { return s.Body != nil },
			Check: func(s Subject) string {
				// Paths only, never the value. A function that returned the match would put a
				// PAN into an error message and from there into a log index, which is the exact
				// breach this rule exists to prevent — so the message names fields and their
				// length and nothing else. See internal/platform/secret.ContainsPAN.
				var hits []string
				walkValues(s.Body, "", 0, maxWalkDepth, func(path string, v any) {
					str, ok := v.(string)
					if !ok || path == "" {
						return
					}
					if secret.ContainsPAN(str) {
						hits = append(hits, path)
					}
				})
				if len(hits) == 0 {
					return ""
				}
				return "field(s) appear to contain a primary account number: " + strings.Join(hits, ", ")
			},
		},
		{
			ID: "L1.NO_CVV_FIELD_NAMES", Severity: engine.Error,
			Code: string(apierror.CodeSensitiveDataInRequest), Field: "body", Pure: true,
			Desc:        "no property is named cvv, cvc, cvv2, cid or securityCode, in any casing",
			Remediation: "This API does not accept CVV data. Remove the field.",
			Applies:     func(s Subject) bool { return s.Body != nil },
			Check: func(s Subject) string {
				var hits []string
				walkValues(s.Body, "", 0, maxWalkDepth, func(path string, v any) {
					m, ok := v.(map[string]any)
					if !ok {
						return
					}
					for k := range m {
						if isCVVName(k) {
							hits = append(hits, join(path, k))
						}
					}
				})
				if len(hits) == 0 {
					return ""
				}
				return "field(s) name card verification data: " + strings.Join(dedupe(hits), ", ")
			},
		},
		{
			ID: "L1.NO_TRACK_DATA_PATTERN", Severity: engine.Error,
			Code: string(apierror.CodeSensitiveDataInRequest), Field: "body", Pure: true,
			Desc:        "no string matches a magnetic-stripe track 1 or track 2 pattern",
			Remediation: "Track data is never accepted. Remove it.",
			Applies:     func(s Subject) bool { return s.Body != nil },
			Check: func(s Subject) string {
				var hits []string
				walkValues(s.Body, "", 0, maxWalkDepth, func(path string, v any) {
					str, ok := v.(string)
					if !ok || path == "" {
						return
					}
					if looksLikeTrackData(str) {
						hits = append(hits, path)
					}
				})
				if len(hits) == 0 {
					return ""
				}
				return "field(s) appear to contain magnetic-stripe track data: " + strings.Join(hits, ", ")
			},
		},
		{
			ID: "L1.IDEMPOTENCY_KEY_PRESENT", Severity: engine.Error,
			Code: string(apierror.CodeIdempotencyKeyRequired), Field: "Idempotency-Key", Pure: true,
			Desc:        "an Idempotency-Key header is present on a route that requires one",
			Remediation: "This endpoint requires an `Idempotency-Key` header. Use a fresh UUID per logical operation.",
			Applies:     func(s Subject) bool { return s.Route.RequiresIdempotencyKey },
			Check: func(s Subject) string {
				if s.hasHeader("Idempotency-Key") {
					return ""
				}
				return "the Idempotency-Key header is missing"
			},
		},
		{
			ID: "L1.IDEMPOTENCY_KEY_WELL_FORMED", Severity: engine.Error,
			Code: string(apierror.CodeIdempotencyKeyRequired), Field: "Idempotency-Key", Pure: true,
			Desc:        "the Idempotency-Key is 1–255 printable ASCII characters with no whitespace",
			Remediation: "`Idempotency-Key` must be 1–255 printable ASCII characters with no whitespace.",
			Applies:     func(s Subject) bool { return s.hasHeader("Idempotency-Key") },
			Check: func(s Subject) string {
				k := s.header("Idempotency-Key")
				if len(k) > 255 {
					return "the Idempotency-Key is " + itoa(len(k)) + " characters; the limit is 255"
				}
				for i := 0; i < len(k); i++ {
					if k[i] <= 0x20 || k[i] >= 0x7f {
						return "the Idempotency-Key contains a character outside printable ASCII"
					}
				}
				return ""
			},
		},
		{
			ID: "L1.IF_MATCH_PRESENT_ON_MUTATION", Severity: engine.Error,
			Code: string(apierror.CodeConfigurationVersionConflict), Field: "If-Match", Pure: true,
			Desc:        "a mutation of a versioned resource carries the If-Match header",
			Remediation: "Send `If-Match` with the ETag you last read.",
			Applies:     func(s Subject) bool { return s.Route.MutatesResource },
			Check: func(s Subject) string {
				if s.hasHeader("If-Match") {
					return ""
				}
				return "the If-Match header is missing on a mutation of a versioned resource"
			},
		},
		{
			ID: "L1.PAGINATION_WITHIN_BOUNDS", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "limit", Pure: true,
			Desc:        "`limit` on a list route is between 1 and the page-size ceiling",
			Remediation: "`limit` must be between 1 and " + itoa(defaultInt(100, 100)) + ".",
			Applies:     func(s Subject) bool { return s.Route.IsList },
			Check: func(s Subject) string {
				raw, ok := s.Query["limit"]
				if !ok || raw == "" {
					return ""
				}
				n, err := strconv.Atoi(raw)
				if err != nil {
					return "`limit` must be an integer"
				}
				limit := defaultInt(d.MaxPageSize, 100)
				if n < 1 || n > limit {
					return "`limit` is " + itoa(n) + "; it must be between 1 and " + itoa(limit)
				}
				return ""
			},
		},
		{
			ID: "L1.CURSOR_IS_DECODABLE", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "cursor", Pure: true,
			Desc:        "the pagination cursor decodes, its HMAC verifies, and its tenant matches the caller",
			Remediation: "Invalid pagination cursor. Restart the listing without `cursor`.",
			Applies:     func(s Subject) bool { return s.Cursor.Present },
			Check: func(s Subject) string {
				switch {
				case !s.Cursor.Decodable:
					return "the pagination cursor could not be decoded"
				case !s.Cursor.HMACValid:
					return "the pagination cursor failed its integrity check"
				case !s.Cursor.TenantMatches:
					return "the pagination cursor belongs to a different tenant"
				}
				return ""
			},
		},
		{
			ID: "L1.METADATA_WITHIN_QUOTA", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/metadata", Pure: true,
			Desc:        "metadata is within the key count, key length, value length and total size quota",
			Remediation: "`metadata` may contain at most 40 keys, 40-character keys, 500-character values and 8 KiB in total.",
			Applies: func(s Subject) bool {
				_, ok := s.bodyValue("metadata")
				return ok
			},
			Check: func(s Subject) string {
				v, _ := s.bodyValue("metadata")
				m, ok := v.(map[string]any)
				if !ok {
					return "`metadata` must be a JSON object"
				}
				maxKeys := defaultInt(d.MaxMetadataKeys, 40)
				maxKeyLen := defaultInt(d.MaxMetadataKeyLen, 40)
				maxValLen := defaultInt(d.MaxMetadataValueLen, 500)
				maxBytes := defaultInt(d.MaxMetadataBytes, 8*1024)
				if len(m) > maxKeys {
					return "`metadata` has " + itoa(len(m)) + " keys; the limit is " + itoa(maxKeys)
				}
				total := 0
				for k, raw := range m {
					total += len(k)
					if utf8.RuneCountInString(k) > maxKeyLen {
						return "metadata key `" + k + "` is longer than " + itoa(maxKeyLen) + " characters"
					}
					str, _ := raw.(string)
					total += len(str)
					if utf8.RuneCountInString(str) > maxValLen {
						return "metadata value for `" + k + "` is longer than " + itoa(maxValLen) + " characters"
					}
				}
				if total > maxBytes {
					return "`metadata` totals " + itoa(total) + " bytes; the limit is " + itoa(maxBytes)
				}
				return ""
			},
		},
		{
			ID: "L1.TRACEPARENT_WELL_FORMED", Severity: engine.Warning,
			Code: "", Field: "traceparent", Pure: true,
			Desc:        "a supplied `traceparent` header is W3C-formatted; a malformed one starts a new trace",
			Remediation: "`traceparent` was malformed and was ignored; a new trace was started.",
			Applies:     func(s Subject) bool { return s.hasHeader("traceparent") },
			Check: func(s Subject) string {
				if isW3CTraceparent(s.header("traceparent")) {
					return ""
				}
				return "the supplied traceparent header is not in W3C format"
			},
		},
	}
}

// maxWalkDepth bounds every body traversal. It is above the nesting limit L1 enforces, so a
// document deep enough to escape a walk has already been rejected by
// L1.BODY_NESTING_WITHIN_LIMIT — which is what stops a self-referential document from turning
// a security control into a denial of service.
const maxWalkDepth = 64

func hasKind(s Subject, kind FieldKind) bool {
	for _, spec := range s.Route.Schema.Fields {
		if spec.Kind == kind {
			return true
		}
	}
	return false
}

func forEachKind(s Subject, kind FieldKind, fn func(path string, v any)) {
	walkValues(s.Body, "", 0, maxWalkDepth, func(path string, v any) {
		if path == "" {
			return
		}
		if spec, ok := s.Route.Schema.Fields[normalizePath(path)]; ok && spec.Kind == kind {
			fn(path, v)
		}
	})
}

// amountProblem states what is wrong with a decoded amount, or "" if nothing is.
//
// The string case is called out separately because it is the most common integration mistake
// and the most dangerous: a client that sends `"10.50"` is doing currency arithmetic in a
// decimal string and will eventually send `"10.5"` for a different number.
func amountProblem(v any) string {
	switch t := v.(type) {
	case string:
		return "must be a JSON number, not a string"
	case json.Number:
		if strings.ContainsAny(string(t), ".eE") {
			return "must be an integer in minor units, with no decimal point and no exponent"
		}
		n, err := strconv.ParseInt(string(t), 10, 64)
		if err != nil {
			return "is not a representable integer"
		}
		return amountRange(n)
	case float64:
		if t != float64(int64(t)) {
			return "must be an integer in minor units, with no decimal point and no exponent"
		}
		return amountRange(int64(t))
	case nil:
		return ""
	default:
		return "must be a JSON number in minor units"
	}
}

func amountRange(n int64) string {
	const maxSafe = int64(1)<<53 - 1
	switch {
	case n <= 0:
		return "must be greater than zero"
	case n > maxSafe:
		return "exceeds the maximum representable amount (2^53−1)"
	}
	return ""
}

// isCVVName matches the card-verification field names case- and separator-insensitively,
// because `security_code`, `securityCode` and `SecurityCode` are the same mistake.
func isCVVName(k string) bool {
	norm := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z':
			return r + 32
		case r == '_' || r == '-' || r == ' ':
			return -1
		}
		return r
	}, k)
	switch norm {
	case "cvv", "cvc", "cvv2", "cvc2", "cid", "securitycode", "cardsecuritycode", "cav2", "cvn":
		return true
	}
	return false
}

// looksLikeTrackData matches the two magnetic-stripe framings: track 1 (`%B…^…^…?`) and
// track 2 (`;…=…?`). Both are structural rather than statistical, so unlike the PAN detector
// there is effectively no false-positive budget to spend.
func looksLikeTrackData(s string) bool {
	if i := strings.Index(s, "%B"); i >= 0 {
		rest := s[i:]
		if strings.Count(rest, "^") >= 2 && strings.Contains(rest, "?") {
			return true
		}
	}
	if i := strings.IndexByte(s, ';'); i >= 0 {
		rest := s[i:]
		if j := strings.IndexByte(rest, '='); j > 12 && strings.Contains(rest[j:], "?") {
			return true
		}
	}
	return false
}

// isW3CTraceparent checks the `00-<32 hex>-<16 hex>-<2 hex>` form.
func isW3CTraceparent(v string) bool {
	parts := strings.Split(v, "-")
	if len(parts) != 4 {
		return false
	}
	lens := []int{2, 32, 16, 2}
	for i, p := range parts {
		if len(p) != lens[i] || !isHex(p) {
			return false
		}
	}
	return parts[1] != strings.Repeat("0", 32) && parts[2] != strings.Repeat("0", 16)
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func containsFold(set []string, v string) bool {
	for _, s := range set {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := in[:0:0]
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func defaultStrings(v, def []string) []string {
	if len(v) == 0 {
		return def
	}
	return v
}

func quoteOrEmpty(s string) string {
	if s == "" {
		return "empty"
	}
	return "`" + s + "`"
}
