package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// PoolConfig is the connection pool's tuning surface.
//
// The defaults come from the Little's-law arithmetic in docs/lld.md §4.1, reproduced here
// because a number without its derivation is a number nobody dares change:
//
//	Pods at 5 000 TPS                     = 9
//	Throughput per pod                    = 5 000 / 9        = 556 TPS
//	Database hold time per payment        = 6.5 ms across 3 transactions
//	Required concurrency (Little's law)   = 556 × 0.0065 s   = 3.6 connections, mean
//	Burst factor (p99 arrival × p99 hold) = 4×
//	MaxConns                              = 16
//	MinConns (kept warm)                  = 4
//
// The counter-intuitive part is worth stating plainly: **a pool larger than the concurrency the
// workload needs does not increase throughput, it moves the queue inside the database and makes
// p99 worse.** Sixteen is not a shortage that someone should raise during an incident; it is the
// answer. At the 48-pod HPA ceiling this is 768 client connections, which is far more than an
// Aurora writer should serve directly — PgBouncer in transaction pooling mode multiplexes them
// onto a server-side budget of 200 backends for this deployable.
//
// Transaction pooling is safe here for exactly one reason, and it is a fragile one: no session
// state is carried across transactions. The single piece of session state this system needs —
// `app.tenant_id` — is set with `SET LOCAL` semantics *inside* each transaction, so it is
// reverted at COMMIT before the connection is handed to the next borrower. A plain `SET` would
// leak one tenant's identity to the next borrower of the connection. See uow.go.
type PoolConfig struct {
	// DSN is the libpq connection string. It carries no credentials in a deployed environment:
	// the password is injected by IRSA into the environment and merged by the caller.
	DSN string

	// MaxConns bounds client-side concurrency per pod. See the derivation above.
	MaxConns int32
	// MinConns is kept warm so that a traffic ramp does not pay connection setup on the payment
	// path. Four is one quarter of MaxConns: enough that a burst finds connections ready,
	// few enough that an idle fleet is not holding hundreds of backends open.
	MinConns int32

	// MaxConnLifetime forces periodic rebalancing across Aurora endpoints. Without it,
	// connections stay pinned to the IP of the writer that was current when they were opened,
	// and after a failover the fleet keeps talking to the demoted instance until something
	// errors.
	MaxConnLifetime time.Duration
	// MaxConnLifetimeJitter prevents the whole fleet from recycling at the same instant, which
	// would be a synchronized reconnect storm against a database that is fine.
	MaxConnLifetimeJitter time.Duration
	// MaxConnIdleTime reclaims capacity after a trough.
	MaxConnIdleTime time.Duration
	// HealthCheckPeriod detects a silently dead connection before a request does.
	HealthCheckPeriod time.Duration
	// ConnectTimeout must be short: a slow connect has to fail fast so the retry can reach a
	// healthy endpoint rather than sitting on a dead one for the request's whole budget.
	ConnectTimeout time.Duration

	// StatementTimeout bounds any single statement. A runaway query must not hold one of the
	// 400 server-side backends. Three seconds on the money path; workers and platformctl set
	// their own, larger, values.
	StatementTimeout time.Duration
	// IdleInTransactionTimeout kills the "forgot to commit" bug class before it blocks vacuum
	// and pins a snapshot for hours.
	IdleInTransactionTimeout time.Duration
	// LockTimeout stops a statement from queueing behind an ACCESS EXCLUSIVE lock. A lock
	// request that queues also blocks every reader behind it, which is how one slow DDL becomes
	// a total outage.
	LockTimeout time.Duration

	// ApplicationName appears in pg_stat_activity and in the slow-query log. It is the difference
	// between "some Go process" and "the outbox relay, pod 3".
	ApplicationName string
}

// DefaultPoolConfig returns the money-path sizing from docs/lld.md §4.1 for the given DSN.
//
// Callers that are not on the money path — workflow-worker, platformctl, the reporting reader —
// override StatementTimeout and MaxConns rather than constructing a config from scratch, so that
// the reasoning above stays the baseline and each deviation is visible as a deviation.
func DefaultPoolConfig(dsn, appName string) PoolConfig {
	return PoolConfig{
		DSN:                      dsn,
		MaxConns:                 16,
		MinConns:                 4,
		MaxConnLifetime:          30 * time.Minute,
		MaxConnLifetimeJitter:    5 * time.Minute,
		MaxConnIdleTime:          5 * time.Minute,
		HealthCheckPeriod:        30 * time.Second,
		ConnectTimeout:           3 * time.Second,
		StatementTimeout:         3 * time.Second,
		IdleInTransactionTimeout: 5 * time.Second,
		LockTimeout:              time.Second,
		ApplicationName:          appName,
	}
}

