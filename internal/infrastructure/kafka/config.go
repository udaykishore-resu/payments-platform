// Package kafka is the broker adapter behind ports.EventPublisher and ports.EventConsumer.
//
// Everything in here is shaped by three facts from docs/events.md that are easy to nod at and
// hard to implement:
//
//   - Delivery is at-least-once and the business effect is effectively-once. That is not a
//     compromise, it is the only combination achievable across a process and a broker boundary,
//     and every knob below is set to make duplicates cheap and losses impossible rather than the
//     other way round.
//   - Ordering exists per partition key only. The producer preserves it across retries; the
//     consumer preserves it across concurrency; the retry tiers deliberately break it, and the
//     three defences for that are documented where they are relied on.
//   - The system of record is Postgres. Kafka is transport. A topic that loses a record is an
//     incident; a topic that loses a record we can re-derive from the outbox is a delay.
//
// The envelope, the registry and the outbox message shape live in internal/events; this package
// deals only in ports.OutboxMessage and bytes.
package kafka

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// SecurityProtocol is the transport and authentication combination.
type SecurityProtocol string

const (
	// ProtocolPlaintext is for a local development broker only. VerifyConfig rejects it when the
	// environment is not local, because "it worked in dev" is how an unauthenticated listener
	// reaches a VPC.
	ProtocolPlaintext SecurityProtocol = "PLAINTEXT"
	// ProtocolSSL is TLS with no SASL — mutual TLS deployments.
	ProtocolSSL SecurityProtocol = "SSL"
	// ProtocolSASLSSL is the production setting: TLS transport, SASL authentication.
	ProtocolSASLSSL SecurityProtocol = "SASL_SSL"
	// ProtocolSASLPlaintext exists because MSK IAM and some test harnesses use it. It is
	// rejected outside local environments for the same reason as PLAINTEXT.
	ProtocolSASLPlaintext SecurityProtocol = "SASL_PLAINTEXT"
)

// SASLMechanism names the SASL mechanism.
type SASLMechanism string

const (
	// MechanismPlain sends the credential in the SASL exchange. Only acceptable inside TLS,
	// which is why it is paired with SASL_SSL and rejected under SASL_PLAINTEXT.
	MechanismPlain SASLMechanism = "PLAIN"
	// MechanismScramSHA256 is challenge-response; the password never crosses the wire.
	MechanismScramSHA256 SASLMechanism = "SCRAM-SHA-256"
	// MechanismScramSHA512 is the default for production.
	MechanismScramSHA512 SASLMechanism = "SCRAM-SHA-512"
)

// Config is the connection configuration shared by the producer, the consumer and the admin
// client.
//
// The password is a secret.Secret, not a string. That single type choice is what makes a
// `log.Info("kafka config", "cfg", cfg)` safe: fmt consults the Secret's Format method for every
// verb, so the credential renders as [REDACTED] even through a third-party library we do not
// control. A bare string here would be one careless log line away from a credential in Loki with
// a 400-day retention.
type Config struct {
	// Brokers is the seed list. Two or three entries in different AZs, not one: a single seed
	// that is down makes the client fail to bootstrap even though the cluster is healthy.
	Brokers []string
	// ClientID appears in broker logs and quota assignment. Set it to the deployable name so a
	// broker-side quota or a noisy-client investigation can name the culprit.
	ClientID string

	Protocol  SecurityProtocol
	Mechanism SASLMechanism
	Username  string
	Password  secret.Secret[string]

	// CACertFile is a PEM bundle for a private CA. Empty means the system pool.
	CACertFile string
	// ServerName overrides the TLS SNI/verification name, for the case where brokers are reached
	// through an address that does not match their certificate.
	ServerName string
	// InsecureSkipVerify disables certificate verification. It exists so that the only way to
	// turn verification off is to set a field named InsecureSkipVerify, which shows up in a code
	// review. VerifyConfig refuses it outside a local environment.
	InsecureSkipVerify bool

	// DialTimeout bounds the TCP+TLS handshake.
	DialTimeout time.Duration
	// RequestTimeout bounds a single broker request.
	RequestTimeout time.Duration
	// RetryTimeout bounds how long the client retries a retryable broker error internally before
	// surfacing it. Kept well under the outbox relay's stale-claim window (30 s) so that a relay
	// crash-recovery re-claim never races an in-flight produce.
	RetryTimeout time.Duration

	// Environment is "local", "sandbox" or "production". It gates the guards above rather than
	// changing behaviour, so a misconfiguration fails at startup instead of at the first
	// unauthenticated connection.
	Environment string
}

