package gateway

import (
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

const (
	testCredRef    = "secret://production/ten_1/mrc_1/stripe"
	testNewCredRef = "secret://production/ten_1/mrc_1/stripe#v2"
)

func newTestConnection(t *testing.T) (*Connection, *shared.FixedClock) {
	t.Helper()
	clk := testClock()
	c, err := NewConnection(NewConnectionParams{
		TenantID:    shared.NewTenantID(),
		MerchantID:  shared.NewMerchantID(),
		GatewayID:   "stripe",
		Environment: shared.EnvironmentProduction,
	}, clk)
	if err != nil {
		t.Fatalf("NewConnection: %v", err)
	}
	return c, clk
}

// declaredConnectionEdges restates the connection transition table independently of the
// implementation. The property test below compares it against the machine over the full
// from × to cross product, so a transition added to (or removed from) connection.go without a
// matching, deliberate change here fails the build.
var declaredConnectionEdges = map[ConnectionStatus]map[ConnectionStatus]bool{
	StatusUnprovisioned: {
		StatusProvisioning: true,
		StatusRevoked:      true,
	},
	StatusProvisioning: {
		StatusProvisioned:        true,
		StatusProvisioningFailed: true,
	},
	StatusProvisioningFailed: {
		StatusProvisioning: true,
		StatusRevoked:      true,
	},
	StatusProvisioned: {
		StatusCertifying:   true,
		StatusProvisioning: true,
		StatusRevoked:      true,
	},
	StatusCertifying: {
		StatusCertified:           true,
		StatusCertificationFailed: true,
	},
	StatusCertificationFailed: {
		StatusCertifying:   true,
		StatusProvisioning: true,
		StatusRevoked:      true,
	},
	StatusCertified: {
		StatusDegraded:   true,
		StatusCertifying: true,
		StatusRevoked:    true,
	},
	StatusDegraded: {
		StatusCertified:  true,
		StatusCertifying: true,
		StatusRevoked:    true,
	},
	StatusRevoked: {},
}

func TestConnectionMachineAcceptsExactlyTheDeclaredEdges(t *testing.T) {
	t.Parallel()

	m := ConnectionMachine()
	states := m.States()
	if len(states) != len(AllConnectionStatuses) {
		t.Fatalf("machine universe has %d states, AllConnectionStatuses has %d",
			len(states), len(AllConnectionStatuses))
	}

	for _, from := range states {
		for _, to := range states {
			want := declaredConnectionEdges[from][to]
			got := m.CanTransition(from, to)
			if got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
			// Transition must agree with CanTransition, and a refusal must be a typed,
			// actionable error rather than a bare failure.
			err := m.Transition(from, to)
			if want && err != nil {
				t.Errorf("Transition(%s, %s) = %v, want nil", from, to, err)
			}
			if !want {
				if err == nil {
					t.Errorf("Transition(%s, %s) = nil, want INVALID_STATE_TRANSITION", from, to)
					continue
				}
				if apierror.CodeOf(err) != apierror.CodeInvalidStateTransition {
					t.Errorf("Transition(%s, %s) code = %s, want INVALID_STATE_TRANSITION",
						from, to, apierror.CodeOf(err))
				}
			}
		}
	}
}

func TestConnectionMachineTerminalAndSelfTransitions(t *testing.T) {
	t.Parallel()

	m := ConnectionMachine()
	for _, s := range m.States() {
		if m.CanTransition(s, s) {
			t.Errorf("%s has an undeclared self-transition", s)
		}
		wantTerminal := s == StatusRevoked
		if m.IsTerminal(s) != wantTerminal {
			t.Errorf("IsTerminal(%s) = %v, want %v", s, m.IsTerminal(s), wantTerminal)
		}
	}
	if m.Initial() != StatusUnprovisioned {
		t.Fatalf("initial = %s, want UNPROVISIONED", m.Initial())
	}
	// Revoking mid-flight is deliberately not expressible: the outcome of the in-flight vendor
	// call is unknown and destroying our credentials would orphan whatever it created.
	for _, inFlight := range []ConnectionStatus{StatusProvisioning, StatusCertifying} {
		if m.CanTransition(inFlight, StatusRevoked) {
			t.Errorf("%s → REVOKED must not be legal while the vendor call's outcome is unknown", inFlight)
		}
	}
}

func TestConnectionHappyPath(t *testing.T) {
	// Verifies: BR-10, FR-35.
	t.Parallel()

	c, clk := newTestConnection(t)
	if c.Status() != StatusUnprovisioned {
		t.Fatalf("initial status = %s", c.Status())
	}
	if c.IsUsableForPayments() {
		t.Fatal("an unprovisioned connection must not carry payments")
	}
	if c.CertificationStatus() != CertificationNotStarted {
		t.Fatalf("certification status = %s", c.CertificationStatus())
	}

	if err := c.BeginProvisioning(clk); err != nil {
		t.Fatalf("BeginProvisioning: %v", err)
	}
	if err := c.CompleteProvisioning("acct_123", testCredRef, clk); err != nil {
		t.Fatalf("CompleteProvisioning: %v", err)
	}
	if c.ExternalAccountRef() != "acct_123" || c.CredentialRef() != testCredRef {
		t.Fatalf("references not recorded: %q %q", c.ExternalAccountRef(), c.CredentialRef())
	}
	if c.ProvisionedAt() == nil {
		t.Fatal("provisionedAt not stamped")
	}
	if c.IsUsableForPayments() {
		t.Fatal("a provisioned but uncertified connection must not carry payments")
	}

	if err := c.RegisterWebhook("whsec_reg_1", "https://hooks.example/stripe", clk); err != nil {
		t.Fatalf("RegisterWebhook: %v", err)
	}
	if err := c.BeginCertification(clk); err != nil {
		t.Fatalf("BeginCertification: %v", err)
	}
	if c.CertificationStatus() != CertificationInProgress {
		t.Fatalf("certification status = %s, want IN_PROGRESS", c.CertificationStatus())
	}
	if err := c.Certify("crt_01", clk); err != nil {
		t.Fatalf("Certify: %v", err)
	}
	if !c.IsUsableForPayments() {
		t.Fatal("a certified connection must carry payments")
	}
	if c.CertificationStatus() != CertificationPassed || c.CertificationReportID() != "crt_01" {
		t.Fatalf("certification evidence not recorded: %s %q",
			c.CertificationStatus(), c.CertificationReportID())
	}

	events := c.DrainEvents()
	want := []EventType{EventGatewayProvisioned, EventGatewayCertified}
	if len(events) != len(want) {
		t.Fatalf("raised %d events, want %d: %+v", len(events), len(want), events)
	}
	for i, e := range events {
		if e.Type != want[i] {
			t.Fatalf("event[%d] = %s, want %s", i, e.Type, want[i])
		}
		if e.AggregateID() != c.ID().String() {
			t.Fatalf("event[%d] partition key = %q, want the connection id", i, e.AggregateID())
		}
		if e.Topic() != "pp.gateways.connection.v1" {
			t.Fatalf("event[%d] topic = %q", i, e.Topic())
		}
	}
	if len(c.DrainEvents()) != 0 {
		t.Fatal("draining twice returned events the second time")
	}
}

func TestConnectionIsUsableForPayments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status ConnectionStatus
		want   bool
	}{
		{StatusUnprovisioned, false},
		{StatusProvisioning, false},
		{StatusProvisioningFailed, false},
		{StatusProvisioned, false},
		{StatusCertifying, false},
		{StatusCertificationFailed, false},
		{StatusCertified, true},
		// A degraded connection is still a valid fallback target. Refusing it converts a partial
		// outage into a total one for every merchant whose primary has just degraded.
		{StatusDegraded, true},
		{StatusRevoked, false},
	}

	if len(tests) != len(AllConnectionStatuses) {
		t.Fatalf("the table covers %d of %d statuses", len(tests), len(AllConnectionStatuses))
	}

	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			t.Parallel()
			c, err := RehydrateConnection(RehydrateConnectionParams{
				ID: shared.NewConnectionID(), GatewayID: "stripe", Status: tc.status, Version: 3,
			})
			if err != nil {
				t.Fatalf("RehydrateConnection: %v", err)
			}
			if got := c.IsUsableForPayments(); got != tc.want {
				t.Fatalf("IsUsableForPayments() in %s = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

func TestCertifyRequiresAReportID(t *testing.T) {
	t.Parallel()

	c, clk := newTestConnection(t)
	mustProvision(t, c, clk)
	if err := c.BeginCertification(clk); err != nil {
		t.Fatalf("BeginCertification: %v", err)
	}

	err := c.Certify("", clk)
	if err == nil {
		t.Fatal("expected certification without a report to be refused")
	}
	if apierror.CodeOf(err) != apierror.CodeCertificationFailed {
		t.Fatalf("code = %s, want CERTIFICATION_FAILED", apierror.CodeOf(err))
	}
	if c.Status() != StatusCertifying {
		t.Fatalf("a refused Certify changed the status to %s", c.Status())
	}
	if c.IsUsableForPayments() {
		t.Fatal("a refused Certify made the connection usable")
	}
}

func TestRevokeRequiresAReasonAndClearsTheCredential(t *testing.T) {
	t.Parallel()

	c, clk := newTestConnection(t)
	mustProvision(t, c, clk)
	if err := c.BeginCertification(clk); err != nil {
		t.Fatalf("BeginCertification: %v", err)
	}
	if err := c.Certify("crt_01", clk); err != nil {
		t.Fatalf("Certify: %v", err)
	}
	c.DrainEvents()

	if err := c.Revoke("", clk); apierror.CodeOf(err) != apierror.CodeValidationFailed {
		t.Fatalf("revoke without a reason: code = %s, want VALIDATION_FAILED", apierror.CodeOf(err))
	}
	if c.Status() != StatusCertified {
		t.Fatalf("a refused Revoke changed the status to %s", c.Status())
	}

	if err := c.Revoke("merchant offboarded", clk); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if c.Status() != StatusRevoked || c.RevocationReason() != "merchant offboarded" {
		t.Fatalf("revocation not recorded: %s %q", c.Status(), c.RevocationReason())
	}
	if c.CredentialRef() != "" {
		t.Fatalf("revocation left a credential reference behind: %q", c.CredentialRef())
	}
	if c.RevokedAt() == nil {
		t.Fatal("revokedAt not stamped")
	}
	if c.IsUsableForPayments() {
		t.Fatal("a revoked connection is usable")
	}

	events := c.DrainEvents()
	if len(events) != 1 || events[0].Type != EventGatewayConnectionRevoked {
		t.Fatalf("events = %+v", events)
	}
	if !events[0].Type.IsUrgentInvalidation() {
		t.Fatal("revocation must be an urgent cache invalidation")
	}
	if !events[0].RequiresOperatorAttention() {
		t.Fatal("revocation must raise an operational signal")
	}

	// Terminal: nothing further is legal.
	if err := c.Recover(clk); err == nil {
		t.Fatal("a revoked connection was recovered")
	}
	if err := c.RotateCredentials(testNewCredRef, clk); err == nil {
		t.Fatal("a revoked connection accepted a credential rotation")
	}
}

func TestSecretRefValidation(t *testing.T) {
	// Verifies: BR-11, NFR-32.
	t.Parallel()

	tests := []struct {
		name  string
		ref   string
		valid bool
	}{
		{name: "valid", ref: "secret://production/ten_1/mrc_1/stripe", valid: true},
		{name: "empty", ref: ""},
		{name: "raw credential material", ref: "sk_test_FAKE_NOT_A_REAL_KEY_51H8xVf"},
		{name: "wrong scheme", ref: "vault://production/ten_1"},
		{name: "empty path", ref: "secret://"},
		{name: "whitespace path", ref: "secret://   "},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateSecretRef(tc.ref, "credentialRef")
			if tc.valid {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if apierror.CodeOf(err) != apierror.CodeValidationFailed {
				t.Fatalf("code = %s, want VALIDATION_FAILED", apierror.CodeOf(err))
			}
		})
	}
}

func TestCompleteProvisioningRequiresBothReferences(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		accountRef string
		credRef    string
		wantCode   apierror.Code
	}{
		{name: "both present", accountRef: "acct_1", credRef: testCredRef},
		{name: "no account ref", accountRef: "", credRef: testCredRef, wantCode: apierror.CodeWorkflowStepFailed},
		{name: "no credential ref", accountRef: "acct_1", credRef: "", wantCode: apierror.CodeValidationFailed},
		{name: "credential material", accountRef: "acct_1", credRef: "sk_test_FAKE_NOT_A_REAL_KEY_material", wantCode: apierror.CodeValidationFailed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, clk := newTestConnection(t)
			if err := c.BeginProvisioning(clk); err != nil {
				t.Fatalf("BeginProvisioning: %v", err)
			}
			err := c.CompleteProvisioning(tc.accountRef, tc.credRef, clk)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				if c.Status() != StatusProvisioned {
					t.Fatalf("status = %s", c.Status())
				}
				return
			}
			if apierror.CodeOf(err) != tc.wantCode {
				t.Fatalf("code = %s, want %s (%v)", apierror.CodeOf(err), tc.wantCode, err)
			}
			if c.Status() != StatusProvisioning {
				t.Fatalf("a refused CompleteProvisioning advanced the status to %s", c.Status())
			}
		})
	}
}

