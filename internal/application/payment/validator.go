package payment

import (
	"context"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/validation/engine"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/l5payment"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/l6response"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// ValidatorDeps is what the production validator needs to build its subjects.
//
// Note what is not here: a repository, an HTTP client, a cache handle. The rule sets are pure by
// construction and are evaluated inside a 5 ms budget; everything impure happens in this file,
// before Evaluate is called, in one pipelined read. Putting a client behind the rule set would
// make the budget a property of the network rather than of the code.
type ValidatorDeps struct {
	// Velocity supplies the rolling counters. Nil is legal and means "no counter store", which
	// degrades the count-based checks to the risk policy's posture rather than to a pass.
	Velocity ports.VelocityCounter
	// Principals resolves the authenticated caller, whose scopes L5 checks. Nil means the
	// scope rules cannot be satisfied and every mutating operation is refused — the fail-closed
	// direction, because an unauthenticated caller must not be treated as one with no
	// restrictions.
	Principals Principals
	Clock      shared.Clock
	// L5 and L6 are the levels' tunables. Zero values are replaced with the platform defaults so
	// that a partially-wired validator still enforces the documented behaviour.
	L5 l5payment.Deps
	L6 l6response.Deps
}

// Validator is the production PaymentValidator: it assembles the L5 and L6 subjects from the
// command, the merchant snapshot and the pre-read counters, runs the rule sets, and converts the
// report into the platform's single error shape.
//
// It is a separate type from the service for the reason the interface exists at all: the
// forty-field subject construction below would otherwise sit in the middle of the orchestrator's
// control flow, where it would hide the sequencing that is the actual product of that file.
type Validator struct {
	deps ValidatorDeps
	l5   l5rules
	l6   l6rules
}

// l5rules and l6rules hold the compiled rule sets, built once at construction. A RuleSet is a
// value with no per-request state, so sharing one across goroutines is safe and rebuilding it
// per request would be pure waste on the hottest path in the platform.
type l5rules struct {
	set engine.RuleSet[l5payment.Subject]
}

type l6rules struct {
	set engine.RuleSet[l6response.Subject]
}

// NewValidator compiles the rule sets and returns the production validator.
func NewValidator(d ValidatorDeps) *Validator {
	if d.Clock == nil {
		d.Clock = shared.SystemClock{}
	}
	if d.L5.MaxConfigStaleness == 0 {
		d.L5 = mergeL5Defaults(d.L5)
	}
	if len(d.L6.AllowedOutcomes) == 0 {
		d.L6 = l6response.DefaultDeps()
	}
	return &Validator{deps: d, l5: l5rules{set: l5payment.Rules(d.L5)}, l6: l6rules{set: l6response.Rules(d.L6)}}
}

// mergeL5Defaults fills the platform defaults for the fields a caller left unset.
//
// A merge rather than an all-or-nothing substitution: a deployment that wants a different
// sanctions list must not thereby lose the scope table, and a caller who set only the scope
// table must not thereby run with a zero staleness ceiling — which would fail every request.
func mergeL5Defaults(in l5payment.Deps) l5payment.Deps {
	def := l5payment.DefaultDeps()
	if in.MaxConfigStaleness == 0 {
		in.MaxConfigStaleness = def.MaxConfigStaleness
	}
	if in.RiskDeclineAt == 0 {
		in.RiskDeclineAt = def.RiskDeclineAt
	}
	if len(in.ScopeForOperation) == 0 {
		in.ScopeForOperation = def.ScopeForOperation
	}
	if in.ElevatedRefundScope == "" {
		in.ElevatedRefundScope = def.ElevatedRefundScope
	}
	if in.AuthValidityDays == 0 {
		in.AuthValidityDays = def.AuthValidityDays
	}
	if in.CustomerDailyLimit == 0 {
		in.CustomerDailyLimit = def.CustomerDailyLimit
	}
	if in.DistinctCardsPerCustomerHour == 0 {
		in.DistinctCardsPerCustomerHour = def.DistinctCardsPerCustomerHour
	}
	if in.DeclineRatioMinAttempts == 0 {
		in.DeclineRatioMinAttempts = def.DeclineRatioMinAttempts
	}
	if in.DeclineRatioPercent == 0 {
		in.DeclineRatioPercent = def.DeclineRatioPercent
	}
	if len(in.ClaimableExemptions) == 0 {
		in.ClaimableExemptions = def.ClaimableExemptions
	}
	return in
}

// ValidateCreate runs L5 over a create-payment command.
func (v *Validator) ValidateCreate(ctx context.Context, cmd CreateCommand, m MerchantSnapshot) error {
	keys := KeysFor(cmd.TenantID, cmd.MerchantID, cmd.MethodRef, cmd.Customer)
	readout := ReadVelocity(ctx, v.deps.Velocity, keys, cmd.Amount.Currency())

	s := v.baseSubject(ctx, l5payment.OpCreate, m, readout)
	s.Request = l5payment.Request{
		Amount:          cmd.Amount,
		Method:          cmd.Method,
		CustomerCountry: cmd.Customer.Country,
		IPCountry:       cmd.Customer.Country,
		TokenRef:        cmd.MethodRef.Token,
		Token: l5payment.TokenMeta{
			Present:         cmd.MethodRef.Token != "",
			OwnerMerchantID: cmd.MerchantID,
			CardExpiryMonth: cmd.MethodRef.ExpMonth,
			CardExpiryYear:  cmd.MethodRef.ExpYear,
			Fingerprint:     keys.Fingerprint,
		},
		CaptureMode:         defaultCapture(cmd.CaptureMethod),
		StatementDescriptor: cmd.StatementRef,
		Metadata:            cmd.Metadata,
		CustomerRef:         cmd.Customer.MerchantCustomerID,
		IdempotencyKey:      cmd.IdempotencyKey,
	}
	return v.run5(ctx, s)
}

// ValidateCapture runs L5 over a capture against an existing payment.
func (v *Validator) ValidateCapture(ctx context.Context, p *payment.Payment, amount money.Money, m MerchantSnapshot) error {
	return v.validateExisting(ctx, l5payment.OpCapture, p, amount, m)
}

// ValidateRefund runs L5 over a refund.
func (v *Validator) ValidateRefund(ctx context.Context, p *payment.Payment, amount money.Money, m MerchantSnapshot) error {
	return v.validateExisting(ctx, l5payment.OpRefund, p, amount, m)
}

// ValidateVoid runs L5 over a void. The amount is the payment's own, because a void is
// all-or-nothing and there is no partial void anywhere in the platform.
func (v *Validator) ValidateVoid(ctx context.Context, p *payment.Payment, m MerchantSnapshot) error {
	return v.validateExisting(ctx, l5payment.OpVoid, p, p.Amount(), m)
}

// validateExisting is the shared shape of capture, refund and void: the same subject with a
// PaymentView attached and the operation switched.
func (v *Validator) validateExisting(ctx context.Context, op l5payment.Operation,
	p *payment.Payment, amount money.Money, m MerchantSnapshot) error {

	keys := KeysFor(p.TenantID(), p.MerchantID(), p.MethodRef(), p.Customer())
	// Money-out operations do not consult velocity: the counters bound how much a merchant may
	// *take*, and applying them to refunds would stop a merchant returning money at exactly the
	// moment their traffic is anomalous. The readout is still taken so the subject is complete
	// and the report is reproducible.
	readout := Readout{
		PerMinute: risk.KnownCount(0), CardHour: risk.KnownCount(0), CustomerDay: risk.KnownCount(0),
		DistinctCards: risk.KnownCount(0), Attempts15Min: risk.KnownCount(0),
		Declines15Min: risk.KnownCount(0),
		VolumeToday:   risk.KnownVolume(money.Zero(amount.Currency())),
	}

	s := v.baseSubject(ctx, op, m, readout)
	s.Request = l5payment.Request{
		Amount:      amount,
		Method:      p.PaymentMethod(),
		CaptureMode: p.CaptureMethod(),
		CustomerRef: p.Customer().MerchantCustomerID,
		TokenRef:    p.MethodRef().Token,
	}
	view := l5payment.PaymentView{
		Found:            true,
		State:            p.State(),
		Currency:         p.Currency(),
		AuthorizedAmount: p.AuthorizedAmount(),
		CapturedTotal:    p.CapturedAmount(),
		RefundedTotal:    p.RefundedAmount(),
		CaptureCount:     captureCount(p),
	}
	if t := p.AuthorizedAt(); t != nil {
		view.AuthorizedAt = *t
	}
	if t := p.CapturedAt(); t != nil {
		view.CapturedAt = *t
	}
	s.Payment = &view
	_ = keys
	return v.run5(ctx, s)
}

// baseSubject builds the parts of the L5 subject that do not depend on the operation.
//
// The idempotency block is deliberately left empty, and that is the one non-obvious decision in
// this file. The claim was made in the transport layer at stage 8, *before* the request body was
// parsed into a command, because the claim must own the response snapshot for the whole handler
// (baseline §14.3). By the time this runs, the caller provably owns the operation. Re-deriving
// the record here would either re-read it — a round trip inside a 5 ms budget for an answer we
// already have — or reconstruct it from the command and compare it against itself.
func (v *Validator) baseSubject(ctx context.Context, op l5payment.Operation,
	m MerchantSnapshot, readout Readout) l5payment.Subject {

	principalID, scopes := v.principal(ctx)
	return l5payment.Subject{
		Op: op,
		Merchant: l5payment.MerchantSnapshot{
			Found:  !m.MerchantID.IsZero(),
			ID:     m.MerchantID,
			Status: m.Status,
		},
		Config:    suppressUnavailable(configSnapshot(m), readout),
		Velocity:  l5Counters(readout, m),
		Principal: l5payment.PrincipalView{ID: principalID, Scopes: scopes},
		Now:       v.deps.Clock.Now(),
	}
}

// principal resolves the caller. A missing resolver or a missing principal yields no scopes,
// which fails L5.OPERATION_SCOPE_AUTHORIZED — the fail-closed direction.
func (v *Validator) principal(ctx context.Context) (string, []string) {
	if v.deps.Principals == nil {
		return "", nil
	}
	id, scopes, ok := v.deps.Principals.FromContext(ctx)
	if !ok {
		return "", nil
	}
	return id, scopes
}

// configSnapshot flattens the merchant snapshot into the shape L5 evaluates.
func configSnapshot(m MerchantSnapshot) l5payment.ConfigSnapshot {
	cands := make([]l5payment.RouteCandidate, 0, len(m.RoutableCombinations))
	for _, c := range m.RoutableCombinations {
		cands = append(cands, l5payment.RouteCandidate{Method: c.Method, Currency: c.Currency, Country: c.Country})
	}
	return l5payment.ConfigSnapshot{
		Present:              m.ConfigPresent,
		Version:              int64(m.ConfigVersion),
		Age:                  m.SnapshotAge,
		Currencies:           m.SupportedCurrencies,
		Methods:              m.PaymentMethods,
		Countries:            m.SupportedCountries,
		BlockedCountries:     m.Risk.BlockedCountries,
		MaxTransactionAmount: m.Risk.MaxTransactionAmount,
		Require3DSAbove:      m.Risk.Require3DSAbove,
		DailyVolumeLimit:     m.Risk.DailyVolumeLimit,
		MaxRefundWindowDays:  int(m.MaxRefundWindow / (24 * time.Hour)),
		MaxPartialCaptures:   m.MaxPartialCaptures,
		MaxPaymentsPerMinute: m.Risk.Velocity.MaxPaymentsPerMinute,
		MaxPerCardPerHour:    m.Risk.Velocity.MaxPerCardPerHour,
		MaxPerCustomerPerDay: m.Risk.Velocity.MaxPerCustomerPerDay,
		MaxDistinctCards:     m.Risk.MaxCardsPerCustomerPerDay,
		ManualCaptureAllowed: m.ManualCaptureAllowed,
		Candidates:           cands,
		RiskDeclineAt:        m.Risk.DeclineScoreAtOrAbove,
	}
}

// l5Counters renders the readout as L5's plain-integer counters.
//
// Where a counter was unavailable the value is left at zero *and* the caller has already zeroed
// the matching limit in configSnapshot's copy — see the note below. A zero limit makes the
// rule's precondition false, so the outcome is recorded as skipped rather than passed, and the
// decision moves to the risk engine, which models unavailability explicitly.
func l5Counters(r Readout, m MerchantSnapshot) l5payment.VelocityCounters {
	out := l5payment.VelocityCounters{TodayVolume: money.Money{}}
	if r.PerMinute.IsAvailable() {
		out.CountLastMinute = int(r.PerMinute.Value())
	}
	if r.CardHour.IsAvailable() {
		out.CountForFingerprintLastHour = int(r.CardHour.Value())
	}
	if r.CustomerDay.IsAvailable() {
		out.CountForCustomerToday = int(r.CustomerDay.Value())
	}
	if r.DistinctCards.IsAvailable() {
		out.DistinctFingerprintsLastHour = int(r.DistinctCards.Value())
	}
	if r.Attempts15Min.IsAvailable() {
		out.AttemptsLast15Min = int(r.Attempts15Min.Value())
	}
	if r.Declines15Min.IsAvailable() {
		out.DeclinesLast15Min = int(r.Declines15Min.Value())
	}
	if r.VolumeToday.IsAvailable() {
		out.TodayVolume = r.VolumeToday.Amount()
	} else {
		out.TodayVolume = money.Zero(m.Risk.DailyVolumeLimit.Currency())
	}
	return out
}

// suppressUnavailable zeroes the limits whose counters could not be read, so that the
// corresponding L5 rule does not run at all rather than running against a zero it would pass.
//
// This is the mechanism referred to in Readout's doc comment, and it is the reason an unreadable
// counter cannot silently become a permissive answer anywhere in the platform.
func suppressUnavailable(c l5payment.ConfigSnapshot, r Readout) l5payment.ConfigSnapshot {
	if !r.PerMinute.IsAvailable() {
		c.MaxPaymentsPerMinute = 0
	}
	if !r.CardHour.IsAvailable() {
		c.MaxPerCardPerHour = 0
	}
	if !r.CustomerDay.IsAvailable() {
		c.MaxPerCustomerPerDay = 0
	}
	if !r.DistinctCards.IsAvailable() {
		c.MaxDistinctCards = 0
	}
	if !r.VolumeToday.IsAvailable() {
		c.DailyVolumeLimit = money.Money{}
	}
	return c
}

// run5 evaluates the L5 set and converts the report.
func (v *Validator) run5(ctx context.Context, s l5payment.Subject) error {
	rep := v.l5.set.Evaluate(ctx, s)
	if err := rep.AsError(); err != nil {
		return err
	}
	return nil
}

// ValidateGatewayResponse is level L6.
//
// It runs after every gateway call and before the state transition. The conversion at the end is
// the same Report.AsError used by L5, which is what makes a contract violation and a validation
// failure the same shape to a caller — but the *consequence* differs entirely, and that
// difference is the orchestrator's to apply: an L5 failure rejects the request, whereas an L6
// failure parks the payment as unknown, because a response we cannot trust is a statement about
// our knowledge and not about the payer's funds.
func (v *Validator) ValidateGatewayResponse(ctx context.Context, r GatewayResponse, expected ExpectedResponse) error {
	n := l6response.Normalized{
		Parsed:         true,
		SchemaComplete: true,
		GatewayStatus:  r.RawStatus,
		StatusMappable: true,
		Outcome:        normalizeOutcome(r.Status),
		TransactionID:  r.GatewayRef,
		EchoedAmount:   expected.Amount,
		EchoedCurrency: expected.Currency,
		DeclineReason:  r.DeclineReason,
		MappedState:    mappedState(r.Status),
	}
	if r.AuthorizedAmount != nil {
		n.EchoedAmount = *r.AuthorizedAmount
		n.EchoedCurrency = r.AuthorizedAmount.Currency()
	}
	if r.CapturedAmount != nil {
		n.CapturedTotal = *r.CapturedAmount
		if expected.Operation == shared.OpCapture {
			n.EchoedAmount = *r.CapturedAmount
			n.EchoedCurrency = r.CapturedAmount.Currency()
		}
	}
	if n.Outcome == l6response.OutcomeUnknown {
		// An unmapped status is the one case the adapter is *not* permitted to shrug at: it
		// means the vendor described a world the platform has no state for.
		n.StatusMappable = false
	}

	s := l6response.Subject{
		Kind: l6response.KindResponse,
		Attempt: l6response.Attempt{
			Operation:        expected.Operation,
			DispatchedAmount: expected.Amount,
		},
		Payment: l6response.PaymentState{
			Present:          expected.CurrentState != "",
			Current:          expected.CurrentState,
			AuthorizedAmount: expected.AuthorizedAmount,
			CapturedTotal:    expected.CapturedTotal,
		},
		Normalized: n,
		Now:        v.deps.Clock.Now(),
	}
	rep := v.l6.set.Evaluate(ctx, s)
	if err := rep.AsError(); err != nil {
		return err
	}
	return nil
}

// normalizeOutcome maps the SPI's status onto L6's normalized outcome. An unrecognised status
// maps to OutcomeUnknown, which fails STATUS_IS_MAPPABLE — deliberately, because the alternative
// is inventing a meaning for something the adapter did not translate.
func normalizeOutcome(s spi.Status) l6response.Outcome {
	switch s {
	case spi.StatusAuthorized, spi.StatusCaptured, spi.StatusVoided,
		spi.StatusRefunded, spi.StatusRefundAccepted:
		return l6response.OutcomeSuccess
	case spi.StatusDeclined:
		return l6response.OutcomeDeclined
	case spi.StatusRequiresAction:
		return l6response.OutcomeRequiresAction
	case spi.StatusPending:
		return l6response.OutcomePending
	case spi.StatusFailed:
		return l6response.OutcomeHardError
	default:
		return l6response.OutcomeUnknown
	}
}

// mappedState is the payment state the response implies, where it implies one.
func mappedState(s spi.Status) payment.State {
	switch s {
	case spi.StatusAuthorized:
		return payment.StateAuthorized
	case spi.StatusCaptured:
		return payment.StateCaptured
	case spi.StatusVoided:
		return payment.StateVoided
	case spi.StatusRefunded:
		return payment.StateRefunded
	case spi.StatusRequiresAction:
		return payment.StateRequiresAction
	case spi.StatusPending:
		return payment.StatePending
	case spi.StatusDeclined:
		return payment.StateFailed
	default:
		return ""
	}
}

func defaultCapture(c payment.CaptureMethod) payment.CaptureMethod {
	if c == "" {
		return payment.CaptureAutomatic
	}
	return c
}

func captureCount(p *payment.Payment) int {
	n := 0
	for _, a := range p.Attempts() {
		if a.Operation() == shared.OpCapture && a.Outcome() == payment.OutcomeSuccess {
			n++
		}
	}
	return n
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: FR-40, FR-41, FR-60.
//
// L5 pre-dispatch validation and L6 response validation around the gateway call
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
