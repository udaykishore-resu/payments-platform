package merchant

import (
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// testEpoch is the instant every clock in this package's tests starts at. Nothing here reads the
// wall clock: rule 5 of docs/spec/06-code-conventions.md.
var testEpoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

const (
	oneYear  = 365 * 24 * time.Hour
	testKYC  = "kyc_case_1"
	testCert = "crt_report_1"
)

func testClock() *shared.FixedClock { return &shared.FixedClock{T: testEpoch} }

// testParams builds a valid registration. Each test takes its own copy and mutates it, so there
// is no shared mutable fixture a parallel sibling can corrupt.
func testParams() NewParams {
	return NewParams{
		TenantID:    shared.NewTenantID(),
		LegalName:   "Acme Widgets Limited",
		DisplayName: "Acme",
		ExternalRef: "erp-4471",
		Environment: shared.EnvironmentProduction,
		Profile: BusinessProfile{
			LegalEntityType: "LTD", RegistrationNumber: "09876543", TaxIDLast4: "6789",
			Country: "GB", AddressLine1: "1 High Street", City: "London", PostalCode: "EC1A 1BB",
			WebsiteURL: "https://acme.example", SupportEmail: "help@acme.example",
			MCC: "5411", Description: "grocery",
			ExpectedMonthlyVolume: money.MustNew(500_000_00, "GBP"),
			ExpectedAverageTicket: money.MustNew(45_00, "GBP"),
		},
	}
}

func testBankAccount() BankAccount {
	return BankAccount{
		ID: "ba_1", Country: "GB", Currency: money.Currency("GBP"),
		HolderName: "Acme Widgets Limited", AccountLast4: "1234", RoutingLast4: "5678",
		SecretRef: "secret://production/merchants/acme/bank/ba_1",
	}
}

func testAttestation(kind string, at time.Time) ComplianceAttestation {
	return ComplianceAttestation{
		Type: kind, Reference: kind + "-2026", AttestedBy: "compliance@acme.example",
		AttestedAt: at, ExpiresAt: at.Add(oneYear), DocumentID: "doc_" + kind,
	}
}

func newTestMerchant(t *testing.T) (*Merchant, *shared.FixedClock) {
	t.Helper()
	clk := testClock()
	m, err := New(testParams(), clk)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.DrainEvents()
	return m, clk
}

// productionReadyParams describes a merchant that has passed every gate and is one Activate call
// away from taking money. Tests knock out one precondition at a time.
func productionReadyParams() RehydrateParams {
	kycExpiry := testEpoch.Add(oneYear)
	completed := testEpoch.Add(-24 * time.Hour)
	verified := testEpoch.Add(-time.Hour)
	acct := testBankAccount()
	acct.Status = BankVerified
	acct.IsDefault = true
	acct.ValidatedAt = &verified
	return RehydrateParams{
		ID: shared.NewMerchantID(), TenantID: shared.NewTenantID(),
		LegalName: "Acme Widgets Limited", DisplayName: "Acme", ExternalRef: "erp-4471",
		Status: StatusProductionReady, Version: 11, Environment: shared.EnvironmentProduction,
		Profile:      testParams().Profile,
		BankAccounts: []BankAccount{acct},
		Principals: []Principal{
			{ID: "pr_1", Role: RoleBeneficialOwner, FirstName: "Ada", LastName: "Lovelace",
				OwnershipPct: 100, Country: "GB", Verified: true},
		},
		Attestations: []ComplianceAttestation{
			testAttestation("PCI_SAQ", testEpoch.Add(-30*24*time.Hour)),
			testAttestation("TERMS_ACCEPTANCE", testEpoch.Add(-30*24*time.Hour)),
		},
		KYCStatus: KYCApproved, KYCProviderRef: testKYC,
		KYCCompletedAt: &completed, KYCExpiresAt: &kycExpiry, RiskRating: RiskStandard,
		CertificationID: testCert, ActiveConfigVersion: 3,
		CreatedAt: testEpoch.Add(-90 * 24 * time.Hour), UpdatedAt: testEpoch.Add(-time.Hour),
	}
}

func TestNewMerchantValidation(t *testing.T) {
	// Verifies: FR-09 — the checks that are true of every merchant in every tenant. Tenant policy
	// (which MCCs and countries *this* tenant may onboard) is an L2 validation-plane concern and
	// is deliberately not hard-coded here.
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(*NewParams)
		wantCode apierror.Code
		check    func(*testing.T, *Merchant)
	}{
		{name: "valid", mutate: func(*NewParams) {}},
		{
			name:     "no tenant",
			mutate:   func(p *NewParams) { p.TenantID = "" },
			wantCode: apierror.CodeMissingTenantContext,
		},
		{
			name:     "no legal name",
			mutate:   func(p *NewParams) { p.LegalName = "   " },
			wantCode: apierror.CodeValidationFailed,
		},
		{
			name:     "legal name too long",
			mutate:   func(p *NewParams) { p.LegalName = string(make([]byte, 257)) },
			wantCode: apierror.CodeValidationFailed,
		},
		{
			// A merchant exists in exactly one environment. Mixing them is the failure mode where
			// a certification run charges a real card.
			name:     "unknown environment",
			mutate:   func(p *NewParams) { p.Environment = "staging" },
			wantCode: apierror.CodeValidationFailed,
		},
		{
			name:     "no environment",
			mutate:   func(p *NewParams) { p.Environment = "" },
			wantCode: apierror.CodeValidationFailed,
		},
		{
			name:     "invalid country",
			mutate:   func(p *NewParams) { p.Profile.Country = "XX" },
			wantCode: apierror.CodeValidationFailed,
		},
		{
			name:     "no country",
			mutate:   func(p *NewParams) { p.Profile.Country = "" },
			wantCode: apierror.CodeValidationFailed,
		},
		{
			// A licensing constraint, not a technical one: acquiring relationships forbid these
			// categories, and processing them puts the platform's own registrations at risk.
			name:     "prohibited merchant category",
			mutate:   func(p *NewParams) { p.Profile.MCC = "7995" },
			wantCode: apierror.CodeValidationFailed,
		},
		{
			name:   "the display name falls back to the legal name",
			mutate: func(p *NewParams) { p.DisplayName = "  " },
			check: func(t *testing.T, m *Merchant) {
				if m.DisplayName() != "Acme Widgets Limited" {
					t.Fatalf("displayName = %q", m.DisplayName())
				}
			},
		},
		{
			name:   "names and references are trimmed",
			mutate: func(p *NewParams) { p.LegalName = "  Acme Widgets Limited  "; p.ExternalRef = "  erp-1  " },
			check: func(t *testing.T, m *Merchant) {
				if m.LegalName() != "Acme Widgets Limited" || m.ExternalRef() != "erp-1" {
					t.Fatalf("legal = %q external = %q", m.LegalName(), m.ExternalRef())
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := testParams()
			tc.mutate(&params)
			m, err := New(params, testClock())
			if tc.wantCode != "" {
				if apierror.CodeOf(err) != tc.wantCode {
					t.Fatalf("code = %s, want %s (%v)", apierror.CodeOf(err), tc.wantCode, err)
				}
				if m != nil {
					t.Fatal("a rejected registration returned a merchant")
				}
				return
			}
			if err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if m.Status() != StatusCreated || m.Version() != 1 {
				t.Fatalf("status = %s version = %d", m.Status(), m.Version())
			}
			// A brand new merchant has done nothing: no verification, no certification, no
			// configuration, and a risk band that assumes nothing.
			if m.KYCStatus() != KYCNotStarted || m.RiskRating() != RiskStandard {
				t.Fatalf("kyc = %s rating = %s", m.KYCStatus(), m.RiskRating())
			}
			if m.CertificationID() != "" || m.ActiveConfigVersion() != 0 || m.ActivatedAt() != nil {
				t.Fatal("a new merchant carries certification, configuration or activation state")
			}
			if m.CreatedAt() != testEpoch || m.UpdatedAt() != testEpoch {
				t.Fatalf("timestamps = %s / %s", m.CreatedAt(), m.UpdatedAt())
			}
			if ok, err := m.CanAcceptPayments(testEpoch); ok || apierror.CodeOf(err) != apierror.CodeMerchantNotActive {
				t.Fatalf("a new merchant can accept payments: %v / %v", ok, err)
			}
			evts := m.PendingEvents()
			if len(evts) != 1 || evts[0].Type != EventMerchantCreated {
				t.Fatalf("events = %+v", evts)
			}
			if evts[0].Version != 1 || evts[0].Status != StatusCreated {
				t.Fatalf("event = %+v", evts[0])
			}
			if evts[0].AggregateID() != m.ID().String() {
				t.Fatalf("partition key = %q, want the merchant id", evts[0].AggregateID())
			}
			if tc.check != nil {
				tc.check(t, m)
			}
		})
	}
}

func TestOnboardingHappyPath(t *testing.T) {
	// Verifies: FR-09, FR-13, docs/spec/00-design-baseline.md §8 and docs/onboarding.md. The
	// lifecycle end to end, asserting the event raised at each step, because a consumer that
	// never sees `certified` will not enable traffic and a consumer that sees `activated` first
	// will enable it too early.
	t.Parallel()

	m, clk := newTestMerchant(t)
	kycExpiry := testEpoch.Add(oneYear)

	steps := []struct {
		name      string
		run       func() error
		want      Status
		wantEvent EventType
		after     func(*testing.T)
	}{
		{
			name: "start validation", run: func() error { return m.StartValidation(clk) },
			want: StatusValidating,
			// No event: nothing outside the onboarding workflow can act on "we started checking".
		},
		{
			name: "pass validation", run: func() error { return m.PassValidation(clk) },
			want: StatusKYCPending, wantEvent: EventMerchantValidated,
			after: func(t *testing.T) {
				if m.KYCStatus() != KYCInProgress {
					t.Fatalf("kycStatus = %s, want IN_PROGRESS", m.KYCStatus())
				}
			},
		},
		{
			name: "approve kyc",
			run:  func() error { return m.ApproveKYC(testKYC, kycExpiry, RiskLow, clk) },
			want: StatusKYCApproved, wantEvent: EventMerchantKYCApproved,
			after: func(t *testing.T) {
				if m.KYCStatus() != KYCApproved || m.KYCProviderRef() != testKYC {
					t.Fatalf("kyc = %s / %q", m.KYCStatus(), m.KYCProviderRef())
				}
				if m.KYCExpiresAt() == nil || !m.KYCExpiresAt().Equal(kycExpiry) {
					t.Fatalf("kycExpiresAt = %v", m.KYCExpiresAt())
				}
				if m.KYCCompletedAt() == nil || !m.KYCCompletedAt().Equal(testEpoch) {
					t.Fatalf("kycCompletedAt = %v", m.KYCCompletedAt())
				}
				if m.RiskRating() != RiskLow {
					t.Fatalf("riskRating = %s", m.RiskRating())
				}
			},
		},
		{
			name: "add a settlement account",
			run:  func() error { return m.AddBankAccount(testBankAccount(), clk) },
			want: StatusKYCApproved,
			after: func(t *testing.T) {
				// The first account added becomes the default, but an unverified account is not a
				// settlement destination.
				if !m.BankAccounts()[0].IsDefault || m.BankAccounts()[0].Status != BankUnverified {
					t.Fatalf("account = %+v", m.BankAccounts()[0])
				}
				if m.DefaultBankAccount() != nil {
					t.Fatal("an unverified account was offered as the default settlement destination")
				}
			},
		},
		{
			name: "validate the settlement account",
			run:  func() error { return m.ValidateBankAccount("ba_1", "vr_1", clk) },
			want: StatusBankValidated, wantEvent: EventMerchantBankValidated,
			after: func(t *testing.T) {
				if m.DefaultBankAccount() == nil || m.DefaultBankAccount().ValidationRef != "vr_1" {
					t.Fatalf("default account = %+v", m.DefaultBankAccount())
				}
			},
		},
		{
			name: "start provisioning", run: func() error { return m.StartProvisioning(clk) },
			want: StatusGatewayProvisioning,
		},
		{
			name: "complete provisioning",
			run:  func() error { return m.CompleteProvisioning([]string{"stripe", "adyen"}, clk) },
			want: StatusConfiguring, wantEvent: EventMerchantGatewayProvisioned,
		},
		{
			name: "apply configuration", run: func() error { return m.ApplyConfiguration(3, clk) },
			want: StatusSandboxValidation,
			after: func(t *testing.T) {
				if m.ActiveConfigVersion() != 3 {
					t.Fatalf("activeConfigVersion = %d", m.ActiveConfigVersion())
				}
			},
		},
		{
			name: "start certification", run: func() error { return m.StartCertification(clk) },
			want: StatusCertification,
		},
		{
			name: "approve", run: func() error { return m.Approve(testCert, "officer@platform", clk) },
			want: StatusApproved, wantEvent: EventMerchantCertified,
			after: func(t *testing.T) {
				if m.CertificationID() != testCert {
					t.Fatalf("certificationId = %q", m.CertificationID())
				}
			},
		},
		{
			name: "mark production ready", run: func() error { return m.MarkProductionReady(clk) },
			want: StatusProductionReady,
		},
		{
			name: "record the PCI attestation",
			run:  func() error { return m.AddAttestation(testAttestation("PCI_SAQ", testEpoch), clk) },
			want: StatusProductionReady,
		},
		{
			name: "record the terms attestation",
			run:  func() error { return m.AddAttestation(testAttestation("TERMS_ACCEPTANCE", testEpoch), clk) },
			want: StatusProductionReady,
		},
		{
			name: "activate", run: func() error { return m.Activate(clk) },
			want: StatusActive, wantEvent: EventMerchantActivated,
			after: func(t *testing.T) {
				if m.ActivatedAt() == nil || !m.ActivatedAt().Equal(testEpoch) {
					t.Fatalf("activatedAt = %v", m.ActivatedAt())
				}
			},
		},
	}

	version := m.Version()
	for _, step := range steps {
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if m.Status() != step.want {
			t.Fatalf("%s: status = %s, want %s", step.name, m.Status(), step.want)
		}
		// Every step is a state change, so every step advances the optimistic-concurrency version.
		if m.Version() <= version {
			t.Fatalf("%s: version did not advance past %d", step.name, version)
		}
		version = m.Version()

		evts := m.DrainEvents()
		if step.wantEvent == "" {
			if len(evts) != 0 {
				t.Fatalf("%s raised %+v, want no event", step.name, evts)
			}
		} else {
			if len(evts) != 1 || evts[0].Type != step.wantEvent {
				t.Fatalf("%s: events = %+v, want one %s", step.name, evts, step.wantEvent)
			}
			if evts[0].Status != m.Status() || evts[0].Version != m.Version() {
				t.Fatalf("%s: event carries status %s version %d, aggregate is %s / %d",
					step.name, evts[0].Status, evts[0].Version, m.Status(), m.Version())
			}
			if !evts[0].OccurredAt.Equal(testEpoch) {
				t.Fatalf("%s: event occurredAt = %s", step.name, evts[0].OccurredAt)
			}
		}
		if step.after != nil {
			step.after(t)
		}
	}

	ok, err := m.CanAcceptPayments(testEpoch)
	if !ok || err != nil {
		t.Fatalf("an activated merchant cannot accept payments: %v / %v", ok, err)
	}
	if !m.Status().CanIssueRefunds() || m.Status().IsOnboarding() {
		t.Fatal("an active merchant is still onboarding, or cannot refund")
	}
}

func TestOnboardingFailurePathsAreCorrectable(t *testing.T) {
	// Verifies: docs/state-machines.md §2.1 #5, #9, #13, #22, #28. A failed state is not a dead
	// end; forcing a merchant to start a new record loses the audit trail of what was wrong.
	t.Parallel()

	t.Run("validation failure and resubmission", func(t *testing.T) {
		t.Parallel()
		m, clk := newTestMerchant(t)
		if err := m.StartValidation(clk); err != nil {
			t.Fatalf("StartValidation: %v", err)
		}
		if err := m.FailValidation("registration number does not match Companies House", clk); err != nil {
			t.Fatalf("FailValidation: %v", err)
		}
		if m.Status() != StatusValidationFailed || !m.Status().IsFailureState() {
			t.Fatalf("status = %s", m.Status())
		}
		if m.StatusReason() == "" {
			t.Fatal("the failure reason was not recorded")
		}
		evts := m.DrainEvents()
		if len(evts) != 1 || evts[0].Type != EventMerchantValidationFailed {
			t.Fatalf("events = %+v", evts)
		}
		if err := m.StartValidation(clk); err != nil {
			t.Fatalf("re-validation after a correction: %v", err)
		}
	})

	t.Run("kyc rejection and resubmission", func(t *testing.T) {
		t.Parallel()
		m, clk := newTestMerchant(t)
		if err := m.StartValidation(clk); err != nil {
			t.Fatalf("StartValidation: %v", err)
		}
		if err := m.PassValidation(clk); err != nil {
			t.Fatalf("PassValidation: %v", err)
		}
		m.DrainEvents()
		if err := m.RejectKYC(testKYC, "identity documents unreadable", clk); err != nil {
			t.Fatalf("RejectKYC: %v", err)
		}
		if m.Status() != StatusKYCFailed || m.KYCStatus() != KYCRejected {
			t.Fatalf("status = %s kyc = %s", m.Status(), m.KYCStatus())
		}
		evts := m.DrainEvents()
		if len(evts) != 1 || evts[0].Type != EventMerchantKYCFailed {
			t.Fatalf("events = %+v", evts)
		}
		// A rejection cannot be overwritten by an approval in place: the approval must come from
		// a new vendor decision, which means passing through KYC_PENDING again.
		if err := m.ApproveKYC(testKYC, testEpoch.Add(oneYear), RiskLow, clk); apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
			t.Fatalf("KYC_FAILED → KYC_APPROVED: code = %s", apierror.CodeOf(err))
		}
		if err := m.ResubmitKYC(clk); err != nil {
			t.Fatalf("ResubmitKYC: %v", err)
		}
		if m.Status() != StatusKYCPending || m.KYCStatus() != KYCInProgress {
			t.Fatalf("status = %s kyc = %s", m.Status(), m.KYCStatus())
		}
		if err := m.ApproveKYC("kyc_case_2", testEpoch.Add(oneYear), RiskElevated, clk); err != nil {
			t.Fatalf("ApproveKYC after resubmission: %v", err)
		}
	})

	t.Run("certification failure routes back to certification or configuration", func(t *testing.T) {
		t.Parallel()
		m := mustRehydrate(t, func(p *RehydrateParams) {
			p.Status = StatusCertification
			p.CertificationID = ""
		})
		clk := testClock()
		if err := m.FailCertification("3DS challenge assertion failed on adyen/EUR", clk); err != nil {
			t.Fatalf("FailCertification: %v", err)
		}
		if m.Status() != StatusCertificationFailed {
			t.Fatalf("status = %s", m.Status())
		}
		evts := m.DrainEvents()
		if len(evts) != 1 || evts[0].Type != EventMerchantCertificationFailed {
			t.Fatalf("events = %+v", evts)
		}
		if err := m.StartCertification(clk); err != nil {
			t.Fatalf("re-running certification: %v", err)
		}
	})

	t.Run("configuration failure and retry", func(t *testing.T) {
		t.Parallel()
		m := mustRehydrate(t, func(p *RehydrateParams) { p.Status = StatusConfiguring })
		clk := testClock()
		if err := m.FailConfiguration("routing rule references an unprovisioned gateway", clk); err != nil {
			t.Fatalf("FailConfiguration: %v", err)
		}
		evts := m.DrainEvents()
		if len(evts) != 1 || evts[0].Type != EventMerchantConfigurationFailed {
			t.Fatalf("events = %+v", evts)
		}
		if err := m.ApplyConfiguration(4, clk); err == nil {
			t.Fatal("CONFIGURATION_FAILED → SANDBOX_VALIDATION was permitted; the retry must go via CONFIGURING")
		}
	})

	t.Run("provisioning failure and retry", func(t *testing.T) {
		t.Parallel()
		m := mustRehydrate(t, func(p *RehydrateParams) { p.Status = StatusGatewayProvisioning })
		clk := testClock()
		if err := m.FailProvisioning("stripe sub-account creation exhausted its retries", clk); err != nil {
			t.Fatalf("FailProvisioning: %v", err)
		}
		evts := m.DrainEvents()
		if len(evts) != 1 || evts[0].Type != EventMerchantProvisioningFailed {
			t.Fatalf("events = %+v", evts)
		}
		if err := m.StartProvisioning(clk); err != nil {
			t.Fatalf("retrying provisioning: %v", err)
		}
	})
}

