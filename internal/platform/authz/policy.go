package authz

import (
	"context"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// The denial reasons. They are the vocabulary of the audit trail and of the "why was this
// denied" question, so they are constants and they are stable.
const (
	ReasonNoTenantContext      = "NO_TENANT_CONTEXT"
	ReasonUnauthenticated      = "UNAUTHENTICATED"
	ReasonRevoked              = "PRINCIPAL_REVOKED"
	ReasonExplicitDeny         = "EXPLICIT_DENY"
	ReasonPermissionNotGranted = "PERMISSION_NOT_GRANTED"
	ReasonConditionFailed      = "CONDITION_FAILED"
	ReasonUnknownPermission    = "UNKNOWN_PERMISSION"
)

// Decision is the result of an evaluation.
//
// It carries *why*. An authorization failure that renders as an opaque 403 costs a support
// engineer an hour and a merchant a day, every time it happens, for the life of the system; a
// decision that names the failing condition costs one line in a log. The matched rules serve the
// same purpose in the other direction: an audit record that says "allowed by
// RBAC.tenant-admin.config:write" is evidence, while one that says "allowed" is an assertion.
type Decision struct {
	// Allow is the answer. It is false in the zero value, which is the default-deny property
	// expressed in the type: a Decision that was never populated denies.
	Allow bool
	// Reason is the denial class, or the empty string on an allow.
	Reason string
	// Detail qualifies the reason — the failing condition's ID, the matched deny rule, the
	// dual-control outcome.
	Detail string
	// Permission is the permission that was evaluated, echoed so a log line is self-contained.
	Permission Permission
	// MatchedRules are the RBAC rule identifiers that granted the permission.
	MatchedRules []string
	// SatisfiedConditions are the ABAC conditions that held. Recorded on an allow because
	// "which conditions did we actually check" is a question every audit asks and no system
	// answers.
	SatisfiedConditions []string
	// RequiredDualControl records whether a second-person approval was demanded and met.
	RequiredDualControl bool
}

// Denied reports the negation of Allow, so call sites read naturally in both directions.
func (d Decision) Denied() bool { return !d.Allow }

// Error renders the decision as the platform error a transport should return.
//
// A denial is FORBIDDEN, and the *reason* is deliberately not in the caller-facing message: the
// message is the registered default, and the detail travels to the audit record and the log.
// Telling a caller which condition failed is telling them which attribute to change, and for
// conditions like tenant match and residency that is an enumeration oracle.
//
// The one exception is the dual-control family, where the remediation is legitimate and public:
// a caller who needs a second person's approval must be told so, or the API is unusable.
func (d Decision) Error() *apierror.Error {
	if d.Allow {
		return nil
	}
	if d.Reason == ReasonNoTenantContext {
		return apierror.New(apierror.CodeMissingTenantContext, "")
	}
	if strings.HasPrefix(d.Detail, "DUAL_CONTROL") {
		return apierror.New(apierror.CodeForbidden, "").
			WithDetail(apierror.Detail{
				Code:    d.Detail,
				Message: "this operation requires approval by a second person who is not the requester",
				RuleID:  "L6." + d.Detail,
			})
	}
	return apierror.New(apierror.CodeForbidden, "")
}

// String renders a decision for a log line.
func (d Decision) String() string {
	if d.Allow {
		return "ALLOW " + string(d.Permission) + " [" + strings.Join(d.MatchedRules, ",") + "]"
	}
	if d.Detail != "" {
		return "DENY " + string(d.Permission) + " " + d.Reason + ":" + d.Detail
	}
	return "DENY " + string(d.Permission) + " " + d.Reason
}

// allow builds an allow decision.
func allowDecision(perm Permission, rules, conds []string, dualControlled bool) Decision {
	return Decision{
		Allow: true, Permission: perm,
		MatchedRules: rules, SatisfiedConditions: conds,
		RequiredDualControl: dualControlled,
	}
}

// deny builds a denial. Every non-allow exit in Evaluate goes through it, which is what makes
// "there is no fallthrough" checkable by reading one function.
func deny(perm Permission, reason, detail string) Decision {
	return Decision{Permission: perm, Reason: reason, Detail: detail}
}

// DenyRule is an explicit denial, evaluated before any grant is computed.
//
// Deny rules exist for incident response: "freeze this client now", "block this merchant while
// we investigate". They are evaluated first and nothing can override them, because a freeze that
// a role binding could undo is not a freeze. The Match function is a pure predicate for the same
// reason every condition is.
type DenyRule struct {
	ID    string
	Match func(req Request) bool
}

// PolicyConfig configures the evaluator.
type PolicyConfig struct {
	// Environment is the deployment's environment, used by the environment condition.
	Environment shared.Environment
	// Region is the deployment's region, used by the residency condition.
	Region string
	// Conditions maps a permission to the conditions that must hold for it. Nil uses
	// DefaultConditions.
	Conditions map[Permission][]Condition
	// ExplicitDenies are evaluated before any grant.
	ExplicitDenies []DenyRule
	// Approvals is the dual-control store. Nil means dual control can never be satisfied, which
	// is the correct default: a deployment that has not wired an approvals store must not be
	// able to perform dual-controlled operations.
	Approvals ApprovalStore
	// Clock supplies the evaluation instant when the caller did not.
	Clock shared.Clock
	// MFAMaxAge is the freshness window for re-authentication before the highest-consequence
	// actions.
	MFAMaxAge time.Duration
}

// DefaultMFAMaxAge is the re-authentication window from security.md §4.3.
const DefaultMFAMaxAge = 5 * time.Minute

// Policy is the evaluator. It is immutable after construction and safe for concurrent use.
type Policy struct {
	cfg        PolicyConfig
	conditions map[Permission][]Condition
	denies     []DenyRule
	clock      shared.Clock
}

// NewPolicy builds an evaluator.
func NewPolicy(cfg PolicyConfig) (*Policy, error) {
	if !cfg.Environment.IsValid() {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"the policy engine requires a valid deployment environment, got %q", cfg.Environment)
	}
	if cfg.Clock == nil {
		cfg.Clock = shared.SystemClock{}
	}
	if cfg.MFAMaxAge <= 0 {
		cfg.MFAMaxAge = DefaultMFAMaxAge
	}
	conds := cfg.Conditions
	if conds == nil {
		conds = DefaultConditions(cfg.Environment, cfg.Region, cfg.MFAMaxAge)
	}
	return &Policy{
		cfg:        cfg,
		conditions: conds,
		denies:     append([]DenyRule(nil), cfg.ExplicitDenies...),
		clock:      cfg.Clock,
	}, nil
}

