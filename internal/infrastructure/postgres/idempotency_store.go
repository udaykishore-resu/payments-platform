package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
)

// IdempotencyStore is the authoritative record of which logical operations have run.
//
// PostgreSQL is authoritative and Redis is a read-through accelerator in front of it (ADR-009).
// Making the cache authoritative would mean that a Redis eviction under memory pressure silently
// converts a duplicate request into a second payment — and eviction under memory pressure is not
// a rare event, it is what a cache does when it is doing its job.
type IdempotencyStore struct {
	q      querier
	tenant shared.TenantID
	clock  shared.Clock
}

var _ ports.IdempotencyStore = (*IdempotencyStore)(nil)

// idempotencyRetention is how long a record is kept. It must exceed the longest client retry
// window; seven days is the platform's contract (baseline §14.3).
const idempotencyRetention = 7 * 24 * time.Hour

// Claim atomically inserts an IN_FLIGHT record, or reports precisely what already exists.
//
// The first statement is the whole concurrency control:
//
//	INSERT ... ON CONFLICT (the claim tuple) DO NOTHING RETURNING ...
//
// Exactly one of N concurrent identical requests gets a row back; the others get zero rows. No
// read precedes the write, so there is no window in which two callers both see "nothing there"
// and both proceed. A read-then-insert here would be correct in every test and wrong under the
// concurrency it exists to handle, which is the only concurrency that matters.
//
// On zero rows, a second statement resolves which of four things happened. That statement is an
// UPDATE, not a SELECT, and the reason is the fourth case:
//
//   - COMPLETED or FAILED_TERMINAL → replay the stored snapshot verbatim.
//   - IN_FLIGHT with a live lease → 409, with Retry-After. The caller is never blocked on
//     another process's lease (A6): blocking a request thread on a lease held by a pod that may
//     already be dead converts one slow request into a thread pool exhaustion.
//   - A stored fingerprint that differs → the key was reused for a different body. That is a
//     client bug, not a replay, and reporting it is the whole point of storing the fingerprint.
//   - IN_FLIGHT with an expired lease → the original holder died. Reclaim it — and reclaim it
//     with `UPDATE ... WHERE lease_expires_at < now() RETURNING`, so that exactly one of several
//     racing reclaimers wins. Read-then-write here would let two callers both observe the
//     expired lease and both re-execute the operation, which is a double charge produced by the
//     very mechanism that exists to prevent one.
func (s *IdempotencyStore) Claim(
	ctx context.Context, rec ports.IdempotencyRecord,
) (ports.ClaimResult, error) {
	if err := requireOwner(ctx, s.tenant, rec.Key.TenantID); err != nil {
		return ports.ClaimResult{}, err
	}

	const insert = `
INSERT INTO pp.idempotency_records (
    idempotency_record_id, tenant_id, merchant_id, method, path_template, idempotency_key,
    request_fingerprint, state, lease_owner, lease_expires_at,
    request_id, trace_id, created_at, expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,'IN_FLIGHT',$8,$9,$10,$11,$12,$13)
ON CONFLICT (tenant_id, merchant_id, method, path_template, idempotency_key) DO NOTHING
RETURNING idempotency_record_id`

	now := s.clock.Now()
	expires := rec.ExpiresAt
	if expires.IsZero() {
		expires = now.Add(idempotencyRetention)
	}
	lease := rec.LeaseExpiresAt
	if lease.IsZero() {
		lease = now.Add(30 * time.Second)
	}

	var newID string
	err := s.q.QueryRow(ctx, insert,
		ids.New(ids.PrefixRequest).String(),
		rec.Key.TenantID.String(), rec.Key.MerchantID.String(),
		rec.Key.Method, rec.Key.PathTemplate, rec.Key.Key,
		rec.Fingerprint, rec.RequestID, lease.UTC(),
		rec.RequestID, rec.TraceID, now, expires.UTC(),
	).Scan(&newID)
	if err == nil {
		return ports.ClaimResult{Outcome: ports.ClaimNew}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ports.ClaimResult{}, mapError(err, "claim idempotency key")
	}

	return s.resolveExisting(ctx, rec, now, lease)
}

