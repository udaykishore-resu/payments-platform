// Package shared is the domain layer's shared kernel: the small set of value objects and
// primitives that more than one bounded context genuinely needs.
//
// It is deliberately tiny. A shared kernel is a coupling point — every context that imports it
// is affected by a change here — so the bar for adding something is "two or more contexts
// cannot express themselves without it", not "it seemed reusable". Anything that is merely
// useful belongs in the context that uses it, even at the cost of a little duplication
// (see docs/architecture.md on where DRY is deliberately not applied).
//
// This package imports only the standard library and pkg/*, per the dependency rule in
// docs/spec/00-design-baseline.md §4.
package shared

import (
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
)

// Typed identifiers.
//
// These are distinct defined types rather than aliases so that passing a MerchantID where a
// PaymentID is expected does not compile. The cost is a conversion at each boundary; the
// benefit is that an entire class of "looked up the wrong entity" bug becomes impossible
// rather than merely unlikely.
type (
	TenantID     string
	APIClientID  string
	MerchantID   string
	OnboardingID string
	WorkflowID   string
	StepID       string
	GatewayID    string
	ConnectionID string
	PaymentID    string
	AttemptID    string
	RefundID     string
	PlanID       string
	LedgerID     string
	WebhookID    string
	EventID      string
	AuditID      string
	ConfigVerID  string
	RequestID    string
)

func (id TenantID) String() string     { return string(id) }
func (id APIClientID) String() string  { return string(id) }
func (id MerchantID) String() string   { return string(id) }
func (id OnboardingID) String() string { return string(id) }
func (id WorkflowID) String() string   { return string(id) }
func (id StepID) String() string       { return string(id) }
func (id GatewayID) String() string    { return string(id) }
func (id ConnectionID) String() string { return string(id) }
func (id PaymentID) String() string    { return string(id) }
func (id AttemptID) String() string    { return string(id) }
func (id RefundID) String() string     { return string(id) }
func (id PlanID) String() string       { return string(id) }
func (id LedgerID) String() string     { return string(id) }
func (id WebhookID) String() string    { return string(id) }
func (id EventID) String() string      { return string(id) }
func (id AuditID) String() string      { return string(id) }
func (id ConfigVerID) String() string  { return string(id) }
func (id RequestID) String() string    { return string(id) }

func (id TenantID) IsZero() bool   { return id == "" }
func (id MerchantID) IsZero() bool { return id == "" }
func (id PaymentID) IsZero() bool  { return id == "" }
func (id GatewayID) IsZero() bool  { return id == "" }
func (id AttemptID) IsZero() bool  { return id == "" }

// Constructors. Each mints a correctly prefixed ULID.
func NewTenantID() TenantID         { return TenantID(ids.New(ids.PrefixTenant)) }
func NewAPIClientID() APIClientID   { return APIClientID(ids.New(ids.PrefixAPIClient)) }
func NewMerchantID() MerchantID     { return MerchantID(ids.New(ids.PrefixMerchant)) }
func NewOnboardingID() OnboardingID { return OnboardingID(ids.New(ids.PrefixOnboardingCase)) }
func NewWorkflowID() WorkflowID     { return WorkflowID(ids.New(ids.PrefixWorkflowInstance)) }
func NewStepID() StepID             { return StepID(ids.New(ids.PrefixWorkflowStep)) }
func NewConnectionID() ConnectionID { return ConnectionID(ids.New(ids.PrefixGatewayConnection)) }
func NewPaymentID() PaymentID       { return PaymentID(ids.New(ids.PrefixPayment)) }
func NewAttemptID() AttemptID       { return AttemptID(ids.New(ids.PrefixPaymentAttempt)) }
func NewRefundID() RefundID         { return RefundID(ids.New(ids.PrefixRefund)) }
func NewPlanID() PlanID             { return PlanID(ids.New(ids.PrefixRoutingPlan)) }
func NewLedgerID() LedgerID         { return LedgerID(ids.New(ids.PrefixLedgerEntry)) }
func NewWebhookID() WebhookID       { return WebhookID(ids.New(ids.PrefixInboundWebhook)) }
func NewEventID() EventID           { return EventID(ids.New(ids.PrefixEvent)) }
func NewAuditID() AuditID           { return AuditID(ids.New(ids.PrefixAuditRecord)) }
func NewConfigVerID() ConfigVerID   { return ConfigVerID(ids.New(ids.PrefixConfigVersion)) }
func NewRequestID() RequestID       { return RequestID(ids.New(ids.PrefixRequest)) }

// Parsers. Each validates both the encoding and the prefix, so a caller cannot pass a merchant
// ID to a payment lookup and get a 404 instead of a 400.
func ParseTenantID(s string) (TenantID, error) {
	id, err := ids.ParseAs(s, ids.PrefixTenant)
	return TenantID(id), wrapIDErr(err, "tenantId")
}

func ParseMerchantID(s string) (MerchantID, error) {
	id, err := ids.ParseAs(s, ids.PrefixMerchant)
	return MerchantID(id), wrapIDErr(err, "merchantId")
}

