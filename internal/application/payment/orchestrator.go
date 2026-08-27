package payment

import (
	"context"
	"errors"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/routing"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Orchestrator executes a payment instruction against gateways.
//
// It is the component the whole platform's correctness rests on, and its job description is
// unusually narrow: given a payment and an ordered routing plan, call gateways until the
// payment reaches a state that is either terminal or genuinely pending, without ever creating
// two charges for one instruction.
//
// It contains no gateway-specific logic. It does not know that Stripe returns
// `requires_capture` or that Adyen returns `Authorised`; the adapters have already normalised
// those. What it does know is the sequencing, and the sequencing is the product.
type Orchestrator struct {
	deps      Deps
	validator PaymentValidator
}

// NewOrchestrator constructs the orchestrator.
func NewOrchestrator(d Deps, v PaymentValidator) *Orchestrator {
	return &Orchestrator{deps: d, validator: v}
}

// DispatchInput is one authorization run.
type DispatchInput struct {
	Payment   *payment.Payment
	Plan      *routing.Plan
	Merchant  MerchantSnapshot
	Risk      risk.Decision
	Operation shared.Operation
	ReturnURL string
}

// GatewayResponse is the normalized result of one gateway call, as the L6 validator sees it.
type GatewayResponse struct {
	Status           spi.Status
	GatewayRef       string
	AuthorizedAmount *money.Money
	CapturedAmount   *money.Money
	DeclineReason    payment.DeclineReason
	RawStatus        string
	RawCode          string
}

// Dispatch runs the authorization, failing over through the plan as the outcomes permit.
//
// The loop below is short, and every line of it is load-bearing. The invariants it maintains:
//
//   - An attempt row is created and committed before the gateway is called (T1), so a crash at
//     any subsequent point leaves a record that reconciliation can act on.
//   - The gateway's answer, the resulting state transition and the outbox event commit in one
//     transaction (T3), so the platform's state and the events derived from it cannot diverge.
//   - `TIMEOUT_UNKNOWN` breaks the loop unconditionally. It is not a failure, it is not
//     retried, and it does not fail over. The payment stays in PROCESSING and the reconciler
//     owns it.
//   - Failover consults the *attempt*, not the loop counter: `attempt.PermitsFailover()` folds
//     together the outcome, the normalized decline reason and any scheme-level "do not retry"
//     advice, taking the most restrictive answer.
func (o *Orchestrator) Dispatch(ctx context.Context, in DispatchInput) (*Result, error) {
	pay := in.Payment
	tried := make([]shared.GatewayID, 0, o.deps.Settings.MaxAttempts)
	var lastErr error

	for i := 0; i < o.deps.Settings.MaxAttempts; i++ {
		sel, ok := in.Plan.Next(tried)
		if !ok {
			break
		}
		tried = append(tried, sel.GatewayID)

		res, err := o.attemptOnce(ctx, pay, in, sel.GatewayID)
		if err != nil {
			// A pre-dispatch failure (circuit open, credentials unresolvable, bulkhead full)
			// leaves the gateway untouched, so trying the next one is safe.
			lastErr = err
			if !o.deps.Settings.EnableFailover {
				break
			}
			continue
		}
		if res.terminal {
			return res.result, nil
		}
		if !o.deps.Settings.EnableFailover {
			break
		}
		lastErr = res.err
	}

	// Every candidate is exhausted. Reload so the response reflects what was actually
	// committed rather than the in-memory aggregate, which may be behind after a conflict
	// retry inside a transaction.
	final, err := o.reload(ctx, pay.ID())
	if err != nil {
		return nil, err
	}
	if final.State().IsInFlight() {
		// An unresolved attempt keeps the payment in flight. This is a 202-shaped answer, not
		// an error: the merchant must poll or wait for the webhook, and must not retry.
		return &Result{Payment: final, Plan: in.Plan}, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return &Result{Payment: final, Plan: in.Plan}, nil
}

// attemptResult carries the outcome of one gateway attempt back to the dispatch loop.
type attemptResult struct {
	// terminal means the loop must stop: the payment succeeded, was definitively declined, or
	// entered an unknown state that forbids further attempts.
	terminal bool
	result   *Result
	err      error
}

// attemptOnce performs one gateway attempt end to end.
func (o *Orchestrator) attemptOnce(ctx context.Context, pay *payment.Payment, in DispatchInput, gw shared.GatewayID) (attemptResult, error) {
	breakerKey := gw.String() + ":" + string(in.Operation)
	record, err := o.deps.Breakers.Allow(breakerKey)
	if err != nil {
		o.deps.Metrics.SetCircuitState(gw, in.Operation, o.deps.Breakers.State(breakerKey))
		return attemptResult{}, apierror.Wrapf(err, apierror.CodeGatewayCircuitOpen,
			"gateway %s is not accepting traffic", gw)
	}

	release, err := o.deps.Bulkheads.Acquire(ctx, gw.String())
	if err != nil {
		record(false, false)
		return attemptResult{}, err
	}
	defer release()

	client, creds, externalAccountID, err := o.deps.Gateways.Resolve(ctx, pay.MerchantID(), gw)
	if err != nil {
		record(false, false)
		return attemptResult{}, err
	}

	// T1 — the attempt row is created and committed before the call. This is the single most
	// important line in the file. Reversing T1 and the gateway call would mean a crash between
	// them leaves a charge at the gateway that no record in our system refers to, and no
	// reconciliation process can find what it does not know exists.
	att, err := pay.StartAttempt(gw, in.Plan.ID, in.Operation, o.deps.Clock)
	if err != nil {
		record(false, false)
		return attemptResult{}, err
	}
	// The connection is bound before the attempt is committed, not after, so the row that exists
	// when the gateway is called already says which credential signed the call. Binding it
	// afterwards would leave exactly the crash window this ordering exists to close: a charge at
	// the gateway whose credential the record cannot name.
	bindConnection(att, in.Merchant, gw)
	if err := pay.MarkProcessing(o.deps.Clock); err != nil && !isSameState(err) {
		record(false, false)
		return attemptResult{}, err
	}
	if err := o.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Payments.Save(ctx, pay)
	}); err != nil {
		record(false, false)
		return attemptResult{}, err
	}

	// T2 — the call itself, under its own deadline so a hung gateway cannot consume the
	// caller's entire budget.
	callCtx, cancel := context.WithTimeout(ctx, o.deps.Settings.GatewayTimeout)
	defer cancel()

	dispatchedAt := o.deps.Clock.Now()
	if err := att.Dispatch(dispatchedAt); err != nil {
		record(false, false)
		return attemptResult{}, err
	}

	resp, callErr := client.Authorize(callCtx, spi.AuthorizeRequest{
		IdempotencyKey:    att.IdempotencyKey(),
		Credentials:       creds,
		ExternalAccountID: externalAccountID,
		PaymentID:         pay.ID(),
		AttemptID:         att.ID(),
		Amount:            pay.Amount(),
		Method:            pay.PaymentMethod(),
		MethodRef:         pay.MethodRef(),
		Capture:           pay.CaptureMethod() == payment.CaptureAutomatic,
		Descriptor:        pay.Description(),
		StatementRef:      pay.StatementRef(),
		ThreeDS: spi.ThreeDSRequest{
			Requested:     in.Risk.Require3DS,
			ExemptionType: string(in.Risk.ExemptionApplied),
		},
		Customer: spi.CustomerData{
			ID:        pay.Customer().MerchantCustomerID,
			EmailHash: pay.Customer().EmailHash,
			IPAddress: pay.Customer().IPAddress,
			Country:   pay.Customer().Country,
		},
		ReturnURL: in.ReturnURL,
		Metadata:  pay.Metadata(),
	})
	latency := o.deps.Clock.Now().Sub(dispatchedAt)

	return o.settle(ctx, settleInput{
		payment: pay, attempt: att, plan: in.Plan, gateway: gw,
		operation: in.Operation, resp: resp, callErr: callErr,
		latency: latency, record: record,
	})
}

