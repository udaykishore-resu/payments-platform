package stripe

import (
	"net/http"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// mapIntentStatus normalizes Stripe's PaymentIntent status.
//
// The mapping is total over the statuses Stripe documents, and the default arm is a *failure*
// rather than a guess: a status this adapter has never seen means Stripe has shipped a lifecycle
// state the platform does not model, and quietly calling it PENDING would park a payment forever
// or, worse, calling it CAPTURED would post a ledger entry for money that never moved.
//
// Two arms deserve their reasoning stated:
//
//   - `requires_payment_method` is Stripe's post-decline state. The intent goes back to needing a
//     payment method precisely because the one supplied was refused, so with a last_payment_error
//     present it is a DECLINE, not a request for input. Without one it means the intent was
//     created but never confirmed, which for this adapter — which always confirms in the same
//     call — is a contract violation worth surfacing.
//   - `succeeded` maps to CAPTURED rather than to a "paid" notion. For a manual-capture intent
//     Stripe never reports `succeeded` until capture, so the two cannot be confused.
func mapIntentStatus(pi *paymentIntent) (spi.Status, error) {
	switch pi.Status {
	case "requires_payment_method":
		if pi.LastPaymentError != nil {
			return spi.StatusDeclined, nil
		}
		return spi.StatusFailed, nil
	case "requires_confirmation":
		// The adapter always sends confirm=true, so this state means Stripe declined to confirm
		// synchronously. It is in flight, not finished.
		return spi.StatusPending, nil
	case "requires_action":
		return spi.StatusRequiresAction, nil
	case "processing":
		return spi.StatusPending, nil
	case "requires_capture":
		return spi.StatusAuthorized, nil
	case "succeeded":
		return spi.StatusCaptured, nil
	case "canceled":
		return spi.StatusVoided, nil
	default:
		return "", apierror.Newf(apierror.CodeGatewayContractViolation,
			"stripe: unrecognised payment_intent status %q", pi.Status)
	}
}

// mapRefundStatus normalizes Stripe's Refund status.
//
// Stripe reports `succeeded` the moment it accepts the refund, not when the issuer posts it to
// the cardholder's statement — which can be five business days later, and which can still fail.
// The SPI is explicit that a successful refund call means *accepted*, so `succeeded` maps to
// REFUND_ACCEPTED and the terminal REFUNDED comes from the `charge.refund.updated` webhook or
// from settlement reconciliation. Reporting REFUNDED here would let the platform tell a merchant
// their customer has been paid before that is true.
func mapRefundStatus(r *refund) (spi.Status, error) {
	switch r.Status {
	case "pending", "requires_action", "succeeded":
		return spi.StatusRefundAccepted, nil
	case "failed", "canceled":
		return spi.StatusFailed, nil
	default:
		return "", apierror.Newf(apierror.CodeGatewayContractViolation,
			"stripe: unrecognised refund status %q", r.Status)
	}
}

// declineTable maps Stripe's `decline_code` to the platform's normalized taxonomy.
//
// Everything absent from this table maps to payment.DeclineUnknown, which does not permit
// failover. That default is the important half of the design: a decline code nobody has reasoned
// about must not cause the platform to re-present the same instruction to a second acquirer,
// because from the schemes' side that is indistinguishable from card testing and it is what gets
// a platform's acquiring relationships terminated.
//
// The soft set — the codes that permit failover — is intentionally tiny: the issuer or the
// network was unavailable, or the gateway itself erred. Everything else is a property of the
// instrument or of the cardholder's account, and no second gateway will change it.
var declineTable = map[string]payment.DeclineReason{
	// Soft: the same instruction may succeed elsewhere or later.
	"do_not_honor":         payment.DeclineDoNotHonorSoft,
	"processing_error":     payment.DeclineProcessingError,
	"issuer_not_available": payment.DeclineIssuerUnavailable,
	"try_again_later":      payment.DeclineTryAgainLater,
	"reenter_transaction":  payment.DeclineProcessingError,
	"approve_with_id":      payment.DeclineProcessingError,

	// Funds.
	"insufficient_funds":              payment.DeclineInsufficientFunds,
	"withdrawal_count_limit_exceeded": payment.DeclineInsufficientFunds,

	// The instrument itself.
	"expired_card":                   payment.DeclineCardExpired,
	"incorrect_number":               payment.DeclineIncorrectNumber,
	"invalid_number":                 payment.DeclineIncorrectNumber,
	"incorrect_cvc":                  payment.DeclineIncorrectCVC,
	"invalid_cvc":                    payment.DeclineIncorrectCVC,
	"invalid_expiry_month":           payment.DeclineCardExpired,
	"invalid_expiry_year":            payment.DeclineCardExpired,
	"incorrect_pin":                  payment.DeclineIncorrectCVC,
	"invalid_pin":                    payment.DeclineIncorrectCVC,
	"pin_try_exceeded":               payment.DeclineRestrictedCard,
	"offline_pin_required":           payment.DeclineRestrictedCard,
	"online_or_offline_pin_required": payment.DeclineRestrictedCard,

	// Reported lost, stolen, or fraudulent. These are also the codes that must never be retried
	// anywhere: see noRetryDeclineCodes.
	"lost_card":   payment.DeclineLostCard,
	"stolen_card": payment.DeclineStolenCard,
	"pickup_card": payment.DeclineStolenCard,
	"fraudulent":  payment.DeclineFraudulent,

	// The account exists but will not transact this way.
	"card_not_supported":               payment.DeclineRestrictedCard,
	"restricted_card":                  payment.DeclineRestrictedCard,
	"not_permitted":                    payment.DeclineRestrictedCard,
	"service_not_allowed":              payment.DeclineRestrictedCard,
	"transaction_not_allowed":          payment.DeclineRestrictedCard,
	"stop_payment_order":               payment.DeclineRestrictedCard,
	"revocation_of_authorization":      payment.DeclineRestrictedCard,
	"revocation_of_all_authorizations": payment.DeclineRestrictedCard,
	"security_violation":               payment.DeclineRestrictedCard,
	// A velocity decline is the issuer's own limit on this card, not ours. There is no velocity
	// member in the platform's taxonomy because the platform's velocity engine is a separate
	// control; mapping it to RESTRICTED_CARD keeps it hard, which is the property that matters —
	// re-presenting a velocity-limited card to another acquirer accelerates the limit.
	"card_velocity_exceeded": payment.DeclineRestrictedCard,

	// The account.
	"invalid_account":                   payment.DeclineInvalidAccount,
	"new_account_information_available": payment.DeclineInvalidAccount,
	"no_action_taken":                   payment.DeclineInvalidAccount,
	"incorrect_zip":                     payment.DeclineInvalidAccount,
	"account_closed":                    payment.DeclineInvalidAccount,

	// Configuration and authentication.
	"currency_not_supported":  payment.DeclineCurrencyNotSupp,
	"authentication_required": payment.DeclineAuthRequired,

	// Stripe's own risk engine, as distinct from the issuer's.
	"merchant_blacklist": payment.DeclineBlockedByRisk,
	"highest_risk_level": payment.DeclineBlockedByRisk,

	// `generic_decline` is deliberately absent. Stripe uses it when the issuer gave no reason, so
	// mapping it to anything specific would be an invention; it falls through to DeclineUnknown,
	// which is hard, which is the conservative answer to "we do not know why".
}

// noRetryDeclineCodes are declines that must never be re-presented, regardless of what the
// platform's own failover policy would otherwise permit.
//
// This is separate from the soft/hard split because it is a *scheme* rule rather than a routing
// judgement: Visa's VAMP and Mastercard's excessive-attempts programmes fine an acquirer per
// retry of a fraud-flagged authorization, and the fine lands whether or not the retry succeeds.
var noRetryDeclineCodes = map[string]struct{}{
	"lost_card":                        {},
	"stolen_card":                      {},
	"pickup_card":                      {},
	"fraudulent":                       {},
	"merchant_blacklist":               {},
	"revocation_of_all_authorizations": {},
	"stop_payment_order":               {},
}

// networkAdviceNoRetry reports whether the scheme told us not to try again.
//
// Stripe surfaces the raw scheme fields since the 2024 network mandates. `network_advice_code`
// "02" means "do not try again", "03" means "do not try again for this reason" and "21" means
// "the payment method has been stopped". "01" means the issuer has new account information and a
// retry is expected to succeed *after an account updater run*, which is not a retry the
// orchestrator can perform, so it too is treated as no-retry here.
func networkAdviceNoRetry(e *stripeError, out *chargeOutcome) bool {
	advice, decline := "", ""
	if e != nil {
		advice, decline = e.NetworkAdviceCode, e.NetworkDeclineCode
	}
	if advice == "" && out != nil {
		advice, decline = out.NetworkAdviceCode, out.NetworkDeclineCode
	}
	switch advice {
	case "01", "02", "03", "21":
		return true
	}
	// Scheme decline codes that are terminal on their own. 04 (pick up card), 07 (pick up card,
	// special conditions), 41 (lost card), 43 (stolen card), 57 (transaction not permitted to
	// cardholder) and 62 (restricted card) are all "do not re-present" under the mandates.
	switch decline {
	case "04", "07", "41", "43", "57", "62":
		return true
	}
	if e != nil {
		if _, ok := noRetryDeclineCodes[e.DeclineCode]; ok {
			return true
		}
	}
	return false
}

// mapDecline turns Stripe's error object into the normalized reason plus the scheme's retry
// guidance. An unmapped code answers DeclineUnknown, which PermitsFailover reports false for.
func mapDecline(e *stripeError, out *chargeOutcome) (payment.DeclineReason, bool) {
	if e == nil {
		return payment.DeclineUnknown, false
	}
	reason, ok := declineTable[e.DeclineCode]
	if !ok {
		// A card_error whose code is itself a decline reason (`expired_card`, `incorrect_cvc`)
		// arrives with no decline_code at all: Stripe puts the reason in `code` when it knows it
		// before reaching the issuer. Falling back to `code` is what stops those becoming UNKNOWN.
		reason, ok = declineTable[e.Code]
	}
	if !ok {
		reason = payment.DeclineUnknown
	}
	return reason, networkAdviceNoRetry(e, out)
}

// mapNextAction translates Stripe's next_action into the platform's redirect instruction.
//
// `use_stripe_sdk` is the 3DS2 challenge handled entirely inside Stripe.js; there is no URL for
// the platform to hand back, and the merchant's client completes it with the intent's client
// secret. Representing it as a challenge with no redirect URL is honest: the platform's job is to
// tell the merchant "the payer must authenticate", not to own the challenge transport.
func mapNextAction(na *nextAction) *spi.NextAction {
	if na == nil {
		return nil
	}
	switch na.Type {
	case "redirect_to_url":
		if na.RedirectToURL == nil || na.RedirectToURL.URL == "" {
			return nil
		}
		return &spi.NextAction{Type: payment.ActionRedirect, RedirectURL: na.RedirectToURL.URL}
	case "use_stripe_sdk":
		return &spi.NextAction{Type: payment.ActionThreeDSChall}
	case "display_bank_transfer_instructions":
		return &spi.NextAction{Type: payment.ActionAwaitTransfer}
	default:
		// An unrecognised action type is still an action: reporting a redirect with no URL is
		// better than reporting no action at all, which would let the orchestrator mark the
		// payment failed while the payer is mid-authentication.
		return &spi.NextAction{Type: payment.ActionRedirect}
	}
}

// mapProcessorResponse lifts the scheme-level detail a dispute defence needs out of the charge.
func mapProcessorResponse(c *charge) spi.ProcessorResponse {
	var pr spi.ProcessorResponse
	if c == nil {
		return pr
	}
	if c.Outcome != nil {
		pr.Network = c.Outcome.NetworkStatus
	}
	d := c.PaymentMethodDetails
	if d == nil || d.Card == nil {
		return pr
	}
	card := d.Card
	pr.Network = card.Network
	pr.NetworkTxnID = card.NetworkTransaction
	pr.AuthCode = card.AuthorizationCode
	if card.Checks != nil {
		pr.AVSResult = card.Checks.AddressPostalCodeCheck
		pr.CVVResult = card.Checks.CVCCheck
	}
	if card.ThreeDSecure != nil {
		pr.ThreeDSResult = card.ThreeDSecure.Result
		pr.ECI = card.ThreeDSecure.ElectronicCommerce
	}
	return pr
}

// mapErrorType turns a Stripe error envelope into a platform error.
//
// The retryability of each arm is the part that carries consequences, so it is stated per type:
//
//   - card_error is never reached here: it is a decline and is handled as a Result, not an error.
//     If one does arrive (a card_error on a non-payment endpoint) it is treated as a business-rule
//     failure, not as something to retry.
//   - invalid_request_error means the request itself is wrong. Retrying it verbatim cannot help,
//     so it is not retryable, and it is a *contract* violation rather than a caller validation
//     failure because the caller did not compose this body — the adapter did.
//   - idempotency_error means the same Idempotency-Key was reused with different parameters.
//     Retrying is not only useless but actively wrong: it means two different requests are
//     competing for one key, and one of them has to be given a new key by whoever generated it.
//     This is the one arm the SPI singles out, and it is explicitly non-retryable.
//   - rate_limit_error is retryable and is safe: Stripe rejects a rate-limited request before it
//     reaches the processing path, so no money moved.
//   - api_error is Stripe telling us their side broke. At 5xx it is handled earlier as an unknown
//     outcome for money-moving calls; reaching here means a 4xx api_error, which is retryable.
//   - authentication_error is our credential being wrong. It maps to ErrCredentialsInvalid, which
//     the orchestrator pages on rather than failing the payment quietly.
func mapErrorType(status int, e *stripeError) error {
	if e == nil {
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"stripe: HTTP %d with no error object", status)
	}
	switch e.Type {
	case "authentication_error":
		return apierror.Wrap(spi.ErrCredentialsInvalid, apierror.CodeGatewayAuthenticationFailed,
			"stripe: the API key was rejected")
	case "idempotency_error":
		return apierror.New(apierror.CodeIdempotencyKeyReused,
			"stripe: this idempotency key was already used with different parameters")
	case "rate_limit_error":
		return apierror.New(apierror.CodeRateLimited, "stripe: rate limit exceeded")
	case "invalid_request_error":
		if status == http.StatusNotFound {
			return apierror.New(apierror.CodePaymentNotFound,
				"stripe: no such resource")
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return apierror.Wrap(spi.ErrCredentialsInvalid, apierror.CodeGatewayAuthenticationFailed,
				"stripe: the API key is not permitted to perform this operation")
		}
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"stripe: the request was rejected as invalid (%s)", safeCode(e))
	case "api_error":
		return apierror.New(apierror.CodeGatewayUnavailable, "stripe: the gateway reported an internal error")
	case "card_error":
		return apierror.Newf(apierror.CodeGatewayDeclined,
			"stripe: the card was declined (%s)", safeCode(e))
	default:
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return apierror.Wrap(spi.ErrCredentialsInvalid, apierror.CodeGatewayAuthenticationFailed,
				"stripe: the API key was rejected")
		}
		return apierror.Newf(apierror.CodeGatewayContractViolation,
			"stripe: unrecognised error type %q at HTTP %d", e.Type, status)
	}
}

