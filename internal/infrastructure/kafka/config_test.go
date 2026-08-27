package kafka

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

const testPassword = "hunter2-very-secret"

func prodConfig() Config {
	c := DefaultConfig()
	c.Brokers = []string{"b1.kafka:9094", "b2.kafka:9094"}
	c.ClientID = "payment-orchestrator"
	c.Username = "pp-orchestrator"
	c.Password = secret.New(testPassword)
	return c
}

func TestValidateAcceptsAProductionConfig(t *testing.T) {
	t.Parallel()
	if err := prodConfig().Validate(); err != nil {
		t.Fatalf("Validate rejected a valid production config: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*Config){
		"no brokers":               func(c *Config) { c.Brokers = nil },
		"broker without a port":    func(c *Config) { c.Brokers = []string{"b1.kafka"} },
		"no client id":             func(c *Config) { c.ClientID = "" },
		"unknown protocol":         func(c *Config) { c.Protocol = "QUIC" },
		"plaintext in production":  func(c *Config) { c.Protocol = ProtocolPlaintext },
		"unknown mechanism":        func(c *Config) { c.Mechanism = "KERBEROS" },
		"no username":              func(c *Config) { c.Username = "" },
		"no password":              func(c *Config) { c.Password = secret.New("") },
		"tls verification off":     func(c *Config) { c.InsecureSkipVerify = true },
		"zero timeout":             func(c *Config) { c.RequestTimeout = 0 },
		"retry beyond stale claim": func(c *Config) { c.RetryTimeout = 45 * time.Second },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := prodConfig()
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("Validate accepted: %s", name)
			}
		})
	}
}

// TestLocalEnvironmentMayUsePlaintext: the guards are environment-gated, not absolute, so a
// developer's docker-compose broker still works.
func TestLocalEnvironmentMayUsePlaintext(t *testing.T) {
	t.Parallel()
	c := DefaultConfig()
	c.Brokers = []string{"localhost:9092"}
	c.Protocol = ProtocolPlaintext
	c.Environment = "local"
	if err := c.Validate(); err != nil {
		t.Fatalf("a local plaintext broker was rejected: %v", err)
	}
}

// TestConfigNeverRendersThePassword is the test that keeps a credential out of Loki.
func TestConfigNeverRendersThePassword(t *testing.T) {
	t.Parallel()
	c := prodConfig()
	for _, rendered := range []string{
		c.Redacted(),
		c.String(),
		fmt.Sprintf("%v", c),
		fmt.Sprintf("%+v", c),
		fmt.Sprintf("%#v", c),
		//nolint:staticcheck // S1025 would replace this with c.String(); the point of the table is
		// to prove every fmt verb redacts, and %s is the one a caller reaches for by reflex.
		fmt.Sprintf("%s", c),
		fmt.Sprintf("%q", c),
	} {
		if strings.Contains(rendered, testPassword) {
			t.Fatalf("the SASL password leaked into a rendering: %s", rendered)
		}
	}
	// And the validation error for a missing password names the variable, never a value.
	c.Password = secret.New("")
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted a config with no password")
	}
	var pe *apierror.Error
	if !errors.As(err, &pe) {
		t.Fatalf("want a platform error, got %T", err)
	}
	var named bool
	for _, d := range pe.Details {
		if d.Field == EnvPassword {
			named = true
		}
		if strings.Contains(d.Message, testPassword) {
			t.Fatal("a validation detail echoed the credential")
		}
	}
	if !named {
		t.Fatalf("no detail names %s: %+v", EnvPassword, pe.Details)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv(EnvBrokers, " b1:9094 , b2:9094 ")
	t.Setenv(EnvClientID, "event-consumer")
	t.Setenv(EnvProtocol, "sasl_ssl")
	t.Setenv(EnvMechanism, "scram-sha-256")
	t.Setenv(EnvUsername, "pp")
	t.Setenv(EnvPassword, testPassword)
	t.Setenv(EnvRequestTimeout, "7s")
	t.Setenv(EnvEnvironment, "production")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if len(cfg.Brokers) != 2 || cfg.Brokers[0] != "b1:9094" {
		t.Fatalf("brokers = %v", cfg.Brokers)
	}
	if cfg.Protocol != ProtocolSASLSSL || cfg.Mechanism != MechanismScramSHA256 {
		t.Fatalf("protocol/mechanism = %s/%s", cfg.Protocol, cfg.Mechanism)
	}
	if cfg.RequestTimeout != 7*time.Second {
		t.Fatalf("request timeout = %v", cfg.RequestTimeout)
	}
	if cfg.Password.Expose() != testPassword {
		t.Fatal("the password did not survive the round trip")
	}
}

func TestConfigFromEnvRejectsAMalformedDuration(t *testing.T) {
	t.Setenv(EnvBrokers, "b1:9094")
	t.Setenv(EnvUsername, "pp")
	t.Setenv(EnvPassword, testPassword)
	t.Setenv(EnvDialTimeout, "ten seconds")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("ConfigFromEnv accepted a malformed duration")
	}
}

func TestClientOptionsBuildForEveryMechanism(t *testing.T) {
	t.Parallel()
	for _, m := range []SASLMechanism{MechanismPlain, MechanismScramSHA256, MechanismScramSHA512} {
		c := prodConfig()
		c.Mechanism = m
		opts, err := c.ClientOptions()
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		if len(opts) == 0 {
			t.Fatalf("%s: no options", m)
		}
	}
}

func TestClientOptionsRejectAnInvalidConfig(t *testing.T) {
	t.Parallel()
	c := prodConfig()
	c.Brokers = nil
	if _, err := c.ClientOptions(); err == nil {
		t.Fatal("ClientOptions built options for an invalid config")
	}
}

func TestTLSConfigIsNilWithoutTLSAndPinnedWithIt(t *testing.T) {
	t.Parallel()
	plain := DefaultConfig()
	plain.Protocol = ProtocolPlaintext
	if tc, err := plain.TLSConfig(); err != nil || tc != nil {
		t.Fatalf("plaintext produced a TLS config: %v %v", tc, err)
	}

	tc, err := prodConfig().TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	if tc.MinVersion < 0x0303 { // TLS 1.2
		t.Fatalf("MinVersion = %x, want at least TLS 1.2", tc.MinVersion)
	}
}

func TestTLSConfigRejectsABadCABundle(t *testing.T) {
	t.Parallel()
	c := prodConfig()
	c.CACertFile = "/nonexistent/ca.pem"
	if _, err := c.TLSConfig(); err == nil {
		t.Fatal("TLSConfig accepted a missing CA bundle")
	}
}