// Pool is the platform's handle on PostgreSQL.
//
// It wraps *pgxpool.Pool rather than exposing it so that the only way to run a statement is
// through a UnitOfWork, which is what guarantees `SET LOCAL app.tenant_id` is in force. A bare
// pool.Query has no transaction, so the GUC has nowhere to live, and the RLS policy would
// therefore see an unset tenant and return zero rows — a silent, intermittent, load-dependent
// wrong answer rather than an error. The uniformity is what makes the rule auditable.
type Pool struct {
	pool *pgxpool.Pool
	cfg  PoolConfig
}

// NewPool opens the pool and verifies it can reach the database before returning.
//
// The eager Ping is deliberate: a pool that constructs successfully and fails on first use turns
// a configuration error into a request-path error, which reaches a customer instead of a
// readiness probe.
func NewPool(ctx context.Context, cfg PoolConfig) (*Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "postgres: invalid connection string")
	}

	pc.MaxConns = cfg.MaxConns
	pc.MinConns = cfg.MinConns
	pc.MaxConnLifetime = cfg.MaxConnLifetime
	pc.MaxConnLifetimeJitter = cfg.MaxConnLifetimeJitter
	pc.MaxConnIdleTime = cfg.MaxConnIdleTime
	pc.HealthCheckPeriod = cfg.HealthCheckPeriod
	pc.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	if cfg.ApplicationName != "" {
		if pc.ConnConfig.RuntimeParams == nil {
			pc.ConnConfig.RuntimeParams = map[string]string{}
		}
		pc.ConnConfig.RuntimeParams["application_name"] = cfg.ApplicationName
	}

	// Server-side *named* prepared statements are disabled because PgBouncer runs in transaction
	// pooling mode: a statement prepared on one server connection is simply not present on the
	// next one the pool hands out, and the resulting "prepared statement does not exist" is
	// intermittent and load-dependent. It is the second thing that surprises people about
	// transaction pooling, so it belongs next to the first.
	//
	// The mode is CacheDescribe rather than Exec, and the difference is a correctness one that
	// cost a real debugging session to find. In Exec mode pgx never asks the server what the
	// parameter types are; it infers them from the Go values, and it infers `[]byte` as `bytea`.
	// Every column in this schema that holds a JSON document — the outbox payload, the workflow
	// input and step output, the webhook body, the audit before/after snapshots — is `jsonb`,
	// and `bytea` into `jsonb` fails at runtime with SQLSTATE 22P02, on the write path, in
	// production, for exactly the rows that carry the platform's events.
	//
	// CacheDescribe asks the server to describe the statement once per connection per SQL text,
	// caches that description client side, and then executes with the correct types. It uses
	// the extended protocol without ever creating a *named* statement, so it is safe under
	// transaction pooling; the cost is one extra round trip the first time a connection sees a
	// given query, amortised to zero thereafter.
	//
	// The caveat worth knowing: a cached description is invalidated by a schema change under
	// it, and pgx surfaces that as an error rather than corrupting the result. Migrations
	// therefore run in their own sync wave before the workloads roll (see docs/deployment.md),
	// and a connection that sees a stale description is discarded by the pool's health check.
	pc.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	pc.ConnConfig.StatementCacheCapacity = 0
	pc.ConnConfig.DescriptionCacheCapacity = 512

	// AfterConnect runs once per physical connection, so the per-connection budgets are paid
	// once rather than on every transaction. They are session-level `SET`s and that is safe
	// here for the one reason a session-level SET is ever safe under transaction pooling: they
	// are identical for every borrower of the connection. `app.tenant_id` is not, which is
	// exactly why it is set with SET LOCAL inside the transaction instead.
	pc.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		stmts := []string{
			fmt.Sprintf("SET statement_timeout = %d", cfg.StatementTimeout.Milliseconds()),
			fmt.Sprintf("SET idle_in_transaction_session_timeout = %d",
				cfg.IdleInTransactionTimeout.Milliseconds()),
			fmt.Sprintf("SET lock_timeout = %d", cfg.LockTimeout.Milliseconds()),
			// Every timestamp that crosses a boundary in this platform is UTC. Pinning the
			// session removes any dependence on the server's or the container's zone, which is
			// otherwise a difference between staging and production that nobody looks for.
			"SET timezone = 'UTC'",
			// search_path is pinned so an unqualified relation name cannot resolve to something
			// in a different schema. It also means a SECURITY DEFINER function planted in a
			// writable schema cannot shadow one of ours.
			"SET search_path = pp, public",
		}
		for _, s := range stmts {
			if _, err := conn.Exec(ctx, s); err != nil {
				return fmt.Errorf("postgres: AfterConnect %q: %w", s, err)
			}
		}
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, mapError(err, "postgres: open pool")
	}

	p := &Pool{pool: pool, cfg: cfg}
	if err := p.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

