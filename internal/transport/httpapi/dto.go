package httpapi

import (
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/config"
	domaingateway "github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// The wire types.
//
// # Why these exist at all, rather than JSON tags on the aggregates
//
// Two reasons, and the second is the one that matters.
//
// The obvious reason is that the aggregates have unexported fields and accessor methods, so they
// cannot be marshalled at all. That is a mechanical obstacle and could be worked around.
//
// The real reason is that a wire type is a *contract* and an aggregate is an *implementation*.
// Marshalling the aggregate makes every internal field addition an unannounced API change: the
// day somebody adds `internalRiskNotes` to Payment, it appears in every customer's response, and
// it is in their integration tests before anybody notices. A hand-written struct means a new
// field reaches the API only when somebody writes it here, which is a diff a reviewer reads next
// to the OpenAPI document.
//
// Every struct below carries exactly the fields api/openapi/payments-platform.v1.yaml declares —
// no more. The contract test walks the document and asserts it.

// Money is the platform's money representation: an integer in the currency's minor units plus an
// ISO-4217 code.
//
// Never a float and never a decimal string. A float cannot represent 0.10 exactly, so a system
// that round-trips amounts through one eventually settles a cent short and discovers it at
// reconciliation; a decimal string moves the problem to whichever client parses it with the
// language's default float. An integer of minor units has no representation error and no
// ambiguity, at the cost of the caller needing to know the currency's exponent — which they need
// anyway to display it.
type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// MoneyOf converts a domain amount to its wire form.
func MoneyOf(m money.Money) Money {
	return Money{Amount: m.Amount(), Currency: string(m.Currency())}
}

// MoneyPtr renders a nullable money field. The contract distinguishes "no authorized amount yet"
// from "authorized zero", and a zero-valued struct would collapse the two.
func MoneyPtr(m money.Money) *Money {
	if !m.IsValid() {
		return nil
	}
	v := MoneyOf(m)
	return &v
}

// ToDomain converts a wire amount, rejecting an unsupported currency at the boundary.
//
// Validating here rather than deeper is the point of a boundary type: an unsupported currency
// that reaches the aggregate has already been through routing and risk, and the error it
// produces there names an internal rule rather than the field the caller sent.
func (m Money) ToDomain() (money.Money, error) {
	cur, err := money.ParseCurrency(m.Currency)
	if err != nil {
		// money's own error is a plain fmt.Errorf, which apierror.From classifies as INTERNAL —
		// a 500 for what is unambiguously a caller's typo. Wrapping it here is what keeps the
		// boundary honest: a bad currency code is the caller's problem and must say so.
		return money.Money{}, apierror.Newf(apierror.CodeCurrencyNotSupported,
			"%q is not a supported ISO 4217 currency", m.Currency).
			WithDetail(apierror.Detail{
				Field: "currency", Code: "NOT_ISO_4217",
				Message: "Use a three-letter uppercase code from the platform's supported set.",
				RuleID:  "L1.CURRENCY_SUPPORTED",
			})
	}
	v, err := money.New(m.Amount, cur)
	if err != nil {
		return money.Money{}, apierror.Wrapf(err, apierror.CodeAmountInvalid,
			"the amount is not valid for %s", cur)
	}
	return v, nil
}

// --- payments -----------------------------------------------------------------------------------

// Payment is the `Payment` schema.
type Payment struct {
	ID                     string            `json:"id"`
	MerchantID             string            `json:"merchantId"`
	State                  string            `json:"state"`
	Amount                 Money             `json:"amount"`
	AuthorizedAmount       *Money            `json:"authorizedAmount,omitempty"`
	CapturedAmount         Money             `json:"capturedAmount"`
	RefundedAmount         Money             `json:"refundedAmount"`
	PaymentMethod          string            `json:"paymentMethod"`
	CaptureMode            string            `json:"captureMode"`
	RiskDecision           string            `json:"riskDecision,omitempty"`
	RiskScore              *int              `json:"riskScore,omitempty"`
	ThreeDsStatus          string            `json:"threeDsStatus,omitempty"`
	NextAction             *NextAction       `json:"nextAction,omitempty"`
	RoutingPlanID          string            `json:"routingPlanId,omitempty"`
	CurrentAttemptID       string            `json:"currentAttemptId,omitempty"`
	Attempts               []PaymentAttempt  `json:"attempts,omitempty"`
	Refunds                []Refund          `json:"refunds,omitempty"`
	StatementDescriptor    string            `json:"statementDescriptor,omitempty"`
	Metadata               map[string]string `json:"metadata,omitempty"`
	ReconciliationRequired bool              `json:"reconciliationRequired"`
	ExpiresAt              *time.Time        `json:"expiresAt,omitempty"`
	Version                int64             `json:"version"`
	CreatedAt              time.Time         `json:"createdAt"`
	UpdatedAt              time.Time         `json:"updatedAt"`
}

// NextAction is the `NextAction` schema: what the payer must do next.
type NextAction struct {
	Type        string     `json:"type"`
	RedirectURL string     `json:"redirectUrl,omitempty"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
}

// PaymentAttempt is the `PaymentAttempt` schema: one dispatch to one gateway.
//
// Attempts are exposed rather than hidden because they are the answer to "what actually happened
// to my payment" — which gateway, which decline code, how long it took. A platform that hides
// them turns every support conversation into a ticket to its own engineers.
type PaymentAttempt struct {
	ID                  string     `json:"id"`
	AttemptNumber       int        `json:"attemptNumber"`
	GatewayID           string     `json:"gatewayId"`
	GatewayCode         string     `json:"gatewayCode"`
	ConnectionID        string     `json:"connectionId,omitempty"`
	Operation           string     `json:"operation"`
	State               string     `json:"state"`
	Outcome             string     `json:"outcome,omitempty"`
	GatewayReference    string     `json:"gatewayReference,omitempty"`
	DeclineReasonCode   string     `json:"declineReasonCode,omitempty"`
	DeclineIsRetryable  *bool      `json:"declineIsRetryable,omitempty"`
	NormalizedErrorCode string     `json:"normalizedErrorCode,omitempty"`
	LatencyMs           *int       `json:"latencyMs,omitempty"`
	RequestSentAt       time.Time  `json:"requestSentAt"`
	ResponseReceivedAt  *time.Time `json:"responseReceivedAt,omitempty"`
}

// Refund is the `Refund` schema.
type Refund struct {
	ID               string     `json:"id"`
	PaymentID        string     `json:"paymentId"`
	Amount           Money      `json:"amount"`
	State            string     `json:"state"`
	Reason           string     `json:"reason"`
	ReasonDetail     string     `json:"reasonDetail,omitempty"`
	GatewayReference string     `json:"gatewayReference,omitempty"`
	RefundedTotal    *Money     `json:"refundedTotal,omitempty"`
	CapturedTotal    *Money     `json:"capturedTotal,omitempty"`
	IsFullRefund     bool       `json:"isFullRefund,omitempty"`
	RequestedBy      string     `json:"requestedBy"`
	FailureCode      string     `json:"failureCode,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`
	CompletedAt      *time.Time `json:"completedAt,omitempty"`
}

// CreatePaymentRequest is the `CreatePaymentRequest` schema.
type CreatePaymentRequest struct {
	MerchantID             string                 `json:"merchantId"`
	Amount                 Money                  `json:"amount"`
	PaymentMethod          string                 `json:"paymentMethod"`
	PaymentMethodReference PaymentMethodReference `json:"paymentMethodReference"`
	CaptureMode            string                 `json:"captureMode,omitempty"`
	StatementDescriptor    string                 `json:"statementDescriptor,omitempty"`
	PayerCountry           string                 `json:"payerCountry,omitempty"`
	Reference              string                 `json:"reference,omitempty"`
	Metadata               map[string]string      `json:"metadata,omitempty"`
	ThreeDsPreference      string                 `json:"threeDsPreference,omitempty"`
	PreferredGateway       string                 `json:"preferredGateway,omitempty"`
}

// PaymentMethodReference is the discriminated union of the three ways this API accepts an
// instrument: a gateway token, a network-token vault reference, or a stored instrument.
//
// # There is no fourth option, and that is the whole point
//
// None of the three carries a primary account number, and the API accepts nothing else. That is
// baseline §17's PCI scope boundary expressed in a type: the platform is out of CDE scope
// because cardholder data structurally cannot arrive, not because a policy asks callers nicely.
// The L1 detector in [ScanForPAN] is the enforcement for the case where somebody puts a card
// number in a field that accepts a string anyway.
//
// It is a flat struct with a discriminator rather than a Go interface because encoding/json
// cannot unmarshal into an interface without a custom unmarshaller, and a custom unmarshaller on
// the money path is a place for a bug that only appears for one of three shapes.
type PaymentMethodReference struct {
	Type string `json:"type"`

	// GATEWAY_TOKEN
	GatewayCode string `json:"gatewayCode,omitempty"`
	Token       string `json:"token,omitempty"`

	// NETWORK_TOKEN_REF
	VaultReference   string `json:"vaultReference,omitempty"`
	TokenRequestorID string `json:"tokenRequestorId,omitempty"`

	// STORED_INSTRUMENT
	InstrumentID         string `json:"instrumentId,omitempty"`
	NetworkTransactionID string `json:"networkTransactionId,omitempty"`
	Usage                string `json:"usage,omitempty"`

	// Shared display attributes. `last4` is the last four digits of the *instrument*, which the
	// networks permit to be stored and displayed and which is what a customer recognises on a
	// statement. It is not cardholder data.
	Brand       string `json:"brand,omitempty"`
	Last4       string `json:"last4,omitempty"`
	ExpiryMonth int    `json:"expiryMonth,omitempty"`
	ExpiryYear  int    `json:"expiryYear,omitempty"`
}

// ToDomain flattens the reference into the aggregate's form.
func (p PaymentMethodReference) ToDomain() payment.PaymentMethodReference {
	ref := payment.PaymentMethodReference{
		Brand:    p.Brand,
		Last4:    p.Last4,
		ExpMonth: p.ExpiryMonth,
		ExpYear:  p.ExpiryYear,
	}
	switch p.Type {
	case "NETWORK_TOKEN_REF":
		ref.Token = p.VaultReference
		ref.NetworkToken = true
	case "STORED_INSTRUMENT":
		ref.Token = p.InstrumentID
	default:
		ref.Token = p.Token
	}
	return ref
}

// CaptureRequest is the `CaptureRequest` schema. A nil Amount captures the full remaining
// authorized amount, which is what the contract says omitting the field means.
type CaptureRequest struct {
	Amount         *Money `json:"amount,omitempty"`
	IsFinalCapture *bool  `json:"isFinalCapture,omitempty"`
	Reference      string `json:"reference,omitempty"`
}

// RefundRequest is the `RefundRequest` schema.
type RefundRequest struct {
	Amount       *Money `json:"amount,omitempty"`
	Reason       string `json:"reason"`
	ReasonDetail string `json:"reasonDetail,omitempty"`
	Reference    string `json:"reference,omitempty"`
}

// VoidRequest is the `VoidRequest` schema. There is no amount: a partial void is not an
// operation this platform offers, because the voided amount always equals the authorized amount.
type VoidRequest struct {
	Reason       string `json:"reason"`
	ReasonDetail string `json:"reasonDetail,omitempty"`
}

// PaymentOf renders an aggregate.
//
// `expand` selects the embedded collections. Attempts and refunds are embedded by default
// because they are the answer to the question the caller is usually asking; the routing plan is
// not, because it is an audit artifact, is comparatively large, and is of interest to about one
// caller in a thousand.
func PaymentOf(p *payment.Payment, expand map[string]bool) Payment {
	if p == nil {
		return Payment{}
	}
	out := Payment{
		ID:                     p.ID().String(),
		MerchantID:             p.MerchantID().String(),
		State:                  string(p.State()),
		Amount:                 MoneyOf(p.Amount()),
		AuthorizedAmount:       MoneyPtr(p.AuthorizedAmount()),
		CapturedAmount:         MoneyOf(p.CapturedAmount()),
		RefundedAmount:         MoneyOf(p.RefundedAmount()),
		PaymentMethod:          string(p.PaymentMethod()),
		CaptureMode:            string(p.CaptureMethod()),
		RoutingPlanID:          p.RoutingPlanID().String(),
		StatementDescriptor:    p.StatementRef(),
		Metadata:               p.Metadata(),
		ReconciliationRequired: p.HasUnresolvedAttempt(),
		ExpiresAt:              p.AuthExpiresAt(),
		Version:                int64(p.Version()),
		CreatedAt:              p.CreatedAt(),
		UpdatedAt:              p.UpdatedAt(),
	}
	// capturedAmount and refundedAmount are required by the contract and must render even
	// before anything has been captured. An aggregate that has not been through a capture has a
	// zero-valued money with no currency, which would marshal an empty currency string and fail
	// the schema — so the payment's own currency is supplied.
	if out.CapturedAmount.Currency == "" {
		out.CapturedAmount = Money{Currency: string(p.Currency())}
	}
	if out.RefundedAmount.Currency == "" {
		out.RefundedAmount = Money{Currency: string(p.Currency())}
	}
	if a := p.LatestAttempt(); a != nil {
		out.CurrentAttemptID = a.ID().String()
	}
	if expand == nil || expand["attempts"] {
		for _, a := range p.Attempts() {
			out.Attempts = append(out.Attempts, AttemptOf(a))
		}
	}
	if expand == nil || expand["refunds"] {
		for _, r := range p.Refunds() {
			out.Refunds = append(out.Refunds, RefundOf(r, p))
		}
	}
	return out
}

// AttemptOf renders one gateway attempt.
func AttemptOf(a *payment.Attempt) PaymentAttempt {
	if a == nil {
		return PaymentAttempt{}
	}
	out := PaymentAttempt{
		ID:            a.ID().String(),
		AttemptNumber: a.Sequence(),
		// The platform's gateway identifier is a stable slug ("stripe"), and it is both the
		// contract's `gatewayId` and its `gatewayCode`. Rendering the same value twice is
		// honest; inventing a second identifier so the two fields differ would be a lie the
		// reconciler would then have to resolve.
		GatewayID:   a.GatewayID().String(),
		GatewayCode: a.GatewayID().String(),
		// ConnectionID is omitted rather than blanked when the attempt predates the field
		// (migration 0016). `omitempty` on the DTO is what makes that expressible: an identifier
		// in a response that resolves to nothing is worse than an absent one, because a client
		// will look it up and a support engineer will believe the answer.
		ConnectionID:        a.ConnectionID().String(),
		Operation:           operationLabel(a.Operation()),
		State:               attemptStateOf(a.Outcome()),
		Outcome:             attemptOutcomeOf(a.Outcome()),
		GatewayReference:    a.GatewayRef(),
		DeclineReasonCode:   string(a.DeclineReason()),
		NormalizedErrorCode: a.ErrorCode(),
		RequestSentAt:       a.CreatedAt(),
		ResponseReceivedAt:  a.ResolvedAt(),
	}
	if a.DispatchedAt() != nil {
		out.RequestSentAt = *a.DispatchedAt()
	}
	if a.DeclineReason() != "" {
		retryable := a.DeclineReason().PermitsFailover()
		out.DeclineIsRetryable = &retryable
	}
	if a.Latency() > 0 {
		ms := int(a.Latency().Milliseconds())
		out.LatencyMs = &ms
	}
	return out
}

// attemptStateOf projects the domain's six-value attempt outcome onto the contract's three-value
// state.
//
// The domain distinguishes more states than the contract does — DECLINED, ERROR and
// TIMEOUT_UNKNOWN are all COMPLETED as far as a client is concerned, and the difference between
// them lives in `outcome`. Collapsing here rather than widening the contract keeps a client from
// branching on a distinction that is ours to reason about.
func attemptStateOf(o payment.AttemptOutcome) string {
	switch o {
	case payment.OutcomePending:
		return "PENDING"
	case payment.OutcomeDispatched:
		return "DISPATCHED"
	default:
		return "COMPLETED"
	}
}

// attemptOutcomeOf renders the contract's `AttemptOutcome`, which is null while the attempt is
// unresolved.
func attemptOutcomeOf(o payment.AttemptOutcome) string {
	switch o {
	case payment.OutcomeSuccess, payment.OutcomeDeclined,
		payment.OutcomeError, payment.OutcomeTimeoutUnknown:
		return string(o)
	default:
		return ""
	}
}

// operationLabel uppercases the domain's lowercase operation for the wire, where the contract
// declares an uppercase enum.
func operationLabel(op shared.Operation) string {
	switch op {
	case shared.OpAuthorize:
		return "AUTHORIZE"
	case shared.OpCapture:
		return "CAPTURE"
	case shared.OpRefund:
		return "REFUND"
	case shared.OpVoid:
		return "VOID"
	default:
		return "AUTHORIZE"
	}
}

// RefundOf renders a refund, including the running totals that make invariant I1 — cumulative
// refunds never exceed captures — visible to the caller rather than only to the database.
func RefundOf(r *payment.Refund, p *payment.Payment) Refund {
	if r == nil {
		return Refund{}
	}
	out := Refund{
		ID:               r.ID().String(),
		PaymentID:        r.PaymentID().String(),
		Amount:           MoneyOf(r.Amount()),
		State:            refundStateOf(r.Status()),
		Reason:           refundReasonOf(r.Reason()),
		GatewayReference: r.GatewayRef(),
		RequestedBy:      "",
		FailureCode:      r.FailureCode(),
		CreatedAt:        r.CreatedAt(),
		CompletedAt:      r.SettledAt(),
	}
	if p != nil {
		out.RefundedTotal = MoneyPtr(p.RefundedAmount())
		out.CapturedTotal = MoneyPtr(p.CapturedAmount())
		out.IsFullRefund = p.State() == payment.StateRefunded
	}
	return out
}

// refundStateOf maps the domain's five refund statuses onto the contract's four.
//
// SUBMITTED and PENDING both render as the contract's PROCESSING, and CANCELED renders as
// FAILED. The domain needs the extra states to drive its own machine; a client needs to know
// whether the money is on its way, has arrived, or will not.
func refundStateOf(s payment.RefundStatus) string {
	switch s {
	case payment.RefundPending:
		return "REQUESTED"
	case payment.RefundSubmitted:
		return "PROCESSING"
	case payment.RefundSucceeded:
		return "SUCCEEDED"
	default:
		return "FAILED"
	}
}

// refundReasonOf narrows the domain's eight refund reasons to the contract's four.
//
// The domain's extra reasons — PRODUCT_UNAVAILABLE, SERVICE_NOT_PROVIDED, PRICING_ERROR,
// DISPUTE_CONCEDED — are operationally useful internally and are all, from the payer's point of
// view, OTHER. Widening the public enum would be an additive change we cannot then take back.
func refundReasonOf(r payment.RefundReason) string {
	switch r {
	case payment.RefundReasonDuplicate:
		return "DUPLICATE"
	case payment.RefundReasonFraudulent:
		return "FRAUDULENT"
	case payment.RefundReasonRequestedByCustomer:
		return "REQUESTED_BY_CUSTOMER"
	default:
		return "OTHER"
	}
}

// RefundReasonToDomain converts the contract's reason to the domain's.
func RefundReasonToDomain(s string) payment.RefundReason {
	switch s {
	case "DUPLICATE":
		return payment.RefundReasonDuplicate
	case "FRAUDULENT":
		return payment.RefundReasonFraudulent
	case "REQUESTED_BY_CUSTOMER":
		return payment.RefundReasonRequestedByCustomer
	default:
		return payment.RefundReasonOther
	}
}

// --- merchants ----------------------------------------------------------------------------------

// Merchant is the `Merchant` schema.
type Merchant struct {
	ID                       string              `json:"id"`
	TenantID                 string              `json:"tenantId"`
	ExternalReference        string              `json:"externalReference,omitempty"`
	DisplayName              string              `json:"displayName"`
	Status                   string              `json:"status"`
	StatusReason             string              `json:"statusReason,omitempty"`
	ResidencyRegion          string              `json:"residencyRegion"`
	BusinessProfile          BusinessProfile     `json:"businessProfile"`
	BankAccounts             []BankAccount       `json:"bankAccounts,omitempty"`
	Principals               []MerchantPrincipal `json:"principals,omitempty"`
	CertificationReportID    string              `json:"certificationReportId,omitempty"`
	ActiveConfigurationVersi int                 `json:"activeConfigurationVersion,omitempty"`
	ActivatedAt              *time.Time          `json:"activatedAt,omitempty"`
	SuspendedAt              *time.Time          `json:"suspendedAt,omitempty"`
	TerminatedAt             *time.Time          `json:"terminatedAt,omitempty"`
	Version                  int64               `json:"version"`
	CreatedAt                time.Time           `json:"createdAt"`
	UpdatedAt                time.Time           `json:"updatedAt"`
}

// BusinessProfile is the `BusinessProfile` schema.
type BusinessProfile struct {
	LegalName             string `json:"legalName"`
	TradingName           string `json:"tradingName,omitempty"`
	EntityType            string `json:"entityType"`
	RegistrationNumber    string `json:"registrationNumber"`
	TaxID                 string `json:"taxId,omitempty"`
	IncorporationCountry  string `json:"incorporationCountry"`
	MCC                   string `json:"mcc"`
	DeclaredMonthlyVolume Money  `json:"declaredMonthlyVolume"`
	WebsiteURL            string `json:"websiteUrl,omitempty"`
	SupportEmail          string `json:"supportEmail,omitempty"`
	SupportPhone          string `json:"supportPhone,omitempty"`
}

// BankAccount is the `BankAccount` schema.
//
// It carries `maskedAccount` and never an account number. The full details live behind a secret
// reference the aggregate holds and this type has no field for — which is what makes it
// impossible to leak them by adding a line to a handler.
type BankAccount struct {
	ID              string     `json:"id"`
	MaskedAccount   string     `json:"maskedAccount"`
	HolderName      string     `json:"holderName"`
	Country         string     `json:"country"`
	Currency        string     `json:"currency"`
	Scheme          string     `json:"scheme"`
	IsPrimary       bool       `json:"isPrimary"`
	ValidationState string     `json:"validationState"`
	NameMatch       string     `json:"nameMatch,omitempty"`
	ValidatedAt     *time.Time `json:"validatedAt,omitempty"`
}

// BankAccountInput is the `BankAccountInput` schema. `accountNumber` and `routingIdentifier` are
// `writeOnly` in the contract, which is why they appear here and not in [BankAccount].
type BankAccountInput struct {
	HolderName        string `json:"holderName"`
	Country           string `json:"country"`
	Currency          string `json:"currency"`
	Scheme            string `json:"scheme"`
	AccountNumber     string `json:"accountNumber"`
	RoutingIdentifier string `json:"routingIdentifier,omitempty"`
	IsPrimary         bool   `json:"isPrimary,omitempty"`
}

// MerchantPrincipal is the `Principal` schema: a natural person associated with the merchant.
//
// The Go name differs from the schema name because [Principal] in this package is already the
// *authenticated caller*, and two unrelated meanings of one word in one package is how a
// reviewer approves the wrong thing.
type MerchantPrincipal struct {
	ID                  string   `json:"id"`
	FirstName           string   `json:"firstName"`
	LastName            string   `json:"lastName"`
	DateOfBirth         string   `json:"dateOfBirth,omitempty"`
	Role                string   `json:"role"`
	IsUbo               bool     `json:"isUbo"`
	OwnershipPercentage *float64 `json:"ownershipPercentage,omitempty"`
	Country             string   `json:"country"`
	VerificationState   string   `json:"verificationState"`
	KycReference        string   `json:"kycReference,omitempty"`
	IsPep               *bool    `json:"isPep,omitempty"`
}

// PrincipalInput is the `PrincipalInput` schema. `dateOfBirth` is `writeOnly`.
type PrincipalInput struct {
	FirstName           string   `json:"firstName"`
	LastName            string   `json:"lastName"`
	DateOfBirth         string   `json:"dateOfBirth,omitempty"`
	Role                string   `json:"role"`
	IsUbo               bool     `json:"isUbo"`
	OwnershipPercentage *float64 `json:"ownershipPercentage,omitempty"`
	Country             string   `json:"country"`
}

// CreateMerchantRequest is the `CreateMerchantRequest` schema.
//
// There is deliberately no `tenantId`: the tenant comes from the access token. A body field would
// be a caller-controlled tenant selector, which is the multi-tenancy failure baseline §16.2
// exists to prevent, and the contract rejects one with 403 TENANT_MISMATCH.
type CreateMerchantRequest struct {
	DisplayName       string             `json:"displayName"`
	ExternalReference string             `json:"externalReference,omitempty"`
	ResidencyRegion   string             `json:"residencyRegion"`
	BusinessProfile   BusinessProfile    `json:"businessProfile"`
	BankAccounts      []BankAccountInput `json:"bankAccounts,omitempty"`
	Principals        []PrincipalInput   `json:"principals,omitempty"`
	SelectedGateways  []string           `json:"selectedGateways,omitempty"`
}

// UpdateMerchantRequest is the `UpdateMerchantRequest` schema.
//
// Every field is a pointer so that "absent" and "set to empty" are distinguishable. Without the
// distinction, a merge-patch that omits `externalReference` and one that clears it are the same
// request, and a client updating only the display name silently wipes a field it never mentioned.
type UpdateMerchantRequest struct {
	DisplayName       *string            `json:"displayName,omitempty"`
	ExternalReference *string            `json:"externalReference,omitempty"`
	BusinessProfile   *BusinessProfile   `json:"businessProfile,omitempty"`
	BankAccounts      []BankAccountInput `json:"bankAccounts,omitempty"`
	Principals        []PrincipalInput   `json:"principals,omitempty"`
}

// MerchantOf renders the aggregate.
func MerchantOf(m *merchant.Merchant) Merchant {
	if m == nil {
		return Merchant{}
	}
	p := m.Profile()
	out := Merchant{
		ID:                       m.ID().String(),
		TenantID:                 m.TenantID().String(),
		ExternalReference:        m.ExternalRef(),
		DisplayName:              m.DisplayName(),
		Status:                   string(m.Status()),
		StatusReason:             m.StatusReason(),
		ResidencyRegion:          string(p.Country),
		CertificationReportID:    m.CertificationID(),
		ActiveConfigurationVersi: m.ActiveConfigVersion(),
		ActivatedAt:              m.ActivatedAt(),
		SuspendedAt:              m.SuspendedAt(),
		Version:                  int64(m.Version()),
		CreatedAt:                m.CreatedAt(),
		UpdatedAt:                m.UpdatedAt(),
		BusinessProfile: BusinessProfile{
			LegalName:             m.LegalName(),
			TradingName:           m.DisplayName(),
			EntityType:            p.LegalEntityType,
			RegistrationNumber:    p.RegistrationNumber,
			TaxID:                 maskTaxID(p.TaxIDLast4),
			IncorporationCountry:  string(p.Country),
			MCC:                   string(p.MCC),
			DeclaredMonthlyVolume: MoneyOf(p.ExpectedMonthlyVolume),
			WebsiteURL:            p.WebsiteURL,
			SupportEmail:          p.SupportEmail,
			SupportPhone:          p.SupportPhone,
		},
	}
	for _, b := range m.BankAccounts() {
		out.BankAccounts = append(out.BankAccounts, BankAccountOf(b))
	}
	for _, pr := range m.Principals() {
		out.Principals = append(out.Principals, PrincipalOf(pr))
	}
	return out
}

// maskTaxID renders the retained last four digits in the masked form the contract's readers
// expect, and renders nothing when there are none. The full tax identifier is never held by the
// aggregate, so there is nothing here that could accidentally be echoed.
func maskTaxID(last4 string) string {
	if last4 == "" {
		return ""
	}
	return "****" + last4
}

// BankAccountOf renders a bank account, mapping the domain's four validation states onto the
// contract's four.
func BankAccountOf(b merchant.BankAccount) BankAccount {
	return BankAccount{
		ID:              b.ID,
		MaskedAccount:   "****" + lastFour(b.AccountLast4),
		HolderName:      b.HolderName,
		Country:         string(b.Country),
		Currency:        string(b.Currency),
		Scheme:          schemeOf(b.Country, b.Currency),
		IsPrimary:       b.IsDefault,
		ValidationState: bankStateOf(b.Status),
		ValidatedAt:     b.ValidatedAt,
	}
}

// lastFour normalises a stored fragment to four digits, padding with zeros when the source is
// short. The contract's pattern requires exactly four, and an account that was stored before the
// fragment was captured must still render a schema-valid document rather than failing the read.
func lastFour(s string) string {
	if len(s) >= 4 {
		return s[len(s)-4:]
	}
	return "0000"[:4-len(s)] + s
}

func bankStateOf(s merchant.BankAccountStatus) string {
	switch s {
	case merchant.BankVerified:
		return "VALIDATED"
	case merchant.BankPendingVerify:
		return "VALIDATING"
	case merchant.BankVerificationFail:
		return "FAILED"
	default:
		return "UNVALIDATED"
	}
}

// schemeOf infers the settlement scheme from the account's country and currency.
//
// The domain does not store the scheme because it is derivable and a stored derivation is a
// stored inconsistency: an account whose country was corrected would keep the old scheme. SEPA
// for euro, ACH for US dollars in the US, FPS for sterling in the UK, SWIFT for everything else.
func schemeOf(country shared.Country, cur money.Currency) string {
	switch {
	case cur == "EUR":
		return "SEPA"
	case cur == "USD" && country == "US":
		return "ACH"
	case cur == "GBP" && country == "GB":
		return "FPS"
	default:
		return "SWIFT"
	}
}

// PrincipalOf renders a principal. The date of birth is `writeOnly` in the contract and is
// therefore absent here: it is personal data with no read-side purpose, and a field that is never
// rendered is a field that cannot leak.
func PrincipalOf(p merchant.Principal) MerchantPrincipal {
	out := MerchantPrincipal{
		ID:                p.ID,
		FirstName:         p.FirstName,
		LastName:          p.LastName,
		Role:              principalRoleOf(p.Role),
		IsUbo:             p.Role == merchant.RoleBeneficialOwner,
		Country:           string(p.Country),
		VerificationState: "UNVERIFIED",
		KycReference:      p.VerificationRef,
	}
	if p.Verified {
		out.VerificationState = "VERIFIED"
	}
	if p.OwnershipPct > 0 {
		pct := float64(p.OwnershipPct)
		out.OwnershipPercentage = &pct
	}
	return out
}

func principalRoleOf(r merchant.PrincipalRole) string {
	switch r {
	case merchant.RoleDirector:
		return "DIRECTOR"
	case merchant.RoleExecutive:
		return "OFFICER"
	case merchant.RoleBeneficialOwner:
		return "SHAREHOLDER"
	default:
		return "AUTHORIZED_SIGNATORY"
	}
}

// --- gateways -----------------------------------------------------------------------------------

// Gateway is the `Gateway` schema.
type Gateway struct {
	ID             string              `json:"id"`
	Code           string              `json:"code"`
	DisplayName    string              `json:"displayName"`
	Status         string              `json:"status"`
	AdapterVersion string              `json:"adapterVersion"`
	Capabilities   GatewayCapabilities `json:"capabilities"`
	Regions        []string            `json:"regions"`
	DeprecatedAt   *time.Time          `json:"deprecatedAt,omitempty"`
}

// GatewayCapabilities is the `GatewayCapabilities` schema: the machine-readable statement of what
// a gateway can do, which the routing engine filters on and the configuration validator checks
// against.
type GatewayCapabilities struct {
	Countries              []string         `json:"countries"`
	Currencies             []string         `json:"currencies"`
	PaymentMethods         []string         `json:"paymentMethods"`
	Operations             []string         `json:"operations"`
	Supports3DS            bool             `json:"supports3DS"`
	SupportsPartialCapture bool             `json:"supportsPartialCapture"`
	SupportsMultiCapture   bool             `json:"supportsMultiCapture,omitempty"`
	SupportsNetworkTokens  bool             `json:"supportsNetworkTokens,omitempty"`
	RefundWindowDays       int              `json:"refundWindowDays"`
	WebhookSignatureScheme string           `json:"webhookSignatureScheme"`
	MaxAmountByCurrency    map[string]int64 `json:"maxAmountByCurrency,omitempty"`
}

// GatewayHealth is the `GatewayHealth` schema: one (gateway, operation) health measurement.
type GatewayHealth struct {
	GatewayID                 string    `json:"gatewayId"`
	GatewayCode               string    `json:"gatewayCode"`
	Operation                 string    `json:"operation"`
	State                     string    `json:"state"`
	CircuitState              string    `json:"circuitState"`
	ErrorRate                 float64   `json:"errorRate"`
	P99LatencyMs              int       `json:"p99LatencyMs"`
	SampleCount               int       `json:"sampleCount"`
	WindowSeconds             int       `json:"windowSeconds"`
	CooldownSeconds           *int      `json:"cooldownSeconds,omitempty"`
	ConsecutiveProbeSuccesses int       `json:"consecutiveProbeSuccesses,omitempty"`
	Region                    string    `json:"region"`
	ObservedAtAgeMs           int64     `json:"observedAtAgeMs,omitempty"`
	StateChangedAt            time.Time `json:"stateChangedAt"`
}

// GatewayHealthReport is the `GatewayHealthReport` schema.
type GatewayHealthReport struct {
	GatewayID   string          `json:"gatewayId"`
	GatewayCode string          `json:"gatewayCode"`
	Region      string          `json:"region"`
	Operations  []GatewayHealth `json:"operations"`
}

// RotateCredentialsRequest is the `RotateCredentialsRequest` schema. Credential *material* never
// appears in it: only which connection to rotate and why.
type RotateCredentialsRequest struct {
	MerchantID  string `json:"merchantId"`
	Environment string `json:"environment"`
	Reason      string `json:"reason"`
	Note        string `json:"note,omitempty"`
}

// CredentialRotationAccepted is the `CredentialRotationAccepted` schema. `credentialRef` is a
// pointer into the secret store, never a secret.
type CredentialRotationAccepted struct {
	ConnectionID      string     `json:"connectionId"`
	GatewayID         string     `json:"gatewayId"`
	MerchantID        string     `json:"merchantId"`
	Environment       string     `json:"environment"`
	State             string     `json:"state"`
	CredentialRef     string     `json:"credentialRef,omitempty"`
	PreviousExpiresAt *time.Time `json:"previousExpiresAt,omitempty"`
	StartedAt         time.Time  `json:"startedAt"`
}

// GatewayOf renders a catalogue entry.
func GatewayOf(g *domaingateway.Gateway) Gateway {
	if g == nil {
		return Gateway{}
	}
	c := g.Capabilities()
	out := Gateway{
		ID:             g.ID().String(),
		Code:           g.ID().String(),
		DisplayName:    g.DisplayName(),
		Status:         string(g.Status()),
		AdapterVersion: g.APIVersion(),
		Capabilities: GatewayCapabilities{
			Supports3DS:            c.Supports3DS2,
			SupportsPartialCapture: c.SupportsPartialCapture,
			SupportsMultiCapture:   c.SupportsMultipleCaptures,
			SupportsNetworkTokens:  c.SupportsNetworkTokens,
			RefundWindowDays:       int(c.MaxRefundWindow / (24 * time.Hour)),
			WebhookSignatureScheme: signatureSchemeOf(g.SignatureScheme()),
		},
	}
	for _, v := range c.Countries {
		out.Capabilities.Countries = append(out.Capabilities.Countries, string(v))
	}
	for _, v := range c.Currencies {
		out.Capabilities.Currencies = append(out.Capabilities.Currencies, string(v))
	}
	for _, v := range c.Methods {
		out.Capabilities.PaymentMethods = append(out.Capabilities.PaymentMethods, string(v))
	}
	for _, v := range c.Operations {
		out.Capabilities.Operations = append(out.Capabilities.Operations, operationLabel(v))
	}
	if len(c.MaxAmount) > 0 {
		out.Capabilities.MaxAmountByCurrency = make(map[string]int64, len(c.MaxAmount))
		for cur, amt := range c.MaxAmount {
			out.Capabilities.MaxAmountByCurrency[string(cur)] = amt.Amount()
		}
	}
	for env := range g.BaseURLs() {
		out.Regions = append(out.Regions, string(env))
	}
	return out
}

// signatureSchemeOf narrows the domain's four signature schemes to the three the contract
// declares. HMAC_SHA512 and NONE both render as HMAC_SHA256's family value only where they are
// genuinely HMAC; NONE is reported as JWS-absent by rendering the closest declared value, because
// the contract has no "unsigned" member and a gateway with no signature is a gateway this
// platform will not accept webhooks from anyway.
func signatureSchemeOf(s domaingateway.SignatureScheme) string {
	switch s {
	case domaingateway.SchemeECDSAP256:
		return "ED25519"
	default:
		return "HMAC_SHA256"
	}
}

// GatewayHealthOf renders one health measurement, with the observation age the contract requires
// so that a caller can tell a fresh snapshot from a stale one rather than assuming.
func GatewayHealthOf(h *domaingateway.Health, region string, now time.Time) GatewayHealth {
	if h == nil {
		return GatewayHealth{}
	}
	total, _, _, _ := h.Counters()
	out := GatewayHealth{
		GatewayID:                 h.GatewayID().String(),
		GatewayCode:               h.GatewayID().String(),
		Operation:                 operationLabel(h.Operation()),
		State:                     string(h.State()),
		CircuitState:              circuitStateOf(h.State()),
		ErrorRate:                 h.ErrorRate(),
		P99LatencyMs:              int(h.P99Latency().Milliseconds()),
		SampleCount:               total,
		WindowSeconds:             int(domaingateway.HealthWindow / time.Second),
		ConsecutiveProbeSuccesses: h.ConsecutiveProbeSuccesses(),
		Region:                    region,
		StateChangedAt:            h.LastChangedAt(),
	}
	if h.Cooldown() > 0 {
		s := int(h.Cooldown() / time.Second)
		out.CooldownSeconds = &s
	}
	if !h.LastObservedAt().IsZero() {
		out.ObservedAtAgeMs = now.Sub(h.LastObservedAt()).Milliseconds()
	}
	return out
}

// circuitStateOf projects health onto the contract's circuit vocabulary. They are the same
// signal seen from two sides: UNHEALTHY means the breaker is open, PROBING means half-open.
func circuitStateOf(s domaingateway.HealthState) string {
	switch s {
	case domaingateway.HealthUnhealthy:
		return "OPEN"
	case domaingateway.HealthProbing:
		return "HALF_OPEN"
	default:
		return "CLOSED"
	}
}

// --- configuration ------------------------------------------------------------------------------

// ConfigurationVersion is the `ConfigurationVersion` schema: one immutable published version.
type ConfigurationVersion struct {
	ConfigurationID        string                `json:"configurationId"`
	ConfigurationVersionID string                `json:"configurationVersionId"`
	MerchantID             string                `json:"merchantId"`
	Version                int                   `json:"version"`
	PreviousVersion        *int                  `json:"previousVersion,omitempty"`
	RollbackOf             *int                  `json:"rollbackOf,omitempty"`
	Document               MerchantConfiguration `json:"document"`
	Digest                 string                `json:"digest"`
	PublishedBy            string                `json:"publishedBy"`
	PublishedAt            time.Time             `json:"publishedAt"`
}

// ConfigurationRollbackRequest is the `ConfigurationRollbackRequest` schema.
type ConfigurationRollbackRequest struct {
	ToVersion int    `json:"toVersion"`
	Reason    string `json:"reason"`
}

// MerchantConfiguration is the `MerchantConfiguration` schema: the desired-state document.
type MerchantConfiguration struct {
	MerchantID          string           `json:"merchantId"`
	Version             int              `json:"version"`
	Status              string           `json:"status"`
	Environment         string           `json:"environment"`
	SupportedCurrencies []string         `json:"supportedCurrencies"`
	PaymentMethods      []string         `json:"paymentMethods"`
	Countries           []string         `json:"countries"`
	Routing             RoutingConfig    `json:"routing"`
	Risk                RiskConfig       `json:"risk"`
	Limits              LimitsConfig     `json:"limits"`
	Webhooks            WebhookConfig    `json:"webhooks"`
	Settlement          SettlementConfig `json:"settlement"`
	FeatureFlags        *FeatureFlags    `json:"featureFlags,omitempty"`
}

// RoutingConfig is the `RoutingConfig` schema.
type RoutingConfig struct {
	Strategy string          `json:"strategy"`
	Primary  string          `json:"primary"`
	Fallback []string        `json:"fallback,omitempty"`
	Rules    []RoutingRule   `json:"rules,omitempty"`
	Weights  *RoutingWeights `json:"weights,omitempty"`
}

// RoutingRule is the `RoutingRule` schema.
type RoutingRule struct {
	When RoutingRuleCondition `json:"when"`
	Then RoutingRuleOutcome   `json:"then"`
}

// RoutingRuleCondition is the `RoutingRuleCondition` schema.
type RoutingRuleCondition struct {
	Currency      string `json:"currency,omitempty"`
	PaymentMethod string `json:"paymentMethod,omitempty"`
	Country       string `json:"country,omitempty"`
	AmountAbove   *Money `json:"amountAbove,omitempty"`
}

// RoutingRuleOutcome is the `RoutingRuleOutcome` schema.
type RoutingRuleOutcome struct {
	Primary  string   `json:"primary"`
	Fallback []string `json:"fallback,omitempty"`
}

// RoutingWeights is the `RoutingWeights` schema: the four scoring dimensions, each in [0,1].
type RoutingWeights struct {
	Health      float64 `json:"health"`
	SuccessRate float64 `json:"successRate"`
	Cost        float64 `json:"cost"`
	Latency     float64 `json:"latency"`
}

// RiskConfig is the `RiskConfig` schema.
type RiskConfig struct {
	MaxTransactionAmount Money          `json:"maxTransactionAmount"`
	Require3DSAbove      *Money         `json:"require3DSAbove,omitempty"`
	DailyVolumeLimit     Money          `json:"dailyVolumeLimit"`
	Velocity             VelocityLimits `json:"velocity"`
	BlockedCountries     []string       `json:"blockedCountries,omitempty"`
	FallbackDecision     string         `json:"fallbackDecision,omitempty"`
}

// VelocityLimits is the `VelocityLimits` schema.
type VelocityLimits struct {
	MaxPaymentsPerMinute int `json:"maxPaymentsPerMinute"`
	MaxPerCardPerHour    int `json:"maxPerCardPerHour"`
}

// LimitsConfig is the `LimitsConfig` schema.
type LimitsConfig struct {
	MaxRefundWindowDays int `json:"maxRefundWindowDays"`
	MaxPartialCaptures  int `json:"maxPartialCaptures"`
}

// WebhookConfig is the `WebhookConfig` schema.
type WebhookConfig struct {
	Endpoints   []WebhookEndpoint  `json:"endpoints"`
	RetryPolicy WebhookRetryPolicy `json:"retryPolicy"`
}

// WebhookEndpoint is the `WebhookEndpoint` schema. `secretRef` is a reference into the secret
// store; the signing secret itself never crosses this boundary in either direction.
type WebhookEndpoint struct {
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	SecretRef string   `json:"secretRef"`
	Active    bool     `json:"active,omitempty"`
}

// WebhookRetryPolicy is the `WebhookRetryPolicy` schema.
type WebhookRetryPolicy struct {
	MaxAttempts         int    `json:"maxAttempts"`
	Backoff             string `json:"backoff"`
	InitialDelaySeconds int    `json:"initialDelaySeconds,omitempty"`
	MaxDelaySeconds     int    `json:"maxDelaySeconds,omitempty"`
}

// SettlementConfig is the `SettlementConfig` schema.
type SettlementConfig struct {
	Schedule        string `json:"schedule"`
	Currency        string `json:"currency"`
	HoldDays        int    `json:"holdDays"`
	WeeklyAnchorDay string `json:"weeklyAnchorDay,omitempty"`
}

// FeatureFlags is the `FeatureFlags` schema. Only the four the contract declares are exposed:
// the platform carries more internally, and publishing them would make an operational toggle
// into an API surface we then cannot remove.
type FeatureFlags struct {
	NetworkTokens     bool `json:"networkTokens,omitempty"`
	PartialCapture    bool `json:"partialCapture,omitempty"`
	MultiCapture      bool `json:"multiCapture,omitempty"`
	ThreeDsExemptions bool `json:"threeDsExemptions,omitempty"`
}

// ConfigurationVersionOf renders a published configuration together with its version metadata.
func ConfigurationVersionOf(c *config.MerchantConfig) ConfigurationVersion {
	if c == nil {
		return ConfigurationVersion{}
	}
	out := ConfigurationVersion{
		ConfigurationID:        "cfg_" + c.MerchantID.String(),
		ConfigurationVersionID: c.ETag(),
		MerchantID:             c.MerchantID.String(),
		Version:                c.Version,
		Document:               MerchantConfigurationOf(c),
		Digest:                 c.ETag(),
		PublishedBy:            c.CreatedBy,
		PublishedAt:            c.CreatedAt,
	}
	if c.PublishedAt != nil {
		out.PublishedAt = *c.PublishedAt
	}
	if c.PreviousVersion > 0 {
		v := c.PreviousVersion
		out.PreviousVersion = &v
	}
	return out
}

// MerchantConfigurationOf renders the desired-state document.
func MerchantConfigurationOf(c *config.MerchantConfig) MerchantConfiguration {
	if c == nil {
		return MerchantConfiguration{}
	}
	out := MerchantConfiguration{
		MerchantID:  c.MerchantID.String(),
		Version:     c.Version,
		Status:      string(c.Status),
		Environment: string(c.Environment),
		Routing: RoutingConfig{
			Strategy: string(c.Routing.Strategy),
			Primary:  c.Routing.Primary.String(),
		},
		Risk: RiskConfig{
			MaxTransactionAmount: MoneyOf(c.Risk.MaxTransactionAmount),
			DailyVolumeLimit:     MoneyOf(c.Risk.DailyVolumeLimit),
			Velocity: VelocityLimits{
				MaxPaymentsPerMinute: c.Risk.Velocity.MaxPaymentsPerMinute,
				MaxPerCardPerHour:    c.Risk.Velocity.MaxPerCardPerHour,
			},
		},
		Limits: LimitsConfig{
			MaxRefundWindowDays: c.Limits.MaxRefundWindowDays,
			MaxPartialCaptures:  c.Limits.MaxPartialCaptures,
		},
		Webhooks: WebhookConfig{
			Endpoints: []WebhookEndpoint{},
			RetryPolicy: WebhookRetryPolicy{
				MaxAttempts: c.Webhook.MaxAttempts,
				Backoff:     backoffOf(c.Webhook.Backoff),
			},
		},
		Settlement: SettlementConfig{
			Schedule: c.Settle.Schedule,
			Currency: string(c.Settle.Currency),
			HoldDays: c.Settle.HoldDays,
		},
	}
	for _, v := range c.SupportedCurrencies {
		out.SupportedCurrencies = append(out.SupportedCurrencies, string(v))
	}
	for _, v := range c.PaymentMethods {
		out.PaymentMethods = append(out.PaymentMethods, string(v))
	}
	for _, v := range c.Countries {
		out.Countries = append(out.Countries, string(v))
	}
	for _, g := range c.Routing.Fallbacks {
		out.Routing.Fallback = append(out.Routing.Fallback, g.String())
	}
	for _, ep := range c.Webhook.Endpoints {
		out.Webhooks.Endpoints = append(out.Webhooks.Endpoints, WebhookEndpoint{
			URL:       ep.URL,
			Events:    ep.Events,
			SecretRef: ep.SecretRef,
			Active:    ep.Active,
		})
	}
	if len(c.FeatureFlags) > 0 {
		out.FeatureFlags = &FeatureFlags{
			NetworkTokens:     c.FeatureFlags["networkTokens"],
			PartialCapture:    c.FeatureFlags["partialCapture"],
			MultiCapture:      c.FeatureFlags["multiCapture"],
			ThreeDsExemptions: c.FeatureFlags["threeDsExemptions"],
		}
	}
	return out
}

// backoffOf normalises the domain's free-form backoff name onto the contract's enum, defaulting
// to exponential-with-jitter — which is the only backoff that does not synchronise a fleet of
// retrying clients into a thundering herd.
func backoffOf(s string) string {
	switch s {
	case "EXPONENTIAL", "LINEAR":
		return s
	default:
		return "EXPONENTIAL_JITTER"
	}
}

// --- operations ---------------------------------------------------------------------------------

// HealthStatus is the `HealthStatus` schema, returned by all three probes.
type HealthStatus struct {
	Status                   string        `json:"status"`
	Service                  string        `json:"service"`
	Version                  string        `json:"version"`
	Region                   string        `json:"region,omitempty"`
	Checks                   []HealthCheck `json:"checks,omitempty"`
	ConfigSnapshotAgeSeconds int64         `json:"configSnapshotAgeSeconds,omitempty"`
}

// HealthCheck is one dependency's contribution to a probe.
type HealthCheck struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	LatencyMs int    `json:"latencyMs,omitempty"`
}

