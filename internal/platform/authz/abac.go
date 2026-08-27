package authz

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/netip"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authn"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Resource is what an operation acts on.
//
// Every field is optional at the type level and mandatory in practice for the conditions that
// read it. That looks lax and is not: a condition that finds the attribute it needs missing
// *denies*, so an under-populated Resource fails closed. The alternative — a constructor that
// demands every field for every operation — would force a caller listing gateways to invent a
// merchant state, and inventing attributes to satisfy a policy engine is how attributes stop
// meaning anything.
type Resource struct {
	// TenantID is the resource's owning tenant. It must match the request's tenant context.
	TenantID shared.TenantID
	// MerchantID is the merchant the resource belongs to, where one applies.
	MerchantID shared.MerchantID
	// Environment is the resource's environment.
	Environment shared.Environment
	// ResidencyRegion is where this resource's data is permitted to live, e.g. "eu-west-1".
	ResidencyRegion string
	// MerchantState is the merchant's lifecycle state, which gates payment operations
	// differently depending on the direction money moves.
	MerchantState string
	// Amount is the money at stake, for the dual-control threshold. Nil for operations that
	// move no money.
	Amount *money.Money
}

// Request is one authorization question. It is a value, not an interface, and it is complete:
// evaluation performs no I/O, so everything a condition needs must already be here.
type Request struct {
	// Principal is who is asking.
	Principal *authn.Principal
	// Permission is what they want to do.
	Permission Permission
	// Resource is what they want to do it to.
	Resource Resource
	// Operation names the specific action, e.g. "refund". Some conditions apply only to
	// particular operations.
	Operation string

	// Now is the evaluation instant, supplied by the caller from the injected clock. Conditions
	// must never read a clock themselves: a policy that consults `time.Now` is a policy that
	// cannot be replayed during an incident reconstruction.
	Now time.Time
	// SourceIP is where the request came from.
	SourceIP netip.Addr
	// PeerThumbprint is the SHA-256 thumbprint of the TLS client certificate on this
	// connection, for sender-constrained tokens.
	PeerThumbprint string

	// DualControlThreshold is the merchant's configured amount above which a refund needs a
	// second person. Zero means every amount requires one, which is the fail-closed reading of
	// a missing configuration.
	DualControlThreshold *money.Money
	// ApprovalRef identifies the second-person approval accompanying this request.
	ApprovalRef string
	// Fingerprint is a hash of the request the approval was granted for. An approval for a
	// £10 refund must not authorize a £10 000 one.
	Fingerprint string

	// PermittedRegions are the regions this subject may act on data in.
	PermittedRegions []string
	// AllowedHours optionally restricts a human principal to a window, expressed in whole
	// hours, UTC. Nil means no restriction.
	AllowedHours *HourWindow
}

// HourWindow is an inclusive-start, exclusive-end window of UTC hours. It wraps around midnight
// when Start > End, because "22:00 to 06:00" is the shape most out-of-hours restrictions
// actually take and forcing a caller to express it as two windows is how one of them gets
// forgotten.
type HourWindow struct {
	StartHour int
	EndHour   int
}

// Contains reports whether t falls inside the window.
func (w HourWindow) Contains(t time.Time) bool {
	h := t.UTC().Hour()
	if w.StartHour <= w.EndHour {
		return h >= w.StartHour && h < w.EndHour
	}
	return h >= w.StartHour || h < w.EndHour
}

// Condition is one attribute predicate.
//
// Conditions are conjunctive — every one attached to a (role, permission) pair must hold — and
// there is no disjunctive escape hatch. That is a deliberate restriction on expressiveness: an
// "any-of" combinator is exactly the construct that lets a broad condition quietly satisfy a
// rule that a narrow one was supposed to gate, and every policy language that has one
// eventually has a review incident about it.
type Condition interface {
	// ID is a stable identifier, emitted on a denial so that "why was this denied" has an
	// answer with a documentation anchor.
	ID() string
	// Holds is a pure predicate: no I/O, no clock, no randomness. Totality is what makes the
	// policy engine replayable.
	Holds(ctx context.Context, req Request) bool
}

// ConditionFunc adapts a function to Condition, so a deployment-specific condition is a closure
// rather than a new type.
type ConditionFunc struct {
	Name string
	Fn   func(ctx context.Context, req Request) bool
}

// ID returns the condition's identifier.
func (c ConditionFunc) ID() string { return c.Name }

// Holds evaluates the predicate.
func (c ConditionFunc) Holds(ctx context.Context, req Request) bool { return c.Fn(ctx, req) }

