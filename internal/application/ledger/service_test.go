package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/apptest"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/application/webhook"
	dledger "github.com/udaykishore-resu/payments-platform/internal/domain/ledger"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

var testEpoch = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)

const (
	testTenant   shared.TenantID   = "ten_01HZTESTTENANT00000000000"
	testMerchant shared.MerchantID = "mrc_01HZTESTMERCHANT000000000"
	testPayment  shared.PaymentID  = "pay_01HZTESTPAYMENT000000000"
)

type env struct {
	t     *testing.T
	store *apptest.Store
	uow   *apptest.UnitOfWork
	svc   *Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	store := apptest.NewStore()
	uow := apptest.NewUnitOfWork(store, apptest.NewRecorder())
	return &env{
		t: t, store: store, uow: uow,
		svc: NewService(Deps{
			UoW: uow, Log: &apptest.PostingLog{Store: store}, Clock: apptest.NewClock(testEpoch),
		}),
	}
}

func eur(n int64) money.Money { return money.MustNew(n, "EUR") }

func captureEffect(ref string, gross, fee money.Money) webhook.Effect {
	return webhook.Effect{
		Reference: ref, Kind: spi.KindCaptureSucceeded,
		TenantID: testTenant, MerchantID: testMerchant, PaymentID: testPayment,
		Amount: gross, Fee: fee, GatewayRef: "gwref", OccurredAt: testEpoch,
	}
}

// TestCapturePostsTheDocumentedPair.
//
// The worked example from docs/payment-flow.md §0: EUR 84.50 captured with a EUR 2.75 fee leaves
// the gateway holding 8450, the merchant having earned 8175, and 275 owed to the gateway. The
// arithmetic ties out, which is the only reason double entry is worth its cost.
func TestCapturePostsTheDocumentedPair(t *testing.T) {
	// Verifies: BR-29, FR-80.
	t.Parallel()
	e := newEnv(t)

	if err := e.svc.Apply(context.Background(), captureEffect("evt_1", eur(8450), eur(275))); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	balances := map[dledger.AccountType]int64{
		dledger.AccountGatewayClearing:    8450,
		dledger.AccountMerchantReceivable: 8175,
		dledger.AccountFeesPayable:        275,
	}
	for acct, want := range balances {
		got, err := e.svc.Balance(context.Background(), testTenant, dledger.AccountKey{
			TenantID: testTenant, MerchantID: testMerchant, Type: acct, Currency: "EUR",
		})
		if err != nil {
			t.Fatalf("Balance(%s): %v", acct, err)
		}
		if got.Amount() != want {
			t.Fatalf("%s = %d, want %d", acct, got.Amount(), want)
		}
	}
}

// TestEveryPostingBalances.
//
// Checked per currency, never across: summing USD cents and JPY yen produces a number with no
// unit, and a group that "balances" only after mixing currencies balances by coincidence.
func TestEveryPostingBalances(t *testing.T) {
	// Verifies: FR-81.
	t.Parallel()
	e := newEnv(t)
	effects := []webhook.Effect{
		captureEffect("evt_cap", eur(8450), eur(275)),
		{
			Reference: "evt_ref", Kind: spi.KindRefundSucceeded,
			TenantID: testTenant, MerchantID: testMerchant, PaymentID: testPayment,
			RefundID: "ref_1", Amount: eur(1000), OccurredAt: testEpoch,
		},
		{
			Reference: "evt_disp", Kind: spi.KindDisputeOpened,
			TenantID: testTenant, MerchantID: testMerchant, PaymentID: testPayment,
			Amount: eur(2000), Fee: eur(1500), GatewayRef: "dp_1", OccurredAt: testEpoch,
		},
		{
			Reference: "evt_settle", Kind: spi.KindPayoutSettled,
			TenantID: testTenant, MerchantID: testMerchant, PaymentID: testPayment,
			Amount: eur(5000), Fee: eur(200), GatewayRef: "po_1", OccurredAt: testEpoch,
		},
	}
	for _, eff := range effects {
		if err := e.svc.Apply(context.Background(), eff); err != nil {
			t.Fatalf("Apply(%s): %v", eff.Kind, err)
		}
	}
	for _, tx := range e.store.LedgerTransactions() {
		if err := tx.Balance(); err != nil {
			t.Fatalf("transaction %s does not balance: %v", tx.ID(), err)
		}
	}
	if n := len(e.store.LedgerTransactions()); n != 5 {
		t.Fatalf("got %d transactions, want 5 (capture, fee, refund, dispute, settlement)", n)
	}
}

