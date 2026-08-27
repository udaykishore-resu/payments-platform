package merchant

import (
	"context"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/apptest"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	dmerchant "github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	dpayment "github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/l2merchant"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

var testEpoch = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)

const testTenant shared.TenantID = "ten_01HZTESTTENANT00000000000"

type env struct {
	t     *testing.T
	store *apptest.Store
	clock *apptest.Clock
	audit *apptest.Auditor
	svc   *Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	store := apptest.NewStore()
	clock := apptest.NewClock(testEpoch)
	deps := l2merchant.DefaultDeps()
	deps.SupportedCountries = []shared.Country{"DE", "FR"}
	deps.LicensedCountries = []shared.Country{"DE", "FR"}
	deps.MonthlyVolumeCeiling = money.MustNew(100000000, "EUR")
	a := &apptest.Auditor{Store: store}
	return &env{
		t: t, store: store, clock: clock, audit: a,
		svc: NewService(Deps{
			UoW:   apptest.NewUnitOfWork(store, apptest.NewRecorder()),
			Audit: a, Clock: clock, L2: deps,
		}),
	}
}

func createCmd() CreateCommand {
	return CreateCommand{
		TenantID:     testTenant,
		LegalName:    "Beispiel Handel GmbH",
		DisplayName:  "Beispiel",
		ExternalRef:  "ext-1",
		Environment:  shared.EnvironmentSandbox,
		BusinessType: l2merchant.LLC,
		Profile: dmerchant.BusinessProfile{
			RegistrationNumber:    "HRB123456",
			Country:               "DE",
			MCC:                   "5734",
			WebsiteURL:            "https://beispiel.example",
			Description:           "software",
			ExpectedMonthlyVolume: money.MustNew(5000000, "EUR"),
			ExpectedAverageTicket: money.MustNew(5000, "EUR"),
		},
		OperatingCountries: []shared.Country{"DE"},
		Actor:              Actor{ID: "usr_1", Name: "Operator"},
	}
}

// TestCreateWritesTheStateChangeAndTheAuditRecordTogether.
//
// The two share a transaction. Writing the audit record afterwards, best-effort, produces a trail
// that diverges from reality precisely when something went wrong — the only time anybody reads it.
func TestCreateWritesTheStateChangeAndTheAuditRecordTogether(t *testing.T) {
	// Verifies: FR-09, FR-88.
	t.Parallel()
	e := newEnv(t)
	m, err := e.svc.Create(context.Background(), createCmd())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if m.Status() != dmerchant.StatusCreated {
		t.Fatalf("status = %s, want CREATED", m.Status())
	}
	if got := e.audit.Actions(); len(got) != 1 || got[0] != "merchant.created" {
		t.Fatalf("audit = %v, want [merchant.created]", got)
	}
	if e.store.AuditLines[0].Detail["actorId"] != "usr_1" {
		t.Fatalf("the audit record does not name the actor: %v", e.store.AuditLines[0].Detail)
	}
}

// TestCreateRunsTheSubmissionSubsetOfL2.
//
// The table is one deviation per row from a submission that passes, which is the only way to tell
// "the rule fired" from "the whole set is unsatisfiable".
func TestCreateRunsTheSubmissionSubsetOfL2(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*CreateCommand)
	}{
		{"an empty legal name", func(c *CreateCommand) { c.LegalName = "" }},
		{"an unknown legal form", func(c *CreateCommand) { c.BusinessType = "PARTNERSHIP_LTD" }},
		{"a prohibited merchant category", func(c *CreateCommand) { c.Profile.MCC = "7995" }},
		{"a country the tenant does not support", func(c *CreateCommand) { c.Profile.Country = "BR" }},
		{"an operating country outside the tenant's licence", func(c *CreateCommand) {
			c.OperatingCountries = []shared.Country{"BR"}
		}},
		{"a plaintext website", func(c *CreateCommand) { c.Profile.WebsiteURL = "http://beispiel.example" }},
		{"a declared volume above the tenant tier", func(c *CreateCommand) {
			c.Profile.ExpectedMonthlyVolume = money.MustNew(900000000, "EUR")
		}},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			cmd := createCmd()
			tc.mutate(&cmd)
			if _, err := e.svc.Create(context.Background(), cmd); err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if len(e.store.AllMerchants()) != 0 {
				t.Fatal("a merchant was persisted for a submission that failed validation")
			}
		})
	}
}

// TestCreateAcceptsAWellFormedSubmission is the positive control for the table above.
func TestCreateAcceptsAWellFormedSubmission(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if _, err := e.svc.Create(context.Background(), createCmd()); err != nil {
		t.Fatalf("a well-formed submission was rejected: %v", err)
	}
}

