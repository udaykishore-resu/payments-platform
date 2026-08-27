// Package config is the configuration and policy bounded context's domain model (BC-5).
//
// A merchant's configuration is *desired state*: the tenant declares what the merchant should
// be able to do, the validation plane checks it, and the data plane enforces it. Three
// properties make this more than a settings table:
//
//   - It is versioned and append-only. A rollback publishes the previous document as a new
//     version; nothing is ever destroyed, so "what was the routing on 3 March" always has an
//     answer.
//   - It is validated as a whole (level L4) before publication, not field by field on write.
//     Configuration defects are usually *combinations* — a currency enabled with no gateway
//     that supports it, a 3DS threshold above the transaction ceiling — and per-field
//     validation cannot see them.
//   - The data plane consumes a snapshot of it, not the database. See ports.ConfigProvider.
package config

import (
	"errors"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/routing"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Status is the publication state of a configuration version.
type Status string

const (
	// StatusDraft is authored but not in force. Drafts exist so a large change can be
	// assembled and validated before it takes effect.
	StatusDraft Status = "DRAFT"
	// StatusActive is the version currently enforced. Exactly one per merchant.
	StatusActive Status = "ACTIVE"
	// StatusSuperseded is a previously active version, retained forever.
	StatusSuperseded Status = "SUPERSEDED"
)

// MerchantConfig is the versioned configuration document from baseline §23.
//
// Unlike the aggregates in other contexts, this type's fields are exported. The reasoning: a
// configuration document is a *value*, not an entity with behaviour — it is constructed
// wholesale, validated wholesale, and replaced wholesale. There is no partial mutation to
// protect, and forcing thirty accessors onto a value object buys nothing but keystrokes.
// Immutability is enforced by the repository, which never updates a row in place.
type MerchantConfig struct {
	MerchantID  shared.MerchantID
	TenantID    shared.TenantID
	Version     int
	Status      Status
	Environment shared.Environment

	SupportedCurrencies []money.Currency
	PaymentMethods      []shared.PaymentMethod
	Countries           []shared.Country

	Routing routing.Policy
	Risk    risk.Policy
	Limits  Limits
	Webhook WebhookConfig
	Settle  SettlementConfig

	// FeatureFlags gate behaviour that is not yet universal. The hard rule, enforced by
	// review and by the flag registry: a flag may enable a *capability*, never change the
	// *semantics* of money already in flight. A payment resolves its flags once, at creation,
	// and carries them for its lifetime — otherwise flipping a flag mid-capture changes the
	// rules a payment is being judged by halfway through.
	FeatureFlags map[string]bool

	CreatedAt   time.Time
	CreatedBy   string
	PublishedAt *time.Time
	// Comment is the author's reason for the change. Required on publish: a configuration
	// history with no reasons is a list of diffs nobody can interpret six months later.
	Comment string
	// PreviousVersion links the chain, so a rollback can find what it is rolling back to.
	PreviousVersion int
}

// Limits are the non-risk operational bounds.
type Limits struct {
	MaxRefundWindowDays  int
	MaxPartialCaptures   int
	MaxRefundsPerPayment int
	// AuthorizationValidityHours bounds how long an authorization may sit uncaptured before
	// the sweeper expires it. Set below the shortest validity of any enabled gateway, so the
	// platform expires an authorization before the gateway silently does.
	AuthorizationValidityHours int
}

// WebhookConfig describes outbound merchant notifications.
type WebhookConfig struct {
	Endpoints   []WebhookEndpoint
	MaxAttempts int
	Backoff     string
}

// WebhookEndpoint is one merchant-supplied destination.
type WebhookEndpoint struct {
	URL string
	// Events are glob patterns over event types, e.g. "payment.*".
	Events []string
	// SecretRef points at the HMAC signing secret. The merchant never sees ours and we never
	// store theirs in the clear.
	SecretRef string
	Active    bool
}

// SettlementConfig is the merchant's payout preference. The platform records it and passes it
// to the gateway during provisioning; it does not perform settlement (baseline §1.3 A1).
type SettlementConfig struct {
	Schedule    string
	Currency    money.Currency
	HoldDays    int
	BankAccount string
}

// Validate is the level L4 entry point: it checks the document as a whole.
//
// The checks that matter are the cross-field ones, because those are the defects a
// field-by-field validator cannot see and an operator will not notice until a payment fails at
// three in the morning.
func (c *MerchantConfig) Validate(supportedByGateways CapabilityLookup) error {
	var details []apierror.Detail

	if c.MerchantID.IsZero() {
		details = append(details, detail("merchantId", "REQUIRED", "a configuration must belong to a merchant", "L4.CONFIG_HAS_MERCHANT"))
	}
	if !c.Environment.IsValid() {
		details = append(details, detail("environment", "INVALID", "must be sandbox or production", "L4.CONFIG_ENVIRONMENT_VALID"))
	}
	if len(c.SupportedCurrencies) == 0 {
		details = append(details, detail("supportedCurrencies", "EMPTY", "at least one currency must be enabled", "L4.CURRENCIES_NON_EMPTY"))
	}
	if len(c.PaymentMethods) == 0 {
		details = append(details, detail("paymentMethods", "EMPTY", "at least one payment method must be enabled", "L4.METHODS_NON_EMPTY"))
	}

	seenCur := map[money.Currency]bool{}
	for _, cur := range c.SupportedCurrencies {
		if !cur.IsSupported() {
			details = append(details, detail("supportedCurrencies", "UNKNOWN_CURRENCY",
				string(cur)+" is not a supported ISO 4217 code", "L4.CURRENCY_SUPPORTED"))
		}
		if seenCur[cur] {
			details = append(details, detail("supportedCurrencies", "DUPLICATE",
				string(cur)+" appears more than once", "L4.CURRENCIES_UNIQUE"))
		}
		seenCur[cur] = true
	}

	seenMethod := map[shared.PaymentMethod]bool{}
	for _, m := range c.PaymentMethods {
		if !m.IsValid() {
			details = append(details, detail("paymentMethods", "UNKNOWN_METHOD",
				string(m)+" is not a supported payment method", "L4.METHOD_SUPPORTED"))
		}
		if seenMethod[m] {
			details = append(details, detail("paymentMethods", "DUPLICATE",
				string(m)+" appears more than once", "L4.METHODS_UNIQUE"))
		}
		seenMethod[m] = true
	}

	for _, co := range c.Countries {
		if !co.IsValid() {
			details = append(details, detail("countries", "UNKNOWN_COUNTRY",
				string(co)+" is not a valid ISO 3166-1 alpha-2 code", "L4.COUNTRY_VALID"))
		}
	}

	if err := c.Routing.Validate(); err != nil {
		details = append(details, extract(err)...)
	}
	if err := c.Risk.Validate(); err != nil {
		details = append(details, extract(err)...)
	}

	// Cross-field check 1: the risk ceiling must not sit below the 3DS threshold. A
	// configuration where every payment large enough to need 3DS is also large enough to be
	// rejected outright is not a policy, it is a typo — and it will present as "3DS never
	// triggers", which is a compliance finding rather than an obvious bug.
	if c.Risk.MaxTransactionAmount.IsPositive() && c.Risk.Require3DSAbove.IsPositive() {
		if c.Risk.MaxTransactionAmount.Currency() == c.Risk.Require3DSAbove.Currency() {
			if lower, _ := c.Risk.MaxTransactionAmount.LessThan(c.Risk.Require3DSAbove); lower {
				details = append(details, detail("risk.require3DSAbove", "UNREACHABLE_THRESHOLD",
					"the 3DS threshold is above the maximum transaction amount, so it can never trigger",
					"L4.THREE_DS_THRESHOLD_REACHABLE"))
			}
		}
	}

	// Cross-field check 2: the refund window must not exceed what every routed gateway can
	// honour. Promising a 365-day refund window on a gateway that allows 180 days produces a
	// failure at the worst possible moment — when a customer is owed money.
	if supportedByGateways != nil {
		window := time.Duration(c.Limits.MaxRefundWindowDays) * 24 * time.Hour
		for _, gw := range c.Routing.ReferencedGateways() {
			if !supportedByGateways.CanRefundAfter(gw, window) {
				details = append(details, detail("limits.maxRefundWindowDays", "EXCEEDS_GATEWAY_CAPABILITY",
					"gateway "+string(gw)+" cannot process refunds this long after capture",
					"L4.REFUND_WINDOW_WITHIN_GATEWAY_MAX"))
			}
		}

		// Cross-field check 3: every enabled (currency, method) pair must be servable by at
		// least one gateway in the routing policy. This is the check that catches "we enabled
		// SEPA but only route to a US-only gateway" at publish time instead of at payment time.
		for _, cur := range c.SupportedCurrencies {
			for _, m := range c.PaymentMethods {
				if !supportedByGateways.AnySupports(c.Routing.ReferencedGateways(), cur, m) {
					details = append(details, detail("routing", "UNSERVABLE_COMBINATION",
						"no gateway in the routing policy supports "+string(cur)+" with "+string(m),
						"L4.EVERY_COMBINATION_ROUTABLE"))
				}
			}
		}
	}

	if c.Limits.MaxRefundWindowDays < 0 || c.Limits.MaxRefundWindowDays > 3650 {
		details = append(details, detail("limits.maxRefundWindowDays", "OUT_OF_RANGE",
			"must be between 0 and 3650 days", "L4.REFUND_WINDOW_RANGE"))
	}
	if c.Limits.MaxPartialCaptures < 0 {
		details = append(details, detail("limits.maxPartialCaptures", "OUT_OF_RANGE",
			"must not be negative", "L4.PARTIAL_CAPTURE_RANGE"))
	}

	for i, ep := range c.Webhook.Endpoints {
		if err := validateWebhookURL(ep.URL); err != nil {
			details = append(details, detail("webhooks.endpoints["+itoa(i)+"].url", "INVALID_URL",
				err.Error(), "L4.WEBHOOK_URL_SAFE"))
		}
		if len(ep.Events) == 0 {
			details = append(details, detail("webhooks.endpoints["+itoa(i)+"].events", "EMPTY",
				"an endpoint with no event patterns will never receive anything", "L4.WEBHOOK_HAS_EVENTS"))
		}
	}

	if len(details) > 0 {
		return apierror.New(apierror.CodeConfigurationInvalid,
			"the configuration is not valid").WithDetails(details...)
	}
	return nil
}

// CapabilityLookup is the narrow view of the gateway registry that L4 validation needs.
// Declared here, with the consumer, so this package does not depend on internal/domain/gateway.
type CapabilityLookup interface {
	CanRefundAfter(g shared.GatewayID, d time.Duration) bool
	AnySupports(gs []shared.GatewayID, c money.Currency, m shared.PaymentMethod) bool
}

// Publish returns a new version of the document, marked ACTIVE and linked to its predecessor.
// The receiver is not mutated: a published configuration is immutable, and returning a new
// value rather than mutating in place makes that structural.
func (c *MerchantConfig) Publish(by, comment string, now time.Time) (*MerchantConfig, error) {
	if comment == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"a change comment is required when publishing configuration").
			WithDetail(detail("comment", "REQUIRED",
				"describe why this change is being made; the history is unreadable without it",
				"L4.PUBLISH_HAS_COMMENT"))
	}
	next := *c
	next.Version = c.Version + 1
	next.PreviousVersion = c.Version
	next.Status = StatusActive
	next.CreatedAt = now
	next.CreatedBy = by
	next.PublishedAt = &now
	next.Comment = comment
	next.FeatureFlags = copyFlags(c.FeatureFlags)
	return &next, nil
}

