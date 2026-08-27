package postgres

import (
	"context"
	"errors"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/ledger"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// LedgerRepository appends balanced transactions to the shadow double-entry ledger.
//
// There is no Update and no Delete, here or on the table: `pp_app` holds neither grant, and a
// trigger refuses both. A correction is a compensating transaction, and the visible scar it
// leaves in a merchant's statement is the feature — an exception resolved by editing history is
// an exception nobody can prove was ever there.
type LedgerRepository struct {
	q      querier
	tenant shared.TenantID
	clock  shared.Clock
}

var _ ports.LedgerRepository = (*LedgerRepository)(nil)

// Append writes one balanced transaction: every entry, plus the balance projection, in the
// caller's transaction.
//
// The whole group is the consistency unit (R-TX-3). Double-entry has no partial state: a group
// half-written is not a smaller posting, it is an unbalanced ledger, and an unbalanced ledger
// fails its own nightly assertion with no way to tell which half was lost.
//
// Idempotency comes from the unique index on (source_event_id, account, side), not from a
// pre-read. A redelivered `payment.captured.v1` collides on that index and the whole append
// fails — which is correct: the caller's dedup row will already have told it this event was
// handled, and reaching here at all means the dedup row and the entries were written in
// different transactions somewhere, which is the bug worth surfacing.
func (r *LedgerRepository) Append(ctx context.Context, tx *ledger.Transaction) error {
	if tx == nil {
		return apierror.New(apierror.CodeInternalError, "postgres: nil ledger transaction")
	}
	if err := requireOwner(ctx, r.tenant, tx.TenantID()); err != nil {
		return err
	}
	// Balance is asserted in the domain before it reaches storage. The deferred constraint
	// trigger on the table asserts it again at COMMIT, which catches the case this check cannot:
	// a caller that appended two groups sharing a transaction_group_id.
	if err := tx.Balance(); err != nil {
		return err
	}

	const entryQ = `
INSERT INTO pp.ledger_entries (
    entry_id, partition_month, tenant_id, merchant_id, account_type, side, amount, currency,
    entry_type, transaction_group_id, source_event_id, source_event_type,
    payment_id, attempt_id, refund_id, gateway_ref, description, occurred_at, recorded_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`

	// The balance projection is maintained in the same transaction as the entries, so it can
	// never be ahead of or behind them at a commit boundary. The entries stay authoritative: a
	// nightly job recomputes the fold and raises a CRITICAL exception on drift, because a
	// projection nobody re-derives is a projection that quietly stops being true.
	const balanceQ = `
INSERT INTO pp.ledger_accounts (
    account_id, tenant_id, merchant_id, currency, account_type, normal_side,
    balance, entry_count, status, version, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,1,'OPEN',1,$8,$8)
ON CONFLICT (tenant_id, merchant_id, currency, account_type) DO UPDATE SET
    balance     = pp.ledger_accounts.balance + EXCLUDED.balance,
    entry_count = pp.ledger_accounts.entry_count + 1,
    version     = pp.ledger_accounts.version + 1,
    updated_at  = EXCLUDED.updated_at`

	for _, e := range tx.Entries() {
		if _, err := r.q.Exec(ctx, entryQ,
			e.ID().String(), e.PartitionMonth(), e.TenantID().String(), e.MerchantID().String(),
			string(e.Account()), string(e.Side()), e.Amount().Amount(), string(e.Currency()),
			string(e.Type()), e.TransactionID().String(),
			"", string(e.Type()),
			e.PaymentID().String(), e.AttemptID().String(), e.RefundID().String(),
			e.GatewayRef(), e.Description(), e.OccurredAt(), e.RecordedAt(),
		); err != nil {
			return mapError(err, "append ledger entry")
		}

		// Signed against the account's normal side: a debit to a debit-normal account increases
		// it, a credit decreases it. Storing the signed delta rather than the raw amount is what
		// makes the projection a plain sum, so recomputing it is `SUM(...)` rather than a
		// procedure that has to know the chart of accounts.
		delta := e.Amount().Amount()
		if !e.IsIncrease() {
			delta = -delta
		}
		if _, err := r.q.Exec(ctx, balanceQ,
			ledgerAccountID(e.AccountKey()), e.TenantID().String(), e.MerchantID().String(),
			string(e.Currency()), string(e.Account()), string(e.Account().NormalSide()),
			delta, r.clock.Now(),
		); err != nil {
			return mapError(err, "update ledger balance")
		}
	}
	return nil
}

// Balance returns an account's current balance from the projection.
//
// It reads the projection rather than folding the entries, because folding a merchant's whole
// entry history to answer "what is the balance" is a scan that grows without bound. The
// projection's correctness is guaranteed by it being written in the same transaction as the
// entries, and checked by the nightly recomputation.
func (r *LedgerRepository) Balance(ctx context.Context, key ledger.AccountKey) (money.Money, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return money.Money{}, err
	}
	if err := key.Validate(); err != nil {
		return money.Money{}, err
	}
	const q = `
SELECT balance FROM pp.ledger_accounts
WHERE tenant_id = $1 AND merchant_id = $2 AND currency = $3 AND account_type = $4`

	var balance int64
	err := r.q.QueryRow(ctx, q, key.TenantID.String(), key.MerchantID.String(),
		string(key.Currency), string(key.Type)).Scan(&balance)
	if err != nil {
		if errors.Is(err, ErrNoRows) {
			// An account that has never been posted to has a zero balance. That is a fact, not a
			// missing row: accounts are opened lazily on first posting precisely so that six
			// account types times forty currencies do not become hundreds of permanently-zero
			// rows per merchant that every report has to filter out.
			return money.Zero(key.Currency), nil
		}
		return money.Money{}, mapError(err, "read ledger balance")
	}
	return money.New(balance, key.Currency)
}

