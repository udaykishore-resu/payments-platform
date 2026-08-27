// Package repository is the only part of the codebase allowed to know SQL. Every query is
// parameterized (never string-concatenated) to eliminate SQL injection by construction, and the
// financial write path (CreatePayment) is a single database transaction so partial state is
// structurally impossible (see docs/adr/ADR-004-idempotency-ledger.md).
package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/payments-platform/services/payments-api/internal/domain"
)

// postgresSerializationFailure is the SQLSTATE Postgres returns when a SERIALIZABLE transaction
// loses a write-write conflict and must be retried by the caller. See ADR-004 and
// docs/04-failure-recovery-design.md ("Race condition on concurrent ledger writes").
const postgresSerializationFailure = "40001"

// postgresUniqueViolation is returned when the idempotency_keys unique index rejects a duplicate.
const postgresUniqueViolation = "23505"

type Repository struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, dsn string, maxConns, minConns int32, connTimeout time.Duration) (*Repository, error) {
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("repository: parse dsn: %w", err)
	}
	poolCfg.MaxConns = maxConns
	poolCfg.MinConns = minConns
	poolCfg.ConnConfig.ConnectTimeout = connTimeout

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("repository: create pool: %w", err)
	}
	return &Repository{pool: pool}, nil
}

func (r *Repository) Close() {
	r.pool.Close()
}

// Ping is used by the readiness probe. It deliberately uses a short, bounded timeout supplied by
// the caller — a slow DB should make the pod fail readiness quickly, not hang the probe.
func (r *Repository) Ping(ctx context.Context) error {
	return r.pool.Ping(ctx)
}

// Stat exposes pool statistics for the DB-connection-pool-saturation metric (see
// internal/observability/metrics.go).
func (r *Repository) Stat() *pgxpool.Stat {
	return r.pool.Stat()
}

// ExecRaw executes an arbitrary, parameter-free SQL statement (or script of statements). It
// exists strictly for schema migrations and test fixture seeding (see
// tests/integration/payment_flow_test.go and deploy tooling) — application request-handling code
// must never call this, since it bypasses the parameterized-query discipline that makes the rest
// of this package injection-safe by construction (docs/05-security-architecture.md).
func (r *Repository) ExecRaw(ctx context.Context, sql string) (pgconn.CommandTag, error) {
	return r.pool.Exec(ctx, sql)
}

// IsSerializationFailure reports whether err is a retryable SERIALIZABLE conflict. The service
// layer uses this to implement the bounded-retry-with-backoff loop described in ADR-004.
func IsSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == postgresSerializationFailure
	}
	return false
}

// CreatePaymentResult is returned by CreatePayment.
type CreatePaymentResult struct {
	Payment       domain.Payment
	IdempotentHit bool // true if this call short-circuited to a previously committed result
}

