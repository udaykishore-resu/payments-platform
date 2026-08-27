package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// WebhookIngestStore records a gateway delivery before anyone knows whose it is.
//
// # Why this exists at all, when WebhookRepository already records webhooks
//
// Every other write in this package refuses to run without a tenant in the context, and that
// refusal is the platform's isolation guarantee rather than a convenience (see tenant.go). The
// inbound webhook is the one row the platform must be able to write *before* it can know the
// tenant: a gateway posts to `POST /v1/webhooks/{gateway}` holding no platform credential, and
// the delivery's tenancy is not knowable until the payload has been resolved to a payment — which
// happens later, in the processor, deliberately after the 202.
//
// Rather than inventing a tenant to satisfy the guard — which would file every gateway's
// deliveries under a fictitious tenant and make the isolation check meaningless everywhere — this
// store writes the row exactly as `migrations/0009_webhooks.up.sql` says it should be written:
// with `tenant_id` NULL, under the one RLS policy in the schema that admits a NULL tenant, and
// through a type narrow enough that its whole surface can be read in one screen.
//
// # Why it refuses a record that names a tenant
//
// The moment tenancy is known, the write belongs to the tenanted repository, where RLS's
// WITH CHECK clause proves the row is being filed under the tenant the transaction was opened
// for. Accepting a tenant here would create a second, unchecked path to a tenanted row — the
// exact hole the resolver in tenant.go exists to close — so it is refused rather than trusted.
type WebhookIngestStore struct {
	pool  *Pool
	clock shared.Clock
}

// NewWebhookIngestStore builds the ingress's platform-scoped recorder.
//
// It takes the pool rather than a unit of work because there is no unit of work to take: the
// UnitOfWork resolves a tenant before it will begin a transaction, which is precisely the thing
// this path cannot do. The dedup claim and the body insert still run in one transaction, so a
// claimed event id can never outlive a failed body write and permanently swallow a redelivery.
func NewWebhookIngestStore(pool *Pool, clock shared.Clock) *WebhookIngestStore {
	if clock == nil {
		clock = shared.SystemClock{}
	}
	return &WebhookIngestStore{pool: pool, clock: clock}
}

var _ ports.WebhookRecorder = (*WebhookIngestStore)(nil)

// Record stores a raw delivery, returning false if the gateway has sent it before.
//
// The deduplication claim goes in first and the body second, in that order and inside one
// transaction. The order is what makes a gateway's retry cheap — a duplicate costs one small
// insert rather than a megabyte of body rewritten — and the transaction is what stops a crash
// between the two from leaving a claim with no delivery behind it, which would make the platform
// answer 200 forever to an event it never actually stored.
func (s *WebhookIngestStore) Record(ctx context.Context, w ports.InboundWebhook) (bool, error) {
	if !w.TenantID.IsZero() {
		return false, apierror.New(apierror.CodeTenantMismatch,
			"postgres: a webhook whose tenant is already known must be recorded through the "+
				"tenanted repository, where row-level security checks the write")
	}

	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return false, mapError(err, "postgres: begin webhook ingest")
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// Rolled back on a detached context for the same reason UnitOfWork does it: when the
		// gateway hung up, ctx is already cancelled and a rollback on it would fail, leaving the
		// transaction to be reaped by idle_in_transaction_session_timeout while holding its locks.
		// The error is captured rather than discarded so the intent is visible: there is nothing
		// a caller could do with a failed rollback of a transaction that is already being
		// abandoned, and the connection is dropped either way.
		if rollbackErr := tx.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil {
			_ = rollbackErr
		}
	}()

	const claimQ = `
INSERT INTO pp.webhook_dedup (gateway_id, gateway_event_id, webhook_id, first_seen_at, expires_at)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (gateway_id, gateway_event_id) DO NOTHING
RETURNING webhook_id`

	now := s.clock.Now()
	var claimed string
	err = tx.QueryRow(ctx, claimQ,
		w.GatewayID.String(), w.GatewayEventID, w.ID.String(),
		now, now.Add(webhookDedupRetention)).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already seen. Dropped silently and counted, never re-processed. The transaction is
		// rolled back by the deferred call: there is nothing to commit.
		return false, nil
	}
	if err != nil {
		return false, mapError(err, "claim webhook dedup")
	}

	const insertQ = `
INSERT INTO pp.inbound_webhooks (
    webhook_id, tenant_id, merchant_id, gateway_id, gateway_event_id, event_type,
    signature, signature_valid, signature_scheme, headers, raw_body, body_sha256,
    status, attempts, received_at)
VALUES ($1,NULL,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`

	hdr, err := json.Marshal(nonNilHeaders(w.Headers))
	if err != nil {
		return false, apierror.Wrap(err, apierror.CodeInternalError, "postgres: encode webhook headers")
	}
	sum := sha256.Sum256(w.Payload)
	status := w.Status
	if status == "" {
		status = "RECEIVED"
	}
	received := w.ReceivedAt
	if received.IsZero() {
		received = now
	}

	if _, err := tx.Exec(ctx, insertQ,
		w.ID.String(), w.MerchantID.String(),
		w.GatewayID.String(), w.GatewayEventID, w.EventType,
		w.Signature, w.Signature != "", "", hdr, w.Payload, hex.EncodeToString(sum[:]),
		status, w.Attempts, received,
	); err != nil {
		return false, mapError(err, "record inbound webhook")
	}

	if err := tx.Commit(ctx); err != nil {
		return false, mapError(err, "postgres: commit webhook ingest")
	}
	committed = true
	return true, nil
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: FR-74, FR-75.
//
// The ingress's platform-scoped write of a delivery whose tenant is not yet known, kept separate
// from the tenanted repository so that no tenanted row can be written without an isolation check
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