// WebhookAck is the `WebhookAck` schema: the gateway ingress acknowledgement.
type WebhookAck struct {
	WebhookID  string     `json:"webhookId,omitempty"`
	Received   bool       `json:"received"`
	Duplicate  bool       `json:"duplicate"`
	ReceivedAt *time.Time `json:"receivedAt,omitempty"`
}

// --- onboarding ---------------------------------------------------------------------------------

// OnboardingCase is the `OnboardingCase` schema.
type OnboardingCase struct {
	ID                 string           `json:"id"`
	MerchantID         string           `json:"merchantId"`
	WorkflowInstanceID string           `json:"workflowInstanceId"`
	WorkflowName       string           `json:"workflowName,omitempty"`
	Status             string           `json:"status"`
	MerchantStatus     string           `json:"merchantStatus,omitempty"`
	CurrentStepKey     string           `json:"currentStepKey"`
	BlockedReason      string           `json:"blockedReason,omitempty"`
	SelectedGateways   []string         `json:"selectedGateways,omitempty"`
	Steps              []OnboardingStep `json:"steps"`
	Annotations        []Annotation     `json:"annotations,omitempty"`
	SLADueAt           *time.Time       `json:"slaDueAt,omitempty"`
	OpenedAt           time.Time        `json:"openedAt"`
	ClosedAt           *time.Time       `json:"closedAt,omitempty"`
	Version            int64            `json:"version"`
}

