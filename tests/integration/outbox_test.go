//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
	"github.com/udaykishore-resu/payments-platform/tests/testenv"
)

// The transactional outbox and its relay.
//
// Verifies: baseline §13.4 (outbox atomicity), ADR-010 (at-least-once, effectively-once),
// ADR-020 (Kafka event backbone), docs/testing.md §4.1 and FS-5.
//
// The property the relay exists to provide is not "events get published" — a loop over a table
// does that. It is **per-aggregate ordering under horizontal scale**, and it comes from the
// `shard_bucket` generated column rather than from `FOR UPDATE SKIP LOCKED`. That distinction is
// why this file runs two relays: SKIP LOCKED stops two replicas from claiming the same *row*; it
// does nothing to stop them claiming two rows of the same aggregate and publishing them out of
// order. A single-relay test would never notice.
//
// One property of the environment makes these assertions exact rather than statistical: this suite
// connects as pp_app, whose policy on pp.outbox_events is tenant-scoped, so both Claim and Backlog
// are filtered by RLS to the calling tenant. In production the relay runs as pp_relay, whose policy
// on this one table is USING (true) — deliberately platform-wide, and deliberately granted to a
// role with no access to payments, merchants or configurations. The relay's *ordering* logic is
// identical either way; what the tenant scoping buys here is a suite that can run in parallel
// without one test draining another's queue.

// relayShard drains one shard of the outbox.
type relayShard struct {
	shard, total int
	published    []ports.OutboxMessage
}

// claimOnce claims and publishes at most one batch, returning how many rows it took.
//
// Claim and MarkPublished share the transaction. In production the broker call sits between them,
// and a crash there produces a redelivery — which consumers dedupe — rather than a lost event.
// That ordering is the whole at-least-once contract, so the test mirrors it rather than marking
// rows published in a separate transaction.
func (r *relayShard) claimOnce(uow *postgres.UnitOfWork, tenant string) (int, error) {
	var batch []ports.OutboxMessage
	err := tryTx(uow, tenant, func(ctx context.Context, repos ports.Repositories) error {
		reader, ok := repos.Outbox.(ports.OutboxReader)
		if !ok {
			return errors.New("the outbox repository does not implement ports.OutboxReader; " +
				"the relay would have no way to claim")
		}
		claimed, err := reader.Claim(ctx, r.shard, r.total, 200)
		if err != nil {
			return err
		}
		ids := make([]shared.EventID, 0, len(claimed))
		for _, m := range claimed {
			// Belt and braces over the RLS filter. If this suite is ever pointed at a DSN whose
			// role sees every tenant, publishing another test's rows would corrupt that test's
			// assertions in a way that looks like a product bug.
			if m.TenantID.String() != tenant {
				continue
			}
			batch = append(batch, m)
			ids = append(ids, m.ID)
		}
		if len(ids) == 0 {
			return nil
		}
		return reader.MarkPublished(ctx, ids)
	})
	if err != nil {
		return 0, err
	}
	r.published = append(r.published, batch...)
	return len(batch), nil
}

// drainShards runs every shard concurrently until `total` rows have been published or the budget
// expires.
//
// The budget, not a retry count, is what bounds it: a shard that owns no keys claims nothing and
// must not be mistaken for a stuck one. Gosched rather than a sleep between empty claims — a sleep
// would be a guess about how long the other shard needs, and the loop already has a deadline that
// turns a genuine stall into a message naming what was drained and what was not.
func drainShards(t *testing.T, uow *postgres.UnitOfWork, tenant string, shards []*relayShard, total int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)

	var (
		mu   sync.Mutex
		got  int
		errs []error
	)
	var wg sync.WaitGroup
	for _, sh := range shards {
		wg.Add(1)
		go func(sh *relayShard) {
			defer wg.Done()
			for {
				mu.Lock()
				done := got >= total
				mu.Unlock()
				if done || time.Now().After(deadline) {
					return
				}
				n, err := sh.claimOnce(uow, tenant)
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					return
				}
				if n == 0 {
					runtime.Gosched()
					continue
				}
				mu.Lock()
				got += n
				mu.Unlock()
			}
		}(sh)
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("a relay shard failed: %v", errs[0])
	}
	if got < total {
		perShard := make([]string, 0, len(shards))
		for _, sh := range shards {
			perShard = append(perShard, fmt.Sprintf("shard %d/%d: %d", sh.shard, sh.total, len(sh.published)))
		}
		t.Fatalf("the relay published %d of %d events before its %s budget expired (%v). Either a "+
			"shard is starved or a row is not claimable by the shard its bucket belongs to.",
			got, total, budget, perShard)
	}
}