func TestLifecycleMethodsRefuseFromTheWrongState(t *testing.T) {
	// Verifies: rule 2 of docs/spec/06-code-conventions.md — every lifecycle method consults the
	// transition table first. A method that mutates its own fields before checking would leave a
	// merchant carrying evidence of a step it never took: a certification id on a merchant that
	// was never certified is exactly what an operator would later point at to force an activation.
	t.Parallel()

	tests := []struct {
		name  string
		from  Status
		run   func(*Merchant, *shared.FixedClock) error
		check func(*testing.T, *Merchant)
	}{
		{
			name: "PassValidation outside VALIDATING", from: StatusActive,
			run: func(m *Merchant, c *shared.FixedClock) error { return m.PassValidation(c) },
			check: func(t *testing.T, m *Merchant) {
				if m.KYCStatus() != KYCApproved {
					t.Fatalf("a refused PassValidation reset kycStatus to %s", m.KYCStatus())
				}
			},
		},
		{
			name: "RejectKYC outside KYC_PENDING", from: StatusActive,
			run: func(m *Merchant, c *shared.FixedClock) error { return m.RejectKYC("kyc_2", "no", c) },
			check: func(t *testing.T, m *Merchant) {
				if m.KYCStatus() != KYCApproved || m.KYCProviderRef() != testKYC {
					t.Fatalf("a refused RejectKYC changed verification: %s / %q",
						m.KYCStatus(), m.KYCProviderRef())
				}
			},
		},
		{
			name: "ResubmitKYC outside KYC_FAILED or COMPLIANCE_REJECTED", from: StatusActive,
			run: func(m *Merchant, c *shared.FixedClock) error { return m.ResubmitKYC(c) },
			check: func(t *testing.T, m *Merchant) {
				if m.KYCStatus() != KYCApproved {
					t.Fatalf("a refused ResubmitKYC reset kycStatus to %s", m.KYCStatus())
				}
			},
		},
		{
			name: "Approve outside CERTIFICATION", from: StatusConfiguring,
			run: func(m *Merchant, c *shared.FixedClock) error { return m.Approve("crt_forged", "operator", c) },
			check: func(t *testing.T, m *Merchant) {
				if m.CertificationID() == "crt_forged" {
					t.Fatal("a refused Approve still attached a certification report")
				}
			},
		},
		{
			name: "MarkProductionReady outside APPROVED", from: StatusActive,
			run: func(m *Merchant, c *shared.FixedClock) error { return m.MarkProductionReady(c) },
		},
		{
			name: "StartValidation outside CREATED", from: StatusActive,
			run: func(m *Merchant, c *shared.FixedClock) error { return m.StartValidation(c) },
		},
		{
			name: "StartProvisioning outside BANK_VALIDATED", from: StatusActive,
			run: func(m *Merchant, c *shared.FixedClock) error { return m.StartProvisioning(c) },
		},
		{
			name: "CompleteProvisioning outside GATEWAY_PROVISIONING", from: StatusActive,
			run: func(m *Merchant, c *shared.FixedClock) error { return m.CompleteProvisioning([]string{"stripe"}, c) },
		},
		{
			name: "ApplyConfiguration outside CONFIGURING", from: StatusActive,
			run: func(m *Merchant, c *shared.FixedClock) error { return m.ApplyConfiguration(4, c) },
			check: func(t *testing.T, m *Merchant) {
				if m.ActiveConfigVersion() != 3 {
					t.Fatalf("a refused ApplyConfiguration set the version to %d", m.ActiveConfigVersion())
				}
			},
		},
		{
			name: "StartCertification outside SANDBOX_VALIDATION", from: StatusActive,
			run: func(m *Merchant, c *shared.FixedClock) error { return m.StartCertification(c) },
		},
		{
			name: "FailCertification outside CERTIFICATION", from: StatusActive,
			run: func(m *Merchant, c *shared.FixedClock) error { return m.FailCertification("no", c) },
		},
		{
			name: "RejectForCompliance outside CERTIFICATION", from: StatusActive,
			run: func(m *Merchant, c *shared.FixedClock) error { return m.RejectForCompliance("R", "d", "o", c) },
		},
		{
			name: "FailValidation outside VALIDATING", from: StatusActive,
			run: func(m *Merchant, c *shared.FixedClock) error { return m.FailValidation("no", c) },
		},
		{
			name: "FailProvisioning outside GATEWAY_PROVISIONING", from: StatusActive,
			run: func(m *Merchant, c *shared.FixedClock) error { return m.FailProvisioning("no", c) },
		},
		{
			name: "FailConfiguration outside CONFIGURING", from: StatusActive,
			run: func(m *Merchant, c *shared.FixedClock) error { return m.FailConfiguration("no", c) },
		},
		{
			name: "FailBankValidation outside KYC_APPROVED", from: StatusActive,
			run: func(m *Merchant, c *shared.FixedClock) error { return m.FailBankValidation("ba_1", "no", c) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := mustRehydrate(t, func(p *RehydrateParams) { p.Status = tc.from })
			version := m.Version()
			clk := testClock()

			err := tc.run(m, clk)
			if apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
				t.Fatalf("code = %s, want INVALID_STATE_TRANSITION (%v)", apierror.CodeOf(err), err)
			}
			if m.Status() != tc.from {
				t.Fatalf("a refused call moved the merchant from %s to %s", tc.from, m.Status())
			}
			if m.Version() != version {
				t.Fatalf("a refused call advanced the version to %d", m.Version())
			}
			if len(m.PendingEvents()) != 0 {
				t.Fatalf("a refused call raised %+v", m.PendingEvents())
			}
			if tc.check != nil {
				tc.check(t, m)
			}
		})
	}
}

