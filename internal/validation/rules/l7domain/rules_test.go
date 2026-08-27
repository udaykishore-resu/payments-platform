package l7domain_test

import (
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/internal/ruletest"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/l7domain"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

var now = time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)

func eur(minor int64) money.Money { return money.MustNew(minor, "EUR") }

// base is a full capture of an authorized two-step payment, written through a unit of work that
// appends exactly one event and one outbox row in the same transaction.
func base() l7domain.Subject {
	return l7domain.Subject{
		Command: l7domain.Command{
			Kind:                 l7domain.KindPayment,
			Operation:            shared.OpCapture,
			ExpectedVersion:      4,
			TargetPaymentState:   payment.StateCaptured,
			TargetAttemptOutcome: payment.OutcomeDispatched,
			Amount:               eur(100_00),
		},
		Payment: l7domain.PaymentAggregate{
			Present:          true,
			ID:               "pay_1",
			TenantID:         "ten_1",
			MerchantID:       "mrc_1",
			State:            payment.StateAuthorized,
			Version:          4,
			AuthorizedAmount: eur(100_00),
			CapturedTotal:    eur(0),
			RefundedTotal:    eur(0),
			TwoStepFlow:      true,
		},
		Attempt: l7domain.AttemptView{
			Present:    true,
			ID:         "att_1",
			PaymentID:  "pay_1",
			TenantID:   "ten_1",
			MerchantID: "mrc_1",
			Outcome:    payment.OutcomePending,
			Amount:     eur(100_00),
		},
		UnitOfWork: l7domain.UnitOfWork{
			StateChanged:        true,
			EventsAppended:      1,
			VersionIncrementBy:  1,
			OutboxWritesQueued:  1,
			OutboxInSameTxn:     true,
			ExpectedOutboxWrite: true,
		},
		Now: now,
	}
}

// activatable is a merchant that satisfies every §8 activation guard.
func activatable() l7domain.MerchantAggregate {
	return l7domain.MerchantAggregate{
		Present:                   true,
		ID:                        "mrc_1",
		Status:                    merchant.StatusProductionReady,
		Version:                   2,
		CertifiedConnections:      1,
		HasValidPublishedConfig:   true,
		ComplianceAttestationDone: true,
	}
}

func activating(s *l7domain.Subject) {
	s.Command.Kind = l7domain.KindMerchant
	s.Command.TargetMerchantStatus = merchant.StatusActive
	s.Merchant = activatable()
}

