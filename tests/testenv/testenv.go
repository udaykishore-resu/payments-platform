// Package testenv is the shared harness for the cross-cutting suites in tests/.
//
// It exists so that every suite discovers its dependencies the same way and, crucially, *skips*
// the same way. A test that silently passes because a service was missing is worse than one that
// fails: it makes the suite's green a statement about the runner's environment rather than about
// the system. So every accessor here either returns a working dependency or calls t.Skip with a
// message that names the environment variable and the command that would provide it.
//
// The package is deliberately untagged. The tagged suites (integration, e2e, chaos) import it,
// and so does the untagged contract suite; keeping it tag-free means one compile of the harness
// covers all four and a break in it is caught by the cheapest CI stage rather than the slowest.
package testenv

import (
	"os"
	"strings"
	"testing"
)

// Environment variables. These are the entire configuration surface of the suite: there is no
// config file and no flag, because a test harness with two ways to be configured has one way to
// be configured wrongly.
const (
	// EnvPostgresDSN is the primary DSN. PP_TEST_DATABASE_URL is accepted as a fallback so this
	// suite and the older internal/infrastructure/postgres suite can share one exported variable.
	EnvPostgresDSN = "PP_TEST_POSTGRES_DSN"
	// EnvPostgresDSNAlt is the name the repository's existing integration tests already use.
	EnvPostgresDSNAlt = "PP_TEST_DATABASE_URL"
	// EnvScratchDSN points at a database that migration_test.go may migrate all the way down.
	// It is separate because "down every migration" destroys the schema, and doing that to the
	// database the rest of the suite is using would turn one test failure into forty.
	EnvScratchDSN = "PP_TEST_POSTGRES_SCRATCH_DSN"

	// EnvRedisAddr is host:port for the Redis accelerator.
	EnvRedisAddr = "PP_TEST_REDIS_ADDR"
	// EnvKafkaBrokers is a comma-separated broker list.
	EnvKafkaBrokers = "PP_TEST_KAFKA_BROKERS"

	// EnvBaseURL is the data-plane HTTP base URL for the e2e suite, e.g. http://localhost:8080.
	EnvBaseURL = "PP_TEST_BASE_URL"
	// EnvControlURL is the control-plane HTTP base URL. Defaults to EnvBaseURL when unset,
	// because the local compose stack fronts both behind one gateway.
	EnvControlURL = "PP_TEST_CONTROL_URL"
	// EnvSimulatorURL is the gateway simulator's base URL, used by e2e and chaos to script
	// gateway behaviour and to count charges at the far end.
	EnvSimulatorURL = "PP_TEST_SIMULATOR_URL"
	// EnvAuthToken is a bearer token with the scopes the e2e suite needs.
	EnvAuthToken = "PP_TEST_AUTH_TOKEN"
	// EnvTenantID is the tenant the e2e token is scoped to. The e2e suite cannot invent one:
	// it drives the system from outside, and outside, tenancy comes from the token.
	EnvTenantID = "PP_TEST_TENANT_ID"

	// EnvChaosInfra opts the chaos suite into the scenarios that need real infrastructure to be
	// stopped and started. Without it the infrastructure scenarios skip and only the
	// in-process port-decorator scenarios run, which is the right default: a nightly job may
	// pause containers, a laptop may not.
	EnvChaosInfra = "PP_TEST_CHAOS_INFRA"
)

// lookup returns a trimmed environment value and whether it was set to something non-blank.
func lookup(name string) (string, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	return v, v != ""
}

// Skip reports the missing dependency in the one form that is actually actionable: the variable
// name, what it is for, and the command that would supply it.
func Skip(t testing.TB, env, what, how string) {
	t.Helper()
	t.Skipf("%s is not set — this test needs %s. %s", env, what, how)
}

const howLocalStack = "Run scripts/dev-up.sh and export the variables it prints, or point the variable at an existing service."

