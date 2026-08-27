package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// OutboxRepository implements both sides of the transactional outbox: the writer that appends
// events in the same transaction as the state change, and the reader the relay claims batches
// with.
//
// The two live together because the claim protocol and the write protocol are one contract, and
// splitting them across files is how a change to the shard derivation lands on one side only.
type OutboxRepository struct {
	q      querier
	tenant shared.TenantID
	clock  shared.Clock
}

var (
	_ ports.OutboxWriter = (*OutboxRepository)(nil)
	_ ports.OutboxReader = (*OutboxRepository)(nil)
)

// OutboxShardBuckets is the fixed number of hash buckets an event's partition key maps into.
//
// It is a constant of the schema, not of the deployment: the bucket is a stored generated column
// on outbox_events, computed at insert time. Deriving the bucket from the live replica count
// instead would move an aggregate to a different replica the moment the fleet scaled, and the
// events in flight across that rescale would be exactly the ones that got reordered — a bug
// that only appears during an autoscale, which is the hardest time to be looking for it.
//
// Sixty-four is comfortably more than the relay's maximum replica count (12 per docs/lld.md
// §4.1), so buckets divide across replicas evenly enough that no replica carries twice another's
// share.
const OutboxShardBuckets = 64

// Append writes events to the outbox.
//
// It takes the same context, and therefore the same transaction, as the state change that
// produced them. That is the entire mechanism: the state row and the event rows commit together
// or not at all, so "state committed but the event was lost" and "event published but the state
// rolled back" both become unrepresentable rather than merely unlikely.
//
// The insert is a single multi-row statement rather than one per message. A payment that raises
// three events in one unit of work would otherwise pay three round trips on the money path.
func (o *OutboxRepository) Append(ctx context.Context, msgs ...ports.OutboxMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	if _, err := tenantOf(ctx); err != nil {
		return err
	}

	// Column-array insert: one statement, one plan, whatever the batch size. The alternative —
	// generating ($1,$2,...),($9,$10,...) placeholder groups — produces a different statement
	// text per batch size and fills the plan cache with near-duplicates.
	const q = `
INSERT INTO pp.outbox_events (
    event_id, tenant_id, aggregate_type, aggregate_id, event_type, topic, partition_key,
    payload, headers, occurred_at, available_at)
SELECT * FROM unnest(
    $1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[],
    $8::bytea[], $9::jsonb[], $10::timestamptz[], $11::timestamptz[])`

	n := len(msgs)
	var (
		ids       = make([]string, n)
		tenants   = make([]string, n)
		aggTypes  = make([]string, n)
		aggIDs    = make([]string, n)
		types     = make([]string, n)
		topics    = make([]string, n)
		keys      = make([]string, n)
		payloads  = make([][]byte, n)
		headers   = make([][]byte, n)
		occurred  = make([]time.Time, n)
		available = make([]time.Time, n)
	)
	now := o.clock.Now()
	for i, m := range msgs {
		if m.TenantID != o.tenant {
			return apierror.Newf(apierror.CodeTenantMismatch,
				"postgres: refusing to enqueue an event for tenant %s under tenant %s",
				m.TenantID, o.tenant)
		}
		if m.PartitionKey == "" {
			// An empty partition key hashes to one bucket for every aggregate, which would
			// serialize the entire platform's events behind one relay replica *and* destroy
			// per-aggregate ordering by mixing unrelated aggregates into one claim batch.
			return apierror.Newf(apierror.CodeInternalError,
				"postgres: outbox message %s has no partition key", m.ID)
		}
		hdr, err := json.Marshal(nonNilHeaders(m.Headers))
		if err != nil {
			return apierror.Wrap(err, apierror.CodeInternalError, "postgres: encode outbox headers")
		}
		id := m.ID
		if id == "" {
			id = shared.NewEventID()
		}
		ids[i], tenants[i] = id.String(), m.TenantID.String()
		aggTypes[i], aggIDs[i] = m.AggregateType, m.AggregateID
		types[i], topics[i], keys[i] = m.Type, m.Topic, m.PartitionKey
		payloads[i], headers[i] = m.Payload, hdr
		occurred[i] = orNow(m.OccurredAt, now)
		available[i] = orNow(m.AvailableAt, now)
	}

	if _, err := o.q.Exec(ctx, q, ids, tenants, aggTypes, aggIDs, types, topics, keys,
		payloads, headers, occurred, available); err != nil {
		return mapError(err, "append outbox")
	}
	return nil
}

