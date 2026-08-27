package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/audit"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// AuditRepository appends to the per-tenant hash chain and reads it back for verification.
//
// The chain is what turns "append-only" from a claim into evidence: each record's digest covers
// the previous record's digest, so altering any record invalidates every digest after it, and
// the head digest is the whole chain compressed into thirty-two bytes.
type AuditRepository struct {
	q      querier
	tenant shared.TenantID
}

var _ ports.AuditRepository = (*AuditRepository)(nil)

// auditChainLockNamespace keeps this package's advisory locks from colliding with any other
// use of pg_advisory_xact_lock in the platform. Two subsystems that both lock on
// hashtext(tenant_id) with no namespace would serialize against each other for no reason, and
// the symptom — one job mysteriously slow whenever another runs — is very hard to trace back.
const auditChainLockNamespace = 0x41554449 // "AUDI"

// Append writes one record, linked to the tenant's current chain head.
//
// The insert is serialized per tenant by `pg_advisory_xact_lock`. That is a genuine
// serialization point and it is chosen deliberately: the chain is only tamper-evident if
// sequence numbers are dense and each digest covers exactly the record before it, and two
// concurrent appends that both read the same head would produce two records claiming the same
// predecessor — a fork, which verification reports as tampering. Taking a lock is acceptable
// here for a reason that does not generalise: the audit write is buffered and off the response
// path, so it is not in the payment latency budget.
//
// The lock is transaction-scoped (`_xact_`), so it is released at COMMIT or ROLLBACK. A
// session-scoped lock would survive a rollback and, under transaction pooling, would be
// inherited by the next borrower of the connection — which is the same class of bug as a leaked
// session GUC and just as hard to reproduce.
func (r *AuditRepository) Append(ctx context.Context, rec audit.Record) error {
	if err := requireOwner(ctx, r.tenant, rec.TenantID()); err != nil {
		return err
	}
	if _, err := r.q.Exec(ctx,
		`SELECT pg_advisory_xact_lock($1, hashtext($2))`,
		auditChainLockNamespace, r.tenant.String()); err != nil {
		return mapError(err, "lock audit chain")
	}

	const q = `
INSERT INTO pp.audit_records (
    audit_id, partition_month, tenant_id, sequence,
    actor_type, actor_id, actor_name, actor_ip, actor_user_agent, actor_on_behalf_of,
    action, resource_type, resource_id, outcome,
    before_state, after_state, reason, correlation_id, trace_id,
    occurred_at, recorded_at, prev_digest, entry_digest)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`

	before, err := marshalSnapshot(rec.Before())
	if err != nil {
		return err
	}
	after, err := marshalSnapshot(rec.After())
	if err != nil {
		return err
	}
	a := rec.Actor()

	if _, err := r.q.Exec(ctx, q,
		rec.ID().String(), rec.PartitionMonth(), rec.TenantID().String(), int64(rec.Sequence()),
		string(a.Type), a.ID, a.Name, a.IP, a.UserAgent, a.OnBehalfOf,
		string(rec.Action()), rec.ResourceType(), rec.ResourceID(), string(rec.Outcome()),
		before, after, rec.Reason(), rec.CorrelationID(), rec.TraceID(),
		rec.OccurredAt(), rec.RecordedAt(), rec.PrevDigest(), rec.Digest(),
	); err != nil {
		return mapError(err, "append audit record")
	}
	return nil
}

// LastDigest returns the tail of a tenant's chain: the digest the next append links to, and the
// sequence number it follows.
//
// An empty chain returns the empty digest and sequence zero. The caller supplies the genesis
// constant in that case — this repository deliberately does not know it, because the genesis
// value is a domain constant and inventing one here would let a mistyped constant produce a
// chain that verifies against itself and against nothing else.
func (r *AuditRepository) LastDigest(ctx context.Context, t shared.TenantID) (string, int64, error) {
	if err := requireOwner(ctx, r.tenant, t); err != nil {
		return "", 0, err
	}
	const q = `
SELECT entry_digest, sequence
FROM pp.audit_records
WHERE tenant_id = $1
ORDER BY sequence DESC
LIMIT 1`
	var digest string
	var seq int64
	err := r.q.QueryRow(ctx, q, t.String()).Scan(&digest, &seq)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", 0, nil
		}
		return "", 0, mapError(err, "read audit chain head")
	}
	return digest, seq, nil
}

// Query returns audit records matching a filter, newest first, cursor-paginated.
func (r *AuditRepository) Query(
	ctx context.Context, f ports.AuditFilter, page ports.Page,
) ([]audit.Record, string, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, "", err
	}
	cur, err := DecodeCursor(page.Cursor)
	if err != nil {
		return nil, "", err
	}
	limit := pageLimit(page.Limit)

	c := newCond(r.tenant.String())
	c.raw("tenant_id = $1")
	if f.ActorID != "" {
		c.eq("actor_id", f.ActorID)
	}
	if f.Action != "" {
		c.eq("action", f.Action)
	}
	if f.ResourceType != "" {
		c.eq("resource_type", f.ResourceType)
	}
	if f.ResourceID != "" {
		c.eq("resource_id", f.ResourceID)
	}
	if f.From != nil {
		c.gte("recorded_at", f.From.UTC())
		c.gte("partition_month", monthOf(*f.From))
	}
	if f.To != nil {
		c.lte("recorded_at", f.To.UTC())
		c.lte("partition_month", monthOf(*f.To))
	}
	c.keysetBefore("recorded_at", "audit_id", cur)

	q := selectAudit + c.where() +
		" ORDER BY recorded_at DESC, audit_id DESC LIMIT " + c.limitPlaceholder()

	rows, err := r.q.Query(ctx, q, c.argsWith(limit+1)...)
	if err != nil {
		return nil, "", mapError(err, "query audit records")
	}
	defer rows.Close()

	out := make([]audit.Record, 0, limit)
	for rows.Next() {
		rec, err := scanAudit(rows)
		if err != nil {
			return nil, "", mapError(err, "query audit records")
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, "", mapError(err, "query audit records")
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = EncodeCursor(Cursor{Time: last.RecordedAt(), ID: last.ID().String()})
	}
	return out, next, nil
}

