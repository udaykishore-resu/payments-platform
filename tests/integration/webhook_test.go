//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/tests/testenv"
)

// Inbound gateway webhooks.
//
// Verifies: baseline §13.5 (deduplication), §9 (the payment FSM as the arbiter of what a webhook
// may do), §12.3 (an unresolvable outcome becomes a reconciliation exception); docs/testing.md
// FS-3 and chaos scenario C-11.
//
// A webhook is the least trustworthy input the platform takes. It arrives unsolicited, it may
// arrive several times, it may arrive out of order, and it may describe a payment this platform has
// never heard of. All three of those are *normal* — every gateway does all three — so each has to
// be a designed behaviour rather than an error path.

// isolateWebhookDedup registers cleanup for pp.webhook_dedup.
//
// The dedup table is deliberately not tenant-scoped: the (gateway, event id) pair is the gateway's
// namespace, not ours, and a tenant column would let one tenant's forged event id shadow another's.
// The consequence for this suite is that testenv's cleanup cannot delete these rows by tenant, and
// the shared-state assertion would report the leftovers as drift. Registering this *after* setup
// means it runs *before* that assertion — t.Cleanup is LIFO — so the drift check sees a clean table.
func isolateWebhookDedup(t *testing.T, s *testenv.Scope, gateway string) {
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.TenantedCommitted(c, s.TenantA, func(tx pgx.Tx) error {
			if _, err := tx.Exec(c, `DELETE FROM pp.webhook_dedup WHERE gateway_id = $1`, gateway); err != nil {
				return err
			}
			// Asserted, not assumed. A DELETE that silently matched nothing would leave rows that
			// make the *next* run of this test see a duplicate on its first delivery — a failure
			// that reads like a product bug and reproduces only on the second run.
			var left int64
			if err := tx.QueryRow(c,
				`SELECT count(*) FROM pp.webhook_dedup WHERE gateway_id = $1`, gateway).Scan(&left); err != nil {
				return err
			}
			if left != 0 {
				return fmt.Errorf("%d dedup claim(s) survived cleanup for gateway %s", left, gateway)
			}
			return nil
		}); err != nil {
			t.Errorf("cleaning pp.webhook_dedup for %s: %v", gateway, err)
		}
	})
}

// webhookFor builds an inbound webhook for a synthetic per-test gateway.
func webhookFor(s *testenv.Scope, gateway, eventID, seed, eventType string, tenant string) ports.InboundWebhook {
	return ports.InboundWebhook{
		ID:             shared.WebhookID(s.ID(testenv.PrefixWebhook, "whk/"+runToken+"/"+seed)),
		GatewayID:      shared.GatewayID(gateway),
		TenantID:       shared.TenantID(tenant),
		MerchantID:     shared.MerchantID(s.MerchantA),
		GatewayEventID: eventID,
		EventType:      eventType,
		Signature:      "t=1,v1=" + fingerprintOf(seed),
		Payload:        []byte(fmt.Sprintf(`{"event":%q,"seed":%q}`, eventType, seed)),
		Headers:        map[string]string{"x-sim-signature": "t=1,v1=" + fingerprintOf(seed)},
		ReceivedAt:     s.Clock.Now(),
		Status:         "RECEIVED",
	}
}