func TestApproveKYCRequiresAProviderReferenceAndAFutureExpiry(t *testing.T) {
	// Verifies: FR-13. Verification that never expires is not verification; every jurisdiction
	// the platform operates in requires periodic re-verification, and an attestation with no end
	// date will eventually be presented at audit as evidence of a control that does not exist.
	t.Parallel()

	tests := []struct {
		name     string
		ref      string
		expires  time.Time
		rating   RiskRating
		wantCode apierror.Code
		check    func(*testing.T, *Merchant)
	}{
		{
			name: "no provider reference", ref: "", expires: testEpoch.Add(oneYear),
			rating: RiskLow, wantCode: apierror.CodeValidationFailed,
		},
		{
			name: "expiry in the past", ref: testKYC, expires: testEpoch.Add(-time.Second),
			rating: RiskLow, wantCode: apierror.CodeValidationFailed,
		},
		{
			// Exactly now is not in the future: a verification that expires at the instant it is
			// recorded is not a verification.
			name: "expiry exactly now", ref: testKYC, expires: testEpoch,
			rating: RiskLow, wantCode: apierror.CodeValidationFailed,
		},
		{
			name: "zero expiry", ref: testKYC, expires: time.Time{},
			rating: RiskLow, wantCode: apierror.CodeValidationFailed,
		},
		{
			name: "one second in the future is enough", ref: testKYC, expires: testEpoch.Add(time.Second),
			rating: RiskHigh,
			check: func(t *testing.T, m *Merchant) {
				if m.RiskRating() != RiskHigh {
					t.Fatalf("riskRating = %s", m.RiskRating())
				}
			},
		},
		{
			// An unknown rating is coerced to STANDARD rather than rejected: refusing a KYC
			// approval over a provider's unmapped band would strand the merchant mid-onboarding
			// over a field that only tunes limits.
			name: "an unknown risk rating falls back to STANDARD", ref: testKYC,
			expires: testEpoch.Add(oneYear), rating: "SPICY",
			check: func(t *testing.T, m *Merchant) {
				if m.RiskRating() != RiskStandard {
					t.Fatalf("riskRating = %s, want STANDARD", m.RiskRating())
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, clk := newTestMerchant(t)
			if err := m.StartValidation(clk); err != nil {
				t.Fatalf("StartValidation: %v", err)
			}
			if err := m.PassValidation(clk); err != nil {
				t.Fatalf("PassValidation: %v", err)
			}
			m.DrainEvents()

			err := m.ApproveKYC(tc.ref, tc.expires, tc.rating, clk)
			if tc.wantCode != "" {
				if apierror.CodeOf(err) != tc.wantCode {
					t.Fatalf("code = %s, want %s (%v)", apierror.CodeOf(err), tc.wantCode, err)
				}
				// A refused approval leaves nothing behind: no status change, no provider
				// reference, no expiry a later activation could pass its guard against.
				if m.Status() != StatusKYCPending || m.KYCStatus() != KYCInProgress {
					t.Fatalf("status = %s kyc = %s", m.Status(), m.KYCStatus())
				}
				if m.KYCProviderRef() != "" || m.KYCExpiresAt() != nil || m.KYCCompletedAt() != nil {
					t.Fatal("a refused approval recorded verification state")
				}
				if len(m.PendingEvents()) != 0 {
					t.Fatal("a refused approval raised an event")
				}
				return
			}
			if err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if m.Status() != StatusKYCApproved {
				t.Fatalf("status = %s", m.Status())
			}
			if tc.check != nil {
				tc.check(t, m)
			}
		})
	}
}

func TestActivateReChecksEveryPrecondition(t *testing.T) {
	// Verifies: docs/spec/00-design-baseline.md §8 ("→ ACTIVE requires…"), docs/state-machines.md
	// §2.1 #32.
	//
	// Activate is the gate before real money moves and it is the one an operator will try to
	// force manually when an onboarding is running late. It therefore re-checks every precondition
	// rather than trusting that the workflow reached PRODUCTION_READY legitimately — a workflow
	// with a bug, or a manual status update in the database, must not be able to buy an activation.
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(*RehydrateParams)
		wantCode apierror.Code
		why      string
	}{
		{
			name:   "every precondition satisfied",
			mutate: func(*RehydrateParams) {},
		},
		{
			name:     "verification is not approved",
			mutate:   func(p *RehydrateParams) { p.KYCStatus = KYCReviewNeeded },
			wantCode: apierror.CodeKYCRequired,
			why:      "processing for an unverified business is the regulatory incident this whole pipeline exists to prevent",
		},
		{
			name:     "verification has no expiry at all",
			mutate:   func(p *RehydrateParams) { p.KYCExpiresAt = nil },
			wantCode: apierror.CodeKYCRequired,
			why:      "a verification with no end date is indistinguishable at audit from a control that does not exist",
		},
		{
			name: "verification has already expired",
			mutate: func(p *RehydrateParams) {
				past := testEpoch.Add(-time.Hour)
				p.KYCExpiresAt = &past
			},
			wantCode: apierror.CodeKYCRequired,
			why:      "an expired verification is a missing one",
		},
		{
			name: "verification expires exactly now",
			mutate: func(p *RehydrateParams) {
				now := testEpoch
				p.KYCExpiresAt = &now
			},
			wantCode: apierror.CodeKYCRequired,
			why:      "the boundary is exclusive; a deadline that has been reached has passed",
		},
		{
			name:     "no settlement account at all",
			mutate:   func(p *RehydrateParams) { p.BankAccounts = nil },
			wantCode: apierror.CodeValidationFailed,
			why:      "there is nowhere to send the money",
		},
		{
			name: "a settlement account that was never verified",
			mutate: func(p *RehydrateParams) {
				p.BankAccounts[0].Status = BankPendingVerify
			},
			wantCode: apierror.CodeValidationFailed,
			why:      "an unverified account is how a typo becomes a settlement to a stranger",
		},
		{
			name: "a settlement account whose verification failed",
			mutate: func(p *RehydrateParams) {
				p.BankAccounts[0].Status = BankVerificationFail
			},
			wantCode: apierror.CodeValidationFailed,
		},
		{
			name:     "no certification report on file",
			mutate:   func(p *RehydrateParams) { p.CertificationID = "" },
			wantCode: apierror.CodeCertificationFailed,
			why:      "'certified' must be an artifact, not an opinion",
		},
		{
			name:     "no configuration version in force",
			mutate:   func(p *RehydrateParams) { p.ActiveConfigVersion = 0 },
			wantCode: apierror.CodeConfigurationInvalid,
			why:      "a merchant with no configuration has no currencies, no routing and no limits",
		},
		{
			name:     "no attestations at all",
			mutate:   func(p *RehydrateParams) { p.Attestations = nil },
			wantCode: apierror.CodeValidationFailed,
		},
		{
			name: "the PCI attestation is missing",
			mutate: func(p *RehydrateParams) {
				p.Attestations = []ComplianceAttestation{testAttestation("TERMS_ACCEPTANCE", testEpoch)}
			},
			wantCode: apierror.CodeValidationFailed,
		},
		{
			name: "the terms attestation is missing",
			mutate: func(p *RehydrateParams) {
				p.Attestations = []ComplianceAttestation{testAttestation("PCI_SAQ", testEpoch)}
			},
			wantCode: apierror.CodeValidationFailed,
		},
		{
			name: "the PCI attestation has expired",
			mutate: func(p *RehydrateParams) {
				stale := testAttestation("PCI_SAQ", testEpoch.Add(-2*oneYear))
				p.Attestations = []ComplianceAttestation{stale, testAttestation("TERMS_ACCEPTANCE", testEpoch)}
			},
			wantCode: apierror.CodeValidationFailed,
			why:      "a stale attestation is indistinguishable from a missing one at audit time",
		},
		{
			name: "the terms attestation expires exactly now",
			mutate: func(p *RehydrateParams) {
				expiring := testAttestation("TERMS_ACCEPTANCE", testEpoch.Add(-oneYear))
				p.Attestations = []ComplianceAttestation{testAttestation("PCI_SAQ", testEpoch), expiring}
			},
			wantCode: apierror.CodeValidationFailed,
			why:      "IsCurrent is strictly before the expiry",
		},
		{
			name:     "the merchant is not production ready",
			mutate:   func(p *RehydrateParams) { p.Status = StatusApproved },
			wantCode: apierror.CodeInvalidStateTransition,
			why:      "APPROVED → ACTIVE would skip the production-readiness gate",
		},
		{
			name:     "the merchant is only certified",
			mutate:   func(p *RehydrateParams) { p.Status = StatusCertification },
			wantCode: apierror.CodeInvalidStateTransition,
		},
		{
			name:     "the merchant was terminated",
			mutate:   func(p *RehydrateParams) { p.Status = StatusTerminated },
			wantCode: apierror.CodeInvalidStateTransition,
			why:      "termination released the credentials; resurrecting the record re-associates a live merchant with de-provisioned gateway accounts",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := mustRehydrate(t, tc.mutate)
			before := m.Status()
			clk := testClock()

			err := m.Activate(clk)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("expected activation, got %v", err)
				}
				if m.Status() != StatusActive || m.ActivatedAt() == nil {
					t.Fatalf("status = %s activatedAt = %v", m.Status(), m.ActivatedAt())
				}
				evts := m.DrainEvents()
				if len(evts) != 1 || evts[0].Type != EventMerchantActivated {
					t.Fatalf("events = %+v", evts)
				}
				if evts[0].Payload["configVersion"] != 3 || evts[0].Payload["certificationId"] != testCert {
					t.Fatalf("activation payload = %+v", evts[0].Payload)
				}
				return
			}
			if apierror.CodeOf(err) != tc.wantCode {
				t.Fatalf("code = %s, want %s (%v) — %s", apierror.CodeOf(err), tc.wantCode, err, tc.why)
			}
			// A refused activation must change nothing at all. An operator who retries after
			// being refused must be refused for the same reason, not find the merchant half-moved.
			if m.Status() != before {
				t.Fatalf("a refused activation moved the merchant from %s to %s", before, m.Status())
			}
			if m.ActivatedAt() != nil {
				t.Fatal("a refused activation stamped activatedAt")
			}
			if len(m.PendingEvents()) != 0 {
				t.Fatalf("a refused activation raised %+v", m.PendingEvents())
			}
		})
	}
}

