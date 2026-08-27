package l1api_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/internal/ruletest"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/l1api"
)

var now = time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)

func deps() l1api.Deps {
	d := l1api.DefaultDeps()
	d.TrustedIssuers = []string{"https://issuer.payments-platform.test"}
	d.Audience = "payments-api"
	return d
}

// schema is the route contract the base subject is validated against. It is deliberately small
// and covers one field of every semantic kind, so the kind-driven rules (amount, currency,
// country, timestamp) have something to find.
func schema() l1api.Schema {
	return l1api.Schema{
		Required: []string{"amount", "currency", "merchantId"},
		Fields: map[string]l1api.FieldSpec{
			"amount":             {Type: l1api.TypeInteger, Kind: l1api.KindAmount},
			"currency":           {Type: l1api.TypeString, Kind: l1api.KindCurrency},
			"merchantId":         {Type: l1api.TypeString, MinLength: 1, MaxLength: 64},
			"tenantId":           {Type: l1api.TypeString, MaxLength: 64},
			"captureMode":        {Type: l1api.TypeString, Enum: []string{"AUTOMATIC", "MANUAL"}},
			"requestedAt":        {Type: l1api.TypeString, Kind: l1api.KindTimestamp},
			"customer":           {Type: l1api.TypeObject},
			"customer.country":   {Type: l1api.TypeString, Kind: l1api.KindCountry},
			"customer.reference": {Type: l1api.TypeString, MaxLength: 64},
			"metadata":           {Type: l1api.TypeObject},
		},
	}
}

// base is a well-formed, authenticated, schema-clean request. Every rule's failing case is one
// mutation away from it, which is what makes the diff between "valid" and "rejected" readable.
func base() l1api.Subject {
	return l1api.Subject{
		Route: l1api.Route{
			Method:                 "POST",
			Path:                   "/v1/payments",
			HasBody:                true,
			Authenticated:          true,
			RequiresIdempotencyKey: true,
			RequiredScope:          "payments:write",
			RequiredAction:         "payments.create",
			Schema:                 schema(),
		},
		TLSVersion: l1api.TLS13,
		Headers: map[string]string{
			"Content-Type":    "application/json",
			"Idempotency-Key": "0f2b6c1e-1c3d-4f0a-9a1b-2c3d4e5f6071",
		},
		RawBody: []byte(`{"amount":1050,"currency":"USD","merchantId":"mrc_123"}`),
		Body: map[string]any{
			"amount":      json.Number("1050"),
			"currency":    "USD",
			"merchantId":  "mrc_123",
			"tenantId":    "ten_live_1",
			"captureMode": "AUTOMATIC",
			"requestedAt": "2026-03-14T09:59:00Z",
			"customer": map[string]any{
				"country":   "DE",
				"reference": "cus_9",
			},
			"metadata": map[string]any{"orderId": "A-1"},
		},
		Query: map[string]string{},
		Auth:  l1api.AuthBearer,
		Token: l1api.TokenClaims{
			Raw:               "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhcGkifQ.c2lnbmF0dXJl",
			Present:           true,
			Algorithm:         "RS256",
			KeyID:             "kid-1",
			Issuer:            "https://issuer.payments-platform.test",
			Audience:          []string{"payments-api"},
			Subject:           "api_client_1",
			TenantID:          "ten_live_1",
			Scopes:            []string{"payments:write", "payments:read"},
			NotBefore:         now.Add(-time.Hour),
			ExpiresAt:         now.Add(time.Hour),
			SignatureVerified: true,
		},
		Principal: l1api.Principal{
			ID:               "api_client_1",
			Roles:            []string{"payments_operator"},
			PermittedActions: []string{"payments.create"},
		},
		TenantActive: true,
		Limits: l1api.RateLimits{
			TenantTokenAvailable:   true,
			MerchantTokenAvailable: true,
			ConcurrencyInFlight:    3,
			ConcurrencyLimit:       100,
		},
		Now: now,
	}
}

func customer(s *l1api.Subject) map[string]any {
	return s.Body["customer"].(map[string]any)
}

func metadata(s *l1api.Subject) map[string]any {
	return s.Body["metadata"].(map[string]any)
}

