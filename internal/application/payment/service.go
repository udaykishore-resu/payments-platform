package payment

import (
	"context"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/routing"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// PaymentValidator is validation level L5, applied before dispatch.
//
// Declared as an interface here rather than calling the rule set directly so that the
// orchestrator's control flow is readable without the forty-field subject construction in the
// middle of it, and so that a use-case test can exercise the sequencing without building a
// complete validation subject. The production implementation assembles the L5 subject and runs
// the rule set; it is wired in the composition root.
type PaymentValidator interface {
	ValidateCreate(ctx context.Context, cmd CreateCommand, m MerchantSnapshot) error
	ValidateCapture(ctx context.Context, p *payment.Payment, amount money.Money, m MerchantSnapshot) error
	ValidateRefund(ctx context.Context, p *payment.Payment, amount money.Money, m MerchantSnapshot) error
	ValidateVoid(ctx context.Context, p *payment.Payment, m MerchantSnapshot) error
	// ValidateGatewayResponse is level L6. It runs after every gateway call and before the
	// state transition, so a gateway that echoes the wrong amount or omits a transaction
	// reference is caught here rather than in a reconciliation report next month.
	ValidateGatewayResponse(ctx context.Context, r GatewayResponse, expected ExpectedResponse) error
}

// ExpectedResponse is what L6 checks the gateway's answer against.
//
// It carries the payment's *current* position as well as the dispatched amount, because two of
// the level's rules are about reachability rather than about the response in isolation: a
// response that maps to a state the payment cannot reach from where it is, and a success arriving
// for a payment already in a terminal failure state, are both descriptions of a world that
// disagrees with ours. Neither is expressible without the current state, and applying either
// would be the moment the platform and the issuer start telling the customer different stories.
type ExpectedResponse struct {
	Amount    money.Money
	Currency  money.Currency
	Operation shared.Operation
	GatewayID shared.GatewayID
	PaymentID shared.PaymentID
	AttemptID shared.AttemptID
	// CurrentState is where the payment stands before the response is applied.
	CurrentState payment.State
	// AuthorizedAmount and CapturedTotal are the running totals the amount-consistency rules
	// compare a capture or refund against.
	AuthorizedAmount money.Money
	CapturedTotal    money.Money
}

// Service is the payment use-case facade. It is the only thing the transport layer calls.
type Service struct {
	deps      Deps
	validator PaymentValidator
	orch      *Orchestrator
}

// NewService wires the payment use cases.
func NewService(d Deps, v PaymentValidator) *Service {
	if d.Settings.MaxAttempts <= 0 {
		d.Settings = DefaultConfig()
	}
	return &Service{deps: d, validator: v, orch: NewOrchestrator(d, v)}
}

// CreateCommand is the input to creating a payment.
type CreateCommand struct {
	TenantID       shared.TenantID
	MerchantID     shared.MerchantID
	Amount         money.Money
	Method         shared.PaymentMethod
	MethodRef      payment.PaymentMethodReference
	CaptureMethod  payment.CaptureMethod
	Description    string
	StatementRef   string
	Reference      string
	Metadata       map[string]string
	Customer       payment.CustomerReference
	ReturnURL      string
	IdempotencyKey string
	CorrelationID  string
	RequestID      string
}

// Result is the outcome of a payment operation, shaped for the transport layer.
type Result struct {
	Payment *payment.Payment
	Plan    *routing.Plan
	Risk    risk.Decision
	// NextAction is populated when the payer must complete an out-of-band step.
	NextAction *payment.NextAction
	// Degraded records that the request was served with stale configuration or a fallen-back
	// risk check. Surfaced so that "we processed forty minutes of traffic degraded" is a fact
	// in the response record rather than an inference from a dashboard.
	Degraded bool
}

// Create runs the full create-payment pipeline: baseline §12 stages 9 through 17.
//
// Stages 1–8 (authentication, tenant resolution, authorization, rate limiting, L1 validation
// and the idempotency claim) happen in the transport layer, because they must run before a
// request body is even parsed into a command and because the idempotency claim must own the
// response snapshot for the whole handler.
//
// The sequencing below is the load-bearing part. Read it as a list of things that must be true
// before the next thing happens:
//
//	merchant context   → we know what this merchant is allowed to do
//	L5 validation      → the request is permissible under that
//	risk               → the request is advisable, and whether it needs 3DS
//	routing            → we know where it should go and in what order
//	persist            → the intent exists durably before anything external hears about it
//	dispatch           → the gateway is called, with the attempt already committed
func (s *Service) Create(ctx context.Context, cmd CreateCommand) (*Result, error) {
	started := s.deps.Clock.Now()
	if err := assertTenant(cmd.TenantID, cmd.TenantID); err != nil {
		return nil, err
	}
	// Every stage below reads the merchant context; the memo makes them read the *same* one, so
	// a configuration publish landing mid-request cannot validate a payment under one version and
	// route it under another.
	ctx = WithRequestCache(ctx)

	m, err := s.deps.Merchants.Load(ctx, cmd.MerchantID)
	if err != nil {
		return nil, err
	}
	if err := assertTenant(cmd.TenantID, m.TenantID); err != nil {
		return nil, err
	}
	if err := s.assertCanTransact(m); err != nil {
		return nil, err
	}
	s.deps.Metrics.ObserveStage("merchant_context", s.deps.Clock.Now().Sub(started))

	if err := s.validator.ValidateCreate(ctx, cmd, m); err != nil {
		return nil, err
	}

	// The payment aggregate is constructed before risk and routing so that both have a stable
	// payment ID to key their decisions and their audit records on. It is not persisted yet:
	// a payment that risk declines should never have existed, and creating a row first would
	// leave the merchant's dashboard full of phantom failures.
	pay, err := payment.New(payment.NewPaymentParams{
		TenantID:       cmd.TenantID,
		MerchantID:     cmd.MerchantID,
		Amount:         cmd.Amount,
		PaymentMethod:  cmd.Method,
		MethodRef:      cmd.MethodRef,
		CaptureMethod:  cmd.CaptureMethod,
		Description:    cmd.Description,
		StatementRef:   cmd.StatementRef,
		Metadata:       cmd.Metadata,
		Customer:       cmd.Customer,
		IdempotencyKey: cmd.IdempotencyKey,
		CorrelationID:  cmd.CorrelationID,
	}, s.deps.Clock)
	if err != nil {
		return nil, err
	}

	riskStart := s.deps.Clock.Now()
	decision, err := s.deps.Risk.Evaluate(ctx, RiskInput{
		Policy: m.Risk, TenantID: cmd.TenantID, MerchantID: cmd.MerchantID,
		PaymentID: pay.ID(), Amount: cmd.Amount, Method: cmd.Method,
		MethodRef: cmd.MethodRef, Customer: cmd.Customer, Merchant: m,
		Now: s.deps.Clock.Now(),
	})
	if err != nil {
		// A risk *engine* failure is not a risk *decision*. It is infrastructure, and it is
		// retryable — as distinct from the engine successfully deciding to decline.
		return nil, apierror.Wrap(err, apierror.CodeDependencyFailure, "risk evaluation failed")
	}
	s.deps.Metrics.ObserveStage("risk", s.deps.Clock.Now().Sub(riskStart))

	if decision.Outcome == risk.OutcomeDecline {
		return nil, riskDeclineError(decision)
	}

	req := routing.RequestContext{
		TenantID: cmd.TenantID, MerchantID: cmd.MerchantID, PaymentID: pay.ID(),
		Amount: cmd.Amount, PaymentMethod: cmd.Method,
		PayerCountry: cmd.Customer.Country, MerchantCountry: m.Country,
		RiskBand:        riskBand(decision),
		ThreeDSRequired: decision.Require3DS,
		Operation:       shared.OpAuthorize,
	}
	candidates, err := s.deps.Candidates.Build(ctx, req, m)
	if err != nil {
		return nil, err
	}
	plan, err := routing.Decide(m.Routing, req, candidates, s.deps.Clock.Now())
	if err != nil {
		return nil, err
	}
	for _, sel := range plan.Selections() {
		s.deps.Metrics.RecordRoutingDecision(sel.GatewayID, sel.Reason)
	}

	// Persist the intent and the plan in one transaction, before any external system is told
	// the payment exists. Everything after this point is recoverable, because the record of
	// what we intended survives a crash.
	if err := s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		if err := r.Payments.Create(ctx, pay); err != nil {
			return err
		}
		return s.deps.Audit.Record(ctx, r, "payment.created", "payment", pay.ID().String(), "SUCCESS",
			map[string]any{"amount": pay.Amount().Amount(), "currency": string(pay.Currency()),
				"routingPlan": plan.ID.String(), "riskOutcome": string(decision.Outcome)})
	}); err != nil {
		return nil, err
	}

	res, err := s.orch.Dispatch(ctx, DispatchInput{
		Payment: pay, Plan: plan, Merchant: m, Risk: decision,
		Operation: shared.OpAuthorize, ReturnURL: cmd.ReturnURL,
	})
	if err != nil {
		return nil, err
	}
	res.Risk = decision
	res.Degraded = decision.Degraded || m.SnapshotAge > 0
	return res, nil
}

