// Package redis is the adapter behind the platform's cache, distributed lock, rate-limiter
// backend, velocity counters and the idempotency read-through accelerator.
//
// One sentence governs everything in this package: **Redis is never authoritative on the money
// path.** Postgres is (ADR-009). Every type here is designed so that a total Redis outage
// degrades latency and nothing else — a cache miss falls through to the origin, a lock that
// cannot be taken means the work runs anyway or is skipped by a scheduler that will run again, a
// rate limiter that cannot decide falls back to a local bucket, and the idempotency accelerator
// simply stops accelerating. If you find yourself writing a branch here whose absence would
// change a payment's outcome, the branch is in the wrong package.
package redis

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Environment variable names.
const (
	EnvAddr           = "REDIS_ADDR"
	EnvUsername       = "REDIS_USERNAME"
	EnvPassword       = "REDIS_PASSWORD"
	EnvDB             = "REDIS_DB"
	EnvTLS            = "REDIS_TLS"
	EnvCACertFile     = "REDIS_TLS_CA_FILE"
	EnvServerName     = "REDIS_TLS_SERVER_NAME"
	EnvInsecure       = "REDIS_TLS_INSECURE_SKIP_VERIFY"
	EnvPoolSize       = "REDIS_POOL_SIZE"
	EnvMinIdleConns   = "REDIS_MIN_IDLE_CONNS"
	EnvDialTimeout    = "REDIS_DIAL_TIMEOUT"
	EnvReadTimeout    = "REDIS_READ_TIMEOUT"
	EnvWriteTimeout   = "REDIS_WRITE_TIMEOUT"
	EnvPoolTimeout    = "REDIS_POOL_TIMEOUT"
	EnvEnvironmentVar = "PLATFORM_ENVIRONMENT"
)

// Config is the Redis connection configuration.
//
// As in the Kafka adapter, the password is a secret.Secret rather than a string, so that a
// config struct reaching any log, error or span renders the credential as [REDACTED] even
// through code that has never heard of this type.
type Config struct {
	Addr     string
	Username string
	Password secret.Secret[string]
	DB       int

	TLS                bool
	CACertFile         string
	ServerName         string
	InsecureSkipVerify bool

	// PoolSize is the maximum number of connections.
	//
	// The reasoning, because a pool that is wrong in either direction hurts: every Redis command
	// in this package is a single round trip of well under a millisecond, so a pod does not need
	// one connection per in-flight request — it needs enough that a request never *waits* for
	// one. Redis is single-threaded, so more connections do not buy more Redis throughput;
	// beyond a point they only buy more context switches on the server and more file descriptors
	// on both ends. The size that matters is roughly (peak concurrent Redis calls per pod) with
	// headroom, and for the data plane that is bounded by the HTTP server's own concurrency
	// limit rather than by anything here.
	//
	// 10 x GOMAXPROCS is go-redis's default and is a reasonable starting point; we set it
	// explicitly rather than inheriting it so that changing GOMAXPROCS — which a Kubernetes CPU
	// limit change does silently — cannot quietly resize the pool underneath a running service.
	PoolSize int
	// MinIdleConns keeps warm connections so that a traffic spike does not pay TLS handshakes on
	// the request path. Zero would mean the first request after an idle period is the slowest,
	// which is exactly the request a health check makes.
	MinIdleConns int
	// ConnMaxIdleTime recycles idle connections. Cloud load balancers silently drop idle TCP
	// connections after a few minutes, and a pool full of dead sockets produces a burst of
	// "connection reset" errors at the worst moment.
	ConnMaxIdleTime time.Duration
	// ConnMaxLifetime bounds how long a connection lives, so a failover behind a DNS name is
	// eventually picked up by every connection rather than only by new ones.
	ConnMaxLifetime time.Duration

	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// PoolTimeout is how long a caller waits for a free connection. It is short on purpose: a
	// caller queueing on a Redis connection is a caller adding latency to a request whose whole
	// reason for using Redis was to be fast. Failing fast lets the caller fall through to the
	// origin, which is always safe here.
	PoolTimeout time.Duration
	// MaxRetries is deliberately small. Redis commands in this package are either idempotent
	// (GET, the Lua scripts) or safe to lose (SET on a cache), so a retry is cheap — but a deep
	// retry chain on a Redis that is down turns a 1 ms cache lookup into a multi-second stall on
	// the request path, which is worse than the cache miss it was avoiding.
	MaxRetries int

	Environment string
}

