package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	apponboarding "github.com/udaykishore-resu/payments-platform/internal/application/onboarding"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/onboarding"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// OnboardingService is the durable onboarding workflow this file exposes.
type OnboardingService interface {
	Start(ctx context.Context, cmd apponboarding.StartCommand) (*apponboarding.Case, error)
	Get(ctx context.Context, tenantID shared.TenantID, id shared.WorkflowID) (*apponboarding.Case, error)
	Signal(ctx context.Context, cmd apponboarding.SignalCommand) (*apponboarding.Case, error)
}

// MerchantCaseLookup resolves a merchant to its live workflow instance.
//
// It is a separate interface from [OnboardingService] because the REST surface is addressed by
// merchant — the URL is `/v1/merchants/{merchantId}/onboarding` — while the workflow engine is
// addressed by workflow instance. Something has to bridge the two, and making it an explicit
// dependency keeps the bridging out of the handler, where it would be business logic.
type MerchantCaseLookup interface {
	WorkflowFor(ctx context.Context, tenantID shared.TenantID, m shared.MerchantID) (shared.WorkflowID, error)
}

func registerOnboarding(rt *httpapi.Router, d Deps) {
	h := &onboardingHandlers{svc: d.Onboarding, lookup: d.OnboardingLookup, baseURL: d.BaseURL}
	rt.Handle(http.MethodPost, httpapi.RouteStartOnboarding, "startOnboarding", h.start)
	rt.Handle(http.MethodGet, httpapi.RouteGetOnboarding, "getOnboardingCase", h.get)
	rt.Handle(http.MethodPost, httpapi.RouteOnboardingSignal, "sendOnboardingSignal", h.signal)
}

type onboardingHandlers struct {
	svc     OnboardingService
	lookup  MerchantCaseLookup
	baseURL string
}

// start implements `startOnboarding`.
//
// # 200 rather than 201 on a second call
//
// The workflow's business key is the merchant id, which guarantees exactly one live instance per
// merchant. Starting it twice is therefore a no-op that returns the existing case — with 200, not
// 201, and not 409.
//
// 409 would be defensible and is wrong here: a client retrying a start it is not sure succeeded
// is the *expected* behaviour after a network timeout, and answering it with a conflict makes
// the correct client behaviour look like an error. 200-with-the-existing-case is the answer that
// makes a retry indistinguishable from a first call, which is what idempotency means.
func (h *onboardingHandlers) start(w http.ResponseWriter, r *http.Request) error {
	id, tc, err := merchantTarget(r)
	if err != nil {
		return err
	}
	var req httpapi.StartOnboardingRequest
	if err := decodeInto(r, &req); err != nil {
		return err
	}
	if len(req.SelectedGateways) == 0 {
		return apierror.New(apierror.CodeValidationFailed,
			"at least one gateway must be selected to start onboarding").
			WithDetail(apierror.Detail{
				Field: "selectedGateways", Code: "EMPTY",
				Message: "Onboarding provisions gateway connections; with none selected there is nothing to do.",
				RuleID:  "L1.SELECTED_GATEWAYS_PRESENT",
			})
	}
	env, err := shared.ParseEnvironment(strings.ToLower(req.Environment))
	if err != nil {
		return err
	}

	existing := h.existingCase(r, tc.TenantID, id)

	input := onboarding.Input{
		MerchantID:  id,
		TenantID:    tc.TenantID,
		Environment: env,
		RequestedBy: onboardingActor(r).ID,
	}
	for _, code := range req.SupportedCurrencies {
		cur, err := money.ParseCurrency(code)
		if err != nil {
			return err
		}
		input.Currencies = append(input.Currencies, cur)
	}
	for _, code := range req.PaymentMethods {
		m, err := shared.ParsePaymentMethod(code)
		if err != nil {
			return err
		}
		input.PaymentMethods = append(input.PaymentMethods, m)
	}
	for _, code := range req.SelectedGateways {
		g, err := shared.ParseGatewayID(code)
		if err != nil {
			return err
		}
		input.Gateways = append(input.Gateways, g)
	}
	c, err := h.svc.Start(r.Context(), apponboarding.StartCommand{
		TenantID: tc.TenantID,
		Input:    input,
		Actor:    onboardingActor(r),
	})
	if err != nil {
		return err
	}
	body := onboardingCaseOf(c)
	httpapi.SetETagRaw(w, caseETag(c))
	if existing != nil && existing.WorkflowID == c.WorkflowID {
		httpapi.WriteJSON(w, r, http.StatusOK, body)
		return nil
	}
	httpapi.SetLocation(w, h.baseURL, "/v1/merchants/"+id.String()+"/onboarding")
	httpapi.WriteJSON(w, r, http.StatusCreated, body)
	return nil
}

