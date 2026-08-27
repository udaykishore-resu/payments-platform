package authn_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authn"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// --- fixtures ------------------------------------------------------------------------------

const (
	testIssuer   = "https://idp.example.com"
	testAudience = "https://api.example.com"
	testJWKSURL  = "https://idp.example.com/.well-known/jwks.json"
	testTenant   = "ten_01J0000000000000000000000A"
	testSubject  = "cli_01J000000000000000000000A"
)

var (
	rsaKeyOnce sync.Once
	rsaKey     *rsa.PrivateKey
	ecKey      *ecdsa.PrivateKey
	rsaKeyAlt  *rsa.PrivateKey
)

// keys are generated once for the whole package: 2048-bit RSA generation is slow enough that
// doing it per subtest would dominate the run.
func keys(t *testing.T) (*rsa.PrivateKey, *ecdsa.PrivateKey, *rsa.PrivateKey) {
	t.Helper()
	rsaKeyOnce.Do(func() {
		var err error
		if rsaKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
		if rsaKeyAlt, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
		if ecKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			panic(err)
		}
	})
	return rsaKey, ecKey, rsaKeyAlt
}

// staticFetcher serves a fixed document and counts fetches, so the rate limit is observable.
type staticFetcher struct {
	mu      sync.Mutex
	body    []byte
	err     error
	fetches int
	lastURL string
}

func (f *staticFetcher) Fetch(_ context.Context, url string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches++
	f.lastURL = url
	if f.err != nil {
		return nil, f.err
	}
	return f.body, nil
}

func (f *staticFetcher) set(body []byte, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body, f.err = body, err
}

func (f *staticFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches
}

func jwksDocument(entries ...map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"keys": entries})
	return b
}

func rsaJWK(kid string, k *rsa.PrivateKey) map[string]any {
	return map[string]any{
		"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
	}
}

func ecJWK(kid string, k *ecdsa.PrivateKey) map[string]any {
	return map[string]any{
		"kty": "EC", "kid": kid, "alg": "ES256", "use": "sig", "crv": "P-256",
		"x": base64.RawURLEncoding.EncodeToString(k.X.FillBytes(make([]byte, 32))),
		"y": base64.RawURLEncoding.EncodeToString(k.Y.FillBytes(make([]byte, 32))),
	}
}

// --- token construction ---------------------------------------------------------------------

// Tokens are assembled by hand rather than with the library's signer, because most of the
// interesting cases are tokens no conforming signer would produce: alg none, a rewritten
// algorithm header, an embedded key.
func segment(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func assemble(header, claims map[string]any, sign func(input string) string) string {
	input := segment(header) + "." + segment(claims)
	return input + "." + sign(input)
}

func signRS256(k *rsa.PrivateKey) func(string) string {
	return func(input string) string {
		sum := sha256.Sum256([]byte(input))
		sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
		if err != nil {
			panic(err)
		}
		return base64.RawURLEncoding.EncodeToString(sig)
	}
}

func signES256(k *ecdsa.PrivateKey) func(string) string {
	return func(input string) string {
		sum := sha256.Sum256([]byte(input))
		r, s, err := ecdsa.Sign(rand.Reader, k, sum[:])
		if err != nil {
			panic(err)
		}
		out := make([]byte, 64)
		r.FillBytes(out[:32])
		s.FillBytes(out[32:])
		return base64.RawURLEncoding.EncodeToString(out)
	}
}

func signHS256(secret []byte) func(string) string {
	return func(input string) string {
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(input))
		return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
}

func signNone() func(string) string { return func(string) string { return "" } }

func baseClaims(now time.Time) map[string]any {
	return map[string]any{
		"iss":         testIssuer,
		"aud":         testAudience,
		"sub":         testSubject,
		"jti":         "jti-1",
		"iat":         now.Unix(),
		"nbf":         now.Add(-time.Second).Unix(),
		"exp":         now.Add(15 * time.Minute).Unix(),
		"tenant_id":   testTenant,
		"tenant_tier": "POOLED",
		"scope":       "payments:write payments:read",
		"env":         "production",
		"roles":       []string{"svc:payment-client"},
	}
}

// --- harness --------------------------------------------------------------------------------

type replayMemory struct {
	mu   sync.Mutex
	seen map[string]bool
	err  error
}

func newReplayMemory() *replayMemory { return &replayMemory{seen: map[string]bool{}} }

func (r *replayMemory) SeenBefore(_ context.Context, issuer, jti string, _ time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return false, r.err
	}
	k := issuer + "|" + jti
	was := r.seen[k]
	r.seen[k] = true
	return was, nil
}

