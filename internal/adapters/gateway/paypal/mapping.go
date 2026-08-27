package paypal

import (
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// mapOrderStatus normalizes PayPal's Checkout order status.
//
// PayPal's order lifecycle describes the *order*, not the money, and the two are only loosely
// coupled — which is why every arm below says what it means for funds rather than restating the
// vendor's word:
//
//   - CREATED and SAVED: the order exists, nothing has been asked of the payer. In flight.
//   - PAYER_ACTION_REQUIRED: the payer must complete a step in PayPal's own flow. This is what the
//     redirect link is for.
//   - APPROVED: the payer has approved and PayPal will honour a capture for the approval window.
//     That is functionally a hold, so it maps to AUTHORIZED. It is not a scheme authorization and
//     no issuer has been asked yet, which is why the platform must still call capture.
//   - COMPLETED: the money has moved.
//   - VOIDED: the authorization was released.
//
// An unrecognised status is an error rather than a guess, on the same reasoning as the other two
// adapters: PayPal shipping a new order state means the platform does not know where the money is.
func mapOrderStatus(status string) (spi.Status, error) {
	switch status {
	case "CREATED", "SAVED":
		return spi.StatusPending, nil
	case "PAYER_ACTION_REQUIRED":
		return spi.StatusRequiresAction, nil
	case "APPROVED":
		return spi.StatusAuthorized, nil
	case "COMPLETED":
		return spi.StatusCaptured, nil
	case "VOIDED":
		return spi.StatusVoided, nil
	default:
		return "", apierror.Newf(apierror.CodeGatewayContractViolation,
			"paypal: unrecognised order status %q", status)
	}
}

// mapCaptureStatus normalizes a capture object's status.
func mapCaptureStatus(status string) (spi.Status, error) {
	switch status {
	case "COMPLETED", "PARTIALLY_REFUNDED":
		return spi.StatusCaptured, nil
	case "PENDING":
		// A pending capture is real: PayPal holds funds for review, or the buyer paid by eCheck,
		// which clears in days. Reporting it as captured would post a ledger entry for money that
		// may still be reversed.
		return spi.StatusPending, nil
	case "DECLINED":
		return spi.StatusDeclined, nil
	case "REFUNDED":
		return spi.StatusRefunded, nil
	case "FAILED":
		return spi.StatusFailed, nil
	default:
		return "", apierror.Newf(apierror.CodeGatewayContractViolation,
			"paypal: unrecognised capture status %q", status)
	}
}

// mapAuthorizationStatus normalizes an authorization object's status.
func mapAuthorizationStatus(status string) (spi.Status, error) {
	switch status {
	case "CREATED":
		return spi.StatusAuthorized, nil
	case "CAPTURED", "PARTIALLY_CAPTURED":
		return spi.StatusCaptured, nil
	case "DENIED":
		return spi.StatusDeclined, nil
	case "VOIDED":
		return spi.StatusVoided, nil
	case "PENDING":
		return spi.StatusPending, nil
	case "EXPIRED":
		// An expired authorization is not a void: the funds were released by the issuer rather than
		// by us, and the payment cannot be captured. FAILED is the honest normalization, and the
		// raw status is preserved so support can tell the two apart.
		return spi.StatusFailed, nil
	default:
		return "", apierror.Newf(apierror.CodeGatewayContractViolation,
			"paypal: unrecognised authorization status %q", status)
	}
}

// mapRefundStatus normalizes a refund object's status.
//
// COMPLETED maps to REFUND_ACCEPTED rather than REFUNDED for the reason the SPI states: a refund is
// asynchronous everywhere, and PayPal's COMPLETED means they have accepted and initiated it, not
// that the payer's issuer has posted it.
func mapRefundStatus(status string) (spi.Status, error) {
	switch status {
	case "COMPLETED", "PENDING":
		return spi.StatusRefundAccepted, nil
	case "CANCELLED", "FAILED":
		return spi.StatusFailed, nil
	default:
		return "", apierror.Newf(apierror.CodeGatewayContractViolation,
			"paypal: unrecognised refund status %q", status)
	}
}

// declineTable maps PayPal's `processor_response.response_code` to the platform's taxonomy.
//
// These are the acquirer's codes passed through for card-funded transactions; a PayPal-wallet
// payment has no processor beneath it and carries no code at all, which is why every lookup here is
// guarded and why an absent code produces DeclineUnknown rather than a default.
//
// The soft set is small and deliberate: 0500/0530/0540 (do not honour — an issuer's discretionary
// refusal that another acquirer can legitimately re-present), 0910/0911/0912 (issuer or system
// unavailable) and 1000/7100 (a processing failure below the issuer). Everything else is a property
// of the instrument or the cardholder.
var declineTable = map[string]payment.DeclineReason{
	// Soft.
	"0500": payment.DeclineDoNotHonorSoft,
	"0530": payment.DeclineDoNotHonorSoft,
	"0540": payment.DeclineDoNotHonorSoft,
	"0910": payment.DeclineIssuerUnavailable,
	"0911": payment.DeclineIssuerUnavailable,
	"0912": payment.DeclineIssuerUnavailable,
	"1000": payment.DeclineProcessingError,
	"7100": payment.DeclineProcessingError,

	// Hard: authentication and verification.
	"00N7": payment.DeclineIncorrectCVC,
	"1380": payment.DeclineIncorrectCVC,
	"5110": payment.DeclineIncorrectCVC,
	"5130": payment.DeclineIncorrectCVC,
	"5650": payment.DeclineAuthRequired,

	// Hard: funds.
	"5120": payment.DeclineInsufficientFunds,
	"5210": payment.DeclineInsufficientFunds,

	// Hard: the instrument.
	"5180": payment.DeclineIncorrectNumber,
	"5400": payment.DeclineCardExpired,
	"5140": payment.DeclineRestrictedCard,
	"5150": payment.DeclineRestrictedCard,
	"5160": payment.DeclineRestrictedCard,
	"5700": payment.DeclineRestrictedCard,
	"5900": payment.DeclineRestrictedCard,
	"5930": payment.DeclineRestrictedCard,
	"5950": payment.DeclineRestrictedCard,
	"0100": payment.DeclineRestrictedCard,

	// Hard: the account.
	"1330": payment.DeclineInvalidAccount,
	"5170": payment.DeclineInvalidAccount,
	"6300": payment.DeclineInvalidAccount,

	// Hard: fraud and risk.
	"5190": payment.DeclineFraudulent,
	"9500": payment.DeclineFraudulent,
	"9520": payment.DeclineLostCard,
	"9540": payment.DeclineFraudulent,
	"9600": payment.DeclineFraudulent,
	"5200": payment.DeclineBlockedByRisk,
	"9510": payment.DeclineBlockedByRisk,

	// "5100" — generic decline — is deliberately absent. PayPal uses it when the issuer supplied no
	// reason, so any specific mapping would be an invention. It falls through to DeclineUnknown,
	// which is hard, which is the conservative answer to "we do not know why".
}

// noRetryResponseCodes are declines that must never be re-presented anywhere, on the same scheme
// mandate the other two adapters honour.
var noRetryResponseCodes = map[string]struct{}{
	"5190": {}, "9500": {}, "9510": {}, "9520": {}, "9540": {}, "9600": {}, "5200": {},
}

// mapDecline turns a processor response into the normalized reason plus the do-not-retry flag.
//
// `payment_advice_code` is PayPal's surfacing of the scheme's own retry guidance: "01" means the
// account has new information (retry only after an account updater run, which the orchestrator
// cannot perform), "02" and "03" mean do not try again, "21" means the instrument was stopped.
func mapDecline(pr *processorResponse, issues []string) (payment.DeclineReason, bool) {
	if pr == nil {
		return declineFromIssues(issues), false
	}
	reason, ok := declineTable[pr.ResponseCode]
	if !ok {
		reason = declineFromIssues(issues)
	}
	_, noRetry := noRetryResponseCodes[pr.ResponseCode]
	switch pr.PaymentAdviceCode {
	case "01", "02", "03", "21":
		noRetry = true
	}
	return reason, noRetry
}

// declineFromIssues is the fallback for a wallet-funded decline, which carries no processor code.
//
// PayPal expresses these as an `issue` string in the error body. Only the ones that carry real
// information are mapped; INSTRUMENT_DECLINED in particular means "the funding instrument was
// refused and the payer should pick another", which tells the platform nothing about *why* and
// therefore maps to DeclineUnknown — hard, no failover.
func declineFromIssues(issues []string) payment.DeclineReason {
	for _, issue := range issues {
		switch issue {
		case "PAYER_ACCOUNT_RESTRICTED", "PAYEE_ACCOUNT_RESTRICTED", "PAYER_ACCOUNT_LOCKED_OR_CLOSED":
			return payment.DeclineRestrictedCard
		case "PAYER_CANNOT_PAY", "TRANSACTION_REFUSED":
			return payment.DeclineUnknown
		case "CARD_EXPIRED":
			return payment.DeclineCardExpired
		case "CURRENCY_NOT_SUPPORTED", "CURRENCY_NOT_SUPPORTED_FOR_CARD_TYPE":
			return payment.DeclineCurrencyNotSupp
		case "PAYMENT_DENIED", "TRANSACTION_BLOCKED_BY_PAYEE":
			return payment.DeclineBlockedByRisk
		case "AUTHENTICATION_FAILURE", "PAYER_ACTION_REQUIRED":
			return payment.DeclineAuthRequired
		}
	}
	return payment.DeclineUnknown
}

// mapProcessorResponse lifts the scheme-level detail a dispute defence needs.
func mapProcessorResponse(pr *processorResponse, authCode string) spi.ProcessorResponse {
	out := spi.ProcessorResponse{AuthCode: authCode, Network: "paypal"}
	if pr == nil {
		return out
	}
	out.AVSResult = pr.AVSCode
	out.CVVResult = pr.CVVCode
	return out
}

// mapErrorName turns a PayPal error body into a platform error.
//
// The retryability of each arm is the consequential part:
//
//   - UNPROCESSABLE_ENTITY carries the business refusals. INSTRUMENT_DECLINED is a decline and is
//     handled as a Result before reaching here; the rest are permanent for the request as composed.
//   - INVALID_REQUEST is our bug, never retryable.
//   - NOT_AUTHORIZED / authentication failures map to ErrCredentialsInvalid, which pages rather
//     than failing a payment quietly.
//   - RATE_LIMIT_REACHED is retryable and safe: PayPal rejects before processing.
//   - INTERNAL_SERVER_ERROR at 4xx is retryable; at 5xx on a money-moving call it is handled
//     earlier as an unknown outcome, because PayPal does not promise a 500 means nothing happened.
func mapErrorName(status int, e *errorResponse) error {
	if e == nil {
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"paypal: HTTP %d with no error body", status)
	}
	name := e.Name
	if name == "" {
		name = e.Error // the OAuth endpoint uses `error` rather than `name`
	}
	switch name {
	case "INVALID_CLIENT", "invalid_client", "NOT_AUTHORIZED", "AUTHENTICATION_FAILURE":
		return apierror.Wrapf(spi.ErrCredentialsInvalid, apierror.CodeGatewayAuthenticationFailed,
			"paypal: the request was not authorized (debug_id %s)", e.DebugID)
	case "RATE_LIMIT_REACHED":
		return apierror.New(apierror.CodeRateLimited, "paypal: rate limit exceeded")
	case "RESOURCE_NOT_FOUND":
		return apierror.New(apierror.CodePaymentNotFound, "paypal: no such resource")
	case "INVALID_REQUEST", "VALIDATION_ERROR":
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"paypal: the request was rejected as invalid (%s, debug_id %s)", firstIssue(e), e.DebugID)
	case "UNPROCESSABLE_ENTITY":
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"paypal: the request could not be processed (%s, debug_id %s)", firstIssue(e), e.DebugID)
	case "INTERNAL_SERVER_ERROR", "SERVICE_UNAVAILABLE":
		return apierror.Newf(apierror.CodeGatewayUnavailable,
			"paypal: the gateway reported an internal error (debug_id %s)", e.DebugID)
	default:
		if status == 401 || status == 403 {
			return apierror.Wrap(spi.ErrCredentialsInvalid, apierror.CodeGatewayAuthenticationFailed,
				"paypal: the request was not authorized")
		}
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"paypal: HTTP %d with error name %q (debug_id %s)", status, name, e.DebugID)
	}
}

