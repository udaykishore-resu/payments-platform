package runtime

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/kafka"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/redis"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/secrets"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/internal/platform/health"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// The infrastructure constructors, each a thin adapter from a configuration group to the
// concrete client.
//
// They are here rather than in each main.go for the reason the package comment gives: a pool
// whose statement timeout is set in eight binaries and forgotten in the ninth is a bug that only
// appears under load, in one deployable, months later. What each binary still decides for itself
// is *whether* it opens a pool at all, and in what order relative to everything else — which is
// the part that is architecture rather than mechanism.

// SetupTelemetry initialises logging, tracing and metrics.
//
// It runs before anything else that can fail, so that every later failure is observable. Setting
// up telemetry after the database means a database failure is invisible — you get a process that
// exits 2 with nothing in the log pipeline and a pod that crash-loops for reasons nobody can see.
func SetupTelemetry(ctx context.Context, service string, build BuildInfo, env ServiceEnv,
	plane telemetry.Plane) (*telemetry.Telemetry, error) {
	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(env.LogLevel)); err != nil {
		return nil, apierror.Newf(apierror.CodeConfigurationInvalid,
			"PP_LOG_LEVEL %q is not one of debug, info, warn, error", env.LogLevel)
	}
	ratio := env.TraceSampleRatio
	return telemetry.Setup(ctx, telemetry.Config{
		Service:          service,
		Version:          build.Version,
		Environment:      env.Environment,
		Region:           env.Region,
		Plane:            plane,
		PodName:          env.PodName,
		PodNamespace:     env.PodNamespace,
		OTLPEndpoint:     env.OTLPEndpoint,
		OTLPInsecure:     env.OTLPInsecure,
		TraceSampleRatio: &ratio,
		LogLevel:         level,
	})
}

// OpenPostgres builds the connection pool with this platform's timeouts.
//
// pgxpool performs an eager connectivity check, so a bad DSN or an unreachable writer fails here
// rather than on the first request. That is the whole reason the pool is opened during startup
// and not lazily: failing fast at startup produces one crash-looping pod with a clear error;
// failing lazily produces a pod that reports ready and then errors on live traffic.
func OpenPostgres(ctx context.Context, service string, env PostgresEnv) (*postgres.Pool, error) {
	// The tenant resolver is installed here rather than in each main.go, and installing it is not
	// optional: until it is, every repository method fails closed with MISSING_TENANT_CONTEXT,
	// because internal/infrastructure/postgres deliberately defaults to a resolver that reports
	// no tenant. Binding it to the pool's construction means a binary that has a database also
	// has the one thing that makes the database usable — the alternative, a separate call each
	// composition root remembers, is a call one of them eventually forgets, and the symptom is
	// every request 500ing on a missing tenant that the request plainly carries.
	//
	// It reads the platform's tenant context and nothing else, which is what keeps
	// docs/security.md §16.2's "the tenant has exactly one origin" true: this function can only
	// read what the authentication middleware, the event-envelope decoder or the workflow lease
	// already verified.
	postgres.UseTenantResolver(func(ctx context.Context) (string, bool) {
		t, err := tenantctx.TenantID(ctx)
		if err != nil || t == "" {
			return "", false
		}
		return t.String(), true
	})

	cfg := postgres.DefaultPoolConfig(env.DatabaseURL.Expose(), service)
	cfg.MaxConns = env.MaxConns
	cfg.MinConns = env.MinConns
	cfg.StatementTimeout = env.StatementTimeout
	cfg.LockTimeout = env.LockTimeout
	return postgres.NewPool(ctx, cfg)
}

// OpenRedis builds the accelerator client, or returns nil when no address is configured.
//
// Returning (nil, nil) rather than an error for an absent address is deliberate and is the one
// place in this file where absence is not a failure: Redis is never authoritative on this
// platform (ADR-009), so a deployment without it is a slower deployment, not a broken one. The
// idempotency store falls back to Postgres and the rate limiter to its local bucket.
func OpenRedis(env RedisEnv, environment string) (*redis.Client, error) {
	if strings.TrimSpace(env.RedisAddr) == "" {
		return nil, nil //nolint:nilnil // deliberate and documented above: Redis is never authoritative (ADR-009), so an absent address is a slower deployment, not a broken one
	}
	cfg := redis.DefaultConfig()
	cfg.Addr = env.RedisAddr
	cfg.Username = env.RedisUsername
	cfg.Password = env.RedisPassword
	cfg.DB = env.RedisDB
	cfg.TLS = env.RedisTLS
	cfg.CACertFile = env.RedisCACertFile
	cfg.ServerName = env.RedisTLSServerName
	cfg.PoolSize = env.RedisPoolSize
	cfg.Environment = environment
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return redis.NewClient(cfg)
}