// DefaultConfig returns the production-shaped defaults with no brokers or credentials.
func DefaultConfig() Config {
	return Config{
		ClientID:       "payments-platform",
		Protocol:       ProtocolSASLSSL,
		Mechanism:      MechanismScramSHA512,
		DialTimeout:    10 * time.Second,
		RequestTimeout: 10 * time.Second,
		RetryTimeout:   20 * time.Second,
		Environment:    "production",
	}
}

// Environment variable names. One place, so that a deployment manifest and this code cannot
// disagree about whether it is KAFKA_BROKERS or KAFKA_BOOTSTRAP_SERVERS.
const (
	EnvBrokers        = "KAFKA_BROKERS"
	EnvClientID       = "KAFKA_CLIENT_ID"
	EnvProtocol       = "KAFKA_SECURITY_PROTOCOL"
	EnvMechanism      = "KAFKA_SASL_MECHANISM"
	EnvUsername       = "KAFKA_SASL_USERNAME"
	EnvPassword       = "KAFKA_SASL_PASSWORD"
	EnvCACertFile     = "KAFKA_TLS_CA_FILE"
	EnvServerName     = "KAFKA_TLS_SERVER_NAME"
	EnvInsecure       = "KAFKA_TLS_INSECURE_SKIP_VERIFY"
	EnvDialTimeout    = "KAFKA_DIAL_TIMEOUT"
	EnvRequestTimeout = "KAFKA_REQUEST_TIMEOUT"
	EnvRetryTimeout   = "KAFKA_RETRY_TIMEOUT"
	EnvEnvironment    = "PLATFORM_ENVIRONMENT"
)

