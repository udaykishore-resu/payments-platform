package l7domain

import (
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/validation/engine"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/internal/ruledef"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

func init() {
	ruledef.Register(defs(DefaultDeps()), "payments-core", "2026-01-01", engine.Enforce)
}

// Rules returns the L7 rule set.
//
// ShortCircuit. The rules are sequentially dependent in the strong sense: asking whether the
// captured total stays within the authorized amount is not a meaningful question about a
// transition the machine has already rejected, and answering it anyway produces a second,
// confusing failure on the same request.
func Rules(_ Deps) engine.RuleSet[Subject] {
	return engine.RuleSet[Subject]{
		Name:  "L7.domain",
		Mode:  engine.ShortCircuit,
		Rules: ruledef.Build(defs(Deps{})),
	}
}

func defs(_ Deps) []ruledef.Def[Subject] {
	movesPayment := func(s Subject) bool {
		return s.Payment.Present && s.Command.TargetPaymentState != "" &&
			s.Command.TargetPaymentState != s.Payment.State
	}
	movesMerchant := func(s Subject) bool {
		return s.Merchant.Present && s.Command.TargetMerchantStatus != "" &&
			s.Command.TargetMerchantStatus != s.Merchant.Status
	}
	activating := func(s Subject) bool {
		return s.Merchant.Present && s.Command.TargetMerchantStatus == merchant.StatusActive
	}
	badTransition := string(apierror.CodeInvalidStateTransition)
	internal := string(apierror.CodeInternalError)

	return []ruledef.Def[Subject]{
		{
			ID: "L7.PAYMENT_TRANSITION_IS_ALLOWED", Severity: engine.Error,
			Code: badTransition, Field: "/state", Pure: true,
			Desc:        "the (from, to) pair is in the payment transition table",
			Remediation: "This payment's current state does not permit that transition. Re-read the payment and choose an operation its state allows.",
			Applies:     movesPayment,
			Check: func(s Subject) string {
				// Asked of the domain machine, never restated here.
				if payment.Machine().CanTransition(s.Payment.State, s.Command.TargetPaymentState) {
					return ""
				}
				return "payment is " + string(s.Payment.State) + "; " +
					string(s.Command.TargetPaymentState) + " is not a permitted transition"
			},
		},
		{
			ID: "L7.NO_TRANSITION_FROM_TERMINAL", Severity: engine.Error,
			Code: badTransition, Field: "/state", Pure: true,
			Desc:        "a payment in a terminal state accepts no transition at all",
			Remediation: "This payment is terminal and can no longer change state. Create a new payment.",
			Applies:     func(s Subject) bool { return s.Payment.Present && s.Command.TargetPaymentState != "" },
			Check: func(s Subject) string {
				if !s.Payment.State.IsTerminal() {
					return ""
				}
				return "payment " + string(s.Payment.ID) + " is terminal in state " + string(s.Payment.State)
			},
		},
		{
			ID: "L7.CREATED_MUST_PASS_THROUGH_PROCESSING", Severity: engine.Error,
			Code: badTransition, Field: "/state", Pure: true,
			Desc:        "a payment may not jump from CREATED straight to CAPTURED",
			Remediation: "Internal transition error; the request was not applied.",
			Applies: func(s Subject) bool {
				return s.Payment.Present && s.Payment.State == payment.StateCreated
			},
			Check: func(s Subject) string {
				// Called out as its own rule rather than left to the table because it is the
				// transition an optimisation keeps trying to introduce: skipping PROCESSING
				// means there is no state in which "we have dispatched and do not yet know" can
				// be represented, and that state is the whole defence against a double charge.
				if s.Command.TargetPaymentState != payment.StateCaptured {
					return ""
				}
				return "a payment must pass through PROCESSING before it can be CAPTURED"
			},
		},
		{
			ID: "L7.ATTEMPT_TRANSITION_IS_ALLOWED", Severity: engine.Error,
			Code: badTransition, Field: "/attempt/outcome", Pure: true,
			Desc:        "the attempt outcome transition is in the attempt machine's table and outcomes are terminal",
			Remediation: "Internal attempt transition error; the request was not applied.",
			Applies: func(s Subject) bool {
				return s.Attempt.Present && s.Command.TargetAttemptOutcome != "" &&
					s.Command.TargetAttemptOutcome != s.Attempt.Outcome
			},
			Check: func(s Subject) string {
				if payment.AttemptMachine().CanTransition(s.Attempt.Outcome, s.Command.TargetAttemptOutcome) {
					return ""
				}
				return "attempt is " + string(s.Attempt.Outcome) + "; " +
					string(s.Command.TargetAttemptOutcome) + " is not a permitted transition"
			},
		},
		{
			ID: "L7.MERCHANT_TRANSITION_IS_ALLOWED", Severity: engine.Error,
			Code: badTransition, Field: "/merchant/status", Pure: true,
			Desc:        "the (from, to) pair is in the merchant lifecycle table",
			Remediation: "This merchant's current state does not permit that transition.",
			Applies:     movesMerchant,
			Check: func(s Subject) string {
				if merchant.Machine().CanTransition(s.Merchant.Status, s.Command.TargetMerchantStatus) {
					return ""
				}
				return "merchant is " + string(s.Merchant.Status) + "; " +
					string(s.Command.TargetMerchantStatus) + " is not permitted"
			},
		},
		{
			ID: "L7.AGGREGATE_VERSION_MATCHES", Severity: engine.Error,
			Code: string(apierror.CodeConfigurationVersionConflict), Field: "/version", Pure: true,
			Desc:        "the command's expected version equals the aggregate's version (invariant I5)",
			Remediation: "The resource changed concurrently. Re-read it and retry.",
			Applies:     func(s Subject) bool { return s.Command.ExpectedVersion != 0 },
			Check: func(s Subject) string {
				actual, ok := aggregateVersion(s)
				if !ok {
					return ""
				}
				if s.Command.ExpectedVersion == actual {
					return ""
				}
				return "the command expected version " + itoa64(int64(s.Command.ExpectedVersion)) +
					" but the aggregate is at " + itoa64(int64(actual))
			},
		},
		{
			ID: "L7.EVENT_APPENDED_PER_TRANSITION", Severity: engine.Error,
			Code: internal, Field: "/events", Pure: true,
			Desc:        "a state change appends exactly one event row and increments the version by one (I5)",
			Remediation: "Internal consistency error; the change was rolled back.",
			Applies:     func(s Subject) bool { return s.UnitOfWork.StateChanged },
			Check: func(s Subject) string {
				if s.UnitOfWork.EventsAppended != 1 {
					return "the state change appended " + itoa(s.UnitOfWork.EventsAppended) +
						" events; exactly one is required"
				}
				if s.UnitOfWork.VersionIncrementBy != 1 {
					return "the state change incremented the version by " +
						itoa64(s.UnitOfWork.VersionIncrementBy) + "; exactly one is required"
				}
				return ""
			},
		},
		{
			ID: "L7.OUTBOX_WRITE_IN_SAME_TRANSACTION", Severity: engine.Error,
			Code: internal, Field: "/outbox", Pure: true,
			Desc:        "the outbox insert shares the state row's transaction",
			Remediation: "Internal consistency error; the change was rolled back.",
			Applies: func(s Subject) bool {
				return s.UnitOfWork.StateChanged && s.UnitOfWork.ExpectedOutboxWrite
			},
			Check: func(s Subject) string {
				// Two transactions instead of one is the classic dual-write bug: the state
				// commits, the process dies, and the event never leaves. Everything downstream
				// silently disagrees with the database from then on.
				if s.UnitOfWork.OutboxWritesQueued == 0 {
					return "no outbox event was queued for a state change that publishes one"
				}
				if !s.UnitOfWork.OutboxInSameTxn {
					return "the outbox insert is not in the same transaction as the state row"
				}
				return ""
			},
		},
		{
			ID: "L7.PAYMENT_IMMUTABLE_FIELDS_UNCHANGED", Severity: engine.Error,
			Code: badTransition, Field: "/payment", Pure: true,
			Desc:        "amount, currency, merchant and tenant never change after creation (invariant I4)",
			Remediation: "A payment's amount, currency and merchant cannot be modified. Create a new payment.",
			Applies:     func(s Subject) bool { return s.Payment.Present },
			Check: func(s Subject) string {
				if len(s.Command.ChangedImmutableFields) == 0 {
					return ""
				}
				return "the command would change immutable field(s): " +
					strings.Join(s.Command.ChangedImmutableFields, ", ")
			},
		},
		{
			ID: "L7.MONEY_CURRENCY_CONSISTENT", Severity: engine.Error,
			Code: string(apierror.CodeCurrencyNotSupported), Field: "/amount", Pure: true,
			Desc:        "both operands of a money operation share a currency",
			Remediation: "The two amounts are in different currencies; the platform never converts implicitly.",
			Applies:     func(s Subject) bool { return s.Operands.Present },
			Check: func(s Subject) string {
				if s.Operands.A.Currency() == s.Operands.B.Currency() {
					return ""
				}
				return "currency mismatch in " + s.Operands.Label + ": " +
					string(s.Operands.A.Currency()) + " and " + string(s.Operands.B.Currency())
			},
		},
		{
			ID: "L7.REFUNDS_NOT_EXCEED_CAPTURED", Severity: engine.Error,
			Code: string(apierror.CodeRefundExceedsCaptured), Field: "/amount", Pure: true,
			Desc:        "the sum of refunds never exceeds the captured amount (invariant I1)",
			Remediation: "This refund exceeds the payment's refundable balance.",
			Applies: func(s Subject) bool {
				return s.Payment.Present && s.Command.Operation == refundOp
			},
			Check: func(s Subject) string {
				total, err := s.Payment.RefundedTotal.Add(s.Command.Amount)
				if err != nil {
					return "the refund currency does not match the payment currency"
				}
				over, err := total.GreaterThan(s.Payment.CapturedTotal)
				if err != nil {
					return "the refund currency does not match the captured currency"
				}
				if !over {
					return ""
				}
				balance, subErr := s.Payment.CapturedTotal.Sub(s.Payment.RefundedTotal)
				if subErr != nil {
					return "the refund exceeds the refundable balance"
				}
				return "the refundable balance is " + balance.String()
			},
		},
		{
			ID: "L7.CAPTURED_NOT_EXCEED_AUTHORIZED", Severity: engine.Error,
			Code: string(apierror.CodeAmountExceedsLimit), Field: "/amount", Pure: true,
			Desc:        "the captured amount never exceeds the authorized amount (invariant I2)",
			Remediation: "This capture exceeds the payment's capturable balance.",
			Applies: func(s Subject) bool {
				return s.Payment.Present && s.Payment.TwoStepFlow && s.Command.Operation == captureOp
			},
			Check: func(s Subject) string {
				total, err := s.Payment.CapturedTotal.Add(s.Command.Amount)
				if err != nil {
					return "the capture currency does not match the payment currency"
				}
				over, err := total.GreaterThan(s.Payment.AuthorizedAmount)
				if err != nil {
					return "the capture currency does not match the authorized currency"
				}
				if !over {
					return ""
				}
				balance, subErr := s.Payment.AuthorizedAmount.Sub(s.Payment.CapturedTotal)
				if subErr != nil {
					return "the capture exceeds the capturable balance"
				}
				return "the capturable balance is " + balance.String()
			},
		},
		{
			ID: "L7.AT_MOST_ONE_SUCCESSFUL_ATTEMPT", Severity: engine.Error,
			Code: string(apierror.CodePaymentAlreadyProcessed), Field: "/attempt", Pure: true,
			Desc:        "at most one attempt per payment reaches a successful terminal state (invariant I3)",
			Remediation: "This payment already succeeded on another attempt.",
			Applies: func(s Subject) bool {
				return s.Payment.Present && s.Command.TargetAttemptOutcome == payment.OutcomeSuccess
			},
			Check: func(s Subject) string {
				// This is the structural anti-double-charge invariant. The rule explains it;
				// the partial unique index on (payment_id) WHERE outcome='SUCCESS' enforces it
				// even when the rule is bypassed or wrong.
				if s.Payment.SuccessfulAttempts == 0 {
					return ""
				}
				return "payment " + string(s.Payment.ID) + " already has a successful attempt"
			},
		},
		{
			ID: "L7.ATTEMPT_BELONGS_TO_PAYMENT", Severity: engine.Error,
			Code: internal, Field: "/attempt/paymentId", Pure: true,
			Desc:        "the attempt's payment, tenant and merchant match the payment being changed",
			Remediation: "Internal reference error; the request was not applied.",
			Applies:     func(s Subject) bool { return s.Attempt.Present && s.Payment.Present },
			Check: func(s Subject) string {
				switch {
				case s.Attempt.PaymentID != s.Payment.ID:
					return "the attempt belongs to a different payment"
				case s.Attempt.TenantID != s.Payment.TenantID:
					return "the attempt belongs to a different tenant"
				case s.Attempt.MerchantID != s.Payment.MerchantID:
					return "the attempt belongs to a different merchant"
				}
				return ""
			},
		},
		{
			ID: "L7.LEDGER_ENTRY_BALANCED", Severity: engine.Error,
			Code: internal, Field: "/ledger", Pure: true,
			Desc:        "debits equal credits within an entry group and the currency is uniform",
			Remediation: "Internal ledger error; the transaction was rolled back.",
			Applies:     func(s Subject) bool { return s.Ledger.Present && len(s.Ledger.Entries) > 0 },
			Check: func(s Subject) string {
				var debits, credits int64
				currency := s.Ledger.Entries[0].Currency
				for _, e := range s.Ledger.Entries {
					if e.Currency != currency {
						return "the entry group mixes " + string(currency) + " and " + string(e.Currency)
					}
					debits += e.DebitMinor
					credits += e.CreditMinor
				}
				if debits != credits {
					return "debits total " + itoa64(debits) + " and credits total " + itoa64(credits)
				}
				return ""
			},
		},
		{
			ID: "L7.LEDGER_IS_APPEND_ONLY", Severity: engine.Error,
			Code: internal, Field: "/ledger", Pure: true,
			Desc:        "no write updates or deletes an existing ledger entry",
			Remediation: "Ledger entries are immutable; post a reversing entry instead.",
			Applies:     func(s Subject) bool { return s.Ledger.Present },
			Check: func(s Subject) string {
				if !s.Ledger.MutatesExistingEntries {
					return ""
				}
				return "the write would modify an existing ledger entry"
			},
		},
		{
			ID: "L7.DISPUTE_RESOLUTION_RESTORES_PRIOR_STATE", Severity: engine.Error,
			Code: badTransition, Field: "/state", Pure: true,
			Desc:        "a won dispute returns the payment to its prior state; a lost one moves it to REFUNDED",
			Remediation: "The dispute resolution could not be applied; an exception was opened.",
			Applies:     func(s Subject) bool { return s.Command.DisputeResolution != nil && s.Payment.Present },
			Check: func(s Subject) string {
				r := s.Command.DisputeResolution
				if r.Won {
					if s.Command.TargetPaymentState != r.PriorState {
						return "a won dispute must return the payment to " + string(r.PriorState) +
							", not " + string(s.Command.TargetPaymentState)
					}
					if r.PriorState != payment.StateCaptured && r.PriorState != payment.StateSettled {
						return "a dispute can only be won back into CAPTURED or SETTLED, not " +
							string(r.PriorState)
					}
					return ""
				}
				if s.Command.TargetPaymentState != payment.StateRefunded {
					return "a lost dispute must move the payment to REFUNDED, not " +
						string(s.Command.TargetPaymentState)
				}
				return ""
			},
		},
		{
			ID: "L7.ACTIVATION_REQUIRES_CERTIFIED_CONNECTION", Severity: engine.Error,
			Code: badTransition, Field: "/merchant/connections", Pure: true,
			Desc:        "activation requires at least one certified gateway connection",
			Remediation: "At least one certified gateway connection is required to activate a merchant.",
			Applies:     activating,
			Check: func(s Subject) string {
				if s.Merchant.CertifiedConnections >= 1 {
					return ""
				}
				return "this merchant has no certified gateway connection"
			},
		},
		{
			ID: "L7.ACTIVATION_REQUIRES_VALID_CONFIG", Severity: engine.Error,
			Code: badTransition, Field: "/merchant/configuration", Pure: true,
			Desc:        "activation requires a published, non-empty, L4-valid configuration",
			Remediation: "Publish a valid configuration before activating the merchant.",
			Applies:     activating,
			Check: func(s Subject) string {
				if s.Merchant.HasValidPublishedConfig {
					return ""
				}
				return "this merchant has no valid published configuration"
			},
		},
		{
			ID: "L7.ACTIVATION_REQUIRES_ATTESTATION", Severity: engine.Error,
			Code: badTransition, Field: "/merchant/compliance", Pure: true,
			Desc:        "activation requires a completed compliance attestation",
			Remediation: "The compliance attestation is outstanding; complete it before activating.",
			Applies:     activating,
			Check: func(s Subject) string {
				if s.Merchant.ComplianceAttestationDone {
					return ""
				}
				return "the compliance attestation is outstanding"
			},
		},
		{
			ID: "L7.ACTIVATION_REQUIRES_CLEAN_RECONCILIATION", Severity: engine.Error,
			Code: badTransition, Field: "/merchant/reconciliation", Pure: true,
			Desc:        "activation requires no open CRITICAL reconciliation exception",
			Remediation: "Resolve the open critical reconciliation exceptions before activating.",
			Applies:     activating,
			Check: func(s Subject) string {
				if s.Merchant.OpenCriticalReconciliation == 0 {
					return ""
				}
				return itoa(s.Merchant.OpenCriticalReconciliation) +
					" critical reconciliation exception(s) are open"
			},
		},
		{
			ID: "L7.SUSPENSION_PERMITS_MONEY_OUT", Severity: engine.Error,
			Code: string(apierror.CodeMerchantNotActive), Field: "/merchant/status", Pure: true,
			Desc:        "a suspended merchant may still refund and void but may not take new payments",
			Remediation: "This merchant is suspended; refunds and voids remain available, new payments do not.",
			Applies: func(s Subject) bool {
				return s.Merchant.Present && s.Merchant.Status == merchant.StatusSuspended &&
					s.Command.Operation != ""
			},
			Check: func(s Subject) string {
				// Delegated to the merchant domain: it owns the answer to "may this state still
				// return money", and it is deliberately broader than "may it take money".
				if s.Merchant.Status.CanIssueRefunds() &&
					(s.Command.Operation == refundOp || s.Command.Operation == voidOp) {
					return ""
				}
				return "merchant is suspended; only refunds and voids are permitted"
			},
		},
		{
			ID: "L7.TERMINATION_REQUIRES_NO_OPEN_PAYMENTS", Severity: engine.Error,
			Code: badTransition, Field: "/merchant/status", Pure: true,
			Desc:        "termination requires zero payments in a non-terminal state",
			Remediation: "Settle or resolve the in-flight payments before terminating the merchant.",
			Applies: func(s Subject) bool {
				return s.Merchant.Present && s.Command.TargetMerchantStatus == merchant.StatusTerminated
			},
			Check: func(s Subject) string {
				if s.Merchant.NonTerminalPayments == 0 {
					return ""
				}
				return itoa(s.Merchant.NonTerminalPayments) +
					" payments are still in flight; settle or resolve them first"
			},
		},
	}
}

// The gateway operations L7 reasons about, aliased so the rule table reads as the invariant.
const (
	captureOp = shared.OpCapture
	refundOp  = shared.OpRefund
	voidOp    = shared.OpVoid
)

// --- helpers ---------------------------------------------------------------------------------

// aggregateVersion returns the version of whichever aggregate the command targets.
func aggregateVersion(s Subject) (shared.Version, bool) {
	switch s.Command.Kind {
	case KindPayment, KindAttempt:
		if s.Payment.Present {
			return s.Payment.Version, true
		}
	case KindMerchant:
		if s.Merchant.Present {
			return s.Merchant.Version, true
		}
	default:
		// KindLedger has no aggregate version of its own; it falls through to the payment version
		// below, which is the aggregate a ledger command is ordered against
	}
	if s.Payment.Present {
		return s.Payment.Version, true
	}
	if s.Merchant.Present {
		return s.Merchant.Version, true
	}
	return 0, false
}

func itoa(n int) string { return itoa64(int64(n)) }

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
