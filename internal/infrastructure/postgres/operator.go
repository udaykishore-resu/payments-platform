package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// OperatorReports is the read side an operator's CLI needs: outbox backlog, workflow inventory,
// the dead-letter queue and the seeded-table counts.
//
// # Why it is here and not assembled from the repositories
//
// The repositories are tenant-scoped by construction — every one of them takes its tenant from the
// unit of work's GUC, because that is what makes cross-tenant reads impossible on the request path.
// An operator asking "how far behind is the outbox" is asking a platform-wide question, and
// answering it through a tenant-scoped repository would mean either looping over every tenant or
// weakening the repository. Neither is right, so the platform-wide reads live here, in their own
// type, with their own connection, and are reachable only from a binary an operator runs.
//
// # What that means for row-level security
//
// These queries are ordinary SELECTs and RLS still applies to them. A connection made as `pp_app`
// sees only its GUC's tenant and would report a backlog of zero for a platform with a million
// unpublished rows. That is why [OperatorReports.Role] exists and why platformctl prints it: the
// operator must be able to tell "the backlog is empty" from "this role cannot see the backlog".
type OperatorReports struct {
	pool *Pool
}

// NewOperatorReports wraps a pool for operator queries.
func NewOperatorReports(pool *Pool) *OperatorReports { return &OperatorReports{pool: pool} }

// Role reports the connected role and whether it bypasses row-level security.
//
// Printed by every command that reports a platform-wide count, because a zero that means "nothing
// is wrong" and a zero that means "you cannot see it" are the same character on a terminal during
// an incident.
func (o *OperatorReports) Role(ctx context.Context) (name string, bypassesRLS bool, err error) {
	row := o.pool.pool.QueryRow(ctx,
		`SELECT current_user, coalesce(bool_or(rolbypassrls OR rolsuper), false)
		   FROM pg_roles WHERE rolname = current_user`)
	if err := row.Scan(&name, &bypassesRLS); err != nil {
		return "", false, mapError(err, "postgres: read connected role")
	}
	return name, bypassesRLS, nil
}

// OutboxStatus is the backlog report.
//
// Backlog is the SLI for every asynchronous path in the platform: a growing outbox means events
// are late, and events being late means projections, webhooks and the ledger are all behind —
// visible to a merchant long before it is visible on a dashboard nobody is watching.
type OutboxStatus struct {
	// Unpublished is the total number of rows the relay has not published.
	Unpublished int
	// OldestUnpublished is the occurred_at of the oldest one. Zero when there is no backlog.
	//
	// It is the number that matters, not the count: ten thousand rows published within a second
	// of each other is a healthy burst, and one row stuck for an hour is an incident. A count
	// alone cannot tell them apart.
	OldestUnpublished time.Time
	// Failed counts rows that have failed to publish at least once and are waiting on a retry.
	Failed int
	// Claimed counts rows a relay replica holds a claim on. A large and unmoving claimed count
	// with no publishing is the signature of a relay that died holding claims.
	Claimed int
	// ByTopic is the unpublished count per topic, which is what identifies *which* consumer is
	// about to fall behind.
	ByTopic map[string]int
}

// Outbox reports the backlog.
func (o *OperatorReports) Outbox(ctx context.Context) (OutboxStatus, error) {
	out := OutboxStatus{ByTopic: map[string]int{}}
	row := o.pool.pool.QueryRow(ctx, `
SELECT count(*)                                             AS unpublished,
       coalesce(min(occurred_at), 'epoch'::timestamptz)     AS oldest,
       count(*) FILTER (WHERE publish_attempts > 0)         AS failed,
       count(*) FILTER (WHERE claimed_at IS NOT NULL)       AS claimed
  FROM pp.outbox_events
 WHERE published_at IS NULL`)
	var oldest time.Time
	if err := row.Scan(&out.Unpublished, &oldest, &out.Failed, &out.Claimed); err != nil {
		return OutboxStatus{}, mapError(err, "postgres: read outbox backlog")
	}
	if oldest.Year() > 1970 {
		out.OldestUnpublished = oldest.UTC()
	}

	rows, err := o.pool.pool.Query(ctx, `
SELECT topic, count(*) FROM pp.outbox_events
 WHERE published_at IS NULL GROUP BY topic ORDER BY count(*) DESC`)
	if err != nil {
		return OutboxStatus{}, mapError(err, "postgres: read outbox backlog by topic")
	}
	defer rows.Close()
	for rows.Next() {
		var topic string
		var n int
		if err := rows.Scan(&topic, &n); err != nil {
			return OutboxStatus{}, mapError(err, "postgres: scan outbox backlog by topic")
		}
		out.ByTopic[topic] = n
	}
	return out, mapError(rows.Err(), "postgres: read outbox backlog by topic")
}

