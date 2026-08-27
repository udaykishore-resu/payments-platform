//go:build integration

package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

// TestOutboxClaimUnderTwoRelaysPreservesPerKeyOrder is the test for the subtle half of the relay
// design, and it is worth being precise about what it proves.
//
// `FOR UPDATE SKIP LOCKED` alone stops two replicas from claiming the same *row* — and nothing
// else. It does not stop replica A from claiming event 1 of payment P while replica B claims
// event 2 of the same payment: both rows are unlocked and both are claimable. They would then be
// published concurrently, and a `payment.captured.v1` could reach the topic before the
// `payment.authorized.v1` that preceded it. Every consumer reasoning about that payment's
// history then sees a capture for a payment it believes was never authorized.
//
// The shard bucket is what prevents it. This test runs two concurrent relays over a backlog of
// interleaved events for several payments and asserts that, for every payment, the events one
// relay claimed are in insertion order and no two relays claimed events for the same payment.
func TestOutboxClaimUnderTwoRelaysPreservesPerKeyOrder(t *testing.T) {
	pool := testPool(t)
	seedTenants(t, pool)
	ctx := tenantContext(t, tenantAlpha)
	uow := testUnitOfWork(t, pool)

	// Interleave: three events for each of eight payments, appended round-robin so that a naive
	// claim ordered purely by insertion would hand adjacent events of one payment to different
	// relays.
	const (
		payments        = 8
		eventsPerAggreg = 3
	)
	keys := make([]string, payments)
	for i := range keys {
		keys[i] = shared.NewPaymentID().String()
	}

	for seq := 0; seq < eventsPerAggreg; seq++ {
		for _, key := range keys {
			msg := ports.OutboxMessage{
				ID:            shared.NewEventID(),
				TenantID:      tenantAlpha,
				Topic:         "pp.payments.payment.v1",
				Type:          "payment.test.v1",
				AggregateType: "payment",
				AggregateID:   key,
				PartitionKey:  key,
				Payload:       []byte(`{"seq":` + itoa(seq) + `}`),
				OccurredAt:    time.Now(),
				AvailableAt:   time.Now(),
			}
			if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
				return r.Outbox.Append(ctx, msg)
			}); err != nil {
				t.Fatalf("append: %v", err)
			}
		}
	}

	// Two relays, two shards, claiming concurrently.
	type claimed struct {
		shard int
		msgs  []ports.OutboxMessage
	}
	var (
		mu      sync.Mutex
		results []claimed
		wg      sync.WaitGroup
		start   = make(chan struct{})
	)
	for shard := 0; shard < 2; shard++ {
		wg.Add(1)
		go func(shard int) {
			defer wg.Done()
			<-start
			var mine []ports.OutboxMessage
			// Drain in several passes, as a real relay does.
			for pass := 0; pass < 5; pass++ {
				err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
					reader, ok := r.Outbox.(ports.OutboxReader)
					if !ok {
						t.Errorf("the outbox writer must also implement OutboxReader")
						return nil
					}
					batch, err := reader.Claim(WithClaimOwner(ctx, "relay-"+itoa(shard)),
						shard, 2, 10)
					if err != nil {
						return err
					}
					mine = append(mine, batch...)
					ids := make([]shared.EventID, 0, len(batch))
					for _, m := range batch {
						ids = append(ids, m.ID)
					}
					return reader.MarkPublished(ctx, ids)
				})
				if err != nil {
					t.Errorf("relay %d: %v", shard, err)
					return
				}
			}
			mu.Lock()
			results = append(results, claimed{shard: shard, msgs: mine})
			mu.Unlock()
		}(shard)
	}
	close(start)
	wg.Wait()

	// Every event for one partition key must have been claimed by exactly one relay. If two
	// relays hold events for the same key, ordering is no longer guaranteed no matter what each
	// of them does next.
	owner := map[string]int{}
	order := map[string][]string{}
	for _, res := range results {
		for _, m := range res.msgs {
			if prev, seen := owner[m.PartitionKey]; seen && prev != res.shard {
				t.Fatalf("partition key %s was claimed by relay %d and relay %d; per-aggregate "+
					"ordering is not preserved and events for this payment can be published "+
					"out of order", m.PartitionKey, prev, res.shard)
			}
			owner[m.PartitionKey] = res.shard
			order[m.PartitionKey] = append(order[m.PartitionKey], string(m.Payload))
		}
	}

	// And within a relay, the events for one key must arrive in insertion order.
	for key, seen := range order {
		for i := range seen {
			want := `{"seq":` + itoa(i) + `}`
			if seen[i] != want {
				t.Fatalf("partition key %s: event %d was claimed as %s, want %s — the claim is "+
					"not ordered by outbox_id within a bucket", key, i, seen[i], want)
			}
		}
	}

	total := 0
	for _, res := range results {
		total += len(res.msgs)
	}
	if total != payments*eventsPerAggreg {
		t.Fatalf("the two relays claimed %d of %d events between them", total, payments*eventsPerAggreg)
	}
}

