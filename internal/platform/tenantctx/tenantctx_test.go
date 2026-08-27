package tenantctx_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
)

func tenantA() shared.TenantID {
	return shared.TenantID(ids.MustParseAs("ten_01J0000000000000000000000A", ids.PrefixTenant))
}
func tenantB() shared.TenantID {
	return shared.TenantID(ids.MustParseAs("ten_01J0000000000000000000000B", ids.PrefixTenant))
}

func merchant(suffix string) shared.MerchantID {
	return shared.MerchantID("mrc_01J000000000000000000000" + suffix)
}

func valid() tenantctx.TenantContext {
	return tenantctx.TenantContext{
		TenantID:      tenantA(),
		Tier:          shared.TierPooled,
		Environment:   shared.EnvironmentProduction,
		Principal:     tenantctx.Principal{Type: tenantctx.PrincipalMachine, ID: "cli_1", Name: "checkout"},
		Scopes:        []string{"payments:write"},
		RequestID:     "req_1",
		CorrelationID: "cor_1",
		Source:        tenantctx.SourceToken,
	}
}

func mustCtx(t *testing.T, tc tenantctx.TenantContext) context.Context {
	t.Helper()
	ctx, err := tenantctx.WithTenant(context.Background(), tc)
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}
	return ctx
}

func codeOf(err error) apierror.Code {
	var e *apierror.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestFromContextWithoutTenantIsMissingTenantContext(t *testing.T) {
	t.Parallel()
	if _, err := tenantctx.FromContext(context.Background()); codeOf(err) != apierror.CodeMissingTenantContext {
		t.Fatalf("want MISSING_TENANT_CONTEXT, got %v", err)
	}
	if _, err := tenantctx.TenantID(context.Background()); codeOf(err) != apierror.CodeMissingTenantContext {
		t.Fatalf("TenantID: want MISSING_TENANT_CONTEXT, got %v", err)
	}
	if got := tenantctx.MustTenantID(context.Background()); got != "" {
		t.Fatalf("MustTenantID on empty context = %q, want empty", got)
	}
}

func TestWithTenantRejectsInvalidContexts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*tenantctx.TenantContext)
	}{
		{"no tenant", func(tc *tenantctx.TenantContext) { tc.TenantID = "" }},
		{"malformed tenant", func(tc *tenantctx.TenantContext) { tc.TenantID = "not-a-tenant" }},
		{"merchant id in tenant slot", func(tc *tenantctx.TenantContext) { tc.TenantID = shared.TenantID(merchant("1")) }},
		{"unknown tier", func(tc *tenantctx.TenantContext) { tc.Tier = "DEDICATED" }},
		{"unknown environment", func(tc *tenantctx.TenantContext) { tc.Environment = "staging" }},
		{"unknown source", func(tc *tenantctx.TenantContext) { tc.Source = "HEADER" }},
		{"absent source", func(tc *tenantctx.TenantContext) { tc.Source = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := valid()
			tc.mutate(&v)
			if _, err := tenantctx.WithTenant(context.Background(), v); codeOf(err) != apierror.CodeMissingTenantContext {
				t.Fatalf("want MISSING_TENANT_CONTEXT, got %v", err)
			}
		})
	}
}

func TestWithTenantCopiesSlices(t *testing.T) {
	t.Parallel()
	scopes := []string{"payments:read"}
	scope := []shared.MerchantID{merchant("1")}
	v := valid()
	v.Scopes, v.MerchantScope = scopes, scope
	ctx := mustCtx(t, v)

	// A caller that keeps the slice must not be able to widen the credential afterwards.
	scopes[0] = "payments:write"
	scope[0] = merchant("9")

	got, err := tenantctx.FromContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Scopes[0] != "payments:read" {
		t.Fatalf("scope mutated through the caller's slice: %q", got.Scopes[0])
	}
	if got.MerchantScope[0] != merchant("1") {
		t.Fatalf("merchant scope mutated through the caller's slice: %q", got.MerchantScope[0])
	}
	// And the accessors must not hand the backing array back out either.
	got.AllScopes()[0] = "secrets:read"
	got.Merchants()[0] = merchant("9")
	again, _ := tenantctx.FromContext(ctx)
	if again.Scopes[0] != "payments:read" || again.MerchantScope[0] != merchant("1") {
		t.Fatal("accessor returned the live backing array")
	}
}

