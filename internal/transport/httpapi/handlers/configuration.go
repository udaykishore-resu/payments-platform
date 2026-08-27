package handlers

import (
	"context"
	"net/http"
	"strings"

	appconfig "github.com/udaykishore-resu/payments-platform/internal/application/config"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/config"
	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/routing"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// ConfigurationService is the desired-state configuration store this file exposes.
type ConfigurationService interface {
	GetActive(ctx context.Context, tenantID shared.TenantID, m shared.MerchantID) (*config.MerchantConfig, error)
	ListVersions(ctx context.Context, tenantID shared.TenantID, m shared.MerchantID, page ports.Page) ([]*config.MerchantConfig, string, error)
	Publish(ctx context.Context, cmd appconfig.PublishCommand) (*config.MerchantConfig, error)
	Rollback(ctx context.Context, cmd appconfig.RollbackCommand) (*config.MerchantConfig, error)
}

func registerConfiguration(rt *httpapi.Router, d Deps) {
	h := &configHandlers{svc: d.Configuration, baseURL: d.BaseURL}
	rt.Handle(http.MethodGet, httpapi.RouteGetConfiguration, "getMerchantConfiguration", h.get)
	rt.Handle(http.MethodPut, httpapi.RoutePutConfiguration, "putMerchantConfiguration", h.put)
	rt.Handle(http.MethodGet, httpapi.RouteListConfigVersions, "listConfigurationVersions", h.listVersions)
	rt.Handle(http.MethodPost, httpapi.RouteRollbackConfig, "rollbackConfiguration", h.rollback)
}

type configHandlers struct {
	svc     ConfigurationService
	baseURL string
}

// get implements `getMerchantConfiguration`.
//
// The ETag is the document digest, not a version counter, which is deliberate: two publishes
// that produce byte-identical documents have the same digest, so a rollback to a version whose
// content already matches is a no-op the caller can detect without diffing. The contract calls
// that digest equality "the machine-checkable proof that the rollback is faithful".
func (h *configHandlers) get(w http.ResponseWriter, r *http.Request) error {
	id, tc, err := merchantTarget(r)
	if err != nil {
		return err
	}
	c, err := h.svc.GetActive(r.Context(), tc.TenantID, id)
	if err != nil {
		return err
	}
	if notModified(w, r, c.ETag()) {
		return nil
	}
	httpapi.SetETagRaw(w, c.ETag())
	httpapi.WriteJSON(w, r, http.StatusOK, httpapi.ConfigurationVersionOf(c))
	return nil
}

// put implements `putMerchantConfiguration`.
//
// # Why a concurrent publish loses rather than merges
//
// If-Match is required, and a mismatch is 412. The platform does not merge configuration
// documents on the caller's behalf, and the reason is that a merge of two routing policies is
// not a textual operation: merging "primary: stripe" with "primary: adyen" has no correct answer,
// and merging two rule lists produces an order neither author intended — which silently sends a
// subset of traffic somewhere nobody chose. Losing the race and being told to re-read, re-merge
// and retry is the only outcome that keeps a human in the decision.
func (h *configHandlers) put(w http.ResponseWriter, r *http.Request) error {
	id, tc, err := merchantTarget(r)
	if err != nil {
		return err
	}
	ifMatch, err := httpapi.RequireIfMatch(r)
	if err != nil {
		return err
	}
	var req httpapi.MerchantConfiguration
	if err := decodeInto(r, &req); err != nil {
		return err
	}
	draft, err := configurationToDomain(req, tc.TenantID, id)
	if err != nil {
		return err
	}
	c, err := h.svc.Publish(r.Context(), appconfig.PublishCommand{
		TenantID:   tc.TenantID,
		MerchantID: id,
		Draft:      draft,
		IfMatch:    ifMatch,
		Actor:      configActor(r),
	})
	if err != nil {
		return err
	}
	httpapi.SetETagRaw(w, c.ETag())
	httpapi.WriteJSON(w, r, http.StatusOK, httpapi.ConfigurationVersionOf(c))
	return nil
}

// listVersions implements `listConfigurationVersions`.
func (h *configHandlers) listVersions(w http.ResponseWriter, r *http.Request) error {
	id, tc, err := merchantTarget(r)
	if err != nil {
		return err
	}
	page, err := httpapi.DecodePage(r)
	if err != nil {
		return err
	}
	items, next, err := h.svc.ListVersions(r.Context(), tc.TenantID, id,
		ports.Page{Limit: page.Limit, Cursor: page.Cursor})
	if err != nil {
		return err
	}
	out := make([]httpapi.ConfigurationVersion, 0, len(items))
	for _, c := range items {
		out = append(out, httpapi.ConfigurationVersionOf(c))
	}
	httpapi.WriteJSON(w, r, http.StatusOK, httpapi.PageOf(out, next))
	return nil
}

// rollback implements `rollbackConfiguration`.
//
// A rollback is an *append*, never a deletion: the target document is re-published as version
// n+1, so the audit trail retains the version being abandoned. 201 rather than 200 says exactly
// that — a new resource was created.
//
// The target is re-validated at rollback time rather than trusted because it was valid when first
// published. A gateway may since have been de-registered, a certification may have expired, a
// residency policy may have tightened. A rollback that cannot be validated is not a safe
// rollback, and the service returns 422 CONFIGURATION_INVALID naming the rules that now fail.
func (h *configHandlers) rollback(w http.ResponseWriter, r *http.Request) error {
	id, tc, err := merchantTarget(r)
	if err != nil {
		return err
	}
	var req httpapi.ConfigurationRollbackRequest
	if err := decodeInto(r, &req); err != nil {
		return err
	}
	if req.ToVersion < 1 {
		return apierror.New(apierror.CodeValidationFailed, "toVersion must be at least 1").
			WithDetail(apierror.Detail{
				Field: "toVersion", Code: "OUT_OF_RANGE",
				Message: "Configuration versions are dense and start at 1.",
				RuleID:  "L1.VERSION_RANGE",
			})
	}
	if strings.TrimSpace(req.Reason) == "" {
		return apierror.New(apierror.CodeValidationFailed, "a rollback reason is required").
			WithDetail(apierror.Detail{
				Field: "reason", Code: "MISSING",
				Message: "A rollback is an operator action and its justification is part of the audit record.",
				RuleID:  "L1.ROLLBACK_REASON_PRESENT",
			})
	}
	c, err := h.svc.Rollback(r.Context(), appconfig.RollbackCommand{
		TenantID:   tc.TenantID,
		MerchantID: id,
		ToVersion:  req.ToVersion,
		IfMatch:    r.Header.Get(httpapi.HeaderIfMatch),
		Actor:      configActor(r),
	})
	if err != nil {
		return err
	}
	httpapi.SetETagRaw(w, c.ETag())
	httpapi.SetLocation(w, h.baseURL, "/v1/merchants/"+id.String()+"/configuration")
	httpapi.WriteJSON(w, r, http.StatusCreated, httpapi.ConfigurationVersionOf(c))
	return nil
}

func configActor(r *http.Request) appconfig.Actor {
	p := httpapi.Principal(r.Context())
	if p == nil {
		return appconfig.Actor{}
	}
	return appconfig.Actor{ID: p.ID, Name: p.Name}
}

// configurationToDomain converts the submitted document.
//
// Every enum and identifier is parsed here rather than deeper. The L4 validator that runs inside
// the service checks *semantics* — does this merchant have a connection to the primary gateway,
// does the refund window exceed what the gateway supports — and it can only do that on a document
// whose syntax already holds. Splitting the two means an L4 failure always names a real policy
// problem rather than a typo.
// ConfigurationToDomain converts a decoded configuration document into the domain aggregate.
//
// Exported because the operator CLI's offline `config validate` runs exactly this conversion: the
// aggregate's constructor is where every value the domain has an opinion about — currencies,
// methods, countries, gateway slugs, amounts, the routing policy's internal consistency — is
// checked, and a second implementation of that mapping in the CLI would be a second thing to keep
// in step with the contract.
func ConfigurationToDomain(in httpapi.MerchantConfiguration, t shared.TenantID, m shared.MerchantID) (*config.MerchantConfig, error) {
	return configurationToDomain(in, t, m)
}

func configurationToDomain(in httpapi.MerchantConfiguration, t shared.TenantID, m shared.MerchantID) (*config.MerchantConfig, error) {
	env, err := shared.ParseEnvironment(strings.ToLower(in.Environment))
	if err != nil {
		return nil, err
	}
	out := &config.MerchantConfig{
		MerchantID:  m,
		TenantID:    t,
		Version:     in.Version,
		Status:      config.Status(in.Status),
		Environment: env,
		Limits: config.Limits{
			MaxRefundWindowDays: in.Limits.MaxRefundWindowDays,
			MaxPartialCaptures:  in.Limits.MaxPartialCaptures,
		},
		Webhook: config.WebhookConfig{
			MaxAttempts: in.Webhooks.RetryPolicy.MaxAttempts,
			Backoff:     in.Webhooks.RetryPolicy.Backoff,
		},
		Settle: config.SettlementConfig{
			Schedule: in.Settlement.Schedule,
			HoldDays: in.Settlement.HoldDays,
		},
	}
	if out.Status == "" {
		out.Status = config.StatusDraft
	}
	for _, code := range in.SupportedCurrencies {
		cur, err := money.ParseCurrency(code)
		if err != nil {
			return nil, err
		}
		out.SupportedCurrencies = append(out.SupportedCurrencies, cur)
	}
	for _, code := range in.PaymentMethods {
		pm, err := shared.ParsePaymentMethod(code)
		if err != nil {
			return nil, err
		}
		out.PaymentMethods = append(out.PaymentMethods, pm)
	}
	for _, code := range in.Countries {
		c, err := shared.ParseCountry(code)
		if err != nil {
			return nil, err
		}
		out.Countries = append(out.Countries, c)
	}
	if out.Routing, err = routingToDomain(in.Routing); err != nil {
		return nil, err
	}
	if out.Risk, err = riskToDomain(in.Risk); err != nil {
		return nil, err
	}
	cur, curErr := money.ParseCurrency(in.Settlement.Currency)
	if curErr != nil {
		return nil, curErr
	}
	out.Settle.Currency = cur
	for _, ep := range in.Webhooks.Endpoints {
		out.Webhook.Endpoints = append(out.Webhook.Endpoints, config.WebhookEndpoint{
			URL:       ep.URL,
			Events:    ep.Events,
			SecretRef: ep.SecretRef,
			Active:    ep.Active,
		})
	}
	if in.FeatureFlags != nil {
		out.FeatureFlags = map[string]bool{
			"networkTokens":     in.FeatureFlags.NetworkTokens,
			"partialCapture":    in.FeatureFlags.PartialCapture,
			"multiCapture":      in.FeatureFlags.MultiCapture,
			"threeDsExemptions": in.FeatureFlags.ThreeDsExemptions,
		}
	}
	return out, nil
}

func routingToDomain(in httpapi.RoutingConfig) (routing.Policy, error) {
	primary, err := shared.ParseGatewayID(in.Primary)
	if err != nil {
		return routing.Policy{}, err
	}
	out := routing.Policy{
		Strategy: routing.Strategy(in.Strategy),
		Primary:  primary,
	}
	for _, code := range in.Fallback {
		g, err := shared.ParseGatewayID(code)
		if err != nil {
			return routing.Policy{}, err
		}
		out.Fallbacks = append(out.Fallbacks, g)
	}
	if in.Weights != nil {
		out.Weights = routing.Weights{
			Health:      in.Weights.Health,
			SuccessRate: in.Weights.SuccessRate,
			Cost:        in.Weights.Cost,
			Latency:     in.Weights.Latency,
		}
	}
	for i, rule := range in.Rules {
		r, err := routingRuleToDomain(i, rule)
		if err != nil {
			return routing.Policy{}, err
		}
		out.Rules = append(out.Rules, r)
	}
	return out, nil
}

func routingRuleToDomain(index int, in httpapi.RoutingRule) (routing.Rule, error) {
	primary, err := shared.ParseGatewayID(in.Then.Primary)
	if err != nil {
		return routing.Rule{}, err
	}
	out := routing.Rule{
		// The contract's rules are anonymous, but the domain's validator reports problems by rule
		// ID and a merchant reading "rule  is shadowed" learns nothing. Numbering by position
		// gives every rule a stable, meaningful name that matches what the operator sees in their
		// own document.
		ID:     "rule-" + itoa(index+1),
		Action: routing.Action{Primary: primary},
	}
	for _, code := range in.Then.Fallback {
		g, err := shared.ParseGatewayID(code)
		if err != nil {
			return routing.Rule{}, err
		}
		out.Action.Fallbacks = append(out.Action.Fallbacks, g)
	}
	if in.When.Currency != "" {
		cur, err := money.ParseCurrency(in.When.Currency)
		if err != nil {
			return routing.Rule{}, err
		}
		out.Condition.Currency = cur
	}
	if in.When.PaymentMethod != "" {
		pm, err := shared.ParsePaymentMethod(in.When.PaymentMethod)
		if err != nil {
			return routing.Rule{}, err
		}
		out.Condition.PaymentMethod = pm
	}
	if in.When.Country != "" {
		c, err := shared.ParseCountry(in.When.Country)
		if err != nil {
			return routing.Rule{}, err
		}
		out.Condition.Country = c
	}
	if in.When.AmountAbove != nil {
		amt, err := in.When.AmountAbove.ToDomain()
		if err != nil {
			return routing.Rule{}, err
		}
		out.Condition.AmountAbove = &amt
	}
	return out, nil
}

func riskToDomain(in httpapi.RiskConfig) (risk.Policy, error) {
	maxTxn, err := in.MaxTransactionAmount.ToDomain()
	if err != nil {
		return risk.Policy{}, err
	}
	daily, err := in.DailyVolumeLimit.ToDomain()
	if err != nil {
		return risk.Policy{}, err
	}
	out := risk.Policy{
		MaxTransactionAmount: maxTxn,
		DailyVolumeLimit:     daily,
		Velocity: risk.Velocity{
			MaxPaymentsPerMinute: in.Velocity.MaxPaymentsPerMinute,
			MaxPerCardPerHour:    in.Velocity.MaxPerCardPerHour,
		},
	}
	if in.Require3DSAbove != nil {
		v, err := in.Require3DSAbove.ToDomain()
		if err != nil {
			return risk.Policy{}, err
		}
		out.Require3DSAbove = v
	}
	// FallbackDecision maps onto the per-check failure postures rather than onto a single field:
	// the domain's fallback is per check, because "the velocity store is down" and "the scorer is
	// down" are different failures with different safe answers. The contract's single value is
	// applied uniformly, which is the closest faithful reading of a one-value knob.
	if in.FallbackDecision != "" {
		out.Postures = posturesFor(in.FallbackDecision)
	}
	for _, code := range in.BlockedCountries {
		c, err := shared.ParseCountry(code)
		if err != nil {
			return risk.Policy{}, err
		}
		out.BlockedCountries = append(out.BlockedCountries, c)
	}
	return out, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// posturesFor expands the contract's single fallbackDecision into the domain's per-check posture
// map.
//
// The domain models fallback per check because the safe answer differs: when the velocity store
// is unreachable, declining every payment turns a Redis blip into an outage, while when the
// sanctions screen is unreachable, allowing is not an option at any cost. A uniform value is
// therefore applied only to the checks whose posture is genuinely a merchant preference, and the
// mandatory checks keep their platform defaults — which is why this returns a partial map rather
// than one entry per CheckID.
func posturesFor(decision string) map[risk.CheckID]risk.FailurePosture {
	posture := risk.PostureFailOpen
	switch decision {
	case "DECLINE":
		posture = risk.PostureFailClosed
	case "REQUIRE_3DS":
		posture = risk.PostureRequire3DS
	}
	return map[risk.CheckID]risk.FailurePosture{
		risk.CheckVelocityPerMinute:   posture,
		risk.CheckVelocityPerCard:     posture,
		risk.CheckVelocityPerCustomer: posture,
		risk.CheckRiskScore:           posture,
		risk.CheckMerchantBlocklist:   posture,
	}
}
