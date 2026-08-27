package ledger

import (
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// testClock is fixed so that every timestamp in these tests is reproducible. No test in this
// package sleeps or reads the wall clock.
func testClock() shared.Clock {
	return shared.FixedClock{T: time.Date(2026, 3, 3, 9, 14, 22, 0, time.UTC)}
}

const (
	testTenant   = shared.TenantID("ten_01JB8Z9K2QW3E4R5T6Y7U8I9O0")
	testMerchant = shared.MerchantID("mrc_01JB8Z9K2QW3E4R5T6Y7U8I9O0")
)

func TestNormalSideTableIsComplete(t *testing.T) {
	t.Parallel()

	// Every account type must declare a normal side. A type with the zero Side would make every
	// posting to it "not on the normal side" and silently invert its balance.
	for _, at := range AllAccountTypes() {

		t.Run(string(at), func(t *testing.T) {
			t.Parallel()
			if !at.NormalSide().IsValid() {
				t.Fatalf("account type %s has no declared normal side", at)
			}
			if !at.IsValid() {
				t.Fatalf("account type %s is not reported valid", at)
			}
		})
	}
	if got, want := len(AllAccountTypes()), 6; got != want {
		t.Fatalf("chart of accounts has %d entries, want %d", got, want)
	}
}

func TestNormalSideAssignments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		account AccountType
		want    Side
	}{
		{AccountGatewayClearing, SideDebit},
		{AccountDisputesHeld, SideDebit},
		{AccountSettlementSuspense, SideDebit},
		{AccountMerchantReceivable, SideCredit},
		{AccountFeesPayable, SideCredit},
		{AccountRefundsPayable, SideCredit},
	}
	for _, tc := range tests {

		t.Run(string(tc.account), func(t *testing.T) {
			t.Parallel()
			if got := tc.account.NormalSide(); got != tc.want {
				t.Fatalf("%s normal side = %s, want %s", tc.account, got, tc.want)
			}
		})
	}
}

// TestSignConventionWorkedExample is the example from AccountType.NormalSide, executed. If the
// sign convention is ever inverted for one account, this is the test that notices: every
// transaction still balances, so only the resulting balances can catch it.
func TestSignConventionWorkedExample(t *testing.T) {
	t.Parallel()

	clock := testClock()
	usd := money.Currency("USD")
	paymentID := shared.NewPaymentID()

	capture, err := CaptureTransaction(CaptureParams{
		TenantID: testTenant, MerchantID: testMerchant, PaymentID: paymentID,
		Gross: money.MustNew(8450, usd), GatewayRef: "ch_1",
	}, clock)
	if err != nil {
		t.Fatalf("CaptureTransaction: %v", err)
	}
	fee, err := FeeTransaction(FeeParams{
		TenantID: testTenant, MerchantID: testMerchant, PaymentID: paymentID,
		Fee: money.MustNew(275, usd), GatewayRef: "ch_1",
	}, clock)
	if err != nil {
		t.Fatalf("FeeTransaction: %v", err)
	}

	balances := foldAll(t, clock, capture, fee)

	want := map[AccountType]int64{
		AccountGatewayClearing:    8450, // the gateway holds the gross
		AccountMerchantReceivable: 8175, // the merchant has earned gross less the fee
		AccountFeesPayable:        275,  // owed to the gateway
	}
	for account, amount := range want {
		got := balances[AccountKey{TenantID: testTenant, MerchantID: testMerchant, Type: account, Currency: usd}]
		if got.Amount() != amount {
			t.Errorf("%s balance = %d, want %d", account, got.Amount(), amount)
		}
	}
	// The identity that makes the convention right: what the gateway holds is exactly what the
	// merchant earned plus what is owed in fees.
	if want[AccountMerchantReceivable]+want[AccountFeesPayable] != want[AccountGatewayClearing] {
		t.Fatal("the worked example itself does not tie out")
	}
}