func (s *IdempotencyStore) resolveExisting(
	ctx context.Context, rec ports.IdempotencyRecord, now, lease time.Time,
) (ports.ClaimResult, error) {
	// Reclaim first, as a single conditional UPDATE. Doing this before the read is what makes
	// the reclaim atomic: if two callers race on an expired lease, one UPDATE matches and the
	// other affects zero rows and falls through to the read, where it correctly observes the
	// winner's fresh lease and gets IN_PROGRESS.
	//
	// The fingerprint is part of the predicate, so a caller reusing the key with a *different*
	// body cannot reclaim an expired lease and quietly execute a different operation under it.
	const reclaim = `
UPDATE pp.idempotency_records
SET lease_owner = $6, lease_expires_at = $7, request_id = $8, trace_id = $9
WHERE tenant_id = $1 AND merchant_id = $2 AND method = $3
  AND path_template = $4 AND idempotency_key = $5
  AND state = 'IN_FLIGHT'
  AND lease_expires_at < now()
  AND request_fingerprint = $10
RETURNING request_id`

	var owner string
	err := s.q.QueryRow(ctx, reclaim,
		rec.Key.TenantID.String(), rec.Key.MerchantID.String(), rec.Key.Method,
		rec.Key.PathTemplate, rec.Key.Key,
		rec.RequestID, lease.UTC(), rec.RequestID, rec.TraceID, rec.Fingerprint,
	).Scan(&owner)
	if err == nil {
		return ports.ClaimResult{Outcome: ports.ClaimReclaimed}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ports.ClaimResult{}, mapError(err, "reclaim idempotency lease")
	}

	const read = `
SELECT state, request_fingerprint, lease_expires_at, response_status, response_body,
       resource_id, request_id, trace_id, completed_at, expires_at
FROM pp.idempotency_records
WHERE tenant_id = $1 AND merchant_id = $2 AND method = $3
  AND path_template = $4 AND idempotency_key = $5`

	var (
		state, fingerprint string
		leaseExpires       time.Time
		status             *int16
		body               []byte
		resourceID         string
		origReq, origTrace string
		completedAt        *time.Time
		expiresAt          time.Time
	)
	if err := s.q.QueryRow(ctx, read,
		rec.Key.TenantID.String(), rec.Key.MerchantID.String(), rec.Key.Method,
		rec.Key.PathTemplate, rec.Key.Key,
	).Scan(&state, &fingerprint, &leaseExpires, &status, &body, &resourceID,
		&origReq, &origTrace, &completedAt, &expiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The row vanished between the failed insert and this read: the retention sweep ran
			// in the gap. Report it as retryable rather than inventing an outcome — the caller's
			// next attempt will win the insert cleanly.
			return ports.ClaimResult{}, apierror.New(apierror.CodeServiceUnavailable,
				"idempotency record disappeared between claim and read; retry")
		}
		return ports.ClaimResult{}, mapError(err, "read idempotency record")
	}

	// Fingerprint mismatch outranks everything else. Same key, different body means one key was
	// used for two different operations, and treating that as a replay would return the first
	// operation's response for the second operation's request — the client would believe a
	// payment they never made had succeeded.
	if fingerprint != rec.Fingerprint {
		return ports.ClaimResult{
			Outcome:       ports.ClaimFingerprintMismatch,
			OriginalReqID: origReq,
			OriginalTrace: origTrace,
		}, nil
	}

	switch state {
	case "COMPLETED", "FAILED_TERMINAL":
		snap := ports.ResponseSnapshot{ResourceID: resourceID, Body: body}
		if status != nil {
			snap.StatusCode = int(*status)
		}
		if completedAt != nil {
			snap.CompletedAt = *completedAt
		}
		return ports.ClaimResult{
			Outcome:       ports.ClaimReplay,
			Snapshot:      &snap,
			OriginalReqID: origReq,
			OriginalTrace: origTrace,
		}, nil

	default: // IN_FLIGHT with a live lease.
		retry := time.Until(leaseExpires)
		if retry < time.Second {
			retry = time.Second
		}
		return ports.ClaimResult{
			Outcome:       ports.ClaimInProgress,
			RetryAfter:    retry,
			OriginalReqID: origReq,
			OriginalTrace: origTrace,
		}, nil
	}
}