// TestEveryEntryPointAssertsTenantContext.
func TestEveryEntryPointAssertsTenantContext(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	m := mustCreate(t, e)

	calls := map[string]func() error{
		"Create": func() error {
			c := createCmd()
			c.TenantID = ""
			_, err := e.svc.Create(context.Background(), c)
			return err
		},
		"Get": func() error { _, err := e.svc.Get(context.Background(), "", m.ID()); return err },
		"List": func() error {
			_, _, err := e.svc.List(context.Background(), "", ports.MerchantFilter{}, ports.Page{})
			return err
		},
		"Update": func() error {
			_, err := e.svc.Update(context.Background(), UpdateCommand{MerchantID: m.ID(), IfMatch: ETag(m)})
			return err
		},
		"Suspend": func() error {
			_, err := e.svc.Suspend(context.Background(), LifecycleCommand{
				MerchantID: m.ID(), Actor: Actor{ID: "u", Reason: "r"},
			})
			return err
		},
		"AddPrincipal": func() error {
			_, err := e.svc.AddPrincipal(context.Background(), AddPrincipalCommand{MerchantID: m.ID()})
			return err
		},
	}
	for name, call := range calls {
		err := call()
		if err == nil {
			t.Fatalf("%s accepted a request with no tenant context", name)
		}
		if apierror.CodeOf(err) != apierror.CodeMissingTenantContext {
			t.Fatalf("%s: got %s, want MISSING_TENANT_CONTEXT", name, apierror.CodeOf(err))
		}
	}
}

// TestGetRefusesAMerchantFromAnotherTenantAsNotFound.
func TestGetRefusesAMerchantFromAnotherTenantAsNotFound(t *testing.T) {
	// Verifies: FR-11, NFR-29.
	t.Parallel()
	e := newEnv(t)
	m := mustCreate(t, e)

	_, err := e.svc.Get(context.Background(), "ten_01HZOTHER00000000000000", m.ID())
	if err == nil {
		t.Fatal("a merchant from another tenant was returned")
	}
	if apierror.CodeOf(err) != apierror.CodeMerchantNotFound {
		t.Fatalf("got %s, want MERCHANT_NOT_FOUND (never FORBIDDEN)", apierror.CodeOf(err))
	}
}