func TestAssertTenant(t *testing.T) {
	// Verifies: FR-06, NFR-29.
	t.Parallel()
	ctx := mustCtx(t, valid())

	if err := tenantctx.AssertTenant(ctx, tenantA()); err != nil {
		t.Fatalf("same tenant must pass: %v", err)
	}
	if err := tenantctx.AssertTenant(ctx, tenantB()); codeOf(err) != apierror.CodeTenantMismatch {
		t.Fatalf("want TENANT_MISMATCH, got %v", err)
	}
	// An unstamped resource is not "probably ours".
	if err := tenantctx.AssertTenant(ctx, ""); codeOf(err) != apierror.CodeTenantMismatch {
		t.Fatalf("empty resource tenant: want TENANT_MISMATCH, got %v", err)
	}
	// No context at all is a different failure with a different remedy.
	if err := tenantctx.AssertTenant(context.Background(), tenantA()); codeOf(err) != apierror.CodeMissingTenantContext {
		t.Fatalf("want MISSING_TENANT_CONTEXT, got %v", err)
	}
}

// The isolation guard's headline case: the token says tenant A, the body says tenant B.
func TestBodySuppliedTenantThatDisagreesIsRejected(t *testing.T) {
	// Verifies: FR-06.
	t.Parallel()
	ctx := mustCtx(t, valid())

	err := tenantctx.AssertBodyTenant(ctx, tenantB().String())
	if codeOf(err) != apierror.CodeTenantMismatch {
		t.Fatalf("disagreeing body tenant: want TENANT_MISMATCH, got %v", err)
	}
	var e *apierror.Error
	errors.As(err, &e)
	if e.HTTPStatus() != 403 {
		t.Fatalf("TENANT_MISMATCH must be 403, got %d", e.HTTPStatus())
	}
	if e.Category != apierror.CategoryAuthorization {
		t.Fatalf("a disagreeing tenant is an authorization event, not a validation error: %v", e.Category)
	}
	// It must not tell the caller which tenant would have been right.
	if len(e.Details) != 0 {
		t.Fatalf("the guard must not return an enumeration oracle: %+v", e.Details)
	}
	if got := e.Error(); got != "TENANT_MISMATCH: the requested resource belongs to a different tenant" {
		t.Fatalf("message leaks the expected tenant: %q", got)
	}

	// An absent body tenant is ignored, and an agreeing one is ignored too — it never becomes
	// the tenant, it is merely not a security event.
	if err := tenantctx.AssertBodyTenant(ctx, ""); err != nil {
		t.Fatalf("absent body tenant must be ignored: %v", err)
	}
	if err := tenantctx.AssertBodyTenant(ctx, tenantA().String()); err != nil {
		t.Fatalf("agreeing body tenant must be ignored: %v", err)
	}
	after, _ := tenantctx.FromContext(ctx)
	if after.TenantID != tenantA() || after.Source != tenantctx.SourceToken {
		t.Fatal("the body must never influence the established tenant context")
	}
}

func TestCoversMerchant(t *testing.T) {
	t.Parallel()
	unscoped := mustCtx(t, valid())
	tc, _ := tenantctx.FromContext(unscoped)
	if !tc.CoversMerchant(merchant("7")) {
		t.Fatal("an empty merchant scope means every merchant of this tenant")
	}

	v := valid()
	v.MerchantScope = []shared.MerchantID{merchant("1"), merchant("2")}
	scoped, _ := tenantctx.FromContext(mustCtx(t, v))
	if !scoped.CoversMerchant(merchant("2")) {
		t.Fatal("in-scope merchant rejected")
	}
	if scoped.CoversMerchant(merchant("3")) {
		t.Fatal("out-of-scope merchant accepted")
	}
}

func TestDetachedKeepsTenantAndDropsCancellation(t *testing.T) {
	t.Parallel()
	base, cancel := context.WithDeadline(mustCtx(t, valid()), time.Now().Add(time.Hour))
	d := tenantctx.Detached(base)
	cancel()

	if base.Err() == nil {
		t.Fatal("precondition: parent should be cancelled")
	}
	if d.Err() != nil {
		t.Fatalf("detached context must survive the parent's cancellation: %v", d.Err())
	}
	if _, ok := d.Deadline(); ok {
		t.Fatal("detached context must not carry the parent's deadline")
	}
	tc, err := tenantctx.FromContext(d)
	if err != nil {
		t.Fatalf("detached context lost the tenant: %v", err)
	}
	if tc.TenantID != tenantA() || tc.CorrelationID != "cor_1" {
		t.Fatalf("detached context lost identity or trace values: %+v", tc)
	}
}

func TestSourceIsValid(t *testing.T) {
	t.Parallel()
	for _, s := range []tenantctx.Source{tenantctx.SourceToken, tenantctx.SourceEventEnvelope, tenantctx.SourceWorkflowInstance} {
		if !s.IsValid() {
			t.Fatalf("%q must be a sanctioned origin", s)
		}
	}
	for _, s := range []tenantctx.Source{"", "HEADER", "BODY", "token"} {
		if s.IsValid() {
			t.Fatalf("%q must not be a sanctioned origin", s)
		}
	}
}