// KafkaConfig assembles the broker configuration.
//
// It validates here rather than at first use because a Kafka misconfiguration that surfaces on the
// first publish is a misconfiguration that surfaces *after* a state change has been committed —
// the outbox row is written, the relay cannot publish it, and the event is late rather than lost.
// Late is survivable; discovering it at startup is better.
func KafkaConfig(clientID string, env KafkaEnv, environment string) (kafka.Config, error) {
	cfg := kafka.DefaultConfig()
	cfg.Brokers = splitList(env.KafkaBrokers)
	cfg.ClientID = clientID
	cfg.Protocol = kafka.SecurityProtocol(env.KafkaProtocol)
	cfg.Mechanism = kafka.SASLMechanism(env.KafkaMech)
	cfg.Username = env.KafkaUsername
	cfg.Password = env.KafkaPassword
	cfg.CACertFile = env.KafkaCAFile
	cfg.Environment = environment
	if err := cfg.Validate(); err != nil {
		return kafka.Config{}, err
	}
	return cfg, nil
}

// splitList parses a comma-separated environment value, dropping empties so a trailing comma is
// not a broker named "".
func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// SplitList exposes the parser for a composition root reading its own comma-separated variable —
// a CORS origin list, a gateway code list — so the parsing rule is one rule.
func SplitList(v string) []string { return splitList(v) }

// NewHealthRegistry builds the probe registry with the platform's cache and timeout defaults.
//
// The cache is what stops a readiness probe from being a load source: with a 5-second TTL and a
// 5-second probe period, a pod issues roughly one `SELECT 1` per period regardless of how many
// probers ask. Without it, the load balancer's health check, Kubernetes' readiness probe and
// Route 53's check each open their own connection every few seconds — which is a measurable
// fraction of a small pool.
func NewHealthRegistry(service, version string) *health.Registry {
	return health.New(health.Options{
		Service:        service,
		Version:        version,
		DefaultTimeout: 2 * time.Second,
		CacheTTL:       5 * time.Second,
	})
}

// RegisterPostgresReadiness gates readiness on the writer being reachable.
//
// Readiness, never liveness. If liveness checked the database, an Aurora failover — a documented,
// expected, ≤60-second event — would fail every pod's liveness probe simultaneously and the
// kubelet would kill the entire fleet, turning a transparent failover into a ten-minute outage
// with cold pools and a thundering herd. Readiness is reversible; liveness is not.
func RegisterPostgresReadiness(reg *health.Registry, pool *postgres.Pool) {
	reg.RegisterReadiness("postgres", true, func(ctx context.Context) error {
		return pool.Ping(ctx)
	})
}

// RegisterRedisReadiness gates readiness on Redis, non-critically.
//
// Non-critical because Redis is an accelerator: a pod that cannot reach it still serves correct
// answers, more slowly. Marking it critical would shed traffic from a fleet that is working, which
// is a self-inflicted outage in defence of a cache.
func RegisterRedisReadiness(reg *health.Registry, client *redis.Client) {
	if client == nil {
		return
	}
	reg.RegisterReadiness("redis", false, func(ctx context.Context) error {
		return client.Health(ctx)
	})
}

// RegisterConfigSnapshotReadiness gates readiness on the configuration snapshot being fresh
// enough to serve payments against.
//
// This is the §15 staleness cliff expressed as a probe. A pod whose snapshot has aged past the
// cliff would fail every payment closed — every merchant looks unknown — so it is better for it
// to leave rotation and let a pod with a fresh snapshot take the traffic.
func RegisterConfigSnapshotReadiness(reg *health.Registry, age func() time.Duration, cliff time.Duration) {
	reg.RegisterReadiness("config-snapshot", true, func(context.Context) error {
		if got := age(); got > cliff {
			return apierror.Newf(apierror.CodeConfigurationStale,
				"the configuration snapshot is %s old, past the %s cliff", got, cliff)
		}
		return nil
	})
}

// RegisterProcessLiveness registers the only kind of check liveness may contain: one that a
// restart can fix.
//
// It is a watchdog counter, not a dependency probe. The distinction is the single most
// consequential probe decision in a Kubernetes deployment, and it is stated at every place a
// liveness check could be added so that adding a dependency here requires ignoring the comment
// rather than not knowing.
func RegisterProcessLiveness(reg *health.Registry) {
	reg.RegisterLiveness("process", func(context.Context) error { return nil })
}