// TestUpdateRequiresAndHonoursIfMatch.
//
// An unconditional update silently overwrites a concurrent edit, which is the failure that turns
// two operators into one lost change nobody can find afterwards.
func TestUpdateRequiresAndHonoursIfMatch(t *testing.T) {
	// Verifies: FR-10.
	t.Parallel()
	e := newEnv(t)
	m := mustCreate(t, e)

	if _, err := e.svc.Update(context.Background(), UpdateCommand{
		TenantID: testTenant, MerchantID: m.ID(), Actor: Actor{ID: "u"},
	}); err == nil {
		t.Fatal("an update with no If-Match was accepted")
	}

	version := 7
	tag := ETag(m)
	updated, err := e.svc.Update(context.Background(), UpdateCommand{
		TenantID: testTenant, MerchantID: m.ID(), IfMatch: tag,
		ActiveConfigVersion: &version, Actor: Actor{ID: "u"},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.ActiveConfigVersion() != 7 {
		t.Fatalf("config version = %d, want 7", updated.ActiveConfigVersion())
	}

	// The stale tag must now be refused rather than silently overwrite the change above.
	_, err = e.svc.Update(context.Background(), UpdateCommand{
		TenantID: testTenant, MerchantID: m.ID(), IfMatch: tag,
		ActiveConfigVersion: &version, Actor: Actor{ID: "u"},
	})
	if err == nil {
		t.Fatal("a stale If-Match was accepted")
	}
	if apierror.CodeOf(err) != apierror.CodeConfigurationVersionConflict {
		t.Fatalf("got %s, want CONFIGURATION_VERSION_CONFLICT", apierror.CodeOf(err))
	}
}

// TestSuspendAndReinstate, including the rule that a suspension whose reason requires review
// cannot be lifted by an automated caller.
func TestSuspendAndReinstate(t *testing.T) {
	// Verifies: BR-31, FR-13.
	t.Parallel()
	e := newEnv(t)
	m := activeMerchant(t, e)

	suspended, err := e.svc.Suspend(context.Background(), LifecycleCommand{
		TenantID: testTenant, MerchantID: m.ID(), Reason: dmerchant.SuspendSanctionsHit,
		Actor: Actor{ID: "u", Reason: "screening hit on a principal"},
	})
	if err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if suspended.Status() != dmerchant.StatusSuspended {
		t.Fatalf("status = %s, want SUSPENDED", suspended.Status())
	}

	if _, err := e.svc.Reinstate(context.Background(), LifecycleCommand{
		TenantID: testTenant, MerchantID: m.ID(),
		Actor: Actor{ID: "svc", Reason: "metric recovered"}, ActorIsOperator: false,
	}); err == nil {
		t.Fatal("an automated caller lifted a sanctions suspension")
	}

	back, err := e.svc.Reinstate(context.Background(), LifecycleCommand{
		TenantID: testTenant, MerchantID: m.ID(),
		Actor: Actor{ID: "usr_2", Reason: "reviewed and cleared"}, ActorIsOperator: true,
	})
	if err != nil {
		t.Fatalf("Reinstate: %v", err)
	}
	if back.Status() != dmerchant.StatusActive {
		t.Fatalf("status = %s, want ACTIVE", back.Status())
	}
}

// TestSuspensionRequiresAReason. A suspension with no recorded cause turns a two-minute answer
// into an investigation.
func TestSuspensionRequiresAReason(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	m := activeMerchant(t, e)
	if _, err := e.svc.Suspend(context.Background(), LifecycleCommand{
		TenantID: testTenant, MerchantID: m.ID(), Reason: dmerchant.SuspendOperatorAction,
		Actor: Actor{ID: "u"},
	}); err == nil {
		t.Fatal("a suspension with no reason was accepted")
	}
}

// TestTerminateIsRefusedWhilePaymentsAreOpen.
//
// Terminating a merchant with in-flight payments orphans them: authorizations with no owner to
// capture or void, refunds nobody is entitled to issue.
func TestTerminateIsRefusedWhilePaymentsAreOpen(t *testing.T) {
	// Verifies: FR-14.
	t.Parallel()
	e := newEnv(t)
	m := activeMerchant(t, e)
	e.store.PutPayment(openPayment(t, e, m.ID()))

	if _, err := e.svc.Terminate(context.Background(), LifecycleCommand{
		TenantID: testTenant, MerchantID: m.ID(), Actor: Actor{ID: "u", Reason: "closing"},
	}); err == nil {
		t.Fatal("a merchant with an open payment was terminated")
	}
	if got := e.store.Merchant(m.ID()); got.Status() == dmerchant.StatusTerminated {
		t.Fatal("the merchant was terminated despite the refusal")
	}
}

// TestTerminateSucceedsOnceNoPaymentIsOpen is the positive control for the guard above.
func TestTerminateSucceedsOnceNoPaymentIsOpen(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	m := activeMerchant(t, e)

	out, err := e.svc.Terminate(context.Background(), LifecycleCommand{
		TenantID: testTenant, MerchantID: m.ID(), Actor: Actor{ID: "u", Reason: "closing"},
	})
	if err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if out.Status() != dmerchant.StatusTerminated {
		t.Fatalf("status = %s, want TERMINATED", out.Status())
	}
}

// TestAddBankAccountRefusesAnAccountWithNoSecretReference.
//
// The account number goes to the secrets store and the gateway; what lives on the aggregate is a
// mask and a pointer. A use case that accepted the number would put one in a database row, a log
// line and an API response.
func TestAddBankAccountRefusesAnAccountWithNoSecretReference(t *testing.T) {
	// Verifies: BR-06, FR-23.
	t.Parallel()
	e := newEnv(t)
	m := mustCreate(t, e)

	if _, err := e.svc.AddBankAccount(context.Background(), AddBankAccountCommand{
		TenantID: testTenant, MerchantID: m.ID(), Actor: Actor{ID: "u"},
		Account: dmerchant.BankAccount{ID: "ba_1", Country: "DE", Currency: "EUR"},
	}); err == nil {
		t.Fatal("a settlement account with no secret reference was accepted")
	}
}

// TestAddPrincipalEnforcesTheOwnershipSum.
func TestAddPrincipalEnforcesTheOwnershipSum(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	m := mustCreate(t, e)

	add := func(id string, pct int) error {
		_, err := e.svc.AddPrincipal(context.Background(), AddPrincipalCommand{
			TenantID: testTenant, MerchantID: m.ID(), Actor: Actor{ID: "u"},
			Principal: dmerchant.Principal{
				ID: id, LastName: "Owner", Role: dmerchant.RoleBeneficialOwner, OwnershipPct: pct,
			},
		})
		return err
	}
	if err := add("p1", 60); err != nil {
		t.Fatalf("first principal: %v", err)
	}
	if err := add("p2", 60); err == nil {
		t.Fatal("declared ownership was allowed to exceed 100%")
	}
}

// TestAddAttestationRecordsItWithItsExpiryAndAudits.
func TestAddAttestationRecordsItWithItsExpiryAndAudits(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	m := mustCreate(t, e)

	out, err := e.svc.AddAttestation(context.Background(), AddAttestationCommand{
		TenantID: testTenant, MerchantID: m.ID(), Actor: Actor{ID: "u"},
		Attestation: dmerchant.ComplianceAttestation{
			Type: "PCI_SAQ", Reference: "saq-a", AttestedBy: "cfo@example",
			AttestedAt: testEpoch, ExpiresAt: testEpoch.AddDate(1, 0, 0),
		},
	})
	if err != nil {
		t.Fatalf("AddAttestation: %v", err)
	}
	if len(out.Attestations()) != 1 {
		t.Fatalf("got %d attestations, want 1", len(out.Attestations()))
	}
	if !containsAction(e.audit.Actions(), "merchant.attestation_added") {
		t.Fatalf("audit = %v, want an attestation_added record", e.audit.Actions())
	}

	// An attestation that expires before it was made is a data error the aggregate refuses.
	if _, err := e.svc.AddAttestation(context.Background(), AddAttestationCommand{
		TenantID: testTenant, MerchantID: m.ID(), Actor: Actor{ID: "u"},
		Attestation: dmerchant.ComplianceAttestation{
			Type: "TERMS_ACCEPTANCE", AttestedBy: "cfo@example",
			AttestedAt: testEpoch, ExpiresAt: testEpoch.Add(-time.Hour),
		},
	}); err == nil {
		t.Fatal("an attestation expiring before it was made was accepted")
	}
}

// TestAFailedMutationLeavesNoAuditRecord is the negative half of the atomicity claim: the audit
// record must not survive a transaction that rolled back.
func TestAFailedMutationLeavesNoAuditRecord(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	m := mustCreate(t, e)
	before := len(e.audit.Actions())

	// Ownership above 100% makes the aggregate refuse after the audit line would have been queued.
	_, _ = e.svc.AddPrincipal(context.Background(), AddPrincipalCommand{
		TenantID: testTenant, MerchantID: m.ID(), Actor: Actor{ID: "u"},
		Principal: dmerchant.Principal{ID: "p", LastName: "X", OwnershipPct: 101},
	})
	if got := len(e.audit.Actions()); got != before {
		t.Fatalf("a rolled-back mutation left %d extra audit records", got-before)
	}
}

func mustCreate(t *testing.T, e *env) *dmerchant.Merchant {
	t.Helper()
	m, err := e.svc.Create(context.Background(), createCmd())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return m
}

// activeMerchant walks a merchant all the way to ACTIVE through its own state machine, so the
// lifecycle tests exercise transitions the aggregate actually permits rather than a state forced
// into the store.
func activeMerchant(t *testing.T, e *env) *dmerchant.Merchant {
	t.Helper()
	m := mustCreate(t, e)
	clock := e.clock
	steps := []func() error{
		func() error { return m.StartValidation(clock) },
		func() error { return m.PassValidation(clock) },
		func() error {
			return m.ApproveKYC("kyc_1", testEpoch.AddDate(1, 0, 0), dmerchant.RiskStandard, clock)
		},
		func() error {
			return m.AddBankAccount(dmerchant.BankAccount{
				ID: "ba_1", Country: "DE", Currency: "EUR", HolderName: "Beispiel",
				AccountLast4: "1234", SecretRef: "secret://x", Status: dmerchant.BankUnverified,
			}, clock)
		},
		func() error { return m.ValidateBankAccount("ba_1", "val_1", clock) },
		func() error { return m.StartProvisioning(clock) },
		func() error { return m.CompleteProvisioning([]string{"gw-a"}, clock) },
		func() error { return m.ApplyConfiguration(1, clock) },
		func() error { return m.StartCertification(clock) },
		func() error {
			return m.AddAttestation(dmerchant.ComplianceAttestation{
				Type: "PCI_SAQ", AttestedBy: "cfo", AttestedAt: testEpoch,
				ExpiresAt: testEpoch.AddDate(1, 0, 0),
			}, clock)
		},
		func() error {
			return m.AddAttestation(dmerchant.ComplianceAttestation{
				Type: "TERMS_ACCEPTANCE", AttestedBy: "cfo", AttestedAt: testEpoch,
				ExpiresAt: testEpoch.AddDate(1, 0, 0),
			}, clock)
		},
		func() error { return m.Approve("crt_1", "usr_1", clock) },
		func() error { return m.MarkProductionReady(clock) },
		func() error { return m.Activate(clock) },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("lifecycle step %d: %v", i, err)
		}
	}
	e.store.PutMerchant(m)
	return m
}

func openPayment(t *testing.T, e *env, id shared.MerchantID) *dpayment.Payment {
	t.Helper()
	p, err := dpayment.New(dpayment.NewPaymentParams{
		TenantID: testTenant, MerchantID: id, Amount: money.MustNew(1000, "EUR"),
		PaymentMethod:  shared.MethodCard,
		MethodRef:      dpayment.PaymentMethodReference{Token: "tok"},
		IdempotencyKey: "k",
	}, e.clock)
	if err != nil {
		t.Fatalf("payment.New: %v", err)
	}
	return p
}

func containsAction(all []string, want string) bool {
	for _, a := range all {
		if a == want {
			return true
		}
	}
	return false
}