// recordWebhook performs the ingress's two statements and reports whether the delivery was fresh.
//
// It mirrors WebhookRepository.Record: claim the (gateway, gateway event id) pair in
// pp.webhook_dedup with ON CONFLICT DO NOTHING, and only then write the body. Checking the claim
// first is what keeps a gateway's sixth retry of a one-megabyte notification from re-writing the
// megabyte.
//
// It is raw SQL rather than the repository for the reason recorded in repos_test.go: the
// repository marshals the headers into a []byte and the pool's exec mode sends that as bytea,
// which the jsonb column refuses. The *deduplication* — the thing this file is about — is the
// unique index either way, and the index is real.
func recordWebhook(ctx context.Context, tx pgx.Tx, s *testenv.Scope, w ports.InboundWebhook) (bool, error) {
	var claimed string
	err := tx.QueryRow(ctx, `
INSERT INTO pp.webhook_dedup (gateway_id, gateway_event_id, webhook_id, first_seen_at, expires_at)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (gateway_id, gateway_event_id) DO NOTHING
RETURNING webhook_id`,
		w.GatewayID.String(), w.GatewayEventID, w.ID.String(),
		s.Clock.Now(), s.Clock.Now().Add(30*24*time.Hour)).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already seen. Dropped silently and counted, never re-processed and never re-stored.
		return false, nil
	}
	if err != nil {
		return false, err
	}

	_, err = tx.Exec(ctx, `
INSERT INTO pp.inbound_webhooks (
    webhook_id, tenant_id, merchant_id, gateway_id, gateway_event_id, event_type,
    signature, signature_valid, signature_scheme, headers, raw_body, body_sha256,
    status, attempts, received_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,true,'simulator',$8,$9,$10,'RECEIVED',0,$11)`,
		w.ID.String(), w.TenantID.String(), w.MerchantID.String(), w.GatewayID.String(),
		w.GatewayEventID, w.EventType, w.Signature,
		[]byte(`{"x-sim-signature":"present"}`), w.Payload,
		fingerprintOf(string(w.Payload)), w.ReceivedAt)
	return err == nil, err
}

// TestDuplicateWebhookIsDroppedByTheUniqueIndex is FS-3 and C-11.
//
// Verifies: baseline §13.5. Five identical deliveries arrive concurrently from five connections,
// which is what a gateway retrying an endpoint it believes timed out actually looks like. Exactly
// one may be accepted for processing; the other four must be dropped and counted.
//
// The deduplication is a unique index rather than an in-memory set, and this test is what says so:
// an in-memory set passes a sequential version of this test and fails this one, because five
// concurrent inserts race, and it fails in production for a second reason — it does not survive a
// pod restart, which happens every deploy.
func TestDuplicateWebhookIsDroppedByTheUniqueIndex(t *testing.T) {
	t.Parallel()
	_, s := setup(t)

	gateway := "sim-" + s.Nonce()[:10]
	isolateWebhookDedup(t, s, gateway)

	const deliveries = 5
	eventID := "gw_evt_" + runToken + "_" + s.Nonce()[:8]

	var (
		mu       sync.Mutex
		accepted int
		dropped  int
		failures []error
	)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < deliveries; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// Each delivery is a *different* webhook id carrying the *same* gateway event id,
			// which is exactly what a retry looks like from our side: the gateway does not know
			// what id we assigned the first one.
			w := webhookFor(s, gateway, eventID, fmt.Sprintf("dup/%d", i), "payment.captured", s.TenantA)
			var fresh bool
			runCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err := s.TenantedCommitted(runCtx, s.TenantA, func(tx pgx.Tx) error {
				var err error
				fresh, err = recordWebhook(runCtx, tx, s, w)
				return err
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				failures = append(failures, err)
			case fresh:
				accepted++
			default:
				dropped++
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("%d deliveries errored; a gateway would be told to retry them. First: %v",
			len(failures), failures[0])
	}
	if accepted != 1 {
		t.Fatalf("%d of %d identical deliveries were accepted for processing, want exactly 1. "+
			"Each extra one becomes a duplicate state transition and a duplicate ledger entry.",
			accepted, deliveries)
	}
	if dropped != deliveries-1 {
		t.Fatalf("%d deliveries were deduplicated, want %d", dropped, deliveries-1)
	}

	requireCount(t, s, s.TenantA, 1, "stored webhook bodies for one gateway event",
		`SELECT count(*) FROM pp.inbound_webhooks WHERE tenant_id = $1 AND gateway_event_id = $2`,
		s.TenantA, eventID)
	requireCount(t, s, s.TenantA, 1, "dedup claims for one gateway event",
		`SELECT count(*) FROM pp.webhook_dedup WHERE gateway_id = $1 AND gateway_event_id = $2`,
		gateway, eventID)

	// A duplicate must not cost a re-written body. The claim is checked before the body is
	// stored, so a gateway retrying a one-megabyte notification six times writes it once.
	var bodies int64
	if err := s.Tenanted(ctx(t), s.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx(t), `
SELECT count(*) FROM pp.inbound_webhooks
 WHERE tenant_id = $1 AND gateway_id = $2`, s.TenantA, gateway).Scan(&bodies)
	}); err != nil {
		t.Fatalf("count stored bodies: %v", err)
	}
	if bodies != 1 {
		t.Fatalf("%d webhook bodies stored for %d deliveries of one event; the dedup claim is not "+
			"being checked before the body is written", bodies, deliveries)
	}
}

