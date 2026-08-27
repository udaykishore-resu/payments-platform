package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// WebhookRepository persists inbound gateway notifications and their deduplication state.
//
// Store first, process later — always. A webhook endpoint that processes synchronously is an
// endpoint that times out under load, which makes the gateway retry, which multiplies the load
// exactly when it is highest. The 50 ms budget on the ingress buys the platform the right to be
// slow at *processing* without being slow at *accepting*.
type WebhookRepository struct {
	q      querier
	tenant shared.TenantID
	clock  shared.Clock
}

var _ ports.WebhookRepository = (*WebhookRepository)(nil)

// webhookDedupRetention must exceed every gateway's own retry window. Thirty days is longer than
// any of the three integrated gateways retries for; a shorter window would let a gateway's last
// retry arrive after we had forgotten the first delivery, and be processed a second time.
const webhookDedupRetention = 30 * 24 * time.Hour

// Record stores a raw webhook, returning false if it is a duplicate.
//
// The deduplication is the unique index on (gateway_id, gateway_event_id) — done at the storage
// layer rather than in memory, so it survives a pod restart and works across replicas. Those are
// exactly the two conditions under which an in-memory set silently stops deduplicating, and both
// of them happen every deploy.
//
// The claim goes into pp.webhook_dedup with ON CONFLICT DO NOTHING and is checked *before* the
// body is written. A duplicate therefore costs one small insert rather than a megabyte of body
// re-written on every one of a gateway's retries.
func (r *WebhookRepository) Record(ctx context.Context, w ports.InboundWebhook) (bool, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return false, err
	}
	const claimQ = `
INSERT INTO pp.webhook_dedup (gateway_id, gateway_event_id, webhook_id, first_seen_at, expires_at)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (gateway_id, gateway_event_id) DO NOTHING
RETURNING webhook_id`

	now := r.clock.Now()
	var claimed string
	err := r.q.QueryRow(ctx, claimQ,
		w.GatewayID.String(), w.GatewayEventID, w.ID.String(),
		now, now.Add(webhookDedupRetention)).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already seen. Dropped silently and counted, never re-processed.
		return false, nil
	}
	if err != nil {
		return false, mapError(err, "claim webhook dedup")
	}

	const q = `
INSERT INTO pp.inbound_webhooks (
    webhook_id, tenant_id, merchant_id, gateway_id, gateway_event_id, event_type,
    signature, signature_valid, signature_scheme, headers, raw_body, body_sha256,
    status, attempts, received_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`

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

	if _, err := r.q.Exec(ctx, q,
		w.ID.String(), nullTenant(w.TenantID), w.MerchantID.String(),
		w.GatewayID.String(), w.GatewayEventID, w.EventType,
		w.Signature, w.Signature != "", "", hdr, w.Payload, hex.EncodeToString(sum[:]),
		status, w.Attempts, received,
	); err != nil {
		return false, mapError(err, "record inbound webhook")
	}
	return true, nil
}

// Get loads one stored webhook, including its raw body.
//
// The body is retained verbatim because it is the only thing that can re-verify a signature
// during a dispute, and a normalized copy proves nothing about what the gateway actually sent.
func (r *WebhookRepository) Get(ctx context.Context, id shared.WebhookID) (*ports.InboundWebhook, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	w, err := scanWebhook(r.q.QueryRow(ctx, selectWebhook+" WHERE webhook_id = $1", id.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFound(apierror.CodeWebhookUnknownEventType, "webhook", id.String())
		}
		return nil, mapError(err, "get inbound webhook")
	}
	return w, nil
}

