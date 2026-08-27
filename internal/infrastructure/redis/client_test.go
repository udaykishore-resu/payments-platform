package redis

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

const testRedisPassword = "correct-horse-battery-staple"

func prodRedisConfig() Config {
	c := DefaultConfig()
	c.Addr = "redis.internal:6379"
	c.Username = "pp"
	c.Password = secret.New(testRedisPassword)
	return c
}

func TestRedisValidateAcceptsAProductionConfig(t *testing.T) {
	t.Parallel()
	if err := prodRedisConfig().Validate(); err != nil {
		t.Fatalf("Validate rejected a valid config: %v", err)
	}
}

func TestRedisValidateRejects(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*Config){
		"no address":            func(c *Config) { c.Addr = "" },
		"address without port":  func(c *Config) { c.Addr = "redis.internal" },
		"negative db":           func(c *Config) { c.DB = -1 },
		"tls off in production": func(c *Config) { c.TLS = false },
		"verification off":      func(c *Config) { c.InsecureSkipVerify = true },
		"zero pool":             func(c *Config) { c.PoolSize = 0 },
		"idle above pool":       func(c *Config) { c.MinIdleConns = c.PoolSize + 1 },
		"zero timeout":          func(c *Config) { c.ReadTimeout = 0 },
		// A cache that can block a request for seconds is worse than no cache: the fallback path
		// is fast and always available.
		"slow read timeout": func(c *Config) { c.ReadTimeout = 10 * time.Second },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := prodRedisConfig()
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("Validate accepted: %s", name)
			}
		})
	}
}

func TestRedisLocalMayDisableTLS(t *testing.T) {
	t.Parallel()
	c := DefaultConfig()
	c.Addr = "localhost:6379"
	c.TLS = false
	c.Environment = "local"
	if err := c.Validate(); err != nil {
		t.Fatalf("a local plaintext Redis was rejected: %v", err)
	}
}

func TestRedisConfigNeverRendersThePassword(t *testing.T) {
	t.Parallel()
	c := prodRedisConfig()
	for _, rendered := range []string{
		c.Redacted(),
		c.String(),
		fmt.Sprintf("%v", c),
		fmt.Sprintf("%+v", c),
		fmt.Sprintf("%#v", c),
		fmt.Sprintf("%q", c),
	} {
		if strings.Contains(rendered, testRedisPassword) {
			t.Fatalf("the password leaked into a rendering: %s", rendered)
		}
	}
}

func TestRedisConfigFromEnv(t *testing.T) {
	t.Setenv(EnvAddr, "redis.internal:6380")
	t.Setenv(EnvUsername, "pp")
	t.Setenv(EnvPassword, testRedisPassword)
	t.Setenv(EnvDB, "3")
	t.Setenv(EnvPoolSize, "40")
	t.Setenv(EnvMinIdleConns, "4")
	t.Setenv(EnvReadTimeout, "300ms")
	t.Setenv(EnvEnvironmentVar, "production")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Addr != "redis.internal:6380" || cfg.DB != 3 || cfg.PoolSize != 40 || cfg.MinIdleConns != 4 {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.ReadTimeout != 300*time.Millisecond {
		t.Fatalf("read timeout = %v", cfg.ReadTimeout)
	}
	if cfg.Password.Expose() != testRedisPassword {
		t.Fatal("the password did not survive the round trip")
	}
}

func TestRedisConfigFromEnvRejectsMalformedValues(t *testing.T) {
	for name, env := range map[string][2]string{
		"pool size": {EnvPoolSize, "many"},
		"duration":  {EnvReadTimeout, "quickly"},
		"boolean":   {EnvTLS, "maybe"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(EnvAddr, "redis.internal:6379")
			t.Setenv(EnvPassword, testRedisPassword)
			t.Setenv(env[0], env[1])
			if _, err := ConfigFromEnv(); err == nil {
				t.Fatalf("ConfigFromEnv accepted a malformed %s", name)
			}
		})
	}
}

func TestRedisTLSConfig(t *testing.T) {
	t.Parallel()
	noTLS := prodRedisConfig()
	noTLS.TLS = false
	if tc, err := noTLS.TLSConfig(); err != nil || tc != nil {
		t.Fatalf("TLS off produced a config: %v %v", tc, err)
	}

	tc, err := prodRedisConfig().TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	if tc.MinVersion < 0x0303 {
		t.Fatalf("MinVersion = %x, want at least TLS 1.2", tc.MinVersion)
	}

	bad := prodRedisConfig()
	bad.CACertFile = "/nonexistent/ca.pem"
	if _, err := bad.TLSConfig(); err == nil {
		t.Fatal("TLSConfig accepted a missing CA bundle")
	}
}

func TestNewClientRejectsAnInvalidConfig(t *testing.T) {
	t.Parallel()
	c := prodRedisConfig()
	c.Addr = ""
	if _, err := NewClient(c); err == nil {
		t.Fatal("NewClient accepted an invalid config")
	}
}

// TestIsUnavailableDistinguishesAnAnswerFromAnOutage. Every component in this package branches on
// this: "Redis said no" may be acted on, "Redis did not answer" must fall through.
func TestIsUnavailableDistinguishesAnAnswerFromAnOutage(t *testing.T) {
	t.Parallel()
	if IsUnavailable(nil) {
		t.Error("nil is not an outage")
	}
	if IsUnavailable(goredis.Nil) {
		t.Error("a missing key is an answer, not an outage")
	}
	if !IsUnavailable(errors.New("connection refused")) {
		t.Error("a connection failure is an outage")
	}
}

func TestWrapRedisClassification(t *testing.T) {
	t.Parallel()
	if wrapRedis(nil, "get") != nil {
		t.Error("wrapRedis(nil) is not nil")
	}
	err := wrapRedis(errors.New("connection refused"), "get")
	if !apierror.IsRetryable(err) {
		t.Errorf("a Redis failure must be retryable: %v", err)
	}
	if apierror.CodeOf(err) != apierror.CodeDependencyFailure {
		t.Errorf("code = %s", apierror.CodeOf(err))
	}
}

// TestPoolDefaultsAreDerivedNotInherited pins the reasoning in the Config doc: the pool size is
// set explicitly so a Kubernetes CPU-limit change cannot silently resize it.
func TestPoolDefaultsAreDerivedNotInherited(t *testing.T) {
	t.Parallel()
	c := DefaultConfig()
	if c.PoolSize != 10*maxProcs() {
		t.Errorf("pool size = %d, want 10 x GOMAXPROCS (%d)", c.PoolSize, 10*maxProcs())
	}
	if c.MinIdleConns < 1 {
		t.Error("min idle connections is zero; the first request after an idle period pays a TLS handshake")
	}
	if c.ConnMaxIdleTime <= 0 || c.ConnMaxLifetime <= 0 {
		t.Error("connections must be recycled; a pool of sockets a load balancer silently dropped produces a burst of resets")
	}
}