// Ping verifies the pool can reach the database. It is the readiness probe's query and it is
// deliberately trivial: a readiness check that runs real work turns a slow database into a
// restart loop, which is how a recoverable slowdown becomes an outage.
func (p *Pool) Ping(ctx context.Context) error {
	if err := p.pool.Ping(ctx); err != nil {
		return mapError(err, "postgres: ping")
	}
	return nil
}

// HealthCheck reports pool saturation alongside reachability.
//
// Reachability alone is not health. A pool at 16 of 16 acquired connections answers Ping
// instantly — the health checker gets a connection because the check itself is cheap — while
// every real request queues behind the pool. Reporting the saturation is what lets the
// readiness gate shed load before the queue turns into timeouts.
func (p *Pool) HealthCheck(ctx context.Context) (Health, error) {
	s := p.pool.Stat()
	h := Health{
		TotalConns:      s.TotalConns(),
		AcquiredConns:   s.AcquiredConns(),
		IdleConns:       s.IdleConns(),
		MaxConns:        s.MaxConns(),
		EmptyAcquires:   s.EmptyAcquireCount(),
		CanceledAcquire: s.CanceledAcquireCount(),
	}
	if h.MaxConns > 0 {
		h.Saturation = float64(h.AcquiredConns) / float64(h.MaxConns)
	}
	if err := p.pool.Ping(ctx); err != nil {
		return h, mapError(err, "postgres: health check")
	}
	h.Reachable = true
	return h, nil
}

// Health is a snapshot of pool state, exported as pp_postgres_* gauges.
type Health struct {
	Reachable       bool
	TotalConns      int32
	AcquiredConns   int32
	IdleConns       int32
	MaxConns        int32
	EmptyAcquires   int64
	CanceledAcquire int64
	// Saturation is AcquiredConns / MaxConns. Alert at 0.8 sustained: past that, the pool is the
	// bottleneck and the correct response is to shed load upstream, not to enlarge the pool —
	// enlarging it moves the queue into the database, where it is slower and harder to see.
	Saturation float64
}

// Close drains the pool, waiting for in-flight transactions to finish or for ctx to expire.
//
// The wait matters. pgxpool.Close returns once connections are closed, which for a transaction
// in flight means the transaction is aborted — and aborting a transaction that has already
// written its state row and its outbox row during a rolling restart is precisely the case the
// outbox exists to make survivable, so there is no reason to provoke it. This gives in-flight
// work a bounded window to commit, then closes regardless: a shutdown that can be blocked
// forever by one stuck transaction is not a shutdown.
func (p *Pool) Close(ctx context.Context) error {
	deadline := time.NewTicker(25 * time.Millisecond)
	defer deadline.Stop()

	for {
		if p.pool.Stat().AcquiredConns() == 0 {
			p.pool.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			// Close anyway. The alternative is a pod that never terminates, which Kubernetes
			// resolves with SIGKILL after the grace period — the same outcome, later, with a
			// less useful log line.
			p.pool.Close()
			return apierror.Wrapf(ctx.Err(), apierror.CodeInternalError,
				"postgres: closed with %d connection(s) still acquired",
				p.pool.Stat().AcquiredConns())
		case <-deadline.C:
		}
	}
}

// Stat exposes the underlying pool statistics for the metrics exporter.
func (p *Pool) Stat() *pgxpool.Stat { return p.pool.Stat() }