// settleInput bundles what settling one attempt needs.
type settleInput struct {
	payment   *payment.Payment
	attempt   *payment.Attempt
	plan      *routing.Plan
	gateway   shared.GatewayID
	operation shared.Operation
	resp      *spi.Result
	callErr   error
	latency   time.Duration
	record    func(success bool, counted bool)
}

// settle classifies the gateway's answer and commits the consequences.
//
// The classification order matters, and it is: unknown first, then transport error, then
// contract violation, then the business outcome. Checking the business outcome first would
// mean a malformed response with a `status: authorized` field could move a payment to
// AUTHORIZED without the platform ever confirming the amount matched.
func (o *Orchestrator) settle(ctx context.Context, in settleInput) (attemptResult, error) {
	now := o.deps.Clock.Now()
	pay, att := in.payment, in.attempt

	o.deps.Metrics.ObserveGatewayRequest(in.gateway, in.operation, outcomeLabel(in.resp, in.callErr), in.latency)

	switch {
	// 1. Unknown. The gateway may or may not have acted. Nothing may be inferred, nothing may
	//    be retried, and the payment does not move.
	case errors.Is(in.callErr, spi.ErrOutcomeUnknown) || (in.resp == nil && isTimeout(in.callErr)):
		in.record(false, true)
		if err := att.TimeOut(safeMessage(in.callErr), now); err != nil {
			return attemptResult{}, err
		}
		pay.RequireReconciliation(att.ID(), "gateway outcome unknown", o.deps.Clock)
		if err := o.commit(ctx, pay, att, "payment.attempt_unknown"); err != nil {
			return attemptResult{}, err
		}
		o.deps.Metrics.RecordPaymentOutcome("unknown", pay.Currency(), pay.PaymentMethod(), in.gateway)
		res, err := o.reloadResult(ctx, pay.ID(), in.plan)
		return attemptResult{terminal: true, result: res}, err

	// 2. Transport or pre-processing failure. The gateway provably did not act, so failover is
	//    safe and the attempt is recorded as an error rather than a decline.
	case in.callErr != nil:
		in.record(false, true)
		code := string(apierror.CodeOf(in.callErr))
		if err := att.Fail(code, safeMessage(in.callErr), now); err != nil {
			return attemptResult{}, err
		}
		if err := o.commit(ctx, pay, att, "payment.attempt_failed"); err != nil {
			return attemptResult{}, err
		}
		return attemptResult{terminal: !att.PermitsFailover(), err: in.callErr}, nil

	// 3. A nil result with a nil error is a broken adapter. Treat it as unknown rather than as
	//    success: the contract suite asserts this cannot happen, but if it ever does, the safe
	//    reading is "we do not know".
	case in.resp == nil:
		in.record(false, true)
		if err := att.TimeOut("adapter returned no result", now); err != nil {
			return attemptResult{}, err
		}
		pay.RequireReconciliation(att.ID(), "adapter returned neither a result nor an error", o.deps.Clock)
		if err := o.commit(ctx, pay, att, "payment.attempt_unknown"); err != nil {
			return attemptResult{}, err
		}
		res, err := o.reloadResult(ctx, pay.ID(), in.plan)
		return attemptResult{terminal: true, result: res}, err
	}

	// 4. L6 — validate the response before believing any of it.
	if err := o.validator.ValidateGatewayResponse(ctx, GatewayResponse{
		Status: in.resp.Status, GatewayRef: in.resp.GatewayRef,
		AuthorizedAmount: in.resp.AuthorizedAmount, CapturedAmount: in.resp.CapturedAmount,
		DeclineReason: in.resp.DeclineReason, RawStatus: in.resp.RawStatus, RawCode: in.resp.RawCode,
	}, ExpectedResponse{
		Amount: pay.Amount(), Currency: pay.Currency(), Operation: in.operation,
		GatewayID: in.gateway, PaymentID: pay.ID(), AttemptID: att.ID(),
		CurrentState: pay.State(), AuthorizedAmount: pay.AuthorizedAmount(),
		CapturedTotal: pay.CapturedAmount(),
	}); err != nil {
		// A contract violation is the gateway's fault, counts against its health, and — because
		// we cannot trust what it told us — leaves the outcome unknown rather than failed.
		in.record(false, true)
		if terr := att.TimeOut("gateway response failed validation: "+err.Error(), now); terr != nil {
			return attemptResult{}, terr
		}
		pay.RequireReconciliation(att.ID(), "gateway response failed L6 validation", o.deps.Clock)
		if cerr := o.commit(ctx, pay, att, "payment.attempt_unknown"); cerr != nil {
			return attemptResult{}, cerr
		}
		res, rerr := o.reloadResult(ctx, pay.ID(), in.plan)
		return attemptResult{terminal: true, result: res}, rerr
	}

	// 5. The business outcome.
	switch in.resp.Status {
	case spi.StatusAuthorized:
		in.record(true, true)
		if err := att.Succeed(in.resp.GatewayRef, in.resp.RawStatus, now); err != nil {
			return attemptResult{}, err
		}
		amt := pay.Amount()
		if in.resp.AuthorizedAmount != nil {
			amt = *in.resp.AuthorizedAmount
		}
		expires := in.resp.AuthExpiresAt
		if expires == nil {
			t := now.Add(o.deps.Settings.AuthorizationValidity)
			expires = &t
		}
		if err := pay.MarkAuthorized(amt, expires, o.deps.Clock); err != nil {
			return attemptResult{}, err
		}
		if err := o.commit(ctx, pay, att, "payment.authorized"); err != nil {
			return attemptResult{}, err
		}
		o.deps.Metrics.RecordPaymentOutcome("authorized", pay.Currency(), pay.PaymentMethod(), in.gateway)
		res, err := o.reloadResult(ctx, pay.ID(), in.plan)
		return attemptResult{terminal: true, result: res}, err

	case spi.StatusCaptured:
		in.record(true, true)
		if err := att.Succeed(in.resp.GatewayRef, in.resp.RawStatus, now); err != nil {
			return attemptResult{}, err
		}
		amt := pay.Amount()
		if in.resp.CapturedAmount != nil {
			amt = *in.resp.CapturedAmount
		}
		if err := pay.MarkCaptured(amt, o.deps.Clock); err != nil {
			return attemptResult{}, err
		}
		if err := o.commit(ctx, pay, att, "payment.captured"); err != nil {
			return attemptResult{}, err
		}
		o.deps.Metrics.RecordPaymentOutcome("captured", pay.Currency(), pay.PaymentMethod(), in.gateway)
		res, err := o.reloadResult(ctx, pay.ID(), in.plan)
		return attemptResult{terminal: true, result: res}, err

	case spi.StatusRequiresAction:
		// The attempt has not resolved — the payer still has to act — so the attempt stays
		// open and the payment parks. Recording this as a success would let a subsequent
		// failover be blocked by an attempt that never actually authorized anything.
		in.record(true, true)
		action := payment.NextAction{Type: payment.ActionThreeDSChall}
		if in.resp.NextAction != nil {
			action = payment.NextAction{
				Type:        in.resp.NextAction.Type,
				RedirectURL: in.resp.NextAction.RedirectURL,
				ExpiresAt:   in.resp.NextAction.ExpiresAt,
			}
		}
		if err := pay.RequireAction(action, o.deps.Clock); err != nil {
			return attemptResult{}, err
		}
		if err := o.commit(ctx, pay, att, "payment.requires_action"); err != nil {
			return attemptResult{}, err
		}
		res, err := o.reloadResult(ctx, pay.ID(), in.plan)
		if res != nil {
			res.NextAction = &action
		}
		return attemptResult{terminal: true, result: res}, err

	case spi.StatusPending:
		in.record(true, true)
		if err := pay.MarkPending(o.deps.Clock); err != nil {
			return attemptResult{}, err
		}
		if err := o.commit(ctx, pay, att, "payment.pending"); err != nil {
			return attemptResult{}, err
		}
		res, err := o.reloadResult(ctx, pay.ID(), in.plan)
		return attemptResult{terminal: true, result: res}, err

	case spi.StatusDeclined:
		// A decline is a *business* outcome, not a gateway failure. It must not count toward
		// the circuit breaker's error rate: a merchant with a high-decline customer cohort
		// would otherwise open the breaker on a perfectly healthy gateway and take the
		// gateway out for every other merchant sharing it.
		in.record(false, false)
		if err := att.Decline(in.resp.DeclineReason, in.resp.GatewayRef, in.resp.RawStatus,
			in.resp.NetworkAdviceNoRetry, now); err != nil {
			return attemptResult{}, err
		}
		canFailover := att.PermitsFailover() && o.deps.Settings.EnableFailover
		if !canFailover {
			if err := pay.MarkFailed(in.resp.DeclineReason, in.resp.RawMessage, o.deps.Clock); err != nil {
				return attemptResult{}, err
			}
		}
		if err := o.commit(ctx, pay, att, "payment.declined"); err != nil {
			return attemptResult{}, err
		}
		o.deps.Metrics.RecordPaymentOutcome("declined", pay.Currency(), pay.PaymentMethod(), in.gateway)
		if canFailover {
			return attemptResult{terminal: false, err: declineError(in.resp.DeclineReason)}, nil
		}
		res, err := o.reloadResult(ctx, pay.ID(), in.plan)
		return attemptResult{terminal: true, result: res}, err

	default:
		// A status the orchestrator does not model. Unknown, not failed.
		in.record(false, true)
		if err := att.TimeOut("unrecognised gateway status: "+string(in.resp.Status), now); err != nil {
			return attemptResult{}, err
		}
		pay.RequireReconciliation(att.ID(), "unrecognised gateway status", o.deps.Clock)
		if err := o.commit(ctx, pay, att, "payment.attempt_unknown"); err != nil {
			return attemptResult{}, err
		}
		res, err := o.reloadResult(ctx, pay.ID(), in.plan)
		return attemptResult{terminal: true, result: res}, err
	}
}

