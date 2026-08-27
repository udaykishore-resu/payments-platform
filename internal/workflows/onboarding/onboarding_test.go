package onboarding_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine/enginetest"
	enginepg "github.com/udaykishore-resu/payments-platform/internal/workflows/engine/postgres"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/onboarding"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// fixture wires the real onboarding definition and its real activities against doubles for every
// port, on the real Postgres engine driven by the in-memory repository. Nothing here is a mock of
// the workflow itself: the saga that runs is the one that ships.
type fixture struct {
	t *testing.T

	clock    *testClock
	repo     *enginetest.Repo
	acts     *engine.Activities
	eng      *enginepg.Engine
	audit    *enginetest.Auditor
	merchant *merchant.Merchant

	merchants *merchantRepo
	kyc       *kycProvider
	bank      *bankValidator
	secrets   *secretsStore
	objects   *objectStore
	configs   *configStore
	gateways  *provisionerSet
	sandbox   *sandbox

	input onboarding.Input
	id    shared.WorkflowID
}

const (
	gwStripe = shared.GatewayID("stripe")
	gwAdyen  = shared.GatewayID("adyen")
)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t:         t,
		clock:     newClock(),
		acts:      engine.NewActivities(),
		audit:     enginetest.NewAuditor(),
		merchants: newMerchantRepo(),
		kyc:       newKYCProvider(),
		bank:      newBankValidator(),
		secrets:   newSecretsStore(),
		objects:   newObjectStore(),
		configs:   newConfigStore(),
		gateways:  newProvisionerSet(gwStripe, gwAdyen),
		sandbox:   newSandbox(gwStripe, gwAdyen),
	}
	f.repo = enginetest.NewRepo(f.clock)

	tenant := shared.TenantID("ten_test")
	f.merchant = newTestMerchant(t, f.clock, tenant)
	if err := f.merchants.Create(context.Background(), f.merchant); err != nil {
		t.Fatalf("seeding the merchant: %v", err)
	}

	deps := onboarding.Deps{
		Merchants:   f.merchants,
		Configs:     f.configs,
		KYC:         f.kyc,
		Bank:        f.bank,
		Secrets:     f.secrets,
		Objects:     f.objects,
		Gateways:    f.gateways,
		Credentials: &credentialSource{},
		Sandbox:     f.sandbox,
		Validator:   onboarding.DefaultMerchantValidator{},
		Clock:       f.clock,
		WebhookURL: func(g shared.GatewayID) string {
			return "https://webhooks.example/v1/webhooks/" + string(g)
		},
	}
	if err := onboarding.Register(f.acts, deps); err != nil {
		t.Fatalf("registering the onboarding activities: %v", err)
	}

	eng, err := enginepg.New(enginepg.Options{
		Repo: f.repo, Activities: f.acts, Definitions: engine.NewRegistry(),
		Clock: f.clock, Auditor: f.audit, WorkerID: "onboarding-test",
		Salt: []byte("onboarding-test"), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tenant: func(context.Context) shared.TenantID { return tenant },
	})
	if err != nil {
		t.Fatalf("building the engine: %v", err)
	}
	f.eng = eng

	f.input = onboarding.Input{
		MerchantID:     f.merchant.ID(),
		TenantID:       tenant,
		Environment:    shared.EnvironmentSandbox,
		Gateways:       []shared.GatewayID{gwStripe, gwAdyen},
		Countries:      []shared.Country{"GB", "US"},
		Currencies:     []money.Currency{"USD"},
		PaymentMethods: []shared.PaymentMethod{shared.MethodCard},
		BankAccountID:  "ba_1",
		RequestedBy:    "usr_operator_1",
	}
	return f
}

func (f *fixture) start() shared.WorkflowID {
	f.t.Helper()
	raw, err := json.Marshal(f.input)
	if err != nil {
		f.t.Fatalf("marshalling the input: %v", err)
	}
	id, err := f.eng.Start(context.Background(), onboarding.Definition(), "", raw)
	if err != nil {
		f.t.Fatalf("Start: %v", err)
	}
	f.id = id
	return id
}

func (f *fixture) resume() {
	f.t.Helper()
	if err := f.eng.Resume(context.Background(), f.id); err != nil {
		f.t.Fatalf("Resume: %v", err)
	}
}

