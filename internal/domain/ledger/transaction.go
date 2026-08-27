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

// TransactionID groups the lines of one movement.
//
// It reuses the registered `led` prefix rather than inventing one: pkg/ids keeps a closed
// prefix registry precisely so that nothing in the codebase mints an unregistered prefix, and
// adding one is a shared-kernel change. The group and its lines are never confusable in
// practice because they live in different columns of different tables, and the doc comment here
// is the place that says so.
type TransactionID string

// String satisfies fmt.Stringer.
func (t TransactionID) String() string { return string(t) }

// IsZero reports whether the identifier is unset.
func (t TransactionID) IsZero() bool { return t == "" }

// NewTransactionID mints a group identifier.
func NewTransactionID() TransactionID { return TransactionID(ids.New(ids.PrefixLedgerEntry)) }

// Transaction is a balanced group of entries: one movement of money, expressed as the lines
// that describe it.
//
// The group, not the line, is the unit of correctness. A single entry is half of a fact — money
// left somewhere and arrived somewhere else, and a ledger that can record one half without the
// other cannot tell a fee from a rounding error. Everything downstream (the append transaction,
// the reconciliation residual, the merchant statement) operates on groups.
//
// Construction is deliberately two-phase — NewTransaction, then AddEntry, then Balance — rather
// than a single constructor taking all the lines. The builders below are the normal path and
// hand back an already-balanced group; the open form exists for the postings the catalog does
// not anticipate (an operator-signed reconciliation adjustment, a mechanical ACH return), and
// for those the caller must call Balance and handle its error before persisting. The
// application layer's append path calls Balance again regardless, so a caller who skips it
// cannot commit an imbalance — the check is a gate, not a courtesy.
type Transaction struct {
	id         TransactionID
	tenantID   shared.TenantID
	merchantID shared.MerchantID
	entryType  EntryType

	description string
	paymentID   shared.PaymentID

	occurredAt time.Time
	// clock is retained so that every line in the group shares one recordedAt and one
	// occurredAt. Holding a Clock — not a context, not a connection — in a builder is the
	// intended use of the interface: it keeps AddEntry from needing a time argument that would
	// let two lines of the same movement disagree about when it happened.
	clock shared.Clock

	entries []Entry
}

// NewTransactionParams are the inputs to opening a group.
type NewTransactionParams struct {
	TenantID    shared.TenantID
	MerchantID  shared.MerchantID
	EntryType   EntryType
	PaymentID   shared.PaymentID
	Description string
	// OccurredAt is the source-of-truth timestamp for the movement. Zero means "now", which is
	// correct only for postings the platform originates itself.
	OccurredAt time.Time
}

// NewTransaction opens an empty group. The group is not valid until it balances.
func NewTransaction(p NewTransactionParams, clock shared.Clock) (*Transaction, error) {
	if p.TenantID.IsZero() {
		return nil, apierror.New(apierror.CodeMissingTenantContext, "a ledger transaction requires a tenant")
	}
	if p.MerchantID.IsZero() {
		return nil, apierror.New(apierror.CodeValidationFailed, "a ledger transaction requires a merchant").
			WithDetail(apierror.Detail{
				Field: "merchantId", Code: "MISSING_MERCHANT",
				Message: "ledger postings are per merchant",
				RuleID:  "L7.LEDGER_ACCOUNT_SCOPED",
			})
	}
	if !p.EntryType.IsValid() {
		return nil, apierror.Newf(apierror.CodeValidationFailed,
			"unknown ledger entry type %q", p.EntryType).
			WithDetail(apierror.Detail{
				Field: "entryType", Code: "UNKNOWN_ENTRY_TYPE",
				Message: "an authorization is a hold, not a movement, and has no entry type",
				RuleID:  "L7.LEDGER_ENTRY_TYPE_KNOWN",
			})
	}
	occurred := p.OccurredAt
	if occurred.IsZero() {
		occurred = clock.Now()
	}
	return &Transaction{
		id:          NewTransactionID(),
		tenantID:    p.TenantID,
		merchantID:  p.MerchantID,
		entryType:   p.EntryType,
		description: p.Description,
		paymentID:   p.PaymentID,
		occurredAt:  occurred.UTC(),
		clock:       clock,
	}, nil
}