// Get loads a payment.
func (s *Service) Get(ctx context.Context, id shared.PaymentID) (*payment.Payment, error) {
	var out *payment.Payment
	err := s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		p, err := r.Payments.Get(ctx, id)
		out = p
		return err
	})
	return out, err
}

// List returns a page of payments.
func (s *Service) List(ctx context.Context, f ports.PaymentFilter, page ports.Page) ([]*payment.Payment, string, error) {
	var out []*payment.Payment
	var cursor string
	err := s.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		var err error
		out, cursor, err = r.Payments.List(ctx, f, page)
		return err
	})
	return out, cursor, err
}

// CaptureCommand converts a hold into a debit.
type CaptureCommand struct {
	// TenantID is the tenant the authenticated principal resolved to. It is asserted against the
	// payment's own tenant rather than used to scope the lookup, because the repository already
	// scopes by row-level security: this field exists to catch the case where the two disagree,
	// which is an isolation defect and not a not-found.
	TenantID       shared.TenantID
	PaymentID      shared.PaymentID
	Amount         *money.Money // nil means capture the full authorized amount
	Final          bool
	IdempotencyKey string
	CorrelationID  string
}

// Capture converts an authorization into a debit.
//
// Note what this does *not* do: it does not route. A capture must go to the gateway holding the
// authorization, and nowhere else. Routing a capture would be meaningless at best and, at a
// gateway that happened to accept it, a second charge at worst.
func (s *Service) Capture(ctx context.Context, cmd CaptureCommand) (*Result, error) {
	ctx = WithRequestCache(ctx)
	p, m, err := s.loadForMutation(ctx, cmd.PaymentID)
	if err != nil {
		return nil, err
	}
	if err := assertTenant(cmd.TenantID, p.TenantID()); err != nil {
		return nil, err
	}
	amount := p.CapturableAmount()
	if cmd.Amount != nil {
		amount = *cmd.Amount
	}
	if err := s.validator.ValidateCapture(ctx, p, amount, m); err != nil {
		return nil, err
	}
	if !p.PaymentMethod().SupportsSeparateCapture() {
		return nil, apierror.Newf(apierror.CodeInvalidStateTransition,
			"payment method %s settles in a single step and cannot be captured separately", p.PaymentMethod())
	}
	return s.orch.CaptureExisting(ctx, p, m, amount, cmd.Final)
}

