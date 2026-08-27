package main

import (
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/platform/config"
	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// TestConfigurationReportsEveryMissingVariableAtOnce is the startup-failure contract.
//
// A loader that stops at the first missing variable turns a misconfigured deployment into a
// sequence of failed rollouts: fix one, redeploy, discover the next, fix it, redeploy. With eight
// missing variables that is eight deploys and an afternoon. This asserts that one attempt produces
// the whole list.
//
// It also asserts *which* variables are required, which is the more subtle property: `required` is
// reserved for things that must never have a default, and a default quietly appearing on one of
// them is how a JWKS URL becomes optional.
func TestConfigurationReportsEveryMissingVariableAtOnce(t *testing.T) {
	t.Parallel()
	var cfg env
	err := config.Load(&cfg, func(string) (string, bool) { return "", false })
	if err == nil {
		t.Fatal("a completely unset environment was accepted")
	}
	e := apierror.From(err)
	if e.Code != apierror.CodeConfigurationInvalid {
		t.Errorf("code = %s, want CONFIGURATION_INVALID", e.Code)
	}

	got := map[string]bool{}
	for _, d := range e.Details {
		got[d.Field] = true
	}
	want := []string{
		// Security-relevant, and therefore never defaulted: a defaulted issuer, audience or JWKS
		// URL is an authentication control that appears configured and verifies nothing.
		"PP_AUTH_JWKS_URL", "PP_AUTH_ISSUER", "PP_AUTH_AUDIENCE",
		// A DSN has no safe default.
		"PP_DATABASE_URL",
		// Defaulting the environment would mean a misconfigured pod defaults to *some*
		// environment, and either default is wrong half the time.
		"PP_ENVIRONMENT",
		// A region is used by the residency policy and reported by the probes.
		"PP_REGION",
		// A server with no address binds nothing and is never reached, which passes every test.
		"PP_HTTP_ADDR",
		// Trusting the Host header instead would hand an attacker a redirect primitive.
		"PP_PUBLIC_BASE_URL",
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("%s is not reported as required", name)
		}
	}
	if len(e.Details) != len(want) {
		t.Errorf("reported %d problems, want exactly %d — a new required variable needs a "+
			"deliberate decision, and a newly-optional one needs one too:\n%v",
			len(e.Details), len(want), got)
	}
}

// TestOptionalVariablesHaveDefaults asserts that a fully-specified minimum is accepted, so the
// required set is genuinely the minimum rather than an under-count.
func TestOptionalVariablesHaveDefaults(t *testing.T) {
	t.Parallel()
	supplied := map[string]string{
		"PP_ENVIRONMENT":     "sandbox",
		"PP_REGION":          "eu-central-1",
		"PP_DATABASE_URL":    "postgres://user:pass@localhost:5432/payments",
		"PP_HTTP_ADDR":       ":8080",
		"PP_PUBLIC_BASE_URL": "https://api.example.com",
		"PP_AUTH_JWKS_URL":   "https://auth.example.com/.well-known/jwks.json",
		"PP_AUTH_ISSUER":     "https://auth.example.com/",
		"PP_AUTH_AUDIENCE":   "payments-platform",
	}
	var cfg env
	if err := config.Load(&cfg, func(k string) (string, bool) {
		v, ok := supplied[k]
		return v, ok
	}); err != nil {
		t.Fatalf("the documented minimum was rejected: %v", err)
	}

	// Spot-check the defaults that matter operationally.
	if cfg.AdminAddr == "" {
		t.Error("PP_ADMIN_ADDR has no default; the probe listener would bind nothing")
	}
	if cfg.ReadHeaderTimeout == 0 {
		t.Error("the read-header timeout defaults to zero, which net/http reads as no deadline — " +
			"a slow-loris vulnerability rather than an omission")
	}
	if cfg.ConcurrencyMax == 0 {
		t.Error("the concurrency ceiling defaults to zero")
	}
	if !cfg.RedisTLS {
		t.Error("Redis TLS defaults to off; it must default to on")
	}
}

// TestRedactedConfigMasksTheDSN asserts the startup dump does not print a connection string.
//
// A DSN conventionally embeds its own credentials. The startup dump exists because "which
// configuration is this pod actually running?" is the first question of most incidents — and it is
// only safe to answer because the masking happens on the variable's *name*, using the same pattern
// the admission policy and the CI scanner use.
func TestRedactedConfigMasksTheDSN(t *testing.T) {
	t.Parallel()
	cfg := env{}
	cfg.DatabaseURL = secret.New("postgres://user:hunter2@db.internal:5432/payments")
	cfg.RedisPassword = secret.New("s3cr3t")
	cfg.HTTPAddr = ":8080"

	for _, entry := range config.Redacted(&cfg) {
		if entry.Name == "PP_DATABASE_URL" {
			if !entry.Masked {
				t.Error("PP_DATABASE_URL is not masked in the startup dump")
			}
			if entry.Value == "postgres://user:hunter2@db.internal:5432/payments" {
				t.Errorf("the DSN was rendered verbatim: %s", entry.Value)
			}
		}
		if entry.Name == "PP_REDIS_PASSWORD" && !entry.Masked {
			t.Error("PP_REDIS_PASSWORD is not masked")
		}
		// A non-secret value must be shown, or the dump is useless and people stop logging it.
		if entry.Name == "PP_HTTP_ADDR" && entry.Value != ":8080" {
			t.Errorf("PP_HTTP_ADDR = %q, want the literal value", entry.Value)
		}
	}
}