func TestSuspendAndReinstate(t *testing.T) {
	// Verifies: FR-14, docs/state-machines.md §2.1 #34, #36.
	t.Parallel()

	t.Run("suspension stops payments but not refunds", func(t *testing.T) {
		t.Parallel()
		m := mustActive(t)
		clk := testClock()
		if err := m.Suspend(SuspendChargebackRate, "chargeback ratio 1.8% over two months", clk); err != nil {
			t.Fatalf("Suspend: %v", err)
		}
		if m.Status() != StatusSuspended || m.SuspensionReason() != SuspendChargebackRate {
			t.Fatalf("status = %s reason = %s", m.Status(), m.SuspensionReason())
		}
		if m.SuspendedAt() == nil || !m.SuspendedAt().Equal(testEpoch) {
			t.Fatalf("suspendedAt = %v", m.SuspendedAt())
		}
		if m.StatusReason() == "" {
			t.Fatal("the suspension detail was not recorded")
		}
		ok, err := m.CanAcceptPayments(testEpoch)
		if ok || apierror.CodeOf(err) != apierror.CodeMerchantSuspended {
			t.Fatalf("a suspended merchant can accept payments: %v / %v", ok, err)
		}
		// The asymmetry that matters: you must always be able to give money back.
		if !m.Status().CanIssueRefunds() {
			t.Fatal("a suspended merchant cannot issue refunds")
		}
		evts := m.DrainEvents()
		if len(evts) != 1 || evts[0].Type != EventMerchantSuspended {
			t.Fatalf("events = %+v", evts)
		}
		if !evts[0].Type.IsUrgentInvalidation() {
			t.Fatal("a suspension is not an urgent cache invalidation; the data plane would keep " +
				"processing for the whole staleness window")
		}
		if evts[0].Payload["reason"] != string(SuspendChargebackRate) {
			t.Fatalf("payload = %+v", evts[0].Payload)
		}
	})

	t.Run("a suspension requiring operator review will not lift automatically", func(t *testing.T) {
		t.Parallel()
		m := mustActive(t)
		clk := testClock()
		if err := m.Suspend(SuspendSanctionsHit, "screening hit on a beneficial owner", clk); err != nil {
			t.Fatalf("Suspend: %v", err)
		}
		// A sanctions suspension lifted by an automated job is a criminal exposure, not a bug.
		err := m.Reinstate(false, clk)
		if apierror.CodeOf(err) != apierror.CodeForbidden {
			t.Fatalf("code = %s, want FORBIDDEN (%v)", apierror.CodeOf(err), err)
		}
		assertRuleID(t, err, "suspensionReason", "REQUIRES_OPERATOR_REVIEW", "L2.SUSPENSION_LIFT_AUTHORITY")
		if m.Status() != StatusSuspended {
			t.Fatalf("a refused reinstatement moved the merchant to %s", m.Status())
		}

		if err := m.Reinstate(true, clk); err != nil {
			t.Fatalf("operator reinstatement: %v", err)
		}
		if m.Status() != StatusActive {
			t.Fatalf("status = %s", m.Status())
		}
		// The reason and the timestamp are cleared, or the next automated check would see a
		// suspension that is no longer in force.
		if m.SuspensionReason() != "" || m.SuspendedAt() != nil {
			t.Fatalf("reinstatement left %q / %v behind", m.SuspensionReason(), m.SuspendedAt())
		}
		evts := m.DrainEvents()
		last := evts[len(evts)-1]
		if last.Type != EventMerchantReinstated {
			t.Fatalf("events = %+v", evts)
		}
		if last.Payload["previousReason"] != string(SuspendSanctionsHit) {
			t.Fatalf("the reinstatement event does not name what was lifted: %+v", last.Payload)
		}
	})

	t.Run("a suspension that does not require review lifts for an automated caller", func(t *testing.T) {
		t.Parallel()
		m := mustActive(t)
		clk := testClock()
		if err := m.Suspend(SuspendRiskBreach, "velocity limit breached", clk); err != nil {
			t.Fatalf("Suspend: %v", err)
		}
		if err := m.Reinstate(false, clk); err != nil {
			t.Fatalf("automated reinstatement of a risk suspension: %v", err)
		}
		if m.Status() != StatusActive {
			t.Fatalf("status = %s", m.Status())
		}
	})

	t.Run("reinstating something that is not suspended", func(t *testing.T) {
		t.Parallel()
		m := mustActive(t)
		if err := m.Reinstate(true, testClock()); apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
			t.Fatalf("code = %s", apierror.CodeOf(err))
		}
	})

	t.Run("a merchant can be suspended before it ever went live", func(t *testing.T) {
		t.Parallel()
		// Amendment A-01's second half: an adverse finding between approval and activation must
		// be expressible without terminating the merchant.
		for _, from := range []Status{StatusApproved, StatusProductionReady} {
			m := mustRehydrate(t, func(p *RehydrateParams) { p.Status = from })
			if err := m.Suspend(SuspendOperatorAction, "adverse media", testClock()); err != nil {
				t.Fatalf("Suspend from %s: %v", from, err)
			}
			if m.Status() != StatusSuspended {
				t.Fatalf("status = %s", m.Status())
			}
		}
	})
}

