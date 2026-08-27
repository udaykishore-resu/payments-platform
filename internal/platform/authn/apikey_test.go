package authn_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/domain/tenant"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authn"
	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
)

type clientStore struct {
	clients map[shared.APIClientID]*tenant.APIClient
	err     error
}

func (s clientStore) GetAPIClient(_ context.Context, id shared.APIClientID) (*tenant.APIClient, error) {
	if s.err != nil {
		return nil, s.err
	}
	c, ok := s.clients[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return c, nil
}

type secretStore struct{ material map[string]string }

func (s secretStore) Resolve(_ context.Context, ref string) (secret.Secret[string], error) {
	v, ok := s.material[ref]
	if !ok {
		return secret.Secret[string]{}, errors.New("no such secret")
	}
	return secret.New(v), nil
}

func newClient(t *testing.T, clock shared.Clock, cidrs []string) *tenant.APIClient {
	t.Helper()
	c, err := tenant.NewAPIClient(tenant.NewAPIClientParams{
		TenantID:      testTenant,
		Name:          "checkout integration",
		Scopes:        []string{"payments:write", "payments:read"},
		AllowedCIDRs:  cidrs,
		CredentialRef: "secret://production/" + testTenant + "/mrc_x/platform/client_secret#v1",
	}, clock)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newAPIKeyHarness(t *testing.T, cidrs []string) (*authn.APIKeyValidator, *tenant.APIClient, *shared.FixedClock, secretStore) {
	t.Helper()
	clock := &shared.FixedClock{T: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)}
	client := newClient(t, clock, cidrs)
	secrets := secretStore{material: map[string]string{client.CredentialRef(): "s3cr3t-current"}}
	v, err := authn.NewAPIKeyValidator(authn.APIKeyConfig{
		Clients:     clientStore{clients: map[shared.APIClientID]*tenant.APIClient{client.ID(): client}},
		Secrets:     secrets,
		Clock:       clock,
		Environment: shared.EnvironmentProduction,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v, client, clock, secrets
}

func TestAPIKeyHappyPath(t *testing.T) {
	// Verifies: FR-04.
	t.Parallel()
	v, client, _, _ := newAPIKeyHarness(t, nil)

	p, err := v.Validate(context.Background(), authn.Credentials{
		ClientID:     client.ID(),
		ClientSecret: "s3cr3t-current",
		SourceIP:     netip.MustParseAddr("203.0.113.4"),
	})
	if err != nil {
		t.Fatalf("a correct credential must authenticate: %v (%s)", err, authn.ReasonOf(err))
	}
	switch {
	case p.Method != authn.MethodAPIKey:
		t.Fatalf("method = %q", p.Method)
	case p.Type != tenantctx.PrincipalMachine:
		t.Fatalf("type = %q", p.Type)
	case p.TenantID != testTenant:
		t.Fatalf("tenant = %q", p.TenantID)
	case !p.HasScope("payments:write"):
		t.Fatalf("scopes = %v", p.Scopes)
	case !p.HasRole(authn.RoleServicePaymentClient):
		t.Fatalf("roles = %v", p.Roles)
	}
	// The bridge to the isolation layer: the tenant comes from the credential and nowhere else.
	tc, err := p.TenantContext("req_1", "cor_1")
	if err != nil {
		t.Fatal(err)
	}
	if tc.TenantID != testTenant || tc.Source != tenantctx.SourceToken {
		t.Fatalf("tenant context = %+v", tc)
	}
}

// The rotation overlap is what makes a 90-day rotation policy achievable rather than aspirational.
func TestAPIKeyRotationOverlap(t *testing.T) {
	// Verifies: FR-08, NFR-31.
	t.Parallel()
	v, client, clock, secrets := newAPIKeyHarness(t, nil)
	oldRef := client.CredentialRef()

	newRef := "secret://production/" + testTenant + "/mrc_x/platform/client_secret#v2"
	secrets.material[newRef] = "s3cr3t-rotated"
	overlapUntil := clock.Now().Add(24 * time.Hour)
	if err := client.Rotate(newRef, overlapUntil, clock); err != nil {
		t.Fatal(err)
	}
	_ = oldRef

	creds := func(sec string) authn.Credentials {
		return authn.Credentials{ClientID: client.ID(), ClientSecret: sec, SourceIP: netip.MustParseAddr("203.0.113.4")}
	}

	// Inside the window both secrets work; that is the entire point.
	if _, err := v.Validate(context.Background(), creds("s3cr3t-rotated")); err != nil {
		t.Fatalf("the new secret must work immediately: %v (%s)", err, authn.ReasonOf(err))
	}
	if _, err := v.Validate(context.Background(), creds("s3cr3t-current")); err != nil {
		t.Fatalf("the previous secret must keep working during the overlap: %v (%s)", err, authn.ReasonOf(err))
	}

	// After the window the old one stops working on our schedule, whether or not the merchant
	// finished their rollout.
	clock.Advance(25 * time.Hour)
	if _, err := v.Validate(context.Background(), creds("s3cr3t-rotated")); err != nil {
		t.Fatalf("the new secret must still work after the overlap: %v", err)
	}
	_, err := v.Validate(context.Background(), creds("s3cr3t-current"))
	if got := reasonOf(t, err); got != authn.ReasonBadSecret {
		t.Fatalf("reason = %s, want BAD_SECRET", got)
	}
}

func TestAPIKeyRejections(t *testing.T) {
	t.Parallel()
	ip := netip.MustParseAddr("203.0.113.4")

	t.Run("wrong secret", func(t *testing.T) {
		t.Parallel()
		v, client, _, _ := newAPIKeyHarness(t, nil)
		_, err := v.Validate(context.Background(), authn.Credentials{
			ClientID: client.ID(), ClientSecret: "wrong", SourceIP: ip})
		if got := reasonOf(t, err); got != authn.ReasonBadSecret {
			t.Fatalf("reason = %s", got)
		}
	})

	t.Run("a secret that is a prefix of the real one", func(t *testing.T) {
		t.Parallel()
		v, client, _, _ := newAPIKeyHarness(t, nil)
		_, err := v.Validate(context.Background(), authn.Credentials{
			ClientID: client.ID(), ClientSecret: "s3cr3t-curren", SourceIP: ip})
		if got := reasonOf(t, err); got != authn.ReasonBadSecret {
			t.Fatalf("reason = %s", got)
		}
	})

	t.Run("empty secret", func(t *testing.T) {
		t.Parallel()
		v, client, _, _ := newAPIKeyHarness(t, nil)
		_, err := v.Validate(context.Background(), authn.Credentials{ClientID: client.ID(), SourceIP: ip})
		if got := reasonOf(t, err); got != authn.ReasonUnknownClient {
			t.Fatalf("reason = %s", got)
		}
	})

	t.Run("unknown client", func(t *testing.T) {
		t.Parallel()
		v, _, _, _ := newAPIKeyHarness(t, nil)
		_, err := v.Validate(context.Background(), authn.Credentials{
			ClientID: shared.APIClientID("cli_01J000000000000000000000Z"), ClientSecret: "x", SourceIP: ip})
		if got := reasonOf(t, err); got != authn.ReasonUnknownClient {
			t.Fatalf("reason = %s", got)
		}
	})

	t.Run("lookup failure is indistinguishable from an unknown client", func(t *testing.T) {
		t.Parallel()
		clock := &shared.FixedClock{T: time.Now().UTC()}
		v, err := authn.NewAPIKeyValidator(authn.APIKeyConfig{
			Clients:     clientStore{err: errors.New("database down")},
			Secrets:     secretStore{},
			Clock:       clock,
			Environment: shared.EnvironmentProduction,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, verr := v.Validate(context.Background(), authn.Credentials{
			ClientID: shared.APIClientID("cli_01J000000000000000000000Z"), ClientSecret: "x", SourceIP: ip})
		if got := reasonOf(t, verr); got != authn.ReasonUnknownClient {
			t.Fatalf("reason = %s; a lookup failure must not be an enumeration oracle", got)
		}
	})

	t.Run("disabled client", func(t *testing.T) {
		t.Parallel()
		v, client, clock, _ := newAPIKeyHarness(t, nil)
		if err := client.Disable("investigation", clock); err != nil {
			t.Fatal(err)
		}
		_, err := v.Validate(context.Background(), authn.Credentials{
			ClientID: client.ID(), ClientSecret: "s3cr3t-current", SourceIP: ip})
		if got := reasonOf(t, err); got != authn.ReasonClientNotActive {
			t.Fatalf("reason = %s", got)
		}
	})

	t.Run("revoked client", func(t *testing.T) {
		t.Parallel()
		v, client, clock, _ := newAPIKeyHarness(t, nil)
		if err := client.Revoke("compromised", clock); err != nil {
			t.Fatal(err)
		}
		_, err := v.Validate(context.Background(), authn.Credentials{
			ClientID: client.ID(), ClientSecret: "s3cr3t-current", SourceIP: ip})
		if got := reasonOf(t, err); got != authn.ReasonClientNotActive {
			t.Fatalf("reason = %s", got)
		}
	})

	// The network restriction is checked after the secret, so it cannot be used as an oracle by
	// a caller who does not hold a valid credential.
	t.Run("source address outside the allowlist", func(t *testing.T) {
		t.Parallel()
		v, client, _, _ := newAPIKeyHarness(t, []string{"198.51.100.0/24"})
		_, err := v.Validate(context.Background(), authn.Credentials{
			ClientID: client.ID(), ClientSecret: "s3cr3t-current", SourceIP: ip})
		if got := reasonOf(t, err); got != authn.ReasonSourceNotAllowed {
			t.Fatalf("reason = %s", got)
		}
		// The same address with a wrong secret must report the secret, not the address.
		_, err = v.Validate(context.Background(), authn.Credentials{
			ClientID: client.ID(), ClientSecret: "wrong", SourceIP: ip})
		if got := reasonOf(t, err); got != authn.ReasonBadSecret {
			t.Fatalf("reason = %s; the allowlist must not be reachable without a valid secret", got)
		}
	})

	t.Run("source address inside the allowlist", func(t *testing.T) {
		t.Parallel()
		v, client, _, _ := newAPIKeyHarness(t, []string{"203.0.113.0/24"})
		if _, err := v.Validate(context.Background(), authn.Credentials{
			ClientID: client.ID(), ClientSecret: "s3cr3t-current", SourceIP: ip}); err != nil {
			t.Fatalf("an allowlisted address must authenticate: %v (%s)", err, authn.ReasonOf(err))
		}
	})

	t.Run("unresolvable credential reference", func(t *testing.T) {
		t.Parallel()
		clock := &shared.FixedClock{T: time.Now().UTC()}
		client := newClient(t, clock, nil)
		v, err := authn.NewAPIKeyValidator(authn.APIKeyConfig{
			Clients:     clientStore{clients: map[shared.APIClientID]*tenant.APIClient{client.ID(): client}},
			Secrets:     secretStore{material: map[string]string{}},
			Clock:       clock,
			Environment: shared.EnvironmentProduction,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, verr := v.Validate(context.Background(), authn.Credentials{
			ClientID: client.ID(), ClientSecret: "s3cr3t-current", SourceIP: ip})
		if got := reasonOf(t, verr); got != authn.ReasonBadSecret {
			t.Fatalf("reason = %s; an unresolvable secret must never authenticate", got)
		}
	})
}

func TestNewAPIKeyValidatorRejectsUnsafeConfiguration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  authn.APIKeyConfig
	}{
		{"no client lookup", authn.APIKeyConfig{Secrets: secretStore{}, Environment: shared.EnvironmentProduction}},
		{"no secret resolver", authn.APIKeyConfig{Clients: clientStore{}, Environment: shared.EnvironmentProduction}},
		{"no environment", authn.APIKeyConfig{Clients: clientStore{}, Secrets: secretStore{}}},
		{"unknown environment", authn.APIKeyConfig{Clients: clientStore{}, Secrets: secretStore{}, Environment: "staging"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := authn.NewAPIKeyValidator(tc.cfg); err == nil {
				t.Fatal("an unsafe configuration must fail at construction")
			}
		})
	}
}

func TestPrincipalScopeWildcardExpandsOnlyInTheGrant(t *testing.T) {
	t.Parallel()
	p := &authn.Principal{Scopes: []string{"payments:*", "config:read"}}
	for _, s := range []string{"payments:read", "payments:write", "payments:refund", "config:read"} {
		if !p.HasScope(s) {
			t.Fatalf("%q must be granted", s)
		}
	}
	for _, s := range []string{"", "payments", "payments:", "config:write", "secrets:read", "payment:read"} {
		if p.HasScope(s) {
			t.Fatalf("%q must not be granted", s)
		}
	}
	// A requirement must never wildcard: asking "does this principal have any config scope"
	// must get a literal answer about the grant `config:*`, which it does not hold.
	if p.HasScope("config:*") {
		t.Fatal("a wildcard requirement must not be satisfied by a narrower grant")
	}
}

func TestPrincipalAccessorsCopy(t *testing.T) {
	t.Parallel()
	p := &authn.Principal{
		Scopes:        []string{"payments:read"},
		Roles:         []string{"svc:payment-client"},
		MerchantScope: []shared.MerchantID{"mrc_01J000000000000000000000A"},
	}
	p.AllScopes()[0] = "secrets:read"
	p.AllRoles()[0] = "platform-admin"
	p.Merchants()[0] = "mrc_other"
	if p.Scopes[0] != "payments:read" || p.Roles[0] != "svc:payment-client" || p.MerchantScope[0] != "mrc_01J000000000000000000000A" {
		t.Fatal("accessors must not hand out the live backing array")
	}
	var nilP *authn.Principal
	if nilP.HasScope("x") || nilP.HasRole("x") || nilP.AllScopes() != nil || nilP.Merchants() != nil {
		t.Fatal("a nil principal must be inert, not a panic")
	}
	if _, err := nilP.TenantContext("r", "c"); err == nil {
		t.Fatal("a nil principal must not produce a tenant context")
	}
}
