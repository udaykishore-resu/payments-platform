// Package ledger holds the shadow-ledger use cases: the consumer that turns money movements into
// balanced double-entry transactions, the balance query, and the reconciliation report.
//
// The premise, restated here because every decision below follows from it: this is a *shadow*
// ledger, not a custody ledger. The platform holds no funds (baseline §1.3 A1). Nothing recorded
// here is a legal claim on money or a statement of what any party may demand; it is the
// platform's own view of what moved, recorded so that reconciliation against the gateway's
// settlement report has something to reconcile against, and so that "what did this merchant earn
// and what did it cost" is a query rather than a replay of the event log.
//
// Two properties carry the weight:
//
//   - **Every posting balances.** Debits equal credits within each currency, checked in the
//     domain, re-checked by the repository and constrained by the database. Single-entry
//     bookkeeping cannot detect a dropped or duplicated posting; double entry turns both into an
//     arithmetic failure at write time instead of a quarterly surprise.
//   - **Replaying an event does not double-post.** Delivery is at-least-once, so a duplicate is
//     not an edge case — it is Tuesday. Idempotency is a claim on the source reference, taken in
//     the same transaction as the posting, so the claim and the entries cannot disagree.
package ledger

import (
	"context"
	"sort"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/application/webhook"
	"github.com/udaykishore-resu/payments-platform/internal/domain/ledger"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// PostingGroup names this consumer in the deduplication log.
//
// It is a consumer group rather than a global key because two independent consumers may
// legitimately process the same event — the ledger and the notification service, say — and a
// global "processed" flag would let whichever ran first suppress the other.
const PostingGroup = "ledger"

// PostingLog is the idempotency record for ledger postings.
//
// It takes the Repositories bundle rather than opening its own transaction, and that is the whole
// design: a dedup row committed separately from the effect it guards is a dedup row that lies.
// Either both commit or neither does, and a crash between them is not representable.
type PostingLog interface {
	// Claim records that the reference has been posted, reporting false if it already had been.
	Claim(ctx context.Context, r ports.Repositories, group, reference string) (bool, error)
}

// Deps is the ledger service's dependency set.
type Deps struct {
	UoW   ports.UnitOfWork
	Log   PostingLog
	Clock shared.Clock
}

// Service posts to the ledger and answers questions about it.
type Service struct {
	deps Deps
}

// NewService constructs the service.
func NewService(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = shared.SystemClock{}
	}
	return &Service{deps: d}
}

// Post appends the entries one money movement implies, inside the caller's transaction.
//
// It implements webhook.LedgerPoster, which is why it takes a Repositories bundle: the payment's
// state change, the ledger entries and the outbox event commit together or not at all. A poster
// that opened its own transaction would reintroduce exactly the dual write the outbox pattern
// exists to remove — and would do it in the one place where the two halves are a state change and
// the money that state change describes.
func (s *Service) Post(ctx context.Context, r ports.Repositories, e webhook.Effect) error {
	if e.Reference == "" {
		// Without a reference there is no way to recognise a replay, and at-least-once delivery
		// guarantees there will be one.
		return apierror.New(apierror.CodeValidationFailed,
			"a ledger posting requires a source reference to deduplicate on")
	}

	if s.deps.Log != nil {
		fresh, err := s.deps.Log.Claim(ctx, r, PostingGroup, e.Reference)
		if err != nil {
			return err
		}
		if !fresh {
			// A replay. Silently doing nothing is correct here and only here: the entries the
			// replay would have written already exist, and writing them again is the double-post
			// this whole mechanism exists to prevent.
			return nil
		}
	}

	txs, err := s.build(e)
	if err != nil {
		return err
	}
	for _, tx := range txs {
		if tx == nil {
			continue
		}
		// Balance is re-checked here even though the builders seal their output, because the
		// open NewTransaction path exists and a caller who composed one by hand must not be able
		// to commit an imbalance. The check is a gate, not a courtesy.
		if err := tx.Balance(); err != nil {
			return err
		}
		if err := r.Ledger.Append(ctx, tx); err != nil {
			return err
		}
	}
	return nil
}

