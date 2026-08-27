package gateway

import (
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// ConnectionStatus is the lifecycle of one merchant's binding to one gateway.
//
// The pipeline is provision → certify → serve, and each phase has a distinct failure state
// rather than one shared FAILED. That is the difference between an operator who can see that
// six merchants failed certification on the same suite step and one who sees "six connections
// failed". The two failures also have different owners: provisioning failures are ours or the
// vendor's, certification failures are usually the merchant's configuration.
type ConnectionStatus string

const (
	// StatusUnprovisioned is a connection that has been decided on but not yet acted on. It
	// exists as a row before anything is called at the gateway, for the same reason a payment
	// attempt does: a crash mid-provisioning must leave evidence that provisioning was started.
	StatusUnprovisioned ConnectionStatus = "UNPROVISIONED"

	// StatusProvisioning means a sub-merchant account is being created at the gateway. The
	// outcome is unknown while the connection sits here.
	StatusProvisioning ConnectionStatus = "PROVISIONING"

	// StatusProvisioningFailed means the gateway refused or the call failed. Recoverable: the
	// usual causes are a missing business document or a vendor-side validation the merchant can
	// fix, after which provisioning is retried.
	StatusProvisioningFailed ConnectionStatus = "PROVISIONING_FAILED"

	// StatusProvisioned means the account exists at the gateway and credentials are in the secret
	// store — but nothing has proved the integration actually works end to end. A connection may
	// not carry payment traffic from here; that is what certification is for.
	StatusProvisioned ConnectionStatus = "PROVISIONED"

	// StatusCertifying means the certification suite (baseline §11.4) is running against sandbox.
	StatusCertifying ConnectionStatus = "CERTIFYING"

	// StatusCertificationFailed means the suite did not pass. Recoverable in two directions: back
	// to CERTIFYING for a re-run after a configuration fix, or back to PROVISIONING when the
	// failure turns out to be a defect in what was provisioned.
	StatusCertificationFailed ConnectionStatus = "CERTIFICATION_FAILED"

	// StatusCertified means the connection is proven and may carry payments.
	StatusCertified ConnectionStatus = "CERTIFIED"

	// StatusDegraded means the connection still works but something is wrong with it — an expiring
	// credential, a failing scheduled L3 probe, a gateway-side warning. It is a *usable* state on
	// purpose; see IsUsableForPayments.
	StatusDegraded ConnectionStatus = "DEGRADED"

	// StatusRevoked means the credentials are destroyed and the binding is permanently dead.
	// Terminal. Re-establishing a relationship with the same gateway creates a new connection
	// with a new ID rather than resurrecting this one, so that the audit trail of what was live
	// when cannot be rewritten.
	StatusRevoked ConnectionStatus = "REVOKED"
)

// AllConnectionStatuses is the complete state universe, used to build the machine and to drive
// the exhaustive transition property test.
var AllConnectionStatuses = []ConnectionStatus{
	StatusUnprovisioned, StatusProvisioning, StatusProvisioningFailed,
	StatusProvisioned, StatusCertifying, StatusCertificationFailed,
	StatusCertified, StatusDegraded, StatusRevoked,
}

// terminalConnectionStatuses have no outgoing transitions. Only REVOKED qualifies: every other
// state, including both failure states, has a way forward, because a connection stuck with no
// legal move is an onboarding case a human has to resolve by editing the database.
var terminalConnectionStatuses = []ConnectionStatus{StatusRevoked}

// connectionMachine is the full transition table. Anything not listed here is rejected with
// INVALID_STATE_TRANSITION, and this table is the single source of truth for the domain check,
// the generated database CHECK constraint and the diagram in docs/state-machines.md.
var connectionMachine = shared.NewStateMachine("gateway_connection", StatusUnprovisioned,
	AllConnectionStatuses, terminalConnectionStatuses,
	[]shared.Transition[ConnectionStatus]{
		// The happy path.
		{From: StatusUnprovisioned, To: StatusProvisioning},
		{From: StatusProvisioning, To: StatusProvisioned},
		{From: StatusProvisioned, To: StatusCertifying},
		{From: StatusCertifying, To: StatusCertified},

		// Failure edges out of each in-flight phase.
		{From: StatusProvisioning, To: StatusProvisioningFailed},
		{From: StatusCertifying, To: StatusCertificationFailed},

		// Recovery edges back into the pipeline. Retrying provisioning is safe because the
		// gateway APIs involved are idempotent on our external reference; a retry either creates
		// the account or returns the one that already exists.
		{From: StatusProvisioningFailed, To: StatusProvisioning},
		{From: StatusCertificationFailed, To: StatusCertifying},
		// Certification failures are not always certification's fault. A suite that fails on
		// "webhook not received" is usually a provisioning defect — the endpoint was registered
		// against the wrong account — so the deeper recovery has to be reachable without deleting
		// and recreating the connection, which would lose the failure history.
		{From: StatusCertificationFailed, To: StatusProvisioning},

		// Re-provisioning a working connection. Happens when a gateway forces a sub-merchant
		// account migration, or when credentials are rotated in a way that requires re-issuing
		// the account reference rather than just the secret.
		{From: StatusProvisioned, To: StatusProvisioning},

		// Re-certification. Certifications expire, and a vendor API version bump invalidates the
		// evidence that the integration works. A certified connection therefore has to be able to
		// re-enter CERTIFYING without first being torn down.
		{From: StatusCertified, To: StatusCertifying},
		// A degraded connection may also be re-certified: running the suite is exactly how an
		// operator confirms whether a warning is real.
		{From: StatusDegraded, To: StatusCertifying},

		// Degradation and recovery.
		{From: StatusCertified, To: StatusDegraded},
		{From: StatusDegraded, To: StatusCertified},

		// Revocation. Reachable from every settled state, and deliberately *not* from the two
		// in-flight states (PROVISIONING, CERTIFYING). Revoking mid-provisioning would destroy
		// our credentials while a sub-merchant account may or may not have just been created at
		// the vendor, leaving an orphan we can neither see nor close — the same
		// unknown-outcome problem as a payment timeout, and it gets the same answer: let the
		// in-flight operation land first, then revoke.
		{From: StatusUnprovisioned, To: StatusRevoked},
		{From: StatusProvisioningFailed, To: StatusRevoked},
		{From: StatusProvisioned, To: StatusRevoked},
		{From: StatusCertificationFailed, To: StatusRevoked},
		{From: StatusCertified, To: StatusRevoked},
		{From: StatusDegraded, To: StatusRevoked},
	})

// ConnectionMachine exposes the connection state machine for the validation plane, the
// documentation generator, the SQL constraint generator and the exhaustive property test.
func ConnectionMachine() *shared.StateMachine[ConnectionStatus] { return connectionMachine }

// IsTerminal reports whether the connection can never change state again.
func (s ConnectionStatus) IsTerminal() bool { return connectionMachine.IsTerminal(s) }

// IsKnown reports whether s is a state this binary understands.
func (s ConnectionStatus) IsKnown() bool { return connectionMachine.IsKnown(s) }

// String satisfies fmt.Stringer.
func (s ConnectionStatus) String() string { return string(s) }

// CertificationStatus is the certification evidence trail, kept alongside the connection status.
//
// It is not redundant with ConnectionStatus even though the two move together, because the two
// answer different questions and diverge the moment the connection moves on. A REVOKED connection
// has no connection-status memory of ever having passed certification; the compliance question
// "was this merchant certified when it processed that payment last March" is answered from here.
type CertificationStatus string

const (
	CertificationNotStarted CertificationStatus = "NOT_STARTED"
	CertificationInProgress CertificationStatus = "IN_PROGRESS"
	CertificationPassed     CertificationStatus = "PASSED"
	CertificationFailed     CertificationStatus = "FAILED"
)

// IsValid reports whether c is a known certification status.
func (c CertificationStatus) IsValid() bool {
	switch c {
	case CertificationNotStarted, CertificationInProgress, CertificationPassed, CertificationFailed:
		return true
	default:
		return false
	}
}

// String satisfies fmt.Stringer.
func (c CertificationStatus) String() string { return string(c) }

// secretRefScheme is the required prefix of every credential reference on a Connection.
//
// The prefix is enforced rather than assumed because it is the only structural guard against the
// failure this platform cannot recover from: a developer putting an actual API key in the field
// named credentialRef, which would then be persisted unencrypted, logged in a connection dump,
// and returned by the control-plane read API. A `secret://` URI is a pointer into the secret
// store — resolving it requires IAM the API process has and the log pipeline does not.
const secretRefScheme = "secret://"

// ValidateSecretRef checks that a credential reference is a reference and not credential
// material. Exported because the tenant context and the webhook configuration carry the same kind
// of value and must apply the same check.
func ValidateSecretRef(ref, field string) error {
	if ref == "" {
		return apierror.New(apierror.CodeValidationFailed, "a credential reference is required").
			WithDetail(apierror.Detail{
				Field: field, Code: "MISSING_SECRET_REF",
				Message: "expected a secret:// URI pointing at the secret store",
				RuleID:  "L4.CREDENTIAL_IS_A_REFERENCE",
			})
	}
	if !strings.HasPrefix(ref, secretRefScheme) {
		return apierror.New(apierror.CodeValidationFailed,
			"a credential reference must be a secret:// URI, never credential material").
			WithDetail(apierror.Detail{
				Field: field, Code: "NOT_A_SECRET_REF",
				Message: "expected a value of the form secret://{env}/{tenant}/{merchant}/{gateway}",
				RuleID:  "L4.CREDENTIAL_IS_A_REFERENCE",
			})
	}
	if strings.TrimSpace(strings.TrimPrefix(ref, secretRefScheme)) == "" {
		return apierror.New(apierror.CodeValidationFailed, "the secret reference has an empty path").
			WithDetail(apierror.Detail{
				Field: field, Code: "EMPTY_SECRET_PATH",
				Message: "expected a value of the form secret://{env}/{tenant}/{merchant}/{gateway}",
				RuleID:  "L4.CREDENTIAL_IS_A_REFERENCE",
			})
	}
	return nil
}

// Connection is the aggregate root for the binding of one merchant to one gateway.
//
// It is a separate aggregate from both Merchant and Gateway, and that is the decision this file
// turns on. A merchant gains and loses gateway connections without changing lifecycle status, and
// a gateway's capabilities change without touching any merchant. Folding connections into either
// neighbour would put a credential rotation and an onboarding transition in the same write
// transaction, at very different rates of change, for no invariant that spans them.
//
// What it holds and, more importantly, what it does not: there is no credential material on this
// struct and no field one could be put in. Everything is a reference — the gateway's own account
// ID, a secret:// URI, a webhook registration ID. A dump of this aggregate is safe to log.
type Connection struct {
	id          shared.ConnectionID
	tenantID    shared.TenantID
	merchantID  shared.MerchantID
	gatewayID   shared.GatewayID
	environment shared.Environment

	// externalAccountRef is the gateway's own identifier for this merchant — Stripe's connected
	// account, Adyen's sub-merchant. It is the join key for every reconciliation against a vendor
	// report, which is why it is captured at provisioning time and never recomputed.
	externalAccountRef string

	// credentialRef points into the secret store. Never material; see ValidateSecretRef.
	credentialRef string

	webhookRegistrationID string
	webhookEndpoint       string

	certificationStatus   CertificationStatus
	certificationReportID string

	status  ConnectionStatus
	version shared.Version

	createdAt     time.Time
	updatedAt     time.Time
	provisionedAt *time.Time
	certifiedAt   *time.Time
	revokedAt     *time.Time

	// lastHealthCheckAt is when the scheduled L3 probe last ran against this connection.
	// Its staleness is itself a signal: a connection nobody has probed for a week is not known
	// to be healthy, it is unmeasured.
	lastHealthCheckAt *time.Time

	statusReason     string
	revocationReason string

	events []Event
}

// NewConnectionParams are the inputs to creating a connection.
type NewConnectionParams struct {
	TenantID    shared.TenantID
	MerchantID  shared.MerchantID
	GatewayID   shared.GatewayID
	Environment shared.Environment
}

// NewConnection creates a connection in UNPROVISIONED.
//
// The row exists before anything is called at the gateway. That ordering is the same one the
// payment attempt uses and it is load-bearing for the same reason: if the process dies during
// provisioning, the connection row is the only evidence that an account may now exist at the
// vendor under our name, and the reconciler needs it to go and look.
func NewConnection(p NewConnectionParams, clock shared.Clock) (*Connection, error) {
	if p.TenantID.IsZero() {
		return nil, apierror.New(apierror.CodeMissingTenantContext, "a connection requires a tenant")
	}
	if p.MerchantID.IsZero() {
		return nil, apierror.New(apierror.CodeValidationFailed, "a connection requires a merchant").
			WithDetail(apierror.Detail{
				Field: "merchantId", Code: "MISSING", Message: "a merchant is required",
				RuleID: "L4.CONNECTION_REQUIRES_MERCHANT",
			})
	}
	if p.GatewayID.IsZero() {
		return nil, apierror.New(apierror.CodeValidationFailed, "a connection requires a gateway").
			WithDetail(apierror.Detail{
				Field: "gatewayId", Code: "MISSING", Message: "a gateway is required",
				RuleID: "L4.CONNECTION_REQUIRES_GATEWAY",
			})
	}
	if !p.Environment.IsValid() {
		return nil, apierror.Newf(apierror.CodeValidationFailed,
			"invalid environment %q: must be sandbox or production", p.Environment)
	}
	now := clock.Now()
	return &Connection{
		id:                  shared.NewConnectionID(),
		tenantID:            p.TenantID,
		merchantID:          p.MerchantID,
		gatewayID:           p.GatewayID,
		environment:         p.Environment,
		certificationStatus: CertificationNotStarted,
		status:              StatusUnprovisioned,
		version:             1,
		createdAt:           now,
		updatedAt:           now,
	}, nil
}

// Accessors.

func (c *Connection) ID() shared.ConnectionID                  { return c.id }
func (c *Connection) TenantID() shared.TenantID                { return c.tenantID }
func (c *Connection) MerchantID() shared.MerchantID            { return c.merchantID }
func (c *Connection) GatewayID() shared.GatewayID              { return c.gatewayID }
func (c *Connection) Environment() shared.Environment          { return c.environment }
func (c *Connection) ExternalAccountRef() string               { return c.externalAccountRef }
func (c *Connection) WebhookRegistrationID() string            { return c.webhookRegistrationID }
func (c *Connection) WebhookEndpoint() string                  { return c.webhookEndpoint }
func (c *Connection) CertificationStatus() CertificationStatus { return c.certificationStatus }
func (c *Connection) CertificationReportID() string            { return c.certificationReportID }
func (c *Connection) Status() ConnectionStatus                 { return c.status }
func (c *Connection) Version() shared.Version                  { return c.version }
func (c *Connection) CreatedAt() time.Time                     { return c.createdAt }
func (c *Connection) UpdatedAt() time.Time                     { return c.updatedAt }
func (c *Connection) ProvisionedAt() *time.Time                { return c.provisionedAt }
func (c *Connection) CertifiedAt() *time.Time                  { return c.certifiedAt }
func (c *Connection) RevokedAt() *time.Time                    { return c.revokedAt }
func (c *Connection) LastHealthCheckAt() *time.Time            { return c.lastHealthCheckAt }
func (c *Connection) StatusReason() string                     { return c.statusReason }
func (c *Connection) RevocationReason() string                 { return c.revocationReason }

// CredentialRef returns the secret store reference. It is safe to return and safe to log: it is
// a pointer, and resolving it requires IAM permissions the caller may not have.
func (c *Connection) CredentialRef() string { return c.credentialRef }

// IsUsableForPayments reports whether the router may dispatch a payment over this connection.
//
// Exactly two states qualify: CERTIFIED and DEGRADED.
//
// DEGRADED qualifying is the non-obvious half, and it is deliberate. A degraded connection is one
// with a warning attached — a credential approaching expiry, a scheduled probe that failed once,
// a vendor-side advisory — not one that is known broken. If the router refused it, then the
// moment a merchant's primary connection degrades, every payment that would have failed over to
// it instead has nowhere to go: a partial outage on one connection becomes a total outage for
// that merchant. The right handling of a degraded connection is to rank it below a certified one
// in the routing score and to page somebody, both of which happen elsewhere. Refusing it here
// converts a warning into an incident.
//
// Health is a separate question with a separate answer: a CERTIFIED connection whose gateway's
// circuit is open is usable by this method and unusable by the health check. Both gates run.
func (c *Connection) IsUsableForPayments() bool {
	return c.status == StatusCertified || c.status == StatusDegraded
}

// BeginProvisioning moves the connection into PROVISIONING, immediately before the gateway call.
func (c *Connection) BeginProvisioning(clock shared.Clock) error {
	return c.transition(StatusProvisioning, "", clock, "", nil)
}

// CompleteProvisioning records that the sub-merchant account exists and credentials are stored.
//
// Both references are mandatory. A connection that reaches PROVISIONED without an external
// account reference cannot be reconciled against a vendor settlement report, and one without a
// credential reference cannot be dispatched to — in both cases the failure surfaces much later,
// in a place with far less context than this call site.
func (c *Connection) CompleteProvisioning(externalAccountRef, credentialRef string, clock shared.Clock) error {
	if externalAccountRef == "" {
		return apierror.New(apierror.CodeWorkflowStepFailed,
			"provisioning did not yield an external account reference").
			WithDetail(apierror.Detail{
				Field: "externalAccountRef", Code: "MISSING",
				Message: "the gateway's own account identifier is required to reconcile settlement reports",
				RuleID:  "L3.PROVISIONING_YIELDS_ACCOUNT_REF",
			})
	}
	if err := ValidateSecretRef(credentialRef, "credentialRef"); err != nil {
		return err
	}
	now := clock.Now()
	if err := c.transition(StatusProvisioned, "", clock, EventGatewayProvisioned, map[string]any{
		"externalAccountRef": externalAccountRef,
		"environment":        string(c.environment),
	}); err != nil {
		return err
	}
	c.externalAccountRef = externalAccountRef
	c.credentialRef = credentialRef
	c.provisionedAt = &now
	return nil
}

// FailProvisioning records that provisioning did not complete, with the reason an operator will
// read first.
func (c *Connection) FailProvisioning(reason string, clock shared.Clock) error {
	if reason == "" {
		return apierror.New(apierror.CodeValidationFailed, "a provisioning failure requires a reason").
			WithDetail(apierror.Detail{
				Field: "reason", Code: "MISSING",
				Message: "record what the gateway said; this is the first thing an operator reads",
				RuleID:  "L3.FAILURE_CARRIES_REASON",
			})
	}
	return c.transition(StatusProvisioningFailed, reason, clock, "", nil)
}

// BeginCertification starts the certification suite (baseline §11.4).
func (c *Connection) BeginCertification(clock shared.Clock) error {
	if err := c.transition(StatusCertifying, "", clock, "", nil); err != nil {
		return err
	}
	c.certificationStatus = CertificationInProgress
	return nil
}

// Certify records a passing certification run.
//
// The report ID is required and the requirement is not ceremony. Certification is the evidence
// that a merchant was allowed to process live money, it is what an auditor or a scheme asks for,
// and a CERTIFIED connection with no retrievable report is a control that cannot be demonstrated
// — which, for audit purposes, is a control that does not exist.
func (c *Connection) Certify(reportID string, clock shared.Clock) error {
	if reportID == "" {
		return apierror.New(apierror.CodeCertificationFailed,
			"certification cannot be recorded without a report identifier").
			WithDetail(apierror.Detail{
				Field: "certificationReportId", Code: "MISSING",
				Message: "a certification report is the evidence that permits live processing and must be retrievable",
				RuleID:  "L3.CERTIFICATION_REQUIRES_REPORT",
			})
	}
	now := clock.Now()
	if err := c.transition(StatusCertified, "", clock, EventGatewayCertified, map[string]any{
		"certificationReportId": reportID,
		"environment":           string(c.environment),
	}); err != nil {
		return err
	}
	c.certificationStatus = CertificationPassed
	c.certificationReportID = reportID
	c.certifiedAt = &now
	return nil
}

// FailCertification records a failing run. The report ID is optional here — a suite that crashed
// before producing a report still has to be recordable, and refusing the transition would leave
// the connection stuck in CERTIFYING with no legal move.
func (c *Connection) FailCertification(reportID, reason string, clock shared.Clock) error {
	if reason == "" {
		return apierror.New(apierror.CodeValidationFailed, "a certification failure requires a reason").
			WithDetail(apierror.Detail{
				Field: "reason", Code: "MISSING",
				Message: "record which suite step failed",
				RuleID:  "L3.FAILURE_CARRIES_REASON",
			})
	}
	if err := c.transition(StatusCertificationFailed, reason, clock, EventGatewayCertificationFailed,
		map[string]any{"certificationReportId": reportID, "reason": reason}); err != nil {
		return err
	}
	c.certificationStatus = CertificationFailed
	if reportID != "" {
		c.certificationReportID = reportID
	}
	return nil
}

// MarkDegraded records that something is wrong with an otherwise working connection.
func (c *Connection) MarkDegraded(reason string, clock shared.Clock) error {
	if reason == "" {
		return apierror.New(apierror.CodeValidationFailed, "degrading a connection requires a reason").
			WithDetail(apierror.Detail{
				Field: "reason", Code: "MISSING",
				Message: "record what degraded; a connection degraded for no stated reason cannot be recovered with confidence",
				RuleID:  "L3.FAILURE_CARRIES_REASON",
			})
	}
	return c.transition(StatusDegraded, reason, clock, "", nil)
}

// Recover clears a degradation and returns the connection to CERTIFIED.
func (c *Connection) Recover(clock shared.Clock) error {
	if err := c.transition(StatusCertified, "", clock, "", nil); err != nil {
		return err
	}
	c.statusReason = ""
	return nil
}

// RegisterWebhook records the gateway-side webhook registration.
//
// Both halves are stored because both are needed later and for different things: the endpoint is
// what we compare an inbound request against, and the registration ID is what we call the vendor
// with to delete it at revocation. A revocation that cannot deregister the webhook leaves the
// vendor posting to an endpoint that will reject every delivery until the vendor gives up, which
// looks like an outage on their dashboards and generates a support conversation.
func (c *Connection) RegisterWebhook(registrationID, endpoint string, clock shared.Clock) error {
	if registrationID == "" || endpoint == "" {
		return apierror.New(apierror.CodeValidationFailed,
			"a webhook registration requires both an identifier and an endpoint").
			WithDetail(apierror.Detail{
				Field: "webhook", Code: "INCOMPLETE",
				Message: "the registration id is needed to deregister at revocation; the endpoint is needed to verify deliveries",
				RuleID:  "L3.WEBHOOK_REGISTRATION_COMPLETE",
			})
	}
	if c.status == StatusRevoked {
		return apierror.New(apierror.CodeInvalidStateTransition,
			"cannot register a webhook on a revoked connection")
	}
	c.webhookRegistrationID = registrationID
	c.webhookEndpoint = endpoint
	c.touch(clock.Now())
	return nil
}

// RotateCredentials swaps the credential reference without changing the connection's state.
//
// Rotation is not a lifecycle transition and modelling it as one would be wrong: a rotation must
// be possible on a CERTIFIED connection carrying live traffic, without a moment in which the
// connection is not usable. The ≤90-day rotation control (baseline §17.2) only works if it is
// invisible to the data plane, and a state transition would not be.
func (c *Connection) RotateCredentials(newRef string, clock shared.Clock) error {
	if err := ValidateSecretRef(newRef, "credentialRef"); err != nil {
		return err
	}
	if c.status == StatusRevoked {
		return apierror.New(apierror.CodeInvalidStateTransition,
			"cannot rotate credentials on a revoked connection")
	}
	if newRef == c.credentialRef {
		return apierror.New(apierror.CodeValidationFailed,
			"the new credential reference is identical to the current one").
			WithDetail(apierror.Detail{
				Field: "credentialRef", Code: "UNCHANGED",
				Message: "a rotation that does not change the reference is not a rotation and must not be recorded as one",
				RuleID:  "L4.ROTATION_CHANGES_CREDENTIAL",
			})
	}
	previous := c.credentialRef
	now := clock.Now()
	c.credentialRef = newRef
	c.touch(now)
	c.raise(EventGatewayCredentialsRotated, now, map[string]any{
		"previousCredentialRef": previous,
		"credentialRef":         newRef,
	})
	return nil
}

// RecordHealthCheck stamps the last successful L3 probe. It is not a state transition and does
// not bump the version: probes run on a schedule against every connection, and letting them
// contend for the optimistic-concurrency version with real edits would make a rotation lose a
// race with a health probe.
func (c *Connection) RecordHealthCheck(at time.Time) {
	t := at
	c.lastHealthCheckAt = &t
}

// Revoke permanently kills the connection.
//
// The reason is mandatory. Revocation is irreversible, it takes a merchant's ability to process
// on a gateway away, and the first question asked afterwards is always "why did this happen" —
// asked by the merchant, by support, and by whoever is reading the audit log a year later. A
// revocation with no recorded reason forces that answer to be reconstructed from surrounding
// events, and it usually cannot be.
func (c *Connection) Revoke(reason string, clock shared.Clock) error {
	if reason == "" {
		return apierror.New(apierror.CodeValidationFailed, "revoking a connection requires a reason").
			WithDetail(apierror.Detail{
				Field: "reason", Code: "MISSING",
				Message: "revocation is irreversible and must record why it happened",
				RuleID:  "L4.REVOCATION_REQUIRES_REASON",
			})
	}
	now := clock.Now()
	if err := c.transition(StatusRevoked, reason, clock, EventGatewayConnectionRevoked,
		map[string]any{"reason": reason, "gatewayId": c.gatewayID.String()}); err != nil {
		return err
	}
	c.revocationReason = reason
	c.revokedAt = &now
	// The reference is cleared on the aggregate as well as destroyed in the secret store. Leaving
	// it would mean a revoked connection still names the path of a credential we intend nobody to
	// resolve, and defence in depth here costs one assignment.
	c.credentialRef = ""
	return nil
}

func (c *Connection) transition(to ConnectionStatus, reason string, clock shared.Clock, evt EventType, payload map[string]any) error {
	if err := connectionMachine.Transition(c.status, to); err != nil {
		return err
	}
	now := clock.Now()
	c.status = to
	if reason != "" {
		c.statusReason = reason
	}
	c.touch(now)
	if evt != "" {
		c.raise(evt, now, payload)
	}
	return nil
}

func (c *Connection) touch(now time.Time) {
	c.updatedAt = now
	c.version = c.version.Next()
}

func (c *Connection) raise(t EventType, at time.Time, payload map[string]any) {
	c.events = append(c.events, Event{
		Type:         t,
		ConnectionID: c.id,
		GatewayID:    c.gatewayID,
		TenantID:     c.tenantID,
		MerchantID:   c.merchantID,
		OccurredAt:   at,
		Version:      c.version,
		Payload:      payload,
	})
}

// PendingEvents returns the domain events raised in this unit of work.
func (c *Connection) PendingEvents() []Event { return append([]Event(nil), c.events...) }

// DrainEvents returns and clears the pending events. Called exactly once per unit of work, by the
// repository, inside the same transaction as the state change (baseline §13.4).
func (c *Connection) DrainEvents() []Event {
	out := c.events
	c.events = nil
	return out
}

// RehydrateConnectionParams carries the persisted state of a Connection back into the aggregate.
type RehydrateConnectionParams struct {
	ID                    shared.ConnectionID
	TenantID              shared.TenantID
	MerchantID            shared.MerchantID
	GatewayID             shared.GatewayID
	Environment           shared.Environment
	ExternalAccountRef    string
	CredentialRef         string
	WebhookRegistrationID string
	WebhookEndpoint       string
	CertificationStatus   CertificationStatus
	CertificationReportID string
	Status                ConnectionStatus
	Version               shared.Version
	CreatedAt             time.Time
	UpdatedAt             time.Time
	ProvisionedAt         *time.Time
	CertifiedAt           *time.Time
	RevokedAt             *time.Time
	LastHealthCheckAt     *time.Time
	StatusReason          string
	RevocationReason      string
}

// RehydrateConnection reconstructs a Connection from persisted state.
//
// This is the single reviewed doorway past the unexported fields, and it validates that the
// persisted status is one this binary understands. A row carrying an unknown status means a
// deployment rolled back over data written by a newer version; coercing it to something plausible
// could put payment traffic on a connection a newer binary had moved out of service.
func RehydrateConnection(p RehydrateConnectionParams) (*Connection, error) {
	if !p.Status.IsKnown() {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"connection %s has unknown status %q; this row may have been written by a newer version of the service",
			p.ID, p.Status)
	}
	if p.CertificationStatus != "" && !p.CertificationStatus.IsValid() {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"connection %s has unknown certification status %q", p.ID, p.CertificationStatus)
	}
	cert := p.CertificationStatus
	if cert == "" {
		cert = CertificationNotStarted
	}
	return &Connection{
		id: p.ID, tenantID: p.TenantID, merchantID: p.MerchantID, gatewayID: p.GatewayID,
		environment: p.Environment, externalAccountRef: p.ExternalAccountRef,
		credentialRef: p.CredentialRef, webhookRegistrationID: p.WebhookRegistrationID,
		webhookEndpoint: p.WebhookEndpoint, certificationStatus: cert,
		certificationReportID: p.CertificationReportID, status: p.Status, version: p.Version,
		createdAt: p.CreatedAt, updatedAt: p.UpdatedAt, provisionedAt: p.ProvisionedAt,
		certifiedAt: p.CertifiedAt, revokedAt: p.RevokedAt, lastHealthCheckAt: p.LastHealthCheckAt,
		statusReason: p.StatusReason, revocationReason: p.RevocationReason,
	}, nil
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-10, BR-11, BR-12, FR-35, FR-38, FR-39.
//
// The merchant-to-gateway connection: provisioning, certification, credential custody by
// reference only, and rotation with a dual-run overlap
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
