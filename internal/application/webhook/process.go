package webhook

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

// LedgerPoster appends the ledger entries a webhook implies, inside the caller's transaction.
//
// It takes the Repositories bundle rather than opening its own transaction, and that is the whole
// point of the interface: the state change, the ledger entries and the outbox event must commit
// together or not at all. A poster that owned its own transaction would reintroduce the dual
// write the outbox pattern exists to remove.
//
// It is idempotent on the reference it is given, so replaying a webhook does not double-post.
type LedgerPoster interface {
	Post(ctx context.Context, r ports.Repositories, e Effect) error
}

// Effect is the money movement a webhook describes, in the ledger's vocabulary.
type Effect struct {
	// Reference is the idempotency key for the posting: the gateway's own event identifier. It is
	// the gateway's rather than ours because a replay of the same webhook must be recognised as
	// the same posting, and a key we minted would be different on every replay.
	Reference  string
	Kind       spi.WebhookKind
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	PaymentID  shared.PaymentID
	AttemptID  shared.AttemptID
	RefundID   shared.RefundID
	Amount     money.Money
	Fee        money.Money
	GatewayRef string
	OccurredAt time.Time
	// DisputeWon distinguishes the two outcomes of KindDisputeClosed, which move money in
	// opposite directions. It is a separate field rather than a second Kind because the gateway
	// reports one event type and the outcome inside it, and inventing two kinds here would put the
	// adapters' mapping table and this one out of step.
	DisputeWon bool
}

// ProcessDeps is what the asynchronous processor needs.
type ProcessDeps struct {
	UoW       ports.UnitOfWork
	Verifiers VerifierSource
	Secrets   SecretSource
	Ledger    LedgerPoster
	Clock     shared.Clock
}

// Processor applies what a gateway told us.
//
// It runs asynchronously and it is allowed to be slow, which is what the accept path's 50 ms
// budget bought. What it is not allowed to be is *guessing*: two situations are handled
// explicitly and neither is a silent drop.
//
//   - A webhook for a transition already applied is a **no-op**, not an error. Gateways retry,
//     deliveries arrive out of order, and a captured payment receiving a second capture
//     notification is the normal case rather than a fault. Treating it as an error would put a
//     healthy platform's most common event on the failure dashboard.
//   - A webhook for a payment we have **no record of** opens a reconciliation exception. It is
//     the single most alarming thing this processor can see — money moved for something the
//     platform did not think existed — and dropping it would make that invisible.
type Processor struct {
	deps ProcessDeps
}

// NewProcessor constructs the processor.
func NewProcessor(d ProcessDeps) *Processor {
	if d.Clock == nil {
		d.Clock = shared.SystemClock{}
	}
	return &Processor{deps: d}
}

// Result reports what processing one webhook did.
type Result struct {
	WebhookID shared.WebhookID
	Kind      spi.WebhookKind
	// Applied is true when the webhook moved the payment. False with no error means it was a
	// no-op: an out-of-order or duplicate delivery, or an event type the platform does not model.
	Applied bool
	// Outcome is the label stored on the webhook row and reported to the metric.
	Outcome string
}

// Process loads a stored webhook, re-verifies it, and applies it.
//
// Re-verification is not redundant with the accept path. The row is read minutes or hours later,
// possibly by a different process, and the only thing that makes its payload trustworthy is the
// signature — which the accept path checked but did not carry forward as a fact anything else can
// rely on. Verifying again is a few microseconds of HMAC against the alternative of a processor
// that trusts a database row an operator could have edited.
func (p *Processor) Process(ctx context.Context, id shared.WebhookID) (*Result, error) {
	var stored *ports.InboundWebhook
	if err := p.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		w, err := r.Webhooks.Get(ctx, id)
		stored = w
		return err
	}); err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, apierror.Newf(apierror.CodeWebhookUnknownEventType, "webhook %s not found", id)
	}
	if stored.ProcessedAt != nil {
		// Already applied. Reporting success rather than an error keeps a duplicate queue
		// delivery off the failure dashboard, where it would drown the deliveries that matter.
		return &Result{WebhookID: id, Outcome: "ALREADY_PROCESSED"}, nil
	}

	event, err := p.reverify(ctx, stored)
	if err != nil {
		return nil, err
	}

	res := &Result{WebhookID: id, Kind: event.Kind}
	if err := p.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		applied, outcome, err := p.apply(ctx, r, stored, event)
		if err != nil {
			return err
		}
		res.Applied, res.Outcome = applied, outcome
		return r.Webhooks.MarkProcessed(ctx, id, outcome)
	}); err != nil {
		_ = p.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
			return r.Webhooks.MarkFailed(ctx, id, err, p.deps.Clock.Now().Add(retryAfter(stored.Attempts)))
		})
		return nil, err
	}
	return res, nil
}

