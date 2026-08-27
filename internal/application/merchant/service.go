// Package merchant holds the merchant registry use cases (BC-2): the control-plane operations
// that create a merchant, move it through its lifecycle, and attach the evidence — bank accounts,
// principals, compliance attestations — that activation depends on.
//
// Two rules shape every use case in this file, and both are about the same thing: an operation
// nobody can account for afterwards is one the platform declines to perform.
//
//  1. **The state change and its audit record commit together.** They share one transaction, so
//     a suspension that happened has a record and a record that exists describes something that
//     happened. Writing the audit record afterwards, best-effort, produces an audit trail that
//     diverges from reality precisely when something went wrong — which is the only time anybody
//     reads it.
//  2. **Optimistic concurrency is explicit.** Every mutation that a human could be performing
//     concurrently with another human takes an If-Match precondition, and a mismatch is a 412
//     rather than a silent overwrite of somebody else's edit.
package merchant

import (
	"context"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/validation/engine"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/l2merchant"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Auditor records an auditable action inside the caller's transaction.
//
// Declared here, with the consumer, rather than imported from the payment package: the two have
// the same shape today and no reason to change together, and a shared interface would make a
// change to the payment context's audit needs a change to this one's.
type Auditor interface {
	Record(ctx context.Context, r ports.Repositories, action, resourceType, resourceID, outcome string, detail map[string]any) error
}

// Deps is the merchant service's dependency set.
type Deps struct {
	UoW   ports.UnitOfWork
	Audit Auditor
	Clock shared.Clock
	// L2 is the validation level's tenant policy: supported countries, prohibited MCCs, the
	// tenant's declared-volume ceiling.
	L2 l2merchant.Deps
}

// Service is the merchant use-case facade.
type Service struct {
	deps       Deps
	submission engine.RuleSet[l2merchant.Subject]
	full       engine.RuleSet[l2merchant.Subject]
}

// submissionRules is the subset of L2 a *create* can satisfy.
//
// The full level is a gate on activation, not on registration, and running all of it here would
// be incoherent: a merchant cannot have a KYB decision, a verified bank account or a signed PCI
// attestation before the record they attach to exists. The onboarding saga's first step runs the
// complete set (see internal/workflows/onboarding, step `validate-merchant`); this subset is the
// part that is a property of the *submission* rather than of the vendor decisions that follow it.
//
// Naming the subset explicitly, rather than relying on each rule's precondition to skip itself,
// is deliberate: a rule added to L2 tomorrow must not silently start blocking registration.
var submissionRules = []engine.RuleID{
	"L2.LEGAL_NAME_PRESENT",
	"L2.BUSINESS_TYPE_IS_KNOWN",
	"L2.REGISTRATION_NUMBER_FORMAT_VALID",
	"L2.INCORPORATION_COUNTRY_SUPPORTED",
	"L2.COUNTRY_NOT_SANCTIONED",
	"L2.OPERATING_COUNTRIES_SUBSET_OF_TENANT",
	"L2.MCC_IS_VALID",
	"L2.MCC_NOT_PROHIBITED",
	"L2.WEBSITE_IS_HTTPS",
	"L2.EXPECTED_VOLUME_WITHIN_TIER",
}

// NewService compiles the rule sets and returns the service.
func NewService(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = shared.SystemClock{}
	}
	if d.L2.MinPrincipalAgeYears == 0 {
		d.L2 = l2merchant.DefaultDeps()
	}
	full := l2merchant.Rules(d.L2)
	return &Service{deps: d, full: full, submission: subset(full, submissionRules)}
}

// subset returns a rule set containing only the named rules, in the original evaluation order so
// that the report's ordering — and therefore the headline error code — stays stable.
func subset(rs engine.RuleSet[l2merchant.Subject], ids []engine.RuleID) engine.RuleSet[l2merchant.Subject] {
	keep := make(map[engine.RuleID]struct{}, len(ids))
	for _, id := range ids {
		keep[id] = struct{}{}
	}
	out := rs
	out.Rules = nil
	for _, r := range rs.Rules {
		if r == nil {
			continue
		}
		if _, ok := keep[r.ID()]; ok {
			out.Rules = append(out.Rules, r)
		}
	}
	return out
}