// ParseEnvironment converts the configured environment name, refusing an unknown one.
//
// Refusing rather than defaulting: an unrecognised environment string is a typo in a manifest,
// and defaulting it to sandbox would run production traffic under sandbox rules — accepting test
// tokens, permitting the gateway simulator — while every dashboard said production.
func ParseEnvironment(v string) (shared.Environment, error) {
	env, err := shared.ParseEnvironment(strings.ToLower(strings.TrimSpace(v)))
	if err != nil {
		return "", apierror.Newf(apierror.CodeConfigurationInvalid,
			"PP_ENVIRONMENT %q is not one of sandbox, production", v)
	}
	return env, nil
}

// RefuseSimulatorInProduction is the second of the two guards on the gateway simulator.
//
// The first is the container image's guard stage, which refuses to build a production image for
// the simulator binary at all. This one covers the other direction: a production *payment* binary
// that has been handed `PP_GATEWAY_ENABLE_SIMULATOR=true`, whether by a copied manifest or by a
// deliberate attempt to route live money to a fake gateway.
//
// Two independent guards because the consequence is a payment that reports authorized and moved
// no money — an outcome that reconciles to nothing and is discovered by a customer.
func RefuseSimulatorInProduction(env shared.Environment, enabled bool) error {
	if enabled && env.IsProduction() {
		return apierror.New(apierror.CodeConfigurationInvalid,
			"the gateway simulator cannot be enabled in production: it would report authorizations "+
				"for money that never moved").
			WithDetail(apierror.Detail{
				Field:   "PP_GATEWAY_ENABLE_SIMULATOR",
				Code:    "FORBIDDEN_IN_PRODUCTION",
				Message: "Unset it, or deploy to sandbox.",
				RuleID:  "L0.NO_SIMULATOR_IN_PRODUCTION",
			})
	}
	return nil
}

// ParseCountries converts a comma-separated ISO 3166-1 alpha-2 list, rejecting an unknown code.
//
// Rejecting rather than skipping: a typo in a supported-country list would otherwise silently
// narrow where merchants may be onboarded, and the symptom — one country's applications failing L2
// — reads as a policy decision rather than a configuration error. Failing at startup names the
// code that is wrong.
//
// It lives here rather than in a composition root because it *iterates over domain values*, which
// is precisely what scripts/check-architecture.sh's H2 heuristic forbids under cmd/** — and the
// heuristic is right: a loop over domain types in a wiring file is where logic starts hiding.
func ParseCountries(raw string) ([]shared.Country, error) {
	var out []shared.Country
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		c, err := shared.ParseCountry(strings.ToUpper(part))
		if err != nil {
			return nil, apierror.Newf(apierror.CodeConfigurationInvalid,
				"%q is not an ISO 3166-1 alpha-2 country code", part)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, apierror.New(apierror.CodeConfigurationInvalid,
			"the country list is empty; onboarding would be impossible everywhere")
	}
	return out, nil
}

// OpenSecrets builds the credential store this deployment should use.
//
// # Why the composition roots call one function rather than choosing a backend each
//
// Nine binaries need a secrets provider and they must all make the same choice. A backend
// selected correctly in eight composition roots and wrongly in the ninth is a defect that appears
// only in the deployable nobody exercises locally, and it appears as a credential outage on the
// money path. The same argument that puts OpenPostgres here puts this here.
//
// The one decision that is never made implicitly is running the file backend in production:
// automatic selection never picks it there, and an explicit `file` still has to pass the file
// provider's own refusal, which names every control being given up.
func OpenSecrets(env SecretsEnv, environment shared.Environment, log *slog.Logger) (ports.SecretsProvider, error) {
	backend, err := secrets.ParseBackend(env.SecretsBackend)
	if err != nil {
		return nil, err
	}
	provider, err := secrets.New(secrets.Config{
		Backend:               backend,
		Environment:           environment,
		Region:                env.AWSRegion,
		Endpoint:              env.SecretsEndpoint,
		CacheTTL:              env.SecretsCacheTTL,
		Path:                  env.SecretsFile,
		AllowFileInProduction: env.SecretsAllowFileInProduction,
		Logger:                log,
	})
	if err != nil {
		return nil, err
	}
	return provider, nil
}

// SecretReferenceCount reports how many references a file-backed provider loaded, or -1 when the
// provider does not know.
//
// It exists for the startup smoke assertion: a process that came up against an empty credential
// document serves payments that all fail on credential resolution, and that is far better said in
// the startup log than discovered from the first merchant. The AWS backend legitimately cannot
// answer — enumerating a merchant estate's secrets at startup would be a ListSecrets storm and an
// IAM permission this platform deliberately does not grant — so it reports -1 rather than 0,
// because "unknown" and "none" call for different reactions.
func SecretReferenceCount(p ports.SecretsProvider) int {
	if f, ok := p.(*secrets.FileProvider); ok {
		return len(f.References())
	}
	return -1
}