func TestL7Rules(t *testing.T) {
	t.Parallel()
	set := l7domain.Rules(l7domain.DefaultDeps())

	ruletest.Run(t, set, base, []ruletest.Case[l7domain.Subject]{
		{
			ID:   "L7.PAYMENT_TRANSITION_IS_ALLOWED",
			Pass: func(s *l7domain.Subject) {},
			Fail: func(s *l7domain.Subject) { s.Command.TargetPaymentState = payment.StateRefunded },
		},
		{
			ID:   "L7.NO_TRANSITION_FROM_TERMINAL",
			Pass: func(s *l7domain.Subject) {},
			Fail: func(s *l7domain.Subject) { s.Payment.State = payment.StateVoided },
		},
		{
			ID: "L7.CREATED_MUST_PASS_THROUGH_PROCESSING",
			Pass: func(s *l7domain.Subject) {
				s.Payment.State = payment.StateCreated
				s.Command.TargetPaymentState = payment.StateProcessing
			},
			Fail: func(s *l7domain.Subject) {
				s.Payment.State = payment.StateCreated
				s.Command.TargetPaymentState = payment.StateCaptured
			},
		},
		{
			ID:   "L7.ATTEMPT_TRANSITION_IS_ALLOWED",
			Pass: func(s *l7domain.Subject) {},
			Fail: func(s *l7domain.Subject) {
				// PENDING → SUCCESS skips DISPATCHED, so an attempt would record a success for
				// a call that was never made.
				s.Command.TargetAttemptOutcome = payment.OutcomeSuccess
			},
		},
		{
			ID: "L7.MERCHANT_TRANSITION_IS_ALLOWED",
			Pass: func(s *l7domain.Subject) {
				activating(s)
			},
			Fail: func(s *l7domain.Subject) {
				activating(s)
				s.Merchant.Status = merchant.StatusCreated
			},
		},
		{
			ID:   "L7.AGGREGATE_VERSION_MATCHES",
			Pass: func(s *l7domain.Subject) {},
			Fail: func(s *l7domain.Subject) { s.Command.ExpectedVersion = 9 },
		},
		{
			ID:   "L7.EVENT_APPENDED_PER_TRANSITION",
			Pass: func(s *l7domain.Subject) {},
			Fail: func(s *l7domain.Subject) { s.UnitOfWork.EventsAppended = 2 },
		},
		{
			ID:   "L7.OUTBOX_WRITE_IN_SAME_TRANSACTION",
			Pass: func(s *l7domain.Subject) {},
			Fail: func(s *l7domain.Subject) { s.UnitOfWork.OutboxInSameTxn = false },
		},
		{
			ID:   "L7.PAYMENT_IMMUTABLE_FIELDS_UNCHANGED",
			Pass: func(s *l7domain.Subject) {},
			Fail: func(s *l7domain.Subject) {
				s.Command.ChangedImmutableFields = []string{"amount", "currency"}
			},
		},
		{
			ID: "L7.MONEY_CURRENCY_CONSISTENT",
			Pass: func(s *l7domain.Subject) {
				s.Operands = l7domain.MoneyOperands{
					Present: true, A: eur(100_00), B: eur(20_00), Label: "capture against authorization",
				}
			},
			Fail: func(s *l7domain.Subject) {
				s.Operands = l7domain.MoneyOperands{
					Present: true, A: eur(100_00), B: money.MustNew(20_00, "GBP"),
					Label: "capture against authorization",
				}
			},
		},
		{
			ID: "L7.REFUNDS_NOT_EXCEED_CAPTURED",
			Pass: func(s *l7domain.Subject) {
				refunding(s, eur(100_00))
			},
			Fail: func(s *l7domain.Subject) {
				refunding(s, eur(100_01))
			},
		},
		{
			ID:   "L7.CAPTURED_NOT_EXCEED_AUTHORIZED",
			Pass: func(s *l7domain.Subject) {},
			Fail: func(s *l7domain.Subject) { s.Command.Amount = eur(100_01) },
		},
		{
			ID: "L7.AT_MOST_ONE_SUCCESSFUL_ATTEMPT",
			Pass: func(s *l7domain.Subject) {
				s.Attempt.Outcome = payment.OutcomeDispatched
				s.Command.TargetAttemptOutcome = payment.OutcomeSuccess
			},
			Fail: func(s *l7domain.Subject) {
				s.Attempt.Outcome = payment.OutcomeDispatched
				s.Command.TargetAttemptOutcome = payment.OutcomeSuccess
				s.Payment.SuccessfulAttempts = 1
			},
		},
		{
			ID:   "L7.ATTEMPT_BELONGS_TO_PAYMENT",
			Pass: func(s *l7domain.Subject) {},
			Fail: func(s *l7domain.Subject) { s.Attempt.PaymentID = "pay_someone_else" },
		},
		{
			ID: "L7.LEDGER_ENTRY_BALANCED",
			Pass: func(s *l7domain.Subject) {
				s.Ledger = l7domain.LedgerWrite{Present: true, Entries: balancedEntries()}
			},
			Fail: func(s *l7domain.Subject) {
				entries := balancedEntries()
				entries[1].CreditMinor = 99_00
				s.Ledger = l7domain.LedgerWrite{Present: true, Entries: entries}
			},
		},
		{
			ID: "L7.LEDGER_IS_APPEND_ONLY",
			Pass: func(s *l7domain.Subject) {
				s.Ledger = l7domain.LedgerWrite{Present: true, Entries: balancedEntries()}
			},
			Fail: func(s *l7domain.Subject) {
				s.Ledger = l7domain.LedgerWrite{
					Present: true, Entries: balancedEntries(), MutatesExistingEntries: true,
				}
			},
		},
		{
			ID: "L7.DISPUTE_RESOLUTION_RESTORES_PRIOR_STATE",
			Pass: func(s *l7domain.Subject) {
				s.Payment.State = payment.StateDisputed
				s.Command.TargetPaymentState = payment.StateCaptured
				s.Command.DisputeResolution = &l7domain.DisputeResolution{
					Won: true, PriorState: payment.StateCaptured,
				}
			},
			Fail: func(s *l7domain.Subject) {
				s.Payment.State = payment.StateDisputed
				s.Command.TargetPaymentState = payment.StateRefunded
				s.Command.DisputeResolution = &l7domain.DisputeResolution{
					Won: true, PriorState: payment.StateCaptured,
				}
			},
		},
		{
			ID:   "L7.ACTIVATION_REQUIRES_CERTIFIED_CONNECTION",
			Pass: activating,
			Fail: func(s *l7domain.Subject) {
				activating(s)
				s.Merchant.CertifiedConnections = 0
			},
		},
		{
			ID:   "L7.ACTIVATION_REQUIRES_VALID_CONFIG",
			Pass: activating,
			Fail: func(s *l7domain.Subject) {
				activating(s)
				s.Merchant.HasValidPublishedConfig = false
			},
		},
		{
			ID:   "L7.ACTIVATION_REQUIRES_ATTESTATION",
			Pass: activating,
			Fail: func(s *l7domain.Subject) {
				activating(s)
				s.Merchant.ComplianceAttestationDone = false
			},
		},
		{
			ID:   "L7.ACTIVATION_REQUIRES_CLEAN_RECONCILIATION",
			Pass: activating,
			Fail: func(s *l7domain.Subject) {
				activating(s)
				s.Merchant.OpenCriticalReconciliation = 2
			},
		},
		{
			ID: "L7.SUSPENSION_PERMITS_MONEY_OUT",
			Pass: func(s *l7domain.Subject) {
				s.Merchant = activatable()
				s.Merchant.Status = merchant.StatusSuspended
				s.Command.Operation = shared.OpRefund
			},
			Fail: func(s *l7domain.Subject) {
				s.Merchant = activatable()
				s.Merchant.Status = merchant.StatusSuspended
				s.Command.Operation = shared.OpAuthorize
			},
		},
		{
			ID: "L7.TERMINATION_REQUIRES_NO_OPEN_PAYMENTS",
			Pass: func(s *l7domain.Subject) {
				terminating(s, 0)
			},
			Fail: func(s *l7domain.Subject) {
				terminating(s, 3)
			},
		},
	})
}