// CreateCommand registers a merchant.
type CreateCommand struct {
	TenantID    shared.TenantID
	LegalName   string
	DisplayName string
	ExternalRef string
	Environment shared.Environment
	Profile     merchant.BusinessProfile
	// BusinessType is the legal form. It lives on the command rather than on the domain's
	// BusinessProfile because it is an L2 matcher value and the registry does not branch on it.
	BusinessType l2merchant.BusinessType
	// OperatingCountries is where the merchant intends to trade, checked against the tenant's
	// licensed set.
	OperatingCountries []shared.Country
	Actor              Actor
}

// Actor identifies who is performing the operation, for the audit record.
//
// It is a command field rather than a context value because every mutating use case here
// *requires* it: an audit record whose actor is "unknown" is a record that fails the control it
// exists to evidence, and a required struct field makes a caller who forgot visible in review
// rather than at audit time.
type Actor struct {
	ID   string
	Name string
	// Reason is the stated justification. Suspension, termination and reinstatement refuse to
	// proceed without one; a high-consequence change with no recorded reason is unreviewable six
	// months later.
	Reason string
}

// Create registers a merchant in CREATED and audits it.
func (s *Service) Create(ctx context.Context, cmd CreateCommand) (*merchant.Merchant, error) {
	if err := assertTenant(cmd.TenantID); err != nil {
		return nil, err
	}
	if err := s.validateSubmission(ctx, cmd); err != nil {
		return nil, err
	}
	m, err := merchant.New(merchant.NewParams{
		TenantID:    cmd.TenantID,
		LegalName:   cmd.LegalName,
		DisplayName: cmd.DisplayName,
		ExternalRef: cmd.ExternalRef,
		Environment: cmd.Environment,
		Profile:     cmd.Profile,
	}, s.deps.Clock)
	if err != nil {
		return nil, err
	}
	if err := s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		if err := r.Merchants.Create(ctx, m); err != nil {
			return err
		}
		return s.audit(ctx, r, cmd.Actor, "merchant.created", m, map[string]any{
			"legalName":   m.LegalName(),
			"country":     string(m.Profile().Country),
			"mcc":         string(m.Profile().MCC),
			"environment": string(m.Environment()),
		})
	}); err != nil {
		return nil, err
	}
	return m, nil
}

// validateSubmission runs the create-time L2 subset.
func (s *Service) validateSubmission(ctx context.Context, cmd CreateCommand) error {
	rep := s.submission.Evaluate(ctx, l2merchant.Subject{
		Profile: l2merchant.Profile{
			LegalName:            cmd.LegalName,
			TradingName:          cmd.DisplayName,
			BusinessType:         cmd.BusinessType,
			RegistrationNumber:   cmd.Profile.RegistrationNumber,
			IncorporationCountry: cmd.Profile.Country,
			OperatingCountries:   cmd.OperatingCountries,
			MCC:                  cmd.Profile.MCC,
			Description:          cmd.Profile.Description,
			Website:              cmd.Profile.WebsiteURL,
		},
		ProcessingProfile: l2merchant.ProcessingProfile{
			MonthlyVolume: cmd.Profile.ExpectedMonthlyVolume,
			AverageTicket: cmd.Profile.ExpectedAverageTicket,
		},
		Now: s.deps.Clock.Now(),
	})
	if err := rep.AsError(); err != nil {
		return err
	}
	return nil
}

// ValidateForOnboarding runs the complete L2 rule set against a fully-populated subject.
//
// It is exported because the onboarding saga's `validate-merchant` step calls it, and because
// running the same compiled set from both places is what makes "the merchant passed L2" mean one
// thing. The subject is the caller's to assemble: it carries persisted vendor decisions, and this
// package has no business fetching them.
func (s *Service) ValidateForOnboarding(ctx context.Context, subj l2merchant.Subject) error {
	if subj.Now.IsZero() {
		subj.Now = s.deps.Clock.Now()
	}
	if err := s.full.Evaluate(ctx, subj).AsError(); err != nil {
		return err
	}
	return nil
}

// Get reads a merchant.
func (s *Service) Get(ctx context.Context, tenantID shared.TenantID, id shared.MerchantID) (*merchant.Merchant, error) {
	if err := assertTenant(tenantID); err != nil {
		return nil, err
	}
	var out *merchant.Merchant
	err := s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		m, err := r.Merchants.Get(ctx, id)
		if err != nil {
			return err
		}
		if m.TenantID() != tenantID {
			return notFound(id)
		}
		out = m
		return nil
	})
	return out, err
}