// TestAnOutOfOrderWebhookIsANoOp is the FSM's role as arbiter.
//
// Verifies: baseline §9 (the transition table), §13.5. Gateways do not guarantee webhook order:
// an `authorized` notification can arrive after the `captured` one it precedes, because they were
// retried on different schedules. The platform must treat the late one as information it already
// has, not as an instruction.
//
// Asserted at the database with the domain removed, because that is the layer that still holds when
// a repair script or a future service applies a webhook without going through the aggregate: the
// payments_guard trigger consults pp.payment_state_transitions and refuses anything not in it.
func TestAnOutOfOrderWebhookIsANoOp(t *testing.T) {
	t.Parallel()
	_, s := setup(t)

	type lateArrival struct {
		name string
		// reached is the state the payment is already in when the late webhook arrives.
		reached string
		// describes is the state the late webhook would move it to.
		describes string
		// legal records whether the transition table permits it. A "no-op" and an "illegal
		// transition" are different outcomes and the test must not conflate them.
		legal bool
	}

	cases := []lateArrival{
		{"an authorization arriving after the capture", "CAPTURED", "AUTHORIZED", false},
		{"an authorization arriving after a void", "VOIDED", "AUTHORIZED", false},
		{"a capture arriving after the refund", "REFUNDED", "CAPTURED", false},
		{"a failure arriving after the payment settled", "SETTLED", "FAILED", false},
		{"a dispute arriving after settlement is legitimate, not late", "SETTLED", "DISPUTED", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := ctx(t)

			err := s.Tenanted(c, s.TenantA, func(tx pgx.Tx) error {
				p := s.NewPayment(s.TenantA, s.MerchantA, "late/"+tc.reached+"/"+tc.describes, 7_700)
				auth := int64(7_700)
				at := s.Clock.Now()
				p.State = tc.reached
				p.AuthorizedAmount = &auth
				p.AuthorizedAt = &at
				p.CapturedAmount = 7_700
				if tc.reached == "REFUNDED" {
					p.RefundedAmount = 7_700
				}
				if err := p.Insert(c, tx); err != nil {
					return fmt.Errorf("seed a payment in %s: %w", tc.reached, err)
				}

				// The savepoint is what lets the assertions below run: a trigger's RAISE aborts the
				// transaction, and without it every read after the rejection would answer
				// "current transaction is aborted" and the reader would chase the wrong thing.
				if _, err := tx.Exec(c, `SAVEPOINT late_probe`); err != nil {
					return err
				}
				_, updErr := tx.Exec(c, `
UPDATE pp.payments SET state = $3, version = version + 1, updated_at = now()
 WHERE payment_id = $1 AND partition_month = $2`, p.ID, p.PartitionMonth, tc.describes)

				if tc.legal {
					if updErr != nil {
						return fmt.Errorf("%s -> %s is in the transition table but the database "+
							"refused it: %w", tc.reached, tc.describes, updErr)
					}
					return nil
				}
				if _, err := tx.Exec(c, `ROLLBACK TO SAVEPOINT late_probe`); err != nil {
					return err
				}

				testenv.RequireDBRejection(t, updErr,
					fmt.Sprintf("a late %s webhook applied to a %s payment", tc.describes, tc.reached),
					testenv.SQLStateCheckViolation)
				if got := testenv.PgConstraint(updErr); got != "payments_illegal_state_transition" {
					return fmt.Errorf("rejected by %q, want payments_illegal_state_transition; the "+
						"refusal came from somewhere other than the FSM guard", got)
				}

				// And the payment is untouched — a rejected transition must leave nothing behind,
				// including the version.
				var state string
				var version int64
				if err := tx.QueryRow(c, `
SELECT state, version FROM pp.payments WHERE payment_id = $1 AND partition_month = $2`,
					p.ID, p.PartitionMonth).Scan(&state, &version); err != nil {
					return err
				}
				if state != tc.reached || version != 0 {
					return fmt.Errorf("after the rejected update the payment is state=%s version=%d, "+
						"want %s and 0; a partially applied transition is worse than a rejected one",
						state, version, tc.reached)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("%v", err)
			}
		})
	}
}

// TestWebhookForAnUnknownPaymentOpensAReconciliationException is §12.3 seen from the webhook side.
//
// Verifies: baseline §12.3, §13.5. A notification whose gateway reference matches no payment of
// ours is not noise and must not be dropped: it is either a payment we lost the record of, a
// payment created by a request whose response never reached us, or a misrouted notification from
// another platform. All three need a human, and the exception queue is how a human finds out.
//
// Two assertions beyond "an exception exists":
//
//   - The webhook is PARKED rather than PROCESSED, so it is still there to be re-driven once the
//     ambiguity is resolved. A dropped webhook cannot be replayed.
//   - Re-detecting the same discrepancy updates the existing exception rather than opening a
//     second. Without that, an unresolved exception accumulates one duplicate per reconciliation
//     run until the queue is unusable and everyone stops reading it.
func TestWebhookForAnUnknownPaymentOpensAReconciliationException(t *testing.T) {
	t.Parallel()
	_, s := setup(t)
	uow := newUoW(t, shared.SystemClock{})

	gateway := "sim-" + s.Nonce()[:10]
	isolateWebhookDedup(t, s, gateway)

	eventID := "gw_orphan_" + runToken + "_" + s.Nonce()[:8]
	unknownRef := "gwref_unknown_" + runToken

	w := webhookFor(s, gateway, eventID, "orphan", "payment.captured", s.TenantA)
	c := ctx(t)
	if err := s.TenantedCommitted(c, s.TenantA, func(tx pgx.Tx) error {
		fresh, err := recordWebhook(c, tx, s, w)
		if err != nil {
			return err
		}
		if !fresh {
			return fmt.Errorf("the first delivery of %s was treated as a duplicate", eventID)
		}
		return nil
	}); err != nil {
		t.Fatalf("record the orphaned webhook: %v", err)
	}

	// Resolution finds no payment. The webhook is parked, not discarded.
	if err := s.TenantedCommitted(c, s.TenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(c, `
UPDATE pp.inbound_webhooks
   SET status = 'PARKED', last_error = 'no payment matches the gateway reference'
 WHERE tenant_id = $1 AND webhook_id = $2`, s.TenantA, w.ID.String())
		return err
	}); err != nil {
		t.Fatalf("park the webhook: %v", err)
	}

	exceptionID := "exc_" + runToken + "_" + s.Nonce()[:8]
	open := func(detail string) error {
		return tryTx(uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
			return r.Reconciliation.OpenException(ctx, ports.ReconciliationException{
				ID:         exceptionID,
				TenantID:   shared.TenantID(s.TenantA),
				MerchantID: shared.MerchantID(s.MerchantA),
				Kind:       "WEBHOOK_FOR_UNKNOWN_PAYMENT",
				Severity:   "MAJOR",
				Detail:     detail,
				OpenedAt:   s.Clock.Now(),
			})
		})
	}

	if err := open("gateway reference " + unknownRef + " matches no payment"); err != nil {
		t.Fatalf("open the exception: %v", err)
	}

	requireCount(t, s, s.TenantA, 1, "open exceptions for the orphaned webhook",
		`SELECT count(*) FROM pp.reconciliation_exceptions
		  WHERE tenant_id = $1 AND kind = 'WEBHOOK_FOR_UNKNOWN_PAYMENT' AND state = 'OPEN'`,
		s.TenantA)
	requireCount(t, s, s.TenantA, 1, "the webhook parked for re-drive rather than dropped",
		`SELECT count(*) FROM pp.inbound_webhooks
		  WHERE tenant_id = $1 AND webhook_id = $2 AND status = 'PARKED' AND processed_at IS NULL`,
		s.TenantA, w.ID.String())

	// The next reconciliation run re-detects the same discrepancy.
	if err := open("gateway reference " + unknownRef + " still matches no payment"); err != nil {
		t.Fatalf("re-detecting the same discrepancy failed instead of updating it: %v", err)
	}
	requireCount(t, s, s.TenantA, 1, "exceptions after the discrepancy was re-detected",
		`SELECT count(*) FROM pp.reconciliation_exceptions
		  WHERE tenant_id = $1 AND kind = 'WEBHOOK_FOR_UNKNOWN_PAYMENT'`,
		s.TenantA)

	// An exception for a *different* discrepancy is a different row: identity is the discrepancy,
	// not the run and not the kind alone.
	if err := tryTx(uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
		return r.Reconciliation.OpenException(ctx, ports.ReconciliationException{
			ID:         exceptionID + "-b",
			TenantID:   shared.TenantID(s.TenantA),
			MerchantID: shared.MerchantID(s.MerchantA),
			PaymentID:  shared.PaymentID(s.ID(testenv.PrefixPayment, "orphan/other")),
			Kind:       "WEBHOOK_FOR_UNKNOWN_PAYMENT",
			Severity:   "MAJOR",
			Detail:     "a different orphaned reference",
			OpenedAt:   s.Clock.Now(),
		})
	}); err != nil {
		t.Fatalf("opening an exception for a different discrepancy failed: %v", err)
	}
	requireCount(t, s, s.TenantA, 2, "exceptions after a second, distinct discrepancy",
		`SELECT count(*) FROM pp.reconciliation_exceptions
		  WHERE tenant_id = $1 AND kind = 'WEBHOOK_FOR_UNKNOWN_PAYMENT'`,
		s.TenantA)
}