// TestShardBucketsPartitionTheKeyspace. Every bucket belongs to exactly one shard and every shard
// gets at least one bucket, so no replica idles while another carries the backlog.
func TestShardBucketsPartitionTheKeyspace(t *testing.T) {
	for _, total := range []int{1, 2, 3, 4, 8, 12, OutboxShardBuckets} {
		seen := map[int16]int{}
		for shard := 0; shard < total; shard++ {
			buckets, err := shardBuckets(shard, total)
			if err != nil {
				t.Fatalf("shardBuckets(%d, %d): %v", shard, total, err)
			}
			if len(buckets) == 0 {
				t.Fatalf("shard %d of %d has no buckets and would idle", shard, total)
			}
			for _, b := range buckets {
				if prev, dup := seen[b]; dup {
					t.Fatalf("bucket %d belongs to both shard %d and shard %d", b, prev, shard)
				}
				seen[b] = shard
			}
		}
		if len(seen) != OutboxShardBuckets {
			t.Fatalf("total=%d covered %d of %d buckets", total, len(seen), OutboxShardBuckets)
		}
	}

	if _, err := shardBuckets(0, 0); err == nil {
		t.Fatal("zero shards must be rejected")
	}
	if _, err := shardBuckets(3, 3); err == nil {
		t.Fatal("a shard index equal to the count must be rejected")
	}
	if _, err := shardBuckets(0, OutboxShardBuckets+1); err == nil {
		t.Fatal("more shards than buckets must be rejected; the surplus replicas would run, " +
			"report healthy, and do no work")
	}
}

// TestOutboxMarkFailedReschedulesRatherThanDropping. The state change has already committed, so
// an event that cannot be published right now must stay in the queue: a broker being briefly
// unavailable is a normal condition, not a reason to lose a payment event.
func TestOutboxMarkFailedReschedulesRatherThanDropping(t *testing.T) {
	pool := testPool(t)
	seedTenants(t, pool)
	ctx := tenantContext(t, tenantAlpha)
	uow := testUnitOfWork(t, pool)

	id := shared.NewEventID()
	key := shared.NewPaymentID().String()
	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Outbox.Append(ctx, ports.OutboxMessage{
			ID: id, TenantID: tenantAlpha, Topic: "t", Type: "payment.test.v1",
			AggregateID: key, PartitionKey: key, Payload: []byte(`{}`),
			OccurredAt: time.Now(), AvailableAt: time.Now(),
		})
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		reader := r.Outbox.(ports.OutboxReader)
		return reader.MarkFailed(ctx, id, errDeliberate, time.Now().Add(time.Hour))
	}); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	tx, done := rawConn(t, pool, tenantAlpha)
	defer done()

	var attempts int
	var published *time.Time
	var availableAt time.Time
	if err := tx.QueryRow(context.Background(), `
SELECT publish_attempts, published_at, available_at
FROM pp.outbox_events WHERE event_id = $1`, id.String()).
		Scan(&attempts, &published, &availableAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if published != nil {
		t.Fatal("a failed publish must not mark the event published")
	}
	if attempts != 1 {
		t.Fatalf("publish_attempts = %d, want 1", attempts)
	}
	if !availableAt.After(time.Now().Add(30 * time.Minute)) {
		t.Fatalf("available_at = %v; the retry was not deferred", availableAt)
	}

	// And it must not be claimable until then, or the retry tier is not a retry tier.
	if err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		batch, err := r.Outbox.(ports.OutboxReader).Claim(ctx, 0, 1, 10)
		if err != nil {
			return err
		}
		for _, m := range batch {
			if m.ID == id {
				t.Error("a deferred event was claimed before its available_at")
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
}

// TestOutboxRefusesAnEmptyPartitionKey. An empty key hashes to one bucket for every aggregate,
// which serialises the whole platform's events behind one relay replica *and* destroys
// per-aggregate ordering by mixing unrelated aggregates into one claim batch.
func TestOutboxRefusesAnEmptyPartitionKey(t *testing.T) {
	pool := testPool(t)
	seedTenants(t, pool)
	ctx := tenantContext(t, tenantAlpha)
	uow := testUnitOfWork(t, pool)

	err := uow.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Outbox.Append(ctx, ports.OutboxMessage{
			ID: shared.NewEventID(), TenantID: tenantAlpha, Topic: "t",
			Type: "payment.test.v1", Payload: []byte(`{}`),
		})
	})
	if err == nil {
		t.Fatal("an outbox message with no partition key must be refused")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