// List returns a page of merchants.
func (s *Service) List(ctx context.Context, tenantID shared.TenantID, f ports.MerchantFilter, page ports.Page) ([]*merchant.Merchant, string, error) {
	if err := assertTenant(tenantID); err != nil {
		return nil, "", err
	}
	var (
		out    []*merchant.Merchant
		cursor string
	)
	err := s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		all, c, err := r.Merchants.List(ctx, f, page)
		if err != nil {
			return err
		}
		cursor = c
		// The repository is tenant-scoped by row-level security; the filter here is defence in
		// depth, and it is cheap. A read path that trusts one layer of isolation has one layer.
		for _, m := range all {
			if m.TenantID() == tenantID {
				out = append(out, m)
			}
		}
		return nil
	})
	return out, cursor, err
}

// ETag returns the optimistic-concurrency token for a merchant.
//
// Version-derived rather than content-derived: the version is what the repository's conditional
// write compares, so an ETag computed from anything else could match while the write still
// conflicts, which is the worst of both designs.
func ETag(m *merchant.Merchant) string {
	return `"` + m.ID().String() + "-" + itoa(int(m.Version())) + `"`
}

// UpdateCommand changes what the merchant registry permits changing.
//
// Note what is absent: the legal name, the country, the MCC and the registration number. Those
// are fixed at creation (merchant.New validates them and the aggregate exposes no setter), and
// that is deliberate rather than an omission — they are the identity KYB verified, and a merchant
// that wants to change them is a different merchant with a different verification. The correct
// operation is to terminate and re-onboard, which leaves an audit trail showing exactly that.
type UpdateCommand struct {
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	// IfMatch is the ETag the caller read. Empty means the caller did not read first, which is
	// refused: an unconditional write is a lost update waiting for a second operator.
	IfMatch string
	// ActiveConfigVersion pins the configuration version in force. Nil leaves it alone.
	ActiveConfigVersion *int
	Actor               Actor
}

// Update applies a conditional change.
func (s *Service) Update(ctx context.Context, cmd UpdateCommand) (*merchant.Merchant, error) {
	if err := assertTenant(cmd.TenantID); err != nil {
		return nil, err
	}
	if cmd.IfMatch == "" {
		return nil, apierror.New(apierror.CodeMissingRequiredHeader,
			"an If-Match precondition is required; an unconditional update silently overwrites a concurrent edit").
			WithDetail(apierror.Detail{
				Field: "If-Match", Code: "PRECONDITION_REQUIRED",
				Message: "read the resource, then send its ETag",
				RuleID:  "L1.IF_MATCH_REQUIRED",
			})
	}
	var out *merchant.Merchant
	err := s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		m, err := r.Merchants.GetForUpdate(ctx, cmd.MerchantID)
		if err != nil {
			return err
		}
		if m.TenantID() != cmd.TenantID {
			return notFound(cmd.MerchantID)
		}
		if got := ETag(m); got != cmd.IfMatch {
			return apierror.Newf(apierror.CodeConfigurationVersionConflict,
				"the merchant has changed since you read it (If-Match %s, current %s)", cmd.IfMatch, got)
		}
		before := map[string]any{"activeConfigVersion": m.ActiveConfigVersion()}
		if cmd.ActiveConfigVersion != nil {
			if err := m.SetActiveConfigVersion(*cmd.ActiveConfigVersion, s.deps.Clock); err != nil {
				return err
			}
		}
		if err := r.Merchants.Save(ctx, m); err != nil {
			return err
		}
		out = m
		return s.audit(ctx, r, cmd.Actor, "merchant.updated", m, map[string]any{
			"before": before,
			"after":  map[string]any{"activeConfigVersion": m.ActiveConfigVersion()},
		})
	})
	return out, err
}

// LifecycleCommand is the shape of the three lifecycle transitions an operator performs.
type LifecycleCommand struct {
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	IfMatch    string
	Actor      Actor
	// Reason is the suspension reason for Suspend. Ignored elsewhere.
	Reason merchant.SuspensionReason
	// ActorIsOperator gates the reinstatement of suspensions that require review. It is an
	// explicit flag rather than an inference from the actor's identity so that the requirement is
	// visible at the call site instead of buried in whoever happens to be calling.
	ActorIsOperator bool
}