// ConfigFromEnv builds a Config from the environment and validates it.
//
// It reads the password with os.Getenv and immediately wraps it, so the plaintext exists as a
// bare string for exactly one statement. It does not log what it read, not even the variable
// names that were present, because "KAFKA_SASL_PASSWORD was set" plus a timestamp is already an
// operational disclosure.
func ConfigFromEnv() (Config, error) {
	cfg := DefaultConfig()

	if v := os.Getenv(EnvBrokers); v != "" {
		cfg.Brokers = splitAndTrim(v)
	}
	if v := os.Getenv(EnvClientID); v != "" {
		cfg.ClientID = v
	}
	if v := os.Getenv(EnvProtocol); v != "" {
		cfg.Protocol = SecurityProtocol(strings.ToUpper(strings.TrimSpace(v)))
	}
	if v := os.Getenv(EnvMechanism); v != "" {
		cfg.Mechanism = SASLMechanism(strings.ToUpper(strings.TrimSpace(v)))
	}
	cfg.Username = os.Getenv(EnvUsername)
	cfg.Password = secret.New(os.Getenv(EnvPassword))
	cfg.CACertFile = os.Getenv(EnvCACertFile)
	cfg.ServerName = os.Getenv(EnvServerName)
	if v := os.Getenv(EnvEnvironment); v != "" {
		cfg.Environment = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv(EnvInsecure); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, configErr(EnvInsecure, "must be a boolean")
		}
		cfg.InsecureSkipVerify = b
	}
	for _, d := range []struct {
		name string
		into *time.Duration
	}{
		{EnvDialTimeout, &cfg.DialTimeout},
		{EnvRequestTimeout, &cfg.RequestTimeout},
		{EnvRetryTimeout, &cfg.RetryTimeout},
	} {
		if v := os.Getenv(d.name); v != "" {
			parsed, err := time.ParseDuration(v)
			if err != nil {
				return Config{}, configErr(d.name, "must be a Go duration, e.g. 10s")
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
//
// The guards that matter are the ones that would otherwise fail silently: an unauthenticated
// connection in production works perfectly until an auditor asks about it, and a disabled
// certificate check works perfectly until someone is between us and the broker.
func (c Config) Validate() error {
	var details []apierror.Detail
	bad := func(field, msg string) {
		details = append(details, apierror.Detail{
			Field: field, Code: "KAFKA_CONFIG_INVALID", Message: msg, RuleID: "L4.KAFKA_CONFIG_VALID",
		})
	}

	if len(c.Brokers) == 0 {
		bad(EnvBrokers, "at least one seed broker is required")
	}
	for _, b := range c.Brokers {
		if !strings.Contains(b, ":") {
			bad(EnvBrokers, "each seed broker must be host:port, got "+b)
		}
	}
	if c.ClientID == "" {
		bad(EnvClientID, "a client id is required so broker-side quotas and logs can name this process")
	}

	local := c.isLocal()

	switch c.Protocol {
	case ProtocolSASLSSL, ProtocolSSL:
	case ProtocolPlaintext, ProtocolSASLPlaintext:
		if !local {
			bad(EnvProtocol, string(c.Protocol)+" is only permitted when every broker is on this host; "+
				"anything reachable over a network is SASL_SSL")
		}
	default:
		bad(EnvProtocol, "must be one of PLAINTEXT, SSL, SASL_PLAINTEXT, SASL_SSL")
	}

	if c.usesSASL() {
		switch c.Mechanism {
		case MechanismPlain:
			if c.Protocol == ProtocolSASLPlaintext && !local {
				bad(EnvMechanism, "PLAIN sends the credential in the clear without TLS; use SASL_SSL or SCRAM")
			}
		case MechanismScramSHA256, MechanismScramSHA512:
		default:
			bad(EnvMechanism, "must be PLAIN, SCRAM-SHA-256 or SCRAM-SHA-512")
		}
		if c.Username == "" {
			bad(EnvUsername, "a SASL username is required")
		}
		if c.Password.Expose() == "" {
			// The message names the variable, never the value.
			bad(EnvPassword, "a SASL password is required")
		}
	}

	if c.InsecureSkipVerify && !local {
		bad(EnvInsecure, "certificate verification may not be disabled for a broker reachable over a network")
	}
	if c.DialTimeout <= 0 || c.RequestTimeout <= 0 || c.RetryTimeout <= 0 {
		bad(EnvRequestTimeout, "timeouts must be positive; an unbounded broker call wedges the relay")
	}
	if c.RetryTimeout >= 30*time.Second {
		bad(EnvRetryTimeout, "must stay below the outbox stale-claim window (30s) so a re-claim cannot race an in-flight produce")
	}

	if len(details) == 0 {
		return nil
	}
	return apierror.New(apierror.CodeConfigurationInvalid, "kafka configuration is invalid").
		WithDetails(details...)
}

// isLocal reports whether every broker in this configuration is on this host, which is the only
// condition under which PLAINTEXT, SASL_PLAINTEXT and a skipped certificate check are permitted.
//
// This function had the same defect as its counterpart in internal/infrastructure/redis, and the
// two must stay in step. It compared the environment's *name* against "local", "test",
// "development" and "dev", none of which runtime.ParseEnvironment can produce — it accepts only
// "sandbox" and "production", and runtime.KafkaConfig assigns that straight onto
// Config.Environment. So the branch was unreachable on any running service, and a developer
// pointing a service at a plaintext Redpanda on their laptop was told PLAINTEXT is "only
// permitted in a local environment" with no way to be in one.
//
// The rule now tests what the control is for: credentials and event payloads must not cross a
// network in the clear. A broker on a loopback address is not on a network. Every broker must
// qualify — one remote broker in the list means the connection leaves the host, and a list that
// is empty is not local, because "no brokers" must never be the thing that unlocks plaintext.
//
// Production is refused whatever the addresses are, for the same reason as in the Redis adapter:
// a loopback broker in production is an architecture decision, not a validator's to make.
func (c Config) isLocal() bool {
	if strings.EqualFold(strings.TrimSpace(c.Environment), "production") {
		return false
	}
	if len(c.Brokers) == 0 {
		return false
	}
	for _, b := range c.Brokers {
		if !brokerIsLoopback(b) {
			return false
		}
	}
	return true
}

// brokerIsLoopback reports whether a host:port names this machine.
//
// A hostname other than "localhost" is not resolved, deliberately. Resolving would make the
// answer depend on DNS at validation time, so a name pointing at 127.0.0.1 on a laptop and at a
// shared broker in a cluster would validate identically — and this is a control deciding whether
// credentials may cross a network in the clear.
func brokerIsLoopback(broker string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(broker))
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func (c Config) usesSASL() bool {
	return c.Protocol == ProtocolSASLSSL || c.Protocol == ProtocolSASLPlaintext
}

func (c Config) usesTLS() bool {
	return c.Protocol == ProtocolSASLSSL || c.Protocol == ProtocolSSL
}

// TLSConfig builds the tls.Config, loading a private CA bundle when one is configured.
//
// MinVersion is pinned to TLS 1.2 rather than left to the Go default because the default is a
// moving target across releases and a broker fleet upgrade should not be able to silently
// negotiate downwards.
func (c Config) TLSConfig() (*tls.Config, error) {
	if !c.usesTLS() {
		return nil, nil //nolint:nilnil // a nil *tls.Config is how "this protocol does not use TLS" is spelled to the client builder; there is no failure to report
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
		return nil, apierror.Wrapf(err, apierror.CodeConfigurationInvalid,
			"reading kafka CA bundle %s", c.CACertFile)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, apierror.Newf(apierror.CodeConfigurationInvalid,
			"kafka CA bundle %s contains no usable certificates", c.CACertFile)
	}
	t.RootCAs = pool
	return t, nil
}

// Redacted returns a loggable rendering of the configuration.
//
// It exists so there is an obvious right thing to log. The password is omitted entirely rather
// than rendered as [REDACTED]: including the placeholder invites someone to ask what it would
// take to see the real value, and the field's presence tells an operator nothing they need.
func (c Config) Redacted() string {
	return fmt.Sprintf(
		"kafka{brokers=%v clientID=%s protocol=%s mechanism=%s username=%s tls=%t caFile=%s env=%s}",
		c.Brokers, c.ClientID, c.Protocol, c.Mechanism, c.Username, c.usesTLS(), c.CACertFile, c.Environment)
}

// String is Redacted, so that a Config reaching any %v path is safe by default.
func (c Config) String() string { return c.Redacted() }

func splitAndTrim(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func configErr(field, msg string) error {
	return apierror.Newf(apierror.CodeConfigurationInvalid, "kafka configuration is invalid").
		WithDetail(apierror.Detail{
			Field: field, Code: "KAFKA_CONFIG_INVALID", Message: msg, RuleID: "L4.KAFKA_CONFIG_VALID",
		})
}

// ClientOptions renders the connection half of a franz-go client configuration.
//
// It is separate from the producer and consumer option sets so that all three clients — producer,
// consumer, admin — authenticate identically. A drift there produces the worst kind of bug: two
// of the three connect, and the third fails only in the environment where the credential differs.
func (c Config) ClientOptions() ([]kgo.Opt, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	opts := []kgo.Opt{
		kgo.SeedBrokers(c.Brokers...),
		kgo.ClientID(c.ClientID),
		kgo.RequestTimeoutOverhead(c.RequestTimeout),
		kgo.RetryTimeout(c.RetryTimeout),
		// DialTimeout, not Dialer.
		//
		// The obvious spelling — kgo.Dialer with a net.Dialer carrying the timeout — cannot be
		// combined with DialTLSConfig: franz-go's own validation refuses a client that sets both
		// ("cannot set both Dialer and DialTLSConfig"), and it refuses it at construction, so the
		// failure is "creating producer client" with no mention of dialers. This code did set
		// both, on the belief that the later option replaced the earlier one. It does not; they
		// are separate fields and setting each is what trips the check. The effect was that every
		// TLS-using protocol — SSL and SASL_SSL, which is to say every production configuration —
		// could not construct a client at all.
		//
		// DialTimeout is the option franz-go documents as the companion to DialTLSConfig, and it
		// applies equally to the plaintext path, so there is one spelling for both.
		kgo.DialTimeout(c.DialTimeout),
	}

	if c.usesTLS() {
		t, err := c.TLSConfig()
		if err != nil {
			return nil, err
		}
		opts = append(opts, kgo.DialTLSConfig(t))
	}

	if c.usesSASL() {
		mech, err := c.saslMechanism()
		if err != nil {
			return nil, err
		}
		opts = append(opts, kgo.SASL(mech))
	}
	return opts, nil
}

// saslMechanism builds the SASL mechanism, exposing the password exactly once, on the line that
// consumes it.
func (c Config) saslMechanism() (sasl.Mechanism, error) {
	switch c.Mechanism {
	case MechanismPlain:
		return plain.Auth{User: c.Username, Pass: c.Password.Expose()}.AsMechanism(), nil
	case MechanismScramSHA256:
		return scram.Auth{User: c.Username, Pass: c.Password.Expose()}.AsSha256Mechanism(), nil
	case MechanismScramSHA512:
		return scram.Auth{User: c.Username, Pass: c.Password.Expose()}.AsSha512Mechanism(), nil
	default:
		return nil, configErr(EnvMechanism, "unsupported SASL mechanism "+string(c.Mechanism))
	}
}