// TestReplayingAnEventDoesNotDoublePost.
//
// At-least-once delivery means a duplicate is not an edge case, it is Tuesday. The claim is taken
// in the same transaction as the posting, so a crash between them is not representable.
func TestReplayingAnEventDoesNotDoublePost(t *testing.T) {
	// Verifies: FR-80.
	t.Parallel()
	e := newEnv(t)
	eff := captureEffect("evt_1", eur(8450), eur(275))

	for i := 0; i < 5; i++ {
		if err := e.svc.Apply(context.Background(), eff); err != nil {
			t.Fatalf("Apply %d: %v", i, err)
		}
	}
	if n := len(e.store.LedgerTransactions()); n != 2 {
		t.Fatalf("five deliveries produced %d transactions, want 2", n)
	}
	got, err := e.svc.Balance(context.Background(), testTenant, dledger.AccountKey{
		TenantID: testTenant, MerchantID: testMerchant,
		Type: dledger.AccountGatewayClearing, Currency: "EUR",
	})
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if got.Amount() != 8450 {
		t.Fatalf("gateway clearing = %d after five deliveries, want 8450", got.Amount())
	}
}

// TestAFailedPostingLeavesTheReferenceUnclaimed.
//
// The claim and the entries share a transaction. If a claim survived a rolled-back posting, the
// event would be permanently unpostable: every retry would see the claim and do nothing, and the
// money movement would silently never reach the ledger.
func TestAFailedPostingLeavesTheReferenceUnclaimed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	eff := captureEffect("evt_1", eur(8450), money.Money{})
	injected := errors.New("injected")

	err := e.uow.Within(context.Background(), func(ctx context.Context, r ports.Repositories) error {
		if err := e.svc.Post(ctx, r, eff); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("Within returned %v, want the injected error", err)
	}
	if n := len(e.store.LedgerTransactions()); n != 0 {
		t.Fatalf("%d transactions survived a rolled-back posting", n)
	}

	// The retry must actually post.
	if err := e.svc.Apply(context.Background(), eff); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if n := len(e.store.LedgerTransactions()); n != 1 {
		t.Fatalf("the retry produced %d transactions, want 1", n)
	}
}

// TestAPostingRequiresASourceReference. Without one there is no way to recognise a replay, and
// at-least-once delivery guarantees there will be one.
func TestAPostingRequiresASourceReference(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	eff := captureEffect("", eur(1000), money.Money{})
	if err := e.svc.Apply(context.Background(), eff); err == nil {
		t.Fatal("a posting with no source reference was accepted")
	}
}

// TestAnAuthorizationPostsNothing.
//
// An authorization is a hold, not a movement: the issuer has reserved the payer's funds, no money
// has left anyone's account, and the merchant has earned nothing. Recording it would overstate
// the merchant's receivable by the value of every uncaptured authorization at every instant —
// including every one that is ultimately voided and whose money never moves at all.
func TestAnAuthorizationPostsNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.svc.Apply(context.Background(), webhook.Effect{
		Reference: "evt_auth", Kind: spi.KindAuthorizationSucceeded,
		TenantID: testTenant, MerchantID: testMerchant, PaymentID: testPayment,
		Amount: eur(8450), OccurredAt: testEpoch,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n := len(e.store.LedgerTransactions()); n != 0 {
		t.Fatalf("an authorization produced %d ledger transactions, want 0", n)
	}
}

// TestDisputeStagesMoveMoneyInOppositeDirections.
//
// Routing disputed funds through DISPUTES_HELD rather than reversing the capture is what makes
// the three stages legible: at any instant that account is exactly the money a merchant has at
// risk, and a won dispute leaves a trail showing the money was withheld and returned rather than
// showing nothing at all.
func TestDisputeStagesMoveMoneyInOppositeDirections(t *testing.T) {
	// Verifies: BR-27.
	t.Parallel()
	for _, tc := range []struct {
		name string
		won  bool
		// wantHeld is the DISPUTES_HELD balance after opening and then closing the dispute. It is
		// zero either way — the money is no longer at risk — and the difference is *where* it went.
		wantReceivable int64
	}{
		{"won: the funds are released back into clearing", true, 8450},
		{"lost: the loss lands on the merchant's earned position", false, 6450},
	} {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			if err := e.svc.Apply(context.Background(), captureEffect("evt_cap", eur(8450), money.Money{})); err != nil {
				t.Fatalf("capture: %v", err)
			}
			if err := e.svc.Apply(context.Background(), webhook.Effect{
				Reference: "evt_open", Kind: spi.KindDisputeOpened,
				TenantID: testTenant, MerchantID: testMerchant, PaymentID: testPayment,
				Amount: eur(2000), GatewayRef: "dp_1", OccurredAt: testEpoch,
			}); err != nil {
				t.Fatalf("dispute opened: %v", err)
			}
			if err := e.svc.Apply(context.Background(), webhook.Effect{
				Reference: "evt_close", Kind: spi.KindDisputeClosed, DisputeWon: tc.won,
				TenantID: testTenant, MerchantID: testMerchant, PaymentID: testPayment,
				Amount: eur(2000), GatewayRef: "dp_1", OccurredAt: testEpoch,
			}); err != nil {
				t.Fatalf("dispute closed: %v", err)
			}

			held, err := e.svc.Balance(context.Background(), testTenant, dledger.AccountKey{
				TenantID: testTenant, MerchantID: testMerchant,
				Type: dledger.AccountDisputesHeld, Currency: "EUR",
			})
			if err != nil {
				t.Fatalf("Balance: %v", err)
			}
			if !held.IsZero() {
				t.Fatalf("disputes held = %d after the dispute closed, want 0", held.Amount())
			}
			recv, err := e.svc.Balance(context.Background(), testTenant, dledger.AccountKey{
				TenantID: testTenant, MerchantID: testMerchant,
				Type: dledger.AccountMerchantReceivable, Currency: "EUR",
			})
			if err != nil {
				t.Fatalf("Balance: %v", err)
			}
			if recv.Amount() != tc.wantReceivable {
				t.Fatalf("merchant receivable = %d, want %d", recv.Amount(), tc.wantReceivable)
			}
		})
	}
}

