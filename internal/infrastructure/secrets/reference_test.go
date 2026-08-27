package secrets

import (
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

func TestParseReferenceAcceptsEveryFormInCirculation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want Reference
	}{
		{
			name: "canonical with tenant and purpose",
			in:   "secret://production/ten_01JB8Z00000000000000000000/mrc_01JB8Z11111111111111111111/stripe/api_key",
			want: Reference{
				Environment: shared.EnvironmentProduction,
				TenantID:    "ten_01JB8Z00000000000000000000",
				MerchantID:  "mrc_01JB8Z11111111111111111111",
				GatewayID:   "stripe", Purpose: "api_key",
			},
		},
		{
			name: "canonical with a version pin",
			in:   "secret://production/ten_01JB8Z00000000000000000000/mrc_01JB8Z11111111111111111111/adyen/webhook_hmac#v8",
			want: Reference{
				Environment: shared.EnvironmentProduction,
				TenantID:    "ten_01JB8Z00000000000000000000",
				MerchantID:  "mrc_01JB8Z11111111111111111111",
				GatewayID:   "adyen", Purpose: "webhook_hmac", Version: "v8",
			},
		},
		{
			name: "tenant, merchant, gateway with no purpose",
			in:   "secret://sandbox/ten_01JB8Z00000000000000000000/mrc_01JB8Z11111111111111111111/paypal",
			want: Reference{
				Environment: shared.EnvironmentSandbox,
				TenantID:    "ten_01JB8Z00000000000000000000",
				MerchantID:  "mrc_01JB8Z11111111111111111111",
				GatewayID:   "paypal",
			},
		},
		{
			name: "the onboarding workflow's tenantless form",
			in:   "secret://sandbox/mrc_01JB8Z11111111111111111111/stripe",
			want: Reference{
				Environment: shared.EnvironmentSandbox,
				MerchantID:  "mrc_01JB8Z11111111111111111111",
				GatewayID:   "stripe",
			},
		},
		{
			name: "the onboarding workflow's signing-secret form",
			in:   "secret://sandbox/mrc_01JB8Z11111111111111111111/stripe/webhook-signing",
			want: Reference{
				Environment: shared.EnvironmentSandbox,
				MerchantID:  "mrc_01JB8Z11111111111111111111",
				GatewayID:   "stripe", Purpose: "webhook-signing",
			},
		},
		{
			name: "a staging label as the version fragment",
			in:   "secret://sandbox/mrc_01JB8Z11111111111111111111/stripe#AWSPENDING",
			want: Reference{
				Environment: shared.EnvironmentSandbox,
				MerchantID:  "mrc_01JB8Z11111111111111111111",
				GatewayID:   "stripe", Version: StagePending,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseReference(tc.in)
			if err != nil {
				t.Fatalf("ParseReference(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseReference(%q)\n got %+v\nwant %+v", tc.in, got, tc.want)
			}
			if round := got.String(); round != tc.in {
				t.Errorf("round trip changed the reference: %q -> %q", tc.in, round)
			}
		})
	}
}

// TestParseReferenceRejectsTraversalAndWildcards is the security half. Each input here is a
// reference that, if accepted, would resolve outside the IAM path prefix the policy grants — or,
// worse, name a set of secrets rather than one.
func TestParseReferenceRejectsTraversalAndWildcards(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, in string }{
		{"empty", ""},
		{"not a secret uri", "https://example.com/secret"},
		{"bare material", "sk_live_not_a_reference"},
		{"empty path", "secret://"},
		{"too few segments", "secret://sandbox/stripe"},
		{"too many segments", "secret://sandbox/ten_a/mrc_b/stripe/api_key/extra"},
		{"parent traversal", "secret://sandbox/ten_a/../ten_b/stripe"},
		{"traversal in a segment", "secret://sandbox/ten_a/mrc_..b/stripe"},
		{"dot segment", "secret://sandbox/ten_a/./stripe"},
		{"star wildcard", "secret://sandbox/ten_a/*/stripe"},
		{"question wildcard", "secret://sandbox/ten_a/mrc_?/stripe"},
		{"bracket glob", "secret://sandbox/ten_a/mrc_[ab]/stripe"},
		{"empty segment", "secret://sandbox//mrc_b/stripe"},
		{"whitespace", "secret://sandbox/ten a/mrc_b/stripe"},
		{"unknown environment", "secret://staging/ten_a/mrc_b/stripe"},
		{"malformed version", "secret://sandbox/ten_a/mrc_b/stripe#latest"},
		{"empty version", "secret://sandbox/ten_a/mrc_b/stripe#"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := ParseReference(tc.in); err == nil {
				t.Fatalf("ParseReference(%q) was accepted as %+v", tc.in, got)
			}
		})
	}
}