func TestTerminate(t *testing.T) {
	// Verifies: FR-14, docs/state-machines.md §2.1 #35. The aggregate cannot see the payment
	// context, so the caller supplies the count; the rule lives here so that it is stated in the
	// domain rather than only in the use case.
	t.Parallel()

	t.Run("refused while payments are still in flight", func(t *testing.T) {
		t.Parallel()
		m := mustActive(t)
		clk := testClock()
		err := m.Terminate("contract ended", 4, clk)
		if apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
			t.Fatalf("code = %s (%v)", apierror.CodeOf(err), err)
		}
		assertRuleID(t, err, "status", "OPEN_PAYMENTS", "L7.TERMINATION_REQUIRES_NO_OPEN_PAYMENTS")
		// Termination revokes connections and permits data erasure. Doing that with an
		// authorization outstanding strands the payment and the payer's money with it.
		if m.Status() != StatusActive {
			t.Fatalf("a refused termination moved the merchant to %s", m.Status())
		}
		if len(m.PendingEvents()) != 0 {
			t.Fatal("a refused termination raised an event")
		}
	})

	t.Run("permitted once nothing is open", func(t *testing.T) {
		t.Parallel()
		m := mustActive(t)
		clk := testClock()
		if err := m.Terminate("contract ended", 0, clk); err != nil {
			t.Fatalf("Terminate: %v", err)
		}
		if m.Status() != StatusTerminated || !m.Status().IsTerminal() {
			t.Fatalf("status = %s", m.Status())
		}
		if m.Status().CanIssueRefunds() || m.Status().CanAcceptPayments() {
			t.Fatal("a terminated merchant can still move money")
		}
		evts := m.DrainEvents()
		if len(evts) != 1 || evts[0].Type != EventMerchantTerminated || !evts[0].Type.IsUrgentInvalidation() {
			t.Fatalf("events = %+v", evts)
		}
		// A returning merchant is a new merchant_id.
		if err := m.Reinstate(true, clk); err == nil {
			t.Fatal("a terminated merchant was reinstated")
		}
		if err := m.Suspend(SuspendOperatorAction, "x", clk); err == nil {
			t.Fatal("a terminated merchant was suspended")
		}
	})

	t.Run("abandoning a merchant that never started", func(t *testing.T) {
		t.Parallel()
		m, clk := newTestMerchant(t)
		if err := m.Terminate("never completed the application", 0, clk); err != nil {
			t.Fatalf("Terminate from CREATED: %v", err)
		}
		if m.Status() != StatusTerminated {
			t.Fatalf("status = %s", m.Status())
		}
	})
}

func TestRejectForComplianceRequiresAReasonCode(t *testing.T) {
	// Verifies: amendment A-01. The rejection is a policy decision, and a policy decision with no
	// recorded reason is unauditable and unappealable.
	t.Parallel()

	m := mustRehydrate(t, func(p *RehydrateParams) {
		p.Status = StatusCertification
		p.CertificationID = ""
	})
	clk := testClock()

	if err := m.RejectForCompliance("", "detail", "officer", clk); apierror.CodeOf(err) != apierror.CodeValidationFailed {
		t.Fatalf("no reason code: code = %s", apierror.CodeOf(err))
	}
	if m.Status() != StatusCertification {
		t.Fatalf("a refused rejection moved the merchant to %s", m.Status())
	}

	if err := m.RejectForCompliance("PROHIBITED_CORRIDOR", "MCC/country combination not permitted", "officer@platform", clk); err != nil {
		t.Fatalf("RejectForCompliance: %v", err)
	}
	if m.Status() != StatusComplianceRejected {
		t.Fatalf("status = %s", m.Status())
	}
	evts := m.DrainEvents()
	if len(evts) != 1 || evts[0].Type != EventMerchantComplianceRejected {
		t.Fatalf("events = %+v", evts)
	}
	if evts[0].Payload["reasonCode"] != "PROHIBITED_CORRIDOR" || evts[0].Payload["rejectedBy"] != "officer@platform" {
		t.Fatalf("payload = %+v", evts[0].Payload)
	}
	// The distinction A-01 exists to make: this is not a certification failure, and the
	// merchant cannot get out of it by re-running the certification suite.
	if err := m.StartCertification(clk); err == nil {
		t.Fatal("a compliance rejection was cleared by re-running certification")
	}
}

