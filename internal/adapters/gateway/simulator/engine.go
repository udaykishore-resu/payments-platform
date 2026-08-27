// Package simulator is an in-repo payment gateway that behaves like a real one.
//
// It exists because the alternative — a hand-written stub per test — produces a platform that is
// only ever exercised against the failure modes somebody remembered to fake. The simulator is the
// place where the failure modes live: a decline that must not fail over, a timeout after the
// request was written, a response that echoes the wrong amount, a webhook delivered twice with
// byte-identical content. Those behave the same way here every time, so a test that depends on one
// is deterministic, and a platform change that breaks the handling of one fails a test rather than
// a merchant.
//
// It has three parts, and the split is what makes it useful beyond unit tests:
//
//   - Engine is the deterministic core and implements all three SPI interfaces directly. Use it
//     in-process where the test is about the orchestrator rather than about HTTP.
//   - Server exposes the Engine over the simulator's own HTTP protocol, so an end-to-end test can
//     point a real adapter, through a real transport, at a real socket.
//   - Adapter is the client for that protocol and is itself an spi.PaymentGateway, so it passes the
//     same contract suite every vendor adapter does. That is the property that makes the simulator
//     trustworthy: it is not exempt from the rules it is used to check.
//
// Behaviour is selected by the amount's minor units or by an injected scenario; the trigger table
// is documented in scenario.go.
package simulator

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// GatewayID is the simulator's registry slug.
const GatewayID shared.GatewayID = "simulator"

// Credential field names the simulator expects. They exist so that the credential-handling paths —
// including the assertion that a credential never appears in an error — are exercised against the
// simulator exactly as they are against a real gateway.
const (
	CredentialAPIKey        = "api_key"
	CredentialWebhookSecret = "webhook_secret"
)

// EngineOptions configures the deterministic core.
type EngineOptions struct {
	Clock shared.Clock
	// SlowDelay is how long ScenarioSlow waits. It is a parameter rather than a constant because a
	// unit test wants milliseconds and a load test wants something near the real gateway's p99.
	SlowDelay time.Duration
	// Scheme selects which vendor's webhook signing the simulator emulates when it emits events.
	Scheme WebhookScheme
	// WebhookSecret is the signing material used for emitted webhooks. For the Adyen scheme it must
	// be hex, because that is what Adyen's keys are and what the verifier will decode.
	WebhookSecret string
	// MerchantAccount is echoed in Adyen-scheme webhooks, which sign over it.
	MerchantAccount string
}

func (o EngineOptions) withDefaults() EngineOptions {
	if o.Clock == nil {
		o.Clock = shared.SystemClock{}
	}
	if o.SlowDelay <= 0 {
		o.SlowDelay = 2 * time.Second
	}
	if o.Scheme == "" {
		o.Scheme = SchemeSimulator
	}
	if o.WebhookSecret == "" {
		// A deterministic default so a test that does not care about rotation does not have to
		// invent one. It is hex so that it is valid for the Adyen scheme too.
		o.WebhookSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	}
	if o.MerchantAccount == "" {
		o.MerchantAccount = "SimulatorMerchantECOM"
	}
	return o
}

// Engine is the simulator's deterministic core.
//
// It is safe for concurrent use and holds all its state in memory: the point is reproducibility,
// and a simulator with a database has a second thing that can be in the wrong state when a test
// fails.
type Engine struct {
	opts EngineOptions

	mu sync.Mutex
	// stored is the idempotency ledger. A repeated key returns the *identical* stored response,
	// which is the behaviour every real gateway promises and the one an adapter most needs to be
	// tested against — an adapter that sends the key but ignores the replayed answer looks correct
	// until the day a retry happens.
	stored map[string]*WireResponse
	// records is what Lookup reads, keyed by reference and by idempotency key so that a lookup with
	// no reference — the reconciliation case — actually resolves.
	byRef      map[string]*WireResponse
	byKey      map[string]*WireResponse
	accounts   map[string]string // account id -> merchant id
	byMerchant map[string]string // merchant id -> account id
	hooks      map[string]WireWebhookResponse
	emitted    []EmittedWebhook
	seq        int64
}