// PostgresDSN returns the DSN or skips.
func PostgresDSN(t testing.TB) string {
	t.Helper()
	if v, ok := lookup(EnvPostgresDSN); ok {
		return v
	}
	if v, ok := lookup(EnvPostgresDSNAlt); ok {
		return v
	}
	Skip(t, EnvPostgresDSN, "a real PostgreSQL 15+ instance", howLocalStack+
		" ("+EnvPostgresDSNAlt+" is accepted as a fallback.)")
	return ""
}

// ScratchDSN returns a DSN whose schema the caller may destroy, or skips.
func ScratchDSN(t testing.TB) string {
	t.Helper()
	if v, ok := lookup(EnvScratchDSN); ok {
		return v
	}
	Skip(t, EnvScratchDSN, "a throwaway PostgreSQL database it may migrate fully down and back up",
		"Create an empty database and point "+EnvScratchDSN+" at it. It must NOT be the database "+EnvPostgresDSN+" names.")
	return ""
}

// RedisAddr returns host:port or skips.
func RedisAddr(t testing.TB) string {
	t.Helper()
	if v, ok := lookup(EnvRedisAddr); ok {
		return v
	}
	Skip(t, EnvRedisAddr, "a real Redis 7 instance", howLocalStack)
	return ""
}

// KafkaBrokers returns the broker list or skips.
func KafkaBrokers(t testing.TB) []string {
	t.Helper()
	v, ok := lookup(EnvKafkaBrokers)
	if !ok {
		Skip(t, EnvKafkaBrokers, "a real Kafka or Redpanda cluster", howLocalStack)
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		Skip(t, EnvKafkaBrokers, "a real Kafka or Redpanda cluster", howLocalStack)
	}
	return out
}

// BaseURL returns the data-plane base URL or skips.
func BaseURL(t testing.TB) string {
	t.Helper()
	if v, ok := lookup(EnvBaseURL); ok {
		return strings.TrimRight(v, "/")
	}
	Skip(t, EnvBaseURL, "the full stack running and reachable over HTTP",
		"Run `make up` (docker compose) and export "+EnvBaseURL+"=http://localhost:8080.")
	return ""
}

// ControlURL returns the control-plane base URL, defaulting to BaseURL.
func ControlURL(t testing.TB) string {
	t.Helper()
	if v, ok := lookup(EnvControlURL); ok {
		return strings.TrimRight(v, "/")
	}
	return BaseURL(t)
}

// SimulatorURL returns the gateway simulator base URL or skips.
func SimulatorURL(t testing.TB) string {
	t.Helper()
	if v, ok := lookup(EnvSimulatorURL); ok {
		return strings.TrimRight(v, "/")
	}
	Skip(t, EnvSimulatorURL, "the gateway simulator running and reachable over HTTP",
		"Run `make up` and export "+EnvSimulatorURL+"=http://localhost:9090.")
	return ""
}

// AuthToken returns a bearer token or skips.
func AuthToken(t testing.TB) string {
	t.Helper()
	if v, ok := lookup(EnvAuthToken); ok {
		return v
	}
	Skip(t, EnvAuthToken, "a bearer token carrying the payments:write and merchants:write scopes",
		"Run `scripts/dev-token.sh` and export the value it prints.")
	return ""
}

// TenantID returns the tenant the e2e token is scoped to, or skips.
func TenantID(t testing.TB) string {
	t.Helper()
	if v, ok := lookup(EnvTenantID); ok {
		return v
	}
	Skip(t, EnvTenantID, "the tenant id the "+EnvAuthToken+" token is scoped to",
		"Export the tenant id printed by scripts/dev-up.sh.")
	return ""
}

// ChaosInfraEnabled reports whether the destructive infrastructure faults may run.
func ChaosInfraEnabled() bool {
	v, ok := lookup(EnvChaosInfra)
	return ok && (v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes"))
}

// RequireChaosInfra skips unless the runner has opted into destructive faults.
func RequireChaosInfra(t testing.TB) {
	t.Helper()
	if !ChaosInfraEnabled() {
		Skip(t, EnvChaosInfra, "permission to stop and start real infrastructure (Postgres, Redis, Kafka)",
			"Export "+EnvChaosInfra+"=1 on a runner where pausing those containers is acceptable.")
	}
}