// Line is one posting to add to a group. The tenant, merchant, transaction and occurrence time
// come from the group, so a line cannot disagree with the movement it is part of.
type Line struct {
	Account     AccountType
	Side        Side
	Amount      money.Money
	EntryType   EntryType
	PaymentID   shared.PaymentID
	AttemptID   shared.AttemptID
	RefundID    shared.RefundID
	GatewayRef  string
	Description string
}

// AddEntry appends one validated line.
//
// A line's EntryType defaults to the group's. Mixed types within one group are legal and
// intentional: a chargeback group carries both the CHARGEBACK lines that move the disputed
// funds and the FEE lines for the dispute fee, because they are one event from the gateway and
// splitting them into two groups would make "what did this dispute cost" a join.
func (t *Transaction) AddEntry(l Line) error {
	et := l.EntryType
	if et == "" {
		et = t.entryType
	}
	paymentID := l.PaymentID
	if paymentID.IsZero() {
		paymentID = t.paymentID
	}
	desc := l.Description
	if desc == "" {
		desc = t.description
	}
	e, err := NewEntry(NewEntryParams{
		TenantID:      t.tenantID,
		MerchantID:    t.merchantID,
		TransactionID: t.id,
		Account:       l.Account,
		Side:          l.Side,
		Amount:        l.Amount,
		EntryType:     et,
		PaymentID:     paymentID,
		AttemptID:     l.AttemptID,
		RefundID:      l.RefundID,
		GatewayRef:    l.GatewayRef,
		Description:   desc,
		OccurredAt:    t.occurredAt,
	}, t.clock)
	if err != nil {
		return err
	}
	t.entries = append(t.entries, e)
	return nil
}

// ID returns the group identifier.
func (t *Transaction) ID() TransactionID { return t.id }

// TenantID returns the owning tenant.
func (t *Transaction) TenantID() shared.TenantID { return t.tenantID }

// MerchantID returns the owning merchant.
func (t *Transaction) MerchantID() shared.MerchantID { return t.merchantID }

// Type returns the group's business event.
func (t *Transaction) Type() EntryType { return t.entryType }

// PaymentID returns the payment this group relates to, if any.
func (t *Transaction) PaymentID() shared.PaymentID { return t.paymentID }

// OccurredAt returns the source-of-truth timestamp shared by every line.
func (t *Transaction) OccurredAt() time.Time { return t.occurredAt }

// Description returns the group's human-readable note.
func (t *Transaction) Description() string { return t.description }

// Entries returns the lines in the order they were added. The slice is a copy; Entry is an
// immutable value, so the copy is a genuine defence and not a ritual — a caller cannot reach
// through it to change a posting.
func (t *Transaction) Entries() []Entry { return append([]Entry(nil), t.entries...) }