type revocationList struct{ revoked map[string]bool }

func (r revocationList) IsRevoked(_ context.Context, _, jti, subject string, _ shared.TenantID) bool {
	return r.revoked[jti] || r.revoked[subject]
}

type recordingObserver struct {
	mu       sync.Mutex
	failures []authn.Reason
	degraded int
}

func (o *recordingObserver) AuthenticationFailed(_ authn.Method, r authn.Reason) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failures = append(o.failures, r)
}

func (o *recordingObserver) ReplayCheckDegraded() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.degraded++
}

func (o *recordingObserver) degradedCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.degraded
}

type harness struct {
	validator *authn.Validator
	jwks      *authn.JWKS
	fetcher   *staticFetcher
	clock     *shared.FixedClock
	replay    *replayMemory
	observer  *recordingObserver
	now       time.Time
}

func newHarness(t *testing.T, mutate func(*authn.ValidatorConfig)) *harness {
	t.Helper()
	rk, ek, _ := keys(t)
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	clock := &shared.FixedClock{T: now}
	fetcher := &staticFetcher{body: jwksDocument(rsaJWK("rsa-1", rk), ecJWK("ec-1", ek))}
	jwks := authn.NewJWKS(fetcher, authn.JWKSConfig{Clock: clock})

	cfg := authn.ValidatorConfig{
		Issuers: []authn.Issuer{{
			Name:             testIssuer,
			JWKSURL:          testJWKSURL,
			ExpectedAudience: testAudience,
			MTLSBoundScopes:  []string{"payments:refund"},
		}},
		Environment: shared.EnvironmentProduction,
		Keys:        jwks,
		Clock:       clock,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	v, err := authn.NewValidator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := jwks.Refresh(context.Background(), testIssuer); err != nil {
		t.Fatalf("warming the key cache: %v", err)
	}
	t.Cleanup(jwks.Stop)

	h := &harness{validator: v, jwks: jwks, fetcher: fetcher, clock: clock, now: now}
	if r, ok := cfg.Replay.(*replayMemory); ok {
		h.replay = r
	}
	if o, ok := cfg.Observer.(*recordingObserver); ok {
		h.observer = o
	}
	return h
}

func reasonOf(t *testing.T, err error) authn.Reason {
	t.Helper()
	if err == nil {
		t.Fatal("expected a rejection, got none")
	}
	// Every failure must present the same uniform 401 to the caller.
	var e *apierror.Error
	if !errors.As(err, &e) {
		t.Fatalf("authentication failures must be platform errors, got %T", err)
	}
	if e.Code != apierror.CodeInvalidToken {
		t.Fatalf("code = %s, want INVALID_TOKEN (a uniform response is the point)", e.Code)
	}
	if e.HTTPStatus() != 401 {
		t.Fatalf("status = %d, want 401", e.HTTPStatus())
	}
	if e.Message != "the access token is invalid" {
		t.Fatalf("message = %q; every failure must render the same body", e.Message)
	}
	return authn.ReasonOf(err)
}

// --- the attack matrix ---------------------------------------------------------------------

func TestValidateHappyPath(t *testing.T) {
	// Verifies: FR-04.
	t.Parallel()
	rk, _, _ := keys(t)
	h := newHarness(t, nil)
	tok := assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
		baseClaims(h.now), signRS256(rk))

	p, err := h.validator.Validate(context.Background(), tok)
	if err != nil {
		t.Fatalf("a well-formed token must validate: %v (%s)", err, authn.ReasonOf(err))
	}
	switch {
	case p.TenantID != testTenant:
		t.Fatalf("tenant = %q", p.TenantID)
	case p.ID != testSubject:
		t.Fatalf("subject = %q", p.ID)
	case !p.HasScope("payments:write"):
		t.Fatalf("scopes = %v", p.Scopes)
	case p.Environment != shared.EnvironmentProduction:
		t.Fatalf("environment = %q", p.Environment)
	case p.Method != authn.MethodJWT:
		t.Fatalf("method = %q", p.Method)
	case p.TokenID != "jti-1":
		t.Fatalf("jti = %q", p.TokenID)
	}
	// A Bearer prefix is accepted, because that is how it arrives.
	if _, err := h.validator.Validate(context.Background(), "Bearer "+tok); err != nil {
		t.Fatalf("a Bearer-prefixed token must validate: %v", err)
	}
}

