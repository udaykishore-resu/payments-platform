package authn_test

import (
	"context"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authn"
)

func TestJWKSResolvesAndRejects(t *testing.T) {
	t.Parallel()
	rk, ek, _ := keys(t)
	clock := &shared.FixedClock{T: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)}
	f := &staticFetcher{body: jwksDocument(rsaJWK("rsa-1", rk), ecJWK("ec-1", ek))}
	j := authn.NewJWKS(f, authn.JWKSConfig{Clock: clock})
	t.Cleanup(j.Stop)
	j.Register(testIssuer, testJWKSURL)

	// Before any fetch there is no key set, and the correct answer is "I do not know", not a
	// guess.
	if _, err := j.Key(context.Background(), testIssuer, "rsa-1"); err == nil {
		t.Fatal("a cache that has never fetched must not resolve a key")
	}
	if err := j.Refresh(context.Background(), testIssuer); err != nil {
		t.Fatal(err)
	}
	for _, kid := range []string{"rsa-1", "ec-1"} {
		k, err := j.Key(context.Background(), testIssuer, kid)
		if err != nil || k.ID != kid {
			t.Fatalf("Key(%q) = %v, %v", kid, k, err)
		}
	}
	if _, err := j.Key(context.Background(), testIssuer, "nope"); err == nil {
		t.Fatal("an unknown kid must not resolve")
	}
	// An unknown issuer never reaches the network: it is not in the allowlist at all.
	if _, err := j.Key(context.Background(), "https://attacker.example.com", "rsa-1"); !errors.Is(err, authn.ErrUnknownIssuer) {
		t.Fatalf("want ErrUnknownIssuer, got %v", err)
	}
	if err := j.Refresh(context.Background(), "https://attacker.example.com"); !errors.Is(err, authn.ErrUnknownIssuer) {
		t.Fatalf("refreshing an unknown issuer must not fetch: %v", err)
	}
	if f.lastURL != testJWKSURL {
		t.Fatalf("fetched %q, want only the configured URL", f.lastURL)
	}
}

// Stale-if-error is the property that keeps an identity provider's outage from becoming ours.
func TestJWKSStaleIfError(t *testing.T) {
	t.Parallel()
	rk, _, _ := keys(t)
	clock := &shared.FixedClock{T: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)}
	f := &staticFetcher{body: jwksDocument(rsaJWK("rsa-1", rk))}
	j := authn.NewJWKS(f, authn.JWKSConfig{
		Clock:              clock,
		StaleIfError:       time.Hour,
		MinRefreshInterval: time.Second,
	})
	t.Cleanup(j.Stop)
	j.Register(testIssuer, testJWKSURL)
	if err := j.Refresh(context.Background(), testIssuer); err != nil {
		t.Fatal(err)
	}

	// The issuer goes away.
	f.set(nil, errors.New("connection refused"))
	clock.Advance(10 * time.Minute)
	if err := j.Refresh(context.Background(), testIssuer); err == nil {
		t.Fatal("the refresh should have failed")
	}
	// Well past the TTL, well inside the stale-if-error window: keep serving.
	if _, err := j.Key(context.Background(), testIssuer, "rsa-1"); err != nil {
		t.Fatalf("stale-if-error must keep serving the last good key set: %v", err)
	}
	if age, ok := j.SnapshotAge(testIssuer); !ok || age != 10*time.Minute {
		t.Fatalf("SnapshotAge = %v, %v", age, ok)
	}

	// Past the bound, the set stops being evidence.
	clock.Advance(51 * time.Minute)
	if _, err := j.Key(context.Background(), testIssuer, "rsa-1"); err == nil {
		t.Fatal("beyond the stale-if-error bound the key set must be refused")
	}

	// And a successful refresh restores service without a restart.
	f.set(jwksDocument(rsaJWK("rsa-1", rk)), nil)
	if err := j.Refresh(context.Background(), testIssuer); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Key(context.Background(), testIssuer, "rsa-1"); err != nil {
		t.Fatalf("a recovered issuer must restore service: %v", err)
	}
}

