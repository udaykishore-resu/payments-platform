package merchant

import (
	"testing"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// declaredMerchantEdges restates the merchant lifecycle table from
// docs/spec/00-design-baseline.md §8 (which includes amendment A-01) and docs/state-machines.md
// §2.1 **by hand**.
//
// Written out longhand rather than derived from Machine().Edges(), for the reason given at the
// top of the payment package's equivalent: an expectation computed from the code under test
// proves nothing. This map and the table in state.go are two independent statements of the same
// specification, and this is where they are compared.
//
// Note that docs/state-machines.md §2.1 predates amendment A-01 — it lists 37 transitions and no
// COMPLIANCE_REJECTED state. The baseline's §8 table is the current one; the five edges A-01 adds
// are marked below.
var declaredMerchantEdges = map[Status]map[Status]bool{
	StatusCreated: {
		StatusValidating: true,
		StatusTerminated: true,
	},
	StatusValidating: {
		StatusKYCPending:       true,
		StatusValidationFailed: true,
	},
	// Every failure a merchant can correct routes back into the pipeline. Forcing them to start a
	// new merchant record would lose the audit trail of what was wrong and duplicate the entity.
	StatusValidationFailed: {
		StatusValidating: true,
		StatusTerminated: true,
	},
	StatusKYCPending: {
		StatusKYCApproved: true,
		StatusKYCFailed:   true,
	},
	StatusKYCFailed: {
		StatusKYCPending: true,
		StatusTerminated: true,
	},
	StatusKYCApproved: {
		StatusBankValidated:        true,
		StatusBankValidationFailed: true,
	},
	// The only exit is back via KYC_APPROVED with a *new* account. Retrying validation on an
	// account that already failed is how a typo becomes a settlement to the wrong beneficiary.
	StatusBankValidationFailed: {
		StatusKYCApproved: true,
		StatusTerminated:  true,
	},
	StatusBankValidated: {
		StatusGatewayProvisioning: true,
	},
	StatusGatewayProvisioning: {
		StatusConfiguring:        true,
		StatusProvisioningFailed: true,
	},
	StatusProvisioningFailed: {
		StatusGatewayProvisioning: true,
		StatusTerminated:          true,
	},
	StatusConfiguring: {
		StatusSandboxValidation:   true,
		StatusConfigurationFailed: true,
	},
	StatusConfigurationFailed: {
		StatusConfiguring: true,
		StatusTerminated:  true,
	},
	StatusSandboxValidation: {
		StatusCertification:       true,
		StatusConfigurationFailed: true,
	},
	StatusCertification: {
		StatusApproved:            true,
		StatusCertificationFailed: true,
		// A-01: the manual compliance gate needs an exit that is neither approval nor a lie about
		// the integration having failed.
		StatusComplianceRejected: true,
	},
	StatusCertificationFailed: {
		StatusCertification: true,
		StatusConfiguring:   true,
		StatusTerminated:    true,
	},
	// A-01: a compliance rejection routes back to fixable configuration, back to fixable
	// evidence, or forward to termination — and nowhere else. In particular it does not route
	// straight back to CERTIFICATION, because nothing about the integration was the problem.
	StatusComplianceRejected: {
		StatusConfiguring: true,
		StatusKYCPending:  true,
		StatusTerminated:  true,
	},
	StatusApproved: {
		StatusProductionReady: true,
		// A-01: an adverse finding between approval and activation must be expressible without
		// terminating the merchant.
		StatusSuspended: true,
	},
	StatusProductionReady: {
		StatusActive:    true,
		StatusSuspended: true,
	},
	StatusActive: {
		StatusSuspended:  true,
		StatusTerminated: true,
	},
	StatusSuspended: {
		StatusActive:     true,
		StatusTerminated: true,
	},
	// Termination releases credentials, revokes connections and permits data erasure. A returning
	// merchant is a new merchant_id.
	StatusTerminated: {},
}

// declaredMerchantEdgeCount is 37 from docs/state-machines.md §2.1 plus the five amendment A-01
// adds, over 21 × 21 = 441 ordered pairs.
const declaredMerchantEdgeCount = 42

func TestMerchantMachineAcceptsExactlyTheDeclaredEdges(t *testing.T) {
	// Verifies: docs/spec/00-design-baseline.md §8, docs/state-machines.md §2.1, and rule 14 of
	// docs/spec/06-code-conventions.md.
	t.Parallel()

	m := Machine()
	if len(m.States()) != len(AllStatuses) {
		t.Fatalf("machine universe has %d states, AllStatuses has %d", len(m.States()), len(AllStatuses))
	}
	if len(declaredMerchantEdges) != len(AllStatuses) {
		t.Fatalf("the declared table covers %d from-states, AllStatuses has %d",
			len(declaredMerchantEdges), len(AllStatuses))
	}

	accepted, pairs := 0, 0
	for _, from := range AllStatuses {
		for _, to := range AllStatuses {
			pairs++
			want := declaredMerchantEdges[from][to]
			if want {
				accepted++
			}
			if got := m.CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
			err := m.Transition(from, to)
			if want {
				if err != nil {
					t.Errorf("Transition(%s, %s) = %v, want nil", from, to, err)
				}
				continue
			}
			if apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
				t.Errorf("Transition(%s, %s) code = %s, want INVALID_STATE_TRANSITION",
					from, to, apierror.CodeOf(err))
			}
		}
	}
	if pairs != len(AllStatuses)*len(AllStatuses) {
		t.Fatalf("visited %d pairs, want %d", pairs, len(AllStatuses)*len(AllStatuses))
	}
	if accepted != declaredMerchantEdgeCount {
		t.Errorf("the declared table has %d edges, the specification states %d",
			accepted, declaredMerchantEdgeCount)
	}
	if got := len(m.Edges()); got != declaredMerchantEdgeCount {
		t.Errorf("machine has %d edges, want %d", got, declaredMerchantEdgeCount)
	}

	// No implicit self-transitions anywhere: a lifecycle that silently accepts X → X hides the
	// duplicate-signal bug where a workflow step is delivered twice.
	for _, s := range AllStatuses {
		if m.CanTransition(s, s) {
			t.Errorf("%s has an undeclared self-transition", s)
		}
	}
}

