//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/tests/testenv"
)

// The idempotency contract, asserted against the real store and the real database.
//
// Verifies: baseline §14 (idempotency), §14.2 (fingerprint), §14.3 (lease and replay),
// amendment A6 (never block on another caller's lease), docs/testing.md §4.1 and FS-2.
//
// These tests run the production *statements* — postgres.IdempotencyStore's INSERT ... ON CONFLICT
// DO NOTHING and its conditional reclaim UPDATE — rather than re-implementing them. That
// distinction is the whole value: a test that issues its own equivalent SQL asserts that the test
// author understands the concurrency control, not that the shipped code does.
//
// The complementary tests in internal/infrastructure/postgres/concurrency_integration_test.go
// assert the same outcomes from inside that package, sequentially. These run in parallel, against
// a shared database, alongside every other test in this suite — which is the condition under which
// a claim that is only *usually* atomic actually fails.

// fingerprintOf renders a request body as the store's fingerprint format: 64 lowercase hex
// characters, which is what the schema's CHECK constraint accepts.
func fingerprintOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// idemKeyFor builds the full claim scope. The client's key is only unique within a tenant, a
// merchant and an endpoint (§14.1); scoping by the bare key would let one tenant's choice of key
// collide with another's, and the collision would look like a successful replay.
func idemKeyFor(tenant, merchant, key string) ports.IdempotencyKey {
	return ports.IdempotencyKey{
		TenantID:     shared.TenantID(tenant),
		MerchantID:   shared.MerchantID(merchant),
		Method:       "POST",
		PathTemplate: "/v1/payments",
		Key:          key,
	}
}

// claimRecordFor builds a claim with a live lease.
func claimRecordFor(key ports.IdempotencyKey, body string, lease time.Time) ports.IdempotencyRecord {
	return ports.IdempotencyRecord{
		Key:            key,
		Fingerprint:    fingerprintOf(body),
		LeaseExpiresAt: lease,
		ExpiresAt:      time.Now().UTC().Add(7 * 24 * time.Hour),
		RequestID:      "req-" + runToken,
		TraceID:        "trace-" + runToken,
	}
}

// insertPaymentWithKey writes a payment row carrying the client's idempotency key.
//
// The key is on the row so that "exactly one payment was created for this key" is a question the
// database can answer directly, rather than one inferred from a count of everything the test
// tenant owns — which would be wrong on the second run of the suite, because DELETE is revoked on
// pp.payments and the previous run's rows are still there.
func insertPaymentWithKey(ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant, merchant, id, key string) error {
	at := s.Clock.Now()
	_, err := tx.Exec(ctx, `
INSERT INTO pp.payments (
    payment_id, partition_month, tenant_id, merchant_id, state, amount, currency,
    payment_method, capture_method, method_token, method_brand, method_last4,
    captured_amount, refunded_amount, idempotency_key, correlation_id,
    version, created_at, updated_at)
VALUES ($1,$2,$3,$4,'CREATED',$5,'USD','CARD','MANUAL','tok_test_visa','visa','4242',
        0,0,$6,$7,0,$8,$8)`,
		id, testenv.PartitionMonth(at), tenant, merchant, int64(5_000), key, "corr-"+runToken, at)
	return err
}