func TestRecoveryEdges(t *testing.T) {
	// Verifies: FR-39.
	t.Parallel()

	t.Run("provisioning failure is retried", func(t *testing.T) {
		t.Parallel()
		c, clk := newTestConnection(t)
		if err := c.BeginProvisioning(clk); err != nil {
			t.Fatalf("BeginProvisioning: %v", err)
		}
		if err := c.FailProvisioning("vendor rejected the tax id", clk); err != nil {
			t.Fatalf("FailProvisioning: %v", err)
		}
		if c.Status() != StatusProvisioningFailed || c.StatusReason() == "" {
			t.Fatalf("status = %s reason = %q", c.Status(), c.StatusReason())
		}
		if err := c.BeginProvisioning(clk); err != nil {
			t.Fatalf("retry after a provisioning failure: %v", err)
		}
	})

	t.Run("certification failure is re-run", func(t *testing.T) {
		t.Parallel()
		c, clk := newTestConnection(t)
		mustProvision(t, c, clk)
		if err := c.BeginCertification(clk); err != nil {
			t.Fatalf("BeginCertification: %v", err)
		}
		if err := c.FailCertification("crt_bad", "refund suite step 4 failed", clk); err != nil {
			t.Fatalf("FailCertification: %v", err)
		}
		if c.CertificationStatus() != CertificationFailed {
			t.Fatalf("certification status = %s", c.CertificationStatus())
		}
		evts := c.DrainEvents()
		last := evts[len(evts)-1]
		if last.Type != EventGatewayCertificationFailed || !last.RequiresOperatorAttention() {
			t.Fatalf("expected an attention-requiring certification failure event, got %+v", last)
		}
		if err := c.BeginCertification(clk); err != nil {
			t.Fatalf("re-run after a certification failure: %v", err)
		}
	})

	t.Run("certification failure can fall back to re-provisioning", func(t *testing.T) {
		t.Parallel()
		c, clk := newTestConnection(t)
		mustProvision(t, c, clk)
		if err := c.BeginCertification(clk); err != nil {
			t.Fatalf("BeginCertification: %v", err)
		}
		if err := c.FailCertification("", "webhook never arrived", clk); err != nil {
			t.Fatalf("FailCertification: %v", err)
		}
		if err := c.BeginProvisioning(clk); err != nil {
			t.Fatalf("re-provision after a certification failure: %v", err)
		}
	})

	t.Run("degrade and recover", func(t *testing.T) {
		t.Parallel()
		c, clk := newTestConnection(t)
		mustCertify(t, c, clk)
		if err := c.MarkDegraded("", clk); err == nil {
			t.Fatal("degrading without a reason was accepted")
		}
		if err := c.MarkDegraded("credential expires in 7 days", clk); err != nil {
			t.Fatalf("MarkDegraded: %v", err)
		}
		if !c.IsUsableForPayments() {
			t.Fatal("a degraded connection must remain a valid fallback target")
		}
		if err := c.Recover(clk); err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if c.Status() != StatusCertified || c.StatusReason() != "" {
			t.Fatalf("status = %s reason = %q", c.Status(), c.StatusReason())
		}
	})
}