// The condition identifiers. They appear in denial reasons and in audit records, so they are
// constants rather than inline strings: an operator grepping for one must find exactly one
// place it is produced.
const (
	CondTenantMatch      = "TENANT_MATCH"
	CondMerchantScope    = "MERCHANT_SCOPE"
	CondEnvironment      = "ENVIRONMENT"
	CondAmountThreshold  = "AMOUNT_THRESHOLD"
	CondResidency        = "RESIDENCY"
	CondMerchantState    = "MERCHANT_STATE"
	CondTimeWindow       = "TIME_WINDOW"
	CondDevicePosture    = "DEVICE_POSTURE"
	CondSourceConstraint = "SOURCE_CONSTRAINT"
	CondMFAFreshness     = "MFA_FRESHNESS"
)

// TenantMatch requires the resource's tenant to equal the pinned tenant context.
//
// It reads the tenant from `ctx`, not from the request body and not from the resource: the
// tenant has already been pinned by pipeline stage 4, and this condition's job is to check the
// resource against it, never to derive it. A policy engine that could infer a tenant would be a
// policy engine whose bugs are cross-tenant.
//
// It additionally requires a tenant-scoped principal's own tenant to agree with the pinned
// context. A workload principal (SPIFFE, no tenant of its own) is exempt from that second check
// by construction — it has no tenant to compare — which is exactly why it can only ever act
// under an explicitly propagated context.
//
// This is baseline §16.2 expressed as a policy condition, and it is defence in depth rather than
// the primary control: the tenant guard has already pinned the context, and row-level security
// will filter the query regardless. It is here anyway because the three controls fail
// differently — a guard bug, a policy bug and a missing RLS policy are independent events — and
// because a denial here produces an explainable audit record rather than a silent empty result.
func TenantMatch() Condition {
	return ConditionFunc{Name: CondTenantMatch, Fn: func(ctx context.Context, req Request) bool {
		tc, err := tenantctx.FromContext(ctx)
		if err != nil || req.Resource.TenantID.IsZero() {
			// A missing tenant context, or an unstamped resource, denies. An unstamped resource
			// is not "probably ours" — that reading is how a migration that forgot to backfill a
			// column becomes a cross-tenant read.
			return false
		}
		if req.Principal != nil && !req.Principal.TenantID.IsZero() && req.Principal.TenantID != tc.TenantID {
			return false
		}
		return req.Resource.TenantID == tc.TenantID
	}}
}

// MerchantScope requires the resource's merchant to be inside the principal's merchant scope.
//
// An empty scope means the whole tenant, which is safe only because the tenant boundary has
// already been enforced by TenantMatch: "every merchant" means every merchant of this tenant.
func MerchantScope() Condition {
	return ConditionFunc{Name: CondMerchantScope, Fn: func(_ context.Context, req Request) bool {
		if req.Principal == nil {
			return false
		}
		if len(req.Principal.MerchantScope) == 0 {
			return true
		}
		if req.Resource.MerchantID == "" {
			// A scoped credential acting on a resource with no merchant cannot be checked, and
			// an uncheckable constraint is a failed one.
			return false
		}
		for _, m := range req.Principal.MerchantScope {
			if m == req.Resource.MerchantID {
				return true
			}
		}
		return false
	}}
}

// EnvironmentMatch requires principal, resource and deployment to agree.
//
// All three, not two. Checking only principal against resource would let a sandbox credential
// act on a sandbox resource that had somehow been replicated into the production database;
// checking only principal against deployment would let a production credential act on a sandbox
// record. The three-way equality is the only version with no gap.
func EnvironmentMatch(deployment shared.Environment) Condition {
	return ConditionFunc{Name: CondEnvironment, Fn: func(_ context.Context, req Request) bool {
		if req.Principal == nil {
			return false
		}
		if !req.Principal.Environment.IsValid() || !req.Resource.Environment.IsValid() {
			return false
		}
		return req.Principal.Environment == req.Resource.Environment &&
			req.Resource.Environment == deployment
	}}
}