// reverify re-authenticates the stored payload.
func (p *Processor) reverify(ctx context.Context, w *ports.InboundWebhook) (*spi.WebhookEvent, error) {
	verifier, err := p.deps.Verifiers.Verifier(ctx, w.GatewayID)
	if err != nil {
		return nil, err
	}
	secrets, err := p.deps.Secrets.SigningSecrets(ctx, w.GatewayID)
	if err != nil {
		return nil, apierror.Wrapf(err, apierror.CodeDependencyFailure,
			"could not resolve the signing secrets for gateway %s", w.GatewayID)
	}
	// The stored receipt time, not now: the signature covers a timestamp with a replay window, and
	// re-verifying against the current clock would reject every webhook older than the window —
	// which is every webhook the retry tier exists for.
	event, err := verifier.Verify(ctx, w.Payload, w.Headers, secrets, w.ReceivedAt)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeWebhookSignatureInvalid,
			"the stored webhook no longer verifies")
	}
	if event == nil {
		return nil, apierror.New(apierror.CodeWebhookSignatureInvalid,
			"the verifier returned neither an event nor an error")
	}
	return event, nil
}

// apply performs the state transition, the ledger posting and the outbox write.
func (p *Processor) apply(ctx context.Context, r ports.Repositories,
	w *ports.InboundWebhook, e *spi.WebhookEvent) (bool, string, error) {

	if e.Kind == spi.KindIgnored {
		// Gateways send far more event types than the platform models. Erroring on one would turn
		// a vendor's feature launch into an incident on our side.
		return false, "IGNORED", nil
	}

	id := e.PaymentID
	if id.IsZero() {
		return p.park(ctx, r, w, e, "WEBHOOK_WITHOUT_PAYMENT_REFERENCE",
			"the webhook carries no payment reference the platform can resolve")
	}
	pay, err := r.Payments.GetForUpdate(ctx, id)
	if err != nil || pay == nil {
		// The alarming case: money moved for something we have no record of.
		return p.park(ctx, r, w, e, "WEBHOOK_FOR_UNKNOWN_PAYMENT",
			"the gateway reported an event for a payment the platform has no record of")
	}

	before := pay.State()
	if err := p.transition(pay, e); err != nil {
		if apierror.CodeOf(err) == apierror.CodeInvalidStateTransition {
			// Out of order, or already applied. A no-op, not a failure: the payment is already
			// where the webhook says it should be, or somewhere later.
			return false, "NO_OP", nil
		}
		return false, "", err
	}
	if pay.State() == before && before != payment.StateDisputed {
		return false, "NO_OP", nil
	}

	if err := r.Payments.Save(ctx, pay); err != nil {
		return false, "", err
	}

	if p.deps.Ledger != nil {
		if err := p.deps.Ledger.Post(ctx, r, Effect{
			Reference: e.GatewayEventID, Kind: e.Kind,
			TenantID: pay.TenantID(), MerchantID: pay.MerchantID(), PaymentID: pay.ID(),
			RefundID: e.RefundID, Amount: effectAmount(pay, e), GatewayRef: e.GatewayRef,
			OccurredAt: e.OccurredAt, DisputeWon: e.Status == spi.StatusVoided,
		}); err != nil {
			return false, "", err
		}
	}
	return true, "APPLIED", nil
}

