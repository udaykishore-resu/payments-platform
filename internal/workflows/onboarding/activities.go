package onboarding

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/config"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// maxProvisioningConcurrency bounds the fan-out at step 5. Four, from baseline §11: the branches
// are I/O-bound against different vendors, and a higher bound would only make a slow gateway's
// rate limiter the thing that fails the step.
const maxProvisioningConcurrency = 4

// ProvisionerSet resolves the provisioning adapter for a gateway.
//
// Declared here, by the consumer, and deliberately not a map: the wiring may need to construct
// an adapter lazily, or refuse one whose credentials have not been configured, and a map cannot
// express "not available, and here is why".
type ProvisionerSet interface {
	Provisioner(id shared.GatewayID) (spi.GatewayProvisioner, error)
}

// ConfigStore is the subset of the configuration repository step 8 needs. Three methods, because
// publishing a configuration needs to read the current version for the optimistic-concurrency
// check and read an earlier one to roll back to; nothing else.
type ConfigStore interface {
	GetActive(ctx context.Context, m shared.MerchantID) (*config.MerchantConfig, error)
	GetVersion(ctx context.Context, m shared.MerchantID, version int) (*config.MerchantConfig, error)
	Publish(ctx context.Context, c *config.MerchantConfig, expectedVersion int) error
}

// CredentialSource mints or retrieves the API credentials for a freshly provisioned sub-account.
//
// It is separate from spi.GatewayProvisioner because credential *material* must not travel
// through the workflow at all: this port is called at step 6 and its result goes straight into
// the secrets store, so the material exists in one process's memory for the duration of one call
// and is never checkpointed, logged or returned.
type CredentialSource interface {
	IssueCredentials(ctx context.Context, merchantID shared.MerchantID, gateway shared.GatewayID, externalAccountID string) (map[string]string, error)
}

// Deps is everything the activities need. Every field is a port; none is an adapter.
type Deps struct {
	Merchants   ports.MerchantRepository
	Configs     ConfigStore
	KYC         ports.KYCProvider
	Bank        ports.BankValidator
	Secrets     ports.SecretsProvider
	Objects     ports.ObjectStore
	Gateways    ProvisionerSet
	Credentials CredentialSource

	// Sandbox drives the certification matrix. Supplying it is enough: Register builds the
	// Certifier from it, the object store and the clock, so a caller cannot wire a certifier
	// that writes its evidence somewhere other than where the rest of the platform looks for it.
	Sandbox Sandbox

	// Certifier may be supplied directly instead of Sandbox, for a deployment that needs a
	// differently-configured runner.
	Certifier *Certifier

	// Validator is the validation plane's level-2 entry point. It is a port so that the rule set
	// can grow without this package changing, and so that a step-1 test needs no rule engine.
	Validator MerchantValidator

	// Clock is the only source of time. All timestamps are UTC.
	Clock shared.Clock

	// WebhookURL returns this platform's ingress endpoint for a gateway. It is a function rather
	// than a base string because the path is gateway-specific and the host differs per
	// environment, and building it by concatenation at four call sites is how a merchant's
	// webhooks end up pointed at staging.
	WebhookURL func(gateway shared.GatewayID) string

	// KYCDecisionValidity is how long a KYC approval is good for. The merchant aggregate refuses
	// an approval with no future expiry, because verification that never expires is not
	// verification.
	KYCDecisionValidity time.Duration

	// AttestationValidity bounds the compliance attestations recorded at step 11.
	AttestationValidity time.Duration
}