func TestES256TokenValidates(t *testing.T) {
	t.Parallel()
	_, ek, _ := keys(t)
	h := newHarness(t, nil)
	tok := assemble(map[string]any{"alg": "ES256", "typ": "JWT", "kid": "ec-1"},
		baseClaims(h.now), signES256(ek))
	if _, err := h.validator.Validate(context.Background(), tok); err != nil {
		t.Fatalf("ES256 is on the allowlist and must validate: %v (%s)", err, authn.ReasonOf(err))
	}
}

// TestAttackMatrix is the negative core of this package: every rejection rule in
// security.md §3.3, each asserted to produce the uniform 401 and the correct internal reason.
func TestAttackMatrix(t *testing.T) {
	// Verifies: FR-04, NFR-30.
	t.Parallel()
	rk, ek, altRSA := keys(t)

	cases := []struct {
		name  string
		build func(h *harness) string
		want  authn.Reason
	}{
		{
			// The oldest JWT attack: strip the signature and declare there was never one.
			name: "alg none",
			build: func(h *harness) string {
				return assemble(map[string]any{"alg": "none", "typ": "JWT", "kid": "rsa-1"},
					baseClaims(h.now), signNone())
			},
			want: authn.ReasonAlgNotAllowed,
		},
		{
			// alg none with the `kid` removed too, in case the kid check were what saved us.
			name: "alg none without kid",
			build: func(h *harness) string {
				return assemble(map[string]any{"alg": "none", "typ": "JWT"},
					baseClaims(h.now), signNone())
			},
			want: authn.ReasonAlgNotAllowed,
		},
		{
			// RS256 → HS256 key confusion, in its exact classic form: the attacker takes the
			// issuer's *public* key — which is published in the JWKS document — and uses it as
			// an HMAC shared secret. A verifier that trusts the header's algorithm and looks up
			// "the key for kid rsa-1" would compute the same MAC and accept.
			name: "RS256 to HS256 confusion with the public key as the HMAC secret",
			build: func(h *harness) string {
				pubDER, err := x509.MarshalPKIXPublicKey(&rk.PublicKey)
				if err != nil {
					t.Fatal(err)
				}
				return assemble(map[string]any{"alg": "HS256", "typ": "JWT", "kid": "rsa-1"},
					baseClaims(h.now), signHS256(pubDER))
			},
			want: authn.ReasonAlgNotAllowed,
		},
		{
			name: "HS512 confusion",
			build: func(h *harness) string {
				return assemble(map[string]any{"alg": "HS512", "typ": "JWT", "kid": "rsa-1"},
					baseClaims(h.now), signHS256([]byte("anything")))
			},
			want: authn.ReasonAlgNotAllowed,
		},
		{
			name: "unsupported asymmetric algorithm",
			build: func(h *harness) string {
				return assemble(map[string]any{"alg": "RS512", "typ": "JWT", "kid": "rsa-1"},
					baseClaims(h.now), signRS256(rk))
			},
			want: authn.ReasonAlgNotAllowed,
		},
		{
			// The token nominates its own verification key. Accepting it makes signature
			// verification a tautology: of course it verifies, the attacker chose the key.
			name: "embedded jwk header",
			build: func(h *harness) string {
				return assemble(map[string]any{
					"alg": "RS256", "typ": "JWT", "kid": "rsa-1",
					"jwk": rsaJWK("rsa-1", altRSA),
				}, baseClaims(h.now), signRS256(altRSA))
			},
			want: authn.ReasonEmbeddedKey,
		},
		{
			// Same attack, one indirection further: fetch the key from a URL the attacker owns.
			// Also an SSRF primitive.
			name: "jku header",
			build: func(h *harness) string {
				return assemble(map[string]any{
					"alg": "RS256", "typ": "JWT", "kid": "rsa-1",
					"jku": "https://attacker.example.com/jwks.json",
				}, baseClaims(h.now), signRS256(rk))
			},
			want: authn.ReasonEmbeddedKey,
		},
		{
			name: "x5u header",
			build: func(h *harness) string {
				return assemble(map[string]any{
					"alg": "RS256", "typ": "JWT", "kid": "rsa-1",
					"x5u": "https://attacker.example.com/cert.pem",
				}, baseClaims(h.now), signRS256(rk))
			},
			want: authn.ReasonEmbeddedKey,
		},
		{
			name: "x5c header",
			build: func(h *harness) string {
				return assemble(map[string]any{
					"alg": "RS256", "typ": "JWT", "kid": "rsa-1",
					"x5c": []string{"MIIB..."},
				}, baseClaims(h.now), signRS256(rk))
			},
			want: authn.ReasonEmbeddedKey,
		},
		{
			name: "missing kid",
			build: func(h *harness) string {
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT"},
					baseClaims(h.now), signRS256(rk))
			},
			want: authn.ReasonMissingKID,
		},
		{
			// An unknown issuer must be rejected before any key lookup, so that an
			// attacker-chosen `iss` can never drive an outbound request.
			name: "wrong issuer",
			build: func(h *harness) string {
				c := baseClaims(h.now)
				c["iss"] = "https://attacker.example.com"
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
					c, signRS256(altRSA))
			},
			want: authn.ReasonUntrustedIssuer,
		},
		{
			name: "unknown kid",
			build: func(h *harness) string {
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-99"},
					baseClaims(h.now), signRS256(rk))
			},
			want: authn.ReasonUnknownKey,
		},
		{
			// The key is published as ES256; presenting it as an RS256 verification key is a
			// second route to algorithm confusion.
			name: "algorithm does not match the key's declared alg",
			build: func(h *harness) string {
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "ec-1"},
					baseClaims(h.now), signRS256(rk))
			},
			want: authn.ReasonAlgKeyMismatch,
		},
		{
			name: "signature from the wrong private key",
			build: func(h *harness) string {
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
					baseClaims(h.now), signRS256(altRSA))
			},
			want: authn.ReasonBadSignature,
		},
		{
			name: "tampered payload",
			build: func(h *harness) string {
				tok := assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
					baseClaims(h.now), signRS256(rk))
				sig := tok[strings.LastIndex(tok, "."):]
				c := baseClaims(h.now)
				c["scope"] = "secrets:read"
				header := segment(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"})
				return header + "." + segment(c) + sig
			},
			want: authn.ReasonBadSignature,
		},
		{
			// A token minted for another service must not be good here. This is the bug that a
			// `strings.Contains` audience check produces.
			name: "wrong audience",
			build: func(h *harness) string {
				c := baseClaims(h.now)
				c["aud"] = "https://internal.example.com"
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
					c, signRS256(rk))
			},
			want: authn.ReasonBadAudience,
		},
		{
			name: "audience merely contains ours",
			build: func(h *harness) string {
				c := baseClaims(h.now)
				c["aud"] = []string{"https://other.example.com", testAudience}
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
					c, signRS256(rk))
			},
			want: authn.ReasonBadAudience,
		},
		{
			name: "missing audience",
			build: func(h *harness) string {
				c := baseClaims(h.now)
				delete(c, "aud")
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
					c, signRS256(rk))
			},
			want: authn.ReasonBadAudience,
		},
		{
			name: "expired beyond the skew tolerance",
			build: func(h *harness) string {
				c := baseClaims(h.now)
				c["iat"] = h.now.Add(-10 * time.Minute).Unix()
				c["exp"] = h.now.Add(-2 * time.Minute).Unix()
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
					c, signRS256(rk))
			},
			want: authn.ReasonExpired,
		},
		{
			name: "no expiry at all",
			build: func(h *harness) string {
				c := baseClaims(h.now)
				delete(c, "exp")
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
					c, signRS256(rk))
			},
			want: authn.ReasonExpired,
		},
		{
			name: "not yet valid",
			build: func(h *harness) string {
				c := baseClaims(h.now)
				c["nbf"] = h.now.Add(5 * time.Minute).Unix()
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
					c, signRS256(rk))
			},
			want: authn.ReasonNotYetValid,
		},
		{
			// The independent max-age backstop: an issuer misconfigured to mint long-lived
			// tokens does not get to extend our exposure.
			name: "issued too long ago even though exp is in the future",
			build: func(h *harness) string {
				c := baseClaims(h.now)
				c["iat"] = h.now.Add(-8 * time.Hour).Unix()
				c["exp"] = h.now.Add(8 * time.Hour).Unix()
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
					c, signRS256(rk))
			},
			want: authn.ReasonStale,
		},
		{
			name: "no issued-at",
			build: func(h *harness) string {
				c := baseClaims(h.now)
				delete(c, "iat")
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
					c, signRS256(rk))
			},
			want: authn.ReasonStale,
		},
		{
			name: "issued in the future",
			build: func(h *harness) string {
				c := baseClaims(h.now)
				c["iat"] = h.now.Add(10 * time.Minute).Unix()
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
					c, signRS256(rk))
			},
			want: authn.ReasonIssuedInFuture,
		},
		{
			name: "missing jti",
			build: func(h *harness) string {
				c := baseClaims(h.now)
				delete(c, "jti")
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
					c, signRS256(rk))
			},
			want: authn.ReasonMissingJTI,
		},
		{
			name: "missing tenant claim",
			build: func(h *harness) string {
				c := baseClaims(h.now)
				delete(c, "tenant_id")
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
					c, signRS256(rk))
			},
			want: authn.ReasonMissingTenant,
		},
		{
			name: "tenant claim is not a tenant identifier",
			build: func(h *harness) string {
				c := baseClaims(h.now)
				c["tenant_id"] = "mrc_01J000000000000000000000A"
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
					c, signRS256(rk))
			},
			want: authn.ReasonMissingTenant,
		},
		{
			// A sandbox credential must never authenticate against production.
			name: "sandbox token in production",
			build: func(h *harness) string {
				c := baseClaims(h.now)
				c["env"] = "sandbox"
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
					c, signRS256(rk))
			},
			want: authn.ReasonEnvMismatch,
		},
		{
			name: "missing environment claim",
			build: func(h *harness) string {
				c := baseClaims(h.now)
				delete(c, "env")
				return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
					c, signRS256(rk))
			},
			want: authn.ReasonEnvMismatch,
		},
		{
			name:  "not a jwt at all",
			build: func(*harness) string { return "not.a.token" },
			want:  authn.ReasonMalformed,
		},
		{
			name:  "empty",
			build: func(*harness) string { return "" },
			want:  authn.ReasonMalformed,
		},
		{
			// The size check runs before any parsing, so an oversized blob costs one length
			// comparison rather than a megabyte of base64 decoding on an unauthenticated path.
			name: "oversized token",
			build: func(h *harness) string {
				pad := make([]byte, authn.MaxTokenBytes+1)
				for i := range pad {
					pad[i] = 'A'
				}
				return string(pad)
			},
			want: authn.ReasonTokenTooLarge,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			obs := &recordingObserver{}
			h := newHarness(t, func(c *authn.ValidatorConfig) { c.Observer = obs })
			_, err := h.validator.Validate(context.Background(), tc.build(h))
			if got := reasonOf(t, err); got != tc.want {
				t.Fatalf("reason = %s, want %s", got, tc.want)
			}
			obs.mu.Lock()
			defer obs.mu.Unlock()
			if len(obs.failures) != 1 || obs.failures[0] != tc.want {
				t.Fatalf("the failure must be observable for metrics: %v", obs.failures)
			}
		})
	}
	_ = ek
}