// build turns one effect into the transactions it implies.
//
// A slice rather than a single transaction because a capture with a known fee is genuinely two
// movements: the capture and the fee. They are separate groups rather than a netted one because
// the capture-time fee is an *estimate* and the settlement row is truth — keeping them apart
// makes the variance an explicit adjustment rather than an unexplained residual in the account we
// reconcile against the gateway.
func (s *Service) build(e webhook.Effect) ([]*ledger.Transaction, error) {
	switch e.Kind {
	case spi.KindCaptureSucceeded:
		capture, err := ledger.CaptureTransaction(ledger.CaptureParams{
			TenantID: e.TenantID, MerchantID: e.MerchantID, PaymentID: e.PaymentID,
			AttemptID: e.AttemptID, Gross: e.Amount, GatewayRef: e.GatewayRef,
			OccurredAt: e.OccurredAt,
		}, s.deps.Clock)
		if err != nil {
			return nil, err
		}
		out := []*ledger.Transaction{capture}
		if e.Fee.IsValid() && e.Fee.IsPositive() {
			fee, err := ledger.FeeTransaction(ledger.FeeParams{
				TenantID: e.TenantID, MerchantID: e.MerchantID, PaymentID: e.PaymentID,
				AttemptID: e.AttemptID, Fee: e.Fee, GatewayRef: e.GatewayRef,
				Description: "processing fee", OccurredAt: e.OccurredAt,
			}, s.deps.Clock)
			if err != nil {
				return nil, err
			}
			out = append(out, fee)
		}
		return out, nil

	case spi.KindRefundSucceeded:
		tx, err := ledger.RefundTransaction(ledger.RefundParams{
			TenantID: e.TenantID, MerchantID: e.MerchantID, PaymentID: e.PaymentID,
			RefundID: e.RefundID, Amount: e.Amount, GatewayRef: e.GatewayRef,
			OccurredAt: e.OccurredAt,
		}, s.deps.Clock)
		if err != nil {
			return nil, err
		}
		return []*ledger.Transaction{tx}, nil

	case spi.KindDisputeOpened:
		tx, err := ledger.ChargebackTransaction(ledger.ChargebackParams{
			TenantID: e.TenantID, MerchantID: e.MerchantID, PaymentID: e.PaymentID,
			Stage: ledger.DisputeOpened, Amount: e.Amount, Fee: e.Fee,
			DisputeRef: e.GatewayRef, OccurredAt: e.OccurredAt,
		}, s.deps.Clock)
		if err != nil {
			return nil, err
		}
		return []*ledger.Transaction{tx}, nil

	case spi.KindDisputeClosed:
		stage := ledger.DisputeLost
		if e.DisputeWon {
			stage = ledger.DisputeWon
		}
		tx, err := ledger.ChargebackTransaction(ledger.ChargebackParams{
			TenantID: e.TenantID, MerchantID: e.MerchantID, PaymentID: e.PaymentID,
			Stage: stage, Amount: e.Amount, DisputeRef: e.GatewayRef,
			OccurredAt: e.OccurredAt,
		}, s.deps.Clock)
		if err != nil {
			return nil, err
		}
		return []*ledger.Transaction{tx}, nil

	case spi.KindPayoutSettled:
		fee := e.Fee
		if !fee.IsValid() {
			fee = money.Zero(e.Amount.Currency())
		}
		net, err := e.Amount.Sub(fee)
		if err != nil {
			return nil, apierror.Wrap(err, apierror.CodeGatewayContractViolation,
				"the settlement amounts are not in a single currency")
		}
		tx, err := ledger.SettlementTransaction(ledger.SettlementParams{
			TenantID: e.TenantID, MerchantID: e.MerchantID, PaymentID: e.PaymentID,
			Gross: e.Amount, Fees: fee, Net: net, SettlementRef: e.GatewayRef,
			OccurredAt: e.OccurredAt,
		}, s.deps.Clock)
		if err != nil {
			return nil, err
		}
		return []*ledger.Transaction{tx}, nil

	default:
		// An authorization is a hold, not a movement, and there is deliberately no entry type for
		// one: recording it would overstate the merchant's receivable by the value of every
		// uncaptured authorization at every instant. Everything else here is an event the ledger
		// simply has nothing to say about.
		return nil, nil
	}
}

// Apply consumes one money movement in its own transaction.
//
// It is the entry point for the event-driven path — a Kafka consumer folding
// `payment.captured.v1` into the ledger — as distinct from Post, which is called by a use case
// that already owns a transaction. The two share build and the idempotency claim, so a movement
// that arrives by both routes posts exactly once.
func (s *Service) Apply(ctx context.Context, e webhook.Effect) error {
	return s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return s.Post(ctx, r, e)
	})
}

