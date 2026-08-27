// Command devissuer is the local development OIDC issuer.
//
// # Why this exists
//
// `internal/platform/authn` requires a real JWKS endpoint and real RS256 tokens: the validator
// refuses to construct without an issuer, an audience and a key source, and it rejects `alg:none`,
// embedded keys, missing `kid`, a wrong `aud`, a missing `jti`, a malformed `tenant_id` and an
// `env` claim that does not equal the deployment's environment. There is deliberately no "skip
// authentication in development" switch, because a switch like that is one environment variable
// away from being on in production.
//
// So a developer needs an issuer. This is the smallest one that satisfies every rule the
// validator enforces, and nothing more:
//
//	GET  /.well-known/jwks.json            the public key, in JWKS form
//	GET  /.well-known/openid-configuration discovery, for tools that expect it
//	GET|POST /token                        mints a token and returns it as JSON
//	GET  /healthz                          liveness, for docker-compose and dev-up.sh
//
// # Why the key is ephemeral by default
//
// The key pair is generated at start and lives only in memory, so a leaked development token is
// worthless the moment the process restarts — which is exactly what `config/dev.yaml` says about
// this endpoint. `-key-file` persists it for the case where a developer restarts the issuer and
// wants their existing tokens to keep working; it is off by default because a private key on disk
// is a private key in a backup.
//
// # Why RS256 and not HS256
//
// The validator's algorithm allowlist is {ES256, RS256} and excludes every symmetric algorithm,
// which is what removes the RS256→HS256 key-confusion attack rather than merely guarding against
// it. A symmetric dev issuer would not exercise the same code path as production.
//
// This binary is a development tool. It has no production build target, it refuses to mint a token
// whose `env` claim is `production`, and the Dockerfile's guard stage covers the image.
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/ids"
)

// defaultScopes is what a developer needs to drive the e2e suite and the quick-start `curl`.
//
// `payments:refund` and `credentials:rotate` are deliberately absent: the validator requires those
// two to be presented on a *sender-constrained* token (RFC 8705, `cnf.x5t#S256` matching the TLS
// client certificate on the connection), and a bearer token carrying them would be rejected with
// NOT_BOUND_TO_CLIENT — a 401 that is very hard to read as "this scope needs mTLS". Ask for them
// explicitly with -scope if you are testing that rejection.
const defaultScopes = "payments:read payments:write payments:capture payments:void " +
	"merchants:read merchants:write config:read config:write config:publish " +
	"gateways:read audit:read"

// defaultRoles is what a *machine* client is, in this platform's RBAC.
//
// `internal/platform/authz` evaluates RBAC as the *union* of grants across a principal's roles,
// and no single role covers a developer's whole loop:
//
//   - `svc:payment-client` grants PermPaymentsWrite and PermPaymentsCapture, and grants
//     PermPaymentsRead only as *merchant-scoped* — so a tenant-wide `GET /v1/payments` with no
//     merchantId is denied by the scope guard, deliberately.
//   - `tenant-admin` grants the tenant-wide reads and the merchant/configuration writes an
//     onboarding flow needs, and denies payment writes.
//   - `operator`, which reads as the obvious default, denies PermPaymentsWrite outright: it is a
//     human support role that reads, suspends and dual-controls. A dev token carrying only
//     `operator` authenticates perfectly and then 403s on POST /v1/payments, which is a
//     genuinely confusing hour.
//
// The union of the three is what a local integration actually is. Narrow it with -roles when
// testing an authorization boundary.
const defaultRoles = "svc:payment-client,svc:onboarding-client,tenant-admin"

type options struct {
	addr     string
	issuer   string
	audience string
	env      string
	tenant   string
	tier     string
	subject  string
	scopes   string
	roles    string
	merchant string
	ttl      time.Duration
	keyFile  string
	once     bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "devissuer:", err)
		os.Exit(1)
	}
}