// Complete stores the response snapshot and marks the record COMPLETED.
//
// The `state = 'IN_FLIGHT'` predicate makes the transition one-way. A COMPLETED record is
// immutable: without the predicate, a late-arriving completion from a process whose lease was
// reclaimed would overwrite the reclaimer's stored response, and every subsequent replay would
// return the loser's answer.
func (s *IdempotencyStore) Complete(
	ctx context.Context, key ports.IdempotencyKey, snapshot ports.ResponseSnapshot,
) error {
	return s.settle(ctx, key, snapshot, "COMPLETED")
}

// FailTerminal stores a non-retryable failure snapshot.
//
// It exists so that a client retrying a request that will never succeed receives the same error
// rather than a fresh attempt. Without it, a permanently-failing request consumes a new
// execution on every retry — which for a payment means repeatedly reaching the risk engine, the
// routing engine and possibly a gateway, all to fail the same way.
func (s *IdempotencyStore) FailTerminal(
	ctx context.Context, key ports.IdempotencyKey, snapshot ports.ResponseSnapshot,
) error {
	return s.settle(ctx, key, snapshot, "FAILED_TERMINAL")
}

func (s *IdempotencyStore) settle(
	ctx context.Context, key ports.IdempotencyKey, snap ports.ResponseSnapshot, state string,
) error {
	if err := requireOwner(ctx, s.tenant, key.TenantID); err != nil {
		return err
	}
	const q = `
UPDATE pp.idempotency_records
SET state = $6, response_status = $7, response_body = $8, resource_id = $9, completed_at = $10
WHERE tenant_id = $1 AND merchant_id = $2 AND method = $3
  AND path_template = $4 AND idempotency_key = $5
  AND state = 'IN_FLIGHT'`

	completed := snap.CompletedAt
	if completed.IsZero() {
		completed = s.clock.Now()
	}
	tag, err := s.q.Exec(ctx, q,
		key.TenantID.String(), key.MerchantID.String(), key.Method, key.PathTemplate, key.Key,
		state, int16(snap.StatusCode), snap.Body, snap.ResourceID, completed.UTC())
	if err != nil {
		return mapError(err, "complete idempotency record")
	}
	if tag.RowsAffected() == 0 {
		// Either the lease was reclaimed and someone else settled it, or this is a duplicate
		// completion. Both are conditions the caller must know about: silently succeeding would
		// let a process believe its response is the one that will be replayed when it is not.
		return apierror.Newf(apierror.CodeIdempotentRequestInProgress,
			"idempotency record for %s %s is no longer claimed by this caller",
			key.Method, key.PathTemplate)
	}
	return nil
}

// Release removes an IN_FLIGHT claim after a retryable failure.
//
// This is the difference between a client's retry being a genuine new attempt and being a replay
// of an error that may since have cleared. It deletes rather than marking, because a released
// key must be claimable again and a state machine with a "released" state would need every read
// path to know about it.
//
// A COMPLETED or FAILED_TERMINAL record is never released — the predicate ensures a settled
// record survives a caller that releases out of order.
func (s *IdempotencyStore) Release(ctx context.Context, key ports.IdempotencyKey) error {
	if err := requireOwner(ctx, s.tenant, key.TenantID); err != nil {
		return err
	}
	const q = `
DELETE FROM pp.idempotency_records
WHERE tenant_id = $1 AND merchant_id = $2 AND method = $3
  AND path_template = $4 AND idempotency_key = $5
  AND state = 'IN_FLIGHT'`
	if _, err := s.q.Exec(ctx, q,
		key.TenantID.String(), key.MerchantID.String(), key.Method,
		key.PathTemplate, key.Key); err != nil {
		return mapError(err, "release idempotency claim")
	}
	return nil
}