// existingCase reports the merchant's live case, or nil.
//
// A lookup failure is nil rather than an error: the only consequence of not knowing is that a
// genuinely-new case is reported as 201 and a resumed one might be too, and failing the whole
// start because the lookup was unavailable would be a worse trade.
func (h *onboardingHandlers) existingCase(r *http.Request, t shared.TenantID, m shared.MerchantID) *apponboarding.Case {
	if h.lookup == nil {
		return nil
	}
	wid, err := h.lookup.WorkflowFor(r.Context(), t, m)
	if err != nil || wid == "" {
		return nil
	}
	c, err := h.svc.Get(r.Context(), t, wid)
	if err != nil {
		return nil
	}
	return c
}

// get implements `getOnboardingCase`.
func (h *onboardingHandlers) get(w http.ResponseWriter, r *http.Request) error {
	id, tc, err := merchantTarget(r)
	if err != nil {
		return err
	}
	if h.lookup == nil {
		return apierror.New(apierror.CodeOnboardingCaseNotFound,
			"this deployment cannot resolve an onboarding case by merchant")
	}
	wid, err := h.lookup.WorkflowFor(r.Context(), tc.TenantID, id)
	if err != nil {
		return err
	}
	c, err := h.svc.Get(r.Context(), tc.TenantID, wid)
	if err != nil {
		return err
	}
	httpapi.SetETagRaw(w, caseETag(c))
	httpapi.WriteJSON(w, r, http.StatusOK, onboardingCaseOf(c))
	return nil
}

// signal implements `sendOnboardingSignal`.
//
// # Why 202 and not the resulting case
//
// The signal is durably recorded and the workflow resumes asynchronously. Returning the
// post-signal case would mean holding the HTTP request open across a workflow step that can call
// a gateway, a KYC provider or a certification suite — minutes, not milliseconds. So the contract
// says 202 and poll, and this handler returns the acknowledgement.
//
// The `compliance-approval` signal is the manual gate at step 11: it requires the
// `onboarding:approve` scope (enforced by the authorization middleware from the route table), is
// subject to dual control (enforced by the policy engine), and the signal itself — actor, scopes,
// source address, reason and attestation reference — is written to the audit trail by the
// application service. None of that is this handler's job, and that is the point.
func (h *onboardingHandlers) signal(w http.ResponseWriter, r *http.Request) error {
	id, tc, err := merchantTarget(r)
	if err != nil {
		return err
	}
	name, err := pathValue(r, "signal")
	if err != nil {
		return err
	}
	if !isKnownSignal(name) {
		return apierror.Newf(apierror.CodeWorkflowSignalNotExpected,
			"unknown onboarding signal %q", name).
			WithDetail(apierror.Detail{
				Field: "signal", Code: "UNKNOWN_SIGNAL",
				Message: "Valid signals are kyc-decision, compliance-approval and gateway-consent.",
				RuleID:  "L1.ENUM_MEMBER",
			})
	}
	var req httpapi.OnboardingSignalRequest
	if err := decodeInto(r, &req); err != nil {
		return err
	}
	if req.Decision == "" {
		return apierror.New(apierror.CodeValidationFailed, "a decision is required").
			WithDetail(apierror.Detail{
				Field: "decision", Code: "MISSING",
				Message: "Supply the decision this signal carries.",
				RuleID:  "L1.SIGNAL_DECISION_PRESENT",
			})
	}
	if h.lookup == nil {
		return apierror.New(apierror.CodeOnboardingCaseNotFound,
			"this deployment cannot resolve an onboarding case by merchant")
	}
	wid, err := h.lookup.WorkflowFor(r.Context(), tc.TenantID, id)
	if err != nil {
		return err
	}
	// The signal payload is re-encoded from the decoded struct rather than forwarded raw. The
	// workflow engine stores it and replays it on resume, and storing whatever bytes a caller
	// sent would let an unknown field survive into a replay months later, where it would be
	// decoded by a definition that has since changed meaning.
	payload, err := json.Marshal(req)
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "the signal payload could not be encoded")
	}
	c, err := h.svc.Signal(r.Context(), apponboarding.SignalCommand{
		TenantID:       tc.TenantID,
		WorkflowID:     wid,
		Name:           name,
		Data:           payload,
		IdempotencyKey: r.Header.Get(httpapi.HeaderIdempotencyKey),
		Actor:          onboardingActor(r),
	})
	if err != nil {
		return err
	}
	httpapi.WriteJSON(w, r, http.StatusAccepted, httpapi.OnboardingSignalAccepted{
		CaseID:             c.WorkflowID.String(),
		WorkflowInstanceID: c.WorkflowID.String(),
		Signal:             name,
		Accepted:           true,
		AcceptedAt:         c.UpdatedAt,
	})
	return nil
}