// Evaluate decides one authorization question.
//
// Deterministic, total, side-effect free, and default-deny at every exit. The step numbering
// mirrors security.md §4.4 so the code and the document can be read side by side; if they ever
// disagree, the document is authoritative and this is a defect.
//
// # Why the ordering is what it is
//
// Tenant isolation is checked first and is not part of policy: it has already pinned the context,
// and this function refuses to proceed without it rather than inferring one. Explicit denies come
// before grants so an incident freeze cannot be overridden. RBAC comes before ABAC because an
// ungranted permission needs no attribute evaluation and because evaluating conditions for a
// permission the principal does not hold would leak, through timing, which attributes matter.
// Dual control comes last because it is the only step with an external dependency, and a step
// with a dependency should not run for a request that was going to be denied anyway.
func (p *Policy) Evaluate(ctx context.Context, req Request) Decision {
	if req.Now.IsZero() {
		req.Now = p.clock.Now()
	}

	// 0. Tenant isolation is not part of policy evaluation. It has already run as pipeline
	//    stage 4 and pinned ctx to exactly one tenant. If it somehow did not, deny — a policy
	//    engine must never infer a tenant.
	tc, err := tenantctx.FromContext(ctx)
	if err != nil {
		return deny(req.Permission, ReasonNoTenantContext, "")
	}

	// 1. The principal must be authenticated and not revoked. Revocation is re-checked here as
	//    well as at authentication because the ≤30 s-stale cache may have been invalidated in
	//    between, and the whole point of priority invalidation is that it takes effect on the
	//    next request rather than the next token.
	if req.Principal == nil {
		return deny(req.Permission, ReasonUnauthenticated, "")
	}
	if req.Principal.Revoked {
		return deny(req.Permission, ReasonRevoked, "")
	}

	// 2. Explicit denies win over everything, and are evaluated before any grant is computed.
	for _, d := range p.denies {
		if d.Match != nil && d.Match(req) {
			return deny(req.Permission, ReasonExplicitDeny, d.ID)
		}
	}

	// A permission this binary does not know about has no grant. Reporting it distinctly from
	// "not granted" turns a typo in a route definition into a one-line diagnosis instead of a
	// puzzling 403 for a role that obviously should have access.
	if !knownPermission(req.Permission) {
		return deny(req.Permission, ReasonUnknownPermission, string(req.Permission))
	}

	// 3. RBAC: the union of grants across the principal's roles. Absent is denied.
	roles := RolesOf(req.Principal)
	grant := GrantForRoles(roles, req.Permission)
	if !grant.Allowed() {
		return deny(req.Permission, ReasonPermissionNotGranted, "")
	}
	matched := make([]string, 0, len(roles))
	for _, r := range roles {
		if GrantFor(r, req.Permission).Allowed() {
			matched = append(matched, RuleID(r, req.Permission))
		}
	}

	// 3a. A merchant-scoped grant (`S`) is only usable within the principal's own merchant
	//     scope, and additionally requires the request to name a merchant at all. A scoped role
	//     performing a tenant-wide listing is the case this catches: without it, `S` would
	//     silently widen to `✓` for any operation that happens not to carry a merchant.
	if grant.MerchantScoped() && req.Resource.MerchantID == "" {
		return deny(req.Permission, ReasonConditionFailed, CondMerchantScope)
	}

	// 4. ABAC: every condition attached to this permission must hold. Conjunctive; no escape.
	satisfied := make([]string, 0, 8)
	for _, cond := range p.conditions[req.Permission] {
		if !cond.Holds(ctx, req) {
			return deny(req.Permission, ReasonConditionFailed, cond.ID())
		}
		satisfied = append(satisfied, cond.ID())
	}

	// 5. Dual control, if the matrix demands it or the amount threshold does.
	//
	//    Note that the amount condition above *holds* when no approval is needed, so reaching
	//    here with a money-moving operation over the threshold means the condition denied and we
	//    never got here — which is why the threshold is re-derived rather than inferred from the
	//    condition's result. The two mechanisms are deliberately separate: the matrix says which
	//    operations always need a second person, the threshold says which instances of an
	//    otherwise-single-person operation do.
	needsDual := grant.RequiresDualControl() || p.overThreshold(req)
	if needsDual {
		switch res := CheckDualControl(ctx, p.cfg.Approvals, req, tc.TenantID, req.Now); res {
		case DualControlOK:
		default:
			return deny(req.Permission, ReasonConditionFailed, string(res))
		}
	}

	// 6. Allow. The decision, its inputs and the matched rules are emitted as an audit record by
	//    the caller for every mutating permission and every denial.
	return allowDecision(req.Permission, matched, satisfied, needsDual)
}

// overThreshold reports whether the amount alone demands a second person, independently of the
// matrix. It duplicates the arithmetic in AmountThreshold rather than sharing a helper because
// the two ask different questions — "may this proceed without approval" and "must we look for
// one" — and collapsing them would make the fail-closed direction of each hard to see.
func (p *Policy) overThreshold(req Request) bool {
	if !isMoneyMoving(req.Operation) || req.Resource.Amount == nil {
		return false
	}
	if req.DualControlThreshold == nil {
		// No configured threshold means we cannot say the amount is small, so we require the
		// second person. Fail closed.
		return true
	}
	over, err := req.Resource.Amount.GreaterThan(*req.DualControlThreshold)
	if err != nil {
		return true
	}
	return over
}

func knownPermission(perm Permission) bool {
	for _, p := range AllPermissions {
		if p == perm {
			return true
		}
	}
	return false
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: FR-05, NFR-34.
//
// Scope and attribute authorization: default deny, explicit deny wins, and no allow that
// crosses a tenant boundary
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