// commit is T3: the attempt, the payment's new state and every event the aggregate raised are
// written in one transaction. There is no path in this file that writes one without the others.
func (o *Orchestrator) commit(ctx context.Context, pay *payment.Payment, att *payment.Attempt, action string) error {
	return o.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		if err := r.Payments.SaveAttempt(ctx, att); err != nil {
			return err
		}
		if err := r.Payments.Save(ctx, pay); err != nil {
			return err
		}
		return o.deps.Audit.Record(ctx, r, action, "payment", pay.ID().String(), "SUCCESS",
			map[string]any{
				"attemptId": att.ID().String(),
				"gatewayId": att.GatewayID().String(),
				"outcome":   string(att.Outcome()),
				"state":     string(pay.State()),
			})
	})
}

// CaptureExisting captures against the gateway that holds the authorization.
func (o *Orchestrator) CaptureExisting(ctx context.Context, pay *payment.Payment, m MerchantSnapshot, amount money.Money, final bool) (*Result, error) {
	success := pay.SuccessfulAttempt()
	if success == nil {
		return nil, apierror.New(apierror.CodeInvalidStateTransition,
			"this payment has no successful authorization to capture")
	}
	return o.followUp(ctx, pay, m, success.GatewayID(), shared.OpCapture,
		func(client spi.PaymentGateway, ctx context.Context, att *payment.Attempt, creds spi.Credentials, ext string) (*spi.Result, error) {
			return client.Capture(ctx, spi.CaptureRequest{
				IdempotencyKey: att.IdempotencyKey(), Credentials: creds, ExternalAccountID: ext,
				PaymentID: pay.ID(), AttemptID: att.ID(), GatewayRef: success.GatewayRef(),
				Amount: amount, Final: final,
			})
		},
		func(res *spi.Result) error {
			amt := amount
			if res.CapturedAmount != nil {
				amt = *res.CapturedAmount
			}
			return pay.MarkCaptured(amt, o.deps.Clock)
		})
}

