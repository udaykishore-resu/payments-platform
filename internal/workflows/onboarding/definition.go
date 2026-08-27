// Package onboarding is `merchant-onboarding@v1`: the twelve-step saga that takes a merchant
// from CREATED to ACTIVE, and the activities that implement it.
//
// It depends on the engine *port*, never on an engine implementation, which is what lets the
// same definition run on the Postgres engine today and on Temporal tomorrow without a line of
// this package changing. It depends on the application's capability ports — KYC, bank
// validation, secrets, object storage, gateway provisioning — never on their adapters, which is
// what lets every step be tested against a double.
//
// The shape of the saga, and why it is that shape (docs/automation-plane.md §2.3):
//
//	1  validate-merchant       pure, runs first, so a malformed merchant is rejected *before*
//	                           any external side effect exists to compensate
//	2  submit-kyc              compensatable: cancel the vendor case
//	3  await-kyc-decision      ◆ PIVOT (retained): the decision is a regulated record kept for
//	                           five years. Nothing before it is compensatable afterwards
//	4  validate-bank-account   read-only; nothing was created, so nothing to undo
//	5  provision-gateways      compensatable: de-provision the sub-account
//	6  store-credentials       compensatable: delete the secret version
//	7  register-webhooks       compensatable: delete the registration
//	8  apply-configuration     compensatable: roll back, which itself publishes a new version
//	9  sandbox-validation      sandbox transactions are inert; nothing to undo
//	10 certification           the report is immutable evidence and is meant to survive
//	11 compliance-review       a human decision is a record, not a mutation
//	12 activate                ◆ PIVOT (irreversible): real payments can now exist. The declared
//	                           compensation is *suspend*, which is forward recovery, not rollback
//
// Two pivots, two different kinds of irreversibility, and conflating them produces a wrong
// design. Step 3's is external and regulatory: cancelling the case stops the process but cannot
// un-submit the data. Step 12's is money-path: once the merchant is ACTIVE, payments exist with
// their own lifecycles, and "undoing" activation by blocking refunds would trap merchant money.
package onboarding

