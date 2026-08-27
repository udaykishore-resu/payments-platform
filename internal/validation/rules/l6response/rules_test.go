package l6response_test

import (
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/internal/ruletest"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/l6response"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

var now = time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)

func eur(minor int64) money.Money { return money.MustNew(minor, "EUR") }

func deps() l6response.Deps { return l6response.DefaultDeps() }

// base is a well-behaved synchronous capture response: it parses, it echoes the dispatched
// amount and currency exactly, it carries a transaction reference and the idempotency key, and
// the state it maps to is reachable from the payment's current state.
func base() l6response.Subject {
	return l6response.Subject{
		Kind: l6response.KindResponse,
		Attempt: l6response.Attempt{
			Operation:                   shared.OpCapture,
			DispatchedAmount:            eur(100_00),
			GatewayIdempotencyKey:       "gwidem_1",
			GatewayEchoesIdempotencyKey: true,
		},
		Payment: l6response.PaymentState{
			Present:          true,
			Current:          payment.StateProcessing,
			AuthorizedAmount: eur(100_00),
			CapturedTotal:    eur(0),
		},
		Raw: []byte(`{"id":"ch_1","status":"succeeded","amount":10000,"currency":"eur"}`),
		Normalized: l6response.Normalized{
			Parsed:               true,
			SchemaComplete:       true,
			APIVersion:           "2026-01-15",
			GatewayStatus:        "succeeded",
			StatusMappable:       true,
			Outcome:              l6response.OutcomeSuccess,
			TransactionID:        "ch_1",
			EchoedIdempotencyKey: "gwidem_1",
			EchoedAmount:         eur(100_00),
			EchoedCurrency:       "EUR",
			CapturedTotal:        eur(100_00),
			RefundedTotal:        eur(0),
			MappedState:          payment.StateCaptured,
		},
		PinnedAPIVersion:        "2026-01-15",
		GatewayEchoesAPIVersion: true,
		Now:                     now,
	}
}

func asWebhook(s *l6response.Subject) {
	s.Signature = l6response.Signature{
		InboundWebhook: true,
		HeaderPresent:  true,
		Verified:       true,
		Timestamp:      s.Now.Add(-30 * time.Second),
		EventID:        "evt_1",
	}
}