// RefundCommand returns captured funds.
type RefundCommand struct {
	TenantID       shared.TenantID
	PaymentID      shared.PaymentID
	Amount         *money.Money // nil means refund the full refundable balance
	Reason         payment.RefundReason
	IdempotencyKey string
	CorrelationID  string
}

// Refund returns funds to the payer.
//
// Refunds are permitted for merchants that may no longer accept payments — a suspended merchant
// still owes its customers their money, and blocking refunds during a suspension converts a
// merchant problem into a consumer-harm problem. See merchant.Status.CanIssueRefunds.
func (s *Service) Refund(ctx context.Context, cmd RefundCommand) (*Result, error) {
	ctx = WithRequestCache(ctx)
	p, m, err := s.loadForMutationAllowingSuspended(ctx, cmd.PaymentID)
	if err != nil {
		return nil, err
	}
	if err := assertTenant(cmd.TenantID, p.TenantID()); err != nil {
		return nil, err
	}
	amount := p.RefundableAmount()
	if cmd.Amount != nil {
		amount = *cmd.Amount
	}
	if err := s.validator.ValidateRefund(ctx, p, amount, m); err != nil {
		return nil, err
	}
	return s.orch.RefundExisting(ctx, p, m, amount, cmd.Reason, cmd.IdempotencyKey)
}

// VoidCommand releases an uncaptured authorization.
type VoidCommand struct {
	TenantID       shared.TenantID
	PaymentID      shared.PaymentID
	IdempotencyKey string
	CorrelationID  string
}

// Void releases a hold. Like refunds, permitted while suspended.
func (s *Service) Void(ctx context.Context, cmd VoidCommand) (*Result, error) {
	ctx = WithRequestCache(ctx)
	p, m, err := s.loadForMutationAllowingSuspended(ctx, cmd.PaymentID)
	if err != nil {
		return nil, err
	}
	if err := assertTenant(cmd.TenantID, p.TenantID()); err != nil {
		return nil, err
	}
	if err := s.validator.ValidateVoid(ctx, p, m); err != nil {
		return nil, err
	}
	return s.orch.VoidExisting(ctx, p, m)
}

// loadForMutation reads a payment and its merchant context, requiring the merchant to be able
// to transact.
func (s *Service) loadForMutation(ctx context.Context, id shared.PaymentID) (*payment.Payment, MerchantSnapshot, error) {
	p, err := s.Get(ctx, id)
	if err != nil {
		return nil, MerchantSnapshot{}, err
	}
	m, err := s.deps.Merchants.Load(ctx, p.MerchantID())
	if err != nil {
		return nil, MerchantSnapshot{}, err
	}
	if err := s.assertCanTransact(m); err != nil {
		return nil, MerchantSnapshot{}, err
	}
	return p, m, nil
}

