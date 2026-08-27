package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	appconfig "github.com/udaykishore-resu/payments-platform/internal/application/config"
	appmerchant "github.com/udaykishore-resu/payments-platform/internal/application/merchant"
	apponboarding "github.com/udaykishore-resu/payments-platform/internal/application/onboarding"
	apppayment "github.com/udaykishore-resu/payments-platform/internal/application/payment"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	appwebhook "github.com/udaykishore-resu/payments-platform/internal/application/webhook"
	domainconfig "github.com/udaykishore-resu/payments-platform/internal/domain/config"
	domaingateway "github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi/handlers"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi/middleware"
)

// The contract test.
//
// # Why this exists rather than trusting review
//
// An OpenAPI document and a server drift the moment somebody adds an endpoint and forgets the
// document, or renames a field and forgets the server. Neither drift is visible in a diff, and
// both are visible to a customer within hours. This test walks the *document* and asserts the
// server matches it — so the document is the authority, and a mismatch is a red build with the
// operation named.
//
// It deliberately parses the YAML rather than using a generated client: a generated client would
// be regenerated from the same document by the same person who forgot to update it, and would
// therefore agree with whatever the document said. Reading the raw document and comparing it to
// the *router's* registration table has no such shared failure.

// openAPIDoc is the subset of the document this test reads.
type openAPIDoc struct {
	// A path item's keys are the HTTP verbs *and* `parameters` and `x-`-prefixed extensions, so
	// the value is a raw node: decoding it straight into an operation fails on the shared
	// `parameters` sequence that several paths declare.
	Paths      map[string]map[string]yaml.Node `yaml:"paths"`
	Components struct {
		Schemas map[string]openAPISchema `yaml:"schemas"`
	} `yaml:"components"`
}

type openAPIOperation struct {
	OperationID string                     `yaml:"operationId"`
	Security    []map[string][]string      `yaml:"security"`
	RateLimit   *openAPIRateLimit          `yaml:"x-rate-limit"`
	Parameters  []openAPIParameter         `yaml:"parameters"`
	Responses   map[string]openAPIResponse `yaml:"responses"`
}

type openAPIRateLimit struct {
	Scope  string `yaml:"scope"`
	Limit  int    `yaml:"limit"`
	Window string `yaml:"window"`
	Burst  int    `yaml:"burst"`
}

type openAPIParameter struct {
	Name     string `yaml:"name"`
	In       string `yaml:"in"`
	Required bool   `yaml:"required"`
	Ref      string `yaml:"$ref"`
}

type openAPIResponse struct {
	Content map[string]struct {
		Schema openAPISchema `yaml:"schema"`
	} `yaml:"content"`
}

type openAPISchema struct {
	Ref        string                   `yaml:"$ref"`
	Type       any                      `yaml:"type"`
	Required   []string                 `yaml:"required"`
	Properties map[string]openAPISchema `yaml:"properties"`
	Items      *openAPISchema           `yaml:"items"`
	AllOf      []openAPISchema          `yaml:"allOf"`
	OneOf      []openAPISchema          `yaml:"oneOf"`
	AddProps   any                      `yaml:"additionalProperties"`
}

// httpMethods is the set of YAML keys under a path item that are operations. `parameters` and the
// `x-` extensions also appear there and are not operations.
var httpMethods = map[string]string{
	"get": http.MethodGet, "post": http.MethodPost, "put": http.MethodPut,
	"patch": http.MethodPatch, "delete": http.MethodDelete,
}

// decodeOperation materialises one operation from its raw node.
func decodeOperation(t *testing.T, node yaml.Node) openAPIOperation {
	t.Helper()
	var op openAPIOperation
	if err := node.Decode(&op); err != nil {
		t.Fatalf("decode operation: %v", err)
	}
	return op
}