func TestMerchantTerminalStates(t *testing.T) {
	t.Parallel()

	for _, s := range AllStatuses {
		want := s == StatusTerminated
		if got := s.IsTerminal(); got != want {
			t.Errorf("Status(%s).IsTerminal() = %v, want %v", s, got, want)
		}
	}
	// Every *_FAILED state is recoverable by design — a failed KYC is a resubmission, not an
	// ending.
	for _, s := range AllStatuses {
		if s.IsFailureState() && s.IsTerminal() {
			t.Errorf("%s is a failure state and terminal; failures must be correctable", s)
		}
	}
	for _, to := range AllStatuses {
		if Machine().CanTransition(StatusTerminated, to) {
			t.Errorf("TERMINATED → %s is permitted", to)
		}
	}
	if !StatusActive.IsKnown() || Status("HIBERNATING").IsKnown() {
		t.Fatal("IsKnown does not discriminate declared statuses")
	}
	if StatusActive.String() != "ACTIVE" {
		t.Fatalf("String() = %q", StatusActive.String())
	}
}

func TestAmendmentA01ComplianceRejection(t *testing.T) {
	// Verifies: amendment A-01 in docs/spec/00-design-baseline.md §8.
	//
	// The original lifecycle had no exit from the manual compliance gate other than approval,
	// which made a compliance officer's rejection unrepresentable: the workflow had to either lie
	// (CERTIFICATION_FAILED, blaming the integration for a policy decision) or hang.
	t.Parallel()

	m := Machine()

	if !m.CanTransition(StatusCertification, StatusComplianceRejected) {
		t.Fatal("CERTIFICATION → COMPLIANCE_REJECTED is refused; a compliance rejection has no home")
	}
	// The rejection is about the business, not the integration, so it is distinct from
	// CERTIFICATION_FAILED and both must exist.
	if !m.CanTransition(StatusCertification, StatusCertificationFailed) {
		t.Fatal("CERTIFICATION → CERTIFICATION_FAILED is refused")
	}

	// Exactly three exits: fixable configuration, fixable evidence, or the end.
	wantExits := map[Status]bool{
		StatusConfiguring: true, StatusKYCPending: true, StatusTerminated: true,
	}
	for _, to := range AllStatuses {
		got := m.CanTransition(StatusComplianceRejected, to)
		if got != wantExits[to] {
			t.Errorf("COMPLIANCE_REJECTED → %s = %v, want %v", to, got, wantExits[to])
		}
	}
	// Specifically not: a compliance rejection cannot be cleared by re-running certification, and
	// it certainly cannot walk straight into APPROVED or ACTIVE.
	for _, to := range []Status{StatusCertification, StatusApproved, StatusProductionReady, StatusActive} {
		if m.CanTransition(StatusComplianceRejected, to) {
			t.Errorf("COMPLIANCE_REJECTED → %s is permitted", to)
		}
	}
	if got := len(m.Next(StatusComplianceRejected)); got != 3 {
		t.Fatalf("COMPLIANCE_REJECTED has %d exits, want 3", got)
	}

	// The second half of A-01: an adverse finding between approval and activation must be
	// expressible without terminating the merchant.
	if !m.CanTransition(StatusApproved, StatusSuspended) {
		t.Fatal("APPROVED → SUSPENDED is refused; an adverse finding before activation has no home")
	}
	if m.CanTransition(StatusApproved, StatusTerminated) {
		t.Fatal("APPROVED → TERMINATED is permitted; suspension is the expressible answer, not termination")
	}
	// A compliance rejection is a place a merchant is parked awaiting a correction, so the
	// onboarding automation must keep a workflow alive for it.
	if !StatusComplianceRejected.IsFailureState() || !StatusComplianceRejected.IsOnboarding() {
		t.Fatal("COMPLIANCE_REJECTED is not classified as a correctable onboarding failure")
	}
}

