// Package ledger implements the platform's double-entry shadow ledger.
//
// # This is a shadow ledger, not a custody ledger
//
// The platform does not hold funds (baseline §1.3 A1: we are a technical orchestrator, not a
// payment institution, and no money ever sits in an account we control). Nothing in this
// package is a legal claim on money, a customer balance, or a statement of what any party is
// entitled to demand. It is the platform's *own view* of money movement, recorded so that
// reconciliation against the gateway's settlement report and the merchant's bank credit has
// something to reconcile against, and so that reporting can answer "what did this merchant
// earn, and what did it cost" without replaying the payment event log.
//
// The distinction is not pedantry. A custody ledger is the authoritative record of who owns
// what and must be defensible to a regulator as such; a shadow ledger is an observation that
// may legitimately disagree with the gateway for a while (fees are estimated at capture and
// truthed at settlement — docs/payment-flow.md §16.2) and whose disagreements are exceptions to
// investigate rather than incidents of missing money. Building the first when you need the
// second buys an e-money licensing conversation nobody wanted; building the second when you
// need the first is fraud. We need the second.
//
// # Double entry, and why
//
// Every recorded movement produces at least two entries whose debits and credits sum to zero
// within each currency. The invariant is enforced in code (Transaction.Balance), in the
// database (the L7.LEDGER_ENTRY_BALANCED constraint) and in reconciliation
// (docs/payment-flow.md §16.3 `LEDGER_IMBALANCE`). Single-entry "just record the amount"
// bookkeeping cannot detect a dropped or duplicated posting; double entry turns both into an
// arithmetic failure at write time instead of a quarterly surprise.
//
// # Append-only
//
// Entries are immutable and there is no correction path that edits one. See entry.go.
//
// This package imports only the standard library, pkg/* and internal/domain/shared, per the
// dependency rule in docs/spec/00-design-baseline.md §4.
package ledger