func run() error {
	var o options
	flag.StringVar(&o.addr, "addr", env("PP_DEV_ISSUER_ADDR", ":8088"), "listen address")
	flag.StringVar(&o.issuer, "issuer", env("PP_DEV_ISSUER_URL", "http://localhost:8088/"),
		"the `iss` claim and the discovery issuer; must match PP_AUTH_ISSUER exactly")
	flag.StringVar(&o.audience, "audience", env("PP_DEV_ISSUER_AUDIENCE", "payments-platform-dev"),
		"the `aud` claim; must match PP_AUTH_AUDIENCE exactly")
	flag.StringVar(&o.env, "env", env("PP_DEV_ISSUER_ENV", "sandbox"),
		"the `env` claim; must equal the deployment's PP_ENVIRONMENT")
	flag.StringVar(&o.tenant, "tenant", env("PP_DEV_ISSUER_TENANT", ""),
		"default `tenant_id` (ten_…); generated if empty")
	flag.StringVar(&o.tier, "tier", env("PP_DEV_ISSUER_TIER", "POOLED"), "tenant tier: POOLED or SILOED")
	flag.StringVar(&o.subject, "subject", env("PP_DEV_ISSUER_SUBJECT", "cli_local_dev"), "the `sub` claim")
	flag.StringVar(&o.scopes, "scope", env("PP_DEV_ISSUER_SCOPES", defaultScopes), "space-delimited scopes")
	flag.StringVar(&o.merchant, "merchant-scope", env("PP_DEV_ISSUER_MERCHANT_SCOPE", ""),
		"comma-separated merchant ids for the `merchant_scope` claim; empty means unscoped")
	// merchant_scope is empty by default on purpose. A narrowed credential is denied any
	// tenant-wide operation — `GET /v1/payments` with no merchantId returns 403, because the
	// policy engine treats a scoped grant performing a tenant-wide listing as a widening it
	// will not permit. That is correct, and it is also the least obvious 403 in the platform,
	// so a developer opts into it rather than starting inside it.
	flag.StringVar(&o.roles, "roles", env("PP_DEV_ISSUER_ROLES", defaultRoles),
		"comma-separated RBAC roles for the `roles` claim")
	flag.DurationVar(&o.ttl, "ttl", envDuration("PP_DEV_ISSUER_TTL", 15*time.Minute), "token lifetime")
	flag.StringVar(&o.keyFile, "key-file", env("PP_DEV_ISSUER_KEY_FILE", ""),
		"persist the RSA key here instead of generating an ephemeral one")
	flag.BoolVar(&o.once, "print-token", false, "mint one token, print it to stdout and exit")
	flag.Parse()

	// The one refusal this tool makes. A development issuer that can mint a production-environment
	// token is a development issuer whose keys are a production credential.
	if strings.EqualFold(o.env, "production") || strings.EqualFold(o.env, "prod") {
		return errors.New("refusing to mint tokens with env=production: this issuer is for local " +
			"development only, and its private key is generated in memory with no protection")
	}
	if o.tenant == "" {
		o.tenant = string(ids.New(ids.PrefixTenant))
	}
	if err := ids.Validate(o.tenant, ids.PrefixTenant); err != nil {
		return fmt.Errorf("-tenant %q is not a valid tenant id: %w", o.tenant, err)
	}
	if !strings.HasSuffix(o.issuer, "/") {
		// The `iss` claim is compared for exact string equality against PP_AUTH_ISSUER. A trailing
		// slash that disagrees produces UNTRUSTED_ISSUER, which reads as "wrong issuer" rather than
		// "one character".
		o.issuer += "/"
	}

	key, kid, err := loadOrGenerateKey(o.keyFile)
	if err != nil {
		return err
	}

	if o.once {
		tok, err := mint(key, kid, o, o.tenant, o.scopes, o.ttl)
		if err != nil {
			return err
		}
		fmt.Println(tok)
		return nil
	}

	srv := &server{key: key, kid: kid, opt: o}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", srv.jwks)
	mux.HandleFunc("/.well-known/openid-configuration", srv.discovery)
	mux.HandleFunc("/token", srv.token)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	log.Printf("devissuer listening on %s", o.addr)
	log.Printf("  issuer   %s", o.issuer)
	log.Printf("  audience %s", o.audience)
	log.Printf("  env      %s", o.env)
	log.Printf("  tenant   %s", o.tenant)
	log.Printf("  jwks     http://localhost%s/.well-known/jwks.json", portOf(o.addr))
	log.Printf("  token    http://localhost%s/token", portOf(o.addr))

	h := &http.Server{
		Addr:              o.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return h.ListenAndServe()
}

type server struct {
	key *rsa.PrivateKey
	kid string
	opt options
}

// jwks publishes the public key.
//
// `alg` and `use` are both present because the platform's parser drops any key without a `kid` or
// with an `alg` outside its allowlist, and skips a key whose `use` is set to anything but `sig`.
func (s *server) jwks(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]any{"keys": []map[string]string{{
		"kty": "RSA",
		"kid": s.kid,
		"alg": "RS256",
		"use": "sig",
		"n":   b64u(s.key.N.Bytes()),
		"e":   b64u(big.NewInt(int64(s.key.E)).Bytes()),
	}}}
	writeJSON(w, http.StatusOK, doc)
}