// A burst of tokens carrying unknown `kid` values must not become an amplifier pointed at the
// identity provider.
func TestJWKSRefreshIsRateLimited(t *testing.T) {
	t.Parallel()
	rk, _, _ := keys(t)
	clock := &shared.FixedClock{T: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)}
	f := &staticFetcher{body: jwksDocument(rsaJWK("rsa-1", rk))}
	j := authn.NewJWKS(f, authn.JWKSConfig{Clock: clock, MinRefreshInterval: 30 * time.Second})
	t.Cleanup(j.Stop)
	j.Register(testIssuer, testJWKSURL)

	if err := j.Refresh(context.Background(), testIssuer); err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 {
		t.Fatalf("fetches = %d, want 1", f.count())
	}
	for i := 0; i < 100; i++ {
		if err := j.Refresh(context.Background(), testIssuer); !errors.Is(err, authn.ErrRateLimited) {
			t.Fatalf("refresh %d: want ErrRateLimited, got %v", i, err)
		}
	}
	if f.count() != 1 {
		t.Fatalf("100 refresh attempts inside the window produced %d fetches, want 1", f.count())
	}

	// A thousand unknown kids must produce no fetches at all: Key never fetches.
	for i := 0; i < 1000; i++ {
		_, _ = j.Key(context.Background(), testIssuer, "unknown-kid")
	}
	if f.count() != 1 {
		t.Fatalf("resolving unknown kids caused %d fetches; the request path must never fetch", f.count())
	}

	clock.Advance(31 * time.Second)
	if err := j.Refresh(context.Background(), testIssuer); err != nil {
		t.Fatalf("past the window a refresh must be allowed: %v", err)
	}
	if f.count() != 2 {
		t.Fatalf("fetches = %d, want 2", f.count())
	}

	// A *failing* refresh must also consume the budget, or a broken issuer is retried as fast
	// as requests arrive.
	f.set(nil, errors.New("boom"))
	clock.Advance(31 * time.Second)
	_ = j.Refresh(context.Background(), testIssuer)
	before := f.count()
	if err := j.Refresh(context.Background(), testIssuer); !errors.Is(err, authn.ErrRateLimited) {
		t.Fatalf("a failing issuer must still be rate-limited, got %v", err)
	}
	if f.count() != before {
		t.Fatal("a rate-limited refresh must not fetch")
	}
}

// Key rotation publishes two keys for thirty days. Both must verify, or every rotation is an
// outage for whichever half of the traffic holds the other key.
func TestJWKSBothRotationKeysValidate(t *testing.T) {
	t.Parallel()
	rk, _, altRSA := keys(t)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	clock := &shared.FixedClock{T: now}
	f := &staticFetcher{body: jwksDocument(rsaJWK("old", rk), rsaJWK("new", altRSA))}
	j := authn.NewJWKS(f, authn.JWKSConfig{Clock: clock})
	t.Cleanup(j.Stop)

	v, err := authn.NewValidator(authn.ValidatorConfig{
		Issuers:     []authn.Issuer{{Name: testIssuer, JWKSURL: testJWKSURL, ExpectedAudience: testAudience}},
		Environment: shared.EnvironmentProduction,
		Keys:        j,
		Clock:       clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Refresh(context.Background(), testIssuer); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		kid string
		key *rsa.PrivateKey
	}{{"old", rk}, {"new", altRSA}} {
		c := baseClaims(now)
		c["jti"] = "jti-" + tc.kid
		tok := assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": tc.kid}, c, signRS256(tc.key))
		if _, err := v.Validate(context.Background(), tok); err != nil {
			t.Fatalf("token signed with the %q key must validate during the overlap: %v (%s)",
				tc.kid, err, authn.ReasonOf(err))
		}
	}

	// After the old key is retired from the document, tokens signed with it stop validating —
	// which is the point of retiring it.
	clock.Advance(time.Hour)
	f.set(jwksDocument(rsaJWK("new", altRSA)), nil)
	if err := j.Refresh(context.Background(), testIssuer); err != nil {
		t.Fatal(err)
	}
	c := baseClaims(clock.Now())
	c["jti"] = "jti-after"
	tok := assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "old"}, c, signRS256(rk))
	if _, err := v.Validate(context.Background(), tok); authn.ReasonOf(err) != authn.ReasonUnknownKey {
		t.Fatalf("a retired key must stop resolving, got %v", authn.ReasonOf(err))
	}
}

