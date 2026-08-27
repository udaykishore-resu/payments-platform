package ledger

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// TestBuildersAlwaysBalance is the property test the ledger's correctness rests on: over a large
// generated table of transactions, every builder helper produces a group that satisfies
// Balance() — for every currency, in every combination of amounts, at every dispute stage, with
// and without fees.
//
// The generator is seeded deterministically, so a failure is reproducible from the case name
// alone; a randomly-seeded property test that fails once and never again is worse than no test.
// The currencies are chosen to cover the exponents that catch people out: USD (2), JPY (0) and
// KWD (3). Nothing in the balance invariant depends on the exponent, and that is exactly what
// is being asserted — an implementation that reached for a decimal representation somewhere
// would fail here.
func TestBuildersAlwaysBalance(t *testing.T) {
	// Verifies: BR-29, FR-81.
	t.Parallel()

	clock := testClock()
	currencies := []money.Currency{"USD", "JPY", "KWD", "EUR"}
	rng := rand.New(rand.NewSource(20260303))

	type generated struct {
		name string
		tx   *Transaction
	}
	var cases []generated

	for i := 0; i < 60; i++ {
		cur := currencies[i%len(currencies)]
		gross := int64(rng.Intn(5_000_000) + 1)
		fee := int64(rng.Intn(int(gross)) + 1)
		net := gross - fee
		paymentID := shared.NewPaymentID()

		capture, err := CaptureTransaction(CaptureParams{
			TenantID: testTenant, MerchantID: testMerchant, PaymentID: paymentID,
			Gross: money.MustNew(gross, cur), GatewayRef: fmt.Sprintf("ch_%d", i),
		}, clock)
		if err != nil {
			t.Fatalf("case %d: CaptureTransaction: %v", i, err)
		}
		cases = append(cases, generated{fmt.Sprintf("capture/%s/%d", cur, gross), capture})

		feeTx, err := FeeTransaction(FeeParams{
			TenantID: testTenant, MerchantID: testMerchant, PaymentID: paymentID,
			Fee: money.MustNew(fee, cur),
		}, clock)
		if err != nil {
			t.Fatalf("case %d: FeeTransaction: %v", i, err)
		}
		cases = append(cases, generated{fmt.Sprintf("fee/%s/%d", cur, fee), feeTx})

		for _, awaiting := range []bool{false, true} {
			refund, err := RefundTransaction(RefundParams{
				TenantID: testTenant, MerchantID: testMerchant, PaymentID: paymentID,
				RefundID: shared.NewRefundID(), Amount: money.MustNew(gross, cur),
				AwaitingGatewayConfirmation: awaiting,
			}, clock)
			if err != nil {
				t.Fatalf("case %d: RefundTransaction: %v", i, err)
			}
			cases = append(cases, generated{fmt.Sprintf("refund/%s/%d/awaiting=%v", cur, gross, awaiting), refund})
		}

		for _, stage := range []DisputeStage{DisputeOpened, DisputeWon, DisputeLost} {
			disputeFee := money.Zero(cur)
			if stage == DisputeOpened {
				disputeFee = money.MustNew(int64(rng.Intn(2000)+1), cur)
			}
			cb, err := ChargebackTransaction(ChargebackParams{
				TenantID: testTenant, MerchantID: testMerchant, PaymentID: paymentID,
				Stage: stage, Amount: money.MustNew(gross, cur), Fee: disputeFee,
				DisputeRef: fmt.Sprintf("dp_%d", i),
			}, clock)
			if err != nil {
				t.Fatalf("case %d: ChargebackTransaction(%s): %v", i, stage, err)
			}
			cases = append(cases, generated{fmt.Sprintf("chargeback/%s/%s/%d", cur, stage, gross), cb})
		}

		settlement, err := SettlementTransaction(SettlementParams{
			TenantID: testTenant, MerchantID: testMerchant, PaymentID: paymentID,
			Gross: money.MustNew(gross, cur), Fees: money.MustNew(fee, cur), Net: money.MustNew(net, cur),
			SettlementRef: fmt.Sprintf("po_%d", i),
		}, clock)
		if err != nil {
			t.Fatalf("case %d: SettlementTransaction: %v", i, err)
		}
		cases = append(cases, generated{fmt.Sprintf("settlement/%s/%d", cur, gross), settlement})
	}

	if len(cases) < 400 {
		t.Fatalf("generated only %d cases; the property test is not exercising the space", len(cases))
	}

	for _, tc := range cases {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.tx.Balance(); err != nil {
				t.Fatalf("builder produced an unbalanced transaction: %v", err)
			}
			if im := tc.tx.Imbalances(); len(im) != 0 {
				t.Fatalf("Imbalances() = %+v, want none", im)
			}
			entries := tc.tx.Entries()
			if len(entries) < 2 {
				t.Fatalf("transaction has %d entries; every movement has a counterparty", len(entries))
			}
			for _, e := range entries {
				if !e.Amount().IsPositive() {
					t.Errorf("entry %s has non-positive amount %s", e.ID(), e.Amount())
				}
				if !e.Side().IsValid() || !e.Account().IsValid() || !e.Type().IsValid() {
					t.Errorf("entry %s is malformed: side=%q account=%q type=%q",
						e.ID(), e.Side(), e.Account(), e.Type())
				}
				if e.TransactionID() != tc.tx.ID() {
					t.Errorf("entry %s belongs to group %s, not %s", e.ID(), e.TransactionID(), tc.tx.ID())
				}
			}
			// Debits and credits must match per currency, computed independently of Balance so
			// that a bug in Balance cannot hide a bug in a builder.
			for _, cur := range tc.tx.Currencies() {
				var dr, cr int64
				for _, e := range entries {
					if e.Currency() != cur {
						continue
					}
					if e.Side() == SideDebit {
						dr += e.Amount().Amount()
					} else {
						cr += e.Amount().Amount()
					}
				}
				if dr != cr {
					t.Errorf("%s: debits %d != credits %d", cur, dr, cr)
				}
			}
		})
	}
}

