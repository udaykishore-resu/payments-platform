package payment

import (
	"context"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// ReconcilerConfig tunes the sweeps.
type ReconcilerConfig struct {
	// MinAge is how long an attempt must have been unresolved before the reconciler touches it.
	// It is not zero on purpose: a gateway that is merely slow will often answer on its own, and
	// a lookup issued two seconds after a timeout mostly races the response it is looking for.
	MinAge time.Duration
	// BatchSize bounds one sweep. Bounded because the sweep runs against live gateways and an
	// unbounded batch after a long outage is a self-inflicted thundering herd at exactly the
	// moment the gateway is recovering.
	BatchSize int
	// LookupTimeout bounds one gateway lookup.
	LookupTimeout time.Duration
	// KeySalt is the per-environment salt for DeriveGatewayIdempotencyKey. It must match the one
	// the dispatcher used, or the recomputed key names a transaction the gateway has never seen
	// and every lookup answers NOT_FOUND — which the reconciler would read as positive evidence
	// that nothing happened. That is the single most dangerous misconfiguration in this file, and
	// it is why the salt is an explicit field rather than a default.
	KeySalt string
}

// DefaultReconcilerConfig returns the production defaults.
func DefaultReconcilerConfig() ReconcilerConfig {
	return ReconcilerConfig{
		MinAge:        30 * time.Second,
		BatchSize:     100,
		LookupTimeout: 10 * time.Second,
	}
}

// ReconcilerDeps is what resolving unknown outcomes requires.
type ReconcilerDeps struct {
	UoW      ports.UnitOfWork
	Gateways GatewayResolver
	Audit    Auditor
	Metrics  Metrics
	Clock    shared.Clock
	Settings ReconcilerConfig
}

// Reconciler resolves the outcomes the dispatch path deliberately refused to guess.
//
// It is the other half of ADR-013. The orchestrator's rule — a timeout never advances a payment
// and never triggers failover — is only survivable because something else eventually finds out
// what happened, and this is that something. Without it, every gateway timeout would be a
// payment parked forever in PROCESSING, which is a worse outcome than either of the guesses the
// orchestrator refused to make.
//
// Three sweeps live here, and they are one type rather than three because they share the
// property that matters: each one resolves an ambiguity by *asking*, never by assuming, and each
// one opens a reconciliation exception when asking does not produce an answer.
type Reconciler struct {
	deps ReconcilerDeps
}

// NewReconciler constructs the reconciler.
func NewReconciler(d ReconcilerDeps) *Reconciler {
	if d.Clock == nil {
		d.Clock = shared.SystemClock{}
	}
	if d.Settings.BatchSize <= 0 {
		def := DefaultReconcilerConfig()
		if d.Settings.MinAge <= 0 {
			d.Settings.MinAge = def.MinAge
		}
		d.Settings.BatchSize = def.BatchSize
		if d.Settings.LookupTimeout <= 0 {
			d.Settings.LookupTimeout = def.LookupTimeout
		}
	}
	return &Reconciler{deps: d}
}

// SweepResult reports what one pass did, for the metric and for the operator's dashboard.
type SweepResult struct {
	Examined   int
	Resolved   int
	Exceptions int
}

// ResolveUnknown is the unknown-outcome sweep.
//
// For each payment carrying a TIMEOUT_UNKNOWN attempt it recomputes the gateway idempotency key
// from the persisted attempt — deterministically, which is the entire reason the key is derived
// rather than random — asks the gateway what happened to it, and resolves the attempt with
// whatever the gateway says.
//
// The four answers and what each means:
//
//   - AUTHORIZED/CAPTURED: money moved. The attempt succeeds and the payment advances. This is
//     the case that would have been a lost sale under any "treat a timeout as a failure" policy.
//   - DECLINED: the gateway acted and refused. The attempt declines; the payment fails if no
//     failover is permitted.
//   - NOT_FOUND: positive evidence that nothing happened — and it is only positive evidence
//     because the key is deterministic. The attempt becomes an ERROR, which is the one outcome
//     that permits a retry.
//   - anything else, or an error: we still do not know. The attempt stays TIMEOUT_UNKNOWN and a
//     reconciliation exception is opened for a human. Guessing here is precisely the failure this
//     whole design exists to avoid.
func (r *Reconciler) ResolveUnknown(ctx context.Context) (SweepResult, error) {
	var out SweepResult
	var pending []*payment.Payment
	if err := r.deps.UoW.Within(ctx, func(ctx context.Context, repo ports.Repositories) error {
		var err error
		pending, err = repo.Payments.FindUnresolved(ctx, r.deps.Settings.MinAge, r.deps.Settings.BatchSize)
		return err
	}); err != nil {
		return out, err
	}

	for _, p := range pending {
		if p == nil {
			continue
		}
		out.Examined++
		resolved, err := r.resolveOne(ctx, p)
		switch {
		case err != nil:
			// One payment's failure must not abandon the batch: the sweep is the recovery path
			// for an outage, and an outage produces correlated failures.
			out.Exceptions++
		case resolved:
			out.Resolved++
		default:
			out.Exceptions++
		}
	}
	return out, nil
}

// resolveOne asks the gateway about one unresolved attempt.
func (r *Reconciler) resolveOne(ctx context.Context, p *payment.Payment) (bool, error) {
	att := unresolvedAttempt(p)
	if att == nil {
		return false, nil
	}

	client, creds, ext, err := r.deps.Gateways.Resolve(ctx, p.MerchantID(), att.GatewayID())
	if err != nil {
		return false, err
	}

	// Recomputed rather than read from the row. The two agree in the ordinary case; they diverge
	// exactly when the crash happened between minting the attempt and persisting its key, which
	// is the case reconciliation exists for.
	key := payment.DeriveGatewayIdempotencyKey(att.ID(), att.Operation(), r.deps.Settings.KeySalt)

	callCtx, cancel := context.WithTimeout(ctx, r.deps.Settings.LookupTimeout)
	defer cancel()
	res, callErr := client.Lookup(callCtx, spi.LookupRequest{
		Credentials: creds, ExternalAccountID: ext,
		GatewayRef: att.GatewayRef(), IdempotencyKey: key, Operation: att.Operation(),
	})

	now := r.deps.Clock.Now()
	if callErr != nil || res == nil {
		return false, r.openException(ctx, p, att, "LOOKUP_FAILED",
			"the gateway could not say what happened to this attempt")
	}

	var (
		outcome payment.AttemptOutcome
		reason  payment.DeclineReason
	)
	switch res.Status {
	case spi.StatusAuthorized, spi.StatusCaptured, spi.StatusRefunded, spi.StatusVoided,
		spi.StatusRefundAccepted:
		outcome = payment.OutcomeSuccess
	case spi.StatusDeclined:
		outcome, reason = payment.OutcomeDeclined, res.DeclineReason
	case spi.StatusNotFound:
		outcome = payment.OutcomeError
	default:
		// PENDING, REQUIRES_ACTION and anything unrecognised are not resolutions. The gateway is
		// telling us it still does not know either, and an exception is the honest record of that.
		return false, r.openException(ctx, p, att, "GATEWAY_STILL_UNKNOWN",
			"the gateway reported "+string(res.Status)+", which does not resolve the outcome")
	}

	if err := att.Reconcile(outcome, res.GatewayRef, res.RawStatus, reason, now); err != nil {
		return false, err
	}
	if err := r.advance(p, att, res, now); err != nil {
		return false, err
	}

	if err := r.deps.UoW.Within(ctx, func(ctx context.Context, repo ports.Repositories) error {
		if err := repo.Payments.SaveAttempt(ctx, att); err != nil {
			return err
		}
		if err := repo.Payments.Save(ctx, p); err != nil {
			return err
		}
		return r.deps.Audit.Record(ctx, repo, "payment.reconciled", "payment", p.ID().String(), "SUCCESS",
			map[string]any{
				"attemptId": att.ID().String(),
				"gatewayId": att.GatewayID().String(),
				"outcome":   string(outcome),
				"state":     string(p.State()),
			})
	}); err != nil {
		return false, err
	}
	if r.deps.Metrics != nil {
		r.deps.Metrics.RecordPaymentOutcome("reconciled_"+string(outcome),
			p.Currency(), p.PaymentMethod(), att.GatewayID())
	}
	return true, nil
}

// advance moves the payment to the state the resolved attempt implies.
//
// The transitions are attempted, and a refusal is tolerated rather than propagated. That is
// deliberate: a webhook may have arrived while the sweep was running and already advanced the
// payment, and the state machine refusing a duplicate transition is the *correct* outcome of
// that race — the reconciler's job is to make sure the payment ends up right, not to be the one
// that moved it.
func (r *Reconciler) advance(p *payment.Payment, att *payment.Attempt, res *spi.Result, now time.Time) error {
	switch {
	case att.Outcome() == payment.OutcomeSuccess && res.Status == spi.StatusAuthorized:
		amt := p.Amount()
		if res.AuthorizedAmount != nil {
			amt = *res.AuthorizedAmount
		}
		expires := res.AuthExpiresAt
		if expires == nil {
			t := now.Add(7 * 24 * time.Hour)
			expires = &t
		}
		_ = p.MarkAuthorized(amt, expires, r.deps.Clock)
	case att.Outcome() == payment.OutcomeSuccess && res.Status == spi.StatusCaptured:
		amt := p.Amount()
		if res.CapturedAmount != nil {
			amt = *res.CapturedAmount
		}
		_ = p.MarkCaptured(amt, r.deps.Clock)
	case att.Outcome() == payment.OutcomeDeclined && !att.PermitsFailover():
		_ = p.MarkFailed(att.DeclineReason(), res.RawMessage, r.deps.Clock)
	case att.Outcome() == payment.OutcomeError:
		// NOT_FOUND: the gateway never acted. The payment stays in flight and the ordinary
		// dispatch path may retry it, which is now safe precisely because we asked.
	}
	return nil
}

// openException records that an ambiguity could not be resolved automatically.
//
// The exception, not the payment, is what carries the alert. The payment stays exactly where it
// was — in flight, with a TIMEOUT_UNKNOWN attempt — because moving it would be the guess this
// package refuses to make, and because the next sweep must find it again.
func (r *Reconciler) openException(ctx context.Context, p *payment.Payment, att *payment.Attempt,
	kind, detail string) error {

	return r.deps.UoW.Within(ctx, func(ctx context.Context, repo ports.Repositories) error {
		return repo.Reconciliation.OpenException(ctx, ports.ReconciliationException{
			ID:         string(ids.New(ids.PrefixReconciliationRun)),
			TenantID:   p.TenantID(),
			MerchantID: p.MerchantID(),
			PaymentID:  p.ID(),
			AttemptID:  att.ID(),
			Kind:       kind,
			Severity:   "CRITICAL",
			Detail:     detail,
			OpenedAt:   r.deps.Clock.Now(),
		})
	})
}

// SweepExpiredAuthorizations moves authorizations past their expiry to EXPIRED.
//
// This exists because an authorization the issuer has already released is one the platform must
// stop offering to capture. A merchant who captures against a lapsed hold gets a decline at best
// and, at several gateways, a *successful* capture that later reverses — which arrives as an
// unexplained chargeback weeks later.
//
// The sweeper expires the platform's record slightly *early* by construction: the configured
// validity is set below the shortest validity of any enabled gateway. Expiring early is harmless
// (the merchant is told to re-authorize); expiring late is the failure above.
func (r *Reconciler) SweepExpiredAuthorizations(ctx context.Context) (SweepResult, error) {
	var out SweepResult
	now := r.deps.Clock.Now()
	var due []*payment.Payment
	if err := r.deps.UoW.Within(ctx, func(ctx context.Context, repo ports.Repositories) error {
		var err error
		due, err = repo.Payments.FindExpiredAuthorizations(ctx, now, r.deps.Settings.BatchSize)
		return err
	}); err != nil {
		return out, err
	}

	for _, p := range due {
		if p == nil {
			continue
		}
		out.Examined++
		// An unresolved attempt outranks an expiry. A payment whose outcome we do not know must
		// not be expired: expiry is a terminal state, and reaching it would assert that no money
		// moved on a payment where money may have.
		if p.HasUnresolvedAttempt() {
			out.Exceptions++
			continue
		}
		if err := p.Expire(r.deps.Clock); err != nil {
			out.Exceptions++
			continue
		}
		if err := r.deps.UoW.Within(ctx, func(ctx context.Context, repo ports.Repositories) error {
			if err := repo.Payments.Save(ctx, p); err != nil {
				return err
			}
			return r.deps.Audit.Record(ctx, repo, "payment.expired", "payment", p.ID().String(), "SUCCESS",
				map[string]any{"authExpiresAt": formatTime(p.AuthExpiresAt())})
		}); err != nil {
			out.Exceptions++
			continue
		}
		out.Resolved++
	}
	return out, nil
}

// SettlementRow is one line of a gateway's settlement report, normalized by the adapter.
//
// Gross, fee and net are all carried even though one is derivable, because the identity
// gross = net + fee is what the ingester *checks*: a row whose three numbers disagree is a
// gateway contract violation or a parsing defect, and either way the correct response is an
// exception rather than a posting that silently makes the arithmetic work.
type SettlementRow struct {
	GatewayID     shared.GatewayID
	GatewayRef    string
	PaymentID     shared.PaymentID
	Gross         money.Money
	Fee           money.Money
	Net           money.Money
	SettledAt     time.Time
	SettlementRef string
}

// IngestSettlement applies a settlement report to the payments it names.
//
// Settlement is *observed*, never asserted: the platform does not settle funds (baseline §1.3
// A1), so the only thing that can move a payment to SETTLED is the gateway saying it did. Two
// failure modes are handled explicitly and neither is silent:
//
//   - A row naming a payment we have no record of opens a reconciliation exception. It is the
//     single most important signal this sweep produces: money moved for something the platform
//     did not think existed.
//   - A row whose amounts do not reconcile opens an exception rather than being posted, for the
//     reason above.
func (r *Reconciler) IngestSettlement(ctx context.Context, rows []SettlementRow) (SweepResult, error) {
	var out SweepResult
	for _, row := range rows {
		out.Examined++
		if err := r.ingestRow(ctx, row); err != nil {
			out.Exceptions++
			continue
		}
		out.Resolved++
	}
	return out, nil
}

func (r *Reconciler) ingestRow(ctx context.Context, row SettlementRow) error {
	if sum, err := row.Net.Add(row.Fee); err != nil || !sum.Equal(row.Gross) {
		return r.openOrphanException(ctx, row, "SETTLEMENT_DOES_NOT_RECONCILE",
			"gross does not equal net plus fees")
	}
	var orphan bool
	err := r.deps.UoW.Within(ctx, func(ctx context.Context, repo ports.Repositories) error {
		p, err := repo.Payments.Get(ctx, row.PaymentID)
		if err != nil || p == nil {
			orphan = true
			return repo.Reconciliation.OpenException(ctx, ports.ReconciliationException{
				ID:        string(ids.New(ids.PrefixReconciliationRun)),
				PaymentID: row.PaymentID,
				Kind:      "SETTLEMENT_FOR_UNKNOWN_PAYMENT",
				Severity:  "CRITICAL",
				Detail:    "the gateway settled a transaction the platform has no record of",
				OpenedAt:  r.deps.Clock.Now(),
			})
		}
		if err := p.MarkSettled(row.SettledAt, row.SettlementRef, r.deps.Clock); err != nil {
			// Already settled, or in a state that cannot settle. A duplicate settlement row is
			// the normal case — gateways re-send reports — so this is a no-op, not an error.
			if apierror.CodeOf(err) == apierror.CodeInvalidStateTransition {
				return nil
			}
			return err
		}
		if err := repo.Payments.Save(ctx, p); err != nil {
			return err
		}
		return r.deps.Audit.Record(ctx, repo, "payment.settled", "payment", p.ID().String(), "SUCCESS",
			map[string]any{
				"settlementRef": row.SettlementRef,
				"gross":         row.Gross.Amount(),
				"fee":           row.Fee.Amount(),
				"net":           row.Net.Amount(),
			})
	})
	if err != nil {
		return err
	}
	if orphan {
		// The exception is committed; the row is still reported as *not* ingested, because an
		// exception nobody counts is an exception nobody looks at.
		return apierror.Newf(apierror.CodePaymentNotFound,
			"settlement row references payment %s, which the platform has no record of", row.PaymentID)
	}
	return nil
}

// openOrphanException records a row that cannot be posted and reports it as an error, so the
// sweep's counters distinguish "ingested" from "parked for a human".
func (r *Reconciler) openOrphanException(ctx context.Context, row SettlementRow, kind, detail string) error {
	if err := r.deps.UoW.Within(ctx, func(ctx context.Context, repo ports.Repositories) error {
		return repo.Reconciliation.OpenException(ctx, ports.ReconciliationException{
			ID:        string(ids.New(ids.PrefixReconciliationRun)),
			PaymentID: row.PaymentID,
			Kind:      kind,
			Severity:  "CRITICAL",
			Detail:    detail,
			OpenedAt:  r.deps.Clock.Now(),
		})
	}); err != nil {
		return err
	}
	return apierror.Newf(apierror.CodeGatewayContractViolation, "settlement row parked: %s", detail)
}

// unresolvedAttempt returns the attempt awaiting reconciliation, or nil. There is at most one:
// StartAttempt refuses to create a new attempt while one is unresolved, which is what stops a
// payment accumulating ambiguities.
func unresolvedAttempt(p *payment.Payment) *payment.Attempt {
	for _, a := range p.Attempts() {
		if a.Outcome().RequiresReconciliation() {
			return a
		}
	}
	return nil
}

func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-28, BR-30, FR-82, FR-83, FR-84, FR-85.
//
// Resolution of ambiguous outcomes and settlement ingestion — the paths that decide what
// actually happened when the gateway did not say
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
