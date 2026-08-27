package handlers

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	apppayment "github.com/udaykishore-resu/payments-platform/internal/application/payment"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// PaymentService is the money path this file exposes.
//
// It is the application service's own signature set, narrowed to what the REST surface calls.
// Narrowing rather than aliasing the concrete type is what lets a handler test run with a
// hand-written double instead of a service that needs a unit of work, a gateway registry, a risk
// evaluator and a clock.
type PaymentService interface {
	Create(ctx context.Context, cmd apppayment.CreateCommand) (*apppayment.Result, error)
	Get(ctx context.Context, id shared.PaymentID) (*payment.Payment, error)
	List(ctx context.Context, f ports.PaymentFilter, page ports.Page) ([]*payment.Payment, string, error)
	Capture(ctx context.Context, cmd apppayment.CaptureCommand) (*apppayment.Result, error)
	Refund(ctx context.Context, cmd apppayment.RefundCommand) (*apppayment.Result, error)
	Void(ctx context.Context, cmd apppayment.VoidCommand) (*apppayment.Result, error)
}

func registerPayments(rt *httpapi.Router, d Deps) {
	h := &paymentHandlers{svc: d.Payments, baseURL: d.BaseURL}
	rt.Handle(http.MethodPost, httpapi.RouteCreatePayment, "createPayment", h.create)
	rt.Handle(http.MethodGet, httpapi.RouteListPayments, "listPayments", h.list)
	rt.Handle(http.MethodGet, httpapi.RouteGetPayment, "getPayment", h.get)
	rt.Handle(http.MethodPost, httpapi.RouteCapturePayment, "capturePayment", h.capture)
	rt.Handle(http.MethodPost, httpapi.RouteRefundPayment, "refundPayment", h.refund)
	rt.Handle(http.MethodPost, httpapi.RouteVoidPayment, "voidPayment", h.void)
}

type paymentHandlers struct {
	svc     PaymentService
	baseURL string
}

// create implements `createPayment`.
//
// # The status code carries the outcome, and 202 is not a failure
//
// 201 means the payment reached a synchronous terminal-for-now state. 202 means the outcome is
// not yet known — the gateway timed out, or the method is asynchronous — and the payment is
// PROCESSING or PENDING.
//
// The distinction is the single most important thing on this surface. Baseline §12.3: if the
// gateway call times out we do not know whether money moved, so the attempt is recorded
// TIMEOUT_UNKNOWN, the payment stays PROCESSING, and this endpoint returns 202 rather than an
// error. Returning 5xx there would tell the client the payment failed, and a client told a
// payment failed re-issues it with a fresh idempotency key — which is how one customer gets
// charged twice. **No timer may fail a payment**, and this status mapping is where that rule is
// enforced at the edge.
func (h *paymentHandlers) create(w http.ResponseWriter, r *http.Request) error {
	var req httpapi.CreatePaymentRequest
	if err := decodeInto(r, &req); err != nil {
		return err
	}
	tc, err := tenantctx.FromContext(r.Context())
	if err != nil {
		return err
	}
	amount, err := req.Amount.ToDomain()
	if err != nil {
		return err
	}
	if !amount.IsPositive() {
		return apierror.New(apierror.CodeAmountInvalid, "amount must be greater than zero").
			WithDetail(apierror.Detail{
				Field: "amount.amount", Code: "NOT_POSITIVE",
				Message: "A payment amount must be at least one minor unit.",
				RuleID:  "L1.AMOUNT_POSITIVE",
			})
	}
	merchantID, err := shared.ParseMerchantID(req.MerchantID)
	if err != nil {
		return err
	}
	method, err := shared.ParsePaymentMethod(req.PaymentMethod)
	if err != nil {
		return err
	}

	cmd := apppayment.CreateCommand{
		TenantID:       tc.TenantID,
		MerchantID:     merchantID,
		Amount:         amount,
		Method:         method,
		MethodRef:      req.PaymentMethodReference.ToDomain(),
		CaptureMethod:  captureMethodOf(req.CaptureMode),
		StatementRef:   req.StatementDescriptor,
		Reference:      req.Reference,
		Metadata:       req.Metadata,
		IdempotencyKey: r.Header.Get(httpapi.HeaderIdempotencyKey),
		CorrelationID:  httpapi.CorrelationID(r.Context()),
		RequestID:      httpapi.RequestID(r.Context()),
	}
	if req.PayerCountry != "" {
		c, err := shared.ParseCountry(req.PayerCountry)
		if err != nil {
			return err
		}
		cmd.Customer = payment.CustomerReference{Country: c}
	}

	res, err := h.svc.Create(r.Context(), cmd)
	if err != nil {
		return err
	}
	body := paymentBody(res, nil)
	status := http.StatusCreated
	if res.Payment.State() == payment.StateProcessing || res.Payment.State() == payment.StatePending {
		status = http.StatusAccepted
		// Retry-After on a 202 is the polling interval, not a backoff from an error. One second
		// is the reconciler's own first-pass latency: telling a client to poll faster than the
		// resolution path can possibly move would generate load that changes nothing.
		w.Header().Set(httpapi.HeaderRetryAfter, "1")
	}
	httpapi.SetETag(w, int64(res.Payment.Version()))
	httpapi.SetLocation(w, h.baseURL, "/v1/payments/"+res.Payment.ID().String())
	httpapi.WriteJSON(w, r, status, body)
	return nil
}