func TestAddPrincipal(t *testing.T) {
	// Verifies: FR-09. Beneficial ownership above 100% means the declaration is wrong, and a KYB
	// file built on a wrong declaration fails at audit rather than at onboarding.
	t.Parallel()

	t.Run("ownership may not total above 100%", func(t *testing.T) {
		t.Parallel()
		m, clk := newTestMerchant(t)
		if err := m.AddPrincipal(Principal{ID: "pr_1", LastName: "Lovelace", OwnershipPct: 60, Role: RoleBeneficialOwner}, clk); err != nil {
			t.Fatalf("AddPrincipal: %v", err)
		}
		// Exactly 100 is fine.
		if err := m.AddPrincipal(Principal{ID: "pr_2", LastName: "Babbage", OwnershipPct: 40, Role: RoleBeneficialOwner}, clk); err != nil {
			t.Fatalf("AddPrincipal to exactly 100%%: %v", err)
		}
		err := m.AddPrincipal(Principal{ID: "pr_3", LastName: "Hopper", OwnershipPct: 1, Role: RoleBeneficialOwner}, clk)
		if apierror.CodeOf(err) != apierror.CodeValidationFailed {
			t.Fatalf("code = %s (%v)", apierror.CodeOf(err), err)
		}
		assertRuleID(t, err, "principals", "OWNERSHIP_EXCEEDS_100", "L2.OWNERSHIP_SUMS_CORRECTLY")
		if len(m.Principals()) != 2 {
			t.Fatalf("%d principals recorded, want 2", len(m.Principals()))
		}
		// A director with no shareholding does not consume the budget.
		if err := m.AddPrincipal(Principal{ID: "pr_4", LastName: "Turing", OwnershipPct: 0, Role: RoleDirector}, clk); err != nil {
			t.Fatalf("adding a non-owning director: %v", err)
		}
	})

	t.Run("validation", func(t *testing.T) {
		t.Parallel()
		m, clk := newTestMerchant(t)
		tests := []struct {
			name string
			p    Principal
		}{
			{"no id", Principal{LastName: "Lovelace"}},
			{"no surname", Principal{ID: "pr_1"}},
			{"negative ownership", Principal{ID: "pr_1", LastName: "L", OwnershipPct: -1}},
			{"ownership above 100", Principal{ID: "pr_1", LastName: "L", OwnershipPct: 101}},
		}
		for _, tc := range tests {
			if err := m.AddPrincipal(tc.p, clk); apierror.CodeOf(err) != apierror.CodeValidationFailed {
				t.Errorf("%s: code = %s", tc.name, apierror.CodeOf(err))
			}
		}
		if len(m.Principals()) != 0 {
			t.Fatalf("a refused principal was recorded: %+v", m.Principals())
		}

		if err := m.AddPrincipal(Principal{ID: "pr_1", LastName: "Lovelace", OwnershipPct: 50}, clk); err != nil {
			t.Fatalf("AddPrincipal: %v", err)
		}
		if err := m.AddPrincipal(Principal{ID: "pr_1", LastName: "Someone Else", OwnershipPct: 10}, clk); apierror.CodeOf(err) != apierror.CodeValidationFailed {
			t.Fatal("a duplicate principal id was accepted")
		}
	})
}

func TestAddAttestationReplacesRatherThanAccumulates(t *testing.T) {
	// Verifies: the activation guard's evidence. The *current* attestation is what matters, and
	// history lives in the audit trail; a list that accumulates would let an expired PCI SAQ sit
	// alongside a current one and make "is there a current PCI_SAQ" depend on iteration order.
	t.Parallel()

	m, clk := newTestMerchant(t)

	first := testAttestation("PCI_SAQ", testEpoch.Add(-oneYear))
	if err := m.AddAttestation(first, clk); err != nil {
		t.Fatalf("AddAttestation: %v", err)
	}
	if err := m.AddAttestation(testAttestation("TERMS_ACCEPTANCE", testEpoch), clk); err != nil {
		t.Fatalf("AddAttestation: %v", err)
	}
	if len(m.Attestations()) != 2 {
		t.Fatalf("%d attestations, want 2", len(m.Attestations()))
	}

	renewed := testAttestation("PCI_SAQ", testEpoch)
	renewed.Reference = "PCI_SAQ-2027"
	if err := m.AddAttestation(renewed, clk); err != nil {
		t.Fatalf("renewing: %v", err)
	}
	if len(m.Attestations()) != 2 {
		t.Fatalf("%d attestations after a renewal, want 2", len(m.Attestations()))
	}
	found := false
	for _, a := range m.Attestations() {
		if a.Type == "PCI_SAQ" {
			if found {
				t.Fatal("two PCI_SAQ attestations are on file")
			}
			found = true
			if a.Reference != "PCI_SAQ-2027" || !a.ExpiresAt.Equal(renewed.ExpiresAt) {
				t.Fatalf("the stale attestation survived: %+v", a)
			}
		}
	}
	if !found {
		t.Fatal("the renewed attestation is not on file")
	}

	t.Run("validation", func(t *testing.T) {
		t.Parallel()
		bad, clk := newTestMerchant(t)
		cases := []ComplianceAttestation{
			{Type: "", AttestedBy: "x", AttestedAt: testEpoch, ExpiresAt: testEpoch.Add(oneYear)},
			{Type: "PCI_SAQ", AttestedBy: "", AttestedAt: testEpoch, ExpiresAt: testEpoch.Add(oneYear)},
			// Expiry is mandatory and must be after the attestation: an attestation without one
			// silently becomes stale, and a stale attestation is indistinguishable from a missing
			// one at audit time.
			{Type: "PCI_SAQ", AttestedBy: "x", AttestedAt: testEpoch, ExpiresAt: testEpoch},
			{Type: "PCI_SAQ", AttestedBy: "x", AttestedAt: testEpoch, ExpiresAt: testEpoch.Add(-time.Hour)},
			{Type: "PCI_SAQ", AttestedBy: "x", AttestedAt: testEpoch},
		}
		for i, a := range cases {
			if err := bad.AddAttestation(a, clk); apierror.CodeOf(err) != apierror.CodeValidationFailed {
				t.Errorf("case %d: code = %s", i, apierror.CodeOf(err))
			}
		}
		if len(bad.Attestations()) != 0 {
			t.Fatalf("a refused attestation was recorded: %+v", bad.Attestations())
		}
	})

	t.Run("IsCurrent boundary", func(t *testing.T) {
		t.Parallel()
		a := testAttestation("PCI_SAQ", testEpoch)
		if !a.IsCurrent(a.ExpiresAt.Add(-time.Nanosecond)) {
			t.Fatal("an attestation is not current one nanosecond before it expires")
		}
		// Exclusive: a deadline that has been reached has passed.
		if a.IsCurrent(a.ExpiresAt) {
			t.Fatal("an attestation is current at the instant it expires")
		}
		if a.IsCurrent(a.ExpiresAt.Add(time.Nanosecond)) {
			t.Fatal("an expired attestation is current")
		}
	})
}

func TestCanAcceptPaymentsCombinesStatusWithKYCFreshness(t *testing.T) {
	// Verifies: FR-13 — an ACTIVE merchant whose periodic re-verification lapsed must stop
	// processing even though nothing moved its status. Nothing in the lifecycle fires on its own
	// when a date passes, so the check has to be at the point of use.
	t.Parallel()

	expiry := testEpoch.Add(oneYear)

	tests := []struct {
		name     string
		at       time.Time
		want     bool
		wantCode apierror.Code
	}{
		{name: "well inside the window", at: testEpoch, want: true},
		{name: "one nanosecond before expiry", at: expiry.Add(-time.Nanosecond), want: true},
		// Exclusive boundary: the day the verification lapses, processing stops.
		{name: "exactly at expiry", at: expiry, wantCode: apierror.CodeKYCRequired},
		{name: "long after expiry", at: expiry.Add(30 * 24 * time.Hour), wantCode: apierror.CodeKYCRequired},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := mustActive(t)
			if m.Status() != StatusActive {
				t.Fatalf("status = %s", m.Status())
			}
			ok, err := m.CanAcceptPayments(tc.at)
			if ok != tc.want {
				t.Fatalf("CanAcceptPayments(%s) = %v, want %v (%v)", tc.at, ok, tc.want, err)
			}
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if apierror.CodeOf(err) != tc.wantCode {
				t.Fatalf("code = %s, want %s", apierror.CodeOf(err), tc.wantCode)
			}
			assertRuleID(t, err, "kycStatus", "EXPIRED", "L5.MERCHANT_KYC_CURRENT")
			// The status is untouched: this is a point-of-use refusal, not a lifecycle event.
			// Moving the merchant would require a decision nobody has taken.
			if m.Status() != StatusActive {
				t.Fatalf("the expiry check changed the status to %s", m.Status())
			}
		})
	}

	t.Run("a merchant with no recorded expiry is not blocked by the freshness check", func(t *testing.T) {
		t.Parallel()
		// Activate refuses this state, so it can only arise from a legacy row. Blocking every
		// such merchant at the point of use would take a whole book of business offline; the
		// activation guard is where the requirement is enforced.
		m := mustRehydrate(t, func(p *RehydrateParams) {
			p.Status = StatusActive
			p.KYCExpiresAt = nil
			p.ActivatedAt = &testEpoch
		})
		if ok, err := m.CanAcceptPayments(testEpoch.Add(10 * oneYear)); !ok || err != nil {
			t.Fatalf("CanAcceptPayments = %v / %v", ok, err)
		}
	})
}