var (
	_ spi.PaymentGateway     = (*Engine)(nil)
	_ spi.GatewayProvisioner = (*Engine)(nil)
	_ spi.WebhookVerifier    = (*Engine)(nil)
)

// NewEngine builds the deterministic core.
func NewEngine(opts EngineOptions) *Engine {
	return &Engine{
		opts:       opts.withDefaults(),
		stored:     make(map[string]*WireResponse),
		byRef:      make(map[string]*WireResponse),
		byKey:      make(map[string]*WireResponse),
		accounts:   make(map[string]string),
		byMerchant: make(map[string]string),
		hooks:      make(map[string]WireWebhookResponse),
	}
}

// ID returns the registry slug.
func (e *Engine) ID() shared.GatewayID { return GatewayID }

// nextRef mints a reference. It is a monotonic counter rather than a random value so that a failing
// test's output is stable across runs and diffable.
func (e *Engine) nextRef(prefix string) string {
	e.seq++
	return prefix + "_" + strconv.FormatInt(e.seq, 10)
}

// Execute is the single entry point the HTTP server and the in-process interfaces share.
//
// Having one implementation of the behaviour means the simulator cannot answer differently
// depending on whether it was reached through a socket — which would make an end-to-end failure
// impossible to reproduce in a unit test, the exact property a simulator exists to provide.
func (e *Engine) Execute(ctx context.Context, req WireRequest) (*WireResponse, Scenario, error) {
	amt := money.Money{}
	if req.Amount != nil {
		if m, err := money.New(req.Amount.MinorUnits, money.Currency(strings.ToUpper(req.Amount.Currency))); err == nil {
			amt = m
		}
	}
	scenario := ResolveScenario(req.Metadata, amt)

	if req.Operation == opLookup {
		return e.lookup(req), scenario, nil
	}
	if req.IdempotencyKey == "" {
		return nil, scenario, apierror.New(apierror.CodeIdempotencyKeyRequired,
			"simulator: every mutating call requires an idempotency key")
	}

	// The idempotency check happens before the scenario is applied, so a repeated key returns the
	// original answer even when the scenario would now produce a different one. That is what a real
	// gateway does, and it is the behaviour that catches an adapter which re-derives its result from
	// the request rather than from the response.
	e.mu.Lock()
	if prev, ok := e.stored[req.IdempotencyKey]; ok {
		clone := *prev
		e.mu.Unlock()
		return &clone, scenario, nil
	}
	e.mu.Unlock()

	switch scenario {
	case ScenarioTimeout:
		// In-process there is no socket to hold open, so the ambiguity is expressed directly. The
		// HTTP server holds the connection instead, which produces the same classification through
		// a different mechanism.
		return nil, scenario, apierror.Wrap(spi.ErrOutcomeUnknown, apierror.CodeGatewayTimeout,
			"simulator: the request was written and no answer arrived; the outcome is unknown")
	case ScenarioAuthFailure:
		return nil, scenario, apierror.Wrap(spi.ErrCredentialsInvalid, apierror.CodeGatewayAuthenticationFailed,
			"simulator: the credentials were rejected")
	case ScenarioGatewayError:
		return nil, scenario, apierror.Wrap(spi.ErrOutcomeUnknown, apierror.CodeGatewayTimeout,
			"simulator: the gateway returned 500 and does not guarantee the request was not processed")
	case ScenarioSlow:
		select {
		case <-ctx.Done():
			return nil, scenario, apierror.Wrap(ctx.Err(), apierror.CodeGatewayTimeout,
				"simulator: the deadline expired while the gateway was slow")
		case <-time.After(e.opts.SlowDelay):
		}
	default:
		// Every other scenario is expressed in the response body rather than in the call's outcome,
		// so it is built below rather than short-circuited here
	}

	resp := e.build(req, amt, scenario)

	e.mu.Lock()
	e.stored[req.IdempotencyKey] = resp
	e.byKey[req.IdempotencyKey] = resp
	if resp.Reference != "" {
		e.byRef[resp.Reference] = resp
	}
	e.mu.Unlock()

	clone := *resp
	return &clone, scenario, nil
}