// get implements `getPayment`.
//
// This is the endpoint the contract tells clients to poll after a 202, after a 504, and while a
// payment is REQUIRES_ACTION — so it supports conditional reads. A client polling every second
// with If-None-Match gets a 304 with no body until something changes, which turns a polling loop
// from a bandwidth cost into a header exchange.
func (h *paymentHandlers) get(w http.ResponseWriter, r *http.Request) error {
	raw, err := pathValue(r, "paymentId")
	if err != nil {
		return err
	}
	id, err := shared.ParsePaymentID(raw)
	if err != nil {
		return err
	}
	p, err := h.svc.Get(r.Context(), id)
	if err != nil {
		return err
	}
	etag := strconv.FormatInt(int64(p.Version()), 10)
	if notModified(w, r, etag) {
		return nil
	}
	httpapi.SetETag(w, int64(p.Version()))
	httpapi.WriteJSON(w, r, http.StatusOK, httpapi.PaymentOf(p, expandSet(r)))
	return nil
}

// list implements `listPayments`.
func (h *paymentHandlers) list(w http.ResponseWriter, r *http.Request) error {
	page, err := httpapi.DecodePage(r)
	if err != nil {
		return err
	}
	filter, err := paymentFilter(r)
	if err != nil {
		return err
	}
	items, next, err := h.svc.List(r.Context(), filter, ports.Page{Limit: page.Limit, Cursor: page.Cursor})
	if err != nil {
		return err
	}
	out := make([]httpapi.Payment, 0, len(items))
	for _, p := range items {
		// A listing embeds neither attempts nor refunds. A page of 200 payments each with a
		// dozen attempts is a megabyte-scale response for a screen that shows a table of
		// amounts and states, and the client that needs the detail is one GET away from it.
		out = append(out, httpapi.PaymentOf(p, map[string]bool{}))
	}
	httpapi.WriteJSON(w, r, http.StatusOK, httpapi.PageOf(out, next))
	return nil
}