// transition maps a normalized webhook kind onto the aggregate's own method.
//
// The mapping goes through the aggregate rather than writing a state column, which is what makes
// an out-of-order delivery safe: the state machine refuses a transition that is not legal from
// where the payment stands, and that refusal is the platform's answer to a late webhook.
func (p *Processor) transition(pay *payment.Payment, e *spi.WebhookEvent) error {
	amount := effectAmount(pay, e)
	switch e.Kind {
	case spi.KindAuthorizationSucceeded:
		expires := pay.AuthExpiresAt()
		if expires == nil {
			t := p.deps.Clock.Now().Add(7 * 24 * time.Hour)
			expires = &t
		}
		return pay.MarkAuthorized(amount, expires, p.deps.Clock)
	case spi.KindAuthorizationFailed, spi.KindCaptureFailed:
		return pay.MarkFailed(e.DeclineReason, "reported by webhook", p.deps.Clock)
	case spi.KindCaptureSucceeded:
		return pay.MarkCaptured(amount, p.deps.Clock)
	case spi.KindRefundSucceeded:
		if e.RefundID == "" {
			return apierror.New(apierror.CodeWebhookUnknownEventType,
				"a refund webhook carries no refund reference")
		}
		return pay.ConfirmRefund(e.RefundID, e.GatewayRef, p.deps.Clock)
	case spi.KindRefundFailed:
		// The refund's own lifecycle is the aggregate's; a failed refund returns the amount to the
		// refundable balance without moving the payment, so there is nothing to transition.
		return nil
	case spi.KindVoidSucceeded:
		return pay.Void(p.deps.Clock)
	case spi.KindPayoutSettled:
		return pay.MarkSettled(e.OccurredAt, e.GatewayRef, p.deps.Clock)
	case spi.KindDisputeOpened:
		return pay.MarkDisputed(e.GatewayRef, string(e.Status), p.deps.Clock)
	case spi.KindDisputeClosed:
		// The gateway's status carries the outcome; anything that is not an explicit win is
		// treated as a loss, because conceding money we have already lost is recoverable and
		// claiming a win we did not get is not.
		return pay.ResolveDispute(e.Status == spi.StatusVoided, p.deps.Clock)
	default:
		return nil
	}
}

// effectAmount resolves the amount a webhook moved, defaulting to the payment's own.
//
// A gateway that reports no amount is reporting that the whole payment moved; a gateway that
// reports one is authoritative, and L6 has already checked it is consistent.
func effectAmount(pay *payment.Payment, e *spi.WebhookEvent) money.Money {
	if e.Amount != nil && e.Amount.IsValid() {
		return *e.Amount
	}
	return pay.Amount()
}

// park records a webhook that cannot be applied and reports it as handled-but-not-applied.
//
// It returns no error, and that is the load-bearing decision. Returning one would roll back the
// transaction the exception was just written in — erasing the only record of the thing that
// alarmed us — and would leave the webhook unprocessed, so the next sweep would park it again,
// forever. The exception is the alert; the processed marker is what stops the loop; the false
// Applied flag is what keeps it off the "successfully applied" count.
func (p *Processor) park(ctx context.Context, r ports.Repositories,
	w *ports.InboundWebhook, e *spi.WebhookEvent, kind, detail string) (bool, string, error) {

	if err := r.Reconciliation.OpenException(ctx, ports.ReconciliationException{
		ID:         string(ids.New(ids.PrefixReconciliationRun)),
		TenantID:   w.TenantID,
		MerchantID: w.MerchantID,
		PaymentID:  e.PaymentID,
		Kind:       kind,
		Severity:   "CRITICAL",
		Detail:     detail + " (gateway " + w.GatewayID.String() + ", event " + e.GatewayEventID + ")",
		OpenedAt:   p.deps.Clock.Now(),
	}); err != nil {
		return false, "", err
	}
	return false, "PARKED", nil
}

// retryAfter is exponential backoff for a webhook whose processing failed.
//
// It is capped, because an uncapped doubling reaches hours and a webhook that failed for a
// transient reason should not wait out a gateway's entire outage before being retried.
func retryAfter(attempts int) time.Duration {
	d := time.Second
	for i := 0; i < attempts && d < 5*time.Minute; i++ {
		d *= 2
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: FR-78, FR-79.
//
// Applying a verified webhook to the domain, including the unknown-payment case that opens a
// reconciliation exception rather than dropping the event
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
