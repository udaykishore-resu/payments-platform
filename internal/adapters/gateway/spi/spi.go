// Package spi defines the service provider interface every payment gateway adapter implements.
//
// This is the anti-corruption layer's contract. Nothing above it — the orchestrator, the
// routing engine, the payment aggregate — knows that Stripe calls a hold a "PaymentIntent",
// that Adyen calls it a "payment" with a `paymentPspReference`, or that PayPal wants a
// two-legged OAuth token refreshed every eight hours. Each adapter translates its vendor's
// world into the types below, and the core reasons only about these.
//
// The contract is deliberately strict, because an adapter that is merely *approximately*
// correct is worse than no adapter: it will succeed in testing and lose money in production.
// The obligations an implementation takes on are written out in the doc comment of each method
// and are mechanically checked by the shared contract test suite in
// internal/adapters/gateway/contract, which every adapter must pass. A new gateway is
// integrated by implementing this interface and making that suite green — there is no other
// definition of "done".
//
// Two rules that are easy to get wrong and are called out repeatedly below:
//
//  1. **Never invent an outcome.** If the adapter does not know whether money moved, it must
//     return ErrOutcomeUnknown. Returning a failure because a socket closed is how a platform
//     double-charges: the orchestrator will fail over, and the first gateway may already have
//     an authorization on the card.
//  2. **Never let a vendor error code reach the core untranslated.** A decline must map to a
//     normalized payment.DeclineReason, and an unmappable one maps to DeclineUnknown, which
//     does not permit failover. Silence is not permission.
package spi