// DefaultConfig returns production-shaped defaults with no address or credential.
func DefaultConfig() Config {
	return Config{
		DB:              0,
		TLS:             true,
		PoolSize:        10 * maxProcs(),
		MinIdleConns:    maxProcs(),
		ConnMaxIdleTime: 5 * time.Minute,
		ConnMaxLifetime: 30 * time.Minute,
		DialTimeout:     3 * time.Second,
		ReadTimeout:     500 * time.Millisecond,
		WriteTimeout:    500 * time.Millisecond,
		PoolTimeout:     1 * time.Second,
		MaxRetries:      2,
		Environment:     "production",
	}
}

// ConfigFromEnv builds a Config from the environment and validates it.
func ConfigFromEnv() (Config, error) {
	cfg := DefaultConfig()
	cfg.Addr = os.Getenv(EnvAddr)
	cfg.Username = os.Getenv(EnvUsername)
	cfg.Password = secret.New(os.Getenv(EnvPassword))
	cfg.CACertFile = os.Getenv(EnvCACertFile)
	cfg.ServerName = os.Getenv(EnvServerName)
	if v := os.Getenv(EnvEnvironmentVar); v != "" {
		cfg.Environment = strings.ToLower(strings.TrimSpace(v))
	}

	for _, b := range []struct {
		name string
		into *bool
	}{
		{EnvTLS, &cfg.TLS},
		{EnvInsecure, &cfg.InsecureSkipVerify},
	} {
		if v := os.Getenv(b.name); v != "" {
			parsed, err := strconv.ParseBool(v)
			if err != nil {
				return Config{}, configErr(b.name, "must be a boolean")
			}
			*b.into = parsed
		}
	}
	for _, n := range []struct {
		name string
		into *int
	}{
		{EnvDB, &cfg.DB},
		{EnvPoolSize, &cfg.PoolSize},
		{EnvMinIdleConns, &cfg.MinIdleConns},
	} {
		if v := os.Getenv(n.name); v != "" {
			parsed, err := strconv.Atoi(v)
			if err != nil {
				return Config{}, configErr(n.name, "must be an integer")
			}
			*n.into = parsed
		}
	}
	for _, d := range []struct {
		name string
		into *time.Duration
	}{
		{EnvDialTimeout, &cfg.DialTimeout},
		{EnvReadTimeout, &cfg.ReadTimeout},
		{EnvWriteTimeout, &cfg.WriteTimeout},
		{EnvPoolTimeout, &cfg.PoolTimeout},
	} {
		if v := os.Getenv(d.name); v != "" {
			parsed, err := time.ParseDuration(v)
			if err != nil {
				return Config{}, configErr(d.name, "must be a Go duration, e.g. 500ms")
			}
			*d.into = parsed
		}
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects a configuration that would start but be wrong.
func (c Config) Validate() error {
	var details []apierror.Detail
	bad := func(field, msg string) {
		details = append(details, apierror.Detail{
			Field: field, Code: "REDIS_CONFIG_INVALID", Message: msg, RuleID: "L4.REDIS_CONFIG_VALID",
		})
	}
	local := c.isLocal()

	if c.Addr == "" {
		bad(EnvAddr, "an address is required")
	} else if !strings.Contains(c.Addr, ":") {
		bad(EnvAddr, "must be host:port")
	}
	if c.DB < 0 {
		bad(EnvDB, "must not be negative")
	}
	if !c.TLS && !local {
		// The velocity counters and the idempotency accelerator carry tenant and merchant
		// identifiers. Those are not secrets, but they are tenant data crossing a network, and
		// "it is only a cache" is how an unencrypted channel gets justified.
		bad(EnvTLS, "TLS may not be disabled outside a local environment")
	}
	if c.InsecureSkipVerify && !local {
		bad(EnvInsecure, "certificate verification may not be disabled outside a local environment")
	}
	if c.PoolSize < 1 {
		bad(EnvPoolSize, "must be at least 1")
	}
	if c.MinIdleConns < 0 || c.MinIdleConns > c.PoolSize {
		bad(EnvMinIdleConns, "must be between 0 and the pool size")
	}
	if c.DialTimeout <= 0 || c.ReadTimeout <= 0 || c.WriteTimeout <= 0 || c.PoolTimeout <= 0 {
		bad(EnvReadTimeout, "every timeout must be positive; an unbounded Redis call is an unbounded request")
	}
	if c.ReadTimeout > 2*time.Second {
		// A cache that can block a request for seconds is worse than no cache: the fallback path
		// is fast and always available.
		bad(EnvReadTimeout, "must stay at or under 2s; a slow cache must fail fast so the caller falls through to the origin")
	}

	if len(details) == 0 {
		return nil
	}
	return apierror.New(apierror.CodeConfigurationInvalid, "redis configuration is invalid").WithDetails(details...)
}

func (c Config) isLocal() bool {
	e := strings.ToLower(c.Environment)
	return e == "local" || e == "test" || e == "development" || e == "dev"
}

// TLSConfig builds the tls.Config, loading a private CA bundle when configured.
func (c Config) TLSConfig() (*tls.Config, error) {
	if !c.TLS {
		return nil, nil //nolint:nilnil // a nil *tls.Config is how "TLS is off" is spelled to the client builder; there is no failure to report
	}
	t := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         c.ServerName,
		InsecureSkipVerify: c.InsecureSkipVerify, //nolint:gosec // rejected outside local by Validate
	}
	if c.CACertFile == "" {
		return t, nil
	}
	pem, err := os.ReadFile(c.CACertFile)
	if err != nil {
		return nil, apierror.Wrapf(err, apierror.CodeConfigurationInvalid, "reading redis CA bundle %s", c.CACertFile)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, apierror.Newf(apierror.CodeConfigurationInvalid,
			"redis CA bundle %s contains no usable certificates", c.CACertFile)
	}
	t.RootCAs = pool
	return t, nil
}

// Redacted renders the configuration for a log line, omitting the credential entirely.
func (c Config) Redacted() string {
	return "redis{addr=" + c.Addr +
		" db=" + strconv.Itoa(c.DB) +
		" tls=" + strconv.FormatBool(c.TLS) +
		" poolSize=" + strconv.Itoa(c.PoolSize) +
		" env=" + c.Environment + "}"
}

// String is Redacted, so a Config on any %v path is safe by default.
func (c Config) String() string { return c.Redacted() }

// Client is the platform's Redis handle.
//
// It wraps *goredis.Client rather than aliasing it so that every type in this package depends on
// a narrow surface we control, and so that Close and health checking have one obvious owner.
type Client struct {
	rdb *goredis.Client
	cfg Config
}

// UniversalClient is the subset of go-redis this package uses. Declaring it lets the components
// below be constructed against a mock in tests without a server, and keeps the coupling to
// go-redis visible in one place.
type UniversalClient interface {
	goredis.Scripter
	Get(ctx context.Context, key string) *goredis.StringCmd
	Set(ctx context.Context, key string, value any, expiration time.Duration) *goredis.StatusCmd
	SetArgs(ctx context.Context, key string, value any, a goredis.SetArgs) *goredis.StatusCmd
	Del(ctx context.Context, keys ...string) *goredis.IntCmd
	Ping(ctx context.Context) *goredis.StatusCmd
}

// NewClient connects to Redis.
//
// It performs no I/O: go-redis dials lazily, and a service that refused to start because Redis
// was briefly unavailable would be a service whose availability is bounded by its cache's —
// exactly the coupling this package exists to avoid.
func NewClient(cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	tlsCfg, err := cfg.TLSConfig()
	if err != nil {
		return nil, err
	}
	rdb := goredis.NewClient(&goredis.Options{
		Addr:            cfg.Addr,
		Username:        cfg.Username,
		Password:        cfg.Password.Expose(),
		DB:              cfg.DB,
		TLSConfig:       tlsCfg,
		PoolSize:        cfg.PoolSize,
		MinIdleConns:    cfg.MinIdleConns,
		ConnMaxIdleTime: cfg.ConnMaxIdleTime,
		ConnMaxLifetime: cfg.ConnMaxLifetime,
		DialTimeout:     cfg.DialTimeout,
		ReadTimeout:     cfg.ReadTimeout,
		WriteTimeout:    cfg.WriteTimeout,
		PoolTimeout:     cfg.PoolTimeout,
		MaxRetries:      cfg.MaxRetries,
	})
	return &Client{rdb: rdb, cfg: cfg}, nil
}

// Redis exposes the underlying client for the components in this package and for wiring.
func (c *Client) Redis() *goredis.Client { return c.rdb }

// Ping checks liveness.
//
// It is a *liveness* check for Redis, not a readiness gate for the service. A service that
// reports itself unready because its cache is down has converted a latency degradation into an
// outage, which is precisely backwards: the fallback paths in this package are all correct, just
// slower. Wiring should surface this as a gauge and an alert, never as a failed readiness probe.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.rdb.Ping(ctx).Err(); err != nil {
		return apierror.Wrap(err, apierror.CodeDependencyFailure, "redis: unreachable")
	}
	return nil
}