// TestUnbalancedTransactionsAreRejected is the other half of the property: a hand-constructed
// group that does not balance must never be accepted.
func TestUnbalancedTransactionsAreRejected(t *testing.T) {
	// Verifies: FR-81.
	t.Parallel()

	clock := testClock()
	usd := money.Currency("USD")
	eur := money.Currency("EUR")

	build := func(t *testing.T, lines ...Line) *Transaction {
		t.Helper()
		tx, err := NewTransaction(NewTransactionParams{
			TenantID: testTenant, MerchantID: testMerchant, EntryType: EntryAdjustment,
		}, clock)
		if err != nil {
			t.Fatalf("NewTransaction: %v", err)
		}
		for _, l := range lines {
			if err := tx.AddEntry(l); err != nil {
				t.Fatalf("AddEntry: %v", err)
			}
		}
		return tx
	}

	tests := []struct {
		name  string
		lines []Line
		// wantCurrencies is the set of currencies the error must name, so that the on-call
		// engineer reading a LEDGER_IMBALANCE page learns by how much and in what.
		wantCurrencies []money.Currency
	}{
		{
			name:  "empty group",
			lines: nil,
		},
		{
			name: "single entry",
			lines: []Line{
				{Account: AccountGatewayClearing, Side: SideDebit, Amount: money.MustNew(100, usd)},
			},
			wantCurrencies: []money.Currency{usd},
		},
		{
			name: "amounts differ",
			lines: []Line{
				{Account: AccountGatewayClearing, Side: SideDebit, Amount: money.MustNew(100, usd)},
				{Account: AccountMerchantReceivable, Side: SideCredit, Amount: money.MustNew(99, usd)},
			},
			wantCurrencies: []money.Currency{usd},
		},
		{
			name: "both lines on the same side",
			lines: []Line{
				{Account: AccountGatewayClearing, Side: SideDebit, Amount: money.MustNew(100, usd)},
				{Account: AccountDisputesHeld, Side: SideDebit, Amount: money.MustNew(100, usd)},
			},
			wantCurrencies: []money.Currency{usd},
		},
		{
			// The case a naive cross-currency sum would accept: 100 USD debited and 100 EUR
			// credited nets to "zero" only if you add cents to cents-of-a-different-currency.
			name: "balances only if currencies are mixed",
			lines: []Line{
				{Account: AccountGatewayClearing, Side: SideDebit, Amount: money.MustNew(100, usd)},
				{Account: AccountMerchantReceivable, Side: SideCredit, Amount: money.MustNew(100, eur)},
			},
			wantCurrencies: []money.Currency{eur, usd},
		},
		{
			name: "one currency balances, the other does not",
			lines: []Line{
				{Account: AccountGatewayClearing, Side: SideDebit, Amount: money.MustNew(100, usd)},
				{Account: AccountMerchantReceivable, Side: SideCredit, Amount: money.MustNew(100, usd)},
				{Account: AccountGatewayClearing, Side: SideDebit, Amount: money.MustNew(250, eur)},
				{Account: AccountMerchantReceivable, Side: SideCredit, Amount: money.MustNew(240, eur)},
			},
			wantCurrencies: []money.Currency{eur},
		},
	}

	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := build(t, tc.lines...)
			err := tx.Balance()
			if err == nil {
				t.Fatal("Balance() accepted an unbalanced transaction")
			}
			got := tx.Imbalances()
			if len(got) != len(tc.wantCurrencies) {
				t.Fatalf("Imbalances() named %d currencies %+v, want %v", len(got), got, tc.wantCurrencies)
			}
			for i, want := range tc.wantCurrencies {
				if got[i].Currency != want {
					t.Errorf("imbalance %d is in %s, want %s", i, got[i].Currency, want)
				}
				if got[i].Delta.IsZero() {
					t.Errorf("imbalance %d reports a zero delta", i)
				}
			}
		})
	}
}