// capture implements `capturePayment`.
//
// Omitting `amount` captures the full remaining authorized amount, which is what the contract
// says an absent field means — hence the pointer in [httpapi.CaptureRequest] and the nil here.
// A zero-valued Money would be indistinguishable from "capture nothing".
func (h *paymentHandlers) capture(w http.ResponseWriter, r *http.Request) error {
	id, tc, err := paymentTarget(r)
	if err != nil {
		return err
	}
	var req httpapi.CaptureRequest
	if err := decodeInto(r, &req); err != nil {
		return err
	}
	amount, err := optionalMoney(req.Amount)
	if err != nil {
		return err
	}
	final := true
	if req.IsFinalCapture != nil {
		final = *req.IsFinalCapture
	}
	res, err := h.svc.Capture(r.Context(), apppayment.CaptureCommand{
		TenantID:       tc.TenantID,
		PaymentID:      id,
		Amount:         amount,
		Final:          final,
		IdempotencyKey: r.Header.Get(httpapi.HeaderIdempotencyKey),
		CorrelationID:  httpapi.CorrelationID(r.Context()),
	})
	if err != nil {
		return err
	}
	httpapi.SetETag(w, int64(res.Payment.Version()))
	status := http.StatusOK
	if res.Payment.State() == payment.StateProcessing {
		status = http.StatusAccepted
		w.Header().Set(httpapi.HeaderRetryAfter, "1")
	}
	httpapi.WriteJSON(w, r, status, paymentBody(res, nil))
	return nil
}

// refund implements `refundPayment`.
//
// The response is the Refund, not the Payment — the contract's choice, and the right one: a
// caller issuing a refund wants the refund's identifier and state so it can be tracked
// independently, and the payment's new totals travel with it as `refundedTotal` and
// `capturedTotal`.
//
// 201 when the gateway confirmed, 202 when it has not yet. The reserved amount already counts
// against invariant I1 in the 202 case, which is why a second refund racing the first cannot
// together exceed the captured total.
func (h *paymentHandlers) refund(w http.ResponseWriter, r *http.Request) error {
	id, tc, err := paymentTarget(r)
	if err != nil {
		return err
	}
	var req httpapi.RefundRequest
	if err := decodeInto(r, &req); err != nil {
		return err
	}
	if req.Reason == "" {
		return apierror.New(apierror.CodeValidationFailed, "a refund reason is required").
			WithDetail(apierror.Detail{
				Field: "reason", Code: "MISSING",
				Message: "Supply one of DUPLICATE, FRAUDULENT, REQUESTED_BY_CUSTOMER, OTHER.",
				RuleID:  "L1.REFUND_REASON_PRESENT",
			})
	}
	amount, err := optionalMoney(req.Amount)
	if err != nil {
		return err
	}
	res, err := h.svc.Refund(r.Context(), apppayment.RefundCommand{
		TenantID:       tc.TenantID,
		PaymentID:      id,
		Amount:         amount,
		Reason:         httpapi.RefundReasonToDomain(req.Reason),
		IdempotencyKey: r.Header.Get(httpapi.HeaderIdempotencyKey),
		CorrelationID:  httpapi.CorrelationID(r.Context()),
	})
	if err != nil {
		return err
	}
	refunds := res.Payment.Refunds()
	if len(refunds) == 0 {
		return apierror.New(apierror.CodeInternalError, "the refund was accepted but not recorded")
	}
	latest := refunds[len(refunds)-1]
	body := httpapi.RefundOf(latest, res.Payment)
	body.ReasonDetail = req.ReasonDetail

	httpapi.SetETag(w, int64(res.Payment.Version()))
	httpapi.SetLocation(w, h.baseURL,
		"/v1/payments/"+res.Payment.ID().String()+"/refunds/"+latest.ID().String())
	status := http.StatusCreated
	if latest.Status() != payment.RefundSucceeded {
		status = http.StatusAccepted
		w.Header().Set(httpapi.HeaderRetryAfter, "1")
	}
	httpapi.WriteJSON(w, r, status, body)
	return nil
}