// seedOutbox writes `perKey` events for each key, in order, inside one transaction.
//
// One transaction on purpose: in production the outbox row and the state change it describes
// commit together, so a relay never sees a partially written aggregate. Seeding them separately
// would be testing a shape production cannot produce.
//
// The rows go in with raw SQL rather than through OutboxRepository.Append, and that is a
// workaround rather than a preference — see the note on repoWritesJSONB in repos_test.go. What is
// under test here is the *reader* side of the relay (Claim, MarkPublished, MarkFailed, Backlog),
// and every one of those is the real production statement.
func seedOutbox(t *testing.T, uow *postgres.UnitOfWork, s *testenv.Scope, tenant string, keys []string, perKey int) []string {
	t.Helper()
	var ids []string
	at := s.Clock.Now()
	c := ctx(t)

	if err := s.TenantedCommitted(c, tenant, func(tx pgx.Tx) error {
		for _, key := range keys {
			for i := 0; i < perKey; i++ {
				// The run token is in the seed because pp.outbox_events.event_id is globally
				// unique, and a run killed before its cleanup ran would otherwise poison every
				// later run.
				id := s.IDAt(testenv.PrefixEvent, at, fmt.Sprintf("outbox/%s/%s/%d", runToken, key, i))
				if _, err := tx.Exec(c, `
INSERT INTO pp.outbox_events (
    event_id, tenant_id, aggregate_type, aggregate_id, event_type, topic, partition_key,
    payload, headers, occurred_at, available_at)
VALUES ($1,$2,'payment',$3,'payment.attempted.v1','pp.payments.payment.v1',$3,$4,$5,$6,now())`,
					id, tenant, key,
					[]byte(fmt.Sprintf(`{"key":%q,"seq":%d}`, key, i)),
					[]byte(`{"pp-event-type":"payment.attempted.v1"}`),
					at.Add(time.Duration(i)*time.Millisecond)); err != nil {
					return err
				}
				ids = append(ids, id)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the outbox: %v", err)
	}
	return ids
}

// TestTwoRelayShardsPreservePerAggregateOrder is the ordering guarantee of §13.4.
//
// Verifies: baseline §13.4, docs/testing.md §4.1 ("TestRelaySkipLockedNoDoublePublish"), C-9.
//
// Three assertions, in increasing order of what they tell you:
//
//  1. Every event is published exactly once. A duplicate becomes a duplicate ledger entry
//     downstream, which the consumer's dedup would also catch — but relying on two mechanisms to
//     cover one bug is how both stop being maintained.
//  2. Within one partition key, the published order is the seeded order. Two shards are what make
//     this observable at all.
//  3. No partition key is drained by both shards. That is the mechanism behind (2).
func TestTwoRelayShardsPreservePerAggregateOrder(t *testing.T) {
	t.Parallel()
	_, s := setup(t)
	uow := newUoW(t, shared.SystemClock{})

	// Twelve keys over 64 buckets: both shards almost certainly get work, but the test asserts
	// only what it needs — exactly-once, order, and disjointness — so an unlucky split still
	// produces a meaningful pass rather than a flake.
	keys := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		keys = append(keys, fmt.Sprintf("pay_key_%s_%02d", s.Nonce()[:6], i))
	}
	const perKey = 5
	total := len(keys) * perKey
	seedOutbox(t, uow, s, s.TenantA, keys, perKey)

	shards := []*relayShard{{shard: 0, total: 2}, {shard: 1, total: 2}}
	drainShards(t, uow, s.TenantA, shards, total, 60*time.Second)

	// 1. Exactly once.
	seen := map[string]int{}
	shardOf := map[string]map[int]bool{}
	for _, sh := range shards {
		for _, m := range sh.published {
			seen[m.ID.String()]++
			if shardOf[m.PartitionKey] == nil {
				shardOf[m.PartitionKey] = map[int]bool{}
			}
			shardOf[m.PartitionKey][sh.shard] = true
		}
	}
	if len(seen) != total {
		t.Fatalf("%d distinct events published, want %d", len(seen), total)
	}
	var duplicated []string
	for id, n := range seen {
		if n != 1 {
			duplicated = append(duplicated, fmt.Sprintf("%s x%d", id, n))
		}
	}
	if len(duplicated) > 0 {
		sort.Strings(duplicated)
		t.Fatalf("%d events were published more than once: %v", len(duplicated), duplicated)
	}

	// 2. Per-key order. Each shard publishes sequentially, so its own slice is the delivery order
	//    for every key it owns.
	for _, sh := range shards {
		last := map[string]int{}
		for _, m := range sh.published {
			seq := sequenceOf(t, m)
			prev, ok := last[m.PartitionKey]
			switch {
			case !ok && seq != 0:
				t.Fatalf("shard %d delivered %s sequence %d first; the aggregate's history starts "+
					"in the middle", sh.shard, m.PartitionKey, seq)
			case ok && seq != prev+1:
				t.Fatalf("shard %d published %s sequence %d directly after %d; per-aggregate "+
					"ordering is broken", sh.shard, m.PartitionKey, seq, prev)
			}
			last[m.PartitionKey] = seq
		}
		for key, seq := range last {
			if seq != perKey-1 {
				t.Fatalf("shard %d ended %s at sequence %d, want %d", sh.shard, key, seq, perKey-1)
			}
		}
	}

	// 3. One aggregate, one shard.
	for key, set := range shardOf {
		if len(set) != 1 {
			t.Fatalf("partition key %s was drained by %d shards. SKIP LOCKED alone does not "+
				"prevent that; the shard_bucket column is what does, and it is not working.",
				key, len(set))
		}
	}

	requireCount(t, s, s.TenantA, 0, "unpublished outbox rows after the drain",
		`SELECT count(*) FROM pp.outbox_events WHERE tenant_id = $1 AND published_at IS NULL`,
		s.TenantA)
}

// sequenceOf reads the sequence this test stamped into the payload.
func sequenceOf(t *testing.T, m ports.OutboxMessage) int {
	t.Helper()
	var body struct {
		Key string `json:"key"`
		Seq int    `json:"seq"`
	}
	if err := json.Unmarshal(m.Payload, &body); err != nil {
		t.Fatalf("event %s: the payload is not the shape this test wrote: %v", m.ID, err)
	}
	return body.Seq
}

// TestAPublishFailureLeavesTheRowClaimable is the loss-prevention half of §13.4.
//
// Verifies: baseline §13.4, FS-5. A broker being briefly unavailable is a normal condition, not a
// reason to lose a payment event. The row must stay in the queue, its attempt count must rise so
// the failure is visible to the alert, and it must become claimable again — by any replica, not
// only the one that failed.
func TestAPublishFailureLeavesTheRowClaimable(t *testing.T) {
	t.Parallel()
	_, s := setup(t)
	uow := newUoW(t, shared.SystemClock{})

	key := "pay_fail_" + s.Nonce()[:8]
	ids := seedOutbox(t, uow, s, s.TenantA, []string{key}, 1)
	id := shared.EventID(ids[0])

	publishErr := errors.New("broker unavailable: no leader for partition")

	inTx(t, uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
		reader := r.Outbox.(ports.OutboxReader)
		claimed, err := reader.Claim(ctx, 0, 1, 200)
		if err != nil {
			return err
		}
		if !containsEvent(claimed, id) {
			return errors.New("the seeded event was not claimable at all")
		}
		// retryAt in the immediate past, so the row is due again with no waiting. A future
		// retryAt would be equally correct and would force this test to sleep — which is how a
		// deterministic assertion becomes a flake.
		return reader.MarkFailed(ctx, id, publishErr, time.Now().Add(-time.Second))
	})

	requireCount(t, s, s.TenantA, 1, "the failed event still queued, with its failure recorded",
		`SELECT count(*) FROM pp.outbox_events
		  WHERE tenant_id = $1 AND event_id = $2 AND published_at IS NULL
		    AND publish_attempts = 1 AND last_error <> '' AND claimed_by = ''`,
		s.TenantA, id.String())

	// A different relay claims and publishes it. Nothing about the first failure is sticky: the
	// claim is released rather than held until a lease expires, so recovery does not wait on the
	// dead replica.
	second := &relayShard{shard: 0, total: 1}
	n, err := second.claimOnce(uow, s.TenantA)
	if err != nil {
		t.Fatalf("second relay: %v", err)
	}
	if n != 1 || !containsEvent(second.published, id) {
		t.Fatalf("the event was not re-claimable after a publish failure; it is lost. "+
			"The second relay claimed %d row(s).", n)
	}

	requireCount(t, s, s.TenantA, 1, "the event published once, with its failure history kept",
		`SELECT count(*) FROM pp.outbox_events
		  WHERE tenant_id = $1 AND event_id = $2 AND published_at IS NOT NULL
		    AND publish_attempts = 1`,
		s.TenantA, id.String())
}

// TestBacklogMetricReflectsReality is the observability half of C-8 and FS-5.
//
// Verifies: baseline §13.4, docs/observability.md (`pp_outbox_backlog`). The backlog gauge is the
// earliest visible symptom of "the state is committing but nobody downstream knows", and it is
// what the OutboxBacklogGrowing alert reads. A gauge that under-reports is worse than no gauge: it
// turns a silent outage into a silent outage with a green dashboard.
//
// The assertion is made three times against the same source of truth — the table — so that a gauge
// which is merely *correlated* with the backlog rather than equal to it fails: after appending,
// after marking one row not-yet-due, and after draining.
func TestBacklogMetricReflectsReality(t *testing.T) {
	t.Parallel()
	_, s := setup(t)
	uow := newUoW(t, shared.SystemClock{})

	const events = 7
	key := "pay_backlog_" + s.Nonce()[:8]

	readGauge := func() int {
		t.Helper()
		var n int
		inTx(t, uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
			var err error
			n, err = r.Outbox.(ports.OutboxReader).Backlog(ctx)
			return err
		})
		return n
	}

	if got := readGauge(); got != 0 {
		t.Fatalf("this tenant's backlog is %d before the test wrote anything; a previous run left "+
			"rows behind and every assertion below would be off by that much", got)
	}

	ids := seedOutbox(t, uow, s, s.TenantA, []string{key}, events)
	if got := readGauge(); got != events {
		t.Fatalf("backlog gauge = %d after appending %d events, want %d", got, events, events)
	}
	requireCount(t, s, s.TenantA, events, "the table the gauge summarises",
		`SELECT count(*) FROM pp.outbox_events
		  WHERE tenant_id = $1 AND published_at IS NULL AND available_at <= now()`, s.TenantA)

	// A row scheduled for the future is not backlog — it is a retry that has not come due. A gauge
	// that counted it would fire the alert during normal backoff, which is how an alert gets muted.
	inTx(t, uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
		return r.Outbox.(ports.OutboxReader).MarkFailed(ctx, shared.EventID(ids[0]),
			errors.New("transient"), time.Now().Add(time.Hour))
	})
	if got := readGauge(); got != events-1 {
		t.Fatalf("backlog gauge = %d after deferring one event to a future retry, want %d",
			got, events-1)
	}

	// Bring it back and drain everything.
	inTx(t, uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
		return r.Outbox.(ports.OutboxReader).MarkFailed(ctx, shared.EventID(ids[0]),
			errors.New("transient"), time.Now().Add(-time.Second))
	})
	drainShards(t, uow, s.TenantA, []*relayShard{{shard: 0, total: 1}}, events, 60*time.Second)

	if got := readGauge(); got != 0 {
		t.Fatalf("backlog gauge = %d after a full drain, want 0", got)
	}
	requireCount(t, s, s.TenantA, 0, "unpublished rows after a full drain",
		`SELECT count(*) FROM pp.outbox_events WHERE tenant_id = $1 AND published_at IS NULL`,
		s.TenantA)
}

func containsEvent(msgs []ports.OutboxMessage, id shared.EventID) bool {
	for _, m := range msgs {
		if m.ID == id {
			return true
		}
	}
	return false
}
