// Package apierror is the platform's single error representation.
//
// One shape, everywhere. A domain rule violation, a validation failure, a gateway decline and
// an infrastructure outage all become an *Error, and every transport (REST, gRPC, event
// consumer, workflow activity) renders that same value. The alternative — each layer inventing
// its own error type and each boundary translating — is where error semantics go to die: the
// `retryable` bit gets lost somewhere in the middle and a client either hammers a permanent
// failure or gives up on a transient one.
//
// The three fields that carry real weight:
//
//   - Code: a stable, documented, machine-readable identifier. Clients branch on this. It never
//     changes meaning once published; see api/errors/catalog.yaml.
//   - Category: determines the HTTP status, the gRPC code, and the default alerting posture.
//   - Retryable: a machine-readable statement about whether repeating the request could
//     plausibly succeed. Client SDKs, the workflow engine, the outbox relay and the gateway
//     dispatcher all branch on this bit. It is not advisory prose.
//
// Depends only on the standard library. See docs/spec/00-design-baseline.md §20.
package apierror

import (
	"errors"
	"fmt"
	"net/http"
)

// Category classifies an error for transport mapping and alerting.
type Category string

// The category set. Adding one requires updating categoryHTTP, categoryGRPC and the docs.
const (
	CategoryValidation     Category = "VALIDATION"
	CategoryAuthentication Category = "AUTHENTICATION"
	CategoryAuthorization  Category = "AUTHORIZATION"
	CategoryNotFound       Category = "NOT_FOUND"
	CategoryConflict       Category = "CONFLICT"
	CategoryBusinessRule   Category = "BUSINESS_RULE"
	CategoryRateLimit      Category = "RATE_LIMIT"
	CategoryGateway        Category = "GATEWAY"
	CategoryTimeout        Category = "TIMEOUT"
	CategoryInfrastructure Category = "INFRASTRUCTURE"
	CategoryInternal       Category = "INTERNAL"
)

// Code is a stable, published error identifier.
type Code string