func (f *fixture) instance() *engine.Instance {
	f.t.Helper()
	inst, err := f.eng.Get(context.Background(), f.id)
	if err != nil {
		f.t.Fatalf("Get: %v", err)
	}
	return inst
}

func (f *fixture) signal(name string, payload any, principal string) {
	f.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatalf("marshalling the signal: %v", err)
	}
	if err := f.eng.Signal(context.Background(), f.id, name, engine.SignalPayload{
		Data: body, Principal: principal, Scopes: []string{"onboarding:approve"},
		Reason: "test", SourceIP: "10.0.0.1",
	}); err != nil {
		f.t.Fatalf("Signal(%s): %v", name, err)
	}
}

func (f *fixture) merchantNow() *merchant.Merchant {
	f.t.Helper()
	m, err := f.merchants.Get(context.Background(), f.input.MerchantID)
	if err != nil {
		f.t.Fatalf("loading the merchant: %v", err)
	}
	return m
}

// --- definition soundness ---------------------------------------------------------------------

func TestOnboardingDefinitionIsSound(t *testing.T) {
	t.Parallel()
	def := onboarding.Definition()
	if err := def.Validate(); err != nil {
		t.Fatalf("merchant-onboarding@v1 is not sound: %v", err)
	}
	if got := len(def.Steps); got != 12 {
		t.Fatalf("the definition has %d steps, want the twelve of baseline §11", got)
	}
	want := []string{
		onboarding.StepValidateMerchant, onboarding.StepSubmitKYC, onboarding.StepAwaitKYCDecision,
		onboarding.StepValidateBankAccount, onboarding.StepProvisionGateways,
		onboarding.StepStoreCredentials, onboarding.StepRegisterWebhooks,
		onboarding.StepApplyConfiguration, onboarding.StepSandboxValidation,
		onboarding.StepCertification, onboarding.StepComplianceReview, onboarding.StepActivate,
	}
	for i, name := range want {
		if def.Steps[i].Name != name {
			t.Fatalf("step %d is %q, want %q", i+1, def.Steps[i].Name, name)
		}
	}

	// The two pivots, and their kinds. Conflating them produces a wrong design: one is a
	// regulated record that must be retained, the other is a state after which real money moves.
	if got := def.PivotIndex(engine.PivotRetained); got != 2 {
		t.Errorf("the retained pivot is at index %d, want await-kyc-decision at 2", got)
	}
	if got := def.PivotIndex(engine.PivotIrreversible); got != 11 {
		t.Errorf("the irreversible pivot is at index %d, want activate at 11", got)
	}
	if def.Steps[11].CompensationKind != engine.CompensationForward {
		t.Error("activate's compensation must be declared as forward recovery, not rollback")
	}
}

func TestEveryActivityAndCompensationIsRegistered(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if err := f.acts.VerifyDefinition(onboarding.Definition()); err != nil {
		t.Fatalf("the definition names activities this binary does not contain: %v", err)
	}
}

// TestMalformedVariantsFailEachSoundnessCheck takes the *real* definition and breaks it in each
// of the four ways that make a saga unsound. Deriving the variants from the real thing is the
// point: a hand-written broken definition proves the validator rejects something, not that it
// would reject a mistake somebody could actually make in this file.
func TestMalformedVariantsFailEachSoundnessCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		mutate   func(*engine.Definition)
		wantCode string
	}{
		{
			name: "a rollback declared after the money pivot",
			mutate: func(d *engine.Definition) {
				d.Steps = append(d.Steps, engine.Step{
					Name: "post-activation-cleanup", Activity: "post-activation-cleanup",
					Timeout: time.Second, Idempotent: true, Compensation: "undo-cleanup",
					Retry: engine.RetryPolicy{MaxAttempts: 1},
				})
			},
			wantCode: "COMPENSATION_AFTER_PIVOT",
		},
		{
			name: "the compliance gate left without a timeout",
			mutate: func(d *engine.Definition) {
				d.Steps[10].Timeout = 0
			},
			wantCode: "MANUAL_GATE_WITHOUT_TIMEOUT",
		},
		{
			name: "provision-gateways retried without being idempotent",
			mutate: func(d *engine.Definition) {
				d.Steps[4].Idempotent = false
			},
			wantCode: "RETRY_WITHOUT_IDEMPOTENCE",
		},
		{
			name: "certification duplicated, making the second copy unreachable",
			mutate: func(d *engine.Definition) {
				d.Steps = append(d.Steps, d.Steps[9])
			},
			wantCode: "UNREACHABLE_STEP",
		},
	}

	for _, tc := range cases {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			def := onboarding.Definition()
			tc.mutate(def)

			err := def.Validate()
			if err == nil {
				t.Fatalf("Validate accepted the malformed variant")
			}
			var ae *apierror.Error
			if !errors.As(err, &ae) {
				t.Fatalf("the error is not an *apierror.Error: %v", err)
			}
			found := false
			for _, d := range ae.Details {
				if d.Code == tc.wantCode {
					found = true
				}
			}
			if !found {
				t.Fatalf("no detail carried %s; got %+v", tc.wantCode, ae.Details)
			}
		})
	}
}

