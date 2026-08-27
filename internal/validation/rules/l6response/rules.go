package l6response

import (
	"encoding/json"
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/validation/engine"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/internal/ruledef"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

func init() {
	ruledef.Register(defs(DefaultDeps()), "payments-core", "2026-01-01", engine.Enforce)
}

// Rules returns the L6 rule set.
//
// ShortCircuit, in the order signature → parse → schema → correlation → echo → status →
// transition. Each group presupposes the one before it, and the first group presupposes the
// rest not running at all: an unverified webhook body must not be parsed, because parsing is
// the first thing that acts on attacker-controlled bytes.
func Rules(d Deps) engine.RuleSet[Subject] {
	return engine.RuleSet[Subject]{
		Name:  "L6.response",
		Mode:  engine.ShortCircuit,
		Rules: ruledef.Build(defs(d)),
	}
}

func defs(d Deps) []ruledef.Def[Subject] {
	webhook := func(s Subject) bool { return s.Signature.InboundWebhook }
	parsed := func(s Subject) bool { return s.Normalized.Parsed }
	success := func(s Subject) bool { return s.Normalized.Outcome == OutcomeSuccess }
	declined := func(s Subject) bool { return s.Normalized.Outcome == OutcomeDeclined }
	contractViolation := string(apierror.CodeGatewayContractViolation)

	return []ruledef.Def[Subject]{
		{
			ID: "L6.SIGNATURE_PRESENT", Severity: engine.Error,
			Code: string(apierror.CodeWebhookSignatureInvalid), Field: "signature", Pure: true,
			Desc:        "the gateway's signature header is present on an inbound webhook",
			Remediation: "Reject with 401 and raise a security event; verify the endpoint's registered secret.",
			Applies:     webhook,
			Check: func(s Subject) string {
				if s.Signature.HeaderPresent {
					return ""
				}
				return "the inbound webhook carries no signature header"
			},
		},
		{
			ID: "L6.SIGNATURE_VERIFIES", Severity: engine.Error,
			Code: string(apierror.CodeWebhookSignatureInvalid), Field: "signature", Pure: true,
			Desc:        "constant-time signature verification over the raw body succeeded",
			Remediation: "Reject with 401, raise a security event, and do not parse the body further.",
			Applies:     func(s Subject) bool { return s.Signature.InboundWebhook && s.Signature.HeaderPresent },
			Check: func(s Subject) string {
				if s.Signature.Verified {
					return ""
				}
				// Verification is over the raw bytes, not the re-serialized document: any
				// normalization between receipt and verification is a signature bypass.
				return "the webhook signature did not verify over the raw body"
			},
		},
		{
			ID: "L6.SIGNATURE_TIMESTAMP_WITHIN_SKEW", Severity: engine.Error,
			Code: string(apierror.CodeWebhookReplayDetected), Field: "signature", Pure: true,
			Desc:        "the signed timestamp is within the accepted clock skew",
			Remediation: "Reject with 401. If this spikes, check NTP on the ingress hosts.",
			Applies: func(s Subject) bool {
				return s.Signature.InboundWebhook && !s.Signature.Timestamp.IsZero()
			},
			Check: func(s Subject) string {
				skew := d.SignatureSkew
				if skew == 0 {
					skew = DefaultDeps().SignatureSkew
				}
				delta := s.Now.Sub(s.Signature.Timestamp)
				if delta < 0 {
					delta = -delta
				}
				if delta <= skew {
					return ""
				}
				return "the signed timestamp is " + itoa64(int64(delta.Seconds())) +
					"s from now; the tolerance is " + itoa64(int64(skew.Seconds())) + "s"
			},
		},
		{
			ID: "L6.SIGNATURE_NONCE_NOT_REPLAYED", Severity: engine.Warning,
			Code: string(apierror.CodeWebhookReplayDetected), Field: "signature", Pure: false,
			Desc:        "the (gateway, event id) pair is not already in the dedup table",
			Remediation: "Drop the duplicate silently and increment the duplicate-webhook counter; this is not an error.",
			Applies: func(s Subject) bool {
				return s.Signature.InboundWebhook && s.Signature.EventID != ""
			},
			Check: func(s Subject) string {
				// A duplicate webhook is normal: gateways retry, and at-least-once delivery is
				// the contract. Treating it as an error would page somebody nightly.
				if !s.Signature.NonceSeen {
					return ""
				}
				return "this webhook event has already been processed"
			},
		},
		{
			ID: "L6.RESPONSE_IS_WELL_FORMED", Severity: engine.Error,
			Code: contractViolation, Field: "body", Pure: true,
			Desc:        "the response body parses in the adapter's declared media type",
			Remediation: "Classify the attempt as ERROR and retry it; raise an adapter defect if this is sustained.",
			Check: func(s Subject) string {
				if s.Normalized.Parsed {
					return ""
				}
				if len(s.Raw) > 0 && json.Valid(s.Raw) {
					// The adapter said it could not parse a document that is syntactically
					// valid JSON, which is an adapter defect rather than a gateway one.
					return "the adapter could not interpret a syntactically valid response body"
				}
				return "the gateway response body did not parse"
			},
		},
		{
			ID: "L6.RESPONSE_MATCHES_ADAPTER_SCHEMA", Severity: engine.Error,
			Code: contractViolation, Field: "body", Pure: true,
			Desc:        "the operation's required fields are present and correctly typed",
			Remediation: "Classify the attempt as ERROR and alert: the gateway has changed its contract.",
			Applies:     parsed,
			Check: func(s Subject) string {
				if s.Normalized.SchemaComplete && len(s.Normalized.MissingFields) == 0 {
					return ""
				}
				if len(s.Normalized.MissingFields) > 0 {
					return "the response is missing: " + strings.Join(s.Normalized.MissingFields, ", ")
				}
				return "the response does not match the adapter's schema for this operation"
			},
		},
		{
			ID: "L6.RESPONSE_API_VERSION_MATCHES_PINNED", Severity: engine.Warning,
			Code: "", Field: "headers", Pure: true,
			Desc:        "the echoed API version equals the version pinned on the connection",
			Remediation: "Record the discrepancy: a silent gateway version change is an incident precursor.",
			Applies: func(s Subject) bool {
				return s.GatewayEchoesAPIVersion && s.PinnedAPIVersion != ""
			},
			Check: func(s Subject) string {
				if s.Normalized.APIVersion == s.PinnedAPIVersion {
					return ""
				}
				return "the gateway answered with API version " + quote(s.Normalized.APIVersion) +
					" against a pinned " + quote(s.PinnedAPIVersion)
			},
		},
		{
			ID: "L6.RESPONSE_HAS_TRANSACTION_ID", Severity: engine.Error,
			Code: contractViolation, Field: "/transactionId", Pure: true,
			Desc:        "a non-empty gateway transaction reference is present",
			Remediation: "Classify the attempt as TIMEOUT_UNKNOWN: without an identifier the outcome cannot be looked up later.",
			Applies: func(s Subject) bool {
				return s.Normalized.Parsed && s.Normalized.Outcome != OutcomeHardError
			},
			Check: func(s Subject) string {
				if s.Normalized.TransactionID != "" {
					return ""
				}
				return "the gateway returned no transaction reference"
			},
		},
		{
			ID: "L6.TRANSACTION_ID_STABLE_ACROSS_RETRIES", Severity: engine.Error,
			Code: contractViolation, Field: "/transactionId", Pure: true,
			Desc:        "a transport retry of the same attempt returned the same transaction reference",
			Remediation: "Two authorizations may exist. Emit payment.reconciliation_required.v1 and page: gateway idempotency did not hold.",
			Applies: func(s Subject) bool {
				return s.Attempt.WasRetried && s.Attempt.PreviousTransactionID != "" &&
					s.Normalized.TransactionID != ""
			},
			Check: func(s Subject) string {
				if s.Normalized.TransactionID == s.Attempt.PreviousTransactionID {
					return ""
				}
				// A different ID on a retry of the same idempotency key means the gateway
				// created a second transaction. Money may have moved twice.
				return "the retry returned a different transaction reference from the original call"
			},
		},
		{
			ID: "L6.RESPONSE_CORRELATES_TO_ATTEMPT", Severity: engine.Error,
			Code: contractViolation, Field: "/idempotencyKey", Pure: true,
			Desc:        "the echoed idempotency key equals this attempt's gateway idempotency key",
			Remediation: "Discard the response, classify TIMEOUT_UNKNOWN and reconcile: a mismatch means a crossed response.",
			Applies: func(s Subject) bool {
				return s.Attempt.GatewayEchoesIdempotencyKey && s.Attempt.GatewayIdempotencyKey != ""
			},
			Check: func(s Subject) string {
				if s.Normalized.EchoedIdempotencyKey == s.Attempt.GatewayIdempotencyKey {
					return ""
				}
				return "the response echoes a different idempotency key from the one dispatched"
			},
		},
		{
			ID: "L6.AMOUNT_ECHO_MATCHES", Severity: engine.Error,
			Code: contractViolation, Field: "/amount", Pure: true,
			Desc:        "the echoed amount equals the dispatched amount exactly, in minor units",
			Remediation: "Do not transition the payment. Emit payment.reconciliation_required.v1 and page.",
			Applies:     success,
			Check: func(s Subject) string {
				if s.Normalized.EchoedAmount.Amount() == s.Attempt.DispatchedAmount.Amount() {
					return ""
				}
				return "the gateway echoed " + itoa64(s.Normalized.EchoedAmount.Amount()) +
					" minor units against a dispatched " + itoa64(s.Attempt.DispatchedAmount.Amount())
			},
		},
		{
			ID: "L6.CURRENCY_ECHO_MATCHES", Severity: engine.Error,
			Code: contractViolation, Field: "/currency", Pure: true,
			Desc:        "the echoed currency equals the dispatched currency",
			Remediation: "Do not transition the payment. Emit payment.reconciliation_required.v1 and page; a currency mismatch is never recoverable automatically.",
			Applies:     success,
			Check: func(s Subject) string {
				if s.Normalized.EchoedCurrency == s.Attempt.DispatchedAmount.Currency() {
					return ""
				}
				return "the gateway echoed " + quote(string(s.Normalized.EchoedCurrency)) +
					" against a dispatched " + quote(string(s.Attempt.DispatchedAmount.Currency()))
			},
		},
		{
			ID: "L6.CAPTURED_NOT_ABOVE_AUTHORIZED", Severity: engine.Error,
			Code: contractViolation, Field: "/capturedTotal", Pure: true,
			Desc:        "the echoed captured total is within the authorized amount (invariant I2)",
			Remediation: "Do not transition. Open a reconciliation exception at CRITICAL severity.",
			Applies: func(s Subject) bool {
				return s.Attempt.Operation == captureOp && s.Payment.Present
			},
			Check: func(s Subject) string {
				over, err := s.Normalized.CapturedTotal.GreaterThan(s.Payment.AuthorizedAmount)
				if err != nil {
					return "the echoed captured total is in a different currency from the authorization"
				}
				if over {
					return "the gateway reports a captured total above the authorized amount"
				}
				return ""
			},
		},
		{
			ID: "L6.REFUNDED_NOT_ABOVE_CAPTURED", Severity: engine.Error,
			Code: contractViolation, Field: "/refundedTotal", Pure: true,
			Desc:        "the echoed refunded total is within the captured total (invariant I1)",
			Remediation: "Do not transition. Open a reconciliation exception at CRITICAL severity.",
			Applies: func(s Subject) bool {
				return s.Attempt.Operation == refundOp && s.Payment.Present
			},
			Check: func(s Subject) string {
				over, err := s.Normalized.RefundedTotal.GreaterThan(s.Payment.CapturedTotal)
				if err != nil {
					return "the echoed refunded total is in a different currency from the capture"
				}
				if over {
					return "the gateway reports a refunded total above the captured total"
				}
				return ""
			},
		},
		{
			ID: "L6.STATUS_IS_MAPPABLE", Severity: engine.Error,
			Code: contractViolation, Field: "/status", Pure: true,
			Desc: "the gateway status maps to exactly one domain outcome, and that outcome is one " +
				"the adapter may return for this operation",
			Remediation: "Classify the attempt as TIMEOUT_UNKNOWN; never guess. Add the mapping and re-drive from the raw record.",
			Applies:     parsed,
			Check: func(s Subject) string {
				if !s.Normalized.StatusMappable || s.Normalized.Outcome == OutcomeUnknown {
					return "gateway status " + quote(s.Normalized.GatewayStatus) +
						" does not map to a domain outcome"
				}
				allowed, ok := d.AllowedOutcomes[s.Attempt.Operation]
				if !ok {
					return ""
				}
				for _, o := range allowed {
					if o == s.Normalized.Outcome {
						return ""
					}
				}
				return string(s.Normalized.Outcome) + " is not an outcome the adapter may return for " +
					string(s.Attempt.Operation)
			},
		},
		{
			ID: "L6.DECLINE_REASON_IS_MAPPABLE", Severity: engine.Warning,
			Code: string(apierror.CodeGatewayDeclined), Field: "/declineReason", Pure: true,
			Desc:        "the gateway's reason code maps to a normalized decline reason",
			Remediation: "Map to UNKNOWN_DECLINE, treat it as a hard decline, and alert: never fail over on an unmapped reason.",
			Applies:     declined,
			Check: func(s Subject) string {
				if s.Normalized.DeclineReason != "" && s.Normalized.DeclineReason != payment.DeclineUnknown {
					return ""
				}
				return "decline reason " + quote(s.Normalized.DeclineReasonRaw) +
					" does not map to a normalized reason"
			},
		},
		{
			ID: "L6.DECLINE_CLASS_IS_KNOWN", Severity: engine.Error,
			Code: string(apierror.CodeGatewayDeclined), Field: "/declineReason", Pure: true,
			Desc:        "the mapped decline reason carries an explicit HARD or SOFT class",
			Remediation: "Treat an unclassified decline as HARD: failing over on one is card-testing behaviour.",
			Applies:     declined,
			Check: func(s Subject) string {
				for _, known := range d.KnownDeclineReasons {
					if known == s.Normalized.DeclineReason {
						return ""
					}
				}
				return "decline reason " + quote(string(s.Normalized.DeclineReason)) +
					" carries no hard/soft classification"
			},
		},
		{
			ID: "L6.THREE_DS_ACTION_HAS_PAYLOAD", Severity: engine.Error,
			Code: contractViolation, Field: "/nextAction", Pure: true,
			Desc:        "a requires-action outcome carries a redirect or challenge payload and a resume reference",
			Remediation: "Classify the attempt as ERROR: the customer cannot complete authentication without it.",
			Applies:     func(s Subject) bool { return s.Normalized.Outcome == OutcomeRequiresAction },
			Check: func(s Subject) string {
				if s.Normalized.RedirectURL == "" && s.Normalized.ChallengePayload == "" {
					return "the gateway requires customer action but returned no redirect or challenge"
				}
				if s.Normalized.ResumeReference == "" {
					return "the gateway returned a challenge with no resume reference"
				}
				return ""
			},
		},
		{
			ID: "L6.STATE_IS_REACHABLE_FROM_CURRENT", Severity: engine.Error,
			Code: string(apierror.CodeInvalidStateTransition), Field: "/status", Pure: true,
			Desc:        "the mapped state is an allowed successor of the payment's current state",
			Remediation: "Do not apply the response. Park it in the exception queue: this is usually a late or out-of-order webhook.",
			Applies: func(s Subject) bool {
				return s.Payment.Present && s.Normalized.MappedState != "" &&
					s.Normalized.MappedState != s.Payment.Current
			},
			Check: func(s Subject) string {
				// The transition table lives in the payment domain and is asked, never copied:
				// a second copy here would be a second definition of what the platform permits.
				if payment.Machine().CanTransition(s.Payment.Current, s.Normalized.MappedState) {
					return ""
				}
				return "the response maps to " + string(s.Normalized.MappedState) +
					", which is not reachable from " + string(s.Payment.Current)
			},
		},
		{
			ID: "L6.NO_SUCCESS_AFTER_TERMINAL_FAILURE", Severity: engine.Error,
			Code: string(apierror.CodeInvalidStateTransition), Field: "/status", Pure: true,
			Desc:        "a success response is rejected for a payment already in a terminal failure state",
			Remediation: "Money may have moved on a payment the client was told had failed. Open a CRITICAL reconciliation exception and page.",
			Applies: func(s Subject) bool {
				return s.Payment.Present && s.Payment.Current.IsTerminal() &&
					!s.Payment.Current.IsSuccessful()
			},
			Check: func(s Subject) string {
				if s.Normalized.Outcome != OutcomeSuccess {
					return ""
				}
				return "the gateway reports success on a payment that is terminal in state " +
					string(s.Payment.Current)
			},
		},
		{
			ID: "L6.SETTLEMENT_FIELDS_PRESENT", Severity: engine.Error,
			Code: contractViolation, Field: "/settlement", Pure: true,
			Desc:        "a settlement record carries a date, a net amount, a fee and a payout reference",
			Remediation: "Park the settlement record: the ledger cannot be balanced without the fee.",
			Applies:     func(s Subject) bool { return s.Kind == KindSettlement },
			Check: func(s Subject) string {
				f := s.Normalized.Settlement
				switch {
				case f.SettlementDate.IsZero():
					return "the settlement record carries no settlement date"
				case f.NetAmount.IsZero() && f.NetAmount.Currency() == "":
					return "the settlement record carries no net amount"
				case f.Fee.Currency() == "":
					return "the settlement record carries no fee"
				case f.PayoutReference == "":
					return "the settlement record carries no payout reference"
				}
				return ""
			},
		},
		{
			ID: "L6.DISPUTE_FIELDS_PRESENT", Severity: engine.Error,
			Code: contractViolation, Field: "/dispute", Pure: true,
			Desc:        "a dispute record carries an identifier, a reason code, an amount and an evidence deadline",
			Remediation: "Park the dispute record: a dispute without a deadline cannot be worked.",
			Applies:     func(s Subject) bool { return s.Kind == KindDispute },
			Check: func(s Subject) string {
				f := s.Normalized.Dispute
				switch {
				case f.DisputeID == "":
					return "the dispute record carries no dispute identifier"
				case f.ReasonCode == "":
					return "the dispute record carries no reason code"
				case f.Amount.Currency() == "":
					return "the dispute record carries no amount"
				case f.EvidenceDeadline.IsZero():
					return "the dispute record carries no evidence deadline"
				}
				return ""
			},
		},
	}
}

// captureOp and refundOp are the gateway operations whose invariant echoes L6 checks. Aliased
// so the rule table reads as the invariant rather than as a package-qualified constant.
const (
	captureOp = shared.OpCapture
	refundOp  = shared.OpRefund
)

func quote(s string) string {
	if s == "" {
		return "(empty)"
	}
	return "`" + s + "`"
}

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