import (
	"context"
	"errors"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Sentinel errors an adapter returns to signal a condition the orchestrator must handle
// specially. Everything else should be an *apierror.Error with an appropriate code.
var (
	// ErrOutcomeUnknown means the adapter cannot say whether the operation took effect. It is
	// the single most important value in this package. Return it for: a client timeout after
	// the request was written, a connection reset mid-response, a 5xx from a gateway that does
	// not guarantee atomicity, or any response the adapter cannot parse.
	//
	// It must NOT be returned for a connection that was refused, a DNS failure, a TLS
	// handshake failure, or a 4xx that the gateway documents as pre-processing — in all of
	// those the gateway provably did not act, and returning unknown needlessly parks a payment
	// in reconciliation.
	ErrOutcomeUnknown = errors.New("spi: gateway outcome unknown")

	// ErrNotSupported means the adapter does not implement this operation for this gateway.
	// The routing engine should have filtered on capabilities before dispatch, so reaching
	// this is a bug — but it is a bug that must fail loudly rather than silently succeed.
	ErrNotSupported = errors.New("spi: operation not supported by this gateway")

	// ErrCredentialsInvalid means authentication with the gateway failed. Distinct from a
	// decline: it is our problem, not the payer's, and it must page rather than fail the
	// payment quietly.
	ErrCredentialsInvalid = errors.New("spi: gateway credentials rejected")
)

// PaymentGateway is the port every adapter implements.
//
// Method-level contract obligations, checked by the contract suite:
//
//   - Every method must honour ctx cancellation and the deadline it carries. An adapter that
//     ignores the deadline breaks the orchestrator's timeout cascade and the bulkhead sizing
//     that depends on it.
//   - Every mutating method must send req.IdempotencyKey to the gateway in whatever form that
//     gateway supports, and must be safe to call twice with the same key.
//   - No method may log, wrap, or return the credential material.
//   - No method may return a nil result and a nil error.
type PaymentGateway interface {
	// ID returns the gateway's slug, matching the registry.
	ID() shared.GatewayID

	// Authorize places a hold, or performs a sale when req.Capture is true.
	//
	// The gateway may legitimately return: authorized, captured (for a sale), requires-action
	// (3DS), pending (asynchronous method), or declined. All five are successful *calls* and
	// return a non-nil result with a nil error. Only a transport or protocol problem is an
	// error.
	Authorize(ctx context.Context, req AuthorizeRequest) (*Result, error)

	// Capture converts a hold into a debit. Partial capture is supported where the gateway's
	// capability descriptor says so; the orchestrator will not call it otherwise.
	Capture(ctx context.Context, req CaptureRequest) (*Result, error)

	// Refund returns captured funds. Refunds are asynchronous at every real gateway: a
	// successful call means accepted, not settled, and the adapter must reflect that in
	// Result.Status rather than reporting SUCCESS optimistically.
	Refund(ctx context.Context, req RefundRequest) (*Result, error)

	// Void releases an uncaptured authorization.
	Void(ctx context.Context, req VoidRequest) (*Result, error)

	// Lookup asks the gateway what happened to a transaction, identified either by the
	// gateway's own reference or by the idempotency key we sent.
	//
	// This is the method that makes a timeout survivable, and it is the method most often
	// omitted from adapter implementations because the happy path never needs it. Without it,
	// an unknown outcome can only be resolved by waiting for a webhook that may never come.
	// The contract suite therefore requires that Lookup can find a transaction by idempotency
	// key alone.
	Lookup(ctx context.Context, req LookupRequest) (*Result, error)
}

// GatewayProvisioner is the onboarding half of a gateway integration. It is a separate
// interface from PaymentGateway because the two have different lifetimes, different
// credentials, and different consumers: the data plane needs the former and must not be able
// to call the latter, and the workflow worker needs the latter and never touches the payment
// path. Interface segregation with a security consequence.
type GatewayProvisioner interface {
	ID() shared.GatewayID

	// Provision creates a sub-merchant or connected account at the gateway. It must be
	// idempotent on req.IdempotencyKey: the onboarding workflow may retry after a crash, and
	// creating a second connected account for one merchant is a manual-cleanup incident at
	// every gateway.
	Provision(ctx context.Context, req ProvisionRequest) (*ProvisionResult, error)

	// Deprovision is the compensation for Provision, invoked when a later saga step fails.
	// It must tolerate the account not existing — compensation runs after crashes, and the
	// crash may have happened before the account was created.
	Deprovision(ctx context.Context, externalAccountID string) error

	// RegisterWebhook subscribes the platform's ingress endpoint to the gateway's events and
	// returns the registration ID and the signing secret.
	RegisterWebhook(ctx context.Context, req WebhookRegistrationRequest) (*WebhookRegistration, error)

	// UnregisterWebhook is the compensation for RegisterWebhook.
	UnregisterWebhook(ctx context.Context, externalAccountID, registrationID string) error

	// VerifyCredentials performs the cheapest authenticated call the gateway offers, used by
	// validation level L3 both during onboarding and as a scheduled probe. It must not have
	// side effects.
	VerifyCredentials(ctx context.Context, creds Credentials) error
}

// WebhookVerifier is implemented by every adapter to authenticate inbound notifications.
//
// It is separate from PaymentGateway because the webhook ingress service needs only this and
// should not be able to initiate payments — the ingress endpoint is the most exposed surface
// in the platform, and the blast radius of compromising it should not include the ability to
// move money.
type WebhookVerifier interface {
	ID() shared.GatewayID

	// Verify authenticates a webhook and extracts what the platform needs. Implementations
	// must:
	//   - use a constant-time comparison for the signature (a variable-time compare on an HMAC
	//     is a timing oracle),
	//   - reject a timestamp outside the tolerance window to stop replays,
	//   - reject before parsing the body, so a forged payload never reaches a parser,
	//   - support multiple active signing secrets so a secret rotation does not drop webhooks
	//     during the overlap window.
	Verify(ctx context.Context, raw []byte, headers map[string]string, secrets []string, now time.Time) (*WebhookEvent, error)
}

// Credentials carries the material needed to authenticate with a gateway. The values are
// resolved from the secrets store at the moment of use and are never stored in this form.
//
// The type is a map rather than named fields because gateways genuinely differ — Stripe wants
// a secret key, Adyen wants an API key plus a merchant account plus an HMAC key, PayPal wants
// a client ID and secret it exchanges for a bearer token. Naming them all would produce a
// struct where two thirds of the fields are empty for any given gateway.
type Credentials struct {
	// Values holds the credential fields. Callers must obtain this from a SecretsProvider and
	// must not log the struct.
	Values map[string]string
	// Version identifies which secret version these came from, so a request that fails
	// authentication can be attributed to a specific rotation.
	Version string
	// Environment selects the gateway's sandbox or live endpoints. Getting this wrong is the
	// failure mode where a certification run charges a real card, so it is a required field
	// with no default.
	Environment shared.Environment
}

// Get returns a credential field.
func (c Credentials) Get(field string) (string, bool) {
	v, ok := c.Values[field]
	return v, ok
}

// String redacts. Credentials must never appear in a log line, an error, or a span attribute,
// and the cheapest way to guarantee that is to make the obvious mistake produce nothing useful.
func (c Credentials) String() string { return "spi.Credentials{[REDACTED]}" }

// GoString redacts, covering the %#v verb.
func (c Credentials) GoString() string { return c.String() }

// AuthorizeRequest is an instruction to hold or take funds.
type AuthorizeRequest struct {
	// IdempotencyKey is derived from the attempt ID (payment.DeriveGatewayIdempotencyKey), not
	// from the client's key. See baseline §14.4.
	IdempotencyKey string

	Credentials       Credentials
	ExternalAccountID string

	PaymentID shared.PaymentID
	AttemptID shared.AttemptID
	Amount    money.Money
	Method    shared.PaymentMethod
	MethodRef payment.PaymentMethodReference

	// Capture true means sale (authorize and capture in one call); false means authorize only.
	Capture bool

	Descriptor   string
	StatementRef string
	Reference    string

	// ThreeDS carries the strong-customer-authentication instruction. Requesting 3DS is a
	// policy decision made by the risk engine, not by the adapter.
	ThreeDS ThreeDSRequest

	Customer  CustomerData
	ReturnURL string
	Metadata  map[string]string
}

// ThreeDSRequest instructs the gateway on strong customer authentication.
type ThreeDSRequest struct {
	// Requested forces a challenge.
	Requested bool
	// ExemptionType names the SCA exemption being claimed, if any. Recording it here and in
	// the payment's audit trail is what makes the exemption defensible later; an exemption
	// applied but not recorded is one the platform cannot prove it was entitled to.
	ExemptionType      string
	ChallengeIndicator string
}

// CustomerData is the minimum payer context a gateway needs for risk and SCA. It carries no
// more personal data than the gateway requires, because every field here is one the platform
// then has to protect, retain and eventually erase.
type CustomerData struct {
	ID        string
	EmailHash string
	IPAddress string
	Country   shared.Country
	UserAgent string
	DeviceID  string
}

// CaptureRequest converts a hold into a debit.
type CaptureRequest struct {
	IdempotencyKey    string
	Credentials       Credentials
	ExternalAccountID string
	PaymentID         shared.PaymentID
	AttemptID         shared.AttemptID
	// GatewayRef is the gateway's identifier for the authorization being captured.
	GatewayRef string
	Amount     money.Money
	// Final indicates no further captures will follow, which some gateways use to release the
	// remainder of the hold immediately rather than at expiry.
	Final    bool
	Metadata map[string]string
}

// RefundRequest returns captured funds.
type RefundRequest struct {
	IdempotencyKey    string
	Credentials       Credentials
	ExternalAccountID string
	PaymentID         shared.PaymentID
	RefundID          shared.RefundID
	GatewayRef        string
	Amount            money.Money
	Reason            payment.RefundReason
	Metadata          map[string]string
}

// VoidRequest releases an uncaptured authorization.
type VoidRequest struct {
	IdempotencyKey    string
	Credentials       Credentials
	ExternalAccountID string
	PaymentID         shared.PaymentID
	GatewayRef        string
	Metadata          map[string]string
}

// LookupRequest asks what happened to a transaction.
type LookupRequest struct {
	Credentials       Credentials
	ExternalAccountID string
	// GatewayRef may be empty when the crash happened before we recorded it — which is exactly
	// the case reconciliation exists for. IdempotencyKey must therefore be sufficient on its
	// own, and the contract suite asserts that.
	GatewayRef     string
	IdempotencyKey string
	Operation      shared.Operation
}

// Status is the normalized outcome of a gateway operation.
type Status string

const (
	StatusAuthorized     Status = "AUTHORIZED"
	StatusCaptured       Status = "CAPTURED"
	StatusRefundAccepted Status = "REFUND_ACCEPTED"
	StatusRefunded       Status = "REFUNDED"
	StatusVoided         Status = "VOIDED"
	StatusRequiresAction Status = "REQUIRES_ACTION"
	StatusPending        Status = "PENDING"
	StatusDeclined       Status = "DECLINED"
	StatusFailed         Status = "FAILED"
	// StatusNotFound is returned by Lookup when the gateway has no record of the transaction,
	// which — combined with a deterministic idempotency key — is positive evidence that the
	// operation never took effect and is therefore safe to retry.
	StatusNotFound Status = "NOT_FOUND"
)

// Result is the normalized outcome of a gateway call.
type Result struct {
	Status Status

	// GatewayRef is the gateway's identifier for the transaction. Required for every status
	// except NOT_FOUND and FAILED; the contract suite checks this, because a successful
	// authorization the platform cannot later reference is an authorization it cannot capture,
	// void or reconcile.
	GatewayRef string

	// AuthorizedAmount and CapturedAmount reflect what the gateway actually did, which is not
	// always what was asked: partial authorization is real, and a gateway that captures a
	// different amount than requested is a contract violation the response validator (L6) must
	// catch rather than the platform silently accepting.
	AuthorizedAmount *money.Money
	CapturedAmount   *money.Money

	// DeclineReason is populated for StatusDeclined and must be a normalized value. An adapter
	// that cannot map the vendor's code sets DeclineUnknown, which does not permit failover.
	DeclineReason payment.DeclineReason

	// NetworkAdviceNoRetry carries scheme-level "do not retry" guidance where the gateway
	// surfaces it. It overrides the platform's own failover judgement, but only in the
	// restrictive direction.
	NetworkAdviceNoRetry bool

	// NextAction tells the merchant where to send the payer for a 3DS challenge or redirect.
	NextAction *NextAction

	// AuthExpiresAt is when the hold lapses. Gateways and schemes differ; taking the value
	// from the gateway rather than assuming seven days is what stops the platform trying to
	// capture an authorization the issuer already released.
	AuthExpiresAt *time.Time

	// RawStatus and RawCode are the vendor's own values, retained verbatim for support and
	// reconciliation. They must never drive control flow — that is what the normalized fields
	// are for, and a `switch` on RawCode anywhere above this package is a review-blocking
	// defect.
	RawStatus  string
	RawCode    string
	RawMessage string

	// ProcessorResponse carries scheme-level detail useful for interchange analysis and
	// dispute defence: AVS and CVV results, the network used, the authorization code.
	ProcessorResponse ProcessorResponse

	// Fee is the gateway's own fee where it reports one at authorization time. Optional; most
	// gateways report fees at settlement.
	Fee *money.Money

	ReceivedAt time.Time
	Latency    time.Duration
}

// ProcessorResponse is scheme-level detail from the authorization.
type ProcessorResponse struct {
	AuthCode      string
	AVSResult     string
	CVVResult     string
	Network       string
	NetworkTxnID  string
	ThreeDSResult string
	ECI           string
}

// NextAction directs the payer to complete an out-of-band step.
type NextAction struct {
	Type        payment.NextActionType
	RedirectURL string
	QRCodeData  string
	ExpiresAt   *time.Time
}

// ProvisionRequest creates a sub-merchant or connected account.
type ProvisionRequest struct {
	IdempotencyKey string
	Credentials    Credentials

	MerchantID         shared.MerchantID
	LegalName          string
	DisplayName        string
	Country            shared.Country
	MCC                shared.MCC
	WebsiteURL         string
	SupportEmail       string
	TaxID              string
	RegistrationNumber string
	AddressLines       []string
	City               string
	Region             string
	PostalCode         string

	Principals  []PrincipalData
	BankAccount BankAccountData
	Currencies  []money.Currency
	Methods     []shared.PaymentMethod
	Environment shared.Environment
}

// PrincipalData is a beneficial owner as the gateway needs it.
type PrincipalData struct {
	FirstName    string
	LastName     string
	Role         string
	OwnershipPct int
	Country      shared.Country
	// VerificationRef points at the KYC provider's record. Gateways increasingly accept a
	// verified-elsewhere reference rather than re-collecting identity documents, which is both
	// faster and less personal data for the platform to hold.
	VerificationRef string
}

// BankAccountData is the settlement destination as the gateway needs it. The account number
// itself is resolved from the secrets store immediately before the call and is not stored in
// this struct's persisted form anywhere.
type BankAccountData struct {
	Country       shared.Country
	Currency      money.Currency
	HolderName    string
	AccountNumber string
	RoutingNumber string
	IBAN          string
}

// ProvisionResult is what the gateway created.
type ProvisionResult struct {
	ExternalAccountID string
	Status            string
	// RequiresAction is true when the gateway needs the merchant to complete a hosted step —
	// PayPal's partner referral consent, for example. The onboarding workflow parks on a
	// manual gate rather than failing.
	RequiresAction bool
	ActionURL      string
	// PendingRequirements lists what the gateway still needs. Surfacing these verbatim to the
	// merchant is what turns "provisioning failed" into an actionable message.
	PendingRequirements []string
	RawStatus           string
}

// WebhookRegistrationRequest subscribes the platform to a gateway's events.
type WebhookRegistrationRequest struct {
	IdempotencyKey    string
	Credentials       Credentials
	ExternalAccountID string
	// URL is the platform's ingress endpoint for this gateway.
	URL        string
	EventTypes []string
}

// WebhookRegistration is the gateway's acknowledgement.
type WebhookRegistration struct {
	RegistrationID string
	// SigningSecret is returned once, at creation, by most gateways. It goes straight into the
	// secrets store; it is never persisted in a database row or returned by an API.
	SigningSecret string
	URL           string
}

// WebhookEvent is a verified inbound notification, normalized.
type WebhookEvent struct {
	// GatewayEventID is the gateway's own event identifier, used as the deduplication key. A
	// gateway that does not supply one forces the adapter to synthesize a stable key from the
	// payload; the contract suite requires that the same payload always yields the same key.
	GatewayEventID string
	EventType      string
	// Kind is the normalized meaning: what the core should do with it.
	Kind WebhookKind

	GatewayRef     string
	IdempotencyKey string
	// PaymentID may be recoverable from the gateway's metadata, in which case the processor
	// does not need a lookup by gateway reference.
	PaymentID shared.PaymentID
	RefundID  shared.RefundID

	Status        Status
	Amount        *money.Money
	DeclineReason payment.DeclineReason
	OccurredAt    time.Time
	Raw           []byte
}

// WebhookKind is the normalized classification of an inbound event.
type WebhookKind string

const (
	KindAuthorizationSucceeded WebhookKind = "AUTHORIZATION_SUCCEEDED"
	KindAuthorizationFailed    WebhookKind = "AUTHORIZATION_FAILED"
	KindCaptureSucceeded       WebhookKind = "CAPTURE_SUCCEEDED"
	KindCaptureFailed          WebhookKind = "CAPTURE_FAILED"
	KindRefundSucceeded        WebhookKind = "REFUND_SUCCEEDED"
	KindRefundFailed           WebhookKind = "REFUND_FAILED"
	KindVoidSucceeded          WebhookKind = "VOID_SUCCEEDED"
	KindPayoutSettled          WebhookKind = "PAYOUT_SETTLED"
	KindDisputeOpened          WebhookKind = "DISPUTE_OPENED"
	KindDisputeClosed          WebhookKind = "DISPUTE_CLOSED"
	KindAccountUpdated         WebhookKind = "ACCOUNT_UPDATED"
	// KindIgnored is a legitimate outcome: gateways send far more event types than the
	// platform models, and an adapter that errors on an unrecognized type turns a gateway's
	// feature launch into an incident on our side.
	KindIgnored WebhookKind = "IGNORED"
)

// Factory builds an adapter for a gateway. The registry uses it so that a new gateway is
// added by registering a factory rather than by editing a switch statement somewhere in the
// orchestrator.
type Factory interface {
	ID() shared.GatewayID
	NewGateway(cfg Config) (PaymentGateway, error)
	NewProvisioner(cfg Config) (GatewayProvisioner, error)
	NewVerifier(cfg Config) (WebhookVerifier, error)
}

// Config is the per-gateway adapter configuration: endpoints, timeouts and the HTTP client the
// adapter must use.
//
// The client is injected rather than constructed by the adapter so that connection pooling,
// timeouts, the bulkhead and the circuit breaker are applied uniformly and are visible in one
// place. An adapter that builds its own http.Client is an adapter outside the resilience
// envelope, and that is invariably the one that takes the service down.
type Config struct {
	BaseURL     string
	APIVersion  string
	Timeout     time.Duration
	HTTPClient  HTTPDoer
	Clock       shared.Clock
	Environment shared.Environment
	// WebhookTolerance is the maximum age of a webhook timestamp before it is treated as a
	// replay. Five minutes by default: long enough to survive clock skew and a slow queue,
	// short enough that a captured request is not indefinitely replayable.
	WebhookTolerance time.Duration
}

// HTTPDoer is the minimal HTTP surface an adapter needs. Narrowing it to one method means a
// test double is three lines rather than a full http.Client stand-in, and it makes it obvious
// that an adapter has no business reaching for transport-level knobs.
type HTTPDoer interface {
	Do(req *HTTPRequest) (*HTTPResponse, error)
}

// HTTPRequest is a transport-agnostic request.
type HTTPRequest struct {
	Ctx     context.Context
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
}

// HTTPResponse is a transport-agnostic response.
type HTTPResponse struct {
	StatusCode int
	Headers    map[string]string
	Body       []byte
	// Timeout reports that the request did not complete. The adapter maps this to
	// ErrOutcomeUnknown for money-moving operations and to a plain error otherwise — the
	// distinction that keeps a slow gateway from becoming a double charge.
	Timeout bool
	Latency time.Duration
}