// TestBalancedGroupWithMultipleCurrencies confirms the invariant is per currency, not global:
// an FX settlement that balances within each currency is legal.
func TestBalancedGroupWithMultipleCurrencies(t *testing.T) {
	t.Parallel()

	clock := testClock()
	tx, err := NewTransaction(NewTransactionParams{
		TenantID: testTenant, MerchantID: testMerchant, EntryType: EntryAdjustment,
	}, clock)
	if err != nil {
		t.Fatalf("NewTransaction: %v", err)
	}
	lines := []Line{
		{Account: AccountGatewayClearing, Side: SideDebit, Amount: money.MustNew(100, "USD")},
		{Account: AccountMerchantReceivable, Side: SideCredit, Amount: money.MustNew(100, "USD")},
		{Account: AccountGatewayClearing, Side: SideDebit, Amount: money.MustNew(9000, "JPY")},
		{Account: AccountMerchantReceivable, Side: SideCredit, Amount: money.MustNew(9000, "JPY")},
	}
	for _, l := range lines {
		if err := tx.AddEntry(l); err != nil {
			t.Fatalf("AddEntry: %v", err)
		}
	}
	if err := tx.Balance(); err != nil {
		t.Fatalf("Balance() rejected a per-currency-balanced group: %v", err)
	}
	if got, want := len(tx.Currencies()), 2; got != want {
		t.Fatalf("Currencies() = %d, want %d", got, want)
	}
}

func TestSettlementMustReconcile(t *testing.T) {
	// Verifies: BR-30.
	t.Parallel()

	clock := testClock()
	usd := money.Currency("USD")

	tests := []struct {
		name    string
		gross   int64
		fees    int64
		net     int64
		wantErr bool
	}{
		{"gross equals net plus fees", 8450, 275, 8175, false},
		{"no fee retained", 8450, 0, 8450, false},
		{"net overstated", 8450, 275, 8200, true},
		{"fees overstated", 8450, 300, 8175, true},
		{"gross understated", 8000, 275, 8175, true},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx, err := SettlementTransaction(SettlementParams{
				TenantID: testTenant, MerchantID: testMerchant, PaymentID: shared.NewPaymentID(),
				Gross: money.MustNew(tc.gross, usd),
				Fees:  money.MustNew(tc.fees, usd),
				Net:   money.MustNew(tc.net, usd),
			}, clock)
			if tc.wantErr {
				if err == nil {
					t.Fatal("SettlementTransaction accepted a row whose amounts do not reconcile")
				}
				return
			}
			if err != nil {
				t.Fatalf("SettlementTransaction: %v", err)
			}
			if err := tx.Balance(); err != nil {
				t.Fatalf("settlement does not balance: %v", err)
			}
		})
	}
}

