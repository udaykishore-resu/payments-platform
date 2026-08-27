package l5payment

import (
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/validation/engine"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/internal/ruledef"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

func init() {
	ruledef.Register(defs(DefaultDeps()), "payments-core", "2026-01-01", engine.Enforce)
}

// Rules returns the L5 rule set.
//
// CollectAll, on a 5 ms budget. The two are compatible only because every rule is pure and
// operates on pre-loaded inputs; the moment one of them reaches for a client the budget stops
// being a property of the code and starts being a property of the network.
func Rules(d Deps) engine.RuleSet[Subject] {
	return engine.RuleSet[Subject]{
		Name:  "L5.payment",
		Mode:  engine.CollectAll,
		Rules: ruledef.Build(defs(d)),
	}
}

func defs(d Deps) []ruledef.Def[Subject] {
	onCreate := func(s Subject) bool { return s.Op == OpCreate }
	onExisting := func(s Subject) bool { return s.Op != OpCreate }
	hasToken := func(s Subject) bool { return s.Request.Token.Present }
	hasRecord := func(s Subject) bool { return s.Idempotency.Exists }
	hasCustomer := func(s Subject) bool { return s.Request.CustomerRef != "" }

	return []ruledef.Def[Subject]{
		{
			ID: "L5.MERCHANT_EXISTS", Severity: engine.Error,
			Code: string(apierror.CodeMerchantNotFound), Field: "/merchantId", Pure: true,
			Desc:        "a merchant snapshot resolved for (tenant, merchant)",
			Remediation: "Check the merchant identifier: it does not exist under your tenant.",
			Check: func(s Subject) string {
				if s.Merchant.Found {
					return ""
				}
				return "merchant " + quote(string(s.Merchant.ID)) + " does not exist under your tenant"
			},
		},
		{
			ID: "L5.MERCHANT_IS_ACTIVE", Severity: engine.Error,
			Code: string(apierror.CodeMerchantNotActive), Field: "/merchantId", Pure: true,
			Desc:        "the merchant is ACTIVE, which is the only state that may take new payments",
			Remediation: "This merchant cannot accept new payments in its current state.",
			Applies:     func(s Subject) bool { return s.Merchant.Found && s.Op == OpCreate },
			Check: func(s Subject) string {
				// Delegated to the merchant domain rather than restated: exactly one state
				// qualifies, and which one is the merchant context's decision, not ours.
				if s.Merchant.Status.CanAcceptPayments() {
					return ""
				}
				return "merchant is " + string(s.Merchant.Status) + " and cannot accept new payments"
			},
		},
		{
			ID: "L5.SUSPENDED_PERMITS_MONEY_OUT", Severity: engine.Error,
			Code: string(apierror.CodeMerchantNotActive), Field: "/merchantId", Pure: true,
			Desc:        "a suspended merchant may still refund and void, but may not take new payments",
			Remediation: "This merchant is suspended. Refunds and voids are still permitted; new payments are not.",
			Applies:     func(s Subject) bool { return s.Merchant.Status == merchant.StatusSuspended },
			Check: func(s Subject) string {
				// A suspension stops a merchant taking money, not returning it. Blocking refunds
				// during a suspension converts a merchant problem into a consumer-harm problem
				// and, in several jurisdictions, a regulatory one.
				if s.Op == OpRefund || s.Op == OpVoid {
					return ""
				}
				return "merchant is suspended; only refunds and voids are permitted"
			},
		},
		{
			ID: "L5.CONFIG_SNAPSHOT_FRESH_ENOUGH", Severity: engine.Error,
			Code: string(apierror.CodeConfigurationStale), Field: "/configuration", Pure: true,
			Desc:        "the cached configuration snapshot is within the staleness ceiling",
			Remediation: "Configuration is temporarily unavailable. Retry shortly.",
			Check: func(s Subject) string {
				ceiling := d.MaxConfigStaleness
				if ceiling == 0 {
					ceiling = DefaultDeps().MaxConfigStaleness
				}
				// A merchant with no snapshot at all fails closed. The alternative — proceeding
				// on an empty configuration — means processing with no limits, no blocked
				// countries and no enabled-currency check, which is worse than a retryable 503.
				if !s.Config.Present {
					return "no configuration snapshot is available for this merchant"
				}
				if s.Config.Age > ceiling {
					return "the configuration snapshot is " + itoa64(int64(s.Config.Age.Seconds())) +
						"s old; the ceiling is " + itoa64(int64(ceiling.Seconds())) + "s"
				}
				return ""
			},
		},
		{
			ID: "L5.AMOUNT_IS_POSITIVE", Severity: engine.Error,
			Code: string(apierror.CodeAmountInvalid), Field: "/amount", Pure: true,
			Desc:        "the amount is greater than zero",
			Remediation: "`amount` must be greater than zero.",
			Applies:     func(s Subject) bool { return s.Op == OpCreate || s.Op == OpCapture || s.Op == OpRefund },
			Check: func(s Subject) string {
				if s.Request.Amount.IsPositive() {
					return ""
				}
				return "amount is " + itoa64(s.Request.Amount.Amount()) + " minor units"
			},
		},
		{
			ID: "L5.AMOUNT_RESPECTS_CURRENCY_EXPONENT", Severity: engine.Error,
			Code: string(apierror.CodeAmountInvalid), Field: "/amount", Pure: true,
			Desc:        "the amount is representable in the currency's minor units",
			Remediation: "Send the amount in the currency's minor units; a zero-decimal currency such as JPY has no sub-unit.",
			Check: func(s Subject) string {
				c := s.Request.Amount.Currency()
				if !c.IsSupported() {
					return quote(string(c)) + " is not a currency this platform can price in"
				}
				if s.Request.MinorDigitsSupplied > c.Exponent() {
					return string(c) + " has " + itoa(c.Exponent()) + " minor digits; the amount was sent with " +
						itoa(s.Request.MinorDigitsSupplied)
				}
				return ""
			},
		},
		{
			ID: "L5.AMOUNT_WITHIN_MERCHANT_LIMIT", Severity: engine.Error,
			Code: string(apierror.CodeAmountExceedsLimit), Field: "/amount", Pure: true,
			Desc:        "the amount is at or below the configured per-transaction limit",
			Remediation: "This amount exceeds your per-transaction limit. Contact your platform administrator to raise it.",
			Applies: func(s Subject) bool {
				return s.Op == OpCreate && !s.Config.MaxTransactionAmount.IsZero()
			},
			Check: func(s Subject) string {
				over, err := s.Request.Amount.GreaterThan(s.Config.MaxTransactionAmount)
				if err != nil {
					// A limit in a different currency is not a limit that can be compared, and
					// silently ignoring it would let a EUR-limited merchant take unlimited USD.
					return "the per-transaction limit is denominated in " +
						string(s.Config.MaxTransactionAmount.Currency()) + ", not " +
						string(s.Request.Amount.Currency())
				}
				if over {
					return "amount " + s.Request.Amount.String() + " exceeds the per-transaction limit of " +
						s.Config.MaxTransactionAmount.String()
				}
				return ""
			},
		},
		{
			ID: "L5.AMOUNT_ABOVE_METHOD_MINIMUM", Severity: engine.Error,
			Code: "AMOUNT_BELOW_MINIMUM", Field: "/amount", Pure: true,
			Desc:        "the amount is at or above the scheme minimum for the method and currency",
			Remediation: "This payment is below the scheme minimum for the payment method and currency.",
			Applies: func(s Subject) bool {
				if s.Op != OpCreate {
					return false
				}
				minAmt, ok := d.MethodMinimums[s.Request.Method]
				return ok && minAmt.Currency() == s.Request.Amount.Currency()
			},
			Check: func(s Subject) string {
				minAmt := d.MethodMinimums[s.Request.Method]
				less, err := s.Request.Amount.LessThan(minAmt)
				if err != nil {
					return ""
				}
				if less {
					return "the minimum " + string(s.Request.Method) + " payment in " +
						string(minAmt.Currency()) + " is " + minAmt.String()
				}
				return ""
			},
		},
		{
			ID: "L5.CURRENCY_IS_ENABLED", Severity: engine.Error,
			Code: string(apierror.CodeCurrencyNotSupported), Field: "/currency", Pure: true,
			Desc:        "the request currency is in the merchant's enabled set",
			Remediation: "Enable this currency in your merchant configuration, or send a currency you have enabled.",
			Applies:     onCreate,
			Check: func(s Subject) string {
				if containsCurrency(s.Config.Currencies, s.Request.Amount.Currency()) {
					return ""
				}
				return string(s.Request.Amount.Currency()) + " is not enabled for this merchant"
			},
		},
		{
			ID: "L5.PAYMENT_METHOD_IS_ENABLED", Severity: engine.Error,
			Code: string(apierror.CodePaymentMethodNotSupported), Field: "/paymentMethod", Pure: true,
			Desc:        "the request method is in the merchant's enabled set",
			Remediation: "Enable this payment method in your merchant configuration, or send a method you have enabled.",
			Applies:     onCreate,
			Check: func(s Subject) string {
				if containsMethod(s.Config.Methods, s.Request.Method) {
					return ""
				}
				return string(s.Request.Method) + " is not enabled for this merchant"
			},
		},
		{
			ID: "L5.METHOD_CURRENCY_PAIR_ROUTABLE", Severity: engine.Error,
			Code: string(apierror.CodeNoEligibleGateway), Field: "/paymentMethod", Pure: true,
			Desc:        "the compiled routing policy has at least one candidate for (method, currency, country)",
			Remediation: "No gateway is configured for this payment method in this currency. Add a gateway or use a different combination.",
			Applies:     onCreate,
			Check: func(s Subject) string {
				for _, c := range s.Config.Candidates {
					if c.Method != s.Request.Method || c.Currency != s.Request.Amount.Currency() {
						continue
					}
					if c.Country == "" || s.Request.CustomerCountry == "" ||
						c.Country == s.Request.CustomerCountry {
						return ""
					}
				}
				return "no gateway is configured for " + string(s.Request.Method) + " in " +
					string(s.Request.Amount.Currency())
			},
		},
		{
			ID: "L5.CUSTOMER_COUNTRY_IN_SUPPORTED_SET", Severity: engine.Error,
			Code: "COUNTRY_NOT_SUPPORTED", Field: "/customer/country", Pure: true,
			Desc:        "the customer country is in the merchant's enabled country set",
			Remediation: "Enable this country in your merchant configuration to accept payments from it.",
			Applies:     func(s Subject) bool { return s.Request.CustomerCountry != "" },
			Check: func(s Subject) string {
				if len(s.Config.Countries) == 0 || containsCountry(s.Config.Countries, s.Request.CustomerCountry) {
					return ""
				}
				return "payments from " + string(s.Request.CustomerCountry) + " are not enabled for this merchant"
			},
		},
		{
			ID: "L5.CUSTOMER_COUNTRY_NOT_BLOCKED", Severity: engine.Error,
			Code: string(apierror.CodeRiskDeclined), Field: "/customer/country", Pure: true,
			Desc:        "the customer country is not on the merchant's blocked list",
			Remediation: "Payments from this country are blocked by your own risk policy; change the policy to accept them.",
			Applies:     func(s Subject) bool { return s.Request.CustomerCountry != "" },
			Check: func(s Subject) string {
				if !containsCountry(s.Config.BlockedCountries, s.Request.CustomerCountry) {
					return ""
				}
				return "payments from " + string(s.Request.CustomerCountry) + " are blocked by your risk policy"
			},
		},
		{
			ID: "L5.IP_COUNTRY_NOT_SANCTIONED", Severity: engine.Error,
			Code: string(apierror.CodeRiskDeclined), Field: "/customer/ipCountry", Pure: true,
			Desc:        "the resolved IP country is not on the platform sanctions set",
			Remediation: "This payment cannot be processed.",
			Applies:     func(s Subject) bool { return s.Request.IPCountry != "" },
			Check: func(s Subject) string {
				// Deliberately uninformative to the caller: naming the sanctions list turns the
				// API into an oracle for which countries are on it.
				if !containsCountry(d.SanctionedCountries, s.Request.IPCountry) {
					return ""
				}
				return "this payment cannot be processed"
			},
		},
		{
			ID: "L5.TOKEN_REFERENCE_PRESENT", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/paymentMethodToken", Pure: true,
			Desc:        "a gateway or network token reference is present for a tokenized method",
			Remediation: "Provide a gateway or network token. Raw card data is never accepted.",
			Applies: func(s Subject) bool {
				return s.Op == OpCreate && s.Request.Method.RequiresSCAConsideration()
			},
			Check: func(s Subject) string {
				if s.Request.TokenRef != "" || s.Request.Token.Present {
					return ""
				}
				return "no payment method token was supplied"
			},
		},
		{
			ID: "L5.TOKEN_BELONGS_TO_MERCHANT", Severity: engine.Error,
			Code: string(apierror.CodeForbidden), Field: "/paymentMethodToken", Pure: true,
			Desc:        "the token's owning merchant equals the request merchant",
			Remediation: "This token belongs to a different merchant. Tokens are not portable between merchants.",
			Applies:     hasToken,
			Check: func(s Subject) string {
				if s.Request.Token.OwnerMerchantID == s.Merchant.ID {
					return ""
				}
				return "this token does not belong to merchant " + string(s.Merchant.ID)
			},
		},
		{
			ID: "L5.TOKEN_NOT_EXPIRED", Severity: engine.Error,
			Code: "PAYMENT_METHOD_EXPIRED", Field: "/paymentMethodToken", Pure: true,
			Desc:        "the token has not expired and the stored card expiry is not in the past",
			Remediation: "The saved payment method has expired. Ask the customer to re-enter it.",
			Applies:     hasToken,
			Check: func(s Subject) string {
				t := s.Request.Token
				if !t.ExpiresAt.IsZero() && !t.ExpiresAt.After(s.Now) {
					return "the stored token expired on " + t.ExpiresAt.UTC().Format("2006-01-02")
				}
				if t.CardExpiryYear > 0 && t.CardExpiryMonth > 0 {
					y, m := s.Now.UTC().Year(), int(s.Now.UTC().Month())
					if t.CardExpiryYear < y || (t.CardExpiryYear == y && t.CardExpiryMonth < m) {
						return "the saved card expired in " + itoa(t.CardExpiryMonth) + "/" + itoa(t.CardExpiryYear)
					}
				}
				return ""
			},
		},
		{
			ID: "L5.CAPTURE_MODE_IS_SUPPORTED", Severity: engine.Error,
			Code: string(apierror.CodePaymentMethodNotSupported), Field: "/captureMode", Pure: true,
			Desc:        "the capture mode is known and manual capture is permitted for this method",
			Remediation: "Manual capture is not available for this payment method; use automatic capture.",
			Applies:     onCreate,
			Check: func(s Subject) string {
				if !s.Request.CaptureMode.IsValid() {
					return quote(string(s.Request.CaptureMode)) + " is not a valid capture mode"
				}
				if s.Request.CaptureMode != payment.CaptureManual {
					return ""
				}
				if !s.Request.Method.SupportsSeparateCapture() {
					return string(s.Request.Method) + " has no separate capture step"
				}
				if !s.Config.ManualCaptureAllowed {
					return "manual capture is not enabled for this merchant"
				}
				return ""
			},
		},
		{
			ID: "L5.IDEMPOTENCY_SCOPE_MATCHES", Severity: engine.Error,
			Code: string(apierror.CodeIdempotencyKeyReused), Field: "Idempotency-Key", Pure: true,
			Desc:        "the stored scope tuple equals this request's (tenant, merchant, method, path template)",
			Remediation: "This idempotency key was used on a different endpoint. Use a fresh key.",
			Applies:     hasRecord,
			Check: func(s Subject) string {
				if s.ExpectedScope == "" || s.Idempotency.Scope == s.ExpectedScope {
					return ""
				}
				return "this idempotency key is bound to a different endpoint"
			},
		},
		{
			ID: "L5.IDEMPOTENCY_FINGERPRINT_MATCHES", Severity: engine.Error,
			Code: string(apierror.CodeIdempotencyKeyReused), Field: "Idempotency-Key", Pure: true,
			Desc:        "the canonicalized request body hashes to the stored fingerprint",
			Remediation: "This idempotency key was already used with a different request body. Use a fresh key.",
			Applies: func(s Subject) bool {
				return s.Idempotency.Exists && s.Idempotency.Fingerprint != ""
			},
			Check: func(s Subject) string {
				if s.RequestFingerprint == s.Idempotency.Fingerprint {
					return ""
				}
				// Replaying a key with a changed body is the one case where honouring the key
				// would be worse than rejecting: the caller believes they are retrying and is
				// in fact asking for a different payment.
				return "this idempotency key was already used with a different request body"
			},
		},
		{
			ID: "L5.NO_INFLIGHT_DUPLICATE", Severity: engine.Error,
			Code: string(apierror.CodeIdempotentRequestInProgress), Field: "Idempotency-Key", Pure: true,
			Desc:        "no unexpired in-flight lease exists for this idempotency key",
			Remediation: "An identical request is in progress. Retry after the interval in `Retry-After`.",
			Applies:     func(s Subject) bool { return s.Idempotency.Exists && s.Idempotency.InFlight },
			Check: func(s Subject) string {
				if !s.Idempotency.LeaseExpiresAt.IsZero() && !s.Idempotency.LeaseExpiresAt.After(s.Now) {
					// An expired lease means the previous holder died; the claim is reclaimable.
					return ""
				}
				return "an identical request is already in progress"
			},
		},
		{
			ID: "L5.OPERATION_SCOPE_AUTHORIZED", Severity: engine.Error,
			Code: string(apierror.CodeForbidden), Field: "principal", Pure: true,
			Desc:        "the principal holds the scope this operation requires",
			Remediation: "Your credentials lack the scope this operation requires.",
			Check: func(s Subject) string {
				want, ok := d.ScopeForOperation[s.Op]
				if !ok || want == "" {
					return ""
				}
				if containsString(s.Principal.Scopes, want) {
					return ""
				}
				return "this operation requires the `" + want + "` scope"
			},
		},
		{
			ID: "L5.REFUND_REQUIRES_ELEVATED_ROLE_ABOVE_THRESHOLD", Severity: engine.Error,
			Code: string(apierror.CodeForbidden), Field: "principal", Pure: true,
			Desc:        "a refund above the tenant threshold requires the elevated refund scope",
			Remediation: "Refunds above your platform's threshold require an elevated role.",
			Applies: func(s Subject) bool {
				if s.Op != OpRefund || d.RefundElevatedThreshold.IsZero() {
					return false
				}
				over, err := s.Request.Amount.GreaterThan(d.RefundElevatedThreshold)
				return err == nil && over
			},
			Check: func(s Subject) string {
				if containsString(s.Principal.Scopes, d.ElevatedRefundScope) {
					return ""
				}
				return "refunds above " + d.RefundElevatedThreshold.String() +
					" require the `" + d.ElevatedRefundScope + "` scope"
			},
		},
		{
			ID: "L5.DAILY_VOLUME_WITHIN_LIMIT", Severity: engine.Error,
			Code: string(apierror.CodeAmountExceedsLimit), Field: "/amount", Pure: true,
			Desc:        "today's volume plus this amount is within the configured daily limit",
			Remediation: "This payment would exceed your daily volume limit. Contact your platform administrator to raise it.",
			Applies: func(s Subject) bool {
				return s.Op == OpCreate && !s.Config.DailyVolumeLimit.IsZero()
			},
			Check: func(s Subject) string {
				total, err := s.Velocity.TodayVolume.Add(s.Request.Amount)
				if err != nil {
					return "today's volume is tracked in " + string(s.Velocity.TodayVolume.Currency()) +
						", which does not match this payment's currency"
				}
				over, err := total.GreaterThan(s.Config.DailyVolumeLimit)
				if err != nil {
					return "the daily volume limit is denominated in a different currency to this payment"
				}
				if over {
					return "this payment would take today's volume to " + total.String() +
						", above your daily limit of " + s.Config.DailyVolumeLimit.String()
				}
				return ""
			},
		},
		{
			ID: "L5.VELOCITY_PAYMENTS_PER_MINUTE", Severity: engine.Error,
			Code: string(apierror.CodeVelocityLimitExceeded), Field: "/merchantId", Pure: true,
			Desc:        "payments in the last minute are below the configured per-minute limit",
			Remediation: "Payment rate limit reached. Retry shortly.",
			Applies:     func(s Subject) bool { return s.Op == OpCreate && s.Config.MaxPaymentsPerMinute > 0 },
			Check: func(s Subject) string {
				if s.Velocity.CountLastMinute < s.Config.MaxPaymentsPerMinute {
					return ""
				}
				return "this merchant has reached its limit of " + itoa(s.Config.MaxPaymentsPerMinute) +
					" payments per minute"
			},
		},
		{
			ID: "L5.VELOCITY_PER_CARD_PER_HOUR", Severity: engine.Error,
			Code: string(apierror.CodeVelocityLimitExceeded), Field: "/paymentMethodToken", Pure: true,
			Desc:        "uses of this instrument in the last hour are below the configured limit",
			Remediation: "This payment method has been used too many times in the last hour.",
			Applies: func(s Subject) bool {
				return s.Request.Token.Fingerprint != "" && s.Config.MaxPerCardPerHour > 0
			},
			Check: func(s Subject) string {
				if s.Velocity.CountForFingerprintLastHour < s.Config.MaxPerCardPerHour {
					return ""
				}
				return "this payment method has been used " + itoa(s.Velocity.CountForFingerprintLastHour) +
					" times in the last hour"
			},
		},
		{
			ID: "L5.VELOCITY_PER_CUSTOMER_PER_DAY", Severity: engine.Error,
			Code: string(apierror.CodeVelocityLimitExceeded), Field: "/customer/reference", Pure: true,
			Desc:        "payments by this customer today are below the configured daily limit",
			Remediation: "This customer has reached the daily payment limit.",
			Applies:     hasCustomer,
			Check: func(s Subject) string {
				limit := s.Config.MaxPerCustomerPerDay
				if limit == 0 {
					limit = defaultInt(d.CustomerDailyLimit, 20)
				}
				if s.Velocity.CountForCustomerToday < limit {
					return ""
				}
				return "this customer has made " + itoa(s.Velocity.CountForCustomerToday) +
					" payments today; the limit is " + itoa(limit)
			},
		},
		{
			ID: "L5.VELOCITY_DISTINCT_CARDS_PER_CUSTOMER", Severity: engine.Error,
			Code: string(apierror.CodeRiskDeclined), Field: "/customer/reference", Pure: true,
			Desc:        "distinct instruments tried by this customer in the last hour are within the card-testing threshold",
			Remediation: "Too many payment methods have been attempted for this customer.",
			Applies:     hasCustomer,
			Check: func(s Subject) string {
				limit := s.Config.MaxDistinctCards
				if limit == 0 {
					limit = defaultInt(d.DistinctCardsPerCustomerHour, 3)
				}
				// Several distinct instruments from one customer in an hour is the signature of
				// card testing, and the cost of a false positive here is one declined payment
				// against the cost of hosting an attacker's BIN-testing run.
				if s.Velocity.DistinctFingerprintsLastHour <= limit {
					return ""
				}
				return "this customer has attempted " + itoa(s.Velocity.DistinctFingerprintsLastHour) +
					" distinct payment methods in the last hour"
			},
		},
		{
			ID: "L5.VELOCITY_DECLINE_RATIO", Severity: engine.Error,
			Code: string(apierror.CodeRiskDeclined), Field: "/merchantId", Pure: true,
			Desc:        "the merchant's recent decline ratio is below the circuit threshold",
			Remediation: "An elevated decline rate was detected; payments are temporarily paused.",
			Applies: func(s Subject) bool {
				return s.Velocity.AttemptsLast15Min >= defaultInt(d.DeclineRatioMinAttempts, 20)
			},
			Check: func(s Subject) string {
				pct := defaultInt(d.DeclineRatioPercent, 60)
				if s.Velocity.DeclinesLast15Min*100 <= s.Velocity.AttemptsLast15Min*pct {
					return ""
				}
				return "the recent decline ratio is above " + itoa(pct) + "%"
			},
		},
		{
			ID: "L5.NOT_ON_MERCHANT_BLOCKLIST", Severity: engine.Error,
			Code: string(apierror.CodeRiskDeclined), Field: "/customer", Pure: true,
			Desc:        "the instrument, email, IP and device are not on the merchant's own blocklist",
			Remediation: "This payment was declined by your own risk rules; review your blocklist to change that.",
			Applies:     func(s Subject) bool { return s.Config.MerchantBlocklistConfigured },
			Check: func(s Subject) string {
				if !s.Risk.OnMerchantBlocklist {
					return ""
				}
				return "this payment matches an entry on your merchant blocklist"
			},
		},
		{
			ID: "L5.NOT_ON_PLATFORM_BLOCKLIST", Severity: engine.Error,
			Code: string(apierror.CodeRiskDeclined), Field: "/customer", Pure: true,
			Desc:        "the payment is not on the platform-wide fraud blocklist",
			Remediation: "This payment cannot be processed.",
			Check: func(s Subject) string {
				if !s.Risk.OnPlatformBlocklist {
					return ""
				}
				return "this payment cannot be processed"
			},
		},
		{
			ID: "L5.RISK_SCORE_BELOW_DECLINE_THRESHOLD", Severity: engine.Error,
			Code: string(apierror.CodeRiskDeclined), Field: "/risk", Pure: true,
			Desc:        "the risk score is below the effective decline threshold",
			Remediation: "This payment was declined by risk screening.",
			Applies:     func(s Subject) bool { return s.Risk.Scored },
			Check: func(s Subject) string {
				threshold := s.Config.RiskDeclineAt
				if threshold == 0 {
					threshold = defaultInt(d.RiskDeclineAt, 90)
				}
				if s.Risk.Score < threshold {
					return ""
				}
				return "the risk score is at or above the decline threshold"
			},
		},
		{
			ID: "L5.THREE_DS_REQUIRED_ABOVE_THRESHOLD", Severity: engine.Error,
			Code: string(apierror.CodeThreeDsRequired), Field: "/amount", Pure: true,
			Desc:        "a card payment above the 3-D Secure threshold has authentication or a valid exemption",
			Remediation: "This payment requires 3-D Secure. Complete the returned challenge.",
			Applies: func(s Subject) bool {
				return s.Op == OpCreate && s.Request.Method.RequiresSCAConsideration() &&
					!s.Config.Require3DSAbove.IsZero()
			},
			Check: func(s Subject) string {
				over, err := s.Request.Amount.GreaterThan(s.Config.Require3DSAbove)
				if err != nil || !over {
					return ""
				}
				if s.Request.ThreeDSCompleted {
					return ""
				}
				if s.Request.ClaimedSCAExemption != "" && s.Request.SCAExemptionPreconditionsHold {
					return ""
				}
				return "this payment is above your 3-D Secure threshold of " +
					s.Config.Require3DSAbove.String() + " and has no valid exemption"
			},
		},
		{
			ID: "L5.SCA_EXEMPTION_IS_CLAIMABLE", Severity: engine.Error,
			Code: string(apierror.CodeThreeDsRequired), Field: "/scaExemption", Pure: true,
			Desc:        "the claimed exemption is in the closed set and its preconditions hold",
			Remediation: "The claimed exemption does not apply to this payment; 3-D Secure is required.",
			Applies:     func(s Subject) bool { return s.Request.ClaimedSCAExemption != "" },
			Check: func(s Subject) string {
				claimed := s.Request.ClaimedSCAExemption
				if !containsString(d.ClaimableExemptions, claimed) {
					return quote(claimed) + " is not an exemption this platform can claim"
				}
				if claimed == "LOW_VALUE" && !d.LowValueCeiling.IsZero() {
					over, err := s.Request.Amount.GreaterThan(d.LowValueCeiling)
					if err == nil && over {
						return "the low-value exemption applies only up to " + d.LowValueCeiling.String()
					}
				}
				if !s.Request.SCAExemptionPreconditionsHold {
					return "the preconditions for the " + claimed + " exemption are not met"
				}
				return ""
			},
		},
		{
			ID: "L5.MIT_HAS_INITIAL_REFERENCE", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/initialTransactionId", Pure: true,
			Desc:        "a merchant-initiated transaction carries the initial network transaction reference",
			Remediation: "A merchant-initiated transaction requires the network reference from the initial customer-initiated payment.",
			Applies:     func(s Subject) bool { return s.Request.MerchantInitiated },
			Check: func(s Subject) string {
				if s.Request.InitialTransactionID != "" {
					return ""
				}
				return "no initial transaction reference was supplied"
			},
		},
		{
			ID: "L5.RECURRING_HAS_MANDATE", Severity: engine.Error,
			Code: string(apierror.CodeValidationFailed), Field: "/mandate", Pure: true,
			Desc:        "a recurring payment references an active mandate",
			Remediation: "A recurring payment requires an active mandate.",
			Applies:     func(s Subject) bool { return s.Request.Recurring },
			Check: func(s Subject) string {
				if s.Request.MandateRef == "" {
					return "no mandate reference was supplied"
				}
				if !s.Request.MandateActive {
					return "the referenced mandate is not active"
				}
				return ""
			},
		},
		{
			ID: "L5.PAYMENT_EXISTS", Severity: engine.Error,
			Code: string(apierror.CodePaymentNotFound), Field: "/paymentId", Pure: true,
			Desc:        "the payment resolves within the tenant and merchant",
			Remediation: "Check the payment identifier: it was not found under this merchant.",
			Applies:     onExisting,
			Check: func(s Subject) string {
				if s.Payment != nil && s.Payment.Found {
					return ""
				}
				return "the payment was not found"
			},
		},
		{
			ID: "L5.PAYMENT_STATE_PERMITS_OPERATION", Severity: engine.Error,
			Code: string(apierror.CodePaymentAlreadyProcessed), Field: "/paymentId", Pure: true,
			Desc:        "the payment's current state permits the requested operation",
			Remediation: "This payment's current state does not permit that operation.",
			Applies:     func(s Subject) bool { return s.Op != OpCreate && s.Payment != nil && s.Payment.Found },
			Check: func(s Subject) string {
				st := s.Payment.State
				switch s.Op {
				case OpCapture:
					if st == payment.StateAuthorized {
						return ""
					}
				case OpRefund:
					// Delegated to the payment domain: which states are refundable is the
					// payment context's decision, and duplicating the list here is how the two
					// drift apart on the day SETTLED is added.
					if st.AllowsRefund() {
						return ""
					}
				case OpVoid:
					if st == payment.StateAuthorized {
						return ""
					}
				default:
					// OpCreate never reaches this rule — Applies excludes it — and any operation added without a
					// state precondition falls through to the refusal below, which fails closed
				}
				return "payment is " + string(st) + " and cannot be " + strings.ToLower(string(s.Op)) + "ed"
			},
		},
		{
			ID: "L5.CAPTURE_AMOUNT_WITHIN_AUTHORIZED", Severity: engine.Error,
			Code: string(apierror.CodeAmountExceedsLimit), Field: "/amount", Pure: true,
			Desc:        "captured total plus this capture is within the authorized amount (invariant I2)",
			Remediation: "This capture exceeds the remaining authorized amount.",
			Applies:     func(s Subject) bool { return s.Op == OpCapture && s.Payment != nil && s.Payment.Found },
			Check: func(s Subject) string {
				total, err := s.Payment.CapturedTotal.Add(s.Request.Amount)
				if err != nil {
					return "the capture currency does not match the payment currency"
				}
				over, err := total.GreaterThan(s.Payment.AuthorizedAmount)
				if err != nil {
					return "the capture currency does not match the authorized currency"
				}
				if over {
					remaining, subErr := s.Payment.AuthorizedAmount.Sub(s.Payment.CapturedTotal)
					if subErr != nil {
						return "the capture exceeds the authorized amount"
					}
					return "capture of " + s.Request.Amount.String() +
						" exceeds the remaining authorized amount of " + remaining.String()
				}
				return ""
			},
		},
		{
			ID: "L5.CAPTURE_COUNT_WITHIN_MAX_PARTIALS", Severity: engine.Error,
			// CAPTURE_LIMIT_EXCEEDED, not AMOUNT_EXCEEDS_LIMIT: the amount is irrelevant here,
			// the authorization has simply been drawn against as many times as it may be.
			Code: string(apierror.CodeCaptureLimitExceeded), Field: "/paymentId", Pure: true,
			Desc:        "the capture count is below the configured partial-capture ceiling",
			Remediation: "You have used all the partial captures permitted on this payment.",
			Applies: func(s Subject) bool {
				return s.Op == OpCapture && s.Payment != nil && s.Payment.Found &&
					s.Config.MaxPartialCaptures > 0
			},
			Check: func(s Subject) string {
				if s.Payment.CaptureCount < s.Config.MaxPartialCaptures {
					return ""
				}
				return "all " + itoa(s.Config.MaxPartialCaptures) +
					" permitted partial captures have been used on this payment"
			},
		},
		{
			ID: "L5.CAPTURE_WITHIN_AUTH_VALIDITY", Severity: engine.Error,
			Code: "AUTHORIZATION_EXPIRED", Field: "/paymentId", Pure: true,
			Desc:        "the authorization has not passed its validity window",
			Remediation: "The authorization has expired. Create a new payment.",
			Applies: func(s Subject) bool {
				return s.Op == OpCapture && s.Payment != nil && s.Payment.Found &&
					!s.Payment.AuthorizedAt.IsZero()
			},
			Check: func(s Subject) string {
				days := defaultInt(d.AuthValidityDays, 7)
				expiry := s.Payment.AuthorizedAt.AddDate(0, 0, days)
				if !s.Now.After(expiry) {
					return ""
				}
				return "the authorization expired on " + expiry.UTC().Format("2006-01-02")
			},
		},
		{
			ID: "L5.REFUND_AMOUNT_WITHIN_CAPTURED", Severity: engine.Error,
			Code: string(apierror.CodeRefundExceedsCaptured), Field: "/amount", Pure: true,
			Desc:        "refunded total plus this refund is within the captured total (invariant I1)",
			Remediation: "This refund exceeds the refundable balance on the payment.",
			Applies:     func(s Subject) bool { return s.Op == OpRefund && s.Payment != nil && s.Payment.Found },
			Check: func(s Subject) string {
				total, err := s.Payment.RefundedTotal.Add(s.Request.Amount)
				if err != nil {
					return "the refund currency does not match the payment currency"
				}
				over, err := total.GreaterThan(s.Payment.CapturedTotal)
				if err != nil {
					return "the refund currency does not match the captured currency"
				}
				if over {
					balance, subErr := s.Payment.CapturedTotal.Sub(s.Payment.RefundedTotal)
					if subErr != nil {
						return "the refund exceeds the refundable balance"
					}
					return "refund of " + s.Request.Amount.String() +
						" exceeds the refundable balance of " + balance.String()
				}
				return ""
			},
		},
		{
			ID: "L5.REFUND_CURRENCY_MATCHES_PAYMENT", Severity: engine.Error,
			Code: string(apierror.CodeCurrencyNotSupported), Field: "/currency", Pure: true,
			Desc:        "the refund is in the payment's original currency",
			Remediation: "Refunds must be issued in the payment's original currency.",
			Applies:     func(s Subject) bool { return s.Op == OpRefund && s.Payment != nil && s.Payment.Found },
			Check: func(s Subject) string {
				if s.Request.Amount.Currency() == s.Payment.Currency {
					return ""
				}
				// Cross-currency refunds are a foreign-exchange position the platform has not
				// taken and cannot settle: the original capture converted at one rate and this
				// refund would convert at another.
				return "refunds must be in the original currency " + string(s.Payment.Currency)
			},
		},
		{
			ID: "L5.REFUND_WITHIN_WINDOW", Severity: engine.Error,
			Code: string(apierror.CodeRefundWindowExpired), Field: "/paymentId", Pure: true,
			Desc:        "the refund is inside the configured refund window from capture",
			Remediation: "The refund window for this payment has closed. Issue the refund out of band.",
			Applies: func(s Subject) bool {
				return s.Op == OpRefund && s.Payment != nil && s.Payment.Found &&
					!s.Payment.CapturedAt.IsZero() && s.Config.MaxRefundWindowDays > 0
			},
			Check: func(s Subject) string {
				closes := s.Payment.CapturedAt.AddDate(0, 0, s.Config.MaxRefundWindowDays)
				if !s.Now.After(closes) {
					return ""
				}
				return "the refund window closed on " + closes.UTC().Format("2006-01-02")
			},
		},
		{
			ID: "L5.VOID_ONLY_WHEN_UNCAPTURED", Severity: engine.Error,
			Code: string(apierror.CodePaymentAlreadyProcessed), Field: "/paymentId", Pure: true,
			Desc:        "a void applies only to an authorization with nothing captured",
			Remediation: "This payment has been captured; issue a refund instead of a void.",
			Applies:     func(s Subject) bool { return s.Op == OpVoid && s.Payment != nil && s.Payment.Found },
			Check: func(s Subject) string {
				if s.Payment.CapturedTotal.Amount() != 0 {
					return "this payment has captured funds and cannot be voided"
				}
				if s.Payment.State != payment.StateAuthorized {
					return "payment is " + string(s.Payment.State) + " and is not a voidable authorization"
				}
				return ""
			},
		},
		{
			ID: "L5.NO_OPEN_DISPUTE_BLOCKS_REFUND", Severity: engine.Error,
			Code: string(apierror.CodePaymentAlreadyProcessed), Field: "/paymentId", Pure: true,
			Desc:        "the payment has no open dispute",
			Remediation: "This payment is disputed. Defend or accept the dispute instead of refunding.",
			Applies:     func(s Subject) bool { return s.Op == OpRefund && s.Payment != nil && s.Payment.Found },
			Check: func(s Subject) string {
				// Refunding a disputed payment debits the merchant twice: once for the refund
				// and again when the chargeback settles.
				if !s.Payment.HasOpenDispute {
					return ""
				}
				return "this payment has an open dispute"
			},
		},
		{
			ID: "L5.STATEMENT_DESCRIPTOR_WELL_FORMED", Severity: engine.Warning,
			Code: "", Field: "/statementDescriptor", Pure: true,
			Desc:        "the statement descriptor is 5–22 printable ASCII characters with at least one letter",
			Remediation: "The statement descriptor was normalized to satisfy card scheme rules.",
			Applies:     func(s Subject) bool { return s.Request.StatementDescriptor != "" },
			Check: func(s Subject) string {
				return descriptorProblem(s.Request.StatementDescriptor)
			},
		},
		{
			ID: "L5.METADATA_LOOKS_NON_PII", Severity: engine.Warning,
			Code: "", Field: "/metadata", Pure: true,
			Desc:        "no metadata value looks like an email, phone number, IBAN or national identifier",
			Remediation: "Metadata is not covered by our PII controls; move personal data into the customer object.",
			Applies:     func(s Subject) bool { return len(s.Request.Metadata) > 0 },
			Check: func(s Subject) string {
				// Keys, never values: the point of the rule is that the value looks like
				// personal data, and echoing it into a warning would put it in the same log the
				// rule is trying to keep clean.
				var flagged []string
				for k, v := range s.Request.Metadata {
					if looksLikePII(v) {
						flagged = append(flagged, k)
					}
				}
				if len(flagged) == 0 {
					return ""
				}
				sortStrings(flagged)
				return "metadata key(s) " + strings.Join(flagged, ", ") + " look like personal data"
			},
		},
	}
}

// --- helpers ---------------------------------------------------------------------------------

// descriptorProblem states why a statement descriptor violates scheme rules, or "".
func descriptorProblem(v string) string {
	n := len(v)
	if n < 5 || n > 22 {
		return "the statement descriptor is " + itoa(n) + " characters; schemes require 5 to 22"
	}
	letters := 0
	for i := 0; i < n; i++ {
		c := v[i]
		if c < 0x20 || c > 0x7e {
			return "the statement descriptor contains a non-printable or non-ASCII character"
		}
		switch c {
		case '<', '>', '\\', '\'', '"', '*':
			return "the statement descriptor contains a character schemes reject: " + string(rune(c))
		}
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			letters++
		}
	}
	if letters == 0 {
		return "the statement descriptor contains no letters"
	}
	return ""
}