// EntriesForPayment returns every ledger line touching one payment, oldest first.
//
// It is the first query in every payment dispute, which is why the index behind it is on
// (payment_id, partition_month): the partition is derived from the payment's ULID, so all of a
// payment's entries — including a refund six months later and a chargeback a year after that —
// live in one partition and are read with one scan rather than one per month since the capture.
func (r *LedgerRepository) EntriesForPayment(
	ctx context.Context, id shared.PaymentID,
) ([]ledger.Entry, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	const q = `
SELECT entry_id, partition_month, tenant_id, merchant_id, account_type, side, amount, currency,
       entry_type, transaction_group_id, payment_id, attempt_id, refund_id,
       gateway_ref, description, occurred_at, recorded_at
FROM pp.ledger_entries
WHERE tenant_id = $1 AND payment_id = $2 AND partition_month = $3
ORDER BY occurred_at ASC, entry_id ASC`

	rows, err := r.q.Query(ctx, q, r.tenant.String(), id.String(), shared.PartitionMonth(id))
	if err != nil {
		return nil, mapError(err, "list ledger entries for payment")
	}
	defer rows.Close()

	var out []ledger.Entry
	for rows.Next() {
		var (
			p                  ledger.RehydrateEntryParams
			entryID, tenantID  string
			merchantID         string
			accountType, side  string
			amount             int64
			currency           string
			entryType, groupID string
			paymentID          string
			attemptID          string
			refundID           string
		)
		if err := rows.Scan(&entryID, &p.PartitionMonth, &tenantID, &merchantID,
			&accountType, &side, &amount, &currency, &entryType, &groupID,
			&paymentID, &attemptID, &refundID,
			&p.GatewayRef, &p.Description, &p.OccurredAt, &p.RecordedAt); err != nil {
			return nil, mapError(err, "list ledger entries for payment")
		}
		c := money.Currency(currency)
		if !c.IsSupported() {
			return nil, apierror.Newf(apierror.CodeInternalError,
				"ledger entry %s is denominated in unsupported currency %q", entryID, currency)
		}
		p.ID = shared.LedgerID(entryID)
		p.TenantID = shared.TenantID(tenantID)
		p.MerchantID = shared.MerchantID(merchantID)
		p.TransactionID = ledger.TransactionID(groupID)
		p.Account = ledger.AccountType(accountType)
		p.Side = ledger.Side(side)
		p.Amount = money.MustNew(amount, c)
		p.EntryType = ledger.EntryType(entryType)
		p.PaymentID = shared.PaymentID(paymentID)
		p.AttemptID = shared.AttemptID(attemptID)
		p.RefundID = shared.RefundID(refundID)

		e, err := ledger.RehydrateEntry(p)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, mapError(rows.Err(), "list ledger entries for payment")
}

// ledgerAccountID derives the account row's surrogate identifier from its natural key.
//
// Deriving rather than minting means a concurrent first-posting from two consumers targets the
// same row and contends on the ON CONFLICT, instead of inserting two rows that the unique index
// then rejects with an error nobody can act on.
func ledgerAccountID(k ledger.AccountKey) string {
	return "lac_" + shortHash(k.String())
}

// shortHash is a stable 32-hex-character digest used to derive surrogate identifiers from
// natural keys. It is not a security primitive and nothing authenticates with it.
func shortHash(s string) string {
	return hexDigest(s)[:32]
}