func TestL1Rules(t *testing.T) {
	t.Parallel()
	set := l1api.Rules(deps())

	ruletest.Run(t, set, base, []ruletest.Case[l1api.Subject]{
		{
			ID:   "L1.TLS_VERSION_AT_LEAST_1_2",
			Pass: func(s *l1api.Subject) { s.TLSVersion = l1api.TLS12 },
			Fail: func(s *l1api.Subject) { s.TLSVersion = l1api.TLS11 },
		},
		{
			ID:   "L1.CONTENT_TYPE_IS_JSON",
			Pass: func(s *l1api.Subject) { s.Headers["Content-Type"] = "application/json; charset=utf-8" },
			Fail: func(s *l1api.Subject) { s.Headers["Content-Type"] = "text/plain" },
		},
		{
			ID:   "L1.BODY_WITHIN_SIZE_LIMIT",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.RawBody = make([]byte, 300*1024) },
		},
		{
			ID:   "L1.BODY_IS_WELL_FORMED_JSON",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.RawBody = []byte(`[1,2,3]`) },
		},
		{
			ID:   "L1.BODY_NESTING_WITHIN_LIMIT",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.Body["deep"] = nest(15) },
		},
		{
			ID:   "L1.AUTH_CREDENTIAL_PRESENT",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.Token.Present = false; s.Cert.Presented = false },
		},
		{
			ID:   "L1.JWT_IS_WELL_FORMED",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.Token.Raw = "not-a-jwt" },
		},
		{
			ID:   "L1.JWT_SIGNATURE_VERIFIES",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.Token.SignatureVerified = false },
		},
		{
			ID:   "L1.JWT_NOT_EXPIRED",
			Pass: func(s *l1api.Subject) { s.Token.ExpiresAt = s.Now.Add(30 * time.Second) },
			Fail: func(s *l1api.Subject) { s.Token.ExpiresAt = s.Now.Add(-2 * time.Minute) },
		},
		{
			ID:   "L1.JWT_ISSUER_TRUSTED",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.Token.Issuer = "https://issuer.attacker.example" },
		},
		{
			ID:   "L1.JWT_AUDIENCE_MATCHES",
			Pass: func(s *l1api.Subject) { s.Token.Audience = []string{"other-api", "payments-api"} },
			Fail: func(s *l1api.Subject) { s.Token.Audience = []string{"control-plane-api"} },
		},
		{
			ID: "L1.MTLS_CHAIN_VALID",
			Pass: func(s *l1api.Subject) {
				s.Auth = l1api.AuthMTLS
				s.Cert = l1api.ClientCert{Presented: true, ChainVerified: true, SANMapsToClient: true}
			},
			Fail: func(s *l1api.Subject) {
				s.Auth = l1api.AuthMTLS
				s.Cert = l1api.ClientCert{Presented: true, ChainVerified: true, SANMapsToClient: true, Revoked: true}
			},
		},
		{
			ID:   "L1.TENANT_CLAIM_PRESENT",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.TenantActive = false },
		},
		{
			ID:   "L1.BODY_TENANT_MATCHES_TOKEN",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.Body["tenantId"] = "ten_live_someone_else" },
		},
		{
			ID:   "L1.TOKEN_SCOPE_COVERS_ROUTE",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.Token.Scopes = []string{"payments:read"} },
		},
		{
			ID:   "L1.PRINCIPAL_ROLE_PERMITS_ACTION",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.Principal.PermittedActions = []string{"payments.read"} },
		},
		{
			ID:   "L1.TENANT_RATE_LIMIT_NOT_EXCEEDED",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.Limits.TenantTokenAvailable = false },
		},
		{
			ID:   "L1.MERCHANT_RATE_LIMIT_NOT_EXCEEDED",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.Limits.MerchantTokenAvailable = false },
		},
		{
			ID:   "L1.CONCURRENCY_BULKHEAD_AVAILABLE",
			Pass: func(s *l1api.Subject) { s.Limits.ConcurrencyInFlight = 99 },
			Fail: func(s *l1api.Subject) { s.Limits.ConcurrencyInFlight = 100 },
		},
		{
			ID:   "L1.REQUIRED_FIELDS_PRESENT",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { delete(s.Body, "amount") },
		},
		{
			ID:   "L1.NO_UNKNOWN_FIELDS",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.Body["captureMod"] = "AUTOMATIC" },
		},
		{
			ID:   "L1.FIELD_TYPES_MATCH_SCHEMA",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.Body["merchantId"] = json.Number("42") },
		},
		{
			ID:   "L1.STRING_LENGTHS_WITHIN_BOUNDS",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.Body["merchantId"] = strings.Repeat("m", 65) },
		},
		{
			ID:   "L1.ENUM_VALUES_ARE_KNOWN",
			Pass: func(s *l1api.Subject) { s.Body["captureMode"] = "MANUAL" },
			Fail: func(s *l1api.Subject) { s.Body["captureMode"] = "SOMETIMES" },
		},
		{
			ID:   "L1.AMOUNT_IS_POSITIVE_MINOR_UNITS",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.Body["amount"] = "1050" },
		},
		{
			ID:   "L1.CURRENCY_IS_ISO4217",
			Pass: func(s *l1api.Subject) { s.Body["currency"] = "JPY" },
			Fail: func(s *l1api.Subject) { s.Body["currency"] = "XBT" },
		},
		{
			ID:   "L1.COUNTRY_IS_ISO3166_ALPHA2",
			Pass: func(s *l1api.Subject) { customer(s)["country"] = "GB" },
			Fail: func(s *l1api.Subject) { customer(s)["country"] = "XX" },
		},
		{
			ID:   "L1.TIMESTAMPS_ARE_RFC3339_UTC",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.Body["requestedAt"] = "14/03/2026 09:59" },
		},
		{
			ID:   "L1.NO_PAN_IN_ANY_STRING_FIELD",
			Pass: func(s *l1api.Subject) { metadata(s)["orderId"] = "ORDER-2026-03-14-0001" },
			Fail: func(s *l1api.Subject) { metadata(s)["orderId"] = "4111 1111 1111 1111" },
		},
		{
			ID:   "L1.NO_CVV_FIELD_NAMES",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.Body["security_code"] = "123" },
		},
		{
			ID:   "L1.NO_TRACK_DATA_PATTERN",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) {
				metadata(s)["swipe"] = "%B4111111111111111^DOE/JOHN^25121011000000000000?"
			},
		},
		{
			ID:   "L1.IDEMPOTENCY_KEY_PRESENT",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { delete(s.Headers, "Idempotency-Key") },
		},
		{
			ID:   "L1.IDEMPOTENCY_KEY_WELL_FORMED",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) { s.Headers["Idempotency-Key"] = "key with spaces" },
		},
		{
			ID: "L1.IF_MATCH_PRESENT_ON_MUTATION",
			Pass: func(s *l1api.Subject) {
				s.Route.MutatesResource = true
				s.Headers["If-Match"] = `W/"7"`
			},
			Fail: func(s *l1api.Subject) { s.Route.MutatesResource = true },
		},
		{
			ID: "L1.PAGINATION_WITHIN_BOUNDS",
			Pass: func(s *l1api.Subject) {
				s.Route.IsList = true
				s.Query["limit"] = "100"
			},
			Fail: func(s *l1api.Subject) {
				s.Route.IsList = true
				s.Query["limit"] = "101"
			},
		},
		{
			ID: "L1.CURSOR_IS_DECODABLE",
			Pass: func(s *l1api.Subject) {
				s.Cursor = l1api.CursorState{Present: true, Decodable: true, HMACValid: true, TenantMatches: true}
			},
			Fail: func(s *l1api.Subject) {
				s.Cursor = l1api.CursorState{Present: true, Decodable: true, HMACValid: false, TenantMatches: true}
			},
		},
		{
			ID:   "L1.METADATA_WITHIN_QUOTA",
			Pass: func(s *l1api.Subject) {},
			Fail: func(s *l1api.Subject) {
				m := metadata(s)
				for i := 0; i < 41; i++ {
					m["k"+itoa(i)] = "v"
				}
			},
		},
		{
			ID: "L1.TRACEPARENT_WELL_FORMED",
			Pass: func(s *l1api.Subject) {
				s.Headers["traceparent"] = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
			},
			Fail: func(s *l1api.Subject) { s.Headers["traceparent"] = "00-not-a-trace" },
		},
	})
}