// TestL7DelegatesToTheDomainTransitionTables is the property that keeps this level honest: the
// rules must agree with the machines for every (from, to) pair, because a second copy of a
// transition table is a second definition that diverges the day a state is added.
func TestL7DelegatesToTheDomainTransitionTables(t *testing.T) {
	t.Parallel()
	set := l7domain.Rules(l7domain.DefaultDeps())
	rule, ok := set.Rule("L7.PAYMENT_TRANSITION_IS_ALLOWED")
	if !ok {
		t.Fatal("the payment transition rule is not in the L7 set")
	}

	for _, from := range payment.AllStates {
		for _, to := range payment.AllStates {
			if from == to {
				continue
			}
			s := base()
			s.Payment.State = from
			s.Command.TargetPaymentState = to

			want := payment.Machine().CanTransition(from, to)
			got := rule.Evaluate(t.Context(), s).Passed
			if got != want {
				t.Errorf("(%s → %s): rule says %v, the payment machine says %v", from, to, got, want)
			}
		}
	}

	merchantRule, ok := set.Rule("L7.MERCHANT_TRANSITION_IS_ALLOWED")
	if !ok {
		t.Fatal("the merchant transition rule is not in the L7 set")
	}
	for _, from := range merchant.AllStatuses {
		for _, to := range merchant.AllStatuses {
			if from == to {
				continue
			}
			s := base()
			s.Merchant = activatable()
			s.Merchant.Status = from
			s.Command.Kind = l7domain.KindMerchant
			s.Command.TargetMerchantStatus = to

			want := merchant.Machine().CanTransition(from, to)
			got := merchantRule.Evaluate(t.Context(), s).Passed
			if got != want {
				t.Errorf("(%s → %s): rule says %v, the merchant machine says %v", from, to, got, want)
			}
		}
	}
}

// TestL7ShortCircuitsOnAnIllegalTransition: once the transition is illegal, the invariant
// checks that assume it are answering questions about a state the aggregate will never reach.
func TestL7ShortCircuitsOnAnIllegalTransition(t *testing.T) {
	t.Parallel()
	s := base()
	s.Command.TargetPaymentState = payment.StateRefunded
	s.Command.Amount = eur(100_01) // would also break the capture invariant

	rep := l7domain.Rules(l7domain.DefaultDeps()).Evaluate(t.Context(), s)

	if rep.OK() {
		t.Fatal("an illegal transition was accepted")
	}
	if got := len(rep.Errors()); got != 1 {
		t.Fatalf("ShortCircuit produced %d errors, want exactly the first", got)
	}
	if _, ran := rep.For("L7.CAPTURED_NOT_EXCEED_AUTHORIZED"); ran {
		t.Fatal("an invariant rule ran after the transition was rejected")
	}
}

func refunding(s *l7domain.Subject, amount money.Money) {
	s.Command.Operation = shared.OpRefund
	s.Command.TargetPaymentState = payment.StateRefunded
	s.Payment.State = payment.StateCaptured
	s.Payment.CapturedTotal = eur(100_00)
	s.Payment.RefundedTotal = eur(0)
	s.Command.Amount = amount
}

func terminating(s *l7domain.Subject, openPayments int) {
	s.Command.Kind = l7domain.KindMerchant
	s.Command.TargetMerchantStatus = merchant.StatusTerminated
	s.Merchant = activatable()
	s.Merchant.Status = merchant.StatusActive
	s.Merchant.NonTerminalPayments = openPayments
}

func balancedEntries() []l7domain.LedgerEntry {
	return []l7domain.LedgerEntry{
		{AccountID: "acct_merchant_receivable", DebitMinor: 100_00, Currency: "EUR"},
		{AccountID: "acct_gateway_clearing", CreditMinor: 100_00, Currency: "EUR"},
	}
}
