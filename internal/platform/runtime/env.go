package runtime

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
)

// The shared configuration groups.
//
// # Why these are here rather than duplicated per binary
//
// Every deployable needs a listen address, a database, and telemetry, and every one of them needs
// them under the *same* variable names — otherwise an operator setting `PP_DATABASE_URL` for one
// pod and `DATABASE_DSN` for another has to remember which is which, and a Helm chart has to
// carry both. Grouping them here makes the names one decision rather than nine.
//
// Each binary's own struct embeds the groups it needs and adds its own fields. What a binary does
// *not* embed, it does not require: `outbox-relay` has no HTTP listener and therefore no
// `PP_HTTP_ADDR`, and asking for one would be a variable an operator must set for a port nothing
// binds.
//
// `required:"true"` is on everything security-relevant and on nothing with a safe default. That
// distinction is the point of the tag: a missing JWKS URL must be a startup failure, never a
// silent fallback to an insecure default, whereas a missing read timeout can safely be 30 s.

// ServiceEnv identifies the process. Every binary embeds it.
type ServiceEnv struct {
	// Environment gates behaviour that must differ between sandbox and production — the
	// environment claim check on a token, whether a gateway simulator may be registered, whether
	// the unit of work asserts a clean tenant GUC. It is required because defaulting it would
	// mean a misconfigured pod defaults to *some* environment, and either default is wrong half
	// the time.
	Environment string `env:"PP_ENVIRONMENT" required:"true"`

	// Region is the deployment region, reported by the probes and used by the residency policy.
	Region string `env:"PP_REGION" required:"true"`

	// LogLevel is the initial level. It is adjustable at runtime through the telemetry handle, so
	// this is a starting point rather than a ceiling.
	LogLevel string `env:"PP_LOG_LEVEL" default:"info"`

	// OTLPEndpoint is the collector. Empty disables export, which is correct for a local run and
	// is why this is not required: a developer without a collector should still get a working
	// binary, and a deployment without one is caught by the absence of traces, loudly.
	OTLPEndpoint string `env:"PP_OTEL_EXPORTER_OTLP_ENDPOINT"`
	OTLPInsecure bool   `env:"PP_OTEL_EXPORTER_OTLP_INSECURE" default:"false"`

	// TraceSampleRatio is the head-sampling ratio. 0.1 keeps a tenth of traces, which is the
	// documented production setting; errors and money-path spans are kept regardless by the
	// sampler's own override.
	TraceSampleRatio float64 `env:"PP_TRACE_SAMPLE_RATIO" default:"0.1"`

	// PodName and PodNamespace come from the downward API and appear on every log line, so a
	// line can be traced to the pod that wrote it after that pod is gone.
	PodName      string `env:"PP_POD_NAME"`
	PodNamespace string `env:"PP_POD_NAMESPACE"`
}

// PostgresEnv is the primary datastore. Binaries that touch persistence embed it.
type PostgresEnv struct {
	// DatabaseURL is the DSN. It is required and it is masked in the startup dump: a DSN
	// conventionally embeds its own credentials, which is why internal/platform/config's secret
	// pattern matches `*_url` for datastore variables specifically and not for URLs generally.
	DatabaseURL secret.Secret[string] `env:"PP_DATABASE_URL" required:"true"`

	// MaxConns and MinConns size the pool. The default of 20 is the documented per-pod ceiling
	// against a writer sized for the fleet: pool size times replica count must stay under the
	// instance's max_connections with headroom for migrations and for a human with psql.
	MaxConns int32 `env:"PP_DATABASE_MAX_CONNS" default:"20"`
	MinConns int32 `env:"PP_DATABASE_MIN_CONNS" default:"2"`

	// StatementTimeout bounds a single statement server-side. It exists so that a pathological
	// query is killed by the database rather than holding a connection until the client's own
	// timeout, which would leak the connection for the duration.
	StatementTimeout time.Duration `env:"PP_DATABASE_STATEMENT_TIMEOUT" default:"5s"`

	// LockTimeout bounds waiting for a lock. Short, because a money-path transaction that cannot
	// take its lock quickly should fail and be retried rather than queue behind a long
	// transaction and blow its latency budget.
	LockTimeout time.Duration `env:"PP_DATABASE_LOCK_TIMEOUT" default:"2s"`
}

