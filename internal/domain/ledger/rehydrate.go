package ledger

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// RehydrateEntryParams carries a persisted ledger line back into the domain.
//
// NewEntry cannot serve this purpose and must not be made to: it mints a fresh identifier and
// stamps recordedAt from the clock, so reading a ledger through it would return entries whose
// identities and record times differ from the ones on disk. For a table whose entire value is
// that it is an unalterable record of what happened, that is not a rounding error — a
// reconciliation report built from re-minted entries would not match the entries it claims to
// summarise, and nobody would be able to say which copy was wrong.
type RehydrateEntryParams struct {
	ID            shared.LedgerID
	TenantID      shared.TenantID
	MerchantID    shared.MerchantID
	TransactionID TransactionID
	Account       AccountType
	Side          Side
	Amount        money.Money
	EntryType     EntryType
	PaymentID     shared.PaymentID
	AttemptID     shared.AttemptID
	RefundID      shared.RefundID
	GatewayRef    string
	Description   string
	OccurredAt    time.Time
	RecordedAt    time.Time
	// PartitionMonth is stored rather than recomputed. Recomputing it would be a pure function
	// of the payment ID and would normally agree — but "normally" is doing too much work for a
	// value that decides which partition a row was actually written to, and an entry whose
	// in-memory partition disagrees with its on-disk one is an entry the archival job will lose.
	PartitionMonth time.Time
}

// RehydrateEntry reconstructs an Entry from persisted state, validating that the row is one this
// binary understands.
//
// It refuses an unknown account type, side or entry type rather than coercing. A ledger line
// silently coerced to a plausible account is a line that balances arithmetically and is charged
// to the wrong party, which is the one kind of ledger error that reconciliation cannot detect.
func RehydrateEntry(p RehydrateEntryParams) (Entry, error) {
	if !p.Account.IsValid() {
		return Entry{}, apierror.Newf(apierror.CodeInternalError,
			"ledger entry %s references unknown account type %q; this row may have been written "+
				"by a newer version of the service", p.ID, p.Account)
	}
	if !p.Side.IsValid() {
		return Entry{}, apierror.Newf(apierror.CodeInternalError,
			"ledger entry %s has unknown side %q", p.ID, p.Side)
	}
	if !p.EntryType.IsValid() {
		return Entry{}, apierror.Newf(apierror.CodeInternalError,
			"ledger entry %s has unknown entry type %q", p.ID, p.EntryType)
	}
	if !p.Amount.IsPositive() {
		return Entry{}, apierror.Newf(apierror.CodeInternalError,
			"ledger entry %s has a non-positive amount; direction is carried by the side", p.ID)
	}
	partition := p.PartitionMonth
	if partition.IsZero() {
		partition = partitionMonthFor(p.PaymentID, p.ID, p.OccurredAt)
	}
	return Entry{
		id:             p.ID,
		tenantID:       p.TenantID,
		merchantID:     p.MerchantID,
		transactionID:  p.TransactionID,
		account:        p.Account,
		side:           p.Side,
		amount:         p.Amount,
		entryType:      p.EntryType,
		paymentID:      p.PaymentID,
		attemptID:      p.AttemptID,
		refundID:       p.RefundID,
		gatewayRef:     p.GatewayRef,
		description:    p.Description,
		occurredAt:     p.OccurredAt.UTC(),
		recordedAt:     p.RecordedAt.UTC(),
		partitionMonth: partition,
	}, nil
}