// PurgeExpired deletes records past their retention, in a bounded batch.
//
// The limit is not a nicety. An unbounded DELETE over a week of idempotency records holds one
// transaction for minutes, which stops vacuum and pins a snapshot across the whole cluster —
// so the retention job would degrade the payment path it exists to keep clean.
func (s *IdempotencyStore) PurgeExpired(
	ctx context.Context, before time.Time, limit int,
) (int, error) {
	if err := requireTenantCtx(ctx, s.tenant); err != nil {
		return 0, err
	}
	const q = `
DELETE FROM pp.idempotency_records
WHERE ctid IN (
    SELECT ctid FROM pp.idempotency_records
    WHERE tenant_id = $1 AND expires_at < $2
    LIMIT $3
)`
	tag, err := s.q.Exec(ctx, q, s.tenant.String(), before.UTC(), pageLimit(limit))
	if err != nil {
		return 0, mapError(err, "purge idempotency records")
	}
	return int(tag.RowsAffected()), nil
}

// PoolIdempotencyStore is the idempotency record outside a transaction.
//
// # Why this exists alongside the transactional one
//
// The idempotency claim happens in the *transport* layer, before the handler runs and therefore
// before any unit of work is open — that ordering is the point of it (baseline §12 stage 8): the
// claim must own the response snapshot for the whole handler, including the parts that open and
// close their own transactions. A store bound to a transaction cannot do that.
//
// It reads the tenant from the context per call rather than being constructed per tenant, because
// it is a process-lifetime object shared by every request. The RLS policy and requireOwner still
// apply exactly as they do inside a transaction; what changes is only that each statement runs on
// its own connection.
type PoolIdempotencyStore struct {
	pool  *Pool
	clock shared.Clock
}

// NewIdempotencyStore builds the pool-backed store.
func NewIdempotencyStore(pool *Pool, clock shared.Clock) *PoolIdempotencyStore {
	if clock == nil {
		clock = shared.SystemClock{}
	}
	return &PoolIdempotencyStore{pool: pool, clock: clock}
}

var _ ports.IdempotencyStore = (*PoolIdempotencyStore)(nil)

// forTenant builds the per-call store bound to the context's tenant.
//
// The tenant comes from the context and nowhere else, which is baseline §16.2's rule: a store
// that took the tenant as a parameter would be a second origin for tenant identity, and a caller
// could set it.
func (s *PoolIdempotencyStore) forTenant(ctx context.Context) (*IdempotencyStore, error) {
	t, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	return &IdempotencyStore{q: s.pool.pool, tenant: t, clock: s.clock}, nil
}

// Claim atomically inserts an IN_FLIGHT record or reports what already exists.
func (s *PoolIdempotencyStore) Claim(ctx context.Context, rec ports.IdempotencyRecord) (ports.ClaimResult, error) {
	inner, err := s.forTenant(ctx)
	if err != nil {
		return ports.ClaimResult{}, err
	}
	return inner.Claim(ctx, rec)
}

// Complete stores the response snapshot and marks the record COMPLETED.
func (s *PoolIdempotencyStore) Complete(ctx context.Context, key ports.IdempotencyKey, snap ports.ResponseSnapshot) error {
	inner, err := s.forTenant(ctx)
	if err != nil {
		return err
	}
	return inner.Complete(ctx, key, snap)
}

// FailTerminal records a terminal failure so a retry replays it rather than re-executing.
func (s *PoolIdempotencyStore) FailTerminal(ctx context.Context, key ports.IdempotencyKey, snap ports.ResponseSnapshot) error {
	inner, err := s.forTenant(ctx)
	if err != nil {
		return err
	}
	return inner.FailTerminal(ctx, key, snap)
}

// Release drops a claim whose holder did not complete, so a retry may take it.
func (s *PoolIdempotencyStore) Release(ctx context.Context, key ports.IdempotencyKey) error {
	inner, err := s.forTenant(ctx)
	if err != nil {
		return err
	}
	return inner.Release(ctx, key)
}

// PurgeExpired removes records past their retention.
func (s *PoolIdempotencyStore) PurgeExpired(ctx context.Context, before time.Time, limit int) (int, error) {
	inner, err := s.forTenant(ctx)
	if err != nil {
		return 0, err
	}
	return inner.PurgeExpired(ctx, before, limit)
}