func TestBuilderShapes(t *testing.T) {
	t.Parallel()

	clock := testClock()
	usd := money.Currency("USD")
	paymentID := shared.NewPaymentID()

	type posting struct {
		account AccountType
		side    Side
		amount  int64
	}
	postings := func(tx *Transaction) []posting {
		out := make([]posting, 0, len(tx.Entries()))
		for _, e := range tx.Entries() {
			out = append(out, posting{e.Account(), e.Side(), e.Amount().Amount()})
		}
		return out
	}

	capture, err := CaptureTransaction(CaptureParams{
		TenantID: testTenant, MerchantID: testMerchant, PaymentID: paymentID,
		Gross: money.MustNew(8450, usd),
	}, clock)
	if err != nil {
		t.Fatalf("CaptureTransaction: %v", err)
	}
	wantCapture := []posting{
		{AccountGatewayClearing, SideDebit, 8450},
		{AccountMerchantReceivable, SideCredit, 8450},
	}
	if got := postings(capture); !equalPostings(got, wantCapture) {
		t.Errorf("capture postings = %+v, want %+v", got, wantCapture)
	}

	opened, err := ChargebackTransaction(ChargebackParams{
		TenantID: testTenant, MerchantID: testMerchant, PaymentID: paymentID,
		Stage: DisputeOpened, Amount: money.MustNew(8450, usd), Fee: money.MustNew(1500, usd),
	}, clock)
	if err != nil {
		t.Fatalf("ChargebackTransaction: %v", err)
	}
	wantOpened := []posting{
		{AccountDisputesHeld, SideDebit, 8450},
		{AccountGatewayClearing, SideCredit, 8450},
		{AccountMerchantReceivable, SideDebit, 1500},
		{AccountFeesPayable, SideCredit, 1500},
	}
	if got := postings(opened); !equalPostings(got, wantOpened) {
		t.Errorf("dispute-opened postings = %+v, want %+v", got, wantOpened)
	}
	if opened.Type() != EntryChargeback {
		t.Errorf("dispute-opened group type = %s, want %s", opened.Type(), EntryChargeback)
	}

	won, err := ChargebackTransaction(ChargebackParams{
		TenantID: testTenant, MerchantID: testMerchant, PaymentID: paymentID,
		Stage: DisputeWon, Amount: money.MustNew(8450, usd),
	}, clock)
	if err != nil {
		t.Fatalf("ChargebackTransaction(WON): %v", err)
	}
	if won.Type() != EntryChargebackReversal {
		t.Errorf("dispute-won group type = %s, want %s", won.Type(), EntryChargebackReversal)
	}
	// The fee is not reversed when a dispute is won: the merchant's true cost stays visible.
	for _, e := range won.Entries() {
		if e.Account() == AccountFeesPayable {
			t.Error("a won dispute reversed the dispute fee; most gateways do not, and the ledger must reflect reality")
		}
	}

	pending, err := RefundTransaction(RefundParams{
		TenantID: testTenant, MerchantID: testMerchant, PaymentID: paymentID,
		Amount: money.MustNew(8450, usd), AwaitingGatewayConfirmation: true,
	}, clock)
	if err != nil {
		t.Fatalf("RefundTransaction: %v", err)
	}
	wantPending := []posting{
		{AccountMerchantReceivable, SideDebit, 8450},
		{AccountRefundsPayable, SideCredit, 8450},
	}
	if got := postings(pending); !equalPostings(got, wantPending) {
		t.Errorf("unconfirmed refund postings = %+v, want %+v", got, wantPending)
	}
}

func TestBuildersRejectInvalidInput(t *testing.T) {
	t.Parallel()

	clock := testClock()
	usd := money.Currency("USD")

	tests := []struct {
		name string
		call func() error
	}{
		{"capture with no tenant", func() error {
			_, err := CaptureTransaction(CaptureParams{
				MerchantID: testMerchant, Gross: money.MustNew(1, usd),
			}, clock)
			return err
		}},
		{"capture with zero amount", func() error {
			_, err := CaptureTransaction(CaptureParams{
				TenantID: testTenant, MerchantID: testMerchant, Gross: money.Zero(usd),
			}, clock)
			return err
		}},
		{"fee with negative amount", func() error {
			_, err := FeeTransaction(FeeParams{
				TenantID: testTenant, MerchantID: testMerchant, Fee: money.MustNew(-5, usd),
			}, clock)
			return err
		}},
		{"refund with no merchant", func() error {
			_, err := RefundTransaction(RefundParams{
				TenantID: testTenant, Amount: money.MustNew(1, usd),
			}, clock)
			return err
		}},
		{"chargeback with unknown stage", func() error {
			_, err := ChargebackTransaction(ChargebackParams{
				TenantID: testTenant, MerchantID: testMerchant,
				Stage: DisputeStage("REOPENED"), Amount: money.MustNew(1, usd),
			}, clock)
			return err
		}},
		{"transaction with unknown entry type", func() error {
			_, err := NewTransaction(NewTransactionParams{
				TenantID: testTenant, MerchantID: testMerchant, EntryType: "AUTHORIZATION_HOLD",
			}, clock)
			return err
		}},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.call(); err == nil {
				t.Fatal("builder accepted invalid input")
			}
		})
	}
}

func equalPostings[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