func TestBankAccountsAndConfiguration(t *testing.T) {
	t.Parallel()

	t.Run("account validation", func(t *testing.T) {
		t.Parallel()
		m, clk := newTestMerchant(t)
		bad := testBankAccount()
		bad.ID = ""
		if apierror.CodeOf(m.AddBankAccount(bad, clk)) != apierror.CodeValidationFailed {
			t.Fatal("an account with no identifier was accepted")
		}
		bad = testBankAccount()
		bad.Country = "XX"
		if apierror.CodeOf(m.AddBankAccount(bad, clk)) != apierror.CodeValidationFailed {
			t.Fatal("an account with an invalid country was accepted")
		}
		bad = testBankAccount()
		bad.Currency = "XYZ"
		if apierror.CodeOf(m.AddBankAccount(bad, clk)) != apierror.CodeCurrencyNotSupported {
			t.Fatal("an account in an unsupported settlement currency was accepted")
		}
		if err := m.AddBankAccount(testBankAccount(), clk); err != nil {
			t.Fatalf("AddBankAccount: %v", err)
		}
		if apierror.CodeOf(m.AddBankAccount(testBankAccount(), clk)) != apierror.CodeValidationFailed {
			t.Fatal("a duplicate account id was accepted")
		}
		if err := m.ValidateBankAccount("ba_missing", "vr", clk); apierror.CodeOf(err) != apierror.CodeValidationFailed {
			t.Fatal("an unknown account was validated")
		}
	})

	t.Run("verifying an extra account after onboarding does not move the lifecycle", func(t *testing.T) {
		t.Parallel()
		m := mustActive(t)
		clk := testClock()
		second := testBankAccount()
		second.ID = "ba_2"
		second.Currency = "EUR"
		if err := m.AddBankAccount(second, clk); err != nil {
			t.Fatalf("AddBankAccount: %v", err)
		}
		if err := m.ValidateBankAccount("ba_2", "vr_2", clk); err != nil {
			t.Fatalf("ValidateBankAccount: %v", err)
		}
		if m.Status() != StatusActive {
			t.Fatalf("verifying an extra account moved a live merchant to %s", m.Status())
		}
		if len(m.DrainEvents()) != 0 {
			t.Fatal("verifying an extra account raised a lifecycle event")
		}
		// The original default is unchanged: a newly verified account must not silently become
		// the settlement destination.
		if m.DefaultBankAccount() == nil || m.DefaultBankAccount().ID != "ba_1" {
			t.Fatalf("default account = %+v", m.DefaultBankAccount())
		}
	})

	t.Run("BUG: a replacement account cannot be validated from BANK_VALIDATION_FAILED", func(t *testing.T) {
		t.Parallel()
		// NOTE — this documents the production code's ACTUAL behaviour. ValidateBankAccount
		// explicitly special-cases BANK_VALIDATION_FAILED as a state it should advance from, but
		// the transition table (correctly, per docs/state-machines.md §2.2) only allows
		// BANK_VALIDATION_FAILED → KYC_APPROVED. The result is that the documented recovery path
		// — #13, "replace the account, re-dispatch step 4" — is unreachable through this method:
		// it always fails with INVALID_STATE_TRANSITION. Worse, the account row has already been
		// marked VERIFIED by the time the transition is refused, so the refusal leaves the
		// aggregate partially mutated. Reported, not fixed here.
		m := mustRehydrate(t, func(p *RehydrateParams) {
			p.Status = StatusKYCApproved
			p.BankAccounts[0].Status = BankUnverified
			p.BankAccounts[0].ValidatedAt = nil
		})
		clk := testClock()
		if err := m.FailBankValidation("ba_1", "account holder name mismatch", clk); err != nil {
			t.Fatalf("FailBankValidation: %v", err)
		}
		if m.Status() != StatusBankValidationFailed {
			t.Fatalf("status = %s", m.Status())
		}
		replacement := testBankAccount()
		replacement.ID = "ba_2"
		if err := m.AddBankAccount(replacement, clk); err != nil {
			t.Fatalf("AddBankAccount: %v", err)
		}

		err := m.ValidateBankAccount("ba_2", "vr_2", clk)
		if err == nil {
			t.Fatal("validating a replacement account from BANK_VALIDATION_FAILED now succeeds — " +
				"the bug this test documents has been fixed; assert the transition instead")
		}
		if apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
			t.Fatalf("code = %s, want INVALID_STATE_TRANSITION (%v)", apierror.CodeOf(err), err)
		}
		// The partial mutation the refusal leaves behind.
		for _, a := range m.BankAccounts() {
			if a.ID == "ba_2" && a.Status != BankVerified {
				t.Fatalf("account ba_2 status = %s; this test documents that the refused call "+
					"still marked it VERIFIED", a.Status)
			}
		}
	})

	t.Run("configuration versions must be positive", func(t *testing.T) {
		t.Parallel()
		m := mustRehydrate(t, func(p *RehydrateParams) { p.Status = StatusConfiguring })
		clk := testClock()
		for _, v := range []int{0, -1} {
			if apierror.CodeOf(m.ApplyConfiguration(v, clk)) != apierror.CodeConfigurationInvalid {
				t.Fatalf("ApplyConfiguration(%d) was accepted", v)
			}
			if apierror.CodeOf(m.SetActiveConfigVersion(v, clk)) != apierror.CodeConfigurationInvalid {
				t.Fatalf("SetActiveConfigVersion(%d) was accepted", v)
			}
		}
		if m.Status() != StatusConfiguring {
			t.Fatalf("a refused configuration moved the merchant to %s", m.Status())
		}
	})

	t.Run("a live merchant publishes a new configuration version without a lifecycle transition", func(t *testing.T) {
		t.Parallel()
		// Dropping a live merchant back into an onboarding state for a config edit would stop
		// payments; the versioned publish path is non-disruptive by design.
		m := mustActive(t)
		clk := testClock()
		before := m.Version()
		if err := m.SetActiveConfigVersion(9, clk); err != nil {
			t.Fatalf("SetActiveConfigVersion: %v", err)
		}
		if m.ActiveConfigVersion() != 9 || m.Status() != StatusActive {
			t.Fatalf("version = %d status = %s", m.ActiveConfigVersion(), m.Status())
		}
		if m.Version() <= before {
			t.Fatal("a configuration change did not advance the aggregate version")
		}
		if len(m.DrainEvents()) != 0 {
			t.Fatal("a configuration change raised a merchant lifecycle event")
		}
	})
}

func TestMerchantAccessorsReturnCopies(t *testing.T) {
	// Verifies: rule 11 of docs/spec/06-code-conventions.md. Returning the live backing array
	// would let a caller change a merchant's beneficial owners, settlement accounts or compliance
	// evidence without going through a method — which is to say, without a version bump, an
	// event, or a guard.
	t.Parallel()

	m := mustActive(t)

	accts := m.BankAccounts()
	accts[0].AccountLast4 = "9999"
	accts[0].Status = BankVerificationFail
	if m.BankAccounts()[0].AccountLast4 == "9999" || m.BankAccounts()[0].Status == BankVerificationFail {
		t.Fatal("mutating the returned bank-account slice changed the aggregate")
	}

	principals := m.Principals()
	principals[0].OwnershipPct = 1
	if m.Principals()[0].OwnershipPct == 1 {
		t.Fatal("mutating the returned principal slice changed the aggregate")
	}

	attestations := m.Attestations()
	attestations[0].ExpiresAt = testEpoch.Add(-oneYear)
	for _, a := range m.Attestations() {
		if a.ExpiresAt.Before(testEpoch) {
			t.Fatal("mutating the returned attestation slice changed the aggregate")
		}
	}

	evts := m.PendingEvents()
	if len(evts) != 0 {
		evts[0].Type = "tampered.v1"
		if m.PendingEvents()[0].Type == "tampered.v1" {
			t.Fatal("mutating the returned event slice changed the aggregate")
		}
	}

	// Rehydrate copies on the way in too: a repository that reuses its scratch slices must not be
	// able to edit an aggregate it has already handed out.
	params := productionReadyParams()
	loaded, err := Rehydrate(params)
	if err != nil {
		t.Fatalf("Rehydrate: %v", err)
	}
	params.BankAccounts[0].AccountLast4 = "0000"
	params.Principals[0].OwnershipPct = 3
	params.Attestations[0].Type = "NOTHING"
	if loaded.BankAccounts()[0].AccountLast4 == "0000" ||
		loaded.Principals()[0].OwnershipPct == 3 ||
		loaded.Attestations()[0].Type == "NOTHING" {
		t.Fatal("mutating the caller's slices after rehydration changed the aggregate")
	}
}