// Currencies returns the currencies present in the group, sorted. A group spanning currencies
// is legal — an FX settlement legitimately touches two — and must balance within each.
func (t *Transaction) Currencies() []money.Currency {
	seen := make(map[money.Currency]struct{}, 2)
	for _, e := range t.entries {
		seen[e.Currency()] = struct{}{}
	}
	out := make([]money.Currency, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Imbalance reports, for one currency, how far a group is from balancing.
type Imbalance struct {
	Currency money.Currency
	Debits   money.Money
	Credits  money.Money
	// Delta is Debits - Credits. Positive means the group is over-debited.
	Delta money.Money
}

// Imbalances returns the currencies in which the group does not balance, sorted. An empty
// result means the fundamental invariant holds. It is exported separately from Balance because
// the reconciliation exception queue reports the residual per account and wants the numbers,
// not the error string.
func (t *Transaction) Imbalances() []Imbalance {
	type sums struct{ dr, cr money.Money }
	acc := make(map[money.Currency]*sums, 2)
	for _, e := range t.entries {
		c := e.Currency()
		s, ok := acc[c]
		if !ok {
			s = &sums{dr: money.Zero(c), cr: money.Zero(c)}
			acc[c] = s
		}
		// Overflow here is unreachable in practice (int64 minor units against realistic volumes)
		// and, if it did happen, must not be silently absorbed: an overflowing sum is reported
		// as an imbalance rather than wrapping into a number that happens to match.
		if e.Side() == SideDebit {
			if v, err := s.dr.Add(e.Amount()); err == nil {
				s.dr = v
			}
		} else {
			if v, err := s.cr.Add(e.Amount()); err == nil {
				s.cr = v
			}
		}
	}
	out := make([]Imbalance, 0, len(acc))
	for c, s := range acc {
		delta, err := s.dr.Sub(s.cr)
		if err != nil {
			delta = money.Zero(c)
		}
		if delta.IsZero() {
			continue
		}
		out = append(out, Imbalance{Currency: c, Debits: s.dr, Credits: s.cr, Delta: delta})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// Balance enforces the fundamental invariant: for each currency in the transaction,
// sum(debits) == sum(credits).
//
// This is the property that makes double-entry worth its cost. It is checked per currency, not
// across the group, because summing USD cents and JPY yen produces a number with no unit; a
// group that "balances" only after mixing currencies balances by coincidence.
//
// A transaction that does not balance cannot be committed. The error lists the imbalance per
// currency, because the first question on a LEDGER_IMBALANCE page (docs/payment-flow.md §16.3,
// CRITICAL, no auto-resolution) is "by how much, and in what" — and an error that says only
// "unbalanced" sends an on-call engineer to a database rather than to the defect.
//
// An empty group is rejected too. Zero debits do equal zero credits, so a naive implementation
// would accept it and write a transaction describing no movement, which reconciliation would
// later find as a group with no lines and no way to tell whether the lines were lost or never
// existed.
func (t *Transaction) Balance() error {
	if len(t.entries) == 0 {
		return apierror.New(apierror.CodeValidationFailed,
			"a ledger transaction must contain at least two entries").
			WithDetail(apierror.Detail{
				Field: "entries", Code: "EMPTY_TRANSACTION",
				Message: "an empty group balances trivially and records nothing; it is never a valid posting",
				RuleID:  "L7.LEDGER_ENTRY_BALANCED",
			})
	}
	if len(t.entries) == 1 {
		return apierror.New(apierror.CodeValidationFailed,
			"a ledger transaction must contain at least two entries").
			WithDetail(apierror.Detail{
				Field: "entries", Code: "SINGLE_ENTRY_TRANSACTION",
				Message: "one entry is half of a movement; every posting has a counterparty",
				RuleID:  "L7.LEDGER_ENTRY_BALANCED",
			})
	}
	imbalances := t.Imbalances()
	if len(imbalances) == 0 {
		return nil
	}
	parts := make([]string, 0, len(imbalances))
	details := make([]apierror.Detail, 0, len(imbalances))
	for _, im := range imbalances {
		parts = append(parts, string(im.Currency)+" debits "+im.Debits.String()+
			" vs credits "+im.Credits.String()+" (delta "+im.Delta.String()+")")
		details = append(details, apierror.Detail{
			Field:   "entries." + string(im.Currency),
			Code:    "LEDGER_IMBALANCE",
			Message: "debits " + im.Debits.String() + ", credits " + im.Credits.String() + ", delta " + im.Delta.String(),
			RuleID:  "L7.LEDGER_ENTRY_BALANCED",
		})
	}
	return apierror.Newf(apierror.CodeValidationFailed,
		"ledger transaction %s does not balance: %s", t.id, strings.Join(parts, "; ")).
		WithDetails(details...)
}

// --- builders for the flows in docs/payment-flow.md -------------------------------------------
//
// Each builder returns an already-balanced Transaction or an error. They exist so that the five
// postings the platform makes thousands of times a day are written once, reviewed once and
// tested once, rather than reconstructed from the account table at each call site — which is
// how a fee ends up credited to the wrong account in exactly one of the four places that post
// fees.
//
// Note what is absent: there is no AuthorizationTransaction. See the note in entry.go.

// CaptureParams are the inputs to a capture posting.
type CaptureParams struct {
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	PaymentID  shared.PaymentID
	AttemptID  shared.AttemptID
	// Gross is the amount captured from the payer, before fees.
	Gross      money.Money
	GatewayRef string
	OccurredAt time.Time
}

// CaptureTransaction records money captured from the payer.
//
//	DR GATEWAY_CLEARING      gross   the gateway now holds this on the merchant's behalf
//	CR MERCHANT_RECEIVABLE   gross   the merchant has earned it
//
// This is the first ledger entry in any card flow. Fees are a separate posting
// (FeeTransaction), not netted here, because the capture-time fee is an estimate and the
// settlement row is truth; keeping them separate makes the variance an explicit adjustment
// rather than a mystery in the clearing balance.
func CaptureTransaction(p CaptureParams, clock shared.Clock) (*Transaction, error) {
	t, err := NewTransaction(NewTransactionParams{
		TenantID: p.TenantID, MerchantID: p.MerchantID, EntryType: EntryCapture,
		PaymentID: p.PaymentID, OccurredAt: p.OccurredAt,
		Description: "capture",
	}, clock)
	if err != nil {
		return nil, err
	}
	line := Line{
		Amount: p.Gross, EntryType: EntryCapture,
		PaymentID: p.PaymentID, AttemptID: p.AttemptID, GatewayRef: p.GatewayRef,
	}
	if err := t.addPair(line, AccountGatewayClearing, AccountMerchantReceivable); err != nil {
		return nil, err
	}
	return t.sealed()
}

// FeeParams are the inputs to a fee posting.
type FeeParams struct {
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	PaymentID  shared.PaymentID
	AttemptID  shared.AttemptID
	// Fee is the processing, scheme or dispute fee.
	Fee        money.Money
	GatewayRef string
	// Description distinguishes a processing fee from a dispute fee in the merchant's
	// statement; both post the same account pair.
	Description string
	OccurredAt  time.Time
}

// FeeTransaction records a fee borne by the merchant.
//
//	DR MERCHANT_RECEIVABLE   fee   the merchant's earned position falls
//	CR FEES_PAYABLE          fee   the obligation to the gateway is recognised
//
// The payable is discharged at settlement, when the gateway retains the fee out of the payout
// (see SettlementTransaction). Netting the fee against GATEWAY_CLEARING at capture time —
// which is how the narrative in docs/payment-flow.md §1 spells it in the merchant's own
// vocabulary — would be equivalent arithmetic but would fold an estimate into the account we
// reconcile against the gateway, and the fee variance would then surface as an unexplained
// clearing residual instead of as a fee adjustment.
func FeeTransaction(p FeeParams, clock shared.Clock) (*Transaction, error) {
	desc := p.Description
	if desc == "" {
		desc = "processing fee"
	}
	t, err := NewTransaction(NewTransactionParams{
		TenantID: p.TenantID, MerchantID: p.MerchantID, EntryType: EntryFee,
		PaymentID: p.PaymentID, OccurredAt: p.OccurredAt, Description: desc,
	}, clock)
	if err != nil {
		return nil, err
	}
	line := Line{
		Amount: p.Fee, EntryType: EntryFee,
		PaymentID: p.PaymentID, AttemptID: p.AttemptID, GatewayRef: p.GatewayRef,
		Description: desc,
	}
	if err := t.addPair(line, AccountMerchantReceivable, AccountFeesPayable); err != nil {
		return nil, err
	}
	return t.sealed()
}

// RefundParams are the inputs to a refund posting.
type RefundParams struct {
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	PaymentID  shared.PaymentID
	RefundID   shared.RefundID
	Amount     money.Money
	GatewayRef string
	OccurredAt time.Time
	// AwaitingGatewayConfirmation is set when the platform has accepted the refund but the
	// gateway has not yet confirmed the money moved — the normal case for bank debits, where
	// the confirmation arrives days later. It changes which account is credited, and therefore
	// whether the merchant's clearing balance already reflects the outflow.
	AwaitingGatewayConfirmation bool
}

// RefundTransaction records money returned to the payer.
//
// Confirmed by the gateway:
//
//	DR MERCHANT_RECEIVABLE   amount   the merchant's earned position falls
//	CR GATEWAY_CLEARING      amount   the funds have left the gateway balance
//
// Accepted but not yet confirmed (asynchronous methods):
//
//	DR MERCHANT_RECEIVABLE   amount
//	CR REFUNDS_PAYABLE       amount   owed to the payer, still sitting at the gateway
//
// and the later confirmation posts DR REFUNDS_PAYABLE / CR GATEWAY_CLEARING as an ADJUSTMENT
// group. Collapsing the two stages into one posting would claim the money had left the gateway
// before it had, which is precisely the fiction that makes a shadow ledger stop being useful
// for reconciliation.
//
// Fees are ordinarily *not* returned with a refund — that is the gateway's policy, not ours,
// and this ledger reflects reality: the merchant is out the refunded gross plus the original
// fee. Where a gateway does return the fee it arrives as a settlement adjustment.
func RefundTransaction(p RefundParams, clock shared.Clock) (*Transaction, error) {
	t, err := NewTransaction(NewTransactionParams{
		TenantID: p.TenantID, MerchantID: p.MerchantID, EntryType: EntryRefund,
		PaymentID: p.PaymentID, OccurredAt: p.OccurredAt, Description: "refund",
	}, clock)
	if err != nil {
		return nil, err
	}
	credit := AccountGatewayClearing
	if p.AwaitingGatewayConfirmation {
		credit = AccountRefundsPayable
	}
	line := Line{
		Amount: p.Amount, EntryType: EntryRefund,
		PaymentID: p.PaymentID, RefundID: p.RefundID, GatewayRef: p.GatewayRef,
	}
	if err := t.addPair(line, AccountMerchantReceivable, credit); err != nil {
		return nil, err
	}
	return t.sealed()
}

// DisputeStage selects which of the three dispute postings to build. A dispute is not one event
// but three — opened, then won or lost — and each moves money differently.
type DisputeStage string

const (
	// DisputeOpened withholds the disputed funds and charges the dispute fee.
	DisputeOpened DisputeStage = "OPENED"
	// DisputeWon releases the withheld funds. The fee is not reversed at most gateways, and the
	// ledger reflects that rather than flattering it: the true cost of a won dispute is visible.
	DisputeWon DisputeStage = "WON"
	// DisputeLost converts the withheld funds into a loss borne by the merchant.
	DisputeLost DisputeStage = "LOST"
)

// IsValid reports whether s is a known stage.
func (s DisputeStage) IsValid() bool {
	return s == DisputeOpened || s == DisputeWon || s == DisputeLost
}

// ChargebackParams are the inputs to a dispute posting.
type ChargebackParams struct {
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	PaymentID  shared.PaymentID
	Stage      DisputeStage
	// Amount is the disputed amount.
	Amount money.Money
	// Fee is the gateway's dispute fee. Zero means no fee, which is legitimate at DisputeWon
	// and DisputeLost — the fee is charged once, when the dispute is opened.
	Fee        money.Money
	DisputeRef string
	OccurredAt time.Time
}

// ChargebackTransaction records one stage of a dispute.
//
//	OPENED  DR DISPUTES_HELD         amount   funds withheld by the gateway, still the merchant's
//	        CR GATEWAY_CLEARING      amount
//	        DR MERCHANT_RECEIVABLE   fee      the dispute fee, borne by the merchant
//	        CR FEES_PAYABLE          fee
//
//	WON     DR GATEWAY_CLEARING      amount   funds released back into the clearing balance
//	        CR DISPUTES_HELD         amount
//
//	LOST    DR MERCHANT_RECEIVABLE   amount   the loss lands on the merchant's earned position
//	        CR DISPUTES_HELD         amount
//
// Routing the disputed funds through DISPUTES_HELD rather than reversing the capture is what
// makes the three stages legible: at any instant, DISPUTES_HELD is exactly the money a merchant
// has at risk, and a won dispute leaves a trail showing the money was withheld and returned
// rather than showing nothing at all.
func ChargebackTransaction(p ChargebackParams, clock shared.Clock) (*Transaction, error) {
	if !p.Stage.IsValid() {
		return nil, apierror.Newf(apierror.CodeValidationFailed,
			"unknown dispute stage %q", p.Stage).
			WithDetail(apierror.Detail{
				Field: "stage", Code: "UNKNOWN_DISPUTE_STAGE",
				Message: "must be OPENED, WON or LOST",
				RuleID:  "L7.LEDGER_DISPUTE_STAGE_KNOWN",
			})
	}
	entryType := EntryChargeback
	if p.Stage == DisputeWon {
		entryType = EntryChargebackReversal
	}
	t, err := NewTransaction(NewTransactionParams{
		TenantID: p.TenantID, MerchantID: p.MerchantID, EntryType: entryType,
		PaymentID: p.PaymentID, OccurredAt: p.OccurredAt,
		Description: "dispute " + strings.ToLower(string(p.Stage)),
	}, clock)
	if err != nil {
		return nil, err
	}
	line := Line{
		Amount: p.Amount, EntryType: entryType,
		PaymentID: p.PaymentID, GatewayRef: p.DisputeRef,
	}
	var debit, credit AccountType
	switch p.Stage {
	case DisputeOpened:
		debit, credit = AccountDisputesHeld, AccountGatewayClearing
	case DisputeWon:
		debit, credit = AccountGatewayClearing, AccountDisputesHeld
	case DisputeLost:
		debit, credit = AccountMerchantReceivable, AccountDisputesHeld
	}
	if err := t.addPair(line, debit, credit); err != nil {
		return nil, err
	}
	if p.Fee.IsPositive() {
		feeLine := Line{
			Amount: p.Fee, EntryType: EntryFee,
			PaymentID: p.PaymentID, GatewayRef: p.DisputeRef, Description: "dispute fee",
		}
		if err := t.addPair(feeLine, AccountMerchantReceivable, AccountFeesPayable); err != nil {
			return nil, err
		}
	}
	return t.sealed()
}

// SettlementParams are the inputs to a settlement posting. The three amounts come straight off
// the gateway's settlement row (docs/payment-flow.md §16.2: txn ref, gross, fee, net).
type SettlementParams struct {
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	PaymentID  shared.PaymentID
	// Gross is the amount leaving the gateway's clearing balance.
	Gross money.Money
	// Fees is the portion the gateway retained. Zero is legal: some gateways invoice fees
	// separately rather than netting them out of the payout.
	Fees money.Money
	// Net is what is actually paid toward the merchant's bank. Gross must equal Net + Fees.
	Net           money.Money
	SettlementRef string
	OccurredAt    time.Time
}

// SettlementTransaction records a payout from the gateway toward the merchant's bank.
//
//	DR SETTLEMENT_SUSPENSE   net     in flight to the merchant's bank
//	DR FEES_PAYABLE          fees    the fee obligation is discharged: the gateway kept it
//	CR GATEWAY_CLEARING      gross   the gateway no longer holds it
//
// The identity gross = net + fees is checked rather than assumed, and a violation is rejected
// rather than absorbed into a balancing plug. A settlement row whose three numbers do not agree
// is a gateway contract violation or a parsing defect, and either way the correct response is
// an exception in the reconciliation queue — never a ledger posting that silently makes the
// arithmetic work.
//
// SETTLEMENT_SUSPENSE is discharged separately, when the merchant's bank credit is confirmed
// (DR MERCHANT_RECEIVABLE / CR SETTLEMENT_SUSPENSE). Posting straight to the merchant's
// position here would assert the money arrived at a bank we have no visibility into, and would
// leave "the gateway says it paid, the merchant says it did not arrive" with no account to
// point at.
func SettlementTransaction(p SettlementParams, clock shared.Clock) (*Transaction, error) {
	if !p.Gross.IsValid() || !p.Net.IsValid() {
		return nil, apierror.New(apierror.CodeAmountInvalid,
			"settlement requires valid gross and net amounts")
	}
	fees := p.Fees
	if fees.Currency() == "" {
		fees = money.Zero(p.Gross.Currency())
	}
	if fees.IsNegative() {
		return nil, apierror.New(apierror.CodeAmountInvalid, "settlement fees may not be negative")
	}
	expected, err := p.Net.Add(fees)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeGatewayContractViolation,
			"settlement amounts are not in a single currency")
	}
	if !expected.Equal(p.Gross) {
		return nil, apierror.Newf(apierror.CodeGatewayContractViolation,
			"settlement does not reconcile: gross %s but net %s + fees %s = %s",
			p.Gross, p.Net, fees, expected).
			WithDetail(apierror.Detail{
				Field: "gross", Code: "SETTLEMENT_DOES_NOT_RECONCILE",
				Message: "gross must equal net plus fees; a mismatch is a reconciliation exception, not a posting",
				RuleID:  "L7.SETTLEMENT_GROSS_EQUALS_NET_PLUS_FEES",
			})
	}
	t, err := NewTransaction(NewTransactionParams{
		TenantID: p.TenantID, MerchantID: p.MerchantID, EntryType: EntrySettlement,
		PaymentID: p.PaymentID, OccurredAt: p.OccurredAt, Description: "settlement",
	}, clock)
	if err != nil {
		return nil, err
	}
	if p.Net.IsPositive() {
		if err := t.AddEntry(Line{
			Account: AccountSettlementSuspense, Side: SideDebit, Amount: p.Net,
			EntryType: EntrySettlement, PaymentID: p.PaymentID, GatewayRef: p.SettlementRef,
		}); err != nil {
			return nil, err
		}
	}
	if fees.IsPositive() {
		if err := t.AddEntry(Line{
			Account: AccountFeesPayable, Side: SideDebit, Amount: fees,
			EntryType: EntryFee, PaymentID: p.PaymentID, GatewayRef: p.SettlementRef,
			Description: "fee retained at settlement",
		}); err != nil {
			return nil, err
		}
	}
	if err := t.AddEntry(Line{
		Account: AccountGatewayClearing, Side: SideCredit, Amount: p.Gross,
		EntryType: EntrySettlement, PaymentID: p.PaymentID, GatewayRef: p.SettlementRef,
	}); err != nil {
		return nil, err
	}
	return t.sealed()
}

// addPair appends the two halves of a movement from a single Line, so that a builder cannot
// write a debit without its credit or transpose the amounts between them.
func (t *Transaction) addPair(l Line, debit, credit AccountType) error {
	dr, cr := l, l
	dr.Account, dr.Side = debit, SideDebit
	cr.Account, cr.Side = credit, SideCredit
	if err := t.AddEntry(dr); err != nil {
		return err
	}
	return t.AddEntry(cr)
}

// sealed runs the invariant before a builder hands the group back, so that a builder's output
// is balanced by construction and a caller never has to remember to check. The open
// NewTransaction path deliberately does not do this: there, the caller is composing something
// the catalog does not cover and owns the check.
func (t *Transaction) sealed() (*Transaction, error) {
	if err := t.Balance(); err != nil {
		return nil, err
	}
	return t, nil
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-27, BR-29, BR-30, FR-80, FR-81.
//
// The double-entry shadow ledger: every posting balances, and a settlement that does not
// reconcile becomes an exception rather than an entry
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
