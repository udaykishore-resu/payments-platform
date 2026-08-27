package adyen

import (
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// mapResultCode normalizes Adyen's `resultCode`.
//
// `capture` reports whether the platform asked for a sale rather than an authorization, because
// Adyen answers `Authorised` for both and the difference lives in what we asked for, not in what
// it says. With `captureDelayHours: 0` Adyen captures the authorization for us shortly afterwards
// and confirms with a CAPTURE notification; reporting CAPTURED at this point is the status the
// orchestrator can act on, and the notification is the confirmation the ledger posts against.
//
// The default arm is an error, not a guess. Adyen adds result codes when they add flows — 3DS2
// brought three of them at once — and a code this adapter has never seen means the platform does
// not know what state the payment is in. Guessing PENDING would strand it; guessing DECLINED would
// lose a sale that succeeded.
func mapResultCode(code string, capture bool) (spi.Status, error) {
	switch code {
	case "Authorised":
		if capture {
			return spi.StatusCaptured, nil
		}
		return spi.StatusAuthorized, nil
	case "PartiallyAuthorised":
		// A partial authorization is an authorization. The amount that was actually held is
		// reported separately in Result.AuthorizedAmount, where the orchestrator can decide
		// whether it is enough for the order.
		return spi.StatusAuthorized, nil
	case "Refused":
		return spi.StatusDeclined, nil
	case "Error":
		// Adyen distinguishes a refusal (the issuer said no) from an error (something broke before
		// an issuer decision). The platform's OutcomeError is exactly that distinction, and it is
		// the one outcome that permits failover, so conflating it with Refused would either
		// suppress legitimate failover or enable illegitimate re-presentment.
		return spi.StatusFailed, nil
	case "Cancelled":
		return spi.StatusVoided, nil
	case "Received":
		// Asynchronous methods — SEPA direct debit, most bank redirects after the shopper returns.
		// The outcome arrives days later as a notification.
		return spi.StatusPending, nil
	case "Pending":
		return spi.StatusPending, nil
	case "RedirectShopper", "IdentifyShopper", "ChallengeShopper", "PresentToShopper":
		return spi.StatusRequiresAction, nil
	case "AuthenticationFinished", "AuthenticationNotRequired":
		// Intermediate 3DS2 states from the /payments/details flow. They are not outcomes; the
		// platform must wait for the follow-up.
		return spi.StatusPending, nil
	default:
		return "", apierror.Newf(apierror.CodeGatewayContractViolation,
			"adyen: unrecognised resultCode %q", code)
	}
}

// mapModificationStatus normalizes the answer to a capture, refund or cancel.
//
// Every Adyen modification answers `received`, and only `received`: the modification is queued and
// the outcome arrives as a notification. The mapping therefore differs per operation, because
// "queued" means different things to the platform — a queued capture is one the orchestrator
// should treat as captured and reconcile, whereas the SPI is explicit that a queued refund must be
// reported as accepted rather than as done.
func mapModificationStatus(status string, op modificationKind) (spi.Status, error) {
	if status != "received" && status != "Received" {
		return "", apierror.Newf(apierror.CodeGatewayContractViolation,
			"adyen: unrecognised modification status %q", status)
	}
	switch op {
	case modificationCapture:
		return spi.StatusCaptured, nil
	case modificationRefund:
		return spi.StatusRefundAccepted, nil
	case modificationCancel:
		return spi.StatusVoided, nil
	default:
		return "", apierror.Newf(apierror.CodeInternalError, "adyen: unknown modification kind %v", op)
	}
}

// modificationKind names the three modification endpoints, so the status mapping above can say
// what "received" means without re-deriving it from a URL.
type modificationKind int

const (
	modificationCapture modificationKind = iota
	modificationRefund
	modificationCancel
)

// refusalTable maps Adyen's `refusalReasonCode` to the platform's taxonomy.
//
// Adyen's codes are numeric strings and are stable — they have not been renumbered in a decade,
// which makes them a better key than the prose `refusalReason`, which is localised and which Adyen
// does change. Anything absent maps to DeclineUnknown, which does not permit failover.
//
// The soft set is codes 4, 9, 19, 21, 37 and 41: the acquirer erred, the issuer was unreachable,
// PIN validation could not be performed, the transaction was never submitted, the terminal fell
// back, or the cardholder verification method needs restarting. Every one of those is a property
// of the *attempt*, so another gateway can legitimately succeed. Everything else is a property of
// the card or the cardholder, and re-presenting it is card testing.
var refusalTable = map[string]payment.DeclineReason{
	"0": payment.DeclineUnknown, // Unknown — Adyen itself does not know
	// "2" (Refused) is deliberately absent: it is Adyen's generic refusal and carries no
	// information, so it falls through to DeclineUnknown rather than being invented into a reason.
	"3":  payment.DeclineRestrictedCard,    // Referral: the cardholder must call their bank
	"4":  payment.DeclineProcessingError,   // Acquirer Error — soft
	"5":  payment.DeclineRestrictedCard,    // Blocked Card
	"6":  payment.DeclineCardExpired,       // Expired Card
	"7":  payment.DeclineUnknown,           // Invalid Amount — the platform has no amount reason
	"8":  payment.DeclineIncorrectNumber,   // Invalid Card Number
	"9":  payment.DeclineIssuerUnavailable, // Issuer Unavailable — soft
	"10": payment.DeclineRestrictedCard,    // Not supported
	"11": payment.DeclineAuthRequired,      // 3D Not Authenticated
	"12": payment.DeclineInsufficientFunds, // Not enough balance
	"14": payment.DeclineFraudulent,        // Acquirer Fraud
	"15": payment.DeclineUnknown,           // Cancelled
	"16": payment.DeclineUnknown,           // Shopper Cancelled
	"17": payment.DeclineIncorrectCVC,      // Invalid Pin
	"18": payment.DeclineRestrictedCard,    // Pin tries exceeded
	"19": payment.DeclineProcessingError,   // Pin validation not possible — soft
	"20": payment.DeclineFraudulent,        // FRAUD
	"21": payment.DeclineProcessingError,   // Not Submitted — soft
	"22": payment.DeclineFraudulent,        // FRAUD-CANCELLED
	"23": payment.DeclineRestrictedCard,    // Transaction Not Permitted
	"24": payment.DeclineIncorrectCVC,      // CVC Declined
	"25": payment.DeclineRestrictedCard,    // Restricted Card
	"26": payment.DeclineRestrictedCard,    // Revocation Of Auth
	"27": payment.DeclineUnknown,           // Declined Non Generic
	"28": payment.DeclineInsufficientFunds, // Withdrawal amount exceeded
	"29": payment.DeclineRestrictedCard,    // Withdrawal count exceeded
	"31": payment.DeclineFraudulent,        // Issuer Suspected Fraud
	"32": payment.DeclineInvalidAccount,    // AVS Declined
	"33": payment.DeclineRestrictedCard,    // Card requires online pin
	"34": payment.DeclineInvalidAccount,    // No checking account available on card
	"35": payment.DeclineInvalidAccount,    // No savings account available on card
	"36": payment.DeclineAuthRequired,      // Mobile PIN required
	"37": payment.DeclineProcessingError,   // Contactless fallback — soft
	"38": payment.DeclineAuthRequired,      // Authentication required (SCA soft decline)
	"39": payment.DeclineAuthRequired,      // RReq not received from DS
	"41": payment.DeclineProcessingError,   // CVM required, restart payment — soft
	"42": payment.DeclineAuthRequired,      // 3DS Authentication Error
	"43": payment.DeclineRestrictedCard,    // Online PIN required
	"44": payment.DeclineProcessingError,   // Try another interface — soft in practice
	"46": payment.DeclineBlockedByRisk,     // Blocked by Adyen to prevent excessive retry fees
}

// noRetryRefusalCodes are refusals that must never be re-presented anywhere.
//
// This is Adyen's half of the same scheme mandate the Stripe adapter honours: code 46 is Adyen
// *telling us* they have started blocking retries because the scheme is about to fine us, and 20
// / 22 / 31 are fraud flags where each further attempt is separately chargeable.
var noRetryRefusalCodes = map[string]struct{}{
	"20": {}, "22": {}, "31": {}, "46": {}, "5": {}, "26": {},
}

// mapRefusal returns the normalized reason plus the scheme's do-not-retry guidance.
func mapRefusal(code string, additional map[string]string) (payment.DeclineReason, bool) {
	reason, ok := refusalTable[code]
	if !ok {
		reason = payment.DeclineUnknown
	}
	_, noRetry := noRetryRefusalCodes[code]
	// Adyen surfaces the scheme's own advice in additionalData when the acquirer passes it
	// through. "02" and "03" are the network's "do not try again" values, identical in meaning to
	// the ones Stripe exposes as first-class fields.
	switch additional["networkAdviceCode"] {
	case "01", "02", "03", "21":
		noRetry = true
	}
	return reason, noRetry
}

// mapAction translates Adyen's shopper action into the platform's redirect instruction.
func mapAction(a *action) *spi.NextAction {
	if a == nil {
		return nil
	}
	switch a.Type {
	case "redirect":
		return &spi.NextAction{Type: payment.ActionRedirect, RedirectURL: a.URL}
	case "threeDS2":
		// The 3DS2 fingerprint and challenge are completed by Adyen's client-side component using
		// the data in the action; there is no URL the platform can hand a merchant's server.
		return &spi.NextAction{Type: payment.ActionThreeDSChall, RedirectURL: a.URL}
	case "qrCode":
		return &spi.NextAction{Type: payment.ActionDisplayQR, QRCodeData: a.URL}
	case "voucher", "await":
		return &spi.NextAction{Type: payment.ActionAwaitTransfer, RedirectURL: a.URL}
	default:
		return &spi.NextAction{Type: payment.ActionRedirect, RedirectURL: a.URL}
	}
}

// mapProcessorResponse lifts the scheme-level detail out of Adyen's additionalData.
//
// additionalData is an open string map, which is why every read here is a lookup with a default
// rather than a field access: a key that is absent for this acquirer, this scheme or this account
// configuration must produce an empty string, not a panic and not a wrong value.
func mapProcessorResponse(ad map[string]string) spi.ProcessorResponse {
	if ad == nil {
		return spi.ProcessorResponse{}
	}
	return spi.ProcessorResponse{
		AuthCode:      ad["authCode"],
		AVSResult:     ad["avsResult"],
		CVVResult:     ad["cvcResult"],
		Network:       ad["paymentMethod"],
		NetworkTxnID:  ad["networkTxReference"],
		ThreeDSResult: ad["threeDAuthenticated"],
		ECI:           ad["eci"],
	}
}

// mapErrorCode turns an Adyen service error into a platform error.
//
// Adyen groups errors by `errorType`: `security` (authentication and permission), `validation`
// (the request is wrong), `configuration` (the account is not set up for this) and `internal`.
// Retryability follows the group, and the two that matter are stated here rather than derived:
// `validation` and `configuration` are never retryable — repeating an identical malformed request
// cannot start working — while `internal` is, because it is Adyen telling us their side broke
// before reaching an issuer.
func mapErrorCode(status int, e *serviceError) error {
	if e == nil {
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"adyen: HTTP %d with no error body", status)
	}
	switch e.ErrorType {
	case "security":
		return apierror.Wrapf(spi.ErrCredentialsInvalid, apierror.CodeGatewayAuthenticationFailed,
			"adyen: the API key was rejected (errorCode %s)", e.ErrorCode)
	case "validation":
		// Adyen returns 422 with errorCode 14_xxx for a refused-by-configuration case and
		// 000/010 for a malformed request; both are permanent for this request as composed.
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"adyen: the request was rejected as invalid (errorCode %s)", e.ErrorCode)
	case "configuration":
		return apierror.Newf(apierror.CodeGatewayNotConfigured,
			"adyen: the merchant account is not configured for this request (errorCode %s)", e.ErrorCode)
	case "internal":
		return apierror.Newf(apierror.CodeGatewayUnavailable,
			"adyen: the gateway reported an internal error (errorCode %s)", e.ErrorCode)
	default:
		if status == 401 || status == 403 {
			return apierror.Wrap(spi.ErrCredentialsInvalid, apierror.CodeGatewayAuthenticationFailed,
				"adyen: the API key was rejected")
		}
		if status == 429 {
			return apierror.New(apierror.CodeRateLimited, "adyen: rate limit exceeded")
		}
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"adyen: HTTP %d with errorCode %s", status, e.ErrorCode)
	}
}