func TestClockSkewIsSymmetricAndBounded(t *testing.T) {
	t.Parallel()
	rk, _, _ := keys(t)

	cases := []struct {
		name   string
		mutate func(c map[string]any, now time.Time)
		ok     bool
	}{
		{"expired 30s ago is inside the tolerance", func(c map[string]any, now time.Time) {
			c["exp"] = now.Add(-30 * time.Second).Unix()
		}, true},
		{"expired 90s ago is outside it", func(c map[string]any, now time.Time) {
			c["exp"] = now.Add(-90 * time.Second).Unix()
		}, false},
		{"nbf 30s in the future is inside the tolerance", func(c map[string]any, now time.Time) {
			c["nbf"] = now.Add(30 * time.Second).Unix()
		}, true},
		{"nbf 90s in the future is outside it", func(c map[string]any, now time.Time) {
			c["nbf"] = now.Add(90 * time.Second).Unix()
		}, false},
		{"iat 30s in the future is inside the tolerance", func(c map[string]any, now time.Time) {
			c["iat"] = now.Add(30 * time.Second).Unix()
		}, true},
		{"iat 90s in the future is outside it", func(c map[string]any, now time.Time) {
			c["iat"] = now.Add(90 * time.Second).Unix()
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil)
			c := baseClaims(h.now)
			tc.mutate(c, h.now)
			tok := assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"}, c, signRS256(rk))
			_, err := h.validator.Validate(context.Background(), tok)
			if tc.ok && err != nil {
				t.Fatalf("must be accepted within ±60s: %v (%s)", err, authn.ReasonOf(err))
			}
			if !tc.ok && err == nil {
				t.Fatal("must be rejected beyond ±60s")
			}
		})
	}
}