func TestL6Rules(t *testing.T) {
	t.Parallel()
	set := l6response.Rules(deps())

	ruletest.Run(t, set, base, []ruletest.Case[l6response.Subject]{
		{
			ID:   "L6.SIGNATURE_PRESENT",
			Pass: asWebhook,
			Fail: func(s *l6response.Subject) {
				asWebhook(s)
				s.Signature.HeaderPresent = false
			},
		},
		{
			ID:   "L6.SIGNATURE_VERIFIES",
			Pass: asWebhook,
			Fail: func(s *l6response.Subject) {
				asWebhook(s)
				s.Signature.Verified = false
			},
		},
		{
			ID:   "L6.SIGNATURE_TIMESTAMP_WITHIN_SKEW",
			Pass: asWebhook,
			Fail: func(s *l6response.Subject) {
				asWebhook(s)
				s.Signature.Timestamp = s.Now.Add(-10 * time.Minute)
			},
		},
		{
			ID:   "L6.SIGNATURE_NONCE_NOT_REPLAYED",
			Pass: asWebhook,
			Fail: func(s *l6response.Subject) {
				asWebhook(s)
				s.Signature.NonceSeen = true
			},
		},
		{
			ID:   "L6.RESPONSE_IS_WELL_FORMED",
			Pass: func(s *l6response.Subject) {},
			Fail: func(s *l6response.Subject) {
				s.Normalized.Parsed = false
				s.Raw = []byte(`<html>502 Bad Gateway</html>`)
			},
		},
		{
			ID:   "L6.RESPONSE_MATCHES_ADAPTER_SCHEMA",
			Pass: func(s *l6response.Subject) {},
			Fail: func(s *l6response.Subject) {
				s.Normalized.SchemaComplete = false
				s.Normalized.MissingFields = []string{"amount_captured"}
			},
		},
		{
			ID:   "L6.RESPONSE_API_VERSION_MATCHES_PINNED",
			Pass: func(s *l6response.Subject) {},
			Fail: func(s *l6response.Subject) { s.Normalized.APIVersion = "2025-09-01" },
		},
		{
			ID:   "L6.RESPONSE_HAS_TRANSACTION_ID",
			Pass: func(s *l6response.Subject) {},
			Fail: func(s *l6response.Subject) { s.Normalized.TransactionID = "" },
		},
		{
			ID: "L6.TRANSACTION_ID_STABLE_ACROSS_RETRIES",
			Pass: func(s *l6response.Subject) {
				s.Attempt.WasRetried = true
				s.Attempt.PreviousTransactionID = "ch_1"
			},
			Fail: func(s *l6response.Subject) {
				s.Attempt.WasRetried = true
				s.Attempt.PreviousTransactionID = "ch_0"
			},
		},
		{
			ID:   "L6.RESPONSE_CORRELATES_TO_ATTEMPT",
			Pass: func(s *l6response.Subject) {},
			Fail: func(s *l6response.Subject) { s.Normalized.EchoedIdempotencyKey = "gwidem_other" },
		},
		{
			ID:   "L6.AMOUNT_ECHO_MATCHES",
			Pass: func(s *l6response.Subject) {},
			Fail: func(s *l6response.Subject) { s.Normalized.EchoedAmount = eur(99_00) },
		},
		{
			ID:   "L6.CURRENCY_ECHO_MATCHES",
			Pass: func(s *l6response.Subject) {},
			Fail: func(s *l6response.Subject) { s.Normalized.EchoedCurrency = "USD" },
		},
		{
			ID:   "L6.CAPTURED_NOT_ABOVE_AUTHORIZED",
			Pass: func(s *l6response.Subject) {},
			Fail: func(s *l6response.Subject) { s.Normalized.CapturedTotal = eur(100_01) },
		},
		{
			ID: "L6.REFUNDED_NOT_ABOVE_CAPTURED",
			Pass: func(s *l6response.Subject) {
				s.Attempt.Operation = shared.OpRefund
				s.Payment.CapturedTotal = eur(100_00)
				s.Normalized.RefundedTotal = eur(100_00)
			},
			Fail: func(s *l6response.Subject) {
				s.Attempt.Operation = shared.OpRefund
				s.Payment.CapturedTotal = eur(100_00)
				s.Normalized.RefundedTotal = eur(100_01)
			},
		},
		{
			ID:   "L6.STATUS_IS_MAPPABLE",
			Pass: func(s *l6response.Subject) {},
			Fail: func(s *l6response.Subject) {
				s.Normalized.StatusMappable = false
				s.Normalized.GatewayStatus = "partially_settled_pending_review"
			},
		},
		{
			ID: "L6.DECLINE_REASON_IS_MAPPABLE",
			Pass: func(s *l6response.Subject) {
				declined(s, payment.DeclineInsufficientFunds, "insufficient_funds")
			},
			Fail: func(s *l6response.Subject) {
				declined(s, payment.DeclineUnknown, "issuer_said_no_reason_47")
			},
		},
		{
			ID: "L6.DECLINE_CLASS_IS_KNOWN",
			Pass: func(s *l6response.Subject) {
				declined(s, payment.DeclineIssuerUnavailable, "issuer_unavailable")
			},
			Fail: func(s *l6response.Subject) {
				declined(s, "NEW_REASON_NOBODY_MAPPED", "issuer_said_no_reason_47")
			},
		},
		{
			ID: "L6.THREE_DS_ACTION_HAS_PAYLOAD",
			Pass: func(s *l6response.Subject) {
				s.Normalized.Outcome = l6response.OutcomeRequiresAction
				s.Normalized.RedirectURL = "https://acs.issuer.example/challenge/1"
				s.Normalized.ResumeReference = "res_1"
			},
			Fail: func(s *l6response.Subject) {
				s.Normalized.Outcome = l6response.OutcomeRequiresAction
			},
		},
		{
			ID:   "L6.STATE_IS_REACHABLE_FROM_CURRENT",
			Pass: func(s *l6response.Subject) {},
			Fail: func(s *l6response.Subject) { s.Normalized.MappedState = payment.StateRefunded },
		},
		{
			ID: "L6.NO_SUCCESS_AFTER_TERMINAL_FAILURE",
			Pass: func(s *l6response.Subject) {
				s.Payment.Current = payment.StateFailed
				s.Normalized.Outcome = l6response.OutcomeDeclined
			},
			Fail: func(s *l6response.Subject) {
				s.Payment.Current = payment.StateFailed
			},
		},
		{
			ID: "L6.SETTLEMENT_FIELDS_PRESENT",
			Pass: func(s *l6response.Subject) {
				s.Kind = l6response.KindSettlement
				s.Normalized.Settlement = l6response.SettlementFields{
					SettlementDate:  s.Now.AddDate(0, 0, -1),
					NetAmount:       eur(95_00),
					Fee:             eur(5_00),
					PayoutReference: "po_1",
				}
			},
			Fail: func(s *l6response.Subject) {
				s.Kind = l6response.KindSettlement
				s.Normalized.Settlement = l6response.SettlementFields{
					SettlementDate: s.Now.AddDate(0, 0, -1),
					NetAmount:      eur(95_00),
					Fee:            eur(5_00),
				}
			},
		},
		{
			ID: "L6.DISPUTE_FIELDS_PRESENT",
			Pass: func(s *l6response.Subject) {
				s.Kind = l6response.KindDispute
				s.Normalized.Dispute = l6response.DisputeFields{
					DisputeID:        "dp_1",
					ReasonCode:       "10.4",
					Amount:           eur(100_00),
					EvidenceDeadline: s.Now.AddDate(0, 0, 20),
				}
			},
			Fail: func(s *l6response.Subject) {
				s.Kind = l6response.KindDispute
				s.Normalized.Dispute = l6response.DisputeFields{
					DisputeID:  "dp_1",
					ReasonCode: "10.4",
					Amount:     eur(100_00),
				}
			},
		},
	})
}