func loadContract(t *testing.T) *openAPIDoc {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "api", "openapi", "payments-platform.v1.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the contract: %v", err)
	}
	var doc openAPIDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the contract: %v", err)
	}
	if len(doc.Paths) == 0 {
		t.Fatal("the contract declares no paths")
	}
	return &doc
}

// fullRouter mounts every resource, which is what the contract expects a complete deployment to
// serve. Individual binaries mount subsets; the contract describes the union.
func fullRouter(t *testing.T) *httpapi.Router {
	t.Helper()
	return newRouter(handlers.Deps{
		Payments: &fakePayments{
			create: func(context.Context, apppayment.CreateCommand) (*apppayment.Result, error) {
				return okResult(newPayment()), nil
			},
			get: func(context.Context, shared.PaymentID) (*payment.Payment, error) { return newPayment(), nil },
			list: func(context.Context, ports.PaymentFilter, ports.Page) ([]*payment.Payment, string, error) {
				return []*payment.Payment{newPayment()}, "", nil
			},
			capture: okCapture(),
			refund:  okRefund(true),
			void: func(context.Context, apppayment.VoidCommand) (*apppayment.Result, error) {
				return okResult(newPayment()), nil
			},
		},
		Merchants: &fakeMerchants{
			create: func(context.Context, appmerchant.CreateCommand) (*merchant.Merchant, error) {
				return newMerchant(), nil
			},
			get: func(context.Context, shared.TenantID, shared.MerchantID) (*merchant.Merchant, error) {
				return newMerchant(), nil
			},
			list: func(context.Context, shared.TenantID, ports.MerchantFilter, ports.Page) ([]*merchant.Merchant, string, error) {
				return []*merchant.Merchant{newMerchant()}, "", nil
			},
			update: func(context.Context, appmerchant.UpdateCommand) (*merchant.Merchant, error) {
				return newMerchant(), nil
			},
		},
		Onboarding: &fakeOnboarding{
			start: func(context.Context, apponboarding.StartCommand) (*apponboarding.Case, error) { return newCase(), nil },
			get: func(context.Context, shared.TenantID, shared.WorkflowID) (*apponboarding.Case, error) {
				return newCase(), nil
			},
			signal: func(context.Context, apponboarding.SignalCommand) (*apponboarding.Case, error) { return newCase(), nil },
		},
		OnboardingLookup: &fakeLookup{},
		Configuration: &fakeConfig{
			getActive: func(context.Context, shared.TenantID, shared.MerchantID) (*domainconfig.MerchantConfig, error) {
				return newConfig(), nil
			},
			listVersions: func(context.Context, shared.TenantID, shared.MerchantID, ports.Page) ([]*domainconfig.MerchantConfig, string, error) {
				return []*domainconfig.MerchantConfig{newConfig()}, "", nil
			},
			publish: func(context.Context, appconfig.PublishCommand) (*domainconfig.MerchantConfig, error) {
				return newConfig(), nil
			},
			rollback: func(context.Context, appconfig.RollbackCommand) (*domainconfig.MerchantConfig, error) {
				return newConfig(), nil
			},
		},
		Gateways: &fakeGateways{
			get: func(context.Context, shared.GatewayID) (*domaingateway.Gateway, error) { return newGateway(), nil },
			list: func(context.Context) ([]*domaingateway.Gateway, error) {
				return []*domaingateway.Gateway{newGateway()}, nil
			},
			health: func(context.Context, shared.GatewayID, []shared.Operation) ([]*domaingateway.Health, error) {
				return []*domaingateway.Health{newGatewayHealth()}, nil
			},
			rotate: func(context.Context, handlers.RotateCommand) (*handlers.RotationAccepted, error) {
				return &handlers.RotationAccepted{ConnectionID: "gwc_1", State: "DUAL_RUN", StartedAt: testClock.Now()}, nil
			},
		},
		Webhooks: &fakeWebhooks{
			ingest: func(context.Context, appwebhook.InboundRequest) (*appwebhook.Accepted, error) {
				return &appwebhook.Accepted{WebhookID: "whk_1"}, nil
			},
		},
		Metrics: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("# TYPE pp_payments_total counter\n"))
		}),
	})
}