// looksLikePII applies four cheap structural detectors. It is deliberately structural rather
// than statistical: a warning that fires on ordinary order references is a warning merchants
// turn off.
func looksLikePII(v string) bool {
	s := strings.TrimSpace(v)
	if s == "" {
		return false
	}
	if at := strings.IndexByte(s, '@'); at > 0 && at < len(s)-3 && strings.Contains(s[at:], ".") {
		return true
	}
	digits, plus := 0, strings.HasPrefix(s, "+")
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			digits++
		}
	}
	if plus && digits >= 9 && digits <= 15 {
		return true
	}
	up := strings.ToUpper(strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		return r
	}, s))
	if len(up) >= 15 && len(up) <= 34 && isAlpha(up[0]) && isAlpha(up[1]) && isDigit(up[2]) && isDigit(up[3]) {
		return true
	}
	// US SSN shape: three digits, two digits, four digits, separated.
	parts := strings.Split(s, "-")
	if len(parts) == 3 && len(parts[0]) == 3 && len(parts[1]) == 2 && len(parts[2]) == 4 &&
		allDigits(parts[0]) && allDigits(parts[1]) && allDigits(parts[2]) {
		return true
	}
	return false
}

func isAlpha(c byte) bool { return c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// sortStrings is an insertion sort: the slices here are single digits long, and pulling in
// sort for them costs more than it saves.
func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func containsString(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func containsCurrency(set []money.Currency, v money.Currency) bool {
	for _, c := range set {
		if c == v {
			return true
		}
	}
	return false
}

func containsMethod(set []shared.PaymentMethod, v shared.PaymentMethod) bool {
	for _, m := range set {
		if m == v {
			return true
		}
	}
	return false
}

func containsCountry(set []shared.Country, v shared.Country) bool {
	for _, c := range set {
		if c == v {
			return true
		}
	}
	return false
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func quote(s string) string {
	if s == "" {
		return "(empty)"
	}
	return "`" + s + "`"
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