// --- the full saga -------------------------------------------------------------------------------

// TestOnboardingHappyPath drives the whole twelve-step saga and checks that each step drove the
// merchant transition it owns and emitted the event that goes with it.
func TestOnboardingHappyPath(t *testing.T) {
	// Verifies: BR-04, FR-24, FR-25, FR-26.
	t.Parallel()
	f := newFixture(t)
	f.start()

	// Steps 1 and 2, then the KYC gate.
	f.resume()
	if got := f.instance().State; got != engine.InstanceWaitingSignal {
		t.Fatalf("expected the KYC gate, got %s (%s)", got, f.instance().LastError)
	}
	if got := f.merchantNow().Status(); got != merchant.StatusKYCPending {
		t.Fatalf("merchant is %s after submit-kyc, want KYC_PENDING", got)
	}
	if !f.merchants.HasEvent(merchant.EventMerchantValidated) {
		t.Error("merchant.validated.v1 was not emitted")
	}
	if got := f.kyc.Submits(); got != 1 {
		t.Fatalf("the KYC vendor saw %d submissions, want 1", got)
	}

	f.signal(onboarding.SignalKYCDecision, map[string]any{
		"decision": "APPROVED", "riskRating": "STANDARD", "evidenceRef": "s3://kyc/evidence/1",
	}, "svc_kyc_acl")

	// Steps 3 through 10, then the compliance gate.
	f.resume()
	inst := f.instance()
	if inst.State != engine.InstanceWaitingSignal {
		t.Fatalf("expected the compliance gate, got %s (%s)", inst.State, inst.LastError)
	}
	if got := f.merchantNow().Status(); got != merchant.StatusCertification {
		t.Fatalf("merchant is %s before compliance review, want CERTIFICATION", got)
	}
	for _, e := range []merchant.EventType{
		merchant.EventMerchantKYCApproved,
		merchant.EventMerchantBankValidated,
		merchant.EventMerchantGatewayProvisioned,
	} {
		if !f.merchants.HasEvent(e) {
			t.Errorf("%s was not emitted", e)
		}
	}
	if got := f.gateways.get(gwStripe).Provisions(); got != 1 {
		t.Errorf("stripe was provisioned %d times, want 1", got)
	}
	if got := f.gateways.get(gwAdyen).Provisions(); got != 1 {
		t.Errorf("adyen was provisioned %d times, want 1", got)
	}
	if got := f.objects.Puts(); got != 1 {
		t.Fatalf("%d certification reports were stored, want exactly the full run's one", got)
	}

	// PRODUCTION_READY must be unreachable without a passing report. The merchant is sitting in
	// CERTIFICATION with a report on file and still cannot activate, because compliance has not
	// ruled.
	if err := f.merchantNow().Activate(f.clock); err == nil {
		t.Fatal("a merchant reached ACTIVE without compliance approval")
	}

	f.signal(onboarding.SignalComplianceApproval, map[string]any{
		"decision": "APPROVE", "reviewerId": "usr_compliance_9",
		"attestationRef": "att_2026_001", "reason": "sanctions and MCC reviewed",
	}, "usr_compliance_9")

	// Steps 11 and 12.
	f.resume()
	inst = f.instance()
	if inst.State != engine.InstanceCompleted {
		t.Fatalf("expected COMPLETED, got %s (%s)", inst.State, inst.LastError)
	}
	m := f.merchantNow()
	if m.Status() != merchant.StatusActive {
		t.Fatalf("merchant is %s, want ACTIVE", m.Status())
	}
	if !f.merchants.HasEvent(merchant.EventMerchantCertified) {
		t.Error("merchant.certified.v1 was not emitted")
	}
	if !f.merchants.HasEvent(merchant.EventMerchantActivated) {
		t.Error("merchant.activated.v1 was not emitted; the data plane would never let payments through")
	}
	if m.CertificationID() == "" {
		t.Error("the merchant carries no certification reference")
	}
	if !strings.Contains(m.CertificationID(), "sha256:") {
		t.Errorf("the certification reference carries no content hash: %q", m.CertificationID())
	}

	// Every step checkpointed exactly once.
	if got := len(inst.CompletedSteps()); got != 12 {
		t.Fatalf("%d steps were checkpointed, want 12: %v", got, inst.CompletedSteps())
	}
	// The gate signals were audited with their principals.
	if ev := f.audit.Find(engine.ActionSignal); ev == nil || ev.Principal == "" {
		t.Fatalf("a gate signal was not audited with a principal: %v", ev)
	}
}

