package ledger

import (
	"sort"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// EntryType names the business event that produced a line.
//
// It is carried on the entry rather than inferred from the account pair because reporting,
// reconciliation and the settlement tie-out all group by it, and because the same account pair
// legitimately appears for more than one reason: DR MERCHANT_RECEIVABLE / CR FEES_PAYABLE is a
// processing fee at capture and a dispute fee at chargeback, and a merchant asking "why did my
// fees double this month" needs those separated.
type EntryType string

const (
	// EntryCapture is money captured from the payer. The first ledger entry in any card flow.
	EntryCapture EntryType = "CAPTURE"
	// EntryRefund is money returned to the payer.
	EntryRefund EntryType = "REFUND"
	// EntryFee is a processing, scheme or dispute fee.
	EntryFee EntryType = "FEE"
	// EntryChargeback is funds withheld or lost to a dispute.
	EntryChargeback EntryType = "CHARGEBACK"
	// EntryChargebackReversal is funds released after a dispute is won. It is a distinct type
	// rather than a negative CHARGEBACK because "how many disputes did we win" must be
	// answerable without inspecting signs.
	EntryChargebackReversal EntryType = "CHARGEBACK_REVERSAL"
	// EntrySettlement is a payout from the gateway toward the merchant's bank.
	EntrySettlement EntryType = "SETTLEMENT"
	// EntryAdjustment is a correction: a fee variance truthed by the settlement report, a
	// mechanical ACH return, an operator-signed reconciliation posting. Every correction in this
	// ledger is an ADJUSTMENT entry in a new transaction; see the immutability note on Entry.
	EntryAdjustment EntryType = "ADJUSTMENT"
)

// There is deliberately no AUTHORIZATION_HOLD entry type, and no way to spell one.
//
// An authorization is a hold, not a movement. The issuer has reserved the payer's funds; no
// money has left anyone's account, the gateway owes the merchant nothing, and the merchant has
// earned nothing. Recording it as a movement would credit MERCHANT_RECEIVABLE and debit
// GATEWAY_CLEARING for the entire authorization-to-capture window — seven days on a typical
// card authorization, longer on some schemes — and would therefore overstate the merchant's
// receivable by the value of every uncaptured authorization at every instant, including every
// authorization that is ultimately voided or left to expire and whose money never moves at all.
// docs/payment-flow.md §0 names this as the single most common modelling error in payment
// ledgers, and §7 makes the point structurally: a voided authorization produces no ledger entry
// to reverse, which is only true because none was written.
//
// The hold is still recorded — as a memo on the payment aggregate and on the attempt, where the
// authorized amount, its expiry and its gateway reference live. It is simply not recorded here,
// because here is where money movement lives.
//
// The absence is enforced by the closed enum: `entryTypes` has no such member, ParseEntryType
// rejects the string, and there is no builder that could construct the pair.

var entryTypes = map[EntryType]struct{}{
	EntryCapture: {}, EntryRefund: {}, EntryFee: {}, EntryChargeback: {},
	EntryChargebackReversal: {}, EntrySettlement: {}, EntryAdjustment: {},
}

// IsValid reports whether t is a known entry type.
func (t EntryType) IsValid() bool { _, ok := entryTypes[t]; return ok }

// String satisfies fmt.Stringer.
func (t EntryType) String() string { return string(t) }

// ParseEntryType validates a persisted or transported entry type.
func ParseEntryType(s string) (EntryType, error) {
	v := EntryType(strings.ToUpper(strings.TrimSpace(s)))
	if !v.IsValid() {
		return "", apierror.Newf(apierror.CodeValidationFailed, "unknown ledger entry type %q", s).
			WithDetail(apierror.Detail{
				Field: "entryType", Code: "UNKNOWN_ENTRY_TYPE",
				Message: "must be one of CAPTURE, REFUND, FEE, CHARGEBACK, CHARGEBACK_REVERSAL, SETTLEMENT, ADJUSTMENT",
				RuleID:  "L7.LEDGER_ENTRY_TYPE_KNOWN",
			})
	}
	return v, nil
}

// AllEntryTypes returns the entry types, sorted, for reporting enums and the exhaustive tests.
func AllEntryTypes() []EntryType {
	out := make([]EntryType, 0, len(entryTypes))
	for t := range entryTypes {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Entry is one immutable line of a balanced transaction.
//
// # Immutability is the whole point
//
// Every field is unexported, there is no setter, and no method on Entry returns a modified
// Entry. A correction is a new, balanced, compensating transaction carrying EntryAdjustment,
// linked to the original by transaction and payment ID — never an edit.
//
// This is not fastidiousness about value semantics. An editable ledger is not a ledger: its
// state at any past instant is unknowable, so "what did we believe on the 3rd of March, and
// when did we change our mind" has no answer, which is exactly the question a reconciliation
// dispute, an audit and a regulator all ask. The entry that was wrong is evidence — of what the
// gateway told us and when — and deleting it destroys the only record that a discrepancy ever
// existed. docs/payment-flow.md §16.3 states it as a triage principle: never resolve an
// exception by editing history. The database backs this up (`pp_app` holds no UPDATE or DELETE
// grant on the entries table, L7.LEDGER_IS_APPEND_ONLY); this type makes it true in memory too,
// so no application code can even stage a mutation to attempt.
//
// The cost is real and worth naming: fixing a fat-fingered posting takes two entries instead of
// zero, and a merchant's statement shows the error and its reversal rather than showing
// nothing. That visible scar is the feature.
type Entry struct {
	id            shared.LedgerID
	tenantID      shared.TenantID
	merchantID    shared.MerchantID
	transactionID TransactionID

	account AccountType
	side    Side
	amount  money.Money

	entryType EntryType

	paymentID  shared.PaymentID
	attemptID  shared.AttemptID
	refundID   shared.RefundID
	gatewayRef string

	description string

	// occurredAt is when the money moved according to the source of truth (the gateway's own
	// timestamp, usually). recordedAt is when we wrote it down. Both are retained for the same
	// reason the audit record retains both: their divergence is the measure of how stale our
	// view is, and a settlement posted with an occurredAt three days before its recordedAt
	// belongs in a different accounting period than the one it arrived in.
	occurredAt time.Time
	recordedAt time.Time

	partitionMonth time.Time
}

// NewEntryParams are the inputs to one line. A parameter struct rather than a positional
// constructor: eight of these fields are strings or string-like, and a positional mix-up
// between a payment ID and a gateway reference is a bug the compiler cannot catch.
type NewEntryParams struct {
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
}

// NewEntry constructs and validates one line.
//
// The amount is required to be strictly positive. Direction is carried by Side and by nothing
// else: permitting a negative debit would give every movement two spellings (a negative debit
// and a positive credit), and a ledger with two spellings for the same thing is a ledger whose
// reports depend on which spelling the writer happened to use.
func NewEntry(p NewEntryParams, clock shared.Clock) (Entry, error) {
	key := AccountKey{
		TenantID:   p.TenantID,
		MerchantID: p.MerchantID,
		Type:       p.Account,
		Currency:   p.Amount.Currency(),
	}
	if err := key.Validate(); err != nil {
		return Entry{}, err
	}
	if !p.Side.IsValid() {
		return Entry{}, apierror.Newf(apierror.CodeValidationFailed,
			"ledger entry side %q must be DEBIT or CREDIT", p.Side).
			WithDetail(apierror.Detail{
				Field: "side", Code: "INVALID_SIDE",
				Message: "must be DEBIT or CREDIT",
				RuleID:  "L7.LEDGER_SIDE_KNOWN",
			})
	}
	if !p.EntryType.IsValid() {
		return Entry{}, apierror.Newf(apierror.CodeValidationFailed,
			"unknown ledger entry type %q", p.EntryType).
			WithDetail(apierror.Detail{
				Field: "entryType", Code: "UNKNOWN_ENTRY_TYPE",
				Message: "an authorization is a hold, not a movement, and has no entry type",
				RuleID:  "L7.LEDGER_ENTRY_TYPE_KNOWN",
			})
	}
	if !p.Amount.IsPositive() {
		return Entry{}, apierror.New(apierror.CodeAmountInvalid,
			"a ledger entry amount must be strictly positive; direction is carried by the side").
			WithDetail(apierror.Detail{
				Field: "amount", Code: "NOT_POSITIVE",
				Message: "post the opposite side rather than a negative amount",
				RuleID:  "L7.LEDGER_AMOUNT_POSITIVE",
			})
	}
	if p.TransactionID == "" {
		return Entry{}, apierror.New(apierror.CodeValidationFailed,
			"a ledger entry must belong to a transaction group").
			WithDetail(apierror.Detail{
				Field: "transactionId", Code: "MISSING_TRANSACTION",
				Message: "entries are only meaningful inside a balanced group",
				RuleID:  "L7.LEDGER_ENTRY_BALANCED",
			})
	}

	now := clock.Now()
	occurred := p.OccurredAt
	if occurred.IsZero() {
		occurred = now
	}
	id := shared.NewLedgerID()
	return Entry{
		id:             id,
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
		occurredAt:     occurred.UTC(),
		recordedAt:     now.UTC(),
		partitionMonth: partitionMonthFor(p.PaymentID, id, occurred),
	}, nil
}

// partitionMonthFor derives the declarative range-partition key.
//
// It is the payment's month, not the entry's, whenever a payment ID is present. That is
// baseline amendment A-02 applied here: making the partition a pure function of the payment's
// immutable ID keeps every entry for a payment in the same partition as the payment itself,
// which is what lets the reconciler read a payment and all of its ledger entries with one
// partition scan instead of a scan across every month since the capture. A refund six months
// after a capture, or a dispute a year later, still lands with its payment.
//
// Entries with no payment — a merchant-level settlement or an operator adjustment — fall back
// to the occurrence month, which is the only sensible key available.
func partitionMonthFor(paymentID shared.PaymentID, entryID shared.LedgerID, occurred time.Time) time.Time {
	if !paymentID.IsZero() {
		if m := shared.PartitionMonth(paymentID); !m.IsZero() {
			return m
		}
	}
	if m := ids.PartitionMonth(ids.ID(entryID)); !m.IsZero() && occurred.IsZero() {
		return m
	}
	t := occurred.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// Accessors. There are no setters, by design; see the type comment.

// ID returns the entry's identifier.
func (e Entry) ID() shared.LedgerID { return e.id }

// TenantID returns the owning tenant.
func (e Entry) TenantID() shared.TenantID { return e.tenantID }

// MerchantID returns the owning merchant.
func (e Entry) MerchantID() shared.MerchantID { return e.merchantID }

// TransactionID returns the balanced group this line belongs to. A line outside a group is
// meaningless: it is half of a movement.
func (e Entry) TransactionID() TransactionID { return e.transactionID }

// Account returns the chart-of-accounts entry this line posts to.
func (e Entry) Account() AccountType { return e.account }

// AccountKey returns the full identity of the account this line posts to.
func (e Entry) AccountKey() AccountKey {
	return AccountKey{
		TenantID:   e.tenantID,
		MerchantID: e.merchantID,
		Type:       e.account,
		Currency:   e.amount.Currency(),
	}
}

// Side returns which column this line lands in.
func (e Entry) Side() Side { return e.side }

// Amount returns the strictly positive magnitude.
func (e Entry) Amount() money.Money { return e.amount }

// Currency returns the entry's currency.
func (e Entry) Currency() money.Currency { return e.amount.Currency() }

// Type returns the business event that produced this line.
func (e Entry) Type() EntryType { return e.entryType }

// PaymentID returns the payment this line relates to, if any.
func (e Entry) PaymentID() shared.PaymentID { return e.paymentID }

// AttemptID returns the attempt this line relates to, if any.
func (e Entry) AttemptID() shared.AttemptID { return e.attemptID }

// RefundID returns the refund this line relates to, if any.
func (e Entry) RefundID() shared.RefundID { return e.refundID }

// GatewayRef returns the gateway's own reference for the movement. This is the join key for the
// settlement tie-out, so it is carried on every entry that has one.
func (e Entry) GatewayRef() string { return e.gatewayRef }

// Description returns the human-readable note.
func (e Entry) Description() string { return e.description }

// OccurredAt returns when the money moved, per the source of truth.
func (e Entry) OccurredAt() time.Time { return e.occurredAt }

// RecordedAt returns when the platform wrote the line down.
func (e Entry) RecordedAt() time.Time { return e.recordedAt }

// PartitionMonth returns the declarative range-partition key.
func (e Entry) PartitionMonth() time.Time { return e.partitionMonth }

// IsIncrease reports whether this line increases the target account's balance, i.e. whether it
// posts on the account's normal side. Reporting uses it; nothing in the invariant does, because
// balance is about debits equalling credits and is indifferent to normal sides.
func (e Entry) IsIncrease() bool { return e.side == e.account.NormalSide() }