// TestReferenceValidateStopsCrossTenantResolution is the control that matters most in this file.
// A merchant configuration is tenant-supplied data; `secretRef` is a field on it. Without this
// check a configuration naming another tenant's path would be resolved by a process whose IAM
// role can read it.
func TestReferenceValidateStopsCrossTenantResolution(t *testing.T) {
	t.Parallel()
	const (
		tenantA = "ten_01JB8Z00000000000000000000"
		tenantB = "ten_01JB8Z11111111111111111111"
	)
	ref, err := ParseReference("secret://production/" + tenantB + "/mrc_01JB8Z22222222222222222222/stripe/api_key")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	if err := ref.Validate(shared.EnvironmentProduction, shared.TenantID(tenantB)); err != nil {
		t.Fatalf("the owning tenant was refused: %v", err)
	}
	err = ref.Validate(shared.EnvironmentProduction, shared.TenantID(tenantA))
	if err == nil {
		t.Fatal("a reference naming another tenant was accepted")
	}
	if apierror.CodeOf(err) != apierror.CodeTenantMismatch {
		t.Errorf("code = %s, want TENANT_MISMATCH", apierror.CodeOf(err))
	}
	// The refusal must not disclose which tenant owns the path: a probe that learns the owner has
	// learned something, one that learns only "not yours" has not.
	if msg := err.Error(); containsAny(msg, tenantA, tenantB) {
		t.Errorf("the cross-tenant refusal names a tenant: %s", msg)
	}

	if err := ref.Validate(shared.EnvironmentProduction, ""); err == nil {
		t.Error("a tenant-scoped reference resolved without a tenant context")
	}
}

// TestReferenceValidateStopsEnvironmentCrossing pins docs/control-plane.md §5.2's structural
// guarantee: the classic "sandbox key in production" incident is impossible because the resolver
// refuses, not because deployment happens to keep them apart.
func TestReferenceValidateStopsEnvironmentCrossing(t *testing.T) {
	t.Parallel()
	ref, err := ParseReference("secret://sandbox/ten_01JB8Z00000000000000000000/mrc_01JB8Z11111111111111111111/stripe")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if err := ref.Validate(shared.EnvironmentProduction, "ten_01JB8Z00000000000000000000"); err == nil {
		t.Fatal("a sandbox reference resolved in a production process")
	}
	if err := ref.Validate(shared.EnvironmentSandbox, "ten_01JB8Z00000000000000000000"); err != nil {
		t.Fatalf("a sandbox reference was refused in a sandbox process: %v", err)
	}
}

// TestSecretIDMirrorsTheIAMPath: the secret's name in the store is the IAM path, which is what
// makes a prefix-scoped policy grant exactly one merchant's credentials. Changing the rendering
// silently widens or breaks every deployed policy, so it is pinned.
func TestSecretIDMirrorsTheIAMPath(t *testing.T) {
	t.Parallel()
	ref, err := ParseReference("secret://production/ten_a1/mrc_b2/stripe/api_key#v3")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if got, want := ref.SecretID(), "/production/ten_a1/mrc_b2/stripe/api_key"; got != want {
		t.Errorf("SecretID() = %q, want %q", got, want)
	}
	if got := ref.Base().Version; got != "" {
		t.Errorf("Base() kept a version pin: %q", got)
	}
	if got, want := ref.Base().WithVersion("v9").String(), "secret://production/ten_a1/mrc_b2/stripe/api_key#v9"; got != want {
		t.Errorf("WithVersion round trip = %q, want %q", got, want)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && len(sub) > 0 && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