func TestReplayDetection(t *testing.T) {
	t.Parallel()
	rk, _, _ := keys(t)
	replay := newReplayMemory()
	h := newHarness(t, func(c *authn.ValidatorConfig) { c.Replay = replay })
	tok := assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
		baseClaims(h.now), signRS256(rk))

	if _, err := h.validator.Validate(context.Background(), tok); err != nil {
		t.Fatalf("first presentation must succeed: %v", err)
	}
	_, err := h.validator.Validate(context.Background(), tok)
	if got := reasonOf(t, err); got != authn.ReasonReplayed {
		t.Fatalf("reason = %s, want TOKEN_REPLAYED", got)
	}

	// A different jti from the same issuer is a different token, not a replay.
	c := baseClaims(h.now)
	c["jti"] = "jti-2"
	fresh := assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"}, c, signRS256(rk))
	if _, err := h.validator.Validate(context.Background(), fresh); err != nil {
		t.Fatalf("a distinct jti must be accepted: %v", err)
	}
}

// The replay store is the one control here that fails open. A Redis outage must not stop
// payments — but it must be loud.
func TestReplayStoreOutageDegradesLoudlyRatherThanFailing(t *testing.T) {
	t.Parallel()
	rk, _, _ := keys(t)
	replay := newReplayMemory()
	replay.err = errors.New("connection refused")
	obs := &recordingObserver{}
	h := newHarness(t, func(c *authn.ValidatorConfig) {
		c.Replay = replay
		c.Observer = obs
	})
	tok := assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
		baseClaims(h.now), signRS256(rk))

	for i := 0; i < 2; i++ {
		if _, err := h.validator.Validate(context.Background(), tok); err != nil {
			t.Fatalf("a replay-store outage must not fail the request: %v", err)
		}
	}
	if obs.degradedCount() != 2 {
		t.Fatalf("degradation must be recorded on every request: %d", obs.degradedCount())
	}
}