// TestResumeDoesNotReplayCompletedSteps re-drives the saga repeatedly and asserts that the
// vendors saw each side effect exactly once.
func TestResumeDoesNotReplayCompletedSteps(t *testing.T) {
	// Verifies: FR-32.
	t.Parallel()
	f := newFixture(t)
	f.start()

	f.resume()
	f.resume() // a poller picking the instance up again while it waits on the gate
	f.signal(onboarding.SignalKYCDecision, map[string]any{"decision": "APPROVED"}, "svc_kyc")
	f.resume()
	f.resume()

	if got := f.kyc.Submits(); got != 1 {
		t.Errorf("the KYC vendor saw %d submissions across four resumes, want 1", got)
	}
	if got := f.bank.Calls(); got != 1 {
		t.Errorf("the bank validator saw %d calls, want 1 — a second penny-drop is real money", got)
	}
	if got := f.gateways.get(gwStripe).Provisions(); got != 1 {
		t.Errorf("stripe was provisioned %d times, want 1", got)
	}
	if got := f.objects.Puts(); got != 1 {
		t.Errorf("%d certification reports were written, want 1", got)
	}
}

// TestCompensationOrderOnCertificationFailure is the sequence from the automation plane's own
// worked example: certification fails, and the saga unwinds 8 → 7 → 6 → 5, skipping everything at
// or before the retained pivot.
func TestCompensationOrderOnCertificationFailure(t *testing.T) {
	// Verifies: FR-31.
	t.Parallel()
	f := newFixture(t)
	// A gateway that never delivers a webhook fails the "the async loop is closed" assertion,
	// which is a business rejection and unwinds the saga.
	f.sandbox.noWebhook = true
	f.start()

	f.resume()
	f.signal(onboarding.SignalKYCDecision, map[string]any{"decision": "APPROVED"}, "svc_kyc")
	f.resume()

	inst := f.instance()
	if inst.State != engine.InstanceFailed {
		t.Fatalf("expected FAILED after an assertion failure, got %s", inst.State)
	}

	// 8: the configuration rolled back by publishing a new version, never by deleting one.
	versions := f.configs.Versions(f.input.MerchantID)
	if len(versions) != 1 {
		// There is no earlier version to restore on a first onboarding, so the compensation is a
		// recorded no-op rather than a destructive one — deleting the only configuration would
		// leave the merchant with none at all.
		t.Fatalf("the configuration history has %d versions; a rollback must never delete one", len(versions))
	}
	// 7: webhook registrations deleted before the sub-accounts they belong to.
	if got := len(f.gateways.get(gwStripe).Unregistered()); got != 1 {
		t.Errorf("stripe's webhook registration was deleted %d times, want 1", got)
	}
	// 6: secret versions scheduled for deletion.
	if got := len(f.secrets.Deleted()); got == 0 {
		t.Error("no secret version was deleted")
	}
	// 5: sub-accounts de-provisioned.
	if got := len(f.gateways.get(gwStripe).Deprovisioned()); got != 1 {
		t.Errorf("stripe's sub-account was de-provisioned %d times, want 1", got)
	}
	// 2 and 3 are at or before the retained pivot: the KYC decision is a record kept by law and
	// there is nothing to cancel.
	if got := f.kyc.Cancelled(); len(got) != 0 {
		t.Errorf("the KYC case was cancelled after the decision landed: %v", got)
	}

	// The step states record the reverse-order walk.
	states := f.repo.StepStates(f.id)
	for _, step := range []string{
		onboarding.StepProvisionGateways, onboarding.StepStoreCredentials,
		onboarding.StepRegisterWebhooks, onboarding.StepApplyConfiguration,
	} {
		if states[step] != string(engine.StepCompensated) {
			t.Errorf("step %s is %q, want COMPENSATED", step, states[step])
		}
	}
	if len(f.repo.DLQ()) == 0 {
		t.Error("an aborted onboarding must leave a DLQ entry with the failing assertion IDs")
	}
}