func TestJWKSDocumentBounds(t *testing.T) {
	t.Parallel()
	rk, ek, alt := keys(t)
	clock := &shared.FixedClock{T: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)}

	t.Run("too many keys", func(t *testing.T) {
		t.Parallel()
		entries := make([]map[string]any, 0, 12)
		for i := 0; i < 12; i++ {
			entries = append(entries, rsaJWK("k", rk))
		}
		j := authn.NewJWKS(&staticFetcher{body: jwksDocument(entries...)}, authn.JWKSConfig{Clock: clock, MaxKeys: 10})
		t.Cleanup(j.Stop)
		j.Register(testIssuer, testJWKSURL)
		if err := j.Refresh(context.Background(), testIssuer); err == nil {
			t.Fatal("an over-large key set must be refused")
		}
	})

	t.Run("oversized document", func(t *testing.T) {
		t.Parallel()
		j := authn.NewJWKS(&staticFetcher{body: make([]byte, 128<<10)}, authn.JWKSConfig{Clock: clock, MaxBytes: 64 << 10})
		t.Cleanup(j.Stop)
		j.Register(testIssuer, testJWKSURL)
		if err := j.Refresh(context.Background(), testIssuer); err == nil {
			t.Fatal("an oversized document must be refused")
		}
	})

	t.Run("keys we cannot use are skipped, not fatal", func(t *testing.T) {
		t.Parallel()
		doc := jwksDocument(
			map[string]any{"kty": "oct", "kid": "sym", "alg": "HS256", "k": "c2VjcmV0"}, // symmetric: never usable here
			map[string]any{"kty": "RSA", "kid": "enc", "alg": "RS256", "use": "enc"},    // encryption key
			map[string]any{"kty": "RSA", "alg": "RS256"},                                // no kid
			rsaJWK("good", rk),
		)
		j := authn.NewJWKS(&staticFetcher{body: doc}, authn.JWKSConfig{Clock: clock})
		t.Cleanup(j.Stop)
		j.Register(testIssuer, testJWKSURL)
		if err := j.Refresh(context.Background(), testIssuer); err != nil {
			t.Fatalf("an issuer publishing keys for other purposes must not break us: %v", err)
		}
		if _, err := j.Key(context.Background(), testIssuer, "good"); err != nil {
			t.Fatalf("the usable key must resolve: %v", err)
		}
		for _, kid := range []string{"sym", "enc"} {
			if _, err := j.Key(context.Background(), testIssuer, kid); err == nil {
				t.Fatalf("kid %q must not be usable for signature verification", kid)
			}
		}
	})

	t.Run("a document with nothing usable is an error", func(t *testing.T) {
		t.Parallel()
		doc := jwksDocument(map[string]any{"kty": "oct", "kid": "sym", "alg": "HS256"})
		j := authn.NewJWKS(&staticFetcher{body: doc}, authn.JWKSConfig{Clock: clock})
		t.Cleanup(j.Stop)
		j.Register(testIssuer, testJWKSURL)
		if err := j.Refresh(context.Background(), testIssuer); err == nil {
			t.Fatal("a document with no signing key must not replace a good set")
		}
	})

	t.Run("an off-curve EC point is refused", func(t *testing.T) {
		t.Parallel()
		bad := ecJWK("ec-bad", ek)
		bad["y"] = ecJWK("ec-ok", ek)["x"] // deliberately not the matching coordinate
		j := authn.NewJWKS(&staticFetcher{body: jwksDocument(bad, rsaJWK("ok", alt))}, authn.JWKSConfig{Clock: clock})
		t.Cleanup(j.Stop)
		j.Register(testIssuer, testJWKSURL)
		if err := j.Refresh(context.Background(), testIssuer); err != nil {
			t.Fatal(err)
		}
		if _, err := j.Key(context.Background(), testIssuer, "ec-bad"); err == nil {
			t.Fatal("a point that is not on P-256 must never become a verification key")
		}
	})
}

// The background refresher must be bounded and must stop cleanly; -race and the leak check in
// TestMain would otherwise be the only thing catching a stray goroutine.
func TestJWKSBackgroundRefreshIsBoundedAndStoppable(t *testing.T) {
	t.Parallel()
	rk, _, _ := keys(t)
	f := &staticFetcher{body: jwksDocument(rsaJWK("rsa-1", rk))}
	j := authn.NewJWKS(f, authn.JWKSConfig{
		Clock:              shared.SystemClock{},
		RefreshInterval:    5 * time.Millisecond,
		MinRefreshInterval: time.Nanosecond,
	})
	j.Register(testIssuer, testJWKSURL)
	j.Start(context.Background())
	// Start twice: it must be idempotent, not a second goroutine.
	j.Start(context.Background())

	select {
	case <-j.Refreshed():
	case <-time.After(2 * time.Second):
		t.Fatal("the background refresher never ran")
	}
	if _, err := j.Key(context.Background(), testIssuer, "rsa-1"); err != nil {
		t.Fatalf("the background refresh should have warmed the cache: %v", err)
	}
	j.Stop()
	j.Stop() // idempotent

	// Start after Stop must not resurrect the goroutine.
	j.Start(context.Background())
	j.Stop()
}

func TestJWKSStopWithoutStart(t *testing.T) {
	t.Parallel()
	j := authn.NewJWKS(&staticFetcher{}, authn.JWKSConfig{})
	j.Stop()
	j.Stop()
}