// TestConcurrentIdenticalCreatesYieldExactlyOnePayment is FS-2(b).
//
// Verifies: baseline §14.3, amendment A6. Sixteen callers submit the identical request with the
// identical key at the same instant. Exactly one may execute; the rest must be told the operation
// is in progress and must *not* block on the winner's lease.
//
// The assertion that matters is the last one, and it is made at the database: one payment row
// carries this key. Counting successful HTTP responses would not distinguish "one payment was
// created" from "two were created and one call happened to fail afterwards".
func TestConcurrentIdenticalCreatesYieldExactlyOnePayment(t *testing.T) {
	t.Parallel()
	_, s := setup(t)
	uow := newUoW(t, shared.SystemClock{})

	const callers = 16
	key := idemKeyFor(s.TenantA, s.MerchantA, "concurrent-create-"+runToken)
	const body = `{"amount":5000,"currency":"USD"}`
	paymentID := s.IDAt(testenv.PrefixPayment, s.Clock.Now(), committedSeed("idem/concurrent"))

	var (
		mu       sync.Mutex
		outcomes = map[string]int{}
		failures []error
	)
	record := func(o ports.ClaimOutcome, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			failures = append(failures, err)
			return
		}
		outcomes[string(o)]++
	}

	// A channel barrier rather than a stagger: every goroutine is already parked on the receive
	// when the close happens, so they contend on the INSERT rather than arriving in a queue. A
	// sleep-based start would serialise them on a slow machine and the test would pass without
	// ever exercising the conflict.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			var outcome ports.ClaimOutcome
			err := tryTx(uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
				res, err := r.Idempotency.Claim(ctx, claimRecordFor(key, body, time.Now().UTC().Add(30*time.Second)))
				if err != nil {
					return err
				}
				outcome = res.Outcome
				return nil
			})
			if err != nil {
				record("", err)
				return
			}
			record(outcome, nil)

			if outcome != ports.ClaimNew {
				return
			}
			// Only the winner performs the operation. Everything after this point is what a
			// duplicate would double if the claim were not exclusive.
			c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := s.TenantedCommitted(c, s.TenantA, func(tx pgx.Tx) error {
				return insertPaymentWithKey(c, tx, s, s.TenantA, s.MerchantA, paymentID, key.Key)
			}); err != nil {
				record("", err)
				return
			}
			if err := tryTx(uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
				return r.Idempotency.Complete(ctx, key, ports.ResponseSnapshot{
					StatusCode: 201, Body: []byte(`{"id":"` + paymentID + `"}`), ResourceID: paymentID,
				})
			}); err != nil {
				record("", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("%d of %d callers errored; the first was: %v", len(failures), callers, failures[0])
	}
	if outcomes[string(ports.ClaimNew)] != 1 {
		t.Fatalf("%d callers were told to execute, want exactly 1. Outcomes: %s",
			outcomes[string(ports.ClaimNew)], describeOutcomes(outcomes))
	}
	// The losers split between IN_PROGRESS and REPLAY, and the split is a property of the race
	// rather than of the contract: a caller that arrives while the winner still holds the lease is
	// told the operation is in progress, and one that arrives after the winner completed is given
	// the stored response. Both are correct and which one a given caller sees depends on
	// scheduling, so asserting a fixed split would be asserting the scheduler.
	//
	// What is *not* allowed is any other outcome. A second NEW is a double charge; a RECLAIMED
	// means a live lease was stolen; a FINGERPRINT_MISMATCH means identical bodies hashed
	// differently, which would make every legitimate retry a 422.
	losers := outcomes[string(ports.ClaimInProgress)] + outcomes[string(ports.ClaimReplay)]
	if losers != callers-1 {
		t.Fatalf("%d callers were told IN_PROGRESS or REPLAY, want %d. Outcomes: %s",
			losers, callers-1, describeOutcomes(outcomes))
	}
	for _, forbidden := range []ports.ClaimOutcome{ports.ClaimReclaimed, ports.ClaimFingerprintMismatch} {
		if n := outcomes[string(forbidden)]; n != 0 {
			t.Fatalf("%d callers got %s. Outcomes: %s", n, forbidden, describeOutcomes(outcomes))
		}
	}

	// The assertion the whole test exists for, made at the database.
	requireCount(t, s, s.TenantA, 1, "payments carrying this idempotency key",
		`SELECT count(*) FROM pp.payments WHERE tenant_id = $1 AND idempotency_key = $2`,
		s.TenantA, key.Key)
	requireCount(t, s, s.TenantA, 1, "idempotency records for this claim tuple",
		`SELECT count(*) FROM pp.idempotency_records
		  WHERE tenant_id = $1 AND merchant_id = $2 AND method = 'POST'
		    AND path_template = '/v1/payments' AND idempotency_key = $3`,
		s.TenantA, s.MerchantA, key.Key)
}