// TestAbortBeforeTheKYCDecisionCancelsTheCase is the other side of the pivot: aborting *before*
// the decision lands genuinely cancels the vendor case, and the world is as it was.
func TestAbortBeforeTheKYCDecisionCancelsTheCase(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.start()
	f.resume()

	if err := f.eng.Cancel(context.Background(), f.id, "the merchant withdrew the application"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	f.resume()

	if got := f.instance().State; got != engine.InstanceCanceled {
		t.Fatalf("expected CANCELED, got %s", got)
	}
	if got := f.kyc.Cancelled(); len(got) != 1 {
		t.Fatalf("the KYC case was cancelled %d times, want 1 — before the decision it is genuinely reversible", len(got))
	}
}

// TestKYCRejectionIsABusinessOutcome: a rejection is recorded and surfaced, never retried.
func TestKYCRejectionIsABusinessOutcome(t *testing.T) {
	// Verifies: BR-05, FR-21.
	t.Parallel()
	f := newFixture(t)
	f.start()
	f.resume()
	f.signal(onboarding.SignalKYCDecision, map[string]any{
		"decision": "REJECTED", "reasonCodes": []string{"ADVERSE_MEDIA", "PEP_MATCH"},
	}, "svc_kyc")
	f.resume()

	if got := f.merchantNow().Status(); got != merchant.StatusKYCFailed {
		t.Fatalf("merchant is %s, want KYC_FAILED", got)
	}
	if !f.merchants.HasEvent(merchant.EventMerchantKYCFailed) {
		t.Error("merchant.kyc_failed.v1 was not emitted")
	}
	if got := f.instance().State; got != engine.InstanceFailed {
		t.Fatalf("instance is %s, want FAILED", got)
	}
	// The reason codes must reach the DLQ so the merchant can be told what to fix.
	dlq := f.repo.DLQ()
	if len(dlq) == 0 || !strings.Contains(dlq[len(dlq)-1].Reason, "ADVERSE_MEDIA") {
		t.Fatalf("the rejection's reason codes did not reach the DLQ: %+v", dlq)
	}
}

// TestComplianceRejectionUsesTheAmendedState checks amendment A-01's path: the integration works,
// the business decision was no, and the audit trail must say exactly that rather than recording a
// merchant that was production-ready when compliance had refused.
func TestComplianceRejectionUsesTheAmendedState(t *testing.T) {
	// Verifies: BR-19, FR-29.
	t.Parallel()
	f := newFixture(t)
	f.start()
	f.resume()
	f.signal(onboarding.SignalKYCDecision, map[string]any{"decision": "APPROVED"}, "svc_kyc")
	f.resume()
	f.signal(onboarding.SignalComplianceApproval, map[string]any{
		"decision": "REJECT", "reviewerId": "usr_compliance_9",
		"reasonCodes": []string{"SANCTIONS_NEXUS"}, "reason": "beneficial owner in a sanctioned jurisdiction",
	}, "usr_compliance_9")
	f.resume()

	m := f.merchantNow()
	if m.Status() != merchant.StatusComplianceRejected {
		t.Fatalf("merchant is %s, want COMPLIANCE_REJECTED", m.Status())
	}
	if m.Status() == merchant.StatusProductionReady {
		t.Fatal("a rejected merchant was marked production-ready")
	}
	if !f.merchants.HasEvent(merchant.EventMerchantComplianceRejected) {
		t.Error("merchant.compliance_rejected.v1 was not emitted")
	}
	if got := f.instance().State; got != engine.InstanceFailed {
		t.Fatalf("instance is %s, want FAILED", got)
	}
}

// --- credential hygiene ---------------------------------------------------------------------------

// TestStoreCredentialsOutputCarriesNoMaterial is the test the step's own doc comment promises.
//
// The output is checkpointed into workflow_instances.context and would otherwise be readable by
// anyone with database access, which would defeat the entire separation between the control plane
// and the secrets store.
func TestStoreCredentialsOutputCarriesNoMaterial(t *testing.T) {
	// Verifies: BR-11, NFR-32.
	t.Parallel()
	f := newFixture(t)
	f.start()
	f.resume()
	f.signal(onboarding.SignalKYCDecision, map[string]any{"decision": "APPROVED"}, "svc_kyc")
	f.resume()

	inst := f.instance()
	var stored []byte
	for _, s := range inst.Steps {
		if s.Name == onboarding.StepStoreCredentials {
			stored = s.Output
		}
	}
	if len(stored) == 0 {
		t.Fatal("store-credentials checkpointed no output")
	}
	body := string(stored)
	for _, forbidden := range []string{"supersecret", "sk_test_stripe", "sk_test_adyen", "whsec_test"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("credential material leaked into the workflow context: %s", body)
		}
	}
	var out onboarding.StoreCredentialsOutput
	if err := json.Unmarshal(stored, &out); err != nil {
		t.Fatalf("the output does not decode: %v", err)
	}
	if len(out.Refs) != 2 {
		t.Fatalf("%d credential references were recorded, want 2", len(out.Refs))
	}
	for _, r := range out.Refs {
		if !strings.HasPrefix(r.SecretRef, "secret://") {
			t.Errorf("reference %q is not a secret:// reference", r.SecretRef)
		}
		if !strings.HasPrefix(r.Fingerprint, "fp_") {
			t.Errorf("fingerprint %q is not a fingerprint", r.Fingerprint)
		}
	}

	// The whole instance context is exported for support; nothing in it may be material.
	if strings.Contains(string(inst.Context), "supersecret") {
		t.Fatal("credential material is present in the instance context")
	}
}