func TestRevocationIsRecheckedPerRequest(t *testing.T) {
	t.Parallel()
	rk, _, _ := keys(t)
	h := newHarness(t, func(c *authn.ValidatorConfig) {
		c.Revocation = revocationList{revoked: map[string]bool{"jti-1": true}}
	})
	tok := assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
		baseClaims(h.now), signRS256(rk))
	if got := reasonOf(t, mustErr(h.validator.Validate(context.Background(), tok))); got != authn.ReasonRevoked {
		t.Fatalf("reason = %s, want REVOKED", got)
	}
}

func TestSenderConstrainedScopeRequiresBinding(t *testing.T) {
	t.Parallel()
	rk, _, _ := keys(t)
	h := newHarness(t, nil)

	withRefund := func(cnf map[string]any) string {
		c := baseClaims(h.now)
		c["scope"] = "payments:refund"
		if cnf != nil {
			c["cnf"] = cnf
		}
		return assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"}, c, signRS256(rk))
	}
	thumb := authn.Thumbprint([]byte("peer-certificate-der"))

	// No cnf at all.
	if got := reasonOf(t, mustErr(h.validator.Validate(
		authn.WithPeerThumbprint(context.Background(), thumb), withRefund(nil)))); got != authn.ReasonNotBoundToClient {
		t.Fatalf("reason = %s, want TOKEN_NOT_BOUND_TO_CLIENT", got)
	}
	// A cnf that does not match the connection: this is the stolen-token case.
	if got := reasonOf(t, mustErr(h.validator.Validate(
		authn.WithPeerThumbprint(context.Background(), thumb),
		withRefund(map[string]any{"x5t#S256": authn.Thumbprint([]byte("someone-elses-cert"))})))); got != authn.ReasonNotBoundToClient {
		t.Fatalf("reason = %s, want TOKEN_NOT_BOUND_TO_CLIENT", got)
	}
	// A correct cnf on a connection with no client certificate at all.
	if got := reasonOf(t, mustErr(h.validator.Validate(
		context.Background(),
		withRefund(map[string]any{"x5t#S256": thumb})))); got != authn.ReasonNotBoundToClient {
		t.Fatalf("reason = %s, want TOKEN_NOT_BOUND_TO_CLIENT", got)
	}
	// The matching case.
	if _, err := h.validator.Validate(
		authn.WithPeerThumbprint(context.Background(), thumb),
		withRefund(map[string]any{"x5t#S256": thumb})); err != nil {
		t.Fatalf("a correctly bound token must validate: %v (%s)", err, authn.ReasonOf(err))
	}
	// A scope that does not require binding is unaffected.
	if _, err := h.validator.Validate(context.Background(),
		assemble(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rsa-1"},
			baseClaims(h.now), signRS256(rk))); err != nil {
		t.Fatalf("an unbound scope must not require a binding: %v", err)
	}
}