// TestReplayReturnsTheByteIdenticalStoredResponse is FS-2(a).
//
// Verifies: baseline §14.3. A completed operation replayed with the same key returns the *stored*
// response, not a freshly computed one. Byte identity is the assertion rather than semantic
// equivalence, because a response recomputed from current state is a different guarantee: it would
// drift the moment anything about the payment changed after completion.
func TestReplayReturnsTheByteIdenticalStoredResponse(t *testing.T) {
	t.Parallel()
	_, s := setup(t)
	uow := newUoW(t, shared.SystemClock{})

	key := idemKeyFor(s.TenantA, s.MerchantA, "replay-"+runToken)
	const body = `{"amount":5000,"currency":"USD"}`
	stored := ports.ResponseSnapshot{
		StatusCode: 201,
		Body:       []byte(`{"id":"pay_x","status":"processing","amount":5000}`),
		ResourceID: "pay_x",
	}

	inTx(t, uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
		res, err := r.Idempotency.Claim(ctx, claimRecordFor(key, body, time.Now().UTC().Add(30*time.Second)))
		if err != nil {
			return err
		}
		if res.Outcome != ports.ClaimNew {
			t.Fatalf("first claim outcome = %s, want NEW", res.Outcome)
		}
		return r.Idempotency.Complete(ctx, key, stored)
	})

	inTx(t, uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
		res, err := r.Idempotency.Claim(ctx, claimRecordFor(key, body, time.Now().UTC().Add(30*time.Second)))
		if err != nil {
			return err
		}
		if res.Outcome != ports.ClaimReplay {
			t.Fatalf("replay outcome = %s, want REPLAY", res.Outcome)
		}
		if res.Snapshot == nil {
			t.Fatal("REPLAY carried no snapshot; the caller has nothing to return to the client")
		}
		if res.Snapshot.StatusCode != stored.StatusCode {
			t.Fatalf("replayed status %d, want %d", res.Snapshot.StatusCode, stored.StatusCode)
		}
		if string(res.Snapshot.Body) != string(stored.Body) {
			t.Fatalf("replayed body is not byte-identical:\n got: %s\nwant: %s",
				res.Snapshot.Body, stored.Body)
		}
		if res.Snapshot.ResourceID != stored.ResourceID {
			t.Fatalf("replayed resource id %q, want %q", res.Snapshot.ResourceID, stored.ResourceID)
		}
		// The original request's identifiers travel with the replay so that two log lines for one
		// logical operation can be joined during an incident.
		if res.OriginalReqID == "" {
			t.Error("the replay did not carry the original request id; a duplicate and its original " +
				"cannot then be correlated in the logs")
		}
		return nil
	})

	requireCount(t, s, s.TenantA, 1, "idempotency records after a replay",
		`SELECT count(*) FROM pp.idempotency_records WHERE tenant_id = $1 AND idempotency_key = $2`,
		s.TenantA, key.Key)
}

// TestSameKeyWithADifferentBodyIsReportedAsReuse is FS-2(c).
//
// Verifies: baseline §14.2. The transport maps ClaimFingerprintMismatch to
// `422 IDEMPOTENCY_KEY_REUSED` (api/errors/catalog.yaml) — a 422 rather than a 409 because the
// client's request is unprocessable as stated, and rather than a replay because returning the
// first operation's response would tell the caller that a payment they never submitted succeeded.
//
// The store's outcome is asserted here rather than the HTTP status: this suite has no transport,
// and asserting the status in tests/e2e as well is what closes the loop.
func TestSameKeyWithADifferentBodyIsReportedAsReuse(t *testing.T) {
	t.Parallel()
	_, s := setup(t)
	uow := newUoW(t, shared.SystemClock{})

	key := idemKeyFor(s.TenantA, s.MerchantA, "reuse-"+runToken)

	cases := []struct {
		name  string
		first string
		then  string
		want  ports.ClaimOutcome
	}{
		{"identical body is in progress", `{"amount":5000}`, `{"amount":5000}`, ports.ClaimInProgress},
		{"different amount is reuse", `{"amount":5000}`, `{"amount":9900}`, ports.ClaimFingerprintMismatch},
		{"reordered but different body is reuse", `{"amount":5000}`, `{"amount":5000,"x":1}`, ports.ClaimFingerprintMismatch},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Deliberately NOT parallel: each case owns one claim tuple and asserts on the
			// second claim against it, which is a sequence, not a set.
			caseKey := key
			caseKey.Key = key.Key + "-" + string(rune('a'+i))

			inTx(t, uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
				res, err := r.Idempotency.Claim(ctx, claimRecordFor(caseKey, tc.first, time.Now().UTC().Add(60*time.Second)))
				if err != nil {
					return err
				}
				if res.Outcome != ports.ClaimNew {
					t.Fatalf("first claim = %s, want NEW", res.Outcome)
				}
				return nil
			})

			inTx(t, uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
				res, err := r.Idempotency.Claim(ctx, claimRecordFor(caseKey, tc.then, time.Now().UTC().Add(60*time.Second)))
				if err != nil {
					return err
				}
				if res.Outcome != tc.want {
					t.Fatalf("second claim = %s, want %s", res.Outcome, tc.want)
				}
				if res.Outcome == ports.ClaimFingerprintMismatch && res.Snapshot != nil {
					t.Fatal("a fingerprint mismatch carried a response snapshot; returning it would " +
						"answer one request with another request's result")
				}
				return nil
			})
		})
	}
}

