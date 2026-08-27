package simulator

import (
	"strings"

	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Scenario names a behaviour the simulator can be made to exhibit.
//
// The set is the list of things that actually go wrong with real gateways, rather than the list of
// things that are easy to fake. Every one of these has cost somebody money at some point: a gateway
// that echoes a different amount, a gateway that sends the same webhook twice, a gateway that takes
// forty seconds to answer a request it already processed.
type Scenario string

const (
	// ScenarioApprove authorizes or captures normally.
	ScenarioApprove Scenario = "approve"
	// ScenarioDeclineInsufficientFunds is a hard decline: no failover.
	ScenarioDeclineInsufficientFunds Scenario = "decline_insufficient_funds"
	// ScenarioDeclineDoNotHonor is a soft decline: failover is legitimate.
	ScenarioDeclineDoNotHonor Scenario = "decline_do_not_honor"
	// ScenarioDeclineStolenCard is a hard decline that also carries scheme do-not-retry advice.
	ScenarioDeclineStolenCard Scenario = "decline_stolen_card"
	// ScenarioDeclineUnmapped returns a vendor code the adapter has never been taught, which must
	// normalize to DeclineUnknown and must not permit failover.
	ScenarioDeclineUnmapped Scenario = "decline_unmapped"
	// ScenarioRequiresAction returns a 3-D Secure challenge with a redirect URL.
	ScenarioRequiresAction Scenario = "requires_action"
	// ScenarioPending accepts the instruction with an asynchronous outcome.
	ScenarioPending Scenario = "pending"
	// ScenarioTimeout writes the request and never answers. In-process this surfaces as
	// spi.ErrOutcomeUnknown; over HTTP the server holds the connection until the client's deadline
	// expires, which is the same failure a real gateway produces.
	ScenarioTimeout Scenario = "timeout"
	// ScenarioMalformed answers 200 with a body that is not valid JSON — the failure that proves an
	// adapter treats an unreadable answer as an unknown outcome rather than as a failure.
	ScenarioMalformed Scenario = "malformed"
	// ScenarioAmountMismatch echoes a different amount and currency from the one requested, which
	// the adapter must surface rather than absorb.
	ScenarioAmountMismatch Scenario = "amount_mismatch"
	// ScenarioDuplicateWebhook emits the same webhook twice with an identical payload, so the
	// platform's deduplication is exercised against a byte-identical repeat rather than a
	// conveniently different one.
	ScenarioDuplicateWebhook Scenario = "duplicate_webhook"
	// ScenarioSlow answers correctly after a configured delay, for latency-budget and bulkhead
	// tests that must not also be testing a failure path.
	ScenarioSlow Scenario = "slow"
	// ScenarioGatewayError answers 500, which on a money-moving call must become an unknown outcome
	// because no gateway guarantees a 500 means nothing happened.
	ScenarioGatewayError Scenario = "gateway_error"
	// ScenarioAuthFailure answers 401, which must map to ErrCredentialsInvalid and page rather than
	// failing the payment quietly.
	ScenarioAuthFailure Scenario = "auth_failure"
)

// ScenarioMetadataKey is the metadata field that overrides the amount-derived trigger.
//
// An explicit override exists because the amount trigger, while convenient, couples a test's
// *scenario* to its *amount* — and some tests need a specific amount for an unrelated reason (a
// currency's minimum, a partial-capture boundary). Where both are present the metadata wins, so a
// test can always say what it means.
const ScenarioMetadataKey = "pp_sim_scenario"

// amountTriggers is the documented trigger table.
//
// The trigger is the last two digits of the amount in **minor units**, so it works identically for
// a two-decimal currency and for JPY. Using the low digits rather than the high ones means a test
// can pick any order value it likes and still select a behaviour, and it means the same trigger
// works across currencies without a conversion table.
//
//	minor units % 100  behaviour
//	-----------------  -----------------------------------------------------------
//	00                 approve
//	01                 decline, INSUFFICIENT_FUNDS (hard, no failover)
//	02                 decline, DO_NOT_HONOR (soft, failover permitted)
//	03                 decline, STOLEN_CARD (hard, scheme says do not retry)
//	04                 decline with an unmapped vendor code (→ UNKNOWN, no failover)
//	05                 requires action (3-D Secure challenge, redirect URL supplied)
//	06                 pending (asynchronous outcome)
//	07                 timeout (no answer; unknown outcome)
//	08                 malformed response body
//	09                 amount/currency echo mismatch
//	10                 duplicate webhook emission
//	11                 slow but correct
//	12                 HTTP 500
//	13                 HTTP 401 (credentials rejected)
//	anything else      approve
var amountTriggers = map[int64]Scenario{
	0:  ScenarioApprove,
	1:  ScenarioDeclineInsufficientFunds,
	2:  ScenarioDeclineDoNotHonor,
	3:  ScenarioDeclineStolenCard,
	4:  ScenarioDeclineUnmapped,
	5:  ScenarioRequiresAction,
	6:  ScenarioPending,
	7:  ScenarioTimeout,
	8:  ScenarioMalformed,
	9:  ScenarioAmountMismatch,
	10: ScenarioDuplicateWebhook,
	11: ScenarioSlow,
	12: ScenarioGatewayError,
	13: ScenarioAuthFailure,
}

// ScenarioForAmount returns the scenario an amount triggers.
func ScenarioForAmount(m money.Money) Scenario {
	v := m.Amount() % 100
	if v < 0 {
		v = -v
	}
	if s, ok := amountTriggers[v]; ok {
		return s
	}
	return ScenarioApprove
}

// ResolveScenario picks the behaviour for a request: the metadata override if present and
// recognised, otherwise the amount trigger.
//
// An *unrecognised* override falls back to the amount trigger rather than erroring. A test that
// misspells a scenario name should get a clear assertion failure about the outcome, not a
// simulator-level error that looks like a transport problem and sends the reader hunting in the
// wrong place.
func ResolveScenario(metadata map[string]string, m money.Money) Scenario {
	if raw, ok := metadata[ScenarioMetadataKey]; ok {
		s := Scenario(strings.TrimSpace(strings.ToLower(raw)))
		if IsKnownScenario(s) {
			return s
		}
	}
	return ScenarioForAmount(m)
}

// IsKnownScenario reports whether s is one the simulator implements.
func IsKnownScenario(s Scenario) bool {
	switch s {
	case ScenarioApprove, ScenarioDeclineInsufficientFunds, ScenarioDeclineDoNotHonor,
		ScenarioDeclineStolenCard, ScenarioDeclineUnmapped, ScenarioRequiresAction,
		ScenarioPending, ScenarioTimeout, ScenarioMalformed, ScenarioAmountMismatch,
		ScenarioDuplicateWebhook, ScenarioSlow, ScenarioGatewayError, ScenarioAuthFailure:
		return true
	default:
		return false
	}
}

// AllScenarios returns every implemented scenario, for a test that wants to sweep them.
func AllScenarios() []Scenario {
	return []Scenario{
		ScenarioApprove, ScenarioDeclineInsufficientFunds, ScenarioDeclineDoNotHonor,
		ScenarioDeclineStolenCard, ScenarioDeclineUnmapped, ScenarioRequiresAction,
		ScenarioPending, ScenarioTimeout, ScenarioMalformed, ScenarioAmountMismatch,
		ScenarioDuplicateWebhook, ScenarioSlow, ScenarioGatewayError, ScenarioAuthFailure,
	}
}

// declineCodeFor returns the simulator's vendor-level decline code for a scenario.
//
// The unmapped scenario returns a code that is deliberately absent from the adapter's table, so the
// "unmapped declines are UNKNOWN and do not fail over" assertion is exercised against a genuinely
// unknown code rather than against a special case the adapter recognises as "the test one".
func declineCodeFor(s Scenario) (code, message string) {
	switch s {
	case ScenarioDeclineInsufficientFunds:
		return "insufficient_funds", "the card has insufficient funds"
	case ScenarioDeclineDoNotHonor:
		return "do_not_honor", "the issuer declined without a reason"
	case ScenarioDeclineStolenCard:
		return "stolen_card", "the card has been reported stolen"
	case ScenarioDeclineUnmapped:
		return "sim_reason_the_adapter_has_never_seen", "an unmapped refusal"
	default:
		return "", ""
	}
}