func isKnownSignal(name string) bool {
	switch name {
	case "kyc-decision", "compliance-approval", "gateway-consent":
		return true
	}
	return false
}

// onboardingActor builds the workflow actor from the authenticated principal, carrying the scopes
// so the service's own dual-control check can see them.
func onboardingActor(r *http.Request) apponboarding.Actor {
	p := httpapi.Principal(r.Context())
	if p == nil {
		return apponboarding.Actor{}
	}
	return apponboarding.Actor{
		ID:     p.ID,
		Name:   p.Name,
		Scopes: p.AllScopes(),
		IP:     r.RemoteAddr,
	}
}

// caseETag derives the case's concurrency token from its updated timestamp.
//
// The workflow instance carries no version counter — its identity is its step history — so the
// last-modified instant in nanoseconds is the closest available monotone token. It is exact
// enough for a conditional read and is never used for a conditional *write*: the workflow is
// advanced by signals, which are idempotent per (instance, signal name) and need no ETag.
func caseETag(c *apponboarding.Case) string {
	if c == nil {
		return ""
	}
	return strconvItoa64(c.UpdatedAt.UTC().UnixNano())
}

func strconvItoa64(v int64) string {
	return time.Unix(0, v).UTC().Format(time.RFC3339Nano)
}

// onboardingCaseOf renders a workflow instance as the contract's onboarding case.
func onboardingCaseOf(c *apponboarding.Case) httpapi.OnboardingCase {
	if c == nil {
		return httpapi.OnboardingCase{}
	}
	out := httpapi.OnboardingCase{
		ID:                 c.WorkflowID.String(),
		MerchantID:         c.MerchantID.String(),
		WorkflowInstanceID: c.WorkflowID.String(),
		WorkflowName:       c.Definition,
		Status:             caseStatusOf(c),
		CurrentStepKey:     c.CurrentStep,
		BlockedReason:      c.LastError,
		Steps:              make([]httpapi.OnboardingStep, 0, len(c.Steps)),
		OpenedAt:           c.CreatedAt,
		ClosedAt:           c.CompletedAt,
		Version:            int64(len(c.Steps)),
	}
	for _, s := range c.Steps {
		step := httpapi.OnboardingStep{
			ID:             c.WorkflowID.String() + ":" + s.Name,
			Key:            s.Name,
			Sequence:       s.Sequence,
			State:          stepStateOf(s),
			AttemptCount:   s.Attempt,
			AwaitingSignal: awaitingSignalOf(c, s),
			StartedAt:      s.StartedAt,
			EndedAt:        s.CompletedAt,
			ErrorMessage:   s.Error,
		}
		out.Steps = append(out.Steps, step)
	}
	return out
}

// caseStatusOf projects the engine's instance state onto the contract's four case statuses.
//
// A case waiting on a manual gate is BLOCKED rather than OPEN, because "blocked" is what an
// operator reading a queue needs to see: OPEN says the workflow is progressing on its own, and a
// case waiting for a human that reads as OPEN is a case nobody picks up.
func caseStatusOf(c *apponboarding.Case) string {
	switch {
	case c.AwaitingSignal != "":
		return "BLOCKED"
	case c.State.IsFinal() && c.LastError != "":
		return "ABANDONED"
	case c.State.IsFinal():
		return "COMPLETED"
	default:
		return "OPEN"
	}
}

func stepStateOf(s apponboarding.StepView) string {
	switch strings.ToUpper(s.State) {
	case "SUCCEEDED", "COMPLETED":
		return "SUCCEEDED"
	case "FAILED":
		return "FAILED"
	case "RUNNING":
		return "RUNNING"
	case "WAITING_SIGNAL", "WAITING":
		return "WAITING_SIGNAL"
	case "COMPENSATING":
		return "COMPENSATING"
	case "COMPENSATED":
		return "COMPENSATED"
	case "SKIPPED":
		return "SKIPPED"
	default:
		return "PENDING"
	}
}

func awaitingSignalOf(c *apponboarding.Case, s apponboarding.StepView) string {
	if c.CurrentStep == s.Name {
		return c.AwaitingSignal
	}
	return ""
}