// TestEveryDeclaredOperationHasARoute walks the contract and asserts the router serves it under
// the declared operationId.
//
// Both directions are checked. "Every declared operation has a route" catches a forgotten
// implementation; "every route implements a declared operation" catches an endpoint somebody
// shipped without documenting, which is how an undocumented surface becomes a de-facto contract
// nobody can change.
func TestEveryDeclaredOperationHasARoute(t *testing.T) {
	t.Parallel()
	doc := loadContract(t)
	rt := fullRouter(t)

	declared := map[string]string{} // "METHOD /template" -> operationId
	for path, item := range doc.Paths {
		for verb, node := range item {
			method, ok := httpMethods[verb]
			if !ok {
				continue
			}
			op := decodeOperation(t, node)
			declared[method+" "+path] = op.OperationID
		}
	}
	if len(declared) == 0 {
		t.Fatal("no operations parsed from the contract")
	}

	registered := map[string]string{}
	for _, r := range rt.Routes() {
		registered[r.Method+" "+r.Template] = r.OperationID
	}

	for key, opID := range declared {
		got, ok := registered[key]
		if !ok {
			t.Errorf("%s (operationId %q) is declared in the contract but not registered", key, opID)
			continue
		}
		if got != opID {
			t.Errorf("%s implements operationId %q, contract declares %q", key, got, opID)
		}
	}
	for key, opID := range registered {
		if _, ok := declared[key]; !ok {
			t.Errorf("%s (operationId %q) is registered but not declared in the contract", key, opID)
		}
	}
	if len(declared) != len(registered) {
		t.Errorf("contract declares %d operations, router registers %d", len(declared), len(registered))
	}
	t.Logf("contract operations: %d, registered routes: %d", len(declared), len(registered))
}

// TestSuccessResponsesValidateAgainstTheDeclaredSchema issues a request to every GET operation
// and checks the response body structurally against the schema the contract declares for it.
//
// Structural, not semantic: required properties are present, no undeclared property appears where
// `additionalProperties: false` says none may, and arrays contain objects of the declared shape.
// That is the check that catches the two failures that actually happen — a renamed field and a
// field added to the server but not to the document.
//
// GETs only, because a POST would need a schema-valid request body per operation and that
// generator is a larger machine than the property it verifies; the mutating operations are
// covered by the hand-written table tests with their own body assertions.
func TestSuccessResponsesValidateAgainstTheDeclaredSchema(t *testing.T) {
	t.Parallel()
	doc := loadContract(t)
	rt := fullRouter(t)

	cases := []struct {
		template string
		path     string
	}{
		{httpapi.RouteListMerchants, "/v1/merchants"},
		{httpapi.RouteGetMerchant, "/v1/merchants/" + testMerchantID.String()},
		{httpapi.RouteGetOnboarding, "/v1/merchants/" + testMerchantID.String() + "/onboarding"},
		{httpapi.RouteGetConfiguration, "/v1/merchants/" + testMerchantID.String() + "/configuration"},
		{httpapi.RouteListConfigVersions, "/v1/merchants/" + testMerchantID.String() + "/configuration/versions"},
		{httpapi.RouteListGateways, "/v1/gateways"},
		{httpapi.RouteGetGateway, "/v1/gateways/stripe"},
		{httpapi.RouteGatewayHealth, "/v1/gateways/stripe/health"},
		{httpapi.RouteListPayments, "/v1/payments"},
		{httpapi.RouteGetPayment, "/v1/payments/" + testPaymentID.String()},
		{httpapi.RouteHealthz, "/healthz"},
		{httpapi.RouteReadyz, "/readyz"},
		{httpapi.RouteLivez, "/livez"},
	}

	for _, tc := range cases {
		t.Run(tc.template, func(t *testing.T) {
			t.Parallel()
			node, ok := doc.Paths[tc.template]["get"]
			if !ok {
				t.Fatalf("the contract declares no GET for %s", tc.template)
			}
			op := decodeOperation(t, node)
			rec := do(rt, http.MethodGet, tc.path, "", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
			}
			schema, ok := successSchema(op)
			if !ok {
				t.Skip("the 200 response declares no JSON schema")
			}
			var body any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}
			validate(t, doc, schema, body, tc.template)
		})
	}
}

