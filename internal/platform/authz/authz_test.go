package authz_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authn"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authz"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

const (
	tenantA  = shared.TenantID("ten_01J0000000000000000000000A")
	tenantB  = shared.TenantID("ten_01J0000000000000000000000B")
	merchant = shared.MerchantID("mrc_01J000000000000000000000A")
	other    = shared.MerchantID("mrc_01J000000000000000000000B")
)

var now = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

func ctxFor(t *testing.T, tenant shared.TenantID) context.Context {
	t.Helper()
	ctx, err := tenantctx.WithTenant(context.Background(), tenantctx.TenantContext{
		TenantID:    tenant,
		Tier:        shared.TierPooled,
		Environment: shared.EnvironmentProduction,
		Principal:   tenantctx.Principal{Type: tenantctx.PrincipalHuman, ID: "usr_1"},
		Source:      tenantctx.SourceToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func principal(role authz.Role, mutate func(*authn.Principal)) *authn.Principal {
	p := &authn.Principal{
		Method:      authn.MethodJWT,
		Type:        tenantctx.PrincipalHuman,
		ID:          "usr_1",
		TenantID:    tenantA,
		Roles:       []string{string(role)},
		Environment: shared.EnvironmentProduction,
		MFA:         true,
		AuthTime:    now.Add(-time.Minute),
		Device:      authn.DevicePosture{Managed: true, Compliant: true},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

func request(p *authn.Principal, perm authz.Permission, mutate func(*authz.Request)) authz.Request {
	req := authz.Request{
		Principal:  p,
		Permission: perm,
		Now:        now,
		SourceIP:   netip.MustParseAddr("203.0.113.4"),
		Resource: authz.Resource{
			TenantID:      tenantA,
			MerchantID:    merchant,
			Environment:   shared.EnvironmentProduction,
			MerchantState: "ACTIVE",
		},
	}
	if mutate != nil {
		mutate(&req)
	}
	return req
}

func newPolicy(t *testing.T, mutate func(*authz.PolicyConfig)) *authz.Policy {
	t.Helper()
	cfg := authz.PolicyConfig{
		Environment: shared.EnvironmentProduction,
		Clock:       shared.FixedClock{T: now},
		Approvals:   authz.NewMemoryApprovals(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := authz.NewPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// --- the matrix ------------------------------------------------------------------------------

// Every (role, permission) pair must have an explicit entry. An omission denies — that is the
// default-deny property — but an *accidental* omission is a bug, and this is what distinguishes
// the two.
func TestMatrixIsComplete(t *testing.T) {
	t.Parallel()
	rows := authz.MatrixRows()
	if len(rows) != len(authz.AllPermissions) {
		t.Fatalf("matrix has %d rows, want %d", len(rows), len(authz.AllPermissions))
	}
	for _, row := range rows {
		if len(row.Grants) != len(authz.AllRoles) {
			t.Fatalf("permission %q has %d columns, want %d", row.Permission, len(row.Grants), len(authz.AllRoles))
		}
	}
}

// The three lines of the matrix that carry the most design intent, asserted so a well-meaning
// widening shows up as a failing test rather than as a review someone was rushed through.
func TestMatrixInvariants(t *testing.T) {
	t.Parallel()

	t.Run("no human role holds payments:write", func(t *testing.T) {
		t.Parallel()
		humans := []authz.Role{
			authz.RolePlatformAdmin, authz.RoleTenantAdmin, authz.RoleMerchantAdmin,
			authz.RoleOperator, authz.RoleAuditor,
		}
		for _, r := range humans {
			for _, perm := range []authz.Permission{authz.PermPaymentsWrite, authz.PermPaymentsCapture} {
				if authz.GrantFor(r, perm).Allowed() {
					t.Fatalf("%s must not hold %s: humans do not create payments", r, perm)
				}
			}
		}
	})

	t.Run("secrets are denied to every principal", func(t *testing.T) {
		t.Parallel()
		for _, r := range authz.AllRoles {
			if authz.GrantFor(r, authz.PermSecrets).Allowed() {
				t.Fatalf("%s must not hold secrets:*", r)
			}
		}
	})

	t.Run("platform-admin is not omnipotent", func(t *testing.T) {
		t.Parallel()
		for _, perm := range []authz.Permission{
			authz.PermPaymentsWrite, authz.PermPaymentsRefund, authz.PermSecrets, authz.PermCredentialsRead,
		} {
			if authz.GrantFor(authz.RolePlatformAdmin, perm).Allowed() {
				t.Fatalf("platform-admin must not hold %s", perm)
			}
		}
	})

	t.Run("a merchant admin's refund is scoped and dual-controlled", func(t *testing.T) {
		t.Parallel()
		g := authz.GrantFor(authz.RoleMerchantAdmin, authz.PermPaymentsRefund)
		if !g.Allowed() || !g.MerchantScoped() || !g.RequiresDualControl() {
			t.Fatalf("merchant-admin × payments:refund = %s, want S+D", g)
		}
	})

	t.Run("terminate and tenant writes are dual-controlled everywhere they are granted", func(t *testing.T) {
		t.Parallel()
		for _, perm := range []authz.Permission{authz.PermMerchantsTerminate, authz.PermTenantsWrite, authz.PermOnboardingApprove} {
			for _, r := range authz.AllRoles {
				g := authz.GrantFor(r, perm)
				if g.Allowed() && !g.RequiresDualControl() {
					t.Fatalf("%s × %s is granted without dual control", r, perm)
				}
			}
		}
	})
}

func TestCanIsRoleOnly(t *testing.T) {
	t.Parallel()
	if !authz.Can(principal(authz.RoleTenantAdmin, nil), authz.PermConfigWrite) {
		t.Fatal("tenant-admin holds config:write")
	}
	if authz.Can(principal(authz.RoleAuditor, nil), authz.PermConfigWrite) {
		t.Fatal("auditor must not hold config:write")
	}
	if authz.Can(nil, authz.PermConfigRead) {
		t.Fatal("a nil principal holds nothing")
	}
	// An unrecognised role contributes nothing rather than failing the request.
	unknown := principal(authz.RoleTenantAdmin, func(p *authn.Principal) { p.Roles = []string{"wizard"} })
	if authz.Can(unknown, authz.PermConfigRead) {
		t.Fatal("an unknown role must grant nothing")
	}
}

func TestGrantForRolesUnions(t *testing.T) {
	t.Parallel()
	// auditor cannot write config; tenant-admin can. Holding both means holding the union.
	roles := []authz.Role{authz.RoleAuditor, authz.RoleTenantAdmin}
	if !authz.GrantForRoles(roles, authz.PermConfigWrite).Allowed() {
		t.Fatal("roles are additive")
	}
	// merchant-admin's refund is scoped+dual; svc:payment-client's is unscoped and undualed.
	// Holding both sheds both qualifiers, because one role genuinely grants it outright.
	g := authz.GrantForRoles([]authz.Role{authz.RoleMerchantAdmin, authz.RoleServicePaymentClient}, authz.PermPaymentsRefund)
	if !g.Allowed() || g.MerchantScoped() || g.RequiresDualControl() {
		t.Fatalf("union = %s, want ✓", g)
	}
	// But a qualifier that *every* granting role carries survives.
	d := authz.GrantForRoles([]authz.Role{authz.RolePlatformAdmin, authz.RoleTenantAdmin}, authz.PermMerchantsTerminate)
	if !d.RequiresDualControl() {
		t.Fatal("a dual-control requirement that every granting role carries must survive the union")
	}
	if authz.GrantForRoles(nil, authz.PermConfigRead).Allowed() {
		t.Fatal("no roles means no grants")
	}
}

// --- default deny ------------------------------------------------------------------------------

func TestDefaultDeny(t *testing.T) {
	// Verifies: FR-05, NFR-34.
	t.Parallel()
	p := newPolicy(t, nil)
	ctx := ctxFor(t, tenantA)

	t.Run("the zero Decision denies", func(t *testing.T) {
		t.Parallel()
		var d authz.Decision
		if d.Allow || !d.Denied() {
			t.Fatal("an unpopulated Decision must deny")
		}
	})

	t.Run("no tenant context", func(t *testing.T) {
		t.Parallel()
		d := p.Evaluate(context.Background(), request(principal(authz.RoleTenantAdmin, nil), authz.PermConfigRead, nil))
		if d.Allow || d.Reason != authz.ReasonNoTenantContext {
			t.Fatalf("decision = %s", d)
		}
		if apierror.CodeOf(d.Error()) != apierror.CodeMissingTenantContext {
			t.Fatalf("error = %v", d.Error())
		}
	})

	t.Run("no principal", func(t *testing.T) {
		t.Parallel()
		d := p.Evaluate(ctx, request(nil, authz.PermConfigRead, nil))
		if d.Allow || d.Reason != authz.ReasonUnauthenticated {
			t.Fatalf("decision = %s", d)
		}
	})

	t.Run("revoked principal", func(t *testing.T) {
		t.Parallel()
		pr := principal(authz.RoleTenantAdmin, func(p *authn.Principal) { p.Revoked = true })
		d := p.Evaluate(ctx, request(pr, authz.PermConfigRead, nil))
		if d.Allow || d.Reason != authz.ReasonRevoked {
			t.Fatalf("decision = %s", d)
		}
	})

	t.Run("permission not granted", func(t *testing.T) {
		t.Parallel()
		d := p.Evaluate(ctx, request(principal(authz.RoleAuditor, nil), authz.PermConfigWrite, nil))
		if d.Allow || d.Reason != authz.ReasonPermissionNotGranted {
			t.Fatalf("decision = %s", d)
		}
		if d.Error().HTTPStatus() != 403 {
			t.Fatalf("status = %d", d.Error().HTTPStatus())
		}
	})

	t.Run("unknown permission", func(t *testing.T) {
		t.Parallel()
		d := p.Evaluate(ctx, request(principal(authz.RolePlatformAdmin, nil), authz.Permission("wat:read"), nil))
		if d.Allow || d.Reason != authz.ReasonUnknownPermission {
			t.Fatalf("decision = %s", d)
		}
	})

	// The exhaustive sweep: every role against every permission, asserting that nothing is
	// allowed that the matrix does not grant.
	t.Run("nothing outside the matrix is ever allowed", func(t *testing.T) {
		t.Parallel()
		for _, role := range authz.AllRoles {
			for _, perm := range authz.AllPermissions {
				pr := principal(role, func(p *authn.Principal) { p.Roles = []string{string(role)} })
				d := p.Evaluate(ctx, request(pr, perm, func(r *authz.Request) {
					r.PermittedRegions = []string{"eu-west-1"}
				}))
				if d.Allow && !authz.GrantFor(role, perm).Allowed() {
					t.Fatalf("%s was allowed %s but the matrix denies it", role, perm)
				}
			}
		}
	})
}

// Every generated principal × permission × resource across a tenant boundary must deny. This is
// the property the whole isolation model rests on.
func TestNoAllowEverCrossesATenantBoundary(t *testing.T) {
	// Verifies: FR-05, NFR-29.
	t.Parallel()
	p := newPolicy(t, nil)
	ctx := ctxFor(t, tenantA)
	for _, role := range authz.AllRoles {
		for _, perm := range authz.AllPermissions {
			pr := principal(role, func(pp *authn.Principal) { pp.Roles = []string{string(role)} })
			d := p.Evaluate(ctx, request(pr, perm, func(r *authz.Request) {
				r.Resource.TenantID = tenantB
				r.PermittedRegions = []string{"eu-west-1"}
			}))
			if d.Allow {
				t.Fatalf("%s was allowed %s across a tenant boundary", role, perm)
			}
		}
	}
}

func TestExplicitDenyBeatsAllow(t *testing.T) {
	t.Parallel()
	pr := principal(authz.RoleTenantAdmin, nil)
	ctx := ctxFor(t, tenantA)

	// Without the freeze, the operation is allowed.
	base := newPolicy(t, nil)
	if d := base.Evaluate(ctx, request(pr, authz.PermConfigRead, nil)); !d.Allow {
		t.Fatalf("precondition: %s", d)
	}

	frozen := newPolicy(t, func(c *authz.PolicyConfig) {
		c.ExplicitDenies = []authz.DenyRule{{
			ID:    "INCIDENT-2026-05-01.FREEZE_CLIENT",
			Match: func(r authz.Request) bool { return r.Principal != nil && r.Principal.ID == "usr_1" },
		}}
	})
	d := frozen.Evaluate(ctx, request(pr, authz.PermConfigRead, nil))
	if d.Allow {
		t.Fatal("an explicit deny must not be overridable by a role binding")
	}
	if d.Reason != authz.ReasonExplicitDeny || d.Detail != "INCIDENT-2026-05-01.FREEZE_CLIENT" {
		t.Fatalf("decision = %s; the matched deny rule must be named for the audit", d)
	}
}

func TestAllowIsExplainable(t *testing.T) {
	t.Parallel()
	p := newPolicy(t, nil)
	d := p.Evaluate(ctxFor(t, tenantA), request(principal(authz.RoleTenantAdmin, nil), authz.PermConfigWrite, nil))
	if !d.Allow {
		t.Fatalf("decision = %s", d)
	}
	if len(d.MatchedRules) != 1 || d.MatchedRules[0] != authz.RuleID(authz.RoleTenantAdmin, authz.PermConfigWrite) {
		t.Fatalf("matched rules = %v; an audit record must name the rule that permitted the action", d.MatchedRules)
	}
	if len(d.SatisfiedConditions) == 0 {
		t.Fatal("an allow must record which conditions were actually checked")
	}
	if d.Error() != nil {
		t.Fatal("an allow has no error")
	}
}

// --- each ABAC condition in isolation -----------------------------------------------------------

func TestEachConditionInIsolation(t *testing.T) {
	t.Parallel()
	ctx := ctxFor(t, tenantA)
	usd := func(minor int64) *money.Money {
		m := money.MustNew(minor, "USD")
		return &m
	}

	cases := []struct {
		name    string
		cond    authz.Condition
		req     authz.Request
		mutate  func(*authz.Request)
		wantOK  bool
		wantBad bool
	}{
		{name: "tenant match holds for the pinned tenant", cond: authz.TenantMatch(),
			req: request(principal(authz.RoleTenantAdmin, nil), authz.PermConfigRead, nil), wantOK: true},
		{name: "tenant match fails across tenants", cond: authz.TenantMatch(),
			req: request(principal(authz.RoleTenantAdmin, nil), authz.PermConfigRead, func(r *authz.Request) {
				r.Resource.TenantID = tenantB
			})},
		{name: "tenant match fails on an unstamped resource", cond: authz.TenantMatch(),
			req: request(principal(authz.RoleTenantAdmin, nil), authz.PermConfigRead, func(r *authz.Request) {
				r.Resource.TenantID = ""
			})},
		{name: "tenant match fails when the principal's tenant disagrees with the context", cond: authz.TenantMatch(),
			req: request(principal(authz.RoleTenantAdmin, func(p *authn.Principal) { p.TenantID = tenantB }),
				authz.PermConfigRead, nil)},

		{name: "merchant scope: empty means the whole tenant", cond: authz.MerchantScope(),
			req: request(principal(authz.RoleTenantAdmin, nil), authz.PermConfigRead, nil), wantOK: true},
		{name: "merchant scope: in scope", cond: authz.MerchantScope(),
			req: request(principal(authz.RoleMerchantAdmin, func(p *authn.Principal) {
				p.MerchantScope = []shared.MerchantID{merchant}
			}), authz.PermConfigRead, nil), wantOK: true},
		{name: "merchant scope: out of scope", cond: authz.MerchantScope(),
			req: request(principal(authz.RoleMerchantAdmin, func(p *authn.Principal) {
				p.MerchantScope = []shared.MerchantID{other}
			}), authz.PermConfigRead, nil)},
		{name: "merchant scope: a scoped credential on a merchantless resource", cond: authz.MerchantScope(),
			req: request(principal(authz.RoleMerchantAdmin, func(p *authn.Principal) {
				p.MerchantScope = []shared.MerchantID{merchant}
			}), authz.PermConfigRead, func(r *authz.Request) { r.Resource.MerchantID = "" })},

		{name: "environment: all three agree", cond: authz.EnvironmentMatch(shared.EnvironmentProduction),
			req: request(principal(authz.RoleTenantAdmin, nil), authz.PermConfigRead, nil), wantOK: true},
		{name: "environment: sandbox principal on production resource", cond: authz.EnvironmentMatch(shared.EnvironmentProduction),
			req: request(principal(authz.RoleTenantAdmin, func(p *authn.Principal) {
				p.Environment = shared.EnvironmentSandbox
			}), authz.PermConfigRead, nil)},
		{name: "environment: sandbox resource in a production deployment", cond: authz.EnvironmentMatch(shared.EnvironmentProduction),
			req: request(principal(authz.RoleTenantAdmin, func(p *authn.Principal) {
				p.Environment = shared.EnvironmentSandbox
			}), authz.PermConfigRead, func(r *authz.Request) { r.Resource.Environment = shared.EnvironmentSandbox })},

		{name: "amount: under the threshold", cond: authz.AmountThreshold(),
			req: request(principal(authz.RoleMerchantAdmin, nil), authz.PermPaymentsRefund, func(r *authz.Request) {
				r.Operation, r.Resource.Amount, r.DualControlThreshold = "refund", usd(1000), usd(50000)
			}), wantOK: true},
		{name: "amount: over the threshold needs a second person", cond: authz.AmountThreshold(),
			req: request(principal(authz.RoleMerchantAdmin, nil), authz.PermPaymentsRefund, func(r *authz.Request) {
				r.Operation, r.Resource.Amount, r.DualControlThreshold = "refund", usd(100000), usd(50000)
			})},
		{name: "amount: an unconfigured threshold is not 'no limit'", cond: authz.AmountThreshold(),
			req: request(principal(authz.RoleMerchantAdmin, nil), authz.PermPaymentsRefund, func(r *authz.Request) {
				r.Operation, r.Resource.Amount = "refund", usd(1000)
			})},
		{name: "amount: a currency mismatch cannot be used to bypass the threshold", cond: authz.AmountThreshold(),
			req: request(principal(authz.RoleMerchantAdmin, nil), authz.PermPaymentsRefund, func(r *authz.Request) {
				eur := money.MustNew(1000, "EUR")
				r.Operation, r.Resource.Amount, r.DualControlThreshold = "refund", &eur, usd(50000)
			})},
		{name: "amount: not a money-moving operation", cond: authz.AmountThreshold(),
			req: request(principal(authz.RoleTenantAdmin, nil), authz.PermConfigRead, nil), wantOK: true},

		{name: "residency: no constraint on the resource", cond: authz.Residency("eu-west-1"),
			req: request(principal(authz.RoleTenantAdmin, nil), authz.PermMerchantsRead, nil), wantOK: true},
		{name: "residency: permitted and in-region", cond: authz.Residency("eu-west-1"),
			req: request(principal(authz.RoleTenantAdmin, nil), authz.PermMerchantsRead, func(r *authz.Request) {
				r.Resource.ResidencyRegion, r.PermittedRegions = "eu-west-1", []string{"eu-west-1"}
			}), wantOK: true},
		{name: "residency: EU data read from a US deployment", cond: authz.Residency("us-east-1"),
			req: request(principal(authz.RoleTenantAdmin, nil), authz.PermMerchantsRead, func(r *authz.Request) {
				r.Resource.ResidencyRegion, r.PermittedRegions = "eu-west-1", []string{"eu-west-1"}
			})},
		{name: "residency: subject not permitted in the region", cond: authz.Residency("eu-west-1"),
			req: request(principal(authz.RoleTenantAdmin, nil), authz.PermMerchantsRead, func(r *authz.Request) {
				r.Resource.ResidencyRegion, r.PermittedRegions = "eu-west-1", []string{"us-east-1"}
			})},

		{name: "merchant state: active merchant may take payments", cond: authz.MerchantState(),
			req: request(principal(authz.RoleServicePaymentClient, nil), authz.PermPaymentsWrite, nil), wantOK: true},
		{name: "merchant state: suspended merchant may not take payments", cond: authz.MerchantState(),
			req: request(principal(authz.RoleServicePaymentClient, nil), authz.PermPaymentsWrite, func(r *authz.Request) {
				r.Resource.MerchantState = "SUSPENDED"
			})},
		{name: "merchant state: suspended merchant may still give money back", cond: authz.MerchantState(),
			req: request(principal(authz.RoleServicePaymentClient, nil), authz.PermPaymentsRefund, func(r *authz.Request) {
				r.Resource.MerchantState = "SUSPENDED"
			}), wantOK: true},
		{name: "merchant state: terminated merchant may not refund", cond: authz.MerchantState(),
			req: request(principal(authz.RoleServicePaymentClient, nil), authz.PermPaymentsRefund, func(r *authz.Request) {
				r.Resource.MerchantState = "TERMINATED"
			})},

		{name: "time window: inside", cond: authz.TimeWindow(),
			req: request(principal(authz.RoleOperator, nil), authz.PermPaymentsRead, func(r *authz.Request) {
				r.AllowedHours = &authz.HourWindow{StartHour: 8, EndHour: 18}
			}), wantOK: true},
		{name: "time window: outside", cond: authz.TimeWindow(),
			req: request(principal(authz.RoleOperator, nil), authz.PermPaymentsRead, func(r *authz.Request) {
				r.AllowedHours = &authz.HourWindow{StartHour: 0, EndHour: 6}
			})},
		{name: "time window: does not apply to machines", cond: authz.TimeWindow(),
			req: request(principal(authz.RoleServicePaymentClient, func(p *authn.Principal) {
				p.Type = tenantctx.PrincipalMachine
			}), authz.PermPaymentsWrite, func(r *authz.Request) {
				r.AllowedHours = &authz.HourWindow{StartHour: 0, EndHour: 6}
			}), wantOK: true},

		{name: "device posture: managed and compliant", cond: authz.DevicePosture(),
			req: request(principal(authz.RolePlatformAdmin, nil), authz.PermCredentialsRotate, nil), wantOK: true},
		{name: "device posture: unmanaged", cond: authz.DevicePosture(),
			req: request(principal(authz.RolePlatformAdmin, func(p *authn.Principal) {
				p.Device = authn.DevicePosture{Managed: false, Compliant: true}
			}), authz.PermCredentialsRotate, nil)},
		{name: "device posture: absent assertion denies", cond: authz.DevicePosture(),
			req: request(principal(authz.RolePlatformAdmin, func(p *authn.Principal) {
				p.Device = authn.DevicePosture{}
			}), authz.PermCredentialsRotate, nil)},

		{name: "source constraint: bound to this connection", cond: authz.SourceConstraint(),
			req: request(principal(authz.RoleServicePaymentClient, func(p *authn.Principal) {
				p.ConfirmationThumbprint = "thumb-1"
			}), authz.PermPaymentsRefund, func(r *authz.Request) { r.PeerThumbprint = "thumb-1" }), wantOK: true},
		{name: "source constraint: stolen token on another connection", cond: authz.SourceConstraint(),
			req: request(principal(authz.RoleServicePaymentClient, func(p *authn.Principal) {
				p.ConfirmationThumbprint = "thumb-1"
			}), authz.PermPaymentsRefund, func(r *authz.Request) { r.PeerThumbprint = "thumb-2" })},
		{name: "source constraint: no binding at all", cond: authz.SourceConstraint(),
			req: request(principal(authz.RoleServicePaymentClient, nil), authz.PermPaymentsRefund, func(r *authz.Request) {
				r.PeerThumbprint = "thumb-1"
			})},

		{name: "mfa freshness: recent", cond: authz.MFAFreshness(5 * time.Minute),
			req: request(principal(authz.RolePlatformAdmin, nil), authz.PermCredentialsRotate, nil), wantOK: true},
		{name: "mfa freshness: stale", cond: authz.MFAFreshness(5 * time.Minute),
			req: request(principal(authz.RolePlatformAdmin, func(p *authn.Principal) {
				p.AuthTime = now.Add(-time.Hour)
			}), authz.PermCredentialsRotate, nil)},
		{name: "mfa freshness: no second factor", cond: authz.MFAFreshness(5 * time.Minute),
			req: request(principal(authz.RolePlatformAdmin, func(p *authn.Principal) { p.MFA = false }),
				authz.PermCredentialsRotate, nil)},
		{name: "mfa freshness: an authentication in the future is not fresh", cond: authz.MFAFreshness(5 * time.Minute),
			req: request(principal(authz.RolePlatformAdmin, func(p *authn.Principal) {
				p.AuthTime = now.Add(time.Hour)
			}), authz.PermCredentialsRotate, nil)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cond.Holds(ctx, tc.req); got != tc.wantOK {
				t.Fatalf("%s.Holds() = %v, want %v", tc.cond.ID(), got, tc.wantOK)
			}
		})
	}
}

// Every condition must deny a nil principal rather than panic: a policy engine that panics is a
// policy engine that fails open the moment someone recovers the panic.
func TestConditionsAreTotalOnEmptyInput(t *testing.T) {
	t.Parallel()
	conds := []authz.Condition{
		authz.TenantMatch(), authz.MerchantScope(), authz.EnvironmentMatch(shared.EnvironmentProduction),
		authz.AmountThreshold(), authz.Residency("eu-west-1"), authz.MerchantState(),
		authz.TimeWindow(), authz.DevicePosture(), authz.SourceConstraint(), authz.MFAFreshness(time.Minute),
	}
	if len(conds) != 10 {
		t.Fatalf("the documented condition set has 10 members, this has %d", len(conds))
	}
	for _, c := range conds {

		t.Run(c.ID(), func(t *testing.T) {
			t.Parallel()
			// An empty request under an empty context must not panic; whether it holds is the
			// condition's own business (MerchantState legitimately holds for a non-payment
			// permission), but it must terminate and return.
			_ = c.Holds(context.Background(), authz.Request{})
			_ = c.Holds(ctxFor(t, tenantA), authz.Request{Now: now})
		})
	}
}

func TestHourWindowWrapsMidnight(t *testing.T) {
	t.Parallel()
	w := authz.HourWindow{StartHour: 22, EndHour: 6}
	for _, h := range []int{22, 23, 0, 5} {
		if !w.Contains(time.Date(2026, 5, 1, h, 30, 0, 0, time.UTC)) {
			t.Fatalf("hour %d must be inside a 22:00–06:00 window", h)
		}
	}
	for _, h := range []int{6, 12, 21} {
		if w.Contains(time.Date(2026, 5, 1, h, 30, 0, 0, time.UTC)) {
			t.Fatalf("hour %d must be outside a 22:00–06:00 window", h)
		}
	}
}

// --- dual control ------------------------------------------------------------------------------

func TestDualControl(t *testing.T) {
	// Verifies: NFR-34.
	t.Parallel()
	ctx := ctxFor(t, tenantA)
	admin := principal(authz.RolePlatformAdmin, func(p *authn.Principal) { p.ID = "usr_requester" })

	terminate := func(ref string) authz.Request {
		return request(admin, authz.PermMerchantsTerminate, func(r *authz.Request) {
			r.ApprovalRef, r.Fingerprint = ref, "fp-1"
			r.Operation = "terminate"
		})
	}

	t.Run("no approval means denied", func(t *testing.T) {
		t.Parallel()
		p := newPolicy(t, nil)
		d := p.Evaluate(ctx, terminate(""))
		if d.Allow || d.Detail != string(authz.DualControlRequired) {
			t.Fatalf("decision = %s", d)
		}
		// The remediation is legitimate and public, so the error says what is needed.
		if len(d.Error().Details) == 0 || d.Error().Details[0].Code != string(authz.DualControlRequired) {
			t.Fatalf("error = %+v", d.Error())
		}
	})

	t.Run("self-approval is refused", func(t *testing.T) {
		t.Parallel()
		store := authz.NewMemoryApprovals()
		if err := store.Request(authz.Approval{
			Ref: "apr-1", TenantID: tenantA, Permission: authz.PermMerchantsTerminate,
			RequesterID: "usr_requester", RequestFingerprint: "fp-1",
			RequestedAt: now, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		// The store refuses the self-approval outright, which is the immediate feedback path.
		if err := store.Approve("apr-1", "usr_requester", now); err == nil {
			t.Fatal("the requester must not be able to approve their own request")
		} else if apierror.CodeOf(err) != apierror.CodeForbidden {
			t.Fatalf("err = %v", err)
		}

		// And evaluation refuses it too, for a record created some other way.
		p := newPolicy(t, func(c *authz.PolicyConfig) { c.Approvals = selfApproved{} })
		d := p.Evaluate(ctx, terminate("apr-self"))
		if d.Allow || d.Detail != string(authz.DualControlSelfApproval) {
			t.Fatalf("decision = %s", d)
		}
	})

	t.Run("a second person's approval is accepted", func(t *testing.T) {
		t.Parallel()
		store := authz.NewMemoryApprovals()
		if err := store.Request(authz.Approval{
			Ref: "apr-2", TenantID: tenantA, Permission: authz.PermMerchantsTerminate,
			RequesterID: "usr_requester", RequestFingerprint: "fp-1",
			RequestedAt: now, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.Approve("apr-2", "usr_approver", now); err != nil {
			t.Fatal(err)
		}
		p := newPolicy(t, func(c *authz.PolicyConfig) { c.Approvals = store })
		d := p.Evaluate(ctx, terminate("apr-2"))
		if !d.Allow {
			t.Fatalf("decision = %s", d)
		}
		if !d.RequiredDualControl {
			t.Fatal("the decision must record that a second person was required")
		}
	})

	t.Run("an unapproved pending record is not an approval", func(t *testing.T) {
		t.Parallel()
		store := authz.NewMemoryApprovals()
		if err := store.Request(authz.Approval{
			Ref: "apr-3", TenantID: tenantA, Permission: authz.PermMerchantsTerminate,
			RequesterID: "usr_requester", RequestFingerprint: "fp-1",
			RequestedAt: now, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		p := newPolicy(t, func(c *authz.PolicyConfig) { c.Approvals = store })
		if d := p.Evaluate(ctx, terminate("apr-3")); d.Allow {
			t.Fatalf("decision = %s", d)
		}
	})

	t.Run("an expired approval is refused", func(t *testing.T) {
		t.Parallel()
		store := approvedStore(t, "apr-4", "fp-1", now.Add(-time.Minute))
		p := newPolicy(t, func(c *authz.PolicyConfig) { c.Approvals = store })
		d := p.Evaluate(ctx, terminate("apr-4"))
		if d.Allow || d.Detail != string(authz.DualControlStale) {
			t.Fatalf("decision = %s", d)
		}
	})

	t.Run("an approval for a different request is refused", func(t *testing.T) {
		t.Parallel()
		store := approvedStore(t, "apr-5", "fp-OTHER", now.Add(time.Hour))
		p := newPolicy(t, func(c *authz.PolicyConfig) { c.Approvals = store })
		d := p.Evaluate(ctx, terminate("apr-5"))
		if d.Allow || d.Detail != string(authz.DualControlStale) {
			t.Fatalf("decision = %s; an approval for a £10 refund must not authorize a £10 000 one", d)
		}
	})

	t.Run("an approval from another tenant is refused", func(t *testing.T) {
		t.Parallel()
		store := authz.NewMemoryApprovals()
		if err := store.Request(authz.Approval{
			Ref: "apr-6", TenantID: tenantB, Permission: authz.PermMerchantsTerminate,
			RequesterID: "usr_requester", RequestFingerprint: "fp-1",
			RequestedAt: now, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.Approve("apr-6", "usr_approver", now); err != nil {
			t.Fatal(err)
		}
		p := newPolicy(t, func(c *authz.PolicyConfig) { c.Approvals = store })
		d := p.Evaluate(ctx, terminate("apr-6"))
		if d.Allow || d.Detail != string(authz.DualControlWrongTenant) {
			t.Fatalf("decision = %s", d)
		}
	})

	// Authorization is the one place the platform is deliberately not fail-static.
	t.Run("an unreachable approvals store denies", func(t *testing.T) {
		t.Parallel()
		p := newPolicy(t, func(c *authz.PolicyConfig) { c.Approvals = brokenStore{} })
		if d := p.Evaluate(ctx, terminate("apr-7")); d.Allow {
			t.Fatal("a dependency failure in authorization must deny, not fail static")
		}
	})

	t.Run("a deployment with no approvals store cannot perform dual-controlled operations", func(t *testing.T) {
		t.Parallel()
		p := newPolicy(t, func(c *authz.PolicyConfig) { c.Approvals = nil })
		if d := p.Evaluate(ctx, terminate("apr-8")); d.Allow {
			t.Fatalf("decision = %s", d)
		}
	})

	// The amount threshold demands a second person even for a permission the matrix grants
	// outright.
	t.Run("a large refund needs approval even for an unqualified grant", func(t *testing.T) {
		t.Parallel()
		client := principal(authz.RoleServicePaymentClient, func(p *authn.Principal) {
			p.Type = tenantctx.PrincipalMachine
			p.ID = "cli_1"
			p.ConfirmationThumbprint = "thumb-1"
		})
		big := money.MustNew(100000, "USD")
		small := money.MustNew(1000, "USD")
		threshold := money.MustNew(50000, "USD")

		p := newPolicy(t, nil)
		below := p.Evaluate(ctx, request(client, authz.PermPaymentsRefund, func(r *authz.Request) {
			r.Operation, r.Resource.Amount, r.DualControlThreshold = "refund", &small, &threshold
			r.PeerThumbprint = "thumb-1"
		}))
		if !below.Allow || below.RequiredDualControl {
			t.Fatalf("a small refund must not require a second person: %s", below)
		}
		above := p.Evaluate(ctx, request(client, authz.PermPaymentsRefund, func(r *authz.Request) {
			r.Operation, r.Resource.Amount, r.DualControlThreshold = "refund", &big, &threshold
			r.PeerThumbprint = "thumb-1"
		}))
		if above.Allow {
			t.Fatalf("a refund above the threshold must require a second person: %s", above)
		}
	})
}

func TestMemoryApprovalsValidation(t *testing.T) {
	t.Parallel()
	store := authz.NewMemoryApprovals()
	base := authz.Approval{
		Ref: "r", TenantID: tenantA, RequesterID: "usr_1",
		RequestFingerprint: "fp", ExpiresAt: now.Add(time.Hour),
	}
	bad := map[string]func(*authz.Approval){
		"no ref":         func(a *authz.Approval) { a.Ref = "" },
		"no tenant":      func(a *authz.Approval) { a.TenantID = "" },
		"no requester":   func(a *authz.Approval) { a.RequesterID = "" },
		"no fingerprint": func(a *authz.Approval) { a.RequestFingerprint = "" },
		"no expiry":      func(a *authz.Approval) { a.ExpiresAt = time.Time{} },
		"pre-approved":   func(a *authz.Approval) { a.ApproverID = "usr_2" },
	}
	for name, mut := range bad {
		a := base
		mut(&a)
		if err := store.Request(a); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
	if err := store.Request(base); err != nil {
		t.Fatal(err)
	}
	if err := store.Approve("nope", "usr_2", now); err == nil {
		t.Fatal("approving a nonexistent request must fail")
	}
	if err := store.Approve("r", "", now); err == nil {
		t.Fatal("an approval requires an approver")
	}
	if err := store.Approve("r", "usr_2", now.Add(2*time.Hour)); err == nil {
		t.Fatal("an expired request must not be approvable")
	}
	if n := store.Purge(now.Add(2 * time.Hour)); n != 1 {
		t.Fatalf("Purge = %d, want 1", n)
	}
}

func TestNewPolicyRequiresAnEnvironment(t *testing.T) {
	t.Parallel()
	if _, err := authz.NewPolicy(authz.PolicyConfig{}); err == nil {
		t.Fatal("a policy engine with no deployment environment cannot evaluate the environment condition")
	}
	if _, err := authz.NewPolicy(authz.PolicyConfig{Environment: "staging"}); err == nil {
		t.Fatal("an unknown environment must be refused")
	}
}

// --- doubles -----------------------------------------------------------------------------------

type selfApproved struct{}

func (selfApproved) Lookup(context.Context, string) (*authz.Approval, error) {
	return &authz.Approval{
		Ref: "apr-self", TenantID: tenantA, Permission: authz.PermMerchantsTerminate,
		RequesterID: "usr_requester", ApproverID: "usr_requester",
		RequestFingerprint: "fp-1", ExpiresAt: now.Add(time.Hour),
	}, nil
}

type brokenStore struct{}

func (brokenStore) Lookup(context.Context, string) (*authz.Approval, error) {
	return nil, context.DeadlineExceeded
}

func approvedStore(t *testing.T, ref, fingerprint string, expires time.Time) authz.ApprovalStore {
	t.Helper()
	return fixedApproval{a: authz.Approval{
		Ref: ref, TenantID: tenantA, Permission: authz.PermMerchantsTerminate,
		RequesterID: "usr_requester", ApproverID: "usr_approver",
		RequestFingerprint: fingerprint, ExpiresAt: expires,
	}}
}

type fixedApproval struct{ a authz.Approval }

func (f fixedApproval) Lookup(context.Context, string) (*authz.Approval, error) {
	cp := f.a
	return &cp, nil
}