// safeCode renders the vendor's machine-readable code for an error message.
//
// It deliberately renders `code`/`decline_code` and never `message`. Stripe's human-readable
// message can echo request parameters back — including, on a malformed tokenization request, a
// fragment of the payment method — and an error string is the one place in this platform that
// gets copied into a support ticket, a log index and a customer email.
func safeCode(e *stripeError) string {
	switch {
	case e == nil:
		return "unknown"
	case e.DeclineCode != "":
		return e.DeclineCode
	case e.Code != "":
		return e.Code
	case e.Type != "":
		return e.Type
	default:
		return "unknown"
	}
}

// webhookKind classifies a Stripe event type.
//
// The default arm is KindIgnored, not an error. Stripe sends over two hundred event types and adds
// more with every product launch; an adapter that errors on an unknown type converts a Stripe
// feature release into a webhook-ingress incident on our side, with a retry storm attached.
func webhookKind(eventType string) spi.WebhookKind {
	switch eventType {
	case "payment_intent.succeeded", "payment_intent.amount_capturable_updated":
		return spi.KindAuthorizationSucceeded
	case "payment_intent.payment_failed":
		return spi.KindAuthorizationFailed
	case "charge.captured", "charge.succeeded":
		return spi.KindCaptureSucceeded
	case "charge.failed":
		return spi.KindCaptureFailed
	case "charge.refunded", "refund.created", "charge.refund.updated":
		return spi.KindRefundSucceeded
	case "refund.failed":
		return spi.KindRefundFailed
	case "payment_intent.canceled":
		return spi.KindVoidSucceeded
	case "payout.paid":
		return spi.KindPayoutSettled
	case "charge.dispute.created":
		return spi.KindDisputeOpened
	case "charge.dispute.closed":
		return spi.KindDisputeClosed
	case "account.updated", "account.application.deauthorized":
		return spi.KindAccountUpdated
	default:
		return spi.KindIgnored
	}
}