import (
	"sort"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Side is which column of a double-entry posting a line lands in.
//
// It is a defined string type rather than a bool because `debit == true` reads correctly to
// exactly nobody at 3am, and because the value is persisted, logged and reported on, where
// "DEBIT" is self-describing and `t` is not.
type Side string

const (
	// SideDebit increases assets and expenses, decreases liabilities, income and equity.
	SideDebit Side = "DEBIT"
	// SideCredit is the mirror of SideDebit.
	SideCredit Side = "CREDIT"
)

// IsValid reports whether s is one of the two sides. A zero Side is not valid, so an
// accidentally-unset field fails validation instead of silently defaulting to debit — which
// would post every line in one direction and still, catastrophically, look like a number.
func (s Side) IsValid() bool { return s == SideDebit || s == SideCredit }

// Opposite returns the other side. Used by the builders to construct the matching line of a
// pair, so that a two-line transaction cannot be written with both lines on the same side.
func (s Side) Opposite() Side {
	if s == SideDebit {
		return SideCredit
	}
	return SideDebit
}

// String satisfies fmt.Stringer.
func (s Side) String() string { return string(s) }

// ParseSide validates a persisted or transported side value.
func ParseSide(s string) (Side, error) {
	v := Side(strings.ToUpper(strings.TrimSpace(s)))
	if !v.IsValid() {
		return "", apierror.Newf(apierror.CodeValidationFailed, "invalid ledger side %q", s).
			WithDetail(apierror.Detail{
				Field: "side", Code: "INVALID_SIDE",
				Message: "must be DEBIT or CREDIT",
				RuleID:  "L7.LEDGER_SIDE_KNOWN",
			})
	}
	return v, nil
}

// AccountType is the chart of accounts.
//
// It is a closed enum rather than free-form account strings for the same reason the state
// machines are tables: the set is small, it is enumerable, and every report, every
// reconciliation rule and every database CHECK constraint is derived from it. A ledger that
// lets a caller invent an account name accumulates typo-accounts ("fee_payable",
// "fees_payble") that each hold real money and that nothing sums.
//
// The names map onto the account labels used in the narratives in docs/payment-flow.md §0 as
// follows — the narrative uses a merchant's-book vocabulary, this enum uses the platform's
// shadow-book vocabulary, and the mapping is stated here so the two cannot silently diverge:
//
//	gateway_receivable      → GATEWAY_CLEARING
//	merchant_revenue        → MERCHANT_RECEIVABLE  (the merchant's earned position)
//	fee_expense             → FEES_PAYABLE         (held as a payable until settlement truths it)
//	refund_expense          → REFUNDS_PAYABLE / GATEWAY_CLEARING, depending on confirmation
//	dispute_holding         → DISPUTES_HELD
//	chargeback_expense      → MERCHANT_RECEIVABLE  (a lost dispute is borne by the merchant)
//	merchant_bank_clearing  → SETTLEMENT_SUSPENSE
type AccountType string

const (
	// AccountMerchantReceivable is the merchant's net earned position: what the platform's view
	// says the merchant is owed out of captured funds, after fees, refunds and lost disputes.
	//
	// Read the name from the *merchant's* point of view — it is the merchant's receivable, and
	// therefore an obligation in the platform's shadow book, which is why its normal side is
	// CREDIT. This is the one name in the enum that reads backwards if you assume the
	// platform's own perspective, and it is spelled out here precisely because a reader who
	// assumes "receivable ⇒ asset ⇒ debit-normal" will invert every merchant balance.
	AccountMerchantReceivable AccountType = "MERCHANT_RECEIVABLE"

	// AccountGatewayClearing holds funds sitting at the gateway attributable to this merchant:
	// captured but not yet paid out. Debit-normal, because it is an asset from the position the
	// ledger observes — the gateway's obligation to pay onward.
	AccountGatewayClearing AccountType = "GATEWAY_CLEARING"

	// AccountFeesPayable holds processing and scheme fees the gateway will retain out of the
	// payout. Credit-normal.
	//
	// Fees are recorded as a payable at capture rather than netted against gateway clearing
	// immediately because the capture-time fee is an *estimate* and the settlement row is truth
	// (docs/payment-flow.md §16.2). Holding the estimate in its own account makes the variance
	// visible as an explicit adjustment at settlement rather than as an unexplained residual in
	// the clearing account.
	AccountFeesPayable AccountType = "FEES_PAYABLE"

	// AccountRefundsPayable holds refunds the platform has accepted but the gateway has not yet
	// confirmed moved — the window that exists for every asynchronous method and that support
	// tickets are almost entirely about. Credit-normal.
	AccountRefundsPayable AccountType = "REFUNDS_PAYABLE"

	// AccountDisputesHeld holds funds the gateway has withheld pending a dispute. Debit-normal:
	// the money still exists and is still attributable to this merchant, it is simply not
	// available. Netting a dispute straight out of gateway clearing would make a won dispute
	// indistinguishable from a capture that never happened.
	AccountDisputesHeld AccountType = "DISPUTES_HELD"

	// AccountSettlementSuspense holds funds in flight between the gateway and the merchant's
	// bank: the payout has been made by the gateway and has not yet been confirmed as credited.
	// Debit-normal. This is the account that makes "the gateway says it paid, the merchant says
	// it did not arrive" a balance you can point at.
	AccountSettlementSuspense AccountType = "SETTLEMENT_SUSPENSE"
)

// normalSides is the authoritative sign convention. It is a table for the same reason the
// transitions are: it is generated into the documentation and the database CHECK constraint,
// and a second copy of this knowledge in an `if` somewhere is a copy that will drift.
var normalSides = map[AccountType]Side{
	AccountMerchantReceivable: SideCredit,
	AccountGatewayClearing:    SideDebit,
	AccountFeesPayable:        SideCredit,
	AccountRefundsPayable:     SideCredit,
	AccountDisputesHeld:       SideDebit,
	AccountSettlementSuspense: SideDebit,
}

// IsValid reports whether t is a known account type.
func (t AccountType) IsValid() bool { _, ok := normalSides[t]; return ok }

// String satisfies fmt.Stringer.
func (t AccountType) String() string { return string(t) }

// NormalSide returns the side on which a posting *increases* this account's balance.
//
// The sign convention, spelled out, because getting it backwards is the classic ledger bug and
// it is silent — every number still adds up, every transaction still balances, and the reports
// are simply wrong in a way that survives code review:
//
//	An account's balance is stored as a non-negative-by-convention magnitude in the account's
//	own normal direction. A posting on the normal side ADDS; a posting on the opposite side
//	SUBTRACTS. A negative balance is therefore meaningful — it says the account is carrying a
//	position against its own nature — and is a signal, not an error.
//
// Worked example, USD 84.50 captured with a USD 2.75 fee (the running example in
// docs/payment-flow.md §0):
//
//	CAPTURE      DR GATEWAY_CLEARING      8450   → debit is GATEWAY_CLEARING's normal side, so
//	                                                its balance goes 0 → +8450
//	             CR MERCHANT_RECEIVABLE   8450   → credit is MERCHANT_RECEIVABLE's normal side,
//	                                                so its balance goes 0 → +8450
//	FEE          DR MERCHANT_RECEIVABLE    275   → debit is the OPPOSITE of its normal side,
//	                                                so its balance goes 8450 → 8175
//	             CR FEES_PAYABLE           275   → 0 → +275
//
//	Result: the gateway holds 8450, the merchant has earned 8175, and 275 is owed to the
//	gateway. 8175 + 275 = 8450. If NormalSide were inverted for MERCHANT_RECEIVABLE the fee
//	would have *increased* the merchant's earnings to 8725 and every line would still balance.
func (t AccountType) NormalSide() Side { return normalSides[t] }

// ParseAccountType validates a persisted or transported account type.
func ParseAccountType(s string) (AccountType, error) {
	v := AccountType(strings.ToUpper(strings.TrimSpace(s)))
	if !v.IsValid() {
		return "", apierror.Newf(apierror.CodeValidationFailed, "unknown ledger account type %q", s).
			WithDetail(apierror.Detail{
				Field: "account", Code: "UNKNOWN_ACCOUNT_TYPE",
				Message: "must be one of the platform's chart-of-accounts entries",
				RuleID:  "L7.LEDGER_ACCOUNT_KNOWN",
			})
	}
	return v, nil
}

// AllAccountTypes returns the chart of accounts, sorted. Used by reporting, by the migration
// generator for the CHECK constraint, and by the exhaustive tests.
func AllAccountTypes() []AccountType {
	out := make([]AccountType, 0, len(normalSides))
	for t := range normalSides {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AccountKey identifies exactly one account. There is one account per
// (tenant, merchant, type, currency), and all four components are load-bearing:
//
//   - tenant, because tenants are isolated at every layer and a balance that spans two tenants
//     is a data-isolation defect that reporting would present as a number;
//   - merchant, because the platform's ledger exists to answer per-merchant questions and a
//     pooled per-tenant balance cannot be decomposed after the fact;
//   - type, because that is what the chart of accounts is;
//   - currency, because an account holding two currencies cannot have a meaningful balance.
//
// That last one is worth stating in full, since "just store the currency on the entry" looks
// like it works. If a single GATEWAY_CLEARING account holds both USD and JPY entries, its
// balance is the sum of a number of cents and a number of yen — an integer with no unit, which
// is not a balance but a coincidence of arithmetic. There is no exchange rate in the domain
// (pkg/money deliberately has none: FX is a business decision with a rate source, a spread and
// an audit trail), so the sum cannot even be converted into something meaningful. Splitting by
// currency makes every account's balance a money.Money, and makes cross-currency arithmetic a
// compile-and-run-time error rather than a silent one.
type AccountKey struct {
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	Type       AccountType
	Currency   money.Currency
}

// Validate checks that the key names a real account.
func (k AccountKey) Validate() error {
	if k.TenantID.IsZero() {
		return apierror.New(apierror.CodeMissingTenantContext, "a ledger account requires a tenant")
	}
	if k.MerchantID.IsZero() {
		return apierror.New(apierror.CodeValidationFailed, "a ledger account requires a merchant").
			WithDetail(apierror.Detail{
				Field: "merchantId", Code: "MISSING_MERCHANT",
				Message: "ledger accounts are per merchant; a tenant-level balance cannot be decomposed later",
				RuleID:  "L7.LEDGER_ACCOUNT_SCOPED",
			})
	}
	if !k.Type.IsValid() {
		return apierror.Newf(apierror.CodeValidationFailed, "unknown ledger account type %q", k.Type).
			WithDetail(apierror.Detail{
				Field: "account", Code: "UNKNOWN_ACCOUNT_TYPE",
				Message: "must be one of the platform's chart-of-accounts entries",
				RuleID:  "L7.LEDGER_ACCOUNT_KNOWN",
			})
	}
	if !k.Currency.IsSupported() {
		return apierror.Newf(apierror.CodeCurrencyNotSupported,
			"ledger account currency %q is not supported", k.Currency).
			WithDetail(apierror.Detail{
				Field: "currency", Code: "UNSUPPORTED_CURRENCY",
				Message: "an account holds exactly one currency and it must be a supported one",
				RuleID:  "L7.LEDGER_ACCOUNT_CURRENCY",
			})
	}
	return nil
}

// String renders the key in the stable form used in logs, reconciliation reports and the
// database's natural key: TYPE:currency:tenant:merchant.
func (k AccountKey) String() string {
	return string(k.Type) + ":" + string(k.Currency) + ":" + k.TenantID.String() + ":" + k.MerchantID.String()
}

// Account is the running balance of one (tenant, merchant, type, currency).
//
// It is a value type with unexported fields and no mutating methods: Apply returns a new
// Account rather than modifying the receiver. That is deliberate. A balance is a fold over an
// immutable entry sequence, and making the fold pure means the in-memory projection can never
// drift from a recomputation over the entries — the recomputation *is* the same function.
// A mutable balance with an `AddEntry` that forgets to bump the version is the bug that makes a
// projection unreproducible, and unreproducible is the one thing a ledger may not be.
type Account struct {
	key        AccountKey
	normalSide Side
	balance    money.Money
	version    shared.Version

	// entryCount is carried alongside the balance because reconciliation compares both: two
	// projections with the same balance and different entry counts have found each other's bug.
	entryCount int64

	createdAt time.Time
	updatedAt time.Time
}

// NewAccount opens an account at a zero balance.
//
// Accounts are opened lazily on first posting rather than provisioned up front: the chart has
// six types and the supported-currency list has dozens of entries, so eager provisioning would
// create hundreds of permanently-zero rows per merchant, all of which reporting would have to
// filter out and none of which would ever be interesting.
func NewAccount(key AccountKey, clock shared.Clock) (Account, error) {
	if err := key.Validate(); err != nil {
		return Account{}, err
	}
	now := clock.Now()
	return Account{
		key:        key,
		normalSide: key.Type.NormalSide(),
		balance:    money.Zero(key.Currency),
		version:    1,
		createdAt:  now,
		updatedAt:  now,
	}, nil
}

// Key returns the account's identity.
func (a Account) Key() AccountKey { return a.key }

// TenantID returns the owning tenant.
func (a Account) TenantID() shared.TenantID { return a.key.TenantID }

// MerchantID returns the owning merchant.
func (a Account) MerchantID() shared.MerchantID { return a.key.MerchantID }

// Type returns the chart-of-accounts entry.
func (a Account) Type() AccountType { return a.key.Type }

// Currency returns the account's single currency.
func (a Account) Currency() money.Currency { return a.key.Currency }

// NormalSide returns the side on which a posting increases this balance.
func (a Account) NormalSide() Side { return a.normalSide }

// Balance returns the balance in the account's normal direction. A negative value means the
// account is carrying a position against its own nature — for GATEWAY_CLEARING that means the
// platform believes the merchant owes the gateway money, which is a real and reportable
// condition (a merchant refunding more than they captured in a period), not an error.
func (a Account) Balance() money.Money { return a.balance }

// Version is the optimistic-concurrency version. The projection is written with
// `WHERE version = $old`, so two consumers folding the same entry twice conflict instead of
// double-counting.
func (a Account) Version() shared.Version { return a.version }

// EntryCount returns how many entries have been folded into this balance.
func (a Account) EntryCount() int64 { return a.entryCount }

// CreatedAt returns when the account was opened.
func (a Account) CreatedAt() time.Time { return a.createdAt }

// UpdatedAt returns when the last entry was folded in.
func (a Account) UpdatedAt() time.Time { return a.updatedAt }

// Apply folds one entry into the balance and returns the resulting account. The receiver is
// unchanged.
//
// It refuses an entry that does not belong to this account rather than coercing it. A posting
// that lands in the wrong account is the single most damaging thing that can happen to a
// ledger, because it is invisible: the transaction still balances, the totals still tie out,
// and one merchant's money is recorded against another's.
func (a Account) Apply(e Entry) (Account, error) {
	if e.AccountKey() != a.key {
		return Account{}, apierror.Newf(apierror.CodeInternalError,
			"ledger entry %s belongs to account %s, not %s", e.ID(), e.AccountKey(), a.key)
	}
	delta := e.Amount()
	if e.Side() != a.normalSide {
		delta = delta.Neg()
	}
	next, err := a.balance.Add(delta)
	if err != nil {
		return Account{}, apierror.Wrapf(err, apierror.CodeInternalError,
			"ledger account %s balance overflow", a.key)
	}
	a.balance = next
	a.version = a.version.Next()
	a.entryCount++
	a.updatedAt = e.RecordedAt()
	return a, nil
}

// RehydrateAccountParams carries a persisted balance back into an Account.
//
// It exists for the same reason payment.Rehydrate does: the fields are unexported so that no
// repository can assemble an Account that the constructor and Apply would have refused, and
// this is the single reviewed doorway back in.
type RehydrateAccountParams struct {
	Key        AccountKey
	Balance    money.Money
	Version    shared.Version
	EntryCount int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// RehydrateAccount reconstructs an Account from persisted state, validating that the row is one
// this binary understands and that its balance is denominated in its own currency.
func RehydrateAccount(p RehydrateAccountParams) (Account, error) {
	if err := p.Key.Validate(); err != nil {
		return Account{}, err
	}
	if p.Balance.Currency() != p.Key.Currency {
		return Account{}, apierror.Newf(apierror.CodeInternalError,
			"ledger account %s has a balance denominated in %s; this row is corrupt",
			p.Key, p.Balance.Currency())
	}
	return Account{
		key:        p.Key,
		normalSide: p.Key.Type.NormalSide(),
		balance:    p.Balance,
		version:    p.Version,
		entryCount: p.EntryCount,
		createdAt:  p.CreatedAt,
		updatedAt:  p.UpdatedAt,
	}, nil
}
