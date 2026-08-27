package l3gateway_test

import (
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/internal/ruletest"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/l3gateway"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

var now = time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)

func deps() l3gateway.Deps {
	d := l3gateway.DefaultDeps()
	d.IngressHost = "webhooks.payments-platform.test"
	d.AdapterVersions = map[shared.GatewayID][]string{"stripe": {"2026-01-15"}}
	return d
}

// base is a certified, healthy production connection: credentials resolve and authenticate, the
// account is live, the descriptor covers the configuration, the webhook is registered with a
// matching secret, and the pinned API version is one the adapter implements.
func base() l3gateway.Subject {
	return l3gateway.Subject{
		Connection: l3gateway.Connection{
			GatewayID:                      "stripe",
			Environment:                    shared.EnvironmentProduction,
			Status:                         gateway.StatusCertified,
			CertificationStatus:            gateway.CertificationPassed,
			Provisioned:                    true,
			ExternalAccountRef:             "acct_live_1",
			PinnedAPIVersion:               "2026-01-15",
			WebhookEndpoint:                "https://webhooks.payments-platform.test/v1/gw/stripe",
			StoredWebhookSecretFingerprint: "sha256:abc",
			CertificationReportID:          "cert_1",
			CertificationAssertionsPassed:  true,
			CertifiedAt:                    now.AddDate(0, 0, -30),
		},
		Descriptor: l3gateway.Descriptor{
			GatewayID:              "stripe",
			Currencies:             []money.Currency{"USD", "EUR", "GBP"},
			Methods:                []shared.PaymentMethod{shared.MethodCard, shared.MethodSEPADebit},
			Countries:              []shared.Country{"US", "DE", "GB"},
			Operations:             []shared.Operation{shared.OpAuthorize, shared.OpCapture, shared.OpRefund, shared.OpVoid, shared.OpLookup},
			Supports3DS:            true,
			ThreeDSCorridors:       []shared.Country{"DE", "GB"},
			SupportsPartialCapture: true,
			MaxPartialCaptures:     4,
			RefundWindowDays:       180,
			SignatureScheme:        gateway.SchemeHMACSHA256,
			SupportedAPIVersions:   []string{"2026-01-15", "2025-09-01"},
			ProcessingRegion:       "eu-west-1",
		},
		Credentials: l3gateway.Credentials{
			ReferencePresent: true,
			Reference:        "secret://tenants/ten_1/gateways/stripe",
			Resolved:         true,
			IssuedAt:         now.AddDate(0, 0, -10),
			ExpiresAt:        now.AddDate(1, 0, 0),
		},
		Probe: l3gateway.Probe{
			Attempted:                 true,
			TLSHandshakeOK:            true,
			Authenticated:             true,
			GrantedScopes:             []string{"charges", "refunds", "webhooks", "account_read"},
			P95LatencyMillis:          320,
			SampleSize:                50,
			AccountResolved:           true,
			ChargesEnabled:            true,
			PayoutsEnabled:            true,
			WebhookEndpointRegistered: true,
			WebhookSecretFingerprint:  "sha256:abc",
			SubscribedEvents:          []string{"auth", "capture", "refund", "dispute", "payout"},
		},
		Config: l3gateway.MerchantConfigView{
			Present:               true,
			Currencies:            []money.Currency{"EUR", "GBP"},
			Methods:               []shared.PaymentMethod{shared.MethodCard},
			Countries:             []shared.Country{"DE", "GB"},
			Requires3DS:           true,
			ThreeDSCorridors:      []shared.Country{"DE"},
			PartialCaptureEnabled: true,
			MaxPartialCaptures:    3,
			MaxRefundWindowDays:   120,
		},
		Now: now,
	}
}

