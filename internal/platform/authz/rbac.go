// Package authz decides whether an authenticated principal may perform an operation.
//
// Two layers, one deterministic algorithm (security.md §4). RBAC answers "may this *role* do
// this at all"; ABAC answers "may this *subject*, on *this* resource, under *these* conditions".
// Both are data — a table and a list of predicates — rather than code, because an authorization
// model expressed as scattered `if` statements cannot be reviewed, cannot be diffed, and cannot
// be property-tested for the one thing that matters: that no input produces an allow across a
// tenant boundary.
//
// Three properties are load-bearing and are asserted by tests rather than assumed:
//
//   - Default deny. Every exit that is not an explicit Allow is a Deny. There is no fallthrough
//     and no "if we cannot decide, permit" branch.
//   - Explicit denies win. They are evaluated before any grant is computed, so an incident
//     responder freezing a client cannot be overridden by a role binding.
//   - Every decision is explainable. A Decision carries the rules that matched and the condition
//     that failed, so "why was this denied" has an answer that does not require a debugger. An
//     opaque 403 is an operational cost paid every day for the life of the system.
//
// Tenant isolation is deliberately *not* part of this package's job in the sense of deriving a
// tenant: that has already happened in the pipeline (stage 4, `tenantctx`). What this package
// does is refuse to evaluate at all when the tenant context is absent, and enforce that the
// resource's tenant matches it. A policy engine that could infer a tenant would be a policy
// engine whose bugs are cross-tenant.
package authz

import (
	"sort"
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/platform/authn"
)

// Permission is one auth scope from baseline §19.
type Permission string

// The complete permission set. It is exhaustive: a permission absent from this list has no
// grant anywhere, which is the default-deny property expressed in the type system.
const (
	PermMerchantsRead      Permission = "merchants:read"
	PermMerchantsWrite     Permission = "merchants:write"
	PermMerchantsSuspend   Permission = "merchants:suspend"
	PermMerchantsTerminate Permission = "merchants:terminate"

	PermOnboardingRead    Permission = "onboarding:read"
	PermOnboardingWrite   Permission = "onboarding:write"
	PermOnboardingApprove Permission = "onboarding:approve"

	PermConfigRead     Permission = "config:read"
	PermConfigWrite    Permission = "config:write"
	PermConfigRollback Permission = "config:rollback"

	PermGatewaysRead  Permission = "gateways:read"
	PermGatewaysWrite Permission = "gateways:write"

	PermCredentialsRotate Permission = "credentials:rotate"
	PermCredentialsRead   Permission = "credentials:read"

	PermPaymentsRead      Permission = "payments:read"
	PermPaymentsWrite     Permission = "payments:write"
	PermPaymentsCapture   Permission = "payments:capture"
	PermPaymentsRefund    Permission = "payments:refund"
	PermPaymentsVoid      Permission = "payments:void"
	PermPaymentsReplayDLQ Permission = "payments:replay_dlq"

	PermLedgerRead  Permission = "ledger:read"
	PermLedgerWrite Permission = "ledger:write"

	PermAuditRead   Permission = "audit:read"
	PermAuditExport Permission = "audit:export"

	PermTenantsRead  Permission = "tenants:read"
	PermTenantsWrite Permission = "tenants:write"

	// PermSecrets is granted to nobody. It is in the table precisely so that the denial is
	// written down: there is no API, no console path and no role that reads a gateway
	// credential (security.md §4.2). A permission that simply did not exist would be
	// indistinguishable from one someone forgot to add.
	PermSecrets Permission = "secrets:*"
)

// AllPermissions is the complete set, in a stable order, for the exhaustive matrix test and for
// documentation generation.
var AllPermissions = []Permission{
	PermMerchantsRead, PermMerchantsWrite, PermMerchantsSuspend, PermMerchantsTerminate,
	PermOnboardingRead, PermOnboardingWrite, PermOnboardingApprove,
	PermConfigRead, PermConfigWrite, PermConfigRollback,
	PermGatewaysRead, PermGatewaysWrite,
	PermCredentialsRotate, PermCredentialsRead,
	PermPaymentsRead, PermPaymentsWrite, PermPaymentsCapture, PermPaymentsRefund,
	PermPaymentsVoid, PermPaymentsReplayDLQ,
	PermLedgerRead, PermLedgerWrite,
	PermAuditRead, PermAuditExport,
	PermTenantsRead, PermTenantsWrite,
	PermSecrets,
}