func (e *Engine) build(req WireRequest, amt money.Money, scenario Scenario) *WireResponse {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := &WireResponse{Scenario: string(scenario)}
	switch req.Operation {
	case opAuthorize:
		out.Reference = e.nextRef("sim_pay")
	case opRefund:
		out.Reference = e.nextRef("sim_rfnd")
	default:
		out.Reference = e.nextRef("sim_mod")
	}

	switch scenario {
	case ScenarioDeclineInsufficientFunds, ScenarioDeclineDoNotHonor,
		ScenarioDeclineStolenCard, ScenarioDeclineUnmapped:
		code, msg := declineCodeFor(scenario)
		out.Status = string(spi.StatusDeclined)
		out.RawStatus = "refused"
		out.DeclineCode = code
		out.RawCode = code
		out.RawMessage = msg
		out.NetworkAdviceNoRetry = scenario == ScenarioDeclineStolenCard
		return out

	case ScenarioRequiresAction:
		out.Status = string(spi.StatusRequiresAction)
		out.RawStatus = "requires_action"
		out.NextAction = &WireNextAction{
			Type:        string(payment.ActionThreeDSChall),
			RedirectURL: "https://simulator.invalid/3ds/" + out.Reference,
		}
		return out

	case ScenarioPending:
		out.Status = string(spi.StatusPending)
		out.RawStatus = "pending"
		return out

	case ScenarioAmountMismatch:
		// Deliberately wrong in both dimensions: a larger amount and a different currency. Either
		// alone is a contract violation; sending both means an adapter cannot pass by checking only
		// the one its author happened to think of.
		out.Status = statusForOperation(req.Operation, req.Capture)
		out.RawStatus = "succeeded"
		mismatch := &WireAmount{MinorUnits: amt.Amount() + 100, Currency: mismatchCurrency(amt)}
		out.AuthorizedAmount = mismatch
		out.CapturedAmount = mismatch
		return out
	default:
		// Approve and the transport-level scenarios (timeout, slow, gateway error, auth failure,
		// malformed, duplicate webhook) leave the success response built above untouched: they are
		// handled by the caller or by the HTTP layer, not by the body
	}

	out.Status = statusForOperation(req.Operation, req.Capture)
	out.RawStatus = "succeeded"
	out.AuthCode = "SIM" + strconv.FormatInt(e.seq, 10)
	out.AVSResult = "Y"
	out.CVVResult = "M"
	if req.Amount != nil {
		echo := &WireAmount{MinorUnits: req.Amount.MinorUnits, Currency: strings.ToUpper(req.Amount.Currency)}
		switch req.Operation {
		case opAuthorize:
			if req.Capture {
				out.CapturedAmount = echo
			} else {
				out.AuthorizedAmount = echo
			}
		case opCapture, opRefund:
			out.CapturedAmount = echo
		}
	}
	return out
}

// statusForOperation gives each operation its successful normalized status.
//
// The authorize arm reads `capture` because a sale and an authorization are different outcomes, and
// a simulator that reported both as AUTHORIZED would let an adapter forget to send the vendor's
// capture-method parameter — the bug that silently takes the payer's money on every
// authorization-only payment.
//
// Note the refund arm: REFUND_ACCEPTED, never REFUNDED. Refunds are asynchronous at every real
// gateway, and a simulator that reported them as final would let an adapter pass a test it would
// fail in production.
func statusForOperation(op string, capture bool) string {
	switch op {
	case opAuthorize:
		if capture {
			return string(spi.StatusCaptured)
		}
		return string(spi.StatusAuthorized)
	case opCapture:
		return string(spi.StatusCaptured)
	case opRefund:
		return string(spi.StatusRefundAccepted)
	case opVoid:
		return string(spi.StatusVoided)
	default:
		return string(spi.StatusPending)
	}
}

// mismatchCurrency picks a currency that is definitely not the requested one, so the echo check has
// something unambiguous to fail on.
func mismatchCurrency(m money.Money) string {
	if m.Currency() == "EUR" {
		return "USD"
	}
	return "EUR"
}