// TestSettlementDrainsClearingAndFees checks the end state of the whole happy path: after a
// capture, its fee and the settlement that pays it out, the gateway holds nothing and no fee is
// outstanding. A non-zero residual in either is the reconciliation exception the design exists
// to surface.
func TestSettlementDrainsClearingAndFees(t *testing.T) {
	t.Parallel()

	clock := testClock()
	usd := money.Currency("USD")
	paymentID := shared.NewPaymentID()

	capture, err := CaptureTransaction(CaptureParams{
		TenantID: testTenant, MerchantID: testMerchant, PaymentID: paymentID,
		Gross: money.MustNew(8450, usd),
	}, clock)
	if err != nil {
		t.Fatalf("CaptureTransaction: %v", err)
	}
	fee, err := FeeTransaction(FeeParams{
		TenantID: testTenant, MerchantID: testMerchant, PaymentID: paymentID,
		Fee: money.MustNew(275, usd),
	}, clock)
	if err != nil {
		t.Fatalf("FeeTransaction: %v", err)
	}
	settlement, err := SettlementTransaction(SettlementParams{
		TenantID: testTenant, MerchantID: testMerchant, PaymentID: paymentID,
		Gross: money.MustNew(8450, usd), Fees: money.MustNew(275, usd), Net: money.MustNew(8175, usd),
		SettlementRef: "po_1",
	}, clock)
	if err != nil {
		t.Fatalf("SettlementTransaction: %v", err)
	}

	balances := foldAll(t, clock, capture, fee, settlement)
	key := func(a AccountType) AccountKey {
		return AccountKey{TenantID: testTenant, MerchantID: testMerchant, Type: a, Currency: usd}
	}
	if got := balances[key(AccountGatewayClearing)]; !got.IsZero() {
		t.Errorf("gateway clearing = %s, want zero after settlement", got)
	}
	if got := balances[key(AccountFeesPayable)]; !got.IsZero() {
		t.Errorf("fees payable = %s, want zero after settlement", got)
	}
	if got := balances[key(AccountSettlementSuspense)]; got.Amount() != 8175 {
		t.Errorf("settlement suspense = %s, want 8175 in flight to the bank", got)
	}
	if got := balances[key(AccountMerchantReceivable)]; got.Amount() != 8175 {
		t.Errorf("merchant receivable = %s, want 8175 still owed until the bank credit lands", got)
	}
}

func TestAccountRejectsEntryForAnotherAccount(t *testing.T) {
	t.Parallel()

	clock := testClock()
	usd := money.Currency("USD")
	acct, err := NewAccount(AccountKey{
		TenantID: testTenant, MerchantID: testMerchant,
		Type: AccountGatewayClearing, Currency: usd,
	}, clock)
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}

	tests := []struct {
		name  string
		entry NewEntryParams
	}{
		{
			name: "different account type",
			entry: NewEntryParams{
				TenantID: testTenant, MerchantID: testMerchant, TransactionID: NewTransactionID(),
				Account: AccountFeesPayable, Side: SideDebit, Amount: money.MustNew(100, usd),
				EntryType: EntryFee,
			},
		},
		{
			name: "different currency",
			entry: NewEntryParams{
				TenantID: testTenant, MerchantID: testMerchant, TransactionID: NewTransactionID(),
				Account: AccountGatewayClearing, Side: SideDebit, Amount: money.MustNew(100, "EUR"),
				EntryType: EntryCapture,
			},
		},
		{
			name: "different merchant",
			entry: NewEntryParams{
				TenantID: testTenant, MerchantID: shared.MerchantID("mrc_01JB8Z9K2QW3E4R5T6Y7U8I9O1"),
				TransactionID: NewTransactionID(),
				Account:       AccountGatewayClearing, Side: SideDebit, Amount: money.MustNew(100, usd),
				EntryType: EntryCapture,
			},
		},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, err := NewEntry(tc.entry, clock)
			if err != nil {
				t.Fatalf("NewEntry: %v", err)
			}
			if _, err := acct.Apply(e); err == nil {
				t.Fatal("Apply accepted an entry belonging to a different account")
			}
		})
	}
}