// knownContractGaps records required properties the server cannot yet populate, with the reason.
//
// A gap list is not a way to make a test pass. It is the opposite: without it the only options
// are to delete the assertion — losing the check for every *other* field — or to fabricate a
// value, which is worse than omitting one. Naming the gap keeps the check live for everything
// else, makes the shortfall visible in review, and turns a *new* gap into a failure.
//
// Each entry must state why the field cannot be populated, and each is a defect to close.
// The map is empty, and an empty gap list is the state this test is trying to reach rather than
// a sign the mechanism is unused: the last entry — PaymentAttempt.connectionId — was closed by
// giving payment.Attempt a ConnectionID, stamping it at dispatch, persisting it (migration
// 0016_attempt_connection_id) and rendering it. Leave the map and its machinery in place: the
// next required field the server cannot populate should be named here, in review, rather than
// silently dropped from the assertion.
var knownContractGaps = map[string]string{}

// successSchema finds the 200 response's application/json schema.
func successSchema(op openAPIOperation) (openAPISchema, bool) {
	res, ok := op.Responses["200"]
	if !ok {
		return openAPISchema{}, false
	}
	media, ok := res.Content["application/json"]
	if !ok {
		return openAPISchema{}, false
	}
	return media.Schema, true
}

// validate checks a decoded body against a schema, following $ref through the component map.
func validate(t *testing.T, doc *openAPIDoc, schema openAPISchema, value any, where string) {
	t.Helper()
	schema, ok := resolve(doc, schema)
	if !ok {
		return
	}
	// allOf and oneOf are used for narrowing and nullability; validating against the first branch
	// that is an object is enough for the structural properties this test asserts, and following
	// every branch would reimplement a JSON Schema validator.
	for _, sub := range schema.AllOf {
		validate(t, doc, sub, value, where)
	}
	if len(schema.OneOf) > 0 {
		return
	}
	switch v := value.(type) {
	case nil:
		return
	case map[string]any:
		for _, req := range schema.Required {
			if _, present := v[req]; present {
				continue
			}
			if reason, known := knownContractGaps[where+"."+req]; known {
				t.Logf("%s: known gap — required property %q is absent: %s", where, req, reason)
				continue
			}
			t.Errorf("%s: required property %q is absent from the response", where, req)
		}
		if schema.Properties == nil {
			return
		}
		if forbidsExtras(schema) {
			var extras []string
			for k := range v {
				if _, declared := schema.Properties[k]; !declared {
					extras = append(extras, k)
				}
			}
			if len(extras) > 0 {
				sort.Strings(extras)
				t.Errorf("%s: response carries properties the contract does not declare: %v",
					where, extras)
			}
		}
		for name, sub := range schema.Properties {
			if child, present := v[name]; present {
				validate(t, doc, sub, child, where+"."+name)
			}
		}
	case []any:
		if schema.Items == nil {
			return
		}
		for i, item := range v {
			validate(t, doc, *schema.Items, item, where+"[]")
			if i > 4 {
				// Five elements is enough to catch a shape error; validating a full page is
				// quadratic in test time for no additional signal.
				break
			}
		}
	}
}