// TestL6ShortCircuitStopsBeforeParsingAnUnverifiedWebhook is the ordering property: an
// unverified body must not be interpreted, because parsing is the first thing that acts on
// bytes an attacker chose.
func TestL6ShortCircuitStopsBeforeParsingAnUnverifiedWebhook(t *testing.T) {
	t.Parallel()
	s := base()
	asWebhook(&s)
	s.Signature.Verified = false
	s.Normalized.SchemaComplete = false // would fail, must never be reached

	rep := l6response.Rules(deps()).Evaluate(t.Context(), s)

	if rep.OK() {
		t.Fatal("an unverified webhook passed L6")
	}
	if _, ran := rep.For("L6.RESPONSE_MATCHES_ADAPTER_SCHEMA"); ran {
		t.Fatal("a schema rule ran on a webhook whose signature did not verify")
	}
}

// TestL6DeclineWithoutAClassIsRejected: failing over on an unclassified decline is
// indistinguishable from card testing, so an absent class must be an error, not a default.
func TestL6DeclineWithoutAClassIsRejected(t *testing.T) {
	t.Parallel()
	s := base()
	declined(&s, "", "some_new_code")

	rule, ok := l6response.Rules(deps()).Rule("L6.DECLINE_CLASS_IS_KNOWN")
	if !ok {
		t.Fatal("the decline-class rule is not in the L6 set")
	}
	if out := rule.Evaluate(t.Context(), s); out.Passed {
		t.Fatal("a decline with no normalized reason was accepted")
	}
}

func declined(s *l6response.Subject, reason payment.DeclineReason, raw string) {
	s.Normalized.Outcome = l6response.OutcomeDeclined
	s.Normalized.DeclineReason = reason
	s.Normalized.DeclineReasonRaw = raw
	s.Normalized.MappedState = payment.StateFailed
}