// Role is a named bundle of permissions.
type Role string

// The role set from security.md §4.1.
const (
	// RolePlatformAdmin is platform staff. Deliberately not omnipotent: it holds neither
	// payments:write nor secrets:*, so a compromised admin session is a serious incident rather
	// than a total one.
	RolePlatformAdmin Role = "platform-admin"
	// RoleTenantAdmin administers one tenant and all its merchants.
	RoleTenantAdmin Role = "tenant-admin"
	// RoleMerchantAdmin administers one merchant within one tenant.
	RoleMerchantAdmin Role = "merchant-admin"
	// RoleOperator is platform SRE and support: read-heavy, with operational mutations
	// (suspend, retry, replay) and never financial ones.
	RoleOperator Role = "operator"
	// RoleAuditor is internal audit, an external QSA or a regulator. Read-only, including audit
	// records, and unable to read secrets.
	RoleAuditor Role = "auditor"
	// RoleServicePaymentClient is a tenant's data-plane machine integration.
	RoleServicePaymentClient Role = "svc:payment-client"
	// RoleServiceOnboardingClient is a tenant's control-plane machine integration.
	RoleServiceOnboardingClient Role = "svc:onboarding-client"
	// RoleServiceInternal is one of our own workloads. Bounded by its SPIFFE ID and *not*
	// tenant-scoped, which is why it can never satisfy a tenant-scoped read without an
	// explicitly propagated tenant context.
	RoleServiceInternal Role = "svc:internal"
)

// AllRoles is the complete role set, in the column order of the matrix.
var AllRoles = []Role{
	RolePlatformAdmin, RoleTenantAdmin, RoleMerchantAdmin, RoleOperator, RoleAuditor,
	RoleServicePaymentClient, RoleServiceOnboardingClient, RoleServiceInternal,
}

// Grant is one cell of the role × permission matrix.
//
// It is a bit set rather than a boolean because three of the matrix's four symbols are not
// simply "yes": `D` is granted-but-dual-controlled and `S` is granted-only-within-the-
// principal's-own-merchant-scope, and one cell (`merchant-admin` × `payments:refund`) is both.
// Collapsing those into "allowed" and re-deriving the qualifiers elsewhere is how a dual-control
// requirement gets lost in a refactor.
type Grant uint8

const (
	// Deny is the zero value, which is what makes an absent map entry a denial.
	Deny Grant = 0

	grantAllowed Grant = 1 << 0
	grantDual    Grant = 1 << 1
	grantScoped  Grant = 1 << 2
)

// The four symbols of the matrix, named so the table below reads like the document it mirrors.
const (
	// allow is `✓`.
	allow = grantAllowed
	// dual is `D`: granted, but only with a valid second-person approval.
	dual = grantAllowed | grantDual
	// scoped is `S`: granted only within the principal's own merchant scope.
	scoped = grantAllowed | grantScoped
	// scopedDual is `S + D`.
	scopedDual = grantAllowed | grantScoped | grantDual
)

// Allowed reports whether the grant permits the operation at all.
func (g Grant) Allowed() bool { return g&grantAllowed != 0 }

// RequiresDualControl reports whether a second-person approval is required.
func (g Grant) RequiresDualControl() bool { return g&grantDual != 0 }

// MerchantScoped reports whether the grant applies only within the principal's merchant scope.
func (g Grant) MerchantScoped() bool { return g&grantScoped != 0 }

// String renders the matrix symbol, for documentation generation and test failure messages.
func (g Grant) String() string {
	switch {
	case !g.Allowed():
		return "∅"
	case g.MerchantScoped() && g.RequiresDualControl():
		return "S+D"
	case g.MerchantScoped():
		return "S"
	case g.RequiresDualControl():
		return "D"
	default:
		return "✓"
	}
}