func TestMerchantForbiddenTransitionsWorthNaming(t *testing.T) {
	// Verifies: docs/state-machines.md §2.2.
	t.Parallel()

	tests := []struct {
		from Status
		to   Status
		why  string
	}{
		{StatusCreated, StatusActive,
			"skips KYC, bank validation, provisioning and certification; a merchant processing live money with no KYB record is a regulatory incident"},
		{StatusProductionReady, StatusCertification,
			"re-certification must not silently un-ready a merchant while payments are in flight"},
		{StatusSuspended, StatusProductionReady,
			"SUSPENDED → ACTIVE is the only exit, and it re-runs the activation guard in full"},
		{StatusKYCFailed, StatusKYCApproved,
			"an approval must come from a new vendor decision, which requires passing through KYC_PENDING"},
		{StatusActive, StatusConfiguring,
			"configuration changes for a live merchant go through the versioned publish path; dropping them back into onboarding stops payments for a config edit"},
		{StatusBankValidationFailed, StatusBankValidated,
			"the only exit is via KYC_APPROVED with a new account; re-validating a failed account is how a typo becomes a settlement to the wrong beneficiary"},
		{StatusCreated, StatusKYCPending,
			"validation is not optional"},
		{StatusActive, StatusApproved,
			"a live merchant does not walk backwards into the onboarding pipeline"},
	}

	for _, tc := range tests {
		t.Run(tc.from.String()+"_to_"+tc.to.String(), func(t *testing.T) {
			t.Parallel()
			if Machine().CanTransition(tc.from, tc.to) {
				t.Fatalf("%s → %s is permitted: %s", tc.from, tc.to, tc.why)
			}
		})
	}
}

