// Package l6response is validation level 6: what the platform asserts about a gateway's answer
// before that answer is allowed to change a payment's state.
//
// The premise of this level is that a gateway response is untrusted input. Not malicious,
// usually — but a gateway that echoes a different amount, returns a status the adapter has
// never seen, or reuses a transaction ID across a transport retry is describing a world in
// which money may already have moved differently from what we are about to record. Applying
// such a response is how a platform tells a customer their payment failed while the issuer
// tells them it succeeded.
//
// So the rules here are not "reject the payment". A failed L6 rule never fails a payment: it
// classifies the attempt as ERROR or TIMEOUT_UNKNOWN and, where money may have moved, raises
// `payment.reconciliation_required.v1`. The distinction matters because a contract violation
// is a statement about our knowledge, not about the customer's funds.
//
// Mode is ShortCircuit, and here it is about correctness rather than cost: STATUS_IS_MAPPABLE
// must pass before STATE_IS_REACHABLE_FROM_CURRENT has a status to reason about, and the
// signature family must pass before the body is parsed at all.
//
// See docs/validation-plane.md §3.6.
package l6response

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// EventKind distinguishes a synchronous operation response from the two asynchronous record
// types that arrive by webhook and have their own required fields.
type EventKind string

// The event kinds L6 validates.
const (
	KindResponse   EventKind = "RESPONSE"
	KindSettlement EventKind = "SETTLEMENT"
	KindDispute    EventKind = "DISPUTE"
)

// Outcome is the adapter's normalized classification of the gateway's answer.
type Outcome string

// The normalized outcomes.
const (
	OutcomeUnknown        Outcome = ""
	OutcomeSuccess        Outcome = "SUCCESS"
	OutcomeDeclined       Outcome = "DECLINED"
	OutcomeRequiresAction Outcome = "REQUIRES_ACTION"
	OutcomePending        Outcome = "PENDING"
	OutcomeHardError      Outcome = "ERROR"
)

// SettlementFields are the ledger-relevant fields of a settlement record.
type SettlementFields struct {
	SettlementDate  time.Time
	NetAmount       money.Money
	Fee             money.Money
	PayoutReference string
}

// DisputeFields are the workable fields of a dispute record. A dispute without an evidence
// deadline cannot be worked, which is why the deadline is required rather than optional.
type DisputeFields struct {
	DisputeID        string
	ReasonCode       string
	Amount           money.Money
	EvidenceDeadline time.Time
}

// Normalized is the adapter's translation of the raw response into platform terms.
//
// Everything the rules consult is here rather than in the raw body, because the adapter is the
// only component allowed to know a vendor's field names. What L6 checks is whether the
// translation is complete and self-consistent.
type Normalized struct {
	Parsed bool
	// SchemaComplete and MissingFields describe whether the operation's required fields arrived
	// with the right types.
	SchemaComplete bool
	MissingFields  []string

	APIVersion string

	// GatewayStatus is the vendor's own status string, kept for the exception queue.
	GatewayStatus string
	// StatusMappable records whether the adapter's total mapping table produced exactly one
	// domain outcome for GatewayStatus.
	StatusMappable bool
	Outcome        Outcome

	TransactionID        string
	EchoedIdempotencyKey string
	EchoedAmount         money.Money
	EchoedCurrency       money.Currency

	CapturedTotal money.Money
	RefundedTotal money.Money

	DeclineReasonRaw string
	DeclineReason    payment.DeclineReason

	// MappedState is the payment state this response implies, when the outcome maps to one.
	MappedState payment.State

	RedirectURL      string
	ChallengePayload string
	ResumeReference  string

	Settlement SettlementFields
	Dispute    DisputeFields
}

// Attempt is the dispatch this response is an answer to.
type Attempt struct {
	Operation             shared.Operation
	DispatchedAmount      money.Money
	GatewayIdempotencyKey string
	// WasRetried marks a transport-level retry of the same attempt, which is the only situation
	// in which the gateway's transaction ID must be stable.
	WasRetried            bool
	PreviousTransactionID string
	// GatewayEchoesIdempotencyKey records whether this gateway echoes the key at all; not every
	// one does, and asserting a correlation the vendor never promised would fail every response.
	GatewayEchoesIdempotencyKey bool
}