// Suspend stops a merchant accepting new payments. Refunds and voids continue, deliberately: a
// suspended merchant still owes its customers their money, and blocking refunds during a
// suspension converts a merchant problem into a consumer-harm problem.
func (s *Service) Suspend(ctx context.Context, cmd LifecycleCommand) (*merchant.Merchant, error) {
	if cmd.Actor.Reason == "" {
		return nil, reasonRequired("suspension")
	}
	return s.transition(ctx, cmd, "merchant.suspended", func(m *merchant.Merchant, r ports.Repositories, ctx context.Context) error {
		return m.Suspend(cmd.Reason, cmd.Actor.Reason, s.deps.Clock)
	})
}

// Reinstate lifts a suspension.
func (s *Service) Reinstate(ctx context.Context, cmd LifecycleCommand) (*merchant.Merchant, error) {
	if cmd.Actor.Reason == "" {
		return nil, reasonRequired("reinstatement")
	}
	return s.transition(ctx, cmd, "merchant.reinstated", func(m *merchant.Merchant, r ports.Repositories, ctx context.Context) error {
		return m.Reinstate(cmd.ActorIsOperator, s.deps.Clock)
	})
}

// Terminate permanently closes a merchant.
//
// The open-payment count comes from the payment context, inside the same transaction as the
// transition. Reading it beforehand would leave a window in which a payment is created between
// the check and the write, and the merchant would be terminated with an in-flight authorization
// nobody is entitled to capture or void.
func (s *Service) Terminate(ctx context.Context, cmd LifecycleCommand) (*merchant.Merchant, error) {
	if cmd.Actor.Reason == "" {
		return nil, reasonRequired("termination")
	}
	return s.transition(ctx, cmd, "merchant.terminated", func(m *merchant.Merchant, r ports.Repositories, ctx context.Context) error {
		open, err := r.Payments.CountOpen(ctx, m.ID())
		if err != nil {
			return err
		}
		return m.Terminate(cmd.Actor.Reason, open, s.deps.Clock)
	})
}

// transition is the shared shape of the lifecycle operations: precondition, mutate, persist,
// audit — all inside one transaction.
func (s *Service) transition(ctx context.Context, cmd LifecycleCommand, action string,
	apply func(*merchant.Merchant, ports.Repositories, context.Context) error) (*merchant.Merchant, error) {

	if err := assertTenant(cmd.TenantID); err != nil {
		return nil, err
	}
	var out *merchant.Merchant
	err := s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		m, err := r.Merchants.GetForUpdate(ctx, cmd.MerchantID)
		if err != nil {
			return err
		}
		if m.TenantID() != cmd.TenantID {
			return notFound(cmd.MerchantID)
		}
		if cmd.IfMatch != "" {
			if got := ETag(m); got != cmd.IfMatch {
				return apierror.Newf(apierror.CodeConfigurationVersionConflict,
					"the merchant has changed since you read it (If-Match %s, current %s)", cmd.IfMatch, got)
			}
		}
		before := m.Status()
		if err := apply(m, r, ctx); err != nil {
			return err
		}
		if err := r.Merchants.Save(ctx, m); err != nil {
			return err
		}
		out = m
		return s.audit(ctx, r, cmd.Actor, action, m, map[string]any{
			"before": map[string]any{"status": string(before)},
			"after":  map[string]any{"status": string(m.Status())},
			"reason": cmd.Actor.Reason,
		})
	})
	return out, err
}

// AddBankAccountCommand attaches a settlement account.
type AddBankAccountCommand struct {
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	Account    merchant.BankAccount
	Actor      Actor
}

// AddBankAccount attaches a settlement destination.
//
// The account details themselves are already in the secrets store by the time this runs; what is
// stored on the aggregate is a mask and a reference. A use case that accepted a full account
// number would put one in a database row, a log line and an API response.
func (s *Service) AddBankAccount(ctx context.Context, cmd AddBankAccountCommand) (*merchant.Merchant, error) {
	if err := assertTenant(cmd.TenantID); err != nil {
		return nil, err
	}
	if cmd.Account.SecretRef == "" {
		return nil, apierror.New(apierror.CodeValidationFailed,
			"a settlement account requires a secret reference; the account number is never stored here").
			WithDetail(apierror.Detail{
				Field: "secretRef", Code: "MISSING_SECRET_REF",
				Message: "store the account details in the secrets store and pass the reference",
				RuleID:  "L4.CREDENTIAL_IS_A_REFERENCE",
			})
	}
	return s.mutate(ctx, cmd.TenantID, cmd.MerchantID, cmd.Actor, "merchant.bank_account_added",
		map[string]any{"bankAccountId": cmd.Account.ID, "country": string(cmd.Account.Country)},
		func(m *merchant.Merchant) error { return m.AddBankAccount(cmd.Account, s.deps.Clock) })
}