func (e *Engine) lookup(req WireRequest) *WireResponse {
	e.mu.Lock()
	defer e.mu.Unlock()
	if req.Reference != "" {
		if r, ok := e.byRef[req.Reference]; ok {
			clone := *r
			return &clone
		}
	}
	if req.IdempotencyKey != "" {
		if r, ok := e.byKey[req.IdempotencyKey]; ok {
			clone := *r
			return &clone
		}
	}
	// NOT_FOUND is a finding, not a failure: with a deterministic idempotency key it is positive
	// evidence the operation never took effect.
	return &WireResponse{Status: string(spi.StatusNotFound)}
}

// --- spi.PaymentGateway, in-process ------------------------------------------------------------

// Authorize places a hold or takes funds, per the resolved scenario.
func (e *Engine) Authorize(ctx context.Context, req spi.AuthorizeRequest) (*spi.Result, error) {
	return e.run(ctx, shared.OpAuthorize, WireRequest{
		Operation:      opAuthorize,
		IdempotencyKey: req.IdempotencyKey,
		Amount:         wireAmount(req.Amount),
		Capture:        req.Capture,
		PaymentID:      req.PaymentID.String(),
		AttemptID:      req.AttemptID.String(),
		ReturnURL:      req.ReturnURL,
		ThreeDS:        req.ThreeDS.Requested,
		Metadata:       req.Metadata,
	}, req.Amount)
}

// Capture converts a hold into a debit.
func (e *Engine) Capture(ctx context.Context, req spi.CaptureRequest) (*spi.Result, error) {
	return e.run(ctx, shared.OpCapture, WireRequest{
		Operation:      opCapture,
		IdempotencyKey: req.IdempotencyKey,
		Reference:      req.GatewayRef,
		Amount:         wireAmount(req.Amount),
		Final:          req.Final,
		PaymentID:      req.PaymentID.String(),
		Metadata:       req.Metadata,
	}, req.Amount)
}

// Refund returns captured funds.
func (e *Engine) Refund(ctx context.Context, req spi.RefundRequest) (*spi.Result, error) {
	return e.run(ctx, shared.OpRefund, WireRequest{
		Operation:      opRefund,
		IdempotencyKey: req.IdempotencyKey,
		Reference:      req.GatewayRef,
		Amount:         wireAmount(req.Amount),
		PaymentID:      req.PaymentID.String(),
		RefundID:       req.RefundID.String(),
		Reason:         string(req.Reason),
		Metadata:       req.Metadata,
	}, req.Amount)
}

// Void releases an uncaptured authorization.
func (e *Engine) Void(ctx context.Context, req spi.VoidRequest) (*spi.Result, error) {
	return e.run(ctx, shared.OpVoid, WireRequest{
		Operation:      opVoid,
		IdempotencyKey: req.IdempotencyKey,
		Reference:      req.GatewayRef,
		PaymentID:      req.PaymentID.String(),
		Metadata:       req.Metadata,
	}, money.Money{})
}

// Lookup resolves a transaction by reference or by idempotency key alone.
func (e *Engine) Lookup(ctx context.Context, req spi.LookupRequest) (*spi.Result, error) {
	return e.run(ctx, shared.OpLookup, WireRequest{
		Operation:      opLookup,
		IdempotencyKey: req.IdempotencyKey,
		Reference:      req.GatewayRef,
	}, money.Money{})
}

func (e *Engine) run(ctx context.Context, op shared.Operation, req WireRequest, requested money.Money) (*spi.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	resp, _, err := e.Execute(ctx, req)
	if err != nil {
		return nil, err
	}
	return toResult(resp, requested, req.IdempotencyKey, e.opts.Clock.Now(), 0)
}

func wireAmount(m money.Money) *WireAmount {
	if !m.IsValid() {
		return nil
	}
	return &WireAmount{MinorUnits: m.Amount(), Currency: string(m.Currency())}
}

func contextError(err error) error {
	return apierror.Wrap(err, apierror.CodeServiceUnavailable,
		"simulator: the call was cancelled before it was made")
}