// PaymentState is the payment as it stands before this response is applied.
type PaymentState struct {
	Present          bool
	Current          payment.State
	AuthorizedAmount money.Money
	CapturedTotal    money.Money
}

// Signature is the inbound webhook's authentication material and the dedup lookup.
type Signature struct {
	// InboundWebhook marks this as a webhook rather than a synchronous response; the signature
	// family only applies to webhooks.
	InboundWebhook bool
	HeaderPresent  bool
	// Verified is the constant-time HMAC or certificate verification result, computed over the
	// raw body by the ingress before any parsing.
	Verified  bool
	Timestamp time.Time
	EventID   string
	// NonceSeen is the `(gateway, event_id)` dedup-table lookup; impure, pre-read.
	NonceSeen bool
}

// Subject is everything L6 evaluates.
type Subject struct {
	Kind             EventKind
	Attempt          Attempt
	Payment          PaymentState
	Raw              []byte
	Normalized       Normalized
	PinnedAPIVersion string
	// GatewayEchoesAPIVersion records whether this gateway returns a version header at all.
	GatewayEchoesAPIVersion bool
	Signature               Signature
	// Now is the injected clock reading.
	Now time.Time
}

// Deps carries the level's tolerances and the adapter's declared allowances. Pure data.
type Deps struct {
	// SignatureSkew is the accepted clock difference on a webhook timestamp (5 min).
	SignatureSkew time.Duration
	// AllowedOutcomes constrains, per gateway operation, which normalized outcomes an adapter
	// is permitted to return. A capture that comes back REQUIRES_ACTION is not a capture the
	// platform has a state for.
	AllowedOutcomes map[shared.Operation][]Outcome
	// KnownDeclineReasons is the normalized decline taxonomy the adapters map into.
	KnownDeclineReasons []payment.DeclineReason
}

// DefaultDeps returns the platform defaults from docs/validation-plane.md §3.6.
//
// The allowed-outcome table is the interesting part. It is an allowlist per operation because
// the failure it prevents is subtle: an adapter that returns REQUIRES_ACTION for a refund is
// describing a flow the platform has no state machine for, and the honest response is to park
// the record rather than to invent one.
func DefaultDeps() Deps {
	return Deps{
		SignatureSkew: 5 * time.Minute,
		AllowedOutcomes: map[shared.Operation][]Outcome{
			shared.OpAuthorize: {OutcomeSuccess, OutcomeDeclined, OutcomeRequiresAction, OutcomePending, OutcomeHardError},
			shared.OpCapture:   {OutcomeSuccess, OutcomeDeclined, OutcomePending, OutcomeHardError},
			shared.OpRefund:    {OutcomeSuccess, OutcomeDeclined, OutcomePending, OutcomeHardError},
			shared.OpVoid:      {OutcomeSuccess, OutcomeDeclined, OutcomeHardError},
			shared.OpLookup:    {OutcomeSuccess, OutcomeDeclined, OutcomePending, OutcomeRequiresAction, OutcomeHardError},
		},
		KnownDeclineReasons: []payment.DeclineReason{
			payment.DeclineIssuerUnavailable, payment.DeclineTryAgainLater,
			payment.DeclineProcessingError, payment.DeclineDoNotHonorSoft,
			payment.DeclineInsufficientFunds, payment.DeclineCardExpired,
			payment.DeclineIncorrectNumber, payment.DeclineIncorrectCVC,
			payment.DeclineStolenCard, payment.DeclineLostCard, payment.DeclineFraudulent,
			payment.DeclineRestrictedCard, payment.DeclineInvalidAccount,
			payment.DeclineCurrencyNotSupp, payment.DeclineAuthRequired,
			payment.DeclineBlockedByRisk,
		},
	}
}