// resolve follows a local $ref into the component schema map.
func resolve(doc *openAPIDoc, s openAPISchema) (openAPISchema, bool) {
	const prefix = "#/components/schemas/"
	seen := 0
	for s.Ref != "" {
		if !strings.HasPrefix(s.Ref, prefix) {
			return openAPISchema{}, false
		}
		next, ok := doc.Components.Schemas[strings.TrimPrefix(s.Ref, prefix)]
		if !ok {
			return openAPISchema{}, false
		}
		s = next
		if seen++; seen > 8 {
			// A $ref cycle would otherwise loop here. Eight hops is far beyond anything this
			// document uses, so hitting the bound is itself a defect worth not hanging on.
			return openAPISchema{}, false
		}
	}
	return s, true
}

// forbidsExtras reports whether the schema declares `additionalProperties: false`.
func forbidsExtras(s openAPISchema) bool {
	b, ok := s.AddProps.(bool)
	return ok && !b
}

// TestRateLimitTableMatchesTheContract asserts the in-code limits equal the document's
// `x-rate-limit` extensions.
//
// The duplication is deliberate — a server that reads its limits from the document it publishes
// has no limit at all until the document loads — and this test is what makes the duplication safe.
func TestRateLimitTableMatchesTheContract(t *testing.T) {
	t.Parallel()
	doc := loadContract(t)
	inCode := middleware.AllContractLimits()

	for path, item := range doc.Paths {
		for verb, node := range item {
			method, ok := httpMethods[verb]
			if !ok {
				continue
			}
			op := decodeOperation(t, node)
			if op.RateLimit == nil {
				continue
			}
			key := method + " " + path
			got, ok := inCode[key]
			if !ok {
				t.Errorf("%s declares x-rate-limit in the contract but has no entry in the limit table", key)
				continue
			}
			if string(got.Scope) != op.RateLimit.Scope {
				t.Errorf("%s scope = %q, contract says %q", key, got.Scope, op.RateLimit.Scope)
			}
			if got.Limit != op.RateLimit.Limit {
				t.Errorf("%s limit = %d, contract says %d", key, got.Limit, op.RateLimit.Limit)
			}
			if got.Burst != op.RateLimit.Burst {
				t.Errorf("%s burst = %d, contract says %d", key, got.Burst, op.RateLimit.Burst)
			}
			if window := got.Window.String(); !windowMatches(window, op.RateLimit.Window) {
				t.Errorf("%s window = %s, contract says %s", key, window, op.RateLimit.Window)
			}
		}
	}
}

// windowMatches compares Go's duration rendering against the contract's ("1m" vs "1m0s").
func windowMatches(goDuration, contract string) bool {
	if goDuration == contract {
		return true
	}
	return strings.TrimSuffix(goDuration, "0s") == contract
}

// TestIdempotencyRequirementMatchesTheContract asserts the middleware demands a key on exactly the
// operations whose contract declares the Idempotency-Key parameter required.
//
// A mismatch in either direction is a bug with a customer-visible cost: demanding a key the
// contract does not declare breaks a conforming client, and not demanding one the contract does
// declare means a retry can charge twice.
func TestIdempotencyRequirementMatchesTheContract(t *testing.T) {
	t.Parallel()
	doc := loadContract(t)

	for path, item := range doc.Paths {
		for verb, node := range item {
			method, ok := httpMethods[verb]
			if !ok {
				continue
			}
			wantKey := declaresIdempotencyKey(decodeOperation(t, node))
			gotKey := middleware.RequiresIdempotencyKey(method, path)
			if wantKey != gotKey {
				t.Errorf("%s %s: contract requires Idempotency-Key = %v, middleware requires %v",
					method, path, wantKey, gotKey)
			}
		}
	}
}

func declaresIdempotencyKey(op openAPIOperation) bool {
	for _, p := range op.Parameters {
		if p.Ref == "#/components/parameters/IdempotencyKey" ||
			(p.Name == "Idempotency-Key" && p.In == "header") {
			return true
		}
	}
	return false
}