// VoidExisting releases the hold at the gateway that holds it.
func (o *Orchestrator) VoidExisting(ctx context.Context, pay *payment.Payment, m MerchantSnapshot) (*Result, error) {
	success := pay.SuccessfulAttempt()
	if success == nil {
		return nil, apierror.New(apierror.CodeInvalidStateTransition,
			"this payment has no authorization to void")
	}
	return o.followUp(ctx, pay, m, success.GatewayID(), shared.OpVoid,
		func(client spi.PaymentGateway, ctx context.Context, att *payment.Attempt, creds spi.Credentials, ext string) (*spi.Result, error) {
			return client.Void(ctx, spi.VoidRequest{
				IdempotencyKey: att.IdempotencyKey(), Credentials: creds, ExternalAccountID: ext,
				PaymentID: pay.ID(), GatewayRef: success.GatewayRef(),
			})
		},
		func(*spi.Result) error { return pay.Void(o.deps.Clock) })
}

// RefundExisting returns funds through the gateway that took them.
//
// The refund entity is created and committed *before* the gateway call, for the same reason the
// attempt is: a crash mid-refund must leave a record, or the merchant has told a customer their
// money is coming back and nothing in the system knows.
func (o *Orchestrator) RefundExisting(ctx context.Context, pay *payment.Payment, m MerchantSnapshot,
	amount money.Money, reason payment.RefundReason, idemKey string) (*Result, error) {
	success := pay.SuccessfulAttempt()
	if success == nil {
		return nil, apierror.New(apierror.CodeInvalidStateTransition,
			"this payment has no captured funds to refund")
	}
	gw := success.GatewayID()

	ref, err := pay.AddRefund(amount, reason, idemKey, o.deps.Clock)
	if err != nil {
		return nil, err
	}
	if err := o.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Payments.Save(ctx, pay)
	}); err != nil {
		return nil, err
	}

	client, creds, ext, err := o.deps.Gateways.Resolve(ctx, pay.MerchantID(), gw)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, o.deps.Settings.GatewayTimeout)
	defer cancel()

	res, callErr := client.Refund(callCtx, spi.RefundRequest{
		IdempotencyKey: idemKey, Credentials: creds, ExternalAccountID: ext,
		PaymentID: pay.ID(), RefundID: ref.ID(), GatewayRef: success.GatewayRef(),
		Amount: amount, Reason: reason,
	})
	now := o.deps.Clock.Now()

	switch {
	case errors.Is(callErr, spi.ErrOutcomeUnknown) || isTimeout(callErr):
		// The refund stays PENDING and enters reconciliation. It is emphatically not retried:
		// a duplicate refund is a duplicate payout.
		pay.RequireReconciliation(success.ID(), "refund outcome unknown", o.deps.Clock)
	case callErr != nil:
		if err := ref.MarkFailed(string(apierror.CodeOf(callErr)), safeMessage(callErr), now); err != nil {
			return nil, err
		}
	case res == nil:
		pay.RequireReconciliation(success.ID(), "refund adapter returned no result", o.deps.Clock)
	case res.Status == spi.StatusRefundAccepted || res.Status == spi.StatusPending:
		if err := ref.MarkSubmitted(res.GatewayRef, now); err != nil {
			return nil, err
		}
	case res.Status == spi.StatusRefunded:
		if err := ref.MarkSubmitted(res.GatewayRef, now); err != nil {
			return nil, err
		}
		if err := pay.ConfirmRefund(ref.ID(), res.GatewayRef, o.deps.Clock); err != nil {
			return nil, err
		}
	default:
		if err := ref.MarkFailed(res.RawCode, res.RawMessage, now); err != nil {
			return nil, err
		}
	}

	if err := o.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		if err := r.Payments.Save(ctx, pay); err != nil {
			return err
		}
		return o.deps.Audit.Record(ctx, r, "payment.refunded", "payment", pay.ID().String(), "SUCCESS",
			map[string]any{"refundId": ref.ID().String(), "amount": amount.Amount(),
				"currency": string(amount.Currency()), "status": string(ref.Status())})
	}); err != nil {
		return nil, err
	}
	return o.reloadResult(ctx, pay.ID(), nil)
}