// --- certification --------------------------------------------------------------------------------

func TestCertificationReportIsHashedAndStoredUnderRetention(t *testing.T) {
	// Verifies: BR-17, FR-28.
	t.Parallel()
	f := newFixture(t)
	f.start()
	f.resume()
	f.signal(onboarding.SignalKYCDecision, map[string]any{"decision": "APPROVED"}, "svc_kyc")
	f.resume()

	inst := f.instance()
	var out onboarding.CertificationRunOutput
	for _, s := range inst.Steps {
		if s.Name == onboarding.StepCertification {
			if err := json.Unmarshal(s.Output, &out); err != nil {
				t.Fatalf("the certification output does not decode: %v", err)
			}
		}
	}
	if !out.Passed || out.ReportRef == "" || out.ContentHash == "" {
		t.Fatalf("the certification step produced no signed report: %+v", out)
	}

	body, err := f.objects.Get(context.Background(), out.ReportRef)
	if err != nil {
		t.Fatalf("the report is not in the object store: %v", err)
	}
	var report onboarding.CertificationReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatalf("the stored report does not decode: %v", err)
	}
	if err := report.VerifyHash(); err != nil {
		t.Fatalf("the stored report's hash does not verify: %v", err)
	}

	opts := f.objects.Options(out.ReportRef)
	if !opts.WORM || opts.RetainUntil == nil {
		t.Fatalf("the report was stored without Object Lock: %+v — a report that can be overwritten is not evidence", opts)
	}

	// Every cell carries every assertion, so two runs can be diffed.
	if len(report.Cells) == 0 {
		t.Fatal("the report contains no cells")
	}
	for _, cell := range report.Cells {
		if len(cell.Assertions) != len(onboarding.AllAssertions) {
			t.Fatalf("cell %s recorded %d assertions, want %d",
				cell.Cell.Key(), len(cell.Assertions), len(onboarding.AllAssertions))
		}
	}
}