// RedisEnv is the accelerator and the distributed rate-limit backend.
//
// Redis is never authoritative on this platform (ADR-009): it is a read-through cache in front of
// Postgres for idempotency, and a token-bucket backend with a local fallback for rate limiting.
// That is why nothing here is required — a process that cannot reach Redis degrades, it does not
// fail.
type RedisEnv struct {
	RedisAddr     string                `env:"PP_REDIS_ADDR"`
	RedisUsername string                `env:"PP_REDIS_USERNAME"`
	RedisPassword secret.Secret[string] `env:"PP_REDIS_PASSWORD"`
	RedisDB       int                   `env:"PP_REDIS_DB" default:"0"`
	RedisTLS      bool                  `env:"PP_REDIS_TLS" default:"true"`
	// RedisCACertFile and RedisTLSServerName configure verification against a private CA, which
	// is the normal case rather than an exotic one: an in-VPC cache presents a certificate from
	// the organisation's own authority, not from a public root. Without a way to supply them the
	// only options are the public trust store — which will not verify — or disabling TLS, and a
	// missing knob that leaves "turn the encryption off" as the only workaround is how the
	// encryption gets turned off.
	RedisCACertFile    string `env:"PP_REDIS_TLS_CA_FILE"`
	RedisTLSServerName string `env:"PP_REDIS_TLS_SERVER_NAME"`
	RedisPoolSize      int    `env:"PP_REDIS_POOL_SIZE" default:"10"`
}

// KafkaEnv is the event backbone.
type KafkaEnv struct {
	KafkaBrokers  string                `env:"PP_KAFKA_BROKERS"`
	KafkaProtocol string                `env:"PP_KAFKA_SECURITY_PROTOCOL" default:"SASL_SSL"`
	KafkaMech     string                `env:"PP_KAFKA_SASL_MECHANISM" default:"SCRAM-SHA-512"`
	KafkaUsername string                `env:"PP_KAFKA_SASL_USERNAME"`
	KafkaPassword secret.Secret[string] `env:"PP_KAFKA_SASL_PASSWORD"`
	KafkaCAFile   string                `env:"PP_KAFKA_TLS_CA_FILE"`
}

// HTTPEnv is a public HTTP listener.
type HTTPEnv struct {
	// HTTPAddr is the public listener. Required: a server with no address is a server that binds
	// a random port and is never reached, which passes every test and fails in production.
	HTTPAddr string `env:"PP_HTTP_ADDR" required:"true"`

	// AdminAddr is the probe and metrics listener, on a separate port so that an ingress that
	// routes the public port cannot reach /metrics by construction.
	AdminAddr string `env:"PP_ADMIN_ADDR" default:":8081"`

	// PublicBaseURL builds Location headers. It comes from configuration rather than from the
	// Host header because trusting Host lets a caller poison the Location of a resource they just
	// created — a redirect primitive handed to an attacker.
	PublicBaseURL string `env:"PP_PUBLIC_BASE_URL" required:"true"`

	// CORSOrigins is a comma-separated exact-match allowlist. Empty denies every cross-origin
	// browser call, which is the correct default for an API whose clients are servers.
	CORSOrigins string `env:"PP_CORS_ALLOWED_ORIGINS"`

	// HSTSMaxAgeSeconds emits Strict-Transport-Security when non-zero. Zero is right behind a
	// TLS-terminating mesh sidecar, where the pod itself speaks plaintext.
	HSTSMaxAgeSeconds int `env:"PP_HSTS_MAX_AGE_SECONDS" default:"0"`

	ReadHeaderTimeout time.Duration `env:"PP_HTTP_READ_HEADER_TIMEOUT" default:"5s"`
	ReadTimeout       time.Duration `env:"PP_HTTP_READ_TIMEOUT" default:"30s"`
	WriteTimeout      time.Duration `env:"PP_HTTP_WRITE_TIMEOUT" default:"35s"`
	IdleTimeout       time.Duration `env:"PP_HTTP_IDLE_TIMEOUT" default:"120s"`
}