func TestMerchantEventsDrain(t *testing.T) {
	t.Parallel()

	m, clk := newTestMerchant(t)
	if err := m.StartValidation(clk); err != nil {
		t.Fatalf("StartValidation: %v", err)
	}
	if err := m.FailValidation("missing UBO", clk); err != nil {
		t.Fatalf("FailValidation: %v", err)
	}
	if len(m.PendingEvents()) != 1 {
		t.Fatalf("pending = %+v", m.PendingEvents())
	}
	// PendingEvents reads; DrainEvents empties. Draining twice must not hand the outbox the same
	// events again.
	if len(m.PendingEvents()) != 1 {
		t.Fatal("PendingEvents emptied the buffer")
	}
	if len(m.DrainEvents()) != 1 {
		t.Fatal("DrainEvents did not return the pending events")
	}
	if len(m.PendingEvents()) != 0 || len(m.DrainEvents()) != 0 {
		t.Fatal("draining twice returned events the second time")
	}
}

func TestRehydrateMerchant(t *testing.T) {
	// Verifies: rule 1 of docs/spec/06-code-conventions.md.
	t.Parallel()

	t.Run("an unknown status is refused rather than coerced", func(t *testing.T) {
		t.Parallel()
		// The specific danger is that coercing an unknown value to something plausible would most
		// likely coerce it towards ACTIVE — re-enabling payment processing for a merchant
		// somebody deliberately stopped.
		params := productionReadyParams()
		params.Status = "HIBERNATING"
		m, err := Rehydrate(params)
		if apierror.CodeOf(err) != apierror.CodeInternalError {
			t.Fatalf("code = %s, want INTERNAL_ERROR (%v)", apierror.CodeOf(err), err)
		}
		if m != nil {
			t.Fatal("a refused rehydration returned a merchant")
		}

		params = productionReadyParams()
		params.Status = ""
		if _, err := Rehydrate(params); err == nil {
			t.Fatal("an empty status was accepted")
		}

		params = productionReadyParams()
		params.RiskRating = "SPICY"
		if _, err := Rehydrate(params); apierror.CodeOf(err) != apierror.CodeInternalError {
			t.Fatalf("unknown risk rating: code = %s", apierror.CodeOf(err))
		}
		// An unset rating is legitimate — rows written before the column existed have none — and
		// must not make the merchant unreadable.
		params = productionReadyParams()
		params.RiskRating = ""
		if _, err := Rehydrate(params); err != nil {
			t.Fatalf("an unset risk rating was refused: %v", err)
		}
	})

	t.Run("every field and the version survive the round trip", func(t *testing.T) {
		t.Parallel()
		params := productionReadyParams()
		params.Status = StatusSuspended
		params.SuspensionReason = SuspendComplianceExpiry
		suspendedAt := testEpoch.Add(-2 * time.Hour)
		activatedAt := testEpoch.Add(-60 * 24 * time.Hour)
		params.SuspendedAt = &suspendedAt
		params.ActivatedAt = &activatedAt
		params.StatusReason = "PCI SAQ lapsed"

		m, err := Rehydrate(params)
		if err != nil {
			t.Fatalf("Rehydrate: %v", err)
		}
		checks := []struct {
			field string
			ok    bool
		}{
			{"id", m.ID() == params.ID},
			{"tenantId", m.TenantID() == params.TenantID},
			{"legalName", m.LegalName() == params.LegalName},
			{"displayName", m.DisplayName() == params.DisplayName},
			{"externalRef", m.ExternalRef() == params.ExternalRef},
			{"status", m.Status() == StatusSuspended},
			{"version", m.Version() == params.Version},
			{"environment", m.Environment() == params.Environment},
			{"profile", m.Profile().MCC == params.Profile.MCC && m.Profile().Country == params.Profile.Country},
			{"bankAccounts", len(m.BankAccounts()) == 1 && m.BankAccounts()[0].ID == "ba_1"},
			{"principals", len(m.Principals()) == 1 && m.Principals()[0].ID == "pr_1"},
			{"attestations", len(m.Attestations()) == 2},
			{"kycStatus", m.KYCStatus() == KYCApproved},
			{"kycProviderRef", m.KYCProviderRef() == testKYC},
			{"kycCompletedAt", m.KYCCompletedAt() != nil && m.KYCCompletedAt().Equal(*params.KYCCompletedAt)},
			{"kycExpiresAt", m.KYCExpiresAt() != nil && m.KYCExpiresAt().Equal(*params.KYCExpiresAt)},
			{"riskRating", m.RiskRating() == RiskStandard},
			{"certificationId", m.CertificationID() == testCert},
			{"suspensionReason", m.SuspensionReason() == SuspendComplianceExpiry},
			{"suspendedAt", m.SuspendedAt() != nil && m.SuspendedAt().Equal(suspendedAt)},
			{"statusReason", m.StatusReason() == "PCI SAQ lapsed"},
			{"activeConfigVersion", m.ActiveConfigVersion() == 3},
			{"createdAt", m.CreatedAt().Equal(params.CreatedAt)},
			{"updatedAt", m.UpdatedAt().Equal(params.UpdatedAt)},
			{"activatedAt", m.ActivatedAt() != nil && m.ActivatedAt().Equal(activatedAt)},
			// Events are a unit-of-work concern; replaying the lifecycle on every read would
			// raise them all again.
			{"events", len(m.PendingEvents()) == 0},
			{"defaultBankAccount", m.DefaultBankAccount() != nil && m.DefaultBankAccount().ID == "ba_1"},
		}
		for _, c := range checks {
			if !c.ok {
				t.Errorf("%s did not survive the round trip", c.field)
			}
		}

		// And the aggregate is live: the suspension it was loaded with governs what it will do.
		if _, err := m.CanAcceptPayments(testEpoch); apierror.CodeOf(err) != apierror.CodeMerchantSuspended {
			t.Fatalf("a rehydrated suspended merchant: code = %s", apierror.CodeOf(err))
		}
		if err := m.Reinstate(false, testClock()); apierror.CodeOf(err) != apierror.CodeForbidden {
			t.Fatalf("a rehydrated compliance suspension lifted automatically: %v", err)
		}
	})
}

func TestDefaultBankAccountPrefersTheDefaultThenAnyVerifiedOne(t *testing.T) {
	t.Parallel()

	verified := testEpoch.Add(-time.Hour)

	t.Run("no verified account at all", func(t *testing.T) {
		t.Parallel()
		m := mustRehydrate(t, func(p *RehydrateParams) {
			p.BankAccounts[0].Status = BankPendingVerify
		})
		if m.DefaultBankAccount() != nil {
			t.Fatal("an unverified account was offered as a settlement destination")
		}
	})

	t.Run("the default flag wins when it is verified", func(t *testing.T) {
		t.Parallel()
		m := mustRehydrate(t, func(p *RehydrateParams) {
			p.BankAccounts[0].IsDefault = false
			p.BankAccounts = append(p.BankAccounts, BankAccount{
				ID: "ba_2", Country: "GB", Currency: "GBP", Status: BankVerified,
				IsDefault: true, ValidatedAt: &verified,
			})
		})
		if got := m.DefaultBankAccount(); got == nil || got.ID != "ba_2" {
			t.Fatalf("default = %+v", got)
		}
	})

	t.Run("a verified non-default is better than nothing", func(t *testing.T) {
		t.Parallel()
		// The default flag pointing at an account that failed verification must not strand
		// settlement; falling back to any verified account is the safe answer.
		m := mustRehydrate(t, func(p *RehydrateParams) {
			p.BankAccounts[0].Status = BankVerificationFail
			p.BankAccounts = append(p.BankAccounts, BankAccount{
				ID: "ba_2", Country: "GB", Currency: "GBP", Status: BankVerified,
				ValidatedAt: &verified,
			})
		})
		if got := m.DefaultBankAccount(); got == nil || got.ID != "ba_2" {
			t.Fatalf("default = %+v", got)
		}
	})
}

// mustRehydrate builds a merchant from productionReadyParams with the given mutation applied.
func mustRehydrate(t *testing.T, mutate func(*RehydrateParams)) *Merchant {
	t.Helper()
	params := productionReadyParams()
	if mutate != nil {
		mutate(&params)
	}
	m, err := Rehydrate(params)
	if err != nil {
		t.Fatalf("Rehydrate: %v", err)
	}
	m.DrainEvents()
	return m
}

// mustActive returns a merchant that has completed onboarding and is ACTIVE.
func mustActive(t *testing.T) *Merchant {
	t.Helper()
	m := mustRehydrate(t, nil)
	if err := m.Activate(testClock()); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	m.DrainEvents()
	return m
}

// assertRuleID checks that err carries exactly the detail a caller is expected to branch on.
// Rule 3 of docs/spec/06-code-conventions.md.
func assertRuleID(t *testing.T, err error, field, code, ruleID string) {
	t.Helper()
	ae := apierror.From(err)
	if ae == nil {
		t.Fatalf("expected a platform error, got %v", err)
	}
	for _, d := range ae.Details {
		if d.Field == field && d.Code == code && d.RuleID == ruleID {
			return
		}
	}
	t.Fatalf("no detail {field: %s, code: %s, rule: %s} on %v; details = %+v",
		field, code, ruleID, err, ae.Details)
}