func TestCertificationHashIsDeterministicAndTamperEvident(t *testing.T) {
	t.Parallel()
	report := &onboarding.CertificationReport{
		RunID: "run_1", MerchantID: "mrc_1", TenantID: "ten_1",
		Environment: "sandbox", Workflow: "merchant-onboarding",
		StartedAt: time.Unix(0, 0).UTC(), CompletedAt: time.Unix(60, 0).UTC(),
		Passed: true,
		Cells: []onboarding.CellResult{
			{Cell: onboarding.MatrixCell{Gateway: "stripe", Method: shared.MethodCard, Currency: "USD"}, Passed: true},
			{Cell: onboarding.MatrixCell{Gateway: "adyen", Method: shared.MethodCard, Currency: "USD"}, Passed: true},
		},
	}
	first, err := report.ComputeHash()
	if err != nil {
		t.Fatalf("ComputeHash: %v", err)
	}

	// Reordering the cells must not change the hash: a hash that depends on iteration order
	// would cry wolf on every run and then be ignored.
	report.Cells[0], report.Cells[1] = report.Cells[1], report.Cells[0]
	second, err := report.ComputeHash()
	if err != nil {
		t.Fatalf("ComputeHash: %v", err)
	}
	if first != second {
		t.Fatalf("the hash depends on cell order: %s vs %s", first, second)
	}

	report.ContentHash = first
	if err := report.VerifyHash(); err != nil {
		t.Fatalf("a genuine report failed verification: %v", err)
	}

	// A tampered verdict must be detectable.
	report.Passed = false
	if err := report.VerifyHash(); err == nil {
		t.Fatal("a modified report verified; the evidence is not tamper-evident")
	}
}

func TestCertificationCatchesAGatewayThatIgnoresIdempotencyKeys(t *testing.T) {
	// Verifies: BR-16, FR-27.
	t.Parallel()
	f := newFixture(t)
	// Every crash-safety argument in the automation plane rests on the vendor deduplicating a
	// repeated idempotency key. This is the assertion that checks the assumption.
	f.sandbox.gateways[gwStripe].noDedupe = true
	f.start()
	f.resume()
	f.signal(onboarding.SignalKYCDecision, map[string]any{"decision": "APPROVED"}, "svc_kyc")
	f.resume()

	if got := f.instance().State; got != engine.InstanceFailed {
		t.Fatalf("a gateway that ignores idempotency keys was certified; instance is %s", got)
	}
	found := false
	for _, entry := range f.repo.DLQ() {
		if strings.Contains(entry.Reason, onboarding.AssertIdempotency) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the failing assertion was not named in the DLQ: %+v", f.repo.DLQ())
	}
}

// --- compensations in isolation ---------------------------------------------------------------------

// TestCompensationsTolerateTheForwardOperationNeverHavingHappened is the property every
// compensation must have: it runs after crashes, and the crash may have happened before the thing
// being undone was created.
func TestCompensationsTolerateTheForwardOperationNeverHavingHappened(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	cases := []struct {
		activity string
		payload  string
	}{
		{onboarding.CompCancelKYCCase, `{}`},
		{onboarding.CompDeprovisionGateways, `{"connections":[]}`},
		{onboarding.CompDeleteSecrets, `{"refs":[]}`},
		{onboarding.CompDeleteWebhooks, `{"registrations":[]}`},
		{onboarding.CompRollbackConfig, `{"configVersion":0}`},
		{onboarding.CompSuspendMerchant, `{}`},
	}
	for _, tc := range cases {

		t.Run(tc.activity, func(t *testing.T) {
			act, err := f.acts.Get(tc.activity)
			if err != nil {
				t.Fatalf("Get(%s): %v", tc.activity, err)
			}
			if _, err := act.Execute(context.Background(), engine.Input{
				WorkflowID:  "wfr_test",
				BusinessKey: string(f.input.MerchantID),
				Step:        "irrelevant",
				Payload:     []byte(tc.payload),
			}); err != nil {
				t.Fatalf("%s failed on a never-created resource: %v", tc.activity, err)
			}
		})
	}
}