// VerifyRange re-computes the chain over [from, to] and reports the first tampered sequence.
//
// It recomputes rather than comparing stored digests to each other, because comparing stored
// values only proves they are self-consistent — an attacker who edited a record and recomputed
// every digest after it would pass that check trivially. Recomputing from the record's *content*
// is what makes the check meaningful, and the recomputation uses the same
// audit.Record.ComputeDigest the writer used, so the verifier and the writer can never disagree
// about the encoding.
//
// A gap in the sequence is reported as tampering at the missing position. It might instead be a
// crashed writer, but the two are indistinguishable from the chain alone, and the safe reading
// of "a record that should exist does not" is the pessimistic one.
func (r *AuditRepository) VerifyRange(
	ctx context.Context, t shared.TenantID, from, to int64,
) (bool, int64, error) {
	if err := requireOwner(ctx, r.tenant, t); err != nil {
		return false, 0, err
	}
	rows, err := r.q.Query(ctx, selectAudit+`
WHERE tenant_id = $1 AND sequence BETWEEN $2 AND $3
ORDER BY sequence ASC`, t.String(), from, to)
	if err != nil {
		return false, 0, mapError(err, "verify audit chain")
	}
	defer rows.Close()

	var (
		expectedSeq = from
		prevDigest  string
		first       = true
	)
	for rows.Next() {
		rec, err := scanAudit(rows)
		if err != nil {
			return false, 0, mapError(err, "verify audit chain")
		}
		if int64(rec.Sequence()) != expectedSeq {
			return false, expectedSeq, nil
		}
		if !first && rec.PrevDigest() != prevDigest {
			// The link is broken: this record does not follow the one before it. Either a record
			// was removed, or one was rewritten.
			return false, int64(rec.Sequence()), nil
		}
		if rec.ComputeDigest() != rec.Digest() {
			// The content does not hash to the digest stored with it.
			return false, int64(rec.Sequence()), nil
		}
		prevDigest = rec.Digest()
		expectedSeq++
		first = false
	}
	if err := rows.Err(); err != nil {
		return false, 0, mapError(err, "verify audit chain")
	}
	return true, 0, nil
}

const selectAudit = `
SELECT audit_id, tenant_id, sequence,
       actor_type, actor_id, actor_name, actor_ip, actor_user_agent, actor_on_behalf_of,
       action, resource_type, resource_id, outcome,
       before_state, after_state, reason, correlation_id, trace_id,
       occurred_at, recorded_at, prev_digest, entry_digest
FROM pp.audit_records`

func scanAudit(row scanRow) (audit.Record, error) {
	var (
		p                    audit.RehydrateRecordParams
		id, tenant           string
		seq                  int64
		actorType            string
		actor                audit.Actor
		action, outcome      string
		beforeRaw, afterRaw  []byte
		occurredAt, recorded time.Time
	)
	if err := row.Scan(&id, &tenant, &seq,
		&actorType, &actor.ID, &actor.Name, &actor.IP, &actor.UserAgent, &actor.OnBehalfOf,
		&action, &p.ResourceType, &p.ResourceID, &outcome,
		&beforeRaw, &afterRaw, &p.Reason, &p.CorrelationID, &p.TraceID,
		&occurredAt, &recorded, &p.PrevDigest, &p.Digest); err != nil {
		return audit.Record{}, err
	}
	actor.Type = audit.ActorType(actorType)
	p.ID = shared.AuditID(id)
	p.TenantID = shared.TenantID(tenant)
	p.Sequence = uint64(seq)
	p.Actor = actor
	p.Action = audit.Action(action)
	p.Outcome = audit.Outcome(outcome)
	p.OccurredAt, p.RecordedAt = occurredAt, recorded

	if err := unmarshalJSON(beforeRaw, &p.Before, "audit before-state"); err != nil {
		return audit.Record{}, err
	}
	if err := unmarshalJSON(afterRaw, &p.After, "audit after-state"); err != nil {
		return audit.Record{}, err
	}
	return audit.RehydrateRecord(p), nil
}

// marshalSnapshot encodes an allowlisted before/after snapshot, or SQL NULL when there is none.
//
// NULL rather than `{}` for an absent snapshot: they mean different things — "this action had no
// before state" versus "the before state was an empty object" — and an investigator reading a
// deletion record needs to be able to tell.
func marshalSnapshot(m map[string]any) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "postgres: encode audit snapshot")
	}
	return b, nil
}
