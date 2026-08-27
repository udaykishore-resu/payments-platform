// Package tenant is the tenancy and identity bounded context's domain model (BC-1).
//
// A tenant is the platform's outermost boundary of isolation and of entitlement — the two are
// related but they are not the same thing, and this package is careful about the difference.
// Isolation is enforced by the infrastructure (row-level security, key prefixes, KMS keys,
// topic ACLs) per baseline §16; that machinery lives outside the domain and this package cannot
// weaken it. Entitlement is a business decision — which gateways, currencies and methods a tenant
// has bought, and how much of the platform it may consume — and that is what this package owns.
//
// The rule that follows from separating them: a bug in this package can cost a tenant an
// erroneous rejection, but it cannot leak one tenant's data to another. Anything that could is
// deliberately not modelled here. Tenant identity comes exclusively from the authenticated
// principal (baseline §16.2) and never from a field a caller can set, which is why nothing in
// this package accepts a tenant ID from request data.
//
// This package imports only the standard library, pkg/* and the shared kernel.
package tenant

import (
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Status is the tenant lifecycle.
//
// Three states and no onboarding pipeline, unlike the merchant. That asymmetry is deliberate: a
// tenant is created by a commercial process that has already completed outside the platform — a
// signed contract — whereas a merchant is onboarded *by* the platform through a workflow with
// KYC, bank validation and certification. Giving the tenant a pipeline it does not have would
// mean inventing states nothing ever sets.
type Status string

const (
	// StatusActive is a tenant that may operate.
	StatusActive Status = "ACTIVE"

	// StatusSuspended is a reversible stop — non-payment, a compliance hold, an investigation.
	// Every write is refused and every merchant under the tenant stops processing, but nothing is
	// deleted and the tenant can be reinstated intact.
	StatusSuspended Status = "SUSPENDED"

	// StatusTerminated is the end of the relationship. Terminal. Data retention past this point
	// is a legal-hold and regulatory-retention question answered in BC-9, not by resurrecting the
	// tenant, which is why there is no edge back.
	StatusTerminated Status = "TERMINATED"
)

// AllStatuses is the complete state universe.
var AllStatuses = []Status{StatusActive, StatusSuspended, StatusTerminated}

// machine is the tenant lifecycle table. It is a table rather than three `if` statements for the
// same reason every other machine in this platform is: the legal edges are enumerable, so they
// can be tested exhaustively, and the same table generates the database CHECK constraint.
var machine = shared.NewStateMachine("tenant", StatusActive,
	AllStatuses, []Status{StatusTerminated},
	[]shared.Transition[Status]{
		{From: StatusActive, To: StatusSuspended},
		{From: StatusSuspended, To: StatusActive},
		{From: StatusActive, To: StatusTerminated},
		// Termination directly from suspension is the normal path, not an exception: the usual
		// sequence is suspend for non-payment, wait out the cure period, terminate. Forcing a
		// reinstatement first would mean briefly re-enabling processing for a tenant that is
		// being shut down.
		{From: StatusSuspended, To: StatusTerminated},
	})

// Machine exposes the tenant state machine for the documentation generator, the SQL constraint
// generator and the exhaustive property test.
func Machine() *shared.StateMachine[Status] { return machine }

// IsKnown reports whether s is a status this binary understands.
func (s Status) IsKnown() bool { return machine.IsKnown(s) }

// IsTerminal reports whether the tenant can never change status again.
func (s Status) IsTerminal() bool { return machine.IsTerminal(s) }

// String satisfies fmt.Stringer.
func (s Status) String() string { return string(s) }

// ResidencyRegion is where a tenant's data — and the processing of it — is permitted to live.
//
// It is a first-class type rather than a string on the configuration because it is a hard
// constraint on routing, not a preference. A tenant under an EU residency commitment whose
// payment is routed to a gateway processing in us-east-1 has had its data exported, and no amount
// of after-the-fact remediation undoes that. The check therefore has to be somewhere the routing
// engine cannot forget to call, which is why PermitsGatewayRegion exists as a method on the
// tenant rather than as a rule in a configuration document somebody might not attach.
type ResidencyRegion string

const (
	// ResidencyGlobal places no restriction. The default for tenants with no regulatory or
	// contractual residency commitment.
	ResidencyGlobal ResidencyRegion = "GLOBAL"
	// ResidencyEU confines processing to the European Economic Area.
	ResidencyEU ResidencyRegion = "EU"
	// ResidencyUK confines processing to the United Kingdom. Separate from EU: post-Brexit UK
	// adequacy is a distinct legal instrument and a UK-residency tenant is not automatically
	// satisfied by EU processing.
	ResidencyUK ResidencyRegion = "UK"
	// ResidencyUS confines processing to the United States.
	ResidencyUS ResidencyRegion = "US"
	// ResidencyAPAC confines processing to the Asia-Pacific region.
	ResidencyAPAC ResidencyRegion = "APAC"
)

// residencyPermitted maps each policy to the gateway processing regions it accepts, normalised to
// lowercase. It is an allowlist, never a blocklist: a region string nobody has classified must be
// refused, because the cost of wrongly refusing is a routing rejection and the cost of wrongly
// permitting is an unlawful data export.
var residencyPermitted = map[ResidencyRegion]map[string]struct{}{
	ResidencyEU:   {"eu": {}, "eea": {}, "eu-west": {}, "eu-central": {}},
	ResidencyUK:   {"uk": {}, "gb": {}, "eu-west-2": {}},
	ResidencyUS:   {"us": {}, "us-east": {}, "us-west": {}},
	ResidencyAPAC: {"apac": {}, "ap": {}, "ap-southeast": {}, "ap-northeast": {}},
}

// IsValid reports whether r is a known residency policy.
func (r ResidencyRegion) IsValid() bool {
	if r == ResidencyGlobal {
		return true
	}
	_, ok := residencyPermitted[r]
	return ok
}

// String satisfies fmt.Stringer.
func (r ResidencyRegion) String() string { return string(r) }

// PermitsGatewayRegion reports whether a gateway processing in the given region satisfies this
// tenant's residency policy.
//
// An empty or unrecognised region answers false for every policy except GLOBAL. That is the
// fail-closed choice and it is the whole point: a gateway whose processing region nobody has
// recorded is a gateway whose processing region we do not know, and "we do not know" is not an
// answer that permits routing EU personal data through it. The operational consequence — a
// misconfigured gateway is simply never selected for residency-constrained tenants — is a
// visible, fixable outage rather than an invisible, unfixable breach.
func (r ResidencyRegion) PermitsGatewayRegion(region string) bool {
	if r == ResidencyGlobal {
		return true
	}
	allowed, ok := residencyPermitted[r]
	if !ok {
		return false
	}
	_, permitted := allowed[strings.ToLower(strings.TrimSpace(region))]
	return permitted
}

// Resource names a quota-limited platform resource.
//
// The set is closed and small because each value is also a metric label and a rate-limiter key;
// an open-ended resource name would let a caller create unbounded cardinality in the metrics
// pipeline, which baseline §22.3 forbids.
type Resource string

const (
	// ResourceMerchants caps how many merchants may exist under the tenant.
	ResourceMerchants Resource = "MERCHANTS"
	// ResourceRequestsPerSecond caps the tenant's sustained API rate.
	ResourceRequestsPerSecond Resource = "REQUESTS_PER_SECOND"
	// ResourceConcurrentPayments caps in-flight payments. This is the bulkhead, and it is a
	// separate limit from the rate limit on purpose: a tenant sending 50 requests per second to a
	// gateway that has slowed to ten-second responses is inside its rate limit and is still
	// consuming every request slot in the pod.
	ResourceConcurrentPayments Resource = "CONCURRENT_PAYMENTS"
	// ResourceCacheMemoryMB caps the tenant's slice of the shared cache. Without it, one tenant's
	// working set evicts every other tenant's configuration snapshots, and the symptom shows up
	// as latency on unrelated tenants (baseline §16.1).
	ResourceCacheMemoryMB Resource = "CACHE_MEMORY_MB"
)

// IsValid reports whether r is a known quota resource.
func (r Resource) IsValid() bool {
	switch r {
	case ResourceMerchants, ResourceRequestsPerSecond, ResourceConcurrentPayments, ResourceCacheMemoryMB:
		return true
	default:
		return false
	}
}

// String satisfies fmt.Stringer.
func (r Resource) String() string { return string(r) }

// Quotas is a tenant's share of the platform.
//
// Zero means unlimited for every counted resource, and that is a considered choice in the
// opposite direction from the rest of this codebase, which fails closed. Quotas protect the
// platform from a tenant, not a tenant from itself; a missing quota row that silently reduced a
// paying tenant's rate limit to zero would take them completely offline, which is a far worse
// outcome than the one it prevents. The control plane's L4 validation is what ensures production
// tenants actually carry limits — this type's job is to make a configuration gap survivable.
type Quotas struct {
	MaxMerchants       int
	RequestsPerSecond  int
	ConcurrentPayments int
	CacheMemoryMB      int

	// MaxPaymentAmount is a backstop, not the primary limit. The limit a merchant actually hits
	// is the per-merchant risk configuration in baseline §23; this one exists so that a
	// compromised or mistaken merchant configuration cannot authorize a payment larger than the
	// tenant's entire commercial relationship justifies. A zero or currency-less value means no
	// tenant-level ceiling.
	MaxPaymentAmount money.Money
}

// Validate rejects negative quotas. Negative is not "unlimited" and is not "zero" — it is a
// data-entry error, and accepting it would make every Check against that resource fail with a
// message about a limit of -1.
func (q Quotas) Validate() error {
	for _, c := range []struct {
		field string
		v     int
	}{
		{"quotas.maxMerchants", q.MaxMerchants},
		{"quotas.requestsPerSecond", q.RequestsPerSecond},
		{"quotas.concurrentPayments", q.ConcurrentPayments},
		{"quotas.cacheMemoryMb", q.CacheMemoryMB},
	} {
		if c.v < 0 {
			return apierror.Newf(apierror.CodeConfigurationInvalid,
				"quota %s may not be negative", c.field).
				WithDetail(apierror.Detail{
					Field: c.field, Code: "NEGATIVE_QUOTA",
					Message: "use zero for unlimited",
					RuleID:  "L4.TENANT_QUOTAS_NON_NEGATIVE",
				})
		}
	}
	if q.MaxPaymentAmount.IsNegative() {
		return apierror.New(apierror.CodeConfigurationInvalid,
			"the maximum payment amount may not be negative").
			WithDetail(apierror.Detail{
				Field: "quotas.maxPaymentAmount", Code: "NEGATIVE_QUOTA",
				Message: "use a zero-valued amount for no tenant-level ceiling",
				RuleID:  "L4.TENANT_QUOTAS_NON_NEGATIVE",
			})
	}
	return nil
}

// Limit returns the configured limit for a counted resource and whether one is set.
func (q Quotas) Limit(r Resource) (int, bool) {
	var v int
	switch r {
	case ResourceMerchants:
		v = q.MaxMerchants
	case ResourceRequestsPerSecond:
		v = q.RequestsPerSecond
	case ResourceConcurrentPayments:
		v = q.ConcurrentPayments
	case ResourceCacheMemoryMB:
		v = q.CacheMemoryMB
	default:
		return 0, false
	}
	return v, v > 0
}

// Check reports whether `requested` units of a counted resource are within the tenant's quota.
//
// The error carries RATE_LIMITED or CONCURRENCY_LIMIT_EXCEEDED where those are the honest
// classification, because those two codes are retryable and a client SDK branches on that bit: a
// tenant that has hit its requests-per-second limit should back off and retry, whereas a tenant
// that has hit its merchant count should call sales. Returning one undifferentiated
// "quota exceeded" would make the retryable and non-retryable cases indistinguishable, which is
// exactly the failure the error model in pkg/apierror exists to prevent.
//
// Note what Check does *not* handle: the maximum payment amount. An amount is not a count.
// Comparing it requires a currency, and an API that let a caller pass EUR minor units to be
// compared against a USD ceiling would be the precise bug money.Money exists to make
// unrepresentable. That check is CheckAmount.
func (q Quotas) Check(resource Resource, requested int64) error {
	if !resource.IsValid() {
		return apierror.Newf(apierror.CodeInternalError, "unknown quota resource %q", resource)
	}
	if requested < 0 {
		return apierror.Newf(apierror.CodeValidationFailed,
			"cannot check a negative quantity against the %s quota", resource)
	}
	limit, set := q.Limit(resource)
	if !set {
		return nil
	}
	if requested <= int64(limit) {
		return nil
	}
	code := apierror.CodeAmountExceedsLimit
	switch resource {
	case ResourceRequestsPerSecond:
		code = apierror.CodeRateLimited
	case ResourceConcurrentPayments:
		code = apierror.CodeConcurrencyLimitExceeded
	default:
		// Every other resource is a plain volume ceiling, and CodeAmountExceedsLimit set above is the
		// right answer for it. Only the two throughput resources map to a different client code
	}
	return apierror.Newf(code, "tenant quota for %s exceeded: requested %d, limit %d",
		resource, requested, limit).
		WithDetail(apierror.Detail{
			Field:   "quota." + string(resource),
			Code:    "QUOTA_EXCEEDED",
			Message: "the tenant's provisioned limit for this resource has been reached",
			RuleID:  "L4.TENANT_QUOTA_" + string(resource),
		})
}

// CheckAmount reports whether an amount is within the tenant's payment ceiling.
//
// A currency mismatch is *not* an error and not a rejection: the ceiling is expressed in one
// currency and payments arrive in many, and this type has no exchange rate — deliberately, for
// the reason money.Add refuses to convert. Rather than guess a conversion, the ceiling simply
// does not apply to other currencies, and the per-merchant risk limits in baseline §23, which are
// configured per currency, do the real work. Silently converting at some hardcoded rate would be
// the worse failure: a limit that is wrong in a way nobody can see.
func (q Quotas) CheckAmount(amount money.Money) error {
	if !q.MaxPaymentAmount.IsValid() || q.MaxPaymentAmount.IsZero() {
		return nil
	}
	if amount.Currency() != q.MaxPaymentAmount.Currency() {
		return nil
	}
	over, err := amount.GreaterThan(q.MaxPaymentAmount)
	if err != nil || !over {
		return nil //nolint:nilerr // GreaterThan can only fail on a currency mismatch, which the two lines above already refused; the quota simply does not apply
	}
	return apierror.Newf(apierror.CodeAmountExceedsLimit,
		"amount %s exceeds the tenant maximum of %s", amount, q.MaxPaymentAmount).
		WithDetail(apierror.Detail{
			Field:   "amount",
			Code:    "ABOVE_TENANT_MAXIMUM",
			Message: "the tenant-level payment ceiling applies in addition to the merchant's own limits",
			RuleID:  "L4.TENANT_MAX_PAYMENT_AMOUNT",
		})
}

// Tenant is the aggregate root for one customer of the platform.
//
// What it owns: identity, lifecycle, isolation tier, residency policy, quotas and the
// entitlement sets — which gateways, currencies and payment methods the tenant has bought.
//
// What it deliberately does not own: its merchants and its API clients. Both are separate
// aggregates. Merchants because a tenant with a thousand merchants would otherwise be a
// thousand-entity aggregate loaded on every request, and API clients because their write rate —
// credential rotation, scope edits — has nothing to do with the tenant's own lifecycle and would
// contend with it on the version.
type Tenant struct {
	id   shared.TenantID
	name string
	tier shared.TenantTier

	status    Status
	residency ResidencyRegion

	// environments is the set the tenant may operate in. Almost every tenant has both, but a
	// tenant in evaluation has sandbox only, and that has to be expressible as data rather than
	// as a status: an evaluating tenant is ACTIVE, it simply cannot touch production.
	environments []shared.Environment

	quotas Quotas

	enabledGateways   []shared.GatewayID
	enabledCurrencies []money.Currency
	enabledMethods    []shared.PaymentMethod

	featureFlags map[string]bool

	// kmsKeyRef is the tenant's dedicated customer master key, required for the siloed tier
	// (baseline §16.1) and empty for pooled tenants, which share the platform key. It is a
	// reference to a key, never key material.
	kmsKeyRef string

	createdAt    time.Time
	updatedAt    time.Time
	suspendedAt  *time.Time
	terminatedAt *time.Time
	statusReason string

	version shared.Version

	events []Event
}

// NewParams are the inputs to creating a tenant.
type NewParams struct {
	Name              string
	Tier              shared.TenantTier
	Residency         ResidencyRegion
	Environments      []shared.Environment
	Quotas            Quotas
	EnabledGateways   []shared.GatewayID
	EnabledCurrencies []money.Currency
	EnabledMethods    []shared.PaymentMethod
	FeatureFlags      map[string]bool
	KMSKeyRef         string
}

// New creates an ACTIVE tenant.
//
// It enforces the one invariant that cannot be deferred: a siloed tenant must have a dedicated
// KMS key. The siloed tier is bought specifically for cryptographic isolation, and a siloed
// tenant silently encrypting with the shared platform key would satisfy nothing while appearing
// on the invoice — a contractual and audit failure that is invisible until somebody asks for
// evidence. Catching it in the constructor means an entity that exists is an entity whose
// isolation claim is true.
func New(p NewParams, clock shared.Clock) (*Tenant, error) {
	if strings.TrimSpace(p.Name) == "" {
		return nil, apierror.New(apierror.CodeValidationFailed, "a tenant requires a name").
			WithDetail(apierror.Detail{
				Field: "name", Code: "MISSING", Message: "a tenant name is required",
				RuleID: "L4.TENANT_NAME_REQUIRED",
			})
	}
	tier := p.Tier
	if tier == "" {
		tier = shared.TierPooled
	}
	if !tier.IsValid() {
		return nil, apierror.Newf(apierror.CodeValidationFailed, "unknown tenant tier %q", p.Tier)
	}
	residency := p.Residency
	if residency == "" {
		residency = ResidencyGlobal
	}
	if !residency.IsValid() {
		return nil, apierror.Newf(apierror.CodeValidationFailed,
			"unknown data residency region %q", p.Residency).
			WithDetail(apierror.Detail{
				Field: "dataResidencyRegion", Code: "UNKNOWN_REGION",
				Message: "must be one of GLOBAL, EU, UK, US, APAC",
				RuleID:  "L4.TENANT_RESIDENCY_KNOWN",
			})
	}
	if tier == shared.TierSiloed && p.KMSKeyRef == "" {
		return nil, apierror.New(apierror.CodeConfigurationInvalid,
			"a siloed tenant requires a dedicated KMS key reference").
			WithDetail(apierror.Detail{
				Field: "kmsKeyRef", Code: "MISSING_FOR_SILOED_TIER",
				Message: "the siloed tier is defined by a dedicated customer master key; without one the isolation claim is false",
				RuleID:  "L4.SILOED_TENANT_HAS_DEDICATED_KEY",
			})
	}
	envs := p.Environments
	if len(envs) == 0 {
		// Defaulting to sandbox only, never production. A tenant created with no explicit
		// environment set is one whose contract has not been read by the code creating it, and
		// the safe reading of an unstated contract is "not live yet".
		envs = []shared.Environment{shared.EnvironmentSandbox}
	}
	for _, e := range envs {
		if !e.IsValid() {
			return nil, apierror.Newf(apierror.CodeValidationFailed, "unknown environment %q", e)
		}
	}
	if err := p.Quotas.Validate(); err != nil {
		return nil, err
	}
	for _, c := range p.EnabledCurrencies {
		if !c.IsSupported() {
			return nil, apierror.Newf(apierror.CodeCurrencyNotSupported,
				"currency %q is not supported by the platform", c)
		}
	}
	for _, m := range p.EnabledMethods {
		if !m.IsValid() {
			return nil, apierror.Newf(apierror.CodePaymentMethodNotSupported,
				"payment method %q is not supported by the platform", m)
		}
	}

	now := clock.Now()
	t := &Tenant{
		id:                shared.NewTenantID(),
		name:              strings.TrimSpace(p.Name),
		tier:              tier,
		status:            StatusActive,
		residency:         residency,
		environments:      append([]shared.Environment(nil), envs...),
		quotas:            p.Quotas,
		enabledGateways:   append([]shared.GatewayID(nil), p.EnabledGateways...),
		enabledCurrencies: append([]money.Currency(nil), p.EnabledCurrencies...),
		enabledMethods:    append([]shared.PaymentMethod(nil), p.EnabledMethods...),
		featureFlags:      copyFlags(p.FeatureFlags),
		kmsKeyRef:         p.KMSKeyRef,
		createdAt:         now,
		updatedAt:         now,
		version:           1,
	}
	t.raise(EventTenantCreated, now, map[string]any{
		"name":      t.name,
		"tier":      string(t.tier),
		"residency": string(t.residency),
	})
	return t, nil
}

// Accessors. Fields are unexported so that nothing outside this package can put a tenant into a
// shape the constructor would have refused — a siloed tier with no key, an unknown residency.

func (t *Tenant) ID() shared.TenantID        { return t.id }
func (t *Tenant) Name() string               { return t.name }
func (t *Tenant) Tier() shared.TenantTier    { return t.tier }
func (t *Tenant) Status() Status             { return t.status }
func (t *Tenant) Residency() ResidencyRegion { return t.residency }
func (t *Tenant) Quotas() Quotas             { return t.quotas }
func (t *Tenant) KMSKeyRef() string          { return t.kmsKeyRef }
func (t *Tenant) CreatedAt() time.Time       { return t.createdAt }
func (t *Tenant) UpdatedAt() time.Time       { return t.updatedAt }
func (t *Tenant) SuspendedAt() *time.Time    { return t.suspendedAt }
func (t *Tenant) TerminatedAt() *time.Time   { return t.terminatedAt }
func (t *Tenant) StatusReason() string       { return t.statusReason }
func (t *Tenant) Version() shared.Version    { return t.version }

// Environments returns a copy of the permitted environment set.
func (t *Tenant) Environments() []shared.Environment {
	return append([]shared.Environment(nil), t.environments...)
}

// EnabledGateways returns a copy of the entitled gateway set.
func (t *Tenant) EnabledGateways() []shared.GatewayID {
	return append([]shared.GatewayID(nil), t.enabledGateways...)
}

// EnabledCurrencies returns a copy of the entitled currency set.
func (t *Tenant) EnabledCurrencies() []money.Currency {
	return append([]money.Currency(nil), t.enabledCurrencies...)
}

// EnabledMethods returns a copy of the entitled payment method set.
func (t *Tenant) EnabledMethods() []shared.PaymentMethod {
	return append([]shared.PaymentMethod(nil), t.enabledMethods...)
}

// FeatureFlags returns a copy of the flag map. A copy rather than the live map, because returning
// the backing map would let a caller flip a feature on for a tenant without going through a
// method, an audit record or a version bump.
func (t *Tenant) FeatureFlags() map[string]bool { return copyFlags(t.featureFlags) }

// FeatureEnabled reports whether a named flag is on. An unknown flag is off: a feature nobody has
// explicitly enabled for this tenant is not enabled for this tenant.
func (t *Tenant) FeatureEnabled(name string) bool { return t.featureFlags[name] }

// IsOperational reports whether the tenant may transact at all. Suspended and terminated tenants
// answer false, and the data plane refuses their traffic at the edge before any merchant lookup —
// a suspended tenant's payment must not consume a database round trip to be rejected.
func (t *Tenant) IsOperational() bool { return t.status == StatusActive }

// PermitsEnvironment reports whether the tenant may operate in the given environment.
func (t *Tenant) PermitsEnvironment(env shared.Environment) bool {
	for _, e := range t.environments {
		if e == env {
			return true
		}
	}
	return false
}

// Permits is the L4 configuration check: may a merchant under this tenant be configured to use
// this gateway, currency and method?
//
// The tenant is the ceiling and a merchant configuration can only narrow it. That direction is
// not a stylistic preference — it is what makes the entitlement model enforceable at all. The
// tenant's set is what the platform contracted, provisioned and priced: gateway credentials
// exist per (tenant, merchant, gateway) and are only issued for gateways the tenant has a
// relationship with, and currencies are enabled per tenant with the acquirer. If a merchant
// configuration could *widen* the tenant's set, then the authoritative statement of what a tenant
// may do would be the union of all its merchants' configurations — a value nobody computes,
// nobody reviews and nobody can revoke in one place. Narrowing, by contrast, is safe by
// construction: the intersection of a permitted set with anything is still permitted.
//
// The practical consequence is that revoking a tenant's entitlement takes effect everywhere at
// once, without touching a single merchant configuration.
//
// An empty entitlement set means "nothing entitled", not "everything entitled". A tenant whose
// currency list nobody has populated is a tenant whose contract has not been transcribed, and
// permitting all currencies for it would be a billing and licensing exposure that surfaces months
// later on an acquirer's report.
func (t *Tenant) Permits(gateway shared.GatewayID, currency money.Currency, method shared.PaymentMethod) error {
	if t.status != StatusActive {
		return apierror.Newf(apierror.CodeForbidden,
			"tenant %s is %s and its configuration may not be changed", t.id, t.status).
			WithDetail(apierror.Detail{
				Field: "tenant", Code: "TENANT_NOT_ACTIVE",
				Message: "the tenant is not active",
				RuleID:  "L4.TENANT_IS_ACTIVE",
			})
	}
	if !containsGateway(t.enabledGateways, gateway) {
		return apierror.Newf(apierror.CodeGatewayNotConfigured,
			"gateway %s is not enabled for this tenant", gateway).
			WithDetail(apierror.Detail{
				Field: "routing.gateway", Code: "GATEWAY_NOT_ENTITLED",
				Message: "a merchant may only be configured for gateways the tenant is entitled to",
				RuleID:  "L4.MERCHANT_WITHIN_TENANT_GATEWAYS",
			})
	}
	if !containsCurrency(t.enabledCurrencies, currency) {
		return apierror.Newf(apierror.CodeCurrencyNotSupported,
			"currency %s is not enabled for this tenant", currency).
			WithDetail(apierror.Detail{
				Field: "supportedCurrencies", Code: "CURRENCY_NOT_ENTITLED",
				Message: "a merchant may only be configured for currencies the tenant is entitled to",
				RuleID:  "L4.MERCHANT_WITHIN_TENANT_CURRENCIES",
			})
	}
	if !containsMethod(t.enabledMethods, method) {
		return apierror.Newf(apierror.CodePaymentMethodNotSupported,
			"payment method %s is not enabled for this tenant", method).
			WithDetail(apierror.Detail{
				Field: "paymentMethods", Code: "METHOD_NOT_ENTITLED",
				Message: "a merchant may only be configured for payment methods the tenant is entitled to",
				RuleID:  "L4.MERCHANT_WITHIN_TENANT_METHODS",
			})
	}
	return nil
}

// Suspend stops the tenant, reversibly. The reason is mandatory: a suspension takes every
// merchant under the tenant offline, it is the first thing an on-call engineer sees when a
// tenant reports a total outage, and "suspended" with no recorded cause turns a two-minute answer
// into an investigation.
func (t *Tenant) Suspend(reason string, clock shared.Clock) error {
	if reason == "" {
		return apierror.New(apierror.CodeValidationFailed, "suspending a tenant requires a reason").
			WithDetail(apierror.Detail{
				Field: "reason", Code: "MISSING",
				Message: "a suspension takes every merchant under the tenant offline and must record why",
				RuleID:  "L4.SUSPENSION_REQUIRES_REASON",
			})
	}
	now := clock.Now()
	if err := t.transition(StatusSuspended, reason, clock, EventTenantSuspended,
		map[string]any{"reason": reason}); err != nil {
		return err
	}
	t.suspendedAt = &now
	return nil
}

// Reinstate lifts a suspension.
func (t *Tenant) Reinstate(clock shared.Clock) error {
	if err := t.transition(StatusActive, "", clock, EventTenantReinstated,
		map[string]any{"previousReason": t.statusReason}); err != nil {
		return err
	}
	t.statusReason = ""
	t.suspendedAt = nil
	return nil
}

// Terminate permanently ends the relationship.
//
// The caller supplies activeMerchants because this aggregate cannot see BC-2, and the check lives
// here rather than only in the use case so that the rule is stated in the domain: terminating a
// tenant with live merchants orphans their payments — in-flight authorizations with no owner to
// capture or void them, and refunds nobody is entitled to issue.
func (t *Tenant) Terminate(reason string, activeMerchants int, clock shared.Clock) error {
	if activeMerchants > 0 {
		return apierror.Newf(apierror.CodeInvalidStateTransition,
			"cannot terminate: %d merchants under this tenant are still active", activeMerchants).
			WithDetail(apierror.Detail{
				Field: "status", Code: "ACTIVE_MERCHANTS",
				Message: "terminate or migrate every merchant under the tenant first",
				RuleID:  "L4.TERMINATE_REQUIRES_NO_ACTIVE_MERCHANTS",
			})
	}
	now := clock.Now()
	if err := t.transition(StatusTerminated, reason, clock, EventTenantTerminated,
		map[string]any{"reason": reason}); err != nil {
		return err
	}
	t.terminatedAt = &now
	return nil
}

// UpdateQuotas replaces the tenant's resource limits.
//
// It is refused on a non-active tenant. Raising a suspended tenant's quota is meaningless — no
// traffic flows — and permitting it means the audit trail shows a quota grant during a compliance
// hold, which is exactly the kind of thing a reviewer stops on.
func (t *Tenant) UpdateQuotas(q Quotas, clock shared.Clock) error {
	if t.status != StatusActive {
		return apierror.Newf(apierror.CodeInvalidStateTransition,
			"cannot update quotas for a %s tenant", t.status)
	}
	if err := q.Validate(); err != nil {
		return err
	}
	previous := t.quotas
	now := clock.Now()
	t.quotas = q
	t.touch(now)
	t.raise(EventTenantQuotasUpdated, now, map[string]any{
		"previousMaxMerchants":       previous.MaxMerchants,
		"maxMerchants":               q.MaxMerchants,
		"previousRequestsPerSecond":  previous.RequestsPerSecond,
		"requestsPerSecond":          q.RequestsPerSecond,
		"previousConcurrentPayments": previous.ConcurrentPayments,
		"concurrentPayments":         q.ConcurrentPayments,
		"previousCacheMemoryMb":      previous.CacheMemoryMB,
		"cacheMemoryMb":              q.CacheMemoryMB,
	})
	return nil
}

// EnableGateway adds a gateway to the tenant's entitlement set. Idempotent: enabling an
// already-enabled gateway is a no-op rather than an error, because the caller is usually a
// declarative provisioning job reconciling a contract, and making it error would force every such
// job to read-then-write with a race in the middle.
func (t *Tenant) EnableGateway(g shared.GatewayID, clock shared.Clock) error {
	if t.status != StatusActive {
		return apierror.Newf(apierror.CodeInvalidStateTransition,
			"cannot change gateway entitlements for a %s tenant", t.status)
	}
	if g.IsZero() {
		return apierror.New(apierror.CodeValidationFailed, "a gateway identifier is required")
	}
	if _, err := shared.ParseGatewayID(g.String()); err != nil {
		return err
	}
	if containsGateway(t.enabledGateways, g) {
		return nil
	}
	now := clock.Now()
	t.enabledGateways = append(t.enabledGateways, g)
	t.touch(now)
	t.raise(EventTenantGatewayEnabled, now, map[string]any{"gatewayId": g.String()})
	return nil
}

// DisableGateway removes a gateway from the entitlement set.
//
// It does not touch the merchants configured to use it, and it does not have to: because the
// tenant is the ceiling, an L4 revalidation of any merchant configuration naming this gateway now
// fails, and the routing engine's own tenant check refuses to select it. Cascading the change
// into every merchant configuration would rewrite documents an operator did not edit, which
// destroys the audit property that a configuration version records a deliberate change.
func (t *Tenant) DisableGateway(g shared.GatewayID, clock shared.Clock) error {
	if t.status != StatusActive {
		return apierror.Newf(apierror.CodeInvalidStateTransition,
			"cannot change gateway entitlements for a %s tenant", t.status)
	}
	idx := -1
	for i, x := range t.enabledGateways {
		if x == g {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	now := clock.Now()
	t.enabledGateways = append(t.enabledGateways[:idx], t.enabledGateways[idx+1:]...)
	t.touch(now)
	t.raise(EventTenantGatewayDisabled, now, map[string]any{"gatewayId": g.String()})
	return nil
}

func (t *Tenant) transition(to Status, reason string, clock shared.Clock, evt EventType, payload map[string]any) error {
	if err := machine.Transition(t.status, to); err != nil {
		return err
	}
	now := clock.Now()
	t.status = to
	if reason != "" {
		t.statusReason = reason
	}
	t.touch(now)
	if evt != "" {
		t.raise(evt, now, payload)
	}
	return nil
}

func (t *Tenant) touch(now time.Time) {
	t.updatedAt = now
	t.version = t.version.Next()
}

func (t *Tenant) raise(e EventType, at time.Time, payload map[string]any) {
	t.events = append(t.events, Event{
		Type:       e,
		TenantID:   t.id,
		Status:     t.status,
		OccurredAt: at,
		Version:    t.version,
		Payload:    payload,
	})
}

// PendingEvents returns the domain events raised in this unit of work.
func (t *Tenant) PendingEvents() []Event { return append([]Event(nil), t.events...) }

// DrainEvents returns and clears the pending events, called once per unit of work by the
// repository inside the state-change transaction.
func (t *Tenant) DrainEvents() []Event {
	out := t.events
	t.events = nil
	return out
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

func containsGateway(set []shared.GatewayID, v shared.GatewayID) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

func containsCurrency(set []money.Currency, v money.Currency) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

func containsMethod(set []shared.PaymentMethod, v shared.PaymentMethod) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

// RehydrateParams carries the persisted state of a Tenant back into the aggregate.
type RehydrateParams struct {
	ID                shared.TenantID
	Name              string
	Tier              shared.TenantTier
	Status            Status
	Residency         ResidencyRegion
	Environments      []shared.Environment
	Quotas            Quotas
	EnabledGateways   []shared.GatewayID
	EnabledCurrencies []money.Currency
	EnabledMethods    []shared.PaymentMethod
	FeatureFlags      map[string]bool
	KMSKeyRef         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	SuspendedAt       *time.Time
	TerminatedAt      *time.Time
	StatusReason      string
	Version           shared.Version
}

// Rehydrate reconstructs a Tenant from persisted state.
//
// It validates the status and the tier rather than trusting the row. An unknown status means a
// rollback landed on data written by a newer binary, and the specific danger here is that
// coercing it to a plausible value would most likely coerce it to ACTIVE — re-enabling a tenant
// somebody deliberately stopped.
func Rehydrate(p RehydrateParams) (*Tenant, error) {
	if !p.Status.IsKnown() {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"tenant %s has unknown status %q; this row may have been written by a newer version of the service",
			p.ID, p.Status)
	}
	if !p.Tier.IsValid() {
		return nil, apierror.Newf(apierror.CodeInternalError, "tenant %s has unknown tier %q", p.ID, p.Tier)
	}
	return &Tenant{
		id: p.ID, name: p.Name, tier: p.Tier, status: p.Status, residency: p.Residency,
		environments:      append([]shared.Environment(nil), p.Environments...),
		quotas:            p.Quotas,
		enabledGateways:   append([]shared.GatewayID(nil), p.EnabledGateways...),
		enabledCurrencies: append([]money.Currency(nil), p.EnabledCurrencies...),
		enabledMethods:    append([]shared.PaymentMethod(nil), p.EnabledMethods...),
		featureFlags:      copyFlags(p.FeatureFlags),
		kmsKeyRef:         p.KMSKeyRef,
		createdAt:         p.CreatedAt, updatedAt: p.UpdatedAt,
		suspendedAt: p.SuspendedAt, terminatedAt: p.TerminatedAt,
		statusReason: p.StatusReason, version: p.Version,
	}, nil
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-01, BR-36, FR-01, FR-02, FR-07, NFR-37.
//
// The tenant aggregate: tier, entitlement ceilings, quotas and the data-residency policy that
// routing treats as a hard exclusion
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