func TestRotateCredentials(t *testing.T) {
	// Verifies: BR-12, FR-38.
	t.Parallel()

	c, clk := newTestConnection(t)
	mustCertify(t, c, clk)
	before := c.Version()
	c.DrainEvents()

	if err := c.RotateCredentials("not-a-ref", clk); err == nil {
		t.Fatal("rotation accepted credential material")
	}
	if err := c.RotateCredentials(testCredRef, clk); err == nil {
		t.Fatal("rotation to the identical reference was accepted")
	}
	if c.Version() != before {
		t.Fatal("a refused rotation bumped the version")
	}

	if err := c.RotateCredentials(testNewCredRef, clk); err != nil {
		t.Fatalf("RotateCredentials: %v", err)
	}
	if c.CredentialRef() != testNewCredRef {
		t.Fatalf("credential ref = %q", c.CredentialRef())
	}
	// Rotation must not be a lifecycle transition: the connection has to keep carrying traffic
	// throughout, which is the only way a ≤90-day rotation control is achievable.
	if c.Status() != StatusCertified || !c.IsUsableForPayments() {
		t.Fatalf("rotation changed the connection status to %s", c.Status())
	}
	evts := c.DrainEvents()
	if len(evts) != 1 || evts[0].Type != EventGatewayCredentialsRotated {
		t.Fatalf("events = %+v", evts)
	}
	if evts[0].Payload["previousCredentialRef"] != testCredRef {
		t.Fatalf("rotation event does not carry the previous reference: %+v", evts[0].Payload)
	}
	if !evts[0].Type.IsUrgentInvalidation() {
		t.Fatal("rotation must invalidate cached credentials urgently")
	}
}