// AuthEnv is the token-verification configuration.
//
// Every field is required. There is no default for any of them and that is the single most
// important property of this struct: a defaulted issuer, audience or JWKS URL is an
// authentication control that appears configured and verifies nothing.
type AuthEnv struct {
	// JWKSURL is where signing keys are fetched, cached and refreshed in the background.
	JWKSURL string `env:"PP_AUTH_JWKS_URL" required:"true"`
	// Issuer is the expected `iss`. A token from any other issuer is rejected before its
	// signature is checked, because verifying a signature against a key set we chose based on an
	// untrusted issuer claim is the whole vulnerability.
	Issuer string `env:"PP_AUTH_ISSUER" required:"true"`
	// Audience is the expected `aud`. Without it, a token minted for a different service in the
	// same issuer's realm is accepted here — which is token confusion, and it is silent.
	Audience string `env:"PP_AUTH_AUDIENCE" required:"true"`
	// MaxTokenAge rejects a token older than this regardless of its own expiry, bounding the
	// damage of a leaked long-lived token.
	MaxTokenAge time.Duration `env:"PP_AUTH_MAX_TOKEN_AGE" default:"1h"`
}

// MeshEnv is the mTLS workload-identity configuration for the internal gRPC surface.
type MeshEnv struct {
	// GRPCAddr is the internal listener.
	GRPCAddr string `env:"PP_GRPC_ADDR" default:":9090"`
	// TrustDomain is the SPIFFE trust domain. Required: accepting any trust domain means
	// accepting a certificate from any mesh that can reach the port.
	TrustDomain string `env:"PP_MESH_TRUST_DOMAIN" required:"true"`
	// EnableReflection publishes the service schema. Off by default; see grpcapi.Config.
	EnableReflection bool `env:"PP_GRPC_REFLECTION" default:"false"`
}

// ConfigSnapshotEnv tunes the data plane's cached view of merchant configuration.
type ConfigSnapshotEnv struct {
	// RefreshInterval is how often the snapshot is refreshed from its source.
	RefreshInterval time.Duration `env:"PP_CONFIG_REFRESH_INTERVAL" default:"10s"`
	// BoundedStaleness is the age past which the snapshot is reported unhealthy but is still
	// served — the fail-static window that keeps the data plane running when the control plane is
	// unreachable (baseline §15).
	BoundedStaleness time.Duration `env:"PP_CONFIG_MAX_STALENESS" default:"30s"`
	// MaxStaleness is the cliff: past it, a payment fails rather than being authorized against a
	// configuration nobody can vouch for. The gap between BoundedStaleness and MaxStaleness is
	// deliberately wide, because the failure mode of the cliff is refusing money and the failure
	// mode of the window is a slightly stale routing decision.
	MaxStaleness time.Duration `env:"PP_CONFIG_CLIFF_STALENESS" default:"5m"`
}

// GatewayEnv is the gateway adapter fleet's shared tuning.
type GatewayEnv struct {
	// GatewayTimeout is the hard per-call ceiling from baseline §12 stage 14. It is a *hard*
	// timeout: past it the attempt is TIMEOUT_UNKNOWN and the payment stays PROCESSING, which is
	// survivable, whereas an unbounded call holds a worker and a connection indefinitely.
	GatewayTimeout time.Duration `env:"PP_GATEWAY_TIMEOUT" default:"8s"`
	// EnableSimulator registers the in-process gateway simulator. It is refused outside sandbox
	// by the composition roots that read it, and the container image has its own guard stage.
	EnableSimulator bool `env:"PP_GATEWAY_ENABLE_SIMULATOR" default:"false"`
	// SimulatorBaseURL points the simulator adapter at a `gateway-simulator` process. Empty means
	// the in-process engine.
	SimulatorBaseURL string `env:"PP_GATEWAY_SIMULATOR_URL"`
}