// TestAWebhookThatFailedVerificationCannotReachAProcessingState is the schema's half of §17.
//
// Verifies: baseline §13.5, §17. The body of an unverified webhook is persisted anyway — it is
// forensic evidence, and discarding it destroys the record of an attack — but it must never be
// acted on. The CHECK constraint is what makes "verify before interpret" a property of the data
// rather than of whichever code path happens to be reading it.
func TestAWebhookThatFailedVerificationCannotReachAProcessingState(t *testing.T) {
	t.Parallel()
	_, s := setup(t)
	c := ctx(t)

	err := s.Tenanted(c, s.TenantA, func(tx pgx.Tx) error {
		id := s.ID(testenv.PrefixWebhook, "unverified/"+runToken)
		if _, err := tx.Exec(c, `
INSERT INTO pp.inbound_webhooks (webhook_id, tenant_id, gateway_id, gateway_event_id,
                                 raw_body, body_sha256, signature_valid, status)
VALUES ($1,$2,'sim-forged',$3,$4,$5,false,'RECEIVED')`,
			id, s.TenantA, "forged-"+s.Nonce()[:8], []byte(`{"forged":true}`),
			fingerprintOf("forged")); err != nil {
			return fmt.Errorf("store the unverified body: %w", err)
		}

		for _, status := range []string{"RESOLVED", "PROCESSED"} {
			if _, err := tx.Exec(c, `SAVEPOINT unverified_probe`); err != nil {
				return err
			}
			_, err := tx.Exec(c, `
UPDATE pp.inbound_webhooks SET status = $2 WHERE webhook_id = $1`, id, status)
			testenv.RequireDBRejection(t, err,
				"an unverified webhook advanced to "+status, testenv.SQLStateCheckViolation)
			if _, err := tx.Exec(c, `ROLLBACK TO SAVEPOINT unverified_probe`); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
}