// TestReconcileReportsBalancesAndAZeroResidual.
//
// The residual is re-derived from the entries rather than read from a projection, which is the
// point: a report computed from the same projection it is checking cannot detect the projection
// being wrong.
func TestReconcileReportsBalancesAndAZeroResidual(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.svc.Apply(context.Background(), captureEffect("evt_cap", eur(8450), eur(275))); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := e.svc.Apply(context.Background(), webhook.Effect{
		Reference: "evt_ref", Kind: spi.KindRefundSucceeded,
		TenantID: testTenant, MerchantID: testMerchant, PaymentID: testPayment,
		RefundID: "ref_1", Amount: eur(1000), OccurredAt: testEpoch,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	rep, err := e.svc.Reconcile(context.Background(), testTenant, testMerchant, "EUR",
		[]shared.PaymentID{testPayment})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !rep.Balanced() {
		t.Fatalf("the ledger does not tie out: residual %s over %d entries", rep.Residual, rep.EntryCount)
	}
	if rep.EntryCount != 6 {
		t.Fatalf("entry count = %d, want 6 (capture 2, fee 2, refund 2)", rep.EntryCount)
	}
	if len(rep.Accounts) == 0 {
		t.Fatal("the report carries no account balances")
	}
	for i := 1; i < len(rep.Accounts); i++ {
		if rep.Accounts[i-1].Account > rep.Accounts[i].Account {
			t.Fatal("the report's lines are not ordered; two runs would not be diffable")
		}
	}
}

// TestEveryEntryPointAssertsTenantContext.
func TestEveryEntryPointAssertsTenantContext(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := dledger.AccountKey{
		TenantID: testTenant, MerchantID: testMerchant,
		Type: dledger.AccountGatewayClearing, Currency: "EUR",
	}
	if _, err := e.svc.Balance(context.Background(), "", key); err == nil {
		t.Fatal("Balance accepted a request with no tenant context")
	} else if apierror.CodeOf(err) != apierror.CodeMissingTenantContext {
		t.Fatalf("got %s, want MISSING_TENANT_CONTEXT", apierror.CodeOf(err))
	}
	if _, err := e.svc.Reconcile(context.Background(), "", testMerchant, "EUR", nil); err == nil {
		t.Fatal("Reconcile accepted a request with no tenant context")
	}
}

// TestBalanceRefusesAnAccountKeyFromAnotherTenant.
//
// A balance that spans two tenants is a data-isolation defect that reporting would present as a
// number, which is the worst possible way to discover one.
func TestBalanceRefusesAnAccountKeyFromAnotherTenant(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, err := e.svc.Balance(context.Background(), testTenant, dledger.AccountKey{
		TenantID: "ten_01HZOTHER00000000000000", MerchantID: testMerchant,
		Type: dledger.AccountGatewayClearing, Currency: "EUR",
	})
	if err == nil {
		t.Fatal("an account key from another tenant was accepted")
	}
	if apierror.CodeOf(err) != apierror.CodeTenantMismatch {
		t.Fatalf("got %s, want TENANT_MISMATCH", apierror.CodeOf(err))
	}
}
