package postgres

import (
	"context"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// ReconciliationRepository tracks unresolved outcomes and settlement exceptions.
//
// An exception is identified by the *discrepancy*, not by the run that found it. That is the
// design decision the whole table turns on: a reconciliation run over a window must be
// idempotent, so re-running yesterday's window must update the exceptions it already opened
// rather than opening a second copy of each. Keying on the run would give a growing pile of
// duplicates that an operator has to deduplicate by eye before they can work the queue.
type ReconciliationRepository struct {
	q      querier
	tenant shared.TenantID
	clock  shared.Clock
}

var _ ports.ReconciliationRepository = (*ReconciliationRepository)(nil)

// OpenException records a discrepancy, or refreshes one already open.
//
// ON CONFLICT on the identity tuple updates severity and detail but deliberately does *not*
// reopen a RESOLVED exception. Re-detection of something a human has already investigated and
// closed is usually the reconciler seeing stale data, and silently reopening it would mean an
// operator's decision is undone by a scheduled job — which is how a queue becomes something
// people stop reading.
func (r *ReconciliationRepository) OpenException(ctx context.Context, e ports.ReconciliationException) error {
	if err := requireOwner(ctx, r.tenant, e.TenantID); err != nil {
		return err
	}
	const q = `
INSERT INTO pp.reconciliation_exceptions (
    exception_id, tenant_id, merchant_id, payment_id, attempt_id, external_ref,
    kind, severity, detail, state, opened_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'OPEN',$10)
ON CONFLICT (tenant_id, kind, payment_id, external_ref) DO UPDATE SET
    severity    = EXCLUDED.severity,
    detail      = EXCLUDED.detail,
    merchant_id = EXCLUDED.merchant_id,
    attempt_id  = EXCLUDED.attempt_id
WHERE pp.reconciliation_exceptions.state IN ('OPEN','INVESTIGATING')`

	id := e.ID
	if id == "" {
		// Derived from the identity tuple rather than minted, so two concurrent runs that both
		// detect the same discrepancy target the same row and contend on the ON CONFLICT instead
		// of racing to insert two rows with different surrogate keys.
		id = "rex_" + shortHash(e.TenantID.String()+"|"+e.Kind+"|"+
			e.PaymentID.String()+"|"+e.Detail)
	}
	opened := e.OpenedAt
	if opened.IsZero() {
		opened = r.clock.Now()
	}

	if _, err := r.q.Exec(ctx, q,
		id, e.TenantID.String(), e.MerchantID.String(), e.PaymentID.String(),
		e.AttemptID.String(), "", e.Kind, e.Severity, e.Detail, opened,
	); err != nil {
		return mapError(err, "open reconciliation exception")
	}
	return nil
}

// ListOpen returns open and investigating exceptions, most severe first, cursor-paginated.
//
// An empty severity means "all". Severity is a filter rather than a fixed ordering because the
// two consumers want different things: the activation guard asks only about CRITICAL, and the
// operator queue wants everything with the worst at the top.
func (r *ReconciliationRepository) ListOpen(
	ctx context.Context, severity string, page ports.Page,
) ([]ports.ReconciliationException, string, error) {
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
	c.raw("state IN ('OPEN','INVESTIGATING')")
	if severity != "" {
		c.eq("severity", severity)
	}
	c.keysetBefore("opened_at", "exception_id", cur)

	q := `
SELECT exception_id, tenant_id, merchant_id, payment_id, attempt_id, kind, severity, detail,
       opened_at, resolved_at, resolution, resolved_by
FROM pp.reconciliation_exceptions` + c.where() +
		" ORDER BY opened_at DESC, exception_id DESC LIMIT " + c.limitPlaceholder()

	rows, err := r.q.Query(ctx, q, c.argsWith(limit+1)...)
	if err != nil {
		return nil, "", mapError(err, "list reconciliation exceptions")
	}
	defer rows.Close()

	out := make([]ports.ReconciliationException, 0, limit)
	for rows.Next() {
		var (
			e                              ports.ReconciliationException
			tenant, merchant, pay, attempt string
		)
		if err := rows.Scan(&e.ID, &tenant, &merchant, &pay, &attempt, &e.Kind, &e.Severity,
			&e.Detail, &e.OpenedAt, &e.ResolvedAt, &e.Resolution, &e.ResolvedBy); err != nil {
			return nil, "", mapError(err, "list reconciliation exceptions")
		}
		e.TenantID = shared.TenantID(tenant)
		e.MerchantID = shared.MerchantID(merchant)
		e.PaymentID = shared.PaymentID(pay)
		e.AttemptID = shared.AttemptID(attempt)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", mapError(err, "list reconciliation exceptions")
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = EncodeCursor(Cursor{Time: last.OpenedAt, ID: last.ID})
	}
	return out, next, nil
}

// Resolve closes an exception with a stated resolution and the person who made the call.
//
// resolved_by is required by the caller's contract and is written verbatim: an exception closed
// by nobody in particular is an exception that will be reopened by the next auditor who asks who
// decided it was acceptable.
func (r *ReconciliationRepository) Resolve(ctx context.Context, id, resolution, by string) error {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return err
	}
	const q = `
UPDATE pp.reconciliation_exceptions
SET state = 'RESOLVED', resolution = $3, resolved_by = $4, resolved_at = $5
WHERE tenant_id = $1 AND exception_id = $2 AND state IN ('OPEN','INVESTIGATING')`

	tag, err := r.q.Exec(ctx, q, r.tenant.String(), id, resolution, by, r.clock.Now())
	if err != nil {
		return mapError(err, "resolve reconciliation exception")
	}
	if tag.RowsAffected() == 0 {
		return notFound(apierror.CodePaymentNotFound, "open reconciliation exception", id)
	}
	return nil
}

// CountOpen returns open exception counts per severity.
//
// It backs both the pp_reconciliation_exceptions gauge and the merchant activation guard, which
// refuses to activate a merchant with an open CRITICAL exception. The count is grouped rather
// than filtered so one query serves both callers.
func (r *ReconciliationRepository) CountOpen(ctx context.Context) (map[string]int, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	const q = `
SELECT severity, count(*) FROM pp.reconciliation_exceptions
WHERE tenant_id = $1 AND state IN ('OPEN','INVESTIGATING')
GROUP BY severity`

	rows, err := r.q.Query(ctx, q, r.tenant.String())
	if err != nil {
		return nil, mapError(err, "count open exceptions")
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var sev string
		var n int
		if err := rows.Scan(&sev, &n); err != nil {
			return nil, mapError(err, "count open exceptions")
		}
		out[sev] = n
	}
	return out, mapError(rows.Err(), "count open exceptions")
}
