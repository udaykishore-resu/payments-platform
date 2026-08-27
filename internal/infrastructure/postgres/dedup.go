package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

// DedupStore records which (consumer group, event) pairs have been processed.
//
// Combined with at-least-once delivery this produces effectively-once *business* semantics, and
// combined with the database invariants it survives a bug in itself: a dedup row that is
// somehow missed still cannot post a ledger entry twice, because the unique index on
// (source_event_id, account, side) refuses it, and still cannot record a second successful
// attempt, because invariant I3 refuses that. Three independent mechanisms, because
// "effectively once" that rests on one mechanism is "at least once" with better marketing.
//
// It is not part of ports.Repositories: a consumer constructs it against its own transaction,
// because the dedup insert and the handler's work must share one transaction. A dedup row
// committed separately from the effect it guards is a dedup row that lies — it says "already
// processed" for work that was rolled back.
type DedupStore struct {
	q     querier
	clock shared.Clock
	// retention must be at least the topic's retention: a consumer that forgets an event before
	// the broker does can be handed that event again and will treat it as new.
	retention time.Duration
}

var _ ports.DedupStore = (*DedupStore)(nil)

// DefaultDedupRetention is 30 days, matching the longest topic retention in docs/events.md.
const DefaultDedupRetention = 30 * 24 * time.Hour

// NewDedupStore builds a dedup store bound to a transaction.
//
// It takes the querier directly rather than being handed out by the unit of work, because the
// consumer's transaction is opened by the consumer — the whole point is that the dedup insert
// and the handler's writes are one atomic unit, and that is only expressible if the consumer
// owns the transaction boundary.
func NewDedupStore(q querier, clock shared.Clock, retention time.Duration) *DedupStore {
	if retention <= 0 {
		retention = DefaultDedupRetention
	}
	if clock == nil {
		clock = shared.SystemClock{}
	}
	return &DedupStore{q: q, clock: clock, retention: retention}
}

// MarkProcessed inserts the pair, reporting false if it was already present.
//
// `ON CONFLICT DO NOTHING RETURNING` in one statement: the insert either wins and returns a row,
// or loses and returns none. There is no read first, so two concurrent deliveries of the same
// event to the same group cannot both observe "not yet processed".
func (d *DedupStore) MarkProcessed(
	ctx context.Context, group string, id shared.EventID,
) (bool, error) {
	if _, err := tenantOf(ctx); err != nil {
		return false, err
	}
	const q = `
INSERT INTO pp.event_dedup (consumer_group, event_id, processed_at, expires_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (consumer_group, event_id) DO NOTHING
RETURNING event_id`

	now := d.clock.Now()
	var got string
	err := d.q.QueryRow(ctx, q, group, id.String(), now, now.Add(d.retention)).Scan(&got)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	default:
		return false, mapError(err, "dedup mark processed")
	}
}

// Purge deletes expired dedup rows in a bounded batch.
//
// The batch bound is the same discipline as every other sweep in this package: an unbounded
// DELETE over a month of dedup rows holds one transaction for minutes, which stops vacuum and
// pins a snapshot for every other session on the cluster.
func (d *DedupStore) Purge(ctx context.Context, before time.Time, limit int) (int, error) {
	const q = `
DELETE FROM pp.event_dedup
WHERE ctid IN (
    SELECT ctid FROM pp.event_dedup WHERE expires_at < $1 LIMIT $2
)`
	tag, err := d.q.Exec(ctx, q, before.UTC(), pageLimit(limit))
	if err != nil {
		return 0, mapError(err, "dedup purge")
	}
	return int(tag.RowsAffected()), nil
}