// void implements `voidPayment`.
//
// There is no amount: the voided amount always equals the authorized amount, because a partial
// void is not an operation this platform offers. Returning part of a capture is a refund, and
// conflating the two would let a caller believe they had released half a hold when the scheme
// released all of it.
func (h *paymentHandlers) void(w http.ResponseWriter, r *http.Request) error {
	id, tc, err := paymentTarget(r)
	if err != nil {
		return err
	}
	var req httpapi.VoidRequest
	if err := decodeInto(r, &req); err != nil {
		return err
	}
	if req.Reason == "" {
		return apierror.New(apierror.CodeValidationFailed, "a void reason is required").
			WithDetail(apierror.Detail{
				Field: "reason", Code: "MISSING",
				Message: "Supply one of MERCHANT_REQUEST, AUTH_EXPIRY_PREVENTION, RISK_REVERSAL, ONBOARDING_TEST.",
				RuleID:  "L1.VOID_REASON_PRESENT",
			})
	}
	res, err := h.svc.Void(r.Context(), apppayment.VoidCommand{
		TenantID:       tc.TenantID,
		PaymentID:      id,
		IdempotencyKey: r.Header.Get(httpapi.HeaderIdempotencyKey),
		CorrelationID:  httpapi.CorrelationID(r.Context()),
	})
	if err != nil {
		return err
	}
	httpapi.SetETag(w, int64(res.Payment.Version()))
	status := http.StatusOK
	if res.Payment.State() == payment.StateProcessing {
		status = http.StatusAccepted
		w.Header().Set(httpapi.HeaderRetryAfter, "1")
	}
	httpapi.WriteJSON(w, r, status, paymentBody(res, nil))
	return nil
}

// paymentTarget resolves the {paymentId} path parameter and the tenant in one step, because
// every mutating payment handler needs both and needs them in that order.
func paymentTarget(r *http.Request) (shared.PaymentID, tenantctx.TenantContext, error) {
	raw, err := pathValue(r, "paymentId")
	if err != nil {
		return "", tenantctx.TenantContext{}, err
	}
	id, err := shared.ParsePaymentID(raw)
	if err != nil {
		return "", tenantctx.TenantContext{}, err
	}
	tc, err := tenantctx.FromContext(r.Context())
	if err != nil {
		return "", tenantctx.TenantContext{}, err
	}
	return id, tc, nil
}

// paymentBody renders the result, carrying across the fields that live on the orchestration
// result rather than on the aggregate: the risk decision, the score and the next action.
//
// They are on the result because they are decisions taken *about* this request, not state the
// payment carries — a payment re-read tomorrow has the same state and no live 3DS challenge.
func paymentBody(res *apppayment.Result, expand map[string]bool) httpapi.Payment {
	out := httpapi.PaymentOf(res.Payment, expand)
	out.RiskDecision = riskDecisionOf(res.Risk.Outcome)
	if res.Risk.Require3DS && out.RiskDecision == "ALLOW" {
		// The domain treats "approved, but authenticate the payer" as APPROVE plus a separate
		// Require3DS flag, because the two are genuinely independent: a regulated corridor with
		// no claimable exemption forces 3DS on a payment the risk engine liked. The contract has
		// only three values, so the flag is folded into the decision here — otherwise a client
		// seeing ALLOW would not know a challenge is coming.
		out.RiskDecision = "REQUIRE_3DS"
	}
	if res.Risk.Score > 0 {
		score := res.Risk.Score
		out.RiskScore = &score
	}
	if res.NextAction != nil {
		out.NextAction = &httpapi.NextAction{
			Type:        nextActionTypeOf(res.NextAction.Type),
			RedirectURL: res.NextAction.RedirectURL,
			ExpiresAt:   res.NextAction.ExpiresAt,
		}
	}
	if res.Plan != nil {
		out.RoutingPlanID = res.Plan.ID.String()
	}
	return out
}

// riskDecisionOf narrows the domain's four risk outcomes to the contract's three.
//
// REVIEW has no public member and renders as DECLINE. That is the conservative direction and the
// only defensible one: a payment held for manual review has not been authorized, and telling a
// merchant ALLOW for money that has not moved would have them ship goods against a payment that
// may yet be refused.
func riskDecisionOf(o risk.Outcome) string {
	switch o {
	case risk.OutcomeApprove:
		return "ALLOW"
	case risk.OutcomeRequire3DS:
		return "REQUIRE_3DS"
	default:
		return "DECLINE"
	}
}