func TestNewValidatorRejectsUnsafeConfiguration(t *testing.T) {
	t.Parallel()
	jwks := authn.NewJWKS(&staticFetcher{}, authn.JWKSConfig{})
	t.Cleanup(jwks.Stop)
	good := authn.Issuer{Name: testIssuer, JWKSURL: testJWKSURL, ExpectedAudience: testAudience}

	cases := []struct {
		name string
		cfg  authn.ValidatorConfig
	}{
		{"no issuers", authn.ValidatorConfig{Environment: shared.EnvironmentProduction, Keys: jwks}},
		{"no environment", authn.ValidatorConfig{Issuers: []authn.Issuer{good}, Keys: jwks}},
		{"unknown environment", authn.ValidatorConfig{Issuers: []authn.Issuer{good}, Environment: "staging", Keys: jwks}},
		{"no key source", authn.ValidatorConfig{Issuers: []authn.Issuer{good}, Environment: shared.EnvironmentProduction}},
		{"issuer without audience", authn.ValidatorConfig{
			Issuers:     []authn.Issuer{{Name: testIssuer, JWKSURL: testJWKSURL}},
			Environment: shared.EnvironmentProduction, Keys: jwks,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := authn.NewValidator(tc.cfg); err == nil {
				t.Fatal("an unsafe configuration must fail at construction, not at runtime")
			}
		})
	}
}

func mustErr(_ *authn.Principal, err error) error { return err }