// Claim locks up to limit unpublished messages belonging to this replica's shard.
//
// Two mechanisms are at work and it is worth being precise about which does what, because the
// second is the subtle one and it is easy to believe the first is sufficient.
//
// `FOR UPDATE SKIP LOCKED` stops two relay replicas from claiming the same *row*. That is all it
// does. It does not stop replica A from claiming event 1 of payment P while replica B claims
// event 2 of the same payment — both rows are unlocked, both are claimable, and both are then
// published concurrently. Kafka preserves the order in which the producer sent them, so a
// `payment.captured.v1` can reach the topic before the `payment.authorized.v1` that preceded it.
// Every consumer that reasons about a payment's history then sees a capture for a payment it
// believes was never authorized, and because the projection is asynchronous the corruption is
// discovered long after the deploy that caused it.
//
// The fix is the shard predicate. `shard_bucket` is a stored column derived from the partition
// key, which for a payment event is the payment ID, so every event of one payment is in one
// bucket. A replica claims whole buckets, so one payment's events are only ever claimed by one
// replica, in `outbox_id` order, and cannot be reordered by scaling the relay out.
//
// The bucket set is computed in Go and passed as an array so the predicate is
// `shard_bucket = ANY($1)` — an index range — rather than `shard_bucket %% $n = $m`, which is
// not indexable and would make every claim a full scan of the unpublished backlog.
func (o *OutboxRepository) Claim(
	ctx context.Context, shard, totalShards, limit int,
) ([]ports.OutboxMessage, error) {
	// The reader side requires *a* tenant in context but does not require it to match the
	// repository's, and this is the one deliberate departure from the tenant-scoping rule in this
	// package. The relay is one of exactly three legitimately platform-wide jobs
	// (docs/multi-tenancy.md §2.3): it runs as pp_relay, whose policy on this table alone is
	// USING (true), and which has no access whatsoever to payments, merchants or configurations.
	// Requiring a tenant match here would make the relay unable to drain any tenant's backlog but
	// its own; dropping the context check entirely would remove the last guard against a caller
	// that reached this method with no authenticated identity at all.
	if _, err := tenantOf(ctx); err != nil {
		return nil, err
	}
	buckets, err := shardBuckets(shard, totalShards)
	if err != nil {
		return nil, err
	}

	// The claim marks rows as taken in the same statement that locks them, so a relay that dies
	// between claiming and publishing leaves rows that are visibly claimed and stale rather than
	// rows that look fresh. claimed_at is what the "relay is stuck" alert reads.
	const q = `
WITH claimed AS (
    SELECT outbox_id
    FROM pp.outbox_events
    WHERE published_at IS NULL
      AND available_at <= now()
      AND shard_bucket = ANY($1)
    ORDER BY outbox_id
    FOR UPDATE SKIP LOCKED
    LIMIT $2
)
UPDATE pp.outbox_events o
SET claimed_at = now(), claimed_by = $3
FROM claimed c
WHERE o.outbox_id = c.outbox_id
RETURNING o.event_id, o.tenant_id, o.aggregate_type, o.aggregate_id, o.event_type, o.topic,
          o.partition_key, o.payload, o.headers, o.occurred_at, o.available_at`

	rows, err := o.q.Query(ctx, q, buckets, pageLimit(limit), claimOwner(ctx))
	if err != nil {
		return nil, mapError(err, "claim outbox batch")
	}
	defer rows.Close()

	var out []ports.OutboxMessage
	for rows.Next() {
		var (
			m                   ports.OutboxMessage
			id, tenant          string
			hdrRaw              []byte
			occurred, available time.Time
			aggType, aggID, typ string
			topic, partitionKey string
			payload             []byte
		)
		if err := rows.Scan(&id, &tenant, &aggType, &aggID, &typ, &topic, &partitionKey,
			&payload, &hdrRaw, &occurred, &available); err != nil {
			return nil, mapError(err, "claim outbox batch")
		}
		m.ID = shared.EventID(id)
		m.TenantID = shared.TenantID(tenant)
		m.AggregateType, m.AggregateID = aggType, aggID
		m.Type, m.Topic, m.PartitionKey = typ, topic, partitionKey
		m.Payload = payload
		m.OccurredAt, m.AvailableAt = occurred, available
		if len(hdrRaw) > 0 {
			if err := json.Unmarshal(hdrRaw, &m.Headers); err != nil {
				return nil, apierror.Wrapf(err, apierror.CodeInternalError,
					"postgres: outbox event %s has unreadable headers", id)
			}
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, "claim outbox batch")
	}
	return out, nil
}