// RollbackTo produces a new version whose content is that of an earlier version.
//
// It is a forward operation, never a deletion. Two reasons: the audit trail must show that a
// rollback happened and who did it, and a "delete the bad version" implementation leaves the
// data plane's cached snapshots pointing at a version number that no longer exists.
func (c *MerchantConfig) RollbackTo(target *MerchantConfig, by string, now time.Time) (*MerchantConfig, error) {
	if target == nil {
		return nil, apierror.New(apierror.CodeValidationFailed, "no target version supplied")
	}
	if target.MerchantID != c.MerchantID {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"the target version belongs to a different merchant")
	}
	if target.Version >= c.Version {
		return nil, apierror.Newf(apierror.CodeValidationFailed,
			"cannot roll back to version %d from version %d", target.Version, c.Version)
	}
	next := *target
	next.Version = c.Version + 1
	next.PreviousVersion = c.Version
	next.Status = StatusActive
	next.CreatedAt = now
	next.CreatedBy = by
	next.PublishedAt = &now
	next.Comment = "rollback to version " + itoa(target.Version)
	next.FeatureFlags = copyFlags(target.FeatureFlags)
	return &next, nil
}

// SupportsCurrency reports whether the merchant may transact in c.
func (c *MerchantConfig) SupportsCurrency(cur money.Currency) bool {
	for _, x := range c.SupportedCurrencies {
		if x == cur {
			return true
		}
	}
	return false
}