func TestNewConnectionValidation(t *testing.T) {
	t.Parallel()

	valid := NewConnectionParams{
		TenantID:    shared.NewTenantID(),
		MerchantID:  shared.NewMerchantID(),
		GatewayID:   "stripe",
		Environment: shared.EnvironmentProduction,
	}

	tests := []struct {
		name     string
		mutate   func(*NewConnectionParams)
		wantCode apierror.Code
	}{
		{name: "valid", mutate: func(*NewConnectionParams) {}},
		{name: "no tenant", mutate: func(p *NewConnectionParams) { p.TenantID = "" }, wantCode: apierror.CodeMissingTenantContext},
		{name: "no merchant", mutate: func(p *NewConnectionParams) { p.MerchantID = "" }, wantCode: apierror.CodeValidationFailed},
		{name: "no gateway", mutate: func(p *NewConnectionParams) { p.GatewayID = "" }, wantCode: apierror.CodeValidationFailed},
		{name: "bad environment", mutate: func(p *NewConnectionParams) { p.Environment = "staging" }, wantCode: apierror.CodeValidationFailed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := valid
			tc.mutate(&p)
			_, err := NewConnection(p, testClock())
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if apierror.CodeOf(err) != tc.wantCode {
				t.Fatalf("code = %s, want %s", apierror.CodeOf(err), tc.wantCode)
			}
		})
	}
}