// firstIssue renders the machine-readable issue for an error message. `message` and `description`
// are prose and can echo request fields, so they are never rendered into a platform error.
func firstIssue(e *errorResponse) string {
	for _, d := range e.Details {
		if d.Issue != "" {
			return d.Issue
		}
	}
	return e.Name
}

// isInstrumentDeclined reports whether an error body is PayPal's way of saying "the card said no".
//
// This is the branch that keeps a decline from being reported as an error. PayPal returns HTTP 422
// with `issue: INSTRUMENT_DECLINED` for a refused card, and an adapter that treats every 422 as a
// failure will report every decline as an ERROR outcome — which permits failover, which is exactly
// what a hard decline must not do.
func isInstrumentDeclined(e *errorResponse) bool {
	if e == nil {
		return false
	}
	for _, d := range e.Details {
		switch d.Issue {
		case "INSTRUMENT_DECLINED", "PAYER_CANNOT_PAY", "TRANSACTION_REFUSED",
			"PAYER_ACCOUNT_RESTRICTED", "PAYER_ACCOUNT_LOCKED_OR_CLOSED", "PAYMENT_DENIED":
			return true
		}
	}
	return false
}

// webhookKind classifies a PayPal event type.
//
// The default is KindIgnored. PayPal's event catalogue is large and grows; erroring on an
// unrecognised type would make their next product launch our incident, with PayPal's own delivery
// retries amplifying it.
func webhookKind(eventType string) spi.WebhookKind {
	switch eventType {
	case "PAYMENT.AUTHORIZATION.CREATED":
		return spi.KindAuthorizationSucceeded
	case "PAYMENT.AUTHORIZATION.VOIDED":
		return spi.KindVoidSucceeded
	case "PAYMENT.CAPTURE.COMPLETED":
		return spi.KindCaptureSucceeded
	case "PAYMENT.CAPTURE.DENIED", "PAYMENT.CAPTURE.DECLINED":
		return spi.KindCaptureFailed
	case "PAYMENT.CAPTURE.REFUNDED", "PAYMENT.CAPTURE.REVERSED":
		return spi.KindRefundSucceeded
	case "CHECKOUT.ORDER.APPROVED", "CHECKOUT.ORDER.COMPLETED":
		return spi.KindAuthorizationSucceeded
	case "CUSTOMER.DISPUTE.CREATED":
		return spi.KindDisputeOpened
	case "CUSTOMER.DISPUTE.RESOLVED":
		return spi.KindDisputeClosed
	case "PAYMENT.PAYOUTSBATCH.SUCCESS":
		return spi.KindPayoutSettled
	case "MERCHANT.ONBOARDING.COMPLETED", "MERCHANT.PARTNER-CONSENT.REVOKED":
		// The revocation event is subscribed deliberately: a merchant can withdraw the platform's
		// access from inside PayPal at any time, and the platform must react by marking the
		// connection unhealthy rather than discovering it on the next payment.
		return spi.KindAccountUpdated
	default:
		return spi.KindIgnored
	}
}