// WorkflowInstanceSummary is one instance as an operator needs to see it: enough to decide whether
// to resume it, and nothing that would put tenant data on a terminal.
type WorkflowInstanceSummary struct {
	ID          string
	TenantID    string
	Definition  string
	State       string
	CurrentStep string
	Attempt     int
	LastError   string
	UpdatedAt   time.Time
	RunAfter    time.Time
	LeaseOwner  string
	LeaseUntil  *time.Time
}

// WorkflowCounts returns the instance count per state, platform-wide.
func (o *OperatorReports) WorkflowCounts(ctx context.Context) (map[string]int, error) {
	rows, err := o.pool.pool.Query(ctx,
		`SELECT state, count(*) FROM pp.workflow_instances GROUP BY state ORDER BY state`)
	if err != nil {
		return nil, mapError(err, "postgres: count workflow instances")
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, mapError(err, "postgres: scan workflow counts")
		}
		out[state] = n
	}
	return out, mapError(rows.Err(), "postgres: count workflow instances")
}

// WorkflowList returns instances filtered by state and by how long they have made no progress.
//
// A zero stuckFor means "no staleness filter". The limit is mandatory rather than optional: an
// operator running this during an incident against a platform with a hundred thousand instances
// wants the worst few, and a command that prints a hundred thousand lines is a command whose
// output nobody reads.
func (o *OperatorReports) WorkflowList(ctx context.Context, state string, stuckFor time.Duration,
	limit int) ([]WorkflowInstanceSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	const q = `
SELECT instance_id, tenant_id, workflow_name, state, current_step, attempt,
       last_error, updated_at, run_after, coalesce(lease_owner, ''), lease_expires_at
  FROM pp.workflow_instances
 WHERE ($1 = '' OR state = $1)
   AND ($2::interval IS NULL OR updated_at < now() - $2::interval)
 ORDER BY updated_at ASC
 LIMIT $3`
	var interval any
	if stuckFor > 0 {
		interval = stuckFor.String()
	}
	rows, err := o.pool.pool.Query(ctx, q, state, interval, limit)
	if err != nil {
		return nil, mapError(err, "postgres: list workflow instances")
	}
	defer rows.Close()
	var out []WorkflowInstanceSummary
	for rows.Next() {
		var s WorkflowInstanceSummary
		if err := rows.Scan(&s.ID, &s.TenantID, &s.Definition, &s.State, &s.CurrentStep,
			&s.Attempt, &s.LastError, &s.UpdatedAt, &s.RunAfter, &s.LeaseOwner, &s.LeaseUntil); err != nil {
			return nil, mapError(err, "postgres: scan workflow instance")
		}
		out = append(out, s)
	}
	return out, mapError(rows.Err(), "postgres: list workflow instances")
}