// loadForMutationAllowingSuspended is the money-out counterpart: it deliberately does not
// require the merchant to be able to accept new payments.
func (s *Service) loadForMutationAllowingSuspended(ctx context.Context, id shared.PaymentID) (*payment.Payment, MerchantSnapshot, error) {
	p, err := s.Get(ctx, id)
	if err != nil {
		return nil, MerchantSnapshot{}, err
	}
	m, err := s.deps.Merchants.Load(ctx, p.MerchantID())
	if err != nil {
		return nil, MerchantSnapshot{}, err
	}
	return p, m, nil
}

func (s *Service) assertCanTransact(m MerchantSnapshot) error {
	// The lifecycle check comes first: a suspended merchant with three healthy connections must
	// be told they are suspended, not told they have no gateway. Reporting the wrong one sends
	// their engineer to look at a configuration that is fine.
	if m.Status != "" && !m.Status.CanAcceptPayments() {
		return merchantStatusError(m.Status, "")
	}
	if len(m.Connections) == 0 {
		return apierror.New(apierror.CodeGatewayNotConfigured,
			"the merchant has no certified gateway connection")
	}
	return nil
}

// assertTenant is the tenant guard every entry point runs.
//
// It is a comparison against the aggregate rather than a lookup, and that is the point: tenant
// identity has one origin (the authenticated principal, baseline §16.2), the command carries the
// tenant that origin produced, and this function's only job is to refuse the case where the two
// disagree. A guard that *derived* the tenant from the resource would be no guard at all.
func assertTenant(want shared.TenantID, got shared.TenantID) error {
	if want.IsZero() {
		return apierror.New(apierror.CodeMissingTenantContext,
			"the request carries no tenant context")
	}
	if got.IsZero() || want == got {
		return nil
	}
	// Deliberately not a 403: distinguishing "not yours" from "does not exist" leaks the
	// existence of another tenant's identifiers to anyone who can guess one.
	return apierror.New(apierror.CodePaymentNotFound, "the resource does not exist under your tenant")
}

// riskBand coarsens a risk decision for the routing engine, which needs a band rather than a
// score: routing rules are authored by humans in terms of "high risk goes to the gateway with
// better fraud tooling", not in terms of a numeric threshold that shifts when the model is
// retrained.
func riskBand(d risk.Decision) routing.RiskBand {
	switch {
	case d.Score >= 70 || d.Outcome == risk.OutcomeReview:
		return routing.RiskBandHigh
	case d.Score >= 35 || d.Require3DS:
		return routing.RiskBandMedium
	default:
		return routing.RiskBandLow
	}
}

// riskDeclineError renders a risk decline as a caller-facing error carrying the checks that
// fired — without leaking the thresholds, which would let an attacker binary-search the
// merchant's limits.
func riskDeclineError(d risk.Decision) error {
	e := apierror.New(apierror.CodeRiskDeclined, "the payment was declined")
	for _, r := range d.Reasons {
		if r.Severity == risk.SeverityCritical {
			e = e.WithDetail(apierror.Detail{
				Code:    string(r.Check),
				Message: "declined by risk policy",
				RuleID:  string(r.Check),
			})
		}
	}
	return e
}

// merchantStatusError is used by the context loader; kept here so the mapping from merchant
// state to API error lives beside the other error mappings.
func merchantStatusError(st merchant.Status, reason merchant.SuspensionReason) error {
	switch st {
	case merchant.StatusSuspended:
		return apierror.Newf(apierror.CodeMerchantSuspended, "merchant is suspended (%s)", reason)
	case merchant.StatusTerminated:
		return apierror.New(apierror.CodeMerchantNotActive, "merchant is terminated")
	default:
		return apierror.Newf(apierror.CodeMerchantNotActive,
			"merchant is in state %s and cannot accept payments", st)
	}
}

// staleConfigError is the fail-static cliff from baseline §15: beyond the maximum tolerated
// staleness the data plane stops accepting *new* merchants rather than either guessing or
// stopping entirely.
func staleConfigError(age, limit time.Duration) error {
	return apierror.Newf(apierror.CodeConfigurationStale,
		"the local configuration snapshot is %s old, beyond the %s tolerance", age.Round(time.Second), limit).
		WithRetryAfter(30)
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-20, FR-53, FR-59, FR-69, FR-70, FR-71, FR-72.
//
// The payment use cases — create, capture, refund, void, read — each one tenant-scoped and
// idempotent at the boundary
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
