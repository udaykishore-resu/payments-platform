package middleware

import (
	"net/http"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authz"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
)

// routeKey identifies one operation. Method *and* template, because POST /v1/payments and
// GET /v1/payments share a template and are a payment creation and a report — different
// permissions, different rate limits and different shedding classes.
type routeKey struct {
	Method   string
	Template string
}

// permissions maps every route in the contract to the permission its `security` block declares.
//
// The table is here, in one place, rather than in each handler, and that is the point: a handler
// that checks its own scope is a handler that can forget to, and a forgotten check is invisible
// because the endpoint keeps working. A route with no entry is denied by [Authorize], and the
// contract test asserts that every registered route has one — so the failure mode of adding an
// endpoint and not thinking about authorization is a red build, not a live hole.
//
// The values are transcribed from api/openapi/payments-platform.v1.yaml. Where the contract
// names an OAuth2 scope (`payments:capture`) the permission constant has the identical string
// value, so the mapping is checkable by eye.
var permissions = map[routeKey]authz.Permission{
	{http.MethodPost, httpapi.RouteCreateMerchant}:    authz.PermMerchantsWrite,
	{http.MethodGet, httpapi.RouteListMerchants}:      authz.PermMerchantsRead,
	{http.MethodGet, httpapi.RouteGetMerchant}:        authz.PermMerchantsRead,
	{http.MethodPatch, httpapi.RouteUpdateMerchant}:   authz.PermMerchantsWrite,
	{http.MethodPost, httpapi.RouteStartOnboarding}:   authz.PermOnboardingWrite,
	{http.MethodGet, httpapi.RouteGetOnboarding}:      authz.PermOnboardingRead,
	{http.MethodPost, httpapi.RouteOnboardingSignal}:  authz.PermOnboardingApprove,
	{http.MethodGet, httpapi.RouteGetConfiguration}:   authz.PermConfigRead,
	{http.MethodPut, httpapi.RoutePutConfiguration}:   authz.PermConfigWrite,
	{http.MethodGet, httpapi.RouteListConfigVersions}: authz.PermConfigRead,
	{http.MethodPost, httpapi.RouteRollbackConfig}:    authz.PermConfigRollback,
	{http.MethodGet, httpapi.RouteListGateways}:       authz.PermGatewaysRead,
	{http.MethodGet, httpapi.RouteGetGateway}:         authz.PermGatewaysRead,
	{http.MethodGet, httpapi.RouteGatewayHealth}:      authz.PermGatewaysRead,
	{http.MethodPost, httpapi.RouteRotateCredentials}: authz.PermCredentialsRotate,
	{http.MethodPost, httpapi.RouteCreatePayment}:     authz.PermPaymentsWrite,
	{http.MethodGet, httpapi.RouteListPayments}:       authz.PermPaymentsRead,
	{http.MethodGet, httpapi.RouteGetPayment}:         authz.PermPaymentsRead,
	{http.MethodPost, httpapi.RouteCapturePayment}:    authz.PermPaymentsCapture,
	{http.MethodPost, httpapi.RouteRefundPayment}:     authz.PermPaymentsRefund,
	{http.MethodPost, httpapi.RouteVoidPayment}:       authz.PermPaymentsVoid,
}

// PermissionFor returns the permission a route requires. The second result is false for a route
// with no policy, which [Authorize] treats as a denial.
func PermissionFor(method, template string) (authz.Permission, bool) {
	p, ok := permissions[routeKey{method, template}]
	return p, ok
}

// Scope is what a rate limit is counted against.
type Scope string

const (
	// ScopeTenant counts a tenant's whole traffic. Used for control-plane operations, where the
	// resource being protected is the shared control-plane database rather than one merchant's
	// throughput.
	ScopeTenant Scope = "tenant"
	// ScopeMerchant counts one merchant. Used for everything on the money path: a merchant's
	// Black Friday must not consume the budget of every other merchant on the pod.
	ScopeMerchant Scope = "merchant"
	// ScopeGateway counts one inbound gateway. Webhook ingress is limited per gateway because
	// the caller is the gateway and there is no tenant to attribute the request to until it has
	// been parsed — which is after the point where a limit is useful.
	ScopeGateway Scope = "gateway"
	// ScopeNone disables limiting. Probes and metrics only.
	ScopeNone Scope = "none"
)