// MarkPublished records that the broker acknowledged the batch.
//
// It is a single statement over the whole batch: acknowledging five hundred events one round
// trip at a time is how a relay falls behind a backlog it is otherwise draining fine.
func (o *OutboxRepository) MarkPublished(ctx context.Context, ids []shared.EventID) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := tenantOf(ctx); err != nil {
		return err
	}
	const q = `
UPDATE pp.outbox_events
SET published_at = now(), last_error = ''
WHERE event_id = ANY($1) AND published_at IS NULL`

	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	if _, err := o.q.Exec(ctx, q, strs); err != nil {
		return mapError(err, "mark outbox published")
	}
	return nil
}

// MarkFailed records a publish failure and reschedules the message.
//
// The row is *not* moved out of the queue and its claim is released, so the next claim picks it
// up at retryAt. Deleting it or parking it on the first failure would drop an event that the
// state change has already committed, which is exactly the loss the outbox exists to prevent —
// the broker being briefly unavailable is a normal condition, not a reason to lose a payment
// event.
func (o *OutboxRepository) MarkFailed(
	ctx context.Context, id shared.EventID, cause error, retryAt time.Time,
) error {
	if _, err := tenantOf(ctx); err != nil {
		return err
	}
	const q = `
UPDATE pp.outbox_events
SET publish_attempts = publish_attempts + 1,
    last_error       = left($2, 500),
    available_at     = $3,
    claimed_at       = NULL,
    claimed_by       = ''
WHERE event_id = $1 AND published_at IS NULL`

	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	if _, err := o.q.Exec(ctx, q, id.String(), msg, retryAt.UTC()); err != nil {
		return mapError(err, "mark outbox failed")
	}
	return nil
}

// Backlog returns the number of unpublished, due messages.
//
// It is exported as pp_outbox_backlog and alerted on, because a growing backlog is the earliest
// visible symptom of "the state is committing but nobody downstream knows" — and it is visible
// minutes before a merchant notices their webhooks stopped.
func (o *OutboxRepository) Backlog(ctx context.Context) (int, error) {
	if _, err := tenantOf(ctx); err != nil {
		return 0, err
	}
	const q = `
SELECT count(*) FROM pp.outbox_events
WHERE published_at IS NULL AND available_at <= now()`
	var n int
	if err := o.q.QueryRow(ctx, q).Scan(&n); err != nil {
		return 0, mapError(err, "outbox backlog")
	}
	return n, nil
}

// shardBuckets returns the hash buckets belonging to one relay replica.
//
// The mapping is `bucket % totalShards == shard`, computed here rather than in SQL so that the
// predicate stays an indexable `= ANY(...)`. Every bucket belongs to exactly one shard and every
// shard gets at least one bucket, which is what stops a replica from idling while another
// carries the whole backlog.
func shardBuckets(shard, totalShards int) ([]int16, error) {
	if totalShards <= 0 {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"postgres: outbox totalShards must be positive, got %d", totalShards)
	}
	if shard < 0 || shard >= totalShards {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"postgres: outbox shard %d is out of range for %d shards", shard, totalShards)
	}
	if totalShards > OutboxShardBuckets {
		// More replicas than buckets would leave the surplus replicas with nothing to claim.
		// Failing loudly beats a replica that runs, reports healthy, and does no work.
		return nil, apierror.Newf(apierror.CodeInternalError,
			"postgres: %d relay shards exceeds the %d outbox buckets; "+
				"raise OutboxShardBuckets and migrate the generated column together",
			totalShards, OutboxShardBuckets)
	}
	var out []int16
	for b := 0; b < OutboxShardBuckets; b++ {
		if b%totalShards == shard {
			out = append(out, int16(b))
		}
	}
	return out, nil
}

// claimOwner labels the claim so an operator can see which replica holds a stuck batch.
// It reads a caller-supplied identity from context when present and falls back to a constant;
// it is diagnostic only and nothing branches on it.
func claimOwner(ctx context.Context) string {
	if v, ok := ctx.Value(claimOwnerKey{}).(string); ok && v != "" {
		return v
	}
	return "outbox-relay"
}

type claimOwnerKey struct{}

// WithClaimOwner labels outbox claims made under ctx with the calling replica's identity.
func WithClaimOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, claimOwnerKey{}, owner)
}

func nonNilHeaders(h map[string]string) map[string]string {
	if h == nil {
		return map[string]string{}
	}
	return h
}

func orNow(t, now time.Time) time.Time {
	if t.IsZero() {
		return now
	}
	return t.UTC()
}