import (
	"encoding/json"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// WorkflowName and WorkflowVersion identify the definition. The business key is the merchant ID,
// which is what guarantees one live onboarding per merchant.
const (
	WorkflowName    = "merchant-onboarding"
	WorkflowVersion = 1
)

// Step names. They are the strings in `workflow_instances.current_step`, in every metric label,
// in every log line and on the operator's screen, so they are stable identifiers rather than
// prose and they match baseline §11 exactly.
const (
	StepValidateMerchant    = "validate-merchant"
	StepSubmitKYC           = "submit-kyc"
	StepAwaitKYCDecision    = "await-kyc-decision"
	StepValidateBankAccount = "validate-bank-account"
	StepProvisionGateways   = "provision-gateways"
	StepStoreCredentials    = "store-credentials"
	StepRegisterWebhooks    = "register-webhooks"
	StepApplyConfiguration  = "apply-configuration"
	StepSandboxValidation   = "sandbox-validation"
	StepCertification       = "certification"
	StepComplianceReview    = "compliance-review"
	StepActivate            = "activate"
)

// Compensation activity names. Each is the *undo* of the step it is named for, and each is
// idempotent and tolerant of the forward operation never having happened — compensation runs
// after crashes, and the crash may have happened before the thing being undone was created.
const (
	CompCancelKYCCase       = "cancel-kyc-case"
	CompDeprovisionGateways = "deprovision-gateways"
	CompDeleteSecrets       = "delete-secret-versions"
	CompDeleteWebhooks      = "delete-webhook-registrations"
	CompRollbackConfig      = "rollback-configuration"
	CompSuspendMerchant     = "suspend-merchant"
)

// Signal names. Both are delivered through the operator surface with an Idempotency-Key and the
// `onboarding:approve` scope, and both are audited with the principal who sent them.
const (
	SignalKYCDecision        = "kyc-decision"
	SignalComplianceApproval = "compliance-approval"
)

// Input is the document Start is given. It is immutable for the instance's life and is handed
// unchanged to every step, which is what makes a DLQ entry replayable without reconstructing
// anything.
type Input struct {
	MerchantID  shared.MerchantID  `json:"merchantId"`
	TenantID    shared.TenantID    `json:"tenantId"`
	Environment shared.Environment `json:"environment"`

	// Gateways is the merchant's selected set. Provisioning fans out over it, bounded at four.
	Gateways []shared.GatewayID `json:"gateways"`

	Countries      []shared.Country       `json:"countries"`
	Currencies     []money.Currency       `json:"currencies"`
	PaymentMethods []shared.PaymentMethod `json:"paymentMethods"`

	// BankAccountID names the settlement account to verify. The account details themselves live
	// in the secrets store and are resolved by reference at the moment of use; they never enter
	// the workflow context, and therefore never enter a workflow diagnostic export.
	BankAccountID string `json:"bankAccountId"`

	// RequestedBy is the principal who started the onboarding, for the audit trail.
	RequestedBy string `json:"requestedBy"`
}

// BusinessKey returns the merchant ID.
func (in Input) BusinessKey() string { return string(in.MerchantID) }

// Validate rejects an input that cannot possibly onboard, before an instance exists.
func (in Input) Validate() error {
	var details []apierror.Detail
	if in.MerchantID.IsZero() {
		details = append(details, detail("merchantId", "REQUIRED", "onboarding is scoped to one merchant", "L2.MERCHANT_PRESENT"))
	}
	if len(in.Gateways) == 0 {
		details = append(details, detail("gateways", "REQUIRED",
			"at least one gateway must be selected or there is nothing to provision", "L2.AT_LEAST_ONE_GATEWAY"))
	}
	if len(in.Currencies) == 0 {
		details = append(details, detail("currencies", "REQUIRED",
			"at least one settlement currency is required", "L2.AT_LEAST_ONE_CURRENCY"))
	}
	if len(in.PaymentMethods) == 0 {
		details = append(details, detail("paymentMethods", "REQUIRED",
			"at least one payment method is required", "L2.AT_LEAST_ONE_METHOD"))
	}
	if !in.Environment.IsValid() {
		details = append(details, detail("environment", "INVALID",
			"must be sandbox or production; getting this wrong is how a certification run charges a real card",
			"L2.ENVIRONMENT_VALID"))
	}
	if len(details) == 0 {
		return nil
	}
	return apierror.New(apierror.CodeValidationFailed, "the onboarding request is not valid").
		WithDetails(details...)
}

func detail(field, code, msg, rule string) apierror.Detail {
	return apierror.Detail{Field: field, Code: code, Message: msg, RuleID: rule}
}

// Definition returns `merchant-onboarding@v1`.
//
// Every number here is from baseline §11 and its expansion in docs/automation-plane.md §3, and
// each one is a decision rather than a default:
//
//   - **Timeouts are per attempt, not per step lifetime.** Certification's thirty minutes is one
//     run of the matrix; the step may legitimately take an hour across two attempts.
//   - **`n × exp a→b` means n total executions**, not n retries. Off-by-one here is the
//     difference between a vendor seeing five requests and six during an outage, multiplied by
//     the whole backlog.
//   - **Every retrying step is marked Idempotent**, and Validate refuses a definition where that
//     is not true, because the retry *is* the duplicate side effect otherwise.
//   - **SideEffecting drives the ambiguity rule.** A timeout on step 1 is transient because
//     nothing external could have happened; the identical timeout on step 2 is ambiguous,
//     because the KYC vendor may have created the case, and the next attempt must look before
//     it acts.
func Definition() *engine.Definition {
	return &engine.Definition{
		Name:        WorkflowName,
		Version:     WorkflowVersion,
		Description: "takes a merchant from CREATED to ACTIVE (baseline §11)",
		BusinessKeyOf: func(raw []byte) (string, error) {
			var in Input
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", apierror.Wrap(err, apierror.CodeMalformedRequest,
					"the onboarding input does not decode")
			}
			if err := in.Validate(); err != nil {
				return "", err
			}
			return in.BusinessKey(), nil
		},
		Steps: []engine.Step{
			{
				Name:        StepValidateMerchant,
				Activity:    StepValidateMerchant,
				Description: "validation plane L2 — pure",
				Timeout:     5 * time.Second,
				// Three attempts at 200 ms: a pure step retries only to absorb a transient
				// database read, so the interval is short and the cap is the interval.
				Retry:      engine.RetryPolicy{MaxAttempts: 3, InitialInterval: 200 * time.Millisecond, MaxInterval: 200 * time.Millisecond, BackoffFactor: 2},
				Idempotent: true,
				// No compensation: nothing was created. That is a positive declaration, reviewed
				// as such, not an omission.
			},
			{
				Name:          StepSubmitKYC,
				Activity:      StepSubmitKYC,
				Description:   "submit the KYB case to the identity vendor",
				Timeout:       30 * time.Second,
				Retry:         engine.RetryPolicy{MaxAttempts: 5, InitialInterval: time.Second, MaxInterval: 60 * time.Second, BackoffFactor: 2},
				Idempotent:    true,
				SideEffecting: true,
				Compensation:  CompCancelKYCCase,
			},
			{
				Name:        StepAwaitKYCDecision,
				Activity:    StepAwaitKYCDecision,
				ManualGate:  true,
				Signal:      SignalKYCDecision,
				Description: "◆ PIVOT (retained) — the vendor's decision, delivered by webhook or by an operator",
				// Seven days. The lease is released for the whole of it: this wait holds no
				// worker resource at all, which is the only reason a seven-day step is sane.
				Timeout: 7 * 24 * time.Hour,
				// Compensatable only while the case is still pending; once the decision lands
				// the record is retained by law and the engine skips this and everything before
				// it. Declaring it anyway is correct — an abort *before* the decision must
				// genuinely cancel the case.
				Compensation: CompCancelKYCCase,
				Pivot:        true,
				PivotKind:    engine.PivotRetained,
			},
			{
				Name:          StepValidateBankAccount,
				Activity:      StepValidateBankAccount,
				Description:   "verify the settlement account",
				Timeout:       30 * time.Second,
				Retry:         engine.RetryPolicy{MaxAttempts: 5, InitialInterval: time.Second, MaxInterval: 60 * time.Second, BackoffFactor: 2},
				Idempotent:    true,
				SideEffecting: true,
				// No compensation: a verification creates nothing. It is side-effecting all the
				// same, because a penny-drop *does* move money, and a duplicate submission would
				// initiate a second micro-deposit — which is exactly why the ambiguity rule
				// applies to it.
			},
			{
				Name:          StepProvisionGateways,
				Activity:      StepProvisionGateways,
				Description:   "fan out over the selected gateways, bounded at 4",
				Timeout:       60 * time.Second,
				Retry:         engine.RetryPolicy{MaxAttempts: 5, InitialInterval: time.Second, MaxInterval: 60 * time.Second, BackoffFactor: 2},
				Idempotent:    true,
				SideEffecting: true,
				FanOut:        true,
				Compensation:  CompDeprovisionGateways,
			},
			{
				Name:          StepStoreCredentials,
				Activity:      StepStoreCredentials,
				Description:   "write gateway credentials to the secrets store; output carries references only",
				Timeout:       10 * time.Second,
				Retry:         engine.RetryPolicy{MaxAttempts: 3, InitialInterval: time.Second, MaxInterval: 10 * time.Second, BackoffFactor: 2},
				Idempotent:    true,
				SideEffecting: true,
				Compensation:  CompDeleteSecrets,
			},
			{
				Name:        StepRegisterWebhooks,
				Activity:    StepRegisterWebhooks,
				Description: "subscribe our ingress to each gateway's events, before any sandbox transaction",
				Timeout:     30 * time.Second,
				Retry:       engine.RetryPolicy{MaxAttempts: 5, InitialInterval: time.Second, MaxInterval: 60 * time.Second, BackoffFactor: 2},
				Idempotent:  true,
				// Registered *before* certification on purpose: certification asserts that a
				// webhook is received, signature-verified and moves the payment state, and
				// registering afterwards would make that assertion unverifiable.
				SideEffecting: true,
				Compensation:  CompDeleteWebhooks,
			},
			{
				Name:          StepApplyConfiguration,
				Activity:      StepApplyConfiguration,
				Description:   "publish the merchant configuration document (boundary B8, synchronous on purpose)",
				Timeout:       10 * time.Second,
				Retry:         engine.RetryPolicy{MaxAttempts: 3, InitialInterval: time.Second, MaxInterval: 10 * time.Second, BackoffFactor: 2},
				Idempotent:    true,
				SideEffecting: true,
				Compensation:  CompRollbackConfig,
			},
			{
				Name:        StepSandboxValidation,
				Activity:    StepSandboxValidation,
				Description: "the certification matrix, sandbox subset",
				// Fifteen minutes per attempt against a sixty-second lease. The activity
				// heartbeats every fifteen seconds, which is what keeps the lease alive rather
				// than having the instance reclaimed mid-run.
				Timeout:       15 * time.Minute,
				Retry:         engine.RetryPolicy{MaxAttempts: 2, InitialInterval: 10 * time.Second, MaxInterval: 10 * time.Minute, BackoffFactor: 2},
				Idempotent:    true,
				SideEffecting: true,
				// No compensation: sandbox transactions are inert by construction.
			},
			{
				Name:          StepCertification,
				Activity:      StepCertification,
				Description:   "the full certification matrix; produces the signed report",
				Timeout:       30 * time.Minute,
				Retry:         engine.RetryPolicy{MaxAttempts: 2, InitialInterval: 10 * time.Second, MaxInterval: 10 * time.Minute, BackoffFactor: 2},
				Idempotent:    true,
				SideEffecting: true,
				// No compensation: the report is immutable evidence and is meant to survive. A
				// superseded report is marked superseded, never deleted.
			},
			{
				Name:        StepComplianceReview,
				Activity:    StepComplianceReview,
				ManualGate:  true,
				Signal:      SignalComplianceApproval,
				Description: "manual gate — a human compliance decision, audited",
				Timeout:     5 * 24 * time.Hour,
				// No compensation: a human decision is a record, not a mutation.
			},
			{
				Name:        StepActivate,
				Activity:    StepActivate,
				Description: "◆ PIVOT (irreversible) — PRODUCTION_READY → ACTIVE",
				Timeout:     5 * time.Second,
				Retry:       engine.RetryPolicy{MaxAttempts: 3, InitialInterval: 200 * time.Millisecond, MaxInterval: 2 * time.Second, BackoffFactor: 2},
				Idempotent:  true,
				Pivot:       true,
				PivotKind:   engine.PivotIrreversible,
				// Forward recovery, not rollback: suspension stops new payments while
				// deliberately continuing to permit refunds, voids and webhook processing.
				// "Undoing" activation by blocking refunds would trap merchant money.
				Compensation:     CompSuspendMerchant,
				CompensationKind: engine.CompensationForward,
			},
		},
	}
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-04, BR-16, BR-17, FR-24, FR-25, FR-26, FR-27, FR-28, FR-29.
//
// The onboarding saga: gateway provisioning fan-out, credential storage and webhook
// registration, initial configuration, sandbox validation, certification and the manual
// compliance gate
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
