// Package config centralizes all runtime configuration, loaded exclusively from environment
// variables. No secrets live in this struct's defaults — DB credentials and signing keys are
// fetched at runtime from the secrets provider (see internal/observability and deploy/k8s for
// how External Secrets Operator projects AWS Secrets Manager values into the pod's environment).
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all tunables for the payments-api process. Every field has a sane production
// default so the service is safe to run even if an operator forgets to set an optional variable;
// only the DSN and queue URL are required with no default, since defaulting those would risk
// silently pointing at the wrong environment.
type Config struct {
	// HTTP server
	HTTPPort            int
	ReadTimeout         time.Duration
	WriteTimeout        time.Duration
	IdleTimeout         time.Duration
	ShutdownGracePeriod time.Duration

	// Database
	DatabaseDSN        string
	DBMaxConns         int32
	DBMinConns         int32
	DBConnTimeout      time.Duration
	DBStatementTimeout time.Duration

	// Auth
	JWKSURL      string
	JWTAudience  string
	JWTIssuer    string
	AuthDisabled bool // ONLY true in local dev; refused at startup if true and Env == "production"

	// Messaging
	AWSRegion             string
	PaymentEventsQueueURL string
	OutboxPollInterval    time.Duration
	OutboxBatchSize       int
	OutboxMaxAttempts     int

	// Rate limiting
	RateLimitPerClientRPS   float64
	RateLimitPerClientBurst int

	// Circuit breaker
	CircuitBreakerFailureThreshold int
	CircuitBreakerOpenDuration     time.Duration

	// Observability
	Env                  string // "production", "staging", "dev"
	ServiceName          string
	LogLevel             string
	OTELExporterEndpoint string
	MetricsPort          int
}

// Load reads configuration from environment variables, applying production-sane defaults.
// It fails fast (returns an error, never a partially-valid Config) if a required variable is
// missing — a service that starts with an invalid config and fails on the first request is a
// worse failure mode than one that never starts.
func Load() (Config, error) {
	cfg := Config{
		HTTPPort:            envInt("HTTP_PORT", 8080),
		ReadTimeout:         envDuration("HTTP_READ_TIMEOUT", 5*time.Second),
		WriteTimeout:        envDuration("HTTP_WRITE_TIMEOUT", 10*time.Second),
		IdleTimeout:         envDuration("HTTP_IDLE_TIMEOUT", 120*time.Second),
		ShutdownGracePeriod: envDuration("SHUTDOWN_GRACE_PERIOD", 25*time.Second),

		DatabaseDSN:        os.Getenv("DATABASE_DSN"),
		DBMaxConns:         int32(envInt("DB_MAX_CONNS", 20)),
		DBMinConns:         int32(envInt("DB_MIN_CONNS", 2)),
		DBConnTimeout:      envDuration("DB_CONN_TIMEOUT", 3*time.Second),
		DBStatementTimeout: envDuration("DB_STATEMENT_TIMEOUT", 5*time.Second),

		JWKSURL:      os.Getenv("JWKS_URL"),
		JWTAudience:  os.Getenv("JWT_AUDIENCE"),
		JWTIssuer:    os.Getenv("JWT_ISSUER"),
		AuthDisabled: envBool("AUTH_DISABLED", false),

		AWSRegion:             envString("AWS_REGION", "us-east-1"),
		PaymentEventsQueueURL: os.Getenv("PAYMENT_EVENTS_QUEUE_URL"),
		OutboxPollInterval:    envDuration("OUTBOX_POLL_INTERVAL", 200*time.Millisecond),
		OutboxBatchSize:       envInt("OUTBOX_BATCH_SIZE", 50),
		OutboxMaxAttempts:     envInt("OUTBOX_MAX_ATTEMPTS", 10),

		RateLimitPerClientRPS:   envFloat("RATE_LIMIT_PER_CLIENT_RPS", 50),
		RateLimitPerClientBurst: envInt("RATE_LIMIT_PER_CLIENT_BURST", 100),

		CircuitBreakerFailureThreshold: envInt("CIRCUIT_BREAKER_FAILURE_THRESHOLD", 5),
		CircuitBreakerOpenDuration:     envDuration("CIRCUIT_BREAKER_OPEN_DURATION", 10*time.Second),

		Env:                  envString("ENV", "dev"),
		ServiceName:          envString("SERVICE_NAME", "payments-api"),
		LogLevel:             envString("LOG_LEVEL", "info"),
		OTELExporterEndpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		MetricsPort:          envInt("METRICS_PORT", 9090),
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.DatabaseDSN == "" {
		return fmt.Errorf("config: DATABASE_DSN is required")
	}
	if c.Env == "production" {
		if c.AuthDisabled {
			return fmt.Errorf("config: AUTH_DISABLED must not be true in production")
		}
		if c.JWKSURL == "" {
			return fmt.Errorf("config: JWKS_URL is required in production")
		}
		if c.PaymentEventsQueueURL == "" {
			return fmt.Errorf("config: PAYMENT_EVENTS_QUEUE_URL is required in production")
		}
	}
	return nil
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