// ClaimUnprocessed takes a batch of webhooks for the async processor.
//
// FOR UPDATE SKIP LOCKED, exactly as the outbox relay does, and for the same reason: several
// processor replicas run concurrently and must not contend or duplicate. Ordering here is by
// arrival rather than by shard, because a webhook does not carry per-aggregate ordering
// semantics — it produces a domain *command*, and the Payment aggregate's state machine is what
// decides whether that command is legal in the order it arrives.
func (r *WebhookRepository) ClaimUnprocessed(ctx context.Context, limit int) ([]ports.InboundWebhook, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	const q = `
WITH claimed AS (
    SELECT webhook_id FROM pp.inbound_webhooks
    WHERE status IN ('RECEIVED','VERIFIED','RESOLVED')
      AND (retry_after IS NULL OR retry_after <= now())
    ORDER BY received_at
    FOR UPDATE SKIP LOCKED
    LIMIT $1
)
UPDATE pp.inbound_webhooks w
SET attempts = w.attempts + 1
FROM claimed c
WHERE w.webhook_id = c.webhook_id
RETURNING w.webhook_id, w.tenant_id, w.merchant_id, w.gateway_id, w.gateway_event_id,
          w.event_type, w.signature, w.signature_valid, w.headers, w.raw_body,
          w.status, w.attempts, w.last_error, w.received_at, w.processed_at`

	rows, err := r.q.Query(ctx, q, pageLimit(limit))
	if err != nil {
		return nil, mapError(err, "claim webhooks")
	}
	defer rows.Close()
	var out []ports.InboundWebhook
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, mapError(err, "claim webhooks")
		}
		out = append(out, *w)
	}
	return out, mapError(rows.Err(), "claim webhooks")
}

// MarkProcessed records a terminal outcome for a webhook.
func (r *WebhookRepository) MarkProcessed(ctx context.Context, id shared.WebhookID, result string) error {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return err
	}
	const q = `
UPDATE pp.inbound_webhooks
SET status = 'PROCESSED', processed_at = $2, last_error = '', retry_after = NULL,
    resolved_event_type = $3
WHERE webhook_id = $1`
	if _, err := r.q.Exec(ctx, q, id.String(), r.clock.Now(), result); err != nil {
		return mapError(err, "mark webhook processed")
	}
	return nil
}

// MarkFailed records a processing failure and schedules a retry.
//
// After the eighth attempt the webhook is PARKED rather than retried: the attempts column has a
// CHECK at eight, so a ninth increment would fail the write and lose the error message along
// with it. Parking makes the give-up explicit and puts the row on the DLQ triage index, which is
// what an operator actually needs — a webhook retried forever is invisible, and a webhook that
// silently stopped being retried is worse.
func (r *WebhookRepository) MarkFailed(
	ctx context.Context, id shared.WebhookID, cause error, retryAt time.Time,
) error {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return err
	}
	const q = `
UPDATE pp.inbound_webhooks
SET last_error  = left($2, 500),
    retry_after = $3,
    status      = CASE WHEN attempts >= 8 THEN 'PARKED' ELSE status END
WHERE webhook_id = $1`
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	if _, err := r.q.Exec(ctx, q, id.String(), msg, retryAt.UTC()); err != nil {
		return mapError(err, "mark webhook failed")
	}
	return nil
}

const selectWebhook = `
SELECT webhook_id, tenant_id, merchant_id, gateway_id, gateway_event_id, event_type,
       signature, signature_valid, headers, raw_body, status, attempts, last_error,
       received_at, processed_at
FROM pp.inbound_webhooks`

func scanWebhook(row scanRow) (*ports.InboundWebhook, error) {
	var (
		w                   ports.InboundWebhook
		id                  string
		tenant              *string
		merchant, gatewayID string
		signatureValid      bool
		hdrRaw              []byte
	)
	if err := row.Scan(&id, &tenant, &merchant, &gatewayID, &w.GatewayEventID, &w.EventType,
		&w.Signature, &signatureValid, &hdrRaw, &w.Payload, &w.Status, &w.Attempts,
		&w.LastError, &w.ReceivedAt, &w.ProcessedAt); err != nil {
		return nil, err
	}
	w.ID = shared.WebhookID(id)
	if tenant != nil {
		w.TenantID = shared.TenantID(*tenant)
	}
	w.MerchantID = shared.MerchantID(merchant)
	w.GatewayID = shared.GatewayID(gatewayID)
	if err := unmarshalJSON(hdrRaw, &w.Headers, "webhook headers"); err != nil {
		return nil, err
	}
	return &w, nil
}

// nullTenant maps an unresolved tenant to SQL NULL.
//
// A webhook's tenancy is genuinely unknown until its payload resolves to a payment, and the RLS
// policy on this table admits NULL for exactly that reason. Writing the empty string instead
// would make the row belong to a tenant whose identifier is "" — visible to nobody, and
// therefore invisible to the resolver that is supposed to claim it.
func nullTenant(t shared.TenantID) *string {
	if t.IsZero() {
		return nil
	}
	s := t.String()
	return &s
}