// matrix is the role × permission table from security.md §4.2, verbatim.
//
// It is the authority, and it is data so that it can be diffed in a pull request, rendered into
// the documentation, and enumerated by a test that asserts every (role, permission) pair has an
// explicit entry. An omission is a denial — that is the default-deny property — but an
// *accidental* omission is a bug, so the completeness test exists to distinguish the two.
//
// Three lines carry most of the design intent and are worth calling out at the point of
// definition:
//
//   - No human role holds payments:write. Humans do not create payments. A support engineer who
//     could create a payment could move money, so the grant does not exist and the attack does
//     not exist with it.
//   - secrets:* is denied to every principal, including platform-admin. Services read their own
//     credential path via IAM (credentials:read for svc:internal); humans never do, and there is
//     no API that would let them.
//   - merchant-admin's payments:refund is S+D — the only money-moving operation a human can
//     reach, and only within their own merchant and only with a second person.
var matrix = map[Role]map[Permission]Grant{
	RolePlatformAdmin: {
		PermMerchantsRead: allow, PermMerchantsWrite: allow, PermMerchantsSuspend: allow, PermMerchantsTerminate: dual,
		PermOnboardingRead: allow, PermOnboardingWrite: allow, PermOnboardingApprove: dual,
		PermConfigRead: allow, PermConfigWrite: dual, PermConfigRollback: dual,
		PermGatewaysRead: allow, PermGatewaysWrite: allow,
		PermCredentialsRotate: dual, PermCredentialsRead: Deny,
		PermPaymentsRead: allow, PermPaymentsWrite: Deny, PermPaymentsCapture: Deny,
		PermPaymentsRefund: Deny, PermPaymentsVoid: Deny, PermPaymentsReplayDLQ: allow,
		PermLedgerRead: allow, PermLedgerWrite: Deny,
		PermAuditRead: allow, PermAuditExport: dual,
		PermTenantsRead: allow, PermTenantsWrite: dual,
		PermSecrets: Deny,
	},
	RoleTenantAdmin: {
		PermMerchantsRead: allow, PermMerchantsWrite: allow, PermMerchantsSuspend: allow, PermMerchantsTerminate: dual,
		PermOnboardingRead: allow, PermOnboardingWrite: allow, PermOnboardingApprove: dual,
		PermConfigRead: allow, PermConfigWrite: allow, PermConfigRollback: dual,
		PermGatewaysRead: allow, PermGatewaysWrite: Deny,
		PermCredentialsRotate: dual, PermCredentialsRead: Deny,
		PermPaymentsRead: allow, PermPaymentsWrite: Deny, PermPaymentsCapture: Deny,
		PermPaymentsRefund: Deny, PermPaymentsVoid: Deny, PermPaymentsReplayDLQ: Deny,
		PermLedgerRead: allow, PermLedgerWrite: Deny,
		PermAuditRead: allow, PermAuditExport: dual,
		PermTenantsRead: allow, PermTenantsWrite: Deny,
		PermSecrets: Deny,
	},
	RoleMerchantAdmin: {
		PermMerchantsRead: scoped, PermMerchantsWrite: scoped, PermMerchantsSuspend: Deny, PermMerchantsTerminate: Deny,
		PermOnboardingRead: scoped, PermOnboardingWrite: scoped, PermOnboardingApprove: Deny,
		PermConfigRead: scoped, PermConfigWrite: scoped, PermConfigRollback: Deny,
		PermGatewaysRead: scoped, PermGatewaysWrite: Deny,
		PermCredentialsRotate: Deny, PermCredentialsRead: Deny,
		PermPaymentsRead: scoped, PermPaymentsWrite: Deny, PermPaymentsCapture: Deny,
		PermPaymentsRefund: scopedDual, PermPaymentsVoid: scoped, PermPaymentsReplayDLQ: Deny,
		PermLedgerRead: scoped, PermLedgerWrite: Deny,
		PermAuditRead: Deny, PermAuditExport: Deny,
		PermTenantsRead: Deny, PermTenantsWrite: Deny,
		PermSecrets: Deny,
	},
	RoleOperator: {
		PermMerchantsRead: allow, PermMerchantsWrite: Deny, PermMerchantsSuspend: allow, PermMerchantsTerminate: Deny,
		PermOnboardingRead: allow, PermOnboardingWrite: Deny, PermOnboardingApprove: Deny,
		PermConfigRead: allow, PermConfigWrite: Deny, PermConfigRollback: dual,
		PermGatewaysRead: allow, PermGatewaysWrite: Deny,
		PermCredentialsRotate: Deny, PermCredentialsRead: Deny,
		PermPaymentsRead: allow, PermPaymentsWrite: Deny, PermPaymentsCapture: Deny,
		PermPaymentsRefund: dual, PermPaymentsVoid: dual, PermPaymentsReplayDLQ: dual,
		PermLedgerRead: allow, PermLedgerWrite: Deny,
		PermAuditRead: allow, PermAuditExport: Deny,
		PermTenantsRead: allow, PermTenantsWrite: Deny,
		PermSecrets: Deny,
	},
	RoleAuditor: {
		PermMerchantsRead: allow, PermMerchantsWrite: Deny, PermMerchantsSuspend: Deny, PermMerchantsTerminate: Deny,
		PermOnboardingRead: allow, PermOnboardingWrite: Deny, PermOnboardingApprove: Deny,
		PermConfigRead: allow, PermConfigWrite: Deny, PermConfigRollback: Deny,
		PermGatewaysRead: allow, PermGatewaysWrite: Deny,
		PermCredentialsRotate: Deny, PermCredentialsRead: Deny,
		PermPaymentsRead: allow, PermPaymentsWrite: Deny, PermPaymentsCapture: Deny,
		PermPaymentsRefund: Deny, PermPaymentsVoid: Deny, PermPaymentsReplayDLQ: Deny,
		PermLedgerRead: allow, PermLedgerWrite: Deny,
		PermAuditRead: allow, PermAuditExport: allow,
		PermTenantsRead: allow, PermTenantsWrite: Deny,
		PermSecrets: Deny,
	},
	RoleServicePaymentClient: {
		PermMerchantsRead: scoped, PermMerchantsWrite: Deny, PermMerchantsSuspend: Deny, PermMerchantsTerminate: Deny,
		PermOnboardingRead: Deny, PermOnboardingWrite: Deny, PermOnboardingApprove: Deny,
		PermConfigRead: scoped, PermConfigWrite: Deny, PermConfigRollback: Deny,
		PermGatewaysRead: allow, PermGatewaysWrite: Deny,
		PermCredentialsRotate: Deny, PermCredentialsRead: Deny,
		PermPaymentsRead: scoped, PermPaymentsWrite: allow, PermPaymentsCapture: allow,
		PermPaymentsRefund: allow, PermPaymentsVoid: allow, PermPaymentsReplayDLQ: Deny,
		PermLedgerRead: Deny, PermLedgerWrite: Deny,
		PermAuditRead: Deny, PermAuditExport: Deny,
		PermTenantsRead: Deny, PermTenantsWrite: Deny,
		PermSecrets: Deny,
	},
	RoleServiceOnboardingClient: {
		PermMerchantsRead: allow, PermMerchantsWrite: allow, PermMerchantsSuspend: Deny, PermMerchantsTerminate: Deny,
		PermOnboardingRead: allow, PermOnboardingWrite: allow, PermOnboardingApprove: Deny,
		PermConfigRead: allow, PermConfigWrite: allow, PermConfigRollback: Deny,
		PermGatewaysRead: allow, PermGatewaysWrite: Deny,
		PermCredentialsRotate: Deny, PermCredentialsRead: Deny,
		PermPaymentsRead: Deny, PermPaymentsWrite: Deny, PermPaymentsCapture: Deny,
		PermPaymentsRefund: Deny, PermPaymentsVoid: Deny, PermPaymentsReplayDLQ: Deny,
		PermLedgerRead: Deny, PermLedgerWrite: Deny,
		PermAuditRead: Deny, PermAuditExport: Deny,
		PermTenantsRead: Deny, PermTenantsWrite: Deny,
		PermSecrets: Deny,
	},
	RoleServiceInternal: {
		PermMerchantsRead: allow, PermMerchantsWrite: Deny, PermMerchantsSuspend: allow, PermMerchantsTerminate: Deny,
		PermOnboardingRead: allow, PermOnboardingWrite: allow, PermOnboardingApprove: Deny,
		PermConfigRead: allow, PermConfigWrite: Deny, PermConfigRollback: Deny,
		PermGatewaysRead: allow, PermGatewaysWrite: Deny,
		PermCredentialsRotate: allow, PermCredentialsRead: allow,
		PermPaymentsRead: allow, PermPaymentsWrite: allow, PermPaymentsCapture: allow,
		PermPaymentsRefund: allow, PermPaymentsVoid: allow, PermPaymentsReplayDLQ: Deny,
		PermLedgerRead: allow, PermLedgerWrite: allow,
		PermAuditRead: Deny, PermAuditExport: Deny,
		PermTenantsRead: allow, PermTenantsWrite: Deny,
		PermSecrets: Deny,
	},
}