// The published code set. §20.2 reserves an excerpt of it; the full catalogue with
// remediation text lives in api/errors/catalog.yaml, and scripts/check-error-catalog.sh
// asserts that this block and that file describe the same error model.
//
// Constant names are the catalogue's `go_const` values verbatim, unabbreviated. The
// catalogue is the published artefact and the generator target, so a shorter name here
// would be a second vocabulary that only this package speaks.
const (
	// Validation and request shape
	CodeValidationFailed       Code = "VALIDATION_FAILED"
	CodeSensitiveDataInRequest Code = "SENSITIVE_DATA_IN_REQUEST"
	CodeMalformedRequest       Code = "MALFORMED_REQUEST"
	CodeUnsupportedMediaType   Code = "UNSUPPORTED_MEDIA_TYPE"
	CodeRequestTooLarge        Code = "REQUEST_TOO_LARGE"
	CodeMissingRequiredHeader  Code = "MISSING_REQUIRED_HEADER"
	CodeInvalidCursor          Code = "INVALID_CURSOR"
	CodeAmountInvalid          Code = "AMOUNT_INVALID"
	CodeCurrencyMismatch       Code = "CURRENCY_MISMATCH"

	// Idempotency and preconditions
	CodeIdempotencyKeyRequired      Code = "IDEMPOTENCY_KEY_REQUIRED"
	CodeIdempotencyKeyReused        Code = "IDEMPOTENCY_KEY_REUSED"
	CodeIdempotentRequestInProgress Code = "IDEMPOTENT_REQUEST_IN_PROGRESS"
	CodePreconditionRequired        Code = "PRECONDITION_REQUIRED"

	// Identity and tenancy
	CodeUnauthenticated          Code = "UNAUTHENTICATED"
	CodeInvalidToken             Code = "INVALID_TOKEN"
	CodeTokenExpired             Code = "TOKEN_EXPIRED"
	CodeForbidden                Code = "FORBIDDEN"
	CodeInsufficientScope        Code = "INSUFFICIENT_SCOPE"
	CodeDualControlRequired      Code = "DUAL_CONTROL_REQUIRED"
	CodeTenantMismatch           Code = "TENANT_MISMATCH"
	CodeMissingTenantContext     Code = "MISSING_TENANT_CONTEXT"
	CodeResidencyPolicyViolation Code = "RESIDENCY_POLICY_VIOLATION"

	// Merchant and onboarding
	CodeMerchantNotFound               Code = "MERCHANT_NOT_FOUND"
	CodeMerchantNotActive              Code = "MERCHANT_NOT_ACTIVE"
	CodeMerchantSuspended              Code = "MERCHANT_SUSPENDED"
	CodeMerchantAlreadyExists          Code = "MERCHANT_ALREADY_EXISTS"
	CodeTerminationBlockedOpenPayments Code = "TERMINATION_BLOCKED_OPEN_PAYMENTS"
	CodeOnboardingCaseNotFound         Code = "ONBOARDING_CASE_NOT_FOUND"
	CodeOnboardingAlreadyInProgress    Code = "ONBOARDING_ALREADY_IN_PROGRESS"
	CodeKYCRequired                    Code = "KYC_REQUIRED"
	CodeComplianceAttestationRequired  Code = "COMPLIANCE_ATTESTATION_REQUIRED"
	CodeCertificationRequired          Code = "CERTIFICATION_REQUIRED"
	CodeCertificationFailed            Code = "CERTIFICATION_FAILED"

	// Payment
	CodePaymentNotFound            Code = "PAYMENT_NOT_FOUND"
	CodeRefundNotFound             Code = "REFUND_NOT_FOUND"
	CodePaymentAlreadyProcessed    Code = "PAYMENT_ALREADY_PROCESSED"
	CodeInvalidStateTransition     Code = "INVALID_STATE_TRANSITION"
	CodeReconciliationPending      Code = "RECONCILIATION_PENDING"
	CodeAmountExceedsLimit         Code = "AMOUNT_EXCEEDS_LIMIT"
	CodeDailyVolumeLimitExceeded   Code = "DAILY_VOLUME_LIMIT_EXCEEDED"
	CodeRefundExceedsCaptured      Code = "REFUND_EXCEEDS_CAPTURED"
	CodeRefundWindowExpired        Code = "REFUND_WINDOW_EXPIRED"
	CodeCaptureExceedsAuthorized   Code = "CAPTURE_EXCEEDS_AUTHORIZED"
	CodeCaptureLimitExceeded       Code = "CAPTURE_LIMIT_EXCEEDED"
	CodePartialCaptureNotSupported Code = "PARTIAL_CAPTURE_NOT_SUPPORTED"
	CodeAuthorizationExpired       Code = "AUTHORIZATION_EXPIRED"
	CodeCurrencyNotSupported       Code = "CURRENCY_NOT_SUPPORTED"
	CodePaymentMethodNotSupported  Code = "PAYMENT_METHOD_NOT_SUPPORTED"

	// Routing and risk
	CodeNoEligibleGateway     Code = "NO_ELIGIBLE_GATEWAY"
	CodeRiskDeclined          Code = "RISK_DECLINED"
	CodeThreeDsRequired       Code = "THREE_DS_REQUIRED"
	CodeVelocityLimitExceeded Code = "VELOCITY_LIMIT_EXCEEDED"
	CodeCountryBlocked        Code = "COUNTRY_BLOCKED"

	// Gateway
	CodeGatewayDeclined              Code = "GATEWAY_DECLINED"
	CodeGatewayTimeout               Code = "GATEWAY_TIMEOUT"
	CodeGatewayUnavailable           Code = "GATEWAY_UNAVAILABLE"
	CodeGatewayContractViolation     Code = "GATEWAY_CONTRACT_VIOLATION"
	CodeGatewayCircuitOpen           Code = "GATEWAY_CIRCUIT_OPEN"
	CodeGatewayAuthenticationFailed  Code = "GATEWAY_AUTHENTICATION_FAILED"
	CodeGatewayNotConfigured         Code = "GATEWAY_NOT_CONFIGURED"
	CodeGatewayNotCertified          Code = "GATEWAY_NOT_CERTIFIED"
	CodeGatewayNotFound              Code = "GATEWAY_NOT_FOUND"
	CodeConnectionNotFound           Code = "CONNECTION_NOT_FOUND"
	CodeCredentialRotationInProgress Code = "CREDENTIAL_ROTATION_IN_PROGRESS"

	// Webhooks
	CodeWebhookSignatureInvalid Code = "WEBHOOK_SIGNATURE_INVALID"
	CodeWebhookReplayDetected   Code = "WEBHOOK_REPLAY_DETECTED"
	CodeWebhookUnknownEventType Code = "WEBHOOK_UNKNOWN_EVENT_TYPE"
	CodeWebhookUnknownGateway   Code = "WEBHOOK_UNKNOWN_GATEWAY"

	// Configuration and workflow
	CodeConfigurationInvalid         Code = "CONFIGURATION_INVALID"
	CodeConfigurationNotFound        Code = "CONFIGURATION_NOT_FOUND"
	CodeConfigurationVersionConflict Code = "CONFIGURATION_VERSION_CONFLICT"
	CodeConfigurationStale           Code = "CONFIGURATION_STALE"
	CodeWorkflowStepFailed           Code = "WORKFLOW_STEP_FAILED"
	CodeWorkflowNotFound             Code = "WORKFLOW_NOT_FOUND"
	CodeWorkflowNotResumable         Code = "WORKFLOW_NOT_RESUMABLE"
	CodeWorkflowSignalNotExpected    Code = "WORKFLOW_SIGNAL_NOT_EXPECTED"

	// Infrastructure
	CodeRateLimited              Code = "RATE_LIMITED"
	CodeConcurrencyLimitExceeded Code = "CONCURRENCY_LIMIT_EXCEEDED"
	CodeServiceUnavailable       Code = "SERVICE_UNAVAILABLE"
	CodeDependencyFailure        Code = "DEPENDENCY_FAILURE"
	CodeInternalError            Code = "INTERNAL_ERROR"
)