func TestCanAcceptPaymentsIsNarrowerThanCanIssueRefunds(t *testing.T) {
	// Verifies: docs/spec/00-design-baseline.md §8 — "suspension rejects new payments but permits
	// refunds, voids and webhook processing; you must always be able to give money back".
	t.Parallel()

	tests := []struct {
		status  Status
		accept  bool
		refund  bool
		onboard bool
		failure bool
	}{
		{status: StatusCreated, onboard: true},
		{status: StatusValidating, onboard: true},
		{status: StatusValidationFailed, onboard: true, failure: true},
		{status: StatusKYCPending, onboard: true},
		{status: StatusKYCApproved, onboard: true},
		{status: StatusKYCFailed, onboard: true, failure: true},
		{status: StatusBankValidated, onboard: true},
		{status: StatusBankValidationFailed, onboard: true, failure: true},
		{status: StatusGatewayProvisioning, onboard: true},
		{status: StatusProvisioningFailed, onboard: true, failure: true},
		{status: StatusConfiguring, onboard: true},
		{status: StatusConfigurationFailed, onboard: true, failure: true},
		{status: StatusSandboxValidation, onboard: true},
		{status: StatusCertification, onboard: true},
		{status: StatusCertificationFailed, onboard: true, failure: true},
		{status: StatusComplianceRejected, onboard: true, failure: true},
		// APPROVED and PRODUCTION_READY may refund but not yet accept: a merchant can reach them
		// carrying refundable sandbox-to-production migration balances and, more importantly,
		// can be suspended from them.
		{status: StatusApproved, refund: true, onboard: true},
		{status: StatusProductionReady, refund: true, onboard: true},
		{status: StatusActive, accept: true, refund: true},
		{status: StatusSuspended, refund: true},
		{status: StatusTerminated},
	}

	if len(tests) != len(AllStatuses) {
		t.Fatalf("the predicate table covers %d statuses, AllStatuses has %d", len(tests), len(AllStatuses))
	}
	seen := make(map[Status]bool, len(tests))

	accepting := 0
	for _, tc := range tests {
		seen[tc.status] = true
		if tc.accept {
			accepting++
		}
		t.Run(tc.status.String(), func(t *testing.T) {
			t.Parallel()
			if got := tc.status.CanAcceptPayments(); got != tc.accept {
				t.Errorf("CanAcceptPayments() = %v, want %v", got, tc.accept)
			}
			if got := tc.status.CanIssueRefunds(); got != tc.refund {
				t.Errorf("CanIssueRefunds() = %v, want %v", got, tc.refund)
			}
			if got := tc.status.IsOnboarding(); got != tc.onboard {
				t.Errorf("IsOnboarding() = %v, want %v", got, tc.onboard)
			}
			if got := tc.status.IsFailureState(); got != tc.failure {
				t.Errorf("IsFailureState() = %v, want %v", got, tc.failure)
			}
			// The asymmetry only ever runs one way: anything that can take money can give it back.
			if tc.accept && !tc.refund {
				t.Error("a status that accepts payments cannot issue refunds")
			}
		})
	}
	for _, s := range AllStatuses {
		if !seen[s] {
			t.Errorf("%s is missing from the predicate table", s)
		}
	}
	if accepting != 1 {
		t.Fatalf("%d statuses accept payments, want exactly 1", accepting)
	}

	// The asymmetry that is deliberate and easy to "simplify" away. A merchant suspended for a
	// risk breach still has customers owed money; blocking refunds during a suspension converts a
	// merchant problem into a consumer-harm problem and, in several jurisdictions, a regulatory
	// one.
	if StatusSuspended.CanAcceptPayments() {
		t.Fatal("a suspended merchant can accept payments")
	}
	if !StatusSuspended.CanIssueRefunds() {
		t.Fatal("a suspended merchant cannot issue refunds; suspension must never strand a customer's money")
	}
	// Only termination stops refunds, and termination requires that no payment is in flight.
	if StatusTerminated.CanIssueRefunds() {
		t.Fatal("a terminated merchant can issue refunds")
	}
	// An unknown status is not a live one.
	if Status("").CanAcceptPayments() || Status("ACTIVE_ISH").CanAcceptPayments() ||
		Status("").CanIssueRefunds() || Status("ACTIVE_ISH").CanIssueRefunds() {
		t.Fatal("an unrecognised status was treated as live")
	}
}

func TestKYCStatusIsSatisfied(t *testing.T) {
	t.Parallel()

	tests := map[KYCStatus]bool{
		KYCNotStarted:   false,
		KYCInProgress:   false,
		KYCApproved:     true,
		KYCRejected:     false,
		KYCReviewNeeded: false,
		KYCExpired:      false,
		"":              false,
		"PROBABLY_FINE": false,
	}
	for status, want := range tests {
		if got := status.IsSatisfied(); got != want {
			t.Errorf("KYCStatus(%q).IsSatisfied() = %v, want %v", status, got, want)
		}
		if status.String() != string(status) {
			t.Errorf("String() = %q", status.String())
		}
	}
}