// AmountThreshold requires dual control above the merchant's configured threshold.
//
// It is expressed as a condition that *holds* when no extra approval is needed, and combined
// with RequiresDualControl below. The threshold makes the common case fast and the dangerous
// case reviewed: the blast radius of a refund is otherwise unbounded, and requiring two people
// for every £3 refund would mean the control is disabled within a month.
//
// A missing threshold denies rather than permits. An unconfigured merchant is one whose limits
// nobody has decided, and "no limit configured" must not read as "no limit".
func AmountThreshold() Condition {
	return ConditionFunc{Name: CondAmountThreshold, Fn: func(_ context.Context, req Request) bool {
		if !isMoneyMoving(req.Operation) || req.Resource.Amount == nil {
			return true
		}
		if req.DualControlThreshold == nil {
			return false
		}
		over, err := req.Resource.Amount.GreaterThan(*req.DualControlThreshold)
		if err != nil {
			// A currency mismatch between the amount and the threshold means the comparison has
			// no meaning. Treating that as "under the threshold" would be a way to bypass dual
			// control by choosing a currency.
			return false
		}
		return !over
	}}
}

// isMoneyMoving reports whether an operation hands money back to a payer, which is the class the
// amount threshold governs (security.md §4.3: "refunds, and any manual money-out").
func isMoneyMoving(operation string) bool {
	switch operation {
	case "refund", "payout", "manual_credit":
		return true
	default:
		return false
	}
}

// Residency requires the resource's region to be one the subject may act on, and to be the
// region this deployment runs in.
//
// The second half is what makes it more than a policy statement: a read of EU-resident data from
// a US region is denied here *and* the data is not present there. The condition exists so that
// the denial is explainable and audited rather than surfacing as a mysterious not-found.
func Residency(deploymentRegion string) Condition {
	return ConditionFunc{Name: CondResidency, Fn: func(_ context.Context, req Request) bool {
		if req.Resource.ResidencyRegion == "" {
			// No residency constraint recorded on the resource. Permitted: most resources carry
			// no personal data and the constraint does not apply to them. The ones that do carry
			// it are stamped at creation, and a missing stamp on those is a data-integrity bug
			// that the merchant-PII code path catches, not a silent widening here.
			return true
		}
		if deploymentRegion != "" && req.Resource.ResidencyRegion != deploymentRegion {
			return false
		}
		if len(req.PermittedRegions) == 0 {
			return false
		}
		for _, r := range req.PermittedRegions {
			if r == req.Resource.ResidencyRegion {
				return true
			}
		}
		return false
	}}
}

// MerchantState gates payment operations on the merchant's lifecycle.
//
// The asymmetry is the point (baseline §8): a suspended merchant may not take new payments, but
// must always be able to give money back. A rule that blocked refunds for a suspended merchant
// would strand that merchant's customers' money for the duration of an investigation, which is
// both a consumer-protection failure and, in most jurisdictions, a regulatory one.
//
// # Why an unknown state defers rather than denies
//
// This condition can only decide when the caller has told it which merchant the request is
// about. On the REST edge that is often not knowable at authorization time: authorization runs
// before the body is parsed — deliberately, so that an unauthorized caller cannot make the
// server decode attacker-controlled input — and `POST /v1/payments` carries its merchant in the
// body, not the path. The same is true of the capture, refund and void routes, which are
// addressed by payment id.
//
// Denying on an empty state would therefore make every payment operation unreachable through the
// public API, which is not a stricter security posture — it is a broken one, and the pressure to
// "fix" it would land on removing the condition entirely.
//
// Deferring is safe because the check is not lost, only moved to the one place that has the
// answer: the L5 validation stage evaluates the merchant's lifecycle against the *same* merchant
// snapshot the router and the orchestrator use (L5.MERCHANT_ACTIVE), and it fails closed on a
// merchant it cannot resolve. A caller that reaches the handler with a suspended merchant is
// refused there, with a message that names the merchant's state rather than a bare 403.
//
// Where the state *is* supplied — the gRPC surface, the workflow engine, any caller that has
// already resolved the merchant — the condition decides exactly as before.
func MerchantState() Condition {
	return ConditionFunc{Name: CondMerchantState, Fn: func(_ context.Context, req Request) bool {
		if req.Resource.MerchantState == "" {
			return true
		}
		switch req.Permission {
		case PermPaymentsWrite, PermPaymentsCapture:
			return req.Resource.MerchantState == "ACTIVE"
		case PermPaymentsRefund, PermPaymentsVoid:
			return req.Resource.MerchantState == "ACTIVE" || req.Resource.MerchantState == "SUSPENDED"
		default:
			return true
		}
	}}
}