// ResumeWorkflow clears an instance's lease and makes it immediately runnable.
//
// # Why this is safe to run twice
//
// The engine's step records make re-execution a no-op for a step that already succeeded, so a
// resumed instance replays from its last incomplete step rather than from the beginning. That
// property is why this command needs no `--confirm` while `migrate down` does.
//
// # Why it bumps the lease epoch
//
// The epoch is the fence. A worker that was paused — GC, a network partition, a stopped container
// — may still believe it holds this instance, and would otherwise wake up and advance it
// concurrently with whichever worker picks it up after the resume. Incrementing the epoch makes
// the stale worker's next write fail its fencing check instead of double-executing a step.
//
// It returns false when the instance does not exist or is in a state that is not resumable —
// COMPLETED and COMPENSATED are finished, and "resuming" one would mean re-running a workflow
// whose effects have already happened.
func (o *OperatorReports) ResumeWorkflow(ctx context.Context, id string) (bool, string, error) {
	const q = `
UPDATE pp.workflow_instances
   SET state            = 'RUNNING',
       lease_owner      = NULL,
       lease_expires_at = NULL,
       attempt_epoch    = attempt_epoch + 1,
       run_after        = now(),
       completed_at     = NULL,
       updated_at       = now()
 WHERE instance_id = $1
   AND state IN ('PENDING', 'RUNNING', 'WAITING_SIGNAL', 'COMPENSATING', 'FAILED')
RETURNING state`
	var newState string
	err := o.pool.pool.QueryRow(ctx, q, id).Scan(&newState)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, "", nil
		}
		return false, "", mapError(err, "postgres: resume workflow")
	}
	return true, newState, nil
}

// WorkflowState reads one instance's current state, so a refused resume can say why.
func (o *OperatorReports) WorkflowState(ctx context.Context, id string) (string, bool, error) {
	var state string
	err := o.pool.pool.QueryRow(ctx,
		`SELECT state FROM pp.workflow_instances WHERE instance_id = $1`, id).Scan(&state)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, mapError(err, "postgres: read workflow state")
	}
	return state, true, nil
}

// DLQEntry is one dead-lettered workflow step.
//
// The payload is deliberately absent: it is the step's input, which for an onboarding or a payment
// workflow contains merchant data, and an operator triaging a DLQ needs the reason and the counts
// rather than the contents. Reading the payload is a separate, audited action.
type DLQEntry struct {
	ID          int64
	TenantID    string
	InstanceID  string
	StepKey     string
	Reason      string
	ParkedAt    time.Time
	ReplayCount int
	PayloadSize int
}

// WorkflowDLQ lists dead-lettered steps that have not been replayed, oldest first.
func (o *OperatorReports) WorkflowDLQ(ctx context.Context, limit int) ([]DLQEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	const q = `
SELECT dlq_id, tenant_id, instance_id, step_key, reason, parked_at, replay_count,
       octet_length(payload::text)
  FROM pp.workflow_dlq
 WHERE replayed_at IS NULL
 ORDER BY parked_at ASC
 LIMIT $1`
	rows, err := o.pool.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, mapError(err, "postgres: list workflow dlq")
	}
	defer rows.Close()
	var out []DLQEntry
	for rows.Next() {
		var e DLQEntry
		if err := rows.Scan(&e.ID, &e.TenantID, &e.InstanceID, &e.StepKey, &e.Reason,
			&e.ParkedAt, &e.ReplayCount, &e.PayloadSize); err != nil {
			return nil, mapError(err, "postgres: scan workflow dlq")
		}
		out = append(out, e)
	}
	return out, mapError(rows.Err(), "postgres: list workflow dlq")
}

// AuditChainRange is the sequence window present for a tenant, used to default the verification
// bounds so an operator does not have to look them up first.
func (o *OperatorReports) AuditChainRange(ctx context.Context, tenantID string) (from, to int64, err error) {
	row := o.pool.pool.QueryRow(ctx,
		`SELECT coalesce(min(sequence), 0), coalesce(max(sequence), 0)
		   FROM pp.audit_records WHERE tenant_id = $1`, tenantID)
	if err := row.Scan(&from, &to); err != nil {
		return 0, 0, mapError(err, "postgres: read audit chain range")
	}
	return from, to, nil
}

// TableCounts returns the row count of each named table in the pp schema.
//
// Used by `seed` to report what it produced and to decide whether a reset is needed. Exact counts
// rather than the planner's estimate, because a seed that reported "about 25 merchants" would be
// useless to a test asserting on a specific one.
func (o *OperatorReports) TableCounts(ctx context.Context, tables []string) (map[string]int, error) {
	out := make(map[string]int, len(tables))
	for _, t := range tables {
		if !safeTableName(t) {
			return nil, apierror.Newf(apierror.CodeInternalError,
				"refusing to count %q: not a plain table name", t)
		}
		var n int
		// The identifier is validated above rather than parameterised, because a table name
		// cannot be a bind parameter in PostgreSQL. The allowlist-by-shape check is what stands
		// in for the parameterisation this statement cannot have.
		if err := o.pool.pool.QueryRow(ctx, `SELECT count(*) FROM pp.`+t).Scan(&n); err != nil {
			return nil, mapError(err, "postgres: count "+t)
		}
		out[t] = n
	}
	return out, nil
}

