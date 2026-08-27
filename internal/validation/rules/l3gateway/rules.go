package l3gateway

import (
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/validation/engine"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/internal/ruledef"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

func init() {
	ruledef.Register(defs(DefaultDeps()), "gateway-integration", "2026-01-01", engine.Enforce)
}

// Rules returns the L3 rule set.
//
// ShortCircuit, in dependency order: credentials → connectivity → account → capability →
// webhook → version. Each stage is meaningless without the one before it — asking whether the
// connected account has charges enabled is not a question you can answer with credentials that
// do not authenticate — and each stage that does run is a network round trip somebody pays
// for.
func Rules(d Deps) engine.RuleSet[Subject] {
	return engine.RuleSet[Subject]{
		Name:  "L3.gateway",
		Mode:  engine.ShortCircuit,
		Rules: ruledef.Build(defs(d)),
	}
}

func defs(d Deps) []ruledef.Def[Subject] {
	resolved := func(s Subject) bool { return s.Credentials.Resolved }
	registered := func(s Subject) bool { return s.Probe.WebhookEndpointRegistered }
	pinned := func(s Subject) bool { return s.Connection.PinnedAPIVersion != "" }
	hasConfig := func(s Subject) bool { return s.Config.Present }

	return []ruledef.Def[Subject]{
		{
			ID: "L3.CREDENTIALS_PRESENT", Severity: engine.Error,
			Code: string(apierror.CodeConfigurationInvalid), Field: "/connection/credentialRef", Pure: true,
			Desc:        "a credential reference exists for (tenant, merchant, gateway, environment)",
			Remediation: "No credentials are stored for this gateway. Re-run provisioning.",
			Check: func(s Subject) string {
				if s.Credentials.ReferencePresent && s.Credentials.Reference != "" {
					return ""
				}
				return "no credential reference is stored for " + string(s.Connection.GatewayID)
			},
		},
		{
			ID: "L3.CREDENTIAL_REFERENCE_RESOLVES", Severity: engine.Error,
			Code: string(apierror.CodeServiceUnavailable), Field: "/connection/credentialRef", Pure: false,
			Desc:        "Secrets Manager returned a current version and the envelope decrypted",
			Remediation: "Credential material could not be retrieved. This is a platform issue; it has been raised.",
			Applies:     func(s Subject) bool { return s.Credentials.ReferencePresent },
			Check: func(s Subject) string {
				if s.Credentials.Resolved {
					return ""
				}
				return "the stored credential reference did not resolve"
			},
		},
		{
			ID: "L3.CREDENTIAL_NOT_EXPIRED", Severity: engine.Error,
			Code: "GATEWAY_CREDENTIAL_EXPIRED", Field: "/connection/credentialRef", Pure: true,
			Desc:        "the credential has not passed its expiry, where the gateway exposes one",
			Remediation: "The gateway credentials have expired. Rotation has been triggered; re-authorize the connection if it does not complete.",
			Applies:     func(s Subject) bool { return s.Credentials.Resolved && !s.Credentials.ExpiresAt.IsZero() },
			Check: func(s Subject) string {
				if s.Credentials.ExpiresAt.After(s.Now) {
					return ""
				}
				return string(s.Connection.GatewayID) + " credentials expired on " +
					s.Credentials.ExpiresAt.UTC().Format("2006-01-02")
			},
		},
		{
			ID: "L3.CREDENTIAL_ROTATION_NOT_OVERDUE", Severity: engine.Warning,
			Code: "", Field: "/connection/credentialRef", Pure: true,
			Desc:        "the credential is younger than the rotation SLA",
			Remediation: "These gateway credentials are overdue for rotation; rotation has been scheduled.",
			Applies:     func(s Subject) bool { return s.Credentials.Resolved && !s.Credentials.IssuedAt.IsZero() },
			Check: func(s Subject) string {
				maxAge := defaultInt(d.MaxCredentialAgeDays, 90)
				age := int(s.Now.Sub(s.Credentials.IssuedAt).Hours() / 24)
				if age <= maxAge {
					return ""
				}
				return string(s.Connection.GatewayID) + " credentials are " + itoa(age) +
					" days old; rotation is due at " + itoa(maxAge)
			},
		},
		{
			ID: "L3.CREDENTIALS_AUTHENTICATE", Severity: engine.Error,
			Code: "GATEWAY_AUTH_FAILED", Field: "/connection", Pure: false,
			Desc:        "a read-only gateway call authenticated successfully",
			Remediation: "The gateway rejected our credentials. Re-authorize the connection.",
			Applies:     resolved,
			Check: func(s Subject) string {
				if s.Probe.Authenticated {
					return ""
				}
				return string(s.Connection.GatewayID) + " rejected the stored credentials"
			},
		},
		{
			ID: "L3.CREDENTIAL_SCOPES_SUFFICIENT", Severity: engine.Error,
			Code: "GATEWAY_AUTH_FAILED", Field: "/connection", Pure: false,
			Desc:        "the granted scopes cover charges, refunds, webhooks and account read",
			Remediation: "The gateway connection lacks a required permission. Re-authorize with full permissions.",
			Applies:     func(s Subject) bool { return s.Probe.Authenticated },
			Check: func(s Subject) string {
				missing := missingFrom(d.RequiredScopes, s.Probe.GrantedScopes)
				if len(missing) == 0 {
					return ""
				}
				return "the connection is missing the " + strings.Join(missing, ", ") + " permission(s)"
			},
		},
		{
			ID: "L3.TLS_HANDSHAKE_SUCCEEDS", Severity: engine.Error,
			Code: string(apierror.CodeServiceUnavailable), Field: "/connection", Pure: false,
			Desc:        "TLS 1.2+ to the gateway host, chaining to the pinned root set",
			Remediation: "We cannot establish a secure connection to this gateway. It has been raised with the platform team.",
			Applies:     func(s Subject) bool { return s.Probe.Attempted },
			Check: func(s Subject) string {
				if s.Probe.TLSHandshakeOK {
					return ""
				}
				return "the TLS handshake with " + string(s.Connection.GatewayID) + " failed"
			},
		},
		{
			ID: "L3.PROBE_LATENCY_WITHIN_BUDGET", Severity: engine.Warning,
			Code: "", Field: "/connection", Pure: false,
			Desc:        "p95 probe latency over the recent window is within budget",
			Remediation: "This gateway is responding slowly; its routing weight has been reduced.",
			Applies: func(s Subject) bool {
				return s.Probe.Attempted && s.Probe.SampleSize >= defaultInt(d.MinProbeSamples, 20)
			},
			Check: func(s Subject) string {
				budget := defaultInt(d.ProbeLatencyBudgetMillis, 1500)
				if s.Probe.P95LatencyMillis <= budget {
					return ""
				}
				return "p95 probe latency is " + itoa(s.Probe.P95LatencyMillis) +
					" ms against a budget of " + itoa(budget) + " ms"
			},
		},
		{
			ID: "L3.ACCOUNT_REFERENCE_EXISTS", Severity: engine.Error,
			Code: "GATEWAY_ACCOUNT_MISSING", Field: "/connection/externalAccountRef", Pure: false,
			Desc:        "the stored sub-merchant or connected-account reference resolves at the gateway",
			Remediation: "The gateway account for this merchant no longer exists. Re-provisioning is required.",
			Applies:     func(s Subject) bool { return s.Connection.Provisioned },
			Check: func(s Subject) string {
				if s.Probe.AccountResolved {
					return ""
				}
				return "the stored account reference does not resolve at " + string(s.Connection.GatewayID)
			},
		},
		{
			ID: "L3.ACCOUNT_CHARGES_ENABLED", Severity: engine.Error,
			Code: "GATEWAY_ACCOUNT_RESTRICTED", Field: "/connection/externalAccountRef", Pure: false,
			Desc:        "the gateway has charges enabled for this account",
			Remediation: "The gateway has not enabled charges for this account. Complete the outstanding requirements.",
			Applies:     func(s Subject) bool { return s.Probe.AccountResolved },
			Check: func(s Subject) string {
				if s.Probe.ChargesEnabled {
					return ""
				}
				return string(s.Connection.GatewayID) + " has not enabled charges for this account"
			},
		},
		{
			ID: "L3.ACCOUNT_PAYOUTS_ENABLED", Severity: engine.Warning,
			Code: "", Field: "/connection/externalAccountRef", Pure: false,
			Desc:        "the gateway has payouts enabled for this account",
			Remediation: "The gateway has not enabled payouts yet; you can process but not settle.",
			Applies:     func(s Subject) bool { return s.Probe.AccountResolved },
			Check: func(s Subject) string {
				if s.Probe.PayoutsEnabled {
					return ""
				}
				return string(s.Connection.GatewayID) + " has not enabled payouts for this account"
			},
		},
		{
			ID: "L3.ACCOUNT_HAS_NO_OPEN_REQUIREMENTS", Severity: engine.Error,
			Code: "GATEWAY_ACCOUNT_RESTRICTED", Field: "/connection/externalAccountRef", Pure: false,
			Desc:        "the gateway reports no currently-due account requirements",
			Remediation: "The gateway still requires outstanding information. Provide it to complete activation.",
			Applies:     func(s Subject) bool { return s.Probe.AccountResolved },
			Check: func(s Subject) string {
				if len(s.Probe.CurrentlyDue) == 0 {
					return ""
				}
				return string(s.Connection.GatewayID) + " still requires: " +
					strings.Join(s.Probe.CurrentlyDue, ", ")
			},
		},
		{
			ID: "L3.DESCRIPTOR_COVERS_CURRENCIES", Severity: engine.Error,
			Code: string(apierror.CodeCurrencyNotSupported), Field: "/configuration/supportedCurrencies", Pure: true,
			Desc:        "every configured currency is in the descriptor's currency set",
			Remediation: "This gateway cannot process one of your enabled currencies for this account's country. Remove the currency or add another gateway.",
			Applies:     hasConfig,
			Check: func(s Subject) string {
				for _, c := range s.Config.Currencies {
					if !containsCurrency(s.Descriptor.Currencies, c) {
						return string(s.Connection.GatewayID) + " cannot process " + string(c)
					}
				}
				return ""
			},
		},
		{
			ID: "L3.DESCRIPTOR_COVERS_METHODS", Severity: engine.Error,
			Code: string(apierror.CodePaymentMethodNotSupported), Field: "/configuration/paymentMethods", Pure: true,
			Desc:        "every configured payment method is in the descriptor's method set",
			Remediation: "This gateway does not support one of your enabled payment methods.",
			Applies:     hasConfig,
			Check: func(s Subject) string {
				for _, m := range s.Config.Methods {
					if !containsMethod(s.Descriptor.Methods, m) {
						return string(s.Connection.GatewayID) + " does not support " + string(m)
					}
				}
				return ""
			},
		},
		{
			ID: "L3.DESCRIPTOR_COVERS_COUNTRIES", Severity: engine.Error,
			Code: "COUNTRY_NOT_SUPPORTED", Field: "/configuration/countries", Pure: true,
			Desc:        "every configured country is in the descriptor's country set",
			Remediation: "This gateway cannot accept payments from one of your enabled countries.",
			Applies:     hasConfig,
			Check: func(s Subject) string {
				for _, c := range s.Config.Countries {
					if !containsCountry(s.Descriptor.Countries, c) {
						return string(s.Connection.GatewayID) + " cannot accept payments from " + string(c)
					}
				}
				return ""
			},
		},
		{
			ID: "L3.DESCRIPTOR_SUPPORTS_OPERATIONS", Severity: engine.Error,
			Code: string(apierror.CodeConfigurationInvalid), Field: "/descriptor/operations", Pure: true,
			Desc:        "the descriptor declares authorize, capture, refund, void and lookup",
			Remediation: "This gateway does not expose every operation a primary route requires.",
			Check: func(s Subject) string {
				for _, op := range d.RequiredOperations {
					if !containsOperation(s.Descriptor.Operations, op) {
						return string(s.Connection.GatewayID) + " does not expose " + string(op)
					}
				}
				return ""
			},
		},
		{
			ID: "L3.DESCRIPTOR_SUPPORTS_3DS_WHEN_REQUIRED", Severity: engine.Error,
			Code: string(apierror.CodeConfigurationInvalid), Field: "/descriptor/threeDS", Pure: true,
			Desc:        "3-D Secure 2.x is declared for every enabled corridor when the risk policy requires it",
			Remediation: "Your risk policy requires 3-D Secure, which this gateway does not support for one of your corridors.",
			Applies:     func(s Subject) bool { return s.Config.Present && s.Config.Requires3DS },
			Check: func(s Subject) string {
				if !s.Descriptor.Supports3DS {
					return string(s.Connection.GatewayID) + " does not support 3-D Secure"
				}
				for _, c := range s.Config.ThreeDSCorridors {
					if !containsCountry(s.Descriptor.ThreeDSCorridors, c) {
						return string(s.Connection.GatewayID) + " does not support 3-D Secure for " + string(c)
					}
				}
				return ""
			},
		},
		{
			ID: "L3.DESCRIPTOR_PARTIAL_CAPTURE_MATCHES", Severity: engine.Error,
			Code: string(apierror.CodeConfigurationInvalid), Field: "/descriptor/partialCapture", Pure: true,
			Desc:        "the descriptor declares partial capture and a ceiling at or above the configured one",
			Remediation: "This gateway supports fewer partial captures than your configuration requires.",
			Applies:     func(s Subject) bool { return s.Config.Present && s.Config.PartialCaptureEnabled },
			Check: func(s Subject) string {
				if !s.Descriptor.SupportsPartialCapture {
					return string(s.Connection.GatewayID) + " does not support partial capture"
				}
				if s.Descriptor.MaxPartialCaptures < s.Config.MaxPartialCaptures {
					return string(s.Connection.GatewayID) + " supports at most " +
						itoa(s.Descriptor.MaxPartialCaptures) + " partial captures; the configuration asks for " +
						itoa(s.Config.MaxPartialCaptures)
				}
				return ""
			},
		},
		{
			ID: "L3.DESCRIPTOR_REFUND_WINDOW_COVERS_CONFIG", Severity: engine.Error,
			Code: string(apierror.CodeConfigurationInvalid), Field: "/descriptor/refundWindowDays", Pure: true,
			Desc:        "the descriptor's refund window is at least the configured maximum",
			Remediation: "This gateway allows refunds for fewer days than your policy asks for.",
			Applies:     func(s Subject) bool { return s.Config.Present && s.Config.MaxRefundWindowDays > 0 },
			Check: func(s Subject) string {
				if s.Descriptor.RefundWindowDays >= s.Config.MaxRefundWindowDays {
					return ""
				}
				return string(s.Connection.GatewayID) + " allows refunds for " +
					itoa(s.Descriptor.RefundWindowDays) + " days; your policy asks for " +
					itoa(s.Config.MaxRefundWindowDays)
			},
		},
		{
			ID: "L3.WEBHOOK_ENDPOINT_REGISTERED", Severity: engine.Error,
			Code: "WEBHOOK_NOT_REGISTERED", Field: "/connection/webhookEndpoint", Pure: false,
			Desc:        "the gateway lists our endpoint URL for this account",
			Remediation: "Webhook registration is missing at the gateway; it has been re-queued.",
			Applies:     func(s Subject) bool { return s.Connection.Provisioned },
			Check: func(s Subject) string {
				if s.Probe.WebhookEndpointRegistered {
					return ""
				}
				return "no webhook endpoint is registered at " + string(s.Connection.GatewayID)
			},
		},
		{
			ID: "L3.WEBHOOK_URL_IS_HTTPS_AND_PUBLIC", Severity: engine.Error,
			Code: string(apierror.CodeConfigurationInvalid), Field: "/connection/webhookEndpoint", Pure: true,
			Desc: "the registered webhook URL is https, publicly resolvable, not RFC 1918, and " +
				"matches this environment's ingress host",
			Remediation: "The webhook URL must be the platform's public HTTPS ingress.",
			Applies:     registered,
			Check: func(s Subject) string {
				return webhookURLProblem(s.Connection.WebhookEndpoint, d.IngressHost)
			},
		},
		{
			ID: "L3.WEBHOOK_SECRET_STORED", Severity: engine.Error,
			Code: string(apierror.CodeConfigurationInvalid), Field: "/connection/webhookSecret", Pure: false,
			Desc:        "a signing secret exists and its fingerprint matches what the gateway reports",
			Remediation: "The webhook signing secret is missing or stale; re-register the endpoint.",
			Applies:     registered,
			Check: func(s Subject) string {
				stored := s.Connection.StoredWebhookSecretFingerprint
				if stored == "" {
					return "no webhook signing secret is stored"
				}
				if s.Probe.WebhookSecretFingerprint != "" && s.Probe.WebhookSecretFingerprint != stored {
					// A mismatch means every webhook this gateway sends will fail signature
					// verification, which presents as silent data loss rather than as an error.
					return "the stored webhook secret does not match the one the gateway holds"
				}
				return ""
			},
		},
		{
			ID: "L3.WEBHOOK_SUBSCRIPTION_COMPLETE", Severity: engine.Error,
			Code: "WEBHOOK_SUBSCRIPTION_INCOMPLETE", Field: "/connection/webhookEndpoint", Pure: false,
			Desc:        "the subscribed event types cover the adapter's required set",
			Remediation: "The webhook subscription is missing required events; it has been re-queued.",
			Applies:     registered,
			Check: func(s Subject) string {
				missing := missingFrom(d.RequiredWebhookEvents, s.Probe.SubscribedEvents)
				if len(missing) == 0 {
					return ""
				}
				return "the webhook subscription is missing: " + strings.Join(missing, ", ")
			},
		},
		{
			ID: "L3.WEBHOOK_SIGNATURE_SCHEME_SUPPORTED", Severity: engine.Error,
			Code: string(apierror.CodeConfigurationInvalid), Field: "/descriptor/signatureScheme", Pure: true,
			Desc:        "the gateway's webhook signature scheme is one the adapters implement",
			Remediation: "This gateway uses a webhook signature scheme the platform does not implement.",
			Applies:     registered,
			Check: func(s Subject) string {
				scheme := s.Descriptor.SignatureScheme
				if !scheme.IsValid() {
					return "unsupported webhook signature scheme " + quote(string(scheme))
				}
				// An unsigned webhook is an unauthenticated instruction to change money state.
				// Sandbox may accept it for integration work; production may not.
				if scheme == gateway.SchemeNone && s.Connection.Environment.IsProduction() {
					return "this gateway does not sign webhooks, which is not permitted in production"
				}
				return ""
			},
		},
		{
			ID: "L3.API_VERSION_PINNED", Severity: engine.Error,
			Code: string(apierror.CodeConfigurationInvalid), Field: "/connection/apiVersion", Pure: true,
			Desc:        "a gateway API version is pinned on the connection",
			Remediation: "No API version is pinned for this gateway; provisioning is incomplete.",
			Check: func(s Subject) string {
				if s.Connection.PinnedAPIVersion != "" {
					return ""
				}
				return "no API version is pinned for " + string(s.Connection.GatewayID)
			},
		},
		{
			ID: "L3.API_VERSION_SUPPORTED_BY_ADAPTER", Severity: engine.Error,
			Code: string(apierror.CodeConfigurationInvalid), Field: "/connection/apiVersion", Pure: true,
			Desc:        "the pinned version is in the adapter's supported set",
			Remediation: "The adapter does not support the pinned gateway API version.",
			Applies:     pinned,
			Check: func(s Subject) string {
				supported := s.Descriptor.SupportedAPIVersions
				if len(supported) == 0 {
					supported = d.AdapterVersions[s.Connection.GatewayID]
				}
				if containsString(supported, s.Connection.PinnedAPIVersion) {
					return ""
				}
				return "the adapter does not support " + string(s.Connection.GatewayID) +
					" API version " + quote(s.Connection.PinnedAPIVersion)
			},
		},
		{
			ID: "L3.API_VERSION_NOT_DEPRECATED", Severity: engine.Warning,
			Code: "", Field: "/connection/apiVersion", Pure: false,
			Desc:        "the gateway is not signalling deprecation or sunset for the pinned version",
			Remediation: "This gateway API version is deprecated; an upgrade has been scheduled.",
			Applies:     func(s Subject) bool { return s.Connection.PinnedAPIVersion != "" && s.Probe.Attempted },
			Check: func(s Subject) string {
				if !s.Probe.DeprecationSignaled {
					return ""
				}
				msg := string(s.Connection.GatewayID) + " API " + s.Connection.PinnedAPIVersion +
					" is deprecated"
				if s.Probe.SunsetDate != "" {
					msg += " (sunset " + s.Probe.SunsetDate + ")"
				}
				return msg
			},
		},
		{
			ID: "L3.CERTIFICATION_REPORT_PASSING", Severity: engine.Error,
			Code: "CERTIFICATION_REQUIRED", Field: "/connection/certification", Pure: true,
			Desc:        "a signed certification report exists, all assertions pass, and it is not older than the SLA",
			Remediation: "This connection must be re-certified before it can carry production traffic.",
			Applies: func(s Subject) bool {
				return s.Connection.CertificationStatus == gateway.CertificationPassed
			},
			Check: func(s Subject) string {
				maxAge := defaultInt(d.CertificationMaxAgeDays, 180)
				switch {
				case s.Connection.CertificationReportID == "":
					return "no certification report is on file"
				case !s.Connection.CertificationAssertionsPassed:
					return "the certification report has failing assertions"
				case s.Connection.CertifiedAt.IsZero():
					return "the certification report carries no date"
				case s.Connection.CertifiedAt.AddDate(0, 0, maxAge).Before(s.Now):
					return "the certification report is older than " + itoa(maxAge) + " days"
				}
				return ""
			},
		},
	}
}

// --- helpers ---------------------------------------------------------------------------------

// webhookURLProblem states why a registered webhook URL is unusable, or "".
//
// The private-range check exists because a webhook URL pointing at an RFC 1918 address is
// either a misconfiguration that silently drops every event, or an SSRF pivot; both are
// resolved by requiring the platform's own ingress host.
func webhookURLProblem(raw, ingressHost string) string {
	if !strings.HasPrefix(strings.ToLower(raw), "https://") {
		return "the webhook URL is not https"
	}
	host := raw[len("https://"):]
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	switch {
	case host == "":
		return "the webhook URL has no host"
	case isPrivateHost(host):
		return "the webhook URL points at a private or loopback address"
	case ingressHost != "" && !strings.EqualFold(host, ingressHost):
		return "the webhook URL host is not this environment's ingress host"
	}
	return ""
}

func isPrivateHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	nums := make([]int, 4)
	for i, p := range parts {
		n, ok := atoi(p)
		if !ok {
			return false
		}
		nums[i] = n
	}
	switch {
	case nums[0] == 10, nums[0] == 127:
		return true
	case nums[0] == 192 && nums[1] == 168:
		return true
	case nums[0] == 172 && nums[1] >= 16 && nums[1] <= 31:
		return true
	case nums[0] == 169 && nums[1] == 254:
		return true
	}
	return false
}

func atoi(s string) (int, bool) {
	if s == "" || len(s) > 3 {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

// missingFrom returns the elements of want that are absent from got, case-insensitively.
func missingFrom(want, got []string) []string {
	var missing []string
	for _, w := range want {
		found := false
		for _, g := range got {
			if strings.EqualFold(w, g) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, w)
		}
	}
	return missing
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

func containsOperation(set []shared.Operation, v shared.Operation) bool {
	for _, o := range set {
		if o == v {
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

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
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
