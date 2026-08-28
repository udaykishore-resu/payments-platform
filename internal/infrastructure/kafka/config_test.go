package kafka

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

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

// TestLocalEnvironmentMayUsePlaintext: the guards are gated on where the brokers are, not
// absolute, so a developer's docker-compose broker reached on a published port still works.
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

// TestPlaintextIsPermittedByBrokerAddressNotByEnvironmentName is the counterpart of the Redis
// test of the same shape, and for the same defect: the predicate compared Config.Environment
// against names runtime.ParseEnvironment cannot produce, so `-tags` aside there was no startable
// configuration in which outbox-relay and event-consumer could reach a local Redpanda.
func TestPlaintextIsPermittedByBrokerAddressNotByEnvironmentName(t *testing.T) {
	t.Parallel()
	c := DefaultConfig()
	c.Brokers = []string{"localhost:19092", "127.0.0.1:19093"}
	c.Protocol = ProtocolPlaintext
	c.Environment = "sandbox" // what runtime.ParseEnvironment actually produces
	if err := c.Validate(); err != nil {
		t.Fatalf("plaintext to loopback brokers was rejected in sandbox: %v", err)
	}
}

// TestPlaintextIsRefusedOffHost pins that the new rule is tighter than the one it replaced.
func TestPlaintextIsRefusedOffHost(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*Config){
		"remote broker in sandbox": func(c *Config) {
			c.Environment = "sandbox"
			c.Brokers = []string{"kafka.internal:9092"}
		},
		"remote broker in an environment named dev": func(c *Config) {
			c.Environment = "dev"
			c.Brokers = []string{"kafka.internal:9092"}
		},
		// One remote broker in the seed list means the connection leaves this host. A rule that
		// looked only at the first entry would be trivially bypassed by ordering.
		"one remote broker among loopback ones": func(c *Config) {
			c.Environment = "sandbox"
			c.Brokers = []string{"localhost:19092", "kafka.internal:9092"}
		},
		"loopback in production": func(c *Config) {
			c.Environment = "production"
			c.Brokers = []string{"127.0.0.1:9092"}
		},
		// "No brokers" must never be the thing that unlocks plaintext: an empty list is a
		// misconfiguration, and a misconfiguration that relaxes a security control is the worst
		// kind. It is rejected for the missing brokers too — this asserts it is not accepted.
		"no brokers at all": func(c *Config) {
			c.Environment = "sandbox"
			c.Brokers = nil
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := DefaultConfig()
			c.Protocol = ProtocolPlaintext
			mutate(&c)
			c.ClientID = "test"
			if err := c.Validate(); err == nil {
				t.Fatalf("Validate accepted plaintext for %s", name)
			}
		})
	}
}

// TestClientOptionsProduceAConstructibleClient is the test whose absence let a production-only
// defect sit in the tree behind a comment in .env.dev.
//
// Validate() passing says the configuration is coherent; it says nothing about whether franz-go
// will accept the options built from it. ClientOptions set both kgo.Dialer and kgo.DialTLSConfig,
// which franz-go refuses at construction — so every TLS-using protocol, meaning every production
// configuration, failed at "creating producer client" while every unit test passed. Building a
// real client is the only assertion that covers the gap, and it is cheap: kgo.NewClient does not
// dial, it validates and returns.
func TestClientOptionsProduceAConstructibleClient(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*Config){
		"SASL_SSL (production)": func(c *Config) { c.Protocol = ProtocolSASLSSL },
		"SSL (mutual TLS)": func(c *Config) {
			c.Protocol = ProtocolSSL
			c.Username, c.Password = "", secret.New("")
		},
		"PLAINTEXT (loopback broker)": func(c *Config) {
			c.Protocol = ProtocolPlaintext
			c.Environment = "sandbox"
			c.Brokers = []string{"localhost:19092"}
			c.Username, c.Password = "", secret.New("")
		},
		"SASL_PLAINTEXT (loopback broker)": func(c *Config) {
			c.Protocol = ProtocolSASLPlaintext
			c.Mechanism = MechanismScramSHA512
			c.Environment = "sandbox"
			c.Brokers = []string{"localhost:19092"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := prodConfig()
			mutate(&c)
			opts, err := c.ClientOptions()
			if err != nil {
				t.Fatalf("ClientOptions: %v", err)
			}
			client, err := kgo.NewClient(opts...)
			if err != nil {
				t.Fatalf("franz-go refused the options: %v", err)
			}
			client.Close()
		})
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