// RouteLimit is one row of the contract's `x-rate-limit` extension.
type RouteLimit struct {
	Scope Scope
	// Limit and Window together give the sustained rate; Burst is the bucket depth.
	Limit  int
	Window time.Duration
	Burst  int
}

// AsLimit converts to the resilience limiter's rate-and-burst form.
//
// The conversion is rate = limit ÷ window, which is what makes `300 per 1s` and `18000 per 1m`
// the same sustained rate with very different burst behaviour — and why the contract states both
// numbers rather than one.
func (l RouteLimit) AsLimit() resilience.Limit {
	if l.Window <= 0 || l.Limit <= 0 {
		return resilience.Limit{}
	}
	burst := l.Burst
	if burst <= 0 {
		burst = l.Limit
	}
	return resilience.Limit{
		Rate:  float64(l.Limit) / l.Window.Seconds(),
		Burst: burst,
	}
}

// LimitTable resolves the limit for a route. It is an interface so a deployment can source
// limits from configuration — a merchant on an enterprise tier has different numbers — while the
// default remains the contract's own declaration.
type LimitTable interface {
	LimitFor(method, template string) (RouteLimit, bool)
}

// ContractLimits is the LimitTable transcribed from the OpenAPI `x-rate-limit` extensions.
//
// Having the numbers in code as well as in the document is duplication, and it is deliberate: a
// server that reads its limits from the document it publishes has no limit at all until the
// document loads, and a document is a file that can be absent. The contract test asserts the two
// agree.
type ContractLimits struct{}

var contractLimits = map[routeKey]RouteLimit{
	{http.MethodPost, httpapi.RouteCreateMerchant}:    {ScopeTenant, 60, time.Minute, 120},
	{http.MethodGet, httpapi.RouteListMerchants}:      {ScopeTenant, 120, time.Minute, 200},
	{http.MethodGet, httpapi.RouteGetMerchant}:        {ScopeTenant, 600, time.Minute, 1000},
	{http.MethodPatch, httpapi.RouteUpdateMerchant}:   {ScopeTenant, 60, time.Minute, 120},
	{http.MethodPost, httpapi.RouteStartOnboarding}:   {ScopeMerchant, 10, time.Minute, 10},
	{http.MethodGet, httpapi.RouteGetOnboarding}:      {ScopeMerchant, 300, time.Minute, 600},
	{http.MethodPost, httpapi.RouteOnboardingSignal}:  {ScopeMerchant, 30, time.Minute, 30},
	{http.MethodGet, httpapi.RouteGetConfiguration}:   {ScopeMerchant, 300, time.Minute, 600},
	{http.MethodPut, httpapi.RoutePutConfiguration}:   {ScopeMerchant, 30, time.Minute, 60},
	{http.MethodGet, httpapi.RouteListConfigVersions}: {ScopeMerchant, 120, time.Minute, 200},
	{http.MethodPost, httpapi.RouteRollbackConfig}:    {ScopeMerchant, 10, time.Minute, 10},
	{http.MethodGet, httpapi.RouteListGateways}:       {ScopeTenant, 120, time.Minute, 200},
	{http.MethodGet, httpapi.RouteGetGateway}:         {ScopeTenant, 300, time.Minute, 600},
	{http.MethodGet, httpapi.RouteGatewayHealth}:      {ScopeTenant, 600, time.Minute, 1000},
	{http.MethodPost, httpapi.RouteRotateCredentials}: {ScopeTenant, 10, time.Minute, 10},
	{http.MethodPost, httpapi.RouteCreatePayment}:     {ScopeMerchant, 300, time.Second, 600},
	{http.MethodGet, httpapi.RouteListPayments}:       {ScopeMerchant, 60, time.Second, 120},
	{http.MethodGet, httpapi.RouteGetPayment}:         {ScopeMerchant, 300, time.Second, 600},
	{http.MethodPost, httpapi.RouteCapturePayment}:    {ScopeMerchant, 100, time.Second, 200},
	{http.MethodPost, httpapi.RouteRefundPayment}:     {ScopeMerchant, 50, time.Second, 100},
	{http.MethodPost, httpapi.RouteVoidPayment}:       {ScopeMerchant, 50, time.Second, 100},
	{http.MethodPost, httpapi.RouteReceiveWebhook}:    {ScopeGateway, 2000, time.Second, 5000},
	{http.MethodGet, httpapi.RouteHealthz}:            {ScopeNone, 0, 0, 0},
	{http.MethodGet, httpapi.RouteReadyz}:             {ScopeNone, 0, 0, 0},
	{http.MethodGet, httpapi.RouteLivez}:              {ScopeNone, 0, 0, 0},
	{http.MethodGet, httpapi.RouteMetrics}:            {ScopeNone, 0, 0, 0},
}