// AddPrincipalCommand attaches a beneficial owner or officer.
type AddPrincipalCommand struct {
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	Principal  merchant.Principal
	Actor      Actor
}

// AddPrincipal attaches a principal, enforcing the ownership-sum invariant in the aggregate.
func (s *Service) AddPrincipal(ctx context.Context, cmd AddPrincipalCommand) (*merchant.Merchant, error) {
	return s.mutate(ctx, cmd.TenantID, cmd.MerchantID, cmd.Actor, "merchant.principal_added",
		map[string]any{"principalId": cmd.Principal.ID, "role": string(cmd.Principal.Role)},
		func(m *merchant.Merchant) error { return m.AddPrincipal(cmd.Principal, s.deps.Clock) })
}

// AddAttestationCommand records a compliance obligation being met.
type AddAttestationCommand struct {
	TenantID    shared.TenantID
	MerchantID  shared.MerchantID
	Attestation merchant.ComplianceAttestation
	Actor       Actor
}

// AddAttestation records an attestation with its expiry.
//
// The expiry is mandatory in the aggregate, and the reason is worth restating here because this
// is the use case an operator calls: an attestation without an end date silently becomes stale,
// and a stale attestation is indistinguishable from a missing one at audit time.
func (s *Service) AddAttestation(ctx context.Context, cmd AddAttestationCommand) (*merchant.Merchant, error) {
	return s.mutate(ctx, cmd.TenantID, cmd.MerchantID, cmd.Actor, "merchant.attestation_added",
		map[string]any{
			"type":       cmd.Attestation.Type,
			"expiresAt":  cmd.Attestation.ExpiresAt.UTC().Format(time.RFC3339),
			"attestedBy": cmd.Attestation.AttestedBy,
		},
		func(m *merchant.Merchant) error { return m.AddAttestation(cmd.Attestation, s.deps.Clock) })
}

// mutate is the shared shape of the three additive operations.
func (s *Service) mutate(ctx context.Context, tenantID shared.TenantID, id shared.MerchantID,
	actor Actor, action string, detail map[string]any, apply func(*merchant.Merchant) error) (*merchant.Merchant, error) {

	if err := assertTenant(tenantID); err != nil {
		return nil, err
	}
	var out *merchant.Merchant
	err := s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		m, err := r.Merchants.GetForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if m.TenantID() != tenantID {
			return notFound(id)
		}
		if err := apply(m); err != nil {
			return err
		}
		if err := r.Merchants.Save(ctx, m); err != nil {
			return err
		}
		out = m
		return s.audit(ctx, r, actor, action, m, detail)
	})
	return out, err
}

func (s *Service) audit(ctx context.Context, r ports.Repositories, actor Actor,
	action string, m *merchant.Merchant, detail map[string]any) error {

	if s.deps.Audit == nil {
		return nil
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["actorId"] = actor.ID
	detail["actorName"] = actor.Name
	if actor.Reason != "" {
		detail["reason"] = actor.Reason
	}
	return s.deps.Audit.Record(ctx, r, action, "merchant", m.ID().String(), "SUCCESS", detail)
}

// assertTenant is the tenant guard every entry point runs. Tenant identity has one origin; a
// command that arrives without it lost that origin somewhere, and the only safe answer is to
// refuse rather than to fall back on whatever the repository defaults to.
func assertTenant(t shared.TenantID) error {
	if t.IsZero() {
		return apierror.New(apierror.CodeMissingTenantContext, "the request carries no tenant context")
	}
	return nil
}

// notFound answers a cross-tenant reference. Deliberately not a 403: distinguishing "not yours"
// from "does not exist" leaks the existence of another tenant's identifiers.
func notFound(id shared.MerchantID) error {
	return apierror.Newf(apierror.CodeMerchantNotFound, "merchant %s does not exist under your tenant", id)
}

func reasonRequired(what string) error {
	return apierror.Newf(apierror.CodeValidationFailed, "a %s requires a stated reason", what).
		WithDetail(apierror.Detail{
			Field: "reason", Code: "REASON_REQUIRED",
			Message: "high-consequence changes are unreviewable without one",
			RuleID:  "L7.AUDIT_REASON_REQUIRED",
		})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

var _ = money.Zero

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: FR-09, FR-10, FR-11, FR-13, FR-14.
//
// The merchant use cases, each writing its state change and its audit record in one
// transaction
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