func TestAccountApplyIsPure(t *testing.T) {
	t.Parallel()

	clock := testClock()
	usd := money.Currency("USD")
	key := AccountKey{TenantID: testTenant, MerchantID: testMerchant, Type: AccountGatewayClearing, Currency: usd}
	acct, err := NewAccount(key, clock)
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	e, err := NewEntry(NewEntryParams{
		TenantID: testTenant, MerchantID: testMerchant, TransactionID: NewTransactionID(),
		Account: AccountGatewayClearing, Side: SideDebit, Amount: money.MustNew(500, usd),
		EntryType: EntryCapture,
	}, clock)
	if err != nil {
		t.Fatalf("NewEntry: %v", err)
	}

	next, err := acct.Apply(e)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !acct.Balance().IsZero() {
		t.Errorf("Apply mutated the receiver: balance = %s, want zero", acct.Balance())
	}
	if next.Balance().Amount() != 500 {
		t.Errorf("applied balance = %s, want 500", next.Balance())
	}
	if next.Version() != acct.Version().Next() {
		t.Errorf("version = %d, want %d", next.Version(), acct.Version().Next())
	}
	if next.EntryCount() != 1 {
		t.Errorf("entry count = %d, want 1", next.EntryCount())
	}
}

func TestAccountKeyValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		key     AccountKey
		wantErr bool
	}{
		{"valid", AccountKey{testTenant, testMerchant, AccountFeesPayable, "USD"}, false},
		{"no tenant", AccountKey{"", testMerchant, AccountFeesPayable, "USD"}, true},
		{"no merchant", AccountKey{testTenant, "", AccountFeesPayable, "USD"}, true},
		{"unknown account", AccountKey{testTenant, testMerchant, AccountType("REVENUE"), "USD"}, true},
		{"unsupported currency", AccountKey{testTenant, testMerchant, AccountFeesPayable, "XBT"}, true},
		{"empty currency", AccountKey{testTenant, testMerchant, AccountFeesPayable, ""}, true},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.key.Validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Validate() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestRehydrateAccountRejectsMismatchedCurrency(t *testing.T) {
	t.Parallel()

	key := AccountKey{testTenant, testMerchant, AccountGatewayClearing, "USD"}
	if _, err := RehydrateAccount(RehydrateAccountParams{
		Key: key, Balance: money.MustNew(100, "EUR"), Version: 3,
	}); err == nil {
		t.Fatal("RehydrateAccount accepted a balance in the wrong currency")
	}
	if _, err := RehydrateAccount(RehydrateAccountParams{
		Key: key, Balance: money.MustNew(100, "USD"), Version: 3,
	}); err != nil {
		t.Fatalf("RehydrateAccount rejected a well-formed row: %v", err)
	}
}

// foldAll projects a set of transactions into per-account balances, opening accounts lazily,
// exactly as the ledger projection does.
func foldAll(t *testing.T, clock shared.Clock, txs ...*Transaction) map[AccountKey]money.Money {
	t.Helper()
	accounts := make(map[AccountKey]Account)
	for _, tx := range txs {
		if err := tx.Balance(); err != nil {
			t.Fatalf("transaction %s does not balance: %v", tx.ID(), err)
		}
		for _, e := range tx.Entries() {
			key := e.AccountKey()
			acct, ok := accounts[key]
			if !ok {
				var err error
				acct, err = NewAccount(key, clock)
				if err != nil {
					t.Fatalf("NewAccount(%s): %v", key, err)
				}
			}
			next, err := acct.Apply(e)
			if err != nil {
				t.Fatalf("Apply(%s): %v", e.ID(), err)
			}
			accounts[key] = next
		}
	}
	out := make(map[AccountKey]money.Money, len(accounts))
	for k, a := range accounts {
		out[k] = a.Balance()
	}
	return out
}