func TestL3Rules(t *testing.T) {
	t.Parallel()
	set := l3gateway.Rules(deps())

	ruletest.Run(t, set, base, []ruletest.Case[l3gateway.Subject]{
		{
			ID:   "L3.CREDENTIALS_PRESENT",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) { s.Credentials.Reference = "" },
		},
		{
			ID:   "L3.CREDENTIAL_REFERENCE_RESOLVES",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) { s.Credentials.Resolved = false },
		},
		{
			ID:   "L3.CREDENTIAL_NOT_EXPIRED",
			Pass: func(s *l3gateway.Subject) { s.Credentials.ExpiresAt = s.Now.Add(time.Minute) },
			Fail: func(s *l3gateway.Subject) { s.Credentials.ExpiresAt = s.Now.Add(-time.Minute) },
		},
		{
			ID:   "L3.CREDENTIAL_ROTATION_NOT_OVERDUE",
			Pass: func(s *l3gateway.Subject) { s.Credentials.IssuedAt = s.Now.AddDate(0, 0, -90) },
			Fail: func(s *l3gateway.Subject) { s.Credentials.IssuedAt = s.Now.AddDate(0, 0, -91) },
		},
		{
			ID:   "L3.CREDENTIALS_AUTHENTICATE",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) { s.Probe.Authenticated = false },
		},
		{
			ID:   "L3.CREDENTIAL_SCOPES_SUFFICIENT",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) {
				s.Probe.GrantedScopes = []string{"charges", "account_read"}
			},
		},
		{
			ID:   "L3.TLS_HANDSHAKE_SUCCEEDS",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) { s.Probe.TLSHandshakeOK = false },
		},
		{
			ID:   "L3.PROBE_LATENCY_WITHIN_BUDGET",
			Pass: func(s *l3gateway.Subject) { s.Probe.P95LatencyMillis = 1500 },
			Fail: func(s *l3gateway.Subject) { s.Probe.P95LatencyMillis = 1501 },
		},
		{
			ID:   "L3.ACCOUNT_REFERENCE_EXISTS",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) { s.Probe.AccountResolved = false },
		},
		{
			ID:   "L3.ACCOUNT_CHARGES_ENABLED",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) { s.Probe.ChargesEnabled = false },
		},
		{
			ID:   "L3.ACCOUNT_PAYOUTS_ENABLED",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) { s.Probe.PayoutsEnabled = false },
		},
		{
			ID:   "L3.ACCOUNT_HAS_NO_OPEN_REQUIREMENTS",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) {
				s.Probe.CurrentlyDue = []string{"business.tax_id", "representative.id_document"}
			},
		},
		{
			ID:   "L3.DESCRIPTOR_COVERS_CURRENCIES",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) {
				s.Config.Currencies = []money.Currency{"EUR", "JPY"}
			},
		},
		{
			ID:   "L3.DESCRIPTOR_COVERS_METHODS",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) {
				s.Config.Methods = []shared.PaymentMethod{shared.MethodCard, shared.MethodIdeal}
			},
		},
		{
			ID:   "L3.DESCRIPTOR_COVERS_COUNTRIES",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) {
				s.Config.Countries = []shared.Country{"DE", "FR"}
			},
		},
		{
			ID:   "L3.DESCRIPTOR_SUPPORTS_OPERATIONS",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) {
				s.Descriptor.Operations = []shared.Operation{shared.OpAuthorize, shared.OpCapture}
			},
		},
		{
			ID:   "L3.DESCRIPTOR_SUPPORTS_3DS_WHEN_REQUIRED",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) { s.Descriptor.Supports3DS = false },
		},
		{
			ID:   "L3.DESCRIPTOR_PARTIAL_CAPTURE_MATCHES",
			Pass: func(s *l3gateway.Subject) { s.Config.MaxPartialCaptures = 4 },
			Fail: func(s *l3gateway.Subject) { s.Config.MaxPartialCaptures = 5 },
		},
		{
			ID:   "L3.DESCRIPTOR_REFUND_WINDOW_COVERS_CONFIG",
			Pass: func(s *l3gateway.Subject) { s.Config.MaxRefundWindowDays = 180 },
			Fail: func(s *l3gateway.Subject) { s.Config.MaxRefundWindowDays = 181 },
		},
		{
			ID:   "L3.WEBHOOK_ENDPOINT_REGISTERED",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) { s.Probe.WebhookEndpointRegistered = false },
		},
		{
			ID:   "L3.WEBHOOK_URL_IS_HTTPS_AND_PUBLIC",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) {
				s.Connection.WebhookEndpoint = "https://10.0.0.5/v1/gw/stripe"
			},
		},
		{
			ID:   "L3.WEBHOOK_SECRET_STORED",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) { s.Probe.WebhookSecretFingerprint = "sha256:rotated" },
		},
		{
			ID:   "L3.WEBHOOK_SUBSCRIPTION_COMPLETE",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) {
				s.Probe.SubscribedEvents = []string{"auth", "capture"}
			},
		},
		{
			ID:   "L3.WEBHOOK_SIGNATURE_SCHEME_SUPPORTED",
			Pass: func(s *l3gateway.Subject) { s.Descriptor.SignatureScheme = gateway.SchemeECDSAP256 },
			Fail: func(s *l3gateway.Subject) { s.Descriptor.SignatureScheme = gateway.SchemeNone },
		},
		{
			ID:   "L3.API_VERSION_PINNED",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) { s.Connection.PinnedAPIVersion = "" },
		},
		{
			ID:   "L3.API_VERSION_SUPPORTED_BY_ADAPTER",
			Pass: func(s *l3gateway.Subject) { s.Connection.PinnedAPIVersion = "2025-09-01" },
			Fail: func(s *l3gateway.Subject) { s.Connection.PinnedAPIVersion = "2019-01-01" },
		},
		{
			ID:   "L3.API_VERSION_NOT_DEPRECATED",
			Pass: func(s *l3gateway.Subject) {},
			Fail: func(s *l3gateway.Subject) {
				s.Probe.DeprecationSignaled = true
				s.Probe.SunsetDate = "2026-12-31"
			},
		},
		{
			ID:   "L3.CERTIFICATION_REPORT_PASSING",
			Pass: func(s *l3gateway.Subject) { s.Connection.CertifiedAt = s.Now.AddDate(0, 0, -179) },
			Fail: func(s *l3gateway.Subject) { s.Connection.CertifiedAt = s.Now.AddDate(0, 0, -181) },
		},
	})
}