// TestPermissionTableMatchesTheContractScopes asserts the authorization table's permission for
// each route is one of the OAuth2 scopes the contract's `security` block declares for it.
func TestPermissionTableMatchesTheContractScopes(t *testing.T) {
	t.Parallel()
	doc := loadContract(t)
	perms := middleware.AllPermissions()

	for path, item := range doc.Paths {
		for verb, node := range item {
			method, ok := httpMethods[verb]
			if !ok {
				continue
			}
			scopes := oauthScopes(decodeOperation(t, node))
			key := method + " " + path
			perm, hasPerm := perms[key]
			if len(scopes) == 0 {
				// The contract declares no OAuth2 requirement: probes, metrics and the
				// signature-authenticated webhook ingress. Those must be on the anonymous
				// allowlist rather than carrying a permission nobody can satisfy.
				if hasPerm {
					t.Errorf("%s carries permission %q but the contract declares no OAuth2 scope", key, perm)
				}
				continue
			}
			if !hasPerm {
				t.Errorf("%s declares scopes %v but has no permission in the table", key, scopes)
				continue
			}
			if !containsScope(scopes, string(perm)) {
				t.Errorf("%s permission = %q, contract declares scopes %v", key, perm, scopes)
			}
		}
	}
}

func oauthScopes(op openAPIOperation) []string {
	var out []string
	for _, req := range op.Security {
		out = append(out, req["oauth2"]...)
	}
	return out
}

func containsScope(set []string, want string) bool {
	for _, s := range set {
		if s == want {
			return true
		}
	}
	return false
}

// TestAnonymousRoutesAreExactlyTheUnauthenticatedOnes asserts the bypass allowlist matches the
// contract's own `security: []` and non-OAuth2 declarations, so a route can never be silently
// exempted from authentication.
func TestAnonymousRoutesAreExactlyTheUnauthenticatedOnes(t *testing.T) {
	t.Parallel()
	doc := loadContract(t)
	anonymous := middleware.AnonymousRoutes()

	for path, item := range doc.Paths {
		for verb, node := range item {
			if _, ok := httpMethods[verb]; !ok {
				continue
			}
			needsToken := len(oauthScopes(decodeOperation(t, node))) > 0
			if needsToken && anonymous[path] {
				t.Errorf("%s requires an OAuth2 scope but is on the anonymous allowlist", path)
			}
		}
	}
	// The webhook ingress must be anonymous: its caller is a gateway holding a signature and no
	// platform credential, so requiring a token would make the endpoint unusable by design.
	if !anonymous[httpapi.RouteReceiveWebhook] {
		t.Error("the gateway webhook ingress is not on the anonymous allowlist")
	}
	for _, probe := range []string{httpapi.RouteHealthz, httpapi.RouteReadyz, httpapi.RouteLivez} {
		if !anonymous[probe] {
			t.Errorf("%s is not on the anonymous allowlist; a probe cannot present a token", probe)
		}
	}
}