// nextActionTypeOf maps the domain's four action types onto the contract's four. They differ in
// two names only: the domain's DISPLAY_QR_CODE and AWAIT_BANK_TRANSFER are the contract's
// DISPLAY_VOUCHER and AWAIT_BANK_DEBIT.
func nextActionTypeOf(t payment.NextActionType) string {
	switch t {
	case payment.ActionRedirect:
		return "REDIRECT"
	case payment.ActionThreeDSChall:
		return "THREE_DS_CHALLENGE"
	case payment.ActionDisplayQR:
		return "DISPLAY_VOUCHER"
	default:
		return "AWAIT_BANK_DEBIT"
	}
}

func captureMethodOf(s string) payment.CaptureMethod {
	if strings.EqualFold(s, string(payment.CaptureManual)) {
		return payment.CaptureManual
	}
	return payment.CaptureAutomatic
}

// optionalMoney converts a nullable wire amount, distinguishing absent from zero.
func optionalMoney(m *httpapi.Money) (*money.Money, error) {
	if m == nil {
		return nil, nil //nolint:nilnil // an absent optional wire field is not an error; (nil, nil) is what distinguishes absent from zero, which is this function's job
	}
	v, err := m.ToDomain()
	if err != nil {
		return nil, err
	}
	if !v.IsPositive() {
		return nil, apierror.New(apierror.CodeAmountInvalid, "amount must be greater than zero").
			WithDetail(apierror.Detail{
				Field: "amount.amount", Code: "NOT_POSITIVE",
				Message: "Omit the field to operate on the full remaining amount.",
				RuleID:  "L1.AMOUNT_POSITIVE",
			})
	}
	return &v, nil
}

// paymentFilter decodes the listing query parameters.
//
// Every unparseable value is an error rather than a silently ignored filter. Ignoring an
// unrecognised state would return the caller a superset of what they asked for, and a client
// reconciling on `?state=CAPTURED` would silently treat authorizations as captures.
func paymentFilter(r *http.Request) (ports.PaymentFilter, error) {
	q := r.URL.Query()
	var f ports.PaymentFilter

	if v := q.Get("merchantId"); v != "" {
		id, err := shared.ParseMerchantID(v)
		if err != nil {
			return f, err
		}
		f.MerchantID = id
	}
	for _, raw := range q["state"] {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			st := payment.State(part)
			if !st.IsKnown() {
				return f, apierror.Newf(apierror.CodeValidationFailed,
					"unknown payment state %q", part).
					WithDetail(apierror.Detail{
						Field: "state", Code: "UNKNOWN_ENUM_VALUE",
						Message: "See the PaymentState enum in the API contract.",
						RuleID:  "L1.ENUM_MEMBER",
					})
			}
			f.States = append(f.States, st)
		}
	}
	after, err := optionalTime(q.Get("createdAfter"), "createdAfter")
	if err != nil {
		return f, err
	}
	f.CreatedAfter = after
	before, err := optionalTime(q.Get("createdBefore"), "createdBefore")
	if err != nil {
		return f, err
	}
	f.CreatedBefore = before
	return f, nil
}

func optionalTime(raw, field string) (*time.Time, error) {
	if raw == "" {
		return nil, nil //nolint:nilnil // an absent optional query parameter is not an error; the caller stores the nil to mean "no bound"
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, apierror.Newf(apierror.CodeValidationFailed,
			"%s must be an RFC 3339 timestamp", field).
			WithDetail(apierror.Detail{
				Field: field, Code: "NOT_RFC3339",
				Message: "Example: 2026-08-01T00:00:00.000Z",
				RuleID:  "L1.TIMESTAMP_FORMAT",
			})
	}
	utc := t.UTC()
	return &utc, nil
}