// TestExpiredLeaseIsReclaimedExactlyOnce is the lease half of §14.3.
//
// Verifies: baseline §14.3, docs/testing.md §4.1 ("TestExpiredLeaseIsReclaimedAtomically").
//
// The scenario is a pod that claimed a key and died before completing. The lease expires and the
// operation must become executable again — but by exactly one caller. Eight callers race for it.
// A read-then-write reclaim would let several observe the expired lease and all proceed, which is
// a double charge produced by the very mechanism that exists to prevent one.
//
// The expiry is set by writing a lease timestamp that is already in the past rather than by
// waiting for one to elapse: the reclaim predicate compares against the database's now(), so a
// past timestamp is exactly as expired as a waited-out one and costs no wall-clock time.
func TestExpiredLeaseIsReclaimedExactlyOnce(t *testing.T) {
	t.Parallel()
	_, s := setup(t)
	uow := newUoW(t, shared.SystemClock{})

	key := idemKeyFor(s.TenantA, s.MerchantA, "reclaim-"+runToken)
	const body = `{"amount":5000,"currency":"USD"}`

	// The original holder: a live claim whose lease is already gone.
	inTx(t, uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
		res, err := r.Idempotency.Claim(ctx, claimRecordFor(key, body, time.Now().UTC().Add(-time.Minute)))
		if err != nil {
			return err
		}
		if res.Outcome != ports.ClaimNew {
			t.Fatalf("seed claim = %s, want NEW", res.Outcome)
		}
		return nil
	})

	const racers = 8
	var (
		mu       sync.Mutex
		outcomes = map[string]int{}
		failures []error
	)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var outcome ports.ClaimOutcome
			err := tryTx(uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
				// Every racer offers a live lease of its own, exactly as a real retry would.
				res, err := r.Idempotency.Claim(ctx, claimRecordFor(key, body, time.Now().UTC().Add(30*time.Second)))
				if err != nil {
					return err
				}
				outcome = res.Outcome
				return nil
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			outcomes[string(outcome)]++
		}()
	}
	close(start)
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("%d racers errored; the first was: %v", len(failures), failures[0])
	}
	if got := outcomes[string(ports.ClaimReclaimed)]; got != 1 {
		t.Fatalf("%d racers reclaimed the expired lease, want exactly 1. Outcomes: %s.\n"+
			"More than one means the reclaim is not atomic and the operation would be executed twice.",
			got, describeOutcomes(outcomes))
	}
	if got := outcomes[string(ports.ClaimInProgress)]; got != racers-1 {
		t.Fatalf("%d racers were told IN_PROGRESS, want %d. Outcomes: %s",
			got, racers-1, describeOutcomes(outcomes))
	}

	requireCount(t, s, s.TenantA, 1, "idempotency records after the reclaim race",
		`SELECT count(*) FROM pp.idempotency_records WHERE tenant_id = $1 AND idempotency_key = $2`,
		s.TenantA, key.Key)
	requireCount(t, s, s.TenantA, 1, "records still IN_FLIGHT with a live lease",
		`SELECT count(*) FROM pp.idempotency_records
		  WHERE tenant_id = $1 AND idempotency_key = $2
		    AND state = 'IN_FLIGHT' AND lease_expires_at > now()`,
		s.TenantA, key.Key)
}

// TestAnExpiredLeaseCannotBeReclaimedWithADifferentBody closes the gap the reclaim path opens.
//
// Verifies: baseline §14.2 together with §14.3. Reclaim exists so a dead holder's operation can be
// retried — but the retry must be the *same* operation. Without the fingerprint in the reclaim
// predicate, a client that reused the key for a different request would inherit the dead holder's
// claim and execute something else under it, and the eventual replay would answer the first
// request with the second one's result.
func TestAnExpiredLeaseCannotBeReclaimedWithADifferentBody(t *testing.T) {
	t.Parallel()
	_, s := setup(t)
	uow := newUoW(t, shared.SystemClock{})

	key := idemKeyFor(s.TenantA, s.MerchantA, "reclaim-mismatch-"+runToken)

	inTx(t, uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
		res, err := r.Idempotency.Claim(ctx, claimRecordFor(key, `{"amount":5000}`, time.Now().UTC().Add(-time.Minute)))
		if err != nil {
			return err
		}
		if res.Outcome != ports.ClaimNew {
			t.Fatalf("seed claim = %s, want NEW", res.Outcome)
		}
		return nil
	})

	inTx(t, uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
		res, err := r.Idempotency.Claim(ctx, claimRecordFor(key, `{"amount":250000}`, time.Now().UTC().Add(30*time.Second)))
		if err != nil {
			return err
		}
		if res.Outcome != ports.ClaimFingerprintMismatch {
			t.Fatalf("claiming an expired lease with a different body = %s, want FINGERPRINT_MISMATCH.\n"+
				"Any other outcome means a client could execute a different operation under a dead "+
				"holder's claim.", res.Outcome)
		}
		return nil
	})
}