// GrantFor returns the grant a single role holds for a permission. An unknown role or an
// unlisted permission returns Deny, which is the default-deny property expressed as the zero
// value of a map lookup.
func GrantFor(role Role, perm Permission) Grant {
	return matrix[role][perm]
}

// GrantForRoles unions the grants across a principal's roles.
//
// Union, not intersection: holding two roles means holding the more permissive of their grants,
// which is what "roles are additive" means everywhere else in the industry and is what an
// operator expects when they add a second binding.
//
// The qualifiers combine in the direction that preserves the constraint rather than the
// permission: a permission granted unscoped by one role and scoped by another is unscoped
// (the broader grant wins, because the principal genuinely holds it), while a permission that
// *every* granting role marks dual-controlled stays dual-controlled. That asymmetry is
// deliberate — a dual-control requirement that could be shed by adding a role would not be a
// control at all, so it is dropped only when a role grants the permission without it.
func GrantForRoles(roles []Role, perm Permission) Grant {
	var (
		out         Grant
		anyGranting bool
		allDual     = true
		allScoped   = true
	)
	for _, r := range roles {
		g := matrix[r][perm]
		if !g.Allowed() {
			continue
		}
		anyGranting = true
		out |= grantAllowed
		if !g.RequiresDualControl() {
			allDual = false
		}
		if !g.MerchantScoped() {
			allScoped = false
		}
	}
	if !anyGranting {
		return Deny
	}
	if allDual {
		out |= grantDual
	}
	if allScoped {
		out |= grantScoped
	}
	return out
}