func ParsePaymentID(s string) (PaymentID, error) {
	id, err := ids.ParseAs(s, ids.PrefixPayment)
	return PaymentID(id), wrapIDErr(err, "paymentId")
}

func ParseAttemptID(s string) (AttemptID, error) {
	id, err := ids.ParseAs(s, ids.PrefixPaymentAttempt)
	return AttemptID(id), wrapIDErr(err, "attemptId")
}

func ParseRefundID(s string) (RefundID, error) {
	id, err := ids.ParseAs(s, ids.PrefixRefund)
	return RefundID(id), wrapIDErr(err, "refundId")
}

func ParseWorkflowID(s string) (WorkflowID, error) {
	id, err := ids.ParseAs(s, ids.PrefixWorkflowInstance)
	return WorkflowID(id), wrapIDErr(err, "workflowId")
}

func wrapIDErr(err error, field string) error {
	if err == nil {
		return nil
	}
	return apierror.New(apierror.CodeValidationFailed, "invalid identifier").
		WithDetail(apierror.Detail{
			Field:   field,
			Code:    "INVALID_IDENTIFIER",
			Message: err.Error(),
			RuleID:  "L1.IDENTIFIER_WELL_FORMED",
		})
}

// PartitionMonth derives the declarative range-partition key from a payment identifier.
// See baseline amendment A-02: making the partition a pure function of an immutable ID is what
// keeps a payment's attempts in the same partition as the payment, which is in turn what makes
// invariant I3's partial unique index actually enforce "at most one successful attempt".
func PartitionMonth(paymentID PaymentID) time.Time {
	return ids.PartitionMonth(ids.ID(paymentID))
}

// GatewayID values are human-authored slugs rather than ULIDs, because they appear in routing
// configuration, in URLs (`/v1/webhooks/{gateway}`) and in metric labels, where "stripe" is
// enormously more useful than "gw_01JB8Z...". They are validated against a strict charset.
const maxGatewaySlug = 32

// ParseGatewayID validates a gateway slug: lowercase alphanumerics and hyphens, starting with
// a letter. The charset is restricted because this value is interpolated into metric label
// values, Redis key prefixes and secret paths.
func ParseGatewayID(s string) (GatewayID, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || len(s) > maxGatewaySlug {
		return "", invalidGateway(s, "must be between 1 and 32 characters")
	}
	if s[0] < 'a' || s[0] > 'z' {
		return "", invalidGateway(s, "must start with a lowercase letter")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-'
		if !ok {
			return "", invalidGateway(s, "may contain only lowercase letters, digits and hyphens")
		}
	}
	return GatewayID(s), nil
}

func invalidGateway(s, why string) error {
	return apierror.Newf(apierror.CodeValidationFailed, "invalid gateway identifier %q", s).
		WithDetail(apierror.Detail{
			Field:   "gateway",
			Code:    "INVALID_GATEWAY_ID",
			Message: why,
			RuleID:  "L1.GATEWAY_ID_WELL_FORMED",
		})
}

// Environment separates production traffic from sandbox traffic at the type level. A merchant
// exists in exactly one environment; credentials, gateway endpoints, webhook registrations and
// routing are all environment-scoped. Mixing them is the failure mode where a certification run
// charges a real card.
type Environment string

const (
	EnvironmentSandbox    Environment = "sandbox"
	EnvironmentProduction Environment = "production"
)

// IsValid reports whether e is one of the two legal environments.
func (e Environment) IsValid() bool {
	return e == EnvironmentSandbox || e == EnvironmentProduction
}

// IsProduction reports whether real money moves in this environment.
func (e Environment) IsProduction() bool { return e == EnvironmentProduction }

// ParseEnvironment validates an environment string.
func ParseEnvironment(s string) (Environment, error) {
	e := Environment(strings.ToLower(strings.TrimSpace(s)))
	if !e.IsValid() {
		return "", apierror.Newf(apierror.CodeValidationFailed,
			"invalid environment %q: must be sandbox or production", s)
	}
	return e, nil
}

// Version is an aggregate's optimistic-concurrency version. It increments on every persisted
// state change; a write with a stale version is rejected rather than merged.
type Version int64

// Next returns the version a write should carry.
func (v Version) Next() Version { return v + 1 }

// TenantTier selects the isolation model applied to a tenant (baseline §16).
type TenantTier string

const (
	// TierPooled shares the database, cache and topics with other tenants, isolated by
	// row-level security and key prefixes. The default: it is the only tier whose unit
	// economics work at thousands of merchants.
	TierPooled TenantTier = "POOLED"
	// TierSiloed gives the tenant a dedicated schema, cache and KMS key. Chosen for
	// contractual isolation requirements, at materially higher cost per tenant.
	TierSiloed TenantTier = "SILOED"
)

// IsValid reports whether t is a known tier.
func (t TenantTier) IsValid() bool { return t == TierPooled || t == TierSiloed }