// TimeWindow restricts a human principal to their permitted hours.
//
// It applies to humans only. A machine integration runs at 03:00 because that is when the
// merchant's batch runs, and imposing office hours on it would break a legitimate integration to
// no security benefit. For a human, the window bounds how long a stolen session is useful.
func TimeWindow() Condition {
	return ConditionFunc{Name: CondTimeWindow, Fn: func(_ context.Context, req Request) bool {
		if req.Principal == nil {
			return false
		}
		if req.AllowedHours == nil || req.Principal.Type != tenantctx.PrincipalHuman {
			return true
		}
		return req.AllowedHours.Contains(req.Now)
	}}
}

// DevicePosture requires a managed, compliant device.
//
// Absent posture information denies. That is the fail-closed direction and it matters: an
// identity provider that stopped asserting posture — because of a misconfiguration, or because
// an attacker is using a token path that does not carry it — must not thereby grant admin
// actions from unmanaged machines.
func DevicePosture() Condition {
	return ConditionFunc{Name: CondDevicePosture, Fn: func(_ context.Context, req Request) bool {
		if req.Principal == nil {
			return false
		}
		return req.Principal.Device.Managed && req.Principal.Device.Compliant
	}}
}

// SourceConstraint requires a sender-constrained token bound to this connection.
//
// The token's `cnf.x5t#S256` must equal the thumbprint of the TLS client certificate that
// completed this handshake (RFC 8705). A bearer token stolen from a log, a heap dump or a proxy
// is then useless without the corresponding private key — which is the difference between a
// credential leak and an incident.
func SourceConstraint() Condition {
	return ConditionFunc{Name: CondSourceConstraint, Fn: func(_ context.Context, req Request) bool {
		if req.Principal == nil {
			return false
		}
		if req.Principal.ConfirmationThumbprint == "" || req.PeerThumbprint == "" {
			return false
		}
		return constantTimeEqual(req.Principal.ConfirmationThumbprint, req.PeerThumbprint)
	}}
}

// MFAFreshness requires a recent authentication with a second factor.
//
// It reads AuthTime, not the token's issue time. A refreshed token has a recent `iat` and the
// original `auth_time`; if this condition read `iat`, an attacker holding a refresh token could
// satisfy a freshness requirement by refreshing, which would make the control decorative for
// exactly the case it exists to stop.
func MFAFreshness(maxAge time.Duration) Condition {
	return ConditionFunc{Name: CondMFAFreshness, Fn: func(_ context.Context, req Request) bool {
		if req.Principal == nil || !req.Principal.MFA || req.Principal.AuthTime.IsZero() {
			return false
		}
		age := req.Now.Sub(req.Principal.AuthTime)
		return age >= 0 && age <= maxAge
	}}
}

// DefaultConditions returns the condition set attached to each (role, permission) pair, given
// the deployment's environment and region.
//
// The universal three — tenant, environment, merchant scope — apply to everything, because there
// is no operation for which acting across a tenant, across an environment, or outside a
// credential's merchant scope is acceptable. The rest attach to the specific permissions whose
// consequences justify them, which is what keeps the model comprehensible: an engineer reading
// this can say why each extra condition is on each row.
func DefaultConditions(deployment shared.Environment, region string, mfaMaxAge time.Duration) map[Permission][]Condition {
	universal := []Condition{TenantMatch(), EnvironmentMatch(deployment), MerchantScope(), Residency(region), TimeWindow()}

	out := map[Permission][]Condition{}
	for _, perm := range AllPermissions {
		conds := append([]Condition(nil), universal...)
		switch perm {
		case PermPaymentsWrite, PermPaymentsCapture, PermPaymentsVoid:
			conds = append(conds, MerchantState())
		case PermPaymentsRefund:
			conds = append(conds, MerchantState(), AmountThreshold(), SourceConstraint())
		case PermCredentialsRotate:
			conds = append(conds, DevicePosture(), SourceConstraint(), MFAFreshness(mfaMaxAge))
		case PermMerchantsTerminate, PermTenantsWrite, PermConfigRollback, PermOnboardingApprove:
			conds = append(conds, MFAFreshness(mfaMaxAge))
		default:
			// Every other permission carries the universal conditions and nothing more. Adding a row here
			// is how a permission acquires an extra control, so the absence of a row is itself the
			// statement that tenant, environment, scope, residency and time window are sufficient
		}
		out[perm] = conds
	}
	return out
}

// constantTimeEqual compares two strings without leaking their contents through timing.
//
// It is used for the certificate thumbprint comparison. A thumbprint is a public value, so the
// timing channel here is worth very little — but the comparison runs on the authorization path
// against caller-supplied input, and the cost of removing the question entirely is one hash of a
// value already in memory. The digests are fixed width, so unequal lengths cannot be
// distinguished by the comparison either.
func constantTimeEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}