func TestSuspensionReasonRequiresOperatorReviewToLift(t *testing.T) {
	// Verifies: docs/spec/00-design-baseline.md §8. Sanctions and compliance suspensions never
	// lift automatically; a risk-threshold suspension may, once the underlying metric recovers.
	t.Parallel()

	tests := []struct {
		reason SuspensionReason
		review bool
		why    string
	}{
		{SuspendRiskBreach, false, "the automation that raised it can clear it when the metric recovers"},
		{SuspendNonPayment, false, "payment of the invoice clears it without a judgement call"},
		{SuspendMerchantRequest, false, "the merchant asked for it and may ask for it to end"},
		{SuspendComplianceExpiry, true, "somebody must look at the renewed document"},
		{SuspendKYCExpiry, true, "re-verification is a decision, not a state change"},
		{SuspendGatewayRevoked, true, "the acquiring relationship has to be re-established by a human"},
		{SuspendChargebackRate, true, "scheme monitoring programmes have exit criteria a human signs off"},
		{SuspendOperatorAction, true, "an operator stopped it deliberately"},
		{SuspendSanctionsHit, true, "a sanctions hit lifted by an automated job is a criminal exposure"},
		// Fail closed: a reason nobody has classified must not be automatically liftable.
		{"", true, "an unset reason is not a licence to reinstate"},
		{"SOMETHING_NEW", true, "a reason this binary does not know is not automatically liftable"},
	}

	for _, tc := range tests {
		t.Run(string(tc.reason), func(t *testing.T) {
			t.Parallel()
			if got := tc.reason.RequiresOperatorReviewToLift(); got != tc.review {
				t.Errorf("RequiresOperatorReviewToLift() = %v, want %v (%s)", got, tc.review, tc.why)
			}
			if tc.reason.String() != string(tc.reason) {
				t.Errorf("String() = %q", tc.reason.String())
			}
		})
	}
}

func TestRiskRatingIsValid(t *testing.T) {
	t.Parallel()

	for _, r := range []RiskRating{RiskLow, RiskStandard, RiskElevated, RiskHigh} {
		if !r.IsValid() {
			t.Errorf("RiskRating(%s).IsValid() = false", r)
		}
	}
	for _, r := range []RiskRating{"", "MEDIUM", "low"} {
		if r.IsValid() {
			t.Errorf("RiskRating(%q).IsValid() = true", r)
		}
	}
}

func TestEveryMerchantEventTypeIsListedAndShareOneTopic(t *testing.T) {
	t.Parallel()

	seen := make(map[EventType]bool, len(AllEventTypes))
	for _, e := range AllEventTypes {
		if seen[e] {
			t.Fatalf("%s is listed twice in AllEventTypes", e)
		}
		seen[e] = true
		// Keyed by merchant ID so a consumer cannot see `activated` before `certified` and enable
		// traffic on the strength of it.
		if e.Topic() != "pp.merchants.merchant.v1" {
			t.Errorf("%s topic = %q", e, e.Topic())
		}
		if e.String() != string(e) {
			t.Errorf("String() = %q", e.String())
		}
	}
	for _, e := range []EventType{
		EventMerchantCreated, EventMerchantValidated, EventMerchantValidationFailed,
		EventMerchantKYCApproved, EventMerchantKYCFailed, EventMerchantBankValidated,
		EventMerchantBankValidationFailed, EventMerchantGatewayProvisioned,
		EventMerchantProvisioningFailed, EventMerchantConfigurationFailed,
		EventMerchantCertified, EventMerchantCertificationFailed, EventMerchantComplianceRejected,
		EventMerchantActivated, EventMerchantSuspended, EventMerchantReinstated,
		EventMerchantTerminated,
	} {
		if !seen[e] {
			t.Errorf("%s is not listed in AllEventTypes", e)
		}
	}

	invalidating := map[EventType]bool{
		EventMerchantActivated: true, EventMerchantSuspended: true,
		EventMerchantReinstated: true, EventMerchantTerminated: true,
	}
	urgent := map[EventType]bool{
		EventMerchantSuspended: true, EventMerchantTerminated: true,
	}
	for _, e := range AllEventTypes {
		if got := e.IsCacheInvalidating(); got != invalidating[e] {
			t.Errorf("%s IsCacheInvalidating() = %v, want %v", e, got, invalidating[e])
		}
		if got := e.IsUrgentInvalidation(); got != urgent[e] {
			t.Errorf("%s IsUrgentInvalidation() = %v, want %v", e, got, urgent[e])
		}
		// A suspended merchant that keeps processing for up to the 30-second staleness window is
		// processing money it should not be, so every urgent event is also an invalidating one.
		if e.IsUrgentInvalidation() && !e.IsCacheInvalidating() {
			t.Errorf("%s is urgent but does not invalidate the cache", e)
		}
	}
}