// OnboardingStep is the `OnboardingStep` schema: one step of the twelve-step workflow.
type OnboardingStep struct {
	ID             string     `json:"id"`
	Key            string     `json:"key"`
	Sequence       int        `json:"sequence"`
	State          string     `json:"state"`
	AttemptCount   int        `json:"attemptCount"`
	MaxAttempts    int        `json:"maxAttempts,omitempty"`
	TimeoutSeconds int        `json:"timeoutSeconds,omitempty"`
	AwaitingSignal string     `json:"awaitingSignal,omitempty"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	EndedAt        *time.Time `json:"endedAt,omitempty"`
	ErrorCode      string     `json:"errorCode,omitempty"`
	ErrorMessage   string     `json:"errorMessage,omitempty"`
}

// Annotation is the `Annotation` schema: one L2 validation finding attached to a case.
type Annotation struct {
	RuleID      string     `json:"ruleId"`
	Severity    string     `json:"severity"`
	Message     string     `json:"message"`
	AnnotatedAt *time.Time `json:"annotatedAt,omitempty"`
}

// StartOnboardingRequest is the `StartOnboardingRequest` schema.
type StartOnboardingRequest struct {
	SelectedGateways    []string `json:"selectedGateways"`
	Environment         string   `json:"environment"`
	SupportedCurrencies []string `json:"supportedCurrencies,omitempty"`
	PaymentMethods      []string `json:"paymentMethods,omitempty"`
	NotificationEmail   string   `json:"notificationEmail,omitempty"`
}

// OnboardingSignalRequest is the `OnboardingSignalRequest` schema.
type OnboardingSignalRequest struct {
	Decision       string   `json:"decision"`
	ReviewerID     string   `json:"reviewerId,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	ReasonCodes    []string `json:"reasonCodes,omitempty"`
	AttestationRef string   `json:"attestationRef,omitempty"`
	EvidenceRef    string   `json:"evidenceRef,omitempty"`
	GatewayCode    string   `json:"gatewayCode,omitempty"`
}

// OnboardingSignalAccepted is the `OnboardingSignalAccepted` schema.
//
// The signal is acknowledged, not resolved: the workflow resumes asynchronously. Returning the
// post-signal state synchronously would mean holding the request open across a workflow step
// that may take minutes, which is why the contract says 202 and poll.
type OnboardingSignalAccepted struct {
	CaseID             string    `json:"caseId"`
	WorkflowInstanceID string    `json:"workflowInstanceId"`
	Signal             string    `json:"signal"`
	Accepted           bool      `json:"accepted"`
	Duplicate          bool      `json:"duplicate,omitempty"`
	AuditRecordID      string    `json:"auditRecordId,omitempty"`
	AcceptedAt         time.Time `json:"acceptedAt"`
}