// Detail is a field-level or item-level annotation on an error. A validation failure usually
// carries several; a business-rule failure usually carries none.
type Detail struct {
	Field   string `json:"field,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	// RuleID ties the detail back to a validation-plane rule (e.g. "L5.AMOUNT_WITHIN_MERCHANT_LIMIT"),
	// so "why was this rejected" has an answer with a documentation anchor.
	RuleID string `json:"ruleId,omitempty"`
}

// Error is the platform error. It implements error and errors.Unwrap.
//
// Fields are exported because this value is serialized directly; the constructors and the
// With* builders are the intended way to create one.
type Error struct {
	Code      Code     `json:"code"`
	Category  Category `json:"category"`
	Message   string   `json:"message"`
	Detail    string   `json:"detail,omitempty"`
	Retryable bool     `json:"retryable"`
	Details   []Detail `json:"details,omitempty"`

	// RetryAfterSeconds is populated for 429 and for 409 in-progress responses. Zero means
	// "no guidance"; a client should then use its own backoff.
	RetryAfterSeconds int `json:"retryAfterSeconds,omitempty"`

	// RequestID and TraceID are filled in at the transport boundary, not by the raiser.
	RequestID string `json:"requestId,omitempty"`
	TraceID   string `json:"traceId,omitempty"`

	// cause is the wrapped underlying error. It is unexported and never serialized: an
	// internal error string is exactly the kind of thing that leaks a table name, a hostname
	// or a credential fragment to a caller. It reaches the logs, never the response.
	cause error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.cause != nil {
		return string(e.Code) + ": " + e.Message + ": " + e.cause.Error()
	}
	return string(e.Code) + ": " + e.Message
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.cause }

// Is supports errors.Is comparison by Code, so `errors.Is(err, apierror.New(CodeX, ""))`
// matches any error carrying that code regardless of message.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return t.Code == e.Code
}

// New constructs an Error, deriving the category and retryability from the code registry.
func New(code Code, message string) *Error {
	spec := lookup(code)
	return &Error{
		Code:      code,
		Category:  spec.category,
		Message:   orDefault(message, spec.message),
		Retryable: spec.retryable,
	}
}

// Newf is New with formatting.
func Newf(code Code, format string, args ...any) *Error {
	return New(code, fmt.Sprintf(format, args...))
}

// Wrap attaches an underlying cause. The cause is logged, never serialized.
func Wrap(cause error, code Code, message string) *Error {
	if cause == nil {
		return New(code, message)
	}
	// If the cause is already one of ours, preserve the innermost code: re-wrapping a
	// GATEWAY_TIMEOUT as an INTERNAL_ERROR at every layer is how a retryable condition
	// becomes a permanent one.
	var existing *Error
	if errors.As(cause, &existing) && code == CodeInternalError {
		e := *existing
		e.cause = cause
		if message != "" {
			e.Detail = message
		}
		return &e
	}
	e := New(code, message)
	e.cause = cause
	return e
}

// Wrapf is Wrap with formatting.
func Wrapf(cause error, code Code, format string, args ...any) *Error {
	return Wrap(cause, code, fmt.Sprintf(format, args...))
}

// WithDetail returns a copy carrying an additional field-level detail.
func (e *Error) WithDetail(d Detail) *Error {
	c := *e
	c.Details = append(append([]Detail(nil), e.Details...), d)
	return &c
}

// WithDetails returns a copy carrying the given details.
func (e *Error) WithDetails(ds ...Detail) *Error {
	c := *e
	c.Details = append(append([]Detail(nil), e.Details...), ds...)
	return &c
}

// WithRetryAfter returns a copy carrying retry guidance in seconds.
func (e *Error) WithRetryAfter(seconds int) *Error {
	c := *e
	c.RetryAfterSeconds = seconds
	return &c
}

// WithMessage returns a copy with a caller-facing message override.
func (e *Error) WithMessage(msg string) *Error {
	c := *e
	c.Message = msg
	return &c
}

// WithCorrelation returns a copy stamped with request and trace identifiers. Called once, at
// the transport boundary.
func (e *Error) WithCorrelation(requestID, traceID string) *Error {
	c := *e
	c.RequestID, c.TraceID = requestID, traceID
	return &c
}

// HTTPStatus returns the HTTP status for this error.
func (e *Error) HTTPStatus() int {
	if s, ok := codeHTTPOverride[e.Code]; ok {
		return s
	}
	return categoryHTTP(e.Category)
}

// GRPCCode returns the gRPC status code as an integer, avoiding a dependency on the grpc
// module from this stdlib-only package. Values match google.golang.org/grpc/codes.
func (e *Error) GRPCCode() int {
	if c, ok := codeGRPCOverride[e.Code]; ok {
		return c
	}
	return categoryGRPC(e.Category)
}

// TypeURI returns the RFC 9457 `type` URI for the problem document.
func (e *Error) TypeURI() string { return "https://errors.payments-platform.io/" + string(e.Code) }

// DocsURL returns the human documentation anchor for this code.
func (e *Error) DocsURL() string { return "https://docs.payments-platform.io/errors#" + string(e.Code) }

// IsRetryable reports whether err is, or wraps, a retryable platform error. Non-platform
// errors are treated as non-retryable: an unclassified error is one nobody has reasoned about,
// and retrying it is a guess.
func IsRetryable(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Retryable
	}
	return false
}

// CodeOf returns the code of err, or CodeInternalError if err is not a platform error.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternalError
}

// CategoryOf returns the category of err, or CategoryInternal.
func CategoryOf(err error) Category {
	var e *Error
	if errors.As(err, &e) {
		return e.Category
	}
	return CategoryInternal
}

// HTTPStatusOf returns the HTTP status for any error, defaulting to 500.
func HTTPStatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.HTTPStatus()
	}
	return http.StatusInternalServerError
}

// From coerces any error into an *Error. A nil error returns nil. An error that is already an
// *Error is returned as-is. Anything else becomes an INTERNAL_ERROR wrapping the original —
// which means the original text is available in logs and absent from the response.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Wrap(err, CodeInternalError, "an unexpected internal error occurred")
}

// --- code registry -------------------------------------------------------------------------

type spec struct {
	category  Category
	retryable bool
	message   string
}

func lookup(c Code) spec {
	if s, ok := registry[c]; ok {
		return s
	}
	// An unregistered code is a programming error. Rather than panic in a request path, treat
	// it as internal and let the CI check (TestEveryCodeIsRegistered) catch it before release.
	return spec{category: CategoryInternal, retryable: false, message: "unclassified error"}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// codeHTTPOverride holds the few codes whose HTTP status differs from their category
// default. Every entry here is a divergence the published catalogue also states, and
// scripts/check-error-catalog.sh compares the two.
var codeHTTPOverride = map[Code]int{
	CodeValidationFailed:       http.StatusBadRequest,
	CodeMalformedRequest:       http.StatusBadRequest,
	CodeSensitiveDataInRequest: http.StatusBadRequest,
	CodeMissingRequiredHeader:  http.StatusBadRequest,
	CodeIdempotencyKeyRequired: http.StatusBadRequest,
	CodeUnsupportedMediaType:   http.StatusUnsupportedMediaType,
	CodeRequestTooLarge:        http.StatusRequestEntityTooLarge,

	// 422, not the category's 400: the request parses and validates, and the conflict is
	// semantic. That is what distinguishes it from a malformed body for a caller deciding
	// whether to fix its serializer or its key management.
	CodeIdempotencyKeyReused: http.StatusUnprocessableEntity,

	CodeIdempotentRequestInProgress: http.StatusConflict,

	// 428, not the category's 409. 409 says "you conflicted"; 428 says "you did not declare
	// what you expected", and those are different fixes for the caller.
	CodePreconditionRequired: http.StatusPreconditionRequired,

	CodeConfigurationVersionConflict: http.StatusPreconditionFailed,
	CodeInvalidStateTransition:       http.StatusConflict,

	// 409, not BUSINESS_RULE's 422. Baseline §20's worked example carries this code with
	// status 409, and so do the OpenAPI examples: the caller's request was fine, the
	// resource had already moved on.
	CodePaymentAlreadyProcessed: http.StatusConflict,

	// 409 for the same reason: the merchant's state, not the request, is what refused.
	CodeMerchantNotActive: http.StatusConflict,

	CodeOnboardingAlreadyInProgress: http.StatusConflict,
	CodeNoEligibleGateway:           http.StatusServiceUnavailable,
	CodeGatewayTimeout:              http.StatusGatewayTimeout,
	CodeThreeDsRequired:             http.StatusUnprocessableEntity,

	// 402 Payment Required, not GATEWAY's 502. A hard decline is a complete, correct answer
	// from the gateway; 502 would say the gateway malfunctioned and invite a retry.
	CodeGatewayDeclined: http.StatusPaymentRequired,
}

// codeGRPCOverride is the gRPC half of the same idea, and it exists because the category
// default is a retry signal in every gRPC client library. UNAVAILABLE and ABORTED are both
// retried automatically by the standard interceptors; FAILED_PRECONDITION is not. A code
// whose whole meaning is "do not repeat this until you change something" must therefore not
// inherit a category default that tells the generated client to repeat it.
var codeGRPCOverride = map[Code]int{
	// CONFLICT defaults to ABORTED, which means "concurrency conflict, retry the whole
	// transaction". These two are optimistic-concurrency failures where a blind retry would
	// re-apply a write against a version the caller has not seen. The caller must re-read.
	CodeConfigurationVersionConflict: grpcFailedPrecond,
	CodeInvalidStateTransition:       grpcFailedPrecond,

	// The caller has not supplied If-Match at all; retrying the identical call cannot help.
	CodePreconditionRequired: grpcFailedPrecond,

	// GATEWAY defaults to UNAVAILABLE. A hard decline is final: retrying it burns the
	// issuer's decline counters and, with a fresh idempotency key, risks a double charge.
	CodeGatewayDeclined: grpcFailedPrecond,
}

func categoryHTTP(c Category) int {
	switch c {
	case CategoryValidation:
		return http.StatusUnprocessableEntity
	case CategoryAuthentication:
		return http.StatusUnauthorized
	case CategoryAuthorization:
		return http.StatusForbidden
	case CategoryNotFound:
		return http.StatusNotFound
	case CategoryConflict:
		return http.StatusConflict
	case CategoryBusinessRule:
		return http.StatusUnprocessableEntity
	case CategoryRateLimit:
		return http.StatusTooManyRequests
	case CategoryGateway:
		return http.StatusBadGateway
	case CategoryTimeout:
		return http.StatusGatewayTimeout
	case CategoryInfrastructure:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// gRPC canonical codes, mirrored to avoid importing the grpc module here.
const (
	grpcOK                = 0
	grpcInvalidArgument   = 3
	grpcDeadlineExceeded  = 4
	grpcNotFound          = 5
	grpcPermissionDenied  = 7
	grpcResourceExhausted = 8
	grpcFailedPrecond     = 9
	grpcAborted           = 10
	grpcInternal          = 13
	grpcUnavailable       = 14
	grpcUnauthenticated   = 16
)

func categoryGRPC(c Category) int {
	switch c {
	case CategoryValidation:
		return grpcInvalidArgument
	case CategoryAuthentication:
		return grpcUnauthenticated
	case CategoryAuthorization:
		return grpcPermissionDenied
	case CategoryNotFound:
		return grpcNotFound
	case CategoryConflict:
		return grpcAborted
	case CategoryBusinessRule:
		return grpcFailedPrecond
	case CategoryRateLimit:
		return grpcResourceExhausted
	case CategoryGateway, CategoryInfrastructure:
		return grpcUnavailable
	case CategoryTimeout:
		return grpcDeadlineExceeded
	default:
		return grpcInternal
	}
}

// registry is the authoritative in-code mirror of api/errors/catalog.yaml. The CI check
// scripts/check-error-catalog.sh asserts the two agree, so a code cannot be published in the
// API contract without being classified here, or vice versa.
//
// The `retryable` column is the one to read carefully. It is not a hint: client SDKs, the
// workflow engine, the outbox relay and the gateway dispatcher all branch on it (§20.1), so
// a wrong value here becomes a duplicate charge or a stuck payment in code we do not own.
var registry = map[Code]spec{
	// --- Validation and request shape ---
	CodeValidationFailed:       {CategoryValidation, false, "the request failed validation"},
	CodeSensitiveDataInRequest: {CategoryValidation, false, "the request appears to contain a primary account number; card data must be tokenized at the gateway edge and must never be sent to this API"},
	CodeMalformedRequest:       {CategoryValidation, false, "the request body could not be parsed"},
	CodeUnsupportedMediaType:   {CategoryValidation, false, "content type must be application/json"},
	CodeRequestTooLarge:        {CategoryValidation, false, "the request body exceeds the maximum permitted size"},
	CodeMissingRequiredHeader:  {CategoryValidation, false, "a required header is missing"},
	CodeInvalidCursor:          {CategoryValidation, false, "the pagination cursor is not valid"},
	CodeAmountInvalid:          {CategoryValidation, false, "the amount is not valid"},
	// A refund whose currency differs from its payment's is a malformed request, not a
	// business-rule refusal: there is no configuration a merchant could change to make it
	// legal. CURRENCY_NOT_SUPPORTED, below, is the configuration one.
	CodeCurrencyMismatch: {CategoryValidation, false, "the currency does not match the currency of the payment being operated on"},

	// --- Idempotency and preconditions ---
	CodeIdempotencyKeyRequired: {CategoryValidation, false, "this operation requires an Idempotency-Key header"},
	// VALIDATION, not CONFLICT. Nothing about the resource's state refused this request and
	// no concurrent writer is involved; the caller sent a key that contradicts what it sent
	// before, and the fix is entirely in the caller's request. §20.1's CONFLICT is "conflicts
	// with the resource's current state or with a concurrent request", and this is neither.
	CodeIdempotencyKeyReused: {CategoryValidation, false, "this idempotency key was already used with a different request body"},
	// The one retryable member of CONFLICT, and it says so on its own entry (§20.1).
	CodeIdempotentRequestInProgress: {CategoryConflict, true, "a request with this idempotency key is currently in progress"},
	CodePreconditionRequired:        {CategoryConflict, false, "this operation requires an If-Match header carrying a prior ETag"},

	// --- Identity and tenancy ---
	CodeUnauthenticated:          {CategoryAuthentication, false, "authentication is required"},
	CodeInvalidToken:             {CategoryAuthentication, false, "the access token is invalid"},
	CodeTokenExpired:             {CategoryAuthentication, false, "the access token has expired"},
	CodeForbidden:                {CategoryAuthorization, false, "the caller is not permitted to perform this operation"},
	CodeInsufficientScope:        {CategoryAuthorization, false, "the access token lacks a required scope"},
	CodeDualControlRequired:      {CategoryAuthorization, false, "this operation requires a second, distinct approver"},
	CodeTenantMismatch:           {CategoryAuthorization, false, "the requested resource belongs to a different tenant"},
	CodeMissingTenantContext:     {CategoryInternal, false, "tenant context was not established for this operation"},
	CodeResidencyPolicyViolation: {CategoryBusinessRule, false, "the operation would move data outside its permitted residency region"},

	// --- Merchant and onboarding ---
	CodeMerchantNotFound:               {CategoryNotFound, false, "merchant not found"},
	CodeMerchantNotActive:              {CategoryBusinessRule, false, "the merchant is not active and cannot accept payments"},
	CodeMerchantSuspended:              {CategoryBusinessRule, false, "the merchant is suspended"},
	CodeMerchantAlreadyExists:          {CategoryConflict, false, "a merchant with this external reference already exists in this tenant"},
	CodeTerminationBlockedOpenPayments: {CategoryBusinessRule, false, "the merchant has open payments; settle, refund or void them before terminating"},
	CodeOnboardingCaseNotFound:         {CategoryNotFound, false, "onboarding case not found"},
	CodeOnboardingAlreadyInProgress:    {CategoryConflict, false, "onboarding is already in progress for this merchant"},
	CodeKYCRequired:                    {CategoryBusinessRule, false, "know-your-business verification must be completed first"},
	CodeComplianceAttestationRequired:  {CategoryBusinessRule, false, "a current compliance attestation is required for this operation"},
	CodeCertificationRequired:          {CategoryBusinessRule, false, "the gateway connection has not been certified"},
	CodeCertificationFailed:            {CategoryBusinessRule, false, "gateway certification did not pass"},

	// --- Payment ---
	CodePaymentNotFound: {CategoryNotFound, false, "payment not found"},
	CodeRefundNotFound:  {CategoryNotFound, false, "refund not found on this payment"},
	// BUSINESS_RULE with a 409 override, matching §20's worked example. The request was
	// well-formed and permitted; the payment had already reached a state in which the
	// operation has no meaning.
	CodePaymentAlreadyProcessed: {CategoryBusinessRule, false, "the payment has already been processed"},
	CodeInvalidStateTransition:  {CategoryConflict, false, "the requested operation is not valid for the payment's current state"},
	// Not retryable: the payment has an attempt whose outcome is unknown, and the reconciler
	// — not the caller — is what resolves it. A caller that retried would be issuing a second
	// authorization against a first one that may already have succeeded.
	CodeReconciliationPending:    {CategoryConflict, false, "the payment has an unresolved attempt and is awaiting reconciliation"},
	CodeAmountExceedsLimit:       {CategoryBusinessRule, false, "the amount exceeds a configured limit"},
	CodeDailyVolumeLimitExceeded: {CategoryBusinessRule, false, "the merchant's configured daily volume limit would be exceeded"},
	CodeRefundExceedsCaptured:    {CategoryBusinessRule, false, "the refund amount exceeds the captured amount"},
	CodeRefundWindowExpired:      {CategoryBusinessRule, false, "the refund window for this payment has expired"},
	CodeCaptureExceedsAuthorized: {CategoryBusinessRule, false, "the capture amount exceeds the authorized amount"},
	// Distinct from CAPTURE_EXCEEDS_AUTHORIZED: that one is about money, this one is
	// about how many times the authorization may be drawn against.
	CodeCaptureLimitExceeded:       {CategoryBusinessRule, false, "the payment has used every partial capture permitted by its configuration"},
	CodePartialCaptureNotSupported: {CategoryBusinessRule, false, "the routed gateway does not support partial capture"},
	CodeAuthorizationExpired:       {CategoryBusinessRule, false, "the authorization has expired and can no longer be captured"},
	CodeCurrencyNotSupported:       {CategoryBusinessRule, false, "the currency is not enabled for this merchant"},
	CodePaymentMethodNotSupported:  {CategoryBusinessRule, false, "the payment method is not enabled for this merchant"},

	// --- Routing and risk ---
	CodeNoEligibleGateway:     {CategoryInfrastructure, true, "no gateway is currently eligible to process this payment"},
	CodeRiskDeclined:          {CategoryBusinessRule, false, "the payment was declined by risk policy"},
	CodeThreeDsRequired:       {CategoryBusinessRule, false, "strong customer authentication is required for this payment"},
	CodeVelocityLimitExceeded: {CategoryBusinessRule, false, "a velocity limit was exceeded"},
	CodeCountryBlocked:        {CategoryBusinessRule, false, "the country is blocked by policy"},

	// --- Gateway ---
	// GATEWAY, not BUSINESS_RULE: the refusal originated at the third party, and §20.1's
	// GATEWAY row contemplates exactly this case ("retryable yes, unless a hard decline").
	// The platform's own refusals are RISK_DECLINED and the limit codes above. Retryable is
	// false and the gRPC code is overridden away from UNAVAILABLE for the same reason: a
	// decline is a complete answer, and repeating it burns issuer decline counters.
	CodeGatewayDeclined: {CategoryGateway, false, "the payment was declined by the payment gateway"},
	CodeGatewayTimeout:  {CategoryTimeout, false, "the payment gateway did not respond in time; the outcome is unknown and is being reconciled"},
	// Retryable: the gateway is down, and the next attempt may reach a healthy node.
	CodeGatewayUnavailable: {CategoryGateway, true, "the payment gateway is unavailable"},
	// NOT retryable, and this is the entry to be most careful about. The gateway answered;
	// its answer failed L6 validation — an echoed amount that does not match what we sent, a
	// currency that changed case and value, a missing reference. Asking the same gateway the
	// same question does not make the previous answer valid, and the payment has already been
	// left for reconciliation because the true outcome is unknown. A `retryable: true` here
	// tells every generated SDK and the workflow engine to re-issue against a gateway that
	// has just demonstrated it may have moved money it did not correctly report: that is the
	// double-charge path.
	CodeGatewayContractViolation: {CategoryGateway, false, "the gateway response failed validation"},
	// INFRASTRUCTURE, not GATEWAY. Nothing was sent to the gateway: our own breaker refused
	// to place the call. §20.1's INFRASTRUCTURE is "the platform cannot serve the request and
	// fails closed", which is precisely a tripped breaker, and its 503 is the honest status —
	// a 502 would attribute to the third party a failure that is ours to clear.
	CodeGatewayCircuitOpen:          {CategoryInfrastructure, true, "the circuit breaker for this gateway is open"},
	CodeGatewayAuthenticationFailed: {CategoryGateway, false, "authentication with the payment gateway failed"},
	CodeGatewayNotConfigured:        {CategoryBusinessRule, false, "the gateway is not configured for this merchant"},
	CodeGatewayNotCertified:         {CategoryBusinessRule, false, "the gateway is not certified for this merchant and cannot be routed to"},
	CodeGatewayNotFound:             {CategoryNotFound, false, "gateway not found"},
	CodeConnectionNotFound:          {CategoryNotFound, false, "gateway connection not found for this merchant"},
	CodeCredentialRotationInProgress: {CategoryConflict, false,
		"a credential rotation is already in progress for this connection"},

	// --- Webhooks ---
	CodeWebhookSignatureInvalid: {CategoryAuthentication, false, "the webhook signature could not be verified"},
	CodeWebhookReplayDetected:   {CategoryAuthentication, false, "the webhook appears to be a replay"},
	CodeWebhookUnknownEventType: {CategoryValidation, false, "unrecognised webhook event type"},
	CodeWebhookUnknownGateway:   {CategoryNotFound, false, "no gateway is registered under that webhook path"},

	// --- Configuration and workflow ---
	CodeConfigurationInvalid:         {CategoryValidation, false, "the configuration is not valid"},
	CodeConfigurationNotFound:        {CategoryNotFound, false, "no configuration exists for this merchant"},
	CodeConfigurationVersionConflict: {CategoryConflict, false, "the configuration was modified by another request"},
	CodeConfigurationStale:           {CategoryInfrastructure, true, "the local configuration snapshot is too stale to serve this request safely"},
	// INTERNAL, not INFRASTRUCTURE, and not retryable. By the time a step failure is
	// transported to a caller, the workflow engine has already applied its own retry
	// classification and given up; the code therefore names the terminal case. Publishing it
	// as a retryable INFRASTRUCTURE 503 would invite the caller to re-drive a saga that the
	// engine has already decided is dead, on top of an incident that pages.
	CodeWorkflowStepFailed:        {CategoryInternal, false, "a workflow step failed"},
	CodeWorkflowNotFound:          {CategoryNotFound, false, "workflow instance not found"},
	CodeWorkflowNotResumable:      {CategoryConflict, false, "the workflow instance is not in a resumable state"},
	CodeWorkflowSignalNotExpected: {CategoryConflict, false, "the workflow instance is not waiting for this signal"},

	// --- Infrastructure ---
	CodeRateLimited:              {CategoryRateLimit, true, "rate limit exceeded"},
	CodeConcurrencyLimitExceeded: {CategoryRateLimit, true, "too many concurrent requests"},
	CodeServiceUnavailable:       {CategoryInfrastructure, true, "the service is temporarily unavailable"},
	CodeDependencyFailure:        {CategoryInfrastructure, true, "a downstream dependency failed"},
	CodeInternalError:            {CategoryInternal, false, "an unexpected internal error occurred"},
}

// AllCodes returns every registered code. Used by the catalog consistency check and by
// documentation generation.
func AllCodes() []Code {
	out := make([]Code, 0, len(registry))
	for c := range registry {
		out = append(out, c)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Spec exposes the classification of a code for tooling.
func Spec(c Code) (category Category, retryable bool, defaultMessage string, ok bool) {
	s, found := registry[c]
	return s.category, s.retryable, s.message, found
}