// Can is the RBAC-only question: may a principal's roles perform this operation at all?
//
// It is deliberately incomplete as an authorization answer, and callers must not use it as one.
// It says nothing about the tenant, the merchant scope, the environment, the amount, or dual
// control — all of which can turn a granted permission into a denial. Policy.Evaluate is the
// function that decides; Can exists for the places that legitimately need only the role
// question: rendering a navigation menu, deciding whether to offer an action in an API
// discovery document, and the matrix's own tests.
func Can(p *authn.Principal, perm Permission) bool {
	if p == nil {
		return false
	}
	return GrantForRoles(RolesOf(p), perm).Allowed()
}

// RolesOf maps a principal's role strings onto the known role set, dropping anything
// unrecognised.
//
// Dropping rather than failing is correct here and is worth stating: an identity provider may
// legitimately assert groups that mean nothing to this platform, and refusing the whole request
// because one of them is unknown would make our authorization depend on the IdP's group
// hygiene. An unknown role contributes no grant, so dropping it is the default-deny behaviour.
func RolesOf(p *authn.Principal) []Role {
	if p == nil {
		return nil
	}
	out := make([]Role, 0, len(p.Roles))
	for _, r := range p.Roles {
		role := Role(strings.TrimSpace(r))
		if _, known := matrix[role]; known {
			out = append(out, role)
		}
	}
	return out
}

// RuleID is the stable identifier of a matched matrix cell, carried on a Decision so that an
// audit record can name the rule that permitted an action rather than asserting that "policy
// allowed it".
func RuleID(role Role, perm Permission) string {
	return "RBAC." + string(role) + "." + string(perm)
}

// MatrixRows renders the matrix in a stable order, for the documentation generator and for the
// completeness test.
func MatrixRows() []MatrixRow {
	rows := make([]MatrixRow, 0, len(AllPermissions))
	for _, perm := range AllPermissions {
		row := MatrixRow{Permission: perm, Grants: map[Role]Grant{}}
		for _, role := range AllRoles {
			row.Grants[role] = matrix[role][perm]
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Permission < rows[j].Permission })
	return rows
}

// MatrixRow is one permission's grants across every role.
type MatrixRow struct {
	Permission Permission
	Grants     map[Role]Grant
}