func TestRehydrateConnectionRejectsUnknownStatus(t *testing.T) {
	t.Parallel()

	if _, err := RehydrateConnection(RehydrateConnectionParams{
		ID: shared.NewConnectionID(), Status: "QUARANTINED",
	}); apierror.CodeOf(err) != apierror.CodeInternalError {
		t.Fatalf("unknown status: code = %s, want INTERNAL_ERROR", apierror.CodeOf(err))
	}
	if _, err := RehydrateConnection(RehydrateConnectionParams{
		ID: shared.NewConnectionID(), Status: StatusCertified, CertificationStatus: "MAYBE",
	}); apierror.CodeOf(err) != apierror.CodeInternalError {
		t.Fatalf("unknown certification status: code = %s, want INTERNAL_ERROR", apierror.CodeOf(err))
	}
	c, err := RehydrateConnection(RehydrateConnectionParams{
		ID: shared.NewConnectionID(), Status: StatusCertified,
	})
	if err != nil {
		t.Fatalf("RehydrateConnection: %v", err)
	}
	if c.CertificationStatus() != CertificationNotStarted {
		t.Fatalf("blank certification status defaulted to %s", c.CertificationStatus())
	}
}

func mustProvision(t *testing.T, c *Connection, clk shared.Clock) {
	t.Helper()
	if err := c.BeginProvisioning(clk); err != nil {
		t.Fatalf("BeginProvisioning: %v", err)
	}
	if err := c.CompleteProvisioning("acct_123", testCredRef, clk); err != nil {
		t.Fatalf("CompleteProvisioning: %v", err)
	}
}

func mustCertify(t *testing.T, c *Connection, clk shared.Clock) {
	t.Helper()
	mustProvision(t, c, clk)
	if err := c.BeginCertification(clk); err != nil {
		t.Fatalf("BeginCertification: %v", err)
	}
	if err := c.Certify("crt_01", clk); err != nil {
		t.Fatalf("Certify: %v", err)
	}
}