// safeTableName accepts only lower-case identifiers, which is every table this schema has.
func safeTableName(t string) bool {
	if t == "" || len(t) > 63 {
		return false
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch {
		case c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// ConnectionSummary is one merchant-to-gateway binding as an operator sees it.
type ConnectionSummary struct {
	ConnectionID        string
	GatewayID           string
	Status              string
	CertificationStatus string
	// CredentialRef is the `secret://` reference, never material. It is safe to print for the
	// reason docs/control-plane.md §5.2 gives: a reference contains no secret-derived data, which
	// is the property that lets it appear in logs, audit records and support tickets.
	CredentialRef string
	UpdatedAt     time.Time
}

// MerchantConnections lists a merchant's connections in one environment.
//
// It answers the question `certify` asks first — "is this merchant certified, and if not, where
// did it stop?" — without running anything.
func (o *OperatorReports) MerchantConnections(ctx context.Context, merchantID, environment string) ([]ConnectionSummary, error) {
	const q = `
SELECT connection_id, gateway_id, status, certification_status, credential_ref, updated_at
  FROM pp.gateway_connections
 WHERE merchant_id = $1 AND environment = $2
 ORDER BY gateway_id`
	rows, err := o.pool.pool.Query(ctx, q, merchantID, environment)
	if err != nil {
		return nil, mapError(err, "postgres: list merchant connections")
	}
	defer rows.Close()
	var out []ConnectionSummary
	for rows.Next() {
		var c ConnectionSummary
		if err := rows.Scan(&c.ConnectionID, &c.GatewayID, &c.Status, &c.CertificationStatus,
			&c.CredentialRef, &c.UpdatedAt); err != nil {
			return nil, mapError(err, "postgres: scan merchant connection")
		}
		out = append(out, c)
	}
	return out, mapError(rows.Err(), "postgres: list merchant connections")
}

// GatewayEndpoint is one catalogue row's endpoint for a single environment.
type GatewayEndpoint struct {
	GatewayID  string
	BaseURL    string
	APIVersion string
}

// GatewayEndpoints reads the platform-global gateway catalogue's endpoints for one environment.
//
// # Why this is not the GatewayRepository
//
// The repository refuses to query without a tenant on the context, and that guard is right for
// every request-path read. This one is neither: `pp.gateways` is platform-global — it has no
// tenant_id column and therefore no RLS policy — and it is read once at startup, before any
// request and therefore before any tenant exists. Forcing a tenant context here would mean
// inventing one, and an invented tenant on a startup path is exactly the kind of thing that later
// gets copied onto a request path.
//
// A gateway with no endpoint for this environment is skipped rather than returned blank, because
// the caller's next action is to configure an adapter and an adapter with no base URL is refused
// at construction.
func (o *OperatorReports) GatewayEndpoints(ctx context.Context, environment string) ([]GatewayEndpoint, error) {
	const q = `
SELECT gateway_id, coalesce(base_urls ->> $1, ''), api_version
  FROM pp.gateways
 WHERE status IN ('ACTIVE', 'DEGRADED')
 ORDER BY gateway_id`
	rows, err := o.pool.pool.Query(ctx, q, environment)
	if err != nil {
		return nil, mapError(err, "postgres: read gateway endpoints")
	}
	defer rows.Close()
	var out []GatewayEndpoint
	for rows.Next() {
		var e GatewayEndpoint
		if err := rows.Scan(&e.GatewayID, &e.BaseURL, &e.APIVersion); err != nil {
			return nil, mapError(err, "postgres: scan gateway endpoint")
		}
		out = append(out, e)
	}
	return out, mapError(rows.Err(), "postgres: read gateway endpoints")
}