func (s *server) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.opt.issuer,
		"jwks_uri":                              strings.TrimSuffix(s.opt.issuer, "/") + "/.well-known/jwks.json",
		"token_endpoint":                        strings.TrimSuffix(s.opt.issuer, "/") + "/token",
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"grant_types_supported":                 []string{"client_credentials"},
		"scopes_supported":                      strings.Fields(defaultScopes),
	})
}

// token mints one access token. Query parameters override the process defaults so a test can ask
// for a narrower scope or a different tenant without restarting the issuer.
func (s *server) token(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tenant := first(q.Get("tenant_id"), s.opt.tenant)
	if err := ids.Validate(tenant, ids.PrefixTenant); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_request", "error_description": "tenant_id must be a ten_… ULID"})
		return
	}
	scope := first(q.Get("scope"), s.opt.scopes)
	opt := s.opt
	opt.roles = first(q.Get("roles"), s.opt.roles)
	opt.merchant = first(q.Get("merchant_scope"), s.opt.merchant)
	ttl := s.opt.ttl
	if v := q.Get("ttl"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "invalid_request", "error_description": "ttl must be a Go duration, e.g. 15m"})
			return
		}
		ttl = d
	}
	opt.audience = first(q.Get("audience"), s.opt.audience)

	tok, err := mint(s.key, s.kid, opt, tenant, scope, ttl)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":   tok,
		"token_type":     "Bearer",
		"expires_in":     int(ttl.Seconds()),
		"scope":          scope,
		"tenant_id":      tenant,
		"roles":          splitRoles(opt.roles),
		"merchant_scope": splitRoles(opt.merchant),
		"issuer":         opt.issuer,
		"audience":       opt.audience,
	})
}

// mint builds and signs the token.
//
// Every claim here is one the validator checks. `aud` is a single string because the validator
// requires exactly one audience value and compares it for equality — accepting a token whose
// audience list merely *includes* ours is a published cross-service replay bug.
func mint(key *rsa.PrivateKey, kid string, o options, tenant, scope string, ttl time.Duration) (string, error) {
	now := time.Now().UTC()
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	claims := map[string]any{
		"iss":         o.issuer,
		"sub":         o.subject,
		"aud":         o.audience,
		"exp":         now.Add(ttl).Unix(),
		"nbf":         now.Add(-30 * time.Second).Unix(),
		"iat":         now.Unix(),
		"jti":         string(ids.New(ids.PrefixRequest)),
		"tenant_id":   tenant,
		"tenant_tier": o.tier,
		"env":         o.env,
		"scope":       scope,
		"roles":       splitRoles(o.roles),
		"name":        "local development",
		"auth_time":   now.Unix(),
	}
	// merchant_scope narrows a credential to specific merchants. It is omitted rather than sent
	// empty: the RBAC evaluation treats an absent scope as "not narrowed", and an empty array as
	// "narrowed to nothing", which are opposite meanings.
	if ms := splitRoles(o.merchant); len(ms) > 0 {
		claims["merchant_scope"] = ms
	}
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := b64u(h) + "." + b64u(c)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64u(sig), nil
}

// loadOrGenerateKey returns the signing key and its `kid`.
//
// The `kid` is derived from the public key so that it is stable for a persisted key and unique for
// an ephemeral one, with no state to keep in step.
func loadOrGenerateKey(path string) (*rsa.PrivateKey, string, error) {
	if path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			block, _ := pem.Decode(raw)
			if block == nil {
				return nil, "", fmt.Errorf("%s is not a PEM file", path)
			}
			key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, "", fmt.Errorf("%s does not contain a PKCS#8 key: %w", path, err)
			}
			rk, ok := key.(*rsa.PrivateKey)
			if !ok {
				return nil, "", fmt.Errorf("%s is not an RSA key", path)
			}
			return rk, kidOf(rk), nil
		} else if !os.IsNotExist(err) {
			return nil, "", err
		}
	}
	// 2048 is the platform's own floor: the JWKS parser rejects an RSA key shorter than that, so a
	// smaller dev key would produce a 401 whose cause is three layers away.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, "", err
	}
	if path != "" {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, "", err
		}
		// 0600: it is a private key, even a disposable one.
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return nil, "", err
		}
	}
	return key, kidOf(key), nil
}

func kidOf(k *rsa.PrivateKey) string {
	sum := sha256.Sum256(k.N.Bytes())
	return "dev-" + b64u(sum[:8])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// splitRoles turns the comma-separated flag into the claim's array form, dropping empties so a
// trailing comma is not a role named "".
func splitRoles(v string) []string {
	out := []string{}
	for _, r := range strings.Split(v, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

func first(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return def
}

// portOf renders ":8088" as ":8088" and "0.0.0.0:8088" as ":8088", for the startup banner.
func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return ":" + addr
}
