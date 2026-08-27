package simulator

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// declineTable maps the simulator's vendor codes to the platform's taxonomy.
//
// It is deliberately incomplete: `sim_reason_the_adapter_has_never_seen` is absent, so the
// "unmapped declines are UNKNOWN and never fail over" behaviour is exercised against a code this
// table genuinely does not contain rather than against a special case. A simulator whose mapping
// was total could not test the default arm at all.
var declineTable = map[string]payment.DeclineReason{
	"insufficient_funds": payment.DeclineInsufficientFunds,
	"do_not_honor":       payment.DeclineDoNotHonorSoft,
	"stolen_card":        payment.DeclineStolenCard,
	"lost_card":          payment.DeclineLostCard,
	"expired_card":       payment.DeclineCardExpired,
	"incorrect_cvc":      payment.DeclineIncorrectCVC,
	"processing_error":   payment.DeclineProcessingError,
	"issuer_unavailable": payment.DeclineIssuerUnavailable,
	"try_again_later":    payment.DeclineTryAgainLater,
	"fraudulent":         payment.DeclineFraudulent,
}

// mapDecline normalizes a simulator decline code. An unmapped code answers DeclineUnknown, which
// PermitsFailover reports false for.
func mapDecline(code string) payment.DeclineReason {
	if r, ok := declineTable[code]; ok {
		return r
	}
	return payment.DeclineUnknown
}

// toResult converts the wire answer into the platform's normalized result.
//
// It is shared by the in-process Engine and the HTTP Adapter, which is what guarantees the two
// cannot disagree — the property that lets a test reproduce an end-to-end failure in-process.
func toResult(resp *WireResponse, requested money.Money, idemKey string, now time.Time, latency time.Duration) (*spi.Result, error) {
	if resp == nil {
		return nil, apierror.New(apierror.CodeGatewayContractViolation,
			"simulator: the gateway returned neither a result nor an error")
	}
	status := spi.Status(resp.Status)
	if !isKnownStatus(status) {
		return nil, apierror.Newf(apierror.CodeGatewayContractViolation,
			"simulator: unrecognised status %q", resp.Status)
	}

	out := &spi.Result{
		Status:     status,
		GatewayRef: resp.Reference,
		RawStatus:  resp.RawStatus,
		RawCode:    resp.RawCode,
		RawMessage: resp.RawMessage,
		ProcessorResponse: spi.ProcessorResponse{
			AuthCode:  resp.AuthCode,
			AVSResult: resp.AVSResult,
			CVVResult: resp.CVVResult,
			Network:   "simulator",
		},
		ReceivedAt: now,
		Latency:    latency,
	}

	if status == spi.StatusDeclined {
		out.DeclineReason = mapDecline(resp.DeclineCode)
		out.NetworkAdviceNoRetry = resp.NetworkAdviceNoRetry
	}
	if resp.NextAction != nil {
		out.NextAction = &spi.NextAction{
			Type:        payment.NextActionType(resp.NextAction.Type),
			RedirectURL: resp.NextAction.RedirectURL,
			QRCodeData:  resp.NextAction.QRCodeData,
		}
	}

	// The echo check. It runs against whichever amount the answer carried, and a mismatch is
	// surfaced as an error rather than absorbed — the platform must not post a ledger entry for an
	// amount the gateway disagrees with.
	for _, candidate := range []*WireAmount{resp.AuthorizedAmount, resp.CapturedAmount} {
		if candidate == nil || !requested.IsValid() {
			continue
		}
		if err := verifyEcho(requested, candidate); err != nil {
			return nil, err
		}
	}
	if m, ok := toMoney(resp.AuthorizedAmount); ok {
		out.AuthorizedAmount = &m
	}
	if m, ok := toMoney(resp.CapturedAmount); ok {
		out.CapturedAmount = &m
	}

	if out.GatewayRef == "" && status != spi.StatusNotFound && status != spi.StatusFailed {
		out.GatewayRef = fallbackRef(idemKey)
	}
	return out, nil
}

func isKnownStatus(s spi.Status) bool {
	switch s {
	case spi.StatusAuthorized, spi.StatusCaptured, spi.StatusRefundAccepted, spi.StatusRefunded,
		spi.StatusVoided, spi.StatusRequiresAction, spi.StatusPending, spi.StatusDeclined,
		spi.StatusFailed, spi.StatusNotFound:
		return true
	default:
		return false
	}
}

func toMoney(a *WireAmount) (money.Money, bool) {
	if a == nil {
		return money.Money{}, false
	}
	m, err := money.New(a.MinorUnits, money.Currency(a.Currency))
	if err != nil {
		return money.Money{}, false
	}
	return m, true
}

// verifyEcho surfaces a currency mismatch or an over-capture as a contract violation, and permits a
// smaller amount as a legitimate partial authorization. Identical in shape to the vendor adapters'
// checks, deliberately: the simulator has to hold itself to the rule it is used to verify.
func verifyEcho(requested money.Money, got *WireAmount) error {
	cur := money.Currency(got.Currency)
	if cur != requested.Currency() {
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"simulator: the response echoed currency %s for a request in %s", cur, requested.Currency()).
			WithDetail(apierror.Detail{
				Field: "amount.currency", Code: "CURRENCY_ECHO_MISMATCH",
				Message: "the gateway acted in a different currency from the one requested",
				RuleID:  "L6.GATEWAY_ECHOES_CURRENCY",
			})
	}
	if got.MinorUnits > requested.Amount() {
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"simulator: the response echoed %d minor units for a request of %d",
			got.MinorUnits, requested.Amount()).
			WithDetail(apierror.Detail{
				Field: "amount.minorUnits", Code: "AMOUNT_ECHO_EXCEEDS_REQUEST",
				Message: "the gateway acted on a larger amount than was requested",
				RuleID:  "L6.GATEWAY_ECHOES_AMOUNT",
			})
	}
	return nil
}

// fallbackRef synthesises a reference from the idempotency key, so every actionable result carries
// something Lookup can resolve. See the identical note in the Stripe adapter.
func fallbackRef(idemKey string) string {
	if idemKey == "" {
		return ""
	}
	return "idemkey:" + idemKey
}