// Balance answers the balance of one account.
//
// One account is (tenant, merchant, type, currency), and all four components are load-bearing —
// see ledger.AccountKey. In particular the currency: an account holding both cents and yen has a
// balance that is an integer with no unit, which is not a balance but a coincidence of
// arithmetic.
func (s *Service) Balance(ctx context.Context, tenantID shared.TenantID, key ledger.AccountKey) (money.Money, error) {
	if err := assertTenant(tenantID); err != nil {
		return money.Money{}, err
	}
	if key.TenantID.IsZero() {
		key.TenantID = tenantID
	}
	if key.TenantID != tenantID {
		return money.Money{}, apierror.New(apierror.CodeTenantMismatch,
			"the account key names a different tenant from the request")
	}
	if err := key.Validate(); err != nil {
		return money.Money{}, err
	}
	var out money.Money
	err := s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		var err error
		out, err = r.Ledger.Balance(ctx, key)
		return err
	})
	return out, err
}

// AccountBalance is one line of a reconciliation report.
type AccountBalance struct {
	Account  ledger.AccountType
	Currency money.Currency
	Balance  money.Money
}

// Report is the reconciliation view of one merchant's position.
//
// It carries the per-account balances *and* the raw debit and credit totals, because the two
// answer different questions. The balances are what a merchant statement is built from; the
// totals are what proves the ledger is internally consistent, and a non-zero Residual is the
// LEDGER_IMBALANCE condition that pages someone (docs/payment-flow.md §16.3, CRITICAL, no
// auto-resolution).
type Report struct {
	MerchantID shared.MerchantID
	Currency   money.Currency
	Accounts   []AccountBalance
	Debits     money.Money
	Credits    money.Money
	// Residual is debits minus credits. It must be zero. A non-zero value means an entry was
	// dropped or duplicated, which single-entry bookkeeping could not have detected at all.
	Residual money.Money
	// EntryCount is reported alongside the residual because two projections with the same balance
	// and different entry counts have found each other's bug.
	EntryCount int
}

// Balanced reports whether the ledger ties out.
func (r Report) Balanced() bool { return r.Residual.IsZero() }

// Reconcile builds the report for one merchant and currency from the entries themselves.
//
// It re-derives the totals from the entry stream rather than reading a projection, and that is
// the point: a report computed from the same projection it is checking cannot detect the
// projection being wrong. The entries are the source of truth; everything else is a fold over
// them.
func (s *Service) Reconcile(ctx context.Context, tenantID shared.TenantID,
	m shared.MerchantID, currency money.Currency, payments []shared.PaymentID) (*Report, error) {

	if err := assertTenant(tenantID); err != nil {
		return nil, err
	}
	if !currency.IsSupported() {
		return nil, apierror.Newf(apierror.CodeCurrencyNotSupported,
			"cannot reconcile in %q", currency)
	}

	rep := &Report{
		MerchantID: m, Currency: currency,
		Debits: money.Zero(currency), Credits: money.Zero(currency),
		Residual: money.Zero(currency),
	}
	balances := map[ledger.AccountType]money.Money{}

	if err := s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		for _, id := range payments {
			entries, err := r.Ledger.EntriesForPayment(ctx, id)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if e.MerchantID() != m || e.Currency() != currency {
					continue
				}
				rep.EntryCount++
				cur, ok := balances[e.Account()]
				if !ok {
					cur = money.Zero(currency)
				}
				delta := e.Amount()
				if e.Side() != e.Account().NormalSide() {
					delta = delta.Neg()
				}
				sum, err := cur.Add(delta)
				if err != nil {
					return err
				}
				balances[e.Account()] = sum

				if e.Side() == ledger.SideDebit {
					if rep.Debits, err = rep.Debits.Add(e.Amount()); err != nil {
						return err
					}
				} else {
					if rep.Credits, err = rep.Credits.Add(e.Amount()); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	residual, err := rep.Debits.Sub(rep.Credits)
	if err != nil {
		return nil, err
	}
	rep.Residual = residual

	rep.Accounts = make([]AccountBalance, 0, len(balances))
	for acct, bal := range balances {
		rep.Accounts = append(rep.Accounts, AccountBalance{Account: acct, Currency: currency, Balance: bal})
	}
	// Sorted so that two runs over the same entries produce the same report. A report whose line
	// order depends on map iteration cannot be diffed against yesterday's.
	sort.Slice(rep.Accounts, func(i, j int) bool { return rep.Accounts[i].Account < rep.Accounts[j].Account })
	return rep, nil
}

func assertTenant(t shared.TenantID) error {
	if t.IsZero() {
		return apierror.New(apierror.CodeMissingTenantContext, "the request carries no tenant context")
	}
	return nil
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-29, FR-80, FR-81.
//
// Ledger postings driven by domain events, idempotent on the source reference
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