// SupportsMethod reports whether the merchant may transact with m.
func (c *MerchantConfig) SupportsMethod(m shared.PaymentMethod) bool {
	for _, x := range c.PaymentMethods {
		if x == m {
			return true
		}
	}
	return false
}

// Flag resolves a feature flag, defaulting to false. A missing flag is off: a flag that
// defaults to on is a flag that changes behaviour when someone forgets to configure it.
func (c *MerchantConfig) Flag(name string) bool { return c.FeatureFlags[name] }

// ETag returns the optimistic-concurrency token for this document, used for If-Match.
func (c *MerchantConfig) ETag() string {
	return `"` + string(c.MerchantID) + "-" + itoa(c.Version) + `"`
}

func detail(field, code, msg, rule string) apierror.Detail {
	return apierror.Detail{Field: field, Code: code, Message: msg, RuleID: rule}
}

// extract pulls the field-level details out of a nested validation error so that one publish
// attempt reports every problem, rather than making the operator fix them one round-trip at a
// time.
func extract(err error) []apierror.Detail {
	var e *apierror.Error
	if errors.As(err, &e) && len(e.Details) > 0 {
		return e.Details
	}
	return []apierror.Detail{{Code: "INVALID", Message: err.Error()}}
}

func copyFlags(in map[string]bool) map[string]bool {
	if in == nil {
		return nil
	}
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-07, BR-08, BR-09, BR-14, FR-49, FR-51, FR-52.
//
// The merchant configuration document and its cross-field validation: enabled payment methods,
// currencies and countries, the transaction limits and SCA thresholds, the webhook endpoints,
// and the proof that every enabled combination is routable through some gateway
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