// Health reports whether Redis is reachable within a bounded time. It never blocks longer than
// the configured read timeout plus the dial timeout.
func (c *Client) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout+c.cfg.ReadTimeout)
	defer cancel()
	return c.Ping(ctx)
}

// PoolStats exposes connection-pool counters for the metrics registry. A rising Timeouts count is
// the earliest signal that PoolSize is too small for the pod's concurrency.
func (c *Client) PoolStats() *goredis.PoolStats { return c.rdb.PoolStats() }

// Close releases the pool. It is bounded — go-redis closes connections without waiting for
// in-flight commands beyond their own timeouts — and safe to call more than once.
func (c *Client) Close() error {
	if err := c.rdb.Close(); err != nil && !strings.Contains(err.Error(), "closed") {
		return apierror.Wrap(err, apierror.CodeDependencyFailure, "redis: closing client")
	}
	return nil
}

// IsUnavailable reports whether err means Redis could not answer, as opposed to answering "no".
//
// Every component in this package branches on this rather than on the error's text, because the
// distinction is the whole safety argument: "Redis said no" may be acted on, "Redis did not
// answer" must fall through to the authoritative path.
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, goredis.Nil) {
		// A missing key is an answer, not an outage.
		return false
	}
	return true
}

// maxProcs is GOMAXPROCS, floored at one. The pool default is derived from it explicitly rather
// than inherited from go-redis so that a Kubernetes CPU-limit change cannot silently resize the
// pool underneath a running service.
func maxProcs() int {
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		return 1
	}
	return n
}

func configErr(field, msg string) error {
	return apierror.New(apierror.CodeConfigurationInvalid, "redis configuration is invalid").
		WithDetail(apierror.Detail{
			Field: field, Code: "REDIS_CONFIG_INVALID", Message: msg, RuleID: "L4.REDIS_CONFIG_VALID",
		})
}