// TestL1PhaseSplitDoesNotLeakSchemaOutcomes is the property the phase boundary exists for: an
// unauthenticated request must not learn anything about the body schema.
func TestL1PhaseSplitDoesNotLeakSchemaOutcomes(t *testing.T) {
	t.Parallel()

	s := base()
	s.Token.SignatureVerified = false // phase A failure
	s.Body["bogus"] = "x"             // phase B failure, must stay invisible

	phaseA := l1api.PhaseARules(deps()).Evaluate(t.Context(), s)
	if phaseA.OK() {
		t.Fatal("phase A passed with an unverifiable token")
	}
	for _, o := range phaseA.Outcomes {
		if o.Rule == "L1.NO_UNKNOWN_FIELDS" {
			t.Fatal("phase A evaluated a schema rule; the phase boundary is not holding")
		}
	}

	// The body phase, run on its own, does see it — which is what makes the ordering, rather
	// than the rules themselves, the security control.
	phaseB := l1api.PhaseBRules(deps()).Evaluate(t.Context(), s)
	if _, ran := phaseB.For("L1.NO_UNKNOWN_FIELDS"); !ran {
		t.Fatal("phase B did not evaluate the schema rule")
	}
}

// TestL1PANRuleNeverEchoesTheValue is the rule's whole reason for existing: the field path is
// actionable, the value is a PCI breach.
func TestL1PANRuleNeverEchoesTheValue(t *testing.T) {
	t.Parallel()

	const pan = "4111111111111111"
	s := base()
	metadata(&s)["orderId"] = pan

	rule, ok := l1api.Rules(deps()).Rule("L1.NO_PAN_IN_ANY_STRING_FIELD")
	if !ok {
		t.Fatal("the PAN rule is not in the L1 set")
	}
	out := rule.Evaluate(t.Context(), s)
	if out.Passed {
		t.Fatal("the PAN rule did not detect a Luhn-valid test PAN")
	}
	rendered := out.Message + " " + out.Remediation + " " + out.Field
	if strings.Contains(rendered, pan) || strings.Contains(rendered, "4111") {
		t.Fatalf("the PAN rule echoed the matched value: %q", rendered)
	}
	if !strings.Contains(out.Message, "metadata.orderId") {
		t.Fatalf("the PAN rule did not name the offending field: %q", out.Message)
	}
}

func nest(depth int) any {
	var v any = "bottom"
	for i := 0; i < depth; i++ {
		v = map[string]any{"n": v}
	}
	return v
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
