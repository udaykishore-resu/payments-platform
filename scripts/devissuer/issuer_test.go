package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authn"
)

// TestMintedTokenPassesTheRealValidator is the reason this tool can be trusted: it drives the
// platform's own authn.Validator against this issuer's JWKS endpoint and one of its minted tokens.
//
// A dev issuer that produces tokens the platform rejects is worse than no dev issuer at all,
// because the failure presents as a platform bug three layers away from its cause.
func TestMintedTokenPassesTheRealValidator(t *testing.T) {
	const audience = "payments-platform-dev"
	const tenant = "ten_01JB8Z00000000000000000000"

	key, kid, err := loadOrGenerateKey("")
	if err != nil {
		t.Fatalf("generating the signing key: %v", err)
	}

	var s *server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.jwks(w, r)
	}))
	defer srv.Close()

	issuer := srv.URL + "/"
	jwksURL := srv.URL + "/.well-known/jwks.json"
	opt := options{
		issuer:   issuer,
		audience: audience,
		env:      string(shared.EnvironmentSandbox),
		tenant:   tenant,
		tier:     string(shared.TierPooled),
		subject:  "cli_local_dev",
		scopes:   defaultScopes,
		ttl:      15 * time.Minute,
	}
	s = &server{key: key, kid: kid, opt: opt}

	keys := authn.NewJWKS(authn.NewHTTPFetcher(), authn.JWKSConfig{})
	v, err := authn.NewValidator(authn.ValidatorConfig{
		Issuers: []authn.Issuer{{
			Name: issuer, JWKSURL: jwksURL, ExpectedAudience: audience,
			// The same two scopes runtime.NewAuthenticator binds, so this test would catch a
			// default scope set that cannot be presented on a bearer token.
			MTLSBoundScopes: []string{"payments:refund", "credentials:rotate"},
		}},
		Environment: shared.EnvironmentSandbox,
		Keys:        keys,
		MaxTokenAge: time.Hour,
	})
	if err != nil {
		t.Fatalf("building the validator: %v", err)
	}
	// Refresh AFTER NewValidator: the constructor registers each issuer, and Register resets that
	// issuer's cached key set. runtime.NewAuthenticator has the same ordering (construct, then
	// Start warms the cache), and getting it backwards produces UNKNOWN_KEY on every token.
	if err := keys.Refresh(context.Background(), issuer); err != nil {
		t.Fatalf("the platform could not fetch this issuer's JWKS: %v", err)
	}

	raw, err := mint(key, kid, opt, tenant, opt.scopes, opt.ttl)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}

	p, err := v.Validate(context.Background(), "Bearer "+raw)
	if err != nil {
		t.Fatalf("the platform's own validator rejected a token this issuer minted: %v", err)
	}
	if string(p.TenantID) != tenant {
		t.Fatalf("tenant_id = %q, want %q", p.TenantID, tenant)
	}
	if p.Environment != shared.EnvironmentSandbox {
		t.Fatalf("env = %q, want sandbox", p.Environment)
	}
	// tests/testenv documents these two as what an e2e token must carry.
	for _, want := range []string{"payments:write", "merchants:write"} {
		if !hasScope(p.Scopes, want) {
			t.Fatalf("scopes %v do not include %q", p.Scopes, want)
		}
	}
}

// TestDefaultScopesExcludeTheMTLSBoundOnes pins the reasoning in defaultScopes' comment.
//
// A bearer token carrying payments:refund or credentials:rotate is rejected with
// NOT_BOUND_TO_CLIENT — a 401 that reads as "bad token" rather than "this scope needs mTLS".
func TestDefaultScopesExcludeTheMTLSBoundOnes(t *testing.T) {
	for _, forbidden := range []string{"payments:refund", "credentials:rotate"} {
		if strings.Contains(defaultScopes, forbidden) {
			t.Fatalf("defaultScopes must not contain the mTLS-bound scope %q", forbidden)
		}
	}
}

func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}