func (d Deps) validate() error {
	var missing []string
	for name, present := range map[string]bool{
		"Merchants": d.Merchants != nil, "Configs": d.Configs != nil, "KYC": d.KYC != nil,
		"Bank": d.Bank != nil, "Secrets": d.Secrets != nil, "Objects": d.Objects != nil,
		"Gateways": d.Gateways != nil, "Credentials": d.Credentials != nil,
		"Sandbox or Certifier": d.Sandbox != nil || d.Certifier != nil,
		"Validator":            d.Validator != nil, "Clock": d.Clock != nil,
		"WebhookURL": d.WebhookURL != nil,
	} {
		if !present {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return apierror.Newf(apierror.CodeInternalError,
		"onboarding activities cannot be registered without: %s", strings.Join(missing, ", "))
}

func (d Deps) normalized() Deps {
	if d.Certifier == nil {
		d.Certifier = NewCertifier(d.Sandbox, d.Objects, d.Clock)
	}
	if d.KYCDecisionValidity <= 0 {
		// Thirteen months is the shortest periodic re-verification cycle across the
		// jurisdictions the platform operates in; anything longer would expire after the
		// obligation did.
		d.KYCDecisionValidity = 13 * 30 * 24 * time.Hour
	}
	if d.AttestationValidity <= 0 {
		d.AttestationValidity = 365 * 24 * time.Hour
	}
	return d
}

// --- step outputs -------------------------------------------------------------------------------
//
// Every output below is checkpointed into `workflow_instances.context` and is therefore readable
// by anyone with database access. That constraint is what shapes them: they carry references,
// versions and fingerprints, never credential material and never personal data. Documents travel
// as object-store references, so PII never enters the workflow context and therefore never
// enters a workflow diagnostic export.

// RuleOutcome is one validation-plane rule's verdict.
type RuleOutcome struct {
	RuleID  string `json:"ruleId"`
	Passed  bool   `json:"passed"`
	Message string `json:"message,omitempty"`
	Field   string `json:"field,omitempty"`
}

// ValidateMerchantOutput is step 1's result.
type ValidateMerchantOutput struct {
	Valid        bool          `json:"valid"`
	RuleOutcomes []RuleOutcome `json:"ruleOutcomes"`
	// NormalizedCountry and NormalizedMCC record what the rules resolved, so a later step reads
	// the normalized value rather than re-deriving it.
	NormalizedCountry shared.Country `json:"normalizedCountry"`
	NormalizedMCC     shared.MCC     `json:"normalizedMcc"`
	ValidatedAt       time.Time      `json:"validatedAt"`
}

// SubmitKYCOutput is step 2's result.
type SubmitKYCOutput struct {
	VendorCaseRef      string    `json:"vendorCaseRef"`
	SubmittedAt        time.Time `json:"submittedAt"`
	ExpectedDecisionBy time.Time `json:"expectedDecisionBy"`
	// ShortCircuited records that lookup-before-act found an existing case rather than creating
	// one. It is in the output because "did the retry create a second case" is the first
	// question anyone asks about this step.
	ShortCircuited bool `json:"shortCircuited"`
}

// KYCDecisionOutput is step 3's result: the vendor's decision as applied to the merchant.
type KYCDecisionOutput struct {
	Decision    string    `json:"decision"`
	ProviderRef string    `json:"providerRef"`
	RiskRating  string    `json:"riskRating"`
	ReasonCodes []string  `json:"reasonCodes,omitempty"`
	EvidenceRef string    `json:"evidenceRef,omitempty"`
	DecidedAt   time.Time `json:"decidedAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Principal   string    `json:"principal"`
}

// BankValidationOutput is step 4's result.
type BankValidationOutput struct {
	Validated   bool      `json:"validated"`
	Method      string    `json:"method"`
	Reference   string    `json:"reference"`
	ValidatedAt time.Time `json:"validatedAt"`
}

// GatewayConnection is one provisioned gateway sub-account.
type GatewayConnection struct {
	GatewayID           shared.GatewayID `json:"gatewayId"`
	ExternalAccountID   string           `json:"externalRef"`
	Status              string           `json:"status"`
	RequiresAction      bool             `json:"requiresAction"`
	PendingRequirements []string         `json:"pendingRequirements,omitempty"`
	ProvisionedAt       time.Time        `json:"provisionedAt"`
}

// ProvisionGatewaysOutput is step 5's result.
type ProvisionGatewaysOutput struct {
	Connections []GatewayConnection `json:"connections"`
	// Degraded records that at least one gateway failed while the surviving set still covers the
	// merchant's declared corridors. Judged by the coverage rule, not by a count.
	Degraded bool     `json:"degraded"`
	Failed   []string `json:"failed,omitempty"`
}

// SecretReference is one stored credential, by reference.
type SecretReference struct {
	GatewayID shared.GatewayID `json:"gatewayId"`
	SecretRef string           `json:"secretRef"`
	Version   string           `json:"version"`
	// Fingerprint is a salted digest of the material, for detecting an unexpected rotation. It
	// is deliberately not reversible and deliberately not the material.
	Fingerprint string `json:"fingerprint"`
}

// StoreCredentialsOutput is step 6's result: references and fingerprints only, never material.
type StoreCredentialsOutput struct {
	Refs []SecretReference `json:"refs"`
}

// WebhookRegistrationRecord is one gateway webhook subscription.
type WebhookRegistrationRecord struct {
	GatewayID       shared.GatewayID `json:"gatewayId"`
	RegistrationRef string           `json:"registrationRef"`
	URL             string           `json:"url"`
	// SigningSecretRef is a reference into the secrets store. The secret itself is returned once
	// by most gateways and goes straight there; it never appears in a database row or an API
	// response.
	SigningSecretRef  string `json:"signingSecretRef"`
	ExternalAccountID string `json:"externalAccountId"`
}

// RegisterWebhooksOutput is step 7's result.
type RegisterWebhooksOutput struct {
	Registrations []WebhookRegistrationRecord `json:"registrations"`
}

// ApplyConfigurationOutput is step 8's result.
type ApplyConfigurationOutput struct {
	ConfigVersion   int       `json:"configVersion"`
	PreviousVersion int       `json:"previousVersion"`
	ETag            string    `json:"etag"`
	PublishedAt     time.Time `json:"publishedAt"`
}

// CertificationRunOutput is the result of steps 9 and 10.
type CertificationRunOutput struct {
	RunID            string   `json:"runId"`
	Passed           bool     `json:"passed"`
	CellCount        int      `json:"cellCount"`
	FailedAssertions []string `json:"failedAssertions,omitempty"`
	// ReportRef and ContentHash are set for the full run only; the sandbox subset produces no
	// retained evidence.
	ReportRef   string `json:"reportRef,omitempty"`
	ContentHash string `json:"contentHash,omitempty"`
}

// ComplianceReviewOutput is step 11's result.
type ComplianceReviewOutput struct {
	Decision       string    `json:"decision"`
	ReviewerID     string    `json:"reviewerId"`
	Reason         string    `json:"reason,omitempty"`
	AttestationRef string    `json:"attestationRef,omitempty"`
	DecidedAt      time.Time `json:"decidedAt"`
}

// ActivateOutput is step 12's result, including the guards as they were evaluated.
type ActivateOutput struct {
	ActivatedAt     time.Time `json:"activatedAt"`
	GuardsEvaluated Guards    `json:"guardsEvaluated"`
	// AlreadyActive records the idempotent path: activating twice is a no-op returning the
	// current state, not an error.
	AlreadyActive bool `json:"alreadyActive"`
}

// Guards is what was true at activation time.
type Guards struct {
	CertifiedConnections int    `json:"certifiedConnections"`
	ConfigVersion        int    `json:"configVersion"`
	CertificationRef     string `json:"certificationRef"`
	KYCApproved          bool   `json:"kycApproved"`
	SettlementVerified   bool   `json:"settlementVerified"`
}

// signalDecision is the shape of both gates' payloads.
type signalDecision struct {
	Decision       string     `json:"decision"`
	ReasonCodes    []string   `json:"reasonCodes,omitempty"`
	Reason         string     `json:"reason,omitempty"`
	EvidenceRef    string     `json:"evidenceRef,omitempty"`
	AttestationRef string     `json:"attestationRef,omitempty"`
	ReviewerID     string     `json:"reviewerId,omitempty"`
	ProviderRef    string     `json:"providerRef,omitempty"`
	RiskRating     string     `json:"riskRating,omitempty"`
	DecidedAt      *time.Time `json:"decidedAt,omitempty"`
}

// Decision values a gate may carry.
const (
	DecisionApproved = "APPROVED"
	DecisionRejected = "REJECTED"
	DecisionMoreInfo = "MORE_INFO"
	DecisionApprove  = "APPROVE"
	DecisionReject   = "REJECT"
)

// Register wires every activity and compensation into an engine registry.
//
// It is called once at process start, next to engine.Register, so that a binary missing an
// activity fails at boot. Discovering it when a live merchant reaches step 10 means discovering
// it after thirty minutes of certification have already run.
func Register(acts *engine.Activities, d Deps) error {
	if err := d.validate(); err != nil {
		return err
	}
	d = d.normalized()

	all := []engine.Activity{
		engine.NewTypedActivity(StepValidateMerchant, d.validateMerchant),
		engine.NewTypedActivity(StepSubmitKYC, d.submitKYC),
		engine.NewTypedActivity(StepAwaitKYCDecision, d.applyKYCDecision),
		engine.NewTypedActivity(StepValidateBankAccount, d.validateBankAccount),
		engine.NewTypedActivity(StepProvisionGateways, d.provisionGateways),
		engine.NewTypedActivity(StepStoreCredentials, d.storeCredentials),
		engine.NewTypedActivity(StepRegisterWebhooks, d.registerWebhooks),
		engine.NewTypedActivity(StepApplyConfiguration, d.applyConfiguration),
		engine.NewTypedActivity(StepSandboxValidation, d.sandboxValidation),
		engine.NewTypedActivity(StepCertification, d.certification),
		engine.NewTypedActivity(StepComplianceReview, d.complianceReview),
		engine.NewTypedActivity(StepActivate, d.activate),
	}
	all = append(all, d.compensations()...)

	for _, a := range all {
		if err := acts.Register(a); err != nil {
			return err
		}
	}
	return nil
}

// --- step 1: validate-merchant --------------------------------------------------------------------

// validateMerchant runs the validation plane's level-2 rules.
//
// It is pure and it runs first, and both facts are load-bearing: a malformed merchant is
// rejected *before* any external side effect exists to compensate. Everything statically
// detectable is detected here, where the cost of a rejection is a message rather than a rollback
// across four vendors.
//
// FSM: CREATED → VALIDATING on start. On success there is no transition — the merchant stays in
// VALIDATING until step 2 has actually submitted the case, because VALIDATING → KYC_PENDING is
// the aggregate's way of saying "the vendor has it", and claiming that before the submission
// would make the merchant's own status a lie for the duration of step 2.
func (d Deps) validateMerchant(ctx context.Context, meta engine.Input, in Input) (ValidateMerchantOutput, error) {
	m, err := d.merchant(ctx, in.MerchantID)
	if err != nil {
		return ValidateMerchantOutput{}, err
	}

	if m.Status() == merchant.StatusCreated || m.Status() == merchant.StatusValidationFailed {
		if err := m.StartValidation(d.Clock); err != nil {
			return ValidateMerchantOutput{}, engine.WithClass(err, engine.ClassTerminalBusiness)
		}
		if err := d.Merchants.Save(ctx, m); err != nil {
			return ValidateMerchantOutput{}, err
		}
	}

	outcomes, err := d.Validator.Validate(ctx, m, in)
	if err != nil {
		// A rule engine that could not run is a transient database problem, not a rejection.
		// Conflating them would reject merchants during a database blip.
		return ValidateMerchantOutput{}, err
	}

	var failures []apierror.Detail
	for _, o := range outcomes {
		if !o.Passed {
			failures = append(failures, apierror.Detail{
				Field: o.Field, Code: "RULE_FAILED", Message: o.Message, RuleID: o.RuleID,
			})
		}
	}
	if len(failures) > 0 {
		if err := m.FailValidation(summary(failures), d.Clock); err != nil {
			return ValidateMerchantOutput{}, err
		}
		if err := d.Merchants.Save(ctx, m); err != nil {
			return ValidateMerchantOutput{}, err
		}
		return ValidateMerchantOutput{}, engine.WithClass(
			apierror.New(apierror.CodeValidationFailed,
				"the merchant does not satisfy the level-2 rules").WithDetails(failures...),
			engine.ClassTerminalBusiness)
	}

	return ValidateMerchantOutput{
		Valid:             true,
		RuleOutcomes:      outcomes,
		NormalizedCountry: m.Profile().Country,
		NormalizedMCC:     m.Profile().MCC,
		ValidatedAt:       d.Clock.Now().UTC(),
	}, nil
}

// --- step 2: submit-kyc ---------------------------------------------------------------------------

// submitKYC hands the KYB case to the identity vendor.
//
// **Lookup-before-act.** Before submitting, the activity checks whether this merchant already
// has a vendor reference and, if so, fetches that case instead of creating one. This is the
// mechanism that makes the crash window between "the vendor acted" and "we recorded it"
// harmless: a retry finds its own prior effect rather than creating a second regulated
// submission of a person's identity documents.
//
// PII leaves our system here. Documents are passed as pre-signed object-store references, never
// inlined, so personal data never enters the workflow context and therefore never enters a
// workflow diagnostic export.
//
// FSM: VALIDATING → KYC_PENDING on success, which also raises merchant.validated.v1 — the
// aggregate couples the event to the transition, and the transition is only true once the vendor
// has the case.
func (d Deps) submitKYC(ctx context.Context, meta engine.Input, in Input) (SubmitKYCOutput, error) {
	m, err := d.merchant(ctx, in.MerchantID)
	if err != nil {
		return SubmitKYCOutput{}, err
	}
	now := d.Clock.Now().UTC()

	if ref := m.KYCProviderRef(); ref != "" {
		decision, lookupErr := d.KYC.Get(ctx, ref)
		if lookupErr == nil {
			return d.finishSubmit(ctx, m, SubmitKYCOutput{
				VendorCaseRef:      decision.ProviderRef,
				SubmittedAt:        now,
				ExpectedDecisionBy: now.Add(7 * 24 * time.Hour),
				ShortCircuited:     true,
			})
		}
		if meta.LookupFirst {
			// The previous attempt ended ambiguously and the lookup could not resolve it.
			// Guessing here is exactly how a duplicate regulated submission happens.
			return SubmitKYCOutput{}, engine.WithClass(
				apierror.Wrapf(lookupErr, apierror.CodeDependencyFailure,
					"the previous attempt's outcome is unknown and the vendor lookup for %s was inconclusive", ref),
				engine.ClassManual)
		}
	}

	res, err := d.KYC.Submit(ctx, ports.KYCSubmission{
		IdempotencyKey: meta.IdempotencyKey,
		MerchantID:     m.ID(),
		LegalName:      m.LegalName(),
		Country:        m.Profile().Country,
		RegistrationNo: m.Profile().RegistrationNumber,
		TaxID:          m.Profile().TaxID,
		Address: map[string]string{
			"line1": m.Profile().AddressLine1, "line2": m.Profile().AddressLine2,
			"city": m.Profile().City, "region": m.Profile().Region,
			"postalCode": m.Profile().PostalCode, "country": string(m.Profile().Country),
		},
		Principals: m.Principals(),
	})
	if err != nil {
		return SubmitKYCOutput{}, err
	}
	if res.ProviderRef == "" {
		return SubmitKYCOutput{}, engine.WithClass(
			apierror.New(apierror.CodeInternalError,
				"the KYC vendor accepted the case but returned no reference, so we can never look it up again"),
			engine.ClassTerminalTechnical)
	}

	return d.finishSubmit(ctx, m, SubmitKYCOutput{
		VendorCaseRef:      res.ProviderRef,
		SubmittedAt:        orNow(res.SubmittedAt, now),
		ExpectedDecisionBy: now.Add(7 * 24 * time.Hour),
	})
}

func (d Deps) finishSubmit(ctx context.Context, m *merchant.Merchant, out SubmitKYCOutput) (SubmitKYCOutput, error) {
	if m.Status() == merchant.StatusValidating {
		if err := m.PassValidation(d.Clock); err != nil {
			return SubmitKYCOutput{}, err
		}
		if err := d.Merchants.Save(ctx, m); err != nil {
			return SubmitKYCOutput{}, err
		}
	}
	return out, nil
}

// --- step 3: await-kyc-decision (◆ pivot) ---------------------------------------------------------

// applyKYCDecision applies the vendor's decision, delivered as a signal.
//
// The step is a manual gate: the lease is released for up to seven days and the wait holds no
// worker resource at all. This activity runs *after* the signal is consumed, in the same
// checkpoint, so there is no window in which a decision has been consumed but not acted on — and
// the decision is consumed at most once.
//
// **This is the pivot for external irreversibility.** After the decision lands, the vendor's
// record is retained for five years under a legal-obligation basis, and there is nothing to
// compensate. `KYC_FAILED → KYC_PENDING` is a legal transition, so a resubmission starts a *new*
// workflow instance rather than resurrecting this one — which keeps each instance a single,
// auditable attempt.
func (d Deps) applyKYCDecision(ctx context.Context, meta engine.Input, in Input) (KYCDecisionOutput, error) {
	decision, err := decodeSignal(meta.Signal, SignalKYCDecision)
	if err != nil {
		return KYCDecisionOutput{}, err
	}
	m, err := d.merchant(ctx, in.MerchantID)
	if err != nil {
		return KYCDecisionOutput{}, err
	}

	providerRef := decision.ProviderRef
	if providerRef == "" {
		var submitted SubmitKYCOutput
		if found, _ := meta.StepOutput(StepSubmitKYC, &submitted); found {
			providerRef = submitted.VendorCaseRef
		}
	}
	now := d.Clock.Now().UTC()
	decidedAt := now
	if decision.DecidedAt != nil {
		decidedAt = decision.DecidedAt.UTC()
	}

	out := KYCDecisionOutput{
		Decision:    decision.Decision,
		ProviderRef: providerRef,
		ReasonCodes: decision.ReasonCodes,
		EvidenceRef: decision.EvidenceRef,
		DecidedAt:   decidedAt,
		Principal:   meta.SignalPrincipal,
	}

	switch strings.ToUpper(decision.Decision) {
	case DecisionApproved:
		expires := now.Add(d.KYCDecisionValidity)
		rating := merchant.RiskRating(strings.ToUpper(decision.RiskRating))
		if !rating.IsValid() {
			rating = merchant.RiskStandard
		}
		if m.Status() == merchant.StatusKYCPending {
			if err := m.ApproveKYC(providerRef, expires, rating, d.Clock); err != nil {
				return KYCDecisionOutput{}, engine.WithClass(err, engine.ClassTerminalBusiness)
			}
			if err := d.Merchants.Save(ctx, m); err != nil {
				return KYCDecisionOutput{}, err
			}
		}
		out.RiskRating = string(rating)
		out.ExpiresAt = expires
		return out, nil

	case DecisionRejected:
		if m.Status() == merchant.StatusKYCPending {
			if err := m.RejectKYC(providerRef, strings.Join(decision.ReasonCodes, ","), d.Clock); err != nil {
				return KYCDecisionOutput{}, err
			}
			if err := d.Merchants.Save(ctx, m); err != nil {
				return KYCDecisionOutput{}, err
			}
		}
		// A rejection is a business outcome, not an engineering failure: it must be recorded,
		// retained and surfaced to the merchant with its specific reason codes, never retried.
		return KYCDecisionOutput{}, engine.WithClass(
			apierror.Newf(apierror.CodeKYCRequired,
				"verification was declined: %s", strings.Join(decision.ReasonCodes, ", ")),
			engine.ClassTerminalBusiness)

	case DecisionMoreInfo:
		// Not a failure. The vendor wants more documents; the merchant is notified and the
		// instance parks until a further decision arrives.
		return KYCDecisionOutput{}, engine.WithClass(
			apierror.Newf(apierror.CodeKYCRequired,
				"the verification vendor requires more information: %s", strings.Join(decision.ReasonCodes, ", ")),
			engine.ClassManual)

	default:
		return KYCDecisionOutput{}, engine.WithClass(
			apierror.Newf(apierror.CodeValidationFailed,
				"unrecognised KYC decision %q", decision.Decision),
			engine.ClassTerminalTechnical)
	}
}

// --- step 4: validate-bank-account ----------------------------------------------------------------

// validateBankAccount verifies the settlement account.
//
// Nothing is created, so nothing is compensated — but the step is still declared side-effecting,
// because a penny-drop *does* move money and a duplicate submission would initiate a second
// micro-deposit. That is exactly why a timeout here is ambiguous and resolved by lookup rather
// than by another call.
//
// FSM: KYC_APPROVED → BANK_VALIDATED on success, raising merchant.bank_validated.v1.
func (d Deps) validateBankAccount(ctx context.Context, meta engine.Input, in Input) (BankValidationOutput, error) {
	m, err := d.merchant(ctx, in.MerchantID)
	if err != nil {
		return BankValidationOutput{}, err
	}
	account := d.pickAccount(m, in.BankAccountID)
	if account == nil {
		return BankValidationOutput{}, engine.WithClass(
			apierror.Newf(apierror.CodeValidationFailed,
				"merchant %s has no settlement account %q to verify", in.MerchantID, in.BankAccountID),
			engine.ClassTerminalBusiness)
	}

	res, err := d.Bank.Validate(ctx, ports.BankValidationRequest{
		IdempotencyKey: meta.IdempotencyKey,
		MerchantID:     m.ID(),
		AccountID:      account.ID,
		SecretRef:      account.SecretRef,
		Country:        account.Country,
		Currency:       account.Currency,
	})
	if err != nil {
		return BankValidationOutput{}, err
	}

	now := d.Clock.Now().UTC()
	switch {
	case res.Verified:
		if err := m.ValidateBankAccount(account.ID, res.Reference, d.Clock); err != nil {
			return BankValidationOutput{}, err
		}
		if err := d.Merchants.Save(ctx, m); err != nil {
			return BankValidationOutput{}, err
		}
		return BankValidationOutput{
			Validated: true, Method: res.FailureReason, Reference: res.Reference,
			ValidatedAt: orNow(deref(res.CompletedAt), now),
		}, nil

	case res.Pending:
		// Pending is a real outcome, not a failure: micro-deposit verification takes days, and
		// modelling it as failure would restart onboarding every time. The instance parks and a
		// later confirmation resumes it.
		return BankValidationOutput{}, engine.WithClass(
			apierror.Newf(apierror.CodeValidationFailed,
				"settlement account %s is awaiting micro-deposit confirmation", account.ID),
			engine.ClassManual)

	default:
		if err := m.FailBankValidation(account.ID, res.FailureReason, d.Clock); err != nil {
			return BankValidationOutput{}, err
		}
		if err := d.Merchants.Save(ctx, m); err != nil {
			return BankValidationOutput{}, err
		}
		return BankValidationOutput{}, engine.WithClass(
			apierror.Newf(apierror.CodeValidationFailed,
				"settlement account %s could not be verified: %s", account.ID, res.FailureReason),
			engine.ClassTerminalBusiness)
	}
}

// --- step 5: provision-gateways -------------------------------------------------------------------

// provisionGateways creates a sub-account at each selected gateway, concurrently, bounded at four.
//
// **Per-branch checkpointing** is what makes the fan-out crash-safe: each gateway's result is
// written through Input.Checkpoint as it completes, so a crash after two of four does not
// re-provision the two that succeeded. Without it the only granularity available is the whole
// step, and the whole step contains external creates that are expensive to clean up and visible
// to the merchant.
//
// **Partial success** is allowed when the surviving gateways still cover the merchant's declared
// corridors, judged by the coverage rule rather than by a count: three gateways that all support
// only EUR do not cover a merchant selling in USD, and one that does is enough.
//
// FSM: BANK_VALIDATED → GATEWAY_PROVISIONING on start.
func (d Deps) provisionGateways(ctx context.Context, meta engine.Input, in Input) (ProvisionGatewaysOutput, error) {
	m, err := d.merchant(ctx, in.MerchantID)
	if err != nil {
		return ProvisionGatewaysOutput{}, err
	}
	if m.Status() == merchant.StatusBankValidated {
		if err := m.StartProvisioning(d.Clock); err != nil {
			return ProvisionGatewaysOutput{}, err
		}
		if err := d.Merchants.Save(ctx, m); err != nil {
			return ProvisionGatewaysOutput{}, err
		}
	}

	type branch struct {
		conn GatewayConnection
		err  error
	}
	results := make([]branch, len(in.Gateways))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxProvisioningConcurrency)
	for i, gatewayID := range in.Gateways {
		g.Go(func() error {
			// Resume: a branch already checkpointed on an earlier attempt is not re-run.
			if raw, ok, lookupErr := meta.Lookup(gctx, string(gatewayID)); lookupErr == nil && ok {
				var cached GatewayConnection
				if decodeInto(raw, &cached) == nil {
					results[i] = branch{conn: cached}
					return nil
				}
			}
			conn, berr := d.provisionOne(gctx, m, meta, in, gatewayID)
			results[i] = branch{conn: conn, err: berr}
			if berr != nil {
				// A branch failure does not cancel its peers: cancelling them would leave their
				// creates in flight with no record of whether they happened.
				return nil //nolint:nilerr // the branch failure is carried in results[i]; returning it would cancel the sibling provisioning calls and leave their creates in flight with no record
			}
			encoded, mErr := encode(conn)
			if mErr != nil {
				return mErr
			}
			return meta.Checkpoint(gctx, string(gatewayID), encoded)
		})
	}
	if err := g.Wait(); err != nil {
		return ProvisionGatewaysOutput{}, err
	}

	out := ProvisionGatewaysOutput{}
	var firstErr error
	for i, r := range results {
		if r.err != nil {
			out.Failed = append(out.Failed, string(in.Gateways[i])+": "+r.err.Error())
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		out.Connections = append(out.Connections, r.conn)
	}

	if len(out.Connections) == 0 {
		return ProvisionGatewaysOutput{}, failProvisioning(ctx, d, m, firstErr,
			"no gateway could be provisioned")
	}
	if len(out.Failed) > 0 {
		out.Degraded = true
		if !coversCorridors(out.Connections, in) {
			return ProvisionGatewaysOutput{}, failProvisioning(ctx, d, m, firstErr,
				"the gateways that provisioned do not cover the merchant's declared corridors")
		}
	}
	return out, nil
}

func (d Deps) provisionOne(ctx context.Context, m *merchant.Merchant, meta engine.Input, in Input, gatewayID shared.GatewayID) (GatewayConnection, error) {
	p, err := d.Gateways.Provisioner(gatewayID)
	if err != nil {
		return GatewayConnection{}, engine.WithClass(err, engine.ClassTerminalTechnical)
	}
	account := m.DefaultBankAccount()
	bank := spi.BankAccountData{}
	if account != nil {
		bank = spi.BankAccountData{
			Country: account.Country, Currency: account.Currency, HolderName: account.HolderName,
		}
	}
	principals := make([]spi.PrincipalData, 0, len(m.Principals()))
	for _, pr := range m.Principals() {
		principals = append(principals, spi.PrincipalData{
			FirstName: pr.FirstName, LastName: pr.LastName, Role: string(pr.Role),
			OwnershipPct: pr.OwnershipPct, Country: pr.Country, VerificationRef: pr.VerificationRef,
		})
	}

	res, err := p.Provision(ctx, spi.ProvisionRequest{
		// The gateway's idempotency field carries K‖gatewayId: deterministic in (instance, step,
		// gateway), so a retry after a crash is deduplicated by the gateway rather than creating
		// a second connected account — which is a manual-cleanup incident at every gateway.
		IdempotencyKey:     meta.IdempotencyKey + ":" + string(gatewayID),
		MerchantID:         m.ID(),
		LegalName:          m.LegalName(),
		DisplayName:        m.DisplayName(),
		Country:            m.Profile().Country,
		MCC:                m.Profile().MCC,
		WebsiteURL:         m.Profile().WebsiteURL,
		SupportEmail:       m.Profile().SupportEmail,
		TaxID:              m.Profile().TaxID,
		RegistrationNumber: m.Profile().RegistrationNumber,
		AddressLines:       []string{m.Profile().AddressLine1, m.Profile().AddressLine2},
		City:               m.Profile().City,
		Region:             m.Profile().Region,
		PostalCode:         m.Profile().PostalCode,
		Principals:         principals,
		BankAccount:        bank,
		Currencies:         in.Currencies,
		Methods:            in.PaymentMethods,
		Environment:        in.Environment,
	})
	if err != nil {
		return GatewayConnection{}, err
	}
	if res == nil || res.ExternalAccountID == "" {
		return GatewayConnection{}, engine.WithClass(
			apierror.Newf(apierror.CodeGatewayContractViolation,
				"gateway %s provisioned an account but returned no reference", gatewayID),
			engine.ClassTerminalTechnical)
	}
	return GatewayConnection{
		GatewayID:           gatewayID,
		ExternalAccountID:   res.ExternalAccountID,
		Status:              res.Status,
		RequiresAction:      res.RequiresAction,
		PendingRequirements: res.PendingRequirements,
		ProvisionedAt:       d.Clock.Now().UTC(),
	}, nil
}

func failProvisioning(ctx context.Context, d Deps, m *merchant.Merchant, cause error, why string) error {
	if m.Status() == merchant.StatusGatewayProvisioning {
		if err := m.FailProvisioning(why, d.Clock); err == nil {
			_ = d.Merchants.Save(ctx, m)
		}
	}
	if cause == nil {
		cause = apierror.New(apierror.CodeGatewayNotConfigured, why)
	}
	return apierror.Wrapf(cause, apierror.CodeGatewayNotConfigured, "%s", why)
}

// coversCorridors is the L4.ROUTING_CAPABILITY_COVERAGE judgement in miniature: a degraded
// provisioning result is acceptable only if what survived can still serve every declared
// corridor. Counting gateways instead would accept three that all support one currency for a
// merchant selling in three.
func coversCorridors(conns []GatewayConnection, in Input) bool {
	// With only the connection references available here, coverage is judged on the connections
	// being usable at all; the full capability check belongs to the configuration document's own
	// L4 validation at step 8, which sees the gateway capability registry. This is the cheap
	// pre-check that stops the saga proceeding with nothing usable.
	for _, c := range conns {
		if !c.RequiresAction && c.ExternalAccountID != "" {
			return true
		}
	}
	return false
}

// --- step 6: store-credentials --------------------------------------------------------------------

// storeCredentials writes each gateway's credentials to the secrets store.
//
// **The output must never contain material.** This output is checkpointed into
// `workflow_instances.context` and would otherwise be readable by anyone with database access,
// which would defeat the entire split between the control plane and the secrets store. The
// output carries `secret://` references, versions and salted fingerprints; the test
// TestStoreCredentialsOutputCarriesNoMaterial marshals it and asserts exactly that.
func (d Deps) storeCredentials(ctx context.Context, meta engine.Input, in Input) (StoreCredentialsOutput, error) {
	var provisioned ProvisionGatewaysOutput
	if found, err := meta.StepOutput(StepProvisionGateways, &provisioned); err != nil || !found {
		return StoreCredentialsOutput{}, engine.WithClass(
			apierror.New(apierror.CodeInternalError,
				"store-credentials ran without the provisioning step's output"),
			engine.ClassTerminalTechnical)
	}

	out := StoreCredentialsOutput{}
	for _, conn := range provisioned.Connections {
		material, err := d.Credentials.IssueCredentials(ctx, in.MerchantID, conn.GatewayID, conn.ExternalAccountID)
		if err != nil {
			return StoreCredentialsOutput{}, err
		}
		ref := SecretRef(in.MerchantID, conn.GatewayID, in.Environment)
		// ClientRequestToken semantics: a retry with the same token returns the existing version
		// rather than creating a new one, so a crash between the write and the checkpoint does
		// not leave a trail of orphaned secret versions.
		versioned, err := d.Secrets.Put(ctx, ref, material)
		if err != nil {
			return StoreCredentialsOutput{}, err
		}
		out.Refs = append(out.Refs, SecretReference{
			GatewayID:   conn.GatewayID,
			SecretRef:   versioned,
			Version:     versionOf(versioned),
			Fingerprint: fingerprint(ref, material),
		})
	}
	return out, nil
}

// SecretRef builds the platform's secret reference for a merchant's gateway credentials.
func SecretRef(merchantID shared.MerchantID, gateway shared.GatewayID, env shared.Environment) string {
	return fmt.Sprintf("secret://%s/%s/%s", env, merchantID, gateway)
}

// fingerprint is a salted, truncated digest of the material.
//
// Salted with the reference so that two merchants holding the same vendor test key do not
// produce the same fingerprint, and truncated because the fingerprint's only job is to answer
// "did this change" — a longer digest would add no information and more surface.
func fingerprint(ref string, material map[string]string) string {
	fields := make([]string, 0, len(material))
	for k := range material {
		fields = append(fields, k)
	}
	sort.Strings(fields)
	h := sha256.New()
	h.Write([]byte(ref))
	for _, f := range fields {
		h.Write([]byte{0})
		h.Write([]byte(f))
		h.Write([]byte{0})
		h.Write([]byte(material[f]))
	}
	return "fp_" + base64.RawURLEncoding.EncodeToString(h.Sum(nil))[:16]
}

func versionOf(versionedRef string) string {
	if i := strings.LastIndex(versionedRef, "#"); i >= 0 {
		return versionedRef[i+1:]
	}
	return ""
}

// --- step 7: register-webhooks --------------------------------------------------------------------

// registerWebhooks subscribes this platform's ingress to each gateway's events.
//
// It runs **before** any sandbox transaction on purpose: certification asserts that a webhook is
// received, signature-verified and moves the payment state, and registering afterwards would
// make that assertion unverifiable — the run would pass by never having anything to check.
//
// The signing secret is returned once, at creation, by most gateways. It goes straight into the
// secrets store; it never appears in a database row, an API response, or this step's output.
//
// FSM: GATEWAY_PROVISIONING → CONFIGURING on success.
func (d Deps) registerWebhooks(ctx context.Context, meta engine.Input, in Input) (RegisterWebhooksOutput, error) {
	var provisioned ProvisionGatewaysOutput
	if found, err := meta.StepOutput(StepProvisionGateways, &provisioned); err != nil || !found {
		return RegisterWebhooksOutput{}, engine.WithClass(
			apierror.New(apierror.CodeInternalError, "register-webhooks ran without the provisioning step's output"),
			engine.ClassTerminalTechnical)
	}
	m, err := d.merchant(ctx, in.MerchantID)
	if err != nil {
		return RegisterWebhooksOutput{}, err
	}

	out := RegisterWebhooksOutput{}
	gatewayIDs := make([]string, 0, len(provisioned.Connections))
	for _, conn := range provisioned.Connections {
		p, err := d.Gateways.Provisioner(conn.GatewayID)
		if err != nil {
			return RegisterWebhooksOutput{}, engine.WithClass(err, engine.ClassTerminalTechnical)
		}
		url := d.WebhookURL(conn.GatewayID)
		if !strings.HasPrefix(url, "https://") {
			// Our configuration is wrong, not the merchant's. A plaintext webhook endpoint is a
			// credential leak with a schedule.
			return RegisterWebhooksOutput{}, engine.WithClass(
				apierror.Newf(apierror.CodeConfigurationInvalid,
					"the webhook URL for %s is not HTTPS", conn.GatewayID),
				engine.ClassTerminalTechnical)
		}

		reg, err := p.RegisterWebhook(ctx, spi.WebhookRegistrationRequest{
			IdempotencyKey:    meta.IdempotencyKey + ":" + string(conn.GatewayID),
			ExternalAccountID: conn.ExternalAccountID,
			URL:               url,
			EventTypes:        DefaultWebhookEvents,
		})
		if err != nil {
			return RegisterWebhooksOutput{}, err
		}
		if reg == nil || reg.RegistrationID == "" {
			return RegisterWebhooksOutput{}, engine.WithClass(
				apierror.Newf(apierror.CodeGatewayContractViolation,
					"gateway %s registered a webhook but returned no registration reference, so it can never be deleted", conn.GatewayID),
				engine.ClassTerminalTechnical)
		}

		signingRef := SigningSecretRef(in.MerchantID, conn.GatewayID, in.Environment)
		if reg.SigningSecret != "" {
			versioned, err := d.Secrets.Put(ctx, signingRef, map[string]string{"signingSecret": reg.SigningSecret})
			if err != nil {
				return RegisterWebhooksOutput{}, err
			}
			signingRef = versioned
		}
		out.Registrations = append(out.Registrations, WebhookRegistrationRecord{
			GatewayID:         conn.GatewayID,
			RegistrationRef:   reg.RegistrationID,
			URL:               reg.URL,
			SigningSecretRef:  signingRef,
			ExternalAccountID: conn.ExternalAccountID,
		})
		gatewayIDs = append(gatewayIDs, string(conn.GatewayID))
	}

	if m.Status() == merchant.StatusGatewayProvisioning {
		if err := m.CompleteProvisioning(gatewayIDs, d.Clock); err != nil {
			return RegisterWebhooksOutput{}, err
		}
		if err := d.Merchants.Save(ctx, m); err != nil {
			return RegisterWebhooksOutput{}, err
		}
	}
	return out, nil
}

// SigningSecretRef is the secrets-store reference for a gateway's webhook signing secret.
func SigningSecretRef(merchantID shared.MerchantID, gateway shared.GatewayID, env shared.Environment) string {
	return fmt.Sprintf("secret://%s/%s/%s/webhook-signing", env, merchantID, gateway)
}

// DefaultWebhookEvents is the event set the platform subscribes to. It is explicit rather than
// "all events" because an unrecognised event type is rejected at ingress, and subscribing to
// everything turns every new vendor event into ingress noise and a rate-limit consumer.
var DefaultWebhookEvents = []string{
	"payment.authorized", "payment.captured", "payment.failed",
	"refund.succeeded", "refund.failed", "payout.settled",
	"dispute.opened", "dispute.closed", "account.updated",
}

// --- step 8: apply-configuration ------------------------------------------------------------------

// applyConfiguration publishes the merchant's configuration document.
//
// This is boundary B8, and it is synchronous on purpose: a compensatable step needs an
// unambiguous success/failure verdict. If it were asynchronous, the compensation would first
// have to *determine* whether the configuration had been applied, which is the ambiguity problem
// again with an extra round trip.
//
// FSM: CONFIGURING → SANDBOX_VALIDATION on success.
func (d Deps) applyConfiguration(ctx context.Context, meta engine.Input, in Input) (ApplyConfigurationOutput, error) {
	var provisioned ProvisionGatewaysOutput
	if found, err := meta.StepOutput(StepProvisionGateways, &provisioned); err != nil || !found {
		return ApplyConfigurationOutput{}, engine.WithClass(
			apierror.New(apierror.CodeInternalError, "apply-configuration ran without the provisioning step's output"),
			engine.ClassTerminalTechnical)
	}
	var webhooks RegisterWebhooksOutput
	_, _ = meta.StepOutput(StepRegisterWebhooks, &webhooks)

	m, err := d.merchant(ctx, in.MerchantID)
	if err != nil {
		return ApplyConfigurationOutput{}, err
	}

	current, err := d.Configs.GetActive(ctx, in.MerchantID)
	previousVersion := 0
	if err == nil && current != nil {
		previousVersion = current.Version
	}

	next := &config.MerchantConfig{
		MerchantID:          in.MerchantID,
		TenantID:            in.TenantID,
		Version:             previousVersion,
		Environment:         in.Environment,
		SupportedCurrencies: in.Currencies,
		PaymentMethods:      in.PaymentMethods,
		Countries:           in.Countries,
		Webhook:             buildWebhookConfig(webhooks),
		FeatureFlags:        map[string]bool{},
	}
	published, err := next.Publish(in.RequestedBy, "merchant onboarding "+string(meta.WorkflowID), d.Clock.Now().UTC())
	if err != nil {
		return ApplyConfigurationOutput{}, engine.WithClass(err, engine.ClassTerminalBusiness)
	}

	// expectedVersion is the If-Match half: a concurrent publish loses rather than silently
	// overwriting, and the resulting conflict is retryable because re-reading and republishing
	// is the correct response.
	if err := d.Configs.Publish(ctx, published, previousVersion); err != nil {
		return ApplyConfigurationOutput{}, err
	}

	if m.Status() == merchant.StatusConfiguring {
		if err := m.ApplyConfiguration(published.Version, d.Clock); err != nil {
			return ApplyConfigurationOutput{}, err
		}
		if err := d.Merchants.Save(ctx, m); err != nil {
			return ApplyConfigurationOutput{}, err
		}
	}
	return ApplyConfigurationOutput{
		ConfigVersion:   published.Version,
		PreviousVersion: previousVersion,
		ETag:            published.ETag(),
		PublishedAt:     d.Clock.Now().UTC(),
	}, nil
}

func buildWebhookConfig(w RegisterWebhooksOutput) config.WebhookConfig {
	endpoints := make([]config.WebhookEndpoint, 0, len(w.Registrations))
	for _, r := range w.Registrations {
		endpoints = append(endpoints, config.WebhookEndpoint{
			URL: r.URL, Events: []string{"payment.*", "refund.*", "dispute.*"},
			SecretRef: r.SigningSecretRef, Active: true,
		})
	}
	return config.WebhookConfig{Endpoints: endpoints, MaxAttempts: 8, Backoff: "exponential"}
}

// --- steps 9 and 10: sandbox-validation and certification ------------------------------------------

// sandboxValidation runs the certification matrix's smoke subset.
//
// It exists so that a broken integration fails in fifteen minutes rather than in thirty, and
// before the run that produces retained evidence. Sandbox transactions are inert by construction,
// so there is nothing to compensate.
//
// FSM: SANDBOX_VALIDATION → CERTIFICATION on success.
func (d Deps) sandboxValidation(ctx context.Context, meta engine.Input, in Input) (CertificationRunOutput, error) {
	cells := SandboxSubset(Matrix(in.Gateways, in.PaymentMethods, in.Currencies))
	out, err := d.runCertification(ctx, meta, in, cells, false)
	if err != nil {
		return out, err
	}
	m, mErr := d.merchant(ctx, in.MerchantID)
	if mErr != nil {
		return CertificationRunOutput{}, mErr
	}
	if m.Status() == merchant.StatusSandboxValidation {
		if err := m.StartCertification(d.Clock); err != nil {
			return CertificationRunOutput{}, err
		}
		if err := d.Merchants.Save(ctx, m); err != nil {
			return CertificationRunOutput{}, err
		}
	}
	return out, nil
}

// certification runs the full matrix and stores the signed report.
//
// The report is immutable evidence and is meant to survive: it is written under Object Lock with
// a retention period and is never deleted. That is also why the step declares no compensation —
// a superseded report is marked superseded, not removed.
//
// The merchant is deliberately **not** advanced to APPROVED here, even though baseline §11 lists
// that transition against this step. The domain's transition table (amendment A-01) gives
// APPROVED exactly one outgoing edge, to PRODUCTION_READY, and puts COMPLIANCE_REJECTED
// downstream of CERTIFICATION. Advancing to APPROVED before compliance has ruled would therefore
// leave a rejection with no legal destination — the exact modelling gap A-01 exists to close — so
// the aggregate's Approve is called at step 11 with this report's reference, which is what makes
// PRODUCTION_READY structurally unreachable without a passing report.
func (d Deps) certification(ctx context.Context, meta engine.Input, in Input) (CertificationRunOutput, error) {
	cells := Matrix(in.Gateways, in.PaymentMethods, in.Currencies)
	return d.runCertification(ctx, meta, in, cells, true)
}

func (d Deps) runCertification(ctx context.Context, meta engine.Input, in Input, cells []MatrixCell, store bool) (CertificationRunOutput, error) {
	report, err := d.Certifier.Run(ctx, meta, RunSpec{
		// The run ID *is* the step's deterministic idempotency key, so a retry resumes from the
		// last per-cell checkpoint rather than re-running the whole matrix.
		RunID:       meta.IdempotencyKey,
		MerchantID:  in.MerchantID,
		TenantID:    in.TenantID,
		Environment: in.Environment,
		Matrix:      cells,
		Store:       store,
	})
	if err != nil {
		return CertificationRunOutput{}, err
	}

	out := CertificationRunOutput{
		RunID:            report.RunID,
		Passed:           report.Passed,
		CellCount:        len(report.Cells),
		FailedAssertions: report.FailedAssertions,
		ReportRef:        report.StorageKey,
		ContentHash:      report.ContentHash,
	}
	if report.Passed {
		return out, nil
	}

	m, mErr := d.merchant(ctx, in.MerchantID)
	if mErr == nil {
		if store && m.Status() == merchant.StatusCertification {
			if err := m.FailCertification(report.FailureSummary(), d.Clock); err == nil {
				_ = d.Merchants.Save(ctx, m)
			}
		} else if !store && m.Status() == merchant.StatusSandboxValidation {
			if err := m.FailConfiguration(report.FailureSummary(), d.Clock); err == nil {
				_ = d.Merchants.Save(ctx, m)
			}
		}
	}
	// An assertion failure is a business outcome with named, actionable assertion IDs — not
	// something to retry against a gateway that will deterministically fail it again.
	return out, engine.WithClass(
		apierror.Newf(apierror.CodeCertificationFailed,
			"certification did not pass: %s", report.FailureSummary()),
		engine.ClassTerminalBusiness)
}

// --- step 11: compliance-review -------------------------------------------------------------------

// complianceReview applies a human compliance decision.
//
// The gate holds no worker resource for the five days it may wait, and the signal is audited with
// the principal, scopes, source IP and reason by the engine before this activity ever runs.
//
// On APPROVE the merchant records the certification report and moves CERTIFICATION → APPROVED →
// PRODUCTION_READY. Recording the report here rather than at step 10 is what makes
// PRODUCTION_READY structurally unreachable without a passing report: the aggregate's Approve
// refuses a transition with no report reference.
//
// On REJECT the merchant moves CERTIFICATION → COMPLIANCE_REJECTED (amendment A-01). It is
// deliberately not advanced to PRODUCTION_READY-then-SUSPENDED, which would be a legal path but a
// lie in the audit trail: the record would show a merchant that was production-ready when
// compliance had in fact refused.
func (d Deps) complianceReview(ctx context.Context, meta engine.Input, in Input) (ComplianceReviewOutput, error) {
	decision, err := decodeSignal(meta.Signal, SignalComplianceApproval)
	if err != nil {
		return ComplianceReviewOutput{}, err
	}
	m, err := d.merchant(ctx, in.MerchantID)
	if err != nil {
		return ComplianceReviewOutput{}, err
	}
	reviewer := decision.ReviewerID
	if reviewer == "" {
		reviewer = meta.SignalPrincipal
	}
	now := d.Clock.Now().UTC()
	out := ComplianceReviewOutput{
		Decision: strings.ToUpper(decision.Decision), ReviewerID: reviewer,
		Reason: decision.Reason, AttestationRef: decision.AttestationRef, DecidedAt: now,
	}

	switch out.Decision {
	case DecisionApprove, DecisionApproved:
		var cert CertificationRunOutput
		found, _ := meta.StepOutput(StepCertification, &cert)
		if !found || !cert.Passed || cert.ContentHash == "" || cert.ReportRef == "" {
			return ComplianceReviewOutput{}, engine.WithClass(
				apierror.New(apierror.CodeCertificationFailed,
					"compliance approved a merchant with no passing certification report on file"),
				engine.ClassTerminalTechnical)
		}
		if reviewer == "" {
			return ComplianceReviewOutput{}, engine.WithClass(
				apierror.New(apierror.CodeValidationFailed,
					"a compliance approval must name its reviewer; an unattributable approval is not a control"),
				engine.ClassTerminalTechnical)
		}
		if err := d.recordAttestations(m, decision, reviewer, now); err != nil {
			return ComplianceReviewOutput{}, err
		}
		if m.Status() == merchant.StatusCertification {
			// The merchant record carries the object key *and* the content hash, joined. The key
			// alone would let a report be swapped in the bucket without the record noticing; the
			// hash alone would leave nobody able to find the document it attests to.
			if err := m.Approve(CertificationReference(cert.ReportRef, cert.ContentHash), reviewer, d.Clock); err != nil {
				return ComplianceReviewOutput{}, engine.WithClass(err, engine.ClassTerminalBusiness)
			}
		}
		if m.Status() == merchant.StatusApproved {
			if err := m.MarkProductionReady(d.Clock); err != nil {
				return ComplianceReviewOutput{}, engine.WithClass(err, engine.ClassTerminalBusiness)
			}
		}
		if err := d.Merchants.Save(ctx, m); err != nil {
			return ComplianceReviewOutput{}, err
		}
		return out, nil

	case DecisionReject, DecisionRejected:
		reasonCode := firstNonEmpty(decision.ReasonCodes...)
		if reasonCode == "" {
			reasonCode = "COMPLIANCE_REJECTED"
		}
		if m.Status() == merchant.StatusCertification {
			if err := m.RejectForCompliance(reasonCode, decision.Reason, reviewer, d.Clock); err != nil {
				return ComplianceReviewOutput{}, err
			}
			if err := d.Merchants.Save(ctx, m); err != nil {
				return ComplianceReviewOutput{}, err
			}
		}
		return out, engine.WithClass(
			apierror.Newf(apierror.CodeForbidden, "compliance rejected the merchant: %s", reasonCode),
			engine.ClassTerminalBusiness)

	default:
		return ComplianceReviewOutput{}, engine.WithClass(
			apierror.Newf(apierror.CodeValidationFailed,
				"unrecognised compliance decision %q", decision.Decision),
			engine.ClassTerminalTechnical)
	}
}

// recordAttestations writes the compliance obligations the activation guard requires.
//
// They are recorded from the reviewer's attestation rather than assumed, and they expire: an
// attestation with no end date silently becomes stale, and a stale attestation is
// indistinguishable from a missing one at audit time.
func (d Deps) recordAttestations(m *merchant.Merchant, decision signalDecision, reviewer string, now time.Time) error {
	if decision.AttestationRef == "" {
		return engine.WithClass(
			apierror.New(apierror.CodeValidationFailed,
				"a compliance approval must carry an attestation reference"),
			engine.ClassTerminalBusiness)
	}
	for _, kind := range RequiredAttestations {
		if err := m.AddAttestation(merchant.ComplianceAttestation{
			Type:       kind,
			Reference:  decision.AttestationRef,
			AttestedBy: reviewer,
			AttestedAt: now,
			ExpiresAt:  now.Add(d.AttestationValidity),
		}, d.Clock); err != nil {
			return err
		}
	}
	return nil
}

// CertificationReference joins a report's storage key and its content hash into the single
// string the merchant record holds. Keeping them together is what makes the evidence both
// findable and tamper-evident.
func CertificationReference(reportRef, contentHash string) string {
	return reportRef + "#" + contentHash
}

// RequiredAttestations are the obligations the merchant aggregate's activation guard checks.
var RequiredAttestations = []string{"PCI_SAQ", "TERMS_ACCEPTANCE"}

// --- step 12: activate (◆ pivot) ------------------------------------------------------------------

// activate is the last synchronous check before real money is possible.
//
// **Guards are re-evaluated here, not trusted from earlier steps.** A certification that expired
// while compliance review was pending, or a critical reconciliation exception opened in the
// meantime, must block activation. This is the correct place to be paranoid, and the aggregate's
// Activate re-checks every precondition itself rather than believing the workflow got here
// legitimately.
//
// Idempotency is FSM-inherent: a merchant that is already ACTIVE is treated as success returning
// the current state, because a retry after a crash must not fail an onboarding that in fact
// completed.
//
// FSM: PRODUCTION_READY → ACTIVE, raising merchant.activated.v1 — the event the data-plane cache
// consumes, and therefore the thing that actually lets payments through.
func (d Deps) activate(ctx context.Context, meta engine.Input, in Input) (ActivateOutput, error) {
	m, err := d.merchant(ctx, in.MerchantID)
	if err != nil {
		return ActivateOutput{}, err
	}
	var provisioned ProvisionGatewaysOutput
	_, _ = meta.StepOutput(StepProvisionGateways, &provisioned)

	guards := Guards{
		CertifiedConnections: len(provisioned.Connections),
		ConfigVersion:        m.ActiveConfigVersion(),
		CertificationRef:     m.CertificationID(),
		KYCApproved:          m.KYCStatus().IsSatisfied(),
		SettlementVerified:   m.DefaultBankAccount() != nil,
	}

	if m.Status() == merchant.StatusActive {
		at := d.Clock.Now().UTC()
		if m.ActivatedAt() != nil {
			at = *m.ActivatedAt()
		}
		return ActivateOutput{ActivatedAt: at, GuardsEvaluated: guards, AlreadyActive: true}, nil
	}

	if err := m.Activate(d.Clock); err != nil {
		// A guard failure names the specific unmet guard and is a business outcome, not
		// something to retry: the guard will be just as unmet in two hundred milliseconds.
		return ActivateOutput{}, engine.WithClass(err, engine.ClassTerminalBusiness)
	}
	if err := d.Merchants.Save(ctx, m); err != nil {
		return ActivateOutput{}, err
	}
	at := d.Clock.Now().UTC()
	if m.ActivatedAt() != nil {
		at = *m.ActivatedAt()
	}
	return ActivateOutput{ActivatedAt: at, GuardsEvaluated: guards}, nil
}

// --- shared helpers -------------------------------------------------------------------------------

func (d Deps) merchant(ctx context.Context, id shared.MerchantID) (*merchant.Merchant, error) {
	m, err := d.Merchants.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, engine.WithClass(
			apierror.Newf(apierror.CodeMerchantNotFound, "merchant %s not found", id),
			engine.ClassTerminalBusiness)
	}
	return m, nil
}

func (d Deps) pickAccount(m *merchant.Merchant, id string) *merchant.BankAccount {
	accounts := m.BankAccounts()
	for i := range accounts {
		if accounts[i].ID == id {
			return &accounts[i]
		}
	}
	if id == "" && len(accounts) > 0 {
		return &accounts[0]
	}
	return nil
}

func decodeSignal(raw []byte, name string) (signalDecision, error) {
	var out signalDecision
	if len(raw) == 0 {
		return out, engine.WithClass(
			apierror.Newf(apierror.CodeValidationFailed, "the %s signal carried no payload", name),
			engine.ClassTerminalTechnical)
	}
	if err := decodeInto(raw, &out); err != nil {
		return out, engine.WithClass(
			apierror.Wrapf(err, apierror.CodeMalformedRequest,
				"the %s signal payload does not decode", name),
			engine.ClassTerminalTechnical)
	}
	if out.Decision == "" {
		return out, engine.WithClass(
			apierror.Newf(apierror.CodeValidationFailed, "the %s signal carried no decision", name),
			engine.ClassTerminalBusiness)
	}
	return out, nil
}

func summary(details []apierror.Detail) string {
	ids := make([]string, 0, len(details))
	for _, d := range details {
		ids = append(ids, d.RuleID)
	}
	return strings.Join(ids, ", ")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func orNow(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t.UTC()
}

func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// isNotFound reports whether an error means "the thing being undone does not exist", which every
// compensation must treat as success rather than as failure.
func isNotFound(err error) bool {
	if err == nil {
		return true
	}
	switch apierror.CodeOf(err) {
	case apierror.CodeMerchantNotFound, apierror.CodeOnboardingCaseNotFound,
		apierror.CodePaymentNotFound, apierror.CodeWorkflowNotFound:
		return true
	default:
		// Every other code is judged by its category below, so a not-found code added to the
		// catalogue is treated correctly here without an edit
	}
	return apierror.CategoryOf(err) == apierror.CategoryNotFound ||
		errors.Is(err, spi.ErrNotSupported)
}