// webhookKind classifies an Adyen eventCode plus its success flag.
//
// The success flag is load-bearing: Adyen sends AUTHORISATION for both an approval and a refusal,
// distinguished only by `success: "true"` / `"false"`. An adapter that ignores it marks declined
// payments as captured, which is the most expensive single bug available in this file.
func webhookKind(eventCode string, success bool) spi.WebhookKind {
	switch eventCode {
	case "AUTHORISATION":
		if success {
			return spi.KindAuthorizationSucceeded
		}
		return spi.KindAuthorizationFailed
	case "CAPTURE":
		if success {
			return spi.KindCaptureSucceeded
		}
		return spi.KindCaptureFailed
	case "CAPTURE_FAILED":
		return spi.KindCaptureFailed
	case "REFUND":
		if success {
			return spi.KindRefundSucceeded
		}
		return spi.KindRefundFailed
	case "REFUND_FAILED", "REFUNDED_REVERSED":
		return spi.KindRefundFailed
	case "CANCELLATION":
		return spi.KindVoidSucceeded
	case "PAYOUT_THIRDPARTY", "PAID_OUT":
		return spi.KindPayoutSettled
	case "NOTIFICATION_OF_CHARGEBACK", "CHARGEBACK", "REQUEST_FOR_INFORMATION":
		return spi.KindDisputeOpened
	case "CHARGEBACK_REVERSED", "SECOND_CHARGEBACK":
		return spi.KindDisputeClosed
	case "ACCOUNT_HOLDER_STATUS_CHANGE", "ACCOUNT_HOLDER_VERIFICATION", "ACCOUNT_HOLDER_UPDATED":
		return spi.KindAccountUpdated
	default:
		// Adyen sends dozens of event codes the platform does not model. Ignoring them is the
		// correct behaviour: erroring would make Adyen retry the delivery for days.
		return spi.KindIgnored
	}
}
