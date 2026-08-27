package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/platform/config"
	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

type dbSettings struct {
	URL      string                `env:"PP_DB_URL" required:"true"`
	MaxConns int                   `env:"PP_DB_MAX_CONNS" default:"25"`
	Timeout  time.Duration         `env:"PP_DB_TIMEOUT" default:"5s"`
	Password secret.Secret[string] `env:"PP_DB_PASSWORD"`
}

type settings struct {
	ServiceName string        `env:"PP_SERVICE_NAME" required:"true"`
	Environment string        `env:"PP_ENVIRONMENT" required:"true"`
	Port        int           `env:"PP_PORT" default:"8443"`
	Debug       bool          `env:"PP_DEBUG" default:"false"`
	Ratio       float64       `env:"PP_SAMPLE_RATIO" default:"0.05"`
	Issuers     []string      `env:"PP_TRUSTED_ISSUERS" default:"https://idp.example.com"`
	Lease       time.Duration `env:"PP_LEASE" default:"60s"`
	// Named to match the secret pattern, so it must be masked without needing the tag.
	StripeAPIKey string `env:"PP_STRIPE_API_KEY" required:"true"`
	// A value that is sensitive but whose name does not say so; the tag forces masking.
	Pepper string `env:"PP_PEPPER" secret:"true" default:"x"`

	DB dbSettings

	// No env tag: must be ignored entirely rather than reported as missing.
	Computed string
}

func envOf(m map[string]string) config.Lookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func fullEnv() map[string]string {
	return map[string]string{
		"PP_SERVICE_NAME":   "payment-api",
		"PP_ENVIRONMENT":    "production",
		"PP_STRIPE_API_KEY": "sk_test_FAKE_abc123",
		"PP_DB_URL":         "postgres://user:hunter2@db/pp",
		"PP_DB_PASSWORD":    "hunter2",
	}
}