// followUp is the shared shape of capture and void: one call to the gateway that already holds
// the authorization, with no routing and no failover.
func (o *Orchestrator) followUp(ctx context.Context, pay *payment.Payment, m MerchantSnapshot,
	gw shared.GatewayID, op shared.Operation,
	call func(spi.PaymentGateway, context.Context, *payment.Attempt, spi.Credentials, string) (*spi.Result, error),
	apply func(*spi.Result) error) (*Result, error) {

	breakerKey := gw.String() + ":" + string(op)
	record, err := o.deps.Breakers.Allow(breakerKey)
	if err != nil {
		return nil, apierror.Wrapf(err, apierror.CodeGatewayCircuitOpen, "gateway %s is not accepting traffic", gw)
	}
	release, err := o.deps.Bulkheads.Acquire(ctx, gw.String())
	if err != nil {
		record(false, false)
		return nil, err
	}
	defer release()

	client, creds, ext, err := o.deps.Gateways.Resolve(ctx, pay.MerchantID(), gw)
	if err != nil {
		record(false, false)
		return nil, err
	}

	att, err := pay.StartAttempt(gw, pay.RoutingPlanID(), op, o.deps.Clock)
	if err != nil {
		record(false, false)
		return nil, err
	}
	bindConnection(att, m, gw)
	if err := o.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		return r.Payments.SaveAttempt(ctx, att)
	}); err != nil {
		record(false, false)
		return nil, err
	}

	callCtx, cancel := context.WithTimeout(ctx, o.deps.Settings.GatewayTimeout)
	defer cancel()
	start := o.deps.Clock.Now()
	if err := att.Dispatch(start); err != nil {
		record(false, false)
		return nil, err
	}
	res, callErr := call(client, callCtx, att, creds, ext)
	now := o.deps.Clock.Now()
	o.deps.Metrics.ObserveGatewayRequest(gw, op, outcomeLabel(res, callErr), now.Sub(start))

	switch {
	case errors.Is(callErr, spi.ErrOutcomeUnknown) || isTimeout(callErr) || (res == nil && callErr == nil):
		record(false, true)
		if err := att.TimeOut(safeMessage(callErr), now); err != nil {
			return nil, err
		}
		pay.RequireReconciliation(att.ID(), string(op)+" outcome unknown", o.deps.Clock)
	case callErr != nil:
		record(false, true)
		if err := att.Fail(string(apierror.CodeOf(callErr)), safeMessage(callErr), now); err != nil {
			return nil, err
		}
	default:
		if err := o.validator.ValidateGatewayResponse(ctx, GatewayResponse{
			Status: res.Status, GatewayRef: res.GatewayRef,
			AuthorizedAmount: res.AuthorizedAmount, CapturedAmount: res.CapturedAmount,
			RawStatus: res.RawStatus, RawCode: res.RawCode,
		}, ExpectedResponse{
			Amount: pay.Amount(), Currency: pay.Currency(), Operation: op,
			GatewayID: gw, PaymentID: pay.ID(), AttemptID: att.ID(),
			CurrentState: pay.State(), AuthorizedAmount: pay.AuthorizedAmount(),
			CapturedTotal: pay.CapturedAmount(),
		}); err != nil {
			record(false, true)
			if terr := att.TimeOut("response failed validation: "+err.Error(), now); terr != nil {
				return nil, terr
			}
			pay.RequireReconciliation(att.ID(), "gateway response failed L6 validation", o.deps.Clock)
			break
		}
		record(true, true)
		if err := att.Succeed(res.GatewayRef, res.RawStatus, now); err != nil {
			return nil, err
		}
		if err := apply(res); err != nil {
			return nil, err
		}
	}

	if err := o.commit(ctx, pay, att, "payment."+string(op)); err != nil {
		return nil, err
	}
	return o.reloadResult(ctx, pay.ID(), nil)
}

