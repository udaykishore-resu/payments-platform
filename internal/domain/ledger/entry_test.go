package ledger

import (
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// TestNoAuthorizationHoldEntryType is the structural expression of the rule in
// docs/payment-flow.md §0: an authorization is a hold, not a movement, and there must be no way
// to spell one in the ledger. If someone adds the type, this fails.
func TestNoAuthorizationHoldEntryType(t *testing.T) {
	t.Parallel()

	for _, et := range AllEntryTypes() {
		if strings.Contains(string(et), "AUTH") || strings.Contains(string(et), "HOLD") {
			t.Fatalf("entry type %s exists: an authorization is a hold, not a movement", et)
		}
	}
	for _, s := range []string{"AUTHORIZATION_HOLD", "AUTHORIZATION", "HOLD", "AUTH"} {
		if _, err := ParseEntryType(s); err == nil {
			t.Errorf("ParseEntryType(%q) succeeded; the ledger has no authorization entry type", s)
		}
	}
	if got, want := len(AllEntryTypes()), 7; got != want {
		t.Fatalf("entry type set has %d members, want %d", got, want)
	}
}

func TestNewEntryValidation(t *testing.T) {
	t.Parallel()

	clock := testClock()
	usd := money.Currency("USD")
	valid := NewEntryParams{
		TenantID: testTenant, MerchantID: testMerchant, TransactionID: NewTransactionID(),
		Account: AccountGatewayClearing, Side: SideDebit, Amount: money.MustNew(8450, usd),
		EntryType: EntryCapture,
	}

	mutate := func(f func(*NewEntryParams)) NewEntryParams {
		p := valid
		f(&p)
		return p
	}

	tests := []struct {
		name    string
		params  NewEntryParams
		wantErr bool
	}{
		{"valid", valid, false},
		{"no tenant", mutate(func(p *NewEntryParams) { p.TenantID = "" }), true},
		{"no merchant", mutate(func(p *NewEntryParams) { p.MerchantID = "" }), true},
		{"no transaction group", mutate(func(p *NewEntryParams) { p.TransactionID = "" }), true},
		{"unknown account", mutate(func(p *NewEntryParams) { p.Account = "REVENUE" }), true},
		{"unset side", mutate(func(p *NewEntryParams) { p.Side = "" }), true},
		{"unknown side", mutate(func(p *NewEntryParams) { p.Side = Side("DR") }), true},
		{"unknown entry type", mutate(func(p *NewEntryParams) { p.EntryType = "AUTHORIZATION_HOLD" }), true},
		{"zero amount", mutate(func(p *NewEntryParams) { p.Amount = money.Zero(usd) }), true},
		{"negative amount", mutate(func(p *NewEntryParams) { p.Amount = money.MustNew(-1, usd) }), true},
		{"unsupported currency", mutate(func(p *NewEntryParams) { p.Amount = money.Money{} }), true},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewEntry(tc.params, clock)
			if tc.wantErr != (err != nil) {
				t.Fatalf("NewEntry() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestPartitionMonthFollowsThePayment checks baseline amendment A-02 as it applies here: a
// refund posted months after its capture must still land in the payment's partition, or reading
// a payment with its ledger entries becomes a scan across every month since.
func TestPartitionMonthFollowsThePayment(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 1, 17, 11, 0, 0, 0, time.UTC)
	paymentID := shared.PaymentID(ids.NewAt(ids.PrefixPayment, created))

	// The entry is recorded six months later.
	late := shared.FixedClock{T: time.Date(2026, 7, 2, 8, 0, 0, 0, time.UTC)}
	e, err := NewEntry(NewEntryParams{
		TenantID: testTenant, MerchantID: testMerchant, TransactionID: NewTransactionID(),
		Account: AccountMerchantReceivable, Side: SideDebit, Amount: money.MustNew(8450, "USD"),
		EntryType: EntryRefund, PaymentID: paymentID,
	}, late)
	if err != nil {
		t.Fatalf("NewEntry: %v", err)
	}
	want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := e.PartitionMonth(); !got.Equal(want) {
		t.Fatalf("partition month = %s, want the payment's month %s", got, want)
	}

	// With no payment, the occurrence month is the only sensible key.
	e2, err := NewEntry(NewEntryParams{
		TenantID: testTenant, MerchantID: testMerchant, TransactionID: NewTransactionID(),
		Account: AccountSettlementSuspense, Side: SideDebit, Amount: money.MustNew(100, "USD"),
		EntryType: EntrySettlement,
	}, late)
	if err != nil {
		t.Fatalf("NewEntry: %v", err)
	}
	if got, want := e2.PartitionMonth(), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("merchant-level entry partition month = %s, want %s", got, want)
	}
}

func TestEntryIsIncreaseFollowsNormalSide(t *testing.T) {
	t.Parallel()

	clock := testClock()
	tests := []struct {
		account AccountType
		side    Side
		want    bool
	}{
		{AccountGatewayClearing, SideDebit, true},
		{AccountGatewayClearing, SideCredit, false},
		{AccountMerchantReceivable, SideCredit, true},
		{AccountMerchantReceivable, SideDebit, false},
	}
	for _, tc := range tests {

		t.Run(string(tc.account)+"/"+string(tc.side), func(t *testing.T) {
			t.Parallel()
			e, err := NewEntry(NewEntryParams{
				TenantID: testTenant, MerchantID: testMerchant, TransactionID: NewTransactionID(),
				Account: tc.account, Side: tc.side, Amount: money.MustNew(1, "USD"),
				EntryType: EntryAdjustment,
			}, clock)
			if err != nil {
				t.Fatalf("NewEntry: %v", err)
			}
			if got := e.IsIncrease(); got != tc.want {
				t.Fatalf("IsIncrease() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSideOpposite(t *testing.T) {
	t.Parallel()

	if SideDebit.Opposite() != SideCredit || SideCredit.Opposite() != SideDebit {
		t.Fatal("Side.Opposite does not invert")
	}
	if !SideDebit.IsValid() || !SideCredit.IsValid() || Side("").IsValid() {
		t.Fatal("Side.IsValid is wrong about the zero value")
	}
}