// CreatePayment is the single most important method in this service. It performs, in one
// SERIALIZABLE transaction:
//  1. Idempotency check (row lookup by key; if the stored request hash differs from this
//     request's hash, returns ErrIdempotencyConflict without writing anything).
//  2. Source/destination account existence, status, and currency validation.
//  3. Two balanced ledger entries (debit source, credit destination) — the database CHECK
//     constraint on ledger_entries guarantees these sum to zero for the payment or the COMMIT
//     itself fails, independent of anything this Go code does correctly or incorrectly.
//  4. One payments row.
//  5. One outbox_events row (the durable, atomic "please eventually tell downstream consumers"
//     record — see ADR-004).
//  6. One idempotency_keys row recording the response so retries short-circuit at step 1.
//
// All of this commits atomically or none of it happens. There is no code path that produces a
// partially-applied payment.
func (r *Repository) CreatePayment(ctx context.Context, in domain.CreatePaymentInput) (CreatePaymentResult, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return CreatePaymentResult{}, fmt.Errorf("repository: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) // no-op if already committed

	// Step 1: idempotency check, inside the same transaction as the write (ADR-004).
	var existingPaymentID, existingRequestHash string
	err = tx.QueryRow(ctx,
		`SELECT payment_id, request_hash FROM idempotency_keys WHERE key = $1 FOR UPDATE`,
		in.IdempotencyKey,
	).Scan(&existingPaymentID, &existingRequestHash)

	switch {
	case err == nil:
		if existingRequestHash != in.RequestHash {
			return CreatePaymentResult{}, domain.ErrIdempotencyConflict
		}
		payment, getErr := r.getPaymentTx(ctx, tx, existingPaymentID)
		if getErr != nil {
			return CreatePaymentResult{}, getErr
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return CreatePaymentResult{}, fmt.Errorf("repository: commit idempotent read: %w", commitErr)
		}
		return CreatePaymentResult{Payment: payment, IdempotentHit: true}, nil
	case errors.Is(err, pgx.ErrNoRows):
		// fall through to create a new payment
	default:
		return CreatePaymentResult{}, fmt.Errorf("repository: idempotency lookup: %w", err)
	}

	// Step 2: validate accounts.
	source, err := r.getAccountTx(ctx, tx, in.SourceAccountID)
	if err != nil {
		return CreatePaymentResult{}, err
	}
	dest, err := r.getAccountTx(ctx, tx, in.DestAccountID)
	if err != nil {
		return CreatePaymentResult{}, err
	}
	if source.Status != domain.AccountStatusActive {
		return CreatePaymentResult{}, domain.ErrAccountFrozen
	}
	if dest.Status != domain.AccountStatusActive {
		return CreatePaymentResult{}, domain.ErrAccountFrozen
	}
	if source.Currency != in.Amount.Currency || dest.Currency != in.Amount.Currency {
		return CreatePaymentResult{}, domain.ErrCurrencyMismatch
	}

	// Balance check: derived from ledger_entries, never from a mutable balance column (see
	// domain.Account doc comment). A hot-account optimization (materialized balance) can be
	// layered in later without changing this correctness model.
	var balance int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount_minor), 0) FROM ledger_entries WHERE account_id = $1`,
		in.SourceAccountID,
	).Scan(&balance); err != nil {
		return CreatePaymentResult{}, fmt.Errorf("repository: balance lookup: %w", err)
	}
	if balance < in.Amount.AmountMinor {
		return CreatePaymentResult{}, domain.ErrInsufficientFunds
	}

	// Step 3 + 4: payment row + two balanced ledger entries.
	paymentID := uuid.NewString()
	now := time.Now().UTC()

	if _, err := tx.Exec(ctx,
		`INSERT INTO payments (id, idempotency_key, source_account_id, dest_account_id, amount_minor, currency, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`,
		paymentID, in.IdempotencyKey, in.SourceAccountID, in.DestAccountID,
		in.Amount.AmountMinor, in.Amount.Currency, domain.PaymentStatusCompleted, now,
	); err != nil {
		return CreatePaymentResult{}, fmt.Errorf("repository: insert payment: %w", err)
	}

	debitID, creditID := uuid.NewString(), uuid.NewString()
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_entries (id, payment_id, account_id, amount_minor, currency, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6), ($7, $2, $8, $9, $5, $6)`,
		debitID, paymentID, in.SourceAccountID, -in.Amount.AmountMinor, in.Amount.Currency, now,
		creditID, in.DestAccountID, in.Amount.AmountMinor,
	); err != nil {
		return CreatePaymentResult{}, fmt.Errorf("repository: insert ledger entries: %w", err)
	}

	// Step 5: outbox row, same transaction — this is the crux of the whole durability story.
	envelope, err := json.Marshal(paymentCompletedEvent{
		EventID:     uuid.NewString(),
		PaymentID:   paymentID,
		SourceID:    in.SourceAccountID,
		DestID:      in.DestAccountID,
		AmountMinor: in.Amount.AmountMinor,
		Currency:    in.Amount.Currency,
		OccurredAt:  now,
	})
	if err != nil {
		return CreatePaymentResult{}, fmt.Errorf("repository: marshal outbox event: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox_events (id, aggregate_id, event_type, payload, published, attempts, created_at)
		 VALUES ($1, $2, 'payment.completed', $3, false, 0, $4)`,
		uuid.NewString(), paymentID, envelope, now,
	); err != nil {
		return CreatePaymentResult{}, fmt.Errorf("repository: insert outbox event: %w", err)
	}

	// Step 6: idempotency record.
	if _, err := tx.Exec(ctx,
		`INSERT INTO idempotency_keys (key, request_hash, payment_id, created_at)
		 VALUES ($1, $2, $3, $4)`,
		in.IdempotencyKey, in.RequestHash, paymentID, now,
	); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == postgresUniqueViolation {
			// Lost a race with a concurrent identical request between our lookup (step 1) and
			// this insert. Safe outcome: surface as a retryable conflict; the caller's retry
			// will hit the now-committed row at step 1 and get the idempotent result.
			return CreatePaymentResult{}, domain.ErrConcurrentModification
		}
		return CreatePaymentResult{}, fmt.Errorf("repository: insert idempotency key: %w", err)
	}

	// Step 7: audit log entry — append-only, immutable, compliance retention (see
	// docs/05-security-architecture.md).
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_log (id, actor, action, entity_type, entity_id, after, created_at)
		 VALUES ($1, $2, 'payment.created', 'payment', $3, $4, $5)`,
		uuid.NewString(), in.ClientID, paymentID, envelope, now,
	); err != nil {
		return CreatePaymentResult{}, fmt.Errorf("repository: insert audit log: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return CreatePaymentResult{}, fmt.Errorf("repository: commit: %w", err)
	}

	return CreatePaymentResult{
		Payment: domain.Payment{
			ID:              paymentID,
			IdempotencyKey:  in.IdempotencyKey,
			SourceAccountID: in.SourceAccountID,
			DestAccountID:   in.DestAccountID,
			Amount:          in.Amount,
			Status:          domain.PaymentStatusCompleted,
			CreatedAt:       now,
			UpdatedAt:       now,
		},
	}, nil
}

type paymentCompletedEvent struct {
	EventID     string    `json:"event_id"`
	PaymentID   string    `json:"payment_id"`
	SourceID    string    `json:"source_account_id"`
	DestID      string    `json:"dest_account_id"`
	AmountMinor int64     `json:"amount_minor"`
	Currency    string    `json:"currency"`
	OccurredAt  time.Time `json:"occurred_at"`
}

func (r *Repository) getAccountTx(ctx context.Context, tx pgx.Tx, id string) (domain.Account, error) {
	var a domain.Account
	err := tx.QueryRow(ctx,
		`SELECT id, currency, status FROM accounts WHERE id = $1`, id,
	).Scan(&a.ID, &a.Currency, &a.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Account{}, domain.ErrAccountNotFound
	}
	if err != nil {
		return domain.Account{}, fmt.Errorf("repository: get account: %w", err)
	}
	return a, nil
}

func (r *Repository) getPaymentTx(ctx context.Context, tx pgx.Tx, id string) (domain.Payment, error) {
	var p domain.Payment
	err := tx.QueryRow(ctx,
		`SELECT id, idempotency_key, source_account_id, dest_account_id, amount_minor, currency, status, created_at, updated_at
		 FROM payments WHERE id = $1`, id,
	).Scan(&p.ID, &p.IdempotencyKey, &p.SourceAccountID, &p.DestAccountID, &p.Amount.AmountMinor, &p.Amount.Currency, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Payment{}, domain.ErrPaymentNotFound
	}
	if err != nil {
		return domain.Payment{}, fmt.Errorf("repository: get payment: %w", err)
	}
	return p, nil
}

// GetPayment is the read path for GET /v1/payments/{id}. Uses the default (READ COMMITTED)
// isolation via a plain pool query — reads don't need SERIALIZABLE, only the write path does.
func (r *Repository) GetPayment(ctx context.Context, id string) (domain.Payment, error) {
	var p domain.Payment
	err := r.pool.QueryRow(ctx,
		`SELECT id, idempotency_key, source_account_id, dest_account_id, amount_minor, currency, status, created_at, updated_at
		 FROM payments WHERE id = $1`, id,
	).Scan(&p.ID, &p.IdempotencyKey, &p.SourceAccountID, &p.DestAccountID, &p.Amount.AmountMinor, &p.Amount.Currency, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Payment{}, domain.ErrPaymentNotFound
	}
	if err != nil {
		return domain.Payment{}, fmt.Errorf("repository: get payment: %w", err)
	}
	return p, nil
}

// ClaimOutboxBatch atomically claims up to `limit` unpublished outbox rows using
// FOR UPDATE SKIP LOCKED, so any number of pods' relay goroutines can poll concurrently without
// double-claiming the same row or needing a leader-election protocol (see ADR-003 and
// docs/04-failure-recovery-design.md, "Leader election failure").
func (r *Repository) ClaimOutboxBatch(ctx context.Context, limit int) ([]domain.OutboxEvent, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("repository: begin claim tx: %w", err)
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx,
		`SELECT id, aggregate_id, event_type, payload, attempts
		 FROM outbox_events
		 WHERE published = false
		 ORDER BY created_at
		 FOR UPDATE SKIP LOCKED
		 LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("repository: claim outbox batch: %w", err)
	}

	var events []domain.OutboxEvent
	for rows.Next() {
		var e domain.OutboxEvent
		if err := rows.Scan(&e.ID, &e.AggregateID, &e.EventType, &e.Payload, &e.Attempts); err != nil {
			rows.Close()
			return nil, fmt.Errorf("repository: scan outbox row: %w", err)
		}
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Mark as "attempted" immediately (increment attempts, still unpublished) within the same
	// claim transaction, so a crash between claim and publish doesn't leave the row claimable
	// forever by a dead process — it will simply be picked up again on the next poll with an
	// incremented attempt count, up to OutboxMaxAttempts before it's routed to a manual-review
	// state (see internal/outbox/relay.go).
	for _, e := range events {
		if _, err := tx.Exec(ctx, `UPDATE outbox_events SET attempts = attempts + 1 WHERE id = $1`, e.ID); err != nil {
			return nil, fmt.Errorf("repository: bump outbox attempts: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("repository: commit claim: %w", err)
	}
	return events, nil
}

func (r *Repository) MarkOutboxPublished(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE outbox_events SET published = true, published_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("repository: mark outbox published: %w", err)
	}
	return nil
}

// UnpublishedOutboxCount backs the outbox_unpublished_count gauge (docs/06-observability.md).
func (r *Repository) UnpublishedOutboxCount(ctx context.Context) (int64, error) {
	var count int64
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE published = false`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("repository: count unpublished outbox: %w", err)
	}
	return count, nil
}

// HashRequestBody produces the stable hash used to detect idempotency-key reuse with a different
// request body (see FR-2 in docs/01-requirements.md).
func HashRequestBody(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