func TestLoadAppliesDefaultsAndConversions(t *testing.T) {
	t.Parallel()
	env := fullEnv()
	env["PP_PORT"] = "9443"
	env["PP_DEBUG"] = "true"
	env["PP_TRUSTED_ISSUERS"] = "https://a.example.com, https://b.example.com"

	var s settings
	if err := config.Load(&s, envOf(env)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	switch {
	case s.ServiceName != "payment-api":
		t.Fatalf("ServiceName = %q", s.ServiceName)
	case s.Port != 9443:
		t.Fatalf("Port = %d", s.Port)
	case !s.Debug:
		t.Fatal("Debug should be true")
	case s.Ratio != 0.05:
		t.Fatalf("Ratio = %v (default not applied)", s.Ratio)
	case s.Lease != time.Minute:
		t.Fatalf("Lease = %v (duration default not applied)", s.Lease)
	case len(s.Issuers) != 2 || s.Issuers[1] != "https://b.example.com":
		t.Fatalf("Issuers = %v", s.Issuers)
	case s.DB.MaxConns != 25:
		t.Fatalf("nested default not applied: %d", s.DB.MaxConns)
	case s.DB.Timeout != 5*time.Second:
		t.Fatalf("nested duration default not applied: %v", s.DB.Timeout)
	case s.DB.Password.Expose() != "hunter2":
		t.Fatal("a secret-typed field must still be populated")
	case s.Computed != "":
		t.Fatal("a field without an env tag must be ignored")
	}
}

// A loader that stops at the first missing variable turns a misconfigured deployment into a
// sequence of failed rollouts. It must report all of them at once.
func TestLoadReportsEveryMissingVariableAtOnce(t *testing.T) {
	t.Parallel()
	var s settings
	err := config.Load(&s, envOf(map[string]string{}))
	if err == nil {
		t.Fatal("a configuration with no variables set must fail")
	}
	var e *apierror.Error
	if !errors.As(err, &e) || e.Code != apierror.CodeConfigurationInvalid {
		t.Fatalf("err = %v", err)
	}
	want := map[string]bool{
		"PP_SERVICE_NAME": true, "PP_ENVIRONMENT": true, "PP_STRIPE_API_KEY": true, "PP_DB_URL": true,
	}
	got := map[string]bool{}
	for _, d := range e.Details {
		got[d.Field] = true
		if d.Code != "MISSING" {
			t.Fatalf("detail %+v", d)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("reported %v, want exactly %v — every missing variable, not the first", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("%s was not reported", k)
		}
	}
	if !strings.Contains(e.Message, "4 problem(s)") {
		t.Fatalf("message = %q", e.Message)
	}
}

func TestLoadReportsTypeErrorsAlongsideMissingOnes(t *testing.T) {
	t.Parallel()
	env := fullEnv()
	delete(env, "PP_ENVIRONMENT")
	env["PP_PORT"] = "not-a-number"
	env["PP_LEASE"] = "not-a-duration"

	var s settings
	err := config.Load(&s, envOf(env))
	var e *apierror.Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v", err)
	}
	kinds := map[string]string{}
	for _, d := range e.Details {
		kinds[d.Field] = d.Code
	}
	if kinds["PP_ENVIRONMENT"] != "MISSING" || kinds["PP_PORT"] != "INVALID" || kinds["PP_LEASE"] != "INVALID" {
		t.Fatalf("details = %+v", e.Details)
	}
	// The message must name the type, never the value: an unparseable connection string is
	// still a connection string.
	for _, d := range e.Details {
		if strings.Contains(d.Message, "not-a-number") || strings.Contains(d.Message, "not-a-duration") {
			t.Fatalf("a type error echoed the value: %q", d.Message)
		}
	}
}

func TestLoadRejectsNonPointer(t *testing.T) {
	t.Parallel()
	var s settings
	if err := config.Load(s, envOf(fullEnv())); err == nil {
		t.Fatal("Load requires a pointer")
	}
	if err := config.Load((*settings)(nil), envOf(fullEnv())); err == nil {
		t.Fatal("Load requires a non-nil pointer")
	}
	if err := config.Load(&[]string{}, envOf(fullEnv())); err == nil {
		t.Fatal("Load requires a struct")
	}
}

func TestRedactedMasksEverythingSensitive(t *testing.T) {
	t.Parallel()
	var s settings
	if err := config.Load(&s, envOf(fullEnv())); err != nil {
		t.Fatal(err)
	}
	entries := config.Redacted(&s)
	byName := map[string]config.RedactedEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}

	mustMask := []string{
		"PP_STRIPE_API_KEY", // name matches api_key
		"PP_DB_PASSWORD",    // name matches password, and the field is secret-typed
		"PP_PEPPER",         // tagged
		"PP_DB_URL",         // name matches connection-string/dsn-shaped credentials
	}
	for _, name := range mustMask {
		e, ok := byName[name]
		if !ok {
			t.Fatalf("%s missing from the dump", name)
		}
		if !e.Masked || e.Value != secret.Redacted {
			t.Fatalf("%s rendered as %q; it must be masked", name, e.Value)
		}
	}
	for _, name := range []string{"PP_SERVICE_NAME", "PP_ENVIRONMENT", "PP_PORT", "PP_LEASE"} {
		e := byName[name]
		if e.Masked {
			t.Fatalf("%s should not be masked; over-masking makes the dump useless", name)
		}
	}
	if byName["PP_SERVICE_NAME"].Value != "payment-api" || byName["PP_LEASE"].Value != "1m0s" {
		t.Fatalf("benign values rendered wrongly: %+v", byName)
	}

	// The rendered line must not contain any of the real secret material anywhere.
	line := config.RedactedString(&s)
	for _, leak := range []string{"hunter2", "sk_test_FAKE_abc123", "postgres://"} {
		if strings.Contains(line, leak) {
			t.Fatalf("the startup dump leaked %q: %s", leak, line)
		}
	}
	if !strings.Contains(line, "PP_SERVICE_NAME=payment-api") {
		t.Fatalf("the dump must still be useful: %s", line)
	}
}

func TestRedactedHandlesOddInput(t *testing.T) {
	t.Parallel()
	if config.Redacted(nil) != nil {
		t.Fatal("nil renders as nothing")
	}
	if config.Redacted((*settings)(nil)) != nil {
		t.Fatal("a nil pointer renders as nothing")
	}
	if config.Redacted("not a struct") != nil {
		t.Fatal("a non-struct renders as nothing")
	}
}

func TestRedactedIsSorted(t *testing.T) {
	t.Parallel()
	var s settings
	_ = config.Load(&s, envOf(fullEnv()))
	entries := config.Redacted(&s)
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Name > entries[i].Name {
			t.Fatalf("the dump must be stably ordered so two runs diff cleanly: %v", entries)
		}
	}
}