// TestL3ShortCircuitsOnDeadCredentials is the reason this level is ShortCircuit: probing a
// gateway twelve more times with credentials that just failed buys nothing.
func TestL3ShortCircuitsOnDeadCredentials(t *testing.T) {
	t.Parallel()
	s := base()
	s.Probe.Authenticated = false

	rep := l3gateway.Rules(deps()).Evaluate(t.Context(), s)

	if rep.OK() {
		t.Fatal("the connection passed with credentials the gateway rejected")
	}
	if _, ran := rep.For("L3.ACCOUNT_CHARGES_ENABLED"); ran {
		t.Fatal("account rules ran after the authentication failure")
	}
	if got := len(rep.Errors()); got != 1 {
		t.Fatalf("ShortCircuit produced %d errors, want exactly the first", got)
	}
}

// TestL3NoRuleReachesTheNetwork: every impure rule here reads a probe result that was captured
// before evaluation. Evaluating the whole set against a subject with no probe attempted must
// therefore still terminate and produce a report.
func TestL3NoRuleReachesTheNetwork(t *testing.T) {
	t.Parallel()
	s := base()
	s.Probe = l3gateway.Probe{}

	rep := l3gateway.Rules(deps()).Evaluate(t.Context(), s)
	if len(rep.Outcomes) == 0 {
		t.Fatal("evaluating without a probe result produced no outcomes at all")
	}
}