// TestSuspendMerchantIsForwardRecovery: step 12's compensation moves the merchant to a new safe
// state rather than restoring the old one, and it deliberately leaves refunds possible.
func TestSuspendMerchantIsForwardRecovery(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.start()
	f.resume()
	f.signal(onboarding.SignalKYCDecision, map[string]any{"decision": "APPROVED"}, "svc_kyc")
	f.resume()
	f.signal(onboarding.SignalComplianceApproval, map[string]any{
		"decision": "APPROVE", "reviewerId": "usr_c", "attestationRef": "att_1",
	}, "usr_c")
	f.resume()
	if got := f.merchantNow().Status(); got != merchant.StatusActive {
		t.Fatalf("setup: merchant is %s, want ACTIVE", got)
	}

	act, err := f.acts.Get(onboarding.CompSuspendMerchant)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	out, err := act.Execute(context.Background(), engine.Input{
		BusinessKey: string(f.input.MerchantID), Payload: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("suspend-merchant: %v", err)
	}
	if !strings.Contains(string(out), `"suspended":true`) {
		t.Fatalf("the compensation reported %s", out)
	}

	m := f.merchantNow()
	if m.Status() != merchant.StatusSuspended {
		t.Fatalf("merchant is %s, want SUSPENDED", m.Status())
	}
	if m.Status().CanAcceptPayments() {
		t.Error("a suspended merchant may not take new payments")
	}
	if !m.Status().CanIssueRefunds() {
		t.Error("a suspended merchant must still be able to refund; blocking refunds traps merchant money")
	}

	// Running it again is a no-op, because a compensation is retried with the same key.
	if _, err := act.Execute(context.Background(), engine.Input{
		BusinessKey: string(f.input.MerchantID), Payload: []byte(`{}`),
	}); err != nil {
		t.Fatalf("the second suspension attempt failed: %v", err)
	}
}

// --- validation plane ------------------------------------------------------------------------------

func TestValidationRejectsAMerchantBeforeAnySideEffect(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// A gateway listed twice would produce two sub-accounts at one gateway.
	f.input.Gateways = []shared.GatewayID{gwStripe, gwStripe}
	f.start()

	f.resume()

	if got := f.instance().State; got != engine.InstanceFailed {
		t.Fatalf("instance is %s, want FAILED", got)
	}
	if got := f.merchantNow().Status(); got != merchant.StatusValidationFailed {
		t.Fatalf("merchant is %s, want VALIDATION_FAILED", got)
	}
	// The point of running the pure step first: no external side effect exists to compensate.
	if got := f.kyc.Submits(); got != 0 {
		t.Fatalf("the KYC vendor was called %d times despite a validation failure", got)
	}
	if got := f.gateways.get(gwStripe).Provisions(); got != 0 {
		t.Fatalf("a gateway was provisioned %d times despite a validation failure", got)
	}
	if !f.merchants.HasEvent(merchant.EventMerchantValidationFailed) {
		t.Error("merchant.validation_failed.v1 was not emitted")
	}
}

func TestDefaultValidatorReportsEveryFailureAtOnce(t *testing.T) {
	// Verifies: FR-16.
	t.Parallel()
	f := newFixture(t)
	f.input.Currencies = []money.Currency{"XXX"}
	f.input.Countries = []shared.Country{"ZZ"}
	f.input.Gateways = nil

	outcomes, err := onboarding.DefaultMerchantValidator{}.Validate(context.Background(), f.merchant, f.input)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	failed := map[string]bool{}
	for _, o := range outcomes {
		if !o.Passed {
			failed[o.RuleID] = true
		}
	}
	// All three at once. A validator that stops at the first failure produces the interaction
	// where a merchant fixes one field, resubmits, waits, and is told about the next one.
	for _, rule := range []string{"L2.CURRENCIES_SUPPORTED", "L2.COUNTRIES_VALID", "L2.AT_LEAST_ONE_GATEWAY"} {
		if !failed[rule] {
			t.Errorf("%s did not fail", rule)
		}
	}
	if len(outcomes) < 10 {
		t.Errorf("only %d rules ran; the passing ones are the evidence that the control executed", len(outcomes))
	}
}