// LimitFor returns the contract's declared limit for a route.
func (ContractLimits) LimitFor(method, template string) (RouteLimit, bool) {
	l, ok := contractLimits[routeKey{method, template}]
	return l, ok
}

// AllContractLimits exposes the table for the contract test, which compares it against the
// `x-rate-limit` blocks in the OpenAPI document.
func AllContractLimits() map[string]RouteLimit {
	out := make(map[string]RouteLimit, len(contractLimits))
	for k, v := range contractLimits {
		out[k.Method+" "+k.Template] = v
	}
	return out
}

// AllPermissions exposes the permission table for the same test.
func AllPermissions() map[string]authz.Permission {
	out := make(map[string]authz.Permission, len(permissions))
	for k, v := range permissions {
		out[k.Method+" "+k.Template] = v
	}
	return out
}

// IdempotentRoutes are the unsafe operations the contract marks `x-idempotent` and for which it
// declares a required Idempotency-Key parameter.
//
// Safe methods are absent: the contract marks GET `x-idempotent: true` as a statement about HTTP
// semantics, not a requirement for a key, and demanding a key on a read would break every client.
var idempotentRoutes = map[routeKey]bool{
	{http.MethodPost, httpapi.RouteCreateMerchant}:    true,
	{http.MethodPatch, httpapi.RouteUpdateMerchant}:   true,
	{http.MethodPost, httpapi.RouteStartOnboarding}:   true,
	{http.MethodPost, httpapi.RouteOnboardingSignal}:  true,
	{http.MethodPut, httpapi.RoutePutConfiguration}:   true,
	{http.MethodPost, httpapi.RouteRollbackConfig}:    true,
	{http.MethodPost, httpapi.RouteRotateCredentials}: true,
	{http.MethodPost, httpapi.RouteCreatePayment}:     true,
	{http.MethodPost, httpapi.RouteCapturePayment}:    true,
	{http.MethodPost, httpapi.RouteRefundPayment}:     true,
	{http.MethodPost, httpapi.RouteVoidPayment}:       true,
}

// RequiresIdempotencyKey reports whether a route demands an Idempotency-Key header.
func RequiresIdempotencyKey(method, template string) bool {
	return idempotentRoutes[routeKey{method, template}]
}

// AnonymousRoutes is the default bypass set: the three probes, the metrics endpoint and the
// gateway webhook ingress.
//
// /metrics is on the list because it is cluster-internal and authenticated by mTLS at the mesh,
// not by a platform token — a scrape from Prometheus carries no OAuth2 credential and never
// will. The composition root narrows or widens this per binary.
func AnonymousRoutes() map[string]bool {
	return map[string]bool{
		httpapi.RouteHealthz:        true,
		httpapi.RouteReadyz:         true,
		httpapi.RouteLivez:          true,
		httpapi.RouteMetrics:        true,
		httpapi.RouteReceiveWebhook: true,
	}
}