// SecretsEnv selects and configures the credential store.
//
// Every variable here carries a *reference* or a knob, never material. That is not a convention
// this struct happens to follow — docs/security.md §5.2 forbids credential material in an
// environment variable, an admission policy rejects any pod spec whose env var name matches the
// credential pattern, and CI runs the same check against deployments/k8s/** and helm/**. The
// closest thing to a secret here is a file *path*.
type SecretsEnv struct {
	// SecretsBackend is "aws", "file", or empty for automatic (file in sandbox, AWS in
	// production). An unknown value is a startup failure rather than a default, because
	// defaulting a typo in a production manifest would silently select the file backend and the
	// process would come up serving payments against an empty local document.
	SecretsBackend string `env:"PP_SECRETS_BACKEND"`

	// SecretsFile is the file backend's JSON or YAML document of reference → field map.
	SecretsFile string `env:"PP_SECRETS_FILE"`

	// SecretsAllowFileInProduction is the explicit override that lets the file backend run in a
	// production-labelled environment. It exists for an isolated break-glass or disaster-recovery
	// stack where the alternative to a file is not having the platform; see
	// secrets.NewFileProvider for what it gives up.
	SecretsAllowFileInProduction bool `env:"PP_SECRETS_ALLOW_FILE_IN_PRODUCTION" default:"false"`

	// AWSRegion is required by the AWS backend and has no default: a client that guessed its
	// region would sign for the wrong endpoint and fail with a signature error that says nothing
	// about the region.
	AWSRegion string `env:"PP_AWS_REGION"`

	// SecretsEndpoint overrides the derived Secrets Manager endpoint. Production points it at the
	// VPC endpoint so credential reads never traverse the public internet (docs/security.md §5.1).
	SecretsEndpoint string `env:"PP_SECRETS_ENDPOINT"`

	// SecretsCacheTTL is the in-process credential cache. The default is chosen against the
	// rotation overlap window rather than against latency; see secrets.DefaultCacheTTL.
	SecretsCacheTTL time.Duration `env:"PP_SECRETS_CACHE_TTL" default:"60s"`
}

// ShedEnv tunes the adaptive concurrency limiter and the priority shedder.
type ShedEnv struct {
	// ConcurrencyInitial is the limiter's starting in-flight ceiling. It adapts from here; the
	// starting value only matters for the first few seconds after a start.
	ConcurrencyInitial int `env:"PP_CONCURRENCY_INITIAL_LIMIT" default:"64"`
	ConcurrencyMin     int `env:"PP_CONCURRENCY_MIN_LIMIT" default:"8"`
	ConcurrencyMax     int `env:"PP_CONCURRENCY_MAX_LIMIT" default:"512"`
}

// WorkerEnv tunes a background worker's polling.
type WorkerEnv struct {
	// PollInterval is how often the worker looks for work when it found none last time. It is a
	// floor on latency for newly-available work and a ceiling on idle database load, and the
	// default trades the former for the latter at a scale where neither is scarce.
	PollInterval time.Duration `env:"PP_WORKER_POLL_INTERVAL" default:"1s"`
	// BatchSize bounds one claim. Larger batches amortise the round trip and lengthen the unit of
	// work that shutdown must wait for, which is why it is bounded rather than "as many as
	// possible".
	BatchSize int `env:"PP_WORKER_BATCH_SIZE" default:"100"`
	// Concurrency is how many units are processed at once.
	Concurrency int `env:"PP_WORKER_CONCURRENCY" default:"4"`
}