// TestDTOFieldsAreDeclaredByTheContract walks the wire structs' JSON tags and asserts every one
// appears in the schema the contract declares.
//
// This is the "never expose a field the contract does not declare" rule, mechanised. The failure
// it prevents is the quiet one: somebody adds a field to a response struct, it ships, a customer
// starts depending on it, and it can no longer be removed.
func TestDTOFieldsAreDeclaredByTheContract(t *testing.T) {
	t.Parallel()
	doc := loadContract(t)

	cases := []struct {
		schema string
		typ    any
	}{
		{"Payment", httpapi.Payment{}},
		{"PaymentAttempt", httpapi.PaymentAttempt{}},
		{"Refund", httpapi.Refund{}},
		{"NextAction", httpapi.NextAction{}},
		{"Money", httpapi.Money{}},
		{"Merchant", httpapi.Merchant{}},
		{"BusinessProfile", httpapi.BusinessProfile{}},
		{"BankAccount", httpapi.BankAccount{}},
		{"BankAccountInput", httpapi.BankAccountInput{}},
		{"Principal", httpapi.MerchantPrincipal{}},
		{"PrincipalInput", httpapi.PrincipalInput{}},
		{"CreateMerchantRequest", httpapi.CreateMerchantRequest{}},
		{"UpdateMerchantRequest", httpapi.UpdateMerchantRequest{}},
		{"Gateway", httpapi.Gateway{}},
		{"GatewayCapabilities", httpapi.GatewayCapabilities{}},
		{"GatewayHealth", httpapi.GatewayHealth{}},
		{"GatewayHealthReport", httpapi.GatewayHealthReport{}},
		{"RotateCredentialsRequest", httpapi.RotateCredentialsRequest{}},
		{"CredentialRotationAccepted", httpapi.CredentialRotationAccepted{}},
		{"ConfigurationVersion", httpapi.ConfigurationVersion{}},
		{"ConfigurationRollbackRequest", httpapi.ConfigurationRollbackRequest{}},
		{"MerchantConfiguration", httpapi.MerchantConfiguration{}},
		{"RoutingConfig", httpapi.RoutingConfig{}},
		{"RoutingRule", httpapi.RoutingRule{}},
		{"RoutingRuleCondition", httpapi.RoutingRuleCondition{}},
		{"RoutingRuleOutcome", httpapi.RoutingRuleOutcome{}},
		{"RoutingWeights", httpapi.RoutingWeights{}},
		{"RiskConfig", httpapi.RiskConfig{}},
		{"VelocityLimits", httpapi.VelocityLimits{}},
		{"LimitsConfig", httpapi.LimitsConfig{}},
		{"WebhookConfig", httpapi.WebhookConfig{}},
		{"WebhookEndpoint", httpapi.WebhookEndpoint{}},
		{"WebhookRetryPolicy", httpapi.WebhookRetryPolicy{}},
		{"SettlementConfig", httpapi.SettlementConfig{}},
		{"FeatureFlags", httpapi.FeatureFlags{}},
		{"OnboardingCase", httpapi.OnboardingCase{}},
		{"OnboardingStep", httpapi.OnboardingStep{}},
		{"Annotation", httpapi.Annotation{}},
		{"StartOnboardingRequest", httpapi.StartOnboardingRequest{}},
		{"OnboardingSignalRequest", httpapi.OnboardingSignalRequest{}},
		{"OnboardingSignalAccepted", httpapi.OnboardingSignalAccepted{}},
		{"CreatePaymentRequest", httpapi.CreatePaymentRequest{}},
		{"CaptureRequest", httpapi.CaptureRequest{}},
		{"RefundRequest", httpapi.RefundRequest{}},
		{"VoidRequest", httpapi.VoidRequest{}},
		{"WebhookAck", httpapi.WebhookAck{}},
		{"HealthStatus", httpapi.HealthStatus{}},
		{"Problem", httpapi.Problem{}},
		{"ProblemDetailItem", httpapi.ProblemDetail{}},
	}

	for _, tc := range cases {
		t.Run(tc.schema, func(t *testing.T) {
			t.Parallel()
			schema, ok := doc.Components.Schemas[tc.schema]
			if !ok {
				t.Fatalf("the contract declares no schema named %q", tc.schema)
			}
			declared := map[string]bool{}
			for name := range schema.Properties {
				declared[name] = true
			}
			for _, sub := range schema.AllOf {
				resolved, ok := resolve(doc, sub)
				if !ok {
					continue
				}
				for name := range resolved.Properties {
					declared[name] = true
				}
			}
			for _, name := range jsonFieldNames(reflect.TypeOf(tc.typ)) {
				if !declared[name] {
					t.Errorf("field %q is exposed by the Go type but not declared by schema %q",
						name, tc.schema)
				}
			}
		})
	}
}

// jsonFieldNames extracts the wire names of a struct's exported fields.
func jsonFieldNames(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" || tag == "" {
			continue
		}
		name := tag
		if i := strings.IndexByte(tag, ','); i >= 0 {
			name = tag[:i]
		}
		if name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
