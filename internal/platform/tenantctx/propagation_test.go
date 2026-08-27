package tenantctx_test

import (
	"context"
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

func TestEventHeadersRoundTripPreservesGuarantees(t *testing.T) {
	t.Parallel()
	v := valid()
	v.MerchantScope = []shared.MerchantID{merchant("1"), merchant("2")}
	ctx := mustCtx(t, v)

	headers, err := tenantctx.EventHeaders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if headers[tenantctx.HeaderTenantID] != tenantA().String() {
		t.Fatalf("tenant header missing: %v", headers)
	}

	restored, err := tenantctx.FromEventHeaders(context.Background(), headers)
	if err != nil {
		t.Fatalf("FromEventHeaders: %v", err)
	}
	got, err := tenantctx.FromContext(restored)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case got.TenantID != v.TenantID:
		t.Fatalf("tenant lost: %q", got.TenantID)
	case got.Tier != v.Tier:
		t.Fatalf("tier lost: %q", got.Tier)
	case got.Environment != v.Environment:
		t.Fatalf("environment lost: %q", got.Environment)
	case got.Principal != v.Principal:
		t.Fatalf("principal lost: %+v", got.Principal)
	case got.CorrelationID != v.CorrelationID || got.RequestID != v.RequestID:
		t.Fatalf("correlation lost: %+v", got)
	case len(got.MerchantScope) != 2 || got.MerchantScope[0] != merchant("1"):
		t.Fatalf("merchant scope lost: %v", got.MerchantScope)
	}
	// The origin must be recorded as the envelope, not silently inherited as TOKEN.
	if got.Source != tenantctx.SourceEventEnvelope {
		t.Fatalf("source = %q, want EVENT_ENVELOPE", got.Source)
	}
}

// Scopes are an authorization grant, not a fact about what happened. An event must not carry
// one, or every event becomes an unsigned, non-expiring bearer token.
func TestEventHeadersDoNotCarryScopes(t *testing.T) {
	t.Parallel()
	v := valid()
	v.Scopes = []string{"payments:write", "merchants:terminate"}
	headers, err := tenantctx.EventHeaders(mustCtx(t, v))
	if err != nil {
		t.Fatal(err)
	}
	for k, val := range headers {
		if val == "payments:write" || val == "merchants:terminate" {
			t.Fatalf("header %q leaked an authorization scope", k)
		}
	}
	restored, _ := tenantctx.FromEventHeaders(context.Background(), headers)
	got, _ := tenantctx.FromContext(restored)
	if len(got.Scopes) != 0 {
		t.Fatalf("consumer restored scopes it was never granted: %v", got.Scopes)
	}
}

func TestEventHeadersRequireATenant(t *testing.T) {
	t.Parallel()
	if _, err := tenantctx.EventHeaders(context.Background()); codeOf(err) != apierror.CodeMissingTenantContext {
		t.Fatalf("an event must not be publishable without a tenant, got %v", err)
	}
}

func TestFromEventHeadersRejectsBadEnvelopes(t *testing.T) {
	t.Parallel()
	good, _ := tenantctx.EventHeaders(mustCtx(t, valid()))

	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"nil", nil},
		{"empty", map[string]string{}},
		{"no tenant", withHeader(good, tenantctx.HeaderTenantID, "")},
		{"malformed tenant", withHeader(good, tenantctx.HeaderTenantID, "ten_nope")},
		{"merchant id in the tenant header", withHeader(good, tenantctx.HeaderTenantID, merchant("1").String())},
		{"unknown tier", withHeader(good, tenantctx.HeaderTenantTier, "DEDICATED")},
		{"unknown environment", withHeader(good, tenantctx.HeaderEnvironment, "staging")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tenantctx.FromEventHeaders(context.Background(), tc.headers); codeOf(err) != apierror.CodeMissingTenantContext {
				t.Fatalf("want MISSING_TENANT_CONTEXT, got %v", err)
			}
		})
	}
}

func TestAssertEnvelopeTenant(t *testing.T) {
	t.Parallel()
	if err := tenantctx.AssertEnvelopeTenant("ten_a", ""); err != nil {
		t.Fatalf("a payload without a tenant is fine: %v", err)
	}
	if err := tenantctx.AssertEnvelopeTenant("ten_a", "ten_a"); err != nil {
		t.Fatalf("agreement is fine: %v", err)
	}
	if err := tenantctx.AssertEnvelopeTenant("ten_a", "ten_b"); codeOf(err) != apierror.CodeTenantMismatch {
		t.Fatalf("want TENANT_MISMATCH, got %v", err)
	}
}

func TestActivityRoundTripPreservesGuarantees(t *testing.T) {
	t.Parallel()
	v := valid()
	v.MerchantScope = []shared.MerchantID{merchant("1")}
	blob, err := tenantctx.MarshalActivity(mustCtx(t, v))
	if err != nil {
		t.Fatal(err)
	}

	restored, err := tenantctx.UnmarshalActivity(context.Background(), blob)
	if err != nil {
		t.Fatalf("UnmarshalActivity: %v", err)
	}
	got, _ := tenantctx.FromContext(restored)
	if got.TenantID != v.TenantID || got.Tier != v.Tier || got.Environment != v.Environment {
		t.Fatalf("identity lost across the workflow boundary: %+v", got)
	}
	if got.Principal != v.Principal {
		t.Fatalf("originating principal lost: %+v", got.Principal)
	}
	if got.Source != tenantctx.SourceWorkflowInstance {
		t.Fatalf("source = %q, want WORKFLOW_INSTANCE", got.Source)
	}
	// An activity restored from an instance must satisfy the isolation guard exactly as the
	// originating HTTP request did.
	if err := tenantctx.AssertTenant(restored, tenantA()); err != nil {
		t.Fatalf("restored activity failed the guard for its own tenant: %v", err)
	}
	if err := tenantctx.AssertTenant(restored, tenantB()); codeOf(err) != apierror.CodeTenantMismatch {
		t.Fatalf("restored activity did not guard against another tenant: %v", err)
	}
}

func TestUnmarshalActivityRejectsUnusableBlobs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		blob []byte
	}{
		{"empty", nil},
		{"not json", []byte("{")},
		{"unknown schema version", []byte(`{"v":99,"tenant_id":"ten_01J0000000000000000000000A","tenant_tier":"POOLED","environment":"production"}`)},
		{"missing version", []byte(`{"tenant_id":"ten_01J0000000000000000000000A","tenant_tier":"POOLED","environment":"production"}`)},
		{"zero tenant", []byte(`{"v":1,"tenant_id":"","tenant_tier":"POOLED","environment":"production"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tenantctx.UnmarshalActivity(context.Background(), tc.blob); codeOf(err) != apierror.CodeMissingTenantContext {
				t.Fatalf("want MISSING_TENANT_CONTEXT, got %v", err)
			}
		})
	}
}

func TestMarshalActivityRequiresATenant(t *testing.T) {
	t.Parallel()
	if _, err := tenantctx.MarshalActivity(context.Background()); codeOf(err) != apierror.CodeMissingTenantContext {
		t.Fatalf("want MISSING_TENANT_CONTEXT, got %v", err)
	}
}

func withHeader(base map[string]string, k, v string) map[string]string {
	out := make(map[string]string, len(base))
	for key, val := range base {
		out[key] = val
	}
	if v == "" {
		delete(out, k)
	} else {
		out[k] = v
	}
	return out
}