func (o *Orchestrator) reload(ctx context.Context, id shared.PaymentID) (*payment.Payment, error) {
	var out *payment.Payment
	err := o.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		p, err := r.Payments.Get(ctx, id)
		out = p
		return err
	})
	return out, err
}

func (o *Orchestrator) reloadResult(ctx context.Context, id shared.PaymentID, plan *routing.Plan) (*Result, error) {
	p, err := o.reload(ctx, id)
	if err != nil {
		return nil, err
	}
	return &Result{Payment: p, Plan: plan}, nil
}

// isSameState reports whether an error is the state machine refusing a no-op transition, which
// is benign when the payment is already in the target state.
func isSameState(err error) bool {
	return apierror.CodeOf(err) == apierror.CodeInvalidStateTransition
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return apierror.CodeOf(err) == apierror.CodeGatewayTimeout
}

// safeMessage renders an error for storage on the attempt without leaking anything sensitive.
// It uses the platform error's caller-facing message rather than the full chain, because the
// chain can contain a URL, a header value or a driver message, and the attempt row is read by
// support staff.
func safeMessage(err error) string {
	if err == nil {
		return ""
	}
	if e := apierror.From(err); e != nil {
		return e.Message
	}
	return "unspecified error"
}

// bindConnection stamps the attempt with the merchant-to-gateway binding it is about to be
// dispatched over.
//
// # Why a failure here is swallowed rather than returned
//
// The connection reference is descriptive: it makes an attempt traceable to the credential that
// signed it, which is what a rotation post-mortem needs. It is not an input to any decision — the
// routing already chose the gateway, and the resolver already read the credential. Failing the
// dispatch because the descriptive field could not be set would convert a bookkeeping gap into a
// declined payment, which is the wrong trade in a system whose first rule is that no timer and no
// bookkeeping concern may fail a payment.
//
// The two ways it can fail are both benign and both mean "leave it blank": the merchant snapshot
// has no connection entry for this gateway (only reachable through a hand-built snapshot in a
// test, because the resolver would already have refused), and the attempt is already bound (only
// reachable on a re-entry, where the existing value is the correct one).
func bindConnection(att *payment.Attempt, m MerchantSnapshot, gw shared.GatewayID) {
	conn, ok := m.Connections[gw]
	if !ok || conn.ConnectionID == "" {
		return
	}
	_ = att.BindConnection(conn.ConnectionID)
}

func declineError(reason payment.DeclineReason) error {
	return apierror.Newf(apierror.CodeGatewayDeclined, "the payment was declined (%s)", reason)
}

func outcomeLabel(res *spi.Result, err error) string {
	switch {
	case errors.Is(err, spi.ErrOutcomeUnknown) || isTimeout(err):
		return "unknown"
	case err != nil:
		return "error"
	case res == nil:
		return "unknown"
	case res.Status == spi.StatusDeclined:
		return "declined"
	default:
		return "success"
	}
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-22, BR-23, FR-63, FR-64, FR-65, FR-66, FR-67.
//
// Attempt dispatch and failover: a retryable failure moves to the next candidate, a hard
// decline terminates, and an unknown outcome does neither
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
