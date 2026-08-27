// Package l3gateway is validation level 3: whether one merchant's connection to one gateway
// actually works — credentials, connectivity, account state, capability coverage, webhook
// registration and API version.
//
// L3 is the only level that is impure by definition, and it is the only level that never runs
// on the payment hot path. It runs during onboarding, during credential rotation, and on a
// five-minute scheduled probe; a failure fails an onboarding step or marks the connection
// UNHEALTHY, and the router stops selecting it. Putting these checks in front of a payment
// instead would mean every authorization waited on a gateway round trip to find out whether a
// gateway round trip would work.
//
// Even here, no rule performs I/O. The probe worker makes the calls and records what happened
// in Probe; the rules assert on that record. That is what keeps them unit-testable with no
// network and what makes a five-minute-old health verdict reconstructible from the stored
// probe result. Mode is ShortCircuit: after CREDENTIALS_AUTHENTICATE fails there is nothing to
// learn from twelve more calls with the same dead credentials.
//
// See docs/validation-plane.md §3.3.
package l3gateway

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Descriptor is the gateway's declared capability surface, snapshotted from the registry.
type Descriptor struct {
	GatewayID  shared.GatewayID
	Currencies []money.Currency
	Methods    []shared.PaymentMethod
	Countries  []shared.Country
	Operations []shared.Operation
	// Supports3DS and ThreeDSCorridors record 3-D Secure 2.x support and where it applies.
	Supports3DS      bool
	ThreeDSCorridors []shared.Country
	// PartialCapture support and its ceiling.
	SupportsPartialCapture bool
	MaxPartialCaptures     int
	// RefundWindowDays is how long after capture the gateway still accepts a refund.
	RefundWindowDays int
	// SignatureScheme is how this gateway signs webhooks; the adapter must implement it.
	SignatureScheme gateway.SignatureScheme
	// SupportedAPIVersions is the adapter's supported version set.
	SupportedAPIVersions []string
	// ProcessingRegion is where the gateway processes and stores, for residency checks.
	ProcessingRegion string
}

// MerchantConfigView is the part of the merchant's configuration L3 compares the descriptor
// against. A view rather than the document itself: L3 asks "can this gateway do what the
// configuration promises", and nothing else in the document is its business.
type MerchantConfigView struct {
	Present               bool
	Currencies            []money.Currency
	Methods               []shared.PaymentMethod
	Countries             []shared.Country
	Requires3DS           bool
	ThreeDSCorridors      []shared.Country
	PartialCaptureEnabled bool
	MaxPartialCaptures    int
	MaxRefundWindowDays   int
}

// Credentials is the credential reference and its metadata — never the credential material.
// A validation rule that held plaintext credentials would put them one stack trace away from
// a log line.
type Credentials struct {
	ReferencePresent bool
	Reference        string
	// Resolved records whether Secrets Manager returned a current version and the envelope
	// decrypted. The resolution happened in the caller; the rule asserts on the outcome.
	Resolved  bool
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// Probe is everything the probe worker observed. Every impure L3 rule reads from here.
type Probe struct {
	Attempted      bool
	TLSHandshakeOK bool
	Authenticated  bool
	GrantedScopes  []string
	// P95LatencyMillis and SampleSize describe the recent probe latency distribution.
	P95LatencyMillis int
	SampleSize       int
	// Account state at the gateway.
	AccountResolved bool
	ChargesEnabled  bool
	PayoutsEnabled  bool
	CurrentlyDue    []string
	// Webhook registration as the gateway reports it.
	WebhookEndpointRegistered bool
	WebhookSecretFingerprint  string
	SubscribedEvents          []string
	// Version deprecation signalling.
	DeprecationSignaled bool
	SunsetDate          string
}

// Connection is the stored binding of a merchant to a gateway.
type Connection struct {
	GatewayID           shared.GatewayID
	Environment         shared.Environment
	Status              gateway.ConnectionStatus
	CertificationStatus gateway.CertificationStatus
	Provisioned         bool
	ExternalAccountRef  string
	PinnedAPIVersion    string
	WebhookEndpoint     string
	// StoredWebhookSecretFingerprint is what the platform holds; the probe reports what the
	// gateway holds. They must agree, or a webhook we cannot verify is on its way.
	StoredWebhookSecretFingerprint string
	CertificationReportID          string
	CertificationAssertionsPassed  bool
	CertifiedAt                    time.Time
}

// Subject is everything L3 evaluates.
type Subject struct {
	Connection  Connection
	Descriptor  Descriptor
	Credentials Credentials
	Probe       Probe
	Config      MerchantConfigView
	// Now is the injected clock reading.
	Now time.Time
}

// Deps carries the level's policy: the required scopes, events and operations, the adapter's
// version support, and the budgets. Pure data — no gateway client, no secrets client.
type Deps struct {
	// RequiredScopes is what a usable connection must be granted.
	RequiredScopes []string
	// RequiredWebhookEvents is the adapter's minimum event subscription.
	RequiredWebhookEvents []string
	// RequiredOperations is what a gateway must expose to be a primary route.
	RequiredOperations []shared.Operation
	// AdapterVersions maps a gateway to the API versions the adapter implements.
	AdapterVersions map[shared.GatewayID][]string
	// MaxCredentialAgeDays is the rotation SLA (90).
	MaxCredentialAgeDays int
	// ProbeLatencyBudgetMillis is the p95 probe latency budget (1500).
	ProbeLatencyBudgetMillis int
	// MinProbeSamples is how many probes must exist before the latency warning is meaningful.
	MinProbeSamples int
	// CertificationMaxAgeDays is how long a certification report stays valid (180).
	CertificationMaxAgeDays int
	// IngressHost is the environment's public webhook ingress hostname.
	IngressHost string
}

// DefaultDeps returns the platform defaults from docs/validation-plane.md §3.3.
func DefaultDeps() Deps {
	return Deps{
		RequiredScopes:           []string{"charges", "refunds", "webhooks", "account_read"},
		RequiredWebhookEvents:    []string{"auth", "capture", "refund", "dispute", "payout"},
		RequiredOperations:       []shared.Operation{shared.OpAuthorize, shared.OpCapture, shared.OpRefund, shared.OpVoid, shared.OpLookup},
		MaxCredentialAgeDays:     90,
		ProbeLatencyBudgetMillis: 1500,
		MinProbeSamples:          20,
		CertificationMaxAgeDays:  180,
	}
}
